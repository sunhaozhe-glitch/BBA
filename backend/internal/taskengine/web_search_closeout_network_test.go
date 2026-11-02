package taskengine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Fresh x509 root cache, exactly like b6ProviderOffline. All endpoints remain
// production constants; only the existing same-package numeric dial seam maps
// them to owned loopback fixtures. No DB, DefaultTransport edits or TLS bypass.
func TestB6CloseoutNetworkOffline(t *testing.T) {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	cert := &x509.Certificate{SerialNumber: big.NewInt(6006), Subject: pkix.Name{CommonName: "B6 closeout offline"}, DNSNames: []string{"api.search.brave.com", "api.tavily.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	kb, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	ca, kp := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "key.pem")
	for p, b := range map[string][]byte{ca: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), kp: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})} {
		if e := os.WriteFile(p, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestB6CloseoutNetworkFixtures$", "-test.v", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "BBA_B6_FIXTURE_CA="+ca, "BBA_B6_FIXTURE_KEY="+kp, "SSL_CERT_FILE="+ca, "SSL_CERT_DIR="+dir)
	out, e := cmd.CombinedOutput()
	t.Log(string(out))
	if e != nil {
		t.Fatal(e)
	}
}

type b6CloseoutProxy struct {
	url           string
	connect, auth atomic.Int32
	reject        atomic.Bool
	entered       chan struct{}
	release       <-chan struct{}
}

// Authenticated CONNECT to one fixed local TLS fixture, never an arbitrary
// requested host. Rejection can itself be held in flight for three-way overlap.
func b6CloseoutTunnel(t *testing.T, target, user string) *b6CloseoutProxy {
	t.Helper()
	p := &b6CloseoutProxy{entered: make(chan struct{}, 8)}
	var tunnels sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.connect.Add(1)
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":fixture-password"))
		if r.Method != "CONNECT" || r.Host != "8.8.8.8:443" || r.Header.Get("Proxy-Authorization") != want {
			t.Error("wrong CONNECT destination or workspace credentials")
			w.WriteHeader(407)
			return
		}
		p.auth.Add(1)
		if p.reject.Load() {
			p.entered <- struct{}{}
			if p.release != nil {
				select {
				case <-p.release:
				case <-r.Context().Done():
					return
				}
			}
			w.WriteHeader(407)
			return
		}
		upstream, e := net.DialTimeout("tcp", target, time.Second)
		if e != nil {
			t.Error(e)
			w.WriteHeader(502)
			return
		}
		c, rw, e := w.(http.Hijacker).Hijack()
		if e != nil {
			upstream.Close()
			t.Error(e)
			return
		}
		tunnels.Add(1)
		defer tunnels.Done()
		defer c.Close()
		defer upstream.Close()
		c.SetDeadline(time.Now().Add(15 * time.Second))
		upstream.SetDeadline(time.Now().Add(15 * time.Second))
		if _, e = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); e != nil {
			return
		}
		if rw.Flush() != nil {
			return
		}
		done := make(chan struct{})
		go func() { io.Copy(upstream, rw); upstream.Close(); close(done) }()
		io.Copy(c, upstream)
		c.Close()
		<-done
	}))
	p.url = srv.URL
	t.Cleanup(func() { srv.Close(); tunnels.Wait() })
	return p
}

func b6CloseoutNetwork(t *testing.T, proxies ...*b6CloseoutProxy) (*modelNetwork, *atomic.Int32) {
	t.Helper()
	direct := &atomic.Int32{}
	allowed := map[string]bool{}
	for _, p := range proxies {
		allowed[strings.TrimPrefix(p.url, "http://")] = true
	}
	n := &modelNetwork{lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "api.search.brave.com" && host != "api.tavily.com" && host != "model.bba.example" {
			t.Errorf("unexpected DNS %s", host)
			return nil, errors.New("blocked")
		}
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}, dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if !allowed[address] {
			direct.Add(1)
			return nil, errors.New("unconfigured/direct dial blocked by offline fixture")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	return n, direct
}

func b6CloseoutSave(t *testing.T, r *ModelRegistry, ws string, v int64, enabled bool, key string, p *b6CloseoutProxy, user string) {
	t.Helper()
	w := b6SearchWrite(v, enabled, "replace", key)
	yes := true
	w.Proxy = searchProxyWrite{Enabled: &yes, URL: &p.url, Action: "replace", Username: user, Password: "fixture-password"}
	got, e := r.saveSearch(ws, "fixture-operator", w)
	if e != nil || got.Version != v+1 {
		t.Fatal(got, e)
	}
	read, e := r.GetSearchSettings(ws, true)
	if e != nil || read.Version != v+1 || read.Enabled != enabled || read.Proxy.URL != p.url {
		t.Fatal("saved search readback", read, e)
	}
}

func b6CloseoutAdmit(r *ModelRegistry, ws string) (searchStored, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startSearchLocked(ws, true)
}

type b6CloseoutResult struct {
	receipt SearchReceipt
	err     error
}

func b6CloseoutStart(t *testing.T, r *ModelRegistry, ws, id, q string) <-chan b6CloseoutResult {
	t.Helper()
	p, e := b6CloseoutAdmit(r, ws)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan b6CloseoutResult, 1)
	go func() { out, e := r.consumeSearch(t.Context(), ws, id, q, p); done <- b6CloseoutResult{out, e} }()
	return done
}
func b6CloseoutWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture did not receive an in-flight request")
	}
}
func b6CloseoutDone(t *testing.T, ch <-chan b6CloseoutResult) SearchReceipt {
	t.Helper()
	select {
	case v := <-ch:
		if v.err != nil {
			t.Fatal(v.err)
		}
		return v.receipt
	case <-time.After(5 * time.Second):
		t.Fatal("search completion timeout")
		return SearchReceipt{}
	}
}
func b6CloseoutDisk(t *testing.T, r *ModelRegistry) modelRegistryDisk {
	t.Helper()
	b, e := os.ReadFile(r.path)
	if e != nil {
		t.Fatal(e)
	}
	var d modelRegistryDisk
	if e = json.Unmarshal(b, &d); e != nil {
		t.Fatal(e)
	}
	return d
}
func b6CloseoutReceipt(t *testing.T, got SearchReceipt, ws, id, q string, v int64, status string) {
	t.Helper()
	qd, e := digest(q)
	if e != nil {
		t.Fatal(e)
	}
	if got.Workspace != ws || got.RequestID != id || got.ConfigVersion != v || got.Source != "brave-free" || got.QueryDigest != qd || got.Status != status || got.At.IsZero() || got.Results == nil {
		t.Fatalf("receipt mismatch %+v", got)
	}
	if status == "success" && (len(got.Results) != 1 || got.Results[0].Title != q) {
		t.Fatalf("result crossed request boundary: %+v", got)
	}
}
func b6CloseoutReply(w http.ResponseWriter, q string) {
	json.NewEncoder(w).Encode(map[string]any{"web": map[string]any{"results": []map[string]string{{"url": "https://example.com/fixture", "title": q, "description": "local fixture"}}}})
}

func TestB6CloseoutNetworkFixtures(t *testing.T) {
	if os.Getenv("BBA_B6_FIXTURE_CA") == "" {
		t.Skip("run through TestB6CloseoutNetworkOffline")
	}
	var envHits atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { envHits.Add(1); w.WriteHeader(502) }))
	defer trap.Close()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(k, trap.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	for _, mode := range []string{"disable", "switch"} {
		t.Run("inflight_"+mode, func(t *testing.T) {
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var oldHits, newHits atomic.Int32
			old, _, _ := b6TLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				oldHits.Add(1)
				if r.TLS == nil || r.Header.Get("X-Subscription-Token") != "old-key" || r.Header.Get("Proxy-Authorization") != "" {
					t.Error("old TLS credential boundary")
				}
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				b6CloseoutReply(w, r.URL.Query().Get("q"))
			}))
			next, _, _ := b6TLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				newHits.Add(1)
				if r.TLS == nil || r.Header.Get("X-Subscription-Token") != "new-key" || r.Header.Get("Proxy-Authorization") != "" {
					t.Error("new TLS credential boundary")
				}
				b6CloseoutReply(w, r.URL.Query().Get("q"))
			}))
			pa, pb := b6CloseoutTunnel(t, old.Listener.Addr().String(), "old-user"), b6CloseoutTunnel(t, next.Listener.Addr().String(), "new-user")
			r := b5Registry(t, nil)
			n, direct := b6CloseoutNetwork(t, pa, pb)
			r.network = n
			b6CloseoutSave(t, r, "ws", 0, true, "old-key", pa, "old-user")
			done := b6CloseoutStart(t, r, "ws", "old", "old-query")
			b6CloseoutWait(t, entered)
			select {
			case <-done:
				t.Fatal("request finished before config mutation")
			default:
			}
			b6CloseoutReceipt(t, b6CloseoutDisk(t, r).SearchCalls[configKey("ws", "old")], "ws", "old", "old-query", 1, "started")
			b6CloseoutSave(t, r, "ws", 1, mode == "switch", "new-key", pb, "new-user")
			if mode == "disable" {
				_, e := b6CloseoutAdmit(r, "ws")
				b6Code(t, e, "SEARCH_DISABLED")
			} else {
				got := b6CloseoutDone(t, b6CloseoutStart(t, r, "ws", "new", "new-query"))
				b6CloseoutReceipt(t, got, "ws", "new", "new-query", 2, "success")
			}
			r.mu.Lock()
			active := r.searchActive["ws"]
			r.mu.Unlock()
			if active != 1 {
				t.Fatalf("old search slot lost: %d", active)
			}
			once.Do(func() { close(release) })
			b6CloseoutReceipt(t, b6CloseoutDone(t, done), "ws", "old", "old-query", 1, "success")
			disk := b6CloseoutDisk(t, r)
			b6CloseoutReceipt(t, disk.SearchCalls[configKey("ws", "old")], "ws", "old", "old-query", 1, "success")
			want := int32(0)
			receipts := 1
			if mode == "switch" {
				want = 1
				receipts = 2
				b6CloseoutReceipt(t, disk.SearchCalls[configKey("ws", "new")], "ws", "new", "new-query", 2, "success")
			}
			r.mu.Lock()
			active = r.searchActive["ws"]
			r.mu.Unlock()
			if oldHits.Load() != 1 || newHits.Load() != want || pa.connect.Load() != 1 || pa.auth.Load() != 1 || pb.connect.Load() != want || pb.auth.Load() != want || direct.Load() != 0 || active != 0 || len(disk.SearchCalls) != receipts || disk.Search["ws"].Version != 2 {
				t.Fatal("snapshot/count/budget mismatch")
			}
			t.Logf("mode=%s old_tls=1 old_CONNECT=1 new_tls=%d new_CONNECT=%d direct=0 receipts=%d old_version=1 saved_version=2 searchActive=0", mode, want, want, receipts)
		})
	}
	t.Run("two_workspaces", b6CloseoutWorkspaces)
	t.Run("search_model_internal_concurrent", b6CloseoutThreeWay)
	if envHits.Load() != 0 {
		t.Fatalf("environment proxy reached %d", envHits.Load())
	}
	t.Log("environment_proxy=0 database=0 external_network=0")
}

func b6CloseoutWorkspaces(t *testing.T) {
	aEntered, bEntered := make(chan struct{}, 2), make(chan struct{}, 2)
	aRelease, bRelease := make(chan struct{}), make(chan struct{})
	var aOnce, bOnce sync.Once
	defer aOnce.Do(func() { close(aRelease) })
	defer bOnce.Do(func() { close(bRelease) })
	var ah, bh atomic.Int32
	fixture := func(key string, hits *atomic.Int32, entered chan<- struct{}, release <-chan struct{}) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if r.TLS == nil || r.Header.Get("X-Subscription-Token") != key || r.Header.Get("Proxy-Authorization") != "" {
				t.Error("workspace key/TLS/proxy isolation")
			}
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			b6CloseoutReply(w, r.URL.Query().Get("q"))
		})
	}
	as, _, _ := b6TLS(t, fixture("a-key", &ah, aEntered, aRelease))
	bs, _, _ := b6TLS(t, fixture("b-key-v2", &bh, bEntered, bRelease))
	pa, pb := b6CloseoutTunnel(t, as.Listener.Addr().String(), "a-user"), b6CloseoutTunnel(t, bs.Listener.Addr().String(), "b-user")
	r := b5Registry(t, nil)
	n, direct := b6CloseoutNetwork(t, pa, pb)
	r.network = n
	b6CloseoutSave(t, r, "a", 0, true, "a-key", pa, "a-user")
	b6CloseoutSave(t, r, "b", 0, true, "b-key-v1", pb, "b-user")
	b6CloseoutSave(t, r, "b", 1, true, "b-key-v2", pb, "b-user")
	r.mu.Lock()
	r.active["a"] = 1
	r.active["search/a"] = 2
	r.mu.Unlock()
	a1 := b6CloseoutStart(t, r, "a", "same-id", "a-one")
	a2 := b6CloseoutStart(t, r, "a", "second", "a-two")
	b1 := b6CloseoutStart(t, r, "b", "same-id", "b-one")
	b6CloseoutWait(t, aEntered)
	b6CloseoutWait(t, aEntered)
	b6CloseoutWait(t, bEntered)
	_, e := b6CloseoutAdmit(r, "a")
	b6Code(t, e, "SEARCH_BUDGET_EXCEEDED")
	b2 := b6CloseoutStart(t, r, "b", "second", "b-two")
	b6CloseoutWait(t, bEntered)
	_, e = b6CloseoutAdmit(r, "b")
	b6Code(t, e, "SEARCH_BUDGET_EXCEEDED")
	r.mu.Lock()
	aa, ba, ma, collision := r.searchActive["a"], r.searchActive["b"], r.active["a"], r.active["search/a"]
	r.mu.Unlock()
	if aa != 2 || ba != 2 || ma != 1 || collision != 2 {
		t.Fatalf("peak budgets %d %d %d %d", aa, ba, ma, collision)
	}
	aOnce.Do(func() { close(aRelease) })
	b6CloseoutReceipt(t, b6CloseoutDone(t, a1), "a", "same-id", "a-one", 1, "success")
	b6CloseoutReceipt(t, b6CloseoutDone(t, a2), "a", "second", "a-two", 1, "success")
	r.mu.Lock()
	aa, ba = r.searchActive["a"], r.searchActive["b"]
	r.mu.Unlock()
	if aa != 0 || ba != 2 {
		t.Fatalf("release crossed workspaces %d %d", aa, ba)
	}
	disk := b6CloseoutDisk(t, r)
	b6CloseoutReceipt(t, disk.SearchCalls[configKey("b", "same-id")], "b", "same-id", "b-one", 2, "started")
	bOnce.Do(func() { close(bRelease) })
	b6CloseoutReceipt(t, b6CloseoutDone(t, b1), "b", "same-id", "b-one", 2, "success")
	b6CloseoutReceipt(t, b6CloseoutDone(t, b2), "b", "second", "b-two", 2, "success")
	disk = b6CloseoutDisk(t, r)
	for _, tc := range []struct {
		ws, id, q string
		v         int64
	}{{"a", "same-id", "a-one", 1}, {"a", "second", "a-two", 1}, {"b", "same-id", "b-one", 2}, {"b", "second", "b-two", 2}} {
		b6CloseoutReceipt(t, disk.SearchCalls[configKey(tc.ws, tc.id)], tc.ws, tc.id, tc.q, tc.v, "success")
	}
	r.mu.Lock()
	aa, ba, ma, collision = r.searchActive["a"], r.searchActive["b"], r.active["a"], r.active["search/a"]
	r.mu.Unlock()
	if aa != 0 || ba != 0 || ma != 1 || collision != 2 || ah.Load() != 2 || bh.Load() != 2 || pa.connect.Load() != 2 || pb.connect.Load() != 2 || pa.auth.Load() != 2 || pb.auth.Load() != 2 || direct.Load() != 0 || len(disk.SearchCalls) != 4 || disk.Search["a"].Version != 1 || disk.Search["b"].Version != 2 || disk.Search["a"].Proxy.URL != pa.url || disk.Search["b"].Proxy.URL != pb.url {
		t.Fatal("workspace final isolation/count mismatch")
	}
	t.Log("workspaces=2 same_registry=true same_request_ids=true tls_a=2 tls_b=2 CONNECT_a=2 CONNECT_b=2 peak_searchActive=2/2 after_a_release=0/2 final_searchActive=0/0 model_active_unchanged=1 collision_active_unchanged=2 receipts=4 versions=1/2 direct=0")
}

func b6CloseoutThreeWay(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "success"
		if reject {
			name = "proxy_reject"
		}
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{}, 3)
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var searchHits, modelHits, internalHits atomic.Int32
			wait := func(r *http.Request) bool {
				entered <- struct{}{}
				select {
				case <-release:
					return true
				case <-r.Context().Done():
					return false
				}
			}
			search, _, _ := b6TLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				searchHits.Add(1)
				if r.TLS == nil || r.Header.Get("X-Subscription-Token") != "three-search-key" || r.Header.Get("Proxy-Authorization") != "" {
					t.Error("search credential boundary")
				}
				if wait(r) {
					b6CloseoutReply(w, r.URL.Query().Get("q"))
				}
			}))
			model, client := b5TLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				modelHits.Add(1)
				if r.TLS == nil || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer three-model-key" || r.Header.Get("Proxy-Authorization") != "" {
					t.Error("model credential boundary")
				}
				io.Copy(io.Discard, r.Body)
				if wait(r) {
					io.WriteString(w, `{"model":"fixture","choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`)
				}
			}))
			internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				internalHits.Add(1)
				if r.URL.Path != "/get_login_info" || r.Header.Get("Authorization") != "Bearer fixture-onebot" || r.Header.Get("Proxy-Authorization") != "" {
					t.Error("internal route/credential boundary")
				}
				if wait(r) {
					io.WriteString(w, `{"status":"ok","retcode":0,"data":{"user_id":123456,"nickname":"fixture"}}`)
				}
			}))
			t.Cleanup(internal.Close)
			ps, pm := b6CloseoutTunnel(t, search.Listener.Addr().String(), "search-user"), b6CloseoutTunnel(t, model.Listener.Addr().String(), "model-user")
			if reject {
				ps.reject.Store(true)
				pm.reject.Store(true)
				ps.release = release
				pm.release = release
			}
			r := b5Registry(t, client)
			n, direct := b6CloseoutNetwork(t, ps, pm)
			r.network = n
			b6CloseoutSave(t, r, "ws", 0, true, "three-search-key", ps, "search-user")
			_, e := r.save("ws", "", modelProfileWrite{Name: "fixture", Model: "fixture", BaseURL: "https://model.bba.example/v1", Enabled: true, IsDefault: true, TimeoutSeconds: 10, MaxConcurrent: 1, SecretAction: "replace", APIKey: "three-model-key"}, false)
			if e != nil {
				t.Fatal(e)
			}
			yes, v, u := true, int64(0), pm.url
			_, e = r.saveProxy("ws", "fixture-operator", proxyWrite{Enabled: &yes, URL: &u, Expected: &v, Action: "replace", Username: "model-user", Password: "fixture-password"})
			if e != nil {
				t.Fatal(e)
			}
			provider, e := LoadLoginProvider(writeLoginProviderConfig(t, internal.URL, "fixture-webui", "fixture-onebot"), false)
			if e != nil {
				t.Fatal(e)
			}
			sd := b6CloseoutStart(t, r, "ws", name, "three-query")
			md, id := make(chan error, 1), make(chan error, 1)
			go func() {
				out, e := r.Complete(t.Context(), ModelCall{WorkspaceID: "ws"})
				if e == nil && (out.Content != "{}" || out.ModelID != "fixture") {
					e = errors.New("model response mismatch")
				}
				md <- e
			}()
			go func() {
				out, e := provider.Identity(t.Context(), napcatTestAccount())
				if e == nil && out.SelfID != "123456" {
					e = errors.New("internal identity mismatch")
				}
				id <- e
			}()
			if reject {
				b6CloseoutWait(t, ps.entered)
				b6CloseoutWait(t, pm.entered)
				b6CloseoutWait(t, entered)
			} else {
				for i := 0; i < 3; i++ {
					b6CloseoutWait(t, entered)
				}
			}
			// All three are now inside real servers, not pre-cancelled/admission-only.
			select {
			case <-sd:
				t.Fatal("search not in flight")
			default:
			}
			select {
			case <-md:
				t.Fatal("model not in flight")
			default:
			}
			select {
			case <-id:
				t.Fatal("internal not in flight")
			default:
			}
			r.mu.Lock()
			sa, ma := r.searchActive["ws"], r.active["ws"]
			r.mu.Unlock()
			if sa != 1 || ma != 1 {
				t.Fatalf("concurrent budgets %d %d", sa, ma)
			}
			once.Do(func() { close(release) })
			var sr b6CloseoutResult
			select {
			case sr = <-sd:
			case <-time.After(5 * time.Second):
				t.Fatal("search timeout")
			}
			wantStatus := "success"
			if reject {
				wantStatus = "SEARCH_PROVIDER_FAILURE"
				b6Code(t, sr.err, wantStatus)
			} else {
				b6Code(t, sr.err, "")
			}
			b6CloseoutReceipt(t, sr.receipt, "ws", name, "three-query", 1, wantStatus)
			select {
			case e = <-md:
			case <-time.After(5 * time.Second):
				t.Fatal("model timeout")
			}
			if reject {
				b6Code(t, e, modelCodeProviderFailed)
			} else {
				b6Code(t, e, "")
			}
			select {
			case e = <-id:
			case <-time.After(5 * time.Second):
				t.Fatal("internal timeout")
			}
			if e != nil {
				t.Fatal(e)
			}
			disk := b6CloseoutDisk(t, r)
			b6CloseoutReceipt(t, disk.SearchCalls[configKey("ws", name)], "ws", name, "three-query", 1, wantStatus)
			want := int32(1)
			if reject {
				want = 0
			}
			r.mu.Lock()
			sa, ma = r.searchActive["ws"], r.active["ws"]
			r.mu.Unlock()
			if searchHits.Load() != want || modelHits.Load() != want || internalHits.Load() != 1 || ps.connect.Load() != 1 || pm.connect.Load() != 1 || ps.auth.Load() != 1 || pm.auth.Load() != 1 || direct.Load() != 0 || sa != 0 || ma != 0 || len(disk.SearchCalls) != 1 {
				t.Fatal("three-way count/isolation mismatch")
			}
			t.Logf("mode=%s actual_concurrent=3 search_tls=%d model_tls=%d internal_http=1 search_CONNECT=1 model_CONNECT=1 proxy_auth=2 direct_fallback=0 peak_search/model=1/1 final_search/model=0/0 receipts=1 status=%s", name, want, want, wantStatus)
		})
	}
}
