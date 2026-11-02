package taskengine

import (
	"context"
	"testing"
)

func TestB3HardGuardHealth(t *testing.T) {
	for _, mode := range []string{"aborted_transaction", "rolled_back", "terminated"} {
		t.Run(mode, func(t *testing.T) {
			s := fixtureStore(t)
			ctx := context.Background()
			g, e := s.acquireContentGuard(ctx, "ws_fixture")
			if e != nil {
				t.Fatal(e)
			}
			defer g.close()
			if e = g.healthy(ctx); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "aborted_transaction":
				if _, e = g.tx.Exec(ctx, `SELECT 1/0`); e == nil {
					t.Fatal("invalidation did not fail")
				}
			case "rolled_back":
				if e = g.tx.Rollback(ctx); e != nil {
					t.Fatal(e)
				}
			case "terminated":
				hardKill(t, s)
			}
			calls := 0
			if e = g.healthy(ctx); e == nil {
				calls++
				t.Error("invalid guard accepted")
			}
			g.close()
			hardCleanup(t, s)
			t.Logf("fault=%s health_error=%v writer_calls=%d", mode, e, calls)
		})
	}
}
