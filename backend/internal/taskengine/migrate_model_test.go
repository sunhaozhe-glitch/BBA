package taskengine

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMigrateUpgradesSchema10Through17To18(t *testing.T) {
	dsn := os.Getenv("BBA_FIXTURE_DSN")
	if dsn == "" || os.Getenv("BBA_FIXTURE_ONLY") != "1" {
		t.Fatal("use tools/kernel.sh test: isolated PostgreSQL fixture required")
	}
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database != "bba_fixture_admin" || cfg.User != "bba_fixture" || cfg.Host != "/fixture" {
		t.Fatal("refusing non-fixture database connection")
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	for _, startVersion := range []int{10, 11, 12, 13, 14, 15, 16, 17} {
		t.Run(fmt.Sprintf("%d-to-18", startVersion), func(t *testing.T) {
			db := newID("migrate")
			if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); err != nil {
					t.Errorf("drop migration fixture: %v", err)
				}
			}()
			dbURL, err := url.Parse(dsn)
			if err != nil {
				t.Fatal(err)
			}
			dbURL.Path = "/" + db
			store, err := Open(ctx, dbURL.String())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			migrations := []string{"0001_core.sql"}
			if startVersion >= 11 {
				migrations = append(migrations, "0002_chat_send_permits.sql")
			}
			if startVersion >= 12 {
				migrations = append(migrations, "0003_model_requests.sql")
			}
			if startVersion >= 13 {
				migrations = append(migrations, "0004_approved_join_text.sql")
			}
			if startVersion >= 14 {
				migrations = append(migrations, "0005_graph_projection.sql")
			}
			if startVersion >= 15 {
				migrations = append(migrations, "0006_plan_proposals.sql")
			}
			if startVersion >= 16 {
				migrations = append(migrations, "0007_group_summary.sql")
			}
			if startVersion >= 17 {
				migrations = append(migrations, "0008_plan_proposal_basis.sql")
			}
			for _, migration := range migrations {
				sql, err := migrationFiles.ReadFile("migrations/" + migration)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Pool.Exec(ctx, string(sql)); err != nil {
					t.Fatalf("apply %s: %v", migration, err)
				}
			}
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("upgrade schema %d: %v", startVersion, err)
			}
			var version int
			if err := store.Pool.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != migrationHead {
				t.Fatalf("schema version=%d, want %d", version, migrationHead)
			}
		})
	}
}
