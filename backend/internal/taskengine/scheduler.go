package taskengine

// P0-07 scheduler: transactional claim/slot/account-quota reservation.
//
// Reserve is the only writer of a new dispatch attempt. It never performs an
// external request: the returned Reservation is consumed later by the bridge,
// which reads the immutable core.execution row and the persistent outbox event
// written here.
//
// Stable Reserve error codes (surfaced through Code(err)):
//   NO_WORK                task/subtask is not dispatchable now: task not RUNNING,
//                          non-live mode, import incomplete, baseline incomplete,
//                          deadline passed, approval missing/revoked/expired/out of
//                          scope, no queued or due RETRY_WAIT subtask, target already
//                          fulfilled, or a used-up logical subtask.
//   NO_CAPACITY            dispatchable work exists but a resource is unavailable:
//                          target claimed by another task, no free goal slot, no
//                          account passes identity/readiness, or retry pinned to an
//                          account that cannot act.
//   LOCAL_BUDGET_EXHAUSTED finite attempt budget or the per-account window quota is
//                          exhausted; an account change never resets either.
//   CAPABILITY_UNAVAILABLE no in-scope account proves the request_join capability.
//                          Conservative: an unproven capability is never assumed.
//   RESOURCE_NOT_FOUND     task does not exist in the workspace.
//
// Lock order inside the transaction is the one fixed by the design:
// task -> target/claim -> account/usage (ascending account id) -> subtask/execution/slot.
// All state is re-checked under those locks; the pre-read candidate list is advisory.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	schedActionFamily = "group.join"
	schedAction       = "request_join"

	schedCodeNoWork             = "NO_WORK"
	schedCodeNoCapacity         = "NO_CAPACITY"
	schedCodeLocalBudget        = "LOCAL_BUDGET_EXHAUSTED"
	schedCodeCapabilityUnavail  = "CAPABILITY_UNAVAILABLE"
	schedMaxCandidateSubtasks   = 64
	schedDispatchRequestedEvent = "dispatch.requested"
	schedDispatchReservedEvent  = "dispatch.reserved"
)

// ExecutionPayload is the canonical, immutable request envelope of one dispatch
// attempt. It is stored verbatim (jsonb) in core.execution.request_payload and
// core.execution.request_digest is digest(ExecutionPayload). The bridge decodes
// the stored jsonb, re-digests it and compares every field with the caller's
// Reservation; it must not trust the caller. Only strings and integers are used
// so that decode -> re-encode -> digest is byte stable.
type ExecutionPayload struct {
	WorkspaceID         string `json:"workspace_id"`
	TaskID              string `json:"task_id"`
	SubtaskID           string `json:"subtask_id"`
	ExecutionID         string `json:"execution_id"`
	OperationID         string `json:"operation_id"`
	ApprovalID          string `json:"approval_ref"`
	Action              string `json:"action"`
	AccountID           string `json:"account_id"`
	RuntimeID           string `json:"runtime_id"`
	TargetID            string `json:"target_id"`
	GroupID             string `json:"group_id"`
	OwnerEpoch          int64  `json:"owner_epoch"`
	Fence               int64  `json:"fence_token"`
	ScopeRevision       int    `json:"scope_revision"`
	Attempt             int    `json:"attempt_no"`
	DeadlineAt          string `json:"deadline_at"`
	JoinReason          string `json:"join_reason"`
	VerificationMessage string `json:"verification_message"`
}

// reserveApproval is the re-read, still-live approval that authorizes a live
// request_join attempt.
type reserveApproval struct {
	principalID         string
	scopeRevision       int
	accountIDs          []string
	targetIDs           []string
	maxAttempts         int
	expiresAt           time.Time
	joinReason          string
	verificationMessage string
}

// reserveCandidate is one queued or due RETRY_WAIT membership subtask with its
// target and optional scheduled retry.
type reserveCandidate struct {
	subtaskID         string
	targetID          string
	targetExternalID  string
	eligibility       string
	baselineCovered   bool
	targetDisabled    bool
	retryID           *string
	retryStrategy     *string
	retryPinnedAcctID *string
}

// reserveAccount is one in-scope account with the facts needed for readiness,
// capability, identity and quota checks.
type reserveAccount struct {
	id                  string
	runtimeID           string
	ownerEpoch          int64
	quotaLimit          int
	identityStatus      string
	connectionState     string
	loginState          string
	controlState        string
	riskState           string
	requestJoinVerified bool
	nextActionAt        time.Time
	runtimeBound        bool
	baselineComplete    bool
	reserved            int
	consumed            int
}

func (a reserveAccount) ready() bool {
	return a.identityStatus == "verified" &&
		a.connectionState == "online" &&
		a.loginState == "logged_in" &&
		a.controlState == "active" &&
		a.riskState == "clear" &&
		a.runtimeBound &&
		a.baselineComplete
}

func (a reserveAccount) active() int { return a.reserved + a.consumed }

// reserveRequest narrows one reservation. The zero value is the legacy
// scheduler: any in-scope queued target/account may be chosen. A work-item bound
// request names the exact subtask, target and account the approved step selected;
// it is never a widening, because the scheduler still re-checks every gate.
type reserveRequest struct {
	PreferredAccountID string
	BoundSubtaskID     string
	BoundTargetID      string
	BoundAccountID     string
}

// Reserve claims exactly one target/goal-slot/account attempt for the task and
// returns its immutable reservation. preferredAccountID is a hint restricted to
// the task account scope; it is never an authorization and is ignored when it is
// out of scope or cannot act. No network call is made.
func (s *Store) Reserve(ctx context.Context, workspace, taskID, preferredAccountID string) (Reservation, error) {
	var reservation Reservation
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, workspace, taskID)
		if err != nil {
			return err
		}
		reservation, err = s.reserveLocked(ctx, tx, task, reserveRequest{PreferredAccountID: preferredAccountID})
		return err
	})
	if err != nil {
		return Reservation{}, err
	}
	return reservation, nil
}

// reserveLocked performs the whole reservation under an already locked task row.
// Callers own the transaction so the work-item binding and its attempt row commit
// with the execution they describe.
func (s *Store) reserveLocked(ctx context.Context, tx pgx.Tx, task TaskRow, req reserveRequest) (Reservation, error) {
	var reservation Reservation
	workspace, taskID := task.WorkspaceID, task.ID
	if task.OrchestrationMode == "proposal" && req.BoundSubtaskID == "" {
		// A proposal task is expanded only by materialized work items.
		return reservation, problem(schedCodeNoWork)
	}
	if task.OrchestrationMode != "proposal" && req.BoundSubtaskID != "" {
		return reservation, problem(taskCodeStateConflict)
	}
	err := func() error {
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
			return err
		}

		// Recheck task-level gates under the task lock.
		if task.State != "RUNNING" || task.Mode != "live" || !task.ImportDone || !task.BaselineComplete {
			return problem(schedCodeNoWork)
		}
		if !task.Deadline.After(dbNow) {
			return problem(schedCodeNoWork)
		}
		approval, err := loadReserveApproval(ctx, tx, task)
		if err != nil {
			return err
		}

		var candidates []reserveCandidate
		if req.BoundSubtaskID != "" {
			bound, err := loadBoundReserveCandidate(ctx, tx, workspace, taskID, req.BoundSubtaskID, req.BoundTargetID)
			if err != nil {
				return err
			}
			candidates = []reserveCandidate{bound}
		} else {
			var err error
			candidates, err = loadReserveCandidates(ctx, tx, workspace, taskID)
			if err != nil {
				return err
			}
		}
		if len(candidates) == 0 {
			return problem(schedCodeNoWork)
		}

		// Pick the first candidate whose global target claim is free (or already
		// ours from a retained retry). At most one claim is acquired per
		// transaction, which keeps claim acquisition deadlock free.
		var chosen *reserveCandidate
		for i := range candidates {
			cand := &candidates[i]
			if cand.targetDisabled || cand.eligibility != "eligible" || cand.baselineCovered {
				continue
			}
			claimed, err := ensureTargetClaim(ctx, tx, workspace, taskID, cand)
			if err != nil {
				return err
			}
			if !claimed {
				continue
			}
			chosen = cand
			break
		}
		if chosen == nil {
			return problem(schedCodeNoCapacity)
		}

		// Account selection and quota (account locks ascending by id).
		account, err := selectReserveAccount(ctx, tx, workspace, taskID, chosen, approval, req.PreferredAccountID, req.BoundAccountID, dbNow)
		if err != nil {
			return err
		}

		// Subtask, retry and finite-budget checks under the subtask lock.
		subtaskRow, err := lockReserveSubtask(ctx, tx, workspace, chosen)
		if err != nil {
			return err
		}
		retryConsumed, err := lockConsumeReservationRetry(ctx, tx, workspace, chosen, account.id, dbNow)
		if err != nil {
			return err
		}

		attemptNo := subtaskRow.attemptCount + 1
		if attemptNo > subtaskRow.maxAttempts || attemptNo > task.MaxAttempts {
			return problem(schedCodeLocalBudget)
		}
		if approval != nil && attemptNo > approval.maxAttempts {
			return problem(schedCodeLocalBudget)
		}
		if task.MaxTotalAttempts > 0 {
			var total int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, workspace, taskID).Scan(&total); err != nil {
				return err
			}
			if total >= task.MaxTotalAttempts {
				return problem(schedCodeLocalBudget)
			}
		}

		slotNo, slotReused, err := reserveGoalSlot(ctx, tx, workspace, taskID, chosen.targetID)
		if err != nil {
			return err
		}

		fence := subtaskRow.fence + 1
		deadline := task.Deadline
		if approval != nil && approval.expiresAt.Before(deadline) {
			deadline = approval.expiresAt
		}
		executionID := newID("exec")
		operationID := newID("op")
		payload := ExecutionPayload{
			WorkspaceID:         workspace,
			TaskID:              taskID,
			SubtaskID:           chosen.subtaskID,
			ExecutionID:         executionID,
			OperationID:         operationID,
			ApprovalID:          *task.ApprovalID,
			Action:              schedAction,
			AccountID:           account.id,
			RuntimeID:           account.runtimeID,
			TargetID:            chosen.targetID,
			GroupID:             chosen.targetExternalID,
			JoinReason:          approval.joinReason,
			VerificationMessage: approval.verificationMessage,
			OwnerEpoch:          account.ownerEpoch,
			Fence:               fence,
			ScopeRevision:       task.ScopeRevision,
			Attempt:             attemptNo,
			DeadlineAt:          deadline.UTC().Format(time.RFC3339Nano),
		}
		requestDigest, err := digest(payload)
		if err != nil {
			return err
		}
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `INSERT INTO core.execution(
 workspace_id,id,task_id,subtask_id,operation_id,account_id,target_id,runtime_id,owner_epoch,
 fence_token,scope_revision,attempt_no,action,request_digest,request_payload,state)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'CREATED')`,
			workspace, executionID, taskID, chosen.subtaskID, operationID, account.id, chosen.targetID, account.runtimeID,
			account.ownerEpoch, fence, task.ScopeRevision, attemptNo, schedAction, requestDigest, payloadJSON); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `UPDATE core.subtask SET state='CLAIMED',assigned_account_id=$3,
 current_execution_id=$4,attempt_count=$5,fence_token=$6,version=version+1
 WHERE workspace_id=$1 AND id=$2`, workspace, chosen.subtaskID, account.id, executionID, attemptNo, fence); err != nil {
			return err
		}

		if retryConsumed {
			if _, err := tx.Exec(ctx, `UPDATE core.retry SET state='consumed'
 WHERE workspace_id=$1 AND subtask_id=$2 AND state='scheduled'`, workspace, chosen.subtaskID); err != nil {
				return err
			}
		}

		if err := reserveAccountUsage(ctx, tx, workspace, account); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `INSERT INTO core.outbox(workspace_id,id,task_id,event_type,payload)
 VALUES($1,$2,$3,$4,$5)`, workspace, newID("out"), taskID, schedDispatchRequestedEvent, payloadJSON); err != nil {
			return err
		}

		if err := appendEvent(ctx, tx, workspace, taskID, schedDispatchReservedEvent, "scheduler", map[string]any{
			"execution_id":   executionID,
			"operation_id":   operationID,
			"subtask_id":     chosen.subtaskID,
			"account_id":     account.id,
			"target_id":      chosen.targetID,
			"slot_no":        slotNo,
			"slot_reused":    slotReused,
			"attempt":        attemptNo,
			"fence_token":    fence,
			"scope_revision": task.ScopeRevision,
			"request_digest": requestDigest,
		}); err != nil {
			return err
		}

		reservation = Reservation{
			WorkspaceID:         workspace,
			TaskID:              taskID,
			SubtaskID:           chosen.subtaskID,
			ExecutionID:         executionID,
			OperationID:         operationID,
			AccountID:           account.id,
			RuntimeID:           account.runtimeID,
			TargetID:            chosen.targetID,
			GroupID:             chosen.targetExternalID,
			Digest:              requestDigest,
			JoinReason:          approval.joinReason,
			VerificationMessage: approval.verificationMessage,
			OwnerEpoch:          account.ownerEpoch,
			Fence:               fence,
			ScopeRevision:       task.ScopeRevision,
			Attempt:             attemptNo,
			Deadline:            deadline,
		}
		return nil
	}()
	if err != nil {
		return Reservation{}, err
	}
	return reservation, nil
}

// loadReserveApproval re-reads the live approval. A live dispatch without a
// current, unrevoked, scope-matching, account/target-scoped approval is never
// reserved.
func loadReserveApproval(ctx context.Context, tx pgx.Tx, task TaskRow) (*reserveApproval, error) {
	if task.ApprovalID == nil {
		return nil, problem(schedCodeNoWork)
	}
	var (
		approval reserveApproval
		action   string
		revoked  bool
		live     bool
	)
	err := tx.QueryRow(ctx, `SELECT principal_id,scope_revision,action,account_ids,target_ids,
	 max_attempts,expires_at,revoked,expires_at>now(),join_reason,verification_message
	 FROM core.approval WHERE workspace_id=$1 AND id=$2`, task.WorkspaceID, *task.ApprovalID).
		Scan(&approval.principalID, &approval.scopeRevision, &action, &approval.accountIDs,
			&approval.targetIDs, &approval.maxAttempts, &approval.expiresAt, &revoked, &live,
			&approval.joinReason, &approval.verificationMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem(schedCodeNoWork)
	}
	if err != nil {
		return nil, err
	}
	if revoked || !live || action != schedAction || approval.scopeRevision != task.ScopeRevision || approval.principalID != task.PrincipalID ||
		len([]rune(approval.joinReason)) > 500 || len([]rune(approval.verificationMessage)) > 500 {
		return nil, problem(schedCodeNoWork)
	}
	return &approval, nil
}

// loadReserveCandidates reads queued and due RETRY_WAIT membership subtasks.
// Result-bearing or fulfilled targets and subtasks with a possibly effective
// execution are excluded: an unresolved external effect never gets a second
// attempt and never loses its slot/claim. Once a retry row exists it must be due;
// a same_account retry later pins the account to the previous execution account.
func loadReserveCandidates(ctx context.Context, tx pgx.Tx, workspace, taskID string) ([]reserveCandidate, error) {
	rows, err := tx.Query(ctx, `SELECT s.id,s.target_id,tg.external_id,
 tt.eligibility,tt.baseline_covered,tg.disabled,
 r.id,r.strategy,p.account_id
 FROM core.subtask s
 JOIN core.target tg ON (tg.workspace_id,tg.id)=(s.workspace_id,s.target_id)
 JOIN core.task_target tt ON (tt.workspace_id,tt.task_id,tt.target_id)=(s.workspace_id,s.task_id,s.target_id)
 LEFT JOIN core.retry r ON r.workspace_id=s.workspace_id AND r.subtask_id=s.id AND r.state='scheduled'
 LEFT JOIN core.execution p ON (p.workspace_id,p.id)=(r.workspace_id,r.previous_execution_id)
 WHERE s.workspace_id=$1 AND s.task_id=$2
   AND s.kind='ensure_membership'
   AND s.state IN ('QUEUED','RETRY_WAIT')
   AND s.current_execution_id IS NULL
   AND s.next_run_at<=now()
   AND (r.id IS NULL OR r.due_at<=now())
   AND NOT EXISTS(SELECT 1 FROM core.goal_slot gs WHERE gs.workspace_id=s.workspace_id AND gs.task_id=s.task_id AND gs.target_id=s.target_id AND gs.state='fulfilled')
   AND NOT EXISTS(SELECT 1 FROM core.result res WHERE res.workspace_id=s.workspace_id AND res.task_id=s.task_id AND res.target_id=s.target_id AND res.valid)
   AND NOT EXISTS(SELECT 1 FROM core.execution e WHERE e.workspace_id=s.workspace_id AND e.subtask_id=s.id
        AND (e.state IN ('DISPATCHING','UNKNOWN') OR e.effect_certainty IN ('unknown','confirmed_submitted')))
 ORDER BY tg.external_id,s.id
 LIMIT $3`, workspace, taskID, schedMaxCandidateSubtasks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reserveCandidate
	for rows.Next() {
		var c reserveCandidate
		if err := rows.Scan(&c.subtaskID, &c.targetID, &c.targetExternalID,
			&c.eligibility, &c.baselineCovered, &c.targetDisabled,
			&c.retryID, &c.retryStrategy, &c.retryPinnedAcctID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// loadBoundReserveCandidate reads exactly one materialized step's subtask. It
// applies the same result/claim/retained-effect exclusions as the generic loader,
// so a work item can never revive a target that already has a possibly effective
// execution.
func loadBoundReserveCandidate(ctx context.Context, tx pgx.Tx, workspace, taskID, subtaskID, targetID string) (reserveCandidate, error) {
	var c reserveCandidate
	err := tx.QueryRow(ctx, `SELECT s.id,s.target_id,tg.external_id,
 tt.eligibility,tt.baseline_covered,tg.disabled,
 r.id,r.strategy,p.account_id
 FROM core.subtask s
 JOIN core.target tg ON (tg.workspace_id,tg.id)=(s.workspace_id,s.target_id)
 JOIN core.task_target tt ON (tt.workspace_id,tt.task_id,tt.target_id)=(s.workspace_id,s.task_id,s.target_id)
 LEFT JOIN core.retry r ON r.workspace_id=s.workspace_id AND r.subtask_id=s.id AND r.state='scheduled'
 LEFT JOIN core.execution p ON (p.workspace_id,p.id)=(r.workspace_id,r.previous_execution_id)
 WHERE s.workspace_id=$1 AND s.task_id=$2 AND s.id=$3 AND s.target_id=$4
   AND s.kind='ensure_membership'
   AND s.state IN ('QUEUED','RETRY_WAIT')
   AND s.current_execution_id IS NULL
   AND s.next_run_at<=now()
   AND (r.id IS NULL OR r.due_at<=now())
   AND NOT EXISTS(SELECT 1 FROM core.goal_slot gs WHERE gs.workspace_id=s.workspace_id AND gs.task_id=s.task_id AND gs.target_id=s.target_id AND gs.state='fulfilled')
   AND NOT EXISTS(SELECT 1 FROM core.result res WHERE res.workspace_id=s.workspace_id AND res.task_id=s.task_id AND res.target_id=s.target_id AND res.valid)
   AND NOT EXISTS(SELECT 1 FROM core.execution e WHERE e.workspace_id=s.workspace_id AND e.subtask_id=s.id
        AND (e.state IN ('DISPATCHING','UNKNOWN') OR e.effect_certainty IN ('unknown','confirmed_submitted')))`,
		workspace, taskID, subtaskID, targetID).Scan(&c.subtaskID, &c.targetID, &c.targetExternalID,
		&c.eligibility, &c.baselineCovered, &c.targetDisabled,
		&c.retryID, &c.retryStrategy, &c.retryPinnedAcctID)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, problem(schedCodeNoWork)
	}
	if err != nil {
		return c, err
	}
	return c, nil
}

// ensureTargetClaim guarantees this subtask owns the single global claim for the
// target/action family. A claim held by another task is never stolen; a resolved
// claim may be re-acquired because it no longer represents an active effect. At
// most one claim is acquired per caller transaction.
func ensureTargetClaim(ctx context.Context, tx pgx.Tx, workspace, taskID string, cand *reserveCandidate) (bool, error) {
	var claimTask, claimSubtask, claimState string
	err := tx.QueryRow(ctx, `SELECT task_id,subtask_id,state FROM core.target_claim
 WHERE workspace_id=$1 AND target_id=$2 AND action_family=$3`, workspace, cand.targetID, schedActionFamily).
		Scan(&claimTask, &claimSubtask, &claimState)
	switch {
	case err == nil:
		if claimState == "active" {
			return claimTask == taskID && claimSubtask == cand.subtaskID, nil
		}
		// A resolved claim stays blocked while a possibly effective execution for
		// the target is still unresolved: the guard trigger would reject it anyway.
		var unresolved bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.execution
 WHERE workspace_id=$1 AND target_id=$2
   AND (state IN ('DISPATCHING','UNKNOWN') OR effect_certainty IN ('unknown','confirmed_submitted')))`,
			workspace, cand.targetID).Scan(&unresolved); err != nil {
			return false, err
		}
		if unresolved {
			return false, nil
		}
		tag, err := tx.Exec(ctx, `UPDATE core.target_claim SET task_id=$3,subtask_id=$4,state='active',
 acquired_at=now(),resolved_at=NULL
 WHERE workspace_id=$1 AND target_id=$2 AND action_family=$5 AND state<>'active'`,
			workspace, cand.targetID, taskID, cand.subtaskID, schedActionFamily)
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() == 1, nil
	case errors.Is(err, pgx.ErrNoRows):
		tag, err := tx.Exec(ctx, `INSERT INTO core.target_claim(workspace_id,target_id,action_family,task_id,subtask_id,state,acquired_at)
 VALUES($1,$2,$3,$4,$5,'active',now())
 ON CONFLICT (workspace_id,target_id,action_family) DO NOTHING`,
			workspace, cand.targetID, schedActionFamily, taskID, cand.subtaskID)
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() == 1, nil
	default:
		return false, err
	}
}

// selectReserveAccount locks the in-scope accounts in ascending id order, filters
// them by active identity, readiness, capability, baseline proof and approval
// scope, then picks the least-used account (preferred account first when usable).
//
// The first read is only used to distinguish "no eligible account" from "no
// proven capability" and to skip needless locking. The authoritative decision is
// made after locking the accounts, in ascending id order, so concurrent
// reservations sharing accounts serialize without a lock cycle.
func selectReserveAccount(ctx context.Context, tx pgx.Tx, workspace, taskID string, cand *reserveCandidate, approval *reserveApproval, preferred, boundAccountID string, dbNow time.Time) (reserveAccount, error) {
	const accountSelect = `SELECT a.id,a.runtime_id,a.owner_epoch,a.quota_limit,a.identity_status,a.connection_state,
 a.login_state,a.control_state,a.risk_state,a.request_join_verified,a.next_action_at,
 (rc.account_id IS NOT NULL),(ba.account_id IS NOT NULL)
 FROM core.task_account ta
 JOIN core.account a ON (a.workspace_id,a.id)=(ta.workspace_id,ta.account_id)
 LEFT JOIN core.runtime_credentials rc ON rc.workspace_id=a.workspace_id AND rc.runtime_id=a.runtime_id AND rc.account_id=a.id AND rc.owner_epoch=a.owner_epoch AND rc.active
 LEFT JOIN core.baseline_account ba ON ba.workspace_id=ta.workspace_id AND ba.task_id=ta.task_id AND ba.account_id=ta.account_id AND ba.complete
 WHERE ta.workspace_id=$1 AND ta.task_id=$2
   AND NOT EXISTS(SELECT 1 FROM core.baseline b WHERE b.workspace_id=ta.workspace_id AND b.task_id=ta.task_id
         AND b.account_id=ta.account_id AND b.target_external_id=$3)
   AND NOT EXISTS(SELECT 1 FROM core.account_action_lock l WHERE l.workspace_id=a.workspace_id
         AND l.account_id=a.id AND l.action_family=$4 AND l.held)`

	rows, err := tx.Query(ctx, accountSelect+` ORDER BY a.id`, workspace, taskID, cand.targetExternalID, schedActionFamily)
	if err != nil {
		return reserveAccount{}, err
	}
	var (
		eligible   []reserveAccount
		blockedCap bool
	)
	for rows.Next() {
		var a reserveAccount
		if err := rows.Scan(&a.id, &a.runtimeID, &a.ownerEpoch, &a.quotaLimit, &a.identityStatus, &a.connectionState,
			&a.loginState, &a.controlState, &a.riskState, &a.requestJoinVerified, &a.nextActionAt,
			&a.runtimeBound, &a.baselineComplete); err != nil {
			rows.Close()
			return reserveAccount{}, err
		}
		if !a.ready() {
			continue
		}
		if !a.requestJoinVerified {
			// An unproven capability is never assumed: this account is blocked,
			// not merely busy.
			blockedCap = true
			continue
		}
		if a.nextActionAt.After(dbNow) {
			continue
		}
		if boundAccountID != "" && a.id != boundAccountID {
			continue
		}
		if accountScopedByApproval(a.id, cand, approval) {
			eligible = append(eligible, a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return reserveAccount{}, err
	}
	if len(eligible) == 0 {
		if blockedCap {
			return reserveAccount{}, problem(schedCodeCapabilityUnavail)
		}
		return reserveAccount{}, problem(schedCodeNoCapacity)
	}

	ids := make([]string, len(eligible))
	for i := range eligible {
		ids[i] = eligible[i].id
	}
	var locked []reserveAccount
	lrows, err := tx.Query(ctx, accountSelect+` AND a.id=ANY($5::text[]) ORDER BY a.id FOR UPDATE OF a`,
		workspace, taskID, cand.targetExternalID, schedActionFamily, ids)
	if err != nil {
		return reserveAccount{}, err
	}
	for lrows.Next() {
		var a reserveAccount
		if err := lrows.Scan(&a.id, &a.runtimeID, &a.ownerEpoch, &a.quotaLimit, &a.identityStatus, &a.connectionState,
			&a.loginState, &a.controlState, &a.riskState, &a.requestJoinVerified, &a.nextActionAt,
			&a.runtimeBound, &a.baselineComplete); err != nil {
			lrows.Close()
			return reserveAccount{}, err
		}
		if a.ready() && a.requestJoinVerified && !a.nextActionAt.After(dbNow) &&
			(boundAccountID == "" || a.id == boundAccountID) && accountScopedByApproval(a.id, cand, approval) {
			locked = append(locked, a)
		}
	}
	lrows.Close()
	if err := lrows.Err(); err != nil {
		return reserveAccount{}, err
	}
	if len(locked) == 0 {
		return reserveAccount{}, problem(schedCodeNoCapacity)
	}

	lockedIDs := make([]string, len(locked))
	for i := range locked {
		lockedIDs[i] = locked[i].id
	}
	usage := make(map[string][2]int, len(lockedIDs))
	urows, err := tx.Query(ctx, `SELECT account_id,reserved,consumed FROM core.account_usage
 WHERE workspace_id=$1 AND action_family=$2 AND window_start=current_date AND account_id=ANY($3::text[])
 ORDER BY account_id FOR UPDATE`, workspace, schedActionFamily, lockedIDs)
	if err != nil {
		return reserveAccount{}, err
	}
	for urows.Next() {
		var id string
		var reserved, consumed int
		if err := urows.Scan(&id, &reserved, &consumed); err != nil {
			urows.Close()
			return reserveAccount{}, err
		}
		usage[id] = [2]int{reserved, consumed}
	}
	urows.Close()
	if err := urows.Err(); err != nil {
		return reserveAccount{}, err
	}

	var usable []reserveAccount
	for _, a := range locked {
		a.reserved = usage[a.id][0]
		a.consumed = usage[a.id][1]
		if a.reserved+a.consumed >= a.quotaLimit {
			continue
		}
		usable = append(usable, a)
	}
	if len(usable) == 0 {
		return reserveAccount{}, problem(schedCodeLocalBudget)
	}
	sort.SliceStable(usable, func(i, j int) bool {
		if (usable[i].id == preferred) != (usable[j].id == preferred) {
			return usable[i].id == preferred
		}
		if usable[i].active() != usable[j].active() {
			return usable[i].active() < usable[j].active()
		}
		if !usable[i].nextActionAt.Equal(usable[j].nextActionAt) {
			return usable[i].nextActionAt.Before(usable[j].nextActionAt)
		}
		return usable[i].id < usable[j].id
	})
	return usable[0], nil
}

// accountScopedByApproval rechecks approval account/target scope and a
// same_account retry pin against one account. It is evaluated before and after
// the account row lock.
func accountScopedByApproval(accountID string, cand *reserveCandidate, approval *reserveApproval) bool {
	if cand.retryStrategy != nil && *cand.retryStrategy == "same_account" {
		if cand.retryPinnedAcctID == nil || *cand.retryPinnedAcctID != accountID {
			return false
		}
	}
	if approval == nil {
		return false
	}
	return contains(approval.accountIDs, accountID) && contains(approval.targetIDs, cand.targetExternalID)
}

type reserveSubtaskRow struct {
	attemptCount int
	maxAttempts  int
	fence        int64
}

// lockReserveSubtask locks the chosen subtask and rechecks that it is still
// dispatchable. A pending/external or already claimed subtask is never claimed
// twice.
func lockReserveSubtask(ctx context.Context, tx pgx.Tx, workspace string, cand *reserveCandidate) (reserveSubtaskRow, error) {
	var row reserveSubtaskRow
	var state string
	var dueNow, executionCleared bool
	err := tx.QueryRow(ctx, `SELECT state,attempt_count,max_attempts,fence_token,next_run_at<=now(),
 current_execution_id IS NULL
 FROM core.subtask WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, cand.subtaskID).
		Scan(&state, &row.attemptCount, &row.maxAttempts, &row.fence, &dueNow, &executionCleared)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, problem(schedCodeNoWork)
	}
	if err != nil {
		return row, err
	}
	if (state != "QUEUED" && state != "RETRY_WAIT") || !dueNow || !executionCleared {
		return row, problem(schedCodeNoWork)
	}
	if row.attemptCount >= row.maxAttempts {
		return row, problem(schedCodeLocalBudget)
	}
	return row, nil
}

// lockConsumeReservationRetry locks the scheduled retry (if any) and validates
// that it is due and that a same_account retry is honored. The caller marks it
// consumed in the same transaction after the execution row exists. The attempt
// counters are never reset here.
func lockConsumeReservationRetry(ctx context.Context, tx pgx.Tx, workspace string, cand *reserveCandidate, accountID string, dbNow time.Time) (bool, error) {
	if cand.retryID == nil {
		return false, nil
	}
	var strategy string
	var dueAt time.Time
	var previousAccount *string
	err := tx.QueryRow(ctx, `SELECT r.strategy,r.due_at,p.account_id
 FROM core.retry r
 LEFT JOIN core.execution p ON (p.workspace_id,p.id)=(r.workspace_id,r.previous_execution_id)
 WHERE r.workspace_id=$1 AND r.id=$2 AND r.state='scheduled' FOR UPDATE OF r`,
		workspace, *cand.retryID).Scan(&strategy, &dueAt, &previousAccount)
	if errors.Is(err, pgx.ErrNoRows) {
		// Consumed/cancelled concurrently: the candidate is stale.
		return false, problem(schedCodeNoWork)
	}
	if err != nil {
		return false, err
	}
	if dueAt.After(dbNow) {
		return false, problem(schedCodeNoWork)
	}
	if strategy == "same_account" && (previousAccount == nil || *previousAccount != accountID) {
		return false, problem(schedCodeNoCapacity)
	}
	return true, nil
}

// reserveGoalSlot reuses the target's already reserved slot (retry) and otherwise
// locks the lowest free slot. Fulfilled targets never produce another attempt.
func reserveGoalSlot(ctx context.Context, tx pgx.Tx, workspace, taskID, targetID string) (int, bool, error) {
	var slotNo int
	var state string
	err := tx.QueryRow(ctx, `SELECT slot_no,state FROM core.goal_slot
 WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state IN ('reserved','fulfilled')
 FOR UPDATE`, workspace, taskID, targetID).Scan(&slotNo, &state)
	switch {
	case err == nil:
		if state == "fulfilled" {
			return 0, false, problem(schedCodeNoWork)
		}
		return slotNo, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, false, err
	}
	err = tx.QueryRow(ctx, `SELECT slot_no FROM core.goal_slot
 WHERE workspace_id=$1 AND task_id=$2 AND state='free'
 ORDER BY slot_no LIMIT 1 FOR UPDATE`, workspace, taskID).Scan(&slotNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, problem(schedCodeNoCapacity)
	}
	if err != nil {
		return 0, false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE core.goal_slot SET state='reserved',target_id=$4,reserved_at=now()
 WHERE workspace_id=$1 AND task_id=$2 AND slot_no=$3 AND state='free'
 AND NOT EXISTS(SELECT 1 FROM core.goal_slot gs WHERE gs.workspace_id=$1 AND gs.task_id=$2
       AND gs.target_id=$4 AND gs.state IN ('reserved','fulfilled'))`,
		workspace, taskID, slotNo, targetID)
	if err != nil {
		return 0, false, err
	}
	if tag.RowsAffected() != 1 {
		return 0, false, problem(schedCodeNoCapacity)
	}
	return slotNo, false, nil
}

// reserveAccountUsage atomically reserves one unit of the account's shared
// action window. The quota is global to the account across tasks and attempts.
func reserveAccountUsage(ctx context.Context, tx pgx.Tx, workspace string, account reserveAccount) error {
	var reserved, consumed int
	err := tx.QueryRow(ctx, `INSERT INTO core.account_usage(workspace_id,account_id,action_family,window_start,reserved,consumed)
 VALUES($1,$2,$3,current_date,1,0)
 ON CONFLICT (workspace_id,account_id,action_family,window_start)
 DO UPDATE SET reserved=core.account_usage.reserved+1
 WHERE core.account_usage.reserved+core.account_usage.consumed<$4
 RETURNING reserved,consumed`, workspace, account.id, schedActionFamily, account.quotaLimit).Scan(&reserved, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return problem(schedCodeLocalBudget)
	}
	return err
}
