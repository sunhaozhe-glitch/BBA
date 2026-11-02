package identity

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestB5RealOneBotReaderIgnoresEnvironment(t *testing.T) {
	var traps, hits atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { traps.Add(1); w.WriteHeader(502) }))
	defer trap.Close()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(key, trap.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/get_login_info" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("OneBot request boundary")
		}
		io.WriteString(w, `{"status":"ok","retcode":0,"data":{"user_id":123456,"nickname":"fixture"}}`)
	}))
	defer server.Close()
	reader, e := NewHTTPLoginInfoReader("fixture-runtime", server.URL, "fixture-token")
	if e != nil {
		t.Fatal(e)
	}
	_, e = reader.ReadLoginInfo(context.Background(), Binding{RuntimeID: "fixture-runtime"})
	if e != nil {
		t.Fatal(e)
	}
	if hits.Load() != 1 || traps.Load() != 0 {
		t.Fatal("OneBot reader did not bypass environment")
	}
	t.Log("real OneBot reader consumed isolated fixture; environment proxy=0; no real 6300")
}
