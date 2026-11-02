package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
)

type fixedModelProvider struct {
	calls      atomic.Int32
	completion ModelCompletion
	err        error
}

func (p *fixedModelProvider) Complete(ctx context.Context, call ModelCall) (ModelCompletion, error) {
	p.calls.Add(1)
	if p.err != nil {
		return ModelCompletion{}, p.err
	}
	return p.completion, nil
}

type blockingModelProvider struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	result  ModelCompletion
}

func (p *blockingModelProvider) Complete(ctx context.Context, call ModelCall) (ModelCompletion, error) {
	p.calls.Add(1)
	close(p.started)
	select {
	case <-p.release:
		return p.result, nil
	case <-ctx.Done():
		return ModelCompletion{}, ctx.Err()
	}
}

func modelTestTask(t *testing.T, s *Store, maxModelAttempts int) contract.Task {
	t.Helper()
	seedAccount(t, s, "acc_model", "rt_model", 10)
	taskRequest := ttRequest([]string{"930000101"}, []string{"acc_model"}, 1)
	taskRequest.Limits.MaxModelAttempts = maxModelAttempts
	created, err := s.CreateTask(context.Background(), fixtureActor(), "model-create", taskRequest)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func modelTestRequest(task contract.Task) contract.ModelRequest {
	request := contract.ModelRequest{
		DeadlineAt:           time.Now().UTC().Add(30 * time.Minute),
		InputRef:             task.Id,
		MaxProviderAttempts:  3,
		OperationKind:        contract.ModelRequestOperationKindComplexPlan,
		PrivacyClass:         contract.ModelRequestPrivacyClassRedactedExternalAllowed,
		PromptVersion:        modelPromptVersion,
		RequestId:            "model-request-fixture",
		RequiredCapabilities: []contract.ModelRequestRequiredCapabilities{contract.StructuredOutput},
		RoutePolicy:          contract.ModelRequestRoutePolicyL2PreferOnline,
		SchemaId:             "PlanProposal.v1",
	}
	request.TaskRef.Set(struct {
		Id            string `json:"id"`
		ScopeRevision int    `json:"scope_revision"`
		Version       int    `json:"version"`
	}{Id: task.Id, ScopeRevision: task.ScopeRevision, Version: task.Version})
	return request
}

func modelTestProposal(t *testing.T, task contract.Task) string {
	t.Helper()
	payload := map[string]any{
		"schema_id": "PlanProposal.v1", "task_id": "task_ref",
		"expected_task_version": task.Version, "scope_revision": task.ScopeRevision,
		"proposal_kind": "request_clarification", "steps": []any{},
		"questions":         []string{"Please provide approved application text."},
		"rationale_summary": "No approved application text is stored.",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func modelTestActor() Actor {
	actor := fixtureActor()
	actor.Permissions[modelPermRequest] = true
	return actor
}

func TestModelProposalValidationClarificationOnly(t *testing.T) {
	task := contract.Task{Id: "task-fixture", Version: 7, ScopeRevision: 3}
	request := modelTestRequest(task)
	call := ModelCall{Version: 7, ScopeRevision: 3, GoalCount: 2}
	valid := modelTestProposal(t, task)
	proposal, err := validateModelProposal(valid, request, call)
	if err != nil {
		t.Fatalf("valid clarification proposal rejected: %v", err)
	}
	if proposal.TaskId != task.Id || len(proposal.Steps) != 0 || proposal.ProposalKind != contract.PlanProposalProposalKindRequestClarification {
		t.Fatalf("proposal escaped clarification-only boundary: %+v", proposal)
	}
	for name, mutate := range map[string]func(map[string]any){
		"executable steps": func(value map[string]any) { value["steps"] = []any{map[string]any{"action": "request_join"}} },
		"wrong task alias": func(value map[string]any) { value["task_id"] = "task-fixture" },
		"wrong version":    func(value map[string]any) { value["expected_task_version"] = 8 },
		"wrong schema":     func(value map[string]any) { value["schema_id"] = "Summary.v1" },
		"empty questions":  func(value map[string]any) { value["questions"] = []any{} },
		"unknown property": func(value map[string]any) { value["execute"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal([]byte(valid), &value); err != nil {
				t.Fatal(err)
			}
			mutate(value)
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := validateModelProposal(string(encoded), request, call); !errors.Is(err, errModelSchemaInvalid) {
				t.Fatalf("invalid proposal err=%v", err)
			}
		})
	}
}

func TestModelRequestSuccessIdempotentReplayAndSingleProviderCall(t *testing.T) {
	s := fixtureStore(t)
	task := modelTestTask(t, s, 2)
	actor := modelTestActor()
	provider := &fixedModelProvider{completion: ModelCompletion{Content: modelTestProposal(t, task), ModelID: "fixture-model-revision", ProviderID: "fixture-provider", InputTokens: 11, OutputTokens: 9}}
	request := modelTestRequest(task)
	first, err := s.RunPlanModelRequest(context.Background(), actor, "model-key-success", request, provider)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RunPlanModelRequest(context.Background(), actor, "model-key-success", request, provider)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 || first.Attempts != 1 || second.Attempts != 1 || first.RequestId != second.RequestId {
		t.Fatalf("provider calls=%d attempts=%d/%d request ids=%q/%q", provider.calls.Load(), first.Attempts, second.Attempts, first.RequestId, second.RequestId)
	}
	if first.Model.Provider != "fixture-provider" || first.Model.ApiModelId != "fixture-model-revision" {
		t.Fatalf("response reported incorrect provider identity: %+v", first.Model)
	}
	if first.LatencyMs != 0 {
		t.Fatalf("fixture completion with zero latency reported %d ms", first.LatencyMs)
	}
	responseWithLatency := modelResponse(request, contract.PlanProposal{TaskId: task.Id}, ModelCompletion{ProviderID: "fixture-provider", ModelID: "fixture-model", Latency: 37 * time.Millisecond})
	if responseWithLatency.LatencyMs != 37 {
		t.Fatalf("provider latency reported %d ms, want 37", responseWithLatency.LatencyMs)
	}
	changed := request
	changed.DeadlineAt = request.DeadlineAt.Add(time.Second)
	if _, err := s.RunPlanModelRequest(context.Background(), actor, "model-key-success", changed, provider); Code(err) != taskCodeIdempotency {
		t.Fatalf("same key with changed request error=%v", err)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("changed idempotent request called provider again: %d", provider.calls.Load())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.model_request WHERE workspace_id='ws_fixture' AND task_id=$1 AND state='completed'`, task.Id); got != 1 {
		t.Fatalf("completed request rows=%d, want 1", got)
	}
}

func TestModelRequestSchemaFailureConsumesOneAttemptWithoutRetry(t *testing.T) {
	s := fixtureStore(t)
	task := modelTestTask(t, s, 2)
	actor := modelTestActor()
	provider := &fixedModelProvider{completion: ModelCompletion{Content: `{"schema_id":"PlanProposal.v1"}`, ModelID: "fixture-model-revision", ProviderID: "fixture-provider"}}
	request := modelTestRequest(task)
	if _, err := s.RunPlanModelRequest(context.Background(), actor, "model-key-invalid", request, provider); Code(err) != modelCodeSchemaInvalid {
		t.Fatalf("schema-invalid request error=%v", err)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls.Load())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.model_request WHERE workspace_id='ws_fixture' AND task_id=$1 AND state='failed' AND attempts=1 AND error_code='MODEL_SCHEMA_INVALID'`, task.Id); got != 1 {
		t.Fatalf("failed request rows=%d, want 1", got)
	}
	if _, err := s.RunPlanModelRequest(context.Background(), actor, "model-key-invalid", request, provider); Code(err) != modelCodeSchemaInvalid {
		t.Fatalf("failed idempotent replay error=%v", err)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("failed replay called provider again: %d", provider.calls.Load())
	}
}

func TestModelRequestBudgetIsCappedAtThree(t *testing.T) {
	s := fixtureStore(t)
	task := modelTestTask(t, s, 4)
	actor := modelTestActor()
	provider := &fixedModelProvider{err: errors.New("offline fixture failure")}
	for i := 0; i < 3; i++ {
		request := modelTestRequest(task)
		request.RequestId = newID("model-request")
		key := string(rune('a' + i))
		if _, err := s.RunPlanModelRequest(context.Background(), actor, "model-key-budget-"+key, request, provider); Code(err) != modelCodeProviderFailed {
			t.Fatalf("attempt %d error=%v", i+1, err)
		}
	}
	request := modelTestRequest(task)
	request.RequestId = newID("model-request")
	if _, err := s.RunPlanModelRequest(context.Background(), actor, "model-key-budget-final", request, provider); Code(err) != modelCodeLocalBudgetExhausted {
		t.Fatalf("fourth call error=%v, want capped budget", err)
	}
	if provider.calls.Load() != 3 {
		t.Fatalf("provider calls=%d, want 3", provider.calls.Load())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.model_request WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id); got != 3 {
		t.Fatalf("ledger rows=%d, want 3", got)
	}
}

func TestConcurrentModelRequestsCannotExceedTaskBudget(t *testing.T) {
	s := fixtureStore(t)
	task := modelTestTask(t, s, 1)
	actor := modelTestActor()
	provider := &blockingModelProvider{
		started: make(chan struct{}), release: make(chan struct{}),
		result: ModelCompletion{Content: modelTestProposal(t, task), ModelID: "fixture-model-revision", ProviderID: "fixture-provider"},
	}
	request := modelTestRequest(task)
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.RunPlanModelRequest(context.Background(), actor, "model-key-concurrent-a", request, provider)
		firstDone <- err
	}()
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first provider call did not start")
	}
	secondRequest := modelTestRequest(task)
	secondRequest.RequestId = newID("model-request")
	_, secondErr := s.RunPlanModelRequest(context.Background(), actor, "model-key-concurrent-b", secondRequest, provider)
	if Code(secondErr) != modelCodeLocalBudgetExhausted {
		close(provider.release)
		t.Fatalf("concurrent request error=%v, want exhausted budget", secondErr)
	}
	close(provider.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first model request failed: %v", err)
	}
	if provider.calls.Load() != 1 || scalarInt(t, s, `SELECT count(*) FROM core.model_request WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id) != 1 {
		t.Fatalf("concurrent budget escaped: provider calls=%d ledger rows=%d", provider.calls.Load(), scalarInt(t, s, `SELECT count(*) FROM core.model_request WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id))
	}
}

func TestModelRequestValidationRejectsSummaryAndDisabledHandler(t *testing.T) {
	task := contract.Task{Id: "task-fixture", Version: 1, ScopeRevision: 1}
	request := modelTestRequest(task)
	request.OperationKind = contract.ModelRequestOperationKindLongSummary
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if err := modelValidateRequest(fields, request); Code(err) != taskCodeValidation {
		t.Fatalf("summary request error=%v, want validation failure", err)
	}
	handler := (&modelHTTP{enabled: false}).ServeHTTP
	response := httptest.NewRecorder()
	requestHTTP := httptest.NewRequest("POST", modelRequestPath, nil)
	handler(response, requestHTTP)
	var envelope contract.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusForbidden || envelope.Error.Code != taskCodeFeatureDisabled {
		t.Fatalf("disabled handler status=%d error=%q", response.Code, envelope.Error.Code)
	}
}
