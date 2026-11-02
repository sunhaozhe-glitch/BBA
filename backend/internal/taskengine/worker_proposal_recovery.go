package taskengine

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// Recovery takes the SAME exclusion before any task rows. The try lock never
// waits for a live coordinator. Transaction scope prevents a check/use race.
// Legacy unmanaged dispatch retains its existing explicit recovery semantics.
func recoveryTaskUnowned(ctx context.Context, tx pgx.Tx, ws, task string) (bool, error) {
	var proposal bool
	if err := tx.QueryRow(ctx, `SELECT orchestration_mode='proposal' FROM core.task WHERE workspace_id=$1 AND id=$2`, ws, task).Scan(&proposal); err != nil {
		return false, err
	}
	if !proposal {
		return true, nil
	}
	var held bool
	err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "taskengine.worker.task."+ws+"."+task).Scan(&held)
	return held, err
}
