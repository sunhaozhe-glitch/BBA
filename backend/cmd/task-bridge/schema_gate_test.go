package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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

func TestB4GatewaySchemaStartupPolicy(t *testing.T) {
	s := schemaGateFixture(t)
	ctx := context.Background()
	check := func(version int, allowed bool) {
		t.Helper()
		var got int
		if e := s.Pool.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&got); e != nil || got != version {
			t.Fatalf("schema=%d got=%d error=%v", version, got, e)
		}
		before := schemaGateSnapshot(t, s)
		e := validateGatewaySchema(s)
		if (e == nil) != allowed {
			t.Fatalf("schema=%d allowed=%v error=%v", version, allowed, e)
		}
		if after := schemaGateSnapshot(t, s); before != after {
			t.Fatal("startup implicitly migrated schema")
		}
		t.Logf("schema=%d allowed=%v no_implicit_migration=true", version, allowed)
	}
	check(22, false)
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	check(taskengine.SchemaHead(), true)
	// Synthetic future ledger in this disposable DB only. Gateway binds exact SchemaHead.
	if _, e := s.Pool.Exec(ctx, `INSERT INTO core.schema_version(version) VALUES($1)`, taskengine.SchemaHead()+1); e != nil {
		t.Fatal(e)
	}
	check(taskengine.SchemaHead()+1, false)
}
