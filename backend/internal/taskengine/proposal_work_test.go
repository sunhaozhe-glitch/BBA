package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// proposal execution fixtures

type proposalWorkAdapter struct {
	mu      sync.Mutex
	calls   int
	outcome SendOutcome
	err     error
}

func (a *proposalWorkAdapter) RequestJoin(context.Context, Reservation) (SendOutcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.outcome, a.err
}

func (a *proposalWorkAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func proposalWorkMode(t *testing.T, s *Store, taskID, reason string) contract.Receipt {
	t.Helper()
	task, err := s.GetTask(context.Background(), fixtureActor(), taskID)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	receipt, err := s.SetOrchestrationMode(context.Background(), fixtureActor(), taskID, newID("mode"),
		OrchestrationModeRequest{ExpectedVersion: task.Version, Mode: "proposal", Reason: reason})
	if err != nil {
		t.Fatalf("switch to proposal mode: %v", err)
	}
	return receipt
}

func proposalWorkSetup(t *testing.T, s *Store, goal int, groups []string) (contract.Task, string, string, string) {
	t.Helper()
	seedAccount(t, s, "acc_pw", "rt_pw", 20)
	task := createReadyTask(t, s, []string{"acc_pw"}, groups, goal, 3)
	var approvalID string
	if err := s.Pool.QueryRow(context.Background(),
		`SELECT approval_id FROM core.task WHERE workspace_id=$1 AND id=$2`,
		"ws_fixture", task.Id).Scan(&approvalID); err != nil {
		t.Fatalf("read approval: %v", err)
	}
	proposalWorkMode(t, s, task.Id, "fixture proposal execution")
	task, err := s.GetTask(context.Background(), fixtureActor(), task.Id)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := s.CreatePlanProposal(context.Background(), fixtureActor(), task.Id, newID("proposal"),
		contract.PlanProposalRequest{TaskId: task.Id, ExpectedVersion: task.Version, ScopeRevision: task.ScopeRevision,
			ProposalKind: contract.PlanProposalRequestProposalKindUseTemplate, Reason: "fixture proposal"})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	proposalID := receipt.ResourceId
	if _, err := s.ApprovePlanProposal(context.Background(), fixtureActor(), task.Id, proposalID, newID("approve"),
		PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID}); err != nil {
		t.Fatalf("approve proposal: %v", err)
	}
	digest := fixtureString(t, s, `SELECT digest FROM core.plan_proposal WHERE workspace_id=$1 AND id=$2`, "ws_fixture", proposalID)
	return task, approvalID, proposalID, digest
}

func proposalWorkMaterialize(t *testing.T, s *Store, task contract.Task, approvalID, proposalID, digest string) contract.Receipt {
	t.Helper()
	receipt, err := s.MaterializePlanProposal(context.Background(), fixtureActor(), task.Id, proposalID, newID("materialize"),
		MaterializeRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID, ProposalDigest: digest})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if receipt.Action != contract.ReceiptAction("materialize") || receipt.ResourceId == "" {
		t.Fatalf("unexpected materialize receipt: %+v", receipt)
	}
	return receipt
}

func proposalWorkWorker(t *testing.T, s *Store, adapter Adapter, steps WorkerSteps) *Worker {
	t.Helper()
	cfg := WorkerConfig{
		WorkerID:            "worker_proposal_" + newID("test"),
		PollInterval:        150 * time.Millisecond,
		BatchSize:           10,
		Steps:               steps,
		ProposalExecution:   true,
		Transport:           WorkerExternalTransport,
		AllowExternalWrites: true,
		Adapter:             adapter,
	}
	worker, err := NewWorker(s, cfg)
	if err != nil {
		t.Fatalf("new proposal worker: %v", err)
	}
	return worker
}

func proposalWorkTick(t *testing.T, worker *Worker) WorkerTick {
	t.Helper()
	tick, err := worker.Tick(context.Background())
	if err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	return tick
}

func proposalWorkItemRow(t *testing.T, s *Store, taskID string) (string, string, string, int, string) {
	t.Helper()
	var id, state, execution, deps string
	var attempts int
	if err := s.Pool.QueryRow(context.Background(), `
		SELECT id,state,COALESCE(execution_id,''),attempt_count,depends_on::text
		FROM core.work_item WHERE workspace_id=$1 AND task_id=$2 ORDER BY step_no LIMIT 1`,
		"ws_fixture", taskID).Scan(&id, &state, &execution, &attempts, &deps); err != nil {
		t.Fatalf("read work item: %v", err)
	}
	return id, state, execution, attempts, deps
}

func proposalWorkCount(t *testing.T, s *Store, sql string, args ...any) int {
	t.Helper()
	return scalarInt(t, s, sql, args...)
}

// ---------------------------------------------------------------------------
// schema

func TestProposalWorkSchemaGuards(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, _, _ := proposalWorkSetup(t, s, 1, []string{"9000201"})
	proposalID := fixtureString(t, s, `SELECT id FROM core.plan_proposal WHERE workspace_id=$1 AND task_id=$2 LIMIT 1`, "ws_fixture", task.Id)
	_ = approvalID
	_ = proposalID
	if mode := fixtureString(t, s, `SELECT orchestration_mode FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id); mode != "proposal" {
		t.Fatalf("task mode=%q, want proposal", mode)
	}
	if rev := scalarInt(t, s, `SELECT authorization_revision FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id); rev < 2 {
		t.Fatalf("authorization revision=%d, want >=2 after mode change", rev)
	}
	rejectsSQL(t, s, `INSERT INTO core.work_item(workspace_id,id,task_id,materialization_id,application_id,proposal_id,step_id,step_no,kind,account_id,target_id,scope_digest,authorization_version,state)
		VALUES('ws_fixture','work_bad',$1,'missing_mat','missing_app','missing_prop','s1',1,'request_join','acc_pw','missing_target',repeat('a',64),1,'READY')`, task.Id)
	rejectsSQL(t, s, `INSERT INTO core.work_item(workspace_id,id,task_id,materialization_id,application_id,proposal_id,step_id,step_no,kind,account_id,target_id,scope_digest,authorization_version,state)
		VALUES('ws_other','work_bad',$1,'missing_mat','missing_app','missing_prop','s1',1,'request_join','acc_pw','missing_target',repeat('a',64),1,'READY')`, task.Id)
}

func TestProposalWorkTerminalAndAttemptGuards(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000202"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	workID, state, _, _, _ := proposalWorkItemRow(t, s, task.Id)
	if state != "READY" {
		t.Fatalf("fresh work item state=%q, want READY", state)
	}
	fenceWrite := func(sql string) error {
		return s.tx(context.Background(), func(tx pgx.Tx) error { _, err := tx.Exec(context.Background(), sql); return err })
	}
	if err := fenceWrite(`UPDATE core.work_item SET state='CANCELLED',blocked_reason='task_cancelled' WHERE workspace_id='ws_fixture' AND id='` + workID + `'`); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := fenceWrite(`UPDATE core.work_item SET state='READY' WHERE workspace_id='ws_fixture' AND id='` + workID + `'`); err == nil {
		t.Fatal("terminal work item reopened to READY")
	}
}

// ---------------------------------------------------------------------------
// orchestration mode

func TestOrchestrationModeGuardsAndAudit(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_mode", "rt_mode", 5)
	task := createReadyTask(t, s, []string{"acc_mode"}, []string{"9000301"}, 1, 3)
	if mode := fixtureString(t, s, `SELECT orchestration_mode FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id); mode != "legacy" {
		t.Fatalf("new task mode=%q, want legacy", mode)
	}
	beforeVersion := task.Version
	beforeRevision := scalarInt(t, s, `SELECT authorization_revision FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id)
	proposalWorkMode(t, s, task.Id, "operator picks proposal")
	if got := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id); got != beforeVersion+1 {
		t.Fatalf("version=%d, want %d", got, beforeVersion+1)
	}
	if got := scalarInt(t, s, `SELECT authorization_revision FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id); got != beforeRevision+1 {
		t.Fatalf("authorization revision=%d, want %d", got, beforeRevision+1)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='task.orchestration_mode_changed'`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("mode audit events=%d, want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.outbox WHERE workspace_id=$1 AND task_id=$2 AND event_type='task.orchestration_mode_changed'`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("mode outbox rows=%d, want 1", got)
	}

	task, err := s.GetTask(context.Background(), fixtureActor(), task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOrchestrationMode(context.Background(), fixtureActor(), task.Id, newID("mode"),
		OrchestrationModeRequest{ExpectedVersion: task.Version, Mode: "proposal", Reason: "same mode"}); err != nil {
		t.Fatalf("same-mode no-op: %v", err)
	}
	if got := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id); got != task.Version {
		t.Fatalf("same-mode request bumped version to %d", got)
	}
	if _, err := s.SetOrchestrationMode(context.Background(), fixtureActor(), task.Id, newID("mode"),
		OrchestrationModeRequest{ExpectedVersion: task.Version - 1, Mode: "legacy", Reason: "stale"}); Code(err) != taskCodeVersionConflict {
		t.Fatalf("stale mode switch code=%q, want %q", Code(err), taskCodeVersionConflict)
	}
	runtimeActor := Actor{WorkspaceID: "ws_fixture", PrincipalID: "principal_fixture", RuntimeID: "rt_mode",
		Permissions: map[string]bool{taskPermControl: true}}
	if _, err := s.SetOrchestrationMode(context.Background(), runtimeActor, task.Id, newID("mode"),
		OrchestrationModeRequest{ExpectedVersion: task.Version, Mode: "legacy", Reason: "runtime"}); Code(err) != taskCodeForbidden {
		t.Fatalf("runtime mode switch code=%q, want %q", Code(err), taskCodeForbidden)
	}
	other := fixtureActor()
	other.WorkspaceID = "ws_other"
	if _, err := s.SetOrchestrationMode(context.Background(), other, task.Id, newID("mode"),
		OrchestrationModeRequest{ExpectedVersion: task.Version, Mode: "legacy", Reason: "foreign"}); Code(err) != taskCodeNotFound {
		t.Fatalf("cross-workspace mode switch code=%q, want %q", Code(err), taskCodeNotFound)
	}
}

func TestOrchestrationModeRequiresQuiescence(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000302"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	ctx := context.Background()
	claim, err := s.ClaimReadyWorkItem(ctx, "worker_quiesce", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	if _, err := s.ReserveWorkItem(ctx, *claim); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// A durable execution in a possibly in-flight state blocks both directions.
	if _, err := s.SetOrchestrationMode(ctx, fixtureActor(), task.Id, newID("mode"),
		OrchestrationModeRequest{ExpectedVersion: scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id),
			Mode: "legacy", Reason: "still sending"}); Code(err) != taskCodeStateConflict {
		t.Fatalf("mode switch with CREATED execution code=%q, want %q", Code(err), taskCodeStateConflict)
	}
	// Cancelling the local execution row still leaves a pending bridge ledger
	// until the observation settles; the switch stays blocked, never guessed.
}

// ---------------------------------------------------------------------------
// materialization

func TestMaterializeRequiresApprovedProposalAndProposalMode(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_mat", "rt_mat", 5)
	task := createReadyTask(t, s, []string{"acc_mat"}, []string{"9000401"}, 1, 3)
	var approvalID string
	if err := s.Pool.QueryRow(context.Background(), `SELECT approval_id FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id).Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Legacy tasks never materialize.
	receipt, err := s.CreatePlanProposal(ctx, fixtureActor(), task.Id, newID("proposal"),
		contract.PlanProposalRequest{TaskId: task.Id, ExpectedVersion: task.Version, ScopeRevision: task.ScopeRevision,
			ProposalKind: contract.PlanProposalRequestProposalKindUseTemplate, Reason: "legacy proposal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovePlanProposal(ctx, fixtureActor(), task.Id, receipt.ResourceId, newID("approve"),
		PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID}); err != nil {
		t.Fatal(err)
	}
	digest := fixtureString(t, s, `SELECT digest FROM core.plan_proposal WHERE workspace_id=$1 AND id=$2`, "ws_fixture", receipt.ResourceId)
	if _, err := s.MaterializePlanProposal(ctx, fixtureActor(), task.Id, receipt.ResourceId, newID("materialize"),
		MaterializeRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID, ProposalDigest: digest}); Code(err) != taskCodeStateConflict {
		t.Fatalf("legacy materialize code=%q, want %q", Code(err), taskCodeStateConflict)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.work_item WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 0 {
		t.Fatalf("legacy materialize created %d work items", got)
	}

	// Proposal mode, but the proposal is not approved yet.
	proposalWorkMode(t, s, task.Id, "proposal mode")
	task, err = s.GetTask(ctx, fixtureActor(), task.Id)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.CreatePlanProposal(ctx, fixtureActor(), task.Id, newID("proposal"),
		contract.PlanProposalRequest{TaskId: task.Id, ExpectedVersion: task.Version, ScopeRevision: task.ScopeRevision,
			ProposalKind: contract.PlanProposalRequestProposalKindUseTemplate, Reason: "pending proposal"})
	if err != nil {
		t.Fatal(err)
	}
	pendingDigest := fixtureString(t, s, `SELECT digest FROM core.plan_proposal WHERE workspace_id=$1 AND id=$2`, "ws_fixture", created.ResourceId)
	if _, err := s.MaterializePlanProposal(ctx, fixtureActor(), task.Id, created.ResourceId, newID("materialize"),
		MaterializeRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID, ProposalDigest: pendingDigest}); Code(err) != taskCodeStateConflict {
		t.Fatalf("unapproved materialize code=%q, want %q", Code(err), taskCodeStateConflict)
	}
	if _, err := s.ApprovePlanProposal(ctx, fixtureActor(), task.Id, created.ResourceId, newID("approve"),
		PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MaterializePlanProposal(ctx, fixtureActor(), task.Id, created.ResourceId, newID("materialize"),
		MaterializeRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID, ProposalDigest: strings.Repeat("0", 64)}); Code(err) != taskCodeStateConflict {
		t.Fatalf("wrong digest materialize code=%q, want %q", Code(err), taskCodeStateConflict)
	}
	// Approval mismatch is rejected even with the right digest.
	otherApproval := newID("approval")
	if _, err := s.Pool.Exec(ctx, `INSERT INTO core.approval(workspace_id,id,principal_id,scope_revision,account_ids,target_ids,max_attempts,expires_at,join_reason,verification_message)
		VALUES('ws_fixture',$1,'principal_fixture',1,$2,$3,10,clock_timestamp()+interval '1 day','fixture reason','fixture answer')`,
		otherApproval, []string{"acc_mat"}, []string{"9000401"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MaterializePlanProposal(ctx, fixtureActor(), task.Id, created.ResourceId, newID("materialize"),
		MaterializeRequest{ExpectedVersion: task.Version, ApprovalRef: otherApproval, ProposalDigest: pendingDigest}); Code(err) != taskCodeStateConflict {
		t.Fatalf("approval mismatch materialize code=%q, want %q", Code(err), taskCodeStateConflict)
	}
	// Legacy /apply stays intent-only and refuses proposal-mode tasks.
	if _, err := s.ApplyPlanProposal(ctx, fixtureActor(), task.Id, created.ResourceId, newID("apply"),
		contract.PlanApplyRequest{ApprovalRef: approvalID, ExpectedVersion: task.Version, ProposalDigest: contract.Digest(pendingDigest)}); Code(err) != taskCodeStateConflict {
		t.Fatalf("legacy apply on proposal task code=%q, want %q", Code(err), taskCodeStateConflict)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.plan_application WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 0 {
		t.Fatalf("rejected materialize wrote %d applications", got)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 0 {
		t.Fatalf("materialize wrote %d executions", got)
	}
}

func TestMaterializeIsIdempotentAndWritesNoExternalEffect(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000402"})
	ctx := context.Background()
	first := proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	if first.State != contract.Accepted {
		t.Fatalf("materialize state=%q", first.State)
	}
	second, err := s.MaterializePlanProposal(ctx, fixtureActor(), task.Id, proposalID, newID("materialize"),
		MaterializeRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID, ProposalDigest: digest})
	if err != nil {
		t.Fatalf("repeat materialize: %v", err)
	}
	if second.ResourceId != first.ResourceId {
		t.Fatalf("repeat materialize created a second materialization: %q != %q", second.ResourceId, first.ResourceId)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.work_item WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("work items=%d, want 1", got)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.plan_application WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("applications=%d, want 1", got)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 0 {
		t.Fatalf("materialize wrote %d executions", got)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.dispatch_permit WHERE workspace_id=$1`, "ws_fixture"); got != 0 {
		t.Fatalf("materialize wrote %d permits", got)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1`, "ws_fixture"); got != 0 {
		t.Fatalf("materialize acquired %d target claims", got)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='plan.proposal.materialized'`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("materialize events=%d, want 1", got)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.outbox WHERE workspace_id=$1 AND task_id=$2 AND event_type='plan.proposal.materialized'`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("materialize outbox=%d, want 1", got)
	}
	if version := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id); version != task.Version+1 {
		t.Fatalf("task version=%d, want %d", version, task.Version+1)
	}
}

// seedMultiStepProposal creates a real two-step proposal from one visible-graph
// snapshot (the S2 seam) rather than inserting stored rows. The candidates are
// the task's two targets in external-id order; the generator emits step 1 and
// step 2 with an explicit depends_on [1].
func seedMultiStepProposal(t *testing.T, s *Store, task contract.Task, approvalID string) (string, string) {
	t.Helper()
	ctx := context.Background()
	snapshotID := "snap_fixture_multistep"
	previousValidator := currentGraphSnapshotValidator()
	RegisterGraphSnapshotValidator(func(ctx context.Context, tx pgx.Tx, workspace, taskID, id string) error {
		if id != snapshotID {
			return problem(taskCodeNotFound)
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.task
			WHERE workspace_id=$1 AND id=$2 AND orchestration_mode='proposal')`, workspace, taskID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return problem(taskCodeStateConflict)
		}
		return nil
	})
	t.Cleanup(func() { RegisterGraphSnapshotValidator(previousValidator) })
	previousWorkItemValidator := currentGraphSnapshotWorkItemValidator()
	RegisterGraphSnapshotWorkItemValidator(func(ctx context.Context, tx pgx.Tx, workspace, taskID, id, proposalID, materializationID, workItemID string) error {
		if id != snapshotID {
			return problem(taskCodeNotFound)
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.task WHERE workspace_id=$1 AND id=$2 AND orchestration_mode='proposal')`, workspace, taskID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return problem(taskCodeStateConflict)
		}
		return nil
	})
	t.Cleanup(func() { RegisterGraphSnapshotWorkItemValidator(previousWorkItemValidator) })

	rows, err := s.Pool.Query(ctx, `SELECT tt.target_id,t.external_id FROM core.task_target tt
		JOIN core.target t ON (t.workspace_id,t.id)=(tt.workspace_id,tt.target_id)
		WHERE tt.workspace_id=$1 AND tt.task_id=$2 ORDER BY t.external_id`, "ws_fixture", task.Id)
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]GraphProposalCandidate, 0, 2)
	for rows.Next() {
		var candidate GraphProposalCandidate
		if err := rows.Scan(&candidate.TargetID, &candidate.ExternalID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(candidates) < 2 {
		t.Fatalf("multi-step fixture needs two targets, got %d", len(candidates))
	}
	snapshot := contract.Id(snapshotID)
	receipt, err := s.CreatePlanProposalFromSnapshot(ctx, fixtureActor(), task.Id, newID("proposal"),
		contract.PlanProposalRequest{TaskId: task.Id, ExpectedVersion: task.Version, ScopeRevision: task.ScopeRevision,
			ProposalKind: contract.PlanProposalRequestProposalKindUseTemplate, Reason: "graph two-step plan",
			GraphSnapshotId: &snapshot},
		snapshotID, candidates[:2])
	if err != nil {
		t.Fatalf("create graph proposal: %v", err)
	}
	proposalID := receipt.ResourceId
	var stepsJSON []byte
	if err := s.Pool.QueryRow(ctx, `SELECT steps FROM core.plan_proposal WHERE workspace_id=$1 AND id=$2`,
		"ws_fixture", proposalID).Scan(&stepsJSON); err != nil {
		t.Fatal(err)
	}
	var steps []contract.PlanStep
	if err := json.Unmarshal(stepsJSON, &steps); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[1].DependsOn == nil || len(*steps[1].DependsOn) != 1 || (*steps[1].DependsOn)[0] != 1 {
		t.Fatalf("generated steps=%+v, want step2 depends_on [1]", steps)
	}
	if _, err := s.ApprovePlanProposal(ctx, fixtureActor(), task.Id, proposalID, newID("approve"),
		PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID}); err != nil {
		t.Fatalf("approve multi-step proposal: %v", err)
	}
	proposalDigest := fixtureString(t, s, `SELECT digest FROM core.plan_proposal WHERE workspace_id=$1 AND id=$2`,
		"ws_fixture", proposalID)
	return proposalID, proposalDigest
}

func TestMaterializeMultiStepDependencyOrder(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_pw", "rt_pw", 20)
	task := createReadyTask(t, s, []string{"acc_pw"}, []string{"9000501", "9000502"}, 2, 3)
	var approvalID string
	if err := s.Pool.QueryRow(context.Background(), `SELECT approval_id FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id).Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	proposalWorkMode(t, s, task.Id, "multi-step proposal mode")
	task, err := s.GetTask(context.Background(), fixtureActor(), task.Id)
	if err != nil {
		t.Fatal(err)
	}
	proposalID, proposalDigest := seedMultiStepProposal(t, s, task, approvalID)
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, proposalDigest)

	rows, err := s.Pool.Query(context.Background(), `SELECT step_no,state,blocked_reason,depends_on::text
		FROM core.work_item WHERE workspace_id=$1 AND task_id=$2 ORDER BY step_no`, "ws_fixture", task.Id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type item struct {
		step    int
		state   string
		reason  *string
		depends string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.step, &it.state, &it.reason, &it.depends); err != nil {
			t.Fatal(err)
		}
		items = append(items, it)
	}
	if len(items) != 2 {
		t.Fatalf("work items=%d, want 2", len(items))
	}
	if items[0].state != "READY" || items[0].depends != "[]" {
		t.Fatalf("step1 = %+v, want READY with no dependencies", items[0])
	}
	if items[1].state != "BLOCKED" || items[1].reason == nil || *items[1].reason != "dependency_pending" {
		t.Fatalf("step2 = %+v, want BLOCKED dependency_pending", items[1])
	}
	var depends []string
	if err := json.Unmarshal([]byte(items[1].depends), &depends); err != nil || len(depends) != 1 {
		t.Fatalf("step2 depends_on=%s", items[1].depends)
	}
}

// proposalDagSetup builds a two-step proposal from a registered visible-graph
// snapshot, approves it and returns its digest. The dependency chain comes from
// the generator's explicit depends_on, not from a SQL-seeded row.
func proposalDagSetup(t *testing.T, s *Store, groups []string) (contract.Task, string, string, string) {
	t.Helper()
	ctx := context.Background()
	seedAccount(t, s, "acc_pw", "rt_pw", 20)
	task := createReadyTask(t, s, []string{"acc_pw"}, groups, len(groups), 3)
	var approvalID string
	if err := s.Pool.QueryRow(ctx, `SELECT approval_id FROM core.task WHERE workspace_id=$1 AND id=$2`,
		"ws_fixture", task.Id).Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	proposalWorkMode(t, s, task.Id, "dag proposal mode")
	task, err := s.GetTask(ctx, fixtureActor(), task.Id)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID := "snap_fixture_dag"
	previousValidator := currentGraphSnapshotValidator()
	RegisterGraphSnapshotValidator(func(context.Context, pgx.Tx, string, string, string) error { return nil })
	t.Cleanup(func() { RegisterGraphSnapshotValidator(previousValidator) })
	previousWorkItemValidator := currentGraphSnapshotWorkItemValidator()
	RegisterGraphSnapshotWorkItemValidator(func(context.Context, pgx.Tx, string, string, string, string, string, string) error { return nil })
	t.Cleanup(func() { RegisterGraphSnapshotWorkItemValidator(previousWorkItemValidator) })

	rows, err := s.Pool.Query(ctx, `SELECT tt.target_id,t.external_id FROM core.task_target tt
		JOIN core.target t ON (t.workspace_id,t.id)=(tt.workspace_id,tt.target_id)
		WHERE tt.workspace_id=$1 AND tt.task_id=$2 ORDER BY t.external_id`, "ws_fixture", task.Id)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []GraphProposalCandidate
	for rows.Next() {
		var candidate GraphProposalCandidate
		if err := rows.Scan(&candidate.TargetID, &candidate.ExternalID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(candidates) < 2 {
		t.Fatalf("dag fixture needs two targets, got %d", len(candidates))
	}
	snapshot := contract.Id(snapshotID)
	receipt, err := s.CreatePlanProposalFromSnapshot(ctx, fixtureActor(), task.Id, newID("proposal"),
		contract.PlanProposalRequest{TaskId: task.Id, ExpectedVersion: task.Version, ScopeRevision: task.ScopeRevision,
			ProposalKind: contract.PlanProposalRequestProposalKindUseTemplate, Reason: "dag proposal",
			GraphSnapshotId: &snapshot},
		snapshotID, candidates[:2])
	if err != nil {
		t.Fatalf("create dag proposal: %v", err)
	}
	if _, err := s.ApprovePlanProposal(ctx, fixtureActor(), task.Id, receipt.ResourceId, newID("approve"),
		PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID}); err != nil {
		t.Fatalf("approve dag proposal: %v", err)
	}
	digest := fixtureString(t, s, `SELECT digest FROM core.plan_proposal WHERE workspace_id=$1 AND id=$2`,
		"ws_fixture", receipt.ResourceId)
	return task, approvalID, receipt.ResourceId, digest
}

func TestProposalDagPromotesDependentThroughTrustedObservation(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalDagSetup(t, s, []string{"9001001", "9001002"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	adapter := &proposalWorkAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "dag_step1"}}
	worker := proposalWorkWorker(t, s, adapter, WorkerSteps{Proposal: true})
	proposalWorkTick(t, worker)
	if adapter.callCount() != 1 {
		t.Fatalf("step1 adapter calls=%d, want 1", adapter.callCount())
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND task_id=$2 AND step_no=2`,
		"ws_fixture", task.Id); state != "BLOCKED" {
		t.Fatalf("step2 state=%q before step1 success, want BLOCKED", state)
	}
	var operationID, targetID string
	if err := s.Pool.QueryRow(context.Background(), `
		SELECT e.operation_id,e.target_id FROM core.work_item wi
		JOIN core.execution e ON (e.workspace_id,e.id)=(wi.workspace_id,wi.execution_id)
		WHERE wi.workspace_id=$1 AND wi.task_id=$2 AND wi.step_no=1`,
		"ws_fixture", task.Id).Scan(&operationID, &targetID); err != nil {
		t.Fatal(err)
	}
	rt := fixtureRuntime("acc_pw", "rt_pw")
	obs := Observation{EventID: "evt_dag_join", OperationID: operationID, AccountID: "acc_pw", TargetID: targetID,
		Kind: "membership.observed", State: "joined", EvidenceRef: "ev_dag_join",
		SourceEpoch: rt.OwnerEpoch, SourceSequence: 2, ObservedAt: time.Now().UTC().Add(2 * time.Second), SnapshotComplete: true}
	if err := s.ReceiveObservation(context.Background(), rt, obs); err != nil {
		t.Fatalf("receive observation: %v", err)
	}
	if err := s.ApplyObservation(context.Background(), "ws_fixture", "rt_pw", obs.EventID); err != nil {
		t.Fatalf("apply observation: %v", err)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND task_id=$2 AND step_no=2`,
		"ws_fixture", task.Id); state != "READY" {
		t.Fatalf("step2 state=%q after step1 success, want READY", state)
	}
	proposalWorkTick(t, worker)
	if adapter.callCount() != 2 {
		t.Fatalf("step2 adapter calls=%d, want 2", adapter.callCount())
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND task_id=$2 AND step_no=2`,
		"ws_fixture", task.Id); state != "WAITING_RESULT" {
		t.Fatalf("step2 state=%q after dispatch, want WAITING_RESULT", state)
	}
}

func TestProposalDagBlocksDependentOnUnknownOutcome(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalDagSetup(t, s, []string{"9001003", "9001004"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	adapter := &proposalWorkAdapter{err: errors.New("ambiguous transport")}
	worker := proposalWorkWorker(t, s, adapter, WorkerSteps{Proposal: true})
	proposalWorkTick(t, worker)
	if adapter.callCount() != 1 {
		t.Fatalf("adapter calls=%d, want 1", adapter.callCount())
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND task_id=$2 AND step_no=1`,
		"ws_fixture", task.Id); state != "UNKNOWN" {
		t.Fatalf("step1 state=%q, want UNKNOWN", state)
	}
	var state string
	var reason *string
	if err := s.Pool.QueryRow(context.Background(), `SELECT state,blocked_reason FROM core.work_item
		WHERE workspace_id=$1 AND task_id=$2 AND step_no=2`, "ws_fixture", task.Id).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "BLOCKED" || reason == nil || *reason != "dependency_failed" {
		t.Fatalf("step2 state=%q reason=%v, want BLOCKED dependency_failed", state, reason)
	}
	proposalWorkTick(t, worker)
	if adapter.callCount() != 1 {
		t.Fatalf("adapter calls=%d after blocked dependency, want 1 (never claimed)", adapter.callCount())
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`,
		"ws_fixture", task.Id); got != 1 {
		t.Fatalf("executions=%d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// claims, fencing, reserve

func TestWorkItemClaimIsSingleWinnerUnderConcurrency(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000601"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	ctx := context.Background()
	type result struct {
		claim *WorkItemClaim
		err   error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := s.ClaimReadyWorkItem(ctx, "worker_race_"+newID("t"), 30*time.Second)
			results <- result{claim, err}
		}()
	}
	wg.Wait()
	close(results)
	claimed := 0
	var winner WorkItemClaim
	for res := range results {
		if res.err != nil {
			t.Fatalf("claim: %v", res.err)
		}
		if res.claim != nil {
			claimed++
			winner = *res.claim
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed=%d, want exactly 1", claimed)
	}
	if winner.LeaseEpoch < 1 {
		t.Fatalf("claim epoch=%d", winner.LeaseEpoch)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND id=$2`, "ws_fixture", winner.WorkItemID); state != "CLAIMED" {
		t.Fatalf("claimed item state=%q", state)
	}
}

func TestWorkItemLeaseSweepReleasesOnlyClaimsWithoutEffect(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000602"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	ctx := context.Background()
	claim, err := s.ClaimReadyWorkItem(ctx, "worker_lease", 30*time.Millisecond)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	if n, err := s.SweepExpiredWorkItemClaims(ctx, 10); err != nil || n != 0 {
		t.Fatalf("early sweep=%d %v, want 0", n, err)
	}
	time.Sleep(60 * time.Millisecond)
	if n, err := s.SweepExpiredWorkItemClaims(ctx, 10); err != nil || n != 1 {
		t.Fatalf("expired sweep=%d %v, want 1", n, err)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND id=$2`, "ws_fixture", claim.WorkItemID); state != "READY" {
		t.Fatalf("swept item state=%q, want READY", state)
	}
	reclaim, err := s.ClaimReadyWorkItem(ctx, "worker_lease2", 30*time.Second)
	if err != nil || reclaim == nil {
		t.Fatalf("reclaim: %v %v", reclaim, err)
	}
	if reclaim.LeaseEpoch <= claim.LeaseEpoch {
		t.Fatalf("reclaim epoch=%d, want > %d", reclaim.LeaseEpoch, claim.LeaseEpoch)
	}
	if _, err := s.ReserveWorkItem(ctx, *reclaim); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if n, err := s.SweepExpiredWorkItemClaims(ctx, 10); err != nil || n != 0 {
		t.Fatalf("sweep after reserve=%d %v, want 0", n, err)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND id=$2`, "ws_fixture", reclaim.WorkItemID); state != "WAITING_RESULT" {
		t.Fatalf("reserved item state=%q, want WAITING_RESULT", state)
	}
}

func TestWorkItemFenceRejectsStaleOwner(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000603"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	ctx := context.Background()
	first, err := s.ClaimReadyWorkItem(ctx, "worker_a", 20*time.Millisecond)
	if err != nil || first == nil {
		t.Fatalf("first claim: %v %v", first, err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := s.SweepExpiredWorkItemClaims(ctx, 10); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimReadyWorkItem(ctx, "worker_b", 30*time.Second)
	if err != nil || second == nil {
		t.Fatalf("second claim: %v %v", second, err)
	}
	if err := s.ReleaseWorkItemClaim(ctx, *first, "READY", "stale_release"); Code(err) != workItemCodeLeaseLost {
		t.Fatalf("stale release code=%q, want %q", Code(err), workItemCodeLeaseLost)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND id=$2`, "ws_fixture", second.WorkItemID); state != "CLAIMED" {
		t.Fatalf("second owner state=%q, want CLAIMED", state)
	}
	if _, err := s.ReserveWorkItem(ctx, *first); Code(err) != workItemCodeLeaseLost {
		t.Fatalf("stale reserve code=%q, want %q", Code(err), workItemCodeLeaseLost)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 0 {
		t.Fatalf("stale owner created %d executions", got)
	}
	if _, err := s.ReserveWorkItem(ctx, *second); err != nil {
		t.Fatalf("current owner reserve: %v", err)
	}
}

func TestReserveWorkItemRechecksCancelAfterClaim(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000604"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	ctx := context.Background()
	claim, err := s.ClaimReadyWorkItem(ctx, "worker_cancel", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	task, err = s.GetTask(ctx, fixtureActor(), task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ControlTask(ctx, fixtureActor(), task.Id, newID("cancel"),
		contract.TaskControlRequest{Action: contract.TaskControlRequestActionCancel, ExpectedVersion: task.Version, Reason: "operator cancel"}); err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND id=$2`, "ws_fixture", claim.WorkItemID); state != "CANCELLED" {
		t.Fatalf("work item state=%q, want CANCELLED", state)
	}
	if _, err := s.ReserveWorkItem(ctx, *claim); err == nil {
		t.Fatal("cancelled task still reserved work")
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 0 {
		t.Fatalf("cancel-after-claim created %d executions", got)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.work_item_attempt WHERE workspace_id=$1`, "ws_fixture"); got != 0 {
		t.Fatalf("cancel-after-claim wrote %d attempts", got)
	}
}

func TestReserveWorkItemRechecksAuthorizationRevision(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000605"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	ctx := context.Background()
	claim, err := s.ClaimReadyWorkItem(ctx, "worker_auth", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	// A durable authorization change after materialization invalidates work
	// materialized under the previous revision.
	if _, err := s.Pool.Exec(ctx, `UPDATE core.task SET authorization_revision=authorization_revision+1 WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveWorkItem(ctx, *claim); Code(err) != taskCodeStateConflict {
		t.Fatalf("stale authorization reserve code=%q, want %q", Code(err), taskCodeStateConflict)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND id=$2`, "ws_fixture", claim.WorkItemID); state != "CANCELLED" {
		t.Fatalf("authorization-invalid item state=%q, want CANCELLED", state)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 0 {
		t.Fatalf("authorization-invalid reserve created %d executions", got)
	}
}

// ---------------------------------------------------------------------------
// worker consumer through Reserve/Bridge

func TestProposalWorkerDispatchAndTrustedObservationLink(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000701"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	adapter := &proposalWorkAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "proposal_receipt"}}
	worker := proposalWorkWorker(t, s, adapter, WorkerSteps{Proposal: true})
	tick := proposalWorkTick(t, worker)
	if tick.ProposalClaimed != 1 || tick.ProposalReserved != 1 || tick.ProposalDispatched != 1 {
		t.Fatalf("tick=%+v, want one claim/reserve/dispatch", tick)
	}
	if adapter.callCount() != 1 {
		t.Fatalf("adapter calls=%d, want 1", adapter.callCount())
	}
	workID, state, executionID, attempts, _ := proposalWorkItemRow(t, s, task.Id)
	if state != "WAITING_RESULT" || executionID == "" || attempts != 1 {
		t.Fatalf("work item state=%q execution=%q attempts=%d", state, executionID, attempts)
	}
	var operationID, applicationID, proposalRef, taskRef string
	if err := s.Pool.QueryRow(context.Background(), `
		SELECT e.operation_id,wi.application_id,wi.proposal_id,wi.task_id
		FROM core.work_item wi JOIN core.execution e ON (e.workspace_id,e.id)=(wi.workspace_id,wi.execution_id)
		WHERE wi.workspace_id=$1 AND wi.id=$2`, "ws_fixture", workID).Scan(&operationID, &applicationID, &proposalRef, &taskRef); err != nil {
		t.Fatal(err)
	}
	if proposalRef != proposalID || taskRef != task.Id || applicationID == "" {
		t.Fatalf("trace mismatch: proposal=%q task=%q application=%q", proposalRef, taskRef, applicationID)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.work_item_attempt WHERE workspace_id=$1 AND work_item_id=$2 AND execution_id=$3`, "ws_fixture", workID, executionID); got != 1 {
		t.Fatalf("attempt linkage rows=%d, want 1", got)
	}

	rt := fixtureRuntime("acc_pw", "rt_pw")
	obs := Observation{EventID: "evt_proposal_join", OperationID: operationID, AccountID: "acc_pw", TargetID: "",
		Kind: "membership.observed", State: "joined", EvidenceRef: "ev_proposal_join",
		SourceEpoch: rt.OwnerEpoch, SourceSequence: 1, ObservedAt: time.Now().UTC().Add(2 * time.Second), SnapshotComplete: true}
	var targetID string
	if err := s.Pool.QueryRow(context.Background(), `SELECT target_id FROM core.execution WHERE workspace_id=$1 AND operation_id=$2`, "ws_fixture", operationID).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	obs.TargetID = targetID
	if err := s.ReceiveObservation(context.Background(), rt, obs); err != nil {
		t.Fatalf("receive observation: %v", err)
	}
	if err := s.ApplyObservation(context.Background(), "ws_fixture", "rt_pw", obs.EventID); err != nil {
		t.Fatalf("apply observation: %v", err)
	}
	state = fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND id=$2`, "ws_fixture", workID)
	if state != "SUCCEEDED" {
		t.Fatalf("observed work item state=%q, want SUCCEEDED", state)
	}
	if outcome := fixtureString(t, s, `SELECT COALESCE(outcome,'') FROM core.work_item_attempt WHERE workspace_id=$1 AND work_item_id=$2`, "ws_fixture", workID); outcome != "SUCCEEDED" {
		t.Fatalf("attempt outcome=%q", outcome)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='work.item.state' AND detail->>'work_item_id'=$3`, "ws_fixture", task.Id, workID); got == 0 {
		t.Fatal("no work item state audit event")
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.outbox WHERE workspace_id=$1 AND task_id=$2 AND event_type='work.item.state'`, "ws_fixture", task.Id); got == 0 {
		t.Fatal("no work item state outbox row")
	}
}

func TestProposalWorkerCrashAfterClaimRecoversThenDispatches(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000702"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	ctx := context.Background()
	claim, err := s.ClaimReadyWorkItem(ctx, "worker_crashed", 20*time.Millisecond)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	time.Sleep(40 * time.Millisecond)
	adapter := &proposalWorkAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "recovered_receipt"}}
	worker := proposalWorkWorker(t, s, adapter, WorkerSteps{Proposal: true})
	proposalWorkTick(t, worker)
	if adapter.callCount() != 1 {
		t.Fatalf("adapter calls=%d, want 1", adapter.callCount())
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("executions=%d, want 1", got)
	}
	_, state, _, _, _ := proposalWorkItemRow(t, s, task.Id)
	if state != "WAITING_RESULT" {
		t.Fatalf("state=%q, want WAITING_RESULT", state)
	}
}

func TestProposalWorkerResumesPersistedReserveWithoutSecondExecution(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000703"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	ctx := context.Background()
	claim, err := s.ClaimReadyWorkItem(ctx, "worker_reserve_crash", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	reservation, err := s.ReserveWorkItem(ctx, *claim)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Simulate a crash between reserve and AcceptOperation: execution stays CREATED.
	adapter := &proposalWorkAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "resumed_receipt"}}
	worker := proposalWorkWorker(t, s, adapter, WorkerSteps{Proposal: true})
	proposalWorkTick(t, worker)
	if adapter.callCount() != 1 {
		t.Fatalf("adapter calls=%d, want 1", adapter.callCount())
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("executions=%d, want 1 (resume must reuse the attempt)", got)
	}
	if op := fixtureString(t, s, `SELECT operation_id FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); op != reservation.OperationID {
		t.Fatalf("resumed operation=%q, want %q", op, reservation.OperationID)
	}
}

func TestProposalWorkerCrashAfterDispatchBecomesUnknownWithoutResend(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000704"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	ctx := context.Background()
	claim, err := s.ClaimReadyWorkItem(ctx, "worker_dispatch_crash", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	reservation, err := s.ReserveWorkItem(ctx, *claim)
	if err != nil {
		t.Fatal(err)
	}
	rt := fixtureRuntime("acc_pw", "rt_pw")
	if _, err := s.AcceptOperation(ctx, rt, reservation); err != nil {
		t.Fatal(err)
	}
	permit, err := s.IssuePermit(ctx, rt, reservation.OperationID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the durable state a consumed permit leaves when the process dies
	// inside the adapter call: the bridge ledger is DISPATCHING.
	if _, err := s.Pool.Exec(ctx, `UPDATE core.dispatch_permit SET state='consumed' WHERE workspace_id=$1 AND id=$2`,
		"ws_fixture", permit.ID); err != nil {
		t.Fatalf("seed consumed permit: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE bridge.gateway_operation SET state='DISPATCHING',permit_id=$3,disposition=NULL WHERE workspace_id=$1 AND operation_id=$2`,
		"ws_fixture", reservation.OperationID, permit.ID); err != nil {
		t.Fatalf("seed dispatching bridge row: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE core.execution SET state='DISPATCHING',effect_certainty='unknown',disposition=NULL WHERE workspace_id=$1 AND operation_id=$2`,
		"ws_fixture", reservation.OperationID); err != nil {
		t.Fatalf("seed dispatching execution: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE core.account_action_lock SET permit_id=$2 WHERE workspace_id=$1 AND account_id='acc_pw' AND action_family='group.join'`,
		"ws_fixture", permit.ID); err != nil {
		t.Fatalf("seed action lock permit: %v", err)
	}
	if _, err := s.RecoverDispatching(ctx, rt); err != nil {
		t.Fatalf("recover dispatching: %v", err)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND id=$2`, "ws_fixture", claim.WorkItemID); state != "UNKNOWN" {
		t.Fatalf("work item state=%q, want UNKNOWN", state)
	}
	adapter := &proposalWorkAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "never"}}
	worker := proposalWorkWorker(t, s, adapter, WorkerSteps{Proposal: true, Outbox: true, Inbox: true})
	proposalWorkTick(t, worker)
	proposalWorkTick(t, worker)
	if adapter.callCount() != 0 {
		t.Fatalf("adapter calls=%d after UNKNOWN, want 0 (never resend)", adapter.callCount())
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("executions=%d, want 1", got)
	}
}

func TestProposalUnknownOutcomeIsNotRetried(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000705"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	adapter := &proposalWorkAdapter{err: errors.New("transport ambiguous after write")}
	worker := proposalWorkWorker(t, s, adapter, WorkerSteps{Proposal: true})
	proposalWorkTick(t, worker)
	if adapter.callCount() != 1 {
		t.Fatalf("adapter calls=%d, want 1", adapter.callCount())
	}
	state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id)
	if state != "UNKNOWN" {
		t.Fatalf("state=%q, want UNKNOWN", state)
	}
	task, err := s.GetTask(context.Background(), fixtureActor(), task.Id)
	if err != nil {
		t.Fatal(err)
	}
	control, err := s.ControlTask(context.Background(), fixtureActor(), task.Id, newID("pause"),
		contract.TaskControlRequest{Action: contract.TaskControlRequestActionPause, ExpectedVersion: task.Version, Reason: "pause after unknown"})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	_ = control
	proposalWorkTick(t, worker)
	if adapter.callCount() != 1 {
		t.Fatalf("adapter calls=%d after unknown, want 1 (no resend)", adapter.callCount())
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.work_item WHERE workspace_id=$1 AND task_id=$2 AND state='READY'`, "ws_fixture", task.Id); got != 0 {
		t.Fatalf("unknown returned %d work items to READY", got)
	}
}

// ---------------------------------------------------------------------------
// legacy compatibility / isolation

func TestProposalModeIsolatedFromLegacyScheduling(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000801"})
	proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	ctx := context.Background()
	if _, err := s.Reserve(ctx, "ws_fixture", task.Id, ""); Code(err) != schedCodeNoWork {
		t.Fatalf("legacy reserve on proposal task code=%q, want %q", Code(err), schedCodeNoWork)
	}
	adapter := &proposalWorkAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "legacy_skip"}}
	legacy := proposalWorkWorker(t, s, adapter, WorkerSteps{Dispatch: true})
	proposalWorkTick(t, legacy)
	if adapter.callCount() != 0 {
		t.Fatalf("legacy dispatch step called adapter %d times for proposal task", adapter.callCount())
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 0 {
		t.Fatalf("legacy worker created %d executions for proposal task", got)
	}

	// A legacy task is untouched by the proposal consumer.
	seedAccount(t, s, "acc_legacy", "rt_legacy", 5)
	legacyTask := createReadyTask(t, s, []string{"acc_legacy"}, []string{"9000802"}, 1, 3)
	proposalOnly := proposalWorkWorker(t, s, adapter, WorkerSteps{Proposal: true})
	proposalWorkTick(t, proposalOnly)
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.work_item WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", legacyTask.Id); got != 0 {
		t.Fatalf("proposal consumer created %d work items for legacy task", got)
	}
	if got := proposalWorkCount(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", legacyTask.Id); got != 0 {
		t.Fatalf("proposal consumer created %d executions for legacy task", got)
	}
	// The proposal consumer may drain the proposal task's own READY item; the
	// isolation claim is that it never touches the legacy task, proven above by
	// zero work items and zero executions there.
}

// ---------------------------------------------------------------------------
// worker feature gate

func TestProposalWorkerRequiresExplicitFeature(t *testing.T) {
	s := fixtureStore(t)
	if _, err := NewWorker(s, WorkerConfig{WorkerID: "no_gate", Steps: WorkerSteps{Proposal: true}}); err == nil {
		t.Fatal("proposal step enabled without ProposalExecution was accepted")
	}
	worker, err := NewWorker(s, WorkerConfig{WorkerID: "gated", Steps: WorkerSteps{Proposal: true}, ProposalExecution: true})
	if err != nil {
		t.Fatalf("gated worker: %v", err)
	}
	if worker == nil {
		t.Fatal("nil worker")
	}
}

// ---------------------------------------------------------------------------
// HTTP surface

func TestOrchestrationHTTPRoutesAndFeatureGate(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID, proposalID, digest := proposalWorkSetup(t, s, 1, []string{"9000901"})
	handler := s.OrchestrationHandler(func(*http.Request) (Actor, error) { return fixtureActor(), nil }, true)
	if code := func() string {
		rec := ttServe(handler, http.MethodPost, "/v1/tasks/"+task.Id+"/orchestration",
			ttJSONBody(t, map[string]any{"expected_version": task.Version, "mode": "proposal", "reason": "http"}),
			map[string]string{"Content-Type": "application/json", "Idempotency-Key": newID("key")})
		if rec.Code != http.StatusAccepted {
			return ttErrorCode(t, rec)
		}
		return ""
	}(); code != "" {
		t.Fatalf("orchestration HTTP error=%q", code)
	}
	received := proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	_ = received
	materialization := proposalWorkMaterialize(t, s, task, approvalID, proposalID, digest)
	rec := ttServe(handler, http.MethodGet, "/v1/tasks/"+task.Id+"/work-items", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("work-items HTTP=%d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Data struct {
			Items []WorkItemView `json:"items"`
		} `json:"data"`
		Meta struct {
			HasMore bool `json:"has_more"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode work-items: %v", err)
	}
	if len(page.Data.Items) != 1 || page.Data.Items[0].MaterializationId != materialization.ResourceId {
		t.Fatalf("work-items page=%+v", page)
	}
	rec = ttServe(handler, http.MethodPost, "/v1/tasks/"+task.Id+"/plan-proposals/"+proposalID+"/materialize",
		ttJSONBody(t, map[string]any{"expected_version": task.Version, "approval_ref": approvalID, "proposal_digest": digest}),
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": newID("key")})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("materialize HTTP=%d body=%s", rec.Code, rec.Body.String())
	}
	var materialized contract.ReceiptEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &materialized); err != nil {
		t.Fatalf("decode materialize receipt: %v", err)
	}
	if materialized.Data.ResourceId != materialization.ResourceId || materialized.Data.Action != "materialize" {
		t.Fatalf("materialize receipt=%+v", materialized.Data)
	}
	disabled := s.OrchestrationHandler(func(*http.Request) (Actor, error) { return fixtureActor(), nil }, false)
	rec = ttServe(disabled, http.MethodGet, "/v1/tasks/"+task.Id+"/work-items", nil, nil)
	if rec.Code != http.StatusForbidden || ttErrorCode(t, rec) != taskCodeFeatureDisabled {
		t.Fatalf("disabled handler HTTP=%d code=%q", rec.Code, ttErrorCode(t, rec))
	}
	rec = ttServe(handler, http.MethodDelete, "/v1/tasks/"+task.Id+"/work-items", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete HTTP=%d, want 405", rec.Code)
	}
	rec = ttServe(handler, http.MethodPost, "/v1/tasks/"+task.Id+"/orchestration",
		ttJSONBody(t, map[string]any{"expected_version": task.Version, "mode": "bogus", "reason": "bad mode"}),
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": newID("key")})
	if rec.Code != http.StatusBadRequest || ttErrorCode(t, rec) != taskCodeValidation {
		t.Fatalf("bad mode HTTP=%d code=%q", rec.Code, ttErrorCode(t, rec))
	}
}
