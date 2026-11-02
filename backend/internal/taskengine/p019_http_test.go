package taskengine

import (
	"encoding/json"
	"net/http"
	"testing"

	"bba.local/taskengine/internal/contract"
)

func TestP019HTTPPagesAndReadOnlyReconcileReceipt(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	seedAccount(t, s, "acc_p019_http", "runtime_p019_http", 5)
	seedAccount(t, s, "acc_p019_http2", "runtime_p019_http2", 5)
	task := createReadyTask(t, s, []string{"acc_p019_http"}, []string{"90001921"}, 1, 1)
	handler := s.Handler(func(*http.Request) (Actor, error) { return actor, nil })

	accounts := ttServe(handler, http.MethodGet, "/v1/accounts?limit=1", nil, nil)
	if accounts.Code != http.StatusOK {
		t.Fatalf("account list status=%d body=%s", accounts.Code, accounts.Body.String())
	}
	var accountPage contract.AccountPageEnvelope
	if err := json.Unmarshal(accounts.Body.Bytes(), &accountPage); err != nil {
		t.Fatal(err)
	}
	if len(accountPage.Data.Items) != 1 || accountPage.Data.Items[0].Id != "acc_p019_http" || accountPage.Data.Items[0].SelfId.IsNull() || !accountPage.Meta.HasMore || accountPage.Meta.NextCursor.IsNull() {
		t.Fatalf("account page=%+v", accountPage)
	}
	accountCursor, _ := accountPage.Meta.NextCursor.Get()
	secondAccountPage := ttServe(handler, http.MethodGet, "/v1/accounts?limit=1&cursor="+accountCursor, nil, nil)
	if secondAccountPage.Code != http.StatusOK {
		t.Fatalf("second account page status=%d body=%s", secondAccountPage.Code, secondAccountPage.Body.String())
	}
	var secondAccounts contract.AccountPageEnvelope
	if err := json.Unmarshal(secondAccountPage.Body.Bytes(), &secondAccounts); err != nil {
		t.Fatal(err)
	}
	if len(secondAccounts.Data.Items) != 1 || secondAccounts.Data.Items[0].Id != "acc_p019_http2" || secondAccounts.Meta.HasMore {
		t.Fatalf("second account page=%+v", secondAccounts)
	}

	tasks := ttServe(handler, http.MethodGet, "/v1/tasks?limit=1", nil, nil)
	if tasks.Code != http.StatusOK {
		t.Fatalf("task list status=%d body=%s", tasks.Code, tasks.Body.String())
	}
	var taskPage contract.TaskPageEnvelope
	if err := json.Unmarshal(tasks.Body.Bytes(), &taskPage); err != nil {
		t.Fatal(err)
	}
	if len(taskPage.Data.Items) != 1 || taskPage.Data.Items[0].Id != task.Id || taskPage.Data.Items[0].ConfirmedUniqueCount != 0 || taskPage.Data.Items[0].UnknownExecutionCount != 0 {
		t.Fatalf("task page=%+v", taskPage)
	}

	invalidPage := ttServe(handler, http.MethodGet, "/v1/tasks?limit=0", nil, nil)
	if invalidPage.Code != http.StatusBadRequest {
		t.Fatalf("invalid task page status=%d body=%s", invalidPage.Code, invalidPage.Body.String())
	}
	readOnlyOperator := actor
	readOnlyOperator.Permissions = map[string]bool{"operator.read": true}
	readOnlyHandler := s.Handler(func(*http.Request) (Actor, error) { return readOnlyOperator, nil })
	readOnlyAccounts := ttServe(readOnlyHandler, http.MethodGet, "/v1/accounts", nil, nil)
	if readOnlyAccounts.Code != http.StatusOK {
		t.Fatalf("account list with operator.read status=%d body=%s", readOnlyAccounts.Code, readOnlyAccounts.Body.String())
	}

	noAccountPermission := actor
	noAccountPermission.Permissions = map[string]bool{"task.read": true}
	noAccountHandler := s.Handler(func(*http.Request) (Actor, error) { return noAccountPermission, nil })
	forbiddenAccounts := ttServe(noAccountHandler, http.MethodGet, "/v1/accounts", nil, nil)
	if forbiddenAccounts.Code != http.StatusForbidden {
		t.Fatalf("account list without manager permission status=%d body=%s", forbiddenAccounts.Code, forbiddenAccounts.Body.String())
	}
	detail := ttServe(handler, http.MethodGet, "/v1/tasks/"+task.Id, nil, nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("task detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	var detailEnvelope contract.TaskEnvelope
	if err := json.Unmarshal(detail.Body.Bytes(), &detailEnvelope); err != nil {
		t.Fatal(err)
	}
	if detailEnvelope.Data.Id != task.Id || detailEnvelope.Data.Version != task.Version || detailEnvelope.Data.PendingExternalCount != taskPage.Data.Items[0].PendingExternalCount || detailEnvelope.Data.UnknownExecutionCount != taskPage.Data.Items[0].UnknownExecutionCount {
		t.Fatalf("detail/list projections disagree: detail=%+v page=%+v", detailEnvelope.Data, taskPage.Data.Items[0])
	}

	req := contract.ReconcileRequest{ExpectedVersion: task.Version, Reason: "fixture read-only reconciliation request"}
	body := ttJSONBody(t, req)
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "p019-http-reconcile"}
	first := ttServe(handler, http.MethodPost, "/v1/tasks/"+task.Id+"/reconcile", body, headers)
	second := ttServe(handler, http.MethodPost, "/v1/tasks/"+task.Id+"/reconcile", body, headers)
	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("reconcile HTTP statuses=%d,%d first=%s second=%s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	var receipt contract.ReceiptEnvelope
	if err := json.Unmarshal(first.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Data.Action != contract.ReceiptActionReconcile || receipt.Data.State != contract.Accepted || receipt.Data.ResourceId != task.Id {
		t.Fatalf("receipt=%+v", receipt.Data)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='manual_reconciliation_requested'`, actor.WorkspaceID, task.Id); got != 1 {
		t.Fatalf("reconcile route wrote %d audit rows", got)
	}
}

func TestP019TaskEntryCannotListOrReconcile(t *testing.T) {
	s := fixtureStore(t)
	actor := Actor{WorkspaceID: "ws_fixture", PrincipalID: "principal_fixture", RuntimeID: "runtime_entry", SenderID: "sender_entry", AccountIDs: []string{"acc_entry"}, Permissions: map[string]bool{"operator.read": true}}
	handler := s.Handler(func(*http.Request) (Actor, error) { return actor, nil })
	entryHeaders := map[string]string{"X-QQ-Account-ID": "acc_entry", "X-QQ-Sender-ID": "sender_entry"}
	for _, path := range []string{"/v1/accounts", "/v1/tasks"} {
		rec := ttServe(handler, http.MethodGet, path, nil, entryHeaders)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("task entry GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	rec := ttServe(handler, http.MethodGet, "/v1/tasks/task_example/reconcile", nil, entryHeaders)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET reconcile status=%d want 405", rec.Code)
	}
	req := contract.ReconcileRequest{ExpectedVersion: 1, Reason: "forbidden"}
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "task-entry-reconcile", "X-QQ-Account-ID": "acc_entry", "X-QQ-Sender-ID": "sender_entry"}
	rec = ttServe(handler, http.MethodPost, "/v1/tasks/task_example/reconcile", ttJSONBody(t, req), headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("task entry reconcile status=%d body=%s", rec.Code, rec.Body.String())
	}
}
