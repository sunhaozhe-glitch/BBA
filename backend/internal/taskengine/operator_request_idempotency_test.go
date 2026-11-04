package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func operatorRequest(t *testing.T, s *Store, a Actor, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return a, nil })
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func operatorCase(t *testing.T, draft bool) (*Store, Actor, string, string) {
	t.Helper()
	s := fixtureStore(t)
	a := conversationActor()
	if !draft {
		return s, a, "/v1/operator/agent-threads/request_thread/messages", `{"body":"request text"}`
	}
	seedAccount(t, s, "request_account", "request_runtime", 1)
	return s, a, "/v1/operator/qq-drafts", `{"account_id":"request_account","conversation_kind":"private","conversation_id":"request_chat","body":"request text","expected_version":1}`
}

func operatorNoEffects(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range []string{"chat_send_permit", "dispatch_permit", "task", "model_request"} {
		if n := scalarInt(t, s, `SELECT count(*) FROM core.`+table); n != 0 {
			t.Fatalf("unexpected %s=%d", table, n)
		}
	}
}

func operatorCounts(t *testing.T, s *Store, messages, drafts, notices, ledger int) {
	t.Helper()
	for table, want := range map[string]int{"operator_message": messages, "qq_draft": drafts, "operator_notification": notices, "idempotency": ledger} {
		if got := scalarInt(t, s, `SELECT count(*) FROM core.`+table); got != want {
			t.Fatalf("%s=%d want %d", table, got, want)
		}
	}
	operatorNoEffects(t, s)
}

func TestOperatorRequestCanonicalPath(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprint(draft), func(t *testing.T) {
			s, a, path, body := operatorCase(t, draft)
			for _, stage := range []string{"before", "after"} {
				got := operatorRequest(t, s, a, path+"/", "canonical", body)
				if got.Code != 405 {
					t.Fatalf("%s alias accepted: %d %s", stage, got.Code, got.Body.String())
				}
				n, d := 0, 0
				if stage == "after" {
					n = 1
					if draft {
						d = 1
					}
				}
				operatorCounts(t, s, n, d, n, n)
				if stage == "before" {
					if w := operatorRequest(t, s, a, path, "canonical", body); w.Code != 201 {
						t.Fatalf("canonical: %d %s", w.Code, w.Body.String())
					}
				}
			}
			if n := scalarInt(t, s, `SELECT count(*) FROM core.idempotency WHERE path=$1`, path); n != 1 {
				t.Fatal("ledger path not canonical")
			}
		})
	}
}

func TestOperatorRequestConcurrentSamePrincipal(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprint(draft), func(t *testing.T) {
			s, a, path, body := operatorCase(t, draft)
			const workers = 6
			for _, same := range []bool{true, false} {
				start := make(chan struct{})
				out := make(chan *httptest.ResponseRecorder, workers)
				for i := 0; i < workers; i++ {
					key := "same"
					if !same {
						key = fmt.Sprintf("key_%d", i)
					}
					go func(key string) { <-start; out <- operatorRequest(t, s, a, path, key, body) }(key)
				}
				close(start)
				first := ""
				for i := 0; i < workers; i++ {
					select {
					case w := <-out:
						if w.Code != 201 {
							t.Fatalf("concurrent: %d %s", w.Code, w.Body.String())
						}
						if same && first != "" && first != w.Body.String() {
							t.Fatal("concurrent replay changed response")
						}
						first = w.Body.String()
					case <-time.After(15 * time.Second):
						t.Fatal("concurrent request timeout")
					}
				}
				n := 1
				if !same {
					n += workers
				}
				d := 0
				if draft {
					d = n
				}
				operatorCounts(t, s, n, d, n, n)
				if got := scalarInt(t, s, `SELECT count(DISTINCT seq) FROM core.operator_message`); got != n {
					t.Fatalf("seq distinct=%d want=%d", got, n)
				}
			}
		})
	}
}

func TestOperatorRequestDraftAllFields(t *testing.T) {
	s, a, path, body := operatorCase(t, true)
	seedAccount(t, s, "another_account", "another_runtime", 1)
	if w := operatorRequest(t, s, a, path, "fields", body); w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	for _, changed := range []string{
		strings.Replace(body, "request_account", "another_account", 1),
		strings.Replace(body, "request_chat", "another_chat", 1),
		strings.Replace(body, "private", "group", 1),
		strings.Replace(body, "request text", "changed text", 1),
	} {
		if w := operatorRequest(t, s, a, path, "fields", changed); w.Code != 409 {
			t.Fatalf("changed effective field: %d %s", w.Code, w.Body.String())
		}
	}
	if w := operatorRequest(t, s, a, path, "fields", strings.Replace(body, `"expected_version":1`, `"expected_version":2`, 1)); w.Code != 400 {
		t.Fatalf("invalid version bypassed validation: %d", w.Code)
	}
	operatorCounts(t, s, 1, 1, 1, 1)
}

// Explicitly construct the pre-upgrade raw-key state. Never rewrite append-only messages.
func operatorLegacy(t *testing.T, s *Store, a Actor, draft bool, key, body string) (string, string, map[string]any) {
	t.Helper()
	ctx := context.Background()
	conv := "request_thread"
	dig := ChatTextDigest("request text")
	resource := "legacy_msg"
	var response map[string]any
	if !draft {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO core.operator_conversation(workspace_id,id,kind,conversation_kind,conversation_id) VALUES($1,$2,'agent','agent','legacy_display')`, a.WorkspaceID, conv); err != nil {
			t.Fatal(err)
		}
		response = map[string]any{"data": map[string]any{"id": resource, "conversation_id": conv, "source": "agent", "direction": "inbound", "delivery_state": "received", "body_digest": dig, "send_permit": "absent"}}
	} else {
		conv = "legacy_conv"
		if _, err := s.Pool.Exec(ctx, `INSERT INTO core.operator_conversation(workspace_id,id,kind,account_id,conversation_kind,conversation_id) VALUES($1,$2,'qq','request_account','private','request_chat')`, a.WorkspaceID, conv); err != nil {
			t.Fatal(err)
		}
		resource = "legacy_draft"
		// Same field order and types as the historical valid request struct.
		var req struct {
			AccountID        string `json:"account_id"`
			ConversationKind string `json:"conversation_kind"`
			ConversationID   string `json:"conversation_id"`
			Body             string `json:"body"`
			ExpectedVersion  int64  `json:"expected_version"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(req)
		dig = ChatTextDigest(string(payload))
		response = map[string]any{"data": map[string]any{"id": resource, "message_id": "legacy_msg", "payload_digest": ChatTextDigest("request text"), "rules_version": int64(9007199254740993), "decision": "draft", "evidence": map[string]any{"rules_version": int64(9007199254740993)}, "send_permit": "absent", "version": 1}}
	}
	source, direction, state := "agent", "inbound", "received"
	if draft {
		source, direction, state = "qq", "outbound_draft", "draft"
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO core.operator_message(workspace_id,id,conversation_id,source,direction,delivery_state,body,body_digest,event_id,seq) VALUES($1,'legacy_msg',$2,$3,$4,$5,'request text',$6,$7,1)`, a.WorkspaceID, conv, source, direction, state, ChatTextDigest("request text"), key); err != nil {
		t.Fatal(err)
	}
	if draft {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO core.qq_draft(workspace_id,id,conversation_id,account_id,message_id,payload_digest,rules_version,decision,evidence,created_by) VALUES($1,$2,$3,'request_account','legacy_msg',$4,9007199254740993,'draft','{}',$5)`, a.WorkspaceID, resource, conv, ChatTextDigest("request text"), a.PrincipalID); err != nil {
			t.Fatal(err)
		}
	}
	return dig, resource, response
}

func TestOperatorRequestLegacy(t *testing.T) {
	for _, draft := range []bool{false, true} {
		for _, ledgerPath := range []string{"none", "canonical", "alias"} {
			t.Run(fmt.Sprintf("%t/%s", draft, ledgerPath), func(t *testing.T) {
				s, a, path, body := operatorCase(t, draft)
				dig, resource, response := operatorLegacy(t, s, a, draft, "legacy", body)
				var before []byte
				if err := s.Pool.QueryRow(context.Background(), `SELECT to_jsonb(m)::text FROM core.operator_message m WHERE id='legacy_msg'`).Scan(&before); err != nil {
					t.Fatal(err)
				}
				ledger := 0
				if ledgerPath != "none" {
					ledger = 1
					p := path
					if ledgerPath == "alias" {
						p += "/"
					}
					if err := s.tx(context.Background(), func(tx pgx.Tx) error {
						return taskWriteIdempotency(context.Background(), tx, a.WorkspaceID, a.PrincipalID, http.MethodPost, p, "legacy", dig, resource, response)
					}); err != nil {
						t.Fatal(err)
					}
				}
				got := operatorRequest(t, s, a, path, "legacy", body)
				if ledger == 0 {
					if got.Code != 409 {
						t.Fatalf("unattributed legacy retry: %d %s", got.Code, got.Body.String())
					}
					if draft {
						for _, altered := range []string{strings.Replace(body, "request_chat", "other_target", 1), strings.Replace(body, "private", "group", 1)} {
							if w := operatorRequest(t, s, a, path, "legacy", altered); w.Code != 409 {
								t.Fatalf("legacy key escaped via target: %d %s", w.Code, w.Body.String())
							}
						}
					}
				} else {
					want, _ := json.Marshal(response)
					if got.Code != 201 || got.Body.String() != string(want)+"\n" {
						t.Fatalf("legacy exact replay: %d %s want %s", got.Code, got.Body.String(), want)
					}
					if w := operatorRequest(t, s, a, path, "legacy", strings.Replace(body, "request text", "changed text", 1)); w.Code != 409 {
						t.Fatalf("legacy changed request: %d", w.Code)
					}
				}
				d := 0
				if draft {
					d = 1
				}
				operatorCounts(t, s, 1, d, 0, ledger)
				var after []byte
				if err := s.Pool.QueryRow(context.Background(), `SELECT to_jsonb(m)::text FROM core.operator_message m WHERE id='legacy_msg'`).Scan(&after); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("legacy message mutated")
				}
			})
		}
	}
}

func TestOperatorRequestReplayGuards(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprint(draft), func(t *testing.T) {
			s, a, path, body := operatorCase(t, draft)
			first := operatorRequest(t, s, a, path, "guard", body)
			if first.Code != 201 {
				t.Fatal(first.Body.String())
			}
			for _, kind := range []string{"permission", "principal", "workspace", "runtime", "auth", "source"} {
				denied := conversationActor()
				status := 403
				authErr := error(nil)
				switch kind {
				case "permission":
					denied.Permissions["conversation.write"] = false
				case "principal":
					denied.PrincipalID = "missing_principal"
				case "workspace":
					denied.WorkspaceID = "missing_workspace"
				case "runtime":
					denied.RuntimeID = "runtime"
					status = 401
				case "auth":
					authErr = errors.New("revoked token")
					status = 401
				case "source":
					status = 404
					if draft {
						denied.AccountIDs = []string{"not_allowed"}
					} else {
						if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id) VALUES('ws_fixture','guard_account','guard_runtime','guard_adapter'); UPDATE core.operator_conversation SET kind='qq',conversation_kind='private',account_id='guard_account' WHERE workspace_id='ws_fixture' AND id='request_thread'`); err != nil {
							t.Fatal(err)
						}
					}
				}
				h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return denied, authErr })
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Idempotency-Key", "guard")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != status || bytes.Contains(w.Body.Bytes(), []byte(jsonPath(t, first, "id"))) {
					t.Fatalf("%s replay guard: %d %s", kind, w.Code, w.Body.String())
				}
			}
			d := 0
			if draft {
				d = 1
			}
			operatorCounts(t, s, 1, d, 1, 1)
		})
	}
}

func TestOperatorRequestAccountBeforeReplay(t *testing.T) {
	s, a, path, body := operatorCase(t, true)
	// A ledger is not proof that the target account still exists in this workspace.
	var req struct {
		AccountID        string `json:"account_id"`
		ConversationKind string `json:"conversation_kind"`
		ConversationID   string `json:"conversation_id"`
		Body             string `json:"body"`
		ExpectedVersion  int64  `json:"expected_version"`
	}
	body = strings.Replace(body, "request_account", "missing_account", 1)
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(req)
	if err := s.tx(context.Background(), func(tx pgx.Tx) error {
		return taskWriteIdempotency(context.Background(), tx, a.WorkspaceID, a.PrincipalID, http.MethodPost, path, "missing", ChatTextDigest(string(payload)), "old_draft", map[string]any{"data": map[string]any{"id": "old_draft"}})
	}); err != nil {
		t.Fatal(err)
	}
	if w := operatorRequest(t, s, a, path, "missing", body); w.Code != 404 {
		t.Fatalf("missing account replay: %d %s", w.Code, w.Body.String())
	}
	operatorCounts(t, s, 0, 0, 0, 1)
}

func TestOperatorRequestRollback(t *testing.T) {
	for _, draft := range []bool{false, true} {
		for _, failTable := range []string{"operator_notification", "idempotency"} {
			t.Run(fmt.Sprintf("%t/%s", draft, failTable), func(t *testing.T) {
				s, a, path, body := operatorCase(t, draft)
				if _, err := s.Pool.Exec(context.Background(), `CREATE FUNCTION core.reject_operator_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture late failure' USING ERRCODE='23514'; END $$; CREATE TRIGGER reject_operator_test BEFORE INSERT ON core.`+failTable+` FOR EACH ROW EXECUTE FUNCTION core.reject_operator_test()`); err != nil {
					t.Fatal(err)
				}
				if w := operatorRequest(t, s, a, path, "rollback", body); w.Code < 400 {
					t.Fatalf("failure committed: %d %s", w.Code, w.Body.String())
				}
				operatorCounts(t, s, 0, 0, 0, 0)
				if n := scalarInt(t, s, `SELECT count(*) FROM core.operator_conversation`); n != 0 {
					t.Fatalf("conversation leaked: %d", n)
				}
				if _, err := s.Pool.Exec(context.Background(), `DROP TRIGGER reject_operator_test ON core.`+failTable); err != nil {
					t.Fatal(err)
				}
				first := operatorRequest(t, s, a, path, "rollback", body)
				if first.Code != 201 {
					t.Fatalf("failed request occupied key: %d %s", first.Code, first.Body.String())
				}
				if w := operatorRequest(t, s, a, path, "rollback", body); w.Code != 201 || w.Body.String() != first.Body.String() {
					t.Fatalf("retry replay: %d %s", w.Code, w.Body.String())
				}
				d := 0
				if draft {
					d = 1
				}
				operatorCounts(t, s, 1, d, 1, 1)
			})
		}
	}
}

func TestOperatorRequestCrossPrincipalSameKey(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprint(draft), func(t *testing.T) {
			s, a, path, body := operatorCase(t, draft)
			b := conversationActor()
			b.PrincipalID = "second_principal"
			if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.principal(workspace_id,id) VALUES($1,$2)`, b.WorkspaceID, b.PrincipalID); err != nil {
				t.Fatal(err)
			}
			first := operatorRequest(t, s, a, path, "shared", body)
			second := operatorRequest(t, s, b, path, "shared", body)
			if first.Code != 201 || second.Code != 201 || first.Body.String() == second.Body.String() {
				t.Fatalf("principal collision: first=%d second=%d %s", first.Code, second.Code, second.Body.String())
			}
			d := 0
			if draft {
				d = 2
			}
			operatorCounts(t, s, 2, d, 2, 2)
		})
	}
}

func TestOperatorRequestFreshLargeNumber(t *testing.T) {
	s, a, path, body := operatorCase(t, true)
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.content_revision(workspace_id,version) VALUES('ws_fixture',9007199254740993)`); err != nil {
		t.Fatal(err)
	}
	first := operatorRequest(t, s, a, path, "large", body)
	if first.Code != 201 || !strings.Contains(first.Body.String(), `"rules_version":9007199254740993`) {
		t.Fatalf("first version: %d %s", first.Code, first.Body.String())
	}
	// Rule changes must not reinterpret a committed review during replay.
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.content_revision SET version=9007199254740994 WHERE workspace_id='ws_fixture'`); err != nil {
		t.Fatal(err)
	}
	if w := operatorRequest(t, s, a, path, "large", body); w.Code != 201 || w.Body.String() != first.Body.String() {
		t.Fatalf("number/review replay: %d %s", w.Code, w.Body.String())
	}
	operatorCounts(t, s, 1, 1, 1, 1)
}
