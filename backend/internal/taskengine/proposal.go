package taskengine

// Bounded proposal lifecycle for the fixed controlled-participation template.
//
// Boundaries this file must keep:
//   - A proposal is generated from trusted task state only. It never calls a
//     model provider, search provider, adapter or bridge.
//   - Approval binds an existing core.approval row. It is not a send.
//   - Application writes the task-kernel ledger (core.plan_application, task
//     version, task event/outbox) and nothing that can reach QQ. Any external
//     action still needs the normal claim, permit and dispatch gate.
//   - Stale task versions, changed scope revisions, expired proposals, reused
//     idempotency keys with a different body and out-of-scope steps fail closed.

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"bba.local/taskengine/internal/contract"
)

const (
	planProposalTemplate = "controlled_participation_v1"
	planProposalTTL      = 15 * time.Minute
	planProposalPerm     = "task.control"
	planProposalApprove  = "task.approve"
	planProposalAction   = "request_join"
)

var planProposalDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// PlanProposalApprovalRequest is the operator approval body. It is deliberately
// local to the proposal surface until the public contract gets an approve route.
type PlanProposalApprovalRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	ApprovalRef     string `json:"approval_ref"`
}

// PlanProposalApprovalReceipt records the durable approval binding. It is not a
// QQ receipt and carries no dispatch claim.
type PlanProposalApprovalReceipt struct {
	AcceptedAt  contract.UTCDateTime `json:"accepted_at"`
	ApprovalRef contract.Id          `json:"approval_ref"`
	ProposalId  contract.Id          `json:"proposal_id"`
	State       string               `json:"state"`
}

// PlanProposalView is the read projection needed to approve/apply a proposal.
// The digest is not a capability: apply still re-checks task version, scope and
// approval. It only binds the exact proposal body the operator reviewed.
type PlanProposalView struct {
	ProposalId          contract.Id           `json:"proposal_id"`
	TaskId              contract.Id           `json:"task_id"`
	State               string                `json:"state"`
	Digest              contract.Digest       `json:"digest"`
	ExpectedTaskVersion int                   `json:"expected_task_version"`
	ScopeRevision       int                   `json:"scope_revision"`
	ExpiresAt           contract.UTCDateTime  `json:"expires_at"`
	Proposal            contract.PlanProposal `json:"proposal"`
	Basis               []PlanProposalBasis   `json:"basis"`
}

// PlanProposalBasis is the recorded provenance of one generated proposal: which
// live Summary version and which graph anchor the generation actually consulted,
// and the rule that selected them. It is reference data; the BBA ledger
// (execution, result, claim, permit) stays execution truth.
type PlanProposalBasis struct {
	AccountId            string               `json:"account_id"`
	GroupId              string               `json:"group_id"`
	TargetId             string               `json:"target_id"`
	AnchorId             *string              `json:"anchor_id"`
	SummaryId            *string              `json:"summary_id"`
	SummaryVersion       *int                 `json:"summary_version"`
	EvidenceKinds        []string             `json:"evidence_kinds"`
	SourceManifestSha256 *string              `json:"source_manifest_sha256"`
	SelectionReason      string               `json:"selection_reason"`
	CreatedAt            contract.UTCDateTime `json:"created_at"`
}

// planProposalBasisInput is what one generation pass consulted. The live-Summary
// read applies the same expiry, retention and generation-skew filters as the
// GroupContext read, so a basis row can never cite a version the runtime could
// not read back.
type planProposalBasisInput struct {
	AccountID string
	GroupID   string
	TargetID  string
	AnchorID  *string
	SummaryID *string
	Version   *int
	Evidence  []string
	Manifest  *string
	Reason    string
}

// The recorded reason states the selection rule only. A Summary never encodes a
// decision, and a statistic summary is not verified group semantic understanding.
const (
	planProposalBasisLiveSummary = "Newest live Summary version for this account/group at generation time; a later observation supersedes it on the next proposal."
	planProposalBasisNoSummary   = "No live Summary for this account/group at generation time; selection used task ledger truth only."
	planProposalBasisWithAnchor  = " Graph anchor for the selected target recorded."
	planProposalBasisNoAnchor    = " No graph anchor exists for the selected target."
)

func planProposalLiveBasis(ctx context.Context, tx pgx.Tx, workspace, taskID, accountID, groupID, targetID string, now time.Time) (planProposalBasisInput, error) {
	basis := planProposalBasisInput{AccountID: accountID, GroupID: groupID, TargetID: targetID}
	var anchorID *string
	err := tx.QueryRow(ctx, `SELECT anchor_id FROM core.task_graph_anchor
		WHERE workspace_id=$1 AND task_id=$2 AND anchor_kind='target' AND target_id=$3
		ORDER BY anchor_id LIMIT 1`, workspace, taskID, targetID).Scan(&anchorID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return basis, err
	}
	if err == nil {
		basis.AnchorID = anchorID
	}
	var summaryID, manifest string
	var version int
	var topicsJSON []byte
	err = tx.QueryRow(ctx, `SELECT summary_id,summary_version,source_manifest_sha256,topics FROM core.group_summary
		WHERE workspace_id=$1 AND account_id=$2 AND group_id=$3
		  AND expires_at>$4 AND window_start+make_interval(days=>retention_days)>$4
		  AND generated_at<=$5
		ORDER BY summary_version DESC LIMIT 1`, workspace, accountID, groupID, now, now.Add(summaryGeneratedSkew)).
		Scan(&summaryID, &version, &manifest, &topicsJSON)
	switch {
	case err == nil:
		var topics []storedSummaryTopic
		if err := json.Unmarshal(topicsJSON, &topics); err != nil {
			return basis, err
		}
		evidence := make([]string, 0, len(topics))
		for _, topic := range topics {
			evidence = append(evidence, topic.EvidenceKind)
		}
		basis.SummaryID, basis.Version, basis.Manifest, basis.Evidence = &summaryID, &version, &manifest, evidence
		basis.Reason = planProposalBasisLiveSummary
	case errors.Is(err, pgx.ErrNoRows):
		basis.Reason = planProposalBasisNoSummary
	default:
		return basis, err
	}
	if basis.AnchorID != nil {
		basis.Reason += planProposalBasisWithAnchor
	} else {
		basis.Reason += planProposalBasisNoAnchor
	}
	return basis, nil
}

// planProposalRecordBasis writes the single append-only provenance row for a
// newly created proposal. A replayed create returns the stored proposal and
// writes no additional row.
func planProposalRecordBasis(ctx context.Context, tx pgx.Tx, workspace, proposalID, taskID string, basis planProposalBasisInput, at time.Time) error {
	var evidenceJSON any
	if basis.Evidence != nil {
		payload, err := json.Marshal(basis.Evidence)
		if err != nil {
			return err
		}
		evidenceJSON = payload
	}
	_, err := tx.Exec(ctx, `INSERT INTO core.plan_proposal_basis(
		workspace_id,proposal_id,task_id,account_id,group_id,target_id,anchor_id,
		summary_id,summary_version,evidence_kinds,source_manifest_sha256,selection_reason,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		workspace, proposalID, taskID, basis.AccountID, basis.GroupID, basis.TargetID,
		basis.AnchorID, basis.SummaryID, basis.Version, evidenceJSON, basis.Manifest, basis.Reason, at)
	return err
}

// planProposalLoadBasis reads the recorded provenance for operator review and the
// trace view. An empty list is a real state, not a missing one.
func planProposalLoadBasis(ctx context.Context, tx pgx.Tx, workspace, proposalID string) ([]PlanProposalBasis, error) {
	rows, err := tx.Query(ctx, `SELECT account_id,group_id,target_id,anchor_id,summary_id,summary_version,
		evidence_kinds,source_manifest_sha256,selection_reason,created_at
		FROM core.plan_proposal_basis WHERE workspace_id=$1 AND proposal_id=$2
		ORDER BY created_at,account_id,group_id`, workspace, proposalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlanProposalBasis{}
	for rows.Next() {
		var item PlanProposalBasis
		var evidence []byte
		var created time.Time
		if err := rows.Scan(&item.AccountId, &item.GroupId, &item.TargetId, &item.AnchorId,
			&item.SummaryId, &item.SummaryVersion, &evidence, &item.SourceManifestSha256,
			&item.SelectionReason, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = contract.UTCDateTime(created.UTC())
		if len(evidence) > 0 {
			if err := json.Unmarshal(evidence, &item.EvidenceKinds); err != nil {
				return nil, err
			}
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type planProposalRow struct {
	WorkspaceID         string
	ID                  string
	TaskID              string
	PrincipalID         string
	ExpectedTaskVersion int
	ScopeRevision       int
	Template            string
	ProposalKind        string
	State               string
	Digest              string
	StepsJSON           []byte
	QuestionsJSON       []byte
	RationaleSummary    string
	Reason              string
	ApprovalID          string
	ApprovedBy          string
	ApprovedAt          time.Time
	AppliedBy           string
	AppliedAt           time.Time
	ExpiresAt           time.Time
	CreatedAt           time.Time
	GraphSnapshotID     string
}

func planProposalPath(taskID string) string {
	return "/v1/tasks/" + taskID + "/plan-proposals"
}

func planProposalActionPath(taskID, proposalID, suffix string) string {
	return planProposalPath(taskID) + "/" + proposalID + "/" + suffix
}

func planProposalLoad(ctx context.Context, tx pgx.Tx, workspace, taskID, proposalID string, forUpdate bool) (planProposalRow, error) {
	query := `
		SELECT id,task_id,principal_id,expected_task_version,scope_revision,template,proposal_kind,state,digest,
		       steps,questions,rationale_summary,reason,COALESCE(approval_id,''),COALESCE(approved_by,''),
		       COALESCE(approved_at,'epoch'::timestamptz),COALESCE(applied_by,''),
		       COALESCE(applied_at,'epoch'::timestamptz),expires_at,created_at,
		       COALESCE(graph_snapshot_id,'')
		FROM core.plan_proposal
		WHERE workspace_id=$1 AND task_id=$2 AND id=$3`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var row planProposalRow
	err := tx.QueryRow(ctx, query, workspace, taskID, proposalID).Scan(
		&row.ID, &row.TaskID, &row.PrincipalID, &row.ExpectedTaskVersion, &row.ScopeRevision,
		&row.Template, &row.ProposalKind, &row.State, &row.Digest, &row.StepsJSON,
		&row.QuestionsJSON, &row.RationaleSummary, &row.Reason, &row.ApprovalID, &row.ApprovedBy,
		&row.ApprovedAt, &row.AppliedBy, &row.AppliedAt, &row.ExpiresAt, &row.CreatedAt,
		&row.GraphSnapshotID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return planProposalRow{}, problem(taskCodeNotFound)
	}
	if err != nil {
		return planProposalRow{}, err
	}
	row.WorkspaceID = workspace
	return row, nil
}

func planProposalProjection(row planProposalRow) (contract.PlanProposal, error) {
	var steps []contract.PlanStep
	var questions []string
	if err := json.Unmarshal(row.StepsJSON, &steps); err != nil {
		return contract.PlanProposal{}, problem(taskCodeStoreUnavailable)
	}
	if err := json.Unmarshal(row.QuestionsJSON, &questions); err != nil {
		return contract.PlanProposal{}, problem(taskCodeStoreUnavailable)
	}
	return contract.PlanProposal{
		SchemaId:            contract.PlanProposalV1,
		TaskId:              row.TaskID,
		ExpectedTaskVersion: row.ExpectedTaskVersion,
		ScopeRevision:       row.ScopeRevision,
		ProposalKind:        contract.PlanProposalProposalKind(row.ProposalKind),
		Steps:               steps,
		Questions:           questions,
		RationaleSummary:    row.RationaleSummary,
	}, nil
}

func planProposalValidateCreate(taskID string, request contract.PlanProposalRequest) error {
	if !taskValidID(taskID) || request.TaskId != taskID ||
		request.ExpectedVersion < 1 || request.ScopeRevision < 1 ||
		!request.ProposalKind.Valid() ||
		request.ProposalKind != contract.PlanProposalRequestProposalKindUseTemplate {
		return problem(taskCodeValidation)
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" || len([]rune(reason)) > 500 {
		return problem(taskCodeValidation)
	}
	return nil
}

func planProposalValidateApply(request contract.PlanApplyRequest) error {
	if request.ExpectedVersion < 1 || !taskValidID(request.ApprovalRef) ||
		!planProposalDigestPattern.MatchString(request.ProposalDigest) {
		return problem(taskCodeValidation)
	}
	return nil
}

func planProposalValidateApprovalRequest(request PlanProposalApprovalRequest) error {
	if request.ExpectedVersion < 1 || !taskValidID(request.ApprovalRef) {
		return problem(taskCodeValidation)
	}
	return nil
}

// planProposalCandidateTarget chooses the lowest external-id eligible target.
// It is deterministic and reads only task/execution truth; it never reserves.
func planProposalCandidateTarget(ctx context.Context, tx pgx.Tx, workspace, taskID string) (string, string, error) {
	var targetID, externalID string
	err := tx.QueryRow(ctx, `
		SELECT tt.target_id,t.external_id
		FROM core.task_target tt
		JOIN core.target t ON (t.workspace_id,t.id)=(tt.workspace_id,tt.target_id)
		WHERE tt.workspace_id=$1 AND tt.task_id=$2
		  AND tt.eligibility='eligible' AND NOT tt.baseline_covered AND NOT t.disabled
		  AND NOT EXISTS(
		    SELECT 1 FROM core.result r
		    WHERE r.workspace_id=tt.workspace_id AND r.task_id=tt.task_id AND r.target_id=tt.target_id AND r.valid)
		  AND NOT EXISTS(
		    SELECT 1 FROM core.target_claim c
		    WHERE c.workspace_id=tt.workspace_id AND c.target_id=tt.target_id
		      AND c.action_family=$3 AND c.state='active')
		  AND NOT core.execution_retains_holds(tt.workspace_id,tt.task_id,tt.target_id)
		ORDER BY t.external_id,tt.target_id
		LIMIT 1`, workspace, taskID, schedActionFamily).Scan(&targetID, &externalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", problem(schedCodeNoWork)
	}
	if err != nil {
		return "", "", err
	}
	return targetID, externalID, nil
}

// planProposalCandidateAccount chooses the lowest-id task account that can
// actually act on the candidate target. Readiness is re-checked at approval and
// application time; this selection is only a suggestion.
func planProposalCandidateAccount(ctx context.Context, tx pgx.Tx, workspace, taskID, targetExternalID string) (string, error) {
	var accountID string
	err := tx.QueryRow(ctx, `
		SELECT a.id
		FROM core.task_account ta
		JOIN core.account a ON (a.workspace_id,a.id)=(ta.workspace_id,ta.account_id)
		WHERE ta.workspace_id=$1 AND ta.task_id=$2
		  AND a.identity_status='verified' AND a.connection_state='online'
		  AND a.login_state='logged_in' AND a.control_state='active' AND a.risk_state='clear'
		  AND a.request_join_verified AND a.next_action_at<=now()
		  AND EXISTS(
		    SELECT 1 FROM core.runtime_credentials rc
		    WHERE rc.workspace_id=a.workspace_id AND rc.runtime_id=a.runtime_id
		      AND rc.account_id=a.id AND rc.owner_epoch=a.owner_epoch AND rc.active)
		  AND EXISTS(
		    SELECT 1 FROM core.baseline_account ba
		    WHERE ba.workspace_id=ta.workspace_id AND ba.task_id=ta.task_id
		      AND ba.account_id=ta.account_id AND ba.complete)
		  AND NOT EXISTS(
		    SELECT 1 FROM core.baseline b
		    WHERE b.workspace_id=ta.workspace_id AND b.task_id=ta.task_id
		      AND b.account_id=ta.account_id AND b.target_external_id=$3)
		  AND NOT EXISTS(
		    SELECT 1 FROM core.account_action_lock l
		    WHERE l.workspace_id=a.workspace_id AND l.account_id=a.id
		      AND l.action_family=$4 AND l.held)
		  AND NOT EXISTS(
		    SELECT 1 FROM core.account_usage u
		    WHERE u.workspace_id=a.workspace_id AND u.account_id=a.id
		      AND u.action_family=$4 AND u.window_start=current_date
		      AND u.reserved+u.consumed>=a.quota_limit)
		ORDER BY a.id
		LIMIT 1`, workspace, taskID, targetExternalID, schedActionFamily).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", problem(schedCodeNoCapacity)
	}
	if err != nil {
		return "", err
	}
	return accountID, nil
}

func planProposalFreeSlot(ctx context.Context, tx pgx.Tx, workspace, taskID string) (bool, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM core.goal_slot
		WHERE workspace_id=$1 AND task_id=$2 AND state='free'`, workspace, taskID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func planProposalBudgetOpen(ctx context.Context, tx pgx.Tx, task TaskRow) error {
	var used int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`,
		task.WorkspaceID, task.ID).Scan(&used); err != nil {
		return err
	}
	if used >= task.MaxTotalAttempts {
		return problem(schedCodeLocalBudget)
	}
	free, err := planProposalFreeSlot(ctx, tx, task.WorkspaceID, task.ID)
	if err != nil {
		return err
	}
	if !free {
		return problem(schedCodeNoCapacity)
	}
	return nil
}

// planProposalHasNewEvidence enforces the anti-replanning bound: after an
// applied proposal, another proposal is allowed only if the task has produced
// an execution or trusted result since that application. A proposal by itself
// is intent, not new evidence, so it cannot feed an unbounded replanning loop.
func planProposalHasNewEvidence(ctx context.Context, tx pgx.Tx, workspace, taskID string) (bool, error) {
	var lastApplied time.Time
	err := tx.QueryRow(ctx, `
		SELECT applied_at FROM core.plan_application
		WHERE workspace_id=$1 AND task_id=$2
		ORDER BY task_version DESC LIMIT 1`, workspace, taskID).Scan(&lastApplied)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var fresh bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM core.execution
		  WHERE workspace_id=$1 AND task_id=$2 AND observed_at>$3)
		OR EXISTS(
		  SELECT 1 FROM core.result
		  WHERE workspace_id=$1 AND task_id=$2 AND confirmed_at>$3)
		OR EXISTS(
		  SELECT 1 FROM core.membership m
		  JOIN core.task_account ta
		    ON ta.workspace_id=m.workspace_id AND ta.account_id=m.account_id AND ta.task_id=$2
		  JOIN core.task_target tt
		    ON tt.workspace_id=m.workspace_id AND tt.target_id=m.target_id AND tt.task_id=$2
		  WHERE m.workspace_id=$1 AND m.observed_at>$3)`,
		workspace, taskID, lastApplied).Scan(&fresh); err != nil {
		return false, err
	}
	return fresh, nil
}

// planProposalValidateStep re-checks one stored step against live task truth.
// It is used by approval and again by application; neither trusts persisted
// eligibility copied at generation time.
func planProposalValidateStep(ctx context.Context, tx pgx.Tx, task TaskRow, step contract.PlanStep) error {
	if step.StepNo < 1 || step.StepNo > 20 || step.Action != contract.Action(planProposalAction) ||
		!step.TargetId.IsSpecified() || step.TargetId.IsNull() ||
		!step.AccountId.IsSpecified() || step.AccountId.IsNull() ||
		!step.Parameters.GroupId.IsSpecified() || step.Parameters.GroupId.IsNull() {
		return problem(taskCodeStateConflict)
	}
	targetID := step.TargetId.MustGet()
	accountID := step.AccountId.MustGet()
	groupID := step.Parameters.GroupId.MustGet()
	if !taskValidID(targetID) || !taskValidID(accountID) || !taskValidQQID(groupID) {
		return problem(taskCodeStateConflict)
	}

	var externalID, eligibility string
	var baselineCovered, disabled bool
	err := tx.QueryRow(ctx, `
		SELECT t.external_id,tt.eligibility,tt.baseline_covered,t.disabled
		FROM core.task_target tt
		JOIN core.target t ON (t.workspace_id,t.id)=(tt.workspace_id,tt.target_id)
		WHERE tt.workspace_id=$1 AND tt.task_id=$2 AND tt.target_id=$3`,
		task.WorkspaceID, task.ID, targetID).Scan(&externalID, &eligibility, &baselineCovered, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return problem(taskCodeStateConflict)
	}
	if err != nil {
		return err
	}
	if externalID != groupID || eligibility != "eligible" || baselineCovered || disabled {
		return problem(taskCodeStateConflict)
	}

	var held, retained, resulted bool
	if err = tx.QueryRow(ctx, `
		SELECT
		  EXISTS(SELECT 1 FROM core.target_claim c
		    WHERE c.workspace_id=$1 AND c.target_id=$3 AND c.action_family=$4 AND c.state='active'),
		  core.execution_retains_holds($1,$2,$3),
		  EXISTS(SELECT 1 FROM core.result r
		    WHERE r.workspace_id=$1 AND r.task_id=$2 AND r.target_id=$3 AND r.valid)`,
		task.WorkspaceID, task.ID, targetID, schedActionFamily).Scan(&held, &retained, &resulted); err != nil {
		return err
	}
	if held || retained || resulted {
		return problem(taskCodeStateConflict)
	}

	return planProposalValidateAccount(ctx, tx, task, accountID, groupID)
}

func planProposalValidateAccount(ctx context.Context, tx pgx.Tx, task TaskRow, accountID, groupID string) error {
	var ok bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1
		  FROM core.task_account ta
		  JOIN core.account a ON (a.workspace_id,a.id)=(ta.workspace_id,ta.account_id)
		  WHERE ta.workspace_id=$1 AND ta.task_id=$2 AND ta.account_id=$3
		    AND a.identity_status='verified' AND a.connection_state='online'
		    AND a.login_state='logged_in' AND a.control_state='active' AND a.risk_state='clear'
		    AND a.request_join_verified AND a.next_action_at<=now()
		    AND EXISTS(
		      SELECT 1 FROM core.runtime_credentials rc
		      WHERE rc.workspace_id=a.workspace_id AND rc.runtime_id=a.runtime_id
		        AND rc.account_id=a.id AND rc.owner_epoch=a.owner_epoch AND rc.active)
		    AND EXISTS(
		      SELECT 1 FROM core.baseline_account ba
		      WHERE ba.workspace_id=ta.workspace_id AND ba.task_id=ta.task_id
		        AND ba.account_id=ta.account_id AND ba.complete)
		    AND NOT EXISTS(
		      SELECT 1 FROM core.baseline b
		      WHERE b.workspace_id=ta.workspace_id AND b.task_id=ta.task_id
		        AND b.account_id=ta.account_id AND b.target_external_id=$4)
		    AND NOT EXISTS(
		      SELECT 1 FROM core.account_action_lock l
		      WHERE l.workspace_id=a.workspace_id AND l.account_id=a.id
		        AND l.action_family=$5 AND l.held)
		    AND NOT EXISTS(
		      SELECT 1 FROM core.account_usage u
		      WHERE u.workspace_id=a.workspace_id AND u.account_id=a.id
		        AND u.action_family=$5 AND u.window_start=current_date
		        AND u.reserved+u.consumed>=a.quota_limit))`,
		task.WorkspaceID, task.ID, accountID, groupID, schedActionFamily).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return problem(schedCodeNoCapacity)
	}
	return nil
}

// planProposalValidateApproval verifies the operator approval row against the
// proposal step. Live tasks additionally require the task's own approval and the
// whole task scope, so a proposal cannot smuggle an out-of-approval step.
func planProposalValidateApproval(ctx context.Context, tx pgx.Tx, task TaskRow, proposal planProposalRow, approvalRef string, step contract.PlanStep) error {
	a, err := taskLoadApproval(ctx, tx, task.WorkspaceID, approvalRef)
	if err != nil {
		return err
	}
	var dbNow time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		return err
	}
	if a.Revoked || !a.ExpiresAt.After(dbNow) || a.PrincipalID != task.PrincipalID ||
		a.ScopeRevision != proposal.ScopeRevision || a.MaxAttempts < 1 {
		return problem(taskCodeForbidden)
	}
	accountID := step.AccountId.MustGet()
	groupID := step.Parameters.GroupId.MustGet()
	if !contains(a.AccountIDs, accountID) || !contains(a.TargetIDs, groupID) {
		return problem(taskCodeForbidden)
	}
	if task.Mode == "live" {
		if task.ApprovalID == nil || *task.ApprovalID != approvalRef {
			return problem(taskCodeForbidden)
		}
		return taskValidateLiveScope(ctx, tx, task)
	}
	return nil
}

func planProposalStep(row planProposalRow) (contract.PlanStep, error) {
	proposal, err := planProposalProjection(row)
	if err != nil {
		return contract.PlanStep{}, err
	}
	if len(proposal.Steps) != 1 {
		return contract.PlanStep{}, problem(taskCodeStateConflict)
	}
	return proposal.Steps[0], nil
}

// planProposalSteps projects and shape-checks the immutable step list. The
// legacy apply action keeps its single-step contract via planProposalStep;
// proposal-mode materialization accepts up to twenty ordered steps.
func planProposalSteps(row planProposalRow) ([]contract.PlanStep, error) {
	proposal, err := planProposalProjection(row)
	if err != nil {
		return nil, err
	}
	if len(proposal.Steps) < 1 || len(proposal.Steps) > 20 {
		return nil, problem(taskCodeStateConflict)
	}
	for i, step := range proposal.Steps {
		// Steps are stored and executed in step_no order 1..N; materialization and
		// the dependency map rely on that exact sequence.
		if step.StepNo != i+1 {
			return nil, problem(taskCodeStateConflict)
		}
		if _, err := planProposalStepDependencies(step); err != nil {
			return nil, err
		}
	}
	return proposal.Steps, nil
}

// planProposalStepDependencies resolves the step numbers one step depends on.
// Contract semantics: a missing/null depends_on means every earlier step (the
// historical total-order plan), an explicit empty list means no dependency, and
// an explicit list must name earlier steps only. Self/forward references and
// duplicates are rejected, so the plan can never contain a cycle.
func planProposalStepDependencies(step contract.PlanStep) ([]int, error) {
	deps := make([]int, 0, max(step.StepNo-1, 0))
	if step.DependsOn == nil {
		for n := 1; n < step.StepNo; n++ {
			deps = append(deps, n)
		}
		return deps, nil
	}
	seen := make(map[int]bool, len(*step.DependsOn))
	for _, n := range *step.DependsOn {
		if n < 1 || n >= step.StepNo || seen[n] {
			return nil, problem(taskCodeStateConflict)
		}
		seen[n] = true
		deps = append(deps, n)
	}
	return deps, nil
}

// GetPlanProposal returns the durable proposal and its digest for operator
// review. It is a read projection only; no version bump and no external action.
func (s *Store) GetPlanProposal(ctx context.Context, actor Actor, taskID, proposalID string) (PlanProposalView, error) {
	if err := actor.require("task.read"); err != nil {
		return PlanProposalView{}, err
	}
	if actor.RuntimeID != "" {
		return PlanProposalView{}, problem(taskCodeForbidden)
	}
	if !taskValidID(taskID) || !taskValidID(proposalID) {
		return PlanProposalView{}, problem(taskCodeNotFound)
	}
	var out PlanProposalView
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM core.task WHERE workspace_id=$1 AND id=$2`,
			actor.WorkspaceID, taskID))
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(taskCodeNotFound)
		}
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.read"); err != nil {
			return err
		}
		row, err := planProposalLoad(ctx, tx, actor.WorkspaceID, taskID, proposalID, false)
		if err != nil {
			return err
		}
		proposal, err := planProposalProjection(row)
		if err != nil {
			return err
		}
		basis, err := planProposalLoadBasis(ctx, tx, actor.WorkspaceID, row.ID)
		if err != nil {
			return err
		}
		out = PlanProposalView{
			ProposalId:          row.ID,
			TaskId:              row.TaskID,
			State:               row.State,
			Digest:              row.Digest,
			ExpectedTaskVersion: row.ExpectedTaskVersion,
			ScopeRevision:       row.ScopeRevision,
			ExpiresAt:           contract.UTCDateTime(row.ExpiresAt.UTC()),
			Proposal:            proposal,
			Basis:               basis,
		}
		return nil
	})
	if err != nil {
		return PlanProposalView{}, err
	}
	return out, nil
}

// CreatePlanProposal materializes one deterministic bounded proposal from the
// live task scope. It does not call a model and does not mutate task version.
func (s *Store) CreatePlanProposal(ctx context.Context, actor Actor, taskID, key string, request contract.PlanProposalRequest) (contract.Receipt, error) {
	return s.createPlanProposal(ctx, actor, taskID, key, request, planProposalCreateOptions{
		SnapshotID: planProposalRequestSnapshot(request),
	})
}

// planProposalRequestSnapshot returns the optional snapshot id carried by the
// request body, so the plain create route can cite provenance without the
// caller-supplied candidate list.
func planProposalRequestSnapshot(request contract.PlanProposalRequest) string {
	if request.GraphSnapshotId == nil {
		return ""
	}
	return string(*request.GraphSnapshotId)
}

// planProposalCreateOptions carries the two proposal sources: the live task
// scope (empty options) and an approved visible-graph snapshot supplied by the
// graph sync slice. Candidates are in-scope task targets only; the option never
// widens what the task or approval allows.
type planProposalCreateOptions struct {
	SnapshotID string
	Candidates []GraphProposalCandidate
}

func (s *Store) createPlanProposal(ctx context.Context, actor Actor, taskID, key string, request contract.PlanProposalRequest, opts planProposalCreateOptions) (contract.Receipt, error) {
	if err := actor.require(planProposalPerm); err != nil {
		return contract.Receipt{}, err
	}
	if actor.RuntimeID != "" {
		return contract.Receipt{}, problem(taskCodeForbidden)
	}
	if err := taskCheckIdempotencyKey(key); err != nil {
		return contract.Receipt{}, err
	}
	if err := planProposalValidateCreate(taskID, request); err != nil {
		return contract.Receipt{}, err
	}
	var digestSource any = request
	if opts.SnapshotID != "" {
		digestSource = struct {
			Request    contract.PlanProposalRequest `json:"request"`
			SnapshotID string                       `json:"graph_snapshot_id"`
		}{request, opts.SnapshotID}
	}
	bodyDigest, err := digest(digestSource)
	if err != nil {
		return contract.Receipt{}, err
	}
	path := planProposalPath(taskID)

	var out contract.Receipt
	err = s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, planProposalPerm); err != nil {
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
		if request.ExpectedVersion != task.Version || request.ScopeRevision != task.ScopeRevision {
			return problem(taskCodeVersionConflict)
		}
		switch task.State {
		case "RUNNING", "WAITING_EXTERNAL", "VERIFYING":
		default:
			return problem(taskCodeStateConflict)
		}
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
			return err
		}
		if !task.Deadline.After(dbNow) {
			return problem(taskCodeStateConflict)
		}
		if task.Mode == "live" {
			if err := taskValidateLiveScope(ctx, tx, task); err != nil {
				return err
			}
		}
		fresh, err := planProposalHasNewEvidence(ctx, tx, task.WorkspaceID, task.ID)
		if err != nil {
			return err
		}
		if !fresh {
			return problem(taskCodeStateConflict)
		}
		if err := planProposalBudgetOpen(ctx, tx, task); err != nil {
			return err
		}
		if opts.SnapshotID != "" {
			if err := validateGraphSnapshot(ctx, tx, actor.WorkspaceID, task.ID, opts.SnapshotID); err != nil {
				return planSnapshotStateError(err)
			}
		}
		steps, err := s.planProposalStepsForCreate(ctx, tx, task, opts)
		if err != nil {
			return err
		}
		rationale := "Fixed controlled-participation template selected one in-scope target and one ready account at the current task version. This proposal records intent only; no external action is performed."
		if len(steps) > 1 {
			rationale = "Fixed controlled-participation template selected ordered in-scope targets and ready accounts from one visible graph snapshot at the current task version. This proposal records intent only; no external action is performed."
		}

		proposal := contract.PlanProposal{
			SchemaId:            contract.PlanProposalV1,
			TaskId:              task.ID,
			ExpectedTaskVersion: task.Version,
			ScopeRevision:       task.ScopeRevision,
			ProposalKind:        contract.PlanProposalProposalKindUseTemplate,
			Steps:               steps,
			Questions:           []string{},
			RationaleSummary:    rationale,
		}
		proposalDigest, err := digest(proposal)
		if err != nil {
			return err
		}
		stepsJSON, err := json.Marshal(proposal.Steps)
		if err != nil {
			return err
		}
		questionsJSON, err := json.Marshal(proposal.Questions)
		if err != nil {
			return err
		}
		expires := dbNow.Add(planProposalTTL)
		if expires.After(task.Deadline) {
			expires = task.Deadline
		}
		if !expires.After(dbNow.Add(time.Second)) {
			return problem(taskCodeStateConflict)
		}

		// lockTask serializes proposal creation per task, so this lookup is
		// race-free; the partial unique index remains as defense in depth.
		var existingID string
		err = tx.QueryRow(ctx, `
			SELECT id FROM core.plan_proposal
			WHERE workspace_id=$1 AND task_id=$2 AND expected_task_version=$3
			  AND scope_revision=$4 AND template=$5 AND digest=$6
			  AND state IN ('pending','approved','applied')`,
			actor.WorkspaceID, task.ID, task.Version, task.ScopeRevision, planProposalTemplate,
			proposalDigest).Scan(&existingID)
		inserted := false
		proposalID := existingID
		switch {
		case err == nil:
			// The same deterministic proposal already exists at this task/scope
			// version. Return that row rather than creating a duplicate.
		case errors.Is(err, pgx.ErrNoRows):
			proposalID = newID("proposal")
			if _, err := tx.Exec(ctx, `
				INSERT INTO core.plan_proposal(
				 workspace_id,id,task_id,principal_id,expected_task_version,scope_revision,template,
				 proposal_kind,state,digest,steps,questions,rationale_summary,reason,expires_at,created_at,
				 graph_snapshot_id)
				VALUES($1,$2,$3,$4,$5,$6,$7,'use_template','pending',$8,$9,$10,$11,$12,$13,$14,$15)`,
				actor.WorkspaceID, proposalID, task.ID, task.PrincipalID, task.Version, task.ScopeRevision,
				planProposalTemplate, proposalDigest, stepsJSON, questionsJSON, proposal.RationaleSummary,
				request.Reason, expires, dbNow, nullableString(opts.SnapshotID)); err != nil {
				return err
			}
			inserted = true
		default:
			return err
		}
		if inserted {
			if err := appendEvent(ctx, tx, actor.WorkspaceID, task.ID, "plan.proposal.created", actor.PrincipalID, map[string]any{
				"proposal_id":           proposalID,
				"proposal_digest":       proposalDigest,
				"template":              planProposalTemplate,
				"expected_task_version": task.Version,
				"scope_revision":        task.ScopeRevision,
				"steps":                 len(proposal.Steps),
				"external_action":       false,
			}); err != nil {
				return err
			}
			// plan_proposal_basis has exactly one append-only row per proposal
			// (workspace,proposal_id). It records the first selected step's live
			// basis; every step is still revalidated against the live task,
			// approval and scope at approve/materialize/reserve time.
			first := steps[0]
			basis, err := planProposalLiveBasis(ctx, tx, actor.WorkspaceID, task.ID,
				first.AccountId.MustGet(), first.Parameters.GroupId.MustGet(), first.TargetId.MustGet(), dbNow)
			if err != nil {
				return err
			}
			if err := planProposalRecordBasis(ctx, tx, actor.WorkspaceID, proposalID, task.ID, basis, dbNow); err != nil {
				return err
			}
		}

		out = contract.Receipt{
			AcceptedAt:      contract.UTCDateTime(dbNow.UTC()),
			Action:          contract.ReceiptActionRequestPlanProposal,
			ExpectedVersion: request.ExpectedVersion,
			Id:              newID("receipt"),
			ResourceId:      proposalID,
			State:           contract.Accepted,
		}
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, proposalID, out)
	})
	if err != nil {
		return contract.Receipt{}, err
	}
	return out, nil
}

// ApprovePlanProposal binds a trusted core.approval row to a pending proposal.
// It does not change task version and performs no external action.
func (s *Store) ApprovePlanProposal(ctx context.Context, actor Actor, taskID, proposalID, key string, request PlanProposalApprovalRequest) (PlanProposalApprovalReceipt, error) {
	if err := actor.require(planProposalApprove); err != nil {
		return PlanProposalApprovalReceipt{}, err
	}
	if actor.RuntimeID != "" {
		return PlanProposalApprovalReceipt{}, problem(taskCodeForbidden)
	}
	if err := taskCheckIdempotencyKey(key); err != nil {
		return PlanProposalApprovalReceipt{}, err
	}
	if err := planProposalValidateApprovalRequest(request); err != nil {
		return PlanProposalApprovalReceipt{}, err
	}
	if !taskValidID(taskID) || !taskValidID(proposalID) {
		return PlanProposalApprovalReceipt{}, problem(taskCodeNotFound)
	}
	bodyDigest, err := digest(request)
	if err != nil {
		return PlanProposalApprovalReceipt{}, err
	}
	path := planProposalActionPath(taskID, proposalID, "approve")

	var out PlanProposalApprovalReceipt
	err = s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, planProposalApprove); err != nil {
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
		proposal, err := planProposalLoad(ctx, tx, actor.WorkspaceID, taskID, proposalID, true)
		if err != nil {
			return err
		}
		if proposal.ExpectedTaskVersion != task.Version || proposal.ScopeRevision != task.ScopeRevision {
			return problem(taskCodeVersionConflict)
		}
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
			return err
		}
		if !proposal.ExpiresAt.After(dbNow) {
			return problem(taskCodeStateConflict)
		}
		if proposal.State == "approved" {
			if proposal.ApprovalID != request.ApprovalRef {
				return problem(taskCodeStateConflict)
			}
			out = PlanProposalApprovalReceipt{
				AcceptedAt:  contract.UTCDateTime(proposal.ApprovedAt.UTC()),
				ApprovalRef: proposal.ApprovalID,
				ProposalId:  proposal.ID,
				State:       proposal.State,
			}
			return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, proposal.ID, out)
		}
		if proposal.State != "pending" {
			return problem(taskCodeStateConflict)
		}
		steps, err := planProposalSteps(proposal)
		if err != nil {
			return err
		}
		for _, step := range steps {
			if err := planProposalValidateStep(ctx, tx, task, step); err != nil {
				return err
			}
			if err := planProposalValidateApproval(ctx, tx, task, proposal, request.ApprovalRef, step); err != nil {
				return err
			}
		}
		updated, err := tx.Exec(ctx, `
			UPDATE core.plan_proposal
			SET state='approved',approval_id=$4,approved_by=$5,approved_at=$6
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='pending'`,
			actor.WorkspaceID, taskID, proposalID, request.ApprovalRef, actor.PrincipalID, dbNow)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return problem(taskCodeStateConflict)
		}
		if err := appendEvent(ctx, tx, actor.WorkspaceID, taskID, "plan.proposal.approved", actor.PrincipalID, map[string]any{
			"proposal_id":     proposalID,
			"proposal_digest": proposal.Digest,
			"approval_ref":    request.ApprovalRef,
			"external_action": false,
		}); err != nil {
			return err
		}
		out = PlanProposalApprovalReceipt{
			AcceptedAt:  contract.UTCDateTime(dbNow.UTC()),
			ApprovalRef: request.ApprovalRef,
			ProposalId:  proposalID,
			State:       "approved",
		}
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, proposalID, out)
	})
	if err != nil {
		return PlanProposalApprovalReceipt{}, err
	}
	return out, nil
}

// ApplyPlanProposal records approved intent in an immutable application row,
// bumps task version and appends an event. No work item is enqueued: the worker
// never consumes applications; existing task scheduling runs independently.
// It writes no execution, permit, claim, usage or bridge operation.
func (s *Store) ApplyPlanProposal(ctx context.Context, actor Actor, taskID, proposalID, key string, request contract.PlanApplyRequest) (contract.Receipt, error) {
	if err := actor.require(planProposalPerm); err != nil {
		return contract.Receipt{}, err
	}
	if actor.RuntimeID != "" {
		return contract.Receipt{}, problem(taskCodeForbidden)
	}
	if err := taskCheckIdempotencyKey(key); err != nil {
		return contract.Receipt{}, err
	}
	if !taskValidID(taskID) || !taskValidID(proposalID) {
		return contract.Receipt{}, problem(taskCodeNotFound)
	}
	if err := planProposalValidateApply(request); err != nil {
		return contract.Receipt{}, err
	}
	bodyDigest, err := digest(request)
	if err != nil {
		return contract.Receipt{}, err
	}
	path := planProposalActionPath(taskID, proposalID, "apply")

	var out contract.Receipt
	err = s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, planProposalPerm); err != nil {
			return err
		}
		// Legacy apply stays intent-only and applies to legacy tasks only. A
		// proposal-mode task is executed through materialize; refusing here keeps
		// exactly one execution path per mode.
		if task.OrchestrationMode == orchestrationModeProposal {
			return problem(taskCodeStateConflict)
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
		if proposal.Digest != request.ProposalDigest || proposal.ApprovalID != request.ApprovalRef {
			return problem(taskCodeStateConflict)
		}
		switch task.State {
		case "RUNNING", "WAITING_EXTERNAL", "VERIFYING":
		default:
			return problem(taskCodeStateConflict)
		}
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
			return err
		}
		if !proposal.ExpiresAt.After(dbNow) {
			return problem(taskCodeStateConflict)
		}
		step, err := planProposalStep(proposal)
		if err != nil {
			return err
		}
		if err := planProposalValidateStep(ctx, tx, task, step); err != nil {
			return err
		}
		if err := planProposalValidateApproval(ctx, tx, task, proposal, request.ApprovalRef, step); err != nil {
			return err
		}
		if err := planProposalBudgetOpen(ctx, tx, task); err != nil {
			return err
		}

		applicationID := newID("planapp")
		receiptID := newID("receipt")
		if _, err := tx.Exec(ctx, `
			INSERT INTO core.plan_application(
			 workspace_id,id,task_id,proposal_id,proposal_digest,task_version,scope_revision,
			 approval_id,step_no,action,account_id,target_id,group_external_id,receipt_id,applied_by,applied_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			actor.WorkspaceID, applicationID, task.ID, proposal.ID, proposal.Digest, task.Version,
			task.ScopeRevision, proposal.ApprovalID, step.StepNo, string(step.Action),
			step.AccountId.MustGet(), step.TargetId.MustGet(), step.Parameters.GroupId.MustGet(),
			receiptID, actor.PrincipalID, dbNow); err != nil {
			return err
		}
		updated, err := tx.Exec(ctx, `
			UPDATE core.plan_proposal
			SET state='applied',applied_by=$4,applied_at=$5
			WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='approved'`,
			actor.WorkspaceID, taskID, proposalID, actor.PrincipalID, dbNow)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return problem(taskCodeStateConflict)
		}
		versioned, err := tx.Exec(ctx, `
			UPDATE core.task SET version=version+1
			WHERE workspace_id=$1 AND id=$2 AND version=$3`,
			actor.WorkspaceID, taskID, task.Version)
		if err != nil {
			return err
		}
		if versioned.RowsAffected() != 1 {
			return problem(taskCodeVersionConflict)
		}
		if err := appendEvent(ctx, tx, actor.WorkspaceID, taskID, "plan.proposal.applied", actor.PrincipalID, map[string]any{
			"application_id":        applicationID,
			"proposal_id":           proposalID,
			"proposal_digest":       proposal.Digest,
			"approval_ref":          proposal.ApprovalID,
			"from_task_version":     task.Version,
			"to_task_version":       task.Version + 1,
			"scope_revision":        task.ScopeRevision,
			"action":                string(step.Action),
			"account_id":            step.AccountId.MustGet(),
			"target_id":             step.TargetId.MustGet(),
			"external_action":       false,
			"dispatch_invoked":      false,
			"external_request_sent": false,
		}); err != nil {
			return err
		}
		out = contract.Receipt{
			AcceptedAt:      contract.UTCDateTime(dbNow.UTC()),
			Action:          contract.ReceiptActionApply,
			ExpectedVersion: request.ExpectedVersion,
			Id:              receiptID,
			ResourceId:      proposalID,
			State:           contract.Accepted,
		}
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, proposalID, out)
	})
	if err != nil {
		if taskUniqueViolation(err) {
			var replay contract.Receipt
			lookupErr := s.tx(ctx, func(tx pgx.Tx) error {
				rec, found, readErr := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key)
				if readErr != nil {
					return readErr
				}
				if !found {
					return problem(taskCodeStateConflict)
				}
				if rec.digest != bodyDigest {
					return problem(taskCodeIdempotency)
				}
				return json.Unmarshal(rec.response, &replay)
			})
			if lookupErr != nil {
				return contract.Receipt{}, lookupErr
			}
			return replay, nil
		}
		return contract.Receipt{}, err
	}
	return out, nil
}
