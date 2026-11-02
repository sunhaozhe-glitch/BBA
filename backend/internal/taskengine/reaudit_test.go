package taskengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
)

// Ported from the read-only independent TestReauditConsumedThenPaused.
//
// The permit is consumed but the adapter has not been called yet. A pause that
// commits in that window must win: persist the pause, cancel the in-flight
// send, and record the operation as unknown, because after a crash at the same
// instant nobody can prove the external call never happened.
func TestReauditConsumedThenPaused(t *testing.T) {
	cases := []struct {
		name        string
		group       string
		taskState   string
		commitPause func(t *testing.T, s *Store, r Reservation)
	}{
		{
			name:      "account_pause",
			group:     "990021",
			taskState: "RUNNING",
			commitPause: func(t *testing.T, s *Store, r Reservation) {
				t.Helper()
				if _, err := s.Pool.Exec(context.Background(),
					`UPDATE core.account SET control_state='paused' WHERE workspace_id=$1 AND id=$2`,
					r.WorkspaceID, r.AccountID); err != nil {
					t.Fatalf("persist account pause: %v", err)
				}
			},
		},
		{
			name:      "task_pause",
			group:     "990022",
			taskState: "PAUSING",
			commitPause: func(t *testing.T, s *Store, r Reservation) {
				t.Helper()
				ctx := context.Background()
				actor := fixtureActor()
				task, err := s.GetTask(ctx, actor, r.TaskID)
				if err != nil {
					t.Fatalf("read task for pause: %v", err)
				}
				if _, err := s.ControlTask(ctx, actor, r.TaskID, "reaudit-pause", contract.TaskControlRequest{
					Action:          contract.TaskControlRequestActionPause,
					ExpectedVersion: task.Version,
					Reason:          "pause committed inside the consumed window",
				}); err != nil {
					t.Fatalf("persist task pause: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, rt, r := bridgeFixture(t, []string{"fix_reaudit_" + tc.name}, []string{tc.group}, 1)
			acceptFixture(t, s, rt, r)
			ctx := context.Background()
			if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			// The attempt row lock parks the child after its consume commit and
			// before the adapter call: exactly the consumed-send window.
			hold, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := hold.Exec(ctx, `SELECT 1 FROM core.subtask WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, r.WorkspaceID, r.SubtaskID); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "adapter_entered")
			cmd, out := remediationSpawnChild(t, "pause", s.Pool.Config().ConnString(), r.OperationID, r.AccountID, r.RuntimeID, marker)
			t.Cleanup(func() {
				_ = hold.Rollback(context.Background())
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			remediationWait(t, "permit consumption before the paused send", func() bool {
				return permitState(t, s, r.OperationID) == "consumed"
			})
			// Wait for the worker to actually park in the pre-send gate, not
			// only for the consume commit: the pause must land inside the
			// blocking window this test claims to cover.
			remediationWait(t, "old worker waiting in pre-send revalidation", func() bool {
				return scalarInt(t, s, `SELECT count(*) FROM pg_stat_activity
					WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FOR UPDATE%'`) > 0
			})
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("child entered the adapter before the pause was committed")
			}
			tc.commitPause(t, s, r)
			if got := obsTestString(t, s, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.TaskID); got != tc.taskState {
				t.Fatalf("task state after the committed pause=%s want %s", got, tc.taskState)
			}
			if err := hold.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("paused child failed: %v output=%s", err, out.String())
				}
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("paused child never returned")
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("worker called the adapter in the consumed window after the pause committed")
			}
			if !strings.Contains(out.String(), "EXTERNAL_OUTCOME_UNKNOWN") {
				t.Fatalf("child output=%s want EXTERNAL_OUTCOME_UNKNOWN", out.String())
			}
			if !strings.Contains(out.String(), "adapter_calls=0") {
				t.Fatalf("child output=%s want adapter_calls=0", out.String())
			}
			if got := gatewayState(t, s, r.OperationID); got != "UNKNOWN" {
				t.Fatalf("gateway state after a pause-cancelled send=%s want UNKNOWN", got)
			}
			if got := fixtureString(t, s, `SELECT COALESCE(disposition,'') FROM bridge.gateway_operation WHERE workspace_id=$1 AND operation_id=$2`, r.WorkspaceID, r.OperationID); got != "unknown" {
				t.Fatalf("gateway disposition after a pause-cancelled send=%s", got)
			}
			if got := executionState(t, s, r.OperationID); got != "UNKNOWN" {
				t.Fatalf("execution state after a pause-cancelled send=%s want UNKNOWN", got)
			}
			if got := fixtureString(t, s, `SELECT COALESCE(effect_certainty,'') FROM core.execution WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.ExecutionID); got != "unknown" {
				t.Fatalf("execution certainty after a pause-cancelled send=%s want unknown", got)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND execution_id=$2 AND held`, r.WorkspaceID, r.ExecutionID); got != 1 {
				t.Fatalf("send lock after an unknown outcome=%d want 1 (never released)", got)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='reserved'`, r.WorkspaceID, r.TaskID); got != 1 {
				t.Fatalf("reserved slots after an unknown outcome=%d want 1", got)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND target_id=$2 AND state='active'`, r.WorkspaceID, r.TargetID); got != 1 {
				t.Fatalf("active claim after an unknown outcome=%d want 1", got)
			}
			if got := scalarInt(t, s, `SELECT COALESCE(sum(consumed),0) FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2`, r.WorkspaceID, r.AccountID); got != 1 {
				t.Fatalf("daily consumed quota after an unknown outcome=%d want 1", got)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, r.WorkspaceID, r.TaskID); got != 0 {
				t.Fatalf("valid results after a pause-cancelled send=%d", got)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM bridge.outbox WHERE workspace_id=$1 AND operation_id=$2 AND state='pending'`, r.WorkspaceID, r.OperationID); got != 1 {
				t.Fatalf("pending unknown receipts=%d want 1", got)
			}
			if err := s.ScheduleRetry(ctx, r.WorkspaceID, r.ExecutionID, "reselect", time.Now()); err == nil {
				t.Fatal("an unknown outcome must never be auto-retried")
			}
			recovered, err := s.RecoverDispatching(ctx, rt)
			if err != nil {
				t.Fatal(err)
			}
			if recovered != 0 {
				t.Fatalf("recovery re-quarantined %d operations that the pause already cancelled", recovered)
			}
			t.Logf("%s: adapter_calls=0; gateway=UNKNOWN; lock/slot/claim/quota retained", tc.name)
		})
	}
}

// Ported from the read-only independent TestReauditPassiveLeaveInvalidatesResult.
//
// A passive (uncorrelated) authoritative not_joined is the same external fact
// as the correlated one: once the account left the group, the credited result
// must stop counting so progress never exceeds reality, exactly like the
// active path.
func TestReauditPassiveLeaveInvalidatesResult(t *testing.T) {
	s, rt, r := remediationConfirmedJoin(t)
	ctx := context.Background()
	passive := Observation{
		EventID:          "passive_leave_one",
		AccountID:        r.AccountID,
		TargetID:         r.TargetID,
		Kind:             "membership.observed",
		State:            "not_joined",
		EvidenceRef:      "passive_leave_evidence",
		SourceEpoch:      r.OwnerEpoch,
		SourceSequence:   7,
		ObservedAt:       time.Now().UTC(),
		SnapshotComplete: true,
	}
	if err := s.ReceiveObservation(ctx, rt, passive); err != nil {
		t.Fatalf("receive passive leave: %v", err)
	}
	if err := s.ApplyObservation(ctx, r.WorkspaceID, r.RuntimeID, passive.EventID); err != nil {
		t.Fatalf("apply passive leave: %v", err)
	}
	if got := obsTestString(t, s, `SELECT state FROM core.membership WHERE workspace_id=$1 AND account_id=$2 AND target_id=$3`, r.WorkspaceID, r.AccountID, r.TargetID); got != "not_joined" {
		t.Fatalf("membership=%s", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("valid results=%d; a passive leave left stale success counted", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='free'`, r.WorkspaceID, r.TaskID); got != 2 {
		t.Fatalf("free slots=%d, want 2 after the passive invalidation", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND target_id=$2 AND state='active'`, r.WorkspaceID, r.TargetID); got != 0 {
		t.Fatalf("active claim=%d after the passive invalidation", got)
	}
	if got := obsTestString(t, s, `SELECT state FROM core.subtask WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.SubtaskID); got != "RECONCILING" {
		t.Fatalf("subtask state=%s want RECONCILING", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='result.invalidated'`, r.WorkspaceID, r.TaskID); got != 1 {
		t.Fatalf("result.invalidated events=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND task_id=$2 AND state='scheduled'`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("scheduled retries=%d; a passive leave must not auto-retry", got)
	}
	if got := scalarInt(t, s, `SELECT COALESCE(sum(consumed),0) FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2`, r.WorkspaceID, r.AccountID); got != 1 {
		t.Fatalf("daily consumed quota=%d; a real external action stays consumed", got)
	}
}
