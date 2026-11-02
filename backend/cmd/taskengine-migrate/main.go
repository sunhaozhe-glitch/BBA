package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"bba.local/taskengine/internal/taskengine"
	"github.com/jackc/pgx/v5/pgxpool"
)

const dsnEnvironment = "BBA_TASKENGINE_DSN"

// minimumSchemaVersion is the lowest migration head the init path may leave
// behind. Migrations are additive, so a later head is reported as-is.
const minimumSchemaVersion = 15

// upgradeLeaseKey is the operator upgrade lease. The kernel migration
// transaction holds pg_advisory_xact_lock(740411) while it applies migrations;
// this command additionally holds 740412 from the version compare through the
// final verification, so two operator upgrades can never interleave their
// precondition CAS with each other. The lease is a session lock on one
// dedicated connection; it is released before the process exits.
const upgradeLeaseKey = 740412

var (
	upgradeFlag     = flag.Bool("upgrade", false, "upgrade an existing Task Engine schema to this build's registered head")
	checkFlag       = flag.Bool("check", false, "read-only schema precondition check; never writes")
	expectedVersion = flag.Int("expected-version", -1, "exact schema version currently recorded in core.schema_version (required with -upgrade or -check)")
	expectHead      = flag.Int("expect-head", 0, "optional exact registered head this build must have (0 = any)")
)

var (
	urlCredentialPattern = regexp.MustCompile(`(?i)(postgres(?:ql)?://[^/\s:@]+:)[^@\s/]*@`)
	passwordFieldPattern = regexp.MustCompile(`(?i)(\bpassword\s*=\s*)(?:'[^']*'|"[^"]*"|[^\s]+)`)
)

// options is the parsed, validated command mode. The default (no flags) is the
// original init-only behavior: initialize exactly one brand-new database.
type options struct {
	upgrade         bool
	check           bool
	expectedVersion int
	expectHead      int
}

func main() {
	flag.Parse()
	opts, err := parseOptions(*upgradeFlag, *checkFlag, *expectedVersion, *expectHead, flag.NArg())
	if err != nil {
		log.Fatal(err)
	}
	dsn := strings.TrimSpace(os.Getenv(dsnEnvironment))
	if dsn == "" {
		log.Fatalf("%s is required", dsnEnvironment)
	}
	ctx := context.Background()
	switch {
	case opts.check:
		err = checkSchema(ctx, dsn, opts)
	case opts.upgrade:
		err = upgradeSchema(ctx, dsn, opts)
	default:
		err = initialize(ctx, dsn)
	}
	if err != nil {
		log.Fatal(redactSecrets(err.Error(), dsn))
	}
}

// parseOptions keeps the flag contract explicit: an upgrade or a check must
// name the exact version it expects to find, and version flags are rejected in
// the init-only mode so a typo can never silently initialize or upgrade.
func parseOptions(upgrade, check bool, expected, head, positionals int) (options, error) {
	opts := options{upgrade: upgrade, check: check, expectedVersion: expected, expectHead: head}
	if positionals != 0 {
		return opts, errors.New("unexpected positional arguments; provide the database connection string via " + dsnEnvironment)
	}
	if upgrade && check {
		return opts, errors.New("-upgrade and -check are mutually exclusive")
	}
	if head < 0 {
		return opts, errors.New("-expect-head cannot be negative")
	}
	if !upgrade && !check {
		if expected >= 0 || head != 0 {
			return opts, errors.New("-expected-version and -expect-head require -upgrade or -check")
		}
		return opts, nil
	}
	if expected < 1 {
		return opts, errors.New("-expected-version must be the positive schema version currently recorded in core.schema_version")
	}
	return opts, nil
}

// initialize accepts only a brand-new database: every user schema other than
// the default empty public schema, and every object inside public, blocks the
// migration before any write is attempted.
func initialize(ctx context.Context, dsn string) error {
	store, err := taskengine.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open empty Task Engine database: %w", err)
	}
	defer store.Close()
	if err := validateFreshDatabase(ctx, store.Pool); err != nil {
		return err
	}
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}
	var version int
	if err := store.Pool.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("verify schema version: %w", err)
	}
	if version < minimumSchemaVersion {
		return fmt.Errorf("initialized schema version %d, want at least %d", version, minimumSchemaVersion)
	}
	emit("initialized schema=%d", version)
	return nil
}

// upgradeSchema advances an existing database from exactly the declared
// current version to this build's registered head. Every refusal happens
// before the first write: a missing schema, a version other than the declared
// one, an unexpected build head, or a lease held by another operator all leave
// the database untouched.
func upgradeSchema(ctx context.Context, dsn string, opts options) error {
	head := taskengine.SchemaHead()
	if opts.expectHead > 0 && opts.expectHead != head {
		return fmt.Errorf("refusing upgrade: registered head %d does not match expected head %d (wrong build for this deployment)", head, opts.expectHead)
	}
	store, err := taskengine.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open Task Engine database: %w", err)
	}
	defer store.Close()
	lease, err := acquireUpgradeLease(ctx, store)
	if err != nil {
		return err
	}
	defer lease.release(ctx)

	version, exists, err := observedSchemaVersion(ctx, lease.conn)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("refusing upgrade: database has no Task Engine schema; initialize an empty database with the init-only default (no -upgrade)")
	}
	if version != opts.expectedVersion {
		return fmt.Errorf("refusing upgrade: schema version %d does not match expected %d (registered head %d); verify with -check before retrying", version, opts.expectedVersion, head)
	}
	if version > head {
		return fmt.Errorf("refusing upgrade: schema version %d is newer than this build's registered head %d", version, head)
	}
	if version == head {
		emit("already_at_head schema=%d expected_version=%d head=%d", version, opts.expectedVersion, head)
		return nil
	}
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("upgrade schema %d->%d: %w", version, head, err)
	}
	after, exists, err := observedSchemaVersion(ctx, lease.conn)
	if err != nil {
		return err
	}
	if !exists || after != head {
		return fmt.Errorf("upgrade ended at schema %d (present=%t), want registered head %d", after, exists, head)
	}
	emit("upgraded from=%d to=%d expected_version=%d head=%d", version, after, opts.expectedVersion, head)
	return nil
}

// checkSchema reports whether the database is exactly at the declared version
// and whether this build is the expected head. It never writes: an empty
// database stays empty and an older schema stays older.
func checkSchema(ctx context.Context, dsn string, opts options) error {
	head := taskengine.SchemaHead()
	store, err := taskengine.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open Task Engine database: %w", err)
	}
	defer store.Close()
	lease, err := acquireUpgradeLease(ctx, store)
	if err != nil {
		return err
	}
	defer lease.release(ctx)
	version, exists, err := observedSchemaVersion(ctx, lease.conn)
	if err != nil {
		return err
	}
	result := "match"
	switch {
	case !exists:
		result = "empty"
	case version != opts.expectedVersion:
		result = "mismatch"
	case opts.expectHead > 0 && opts.expectHead != head:
		result = "head_mismatch"
	}
	schema := "none"
	upgradeRequired := false
	if exists {
		schema = strconv.Itoa(version)
		upgradeRequired = version < head
	}
	emit("check schema=%s expected_version=%d head=%d upgrade_required=%t result=%s", schema, opts.expectedVersion, head, upgradeRequired, result)
	if result != "match" {
		return fmt.Errorf("schema check failed: result=%s (schema=%s expected_version=%d head=%d)", result, schema, opts.expectedVersion, head)
	}
	return nil
}

// acquireUpgradeLease fails fast instead of blocking: an operator who finds the
// lease held must look at the other run rather than queue a second upgrade.
func acquireUpgradeLease(ctx context.Context, store *taskengine.Store) (*upgradeLease, error) {
	conn, err := store.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire upgrade lease connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, upgradeLeaseKey).Scan(&locked); err != nil {
		conn.Release()
		return nil, fmt.Errorf("acquire upgrade lease: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, fmt.Errorf("refusing: upgrade lease %d is held by another process; wait for it to finish", upgradeLeaseKey)
	}
	return &upgradeLease{conn: conn}, nil
}

type upgradeLease struct{ conn *pgxpool.Conn }

func (l *upgradeLease) release(ctx context.Context) {
	_, _ = l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, upgradeLeaseKey)
	l.conn.Release()
}

// observedSchemaVersion reads the recorded head under the caller's lease. The
// boolean is false for a database that never ran a migration.
func observedSchemaVersion(ctx context.Context, conn *pgxpool.Conn) (int, bool, error) {
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('core.schema_version') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, false, fmt.Errorf("check Task Engine schema: %w", err)
	}
	if !exists {
		return 0, false, nil
	}
	var version int
	if err := conn.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&version); err != nil {
		return 0, false, fmt.Errorf("read schema version: %w", err)
	}
	return version, true, nil
}

func emit(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
	_ = os.Stdout.Sync()
}

func validateFreshDatabase(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `
		SELECT nspname
		FROM pg_namespace
		WHERE nspname NOT IN ('pg_catalog', 'information_schema')
		  AND nspname NOT LIKE 'pg_toast%'
		  AND nspname NOT LIKE 'pg_temp%'
		  AND nspname <> 'public'
		ORDER BY nspname`)
	if err != nil {
		return fmt.Errorf("check empty database schemas: %w", err)
	}
	var schemas []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("check empty database schemas: %w", err)
		}
		schemas = append(schemas, name)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("check empty database schemas: %w", err)
	}
	if len(schemas) > 0 {
		return fmt.Errorf("refusing to initialize non-empty database: unexpected schema(s) %s", strings.Join(schemas, ", "))
	}
	var objects int
	err = pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM pg_class o JOIN pg_namespace n ON n.oid = o.relnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_proc o JOIN pg_namespace n ON n.oid = o.pronamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_type o JOIN pg_namespace n ON n.oid = o.typnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_collation o JOIN pg_namespace n ON n.oid = o.collnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_conversion o JOIN pg_namespace n ON n.oid = o.connamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_operator o JOIN pg_namespace n ON n.oid = o.oprnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_opclass o JOIN pg_namespace n ON n.oid = o.opcnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_opfamily o JOIN pg_namespace n ON n.oid = o.opfnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_ts_config o JOIN pg_namespace n ON n.oid = o.cfgnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_ts_dict o JOIN pg_namespace n ON n.oid = o.dictnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_ts_parser o JOIN pg_namespace n ON n.oid = o.prsnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_ts_template o JOIN pg_namespace n ON n.oid = o.tmplnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_statistic_ext o JOIN pg_namespace n ON n.oid = o.stxnamespace WHERE n.nspname = 'public')
		     + (SELECT count(*) FROM pg_extension o JOIN pg_namespace n ON n.oid = o.extnamespace WHERE n.nspname = 'public')`).Scan(&objects)
	if err != nil {
		return fmt.Errorf("check public schema objects: %w", err)
	}
	if objects > 0 {
		return fmt.Errorf("refusing to initialize non-empty database: public schema contains %d object(s)", objects)
	}
	return nil
}

// redactSecrets keeps the connection string and any password it carries out of
// logs, including errors that echo a malformed DSN.
func redactSecrets(message, dsn string) string {
	for _, secret := range append([]string{dsn}, dsnSecrets(dsn)...) {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	message = urlCredentialPattern.ReplaceAllString(message, "$1[redacted]@")
	message = passwordFieldPattern.ReplaceAllString(message, "$1[redacted]")
	return message
}

func dsnSecrets(dsn string) []string {
	var secrets []string
	if parsed, err := url.Parse(dsn); err == nil && parsed.User != nil {
		if password, ok := parsed.User.Password(); ok {
			secrets = append(secrets, password)
			if decoded, err := url.QueryUnescape(password); err == nil && decoded != password {
				secrets = append(secrets, decoded)
			}
		}
	}
	for _, field := range strings.Fields(dsn) {
		const prefix = "password="
		if len(field) <= len(prefix) || !strings.EqualFold(field[:len(prefix)], prefix) {
			continue
		}
		value := strings.Trim(field[len(prefix):], `'"`)
		secrets = append(secrets, value)
		if decoded, err := url.QueryUnescape(value); err == nil && decoded != value {
			secrets = append(secrets, decoded)
		}
	}
	return secrets
}
