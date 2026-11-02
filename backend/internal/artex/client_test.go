package artex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

const (
	testBaseURL = "http://artex.test.invalid:8787"
	testToken   = "fixture-token"
)

// replayTransport serves recorded ARTEX replies and records what the adapter
// asked for. It is the only transport used by these tests: no dial, no network.
type replayTransport struct {
	mu       sync.Mutex
	requests []recordedRequest
	respond  func(attempt int, r *http.Request) (*http.Response, error)
}

type recordedRequest struct {
	Method    string
	URL       *url.URL
	Header    http.Header
	HasBody   bool
	AuthValue string
}

func (rt *replayTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	recorded := recordedRequest{
		Method:    r.Method,
		URL:       r.URL,
		Header:    r.Header.Clone(),
		HasBody:   r.Body != nil && r.Body != http.NoBody,
		AuthValue: r.Header.Get("Authorization"),
	}
	rt.requests = append(rt.requests, recorded)
	attempt := len(rt.requests)
	rt.mu.Unlock()
	if rt.respond == nil {
		return nil, errors.New("replay transport has no prepared response")
	}
	return rt.respond(attempt, r)
}

func (rt *replayTransport) count() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.requests)
}

func (rt *replayTransport) request(i int) recordedRequest {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.requests[i]
}

func (rt *replayTransport) last() recordedRequest { return rt.request(rt.count() - 1) }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func jsonReply(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func bodyReply(status int, body string) *http.Response { return jsonReply(status, []byte(body)) }

// newTestClient builds a wired client whose backoff is disabled so retry paths
// are exercised without wall-clock waits.
func newTestClient(t *testing.T, rt http.RoundTripper) *Client {
	t.Helper()
	client, err := New(Config{
		BaseURL:   testBaseURL,
		Token:     testToken,
		Timeout:   2 * time.Second,
		Retry:     RetryPolicy{MaxAttempts: 3, BaseDelay: time.Nanosecond},
		Transport: rt,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.retry.BaseDelay = 0
	return client
}

// ---- protocol mapping ----

func TestNodesMapsARTEXPage(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return jsonReply(http.StatusOK, fixture(t, "nodes_page.json")), nil
	}}
	client := newTestClient(t, rt)

	page, err := client.Nodes(context.Background(), NodeQuery{
		TaskID: "17",
		Kinds:  []string{KindFact},
		States: []string{"confirmed"},
		Query:  "nginx",
		Page:   1,
		Size:   2,
	})
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	if got := rt.count(); got != 1 {
		t.Fatalf("requests=%d, want 1", got)
	}
	req := rt.last()
	if req.Method != http.MethodGet {
		t.Fatalf("method=%s, want GET (adapter is read-only)", req.Method)
	}
	if req.HasBody {
		t.Fatal("read request carried a body")
	}
	if req.URL.Path != "/api/exploration/nodes" {
		t.Fatalf("path=%s", req.URL.Path)
	}
	query := req.URL.Query()
	want := map[string]string{
		"task":  "17",
		"kind":  "fact",
		"state": "confirmed",
		"q":     "nginx",
		"page":  "1",
		"size":  "2",
	}
	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Fatalf("query %s=%q, want %q", key, got, value)
		}
	}
	if auth := req.Header.Get("Authorization"); auth != "Bearer "+testToken {
		t.Fatalf("Authorization=%q, want bearer token (ARTEX/server/auth.go:108-118)", auth)
	}
	if accept := req.Header.Get("Accept"); accept != "application/json" {
		t.Fatalf("Accept=%q", accept)
	}

	if page.Total != 2 || page.Page != 1 || page.Size != 2 {
		t.Fatalf("page envelope=%+v", page)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items=%d, want 2", len(page.Items))
	}
	if len(page.Edges) != 2 {
		t.Fatalf("edges=%d, want 2", len(page.Edges))
	}

	// Required fields of the wire shape (id/type/state/ts) must survive mapping.
	inherited := page.Items[0]
	if inherited.ID != 418 || inherited.Kind != KindFact || inherited.State != "confirmed" {
		t.Fatalf("item[0]=%+v", inherited)
	}
	if inherited.Origin != "worker" || inherited.Priority != 0 {
		t.Fatalf("item[0] origin/priority=%+v", inherited)
	}
	if inherited.SourceTaskID != 17 || !inherited.Inherited {
		t.Fatalf("item[0] provenance=%+v", inherited)
	}
	wantTS := time.Date(2026, 10, 20, 9, 14, 3, 0, time.UTC)
	if !inherited.TS.Equal(wantTS) || inherited.TS.Location() != time.UTC {
		t.Fatalf("item[0] ts=%v, want %v UTC", inherited.TS, wantTS)
	}
	const wantPayload = `{"summary":"10.0.0.5:8080 返回 200，指纹 nginx/1.24.0","confidence":"observed"}`
	if string(inherited.Payload) != wantPayload {
		t.Fatalf("item[0] payload=%s", inherited.Payload)
	}

	// Nullable wire field: payload JSON null maps to an absent payload, and the
	// omitted source_task_id/inherited flags stay at their zero values.
	minimal := page.Items[1]
	if minimal.ID != 417 || minimal.Kind != KindFact {
		t.Fatalf("item[1]=%+v", minimal)
	}
	if minimal.Payload != nil {
		t.Fatalf("item[1] payload=%q, want nil for JSON null", minimal.Payload)
	}
	if minimal.SourceTaskID != 0 || minimal.Inherited || minimal.DeleteReason != "" {
		t.Fatalf("item[1] optional fields=%+v", minimal)
	}
	if minimal.Origin != "" {
		t.Fatalf("item[1] origin=%q, want empty string", minimal.Origin)
	}

	// Neighbour refs are the same node shape keyed by decimal id.
	if len(page.Refs) != 1 {
		t.Fatalf("refs=%d, want 1", len(page.Refs))
	}
	ref, ok := page.Refs[416]
	if !ok {
		t.Fatalf("refs keys=%v", page.Refs)
	}
	if ref.Kind != KindIntent || ref.State != "done" || ref.Priority != 3 {
		t.Fatalf("ref[416]=%+v", ref)
	}

	// Unconsumed envelope members are preserved, not invented.
	if len(page.Assets) == 0 {
		t.Fatalf("assets not preserved: %q", page.Assets)
	}
	var assets map[string][]struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(page.Assets, &assets); err != nil {
		t.Fatalf("assets decode: %v", err)
	}
	if len(assets["418"]) != 1 || assets["418"][0].Label != "10.0.0.5:8080" {
		t.Fatalf("assets=%+v", assets)
	}

	edge := page.Edges[0]
	if edge != (Edge{Src: 416, Dst: 418, Rel: RelYields}) {
		t.Fatalf("edge[0]=%+v", edge)
	}
}

func TestGraphMapsARTEXGraph(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return jsonReply(http.StatusOK, fixture(t, "graph.json")), nil
	}}
	client := newTestClient(t, rt)

	graph, err := client.Graph(context.Background(), "17")
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	req := rt.last()
	if req.Method != http.MethodGet || req.URL.Path != "/api/exploration/graph" {
		t.Fatalf("request=%s %s", req.Method, req.URL.Path)
	}
	if got := req.URL.Query().Get("task"); got != "17" {
		t.Fatalf("task=%q", got)
	}
	if len(graph.Nodes) != 3 || len(graph.Edges) != 3 {
		t.Fatalf("graph=%+v", graph)
	}
	if graph.Nodes[0].ID != 1 || graph.Nodes[0].State != "origin" || graph.Nodes[0].Kind != KindFact {
		t.Fatalf("origin node=%+v", graph.Nodes[0])
	}
	if string(graph.Nodes[0].Payload) != `{"summary":"任务根节点：目标 example.internal","confidence":"observed"}` {
		t.Fatalf("origin payload=%s", graph.Nodes[0].Payload)
	}
	if graph.Nodes[1].Kind != KindIntent || graph.Nodes[1].Priority != 5 {
		t.Fatalf("intent node=%+v", graph.Nodes[1])
	}
	if graph.Edges[2] != (Edge{Src: 2, Dst: 1, Rel: RelDerivedFrom}) {
		t.Fatalf("edge[2]=%+v", graph.Edges[2])
	}
}

func TestGraphEmptyIsNotAnError(t *testing.T) {
	// ARTEX answers HTTP 200 with empty arrays for an unresolvable task
	// (ARTEX/server/server.go:2633-2637); the adapter must not invent an error or
	// a "task missing" signal from that.
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"nodes":[],"edges":[]}`), nil
	}}
	client := newTestClient(t, rt)
	graph, err := client.Graph(context.Background(), "404-task")
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if len(graph.Nodes) != 0 || len(graph.Edges) != 0 {
		t.Fatalf("graph=%+v", graph)
	}
}

func TestNodeByIDUsesNumericQAndVerifiesID(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return jsonReply(http.StatusOK, fixture(t, "node_by_id.json")), nil
	}}
	client := newTestClient(t, rt)

	node, err := client.Node(context.Background(), "17", 41)
	if err != nil {
		t.Fatalf("Node: %v", err)
	}
	req := rt.last()
	if req.URL.Path != "/api/exploration/nodes" {
		t.Fatalf("path=%s", req.URL.Path)
	}
	query := req.URL.Query()
	if query.Get("q") != "41" || query.Get("page") != "1" || query.Get("size") != strconv.Itoa(maxPageSize) {
		t.Fatalf("query=%v", query)
	}
	if query.Get("kind") != "" || query.Get("state") != "" || query.Get("order") != "" {
		t.Fatalf("unexpected filters on single-node read: %v", query)
	}
	// The fixture also contains id 17 whose payload mentions "41": only the exact
	// id match may be returned.
	if node.ID != 41 || node.State != "dismissed" {
		t.Fatalf("node=%+v", node)
	}
	if string(node.Payload) != `{"summary":"10.0.0.5:41 端口关闭","confidence":"observed"}` {
		t.Fatalf("payload=%s", node.Payload)
	}
}

func TestNodeByIDAbsentFromShortPageIsNotFound(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"items":[],"total":0,"page":1,"size":200,"edges":[],"refs":{},"assets":{}}`), nil
	}}
	client := newTestClient(t, rt)
	_, err := client.Node(context.Background(), "17", 999)
	if !HasCode(err, CodeNotFound) {
		t.Fatalf("err=%v, want %s", err, CodeNotFound)
	}
}

func TestNodeByIDSaturatedPageIsIndeterminate(t *testing.T) {
	// The numeric-q filter is an OR with a payload substring match, so a saturated
	// page cannot prove absence: the adapter must not report a false not-found.
	items := make([]map[string]any, 0, maxPageSize)
	for i := 1; i <= maxPageSize; i++ {
		items = append(items, map[string]any{
			"id":       strconv.Itoa(i),
			"type":     KindFact,
			"payload":  `{"summary":"999 号线索复查"}`,
			"priority": 0,
			"state":    "confirmed",
			"origin":   "worker",
			"ts":       "2026-10-20T09:00:00Z",
		})
	}
	payload, err := json.Marshal(map[string]any{
		"items": items, "total": maxPageSize, "page": 1, "size": maxPageSize,
		"edges": []any{}, "refs": map[string]any{}, "assets": map[string]any{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return jsonReply(http.StatusOK, payload), nil
	}}
	client := newTestClient(t, rt)
	_, err = client.Node(context.Background(), "17", 999)
	if !HasCode(err, CodeIndeterminate) {
		t.Fatalf("err=%v, want %s", err, CodeIndeterminate)
	}
}

func TestNodesDefaultsAndClamp(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"items":[],"total":0,"page":1,"size":200,"edges":[],"refs":{},"assets":{}}`), nil
	}}
	client := newTestClient(t, rt)

	if _, err := client.Nodes(context.Background(), NodeQuery{TaskID: "17"}); err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	// No page/size → ARTEX applies its own defaults (page=1, size=20).
	query := rt.last().URL.Query()
	if _, ok := query["page"]; ok {
		t.Fatalf("page sent without a caller value: %v", query)
	}
	if _, ok := query["size"]; ok {
		t.Fatalf("size sent without a caller value: %v", query)
	}
	if _, ok := query["order"]; ok {
		t.Fatalf("order sent for the default newest-first read: %v", query)
	}

	if _, err := client.Nodes(context.Background(), NodeQuery{TaskID: "17", Page: 2, Size: 5000, Asc: true}); err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	query = rt.last().URL.Query()
	if query.Get("page") != "2" || query.Get("order") != "asc" {
		t.Fatalf("query=%v", query)
	}
	if got := query.Get("size"); got != strconv.Itoa(maxPageSize) {
		t.Fatalf("size=%q, want ARTEX ceiling %d", got, maxPageSize)
	}
}

func TestKindFilterIsCommaSeparated(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"items":[],"total":0,"page":1,"size":20,"edges":[],"refs":{},"assets":{}}`), nil
	}}
	client := newTestClient(t, rt)
	_, err := client.Nodes(context.Background(), NodeQuery{
		TaskID: "17",
		Kinds:  []string{KindFact, " ", KindFinding},
		States: []string{"confirmed"},
	})
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	query := rt.last().URL.Query()
	if got := query.Get("kind"); got != "fact,finding" {
		t.Fatalf("kind=%q", got)
	}
	if got := query.Get("state"); got != "confirmed" {
		t.Fatalf("state=%q", got)
	}
}

// ---- unwired vs wired-but-failing ----

func TestNotWiredPerformsNoIO(t *testing.T) {
	rt := &replayTransport{}
	ctx := context.Background()

	noConfig, err := New(Config{Transport: rt})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if noConfig.Wired() {
		t.Fatal("client without base URL/token reports itself wired")
	}
	urlOnly, err := New(Config{BaseURL: testBaseURL, Transport: rt})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// ARTEX requires the JWT on every /api/* read (ARTEX/server/auth.go:120-137),
	// so a token-less client is not wired either.
	if urlOnly.Wired() {
		t.Fatal("client without a token reports itself wired")
	}

	for name, client := range map[string]*Client{"empty": noConfig, "url-only": urlOnly} {
		if _, err := client.Nodes(ctx, NodeQuery{TaskID: "17", Kinds: []string{KindFact}}); !HasCode(err, CodeNotConfigured) || !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("%s Nodes err=%v, want %s wrapping ErrNotConfigured", name, err, CodeNotConfigured)
		}
		if _, err := client.Graph(ctx, "17"); !HasCode(err, CodeNotConfigured) {
			t.Fatalf("%s Graph err=%v", name, err)
		}
		if _, err := client.Node(ctx, "17", 41); !HasCode(err, CodeNotConfigured) {
			t.Fatalf("%s Node err=%v", name, err)
		}
	}
	if got := rt.count(); got != 0 {
		t.Fatalf("not-wired client issued %d request(s)", got)
	}
}

func TestInvalidRequestsAreRejectedLocally(t *testing.T) {
	rt := &replayTransport{}
	client := newTestClient(t, rt)
	ctx := context.Background()

	if _, err := client.Nodes(ctx, NodeQuery{}); !HasCode(err, CodeInvalidRequest) {
		t.Fatalf("empty task Nodes err=%v", err)
	}
	if _, err := client.Graph(ctx, "   "); !HasCode(err, CodeInvalidRequest) {
		t.Fatalf("empty task Graph err=%v", err)
	}
	if _, err := client.Node(ctx, "17", 0); !HasCode(err, CodeInvalidRequest) {
		t.Fatalf("zero node id err=%v", err)
	}
	if _, err := client.Nodes(ctx, NodeQuery{TaskID: "17", Size: -1}); !HasCode(err, CodeInvalidRequest) {
		t.Fatalf("negative size err=%v", err)
	}
	if got := rt.count(); got != 0 {
		t.Fatalf("invalid requests issued %d I/O call(s)", got)
	}
}

func TestUnauthorizedIsNotRetried(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return jsonReply(http.StatusUnauthorized, fixture(t, "error_unauthorized.json")), nil
	}}
	client := newTestClient(t, rt)

	_, err := client.Nodes(context.Background(), NodeQuery{TaskID: "17", Kinds: []string{KindFact}})
	if !HasCode(err, CodeUnauthorized) {
		t.Fatalf("err=%v, want %s", err, CodeUnauthorized)
	}
	var artexErr *Error
	if !errors.As(err, &artexErr) {
		t.Fatalf("err type=%T", err)
	}
	if artexErr.HTTPStatus != http.StatusUnauthorized || artexErr.Detail != "未授权" || artexErr.Attempts != 1 {
		t.Fatalf("error=%+v", artexErr)
	}
	if got := rt.count(); got != 1 {
		t.Fatalf("401 was retried: requests=%d", got)
	}
}

func TestNonRetryableFailuresMakeOneAttempt(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		code   Code
	}{
		{"bad request", http.StatusBadRequest, `{"error":"bad task id"}`, CodeRequestRejected},
		{"forbidden", http.StatusForbidden, `{"error":"forbidden"}`, CodeForbidden},
		{"not found", http.StatusNotFound, `{"error":"not found"}`, CodeNotFound},
		{"internal error", http.StatusInternalServerError, `{"error":"boom"}`, CodeUpstreamError},
		{"not modified", http.StatusNotModified, ``, CodeInvalidResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
				return bodyReply(tc.status, tc.body), nil
			}}
			client := newTestClient(t, rt)
			_, err := client.Graph(context.Background(), "17")
			if !HasCode(err, tc.code) {
				t.Fatalf("err=%v, want %s", err, tc.code)
			}
			var artexErr *Error
			if !errors.As(err, &artexErr) || artexErr.Attempts != 1 || artexErr.HTTPStatus != tc.status {
				t.Fatalf("error=%+v", artexErr)
			}
			if got := rt.count(); got != 1 {
				t.Fatalf("non-retryable status %d retried: requests=%d", tc.status, got)
			}
		})
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		response := bodyReply(http.StatusFound, ``)
		response.Header.Set("Location", "http://elsewhere.test/collect")
		return response, nil
	}}
	client := newTestClient(t, rt)
	_, err := client.Graph(context.Background(), "17")
	if !HasCode(err, CodeInvalidResponse) {
		t.Fatalf("err=%v, want %s", err, CodeInvalidResponse)
	}
	if got := rt.count(); got != 1 {
		t.Fatalf("redirect was followed: requests=%d", got)
	}
}

func TestRetryableStatusRecovers(t *testing.T) {
	rt := &replayTransport{respond: func(attempt int, _ *http.Request) (*http.Response, error) {
		if attempt < 3 {
			return bodyReply(http.StatusServiceUnavailable, `{"error":"restarting"}`), nil
		}
		return jsonReply(http.StatusOK, fixture(t, "graph.json")), nil
	}}
	client := newTestClient(t, rt)

	graph, err := client.Graph(context.Background(), "17")
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if len(graph.Nodes) != 3 {
		t.Fatalf("nodes=%d", len(graph.Nodes))
	}
	if got := rt.count(); got != 3 {
		t.Fatalf("requests=%d, want 3", got)
	}
}

func TestRetryBudgetExhausted(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusBadGateway, `{"error":"upstream down"}`), nil
	}}
	client := newTestClient(t, rt)

	_, err := client.Nodes(context.Background(), NodeQuery{TaskID: "17"})
	if !HasCode(err, CodeUnavailable) {
		t.Fatalf("err=%v, want %s", err, CodeUnavailable)
	}
	var artexErr *Error
	if !errors.As(err, &artexErr) {
		t.Fatalf("err type=%T", err)
	}
	if artexErr.Attempts != 3 || artexErr.HTTPStatus != http.StatusBadGateway || artexErr.Detail != "upstream down" {
		t.Fatalf("error=%+v", artexErr)
	}
	if got := rt.count(); got != 3 {
		t.Fatalf("requests=%d, want 3", got)
	}
}

func TestTransportErrorIsRetried(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp 127.0.0.1:8787: connect: connection refused")
	}}
	client := newTestClient(t, rt)

	_, err := client.Graph(context.Background(), "17")
	if !HasCode(err, CodeUnavailable) {
		t.Fatalf("err=%v, want %s", err, CodeUnavailable)
	}
	var artexErr *Error
	if !errors.As(err, &artexErr) || artexErr.Attempts != 3 || artexErr.HTTPStatus != 0 {
		t.Fatalf("error=%+v", artexErr)
	}
	if got := rt.count(); got != 3 {
		t.Fatalf("requests=%d, want 3", got)
	}
}

func TestPerAttemptTimeoutIsRetried(t *testing.T) {
	rt := &replayTransport{respond: func(_ int, r *http.Request) (*http.Response, error) {
		<-r.Context().Done() // per-attempt deadline (not the caller's context)
		return nil, r.Context().Err()
	}}
	client, err := New(Config{
		BaseURL:   testBaseURL,
		Token:     testToken,
		Timeout:   10 * time.Millisecond,
		Retry:     RetryPolicy{MaxAttempts: 2, BaseDelay: time.Nanosecond},
		Transport: rt,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.retry.BaseDelay = 0

	_, err = client.Graph(context.Background(), "17")
	if !HasCode(err, CodeUnavailable) {
		t.Fatalf("err=%v, want %s", err, CodeUnavailable)
	}
	var artexErr *Error
	if !errors.As(err, &artexErr) || artexErr.Attempts != 2 {
		t.Fatalf("error=%+v", artexErr)
	}
	if got := rt.count(); got != 2 {
		t.Fatalf("requests=%d, want 2", got)
	}
}

func TestCallerCancellationStopsImmediately(t *testing.T) {
	rt := &replayTransport{respond: func(_ int, r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}}
	client := newTestClient(t, rt)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Graph(ctx, "17")
	if !HasCode(err, CodeCanceled) {
		t.Fatalf("err=%v, want %s", err, CodeCanceled)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v does not wrap context.Canceled", err)
	}
	if got := rt.count(); got != 1 {
		t.Fatalf("requests=%d, want 1 (no retry after caller cancellation)", got)
	}
}

// ---- received-but-invalid replies are never replayed ----

func readErrorBody() io.ReadCloser { return io.NopCloser(errReader{}) }

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("connection reset while reading body")
}

func TestUnreadableBodyIsNotReplayed(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: readErrorBody()}, nil
	}}
	client := newTestClient(t, rt)

	_, err := client.Graph(context.Background(), "17")
	if !HasCode(err, CodeUnavailable) {
		t.Fatalf("err=%v, want %s", err, CodeUnavailable)
	}
	var artexErr *Error
	if !errors.As(err, &artexErr) || artexErr.Attempts != 1 || artexErr.Detail != "response body could not be read" {
		t.Fatalf("error=%+v", artexErr)
	}
	if got := rt.count(); got != 1 {
		t.Fatalf("unknown outcome was replayed: requests=%d", got)
	}
}

func TestInvalidJSONIsNotRetried(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"nodes":[`), nil
	}}
	client := newTestClient(t, rt)
	_, err := client.Graph(context.Background(), "17")
	if !HasCode(err, CodeInvalidResponse) {
		t.Fatalf("err=%v, want %s", err, CodeInvalidResponse)
	}
	if got := rt.count(); got != 1 {
		t.Fatalf("malformed reply retried: requests=%d", got)
	}
}

func TestTrailingContentIsRejected(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"nodes":[],"edges":[]}{"nodes":[]}`), nil
	}}
	client := newTestClient(t, rt)
	_, err := client.Graph(context.Background(), "17")
	if !HasCode(err, CodeInvalidResponse) {
		t.Fatalf("err=%v, want %s", err, CodeInvalidResponse)
	}
	if got := rt.count(); got != 1 {
		t.Fatalf("requests=%d, want 1", got)
	}
}

func TestMissingRequiredFieldIsRejected(t *testing.T) {
	// "ts" is required on the wire (ARTEX/server/dto.go:196-208).
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"nodes":[{"id":"7","type":"fact","payload":"{}","priority":0,"state":"confirmed","origin":""}],"edges":[]}`), nil
	}}
	client := newTestClient(t, rt)
	_, err := client.Graph(context.Background(), "17")
	if !HasCode(err, CodeInvalidResponse) {
		t.Fatalf("err=%v, want %s", err, CodeInvalidResponse)
	}
	if got := rt.count(); got != 1 {
		t.Fatalf("requests=%d, want 1", got)
	}
}

func TestPayloadMustBeJSONText(t *testing.T) {
	rt := &replayTransport{respond: func(int, *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"nodes":[{"id":"7","type":"fact","payload":"not json","priority":0,"state":"confirmed","origin":"","ts":"2026-10-20T09:00:00Z"}],"edges":[]}`), nil
	}}
	client := newTestClient(t, rt)
	_, err := client.Graph(context.Background(), "17")
	if !HasCode(err, CodeInvalidResponse) {
		t.Fatalf("err=%v, want %s", err, CodeInvalidResponse)
	}
}

// ---- read-only guarantee and constructor validation ----

func TestEveryRequestIsReadOnly(t *testing.T) {
	rt := &replayTransport{respond: func(_ int, r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case pathNodes:
			return jsonReply(http.StatusOK, fixture(t, "nodes_page.json")), nil
		default:
			return jsonReply(http.StatusOK, fixture(t, "graph.json")), nil
		}
	}}
	client := newTestClient(t, rt)
	ctx := context.Background()
	if _, err := client.Graph(ctx, "17"); err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if _, err := client.Nodes(ctx, NodeQuery{TaskID: "17", Kinds: []string{KindFact}, Size: 1}); err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if _, err := client.Node(ctx, "17", 418); err != nil {
		t.Fatalf("Node: %v", err)
	}
	for i := range rt.count() {
		req := rt.request(i)
		if req.Method != http.MethodGet {
			t.Fatalf("request %d used %s: the adapter must be read-only", i, req.Method)
		}
		if req.HasBody {
			t.Fatalf("request %d carried a body", i)
		}
	}
}

func TestNewRejectsInvalidBaseURL(t *testing.T) {
	cases := []string{
		"127.0.0.1:8787",
		"ftp://artex.test.invalid",
		"http://user:pass@artex.test.invalid",
		"http://artex.test.invalid?x=1",
		"http://artex.test.invalid#frag",
	}
	for _, raw := range cases {
		if _, err := New(Config{BaseURL: raw, Token: testToken}); err == nil {
			t.Fatalf("New accepted base URL %q", raw)
		}
	}
	client, err := New(Config{BaseURL: testBaseURL + "/", Token: testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.baseURL != testBaseURL {
		t.Fatalf("baseURL=%q, want trailing slash trimmed", client.baseURL)
	}
}

func TestRetryPolicyBounds(t *testing.T) {
	for _, maxAttempts := range []int{-3, 0, 1, 9} {
		normalized := RetryPolicy{MaxAttempts: maxAttempts}.normalize()
		want := maxAttempts
		switch {
		case maxAttempts <= 0:
			want = defaultMaxAttempts
		case maxAttempts > 5:
			want = 5
		}
		if normalized.MaxAttempts != want {
			t.Fatalf("MaxAttempts %d normalized to %d, want %d", maxAttempts, normalized.MaxAttempts, want)
		}
	}
	policy := RetryPolicy{BaseDelay: 100 * time.Millisecond}.normalize()
	if got := policy.delay(2); got != 100*time.Millisecond {
		t.Fatalf("delay(2)=%v", got)
	}
	if got := policy.delay(3); got != 200*time.Millisecond {
		t.Fatalf("delay(3)=%v", got)
	}
	if got := policy.delay(10); got != maxBaseDelay {
		t.Fatalf("delay(10)=%v, want cap %v", got, maxBaseDelay)
	}
	if got := policy.delay(1); got != 0 {
		t.Fatalf("delay(1)=%v, want 0", got)
	}
}

func TestResponseEnvelopeShapeIsDocumentedByFixture(t *testing.T) {
	// Guards the fixtures themselves: they must stay faithful to the wire shape
	// derived from ARTEX/server/dto.go:183-211,268-277 and
	// ARTEX/server/server.go:2731-2741 (decimal-string ids, RFC3339 ts, string
	// payload holding JSON text, refs keyed by id).
	for _, name := range []string{"nodes_page.json", "graph.json", "node_by_id.json"} {
		var envelope struct {
			Nodes []map[string]json.RawMessage `json:"nodes"`
			Items []map[string]json.RawMessage `json:"items"`
			Edges []map[string]json.RawMessage `json:"edges"`
			Refs  map[string]json.RawMessage   `json:"refs"`
		}
		if err := json.Unmarshal(fixture(t, name), &envelope); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		nodes := envelope.Nodes
		if len(nodes) == 0 {
			nodes = envelope.Items
		}
		if len(nodes) == 0 {
			t.Fatalf("%s: fixture has no nodes", name)
		}
		for i, node := range nodes {
			for _, field := range []string{"id", "type", "payload", "priority", "state", "origin", "ts"} {
				if _, ok := node[field]; !ok {
					t.Fatalf("%s node %d missing required wire field %q", name, i, field)
				}
			}
			var id string
			if err := json.Unmarshal(node["id"], &id); err != nil {
				t.Fatalf("%s node %d id is not a decimal string: %v", name, i, err)
			}
			if _, err := strconv.ParseInt(id, 10, 64); err != nil {
				t.Fatalf("%s node %d id %q is not decimal", name, i, id)
			}
			var ts string
			if err := json.Unmarshal(node["ts"], &ts); err != nil {
				t.Fatalf("%s node %d ts is not a string: %v", name, i, err)
			}
			if _, err := time.Parse(time.RFC3339, ts); err != nil {
				t.Fatalf("%s node %d ts %q is not RFC3339", name, i, ts)
			}
			var payload string
			if err := json.Unmarshal(node["payload"], &payload); err != nil {
				t.Fatalf("%s node %d payload is not a JSON string: %v", name, i, err)
			}
			if payload != "null" && !json.Valid([]byte(payload)) {
				t.Fatalf("%s node %d payload %q is not valid JSON text", name, i, payload)
			}
		}
		for i, edge := range envelope.Edges {
			for _, field := range []string{"src", "dst", "rel"} {
				if _, ok := edge[field]; !ok {
					t.Fatalf("%s edge %d missing %q", name, i, field)
				}
			}
		}
	}
}

func TestWireConversionRejectsBadIDsAndRefs(t *testing.T) {
	cases := map[string]nodeWire{
		"zero id":        {ID: "0", Type: KindFact, State: "confirmed", TS: "2026-10-20T09:00:00Z"},
		"non-decimal id": {ID: "4a", Type: KindFact, State: "confirmed", TS: "2026-10-20T09:00:00Z"},
		"empty type":     {ID: "4", State: "confirmed", TS: "2026-10-20T09:00:00Z"},
		"empty state":    {ID: "4", Type: KindFact, TS: "2026-10-20T09:00:00Z"},
		"empty ts":       {ID: "4", Type: KindFact, State: "confirmed"},
		"bad ts":         {ID: "4", Type: KindFact, State: "confirmed", TS: "2026-10-20 09:00:00"},
	}
	for name, wire := range cases {
		if _, err := wire.toNode(); err == nil {
			t.Fatalf("%s: toNode accepted the wire value", name)
		}
	}
	if _, err := (nodesPageWire{Refs: map[string]nodeWire{"9": {ID: "8", Type: KindFact, State: "confirmed", TS: "2026-10-20T09:00:00Z"}}}).toPage(); err == nil {
		t.Fatal("toPage accepted a refs key that disagrees with the node id")
	}
	if _, err := (edgeWire{Src: "1", Dst: "x", Rel: RelYields}).toEdge(); err == nil {
		t.Fatal("toEdge accepted a non-decimal dst")
	}
	if _, err := (edgeWire{Src: "1", Dst: "2", Rel: " "}).toEdge(); err == nil {
		t.Fatal("toEdge accepted an empty rel")
	}
}

func TestHasCodeMatchesOnlyArtexErrors(t *testing.T) {
	if HasCode(nil, CodeNotFound) {
		t.Fatal("HasCode(nil) matched")
	}
	if HasCode(fmt.Errorf("plain"), CodeNotFound) {
		t.Fatal("HasCode matched a non-ARTEX error")
	}
	wrapped := fmt.Errorf("outer: %w", &Error{Code: CodeNotFound})
	if !HasCode(wrapped, CodeNotFound) {
		t.Fatal("HasCode did not match a wrapped ARTEX error")
	}
}
