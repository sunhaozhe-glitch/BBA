package taskengine

// Historical B4 gap evidence is retained in the prior delivery and this file's
// baseline backup. This now permanently asserts unique task-coordinator exclusion
// through an adapter flight, not the absence of per-task slots.
import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type b4GapBarrier struct {
	mu              sync.Mutex
	started         chan struct{}
	release         chan struct{}
	active, maximum int
	events          []string
}

func (a *b4GapBarrier) RequestJoin(ctx context.Context, _ Reservation) (SendOutcome, error) {
	a.mu.Lock()
	a.active++
	if a.active > a.maximum {
		a.maximum = a.active
	}
	a.events = append(a.events, "adapter.start")
	a.mu.Unlock()
	close(a.started)
	var err error
	select {
	case <-a.release:
	case <-ctx.Done():
		err = ctx.Err()
	}
	a.mu.Lock()
	a.active--
	a.events = append(a.events, "adapter.end")
	a.mu.Unlock()
	// Explicit offline adapter boundary: not a QQ result or membership fact.
	if err != nil {
		return SendOutcome{}, err
	}
	return SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "b4_offline_barrier_only"}, nil
}

func TestB4GapProposalConsumerHoldsTaskLeaseAcrossAdapter(t *testing.T) {
	s := fixtureStore(t)
	task, approval, proposal, digest := proposalWorkSetup(t, s, 1, []string{"9004101"})
	proposalWorkMaterialize(t, s, task, approval, proposal, digest)
	a := &b4GapBarrier{started: make(chan struct{}), release: make(chan struct{})}
	w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	claim, err := s.ClaimReadyWorkItem(ctx, w.cfg.WorkerID, proposalWorkLease)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	done := make(chan error, 1)
	go func() { done <- w.processWorkItemClaim(ctx, *claim, &WorkerTick{}) }()
	var release sync.Once
	unblock := func() { release.Do(func() { close(a.release) }) }
	defer unblock()
	select {
	case <-a.started:
	case <-ctx.Done():
		t.Fatal("consumer did not reach adapter barrier")
	}
	// No sleep-based inference: adapter.start is a handshake and the adapter
	// remains blocked while the second real lease acquisition is attempted.
	other := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
	invoked := false
	processed, err := other.withTaskLease(ctx, claim.WorkspaceID, claim.TaskID, func(*taskLease) error {
		invoked = true
		return nil
	})
	if err != nil || processed || invoked {
		t.Errorf("same task unexpectedly admitted: processed=%v invoked=%v err=%v", processed, invoked, err)
	}
	a.mu.Lock()
	a.events = append(a.events, "same_task.lease_denied")
	a.mu.Unlock()
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("consumer: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("consumer did not finish after barrier release")
	}
	lease, err := acquireTaskLease(ctx, s.Pool, claim.WorkspaceID, claim.TaskID)
	if err != nil || lease == nil {
		t.Fatalf("task lease leaked: %v", err)
	}
	lease.release()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != 0 || a.maximum != 1 {
		t.Fatalf("active=%d max=%d", a.active, a.maximum)
	}
	payload, err := json.Marshal(map[string]any{"events": a.events, "maximum_inflight": a.maximum, "final_inflight": a.active, "task_id": task.Id, "settings_implemented": false, "unique_coordinator_exclusion": true})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(payload))
}
