package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBindingsRequiresFourUniquePortsAndIDs(t *testing.T) {
	got, err := parseBindings("3212:443952741,3210:3256395185,3213:454755768,3211:36374700")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Port != 3210 || got[3].Port != 3213 {
		t.Fatalf("bindings not sorted by port: %+v", got)
	}
	for name, raw := range map[string]string{
		"three":          "3210:3256395185,3211:36374700,3212:443952741",
		"duplicate_port": "3210:3256395185,3210:36374700,3212:443952741,3213:454755768",
		"duplicate_id":   "3210:3256395185,3211:3256395185,3212:443952741,3213:454755768",
		"string_id":      "3210:abc,3211:36374700,3212:443952741,3213:454755768",
		"bad_port":       "0:3256395185,3211:36374700,3212:443952741,3213:454755768",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseBindings(raw); err == nil {
				t.Fatalf("accepted invalid bindings %q", raw)
			}
		})
	}
}

func TestSelectInstancesUsesPortNotLabel(t *testing.T) {
	instances := []stateInstance{
		{ID: "wrong-label", Label: "443952741", OneBotPort: 3210, OneBotToken: "token-3210"},
		{ID: "right-label", Label: "36374700", OneBotPort: 3211, OneBotToken: "token-3211"},
	}
	selected, err := selectInstances(instances, []binding{{Port: 3210, SelfID: "3256395185"}, {Port: 3211, SelfID: "36374700"}})
	if err != nil {
		t.Fatal(err)
	}
	if selected[0].InstanceID != "wrong-label" || selected[0].Token != "token-3210" {
		t.Fatalf("selection used a label instead of the port: %+v", selected[0])
	}
	if _, err := selectInstances(instances, []binding{{Port: 3212, SelfID: "443952741"}}); err == nil {
		t.Fatal("accepted a missing port")
	}
}

func TestGroupListAcceptsEmptyAndRejectsIncomplete(t *testing.T) {
	if count, _, err := groupList(json.RawMessage(`[]`)); err != nil || count != 0 {
		t.Fatalf("empty list count=%d err=%v", count, err)
	}
	valid := `[{"group_id":1106856963,"group_name":"group","member_count":1,"max_member_count":2}]`
	if count, digest, err := groupList(json.RawMessage(valid)); err != nil || count != 1 || len(digest) != 64 {
		t.Fatalf("valid list count=%d digest=%q err=%v", count, digest, err)
	}
	for name, raw := range map[string]string{
		"null":           `null`,
		"string_id":      `[{"group_id":"1106856963","group_name":"group","member_count":1,"max_member_count":2}]`,
		"missing_name":   `[{"group_id":1106856963,"member_count":1,"max_member_count":2}]`,
		"missing_counts": `[{"group_id":1106856963,"group_name":"group"}]`,
		"alternate":      `[{"group_id":1106856963,"group_name":"group","member_count":1,"max_member_count":2,"groupCode":1106856963}]`,
		"duplicate":      `[{"group_id":1106856963,"group_name":"a","member_count":1,"max_member_count":2},{"group_id":1106856963,"group_name":"b","member_count":1,"max_member_count":2}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := groupList(json.RawMessage(raw)); err == nil {
				t.Fatalf("accepted invalid group list %s", raw)
			}
		})
	}
}

func TestEvidenceDoesNotPersistBearerToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get_login_info" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Fatalf("missing bearer")
		}
		fmt.Fprint(w, `{"status":"ok","retcode":0,"data":{"user_id":123456,"nickname":"nick"}}`)
	}))
	defer server.Close()

	dir := t.TempDir()
	r := &runner{ctx: context.Background(), client: server.Client(), evidence: &evidence{dir: dir}}
	raw, err := r.do("3211/123456", "get_login_info", http.MethodPost, server.URL+"/get_login_info", map[string]any{}, "secret-token", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeEnvelope(raw); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "secret-token") || strings.Contains(string(body), "Bearer") {
		t.Fatalf("bearer persisted in evidence: %s", body)
	}
	if !strings.Contains(string(body), `"nickname":"nick"`) {
		t.Fatalf("response body missing from evidence: %s", body)
	}
}

func TestUniqueIDsRejectsDuplicateObservedIdentity(t *testing.T) {
	if got := uniqueIDs([]accountResult{{ObservedSelfID: "1"}, {ObservedSelfID: "1"}}); got != 1 {
		t.Fatalf("unique IDs=%d", got)
	}
	if got := uniqueIDs([]accountResult{{ObservedSelfID: "1"}, {ObservedSelfID: "2"}}); got != 2 {
		t.Fatalf("unique IDs=%d", got)
	}
}

func TestOneBotRejectsWritesBeforeHTTPRequest(t *testing.T) {
	r := &runner{ctx: context.Background(), client: http.DefaultClient, evidence: &evidence{dir: t.TempDir()}}
	for _, action := range []string{"send_group_msg", "set_group_add_request", "get_status/../send_group_msg", ""} {
		if _, err := r.oneBot("account", 1, "secret", "write", action); err == nil {
			t.Fatalf("accepted action %q", action)
		}
	}
	if r.calls != 0 || len(r.evidence.records) != 0 {
		t.Fatalf("rejected actions reached transport: calls=%d records=%d", r.calls, len(r.evidence.records))
	}
}

func TestEngineSmokeRejectsUnsafeEnvironmentBeforeReadingState(t *testing.T) {
	t.Setenv("P014_EVIDENCE_DIR", t.TempDir())
	t.Setenv("P014_ENGINE_SMOKE", "1")
	t.Setenv("P014_STATE_FILE", filepath.Join(t.TempDir(), "missing-state.json"))
	t.Setenv("P014_BINDINGS", "3210:443952741,3211:36374700,3212:3256395185,3213:454755768")
	t.Setenv("BBA_FIXTURE_ONLY", "0")
	t.Setenv("BBA_FIXTURE_DSN", "postgres://bba_fixture@/bba_fixture_admin?host=/fixture&sslmode=disable")
	if err := run(); err == nil || !strings.Contains(err.Error(), "engine smoke preflight") {
		t.Fatalf("unsafe engine environment reached state read: %v", err)
	}
}

func TestEngineSmokePreflightAcceptsFixtureBeforeNapcatStateRead(t *testing.T) {
	t.Setenv("P014_EVIDENCE_DIR", t.TempDir())
	t.Setenv("P014_ENGINE_SMOKE", "1")
	t.Setenv("P014_STATE_FILE", filepath.Join(t.TempDir(), "missing-state.json"))
	t.Setenv("P014_BINDINGS", "3210:443952741,3211:36374700,3212:3256395185,3213:454755768")
	t.Setenv("BBA_FIXTURE_ONLY", "1")
	t.Setenv("BBA_FIXTURE_DSN", "postgres://bba_fixture@/bba_fixture_admin?host=/fixture&sslmode=disable")
	err := run()
	if err == nil || !strings.Contains(err.Error(), "cannot read napcatd state") {
		t.Fatalf("expected failure at read-only state-file loading after fixture preflight, got %v", err)
	}
}
