package taskengine

import (
	"context"
	"embed"
	"errors"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationTail is every migration after 0001 in version order. Entry i raises
// the schema from version 10+i to 11+i, so the migration an existing database
// still needs is exactly migrationTail[version-10:], and the head is
// 10+len(migrationTail) == migrationHead.
var migrationTail = []string{
	"0002_chat_send_permits.sql",
	"0003_model_requests.sql",
	"0004_approved_join_text.sql",
	"0005_graph_projection.sql",
	"0006_plan_proposals.sql",
	"0007_group_summary.sql",
	"0008_plan_proposal_basis.sql",
	"0009_worker_cursor.sql",
	"0010_proposal_work.sql",
	"0011_artex_sync.sql",
	"0012_login_sessions.sql",
	"0013_content_rules.sql",
	"0014_work_slots.sql",
	"0015_work_settings.sql",
	"0016_qq_conversations.sql",
	"0017_operator_notification_scope.sql",
	"0018_goal_runs.sql",
	"0019_goal_reasoner.sql",
}

// migrationHead is the schema version this build migrates to: the version the
// last migrationTail entry records. Product migrations add proposal work,
// ARTEX sync and scoped login sessions without changing legacy task mode.
const migrationHead = 28

// Migrate explicitly initializes or upgrades a Task Engine fixture/database.
// Open never migrates; existing schemas advance additively to migrationHead.
func (s *Store) Migrate(ctx context.Context) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(740411)`); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('core.schema_version') IS NOT NULL`).Scan(&exists); err != nil {
			return err
		}
		var names []string
		if exists {
			var version int
			if err := tx.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&version); err != nil {
				return err
			}
			switch {
			case version == migrationHead:
				return nil
			case version >= 10 && version < migrationHead:
				names = migrationTail[version-10:]
			default:
				return errors.New("unsupported Task Engine schema version")
			}
		} else {
			names = append([]string{"0001_core.sql"}, migrationTail...)
		}
		for _, name := range names {
			sql, err := migrationFiles.ReadFile("migrations/" + name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
		}
		return nil
	})
}
