package taskengine

// Round-4 runtime rotation fixtures (docs/astra-audit-next-04.md, item 2).
//
// The worker must sweep every active runtime/owner group through a bounded,
// durable, restartable round robin. These fixtures are written against the
// head-restarting enumeration on purpose: a pass that re-walks from the first
// page can never reach a group past workerMaxRuntimePages pages, so the tail
// below is starved and the pass is closed after only part of the population.

import (
	"context"
	"fmt"
	"testing"
)

// workerRotationOperations seeds count runtime groups, each with the crash
// window recovery exists for (reserved, accepted and permitted, never sent).
// Runtime ids sort in seeding order, so the rotation order is known.
func workerRotationOperations(t *testing.T, s *Store, prefix string, base, count int) []Reservation {
	t.Helper()
	out := make([]Reservation, 0, count)
	for i := range count {
		out = append(out, workerRecoveryPendingOperationRoute(t, s, prefix, base, i))
	}
	return out
}

// workerRecoveryPendingOperationRoute is workerRecoveryPendingOperation with
// deterministic, sortable account/runtime/group names.
func workerRecoveryPendingOperationRoute(t *testing.T, s *Store, prefix string, base, index int) Reservation {
	t.Helper()
	return workerRecoveryPendingOperation(t, s,
		fmt.Sprintf("acc_%s_%02d", prefix, index+1),
		fmt.Sprintf("rt_%s_%02d", prefix, index+1),
		// Group ids are workspace-wide, so each fixture block uses its own range.
		fmt.Sprintf("94%05d", base+index+1))
}

// workerRotationQuarantined counts how many of the seeded groups were
// quarantined as UNKNOWN, i.e. actually visited by a recovery pass.
func workerRotationQuarantined(t *testing.T, s *Store, reservations []Reservation) int {
	t.Helper()
	ids := make([]string, 0, len(reservations))
	for _, r := range reservations {
		ids = append(ids, r.OperationID)
	}
	return scalarInt(t, s, `SELECT count(*) FROM bridge.gateway_operation WHERE workspace_id='ws_fixture' AND operation_id=ANY($1::text[]) AND state='UNKNOWN'`, ids)
}

// F4: a population of 25 groups with BatchSize 1 needs three bounded ticks at
// workerMaxRuntimePages pages per tick. A pass that restarts at the head every
// tick quarantines the same ten groups and never reaches the tail, and it closes
// the pass after the first tick even though 15 groups were never visited.
func TestWorkerRecoveryFairRotationBeyondPageCap(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	const groupCount = 25
	reservations := workerRotationOperations(t, s, "fair", 100, groupCount)
	worker := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	worker.cfg.BatchSize = 1

	recovered := 0
	for tick := 1; tick <= 3; tick++ {
		report, err := worker.Tick(ctx)
		if err != nil {
			t.Fatalf("rotation tick %d: %v", tick, err)
		}
		recovered += report.RecoveredDispatching
		if tick == 1 {
			if report.RecoveredDispatching == 0 {
				t.Fatal("first rotation tick quarantined nothing")
			}
			if !worker.recoveryPending() {
				t.Fatalf("recovery pass closed after %d of %d groups: the tail was skipped", recovered, groupCount)
			}
		}
	}
	if got := workerRotationQuarantined(t, s, reservations); got != groupCount {
		t.Fatalf("rotation quarantined %d of %d groups after 3 bounded ticks", got, groupCount)
	}
	if recovered != groupCount {
		t.Fatalf("rotation recovered %d operations for %d groups: a group was visited twice instead of advancing", recovered, groupCount)
	}
	if worker.recoveryPending() {
		t.Fatal("recovery pass still pending after the rotation closed at the tail")
	}
}

// F4: the position is durable, so a restarted worker continues the rotation
// instead of serving the same head page again.
func TestWorkerRecoveryRotationCursorSurvivesRestart(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	const groupCount = 13
	reservations := workerRotationOperations(t, s, "restart", 200, groupCount)

	first := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	first.cfg.BatchSize = 1
	report, err := first.Tick(ctx)
	if err != nil {
		t.Fatalf("first rotation tick: %v", err)
	}
	if report.RecoveredDispatching == 0 || report.RecoveredDispatching >= groupCount {
		t.Fatalf("first tick recovered=%d want a bounded page of the %d groups", report.RecoveredDispatching, groupCount)
	}
	firstRecovered := report.RecoveredDispatching

	restarted := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	restarted.cfg.BatchSize = 1
	report, err = restarted.Tick(ctx)
	if err != nil {
		t.Fatalf("restarted rotation tick: %v", err)
	}
	if report.RecoveredDispatching != groupCount-firstRecovered {
		t.Fatalf("restarted tick recovered=%d want the %d unvisited groups", report.RecoveredDispatching, groupCount-firstRecovered)
	}
	if got := workerRotationQuarantined(t, s, reservations); got != groupCount {
		t.Fatalf("restarted rotation quarantined %d of %d groups", got, groupCount)
	}
	// The inherited lap may not close the pass: the restarted process owes a full
	// head-to-tail lap, so a group that started DISPATCHING after the previous
	// process committed its last page is still covered.
	if !restarted.recoveryPending() {
		t.Fatal("restarted worker closed its pass on an inherited partial lap")
	}
	for tick := range 3 {
		if _, err := restarted.Tick(ctx); err != nil {
			t.Fatalf("startup coverage tick %d: %v", tick+1, err)
		}
	}
	if restarted.recoveryPending() {
		t.Fatal("restarted recovery pass did not close after a full startup lap")
	}
}

// F4: retiring or deleting a group the cursor has not reached must not act for it
// under a stale credential, a group inserted ahead of the cursor is served in the
// same lap, and a group inserted behind the closed lap's position is served by the
// next rotation (next lap or next process) instead of being skipped. Groups the
// first window already quarantined keep that state, and no group is acted for
// twice.
func TestWorkerRecoveryRotationSurvivesInsertAndDelete(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	const initial = 14
	reservations := workerRotationOperations(t, s, "churn", 300, initial)
	worker := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	worker.cfg.BatchSize = 1

	report, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("pre-churn rotation tick: %v", err)
	}
	visited := report.RecoveredDispatching
	if visited == 0 || visited >= initial {
		t.Fatalf("pre-churn tick recovered=%d want a bounded page", visited)
	}
	if got := workerRotationQuarantined(t, s, reservations[:visited]); got != visited {
		t.Fatalf("pre-churn window quarantined %d of %d groups", got, visited)
	}

	// The last two groups are still unvisited when the operator retires one
	// credential and deletes the other.
	retired := reservations[initial-2]
	deleted := reservations[initial-1]
	if _, err := s.Pool.Exec(ctx, `UPDATE core.runtime_credentials SET active=false WHERE workspace_id='ws_fixture' AND runtime_id=$1`, retired.RuntimeID); err != nil {
		t.Fatalf("retire credentials: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM core.runtime_credentials WHERE workspace_id='ws_fixture' AND runtime_id=$1`, deleted.RuntimeID); err != nil {
		t.Fatalf("delete credentials: %v", err)
	}

	// One group is inserted behind the cursor (inside the window this lap already
	// covered) and one past the tail.
	behind := workerRecoveryPendingOperation(t, s, "acc_churn_before", "rt_churn_05b", "94315")
	ahead := workerRecoveryPendingOperationRoute(t, s, "zchurn", 400, 0)

	// The same worker must keep rotating: the population change is detected, the
	// lap that closed over the old population cannot complete the pass, and the next
	// lap starts at the head and serves the group that appeared behind the position.
	if _, err := worker.Tick(ctx); err != nil {
		t.Fatalf("post-churn rotation tick: %v", err)
	}
	if !worker.recoveryPending() {
		t.Fatal("recovery pass closed on a lap whose population changed underneath it")
	}
	for tick := range 2 {
		if _, err := worker.Tick(ctx); err != nil {
			t.Fatalf("post-churn rotation tick %d: %v", tick+2, err)
		}
	}
	if got := workerRotationQuarantined(t, s, []Reservation{ahead}); got != 1 {
		t.Fatal("group inserted past the tail was never served")
	}
	if got := workerRotationQuarantined(t, s, []Reservation{behind}); got != 1 {
		t.Fatal("group inserted behind the closed lap's position was never served by the same worker")
	}
	if worker.recoveryPending() {
		t.Fatal("recovery pass did not close after the population change was swept")
	}
	for _, stale := range []Reservation{retired, deleted} {
		if got := gatewayState(t, s, stale.OperationID); got != "DISPATCHING" {
			t.Fatalf("retired group acted for under a stale credential: state=%s want DISPATCHING", got)
		}
		if got := executionState(t, s, stale.OperationID); got != "ACCEPTED" {
			t.Fatalf("retired group execution state=%s want ACCEPTED (original never-sent state)", got)
		}
	}
	if got := workerRotationQuarantined(t, s, reservations[:visited]); got != visited {
		t.Fatalf("rotation revisited and changed %d already quarantined groups", visited-got)
	}
	settled, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("settled rotation tick: %v", err)
	}
	if settled.RecoveredDispatching != 0 {
		t.Fatalf("settled rotation re-quarantined %d groups", settled.RecoveredDispatching)
	}
	if got := workerRotationQuarantined(t, s, reservations[:visited]); got != visited {
		t.Fatalf("settled rotation changed %d already quarantined groups", visited-got)
	}
}

// F4: one group that cannot be quarantined must not starve the rest of the
// rotation, and the failure is durable: a restarted worker that reaches the tail
// must not close the pass before that group was retried.
func TestWorkerRecoveryFailedActorDoesNotStarveRotation(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	const groupCount = 12
	reservations := workerRotationOperations(t, s, "starve", 500, groupCount)
	workerRecoveryFault(t, s, reservations[0].ExecutionID)
	first := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	first.cfg.BatchSize = 1

	report, err := first.Tick(ctx)
	if err == nil {
		t.Fatal("rotation tick with the fault injected succeeded")
	}
	if report.RecoveredDispatching == 0 {
		t.Fatal("one failed group blocked the whole rotation")
	}
	if !first.recoveryPending() {
		t.Fatal("failed lap was treated as a completed recovery pass")
	}
	workerRecoveryFaultClear(t, s)

	// A restarted worker reaches the tail with the durable failure still owed. It
	// must keep the pass pending instead of closing it over the failed group.
	restarted := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	restarted.cfg.BatchSize = 1
	if _, err := restarted.Tick(ctx); err != nil {
		t.Fatalf("restarted rotation tick: %v", err)
	}
	if !restarted.recoveryPending() {
		t.Fatal("restarted worker closed the pass on a clean tail while an earlier page had failed")
	}
	for tick := range 4 {
		if _, err := restarted.Tick(ctx); err != nil {
			t.Fatalf("retry rotation tick %d: %v", tick+1, err)
		}
	}
	if got := workerRotationQuarantined(t, s, reservations); got != groupCount {
		t.Fatalf("rotation quarantined %d of %d groups after the fault cleared", got, groupCount)
	}
	if restarted.recoveryPending() {
		t.Fatal("recovery pass did not close after a clean lap")
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND state='UNKNOWN'`); got != groupCount {
		t.Fatalf("quarantined executions=%d want %d", got, groupCount)
	}
}

// F4: enumeration alone claims nothing. An enumerated batch that is never acted
// on (the crash window the durable cursor exists for) must be repeated by the
// next worker, not stepped over.
func TestWorkerRecoveryEnumerationClaimsNothingUntilActed(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	const groupCount = 12
	reservations := workerRotationOperations(t, s, "abandon", 600, groupCount)
	worker := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	worker.cfg.BatchSize = 1

	rotation, err := worker.runtimeActors(ctx, s.Pool, workerCursorRecovery, worker.cfg.BatchSize)
	if err != nil {
		t.Fatalf("walk runtime groups: %v", err)
	}
	if len(rotation.Actors) == 0 || rotation.LapClosed {
		t.Fatalf("walk actors=%d lapClosed=%v want a bounded page of %d groups", len(rotation.Actors), rotation.LapClosed, groupCount)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.worker_cursor WHERE consumer=$1`, workerCursorRecovery); got != 0 {
		t.Fatalf("enumeration stored a position without acting on the batch: cursor rows=%d", got)
	}
	if got := workerRotationQuarantined(t, s, reservations); got != 0 {
		t.Fatalf("enumeration quarantined %d groups", got)
	}

	restarted := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	restarted.cfg.BatchSize = 1
	for tick := range 3 {
		if _, err := restarted.Tick(ctx); err != nil {
			t.Fatalf("recovery tick %d: %v", tick+1, err)
		}
	}
	if got := workerRotationQuarantined(t, s, reservations); got != groupCount {
		t.Fatalf("recovery after an abandoned batch quarantined %d of %d groups", got, groupCount)
	}
	if restarted.recoveryPending() {
		t.Fatal("recovery pass did not close")
	}
}

// F4: the recovery rotation is owned by one session at a time, so a second
// instance cannot close a lap (or clear a durable failure) it does not own.
func TestWorkerRecoveryRotationLeaseExcludesOtherInstances(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	const groupCount = 3
	reservations := workerRotationOperations(t, s, "lease", 700, groupCount)
	other := workerFixture(t, s, nil, WorkerSteps{Recover: true})
	other.cfg.BatchSize = 1

	owner, err := acquireSessionLease(ctx, s.Pool, "taskengine.worker.rotation."+workerCursorRecovery)
	if err != nil || owner == nil {
		t.Fatalf("acquire the recovery rotation lease: %v", err)
	}
	for range 2 {
		if _, err := other.Tick(ctx); err != nil {
			t.Fatalf("excluded recovery tick: %v", err)
		}
	}
	if got := workerRotationQuarantined(t, s, reservations); got != 0 {
		t.Fatalf("excluded worker quarantined %d groups it does not own", got)
	}
	if !other.recoveryPending() {
		t.Fatal("excluded worker closed its recovery pass")
	}
	owner.release()

	report, err := other.Tick(ctx)
	if err != nil {
		t.Fatalf("recovery tick after the lease was released: %v", err)
	}
	if report.RecoveredDispatching != groupCount {
		t.Fatalf("recovery after the lease was released recovered=%d want %d", report.RecoveredDispatching, groupCount)
	}
	if other.recoveryPending() {
		t.Fatal("recovery pass did not close after the lease was released")
	}
}

// F4: the bridge outbox pump rotates through the same durable cursor, so a
// receipt past the page cap is still delivered, exactly once, with no repeated
// external effect.
func TestWorkerBridgeOutboxRotationBeyondPageCap(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	const groupCount = 12
	adapter := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fixture_rotation_evidence"}}
	operations := make([]string, 0, groupCount)
	for i := range groupCount {
		account := fmt.Sprintf("acc_bridge_%02d", i+1)
		runtime := fmt.Sprintf("rt_bridge_%02d", i+1)
		group := fmt.Sprintf("95%05d", i+1)
		seedAccount(t, s, account, runtime, 2)
		task := createReadyTask(t, s, []string{account}, []string{group}, 1, 2)
		r := reserveFixture(t, s, string(task.Id), account)
		accepted := acceptFixture(t, s, fixtureRuntime(account, runtime), r)
		if _, err := s.IssuePermit(ctx, fixtureRuntime(account, runtime), accepted.OperationID, 10_000_000_000); err != nil {
			t.Fatalf("issue permit %s: %v", runtime, err)
		}
		if _, err := s.Dispatch(ctx, fixtureRuntime(account, runtime), accepted.OperationID, identityOK, adapter); err != nil {
			t.Fatalf("dispatch %s: %v", runtime, err)
		}
		operations = append(operations, accepted.OperationID)
	}
	if got := adapter.count(); got != groupCount {
		t.Fatalf("adapter calls=%d want %d", got, groupCount)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM bridge.outbox WHERE workspace_id='ws_fixture' AND state='pending'`); got != groupCount {
		t.Fatalf("pending bridge rows before the pump=%d want %d", got, groupCount)
	}

	worker := workerFixture(t, s, nil, WorkerSteps{Outbox: true})
	worker.cfg.BatchSize = 1
	pumped := 0
	for tick := range 2 {
		report, err := worker.Tick(ctx)
		if err != nil {
			t.Fatalf("bridge rotation tick %d: %v", tick+1, err)
		}
		pumped += report.BridgeAccepted
	}
	if pumped != groupCount {
		t.Fatalf("bridge rotation pumped %d of %d receipts", pumped, groupCount)
	}
	for _, operationID := range operations {
		if got := scalarInt(t, s, `SELECT count(*) FROM bridge.outbox WHERE workspace_id='ws_fixture' AND operation_id=$1 AND state='delivered'`, operationID); got != 1 {
			t.Fatalf("operation %s receipt delivered=%d want 1", operationID, got)
		}
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.inbox WHERE workspace_id='ws_fixture'`); got != groupCount {
		t.Fatalf("inbox rows=%d want %d (repeated receipt application)", got, groupCount)
	}
	if got := adapter.count(); got != groupCount {
		t.Fatalf("bridge rotation repeated the external send: adapter calls=%d want %d", got, groupCount)
	}
}
