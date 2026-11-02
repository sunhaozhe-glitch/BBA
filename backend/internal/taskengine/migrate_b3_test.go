package taskengine

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Never fabricate an old schema by relabelling head: execute precisely the
// embedded historical sequence, 0001 through 0012, then call public Migrate.
func TestB3Migrate21PreservesExistingData(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("BBA_FIXTURE_DSN")
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil || os.Getenv("BBA_FIXTURE_ONLY") != "1" || cfg.Host != "/fixture" || cfg.Database != "bba_fixture_admin" || cfg.User != "bba_fixture" {
		t.Fatal("isolated kernel fixture required, never skip")
	}
	admin, e := pgx.ConnectConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	db := newID("migrate_b3")
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if _, e := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	u, _ := url.Parse(dsn)
	u.Path = "/" + db
	s, e := Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	names := append([]string{"0001_core.sql"}, migrationTail[:11]...)
	for _, name := range names {
		b, e := migrationFiles.ReadFile("migrations/" + name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Pool.Exec(ctx, string(b)); e != nil {
			t.Fatalf("%s: %v", name, e)
		}
	}
	if v := scalarInt(t, s, `SELECT max(version) FROM core.schema_version`); v != 21 {
		t.Fatalf("not real schema21: %d", v)
	}
	if v := scalarInt(t, s, `SELECT count(*) FROM pg_tables WHERE schemaname='core' AND tablename LIKE 'content_%'`); v != 0 {
		t.Fatal("B3 tables existed before upgrade")
	}
	must := func(sql string, args ...any) {
		t.Helper()
		if _, e := s.Pool.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	must(`INSERT INTO core.workspace(id) VALUES('ws_fixture'),('ws_other');INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','principal_fixture'),('ws_other','principal_other')`)
	seedAccount(t, s, "acc_migrate", "rt_migrate", 100)
	task := workSettingsReadyTask(t, s, []string{"acc_migrate"}, []string{"777"}, 1, 1, "fixture reason", "fixture answer", fixtureActor())
	if _, e = s.Pool.Exec(ctx, `UPDATE core.task SET state='RUNNING',version=version+1 WHERE id=$1`, task.Id); e != nil {
		t.Fatal(e)
	}
	must(`INSERT INTO core.login_session(workspace_id,id,account_id,adapter_instance_id,runtime_id,provider,expires_at) VALUES('ws_fixture','session_migrate','acc_migrate','adapter_acc_migrate','rt_migrate','fixture',clock_timestamp()+interval '1 hour');INSERT INTO core.login_session_event(workspace_id,session_id,seq,id,kind,generation,detail,actor) VALUES('ws_fixture','session_migrate',1,'event_migrate','created',0,'{}','principal_fixture')`)
	must(`INSERT INTO core.model_request(workspace_id,id,task_id,principal_id,idempotency_key,request_digest,operation_kind,schema_id,prompt_version,input_ref,deadline_at,max_provider_attempts) VALUES('ws_fixture','model_migrate',$1,'principal_fixture','model_key',repeat('a',64),'complex_plan','PlanProposal.v1','v1',$1,clock_timestamp()+interval '1 hour',1)`, task.Id)
	must(`INSERT INTO core.task_graph_roots(workspace_id,task_id,graph_kind,provider,external_root) VALUES('ws_fixture',$1,'goal','artex','isolated-root')`, task.Id)
	deadline := chatTestDeadline(t)
	chatSeedPolicy(t, s, "acc_migrate", 100)
	chatSeedSpeaker(t, s, "777", "acc_migrate", 1, deadline)
	chatSeedTrigger(t, s, "trigger_migrate", "rt_migrate", "acc_migrate", "group", "777", "20001", nil, deadline, 1)
	if _, e = s.IssueChatSendPermit(ctx, fixtureRuntime("acc_migrate", "rt_migrate"), "permit_migrate", chatPermitRequest("send_migrate", "acc_migrate", "trigger_migrate", "group", "777", 0, 1, deadline)); e != nil {
		t.Fatal(e)
	}
	// B1/B2 model/Skill/Agent configuration is protected-file storage, NOT SQL.
	// Preserve the real registry bytes and reopen the public registry too.
	dir := t.TempDir()
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "registry.json")
	r, e := OpenModelRegistry(path, "", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	m, e := r.save("ws_fixture", "", modelProfileWrite{Name: "migration model", Model: "isolated", BaseURL: "https://example.invalid/v1", Enabled: true, IsDefault: true, TimeoutSeconds: 5, MaxConcurrent: 1, SecretAction: "replace", APIKey: "MIGRATION_SYNTHETIC_SECRET"}, false)
	if e != nil {
		t.Fatal(e)
	}
	sk, e := r.saveSkill("ws_fixture", "principal_fixture", "", SkillProfile{ConfigMeta: ConfigMeta{Name: "migration skill", Enabled: true}, Goal: "test", Steps: []string{"test"}, StopConditions: []string{"stop"}, Published: true}, 0, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.saveAgent("ws_fixture", "principal_fixture", "", AgentProfile{ConfigMeta: ConfigMeta{Name: "migration agent", Enabled: true}, ModelID: m.ID, Skills: []SkillBinding{{ID: sk.ID, Version: sk.Version}}}, 0, false); e != nil {
		t.Fatal(e)
	}
	r.Close()
	registryBefore, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	rows, e := s.Pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='core' AND tablename<>'schema_version' AND tablename NOT LIKE 'content_%' AND tablename NOT IN ('workspace_work_settings','workspace_work_settings_audit') ORDER BY tablename`)
	if e != nil {
		t.Fatal(e)
	}
	var legacyTables []string
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			t.Fatal(e)
		}
		legacyTables = append(legacyTables, name)
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	rows.Close()
	snapshot := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, table := range legacyTables {
			var raw string
			q := fmt.Sprintf(`SELECT COALESCE(jsonb_agg((to_jsonb(x)-'work_slots') ORDER BY (to_jsonb(x)-'work_slots')::text),'[]'::jsonb)::text FROM core.%s x`, pgx.Identifier{table}.Sanitize())
			if e = s.Pool.QueryRow(ctx, q).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			out[table] = raw
		}
		return out
	}
	before := snapshot()
	nonempty := []string{}
	for k, v := range before {
		if v != "[]" {
			nonempty = append(nonempty, k)
		}
	}
	t.Logf("historical scripts=%v populated=%v", names, nonempty)
	legacyConstraintTables := make([]string, len(legacyTables))
	for i, table := range legacyTables {
		legacyConstraintTables[i] = "core." + table
	}
	constraintSQL := `SELECT COALESCE(jsonb_agg(jsonb_build_array(c.conrelid::regclass::text,c.conname,pg_get_constraintdef(c.oid)) ORDER BY c.conrelid::regclass::text,c.conname),'[]'::jsonb)::text FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname='core' AND c.conrelid::regclass::text = ANY($1::text[]) AND c.conname<>'task_work_slots_check'`
	var constraints string
	if e = s.Pool.QueryRow(ctx, constraintSQL, legacyConstraintTables).Scan(&constraints); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if e = s.Migrate(ctx); e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(before, snapshot()) {
			t.Fatal("preexisting row contents changed")
		}
	}
	s.Close()
	s, e = Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	if scalarInt(t, s, `SELECT max(version) FROM core.schema_version`) != migrationHead || !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("reopen changed preserved data")
	}
	var afterConstraints string
	if e = s.Pool.QueryRow(ctx, constraintSQL, legacyConstraintTables).Scan(&afterConstraints); e != nil || constraints != afterConstraints {
		t.Fatal("legacy constraints changed", e)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM pg_tables WHERE schemaname='core' AND tablename=ANY($1::text[])`, []string{"operator_conversation", "operator_message", "qq_draft", "operator_notification"}); got != 4 {
		t.Fatalf("QQ conversation migration tables=%d, want 4", got)
	}
	for _, bad := range []string{`INSERT INTO core.login_session_event(workspace_id,session_id,seq,id,kind,generation,detail,actor) VALUES('ws_other','session_migrate',2,'bad','created',0,'{}','principal_other')`, `UPDATE core.login_session_event SET actor='changed' WHERE id='event_migrate'`, `INSERT INTO core.content_review_note(workspace_id,review_id,id,note,updated_by) VALUES('ws_fixture','missing','bad','note','principal_fixture')`} {
		if _, e = s.Pool.Exec(ctx, bad); e == nil {
			t.Fatalf("constraint unexpectedly allowed %s", bad)
		}
	}
	r, e = OpenModelRegistry(path, "", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	registryAfter, e := os.ReadFile(path)
	if e != nil || string(registryBefore) != string(registryAfter) {
		t.Fatal("registry changed")
	}
	public, _ := json.Marshal(r.list("ws_fixture"))
	if strings.Contains(string(public), "MIGRATION_SYNTHETIC_SECRET") {
		t.Fatal("public model leaks secret")
	}
	h := s.ContentHandler(func(*http.Request) (Actor, error) { return contentOperator(), nil })
	res := contentRequest(t, h, "/v1/operator/content-reviews", "GET", nil)
	if res.Code != 200 || strings.Contains(res.Body.String(), "MIGRATION_SYNTHETIC_SECRET") {
		t.Fatal("content endpoint unavailable or leaks")
	}
	fresh := fixtureStore(t)
	if scalarInt(t, fresh, `SELECT max(version) FROM core.schema_version`) != migrationHead {
		t.Fatal("fresh initialization failed")
	}
	t.Logf("PASS: actual21->public Migrate%d, exact legacy rows, legacy constraints, idempotence, Open, registry reopen, redaction, fresh%d", migrationHead, migrationHead)
}
