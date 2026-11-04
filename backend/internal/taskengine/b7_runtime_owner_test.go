package taskengine

import (
	"context"
	"net/http"
	"testing"
)

func TestB7RuntimeOwnerChangedWithCredentialRetained(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_owner", "runtime_old", 1)
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.account SET runtime_id='runtime_new' WHERE workspace_id='ws_fixture' AND id='account_owner'`); err != nil {
		t.Fatal(err)
	}
	a := conversationActor()
	a.RuntimeID = "runtime_old"
	a.OwnerEpoch = 1
	a.AccountIDs = []string{"account_owner"}
	a.Permissions["chat.trigger"] = true
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return a, nil })
	w := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_owner","conversation_kind":"group","conversation_id":"owner_probe","event_id":"owner_changed","body":"fixture"}`, a)
	if w.Code < 400 {
		t.Fatalf("old runtime accepted after account owner switch: %d %s", w.Code, w.Body.String())
	}
	for _, table := range []string{"operator_conversation", "operator_message", "operator_notification", "chat_send_permit"} {
		if count := scalarInt(t, s, "SELECT count(*) FROM core."+table+" WHERE workspace_id='ws_fixture'"); count != 0 {
			t.Errorf("denial side effect table=%s count=%d", table, count)
		}
	}
}
func TestB7RuntimeOwnerCurrentOwnerAccepted(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_owner", "runtime_old", 1)
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.account SET runtime_id='runtime_new' WHERE workspace_id='ws_fixture' AND id='account_owner'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority) VALUES('ws_fixture','runtime_new','account_owner',1,true)`); err != nil {
		t.Fatal(err)
	}
	a := conversationActor()
	a.RuntimeID = "runtime_new"
	a.OwnerEpoch = 1
	a.AccountIDs = []string{"account_owner"}
	a.Permissions["chat.trigger"] = true
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return a, nil })
	w := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_owner","conversation_kind":"group","conversation_id":"owner_probe","event_id":"current_owner","body":"fixture"}`, a)
	if w.Code != 201 {
		t.Fatalf("current owner accepted after account owner switch: %d %s", w.Code, w.Body.String())
	}
	for _, want := range []struct {
		table string
		count int
	}{{"operator_conversation", 1}, {"operator_message", 1}, {"operator_notification", 1}, {"chat_send_permit", 0}} {
		if count := scalarInt(t, s, "SELECT count(*) FROM core."+want.table+" WHERE workspace_id='ws_fixture'"); count != want.count {
			t.Errorf("accepted write table=%s count=%d want=%d", want.table, count, want.count)
		}
	}
}
