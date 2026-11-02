// Command product-fixture builds the single isolated product fixture used by
// the console acceptance run.
//
//	product-fixture -fixture -mode seed -output seed.json
//	product-fixture -fixture -mode scenario [-artex-fixture] -output scenario.json
//
// It requires BBA_PRODUCT_FIXTURE=1, an explicit -fixture acknowledgement and a
// database whose user/database/host match the isolated fixture contract. Seed
// refuses an existing workspace, never touches group summaries and produces IDs,
// versions and digests only. Scenario drives the real proposal and graph chains
// through the public Store/Worker APIs with fake adapter and ARTEX protocol
// boundaries; it never creates a second sender, scheduler or external client.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"bba.local/taskengine/internal/taskengine"
)

const (
	workspaceID           = "ws_fixture"
	operatorPrincipalID   = "product-operator"
	readOnlyPrincipalID   = "product-reader"
	productFixtureEnv     = "BBA_PRODUCT_FIXTURE"
	taskEngineDSNEnv      = "BBA_TASKENGINE_DSN"
	expectedSchemaVersion = 21

	fixtureJoinReason          = "fixture product join reason"
	fixtureVerificationMessage = "fixture product verification"
)

type result struct {
	OK            bool            `json:"ok"`
	Mode          string          `json:"mode"`
	Fixture       bool            `json:"fixture"`
	SchemaVersion int             `json:"schema_version,omitempty"`
	Error         string          `json:"error,omitempty"`
	Seed          *seedResult     `json:"seed,omitempty"`
	Scenario      *scenarioResult `json:"scenario,omitempty"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("product-fixture", flag.ContinueOnError)
	flags.SetOutput(stderr)
	fixture := flags.Bool("fixture", false, "required: acknowledge that the target database is an isolated fixture")
	mode := flags.String("mode", "seed", "seed or scenario")
	output := flags.String("output", "", "optional path to also write the JSON result")
	artexFixture := flags.Bool("artex-fixture", false, "scenario only: run the ARTEX graph chain against the in-process protocol fixture")
	requireCollectorSummary := flags.Bool("require-collector-summary", false,
		"scenario graph: require an existing live collector Summary for the task target instead of ingesting fixture data")
	timeout := flags.Duration("timeout", 5*time.Minute, "overall command timeout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "product-fixture: unexpected arguments %q\n", flags.Args())
		return 2
	}
	res := &result{Fixture: true, Mode: *mode}
	fail := func(err error) int {
		res.OK = false
		res.Error = err.Error()
		if writeErr := writeResult(*output, stdout, res); writeErr != nil {
			fmt.Fprintf(stderr, "product-fixture: %v\n", writeErr)
		}
		fmt.Fprintf(stderr, "product-fixture %s FAIL: %v\n", *mode, err)
		return 1
	}
	if !*fixture || strings.TrimSpace(os.Getenv(productFixtureEnv)) != "1" {
		return fail(fmt.Errorf("%s=1 and -fixture are required", productFixtureEnv))
	}
	if *mode != "seed" && *mode != "scenario" {
		return fail(errors.New("-mode must be seed or scenario"))
	}
	dsn := strings.TrimSpace(os.Getenv(taskEngineDSNEnv))
	if err := validateFixtureDSN(dsn); err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	store, err := taskengine.Open(ctx, dsn)
	if err != nil {
		return fail(fmt.Errorf("open fixture database: %w", err))
	}
	defer store.Close()
	requireCollector := *requireCollectorSummary || strings.TrimSpace(os.Getenv("BBA_PRODUCT_REQUIRE_COLLECTOR_SUMMARY")) == "1"
	artexChain := *artexFixture || strings.TrimSpace(os.Getenv("BBA_PRODUCT_FIXTURE_ARTEX")) == "1" || requireCollector
	if err := execute(ctx, store, *mode, artexChain, requireCollector, res); err != nil {
		return fail(err)
	}
	res.OK = true
	if err := writeResult(*output, stdout, res); err != nil {
		fmt.Fprintf(stderr, "product-fixture: %v\n", err)
		return 1
	}
	return 0
}

func execute(ctx context.Context, store *taskengine.Store, mode string, artexFixture, requireCollectorSummary bool, res *result) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%v", recovered)
		}
	}()
	version, err := ensureSchema(ctx, store, mode == "seed")
	if err != nil {
		return err
	}
	res.SchemaVersion = version
	if mode == "seed" {
		res.Seed, err = seedFixture(ctx, store, version)
		return err
	}
	res.Scenario, err = runScenario(ctx, store, version, artexFixture, requireCollectorSummary)
	return err
}

// ensureSchema never upgrades a scenario database and never guesses a head:
// the parent registers 0011_artex_sync and 0012_login_sessions before this runs.
func ensureSchema(ctx context.Context, store *taskengine.Store, allowMigrate bool) (int, error) {
	if allowMigrate {
		// Migrate is a no-op at the registered head and additive below it. It
		// never runs in scenario mode, which must observe the seeded database.
		if err := store.Migrate(ctx); err != nil {
			return 0, fmt.Errorf("migrate fixture schema: %w", err)
		}
	} else {
		var exists bool
		if err := store.Pool.QueryRow(ctx, `SELECT to_regclass('core.schema_version') IS NOT NULL`).Scan(&exists); err != nil {
			return 0, fmt.Errorf("read schema presence: %w", err)
		}
		if !exists {
			return 0, errors.New("task engine schema is missing; migrate the fixture database first")
		}
	}
	var version int
	if err := store.Pool.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM core.schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if version != expectedSchemaVersion {
		return version, fmt.Errorf("fixture schema version=%d, want %d", version, expectedSchemaVersion)
	}
	return version, nil
}

// validateFixtureDSN pins the account, database, credential shape and host of
// the disposable fixture. It accepts the /fixture Unix socket or a loopback TCP
// port (the parent runs PostgreSQL in a locally bound container) and nothing
// else. The DSN is never printed.
func validateFixtureDSN(dsn string) error {
	if dsn == "" {
		return fmt.Errorf("%s is required", taskEngineDSNEnv)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("BBA_TASKENGINE_DSN is invalid")
	}
	if cfg.User != "bba_fixture" || cfg.Database != "bba_fixture_admin" {
		return fmt.Errorf("refusing non-fixture database user=%q database=%q", cfg.User, cfg.Database)
	}
	if cfg.Password != "" {
		return errors.New("BBA_TASKENGINE_DSN must not carry a password")
	}
	if len(cfg.Fallbacks) != 0 {
		return errors.New("BBA_TASKENGINE_DSN must not define fallback hosts")
	}
	if cfg.Host != "/fixture" {
		ip := net.ParseIP(cfg.Host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("refusing non-loopback fixture host %q", cfg.Host)
		}
	}
	return nil
}

func writeResult(path string, stdout io.Writer, res *result) error {
	encoded, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if path != "" {
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	_, err = stdout.Write(encoded)
	return err
}

func must(err error, what string) {
	if err != nil {
		panic(fmt.Errorf("%s: %w", what, err))
	}
}

func require(ok bool, format string, args ...any) {
	if !ok {
		panic(fmt.Errorf(format, args...))
	}
}

func digestOf(value any) string {
	encoded, err := json.Marshal(value)
	must(err, "encode digest input")
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func scalarInt(ctx context.Context, store *taskengine.Store, query string, args ...any) int {
	var value int
	must(store.Pool.QueryRow(ctx, query, args...).Scan(&value), "scalar count")
	return value
}

func scalarString(ctx context.Context, store *taskengine.Store, query string, args ...any) string {
	var value string
	must(store.Pool.QueryRow(ctx, query, args...).Scan(&value), "scalar value")
	return value
}
