package taskengine

// P0-07 deterministic PostgreSQL tests for Scheduler.Reserve.
//
// Every test runs against a real, migrated PostgreSQL fixture database created
// by fixtureStore. Concurrency tests start real goroutines and assert database
// constraints and row counts; there are no mocked transactions. Fixture rows that
// belong to a later phase (bridge dispatch outcome, retry scheduling state) are
// written with explicit SQL inside a real transaction, never through a fake
// adapter, and never against a production database.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	schedWS     = "ws_fixture"
	schedAcctA  = "acct_sched_a"
	schedAcctB  = "acct_sched_b"
	schedRTAA   = "rt_sched_a"
	schedRTBB   = "rt_sched_b"
	schedGroup1 = "900000001"
	schedGroup2 = "900000002"
	schedGroup3 = "900000003"
)

func schedExec(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture sql: %v\n%s", err, sql)
	}
}

// schedTxSQL applies fixture statements atomically so deferred constraint
// triggers see a coherent state.
func schedTxSQL(t *testing.T, s *Store, statements ...string) {
	t.Helper()
	err := s.tx(context.Background(), func(tx pgx.Tx) error {
		for _, statement := range statements {
			if _, err := tx.Exec(context.Background(), statement); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fixture transaction: %v", err)
	}
}

func schedReserve(t *testing.T, s *Store, taskID, preferred string) (Reservation, string) {
	t.Helper()
	res, err := s.Reserve(context.Background(), schedWS, taskID, preferred)
	if err == nil {
		return res, ""
	}
	return Reservation{}, Code(err)
}

func schedWantCode(t *testing.T, want, got string) {
	t.Helper()
	if got != want {
		t.Fatalf("reserve error code = %q, want %q", got, want)
	}
}

// schedNoReservation asserts the failed attempt left no execution, no reserved
// slot and no account usage behind (the whole reservation transaction rolled
// back).
func schedNoReservation(t *testing.T, s *Store) {
	t.Helper()
	if n := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1`, schedWS); n != 0 {
		t.Fatalf("failed reserve left %d execution rows", n)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND state<>'free'`, schedWS); n != 0 {
		t.Fatalf("failed reserve left %d non-free goal slots", n)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.account_usage WHERE workspace_id=$1`, schedWS); n != 0 {
		t.Fatalf("failed reserve left %d account_usage rows", n)
	}
}

func schedSeedPair(t *testing.T, s *Store, quota int) {
	t.Helper()
	seedAccount(t, s, schedAcctA, schedRTAA, quota)
	seedAccount(t, s, schedAcctB, schedRTBB, quota)
}

func schedBothAccounts() []string { return []string{schedAcctA, schedAcctB} }

// schedMarkPending simulates the bridge having started a send for the reservation
// and leaves the durable possibly-effective execution state. Slots and claims
// must survive; a second attempt must never be reserved for that subtask.
func schedMarkPending(t *testing.T, s *Store, res Reservation, state string) {
	t.Helper()
	disposition := "NULL"
	if state == "UNKNOWN" {
		disposition = "'unknown'"
	}
	schedTxSQL(t, s,
		`UPDATE core.execution SET state='`+state+`', effect_certainty='unknown',
		 disposition=`+disposition+`, started_at=clock_timestamp()
		 WHERE workspace_id='ws_fixture' AND id='`+res.ExecutionID+`'`,
		`UPDATE core.subtask SET state='WAITING_EXTERNAL', version=version+1
		 WHERE workspace_id='ws_fixture' AND id='`+res.SubtaskID+`'`)
	if n := scalarInt(t, s, `SELECT count(*) FROM core.execution
		WHERE workspace_id=$1 AND subtask_id=$2
		  AND (state IN ('DISPATCHING','UNKNOWN') OR effect_certainty IN ('unknown','confirmed_submitted'))`,
		schedWS, res.SubtaskID); n != 1 {
		t.Fatalf("pending fixture: %d possibly effective executions for subtask", n)
	}
}

// schedRecordNoEffect stores a trusted technical no_effect outcome in the
// persistent execution and bridge ledger rows, which is the only precondition
// ScheduleRetry accepts.
func schedRecordNoEffect(t *testing.T, s *Store, taskID string, res Reservation) {
	t.Helper()
	err := s.tx(context.Background(), func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx,
			`UPDATE core.execution SET state='OBSERVED', disposition='failed_no_effect',
			 effect_certainty='no_effect', observed_at=clock_timestamp()
			 WHERE workspace_id=$1 AND id=$2`, schedWS, res.ExecutionID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO bridge.gateway_operation(
			 workspace_id,operation_id,execution_id,task_id,subtask_id,account_id,target_id,
			 runtime_id,owner_epoch,fence_token,scope_revision,request_digest,payload,
			 state,disposition,effect_certainty,evidence_ref)
			 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'{}'::jsonb,
			 'OBSERVED','failed_no_effect','no_effect','fixture_no_effect')`,
			schedWS, res.OperationID, res.ExecutionID, taskID, res.SubtaskID, res.AccountID,
			res.TargetID, res.RuntimeID, res.OwnerEpoch, res.Fence, res.ScopeRevision, res.Digest)
		return err
	})
	if err != nil {
		t.Fatalf("no_effect fixture: %v", err)
	}
}

// schedMakeRetryDue makes a scheduled retry immediately runnable.
func schedMakeRetryDue(t *testing.T, s *Store, subtaskID string) {
	t.Helper()
	schedTxSQL(t, s,
		`UPDATE core.retry SET due_at=now()-interval '1 second'
		 WHERE workspace_id='ws_fixture' AND subtask_id='`+subtaskID+`' AND state='scheduled'`,
		`UPDATE core.subtask SET next_run_at=now()-interval '1 second'
		 WHERE workspace_id='ws_fixture' AND id='`+subtaskID+`'`)
}

// TestConcurrentGoalReservation proves that the last free goal slot is granted to
// exactly one caller and that no second execution/claim/usage row can be created
// by a racing reservation.
func TestConcurrentGoalReservation(t *testing.T) {
	t.Run("single_task_single_slot", func(t *testing.T) {
		store := fixtureStore(t)
		schedSeedPair(t, store, 5)
		task := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 2)

		const callers = 8
		type outcome struct {
			res  Reservation
			code string
		}
		start := make(chan struct{})
		outcomes := make(chan outcome, callers)
		var wg sync.WaitGroup
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				res, err := store.Reserve(context.Background(), schedWS, task.Id, "")
				if err != nil {
					outcomes <- outcome{code: Code(err)}
					return
				}
				outcomes <- outcome{res: res}
			}()
		}
		close(start)
		wg.Wait()
		close(outcomes)

		successes := 0
		var winner Reservation
		for got := range outcomes {
			if got.code != "" {
				switch got.code {
				case schedCodeNoWork, schedCodeNoCapacity, schedCodeLocalBudget:
				default:
					t.Fatalf("concurrent reserve returned unexpected code %q", got.code)
				}
				continue
			}
			successes++
			winner = got.res
		}
		if successes != 1 {
			t.Fatalf("concurrent reserve successes = %d, want exactly 1", successes)
		}
		if winner.TaskID != task.Id || winner.Attempt != 1 || winner.Fence != 1 {
			t.Fatalf("unexpected winner reservation %+v", winner)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, schedWS, task.Id); n != 1 {
			t.Fatalf("executions = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='reserved'`, schedWS, task.Id); n != 1 {
			t.Fatalf("reserved slots = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND state='active'`, schedWS); n != 1 {
			t.Fatalf("active claims = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT COALESCE(sum(reserved),0) FROM core.account_usage WHERE workspace_id=$1`, schedWS); n != 1 {
			t.Fatalf("reserved usage = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.subtask WHERE workspace_id=$1 AND task_id=$2 AND attempt_count=1 AND state='CLAIMED'`, schedWS, task.Id); n != 1 {
			t.Fatalf("claimed subtasks = %d, want 1", n)
		}

		// A later sequential caller is refused by the same last-slot protection.
		if _, code := schedReserve(t, store, task.Id, ""); code != schedCodeNoWork {
			t.Fatalf("post-race reserve code = %q, want %q", code, schedCodeNoWork)
		}
	})

	t.Run("last_slot_only_one_target", func(t *testing.T) {
		store := fixtureStore(t)
		schedSeedPair(t, store, 5)
		task := createReadyTask(t, store, schedBothAccounts(),
			[]string{schedGroup1, schedGroup2, schedGroup3}, 2, 2)

		first := reserveFixture(t, store, task.Id, "")
		second := reserveFixture(t, store, task.Id, "")
		if first.TargetID == second.TargetID {
			t.Fatalf("two reservations claimed the same target %q", first.TargetID)
		}

		_, code := schedReserve(t, store, task.Id, "")
		schedWantCode(t, schedCodeNoCapacity, code)

		// The third target's claim was rolled back with the failed transaction.
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, schedWS, task.Id); n != 2 {
			t.Fatalf("executions = %d, want 2", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND state='active'`, schedWS); n != 2 {
			t.Fatalf("active claims = %d, want 2", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='reserved'`, schedWS, task.Id); n != 2 {
			t.Fatalf("reserved slots = %d, want 2", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.subtask WHERE workspace_id=$1 AND task_id=$2 AND attempt_count=1`, schedWS, task.Id); n != 2 {
			t.Fatalf("advanced subtasks = %d, want 2", n)
		}
	})
}

// TestAccountQuotaAcrossTasks proves the per-account window quota is shared by
// every task of the workspace and is never bypassed by a new task or attempt.
func TestAccountQuotaAcrossTasks(t *testing.T) {
	t.Run("quota_one_blocks_second_task", func(t *testing.T) {
		store := fixtureStore(t)
		seedAccount(t, store, schedAcctA, schedRTAA, 1)
		account := []string{schedAcctA}
		taskA := createReadyTask(t, store, account, []string{schedGroup1}, 1, 2)
		taskB := createReadyTask(t, store, account, []string{schedGroup2}, 1, 2)

		reserveFixture(t, store, taskA.Id, "")
		_, code := schedReserve(t, store, taskB.Id, "")
		schedWantCode(t, schedCodeLocalBudget, code)

		if n := scalarInt(t, store, `SELECT reserved FROM core.account_usage
			WHERE workspace_id=$1 AND account_id=$2 AND action_family='group.join' AND window_start=current_date`,
			schedWS, schedAcctA); n != 1 {
			t.Fatalf("account reserved usage = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.account_usage WHERE workspace_id=$1`, schedWS); n != 1 {
			t.Fatalf("account_usage rows = %d, want 1 shared window row", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1`, schedWS); n != 1 {
			t.Fatalf("executions = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND state='active'`, schedWS); n != 1 {
			t.Fatalf("active claims = %d, want 1 (second target claim rolled back)", n)
		}
	})

	t.Run("quota_two_allows_two_tasks", func(t *testing.T) {
		store := fixtureStore(t)
		seedAccount(t, store, schedAcctA, schedRTAA, 2)
		account := []string{schedAcctA}
		taskA := createReadyTask(t, store, account, []string{schedGroup1}, 1, 2)
		taskB := createReadyTask(t, store, account, []string{schedGroup2}, 1, 2)
		taskC := createReadyTask(t, store, account, []string{schedGroup3}, 1, 2)

		reserveFixture(t, store, taskA.Id, "")
		reserveFixture(t, store, taskB.Id, "")
		_, code := schedReserve(t, store, taskC.Id, "")
		schedWantCode(t, schedCodeLocalBudget, code)

		if n := scalarInt(t, store, `SELECT reserved FROM core.account_usage
			WHERE workspace_id=$1 AND account_id=$2 AND action_family='group.join' AND window_start=current_date`,
			schedWS, schedAcctA); n != 2 {
			t.Fatalf("account reserved usage = %d, want 2", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1`, schedWS); n != 2 {
			t.Fatalf("executions = %d, want 2", n)
		}
	})
}

// TestTargetClaimGlobalAcrossTasks proves the target claim is unique across the
// whole workspace, not per task.
func TestTargetClaimGlobalAcrossTasks(t *testing.T) {
	store := fixtureStore(t)
	schedSeedPair(t, store, 5)
	first := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 2)
	second := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 2)
	third := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 2)

	winner := reserveFixture(t, store, first.Id, "")
	for _, taskID := range []string{second.Id, third.Id} {
		_, code := schedReserve(t, store, taskID, "")
		schedWantCode(t, schedCodeNoCapacity, code)
	}
	if n := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND state='active'`, schedWS); n != 1 {
		t.Fatalf("active claims = %d, want 1", n)
	}
	if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1`, schedWS); n != 1 {
		t.Fatalf("executions = %d, want 1", n)
	}
	if n := scalarInt(t, store, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND state='reserved'`, schedWS); n != 1 {
		t.Fatalf("reserved slots = %d, want 1", n)
	}
	if n := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND subtask_id=$2 AND state='active'`, schedWS, winner.SubtaskID); n != 1 {
		t.Fatalf("winner does not own the active claim")
	}
}

// TestSchedulerConstraintRaces drives parallel reserves over several tasks that
// share candidate targets and accounts, then verifies the database constraints
// held: one claim per target, at most goal slots per task, monotonic attempts and
// quota never exceeded. Intended to run with -race.
func TestSchedulerConstraintRaces(t *testing.T) {
	store := fixtureStore(t)
	schedSeedPair(t, store, 3)
	accounts := schedBothAccounts()
	groups := []string{schedGroup1, schedGroup2, schedGroup3}
	tasks := make([]string, 4)
	for i := range tasks {
		task := createReadyTask(t, store, accounts, groups, 2, 2)
		tasks[i] = task.Id
	}

	const callers = 16
	type outcome struct{ code string }
	start := make(chan struct{})
	outcomes := make(chan outcome, callers)
	var wg sync.WaitGroup
	for i := range callers {
		taskID := tasks[i%len(tasks)]
		wg.Add(1)
		go func(tid string) {
			defer wg.Done()
			<-start
			_, err := store.Reserve(context.Background(), schedWS, tid, "")
			if err != nil {
				outcomes <- outcome{code: Code(err)}
				return
			}
			outcomes <- outcome{}
		}(taskID)
	}
	close(start)
	wg.Wait()
	close(outcomes)

	successes := 0
	for got := range outcomes {
		if got.code == "" {
			successes++
			continue
		}
		switch got.code {
		case schedCodeNoWork, schedCodeNoCapacity, schedCodeLocalBudget:
		default:
			t.Fatalf("race returned unexpected code %q", got.code)
		}
	}
	// Three distinct targets exist, so at most three reservations can be granted;
	// two accounts with quota 3 and four tasks with two slots leave enough room
	// that all three targets are eventually reserved.
	if successes != 3 {
		t.Fatalf("successes = %d, want 3 (one per globally unique target)", successes)
	}

	if n := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND state='active'`, schedWS); n != 3 {
		t.Fatalf("active claims = %d, want 3", n)
	}
	if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1`, schedWS); n != 3 {
		t.Fatalf("executions = %d, want 3", n)
	}
	if n := scalarInt(t, store, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND state='reserved'`, schedWS); n != 3 {
		t.Fatalf("reserved slots = %d, want 3", n)
	}
	if n := scalarInt(t, store, `SELECT COALESCE(sum(reserved),0) FROM core.account_usage WHERE workspace_id=$1`, schedWS); n != 3 {
		t.Fatalf("reserved usage total = %d, want 3", n)
	}
	// Explicit database-constraint probes: no duplicate active claim per target,
	// no duplicate active slot per (task,target), no attempt double-spend.
	if n := scalarInt(t, store, `SELECT count(*) FROM (
		SELECT workspace_id,target_id FROM core.target_claim WHERE state='active'
		GROUP BY workspace_id,target_id HAVING count(*)>1) dup`); n != 0 {
		t.Fatalf("%d targets have more than one active claim", n)
	}
	if n := scalarInt(t, store, `SELECT count(*) FROM (
		SELECT workspace_id,task_id,target_id FROM core.goal_slot WHERE state IN ('reserved','fulfilled')
		GROUP BY workspace_id,task_id,target_id HAVING count(*)>1) dup`); n != 0 {
		t.Fatalf("%d task targets hold more than one active goal slot", n)
	}
	if n := scalarInt(t, store, `SELECT count(*) FROM (
		SELECT workspace_id,subtask_id,attempt_no FROM core.execution
		GROUP BY workspace_id,subtask_id,attempt_no HAVING count(*)>1) dup`); n != 0 {
		t.Fatalf("%d subtasks have duplicated attempt numbers", n)
	}
	for _, taskID := range tasks {
		goal := scalarInt(t, store, `SELECT goal_count FROM core.task WHERE workspace_id=$1 AND id=$2`, schedWS, taskID)
		if n := scalarInt(t, store, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='reserved'`, schedWS, taskID); n > goal {
			t.Fatalf("task %s reserved %d slots beyond goal %d", taskID, n, goal)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, schedWS, taskID); n > goal {
			t.Fatalf("task %s has %d executions beyond goal %d", taskID, n, goal)
		}
	}
	for _, accountID := range accounts {
		if n := scalarInt(t, store, `SELECT COALESCE(reserved,0)+COALESCE(consumed,0) FROM core.account_usage
			WHERE workspace_id=$1 AND account_id=$2 AND action_family='group.join' AND window_start=current_date`,
			schedWS, accountID); n > 3 {
			t.Fatalf("account %s used %d > quota 3", accountID, n)
		}
	}
	if n := scalarInt(t, store, `SELECT count(*) FROM core.subtask WHERE workspace_id=$1 AND attempt_count>1`, schedWS); n != 0 {
		t.Fatalf("%d subtasks advanced more than once", n)
	}
}

// TestPendingRetainsSlotAndClaim proves a possibly effective dispatch is never
// retried and never loses its goal slot or target claim, even if the subtask
// state was reset incorrectly.
func TestPendingRetainsSlotAndClaim(t *testing.T) {
	t.Run("pending_subtask_not_reclaimed", func(t *testing.T) {
		store := fixtureStore(t)
		schedSeedPair(t, store, 5)
		task := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1, schedGroup2}, 2, 2)

		first := reserveFixture(t, store, task.Id, "")
		schedMarkPending(t, store, first, "DISPATCHING")

		second := reserveFixture(t, store, task.Id, "")
		if second.TargetID == first.TargetID {
			t.Fatalf("scheduler reserved the pending target %q again", first.TargetID)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND subtask_id=$2`, schedWS, first.SubtaskID); n != 1 {
			t.Fatalf("pending subtask executions = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='reserved'`, schedWS, task.Id, first.TargetID); n != 1 {
			t.Fatalf("pending target lost its goal slot")
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND target_id=$2 AND state='active'`, schedWS, first.TargetID); n != 1 {
			t.Fatalf("pending target lost its active claim")
		}

		schedMarkPending(t, store, second, "UNKNOWN")
		_, code := schedReserve(t, store, task.Id, "")
		schedWantCode(t, schedCodeNoWork, code)
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, schedWS, task.Id); n != 2 {
			t.Fatalf("executions = %d, want 2", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND state='active'`, schedWS); n != 2 {
			t.Fatalf("active claims = %d, want 2", n)
		}
	})

	t.Run("defensive_reset_cannot_double_reserve", func(t *testing.T) {
		store := fixtureStore(t)
		schedSeedPair(t, store, 5)
		task := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 2)

		first := reserveFixture(t, store, task.Id, "")
		schedMarkPending(t, store, first, "DISPATCHING")
		// A buggy/foreign reset must not let the scheduler dispatch a second time.
		schedExec(t, store, `UPDATE core.subtask SET state='QUEUED', next_run_at=now()-interval '1 second'
			WHERE workspace_id=$1 AND id=$2`, schedWS, first.SubtaskID)

		_, code := schedReserve(t, store, task.Id, "")
		schedWantCode(t, schedCodeNoWork, code)
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND subtask_id=$2`, schedWS, first.SubtaskID); n != 1 {
			t.Fatalf("subtask executions = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT attempt_count FROM core.subtask WHERE workspace_id=$1 AND id=$2`, schedWS, first.SubtaskID); n != 1 {
			t.Fatalf("attempt_count = %d, want 1 (never reset)", n)
		}
	})
}

// TestRetryWaitDueReservation proves a due RETRY_WAIT subtask is reservable
// again, honors same_account, consumes the retry row, and never resets the
// logical attempt budget.
func TestRetryWaitDueReservation(t *testing.T) {
	t.Run("same_account_due", func(t *testing.T) {
		store := fixtureStore(t)
		schedSeedPair(t, store, 5)
		task := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 2)

		first := reserveFixture(t, store, task.Id, "")
		schedRecordNoEffect(t, store, task.Id, first)
		if err := store.ScheduleRetry(context.Background(), schedWS, first.ExecutionID, "same_account",
			time.Now().UTC().Add(10*time.Minute)); err != nil {
			t.Fatalf("schedule retry: %v", err)
		}

		// Not due yet.
		if _, code := schedReserve(t, store, task.Id, ""); code != schedCodeNoWork {
			t.Fatalf("early retry reserve code = %q, want %q", code, schedCodeNoWork)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, schedWS, task.Id); n != 1 {
			t.Fatalf("early retry created an execution (%d rows)", n)
		}

		schedMakeRetryDue(t, store, first.SubtaskID)
		second := reserveFixture(t, store, task.Id, "")
		if second.AccountID != first.AccountID {
			t.Fatalf("same_account retry moved to %q from %q", second.AccountID, first.AccountID)
		}
		if second.Attempt != 2 {
			t.Fatalf("retry attempt = %d, want 2", second.Attempt)
		}
		if second.Fence != 2 {
			t.Fatalf("retry fence = %d, want 2", second.Fence)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND subtask_id=$2 AND state='consumed'`, schedWS, first.SubtaskID); n != 1 {
			t.Fatalf("consumed retry rows = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND subtask_id=$2 AND state='scheduled'`, schedWS, first.SubtaskID); n != 0 {
			t.Fatalf("scheduled retry rows = %d, want 0", n)
		}
		if n := scalarInt(t, store, `SELECT attempt_count FROM core.subtask WHERE workspace_id=$1 AND id=$2`, schedWS, first.SubtaskID); n != 2 {
			t.Fatalf("attempt_count = %d, want 2", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND subtask_id=$2`, schedWS, first.SubtaskID); n != 2 {
			t.Fatalf("subtask executions = %d, want 2", n)
		}
		if n := scalarInt(t, store, `SELECT count(DISTINCT attempt_no) FROM core.execution WHERE workspace_id=$1 AND subtask_id=$2`, schedWS, first.SubtaskID); n != 2 {
			t.Fatalf("distinct attempts = %d, want 2", n)
		}
		if n := scalarInt(t, store, `SELECT reserved FROM core.account_usage
			WHERE workspace_id=$1 AND account_id=$2 AND action_family='group.join' AND window_start=current_date`,
			schedWS, second.AccountID); n != 2 {
			t.Fatalf("attempt usage on account %s = %d, want 2 (one per attempt)", second.AccountID, n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='reserved'`, schedWS, task.Id); n != 1 {
			t.Fatalf("reserved slots = %d, want 1 reused slot", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND state='active'`, schedWS); n != 1 {
			t.Fatalf("active claims = %d, want 1 reused claim", n)
		}

		// Budget exhausted at max_attempts: Reserve must refuse rather than reset.
		schedTxSQL(t, store,
			`UPDATE core.execution SET state='OBSERVED', disposition='failed_no_effect',
			 effect_certainty='no_effect', observed_at=clock_timestamp()
			 WHERE workspace_id='ws_fixture' AND id='`+second.ExecutionID+`'`,
			`UPDATE core.subtask SET state='RETRY_WAIT', current_execution_id=NULL,
			 next_run_at=now()-interval '1 second', version=version+1
			 WHERE workspace_id='ws_fixture' AND id='`+second.SubtaskID+`'`)
		if _, code := schedReserve(t, store, task.Id, ""); code != schedCodeLocalBudget {
			t.Fatalf("exhausted budget reserve code = %q, want %q", code, schedCodeLocalBudget)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND subtask_id=$2`, schedWS, second.SubtaskID); n != 2 {
			t.Fatalf("executions after budget refusal = %d, want 2", n)
		}
		if n := scalarInt(t, store, `SELECT attempt_count FROM core.subtask WHERE workspace_id=$1 AND id=$2`, schedWS, second.SubtaskID); n != 2 {
			t.Fatalf("attempt_count after budget refusal = %d, want 2", n)
		}
	})

	t.Run("released_claim_takeover_reselect", func(t *testing.T) {
		store := fixtureStore(t)
		schedSeedPair(t, store, 5)
		task := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 2)

		first := reserveFixture(t, store, task.Id, "")
		schedRecordNoEffect(t, store, task.Id, first)
		// A proven no_effect attempt may release its goal slot and target claim
		// before the retry is scheduled; the retry must be able to re-acquire both.
		schedTxSQL(t, store,
			`UPDATE core.goal_slot SET state='free', target_id=NULL, reserved_at=NULL, fulfilled_at=NULL
			 WHERE workspace_id='ws_fixture' AND task_id='`+task.Id+`'`,
			`UPDATE core.target_claim SET state='resolved', resolved_at=now()
			 WHERE workspace_id='ws_fixture' AND target_id='`+first.TargetID+`' AND state='active'`)
		if err := store.ScheduleRetry(context.Background(), schedWS, first.ExecutionID, "reselect",
			time.Now().UTC().Add(-time.Second)); err != nil {
			t.Fatalf("schedule retry: %v", err)
		}

		second := reserveFixture(t, store, task.Id, "")
		if second.Attempt != 2 || second.Fence != 2 {
			t.Fatalf("reselect retry attempt/fence = %d/%d, want 2/2", second.Attempt, second.Fence)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.goal_slot
			WHERE workspace_id=$1 AND task_id=$2 AND state='reserved' AND target_id=$3`,
			schedWS, task.Id, first.TargetID); n != 1 {
			t.Fatalf("re-acquired goal slot rows = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.target_claim
			WHERE workspace_id=$1 AND target_id=$2 AND state='active' AND subtask_id=$3`,
			schedWS, first.TargetID, first.SubtaskID); n != 1 {
			t.Fatalf("re-acquired active claim rows = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.retry
			WHERE workspace_id=$1 AND subtask_id=$2 AND state='consumed'`, schedWS, first.SubtaskID); n != 1 {
			t.Fatalf("consumed retry rows = %d, want 1", n)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution
			WHERE workspace_id=$1 AND subtask_id=$2`, schedWS, first.SubtaskID); n != 2 {
			t.Fatalf("subtask executions = %d, want 2", n)
		}
	})

	t.Run("task_total_budget", func(t *testing.T) {
		store := fixtureStore(t)
		schedSeedPair(t, store, 5)
		task := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 5)
		schedExec(t, store, `UPDATE core.task SET max_total_attempts=1 WHERE workspace_id=$1 AND id=$2`, schedWS, task.Id)

		first := reserveFixture(t, store, task.Id, "")
		schedTxSQL(t, store,
			`UPDATE core.execution SET state='OBSERVED', disposition='failed_no_effect',
			 effect_certainty='no_effect', observed_at=clock_timestamp()
			 WHERE workspace_id='ws_fixture' AND id='`+first.ExecutionID+`'`,
			`UPDATE core.subtask SET state='RETRY_WAIT', current_execution_id=NULL,
			 next_run_at=now()-interval '1 second', version=version+1
			 WHERE workspace_id='ws_fixture' AND id='`+first.SubtaskID+`'`)

		if _, code := schedReserve(t, store, task.Id, ""); code != schedCodeLocalBudget {
			t.Fatalf("task total budget reserve code = %q, want %q", code, schedCodeLocalBudget)
		}
		if n := scalarInt(t, store, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, schedWS, task.Id); n != 1 {
			t.Fatalf("executions = %d, want 1", n)
		}
	})
}

// TestReserveRechecks covers every runnable/readiness gate the scheduler must
// re-read inside the transaction. Every case ends with zero execution, slot and
// usage rows because the reservation transaction rolls back.
func TestReserveRechecks(t *testing.T) {
	const group = schedGroup1
	type testCase struct {
		name   string
		mutate func(t *testing.T, s *Store, taskID, accountID string)
		code   string
	}
	cases := []testCase{
		{"task_not_running", func(t *testing.T, s *Store, taskID, _ string) {
			schedExec(t, s, `UPDATE core.task SET state='PAUSED' WHERE workspace_id=$1 AND id=$2`, schedWS, taskID)
		}, schedCodeNoWork},
		{"task_dry_run", func(t *testing.T, s *Store, taskID, _ string) {
			schedExec(t, s, `UPDATE core.task SET mode='dry_run' WHERE workspace_id=$1 AND id=$2`, schedWS, taskID)
		}, schedCodeNoWork},
		{"task_read_only", func(t *testing.T, s *Store, taskID, _ string) {
			schedExec(t, s, `UPDATE core.task SET mode='read_only' WHERE workspace_id=$1 AND id=$2`, schedWS, taskID)
		}, schedCodeNoWork},
		{"deadline_passed", func(t *testing.T, s *Store, taskID, _ string) {
			schedExec(t, s, `UPDATE core.task SET deadline_at=now()-interval '1 minute' WHERE workspace_id=$1 AND id=$2`, schedWS, taskID)
		}, schedCodeNoWork},
		{"baseline_incomplete", func(t *testing.T, s *Store, taskID, _ string) {
			schedExec(t, s, `UPDATE core.task SET baseline_complete=false WHERE workspace_id=$1 AND id=$2`, schedWS, taskID)
		}, schedCodeNoWork},
		{"import_incomplete", func(t *testing.T, s *Store, taskID, _ string) {
			schedExec(t, s, `UPDATE core.task SET import_done=false WHERE workspace_id=$1 AND id=$2`, schedWS, taskID)
		}, schedCodeNoWork},
		{"approval_revoked", func(t *testing.T, s *Store, _ string, _ string) {
			schedExec(t, s, `UPDATE core.approval SET revoked=true WHERE workspace_id=$1`, schedWS)
		}, schedCodeNoWork},
		{"approval_expired", func(t *testing.T, s *Store, _ string, _ string) {
			schedExec(t, s, `UPDATE core.approval SET expires_at=now()-interval '1 second' WHERE workspace_id=$1`, schedWS)
		}, schedCodeNoWork},
		{"approval_account_scope", func(t *testing.T, s *Store, _ string, _ string) {
			schedExec(t, s, `UPDATE core.approval SET account_ids=ARRAY['acct_other']::text[] WHERE workspace_id=$1`, schedWS)
		}, schedCodeNoCapacity},
		{"approval_target_scope", func(t *testing.T, s *Store, _ string, _ string) {
			schedExec(t, s, `UPDATE core.approval SET target_ids=ARRAY['999999999']::text[] WHERE workspace_id=$1`, schedWS)
		}, schedCodeNoCapacity},
		{"identity_stale", func(t *testing.T, s *Store, _ string, accountID string) {
			schedExec(t, s, `UPDATE core.account SET identity_status='stale' WHERE workspace_id=$1 AND id=$2`, schedWS, accountID)
		}, schedCodeNoCapacity},
		{"account_paused", func(t *testing.T, s *Store, _ string, accountID string) {
			schedExec(t, s, `UPDATE core.account SET control_state='paused' WHERE workspace_id=$1 AND id=$2`, schedWS, accountID)
		}, schedCodeNoCapacity},
		{"account_offline", func(t *testing.T, s *Store, _ string, accountID string) {
			schedExec(t, s, `UPDATE core.account SET connection_state='offline' WHERE workspace_id=$1 AND id=$2`, schedWS, accountID)
		}, schedCodeNoCapacity},
		{"risk_restricted", func(t *testing.T, s *Store, _ string, accountID string) {
			schedExec(t, s, `UPDATE core.account SET risk_state='restricted' WHERE workspace_id=$1 AND id=$2`, schedWS, accountID)
		}, schedCodeNoCapacity},
		{"capability_unproven", func(t *testing.T, s *Store, _ string, accountID string) {
			schedExec(t, s, `UPDATE core.account SET request_join_verified=false WHERE workspace_id=$1 AND id=$2`, schedWS, accountID)
		}, schedCodeCapabilityUnavail},
		{"cooldown_active", func(t *testing.T, s *Store, _ string, accountID string) {
			schedExec(t, s, `UPDATE core.account SET next_action_at=now()+interval '10 minutes' WHERE workspace_id=$1 AND id=$2`, schedWS, accountID)
		}, schedCodeNoCapacity},
		{"credential_inactive", func(t *testing.T, s *Store, _ string, accountID string) {
			schedExec(t, s, `UPDATE core.runtime_credentials SET active=false WHERE workspace_id=$1 AND account_id=$2`, schedWS, accountID)
		}, schedCodeNoCapacity},
		{"credential_owner_epoch_mismatch", func(t *testing.T, s *Store, _ string, accountID string) {
			schedExec(t, s, `UPDATE core.runtime_credentials SET owner_epoch=2 WHERE workspace_id=$1 AND account_id=$2`, schedWS, accountID)
		}, schedCodeNoCapacity},
		{"baseline_proof_missing", func(t *testing.T, s *Store, taskID, _ string) {
			schedExec(t, s, `DELETE FROM core.baseline_account WHERE workspace_id=$1 AND task_id=$2`, schedWS, taskID)
		}, schedCodeNoCapacity},
		{"target_disabled", func(t *testing.T, s *Store, _ string, _ string) {
			schedExec(t, s, `UPDATE core.target SET disabled=true WHERE workspace_id=$1 AND external_id=$2`, schedWS, group)
		}, schedCodeNoCapacity},
		{"target_excluded", func(t *testing.T, s *Store, taskID, _ string) {
			schedExec(t, s, `UPDATE core.task_target SET eligibility='excluded' WHERE workspace_id=$1 AND task_id=$2`, schedWS, taskID)
		}, schedCodeNoCapacity},
		{"already_member_in_baseline", func(t *testing.T, s *Store, taskID, accountID string) {
			schedExec(t, s, `INSERT INTO core.baseline(workspace_id,task_id,account_id,target_external_id,evidence_ref,observed_at)
				VALUES($1,$2,$3,$4,'fixture_baseline_evidence',clock_timestamp())`, schedWS, taskID, accountID, group)
		}, schedCodeNoCapacity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := fixtureStore(t)
			seedAccount(t, store, schedAcctA, schedRTAA, 5)
			task := createReadyTask(t, store, []string{schedAcctA}, []string{group}, 1, 2)
			tc.mutate(t, store, task.Id, schedAcctA)

			_, code := schedReserve(t, store, task.Id, "")
			schedWantCode(t, tc.code, code)
			schedNoReservation(t, store)
		})
	}
}

// TestReservePreferredAccount proves the preferred account is a scope-limited
// hint: an in-scope preference wins, an out-of-scope preference never becomes an
// authorization.
func TestReservePreferredAccount(t *testing.T) {
	t.Run("in_scope_preference", func(t *testing.T) {
		store := fixtureStore(t)
		schedSeedPair(t, store, 5)
		task := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 2)
		res := reserveFixture(t, store, task.Id, schedAcctB)
		if res.AccountID != schedAcctB {
			t.Fatalf("preferred account = %q, want %q", res.AccountID, schedAcctB)
		}
	})

	t.Run("out_of_scope_preference_ignored", func(t *testing.T) {
		store := fixtureStore(t)
		schedSeedPair(t, store, 5)
		task := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 2)
		res := reserveFixture(t, store, task.Id, "acct_not_in_scope")
		if res.AccountID == "acct_not_in_scope" {
			t.Fatalf("out-of-scope account was reserved")
		}
		if res.AccountID != schedAcctA && res.AccountID != schedAcctB {
			t.Fatalf("reserved account %q is outside the task scope", res.AccountID)
		}
	})
}

// TestExecutionPayloadImmutable proves the reservation writes one canonical,
// digest-verifiable request envelope, exposes it through the persistent outbox,
// and that the database rejects rewriting the execution request or digest.
func TestExecutionPayloadImmutable(t *testing.T) {
	store := fixtureStore(t)
	schedSeedPair(t, store, 5)
	task := createReadyTask(t, store, schedBothAccounts(), []string{schedGroup1}, 1, 2)
	res := reserveFixture(t, store, task.Id, "")
	ctx := context.Background()

	var approvalID string
	if err := store.Pool.QueryRow(ctx,
		`SELECT approval_id FROM core.task WHERE workspace_id=$1 AND id=$2`,
		schedWS, task.Id).Scan(&approvalID); err != nil {
		t.Fatalf("read approval_id: %v", err)
	}
	if approvalID == "" {
		t.Fatal("live task has no approval_id")
	}

	var raw []byte
	if err := store.Pool.QueryRow(ctx,
		`SELECT request_payload FROM core.execution WHERE workspace_id=$1 AND id=$2`,
		schedWS, res.ExecutionID).Scan(&raw); err != nil {
		t.Fatalf("read request_payload: %v", err)
	}
	var payload ExecutionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode request_payload: %v", err)
	}
	want := ExecutionPayload{
		WorkspaceID: res.WorkspaceID, TaskID: res.TaskID, SubtaskID: res.SubtaskID,
		ExecutionID: res.ExecutionID, OperationID: res.OperationID, ApprovalID: approvalID,
		Action: "request_join", AccountID: res.AccountID, RuntimeID: res.RuntimeID,
		TargetID: res.TargetID, GroupID: res.GroupID,
		JoinReason: "fixture reason", VerificationMessage: "fixture answer",
		OwnerEpoch: res.OwnerEpoch, Fence: res.Fence, ScopeRevision: res.ScopeRevision,
		Attempt: res.Attempt, DeadlineAt: res.Deadline.UTC().Format(time.RFC3339Nano),
	}
	if payload != want {
		t.Fatalf("stored payload = %+v, want %+v", payload, want)
	}
	recomputed, err := digest(payload)
	if err != nil {
		t.Fatalf("digest payload: %v", err)
	}
	if recomputed != res.Digest {
		t.Fatalf("recomputed digest = %q, reservation digest = %q", recomputed, res.Digest)
	}
	var storedDigest string
	if err := store.Pool.QueryRow(ctx,
		`SELECT request_digest FROM core.execution WHERE workspace_id=$1 AND id=$2`,
		schedWS, res.ExecutionID).Scan(&storedDigest); err != nil {
		t.Fatalf("read request_digest: %v", err)
	}
	if storedDigest != recomputed {
		t.Fatalf("stored digest = %q, want %q", storedDigest, recomputed)
	}

	// Exactly one persistent dispatch event carries the same canonical envelope.
	if n := scalarInt(t, store, `SELECT count(*) FROM core.outbox
		WHERE workspace_id=$1 AND task_id=$2 AND event_type='dispatch.requested'`, schedWS, task.Id); n != 1 {
		t.Fatalf("dispatch.requested outbox rows = %d, want 1", n)
	}
	var outRaw []byte
	if err := store.Pool.QueryRow(ctx,
		`SELECT payload FROM core.outbox WHERE workspace_id=$1 AND task_id=$2 AND event_type='dispatch.requested'`,
		schedWS, task.Id).Scan(&outRaw); err != nil {
		t.Fatalf("read outbox payload: %v", err)
	}
	var outPayload ExecutionPayload
	if err := json.Unmarshal(outRaw, &outPayload); err != nil {
		t.Fatalf("decode outbox payload: %v", err)
	}
	if outPayload != payload {
		t.Fatalf("outbox payload differs from execution payload")
	}

	// The database, not the application, rejects rewriting the envelope.
	if _, err := store.Pool.Exec(ctx,
		`UPDATE core.execution SET request_payload='{"tampered":true}'::jsonb WHERE workspace_id=$1 AND id=$2`,
		schedWS, res.ExecutionID); err == nil {
		t.Fatal("immutable execution payload was updated")
	} else if code := Code(err); code != "STATE_CONSTRAINT" {
		t.Fatalf("tampered payload error code = %q, want STATE_CONSTRAINT", code)
	}
	if _, err := store.Pool.Exec(ctx,
		`UPDATE core.execution SET request_digest=repeat('0',64) WHERE workspace_id=$1 AND id=$2`,
		schedWS, res.ExecutionID); err == nil {
		t.Fatal("immutable execution digest was updated")
	} else if code := Code(err); code != "STATE_CONSTRAINT" {
		t.Fatalf("tampered digest error code = %q, want STATE_CONSTRAINT", code)
	}
	var finalDigest string
	if err := store.Pool.QueryRow(ctx,
		`SELECT request_digest FROM core.execution WHERE workspace_id=$1 AND id=$2`,
		schedWS, res.ExecutionID).Scan(&finalDigest); err != nil {
		t.Fatalf("re-read request_digest: %v", err)
	}
	if finalDigest != res.Digest {
		t.Fatalf("execution digest changed to %q", finalDigest)
	}
}
