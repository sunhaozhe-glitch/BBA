package taskengine

// Deadline sweep fixtures. They assert committed parent-task state, execution
// facts, event counts and the absence of retries, not just a successful return
// value.

import (
	"context"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
)

func sweepExpireDeadline(t *testing.T, s *Store, taskID string) {
	t.Helper()
	_, err := s.Pool.Exec(context.Background(), `
		UPDATE core.task
		   SET deadline_at=clock_timestamp()-interval '5 minutes',version=version+1
		 WHERE workspace_id='ws_fixture' AND id=$1`, taskID)
	if err != nil {
		t.Fatalf("expire task deadline: %v", err)
	}
}

// sweepDispatchAttempt runs the authorized attempt chain exactly once and
// returns its reservation.
func sweepDispatchAttempt(t *testing.T, s *Store, taskID, account, runtime string, adapter Adapter) Reservation {
	t.Helper()
	ctx := context.Background()
	r := reserveFixture(t, s, taskID, account)
	rt := fixtureRuntime(account, runtime)
	if _, err := s.AcceptOperation(ctx, rt, r); err != nil {
		t.Fatalf("accept operation: %v", err)
	}
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 10*time.Second); err != nil {
		t.Fatalf("issue permit: %v", err)
	}
	if _, err := s.Dispatch(ctx, rt, r.OperationID, func(context.Context, Reservation) error { return nil }, adapter); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return r
}

func TestDeadlineSweepClosesSilentDeadlineOnce(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_sweep_silent", "rt_sweep_silent", 2)
	task := createReadyTask(t, s, []string{"acc_sweep_silent"}, []string{"920001", "920002"}, 2, 2)
	taskID := string(task.Id)
	sweepExpireDeadline(t, s, taskID)

	first, err := s.SweepDeadlines(ctx, "ws_fixture", 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if first.Scanned != 1 || first.Closed != 1 {
		t.Fatalf("first sweep=%+v want 1 scanned, 1 closed", first)
	}
	if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID); got != "FAILED" {
		t.Fatalf("silent deadline task state=%s want FAILED", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event
		WHERE workspace_id='ws_fixture' AND task_id=$1 AND kind='task.terminal'
		  AND detail->>'reason'='deadline_reached'`, taskID); got != 1 {
		t.Fatalf("deadline terminal events=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("silent deadline produced executions=%d want 0", got)
	}
	version := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID)

	second, err := s.SweepDeadlines(ctx, "ws_fixture", 10)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second.Scanned != 0 || second.Closed != 0 {
		t.Fatalf("second sweep=%+v want 0 scanned, 0 closed", second)
	}
	if got := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID); got != version {
		t.Fatalf("repeated sweep changed version %d -> %d", version, got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event
		WHERE workspace_id='ws_fixture' AND task_id=$1 AND kind='task.terminal'`, taskID); got != 1 {
		t.Fatalf("repeated sweep terminal events=%d want 1", got)
	}
}

func TestDeadlineSweepPreservesPendingExternalEffect(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_sweep_pending", "rt_sweep_pending", 2)
	task := createReadyTask(t, s, []string{"acc_sweep_pending"}, []string{"920011"}, 1, 2)
	taskID := string(task.Id)
	adapter := &workerCountAdapter{}
	r := sweepDispatchAttempt(t, s, taskID, "acc_sweep_pending", "rt_sweep_pending", adapter)
	// Model a join request that the bridge reported as pending: an external
	// effect may exist and is not success.
	obsTestExec(t, s, `UPDATE core.execution
	  SET state='OBSERVED',disposition='pending',effect_certainty='confirmed_submitted',observed_at=clock_timestamp()
	 WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)

	// A dispatch-only worker pass must not turn pending work into another attempt.
	worker := workerFixture(t, s, adapter, WorkerSteps{Dispatch: true})
	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	if tick.Dispatched != 0 || adapter.count() != 1 {
		t.Fatalf("worker re-dispatched pending work: tick=%+v adapter_calls=%d", tick, adapter.count())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 1 {
		t.Fatalf("pending work produced executions=%d want 1", got)
	}

	sweepExpireDeadline(t, s, taskID)
	sweep, err := s.SweepDeadlines(ctx, "ws_fixture", 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sweep.Scanned != 1 || sweep.Closed != 1 {
		t.Fatalf("sweep=%+v want 1 scanned, 1 closed", sweep)
	}
	if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID); got != "FAILED" {
		t.Fatalf("task state=%s want FAILED (no valid result)", got)
	}
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID); got != "OBSERVED/pending/confirmed_submitted" {
		t.Fatalf("pending execution changed to %s", got)
	}
	if got := scalarInt(t, s, `SELECT COALESCE((detail->>'pending_external')::int,0) FROM core.task_event
		WHERE workspace_id='ws_fixture' AND task_id=$1 AND kind='task.terminal'`, taskID); got != 1 {
		t.Fatalf("terminal event pending_external=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot
		WHERE workspace_id='ws_fixture' AND task_id=$1 AND target_id=$2 AND state='reserved'`, taskID, r.TargetID); got != 1 {
		t.Fatalf("deadline sweep released the goal slot: reserved=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim
		WHERE workspace_id='ws_fixture' AND target_id=$1 AND state='active'`, r.TargetID); got != 1 {
		t.Fatalf("deadline sweep released the target claim: active=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("deadline sweep scheduled retries=%d want 0", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 1 {
		t.Fatalf("deadline sweep created a second execution: count=%d want 1", got)
	}
	if got := adapter.count(); got != 1 {
		t.Fatalf("deadline sweep called the adapter: calls=%d want 1", got)
	}
}

func TestDeadlineSweepPreservesUnknownExternalEffect(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_sweep_unknown", "rt_sweep_unknown", 2)
	task := createReadyTask(t, s, []string{"acc_sweep_unknown"}, []string{"920021"}, 1, 2)
	taskID := string(task.Id)
	adapter := &workerCountAdapter{}
	r := sweepDispatchAttempt(t, s, taskID, "acc_sweep_unknown", "rt_sweep_unknown", adapter)
	obsTestExec(t, s, `UPDATE core.execution
	  SET state='UNKNOWN',disposition='unknown',effect_certainty='unknown'
	 WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)

	worker := workerFixture(t, s, adapter, WorkerSteps{Dispatch: true, Recover: true})
	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	if tick.Dispatched != 0 || adapter.count() != 1 {
		t.Fatalf("worker re-dispatched unknown work: tick=%+v adapter_calls=%d", tick, adapter.count())
	}

	sweepExpireDeadline(t, s, taskID)
	sweep, err := s.SweepDeadlines(ctx, "ws_fixture", 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sweep.Scanned != 1 || sweep.Closed != 1 {
		t.Fatalf("sweep=%+v want 1 scanned, 1 closed", sweep)
	}
	if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID); got != "FAILED" {
		t.Fatalf("task state=%s want FAILED", got)
	}
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID); got != "UNKNOWN/unknown/unknown" {
		t.Fatalf("unknown execution weakened to %s", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("unknown close scheduled retries=%d want 0", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot
		WHERE workspace_id='ws_fixture' AND task_id=$1 AND target_id=$2 AND state='reserved'`, taskID, r.TargetID); got != 1 {
		t.Fatalf("unknown close released the goal slot: reserved=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim
		WHERE workspace_id='ws_fixture' AND target_id=$1 AND state='active'`, r.TargetID); got != 1 {
		t.Fatalf("unknown close released the target claim: active=%d want 1", got)
	}
	if got := adapter.count(); got != 1 {
		t.Fatalf("unknown close called the adapter: calls=%d want 1", got)
	}
}

func TestDeadlineSweepLeavesCancelledTaskClosedAfterLateObservation(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_sweep_cancel", "rt_sweep_cancel", 2)
	task := createReadyTask(t, s, []string{"acc_sweep_cancel"}, []string{"920031"}, 1, 2)
	taskID := string(task.Id)
	adapter := &workerCountAdapter{}
	r := sweepDispatchAttempt(t, s, taskID, "acc_sweep_cancel", "rt_sweep_cancel", adapter)

	current, err := s.GetTask(ctx, fixtureActor(), taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if _, err := s.ControlTask(ctx, fixtureActor(), taskID, newID("cancel"), contract.TaskControlRequest{
		Action:          "cancel",
		ExpectedVersion: current.Version,
		Reason:          "fixture cancel",
	}); err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	version := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID)

	// A trusted membership fact arrives after cancellation. It may only become a
	// historical audit event.
	late := obsTestObservation(r, "evt_sweep_late_join", 9, "membership.observed", "joined", true,
		"ev_sweep_late_join", time.Now().UTC())
	obsTestReceiveApply(t, s, r, late)

	if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID); got != "CANCELLED" {
		t.Fatalf("late observation revived task to %s", got)
	}
	if got := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID); got != version {
		t.Fatalf("late observation changed version %d -> %d", version, got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1 AND valid`, taskID); got != 0 {
		t.Fatalf("late observation created results=%d want 0", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event
		WHERE workspace_id='ws_fixture' AND task_id=$1 AND kind='membership.late_observed'`, taskID); got != 1 {
		t.Fatalf("late observation audit events=%d want 1", got)
	}

	sweepExpireDeadline(t, s, taskID)
	sweep, err := s.SweepDeadlines(ctx, "ws_fixture", 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sweep.Scanned != 0 || sweep.Closed != 0 {
		t.Fatalf("sweep touched a cancelled task: %+v", sweep)
	}
	if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID); got != "CANCELLED" {
		t.Fatalf("sweep changed cancelled task to %s", got)
	}

	worker := workerFixture(t, s, adapter, WorkerSteps{Dispatch: true})
	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	if tick.Dispatched != 0 || adapter.count() != 1 {
		t.Fatalf("worker dispatched after cancel: tick=%+v adapter_calls=%d", tick, adapter.count())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 1 {
		t.Fatalf("worker created a second execution after cancel: count=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("cancel path scheduled retries=%d want 0", got)
	}
}
