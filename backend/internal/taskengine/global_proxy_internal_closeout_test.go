package taskengine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestB5RealLoginNapCatIgnoresEnvironment(t *testing.T) {
	var traps atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { traps.Add(1); w.WriteHeader(502) }))
	defer trap.Close()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(key, trap.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	stub := &napcatStub{t: t, webuiToken: "fixture-webui", onebotToken: "fixture-onebot", credential: "fixture-session", statusResponse: map[string]any{"isLogin": true, "isOffline": false, "loginPhase": "ready"}, identity: map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{"user_id": 123456, "nickname": "fixture"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credential on internal request")
		}
		stub.ServeHTTP(w, r)
	}))
	defer server.Close()
	provider, e := LoadLoginProvider(writeLoginProviderConfig(t, server.URL, stub.webuiToken, stub.onebotToken), false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = provider.Start(context.Background(), napcatTestAccount()); e != nil {
		t.Fatal(e)
	}
	if _, e = provider.Status(context.Background(), napcatTestAccount()); e != nil {
		t.Fatal(e)
	}
	identity, e := provider.Identity(context.Background(), napcatTestAccount())
	if e != nil || identity.SelfID != "123456" {
		t.Fatal("identity fixture failed", e)
	}
	if traps.Load() != 0 || atomic.LoadInt32(&stub.authCalls) != 1 || atomic.LoadInt32(&stub.qrCalls) != 1 || atomic.LoadInt32(&stub.statusCalls) != 1 {
		t.Fatal("internal provider used proxy or failed consumption")
	}
	t.Log("real LoadLoginProvider auth/QR/status/OneBot identity consumed ephemeral fixture; malicious environment trap=0; no real 6300")
}
