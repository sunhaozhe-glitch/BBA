package taskengine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
)

// fourthPauseFlightAdapter blocks inside RequestJoin until the test releases
// it, so the pause provably lands while an actual adapter call is live.
type fourthPauseFlightAdapter struct {
	entered chan struct{}
	release chan struct{}
	calls   int32
}

func (a *fourthPauseFlightAdapter) RequestJoin(context.Context, Reservation) (SendOutcome, error) {
	atomic.AddInt32(&a.calls, 1)
	close(a.entered)
	<-a.release
	return SendOutcome{Disposition: "failed_no_effect", EffectCertainty: "no_effect", EvidenceRef: "fourth_pause_flight_no_effect"}, nil
}

func (a *fourthPauseFlightAdapter) count() int { return int(atomic.LoadInt32(&a.calls)) }

// fourthPauseSnapshot is the pause-boundary evidence for one task. A local
// stop may only change the task lifecycle: unresolved external requests keep
// their subtask, slot, claim, quota and send evidence.
type fourthPauseSnapshot struct {
	held         int
	issued       int
	reserved     int
	free         int
	activeClaims int
	consumed     int
	executions   int
	pending      int
	unknown      int
	retries      int
	adapters     int
}

func fourthPauseObserve(t *testing.T, s *Store, r Reservation, adapter *fakeJoinAdapter) fourthPauseSnapshot {
	t.Helper()
	return fourthPauseSnapshot{
		held: scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock l
			JOIN core.execution e ON (e.workspace_id,e.id)=(l.workspace_id,l.execution_id)
			WHERE l.workspace_id=$1 AND e.task_id=$2 AND l.held`, r.WorkspaceID, r.TaskID),
		issued: scalarInt(t, s, `SELECT count(*) FROM core.dispatch_permit p
			JOIN core.execution e ON (e.workspace_id,e.id)=(p.workspace_id,p.execution_id)
			WHERE p.workspace_id=$1 AND e.task_id=$2 AND p.state='issued'`, r.WorkspaceID, r.TaskID),
		reserved:     scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='reserved'`, r.WorkspaceID, r.TaskID),
		free:         scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='free'`, r.WorkspaceID, r.TaskID),
		activeClaims: scalarInt(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND task_id=$2 AND state='active'`, r.WorkspaceID, r.TaskID),
		consumed:     scalarInt(t, s, `SELECT COALESCE(sum(consumed),0) FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2`, r.WorkspaceID, r.AccountID),
		executions:   scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, r.WorkspaceID, r.TaskID),
		pending: scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2
			AND effect_certainty='confirmed_submitted'`, r.WorkspaceID, r.TaskID),
		unknown: scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2
			AND state='UNKNOWN'`, r.WorkspaceID, r.TaskID),
		retries:  scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND task_id=$2`, r.WorkspaceID, r.TaskID),
		adapters: adapter.count(),
	}
}

func fourthPauseAssertPreserved(t *testing.T, label string, got, want fourthPauseSnapshot) {
	t.Helper()
	if got.held != want.held || got.issued != want.issued || got.reserved != want.reserved ||
		got.free != want.free || got.activeClaims != want.activeClaims || got.consumed != want.consumed ||
		got.executions != want.executions || got.pending != want.pending || got.unknown != want.unknown ||
		got.retries != want.retries || got.adapters != want.adapters {
		t.Fatalf("%s snapshot=%+v want %+v", label, got, want)
	}
}

func fourthPauseControl(t *testing.T, s *Store, taskID string, action contract.TaskControlRequestAction, key, reason string) error {
	t.Helper()
	task, err := s.GetTask(context.Background(), fixtureActor(), taskID)
	if err != nil {
		t.Fatalf("read task for %s: %v", action, err)
	}
	_, err = s.ControlTask(context.Background(), fixtureActor(), taskID, key, contract.TaskControlRequest{
		Action:          action,
		ExpectedVersion: task.Version,
		Reason:          reason,
	})
	return err
}

func fourthPauseTaskState(t *testing.T, s *Store, taskID string) string {
	t.Helper()
	return fixtureString(t, s, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", taskID)
}

// A returned "submitted" adapter result is an unresolved external request, not
// a local send still in flight. Pause must declare the local stop, resume must
// return to RUNNING without requeueing or resending, and the remaining free
// slot must be usable for other work.
func TestFourthPauseSettlesPendingExternalAndResumes(t *testing.T) {
	s, rt, r := bridgeFixture(t, []string{"fourth_pause_pending"}, []string{"995001", "995002"}, 2)
	ctx := context.Background()
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fourth_pause_pending_evidence"}}
	if _, err := s.Dispatch(ctx, rt, r.OperationID, identityOK, adapter); err != nil {
		t.Fatal(err)
	}
	external := fourthPauseSnapshot{held: 0, issued: 0, reserved: 1, free: 1, activeClaims: 1, consumed: 1, executions: 1, pending: 1, adapters: 1}
	fourthPauseAssertPreserved(t, "after submitted dispatch", fourthPauseObserve(t, s, r, adapter), external)
	subtaskExecution := fixtureString(t, s, `SELECT COALESCE(current_execution_id,'') FROM core.subtask WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.SubtaskID)
	if subtaskExecution != r.ExecutionID {
		t.Fatalf("subtask current execution=%q want %q", subtaskExecution, r.ExecutionID)
	}
	if got := fourthPauseTaskState(t, s, r.TaskID); got != "RUNNING" {
		t.Fatalf("task state before pause=%s", got)
	}

	if err := fourthPauseControl(t, s, r.TaskID, contract.TaskControlRequestActionPause, "fourth-pause-pending", "local stop with a returned external request"); err != nil {
		t.Fatalf("pause with a returned external request: %v", err)
	}
	if got := fourthPauseTaskState(t, s, r.TaskID); got != "PAUSED" {
		t.Fatalf("task state after local stop=%s want PAUSED", got)
	}
	fourthPauseAssertPreserved(t, "after pause", fourthPauseObserve(t, s, r, adapter), external)

	if err := fourthPauseControl(t, s, r.TaskID, contract.TaskControlRequestActionResume, "fourth-resume-pending", "resume remaining free slot"); err != nil {
		t.Fatalf("resume after local stop: %v", err)
	}
	if got := fourthPauseTaskState(t, s, r.TaskID); got != "RUNNING" {
		t.Fatalf("task state after resume=%s want RUNNING", got)
	}
	fourthPauseAssertPreserved(t, "after resume", fourthPauseObserve(t, s, r, adapter), external)
	if got := fixtureString(t, s, `SELECT COALESCE(current_execution_id,'') FROM core.subtask WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.SubtaskID); got != r.ExecutionID {
		t.Fatalf("resume requeued the unresolved subtask to %q want %q", got, r.ExecutionID)
	}
	if got := fixtureString(t, s, `SELECT state||'/'||effect_certainty FROM core.execution WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.ExecutionID); got != "OBSERVED/confirmed_submitted" {
		t.Fatalf("unresolved execution rewritten to %s", got)
	}

	next := reserveFixture(t, s, r.TaskID, "")
	if next.TargetID == r.TargetID || next.SubtaskID == r.SubtaskID {
		t.Fatalf("remaining free slot reused the unresolved request: %#v", next)
	}
	after := fourthPauseSnapshot{held: 0, issued: 0, reserved: 2, free: 0, activeClaims: 2, consumed: 1, executions: 2, pending: 1, adapters: 1}
	fourthPauseAssertPreserved(t, "after reserving remaining free slot", fourthPauseObserve(t, s, r, adapter), after)
}

// An UNKNOWN outcome is a read-only safety hold, not a local send. It keeps
// its lock, slot, claim and quota, but it must not pin the task in PAUSING
// forever or block resume: neither path may requeue or resend it.
func TestFourthPauseUnknownHeldLockLocalStopResume(t *testing.T) {
	s, rt, r := bridgeFixture(t, []string{"fourth_pause_unknown"}, []string{"995101", "995102"}, 2)
	ctx := context.Background()
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}}
	if _, err := s.Dispatch(ctx, rt, r.OperationID, identityOK, adapter); err != nil {
		t.Fatal(err)
	}
	unknown := fourthPauseSnapshot{held: 1, issued: 0, reserved: 1, free: 1, activeClaims: 1, consumed: 1, executions: 1, unknown: 1, adapters: 1}
	fourthPauseAssertPreserved(t, "after unknown outcome", fourthPauseObserve(t, s, r, adapter), unknown)

	if err := fourthPauseControl(t, s, r.TaskID, contract.TaskControlRequestActionPause, "fourth-pause-unknown", "local stop with a retained unknown"); err != nil {
		t.Fatalf("pause with a retained unknown: %v", err)
	}
	if got := fourthPauseTaskState(t, s, r.TaskID); got != "PAUSED" {
		t.Fatalf("task state after local stop with unknown=%s want PAUSED", got)
	}
	fourthPauseAssertPreserved(t, "after pause", fourthPauseObserve(t, s, r, adapter), unknown)

	if err := fourthPauseControl(t, s, r.TaskID, contract.TaskControlRequestActionResume, "fourth-resume-unknown", "resume while unknown is retained"); err != nil {
		t.Fatalf("resume with a retained unknown: %v", err)
	}
	if got := fourthPauseTaskState(t, s, r.TaskID); got != "RUNNING" {
		t.Fatalf("task state after resume=%s want RUNNING", got)
	}
	fourthPauseAssertPreserved(t, "after resume", fourthPauseObserve(t, s, r, adapter), unknown)
	if got := fixtureString(t, s, `SELECT state||'/'||effect_certainty FROM core.execution WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.ExecutionID); got != "UNKNOWN/unknown" {
		t.Fatalf("resume rewrote the unknown execution to %s", got)
	}
	// The held account lock still bars another send for this account, exactly
	// as before the pause; no retry or replacement is fabricated.
	if _, err := s.Reserve(ctx, r.WorkspaceID, r.TaskID, ""); Code(err) != schedCodeNoCapacity {
		t.Fatalf("reserve with a retained held lock=%v want %s", err, schedCodeNoCapacity)
	}
	fourthPauseAssertPreserved(t, "after refused reserve", fourthPauseObserve(t, s, r, adapter), unknown)
}

// Only real local flight blocks the pause: an actual adapter entry and an
// issued, unexpired permit that the bridge could still consume.
func TestFourthPauseActualSendOrIssuedPermitBlocks(t *testing.T) {
	t.Run("issued permit", func(t *testing.T) {
		s, rt, r := bridgeFixture(t, []string{"fourth_pause_issued"}, []string{"995201"}, 1)
		acceptFixture(t, s, rt, r)
		if _, err := s.IssuePermit(context.Background(), rt, r.OperationID, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		adapter := &fakeJoinAdapter{}
		issued := fourthPauseSnapshot{held: 1, issued: 1, reserved: 1, activeClaims: 1, executions: 1, adapters: 0}
		if err := fourthPauseControl(t, s, r.TaskID, contract.TaskControlRequestActionPause, "fourth-pause-issued", "issued permit may still send"); err != nil {
			t.Fatalf("pause with issued permit: %v", err)
		}
		if got := fourthPauseTaskState(t, s, r.TaskID); got != "PAUSING" {
			t.Fatalf("task state with issued permit=%s want PAUSING", got)
		}
		fourthPauseAssertPreserved(t, "issued permit", fourthPauseObserve(t, s, r, adapter), issued)
		if err := fourthPauseControl(t, s, r.TaskID, contract.TaskControlRequestActionResume, "fourth-resume-issued", "must not resume around local flight"); Code(err) != taskCodeStateConflict {
			t.Fatalf("resume with issued permit=%v want %s", err, taskCodeStateConflict)
		}
	})

	t.Run("adapter in flight", func(t *testing.T) {
		s, rt, r := bridgeFixture(t, []string{"fourth_pause_flight"}, []string{"995202"}, 1)
		acceptFixture(t, s, rt, r)
		if _, err := s.IssuePermit(context.Background(), rt, r.OperationID, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		adapter := &fourthPauseFlightAdapter{entered: entered, release: release}
		dispatched := make(chan error, 1)
		go func() {
			_, err := s.Dispatch(context.Background(), rt, r.OperationID, identityOK, adapter)
			dispatched <- err
		}()
		<-entered
		if err := fourthPauseControl(t, s, r.TaskID, contract.TaskControlRequestActionPause, "fourth-pause-flight", "adapter already entered"); err != nil {
			t.Fatalf("pause during an in-flight send: %v", err)
		}
		if got := fourthPauseTaskState(t, s, r.TaskID); got != "PAUSING" {
			t.Fatalf("task state during an in-flight send=%s want PAUSING", got)
		}
		if got := permitState(t, s, r.OperationID); got != "consumed" {
			t.Fatalf("in-flight permit=%s want consumed", got)
		}
		close(release)
		if err := <-dispatched; err != nil {
			t.Fatal(err)
		}
		if got := fourthPauseTaskState(t, s, r.TaskID); got != "PAUSED" {
			t.Fatalf("task state after the no-effect receipt=%s want PAUSED", got)
		}
		if adapter.count() != 1 {
			t.Fatalf("adapter calls=%d", adapter.count())
		}
	})
}

// Recovery may quarantine the durable row while this managed worker is still
// inside the adapter. The process-local dispatch lifetime must keep the pause
// from claiming a local stop and must keep resume refused until the call
// actually returns; the UNKNOWN row alone is not proof that the sender exited.
func TestFourthPauseRecoveryDuringLiveAdapter(t *testing.T) {
	s, rt, r := bridgeFixture(t, []string{"fourth_pause_recovery_live"}, []string{"995301"}, 1)
	ctx := context.Background()
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	adapter := &fourthPauseFlightAdapter{entered: entered, release: release}
	dispatched := make(chan error, 1)
	go func() {
		_, err := s.Dispatch(ctx, rt, r.OperationID, identityOK, adapter)
		dispatched <- err
	}()
	<-entered
	recovered, err := s.RecoverDispatching(ctx, rt)
	if err != nil || recovered != 1 {
		t.Fatalf("recover while adapter is live: n=%d err=%v", recovered, err)
	}
	if got := gatewayState(t, s, r.OperationID); got != "UNKNOWN" {
		t.Fatalf("gateway state after recovery=%s want UNKNOWN", got)
	}
	if got := executionState(t, s, r.OperationID); got != "UNKNOWN" {
		t.Fatalf("execution state after recovery=%s want UNKNOWN", got)
	}

	if err := fourthPauseControl(t, s, r.TaskID, contract.TaskControlRequestActionPause, "fourth-pause-recovery-live", "recovery raced a live adapter call"); err != nil {
		t.Fatalf("pause during a recovered live call: %v", err)
	}
	if got := fourthPauseTaskState(t, s, r.TaskID); got != "PAUSING" {
		t.Fatalf("task state during a recovered live call=%s want PAUSING", got)
	}
	if err := fourthPauseControl(t, s, r.TaskID, contract.TaskControlRequestActionResume, "fourth-resume-recovery-live", "must not resume around a live adapter"); Code(err) != taskCodeStateConflict {
		t.Fatalf("resume during a recovered live call=%v want %s", err, taskCodeStateConflict)
	}

	close(release)
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	if got := fourthPauseTaskState(t, s, r.TaskID); got != "PAUSED" {
		t.Fatalf("task state after the adapter returned=%s want PAUSED", got)
	}
	if adapter.count() != 1 {
		t.Fatalf("adapter calls=%d want exactly the one live call", adapter.count())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock l
		JOIN core.execution e ON (e.workspace_id,e.id)=(l.workspace_id,l.execution_id)
		WHERE l.workspace_id=$1 AND e.task_id=$2 AND l.held`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("send lock after the returned no-effect call=%d", got)
	}
}

// Recovery can quarantine the durable row while the managed worker is parked
// in its pre-send revalidation. Pause is PAUSING at that instant; when the
// worker wakes it refuses the adapter and its early "already recorded" branch
// must still settle the local lifecycle. Holds, slot, claim and quota stay.
func TestFourthPauseRecoveryBeforeSendRefusalSettles(t *testing.T) {
	s, rt, r := bridgeFixture(t, []string{"fourth_pause_recovery_parked"}, []string{"995401"}, 1)
	ctx := context.Background()
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	hold, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
	if _, err := hold.Exec(ctx, `SELECT 1 FROM core.subtask WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, r.WorkspaceID, r.SubtaskID); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeJoinAdapter{}
	dispatched := make(chan error, 1)
	go func() {
		_, err := s.Dispatch(ctx, rt, r.OperationID, identityOK, adapter)
		dispatched <- err
	}()
	remediationWait(t, "permit consumption before the parked send", func() bool {
		return permitState(t, s, r.OperationID) == "consumed"
	})
	remediationWait(t, "worker parked in pre-send revalidation", func() bool {
		return scalarInt(t, s, `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FOR UPDATE%'`) > 0
	})

	if err := fourthPauseControl(t, s, r.TaskID, contract.TaskControlRequestActionPause, "fourth-pause-recovery-parked", "local worker is parked before send"); err != nil {
		t.Fatalf("pause with a parked consumed send: %v", err)
	}
	if got := fourthPauseTaskState(t, s, r.TaskID); got != "PAUSING" {
		t.Fatalf("task state with a parked consumed send=%s want PAUSING", got)
	}
	recovered, err := s.RecoverDispatching(ctx, rt)
	if err != nil || recovered != 1 {
		t.Fatalf("recover while worker is parked: n=%d err=%v", recovered, err)
	}
	if got := gatewayState(t, s, r.OperationID); got != "UNKNOWN" {
		t.Fatalf("gateway state after recovery=%s", got)
	}
	if got := executionState(t, s, r.OperationID); got != "UNKNOWN" {
		t.Fatalf("execution state after recovery=%s", got)
	}
	if got := fourthPauseTaskState(t, s, r.TaskID); got != "PAUSING" {
		t.Fatalf("task state while the parked worker is still live=%s want PAUSING", got)
	}

	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-dispatched:
		if Code(err) != "EXTERNAL_OUTCOME_UNKNOWN" {
			t.Fatalf("parked worker result=%v want EXTERNAL_OUTCOME_UNKNOWN", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("parked worker never returned")
	}
	if adapter.count() != 0 {
		t.Fatalf("adapter calls after quarantined refusal=%d want 0", adapter.count())
	}
	if got := fourthPauseTaskState(t, s, r.TaskID); got != "PAUSED" {
		t.Fatalf("task state after the refusing worker returned=%s want PAUSED", got)
	}
	retained := fourthPauseSnapshot{held: 1, issued: 0, reserved: 1, free: 0, activeClaims: 1, consumed: 1, executions: 1, unknown: 1, adapters: 0}
	fourthPauseAssertPreserved(t, "after quarantined refusal", fourthPauseObserve(t, s, r, adapter), retained)
}
