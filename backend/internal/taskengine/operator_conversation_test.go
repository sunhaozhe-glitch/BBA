package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func conversationActor() Actor {
	a := fixtureActor()
	a.OwnerEpoch = 1
	a.Permissions["conversation.read"] = true
	a.Permissions["conversation.write"] = true
	a.Permissions["notification.read"] = true
	return a
}

func conversationRequest(h http.Handler, method, path, body string, actor Actor) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/json")
	if method == http.MethodPost {
		r.Header.Set("Idempotency-Key", "fixture-request")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestConversationWorkspaceAccountIsolation(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7", "runtime_b7", 1)
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return conversationActor(), nil })
	w := conversationRequest(h, http.MethodPost, "/v1/operator/qq-drafts", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_1","body":"hello","expected_version":1}`, conversationActor())
	if w.Code != http.StatusCreated {
		t.Fatalf("draft status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			ID             string `json:"id"`
			ConversationID string `json:"conversation_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.ID == "" {
		t.Fatal("missing draft ID")
	}
	other := conversationActor()
	other.WorkspaceID = "ws_other"
	other.PrincipalID = "principal_other"
	other.Permissions["conversation.read"] = true
	otherHandler := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return other, nil })
	got := conversationRequest(otherHandler, http.MethodGet, "/v1/operator/qq-drafts/"+response.Data.ID, "", other)
	if got.Code != http.StatusForbidden && got.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace draft status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestDraftContentRuleAndPermitBoundary(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7", "runtime_b7", 1)
	_, err := s.Pool.Exec(context.Background(), `INSERT INTO core.content_revision(workspace_id,version) VALUES('ws_fixture',1); INSERT INTO core.content_rule(workspace_id,id,version,document) VALUES('ws_fixture','rule_block',1,'{"id":"rule_block","version":1,"name":"blocked phrase","kind":"keyword","expression":"blocked","enabled":true,"case_sensitive":false}')`)
	if err != nil {
		t.Fatal(err)
	}
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return conversationActor(), nil })
	w := conversationRequest(h, http.MethodPost, "/v1/operator/qq-drafts", `{"account_id":"account_b7","conversation_kind":"private","conversation_id":"qq_user_2","body":"blocked text","expected_version":1}`, conversationActor())
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var v struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	var decision string
	if err = s.Pool.QueryRow(context.Background(), `SELECT decision FROM core.qq_draft WHERE workspace_id='ws_fixture' AND id=$1`, v.Data.ID).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if decision != "blocked" {
		t.Fatalf("decision=%s", decision)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.chat_send_permit`); got != 0 {
		t.Fatalf("permits=%d", got)
	}
}

func TestNotificationProjectionIdempotencyAndReadCAS(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7", "runtime_b7", 1)
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return conversationActor(), nil })
	w := conversationRequest(h, http.MethodPost, "/v1/operator/qq-drafts", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_3","body":"draft","expected_version":1}`, conversationActor())
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var id string
	if err := s.Pool.QueryRow(context.Background(), `SELECT id FROM core.operator_notification WHERE workspace_id='ws_fixture'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("notification count=%d err=%v", count, err)
	}
	read := conversationRequest(h, http.MethodPost, "/v1/operator/notifications/"+id, `{"expected_version":1,"read":true}`, conversationActor())
	if read.Code != http.StatusOK {
		t.Fatalf("CAS status=%d body=%s", read.Code, read.Body.String())
	}
	stale := conversationRequest(h, http.MethodPost, "/v1/operator/notifications/"+id, `{"expected_version":1,"read":false}`, conversationActor())
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale CAS status=%d body=%s", stale.Code, stale.Body.String())
	}
}

func TestConversationSourceBoundary(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7", "runtime_b7", 1)
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return conversationActor(), nil })
	agent := conversationRequest(h, http.MethodPost, "/v1/operator/agent-threads/thread_boundary/messages", `{"body":"agent instruction"}`, conversationActor())
	if agent.Code != http.StatusCreated {
		t.Fatalf("agent message status=%d body=%s", agent.Code, agent.Body.String())
	}
	ctx := context.Background()
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND source='agent' AND direction<>'inbound'`); rows != 0 {
		t.Fatalf("agent inbound rows=%d, want none in QQ outbound", rows)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.chat_send_permit`); rows != 0 {
		t.Fatalf("agent thread messages must not create QQ permits, got %d", rows)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_conversation WHERE workspace_id='ws_fixture' AND kind='qq' AND conversation_id<>'agent' AND id IN (SELECT conversation_id FROM core.operator_message WHERE source='agent')`); rows != 0 {
		t.Fatalf("agent message landed in a QQ conversation (%d)", rows)
	}
	runtime := conversationActor()
	runtime.RuntimeID = "runtime_b7"
	runtime.Permissions["chat.trigger"] = true
	runtime.AccountIDs = []string{"account_b7"}
	r := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return runtime, nil })
	in := conversationRequest(r, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_boundary","event_id":"evt_boundary_1","body":"customer text"}`, runtime)
	if in.Code != http.StatusCreated {
		t.Fatalf("inbound event status=%d body=%s", in.Code, in.Body.String())
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND conversation_id='thread_boundary'`); rows != 1 {
		t.Fatalf("QQ inbound must not append to the agent thread, got %d rows", rows)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.chat_send_permit`); rows != 0 {
		t.Fatalf("QQ inbound must not create permits automatically, got %d", rows)
	}
	var source, direction, state string
	if err := s.Pool.QueryRow(ctx, `SELECT source,direction,delivery_state FROM core.operator_message WHERE workspace_id='ws_fixture' AND event_id='evt_boundary_1'`).Scan(&source, &direction, &state); err != nil {
		t.Fatal(err)
	}
	if source != "qq" || direction != "inbound" || state != "received" {
		t.Fatalf("inbound row marked %s/%s/%s", source, direction, state)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND source='agent' AND event_id='evt_boundary_1'`); rows != 0 {
		t.Fatalf("QQ inbound created %d agent instruction rows", rows)
	}
}

func TestInboxEventIdempotency(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7", "runtime_b7", 1)
	runtime := conversationActor()
	runtime.RuntimeID = "runtime_b7"
	runtime.Permissions["chat.trigger"] = true
	runtime.AccountIDs = []string{"account_b7"}
	r := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return runtime, nil })
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return conversationActor(), nil })
	operatorIn := conversationRequest(h, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_idem","event_id":"evt_x","body":"text one"}`, conversationActor())
	if operatorIn.Code != http.StatusForbidden {
		t.Fatalf("operator identity must not forge runtime ingest, got %d body=%s", operatorIn.Code, operatorIn.Body.String())
	}
	first := conversationRequest(r, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_idem","event_id":"evt_x","body":"text one"}`, runtime)
	if first.Code != http.StatusCreated {
		t.Fatalf("first ingest status=%d body=%s", first.Code, first.Body.String())
	}
	firstID := jsonPath(t, first, "id")
	replay := conversationRequest(r, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_idem","event_id":"evt_x","body":"text one"}`, runtime)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	if jsonPath(t, replay, "id") != firstID {
		t.Fatal("idempotent replay returned a different message id")
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND event_id='evt_x'`); rows != 1 {
		t.Fatalf("same event_id+digest replay produced %d rows", rows)
	}
	conflict := conversationRequest(r, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_idem","event_id":"evt_x","body":"changed body"}`, runtime)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same event_id different digest status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	late := conversationRequest(r, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_idem","event_id":"evt_y","body":"text two"}`, runtime)
	if late.Code != http.StatusCreated {
		t.Fatalf("out-of-order event status=%d body=%s", late.Code, late.Body.String())
	}
	convID := jsonPath(t, first, "conversation_id")
	list1 := conversationRequest(h, http.MethodGet, "/v1/operator/conversations/"+convID+"/messages", "", conversationActor())
	list2 := conversationRequest(h, http.MethodGet, "/v1/operator/conversations/"+convID+"/messages", "", conversationActor())
	if list1.Code != http.StatusOK || list2.Code != http.StatusOK {
		t.Fatalf("message read status=%d/%d", list1.Code, list2.Code)
	}
	if list1.Body.String() != list2.Body.String() {
		t.Fatal("stable read produced different order")
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_key='inbox:evt_x'`); rows != 1 {
		t.Fatalf("duplicate inbox events must project exactly one notification, got %d", rows)
	}
}

func jsonPath(t *testing.T, w *httptest.ResponseRecorder, key string) string {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	value, ok := envelope.Data[key].(string)
	if !ok || value == "" {
		t.Fatalf("missing data.%s in %s", key, w.Body.String())
	}
	return value
}

func TestConversationAndNotificationHTTPAuth(t *testing.T) {
	s := fixtureStore(t)
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return conversationActor(), nil })
	if got := conversationRequest(h, http.MethodGet, "/v1/operator/conversations?cursor=not-a-cursor", "", conversationActor()); got.Code != http.StatusBadRequest {
		t.Fatalf("cursor status=%d body=%s", got.Code, got.Body.String())
	}
	if got := conversationRequest(h, http.MethodPut, "/v1/operator/conversations", "", conversationActor()); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status=%d body=%s", got.Code, got.Body.String())
	}
	denied := s.OperatorConversationHandler(func(*http.Request) (Actor, error) {
		a := conversationActor()
		a.Permissions["conversation.read"] = false
		return a, nil
	})
	if got := conversationRequest(denied, http.MethodGet, "/v1/operator/conversations", "", conversationActor()); got.Code != http.StatusForbidden {
		t.Fatalf("auth status=%d body=%s", got.Code, got.Body.String())
	}
	unauth := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return Actor{}, problem("UNAUTHENTICATED") })
	if got := conversationRequest(unauth, http.MethodGet, "/v1/operator/conversations", "", Actor{}); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", got.Code)
	}
}

// TestUnscopedOperatorSeesSeededRows guards the real-service path: a resolver
// operator credential has no account scope, so pgx sends a NULL text[] for the
// account filter. The query must treat "no scope" as "see everything in the
// workspace" instead of silently filtering every row out.
func TestUnscopedOperatorSeesSeededRows(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7", "runtime_b7", 1)
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return conversationActor(), nil })
	draft := conversationRequest(h, http.MethodPost, "/v1/operator/qq-drafts", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_unscoped","body":"unscoped draft","expected_version":1}`, conversationActor())
	if draft.Code != http.StatusCreated {
		t.Fatalf("draft status=%d body=%s", draft.Code, draft.Body.String())
	}
	list := conversationRequest(h, http.MethodGet, "/v1/operator/conversations", "", conversationActor())
	if list.Code != http.StatusOK {
		t.Fatalf("conversation list status=%d body=%s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), "group_unscoped") {
		t.Fatalf("unscoped operator lost the conversation it just created: %s", list.Body.String())
	}
	notes := conversationRequest(h, http.MethodGet, "/v1/operator/notifications", "", conversationActor())
	if notes.Code != http.StatusOK {
		t.Fatalf("notification list status=%d body=%s", notes.Code, notes.Body.String())
	}
	if !strings.Contains(notes.Body.String(), "QQ draft review: draft") {
		t.Fatalf("unscoped operator lost the notification projection: %s", notes.Body.String())
	}
	draftID := jsonPath(t, draft, "id")
	got := conversationRequest(h, http.MethodGet, "/v1/operator/qq-drafts/"+draftID, "", conversationActor())
	if got.Code != http.StatusOK {
		t.Fatalf("unscoped draft read status=%d body=%s", got.Code, got.Body.String())
	}
}

// TestConversationAgentAppendOnExistingQQID pins the trust-domain boundary the
// existing source test leaves open: an agent thread id is attacker-controlled,
// so an operator must never be able to append a source=agent message onto a
// pre-existing QQ conversation by addressing its id directly. Validation of
// kind, account ownership and conversation identity happens server-side inside
// the transaction, the denial uses the established RESOURCE_NOT_FOUND code to
// avoid leaking the other domain, and a denied append leaves no message,
// notification or permit rows. Legitimate agent-thread creation, workspace
// scoping and runtime identity refusal stay intact.
func TestConversationAgentAppendOnExistingQQID(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "account_b7", "runtime_b7", 1)
	h := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return conversationActor(), nil })
	scoped := conversationActor()
	scoped.AccountIDs = []string{"account_b7"}
	scopedHandler := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return scoped, nil })
	runtime := conversationActor()
	runtime.RuntimeID = "runtime_b7"
	runtime.Permissions["chat.trigger"] = true
	runtime.AccountIDs = []string{"account_b7"}
	runtimeHandler := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return runtime, nil })
	groupConv := jsonPath(t, conversationRequest(runtimeHandler, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_b7","conversation_kind":"group","conversation_id":"group_p0_boundary","event_id":"evt_p0_group","body":"group text"}`, runtime), "conversation_id")
	privateConv := jsonPath(t, conversationRequest(runtimeHandler, http.MethodPost, "/v1/operator/qq-inbox/events", `{"account_id":"account_b7","conversation_kind":"private","conversation_id":"qq_user_p0_boundary","event_id":"evt_p0_private","body":"private text"}`, runtime), "conversation_id")
	attackGroup := conversationRequest(h, http.MethodPost, "/v1/operator/agent-threads/"+groupConv+"/messages", `{"body":"agent append onto existing group"}`, conversationActor())
	if attackGroup.Code != http.StatusNotFound || !strings.Contains(attackGroup.Body.String(), "RESOURCE_NOT_FOUND") {
		t.Fatalf("existing QQ group id attack status=%d body=%s", attackGroup.Code, attackGroup.Body.String())
	}
	attackPrivate := conversationRequest(h, http.MethodPost, "/v1/operator/agent-threads/"+privateConv+"/messages", `{"body":"agent append onto existing private"}`, conversationActor())
	if attackPrivate.Code != http.StatusNotFound || !strings.Contains(attackPrivate.Body.String(), "RESOURCE_NOT_FOUND") {
		t.Fatalf("existing QQ private id attack status=%d body=%s", attackPrivate.Code, attackPrivate.Body.String())
	}
	attackScoped := conversationRequest(scopedHandler, http.MethodPost, "/v1/operator/agent-threads/"+groupConv+"/messages", `{"body":"scoped agent append attempt"}`, scoped)
	if attackScoped.Code != http.StatusNotFound || !strings.Contains(attackScoped.Body.String(), "RESOURCE_NOT_FOUND") {
		t.Fatalf("account-scoped operator append attempt status=%d body=%s", attackScoped.Code, attackScoped.Body.String())
	}
	runtimePost := conversationRequest(runtimeHandler, http.MethodPost, "/v1/operator/agent-threads/thread_p0_runtime/messages", `{"body":"runtime must not post agent messages"}`, runtime)
	if runtimePost.Code != http.StatusUnauthorized || !strings.Contains(runtimePost.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("runtime identity on agent thread path status=%d body=%s", runtimePost.Code, runtimePost.Body.String())
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_conversation WHERE workspace_id='ws_fixture' AND id='thread_p0_runtime'`); rows != 0 {
		t.Fatalf("refused runtime identity must leave no conversation, got %d", rows)
	}
	legit := conversationRequest(h, http.MethodPost, "/v1/operator/agent-threads/thread_p0_legit/messages", `{"body":"legitimate agent instruction"}`, conversationActor())
	if legit.Code != http.StatusCreated {
		t.Fatalf("legitimate agent thread creation status=%d body=%s", legit.Code, legit.Body.String())
	}
	legitScoped := conversationRequest(scopedHandler, http.MethodPost, "/v1/operator/agent-threads/thread_p0_scoped/messages", `{"body":"scoped legitimate agent instruction"}`, scoped)
	if legitScoped.Code != http.StatusCreated {
		t.Fatalf("account-scoped legitimate agent thread creation status=%d body=%s", legitScoped.Code, legitScoped.Body.String())
	}
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.workspace(id) VALUES('ws_p0_other'); INSERT INTO core.principal(workspace_id,id) VALUES('ws_p0_other','principal_p0_other'); INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id) VALUES('ws_p0_other','account_p0_other','runtime_p0_other','adapter_p0_other')`); err != nil {
		t.Fatal(err)
	}
	other := conversationActor()
	other.WorkspaceID = "ws_p0_other"
	other.PrincipalID = "principal_p0_other"
	otherHandler := s.OperatorConversationHandler(func(*http.Request) (Actor, error) { return other, nil })
	otherLegit := conversationRequest(otherHandler, http.MethodPost, "/v1/operator/agent-threads/thread_p0_legit/messages", `{"body":"other workspace may create its own thread"}`, other)
	if otherLegit.Code != http.StatusCreated {
		t.Fatalf("workspace isolation must keep creation legitimate, status=%d body=%s", otherLegit.Code, otherLegit.Body.String())
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_p0_other' AND conversation_id='thread_p0_legit'`); rows != 1 {
		t.Fatalf("cross-workspace creation lost its own message row, got %d", rows)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND conversation_id='thread_p0_legit'`); rows != 1 {
		t.Fatalf("cross-workspace write appended to the fixture workspace row, got %d", rows)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND conversation_id=$1 AND source='qq'`, groupConv); rows != 1 {
		t.Fatalf("QQ group conversation must hold only its inbound event, got %d rows", rows)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND conversation_id=$1 AND source='qq'`, privateConv); rows != 1 {
		t.Fatalf("QQ private conversation must hold only its inbound event, got %d rows", rows)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_message WHERE workspace_id='ws_fixture' AND source='agent'`); rows != 2 {
		t.Fatalf("denied appends must leave no agent message rows, got %d in the fixture workspace", rows)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_fixture' AND event_type='agent.message'`); rows != 2 {
		t.Fatalf("denied appends must leave no notification projections, got %d in the fixture workspace", rows)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.operator_notification WHERE workspace_id='ws_p0_other' AND event_type='agent.message'`); rows != 1 {
		t.Fatalf("the legitimate cross-workspace creation lost its notification, got %d", rows)
	}
	if rows := scalarInt(t, s, `SELECT count(*) FROM core.chat_send_permit`); rows != 0 {
		t.Fatalf("agent thread writes must not create QQ permits, got %d", rows)
	}
}
