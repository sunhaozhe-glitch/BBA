package taskengine

import (
	"context"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"reflect"
	"testing"
)

// A genuine populated schema23 fixture, not a head database relabelled as old.
func TestWorkSettingsMigrate23PreservesRows(t *testing.T) {
	ctx := context.Background()
	cfg, e := pgx.ParseConfig(os.Getenv("BBA_FIXTURE_DSN"))
	if e != nil || os.Getenv("BBA_FIXTURE_ONLY") != "1" || cfg.Host != "/fixture" || cfg.Database != "bba_fixture_admin" || cfg.User != "bba_fixture" {
		t.Fatal("isolated fixture required")
	}
	admin, e := pgx.ConnectConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	db := newID("settings_old23")
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if _, e := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	u, e := url.Parse(os.Getenv("BBA_FIXTURE_DSN"))
	if e != nil {
		t.Fatal(e)
	}
	u.Path = "/" + db
	s, e := Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	names := append([]string{"0001_core.sql"}, migrationTail[:13]...)
	for _, name := range names {
		sql, e := migrationFiles.ReadFile("migrations/" + name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Pool.Exec(ctx, string(sql)); e != nil {
			t.Fatal(name, e)
		}
	}
	if n := scalarInt(t, s, `SELECT max(version) FROM core.schema_version`); n != 23 {
		t.Fatal(n)
	}
	if _, e = s.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES('ws_fixture'); INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','principal_fixture')`); e != nil {
		t.Fatal(e)
	}
	seedAccount(t, s, "acc_old22", "rt_old22", 20)
	// Start the historical task with the old code path's SQL transaction setting
	// unavailable: the new client must not attempt the new column before migration.
	task := workSettingsReadyTask(t, s, []string{"acc_old22"}, []string{"9014222"}, 1, 2, "fixture reason", "fixture answer", fixtureActor())
	if _, e = s.Pool.Exec(ctx, `UPDATE core.task SET state='RUNNING',version=version+1 WHERE id=$1`, task.Id); e != nil {
		t.Fatal(e)
	}
	b4Control(t, s, task.Id, "pause")
	rows, e := s.Pool.Query(ctx, `SELECT schemaname,tablename FROM pg_tables WHERE schemaname IN ('core','bridge') AND tablename<>'schema_version' AND tablename NOT IN ('workspace_work_settings','workspace_work_settings_audit') ORDER BY 1,2`)
	if e != nil {
		t.Fatal(e)
	}
	type ref struct{ s, n string }
	var refs []ref
	for rows.Next() {
		var r ref
		if e = rows.Scan(&r.s, &r.n); e != nil {
			t.Fatal(e)
		}
		refs = append(refs, r)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	snapshot := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, r := range refs {
			var raw string
			q := `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY (to_jsonb(x))::text),'[]'::jsonb)::text FROM ` + pgx.Identifier{r.s, r.n}.Sanitize() + ` x`
			if e = s.Pool.QueryRow(ctx, q).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			out[r.s+"."+r.n] = raw
		}
		return out
	}
	before := snapshot()
	s.WorkSlotsDefault = 8
	for i := 0; i < 2; i++ {
		if e = s.Migrate(ctx); e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(before, snapshot()) {
			t.Fatal("historical contents changed")
		}
	}
	if n := scalarInt(t, s, `SELECT work_slots FROM core.task WHERE id=$1`, task.Id); n != 1 {
		t.Fatal("old task did not retain compatible1", n)
	}
	b4Control(t, s, task.Id, "resume")
	if n := scalarInt(t, s, `SELECT work_slots FROM core.task WHERE id=$1`, task.Id); n != 1 {
		t.Fatal("old resume took new default", n)
	}
	beforeReopen := snapshot()
	s.Close()
	s, e = Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	// Pause/resume above legitimately adds lifecycle events; migration/reopen must not.
	reopened := snapshot()
	if !reflect.DeepEqual(beforeReopen, reopened) {
		t.Fatal("reopen changed original rows")
	}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(reopened, snapshot()) {
		t.Fatal("idempotent reopen changed rows")
	}
	if scalarInt(t, s, `SELECT max(version) FROM core.schema_version`) != migrationHead {
		t.Fatal("wrong head")
	}
	fresh := fixtureStore(t)
	if scalarInt(t, fresh, `SELECT max(version) FROM core.schema_version`) != migrationHead || scalarInt(t, fresh, `SELECT count(*) FROM core.workspace_work_settings`) != 0 {
		t.Fatal("fresh settings schema mismatch")
	}
	t.Logf("real23->%d; all core+bridge legacy columns preserved; old paused task1 survives default8/resume; scripts=%v", migrationHead, names)
}
