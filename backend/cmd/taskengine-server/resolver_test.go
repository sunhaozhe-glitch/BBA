package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeResolverFixture(t *testing.T, mode os.FileMode, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolver.json")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolverMapsBearerTokenToTrustedActor(t *testing.T) {
	token := "fixture-secret-never-stored"
	digest := sha256.Sum256([]byte(token))
	config, err := json.Marshal(resolverFile{Credentials: []credentialRecord{{
		TokenSHA256: hex.EncodeToString(digest[:]), WorkspaceID: "ws_a", PrincipalID: "operator_a",
		Permissions: []string{"operator.read", "task.read", "task.control"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := loadResolver(writeResolverFixture(t, 0600, string(config)))
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:18421/v1/accounts", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	actor, err := resolver(request)
	if err != nil {
		t.Fatal(err)
	}
	if actor.WorkspaceID != "ws_a" || actor.PrincipalID != "operator_a" || !actor.Permissions["operator.read"] || !actor.Permissions["task.read"] || !actor.Permissions["task.control"] || actor.Permissions["task.manage_all"] {
		t.Fatalf("resolver returned unexpected actor: %+v", actor)
	}
	request.Header.Set("X-Principal-ID", "attacker")
	actor, err = resolver(request)
	if err != nil || actor.PrincipalID != "operator_a" {
		t.Fatalf("client principal header affected resolver: actor=%+v err=%v", actor, err)
	}
}

func TestResolverRejectsMissingTokenAndWeakOrMalformedConfig(t *testing.T) {
	token := "fixture-token"
	digest := sha256.Sum256([]byte(token))
	valid, _ := json.Marshal(resolverFile{Credentials: []credentialRecord{{TokenSHA256: hex.EncodeToString(digest[:]), WorkspaceID: "ws_a", PrincipalID: "operator_a", Permissions: []string{"task.read"}}}})
	for _, tc := range []struct {
		name string
		mode os.FileMode
		body string
	}{
		{name: "group-readable", mode: 0640, body: string(valid)},
		{name: "unknown-field", mode: 0600, body: `{"credentials":[],"token":"plaintext"}`},
		{name: "empty", mode: 0600, body: `{"credentials":[]}`},
		{name: "duplicate-permission", mode: 0600, body: `{"credentials":[{"token_sha256":"` + hex.EncodeToString(digest[:]) + `","workspace_id":"ws_a","principal_id":"p","permissions":["task.read","task.read"]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadResolver(writeResolverFixture(t, tc.mode, tc.body)); err == nil {
				t.Fatal("invalid resolver config accepted")
			}
		})
	}
	resolver, err := loadResolver(writeResolverFixture(t, 0600, string(valid)))
	if err != nil {
		t.Fatal(err)
	}
	for _, authorization := range []string{"", "Bearer", "Bearer ", "Bearer unknown", "Basic fixture-token"} {
		request, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:18421/v1/accounts", nil)
		request.Header.Set("Authorization", authorization)
		if _, err := resolver(request); err == nil {
			t.Fatalf("accepted authorization %q", authorization)
		}
	}
}

func TestDeploymentOriginRequiresExactLoopbackAndFixedHost(t *testing.T) {
	for _, tc := range []struct {
		origin, listen string
		wantErr        bool
	}{
		{"http://127.0.0.1:18421", "127.0.0.1:18421", false},
		{"https://127.0.0.1:18421", "127.0.0.1:18421", true},
		{"http://0.0.0.0:18421", "0.0.0.0:18421", true},
		{"http://127.0.0.1:18422", "127.0.0.1:18421", true},
		{"http://127.0.0.1:18421/path", "127.0.0.1:18421", true},
	} {
		_, err := validateDeploymentOrigin(tc.origin, tc.listen)
		if (err != nil) != tc.wantErr {
			t.Fatalf("origin=%s listen=%s err=%v wantErr=%v", tc.origin, tc.listen, err, tc.wantErr)
		}
	}
	called := false
	handler := fixedHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}), "127.0.0.1:18421")
	request, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:18421/v1/accounts", nil)
	request.Host = "evil.example"
	recorder := &responseRecorder{}
	handler.ServeHTTP(recorder, request)
	if recorder.status != http.StatusMisdirectedRequest || called {
		t.Fatalf("wrong Host status=%d called=%v", recorder.status, called)
	}
}

func TestTaskEngineSurfaceAllowsOnlyReadAndReconcileByDefault(t *testing.T) {
	calls := 0
	handler := taskEngineSurface(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/v1/accounts", http.StatusNoContent},
		{http.MethodGet, "/v1/tasks", http.StatusNoContent},
		{http.MethodGet, "/v1/tasks/task_1", http.StatusNoContent},
		{http.MethodPost, "/v1/tasks/task_1/reconcile", http.StatusNoContent},
		{http.MethodPost, "/v1/tasks", http.StatusNotFound},
		{http.MethodPost, "/v1/tasks/task_1/controls", http.StatusNotFound},
		{http.MethodPost, "/v1/tasks/task_1/plan-proposals", http.StatusNotFound},
		{http.MethodPost, "/v1/internal/chat-send-permits", http.StatusNotFound},
	} {
		recorder := &responseRecorder{}
		request := httptest.NewRequest(tc.method, tc.path, nil)
		handler.ServeHTTP(recorder, request)
		if recorder.status != tc.want {
			t.Fatalf("%s %s status=%d want %d", tc.method, tc.path, recorder.status, tc.want)
		}
	}
	if calls != 4 {
		t.Fatalf("underlying handler called %d times, want 4", calls)
	}
}

func TestServerAPISurfaceRoutesOperatorConversations(t *testing.T) {
	var gotMethod, gotPath string
	marker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	})
	h := serverAPISurface(serverRoutes{OperatorConversations: marker, WorkSettings: other, Runtime: other, Operator: other})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/operator/conversations"},
		{http.MethodGet, "/v1/operator/conversations/c1/messages"},
		{http.MethodPost, "/v1/operator/agent-threads/t1/messages"},
		{http.MethodPost, "/v1/operator/qq-drafts"},
		{http.MethodPost, "/v1/operator/qq-inbox/events"},
		{http.MethodGet, "/v1/operator/notifications"},
		{http.MethodPost, "/v1/operator/notifications/n1"},
	} {
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != http.StatusNoContent || gotMethod != tc.method || gotPath != tc.path {
			t.Fatalf("%s %s -> status=%d forwarded=%s %s", tc.method, tc.path, recorder.Code, gotMethod, gotPath)
		}
	}
	for _, tc := range []struct {
		method, path, wantPath string
		wantStatus             int
	}{
		{http.MethodGet, "/v1/operator/system/work-slots", "/v1/operator/system/work-slots", http.StatusAccepted},
		{http.MethodGet, "/v1/operator/runtime", "/v1/operator/runtime", http.StatusAccepted},
	} {
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != tc.wantStatus || gotPath != tc.wantPath {
			t.Fatalf("unrelated %s %s forwarded to %s: status=%d", tc.method, tc.path, gotPath, recorder.Code)
		}
	}
	noHandler := httptest.NewRecorder()
	serverAPISurface(serverRoutes{}).ServeHTTP(noHandler, httptest.NewRequest(http.MethodGet, "/v1/operator/conversations", nil))
	if noHandler.Code != http.StatusNotFound {
		t.Fatalf("nil conversation handler status=%d, want 404", noHandler.Code)
	}
}

func TestTaskEngineSurfaceEnablesBoundedDevelopmentOperationsExplicitly(t *testing.T) {
	calls := 0
	handler := taskEngineSurface(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}), true)
	for _, path := range []string{"/v1/tasks", "/v1/tasks/task_1/controls", "/v1/tasks/task_1/plan-proposals", "/v1/tasks/task_1/plan-proposals/proposal_1/approve", "/v1/tasks/task_1/plan-proposals/proposal_1/apply"} {
		recorder := &responseRecorder{}
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		if recorder.status != http.StatusNoContent {
			t.Fatalf("POST %s status=%d", path, recorder.status)
		}
	}
	if calls != 5 {
		t.Fatalf("underlying handler called %d times, want 5", calls)
	}
}

func TestConsoleSurfaceEmbedsReadOnlyAssetsAndKeepsAPIAllowlist(t *testing.T) {
	called := 0
	api := taskEngineSurface(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	handler := consoleSurface(api)

	for _, tc := range []struct {
		method, path string
		want         int
		contentType  string
	}{
		{http.MethodGet, "/", http.StatusOK, "text/html; charset=utf-8"},
		{http.MethodGet, "/app.css", http.StatusOK, "text/css; charset=utf-8"},
		{http.MethodGet, "/app.js", http.StatusOK, "text/javascript; charset=utf-8"},
		{http.MethodPost, "/", http.StatusNotFound, "text/plain; charset=utf-8"},
		{http.MethodGet, "/v1/accounts", http.StatusNoContent, ""},
		{http.MethodPost, "/v1/tasks/task_1/controls", http.StatusNotFound, "text/plain; charset=utf-8"},
	} {
		recorder := &responseRecorder{}
		request, _ := http.NewRequest(tc.method, tc.path, nil)
		handler.ServeHTTP(recorder, request)
		if recorder.status != tc.want {
			t.Fatalf("%s %s status=%d want %d", tc.method, tc.path, recorder.status, tc.want)
		}
		if got := recorder.Header().Get("Content-Type"); got != tc.contentType {
			t.Fatalf("%s %s Content-Type=%q want %q", tc.method, tc.path, got, tc.contentType)
		}
		if tc.method == http.MethodGet && tc.path == "/" && recorder.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("console document did not set a content security policy")
		}
	}
	if called != 1 {
		t.Fatalf("API handler called %d times, want only the allowed account GET", called)
	}
}

type responseRecorder struct {
	status int
	header http.Header
}

func (r *responseRecorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}
func (r *responseRecorder) WriteHeader(status int) { r.status = status }
func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return len(data), nil
}
