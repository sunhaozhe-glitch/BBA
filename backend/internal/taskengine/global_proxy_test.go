package taskengine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type b5Proxy struct {
	url   string
	count atomic.Int32
	fail  atomic.Bool
	auth  atomic.Int32
}

func b5FixtureProxy(t *testing.T, kind, target string) *b5Proxy {
	t.Helper()
	return b5FixtureProxyAt(t, kind, target, "127.0.0.1:0")
}
func b5FixtureProxyAt(t *testing.T, kind, target, addr string) *b5Proxy {
	t.Helper()
	l, e := net.Listen("tcp", addr)
	if e != nil {
		t.Fatal(e)
	}
	p := &b5Proxy{url: kind + "://" + l.Addr().String()}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				p.count.Add(1)
				if p.fail.Load() {
					return
				}
				if kind == "http" {
					req, e := http.ReadRequest(bufio.NewReader(c))
					if e != nil || req.Method != "CONNECT" || !strings.HasPrefix(req.Host, "8.8.8.8:") {
						return
					}
					if req.Header.Get("Proxy-Authorization") != "" {
						p.auth.Add(1)
					}
					io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				} else {
					b := make([]byte, 3)
					if _, e := io.ReadFull(c, b); e != nil || b[0] != 5 {
						return
					}
					c.Write([]byte{5, b[2]})
					if b[2] == 2 {
						a := make([]byte, 2)
						if _, e := io.ReadFull(c, a); e != nil {
							return
						}
						if _, e := io.ReadFull(c, make([]byte, int(a[1]))); e != nil {
							return
						}
						if _, e := io.ReadFull(c, a[:1]); e != nil {
							return
						}
						if _, e := io.ReadFull(c, make([]byte, int(a[0]))); e != nil {
							return
						}
						p.auth.Add(1)
						c.Write([]byte{1, 0})
					}
					b = make([]byte, 10)
					if _, e := io.ReadFull(c, b); e != nil || b[3] != 1 || net.IP(b[4:8]).String() != "8.8.8.8" {
						return
					}
					c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				}
				upstream, e := net.Dial("tcp", target)
				if e != nil {
					return
				}
				defer upstream.Close()
				done := make(chan struct{})
				go func() { io.Copy(upstream, c); upstream.Close(); close(done) }()
				io.Copy(c, upstream)
				c.Close()
				<-done
			}()
		}
	}()
	return p
}
func b5TLS(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) { return b5TLSAt(t, h, "") }
func b5TLSAt(t *testing.T, h http.Handler, addr string) (*httptest.Server, *http.Client) {
	t.Helper()
	k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "model.bba.example"}, DNSNames: []string{"model.bba.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, cert, cert, &k.PublicKey, k)
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewUnstartedServer(h)
	if addr != "" {
		srv.Listener.Close()
		srv.Listener, e = net.Listen("tcp", addr)
		if e != nil {
			t.Fatal(e)
		}
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: k}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	leaf, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	roots.AddCert(leaf)
	return srv, &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}, Timeout: 3 * time.Second}
}
func b5Registry(t *testing.T, client *http.Client) *ModelRegistry {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	r, e := OpenModelRegistry(filepath.Join(dir, "models.json"), "", nil, client)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(r.Close)
	return r
}
func b5Save(t *testing.T, r *ModelRegistry, ws string, enabled bool, u, action string) {
	t.Helper()
	old, e := r.GetProxySettings(ws, true)
	if e != nil {
		t.Fatal(e)
	}
	v := old.Version
	w := proxyWrite{Enabled: &enabled, URL: &u, Expected: &v, Action: action}
	if action == "replace" {
		w.Username = "fixture-proxy-user"
		w.Password = "fixture-proxy-password"
	}
	if _, e := r.saveProxy(ws, "principal_fixture", w); e != nil {
		t.Fatal(e)
	}
}
func TestB5RealModelRoutesAndSnapshots(t *testing.T) {
	s := fixtureStore(t)
	task := modelTestTask(t, s, 3)
	var providerCount atomic.Int32
	var block atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv, client := b5TLS(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		providerCount.Add(1)
		if req.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy auth reached model")
		}
		body, _ := io.ReadAll(req.Body)
		if bytes.Contains(body, []byte("fixture-proxy-password")) || bytes.Contains(body, []byte("fixture-proxy-user")) {
			t.Error("proxy credentials reached prompt")
		}
		if block.Load() {
			entered <- struct{}{}
			<-release
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "fixture", "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": modelTestProposal(t, task)}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	}))
	target := srv.Listener.Addr().String()
	connect := b5FixtureProxy(t, "http", target)
	socks := b5FixtureProxy(t, "socks5", target)
	r := b5Registry(t, client)
	var direct atomic.Int32
	n := defaultModelNetwork()
	n.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	dial := n.dial
	n.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "8.8.8.8:") {
			direct.Add(1)
			addr = target
		}
		return dial(ctx, network, addr)
	}
	r.network = &n
	t.Setenv("HTTP_PROXY", connect.url)
	t.Setenv("HTTPS_PROXY", connect.url)
	t.Setenv("NO_PROXY", "")
	write := modelProfileWrite{Name: "fixture", Model: "fixture", BaseURL: "https://model.bba.example/v1", Enabled: true, IsDefault: true, TimeoutSeconds: 3, MaxConcurrent: 4, SecretAction: "replace", APIKey: "fixture-model-key"}
	m, e := r.save(task.WorkspaceId, "", write, false)
	if e != nil {
		t.Fatal(e)
	}
	invoke := func(ws string) {
		t.Helper()
		if _, e := r.Complete(context.Background(), ModelCall{WorkspaceID: ws}); e != nil {
			t.Fatal(e)
		}
	}
	invoke(task.WorkspaceId)
	if direct.Load() != 1 || connect.count.Load() != 0 {
		t.Fatal("absent config inherited environment proxy")
	}
	b5Save(t, r, task.WorkspaceId, true, connect.url, "replace")
	invoke(task.WorkspaceId)
	if connect.count.Load() != 1 || direct.Load() != 1 || connect.auth.Load() != 1 {
		t.Fatal("CONNECT not consumed")
	}
	// Exercise the REAL saved model connection-test API (including shape validation).
	a := modelTestActor()
	a.Permissions["operator.read"] = true
	a.Permissions["model.configure"] = true
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/operator/models/"+m.ID+"/test", nil)
	r.Handler(func(*http.Request) (Actor, error) { return a, nil }, s).ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("connection-test %d %s", rr.Code, rr.Body)
	}
	// Real B1 budget/idempotency/store consumer, not a replacement provider.
	request := modelTestRequest(task)
	if _, e := s.RunPlanModelRequest(context.Background(), a, "b5-real-plan", request, r); e != nil {
		t.Fatal(e)
	}
	if connect.count.Load() != 3 {
		t.Fatalf("real routes did not proxy: %d", connect.count.Load())
	}
	b5Save(t, r, task.WorkspaceId, true, socks.url, "keep")
	invoke(task.WorkspaceId)
	if socks.count.Load() != 1 || socks.auth.Load() != 1 {
		t.Fatal("SOCKS not consumed")
	}
	b5Save(t, r, task.WorkspaceId, false, socks.url, "keep")
	invoke(task.WorkspaceId)
	if direct.Load() != 2 {
		t.Fatal("disabled did not direct")
	}
	if _, e := r.save("ws_other", "", write, false); e != nil {
		t.Fatal(e)
	}
	invoke("ws_other")
	if direct.Load() != 3 {
		t.Fatal("workspace proxy leaked")
	}
	b5Save(t, r, task.WorkspaceId, true, connect.url, "keep")
	block.Store(true)
	done := make(chan error, 1)
	go func() { _, e := r.Complete(context.Background(), ModelCall{WorkspaceID: task.WorkspaceId}); done <- e }()
	<-entered
	b5Save(t, r, task.WorkspaceId, true, socks.url, "clear")
	block.Store(false)
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	invoke(task.WorkspaceId)
	if connect.count.Load() != 4 || socks.count.Load() != 2 {
		t.Fatal("inflight snapshot changed/replayed")
	}
	before := providerCount.Load()
	socks.fail.Store(true)
	if _, e := r.Complete(context.Background(), ModelCall{WorkspaceID: task.WorkspaceId}); e == nil {
		t.Fatal("proxy failure hidden")
	}
	if direct.Load() != 3 || providerCount.Load() != before {
		t.Fatal("failed proxy fell back")
	}
	path := r.path
	r.Close()
	r, e = OpenModelRegistry(path, "", nil, client)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	r.network = &n
	p, e := r.GetProxySettings(task.WorkspaceId, true)
	if e != nil || !p.Enabled || p.URL != socks.url || p.HasCredentials {
		t.Fatal("restart snapshot lost")
	}
	if e = os.Rename(path, path+".unavailable"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Complete(context.Background(), ModelCall{WorkspaceID: task.WorkspaceId}); Code(e) != taskCodeStoreUnavailable {
		t.Fatalf("read failure not closed: %v", e)
	}
	if providerCount.Load() != before {
		t.Fatal("store failure invoked provider")
	}
	t.Logf("provider=%d direct=%d CONNECT=%d SOCKS=%d failed_proxy_provider_delta=0 cross_workspace_direct=true immutable_snapshot=true", providerCount.Load(), direct.Load(), connect.count.Load(), socks.count.Load())
}
