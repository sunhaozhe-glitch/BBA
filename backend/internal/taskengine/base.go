package taskengine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oapi-codegen/nullable"
)

type Error struct{ Code string }

func (e *Error) Error() string  { return e.Code }
func problem(code string) error { return &Error{Code: code} }
func Code(err error) string {
	var p *Error
	if errors.As(err, &p) {
		return p.Code
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "RESOURCE_NOT_FOUND"
	}
	var pge *pgconn.PgError
	if errors.As(err, &pge) {
		switch pge.Code {
		case "23505":
			return "CONSTRAINT_CONFLICT"
		case "23503", "23514", "23502":
			return "STATE_CONSTRAINT"
		case "40001", "40P01":
			return "TRANSACTION_CONFLICT"
		}
	}
	if err != nil {
		return "STATE_STORE_UNAVAILABLE"
	}
	return ""
}

// WorkSlotsDefault is startup configuration, fixed before serving requests.
// 0 preserves BBA's default of 1. It is NOT a live configuration management API.
type Store struct {
	Pool             *pgxpool.Pool
	WorkSlotsDefault int
}

const MaxTaskWorkSlots = 8

func (s *Store) workSlotsDefault() (int, error) {
	n := s.WorkSlotsDefault
	if n == 0 {
		n = 1
	}
	if n < 1 || n > MaxTaskWorkSlots {
		return 0, problem(taskCodeValidation)
	}
	return n, nil
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// Respect explicit pool_max_conns, including 1. Guards do not occupy Pool.
	if !strings.Contains(dsn, "pool_max_conns") {
		config.MaxConns = 24
	}
	if config.MaxConns < 1 {
		return nil, problem(taskCodeValidation)
	}
	config.ConnConfig.RuntimeParams["timezone"] = "UTC"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	config.ConnConfig.RuntimeParams["lock_timeout"] = "5000"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}
func (s *Store) Close() { s.Pool.Close() }
func (s *Store) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
func digest(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var normalized any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err = d.Decode(&normalized); err != nil {
		return "", err
	}
	b, err = json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

type Actor struct {
	WorkspaceID, PrincipalID, RuntimeID, SenderID string
	AccountIDs                                    []string
	Permissions                                   map[string]bool
}

func (a Actor) require(permission string) error {
	if a.WorkspaceID == "" || a.PrincipalID == "" {
		return problem("UNAUTHENTICATED")
	}
	if !a.Permissions[permission] {
		return problem("ACTION_FORBIDDEN")
	}
	return nil
}
func contains(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
func authorizeTask(ctx context.Context, tx pgx.Tx, actor Actor, task TaskRow, permission string) error {
	if err := actor.require(permission); err != nil {
		return err
	}
	if actor.WorkspaceID != task.WorkspaceID || (actor.PrincipalID != task.PrincipalID && !actor.Permissions["task.manage_all"]) {
		return problem("RESOURCE_NOT_FOUND")
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.principal WHERE workspace_id=$1 AND id=$2)`, actor.WorkspaceID, actor.PrincipalID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return problem("RESOURCE_NOT_FOUND")
	}
	if actor.RuntimeID != "" {
		rows, err := tx.Query(ctx, `SELECT a.id,a.runtime_id FROM core.task_account ta JOIN core.account a ON (a.workspace_id,a.id)=(ta.workspace_id,ta.account_id) WHERE ta.workspace_id=$1 AND ta.task_id=$2`, task.WorkspaceID, task.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, runtime string
			if err := rows.Scan(&id, &runtime); err != nil {
				return err
			}
			if runtime != actor.RuntimeID || !contains(actor.AccountIDs, id) {
				return problem("RESOURCE_NOT_FOUND")
			}
		}
		return rows.Err()
	}
	return nil
}

type RuntimeActor struct {
	WorkspaceID, RuntimeID string
	OwnerEpoch             int64
	AccountIDs             []string
}

func authorizeRuntime(ctx context.Context, tx pgx.Tx, actor RuntimeActor, accountID string) error {
	if actor.WorkspaceID == "" || actor.RuntimeID == "" || actor.OwnerEpoch <= 0 || !contains(actor.AccountIDs, accountID) {
		return problem("RESOURCE_NOT_FOUND")
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.account a JOIN core.runtime_credentials c ON (c.workspace_id,c.account_id,c.runtime_id)=(a.workspace_id,a.id,a.runtime_id) WHERE a.workspace_id=$1 AND a.id=$2 AND a.runtime_id=$3 AND a.owner_epoch=$4 AND c.owner_epoch=$4 AND c.active)`, actor.WorkspaceID, accountID, actor.RuntimeID, actor.OwnerEpoch).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return problem("RESOURCE_NOT_FOUND")
	}
	return nil
}

type TaskRow struct {
	WorkspaceID, ID, PrincipalID, State, Mode, Title                               string
	Version, ScopeRevision, GoalCount, MaxAttempts, MaxTotalAttempts, ImportOffset int
	BaselineComplete                                                               bool
	Deadline, CreatedAt                                                            time.Time
	ApprovalID                                                                     *string
	ImportDone                                                                     bool
	OrchestrationMode                                                              string
	AuthorizationRevision                                                          int
	RequestPayload                                                                 []byte
	Type                                                                           string
}

// taskTypeFromPayload derives the persisted task type from the canonical
// request_payload jsonb, the storage that has existed since schema 0001 and is
// therefore readable at every supported schema level (no new column, old rows
// keep group.acquire semantics exactly). Every creation request carries the
// type member; the default only guards corrupt or pre-contract data.
func taskTypeFromPayload(raw []byte) string {
	var payload struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Type == "" {
		return "group.acquire"
	}
	return payload.Type
}

const taskColumns = `workspace_id,id,principal_id,state,mode,title,version,scope_revision,goal_count,max_attempts,max_total_attempts,import_offset,baseline_complete,deadline_at,approval_id,import_done,created_at,orchestration_mode,authorization_revision,request_payload`

func scanTask(row pgx.Row) (TaskRow, error) {
	var t TaskRow
	err := row.Scan(&t.WorkspaceID, &t.ID, &t.PrincipalID, &t.State, &t.Mode, &t.Title, &t.Version, &t.ScopeRevision, &t.GoalCount, &t.MaxAttempts, &t.MaxTotalAttempts, &t.ImportOffset, &t.BaselineComplete, &t.Deadline, &t.ApprovalID, &t.ImportDone, &t.CreatedAt, &t.OrchestrationMode, &t.AuthorizationRevision, &t.RequestPayload)
	t.Type = taskTypeFromPayload(t.RequestPayload)
	return t, err
}
func lockTask(ctx context.Context, tx pgx.Tx, workspace, taskID string) (TaskRow, error) {
	t, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM core.task WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return t, problem("RESOURCE_NOT_FOUND")
	}
	return t, err
}
func appendEvent(ctx context.Context, tx pgx.Tx, workspace, taskID, kind, actor string, detail any) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var seq int64
	if err = tx.QueryRow(ctx, `UPDATE core.task SET event_seq=event_seq+1 WHERE workspace_id=$1 AND id=$2 RETURNING event_seq`, workspace, taskID).Scan(&seq); err != nil {
		return err
	}
	id := newID("evt")
	if _, err = tx.Exec(ctx, `INSERT INTO core.task_event(workspace_id,task_id,seq,id,kind,actor,detail) VALUES($1,$2,$3,$4,$5,$6,$7)`, workspace, taskID, seq, id, kind, actor, payload); err != nil {
		return err
	}
	eventPayload, err := json.Marshal(map[string]any{"event_id": id, "task_id": taskID, "seq": fmt.Sprint(seq), "type": kind, "detail": json.RawMessage(payload)})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO core.outbox(workspace_id,id,task_id,event_type,payload) VALUES($1,$2,$3,$4,$5)`, workspace, id, taskID, kind, eventPayload)
	return err
}
func (s *Store) GetTask(ctx context.Context, actor Actor, taskID string) (contract.Task, error) {
	if err := actor.require("task.read"); err != nil {
		return contract.Task{}, err
	}
	var result contract.Task
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	t, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return result, problem("RESOURCE_NOT_FOUND")
	}
	if err != nil {
		return result, err
	}
	if err = authorizeTask(ctx, tx, actor, t, "task.read"); err != nil {
		return result, err
	}
	result = contract.Task{Id: t.ID, WorkspaceId: t.WorkspaceID, Title: t.Title, Type: contract.TaskType(t.Type), State: contract.TaskState(t.State), Version: t.Version, ScopeRevision: t.ScopeRevision, ExecutionMode: contract.ExecutionMode(t.Mode), GoalCount: t.GoalCount, DeadlineAt: t.Deadline, BlockedReason: nullable.NewNullNullable[string](), OrchestrationMode: contract.OrchestrationMode(t.OrchestrationMode), AuthorizationRevision: t.AuthorizationRevision}
	err = tx.QueryRow(ctx, `SELECT created_at,(SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid),(SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='reserved'),(SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2 AND disposition IN ('pending','submitted')),(SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2 AND state='UNKNOWN') FROM core.task WHERE workspace_id=$1 AND id=$2`, t.WorkspaceID, t.ID).Scan(&result.CreatedAt, &result.ConfirmedUniqueCount, &result.ReservedTargetCount, &result.PendingExternalCount, &result.UnknownExecutionCount)
	if err != nil {
		return result, err
	}
	result.CreatedAt = result.CreatedAt.UTC()
	result.DeadlineAt = result.DeadlineAt.UTC()
	if t.State == "BLOCKED" {
		result.BlockedReason.Set("manual_reconciliation_required")
	}
	return result, tx.Commit(ctx)
}

type Reservation struct {
	WorkspaceID, TaskID, SubtaskID, ExecutionID, OperationID, AccountID, RuntimeID, TargetID, GroupID, Digest string
	JoinReason, VerificationMessage                                                                           string
	OwnerEpoch, Fence                                                                                         int64
	ScopeRevision, Attempt                                                                                    int
	Deadline                                                                                                  time.Time
}
type Permit struct {
	ID, OperationID, ExecutionID, WorkspaceID, AccountID, TargetID, Digest, State string
	ScopeRevision                                                                 int
	OwnerEpoch, Fence                                                             int64
	IssuedAt, ExpiresAt                                                           time.Time
}
type SendOutcome struct{ Disposition, EffectCertainty, EvidenceRef, RequestRef string }
type Adapter interface {
	RequestJoin(context.Context, Reservation) (SendOutcome, error)
}
type IdentityCheck func(context.Context, Reservation) error
type Observation struct {
	EventID, OperationID, AccountID, TargetID, Kind, State, EvidenceRef string
	SourceEpoch, SourceSequence                                         int64
	ObservedAt                                                          time.Time
	SnapshotComplete                                                    bool
}
type BaselineSnapshot struct {
	AccountID, EvidenceRef string
	ObservedAt             time.Time
	Complete               bool
	GroupIDs               []string
}
