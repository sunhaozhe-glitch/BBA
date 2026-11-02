package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/jackc/pgx/v5/pgxpool"
)

type chatTriggerEnvelope struct {
	Data ChatTrigger          `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

type chatContextEnvelope struct {
	Data ChatContext          `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

type chatPolicyEnvelope struct {
	Data ChatPolicy           `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

type chatLeaseEnvelope struct {
	Data GroupSpeakerLease    `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

func chatRuntimeAuth(actor RuntimeActor) func(*http.Request) (RuntimeActor, error) {
	return func(r *http.Request) (RuntimeActor, error) {
		if r.Header.Get("Authorization") != "Runtime chat-test-token" {
			return RuntimeActor{}, problem(taskCodeUnauthenticated)
		}
		return actor, nil
	}
}

func chatOperatorAuth(actor Actor) func(*http.Request) (Actor, error) {
	return func(r *http.Request) (Actor, error) {
		if r.Header.Get("Authorization") != "Operator chat-test-token" {
			return Actor{}, problem(taskCodeUnauthenticated)
		}
		return actor, nil
	}
}

func chatServe(t *testing.T, handler http.Handler, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func chatHeaders(key string) map[string]string {
	return map[string]string{
		"Authorization":   "Runtime chat-test-token",
		"Content-Type":    "application/json",
		"Idempotency-Key": key,
	}
}

func chatAdminHeaders(key string) map[string]string {
	return map[string]string{
		"Authorization":   "Operator chat-test-token",
		"Content-Type":    "application/json",
		"Idempotency-Key": key,
	}
}

func chatSelfID(t *testing.T, s *Store, accountID string) string {
	t.Helper()
	return fixtureString(t, s, `SELECT self_id FROM core.account WHERE workspace_id='ws_fixture' AND id=$1`, accountID)
}

func chatTriggerRequest(ref, account, sender string) ChatTriggerRequest {
	return ChatTriggerRequest{
		TriggerRef:       ref,
		AccountID:        account,
		ConversationKind: chatConversationGroup,
		ConversationID:   "900001",
		SourceEventID:    "evt_" + ref,
		SenderID:         sender,
		DeadlineAt:       time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond),
		SegmentCount:     2,
	}
}

func chatAdminActor() Actor {
	actor := fixtureActor()
	actor.Permissions["account.chat.manage"] = true
	actor.Permissions["group.binding.manage"] = true
	return actor
}

// TestChatUsageDayUsesUTC pins the daily budget key to UTC even when the
// database session TimeZone is on a different calendar day. current_date
// would charge or read the local day and let the limit reset at local
// midnight.
func TestChatUsageDayUsesUTC(t *testing.T) {
	s, actor, deadline := chatFixture(t, 5)
	ctx := context.Background()
	var pool *pgxpool.Pool
	var chosen, localDay, utcDay string
	for _, tz := range []string{"Etc/GMT+12", "Pacific/Kiritimati"} {
		cfg := s.Pool.Config().Copy()
		cfg.ConnConfig.RuntimeParams["timezone"] = tz
		candidate, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("timezone pool: %v", err)
		}
		if err := candidate.QueryRow(ctx, `SELECT current_date::text, (`+chatUsageDateExpr+`)::text`).Scan(&localDay, &utcDay); err != nil {
			candidate.Close()
			t.Fatalf("date setup: %v", err)
		}
		if localDay != utcDay {
			pool, chosen = candidate, tz
			break
		}
		candidate.Close()
	}
	if pool == nil {
		t.Fatal("could not place the session TimeZone on a different calendar day")
	}
	oldPool := s.Pool
	s.Pool = pool
	t.Cleanup(func() {
		pool.Close()
		s.Pool = oldPool
	})

	before := time.Now().UTC().Format("2006-01-02")
	req := chatPermitRequest("send_utc:0", "acc_chat", "trig_chat", "group", "424242", 0, 2, deadline)
	if _, err := s.IssueChatSendPermit(ctx, actor, "idem-utc", req); err != nil {
		t.Fatalf("issue: %v", err)
	}
	after := time.Now().UTC().Format("2006-01-02")
	var window time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT window_start FROM core.chat_send_usage
		WHERE workspace_id='ws_fixture' AND account_id='acc_chat'`).Scan(&window); err != nil {
		t.Fatalf("usage row: %v", err)
	}
	got := window.Format("2006-01-02")
	if got != before && got != after {
		t.Fatalf("usage day=%s, want UTC day %s/%s (local %s via %s)", got, before, after, localDay, chosen)
	}
}

// TestChatTriggerManagedSelfIDAndIdempotency is the consumer-visible trigger
// contract: a managed bot self-id can never start a reply, the same body under
// a new key replays, and every differing reuse is an explicit conflict.
func TestChatTriggerManagedSelfIDAndIdempotency(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_chat_a", "rt_chat_a", 5)
	seedAccount(t, s, "acc_chat_b", "rt_chat_b", 5)
	actorA := fixtureRuntime("acc_chat_a", "rt_chat_a")
	handler := s.ChatRuntimeHandler(chatRuntimeAuth(actorA))
	req := chatTriggerRequest("trg_chat_1", "acc_chat_a", "700001")

	first := chatServe(t, handler, http.MethodPost, chatTriggerPath, req, chatHeaders("chat-trig-key-1"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first trigger status=%d body=%s", first.Code, first.Body.String())
	}
	var created chatTriggerEnvelope
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data.TriggerRef != req.TriggerRef || created.Data.NextSegment != 0 || created.Data.State != "open" ||
		created.Data.SegmentCount != 2 || created.Data.CreatedAt.IsZero() {
		t.Fatalf("created trigger=%+v", created.Data)
	}

	replay := chatServe(t, handler, http.MethodPost, chatTriggerPath, req, chatHeaders("chat-trig-key-2"))
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	var replayed chatTriggerEnvelope
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Data.TriggerRef != created.Data.TriggerRef || !replayed.Data.CreatedAt.Equal(created.Data.CreatedAt) {
		t.Fatalf("replay changed trigger: %+v vs %+v", replayed.Data, created.Data)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.chat_trigger WHERE workspace_id='ws_fixture'`); got != 1 {
		t.Fatalf("trigger rows=%d", got)
	}

	changed := req
	changed.SegmentCount = 3
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, changed, chatHeaders("chat-trig-key-1")); recorder.Code != http.StatusConflict || hbErrorCode(t, recorder) != chatCodeTriggerConflict {
		t.Fatalf("key reuse status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	changed = req
	changed.DeadlineAt = req.DeadlineAt.Add(time.Second)
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, changed, chatHeaders("chat-trig-key-3")); recorder.Code != http.StatusConflict || hbErrorCode(t, recorder) != chatCodeTriggerConflict {
		t.Fatalf("trigger reuse status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	sameSource := req
	sameSource.TriggerRef = "trg_chat_2"
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, sameSource, chatHeaders("chat-trig-key-4")); recorder.Code != http.StatusConflict {
		t.Fatalf("source reuse status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	managed := chatTriggerRequest("trg_chat_managed", "acc_chat_a", chatSelfID(t, s, "acc_chat_b"))
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, managed, chatHeaders("chat-trig-key-5")); recorder.Code != http.StatusForbidden || hbErrorCode(t, recorder) != chatCodeManagedSelfID {
		t.Fatalf("managed sender status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	private := chatTriggerRequest("trg_chat_private", "acc_chat_a", "700001")
	private.ConversationKind = chatConversationPrivate
	private.ConversationID = chatSelfID(t, s, "acc_chat_b")
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, private, chatHeaders("chat-trig-key-6")); recorder.Code != http.StatusForbidden || hbErrorCode(t, recorder) != chatCodeManagedSelfID {
		t.Fatalf("managed private peer status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	crossAccount := chatTriggerRequest("trg_chat_cross", "acc_chat_b", "700001")
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, crossAccount, chatHeaders("chat-trig-key-7")); recorder.Code != http.StatusNotFound {
		t.Fatalf("cross account status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	expired := chatTriggerRequest("trg_chat_expired", "acc_chat_a", "700001")
	expired.DeadlineAt = time.Now().UTC().Add(-time.Second)
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, expired, chatHeaders("chat-trig-key-8")); recorder.Code != http.StatusConflict || hbErrorCode(t, recorder) != chatCodeTriggerExpired {
		t.Fatalf("expired status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestChatTriggerHTTPStrict keeps the wire boundary closed: missing key,
// unknown fields, missing nullable members, non-Z timestamps and wrong media
// type are all rejected before any trigger row exists.
func TestChatTriggerHTTPStrict(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_chat_strict", "rt_chat_strict", 5)
	handler := s.ChatRuntimeHandler(chatRuntimeAuth(fixtureRuntime("acc_chat_strict", "rt_chat_strict")))
	req := chatTriggerRequest("trg_chat_strict", "acc_chat_strict", "700001")

	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, req, map[string]string{
		"Authorization": "Runtime chat-test-token", "Content-Type": "application/json",
	}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing key status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var unknown map[string]any
	body, _ := json.Marshal(req)
	if err := json.Unmarshal(body, &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["unexpected"] = true
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, unknown, chatHeaders("chat-strict-1")); recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	delete(unknown, "unexpected")
	delete(unknown, "directed_account_id")
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, unknown, chatHeaders("chat-strict-2")); recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing directed_account_id status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	offset := req
	offset.DeadlineAt = time.Now().In(time.FixedZone("CST", 8*3600)).Add(time.Minute)
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, offset, chatHeaders("chat-strict-3")); recorder.Code != http.StatusBadRequest {
		t.Fatalf("offset deadline status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	headers := chatHeaders("chat-strict-4")
	headers["Content-Type"] = "text/plain"
	if recorder := chatServe(t, handler, http.MethodPost, chatTriggerPath, req, headers); recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("media status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.chat_trigger WHERE workspace_id='ws_fixture'`); got != 0 {
		t.Fatalf("strict failures created %d trigger rows", got)
	}
}

// TestChatAdminPolicyAndSpeakerLease covers trusted pause/budget and the
// group-active-speaker lease, including private-context epoch fallback.
func TestChatAdminPolicyAndSpeakerLease(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_chat_a", "rt_chat_a", 5)
	seedAccount(t, s, "acc_chat_b", "rt_chat_b", 5)
	admin := chatAdminActor()
	adminHandler := s.ChatAdminHandler(chatOperatorAuth(admin))
	runtimeHandler := s.ChatRuntimeHandler(chatRuntimeAuth(fixtureRuntime("acc_chat_a", "rt_chat_a")))

	policy := ChatPolicyRequest{Enabled: true, DailyLimit: 2}
	recorder := chatServe(t, adminHandler, http.MethodPut, chatPolicyPath("acc_chat_a"), policy, chatAdminHeaders("chat-policy-1"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("policy status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var policyEnvelope chatPolicyEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &policyEnvelope); err != nil {
		t.Fatal(err)
	}
	if !policyEnvelope.Data.Enabled || policyEnvelope.Data.DailyLimit != 2 || policyEnvelope.Data.Version != 1 {
		t.Fatalf("policy=%+v", policyEnvelope.Data)
	}
	replay := chatServe(t, adminHandler, http.MethodPut, chatPolicyPath("acc_chat_a"), policy, chatAdminHeaders("chat-policy-1"))
	if replay.Code != http.StatusOK {
		t.Fatalf("policy replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	var replayedPolicy chatPolicyEnvelope
	if err := json.Unmarshal(replay.Body.Bytes(), &replayedPolicy); err != nil {
		t.Fatal(err)
	}
	if replayedPolicy.Data.Version != 1 {
		t.Fatalf("policy replay advanced version to %d", replayedPolicy.Data.Version)
	}
	stale := policy
	expected := 0
	stale.ExpectedVersion = &expected
	if recorder := chatServe(t, adminHandler, http.MethodPut, chatPolicyPath("acc_chat_a"), stale, chatAdminHeaders("chat-policy-2")); recorder.Code != http.StatusConflict || hbErrorCode(t, recorder) != chatCodePolicyConflict {
		t.Fatalf("stale policy status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	next := policy
	expected = 1
	next.ExpectedVersion = &expected
	if recorder := chatServe(t, adminHandler, http.MethodPut, chatPolicyPath("acc_chat_a"), next, chatAdminHeaders("chat-policy-3")); recorder.Code != http.StatusOK {
		t.Fatalf("expected policy status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	lease := GroupSpeakerLeaseRequest{
		AccountID:      "acc_chat_a",
		BindingEpoch:   "1",
		LeaseExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
	}
	recorder = chatServe(t, adminHandler, http.MethodPut, chatSpeakerLeasePath("900001"), lease, chatAdminHeaders("chat-lease-1"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("lease status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var leaseEnvelope chatLeaseEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &leaseEnvelope); err != nil {
		t.Fatal(err)
	}
	if leaseEnvelope.Data.ActiveSpeakerAccountID != "acc_chat_a" || leaseEnvelope.Data.BindingEpoch != "1" ||
		leaseEnvelope.Data.Version != 1 || leaseEnvelope.Data.GroupID != "900001" {
		t.Fatalf("lease=%+v", leaseEnvelope.Data)
	}

	contextPath := chatContextPath + "?account_id=acc_chat_a&conversation_kind=group&conversation_id=900001"
	recorder = chatServe(t, runtimeHandler, http.MethodGet, contextPath, nil, map[string]string{"Authorization": "Runtime chat-test-token"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("context status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var contextEnvelope chatContextEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &contextEnvelope); err != nil {
		t.Fatal(err)
	}
	if !contextEnvelope.Data.SpeakerActive || !contextEnvelope.Data.ChatEnabled || contextEnvelope.Data.DailyLimit != 2 ||
		contextEnvelope.Data.BindingEpoch == nil || *contextEnvelope.Data.BindingEpoch != "1" ||
		contextEnvelope.Data.ActiveSpeakerAccountID == nil || *contextEnvelope.Data.ActiveSpeakerAccountID != "acc_chat_a" {
		t.Fatalf("context=%+v", contextEnvelope.Data)
	}
	if len(contextEnvelope.Data.ManagedSelfIDs) != 2 {
		t.Fatalf("managed self ids=%v", contextEnvelope.Data.ManagedSelfIDs)
	}

	// Non-speaker account sees the same active speaker but cannot send.
	nonSpeakerHandler := s.ChatRuntimeHandler(chatRuntimeAuth(fixtureRuntime("acc_chat_b", "rt_chat_b")))
	nonSpeakerPath := chatContextPath + "?account_id=acc_chat_b&conversation_kind=group&conversation_id=900001"
	recorder = chatServe(t, nonSpeakerHandler, http.MethodGet, nonSpeakerPath, nil, map[string]string{"Authorization": "Runtime chat-test-token"})
	var nonSpeaker chatContextEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &nonSpeaker); err != nil {
		t.Fatal(err)
	}
	if nonSpeaker.Data.SpeakerActive {
		t.Fatalf("non-speaker marked active: %+v", nonSpeaker.Data)
	}

	// Same-speaker lease renewal advances version but keeps the epoch.
	renew := lease
	expected = 1
	renew.ExpectedVersion = &expected
	renew.LeaseExpiresAt = lease.LeaseExpiresAt.Add(time.Hour)
	if recorder := chatServe(t, adminHandler, http.MethodPut, chatSpeakerLeasePath("900001"), renew, chatAdminHeaders("chat-lease-2")); recorder.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	// A speaker change requires a strictly higher epoch.
	switchSpeaker := lease
	switchSpeaker.AccountID = "acc_chat_b"
	expected = 2
	switchSpeaker.ExpectedVersion = &expected
	if recorder := chatServe(t, adminHandler, http.MethodPut, chatSpeakerLeasePath("900001"), switchSpeaker, chatAdminHeaders("chat-lease-3")); recorder.Code != http.StatusConflict || hbErrorCode(t, recorder) != chatCodeBindingConflict {
		t.Fatalf("same-epoch switch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	switchSpeaker.BindingEpoch = "2"
	expected = 2
	switchSpeaker.ExpectedVersion = &expected
	if recorder := chatServe(t, adminHandler, http.MethodPut, chatSpeakerLeasePath("900001"), switchSpeaker, chatAdminHeaders("chat-lease-4")); recorder.Code != http.StatusOK {
		t.Fatalf("epoch switch status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// Private context returns the owner epoch so a permit can echo it.
	privatePath := chatContextPath + "?account_id=acc_chat_a&conversation_kind=private&conversation_id=700001"
	recorder = chatServe(t, runtimeHandler, http.MethodGet, privatePath, nil, map[string]string{"Authorization": "Runtime chat-test-token"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("private context status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var privateContext chatContextEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &privateContext); err != nil {
		t.Fatal(err)
	}
	if !privateContext.Data.SpeakerActive || privateContext.Data.ActiveSpeakerAccountID != nil || privateContext.Data.LeaseExpiresAt != nil ||
		privateContext.Data.BindingEpoch == nil || *privateContext.Data.BindingEpoch != "1" {
		t.Fatalf("private context=%+v", privateContext.Data)
	}
}

// TestChatAdminAuthorizationAndIdempotency keeps the operator surface closed
// and prevents a reused key from silently applying a different budget.
func TestChatAdminAuthorizationAndIdempotency(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_chat_ops", "rt_chat_ops", 5)
	denied := Actor{WorkspaceID: "ws_fixture", PrincipalID: "principal_fixture", Permissions: map[string]bool{}}
	handler := s.ChatAdminHandler(chatOperatorAuth(denied))
	policy := ChatPolicyRequest{Enabled: true, DailyLimit: 3}
	if recorder := chatServe(t, handler, http.MethodPut, chatPolicyPath("acc_chat_ops"), policy, chatAdminHeaders("chat-ops-1")); recorder.Code != http.StatusForbidden || hbErrorCode(t, recorder) != taskCodeForbidden {
		t.Fatalf("denied status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	handler = s.ChatAdminHandler(chatOperatorAuth(chatAdminActor()))
	if recorder := chatServe(t, handler, http.MethodPut, chatPolicyPath("acc_chat_ops"), policy, chatAdminHeaders("chat-ops-2")); recorder.Code != http.StatusOK {
		t.Fatalf("allowed status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	other := policy
	other.DailyLimit = 4
	if recorder := chatServe(t, handler, http.MethodPut, chatPolicyPath("acc_chat_ops"), other, chatAdminHeaders("chat-ops-2")); recorder.Code != http.StatusConflict || hbErrorCode(t, recorder) != taskCodeIdempotency {
		t.Fatalf("key conflict status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, handler, http.MethodPut, chatPolicyPath("missing_account"), policy, chatAdminHeaders("chat-ops-3")); recorder.Code != http.StatusNotFound {
		t.Fatalf("missing account status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	badLease := GroupSpeakerLeaseRequest{AccountID: "acc_chat_ops", BindingEpoch: "1", LeaseExpiresAt: time.Now().UTC().Add(-time.Minute)}
	if recorder := chatServe(t, handler, http.MethodPut, chatSpeakerLeasePath("900009"), badLease, chatAdminHeaders("chat-ops-4")); recorder.Code != http.StatusConflict || hbErrorCode(t, recorder) != chatCodeBindingConflict {
		t.Fatalf("past lease status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestChatTriggerEvidenceIsAppendOnly verifies the durable trigger cannot be
// rewritten or deleted and that segment progress advances at most one step.
func TestChatTriggerEvidenceIsAppendOnly(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_chat_audit", "rt_chat_audit", 5)
	actor := fixtureRuntime("acc_chat_audit", "rt_chat_audit")
	trigger, _, err := s.RegisterChatTrigger(context.Background(), actor, "chat-audit-1", chatTriggerRequest("trg_chat_audit", "acc_chat_audit", "700001"))
	if err != nil {
		t.Fatal(err)
	}
	rejectsSQL(t, s, `UPDATE core.chat_trigger SET sender_id='700002' WHERE workspace_id='ws_fixture' AND trigger_ref=$1`, trigger.TriggerRef)
	rejectsSQL(t, s, `UPDATE core.chat_trigger SET next_segment=2 WHERE workspace_id='ws_fixture' AND trigger_ref=$1`, trigger.TriggerRef)
	rejectsSQL(t, s, `DELETE FROM core.chat_trigger WHERE workspace_id='ws_fixture' AND trigger_ref=$1`, trigger.TriggerRef)
	rejectsSQL(t, s, `UPDATE core.chat_trigger_idempotency SET request_digest=request_digest WHERE workspace_id='ws_fixture'`)
	rejectsSQL(t, s, `DELETE FROM core.chat_trigger_idempotency WHERE workspace_id='ws_fixture'`)
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.chat_trigger SET next_segment=1 WHERE workspace_id='ws_fixture' AND trigger_ref=$1`, trigger.TriggerRef); err != nil {
		t.Fatalf("advance one segment: %v", err)
	}
	if _, err := s.Pool.Exec(context.Background(), `UPDATE core.chat_trigger SET next_segment=2,state='complete' WHERE workspace_id='ws_fixture' AND trigger_ref=$1`, trigger.TriggerRef); err != nil {
		t.Fatalf("complete trigger: %v", err)
	}
	rejectsSQL(t, s, `UPDATE core.chat_trigger SET state='open' WHERE workspace_id='ws_fixture' AND trigger_ref=$1`, trigger.TriggerRef)
}

func TestChatContextRejectsUnknownQueryAndAccount(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_chat_q", "rt_chat_q", 5)
	handler := s.ChatRuntimeHandler(chatRuntimeAuth(fixtureRuntime("acc_chat_q", "rt_chat_q")))
	if recorder := chatServe(t, handler, http.MethodGet, chatContextPath+"?account_id=acc_chat_q&conversation_kind=group", nil, map[string]string{"Authorization": "Runtime chat-test-token"}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing query status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, handler, http.MethodGet, chatContextPath+"?account_id=acc_chat_q&conversation_kind=group&conversation_id=900001&extra=1", nil, map[string]string{"Authorization": "Runtime chat-test-token"}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("extra query status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, handler, http.MethodGet, chatContextPath+"?account_id=acc_missing&conversation_kind=group&conversation_id=900001", nil, map[string]string{"Authorization": "Runtime chat-test-token"}); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown account status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, handler, http.MethodGet, chatContextPath, nil, map[string]string{"Authorization": "wrong"}); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, handler, http.MethodPost, chatContextPath, nil, map[string]string{"Authorization": "Runtime chat-test-token"}); recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("method status=%d allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}
	if recorder := chatServe(t, handler, http.MethodGet, "/v1/internal/chat-send-permits", nil, map[string]string{"Authorization": "Runtime chat-test-token"}); recorder.Code != http.StatusNotFound {
		t.Fatalf("foreign route status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestChatTriggerUnicodeLengthBoundary(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_chat_len", "rt_chat_len", 5)
	actor := fixtureRuntime("acc_chat_len", "rt_chat_len")
	req := chatTriggerRequest(strings.Repeat("界", 128), "acc_chat_len", "700001")
	req.SourceEventID = "evt_chat_len"
	if len(req.TriggerRef) != 384 {
		t.Fatal("test setup did not create a 384-byte string")
	}
	if _, _, err := s.RegisterChatTrigger(context.Background(), actor, "chat-len-1", req); Code(err) == taskCodeValidation {
		t.Fatalf("128-rune trigger_ref was rejected: %v", err)
	}
	req.TriggerRef = strings.Repeat("界", 129)
	if _, _, err := s.RegisterChatTrigger(context.Background(), actor, "chat-len-2", req); Code(err) != taskCodeValidation {
		t.Fatalf("129-rune trigger_ref error=%v, want VALIDATION_FAILED", err)
	}
}
