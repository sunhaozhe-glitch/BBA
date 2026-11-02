package taskengine

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// taskLease is the worker's exclusion token for one task.
type taskLease struct {
	mu            sync.Mutex
	conn          *pgxpool.Conn
	direct        *pgx.Conn
	key           int64
	released      bool
	budgetRelease func()
}

// acquireTaskLease takes a session advisory lock on a dedicated non-pooled
// connection. A nil lease with a nil error means another worker holds the task.
func acquireTaskLease(ctx context.Context, pool *pgxpool.Pool, workspace, taskID string) (*taskLease, error) {
	return acquireNamedTaskLease(ctx, pool, "taskengine.worker.task."+workspace+"."+taskID)
}
func acquireNamedTaskLease(ctx context.Context, pool *pgxpool.Pool, name string) (*taskLease, error) {
	// Reserve before dialing; failure/cancellation returns the slot, success
	// transfers it to the lease until the dedicated connection has closed.
	release, err := processLeaseConnections.acquire(ctx, pool)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	var key int64
	var held bool
	if err = conn.QueryRow(ctx, `SELECT hashtextextended($1,0),pg_try_advisory_lock(hashtextextended($1,0))`, name).Scan(&key, &held); err != nil || !held {
		closeCtx, c := context.WithTimeout(context.Background(), workerLeaseReleaseTimeout)
		defer c()
		_ = conn.Close(closeCtx)
		return nil, err
	}
	transferred = true
	return &taskLease{direct: conn, key: key, budgetRelease: release}, nil
}

// acquireSessionLease takes a named session advisory lock on a dedicated non-pooled
// connection. A nil lease with a nil error means another session holds the name.
// The lock lives exactly as long as the session, so a crashed process releases
// it without a schema change.
func acquireSessionLease(ctx context.Context, pool *pgxpool.Pool, name string) (*taskLease, error) {
	return acquireNamedTaskLease(ctx, pool, name)
}

// valid re-checks that the lease session still exists. The session is owned
// exclusively by this lease and the lock is only released by release(), so a
// live session still holds the lock. A dead session fails the query and the
// caller aborts before its next write.
func (l *taskLease) valid(ctx context.Context) error {
	if l == nil {
		return problem(workerLeaseLost)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return problem(workerLeaseLost)
	}
	var held bool
	var row pgx.Row
	sql := `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND granted AND mode='ExclusiveLock' AND objsubid=1 AND classid=(($1::bigint>>32)&4294967295)::oid AND objid=($1::bigint&4294967295)::oid)`
	if l.direct != nil {
		row = l.direct.QueryRow(ctx, sql, l.key)
	} else {
		row = l.conn.QueryRow(ctx, sql, l.key)
	}
	if err := row.Scan(&held); err != nil || !held {
		return problem(workerLeaseLost)
	}
	return nil
}

// release unlocks and returns the connection. It deliberately uses its own
// shutdown context: a cancelled tick must not leave the lease behind.
func (l *taskLease) release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	if l.budgetRelease != nil {
		defer l.budgetRelease()
	}
	if l.direct != nil {
		ctx, c := context.WithTimeout(context.Background(), workerLeaseReleaseTimeout)
		defer c()
		_ = l.direct.Close(ctx)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), workerLeaseReleaseTimeout)
	defer cancel()
	var unlocked bool
	if err := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, l.key).Scan(&unlocked); err != nil || !unlocked {
		// Closing the session is the only way to guarantee the server drops the
		// lock, and it keeps a still-locked session out of the pool.
		_ = l.conn.Conn().Close(ctx)
	}
	l.conn.Release()
}

// withTaskLease runs fn while holding the task lease. The second return value
// reports whether the task was processed at all.
func (w *Worker) withTaskLease(ctx context.Context, workspace, taskID string, fn func(*taskLease) error) (bool, error) {
	lease, err := acquireTaskLease(ctx, w.store.Pool, workspace, taskID)
	if err != nil {
		return false, err
	}
	if lease == nil {
		return false, nil
	}
	defer lease.release()
	return true, fn(lease)
}
