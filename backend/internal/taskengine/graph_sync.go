package taskengine

// ARTEX dual-graph sync: durable root bindings, provenance anchors, an external
// node read model, a transactional local outbox and per-partition checkpoints.
//
// Boundaries this file keeps:
//   - PostgreSQL remains the execution fact authority. Nothing here writes an
//     execution, a permit, a reservation or a QQ action; the external graph is
//     reference data and the only external mutation is an ARTEX goal node.
//   - A task syncs only after an explicit root binding written through
//     BindGraphRoot. There is no default root and no "local graph" fallback: a
//     missing provider or an unbound task is reported as such.
//   - Every external intent is written to core.graph_sync_outbox in the same
//     transaction as the decision that produced it. The call itself happens
//     outside any transaction (see graph_sync_worker.go).
//   - A write whose external outcome was not answered unambiguously moves to
//     needs_reconcile. It is never silently retried: ARTEX publishes no
//     idempotency key or unique caller label for goal writes, so a replay could
//     create a second goal.
//   - Trusted facts and external inference are separate rows with separate
//     provenance. An inference mapping never overwrites a trusted fact anchor,
//     and group text read from the external graph never becomes an instruction.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"bba.local/taskengine/internal/artex"
	"github.com/jackc/pgx/v5"
)

const (
	graphSyncProviderArtex = "artex"
	graphSyncKindFact      = "fact"
	graphSyncKindGoal      = "goal"

	graphSyncSourceOutbox  = "core.outbox"
	graphSyncSourceSummary = "group_summary"

	// Failure codes. They are stable, operator-visible strings; the HTTP layer
	// in graph_sync_http.go maps them to statuses and never rewords them.
	graphSyncCodeNotConfigured   = "GRAPH_SYNC_NOT_CONFIGURED"
	graphSyncCodeRootNotBound    = "GRAPH_SYNC_ROOT_NOT_BOUND"
	graphSyncCodeWritesDisabled  = "GRAPH_SYNC_WRITES_DISABLED"
	graphSyncCodeUnauthorized    = "GRAPH_SYNC_PROVIDER_UNAUTHORIZED"
	graphSyncCodeForbidden       = "GRAPH_SYNC_PROVIDER_FORBIDDEN"
	graphSyncCodeNotFound        = "GRAPH_SYNC_PROVIDER_NOT_FOUND"
	graphSyncCodeProtocol        = "GRAPH_SYNC_PROVIDER_PROTOCOL"
	graphSyncCodeUnavailable     = "GRAPH_SYNC_PROVIDER_UNAVAILABLE"
	graphSyncCodeRejected        = "GRAPH_SYNC_PROVIDER_REJECTED"
	graphSyncCodeOutcomeUnknown  = "GRAPH_SYNC_OUTCOME_UNKNOWN"
	graphSyncCodeGoalUnmapped    = "GRAPH_SYNC_GOAL_UNMAPPED"
	graphSyncCodeSnapshotStale   = "GRAPH_SYNC_SNAPSHOT_STALE"
	graphSyncCodeSnapshotExpired = "GRAPH_SYNC_SNAPSHOT_EXPIRED"
	graphSyncCodePlanningBudget  = "GRAPH_SYNC_PLANNING_BUDGET_EXHAUSTED"

	graphSyncEventBatch       = 50
	graphSyncSummaryBatch     = 50
	graphSyncLeaseSeconds     = 30
	graphSyncMaxBatch         = 100
	graphSyncDefaultBatch     = 20
	graphSyncMaxAttempts      = 3
	graphSyncMaxGraphNodes    = 2000
	graphSyncMaxGraphEdges    = 5000
	graphSyncMaxNodesStored   = 2000
	graphSyncSnapshotTTL      = 15 * time.Minute
	graphSyncSnapshotRounds   = 6
	graphSyncSnapshotWindow   = time.Hour
	graphSyncMaxCandidates    = 20
	graphSyncMaxSummaryRefs   = 64
	graphSyncMaxAnchorIDs     = 256
	graphSyncPlanningTemplate = "graph_planning_v1"
	graphSyncPlanningModel    = "none" // no model call is made to build a snapshot

	graphSyncLockNamespace = 740412
)

// GraphSyncProvider is the verified external graph surface the worker and the
// store use. *artex.Client implements it; tests point the same client at the
// artexfixture protocol server. A nil provider is the "not configured" state and
// every entry point reports it instead of degrading to a local graph.
type GraphSyncProvider interface {
	Wired() bool
	Capabilities() artex.Capabilities
	Graph(ctx context.Context, taskID string) (artex.Graph, error)
	Goals(ctx context.Context, taskID string) ([]artex.Goal, error)
	AddGoal(ctx context.Context, taskID, text, vulnClass string) (artex.Goal, error)
	UpdateGoal(ctx context.Context, taskID string, goalID int64, text, vulnClass string) (artex.Goal, error)
	DeleteGoal(ctx context.Context, taskID string, goalID int64) error
}

// GraphSyncProviderStatus is the configuration half of the status document. The
// store never invents it; the HTTP/worker caller fills it from the real client.
type GraphSyncProviderStatus struct {
	Provider      string `json:"provider"`
	Configured    bool   `json:"configured"`
	Wired         bool   `json:"wired"`
	WritesAllowed bool   `json:"writes_allowed"`
}

// GraphSyncError is the last external failure recorded on a root.
type GraphSyncError struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// GraphSyncCheckpoint is one durable consumption offset.
type GraphSyncCheckpoint struct {
	Source    string    `json:"source"`
	ScopeKey  string    `json:"scope_key"`
	Offset    int64     `json:"offset"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GraphSyncGraphStatus is one graph kind's operator projection.
type GraphSyncGraphStatus struct {
	GraphKind            string                `json:"graph_kind"`
	Bound                bool                  `json:"bound"`
	ExternalRoot         *string               `json:"external_root"`
	SyncState            string                `json:"sync_state"`
	TaskVersion          int                   `json:"task_version"`
	ScopeRevision        int                   `json:"scope_revision"`
	GraphRevision        int64                 `json:"graph_revision"`
	LastSyncAt           *time.Time            `json:"last_sync_at"`
	SyncLagSeconds       *int64                `json:"sync_lag_seconds"`
	Anchors              int64                 `json:"anchors"`
	ExternalNodes        int64                 `json:"external_nodes"`
	PendingOutbox        int64                 `json:"pending_outbox"`
	NeedsReconcileOutbox int64                 `json:"needs_reconcile_outbox"`
	FailedOutbox         int64                 `json:"failed_outbox"`
	LastError            *GraphSyncError       `json:"last_error"`
	Checkpoints          []GraphSyncCheckpoint `json:"checkpoints"`
}

// GraphSyncStatus is the whole read projection for one task.
type GraphSyncStatus struct {
	WorkspaceID string                  `json:"workspace_id"`
	TaskID      string                  `json:"task_id"`
	GeneratedAt time.Time               `json:"generated_at"`
	Provider    GraphSyncProviderStatus `json:"provider"`
	Graphs      []GraphSyncGraphStatus  `json:"graphs"`
}

// GraphRootRequest binds one graph kind to one external ARTEX task id.
type GraphRootRequest struct {
	ExternalRoot string `json:"external_root"`
}

// GraphSyncOutboxItem is one durable external intent.
type GraphSyncOutboxItem struct {
	WorkspaceID    string          `json:"workspace_id"`
	TaskID         string          `json:"task_id"`
	ID             string          `json:"id"`
	GraphKind      string          `json:"graph_kind"`
	Operation      string          `json:"operation"`
	IdempotencyKey string          `json:"idempotency_key"`
	Payload        json.RawMessage `json:"payload"`
	PayloadDigest  string          `json:"payload_digest"`
	Scope          string          `json:"scope"`
	Sequence       int64           `json:"sequence"`
	State          string          `json:"state"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	ExternalNodeID *string         `json:"external_node_id,omitempty"`
	LastErrorCode  *string         `json:"last_error_code,omitempty"`
	LastError      *string         `json:"last_error,omitempty"`
	NextAttemptAt  time.Time       `json:"next_attempt_at"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// GraphSyncReconcileRequest resolves one ambiguous external write by readback.
// Outcome is "applied" or "not_applied"; the store verifies it against the
// provider's current goal list and never sends a mutation.
type GraphSyncReconcileRequest struct {
	Outcome        string `json:"outcome"`
	ExternalNodeID string `json:"external_node_id"`
}

type graphRootRow struct {
	ExternalRoot  string
	GraphRevision int64
	LastSyncAt    *time.Time
	LastErrorCode *string
	LastError     *string
	LastErrorAt   *time.Time
}

var errGraphRootMissing = errors.New("graph root is not bound")

func graphSyncKindValid(kind string) bool {
	return kind == graphSyncKindFact || kind == graphSyncKindGoal
}

// graphAdvisoryKey hashes advisory-lock inputs into NUL-free text. PostgreSQL
// text parameters reject 0x00, so a joined key can never be sent verbatim.
func graphAdvisoryKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// graphTaskTerminal reports task states where a goal write could only orphan an
// external goal: the task will emit no further progress or cancel event.
func graphTaskTerminal(state string) bool {
	switch state {
	case "CANCELLING", "CANCELLED", "SUCCEEDED", "PARTIAL", "FAILED":
		return true
	}
	return false
}

// loadGraphRoot reads the binding row inside a caller transaction.
func loadGraphRoot(ctx context.Context, tx pgx.Tx, workspace, taskID, kind string) (graphRootRow, bool, error) {
	var root graphRootRow
	err := tx.QueryRow(ctx, `
		SELECT external_root,graph_revision,last_sync_at,last_error_code,last_error,last_error_at
		FROM core.task_graph_roots
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
		workspace, taskID, kind, graphSyncProviderArtex,
	).Scan(&root.ExternalRoot, &root.GraphRevision, &root.LastSyncAt, &root.LastErrorCode, &root.LastError, &root.LastErrorAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return graphRootRow{}, false, nil
	}
	if err != nil {
		return graphRootRow{}, false, err
	}
	return root, true, nil
}

// BindGraphRoot writes or replays one explicit root binding. A rebind to a
// different external root is refused once the task has any sync state for that
// graph kind, because silently repointing would orphan every mapping and leave
// pending writes addressed to the old root.
func (s *Store) BindGraphRoot(ctx context.Context, actor Actor, taskID, graphKind, key string, request GraphRootRequest) (GraphSyncStatus, error) {
	if actor.RuntimeID != "" {
		return GraphSyncStatus{}, problem(taskCodeForbidden)
	}
	if err := actor.require("task.control"); err != nil {
		return GraphSyncStatus{}, err
	}
	if err := taskCheckIdempotencyKey(key); err != nil {
		return GraphSyncStatus{}, err
	}
	if !taskValidID(taskID) {
		return GraphSyncStatus{}, problem(taskCodeNotFound)
	}
	kind := strings.TrimSpace(graphKind)
	if !graphSyncKindValid(kind) {
		return GraphSyncStatus{}, problem(taskCodeValidation)
	}
	root := strings.TrimSpace(request.ExternalRoot)
	if root == "" || root == "active" || len(root) > 128 {
		return GraphSyncStatus{}, problem(taskCodeValidation)
	}
	bodyDigest, err := digest(struct {
		GraphKind    string `json:"graph_kind"`
		ExternalRoot string `json:"external_root"`
	}{kind, root})
	if err != nil {
		return GraphSyncStatus{}, err
	}
	path := "/v1/tasks/" + taskID + "/graph-sync/roots/" + kind
	var out GraphSyncStatus
	err = s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.control"); err != nil {
			return err
		}
		rec, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "PUT", path, key)
		if err != nil {
			return err
		}
		if found {
			if rec.digest != bodyDigest {
				return problem(taskCodeIdempotency)
			}
			if err := json.Unmarshal(rec.response, &out); err != nil {
				return problem(taskCodeStoreUnavailable)
			}
			return nil
		}
		if kind == graphSyncKindGoal && graphTaskTerminal(task.State) {
			// Creating a goal for a task that can no longer act would leave an
			// external goal nothing will ever close.
			return problem(taskCodeStateConflict)
		}
		var other string
		err = tx.QueryRow(ctx, `
			SELECT task_id FROM core.task_graph_roots
			WHERE workspace_id=$1 AND provider=$2 AND graph_kind=$3 AND external_root=$4 AND task_id<>$5`,
			actor.WorkspaceID, graphSyncProviderArtex, kind, root, taskID,
		).Scan(&other)
		if err == nil {
			// One external graph cannot back two local tasks: their goal writes
			// would interleave with no way to attribute them.
			return problem(taskCodeStateConflict)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var existing string
		err = tx.QueryRow(ctx, `
			SELECT external_root FROM core.task_graph_roots
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
			actor.WorkspaceID, taskID, kind, graphSyncProviderArtex,
		).Scan(&existing)
		switch {
		case err == nil && existing == root:
			// Replay of the same bind: no root change, no new state.
		case err == nil:
			var used int64
			if err := tx.QueryRow(ctx, `
                SELECT
                    (SELECT count(*) FROM core.graph_sync_outbox WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4 AND state<>'canceled')
                  + (SELECT count(*) FROM core.graph_sync_nodes WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4)
                  + (SELECT count(*) FROM core.task_graph_anchors WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4)`,
				actor.WorkspaceID, taskID, kind, graphSyncProviderArtex,
			).Scan(&used); err != nil {
				return err
			}
			if used > 0 {
				return problem(taskCodeStateConflict)
			}
			if _, err := tx.Exec(ctx, `
                UPDATE core.task_graph_roots
                SET external_root=$5,graph_revision=0,last_sync_at=NULL,last_error_code=NULL,last_error=NULL,last_error_at=NULL,updated_at=clock_timestamp()
                WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
				actor.WorkspaceID, taskID, kind, graphSyncProviderArtex, root); err != nil {
				return err
			}
			// Offsets belong to the old root's sources.
			if _, err := tx.Exec(ctx, `
                DELETE FROM core.graph_sync_checkpoints
                WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
				actor.WorkspaceID, taskID, kind, graphSyncProviderArtex); err != nil {
				return err
			}
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.Exec(ctx, `
                INSERT INTO core.task_graph_roots(workspace_id,task_id,graph_kind,provider,external_root)
                VALUES($1,$2,$3,$4,$5)`,
				actor.WorkspaceID, taskID, kind, graphSyncProviderArtex, root); err != nil {
				return err
			}
		default:
			return err
		}
		ops := make([]graphOutboxOp, 0, 2)
		if kind == graphSyncKindFact {
			ops = append(ops, graphOutboxOp{
				GraphKind: kind,
				Operation: "graph.pull",
				Key:       "bind:" + root,
				Payload:   map[string]any{"reason": "root_bound", "external_root": root},
			})
		} else {
			ops = append(ops, goalCreateOp(taskID, task.Title, task.GoalCount))
		}
		if _, err := enqueueGraphOutbox(ctx, tx, actor.WorkspaceID, taskID, ops); err != nil {
			return err
		}
		out, err = graphSyncStatusTx(ctx, tx, task)
		if err != nil {
			return err
		}
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "PUT", path, key, bodyDigest, taskID, out)
	})
	if err != nil {
		return GraphSyncStatus{}, err
	}
	return out, nil
}

// GetGraphSyncStatus authorizes against the task scope and returns the durable
// sync projection. It never contacts ARTEX; provider state is filled by the
// caller that owns the real client.
func (s *Store) GetGraphSyncStatus(ctx context.Context, actor Actor, taskID string) (GraphSyncStatus, error) {
	if actor.RuntimeID != "" {
		return GraphSyncStatus{}, problem(taskCodeForbidden)
	}
	if err := actor.require("task.read"); err != nil {
		return GraphSyncStatus{}, err
	}
	if !taskValidID(taskID) {
		return GraphSyncStatus{}, problem(taskCodeNotFound)
	}
	var out GraphSyncStatus
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.read"); err != nil {
			return err
		}
		out, err = graphSyncStatusTx(ctx, tx, task)
		return err
	})
	if err != nil {
		return GraphSyncStatus{}, err
	}
	return out, nil
}

func graphSyncStatusTx(ctx context.Context, tx pgx.Tx, task TaskRow) (GraphSyncStatus, error) {
	now := time.Now().UTC()
	out := GraphSyncStatus{
		WorkspaceID: task.WorkspaceID,
		TaskID:      task.ID,
		GeneratedAt: now,
		Provider:    GraphSyncProviderStatus{Provider: graphSyncProviderArtex},
		Graphs:      make([]GraphSyncGraphStatus, 0, 2),
	}
	for _, kind := range []string{graphSyncKindFact, graphSyncKindGoal} {
		graph := GraphSyncGraphStatus{
			GraphKind:     kind,
			TaskVersion:   task.Version,
			ScopeRevision: task.ScopeRevision,
			Checkpoints:   []GraphSyncCheckpoint{},
		}
		root, bound, err := loadGraphRoot(ctx, tx, task.WorkspaceID, task.ID, kind)
		if err != nil {
			return GraphSyncStatus{}, err
		}
		if bound {
			external := root.ExternalRoot
			graph.Bound = true
			graph.ExternalRoot = &external
			graph.GraphRevision = root.GraphRevision
			graph.LastSyncAt = root.LastSyncAt
			if root.LastSyncAt != nil {
				lag := int64(now.Sub(*root.LastSyncAt).Seconds())
				if lag < 0 {
					lag = 0
				}
				graph.SyncLagSeconds = &lag
			}
			if root.LastErrorCode != nil {
				at := now
				if root.LastErrorAt != nil {
					at = *root.LastErrorAt
				}
				message := ""
				if root.LastError != nil {
					message = *root.LastError
				}
				graph.LastError = &GraphSyncError{Code: *root.LastErrorCode, Message: message, At: at}
			}
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM core.task_graph_anchors
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
			task.WorkspaceID, task.ID, kind, graphSyncProviderArtex).Scan(&graph.Anchors); err != nil {
			return GraphSyncStatus{}, err
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM core.graph_sync_nodes
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
			task.WorkspaceID, task.ID, kind, graphSyncProviderArtex).Scan(&graph.ExternalNodes); err != nil {
			return GraphSyncStatus{}, err
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE state IN ('pending','sending')),
			       count(*) FILTER (WHERE state='needs_reconcile'),
			       count(*) FILTER (WHERE state='failed')
			FROM core.graph_sync_outbox
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
			task.WorkspaceID, task.ID, kind, graphSyncProviderArtex,
		).Scan(&graph.PendingOutbox, &graph.NeedsReconcileOutbox, &graph.FailedOutbox); err != nil {
			return GraphSyncStatus{}, err
		}
		rows, err := tx.Query(ctx, `
			SELECT source,scope_key,offset_value,updated_at
			FROM core.graph_sync_checkpoints
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4
			ORDER BY source,scope_key`,
			task.WorkspaceID, task.ID, kind, graphSyncProviderArtex)
		if err != nil {
			return GraphSyncStatus{}, err
		}
		for rows.Next() {
			var checkpoint GraphSyncCheckpoint
			if err := rows.Scan(&checkpoint.Source, &checkpoint.ScopeKey, &checkpoint.Offset, &checkpoint.UpdatedAt); err != nil {
				rows.Close()
				return GraphSyncStatus{}, err
			}
			graph.Checkpoints = append(graph.Checkpoints, checkpoint)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return GraphSyncStatus{}, err
		}
		rows.Close()
		graph.SyncState = graphSyncState(graph)
		out.Graphs = append(out.Graphs, graph)
	}
	return out, nil
}

func graphSyncState(graph GraphSyncGraphStatus) string {
	switch {
	case !graph.Bound:
		return "unbound"
	case graph.NeedsReconcileOutbox > 0:
		return "needs_reconcile"
	case graph.FailedOutbox > 0 || graph.LastError != nil:
		return "failed"
	case graph.PendingOutbox > 0:
		return "pending"
	case graph.LastSyncAt != nil:
		return "synced"
	default:
		return "pending"
	}
}

// ---------------------------------------------------------------- outbox

type graphOutboxOp struct {
	GraphKind   string
	Operation   string
	Key         string
	Payload     map[string]any
	MaxAttempts int
}

// goalCreateKey is the stable local key of a task's canonical external goal.
func goalCreateKey(taskID string) string { return "goal:canonical:" + taskID }

func goalCreateOp(taskID, title string, goalCount int) graphOutboxOp {
	text := strings.TrimSpace(title)
	if text == "" {
		text = "task " + taskID
	}
	return graphOutboxOp{
		GraphKind: graphSyncKindGoal,
		Operation: "goal.create",
		Key:       goalCreateKey(taskID),
		Payload: map[string]any{
			"goal_key":   goalCreateKey(taskID),
			"text":       text,
			"goal_count": goalCount,
		},
	}
}

// enqueueGraphOutbox writes external intents inside the caller's transaction.
// The advisory lock serializes sequence allocation per task so two concurrent
// enqueues cannot mint the same sequence for one partition.
func enqueueGraphOutbox(ctx context.Context, tx pgx.Tx, workspace, taskID string, ops []graphOutboxOp) (int, error) {
	if len(ops) == 0 {
		return 0, nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1,hashtext($2))`, int32(graphSyncLockNamespace), graphAdvisoryKey(workspace, taskID)); err != nil {
		return 0, err
	}
	inserted := 0
	for _, op := range ops {
		if !graphSyncKindValid(op.GraphKind) {
			return inserted, problem(taskCodeValidation)
		}
		payload, err := json.Marshal(op.Payload)
		if err != nil {
			return inserted, err
		}
		sum := hashBytes(payload)
		maxAttempts := op.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = graphSyncMaxAttempts
		}
		if maxAttempts > 10 {
			maxAttempts = 10
		}
		scope := fmt.Sprintf("task:%s:%s:%s", taskID, op.GraphKind, graphSyncProviderArtex)
		var tag string
		err = tx.QueryRow(ctx, `
			INSERT INTO core.graph_sync_outbox(
				workspace_id,task_id,graph_kind,provider,id,operation,idempotency_key,payload,payload_digest,scope,sequence,max_attempts)
            SELECT $1::core.id,$2::core.id,$3::text,$4::text,$5::core.id,$6::text,$7::text,$8::jsonb,$9::text,$10::text,
                   COALESCE((SELECT max(sequence) FROM core.graph_sync_outbox
                             WHERE workspace_id=$1::core.id AND task_id=$2::core.id AND graph_kind=$3::text AND provider=$4::text),0)+1,
			       $11
			ON CONFLICT (workspace_id,task_id,graph_kind,provider,idempotency_key) DO NOTHING
			RETURNING id`,
			workspace, taskID, op.GraphKind, graphSyncProviderArtex, newID("gso"), op.Operation, op.Key, payload, sum, scope, maxAttempts,
		).Scan(&tag)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return inserted, err
		}
		inserted++
	}
	return inserted, nil
}

func hashBytes(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- ingest

// ingestGraphSources consumes core.outbox task events and live Summary versions
// for one task and turns them into external intents, all in one transaction with
// the checkpoints that make the consumption durable.
func (s *Store) ingestGraphSources(ctx context.Context, workspace, taskID string) (int, error) {
	trusted, err := s.trustedResultCount(ctx, workspace, taskID)
	if err != nil {
		return 0, err
	}
	enqueued := 0
	err = s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, workspace, taskID)
		if err != nil {
			return err
		}
		roots := map[string]graphRootRow{}
		for _, kind := range []string{graphSyncKindFact, graphSyncKindGoal} {
			root, bound, err := loadGraphRoot(ctx, tx, workspace, taskID, kind)
			if err != nil {
				return err
			}
			if bound {
				roots[kind] = root
			}
		}
		if len(roots) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1,hashtext($2))`, int32(graphSyncLockNamespace), graphAdvisoryKey(workspace, taskID)); err != nil {
			return err
		}
		lastGoalText, err := lastGoalText(ctx, tx, workspace, taskID)
		if err != nil {
			return err
		}
		for _, kind := range []string{graphSyncKindFact, graphSyncKindGoal} {
			root, bound := roots[kind]
			if !bound {
				continue
			}
			offset, err := graphCheckpointOffset(ctx, tx, workspace, taskID, kind, graphSyncSourceOutbox, "event_seq")
			if err != nil {
				return err
			}
			events, err := pendingTaskEvents(ctx, tx, workspace, taskID, offset)
			if err != nil {
				return err
			}
			ops := make([]graphOutboxOp, 0, len(events))
			lastSeq := offset
			for _, event := range events {
				if event.Seq <= lastSeq {
					continue
				}
				lastSeq = event.Seq
				if kind == graphSyncKindFact {
					continue
				}
				eventOps, err := graphGoalOpsForEvent(task, event, trusted, lastGoalText)
				if err != nil {
					return err
				}
				for _, op := range eventOps {
					if op.Operation == "goal.update" {
						text, _ := op.Payload["text"].(string)
						if text != "" && text == lastGoalText {
							continue
						}
					}
					ops = append(ops, op)
					if op.Operation == "goal.update" {
						lastGoalText, _ = op.Payload["text"].(string)
					}
				}
			}
			if kind == graphSyncKindFact && len(events) > 0 {
				ops = append(ops, graphOutboxOp{
					GraphKind: kind,
					Operation: "graph.pull",
					Key:       fmt.Sprintf("pull:event:%d", lastSeq),
					Payload: map[string]any{
						"reason":         "task_event",
						"last_event_seq": lastSeq,
						"external_root":  root.ExternalRoot,
					},
				})
			}
			if len(events) > 0 {
				inserted, err := enqueueGraphOutbox(ctx, tx, workspace, taskID, ops)
				if err != nil {
					return err
				}
				enqueued += inserted
				if err := upsertGraphCheckpoint(ctx, tx, workspace, taskID, kind, graphSyncSourceOutbox, "event_seq", lastSeq); err != nil {
					return err
				}
			}
		}
		if _, bound := roots[graphSyncKindFact]; bound {
			summaryOps, err := s.ingestSummarySources(ctx, tx, task)
			if err != nil {
				return err
			}
			enqueued += summaryOps
		}
		return nil
	})
	return enqueued, err
}

type graphTaskEvent struct {
	Seq    int64
	ID     string
	Kind   string
	Detail map[string]any
}

// pendingTaskEvents reads the append-only task event log, ordered by its durable
// sequence. It never depends on the core outbox delivery state (another consumer
// owns that), so an event cannot disappear before this consumer sees it, and
// reading here does not alter delivery.
func pendingTaskEvents(ctx context.Context, tx pgx.Tx, workspace, taskID string, after int64) ([]graphTaskEvent, error) {
	rows, err := tx.Query(ctx, `
		SELECT seq,id,kind,detail
		FROM core.task_event
		WHERE workspace_id=$1 AND task_id=$2 AND seq>$3
		ORDER BY seq
		LIMIT $4`, workspace, taskID, after, graphSyncEventBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]graphTaskEvent, 0)
	for rows.Next() {
		var event graphTaskEvent
		var raw []byte
		if err := rows.Scan(&event.Seq, &event.ID, &event.Kind, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &event.Detail); err != nil {
				return nil, err
			}
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

// graphGoalOpsForEvent maps one trusted local event to goal-graph intents. The
// task's goal text is only ever derived from local task truth and trusted result
// count; group text never becomes an instruction.
func graphGoalOpsForEvent(task TaskRow, event graphTaskEvent, trusted int, lastText string) ([]graphOutboxOp, error) {
	switch event.Kind {
	case "task.created":
		return []graphOutboxOp{goalCreateOp(task.ID, task.Title, task.GoalCount)}, nil
	case "task.control":
		state, _ := event.Detail["to"].(string)
		if state == "" {
			state, _ = event.Detail["state"].(string)
		}
		if state == "CANCELLED" {
			// Cancellation is terminal for the goal graph: no progress update may
			// follow the delete in the same partition.
			return []graphOutboxOp{{
				GraphKind: graphSyncKindGoal,
				Operation: "goal.delete",
				Key:       "goal:cancel:" + event.ID,
				Payload:   map[string]any{"goal_key": goalCreateKey(task.ID), "reason": "task_cancelled"},
			}}, nil
		}
	}
	if task.State == "CANCELLED" {
		return nil, nil
	}
	text := graphProgressText(task, trusted)
	if text == "" || text == lastText {
		return nil, nil
	}
	return []graphOutboxOp{{
		GraphKind: graphSyncKindGoal,
		Operation: "goal.update",
		Key:       "goal:progress:" + event.ID,
		Payload:   map[string]any{"goal_key": goalCreateKey(task.ID), "text": text, "trusted_results": trusted, "goal_count": task.GoalCount},
	}}, nil
}

func graphProgressText(task TaskRow, trusted int) string {
	title := strings.TrimSpace(task.Title)
	if title == "" {
		return ""
	}
	if trusted > task.GoalCount {
		trusted = task.GoalCount
	}
	return fmt.Sprintf("%s（已确认 %d/%d）", title, trusted, task.GoalCount)
}

func graphCheckpointOffset(ctx context.Context, tx pgx.Tx, workspace, taskID, kind, source, scope string) (int64, error) {
	var offset int64
	err := tx.QueryRow(ctx, `
		SELECT offset_value FROM core.graph_sync_checkpoints
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4 AND source=$5 AND scope_key=$6`,
		workspace, taskID, kind, graphSyncProviderArtex, source, scope).Scan(&offset)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return offset, nil
}

func upsertGraphCheckpoint(ctx context.Context, tx pgx.Tx, workspace, taskID, kind, source, scope string, offset int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO core.graph_sync_checkpoints(workspace_id,task_id,graph_kind,provider,source,scope_key,offset_value)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (workspace_id,task_id,graph_kind,provider,source,scope_key) DO UPDATE
		SET offset_value=GREATEST(core.graph_sync_checkpoints.offset_value,EXCLUDED.offset_value),updated_at=clock_timestamp()`,
		workspace, taskID, kind, graphSyncProviderArtex, source, scope, offset)
	return err
}

// lastGoalText returns the most recent goal text this task pushed or read back,
// used to avoid re-issuing an identical goal.update.
func lastGoalText(ctx context.Context, tx pgx.Tx, workspace, taskID string) (string, error) {
	var text string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(payload->>'text','') FROM core.graph_sync_nodes
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4 AND local_key=$5`,
		workspace, taskID, graphSyncKindGoal, graphSyncProviderArtex, goalCreateKey(taskID)).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return text, nil
}

// trustedResultCount reads the trusted task-result count from the local
// projection after refreshing it. The projection is the only trusted count: raw
// core.result rows are not enough (graph.go owns the join that proves trust).
func (s *Store) trustedResultCount(ctx context.Context, workspace, taskID string) (int, error) {
	if _, err := s.ProjectTaskGraph(ctx, workspace, taskID); err != nil {
		return 0, err
	}
	var count int
	if err := s.Pool.QueryRow(ctx, `
		SELECT count(*) FROM core.task_graph_fact
		WHERE workspace_id=$1 AND task_id=$2 AND fact_kind='task_result'`, workspace, taskID).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// ingestSummarySources persists trusted Summary anchors and enqueues one fact
// pull per new version. It runs inside the caller transaction; the external call
// happens later, outside it.
func (s *Store) ingestSummarySources(ctx context.Context, tx pgx.Tx, task TaskRow) (int, error) {
	summaries, err := newSummaryVersions(ctx, tx, task)
	if err != nil {
		return 0, err
	}
	enqueued := 0
	for _, ref := range summaries {
		if err := upsertSummaryAnchor(ctx, tx, task, ref); err != nil {
			return enqueued, err
		}
		ops := []graphOutboxOp{{
			GraphKind: graphSyncKindFact,
			Operation: "graph.pull",
			Key:       fmt.Sprintf("summary:%s:%s:%d", ref.AccountID, ref.GroupID, ref.SummaryVersion),
			Payload: map[string]any{
				"reason":          "summary",
				"account_id":      ref.AccountID,
				"group_id":        ref.GroupID,
				"summary_id":      ref.SummaryID,
				"summary_version": ref.SummaryVersion,
			},
		}}
		inserted, err := enqueueGraphOutbox(ctx, tx, task.WorkspaceID, task.ID, ops)
		if err != nil {
			return enqueued, err
		}
		enqueued += inserted
		scope := fmt.Sprintf("account:%s:group:%s", ref.AccountID, ref.GroupID)
		if err := upsertGraphCheckpoint(ctx, tx, task.WorkspaceID, task.ID, graphSyncKindFact, graphSyncSourceSummary, scope, int64(ref.SummaryVersion)); err != nil {
			return enqueued, err
		}
	}
	return enqueued, nil
}

// newSummaryVersions selects live Summary versions newer than the per-scope
// checkpoint for account/group pairs that are both in this task's scope. The
// join keeps the read inside the exact task scope: a group the task does not
// target can never contribute an anchor or a candidate.
func newSummaryVersions(ctx context.Context, tx pgx.Tx, task TaskRow) ([]GraphPlanSummaryRef, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.account_id,s.group_id,s.summary_id,s.summary_version,s.source_manifest_sha256
		FROM core.group_summary s
		JOIN core.task_account ta
		  ON ta.workspace_id=s.workspace_id AND ta.account_id=s.account_id AND ta.task_id=$2
		JOIN core.task_target tt
		  ON tt.workspace_id=s.workspace_id AND tt.task_id=$2
		JOIN core.target t
		  ON t.workspace_id=tt.workspace_id AND t.id=tt.target_id AND t.external_id=s.group_id
		WHERE s.workspace_id=$1 AND s.expires_at>clock_timestamp()
		  AND s.summary_version>COALESCE((
		    SELECT c.offset_value FROM core.graph_sync_checkpoints c
		    WHERE c.workspace_id=s.workspace_id AND c.task_id=$2 AND c.graph_kind=$3 AND c.provider=$4
		      AND c.source='group_summary'
		      AND c.scope_key='account:'||s.account_id||':group:'||s.group_id),0)
		ORDER BY s.account_id,s.group_id,s.summary_version
		LIMIT $5`,
		task.WorkspaceID, task.ID, graphSyncKindFact, graphSyncProviderArtex, graphSyncSummaryBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]GraphPlanSummaryRef, 0)
	for rows.Next() {
		var ref GraphPlanSummaryRef
		if err := rows.Scan(&ref.AccountID, &ref.GroupID, &ref.SummaryID, &ref.SummaryVersion, &ref.ManifestSHA256); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func upsertSummaryAnchor(ctx context.Context, tx pgx.Tx, task TaskRow, ref GraphPlanSummaryRef) error {
	anchorID := graphID("anchor", task.WorkspaceID, task.ID, fmt.Sprintf("fact:%s:%s:%d", ref.AccountID, ref.GroupID, ref.SummaryVersion))
	_, err := tx.Exec(ctx, `
		INSERT INTO core.task_graph_anchors(
			workspace_id,task_id,anchor_id,graph_kind,provider,account_id,group_id,
			summary_id,summary_version,manifest_sha256,projection_sequence,provenance)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::integer,$10,$9::bigint,'fact')
		ON CONFLICT (workspace_id,task_id,anchor_id) DO UPDATE
		SET summary_id=EXCLUDED.summary_id,summary_version=EXCLUDED.summary_version,
		    manifest_sha256=EXCLUDED.manifest_sha256,
		    projection_sequence=GREATEST(core.task_graph_anchors.projection_sequence,EXCLUDED.projection_sequence),
		    updated_at=clock_timestamp()`,
		task.WorkspaceID, task.ID, anchorID, graphSyncKindFact, graphSyncProviderArtex,
		ref.AccountID, ref.GroupID, ref.SummaryID, ref.SummaryVersion, ref.ManifestSHA256)
	return err
}

// latestSummaryRefs returns the newest live Summary version per account/group in
// task scope. Planning snapshots bind exactly these versions.
func latestSummaryRefs(ctx context.Context, tx pgx.Tx, workspace, taskID string) ([]GraphPlanSummaryRef, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (s.account_id,s.group_id)
		       s.account_id,s.group_id,s.summary_id,s.summary_version,s.source_manifest_sha256
		FROM core.group_summary s
		JOIN core.task_account ta
		  ON ta.workspace_id=s.workspace_id AND ta.account_id=s.account_id AND ta.task_id=$2
		JOIN core.task_target tt
		  ON tt.workspace_id=s.workspace_id AND tt.task_id=$2
		JOIN core.target t
		  ON t.workspace_id=tt.workspace_id AND t.id=tt.target_id AND t.external_id=s.group_id
		WHERE s.workspace_id=$1 AND s.expires_at>clock_timestamp()
		ORDER BY s.account_id,s.group_id,s.summary_version DESC`,
		workspace, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]GraphPlanSummaryRef, 0)
	for rows.Next() {
		var ref GraphPlanSummaryRef
		if err := rows.Scan(&ref.AccountID, &ref.GroupID, &ref.SummaryID, &ref.SummaryVersion, &ref.ManifestSHA256); err != nil {
			return nil, err
		}
		if len(out) >= graphSyncMaxSummaryRefs {
			break
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AccountID != out[j].AccountID {
			return out[i].AccountID < out[j].AccountID
		}
		return out[i].GroupID < out[j].GroupID
	})
	return out, nil
}

// anchorIDs lists the anchors a snapshot binds, bounded and ordered.
func anchorIDs(ctx context.Context, tx pgx.Tx, workspace, taskID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT anchor_id FROM core.task_graph_anchors
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4
		ORDER BY anchor_id LIMIT $5`,
		workspace, taskID, graphSyncKindFact, graphSyncProviderArtex, graphSyncMaxAnchorIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- reconcile

// ReconcileGraphSyncItem resolves one ambiguous external write by reading the
// ARTEX goal list. It never sends a mutation: the operator states what the
// readback proves, and the store accepts only a proof that matches the current
// external state. A repeated request replays the stored result and never reads
// or writes ARTEX again.
func (s *Store) ReconcileGraphSyncItem(ctx context.Context, actor Actor, taskID, itemID, key string, request GraphSyncReconcileRequest, provider GraphSyncProvider) (GraphSyncOutboxItem, error) {
	if actor.RuntimeID != "" {
		return GraphSyncOutboxItem{}, problem(taskCodeForbidden)
	}
	if err := actor.require("task.control"); err != nil {
		return GraphSyncOutboxItem{}, err
	}
	if err := taskCheckIdempotencyKey(key); err != nil {
		return GraphSyncOutboxItem{}, err
	}
	if !taskValidID(taskID) || !taskValidID(itemID) {
		return GraphSyncOutboxItem{}, problem(taskCodeNotFound)
	}
	outcome := strings.TrimSpace(request.Outcome)
	if outcome != "applied" && outcome != "not_applied" {
		return GraphSyncOutboxItem{}, problem(taskCodeValidation)
	}
	if provider == nil || !provider.Wired() {
		return GraphSyncOutboxItem{}, problem(graphSyncCodeNotConfigured)
	}
	bodyDigest, err := digest(request)
	if err != nil {
		return GraphSyncOutboxItem{}, err
	}
	path := "/v1/tasks/" + taskID + "/graph-sync/outbox/" + itemID + "/reconcile"
	var out GraphSyncOutboxItem
	done, err := s.reconcileReplay(ctx, actor, path, key, bodyDigest, taskID, itemID, &out)
	if err != nil {
		return GraphSyncOutboxItem{}, err
	}
	if done {
		return out, nil
	}

	item, root, mappedID, err := s.readAmbiguousItem(ctx, actor, taskID, itemID)
	if err != nil {
		return GraphSyncOutboxItem{}, err
	}
	goals, err := provider.Goals(ctx, root.ExternalRoot)
	if err != nil {
		code, _ := graphSyncErrorCode(err)
		return GraphSyncOutboxItem{}, problem(code)
	}
	goalKey, text := graphOutboxGoal(item)
	resolved, err := readbackGoalOutcome(item, goalKey, text, mappedID, request, goals)
	if err != nil {
		return GraphSyncOutboxItem{}, err
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1,hashtext($2))`, int32(graphSyncLockNamespace), graphAdvisoryKey(actor.WorkspaceID, taskID, itemID)); err != nil {
			return err
		}
		rec, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key)
		if err != nil {
			return err
		}
		if found {
			if rec.digest != bodyDigest {
				return problem(taskCodeIdempotency)
			}
			if err := json.Unmarshal(rec.response, &out); err != nil {
				return problem(taskCodeStoreUnavailable)
			}
			return nil
		}
		current, err := scanOutboxItem(tx.QueryRow(ctx, `
			SELECT workspace_id,task_id,id,graph_kind,operation,idempotency_key,payload,payload_digest,scope,sequence,
			       state,attempts,max_attempts,external_node_id,last_error_code,last_error,next_attempt_at,created_at,updated_at
			FROM core.graph_sync_outbox
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3 FOR UPDATE`,
			actor.WorkspaceID, taskID, itemID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return problem(taskCodeNotFound)
			}
			return err
		}
		if current.State != "needs_reconcile" {
			return problem(taskCodeStateConflict)
		}
		target, bound, err := loadGraphRoot(ctx, tx, actor.WorkspaceID, taskID, current.GraphKind)
		if err != nil {
			return err
		}
		if !bound {
			return problem(graphSyncCodeRootNotBound)
		}
		revision := target.GraphRevision + 1
		var external any
		if resolved.externalID != "" {
			external = resolved.externalID
		}
		if resolved.applied {
			if resolved.externalID != "" {
				if current.Operation != "goal.delete" {
					p, err := json.Marshal(map[string]any{"text": resolved.text})
					if err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, `
						INSERT INTO core.graph_sync_nodes(
							workspace_id,task_id,graph_kind,provider,external_node_id,external_kind,external_state,
							origin,local_key,payload,provenance,observed_revision)
						VALUES($1,$2,$3,$4,$5,'goal','open','bba',$6,$7,'inference',$8)
						ON CONFLICT (workspace_id,task_id,graph_kind,provider,external_node_id) DO UPDATE
						SET payload=EXCLUDED.payload,local_key=EXCLUDED.local_key,
						    observed_revision=EXCLUDED.observed_revision,updated_at=clock_timestamp()`,
						actor.WorkspaceID, taskID, current.GraphKind, graphSyncProviderArtex,
						resolved.externalID, resolved.goalKey, p, revision); err != nil {
						return err
					}
				} else {
					if _, err := tx.Exec(ctx, `
						DELETE FROM core.graph_sync_nodes
						WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4 AND external_node_id=$5`,
						actor.WorkspaceID, taskID, current.GraphKind, graphSyncProviderArtex, resolved.externalID); err != nil {
						return err
					}
				}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE core.graph_sync_outbox
				SET state='sent',external_node_id=$4,last_error_code=NULL,last_error=NULL,
				    lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
				WHERE workspace_id=$1 AND task_id=$2 AND id=$3`,
				actor.WorkspaceID, taskID, itemID, external); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE core.task_graph_roots
				SET graph_revision=$5,last_sync_at=clock_timestamp(),last_error_code=NULL,last_error=NULL,last_error_at=NULL,updated_at=clock_timestamp()
				WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
				actor.WorkspaceID, taskID, current.GraphKind, graphSyncProviderArtex, revision); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `
				UPDATE core.graph_sync_outbox
				SET state='canceled',last_error_code=NULL,last_error=$4,
				    lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
				WHERE workspace_id=$1 AND task_id=$2 AND id=$3`,
				actor.WorkspaceID, taskID, itemID, "reconciled as not applied; verified by goal readback"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE core.task_graph_roots
				SET last_error_code=NULL,last_error=NULL,last_error_at=NULL,updated_at=clock_timestamp()
				WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4`,
				actor.WorkspaceID, taskID, current.GraphKind, graphSyncProviderArtex); err != nil {
				return err
			}
		}
		out, err = scanOutboxItem(tx.QueryRow(ctx, `
			SELECT workspace_id,task_id,id,graph_kind,operation,idempotency_key,payload,payload_digest,scope,sequence,
			       state,attempts,max_attempts,external_node_id,last_error_code,last_error,next_attempt_at,created_at,updated_at
			FROM core.graph_sync_outbox
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3`,
			actor.WorkspaceID, taskID, itemID))
		if err != nil {
			return err
		}
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, itemID, out)
	})
	if err != nil {
		return GraphSyncOutboxItem{}, err
	}
	return out, nil
}

func (s *Store) reconcileReplay(ctx context.Context, actor Actor, path, key, bodyDigest, taskID, itemID string, out *GraphSyncOutboxItem) (bool, error) {
	done := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := taskEnsurePrincipal(ctx, tx, actor); err != nil {
			return err
		}
		rec, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key)
		if err != nil || !found {
			return err
		}
		if rec.digest != bodyDigest {
			return problem(taskCodeIdempotency)
		}
		var stored GraphSyncOutboxItem
		if err := json.Unmarshal(rec.response, &stored); err != nil {
			return problem(taskCodeStoreUnavailable)
		}
		if stored.TaskID != taskID || stored.ID != itemID {
			return problem(taskCodeIdempotency)
		}
		*out = stored
		done = true
		return nil
	})
	return done, err
}

// readAmbiguousItem authorizes and loads one needs_reconcile item, its root and
// the external goal id its local_key maps to (zero when no create succeeded).
func (s *Store) readAmbiguousItem(ctx context.Context, actor Actor, taskID, itemID string) (GraphSyncOutboxItem, graphRootRow, int64, error) {
	var item GraphSyncOutboxItem
	var root graphRootRow
	var mappedID int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.control"); err != nil {
			return err
		}
		item, err = scanOutboxItem(tx.QueryRow(ctx, `
			SELECT workspace_id,task_id,id,graph_kind,operation,idempotency_key,payload,payload_digest,scope,sequence,
			       state,attempts,max_attempts,external_node_id,last_error_code,last_error,next_attempt_at,created_at,updated_at
			FROM core.graph_sync_outbox
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3`,
			actor.WorkspaceID, taskID, itemID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return problem(taskCodeNotFound)
			}
			return err
		}
		if item.State != "needs_reconcile" {
			return problem(taskCodeStateConflict)
		}
		var bound bool
		root, bound, err = loadGraphRoot(ctx, tx, actor.WorkspaceID, taskID, item.GraphKind)
		if err != nil {
			return err
		}
		if !bound {
			return problem(graphSyncCodeRootNotBound)
		}
		goalKey, _ := graphOutboxGoal(item)
		if goalKey == "" {
			return nil
		}
		var external string
		err = tx.QueryRow(ctx, `
			SELECT external_node_id FROM core.graph_sync_nodes
			WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4 AND local_key=$5`,
			actor.WorkspaceID, taskID, graphSyncKindGoal, graphSyncProviderArtex, goalKey).Scan(&external)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var id int64
		if _, scanErr := fmt.Sscan(external, &id); scanErr == nil && id > 0 {
			mappedID = id
		}
		return nil
	})
	return item, root, mappedID, err
}

type graphReadbackOutcome struct {
	applied    bool
	externalID string
	goalKey    string
	text       string
}

func graphOutboxGoal(item GraphSyncOutboxItem) (goalKey, text string) {
	var payload struct {
		GoalKey string `json:"goal_key"`
		Text    string `json:"text"`
	}
	_ = json.Unmarshal(item.Payload, &payload)
	return payload.GoalKey, payload.Text
}

// readbackGoalOutcome verifies the operator's claim against the goal list ARTEX
// publishes now. Nothing here mutates the external graph.
func readbackGoalOutcome(item GraphSyncOutboxItem, goalKey, text string, mappedID int64, request GraphSyncReconcileRequest, goals []artex.Goal) (graphReadbackOutcome, error) {
	out := graphReadbackOutcome{goalKey: goalKey, text: text}
	externalID := strings.TrimSpace(request.ExternalNodeID)
	switch item.Operation {
	case "goal.create":
		if request.Outcome == "applied" {
			if externalID == "" {
				return out, problem(taskCodeValidation)
			}
			for _, goal := range goals {
				if fmt.Sprint(goal.ID) == externalID && goal.Text == text {
					out.applied, out.externalID = true, externalID
					return out, nil
				}
			}
			return out, problem(taskCodeStateConflict)
		}
		for _, goal := range goals {
			if externalID != "" && fmt.Sprint(goal.ID) == externalID {
				return out, problem(taskCodeStateConflict)
			}
			if externalID == "" && goal.Text == text {
				return out, problem(taskCodeStateConflict)
			}
		}
		return out, nil
	case "goal.update":
		if mappedID <= 0 {
			return out, problem(graphSyncCodeGoalUnmapped)
		}
		for _, goal := range goals {
			if goal.ID != mappedID {
				continue
			}
			if request.Outcome == "applied" {
				if goal.Text != text {
					return out, problem(taskCodeStateConflict)
				}
				out.applied, out.externalID = true, fmt.Sprint(mappedID)
				return out, nil
			}
			if goal.Text == text {
				return out, problem(taskCodeStateConflict)
			}
			out.externalID = fmt.Sprint(mappedID)
			return out, nil
		}
		return out, problem(taskCodeStateConflict)
	case "goal.delete":
		if mappedID <= 0 {
			return out, problem(graphSyncCodeGoalUnmapped)
		}
		found := false
		for _, goal := range goals {
			if goal.ID == mappedID {
				found = true
			}
		}
		if request.Outcome == "applied" {
			if found {
				return out, problem(taskCodeStateConflict)
			}
			out.applied, out.externalID = true, fmt.Sprint(mappedID)
			return out, nil
		}
		if !found {
			return out, problem(taskCodeStateConflict)
		}
		out.externalID = fmt.Sprint(mappedID)
		return out, nil
	default:
		return out, problem(taskCodeStateConflict)
	}
}

func scanOutboxItem(row pgx.Row) (GraphSyncOutboxItem, error) {
	var item GraphSyncOutboxItem
	err := row.Scan(&item.WorkspaceID, &item.TaskID, &item.ID, &item.GraphKind, &item.Operation,
		&item.IdempotencyKey, &item.Payload, &item.PayloadDigest, &item.Scope, &item.Sequence,
		&item.State, &item.Attempts, &item.MaxAttempts, &item.ExternalNodeID, &item.LastErrorCode,
		&item.LastError, &item.NextAttemptAt, &item.CreatedAt, &item.UpdatedAt)
	return item, err
}
