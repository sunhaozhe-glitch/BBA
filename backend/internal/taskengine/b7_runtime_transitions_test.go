package taskengine

import (
	"context"
	"net/http"
	"testing"
)

func TestB7IndependentRuntimeCredentialTransitions(t *testing.T) {
	for _, scenario := range []struct{ name, sql string }{
		{"inactive", `UPDATE core.runtime_credentials SET active=false WHERE workspace_id='ws_fixture' AND runtime_id='runtime_a'`},
		{"credential_epoch_stale", `UPDATE core.runtime_credentials SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND runtime_id='runtime_a'`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := fixtureStore(t)
			seedAccount(t, s, "account_a", "runtime_a", 1)
			if _, err := s.Pool.Exec(context.Background(), scenario.sql); err != nil {
				t.Fatal(err)
			}
			actor := conversationActor()
			actor.RuntimeID = "runtime_a"
			actor.AccountIDs = []string{"account_a"}
			actor.Permissions["chat.trigger"] = true
			h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return actor, nil })
			r := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_a","conversation_kind":"private","conversation_id":"transition","event_id":"transition_event","body":"fixture transition"}`, actor)
			if r.Code < 400 {
				t.Errorf("credential transition accepted status=%d body=%s", r.Code, r.Body.String())
			}
			for _, table := range []string{"operator_conversation", "operator_message", "operator_notification", "chat_send_permit"} {
				if count := scalarInt(t, s, "SELECT count(*) FROM core."+table+" WHERE workspace_id='ws_fixture'"); count != 0 {
					t.Errorf("denial side effect table=%s count=%d", table, count)
				}
			}
		})
	}
}
