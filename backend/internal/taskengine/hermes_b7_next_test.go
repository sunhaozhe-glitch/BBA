package taskengine

import (
	"net/http"
	"testing"
)

func TestHermesAgentExactReplay(t *testing.T) {
	s := fixtureStore(t)
	a := conversationActor()
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return a, nil })
	path := "/v1/operator/agent-threads/hermes_replay/messages"
	first := conversationRequest(h, http.MethodPost, path, `{"body":"synthetic"}`, a)
	replay := conversationRequest(h, http.MethodPost, path, `{"body":"synthetic"}`, a)
	if first.Code != 201 {
		t.Fatalf("positive path: %d %s", first.Code, first.Body.String())
	}
	if replay.Code != first.Code || replay.Body.String() != first.Body.String() {
		t.Errorf("not exact replay first=%d %s replay=%d %s", first.Code, first.Body.String(), replay.Code, replay.Body.String())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.operator_message`); got != 1 {
		t.Errorf("message count=%d", got)
	}
}

func TestHermesDraftExactReplay(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "draft_account", "draft_runtime", 1)
	a := conversationActor()
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return a, nil })
	body := `{"account_id":"draft_account","conversation_kind":"group","conversation_id":"synthetic_group","body":"synthetic","expected_version":1}`
	first := conversationRequest(h, http.MethodPost, "/v1/operator/qq-drafts", body, a)
	replay := conversationRequest(h, http.MethodPost, "/v1/operator/qq-drafts", body, a)
	if first.Code != 201 {
		t.Fatalf("positive path: %d %s", first.Code, first.Body.String())
	}
	if replay.Code != first.Code || replay.Body.String() != first.Body.String() {
		t.Errorf("not exact replay first=%d %s replay=%d %s", first.Code, first.Body.String(), replay.Code, replay.Body.String())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.qq_draft`); got != 1 {
		t.Errorf("draft count=%d", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.chat_send_permit`); got != 0 {
		t.Errorf("permit count=%d", got)
	}
}
