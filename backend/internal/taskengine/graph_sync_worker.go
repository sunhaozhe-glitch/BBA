package taskengine

// GraphSyncWorker is the bounded consumer of the local graph outbox.
//
// One tick does two things and nothing else:
//
//  1. Ingest: consume new core.outbox task events and live Summary versions for
//     every root-bound task, writing external intents into
//     core.graph_sync_outbox in the same transaction as the durable checkpoint.
//  2. Dispatch: claim the head of each (task, graph kind) partition, call ARTEX
//     outside any transaction, and record the outcome in a second transaction.
//
// The external process is never mixed with the local transaction. A write whose
// outcome is not answered unambiguously becomes needs_reconcile and is never
// resent; only a read (graph.pull) is safe to replay. A local failure while
// recording a successful write leaves the item leased in 'sending', so the next
// tick classifies it as an unknown outcome instead of assuming success.
//
// Isolation: every task is ingested in its own transaction and every outbox
// partition is ordered independently, so one failing task or graph never blocks
// another.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"bba.local/taskengine/internal/artex"
	"github.com/jackc/pgx/v5"
)

// GraphSyncWorkerConfig configures one worker instance.
type GraphSyncWorkerConfig struct {
	// WorkerID is the outbox lease owner. Two concurrent workers must not share
	// it. Empty selects "graph-sync-<pid-like default>".
	WorkerID string
	// Provider is the wired ARTEX client. A nil provider is refused: an
	// unconfigured worker must not start and pretend to sync.
	Provider GraphSyncProvider
	// BatchSize bounds tasks ingested and items dispatched per tick (default 20,
	// max 100).
	BatchSize int
	// PollInterval is the idle delay between ticks in Run.
	PollInterval time.Duration
	// TickTimeout bounds one whole tick.
	TickTimeout time.Duration
	// Logf receives bounded progress/failure lines. Nil discards.
	Logf func(string, ...any)
}

// GraphSyncTick summarises one bounded pass.
type GraphSyncTick struct {
	StartedAt time.Time
	Duration  time.Duration

	Tasks          int
	Enqueued       int
	Claimed        int
	Pulled         int
	GoalsCreated   int
	GoalsUpdated   int
	GoalsDeleted   int
	Requeued       int
	NeedsReconcile int
	Failed         int
	Skipped        int
	TaskErrors     int
}

func (t GraphSyncTick) active() bool {
	return t.Tasks != 0 || t.Enqueued != 0 || t.Claimed != 0 || t.Pulled != 0 ||
		t.GoalsCreated != 0 || t.GoalsUpdated != 0 || t.GoalsDeleted != 0 ||
		t.Requeued != 0 || t.NeedsReconcile != 0 || t.Failed != 0 ||
		t.Skipped != 0 || t.TaskErrors != 0
}

func (t GraphSyncTick) String() string {
	return fmt.Sprintf("tasks=%d enqueued=%d claimed=%d pulled=%d goals(create/update/delete)=%d/%d/%d requeued=%d reconcile=%d failed=%d skipped=%d task_errors=%d duration=%s",
		t.Tasks, t.Enqueued, t.Claimed, t.Pulled, t.GoalsCreated, t.GoalsUpdated, t.GoalsDeleted,
		t.Requeued, t.NeedsReconcile, t.Failed, t.Skipped, t.TaskErrors, t.Duration.Round(time.Millisecond))
}

// GraphSyncWorker consumes the local graph outbox for one Store.
type GraphSyncWorker struct {
	store *Store
	cfg   GraphSyncWorkerConfig

	stop chan struct{}
	once sync.Once
	mu   sync.Mutex
	run  bool

	// cursor rotates the root-bound task population so a population larger than
	// one batch is still visited. It is a hint, not a checkpoint: a restart
	// simply starts another lap at the head.
	cursorWorkspace string
	cursorTask      string
}

// NewGraphSyncWorker validates the configuration. It starts no work.
func NewGraphSyncWorker(store *Store, cfg GraphSyncWorkerConfig) (*GraphSyncWorker, error) {
	if store == nil || store.Pool == nil || cfg.Provider == nil {
		return nil, problem(taskCodeValidation)
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "graph-sync-" + newID("w")
	}
	if len(cfg.WorkerID) > 96 {
		return nil, problem(taskCodeValidation)
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = graphSyncDefaultBatch
	}
	if cfg.BatchSize < 1 || cfg.BatchSize > graphSyncMaxBatch {
		return nil, problem(taskCodeValidation)
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 15 * time.Second
	}
	if cfg.PollInterval < 100*time.Millisecond {
		return nil, problem(taskCodeValidation)
	}
	if cfg.TickTimeout == 0 {
		cfg.TickTimeout = 30 * time.Second
	}
	if cfg.TickTimeout < time.Second {
		return nil, problem(taskCodeValidation)
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &GraphSyncWorker{store: store, cfg: cfg, stop: make(chan struct{})}, nil
}

// Stop requests a graceful stop. The current tick is not interrupted.
func (w *GraphSyncWorker) Stop() {
	if w == nil {
		return
	}
	w.once.Do(func() { close(w.stop) })
}

// Run ticks immediately and then every PollInterval until ctx ends or Stop is
// called. A tick error is logged and never terminates the loop.
func (w *GraphSyncWorker) Run(ctx context.Context) error {
	if w == nil || w.store == nil {
		return problem(taskCodeValidation)
	}
	w.mu.Lock()
	if w.run {
		w.mu.Unlock()
		return problem("WORKER_ALREADY_RUNNING")
	}
	w.run = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.run = false
		w.mu.Unlock()
	}()
	w.cfg.Logf("graph-sync worker %s started poll=%s batch=%d", w.cfg.WorkerID, w.cfg.PollInterval, w.cfg.BatchSize)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.stop:
			return nil
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return nil
		}
		tickCtx, cancel := context.WithTimeout(ctx, w.cfg.TickTimeout)
		tick, err := w.Tick(tickCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			w.cfg.Logf("graph-sync worker %s tick failed: %v", w.cfg.WorkerID, err)
		} else if tick.active() {
			w.cfg.Logf("graph-sync worker %s tick %s", w.cfg.WorkerID, tick.String())
		}
		timer.Reset(w.cfg.PollInterval)
	}
}

// Tick runs one bounded pass: ingest, then dispatch.
func (w *GraphSyncWorker) Tick(ctx context.Context) (tick GraphSyncTick, err error) {
	tick = GraphSyncTick{StartedAt: time.Now().UTC()}
	if w == nil || w.store == nil || w.cfg.Provider == nil {
		return tick, problem(graphSyncCodeNotConfigured)
	}
	if !w.cfg.Provider.Wired() {
		return tick, problem(graphSyncCodeNotConfigured)
	}
	defer func() { tick.Duration = time.Since(tick.StartedAt) }()

	tasks, err := w.rootBoundTasks(ctx, w.cfg.BatchSize)
	if err != nil {
		return tick, err
	}
	tick.Tasks = len(tasks)
	for _, task := range tasks {
		enqueued, err := w.store.ingestGraphSources(ctx, task.workspace, task.taskID)
		if err != nil {
			tick.TaskErrors++
			w.cfg.Logf("graph-sync task %s/%s ingest failed: %v", task.workspace, task.taskID, err)
			continue
		}
		tick.Enqueued += enqueued
	}

	remaining := w.cfg.BatchSize
	for remaining > 0 {
		items, quarantined, failed, err := w.store.claimGraphOutbox(ctx, w.cfg.WorkerID, remaining)
		if err != nil {
			return tick, err
		}
		tick.NeedsReconcile += quarantined
		tick.Failed += failed
		if len(items) == 0 {
			break
		}
		tick.Claimed += len(items)
		remaining -= len(items)
		for _, item := range items {
			result := w.dispatchItem(ctx, item)
			switch result {
			case graphDispatchPulled:
				tick.Pulled++
			case graphDispatchCreated:
				tick.GoalsCreated++
			case graphDispatchUpdated:
				tick.GoalsUpdated++
			case graphDispatchDeleted:
				tick.GoalsDeleted++
			case graphDispatchRequeued:
				tick.Requeued++
			case graphDispatchNeedsReconcile:
				tick.NeedsReconcile++
			case graphDispatchFailed:
				tick.Failed++
			case graphDispatchSkipped:
				tick.Skipped++
			}
		}
	}
	return tick, nil
}

type graphRootTask struct {
	workspace string
	taskID    string
}

// rootBoundTasks returns root-bound tasks, rotating past the previous batch so a
// population larger than one tick is still visited.
func (w *GraphSyncWorker) rootBoundTasks(ctx context.Context, limit int) ([]graphRootTask, error) {
	query := `
		SELECT DISTINCT r.workspace_id,r.task_id
		FROM core.task_graph_roots r
		WHERE r.provider=$1 AND (r.workspace_id,r.task_id)>($2,$3)
		ORDER BY r.workspace_id,r.task_id
		LIMIT $4`
	rows, err := w.store.Pool.Query(ctx, query, graphSyncProviderArtex, w.cursorWorkspace, w.cursorTask, limit)
	if err != nil {
		return nil, err
	}
	out := make([]graphRootTask, 0)
	for rows.Next() {
		var task graphRootTask
		if err := rows.Scan(&task.workspace, &task.taskID); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(out) < limit {
		// Reached the end of the population: the next tick starts a new lap.
		w.cursorWorkspace, w.cursorTask = "", ""
	} else {
		last := out[len(out)-1]
		w.cursorWorkspace, w.cursorTask = last.workspace, last.taskID
	}
	return out, nil
}

type graphDispatchResult int

const (
	graphDispatchSkipped graphDispatchResult = iota
	graphDispatchPulled
	graphDispatchCreated
	graphDispatchUpdated
	graphDispatchDeleted
	graphDispatchRequeued
	graphDispatchNeedsReconcile
	graphDispatchFailed
)

// claimGraphOutbox leases the head item of each partition. A stale write lease
// is not redispatched: it becomes needs_reconcile because the external effect is
// unverified. A stale read lease is requeued because a read has no effect.
func (s *Store) claimGraphOutbox(ctx context.Context, worker string, limit int) ([]GraphSyncOutboxItem, int, int, error) {
	if worker == "" || limit <= 0 {
		return nil, 0, 0, problem(taskCodeValidation)
	}
	if limit > graphSyncMaxBatch {
		limit = graphSyncMaxBatch
	}
	var claimed []GraphSyncOutboxItem
	reconcileCount, failedCount := 0, 0
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// The head of a partition is the smallest non-terminal sequence; the
		// correlated subquery keeps that rule without mixing DISTINCT with
		// FOR UPDATE, so the row lock is applied to outbox rows only.
		rows, err := tx.Query(ctx, `
			SELECT o.workspace_id,o.task_id,o.id,o.graph_kind,o.operation,o.idempotency_key,
			       o.payload,o.payload_digest,o.scope,o.sequence,o.state,o.attempts,o.max_attempts,
			       o.external_node_id,o.last_error_code,o.last_error,o.next_attempt_at,o.created_at,o.updated_at
			FROM core.graph_sync_outbox o
			WHERE o.provider=$1
			  AND o.sequence=(
			    SELECT min(h.sequence) FROM core.graph_sync_outbox h
			    WHERE h.workspace_id=o.workspace_id AND h.task_id=o.task_id
			      AND h.graph_kind=o.graph_kind AND h.provider=o.provider
			      AND h.state IN ('pending','sending'))
			  AND ((o.state='pending' AND o.next_attempt_at<=clock_timestamp())
			    OR (o.state='sending' AND o.lease_until<clock_timestamp()))
			ORDER BY o.workspace_id,o.task_id,o.graph_kind,o.sequence
			LIMIT $2
			FOR UPDATE OF o SKIP LOCKED`, graphSyncProviderArtex, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item GraphSyncOutboxItem
			if err := rows.Scan(&item.WorkspaceID, &item.TaskID, &item.ID, &item.GraphKind, &item.Operation,
				&item.IdempotencyKey, &item.Payload, &item.PayloadDigest, &item.Scope, &item.Sequence,
				&item.State, &item.Attempts, &item.MaxAttempts, &item.ExternalNodeID, &item.LastErrorCode,
				&item.LastError, &item.NextAttemptAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
				rows.Close()
				return err
			}
			claimed = append(claimed, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		keep := claimed[:0]
		for _, item := range claimed {
			if item.State == "sending" {
				if item.Operation == "graph.pull" {
					if _, err := tx.Exec(ctx, `
						UPDATE core.graph_sync_outbox
						SET state='pending',lease_owner=NULL,lease_until=NULL,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp()
						WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='sending'`,
						item.WorkspaceID, item.TaskID, item.ID); err != nil {
						return err
					}
					item.State = "pending"
				} else {
					if _, err := tx.Exec(ctx, `
						UPDATE core.graph_sync_outbox
						SET state='needs_reconcile',last_error_code=$4,last_error=$5,
						    lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
						WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='sending'`,
						item.WorkspaceID, item.TaskID, item.ID, graphSyncCodeOutcomeUnknown,
						"stale dispatch lease: the external outcome was never recorded; do not resend"); err != nil {
						return err
					}
					if err := recordGraphRootErrorTx(ctx, tx, item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncCodeOutcomeUnknown,
						"stale dispatch lease: the external outcome was never recorded; do not resend"); err != nil {
						return err
					}
					reconcileCount++
					continue
				}
			}
			if item.Attempts >= item.MaxAttempts {
				if _, err := tx.Exec(ctx, `
					UPDATE core.graph_sync_outbox
					SET state='failed',last_error_code=$4,last_error=$5,lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
					WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='pending'`,
					item.WorkspaceID, item.TaskID, item.ID, graphSyncCodeUnavailable, "attempt budget exhausted"); err != nil {
					return err
				}
				if err := recordGraphRootErrorTx(ctx, tx, item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncCodeUnavailable,
					"attempt budget exhausted"); err != nil {
					return err
				}
				failedCount++
				continue
			}
			var leasedState string
			if err := tx.QueryRow(ctx, `
				UPDATE core.graph_sync_outbox
				SET state='sending',lease_owner=$4,lease_until=clock_timestamp()+make_interval(secs => $5::double precision),
				    attempts=attempts+1,updated_at=clock_timestamp()
				WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='pending'
				RETURNING state`,
				item.WorkspaceID, item.TaskID, item.ID, worker, graphSyncLeaseSeconds).Scan(&leasedState); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				return err
			}
			item.State = leasedState
			item.Attempts++
			keep = append(keep, item)
		}
		claimed = append([]GraphSyncOutboxItem{}, keep...)
		return nil
	})
	if err != nil {
		return nil, 0, 0, err
	}
	return claimed, reconcileCount, failedCount, nil
}

// dispatchItem performs the external call for one claimed item and records the
// result. It never starts a local transaction around the call.
func (w *GraphSyncWorker) dispatchItem(ctx context.Context, item GraphSyncOutboxItem) graphDispatchResult {
	root, bound, err := w.loadRoot(ctx, item.WorkspaceID, item.TaskID, item.GraphKind)
	if err != nil {
		w.cfg.Logf("graph-sync load root %s/%s/%s: %v", item.WorkspaceID, item.TaskID, item.GraphKind, err)
		return w.finalizeFailure(ctx, item, graphSyncCodeUnavailable, err.Error())
	}
	if !bound {
		return w.finalizeFailure(ctx, item, graphSyncCodeRootNotBound, "graph root is not bound")
	}
	switch item.Operation {
	case "graph.pull":
		return w.dispatchPull(ctx, item, root)
	case "goal.create":
		var payload struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		if err := json.Unmarshal(item.Payload, &payload); err != nil || payload.Text == "" {
			return w.finalizeFailure(ctx, item, graphSyncCodeProtocol, "goal.create payload has no text")
		}
		goal, err := w.cfg.Provider.AddGoal(ctx, root.ExternalRoot, payload.Text, payload.VulnClass)
		if err != nil {
			return w.handleWriteError(ctx, item, err)
		}
		if err := w.finalizeGoalWrite(ctx, item, root, goal.ID, payload.Text, false); err != nil {
			w.cfg.Logf("graph-sync create finalize %s: %v (left leased for reconcile)", item.ID, err)
			return graphDispatchSkipped
		}
		return graphDispatchCreated
	case "goal.update":
		goalID, text, err := w.resolveGoalTarget(ctx, item)
		if err != nil {
			return w.finalizeFailure(ctx, item, graphSyncCodeGoalUnmapped, err.Error())
		}
		goal, err := w.cfg.Provider.UpdateGoal(ctx, root.ExternalRoot, goalID, text, "")
		if err != nil {
			return w.handleWriteError(ctx, item, err)
		}
		if err := w.finalizeGoalWrite(ctx, item, root, goal.ID, text, false); err != nil {
			w.cfg.Logf("graph-sync update finalize %s: %v (left leased for reconcile)", item.ID, err)
			return graphDispatchSkipped
		}
		return graphDispatchUpdated
	case "goal.delete":
		goalID, _, err := w.resolveGoalTarget(ctx, item)
		if err != nil {
			return w.finalizeFailure(ctx, item, graphSyncCodeGoalUnmapped, err.Error())
		}
		if err := w.cfg.Provider.DeleteGoal(ctx, root.ExternalRoot, goalID); err != nil {
			return w.handleWriteError(ctx, item, err)
		}
		if err := w.finalizeGoalWrite(ctx, item, root, goalID, "", true); err != nil {
			w.cfg.Logf("graph-sync delete finalize %s: %v (left leased for reconcile)", item.ID, err)
			return graphDispatchSkipped
		}
		return graphDispatchDeleted
	default:
		return w.finalizeFailure(ctx, item, graphSyncCodeProtocol, "unknown outbox operation")
	}
}

func (w *GraphSyncWorker) loadRoot(ctx context.Context, workspace, taskID, kind string) (graphRootRow, bool, error) {
	var root graphRootRow
	var found bool
	err := w.store.tx(ctx, func(tx pgx.Tx) error {
		var err error
		root, found, err = loadGraphRoot(ctx, tx, workspace, taskID, kind)
		return err
	})
	return root, found, err
}

func (w *GraphSyncWorker) dispatchPull(ctx context.Context, item GraphSyncOutboxItem, root graphRootRow) graphDispatchResult {
	if item.GraphKind == graphSyncKindFact {
		graph, err := w.cfg.Provider.Graph(ctx, root.ExternalRoot)
		if err != nil {
			return w.handlePullError(ctx, item, err)
		}
		if err := w.store.applyFactPull(ctx, item, graph); err != nil {
			w.cfg.Logf("graph-sync fact pull apply %s: %v (left leased, replay is safe)", item.ID, err)
			return graphDispatchSkipped
		}
		return graphDispatchPulled
	}
	goals, err := w.cfg.Provider.Goals(ctx, root.ExternalRoot)
	if err != nil {
		return w.handlePullError(ctx, item, err)
	}
	if err := w.store.applyGoalPull(ctx, item, goals); err != nil {
		w.cfg.Logf("graph-sync goal pull apply %s: %v (left leased, replay is safe)", item.ID, err)
		return graphDispatchSkipped
	}
	return graphDispatchPulled
}

// handlePullError classifies a read failure. Reads are replayable, so a
// transient failure is retried with a bounded attempt budget.
func (w *GraphSyncWorker) handlePullError(ctx context.Context, item GraphSyncOutboxItem, err error) graphDispatchResult {
	code, retryable := graphSyncErrorCode(err)
	if !retryable {
		return w.finalizeFailure(ctx, item, code, err.Error())
	}
	if item.Attempts >= item.MaxAttempts {
		return w.finalizeFailure(ctx, item, code, err.Error())
	}
	if err := w.store.requeueGraphOutbox(ctx, item, code, err.Error()); err != nil {
		w.cfg.Logf("graph-sync requeue %s: %v", item.ID, err)
		return graphDispatchFailed
	}
	return graphDispatchRequeued
}

// handleWriteError classifies a write failure. A definite refusal is terminal; an
// undecided effect becomes needs_reconcile; a pre-send cancellation is the only
// case that may be requeued.
func (w *GraphSyncWorker) handleWriteError(ctx context.Context, item GraphSyncOutboxItem, err error) graphDispatchResult {
	if artex.HasCode(err, artex.CodeCanceled) {
		if requeueErr := w.store.requeueGraphOutbox(ctx, item, graphSyncCodeUnavailable, "canceled before send"); requeueErr != nil {
			w.cfg.Logf("graph-sync requeue %s: %v", item.ID, requeueErr)
			return graphDispatchFailed
		}
		return graphDispatchRequeued
	}
	code, _ := graphSyncErrorCode(err)
	if code == graphSyncCodeOutcomeUnknown {
		return w.markNeedsReconcile(ctx, item, code, err.Error())
	}
	return w.finalizeFailure(ctx, item, code, err.Error())
}

// graphSyncErrorCode maps one adapter error to a stable graph-sync code and
// whether the operation may be retried (reads only).
func graphSyncErrorCode(err error) (string, bool) {
	switch {
	case artex.HasCode(err, artex.CodeNotConfigured):
		return graphSyncCodeNotConfigured, false
	case artex.HasCode(err, artex.CodeUnauthorized):
		return graphSyncCodeUnauthorized, false
	case artex.HasCode(err, artex.CodeForbidden):
		return graphSyncCodeForbidden, false
	case artex.HasCode(err, artex.CodeNotFound):
		return graphSyncCodeNotFound, false
	case artex.HasCode(err, artex.CodeInvalidResponse), artex.HasCode(err, artex.CodeResponseTooLarge):
		return graphSyncCodeProtocol, false
	case artex.HasCode(err, artex.CodeInvalidRequest):
		return graphSyncCodeRejected, false
	case artex.HasCode(err, artex.CodeRequestRejected):
		return graphSyncCodeRejected, false
	case artex.HasCode(err, artex.CodeWriteNotAllowed):
		return graphSyncCodeWritesDisabled, false
	case artex.HasCode(err, artex.CodeOutcomeUnknown):
		return graphSyncCodeOutcomeUnknown, false
	case artex.HasCode(err, artex.CodeCanceled):
		return graphSyncCodeUnavailable, true
	case artex.HasCode(err, artex.CodeUnavailable), artex.HasCode(err, artex.CodeUpstreamError), artex.HasCode(err, artex.CodeIndeterminate):
		return graphSyncCodeUnavailable, true
	default:
		return graphSyncCodeUnavailable, true
	}
}

// resolveGoalTarget maps a goal operation back to the ARTEX goal id recorded
// when its create succeeded. There is no guess: without a mapping the operation
// fails instead of addressing a random goal.
func (w *GraphSyncWorker) resolveGoalTarget(ctx context.Context, item GraphSyncOutboxItem) (int64, string, error) {
	var payload struct {
		GoalKey string `json:"goal_key"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return 0, "", err
	}
	if payload.GoalKey == "" {
		return 0, "", errors.New("payload has no goal_key")
	}
	var external string
	err := w.store.Pool.QueryRow(ctx, `
		SELECT external_node_id FROM core.graph_sync_nodes
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4 AND local_key=$5`,
		item.WorkspaceID, item.TaskID, graphSyncKindGoal, graphSyncProviderArtex, payload.GoalKey).Scan(&external)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("no external goal is mapped to %q", payload.GoalKey)
	}
	if err != nil {
		return 0, "", err
	}
	var goalID int64
	if _, err := fmt.Sscan(external, &goalID); err != nil || goalID <= 0 {
		return 0, "", fmt.Errorf("mapped external goal id %q is not positive", external)
	}
	return goalID, payload.Text, nil
}

// finalizeGoalWrite records a successful external goal mutation. The external
// call already happened, so a failure here leaves the item leased and the next
// tick treats it as an unknown outcome.
func (w *GraphSyncWorker) finalizeGoalWrite(ctx context.Context, item GraphSyncOutboxItem, root graphRootRow, goalID int64, text string, deleted bool) error {
	return w.store.tx(ctx, func(tx pgx.Tx) error {
		externalID := fmt.Sprint(goalID)
		current, bound, err := loadGraphRoot(ctx, tx, item.WorkspaceID, item.TaskID, item.GraphKind)
		if err != nil {
			return err
		}
		if !bound {
			return errGraphRootMissing
		}
		revision := current.GraphRevision + 1
		if deleted {
			if _, err := tx.Exec(ctx, `
				DELETE FROM core.graph_sync_nodes
				WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4 AND external_node_id=$5`,
				item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex, externalID); err != nil {
				return err
			}
		} else {
			payload := map[string]any{"text": text}
			encoded, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			var goalKey string
			var op struct {
				GoalKey string `json:"goal_key"`
			}
			_ = json.Unmarshal(item.Payload, &op)
			goalKey = op.GoalKey
			if _, err := tx.Exec(ctx, `
				INSERT INTO core.graph_sync_nodes(
					workspace_id,task_id,graph_kind,provider,external_node_id,external_kind,external_state,
					origin,local_key,payload,provenance,observed_revision)
				VALUES($1,$2,$3,$4,$5,'goal','open','bba',$6,$7,'inference',$8)
				ON CONFLICT (workspace_id,task_id,graph_kind,provider,external_node_id) DO UPDATE
				SET external_state=EXCLUDED.external_state,payload=EXCLUDED.payload,
				    local_key=EXCLUDED.local_key,provenance=EXCLUDED.provenance,
				    observed_revision=EXCLUDED.observed_revision,updated_at=clock_timestamp()`,
				item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex, externalID,
				goalKey, encoded, revision); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE core.graph_sync_outbox
			SET state='sent',external_node_id=$4,last_error_code=NULL,last_error=NULL,
			    lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='sending'`,
			item.WorkspaceID, item.TaskID, item.ID, externalID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE core.task_graph_roots
			SET graph_revision=$5,last_sync_at=clock_timestamp(),last_error_code=NULL,last_error=NULL,last_error_at=NULL,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
			item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex, revision)
		return err
	})
}

// applyFactPull stores the external fact graph read model and advances the
// revision. Facts from ARTEX are inference from BBA's point of view: trusted
// local facts live in the task graph projection, never here.
func (s *Store) applyFactPull(ctx context.Context, item GraphSyncOutboxItem, graph artex.Graph) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		current, bound, err := loadGraphRoot(ctx, tx, item.WorkspaceID, item.TaskID, item.GraphKind)
		if err != nil {
			return err
		}
		if !bound {
			return errGraphRootMissing
		}
		revision := current.GraphRevision + 1
		if err := replaceGraphNodes(ctx, tx, item, revision, graph.Nodes); err != nil {
			return err
		}
		// An anchor may only cite a node this read actually returned; a vanished
		// node clears the link instead of leaving a dangling inference reference.
		ids := make([]string, 0, len(graph.Nodes))
		for _, node := range graph.Nodes {
			ids = append(ids, fmt.Sprint(node.ID))
		}
		if _, err := tx.Exec(ctx, `
			UPDATE core.task_graph_anchors
			SET external_node_id=NULL,external_provenance=NULL,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4
			  AND external_node_id IS NOT NULL AND NOT (external_node_id = ANY($5::text[]))`,
			item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex, ids); err != nil {
			return err
		}
		if err := mapAnchorExternalNodes(ctx, tx, item); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE core.graph_sync_outbox
			SET state='sent',last_error_code=NULL,last_error=NULL,lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='sending'`,
			item.WorkspaceID, item.TaskID, item.ID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE core.task_graph_roots
			SET graph_revision=$5,last_sync_at=clock_timestamp(),last_error_code=NULL,last_error=NULL,last_error_at=NULL,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
			item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex, revision)
		return err
	})
}

// applyGoalPull stores the external goal list as the goal graph read model while
// preserving the local_key that maps a locally created goal back to its create
// operation.
func (s *Store) applyGoalPull(ctx context.Context, item GraphSyncOutboxItem, goals []artex.Goal) error {
	nodes := make([]artex.Node, 0, len(goals))
	for _, goal := range goals {
		payload, err := json.Marshal(map[string]any{"text": goal.Text, "vulnclass": goal.VulnClass})
		if err != nil {
			return err
		}
		nodes = append(nodes, artex.Node{
			ID:      goal.ID,
			Kind:    graphSyncKindGoal,
			State:   goal.State,
			Origin:  goal.Origin,
			Payload: payload,
			TS:      goal.TS,
		})
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		current, bound, err := loadGraphRoot(ctx, tx, item.WorkspaceID, item.TaskID, item.GraphKind)
		if err != nil {
			return err
		}
		if !bound {
			return errGraphRootMissing
		}
		revision := current.GraphRevision + 1
		if err := replaceGraphNodes(ctx, tx, item, revision, nodes); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE core.graph_sync_outbox
			SET state='sent',last_error_code=NULL,last_error=NULL,lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='sending'`,
			item.WorkspaceID, item.TaskID, item.ID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE core.task_graph_roots
			SET graph_revision=$5,last_sync_at=clock_timestamp(),last_error_code=NULL,last_error=NULL,last_error_at=NULL,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
			item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex, revision)
		return err
	})
}

// replaceGraphNodes mirrors one external read into core.graph_sync_nodes. Nodes
// missing from the read are removed: the table is a read model of what ARTEX
// publishes now, not an append-only history.
func replaceGraphNodes(ctx context.Context, tx pgx.Tx, item GraphSyncOutboxItem, revision int64, nodes []artex.Node) error {
	if len(nodes) > graphSyncMaxNodesStored {
		nodes = nodes[:graphSyncMaxNodesStored]
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, fmt.Sprint(node.ID))
		payload := map[string]any{"raw": json.RawMessage(node.Payload)}
		if len(node.Payload) > 0 {
			var object map[string]any
			if err := json.Unmarshal(node.Payload, &object); err == nil {
				payload = object
			}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO core.graph_sync_nodes(
				workspace_id,task_id,graph_kind,provider,external_node_id,external_kind,external_state,
				origin,payload,provenance,observed_revision)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'inference',$10)
			ON CONFLICT (workspace_id,task_id,graph_kind,provider,external_node_id) DO UPDATE
			SET external_kind=EXCLUDED.external_kind,external_state=EXCLUDED.external_state,
			    origin=EXCLUDED.origin,payload=EXCLUDED.payload,provenance=EXCLUDED.provenance,
			    observed_revision=EXCLUDED.observed_revision,updated_at=clock_timestamp()`,
			item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex,
			fmt.Sprint(node.ID), node.Kind, node.State, node.Origin, encoded, revision); err != nil {
			return err
		}
	}
	if len(ids) == 0 {
		_, err := tx.Exec(ctx, `
			DELETE FROM core.graph_sync_nodes
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
			item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex)
		return err
	}
	_, err := tx.Exec(ctx, `
		DELETE FROM core.graph_sync_nodes
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4
		  AND NOT (external_node_id = ANY($5::text[]))`,
		item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex, ids)
	return err
}

// mapAnchorExternalNodes links an anchor to the external node that names its
// group. The mapping is marked inference: the anchor fact is trusted, but the
// external node is only a model-side reference to it.
func mapAnchorExternalNodes(ctx context.Context, tx pgx.Tx, item GraphSyncOutboxItem) error {
	rows, err := tx.Query(ctx, `
		SELECT anchor_id,group_id FROM core.task_graph_anchors
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4 AND external_node_id IS NULL
		ORDER BY anchor_id LIMIT 256`,
		item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex)
	if err != nil {
		return err
	}
	type anchorRow struct {
		id    string
		group string
	}
	anchors := make([]anchorRow, 0)
	for rows.Next() {
		var anchor anchorRow
		if err := rows.Scan(&anchor.id, &anchor.group); err != nil {
			rows.Close()
			return err
		}
		anchors = append(anchors, anchor)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(anchors) == 0 {
		return nil
	}
	nodes, err := tx.Query(ctx, `
		SELECT external_node_id,payload::text FROM core.graph_sync_nodes
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4
		ORDER BY (external_kind IN ('intent','goal')) DESC,external_node_id LIMIT 512`,
		item.WorkspaceID, item.TaskID, item.GraphKind, graphSyncProviderArtex)
	if err != nil {
		return err
	}
	type nodeRow struct {
		id      string
		payload string
	}
	graphNodes := make([]nodeRow, 0)
	for nodes.Next() {
		var node nodeRow
		if err := nodes.Scan(&node.id, &node.payload); err != nil {
			nodes.Close()
			return err
		}
		graphNodes = append(graphNodes, node)
	}
	if err := nodes.Err(); err != nil {
		nodes.Close()
		return err
	}
	nodes.Close()
	for _, anchor := range anchors {
		for _, node := range graphNodes {
			if !strings.Contains(node.payload, anchor.group) {
				continue
			}
			if _, err := tx.Exec(ctx, `
				UPDATE core.task_graph_anchors
				SET external_node_id=$4,external_provenance='inference',updated_at=clock_timestamp()
				WHERE workspace_id=$1 AND task_id=$2 AND anchor_id=$3 AND external_node_id IS NULL`,
				item.WorkspaceID, item.TaskID, anchor.id, node.id); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// requeueGraphOutbox returns a read to the queue with a bounded backoff.
func (s *Store) requeueGraphOutbox(ctx context.Context, item GraphSyncOutboxItem, code, message string) error {
	backoff := int(item.Attempts) * 5
	if backoff > 300 {
		backoff = 300
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE core.graph_sync_outbox
			SET state='pending',last_error_code=$4,last_error=$5,
			    next_attempt_at=clock_timestamp()+make_interval(secs => $6::double precision),
			    lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='sending'`,
			item.WorkspaceID, item.TaskID, item.ID, code, message, backoff); err != nil {
			return err
		}
		return nil
	})
}

func (w *GraphSyncWorker) markNeedsReconcile(ctx context.Context, item GraphSyncOutboxItem, code, message string) graphDispatchResult {
	if err := w.store.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE core.graph_sync_outbox
			SET state='needs_reconcile',last_error_code=$4,last_error=$5,
			    lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='sending'`,
			item.WorkspaceID, item.TaskID, item.ID, code, message); err != nil {
			return err
		}
		return recordGraphRootErrorTx(ctx, tx, item.WorkspaceID, item.TaskID, item.GraphKind, code, message)
	}); err != nil {
		w.cfg.Logf("graph-sync needs_reconcile %s: %v", item.ID, err)
		return graphDispatchFailed
	}
	return graphDispatchNeedsReconcile
}

func (w *GraphSyncWorker) finalizeFailure(ctx context.Context, item GraphSyncOutboxItem, code, message string) graphDispatchResult {
	if err := w.store.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE core.graph_sync_outbox
			SET state='failed',last_error_code=$4,last_error=$5,
			    lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='sending'`,
			item.WorkspaceID, item.TaskID, item.ID, code, message); err != nil {
			return err
		}
		return recordGraphRootErrorTx(ctx, tx, item.WorkspaceID, item.TaskID, item.GraphKind, code, message)
	}); err != nil {
		w.cfg.Logf("graph-sync finalize failure %s: %v", item.ID, err)
		return graphDispatchFailed
	}
	return graphDispatchFailed
}

func recordGraphRootErrorTx(ctx context.Context, tx pgx.Tx, workspace, taskID, kind, code, message string) error {
	_, err := tx.Exec(ctx, `
		UPDATE core.task_graph_roots
		SET last_error_code=$5,last_error=$6,last_error_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
		workspace, taskID, kind, graphSyncProviderArtex, code, message)
	return err
}
