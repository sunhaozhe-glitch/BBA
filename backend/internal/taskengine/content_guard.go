package taskengine

import (
	"context"
	"github.com/jackc/pgx/v5"
	"sync"
	"time"
)

const contentSendTimeout = 20 * time.Second

// Process-wide ceiling includes senders and rule HTTP waiters across Stores.
// A slot is acquired BEFORE dialing; queued callers use no DB connection.
// Budget 32 extra connections per service process in addition to Pool sizes.
// Slot/dial/lock waits honor caller cancellation and a 5s acquisition deadline.
const contentGuardCapacity = 32

var contentGuardSlots = make(chan struct{}, contentGuardCapacity)

// Always acquire the workspace gate BEFORE borrowing Pool for review/results.
// The dedicated connection holds the existing transaction advisory lock.
// PostgreSQL and external HTTP are not an atomic transaction.
type contentGuard struct {
	conn      *pgx.Conn
	tx        pgx.Tx
	workspace string
	shared    bool
	closeOnce sync.Once
}

func (s *Store) acquireContentGuard(ctx context.Context, ws string) (*contentGuard, error) {
	return s.acquireContentGuardMode(ctx, ws, false)
}
func (s *Store) acquireContentSendGuard(ctx context.Context, ws string) (*contentGuard, error) {
	return s.acquireContentGuardMode(ctx, ws, true)
}
func (s *Store) acquireContentGuardMode(ctx context.Context, ws string, shared bool) (*contentGuard, error) {
	acquire, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case contentGuardSlots <- struct{}{}:
	case <-acquire.Done():
		return nil, acquire.Err()
	}
	cfg := s.Pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "bba_content_guard"
	conn, e := pgx.ConnectConfig(acquire, cfg)
	if e != nil {
		<-contentGuardSlots
		return nil, e
	}
	g := &contentGuard{conn: conn, workspace: ws, shared: shared}
	g.tx, e = conn.Begin(acquire)
	if e == nil {
		if shared {
			_, e = g.tx.Exec(acquire, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1,732613))`, ws)
		} else {
			e = contentLock(acquire, g.tx, ws)
		}
	}
	if e != nil {
		g.close()
		return nil, e
	}
	return g, nil
}
func (g *contentGuard) close() {
	g.closeOnce.Do(func() {
		defer func() { <-contentGuardSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = g.conn.Close(ctx)
	})
}
func (g *contentGuard) healthy(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	var held bool
	mode := "ExclusiveLock"
	if g.shared {
		mode = "ShareLock"
	}
	e := g.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND granted AND mode=$2 AND objsubid=1 AND classid=((hashtextextended($1,732613)>>32)&4294967295)::oid AND objid=(hashtextextended($1,732613)&4294967295)::oid)`, g.workspace, mode).Scan(&held)
	if e != nil {
		return e
	}
	if !held || g.conn.PgConn().TxStatus() != 'T' {
		return problem(taskCodeStoreUnavailable)
	}
	return ctx.Err()
}

// The goroutine owns failure until done closes. stop joins BEFORE reading it
// and BEFORE its own cancel: normal stop cannot manufacture a health fault.
// A running probe is bounded by 500ms. A final bounded probe catches lock loss
// between the last periodic probe and writer return, not DB+HTTP atomicity.
func (g *contentGuard) watch(ctx context.Context) (context.Context, func() error) {
	send, cancel := context.WithCancel(ctx)
	stop, done := make(chan struct{}), make(chan struct{})
	var failure error
	go func() {
		defer close(done)
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-send.Done():
				failure = ctx.Err()
				return
			case <-ticker.C:
				probe, c := context.WithTimeout(send, 500*time.Millisecond)
				e := g.healthy(probe)
				c()
				if e != nil {
					failure = e
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	return send, func() error {
		once.Do(func() {
			close(stop)
			<-done
			if failure == nil {
				failure = ctx.Err()
			}
			if failure == nil {
				probe, c := context.WithTimeout(context.WithoutCancel(ctx), 500*time.Millisecond)
				failure = g.healthy(probe)
				c()
			}
			cancel()
		})
		return failure
	}
}
