package taskengine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestB4LeaseBudgetRealWorkersHighBatch(t *testing.T) {
	s := fixtureStore(t)
	s.WorkSlotsDefault = 1
	for i := 0; i < taskLeaseStoreLimit+2; i++ {
		b4SlotTaskNamed(t, s, fmt.Sprintf("_cap%d", i), []string{fmt.Sprint(9020000 + i*3), fmt.Sprint(9020001 + i*3), fmt.Sprint(9020002 + i*3)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	a := &b4SlotBarrier{entered: make(chan Reservation, 32), release: make(chan struct{})}
	defer b4Close(a.release)
	first := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
	first.cfg.BatchSize = workerMaxBatch
	second := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
	second.cfg.BatchSize = workerMaxBatch
	done := make(chan error, 1)
	go func() { _, e := first.Tick(ctx); done <- e }()
	for i := 0; i < taskLeaseStoreLimit; i++ {
		b4Wait(t, a.entered, ctx)
	}
	processLeaseConnections.mu.Lock()
	held := processLeaseConnections.byPool[s.Pool]
	total := processLeaseConnections.total
	processLeaseConnections.mu.Unlock()
	if held != taskLeaseStoreLimit || total > taskLeaseProcessLimit {
		t.Fatal(held, total)
	}
	if _, e := second.Tick(ctx); !errors.Is(e, ErrLeaseConnectionBudget) {
		t.Fatalf("second worker overload not explicit: %v", e)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.work_item WHERE state='READY'`); n != (taskLeaseStoreLimit+2)*3-taskLeaseStoreLimit {
		t.Fatal("overflow preclaimed", n)
	}
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, ErrLeaseConnectionBudget) {
			t.Fatal("first overload swallowed", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not drain")
	}
	processLeaseConnections.mu.Lock()
	remaining := processLeaseConnections.total
	processLeaseConnections.mu.Unlock()
	if remaining != 0 {
		t.Fatal("capacity leak", remaining)
	}
	l, e := acquireTaskLease(context.Background(), s.Pool, "capacity", "replacement")
	if e != nil || l == nil {
		t.Fatal(e)
	}
	l.release()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.max != taskLeaseStoreLimit || a.active != 0 {
		t.Fatal(a.max, a.active)
	}
	t.Logf("two real Workers BatchSize=%d tasks=%d: max task sessions=%d; excess ErrLeaseConnectionBudget; no preclaim; cancel drains and capacity reusable", workerMaxBatch, taskLeaseStoreLimit+2, held)
}
