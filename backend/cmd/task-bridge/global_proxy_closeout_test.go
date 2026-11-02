package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestB5RealBridgeClientIgnoresEnvironment(t *testing.T) {
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
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("internal proxy header leak")
		}
		io.WriteString(w, "fixture bridge response")
	}))
	defer server.Close()
	client := newGatewayHTTPClient()
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("gateway has environment proxy")
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", server.URL+"/get_login_info", nil)
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if hits.Load() != 1 || traps.Load() != 0 {
		t.Fatal("bridge internal fixture not direct")
	}
	t.Log("real task-bridge default client consumed isolated loopback fixture; environment trap=0; no real 6300")
}
