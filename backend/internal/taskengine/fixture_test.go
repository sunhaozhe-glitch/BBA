package taskengine

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
)

// fixtureStore clones a process-local, migrated, connection-disabled database.
// Business data is seeded only in each private clone, never in the template.
func fixtureStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	cfg := fixtureAdminConfig(t)
	template, err := ensureFixtureTemplate(ctx, cfg)
	if err != nil {
		t.Fatalf("fixture template: %v", err)
	}
	return fixtureDatabase(t, cfg, template, true)
}

func fixtureAdminConfig(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	dsn := os.Getenv("BBA_FIXTURE_DSN")
	if dsn == "" || os.Getenv("BBA_FIXTURE_ONLY") != "1" {
		t.Fatal("use tools/kernel.sh test: isolated PostgreSQL fixture required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database != "bba_fixture_admin" || cfg.User != "bba_fixture" || cfg.Host != "/fixture" {
		t.Fatal("refusing non-fixture database connection")
	}
	return cfg
}

// An empty template argument deliberately creates a real empty database. This
// path is used by the independent full migration test, not by the clone cache.
func fixtureDatabase(t *testing.T, cfg *pgx.ConnConfig, template string, seed bool) *Store {
	t.Helper()
	ctx := context.Background()
	start := time.Now()
	admin, err := pgx.ConnectConfig(ctx, cfg.Copy())
	if err != nil {
		t.Fatal(err)
	}
	db := newID("fixture")
	query := "CREATE DATABASE " + pgx.Identifier{db}.Sanitize()
	if template != "" {
		query += " TEMPLATE " + pgx.Identifier{template}.Sanitize()
	}
	if _, err = admin.Exec(ctx, query); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	fixtureResources.Lock()
	fixtureResources.names = append(fixtureResources.names, db)
	fixtureResources.cfg = cfg.Copy()
	fixtureResources.Unlock()
	t.Logf("FIXTURE_CREATED %s template=%s", db, template)
	var store *Store
	// Register immediately after CREATE: Open, migration and seed failures clean up.
	t.Cleanup(func() {
		if store != nil {
			store.Close()
		}
		_, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)")
		if err != nil {
			t.Errorf("drop fixture %s: %v", db, err)
		}
		admin.Close(ctx)
	})
	u, err := url.Parse(os.Getenv("BBA_FIXTURE_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	store, err = Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if seed {
		if _, err = store.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES('ws_fixture'); INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','principal_fixture')`); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("FIXTURE_READY %s setup_ms=%.3f", db, float64(time.Since(start).Microseconds())/1000)
	return store
}
func fixtureActor() Actor {
	return Actor{WorkspaceID: "ws_fixture", PrincipalID: "principal_fixture", Permissions: map[string]bool{"task.create": true, "task.read": true, "task.control": true, "task.manage_all": true, "task.approve": true}}
}
func seedAccount(t *testing.T, s *Store, id, runtime string, quota int) {
	t.Helper()
	ctx := context.Background()
	var num int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM core.account`).Scan(&num)
	if err != nil {
		t.Fatal(err)
	}
	self := fmt.Sprint(10001 + num)
	_, err = s.Pool.Exec(ctx, `INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit,heartbeat_at) VALUES('ws_fixture',$1,$2,$3,$4,'verified','online','logged_in','active','clear',true,true,$5,clock_timestamp())`, id, runtime, "adapter_"+id, self, quota)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority) VALUES('ws_fixture',$1,$2,1,true)`, runtime, id)
	if err != nil {
		t.Fatal(err)
	}
}
func fixtureRuntime(account, runtime string) RuntimeActor {
	return RuntimeActor{WorkspaceID: "ws_fixture", RuntimeID: runtime, OwnerEpoch: 1, AccountIDs: []string{account}}
}
func createReadyTask(t *testing.T, s *Store, accounts, groups []string, goal, maxAttempts int) contract.Task {
	t.Helper()
	return createReadyTaskWithText(t, s, accounts, groups, goal, maxAttempts, "fixture reason", "fixture answer")
}

func createReadyTaskWithText(t *testing.T, s *Store, accounts, groups []string, goal, maxAttempts int, reason, answer string) contract.Task {
	t.Helper()
	ctx := context.Background()
	actor := fixtureActor()
	approval := newID("approval")
	_, err := s.Pool.Exec(ctx, `INSERT INTO core.approval(workspace_id,id,principal_id,scope_revision,account_ids,target_ids,max_attempts,expires_at,join_reason,verification_message) VALUES($1,$2,$3,1,$4,$5,10000,clock_timestamp()+interval '1 day',$6,$7)`, actor.WorkspaceID, approval, actor.PrincipalID, accounts, groups, reason, answer)
	if err != nil {
		t.Fatal(err)
	}
	fixed := make([]contract.FixedCandidate, 0, len(groups))
	for _, id := range groups {
		fixed = append(fixed, contract.FixedCandidate{GroupId: id, SourceRef: "fixture_import"})
	}
	mode := contract.ExecutionMode("live")
	req := contract.CreateTaskRequest{Type: "group.acquire", Title: "fixture task", JoinReason: reason, VerificationMessage: answer, Goal: contract.Goal{Metric: "new_unique_groups_for_account_pool", Count: goal, StrictTargetCap: true, RequireFinalMembershipCheck: true}, AllowedAccountIds: accounts, CandidateSource: contract.CandidateSource{Kind: "fixed", Groups: fixed}, ExecutionMode: &mode, DeadlineAt: time.Now().UTC().Add(time.Hour), Limits: contract.TaskLimits{MaxCandidates: len(groups), MaxAttemptsPerAction: maxAttempts, MaxTotalExternalAttempts: 100, MaxModelAttempts: 0}, ApprovalRef: nullable.NewNullableWithValue(approval), Submit: true}
	task, err := s.CreateTask(ctx, actor, newID("create"), req)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	for {
		done, err := s.ExpandFixedPage(ctx, actor, task.Id, 100)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	snapshots := make([]BaselineSnapshot, 0, len(accounts))
	for _, id := range accounts {
		snapshots = append(snapshots, BaselineSnapshot{AccountID: id, EvidenceRef: "fixture_complete_baseline", ObservedAt: time.Now().UTC(), Complete: true, GroupIDs: []string{}})
	}
	if err := s.CompleteBaseline(ctx, actor, task.Id, snapshots); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	task, err = s.GetTask(ctx, actor, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ControlTask(ctx, actor, task.Id, newID("start"), contract.TaskControlRequest{Action: "start", ExpectedVersion: task.Version, Reason: "fixture start"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err = s.GetTask(ctx, actor, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func reserveFixture(t *testing.T, s *Store, taskID, preferred string) Reservation {
	t.Helper()
	r, err := s.Reserve(context.Background(), "ws_fixture", taskID, preferred)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return r
}
func scalarInt(t *testing.T, s *Store, sql string, args ...any) int {
	t.Helper()
	var value int
	if err := s.Pool.QueryRow(context.Background(), sql, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
