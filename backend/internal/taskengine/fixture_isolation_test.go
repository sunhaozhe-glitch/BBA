package taskengine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Never clone here: execute every production migration on an independent empty
// database, check idempotence and compare all application table columns and rows
// with an unseeded clone. Historical migration tests remain unchanged.
func TestFixtureEmptyDatabaseFullMigration(t *testing.T) {
	ctx := context.Background()
	cfg := fixtureAdminConfig(t)
	empty := fixtureDatabase(t, cfg, "", false)
	if scalarInt(t, empty, `SELECT count(*) FROM pg_namespace WHERE nspname IN ('core','bridge')`) != 0 {
		t.Fatal("not an empty database")
	}
	if err := empty.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if scalarInt(t, empty, `SELECT max(version) FROM core.schema_version`) != migrationHead {
		t.Fatal("head mismatch")
	}
	before := fixtureSchemaAndRows(t, empty, false)
	if err := empty.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixtureSchemaAndRows(t, empty, false)) {
		t.Fatal("full migration not idempotent")
	}
	template, err := ensureFixtureTemplate(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	clone := fixtureDatabase(t, cfg, template, false)
	// Only migration-seed wall timestamps differ between independently migrated DBs.
	before = fixtureSchemaAndRows(t, empty, true)
	after := fixtureSchemaAndRows(t, clone, true)
	if !reflect.DeepEqual(before, after) {
		for k, v := range before {
			if v != after[k] {
				t.Errorf("schema/data mismatch at %s", k)
			}
		}
		t.Fatal("empty migration and template schema/data differ")
	}
	t.Logf("independent empty database migrated to %d, idempotent; schema and all application rows match clone", migrationHead)
}

func fixtureSchemaAndRows(t *testing.T, s *Store, normalizeSeedTimes bool) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	// information_schema normalizes database-local OIDs while preserving column
	// types, order, defaults and nullability. Data includes migration seed rows.
	var columns string
	if err := s.Pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(c) - ARRAY['table_catalog','udt_catalog','domain_catalog','scope_catalog','collation_catalog','character_set_catalog'] ORDER BY table_schema,table_name,ordinal_position),'[]')::text FROM information_schema.columns c WHERE table_schema IN ('core','bridge')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	out["columns"] = columns
	rows, err := s.Pool.Query(ctx, `SELECT schemaname,tablename FROM pg_tables WHERE schemaname IN ('core','bridge') ORDER BY 1,2`)
	if err != nil {
		t.Fatal(err)
	}
	var tables [][2]string
	for rows.Next() {
		var schema, table string
		if err := rows.Scan(&schema, &table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, [2]string{schema, table})
	}
	rows.Close()
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	for _, table := range tables {
		var data string
		value := "to_jsonb(x)"
		if normalizeSeedTimes && table[0]+"."+table[1] == "core.schema_version" {
			value += " - 'applied_at'"
		}
		if normalizeSeedTimes && table[0]+"."+table[1] == "core.runtime_credential_population" {
			value += " - 'changed_at'"
		}
		if err := s.Pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(`+value+` ORDER BY to_jsonb(x)::text),'[]')::text FROM `+pgx.Identifier{table[0], table[1]}.Sanitize()+` x`).Scan(&data); err != nil {
			t.Fatal(err)
		}
		out[table[0]+"."+table[1]] = data
	}
	return out
}

func TestFixtureTemplateFrozen(t *testing.T) {
	ctx := context.Background()
	cfg := fixtureAdminConfig(t)
	template, err := ensureFixtureTemplate(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var allowed bool
	var sessions int
	var readOnly bool
	if err := admin.QueryRow(ctx, `SELECT datallowconn,(SELECT count(*) FROM pg_stat_activity WHERE datname=$1),EXISTS(SELECT 1 FROM pg_db_role_setting WHERE setdatabase=pg_database.oid AND 'default_transaction_read_only=on'=ANY(setconfig)) FROM pg_database WHERE datname=$1`, template).Scan(&allowed, &sessions, &readOnly); err != nil {
		t.Fatal(err)
	}
	if allowed || sessions != 0 || !readOnly {
		t.Fatalf("freeze: allowed=%v sessions=%d readOnly=%v", allowed, sessions, readOnly)
	}
	c := cfg.Copy()
	c.Database = template
	conn, err := pgx.ConnectConfig(ctx, c)
	if conn != nil {
		conn.Close(ctx)
	}
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "55000" {
		t.Fatalf("template connection must be refused, got %v", err)
	}
	s := fixtureStore(t)
	if scalarInt(t, s, `SELECT count(*) FROM core.account`) != 0 {
		t.Fatal("template has business data")
	}
	if _, err := s.Pool.Exec(ctx, `CREATE TABLE public.clone_writable(id int)`); err != nil {
		t.Fatal("clone is not writable", err)
	}
	t.Logf("template %s rejects connections (55000), sessions=0, read-only default; clone writable", template)
}

func TestFixtureParallelDataConnectionIsolation(t *testing.T) {
	var mu sync.Mutex
	dbs := map[string]bool{}
	pids := map[int]bool{}
	ready := make(chan struct{})
	arrived := 0
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				s := fixtureStore(t)
				ctx := context.Background()
				var db string
				var pid int
				conn, err := s.Pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Release()
				if err = conn.QueryRow(ctx, `SELECT current_database(),pg_backend_pid()`).Scan(&db, &pid); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				duplicate := dbs[db] || pids[pid]
				dbs[db] = true
				pids[pid] = true
				mu.Unlock()
				if duplicate {
					t.Fatal("shared database or connection")
				}
				// Identical DDL, primary key and sequence names must succeed in every clone.
				if _, err = conn.Exec(ctx, `CREATE TABLE public.fixture_isolation(id serial PRIMARY KEY, value text); INSERT INTO public.fixture_isolation(value) VALUES('private'); INSERT INTO core.workspace(id) VALUES('same_in_every_clone'); SET application_name='private_clone'`); err != nil {
					t.Fatal(err)
				}
				var id int
				if err = conn.QueryRow(ctx, `SELECT id FROM public.fixture_isolation`).Scan(&id); err != nil || id != 1 {
					t.Fatalf("sequence isolation id=%d err=%v", id, err)
				}
				if scalarInt(t, s, `SELECT count(*) FROM public.fixture_isolation`) != 1 || scalarInt(t, s, `SELECT count(*) FROM core.workspace`) != 2 {
					t.Fatal("data leaked")
				}
				// Keep one session lock until cleanup; same key must not block other DBs.
				var locked bool
				if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(740499)`).Scan(&locked); err != nil || !locked {
					t.Fatalf("lock isolation: %v %v", locked, err)
				}
				defer conn.Exec(ctx, `SELECT pg_advisory_unlock(740499)`)
				// All eight independent databases and locks must coexist before any release.
				mu.Lock()
				arrived++
				if arrived == 8 {
					close(ready)
				}
				mu.Unlock()
				select {
				case <-ready:
				case <-time.After(10 * time.Second):
					t.Fatal("parallel clone rendezvous timed out")
				}
				t.Logf("private database=%s pid=%d rows=1 sequence=1", db, pid)
			})
		}(i)
	}
	workers.Wait()
}

func TestFixtureLaterClonePristine(t *testing.T) {
	t.Run("mutate", func(t *testing.T) {
		s := fixtureStore(t)
		if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.workspace(id) VALUES('must_not_leak'); CREATE TABLE public.must_not_leak(id int)`); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("fresh", func(t *testing.T) {
		s := fixtureStore(t)
		if scalarInt(t, s, `SELECT count(*) FROM core.workspace`) != 1 || scalarInt(t, s, `SELECT count(*) FROM pg_tables WHERE tablename='must_not_leak'`) != 0 {
			t.Fatal("prior test leaked data/schema")
		}
	})
}

func TestFixtureMigrationFingerprintInvalidation(t *testing.T) {
	base := func() fstest.MapFS {
		return fstest.MapFS{"migrations/0001.sql": &fstest.MapFile{Data: []byte("original")}}
	}
	fingerprint := func(files fstest.MapFS, head int, tail []string) string {
		t.Helper()
		s, err := fixtureMigrationFingerprint(files, head, tail)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	original := fingerprint(base(), 24, []string{"0001.sql"})
	if fingerprint(base(), 24, []string{"0001.sql"}) != original {
		t.Fatal("unstable fingerprint")
	}
	cases := map[string]func() (fstest.MapFS, int, []string){
		"sql_content": func() (fstest.MapFS, int, []string) {
			f := base()
			f["migrations/0001.sql"].Data = []byte("changed")
			return f, 24, []string{"0001.sql"}
		},
		"added_file": func() (fstest.MapFS, int, []string) {
			f := base()
			f["migrations/new.sql"] = &fstest.MapFile{Data: []byte("new")}
			return f, 24, []string{"0001.sql"}
		},
		"renamed_file": func() (fstest.MapFS, int, []string) {
			f := base()
			f["migrations/renamed.sql"] = f["migrations/0001.sql"]
			delete(f, "migrations/0001.sql")
			return f, 24, []string{"0001.sql"}
		},
		"head":  func() (fstest.MapFS, int, []string) { return base(), 25, []string{"0001.sql"} },
		"order": func() (fstest.MapFS, int, []string) { return base(), 24, []string{"other.sql", "0001.sql"} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, head, tail := change()
			if fingerprint(f, head, tail) == original {
				t.Fatal("stale fingerprint")
			}
		})
	}
	// Fingerprint is diagnostic, not a reusable cache key: even unchanged SQL
	// uses a new random template per executable. Changes to Migrate itself are
	// therefore also exercised on every new process, not hidden by a disk cache.
}
