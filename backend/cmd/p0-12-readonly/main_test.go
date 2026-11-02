package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestApprovedStateBinding(t *testing.T) {
	for _, tc := range []struct {
		name, instances string
		valid           bool
	}{
		{"approved", `[{"label":"36374700","webui_port":6211,"onebot_port":3211,"onebot_token":"fixture-secret"}]`, true},
		{"wrong_account", `[{"label":"other","webui_port":6211,"onebot_port":3211,"onebot_token":"fixture-secret"}]`, false},
		{"wrong_port", `[{"label":"36374700","webui_port":6210,"onebot_port":3210,"onebot_token":"fixture-secret"}]`, false},
		{"empty_token", `[{"label":"36374700","webui_port":6211,"onebot_port":3211}]`, false},
		{"ambiguous", `[{"label":"36374700","webui_port":6211,"onebot_port":3211,"onebot_token":"fixture-secret"},{"label":"36374700","webui_port":6211,"onebot_port":3211,"onebot_token":"fixture-secret"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(p, []byte(`{"version":1,"instances":`+tc.instances+`}`), 0600); err != nil {
				t.Fatal(err)
			}
			endpoint, token, err := loadOneBotConfig(p)
			if tc.valid {
				if err != nil || endpoint != "http://127.0.0.1:3211" || token != "fixture-secret" {
					t.Fatal("approved state binding was not selected")
				}
			} else if err == nil {
				t.Fatal("unapproved binding accepted")
			}
		})
	}
}

func TestP012NonJSON5xxRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "upstream unavailable: retry me")
			return
		}
		fmt.Fprint(w, `{"status":"ok","retcode":0,"data":{"user_id":36374700}}`)
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	dir := t.TempDir()
	r := &runner{ctx: context.Background(), endpoint: endpoint, client: loopbackClient(), evidence: &evidence{dir: dir}}
	if _, err := r.oneBotCall("login", allowedActionLogin, map[string]any{}); err != nil {
		t.Fatalf("plaintext 5xx must take the single retry: %v", err)
	}
	if calls != 2 || !r.retryUsed {
		t.Fatalf("calls=%d retryUsed=%v", calls, r.retryUsed)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	if err != nil || len(data) == 0 {
		t.Fatalf("evidence missing after non-JSON 5xx: %v", err)
	}
	var records []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("evidence line is not valid JSON: %v: %s", err, scanner.Text())
		}
		records = append(records, record)
	}
	if len(records) != 2 || records[0]["response_status"] != float64(503) {
		t.Fatalf("records=%v", records)
	}
	if text, _ := records[0]["response_text"].(string); text != "upstream unavailable: retry me" {
		t.Fatalf("plaintext response not preserved: %v", records[0])
	}
}

// The evidence file must contain each step before the next response is
// consumed, so a crash between steps still leaves the completed step on disk.
func TestP012ImmediateEvidence(t *testing.T) {
	dir := t.TempDir()
	counts := make([]int, 0, 3)
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
		lines := 0
		if err == nil {
			lines = bytes.Count(data, []byte("\n"))
		}
		mu.Lock()
		counts = append(counts, lines)
		mu.Unlock()
		fmt.Fprint(w, `{"status":"ok","retcode":0,"data":{}}`)
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	r := &runner{ctx: context.Background(), endpoint: endpoint, client: loopbackClient(), evidence: &evidence{dir: dir}}
	for _, action := range []string{allowedActionLogin, allowedActionStatus, allowedActionGroups} {
		if _, err := r.oneBotCall(action, action, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(counts) != 3 || counts[0] != 0 || counts[1] != 1 || counts[2] != 2 {
		t.Fatalf("persisted lines observed before each response=%v want [0 1 2]", counts)
	}
}

// Contract: design 03 §10 requires per-attempt evidence. A retry must not
// reuse the previous attempt's response fields.
func TestP012RetryEvidenceIsolation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "first attempt failure")
			return
		}
		fmt.Fprint(w, `{"status":"ok","retcode":0,"data":{}}`)
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	dir := t.TempDir()
	r := &runner{ctx: context.Background(), endpoint: endpoint, client: loopbackClient(), evidence: &evidence{dir: dir}}
	if _, err := r.oneBotCall("login", allowedActionLogin, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("attempt lines=%d want 2", len(lines))
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if second["response_status"] != float64(http.StatusOK) {
		t.Fatalf("second attempt status=%v", second["response_status"])
	}
	if _, leaked := second["response_text"]; leaked {
		t.Fatalf("second attempt carries the first attempt response_text: %v", second["response_text"])
	}
}

// Round 5 regression restored verbatim: one retry is allowed across the whole
// read chain, not once per action.
func TestSingleRetryAcrossReadChain(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			fmt.Fprint(w, `{"status":"ok","retcode":0,"data":{}}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status":"failed","retcode":1,"data":{}}`)
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	dir := t.TempDir()
	r := &runner{ctx: context.Background(), endpoint: endpoint, client: loopbackClient(), evidence: &evidence{dir: dir}}
	if _, err := r.oneBotCall("login", allowedActionLogin, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.oneBotCall("status", allowedActionStatus, map[string]any{}); err == nil {
		t.Fatal("503 accepted")
	}
	if calls != 3 {
		t.Fatalf("requests=%d; one retry allowed across whole chain", calls)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	if err != nil || len(data) == 0 {
		t.Fatal("step evidence was not persisted before next step")
	}
}
