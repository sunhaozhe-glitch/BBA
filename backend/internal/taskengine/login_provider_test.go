package taskengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type napcatStub struct {
	t              *testing.T
	webuiToken     string
	onebotToken    string
	credential     string
	authCalls      int32
	qrCalls        int32
	refreshCalls   int32
	statusCalls    int32
	rejectFirstQR  bool
	statusResponse map[string]any
	identity       map[string]any
}

func (stub *napcatStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	switch {
	case r.URL.Path == "/api/auth/login":
		expected := sha256.Sum256([]byte(stub.webuiToken + ".napcat"))
		var payload struct {
			Hash string `json:"hash"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.Hash != hex.EncodeToString(expected[:]) {
			http.Error(w, `{"code":-1,"message":"bad hash"}`, http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&stub.authCalls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "", "data": map[string]string{"Credential": stub.credential}})
	case strings.HasPrefix(r.URL.Path, "/api/QQLogin/"):
		if r.Header.Get("Authorization") != "Bearer "+stub.credential {
			http.Error(w, `{"code":-1,"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/QQLogin/GetQQLoginQrcode":
			if stub.rejectFirstQR && atomic.AddInt32(&stub.qrCalls, 1) == 1 {
				http.Error(w, `{"code":-1,"message":"Unauthorized"}`, http.StatusUnauthorized)
				return
			}
			atomic.AddInt32(&stub.qrCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"qrcode": "data:image/png;base64,AAAA"}})
		case "/api/QQLogin/RefreshQRcode":
			atomic.AddInt32(&stub.refreshCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{}})
		case "/api/QQLogin/CheckLoginStatus":
			atomic.AddInt32(&stub.statusCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": stub.statusResponse})
		default:
			http.NotFound(w, r)
		}
	case r.URL.Path == "/get_login_info":
		if r.Header.Get("Authorization") != "Bearer "+stub.onebotToken {
			http.Error(w, `{"status":"failed","retcode":1403}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(stub.identity)
	default:
		http.NotFound(w, r)
	}
}

func writeLoginProviderConfig(t *testing.T, serverURL, webuiToken, onebotToken string) string {
	t.Helper()
	dir := t.TempDir()
	webuiPath := filepath.Join(dir, "webui.token")
	onebotPath := filepath.Join(dir, "onebot.token")
	for path, token := range map[string]string{webuiPath: webuiToken, onebotPath: onebotToken} {
		if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := map[string]any{
		"mode":                "napcat",
		"session_ttl_seconds": 60,
		"qr_ttl_seconds":      30,
		"instances": []map[string]string{{
			"adapter_instance_id": "adapter_napcat_test",
			"webui_url":           serverURL + "/api",
			"webui_token_file":    webuiPath,
			"onebot_url":          serverURL,
			"onebot_token_file":   onebotPath,
		}},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "provider.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func napcatTestAccount() LoginAccount {
	return LoginAccount{WorkspaceID: "ws_fixture", AccountID: "acc_napcat", AdapterInstanceID: "adapter_napcat_test", RuntimeID: "rt_napcat"}
}

func TestNapCatProviderUsesVerifiedProtocolAndReauth(t *testing.T) {
	stub := &napcatStub{
		t:           t,
		webuiToken:  "webui-secret",
		onebotToken: "onebot-secret",
		credential:  "credential-1",
		statusResponse: map[string]any{
			"isLogin": true, "isOffline": false, "loginPhase": "ready",
		},
		identity: map[string]any{
			"status": "ok", "retcode": 0,
			"data": map[string]any{"user_id": 123456, "nickname": "QQ Nick"},
		},
	}
	server := httptest.NewServer(stub)
	defer server.Close()

	provider, err := LoadLoginProvider(writeLoginProviderConfig(t, server.URL, stub.webuiToken, stub.onebotToken), false)
	if err != nil {
		t.Fatalf("load provider: %v", err)
	}
	ctx := context.Background()
	account := napcatTestAccount()
	qr, err := provider.Start(ctx, account)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if qr.ImageDataURL != "data:image/png;base64,AAAA" || qr.ExpiresAt.IsZero() {
		t.Fatalf("qr=%+v", qr)
	}
	if _, err := provider.Refresh(ctx, account); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if atomic.LoadInt32(&stub.refreshCalls) != 1 || atomic.LoadInt32(&stub.authCalls) != 1 {
		t.Fatalf("refresh calls=%d auth=%d", stub.refreshCalls, stub.authCalls)
	}
	status, err := provider.Status(ctx, account)
	if err != nil || status.Phase != LoginPhaseOnline {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	identity, err := provider.Identity(ctx, account)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if identity.SelfID != "123456" || identity.Nickname != "QQ Nick" {
		t.Fatalf("identity=%+v", identity)
	}

	// An Unauthorized answer re-authenticates exactly once and then succeeds.
	stub.rejectFirstQR = true
	stub.credential = "credential-2"
	atomic.StoreInt32(&stub.qrCalls, 0)
	atomic.StoreInt32(&stub.authCalls, 0)
	provider2, err := LoadLoginProvider(writeLoginProviderConfig(t, server.URL, stub.webuiToken, stub.onebotToken), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider2.Start(ctx, account); err != nil {
		t.Fatalf("reauth start: %v", err)
	}
	if calls := atomic.LoadInt32(&stub.authCalls); calls != 2 {
		t.Fatalf("reauth calls=%d want 2", calls)
	}
}

func TestNapCatProviderMapsPhasesAndRejectsUnconfiguredInstance(t *testing.T) {
	statusResponse := map[string]any{"isLogin": false, "isOffline": false, "loginPhase": "qrcode_scanned"}
	stub := &napcatStub{t: t, webuiToken: "w", onebotToken: "o", credential: "c", statusResponse: statusResponse,
		identity: map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{"user_id": 1, "nickname": "n"}}}
	server := httptest.NewServer(stub)
	defer server.Close()
	provider, err := LoadLoginProvider(writeLoginProviderConfig(t, server.URL, "w", "o"), false)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := provider.Status(context.Background(), napcatTestAccount()); err != nil || status.Phase != LoginPhaseScanned {
		t.Fatalf("scanned status=%+v err=%v", status, err)
	}
	missing := napcatTestAccount()
	missing.AdapterInstanceID = "adapter_missing"
	if _, err := provider.Start(context.Background(), missing); err == nil {
		t.Fatal("unconfigured adapter instance must fail")
	}

	// loginError text is classified into a bounded phase without echoing it.
	stub.statusResponse = map[string]any{"isLogin": false, "loginError": "二维码已过期"}
	if status, err := provider.Status(context.Background(), napcatTestAccount()); err != nil || status.Phase != LoginPhaseExpired {
		t.Fatalf("expired status=%+v err=%v", status, err)
	}
	stub.statusResponse = map[string]any{"isLogin": false, "loginError": "other failure"}
	if status, err := provider.Status(context.Background(), napcatTestAccount()); err != nil || status.Phase != LoginPhaseFailed || status.Reason != loginFailureProvider {
		t.Fatalf("failed status=%+v err=%v", status, err)
	}
}

func TestNapCatProviderRejectsMalformedResponses(t *testing.T) {
	stub := &napcatStub{t: t, webuiToken: "w", onebotToken: "o", credential: "c", statusResponse: map[string]any{}}
	server := httptest.NewServer(stub)
	defer server.Close()
	provider, err := LoadLoginProvider(writeLoginProviderConfig(t, server.URL, "w", "o"), false)
	if err != nil {
		t.Fatal(err)
	}
	// The identity endpoint returns a non-QQ id shape here.
	stub.identity = map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{"user_id": "not-a-qq-id", "nickname": "n"}}
	if _, err := provider.Identity(context.Background(), napcatTestAccount()); err == nil {
		t.Fatal("invalid identity must fail")
	}
	stub.identity = map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{"user_id": 55, "nickname": ""}}
	if _, err := provider.Identity(context.Background(), napcatTestAccount()); err == nil {
		t.Fatal("empty nickname must fail")
	}
}
