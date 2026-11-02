package taskengine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type modelRoundTripper func(*http.Request) (*http.Response, error)

func (fn modelRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestOpenAICompatibleProviderSingleStructuredRequest(t *testing.T) {
	const content = `{"schema_id":"PlanProposal.v1","task_id":"task_ref","expected_task_version":7,"scope_revision":3,"proposal_kind":"request_clarification","steps":[],"questions":["Please provide approved application text."],"rationale_summary":"No approved application text is stored."}`
	responseBody, err := json.Marshal(map[string]any{
		"model": "fixture-model-revision",
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"content": content, "tool_calls": []any{}},
		}},
		"usage": map[string]int{"prompt_tokens": 12, "completion_tokens": 18},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: modelRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.String() != "https://api.onehop.ai/v1/chat/completions" {
			t.Fatalf("request method/url = %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer fixture-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected request headers: %#v", r.Header)
		}
		var body openAICompatibleRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "deepseek/deepseek-v4.1-flash" || body.ResponseFormat["type"] != "json_object" || body.Stream || body.MaxTokens <= 0 {
			t.Fatalf("unexpected provider request: %+v", body)
		}
		if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" ||
			!strings.Contains(body.Messages[1].Content, "version 7") || strings.Contains(body.Messages[1].Content, "fixture-task-id") {
			t.Fatalf("unexpected prompt: %+v", body.Messages)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(responseBody)))}, nil
	})}
	provider, err := NewOpenAICompatibleProvider(OpenAICompatibleProviderConfig{
		BaseURL: "https://api.onehop.ai/v1", ModelID: "deepseek/deepseek-v4.1-flash", ProviderID: "onehop.ai", APIKey: "fixture-key",
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	completion, err := provider.Complete(context.Background(), ModelCall{Version: 7, ScopeRevision: 3, GoalCount: 4, PromptVersion: modelPromptVersion})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || completion.Content != content || completion.ModelID != "fixture-model-revision" || completion.ProviderID != "onehop.ai" || completion.InputTokens != 12 || completion.OutputTokens != 18 {
		t.Fatalf("calls=%d completion=%+v", calls, completion)
	}
}

func TestOpenAICompatibleProviderRejectsInvalidEndpoint(t *testing.T) {
	for _, baseURL := range []string{
		"http://api.onehop.ai/v1", "https://api.onehop.ai/v1?query=x", "https://api.onehop.ai/v1/chat/completions", "https://user@api.onehop.ai/v1",
	} {
		t.Run(baseURL, func(t *testing.T) {
			if _, err := NewOpenAICompatibleProvider(OpenAICompatibleProviderConfig{
				BaseURL: baseURL, ModelID: "model", ProviderID: "provider", APIKey: "fixture-key",
			}, nil); err == nil {
				t.Fatalf("invalid endpoint accepted: %s", baseURL)
			}
		})
	}
}

func TestOpenAICompatibleProviderPrivateEndpointApproval(t *testing.T) {
	for _, tc := range []struct {
		name, base, approved string
		allowed              bool
	}{
		{"approved", "http://192.168.68.106:8166/v1", "http://192.168.68.106:8166/v1", true},
		{"missing approval", "http://192.168.68.106:8166/v1", "", false},
		{"different host", "http://192.168.68.107:8166/v1", "http://192.168.68.106:8166/v1", false},
		{"different port", "http://192.168.68.106:8167/v1", "http://192.168.68.106:8166/v1", false},
		{"public address", "http://8.8.8.8/v1", "http://8.8.8.8/v1", false},
		{"hostname", "http://model.internal/v1", "http://model.internal/v1", false},
		{"credential URL", "http://user@192.168.68.106/v1", "http://user@192.168.68.106/v1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOpenAICompatibleProvider(OpenAICompatibleProviderConfig{
				BaseURL: tc.base, ApprovedPrivateHTTPBaseURL: tc.approved,
				ModelID: "fixture-model", ProviderID: "fixture", APIKey: "fixture-key",
			}, nil)
			if (err == nil) != tc.allowed {
				t.Fatalf("endpoint accepted=%v, want %v", err == nil, tc.allowed)
			}
		})
	}
}

func TestOpenAICompatibleProviderRejectsEmptyContentWithoutRetry(t *testing.T) {
	responseBody := `{"model":"fixture-model","choices":[{"finish_reason":"stop","message":{"content":""}}],"usage":{"prompt_tokens":1,"completion_tokens":0}}`
	calls := 0
	provider, err := NewOpenAICompatibleProvider(OpenAICompatibleProviderConfig{
		BaseURL: "https://api.onehop.ai/v1", ModelID: "model", ProviderID: "provider", APIKey: "fixture-key",
	}, &http.Client{Transport: modelRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(responseBody))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete(context.Background(), ModelCall{Version: 1, ScopeRevision: 1, GoalCount: 1}); err == nil {
		t.Fatal("empty model content was accepted")
	}
	if calls != 1 {
		t.Fatalf("provider calls=%d, want exactly one", calls)
	}
}

func TestOpenAICompatibleProviderLiveSmoke(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("BBA_P017_LIVE_API_KEY"))
	if apiKey == "" {
		t.Skip("BBA_P017_LIVE_API_KEY is not set")
	}
	baseURL, modelID, providerID := os.Getenv("BBA_P017_LIVE_BASE_URL"), os.Getenv("BBA_P017_LIVE_MODEL_ID"), os.Getenv("BBA_P017_LIVE_PROVIDER_ID")
	if baseURL == "" || modelID == "" || providerID == "" {
		t.Fatal("live provider requires explicit base URL, model ID and provider ID")
	}
	provider, err := NewOpenAICompatibleProvider(OpenAICompatibleProviderConfig{
		BaseURL: baseURL, ModelID: modelID, ProviderID: providerID, APIKey: apiKey,
		ApprovedPrivateHTTPBaseURL: os.Getenv("BBA_P017_APPROVED_PRIVATE_HTTP_BASE_URL"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := fixtureStore(t)
	task := modelTestTask(t, s, 1)
	request := modelTestRequest(task)
	actor := modelTestActor()
	response, err := s.RunPlanModelRequest(ctx, actor, "live-p017-once", request, provider)
	if err != nil {
		t.Fatalf("live provider request or ledger validation failed: %v", err)
	}
	if response.Model.Provider != providerID || response.Model.ApiModelId != modelID || response.Attempts != 1 {
		t.Fatalf("unexpected live response metadata: provider=%s model=%s attempts=%d", response.Model.Provider, response.Model.ApiModelId, response.Attempts)
	}
	if response.LatencyMs <= 0 {
		t.Fatalf("live provider latency was not recorded: %d ms", response.LatencyMs)
	}
	if _, err := s.RunPlanModelRequest(ctx, actor, "live-p017-once", request, provider); err != nil {
		t.Fatalf("live ledger idempotent replay failed: %v", err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.model_request WHERE workspace_id='ws_fixture' AND task_id=$1 AND state='completed' AND attempts=1`, task.Id); got != 1 {
		t.Fatalf("completed provider ledger rows=%d, want 1", got)
	}
	t.Logf("live P0-17 ledger smoke passed: provider=%s returned_model=%s input_tokens=%d output_tokens=%d latency_ms=%d", response.Model.Provider, response.Model.ApiModelId, response.Usage.InputTokens, response.Usage.OutputTokens, response.LatencyMs)
}
