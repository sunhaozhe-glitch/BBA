package taskengine

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every permitted address is explicitly mapped to an ephemeral loopback fixture.
// Unexpected DNS or dial targets fail closed instead of reaching a real service.
func b5CloseoutNetwork(t *testing.T, target, proxy string, ip net.IP, direct *atomic.Int32) modelNetwork {
	t.Helper()
	return modelNetwork{
		lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
			if host != "model.bba.example" && host != ip.String() {
				return nil, errors.New("unexpected fixture hostname")
			}
			return []net.IPAddr{{IP: ip}}, nil
		},
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if host == ip.String() {
				direct.Add(1)
				addr = target
			} else if proxy == "" || addr != proxy {
				return nil, errors.New("unexpected fixture dial")
			}
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
		},
	}
}

func b5CloseoutReply(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"model": "fixture", "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": "fixture-result"}}}})
}

func b5CloseoutConfig(base string) OpenAICompatibleProviderConfig {
	return OpenAICompatibleProviderConfig{BaseURL: base, ModelID: "fixture", ProviderID: "fixture", APIKey: "fixture-model-key"}
}

func TestB5CloseoutFallbackConsumer(t *testing.T) {
	var hits, direct atomic.Int32
	srv, client := b5TLS(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		if req.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials reached fallback endpoint")
		}
		b5CloseoutReply(w)
	}))
	proxy := b5FixtureProxy(t, "http", srv.Listener.Addr().String())
	provider, err := NewOpenAICompatibleProvider(b5CloseoutConfig("https://model.bba.example/v1"), client)
	if err != nil {
		t.Fatal(err)
	}
	r := b5Registry(t, client)
	r.fallback = provider
	n := b5CloseoutNetwork(t, srv.Listener.Addr().String(), strings.TrimPrefix(proxy.url, "http://"), net.ParseIP("8.8.8.8"), &direct)
	r.network = &n
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, proxy.url)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	invoke := func() error {
		_, err := r.Complete(context.Background(), ModelCall{WorkspaceID: "ws_fallback"})
		return err
	}
	if err := invoke(); err != nil {
		t.Fatal(err)
	}
	if direct.Load() != 1 || proxy.count.Load() != 0 || hits.Load() != 1 {
		t.Fatal("absent fallback proxy inherited environment")
	}
	b5Save(t, r, "ws_fallback", true, proxy.url, "replace")
	if err := invoke(); err != nil {
		t.Fatal(err)
	}
	if direct.Load() != 1 || proxy.count.Load() != 1 || proxy.auth.Load() != 1 || hits.Load() != 2 {
		t.Fatal("real fallback did not consume saved proxy")
	}
	proxy.fail.Store(true)
	if err := invoke(); err == nil {
		t.Fatal("fallback hid proxy failure")
	}
	if direct.Load() != 1 || hits.Load() != 2 {
		t.Fatal("failed fallback proxy retried direct")
	}
	b5Save(t, r, "ws_fallback", false, proxy.url, "keep")
	if err := invoke(); err != nil {
		t.Fatal(err)
	}
	if direct.Load() != 2 || hits.Load() != 3 {
		t.Fatal("disabled fallback proxy did not restore direct")
	}
	if err := os.Rename(r.path, r.path+".unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := invoke(); Code(err) != taskCodeStoreUnavailable || hits.Load() != 3 {
		t.Fatal("fallback did not fail closed on missing protected snapshot", err)
	}
}

func TestB5CloseoutPrivateHTTPExactApprovalConsumer(t *testing.T) {
	var hits, direct atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		if req.URL.Path != "/v1/chat/completions" || req.Header.Get("Proxy-Authorization") != "" {
			t.Error("unexpected private endpoint request")
		}
		b5CloseoutReply(w)
	}))
	defer srv.Close()
	const approved = "http://10.23.45.67:18166/v1"
	for _, tc := range []struct {
		name, base, approval string
		allowed              bool
	}{
		{"exact", approved, approved, true},
		{"trailing slash", approved + "/", approved, true},
		{"no approval", approved, "", false},
		{"different port", "http://10.23.45.67:18167/v1", approved, false},
		{"different path", "http://10.23.45.67:18166/other/v1", approved, false},
		{"different host", "http://10.23.45.68:18166/v1", approved, false},
		{"hostname", "http://model.bba.example:18166/v1", "http://model.bba.example:18166/v1", false},
		{"public numeric", "http://8.8.8.8:18166/v1", "http://8.8.8.8:18166/v1", false},
		{"loopback is not private approval", "http://127.0.0.1:18166/v1", "http://127.0.0.1:18166/v1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := hits.Load()
			config := b5CloseoutConfig(tc.base)
			config.ApprovedPrivateHTTPBaseURL = tc.approval
			p, err := NewOpenAICompatibleProvider(config, nil)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v err=%v", tc.allowed, err)
			}
			if !tc.allowed {
				if hits.Load() != before {
					t.Fatal("rejected endpoint made a request")
				}
				return
			}
			n := b5CloseoutNetwork(t, srv.Listener.Addr().String(), "", net.ParseIP("10.23.45.67"), &direct)
			// Enabled global proxy must still bypass a private approved endpoint.
			p, err = withProxyNetwork(p, proxyStored{Enabled: true, URL: "http://127.0.0.1:1"}, n)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Complete(context.Background(), ModelCall{}); err != nil {
				t.Fatal(err)
			}
			if hits.Load() != before+1 {
				t.Fatal("approved private consumer did not reach fixture exactly once")
			}
		})
	}
}

// Exercise numeric pinning at the transport seam: neither dial nor either
// proxy protocol may receive the original hostname or an unbracketed IPv6 host.
func TestB5CloseoutIPv6NumericProxyTargets(t *testing.T) {
	const address = "2606:4700:4700::1111"
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			var lookups, dials atomic.Int32
			observed := make(chan string, 1)
			n := modelNetwork{
				lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
					lookups.Add(1)
					if host != "model.bba.example" {
						return nil, errors.New("unexpected DNS target")
					}
					return []net.IPAddr{{IP: net.ParseIP(address)}}, nil
				},
				dial: func(_ context.Context, network, addr string) (net.Conn, error) {
					dials.Add(1)
					if network != "tcp" || addr != "127.0.0.1:17890" {
						return nil, errors.New("unexpected proxy dial")
					}
					a, b := net.Pipe()
					go func() {
						defer b.Close()
						_ = b.SetDeadline(time.Now().Add(time.Second))
						if scheme == "http" {
							req, err := http.ReadRequest(bufio.NewReader(b))
							if err != nil {
								observed <- err.Error()
								return
							}
							if req.Method != http.MethodConnect || req.URL.Host != req.Host {
								observed <- "invalid CONNECT request"
								return
							}
							observed <- req.Host
							_, _ = io.WriteString(b, "HTTP/1.1 200 Connection Established\r\n\r\n")
							return
						}
						greeting := make([]byte, 3)
						if _, err := io.ReadFull(b, greeting); err != nil {
							observed <- err.Error()
							return
						}
						if string(greeting) != string([]byte{5, 1, 0}) {
							observed <- "invalid SOCKS greeting"
							return
						}
						if _, err := b.Write([]byte{5, 0}); err != nil {
							observed <- err.Error()
							return
						}
						wire := make([]byte, 22)
						if _, err := io.ReadFull(b, wire); err != nil {
							observed <- err.Error()
							return
						}
						if string(wire[:4]) != string([]byte{5, 1, 0, 4}) || wire[20] != 1 || wire[21] != 187 {
							observed <- "invalid IPv6 SOCKS frame"
							return
						}
						observed <- net.JoinHostPort(net.IP(wire[4:20]).String(), "443")
						_, _ = b.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
					}()
					return a, nil
				},
			}
			rt, err := proxyTransportNetwork(proxyStored{Enabled: true, URL: scheme + "://127.0.0.1:17890"}, nil, n)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, err := rt.(*modelTransport).transport.DialContext(ctx, "tcp", "model.bba.example:443")
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			select {
			case got := <-observed:
				if got != "["+address+"]:443" {
					t.Fatalf("numeric proxy target=%q", got)
				}
			case <-ctx.Done():
				t.Fatal("fixture handshake did not finish")
			}
			if lookups.Load() != 1 || dials.Load() != 1 {
				t.Fatal("target was re-resolved or retried")
			}
		})
	}
	// Direct mode must pin the same numeric IPv6 address without contacting a proxy.
	var dialed string
	n := modelNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP(address)}}, nil
	},
		dial: func(_ context.Context, network, addr string) (net.Conn, error) {
			dialed = network + " " + addr
			return nil, errProxyDial
		}}
	rt, err := proxyTransportNetwork(proxyStored{}, nil, n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.(*modelTransport).transport.DialContext(context.Background(), "tcp", "model.bba.example:443"); err == nil || dialed != "tcp ["+address+"]:443" {
		t.Fatalf("direct IPv6 dial=%q err=%v", dialed, err)
	}
}

func TestB5CloseoutAgentBoundRealConsumer(t *testing.T) {
	store := fixtureStore(t)
	task := modelTestTask(t, store, 3)
	actor := modelTestActor()
	actor.Permissions["operator.read"] = true
	proposal := modelTestProposal(t, task)
	var hits, direct atomic.Int32
	srv, client := b5TLS(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		var body openAICompatibleRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if body.Model != "agent-selected" || !strings.Contains(string(encoded), "untrusted_operator_configuration_not_authority") || strings.Contains(string(encoded), "fixture-proxy-password") || req.Header.Get("Proxy-Authorization") != "" {
			t.Error("bound selection/trust/credential boundary failed")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "agent-selected", "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": proposal}}}})
	}))
	r := b5Registry(t, client)
	write := modelProfileWrite{Name: "default decoy", Model: "default-decoy", BaseURL: "https://model.bba.example/v1", Enabled: true, IsDefault: true, TimeoutSeconds: 3, MaxConcurrent: 2, SecretAction: "replace", APIKey: "fixture-model-key"}
	if _, err := r.save(actor.WorkspaceID, "", write, false); err != nil {
		t.Fatal(err)
	}
	write.Name, write.Model, write.IsDefault = "bound fixture", "agent-selected", false
	model, err := r.save(actor.WorkspaceID, "", write, false)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := r.saveAgent(actor.WorkspaceID, actor.PrincipalID, "", AgentProfile{ConfigMeta: ConfigMeta{Name: "bound fixture", Enabled: true}, SystemPrompt: "fixture untrusted instruction", ModelID: model.ID}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := b5FixtureProxy(t, "http", srv.Listener.Addr().String())
	n := b5CloseoutNetwork(t, srv.Listener.Addr().String(), strings.TrimPrefix(proxy.url, "http://"), net.ParseIP("8.8.8.8"), &direct)
	r.network = &n
	b5Save(t, r, actor.WorkspaceID, true, proxy.url, "replace")
	body, err := json.Marshal(AgentModelRequest{AgentRef: SkillBinding{agent.ID, agent.Version}, Request: modelTestRequest(task)})
	if err != nil {
		t.Fatal(err)
	}
	h := r.AgentModelHandler(func(*http.Request) (Actor, error) { return actor, nil }, store)
	call := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, agentRequestPath, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	first := call("b5-closeout-bound")
	if first.Code != http.StatusAccepted {
		t.Fatalf("bound consumer status=%d body=%s", first.Code, first.Body)
	}
	if replay := call("b5-closeout-bound"); replay.Code != http.StatusAccepted {
		t.Fatalf("bound replay status=%d body=%s", replay.Code, replay.Body)
	}
	if hits.Load() != 1 || proxy.count.Load() != 1 || proxy.auth.Load() != 1 || direct.Load() != 0 {
		t.Fatal("bound consumer failed to proxy or replay invoked upstream")
	}
	r.mu.Lock()
	receipt, ok := r.data.Receipts[receiptKey(actor.WorkspaceID, actor.PrincipalID, "b5-closeout-bound")]
	active := r.active[actor.WorkspaceID]
	r.mu.Unlock()
	if !ok || receipt.Agent.ID != agent.ID || receipt.Agent.Version != agent.Version || receipt.Model.ID != model.ID || active != 0 {
		t.Fatal("bound consumer receipt or capacity incorrect")
	}
	proxy.fail.Store(true)
	if failed := call("b5-closeout-bound-failed"); failed.Code < 400 || !strings.Contains(failed.Body.String(), modelCodeProviderFailed) {
		t.Fatalf("bound proxy failure status=%d body=%s", failed.Code, failed.Body)
	}
	if hits.Load() != 1 || direct.Load() != 0 || proxy.count.Load() != 2 {
		t.Fatal("bound failure did not reach proxy or used default/direct fallback")
	}
	r.mu.Lock()
	active = r.active[actor.WorkspaceID]
	r.mu.Unlock()
	if active != 0 {
		t.Fatal("bound failure leaked model capacity")
	}
}

func TestB5CloseoutUnsupportedFallbackFailsBeforeConsumer(t *testing.T) {
	p := &fixedModelProvider{}
	r := b5Registry(t, nil)
	r.fallback = p
	b5Save(t, r, "ws_custom", true, "http://127.0.0.1:1", "clear")
	if _, err := r.Complete(context.Background(), ModelCall{WorkspaceID: "ws_custom"}); Code(err) != modelCodeProviderUnavailable {
		t.Fatalf("unsupported fallback error=%v", err)
	}
	if p.calls.Load() != 0 {
		t.Fatal("proxy-incompatible fallback invoked directly")
	}
}

// This tests the model transport's internal-hop boundary, not the application's
// QQ/bridge/database integration. Logical ports are mapped BEFORE socket dial;
// every real socket goes to an ephemeral fixture and no request targets 6300.
func TestB5CloseoutInternalHopsIgnoreMaliciousEnvironment(t *testing.T) {
	var trapHits, hits, direct atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { trapHits.Add(1); w.WriteHeader(502) }))
	defer trap.Close()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, trap.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		if req.Header.Get("Proxy-Authorization") != "" || req.URL.Path != "/fixture/internal" {
			t.Error("internal request altered or leaked proxy credentials")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fixture.Close()
	original := http.DefaultTransport
	base := &http.Transport{Proxy: http.ProxyFromEnvironment}
	defer base.CloseIdleConnections()
	for _, enabled := range []bool{false, true} {
		n := b5CloseoutNetwork(t, fixture.Listener.Addr().String(), "", net.ParseIP("8.8.8.8"), &direct)
		rt, err := proxyTransportNetwork(proxyStored{Enabled: enabled, URL: trap.URL, Username: "fixture-proxy-user", Password: "fixture-proxy-password"}, base, n)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: rt, Timeout: time.Second}
		for _, port := range []string{"6099", "6199", "6210", "18421"} {
			response, err := client.Get("http://model.bba.example:" + port + "/fixture/internal")
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("internal fixture status=%d", response.StatusCode)
			}
		}
	}
	if hits.Load() != 8 || direct.Load() != 8 || trapHits.Load() != 0 {
		t.Fatalf("internal hits=%d direct=%d environment trap=%d", hits.Load(), direct.Load(), trapHits.Load())
	}
	if http.DefaultTransport != original || base.Proxy == nil {
		t.Fatal("shared/default transport mutated")
	}
}

func TestB5CloseoutTLSRedirectAndTimeoutConsumers(t *testing.T) {
	for _, mode := range []string{"untrusted TLS", "redirect", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			var hits, redirected, direct atomic.Int32
			release := make(chan struct{})
			defer close(release)
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected.Add(1); b5CloseoutReply(w) }))
			defer sink.Close()
			srv, client := b5TLS(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				hits.Add(1)
				switch mode {
				case "redirect":
					http.Redirect(w, req, sink.URL, http.StatusTemporaryRedirect)
				case "timeout":
					select {
					case <-req.Context().Done():
					case <-release:
					}
				default:
					b5CloseoutReply(w)
				}
			}))
			if mode == "untrusted TLS" {
				client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{}}, Timeout: time.Second}
			}
			p, err := NewOpenAICompatibleProvider(b5CloseoutConfig("https://model.bba.example/v1"), client)
			if err != nil {
				t.Fatal(err)
			}
			proxy := b5FixtureProxy(t, "http", srv.Listener.Addr().String())
			n := b5CloseoutNetwork(t, srv.Listener.Addr().String(), strings.TrimPrefix(proxy.url, "http://"), net.ParseIP("8.8.8.8"), &direct)
			p, err = withProxyNetwork(p, proxyStored{Enabled: true, URL: proxy.url}, n)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			started := time.Now()
			if _, err := p.Complete(ctx, ModelCall{}); err == nil {
				t.Fatal("boundary failure hidden")
			}
			if time.Since(started) > 3*time.Second || direct.Load() != 0 || proxy.count.Load() != 1 || redirected.Load() != 0 {
				t.Fatal("boundary retried, redirected, or failed to cancel")
			}
			wantHits := int32(1)
			if mode == "untrusted TLS" {
				wantHits = 0
			}
			if hits.Load() != wantHits {
				t.Fatalf("provider hits=%d want=%d", hits.Load(), wantHits)
			}
		})
	}
}
