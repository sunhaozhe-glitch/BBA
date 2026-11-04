package taskengine

import (
	"context"
	"net/http"
	"testing"
)

func TestB7IndependentAccountMessageIsolation(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_a", "runtime_a", 1)
	seedAccount(t, s, "account_b", "runtime_b", 1)
	op := conversationActor()
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return op, nil })
	d := conversationRequest(h, http.MethodPost, "/v1/operator/qq-drafts", `{"account_id":"account_b","conversation_kind":"group","conversation_id":"group_b","body":"private-account-b","expected_version":1}`, op)
	if d.Code != 201 {
		t.Fatal(d.Code, d.Body.String())
	}
	var conv string
	if e := s.Pool.QueryRow(context.Background(), `SELECT conversation_id FROM core.qq_draft WHERE workspace_id='ws_fixture'`).Scan(&conv); e != nil {
		t.Fatal(e)
	}
	op.AccountIDs = []string{"account_a"}
	r := conversationRequest(h, http.MethodGet, "/v1/operator/conversations/"+conv+"/messages", "", op)
	if r.Code != 404 {
		t.Fatalf("cross-account messages accepted status=%d body=%s", r.Code, r.Body.String())
	}
	// Account-scoped actors are a handler/custom-auth contract; the production
	// operator resolver intentionally does not expose this configuration.
	for _, scope := range [][]string{{"account_b"}, nil} {
		op.AccountIDs = scope
		r = conversationRequest(h, http.MethodGet, "/v1/operator/conversations/"+conv+"/messages", "", op)
		if r.Code != http.StatusOK {
			t.Fatalf("allowed scope=%v status=%d body=%s", scope, r.Code, r.Body.String())
		}
	}
}
func TestB7IndependentRuntimeOwnership(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_a", "runtime_a", 1)
	seedAccount(t, s, "account_b", "runtime_b", 1)
	runtime := conversationActor()
	runtime.RuntimeID = "runtime_a"
	runtime.OwnerEpoch = 1
	runtime.AccountIDs = []string{"account_b"}
	runtime.Permissions["chat.trigger"] = true
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return runtime, nil })
	r := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_b","conversation_kind":"group","conversation_id":"group_x","event_id":"bad-owner","body":"wrong runtime"}`, runtime)
	if r.Code < 400 {
		t.Fatalf("runtime/account mismatch accepted status=%d body=%s", r.Code, r.Body.String())
	}
}
func TestB7IndependentRuntimeRevocation(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_a", "runtime_a", 1)
	if _, e := s.Pool.Exec(context.Background(), `DELETE FROM core.runtime_credentials WHERE workspace_id='ws_fixture' AND runtime_id='runtime_a'`); e != nil {
		t.Fatal(e)
	}
	runtime := conversationActor()
	runtime.RuntimeID = "runtime_a"
	runtime.OwnerEpoch = 1
	runtime.AccountIDs = []string{"account_a"}
	runtime.Permissions["chat.trigger"] = true
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return runtime, nil })
	r := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_a","conversation_kind":"group","conversation_id":"group_x","event_id":"revoked","body":"revoked runtime"}`, runtime)
	if r.Code < 400 {
		t.Fatalf("revoked runtime accepted status=%d body=%s", r.Code, r.Body.String())
	}
}
func TestB7IndependentRuntimeCredentialPositivePath(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_a", "runtime_a", 1)
	runtime := conversationActor()
	runtime.RuntimeID = "runtime_a"
	runtime.OwnerEpoch = 1
	runtime.AccountIDs = []string{"account_a"}
	runtime.Permissions["chat.trigger"] = true
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return runtime, nil })
	r := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_a","conversation_kind":"private","conversation_id":"group_current","event_id":"current_event","body":"matching runtime"}`, runtime)
	if r.Code != 201 {
		t.Fatalf("current credential ingest status=%d body=%s", r.Code, r.Body.String())
	}
	conv := jsonPath(t, r, "conversation_id")
	for _, want := range []struct {
		table string
		count int
	}{{"operator_conversation", 1}, {"operator_message", 1}, {"operator_notification", 1}, {"chat_send_permit", 0}} {
		if count := scalarInt(t, s, "SELECT count(*) FROM core."+want.table+" WHERE workspace_id='ws_fixture'"); count != want.count {
			t.Errorf("accepted write table=%s count=%d want=%d", want.table, count, want.count)
		}
	}
	owner := conversationActor()
	owner.AccountIDs = []string{"account_a"}
	if g := conversationRequest(s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return owner, nil }), http.MethodGet, "/v1/operator/conversations/"+conv+"/messages", "", owner); g.Code != 200 {
		t.Errorf("owner scope messages read status=%d body=%s", g.Code, g.Body.String())
	}
}
func TestB7IndependentAuthRevocationLeavesNoWrites(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		grant     bool
	}{
		{"credential_revoked", `DELETE FROM core.runtime_credentials WHERE workspace_id='ws_fixture' AND runtime_id='runtime_a'`, true},
		{"permission_revoked", "", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := fixtureStore(t)
			seedAccount(t, s, "account_a", "runtime_a", 1)
			if scenario.sql != "" {
				if _, e := s.Pool.Exec(context.Background(), scenario.sql); e != nil {
					t.Fatal(e)
				}
			}
			actor := conversationActor()
			actor.RuntimeID = "runtime_a"
			actor.OwnerEpoch = 1
			actor.AccountIDs = []string{"account_a"}
			if scenario.grant {
				actor.Permissions["chat.trigger"] = true
			}
			h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return actor, nil })
			r := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_a","conversation_kind":"private","conversation_id":"group_revoked","event_id":"revoked_event","body":"revoked auth"}`, actor)
			if r.Code < 400 {
				t.Fatalf("revoked auth accepted status=%d body=%s", r.Code, r.Body.String())
			}
			for _, table := range []string{"operator_conversation", "operator_message", "operator_notification", "chat_send_permit"} {
				if count := scalarInt(t, s, "SELECT count(*) FROM core."+table+" WHERE workspace_id='ws_fixture'"); count != 0 {
					t.Errorf("denial side effect table=%s count=%d", table, count)
				}
			}
		})
	}
}
