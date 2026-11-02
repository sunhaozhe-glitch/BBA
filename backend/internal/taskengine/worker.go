package taskengine

// Continuous deterministic Task Engine worker.
//
// The worker is a bounded, restartable local loop over the existing Store
// methods. It never shells out to the single-shot p0-20 CLI, never invents an
// identity, baseline, approval or capability fact, and never retries an
// unresolved external effect. Every step is idempotent under repeated ticks:
//
//   - expansion imports one deterministic page of the fixed candidate template;
//   - Reserve/AcceptOperation/IssuePermit/Dispatch keep the P0-07/P0-11 gates;
//   - bridge outbox -> inbox -> observation application reuses the P0-11 path;
//   - operator reconciliation requests are consumed read-only;
//   - the deadline sweep reuses reconcileTaskProgress for silent deadlines.
//
// A dispatchable task has exactly one coordinator; proposal tasks may have
// a bounded, persisted number of admitted work slots under that coordinator. The worker
// claims a task with a PostgreSQL session advisory lock held on a dedicated
// pooled connection for the duration of that task's step. That lock is the
// worker lease: a concurrent worker skips the task, and a crashed worker's
// session ends with its connection, dropping the lock without a schema change.
// The lease is revalidated before each mutating step, and the Store's own
// task-version, owner-epoch, fence-token and permit checks reject a write that a
// newer owner already superseded. Advisory locks do not outlive a session, so a
// stale worker whose connection died cannot keep acting on the task.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	workerDefaultPoll            = 15 * time.Second
	workerMinPoll                = 100 * time.Millisecond
	workerDefaultBatch           = 20
	workerMaxBatch               = 100
	workerDefaultPage            = 100
	workerPagesPerTaskPerTick    = 4
	workerDefaultPermitTTL       = 10 * time.Second
	workerDefaultTickTimeout     = 30 * time.Second
	workerDefaultShutdownTimeout = 5 * time.Second
	workerLeaseReleaseTimeout    = 2 * time.Second
	workerMaxRuntimeActors       = 100
	workerMaxWorkspaces          = 100
	workerMaxRuntimePages        = 10

	workerLeaseLost = "WORKER_LEASE_LOST"

	// workerCursorRecovery and workerCursorBridgeOutbox name the two durable
	// rotation cursors, one row each in core.worker_cursor. A consumer keeps its
	// own position, so a long recovery pass cannot delay the bridge pump.
	workerCursorRecovery     = "recovery"
	workerCursorBridgeOutbox = "bridge_outbox"
)

// WorkerTransport selects how a worker talks to the outside world.
type WorkerTransport string

const (
	// WorkerFixtureTransport is the default. It performs no network call at
	// all and returns a deterministic submitted receipt, so the whole join
	// chain can run in an isolated development database.
	WorkerFixtureTransport WorkerTransport = "fixture"
	// WorkerExternalTransport carries a caller-supplied Adapter and additionally
	// requires AllowExternalWrites. It exists for a future authorized writer;
	// the development server never enables it.
	WorkerExternalTransport WorkerTransport = "external"
)

// WorkerSteps selects the bounded pipeline steps of one tick. Zero values mean
// "skip"; callers that want the full development pipeline use
// DefaultWorkerSteps.
type WorkerSteps struct {
	// Expand imports one page of the fixed candidate template per draft task.
	Expand bool
	// Dispatch reserves, accepts, permits and dispatches one authorized attempt.
	Dispatch bool
	// Outbox pumps the bridge outbox into the inbox and delivers core.outbox.
	Outbox bool
	// Inbox applies pending inbound observations.
	Inbox bool
	// Recover quarantines this runtime's leftover DISPATCHING operations as
	// UNKNOWN and expires never-consumed permits before dispatching. It runs on
	// the first tick of a Worker instance, and a pass that failed any runtime
	// group stays pending so a later tick retries it.
	Recover bool
	// Reconcile writes the existing read-only timeout audit and acknowledges
	// operator reconciliation requests read-only.
	Reconcile bool
	// Deadlines runs one bounded SweepDeadlines pass.
	Deadlines bool
	// Proposal consumes durable proposal-mode work items through the shared
	// Reserve/AcceptOperation/IssuePermit/Dispatch chain. It requires the
	// explicit ProposalExecution gate and never handles legacy tasks.
	Proposal bool
}

// DefaultWorkerSteps is the development pipeline: everything except recovery,
// which advances external state and is enabled explicitly.
func DefaultWorkerSteps() WorkerSteps {
	return WorkerSteps{
		Expand:    true,
		Dispatch:  true,
		Outbox:    true,
		Inbox:     true,
		Reconcile: true,
		Deadlines: true,
	}
}

// WorkerOutboxSink is the delivery callback passed to DeliverOutbox. The default
// fixture sink acknowledges locally and performs no external action.
type WorkerOutboxSink func(context.Context, string, []byte) error

// WorkerConfig is the complete worker configuration. Zero durations and zero
// batch sizes are replaced by the documented defaults; an explicit Transport is
// required for external writes.
type WorkerConfig struct {
	// WorkerID is stable per process. It is the outbox lease owner and the actor
	// recorded on worker events; two concurrent workers must not share it.
	WorkerID string
	// PollInterval is the idle delay between ticks. It is configurable and
	// bounded below so a misconfiguration cannot spin the database.
	PollInterval time.Duration
	// BatchSize bounds the tasks (and runtime actors) handled by one step.
	BatchSize int
	// PageSize bounds one template expansion page (1..500).
	PageSize int
	// PermitTTL is the dispatch permit lifetime (1s..30s, the Store limit).
	PermitTTL time.Duration
	// TickTimeout bounds one whole tick. The lease is still released when it
	// fires, because release uses its own shutdown context.
	TickTimeout time.Duration
	// ShutdownTimeout bounds the exit drain (already produced notifications
	// only; never a new dispatch or reservation).
	ShutdownTimeout time.Duration

	Transport WorkerTransport
	// Adapter is required by WorkerExternalTransport and rejected by the
	// fixture transport: enabling a real adapter must be an explicit decision.
	Adapter Adapter
	// AllowExternalWrites acknowledges that the external adapter is covered by
	// a current, specific write authorization. It is not a secret.
	AllowExternalWrites bool
	// OutboxSink receives core.outbox deliveries. Nil uses the fixture sink.
	OutboxSink WorkerOutboxSink

	// ProposalExecution is the explicit feature gate for proposal-mode
	// execution. The default is false: no proposal work is claimed, reserved or
	// dispatched until an operator turns it on.
	ProposalExecution bool

	Steps WorkerSteps
	Logf  func(format string, args ...any)
}

// WorkerTick summarises one bounded pass. Every field is a count of committed
// effects in that pass; a repeated tick over settled state reports zeros.
type WorkerTick struct {
	StartedAt time.Time
	Duration  time.Duration

	RecoveredDispatching int
	ExpiredPermits       int
	ExpandedTasks        int
	ExpandedPages        int
	Reservations         int
	ResumedReservations  int
	Dispatched           int
	BridgeAccepted       int
	InboxApplied         int
	OutboxDelivered      int
	ReconcileAudits      int
	ManualReconcileAcks  int
	DeadlineScanned      int
	DeadlineClosed       int
	ProposalSwept        int
	ProposalProjected    int
	ProposalResumed      int
	ProposalClaimed      int
	ProposalReserved     int
	ProposalDispatched   int
	Skipped              int
}

func (t WorkerTick) active() bool {
	return t.RecoveredDispatching != 0 || t.ExpiredPermits != 0 || t.ExpandedTasks != 0 ||
		t.ExpandedPages != 0 || t.Reservations != 0 || t.ResumedReservations != 0 ||
		t.Dispatched != 0 || t.BridgeAccepted != 0 || t.InboxApplied != 0 ||
		t.OutboxDelivered != 0 || t.ReconcileAudits != 0 || t.ManualReconcileAcks != 0 ||
		t.DeadlineScanned != 0 || t.DeadlineClosed != 0 || t.Skipped != 0 ||
		t.ProposalSwept != 0 || t.ProposalProjected != 0 || t.ProposalResumed != 0 ||
		t.ProposalClaimed != 0 || t.ProposalReserved != 0 || t.ProposalDispatched != 0
}

func (t WorkerTick) String() string {
	return fmt.Sprintf("expanded=%d/%d reservations=%d resumed=%d dispatched=%d bridge=%d inbox=%d outbox=%d reconcile=%d manual=%d deadlines=%d/%d skipped=%d recovered=%d expired=%d duration=%s",
		t.ExpandedTasks, t.ExpandedPages, t.Reservations, t.ResumedReservations, t.Dispatched,
		t.BridgeAccepted, t.InboxApplied, t.OutboxDelivered, t.ReconcileAudits,
		t.ManualReconcileAcks, t.DeadlineScanned, t.DeadlineClosed, t.Skipped,
		t.RecoveredDispatching, t.ExpiredPermits, t.Duration.Round(time.Millisecond)) +
		fmt.Sprintf(" proposal[swept=%d projected=%d resumed=%d claimed=%d reserved=%d dispatched=%d]",
			t.ProposalSwept, t.ProposalProjected, t.ProposalResumed,
			t.ProposalClaimed, t.ProposalReserved, t.ProposalDispatched)
}

// Worker is a bounded deterministic loop over one Store.
type Worker struct {
	store   *Store
	cfg     WorkerConfig
	adapter Adapter

	stop    chan struct{}
	mu      sync.Mutex
	tickMu  sync.Mutex
	once    sync.Once
	running bool
	// recoverPending is set while the instance still owes a recovery pass; it is
	// cleared only by a lap that reached the end of the active population with no
	// unfinished group, which the durable per-consumer cursor records.
	recoverPending bool
	// recoveryStarted records that this instance has already marked an inherited
	// lap as incomplete, so its first pass cannot close on a partial lap.
	recoveryStarted bool
	// recoveryGeneration is the active-population generation observed when the
	// current recovery lap started. A lap that closes after the population changed
	// cannot complete the pass; it only costs the next lap.
	recoveryGeneration    int64
	recoveryGenerationSet bool
}

// NewWorker validates the configuration and resolves the transport. It starts no
// work.
func NewWorker(store *Store, cfg WorkerConfig) (*Worker, error) {
	if store == nil || store.Pool == nil {
		return nil, problem("VALIDATION_FAILED")
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = fmt.Sprintf("worker-%d", os.Getpid())
	}
	if len(cfg.WorkerID) > 96 {
		return nil, problem("VALIDATION_FAILED")
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = workerDefaultPoll
	}
	if cfg.PollInterval < workerMinPoll {
		return nil, problem("VALIDATION_FAILED")
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = workerDefaultBatch
	}
	if cfg.BatchSize < 1 || cfg.BatchSize > workerMaxBatch {
		return nil, problem("VALIDATION_FAILED")
	}
	if cfg.PageSize == 0 {
		cfg.PageSize = workerDefaultPage
	}
	if cfg.PageSize < 1 || cfg.PageSize > 500 {
		return nil, problem("VALIDATION_FAILED")
	}
	if cfg.PermitTTL == 0 {
		cfg.PermitTTL = workerDefaultPermitTTL
	}
	if cfg.PermitTTL < time.Second || cfg.PermitTTL > 30*time.Second {
		return nil, problem("VALIDATION_FAILED")
	}
	if cfg.TickTimeout == 0 {
		cfg.TickTimeout = workerDefaultTickTimeout
	}
	if cfg.TickTimeout < time.Second {
		return nil, problem("VALIDATION_FAILED")
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = workerDefaultShutdownTimeout
	}
	if cfg.ShutdownTimeout < time.Second {
		return nil, problem("VALIDATION_FAILED")
	}
	if cfg.Transport == "" {
		cfg.Transport = WorkerFixtureTransport
	}
	adapter, err := cfg.resolveAdapter()
	if err != nil {
		return nil, err
	}
	if cfg.Steps.Proposal && !cfg.ProposalExecution {
		// The proposal consumer is opt-in; enabling it without the gate would
		// make a deployment claim durable work it never authorized.
		return nil, problem("VALIDATION_FAILED")
	}
	if cfg.OutboxSink == nil {
		cfg.OutboxSink = fixtureOutboxSink
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Worker{
		store:          store,
		cfg:            cfg,
		adapter:        adapter,
		stop:           make(chan struct{}),
		recoverPending: cfg.Steps.Recover,
	}, nil
}

// resolveAdapter enforces the transport decision. The fixture transport is the
// only default and refuses a caller adapter; the external transport requires
// both an adapter and the explicit write-authorization acknowledgement.
func (c WorkerConfig) resolveAdapter() (Adapter, error) {
	switch c.Transport {
	case "", WorkerFixtureTransport:
		if c.Adapter != nil {
			return nil, problem("VALIDATION_FAILED")
		}
		return fixtureTransport{}, nil
	case WorkerExternalTransport:
		if c.Adapter == nil || !c.AllowExternalWrites {
			return nil, problem("ACTION_FORBIDDEN")
		}
		return c.Adapter, nil
	default:
		return nil, problem("VALIDATION_FAILED")
	}
}

// Stop requests a graceful stop. The current tick is never interrupted; once it
// finishes the loop drains already-produced notifications and returns. It is
// safe to call more than once and before Run.
func (w *Worker) Stop() {
	if w == nil {
		return
	}
	w.once.Do(func() { close(w.stop) })
}

// Run executes ticks immediately and then every PollInterval until ctx is done
// or Stop is called. It returns nil on an orderly stop. Tick errors are logged
// and never terminate the loop, because one unavailable account or one
// conflicting observation must not stop the whole worker.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil || w.store == nil {
		return problem("VALIDATION_FAILED")
	}
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return problem("WORKER_ALREADY_RUNNING")
	}
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	w.cfg.Logf("worker %s started poll=%s batch=%d transport=%s", w.cfg.WorkerID, w.cfg.PollInterval, w.cfg.BatchSize, w.cfg.Transport)
	w.loop(ctx)
	w.drain(ctx)
	w.cfg.Logf("worker %s stopped", w.cfg.WorkerID)
	return nil
}

func (w *Worker) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-timer.C:
		}
		if ctx.Err() != nil || w.stopped() {
			return
		}
		tickCtx, cancel := context.WithTimeout(ctx, w.cfg.TickTimeout)
		tick, err := w.Tick(tickCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			w.cfg.Logf("worker %s tick failed: %v", w.cfg.WorkerID, err)
		} else if tick.active() {
			w.cfg.Logf("worker %s tick %s", w.cfg.WorkerID, tick.String())
		}
		timer.Reset(w.cfg.PollInterval)
	}
}

func (w *Worker) stopped() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

// drain consumes only already-produced local notifications on the way out. It
// never reserves, dispatches or schedules a retry, and it never delivers a task
// event to an outbox sink, so a shutdown cannot create a new external action.
// Undelivered core.outbox rows keep their state and are delivered on the next
// start.
func (w *Worker) drain(ctx context.Context) {
	// The shutdown signal has already cancelled ctx; draining local
	// notifications is part of an orderly stop, so it gets its own bounded
	// context instead of the cancelled one.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.ShutdownTimeout)
	defer cancel()
	if w.cfg.Steps.Outbox {
		if _, err := w.pumpBridgeOutbox(drainCtx, &WorkerTick{}); err != nil {
			w.cfg.Logf("worker %s drain bridge outbox: %v", w.cfg.WorkerID, err)
		}
	}
	if w.cfg.Steps.Inbox {
		if _, err := w.applyInbox(drainCtx, &WorkerTick{}); err != nil {
			w.cfg.Logf("worker %s drain inbox: %v", w.cfg.WorkerID, err)
		}
	}
}

// Tick runs one bounded pass. Steps run in a fixed order so a reservation made
// in this pass has its receipt, bridge outbox row and inbox fact consumed before
// the deadline sweep looks at the task. A failing step never skips the remaining
// independent steps; the joined error is returned for logging.
func (w *Worker) Tick(ctx context.Context) (WorkerTick, error) {
	tick := WorkerTick{StartedAt: time.Now().UTC()}
	if w == nil || w.store == nil {
		return tick, problem("VALIDATION_FAILED")
	}
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	steps := []struct {
		enabled bool
		run     func(context.Context, *WorkerTick) error
	}{
		{w.cfg.Steps.Recover, w.stepRecover},
		{w.cfg.Steps.Expand, w.stepExpand},
		{w.cfg.Steps.Dispatch, w.stepDispatch},
		{w.cfg.Steps.Proposal, w.stepProposal},
		{w.cfg.Steps.Outbox, w.stepPumpBridgeOutbox},
		{w.cfg.Steps.Inbox, w.stepApplyInbox},
		{w.cfg.Steps.Reconcile, w.stepReconcile},
		{w.cfg.Steps.Deadlines, w.stepDeadlines},
		{w.cfg.Steps.Outbox, w.stepDeliverOutbox},
	}
	var errs []error
	for _, step := range steps {
		if !step.enabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := step.run(ctx, &tick); err != nil {
			errs = append(errs, err)
		}
	}
	tick.Duration = time.Since(tick.StartedAt)
	return tick, errors.Join(errs...)
}

// recoveryPending reports whether this Worker instance still owes its recovery
// pass.
func (w *Worker) recoveryPending() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recoverPending
}

// recoveryNeedsStart reports whether this instance still has to mark an
// inherited lap as incomplete.
func (w *Worker) recoveryNeedsStart() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.recoveryStarted
}

// recoveryLapGeneration records gen as the population baseline of the current lap
// and returns the baseline in force. A lap that started in an earlier segment,
// tick or process keeps its original baseline.
func (w *Worker) recoveryLapGeneration(gen int64) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.recoveryGenerationSet {
		w.recoveryGeneration = gen
		w.recoveryGenerationSet = true
	}
	return w.recoveryGeneration
}

// closeRecoveryLap makes gen the baseline of the next lap and reports whether the
// active population changed during the lap that just closed, which means that lap
// did not sweep the population a passing process would have to cover.
func (w *Worker) closeRecoveryLap(gen int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	changed := w.recoveryGenerationSet && gen != w.recoveryGeneration
	w.recoveryGeneration = gen
	w.recoveryGenerationSet = true
	return changed
}

// markRecoveryStarted records that this instance has taken over the rotation.
func (w *Worker) markRecoveryStarted() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recoveryStarted = true
}

// completeRecovery marks the instance's recovery pass as done. It runs only for a
// lap that closed cleanly over the whole active population, so an unfinished
// group keeps the pass retryable on the next lap instead of being consumed
// before the work.
func (w *Worker) completeRecovery() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recoverPending = false
}

// workerDB is the database surface the rotation uses. The recovery segment passes
// its lease connection, so every cursor and lap write runs on the session that
// holds the per-consumer lock and fails outright if that session died; the bridge
// pump passes the shared pool, which needs no exclusion.
type workerDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// workerCursor is one position in the active runtime-credential keyset
// (workspace_id, runtime_id, owner_epoch). The zero value is "before the first
// group", which is also the start of a lap.
type workerCursor struct {
	WorkspaceID, RuntimeID string
	OwnerEpoch             int64
}

func actorCursor(a RuntimeActor) workerCursor {
	return workerCursor{WorkspaceID: a.WorkspaceID, RuntimeID: a.RuntimeID, OwnerEpoch: a.OwnerEpoch}
}

// runtimeRotation is the bounded result of one tick's walk over the active
// runtime/owner groups. Actors holds at most workerMaxRuntimePages pages of
// limit groups each; LapClosed reports that the walk reached the end of the
// active population, so the next tick starts a new lap at the head. From and To
// are the stored position at the start of the walk and the position to store
// once its work has been attempted.
type runtimeRotation struct {
	Actors    []RuntimeActor
	From, To  workerCursor
	LapClosed bool
}

// runtimeActors enumerates the active runtime/owner groups the worker may speak
// for. Only persisted, active credentials are returned; the worker never
// promotes one, and the owner epoch is part of the group key, so a superseded
// owner is a different group and never inherits the old one's work.
//
// The walk is a bounded keyset page loop over the consumer's durable cursor.
// Each page holds at most limit groups; one tick reads at most
// workerMaxRuntimePages pages, so the work per tick is bounded even when the
// population is large. The caller stores the returned position after it has
// attempted the work, so a cancelled or crashed tick repeats its window instead
// of skipping it, and a group whose work failed is still left behind the cursor
// so the rotation cannot be held back by one group. Reaching the end of the
// population (a short or empty page) closes the lap and returns the head as the
// next position.
//
// Enumeration itself is read-only: each actor's work is its own transaction, so
// a failed actor stays pending and a retry runs only the unfinished work.
func (w *Worker) runtimeActors(ctx context.Context, db workerDB, consumer string, limit int) (runtimeRotation, error) {
	var rotation runtimeRotation
	if limit < 1 {
		limit = 1
	}
	if limit > workerMaxRuntimeActors {
		limit = workerMaxRuntimeActors
	}
	start, err := w.loadRuntimeCursor(ctx, db, consumer)
	if err != nil {
		return rotation, err
	}
	rotation.Actors = make([]RuntimeActor, 0, limit)
	rotation.From = start
	pos := start
	for range workerMaxRuntimePages {
		page, err := w.scanRuntimeActors(ctx, db, pos, limit)
		if err != nil {
			return rotation, err
		}
		rotation.Actors = append(rotation.Actors, page...)
		if len(page) < limit {
			// The active population ends here: this lap is complete and the next
			// tick starts again at the head.
			rotation.LapClosed = true
			pos = workerCursor{}
			break
		}
		pos = actorCursor(page[len(page)-1])
	}
	rotation.To = pos
	return rotation, nil
}

// scanRuntimeActors reads one bounded page of active groups strictly after the
// given keyset position. The tuple comparison matches the column order of the
// partial runtime_credentials_active_rotation index, so the planner can turn the
// page into an index range read that starts at the position instead of filtering
// the whole prefix. The rows read are the active credential rows of the groups in
// that range; only one actor row per group reaches the caller.
func (w *Worker) scanRuntimeActors(ctx context.Context, db workerDB, after workerCursor, limit int) ([]RuntimeActor, error) {
	rows, err := db.Query(ctx, `
		SELECT workspace_id,runtime_id,owner_epoch,array_agg(account_id::text ORDER BY account_id::text)::text[]
		  FROM core.runtime_credentials
		 WHERE active
		   AND (workspace_id,runtime_id,owner_epoch) > ($1::text,$2::text,$3::bigint)
		 GROUP BY workspace_id,runtime_id,owner_epoch
		 ORDER BY workspace_id,runtime_id,owner_epoch
		 LIMIT $4`, after.WorkspaceID, after.RuntimeID, after.OwnerEpoch, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := make([]RuntimeActor, 0, limit)
	for rows.Next() {
		var actor RuntimeActor
		if err := rows.Scan(&actor.WorkspaceID, &actor.RuntimeID, &actor.OwnerEpoch, &actor.AccountIDs); err != nil {
			return nil, err
		}
		page = append(page, actor)
	}
	return page, rows.Err()
}

// loadRuntimeCursor reads the consumer's durable position. A consumer that never
// wrote one starts at the head.
func (w *Worker) loadRuntimeCursor(ctx context.Context, db workerDB, consumer string) (workerCursor, error) {
	var c workerCursor
	err := db.QueryRow(ctx, `
		SELECT workspace_id,runtime_id,owner_epoch
		  FROM core.worker_cursor
		 WHERE consumer=$1`, consumer).Scan(&c.WorkspaceID, &c.RuntimeID, &c.OwnerEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return workerCursor{}, nil
	}
	if err != nil {
		return workerCursor{}, err
	}
	return c, nil
}

// runtimePopulationGeneration reads the active-population counter. Every
// credential insert, update and delete bumps it, so comparing it across a lap is
// an exact and constant-cost way to learn that the population changed while the
// lap ran.
func (w *Worker) runtimePopulationGeneration(ctx context.Context, db workerDB) (int64, error) {
	var generation int64
	if err := db.QueryRow(ctx, `SELECT generation FROM core.runtime_credential_population WHERE only_row`).Scan(&generation); err != nil {
		return 0, err
	}
	return generation, nil
}

// saveRuntimeCursor advances the consumer's position in one statement. The
// upsert seeds the row on first use and only replaces a position that still
// matches the one this walk read, so a concurrent worker instance can never
// rewind another instance's progress. A lost compare-and-swap is not an error:
// both instances would have covered the same window, every actor's work is its
// own idempotent transaction, and the winner's position is the one that counts.
//
// Two properties follow from the compare-and-swap. A stored non-zero position
// always means "the current lap has already covered everything up to here",
// because a walk that closes a lap writes the head back; a stale instance whose
// window was overtaken therefore either matches the stored position (its window
// is the same one) or loses the swap and writes nothing. And a group can only
// fall behind the stored position after it has been enumerated, because a window
// is a contiguous keyset range, so the worst case of an overlapping window is
// duplicate idle work, never a skipped group.
func (w *Worker) saveRuntimeCursor(ctx context.Context, db workerDB, consumer string, from, to workerCursor) error {
	if from == to {
		return nil
	}
	_, err := db.Exec(ctx, `
		INSERT INTO core.worker_cursor(consumer,workspace_id,runtime_id,owner_epoch,updated_at)
		VALUES($1,$2,$3,$4,clock_timestamp())
		ON CONFLICT (consumer) DO UPDATE
		   SET workspace_id=EXCLUDED.workspace_id,
		       runtime_id=EXCLUDED.runtime_id,
		       owner_epoch=EXCLUDED.owner_epoch,
		       updated_at=EXCLUDED.updated_at
		 WHERE (core.worker_cursor.workspace_id,core.worker_cursor.runtime_id,core.worker_cursor.owner_epoch)=($5::text,$6::text,$7::bigint)`,
		consumer,
		to.WorkspaceID, to.RuntimeID, to.OwnerEpoch,
		from.WorkspaceID, from.RuntimeID, from.OwnerEpoch)
	return err
}

// markRecoveryLapIncomplete durably marks the current recovery lap as not
// eligible to close the pass. Two events raise it: a group that could not be
// quarantined, and a lap inherited from a previous process. The obligation
// survives the rest of the tick, a restart and a lap boundary, so a later clean
// page can never be mistaken for a complete one.
func (w *Worker) markRecoveryLapIncomplete(ctx context.Context, db workerDB) error {
	_, err := db.Exec(ctx, `
		INSERT INTO core.worker_cursor(consumer,lap_incomplete)
		VALUES($1,true)
		ON CONFLICT (consumer) DO UPDATE
		   SET lap_incomplete=true, updated_at=clock_timestamp()
		 WHERE NOT core.worker_cursor.lap_incomplete`, workerCursorRecovery)
	return err
}

// inheritRecoveryLap marks a lap this process did not start as incomplete, so its
// first pass cannot close on the tail of a lap whose earlier pages another
// process never finished. A lap that starts at the head needs no marker: it is
// already a full sweep.
func (w *Worker) inheritRecoveryLap(ctx context.Context, db workerDB) error {
	_, err := db.Exec(ctx, `
		UPDATE core.worker_cursor
		   SET lap_incomplete=true, updated_at=clock_timestamp()
		 WHERE consumer=$1
		   AND NOT lap_incomplete
		   AND (workspace_id<>'' OR runtime_id<>'' OR owner_epoch<>0)`, workerCursorRecovery)
	return err
}

// claimRecoveryLap clears the incomplete marker for the closing lap and reports
// whether that lap was clean. A false result means some earlier page of the same
// lap failed, or the lap was inherited, so the pass stays pending and the next
// lap, which starts at the head, retries the groups before completion.
func (w *Worker) claimRecoveryLap(ctx context.Context, db workerDB) (bool, error) {
	tag, err := db.Exec(ctx, `
		UPDATE core.worker_cursor
		   SET lap_incomplete=false, updated_at=clock_timestamp()
		 WHERE consumer=$1 AND lap_incomplete`, workerCursorRecovery)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 0, nil
}

func runtimeActorFor(r Reservation) RuntimeActor {
	return RuntimeActor{
		WorkspaceID: r.WorkspaceID,
		RuntimeID:   r.RuntimeID,
		OwnerEpoch:  r.OwnerEpoch,
		AccountIDs:  []string{r.AccountID},
	}
}
