package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"bba.local/taskengine/internal/taskengine"
)

func TestGatewayAuthBindsTokenToFixedRuntime(t *testing.T) {
	const token = "fixture-task-bridge-token"
	digest := sha256.Sum256([]byte(token))
	server, auth, err := newGateway(gatewayConfig{Runtimes: []gatewayRuntimeConfig{{
		TokenSHA256: hex.EncodeToString(digest[:]),
		WorkspaceID: "ws_fixture",
		RuntimeID:   "rt_fixture",
		AccountID:   "acc_fixture",
		OwnerEpoch:  7,
		OneBotURL:   "http://127.0.0.1:19001",
		OneBotToken: "fixture-onebot-token",
		WebUIURL:    "http://127.0.0.1:19002",
		WebUIToken:  "fixture-webui-token",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(server.runtimes) != 1 {
		t.Fatalf("runtime count=%d, want 1", len(server.runtimes))
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18422/v1/internal/bridge/identity", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	actor, err := auth.authenticate(request)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if actor.WorkspaceID != "ws_fixture" || actor.RuntimeID != "rt_fixture" || actor.OwnerEpoch != 7 || len(actor.AccountIDs) != 1 || actor.AccountIDs[0] != "acc_fixture" {
		t.Fatalf("actor=%+v", actor)
	}

	request.Header.Set("Authorization", "Bearer wrong-token")
	if _, err := auth.authenticate(request); taskengine.Code(err) != "UNAUTHENTICATED" {
		t.Fatalf("wrong token error=%v code=%q", err, taskengine.Code(err))
	}
	request.Header.Set("Authorization", "Bearer "+token+" ")
	if _, err := auth.authenticate(request); taskengine.Code(err) != "UNAUTHENTICATED" {
		t.Fatalf("whitespace token error=%v code=%q", err, taskengine.Code(err))
	}
}

func TestBridgeFixedHostRejectsMisdirectedRequests(t *testing.T) {
	called := false
	handler := bridgeFixedHost(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), "http://127.0.0.1:18422")

	wrongHost := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18422/v1/internal/bridge/identity", nil)
	wrongHost.Host = "127.0.0.1:18423"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, wrongHost)
	if response.Code != http.StatusMisdirectedRequest || called {
		t.Fatalf("wrong host status=%d called=%v", response.Code, called)
	}

	absolute := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18422/v1/internal/bridge/identity", nil)
	absolute.URL.Scheme = "http"
	absolute.URL.Host = "127.0.0.1:18422"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, absolute)
	if response.Code != http.StatusMisdirectedRequest || called {
		t.Fatalf("absolute URL status=%d called=%v", response.Code, called)
	}

	valid := httptest.NewRequest(http.MethodGet, "/v1/internal/bridge/identity", nil)
	valid.Host = "127.0.0.1:18422"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, valid)
	if response.Code != http.StatusOK || !called || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("valid request status=%d called=%v cache-control=%q", response.Code, called, response.Header().Get("Cache-Control"))
	}
}
