package taskengine

import (
	"context"
	"net/http"
	"testing"
)

// Independent regression: rotation of both persisted rows must not revive an
// already issued runtime identity. HTTP resolver claim propagation is checked
// separately by the compiled-service probe.
func TestHermesSynchronizedRotationDeniesOldIdentity(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "rotation_account", "rotation_runtime", 1)
	a := conversationActor()
	a.RuntimeID = "rotation_runtime"
	a.AccountIDs = []string{"rotation_account"}
	a.Permissions["chat.trigger"] = true
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return a, nil })
	body := `{"account_id":"rotation_account","conversation_kind":"private","conversation_id":"rotation_chat","event_id":"before_rotation","body":"synthetic"}`
	before := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", body, a)
	if before.Code != 201 {
		t.Fatalf("positive path: %d %s", before.Code, before.Body.String())
	}
	_, err := s.Pool.Exec(context.Background(), `UPDATE core.account SET owner_epoch=owner_epoch+1 WHERE workspace_id='ws_fixture' AND id='rotation_account'; UPDATE core.runtime_credentials SET owner_epoch=owner_epoch+1 WHERE workspace_id='ws_fixture' AND runtime_id='rotation_runtime'`)
	if err != nil {
		t.Fatal(err)
	}
	beforeCount := scalarInt(t, s, `SELECT count(*) FROM core.operator_message`)
	replay := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", body, a)
	fresh := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"rotation_account","conversation_kind":"private","conversation_id":"rotation_chat","event_id":"after_rotation","body":"synthetic"}`, a)
	if replay.Code < 400 {
		t.Errorf("old identity replay accepted: %d %s", replay.Code, replay.Body.String())
	}
	if fresh.Code < 400 {
		t.Errorf("old identity new event accepted: %d %s", fresh.Code, fresh.Body.String())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.operator_message`); got != beforeCount {
		t.Errorf("persistent message growth: before=%d after=%d", beforeCount, got)
	}
}
