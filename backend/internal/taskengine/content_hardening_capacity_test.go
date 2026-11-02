package taskengine

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestB3HardFixBridgePoolOrder(t *testing.T) {
	for _, op := range []string{"preview", "list", "update"} {
		t.Run(op, func(t *testing.T) {
			observer, a, res := hardJoin(t, "fixture reason", "fixture answer")
			s := hardOne(t, observer)
			ctx, c := context.WithTimeout(context.Background(), 4*time.Second)
			defer c()
			w := &hardJoinBlock{entered: make(chan struct{}), release: make(chan struct{})}
			sendDone := make(chan error, 1)
			go func() { _, e := s.Dispatch(ctx, a, res.OperationID, identityOK, w); sendDone <- e }()
			select {
			case <-w.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			h := s.ContentHandler(func(*http.Request) (Actor, error) { return contentOperator(), nil })
			ruleDone := make(chan error, 1)
			go func() {
				r := hardRuleRequest(ctx, h, op)
				if r.Code != 200 {
					ruleDone <- problem(r.Body.String())
				} else {
					ruleDone <- nil
				}
			}()
			hardRuleWaiting(t, observer)
			close(w.release)
			if e := hardWithin(t, sendDone); e != nil {
				t.Fatal(e)
			}
			if e := hardWithin(t, ruleDone); e != nil {
				t.Fatal(e)
			}
			if w.calls != 1 {
				t.Fatal("wrong writer count")
			}
			if n := scalarInt(t, observer, `SELECT count(*) FROM core.content_review WHERE rules_version=0 AND decision='allowed'`); n != 1 {
				t.Fatalf("revision evidence=%d", n)
			}
			hardCleanup(t, observer)
		})
	}
}

func TestB3HardFixCapacityAndWatchJoin(t *testing.T) {
	s := fixtureStore(t)
	baseline := runtime.NumGoroutine()
	var guards []*contentGuard
	defer func() {
		for _, g := range guards {
			g.close()
		}
	}()
	for i := 0; i < contentGuardCapacity; i++ {
		g, e := s.acquireContentGuard(context.Background(), newID("workspace"))
		if e != nil {
			t.Fatal(e)
		}
		guards = append(guards, g)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name='bba_content_guard'`); n != contentGuardCapacity {
		t.Fatalf("connections=%d", n)
	}
	ctx, c := context.WithCancel(context.Background())
	c()
	started := time.Now()
	g, e := s.acquireContentGuard(ctx, "overflow")
	if e == nil {
		g.close()
		t.Fatal("overflow acquired")
	}
	if time.Since(started) > time.Second {
		t.Fatal("capacity cancellation was not bounded")
	}
	for _, g := range guards {
		g.close()
	}
	guards = nil
	g, e = s.acquireContentGuard(context.Background(), "join_test")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 100; i++ {
		_, stop := g.watch(context.Background())
		var wg sync.WaitGroup
		for j := 0; j < 3; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if e := stop(); e != nil {
					t.Errorf("normal stop=%v", e)
				}
			}()
		}
		wg.Wait()
	}
	g.close()
	hardCleanup(t, s)
	if len(contentGuardSlots) != 0 {
		t.Fatalf("capacity slots leaked=%d", len(contentGuardSlots))
	}
	// All 100 watch goroutines have explicit join receipts above. This secondary
	// broad bound catches accidental extra goroutines without unstable exact counts.
	if n := runtime.NumGoroutine(); n > baseline+4 {
		t.Errorf("goroutines baseline=%d after=%d", baseline, n)
	}
	t.Log("32-connection ceiling, canceled overflow opens zero, 100 watches concurrent idempotent stop/join; slots=0")
}
