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

// Ported from the read-only third-round TestThirdHistoricalPassiveBeforeConfirmed.
//
// A passive not_joined observed before this send cannot certify anything about
// the effect this send produces. Applying it early must not become the release
// key for a confirmed execution that still has no membership result: both the
// slot and the claim guard have to keep refusing.
func TestThirdHistoricalPassiveBeforeConfirmed(t *testing.T) {
	ctx := context.Background()
	s, rt, r := bridgeFixture(t, []string{"third_reverse"}, []string{"993001"}, 1)
	acceptFixture(t, s, rt, r)
	obs := obsTestObservation(r, "third_historical_leave", 1, "membership.observed", "not_joined", true, "third_old_absence", time.Now().UTC().Add(-time.Minute))
	obs.OperationID = ""
	if err := s.ReceiveObservation(ctx, rt, obs); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyObservation(ctx, r.WorkspaceID, r.RuntimeID, obs.EventID); err != nil {
		t.Fatalf("apply historical leave: %v", err)
	}
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	a := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "confirmed", EffectCertainty: "confirmed", EvidenceRef: "third_new_effect"}}
	if _, err := s.Dispatch(ctx, rt, r.OperationID, identityOK, a); err != nil {
		t.Fatalf("dispatch confirmed: %v", err)
	}
	_, slotErr := s.Pool.Exec(ctx, `UPDATE core.goal_slot SET state='free',target_id=NULL,reserved_at=NULL WHERE task_id=$1`, r.TaskID)
	_, claimErr := s.Pool.Exec(ctx, `UPDATE core.target_claim SET state='resolved',resolved_at=clock_timestamp() WHERE target_id=$1`, r.TargetID)
	t.Logf("adapter_calls=%d execution=%s results=%d old_membership=%s slot_release=%v claim_release=%v",
		a.count(), executionState(t, s, r.OperationID),
		scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, r.WorkspaceID, r.TaskID),
		fixtureString(t, s, `SELECT state FROM core.membership WHERE workspace_id=$1 AND target_id=$2`, r.WorkspaceID, r.TargetID),
		slotErr, claimErr)
	if slotErr == nil || claimErr == nil {
		t.Fatal("historical passive absence released a NEW confirmed effect that has no result yet")
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='reserved'`, r.WorkspaceID, r.TaskID); got != 1 {
		t.Fatalf("reserved slots=%d after rejected releases", got)
	}
}

// Ported from the read-only third-round TestThirdTerminalPassiveLeave.
//
// A closed task keeps its historical counts, but a later authoritative fact is
// still a fact: it must reach the projection, the inbox and the audit instead
// of being rolled back by the success closure.
func TestThirdTerminalPassiveLeave(t *testing.T) {
	s, rt, r := remediationConfirmedJoin(t)
	ctx := context.Background()
	task, err := s.GetTask(ctx, fixtureActor(), r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ControlTask(ctx, fixtureActor(), r.TaskID, "third_cancel", contract.TaskControlRequest{
		Action:          contract.TaskControlRequestActionCancel,
		ExpectedVersion: task.Version,
		Reason:          "stop after partial success",
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	before := fixtureString(t, s, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.TaskID)
	if before != "CANCELLED" {
		t.Fatalf("precondition terminal state=%s", before)
	}
	obs := obsTestObservation(r, "third_terminal_leave", 2, "membership.observed", "not_joined", true, "third_terminal_proof", time.Now().UTC())
	obs.OperationID = ""
	if err := s.ReceiveObservation(ctx, rt, obs); err != nil {
		t.Fatal(err)
	}
	applyErr := s.ApplyObservation(ctx, r.WorkspaceID, r.RuntimeID, obs.EventID)
	t.Logf("before=%s apply=%v after=%s membership=%s inbox=%s terminal_audit=%d",
		before, applyErr,
		fixtureString(t, s, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.TaskID),
		fixtureString(t, s, `SELECT state FROM core.membership WHERE workspace_id=$1 AND target_id=$2`, r.WorkspaceID, r.TargetID),
		fixtureString(t, s, `SELECT apply_state FROM core.inbox WHERE workspace_id=$1 AND event_id=$2`, r.WorkspaceID, obs.EventID),
		scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='membership.passive_leave_terminal'`, r.WorkspaceID, r.TaskID))
	if applyErr != nil {
		t.Fatalf("late passive fact cannot commit on a terminal task: %v", applyErr)
	}
	if got := fixtureString(t, s, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.TaskID); got != "CANCELLED" {
		t.Fatalf("late fact reopened the terminal task: %s", got)
	}
	if got := fixtureString(t, s, `SELECT state FROM core.membership WHERE workspace_id=$1 AND target_id=$2`, r.WorkspaceID, r.TargetID); got != "not_joined" {
		t.Fatalf("late fact lost in the projection: %s", got)
	}
	if got := fixtureString(t, s, `SELECT apply_state FROM core.inbox WHERE workspace_id=$1 AND event_id=$2`, r.WorkspaceID, obs.EventID); got != "applied" {
		t.Fatalf("late fact inbox state=%s", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='membership.passive_leave_terminal'`, r.WorkspaceID, r.TaskID); got != 1 {
		t.Fatalf("terminal audit events=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, r.WorkspaceID, r.TaskID); got != 1 {
		t.Fatalf("historical counts=%d want the closed success preserved", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND task_id=$2 AND state='scheduled'`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("scheduled retries=%d after a late leave", got)
	}
}

// Ported from the read-only third-round TestThirdRecoveryBeforeSend.
//
// Recovery committed UNKNOWN for an operation whose worker is still alive and
// parked before the adapter call. The worker must re-prove the recorded bridge
// state at the send point instead of waking up and sending.
func TestThirdRecoveryBeforeSend(t *testing.T) {
	ctx := context.Background()
	s, rt, r := bridgeFixture(t, []string{"third_recovery"}, []string{"993003"}, 1)
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	hold, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM core.subtask WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, r.WorkspaceID, r.SubtaskID); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "adapter_entered")
	cmd, out := remediationSpawnChild(t, "third_recovery", s.Pool.Config().ConnString(), r.OperationID, r.AccountID, r.RuntimeID, marker)
	t.Cleanup(func() {
		_ = hold.Rollback(context.Background())
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	remediationWait(t, "consumed commit", func() bool {
		return permitState(t, s, r.OperationID) == "consumed"
	})
	remediationWait(t, "old worker waiting in pre-send revalidation", func() bool {
		return scalarInt(t, s, `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FOR UPDATE%'`) > 0
	})
	recoverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	recovered, err := s.RecoverDispatching(recoverCtx, rt)
	if err != nil {
		t.Fatalf("recovery before send blocked or failed: %v", err)
	}
	if recovered != 1 || gatewayState(t, s, r.OperationID) != "UNKNOWN" {
		t.Fatalf("quarantine precondition recovered=%d state=%s", recovered, gatewayState(t, s, r.OperationID))
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("old worker error=%v output=%s", err, out.String())
	}
	_, markerErr := os.Stat(marker)
	t.Logf("recovered=%d marker_exists=%t child=%s gateway=%s",
		recovered, markerErr == nil, strings.TrimSpace(out.String()), gatewayState(t, s, r.OperationID))
	if markerErr == nil || !strings.Contains(out.String(), "adapter_calls=0") {
		t.Fatal("quarantined worker sent after the UNKNOWN commit")
	}
	if !strings.Contains(out.String(), "EXTERNAL_OUTCOME_UNKNOWN") {
		t.Fatalf("child output=%s want EXTERNAL_OUTCOME_UNKNOWN", out.String())
	}
	if got := gatewayState(t, s, r.OperationID); got != "UNKNOWN" {
		t.Fatalf("gateway state after the old worker woke=%s want UNKNOWN", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM bridge.outbox WHERE workspace_id=$1 AND operation_id=$2`, r.WorkspaceID, r.OperationID); got != 1 {
		t.Fatalf("receipts=%d want exactly the recovery receipt", got)
	}
}
