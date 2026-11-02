package taskengine

import (
	"context"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/jackc/pgx/v5"
)

// The blocker PID plus pg_blocking_pids proves Dispatch has passed consume
// and is parked in this workspace's contentLock, not an earlier row lock.
func b3ContentPark(t *testing.T, s *Store, rt RuntimeActor, r Reservation, ctx context.Context, adapter *fakeJoinAdapter) (pgx.Tx, <-chan error) {
	t.Helper()
	hold, err := s.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
	if err := contentLock(context.Background(), hold, r.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err := hold.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, err := s.Dispatch(workerCtx, rt, r.OperationID, identityOK, adapter)
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		_ = hold.Rollback(context.Background())
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("Dispatch goroutine failed to exit")
		}
	})
	remediationWait(t, "Dispatch blocked by exact workspace content-lock holder", func() bool {
		return scalarInt(t, s, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
   AND wait_event_type='Lock' AND wait_event='advisory'
   AND application_name='bba_content_guard'
   AND query LIKE '%pg_advisory_xact_lock_shared(hashtextextended%'
   AND $1::int=ANY(pg_blocking_pids(pid))`, blocker) == 1
	})
	if permitState(t, s, r.OperationID) != "consumed" || activeLocalDispatches.count(r.WorkspaceID, r.TaskID) != 1 || adapter.count() != 0 {
		t.Fatal("invalid parked send barrier")
	}
	t.Logf("barrier: contentLock waiting on blocker PID=%d; permit=consumed; local_flight=1; adapter=0", blocker)
	return hold, done
}

func b3ContentDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Dispatch did not exit within bound")
		return nil
	}
}

func b3ContentExec(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestB3BridgeContentWaitAuthorization(t *testing.T) {
	for _, name := range []string{"pause", "cancel", "permit_expired", "task_expired", "identity_generation", "runtime_revoked", "owner_epoch", "recovery_unknown", "allowed"} {
		t.Run(name, func(t *testing.T) {
			s, rt, r := bridgeFixture(t, []string{"b3_wait_" + name}, []string{"996901"}, 1)
			acceptFixture(t, s, rt, r)
			ttl := 30 * time.Second
			if name == "permit_expired" {
				ttl = 2 * time.Second
			}
			if _, err := s.IssuePermit(context.Background(), rt, r.OperationID, ttl); err != nil {
				t.Fatal(err)
			}
			adapter := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "b3_wait_receipt"}}
			hold, done := b3ContentPark(t, s, rt, r, context.Background(), adapter)
			wantCode, wantState, wantEffect := "EXTERNAL_OUTCOME_UNKNOWN", "UNKNOWN", "unknown"
			switch name {
			case "pause", "cancel":
				action := contract.TaskControlRequestActionPause
				if name == "cancel" {
					action = contract.TaskControlRequestActionCancel
				}
				if err := fourthPauseControl(t, s, r.TaskID, action, "b3-content-"+name, "blocked in content lock"); err != nil {
					t.Fatal(err)
				}
			case "permit_expired":
				remediationWait(t, "database clock proves permit expired while content lock held", func() bool {
					return scalarInt(t, s, `SELECT count(*) FROM core.dispatch_permit WHERE workspace_id=$1 AND operation_id=$2 AND expires_at<=clock_timestamp()`, r.WorkspaceID, r.OperationID) == 1
				})
				wantCode, wantState, wantEffect = "PERMIT_EXPIRED", "OBSERVED", "no_effect"
			case "task_expired":
				sweepExpireDeadline(t, s, r.TaskID)
				wantCode, wantState, wantEffect = "DEADLINE_EXPIRED", "OBSERVED", "no_effect"
			case "identity_generation":
				b3ContentExec(t, s, `UPDATE core.account SET identity_generation=identity_generation+1 WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.AccountID)
				wantCode, wantState, wantEffect = "ACCOUNT_RESTRICTED", "OBSERVED", "no_effect"
			case "runtime_revoked":
				b3ContentExec(t, s, `UPDATE core.runtime_credentials SET active=false WHERE workspace_id=$1 AND account_id=$2`, r.WorkspaceID, r.AccountID)
				// Preserve the old refusal contract: a revoked actor cannot persist an
				// outcome. The consumed DISPATCHING row is left for authorized recovery.
				wantCode, wantState, wantEffect = "RESOURCE_NOT_FOUND", "DISPATCHING", "unknown"
			case "owner_epoch":
				b3ContentExec(t, s, `UPDATE core.account SET owner_epoch=owner_epoch+1 WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.AccountID)
				wantCode, wantState, wantEffect = "RESOURCE_NOT_FOUND", "DISPATCHING", "unknown"
			case "recovery_unknown":
				if n, err := s.RecoverDispatching(context.Background(), rt); err != nil || n != 1 {
					t.Fatalf("recover=%d %v", n, err)
				}
			case "allowed":
				wantCode, wantState, wantEffect = "", "OBSERVED", "confirmed_submitted"
			}
			if err := hold.Rollback(context.Background()); err != nil {
				t.Fatal(err)
			}
			err := b3ContentDone(t, done)
			wantCalls := 0
			if name == "allowed" {
				wantCalls = 1
			}
			// Log all observed facts before assertions, including RED evidence.
			t.Logf("result: error=%v code=%s adapter=%d gateway=%s execution=%s permit=%s flight=%d", err, Code(err), adapter.count(), gatewayState(t, s, r.OperationID), executionState(t, s, r.OperationID), permitState(t, s, r.OperationID), activeLocalDispatches.count(r.WorkspaceID, r.TaskID))
			if adapter.count() != wantCalls {
				t.Errorf("adapter calls=%d want %d", adapter.count(), wantCalls)
			}
			if Code(err) != wantCode {
				t.Errorf("code=%s want %s", Code(err), wantCode)
			}
			if gatewayState(t, s, r.OperationID) != wantState || executionState(t, s, r.OperationID) != wantState {
				t.Errorf("durable states want %s", wantState)
			}
			if got := fixtureString(t, s, `SELECT effect_certainty FROM core.execution WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.ExecutionID); got != wantEffect {
				t.Errorf("effect=%s want %s", got, wantEffect)
			}
			if permitState(t, s, r.OperationID) != "consumed" || activeLocalDispatches.count(r.WorkspaceID, r.TaskID) != 0 {
				t.Error("permit or local flight contract changed")
			}
			if name == "pause" && fourthPauseTaskState(t, s, r.TaskID) != "PAUSED" {
				t.Error("pause did not converge")
			}
			if name == "cancel" && fourthPauseTaskState(t, s, r.TaskID) != "CANCELLED" {
				t.Error("cancel was reopened")
			}
			if wantState == "UNKNOWN" {
				fourthPauseAssertPreserved(t, "quarantined", fourthPauseObserve(t, s, r, adapter), fourthPauseSnapshot{held: 1, reserved: 1, activeClaims: 1, consumed: 1, executions: 1, unknown: 1})
				if err := s.ScheduleRetry(context.Background(), r.WorkspaceID, r.ExecutionID, "reselect", time.Now()); err == nil {
					t.Error("UNKNOWN permitted retry")
				}
			}
			if wantState == "DISPATCHING" {
				fourthPauseAssertPreserved(t, "revoked owner retains recovery holds", fourthPauseObserve(t, s, r, adapter), fourthPauseSnapshot{held: 1, reserved: 1, activeClaims: 1, consumed: 1, executions: 1})
			}
			if wantEffect == "no_effect" {
				fourthPauseAssertPreserved(t, "no effect", fourthPauseObserve(t, s, r, adapter), fourthPauseSnapshot{free: 1, executions: 1})
			}
			before := adapter.count()
			_, _ = s.Dispatch(context.Background(), rt, r.OperationID, identityOK, adapter)
			if adapter.count() != before {
				t.Error("replay sent again")
			}
			t.Logf("replay additional adapter calls=%d", adapter.count()-before)
			if got := scalarInt(t, s, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`); got != 0 {
				t.Errorf("leaked fixture advisory locks=%d", got)
			}
			t.Log("cleanup readback: fixture advisory locks=0; local flight=0")
		})
	}
}

func TestB3BridgeContentWaitBounded(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "default_timeout"} {
		t.Run(mode, func(t *testing.T) {
			s, rt, r := bridgeFixture(t, []string{"b3_bound_" + mode}, []string{"996902"}, 1)
			acceptFixture(t, s, rt, r)
			if _, err := s.IssuePermit(context.Background(), rt, r.OperationID, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" {
				var c context.CancelFunc
				ctx, c = context.WithTimeout(ctx, 2*time.Second)
				defer c()
			}
			adapter := &fakeJoinAdapter{}
			hold, done := b3ContentPark(t, s, rt, r, ctx, adapter)
			started := time.Now()
			if mode == "cancel" {
				cancel()
			}
			// Keep the blocker held: exit must be due to cancellation / timeout.
			err := b3ContentDone(t, done)
			if err == nil || adapter.count() != 0 || activeLocalDispatches.count(r.WorkspaceID, r.TaskID) != 0 {
				t.Fatalf("err=%v calls=%d flight=%d", err, adapter.count(), activeLocalDispatches.count(r.WorkspaceID, r.TaskID))
			}
			if gatewayState(t, s, r.OperationID) != "DISPATCHING" || permitState(t, s, r.OperationID) != "consumed" {
				t.Error("ambiguous consumed attempt was changed")
			}
			if err := hold.Rollback(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, _ = s.Dispatch(context.Background(), rt, r.OperationID, identityOK, adapter)
			if adapter.count() != 0 {
				t.Error("cancelled/timeout replay sent")
			}
			t.Logf("bounded exit=%v elapsed=%s; adapter=0 flight=0 replay=0", err, time.Since(started))
		})
	}
}
