package taskengine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// These exercise the production handler against PostgreSQL, never a writer.
func TestPrincipalEventScopeLifecycle(t *testing.T) {
	for _, draft := range []bool{false, true} {
		name := "agent"
		if draft {
			name = "draft"
		}
		t.Run(name, func(t *testing.T) {
			s := fixtureStore(t)
			a := conversationActor()
			a.AccountIDs = []string{"scope_account"}
			b := conversationActor()
			b.PrincipalID = "scope_second"
			b.AccountIDs = []string{"scope_account"}
			if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','scope_second')`); err != nil {
				t.Fatal(err)
			}
			path := "/v1/operator/agent-threads/scope_thread/messages"
			body := `{"body":"synthetic scope text"}`
			changed := `{"body":"changed synthetic scope text"}`
			if draft {
				seedAccount(t, s, "scope_account", "scope_runtime", 1)
				path = "/v1/operator/qq-drafts"
				body = `{"account_id":"scope_account","conversation_kind":"private","conversation_id":"scope_chat","body":"synthetic scope text","expected_version":1}`
				changed = `{"account_id":"scope_account","conversation_kind":"private","conversation_id":"scope_chat","body":"changed synthetic scope text","expected_version":1}`
			}
			request := func(actor Actor, key, text string) *httptest.ResponseRecorder {
				h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return actor, nil })
				r := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(text))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Idempotency-Key", key)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			checkCounts := func(want int) {
				t.Helper()
				for _, table := range []string{"operator_message", "operator_notification", "idempotency"} {
					if n := scalarInt(t, s, `SELECT count(*) FROM core.`+table); n != want {
						t.Fatalf("%s=%d want %d", table, n, want)
					}
				}
				dw := 0
				if draft {
					dw = want
				}
				if n := scalarInt(t, s, `SELECT count(*) FROM core.qq_draft`); n != dw {
					t.Fatalf("draft=%d want %d", n, dw)
				}
				for _, table := range []string{"chat_send_permit", "dispatch_permit"} {
					if n := scalarInt(t, s, `SELECT count(*) FROM core.`+table); n != 0 {
						t.Fatalf("%s=%d", table, n)
					}
				}
			}
			first := request(a, "shared", body)
			if first.Code != 201 {
				t.Fatalf("first %d %s", first.Code, first.Body.String())
			}
			// Treat the first response as lost; a fresh handler must reproduce it exactly.
			replay := request(a, "shared", body)
			if replay.Code != 201 || replay.Body.String() != first.Body.String() {
				t.Fatalf("lost response replay %d %s", replay.Code, replay.Body.String())
			}
			conflict := request(a, "shared", changed)
			if conflict.Code != 409 || bytes.Contains(conflict.Body.Bytes(), []byte("synthetic")) {
				t.Fatalf("conflict %d %s", conflict.Code, conflict.Body.String())
			}
			checkCounts(1)
			if draft {
				for _, altered := range []string{
					strings.Replace(body, "scope_chat", "another_chat", 1),
					strings.Replace(body, "private", "group", 1),
				} {
					if got := request(a, "shared", altered); got.Code != 409 {
						t.Fatalf("changed resource reused ledger key: %d %s", got.Code, got.Body.String())
					}
				}
				checkCounts(1)
			}
			second := request(b, "shared", body)
			if second.Code != 201 || second.Body.String() == first.Body.String() {
				t.Fatalf("second principal %d %s", second.Code, second.Body.String())
			}
			for _, v := range []struct {
				a Actor
				w *httptest.ResponseRecorder
			}{{a, first}, {b, second}} {
				got := request(v.a, "shared", body)
				if got.Code != 201 || got.Body.String() != v.w.Body.String() {
					t.Fatalf("principal replay %d %s", got.Code, got.Body.String())
				}
			}
			checkCounts(2)
			denied := conversationActor()
			denied.Permissions["conversation.write"] = false
			if got := request(denied, "shared", body); got.Code != 403 {
				t.Fatalf("revoked permission replay %d", got.Code)
			}
			if draft {
				denied = a
				denied.AccountIDs = []string{"another_account"}
				if got := request(denied, "shared", body); got.Code != 404 {
					t.Fatalf("account denied replay %d", got.Code)
				}
			}
			// Two principals enter the same conversation concurrently with a new key.
			start := make(chan struct{})
			results := make(chan *httptest.ResponseRecorder, 2)
			var wg sync.WaitGroup
			for _, actor := range []Actor{a, b} {
				wg.Add(1)
				go func(actor Actor) { defer wg.Done(); <-start; results <- request(actor, "concurrent", body) }(actor)
			}
			close(start)
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("concurrent requests timed out")
			}
			close(results)
			ids := map[string]bool{}
			for got := range results {
				if got.Code != 201 {
					t.Fatalf("concurrent %d %s", got.Code, got.Body.String())
				}
				id := jsonPath(t, got, "id")
				if ids[id] {
					t.Fatal("principals shared resource")
				}
				ids[id] = true
			}
			checkCounts(4)
			if n := scalarInt(t, s, `SELECT count(DISTINCT event_id) FROM core.operator_message`); n != 4 {
				t.Fatalf("distinct event identities=%d", n)
			}
		})
	}
}
