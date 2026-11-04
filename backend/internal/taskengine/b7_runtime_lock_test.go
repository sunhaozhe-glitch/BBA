package taskengine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestB7InboxWaitsForRuntimeMutation(t *testing.T) {
	for _, scenario := range []struct{ name, sql string }{
		{"owner", `UPDATE core.account SET runtime_id='runtime_new' WHERE workspace_id='ws_fixture' AND id='account_a'`},
		{"revocation", `UPDATE core.runtime_credentials SET active=false WHERE workspace_id='ws_fixture' AND account_id='account_a'`},
		{"credential_epoch", `UPDATE core.runtime_credentials SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND account_id='account_a'`},
		{"synchronized_rotation", `UPDATE core.account SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND id='account_a'; UPDATE core.runtime_credentials SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND account_id='account_a'`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := fixtureStore(t)
			seedAccount(t, s, "account_a", "runtime_a", 1)
			a := conversationRuntimeActor("runtime_a", "account_a")
			before := b7PersistentSnapshot(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			mutation, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer mutation.Rollback(context.Background())
			if _, err = mutation.Exec(ctx, scenario.sql); err != nil {
				t.Fatal(err)
			}
			// Capture the mutation's own expected rows, including the existing
			// runtime_credential_population trigger, before releasing the lock.
			expected := b7SnapshotQuery(t, mutation)
			blocker := int(mutation.Conn().PgConn().PID())
			h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return a, nil })
			req := httptest.NewRequest(http.MethodPost, "/v1/operator/qq-inbox/events", bytes.NewBufferString(`{"account_id":"account_a","conversation_kind":"group","conversation_id":"locked","event_id":"locked_event","body":"fixture"}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "locked_request")
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { w := httptest.NewRecorder(); h.ServeHTTP(w, req); done <- w }()
			// Wait for a real PostgreSQL lock dependency on the mutation transaction.
			// No guessed delay is used as evidence that the inbox request is blocked.
			hardWait(t, s, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1::int=ANY(pg_blocking_pids(pid)) AND query LIKE '%FOR SHARE%'`, blocker)
			select {
			case w := <-done:
				t.Fatalf("inbox completed before mutation commit: %d %s", w.Code, w.Body.String())
			default:
			}
			if !reflect.DeepEqual(before, b7PersistentSnapshot(t, s)) {
				t.Fatal("pending inbox/mutation changed committed state")
			}
			if err = mutation.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case w := <-done:
				if w.Code != http.StatusNotFound {
					t.Fatalf("inbox after mutation commit: %d %s", w.Code, w.Body.String())
				}
			case <-ctx.Done():
				t.Fatal("inbox did not finish after mutation commit", ctx.Err())
			}

			after := b7PersistentSnapshot(t, s)
			if !reflect.DeepEqual(expected, after) {
				for table, data := range after {
					if data != expected[table] {
						t.Errorf("rejected inbox changed %s beyond the committed mutation", table)
					}
				}
			}

			t.Logf("observed inbox waiting on mutation pid=%d; committed %s; inbox rejected without effects", blocker, scenario.name)
		})
	}
}
