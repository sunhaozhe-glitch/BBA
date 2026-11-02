package taskengine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/oapi-codegen/nullable"
)

func TestP019TaskListPaginatesAndScopesToPrincipal(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	seedAccount(t, s, "acc_p019", "runtime_p019", 5)
	task1 := createReadyTask(t, s, []string{"acc_p019"}, []string{"90001901"}, 1, 1)
	task2 := createReadyTask(t, s, []string{"acc_p019"}, []string{"90001902"}, 1, 1)
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.task SET created_at=$2 WHERE workspace_id=$1 AND id=ANY($3)`, actor.WorkspaceID, task1.CreatedAt, []string{task1.Id, task2.Id}); err != nil {
		t.Fatal(err)
	}
	actor.Permissions = map[string]bool{"task.create": true, "task.read": true, "task.control": true}
	other := Actor{WorkspaceID: actor.WorkspaceID, PrincipalID: "principal_other", Permissions: map[string]bool{"task.create": true, "task.read": true}}
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.principal(workspace_id,id) VALUES($1,$2)`, other.WorkspaceID, other.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(context.Background(), other, "other-create", p019TaskRequest("acc_p019", "90001903")); err != nil {
		t.Fatalf("create other-principal task: %v", err)
	}
	page, cursor, more, err := s.ListTasks(context.Background(), actor, "", 1)
	if err != nil || len(page) != 1 || !more || cursor == "" {
		t.Fatalf("first page items=%d cursor=%q more=%v err=%v", len(page), cursor, more, err)
	}
	if page[0].ConfirmedUniqueCount != 0 || page[0].PendingExternalCount != 0 || page[0].UnknownExecutionCount != 0 {
		t.Fatalf("unexpected outcome counters: %+v", page[0])
	}
	second, next, hasMore, err := s.ListTasks(context.Background(), actor, cursor, 1)
	if err != nil || len(second) != 1 || hasMore || next != "" || second[0].Id == page[0].Id {
		t.Fatalf("second page=%+v cursor=%q more=%v err=%v", second, next, hasMore, err)
	}
	// PENDING and UNKNOWN are distinct unresolved outcomes; neither counts as confirmed.
	accountIDs := []string{"acc_p019_outcome1", "acc_p019_outcome2", "acc_p019_outcome3"}
	for index, accountID := range accountIDs {
		seedAccount(t, s, accountID, fmt.Sprintf("runtime_p019_outcome%d", index+1), 5)
	}
	outcomeTask := createReadyTask(t, s, accountIDs, []string{"90001904", "90001905", "90001906"}, 3, 1)
	for index, accountID := range accountIDs {
		reservation := reserveFixture(t, s, string(outcomeTask.Id), accountID)
		rt := obsTestAcceptPermit(t, s, reservation)
		if index == 0 {
			obsTestDispatch(t, s, rt, reservation, SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "p019-pending-submit"}, nil, false)
			obsTestExec(t, s, `UPDATE core.execution SET state='OBSERVED',disposition='pending',effect_certainty='confirmed_submitted',observed_at=clock_timestamp() WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, reservation.ExecutionID)
		} else if index == 1 {
			obsTestDispatch(t, s, rt, reservation, SendOutcome{}, errors.New("fixture transport uncertainty"), true)
		} else {
			obsTestDispatch(t, s, rt, reservation, SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "p019-confirmed-submit"}, nil, false)
			obsTestReceiveApply(t, s, reservation, obsTestObservation(reservation, "p019-confirmed-membership", 1, "membership.observed", "joined", true, "p019-confirmed-evidence", time.Now().UTC().Add(time.Second)))
		}
	}
	projected, _, _, err := s.ListTasks(context.Background(), actor, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	foundOutcome := false
	for _, item := range projected {
		if item.Id == outcomeTask.Id {
			foundOutcome = true
			if item.ConfirmedUniqueCount != 1 || item.PendingExternalCount != 1 || item.UnknownExecutionCount != 1 || item.ReservedTargetCount != 2 {
				t.Fatalf("pending/unknown projection=%+v", item)
			}
		}
	}
	if !foundOutcome {
		t.Fatal("outcome task missing from manager task list")
	}
	manager := actor
	manager.PrincipalID = "principal_manager"
	manager.Permissions = map[string]bool{"task.read": true, "task.manage_all": true}
	all, _, allMore, err := s.ListTasks(context.Background(), manager, "", 10)
	if err != nil || len(all) != 4 || allMore {
		t.Fatalf("manager list items=%d more=%v err=%v want 4 tasks", len(all), allMore, err)
	}
}

func p019TaskRequest(accountID, groupID string) contract.CreateTaskRequest {
	mode := contract.ExecutionMode("dry_run")
	return contract.CreateTaskRequest{Type: "group.acquire", Title: "p019 isolation", JoinReason: "fixture reason", VerificationMessage: "fixture answer", Goal: contract.Goal{Metric: "new_unique_groups_for_account_pool", Count: 1, StrictTargetCap: true, RequireFinalMembershipCheck: true}, AllowedAccountIds: []string{accountID}, CandidateSource: contract.CandidateSource{Kind: "fixed", Groups: []contract.FixedCandidate{{GroupId: groupID, SourceRef: "p019"}}}, ExecutionMode: &mode, DeadlineAt: time.Now().UTC().Add(time.Hour), Limits: contract.TaskLimits{MaxCandidates: 1, MaxAttemptsPerAction: 1, MaxTotalExternalAttempts: 1, MaxModelAttempts: 0}, ApprovalRef: nullable.NewNullNullable[string](), Submit: false}
}

func TestP019ReconcileReceiptIsDurableIdempotentAndReadOnly(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	seedAccount(t, s, "acc_p019", "runtime_p019", 5)
	task := createReadyTask(t, s, []string{"acc_p019"}, []string{"90001911"}, 1, 1)
	req := contract.ReconcileRequest{ExpectedVersion: task.Version, Reason: "operator requests read-only membership refresh"}
	first, err := s.ReconcileTask(context.Background(), actor, task.Id, "p019-reconcile-key", req)
	if err != nil || first.Action != contract.ReceiptActionReconcile || first.State != contract.Accepted {
		t.Fatalf("reconcile receipt=%+v err=%v", first, err)
	}
	replay, err := s.ReconcileTask(context.Background(), actor, task.Id, "p019-reconcile-key", req)
	if err != nil || replay != first {
		t.Fatalf("replay=%+v first=%+v err=%v", replay, first, err)
	}
	if _, err := s.ReconcileTask(context.Background(), actor, task.Id, "p019-stale-key", contract.ReconcileRequest{ExpectedVersion: task.Version + 1, Reason: req.Reason}); Code(err) != taskCodeVersionConflict {
		t.Fatalf("stale reconcile code=%q want %s", Code(err), taskCodeVersionConflict)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='manual_reconciliation_requested'`, actor.WorkspaceID, task.Id); got != 1 {
		t.Fatalf("reconcile audit events=%d want exactly 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.outbox WHERE workspace_id=$1 AND task_id=$2 AND event_type='manual_reconciliation_requested'`, actor.WorkspaceID, task.Id); got != 1 {
		t.Fatalf("reconcile outbox rows=%d want exactly 1", got)
	}
	current, err := s.GetTask(context.Background(), actor, task.Id)
	if err != nil || current.Version != task.Version || current.State != task.State {
		t.Fatalf("read-only reconcile changed task: before=%+v after=%+v err=%v", task, current, err)
	}
}
