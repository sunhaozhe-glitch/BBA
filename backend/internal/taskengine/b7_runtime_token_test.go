package taskengine

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Snapshot every persistent user table, including row contents, so a denied
// fresh request or replay cannot hide an update or a write in another domain.
func b7PersistentSnapshot(t *testing.T, s *Store) map[string]string {
	t.Helper()
	return b7SnapshotQuery(t, s.Pool)
}

func b7SnapshotQuery(t *testing.T, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := q.Query(ctx, `SELECT schemaname,tablename FROM pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema') ORDER BY 1,2`)
	if err != nil {
		t.Fatal(err)
	}
	var tables [][2]string
	for rows.Next() {
		var table [2]string
		if err := rows.Scan(&table[0], &table[1]); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	snapshot := make(map[string]string, len(tables))
	for _, table := range tables {
		name := pgx.Identifier{table[0], table[1]}.Sanitize()
		var data string
		if err := q.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text),'[]'::jsonb)::text FROM `+name+` r`).Scan(&data); err != nil {
			t.Fatal(err)
		}
		snapshot[name] = data
	}
	return snapshot
}

func TestB7RuntimeTokenFreshAndReplay(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		sql      string
		alter    func(*Actor)
		account  string
		accepted bool
	}{
		{name: "current_token", accepted: true},
		{name: "empty_account", account: ""},
		{name: "missing_account", account: "account_missing", alter: func(a *Actor) { a.AccountIDs = []string{"account_missing"} }},
		{name: "empty_scope", alter: func(a *Actor) { a.AccountIDs = nil }},
		{name: "wrong_account_scope", alter: func(a *Actor) { a.AccountIDs = []string{"account_b"} }},
		{name: "cross_workspace", sql: `INSERT INTO core.workspace(id) VALUES('ws_other'); INSERT INTO core.principal(workspace_id,id) VALUES('ws_other','principal_fixture')`, alter: func(a *Actor) { a.WorkspaceID = "ws_other" }},
		{name: "wrong_runtime", alter: func(a *Actor) { a.RuntimeID = "runtime_other" }},
		{name: "missing_token_epoch", alter: func(a *Actor) { a.OwnerEpoch = 0 }},
		{name: "negative_token_epoch", alter: func(a *Actor) { a.OwnerEpoch = -1 }},
		{name: "wrong_token_epoch", alter: func(a *Actor) { a.OwnerEpoch = 2 }},
		{name: "credential_deleted", sql: `DELETE FROM core.runtime_credentials WHERE workspace_id='ws_fixture' AND account_id='account_a'`},
		{name: "credential_inactive", sql: `UPDATE core.runtime_credentials SET active=false WHERE workspace_id='ws_fixture' AND account_id='account_a'`},
		{name: "account_epoch_changed", sql: `UPDATE core.account SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND id='account_a'`},
		{name: "credential_epoch_changed", sql: `UPDATE core.runtime_credentials SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND account_id='account_a'`},
		{name: "both_epochs_rotated_old_token", sql: `UPDATE core.account SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND id='account_a'; UPDATE core.runtime_credentials SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND account_id='account_a'`},
		{name: "both_epochs_rotated_new_token", sql: `UPDATE core.account SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND id='account_a'; UPDATE core.runtime_credentials SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND account_id='account_a'`, alter: func(a *Actor) { a.OwnerEpoch = 2 }, accepted: true},
		{name: "owner_changed_old_credential_retained", sql: `UPDATE core.account SET runtime_id='runtime_new' WHERE workspace_id='ws_fixture' AND id='account_a'`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for _, replay := range []bool{false, true} {
				t.Run(fmt.Sprintf("replay=%t", replay), func(t *testing.T) {
					s := fixtureStore(t)
					seedAccount(t, s, "account_a", "runtime_a", 1)
					a := conversationRuntimeActor("runtime_a", "account_a")
					h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return a, nil })
					body := func(account, event string) string {
						return fmt.Sprintf(`{"account_id":%q,"conversation_kind":"group","conversation_id":"token_boundary","event_id":%q,"body":"fixture message"}`, account, event)
					}
					var originalID string
					if replay {
						w := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", body("account_a", "event_a"), a)
						if w.Code != 201 {
							t.Fatalf("seed event: %d %s", w.Code, w.Body.String())
						}
						originalID = jsonPath(t, w, "id")
					}
					if scenario.sql != "" {
						// Match the operations protocol: account then credential in one transaction.
						if err := s.tx(context.Background(), func(tx pgx.Tx) error { _, err := tx.Exec(context.Background(), scenario.sql); return err }); err != nil {
							t.Fatal(err)
						}
					}
					if scenario.alter != nil {
						scenario.alter(&a)
					}
					account := "account_a"
					if scenario.account != "" || scenario.name == "empty_account" {
						account = scenario.account
					}
					before := b7PersistentSnapshot(t, s)
					w := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", body(account, "event_a"), a)
					if !scenario.accepted {
						want := http.StatusNotFound
						if scenario.name == "empty_account" {
							want = http.StatusBadRequest
						}
						if w.Code != want {
							t.Errorf("denial status=%d want=%d body=%s", w.Code, want, w.Body.String())
						}
						after := b7PersistentSnapshot(t, s)
						if !reflect.DeepEqual(before, after) {
							for table, data := range after {
								if data != before[table] {
									t.Errorf("denied request changed %s", table)
								}
							}
						}
						return
					}
					want := http.StatusCreated
					if replay {
						want = http.StatusOK
					}
					if w.Code != want {
						t.Fatalf("current token status=%d want=%d body=%s", w.Code, want, w.Body.String())
					}
					if replay {
						if jsonPath(t, w, "id") != originalID {
							t.Error("replay changed message identity")
						}
						if !reflect.DeepEqual(before, b7PersistentSnapshot(t, s)) {
							t.Error("valid replay changed persistent state")
						}
					}
					for _, table := range []string{"chat_send_permit", "dispatch_permit", "task", "execution"} {
						if count := scalarInt(t, s, "SELECT count(*) FROM core."+table); count != 0 {
							t.Errorf("inbound event created %s rows=%d", table, count)
						}
					}
				})
			}
		})
	}
}
