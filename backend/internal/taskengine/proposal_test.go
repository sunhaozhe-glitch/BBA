package taskengine

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"bba.local/taskengine/internal/contract"
)

func proposalFixtureTask(t *testing.T, s *Store) (contract.Task, string) {
	t.Helper()
	seedAccount(t, s, "acc_proposal", "rt_proposal", 5)
	task := createReadyTask(t, s, []string{"acc_proposal"}, []string{"9000101"}, 1, 3)
	var approvalID string
	if err := s.Pool.QueryRow(context.Background(), `
		SELECT approval_id FROM core.task WHERE workspace_id=$1 AND id=$2`,
		"ws_fixture", task.Id).Scan(&approvalID); err != nil {
		t.Fatalf("read task approval: %v", err)
	}
	if approvalID == "" {
		t.Fatal("proposal fixture requires a task approval")
	}
	return task, approvalID
}

func proposalCreateFixture(t *testing.T, s *Store, task contract.Task, key string) contract.Receipt {
	t.Helper()
	request := contract.PlanProposalRequest{
		TaskId:          task.Id,
		ExpectedVersion: task.Version,
		ScopeRevision:   task.ScopeRevision,
		ProposalKind:    contract.PlanProposalRequestProposalKindUseTemplate,
		Reason:          "fixture bounded proposal",
	}
	receipt, err := s.CreatePlanProposal(context.Background(), fixtureActor(), task.Id, key, request)
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	if receipt.Action != contract.ReceiptActionRequestPlanProposal || receipt.ResourceId == "" {
		t.Fatalf("unexpected create receipt: %+v", receipt)
	}
	return receipt
}

func proposalApproveFixture(t *testing.T, s *Store, task contract.Task, proposalID, approvalID, key string) PlanProposalApprovalReceipt {
	t.Helper()
	receipt, err := s.ApprovePlanProposal(context.Background(), fixtureActor(), task.Id, proposalID, key, PlanProposalApprovalRequest{
		ExpectedVersion: task.Version,
		ApprovalRef:     approvalID,
	})
	if err != nil {
		t.Fatalf("approve proposal: %v", err)
	}
	if receipt.State != "approved" || receipt.ProposalId != proposalID || receipt.ApprovalRef != approvalID {
		t.Fatalf("unexpected approval receipt: %+v", receipt)
	}
	return receipt
}

func proposalDigestFixture(t *testing.T, s *Store, taskID, proposalID string) string {
	t.Helper()
	value := fixtureString(t, s, `
		SELECT digest FROM core.plan_proposal WHERE workspace_id=$1 AND task_id=$2 AND id=$3`,
		"ws_fixture", taskID, proposalID)
	if !planProposalDigestPattern.MatchString(value) {
		t.Fatalf("proposal digest is not canonical: %q", value)
	}
	return value
}

func TestPlanProposalLifecycleAndIdempotentApply(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID := proposalFixtureTask(t, s)
	ctx := context.Background()

	created := proposalCreateFixture(t, s, task, "proposal-create-1")
	proposalID := created.ResourceId
	duplicate := proposalCreateFixture(t, s, task, "proposal-create-2")
	if duplicate.ResourceId != proposalID {
		t.Fatalf("same task/version proposal duplicated: %q != %q", duplicate.ResourceId, proposalID)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.plan_proposal WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("proposal rows=%d, want 1", got)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.plan_proposal WHERE workspace_id=$1 AND id=$2`, "ws_fixture", proposalID); state != "pending" {
		t.Fatalf("proposal state=%q, want pending", state)
	}
	digest := proposalDigestFixture(t, s, task.Id, proposalID)
	if got := scalarInt(t, s, `SELECT jsonb_array_length(steps) FROM core.plan_proposal WHERE workspace_id=$1 AND id=$2`, "ws_fixture", proposalID); got != 1 {
		t.Fatalf("proposal step count=%d, want 1", got)
	}

	applyRequest := contract.PlanApplyRequest{
		ApprovalRef:     approvalID,
		ExpectedVersion: task.Version,
		ProposalDigest:  digest,
	}
	// A proposal cannot be applied without its durable approval binding.
	if _, err := s.ApplyPlanProposal(ctx, fixtureActor(), task.Id, proposalID, "proposal-apply-before-approval", applyRequest); Code(err) != taskCodeStateConflict {
		t.Fatalf("unapproved apply code=%q, want %q", Code(err), taskCodeStateConflict)
	}

	proposalApproveFixture(t, s, task, proposalID, approvalID, "proposal-approve-1")
	first, err := s.ApplyPlanProposal(ctx, fixtureActor(), task.Id, proposalID, "proposal-apply-1", applyRequest)
	if err != nil {
		t.Fatalf("apply proposal: %v", err)
	}
	if first.Action != contract.ReceiptActionApply || first.ResourceId != proposalID || first.ExpectedVersion != task.Version {
		t.Fatalf("unexpected apply receipt: %+v", first)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.plan_proposal WHERE workspace_id=$1 AND id=$2`, "ws_fixture", proposalID); state != "applied" {
		t.Fatalf("proposal state=%q, want applied", state)
	}
	if got := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id); got != task.Version+1 {
		t.Fatalf("task version=%d, want %d", got, task.Version+1)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.plan_application WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("application count=%d, want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='plan.proposal.applied'`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("applied event count=%d, want 1", got)
	}
	// Applying intent is not new evidence: a second proposal at the new version
	// must wait for a real execution/result instead of replanning forever.
	if _, err := s.CreatePlanProposal(ctx, fixtureActor(), task.Id, "proposal-after-apply", contract.PlanProposalRequest{
		TaskId:          task.Id,
		ExpectedVersion: task.Version + 1,
		ScopeRevision:   task.ScopeRevision,
		ProposalKind:    contract.PlanProposalRequestProposalKindUseTemplate,
		Reason:          "no new evidence yet",
	}); Code(err) != taskCodeStateConflict {
		t.Fatalf("post-apply create code=%q, want %q", Code(err), taskCodeStateConflict)
	}

	// Same key and body replay the original receipt without a second version
	// bump, application row or event.
	replay, err := s.ApplyPlanProposal(ctx, fixtureActor(), task.Id, proposalID, "proposal-apply-1", applyRequest)
	if err != nil {
		t.Fatalf("replay apply: %v", err)
	}
	if replay.Id != first.Id {
		t.Fatalf("replay changed receipt: first=%+v replay=%+v", first, replay)
	}
	if got := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id); got != task.Version+1 {
		t.Fatalf("replay mutated task version: %d", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.plan_application WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("replay duplicated application: %d", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='plan.proposal.applied'`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("replay duplicated applied event: %d", got)
	}

	// A different key against the already-applied proposal is a version
	// conflict, not a second application.
	if _, err := s.ApplyPlanProposal(ctx, fixtureActor(), task.Id, proposalID, "proposal-apply-2", applyRequest); Code(err) != taskCodeVersionConflict {
		t.Fatalf("second apply code=%q, want %q", Code(err), taskCodeVersionConflict)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.plan_application WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("second apply duplicated application: %d", got)
	}

	// Reusing the same idempotency key with a different body is rejected.
	changed := applyRequest
	changed.ProposalDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := s.ApplyPlanProposal(ctx, fixtureActor(), task.Id, proposalID, "proposal-apply-1", changed); Code(err) != taskCodeIdempotency {
		t.Fatalf("idempotency reuse code=%q, want %q", Code(err), taskCodeIdempotency)
	}
}

func TestPlanProposalApplyRejectsStaleVersion(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID := proposalFixtureTask(t, s)
	ctx := context.Background()

	created := proposalCreateFixture(t, s, task, "stale-create")
	proposalID := created.ResourceId
	proposalApproveFixture(t, s, task, proposalID, approvalID, "stale-approve")
	digest := proposalDigestFixture(t, s, task.Id, proposalID)

	// Any real task mutation advances the version. Pausing is enough; the
	// proposal must not apply against the new version.
	paused, err := s.ControlTask(ctx, fixtureActor(), task.Id, "stale-pause", contract.TaskControlRequest{
		Action:          contract.TaskControlRequestActionPause,
		ExpectedVersion: task.Version,
		Reason:          "fixture advances the task version",
	})
	if err != nil {
		t.Fatalf("pause task: %v", err)
	}
	if paused.ExpectedVersion != task.Version {
		t.Fatalf("pause receipt version=%d", paused.ExpectedVersion)
	}

	_, err = s.ApplyPlanProposal(ctx, fixtureActor(), task.Id, proposalID, "stale-apply", contract.PlanApplyRequest{
		ApprovalRef:     approvalID,
		ExpectedVersion: task.Version,
		ProposalDigest:  digest,
	})
	if Code(err) != taskCodeVersionConflict {
		t.Fatalf("stale apply code=%q, want %q", Code(err), taskCodeVersionConflict)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.plan_application WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 0 {
		t.Fatalf("stale apply wrote application: %d", got)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.plan_proposal WHERE workspace_id=$1 AND id=$2`, "ws_fixture", proposalID); state != "approved" {
		t.Fatalf("stale apply changed proposal state: %q", state)
	}
}

func TestPlanProposalApplyWritesNoExternalDispatch(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID := proposalFixtureTask(t, s)
	ctx := context.Background()

	created := proposalCreateFixture(t, s, task, "no-dispatch-create")
	proposalID := created.ResourceId
	proposalApproveFixture(t, s, task, proposalID, approvalID, "no-dispatch-approve")
	digest := proposalDigestFixture(t, s, task.Id, proposalID)

	subtasksBefore := scalarInt(t, s, `SELECT count(*) FROM core.subtask WHERE workspace_id=$1 AND task_id=$2 AND state='QUEUED'`, "ws_fixture", task.Id)
	if _, err := s.ApplyPlanProposal(ctx, fixtureActor(), task.Id, proposalID, "no-dispatch-apply", contract.PlanApplyRequest{
		ApprovalRef:     approvalID,
		ExpectedVersion: task.Version,
		ProposalDigest:  digest,
	}); err != nil {
		t.Fatalf("apply proposal: %v", err)
	}

	for _, table := range []string{"core.execution", "bridge.gateway_operation", "bridge.outbox", "core.dispatch_permit", "core.target_claim", "core.account_usage", "core.chat_send_usage", "core.plan_application"} {
		want := 0
		if table == "core.plan_application" {
			want = 1
		}
		if got := scalarInt(t, s, `SELECT count(*) FROM `+table+` WHERE workspace_id=$1`, "ws_fixture"); got != want {
			t.Fatalf("%s rows=%d, want %d; proposal apply must not touch external dispatch state", table, got, want)
		}
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.outbox WHERE workspace_id=$1 AND event_type='dispatch.requested'`, "ws_fixture"); got != 0 {
		t.Fatalf("dispatch.requested outbox rows=%d, want 0", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.outbox WHERE workspace_id=$1 AND event_type='plan.proposal.applied'`, "ws_fixture"); got != 1 {
		t.Fatalf("plan.proposal.applied outbox rows=%d, want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.subtask WHERE workspace_id=$1 AND task_id=$2 AND state='QUEUED'`, "ws_fixture", task.Id); got != subtasksBefore {
		t.Fatalf("proposal apply changed queued subtasks: %d != %d", got, subtasksBefore)
	}
}

func TestPlanProposalHTTPLifecycle(t *testing.T) {
	s := fixtureStore(t)
	task, approvalID := proposalFixtureTask(t, s)
	actor := fixtureActor()
	handler := s.ProposalHandler(func(*http.Request) (Actor, error) { return actor, nil }, true)

	createBody := contract.PlanProposalRequest{
		TaskId:          task.Id,
		ExpectedVersion: task.Version,
		ScopeRevision:   task.ScopeRevision,
		ProposalKind:    contract.PlanProposalRequestProposalKindUseTemplate,
		Reason:          "http proposal fixture",
	}
	rec := ttServe(handler, http.MethodPost, "/v1/tasks/"+task.Id+"/plan-proposals", ttJSONBody(t, createBody), map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "http-proposal-create",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("proposal create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created contract.ReceiptEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	proposalID := created.Data.ResourceId
	rec = ttServe(handler, http.MethodGet, "/v1/tasks/"+task.Id+"/plan-proposals/"+proposalID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("proposal read status=%d body=%s", rec.Code, rec.Body.String())
	}
	var view proposalEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Data.ProposalId != proposalID || view.Data.State != "pending" || !planProposalDigestPattern.MatchString(view.Data.Digest) {
		t.Fatalf("unexpected proposal view: %+v", view.Data)
	}
	digest := view.Data.Digest

	rec = ttServe(handler, http.MethodPost, "/v1/tasks/"+task.Id+"/plan-proposals/"+proposalID+"/approve", ttJSONBody(t, PlanProposalApprovalRequest{
		ExpectedVersion: task.Version, ApprovalRef: approvalID,
	}), map[string]string{"Content-Type": "application/json", "Idempotency-Key": "http-proposal-approve"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("proposal approve status=%d body=%s", rec.Code, rec.Body.String())
	}
	var approved proposalApprovalEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Data.State != "approved" {
		t.Fatalf("approval state=%q", approved.Data.State)
	}

	rec = ttServe(handler, http.MethodPost, "/v1/tasks/"+task.Id+"/plan-proposals/"+proposalID+"/apply", ttJSONBody(t, contract.PlanApplyRequest{
		ApprovalRef: approvalID, ExpectedVersion: task.Version, ProposalDigest: digest,
	}), map[string]string{"Content-Type": "application/json", "Idempotency-Key": "http-proposal-apply"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("proposal apply status=%d body=%s", rec.Code, rec.Body.String())
	}
	var applied contract.ReceiptEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Data.Action != contract.ReceiptActionApply || applied.Data.ResourceId != proposalID {
		t.Fatalf("unexpected apply receipt: %+v", applied.Data)
	}

	disabled := s.ProposalHandler(func(*http.Request) (Actor, error) { return actor, nil }, false)
	rec = ttServe(disabled, http.MethodPost, "/v1/tasks/"+task.Id+"/plan-proposals", ttJSONBody(t, createBody), map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "http-proposal-disabled",
	})
	if rec.Code != http.StatusForbidden || ttErrorCode(t, rec) != taskCodeFeatureDisabled {
		t.Fatalf("disabled proposal handler status=%d code=%q", rec.Code, ttErrorCode(t, rec))
	}
}
