package identity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func readerBinding(runtimeID string) Binding {
	binding := bindingFor("a")
	binding.RuntimeID = runtimeID
	return binding
}

func TestHTTPLoginInfoReaderOnlyPostsAuthenticatedGetLoginInfo(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&requests, 1)
		if request.Method != http.MethodPost || request.URL.Path != "/get_login_info" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.URL.RawQuery != "" {
			t.Errorf("credential/query leaked into URL: %s", request.URL.RawQuery)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer secret-test-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ok","retcode":0,"data":{"user_id":10001,"nickname":"fixture"}}`)
	}))
	defer server.Close()

	reader, err := NewHTTPLoginInfoReader("runtime-a", server.URL, "secret-test-token")
	if err != nil {
		t.Fatal(err)
	}
	info, err := reader.ReadLoginInfo(context.Background(), readerBinding("runtime-a"))
	if err != nil || info.SelfID != "10001" || info.Source != SourceGetLoginInfo {
		t.Fatalf("read info = %+v, %v", info, err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
	if _, err := reader.ReadLoginInfo(context.Background(), readerBinding("runtime-b")); !hasCode(err, ErrRuntimeUnavailable) {
		t.Fatalf("wrong runtime error = %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("wrong runtime binding reached the provider: requests = %d", got)
	}
}

func TestHTTPLoginInfoReaderDecodesIDWithoutFloatConversion(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantID string
	}{
		{name: "string user id", body: `{"status":"ok","retcode":0,"data":{"user_id":"10002"}}`, wantID: "10002"},
		{name: "integer beyond float64 precision", body: `{"status":"ok","retcode":0,"data":{"user_id":9007199254740993}}`, wantID: "9007199254740993"},
		{name: "integer beyond int64 range", body: `{"status":"ok","retcode":0,"data":{"user_id":123456789012345678901234567890}}`, wantID: "123456789012345678901234567890"},
		{name: "missing id", body: `{"status":"ok","retcode":0,"data":{"nickname":"fixture"}}`},
		{name: "failed action", body: `{"status":"failed","retcode":100,"data":{"user_id":10002}}`},
		{name: "zero id", body: `{"status":"ok","retcode":0,"data":{"user_id":0}}`},
		{name: "fractional id", body: `{"status":"ok","retcode":0,"data":{"user_id":10002.5}}`},
		{name: "malformed envelope", body: `{"status":"ok","retcode":0,"data":{"user_id":10002}`},
		{name: "trailing json value", body: `{"status":"ok","retcode":0,"data":{"user_id":10002}} {"status":"ok"}`},
		{name: "trailing garbage", body: "{\"status\":\"ok\",\"retcode\":0,\"data\":{\"user_id\":10002}}garbage"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			reader, err := NewHTTPLoginInfoReader("runtime-a", server.URL, "token")
			if err != nil {
				t.Fatal(err)
			}
			info, err := reader.ReadLoginInfo(context.Background(), readerBinding("runtime-a"))
			if test.wantID == "" {
				if !hasCode(err, ErrIdentityUnverified) {
					t.Fatalf("invalid identity response = %+v, %v", info, err)
				}
				if info != (LoginInfo{}) {
					t.Fatalf("rejected response still produced identity %+v", info)
				}
				return
			}
			if err != nil || info.SelfID != test.wantID || info.Source != SourceGetLoginInfo {
				t.Fatalf("read info = %+v, %v; want self id %q", info, err, test.wantID)
			}
		})
	}
}

func TestHTTPLoginInfoReaderRejectsOversizedResponse(t *testing.T) {
	body := `{"status":"ok","retcode":0,"data":{"user_id":10001,"nickname":"` + strings.Repeat("a", maxLoginInfoResponseBytes) + `"}}`
	if len(body) <= maxLoginInfoResponseBytes {
		t.Fatalf("fixture is not oversized: %d bytes", len(body))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}))
	defer server.Close()

	reader, err := NewHTTPLoginInfoReader("runtime-a", server.URL, "oversize-token")
	if err != nil {
		t.Fatal(err)
	}
	info, err := reader.ReadLoginInfo(context.Background(), readerBinding("runtime-a"))
	if !hasCode(err, ErrIdentityUnverified) {
		t.Fatalf("oversized response error = %v", err)
	}
	if info != (LoginInfo{}) {
		t.Fatalf("oversized response still produced identity %+v", info)
	}
	if message := err.Error(); strings.Contains(message, "aaaa") || strings.Contains(message, "oversize-token") {
		t.Fatalf("error leaked body or credential: %v", err)
	}
}

func TestHTTPLoginInfoReaderNeverFollowsRedirectsOrForwardsCredentials(t *testing.T) {
	var originRequests, targetRequests int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&targetRequests, 1)
		if got := request.Header.Get("Authorization"); got != "" {
			t.Errorf("credentials forwarded to redirect target: %q", got)
		}
		_, _ = fmt.Fprint(w, `{"status":"ok","retcode":0,"data":{"user_id":10001}}`)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&originRequests, 1)
		if got := request.Header.Get("Authorization"); got != "Bearer redirect-token" {
			t.Errorf("origin authorization = %q", got)
		}
		http.Redirect(w, request, target.URL+"/get_login_info", http.StatusFound)
	}))
	defer origin.Close()

	reader, err := NewHTTPLoginInfoReader("runtime-a", origin.URL, "redirect-token")
	if err != nil {
		t.Fatal(err)
	}
	info, err := reader.ReadLoginInfo(context.Background(), readerBinding("runtime-a"))
	if !hasCode(err, ErrRuntimeUnavailable) {
		t.Fatalf("redirected response error = %v", err)
	}
	if info != (LoginInfo{}) {
		t.Fatalf("redirected response still produced identity %+v", info)
	}
	if got := atomic.LoadInt32(&originRequests); got != 1 {
		t.Fatalf("origin requests = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&targetRequests); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
}

func TestHTTPLoginInfoReaderRequiresLoopbackOrigin(t *testing.T) {
	for _, endpoint := range []string{
		"http://example.com:3001",
		"http://192.0.2.1:3001",
		"http://127.0.0.1:3001/api",
		"http://127.0.0.1:3001?access_token=secret",
		"http://user:pass@127.0.0.1:3001",
		"http://127.0.0.1:3001#fragment",
		"ftp://127.0.0.1:3001",
		"",
	} {
		if _, err := NewHTTPLoginInfoReader("runtime-a", endpoint, "token"); err == nil {
			t.Errorf("accepted unsafe endpoint %q", endpoint)
		}
	}
	if _, err := NewHTTPLoginInfoReader("", "http://127.0.0.1:3001", "token"); err == nil {
		t.Fatal("accepted blank runtime id")
	}
	if _, err := NewHTTPLoginInfoReader("runtime-a", "http://127.0.0.1:3001", "  "); err == nil {
		t.Fatal("accepted blank access token")
	}
	if _, err := NewHTTPLoginInfoReader("runtime-a", "https://localhost:3001", "token"); err != nil {
		t.Fatalf("localhost endpoint rejected: %v", err)
	}
	if _, err := NewHTTPLoginInfoReader("runtime-a", "http://[::1]:3001", "token"); err != nil {
		t.Fatalf("IPv6 loopback endpoint rejected: %v", err)
	}
}

func TestHTTPLoginInfoReaderDoesNotLeakCredentialInError(t *testing.T) {
	const token = "dont-log-this-token"
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&requests, 1)
		if got := request.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("authorization = %q", got)
		}
		http.Error(w, "provider secret detail", http.StatusForbidden)
	}))
	defer server.Close()
	reader, err := NewHTTPLoginInfoReader("runtime-a", server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	info, err := reader.ReadLoginInfo(context.Background(), readerBinding("runtime-a"))
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("provider request was not exercised: requests = %d", got)
	}
	if !hasCode(err, ErrRuntimeUnavailable) {
		t.Fatalf("forbidden response error = %v", err)
	}
	if info != (LoginInfo{}) {
		t.Fatalf("forbidden response still produced identity %+v", info)
	}
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "provider secret detail") {
		t.Fatalf("unsafe error content: %v", err)
	}
}

func TestHTTPLoginInfoReaderDoesNotLeakCredentialOnDialFailure(t *testing.T) {
	const token = "dont-log-this-token"
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close()

	reader, err := NewHTTPLoginInfoReader("runtime-a", endpoint, token)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reader.ReadLoginInfo(context.Background(), readerBinding("runtime-a"))
	if !hasCode(err, ErrRuntimeUnavailable) {
		t.Fatalf("unreachable provider error = %v", err)
	}
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), endpoint) {
		t.Fatalf("unsafe error content: %v", err)
	}
}
