package taskengine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeJoinAdapter struct {
	mu      sync.Mutex
	calls   int
	outcome SendOutcome
	err     error
	delay   time.Duration
}

func (a *fakeJoinAdapter) RequestJoin(ctx context.Context, res Reservation) (SendOutcome, error) {
	a.mu.Lock()
	a.calls++
	outcome, err := a.outcome, a.err
	delay := a.delay
	a.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	return outcome, err
}

func (a *fakeJoinAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func identityOK(context.Context, Reservation) error { return nil }

func acceptFixture(t *testing.T, s *Store, actor RuntimeActor, res Reservation) Reservation {
	t.Helper()
	accepted, err := s.AcceptOperation(context.Background(), actor, res)
	if err != nil {
		t.Fatalf("accept operation: %v", err)
	}
	return accepted
}

func fixtureString(t *testing.T, s *Store, sql string, args ...any) string {
	t.Helper()
	var value string
	if err := s.Pool.QueryRow(context.Background(), sql, args...).Scan(&value); err != nil {
		t.Fatalf("scalar string: %v", err)
	}
	return value
}

func gatewayState(t *testing.T, s *Store, operationID string) string {
	t.Helper()
	return fixtureString(t, s, `SELECT state FROM bridge.gateway_operation WHERE workspace_id=$1 AND operation_id=$2`, "ws_fixture", operationID)
}

func permitState(t *testing.T, s *Store, operationID string) string {
	t.Helper()
	return fixtureString(t, s, `SELECT state FROM core.dispatch_permit WHERE workspace_id=$1 AND operation_id=$2`, "ws_fixture", operationID)
}

func executionState(t *testing.T, s *Store, operationID string) string {
	t.Helper()
	return fixtureString(t, s, `SELECT state FROM core.execution WHERE workspace_id=$1 AND operation_id=$2`, "ws_fixture", operationID)
}

func bridgeFixture(t *testing.T, accounts, groups []string, goal int) (*Store, RuntimeActor, Reservation) {
	t.Helper()
	store := fixtureStore(t)
	for _, account := range accounts {
		seedAccount(t, store, account, "rt_"+account, 5)
	}
	task := createReadyTask(t, store, accounts, groups, goal, 3)
	res := reserveFixture(t, store, task.Id, accounts[0])
	return store, fixtureRuntime(accounts[0], "rt_"+accounts[0]), res
}

func TestBridgeDuplicateOperation(t *testing.T) {
	store, actor, res := bridgeFixture(t, []string{"acc_dup"}, []string{"700001"}, 1)
	first := acceptFixture(t, store, actor, res)
	second, err := store.AcceptOperation(context.Background(), actor, res)
	if err != nil {
		t.Fatalf("duplicate accept: %v", err)
	}
	if first.ExecutionID != second.ExecutionID || first.OperationID != second.OperationID ||
		first.AccountID != second.AccountID || first.TargetID != second.TargetID || first.Fence != second.Fence ||
		first.ExecutionID != res.ExecutionID || first.Digest != res.Digest ||
		!first.Deadline.Equal(second.Deadline) || !first.Deadline.Equal(res.Deadline) || first.GroupID != res.GroupID {
		t.Fatalf("duplicate accept changed canonical reservation: %#v vs %#v", first, second)
	}
	if got := scalarInt(t, store, `SELECT count(*) FROM bridge.gateway_operation WHERE workspace_id=$1 AND operation_id=$2`, "ws_fixture", res.OperationID); got != 1 {
		t.Fatalf("gateway ledger rows=%d", got)
	}
	if got := executionState(t, store, res.OperationID); got != "ACCEPTED" {
		t.Fatalf("execution state=%s", got)
	}
	persisted, err := store.GetReservation(context.Background(), actor, res.OperationID)
	if err != nil {
		t.Fatalf("get reservation: %v", err)
	}
	if persisted.Digest != res.Digest || persisted.ExecutionID != res.ExecutionID || !persisted.Deadline.Equal(res.Deadline) {
		t.Fatalf("persisted reservation=%#v", persisted)
	}
	op, err := store.GetOperation(context.Background(), actor, res.OperationID)
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if string(op.State) != "ACCEPTED" || string(op.EffectCertainty) != "no_effect" {
		t.Fatalf("operation projection=%+v", op)
	}
}

func TestOperationPayloadConflict(t *testing.T) {
	store, actor, res := bridgeFixture(t, []string{"acc_conflict"}, []string{"700002"}, 1)
	acceptFixture(t, store, actor, res)
	cases := []struct {
		name string
		edit func(*Reservation)
	}{
		{"digest", func(r *Reservation) { r.Digest = strings.Repeat("a", 64) }},
		{"target", func(r *Reservation) { r.TargetID = "target_other" }},
		{"scope", func(r *Reservation) { r.ScopeRevision++ }},
		{"fence", func(r *Reservation) { r.Fence++ }},
		{"deadline", func(r *Reservation) { r.Deadline = r.Deadline.Add(time.Second) }},
		{"group", func(r *Reservation) { r.GroupID = "799999" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tampered := res
			tc.edit(&tampered)
			if _, err := store.AcceptOperation(context.Background(), actor, tampered); Code(err) != "OPERATION_PAYLOAD_CONFLICT" {
				t.Fatalf("want payload conflict, got %v", err)
			}
			if got := scalarInt(t, store, `SELECT count(*) FROM bridge.gateway_operation WHERE workspace_id=$1`, "ws_fixture"); got != 1 {
				t.Fatalf("payload conflict inserted ledger rows: %d", got)
			}
			if got := gatewayState(t, store, res.OperationID); got != "ACCEPTED" {
				t.Fatalf("conflict moved ledger to %s", got)
			}
		})
	}
}

func TestExpiredPermitNoSend(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		store, actor, res := bridgeFixture(t, []string{"acc_expired"}, []string{"700003"}, 1)
		acceptFixture(t, store, actor, res)
		if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Millisecond); err != nil {
			t.Fatalf("issue permit: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		adapter := &fakeJoinAdapter{}
		if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); Code(err) != "PERMIT_EXPIRED" {
			t.Fatalf("want expired permit error, got %v", err)
		}
		if adapter.count() != 0 {
			t.Fatalf("expired permit invoked adapter %d times", adapter.count())
		}
		if got := gatewayState(t, store, res.OperationID); got != "REJECTED_BEFORE_SEND" {
			t.Fatalf("gateway state=%s", got)
		}
		if got := permitState(t, store, res.OperationID); got != "expired" {
			t.Fatalf("permit state=%s", got)
		}
		if got := executionState(t, store, res.OperationID); got != "REJECTED_BEFORE_SEND" {
			t.Fatalf("execution state=%s", got)
		}
		if got := scalarInt(t, store, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND account_id=$2 AND held`, "ws_fixture", "acc_expired"); got != 0 {
			t.Fatalf("expired permit retained account lock")
		}
	})

	t.Run("missing permit", func(t *testing.T) {
		store, actor, res := bridgeFixture(t, []string{"acc_missing"}, []string{"700004"}, 1)
		acceptFixture(t, store, actor, res)
		if _, err := store.Pool.Exec(context.Background(), `UPDATE bridge.gateway_operation SET state='DISPATCHING',disposition=NULL,effect_certainty='unknown' WHERE workspace_id=$1 AND operation_id=$2`, "ws_fixture", res.OperationID); err != nil {
			t.Fatal(err)
		}
		adapter := &fakeJoinAdapter{}
		if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); Code(err) != "PERMIT_MISSING" {
			t.Fatalf("want missing permit error, got %v", err)
		}
		if adapter.count() != 0 {
			t.Fatalf("missing permit invoked adapter %d times", adapter.count())
		}
		if got := gatewayState(t, store, res.OperationID); got != "REJECTED_BEFORE_SEND" {
			t.Fatalf("gateway state=%s", got)
		}
	})

	t.Run("consumed permit", func(t *testing.T) {
		store, actor, res := bridgeFixture(t, []string{"acc_consumed"}, []string{"700005"}, 1)
		acceptFixture(t, store, actor, res)
		if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second); err != nil {
			t.Fatalf("issue permit: %v", err)
		}
		if _, err := store.Pool.Exec(context.Background(), `UPDATE core.dispatch_permit SET state='consumed' WHERE workspace_id=$1 AND operation_id=$2`, "ws_fixture", res.OperationID); err != nil {
			t.Fatal(err)
		}
		adapter := &fakeJoinAdapter{}
		if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); Code(err) != "PERMIT_CONSUMED" {
			t.Fatalf("want consumed permit error, got %v", err)
		}
		if adapter.count() != 0 {
			t.Fatalf("consumed permit invoked adapter %d times", adapter.count())
		}
	})

	t.Run("database unavailable", func(t *testing.T) {
		store, actor, res := bridgeFixture(t, []string{"acc_down"}, []string{"700006"}, 1)
		acceptFixture(t, store, actor, res)
		if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second); err != nil {
			t.Fatalf("issue permit: %v", err)
		}
		store.Close()
		adapter := &fakeJoinAdapter{}
		if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); err == nil {
			t.Fatal("dispatch against closed store succeeded")
		}
		if adapter.count() != 0 {
			t.Fatalf("database outage invoked adapter %d times", adapter.count())
		}
	})
}

func TestPauseStopsNewDispatch(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*testing.T, *Store, Reservation)
		wantErr string
		adapter int
	}{
		{
			name: "task paused",
			mutate: func(t *testing.T, s *Store, res Reservation) {
				if _, err := s.Pool.Exec(context.Background(), `UPDATE core.task SET state='PAUSED',version=version+1 WHERE workspace_id=$1 AND id=$2`, "ws_fixture", res.TaskID); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "OPERATION_NOT_DISPATCHABLE",
		},
		{
			name: "account paused",
			mutate: func(t *testing.T, s *Store, res Reservation) {
				if _, err := s.Pool.Exec(context.Background(), `UPDATE core.account SET control_state='paused' WHERE workspace_id=$1 AND id=$2`, "ws_fixture", res.AccountID); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "ACCOUNT_RESTRICTED",
		},
		{
			name: "approval revoked",
			mutate: func(t *testing.T, s *Store, res Reservation) {
				if _, err := s.Pool.Exec(context.Background(), `UPDATE core.approval SET revoked=true WHERE workspace_id=$1 AND id=(SELECT approval_id FROM core.task WHERE workspace_id=$1 AND id=$2)`, "ws_fixture", res.TaskID); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "OPERATION_NOT_DISPATCHABLE",
		},
		{
			name: "slot removed",
			mutate: func(t *testing.T, s *Store, res Reservation) {
				if _, err := s.Pool.Exec(context.Background(), `DELETE FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3`, "ws_fixture", res.TaskID, res.TargetID); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "NO_CAPACITY",
		},
		{
			name: "owner epoch mismatch",
			mutate: func(t *testing.T, s *Store, res Reservation) {
				// Runtime credential owner stays 1; the bridge presents epoch 2.
			},
			wantErr: "RESOURCE_NOT_FOUND",
		},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			group := []string{"71000" + string(rune('0'+index))}
			store, actor, res := bridgeFixture(t, []string{"acc_pause" + string(rune('0'+index))}, group, 1)
			acceptFixture(t, store, actor, res)
			tc.mutate(t, store, res)
			if tc.name == "owner epoch mismatch" {
				actor.OwnerEpoch = 2
			}
			adapter := &fakeJoinAdapter{}
			if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second); Code(err) != tc.wantErr {
				t.Fatalf("want %s, got %v", tc.wantErr, err)
			}
			if got := scalarInt(t, store, `SELECT count(*) FROM core.dispatch_permit WHERE workspace_id=$1 AND operation_id=$2`, "ws_fixture", res.OperationID); got != 0 {
				t.Fatalf("permit issued while paused")
			}
			if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); err == nil {
				t.Fatal("dispatch succeeded without a permit")
			}
			if adapter.count() != tc.adapter {
				t.Fatalf("adapter calls=%d", adapter.count())
			}
		})
	}
}

func TestPermitDuplicateNoSecondSend(t *testing.T) {
	store, actor, res := bridgeFixture(t, []string{"acc_permit"}, []string{"700007"}, 1)
	acceptFixture(t, store, actor, res)
	first, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second)
	if err != nil {
		t.Fatalf("issue permit: %v", err)
	}
	second, err := store.IssuePermit(context.Background(), actor, res.OperationID, 30*time.Second)
	if err != nil {
		t.Fatalf("duplicate permit: %v", err)
	}
	if first.ID != second.ID || !first.ExpiresAt.Equal(second.ExpiresAt) || !first.IssuedAt.Equal(second.IssuedAt) {
		t.Fatalf("duplicate permit changed identity or expiry: %#v %#v", first, second)
	}
	if second.State != "issued" {
		t.Fatalf("duplicate permit state=%s", second.State)
	}
	adapter := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fixture_submitted_evidence", RequestRef: "fixture_request_ref"}}
	sent, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if adapter.count() != 1 || sent.Disposition != "submitted" || sent.RequestRef != "fixture_request_ref" {
		t.Fatalf("dispatch outcome=%+v calls=%d", sent, adapter.count())
	}
	third, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second)
	if err != nil {
		t.Fatalf("permit after consumption: %v", err)
	}
	if third.ID != first.ID || !third.ExpiresAt.Equal(first.ExpiresAt) || third.State != "consumed" {
		t.Fatalf("consumed duplicate changed permit: %#v", third)
	}
	again, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter)
	if err != nil {
		t.Fatalf("duplicate dispatch returned error: %v", err)
	}
	if adapter.count() != 1 {
		t.Fatalf("duplicate dispatch sent again: %d", adapter.count())
	}
	if again.Disposition != "submitted" {
		t.Fatalf("duplicate dispatch outcome=%+v", again)
	}
	if got := gatewayState(t, store, res.OperationID); got != "OBSERVED" {
		t.Fatalf("gateway state=%s", got)
	}
	if got := executionState(t, store, res.OperationID); got != "OBSERVED" {
		t.Fatalf("execution state=%s", got)
	}
	if got := scalarInt(t, store, `SELECT count(*) FROM bridge.outbox WHERE workspace_id=$1 AND operation_id=$2 AND event_type='execution.observed'`, "ws_fixture", res.OperationID); got != 1 {
		t.Fatalf("bridge outbox rows=%d", got)
	}
	if got := scalarInt(t, store, `SELECT count(*) FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2 AND consumed=1 AND reserved=0`, "ws_fixture", "acc_permit"); got != 1 {
		t.Fatalf("usage not consumed exactly once")
	}
	if got := scalarInt(t, store, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND account_id=$2 AND held`, "ws_fixture", "acc_permit"); got != 0 {
		t.Fatalf("account lock retained after returned send")
	}
	permit, err := store.GetPermit(context.Background(), actor, res.OperationID)
	if err != nil {
		t.Fatalf("get permit: %v", err)
	}
	if permit.PermitId != first.ID || string(permit.State) != "consumed" {
		t.Fatalf("permit projection=%+v", permit)
	}
	op, err := store.GetOperation(context.Background(), actor, res.OperationID)
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if string(op.State) != "OBSERVED" || !op.Disposition.IsSpecified() || string(op.Disposition.MustGet()) != "submitted" {
		t.Fatalf("operation projection=%+v", op)
	}
}

func TestBridgeConcurrentDispatchSingleSend(t *testing.T) {
	store, actor, res := bridgeFixture(t, []string{"acc_race"}, []string{"700008"}, 1)
	acceptFixture(t, store, actor, res)
	if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, 5*time.Second); err != nil {
		t.Fatalf("issue permit: %v", err)
	}
	adapter := &fakeJoinAdapter{
		outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fixture_race_evidence"},
		delay:   50 * time.Millisecond,
	}
	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, errs[index] = store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter)
		}(i)
	}
	wg.Wait()
	if adapter.count() != 1 {
		t.Fatalf("concurrent dispatch invoked adapter %d times", adapter.count())
	}
	if got := scalarInt(t, store, `SELECT count(*) FROM bridge.outbox WHERE workspace_id=$1 AND operation_id=$2`, "ws_fixture", res.OperationID); got != 1 {
		t.Fatalf("concurrent dispatch wrote %d observations", got)
	}
	if got := executionState(t, store, res.OperationID); got != "OBSERVED" {
		t.Fatalf("execution state=%s", got)
	}
	for _, err := range errs {
		if err != nil && Code(err) != "PERMIT_CONSUMED" && Code(err) != "OPERATION_NOT_DISPATCHABLE" && Code(err) != "PERMIT_EXPIRED" {
			t.Fatalf("unexpected concurrent dispatch error: %v", err)
		}
	}
}

func TestRecoverDispatchingUnknownQuarantine(t *testing.T) {
	store, actor, res := bridgeFixture(t, []string{"acc_recover"}, []string{"700009"}, 1)
	acceptFixture(t, store, actor, res)
	if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second); err != nil {
		t.Fatalf("issue permit: %v", err)
	}
	recovered, err := store.RecoverDispatching(context.Background(), actor)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered=%d", recovered)
	}
	if got := gatewayState(t, store, res.OperationID); got != "UNKNOWN" {
		t.Fatalf("gateway state=%s", got)
	}
	if got := executionState(t, store, res.OperationID); got != "UNKNOWN" {
		t.Fatalf("execution state=%s", got)
	}
	if got := permitState(t, store, res.OperationID); got != "issued" {
		t.Fatalf("recovery must not rewrite permit state, got %s", got)
	}
	if got := scalarInt(t, store, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND account_id=$2 AND execution_id=$3 AND held`, "ws_fixture", "acc_recover", res.ExecutionID); got != 1 {
		t.Fatalf("recovery released the send lock")
	}
	if got := scalarInt(t, store, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='reserved'`, "ws_fixture", res.TaskID, res.TargetID); got != 1 {
		t.Fatalf("recovery released goal slot")
	}
	if got := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND target_id=$2 AND state='active'`, "ws_fixture", res.TargetID); got != 1 {
		t.Fatalf("recovery released target claim")
	}
	op, err := store.GetOperation(context.Background(), actor, res.OperationID)
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if string(op.State) != "UNKNOWN" || string(op.EffectCertainty) != "unknown" {
		t.Fatalf("operation projection=%+v", op)
	}
	adapter := &fakeJoinAdapter{}
	if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); err != nil {
		t.Fatalf("quarantined dispatch: %v", err)
	}
	if adapter.count() != 0 {
		t.Fatalf("quarantined operation sent %d times", adapter.count())
	}
	// A duplicate accept never resurrects or replays a quarantined operation.
	if _, err := store.AcceptOperation(context.Background(), actor, res); err != nil {
		t.Fatalf("duplicate accept after recovery: %v", err)
	}
	if got := gatewayState(t, store, res.OperationID); got != "UNKNOWN" {
		t.Fatalf("duplicate accept rewrote state to %s", got)
	}
	other := fixtureRuntime("acc_recover", "rt_other")
	if recovered, err := store.RecoverDispatching(context.Background(), other); err != nil || recovered != 0 {
		t.Fatalf("foreign runtime recovery: n=%d err=%v", recovered, err)
	}
}

func TestIdentityCheckMandatory(t *testing.T) {
	store, actor, res := bridgeFixture(t, []string{"acc_identity"}, []string{"700010"}, 1)
	acceptFixture(t, store, actor, res)
	if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second); err != nil {
		t.Fatalf("issue permit: %v", err)
	}
	adapter := &fakeJoinAdapter{}
	if _, err := store.Dispatch(context.Background(), actor, res.OperationID, nil, adapter); Code(err) != "VALIDATION_FAILED" {
		t.Fatalf("nil identity check accepted: %v", err)
	}
	if adapter.count() != 0 {
		t.Fatalf("nil identity check sent")
	}
	failing := func(context.Context, Reservation) error { return problem("CHALLENGE_REQUIRED") }
	if _, err := store.Dispatch(context.Background(), actor, res.OperationID, failing, adapter); Code(err) != "CHALLENGE_REQUIRED" {
		t.Fatalf("identity failure not propagated: %v", err)
	}
	if adapter.count() != 0 {
		t.Fatalf("failed identity check sent")
	}
	if got := gatewayState(t, store, res.OperationID); got != "REJECTED_BEFORE_SEND" {
		t.Fatalf("gateway state=%s", got)
	}
	if got := permitState(t, store, res.OperationID); got != "expired" {
		t.Fatalf("permit state=%s", got)
	}
	if got := scalarInt(t, store, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND account_id=$2 AND held`, "ws_fixture", "acc_identity"); got != 0 {
		t.Fatalf("identity failure retained account lock")
	}
}

func TestStaleIdentityNoSend(t *testing.T) {
	cases := []struct {
		name   string
		mutate string
	}{
		{"generation changed", `UPDATE core.account SET identity_generation=identity_generation+1 WHERE workspace_id=$1 AND id=$2`},
		{"identity stale", `UPDATE core.account SET identity_status='stale' WHERE workspace_id=$1 AND id=$2`},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := "acc_stale" + string(rune('0'+index))
			store, actor, res := bridgeFixture(t, []string{account}, []string{"70002" + string(rune('0'+index))}, 1)
			acceptFixture(t, store, actor, res)
			if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second); err != nil {
				t.Fatalf("issue permit: %v", err)
			}
			if _, err := store.Pool.Exec(context.Background(), tc.mutate, "ws_fixture", account); err != nil {
				t.Fatal(err)
			}
			adapter := &fakeJoinAdapter{}
			if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); Code(err) != "ACCOUNT_RESTRICTED" {
				t.Fatalf("want restricted identity, got %v", err)
			}
			if adapter.count() != 0 {
				t.Fatalf("stale identity sent %d times", adapter.count())
			}
			if got := permitState(t, store, res.OperationID); got != "issued" {
				t.Fatalf("stale identity consumed permit: %s", got)
			}
			if got := executionState(t, store, res.OperationID); got != "ACCEPTED" {
				t.Fatalf("stale identity moved execution: %s", got)
			}
		})
	}
}

func TestExpirePermitsUnresolvedSafe(t *testing.T) {
	t.Run("unconsumed expired", func(t *testing.T) {
		store, actor, res := bridgeFixture(t, []string{"acc_expire"}, []string{"700030"}, 1)
		acceptFixture(t, store, actor, res)
		issued, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Millisecond)
		if err != nil {
			t.Fatalf("issue permit: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		expired, err := store.ExpirePermits(context.Background(), actor)
		if err != nil {
			t.Fatalf("expire permits: %v", err)
		}
		if expired != 1 {
			t.Fatalf("expired=%d", expired)
		}
		if got := gatewayState(t, store, res.OperationID); got != "REJECTED_BEFORE_SEND" {
			t.Fatalf("gateway state=%s", got)
		}
		if got := permitState(t, store, res.OperationID); got != "expired" {
			t.Fatalf("permit state=%s", got)
		}
		if got := executionState(t, store, res.OperationID); got != "REJECTED_BEFORE_SEND" {
			t.Fatalf("execution state=%s", got)
		}
		if got := scalarInt(t, store, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND account_id=$2 AND held`, "ws_fixture", "acc_expire"); got != 0 {
			t.Fatalf("expiry retained account lock")
		}
		if got := scalarInt(t, store, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='reserved'`, "ws_fixture", res.TaskID, res.TargetID); got != 1 {
			t.Fatalf("expiry released unresolved goal slot")
		}
		if got := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND target_id=$2 AND state='active'`, "ws_fixture", res.TargetID); got != 1 {
			t.Fatalf("expiry released unresolved target claim")
		}
		if got := scalarInt(t, store, `SELECT count(*) FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2 AND reserved=1 AND consumed=0`, "ws_fixture", "acc_expire"); got != 1 {
			t.Fatalf("expiry mutated reserved usage")
		}
		adapter := &fakeJoinAdapter{}
		if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); Code(err) != "PERMIT_EXPIRED" {
			t.Fatalf("dispatch after expiry: %v", err)
		}
		if adapter.count() != 0 {
			t.Fatalf("expired operation sent after cleanup")
		}
		again, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second)
		if err != nil {
			t.Fatalf("duplicate issuance after expiry: %v", err)
		}
		if again.ID != issued.ID || !again.ExpiresAt.Equal(issued.ExpiresAt) || again.State != "expired" {
			t.Fatalf("expiry changed stable receipt: %#v", again)
		}
		if more, err := store.ExpirePermits(context.Background(), actor); err != nil || more != 0 {
			t.Fatalf("second expiry n=%d err=%v", more, err)
		}
	})
	t.Run("expired last permit settles pause", func(t *testing.T) {
		store, actor, res := bridgeFixture(t, []string{"acc_pause_expiry"}, []string{"700032"}, 1)
		acceptFixture(t, store, actor, res)
		if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Pool.Exec(context.Background(), `UPDATE core.task SET state='PAUSING',version=version+1 WHERE workspace_id=$1 AND id=$2`, res.WorkspaceID, res.TaskID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		if n, err := store.ExpirePermits(context.Background(), actor); err != nil || n != 1 {
			t.Fatalf("expire n=%d err=%v", n, err)
		}
		var state string
		if err := store.Pool.QueryRow(context.Background(), `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, res.WorkspaceID, res.TaskID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "PAUSED" {
			t.Fatalf("task state=%s want PAUSED after safely expiring final permit", state)
		}
	})

	t.Run("consumed untouched", func(t *testing.T) {
		store, actor, res := bridgeFixture(t, []string{"acc_expire_sent"}, []string{"700031"}, 1)
		acceptFixture(t, store, actor, res)
		if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second); err != nil {
			t.Fatalf("issue permit: %v", err)
		}
		adapter := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fixture_expire_sent"}}
		if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if expired, err := store.ExpirePermits(context.Background(), actor); err != nil || expired != 0 {
			t.Fatalf("expiry touched consumed permit: n=%d err=%v", expired, err)
		}
		if got := gatewayState(t, store, res.OperationID); got != "OBSERVED" {
			t.Fatalf("gateway state=%s", got)
		}
		if got := permitState(t, store, res.OperationID); got != "consumed" {
			t.Fatalf("permit state=%s", got)
		}
		if adapter.count() != 1 {
			t.Fatalf("adapter calls=%d", adapter.count())
		}
	})

	t.Run("expire races dispatch", func(t *testing.T) {
		store, actor, res := bridgeFixture(t, []string{"acc_expire_race"}, []string{"700033"}, 1)
		acceptFixture(t, store, actor, res)
		if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Millisecond); err != nil {
			t.Fatalf("issue permit: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		adapter := &fakeJoinAdapter{}
		var dispatchErr error
		var expired int
		var expiryErr error
		done := make(chan struct{}, 2)
		go func() {
			_, dispatchErr = store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter)
			done <- struct{}{}
		}()
		go func() {
			expired, expiryErr = store.ExpirePermits(context.Background(), actor)
			done <- struct{}{}
		}()
		<-done
		<-done
		if expiryErr != nil {
			t.Fatalf("expire race: %v", expiryErr)
		}
		if expired != 0 && expired != 1 {
			t.Fatalf("expired=%d", expired)
		}
		if Code(dispatchErr) != "PERMIT_EXPIRED" {
			t.Fatalf("racing dispatch: %v", dispatchErr)
		}
		if adapter.count() != 0 {
			t.Fatalf("racing cleanup sent %d adapter calls", adapter.count())
		}
		if got := gatewayState(t, store, res.OperationID); got != "REJECTED_BEFORE_SEND" {
			t.Fatalf("gateway state=%s", got)
		}
		if got := permitState(t, store, res.OperationID); got != "expired" {
			t.Fatalf("permit state=%s", got)
		}
	})

	t.Run("unknown untouched", func(t *testing.T) {
		store, actor, res := bridgeFixture(t, []string{"acc_expire_unknown"}, []string{"700032"}, 1)
		acceptFixture(t, store, actor, res)
		if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Millisecond); err != nil {
			t.Fatalf("issue permit: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		if recovered, err := store.RecoverDispatching(context.Background(), actor); err != nil || recovered != 1 {
			t.Fatalf("recover n=%d err=%v", recovered, err)
		}
		if expired, err := store.ExpirePermits(context.Background(), actor); err != nil || expired != 0 {
			t.Fatalf("expiry downgraded unknown: n=%d err=%v", expired, err)
		}
		if got := gatewayState(t, store, res.OperationID); got != "UNKNOWN" {
			t.Fatalf("gateway state=%s", got)
		}
		if got := permitState(t, store, res.OperationID); got != "issued" {
			t.Fatalf("recovery/cleanup rewrote permit state: %s", got)
		}
		if got := executionState(t, store, res.OperationID); got != "UNKNOWN" {
			t.Fatalf("execution state=%s", got)
		}
		if got := scalarInt(t, store, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND account_id=$2 AND held`, "ws_fixture", "acc_expire_unknown"); got != 1 {
			t.Fatalf("unknown cleanup released send lock")
		}
		adapter := &fakeJoinAdapter{}
		if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); err != nil {
			t.Fatalf("unknown duplicate dispatch: %v", err)
		}
		if adapter.count() != 0 {
			t.Fatalf("unknown replayed after cleanup")
		}
	})
}

func TestAdapterErrorPersistsUnknownNoRetry(t *testing.T) {
	store, actor, res := bridgeFixture(t, []string{"acc_unknown"}, []string{"700011"}, 1)
	acceptFixture(t, store, actor, res)
	if _, err := store.IssuePermit(context.Background(), actor, res.OperationID, time.Second); err != nil {
		t.Fatalf("issue permit: %v", err)
	}
	adapter := &fakeJoinAdapter{err: errors.New("fixture transport timeout")}
	if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); Code(err) != "EXTERNAL_OUTCOME_UNKNOWN" {
		t.Fatalf("want unknown outcome error, got %v", err)
	}
	if adapter.count() != 1 {
		t.Fatalf("adapter calls=%d", adapter.count())
	}
	if got := gatewayState(t, store, res.OperationID); got != "UNKNOWN" {
		t.Fatalf("gateway state=%s", got)
	}
	if got := executionState(t, store, res.OperationID); got != "UNKNOWN" {
		t.Fatalf("execution state=%s", got)
	}
	if got := scalarInt(t, store, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND account_id=$2 AND held`, "ws_fixture", "acc_unknown"); got != 1 {
		t.Fatalf("unknown outcome released the send lock")
	}
	if _, err := store.Dispatch(context.Background(), actor, res.OperationID, identityOK, adapter); err != nil {
		t.Fatalf("duplicate dispatch on unknown: %v", err)
	}
	if adapter.count() != 1 {
		t.Fatalf("unknown outcome retried the adapter: %d", adapter.count())
	}
}
