package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func boundedTaskConfig(now time.Time) executionConfig {
	return executionConfig{
		Version:                  1,
		WorkspaceID:              "ws_test",
		PrincipalID:              "principal_test",
		RuntimeID:                "runtime_test",
		AccountID:                "account_test",
		OwnerEpoch:               1,
		TargetGroupID:            "829117089",
		TaskTitle:                "bounded target",
		ApprovalExpiresAt:        &utcTime{Time: now.Add(30 * time.Minute)},
		TaskDeadlineAt:           &utcTime{Time: now.Add(time.Hour)},
		GoalCount:                1,
		MaxCandidates:            1,
		MaxAttemptsPerAction:     1,
		MaxTotalExternalAttempts: 1,
		MaxQQReadCalls:           7,
		TaskBridgeURL:            "http://127.0.0.1:18422",
		InstanceLabel:            "instance_test",
		JoinReason:               "owner approved",
		JoinAnswer:               "approved answer",
		EvidenceDir:              "/tmp/bba-evidence",
	}
}

func TestTaskConfigRequiresSingleBoundedScope(t *testing.T) {
	now := time.Now().UTC()
	for name, mutate := range map[string]func(*executionConfig){
		"missing target":              func(c *executionConfig) { c.TargetGroupID = "" },
		"goal count":                  func(c *executionConfig) { c.GoalCount = 2 },
		"candidate count":             func(c *executionConfig) { c.MaxCandidates = 2 },
		"zero attempts":               func(c *executionConfig) { c.MaxAttemptsPerAction = 0 },
		"per-action retries rejected": func(c *executionConfig) { c.MaxAttemptsPerAction = 2 },
		"total retries rejected":      func(c *executionConfig) { c.MaxTotalExternalAttempts = 2 },
		"read budget too small":       func(c *executionConfig) { c.MaxQQReadCalls = 6 },
		"read budget too large":       func(c *executionConfig) { c.MaxQQReadCalls = 31 },
		"missing bridge":              func(c *executionConfig) { c.TaskBridgeURL = "" },
		"missing join reason":         func(c *executionConfig) { c.JoinReason = "  " },
		"expired approval":            func(c *executionConfig) { c.ApprovalExpiresAt = &utcTime{Time: now.Add(-time.Minute)} },
		"approval past task":          func(c *executionConfig) { c.ApprovalExpiresAt = &utcTime{Time: now.Add(2 * time.Hour)} },
		"task deadline too far":       func(c *executionConfig) { c.TaskDeadlineAt = &utcTime{Time: now.Add(7 * time.Hour)} },
		"missing task title":          func(c *executionConfig) { c.TaskTitle = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := boundedTaskConfig(now)
			mutate(&cfg)
			if err := cfg.validateTaskConfig(now); err == nil {
				t.Fatalf("accepted invalid config: %+v", cfg)
			}
		})
	}
}

func TestTaskConfigAcceptsExactBound(t *testing.T) {
	now := time.Now().UTC()
	if err := boundedTaskConfig(now).validateTaskConfig(now); err != nil {
		t.Fatalf("rejected bounded config: %v", err)
	}
}

func TestConfigDigestConfirmation(t *testing.T) {
	digest := strings.Repeat("a", 64)
	if err := confirmConfigDigest(digest, digest); err != nil {
		t.Fatalf("rejected exact digest: %v", err)
	}
	for name, value := range map[string]string{
		"empty":     "",
		"uppercase": strings.Repeat("A", 64),
		"short":     strings.Repeat("a", 63),
		"nonhex":    strings.Repeat("z", 64),
		"suffix":    strings.Repeat("a", 65),
	} {
		t.Run(name, func(t *testing.T) {
			if err := confirmConfigDigest(digest, value); err == nil {
				t.Fatalf("accepted confirmation %q", value)
			}
		})
	}
}

func TestConfigFileMustBeOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadExecutionConfig(path); err == nil {
		t.Fatal("accepted a group/world-readable config")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadExecutionConfig(path); err == nil {
		t.Fatal("accepted a config that is not a complete execution document")
	}
}

func TestOneBotErrorDoesNotExposeServerMessage(t *testing.T) {
	_, err := decodeOneBotData([]byte(`{"status":"failed","retcode":100,"message":"private response detail","data":null}`))
	if err == nil {
		t.Fatal("accepted a failed OneBot response")
	}
	if strings.Contains(err.Error(), "private response detail") {
		t.Fatalf("error exposed server message: %v", err)
	}
}

func TestChatIdempotencyKeyIsStableAndBounded(t *testing.T) {
	first := chatIdempotencyKey("send_1", 0)
	if first != chatIdempotencyKey("send_1", 0) {
		t.Fatal("idempotency key is not stable")
	}
	if first == chatIdempotencyKey("send_1", 1) {
		t.Fatal("idempotency key does not include the segment")
	}
	if len(first) > 128 {
		t.Fatalf("idempotency key length=%d", len(first))
	}
}

func TestPreflightSummaryUsesExplicitTarget(t *testing.T) {
	pre := preflightResult{
		TargetGroupID: "1",
		Identity:      identityFacts{GroupIDs: []string{"1"}},
	}
	summary := preflightSummary(pre)
	observed, ok := summary["observed"].(map[string]any)
	if !ok {
		t.Fatal("missing observed summary")
	}
	if present, ok := observed["target_present"].(bool); !ok || !present {
		t.Fatalf("target_present=%v, want true", observed["target_present"])
	}
}

func TestPreflightSummaryOmitsIdentityAndTargetIdentifiers(t *testing.T) {
	pre := preflightResult{
		Account:       accountFacts{SelfID: "10001"},
		Identity:      identityFacts{SelfID: "10001", Nickname: "private nickname", GroupIDs: []string{"829117089"}},
		TargetGroupID: "829117089",
	}
	summary, err := json.Marshal(preflightSummary(pre))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"10001", "private nickname", "829117089"} {
		if strings.Contains(string(summary), value) {
			t.Fatalf("preflight summary exposed %q", value)
		}
	}
}

func TestTaskAccountRequiresPersistedHeartbeat(t *testing.T) {
	now := time.Now().UTC()
	cfg := boundedTaskConfig(now)
	facts := accountFacts{
		RuntimeID: cfg.RuntimeID, SelfID: "10001", IdentityStatus: "verified",
		ConnectionState: "online", LoginState: "logged_in", ControlState: "active", RiskState: "clear",
		IdentityGeneration: 1, AccountOwnerEpoch: cfg.OwnerEpoch, CredentialOwnerEpoch: cfg.OwnerEpoch,
		CredentialActive: true, MembershipAuthority: true, RequestJoinVerified: true,
		MembershipVerified: true, QuotaLimit: 1,
	}
	if blockers := validateAccount(cfg, facts, true); len(blockers) == 0 {
		t.Fatal("accepted a task account without a persisted heartbeat")
	}
	facts.HeartbeatAt = &now
	if blockers := validateAccount(cfg, facts, true); len(blockers) != 0 {
		t.Fatalf("rejected a task account with a persisted heartbeat: %v", blockers)
	}
}

func TestTaskBridgeClientUsesAuthenticatedLoopbackAndUnknownIsTerminal(t *testing.T) {
	const token = "fixture-runtime-token"
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":{"code":"UNAUTHENTICATED"}}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/internal/bridge/identity" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"self_id":"10001","nickname":"fixture","online":true,"good":true,"group_ids":["424242"]}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/gateway-send") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"EXTERNAL_OUTCOME_UNKNOWN"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := newTaskBridgeClient(server.URL, token, 3, nil)
	if err == nil {
		identity, err := client.identity(context.Background(), true)
		if err != nil || identity.SelfID != "10001" || len(identity.GroupIDs) != 1 || identity.GroupIDs[0] != "424242" {
			t.Fatalf("identity=%+v err=%v", identity, err)
		}
		if _, sent, err := client.sendChatText(context.Background(), "permit_fixture", "send_fixture:0", 0, "fixture text"); sent || err == nil || !strings.Contains(err.Error(), "EXTERNAL_OUTCOME_UNKNOWN") {
			t.Fatalf("unknown send sent=%v err=%v", sent, err)
		}
	} else {
		t.Fatalf("new Task Bridge client: %v", err)
	}
	if requests != 2 {
		t.Fatalf("fake Bridge requests=%d, want 2", requests)
	}
}
