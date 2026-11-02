package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"bba.local/taskengine/internal/contract"
)

const (
	modelCodeProviderFailed       = "MODEL_PROVIDER_FAILED"
	modelCodeSchemaInvalid        = "MODEL_SCHEMA_INVALID"
	modelCodeProviderUnavailable  = "MODEL_PROVIDER_UNAVAILABLE"
	modelCodeLocalBudgetExhausted = "LOCAL_BUDGET_EXHAUSTED"
)

const (
	modelPermRequest  = "model.request"
	modelRequestPath  = "/v1/model-requests"
	maxModelBodyBytes = 64 << 10
)

type ModelCall struct {
	WorkspaceID   string
	PromptVersion string
	Version       int
	ScopeRevision int
	GoalCount     int
	AgentContext  string
	SearchContext string
}

type ModelCompletion struct {
	Content      string
	ModelID      string
	ProviderID   string
	InputTokens  int
	OutputTokens int
	Latency      time.Duration
}

type ModelProvider interface {
	Complete(context.Context, ModelCall) (ModelCompletion, error)
}

type modelHTTP struct {
	store    *Store
	auth     func(*http.Request) (Actor, error)
	provider ModelProvider
	enabled  bool
}

// ModelHandler is an isolated model-workload endpoint. Handler does not mount
// it; the caller must explicitly wire it and provide a trusted workload auth.
func (s *Store) ModelHandler(auth func(*http.Request) (Actor, error), provider ModelProvider, enabled bool) http.Handler {
	return &modelHTTP{store: s, auth: auth, provider: provider, enabled: enabled}
}

func (h *modelHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != modelRequestPath {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	if r.Method != http.MethodPost {
		thMethodNotAllowed(w, r, http.MethodPost)
		return
	}
	if !h.enabled {
		thWriteError(w, r, problem(taskCodeFeatureDisabled))
		return
	}
	if h.auth == nil {
		thWriteError(w, r, problem(taskCodeUnauthenticated))
		return
	}
	actor, err := h.auth(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := actor.require(modelPermRequest); err != nil {
		thWriteError(w, r, err)
		return
	}
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	var request contract.ModelRequest
	raw, err := modelDecodeStrict(w, r, &request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := modelValidateRequest(raw, request); err != nil {
		thWriteError(w, r, err)
		return
	}
	if h.provider == nil {
		thWriteError(w, r, problem(modelCodeProviderUnavailable))
		return
	}
	response, err := h.store.RunPlanModelRequest(r.Context(), actor, key, request, h.provider)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusAccepted, contract.ModelEnvelope{Data: response, Meta: thMeta(r)})
}

func modelDecodeStrict(w http.ResponseWriter, r *http.Request, dst any) (map[string]json.RawMessage, error) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	if err != nil || mediaType != "application/json" || r.Body == nil {
		return nil, problem(taskCodeUnsupportedMedia)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxModelBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, problem(taskCodePayloadTooLarge)
		}
		return nil, problem(taskCodeValidation)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, problem(taskCodeValidation)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return nil, problem(taskCodeValidation)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, problem(taskCodeValidation)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, problem(taskCodeValidation)
	}
	return raw, nil
}

func modelValidateRequest(raw map[string]json.RawMessage, req contract.ModelRequest) error {
	for _, key := range []string{"request_id", "operation_kind", "task_ref", "route_policy", "privacy_class", "required_capabilities", "schema_id", "prompt_version", "input_ref", "max_provider_attempts", "deadline_at"} {
		if _, ok := raw[key]; !ok {
			return problem(taskCodeValidation)
		}
	}
	if !taskValidID(req.RequestId) || req.OperationKind != contract.ModelRequestOperationKindComplexPlan ||
		req.RoutePolicy != contract.ModelRequestRoutePolicyL2PreferOnline || req.PrivacyClass != contract.ModelRequestPrivacyClassRedactedExternalAllowed ||
		req.SchemaId != "PlanProposal.v1" || req.PromptVersion != modelPromptVersion ||
		!taskValidID(req.InputRef) || req.MaxProviderAttempts < 1 || req.MaxProviderAttempts > 3 ||
		!req.TaskRef.IsSpecified() || req.TaskRef.IsNull() {
		return problem(taskCodeValidation)
	}
	capabilities := make(map[string]bool, len(req.RequiredCapabilities))
	for _, capability := range req.RequiredCapabilities {
		if capabilities[string(capability)] || !capability.Valid() {
			return problem(taskCodeValidation)
		}
		capabilities[string(capability)] = true
	}
	if len(capabilities) != 1 || !capabilities[string(contract.StructuredOutput)] {
		return problem(taskCodeValidation)
	}
	var deadlineText string
	if json.Unmarshal(raw["deadline_at"], &deadlineText) != nil || !strings.HasSuffix(deadlineText, "Z") || !req.DeadlineAt.After(time.Now()) {
		return problem(taskCodeValidation)
	}
	ref := req.TaskRef.MustGet()
	if !taskValidID(ref.Id) || ref.Version < 1 || ref.ScopeRevision < 1 {
		return problem(taskCodeValidation)
	}
	return nil
}

func (s *Store) RunPlanModelRequest(ctx context.Context, actor Actor, key string, request contract.ModelRequest, provider ModelProvider) (contract.ModelResponse, error) {
	return s.runBoundPlanModelRequest(ctx, actor, key, request, provider, nil)
}
func (s *Store) runBoundPlanModelRequest(ctx context.Context, actor Actor, key string, request contract.ModelRequest, provider ModelProvider, binding *SkillBinding) (contract.ModelResponse, error) {
	if err := actor.require(modelPermRequest); err != nil {
		return contract.ModelResponse{}, err
	}
	if provider == nil || taskBlank(key) || len(key) > 128 || !request.TaskRef.IsSpecified() || request.TaskRef.IsNull() {
		return contract.ModelResponse{}, problem(taskCodeValidation)
	}
	ref := request.TaskRef.MustGet()
	if request.InputRef != ref.Id {
		return contract.ModelResponse{}, problem(taskCodeNotFound)
	}
	requestDigest, err := digest(request)
	if binding != nil {
		requestDigest, err = digest(struct {
			Request  contract.ModelRequest
			AgentRef SkillBinding
		}{request, *binding})
	}
	if b, ok := provider.(interface{ searchBinding() SearchQuery }); ok {
		requestDigest, err = digest(struct {
			Digest string
			Search SearchQuery
		}{requestDigest, b.searchBinding()})
	}
	if err != nil {
		return contract.ModelResponse{}, err
	}
	var call ModelCall
	var requestRowID string
	var replay *contract.ModelResponse
	var replayCode string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, ref.Id)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, modelPermRequest); err != nil {
			return err
		}
		var storedDigest, state string
		var rawResponse []byte
		err = tx.QueryRow(ctx, `SELECT request_digest,state,COALESCE(response,'null'::jsonb),COALESCE(error_code,'')
			FROM core.model_request WHERE workspace_id=$1 AND principal_id=$2 AND idempotency_key=$3`,
			actor.WorkspaceID, actor.PrincipalID, key).Scan(&storedDigest, &state, &rawResponse, &replayCode)
		if err == nil {
			if storedDigest != requestDigest {
				return problem(taskCodeIdempotency)
			}
			switch state {
			case "completed":
				var result contract.ModelResponse
				if json.Unmarshal(rawResponse, &result) != nil {
					return problem(taskCodeStoreUnavailable)
				}
				replay = &result
				return nil
			case "failed":
				return problem(replayCode)
			default:
				return problem(taskCodeStateConflict)
			}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if request.InputRef != task.ID || ref.Version != task.Version || ref.ScopeRevision != task.ScopeRevision {
			return problem(taskCodeVersionConflict)
		}
		if !request.DeadlineAt.After(time.Now()) || request.DeadlineAt.After(task.Deadline) {
			return problem(taskCodeStateConflict)
		}
		var payload []byte
		if err := tx.QueryRow(ctx, `SELECT request_payload FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.ID).Scan(&payload); err != nil {
			return err
		}
		var taskRequest contract.CreateTaskRequest
		if json.Unmarshal(payload, &taskRequest) != nil || taskRequest.Limits.MaxModelAttempts < 1 {
			return problem(modelCodeLocalBudgetExhausted)
		}
		var used int
		if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM core.model_request WHERE workspace_id=$1 AND task_id=$2`, actor.WorkspaceID, task.ID).Scan(&used); err != nil {
			return err
		}
		budget := taskRequest.Limits.MaxModelAttempts
		if budget > 3 {
			budget = 3
		}
		if used >= budget {
			return problem(modelCodeLocalBudgetExhausted)
		}
		call = ModelCall{WorkspaceID: actor.WorkspaceID, PromptVersion: request.PromptVersion, Version: task.Version,
			ScopeRevision: task.ScopeRevision, GoalCount: task.GoalCount}
		requestRowID = newID("modelreq")
		result, err := tx.Exec(ctx, `INSERT INTO core.model_request(workspace_id,id,task_id,principal_id,idempotency_key,request_digest,
			operation_kind,schema_id,prompt_version,input_ref,deadline_at,max_provider_attempts,state)
			VALUES($1,$2,$3,$4,$5,$6,'complex_plan','PlanProposal.v1',$7,$8,$9,$10,'started')
			ON CONFLICT (workspace_id,principal_id,idempotency_key) DO NOTHING`,
			actor.WorkspaceID, requestRowID, task.ID, actor.PrincipalID, key, requestDigest,
			request.PromptVersion, request.InputRef, request.DeadlineAt, request.MaxProviderAttempts)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return problem(taskCodeIdempotency)
		}
		return nil
	})
	if err != nil {
		return contract.ModelResponse{}, err
	}
	if replay != nil {
		return *replay, nil
	}
	ctx, cancel := context.WithDeadline(ctx, request.DeadlineAt)
	defer cancel()
	completion, providerErr := provider.Complete(ctx, call)
	var response contract.ModelResponse
	if providerErr == nil {
		proposal, validateErr := validateModelProposal(completion.Content, request, call)
		if validateErr != nil {
			providerErr = validateErr
		} else {
			response = modelResponse(request, proposal, completion)
		}
	}
	if providerErr != nil {
		code := modelProviderErrorCode(providerErr)
		if err := s.finishModelRequest(context.Background(), actor.WorkspaceID, requestRowID, code, nil); err != nil {
			return contract.ModelResponse{}, err
		}
		return contract.ModelResponse{}, problem(code)
	}
	if err := s.finishModelRequest(context.Background(), actor.WorkspaceID, requestRowID, "", &response); err != nil {
		return contract.ModelResponse{}, err
	}
	return response, nil
}

func (s *Store) finishModelRequest(ctx context.Context, workspace, id, code string, response *contract.ModelResponse) error {
	var raw any
	state := "failed"
	if response != nil {
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		raw = encoded
		state = "completed"
	}
	result, err := s.Pool.Exec(ctx, `UPDATE core.model_request SET state=$3,error_code=NULLIF($4,''),response=$5,completed_at=clock_timestamp()
		WHERE workspace_id=$1 AND id=$2 AND state='started'`, workspace, id, state, code, raw)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return problem(taskCodeStateConflict)
	}
	return nil

}

// Only explicit, trusted taskengine domain errors survive the provider boundary.
// Code intentionally classifies arbitrary non-nil errors as store failures for
// store callers; it must not be used to classify unknown provider/network errors.
func modelProviderErrorCode(err error) string {
	if errors.Is(err, errModelSchemaInvalid) {
		return modelCodeSchemaInvalid
	}
	var domain *Error
	if errors.As(err, &domain) && domain != nil {
		if strings.HasPrefix(domain.Code, "SEARCH_") {
			return domain.Code
		}
		switch domain.Code {
		case "AGENT_REFERENCE_UNAVAILABLE", "MODEL_REFERENCE_UNAVAILABLE",
			"SKILL_REFERENCE_UNAVAILABLE", "SKILL_VERSION_UNAVAILABLE",
			taskCodeStoreUnavailable, taskCodeStateConflict:
			return domain.Code
		}
	}
	return modelCodeProviderFailed
}

var errModelSchemaInvalid = errors.New("model schema invalid")

type rawPlanProposal struct {
	SchemaID            string   `json:"schema_id"`
	TaskID              string   `json:"task_id"`
	ExpectedTaskVersion int      `json:"expected_task_version"`
	ScopeRevision       int      `json:"scope_revision"`
	ProposalKind        string   `json:"proposal_kind"`
	Steps               []any    `json:"steps"`
	Questions           []string `json:"questions"`
	RationaleSummary    string   `json:"rationale_summary"`
}

func validateModelProposal(content string, request contract.ModelRequest, call ModelCall) (contract.PlanProposal, error) {
	invalid := func() (contract.PlanProposal, error) { return contract.PlanProposal{}, errModelSchemaInvalid }
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var proposal rawPlanProposal
	if decoder.Decode(&proposal) != nil || decoder.Decode(new(any)) != io.EOF ||
		json.Unmarshal([]byte(content), &object) != nil || object == nil || len(object) != 8 {
		return invalid()
	}
	for _, key := range []string{"schema_id", "task_id", "expected_task_version", "scope_revision", "proposal_kind", "steps", "questions", "rationale_summary"} {
		if _, ok := object[key]; !ok {
			return invalid()
		}
	}
	if proposal.SchemaID != request.SchemaId || proposal.TaskID != "task_ref" ||
		proposal.ExpectedTaskVersion != call.Version || proposal.ScopeRevision != call.ScopeRevision ||
		proposal.ProposalKind != string(contract.PlanProposalProposalKindRequestClarification) || len(proposal.Steps) != 0 ||
		len(proposal.Questions) == 0 || len(proposal.Questions) > 20 ||
		strings.TrimSpace(proposal.RationaleSummary) == "" || len(proposal.RationaleSummary) > 2000 {
		return invalid()
	}
	for _, question := range proposal.Questions {
		if strings.TrimSpace(question) == "" || len(question) > 500 {
			return invalid()
		}
	}
	taskRef := request.TaskRef.MustGet()
	return contract.PlanProposal{
		SchemaId: contract.PlanProposalV1, TaskId: taskRef.Id,
		ExpectedTaskVersion: call.Version, ScopeRevision: call.ScopeRevision,
		ProposalKind: contract.PlanProposalProposalKindRequestClarification,
		Steps:        []contract.PlanStep{}, Questions: append([]string(nil), proposal.Questions...),
		RationaleSummary: proposal.RationaleSummary,
	}, nil
}
func modelResponse(request contract.ModelRequest, proposal contract.PlanProposal, completion ModelCompletion) contract.ModelResponse {
	var response contract.ModelResponse
	response.Attempts = 1
	response.CompletedAt = contract.UTCDateTime(time.Now().UTC())
	response.FallbackUsed = false
	response.LatencyMs = int(completion.Latency / time.Millisecond)
	response.Model.Provider = completion.ProviderID
	response.Model.ApiModelId = completion.ModelID
	response.Model.Revision.SetNull()
	response.Model.Quantization.SetNull()
	response.OperationKind = contract.ModelResponseOperationKindComplexPlan
	response.RequestId = request.RequestId
	response.SchemaId = request.SchemaId
	_ = response.Result.FromPlanProposal(proposal)
	response.Usage.InputTokens = completion.InputTokens
	response.Usage.OutputTokens = completion.OutputTokens
	response.Usage.CostMicros.SetNull()
	return response
}
