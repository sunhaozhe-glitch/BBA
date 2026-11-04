package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

func b7IngestPost(h http.Handler, actor Actor, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/operator/qq-inbox/events", bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "fixture-request")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func b7Data(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	data, ok := doc["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data object in %s", w.Body.String())
	}
	return data
}

func b7ErrorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope %s: %v", w.Body.String(), err)
	}
	return envelope.Error.Code
}

func TestB7NotificationScopedEventIdentity(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7_a", "runtime_b7_a", 1)
	seedAccount(t, s, "account_b7_b", "runtime_b7_b", 1)
	runtimeA := conversationActor()
	runtimeA.RuntimeID = "runtime_b7_a"
	runtimeA.Permissions["chat.trigger"] = true
	runtimeA.AccountIDs = []string{"account_b7_a"}
	runtimeB := conversationActor()
	runtimeB.RuntimeID = "runtime_b7_b"
	runtimeB.Permissions["chat.trigger"] = true
	runtimeB.AccountIDs = []string{"account_b7_b"}
	runtime := runtimeA
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return runtime, nil })
	sameEventTwoAccounts := `{"account_id":"account_b7_b","conversation_kind":"group","conversation_id":"group_shared","event_id":"evt_shared","body":"shared text"}`
	first := b7IngestPost(h, runtime, `{"account_id":"account_b7_a","conversation_kind":"group","conversation_id":"group_shared","event_id":"evt_shared","body":"shared text"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("account one ingest status=%d body=%s", first.Code, first.Body.String())
	}
	runtime = runtimeB
	second := b7IngestPost(h, runtime, sameEventTwoAccounts)
	if second.Code != http.StatusCreated {
		t.Fatalf("account two ingest status=%d body=%s", second.Code, second.Body.String())
	}
	if jsonPath(t, second, "id") == jsonPath(t, first, "id") {
		t.Fatal("the same event id in another account must project its own message")
	}
	runtime = runtimeA
	third := b7IngestPost(h, runtime, `{"account_id":"account_b7_a","conversation_kind":"private","conversation_id":"user_shared","event_id":"evt_shared","body":"shared text"}`)
	if third.Code != http.StatusCreated {
		t.Fatalf("other conversation ingest status=%d body=%s", third.Code, third.Body.String())
	}
	if jsonPath(t, third, "id") == jsonPath(t, first, "id") {
		t.Fatal("the same event id in another conversation must project its own message")
	}
	replay := b7IngestPost(h, runtime, `{"account_id":"account_b7_a","conversation_kind":"group","conversation_id":"group_shared","event_id":"evt_shared","body":"shared text"}`)
	if replay.Code != http.StatusOK {
		t.Fatalf("same scoped event replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	if jsonPath(t, replay, "id") != jsonPath(t, first, "id") || jsonPath(t, replay, "conversation_id") != jsonPath(t, first, "conversation_id") {
		t.Fatalf("same scoped event replay must read back the original ids, got %s", replay.Body.String())
	}
	changed := b7IngestPost(h, runtime, `{"account_id":"account_b7_a","conversation_kind":"group","conversation_id":"group_shared","event_id":"evt_shared","body":"changed text"}`)
	if changed.Code != http.StatusConflict || b7ErrorCode(t, changed) != taskCodeIdempotency {
		t.Fatalf("changed digest status=%d code=%s", changed.Code, b7ErrorCode(t, changed))
	}
	for _, want := range []struct {
		query string
		id    string
		count int
	}{
		{query: `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='inbox:evt_shared'`, count: 3},
		{query: `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='inbox:evt_shared' AND account_id='account_b7_a' AND conversation_id=$1`, id: jsonPath(t, first, "conversation_id"), count: 1},
		{query: `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='inbox:evt_shared' AND account_id='account_b7_b' AND conversation_id=$1`, id: jsonPath(t, second, "conversation_id"), count: 1},
		{query: `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='inbox:evt_shared' AND conversation_id=$1`, id: jsonPath(t, third, "conversation_id"), count: 1},
		{query: `SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND event_id='evt_shared'`, count: 3},
		{query: `SELECT count(*) FROM core.chat_send_permit`, count: 0},
	} {
		var args []any
		if strings.Contains(want.query, "$1") {
			args = append(args, want.id)
		}
		if got := scalarInt(t, s, want.query, args...); got != want.count {
			t.Fatalf("%s %v = %d, want %d", want.query, args, got, want.count)
		}
	}
}

func TestB7NotificationKeyScopePreservesNonInboxKeys(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7_a", "runtime_b7_a", 1)
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return conversationActor(), nil })
	firstThread := conversationRequest(h, http.MethodPost, "/v1/operator/agent-threads/thread_b7_one/messages", `{"body":"first thread"}`, conversationActor())
	if firstThread.Code != http.StatusCreated {
		t.Fatalf("agent message status=%d body=%s", firstThread.Code, firstThread.Body.String())
	}
	secondThread := conversationRequest(h, http.MethodPost, "/v1/operator/agent-threads/thread_b7_two/messages", `{"body":"second thread"}`, conversationActor())
	if secondThread.Code != http.StatusCreated {
		t.Fatalf("second thread status=%d body=%s", secondThread.Code, secondThread.Body.String())
	}
	if jsonPath(t, secondThread, "id") == jsonPath(t, firstThread, "id") {
		t.Fatal("the second thread must get its own message")
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_type='agent.message'`: 2,
		`SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND source='agent'`:                  2,
		`SELECT count(*) FROM core.chat_send_permit`:                                                                     0,
	} {
		if got := scalarInt(t, s, query); got != want {
			t.Fatalf("%s = %d, want %d", query, got, want)
		}
	}
}

func TestB7NotificationConcurrentSameScopedEventProjectsOnce(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7_a", "runtime_b7_a", 1)
	runtime := conversationActor()
	runtime.RuntimeID = "runtime_b7_a"
	runtime.Permissions["chat.trigger"] = true
	runtime.AccountIDs = []string{"account_b7_a"}
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return runtime, nil })
	body := `{"account_id":"account_b7_a","conversation_kind":"group","conversation_id":"group_race_notify","event_id":"evt_race","body":"raced text"}`
	var responses [2]*httptest.ResponseRecorder
	var wg sync.WaitGroup
	for i := range responses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			responses[i] = b7IngestPost(h, runtime, body)
		}(i)
	}
	wg.Wait()
	created := 0
	for _, w := range responses {
		if w.Code == http.StatusCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("concurrent identical ingest created %d, want exactly one; statuses %d/%d", created, responses[0].Code, responses[1].Code)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND event_id='evt_race'`:             1,
		`SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='inbox:evt_race'`: 1,
		`SELECT count(*) FROM core.chat_send_permit`:                                                                     0,
	} {
		if got := scalarInt(t, s, query); got != want {
			t.Fatalf("%s = %d, want %d", query, got, want)
		}
	}
}

// TestB7NotificationMigrate25PreservesRows is the additive-upgrade regression for
// the previous head: a genuine populated schema-25 database keeps every historical
// notification row verbatim across the upgrade and its idempotent re-run. The
// scoped event identity only changes which notifications may coexist afterwards.
func TestB7NotificationMigrate25PreservesRows(t *testing.T) {
	ctx := context.Background()
	s := b7OldSchemaStore(t)
	var err error
	if _, err = s.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES('ws_fixture');
		INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','principal_fixture');
		INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id) VALUES('ws_fixture','account_b7_c','rt_b7_c','rt_b7_c','100003'),('ws_fixture','account_b7_d','rt_b7_d','rt_b7_d','100004');
		INSERT INTO core.operator_conversation(workspace_id,id,kind,account_id,conversation_kind,conversation_id) VALUES
			('ws_fixture','conv_b7_hist_qq','qq','account_b7_c','group','hist_group'),('ws_fixture','conv_b7_hist_agent','agent',NULL,'agent','thread_b7_hist'),('ws_fixture','conv_b7_hist_bare','qq','account_b7_d','private','hist_bare');
		INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,account_id,conversation_id,summary,evidence_ref) VALUES
			('ws_fixture','notice_b7_hist_inbox','inbox:evt_hist','qq.message.inbound','obj_b7_1','account_b7_c','conv_b7_hist_qq','QQ inbox event received','sha256:hist'),
			('ws_fixture','notice_b7_hist_agent','agent-message:thread_b7_hist:key_hist','agent.message','obj_b7_2',NULL,'conv_b7_hist_agent','Agent thread message',''),
			('ws_fixture','notice_b7_hist_bare','draft:draft_b7_hist','qq.draft.created','obj_b7_3',NULL,NULL,'QQ draft review: draft','')`); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var raw string
		if err := s.Pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY (to_jsonb(x))::text),'[]'::jsonb)::text FROM core.operator_notification x`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	for i := 0; i < 2; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if snapshot() != before {
			t.Fatal("upgrade changed historical notification rows")
		}
	}
	if n := scalarInt(t, s, `SELECT max(version) FROM core.schema_version`); n != migrationHead {
		t.Fatalf("head %d, want %d", n, migrationHead)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM pg_constraint WHERE conname='operator_notification_workspace_id_event_key_key'`); n != 0 {
		t.Fatalf("old workspace-wide key still present: %d", n)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM pg_indexes WHERE schemaname='core' AND indexname='operator_notification_event_scope'`); n != 1 {
		t.Fatalf("scoped event index missing: %d", n)
	}
	if _, err = s.Pool.Exec(ctx, `INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,account_id,conversation_id,summary) VALUES ('ws_fixture','notice_b7_new_a','inbox:evt_second','qq.message.inbound','obj_b7_4','account_b7_c','conv_b7_hist_qq','second scope'),('ws_fixture','notice_b7_new_b','inbox:evt_second','qq.message.inbound','obj_b7_5','account_b7_d','conv_b7_hist_bare','second scope') ON CONFLICT(workspace_id,COALESCE(conversation_id,''),event_key) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='inbox:evt_second'`); n != 2 {
		t.Fatalf("one event key across two scopes produced %d rows, want one per scope", n)
	}
	if _, err = s.Pool.Exec(ctx, `INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,account_id,conversation_id,summary) VALUES ('ws_fixture','notice_b7_dup','inbox:evt_second','qq.message.inbound','obj_b7_6','account_b7_d','conv_b7_hist_bare','second scope') ON CONFLICT(workspace_id,COALESCE(conversation_id,''),event_key) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='inbox:evt_second'`); n != 2 {
		t.Fatalf("same scope and key produced %d rows, want the duplicate dropped", n)
	}
	t.Logf("real 25->%d: historical rows retained, scoped identity live, same-scope dedup preserved", migrationHead)
}

// b7OldSchemaStore builds a real database at the previous schema head -- the
// whole migration list minus the newest entry -- so both the additive upgrade and
// the old-schema boundary are measured against a database that never saw the
// scoped index.
func b7OldSchemaStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(os.Getenv("BBA_FIXTURE_DSN"))
	if err != nil || os.Getenv("BBA_FIXTURE_ONLY") != "1" || cfg.Host != "/fixture" || cfg.Database != "bba_fixture_admin" || cfg.User != "bba_fixture" {
		t.Fatal("isolated fixture required")
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	name := newID("b7_old25")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(os.Getenv("BBA_FIXTURE_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	old := append([]string{"0001_core.sql"}, migrationTail[:len(migrationTail)-1]...)
	for _, file := range old {
		sql, err := migrationFiles.ReadFile("migrations/" + file)
		if err != nil {
			t.Fatal(file, err)
		}
		if _, err = s.Pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(file, err)
		}
	}
	if n := scalarInt(t, s, `SELECT max(version) FROM core.schema_version`); n != migrationHead-1 {
		t.Fatalf("built old head %d, want %d", n, migrationHead-1)
	}
	return s
}

// TestB7DraftInjectedNotificationFailureRollsBack injects a notification insert
// failure through the isolated fixture database: the create must fail as a store
// error and roll back the whole transaction, so a failed notification is never
// swallowed by the idempotency write that follows it.
func TestB7DraftInjectedNotificationFailureRollsBack(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7", "runtime_b7", 1)
	if _, err := s.Pool.Exec(context.Background(), `CREATE FUNCTION core.b7_fail_notification() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'b7_injected_notification_failure'; END $$;
		CREATE TRIGGER b7_fail_notification BEFORE INSERT ON core.operator_notification FOR EACH ROW EXECUTE FUNCTION core.b7_fail_notification()`); err != nil {
		t.Fatal(err)
	}
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return conversationActor(), nil })
	fail := conversationRequest(h, http.MethodPost, "/v1/operator/qq-drafts", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_b7_inject","body":"draft","expected_version":1}`, conversationActor())
	if fail.Code != http.StatusInternalServerError || b7ErrorCode(t, fail) != taskCodeStoreUnavailable {
		t.Fatalf("injected notification failure status=%d code=%s", fail.Code, b7ErrorCode(t, fail))
	}
	if _, err := s.Pool.Exec(context.Background(), `DROP TRIGGER b7_fail_notification ON core.operator_notification`); err != nil {
		t.Fatal(err)
	}
	// The same create succeeds once the injected failure is gone, so the failed
	// request above rolled the whole transaction back instead of partially writing.
	control := conversationRequest(h, http.MethodPost, "/v1/operator/qq-drafts", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_b7_control","body":"draft","expected_version":1}`, conversationActor())
	if control.Code != http.StatusCreated {
		t.Fatalf("post-injection control status=%d body=%s", control.Code, control.Body.String())
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM core.operator_conversation WHERE workspace_id='ws_fixture' AND conversation_id='group_b7_inject'`:                                                                 0,
		`SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND conversation_id IN (SELECT id FROM core.operator_conversation WHERE conversation_id='group_b7_inject')`: 0,
		`SELECT count(*) FROM core.qq_draft WHERE workspace_id='ws_fixture' AND conversation_id IN (SELECT id FROM core.operator_conversation WHERE conversation_id='group_b7_inject')`:         0,
		`SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_type='qq.draft.created'`:                                                                     1,
		`SELECT count(*) FROM core.chat_send_permit`: 0,
	} {
		if got := scalarInt(t, s, query); got != want {
			t.Fatalf("%s = %d, want %d", query, got, want)
		}
	}
}

// TestB7NotificationNullConversationStaysWorkspaceScoped pins the half of the
// scope the migration deliberately collapses: a notification that carries no
// persisted conversation -- historical rows and events not bound to a conversation
// row -- still dedupes across the whole workspace, while another event key in that
// scope keeps its own row. Every handler path that writes a notification resolves a
// real conversation row, so this is the preserved fallback, not the common case.
func TestB7NotificationNullConversationStaysWorkspaceScoped(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7", "runtime_b7", 1)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,account_id,conversation_id,summary) VALUES
		('ws_fixture','notice_b7_null_a','draft:b7_null','qq.draft.created','obj_b7_a','account_b7',NULL,'null scope first'),
		('ws_fixture','notice_b7_null_b','draft:b7_null','qq.draft.created','obj_b7_b','account_b7',NULL,'null scope second')
		ON CONFLICT(workspace_id,COALESCE(conversation_id,''),event_key) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='draft:b7_null'`); n != 1 {
		t.Fatalf("same workspace, NULL conversation, same event key: %d rows, want one", n)
	}
	var survivor string
	if err := s.Pool.QueryRow(ctx, `SELECT id FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='draft:b7_null'`).Scan(&survivor); err != nil {
		t.Fatal(err)
	}
	if survivor != "notice_b7_null_a" {
		t.Fatalf("the first insert must win the scope, survivor=%s", survivor)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,account_id,conversation_id,summary) VALUES ('ws_fixture','notice_b7_null_other','draft:b7_null_other','qq.draft.created','obj_b7_c','account_b7',NULL,'null scope other key') ON CONFLICT(workspace_id,COALESCE(conversation_id,''),event_key) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND conversation_id IS NULL`); n != 2 {
		t.Fatalf("two event keys in the workspace-wide NULL scope: %d rows, want two", n)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.chat_send_permit`); n != 0 {
		t.Fatalf("chat_send_permit = %d, want 0", n)
	}
}

// TestB7NotificationOldSchemaNeedsMigrationFirst records the deployment boundary
// for the previous schema head. The server startup gate stays at its minimum of
// 24, so this build still starts against a schema-25 database, and that database
// still satisfies the old workspace-wide conflict target the previous build emits.
// The scoped target this build emits is rejected there before any row is written,
// so this build and the old schema are not rolling-upgrade compatible and the
// database has to be migrated first. The Bridge is the other half of the boundary:
// its gate requires the exact head, so a migrated database no longer starts the
// previous Bridge -- migrate and restart both binaries together.
func TestB7NotificationOldSchemaNeedsMigrationFirst(t *testing.T) {
	s := b7OldSchemaStore(t)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES('ws_fixture');
		INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id) VALUES('ws_fixture','account_b7_old','rt_b7_old','rt_b7_old','100009')`); err != nil {
		t.Fatal(err)
	}
	old := `INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,account_id,conversation_id,summary) VALUES ('ws_fixture','notice_b7_old_a','inbox:evt_old','qq.message.inbound','obj_b7_old','account_b7_old',NULL,'old target') ON CONFLICT(workspace_id,event_key) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, old); err != nil {
		t.Fatalf("the previous binary path on the old schema must keep working: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, old); err != nil {
		t.Fatalf("the previous binary path replay on the old schema must keep working: %v", err)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='inbox:evt_old'`); n != 1 {
		t.Fatalf("old target on the old schema: %d rows, want the replay deduped to one", n)
	}
	scoped := `INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,account_id,conversation_id,summary) VALUES ('ws_fixture','notice_b7_old_b','inbox:evt_old','qq.message.inbound','obj_b7_old','account_b7_old',NULL,'new target') ON CONFLICT(workspace_id,COALESCE(conversation_id,''),event_key) DO NOTHING`
	_, err := s.Pool.Exec(ctx, scoped)
	var pgerr *pgconn.PgError
	if err == nil || sqlState(err, &pgerr) != "42P10" {
		t.Fatalf("the new conflict target on the old schema must fail with 42P10, got %v", err)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='inbox:evt_old'`); n != 1 {
		t.Fatalf("the rejected insert must write nothing, got %d rows", n)
	}
	t.Logf("previous head %d: old binary path works, this build scoped target fails 42P10 -- migrate before deploying", migrationHead-1)
}
