package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestB5ProxySettingsPermissionsCASAudit(t *testing.T) {
	s := fixtureStore(t)
	a := fixtureActor()
	a.Permissions["system.read"] = true
	a.Permissions["system.configure"] = true
	r := b5Registry(t, nil)
	selected := a
	h := r.ProxySettingsHandler(func(*http.Request) (Actor, error) { return selected, nil }, s)
	call := func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, GlobalProxyPath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		return rr
	}
	good := `{"enabled":true,"url":"http://127.0.0.1:7890","expected_version":0,"secret_action":"replace","username":"fixture-user","password":"fixture-password"}`
	reader := fixtureActor()
	reader.Permissions["system.read"] = true
	selected = reader
	if rr := call("PUT", good); rr.Code != 403 {
		t.Fatal(rr.Code)
	}
	selected = a
	if rr := call("PUT", good); rr.Code != 200 || strings.Contains(rr.Body.String(), "fixture-password") || strings.Contains(rr.Body.String(), "fixture-user") {
		t.Fatal("save/masking failed", rr.Code)
	}
	if rr := call("GET", ""); rr.Code != 200 || strings.Contains(rr.Body.String(), "password") || strings.Contains(rr.Body.String(), "username") {
		t.Fatal("read leaks secrets")
	}
	if rr := call("PUT", good); rr.Code != 409 {
		t.Fatal("CAS", rr.Code)
	}
	for _, bad := range []string{`{}`, `{"enabled":null}`, `{"enabled":true,"enabled":false}`, strings.Replace(good, `"expected_version":0`, `"expected_version":1,"workspace_id":"other"`, 1), strings.Replace(good, `"expected_version":0`, `"expected_version":1.0`, 1), strings.Replace(good, "http://127.0.0.1:7890", "https://127.0.0.1:7890", 1), strings.Replace(good, "http://127.0.0.1:7890", "http://user:pass@127.0.0.1:7890", 1)} {
		if rr := call("PUT", bad); rr.Code != 400 && rr.Code != 409 {
			t.Fatalf("bad accepted %d", rr.Code)
		}
	}
	// Two actual concurrent saves at the same version: exactly one commit/audit.
	enabled := false
	u := "http://127.0.0.1:7890"
	v := int64(1)
	var wg sync.WaitGroup
	codes := make(chan string, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := r.saveProxy(a.WorkspaceID, a.PrincipalID, proxyWrite{Enabled: &enabled, URL: &u, Expected: &v, Action: "clear"})
			if e == nil {
				codes <- "ok"
			} else {
				codes <- Code(e)
			}
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[string]int{}
	for c := range codes {
		counts[c]++
	}
	if counts["ok"] != 1 || counts[taskCodeVersionConflict] != 1 {
		t.Fatal(counts)
	}
	b, e := os.ReadFile(r.path)
	if e != nil {
		t.Fatal(e)
	}
	var disk modelRegistryDisk
	if json.Unmarshal(b, &disk) != nil {
		t.Fatal("disk")
	}
	if len(disk.ProxyAudit) != 2 || disk.Proxies[a.WorkspaceID].Password != "" {
		t.Fatal("atomic audit/clear")
	}
	audit, _ := json.Marshal(disk.ProxyAudit)
	if bytes.Contains(audit, []byte("fixture-user")) || bytes.Contains(audit, []byte("fixture-password")) || bytes.Contains(audit, []byte("Proxy-Authorization")) {
		t.Fatal("audit leak")
	}
	info, e := os.Stat(r.path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("file mode")
	}
	// A failed rename must not mutate the in-memory snapshot or append audit.
	if e := os.Rename(r.path, r.path+".saved"); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(r.path, 0700); e != nil {
		t.Fatal(e)
	}
	if _, e := r.saveProxy(a.WorkspaceID, a.PrincipalID, proxyWrite{Enabled: &enabled, URL: &u, Expected: &v, Action: "keep"}); Code(e) != taskCodeStoreUnavailable {
		t.Fatal("store error hidden")
	}
	if len(r.data.ProxyAudit) != 2 {
		t.Fatal("failed persistence appended audit")
	}
	t.Log("readonly403 actualCAS409 concurrentCAS=1 atomic_audit=2 clear_masked=true protected_file=true store_failure_closed=true")
}
func TestB5NetworkBypassDNSAndCancel(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.2", "169.254.169.254", "0.0.0.0", "224.0.0.1", "100.64.0.1", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1"} {
		if !localModelIP(net.ParseIP(ip)) {
			t.Fatalf("local address not bypassed %s", ip)
		}
	}
	if localModelIP(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("public IPv6 bypassed")
	}
	for _, answers := range [][]string{{"127.0.0.1"}, {"8.8.8.8", "10.1.1.1"}, {"::1"}} {
		var addresses []string
		n := modelNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
			ips := []net.IPAddr{}
			for _, s := range answers {
				ips = append(ips, net.IPAddr{IP: net.ParseIP(s)})
			}
			return ips, nil
		}, dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			addresses = append(addresses, addr)
			return nil, errors.New("fixture dial refused")
		}}
		rt, e := proxyTransportNetwork(proxyStored{Enabled: true, URL: "http://127.0.0.1:7890"}, nil, n)
		if e != nil {
			t.Fatal(e)
		}
		req, _ := http.NewRequest("GET", "https://model.bba.example/v1", nil)
		rt.RoundTrip(req)
		if len(addresses) != 1 || addresses[0] == "127.0.0.1:7890" {
			t.Fatal("local DNS target sent to proxy", addresses)
		}
	}
	// IPv6 public target is sent numerically; internal control ports bypass even
	// when an isolated resolver presents a public address.
	for _, port := range []string{"6300", "5432", "6099", "6199", "6210", "18421"} {
		var addr string
		n := modelNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}, dial: func(_ context.Context, _, s string) (net.Conn, error) { addr = s; return nil, errProxyDial }}
		rt, _ := proxyTransportNetwork(proxyStored{Enabled: true, URL: "http://127.0.0.1:7890"}, nil, n)
		req, _ := http.NewRequest("GET", "https://model.bba.example:"+port+"/v1", nil)
		rt.RoundTrip(req)
		if addr != "8.8.8.8:"+port {
			t.Fatal("internal port proxied", addr)
		}
	}
	n := defaultModelNetwork()
	n.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	peerDone := make(chan struct{})
	n.dial = func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() { defer close(peerDone); defer b.Close(); io.Copy(io.Discard, b) }()
		return a, nil
	}
	rt, _ := proxyTransportNetwork(proxyStored{Enabled: true, URL: "http://127.0.0.1:7890"}, nil, n)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://model.bba.example/v1", nil)
	started := time.Now()
	if _, e := rt.RoundTrip(req); e == nil || time.Since(started) > time.Second {
		t.Fatal("proxy handshake did not cancel")
	}
	select {
	case <-peerDone:
	case <-time.After(time.Second):
		t.Fatal("cancel leaked handshake")
	}
	t.Log("mixed_DNS_local_bypass=true IPv6_local=true control_ports_direct=true handshake_cancel_closed=true")
}
