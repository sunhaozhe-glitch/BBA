package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
)

func TestSystemSettingsSurfaceNilRegistryUnavailableWithoutPanic(t *testing.T) {
	store := authIntegrationStore(t)
	actor := taskengine.Actor{
		WorkspaceID: "ws_auth",
		PrincipalID: "p_auth",
		Permissions: map[string]bool{
			"system.read":      true,
			"system.configure": true,
			"operator.read":    true,
			"model.request":    true,
		},
	}
	auth := func(*http.Request) (taskengine.Actor, error) { return actor, nil }
	fallbackCalls := 0
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	api := systemSettingsSurface(fallback, nil, auth, store)
	handler := productSurface(api, http.NotFoundHandler(), auth)

	for _, tc := range []struct {
		name, method, path string
		body               string
	}{
		{"proxy get", http.MethodGet, taskengine.GlobalProxyPath, ""},
		{"proxy put", http.MethodPut, taskengine.GlobalProxyPath, `{}`},
		{"search get", http.MethodGet, taskengine.WebSearchPath, ""},
		{"search put", http.MethodPut, taskengine.WebSearchPath, `{}`},
		{"search test", http.MethodPost, taskengine.WebSearchPath + "/test", `{"query":"fixture query"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.method == http.MethodPut || tc.path == taskengine.WebSearchPath+"/test" {
				req.Header.Set("Content-Type", "application/json")
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
			}
			var envelope contract.ErrorEnvelope
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v; body=%s", err, recorder.Body.String())
			}
			if envelope.Error.Code != "STATE_STORE_UNAVAILABLE" {
				t.Fatalf("error code=%q, want STATE_STORE_UNAVAILABLE; body=%s", envelope.Error.Code, recorder.Body.String())
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/operator/system/other", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || fallbackCalls != 1 {
		t.Fatalf("unrelated product path status=%d fallback calls=%d, want 204 and one call", recorder.Code, fallbackCalls)
	}
}

func TestOptionalRegistrySettingsRoutes(t *testing.T) {
	store := authIntegrationStore(t)
	authorizedActor := taskengine.Actor{
		WorkspaceID: "ws_auth",
		PrincipalID: "p_auth",
		Permissions: map[string]bool{
			"system.read":      true,
			"system.configure": true,
			"operator.read":    true,
			"model.request":    true,
		},
	}
	authorizedAuth := func(*http.Request) (taskengine.Actor, error) { return authorizedActor, nil }
	deniedAuth := func(*http.Request) (taskengine.Actor, error) { return taskengine.Actor{}, nil }
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	registry, err := taskengine.OpenModelRegistry(filepath.Join(dir, "registry.json"), "", nil, nil)
	if err != nil {
		t.Fatalf("configured registry open: %v", err)
	}
	t.Cleanup(registry.Close)
	configured := systemSettingsSurface(http.NotFoundHandler(), registry, authorizedAuth, store)
	unconfigured := systemSettingsSurface(http.NotFoundHandler(), nil, authorizedAuth, store)

	do := func(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	decodeData := func(recorder *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode failed: body=%s", recorder.Body.String())
		}
		return envelope.Data
	}

	recorder := do(configured, http.MethodGet, taskengine.GlobalProxyPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("configured proxy GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if data := decodeData(recorder); data["workspace_id"] != "ws_auth" {
		t.Fatalf("configured proxy GET data=%v", data)
	}
	recorder = do(configured, http.MethodGet, taskengine.WebSearchPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("configured search GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if data := decodeData(recorder); data["workspace_id"] != "ws_auth" {
		t.Fatalf("configured search GET data=%v", data)
	}
	recorder = do(configured, http.MethodPut, taskengine.GlobalProxyPath, `{"enabled":false,"url":"","expected_version":0,"secret_action":"keep"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("configured proxy PUT status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if data := decodeData(recorder); data["version"] != float64(1) || data["can_configure"] != true {
		t.Fatalf("configured proxy PUT data=%v", data)
	}
	recorder = do(configured, http.MethodDelete, taskengine.GlobalProxyPath, "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("configured proxy DELETE status=%d, want 405; body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = do(configured, http.MethodDelete, taskengine.WebSearchPath, "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("configured search DELETE status=%d, want 405; body=%s", recorder.Code, recorder.Body.String())
	}
	{
		handler := systemSettingsSurface(http.NotFoundHandler(), registry, deniedAuth, store)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, taskengine.GlobalProxyPath, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("configured proxy unauthenticated status=%d, want 401; body=%s", recorder.Code, recorder.Body.String())
		}
	}
	{
		handler := systemSettingsSurface(http.NotFoundHandler(), registry, func(*http.Request) (taskengine.Actor, error) {
			return taskengine.Actor{WorkspaceID: "ws_auth", PrincipalID: "p_auth", Permissions: map[string]bool{"model.request": true}}, nil
		}, store)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, taskengine.GlobalProxyPath, strings.NewReader(`{}`)))
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("configured proxy wrong-permission PUT status=%d, want 403; body=%s", recorder.Code, recorder.Body.String())
		}
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, taskengine.GlobalProxyPath, ""},
		{http.MethodPut, taskengine.WebSearchPath, "{}"},
	} {
		recorder := do(unconfigured, tc.method, tc.path, tc.body)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("unconfigured registry %s %s status=%d, want 503; body=%s", tc.method, tc.path, recorder.Code, recorder.Body.String())
		}
		var envelope contract.ErrorEnvelope
		if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode unconfigured envelope: %v; body=%s", err, recorder.Body.String())
		}
		if envelope.Error.Code != "STATE_STORE_UNAVAILABLE" {
			t.Fatalf("unconfigured registry error code=%q, want STATE_STORE_UNAVAILABLE", envelope.Error.Code)
		}
	}
}

func TestSystemSettingsSurfaceNilRegistryPreservesAuthenticationAndMethodOrder(t *testing.T) {
	store := authIntegrationStore(t)
	deniedAuth := func(*http.Request) (taskengine.Actor, error) {
		return taskengine.Actor{}, nil
	}
	authorizedActor := taskengine.Actor{
		WorkspaceID: "ws_auth",
		PrincipalID: "p_auth",
		Permissions: map[string]bool{
			"system.read":      true,
			"system.configure": true,
			"operator.read":    true,
			"model.request":    true,
		},
	}
	authorizedAuth := func(*http.Request) (taskengine.Actor, error) { return authorizedActor, nil }
	for _, tc := range []struct {
		name   string
		auth   func(*http.Request) (taskengine.Actor, error)
		method string
		path   string
		want   int
	}{
		{"proxy unauthenticated", deniedAuth, http.MethodGet, taskengine.GlobalProxyPath, http.StatusUnauthorized},
		{"search unauthenticated", deniedAuth, http.MethodGet, taskengine.WebSearchPath, http.StatusUnauthorized},
		{"proxy unsupported method remains unavailable", authorizedAuth, http.MethodDelete, taskengine.GlobalProxyPath, http.StatusServiceUnavailable},
		{"search unsupported method remains unavailable", authorizedAuth, http.MethodDelete, taskengine.WebSearchPath, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := systemSettingsSurface(http.NotFoundHandler(), nil, tc.auth, store)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code != tc.want {
				t.Errorf("%s %s status=%d, want %d; body=%s", tc.method, tc.path, recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}
