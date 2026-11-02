package taskengine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func b4Until(t *testing.T, ctx context.Context, fn func() bool) {
	t.Helper()
	timer := time.NewTicker(5 * time.Millisecond)
	defer timer.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("DB barrier timeout")
		case <-timer.C:
		}
	}
}
func b4KillOwner(t *testing.T, s *Store, ctx context.Context, task string) {
	t.Helper()
	key := "taskengine.worker.task.ws_fixture." + task
	var n int
	if e := s.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND granted AND mode='ExclusiveLock' AND objsubid=1 AND classid=((hashtextextended($1,0)>>32)&4294967295)::oid AND objid=(hashtextextended($1,0)&4294967295)::oid) q`, key).Scan(&n); e != nil || n != 1 {
		t.Fatalf("kill owner n=%d e=%v", n, e)
	}
}
func TestB4SlotBeforeSendBoundary(t *testing.T) {
	for _, action := range []string{"pause", "cancel", "lease_loss"} {
		t.Run(action, func(t *testing.T) {
			s := fixtureStore(t)
			s.WorkSlotsDefault = 2
			task := b4SlotTask(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Exclusive rule update lock blocks both slots AFTER permit consume but
			// BEFORE the final gateway and task-lease revalidation. Real DB handshake.
			guard, e := s.acquireContentGuard(ctx, "ws_fixture")
			if e != nil {
				t.Fatal(e)
			}
			defer guard.close()
			a := &proposalWorkAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted"}}
			w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
			done := make(chan error, 1)
			go func() { _, e := w.Tick(ctx); done <- e }()
			b4Until(t, ctx, func() bool {
				return scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE task_id=$1 AND state='DISPATCHING'`, task) == 2
			})
			if action == "lease_loss" {
				b4KillOwner(t, s, ctx, task)
			} else {
				b4Control(t, s, task, action)
			}
			guard.close()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("blocked worker did not exit")
			}
			if n := a.callCount(); n != 0 {
				t.Fatalf("%s sent %d", action, n)
			}
			if action == "lease_loss" {
				// Consume already committed; quarantining after owner loss is explicit and
				// never a replay. Recovery is allowed only after coordinator lock loss.
				rows, e := s.Pool.Query(ctx, `SELECT account_id,runtime_id,owner_epoch FROM core.execution WHERE task_id=$1`, task)
				if e != nil {
					t.Fatal(e)
				}
				var actors []RuntimeActor
				for rows.Next() {
					a := RuntimeActor{WorkspaceID: "ws_fixture"}
					var account string
					if e = rows.Scan(&account, &a.RuntimeID, &a.OwnerEpoch); e != nil {
						t.Fatal(e)
					}
					a.AccountIDs = []string{account}
					actors = append(actors, a)
				}
				rows.Close()
				if e = rows.Err(); e != nil {
					t.Fatal(e)
				}
				for _, a := range actors {
					if _, e = s.RecoverDispatching(ctx, a); e != nil {
						t.Fatal(e)
					}
				}
			}
			if n := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE task_id=$1 AND state='UNKNOWN'`, task); n != 2 {
				t.Fatalf("%s unknown=%d", action, n)
			}
			t.Logf("%s after consume/content wait: adapter calls=0 unknown=2", action)
		})
	}
}
func TestB4SlotAfterAdmissionControls(t *testing.T) {
	for _, action := range []string{"pause", "cancel", "lease_loss"} {
		t.Run(action, func(t *testing.T) {
			s := fixtureStore(t)
			task := b4SlotTask(t, s)
			ctx := context.Background()
			c, e := s.ClaimReadyWorkItem(ctx, "admission", proposalWorkLease)
			if e != nil || c == nil {
				t.Fatal(c, e)
			}
			r, e := s.ReserveWorkItem(ctx, *c)
			if e != nil {
				t.Fatal(e)
			}
			l, e := acquireTaskLease(ctx, s.Pool, "ws_fixture", task)
			if e != nil || l == nil {
				t.Fatal(e)
			}
			defer l.release()
			if action == "lease_loss" {
				b4KillOwner(t, s, ctx, task)
			} else {
				b4Control(t, s, task, action)
			}
			a := &proposalWorkAdapter{}
			w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
			e = w.dispatchReservation(ctx, l, r, &WorkerTick{})
			if e == nil || a.callCount() != 0 {
				t.Fatalf("%s after reserve err=%v calls=%d", action, e, a.callCount())
			}
		})
	}
}
func TestB4SlotAdapterInFlightControls(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		t.Run(action, func(t *testing.T) {
			s := fixtureStore(t)
			s.WorkSlotsDefault = 2
			task := b4SlotTask(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a := &b4SlotBarrier{entered: make(chan Reservation, 3), release: make(chan struct{})}
			w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
			done := make(chan error, 1)
			go func() { _, e := w.Tick(ctx); done <- e }()
			released := false
			defer func() {
				if !released {
					close(a.release)
				}
			}()
			b4Wait(t, a.entered, ctx)
			b4Wait(t, a.entered, ctx)
			b4Control(t, s, task, action)
			if state := fixtureString(t, s, `SELECT state FROM core.task WHERE id=$1`, task); action == "pause" && state != "PAUSING" {
				t.Fatal("pause falsely settled", state)
			}
			close(a.release)
			released = true
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("inflight not drained")
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if len(a.events) != 4 || a.active != 0 {
				t.Fatal("new admission after control", a.events)
			}
		})
	}
}
func TestB4SlotDistinctTasksAndSmallPool(t *testing.T) {
	for _, size := range []int{1, 2} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := fixtureStore(t)
			s.WorkSlotsDefault = 1
			first := b4SlotTask(t, s)
			second := b4SlotTaskNamed(t, s, "_other", []string{"9014301", "9014302", "9014303"})
			dsn := s.Pool.Config().ConnString()
			if strings.Contains(dsn, "?") {
				dsn += "&"
			} else {
				dsn += "?"
			}
			dsn += fmt.Sprintf("pool_max_conns=%d", size)
			small, e := Open(context.Background(), dsn)
			if e != nil {
				t.Fatal(e)
			}
			defer small.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			a := &b4SlotBarrier{entered: make(chan Reservation, 6), release: make(chan struct{})}
			w := proposalWorkWorker(t, small, a, WorkerSteps{Proposal: true})
			done := make(chan error, 1)
			go func() { _, e := w.Tick(ctx); done <- e }()
			released := false
			defer func() {
				if !released {
					close(a.release)
				}
			}()
			r1 := b4Wait(t, a.entered, ctx)
			r2 := b4Wait(t, a.entered, ctx)
			if r1.TaskID == r2.TaskID {
				t.Fatal("tasks did not progress independently")
			}
			if r1.TaskID != first && r1.TaskID != second {
				t.Fatal(r1.TaskID)
			}
			close(a.release)
			released = true
			b4Done(t, done, ctx)
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.max != 2 || a.active != 0 || len(a.events) != 12 {
				t.Fatal(a.max, a.active, a.events)
			}
		})
	}
}
func TestB4SlotDuplicateAndEpochClaimRefused(t *testing.T) {
	for _, mode := range []string{"duplicate", "epoch"} {
		t.Run(mode, func(t *testing.T) {
			s := fixtureStore(t)
			b4SlotTask(t, s)
			ctx := context.Background()
			c, e := s.ClaimReadyWorkItem(ctx, "old", proposalWorkLease)
			if e != nil || c == nil {
				t.Fatal(c, e)
			}
			if mode == "duplicate" {
				if _, e = s.ReserveWorkItem(ctx, *c); e != nil {
					t.Fatal(e)
				}
			} else {
				if _, e = s.Pool.Exec(ctx, `UPDATE core.work_item SET lease_epoch=lease_epoch+1 WHERE id=$1`, c.WorkItemID); e != nil {
					t.Fatal(e)
				}
			}
			if _, e = s.ReserveWorkItem(ctx, *c); Code(e) != workItemCodeLeaseLost {
				t.Fatal("stale admitted", e)
			}
		})
	}
}
