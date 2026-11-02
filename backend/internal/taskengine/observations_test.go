package taskengine

import (
	"bba.local/taskengine/internal/contract"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"time"
)

type obsTestAdapter struct {
	calls   int
	outcome SendOutcome
	err     error
}

func (a *obsTestAdapter) RequestJoin(context.Context, Reservation) (SendOutcome, error) {
	a.calls++
	return a.outcome, a.err
}

func obsTestAcceptPermit(t *testing.T, s *Store, r Reservation) RuntimeActor {
	t.Helper()
	ctx := context.Background()
	rt := fixtureRuntime(r.AccountID, r.RuntimeID)
	if _, err := s.AcceptOperation(ctx, rt, r); err != nil {
		t.Fatalf("accept operation: %v", err)
	}
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 10*time.Second); err != nil {
		t.Fatalf("issue permit: %v", err)
	}
	return rt
}

func obsTestDispatch(t *testing.T, s *Store, rt RuntimeActor, r Reservation, outcome SendOutcome, adapterErr error, expectError bool) *obsTestAdapter {
	t.Helper()
	a := &obsTestAdapter{outcome: outcome, err: adapterErr}
	_, err := s.Dispatch(context.Background(), rt, r.OperationID,
		func(context.Context, Reservation) error { return nil }, a)
	if expectError {
		if err == nil {
			t.Fatal("expected dispatch error before receipt")
		}
	} else if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return a
}

func obsTestObservation(r Reservation, eventID string, sequence int64, kind, state string, complete bool, evidence string, observed time.Time) Observation {
	return Observation{
		EventID:          eventID,
		OperationID:      r.OperationID,
		AccountID:        r.AccountID,
		TargetID:         r.TargetID,
		Kind:             kind,
		State:            state,
		EvidenceRef:      evidence,
		SourceEpoch:      r.OwnerEpoch,
		SourceSequence:   sequence,
		ObservedAt:       observed,
		SnapshotComplete: complete,
	}
}

func obsTestReceiveApply(t *testing.T, s *Store, r Reservation, obs Observation) {
	t.Helper()
	ctx := context.Background()
	rt := fixtureRuntime(r.AccountID, r.RuntimeID)
	if err := s.ReceiveObservation(ctx, rt, obs); err != nil {
		t.Fatalf("receive observation %s: %v", obs.EventID, err)
	}
	if err := s.ApplyObservation(ctx, "ws_fixture", r.RuntimeID, obs.EventID); err != nil {
		t.Fatalf("apply observation %s: %v", obs.EventID, err)
	}
}

func obsTestString(t *testing.T, s *Store, sql string, args ...any) string {
	t.Helper()
	var value string
	if err := s.Pool.QueryRow(context.Background(), sql, args...).Scan(&value); err != nil {
		t.Fatalf("query string: %v", err)
	}
	return value
}

func obsTestExec(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec fixture sql: %v", err)
	}
}

func TestTaskTerminalReductionOnExhaustionAndUnknown(t *testing.T) {
	t.Run("exhausted_with_result_is_partial", func(t *testing.T) {
		s := fixtureStore(t)
		seedAccount(t, s, "acc_reduce_partial", "rt_reduce_partial", 3)
		task := createReadyTask(t, s, []string{"acc_reduce_partial"}, []string{"900061", "900062"}, 2, 2)
		first := reserveFixture(t, s, string(task.Id), "acc_reduce_partial")
		rt := obsTestAcceptPermit(t, s, first)
		obsTestDispatch(t, s, rt, first, SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "ev_reduce_submitted"}, nil, false)
		firstEvent := obsTestObservation(first, "evt_reduce_join", 1, "membership.observed", "joined", true, "ev_reduce_join", time.Now().UTC().Add(time.Second))
		if err := s.ReceiveObservation(context.Background(), rt, firstEvent); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PumpBridgeOutbox(context.Background(), rt); err != nil {
			t.Fatal(err)
		}
		var firstInboxEvent string
		if err := s.Pool.QueryRow(context.Background(), `SELECT event_id FROM core.inbox WHERE workspace_id='ws_fixture' AND operation_id=$1 AND apply_state='pending'`, first.OperationID).Scan(&firstInboxEvent); err != nil {
			t.Fatal(err)
		}
		if err := s.ApplyObservation(context.Background(), first.WorkspaceID, first.RuntimeID, firstInboxEvent); err != nil {
			t.Fatalf("apply confirmed first target: %v", err)
		}
		second := reserveFixture(t, s, string(task.Id), "acc_reduce_partial")
		rt2 := obsTestAcceptPermit(t, s, second)
		obsTestDispatch(t, s, rt2, second, SendOutcome{Disposition: "rejected", EffectCertainty: "confirmed_rejected", EvidenceRef: "ev_reduce_rejected"}, nil, false)
		if n, err := s.PumpBridgeOutbox(context.Background(), rt2); err != nil || n != 1 {
			t.Fatalf("pump rejected observation n=%d err=%v want 1,nil", n, err)
		}
		var rejectedEvent string
		if err := s.Pool.QueryRow(context.Background(), `SELECT event_id FROM core.inbox WHERE workspace_id='ws_fixture' AND operation_id=$1 AND apply_state='pending'`, second.OperationID).Scan(&rejectedEvent); err != nil {
			t.Fatal(err)
		}
		if err := s.ApplyObservation(context.Background(), second.WorkspaceID, second.RuntimeID, rejectedEvent); err != nil {
			t.Fatalf("apply rejected observation: %v", err)
		}
		if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, task.Id); got != "PARTIAL" {
			t.Fatalf("exhausted task state=%s want PARTIAL", got)
		}
		if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id='ws_fixture' AND task_id=$1 AND kind='task.terminal'`, task.Id); got != 1 {
			t.Fatalf("task terminal events=%d want 1", got)
		}
	})

	t.Run("unknown_at_deadline_closes_without_releasing_hold", func(t *testing.T) {
		s := fixtureStore(t)
		seedAccount(t, s, "acc_reduce_unknown", "rt_reduce_unknown", 2)
		task := createReadyTask(t, s, []string{"acc_reduce_unknown"}, []string{"900064"}, 1, 2)
		r := reserveFixture(t, s, string(task.Id), "acc_reduce_unknown")
		rt := obsTestAcceptPermit(t, s, r)
		obsTestDispatch(t, s, rt, r, SendOutcome{}, errors.New("fixture timeout"), true)
		unknown := obsTestObservation(r, "evt_reduce_unknown", 1, "operation.observed", "unknown", false, "ev_reduce_unknown", time.Now().UTC().Add(time.Second))
		obsTestReceiveApply(t, s, r, unknown)
		if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, task.Id); got != "RUNNING" {
			t.Fatalf("unresolved task closed before deadline as %s", got)
		}
		obsTestExec(t, s, `UPDATE core.task SET deadline_at=clock_timestamp()-interval '1 second' WHERE workspace_id='ws_fixture' AND id=$1`, task.Id)
		lateUnknown := obsTestObservation(r, "evt_reduce_unknown_deadline", 2, "operation.observed", "unknown", false, "ev_reduce_unknown_deadline", time.Now().UTC().Add(2*time.Second))
		obsTestReceiveApply(t, s, r, lateUnknown)
		if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, task.Id); got != "FAILED" {
			t.Fatalf("deadline task state=%s want FAILED", got)
		}
		if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id='ws_fixture' AND task_id=$1 AND target_id=$2 AND state='reserved'`, task.Id, r.TargetID); got != 1 {
			t.Fatalf("deadline released uncertain slot count=%d want 1", got)
		}
	})

	t.Run("cancelled_task_is_not_reopened", func(t *testing.T) {
		s := fixtureStore(t)
		seedAccount(t, s, "acc_reduce_cancel", "rt_reduce_cancel", 2)
		task := createReadyTask(t, s, []string{"acc_reduce_cancel"}, []string{"900063"}, 1, 2)
		r := reserveFixture(t, s, string(task.Id), "acc_reduce_cancel")
		rt := obsTestAcceptPermit(t, s, r)
		obsTestDispatch(t, s, rt, r, SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "ev_reduce_cancel_submit"}, nil, false)
		actor := fixtureActor()
		current, err := s.GetTask(context.Background(), actor, string(task.Id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ControlTask(context.Background(), actor, string(task.Id), "reduce-cancel", contract.TaskControlRequest{Action: "cancel", ExpectedVersion: current.Version, Reason: "fixture cancellation"}); err != nil {
			t.Fatal(err)
		}
		obsTestReceiveApply(t, s, r, obsTestObservation(r, "evt_reduce_late_join", 1, "membership.observed", "joined", true, "ev_reduce_late_join", time.Now().UTC().Add(time.Second)))
		if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, task.Id); got != "CANCELLED" {
			t.Fatalf("late observation reopened task as %s", got)
		}
	})
}
func TestObservationReplayExactlyOnceApply(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_obs1", "rt_obs1", 2)
	task := createReadyTask(t, s, []string{"acc_obs1"}, []string{"900001"}, 1, 2)
	taskID := string(task.Id)
	r := reserveFixture(t, s, taskID, "acc_obs1")
	rt := obsTestAcceptPermit(t, s, r)
	obsTestDispatch(t, s, rt, r, SendOutcome{
		Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "ev_submitted_1",
	}, nil, false)

	joined := obsTestObservation(r, "evt_join_once", 1, "membership.observed", "joined", true,
		"ev_join_once", time.Now().UTC().Add(time.Second))
	if err := s.ReceiveObservation(ctx, rt, joined); err != nil {
		t.Fatalf("first receive: %v", err)
	}
	if err := s.ReceiveObservation(ctx, rt, joined); err != nil {
		t.Fatalf("duplicate receive must be idempotent: %v", err)
	}
	if err := s.ApplyObservation(ctx, "ws_fixture", r.RuntimeID, joined.EventID); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := s.ApplyObservation(ctx, "ws_fixture", r.RuntimeID, joined.EventID); err != nil {
		t.Fatalf("duplicate apply must be idempotent: %v", err)
	}

	// A different payload under the same event id is a durable conflict.
	conflict := joined
	conflict.State = "not_joined"
	conflict.EvidenceRef = "ev_conflict"
	if err := s.ReceiveObservation(ctx, rt, conflict); err == nil || Code(err) != "OBSERVATION_DIGEST_CONFLICT" {
		t.Fatalf("expected OBSERVATION_DIGEST_CONFLICT, got %v", err)
	}

	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1 AND valid`, taskID); got != 1 {
		t.Fatalf("result count=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id='ws_fixture' AND task_id=$1 AND state='fulfilled'`, taskID); got != 1 {
		t.Fatalf("fulfilled slots=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id='ws_fixture' AND task_id=$1 AND state='reserved'`, taskID); got != 0 {
		t.Fatalf("reserved slots=%d want 0", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.membership WHERE workspace_id='ws_fixture' AND account_id=$1 AND target_id=$2 AND state='joined'`, r.AccountID, r.TargetID); got != 1 {
		t.Fatalf("membership joined rows=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id='ws_fixture' AND task_id=$1 AND kind IN ('result.confirmed','task.terminal')`, taskID); got != 2 {
		t.Fatalf("confirmation and terminal events=%d want 2", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.inbox WHERE workspace_id='ws_fixture' AND source_runtime_id=$1 AND event_id=$2 AND apply_state='applied'`, r.RuntimeID, joined.EventID); got != 1 {
		t.Fatalf("applied inbox rows=%d want 1", got)
	}
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID); got != "OBSERVED/confirmed/confirmed" {
		t.Fatalf("execution outcome=%s want OBSERVED/confirmed/confirmed", got)
	}
	if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID); got != "SUCCEEDED" {
		t.Fatalf("task state=%s want SUCCEEDED after goal confirmation", got)
	}
	if got := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID); got != task.Version+1 {
		t.Fatalf("task version=%d want %d after terminal transition", got, task.Version+1)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id='ws_fixture' AND task_id=$1 AND kind='task.terminal'`, taskID); got != 1 {
		t.Fatalf("task.terminal events=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.outbox WHERE workspace_id='ws_fixture' AND task_id=$1 AND event_type='task.terminal'`, taskID); got != 1 {
		t.Fatalf("task.terminal outbox events=%d want 1", got)
	}

	// A delayed fact from an old owner epoch is preserved, but it must not
	// credit the task or mutate the old execution.
	staleTask := createReadyTask(t, s, []string{"acc_obs1"}, []string{"900006"}, 1, 2)
	staleTaskID := string(staleTask.Id)
	staleRes := reserveFixture(t, s, staleTaskID, "acc_obs1")
	staleRT := obsTestAcceptPermit(t, s, staleRes)
	obsTestDispatch(t, s, staleRT, staleRes, SendOutcome{
		Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "ev_submitted_stale",
	}, nil, false)
	obsTestExec(t, s, `UPDATE core.account SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND id=$1`, staleRes.AccountID)
	obsTestExec(t, s, `UPDATE core.runtime_credentials SET owner_epoch=2
	 WHERE workspace_id='ws_fixture' AND runtime_id=$1 AND account_id=$2`, staleRes.RuntimeID, staleRes.AccountID)
	staleActor := RuntimeActor{
		WorkspaceID: "ws_fixture", RuntimeID: staleRes.RuntimeID, OwnerEpoch: 2,
		AccountIDs: []string{staleRes.AccountID},
	}
	staleObs := obsTestObservation(staleRes, "evt_stale_join", 10, "membership.observed", "joined", true,
		"ev_stale_join", time.Now().UTC().Add(time.Second))
	if err := s.ReceiveObservation(ctx, staleActor, staleObs); err != nil {
		t.Fatalf("receive stale epoch fact: %v", err)
	}
	if err := s.ApplyObservation(ctx, "ws_fixture", staleRes.RuntimeID, staleObs.EventID); err != nil {
		t.Fatalf("apply stale epoch fact: %v", err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1`, staleTaskID); got != 0 {
		t.Fatalf("stale epoch credited result rows=%d want 0", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id='ws_fixture' AND task_id=$1 AND kind='manual_reconciliation' AND detail->>'execution_id'=$2`, staleTaskID, staleRes.ExecutionID); got != 1 {
		t.Fatalf("stale epoch reconciliation events=%d want 1", got)
	}
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty||'/'||COALESCE(evidence_ref,'')
	 FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, staleRes.ExecutionID); got != "OBSERVED/submitted/confirmed_submitted/ev_submitted_stale" {
		t.Fatalf("stale epoch mutated old execution: %s", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.membership WHERE workspace_id='ws_fixture' AND account_id=$1 AND target_id=$2`, staleRes.AccountID, staleRes.TargetID); got != 0 {
		t.Fatalf("stale epoch overwrote authoritative membership rows=%d want 0", got)
	}
}

func TestResultUniquePerTarget(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_res1", "rt_res1", 2)
	seedAccount(t, s, "acc_res2", "rt_res2", 2)
	task := createReadyTask(t, s, []string{"acc_res1", "acc_res2"}, []string{"900002"}, 1, 2)
	taskID := string(task.Id)
	r1 := reserveFixture(t, s, taskID, "acc_res1")
	rt1 := obsTestAcceptPermit(t, s, r1)
	obsTestDispatch(t, s, rt1, r1, SendOutcome{
		Disposition: "confirmed", EffectCertainty: "confirmed", EvidenceRef: "ev_confirmed_1",
	}, nil, false)
	first := obsTestObservation(r1, "evt_res_first", 1, "membership.observed", "joined", true,
		"ev_res_first", time.Now().UTC().Add(time.Second))
	obsTestReceiveApply(t, s, r1, first)

	// A second execution for the same target/account-pool, as a late duplicate
	// attempt would produce. It is a confirmed operation with a consumed permit,
	// but the target is already credited.
	exec2 := newID("exec")
	op2 := newID("op")
	err := s.tx(context.Background(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `UPDATE core.subtask SET state='RUNNING',attempt_count=2 WHERE workspace_id='ws_fixture' AND id=$1`, r1.SubtaskID); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `INSERT INTO core.execution(workspace_id,id,task_id,subtask_id,operation_id,account_id,target_id,runtime_id,owner_epoch,fence_token,scope_revision,attempt_no,action,request_digest,request_payload,state,disposition,effect_certainty,evidence_ref,started_at,observed_at) VALUES('ws_fixture',$1,$2,$3,$4,'acc_res2',$5,'rt_res2',1,2,1,2,'request_join',$6,'{}'::jsonb,'OBSERVED','confirmed','confirmed','ev_confirmed_2',clock_timestamp()-interval '1 minute',clock_timestamp())`, exec2, taskID, r1.SubtaskID, op2, r1.TargetID, strings.Repeat("b", 64))
		return err
	})
	if err != nil {
		t.Fatalf("insert duplicate execution: %v", err)
	}
	obsTestExec(t, s, `INSERT INTO core.dispatch_permit(
	  workspace_id,id,execution_id,operation_id,account_id,target_id,request_digest,
	  scope_revision,owner_epoch,fence_token,issued_at,expires_at,state)
	 VALUES('ws_fixture',$1,$2,$1,'acc_res2',$3,$4,1,1,2,clock_timestamp(),clock_timestamp()+interval '1 minute','consumed')`,
		op2, exec2, r1.TargetID, strings.Repeat("b", 64))

	r2 := r1
	r2.ExecutionID = exec2
	r2.OperationID = op2
	r2.AccountID = "acc_res2"
	r2.RuntimeID = "rt_res2"
	second := obsTestObservation(r2, "evt_res_second", 2, "membership.observed", "joined", true,
		"ev_res_second", time.Now().UTC().Add(time.Second))
	obsTestReceiveApply(t, s, r2, second)

	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1 AND target_id=$2`, taskID, r1.TargetID); got != 1 {
		t.Fatalf("result rows per target=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1 AND valid`, taskID); got != 1 {
		t.Fatalf("valid result rows=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id='ws_fixture' AND task_id=$1 AND target_id=$2 AND state='fulfilled'`, taskID, r1.TargetID); got != 1 {
		t.Fatalf("fulfilled target slots=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id='ws_fixture' AND task_id=$1 AND kind='result.confirmed'`, taskID); got != 1 {
		t.Fatalf("result events=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.membership WHERE workspace_id='ws_fixture' AND account_id='acc_res2' AND target_id=$1 AND state='joined'`, r1.TargetID); got != 1 {
		t.Fatalf("second account fact=%d want 1 (facts are preserved)", got)
	}
}

func TestCrashAfterSendBeforeReceipt(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_crash", "rt_crash", 2)
	seedAccount(t, s, "acc_crash_alt", "rt_crash_alt", 2)
	task := createReadyTask(t, s, []string{"acc_crash", "acc_crash_alt"}, []string{"900003"}, 1, 3)
	taskID := string(task.Id)
	r := reserveFixture(t, s, taskID, "acc_crash")
	rt := obsTestAcceptPermit(t, s, r)

	// The adapter is actually invoked once, then the process disappears before
	// a receipt can be persisted: exactly the crash window the bridge must keep
	// as UNKNOWN rather than retry.
	adapter := obsTestDispatch(t, s, rt, r, SendOutcome{}, errors.New("simulated crash after send"), true)
	if adapter.calls != 1 {
		t.Fatalf("adapter calls=%d want exactly 1", adapter.calls)
	}
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID); got != "UNKNOWN/unknown/unknown" {
		t.Fatalf("crashed execution=%s want UNKNOWN/unknown/unknown", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.dispatch_permit WHERE workspace_id='ws_fixture' AND execution_id=$1 AND state='consumed'`, r.ExecutionID); got != 1 {
		t.Fatalf("consumed permit=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id='ws_fixture' AND execution_id=$1 AND held`, r.ExecutionID); got != 1 {
		t.Fatalf("held account lock=%d want 1 before reconciliation", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("result count=%d want 0 for possibly-effected execution", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id='ws_fixture' AND task_id=$1 AND target_id=$2 AND state='reserved'`, taskID, r.TargetID); got != 1 {
		t.Fatalf("reserved slot=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id='ws_fixture' AND target_id=$1 AND state='active'`, r.TargetID); got != 1 {
		t.Fatalf("active claim=%d want 1", got)
	}

	if n, err := s.RecoverDispatching(ctx, rt); err != nil || n != 0 {
		t.Fatalf("recover dispatching n=%d err=%v want 0,nil (already UNKNOWN)", n, err)
	}
	if err := s.ScheduleRetry(ctx, "ws_fixture", r.ExecutionID, "same_account", time.Now().Add(time.Minute)); err == nil || Code(err) != "EXTERNAL_OUTCOME_UNKNOWN" {
		t.Fatalf("retry of unknown execution err=%v want EXTERNAL_OUTCOME_UNKNOWN", err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND subtask_id=$1`, r.SubtaskID); got != 0 {
		t.Fatalf("retry rows=%d want 0", got)
	}

	// Age the unresolved operation past the 24h manual reconciliation boundary.
	obsTestExec(t, s, `UPDATE core.execution
	  SET started_at=clock_timestamp()-interval '25 hours',
	      observed_at=clock_timestamp()-interval '25 hours'
	 WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)
	if n, err := s.ReconcileTimeouts(ctx, "ws_fixture"); err != nil || n != 1 {
		t.Fatalf("reconcile n=%d err=%v want 1,nil", n, err)
	}
	if n, err := s.ReconcileTimeouts(ctx, "ws_fixture"); err != nil || n != 0 {
		t.Fatalf("dedup reconcile n=%d err=%v want 0,nil", n, err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id='ws_fixture' AND task_id=$1 AND kind='manual_reconciliation' AND detail->>'execution_id'=$2`, taskID, r.ExecutionID); got != 1 {
		t.Fatalf("manual reconciliation events=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.outbox WHERE workspace_id='ws_fixture' AND event_type='manual_reconciliation'`); got != 1 {
		t.Fatalf("reconciliation outbox rows=%d want 1", got)
	}
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID); got != "UNKNOWN/unknown/unknown" {
		t.Fatalf("timeout weakened execution to %s want UNKNOWN/unknown/unknown", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id='ws_fixture' AND task_id=$1 AND target_id=$2 AND state='reserved'`, taskID, r.TargetID); got != 1 {
		t.Fatalf("timeout released slot; reserved=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id='ws_fixture' AND target_id=$1 AND state='active'`, r.TargetID); got != 1 {
		t.Fatalf("timeout released claim; active=%d want 1", got)
	}

	// The late authoritative membership fact resolves the unknown without a
	// resend, creates exactly one result and releases the send lock.
	late := obsTestObservation(r, "evt_late_join", 1, "membership.observed", "joined", true,
		"ev_late_join", time.Now().UTC())
	obsTestReceiveApply(t, s, r, late)
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1 AND valid`, taskID); got != 1 {
		t.Fatalf("late result=%d want 1", got)
	}
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID); got != "OBSERVED/confirmed/confirmed" {
		t.Fatalf("resolved execution=%s want OBSERVED/confirmed/confirmed", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id='ws_fixture' AND execution_id=$1 AND held`, r.ExecutionID); got != 0 {
		t.Fatalf("held send lock=%d want 0 after authoritative resolution", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND subtask_id=$1`, r.SubtaskID); got != 0 {
		t.Fatalf("late resolution created retry rows=%d want 0", got)
	}
	// Replay after resolution still applies exactly once.
	if err := s.ReceiveObservation(ctx, rt, late); err != nil {
		t.Fatalf("replay receive: %v", err)
	}
	if err := s.ApplyObservation(ctx, "ws_fixture", r.RuntimeID, late.EventID); err != nil {
		t.Fatalf("replay apply: %v", err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1 AND valid`, taskID); got != 1 {
		t.Fatalf("replayed result=%d want 1", got)
	}
}

func TestPendingNotRetried(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_pending", "rt_pending", 2)
	task := createReadyTask(t, s, []string{"acc_pending"}, []string{"900004"}, 1, 2)
	taskID := string(task.Id)
	r := reserveFixture(t, s, taskID, "acc_pending")
	rt := obsTestAcceptPermit(t, s, r)
	obsTestDispatch(t, s, rt, r, SendOutcome{
		Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "ev_pending_submit",
	}, nil, false)
	// Model the bridge's pending join request state explicitly; it is a
	// possible external effect, not success and not a retryable no-effect.
	obsTestExec(t, s, `UPDATE core.execution
	  SET state='OBSERVED',disposition='pending',effect_certainty='confirmed_submitted',
	      observed_at=clock_timestamp()
	 WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)

	pending := obsTestObservation(r, "evt_pending", 5, "operation.observed", "pending", false,
		"ev_pending", time.Now().UTC().Add(time.Second))
	obsTestReceiveApply(t, s, r, pending)
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID); got != "OBSERVED/pending/confirmed_submitted" {
		t.Fatalf("pending execution=%s want OBSERVED/pending/confirmed_submitted", got)
	}
	if err := s.ScheduleRetry(ctx, "ws_fixture", r.ExecutionID, "same_account", time.Now().Add(time.Minute)); err == nil || Code(err) != "EXTERNAL_OUTCOME_UNKNOWN" {
		t.Fatalf("pending retry err=%v want EXTERNAL_OUTCOME_UNKNOWN", err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND subtask_id=$1`, r.SubtaskID); got != 0 {
		t.Fatalf("pending retry rows=%d want 0", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id='ws_fixture' AND task_id=$1 AND target_id=$2 AND state='reserved'`, taskID, r.TargetID); got != 1 {
		t.Fatalf("pending released slot; reserved=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id='ws_fixture' AND target_id=$1 AND state='active'`, r.TargetID); got != 1 {
		t.Fatalf("pending released claim; active=%d want 1", got)
	}

	obsTestExec(t, s, `UPDATE core.execution SET observed_at=clock_timestamp()-interval '25 hours'
	 WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)
	if n, err := s.ReconcileTimeouts(ctx, "ws_fixture"); err != nil || n != 1 {
		t.Fatalf("pending reconcile n=%d err=%v want 1,nil", n, err)
	}
	if n, err := s.ReconcileTimeouts(ctx, "ws_fixture"); err != nil || n != 0 {
		t.Fatalf("pending reconcile dedup n=%d err=%v want 0,nil", n, err)
	}
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID); got != "OBSERVED/pending/confirmed_submitted" {
		t.Fatalf("reconcile weakened pending execution to %s", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND subtask_id=$1`, r.SubtaskID); got != 0 {
		t.Fatalf("reconcile created retry rows=%d want 0", got)
	}

	// not_joined does not prove absence of a pending request and must never
	// release holds.
	notJoined := obsTestObservation(r, "evt_not_joined", 7, "membership.observed", "not_joined", true,
		"ev_not_joined", time.Now().UTC().Add(time.Second))
	obsTestReceiveApply(t, s, r, notJoined)
	if got := obsTestString(t, s, `SELECT state FROM core.membership WHERE workspace_id='ws_fixture' AND account_id=$1 AND target_id=$2`, r.AccountID, r.TargetID); got != "not_joined" {
		t.Fatalf("membership fact=%s want not_joined", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("not_joined created result rows=%d want 0", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id='ws_fixture' AND task_id=$1 AND target_id=$2 AND state='reserved'`, taskID, r.TargetID); got != 1 {
		t.Fatalf("not_joined released slot; reserved=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id='ws_fixture' AND target_id=$1 AND state='active'`, r.TargetID); got != 1 {
		t.Fatalf("not_joined released claim; active=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND subtask_id=$1`, r.SubtaskID); got != 0 {
		t.Fatalf("not_joined created retry rows=%d want 0", got)
	}

	// A delayed joined fact with an older source sequence must not win merely
	// because its observed_at is newer. The newer not_joined sequence is the
	// authoritative membership fact and this late event cannot credit.
	olderJoined := obsTestObservation(r, "evt_older_joined", 6, "membership.observed", "joined", true,
		"ev_older_joined", time.Now().UTC().Add(2*time.Second))
	obsTestReceiveApply(t, s, r, olderJoined)
	if got := obsTestString(t, s, `SELECT state||'/'||source_event_id FROM core.membership
	 WHERE workspace_id='ws_fixture' AND account_id=$1 AND target_id=$2`, r.AccountID, r.TargetID); got != "not_joined/evt_not_joined" {
		t.Fatalf("older joined overwrote membership: %s want not_joined/evt_not_joined", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("older joined credited result rows=%d want 0", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.inbox WHERE workspace_id='ws_fixture' AND source_runtime_id=$1 AND event_id=$2 AND apply_state='applied'`, r.RuntimeID, olderJoined.EventID); got != 1 {
		t.Fatalf("older joined inbox applied rows=%d want 1", got)
	}
}

func TestRetryBudgetNotResetByAccountChange(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_budget1", "rt_budget1", 2)
	seedAccount(t, s, "acc_budget2", "rt_budget2", 2)
	task := createReadyTask(t, s, []string{"acc_budget1", "acc_budget2"}, []string{"900005"}, 1, 2)
	taskID := string(task.Id)
	// Keep the logical subtask budget at two attempts and set a task-wide cap
	// that the test can raise independently.
	obsTestExec(t, s, `UPDATE core.task SET max_total_attempts=3 WHERE workspace_id='ws_fixture' AND id=$1`, taskID)

	r1 := reserveFixture(t, s, taskID, "acc_budget1")
	rt1 := obsTestAcceptPermit(t, s, r1)
	obsTestDispatch(t, s, rt1, r1, SendOutcome{
		Disposition: "failed_no_effect", EffectCertainty: "no_effect", EvidenceRef: "ev_no_effect_1",
	}, nil, false)
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r1.ExecutionID); got != "OBSERVED/failed_no_effect/no_effect" {
		t.Fatalf("first failure=%s want OBSERVED/failed_no_effect/no_effect", got)
	}
	due := time.Now().Add(time.Minute)
	if err := s.ScheduleRetry(ctx, "ws_fixture", r1.ExecutionID, "same_account", due); err != nil {
		t.Fatalf("first retry: %v", err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND subtask_id=$1 AND state='scheduled' AND strategy='same_account'`, r1.SubtaskID); got != 1 {
		t.Fatalf("scheduled retries=%d want 1", got)
	}
	if got := obsTestString(t, s, `SELECT state FROM core.subtask WHERE workspace_id='ws_fixture' AND id=$1`, r1.SubtaskID); got != "RETRY_WAIT" {
		t.Fatalf("subtask state=%s want RETRY_WAIT", got)
	}
	if got := scalarInt(t, s, `SELECT attempt_count FROM core.subtask WHERE workspace_id='ws_fixture' AND id=$1`, r1.SubtaskID); got != 1 {
		t.Fatalf("attempt_count=%d want 1 after first retry scheduling", got)
	}
	if err := s.ScheduleRetry(ctx, "ws_fixture", r1.ExecutionID, "same_account", due); err == nil || Code(err) != "RETRY_ALREADY_SCHEDULED" {
		t.Fatalf("duplicate retry err=%v want RETRY_ALREADY_SCHEDULED", err)
	}

	// Consume the scheduled retry, then model a second attempt on a different
	// account. Attempt accounting stays on the logical subtask.
	obsTestExec(t, s, `UPDATE core.retry SET state='consumed'
	 WHERE workspace_id='ws_fixture' AND subtask_id=$1 AND state='scheduled'`, r1.SubtaskID)
	exec2 := newID("exec")
	op2 := newID("op")
	txErr := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE core.subtask SET state='RUNNING',assigned_account_id='acc_budget2',current_execution_id=NULL,attempt_count=2,fence_token=2,next_run_at=clock_timestamp(),version=version+1 WHERE workspace_id='ws_fixture' AND id=$1`, r1.SubtaskID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO core.execution(workspace_id,id,task_id,subtask_id,operation_id,account_id,target_id,runtime_id,owner_epoch,fence_token,scope_revision,attempt_no,action,request_digest,request_payload,state,disposition,effect_certainty,evidence_ref,started_at,observed_at) VALUES('ws_fixture',$1,$2,$3,$4,'acc_budget2',$5,'rt_budget2',1,3,1,2,'request_join',$6,'{}'::jsonb,'OBSERVED','failed_no_effect','no_effect','ev_no_effect_2',clock_timestamp()-interval '1 minute',clock_timestamp())`, exec2, taskID, r1.SubtaskID, op2, r1.TargetID, strings.Repeat("c", 64))
		return err
	})
	if txErr != nil {
		t.Fatalf("insert second attempt: %v", txErr)
	}
	obsTestExec(t, s, `INSERT INTO bridge.gateway_operation(workspace_id,operation_id,execution_id,task_id,subtask_id,account_id,target_id,runtime_id,owner_epoch,fence_token,scope_revision,request_digest,payload,state,disposition,effect_certainty,evidence_ref) VALUES('ws_fixture',$1,$2,$3,$4,'acc_budget2',$5,'rt_budget2',1,3,1,$6,'{}'::jsonb,'OBSERVED','failed_no_effect','no_effect','ev_no_effect_2')`, op2, exec2, taskID, r1.SubtaskID, r1.TargetID, strings.Repeat("c", 64))
	obsTestExec(t, s, `UPDATE core.subtask SET current_execution_id=$2,fence_token=3,version=version+1 WHERE workspace_id='ws_fixture' AND id=$1`, r1.SubtaskID, exec2)

	if err := s.ScheduleRetry(ctx, "ws_fixture", exec2, "reselect", time.Now().Add(time.Minute)); err == nil || Code(err) != "LOCAL_BUDGET_EXHAUSTED" {
		t.Fatalf("account-change retry err=%v want LOCAL_BUDGET_EXHAUSTED", err)
	}
	if got := scalarInt(t, s, `SELECT attempt_count FROM core.subtask WHERE workspace_id='ws_fixture' AND id=$1`, r1.SubtaskID); got != 2 {
		t.Fatalf("attempt_count reset to %d; want logical count 2", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND subtask_id=$1 AND state='scheduled'`, r1.SubtaskID); got != 0 {
		t.Fatalf("scheduled retry rows=%d want 0 after budget rejection", got)
	}

	// Task-wide attempts are independent of the subtask count and must also be
	// enforced even when the per-subtask limit is raised.
	obsTestExec(t, s, `UPDATE core.subtask SET max_attempts=5 WHERE workspace_id='ws_fixture' AND id=$1`, r1.SubtaskID)
	obsTestExec(t, s, `UPDATE core.task SET max_attempts=5,max_total_attempts=2 WHERE workspace_id='ws_fixture' AND id=$1`, taskID)
	if err := s.ScheduleRetry(ctx, "ws_fixture", exec2, "reselect", time.Now().Add(time.Minute)); err == nil || Code(err) != "LOCAL_BUDGET_EXHAUSTED" {
		t.Fatalf("task-total retry err=%v want LOCAL_BUDGET_EXHAUSTED", err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND subtask_id=$1 AND state='scheduled'`, r1.SubtaskID); got != 0 {
		t.Fatalf("scheduled retry rows=%d want 0 after task-total rejection", got)
	}

	// Raising only the task-wide cap permits the retry; the logical subtask
	// counters are still not reset by the account/execution change.
	obsTestExec(t, s, `UPDATE core.task SET max_total_attempts=5 WHERE workspace_id='ws_fixture' AND id=$1`, taskID)
	if err := s.ScheduleRetry(ctx, "ws_fixture", exec2, "reselect", time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("retry after raising task cap: %v", err)
	}
	if got := scalarInt(t, s, `SELECT attempt_count FROM core.subtask WHERE workspace_id='ws_fixture' AND id=$1`, r1.SubtaskID); got != 2 {
		t.Fatalf("attempt_count=%d want 2 after scheduling", got)
	}
	if got := scalarInt(t, s, `SELECT max_attempts FROM core.subtask WHERE workspace_id='ws_fixture' AND id=$1`, r1.SubtaskID); got != 5 {
		t.Fatalf("max_attempts=%d want unchanged 5", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND subtask_id=$1 AND state='scheduled' AND strategy='reselect'`, r1.SubtaskID); got != 1 {
		t.Fatalf("scheduled reselect retries=%d want 1", got)
	}
}
