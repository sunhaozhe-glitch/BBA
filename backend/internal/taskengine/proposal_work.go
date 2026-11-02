package taskengine

// Proposal-driven execution: explicit task orchestration mode, the materialize
// command, the durable work-item/attempt ledger and the worker claim fence.
//
// Boundaries this file keeps:
//   * The legacy ApplyPlanProposal stays intent only. Materialize is a separate,
//     explicitly gated command that turns an approved proposal into durable work
//     items for a proposal-mode task. Nothing here calls an adapter, the model
//     gateway, ARTEX or QQ.
//   * A task is expanded by exactly one mechanism. Legacy Reserve refuses a
//     proposal-mode task; the proposal consumer claims only proposal-mode tasks.
//   * The worker never gets a second sender: after a claim it calls the existing
//     Reserve/AcceptOperation/IssuePermit/Dispatch chain, so the unified gate,
//     permit, owner-epoch and bridge fencing still decide the send.
//   * authorization_revision is separate from task version. A work item records
//     the revision it was materialized under, and the reserve transaction refuses
//     a work item whose authorization changed after materialization.
//   * A leased claim is written through its lease_epoch fence. An expired lease is
//     released only when the item has no execution; anything that may have reached
//     the bridge stays WAITING_RESULT and is resolved by the existing UNKNOWN path.

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oapi-codegen/nullable"

	"bba.local/taskengine/internal/contract"
)

const (
	workItemCodeLeaseLost = "WORK_ITEM_LEASE_LOST"

	orchestrationModeLegacy   = "legacy"
	orchestrationModeProposal = "proposal"

	workItemStateBlocked       = "BLOCKED"
	workItemStateReady         = "READY"
	workItemStateClaimed       = "CLAIMED"
	workItemStateWaitingResult = "WAITING_RESULT"
	workItemStateSucceeded     = "SUCCEEDED"
	workItemStateFailed        = "FAILED"
	workItemStateUnknown       = "UNKNOWN"
	workItemStateCancelled     = "CANCELLED"

	workItemKindRequestJoin = "request_join"

	workItemReasonDependencyPending    = "dependency_pending"
	workItemReasonDependencyFailed     = "dependency_failed"
	workItemReasonTaskCancelled        = "task_cancelled"
	workItemReasonAuthorizationInvalid = "authorization_invalid"
	workItemReasonDeadlineExpired      = "deadline_expired"
	workItemReasonBudgetExhausted      = "budget_exhausted"
	workItemReasonLeaseExpired         = "lease_expired"

	workItemMinLease = 10 * time.Millisecond
	workItemMaxLease = 5 * time.Minute
)

var workItemDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// OrchestrationModeRequest switches the expansion mechanism of one quiescent
// task. It never widens scope: the task, approval and limits stay unchanged.
type OrchestrationModeRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Mode            string `json:"mode"`
	Reason          string `json:"reason"`
}

// GraphProposalCandidate is one in-scope task target proposed from a visible
// graph snapshot. The graph sync slice supplies candidates from its own snapshot
// tables; taskengine re-validates every one against task targets, eligibility,
// baseline, account readiness and approval scope, so a candidate can never widen
// the authorised scope. ExternalID is optional and must match the target row.
type GraphProposalCandidate struct {
	TargetID   string
	ExternalID string
}

// GraphSnapshotValidator is the seam to the graph sync slice. It must prove that
// snapshotID names a completed visible-graph snapshot for this exact task inside
// the caller's transaction. The default is unregistered: a proposal that cites a
// snapshot is refused with CAPABILITY_UNAVAILABLE until S2 registers the real
// validator, rather than trusting an unverifiable identifier.
type GraphSnapshotValidator func(ctx context.Context, tx pgx.Tx, workspaceID, taskID, snapshotID string) error

type GraphSnapshotWorkItemValidator func(ctx context.Context, tx pgx.Tx, workspaceID, taskID, snapshotID, proposalID, materializationID, workItemID string) error

var (
	graphSnapshotMu                sync.RWMutex
	graphSnapshotValidator         GraphSnapshotValidator
	graphSnapshotWorkItemValidator GraphSnapshotWorkItemValidator
)

// RegisterGraphSnapshotValidator installs the S2 snapshot proof. It is called
// once from server wiring; a nil validator restores the safe default.
func RegisterGraphSnapshotValidator(v GraphSnapshotValidator) {
	graphSnapshotMu.Lock()
	defer graphSnapshotMu.Unlock()
	graphSnapshotValidator = v
}

func RegisterGraphSnapshotWorkItemValidator(v GraphSnapshotWorkItemValidator) {
	graphSnapshotMu.Lock()
	defer graphSnapshotMu.Unlock()
	graphSnapshotWorkItemValidator = v
}

// currentGraphSnapshotValidator exposes the installed validator to tests so a
// test double can be restored without dropping the real registration.
func currentGraphSnapshotValidator() GraphSnapshotValidator {
	graphSnapshotMu.RLock()
	defer graphSnapshotMu.RUnlock()
	return graphSnapshotValidator
}

func currentGraphSnapshotWorkItemValidator() GraphSnapshotWorkItemValidator {
	graphSnapshotMu.RLock()
	defer graphSnapshotMu.RUnlock()
	return graphSnapshotWorkItemValidator
}

func validateGraphSnapshot(ctx context.Context, tx pgx.Tx, workspaceID, taskID, snapshotID string) error {
	if snapshotID == "" {
		return nil
	}
	if !taskValidID(snapshotID) {
		return problem(taskCodeValidation)
	}
	graphSnapshotMu.RLock()
	v := graphSnapshotValidator
	graphSnapshotMu.RUnlock()
	if v == nil {
		return problem(taskCodeCapabilityUnavailable)
	}
	return v(ctx, tx, workspaceID, taskID, snapshotID)
}

func validateGraphSnapshotForWorkItem(ctx context.Context, tx pgx.Tx, workspaceID, taskID, snapshotID, proposalID, materializationID, workItemID string) error {
	if snapshotID == "" {
		return nil
	}
	if !taskValidID(snapshotID) || !taskValidID(proposalID) || !taskValidID(materializationID) || !taskValidID(workItemID) {
		return problem(taskCodeValidation)
	}
	graphSnapshotMu.RLock()
	v := graphSnapshotWorkItemValidator
	graphSnapshotMu.RUnlock()
	if v == nil {
		return problem(taskCodeCapabilityUnavailable)
	}
	return v(ctx, tx, workspaceID, taskID, snapshotID, proposalID, materializationID, workItemID)
}

// planSnapshotStateError folds the graph-sync snapshot lifecycle codes into the
// task state vocabulary at proposal/materialize/reserve boundaries. The staleness
// is a task/snapshot state conflict (409), not a transport failure; other codes
// such as RESOURCE_NOT_FOUND pass through unchanged.
func planSnapshotStateError(err error) error {
	switch Code(err) {
	case "GRAPH_SYNC_SNAPSHOT_STALE", "GRAPH_SYNC_SNAPSHOT_EXPIRED":
		return problem(taskCodeStateConflict)
	}
	return err
}

// CreatePlanProposalFromSnapshot is the proposal generator for graph-derived
// plans. With no candidates it behaves exactly like CreatePlanProposal. With
// candidates it emits up to twenty ordered steps (visible target order) and
// stores graph_snapshot_id on the proposal for audit.
func (s *Store) CreatePlanProposalFromSnapshot(ctx context.Context, actor Actor, taskID, key string, request contract.PlanProposalRequest, snapshotID string, candidates []GraphProposalCandidate) (contract.Receipt, error) {
	if !taskValidID(snapshotID) {
		return contract.Receipt{}, problem(taskCodeValidation)
	}
	if fromRequest := planProposalRequestSnapshot(request); fromRequest != "" && fromRequest != snapshotID {
		return contract.Receipt{}, problem(taskCodeValidation)
	}
	return s.createPlanProposal(ctx, actor, taskID, key, request, planProposalCreateOptions{
		SnapshotID: snapshotID,
		Candidates: candidates,
	})
}

// planProposalStepsForCreate resolves proposal steps. Candidate targets are
// resolved to task targets by id; the account is selected by the existing live
// selector, and each step passes the same validator approval uses. Steps are
// ordered as supplied by the source; materialization stores that order as the
// dependency chain.
func (s *Store) planProposalStepsForCreate(ctx context.Context, tx pgx.Tx, task TaskRow, opts planProposalCreateOptions) ([]contract.PlanStep, error) {
	if len(opts.Candidates) == 0 {
		targetID, groupID, err := planProposalCandidateTarget(ctx, tx, task.WorkspaceID, task.ID)
		if err != nil {
			return nil, err
		}
		accountID, err := planProposalCandidateAccount(ctx, tx, task.WorkspaceID, task.ID, groupID)
		if err != nil {
			return nil, err
		}
		return []contract.PlanStep{newPlanProposalStep(1, targetID, accountID, groupID)}, nil
	}
	if len(opts.Candidates) > 20 {
		return nil, problem(taskCodeValidation)
	}
	seen := make(map[string]bool, len(opts.Candidates))
	steps := make([]contract.PlanStep, 0, len(opts.Candidates))
	for _, candidate := range opts.Candidates {
		if !taskValidID(candidate.TargetID) || seen[candidate.TargetID] {
			return nil, problem(taskCodeStateConflict)
		}
		seen[candidate.TargetID] = true
		var groupID string
		err := tx.QueryRow(ctx, `
			SELECT t.external_id
			  FROM core.task_target tt
			  JOIN core.target t ON (t.workspace_id,t.id)=(tt.workspace_id,tt.target_id)
			 WHERE tt.workspace_id=$1 AND tt.task_id=$2 AND tt.target_id=$3`,
			task.WorkspaceID, task.ID, candidate.TargetID).Scan(&groupID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(taskCodeStateConflict)
		}
		if err != nil {
			return nil, err
		}
		if candidate.ExternalID != "" && candidate.ExternalID != groupID {
			return nil, problem(taskCodeStateConflict)
		}
		accountID, err := planProposalCandidateAccount(ctx, tx, task.WorkspaceID, task.ID, groupID)
		if err != nil {
			return nil, err
		}
		step := newPlanProposalStep(len(steps)+1, candidate.TargetID, accountID, groupID)
		if err := planProposalValidateStep(ctx, tx, task, step); err != nil {
			return nil, err
		}
		// The source order is the dependency chain: step N waits for every
		// earlier candidate. The list is explicit so it is auditable in the
		// stored proposal instead of being re-inferred at materialization.
		deps := make([]int, 0, len(steps))
		for n := 1; n <= len(steps); n++ {
			deps = append(deps, n)
		}
		step.DependsOn = &deps
		steps = append(steps, step)
	}
	if len(steps) == 0 {
		return nil, problem(taskCodeStateConflict)
	}
	return steps, nil
}

func newPlanProposalStep(stepNo int, targetID, accountID, groupID string) contract.PlanStep {
	var step contract.PlanStep
	step.StepNo = stepNo
	step.Action = contract.Action(planProposalAction)
	step.AccountId.Set(accountID)
	step.TargetId.Set(targetID)
	step.Parameters.GroupId.Set(groupID)
	step.Parameters.Keyword = nullable.NewNullNullable[string]()
	step.Parameters.Limit = nullable.NewNullNullable[int]()
	step.Parameters.Note = nullable.NewNullNullable[string]()
	step.Parameters.Text = nullable.NewNullNullable[string]()
	step.Parameters.VerificationMessage = nullable.NewNullNullable[string]()
	return step
}

// MaterializeRequest is the explicit execution-plan command. The digest and
// approval reference bind the exact proposal the operator reviewed.
type MaterializeRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	ApprovalRef     string `json:"approval_ref"`
	ProposalDigest  string `json:"proposal_digest"`
}

// WorkItemView is the operator read projection of one durable work item. It
// joins the proposal -> application -> attempt/execution -> observation chain by
// identifiers; a missing identifier is shown as null rather than inferred.
type WorkItemView struct {
	Id                   contract.Id          `json:"id"`
	TaskId               contract.Id          `json:"task_id"`
	MaterializationId    contract.Id          `json:"materialization_id"`
	ApplicationId        contract.Id          `json:"application_id"`
	ProposalId           contract.Id          `json:"proposal_id"`
	StepId               string               `json:"step_id"`
	StepNo               int                  `json:"step_no"`
	Kind                 string               `json:"kind"`
	State                string               `json:"state"`
	BlockedReason        *string              `json:"blocked_reason"`
	DependsOn            []string             `json:"depends_on"`
	AccountId            contract.Id          `json:"account_id"`
	TargetId             contract.Id          `json:"target_id"`
	AuthorizationVersion int                  `json:"authorization_version"`
	ExecutionId          *string              `json:"execution_id"`
	AttemptCount         int                  `json:"attempt_count"`
	MaxAttempts          int                  `json:"max_attempts"`
	LastError            *string              `json:"last_error"`
	CreatedAt            contract.UTCDateTime `json:"created_at"`
	UpdatedAt            contract.UTCDateTime `json:"updated_at"`
	// Attempts is the exact attempt -> execution -> operation/observation
	// trace, oldest first. It is read from core.work_item_attempt, never
	// inferred from "same account and group".
	Attempts []WorkItemAttemptView `json:"attempts"`
}

// WorkItemAttemptView is one durable attempt of a work item. ExecutionID and
// OperationID identify the exact send; ObservationEventID is set only when a
// trusted observation settled the attempt. Empty identifiers are null.
type WorkItemAttemptView struct {
	AttemptNo          int                   `json:"attempt_no"`
	LeaseEpoch         int64                 `json:"lease_epoch"`
	ExecutionId        *string               `json:"execution_id"`
	OperationId        *string               `json:"operation_id"`
	ExecutionState     *string               `json:"execution_state"`
	Outcome            *string               `json:"outcome"`
	ErrorCode          *string               `json:"error_code"`
	EvidenceRef        *string               `json:"evidence_ref"`
	ObservationEventId *string               `json:"observation_event_id"`
	StartedAt          contract.UTCDateTime  `json:"started_at"`
	FinishedAt         *contract.UTCDateTime `json:"finished_at"`
}

// WorkItemClaim is the exclusive lease returned by ClaimReadyWorkItem. Every
// later write must present the same LeaseOwner and LeaseEpoch.
type WorkItemClaim struct {
	WorkspaceID          string
	TaskID               string
	WorkItemID           string
	StepID               string
	LeaseOwner           string
	LeaseEpoch           int64
	LeaseUntil           time.Time
	AccountID            string
	TargetID             string
	MaterializationID    string
	ApplicationID        string
	AuthorizationVersion int
}

func orchestrationPath(taskID string) string { return "/v1/tasks/" + taskID + "/orchestration" }

func materializePath(taskID, proposalID string) string {
	return planProposalActionPath(taskID, proposalID, "materialize")
}

// SetOrchestrationMode performs the explicit mode switch. It requires an
// expected version, refuses a runtime actor, refuses a task with active local
// work, bumps authorization_revision (not merely version) and writes the audit
// event and outbox row in the same transaction.
func (s *Store) SetOrchestrationMode(ctx context.Context, actor Actor, taskID, key string, request OrchestrationModeRequest) (contract.Receipt, error) {
	if err := actor.require(taskPermControl); err != nil {
		return contract.Receipt{}, err
	}
	if actor.RuntimeID != "" {
		return contract.Receipt{}, problem(taskCodeForbidden)
	}
	if err := taskCheckIdempotencyKey(key); err != nil {
		return contract.Receipt{}, err
	}
	if !taskValidID(taskID) || request.ExpectedVersion < 1 ||
		(request.Mode != orchestrationModeLegacy && request.Mode != orchestrationModeProposal) ||
		taskBlank(request.Reason) || len([]rune(request.Reason)) > 500 {
		return contract.Receipt{}, problem(taskCodeValidation)
	}
	bodyDigest, err := digest(request)
	if err != nil {
		return contract.Receipt{}, err
	}
	path := orchestrationPath(taskID)
	var out contract.Receipt
	err = s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, taskPermControl); err != nil {
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
			return json.Unmarshal(rec.response, &out)
		}
		if request.ExpectedVersion != task.Version {
			return problem(taskCodeVersionConflict)
		}
		var acceptedAt time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&acceptedAt); err != nil {
			return err
		}
		if request.Mode == task.OrchestrationMode {
			out = orchestrationReceipt(taskID, request.ExpectedVersion, acceptedAt)
			return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, taskID, out)
		}
		if err := taskModeSwitchAllowed(ctx, tx, task, request.Mode); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE core.task SET orchestration_mode=$3, version=version+1, authorization_revision=authorization_revision+1
			WHERE workspace_id=$1 AND id=$2 AND version=$4`,
			actor.WorkspaceID, taskID, request.Mode, task.Version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return problem(taskCodeVersionConflict)
		}
		if err := appendEvent(ctx, tx, actor.WorkspaceID, taskID, "task.orchestration_mode_changed", actor.PrincipalID, map[string]any{
			"from":                        task.OrchestrationMode,
			"to":                          request.Mode,
			"reason":                      request.Reason,
			"expected_version":            request.ExpectedVersion,
			"from_authorization_revision": task.AuthorizationRevision,
			"to_authorization_revision":   task.AuthorizationRevision + 1,
			"external_action":             false,
		}); err != nil {
			return err
		}
		out = orchestrationReceipt(taskID, request.ExpectedVersion, acceptedAt)
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, taskID, out)
	})
	if err != nil {
		return contract.Receipt{}, err
	}
	return out, nil
}

func orchestrationReceipt(taskID string, expectedVersion int, at time.Time) contract.Receipt {
	return contract.Receipt{
		AcceptedAt:      contract.UTCDateTime(at.UTC()),
		Action:          contract.ReceiptAction("set_orchestration_mode"),
		ExpectedVersion: expectedVersion,
		Id:              newID("receipt"),
		ResourceId:      contract.Id(taskID),
		State:           contract.Accepted,
	}
}

// taskModeSwitchAllowed refuses a mode change while any durable work may still
// become an external effect. It is evaluated under the task lock and reads only
// committed state; an in-process adapter call is additionally counted through
// activeLocalDispatches, and the legacy local flight count covers DISPATCHING and
// issued permits.
func taskModeSwitchAllowed(ctx context.Context, tx pgx.Tx, task TaskRow, target string) error {
	flights, err := taskLocalFlightCount(ctx, tx, task.WorkspaceID, task.ID)
	if err != nil {
		return err
	}
	if flights != 0 {
		return problem(taskCodeStateConflict)
	}
	var blocked bool
	err = tx.QueryRow(ctx, `
		SELECT
		  EXISTS(SELECT 1 FROM core.execution
		    WHERE workspace_id=$1 AND task_id=$2 AND state IN ('CREATED','ACCEPTED','DISPATCHING'))
		  OR EXISTS(SELECT 1 FROM core.subtask
		    WHERE workspace_id=$1 AND task_id=$2
		      AND state IN ('CLAIMED','RUNNING','WAITING_EXTERNAL','RECONCILING'))
		  OR EXISTS(SELECT 1 FROM core.work_item
		    WHERE workspace_id=$1 AND task_id=$2 AND state IN ('CLAIMED','WAITING_RESULT'))
		  OR EXISTS(SELECT 1 FROM core.retry
		    WHERE workspace_id=$1 AND task_id=$2 AND state='scheduled')`,
		task.WorkspaceID, task.ID).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return problem(taskCodeStateConflict)
	}
	if target == orchestrationModeLegacy {
		var pending bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM core.work_item
			  WHERE workspace_id=$1 AND task_id=$2 AND state IN ('BLOCKED','READY'))`,
			task.WorkspaceID, task.ID).Scan(&pending); err != nil {
			return err
		}
		if pending {
			// Returning the task to the legacy expander while materialized work
			// is still queued would allow two mechanisms to expand one task.
			return problem(taskCodeStateConflict)
		}
	}
	return nil
}

// MaterializePlanProposal is the explicit, gated execution-plan command. It
// validates the approved proposal, the live approval, the task mode/version,
// authorization revision, deadline, budget and step scope, then writes the
// materialization, one application row and one work item per approved step in a
// single transaction. It never reserves a slot, claim, permit or execution.
func (s *Store) MaterializePlanProposal(ctx context.Context, actor Actor, taskID, proposalID, key string, request MaterializeRequest) (contract.Receipt, error) {
	if err := actor.require(taskPermControl); err != nil {
		return contract.Receipt{}, err
	}
	if actor.RuntimeID != "" {
		return contract.Receipt{}, problem(taskCodeForbidden)
	}
	if err := taskCheckIdempotencyKey(key); err != nil {
		return contract.Receipt{}, err
	}
	if !taskValidID(taskID) || !taskValidID(proposalID) ||
		request.ExpectedVersion < 1 || !taskValidID(request.ApprovalRef) ||
		!workItemDigestPattern.MatchString(request.ProposalDigest) {
		return contract.Receipt{}, problem(taskCodeValidation)
	}
	bodyDigest, err := digest(request)
	if err != nil {
		return contract.Receipt{}, err
	}
	path := materializePath(taskID, proposalID)
	var out contract.Receipt
	err = s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, taskPermControl); err != nil {
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
			return json.Unmarshal(rec.response, &out)
		}
		// A repeat under a new key returns the durable materialization instead of
		// expanding the same approved proposal twice.
		if existing, ok, err := materializationForProposal(ctx, tx, actor.WorkspaceID, proposalID, taskID); err != nil {
			return err
		} else if ok {
			out = materializationReceipt(existing)
			return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, existing.id, out)
		}
		if request.ExpectedVersion != task.Version {
			return problem(taskCodeVersionConflict)
		}
		if task.OrchestrationMode != orchestrationModeProposal {
			return problem(taskCodeStateConflict)
		}
		if task.State != "RUNNING" {
			return problem(taskCodeStateConflict)
		}
		if task.Mode != "live" {
			return problem(taskCodeStateConflict)
		}
		proposal, err := planProposalLoad(ctx, tx, actor.WorkspaceID, taskID, proposalID, true)
		if err != nil {
			return err
		}
		if proposal.ExpectedTaskVersion != task.Version || proposal.ScopeRevision != task.ScopeRevision {
			return problem(taskCodeVersionConflict)
		}
		if proposal.State != "approved" {
			return problem(taskCodeStateConflict)
		}
		if proposal.Digest != request.ProposalDigest || proposal.ApprovalID != request.ApprovalRef ||
			task.ApprovalID == nil || *task.ApprovalID != proposal.ApprovalID {
			return problem(taskCodeStateConflict)
		}
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
			return err
		}
		if !proposal.ExpiresAt.After(dbNow) || !task.Deadline.After(dbNow) {
			return problem(taskCodeStateConflict)
		}
		if err := validateGraphSnapshot(ctx, tx, task.WorkspaceID, task.ID, proposal.GraphSnapshotID); err != nil {
			return planSnapshotStateError(err)
		}
		steps, err := planProposalSteps(proposal)
		if err != nil {
			return err
		}
		for _, step := range steps {
			if err := planProposalValidateStep(ctx, tx, task, step); err != nil {
				return problem(taskCodeStateConflict)
			}
			if err := planProposalValidateApproval(ctx, tx, task, proposal, proposal.ApprovalID, step); err != nil {
				return err
			}
		}
		if err := planProposalBudgetOpen(ctx, tx, task); err != nil {
			return problem(taskCodeStateConflict)
		}
		approval, err := taskLoadApproval(ctx, tx, task.WorkspaceID, proposal.ApprovalID)
		if err != nil {
			return err
		}
		maxAttempts := task.MaxAttempts
		if approval.MaxAttempts < maxAttempts {
			maxAttempts = approval.MaxAttempts
		}
		if maxAttempts > 10 {
			maxAttempts = 10
		}
		if maxAttempts < 1 {
			return problem(taskCodeStateConflict)
		}

		materializationID := newID("materialization")
		if _, err := tx.Exec(ctx, `
			INSERT INTO core.work_materialization(
			 workspace_id,id,task_id,proposal_id,proposal_digest,approval_id,task_version,
			 authorization_revision,materialized_by,materialized_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			task.WorkspaceID, materializationID, task.ID, proposal.ID, proposal.Digest,
			proposal.ApprovalID, task.Version, task.AuthorizationRevision, actor.PrincipalID, dbNow); err != nil {
			return err
		}
		receiptID := newID("receipt")
		stepIDs := make([]string, 0, len(steps))
		workItemIDs := make([]string, 0, len(steps))
		for i, step := range steps {
			applicationID := newID("planapp")
			if _, err := tx.Exec(ctx, `
				INSERT INTO core.plan_application(
				 workspace_id,id,task_id,proposal_id,proposal_digest,task_version,scope_revision,
				 approval_id,step_no,action,account_id,target_id,group_external_id,receipt_id,applied_by,applied_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
				task.WorkspaceID, applicationID, task.ID, proposal.ID, proposal.Digest, task.Version,
				task.ScopeRevision, proposal.ApprovalID, step.StepNo, string(step.Action),
				step.AccountId.MustGet(), step.TargetId.MustGet(), step.Parameters.GroupId.MustGet(),
				newID("receipt"), actor.PrincipalID, dbNow); err != nil {
				return err
			}
			stepID := applicationID + "/" + itoa(step.StepNo)
			stepIDs = append(stepIDs, stepID)
			if err := ensureWorkSubtask(ctx, tx, task, step, maxAttempts); err != nil {
				return err
			}
			scopeDigest, err := workItemScopeDigest(task, materializationID, applicationID, stepID,
				step.AccountId.MustGet(), step.TargetId.MustGet(), step.Parameters.GroupId.MustGet(), proposal.ApprovalID)
			if err != nil {
				return err
			}
			depStepNos, err := planProposalStepDependencies(step)
			if err != nil {
				return err
			}
			dependsOn := make([]string, 0, len(depStepNos))
			for _, stepNo := range depStepNos {
				// planProposalSteps already proved 1 <= stepNo < current step and a
				// dense 1..N sequence, so this can never invent a dependency.
				if stepNo < 1 || stepNo > i {
					return problem(taskCodeStateConflict)
				}
				dependsOn = append(dependsOn, stepIDs[stepNo-1])
			}
			dependsJSON, err := json.Marshal(dependsOn)
			if err != nil {
				return err
			}
			state := workItemStateReady
			var blockedReason any
			if len(dependsOn) > 0 {
				state = workItemStateBlocked
				blockedReason = workItemReasonDependencyPending
			}
			workItemID := newID("work")
			if _, err := tx.Exec(ctx, `
				INSERT INTO core.work_item(
				 workspace_id,id,task_id,materialization_id,application_id,proposal_id,step_id,step_no,
				 kind,account_id,target_id,scope_digest,authorization_version,state,blocked_reason,depends_on,
				 lease_epoch,attempt_count,created_at,updated_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,0,0,$17,$17)`,
				task.WorkspaceID, workItemID, task.ID, materializationID, applicationID, proposal.ID,
				stepID, step.StepNo, workItemKindRequestJoin, step.AccountId.MustGet(), step.TargetId.MustGet(),
				scopeDigest, task.AuthorizationRevision, state, blockedReason, dependsJSON, dbNow); err != nil {
				return err
			}
			workItemIDs = append(workItemIDs, workItemID)
		}
		updated, err := tx.Exec(ctx, `
			UPDATE core.plan_proposal SET state='applied',applied_by=$4,applied_at=$5
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='approved'`,
			task.WorkspaceID, task.ID, proposal.ID, actor.PrincipalID, dbNow)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return problem(taskCodeStateConflict)
		}
		versioned, err := tx.Exec(ctx, `
			UPDATE core.task SET version=version+1 WHERE workspace_id=$1 AND id=$2 AND version=$3`,
			task.WorkspaceID, task.ID, task.Version)
		if err != nil {
			return err
		}
		if versioned.RowsAffected() != 1 {
			return problem(taskCodeVersionConflict)
		}
		if err := appendEvent(ctx, tx, task.WorkspaceID, task.ID, "plan.proposal.materialized", actor.PrincipalID, map[string]any{
			"materialization_id":     materializationID,
			"proposal_id":            proposal.ID,
			"proposal_digest":        proposal.Digest,
			"approval_ref":           proposal.ApprovalID,
			"task_version":           task.Version,
			"authorization_revision": task.AuthorizationRevision,
			"application_ids":        applicationIDsFromSteps(stepIDs),
			"work_item_ids":          workItemIDs,
			"steps":                  len(steps),
			"external_action":        false,
			"dispatch_invoked":       false,
		}); err != nil {
			return err
		}
		out = contract.Receipt{
			AcceptedAt:      contract.UTCDateTime(dbNow.UTC()),
			Action:          contract.ReceiptAction("materialize"),
			ExpectedVersion: request.ExpectedVersion,
			Id:              receiptID,
			ResourceId:      contract.Id(materializationID),
			State:           contract.Accepted,
		}
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, materializationID, out)
	})
	if err != nil {
		return contract.Receipt{}, err
	}
	return out, nil
}

// materializedRow is the durable result of one materialize command.
type materializedRow struct {
	id           string
	acceptedAt   time.Time
	expectedTask int
}

func materializationForProposal(ctx context.Context, tx pgx.Tx, workspace, proposalID, taskID string) (materializedRow, bool, error) {
	var row materializedRow
	err := tx.QueryRow(ctx, `
		SELECT id,materialized_at,task_version FROM core.work_materialization
		WHERE workspace_id=$1 AND proposal_id=$2 AND task_id=$3`,
		workspace, proposalID, taskID).Scan(&row.id, &row.acceptedAt, &row.expectedTask)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	return row, true, nil
}

func materializationReceipt(row materializedRow) contract.Receipt {
	return contract.Receipt{
		AcceptedAt:      contract.UTCDateTime(row.acceptedAt.UTC()),
		Action:          contract.ReceiptAction("materialize"),
		ExpectedVersion: row.expectedTask,
		Id:              newID("receipt"),
		ResourceId:      contract.Id(row.id),
		State:           contract.Accepted,
	}
}

// ensureWorkSubtask reuses the import-created queued subtask for the approved
// target or creates one. It never reopens an attempt-bearing or terminal subtask.
func ensureWorkSubtask(ctx context.Context, tx pgx.Tx, task TaskRow, step contract.PlanStep, maxAttempts int) error {
	targetID := step.TargetId.MustGet()
	var subtaskID, state string
	var attemptCount int
	var currentExecution *string
	err := tx.QueryRow(ctx, `
		SELECT id,state,attempt_count,current_execution_id FROM core.subtask
		WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND kind='ensure_membership'
		FOR UPDATE`, task.WorkspaceID, task.ID, targetID).Scan(&subtaskID, &state, &attemptCount, &currentExecution)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO core.subtask(workspace_id,id,task_id,target_id,kind,state,max_attempts)
			VALUES($1,$2,$3,$4,'ensure_membership','QUEUED',$5)`,
			task.WorkspaceID, newID("subtask"), task.ID, targetID, maxAttempts)
		return err
	case err != nil:
		return err
	}
	if state != "QUEUED" || attemptCount != 0 || currentExecution != nil {
		return problem(taskCodeStateConflict)
	}
	var executions int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND subtask_id=$2`,
		task.WorkspaceID, subtaskID).Scan(&executions); err != nil {
		return err
	}
	if executions != 0 {
		return problem(taskCodeStateConflict)
	}
	return nil
}

func workItemScopeDigest(task TaskRow, materializationID, applicationID, stepID, accountID, targetID, groupID, approvalID string) (string, error) {
	return digest(map[string]any{
		"workspace_id":           task.WorkspaceID,
		"task_id":                task.ID,
		"materialization_id":     materializationID,
		"application_id":         applicationID,
		"step_id":                stepID,
		"kind":                   workItemKindRequestJoin,
		"account_id":             accountID,
		"target_id":              targetID,
		"group_id":               groupID,
		"approval_id":            approvalID,
		"scope_revision":         task.ScopeRevision,
		"authorization_revision": task.AuthorizationRevision,
	})
}

func applicationIDsFromSteps(stepIDs []string) []string {
	out := make([]string, 0, len(stepIDs))
	for _, id := range stepIDs {
		if idx := strings.IndexByte(id, '/'); idx > 0 {
			out = append(out, id[:idx])
		}
	}
	return out
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// claims and reservation

// ClaimReadyWorkItem takes exactly one READY work item of a running proposal
// task whose dependencies succeeded, using a short transaction with FOR UPDATE
// SKIP LOCKED. A concurrent worker skips the row.
func (s *Store) ClaimReadyWorkItem(ctx context.Context, workerID string, ttl time.Duration) (*WorkItemClaim, error) {
	return s.claimReadyWorkItem(ctx, workerID, ttl, "", "", []string{})
}
func (s *Store) claimReadyWorkItem(ctx context.Context, workerID string, ttl time.Duration, workspace, taskID string, excluded []string) (*WorkItemClaim, error) {
	if taskBlank(workerID) || len(workerID) > 96 || ttl < workItemMinLease || ttl > workItemMaxLease {
		return nil, problem(taskCodeValidation)
	}
	var claim WorkItemClaim
	found := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			WITH candidate AS (
			 SELECT wi.workspace_id,wi.id
			   FROM core.work_item wi
			   JOIN core.task t ON (t.workspace_id,t.id)=(wi.workspace_id,wi.task_id)
			  WHERE wi.state='READY'
 AND ($3::text='' OR wi.workspace_id=$3) AND ($4::text='' OR wi.task_id=$4)
 AND NOT(wi.id=ANY($5::text[]))
			    AND t.orchestration_mode='proposal'
			    AND t.state='RUNNING'
			    AND t.deadline_at>clock_timestamp()
			    AND NOT EXISTS(
			      SELECT 1 FROM jsonb_array_elements_text(wi.depends_on) AS dep(step_id)
			      JOIN core.work_item dw
			        ON dw.workspace_id=wi.workspace_id AND dw.task_id=wi.task_id AND dw.step_id=dep.step_id
			      WHERE dw.state<>'SUCCEEDED')
			  ORDER BY wi.created_at,wi.id
			  FOR UPDATE OF wi SKIP LOCKED
			  LIMIT 1)
			UPDATE core.work_item w
			   SET state='CLAIMED',lease_owner=$1,lease_epoch=w.lease_epoch+1,
			       lease_until=clock_timestamp()+make_interval(secs=>$2),updated_at=clock_timestamp()
			  FROM candidate
			 WHERE w.workspace_id=candidate.workspace_id AND w.id=candidate.id
			 RETURNING w.workspace_id,w.task_id,w.id,w.step_id,w.lease_owner,w.lease_epoch,w.lease_until,
			           w.account_id,w.target_id,w.materialization_id,w.application_id,w.authorization_version`,
			workerID, ttl.Seconds(), workspace, taskID, excluded).Scan(&claim.WorkspaceID, &claim.TaskID, &claim.WorkItemID, &claim.StepID,
			&claim.LeaseOwner, &claim.LeaseEpoch, &claim.LeaseUntil, &claim.AccountID, &claim.TargetID,
			&claim.MaterializationID, &claim.ApplicationID, &claim.AuthorizationVersion)
		if errors.Is(row, pgx.ErrNoRows) {
			return nil
		}
		if err := row; err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &claim, nil
}

// ReleaseWorkItemClaim returns a claim to READY (no effect, retry later) or
// moves it to CANCELLED (no effect and no safe retry). The write is fenced by the
// claim's lease_epoch; a stale owner is rejected.
func (s *Store) ReleaseWorkItemClaim(ctx context.Context, claim WorkItemClaim, nextState, code string) error {
	if !taskValidID(claim.WorkItemID) || !taskValidID(claim.TaskID) ||
		nextState != workItemStateReady && nextState != workItemStateCancelled ||
		taskBlank(code) || len(code) > 128 {
		return problem(taskCodeValidation)
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, claim.WorkspaceID, claim.TaskID)
		if err != nil {
			return err
		}
		return s.releaseWorkItemLocked(ctx, tx, task, claim, nextState, code)
	})
}

func (s *Store) releaseWorkItemLocked(ctx context.Context, tx pgx.Tx, task TaskRow, claim WorkItemClaim, nextState, code string) error {
	var state, owner string
	var epoch int64
	err := tx.QueryRow(ctx, `
		SELECT state,COALESCE(lease_owner,''),lease_epoch FROM core.work_item
		WHERE workspace_id=$1 AND id=$2 AND task_id=$3 FOR UPDATE`,
		claim.WorkspaceID, claim.WorkItemID, claim.TaskID).Scan(&state, &owner, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return problem(workItemCodeLeaseLost)
	}
	if err != nil {
		return err
	}
	if state != workItemStateClaimed || owner != claim.LeaseOwner || epoch != claim.LeaseEpoch {
		return problem(workItemCodeLeaseLost)
	}
	var blockedReason any
	if nextState == workItemStateCancelled {
		blockedReason = code
	}
	tag, err := tx.Exec(ctx, `
		UPDATE core.work_item
		   SET state=$4,lease_owner=NULL,lease_until=NULL,blocked_reason=$5,last_error=$6,updated_at=clock_timestamp()
		 WHERE workspace_id=$1 AND id=$2 AND state='CLAIMED' AND lease_owner=$3 AND lease_epoch=$7`,
		claim.WorkspaceID, claim.WorkItemID, claim.LeaseOwner, nextState, blockedReason, code, claim.LeaseEpoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return problem(workItemCodeLeaseLost)
	}
	return appendEvent(ctx, tx, task.WorkspaceID, task.ID, "work.item.released", claim.LeaseOwner, map[string]any{
		"work_item_id": claim.WorkItemID,
		"state":        nextState,
		"code":         code,
		"lease_epoch":  claim.LeaseEpoch,
	})
}

// SweepExpiredWorkItemClaims returns expired CLAIMED items that never produced an
// execution to READY. A claimed item cannot carry an execution (the reserve
// transaction commits the WAITING_RESULT transition together with it), so an
// expired claim is always provably effect free.
func (s *Store) SweepExpiredWorkItemClaims(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, problem(taskCodeValidation)
	}
	type ref struct{ workspace, taskID, workItemID string }
	var refs []ref
	released := 0
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT workspace_id,task_id,id FROM core.work_item
			WHERE state='CLAIMED' AND execution_id IS NULL AND lease_until<=clock_timestamp()
			ORDER BY workspace_id,task_id,lease_until,id
			LIMIT $1`, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item ref
			if err := rows.Scan(&item.workspace, &item.taskID, &item.workItemID); err != nil {
				rows.Close()
				return err
			}
			refs = append(refs, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		lastTask := ""
		var task TaskRow
		for _, item := range refs {
			if item.workspace+"/"+item.taskID != lastTask {
				task, err = lockTask(ctx, tx, item.workspace, item.taskID)
				if err != nil {
					return err
				}
				lastTask = item.workspace + "/" + item.taskID
			}
			tag, err := tx.Exec(ctx, `
				UPDATE core.work_item
				   SET state='READY',lease_owner=NULL,lease_until=NULL,last_error=$3,updated_at=clock_timestamp()
				 WHERE workspace_id=$1 AND id=$2 AND state='CLAIMED' AND execution_id IS NULL
				   AND lease_until<=clock_timestamp()`,
				item.workspace, item.workItemID, workItemReasonLeaseExpired)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				continue
			}
			released++
			if err := appendEvent(ctx, tx, task.WorkspaceID, task.ID, "work.item.released", "worker_sweep", map[string]any{
				"work_item_id": item.workItemID,
				"state":        workItemStateReady,
				"code":         workItemReasonLeaseExpired,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return released, nil
}

// ReserveWorkItem reserves exactly the approved step behind a claimed work item,
// through the existing scheduler gates. The task, work item and attempt are
// locked in the fixed order task -> work item -> target/claim -> account/usage ->
// subtask/execution/slot. A refusal before any execution is produced releases the
// claim to READY (retryable) or CANCELLED (no safe retry).
func (s *Store) ReserveWorkItem(ctx context.Context, claim WorkItemClaim) (Reservation, error) {
	var reservation Reservation
	var refusal error
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, claim.WorkspaceID, claim.TaskID)
		if err != nil {
			return err
		}
		var state, owner, proposalID, materializationID, applicationID, scopeDigest string
		var epoch int64
		var authorizationVersion, attemptCount int
		var leaseExpired bool
		var executionID *string
		err = tx.QueryRow(ctx, `
			SELECT state,COALESCE(lease_owner,''),lease_epoch,proposal_id,materialization_id,application_id,
			       scope_digest,authorization_version,attempt_count,execution_id,COALESCE(lease_until<=clock_timestamp(),true)
			  FROM core.work_item WHERE workspace_id=$1 AND id=$2 AND task_id=$3 FOR UPDATE`,
			claim.WorkspaceID, claim.WorkItemID, claim.TaskID).Scan(&state, &owner, &epoch, &proposalID,
			&materializationID, &applicationID, &scopeDigest, &authorizationVersion, &attemptCount, &executionID, &leaseExpired)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(workItemCodeLeaseLost)
		}
		if err != nil {
			return err
		}
		if state != workItemStateClaimed || owner != claim.LeaseOwner || epoch != claim.LeaseEpoch || executionID != nil || leaseExpired {
			return problem(workItemCodeLeaseLost)
		}
		effectiveClaim := claim
		effectiveClaim.LeaseEpoch = epoch
		effectiveClaim.AccountID = claim.AccountID
		effectiveClaim.TargetID = claim.TargetID

		refuse := func(nextState, code string, cause error) error {
			refusal = cause
			return s.releaseWorkItemLocked(ctx, tx, task, effectiveClaim, nextState, code)
		}
		if task.OrchestrationMode != orchestrationModeProposal {
			return refuse(workItemStateCancelled, workItemReasonAuthorizationInvalid, problem(taskCodeStateConflict))
		}
		if authorizationVersion != task.AuthorizationRevision {
			return refuse(workItemStateCancelled, workItemReasonAuthorizationInvalid, problem(taskCodeStateConflict))
		}
		switch task.State {
		case "RUNNING":
		case "CANCELLED", "SUCCEEDED", "PARTIAL", "FAILED":
			return refuse(workItemStateCancelled, workItemReasonTaskCancelled, problem(taskCodeStateConflict))
		default:
			// PAUSED/PAUSING/BLOCKED etc: no effect happened, retry after resume.
			return refuse(workItemStateReady, "task_not_running", problem(schedCodeNoWork))
		}
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
			return err
		}
		if !task.Deadline.After(dbNow) {
			return refuse(workItemStateCancelled, workItemReasonDeadlineExpired, problem(schedCodeNoWork))
		}
		var proposalExpires time.Time
		var snapshotID string
		if err := tx.QueryRow(ctx, `SELECT expires_at,COALESCE(graph_snapshot_id,'') FROM core.plan_proposal
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3`, claim.WorkspaceID, claim.TaskID, proposalID).
			Scan(&proposalExpires, &snapshotID); err != nil {
			return err
		}
		if !proposalExpires.After(dbNow) {
			return refuse(workItemStateCancelled, workItemReasonAuthorizationInvalid, problem(taskCodeStateConflict))
		}
		if err := validateGraphSnapshotForWorkItem(ctx, tx, claim.WorkspaceID, claim.TaskID, snapshotID, proposalID, materializationID, claim.WorkItemID); err != nil {
			return refuse(workItemStateCancelled, workItemReasonAuthorizationInvalid, planSnapshotStateError(err))
		}
		if _, err := loadReserveApproval(ctx, tx, task); err != nil {
			return refuse(workItemStateCancelled, workItemReasonAuthorizationInvalid, err)
		}
		_ = applicationID
		_ = scopeDigest
		subtaskID, err := subtaskForWorkItem(ctx, tx, task, claim, effectiveClaim)
		if err != nil {
			if Code(err) == schedCodeNoWork {
				return refuse(workItemStateReady, schedCodeNoWork, err)
			}
			return err
		}
		reserved, err := s.reserveLocked(ctx, tx, task, reserveRequest{
			BoundSubtaskID: subtaskID,
			BoundTargetID:  claim.TargetID,
			BoundAccountID: claim.AccountID,
		})
		if err != nil {
			switch Code(err) {
			case schedCodeNoCapacity, schedCodeCapabilityUnavail:
				return refuse(workItemStateReady, Code(err), err)
			case schedCodeLocalBudget:
				return refuse(workItemStateCancelled, workItemReasonBudgetExhausted, err)
			case schedCodeNoWork:
				return refuse(workItemStateReady, Code(err), err)
			default:
				return err
			}
		}
		attemptNo := attemptCount + 1
		if _, err := tx.Exec(ctx, `
			INSERT INTO core.work_item_attempt(workspace_id,id,work_item_id,attempt_no,lease_epoch,execution_id)
			VALUES($1,$2,$3,$4,$5,$6)`,
			claim.WorkspaceID, newID("attempt"), claim.WorkItemID, attemptNo, epoch, reserved.ExecutionID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE core.work_item
			   SET state='WAITING_RESULT',execution_id=$3,attempt_count=$4,lease_owner=NULL,lease_until=NULL,
			       last_error=NULL,updated_at=clock_timestamp()
			 WHERE workspace_id=$1 AND id=$2 AND state='CLAIMED' AND lease_epoch=$5`,
			claim.WorkspaceID, claim.WorkItemID, reserved.ExecutionID, attemptNo, epoch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return problem(workItemCodeLeaseLost)
		}
		if err := appendEvent(ctx, tx, task.WorkspaceID, task.ID, "work.item.reserved", claim.LeaseOwner, map[string]any{
			"work_item_id":    claim.WorkItemID,
			"attempt_no":      attemptNo,
			"execution_id":    reserved.ExecutionID,
			"operation_id":    reserved.OperationID,
			"account_id":      reserved.AccountID,
			"target_id":       reserved.TargetID,
			"lease_epoch":     epoch,
			"external_action": false,
		}); err != nil {
			return err
		}
		reservation = reserved
		return nil
	})
	if err != nil {
		return Reservation{}, err
	}
	if refusal != nil {
		return Reservation{}, refusal
	}
	return reservation, nil
}

// subtaskForWorkItem resolves the kernel subtask behind the approved step and
// verifies it still matches the work item's exact account/target binding.
func subtaskForWorkItem(ctx context.Context, tx pgx.Tx, task TaskRow, claim, effective WorkItemClaim) (string, error) {
	var subtaskID, targetID string
	err := tx.QueryRow(ctx, `
		SELECT s.id,s.target_id FROM core.subtask s
		JOIN core.plan_application a
		  ON a.workspace_id=$1 AND a.id=$3 AND a.target_id=s.target_id AND a.task_id=s.task_id
		WHERE s.workspace_id=$1 AND s.task_id=$2 AND s.target_id=$4
		  AND s.kind='ensure_membership'`,
		claim.WorkspaceID, claim.TaskID, claim.ApplicationID, claim.TargetID).Scan(&subtaskID, &targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", problem(schedCodeNoWork)
	}
	if err != nil {
		return "", err
	}
	if targetID != claim.TargetID {
		return "", problem(schedCodeNoWork)
	}
	return subtaskID, nil
}

// ---------------------------------------------------------------------------
// trusted observation projection

// projectWorkItemExecution maps one execution outcome onto its exact
// work_item/attempt chain. It is called inside the transaction that already holds
// the task lock and has persisted the observation or bridge outcome. It never
// reopens a terminal work item and never resends anything.
func (s *Store) projectWorkItemExecution(ctx context.Context, tx pgx.Tx, task TaskRow, executionID, source, eventID string) error {
	if executionID == "" {
		return nil
	}
	var workItemID, stepID string
	var attemptNo int
	err := tx.QueryRow(ctx, `
		SELECT a.work_item_id,a.attempt_no,w.step_id
		  FROM core.work_item_attempt a
		  JOIN core.work_item w ON (w.workspace_id,w.id)=(a.workspace_id,a.work_item_id)
		 WHERE a.workspace_id=$1 AND a.execution_id=$2`, task.WorkspaceID, executionID).Scan(&workItemID, &attemptNo, &stepID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	derived, lastError, evidence, err := deriveWorkItemState(ctx, tx, task.WorkspaceID, executionID)
	if err != nil {
		return err
	}
	var state string
	var currentExecution *string
	err = tx.QueryRow(ctx, `
		SELECT state,execution_id FROM core.work_item
		WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, task.WorkspaceID, workItemID).Scan(&state, &currentExecution)
	if err != nil {
		return err
	}
	if terminalWorkItemState(state) {
		if derived != state && source == "observation" {
			return appendEvent(ctx, tx, task.WorkspaceID, task.ID, "work.item.late_observation", "runtime", map[string]any{
				"work_item_id":      workItemID,
				"execution_id":      executionID,
				"observation_event": eventID,
				"state":             state,
				"late_outcome":      derived,
				"reopened":          false,
			})
		}
		return nil
	}
	if derived == state {
		// Still observed or already in exactly this state: nothing to append.
		return nil
	}
	if !terminalWorkItemState(derived) {
		// A submitted/pending receipt is not a work item outcome yet; the
		// attempt stays open until a terminal state arrives.
		return nil
	}
	var evidenceArg any
	if evidence != "" {
		evidenceArg = evidence
	}
	var eventArg any
	if eventID != "" {
		eventArg = eventID
	}
	if _, err := tx.Exec(ctx, `
		UPDATE core.work_item_attempt
		   SET outcome=$3,evidence_ref=$4,observation_event_id=$5,error_code=$6,finished_at=clock_timestamp()
		 WHERE workspace_id=$1 AND work_item_id=$2 AND attempt_no=$7 AND outcome IS NULL`,
		task.WorkspaceID, workItemID, derived, evidenceArg, eventArg, nullableString(lastError), attemptNo); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE core.work_item
		   SET state=$3,last_error=$4,updated_at=clock_timestamp()
		 WHERE workspace_id=$1 AND id=$2 AND state IN ('WAITING_RESULT','CLAIMED')`,
		task.WorkspaceID, workItemID, derived, nullableString(lastError))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := appendEvent(ctx, tx, task.WorkspaceID, task.ID, "work.item.state", "runtime", map[string]any{
		"work_item_id":      workItemID,
		"execution_id":      executionID,
		"observation_event": eventID,
		"state":             derived,
		"source":            source,
		"attempt_no":        attemptNo,
	}); err != nil {
		return err
	}
	switch derived {
	case workItemStateSucceeded:
		return promoteDependentWorkItems(ctx, tx, task, stepID)
	case workItemStateFailed, workItemStateUnknown:
		return blockDependentWorkItems(ctx, tx, task, stepID)
	}
	return nil
}

func terminalWorkItemState(state string) bool {
	switch state {
	case workItemStateSucceeded, workItemStateFailed, workItemStateUnknown, workItemStateCancelled:
		return true
	}
	return false
}

// deriveWorkItemState maps durable execution truth (and a credited result) onto
// the work item vocabulary. A submitted/pending receipt is not success.
func deriveWorkItemState(ctx context.Context, tx pgx.Tx, workspace, executionID string) (string, string, string, error) {
	var state, effect string
	var disposition, evidence *string
	var hasResult bool
	err := tx.QueryRow(ctx, `
		SELECT e.state,e.effect_certainty,e.disposition,e.evidence_ref,
		       EXISTS(SELECT 1 FROM core.result r WHERE r.workspace_id=$1 AND r.execution_id=$2 AND r.valid)
		  FROM core.execution e WHERE e.workspace_id=$1 AND e.id=$2`,
		workspace, executionID).Scan(&state, &effect, &disposition, &evidence, &hasResult)
	if err != nil {
		return "", "", "", err
	}
	evidenceRef := ""
	if evidence != nil {
		evidenceRef = *evidence
	}
	dispositionValue := ""
	if disposition != nil {
		dispositionValue = *disposition
	}
	switch state {
	case "UNKNOWN":
		return workItemStateUnknown, "EXTERNAL_OUTCOME_UNKNOWN", evidenceRef, nil
	case "REJECTED_BEFORE_SEND":
		return workItemStateFailed, "REJECTED_BEFORE_SEND", evidenceRef, nil
	case "OBSERVED":
		if hasResult {
			return workItemStateSucceeded, "", evidenceRef, nil
		}
		switch dispositionValue {
		case "rejected":
			return workItemStateFailed, "JOIN_REJECTED", evidenceRef, nil
		case "failed_no_effect":
			return workItemStateFailed, "NO_EFFECT", evidenceRef, nil
		default:
			// confirmed/submitted only proves the request reached the group;
			// success needs a credited membership result or observation.
			return workItemStateWaitingResult, "", evidenceRef, nil
		}
	default:
		return workItemStateWaitingResult, "", evidenceRef, nil
	}
}

// promoteDependentWorkItems makes BLOCKED dependents of a succeeded step READY
// when every dependency has succeeded.
func promoteDependentWorkItems(ctx context.Context, tx pgx.Tx, task TaskRow, stepID string) error {
	rows, err := tx.Query(ctx, `
		UPDATE core.work_item w
		   SET state='READY',blocked_reason=NULL,updated_at=clock_timestamp()
		 WHERE w.workspace_id=$1 AND w.task_id=$2 AND w.state='BLOCKED'
		   AND w.depends_on ? $3
		   AND NOT EXISTS(
		     SELECT 1 FROM jsonb_array_elements_text(w.depends_on) AS dep(step_id)
		     JOIN core.work_item dw
		       ON dw.workspace_id=w.workspace_id AND dw.task_id=w.task_id AND dw.step_id=dep.step_id
		     WHERE dw.state<>'SUCCEEDED')
		 RETURNING w.id`, task.WorkspaceID, task.ID, stepID)
	if err != nil {
		return err
	}
	var promoted []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		promoted = append(promoted, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(promoted) == 0 {
		return nil
	}
	return appendEvent(ctx, tx, task.WorkspaceID, task.ID, "work.item.dependencies_ready", "runtime", map[string]any{
		"step_id":       stepID,
		"work_item_ids": promoted,
	})
}

// blockDependentWorkItems marks still-blocked dependents of a failed/unknown step
// so they can never become READY.
func blockDependentWorkItems(ctx context.Context, tx pgx.Tx, task TaskRow, stepID string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE core.work_item w
		   SET blocked_reason=$4,updated_at=clock_timestamp()
		 WHERE w.workspace_id=$1 AND w.task_id=$2 AND w.state='BLOCKED'
		   AND w.depends_on ? $3 AND w.blocked_reason<>$4`,
		task.WorkspaceID, task.ID, stepID, workItemReasonDependencyFailed)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return appendEvent(ctx, tx, task.WorkspaceID, task.ID, "work.item.dependencies_blocked", "runtime", map[string]any{
		"step_id": stepID,
		"reason":  workItemReasonDependencyFailed,
		"count":   tag.RowsAffected(),
	})
}

// cancelWorkItemsUnderTask marks every work item that provably has no external
// effect CANCELLED. Items already waiting for a result stay observed.
func (s *Store) cancelWorkItemsUnderTask(ctx context.Context, tx pgx.Tx, task TaskRow) error {
	rows, err := tx.Query(ctx, `
		UPDATE core.work_item
		   SET state='CANCELLED',blocked_reason=$3,lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp()
		 WHERE workspace_id=$1 AND task_id=$2 AND state IN ('BLOCKED','READY','CLAIMED') AND execution_id IS NULL
		 RETURNING id`, task.WorkspaceID, task.ID, workItemReasonTaskCancelled)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return appendEvent(ctx, tx, task.WorkspaceID, task.ID, "work.item.cancelled", "operator", map[string]any{
		"work_item_ids": ids,
		"reason":        workItemReasonTaskCancelled,
	})
}

// ReconcileWorkItemStates re-projects work items whose execution already has a
// durable terminal (or still-observed) outcome. The observation and bridge paths
// project inline; this is the bounded recovery sweep for a crash between the two.
func (s *Store) ReconcileWorkItemStates(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, problem(taskCodeValidation)
	}
	type ref struct{ workspace, taskID, executionID string }
	var refs []ref
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT wi.workspace_id,wi.task_id,wi.execution_id
			  FROM core.work_item wi
			 WHERE wi.state='WAITING_RESULT' AND wi.execution_id IS NOT NULL
			 ORDER BY wi.workspace_id,wi.task_id,wi.created_at,wi.id
			 LIMIT $1`, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item ref
			if err := rows.Scan(&item.workspace, &item.taskID, &item.executionID); err != nil {
				rows.Close()
				return err
			}
			refs = append(refs, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		lastTask := ""
		var task TaskRow
		for _, item := range refs {
			if item.workspace+"/"+item.taskID != lastTask {
				task, err = lockTask(ctx, tx, item.workspace, item.taskID)
				if err != nil {
					return err
				}
				lastTask = item.workspace + "/" + item.taskID
			}
			if err := s.projectWorkItemExecution(ctx, tx, task, item.executionID, "reconcile", ""); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(refs), nil
}

// ---------------------------------------------------------------------------
// read projection and internal helpers

// ListWorkItems returns the durable work ledger of one task in creation order.
func (s *Store) ListWorkItems(ctx context.Context, actor Actor, taskID, cursor string, limit int) ([]WorkItemView, string, bool, error) {
	if err := actor.require("task.read"); err != nil {
		return nil, "", false, err
	}
	if actor.RuntimeID != "" {
		return nil, "", false, problem(taskCodeForbidden)
	}
	if !taskValidID(taskID) {
		return nil, "", false, problem(taskCodeNotFound)
	}
	if limit < 1 || limit > 100 {
		return nil, "", false, problem(taskCodeValidation)
	}
	parts, err := p019DecodeCursor(cursor, 2)
	if err != nil {
		return nil, "", false, err
	}
	var after time.Time
	if len(parts) == 2 {
		micros, err := parseInt64(parts[0])
		if err != nil || !taskValidID(parts[1]) {
			return nil, "", false, problem(taskCodeCursorExpired)
		}
		after = time.UnixMicro(micros).UTC()
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, "", false, err
	}
	defer tx.Rollback(ctx)
	task, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false, problem(taskCodeNotFound)
	}
	if err != nil {
		return nil, "", false, err
	}
	if err := authorizeTask(ctx, tx, actor, task, "task.read"); err != nil {
		return nil, "", false, err
	}
	query := `
		SELECT wi.id,wi.task_id,wi.materialization_id,wi.application_id,wi.proposal_id,wi.step_id,wi.step_no,
		       wi.kind,wi.state,wi.blocked_reason,wi.depends_on::text,wi.account_id,wi.target_id,
		       wi.authorization_version,wi.execution_id,wi.attempt_count,
		       COALESCE((SELECT s.max_attempts FROM core.subtask s
		                  WHERE s.workspace_id=wi.workspace_id AND s.task_id=wi.task_id AND s.target_id=wi.target_id
		                  LIMIT 1),0),
		       wi.last_error,wi.created_at,wi.updated_at
		  FROM core.work_item wi
		 WHERE wi.workspace_id=$1 AND wi.task_id=$2`
	args := []any{actor.WorkspaceID, taskID}
	if len(parts) == 2 {
		query += ` AND (wi.created_at,wi.id)>($3,$4)`
		args = append(args, after, parts[1])
	}
	query += ` ORDER BY wi.created_at,wi.id LIMIT $` + itoa(len(args)+1)
	args = append(args, limit+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, "", false, err
	}
	items := make([]WorkItemView, 0, limit+1)
	for rows.Next() {
		var item WorkItemView
		var dependsJSON string
		var created, updated time.Time
		if err := rows.Scan(&item.Id, &item.TaskId, &item.MaterializationId, &item.ApplicationId, &item.ProposalId,
			&item.StepId, &item.StepNo, &item.Kind, &item.State, &item.BlockedReason, &dependsJSON,
			&item.AccountId, &item.TargetId, &item.AuthorizationVersion, &item.ExecutionId, &item.AttemptCount,
			&item.MaxAttempts, &item.LastError, &created, &updated); err != nil {
			rows.Close()
			return nil, "", false, err
		}
		item.DependsOn = []string{}
		if dependsJSON != "" && dependsJSON != "[]" {
			if err := json.Unmarshal([]byte(dependsJSON), &item.DependsOn); err != nil {
				rows.Close()
				return nil, "", false, problem(taskCodeStoreUnavailable)
			}
		}
		item.CreatedAt = contract.UTCDateTime(created.UTC())
		item.UpdatedAt = contract.UTCDateTime(updated.UTC())
		item.Attempts = []WorkItemAttemptView{}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", false, err
	}
	rows.Close()
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if len(items) > 0 {
		ids := make([]string, len(items))
		position := make(map[string]int, len(items))
		for i := range items {
			ids[i] = string(items[i].Id)
			position[ids[i]] = i
		}
		attemptRows, err := tx.Query(ctx, `
			SELECT a.work_item_id,a.attempt_no,a.lease_epoch,a.execution_id,e.operation_id,e.state,
			       a.outcome,a.error_code,a.evidence_ref,a.observation_event_id,a.started_at,a.finished_at
			  FROM core.work_item_attempt a
			  LEFT JOIN core.execution e ON (e.workspace_id,e.id)=(a.workspace_id,a.execution_id)
			 WHERE a.workspace_id=$1 AND a.work_item_id=ANY($2)
			 ORDER BY a.work_item_id,a.attempt_no`, actor.WorkspaceID, ids)
		if err != nil {
			return nil, "", false, err
		}
		for attemptRows.Next() {
			var workItemID string
			var attempt WorkItemAttemptView
			var started time.Time
			var finished *time.Time
			if err := attemptRows.Scan(&workItemID, &attempt.AttemptNo, &attempt.LeaseEpoch,
				&attempt.ExecutionId, &attempt.OperationId, &attempt.ExecutionState,
				&attempt.Outcome, &attempt.ErrorCode, &attempt.EvidenceRef,
				&attempt.ObservationEventId, &started, &finished); err != nil {
				attemptRows.Close()
				return nil, "", false, err
			}
			attempt.StartedAt = contract.UTCDateTime(started.UTC())
			if finished != nil {
				at := contract.UTCDateTime(finished.UTC())
				attempt.FinishedAt = &at
			}
			if idx, ok := position[workItemID]; ok {
				items[idx].Attempts = append(items[idx].Attempts, attempt)
			}
		}
		if err := attemptRows.Err(); err != nil {
			attemptRows.Close()
			return nil, "", false, err
		}
		attemptRows.Close()
	}
	next := ""
	if hasMore {
		last := items[len(items)-1]
		next = p019Cursor(formatInt64(last.CreatedAt.UnixMicro()), string(last.Id))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", false, err
	}
	return items, next, hasMore, nil
}

func parseInt64(value string) (int64, error) {
	var out int64
	if value == "" {
		return 0, problem(taskCodeCursorExpired)
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, problem(taskCodeCursorExpired)
		}
		out = out*10 + int64(char-'0')
	}
	return out, nil
}

func formatInt64(value int64) string {
	return itoa64(value)
}

func itoa64(value int64) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[i:])
}

var _ = pgconn.PgError{}
