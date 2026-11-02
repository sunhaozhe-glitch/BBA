package taskengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
)

type remediationBlockingAdapter struct{ entered, release chan struct{} }

func (a *remediationBlockingAdapter) RequestJoin(context.Context, Reservation) (SendOutcome, error) {
	close(a.entered)
	<-a.release
	return SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "actual_receipt"}, nil
}

// Ported from the read-only independent TestReviewConcurrentIdentityFailure.
func TestRemediationConcurrentIdentityFailure(t *testing.T) {
	s, rt, r := bridgeFixture(t, []string{"fix_identity"}, []string{"990003"}, 1)
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(context.Background(), rt, r.OperationID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	a := &remediationBlockingAdapter{make(chan struct{}), make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := s.Dispatch(context.Background(), rt, r.OperationID, identityOK, a); done <- err }()
	select {
	case <-a.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first dispatch never entered adapter")
	}
	_, secondErr := s.Dispatch(context.Background(), rt, r.OperationID, func(context.Context, Reservation) error { return errors.New("identity failure") }, &fakeJoinAdapter{})
	state := gatewayState(t, s, r.OperationID)
	held := scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock WHERE execution_id=$1 AND held`, r.ExecutionID)
	permit := permitState(t, s, r.OperationID)
	close(a.release)
	firstErr := <-done
	receipts := scalarInt(t, s, `SELECT count(*) FROM bridge.outbox WHERE operation_id=$1`, r.OperationID)
	evidence := fixtureString(t, s, `SELECT COALESCE(evidence_ref,'') FROM core.execution WHERE id=$1`, r.ExecutionID)
	t.Logf("second_error=%v in_flight=%s held=%d permit=%s first_error=%v receipts=%d evidence=%s", secondErr, state, held, permit, firstErr, receipts, evidence)
	if secondErr == nil || state != "DISPATCHING" || held != 1 || permit != "consumed" || firstErr != nil || receipts != 1 || evidence != "actual_receipt" || executionState(t, s, r.OperationID) != "OBSERVED" {
		t.Fatal("concurrent identity failure corrupted in-flight dispatch or lost actual receipt")
	}
}

func remediationConfirmedJoin(t *testing.T) (*Store, RuntimeActor, Reservation) {
	t.Helper()
	s, rt, r := bridgeFixture(t, []string{"fix_result"}, []string{"990004", "990005"}, 2)
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(context.Background(), rt, r.OperationID, time.Second); err != nil {
		t.Fatal(err)
	}
	a := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fix_confirm"}}
	if _, err := s.Dispatch(context.Background(), rt, r.OperationID, identityOK, a); err != nil {
		t.Fatal(err)
	}
	joined := obsTestObservation(r, "fix_joined", 1, "membership.observed", "joined", true, "fix_joined_evidence", time.Now().UTC())
	obsTestReceiveApply(t, s, r, joined)
	return s, rt, r
}

// Ported from the read-only independent TestReviewNewerMembershipAfterResult.
func TestRemediationNewerMembershipInvalidatesResult(t *testing.T) {
	s, rt, r := remediationConfirmedJoin(t)
	obs := obsTestObservation(r, "fix_left", 2, "membership.observed", "not_joined", true, "fix_left_evidence", time.Now().UTC())
	if err := s.ReceiveObservation(context.Background(), rt, obs); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyObservation(context.Background(), r.WorkspaceID, r.RuntimeID, obs.EventID); err != nil {
		t.Fatalf("apply newer membership: %v", err)
	}
	if got := obsTestString(t, s, `SELECT state FROM core.membership WHERE workspace_id=$1 AND account_id=$2 AND target_id=$3`, r.WorkspaceID, r.AccountID, r.TargetID); got != "not_joined" {
		t.Fatalf("membership=%s", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("valid results=%d; stale success remains counted", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='free'`, r.WorkspaceID, r.TaskID); got != 2 {
		t.Fatalf("free slots=%d, want 2 after invalidation and replenishment", got)
	}
}

// Ported from the read-only independent TestReviewConfirmedWithoutMembershipHolds.
func TestRemediationConfirmedWithoutMembershipRetainsHolds(t *testing.T) {
	s, rt, r := bridgeFixture(t, []string{"fix_confirmed"}, []string{"990007"}, 1)
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(context.Background(), rt, r.OperationID, time.Second); err != nil {
		t.Fatal(err)
	}
	a := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "confirmed", EffectCertainty: "confirmed", EvidenceRef: "fix_adapter_confirmed"}}
	if _, err := s.Dispatch(context.Background(), rt, r.OperationID, identityOK, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.goal_slot SET state='free',target_id=NULL,reserved_at=NULL WHERE workspace_id=$1 AND task_id=$2`, r.WorkspaceID, r.TaskID); err == nil {
		t.Fatal("confirmed execution without result released goal slot")
	}
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.target_claim SET state='resolved',resolved_at=clock_timestamp() WHERE workspace_id=$1 AND target_id=$2`, r.WorkspaceID, r.TargetID); err == nil {
		t.Fatal("confirmed execution without result released target claim")
	}
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.execution SET observed_at=clock_timestamp()-interval '25 hours' WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReconcileTimeouts(context.Background(), r.WorkspaceID); err != nil || n != 1 {
		t.Fatalf("confirmed no-result reconciliation n=%d err=%v", n, err)
	}
	if got := fixtureString(t, s, `SELECT detail->>'reason' FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='manual_reconciliation' ORDER BY seq DESC LIMIT 1`, r.WorkspaceID, r.TaskID); got != "membership_result_missing_24h" {
		t.Fatalf("reconcile reason=%q", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='reserved'`, r.WorkspaceID, r.TaskID); got != 1 {
		t.Fatalf("reserved slots after reconciliation=%d", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND task_id=$2`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("reconciliation scheduled retries=%d", got)
	}
}

// Ported from the read-only independent TestReviewResultDeleteClosure.
func TestRemediationResultCannotBeDeleted(t *testing.T) {
	s, _, r := remediationConfirmedJoin(t)
	if _, err := s.Pool.Exec(context.Background(), `DELETE FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3`, r.WorkspaceID, r.TaskID, r.TargetID); err == nil {
		t.Fatal("database deleted result while its goal slot remained fulfilled")
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND valid`, r.WorkspaceID, r.TaskID, r.TargetID); got != 1 {
		t.Fatalf("valid result rows after rejected delete=%d", got)
	}
}

// A draft stays editable through import/baseline, then an explicit operator
// start performs the submission and RUNNING transition atomically.
func TestRemediationDraftStartLifecycle(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "fix_draft", "rt_fix_draft", 5)
	req := ttRequest([]string{"990008"}, []string{"fix_draft"}, 1)
	req.Submit = false
	task := ttCreate(t, s, "fix_draft", req)
	ctx := context.Background()
	actor := fixtureActor()
	if task.State != "DRAFT" {
		t.Fatalf("created draft state=%s", task.State)
	}
	if done, err := s.ExpandFixedPage(ctx, actor, task.Id, 100); err != nil || !done {
		t.Fatalf("expand done=%v err=%v", done, err)
	}
	if err := s.CompleteBaseline(ctx, actor, task.Id, []BaselineSnapshot{{AccountID: "fix_draft", EvidenceRef: "fix_draft_baseline", ObservedAt: time.Now().UTC(), Complete: true}}); err != nil {
		t.Fatal(err)
	}
	task, err := s.GetTask(ctx, actor, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != "DRAFT" {
		t.Fatalf("baseline completion rewrote draft lifecycle to %s", task.State)
	}
	if _, err := s.ControlTask(ctx, actor, task.Id, "fix_draft_start", contract.TaskControlRequest{
		Action: contract.TaskControlRequestActionStart, ExpectedVersion: task.Version, Reason: "start prepared draft",
	}); err != nil {
		t.Fatalf("draft start: %v", err)
	}
	task, err = s.GetTask(ctx, actor, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != "RUNNING" {
		t.Fatalf("started draft state=%s want RUNNING", task.State)
	}
}

// Ported from the read-only independent TestReviewFixedCandidateDBConstraint.
func TestRemediationFixedCandidateCheckRejectsMissingFields(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "fix_fixed", "rt_fix_fixed", 5)
	task := ttCreate(t, s, "fix_fixed", ttRequest([]string{"990006"}, []string{"fix_fixed"}, 1))
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.task SET candidate_source='{}'::jsonb WHERE workspace_id=$1 AND id=$2`, task.WorkspaceId, task.Id); err == nil {
		t.Fatal("database CHECK accepted missing fixed candidate kind/groups")
	}
}

// remediationMarkerAdapter proves from outside the library that the adapter
// was entered; the child process writes the marker before returning.
type remediationMarkerAdapter struct {
	path    string
	outcome SendOutcome
	mu      sync.Mutex
	calls   int
}

func (a *remediationMarkerAdapter) RequestJoin(context.Context, Reservation) (SendOutcome, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if a.path != "" {
		if err := os.WriteFile(a.path, []byte("adapter_entered"), 0o644); err != nil {
			return SendOutcome{}, err
		}
	}
	return a.outcome, nil
}

func (a *remediationMarkerAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// TestRemediationProcessChild is the re-executed worker for the two
// process-level proofs. It only runs when the parent sets BBA_REM_CHILD.
func TestRemediationProcessChild(t *testing.T) {
	mode := os.Getenv("BBA_REM_CHILD")
	if mode == "" {
		t.Skip("child process helper")
	}
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("BBA_REM_DSN"))
	if err != nil {
		fmt.Fprintf(os.Stdout, "child_open_error=%v\n", err)
		os.Exit(11)
	}
	defer store.Close()
	actor := fixtureRuntime(os.Getenv("BBA_REM_ACCOUNT"), os.Getenv("BBA_REM_RUNTIME"))
	adapter := &remediationMarkerAdapter{
		path:    os.Getenv("BBA_REM_MARKER"),
		outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "child_actual_receipt"},
	}
	_, dispatchErr := store.Dispatch(ctx, actor, os.Getenv("BBA_REM_OP"), identityOK, adapter)
	fmt.Fprintf(os.Stdout, "child_mode=%s dispatch_error=%v adapter_calls=%d\n", mode, dispatchErr, adapter.count())
	os.Exit(0)
}

func remediationSpawnChild(t *testing.T, mode, dsn, operationID, account, runtime, marker string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRemediationProcessChild$")
	cmd.Env = append(os.Environ(),
		"BBA_REM_CHILD="+mode,
		"BBA_REM_DSN="+dsn,
		"BBA_REM_OP="+operationID,
		"BBA_REM_ACCOUNT="+account,
		"BBA_REM_RUNTIME="+runtime,
		"BBA_REM_MARKER="+marker,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	return cmd, &out
}

func remediationReceiptInsertActive(t *testing.T, s *Store) bool {
	t.Helper()
	return scalarInt(t, s, `SELECT count(*) FROM pg_stat_activity
		WHERE datname=current_database() AND pid<>pg_backend_pid()
		  AND state='active' AND query LIKE '%INSERT INTO bridge.outbox%'`) > 0
}

func remediationRetry(t *testing.T, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for {
		if last = fn(); last == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func remediationWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Evidence A: a real child process dies by SIGKILL after its adapter returned
// and before its receipt committed. Recovery must quarantine the operation as
// UNKNOWN, keep every hold, and never send a second adapter call.
func TestRemediationKillAfterAdapterBeforeReceipt(t *testing.T) {
	s, rt, r := bridgeFixture(t, []string{"fix_kill"}, []string{"990010"}, 1)
	acceptFixture(t, s, rt, r)
	ctx := context.Background()
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	// Widen the real post-adapter window: the receipt insert sleeps inside the
	// fixture database only, so the kill can land after the adapter returned
	// and before anything commits.
	for _, ddl := range []string{
		`CREATE FUNCTION fixture_slow_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(5); RETURN NEW; END $$`,
		`CREATE TRIGGER fixture_slow_receipt BEFORE INSERT ON bridge.outbox FOR EACH ROW EXECUTE FUNCTION fixture_slow_receipt()`,
	} {
		if _, err := s.Pool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(t.TempDir(), "adapter_entered")
	cmd, out := remediationSpawnChild(t, "kill", s.Pool.Config().ConnString(), r.OperationID, r.AccountID, r.RuntimeID, marker)
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	remediationWait(t, "child adapter return and stuck receipt insert", func() bool {
		if _, err := os.Stat(marker); err != nil {
			return false
		}
		return remediationReceiptInsertActive(t, s)
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatalf("child was expected to die from SIGKILL; output=%s", out.String())
	}
	// The killed child's backend is still inside the trigger sleep; the server
	// only notices the dead client after it wakes, so retry the DDL instead of
	// racing the orphan's table lock.
	remediationRetry(t, "drop slow receipt trigger", func() error {
		_, err := s.Pool.Exec(ctx, `DROP TRIGGER fixture_slow_receipt ON bridge.outbox`)
		return err
	})
	remediationWait(t, "killed backend to leave its receipt transaction", func() bool {
		return !remediationReceiptInsertActive(t, s)
	})
	if got := scalarInt(t, s, `SELECT count(*) FROM bridge.outbox WHERE workspace_id=$1 AND operation_id=$2`, r.WorkspaceID, r.OperationID); got != 0 {
		t.Fatalf("receipt commits=%d after the kill; window was not reproduced", got)
	}
	if got := gatewayState(t, s, r.OperationID); got != "DISPATCHING" {
		t.Fatalf("gateway state after kill=%s", got)
	}
	if got := executionState(t, s, r.OperationID); got != "DISPATCHING" {
		t.Fatalf("execution state after kill=%s", got)
	}
	if got := permitState(t, s, r.OperationID); got != "consumed" {
		t.Fatalf("permit state after kill=%s", got)
	}

	// Recovery runs from durable state only, through a fresh connection taken
	// from the pool of the surviving process. This is pool-level re-opening, not
	// a new process and not a new Store instance.
	recovered, err := s.RecoverDispatching(ctx, rt)
	if err != nil || recovered != 1 {
		t.Fatalf("recover dispatching n=%d err=%v", recovered, err)
	}
	if got := gatewayState(t, s, r.OperationID); got != "UNKNOWN" {
		t.Fatalf("gateway state after recovery=%s", got)
	}
	if got := executionState(t, s, r.OperationID); got != "UNKNOWN" {
		t.Fatalf("execution state after recovery=%s", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND execution_id=$2 AND held`, r.WorkspaceID, r.ExecutionID); got != 1 {
		t.Fatalf("send lock after recovery=%d; an unknown effect must keep the account lock", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='reserved'`, r.WorkspaceID, r.TaskID, r.TargetID); got != 1 {
		t.Fatalf("reserved slot after recovery=%d", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND target_id=$2 AND state='active'`, r.WorkspaceID, r.TargetID); got != 1 {
		t.Fatalf("active claim after recovery=%d", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("recovery credited results=%d", got)
	}
	// Recovery is not a resend: a later Dispatch returns persisted state only.
	adapter := &fakeJoinAdapter{}
	if _, err := s.Dispatch(ctx, rt, r.OperationID, identityOK, adapter); err != nil {
		t.Fatalf("dispatch after recovery: %v", err)
	}
	if adapter.count() != 0 {
		t.Fatalf("recovery caused %d additional adapter calls", adapter.count())
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE core.execution SET observed_at=clock_timestamp()-interval '25 hours' WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReconcileTimeouts(ctx, r.WorkspaceID); err != nil || n != 1 {
		t.Fatalf("24h reconciliation n=%d err=%v", n, err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND task_id=$2`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("unknown outcome scheduled retries=%d", got)
	}
	t.Logf("SIGKILL child pid=%d after adapter return; receipts=0; recovery=UNKNOWN with slot/claim/lock retained; adapter_calls_total=1", cmd.Process.Pid)
}

// Evidence B: the permit is consumed and committed, then the worker stalls
// before the adapter call. Once the permit expires or the runtime generation
// moves on, the old worker must not send, must record a definite pre-send
// no-effect, and must leave the work retryable within budget.
func TestRemediationLongPauseAfterConsumeNeverSends(t *testing.T) {
	cases := []struct {
		name   string
		ttl    time.Duration
		drift  bool
		group  string
		expect string
		wait   time.Duration
	}{
		{name: "permit_expires_before_send", ttl: 2 * time.Second, group: "990011", expect: "PERMIT_EXPIRED", wait: 0},
		{name: "runtime_generation_moves_before_send", ttl: 30 * time.Second, drift: true, group: "990012", expect: "ACCOUNT_RESTRICTED", wait: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, rt, r := bridgeFixture(t, []string{"fix_pause_" + tc.group}, []string{tc.group}, 1)
			acceptFixture(t, s, rt, r)
			ctx := context.Background()
			if _, err := s.IssuePermit(ctx, rt, r.OperationID, tc.ttl); err != nil {
				t.Fatal(err)
			}
			// The attempt row lock parks the child after its consume commit and
			// before the adapter call: exactly the long-pause window.
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
				// A failed assertion must not strand the hold or the child: the
				// fixture cleanup would otherwise wait on both.
				_ = hold.Rollback(context.Background())
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			remediationWait(t, "permit consumption before the paused send", func() bool {
				return permitState(t, s, r.OperationID) == "consumed"
			})
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("child entered the adapter before the pause was released")
			}
			if tc.drift {
				if _, err := s.Pool.Exec(ctx, `UPDATE core.account SET identity_generation=identity_generation+1 WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.AccountID); err != nil {
					t.Fatal(err)
				}
			} else {
				time.Sleep(tc.ttl + time.Second)
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
				t.Fatal("old worker called the adapter after its permit/generation went stale")
			}
			if !strings.Contains(out.String(), tc.expect) {
				t.Fatalf("child output=%s want refusal %s", out.String(), tc.expect)
			}
			if got := gatewayState(t, s, r.OperationID); got != "OBSERVED" {
				t.Fatalf("gateway state after refusal=%s", got)
			}
			if got := fixtureString(t, s, `SELECT COALESCE(disposition,'') FROM bridge.gateway_operation WHERE workspace_id=$1 AND operation_id=$2`, r.WorkspaceID, r.OperationID); got != "failed_no_effect" {
				t.Fatalf("gateway disposition after refusal=%s", got)
			}
			if got := executionState(t, s, r.OperationID); got != "OBSERVED" {
				t.Fatalf("execution state after refusal=%s", got)
			}
			if got := fixtureString(t, s, `SELECT COALESCE(effect_certainty,'') FROM core.execution WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.ExecutionID); got != "no_effect" {
				t.Fatalf("execution certainty after refusal=%s", got)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND execution_id=$2 AND held`, r.WorkspaceID, r.ExecutionID); got != 0 {
				t.Fatalf("send lock after a definite pre-send refusal=%d", got)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='free'`, r.WorkspaceID, r.TaskID); got != 1 {
				t.Fatalf("free slots after a definite pre-send refusal=%d", got)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND target_id=$2 AND state='active'`, r.WorkspaceID, r.TargetID); got != 0 {
				t.Fatalf("active claim after a definite pre-send refusal=%d", got)
			}
			if got := scalarInt(t, s, `SELECT COALESCE(sum(consumed),0) FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2`, r.WorkspaceID, r.AccountID); got != 0 {
				t.Fatalf("daily consumed quota after a definite pre-send refusal=%d", got)
			}
			if err := s.ScheduleRetry(ctx, r.WorkspaceID, r.ExecutionID, "reselect", time.Now()); err != nil {
				t.Fatalf("no-effect refusal must stay retryable: %v", err)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND task_id=$2 AND state='scheduled'`, r.WorkspaceID, r.TaskID); got != 1 {
				t.Fatalf("scheduled retries after refusal=%d", got)
			}
			t.Logf("paused worker refused with %s; adapter_calls=0; holds released only as definite no-effect", tc.expect)
		})
	}
}
