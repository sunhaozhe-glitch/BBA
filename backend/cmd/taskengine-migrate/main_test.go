package main

import (
	"bytes"
	"context"
	"fmt"
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

var migrateBinaryPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "taskengine-migrate-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := func() int {
		defer os.RemoveAll(dir)
		bin := filepath.Join(dir, "taskengine-migrate")
		build := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "go build taskengine-migrate: %v\n%s", err, out)
			return 1
		}
		migrateBinaryPath = bin
		return m.Run()
	}()
	os.Exit(code)
}

type fixtureDatabase struct {
	admin *pgx.Conn
	conn  *pgx.Conn
	dsn   string
	name  string
}

func newFixtureDatabase(t *testing.T) *fixtureDatabase {
	t.Helper()
	if os.Getenv("BBA_FIXTURE_ONLY") != "1" {
		t.Fatal("fixture only: use tools/kernel.sh test with an isolated PostgreSQL fixture")
	}
	base := os.Getenv("BBA_FIXTURE_DSN")
	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "/fixture" || cfg.User != "bba_fixture" || cfg.Database != "bba_fixture_admin" {
		t.Fatal("refusing non-fixture database connection")
	}
	ctx := context.Background()
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("migprobe_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	u.Path = "/" + name
	dbDSN := u.String()
	conn, err := pgx.Connect(ctx, dbDSN)
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close(ctx)
		t.Fatal(err)
	}
	db := &fixtureDatabase{admin: admin, conn: conn, dsn: dbDSN, name: name}
	t.Cleanup(func() {
		conn.Close(ctx)
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop fixture database: %v", err)
		}
		admin.Close(ctx)
	})
	return db
}

// migrateEnv returns the child environment with the caller's Task Engine DSN
// stripped so every invocation uses exactly the fixture DSN under test.
func migrateEnv(dsn string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "BBA_TASKENGINE_DSN=") {
			continue
		}
		env = append(env, entry)
	}
	if dsn != "" {
		env = append(env, "BBA_TASKENGINE_DSN="+dsn)
	}
	return env
}

func runMigrate(t *testing.T, dsn string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(migrateBinaryPath, args...)
	cmd.Env = migrateEnv(dsn)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err := cmd.Run()
	return combined.String(), err
}

const userNamespaceFilter = `n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'`

// databaseState renders every user-visible schema object so a refusal can be
// proven inert instead of merely reported.
func databaseState(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		SELECT kind, name FROM (
			SELECT 'schema' AS kind, n.nspname AS name FROM pg_namespace n WHERE `+userNamespaceFilter+`
			UNION ALL
			SELECT 'relation', n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE `+userNamespaceFilter+`
			UNION ALL
			SELECT 'routine', n.nspname || '.' || p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE `+userNamespaceFilter+`
			UNION ALL
			SELECT 'type', n.nspname || '.' || t.typname FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE `+userNamespaceFilter+`
		) objects ORDER BY kind, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, kind+" "+name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// initializedSchemaVersion returns the migration head the command left behind.
// Migrations are additive, so the exact head is asserted as a minimum here and
// the command output is compared against the observed value.
func initializedSchemaVersion(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var version int
	if err := conn.QueryRow(context.Background(), `SELECT max(version) FROM core.schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 15 {
		t.Fatalf("schema version = %d, want at least 15", version)
	}
	return version
}

func schemaExists(t *testing.T, conn *pgx.Conn, name string) bool {
	t.Helper()
	var exists bool
	err := conn.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, name).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

func publicRowCount(t *testing.T, conn *pgx.Conn, table string) int {
	t.Helper()
	var count int
	err := conn.QueryRow(context.Background(), "SELECT count(*) FROM "+pgx.Identifier{"public", table}.Sanitize()).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func requireRefusalUnchanged(t *testing.T, db *fixtureDatabase, before string, out string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("non-fresh database was initialized instead of refused: %s", out)
	}
	if !strings.Contains(out, "refusing") {
		t.Fatalf("refusal message is missing: %s", out)
	}
	if schemaExists(t, db.conn, "core") {
		t.Fatal("refusal created the core schema")
	}
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("refusal changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMigrateEmptyDatabaseInitializesSchema15(t *testing.T) {
	db := newFixtureDatabase(t)
	before := databaseState(t, db.conn)
	out, err := runMigrate(t, db.dsn)
	if err != nil {
		t.Fatalf("fresh empty database must initialize: %v\n%s", err, out)
	}
	version := initializedSchemaVersion(t, db.conn)
	if !strings.Contains(out, fmt.Sprintf("initialized schema=%d", version)) {
		t.Fatalf("unexpected output: %s", out)
	}
	if !schemaExists(t, db.conn, "core") {
		t.Fatal("core schema was not created")
	}
	if databaseState(t, db.conn) == before {
		t.Fatal("initialization left the database empty")
	}
}

func TestMigrateRefusesRepeatInitialization(t *testing.T) {
	db := newFixtureDatabase(t)
	if out, err := runMigrate(t, db.dsn); err != nil {
		t.Fatalf("first initialization failed: %v\n%s", err, out)
	}
	before := databaseState(t, db.conn)
	var versionsBefore int
	if err := db.conn.QueryRow(context.Background(), `SELECT count(*) FROM core.schema_version`).Scan(&versionsBefore); err != nil {
		t.Fatal(err)
	}
	out, err := runMigrate(t, db.dsn)
	if err == nil {
		t.Fatalf("second initialization was accepted: %s", out)
	}
	if !strings.Contains(out, "refusing") {
		t.Fatalf("second initialization did not report a refusal: %s", out)
	}
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("refused repeat initialization changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	var versionsAfter int
	if err := db.conn.QueryRow(context.Background(), `SELECT count(*) FROM core.schema_version`).Scan(&versionsAfter); err != nil {
		t.Fatal(err)
	}
	if versionsBefore != versionsAfter {
		t.Fatalf("schema_version rows changed from %d to %d", versionsBefore, versionsAfter)
	}
}

func TestMigrateRefusesPublicTableWithData(t *testing.T) {
	db := newFixtureDatabase(t)
	if _, err := db.conn.Exec(context.Background(), `CREATE TABLE public.owner_asset(id integer PRIMARY KEY); INSERT INTO public.owner_asset(id) VALUES (7)`); err != nil {
		t.Fatal(err)
	}
	before := databaseState(t, db.conn)
	out, err := runMigrate(t, db.dsn)
	requireRefusalUnchanged(t, db, before, out, err)
	if rows := publicRowCount(t, db.conn, "owner_asset"); rows != 1 {
		t.Fatalf("public.owner_asset rows = %d, want 1", rows)
	}
}

func TestMigrateRefusesCustomSchemaObjects(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"table", `CREATE SCHEMA owner_custom; CREATE TABLE owner_custom.asset(id integer); INSERT INTO owner_custom.asset(id) VALUES (3)`},
		{"empty schema", `CREATE SCHEMA owner_custom`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newFixtureDatabase(t)
			if _, err := db.conn.Exec(context.Background(), tc.sql); err != nil {
				t.Fatal(err)
			}
			before := databaseState(t, db.conn)
			out, err := runMigrate(t, db.dsn)
			requireRefusalUnchanged(t, db, before, out, err)
		})
	}
}

func TestMigrateRefusesPublicObjectsWithoutData(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"empty table", `CREATE TABLE public.owner_asset(id integer)`},
		{"function", `CREATE FUNCTION public.owner_probe() RETURNS integer LANGUAGE sql AS $$ SELECT 1 $$`},
		{"type", `CREATE TYPE public.owner_state AS ENUM ('held')`},
		{"domain", `CREATE DOMAIN public.owner_domain AS text`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newFixtureDatabase(t)
			if _, err := db.conn.Exec(context.Background(), tc.sql); err != nil {
				t.Fatal(err)
			}
			before := databaseState(t, db.conn)
			out, err := runMigrate(t, db.dsn)
			requireRefusalUnchanged(t, db, before, out, err)
		})
	}
}

func TestMigrateRequiresEnvironmentDSN(t *testing.T) {
	out, err := runMigrate(t, "")
	if err == nil {
		t.Fatalf("migration without BBA_TASKENGINE_DSN succeeded: %s", out)
	}
	if !strings.Contains(out, "BBA_TASKENGINE_DSN") {
		t.Fatalf("missing environment DSN error is unclear: %s", out)
	}
}

func TestMigrateRejectsDSNFlag(t *testing.T) {
	db := newFixtureDatabase(t)
	out, err := runMigrate(t, "", "-dsn", db.dsn)
	if err == nil {
		t.Fatalf("-dsn was accepted; the DSN must come only from the environment: %s", out)
	}
	if strings.Contains(out, db.dsn) {
		t.Fatalf("-dsn refusal echoed the connection string: %s", out)
	}
	if schemaExists(t, db.conn, "core") {
		t.Fatal("-dsn invocation created the core schema")
	}
}

func TestMigrateRedactsDSNOnErrors(t *testing.T) {
	const sentinel = "fixture-secret-sentinel"
	dsn := "postgres://owner_user:" + sentinel + "@127.0.0.1:1/owner_db?sslmode=disable&connect_timeout=1"
	out, err := runMigrate(t, dsn)
	if err == nil {
		t.Fatalf("unreachable database was reported as success: %s", out)
	}
	if strings.Contains(out, sentinel) {
		t.Fatalf("error output leaked the DSN password: %s", out)
	}
	if strings.Contains(out, dsn) {
		t.Fatalf("error output leaked the full DSN: %s", out)
	}
}

func TestMigrateChildCommandLineHasNoDSN(t *testing.T) {
	db := newFixtureDatabase(t)
	u, err := url.Parse(db.dsn)
	if err != nil {
		t.Fatal(err)
	}
	const sentinel = "fixture-secret-sentinel"
	u.User = url.UserPassword("bba_fixture", sentinel)
	dsn := u.String()

	ctx := context.Background()
	lockConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Close(ctx)
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(740411)`); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(migrateBinaryPath)
	cmd.Env = migrateEnv(dsn)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		err := lockConn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting)
		if err == nil && waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("migration child never waited on the database lock; output: %s", output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	proc := fmt.Sprintf("/proc/%d/cmdline", cmd.Process.Pid)
	raw, err := os.ReadFile(proc)
	if err != nil {
		t.Fatalf("read %s: %v", proc, err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s is empty", proc)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(args) != 1 {
		t.Fatalf("running migration child exposed arguments: %q", args)
	}
	if filepath.Base(args[0]) != "taskengine-migrate" {
		t.Fatalf("unexpected child executable: %q", args[0])
	}
	commandLine := strings.Join(args, " ")
	for _, forbidden := range []string{dsn, sentinel, "bba_fixture", "-dsn", "postgres://"} {
		if strings.Contains(commandLine, forbidden) {
			t.Fatalf("child command line contains %q: %q", forbidden, commandLine)
		}
	}

	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_unlock(740411)`); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		waited = true
		t.Fatalf("migration after unlocking failed: %v\n%s", err, output.String())
	}
	waited = true
	version := initializedSchemaVersion(t, db.conn)
	if !strings.Contains(output.String(), fmt.Sprintf("initialized schema=%d", version)) {
		t.Fatalf("unexpected child output after lock release: %s", output.String())
	}
}

// applyMigrationSQL builds an older, real schema by applying the kernel's own
// migration files up to an earlier version. It is only a precondition: the
// upgrade path itself is always the command under test.
func applyMigrationSQL(t *testing.T, db *fixtureDatabase, names ...string) int {
	t.Helper()
	ctx := context.Background()
	for _, name := range names {
		path := filepath.Join("..", "..", "internal", "taskengine", "migrations", name)
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := db.conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
	var version int
	if err := db.conn.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&version); err != nil {
		t.Fatalf("read applied schema version: %v", err)
	}
	return version
}

func requireSchemaVersion(t *testing.T, conn *pgx.Conn, want int) {
	t.Helper()
	var version int
	if err := conn.QueryRow(context.Background(), `SELECT max(version) FROM core.schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != want {
		t.Fatalf("schema version = %d, want %d", version, want)
	}
}

// TestMigrateDefaultModeNeverUpgradesExistingSchema pins the init-only default:
// an existing schema is refused, not silently advanced.
func TestMigrateDefaultModeNeverUpgradesExistingSchema(t *testing.T) {
	db := newFixtureDatabase(t)
	if version := applyMigrationSQL(t, db, "0001_core.sql"); version != 10 {
		t.Fatalf("precondition schema version = %d, want 10", version)
	}
	before := databaseState(t, db.conn)
	out, err := runMigrate(t, db.dsn)
	if err == nil {
		t.Fatalf("default mode upgraded an existing schema: %s", out)
	}
	if !strings.Contains(out, "refusing") {
		t.Fatalf("default mode refusal is unclear: %s", out)
	}
	requireSchemaVersion(t, db.conn, 10)
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("refused default mode changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMigrateUpgradeRequiresExpectedVersion(t *testing.T) {
	db := newFixtureDatabase(t)
	applyMigrationSQL(t, db, "0001_core.sql")
	before := databaseState(t, db.conn)
	out, err := runMigrate(t, db.dsn, "-upgrade")
	if err == nil {
		t.Fatalf("upgrade without -expected-version was accepted: %s", out)
	}
	if !strings.Contains(out, "-expected-version") {
		t.Fatalf("missing -expected-version error is unclear: %s", out)
	}
	requireSchemaVersion(t, db.conn, 10)
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("refused upgrade changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMigrateUpgradeReachesRegisteredHead(t *testing.T) {
	db := newFixtureDatabase(t)
	if version := applyMigrationSQL(t, db, "0001_core.sql"); version != 10 {
		t.Fatalf("precondition schema version = %d, want 10", version)
	}
	head := taskengine.SchemaHead()
	out, err := runMigrate(t, db.dsn, "-upgrade", "-expected-version", "10")
	if err != nil {
		t.Fatalf("upgrade from 10 failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, fmt.Sprintf("upgraded from=10 to=%d", head)) {
		t.Fatalf("unexpected upgrade output: %s", out)
	}
	requireSchemaVersion(t, db.conn, head)
	if !schemaExists(t, db.conn, "core") {
		t.Fatal("core schema is missing after upgrade")
	}
	if head < 19 {
		t.Fatalf("registered head = %d, want at least the proposal work schema (19)", head)
	}
}

func TestMigrateUpgradeRefusesWrongExpectedVersion(t *testing.T) {
	db := newFixtureDatabase(t)
	applyMigrationSQL(t, db, "0001_core.sql")
	before := databaseState(t, db.conn)
	out, err := runMigrate(t, db.dsn, "-upgrade", "-expected-version", "11")
	if err == nil {
		t.Fatalf("upgrade with a wrong expected version was accepted: %s", out)
	}
	if !strings.Contains(out, "refusing upgrade") || !strings.Contains(out, "does not match expected 11") {
		t.Fatalf("wrong expected version refusal is unclear: %s", out)
	}
	requireSchemaVersion(t, db.conn, 10)
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("refused upgrade changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMigrateUpgradeRefusesEmptyDatabase(t *testing.T) {
	db := newFixtureDatabase(t)
	before := databaseState(t, db.conn)
	out, err := runMigrate(t, db.dsn, "-upgrade", "-expected-version", "10")
	if err == nil {
		t.Fatalf("upgrade initialized an empty database: %s", out)
	}
	if !strings.Contains(out, "no Task Engine schema") {
		t.Fatalf("empty database upgrade refusal is unclear: %s", out)
	}
	if schemaExists(t, db.conn, "core") {
		t.Fatal("refused upgrade created the core schema")
	}
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("refused empty-database upgrade changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMigrateUpgradeAtHeadIsIdempotent(t *testing.T) {
	db := newFixtureDatabase(t)
	if out, err := runMigrate(t, db.dsn); err != nil {
		t.Fatalf("initialization failed: %v\n%s", err, out)
	}
	head := initializedSchemaVersion(t, db.conn)
	var rowsBefore int
	if err := db.conn.QueryRow(context.Background(), `SELECT count(*) FROM core.schema_version`).Scan(&rowsBefore); err != nil {
		t.Fatal(err)
	}
	out, err := runMigrate(t, db.dsn, "-upgrade", "-expected-version", fmt.Sprintf("%d", head))
	if err != nil {
		t.Fatalf("upgrade already at head failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, fmt.Sprintf("already_at_head schema=%d", head)) {
		t.Fatalf("unexpected already-at-head output: %s", out)
	}
	var rowsAfter int
	if err := db.conn.QueryRow(context.Background(), `SELECT count(*) FROM core.schema_version`).Scan(&rowsAfter); err != nil {
		t.Fatal(err)
	}
	if rowsAfter != rowsBefore {
		t.Fatalf("already-at-head run wrote schema_version rows: %d -> %d", rowsBefore, rowsAfter)
	}
}

func TestMigrateUpgradeRefusesUnexpectedRegisteredHead(t *testing.T) {
	db := newFixtureDatabase(t)
	applyMigrationSQL(t, db, "0001_core.sql")
	before := databaseState(t, db.conn)
	head := taskengine.SchemaHead()
	out, err := runMigrate(t, db.dsn, "-upgrade", "-expected-version", "10", "-expect-head", fmt.Sprintf("%d", head+1))
	if err == nil {
		t.Fatalf("upgrade with an unexpected build head was accepted: %s", out)
	}
	if !strings.Contains(out, "registered head") || !strings.Contains(out, "does not match expected head") {
		t.Fatalf("head mismatch refusal is unclear: %s", out)
	}
	requireSchemaVersion(t, db.conn, 10)
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("refused head mismatch changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMigrateUpgradeRefusesHeldLease(t *testing.T) {
	db := newFixtureDatabase(t)
	applyMigrationSQL(t, db, "0001_core.sql")
	ctx := context.Background()
	lockConn, err := pgx.Connect(ctx, db.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Close(ctx)
	if _, err := lockConn.Exec(ctx, fmt.Sprintf(`SELECT pg_advisory_lock(%d)`, upgradeLeaseKey)); err != nil {
		t.Fatal(err)
	}
	defer lockConn.Exec(ctx, fmt.Sprintf(`SELECT pg_advisory_unlock(%d)`, upgradeLeaseKey)) //nolint:errcheck
	out, err := runMigrate(t, db.dsn, "-upgrade", "-expected-version", "10")
	if err == nil {
		t.Fatalf("upgrade ran while the operator lease was held: %s", out)
	}
	if !strings.Contains(out, "upgrade lease") {
		t.Fatalf("held lease refusal is unclear: %s", out)
	}
	requireSchemaVersion(t, db.conn, 10)
}

func TestMigrateCheckIsReadOnly(t *testing.T) {
	db := newFixtureDatabase(t)
	applyMigrationSQL(t, db, "0001_core.sql")
	before := databaseState(t, db.conn)
	head := taskengine.SchemaHead()
	out, err := runMigrate(t, db.dsn, "-check", "-expected-version", "10", "-expect-head", fmt.Sprintf("%d", head))
	if err != nil {
		t.Fatalf("check at the expected version failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, fmt.Sprintf("check schema=10 expected_version=10 head=%d upgrade_required=true result=match", head)) {
		t.Fatalf("unexpected check output: %s", out)
	}
	requireSchemaVersion(t, db.conn, 10)
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("check changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMigrateCheckReportsMismatch(t *testing.T) {
	db := newFixtureDatabase(t)
	applyMigrationSQL(t, db, "0001_core.sql")
	before := databaseState(t, db.conn)
	out, err := runMigrate(t, db.dsn, "-check", "-expected-version", "11")
	if err == nil {
		t.Fatalf("check with a mismatched version succeeded: %s", out)
	}
	if !strings.Contains(out, "result=mismatch") {
		t.Fatalf("mismatch check output is unclear: %s", out)
	}
	requireSchemaVersion(t, db.conn, 10)
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("failed check changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMigrateCheckReportsEmptyDatabase(t *testing.T) {
	db := newFixtureDatabase(t)
	before := databaseState(t, db.conn)
	out, err := runMigrate(t, db.dsn, "-check", "-expected-version", "10")
	if err == nil {
		t.Fatalf("check on an empty database succeeded: %s", out)
	}
	if !strings.Contains(out, "check schema=none expected_version=10") || !strings.Contains(out, "result=empty") {
		t.Fatalf("empty check output is unclear: %s", out)
	}
	if schemaExists(t, db.conn, "core") {
		t.Fatal("check created the core schema")
	}
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("check changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMigrateRejectsVersionFlagsInInitMode(t *testing.T) {
	db := newFixtureDatabase(t)
	before := databaseState(t, db.conn)
	out, err := runMigrate(t, db.dsn, "-expected-version", "10")
	if err == nil {
		t.Fatalf("init mode accepted a version flag: %s", out)
	}
	if !strings.Contains(out, "require -upgrade or -check") {
		t.Fatalf("init mode flag refusal is unclear: %s", out)
	}
	if schemaExists(t, db.conn, "core") {
		t.Fatal("init mode with a version flag created the core schema")
	}
	if after := databaseState(t, db.conn); after != before {
		t.Fatalf("init mode with a version flag changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMigrateRejectsUpgradeAndCheckTogether(t *testing.T) {
	db := newFixtureDatabase(t)
	out, err := runMigrate(t, db.dsn, "-upgrade", "-check", "-expected-version", "10")
	if err == nil {
		t.Fatalf("-upgrade -check together was accepted: %s", out)
	}
	if !strings.Contains(out, "mutually exclusive") {
		t.Fatalf("mutual exclusion error is unclear: %s", out)
	}
	if schemaExists(t, db.conn, "core") {
		t.Fatal("conflicting flags created the core schema")
	}
}
