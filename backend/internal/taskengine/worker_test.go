package taskengine

// Worker fixtures: lifecycle (bounded poll, graceful stop, cancellation), lease
// ownership, idempotent ticks, concurrent workers, crash recovery and the
// default fixture transport gate.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/oapi-codegen/nullable"
)

// workerCountAdapter is a deterministic test double. It performs no network
// call and exists only to count how many times the worker reached the adapter
// boundary; the external-transport gate below is enabled explicitly for tests.
type workerCountAdapter struct {
	mu      sync.Mutex
	calls   int
	outcome SendOutcome
	err     error
}

func (a *workerCountAdapter) RequestJoin(context.Context, Reservation) (SendOutcome, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if a.err != nil {
		return SendOutcome{}, a.err
	}
	if a.outcome.Disposition == "" {
		return SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "ev_worker_send"}, nil
	}
	return a.outcome, nil
}

func (a *workerCountAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func workerFixture(t *testing.T, s *Store, adapter Adapter, steps WorkerSteps) *Worker {
	t.Helper()
	cfg := WorkerConfig{
		WorkerID:     "worker_" + newID("test"),
		PollInterval: 150 * time.Millisecond,
		BatchSize:    10,
		Steps:        steps,
	}
	if adapter != nil {
		// Test-only: the injected double is deterministic and offline. The
		// external transport is never selected by the development server.
		cfg.Transport = WorkerExternalTransport
		cfg.AllowExternalWrites = true
		cfg.Adapter = adapter
	}
	worker, err := NewWorker(s, cfg)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	return worker
}

func workerWaitRunning(t *testing.T, worker *Worker) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		worker.mu.Lock()
		running := worker.running
		worker.mu.Unlock()
		if running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("worker did not start")
}

func workerDraftTask(t *testing.T, s *Store, accounts, groups []string) string {
	t.Helper()
	fixed := make([]contract.FixedCandidate, 0, len(groups))
	for _, id := range groups {
		fixed = append(fixed, contract.FixedCandidate{GroupId: id, SourceRef: "fixture_import"})
	}
	mode := contract.ExecutionMode("read_only")
	req := contract.CreateTaskRequest{
		Type:                "group.acquire",
		Title:               "worker draft",
		JoinReason:          "fixture reason",
		VerificationMessage: "fixture answer",
		Goal: contract.Goal{
			Metric: "new_unique_groups_for_account_pool", Count: 1,
			StrictTargetCap: true, RequireFinalMembershipCheck: true,
		},
		AllowedAccountIds: accounts,
		CandidateSource:   contract.CandidateSource{Kind: "fixed", Groups: fixed},
		ExecutionMode:     &mode,
		DeadlineAt:        time.Now().UTC().Add(time.Hour),
		Limits: contract.TaskLimits{
			MaxCandidates: len(groups), MaxAttemptsPerAction: 1,
			MaxTotalExternalAttempts: len(groups), MaxModelAttempts: 0,
		},
		ApprovalRef: nullable.NewNullNullable[string](),
		Submit:      false,
	}
	task, err := s.CreateTask(context.Background(), fixtureActor(), newID("create"), req)
	if err != nil {
		t.Fatalf("create draft task: %v", err)
	}
	return task.Id
}

func TestWorkerNoCandidateTickIsEmpty(t *testing.T) {
	s := fixtureStore(t)
	worker := workerFixture(t, s, nil, DefaultWorkerSteps())
	tick, err := worker.Tick(context.Background())
	if err != nil {
		t.Fatalf("empty tick: %v", err)
	}
	zero := WorkerTick{StartedAt: tick.StartedAt, Duration: tick.Duration}
	if tick != zero {
		t.Fatalf("empty database produced work: %s", tick.String())
	}
}

func TestWorkerExpandsDraftTemplate(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_worker_expand", "rt_worker_expand", 2)
	taskID := workerDraftTask(t, s, []string{"acc_worker_expand"}, []string{"930001", "930002", "930003"})
	worker := workerFixture(t, s, nil, WorkerSteps{Expand: true})

	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("expand tick: %v", err)
	}
	if tick.ExpandedTasks != 1 || tick.ExpandedPages != 1 {
		t.Fatalf("expand tick=%+v want 1 task, 1 page", tick)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_target WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 3 {
		t.Fatalf("imported targets=%d want 3", got)
	}
	var importDone bool
	if err := s.Pool.QueryRow(ctx, `SELECT import_done FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`, taskID).Scan(&importDone); err != nil {
		t.Fatal(err)
	}
	if !importDone {
		t.Fatal("expansion did not finish the fixed import")
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("expansion produced executions=%d want 0", got)
	}

	repeat, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("repeat expand tick: %v", err)
	}
	if repeat.ExpandedTasks != 0 || repeat.ExpandedPages != 0 {
		t.Fatalf("repeated expansion imported again: %+v", repeat)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_target WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 3 {
		t.Fatalf("repeated expansion changed targets=%d want 3", got)
	}
}

func TestWorkerDispatchIsIdempotent(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_worker_tick", "rt_worker_tick", 3)
	task := createReadyTask(t, s, []string{"acc_worker_tick"}, []string{"930011"}, 1, 2)
	taskID := string(task.Id)
	adapter := &workerCountAdapter{}
	worker := workerFixture(t, s, adapter, WorkerSteps{Dispatch: true, Outbox: true, Inbox: true})

	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("dispatch tick: %v", err)
	}
	if tick.Reservations != 1 || tick.Dispatched != 1 {
		t.Fatalf("dispatch tick=%+v want 1 reservation, 1 dispatch", tick)
	}
	if got := adapter.count(); got != 1 {
		t.Fatalf("adapter calls=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 1 {
		t.Fatalf("executions=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("retries=%d want 0", got)
	}
	events := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID)

	repeat, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("repeat dispatch tick: %v", err)
	}
	if repeat.Dispatched != 0 || adapter.count() != 1 {
		t.Fatalf("repeated tick re-dispatched: tick=%+v calls=%d", repeat, adapter.count())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 1 {
		t.Fatalf("repeated tick executions=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != events {
		t.Fatalf("repeated tick appended events %d -> %d", events, got)
	}
}

func TestWorkerConcurrentWorkersMakeOneAttempt(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_worker_race", "rt_worker_race", 3)
	task := createReadyTask(t, s, []string{"acc_worker_race"}, []string{"930021"}, 1, 2)
	taskID := string(task.Id)
	adapter := &workerCountAdapter{}
	workers := []*Worker{
		workerFixture(t, s, adapter, WorkerSteps{Dispatch: true}),
		workerFixture(t, s, adapter, WorkerSteps{Dispatch: true}),
	}
	errs := make([]error, len(workers))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index, worker := range workers {
		wg.Add(1)
		go func(index int, worker *Worker) {
			defer wg.Done()
			<-start
			_, errs[index] = worker.Tick(ctx)
		}(index, worker)
	}
	close(start)
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("worker %d tick: %v", index, err)
		}
	}
	if got := adapter.count(); got != 1 {
		t.Fatalf("adapter calls=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 1 {
		t.Fatalf("executions=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("retries=%d want 0", got)
	}
}

// workerRecoveryFault installs a fixture-database-only fault that makes the
// recovery write for the named executions fail, so a transient error on the
// first recovery pass is reproduced through the real Store path.
func workerRecoveryFault(t *testing.T, s *Store, executionIDs ...string) {
	t.Helper()
	if len(executionIDs) == 0 {
		t.Fatal("recovery fault needs at least one execution id")
	}
	quoted := make([]string, 0, len(executionIDs))
	for _, id := range executionIDs {
		if strings.IndexFunc(id, func(r rune) bool { return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789_", r) }) >= 0 {
			t.Fatalf("unexpected execution id %q", id)
		}
		quoted = append(quoted, "'"+id+"'")
	}
	ctx := context.Background()
	ddl := fmt.Sprintf(`CREATE FUNCTION fixture_fail_execution_update() RETURNS trigger LANGUAGE plpgsql AS $fn$ BEGIN IF NEW.id IN (%s) THEN RAISE EXCEPTION 'fixture recovery fault'; END IF; RETURN NEW; END $fn$`, strings.Join(quoted, ","))
	if _, err := s.Pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("create recovery fault: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `CREATE TRIGGER fixture_fail_execution_update BEFORE UPDATE ON core.execution FOR EACH ROW EXECUTE FUNCTION fixture_fail_execution_update()`); err != nil {
		t.Fatalf("create recovery fault trigger: %v", err)
	}
}

func workerRecoveryFaultClear(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `DROP TRIGGER IF EXISTS fixture_fail_execution_update ON core.execution`); err != nil {
		t.Fatalf("drop recovery fault trigger: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `DROP FUNCTION IF EXISTS fixture_fail_execution_update()`); err != nil {
		t.Fatalf("drop recovery fault function: %v", err)
	}
}

// workerRecoveryPendingOperation drives one runtime group into the crash window
// recovery exists for: reserved, accepted and permitted, never sent.
func workerRecoveryPendingOperation(t *testing.T, s *Store, account, runtime, group string) Reservation {
	t.Helper()
	ctx := context.Background()
	seedAccount(t, s, account, runtime, 2)
	task := createReadyTask(t, s, []string{account}, []string{group}, 1, 2)
	r := reserveFixture(t, s, string(task.Id), account)
	actor := fixtureRuntime(account, runtime)
	if _, err := s.AcceptOperation(ctx, actor, r); err != nil {
		t.Fatalf("accept operation %s: %v", runtime, err)
	}
	if _, err := s.IssuePermit(ctx, actor, r.OperationID, 10*time.Second); err != nil {
		t.Fatalf("issue permit %s: %v", runtime, err)
	}
	return r
}

// F2: a transient failure during the first recovery pass must leave the pass
// unclaimed, so the same Worker instance still recovers on a later tick.
func TestWorkerRecoveryRetriesAfterTransientFailure(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	r := workerRecoveryPendingOperation(t, s, "acc_f2_retry", "rt_f2_a", "930030")
	workerRecoveryFault(t, s, r.ExecutionID)
	worker := workerFixture(t, s, nil, WorkerSteps{Recover: true})

	if _, err := worker.Tick(ctx); err == nil {
		t.Fatal("recovery tick with the fault injected succeeded")
	}
	workerRecoveryFaultClear(t, s)

	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("retry recovery tick: %v", err)
	}
	if tick.RecoveredDispatching != 1 {
		t.Fatalf("retry recovered dispatching=%d want 1", tick.RecoveredDispatching)
	}
	if got := gatewayState(t, s, r.OperationID); got != "UNKNOWN" {
		t.Fatalf("retry recovery gateway state=%s want UNKNOWN", got)
	}
	if got := executionState(t, s, r.OperationID); got != "UNKNOWN" {
		t.Fatalf("retry recovery execution state=%s want UNKNOWN", got)
	}
	if got := permitState(t, s, r.OperationID); got != "issued" {
		t.Fatalf("recovery retry consumed the permit: %s want issued", got)
	}
}

// F3: runtime enumeration must page past BatchSize so groups beyond the first
// page are never starved.
func TestWorkerRecoveryProcessesAllRuntimePages(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	accounts := []string{"acc_page_a", "acc_page_b", "acc_page_c"}
	runtimes := []string{"rt_page_a", "rt_page_b", "rt_page_c"}
	groups := []string{"930041", "930042", "930043"}
	operations := make([]string, 0, len(accounts))
	for i := range accounts {
		r := workerRecoveryPendingOperation(t, s, accounts[i], runtimes[i], groups[i])
		operations = append(operations, r.OperationID)
	}
	worker := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	worker.cfg.BatchSize = 2

	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("paged recovery tick: %v", err)
	}
	if tick.RecoveredDispatching != len(operations) {
		t.Fatalf("paged recovery recovered=%d want %d", tick.RecoveredDispatching, len(operations))
	}
	for _, operationID := range operations {
		if got := gatewayState(t, s, operationID); got != "UNKNOWN" {
			t.Fatalf("paged recovery %s state=%s want UNKNOWN", operationID, got)
		}
	}
}

// F3: a first page with no work must not stop the pass; the idle groups are
// enumerated first and the work sits on the last page.
func TestWorkerRecoveryReachesIdleFirstPage(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_idle_a", "rt_idle_a", 2)
	seedAccount(t, s, "acc_idle_b", "rt_idle_b", 2)
	r := workerRecoveryPendingOperation(t, s, "acc_work_c", "rt_work_c", "930053")
	worker := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	worker.cfg.BatchSize = 1

	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("idle first page tick: %v", err)
	}
	if tick.RecoveredDispatching != 1 {
		t.Fatalf("reached tail work=%d want 1", tick.RecoveredDispatching)
	}
	if got := gatewayState(t, s, r.OperationID); got != "UNKNOWN" {
		t.Fatalf("tail recovery state=%s want UNKNOWN", got)
	}
}

// F3: one failing actor must not swallow the rest of the pass, and a restart
// must finish what the interrupted pass left behind without double counting.
func TestWorkerRecoveryRetriesFailedActorAfterRestart(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	failed := workerRecoveryPendingOperation(t, s, "acc_part_a", "rt_part_a", "930061")
	settled := workerRecoveryPendingOperation(t, s, "acc_part_b", "rt_part_b", "930062")
	workerRecoveryFault(t, s, failed.ExecutionID)
	worker := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	worker.cfg.BatchSize = 1

	tick, err := worker.Tick(ctx)
	if err == nil {
		t.Fatal("recovery tick with a faulted actor succeeded")
	}
	if tick.RecoveredDispatching != 1 {
		t.Fatalf("partial recovery recovered=%d want 1", tick.RecoveredDispatching)
	}
	if got := gatewayState(t, s, settled.OperationID); got != "UNKNOWN" {
		t.Fatalf("healthy actor state=%s want UNKNOWN", got)
	}
	if got := gatewayState(t, s, failed.OperationID); got != "DISPATCHING" {
		t.Fatalf("faulted actor state=%s want DISPATCHING", got)
	}
	workerRecoveryFaultClear(t, s)

	restarted := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	restarted.cfg.BatchSize = 1
	tick, err = restarted.Tick(ctx)
	if err != nil {
		t.Fatalf("restarted recovery tick: %v", err)
	}
	if tick.RecoveredDispatching != 1 {
		t.Fatalf("restarted recovery recovered=%d want 1", tick.RecoveredDispatching)
	}
	if got := gatewayState(t, s, failed.OperationID); got != "UNKNOWN" {
		t.Fatalf("faulted actor after restart=%s want UNKNOWN", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM bridge.gateway_operation WHERE workspace_id='ws_fixture' AND state='UNKNOWN'`); got != 2 {
		t.Fatalf("settled actors re-counted: UNKNOWN rows=%d want 2", got)
	}
	if got := permitState(t, s, failed.OperationID); got != "issued" {
		t.Fatalf("recovery retry consumed the permit: %s want issued", got)
	}
}

// F3: the per-tick bridge outbox pump uses the same enumeration, so a group
// past the first page must still get its receipt delivered. Both receipts are
// persisted before the pump runs, so only the enumeration can explain the result.
func TestWorkerBridgeOutboxPagesPastFirstRuntimeGroup(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	adapter := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fixture_pump_evidence"}}
	accounts := []string{"acc_pump_a", "acc_pump_b"}
	runtimes := []string{"rt_pump_a", "rt_pump_b"}
	groups := []string{"930071", "930072"}
	operations := make([]string, 0, len(accounts))
	for i, account := range accounts {
		seedAccount(t, s, account, runtimes[i], 2)
		task := createReadyTask(t, s, []string{account}, []string{groups[i]}, 1, 2)
		r := reserveFixture(t, s, string(task.Id), account)
		actor := fixtureRuntime(account, runtimes[i])
		accepted := acceptFixture(t, s, actor, r)
		if _, err := s.IssuePermit(ctx, actor, accepted.OperationID, 10*time.Second); err != nil {
			t.Fatalf("issue permit %s: %v", runtimes[i], err)
		}
		if _, err := s.Dispatch(ctx, actor, accepted.OperationID, identityOK, adapter); err != nil {
			t.Fatalf("dispatch %s: %v", runtimes[i], err)
		}
		operations = append(operations, accepted.OperationID)
	}
	if got := adapter.count(); got != len(accounts) {
		t.Fatalf("adapter calls=%d want %d", got, len(accounts))
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM bridge.outbox WHERE workspace_id='ws_fixture' AND state='pending'`); got != len(accounts) {
		t.Fatalf("pending bridge rows before the pump=%d want %d", got, len(accounts))
	}
	worker := workerFixture(t, s, nil, WorkerSteps{Outbox: true})
	worker.cfg.BatchSize = 1

	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("paged bridge tick: %v", err)
	}
	if tick.BridgeAccepted != len(accounts) {
		t.Fatalf("bridge receipts pumped=%d want %d", tick.BridgeAccepted, len(accounts))
	}
	for _, operationID := range operations {
		if got := scalarInt(t, s, `SELECT count(*) FROM bridge.outbox WHERE workspace_id='ws_fixture' AND operation_id=$1 AND state='delivered'`, operationID); got != 1 {
			t.Fatalf("operation %s receipt not delivered: %d", operationID, got)
		}
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.inbox WHERE workspace_id='ws_fixture'`); got != len(accounts) {
		t.Fatalf("inbox rows=%d want %d", got, len(accounts))
	}
}

func TestWorkerRecoveryNeverSendsAgain(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_worker_recover", "rt_worker_recover", 2)
	task := createReadyTask(t, s, []string{"acc_worker_recover"}, []string{"930031"}, 1, 2)
	taskID := string(task.Id)

	// Crash window: reserved, accepted and permitted, then the process died
	// before the adapter was reached.
	r := reserveFixture(t, s, taskID, "acc_worker_recover")
	rt := fixtureRuntime("acc_worker_recover", "rt_worker_recover")
	if _, err := s.AcceptOperation(ctx, rt, r); err != nil {
		t.Fatalf("accept operation: %v", err)
	}
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 10*time.Second); err != nil {
		t.Fatalf("issue permit: %v", err)
	}
	adapter := &workerCountAdapter{}
	worker := workerFixture(t, s, adapter, WorkerSteps{Recover: true, Dispatch: true, Outbox: true, Inbox: true})

	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	if tick.RecoveredDispatching != 1 {
		t.Fatalf("recovered dispatching=%d want 1", tick.RecoveredDispatching)
	}
	if got := adapter.count(); got != 0 {
		t.Fatalf("recovery reached the adapter: calls=%d want 0", got)
	}
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID); got != "UNKNOWN/unknown/unknown" {
		t.Fatalf("recovered execution=%s want UNKNOWN/unknown/unknown", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id='ws_fixture' AND task_id=$1 AND target_id=$2 AND state='reserved'`, taskID, r.TargetID); got != 1 {
		t.Fatalf("recovery released the goal slot: reserved=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 0 {
		t.Fatalf("recovery scheduled retries=%d want 0", got)
	}

	repeat, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("repeat recovery tick: %v", err)
	}
	if repeat.RecoveredDispatching != 0 || adapter.count() != 0 {
		t.Fatalf("repeat tick sent after recovery: tick=%+v calls=%d", repeat, adapter.count())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != 1 {
		t.Fatalf("recovery created a second execution: count=%d want 1", got)
	}
}

func TestWorkerRunStopsOnContextCancellation(t *testing.T) {
	s := fixtureStore(t)
	worker := workerFixture(t, s, nil, DefaultWorkerSteps())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	workerWaitRunning(t, worker)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
	// The lease used by a cancelled tick must be released, not leaked.
	lease, err := acquireTaskLease(context.Background(), s.Pool, "ws_fixture", "task_lifecycle_probe")
	if err != nil {
		t.Fatalf("acquire lease after stop: %v", err)
	}
	if lease == nil {
		t.Fatal("lease leaked after cancelled worker run")
	}
	lease.release()
}

func TestWorkerStopIsGracefulAndIdempotent(t *testing.T) {
	s := fixtureStore(t)
	worker := workerFixture(t, s, nil, DefaultWorkerSteps())
	done := make(chan error, 1)
	go func() { done <- worker.Run(context.Background()) }()
	workerWaitRunning(t, worker)
	worker.Stop()
	worker.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not stop after Stop")
	}

	// Stop before Run still terminates promptly.
	early := workerFixture(t, s, nil, DefaultWorkerSteps())
	early.Stop()
	earlyDone := make(chan error, 1)
	go func() { earlyDone <- early.Run(context.Background()) }()
	select {
	case err := <-earlyDone:
		if err != nil {
			t.Fatalf("early stop run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stopped-before-run worker did not return")
	}
}

func TestWorkerLeaseMutualExclusionAndRelease(t *testing.T) {
	s := fixtureStore(t)
	first, err := acquireTaskLease(context.Background(), s.Pool, "ws_fixture", "task_lease_probe")
	if err != nil {
		t.Fatalf("acquire first lease: %v", err)
	}
	if first == nil {
		t.Fatal("first lease not acquired")
	}
	second, err := acquireTaskLease(context.Background(), s.Pool, "ws_fixture", "task_lease_probe")
	if err != nil {
		first.release()
		t.Fatalf("acquire second lease: %v", err)
	}
	if second != nil {
		second.release()
		first.release()
		t.Fatal("two workers acquired the same task lease")
	}
	first.release()

	// Release must work when the acquisition context has already been cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	third, err := acquireTaskLease(ctx, s.Pool, "ws_fixture", "task_lease_probe")
	if err != nil || third == nil {
		cancel()
		t.Fatalf("acquire third lease: lease=%v err=%v", third, err)
	}
	cancel()
	third.release()
	fourth, err := acquireTaskLease(context.Background(), s.Pool, "ws_fixture", "task_lease_probe")
	if err != nil || fourth == nil {
		t.Fatalf("lease not released after cancellation: lease=%v err=%v", fourth, err)
	}
	fourth.release()
}

func TestWorkerLeaseLossStopsTheStaleWorker(t *testing.T) {
	s := fixtureStore(t)
	lease, err := acquireTaskLease(context.Background(), s.Pool, "ws_fixture", "task_stale_probe")
	if err != nil || lease == nil {
		t.Fatalf("acquire lease: lease=%v err=%v", lease, err)
	}
	// Simulate the server dropping the session that owned the lease.
	if err := lease.direct.Close(context.Background()); err != nil {
		t.Fatalf("close lease session: %v", err)
	}
	if err := lease.valid(context.Background()); Code(err) != workerLeaseLost {
		t.Fatalf("stale lease still valid: err=%v code=%s", err, Code(err))
	}
	lease.release()
	var takeover *taskLease
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		takeover, err = acquireTaskLease(context.Background(), s.Pool, "ws_fixture", "task_stale_probe")
		if err != nil {
			t.Fatalf("acquire takeover lease: %v", err)
		}
		if takeover != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if takeover == nil {
		t.Fatal("lease not recoverable after session loss")
	}
	takeover.release()
}

func TestWorkerTransportGate(t *testing.T) {
	s := fixtureStore(t)
	if _, err := NewWorker(s, WorkerConfig{
		Transport: WorkerFixtureTransport,
		Adapter:   &workerCountAdapter{},
		Steps:     DefaultWorkerSteps(),
	}); Code(err) != "VALIDATION_FAILED" {
		t.Fatalf("fixture transport accepted a caller adapter: %v", err)
	}
	if _, err := NewWorker(s, WorkerConfig{
		Transport: WorkerExternalTransport,
		Steps:     DefaultWorkerSteps(),
	}); Code(err) != "ACTION_FORBIDDEN" {
		t.Fatalf("external transport accepted without adapter/authorization: %v", err)
	}
	worker, err := NewWorker(s, WorkerConfig{Steps: DefaultWorkerSteps()})
	if err != nil {
		t.Fatalf("default worker: %v", err)
	}
	if worker.cfg.Transport != WorkerFixtureTransport {
		t.Fatalf("default transport=%s want fixture", worker.cfg.Transport)
	}
	if worker.cfg.PollInterval != workerDefaultPoll || worker.cfg.BatchSize != workerDefaultBatch {
		t.Fatalf("defaults poll=%s batch=%d", worker.cfg.PollInterval, worker.cfg.BatchSize)
	}
	if _, err := NewWorker(s, WorkerConfig{PollInterval: 10 * time.Millisecond, Steps: DefaultWorkerSteps()}); Code(err) != "VALIDATION_FAILED" {
		t.Fatalf("sub-minimum poll accepted: %v", err)
	}
}

func TestWorkerRunRejectsSecondRun(t *testing.T) {
	s := fixtureStore(t)
	worker := workerFixture(t, s, nil, DefaultWorkerSteps())
	if !strings.Contains(worker.cfg.WorkerID, "worker_") {
		t.Fatalf("fixture worker id=%s", worker.cfg.WorkerID)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Run(context.Background()) }()
	workerWaitRunning(t, worker)
	if err := worker.Run(context.Background()); Code(err) != "WORKER_ALREADY_RUNNING" {
		t.Fatalf("second Run err=%v want WORKER_ALREADY_RUNNING", err)
	}
	worker.Stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestWorkerFixtureTransportReceiptIsSubmitted(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_worker_fixture", "rt_worker_fixture", 2)
	task := createReadyTask(t, s, []string{"acc_worker_fixture"}, []string{"930041"}, 1, 2)
	taskID := string(task.Id)
	worker := workerFixture(t, s, nil, WorkerSteps{Dispatch: true, Outbox: true, Inbox: true})

	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("fixture transport tick: %v", err)
	}
	if tick.Dispatched != 1 {
		t.Fatalf("fixture transport dispatched=%d want 1", tick.Dispatched)
	}
	if got := obsTestString(t, s, `SELECT state||'/'||disposition||'/'||effect_certainty
		FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, taskID); got != "OBSERVED/submitted/confirmed_submitted" {
		t.Fatalf("fixture receipt execution=%s want OBSERVED/submitted/confirmed_submitted", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot
		WHERE workspace_id='ws_fixture' AND task_id=$1 AND state='reserved'`, taskID); got != 1 {
		t.Fatalf("fixture receipt reserved slots=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id='ws_fixture' AND task_id=$1 AND valid`, taskID); got != 0 {
		t.Fatalf("submitted receipt produced results=%d want 0 (not membership success)", got)
	}
}
