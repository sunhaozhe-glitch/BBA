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

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
	"github.com/jackc/pgx/v5"
)

func engineTestAccounts() []accountResult {
	return []accountResult{
		{OneBotPort: 3210, InstanceID: "qq-instance-3210", ExpectedSelfID: "443952741", ObservedSelfID: "443952741", Online: true, Good: true, GroupCount: 12},
		{OneBotPort: 3211, InstanceID: "qq-instance-3211", ExpectedSelfID: "36374700", ObservedSelfID: "36374700", Online: true, Good: true, GroupCount: 52},
		{OneBotPort: 3212, InstanceID: "qq-instance-3212", ExpectedSelfID: "3256395185", ObservedSelfID: "3256395185", Online: true, Good: true, GroupCount: 0},
		{OneBotPort: 3213, InstanceID: "qq-instance-3213", ExpectedSelfID: "454755768", ObservedSelfID: "454755768", Online: true, Good: true, GroupCount: 31},
	}
}

func cloneEngineTestAccounts() []accountResult {
	return append([]accountResult(nil), engineTestAccounts()...)
}

func TestValidateEngineSmokeEnvRestrictsFixture(t *testing.T) {
	const valid = "postgres://bba_fixture@/bba_fixture_admin?host=/fixture&sslmode=disable"
	for name, test := range map[string]struct {
		only bool
		dsn  string
		ok   bool
	}{
		"valid":               {only: true, dsn: valid, ok: true},
		"missing_only":        {only: false, dsn: valid},
		"missing_dsn":         {only: true, dsn: ""},
		"tcp_host":            {only: true, dsn: "postgres://bba_fixture@127.0.0.1:5432/bba_fixture_admin?sslmode=disable"},
		"fallback_tcp":        {only: true, dsn: "postgres://bba_fixture@/bba_fixture_admin?host=/fixture,127.0.0.1&sslmode=disable"},
		"port_override":       {only: true, dsn: "postgres://bba_fixture@/bba_fixture_admin?host=/fixture&port=6432&sslmode=disable"},
		"password":            {only: true, dsn: "postgres://bba_fixture:secret@/bba_fixture_admin?host=/fixture&sslmode=disable"},
		"wrong_user":          {only: true, dsn: "postgres://someone@/bba_fixture_admin?host=/fixture&sslmode=disable"},
		"wrong_database":      {only: true, dsn: "postgres://bba_fixture@/other?host=/fixture&sslmode=disable"},
		"production_database": {only: true, dsn: "postgres://bba_fixture@/bba_production?host=/fixture&sslmode=disable"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("BBA_FIXTURE_ONLY", map[bool]string{true: "1", false: ""}[test.only])
			t.Setenv("BBA_FIXTURE_DSN", test.dsn)
			err := validateEngineSmokeEnv()
			if test.ok && err != nil {
				t.Fatalf("valid fixture rejected: %v", err)
			}
			if !test.ok && err == nil {
				t.Fatal("unsafe fixture accepted")
			}
		})
	}
}

func TestValidateEngineAccountsRequiresFourVerifiedMappings(t *testing.T) {
	if err := validateEngineAccounts(engineTestAccounts()); err != nil {
		t.Fatalf("valid live inputs rejected: %v", err)
	}
	cases := map[string]func([]accountResult) []accountResult{
		"three": func(accounts []accountResult) []accountResult {
			return accounts[:3]
		},
		"duplicate_port": func(accounts []accountResult) []accountResult {
			accounts[3].OneBotPort = accounts[0].OneBotPort
			return accounts
		},
		"duplicate_instance": func(accounts []accountResult) []accountResult {
			accounts[3].InstanceID = accounts[0].InstanceID
			return accounts
		},
		"duplicate_self_id": func(accounts []accountResult) []accountResult {
			accounts[3].ExpectedSelfID = accounts[0].ExpectedSelfID
			accounts[3].ObservedSelfID = accounts[0].ExpectedSelfID
			return accounts
		},
		"offline": func(accounts []accountResult) []accountResult {
			accounts[2].Online = false
			return accounts
		},
		"not_good": func(accounts []accountResult) []accountResult {
			accounts[2].Good = false
			return accounts
		},
		"identity_mismatch": func(accounts []accountResult) []accountResult {
			accounts[1].ObservedSelfID = "100000001"
			return accounts
		},
		"identity_invalid": func(accounts []accountResult) []accountResult {
			accounts[1].ExpectedSelfID = "abc"
			accounts[1].ObservedSelfID = "abc"
			return accounts
		},
		"blank_instance": func(accounts []accountResult) []accountResult {
			accounts[1].InstanceID = "  "
			return accounts
		},
		"read_error": func(accounts []accountResult) []accountResult {
			accounts[0].Error = "transport failed"
			return accounts
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateEngineAccounts(mutate(cloneEngineTestAccounts())); err == nil {
				t.Fatal("invalid live inputs accepted")
			}
		})
	}
}

func TestPrepareEngineAccountsBoundsOneRuntimeTokenEach(t *testing.T) {
	accounts, err := prepareEngineAccounts(engineTestAccounts())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 4 || accounts[0].OneBotPort != 3210 || accounts[3].OneBotPort != 3213 {
		t.Fatalf("accounts not ordered by port: %+v", accounts)
	}
	tokens := map[string]bool{}
	for _, account := range accounts {
		if account.Token == "" || tokens[account.Token] {
			t.Fatalf("account %s has a blank or reused runtime token", account.AccountID)
		}
		tokens[account.Token] = true
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks", nil)
		request.Header.Set("Authorization", "Bearer "+account.Token)
		harmless := &engineSmoke{accounts: accounts}
		actor, err := harmless.authenticateTask(request)
		if err != nil {
			t.Fatalf("task auth for %s: %v", account.AccountID, err)
		}
		if actor.RuntimeID != account.RuntimeID || len(actor.AccountIDs) != 1 || actor.AccountIDs[0] != account.AccountID {
			t.Fatalf("task auth mapped %s to %+v", account.AccountID, actor)
		}
		runtimeActor, err := harmless.authenticateRuntime(request)
		if err != nil {
			t.Fatalf("runtime auth for %s: %v", account.AccountID, err)
		}
		if runtimeActor.RuntimeID != account.RuntimeID || len(runtimeActor.AccountIDs) != 1 || runtimeActor.AccountIDs[0] != account.AccountID {
			t.Fatalf("runtime auth mapped %s to %+v", account.AccountID, runtimeActor)
		}
	}
	for name, header := range map[string]string{
		"missing": "",
		"foreign": "Bearer not-a-fixture-token",
		"scheme":  "Runtime not-a-fixture-token",
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks", nil)
			if header != "" {
				request.Header.Set("Authorization", header)
			}
			if _, err := (&engineSmoke{accounts: accounts}).authenticateTask(request); err == nil {
				t.Fatal("foreign token authenticated")
			}
		})
	}
}

func TestEngineTaskRequestIsDryRunAndOwnAccountOnly(t *testing.T) {
	accounts, err := prepareEngineAccounts(engineTestAccounts())
	if err != nil {
		t.Fatal(err)
	}
	request := engineTaskRequest(accounts[0], engineTargetGroup(0))
	if request.ExecutionMode == nil || *request.ExecutionMode != contract.DryRun || request.Submit {
		t.Fatalf("task request is not an unsubmitted dry run: %+v", request)
	}
	if len(request.AllowedAccountIds) != 1 || request.AllowedAccountIds[0] != accounts[0].AccountID {
		t.Fatalf("task request account scope=%v, want %s", request.AllowedAccountIds, accounts[0].AccountID)
	}
	if request.Goal.Count != 1 || len(request.CandidateSource.Groups) != 1 ||
		request.CandidateSource.Groups[0].GroupId != engineTargetGroup(0) {
		t.Fatalf("task request candidate/goal mismatch: %+v", request)
	}
	if request.ApprovalRef == nil || request.ApprovalRef.IsSpecified() == false || !request.ApprovalRef.IsNull() {
		t.Fatalf("dry-run approval_ref must be explicitly null")
	}
}

func TestRunEngineSmokeRejectsNonFixtureEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BBA_FIXTURE_ONLY", "")
	t.Setenv("BBA_FIXTURE_DSN", "postgres://bba_fixture@/bba_fixture_admin?host=/fixture&sslmode=disable")
	proof, err := runEngineSmoke(context.Background(), dir, engineTestAccounts())
	if err == nil {
		t.Fatal("engine smoke accepted a missing fixture marker")
	}
	if proof["outcome"] != "FAIL" {
		t.Fatalf("outcome=%v, want FAIL", proof["outcome"])
	}
	body, err := os.ReadFile(filepath.Join(dir, engineEvidence))
	if err != nil {
		t.Fatalf("failure evidence: %v", err)
	}
	var document struct {
		Outcome string         `json:"outcome"`
		Fixture map[string]any `json:"fixture"`
		Error   string         `json:"error"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if document.Outcome != "FAIL" || document.Error == "" {
		t.Fatalf("failure evidence=%s", body)
	}
	if document.Fixture != nil {
		t.Fatalf("guard failure created a fixture database: %s", body)
	}
}

func TestRunEngineSmokeRejectsMismatchedLiveInputs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BBA_FIXTURE_ONLY", "1")
	t.Setenv("BBA_FIXTURE_DSN", "postgres://bba_fixture@/bba_fixture_admin?host=/fixture&sslmode=disable")
	accounts := cloneEngineTestAccounts()
	accounts[2].ObservedSelfID = "100000002"
	proof, err := runEngineSmoke(context.Background(), dir, accounts)
	if err == nil {
		t.Fatal("engine smoke accepted an identity mismatch")
	}
	if proof["outcome"] != "FAIL" {
		t.Fatalf("outcome=%v, want FAIL", proof["outcome"])
	}
	body, err := os.ReadFile(filepath.Join(dir, engineEvidence))
	if err != nil {
		t.Fatalf("failure evidence: %v", err)
	}
	var document struct {
		Outcome string         `json:"outcome"`
		Fixture map[string]any `json:"fixture"`
		Error   string         `json:"error"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if document.Outcome != "FAIL" || !strings.Contains(document.Error, "does not match binding") {
		t.Fatalf("failure evidence=%s", body)
	}
	if document.Fixture != nil {
		t.Fatalf("mismatched live input created a fixture database: %s", body)
	}
}

func TestRunEngineSmokeFourAccountFixtureIsolation(t *testing.T) {
	if os.Getenv("BBA_FIXTURE_ONLY") != "1" || os.Getenv("BBA_FIXTURE_DSN") == "" {
		t.Skip("isolated PostgreSQL fixture is required")
	}
	if err := validateEngineSmokeEnv(); err != nil {
		t.Skipf("isolated PostgreSQL fixture is unavailable: %v", err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := runEngineSmoke(ctx, dir, engineTestAccounts())
	body, readErr := os.ReadFile(filepath.Join(dir, engineEvidence))
	if readErr != nil {
		t.Fatalf("read engine evidence: %v (run error: %v)", readErr, err)
	}
	var document struct {
		Outcome       string `json:"outcome"`
		SchemaVersion int    `json:"schema_version"`
		Fixture       struct {
			Database string `json:"database"`
			Unix     bool   `json:"unix_socket"`
			TCP      bool   `json:"tcp"`
			Retained bool   `json:"retained"`
		} `json:"fixture"`
		Accounts []engineAccountEvidence `json:"accounts"`
		Mappings []engineMappingEvidence `json:"mappings"`
		DB       struct {
			TaskRows                 int `json:"task_rows"`
			TaskAccountRows          int `json:"task_account_rows"`
			IdempotencyRows          int `json:"idempotency_rows"`
			HeartbeatRows            int `json:"heartbeat_rows"`
			HeartbeatIdempotencyRows int `json:"heartbeat_idempotency_rows"`
			AccountRows              int `json:"account_rows"`
			CredentialRows           int `json:"credential_rows"`
			GoalSlotRows             int `json:"goal_slot_rows"`
			GoalSlotActiveRows       int `json:"goal_slot_active_rows"`
			ExecutionRows            int `json:"execution_rows"`
			GatewayOperationRows     int `json:"gateway_operation_rows"`
			DispatchPermitRows       int `json:"dispatch_permit_rows"`
			ResultRows               int `json:"result_rows"`
			SubtaskRows              int `json:"subtask_rows"`
			TargetClaimRows          int `json:"target_claim_rows"`
			AccountUsageRows         int `json:"account_usage_rows"`
			AccountActionLockRows    int `json:"account_action_lock_rows"`
			InboxRows                int `json:"inbox_rows"`
			MembershipRows           int `json:"membership_rows"`
			ApprovalRows             int `json:"approval_rows"`
		} `json:"db"`
		Rejections map[string]struct {
			Status int    `json:"status"`
			Code   string `json:"code"`
		} `json:"rejections"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if document.Fixture.Database != "" {
		t.Cleanup(func() { dropEngineFixtureDatabase(t, os.Getenv("BBA_FIXTURE_DSN"), document.Fixture.Database) })
	}
	if err != nil {
		t.Fatalf("engine smoke failed: %v evidence=%s", err, body)
	}
	if document.Outcome != "PASS" {
		t.Fatalf("outcome=%s evidence=%s", document.Outcome, body)
	}
	if document.SchemaVersion != taskengine.SchemaHead() || document.Fixture.Database == "" || !document.Fixture.Unix || document.Fixture.TCP || !document.Fixture.Retained {
		t.Fatalf("fixture evidence=%+v", document.Fixture)
	}
	if len(document.Accounts) != 4 || len(document.Mappings) != 4 {
		t.Fatalf("accounts=%d mappings=%d", len(document.Accounts), len(document.Mappings))
	}
	accountIDs := map[string]bool{}
	runtimeIDs := map[string]bool{}
	selfIDs := map[string]bool{}
	taskIDs := map[string]bool{}
	for _, account := range document.Accounts {
		if accountIDs[account.AccountID] || runtimeIDs[account.RuntimeID] || selfIDs[account.ObservedSelfID] || taskIDs[account.Task.TaskID] {
			t.Fatalf("duplicate account/runtime/self/task evidence: %+v", account)
		}
		accountIDs[account.AccountID] = true
		runtimeIDs[account.RuntimeID] = true
		selfIDs[account.ObservedSelfID] = true
		taskIDs[account.Task.TaskID] = true
		if account.Heartbeat.Status != http.StatusAccepted || account.Heartbeat.ReplayStatus != http.StatusAccepted ||
			!account.Heartbeat.ReplayExact || account.Heartbeat.Rows != 1 || account.Heartbeat.AccountVersion != 2 {
			t.Fatalf("heartbeat evidence for %s: %+v", account.AccountID, account.Heartbeat)
		}
		if account.Task.CreateStatus != http.StatusCreated || account.Task.ReplayStatus != http.StatusCreated ||
			!account.Task.ReplayExact || account.Task.ReadStatus != http.StatusOK ||
			account.Task.TaskAccountRows != 1 || account.Task.Mode != "dry_run" ||
			account.Task.State != string("DRAFT") || account.Task.ReadState != account.Task.State {
			t.Fatalf("task evidence for %s: %+v", account.AccountID, account.Task)
		}
		if account.Database.RuntimeID != account.RuntimeID || account.Database.SelfID != account.ObservedSelfID ||
			account.Database.IdentityStatus != "verified" ||
			account.Database.ConnectionState != "online" || account.Database.LoginState != "logged_in" ||
			account.Database.ControlState != "paused" || account.Database.Version != 2 ||
			account.Database.RequestJoinVerified || account.Database.MembershipVerified ||
			account.Database.MembershipAuthority || !account.Database.CredentialActive ||
			account.Database.HeartbeatRows != 1 {
			t.Fatalf("account projection for %s: %+v", account.AccountID, account.Database)
		}
	}
	for _, mapping := range document.Mappings {
		matched := false
		for _, account := range document.Accounts {
			if mapping.TaskID == account.Task.TaskID && mapping.AccountID == account.AccountID &&
				mapping.RuntimeID == account.RuntimeID && mapping.SelfID == account.ObservedSelfID {
				matched = true
				break
			}
		}
		if !matched || mapping.Mode != "dry_run" || mapping.State != "DRAFT" {
			t.Fatalf("unexpected task mapping: %+v", mapping)
		}
	}
	if document.DB.TaskRows != 4 || document.DB.TaskAccountRows != 4 ||
		document.DB.IdempotencyRows != 4 || document.DB.HeartbeatRows != 4 ||
		document.DB.HeartbeatIdempotencyRows != 4 || document.DB.AccountRows != 4 ||
		document.DB.CredentialRows != 4 || document.DB.GoalSlotRows != 4 ||
		document.DB.GoalSlotActiveRows != 0 {
		t.Fatalf("unexpected duplicate counts: %+v", document.DB)
	}
	if document.DB.ExecutionRows != 0 || document.DB.GatewayOperationRows != 0 ||
		document.DB.DispatchPermitRows != 0 || document.DB.ResultRows != 0 ||
		document.DB.SubtaskRows != 0 || document.DB.TargetClaimRows != 0 ||
		document.DB.AccountUsageRows != 0 || document.DB.AccountActionLockRows != 0 ||
		document.DB.InboxRows != 0 || document.DB.MembershipRows != 0 || document.DB.ApprovalRows != 0 {
		t.Fatalf("sensitive rows were written: %+v", document.DB)
	}
	for name, want := range map[string]int{
		"cross_runtime_heartbeat":   http.StatusNotFound,
		"cross_account_heartbeat":   http.StatusNotFound,
		"cross_account_task_read":   http.StatusNotFound,
		"cross_account_task_create": http.StatusForbidden,
	} {
		rejection, ok := document.Rejections[name]
		if !ok || rejection.Status != want {
			t.Fatalf("rejection %s=%+v, want status %d", name, rejection, want)
		}
		if want == http.StatusNotFound && rejection.Code != "RESOURCE_NOT_FOUND" {
			t.Fatalf("rejection %s code=%s", name, rejection.Code)
		}
		if want == http.StatusForbidden && rejection.Code != "ACTION_FORBIDDEN" {
			t.Fatalf("rejection %s code=%s", name, rejection.Code)
		}
	}
}

func dropEngineFixtureDatabase(t *testing.T, dsn, name string) {
	t.Helper()
	if name == "" {
		return
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Errorf("parse fixture DSN for cleanup: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Errorf("connect fixture admin for cleanup: %v", err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		t.Errorf("drop engine fixture database %s: %v", name, err)
	}
}
