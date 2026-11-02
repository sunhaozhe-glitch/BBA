package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bba.local/taskengine/internal/taskengine"
	"github.com/jackc/pgx/v5"
)

// Never touches a caller's database: require the kernel fixture and allocate a child DB.
func schemaGateFixture(t *testing.T) *taskengine.Store {
	t.Helper()
	ctx := context.Background()
	cfg, e := pgx.ParseConfig(os.Getenv("BBA_FIXTURE_DSN"))
	if e != nil {
		t.Fatal(e)
	}
	if os.Getenv("BBA_FIXTURE_ONLY") != "1" || cfg.Host != "/fixture" || cfg.User != "bba_fixture" || cfg.Database != "bba_fixture_admin" {
		t.Fatal("kernel fixture required")
	}
	admin, e := pgx.ConnectConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	db := fmt.Sprintf("schema_gate_%d", time.Now().UnixNano())
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); e != nil {
		admin.Close(ctx)
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("BBA_FIXTURE_DSN"))
	if e != nil {
		t.Fatal(e)
	}
	u.Path = "/" + db
	s, e := taskengine.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		s.Close()
		if _, e := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
		var remains bool
		if e := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, db).Scan(&remains); e != nil || remains {
			t.Errorf("fixture cleanup database=%s remains=%v error=%v", db, remains, e)
		} else {
			t.Logf("fixture cleanup database=%s absent=true", db)
		}
		admin.Close(ctx)
	})
	files, e := filepath.Glob("../../internal/taskengine/migrations/*.sql")
	if e != nil || len(files) == 0 {
		t.Fatal("historical migration files missing", e)
	}
	for _, p := range files {
		if filepath.Base(p) >= "0014_" {
			break
		}
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Pool.Exec(ctx, string(b)); e != nil {
			t.Fatal(p, e)
		}
	}
	var version, workSlots int
	if e := s.Pool.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&version); e != nil || version != 22 {
		t.Fatalf("historical migration sequence: version=%d error=%v", version, e)
	}
	if e := s.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='core' AND table_name='task' AND column_name='work_slots'`).Scan(&workSlots); e != nil || workSlots != 0 {
		t.Fatalf("schema22 unexpectedly has work_slots: count=%d error=%v", workSlots, e)
	}
	return s
}

func schemaGateSnapshot(t *testing.T, s *taskengine.Store) string {
	t.Helper()
	var out string
	// Both schema ledger and catalog: startup must not create content tables or other objects.
	e := s.Pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'ledger', (SELECT jsonb_agg(to_jsonb(v) ORDER BY version) FROM core.schema_version v),
 'columns', (SELECT jsonb_agg(to_jsonb(c) ORDER BY table_schema,table_name,ordinal_position) FROM information_schema.columns c WHERE table_schema IN ('core','bridge')),
 'constraints', (SELECT jsonb_agg(jsonb_build_array(n.nspname,c.conname,pg_get_constraintdef(c.oid)) ORDER BY n.nspname,c.conname,c.oid) FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname IN ('core','bridge')),
 'objects', (SELECT jsonb_agg(to_jsonb(c) ORDER BY c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('core','bridge')),
 'functions', (SELECT jsonb_agg(pg_get_functiondef(p.oid) ORDER BY p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname IN ('core','bridge')),
 'triggers', (SELECT jsonb_agg(pg_get_triggerdef(t.oid) ORDER BY t.oid) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('core','bridge'))
 )::text`).Scan(&out)
	if e != nil {
		t.Fatal(e)
	}
	return out
}

func TestB4ServerSchemaStartupChild(t *testing.T) {
	if os.Getenv("BBA_SCHEMA_STARTUP_CHILD") != "1" {
		return
	}
	os.Args = []string{"taskengine-server", "-worker", "-listen=127.0.0.1:0", "-origin=http://127.0.0.1:0", "-resolver-file=" + os.Getenv("BBA_SCHEMA_RESOLVER"), "-login-provider-file=/fixture/schema-gate-provider-must-not-be-read"}
	main()
	t.Fatal("schema22 main unexpectedly returned")
}

func TestB4ServerSchemaStartupPolicy(t *testing.T) {
	s := schemaGateFixture(t)
	ctx := context.Background()
	check := func(version int, allowed bool) {
		t.Helper()
		before := schemaGateSnapshot(t, s)
		got, e := validateServerSchema(s)
		if got != version || (e == nil) != allowed {
			t.Fatalf("schema=%d got=%d allowed=%v error=%v", version, got, allowed, e)
		}
		if after := schemaGateSnapshot(t, s); before != after {
			t.Fatal("startup implicitly migrated schema")
		}
		t.Logf("schema=%d allowed=%v no_implicit_migration=true", version, allowed)
	}
	check(22, false)
	// Exercise main itself with -worker: old schemas must fail before provider
	// configuration, binding a listener, or allowing worker SQL to run.
	before := schemaGateSnapshot(t, s)
	resolver := writeResolverFixture(t, 0600, `{"credentials":[{"token_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","workspace_id":"ws_schema_fixture","principal_id":"operator_schema_fixture","permissions":["task.read"]}]}`)
	binary, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("BBA_FIXTURE_DSN"))
	if e != nil {
		t.Fatal(e)
	}
	u.Path = "/" + s.Pool.Config().ConnConfig.Database
	childCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(childCtx, binary, "-test.run=^TestB4ServerSchemaStartupChild$")
	cmd.Env = append(os.Environ(), "BBA_SCHEMA_STARTUP_CHILD=1", "BBA_SCHEMA_RESOLVER="+resolver, "BBA_TASKENGINE_DSN="+u.String())
	output, runErr := cmd.CombinedOutput()
	exit, ok := runErr.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 || !strings.Contains(string(output), fmt.Sprintf("requires schema >= %d", serverMinimumSchema)) || !strings.Contains(string(output), "found 22") {
		t.Fatalf("worker schema22 startup did not fail early: error=%v output=%s", runErr, output)
	}
	if after := schemaGateSnapshot(t, s); before != after {
		t.Fatal("worker startup implicitly mutated historical schema22")
	}
	t.Logf("main -worker schema22 exit=1 before_provider_listener_worker=true no_implicit_migration=true output=%s", strings.TrimSpace(string(output)))
	historical23, e := os.ReadFile("../../internal/taskengine/migrations/0014_work_slots.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Pool.Exec(ctx, string(historical23)); e != nil {
		t.Fatal(e)
	}
	check(23, false)
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	check(taskengine.SchemaHead(), true)
	// Preserve existing server minimum-version compatibility, not the bridge exact-head policy.
	if _, e := s.Pool.Exec(ctx, `INSERT INTO core.schema_version(version) VALUES($1)`, taskengine.SchemaHead()+1); e != nil {
		t.Fatal(e)
	}
	check(taskengine.SchemaHead()+1, true)
}

// Resolver permissions are opt-in and never implied by operator/agent access.
func TestWorkSettingsResolverPermissionsAndRoutes(t *testing.T) {
	token := "isolated-work-settings-token"
	sum := sha256.Sum256([]byte(token))
	for _, permissions := range []string{`["system.read","system.configure"]`, `["agent.configure"]`} {
		path := writeResolverFixture(t, 0600, fmt.Sprintf(`{"credentials":[{"token_sha256":"%x","workspace_id":"ws_fixture","principal_id":"principal_fixture","permissions":%s}]}`, sum, permissions))
		resolve, e := loadResolver(path)
		if e != nil {
			t.Fatal(e)
		}
		req := httptest.NewRequest("GET", "/v1/operator/system/work-slots", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		actor, e := resolve(req)
		if e != nil {
			t.Fatal(e)
		}
		expected := strings.Contains(permissions, "system.read")
		if actor.Permissions["system.read"] != expected || actor.Permissions["system.configure"] != expected {
			t.Fatal(actor.Permissions)
		}
	}
	for _, path := range []string{"/v1/operator/system/work-slots", "/v1/operator/system/task-work-slots/task_fixture"} {
		for _, method := range []string{"GET", "PUT"} {
			called := false
			h := serverAPISurface(serverRoutes{WorkSettings: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) })})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
			if !called || w.Code != 204 {
				t.Fatal(method, path, w.Code)
			}
		}
	}
}
