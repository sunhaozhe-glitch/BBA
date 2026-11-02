package taskengine

// Bounded deadline sweep.
//
// reconcileTaskProgress is the single reduction rule for task progress. It was
// originally reached only from an observation, so a task whose deadline passed
// without any new observation stayed RUNNING forever. The sweep gives that rule
// a second, deterministic entry point: it locks a bounded set of expired
// non-terminal tasks and runs the same reduction inside the same transaction.
//
// The sweep never retries, releases, reassigns or reclassifies an external
// effect. A pending/unknown execution keeps its row, its goal slot, its target
// claim and its account action window exactly as they are; the task's terminal
// event records the number of unresolved external effects at close time. That is
// why "task deadline reached" never means "the QQ join request disappeared" and
// never re-authorizes a second send.

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// deadlineSweepMaxBatch bounds one sweep pass. The bound is a hard ceiling so a
// large backlog is closed over several polls instead of one long transaction.
const deadlineSweepMaxBatch = 200

// DeadlineSweep is the committed summary of one bounded deadline pass.
type DeadlineSweep struct {
	// Scanned counts locked candidate tasks that were passed to
	// reconcileTaskProgress.
	Scanned int
	// Closed counts tasks that are terminal after the pass and were not
	// terminal before it. transitionTaskTerminal still re-checks state and
	// version, so a concurrent change is never overwritten.
	Closed int
}

// SweepDeadlines locks at most limit expired, non-terminal tasks in one
// transaction and closes the ones that reconcileTaskProgress proves finished.
// An empty workspace sweeps every workspace; callers that need a fixed scope
// pass one workspace id.
//
// Candidates are selected with FOR UPDATE SKIP LOCKED: concurrent sweepers and
// task-control transactions never wait for each other, and a task locked by an
// operator request is simply left for the next poll. Because the sweep only
// closes tasks through reconcileTaskProgress, a task with a possible external
// effect is recorded as terminal with pending_external > 0 and the execution row
// is left untouched.
func (s *Store) SweepDeadlines(ctx context.Context, workspace string, limit int) (DeadlineSweep, error) {
	var out DeadlineSweep
	if s == nil || s.Pool == nil {
		return out, problem("VALIDATION_FAILED")
	}
	if limit < 1 {
		return out, problem("VALIDATION_FAILED")
	}
	if limit > deadlineSweepMaxBatch {
		limit = deadlineSweepMaxBatch
	}
	if len(workspace) > 128 {
		return out, problem("VALIDATION_FAILED")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+taskColumns+`
			  FROM core.task
			 WHERE state NOT IN ('CANCELLED','SUCCEEDED','PARTIAL','FAILED')
			   AND deadline_at <= clock_timestamp()
			   AND ($2::text = '' OR workspace_id = $2::text)
			 ORDER BY deadline_at, workspace_id, id
			 LIMIT $1
			 FOR UPDATE SKIP LOCKED`, limit, workspace)
		if err != nil {
			return err
		}
		tasks := make([]TaskRow, 0, limit)
		for rows.Next() {
			task, err := scanTask(rows)
			if err != nil {
				rows.Close()
				return err
			}
			tasks = append(tasks, task)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, task := range tasks {
			if err := reconcileTaskProgress(ctx, tx, task); err != nil {
				return err
			}
			var state string
			if err := tx.QueryRow(ctx,
				`SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`,
				task.WorkspaceID, task.ID).Scan(&state); err != nil {
				return err
			}
			out.Scanned++
			if terminalTaskState(state) && !terminalTaskState(task.State) {
				out.Closed++
			}
		}
		return nil
	})
	if err != nil {
		return DeadlineSweep{}, err
	}
	return out, nil
}
