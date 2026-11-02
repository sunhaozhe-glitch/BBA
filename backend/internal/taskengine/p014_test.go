package taskengine

// P0-14: four-account routing isolation and fault injection.
//
// These tests are fixture-only. They use four synthetic accounts and the real
// PostgreSQL fixture schema; they never call QQ, a live plugin, or a production
// database. The acceptance question is deliberately narrow: can a reservation,
// permit, dispatch, observation, or slot from one account be attributed to
// another account, and can a duplicate event, disconnect, or process restart
// create a second request or a second result?

import (
	"context"
	"sync"
	"testing"
	"time"
)

type p014Account struct {
	id      string
	runtime string
}

func p014SeedFour(t *testing.T, s *Store) []p014Account {
	t.Helper()
	return p014SeedFourWithQuota(t, s, 5)
}

func p014SeedFourWithQuota(t *testing.T, s *Store, quota int) []p014Account {
	t.Helper()
	accounts := []p014Account{
		{id: "acct_p014_a", runtime: "rt_p014_a"},
		{id: "acct_p014_b", runtime: "rt_p014_b"},
		{id: "acct_p014_c", runtime: "rt_p014_c"},
		{id: "acct_p014_d", runtime: "rt_p014_d"},
	}
	for _, account := range accounts {
		seedAccount(t, s, account.id, account.runtime, quota)
	}
	return accounts
}

func p014Actor(account p014Account) RuntimeActor {
	return fixtureRuntime(account.id, account.runtime)
}

func p014Reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	dsn := s.Pool.Config().ConnConfig.ConnString()
	s.Close()
	reopened, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen fixture store: %v", err)
	}
	t.Cleanup(reopened.Close)
	return reopened
}

func p014Target(index int) string {
	return []string{"930000001", "930000002", "930000003", "930000004"}[index]
}

// TestP014FourAccountRoutingIsolation reserves one target-only task per
// account, rejects every wrong-account actor before an adapter call, then
// dispatches exactly once through each account's runtime.
func TestP014FourAccountRoutingIsolation(t *testing.T) {
	s := fixtureStore(t)
	accounts := p014SeedFour(t, s)

	type taskCase struct {
		account p014Account
		res     Reservation
	}
	cases := make([]taskCase, 0, len(accounts))
	for i, account := range accounts {
		task := createReadyTask(t, s, []string{account.id}, []string{p014Target(i)}, 1, 1)
		res := reserveFixture(t, s, string(task.Id), account.id)
		if res.AccountID != account.id || res.RuntimeID != account.runtime {
			t.Fatalf("task %d reserved account=%s runtime=%s, want %s/%s", i, res.AccountID, res.RuntimeID, account.id, account.runtime)
		}
		cases = append(cases, taskCase{account: account, res: res})
	}

	// No wrong-account actor may accept another account's reservation.
	for i := range cases {
		wrong := p014Actor(accounts[(i+1)%len(accounts)])
		if _, err := s.AcceptOperation(context.Background(), wrong, cases[i].res); Code(err) != "RESOURCE_NOT_FOUND" {
			t.Fatalf("case %d accepted by account %s: got %v, want RESOURCE_NOT_FOUND", i, wrong.AccountIDs[0], err)
		}
	}

	for i := range cases {
		account := cases[i].account
		res := cases[i].res
		rt := p014Actor(account)
		acceptFixture(t, s, rt, res)
		if _, err := s.IssuePermit(context.Background(), rt, res.OperationID, 10*time.Second); err != nil {
			t.Fatalf("issue permit for %s: %v", account.id, err)
		}

		// A different account must still be rejected after the permit exists.
		wrong := p014Actor(accounts[(i+1)%len(accounts)])
		wrongAdapter := &fakeJoinAdapter{}
		if _, err := s.Dispatch(context.Background(), wrong, res.OperationID, identityOK, wrongAdapter); Code(err) != "RESOURCE_NOT_FOUND" {
			t.Fatalf("wrong account dispatched %s: got %v, want RESOURCE_NOT_FOUND", account.id, err)
		}
		if wrongAdapter.count() != 0 {
			t.Fatalf("wrong account invoked adapter for %s", account.id)
		}

		adapter := &fakeJoinAdapter{outcome: SendOutcome{
			Disposition: "submitted", EffectCertainty: "confirmed_submitted",
			EvidenceRef: "p014_submit_" + account.id,
		}}
		outcome, err := s.Dispatch(context.Background(), rt, res.OperationID, identityOK, adapter)
		if err != nil {
			t.Fatalf("dispatch %s: %v", account.id, err)
		}
		if outcome.Disposition != "submitted" {
			t.Fatalf("dispatch %s disposition=%q", account.id, outcome.Disposition)
		}
		if adapter.count() != 1 {
			t.Fatalf("account %s adapter calls=%d, want 1", account.id, adapter.count())
		}
	}

	// Each account's own membership fact credits exactly one result and one
	// fulfilled slot; four accounts must produce four, not one shared count.
	for _, c := range cases {
		obs := obsTestObservation(c.res, "p014_four_join_"+c.account.id, 1, "membership.observed", "joined", true,
			"p014_four_join_proof_"+c.account.id, time.Now().UTC())
		if err := s.ReceiveObservation(context.Background(), p014Actor(c.account), obs); err != nil {
			t.Fatalf("receive four-account observation for %s: %v", c.account.id, err)
		}
		if err := s.ApplyObservation(context.Background(), "ws_fixture", c.account.runtime, obs.EventID); err != nil {
			t.Fatalf("apply four-account observation for %s: %v", c.account.id, err)
		}
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND valid`, "ws_fixture"); got != 4 {
		t.Fatalf("four-account valid results=%d, want 4", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND state='fulfilled'`, "ws_fixture"); got != 4 {
		t.Fatalf("four-account fulfilled slots=%d, want 4", got)
	}
	for _, account := range accounts {
		if got := scalarInt(t, s, `SELECT count(*) FROM core.membership WHERE workspace_id=$1 AND account_id=$2 AND state='joined'`, "ws_fixture", account.id); got != 1 {
			t.Fatalf("account %s joined membership rows=%d, want 1", account.id, got)
		}
	}
	for _, account := range accounts {
		if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND account_id=$2`, "ws_fixture", account.id); got != 1 {
			t.Fatalf("account %s executions=%d, want 1", account.id, got)
		}
		if got := scalarInt(t, s, `SELECT count(*) FROM bridge.gateway_operation WHERE workspace_id=$1 AND account_id=$2`, "ws_fixture", account.id); got != 1 {
			t.Fatalf("account %s gateway rows=%d, want 1", account.id, got)
		}
		if got := scalarInt(t, s, `SELECT count(*) FROM core.dispatch_permit WHERE workspace_id=$1 AND account_id=$2 AND state='consumed'`, "ws_fixture", account.id); got != 1 {
			t.Fatalf("account %s consumed permits=%d, want 1", account.id, got)
		}
		if got := scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND account_id=$2 AND held`, "ws_fixture", account.id); got != 0 {
			t.Fatalf("account %s retained an action lock", account.id)
		}
		if got := scalarInt(t, s, `SELECT COALESCE(consumed,0) FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2 AND action_family='group.join'`, "ws_fixture", account.id); got != 1 {
			t.Fatalf("account %s consumed usage=%d, want 1", account.id, got)
		}
	}
}

// TestP014CrossAccountObservationDoesNotCredit sends a joined observation
// through account B for account A's operation. The inbox may retain the fact,
// but it must not credit A or create a second result; A's own later fact does.
func TestP014CrossAccountObservationDoesNotCredit(t *testing.T) {
	s := fixtureStore(t)
	accounts := p014SeedFour(t, s)
	accountA, accountB := accounts[0], accounts[1]

	task := createReadyTask(t, s, []string{accountA.id}, []string{"930000011"}, 1, 1)
	res := reserveFixture(t, s, string(task.Id), accountA.id)
	rtA := p014Actor(accountA)
	acceptFixture(t, s, rtA, res)
	if _, err := s.IssuePermit(context.Background(), rtA, res.OperationID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeJoinAdapter{outcome: SendOutcome{
		Disposition: "submitted", EffectCertainty: "confirmed_submitted",
		EvidenceRef: "p014_cross_submit",
	}}
	if _, err := s.Dispatch(context.Background(), rtA, res.OperationID, identityOK, adapter); err != nil {
		t.Fatal(err)
	}

	foreign := obsTestObservation(res, "p014_foreign_join", 1, "membership.observed", "joined", true,
		"p014_foreign_join_proof", time.Now().UTC().Add(time.Second))
	foreign.AccountID = accountB.id
	if err := s.ReceiveObservation(context.Background(), p014Actor(accountB), foreign); err != nil {
		t.Fatalf("receive foreign observation: %v", err)
	}
	if err := s.ApplyObservation(context.Background(), "ws_fixture", accountB.runtime, foreign.EventID); err != nil {
		t.Fatalf("apply foreign observation: %v", err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, "ws_fixture", string(task.Id)); got != 0 {
		t.Fatalf("foreign observation credited %d results, want 0", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='manual_reconciliation' AND detail->>'execution_id'=$3`, "ws_fixture", string(task.Id), res.ExecutionID); got != 1 {
		t.Fatalf("foreign observation reconciliation rows=%d, want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.membership WHERE workspace_id=$1 AND account_id=$2 AND target_id=$3`, "ws_fixture", accountB.id, res.TargetID); got != 0 {
		t.Fatalf("foreign observation wrote account B membership rows=%d, want 0", got)
	}

	own := obsTestObservation(res, "p014_own_join", 2, "membership.observed", "joined", true,
		"p014_own_join_proof", time.Now().UTC().Add(2*time.Second))
	if err := s.ReceiveObservation(context.Background(), rtA, own); err != nil {
		t.Fatal(err)
	}
	if err := s.ReceiveObservation(context.Background(), rtA, own); err != nil {
		t.Fatalf("duplicate own receive: %v", err)
	}
	if err := s.ApplyObservation(context.Background(), "ws_fixture", accountA.runtime, own.EventID); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyObservation(context.Background(), "ws_fixture", accountA.runtime, own.EventID); err != nil {
		t.Fatalf("duplicate own apply: %v", err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, "ws_fixture", string(task.Id)); got != 1 {
		t.Fatalf("own observation results=%d, want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.membership WHERE workspace_id=$1 AND account_id=$2 AND target_id=$3 AND state='joined'`, "ws_fixture", accountA.id, res.TargetID); got != 1 {
		t.Fatalf("own observation membership rows=%d, want 1", got)
	}
}

// TestP014RestartNoResend reopens the store after a permit is issued but before
// the adapter call, recovers the dispatching operation to UNKNOWN, and proves
// that neither recovery nor a retried Dispatch calls the adapter or releases
// the reserved slot.
func TestP014RestartNoResend(t *testing.T) {
	s := fixtureStore(t)
	accounts := p014SeedFour(t, s)
	accountA := accounts[0]
	task := createReadyTask(t, s, []string{accountA.id}, []string{"930000021"}, 1, 1)
	res := reserveFixture(t, s, string(task.Id), accountA.id)
	rt := p014Actor(accountA)
	acceptFixture(t, s, rt, res)
	if _, err := s.IssuePermit(context.Background(), rt, res.OperationID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := gatewayState(t, s, res.OperationID); got != "DISPATCHING" {
		t.Fatalf("gateway state before restart=%s, want DISPATCHING", got)
	}

	restarted := p014Reopen(t, s)
	recovered, err := restarted.RecoverDispatching(context.Background(), rt)
	if err != nil {
		t.Fatalf("recover after restart: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered=%d, want 1", recovered)
	}
	if got := executionState(t, restarted, res.OperationID); got != "UNKNOWN" {
		t.Fatalf("execution after restart=%s, want UNKNOWN", got)
	}
	if got := scalarInt(t, restarted, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='reserved'`, "ws_fixture", string(task.Id), res.TargetID); got != 1 {
		t.Fatalf("restart lost reserved goal slot: %d", got)
	}
	if got := scalarInt(t, restarted, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND target_id=$2 AND state='active'`, "ws_fixture", res.TargetID); got != 1 {
		t.Fatalf("restart lost active target claim: %d", got)
	}

	adapter := &fakeJoinAdapter{}
	outcome, err := restarted.Dispatch(context.Background(), rt, res.OperationID, identityOK, adapter)
	if err != nil {
		t.Fatalf("dispatch after restart: %v", err)
	}
	if outcome.Disposition != "unknown" || outcome.EffectCertainty != "unknown" {
		t.Fatalf("dispatch after restart outcome=%+v, want unknown/unknown", outcome)
	}
	if adapter.count() != 0 {
		t.Fatalf("restart retried the adapter %d times, want 0", adapter.count())
	}
	if got := scalarInt(t, restarted, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", string(task.Id)); got != 0 {
		t.Fatalf("restart created %d results, want 0", got)
	}
	if again, err := restarted.RecoverDispatching(context.Background(), rt); err != nil || again != 0 {
		t.Fatalf("second recovery=%d err=%v, want 0/nil", again, err)
	}
}

// TestP014OfflineAccountNoSend proves an offline account cannot reach the
// adapter, and that coming back online permits exactly one send for the same
// operation without a duplicate.
func TestP014OfflineAccountNoSend(t *testing.T) {
	s := fixtureStore(t)
	accounts := p014SeedFour(t, s)
	account := accounts[1]
	task := createReadyTask(t, s, []string{account.id}, []string{"930000041"}, 1, 1)
	res := reserveFixture(t, s, string(task.Id), account.id)
	rt := p014Actor(account)
	acceptFixture(t, s, rt, res)
	if _, err := s.IssuePermit(context.Background(), rt, res.OperationID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.account SET connection_state='offline' WHERE workspace_id=$1 AND id=$2`, "ws_fixture", account.id); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeJoinAdapter{}
	if _, err := s.Dispatch(context.Background(), rt, res.OperationID, identityOK, adapter); Code(err) != "ACCOUNT_RESTRICTED" {
		t.Fatalf("offline dispatch error=%v, want ACCOUNT_RESTRICTED", err)
	}
	if adapter.count() != 0 {
		t.Fatalf("offline account invoked adapter %d times, want 0", adapter.count())
	}
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.account SET connection_state='online' WHERE workspace_id=$1 AND id=$2`, "ws_fixture", account.id); err != nil {
		t.Fatal(err)
	}
	adapter.outcome = SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "p014_offline_reconnect"}
	if _, err := s.Dispatch(context.Background(), rt, res.OperationID, identityOK, adapter); err != nil {
		t.Fatalf("reconnect dispatch: %v", err)
	}
	if adapter.count() != 1 {
		t.Fatalf("reconnect adapter calls=%d, want 1", adapter.count())
	}
}

// TestP014FourAccountQuotaAndDisconnect checks that each account has its own
// quota and that a dispatched unknown outcome is never retried.
func TestP014FourAccountQuotaAndDisconnect(t *testing.T) {
	s := fixtureStore(t)
	accounts := p014SeedFourWithQuota(t, s, 1)

	type caseState struct {
		account p014Account
		taskID  string
		res     Reservation
		rt      RuntimeActor
		adapter *fakeJoinAdapter
	}
	cases := make([]caseState, 0, len(accounts))
	for i, account := range accounts {
		task := createReadyTask(t, s, []string{account.id}, []string{p014Target(i)}, 1, 1)
		res := reserveFixture(t, s, string(task.Id), account.id)
		rt := p014Actor(account)
		acceptFixture(t, s, rt, res)
		if _, err := s.IssuePermit(context.Background(), rt, res.OperationID, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, caseState{account: account, taskID: string(task.Id), res: res, rt: rt})
	}

	// Four concurrent dispatches, one per account. B loses its connection after
	// the adapter call and is recorded as UNKNOWN; the other three submit.
	var wg sync.WaitGroup
	for i := range cases {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			c := &cases[index]
			c.adapter = &fakeJoinAdapter{}
			if index == 1 {
				c.adapter.err = context.DeadlineExceeded
			} else {
				c.adapter.outcome = SendOutcome{
					Disposition: "submitted", EffectCertainty: "confirmed_submitted",
					EvidenceRef: "p014_quota_submit_" + c.account.id,
				}
			}
			_, err := s.Dispatch(context.Background(), c.rt, c.res.OperationID, identityOK, c.adapter)
			if index == 1 {
				if Code(err) != "EXTERNAL_OUTCOME_UNKNOWN" {
					t.Errorf("account %s dispatch error=%v, want EXTERNAL_OUTCOME_UNKNOWN", c.account.id, err)
				}
			} else if err != nil {
				t.Errorf("account %s dispatch: %v", c.account.id, err)
			}
		}(i)
	}
	wg.Wait()

	for i := range cases {
		c := &cases[i]
		if c.adapter == nil || c.adapter.count() != 1 {
			t.Fatalf("account %s adapter calls=%d, want 1", c.account.id, c.adapter.count())
		}
		if i == 1 {
			if got := executionState(t, s, c.res.OperationID); got != "UNKNOWN" {
				t.Fatalf("account B execution=%s, want UNKNOWN", got)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2 AND action_family='group.join' AND consumed=1`, "ws_fixture", c.account.id); got != 1 {
				t.Fatalf("account B consumed usage=%d, want 1", got)
			}
			before := c.adapter.count()
			out, err := s.Dispatch(context.Background(), c.rt, c.res.OperationID, identityOK, c.adapter)
			if err != nil {
				t.Fatalf("retry unknown dispatch: %v", err)
			}
			if out.Disposition != "unknown" {
				t.Fatalf("retry unknown disposition=%q, want unknown", out.Disposition)
			}
			if c.adapter.count() != before {
				t.Fatalf("unknown retry called adapter: before=%d after=%d", before, c.adapter.count())
			}
		} else {
			if got := executionState(t, s, c.res.OperationID); got != "OBSERVED" {
				t.Fatalf("account %s execution=%s, want OBSERVED", c.account.id, got)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2 AND action_family='group.join' AND consumed=1`, "ws_fixture", c.account.id); got != 1 {
				t.Fatalf("account %s consumed usage=%d, want 1", c.account.id, got)
			}
		}
	}

	// A second account's reservation cannot consume another account's quota.
	extra := createReadyTask(t, s, []string{accounts[0].id}, []string{"930000031"}, 1, 1)
	if _, err := s.Reserve(context.Background(), "ws_fixture", string(extra.Id), accounts[0].id); Code(err) != "NO_CAPACITY" && Code(err) != "LOCAL_BUDGET_EXHAUSTED" {
		t.Fatalf("extra reservation for account A: got %v, want NO_CAPACITY or LOCAL_BUDGET_EXHAUSTED", err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1`, "ws_fixture"); got != 4 {
		t.Fatalf("extra reservation added executions: got %d, want 4", got)
	}
}
