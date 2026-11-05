package taskengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type recordingStructuredProvider struct {
	responses []string
	calls     []StructuredModelCall
}

func fakePrivateProvider(t *testing.T, server *httptest.Server, timeout time.Duration) ModelProvider {
	t.Helper()
	tcp, ok := server.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("fake server listener is not TCP: %T", server.Listener.Addr())
	}
	base := fmt.Sprintf("http://10.23.45.67:%d/v1", tcp.Port)
	network := modelNetwork{
		lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
		dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	provider, err := newOpenAICompatibleProvider(OpenAICompatibleProviderConfig{BaseURL: base, ModelID: "fake-goal", ProviderID: "fake-provider", APIKey: "fake-key", ApprovedPrivateHTTPBaseURL: base}, &http.Client{Timeout: timeout}, network)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func (p *recordingStructuredProvider) Complete(context.Context, ModelCall) (ModelCompletion, error) {
	return ModelCompletion{}, nil
}
func (p *recordingStructuredProvider) CompleteStructured(_ context.Context, call StructuredModelCall) (ModelCompletion, error) {
	p.calls = append(p.calls, call)
	if len(p.responses) == 0 {
		return ModelCompletion{}, problem("GOAL_PROVIDER_FAILED")
	}
	body := p.responses[0]
	p.responses = p.responses[1:]
	return ModelCompletion{Content: body, ProviderID: "fake", ModelID: "goal-model"}, nil
}

func testGoalSnapshot() GoalPlanningSnapshot {
	return GoalPlanningSnapshot{
		SchemaID:    "GoalPlanningSnapshot.v1",
		Version:     1,
		WorkspaceID: "ws_fixture",
		RunID:       "goal_snapshot_run",
		Round:       1,
		Goal:        "识别群聊中的明确需求",
		Scope:       GoalScope{GroupID: "20001"},
		Graph: GoalGraph{
			Assets:  []GoalAsset{{ID: "asset_group", Type: "qq_group", Label: "20001"}},
			Nodes:   []GoalNode{{ID: "goal_node", Kind: "goal", Label: "识别需求", State: "open"}},
			Edges:   []GoalEdge{{From: "goal_node", To: "asset_group", Kind: "targets"}},
			Anchors: []GoalAnchor{{NodeID: "goal_node", AssetID: "asset_group"}},
		},
		Evidence:       []GoalSnapshotEvidence{{ID: "evidence_1", Version: 1, Kind: "raw", Source: "raw_message", Text: "需求：课程", Round: 1}},
		Memory:         GoalMemory{Summary: "尚缺预算", InformationGaps: []string{"预算"}, UpdatedRound: 0},
		UnresolvedGaps: []string{"预算"},
		Limits:         GoalRunLimits{MaxRounds: 4, MaxSteps: 8, AttemptTimeoutSeconds: 30},
		Coverage:       GoalSnapshotCoverage{GraphComplete: true, EvidenceComplete: true},
	}
}

func testDecision(snapshot GoalPlanningSnapshot, decision GoalDecisionKind) GoalDecision {
	out := GoalDecision{
		SchemaID:     "GoalDecision.v1",
		WorkspaceID:  snapshot.WorkspaceID,
		RunID:        snapshot.RunID,
		Scope:        snapshot.Scope,
		Round:        snapshot.Round,
		Decision:     decision,
		EvidenceRefs: []GoalEvidenceReference{{ID: "evidence_1", Version: 1, Kind: "raw", Source: "raw_message"}},
		Rationale:    "依据当前证据决定下一步",
	}
	switch decision {
	case GoalDecisionAnalyze:
		out.AnalysisKind = "window_summary"
	case GoalDecisionWaitForInput, GoalDecisionRequestVerification:
		out.Request = "请补充或核对预算"
	case GoalDecisionBlocked, GoalDecisionSuggestComplete:
		out.Reason = "当前运行需要人工处理"
	}
	return out
}

func TestGoalPlanningSnapshotBoundsAndPreservesCounterevidence(t *testing.T) {
	graph := GoalGraph{Assets: []GoalAsset{{ID: "asset_group", Type: "qq_group", Label: "20001"}}}
	for i := 0; i < 70; i++ {
		graph.Nodes = append(graph.Nodes, GoalNode{ID: fmt.Sprintf("node_%d", i), Kind: "fact", Label: fmt.Sprintf("node %d", i), State: "open"})
		graph.Anchors = append(graph.Anchors, GoalAnchor{NodeID: fmt.Sprintf("node_%d", i), AssetID: "asset_group"})
	}
	for i := 0; i < 140; i++ {
		graph.Edges = append(graph.Edges, GoalEdge{From: fmt.Sprintf("node_%d", i%70), To: fmt.Sprintf("node_%d", (i+1)%70), Kind: "supports"})
	}
	evidence := make([]GoalSnapshotEvidence, 0, 70)
	for i := 0; i < 70; i++ {
		item := GoalSnapshotEvidence{ID: fmt.Sprintf("evidence_%d", i), Version: 1, Kind: "raw", Source: "raw_message", Text: fmt.Sprintf("证据 %d", i), Round: 1}
		if i == 0 {
			item.Kind = "conflict"
			item.Source = "conflict_report"
		}
		evidence = append(evidence, item)
	}
	snapshot, err := buildGoalPlanningSnapshot("ws_fixture", "goal_snapshot_bounds", 1, "核验目标", "20001", graph, evidence, GoalMemory{Conflicts: []GoalEvidenceConflict{{EvidenceIDs: []string{"evidence_0", "evidence_1"}, Summary: "两条证据互相矛盾"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Coverage.GraphComplete || snapshot.Coverage.EvidenceComplete || snapshot.Coverage.OmittedNodes != 6 || snapshot.Coverage.OmittedEdges != 12 || snapshot.Coverage.OmittedEvidence != 6 {
		t.Fatalf("incorrect coverage: %+v", snapshot.Coverage)
	}
	seen := map[string]bool{}
	for _, item := range snapshot.Evidence {
		seen[item.ID] = true
	}
	if len(snapshot.Evidence) != 64 || !seen["evidence_0"] || !seen["evidence_1"] {
		t.Fatalf("bounded evidence lost required counterevidence: len=%d seen=%v", len(snapshot.Evidence), seen)
	}
}

func TestGoalReasonerUsesSnapshotAndChangesDecisionWithEvidence(t *testing.T) {
	snapshot := testGoalSnapshot()
	first := testDecision(snapshot, GoalDecisionAnalyze)
	secondSnapshot := snapshot
	secondSnapshot.Evidence = append(secondSnapshot.Evidence, GoalSnapshotEvidence{ID: "evidence_2", Version: 1, Kind: "raw", Source: "raw_message", Text: "预算：1000", Round: 2})
	secondSnapshot.UnresolvedGaps = nil
	second := testDecision(secondSnapshot, GoalDecisionSuggestComplete)
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	provider := &recordingStructuredProvider{responses: []string{string(firstJSON), string(secondJSON)}}
	reasoner := NewGoalReasoner(provider)

	got, err := reasoner.Decide(context.Background(), snapshot)
	if err != nil || got.Decision != GoalDecisionAnalyze {
		t.Fatalf("first decision = %#v, err=%v", got, err)
	}
	got, err = reasoner.Decide(context.Background(), secondSnapshot)
	if err != nil || got.Decision != GoalDecisionSuggestComplete {
		t.Fatalf("second decision = %#v, err=%v", got, err)
	}
	if len(provider.calls) != 2 || !strings.Contains(provider.calls[0].UserPrompt, "evidence_1") || !strings.Contains(provider.calls[1].UserPrompt, "evidence_2") {
		t.Fatalf("snapshot was not sent as planning input: %#v", provider.calls)
	}
	if provider.calls[0].SchemaID != "GoalDecision.v1" || provider.calls[0].MaxTokens != goalReasonerMaxOutputTokens {
		t.Fatalf("unexpected structured call: %#v", provider.calls[0])
	}
}

func TestGoalReasonerRejectsInvalidDecisionAndEvidence(t *testing.T) {
	snapshot := testGoalSnapshot()
	cases := []struct {
		name string
		make func() string
	}{
		{"illegal decision", func() string {
			v := testDecision(snapshot, GoalDecisionAnalyze)
			v.Decision = "send_message"
			b, _ := json.Marshal(v)
			return string(b)
		}},
		{"forged evidence", func() string {
			v := testDecision(snapshot, GoalDecisionAnalyze)
			v.EvidenceRefs[0].ID = "not-in-snapshot"
			b, _ := json.Marshal(v)
			return string(b)
		}},
		{"extra field", func() string {
			v := testDecision(snapshot, GoalDecisionAnalyze)
			b, _ := json.Marshal(v)
			var object map[string]any
			_ = json.Unmarshal(b, &object)
			object["tool"] = "send_group_msg"
			b, _ = json.Marshal(object)
			return string(b)
		}},
		{"missing required request", func() string {
			v := testDecision(snapshot, GoalDecisionWaitForInput)
			v.Request = ""
			b, _ := json.Marshal(v)
			return string(b)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &recordingStructuredProvider{responses: []string{tc.make()}}
			_, err := NewGoalReasoner(provider).Decide(context.Background(), snapshot)
			if Code(err) != "GOAL_DECISION_INVALID" {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOpenAICompatibleStructuredProviderUsesBoundedJSONTransport(t *testing.T) {
	decision := testDecision(testGoalSnapshot(), GoalDecisionWaitForInput)
	body, _ := json.Marshal(decision)
	var seen openAICompatibleRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-key" {
			t.Errorf("authorization leaked or missing")
		}
		if r.Header.Get("X-BBA-Goal-Request-ID") != "goalreq_test" {
			t.Errorf("request identity missing: %q", r.Header.Get("X-BBA-Goal-Request-ID"))
		}
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Errorf("request decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"fake-goal","choices":[{"finish_reason":"stop","message":{"content":` + string(reasonerJSON(body)) + `}}],"usage":{"prompt_tokens":12,"completion_tokens":8}}`))
	}))
	defer server.Close()
	provider := fakePrivateProvider(t, server, time.Second)
	structured, ok := provider.(StructuredModelProvider)
	if !ok {
		t.Fatal("OpenAI-compatible provider does not expose structured goal calls")
	}
	completion, err := structured.CompleteStructured(context.Background(), StructuredModelCall{WorkspaceID: "ws_fixture", RequestID: "goalreq_test", SchemaID: "GoalDecision.v1", SystemPrompt: "system", UserPrompt: "snapshot evidence_1", MaxTokens: 123})
	if err != nil || completion.Content == "" {
		t.Fatalf("completion = %#v, err=%v", completion, err)
	}
	if seen.Model != "fake-goal" || seen.Stream || seen.ResponseFormat["type"] != "json_object" || len(seen.Messages) != 2 || !strings.Contains(seen.Messages[1].Content, "snapshot evidence_1") {
		t.Fatalf("unexpected request: %#v", seen)
	}
}

func reasonerJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestGoalModelServiceUsesFakeHTTPAndChangesDecisionWithNewEvidence(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request openAICompatibleRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("request decode: %v", err)
			return
		}
		if r.Header.Get("X-BBA-Goal-Request-ID") == "" {
			t.Errorf("goal request identity header missing")
		}
		user := request.Messages[1].Content
		start := strings.Index(user, "{")
		var snapshot GoalPlanningSnapshot
		if start < 0 || json.Unmarshal([]byte(user[start:]), &snapshot) != nil {
			t.Errorf("missing planning snapshot: %q", user)
			return
		}
		calls = append(calls, user)
		var content any
		refs := make([]GoalEvidenceReference, 0, len(snapshot.Evidence))
		for _, evidence := range snapshot.Evidence {
			refs = append(refs, GoalEvidenceReference{ID: evidence.ID, Version: evidence.Version, Kind: evidence.Kind, Source: evidence.Source})
		}
		if strings.Contains(user, "预算：") && strings.Contains(request.Messages[0].Content, "eleven keys") {
			content = GoalDecision{SchemaID: goalDecisionSchemaID, WorkspaceID: snapshot.WorkspaceID, RunID: snapshot.RunID, Scope: snapshot.Scope, Round: snapshot.Round, Decision: GoalDecisionSuggestComplete, EvidenceRefs: refs, Rationale: "预算证据已补充，建议由操作员确认完成", Reason: "当前窗口已覆盖目标所需字段"}
		} else if strings.Contains(request.Messages[0].Content, "eleven keys") {
			content = GoalDecision{SchemaID: goalDecisionSchemaID, WorkspaceID: snapshot.WorkspaceID, RunID: snapshot.RunID, Scope: snapshot.Scope, Round: snapshot.Round, Decision: GoalDecisionAnalyze, EvidenceRefs: refs, Rationale: "先分析当前窗口中的目标证据", AnalysisKind: "window_summary"}
		} else {
			gaps := []string{"预算"}
			if strings.Contains(user, "预算：") {
				gaps = nil
			}
			content = GoalAnalysis{SchemaID: "GoalAnalysis.v1", WorkspaceID: snapshot.WorkspaceID, RunID: snapshot.RunID, Scope: snapshot.Scope, Round: snapshot.Round, EvidenceRefs: refs, Summary: "按当前窗口完成有界需求分析", InformationGaps: gaps}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake-goal", "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": string(reasonerJSON(content))}}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 12}})
	}))
	defer server.Close()
	provider := fakePrivateProvider(t, server, time.Second)
	structured, ok := provider.(StructuredModelProvider)
	if !ok {
		t.Fatal("provider lacks structured support")
	}
	fixture, actor, run := goalFixture(t)
	modelService := NewGoalServiceWithReasoner(fixture.store, fixture.registry, NewGoalReasoner(structured))
	ctx := context.Background()
	if _, err := modelService.Inject(ctx, actor, run.ID, "fake-sparse", goalWindow("fake-sparse", "需求：课程")); err != nil {
		t.Fatal(err)
	}
	goalDrain(t, modelService)
	first, err := modelService.Get(ctx, actor, run.ID)
	if err != nil || first.State != "WAITING_INPUT" {
		t.Fatalf("first model round = %+v, err=%v", first, err)
	}
	if _, err := modelService.Inject(ctx, actor, run.ID, "fake-full", goalWindow("fake-full", "需求：课程；预算：1000；时间：本周")); err != nil {
		t.Fatal(err)
	}
	goalDrain(t, modelService)
	second, err := modelService.Get(ctx, actor, run.ID)
	if err != nil || second.State != "AWAITING_CONFIRMATION" {
		t.Fatalf("second model round = %+v, err=%v", second, err)
	}
	if len(calls) != 3 || !strings.Contains(calls[0], "GoalPlanningSnapshot.v1") || !strings.Contains(calls[2], "预算：") {
		t.Fatalf("fake HTTP did not receive changed snapshots: %d calls", len(calls))
	}
	var secondRound GoalPlanningSnapshot
	secondStart := strings.Index(calls[2], "{")
	if secondStart < 0 || json.Unmarshal([]byte(calls[2][secondStart:]), &secondRound) != nil || secondRound.PreviousDecision == nil || secondRound.PreviousDecision.Decision != GoalDecisionAnalyze {
		t.Fatalf("previous planner decision was not carried into the next snapshot: %+v", secondRound.PreviousDecision)
	}
	var contextVersion int
	var contextDigest string
	var contextSize int
	var modelRequestID string
	if err := fixture.store.Pool.QueryRow(ctx, "SELECT context_version,context_digest,pg_column_size(context_snapshot),model_request_id FROM core.goal_step WHERE workspace_id=$1 AND run_id=$2 AND model_request_id<>'' ORDER BY round DESC,id DESC LIMIT 1", actor.WorkspaceID, run.ID).Scan(&contextVersion, &contextDigest, &contextSize, &modelRequestID); err != nil {
		t.Fatal(err)
	}
	if contextVersion != 1 || len(contextDigest) != 64 || contextSize <= 0 || contextSize > goalReasonerMaxSnapshot || !strings.HasPrefix(modelRequestID, "goalreq_") {
		t.Fatalf("invalid persisted context metadata: version=%d digest=%q size=%d request=%q", contextVersion, contextDigest, contextSize, modelRequestID)
	}
}

type scriptedStructuredProvider struct {
	fn func(StructuredModelCall) (ModelCompletion, error)
}

func (p scriptedStructuredProvider) Complete(context.Context, ModelCall) (ModelCompletion, error) {
	return ModelCompletion{}, nil
}

func (p scriptedStructuredProvider) CompleteStructured(ctx context.Context, call StructuredModelCall) (ModelCompletion, error) {
	return p.fn(call)
}

func TestGoalAnalysisConflictNeverSuggestsComplete(t *testing.T) {
	g, actor, run := goalFixture(t)
	provider := scriptedStructuredProvider{fn: func(call StructuredModelCall) (ModelCompletion, error) {
		start := strings.Index(call.UserPrompt, "{")
		if start < 0 {
			return ModelCompletion{}, problem("GOAL_PROVIDER_FAILED")
		}
		var snapshot GoalPlanningSnapshot
		if err := json.Unmarshal([]byte(call.UserPrompt[start:]), &snapshot); err != nil {
			return ModelCompletion{}, err
		}
		refs := make([]GoalEvidenceReference, 0, len(snapshot.Evidence))
		for _, evidence := range snapshot.Evidence {
			refs = append(refs, GoalEvidenceReference{ID: evidence.ID, Version: evidence.Version, Kind: evidence.Kind, Source: evidence.Source})
		}
		if call.SchemaID == goalDecisionSchemaID {
			decision := GoalDecision{
				SchemaID: goalDecisionSchemaID, WorkspaceID: snapshot.WorkspaceID, RunID: snapshot.RunID,
				Scope: snapshot.Scope, Round: snapshot.Round, Decision: GoalDecisionAnalyze, EvidenceRefs: refs,
				Rationale: "先分析当前窗口中的目标证据", AnalysisKind: "window_summary",
			}
			return ModelCompletion{Content: string(reasonerJSON(decision))}, nil
		}
		if len(refs) < 2 {
			return ModelCompletion{}, problem("GOAL_PROVIDER_FAILED")
		}
		analysis := GoalAnalysis{
			SchemaID: "GoalAnalysis.v1", WorkspaceID: snapshot.WorkspaceID, RunID: snapshot.RunID,
			Scope: snapshot.Scope, Round: snapshot.Round, EvidenceRefs: refs, Summary: "证据存在矛盾，需要核对",
			Conflicts: []GoalEvidenceConflict{{EvidenceIDs: []string{refs[0].ID, refs[1].ID}, Summary: "两条消息对预算描述相反"}},
		}
		return ModelCompletion{Content: string(reasonerJSON(analysis))}, nil
	}}
	modelService := NewGoalServiceWithReasoner(g.store, g.registry, NewGoalReasoner(provider))
	window := goalWindow("conflict", "预算：1000")
	now := time.Now().UTC()
	window.Messages = append(window.Messages, GoalMessage{ID: "conflict_msg_2", At: now.Add(-2 * time.Second), Text: "预算：不确定"})
	if _, err := modelService.Inject(context.Background(), actor, run.ID, "conflict", window); err != nil {
		t.Fatal(err)
	}
	goalDrain(t, modelService)
	got, err := modelService.Get(context.Background(), actor, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "WAITING_INPUT" || !strings.Contains(got.Reason, "证据矛盾需核对") {
		t.Fatalf("conflict must require verification, got state=%s reason=%q", got.State, got.Reason)
	}
}

func TestGoalReasonerMapsFakeHTTPFailuresWithoutRetrying(t *testing.T) {
	for _, status := range []int{429, 500} {
		t.Run(string(rune(status)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			provider := fakePrivateProvider(t, server, time.Second)
			structured := provider.(StructuredModelProvider)
			_, err := NewGoalReasoner(structured).Decide(context.Background(), testGoalSnapshot())
			if Code(err) != "GOAL_PROVIDER_FAILED" {
				t.Fatalf("status %d error = %v", status, err)
			}
		})
	}
}

func TestGoalReasonerFakeHTTPRejectsMalformedResponsesAndTransportFaults(t *testing.T) {
	snapshot := testGoalSnapshot()
	valid := testDecision(snapshot, GoalDecisionAnalyze)
	validRaw := reasonerJSON(valid)
	cases := []struct {
		name      string
		status    int
		content   []byte
		handler   func(http.ResponseWriter, *http.Request)
		wantError string
		timeout   time.Duration
	}{
		{name: "invalid json", content: []byte("{"), wantError: "GOAL_DECISION_INVALID"},
		{name: "extra field", content: func() []byte {
			var object map[string]any
			_ = json.Unmarshal(validRaw, &object)
			object["tool"] = "send_group_msg"
			return reasonerJSON(object)
		}(), wantError: "GOAL_DECISION_INVALID"},
		{name: "illegal decision", content: func() []byte {
			var object map[string]any
			_ = json.Unmarshal(validRaw, &object)
			object["decision"] = "send_message"
			return reasonerJSON(object)
		}(), wantError: "GOAL_DECISION_INVALID"},
		{name: "forged evidence", content: func() []byte { invalid := valid; invalid.EvidenceRefs[0].ID = "forged"; return reasonerJSON(invalid) }(), wantError: "GOAL_DECISION_INVALID"},
		{name: "rate limited", status: 429, wantError: "GOAL_PROVIDER_FAILED"},
		{name: "server error", status: 500, wantError: "GOAL_PROVIDER_FAILED"},
		{name: "timeout", handler: func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }, wantError: "GOAL_PROVIDER_FAILED", timeout: 30 * time.Millisecond},
		{name: "disconnect", handler: func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, wantError: "GOAL_PROVIDER_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.handler != nil {
					tc.handler(w, r)
					return
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake-goal", "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": string(tc.content)}}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1}})
			}))
			defer server.Close()
			timeout := tc.timeout
			if timeout == 0 {
				timeout = time.Second
			}
			provider := fakePrivateProvider(t, server, timeout)
			_, err := NewGoalReasoner(provider.(StructuredModelProvider)).Decide(context.Background(), snapshot)
			if Code(err) != tc.wantError {
				t.Fatalf("error = %v, want %s", err, tc.wantError)
			}
		})
	}
}
