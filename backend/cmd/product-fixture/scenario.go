package main

// Scenario mode drives the real proposal path on the seeded proposal task:
// template proposal -> approval -> materialize (repeated keys) -> the existing
// proposal worker claim/reserve/permit/bridge -> adapter boundary -> trusted
// observation -> work/attempt/application/execution/task projection. It also
// proves UNKNOWN is never resent and that the legacy scheduler cannot expand a
// proposal-mode task. No execution is inserted directly and no second sender
// exists.

import (
	"context"
	"errors"
	"sync"
	"time"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
)

type check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

type workChain struct {
	WorkItemID     string
	StepID         string
	ApplicationID  string
	ProposalID     string
	State          string
	ExecutionID    string
	OperationID    string
	AccountID      string
	TargetID       string
	AttemptCount   int
	AttemptNo      int
	AttemptOutcome string
	ObservationID  string
}

type idempotencyView struct {
	SameKeyMaterialization string `json:"same_key_materialization"`
	NewKeyMaterialization  string `json:"new_key_materialization"`
	WorkItems              int    `json:"work_items"`
	Applications           int    `json:"applications"`
	ExecutionsBeforeWorker int    `json:"executions_before_worker"`
	PermitsBeforeWorker    int    `json:"permits_before_worker"`
	ClaimsBeforeWorker     int    `json:"claims_before_worker"`
}

type unknownView struct {
	ProposalID        string `json:"proposal_id"`
	MaterializationID string `json:"materialization_id"`
	WorkItemID        string `json:"work_item_id"`
	ExecutionID       string `json:"execution_id"`
	OperationID       string `json:"operation_id"`
	WorkState         string `json:"work_state"`
	ExecutionState    string `json:"execution_state"`
	AdapterCalls      int    `json:"adapter_calls"`
	AdapterCallsAfter int    `json:"adapter_calls_after_retick"`
	ExecutionsTotal   int    `json:"executions_total"`
	ReadyWorkItems    int    `json:"ready_work_items"`
	UnknownWorkItems  int    `json:"unknown_work_items"`
}

type modeSeparationView struct {
	LegacyReserveCode        string `json:"legacy_reserve_code"`
	ModeSwitchCode           string `json:"mode_switch_code"`
	LegacyWorkerAdapterCalls int    `json:"legacy_worker_adapter_calls"`
	LegacyWorkerExecutions   int    `json:"legacy_worker_executions"`
}

type scenarioTrace struct {
	TaskID            string `json:"task_id"`
	ApprovalRef       string `json:"approval_ref"`
	ProposalID        string `json:"proposal_id"`
	ProposalDigest    string `json:"proposal_digest"`
	MaterializationID string `json:"materialization_id"`
	ApplicationID     string `json:"application_id"`
	StepID            string `json:"step_id"`
	WorkItemID        string `json:"work_item_id"`
	AttemptNo         int    `json:"attempt_no"`
	ExecutionID       string `json:"execution_id"`
	OperationID       string `json:"operation_id"`
	ObservationID     string `json:"observation_event_id"`
	AccountID         string `json:"account_id"`
	TargetID          string `json:"target_id"`
}

type scenarioResult struct {
	SchemaVersion      int                  `json:"schema_version"`
	TaskID             string               `json:"task_id"`
	ApprovalRef        string               `json:"approval_ref"`
	ProposalID         string               `json:"proposal_id"`
	ProposalDigest     string               `json:"proposal_digest"`
	MaterializationID  string               `json:"materialization_id"`
	ApplicationID      string               `json:"application_id"`
	WorkItemID         string               `json:"work_item_id"`
	StepID             string               `json:"step_id"`
	ExecutionID        string               `json:"execution_id"`
	OperationID        string               `json:"operation_id"`
	AttemptNo          int                  `json:"attempt_no"`
	ObservationEventID string               `json:"observation_event_id"`
	WorkItemState      string               `json:"work_item_state"`
	AttemptOutcome     string               `json:"attempt_outcome"`
	TaskState          string               `json:"task_state"`
	TaskVersion        int                  `json:"task_version"`
	ConfirmedResults   int                  `json:"confirmed_results"`
	Idempotency        idempotencyView      `json:"idempotency"`
	Unknown            unknownView          `json:"unknown"`
	ModeSeparation     modeSeparationView   `json:"mode_separation"`
	Trace              scenarioTrace        `json:"trace"`
	Graph              *graphScenarioResult `json:"graph,omitempty"`
	Checks             []check              `json:"checks"`
}

type countingAdapter struct {
	mu      sync.Mutex
	calls   int
	outcome taskengine.SendOutcome
	err     error
}

func (a *countingAdapter) RequestJoin(context.Context, taskengine.Reservation) (taskengine.SendOutcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.outcome, a.err
}

func (a *countingAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func addCheck(checks *[]check, name string) {
	*checks = append(*checks, check{Name: name, OK: true})
}

func runScenario(ctx context.Context, store *taskengine.Store, schemaVersion int, artexFixture, requireCollectorSummary bool) (*scenarioResult, error) {
	require(scalarInt(ctx, store, `SELECT count(*) FROM core.workspace WHERE id=$1`, workspaceID) == 1,
		"workspace %q is not seeded; run -mode seed first", workspaceID)
	actor := operatorActor()
	out := &scenarioResult{SchemaVersion: schemaVersion, Checks: []check{}}

	taskID := scalarString(ctx, store, `SELECT id FROM core.task WHERE workspace_id=$1 AND approval_id=$2`,
		workspaceID, proposalApprovalID)
	task, err := store.GetTask(ctx, actor, taskID)
	must(err, "read proposal task")
	require(task.State == contract.TaskState("RUNNING"), "proposal task state=%s, want RUNNING", task.State)
	require(string(task.ExecutionMode) == "live", "proposal task mode=%s, want live", task.ExecutionMode)
	require(string(task.OrchestrationMode) == "proposal", "proposal task orchestration=%s, want proposal", task.OrchestrationMode)
	out.TaskID = taskID
	out.Checks = append(out.Checks, check{Name: "seeded proposal task is RUNNING/live/proposal", OK: true})

	// -- create, approve and materialize the first proposal
	proposalRequest := contract.PlanProposalRequest{
		TaskId:          contract.Id(taskID),
		ExpectedVersion: task.Version,
		ScopeRevision:   task.ScopeRevision,
		ProposalKind:    contract.PlanProposalRequestProposalKindUseTemplate,
		Reason:          "product fixture proposal scenario",
	}
	created, err := store.CreatePlanProposal(ctx, actor, taskID, "fixture-proposal-create-main", proposalRequest)
	must(err, "create proposal")
	replayedProposal, err := store.CreatePlanProposal(ctx, actor, taskID, "fixture-proposal-create-main", proposalRequest)
	must(err, "replay proposal create with same key")
	require(replayedProposal.ResourceId == created.ResourceId,
		"proposal replay created a second proposal: %q != %q", replayedProposal.ResourceId, created.ResourceId)
	proposalID := string(created.ResourceId)
	out.ProposalID = proposalID
	addCheck(&out.Checks, "template proposal created once through Store.CreatePlanProposal (same-key replay is identical)")

	_, err = store.ApprovePlanProposal(ctx, actor, taskID, proposalID, "fixture-proposal-approve-main",
		taskengine.PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: proposalApprovalID})
	must(err, "approve proposal")
	addCheck(&out.Checks, "proposal approved against the seeded operator approval")

	view, err := store.GetPlanProposal(ctx, actor, taskID, proposalID)
	must(err, "read proposal")
	proposalDigest := string(view.Digest)
	out.ProposalDigest = proposalDigest
	out.ApprovalRef = proposalApprovalID

	materialize := taskengine.MaterializeRequest{
		ExpectedVersion: task.Version,
		ApprovalRef:     proposalApprovalID,
		ProposalDigest:  proposalDigest,
	}
	first, err := store.MaterializePlanProposal(ctx, actor, taskID, proposalID, "fixture-proposal-materialize-main", materialize)
	must(err, "materialize proposal")
	out.MaterializationID = string(first.ResourceId)
	second, err := store.MaterializePlanProposal(ctx, actor, taskID, proposalID, "fixture-proposal-materialize-main", materialize)
	must(err, "repeat materialize with same key")
	third, err := store.MaterializePlanProposal(ctx, actor, taskID, proposalID, "fixture-proposal-materialize-main-replay", materialize)
	must(err, "repeat materialize with a fresh key")
	require(second.ResourceId == first.ResourceId && third.ResourceId == first.ResourceId,
		"materialize replay changed the resource: %q %q %q", first.ResourceId, second.ResourceId, third.ResourceId)
	out.Idempotency.SameKeyMaterialization = string(second.ResourceId)
	out.Idempotency.NewKeyMaterialization = string(third.ResourceId)
	out.Idempotency.WorkItems = scalarInt(ctx, store, `SELECT count(*) FROM core.work_item WHERE workspace_id=$1 AND task_id=$2`, workspaceID, taskID)
	out.Idempotency.Applications = scalarInt(ctx, store, `SELECT count(*) FROM core.plan_application WHERE workspace_id=$1 AND task_id=$2`, workspaceID, taskID)
	out.Idempotency.ExecutionsBeforeWorker = scalarInt(ctx, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, workspaceID, taskID)
	out.Idempotency.PermitsBeforeWorker = scalarInt(ctx, store, `SELECT count(*) FROM core.dispatch_permit p JOIN core.execution e ON (e.workspace_id,e.id)=(p.workspace_id,p.execution_id) WHERE e.workspace_id=$1 AND e.task_id=$2`, workspaceID, taskID)
	out.Idempotency.ClaimsBeforeWorker = scalarInt(ctx, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND task_id=$2`, workspaceID, taskID)
	require(out.Idempotency.WorkItems == 1 && out.Idempotency.Applications == 1,
		"repeat materialize created work=%d applications=%d, want 1/1", out.Idempotency.WorkItems, out.Idempotency.Applications)
	require(out.Idempotency.ExecutionsBeforeWorker == 0 && out.Idempotency.PermitsBeforeWorker == 0 && out.Idempotency.ClaimsBeforeWorker == 0,
		"materialize produced external state executions=%d permits=%d claims=%d",
		out.Idempotency.ExecutionsBeforeWorker, out.Idempotency.PermitsBeforeWorker, out.Idempotency.ClaimsBeforeWorker)
	addCheck(&out.Checks, "materialize is idempotent per key and per proposal, writes no execution/permit/claim")

	// -- the legacy scheduler must not expand a proposal-mode task
	legacyAdapter := &countingAdapter{outcome: taskengine.SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "legacy_must_not_send"}}
	legacyWorker, err := taskengine.NewWorker(store, taskengine.WorkerConfig{
		WorkerID: "product-fixture-legacy", BatchSize: 10, Steps: taskengine.WorkerSteps{Dispatch: true},
		Transport: taskengine.WorkerExternalTransport, AllowExternalWrites: true, Adapter: legacyAdapter,
	})
	must(err, "new legacy worker")
	_, err = legacyWorker.Tick(ctx)
	must(err, "legacy worker tick")
	require(legacyAdapter.count() == 0, "legacy worker called the adapter %d times for a proposal task", legacyAdapter.count())
	out.ModeSeparation.LegacyWorkerAdapterCalls = legacyAdapter.count()
	out.ModeSeparation.LegacyWorkerExecutions = scalarInt(ctx, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, workspaceID, taskID)
	require(out.ModeSeparation.LegacyWorkerExecutions == 0, "legacy worker created %d executions for a proposal task", out.ModeSeparation.LegacyWorkerExecutions)
	addCheck(&out.Checks, "legacy dispatch step ignores the proposal-mode task (adapter calls=0)")

	legacyReserveErr := func() error {
		_, err := store.Reserve(ctx, workspaceID, taskID, "")
		return err
	}()
	out.ModeSeparation.LegacyReserveCode = taskengine.Code(legacyReserveErr)
	require(out.ModeSeparation.LegacyReserveCode == "NO_WORK", "legacy Reserve on proposal task code=%q, want NO_WORK", out.ModeSeparation.LegacyReserveCode)
	addCheck(&out.Checks, "legacy Reserve refuses the proposal-mode task with NO_WORK")

	// -- the single proposal worker claims -> reserves -> permits -> dispatches
	adapter := &countingAdapter{outcome: taskengine.SendOutcome{
		Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fixture_proposal_receipt",
	}}
	worker, err := taskengine.NewWorker(store, taskengine.WorkerConfig{
		WorkerID: "product-fixture-proposal", BatchSize: 10,
		Steps: taskengine.WorkerSteps{Proposal: true}, ProposalExecution: true,
		Transport: taskengine.WorkerExternalTransport, AllowExternalWrites: true, Adapter: adapter,
	})
	must(err, "new proposal worker")
	tick, err := worker.Tick(ctx)
	must(err, "proposal worker tick")
	require(tick.ProposalClaimed == 1 && tick.ProposalReserved == 1 && tick.ProposalDispatched == 1,
		"proposal tick claimed=%d reserved=%d dispatched=%d, want 1/1/1",
		tick.ProposalClaimed, tick.ProposalReserved, tick.ProposalDispatched)
	require(adapter.count() == 1, "adapter calls=%d, want 1", adapter.count())
	addCheck(&out.Checks, "proposal worker claimed/reserved/permitted/dispatched through the existing bridge")

	firstChain := loadWorkChain(ctx, store, taskID, false)
	require(firstChain.State == "WAITING_RESULT" && firstChain.ExecutionID != "" && firstChain.OperationID != "",
		"first work chain state=%q execution=%q operation=%q", firstChain.State, firstChain.ExecutionID, firstChain.OperationID)
	require(firstChain.AttemptCount == 1 && firstChain.AttemptNo == 1,
		"first attempt count=%d attempt_no=%d", firstChain.AttemptCount, firstChain.AttemptNo)
	out.WorkItemID = firstChain.WorkItemID
	out.StepID = firstChain.StepID
	out.ApplicationID = firstChain.ApplicationID
	out.ExecutionID = firstChain.ExecutionID
	out.OperationID = firstChain.OperationID
	out.AttemptNo = firstChain.AttemptNo
	addCheck(&out.Checks, "work item is WAITING_RESULT with one attempt linked to the exact execution")

	// -- trusted observation for exactly that operation
	runtimeID := scalarString(ctx, store, `SELECT runtime_id FROM core.account WHERE workspace_id=$1 AND id=$2`,
		workspaceID, firstChain.AccountID)
	observation := taskengine.Observation{
		EventID:          "evt_" + firstChain.ExecutionID,
		OperationID:      firstChain.OperationID,
		AccountID:        firstChain.AccountID,
		TargetID:         firstChain.TargetID,
		Kind:             "membership.observed",
		State:            "joined",
		EvidenceRef:      "evidence_" + firstChain.ExecutionID,
		SourceEpoch:      1,
		SourceSequence:   nextObservationSequence(ctx, store, runtimeID, firstChain.AccountID),
		ObservedAt:       time.Now().UTC().Add(2 * time.Second),
		SnapshotComplete: true,
	}
	runtimeActor := taskengine.RuntimeActor{
		WorkspaceID: workspaceID, RuntimeID: runtimeID, OwnerEpoch: 1, AccountIDs: []string{firstChain.AccountID},
	}
	must(store.ReceiveObservation(ctx, runtimeActor, observation), "receive trusted observation")
	must(store.ApplyObservation(ctx, workspaceID, runtimeID, observation.EventID), "apply trusted observation")
	out.ObservationEventID = observation.EventID

	applied := loadWorkChain(ctx, store, taskID, false)
	out.WorkItemState = applied.State
	out.AttemptOutcome = applied.AttemptOutcome
	require(applied.State == "SUCCEEDED", "observed work item state=%q, want SUCCEEDED", applied.State)
	require(applied.AttemptOutcome == "SUCCEEDED", "observed attempt outcome=%q, want SUCCEEDED", applied.AttemptOutcome)
	require(applied.ObservationID == observation.EventID, "attempt observation=%q, want %q", applied.ObservationID, observation.EventID)
	require(scalarInt(ctx, store, `SELECT count(*) FROM core.result
		WHERE workspace_id=$1 AND task_id=$2 AND execution_id=$3 AND target_id=$4 AND account_id=$5 AND valid`,
		workspaceID, taskID, firstChain.ExecutionID, firstChain.TargetID, firstChain.AccountID) == 1,
		"trusted result is not linked to the exact execution/target/account")
	require(scalarInt(ctx, store, `SELECT count(*) FROM core.plan_application
		WHERE workspace_id=$1 AND id=$2 AND proposal_id=$3 AND target_id=$4 AND account_id=$5`,
		workspaceID, firstChain.ApplicationID, proposalID, firstChain.TargetID, firstChain.AccountID) == 1,
		"application chain does not match the executed step")
	addCheck(&out.Checks, "trusted observation projected SUCCEEDED onto work item, attempt, application, result and task")
	if artexFixture {
		graph, err := runGraphScenario(ctx, store, actor, requireCollectorSummary)
		must(err, "graph scenario")
		out.Graph = graph
	} else {
		out.Graph = &graphScenarioResult{Status: "skipped", Reason: "ARTEX protocol fixture not requested (-artex-fixture)"}
	}

	// -- second approved proposal exercises UNKNOWN without a resend
	task, err = store.GetTask(ctx, actor, taskID)
	must(err, "read task before second proposal")
	secondRequest := proposalRequest
	secondRequest.ExpectedVersion = task.Version
	secondRequest.Reason = "product fixture unknown outcome scenario"
	createdUnknown, err := store.CreatePlanProposal(ctx, actor, taskID, "fixture-proposal-create-unknown", secondRequest)
	must(err, "create second proposal")
	unknownProposalID := string(createdUnknown.ResourceId)
	_, err = store.ApprovePlanProposal(ctx, actor, taskID, unknownProposalID, "fixture-proposal-approve-unknown",
		taskengine.PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: proposalApprovalID})
	must(err, "approve second proposal")
	unknownView, err := store.GetPlanProposal(ctx, actor, taskID, unknownProposalID)
	must(err, "read second proposal")
	unknownDigest := string(unknownView.Digest)
	unknownMaterialization, err := store.MaterializePlanProposal(ctx, actor, taskID, unknownProposalID,
		"fixture-proposal-materialize-unknown", taskengine.MaterializeRequest{
			ExpectedVersion: task.Version, ApprovalRef: proposalApprovalID, ProposalDigest: unknownDigest,
		})
	must(err, "materialize second proposal")
	out.Unknown.ProposalID = unknownProposalID
	out.Unknown.MaterializationID = string(unknownMaterialization.ResourceId)

	secondChain := loadWorkChain(ctx, store, taskID, true)
	require(secondChain.State == "READY" && secondChain.ApplicationID != "",
		"second work chain state=%q application=%q, want READY", secondChain.State, secondChain.ApplicationID)
	out.Unknown.WorkItemID = secondChain.WorkItemID

	// A live READY item may not be abandoned by switching the task back to the
	// legacy expander.
	task, err = store.GetTask(ctx, actor, taskID)
	must(err, "read task before mode switch probe")
	_, switchErr := store.SetOrchestrationMode(ctx, actor, taskID, "fixture-mode-legacy-probe",
		taskengine.OrchestrationModeRequest{ExpectedVersion: task.Version, Mode: "legacy", Reason: "probe pending work"})
	out.ModeSeparation.ModeSwitchCode = taskengine.Code(switchErr)
	require(out.ModeSeparation.ModeSwitchCode == "STATE_CONFLICT",
		"mode switch with READY proposal work code=%q, want STATE_CONFLICT", out.ModeSeparation.ModeSwitchCode)
	addCheck(&out.Checks, "orchestration mode switch is refused while proposal work is queued")

	errAdapter := &countingAdapter{err: errors.New("fixture adapter ambiguous after write")}
	unknownWorker, err := taskengine.NewWorker(store, taskengine.WorkerConfig{
		WorkerID: "product-fixture-unknown", BatchSize: 10,
		Steps: taskengine.WorkerSteps{Proposal: true}, ProposalExecution: true,
		Transport: taskengine.WorkerExternalTransport, AllowExternalWrites: true, Adapter: errAdapter,
	})
	unknownTick, tickErr := unknownWorker.Tick(ctx)
	require(tickErr == nil, "durably recorded unknown outcome returned worker error: %v", tickErr)
	require(unknownTick.ProposalDispatched == 1 && errAdapter.count() == 1,
		"unknown tick dispatched=%d adapter_calls=%d", unknownTick.ProposalDispatched, errAdapter.count())
	out.Unknown.AdapterCalls = errAdapter.count()
	out.Unknown.WorkState = scalarString(ctx, store, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND id=$2`, workspaceID, secondChain.WorkItemID)
	out.Unknown.ExecutionID = scalarString(ctx, store, `SELECT execution_id FROM core.work_item WHERE workspace_id=$1 AND id=$2`, workspaceID, secondChain.WorkItemID)
	out.Unknown.OperationID = scalarString(ctx, store, `SELECT operation_id FROM core.execution WHERE workspace_id=$1 AND id=$2`, workspaceID, out.Unknown.ExecutionID)
	out.Unknown.ExecutionState = scalarString(ctx, store, `SELECT state FROM core.execution WHERE workspace_id=$1 AND id=$2`, workspaceID, out.Unknown.ExecutionID)
	require(out.Unknown.WorkState == "UNKNOWN" && out.Unknown.ExecutionState == "UNKNOWN",
		"unknown outcome work=%q execution=%q, want UNKNOWN/UNKNOWN", out.Unknown.WorkState, out.Unknown.ExecutionState)
	addCheck(&out.Checks, "ambiguous adapter boundary produced durable UNKNOWN for the exact execution")

	recorder := &countingAdapter{outcome: taskengine.SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "must_not_resend"}}
	replayWorker, err := taskengine.NewWorker(store, taskengine.WorkerConfig{
		WorkerID: "product-fixture-unknown-replay", BatchSize: 10,
		Steps: taskengine.WorkerSteps{Proposal: true}, ProposalExecution: true,
		Transport: taskengine.WorkerExternalTransport, AllowExternalWrites: true, Adapter: recorder,
	})
	must(err, "new replay proposal worker")
	_, err = replayWorker.Tick(ctx)
	must(err, "proposal replay tick")
	_, err = replayWorker.Tick(ctx)
	must(err, "second proposal replay tick")
	out.Unknown.AdapterCallsAfter = recorder.count()
	require(out.Unknown.AdapterCallsAfter == 0, "UNKNOWN work called the adapter %d more times", out.Unknown.AdapterCallsAfter)
	out.Unknown.ExecutionsTotal = scalarInt(ctx, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, workspaceID, taskID)
	out.Unknown.ReadyWorkItems = scalarInt(ctx, store, `SELECT count(*) FROM core.work_item WHERE workspace_id=$1 AND task_id=$2 AND state='READY'`, workspaceID, taskID)
	out.Unknown.UnknownWorkItems = scalarInt(ctx, store, `SELECT count(*) FROM core.work_item WHERE workspace_id=$1 AND task_id=$2 AND state='UNKNOWN'`, workspaceID, taskID)
	require(out.Unknown.ExecutionsTotal == 2, "executions after unknown=%d, want exactly 2", out.Unknown.ExecutionsTotal)
	require(out.Unknown.ReadyWorkItems == 0 && out.Unknown.UnknownWorkItems == 1,
		"work after unknown ready=%d unknown=%d, want 0/1", out.Unknown.ReadyWorkItems, out.Unknown.UnknownWorkItems)
	addCheck(&out.Checks, "UNKNOWN work is never resent and is never returned to READY")

	task, err = store.GetTask(ctx, actor, taskID)
	must(err, "read final task")
	out.TaskState = string(task.State)
	out.TaskVersion = task.Version
	out.ConfirmedResults = task.ConfirmedUniqueCount
	require(out.ConfirmedResults == 1, "confirmed results=%d, want 1", out.ConfirmedResults)
	out.Trace = scenarioTrace{
		TaskID: taskID, ApprovalRef: proposalApprovalID, ProposalID: proposalID,
		ProposalDigest: proposalDigest, MaterializationID: string(first.ResourceId),
		ApplicationID: firstChain.ApplicationID, StepID: firstChain.StepID,
		WorkItemID: firstChain.WorkItemID, AttemptNo: firstChain.AttemptNo,
		ExecutionID: firstChain.ExecutionID, OperationID: firstChain.OperationID,
		ObservationID: observation.EventID, AccountID: firstChain.AccountID, TargetID: firstChain.TargetID,
	}
	addCheck(&out.Checks, "proposal→materialization→work→attempt→execution→observation→task trace is exact")

	return out, nil
}

func loadWorkChain(ctx context.Context, store *taskengine.Store, taskID string, newest bool) workChain {
	order := "ASC"
	if newest {
		order = "DESC"
	}
	query := `
		SELECT w.id,w.step_id,w.application_id,w.proposal_id,w.state,COALESCE(w.execution_id,''),
		       COALESCE(e.operation_id,''),w.account_id,w.target_id,w.attempt_count,
		       COALESCE((SELECT a.attempt_no FROM core.work_item_attempt a
		                 WHERE a.workspace_id=w.workspace_id AND a.work_item_id=w.id
		                 ORDER BY a.attempt_no DESC LIMIT 1),0),
		       COALESCE((SELECT a.outcome FROM core.work_item_attempt a
		                 WHERE a.workspace_id=w.workspace_id AND a.work_item_id=w.id
		                 ORDER BY a.attempt_no DESC LIMIT 1),''),
		       COALESCE((SELECT a.observation_event_id FROM core.work_item_attempt a
		                 WHERE a.workspace_id=w.workspace_id AND a.work_item_id=w.id
		                 ORDER BY a.attempt_no DESC LIMIT 1),'')
		FROM core.work_item w
		LEFT JOIN core.execution e ON (e.workspace_id,e.id)=(w.workspace_id,w.execution_id)
		WHERE w.workspace_id=$1 AND w.task_id=$2
		ORDER BY w.created_at ` + order + `, w.id ` + order + ` LIMIT 1`
	var chain workChain
	must(store.Pool.QueryRow(ctx, query, workspaceID, taskID).Scan(
		&chain.WorkItemID, &chain.StepID, &chain.ApplicationID, &chain.ProposalID, &chain.State,
		&chain.ExecutionID, &chain.OperationID, &chain.AccountID, &chain.TargetID, &chain.AttemptCount,
		&chain.AttemptNo, &chain.AttemptOutcome, &chain.ObservationID), "read work item chain")
	return chain
}

func nextObservationSequence(ctx context.Context, store *taskengine.Store, runtimeID, accountID string) int64 {
	var sequence int64
	must(store.Pool.QueryRow(ctx, `
		SELECT COALESCE(max(source_sequence),0)+1 FROM core.inbox
		WHERE workspace_id=$1 AND source_runtime_id=$2 AND source_account_id=$3 AND source_epoch=1`,
		workspaceID, runtimeID, accountID).Scan(&sequence), "next observation sequence")
	if sequence < 1 {
		sequence = 1
	}
	return sequence
}
