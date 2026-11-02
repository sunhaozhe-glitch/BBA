package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"bba.local/taskengine/internal/taskengine"
)

func p013TestRunner(t *testing.T, endpoint string) *runner {
	t.Helper()
	return &runner{ctx: context.Background(), client: loopbackClient(), onebot: endpoint, webui: endpoint, evidence: &evidence{dir: t.TempDir()}}
}

func TestP013WebUIAuthEvidenceDoesNotContainSecrets(t *testing.T) {
	const credential = "credential-must-not-persist"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/login" || r.Method != http.MethodPost {
			t.Fatalf("unexpected auth request: %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"code":0,"data":{"Credential":"`+credential+`"}}`)
	}))
	defer server.Close()

	token := "webui-secret"
	hash := sha256.Sum256([]byte(token + ".napcat"))
	expectedHash := hex.EncodeToString(hash[:])
	r := p013TestRunner(t, server.URL)
	r.webuiToken = token
	if got, err := r.webuiCredential(); err != nil || got != credential {
		t.Fatalf("credential=%q err=%v", got, err)
	}
	body, err := os.ReadFile(filepath.Join(r.evidence.dir, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), credential) || strings.Contains(string(body), expectedHash) {
		t.Fatalf("WebUI auth secret persisted in evidence: %s", body)
	}
}

func TestP013GroupIDsRejectsMixedOrIncompleteRecords(t *testing.T) {
	valid := `[{"group_id":36374700,"group_name":"target","member_count":1,"max_member_count":2}]`
	if got, err := groupIDs(json.RawMessage(valid)); err != nil || len(got) != 1 || got[0] != "36374700" {
		t.Fatalf("valid group list=%v err=%v", got, err)
	}
	for name, raw := range map[string]string{
		"mixed_key":      `[{"group_id":36374700,"group_name":"target","member_count":1,"max_member_count":2},{"groupCode":36374701,"group_name":"mixed","member_count":1,"max_member_count":2}]`,
		"missing_name":   `[{"group_id":36374700,"member_count":1,"max_member_count":2}]`,
		"missing_counts": `[{"group_id":36374700,"group_name":"target"}]`,
		"string_id":      `[{"group_id":"36374700","group_name":"target","member_count":1,"max_member_count":2}]`,
		"null_id":        `[{"group_id":null,"group_name":"target","member_count":1,"max_member_count":2}]`,
		"fractional_id":  `[{"group_id":36374700.5,"group_name":"target","member_count":1,"max_member_count":2}]`,
		"null_list":      `null`,
		"trailing":       `[] []`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := groupIDs(json.RawMessage(raw)); err == nil {
				t.Fatalf("accepted malformed list=%v", got)
			}
		})
	}
}

func TestP013StatusGoodRequiresBooleanTrue(t *testing.T) {
	for name, raw := range map[string]string{
		"missing": `{"online":true}`,
		"null":    `{"online":true,"good":null}`,
		"string":  `{"online":true,"good":"true"}`,
		"false":   `{"online":true,"good":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			status, err := parseStatus(json.RawMessage(raw))
			if err != nil {
				t.Fatal(err)
			}
			if err := validateStatus(status); err == nil {
				t.Fatalf("accepted status=%v", status)
			}
		})
	}
	status, err := parseStatus(json.RawMessage(`{"online":true,"good":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStatus(status); err != nil {
		t.Fatalf("rejected valid status: %v", err)
	}
}

func TestP013CleanerPreflightRequiresActivePluginEvidence(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		wantErr    bool
	}{
		{name: "403", statusCode: http.StatusForbidden, body: `{"code":-1}`, wantErr: true},
		{name: "404", statusCode: http.StatusNotFound, body: `{"code":-1}`, wantErr: true},
		{name: "500", statusCode: http.StatusInternalServerError, body: `{"code":-1}`, wantErr: true},
		{name: "manager_missing", statusCode: http.StatusOK, body: `{"code":0,"data":{"pluginManagerNotFound":true,"plugins":[]}}`, wantErr: true},
		{name: "plugin_stopped", statusCode: http.StatusOK, body: `{"code":0,"data":{"pluginManagerNotFound":false,"plugins":[{"id":"napcat-plugin-cleaner","status":"stopped"}]}}`, wantErr: true},
		{name: "empty_success", statusCode: http.StatusOK, body: `{}`, wantErr: true},
		{name: "missing_flag", statusCode: http.StatusOK, body: `{"code":0,"data":{"plugins":[{"id":"napcat-plugin-cleaner","status":"active"}]}}`, wantErr: true},
		{name: "wrong_plugin", statusCode: http.StatusOK, body: `{"code":0,"data":{"pluginManagerNotFound":false,"plugins":[{"id":"other","status":"active"}]}}`, wantErr: true},
		{name: "active", statusCode: http.StatusOK, body: `{"code":0,"data":{"pluginManagerNotFound":false,"plugins":[{"id":"napcat-plugin-cleaner","status":"active"}]}}`, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("preflight must be read-only: %s", r.Method)
				}
				w.WriteHeader(tc.statusCode)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			r := p013TestRunner(t, server.URL)
			err := r.pluginAvailable("credential")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestP013ReadRetryIsOnePerRun(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 || calls == 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"failed","retcode":1,"data":{}}`)
			return
		}
		fmt.Fprint(w, `{"status":"ok","retcode":0,"data":{}}`)
	}))
	defer server.Close()
	r := p013TestRunner(t, server.URL)
	if _, err := r.oneBot("login", "get_login_info", map[string]any{}); err != nil {
		t.Fatalf("first read should consume the one retry: %v", err)
	}
	if _, err := r.oneBot("status", "get_status", map[string]any{}); err == nil {
		t.Fatal("second read received an unauthorized retry")
	}
	if calls != 3 || r.retries != 1 {
		t.Fatalf("calls=%d retries=%d want calls=3 retries=1", calls, r.retries)
	}
}

func TestP013CleanerWriteNeverRetries(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `temporary failure`)
	}))
	defer server.Close()
	r := p013TestRunner(t, server.URL)
	adapter := &cleanerAdapter{runner: r, cred: "credential", groupID: "36374701", reason: applyText, answer: applyAnswer}
	outcome, err := adapter.RequestJoin(context.Background(), taskengine.Reservation{GroupID: adapter.groupID, JoinReason: adapter.reason, VerificationMessage: adapter.answer})
	if err == nil || outcome.Disposition != "unknown" || calls != 1 || r.retries != 0 {
		t.Fatalf("outcome=%+v err=%v calls=%d retries=%d", outcome, err, calls, r.retries)
	}
}

func TestP013CleanerJoinEvidenceOmitsReasonAndAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/Plugin/ext/napcat-plugin-cleaner/join-group" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"ok":true,"result":1}`)
	}))
	defer server.Close()
	r := p013TestRunner(t, server.URL)
	adapter := &cleanerAdapter{runner: r, cred: "credential", groupID: "829117089", reason: applyText, answer: applyAnswer}
	if _, err := adapter.RequestJoin(context.Background(), taskengine.Reservation{GroupID: adapter.groupID, JoinReason: adapter.reason, VerificationMessage: adapter.answer}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(r.evidence.dir, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, applyText) || strings.Contains(text, applyAnswer) {
		t.Fatalf("cleaner join secret persisted in evidence: %s", text)
	}
}

func TestP013TransportRetryIsSharedAcrossReads(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		call := calls.Add(1)
		if call == 1 || call == 3 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		fmt.Fprint(w, `{"status":"ok","retcode":0,"data":{}}`)
	}))
	defer server.Close()
	r := p013TestRunner(t, server.URL)
	if _, err := r.oneBot("login", "get_login_info", map[string]any{}); err != nil {
		t.Fatalf("first transport failure should be retried: %v", err)
	}
	if _, err := r.oneBot("status", "get_status", map[string]any{}); err == nil {
		t.Fatal("second transport failure was retried")
	}
	if calls.Load() != 3 || r.retries != 1 {
		t.Fatalf("calls=%d retries=%d want 3/1", calls.Load(), r.retries)
	}
}

func TestP013CleanerOnlySubmittedResultContinues(t *testing.T) {
	for _, result := range []string{"null", "0", "2", `"1"`, "1"} {
		t.Run(result, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				fmt.Fprintf(w, `{"ok":true,"result":%s,"raw":{"request_ref":"fixture-reference"}}`, result)
			}))
			defer server.Close()
			r := p013TestRunner(t, server.URL)
			adapter := &cleanerAdapter{runner: r, cred: "fixture", groupID: "829117089", reason: applyText, answer: applyAnswer}
			outcome, err := adapter.RequestJoin(context.Background(), taskengine.Reservation{GroupID: adapter.groupID, JoinReason: adapter.reason, VerificationMessage: adapter.answer})
			if result == "1" {
				if err != nil || outcome.Disposition != "submitted" || outcome.RequestRef != "fixture-reference" {
					t.Fatalf("outcome=%+v err=%v", outcome, err)
				}
			} else if err == nil || outcome.Disposition != "unknown" {
				t.Fatalf("unverified result continued: outcome=%+v err=%v", outcome, err)
			}
			if calls.Load() != 1 || r.retries != 0 {
				t.Fatalf("calls=%d retries=%d", calls.Load(), r.retries)
			}
		})
	}
}
