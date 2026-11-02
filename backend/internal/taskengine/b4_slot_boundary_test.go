package taskengine

import (
	"bba.local/taskengine/internal/contract"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func b4Wait(t *testing.T, ch <-chan Reservation, ctx context.Context) Reservation {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-ctx.Done():
		t.Fatal("adapter barrier not reached")
		return Reservation{}
	}
}
func b4Done(t *testing.T, ch <-chan error, ctx context.Context) {
	t.Helper()
	select {
	case e := <-ch:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("consumer failed to join")
	}
}
func b4Control(t *testing.T, s *Store, task, action string) {
	t.Helper()
	ctx := context.Background()
	v, e := s.GetTask(ctx, fixtureActor(), task)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.ControlTask(ctx, fixtureActor(), task, newID("ctl"), contract.TaskControlRequest{Action: contract.TaskControlRequestAction(action), ExpectedVersion: v.Version, Reason: "offline B4 boundary"})
	if e != nil {
		t.Fatal(e)
	}
}

func TestB4SlotExpiredClaimRefused(t *testing.T) {
	s := fixtureStore(t)
	b4SlotTask(t, s)
	ctx := context.Background()
	c, e := s.ClaimReadyWorkItem(ctx, "expired", proposalWorkLease)
	if e != nil || c == nil {
		t.Fatalf("claim %v %v", c, e)
	}
	if _, e = s.Pool.Exec(ctx, `UPDATE core.work_item SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, c.WorkItemID); e != nil {
		t.Fatal(e)
	}
	_, e = s.ReserveWorkItem(ctx, *c)
	if Code(e) != workItemCodeLeaseLost {
		t.Fatalf("expired claim reserved: err=%v executions=%d", e, scalarInt(t, s, `SELECT count(*) FROM core.execution`))
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.execution`); n != 0 {
		t.Fatalf("expired created %d executions", n)
	}
}
func TestB4SlotReserveErrorObservable(t *testing.T) {
	s := fixtureStore(t)
	b4SlotTask(t, s)
	ctx := context.Background()
	if _, e := s.Pool.Exec(ctx, `CREATE FUNCTION core.b4_fail_reserve() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'b4_injected_reserve_failure'; END $$; CREATE TRIGGER b4_fail_reserve BEFORE INSERT ON core.execution FOR EACH ROW EXECUTE FUNCTION core.b4_fail_reserve()`); e != nil {
		t.Fatal(e)
	}
	a := &proposalWorkAdapter{}
	w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
	tick, e := w.Tick(ctx)
	if e == nil {
		t.Fatalf("reserve database error swallowed tick=%+v", tick)
	}
	if a.callCount() != 0 || tick.ProposalReserved != 0 {
		t.Fatalf("failure sent: calls=%d tick=%+v", a.callCount(), tick)
	}
	t.Logf("observable injected failure: %v", e)
}
func TestB4SlotResumeSharesAdmission(t *testing.T) {
	for _, state := range []string{"CREATED", "ACCEPTED"} {
		t.Run(state, func(t *testing.T) {
			s := fixtureStore(t)
			s.WorkSlotsDefault = 2
			task := b4SlotTask(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for i := 0; i < 3; i++ {
				c, e := s.ClaimReadyWorkItem(ctx, "old_worker", proposalWorkLease)
				if e != nil || c == nil {
					t.Fatal(c, e)
				}
				r, e := s.ReserveWorkItem(ctx, *c)
				if e != nil {
					t.Fatal(e)
				}
				if state == "ACCEPTED" {
					if _, e = s.AcceptOperation(ctx, runtimeActorFor(r), r); e != nil {
						t.Fatal(e)
					}
				}
			}
			a := &b4SlotBarrier{entered: make(chan Reservation, 3), release: make(chan struct{})}
			w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
			done := make(chan error, 1)
			go func() { _, e := w.Tick(ctx); done <- e }()
			released := false
			defer func() {
				if !released {
					close(a.release)
				}
				cancel()
			}()
			b4Wait(t, a.entered, ctx)
			b4Wait(t, a.entered, ctx)
			if n := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE task_id=$1 AND state=$2`, task, state); n != 1 {
				t.Fatalf("resume overflow remaining=%d", n)
			}
			close(a.release)
			released = true
			b4Done(t, done, ctx)
			if n := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE task_id=$1`, task); n != 3 {
				t.Fatalf("resume added execution: %d", n)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.max != 2 || a.active != 0 {
				t.Fatal(a.max, a.active)
			}
		})
	}
}
func TestB4SlotRecoverySkipsLiveCoordinator(t *testing.T) {
	s := fixtureStore(t)
	s.WorkSlotsDefault = 2
	b4SlotTask(t, s)
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
		cancel()
	}()
	r1 := b4Wait(t, a.entered, ctx)
	r2 := b4Wait(t, a.entered, ctx)
	for _, r := range []Reservation{r1, r2} {
		n, e := s.RecoverDispatching(ctx, runtimeActorFor(r))
		if e != nil || n != 0 {
			t.Fatalf("live sibling quarantined %d %v", n, e)
		}
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE state='DISPATCHING'`); n != 2 {
		t.Fatal(n)
	}
	close(a.release)
	released = true
	b4Done(t, done, ctx)
}
func TestB4SlotOwnerLossUnknownNoReplay(t *testing.T) {
	s := fixtureStore(t)
	s.WorkSlotsDefault = 2
	task := b4SlotTask(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := &b4SlotBarrier{entered: make(chan Reservation, 3), release: make(chan struct{})}
	w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
	done := make(chan error, 1)
	go func() { _, e := w.Tick(ctx); done <- e }()
	defer close(a.release)
	r1 := b4Wait(t, a.entered, ctx)
	r2 := b4Wait(t, a.entered, ctx)
	key := "taskengine.worker.task.ws_fixture." + task
	var killed int
	if e := s.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND granted AND mode='ExclusiveLock' AND objsubid=1 AND classid=((hashtextextended($1,0)>>32)&4294967295)::oid AND objid=(hashtextextended($1,0)&4294967295)::oid) q`, key).Scan(&killed); e != nil || killed != 1 {
		t.Fatalf("kill owner %d %v", killed, e)
	}
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("lease loss not observable")
		}
	case <-ctx.Done():
		t.Fatal("owner loss failed to drain")
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.work_item WHERE task_id=$1 AND state='UNKNOWN'`, task); n != 2 {
		t.Fatalf("unknown=%d", n)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE task_id=$1 AND state='reserved'`, task); n != 2 {
		t.Fatalf("unknown slots=%d", n)
	}
	for _, r := range []Reservation{r1, r2} {
		_, e := s.Dispatch(ctx, runtimeActorFor(r), r.OperationID, w.identityCheck, w.adapter)
		if e != nil {
			t.Fatal(e)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.events) != 4 || a.active != 0 {
		t.Fatalf("replay or leaked: %v active=%d", a.events, a.active)
	}
	t.Logf("owner terminated=%d unknown preserved=2 replay=0 events=%v", killed, a.events)
}

func TestB4SlotSnapshotFirstStartResumeReopen(t *testing.T) {
	s := fixtureStore(t)
	s.WorkSlotsDefault = 2
	task := b4SlotTask(t, s)
	snap := func() int { return scalarInt(t, s, `SELECT work_slots FROM core.task WHERE id=$1`, task) }
	if snap() != 2 {
		t.Fatal("first start missing snapshot")
	}
	s.WorkSlotsDefault = 1
	b4Control(t, s, task, "pause")
	b4Control(t, s, task, "resume")
	if snap() != 2 {
		t.Fatal("lower default changed resumed task")
	}
	other, e := Open(context.Background(), s.Pool.Config().ConnString())
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	other.WorkSlotsDefault = 1
	if n := scalarInt(t, other, `SELECT work_slots FROM core.task WHERE id=$1`, task); n != 2 {
		t.Fatal("restart reset snapshot")
	}
	for _, bad := range []int{0, 1, 9} {
		_, e := s.Pool.Exec(context.Background(), `UPDATE core.task SET work_slots=$2 WHERE id=$1`, task, bad)
		if e == nil {
			t.Fatal("snapshot changed", bad)
		}
	}
	for _, bad := range []int{-1, 9} {
		s.WorkSlotsDefault = bad
		if _, e := s.workSlotsDefault(); e == nil {
			t.Fatal("invalid default", bad)
		}
	}
}

// An adapter which observes cancellation but has not yet returned still owns
// its slot and task lease. No fire-and-forget sender survives coordinator exit.
type b4DrainAdapter struct {
	entered   chan Reservation
	cancelled chan struct{}
	release   chan struct{}
}

func (a *b4DrainAdapter) RequestJoin(ctx context.Context, r Reservation) (SendOutcome, error) {
	a.entered <- r
	<-ctx.Done()
	close(a.cancelled)
	<-a.release
	return SendOutcome{}, ctx.Err()
}
func TestB4SlotShutdownJoinsBeforeRelease(t *testing.T) {
	s := fixtureStore(t)
	task := b4SlotTask(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := &b4DrainAdapter{make(chan Reservation, 1), make(chan struct{}), make(chan struct{})}
	w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
	done := make(chan error, 1)
	go func() { _, e := w.Tick(ctx); done <- e }()
	b4Wait(t, a.entered, ctx)
	w.Stop()
	select {
	case <-a.cancelled:
	case <-ctx.Done():
		t.Fatal("stop not propagated")
	}
	l, e := acquireTaskLease(ctx, s.Pool, "ws_fixture", task)
	if e != nil {
		t.Fatal(e)
	}
	if l != nil {
		l.release()
		t.Fatal("lease released before child exited")
	}
	select {
	case <-done:
		t.Fatal("coordinator abandoned sender")
	default:
	}
	close(a.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("shutdown not joined")
	}
	l, e = acquireTaskLease(ctx, s.Pool, "ws_fixture", task)
	if e != nil || l == nil {
		t.Fatal("lease not released after drain", e)
	}
	l.release()
}

func TestB4SlotSameTaskOtherCoordinatorNoClaim(t *testing.T) {
	s := fixtureStore(t)
	task := b4SlotTask(t, s)
	ctx := context.Background()
	l, e := acquireTaskLease(ctx, s.Pool, "ws_fixture", task)
	if e != nil || l == nil {
		t.Fatal(e)
	}
	defer l.release()
	a := &proposalWorkAdapter{}
	w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
	tick, e := w.Tick(ctx)
	if e != nil || tick.ProposalClaimed != 0 || a.callCount() != 0 {
		t.Fatal(tick, e, a.callCount())
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.work_item WHERE task_id=$1 AND state='READY'`, task); n != 3 {
		t.Fatal(n)
	}
}

var _ = errors.Is
var _ = fmt.Sprint
