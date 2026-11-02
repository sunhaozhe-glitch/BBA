package taskengine

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/jackc/pgx/v5"
)

// Bridge P08/P09. The bridge owns bridge.gateway_operation and bridge.outbox;
// the core tables are touched only through explicit transactional transitions.
// A permit authorizes at most one adapter call. No method retries a send.

const (
	actionFamilyJoin = "group.join"
	bridgeEventKind  = "execution.observed"
)

// localDispatchRegistry tracks adapter calls that this process is executing
// right now. Recovery may rewrite the durable execution to UNKNOWN while a
// managed worker is still inside RequestJoin, so the durable row alone cannot
// prove that local sending has stopped. This registry is intentionally
// process-local: it is not a cross-process or global isolation claim, and a
// worker that has not entered the adapter is stopped by revalidateBeforeSend
// instead.
type localDispatchKey struct{ workspaceID, taskID string }

type localDispatchRegistry struct {
	mu     sync.Mutex
	byTask map[localDispatchKey]int
}

var activeLocalDispatches = &localDispatchRegistry{byTask: make(map[localDispatchKey]int)}

func (r *localDispatchRegistry) begin(workspaceID, taskID string) {
	key := localDispatchKey{workspaceID: workspaceID, taskID: taskID}
	r.mu.Lock()
	r.byTask[key]++
	r.mu.Unlock()
}

func (r *localDispatchRegistry) end(workspaceID, taskID string) {
	key := localDispatchKey{workspaceID: workspaceID, taskID: taskID}
	r.mu.Lock()
	if r.byTask[key] <= 1 {
		delete(r.byTask, key)
	} else {
		r.byTask[key]--
	}
	r.mu.Unlock()
}

func (r *localDispatchRegistry) count(workspaceID, taskID string) int {
	key := localDispatchKey{workspaceID: workspaceID, taskID: taskID}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byTask[key]
}

type gatewayRow struct {
	WorkspaceID, OperationID, ExecutionID, TaskID, SubtaskID, AccountID, TargetID, RuntimeID string
	OwnerEpoch, Fence                                                                        int64
	ScopeRevision                                                                            int
	RequestDigest                                                                            string
	Payload                                                                                  []byte
	State                                                                                    string
	Disposition                                                                              *string
	EffectCertainty                                                                          string
	PermitID                                                                                 *string
	Response                                                                                 []byte
	EvidenceRef                                                                              *string
	CreatedAt, UpdatedAt                                                                     time.Time
}

type executionRow struct {
	WorkspaceID, ID, TaskID, SubtaskID, OperationID, AccountID, TargetID, RuntimeID string
	OwnerEpoch, Fence                                                               int64
	ScopeRevision, Attempt                                                          int
	RequestDigest                                                                   string
	Payload                                                                         []byte
	State                                                                           string
}

type permitRow struct {
	ID, WorkspaceID, ExecutionID, OperationID, AccountID, TargetID, Digest, State string
	ScopeRevision                                                                 int
	OwnerEpoch, Fence                                                             int64
	IssuedAt, ExpiresAt                                                           time.Time
}

const gatewayColumns = `workspace_id,operation_id,execution_id,task_id,subtask_id,account_id,target_id,runtime_id,owner_epoch,fence_token,scope_revision,request_digest,payload,state,disposition,effect_certainty,permit_id,response,evidence_ref,created_at,updated_at`

const executionBridgeColumns = `workspace_id,id,task_id,subtask_id,operation_id,account_id,target_id,runtime_id,owner_epoch,fence_token,scope_revision,attempt_no,request_digest,request_payload,state`

const permitColumns = `id,workspace_id,execution_id,operation_id,account_id,target_id,request_digest,scope_revision,owner_epoch,fence_token,issued_at,expires_at,state`

func scanGateway(row pgx.Row) (gatewayRow, error) {
	var g gatewayRow
	err := row.Scan(&g.WorkspaceID, &g.OperationID, &g.ExecutionID, &g.TaskID, &g.SubtaskID, &g.AccountID, &g.TargetID, &g.RuntimeID,
		&g.OwnerEpoch, &g.Fence, &g.ScopeRevision, &g.RequestDigest, &g.Payload, &g.State, &g.Disposition, &g.EffectCertainty,
		&g.PermitID, &g.Response, &g.EvidenceRef, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

func (s *Store) loadGateway(ctx context.Context, tx pgx.Tx, workspace, operationID string, forUpdate bool) (gatewayRow, error) {
	q := `SELECT ` + gatewayColumns + ` FROM bridge.gateway_operation WHERE workspace_id=$1 AND operation_id=$2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	g, err := scanGateway(tx.QueryRow(ctx, q, workspace, operationID))
	if err == pgx.ErrNoRows {
		return g, problem("RESOURCE_NOT_FOUND")
	}
	return g, err
}

func scanExecution(row pgx.Row) (executionRow, error) {
	var e executionRow
	err := row.Scan(&e.WorkspaceID, &e.ID, &e.TaskID, &e.SubtaskID, &e.OperationID, &e.AccountID, &e.TargetID, &e.RuntimeID,
		&e.OwnerEpoch, &e.Fence, &e.ScopeRevision, &e.Attempt, &e.RequestDigest, &e.Payload, &e.State)
	return e, err
}

func (s *Store) loadExecutionByOperation(ctx context.Context, tx pgx.Tx, workspace, operationID string) (executionRow, error) {
	e, err := scanExecution(tx.QueryRow(ctx, `SELECT `+executionBridgeColumns+` FROM core.execution WHERE workspace_id=$1 AND operation_id=$2`, workspace, operationID))
	if err == pgx.ErrNoRows {
		return e, problem("RESOURCE_NOT_FOUND")
	}
	return e, err
}

func (s *Store) loadExecutionByID(ctx context.Context, tx pgx.Tx, workspace, executionID string) (executionRow, error) {
	e, err := scanExecution(tx.QueryRow(ctx, `SELECT `+executionBridgeColumns+` FROM core.execution WHERE workspace_id=$1 AND id=$2`, workspace, executionID))
	if err == pgx.ErrNoRows {
		return e, problem("RESOURCE_NOT_FOUND")
	}
	return e, err
}

func (s *Store) loadPermitByOperation(ctx context.Context, tx pgx.Tx, workspace, operationID string, forUpdate bool) (permitRow, error) {
	q := `SELECT ` + permitColumns + ` FROM core.dispatch_permit WHERE workspace_id=$1 AND operation_id=$2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var p permitRow
	err := tx.QueryRow(ctx, q, workspace, operationID).Scan(&p.ID, &p.WorkspaceID, &p.ExecutionID, &p.OperationID, &p.AccountID, &p.TargetID,
		&p.Digest, &p.ScopeRevision, &p.OwnerEpoch, &p.Fence, &p.IssuedAt, &p.ExpiresAt, &p.State)
	if err == pgx.ErrNoRows {
		return p, problem("RESOURCE_NOT_FOUND")
	}
	return p, err
}

func (s *Store) dbNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}

func decodeExecutionPayload(e executionRow) (ExecutionPayload, error) {
	var p ExecutionPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return p, problem("STATE_CONSTRAINT")
	}
	if err := validateExecutionPayload(e, p); err != nil {
		return p, err
	}
	return p, nil
}

func validateExecutionPayload(e executionRow, p ExecutionPayload) error {
	recomputed, err := digest(p)
	if err != nil || recomputed != e.RequestDigest {
		return problem("STATE_CONSTRAINT")
	}
	if p.WorkspaceID != e.WorkspaceID || p.TaskID != e.TaskID || p.SubtaskID != e.SubtaskID || p.ExecutionID != e.ID ||
		p.OperationID != e.OperationID || p.AccountID != e.AccountID || p.TargetID != e.TargetID || p.RuntimeID != e.RuntimeID ||
		p.OwnerEpoch != e.OwnerEpoch || p.Fence != e.Fence || p.ScopeRevision != e.ScopeRevision || p.Attempt != e.Attempt {
		return problem("STATE_CONSTRAINT")
	}
	if p.Action != string(contract.ActionRequestJoin) {
		return problem("OPERATION_PAYLOAD_CONFLICT")
	}
	if len([]rune(p.JoinReason)) > 500 || len([]rune(p.VerificationMessage)) > 500 {
		return problem("OPERATION_PAYLOAD_CONFLICT")
	}
	if _, err := time.Parse(time.RFC3339Nano, p.DeadlineAt); err != nil {
		return problem("STATE_CONSTRAINT")
	}
	return nil
}

func reservationFromPayload(p ExecutionPayload) (Reservation, error) {
	deadline, err := time.Parse(time.RFC3339Nano, p.DeadlineAt)
	if err != nil {
		return Reservation{}, problem("STATE_CONSTRAINT")
	}
	return Reservation{
		WorkspaceID: p.WorkspaceID, TaskID: p.TaskID, SubtaskID: p.SubtaskID, ExecutionID: p.ExecutionID, OperationID: p.OperationID,
		AccountID: p.AccountID, RuntimeID: p.RuntimeID, TargetID: p.TargetID, GroupID: p.GroupID,
		JoinReason: p.JoinReason, VerificationMessage: p.VerificationMessage,
		OwnerEpoch: p.OwnerEpoch, Fence: p.Fence, ScopeRevision: p.ScopeRevision, Attempt: p.Attempt, Deadline: deadline,
	}, nil
}

func matchReservation(res Reservation, e executionRow, p ExecutionPayload) error {
	if res.WorkspaceID != p.WorkspaceID || res.TaskID != p.TaskID || res.SubtaskID != p.SubtaskID ||
		res.ExecutionID != p.ExecutionID || res.OperationID != p.OperationID || res.AccountID != p.AccountID ||
		res.RuntimeID != p.RuntimeID || res.TargetID != p.TargetID || res.GroupID != p.GroupID ||
		res.JoinReason != p.JoinReason || res.VerificationMessage != p.VerificationMessage ||
		res.OwnerEpoch != p.OwnerEpoch || res.Fence != p.Fence || res.ScopeRevision != p.ScopeRevision || res.Attempt != p.Attempt {
		return problem("OPERATION_PAYLOAD_CONFLICT")
	}
	deadline, err := time.Parse(time.RFC3339Nano, p.DeadlineAt)
	if err != nil || !res.Deadline.Equal(deadline) || res.Digest != e.RequestDigest {
		return problem("OPERATION_PAYLOAD_CONFLICT")
	}
	return nil
}

func verifyGatewayEnvelope(g gatewayRow, e executionRow, p ExecutionPayload) error {
	if g.ExecutionID != e.ID || g.TaskID != e.TaskID || g.SubtaskID != e.SubtaskID || g.AccountID != e.AccountID ||
		g.TargetID != e.TargetID || g.RuntimeID != e.RuntimeID || g.OwnerEpoch != e.OwnerEpoch || g.Fence != e.Fence ||
		g.ScopeRevision != e.ScopeRevision || g.RequestDigest != e.RequestDigest ||
		p.ExecutionID != g.ExecutionID || p.AccountID != g.AccountID || p.TargetID != g.TargetID {
		return problem("STATE_CONSTRAINT")
	}
	if g.OperationID != e.OperationID {
		return problem("STATE_CONSTRAINT")
	}
	return nil
}

func validRuntimeOwner(actor RuntimeActor, g gatewayRow) error {
	if actor.WorkspaceID != g.WorkspaceID || actor.RuntimeID != g.RuntimeID || actor.OwnerEpoch != g.OwnerEpoch {
		return problem("RESOURCE_NOT_FOUND")
	}
	return nil
}

func targetFromPayload(p ExecutionPayload) contract.Target {
	return contract.Target{Platform: contract.TargetPlatformQq, Kind: contract.TargetKindGroup, ExternalId: contract.QQId(p.GroupID)}
}

// AcceptOperation persists the canonical operation envelope. The caller
// reservation is only compared against the immutable core.execution row and
// its digest-verified payload; it can never widen or alter the operation.
func (s *Store) AcceptOperation(ctx context.Context, actor RuntimeActor, res Reservation) (Reservation, error) {
	var out Reservation
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if actor.WorkspaceID == "" || actor.RuntimeID == "" || actor.OwnerEpoch <= 0 || res.OperationID == "" || res.ExecutionID == "" {
			return problem("VALIDATION_FAILED")
		}
		if res.WorkspaceID != actor.WorkspaceID || res.RuntimeID != actor.RuntimeID || res.OwnerEpoch != actor.OwnerEpoch {
			return problem("RESOURCE_NOT_FOUND")
		}
		if err := authorizeRuntime(ctx, tx, actor, res.AccountID); err != nil {
			return err
		}
		e, err := s.loadExecutionByOperation(ctx, tx, res.WorkspaceID, res.OperationID)
		if err != nil {
			return err
		}
		p, err := decodeExecutionPayload(e)
		if err != nil {
			return err
		}
		if err := matchReservation(res, e, p); err != nil {
			return err
		}
		// Task lock first: the deferred execution closure trigger also locks the
		// task at commit, so every bridge tx follows task -> execution -> bridge.
		lockedTask, err := lockTask(ctx, tx, e.WorkspaceID, e.TaskID)
		if err != nil {
			return err
		}
		if (lockedTask.ApprovalID == nil && p.ApprovalID != "") ||
			(lockedTask.ApprovalID != nil && p.ApprovalID != *lockedTask.ApprovalID) {
			return problem("OPERATION_PAYLOAD_CONFLICT")
		}
		// No execution row lock is held here so a concurrent AcceptOperation or
		// Dispatch (gateway lock) cannot form a lock cycle with the scheduler.
		if _, err := tx.Exec(ctx, `INSERT INTO bridge.gateway_operation(workspace_id,operation_id,execution_id,task_id,subtask_id,account_id,target_id,runtime_id,owner_epoch,fence_token,scope_revision,request_digest,payload,state,effect_certainty)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'ACCEPTED','no_effect')
			ON CONFLICT (workspace_id,operation_id) DO NOTHING`,
			e.WorkspaceID, e.OperationID, e.ID, e.TaskID, e.SubtaskID, e.AccountID, e.TargetID, e.RuntimeID,
			e.OwnerEpoch, e.Fence, e.ScopeRevision, e.RequestDigest, string(e.Payload)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE core.execution SET state='ACCEPTED' WHERE workspace_id=$1 AND id=$2 AND state='CREATED'`, e.WorkspaceID, e.ID); err != nil {
			return err
		}
		g, err := s.loadGateway(ctx, tx, res.WorkspaceID, res.OperationID, false)
		if err != nil {
			return err
		}
		if err := validRuntimeOwner(actor, g); err != nil {
			return err
		}
		if err := verifyGatewayEnvelope(g, e, p); err != nil {
			return err
		}
		canonical, err := reservationFromPayload(p)
		if err != nil {
			return err
		}
		canonical.Digest = e.RequestDigest
		out = canonical
		return nil
	})
	if err != nil {
		return Reservation{}, err
	}
	return out, nil
}

func (s *Store) checkTaskDispatchable(ctx context.Context, tx pgx.Tx, task TaskRow, e executionRow, p ExecutionPayload, now time.Time) (TaskRow, error) {
	if task.WorkspaceID != e.WorkspaceID || task.ID != e.TaskID {
		return task, problem("STATE_CONSTRAINT")
	}
	var err error
	if task.State != "RUNNING" || task.Mode != "live" || !task.BaselineComplete {
		return task, problem("OPERATION_NOT_DISPATCHABLE")
	}
	if !task.Deadline.After(now) || task.ApprovalID == nil {
		return task, problem("OPERATION_NOT_DISPATCHABLE")
	}
	if task.ScopeRevision != e.ScopeRevision {
		return task, problem("OPERATION_NOT_DISPATCHABLE")
	}
	var principal, approvalReason, approvalMessage string
	var approvalScope int
	var action string
	var accounts, targets []string
	var maxAttempts int
	var expiresAt time.Time
	var revoked bool
	err = tx.QueryRow(ctx, `SELECT principal_id,scope_revision,action,account_ids,target_ids,max_attempts,expires_at,revoked,
		join_reason,verification_message
		FROM core.approval WHERE workspace_id=$1 AND id=$2 FOR SHARE`, e.WorkspaceID, *task.ApprovalID).
		Scan(&principal, &approvalScope, &action, &accounts, &targets, &maxAttempts, &expiresAt, &revoked,
			&approvalReason, &approvalMessage)
	if err != nil {
		if err == pgx.ErrNoRows {
			return task, problem("OPERATION_NOT_DISPATCHABLE")
		}
		return task, err
	}
	if principal != task.PrincipalID || revoked || action != string(contract.ActionRequestJoin) || approvalScope != e.ScopeRevision ||
		p.ApprovalID == "" || p.ApprovalID != *task.ApprovalID ||
		maxAttempts < e.Attempt || !expiresAt.After(now) || !contains(accounts, e.AccountID) || !contains(targets, p.GroupID) ||
		approvalReason != p.JoinReason || approvalMessage != p.VerificationMessage {
		return task, problem("OPERATION_NOT_DISPATCHABLE")
	}
	return task, nil
}

func (s *Store) checkClaimSlotAccount(ctx context.Context, tx pgx.Tx, e executionRow, now time.Time, ttl time.Duration) (int64, error) {
	var claimTask, claimSubtask, claimState string
	err := tx.QueryRow(ctx, `SELECT task_id,subtask_id,state FROM core.target_claim
		WHERE workspace_id=$1 AND target_id=$2 AND action_family=$3 FOR UPDATE`, e.WorkspaceID, e.TargetID, actionFamilyJoin).
		Scan(&claimTask, &claimSubtask, &claimState)
	if err != nil {
		return 0, problem("OPERATION_NOT_DISPATCHABLE")
	}
	if claimState != "active" || claimTask != e.TaskID || claimSubtask != e.SubtaskID {
		return 0, problem("OPERATION_NOT_DISPATCHABLE")
	}

	var accountRuntime, identity, connection, login, control, risk string
	var accountOwner, identityGeneration int64
	var selfID *string
	var requestJoinVerified bool
	err = tx.QueryRow(ctx, `SELECT runtime_id,identity_status,connection_state,login_state,control_state,risk_state,owner_epoch,identity_generation,self_id,request_join_verified
		FROM core.account WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, e.WorkspaceID, e.AccountID).
		Scan(&accountRuntime, &identity, &connection, &login, &control, &risk, &accountOwner, &identityGeneration, &selfID, &requestJoinVerified)
	if err != nil {
		return 0, problem("OPERATION_NOT_DISPATCHABLE")
	}
	if accountRuntime != e.RuntimeID || accountOwner != e.OwnerEpoch || selfID == nil ||
		identity != "verified" || connection != "online" || login != "logged_in" || control != "active" ||
		risk != "clear" || !requestJoinVerified {
		return 0, problem("ACCOUNT_RESTRICTED")
	}
	// The short send lock is taken here, after the account row and before the
	// usage row, matching the scheduler's account -> usage lock order.
	if err := s.holdAccountLock(ctx, tx, e, now, ttl); err != nil {
		return 0, err
	}

	var reserved int
	err = tx.QueryRow(ctx, `SELECT reserved FROM core.account_usage
		WHERE workspace_id=$1 AND account_id=$2 AND action_family=$3 AND window_start=current_date FOR UPDATE`,
		e.WorkspaceID, e.AccountID, actionFamilyJoin).Scan(&reserved)
	if err != nil || reserved <= 0 {
		return 0, problem("NO_CAPACITY")
	}

	var subtaskState, assigned string
	var currentExecution *string
	var fence int64
	err = tx.QueryRow(ctx, `SELECT state,assigned_account_id,current_execution_id,fence_token FROM core.subtask
		WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, e.WorkspaceID, e.SubtaskID).
		Scan(&subtaskState, &assigned, &currentExecution, &fence)
	if err != nil {
		return 0, problem("OPERATION_NOT_DISPATCHABLE")
	}
	if assigned != e.AccountID || currentExecution == nil || *currentExecution != e.ID || fence != e.Fence ||
		(subtaskState != "CLAIMED" && subtaskState != "RUNNING") {
		return 0, problem("OPERATION_NOT_DISPATCHABLE")
	}

	var slotState, slotTarget string
	err = tx.QueryRow(ctx, `SELECT state,target_id FROM core.goal_slot
		WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='reserved' FOR UPDATE`,
		e.WorkspaceID, e.TaskID, e.TargetID).Scan(&slotState, &slotTarget)
	if err != nil || slotState != "reserved" || slotTarget != e.TargetID {
		return 0, problem("NO_CAPACITY")
	}
	return identityGeneration, nil
}

func (s *Store) holdAccountLock(ctx context.Context, tx pgx.Tx, e executionRow, now time.Time, ttl time.Duration) error {
	if _, err := tx.Exec(ctx, `INSERT INTO core.account_action_lock(workspace_id,account_id,action_family,execution_id,permit_id,held,expires_at)
		VALUES($1,$2,$3,$4,NULL,true,$5) ON CONFLICT (workspace_id,account_id,action_family) DO NOTHING`,
		e.WorkspaceID, e.AccountID, actionFamilyJoin, e.ID, now.Add(ttl)); err != nil {
		return err
	}
	var holder string
	var held bool
	var expiresAt time.Time
	err := tx.QueryRow(ctx, `SELECT execution_id,held,expires_at FROM core.account_action_lock
		WHERE workspace_id=$1 AND account_id=$2 AND action_family=$3 FOR UPDATE`,
		e.WorkspaceID, e.AccountID, actionFamilyJoin).Scan(&holder, &held, &expiresAt)
	if err != nil {
		return err
	}
	if held && holder != e.ID {
		// Never steal an expired or live holder. Explicit reconciliation owns release.
		return problem("ACCOUNT_ACTION_BUSY")
	}
	if _, err := tx.Exec(ctx, `UPDATE core.account_action_lock SET execution_id=$4,permit_id=NULL,held=true,expires_at=$5
		WHERE workspace_id=$1 AND account_id=$2 AND action_family=$3`,
		e.WorkspaceID, e.AccountID, actionFamilyJoin, e.ID, now.Add(ttl)); err != nil {
		return err
	}
	return nil
}

func ttlMicros(ttl time.Duration) int64 {
	us := ttl.Microseconds()
	if us < 1 {
		return 1
	}
	return us
}

// IssuePermit issues exactly one stable, short-lived permit per operation.
// Duplicate calls return the persisted permit without extending expiry, even
// after consumption. The bridge ledger moves to DISPATCHING before the permit
// can be consumed.
func (s *Store) IssuePermit(ctx context.Context, actor RuntimeActor, operationID string, ttl time.Duration) (Permit, error) {
	var out Permit
	if operationID == "" || ttl <= 0 || ttl > 30*time.Second {
		return out, problem("VALIDATION_FAILED")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		pre, err := s.loadGateway(ctx, tx, actor.WorkspaceID, operationID, false)
		if err != nil {
			return err
		}
		if err := authorizeRuntime(ctx, tx, actor, pre.AccountID); err != nil {
			return err
		}
		if err := validRuntimeOwner(actor, pre); err != nil {
			return err
		}
		task, err := lockTask(ctx, tx, pre.WorkspaceID, pre.TaskID)
		if err != nil {
			return err
		}
		g, err := s.loadGateway(ctx, tx, actor.WorkspaceID, operationID, true)
		if err != nil {
			return err
		}
		if g.TaskID != task.ID || g.WorkspaceID != task.WorkspaceID {
			return problem("STATE_CONSTRAINT")
		}
		now, err := s.dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if existing, err := s.loadPermitByOperation(ctx, tx, g.WorkspaceID, operationID, true); err == nil {
			out = buildPermit(existing, operationalTarget(g), now)
			return nil
		} else if Code(err) != "RESOURCE_NOT_FOUND" {
			return err
		}
		if g.State != "ACCEPTED" && g.State != "DISPATCHING" {
			return problem("OPERATION_NOT_DISPATCHABLE")
		}
		e, err := s.loadExecutionByID(ctx, tx, g.WorkspaceID, g.ExecutionID)
		if err != nil {
			return err
		}
		p, err := decodeExecutionPayload(e)
		if err != nil {
			return err
		}
		if err := verifyGatewayEnvelope(g, e, p); err != nil {
			return err
		}
		if e.State != "ACCEPTED" && e.State != "DISPATCHING" {
			return problem("OPERATION_NOT_DISPATCHABLE")
		}
		if _, err := s.checkTaskDispatchable(ctx, tx, task, e, p, now); err != nil {
			return err
		}
		identityGeneration, err := s.checkClaimSlotAccount(ctx, tx, e, now, ttl)
		if err != nil {
			return err
		}
		permitID := newID("permit")
		var issued permitRow
		err = tx.QueryRow(ctx, `INSERT INTO core.dispatch_permit(workspace_id,id,execution_id,operation_id,account_id,target_id,request_digest,scope_revision,owner_epoch,fence_token,issued_at,expires_at,state)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,clock_timestamp(),clock_timestamp()+($11::bigint*interval '1 microsecond'),'issued')
			RETURNING `+permitColumns,
			e.WorkspaceID, permitID, e.ID, e.OperationID, e.AccountID, e.TargetID, e.RequestDigest, e.ScopeRevision,
			e.OwnerEpoch, e.Fence, ttlMicros(ttl)).
			Scan(&issued.ID, &issued.WorkspaceID, &issued.ExecutionID, &issued.OperationID, &issued.AccountID, &issued.TargetID,
				&issued.Digest, &issued.ScopeRevision, &issued.OwnerEpoch, &issued.Fence, &issued.IssuedAt, &issued.ExpiresAt, &issued.State)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE core.account_action_lock SET permit_id=$4 WHERE workspace_id=$1 AND account_id=$2 AND action_family=$3`,
			e.WorkspaceID, e.AccountID, actionFamilyJoin, issued.ID); err != nil {
			return err
		}
		capturedIdentity := string(mustJSON(map[string]any{"identity_generation": identityGeneration}))
		if _, err := tx.Exec(ctx, `UPDATE bridge.gateway_operation SET state='DISPATCHING',disposition=NULL,effect_certainty='unknown',permit_id=$3,response=$4,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND operation_id=$2`, g.WorkspaceID, g.OperationID, issued.ID, capturedIdentity); err != nil {
			return err
		}
		out = buildPermit(issued, targetFromPayload(p), now)
		return nil
	})
	if err != nil {
		return Permit{}, err
	}
	return out, nil
}

func operationalTarget(g gatewayRow) contract.Target {
	var p ExecutionPayload
	if err := json.Unmarshal(g.Payload, &p); err == nil && p.GroupID != "" {
		return targetFromPayload(p)
	}
	return contract.Target{Platform: contract.TargetPlatformQq, Kind: contract.TargetKindGroup, ExternalId: contract.QQId(g.TargetID)}
}

func buildPermit(p permitRow, target contract.Target, now time.Time) Permit {
	state := p.State
	if state == "issued" && !now.Before(p.ExpiresAt) {
		state = "expired"
	}
	return Permit{
		ID: p.ID, OperationID: p.OperationID, ExecutionID: p.ExecutionID, WorkspaceID: p.WorkspaceID, AccountID: p.AccountID,
		TargetID: p.TargetID, Digest: p.Digest, State: state, ScopeRevision: p.ScopeRevision, OwnerEpoch: p.OwnerEpoch,
		Fence: p.Fence, IssuedAt: p.IssuedAt, ExpiresAt: p.ExpiresAt,
	}
}

type normalizedOutcome struct {
	State        string
	Disposition  *string
	Effect       string
	EvidenceRef  string
	RequestRef   string
	Observation  string
	SafeResponse []byte
}

func normalizeOutcome(in SendOutcome, adapterErr error) normalizedOutcome {
	unknown := normalizedOutcome{State: "UNKNOWN", Disposition: new("unknown"), Effect: "unknown", Observation: "unknown"}
	unknown.SafeResponse = []byte(`{"error":"adapter_error"}`)
	if adapterErr != nil {
		return unknown
	}
	evidence := in.EvidenceRef
	switch {
	case in.Disposition == "failed_no_effect" && in.EffectCertainty == "no_effect" && evidence != "":
		return observed(in, "failed_no_effect", "no_effect")
	case in.Disposition == "confirmed" && in.EffectCertainty == "confirmed" && evidence != "":
		return observed(in, "confirmed", "confirmed")
	case in.Disposition == "submitted" && in.EffectCertainty == "confirmed_submitted" && evidence != "":
		return observed(in, "submitted", "confirmed_submitted")
	case in.Disposition == "rejected" && in.EffectCertainty == "confirmed_rejected" && evidence != "":
		return observed(in, "rejected", "confirmed_rejected")
	case in.Disposition == "unknown" && in.EffectCertainty == "unknown":
		return unknown
	}
	return unknown
}

func observed(in SendOutcome, disposition, effect string) normalizedOutcome {
	body, _ := json.Marshal(map[string]any{"disposition": disposition, "effect_certainty": effect, "request_ref": nullableString(in.RequestRef), "evidence_ref": in.EvidenceRef})
	return normalizedOutcome{
		State: "OBSERVED", Disposition: new(disposition), Effect: effect, EvidenceRef: in.EvidenceRef,
		RequestRef: in.RequestRef, Observation: disposition, SafeResponse: body,
	}
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func outcomeFromGateway(g gatewayRow) SendOutcome {
	out := SendOutcome{EffectCertainty: g.EffectCertainty}
	if g.Disposition != nil {
		out.Disposition = *g.Disposition
	}
	if g.EvidenceRef != nil {
		out.EvidenceRef = *g.EvidenceRef
	}
	if len(g.Response) > 0 {
		var response struct {
			RequestRef string `json:"request_ref"`
		}
		if json.Unmarshal(g.Response, &response) == nil {
			out.RequestRef = response.RequestRef
		}
	}
	return out
}

func gatewayErrorCode(g gatewayRow) string {
	if len(g.Response) == 0 {
		return ""
	}
	var response struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(g.Response, &response) == nil {
		return response.Code
	}
	return ""
}

func (s *Store) rejectBeforeSend(ctx context.Context, tx pgx.Tx, g gatewayRow, e executionRow, code string) error {
	// Callers hold the task and gateway locks. Read the locked durable state;
	// the Dispatch caller's execution snapshot may predate permit consumption.
	var executionState string
	if err := tx.QueryRow(ctx, `SELECT state FROM core.execution WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, e.WorkspaceID, e.ID).Scan(&executionState); err != nil {
		return err
	}
	if executionState != "CREATED" && executionState != "ACCEPTED" {
		return nil
	}
	if g.PermitID != nil {
		if _, err := tx.Exec(ctx, `UPDATE core.dispatch_permit SET state='expired' WHERE workspace_id=$1 AND id=$2 AND state='issued'`, g.WorkspaceID, *g.PermitID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE bridge.gateway_operation SET state='REJECTED_BEFORE_SEND',disposition=NULL,effect_certainty='no_effect',response=$3,evidence_ref=NULL,updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND operation_id=$2 AND state IN ('ACCEPTED','DISPATCHING')`,
		g.WorkspaceID, g.OperationID, string(mustJSON(map[string]any{"code": code}))); err != nil {
		return err
	}
	// Account before execution, matching the scheduler/bridge global order.
	if _, err := tx.Exec(ctx, `UPDATE core.account_action_lock SET held=false WHERE workspace_id=$1 AND account_id=$2 AND action_family=$3 AND execution_id=$4 AND permit_id IS NOT DISTINCT FROM $5`,
		e.WorkspaceID, e.AccountID, actionFamilyJoin, e.ID, g.PermitID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE core.execution SET state='REJECTED_BEFORE_SEND',disposition=NULL,effect_certainty='no_effect'
		WHERE workspace_id=$1 AND id=$2 AND state IN ('CREATED','ACCEPTED')`, e.WorkspaceID, e.ID); err != nil {
		return err
	}
	task, err := lockTask(ctx, tx, g.WorkspaceID, g.TaskID)
	if err != nil {
		return err
	}
	if err := s.projectWorkItemExecution(ctx, tx, task, e.ID, "dispatch", ""); err != nil {
		return err
	}
	return settlePausingTask(ctx, tx, g.WorkspaceID, g.TaskID)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

// Dispatch performs the final transactional consume and then exactly one
// adapter call. The identity check runs before consume and is mandatory. Any
// missing, consumed, expired or unbindable permit results in zero adapter calls.
func (s *Store) Dispatch(ctx context.Context, actor RuntimeActor, operationID string, check IdentityCheck, adapter Adapter) (SendOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, contentSendTimeout)
	defer cancel()
	if operationID == "" || check == nil || adapter == nil {
		return SendOutcome{}, problem("VALIDATION_FAILED")
	}
	var g gatewayRow
	var e executionRow
	var p ExecutionPayload
	var res Reservation
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		g, err = s.loadGateway(ctx, tx, actor.WorkspaceID, operationID, false)
		if err != nil {
			return err
		}
		if err := authorizeRuntime(ctx, tx, actor, g.AccountID); err != nil {
			return err
		}
		if err := validRuntimeOwner(actor, g); err != nil {
			return err
		}
		e, err = s.loadExecutionByID(ctx, tx, g.WorkspaceID, g.ExecutionID)
		if err != nil {
			return err
		}
		p, err = decodeExecutionPayload(e)
		if err != nil {
			return err
		}
		if err := verifyGatewayEnvelope(g, e, p); err != nil {
			return err
		}
		res, err = reservationFromPayload(p)
		if err != nil {
			return err
		}
		res.Digest = g.RequestDigest
		return nil
	})
	if err != nil {
		return SendOutcome{}, err
	}
	switch g.State {
	case "OBSERVED":
		return outcomeFromGateway(g), nil
	case "UNKNOWN":
		return outcomeFromGateway(g), nil
	case "REJECTED_BEFORE_SEND":
		if code := gatewayErrorCode(g); code != "" {
			return SendOutcome{}, problem(code)
		}
		return SendOutcome{}, problem("OPERATION_NOT_DISPATCHABLE")
	case "DISPATCHING":
		// continue
	default:
		return SendOutcome{}, problem("OPERATION_NOT_DISPATCHABLE")
	}

	if err := check(ctx, res); err != nil {
		persistCtx := context.WithoutCancel(ctx)
		_ = s.tx(persistCtx, func(tx pgx.Tx) error {
			if _, loadErr := lockTask(persistCtx, tx, g.WorkspaceID, p.TaskID); loadErr != nil {
				return nil
			}
			locked, loadErr := s.loadGateway(persistCtx, tx, g.WorkspaceID, g.OperationID, true)
			if loadErr != nil || locked.State != "DISPATCHING" || locked.TaskID != p.TaskID {
				return nil
			}
			if err := authorizeRuntime(persistCtx, tx, actor, locked.AccountID); err != nil {
				return nil
			}
			row, loadErr := s.loadExecutionByID(persistCtx, tx, locked.WorkspaceID, locked.ExecutionID)
			if loadErr != nil {
				return nil
			}
			if err := s.rejectBeforeSend(persistCtx, tx, locked, row, "IDENTITY_CHECK_FAILED"); err != nil {
				return err
			}
			return nil
		})
		return SendOutcome{}, err
	}

	var earlyOutcome *SendOutcome
	var earlyErr error
	consumeErr := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockTask(ctx, tx, g.WorkspaceID, p.TaskID); err != nil {
			return err
		}
		locked, err := s.loadGateway(ctx, tx, g.WorkspaceID, g.OperationID, true)
		if err != nil {
			return err
		}
		if locked.TaskID != p.TaskID || locked.WorkspaceID != g.WorkspaceID {
			return problem("STATE_CONSTRAINT")
		}
		if err := authorizeRuntime(ctx, tx, actor, locked.AccountID); err != nil {
			return err
		}
		if err := validRuntimeOwner(actor, locked); err != nil {
			return err
		}
		if locked.State == "OBSERVED" || locked.State == "UNKNOWN" || locked.State == "REJECTED_BEFORE_SEND" {
			if locked.State == "REJECTED_BEFORE_SEND" {
				if code := gatewayErrorCode(locked); code != "" {
					earlyErr = problem(code)
				} else {
					earlyErr = problem("OPERATION_NOT_DISPATCHABLE")
				}
				return nil
			}
			out := outcomeFromGateway(locked)
			earlyOutcome = &out
			return nil
		}
		if locked.State != "DISPATCHING" {
			return problem("OPERATION_NOT_DISPATCHABLE")
		}
		row, err := s.loadExecutionByID(ctx, tx, locked.WorkspaceID, locked.ExecutionID)
		if err != nil {
			return err
		}
		payload, err := decodeExecutionPayload(row)
		if err != nil {
			return err
		}
		if err := verifyGatewayEnvelope(locked, row, payload); err != nil {
			return err
		}
		// Final readiness check: a restarted runtime marks persisted identity
		// stale, so the identity generation captured at permit issue must still
		// match the live account row. Pause after issue remains an in-flight
		// permit, but stale/drift identity never sends.
		var identityRuntime, identity, identityControl, identityRisk, identityConnection, identityLogin string
		var identityOwner, liveGeneration int64
		err = tx.QueryRow(ctx, `SELECT runtime_id,identity_status,owner_epoch,identity_generation,control_state,risk_state,connection_state,login_state
			FROM core.account WHERE workspace_id=$1 AND id=$2 FOR SHARE`, row.WorkspaceID, row.AccountID).
			Scan(&identityRuntime, &identity, &identityOwner, &liveGeneration, &identityControl, &identityRisk, &identityConnection, &identityLogin)
		if err != nil || identityRuntime != row.RuntimeID || identityOwner != row.OwnerEpoch || identity != "verified" ||
			identityControl != "active" || identityRisk != "clear" || identityConnection != "online" || identityLogin != "logged_in" {
			earlyErr = problem("ACCOUNT_RESTRICTED")
			return nil
		}
		if captured, ok := gatewayIdentityGeneration(locked); ok && captured != liveGeneration {
			earlyErr = problem("ACCOUNT_RESTRICTED")
			return nil
		}
		permit, err := s.loadPermitByOperation(ctx, tx, locked.WorkspaceID, locked.OperationID, true)
		if err != nil {
			if Code(err) == "RESOURCE_NOT_FOUND" {
				if err := s.rejectBeforeSend(ctx, tx, locked, row, "PERMIT_MISSING"); err != nil {
					return err
				}
				earlyErr = problem("PERMIT_MISSING")
				return nil
			}
			return err
		}
		if permit.ExecutionID != row.ID || permit.AccountID != row.AccountID || permit.TargetID != row.TargetID ||
			permit.Digest != row.RequestDigest || permit.ScopeRevision != row.ScopeRevision || permit.OwnerEpoch != row.OwnerEpoch ||
			permit.Fence != row.Fence || locked.PermitID == nil || *locked.PermitID != permit.ID {
			return problem("STATE_CONSTRAINT")
		}
		if permit.State == "consumed" {
			earlyErr = problem("PERMIT_CONSUMED")
			return nil
		}
		now, err := s.dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if permit.State != "issued" || !now.Before(permit.ExpiresAt) {
			if err := s.rejectBeforeSend(ctx, tx, locked, row, "PERMIT_EXPIRED"); err != nil {
				return err
			}
			earlyErr = problem("PERMIT_EXPIRED")
			return nil
		}
		deadline, _ := time.Parse(time.RFC3339Nano, payload.DeadlineAt)
		if !now.Before(deadline) {
			if err := s.rejectBeforeSend(ctx, tx, locked, row, "DEADLINE_EXPIRED"); err != nil {
				return err
			}
			earlyErr = problem("DEADLINE_EXPIRED")
			return nil
		}
		var holder string
		var held bool
		var lockPermit *string
		err = tx.QueryRow(ctx, `SELECT execution_id,held,permit_id FROM core.account_action_lock
			WHERE workspace_id=$1 AND account_id=$2 AND action_family=$3 FOR UPDATE`,
			row.WorkspaceID, row.AccountID, actionFamilyJoin).Scan(&holder, &held, &lockPermit)
		if err != nil || !held || holder != row.ID || lockPermit == nil || *lockPermit != permit.ID {
			earlyErr = problem("ACCOUNT_ACTION_BUSY")
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE core.dispatch_permit SET state='consumed'
			WHERE workspace_id=$1 AND id=$2 AND state='issued' AND expires_at>clock_timestamp()`, permit.WorkspaceID, permit.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			earlyErr = problem("PERMIT_EXPIRED")
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE bridge.gateway_operation SET state='DISPATCHING',permit_id=$3,updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND operation_id=$2`, locked.WorkspaceID, locked.OperationID, permit.ID); err != nil {
			return err
		}
		// Keep scheduler lock order (usage before execution row) to avoid a
		// lock cycle with a concurrent reservation for the same account.
		tag, err = tx.Exec(ctx, `UPDATE core.account_usage SET reserved=reserved-1,consumed=consumed+1
			WHERE workspace_id=$1 AND account_id=$2 AND action_family=$3 AND window_start=current_date AND reserved>0`,
			row.WorkspaceID, row.AccountID, actionFamilyJoin)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return problem("STATE_CONSTRAINT")
		}
		tag, err = tx.Exec(ctx, `UPDATE core.execution SET state='DISPATCHING',disposition=NULL,effect_certainty='unknown',started_at=COALESCE(started_at,clock_timestamp())
			WHERE workspace_id=$1 AND id=$2 AND state IN ('CREATED','ACCEPTED')`, row.WorkspaceID, row.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return problem("STATE_CONSTRAINT")
		}
		return nil
	})
	if consumeErr != nil {
		return SendOutcome{}, consumeErr
	}
	if earlyOutcome != nil {
		return *earlyOutcome, nil
	}
	if earlyErr != nil {
		return SendOutcome{}, earlyErr
	}

	// The consume commit is the point of no return for the permit, but not yet
	// for the external call. Register the managed local send before the gate:
	// recovery can rewrite the durable row to UNKNOWN while this worker is
	// still between consume and adapter, and the pause must not settle until
	// this worker either refuses or returns. The guarded defer covers error
	// and panic paths; explicit end below runs before any outcome/refusal
	// commit so that its settle sees the durable result, not this call.
	activeLocalDispatches.begin(g.WorkspaceID, p.TaskID)
	localFlight := true
	defer func() {
		if localFlight {
			activeLocalDispatches.end(g.WorkspaceID, p.TaskID)
		}
	}()

	// The adapter call is the only remaining local flight. Recovery may mark
	// the durable row UNKNOWN while this call is still live, so the pause must
	// consult the registry too; release before persisting the outcome so the
	// subsequent settle sees the durable result, not this call.
	contentGuard, err := s.acquireContentSendGuard(ctx, g.WorkspaceID)
	if err != nil {
		return SendOutcome{}, err
	}
	defer contentGuard.close()
	finalDigest, err := digest(res)
	if err != nil {
		return SendOutcome{}, err
	}
	decision, err := s.contentInspectFields(ctx, g.WorkspaceID, res.AccountID, "join", res.TargetID, g.OperationID, p.TaskID, []contentField{{"reason", res.JoinReason}, {"answer", res.VerificationMessage}}, finalDigest, false)
	if err != nil {
		return SendOutcome{}, err
	}
	if decision != "allowed" {
		return SendOutcome{}, problem("CONTENT_BLOCKED")
	}
	// All potentially blocking content acquisition and review persistence must
	// precede authorization. Keep the content guard through the adapter call:
	// rule updates serialize on it, but the authorization commit and external
	// effect cannot be atomic. Do not claim this closes that final window.
	// A worker that was stalled between consume and send must re-prove that
	// its permit, runtime generation and task lifecycle are still current,
	// otherwise a long-paused process could send stale work.
	gate, err := s.revalidateBeforeSend(ctx, actor, g, p)
	if err != nil {
		return SendOutcome{}, err
	}
	if gate.code != "" {
		// No adapter call can happen for this worker, so its local send is
		// over even though the recorded outcome stays UNKNOWN (or no_effect).
		localFlight = false
		activeLocalDispatches.end(g.WorkspaceID, p.TaskID)
		persistCtx := context.WithoutCancel(ctx)
		if gate.quarantine {
			// A pause or cancel won the race after the permit was consumed but
			// before the adapter call. The send is stopped and the operation
			// stays unknown: nothing proves to a later reader that the external
			// call never happened.
			if err := s.quarantineConsumedSend(persistCtx, actor, g, p, gate.code); err != nil {
				return SendOutcome{}, err
			}
			return SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, problem("EXTERNAL_OUTCOME_UNKNOWN")
		}
		if err := s.refuseConsumedSend(persistCtx, actor, g, p, gate.code); err != nil {
			return SendOutcome{}, err
		}
		return SendOutcome{}, problem(gate.code)
	}

	// A proposal coordinator must still own the task after every blocking
	// content wait and the existing final gateway checks. Never replace them
	// with an early lease.valid check.
	if lease, ok := ctx.Value(proposalLeaseContextKey{}).(*taskLease); ok {
		if err := lease.valid(ctx); err != nil {
			return SendOutcome{}, err
		}
	}
	if err := contentGuard.healthy(ctx); err != nil {
		return SendOutcome{}, err
	}
	sendCtx, stopWatch := contentGuard.watch(ctx)
	defer stopWatch()
	outcome, adapterErr := adapter.RequestJoin(sendCtx, res)
	if healthErr := stopWatch(); healthErr != nil {
		adapterErr = healthErr
	}
	localFlight = false
	activeLocalDispatches.end(g.WorkspaceID, p.TaskID)
	normalized := normalizeOutcome(outcome, adapterErr)
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer persistCancel()
	if err := s.persistOutcome(persistCtx, actor, g.WorkspaceID, g.OperationID, p, normalized); err != nil {
		return SendOutcome{}, err
	}
	if adapterErr != nil {
		return SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, problem("EXTERNAL_OUTCOME_UNKNOWN")
	}
	return SendOutcome{Disposition: normalized.Observation, EffectCertainty: normalized.Effect, EvidenceRef: normalized.EvidenceRef, RequestRef: normalized.RequestRef}, nil
}

// revalidateBeforeSend runs one short transaction between the consume commit
// and the adapter call. It locks the attempt row first, so a concurrent
// scheduler move, pause or cancel cannot slip underneath the check, and then
// re-proves permit expiry, runtime ownership, identity generation and task
// lifecycle. A non-empty result means this worker must not call the adapter.
// sendGate is the outcome of the last pre-send revalidation. A non-empty code
// means this worker must not call the adapter. quarantine marks the refusals
// that stop a still-live send by policy (pause, cancel) rather than by proof
// that no effect is possible: those must be recorded as unknown, never as a
// definite no-effect.
type sendGate struct {
	code       string
	quarantine bool
}

func (s *Store) revalidateBeforeSend(ctx context.Context, actor RuntimeActor, g gatewayRow, p ExecutionPayload) (sendGate, error) {
	var gate sendGate
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var subtaskState string
		var attempt int
		var currentExecution *string
		// The blocking statement reads only the row it locks. Its snapshot is
		// taken before the wait, so a pause committed while this worker is
		// parked would stay invisible to anything read here. Task, permit and
		// account state are read afterwards, each in its own statement, and
		// therefore observe everything committed before them.
		if err := tx.QueryRow(ctx,
			`SELECT state,attempt_count,current_execution_id FROM core.subtask
			  WHERE workspace_id=$1 AND id=$2 AND task_id=$3
			  FOR UPDATE`,
			g.WorkspaceID, p.SubtaskID, p.TaskID).
			Scan(&subtaskState, &attempt, &currentExecution); err != nil {
			return err
		}
		if currentExecution == nil || *currentExecution != g.ExecutionID || attempt != p.Attempt {
			gate.code = "SUBTASK_REASSIGNED"
			return nil
		}
		switch subtaskState {
		case "CANCELLED", "FAILED", "SUCCEEDED", "BLOCKED":
			gate.code = "SUBTASK_NOT_DISPATCHABLE"
			return nil
		}
		// Recovery may quarantine this operation while its worker is parked
		// right here. Re-read the recorded bridge state in its own statement:
		// a committed UNKNOWN is final for send authorization, and "the
		// wake-up proves nothing was sent" must never turn into "so now send
		// it".
		var bridgeState, executionState string
		if err := tx.QueryRow(ctx,
			`SELECT g.state,e.state FROM bridge.gateway_operation g
			   JOIN core.execution e ON (e.workspace_id,e.id)=(g.workspace_id,g.execution_id)
			  WHERE g.workspace_id=$1 AND g.operation_id=$2`,
			g.WorkspaceID, g.OperationID).Scan(&bridgeState, &executionState); err != nil {
			return err
		}
		if bridgeState != "DISPATCHING" || executionState != "DISPATCHING" {
			gate = sendGate{code: "EXTERNAL_OUTCOME_UNKNOWN", quarantine: true}
			return nil
		}
		var taskState string
		var taskExpired bool
		if err := tx.QueryRow(ctx,
			`SELECT state,deadline_at<=clock_timestamp() FROM core.task WHERE workspace_id=$1 AND id=$2`,
			g.WorkspaceID, p.TaskID).Scan(&taskState, &taskExpired); err != nil {
			return err
		}
		switch taskState {
		case "PAUSING", "PAUSED", "CANCELLING", "CANCELLED", "FAILED", "SUCCEEDED", "PARTIAL":
			gate = sendGate{code: "TASK_PAUSED", quarantine: true}
			return nil
		}
		deadline, err := time.Parse(time.RFC3339Nano, p.DeadlineAt)
		if err != nil {
			return err
		}
		var payloadExpired bool
		if err := tx.QueryRow(ctx, `SELECT $1::timestamptz<=clock_timestamp()`, deadline).Scan(&payloadExpired); err != nil {
			return err
		}
		if taskExpired || payloadExpired {
			gate.code = "DEADLINE_EXPIRED"
			return nil
		}
		var permitState, identityStatus, accountRuntime, controlState, riskState, connectionState, loginState string
		var expired, credentialActive bool
		var accountOwner, credentialOwner, generation int64
		if err := tx.QueryRow(ctx,
			`SELECT p.state,p.expires_at<=clock_timestamp(),
			        COALESCE(rc.active,false),COALESCE(rc.owner_epoch,0),
			        a.identity_status,a.runtime_id,a.owner_epoch,a.identity_generation,a.control_state,a.risk_state,a.connection_state,a.login_state
			   FROM core.dispatch_permit p
			   JOIN core.account a ON (a.workspace_id,a.id)=(p.workspace_id,p.account_id)
			   LEFT JOIN core.runtime_credentials rc
			     ON (rc.workspace_id,rc.runtime_id,rc.account_id)=(p.workspace_id,a.runtime_id,a.id)
			  WHERE p.workspace_id=$1 AND p.operation_id=$2`,
			g.WorkspaceID, g.OperationID).
			Scan(&permitState, &expired, &credentialActive, &credentialOwner,
				&identityStatus, &accountRuntime, &accountOwner, &generation, &controlState, &riskState, &connectionState, &loginState); err != nil {
			return err
		}
		if permitState != "consumed" || expired {
			// A dead permit makes this send impossible in time, so the worker
			// can still prove no effect even if an operator paused meanwhile.
			gate.code = "PERMIT_EXPIRED"
			return nil
		}
		if controlState != "active" || riskState != "clear" || connectionState != "online" || loginState != "logged_in" {
			gate = sendGate{code: "ACCOUNT_RESTRICTED", quarantine: true}
			return nil
		}
		if !credentialActive || accountRuntime != actor.RuntimeID ||
			accountOwner != p.OwnerEpoch || credentialOwner != p.OwnerEpoch ||
			accountOwner != actor.OwnerEpoch || identityStatus != "verified" {
			gate.code = "ACCOUNT_RESTRICTED"
			return nil
		}
		if captured, ok := gatewayIdentityGeneration(g); ok && captured != generation {
			gate.code = "ACCOUNT_RESTRICTED"
			return nil
		}
		return nil
	})
	return gate, err
}

// refuseConsumedSend records a definite pre-send refusal for a permit this
// worker already consumed: the adapter was never called, so there is no
// external effect to protect. The execution keeps its attempt and inbox-style
// evidence, the bridge row carries the dispatcher proof the immutable
// execution trigger requires, and only the derived holds are released.
func (s *Store) refuseConsumedSend(ctx context.Context, actor RuntimeActor, g gatewayRow, p ExecutionPayload, code string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, g.WorkspaceID, p.TaskID)
		if err != nil {
			return err
		}
		locked, err := s.loadGateway(ctx, tx, g.WorkspaceID, g.OperationID, true)
		if err != nil {
			return err
		}
		if locked.State != "DISPATCHING" {
			// The durable outcome was already recorded while this worker was
			// parked. Its own send is over without entering the adapter, so
			// settle only the local lifecycle and release nothing.
			return settlePausingTask(ctx, tx, locked.WorkspaceID, locked.TaskID)
		}
		if err := authorizeRuntime(ctx, tx, actor, locked.AccountID); err != nil {
			return err
		}
		if err := validRuntimeOwner(actor, locked); err != nil {
			return err
		}
		row, err := s.loadExecutionByID(ctx, tx, locked.WorkspaceID, locked.ExecutionID)
		if err != nil {
			return err
		}
		permit, err := s.loadPermitByOperation(ctx, tx, locked.WorkspaceID, locked.OperationID, true)
		if err != nil {
			return err
		}
		if permit.State != "consumed" {
			return problem("STATE_CONSTRAINT")
		}
		now, err := s.dbNow(ctx, tx)
		if err != nil {
			return err
		}
		evidence := newID("evid")
		response := mustJSON(map[string]any{"code": code, "dispatched": false})
		if _, err := tx.Exec(ctx, `UPDATE bridge.gateway_operation
			SET state='OBSERVED',disposition='failed_no_effect',effect_certainty='no_effect',
			    response=$3,evidence_ref=$4,updated_at=$5
			WHERE workspace_id=$1 AND operation_id=$2`,
			locked.WorkspaceID, locked.OperationID, string(response), evidence, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE core.execution
			SET state='OBSERVED',disposition='failed_no_effect',effect_certainty='no_effect',
			    evidence_ref=$3,observed_at=$4
			WHERE workspace_id=$1 AND id=$2`,
			row.WorkspaceID, row.ID, evidence, now); err != nil {
			return err
		}
		if err := s.projectWorkItemExecution(ctx, tx, task, row.ID, "dispatch", ""); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE core.account_action_lock SET held=false
			WHERE workspace_id=$1 AND account_id=$2 AND action_family=$3 AND execution_id=$4`,
			row.WorkspaceID, row.AccountID, actionFamilyJoin, row.ID); err != nil {
			return err
		}
		// No external action happened, so the daily quota must not stay spent.
		if _, err := tx.Exec(ctx, `UPDATE core.account_usage SET consumed=consumed-1
			WHERE workspace_id=$1 AND account_id=$2 AND action_family=$3
			  AND window_start=current_date AND consumed>0`,
			row.WorkspaceID, row.AccountID, actionFamilyJoin); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE core.goal_slot
			SET state='free',target_id=NULL,reserved_at=NULL,fulfilled_at=NULL
			WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='reserved'
			  AND NOT core.execution_retains_holds($1,$2,$3)`,
			row.WorkspaceID, row.TaskID, row.TargetID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE core.target_claim SET state='resolved',resolved_at=clock_timestamp()
			WHERE workspace_id=$1 AND task_id=$2 AND subtask_id=$3 AND target_id=$4 AND state='active'
			  AND NOT core.execution_retains_holds($1,$2,$4)`,
			row.WorkspaceID, row.TaskID, row.SubtaskID, row.TargetID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, row.WorkspaceID, row.TaskID,
			"execution.failed_no_effect", "runtime:"+row.RuntimeID, map[string]any{
				"execution_id": row.ID,
				"operation_id": row.OperationID,
				"code":         code,
				"dispatched":   false,
				"retryable":    true,
			}); err != nil {
			return err
		}
		return settlePausingTask(ctx, tx, row.WorkspaceID, row.TaskID)
	})
}

// quarantineConsumedSend records the pause-cancelled send window. The pause was
// persisted before this transaction, the adapter is never called, and the
// operation still becomes UNKNOWN: an auditor who finds this execution after a
// crash cannot distinguish "stopped before the call" from "called and lost the
// receipt", and the pause must not be used to release a possible external
// effect. Holds, claim, slot and consumed quota are all retained for the
// existing read-only reconciliation path.
func (s *Store) quarantineConsumedSend(ctx context.Context, actor RuntimeActor, g gatewayRow, p ExecutionPayload, code string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, g.WorkspaceID, p.TaskID)
		if err != nil {
			return err
		}
		locked, err := s.loadGateway(ctx, tx, g.WorkspaceID, g.OperationID, true)
		if err != nil {
			return err
		}
		if locked.State != "DISPATCHING" {
			// The durable outcome was already recorded while this worker was
			// parked. Its own send is over without entering the adapter, so
			// settle only the local lifecycle and release nothing.
			return settlePausingTask(ctx, tx, locked.WorkspaceID, locked.TaskID)
		}
		if err := authorizeRuntime(ctx, tx, actor, locked.AccountID); err != nil {
			return err
		}
		if err := validRuntimeOwner(actor, locked); err != nil {
			return err
		}
		row, err := s.loadExecutionByID(ctx, tx, locked.WorkspaceID, locked.ExecutionID)
		if err != nil {
			return err
		}
		permit, err := s.loadPermitByOperation(ctx, tx, locked.WorkspaceID, locked.OperationID, true)
		if err != nil {
			return err
		}
		if permit.State != "consumed" {
			return problem("STATE_CONSTRAINT")
		}
		now, err := s.dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE bridge.gateway_operation
			SET state='UNKNOWN',disposition='unknown',effect_certainty='unknown',updated_at=$3
			WHERE workspace_id=$1 AND operation_id=$2 AND state='DISPATCHING'`,
			locked.WorkspaceID, locked.OperationID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE core.execution
			SET state='UNKNOWN',disposition='unknown',effect_certainty='unknown'
			WHERE workspace_id=$1 AND id=$2 AND state IN ('CREATED','ACCEPTED','DISPATCHING')`,
			row.WorkspaceID, row.ID); err != nil {
			return err
		}
		normalized := normalizedOutcome{State: "UNKNOWN", Disposition: new("unknown"), Effect: "unknown", Observation: "unknown"}
		if err := s.insertBridgeObservation(ctx, tx, locked, p, normalized, actor, now); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, row.WorkspaceID, row.TaskID,
			"execution.pause_cancelled_send", "runtime:"+row.RuntimeID, map[string]any{
				"execution_id": row.ID,
				"operation_id": row.OperationID,
				"code":         code,
				"dispatched":   false,
				"effect":       "unknown",
				"retryable":    false,
				"read_only":    true,
			}); err != nil {
			return err
		}
		if err := s.projectWorkItemExecution(ctx, tx, task, row.ID, "dispatch", ""); err != nil {
			return err
		}
		// This worker is in its own pre-adapter gate: no adapter call happened
		// and none can start for this consumed permit. The unknown outcome and
		// its holds stay; only the local lifecycle may settle. A different
		// live worker cannot exist for the same operation because a consumed
		// permit is required to enter the adapter and duplicate dispatch stops
		// at the recorded state.
		return settlePausingTask(ctx, tx, row.WorkspaceID, row.TaskID)
	})
}

func (s *Store) persistOutcome(ctx context.Context, actor RuntimeActor, workspace, operationID string, p ExecutionPayload, normalized normalizedOutcome) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, workspace, p.TaskID)
		if err != nil {
			return err
		}
		g, err := s.loadGateway(ctx, tx, workspace, operationID, true)
		if err != nil {
			return err
		}
		if g.TaskID != p.TaskID || g.WorkspaceID != workspace {
			return problem("STATE_CONSTRAINT")
		}
		if err := authorizeRuntime(ctx, tx, actor, g.AccountID); err != nil {
			return err
		}
		if err := validRuntimeOwner(actor, g); err != nil {
			return err
		}
		if g.State == "OBSERVED" || g.State == "REJECTED_BEFORE_SEND" {
			return nil
		}
		if g.State != "DISPATCHING" && g.State != "UNKNOWN" {
			return problem("STATE_CONSTRAINT")
		}
		e, err := s.loadExecutionByID(ctx, tx, workspace, g.ExecutionID)
		if err != nil {
			return err
		}
		permit, err := s.loadPermitByOperation(ctx, tx, workspace, operationID, true)
		if err != nil {
			return err
		}
		if permit.State != "consumed" {
			return problem("STATE_CONSTRAINT")
		}
		now, err := s.dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if normalized.State != "UNKNOWN" {
			if _, err := tx.Exec(ctx, `UPDATE core.account_action_lock SET held=false
				WHERE workspace_id=$1 AND account_id=$2 AND action_family=$3 AND execution_id=$4`,
				e.WorkspaceID, e.AccountID, actionFamilyJoin, e.ID); err != nil {
				return err
			}
		}
		// The bridge proof row is written first so the immutable execution
		// trigger can verify a technical no-effect downgrade in the same tx.
		if _, err := tx.Exec(ctx, `UPDATE bridge.gateway_operation
			SET state=$3,disposition=$4,effect_certainty=$5,response=$6,evidence_ref=$7,updated_at=$8
			WHERE workspace_id=$1 AND operation_id=$2`,
			g.WorkspaceID, g.OperationID, normalized.State, normalized.Disposition, normalized.Effect,
			string(normalized.SafeResponse), nullableText(normalized.EvidenceRef), now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE core.execution
			SET state=$3,disposition=$4,effect_certainty=$5,external_request_ref=$6,evidence_ref=$7,observed_at=$8
			WHERE workspace_id=$1 AND id=$2`,
			e.WorkspaceID, e.ID, normalized.State, normalized.Disposition, normalized.Effect,
			nullableText(normalized.RequestRef), nullableText(normalized.EvidenceRef), now); err != nil {
			return err
		}
		if err := s.insertBridgeObservation(ctx, tx, g, p, normalized, actor, now); err != nil {
			return err
		}
		if err := s.projectWorkItemExecution(ctx, tx, task, e.ID, "dispatch", ""); err != nil {
			return err
		}
		return settlePausingTask(ctx, tx, workspace, p.TaskID)
	})
}

func gatewayIdentityGeneration(g gatewayRow) (int64, bool) {
	if len(g.Response) == 0 {
		return 0, false
	}
	var response struct {
		IdentityGeneration *int64 `json:"identity_generation"`
	}
	if json.Unmarshal(g.Response, &response) != nil || response.IdentityGeneration == nil {
		return 0, false
	}
	return *response.IdentityGeneration, true
}

func nullableText(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

type bridgeObservation struct {
	SchemaVersion   string          `json:"schema_version"`
	EventID         string          `json:"event_id"`
	WorkspaceID     string          `json:"workspace_id"`
	SourceRuntimeID string          `json:"source_runtime_id"`
	SourceAccountID string          `json:"source_account_id"`
	SourceEpoch     string          `json:"source_epoch"`
	SourceSequence  string          `json:"source_sequence"`
	OperationID     string          `json:"operation_id"`
	Target          contract.Target `json:"target"`
	Kind            string          `json:"kind"`
	Payload         bridgeObserved  `json:"payload"`
	ObservedAt      string          `json:"observed_at"`
	EvidenceRef     string          `json:"evidence_ref"`
}

type bridgeObserved struct {
	Outcome         string  `json:"outcome"`
	EffectCertainty string  `json:"effect_certainty"`
	RequestRef      *string `json:"request_ref"`
	MessageRef      *string `json:"message_ref"`
}

func bridgeEvidence(workspace, operationID string) string {
	h, err := digest(map[string]string{"workspace_id": workspace, "operation_id": operationID})
	if err != nil {
		return "bridge_unknown_operation"
	}
	return "bridge_" + h[:32]
}

func (s *Store) insertBridgeObservation(ctx context.Context, tx pgx.Tx, g gatewayRow, p ExecutionPayload, normalized normalizedOutcome, actor RuntimeActor, now time.Time) error {
	evidence := normalized.EvidenceRef
	if evidence == "" {
		evidence = bridgeEvidence(g.WorkspaceID, g.OperationID)
	}
	eventDigest, err := digest(map[string]any{"workspace_id": g.WorkspaceID, "operation_id": g.OperationID, "state": normalized.State, "disposition": normalized.Observation})
	if err != nil {
		return err
	}
	eventID := "evt_" + eventDigest[:32]
	outboxID := "bo_" + eventDigest[:32]
	payload := bridgeObservation{
		SchemaVersion: "1", EventID: eventID, WorkspaceID: g.WorkspaceID,
		SourceRuntimeID: g.RuntimeID, SourceAccountID: g.AccountID, SourceEpoch: strconv.FormatInt(actor.OwnerEpoch, 10),
		SourceSequence: strconv.FormatInt(now.UnixNano(), 10), OperationID: g.OperationID, Target: targetFromPayload(p),
		Kind: bridgeEventKind,
		Payload: bridgeObserved{
			Outcome: normalized.Observation, EffectCertainty: normalized.Effect,
			RequestRef: nullableText(normalized.RequestRef), MessageRef: nil,
		},
		ObservedAt: now.UTC().Format(time.RFC3339Nano), EvidenceRef: evidence,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO bridge.outbox(workspace_id,id,operation_id,event_type,payload,state)
		VALUES($1,$2,$3,$4,$5,'pending') ON CONFLICT (workspace_id,id) DO NOTHING`,
		g.WorkspaceID, outboxID, g.OperationID, bridgeEventKind, string(body))
	return err
}

// ExpirePermits safely retires only provably unconsumed, already-expired
// permits owned by this runtime/owner. It rejects the operation before send and
// releases the short send lock; it never downgrades UNKNOWN/consumed effects and
// never releases goal slots, target claims or reserved usage.
func (s *Store) ExpirePermits(ctx context.Context, actor RuntimeActor) (int, error) {
	if actor.WorkspaceID == "" || actor.RuntimeID == "" || actor.OwnerEpoch <= 0 || len(actor.AccountIDs) == 0 {
		return 0, problem("VALIDATION_FAILED")
	}
	expired := 0
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT g.operation_id,g.task_id
			FROM bridge.gateway_operation g
			JOIN core.dispatch_permit p
			  ON (p.workspace_id,p.operation_id)=(g.workspace_id,g.operation_id)
			WHERE g.workspace_id=$1 AND g.runtime_id=$2 AND g.owner_epoch=$3
			  AND g.state='DISPATCHING' AND g.account_id=ANY($4::text[])
			  AND p.state='issued' AND p.expires_at<=clock_timestamp()
			ORDER BY g.task_id,g.operation_id`, actor.WorkspaceID, actor.RuntimeID, actor.OwnerEpoch, actor.AccountIDs)
		if err != nil {
			return err
		}
		type pendingExpiry struct{ operationID, taskID string }
		var pending []pendingExpiry
		for rows.Next() {
			var item pendingExpiry
			if err := rows.Scan(&item.operationID, &item.taskID); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		lastTask := ""
		for _, item := range pending {
			ok, err := recoveryTaskUnowned(ctx, tx, actor.WorkspaceID, item.taskID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if item.taskID != lastTask {
				if _, err := lockTask(ctx, tx, actor.WorkspaceID, item.taskID); err != nil {
					return err
				}
				lastTask = item.taskID
			}
			g, err := s.loadGateway(ctx, tx, actor.WorkspaceID, item.operationID, true)
			if err != nil {
				return err
			}
			if g.TaskID != item.taskID || g.State != "DISPATCHING" || g.PermitID == nil {
				continue
			}
			if err := authorizeRuntime(ctx, tx, actor, g.AccountID); err != nil {
				continue
			}
			if err := validRuntimeOwner(actor, g); err != nil {
				continue
			}
			e, err := s.loadExecutionByID(ctx, tx, g.WorkspaceID, g.ExecutionID)
			if err != nil {
				return err
			}
			p, err := decodeExecutionPayload(e)
			if err != nil {
				return err
			}
			if err := verifyGatewayEnvelope(g, e, p); err != nil {
				return err
			}
			permit, err := s.loadPermitByOperation(ctx, tx, g.WorkspaceID, g.OperationID, true)
			if err != nil {
				return err
			}
			if *g.PermitID != permit.ID {
				continue
			}
			now, err := s.dbNow(ctx, tx)
			if err != nil {
				return err
			}
			if permit.State != "issued" || now.Before(permit.ExpiresAt) {
				continue
			}
			// A never-consumed permit proves no adapter call happened. Only a
			// pre-send execution state may be classified no_effect; uncertain
			// or already observed states are left untouched.
			if e.State != "CREATED" && e.State != "ACCEPTED" {
				continue
			}
			if err := s.rejectBeforeSend(ctx, tx, g, e, "PERMIT_EXPIRED"); err != nil {
				return err
			}
			expired++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return expired, nil
}

// RecoverDispatching quarantines this runtime/owner's DISPATCHING operations as
// durably UNKNOWN without sending. Goal slots, target claims and the account
// action lock stay held; no no_effect claim is invented.
//
// The caller does not have to prove that the old workers are dead. A worker
// that is still alive resumes only through revalidateBeforeSend, which re-reads
// the recorded gateway state in its own statement: once this transaction
// commits UNKNOWN, the parked worker refuses to call the adapter and returns
// EXTERNAL_OUTCOME_UNKNOWN instead of turning "the wake-up proves nothing was
// sent" into a second send.
func (s *Store) RecoverDispatching(ctx context.Context, actor RuntimeActor) (int, error) {
	if actor.WorkspaceID == "" || actor.RuntimeID == "" || actor.OwnerEpoch <= 0 || len(actor.AccountIDs) == 0 {
		return 0, problem("VALIDATION_FAILED")
	}
	recovered := 0
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// Advisory read only; task locks are taken first and in task_id order so
		// the deferred execution closure trigger cannot invert our lock order.
		rows, err := tx.Query(ctx, `SELECT operation_id,task_id FROM bridge.gateway_operation
			WHERE workspace_id=$1 AND runtime_id=$2 AND owner_epoch=$3 AND state='DISPATCHING' AND account_id=ANY($4::text[])
			ORDER BY task_id,operation_id`, actor.WorkspaceID, actor.RuntimeID, actor.OwnerEpoch, actor.AccountIDs)
		if err != nil {
			return err
		}
		type pendingOperation struct{ operationID, taskID string }
		var pending []pendingOperation
		for rows.Next() {
			var item pendingOperation
			if err := rows.Scan(&item.operationID, &item.taskID); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		lastTask := ""
		var task TaskRow
		for _, item := range pending {
			ok, err := recoveryTaskUnowned(ctx, tx, actor.WorkspaceID, item.taskID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if item.taskID != lastTask {
				task, err = lockTask(ctx, tx, actor.WorkspaceID, item.taskID)
				if err != nil {
					return err
				}
				lastTask = item.taskID
			}
			g, err := s.loadGateway(ctx, tx, actor.WorkspaceID, item.operationID, true)
			if err != nil {
				return err
			}
			if g.TaskID != item.taskID || g.State != "DISPATCHING" {
				continue
			}
			if err := authorizeRuntime(ctx, tx, actor, g.AccountID); err != nil {
				continue
			}
			if err := validRuntimeOwner(actor, g); err != nil {
				continue
			}
			e, err := s.loadExecutionByID(ctx, tx, g.WorkspaceID, g.ExecutionID)
			if err != nil {
				return err
			}
			p, err := decodeExecutionPayload(e)
			if err != nil {
				return err
			}
			if err := verifyGatewayEnvelope(g, e, p); err != nil {
				return err
			}
			now, err := s.dbNow(ctx, tx)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE bridge.gateway_operation
				SET state='UNKNOWN',disposition='unknown',effect_certainty='unknown',updated_at=$3
				WHERE workspace_id=$1 AND operation_id=$2 AND state='DISPATCHING'`,
				g.WorkspaceID, g.OperationID, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE core.execution
				SET state='UNKNOWN',disposition='unknown',effect_certainty='unknown'
				WHERE workspace_id=$1 AND id=$2 AND state IN ('CREATED','ACCEPTED','DISPATCHING')`,
				e.WorkspaceID, e.ID); err != nil {
				return err
			}
			normalized := normalizedOutcome{State: "UNKNOWN", Disposition: new("unknown"), Effect: "unknown", Observation: "unknown"}
			if err := s.insertBridgeObservation(ctx, tx, g, p, normalized, actor, now); err != nil {
				return err
			}
			if err := s.projectWorkItemExecution(ctx, tx, task, e.ID, "dispatch", ""); err != nil {
				return err
			}
			recovered++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return recovered, nil
}

// GetReservation reconstructs the immutable canonical reservation from the
// persisted execution payload and includes the request digest. Callers can
// compare this with their own Reservation without trusting input.
func (s *Store) GetReservation(ctx context.Context, actor RuntimeActor, operationID string) (Reservation, error) {
	var out Reservation
	if operationID == "" {
		return out, problem("VALIDATION_FAILED")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		g, err := s.loadGateway(ctx, tx, actor.WorkspaceID, operationID, false)
		if err != nil {
			return err
		}
		if err := authorizeRuntime(ctx, tx, actor, g.AccountID); err != nil {
			return err
		}
		if err := validRuntimeOwner(actor, g); err != nil {
			return err
		}
		e, err := s.loadExecutionByID(ctx, tx, g.WorkspaceID, g.ExecutionID)
		if err != nil {
			return err
		}
		p, err := decodeExecutionPayload(e)
		if err != nil {
			return err
		}
		if err := verifyGatewayEnvelope(g, e, p); err != nil {
			return err
		}
		res, err := reservationFromPayload(p)
		if err != nil {
			return err
		}
		res.Digest = e.RequestDigest
		out = res
		return nil
	})
	if err != nil {
		return Reservation{}, err
	}
	return out, nil
}

// GetUnacceptedReservation reconstructs a pending execution from its immutable
// payload before the gateway ledger exists. Only the owning runtime may read it.
func (s *Store) GetUnacceptedReservation(ctx context.Context, actor RuntimeActor, operationID string) (Reservation, error) {
	var out Reservation
	if operationID == "" {
		return out, problem("VALIDATION_FAILED")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		e, err := s.loadExecutionByOperation(ctx, tx, actor.WorkspaceID, operationID)
		if err != nil {
			return err
		}
		if err := authorizeRuntime(ctx, tx, actor, e.AccountID); err != nil {
			return err
		}
		if e.RuntimeID != actor.RuntimeID || e.OwnerEpoch != actor.OwnerEpoch || e.State != "CREATED" {
			return problem("RESOURCE_NOT_FOUND")
		}
		p, err := decodeExecutionPayload(e)
		if err != nil {
			return err
		}
		if p.WorkspaceID != actor.WorkspaceID || p.RuntimeID != actor.RuntimeID || p.OwnerEpoch != actor.OwnerEpoch {
			return problem("RESOURCE_NOT_FOUND")
		}
		if err := validateTaskApprovalPayload(ctx, tx, e, p); err != nil {
			return err
		}
		out, err = reservationFromPayload(p)
		if err != nil {
			return err
		}
		out.Digest = e.RequestDigest
		return nil
	})
	if err != nil {
		return Reservation{}, err
	}
	return out, nil
}

func validateTaskApprovalPayload(ctx context.Context, tx pgx.Tx, e executionRow, p ExecutionPayload) error {
	task, err := lockTask(ctx, tx, e.WorkspaceID, e.TaskID)
	if err != nil {
		return err
	}
	if task.ApprovalID == nil || *task.ApprovalID != p.ApprovalID || task.ScopeRevision != p.ScopeRevision {
		return problem("OPERATION_PAYLOAD_CONFLICT")
	}
	var reason, answer string
	var revoked bool
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT join_reason,verification_message,revoked,expires_at
		FROM core.approval WHERE workspace_id=$1 AND id=$2`, e.WorkspaceID, p.ApprovalID).
		Scan(&reason, &answer, &revoked, &expires)
	if err != nil || revoked || !expires.After(time.Now()) || reason != p.JoinReason || answer != p.VerificationMessage {
		return problem("OPERATION_PAYLOAD_CONFLICT")
	}
	return nil
}

// GetOperation is the read-only persisted-ledger projection.
func (s *Store) GetOperation(ctx context.Context, actor RuntimeActor, operationID string) (contract.Operation, error) {
	var out contract.Operation
	if operationID == "" {
		return out, problem("VALIDATION_FAILED")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		g, err := s.loadGateway(ctx, tx, actor.WorkspaceID, operationID, false)
		if err != nil {
			return err
		}
		if err := authorizeRuntime(ctx, tx, actor, g.AccountID); err != nil {
			return err
		}
		if err := validRuntimeOwner(actor, g); err != nil {
			return err
		}
		body := map[string]any{
			"operation_id": g.OperationID, "state": g.State, "effect_certainty": g.EffectCertainty,
			"disposition": nil, "external_request_ref": nil, "observed_at": nil, "evidence_ref": nil, "error": nil,
		}
		if g.Disposition != nil {
			body["disposition"] = *g.Disposition
		}
		if g.EvidenceRef != nil {
			body["evidence_ref"] = *g.EvidenceRef
		}
		if g.State == "OBSERVED" || g.State == "UNKNOWN" {
			body["observed_at"] = g.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
		if len(g.Response) > 0 {
			var response struct {
				RequestRef string `json:"request_ref"`
				Code       string `json:"code"`
			}
			if json.Unmarshal(g.Response, &response) == nil {
				if response.RequestRef != "" {
					body["external_request_ref"] = response.RequestRef
				}
				if response.Code != "" {
					body["error"] = map[string]any{
						"code": response.Code, "message": "operation rejected before send", "retryable": false,
						"retry_after_ms": nil, "details": map[string]any{},
					}
				}
			}
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return contract.Operation{}, err
	}
	return out, nil
}

// GetPermit returns the persisted permit with its current derived state.
func (s *Store) GetPermit(ctx context.Context, actor RuntimeActor, operationID string) (contract.DispatchPermit, error) {
	var out contract.DispatchPermit
	if operationID == "" {
		return out, problem("VALIDATION_FAILED")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		g, err := s.loadGateway(ctx, tx, actor.WorkspaceID, operationID, false)
		if err != nil {
			return err
		}
		if err := authorizeRuntime(ctx, tx, actor, g.AccountID); err != nil {
			return err
		}
		if err := validRuntimeOwner(actor, g); err != nil {
			return err
		}
		permit, err := s.loadPermitByOperation(ctx, tx, g.WorkspaceID, operationID, false)
		if err != nil {
			return err
		}
		now, err := s.dbNow(ctx, tx)
		if err != nil {
			return err
		}
		state := permit.State
		if state == "issued" && !now.Before(permit.ExpiresAt) {
			state = "expired"
		}
		out = contract.DispatchPermit{
			PermitId: permit.ID, OperationId: permit.OperationID, AccountId: permit.AccountID,
			Target: operationalTarget(g), Action: contract.ActionRequestJoin, RequestDigest: permit.Digest,
			ScopeRevision: permit.ScopeRevision, OwnerEpoch: strconv.FormatInt(permit.OwnerEpoch, 10),
			FenceToken: strconv.FormatInt(permit.Fence, 10), IssuedAt: permit.IssuedAt.UTC(),
			ExpiresAt: permit.ExpiresAt.UTC(), State: contract.DispatchPermitState(state),
		}
		return nil
	})
	if err != nil {
		return contract.DispatchPermit{}, err
	}
	return out, nil
}
