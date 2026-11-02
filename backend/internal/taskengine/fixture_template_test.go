package taskengine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// No persisted cache: each test executable migrates its own new template once.
// sync.Once publishes only after migration, pool closure and connection freeze.
// Separate executables use random names, even with identical migration contents.
var fixtureTemplate struct {
	once sync.Once
	name string
	err  error
}
var fixtureResources struct {
	sync.Mutex
	names    []string
	template string
	cfg      *pgx.ConnConfig
}

func fixtureMigrationFingerprint(files fs.FS, head int, tail []string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "head=%d;tail=%q;", head, tail)
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return "", fmt.Errorf("unexpected migration directory %s", entry.Name())
		}
		data, err := fs.ReadFile(files, "migrations/"+entry.Name())
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%d:%s:%d:", len(entry.Name()), entry.Name(), len(data))
		h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func ensureFixtureTemplate(ctx context.Context, cfg *pgx.ConnConfig) (string, error) {
	fixtureTemplate.once.Do(func() {
		start := time.Now()
		fingerprint, err := fixtureMigrationFingerprint(migrationFiles, migrationHead, migrationTail)
		if err != nil {
			fixtureTemplate.err = err
			return
		}
		name := newID("ft_" + fingerprint[:12])
		admin, err := pgx.ConnectConfig(ctx, cfg.Copy())
		if err != nil {
			fixtureTemplate.err = err
			return
		}
		defer admin.Close(ctx)
		if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			fixtureTemplate.err = err
			return
		}
		fixtureResources.Lock()
		fixtureResources.template = name
		fixtureResources.names = append(fixtureResources.names, name)
		fixtureResources.cfg = cfg.Copy()
		fixtureResources.Unlock()
		// Record ownership immediately, including a partially initialized template.
		fmt.Fprintf(os.Stderr, "FIXTURE_TEMPLATE_CREATED %s fingerprint=%s\n", name, fingerprint)
		u, err := url.Parse(os.Getenv("BBA_FIXTURE_DSN"))
		if err != nil {
			fixtureTemplate.err = err
			return
		}
		u.Path = "/" + name
		s, err := Open(ctx, u.String())
		if err != nil {
			fixtureTemplate.err = err
			return
		}
		err = s.Migrate(ctx)
		s.Close()
		if err != nil {
			fixtureTemplate.err = err
			return
		}
		// No seed data or connection survives publication. ALLOW_CONNECTIONS is the
		// hard freeze (including superusers); read-only is additional defence.
		if _, err = admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" SET default_transaction_read_only = on"); err != nil {
			fixtureTemplate.err = err
			return
		}
		if _, err = admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" ALLOW_CONNECTIONS false"); err != nil {
			fixtureTemplate.err = err
			return
		}
		var frozen bool
		var sessions int
		err = admin.QueryRow(ctx, `SELECT NOT datallowconn,(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&frozen, &sessions)
		if err != nil {
			fixtureTemplate.err = err
			return
		}
		if !frozen || sessions != 0 {
			fixtureTemplate.err = fmt.Errorf("template not frozen: disabled=%v sessions=%d", frozen, sessions)
			return
		}
		fixtureTemplate.name = name
		fmt.Fprintf(os.Stderr, "FIXTURE_TEMPLATE_READY %s head=%d sessions=%d frozen=%v setup_ms=%.3f\n", name, migrationHead, sessions, frozen, float64(time.Since(start).Microseconds())/1000)
	})
	return fixtureTemplate.name, fixtureTemplate.err
}

// Cleanup occurs after all parallel tests and their per-test cleanups. Only
// exact names recorded by this executable are inspected; no prefix sweeping.
func TestMain(m *testing.M) {
	code := m.Run()
	fixtureResources.Lock()
	names := append([]string(nil), fixtureResources.names...)
	template, cfg := fixtureResources.template, fixtureResources.cfg
	fixtureResources.Unlock()
	if len(names) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		admin, err := pgx.ConnectConfig(ctx, cfg.Copy())
		if err == nil {
			if template != "" {
				_, err = admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{template}.Sanitize()+" WITH (FORCE)")
			}
			if err == nil {
				var databases, sessions int
				err = admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_database WHERE datname=ANY($1)),(SELECT count(*) FROM pg_stat_activity WHERE datname=ANY($1))`, names).Scan(&databases, &sessions)
				fmt.Fprintf(os.Stderr, "FIXTURE_CLEANUP_READBACK owned=%d databases=%d sessions=%d names=%q\n", len(names), databases, sessions, names)
				if databases != 0 || sessions != 0 {
					err = fmt.Errorf("owned fixture resource leak")
				}
			}
			admin.Close(ctx)
		}
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "FIXTURE_CLEANUP_ERROR", err)
			code = 1
		}
	}
	os.Exit(code)
}
