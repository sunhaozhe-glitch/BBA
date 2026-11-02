package taskengine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// A subprocess gives crypto/x509 a fresh system-root cache. The ONLY trust
// override is a temporary test CA via SSL_CERT_FILE, never InsecureSkipVerify,
// global transport mutation, endpoint configuration or a fake RoundTripper.
func TestB6ProviderOffline(t *testing.T) {
	b6ProviderOffline(t, false)
}

func b6ProviderOffline(t *testing.T, hardTimeoutOnly bool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert := &x509.Certificate{SerialNumber: big.NewInt(6), Subject: pkix.Name{CommonName: "B6 offline fixture"}, DNSNames: []string{"api.search.brave.com", "api.tavily.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ca, kp := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "key.pem")
	for p, b := range map[string][]byte{ca: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), kp: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})} {
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"-test.run=^TestB6ProviderFixtures$", "-test.v", "-test.timeout=150s"}
	if hardTimeoutOnly {
		args[0] = "-test.run=^TestB6ProviderFixtures$/^hard_timeout$"
	} else {
		// Mandatory real wall-clock coverage lives in the b6_wall_timeout gate.
		// Default coverage still exercises caller deadline and header/body cancellation.
		args = append(args, "-test.skip=^TestB6ProviderFixtures$/^hard_timeout$")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...)
	cmd.Env = append(os.Environ(), "BBA_B6_FIXTURE_CA="+ca, "BBA_B6_FIXTURE_KEY="+kp, "SSL_CERT_FILE="+ca, "SSL_CERT_DIR="+dir)
	out, err := cmd.CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatal(err)
	}
}

func b6TLS(t *testing.T, h http.Handler) (*httptest.Server, modelNetwork, *atomic.Int32) {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(os.Getenv("BBA_B6_FIXTURE_CA"), os.Getenv("BBA_B6_FIXTURE_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	dials := &atomic.Int32{}
	n := modelNetwork{
		lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
			if host != "api.search.brave.com" && host != "api.tavily.com" {
				t.Errorf("unexpected DNS (result was followed?): %s", host)
				return nil, errors.New("blocked")
			}
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			if address != "8.8.8.8:443" {
				t.Errorf("unexpected dial %s", address)
				return nil, errors.New("blocked")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
		},
	}
	return srv, n, dials
}
func b6Code(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if err == nil || err.Error() != want || Code(err) != want {
		t.Fatalf("error=%v want=%s", err, want)
	}
}
func TestB6ProviderFixtures(t *testing.T) {
	if os.Getenv("BBA_B6_FIXTURE_CA") == "" {
		t.Skip("run through TestB6ProviderOffline")
	}
	var envHits atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { envHits.Add(1); w.WriteHeader(502) }))
	defer trap.Close()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(k, trap.URL)
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	for _, source := range []string{"brave-free", "tavily"} {
		t.Run(source, func(t *testing.T) {
			var calls atomic.Int32
			_, n, dials := b6TLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.TLS == nil || r.Header.Get("Accept") != "application/json" || r.Header.Get("Proxy-Authorization") != "" {
					t.Error("TLS/header boundary")
				}
				if source == "brave-free" {
					if r.Method != "GET" || r.Host != "api.search.brave.com" || r.URL.Path != "/res/v1/web/search" || r.URL.Query().Get("q") != "中文 &?=+" || r.URL.Query().Get("count") != "5" || r.Header.Get("X-Subscription-Token") != "fixture-secret" {
						t.Error("Brave protocol")
					}
				} else {
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil {
						t.Error("request JSON")
					}
					if r.Method != "POST" || r.Host != "api.tavily.com" || r.URL.Path != "/search" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer fixture-secret" || body["api_key"] != nil || body["query"] != "中文 &?=+" || body["max_results"] != float64(5) || body["search_depth"] != "basic" || body["include_answer"] != false || body["include_raw_content"] != false {
						t.Error("Tavily protocol")
					}
				}
				rows := []map[string]string{}
				for _, u := range []string{"http://127.0.0.1/", "http://localhost/", "http://10.0.0.1/", "http://[::1]/", "https://user:secret@example.com/", "file:///etc/passwd", "https://example.com/%0a", "http://2130706433/", "http://127.1/", "https://example.com/" + strings.Repeat("a", 2048)} {
					rows = append(rows, map[string]string{"url": u})
				}
				for i := 0; i < 8; i++ {
					rows = append(rows, map[string]string{"url": "https://example.com/path", "title": strings.Repeat("中", 300), "description": strings.Repeat("文", 1000), "content": strings.Repeat("文", 1000)})
				}
				var payload any = map[string]any{"results": rows}
				if source == "brave-free" {
					payload = map[string]any{"web": payload}
				}
				json.NewEncoder(w).Encode(payload)
			}))
			results, err := searchProviderCall(t.Context(), source, "fixture-secret", "中文 &?=+", proxyStored{}, &n)
			b6Code(t, err, "")
			if len(results) != 5 || calls.Load() != 1 || dials.Load() != 1 {
				t.Fatalf("hits=%d calls=%d dials=%d", len(results), calls.Load(), dials.Load())
			}
			for _, r := range results {
				if r.URL != "https://example.com/path" || len(r.Title) > 512 || len(r.Snippet) > 2048 || !utf8.ValidString(r.Title+r.Snippet) || r.Snippet == "" {
					t.Fatalf("bad bounded result %#v", r)
				}
			}
		})
	}
	cases := []struct {
		name, body, code string
		status           int
	}{
		{"empty", `{"results":[]}`, "", 200},
		{"brave_empty", `{"web":{"results":[]}}`, "", 200},
		{"rate_limit", "fixture-secret", "SEARCH_PROVIDER_RATE_LIMIT", 429},
		{"auth", "fixture-secret", "SEARCH_PROVIDER_FAILURE", 401},
		{"server", "fixture-secret", "SEARCH_PROVIDER_FAILURE", 503},
		{"redirect", "", "SEARCH_PROVIDER_FAILURE", 302},
		{"bad_json", `{"results":`, "SEARCH_RESPONSE_INVALID", 200},
		{"wrong_type", `{"results":"secret"}`, "SEARCH_RESPONSE_INVALID", 200},
		{"null", `null`, "SEARCH_RESPONSE_INVALID", 200},
		{"trailing_json", `{"results":[]} {}`, "SEARCH_RESPONSE_INVALID", 200},
		{"large", strings.Repeat("x", searchResponseLimit+1), "SEARCH_RESPONSE_TOO_LARGE", 200},
		{"chunked_large", strings.Repeat("x", searchResponseLimit+1), "SEARCH_RESPONSE_TOO_LARGE", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			_, n, dials := b6TLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if tc.name == "redirect" {
					w.Header().Set("Location", trap.URL+"/fixture-secret")
				}
				if tc.name == "large" {
					w.Header().Set("Content-Length", "262145")
				}
				w.WriteHeader(tc.status)
				if tc.name == "chunked_large" {
					w.(http.Flusher).Flush()
				}
				io.WriteString(w, tc.body)
			}))
			source := "tavily"
			if tc.name == "brave_empty" {
				source = "brave-free"
			}
			results, err := searchProviderCall(t.Context(), source, "fixture-secret", "query", proxyStored{}, &n)
			b6Code(t, err, tc.code)
			if tc.code == "" && (results == nil || len(results) != 0) {
				t.Fatal("empty response must be []")
			}
			if calls.Load() != 1 || dials.Load() != 1 {
				t.Fatalf("retry calls=%d dials=%d", calls.Load(), dials.Load())
			}
		})
	}
	for _, mode := range []string{"cancel", "timeout", "body_cancel", "hard_timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "timeout" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 50*time.Millisecond)
				defer stop()
			}
			release := make(chan struct{})
			defer close(release)
			_, n, _ := b6TLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if mode == "body_cancel" {
					io.WriteString(w, `{"results":[`)
					w.(http.Flusher).Flush()
				}
				if mode == "cancel" || mode == "body_cancel" {
					cancel()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			start := time.Now()
			_, err := searchProviderCall(ctx, "tavily", "fixture-secret", "q", proxyStored{}, &n)
			want := "SEARCH_PROVIDER_CANCELLED"
			if mode == "timeout" || mode == "hard_timeout" {
				want = "SEARCH_PROVIDER_TIMEOUT"
			}
			b6Code(t, err, want)
			if time.Since(start) > 12*time.Second {
				t.Fatal("deadline exceeded wall bound")
			}
		})
	}
	t.Run("untrusted_tls", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
		defer srv.Close()
		n := modelNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}, dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
		}}
		_, err := searchProviderCall(t.Context(), "tavily", "fixture-secret", "q", proxyStored{}, &n)
		b6Code(t, err, "SEARCH_PROVIDER_FAILURE")
		if calls.Load() != 0 {
			t.Fatal("TLS verification bypassed")
		}
	})
	t.Run("no_retry_disconnect", func(t *testing.T) {
		var calls atomic.Int32
		_, n, dials := b6TLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			c, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			c.Close()
		}))
		_, err := searchProviderCall(t.Context(), "brave-free", "fixture-secret", "q", proxyStored{}, &n)
		b6Code(t, err, "SEARCH_PROVIDER_FAILURE")
		if calls.Load() != 1 || dials.Load() != 1 {
			t.Fatal("retried disconnect")
		}
	})
	t.Run("http_proxy_rejection", func(t *testing.T) {
		var calls atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Method != "CONNECT" || r.Host != "8.8.8.8:443" || r.Header.Get("Proxy-Authorization") == "" {
				t.Error("CONNECT protocol")
			}
			w.WriteHeader(407)
		}))
		defer proxy.Close()
		n := modelNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}, dial: func(ctx context.Context, _, address string) (net.Conn, error) {
			if address != strings.TrimPrefix(proxy.URL, "http://") {
				t.Error("wrong proxy")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(proxy.URL, "http://"))
		}}
		_, err := searchProviderCall(t.Context(), "tavily", "fixture-secret", "q", proxyStored{Enabled: true, URL: proxy.URL, Username: "fixture-proxy-user", Password: "fixture-proxy-pass"}, &n)
		b6Code(t, err, "SEARCH_PROVIDER_FAILURE")
		if calls.Load() != 1 {
			t.Fatal("CONNECT retried")
		}
	})
	if envHits.Load() != 0 {
		t.Fatalf("environment proxy or redirect reached: %d", envHits.Load())
	}
}

func TestB6ProviderPreflightAndURLs(t *testing.T) {
	for _, source := range []string{"", "ddgs", "deepseek", "brave", "TAVILY", "unexpected"} {
		_, err := searchProviderCall(t.Context(), source, "fixture-secret", "q", proxyStored{}, &modelNetwork{})
		b6Code(t, err, "SEARCH_SOURCE_UNSUPPORTED")
	}
	_, err := searchProviderCall(t.Context(), "tavily", "fixture-secret", "q", proxyStored{}, &modelNetwork{})
	b6Code(t, err, "SEARCH_TRANSPORT_UNAVAILABLE")
	_, err = searchProviderCall(t.Context(), "tavily", "fixture-secret", "q", proxyStored{Enabled: true, URL: "https://bad:443"}, nil)
	b6Code(t, err, "SEARCH_TRANSPORT_UNAVAILABLE")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = searchProviderCall(ctx, "tavily", "fixture-secret", "q", proxyStored{}, nil)
	b6Code(t, err, "SEARCH_PROVIDER_CANCELLED")
	for _, u := range []string{"http://192.168.1.1/", "https://100.64.0.1/", "http://169.254.169.254/", "http://[fc00::1]/", "http://[::ffff:127.0.0.1]/", "http://192.0.2.1/", "http://printer.lan/", "http://metadata.google.internal/", "http://example.com:0/", "https://example.com/%7f", "https://example.com/\n", "https://0x7f000001/", "https://127.0.0.01/", "https://example.com:65536/", "https://example.com:/", "https://a..com/", "https://user@example.com/"} {
		if searchPublicURL(u) {
			t.Errorf("accepted unsafe URL %q", u)
		}
	}
	for _, u := range []string{"https://example.com/path?q=a", "http://8.8.8.8/", "https://[2606:4700:4700::1111]/"} {
		if !searchPublicURL(u) {
			t.Errorf("rejected public URL %q", u)
		}
	}
}
