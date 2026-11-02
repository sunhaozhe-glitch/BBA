package taskengine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLeaseBudgetBounds(t *testing.T) {
	var b leaseConnectionBudget
	ctx := context.Background()
	var releases []func()
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	for i := 0; i < taskLeaseProcessLimit/taskLeaseStoreLimit; i++ {
		pool := new(pgxpool.Pool)
		for j := 0; j < taskLeaseStoreLimit; j++ {
			r, e := b.acquire(ctx, pool)
			if e != nil {
				t.Fatal(e)
			}
			releases = append(releases, r)
		}
		if _, e := b.acquire(ctx, pool); !errors.Is(e, ErrLeaseConnectionBudget) {
			t.Fatalf("Store capacity: %v", e)
		}
	}
	if _, e := b.acquire(ctx, new(pgxpool.Pool)); !errors.Is(e, ErrLeaseConnectionBudget) {
		t.Fatalf("process capacity: %v", e)
	}
	for _, r := range releases {
		r()
		r()
	}
	if b.total != 0 || len(b.byPool) != 0 {
		t.Fatalf("retained budget: %+v", b)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e := b.acquire(cancelled, new(pgxpool.Pool)); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	r, e := b.acquire(ctx, new(pgxpool.Pool))
	if e != nil {
		t.Fatal(e)
	}
	r()
}

func TestLeaseBudgetConcurrentAdmission(t *testing.T) {
	var b leaseConnectionBudget
	pool := new(pgxpool.Pool)
	start := make(chan struct{})
	releases := make(chan func(), 64)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, e := b.acquire(context.Background(), pool)
			if e == nil {
				releases <- r
			} else if !errors.Is(e, ErrLeaseConnectionBudget) {
				t.Error(e)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(releases)
	if len(releases) != taskLeaseStoreLimit {
		t.Fatalf("admitted %d", len(releases))
	}
	for r := range releases {
		r()
	}
}

func TestLeaseBudgetFixtureLifecycle(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	cfg := s.Pool.Config()
	cfg.MaxConns = 1
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	// Two Workers on one Store share the same budget, not individual limits.
	store := &Store{Pool: pool}
	w1 := &Worker{store: store}
	w2 := &Worker{store: store}
	var leases []*taskLease
	defer func() {
		for _, l := range leases {
			l.release()
		}
	}()
	for i := 0; i < taskLeaseStoreLimit; i++ {
		var l *taskLease
		if i%2 == 0 {
			l, e = acquireTaskLease(ctx, w1.store.Pool, "budget", fmt.Sprint(i))
		} else {
			l, e = acquireSessionLease(ctx, w2.store.Pool, fmt.Sprint("rotation-budget-", i))
		}
		if e != nil || l == nil {
			t.Fatalf("lease %d: %v", i, e)
		}
		leases = append(leases, l)
	}
	if _, e = acquireTaskLease(ctx, pool, "budget", "overflow"); !errors.Is(e, ErrLeaseConnectionBudget) {
		t.Fatalf("overflow: %v", e)
	}
	// Pool=1 remains usable even while every dedicated budget slot is held.
	var one int
	if e = pool.QueryRow(ctx, "SELECT 1").Scan(&one); e != nil {
		t.Fatal(e)
	}
	leases[0].release()
	leases[0].release()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = acquireTaskLease(cancelled, pool, "budget", "cancel"); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	// Force cancellation after budget admission, during the real dial path.
	failCfg := pool.Config()
	failCfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) { return nil, context.Canceled }
	failPool, err := pgxpool.NewWithConfig(ctx, failCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer failPool.Close()
	for i := 0; i < taskLeaseStoreLimit+1; i++ {
		if _, err = acquireTaskLease(ctx, failPool, "budget", "fail"); !errors.Is(err, context.Canceled) {
			t.Fatalf("dial failure: %v", err)
		}
	}
	// Contended lock must also return the provisional budget slot.
	for i := 0; i < taskLeaseStoreLimit+1; i++ {
		l, err := acquireSessionLease(ctx, pool, "rotation-budget-1")
		if err != nil || l != nil {
			t.Fatalf("contention: %v %v", l, err)
		}
	}
	l, e := acquireTaskLease(ctx, pool, "budget", "replacement")
	if e != nil || l == nil {
		t.Fatalf("recovered: %v", e)
	}
	leases = append(leases, l)
	if e = l.valid(ctx); e != nil {
		t.Fatal(e)
	}
	for _, held := range leases {
		held.release()
	}
	// Cancellation and callback failure after a successful lease acquisition
	// must release the session as well as capacity via withTaskLease's defer.
	for i := 0; i < taskLeaseStoreLimit+1; i++ {
		callCtx, stop := context.WithCancel(ctx)
		processed, err := w1.withTaskLease(callCtx, "budget", "callback", func(*taskLease) error {
			stop()
			return callCtx.Err()
		})
		stop()
		if !processed || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled callback: %v %v", processed, err)
		}
		callbackErr := errors.New("callback failed")
		processed, err = w2.withTaskLease(ctx, "budget", "callback", func(*taskLease) error { return callbackErr })
		if !processed || !errors.Is(err, callbackErr) {
			t.Fatalf("failed callback: %v %v", processed, err)
		}
	}
	processLeaseConnections.mu.Lock()
	remaining := processLeaseConnections.total
	keys := len(processLeaseConnections.byPool)
	processLeaseConnections.mu.Unlock()
	if remaining != 0 || keys != 0 {
		t.Fatalf("budget leak: total=%d pools=%d", remaining, keys)
	}
}
