package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
)

// Exercises private server atomic invariants only. This is NOT a public send grant.
// Final gateway HTTP/writer business cases live in TestChatGatewaySendConsumesOnceAndNeverResends.
func consumeServerCoreForTest(t *testing.T, s *Store, ctx context.Context, actor RuntimeActor, id string, req ChatConsumeRequest) (contract.ChatPermit, error) {
	t.Helper()
	return s.consumeChatSendPermit(ctx, actor, id, req)
}

func chatTestDeadline(t *testing.T) time.Time {
	t.Helper()
	return time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
}

func chatSeedPolicy(t *testing.T, s *Store, account string, dailyLimit int) {
	t.Helper()
	_, err := s.Pool.Exec(context.Background(), `INSERT INTO core.chat_send_policy(workspace_id,account_id,enabled,daily_limit)
		VALUES('ws_fixture',$1,true,$2)`, account, dailyLimit)
	if err != nil {
		t.Fatalf("seed chat policy: %v", err)
	}
}

func chatSeedTrigger(t *testing.T, s *Store, triggerRef, runtime, account, kind, conversationID string, sender string, directed *string, deadline time.Time, segments int) {
	t.Helper()
	_, err := s.Pool.Exec(context.Background(), `INSERT INTO core.chat_trigger
		(workspace_id,trigger_ref,runtime_id,owner_epoch,account_id,source_event_id,conversation_kind,conversation_id,sender_id,directed_account_id,deadline_at,segment_count,state,request_digest)
		VALUES('ws_fixture',$1,$2,1,$3,$4,$5,$6,$7,$8,$9,$10,'open',$11)`,
		triggerRef, runtime, account, "event_"+triggerRef, kind, conversationID, sender, directed, deadline, segments, chatTestDigest())
	if err != nil {
		t.Fatalf("seed chat trigger: %v", err)
	}
}

func chatSeedSpeaker(t *testing.T, s *Store, groupID, account string, epoch int64, lease time.Time) {
	t.Helper()
	_, err := s.Pool.Exec(context.Background(), `INSERT INTO core.group_binding(workspace_id,group_id,active_speaker_account_id,binding_epoch,lease_expires_at)
		VALUES('ws_fixture',$1,$2,$3,$4)`, groupID, account, epoch, lease)
	if err != nil {
		t.Fatalf("seed speaker lease: %v", err)
	}
}

func chatTestDigest() string {
	return ChatTextDigest("fixture text")
}

func chatPermitRequest(sendID, account, triggerRef, kind, conversationID string, index, count int, deadline time.Time) contract.ChatPermitRequest {
	return contract.ChatPermitRequest{
		SendId:               contract.Id(sendID),
		AccountId:            contract.Id(account),
		ConversationKind:     contract.ChatPermitRequestConversationKind(kind),
		ConversationId:       contract.QQId(conversationID),
		TriggerRef:           contract.Id(triggerRef),
		TextDigest:           contract.Digest(chatTestDigest()),
		ExpectedBindingEpoch: contract.DecimalSequence("1"),
		DeadlineAt:           deadline,
		SegmentIndex:         index,
		SegmentCount:         count,
	}
}

func chatFixture(t *testing.T, dailyLimit int) (*Store, RuntimeActor, time.Time) {
	t.Helper()
	s := fixtureStore(t)
	seedAccount(t, s, "acc_chat", "rt_chat", 5)
	chatSeedPolicy(t, s, "acc_chat", dailyLimit)
	deadline := chatTestDeadline(t)
	chatSeedTrigger(t, s, "trig_chat", "rt_chat", "acc_chat", "group", "424242", "20001", nil, deadline, 2)
	chatSeedSpeaker(t, s, "424242", "acc_chat", 1, time.Now().UTC().Add(time.Hour))
	return s, fixtureRuntime("acc_chat", "rt_chat"), deadline
}

func chatCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	return scalarInt(t, s, query, args...)
}

func TestChatPermitIssueIsStableAndBudgetedOnce(t *testing.T) {
	s, actor, deadline := chatFixture(t, 5)
	ctx := context.Background()
	req := chatPermitRequest("send_1:0", "acc_chat", "trig_chat", "group", "424242", 0, 2, deadline)

	first, err := s.IssueChatSendPermit(ctx, actor, "idem-1", req)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if first.State != contract.ChatPermitStateIssued || first.SegmentIndex != 0 || first.SegmentCount != 2 {
		t.Fatalf("permit=%+v", first)
	}
	if first.ExpiresAt.After(first.IssuedAt.Add(chatPermitTTL)) || first.ExpiresAt.After(deadline) {
		t.Fatalf("expiry=%v issued=%v deadline=%v", first.ExpiresAt, first.IssuedAt, deadline)
	}
	replay, err := s.IssueChatSendPermit(ctx, actor, "idem-1", req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.PermitId != first.PermitId || !replay.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("replay permit=%+v first=%+v", replay, first)
	}
	if got := chatCount(t, s, `SELECT count(*) FROM core.chat_send_permit WHERE workspace_id='ws_fixture'`); got != 1 {
		t.Fatalf("permit rows=%d", got)
	}
	if got := chatCount(t, s, `SELECT issued FROM core.chat_send_usage WHERE workspace_id='ws_fixture' AND account_id='acc_chat' AND window_start=`+chatUsageDateExpr); got != 1 {
		t.Fatalf("issued usage=%d", got)
	}

	changed := req
	changed.TextDigest = contract.Digest(ChatTextDigest("different text"))
	if _, err := s.IssueChatSendPermit(ctx, actor, "idem-2", changed); Code(err) != chatCodeSendDigestConflict {
		t.Fatalf("different digest error=%v code=%s", err, Code(err))
	}
	outOfOrder := chatPermitRequest("send_1:1", "acc_chat", "trig_chat", "group", "424242", 1, 2, deadline)
	if _, err := s.IssueChatSendPermit(ctx, actor, "idem-3", outOfOrder); Code(err) != chatCodeSegmentOutOfOrder {
		t.Fatalf("out of order error=%v code=%s", err, Code(err))
	}

	consumed, err := consumeServerCoreForTest(t, s, ctx, actor, first.PermitId, ChatConsumeRequest{
		SendId: first.SendId, SegmentIndex: 0, TextDigest: string(first.TextDigest),
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if consumed.State != contract.ChatPermitStateConsumed {
		t.Fatalf("consumed state=%s", consumed.State)
	}
	if _, err := consumeServerCoreForTest(t, s, ctx, actor, first.PermitId, ChatConsumeRequest{
		SendId: first.SendId, SegmentIndex: 0, TextDigest: string(first.TextDigest),
	}); Code(err) != chatCodePermitConsumed {
		t.Fatalf("second consume error=%v code=%s", err, Code(err))
	}
	if got := chatCount(t, s, `SELECT next_segment FROM core.chat_trigger WHERE workspace_id='ws_fixture' AND trigger_ref='trig_chat'`); got != 1 {
		t.Fatalf("next_segment=%d", got)
	}

	second, err := s.IssueChatSendPermit(ctx, actor, "idem-4", chatPermitRequest("send_1:1", "acc_chat", "trig_chat", "group", "424242", 1, 2, deadline))
	if err != nil {
		t.Fatalf("issue segment 1: %v", err)
	}
	if second.SegmentIndex != 1 {
		t.Fatalf("segment=%d", second.SegmentIndex)
	}
	if got := chatCount(t, s, `SELECT issued FROM core.chat_send_usage WHERE workspace_id='ws_fixture' AND account_id='acc_chat' AND window_start=`+chatUsageDateExpr); got != 2 {
		t.Fatalf("issued usage after segment 1=%d", got)
	}

	outcome, created, err := s.RecordChatSendOutcome(ctx, actor, first.PermitId, ChatSendOutcomeRequest{
		SendId: first.SendId, State: chatPermitOutcomeSent, OccurredAt: time.Now().UTC().Truncate(time.Microsecond),
	})
	if err != nil || !created || outcome.State != chatPermitOutcomeSent || outcome.MessageId != nil {
		t.Fatalf("outcome=%+v created=%v err=%v", outcome, created, err)
	}
	replayed, createdAgain, err := s.RecordChatSendOutcome(ctx, actor, first.PermitId, ChatSendOutcomeRequest{
		SendId: first.SendId, State: chatPermitOutcomeSent, OccurredAt: outcome.OccurredAt,
	})
	if err != nil || createdAgain || replayed.State != outcome.State {
		t.Fatalf("outcome replay=%+v created=%v err=%v", replayed, createdAgain, err)
	}
	if _, _, err := s.RecordChatSendOutcome(ctx, actor, first.PermitId, ChatSendOutcomeRequest{
		SendId: first.SendId, State: chatPermitOutcomeUnknown, OccurredAt: outcome.OccurredAt,
	}); Code(err) != chatCodeOutcomeConflict {
		t.Fatalf("outcome conflict error=%v code=%s", err, Code(err))
	}
	if got := chatCount(t, s, `SELECT count(*) FROM core.chat_send_outcome WHERE workspace_id='ws_fixture'`); got != 1 {
		t.Fatalf("outcome rows=%d", got)
	}
}

func TestChatPermitDailyBudgetAndConsumeAuthority(t *testing.T) {
	s, actor, deadline := chatFixture(t, 1)
	ctx := context.Background()
	req := chatPermitRequest("send_b:0", "acc_chat", "trig_chat", "group", "424242", 0, 2, deadline)
	permit, err := s.IssueChatSendPermit(ctx, actor, "idem-b1", req)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := s.IssueChatSendPermit(ctx, actor, "idem-b2", chatPermitRequest("send_b:1", "acc_chat", "trig_chat", "group", "424242", 0, 2, deadline)); Code(err) != chatCodeBudgetExhausted {
		t.Fatalf("budget error=%v code=%s", err, Code(err))
	}

	wrongText := ChatConsumeRequest{SendId: permit.SendId, SegmentIndex: 0, TextDigest: ChatTextDigest("not the permitted text")}
	if _, err := consumeServerCoreForTest(t, s, ctx, actor, permit.PermitId, wrongText); Code(err) != chatCodeSendDigestConflict {
		t.Fatalf("wrong text error=%v code=%s", err, Code(err))
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE core.account SET control_state='paused' WHERE workspace_id='ws_fixture' AND id='acc_chat'`); err != nil {
		t.Fatal(err)
	}
	rightText := ChatConsumeRequest{SendId: permit.SendId, SegmentIndex: 0, TextDigest: string(permit.TextDigest)}
	if _, err := consumeServerCoreForTest(t, s, ctx, actor, permit.PermitId, rightText); Code(err) != chatCodeAccountRestricted {
		t.Fatalf("paused consume error=%v code=%s", err, Code(err))
	}
	if got := chatCount(t, s, `SELECT count(*) FROM core.chat_send_permit WHERE workspace_id='ws_fixture' AND state='issued'`); got != 1 {
		t.Fatalf("issued permits after rejected consume=%d", got)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE core.account SET control_state='active' WHERE workspace_id='ws_fixture' AND id='acc_chat'`); err != nil {
		t.Fatal(err)
	}
	if _, err := consumeServerCoreForTest(t, s, ctx, actor, permit.PermitId, rightText); err != nil {
		t.Fatalf("consume after resume: %v", err)
	}
	// A second consume of the same permit must never return success: the
	// caller would otherwise be authorized to send twice.
	if _, err := consumeServerCoreForTest(t, s, ctx, actor, permit.PermitId, rightText); Code(err) != chatCodePermitConsumed {
		t.Fatalf("double consume error=%v code=%s", err, Code(err))
	}
}

func TestChatPermitUnknownOutcomeSuppressesTrigger(t *testing.T) {
	s, actor, deadline := chatFixture(t, 5)
	ctx := context.Background()
	permit, err := s.IssueChatSendPermit(ctx, actor, "idem-unk-1", chatPermitRequest("send_u:0", "acc_chat", "trig_chat", "group", "424242", 0, 2, deadline))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := consumeServerCoreForTest(t, s, ctx, actor, permit.PermitId, ChatConsumeRequest{
		SendId: permit.SendId, SegmentIndex: 0, TextDigest: string(permit.TextDigest),
	}); err != nil {
		t.Fatalf("consume: %v", err)
	}
	// A next-segment permit can be issued before the unknown outcome arrives.
	// It must become unusable once the trigger is suppressed.
	preissued, err := s.IssueChatSendPermit(ctx, actor, "idem-unk-pre", chatPermitRequest("send_pre:1", "acc_chat", "trig_chat", "group", "424242", 1, 2, deadline))
	if err != nil {
		t.Fatalf("pre-issue next segment: %v", err)
	}
	if _, created, err := s.RecordChatSendOutcome(ctx, actor, permit.PermitId, ChatSendOutcomeRequest{
		SendId: permit.SendId, State: chatPermitOutcomeUnknown, OccurredAt: time.Now().UTC().Truncate(time.Microsecond),
	}); err != nil || !created {
		t.Fatalf("unknown outcome created=%v err=%v", created, err)
	}
	if got := chatCount(t, s, `SELECT count(*) FROM core.chat_trigger WHERE workspace_id='ws_fixture' AND trigger_ref='trig_chat' AND state='suppressed'`); got != 1 {
		t.Fatalf("trigger not suppressed: %d", got)
	}
	if _, err := consumeServerCoreForTest(t, s, ctx, actor, preissued.PermitId, ChatConsumeRequest{
		SendId: preissued.SendId, SegmentIndex: 1, TextDigest: string(preissued.TextDigest),
	}); Code(err) != chatCodeTriggerConflict {
		t.Fatalf("post-unknown consume error=%v code=%s", err, Code(err))
	}
	if got := chatCount(t, s, `SELECT count(*) FROM core.chat_send_permit WHERE workspace_id='ws_fixture' AND permit_id=$1 AND state='issued'`, preissued.PermitId); got != 1 {
		t.Fatalf("pre-issued permit state rows=%d", got)
	}
	if _, err := s.IssueChatSendPermit(ctx, actor, "idem-unk-2", chatPermitRequest("send_u:1", "acc_chat", "trig_chat", "group", "424242", 1, 2, deadline)); Code(err) != chatCodeTriggerConflict {
		t.Fatalf("post-unknown issue error=%v code=%s", err, Code(err))
	}
	if got := chatCount(t, s, `SELECT count(*) FROM core.chat_send_permit WHERE workspace_id='ws_fixture' AND send_id='send_u:1' AND segment_index=1`); got != 0 {
		t.Fatalf("later segment permit rows=%d", got)
	}
}

func TestChatPermitSpeakerAndPrivateBinding(t *testing.T) {
	s, actor, deadline := chatFixture(t, 10)
	ctx := context.Background()
	seedAccount(t, s, "acc_other", "rt_other", 5)
	chatSeedPolicy(t, s, "acc_other", 10)

	if _, err := s.IssueChatSendPermit(ctx, actor, "idem-spk", chatPermitRequest("send_s:0", "acc_chat", "trig_chat", "group", "424242", 0, 2, deadline)); err != nil {
		t.Fatalf("speaker issue: %v", err)
	}
	otherReq := chatPermitRequest("send_s:0", "acc_other", "trig_chat", "group", "424242", 0, 2, deadline)
	if _, err := s.IssueChatSendPermit(ctx, fixtureRuntime("acc_other", "rt_other"), "idem-spk2", otherReq); Code(err) != chatCodeTriggerUnknown {
		t.Fatalf("cross-runtime trigger error=%v code=%s", err, Code(err))
	}

	privateDeadline := chatTestDeadline(t)
	chatSeedTrigger(t, s, "trig_private", "rt_chat", "acc_chat", "private", "20001", "20001", nil, privateDeadline, 1)
	privateReq := chatPermitRequest("send_p:0", "acc_chat", "trig_private", "private", "20001", 0, 1, privateDeadline)
	privateReq.ExpectedBindingEpoch = contract.DecimalSequence("2")
	if _, err := s.IssueChatSendPermit(ctx, actor, "idem-priv-bad", privateReq); Code(err) != chatCodeBindingConflict {
		t.Fatalf("private epoch error=%v code=%s", err, Code(err))
	}
	privateReq.ExpectedBindingEpoch = contract.DecimalSequence("1")
	privatePermit, err := s.IssueChatSendPermit(ctx, actor, "idem-priv", privateReq)
	if err != nil {
		t.Fatalf("private issue: %v", err)
	}
	if _, err := consumeServerCoreForTest(t, s, ctx, actor, privatePermit.PermitId, ChatConsumeRequest{
		SendId: privatePermit.SendId, SegmentIndex: 0, TextDigest: string(privatePermit.TextDigest),
	}); err != nil {
		t.Fatalf("private consume: %v", err)
	}
}

func TestChatPermitHTTPContract(t *testing.T) {
	s, actor, deadline := chatFixture(t, 5)
	handler := s.ChatPermitRuntimeHandler(func(r *http.Request) (RuntimeActor, error) {
		if r.Header.Get("Authorization") != "Runtime chat-test" {
			return RuntimeActor{}, problem(taskCodeUnauthenticated)
		}
		return actor, nil
	})
	req := chatPermitRequest("send_http:0", "acc_chat", "trig_chat", "group", "424242", 0, 2, deadline)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{
		"Authorization":   "Runtime chat-test",
		"Content-Type":    "application/json",
		"Idempotency-Key": "http-idem-1",
		"X-Request-Id":    "req-chat-1",
	}
	recorder := hbServe(t, handler, http.MethodPost, chatPermitPath, body, headers)
	if recorder.Code != http.StatusOK {
		t.Fatalf("issue status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var issued contract.ChatPermitEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Data.State != contract.ChatPermitStateIssued || issued.Meta.RequestId != "req-chat-1" {
		t.Fatalf("envelope=%+v", issued)
	}

	noKey := map[string]string{"Authorization": "Runtime chat-test", "Content-Type": "application/json"}
	if recorder := hbServe(t, handler, http.MethodPost, chatPermitPath, body, noKey); recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var missing map[string]any
	if err := json.Unmarshal(body, &missing); err != nil {
		t.Fatal(err)
	}
	delete(missing, "trigger_ref")
	withoutTrigger, _ := json.Marshal(missing)
	if recorder := hbServe(t, handler, http.MethodPost, chatPermitPath, withoutTrigger, headers); recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing trigger status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	consumeBody, _ := json.Marshal(ChatConsumeRequest{SendId: issued.Data.SendId, SegmentIndex: 0, TextDigest: string(issued.Data.TextDigest)})
	for i := 0; i < 2; i++ {
		recorder = hbServe(t, handler, http.MethodPost, chatPermitPath+"/"+string(issued.Data.PermitId)+"/consume", consumeBody, headers)
		if recorder.Code != 403 || hbErrorCode(t, recorder) != "CONTENT_FINAL_TEXT_REQUIRED" {
			t.Fatalf("legacy migration: %d %s", recorder.Code, recorder.Body)
		}
	}
	outcomeBody, _ := json.Marshal(ChatSendOutcomeRequest{SendId: issued.Data.SendId, State: chatPermitOutcomeSent, OccurredAt: time.Now().UTC()})
	recorder = hbServe(t, handler, http.MethodPost, chatPermitPath+"/"+string(issued.Data.PermitId)+"/outcome", outcomeBody, headers)
	if recorder.Code != 403 {
		t.Fatalf("client outcome still accepted: %s", recorder.Body)
	}
}

type fixtureChatGatewayWriter struct {
	calls     int
	messageID string
	err       error
}

func (w *fixtureChatGatewayWriter) SendChatText(_ context.Context, _ RuntimeActor, _, _, _ string) (string, error) {
	w.calls++
	return w.messageID, w.err
}

func TestChatGatewaySendConsumesOnceAndNeverResends(t *testing.T) {
	t.Run("digest and presence checks happen before consume", func(t *testing.T) {
		s, actor, deadline := chatFixture(t, 5)
		permit, err := s.IssueChatSendPermit(context.Background(), actor, "gateway-validate", chatPermitRequest("send_gateway:0", "acc_chat", "trig_chat", "group", "424242", 0, 2, deadline))
		if err != nil {
			t.Fatal(err)
		}
		writer := &fixtureChatGatewayWriter{messageID: "msg-validate"}
		handler := s.ChatSendGatewayHandler(hbAuth(actor), writer)
		path := chatPermitPath + "/" + string(permit.PermitId) + "/gateway-send"
		headers := map[string]string{"Authorization": "Runtime hb-test-token", "Content-Type": "application/json", "Idempotency-Key": "gateway-validate-1"}

		missingSegment, _ := json.Marshal(map[string]any{"send_id": permit.SendId, "text": "fixture text"})
		if response := hbServe(t, handler, http.MethodPost, path, missingSegment, headers); response.Code != http.StatusBadRequest {
			t.Fatalf("missing segment status=%d body=%s", response.Code, response.Body.String())
		}
		wrongText, _ := json.Marshal(map[string]any{"send_id": permit.SendId, "segment_index": 0, "text": "different text"})
		if response := hbServe(t, handler, http.MethodPost, path, wrongText, headers); response.Code != http.StatusConflict || hbErrorCode(t, response) != chatCodeSendDigestConflict {
			t.Fatalf("digest conflict status=%d code=%s body=%s", response.Code, hbErrorCode(t, response), response.Body.String())
		}
		if writer.calls != 0 {
			t.Fatalf("writer calls=%d before valid request, want 0", writer.calls)
		}
	})

	t.Run("sent outcome is replayed without a second writer call", func(t *testing.T) {
		s, actor, deadline := chatFixture(t, 5)
		chatSeedTrigger(t, s, "trig_gateway_single", "rt_chat", "acc_chat", "group", "424242", "20001", nil, deadline, 1)
		permit, err := s.IssueChatSendPermit(context.Background(), actor, "gateway-sent", chatPermitRequest("send_gateway:0", "acc_chat", "trig_gateway_single", "group", "424242", 0, 1, deadline))
		if err != nil {
			t.Fatal(err)
		}
		writer := &fixtureChatGatewayWriter{messageID: "msg-sent"}
		handler := s.ChatSendGatewayHandler(hbAuth(actor), writer)
		path := chatPermitPath + "/" + string(permit.PermitId) + "/gateway-send"
		body, _ := json.Marshal(map[string]any{"send_id": permit.SendId, "segment_index": 0, "text": "fixture text"})
		headers := map[string]string{"Authorization": "Runtime hb-test-token", "Content-Type": "application/json", "Idempotency-Key": "gateway-sent-1"}
		if response := hbServe(t, handler, http.MethodPost, path, body, headers); response.Code != http.StatusOK {
			t.Fatalf("first send status=%d body=%s", response.Code, response.Body.String())
		}
		if response := hbServe(t, handler, http.MethodPost, path, body, headers); response.Code != http.StatusOK {
			t.Fatalf("replayed send status=%d body=%s", response.Code, response.Body.String())
		}
		if writer.calls != 1 {
			t.Fatalf("writer calls=%d after replay, want 1", writer.calls)
		}
	})

	t.Run("unknown outcome is replayed without a second writer call", func(t *testing.T) {
		s, actor, deadline := chatFixture(t, 5)
		chatSeedTrigger(t, s, "trig_gateway_single", "rt_chat", "acc_chat", "group", "424242", "20001", nil, deadline, 1)
		permit, err := s.IssueChatSendPermit(context.Background(), actor, "gateway-unknown", chatPermitRequest("send_gateway:0", "acc_chat", "trig_gateway_single", "group", "424242", 0, 1, deadline))
		if err != nil {
			t.Fatal(err)
		}
		writer := &fixtureChatGatewayWriter{err: errors.New("fixture transport timeout")}
		handler := s.ChatSendGatewayHandler(hbAuth(actor), writer)
		path := chatPermitPath + "/" + string(permit.PermitId) + "/gateway-send"
		body, _ := json.Marshal(map[string]any{"send_id": permit.SendId, "segment_index": 0, "text": "fixture text"})
		headers := map[string]string{"Authorization": "Runtime hb-test-token", "Content-Type": "application/json", "Idempotency-Key": "gateway-unknown-1"}
		if response := hbServe(t, handler, http.MethodPost, path, body, headers); response.Code != http.StatusConflict || hbErrorCode(t, response) != "EXTERNAL_OUTCOME_UNKNOWN" {
			t.Fatalf("unknown send status=%d code=%s body=%s", response.Code, hbErrorCode(t, response), response.Body.String())
		}
		if response := hbServe(t, handler, http.MethodPost, path, body, headers); response.Code != http.StatusConflict || hbErrorCode(t, response) != "EXTERNAL_OUTCOME_UNKNOWN" {
			t.Fatalf("unknown replay status=%d code=%s body=%s", response.Code, hbErrorCode(t, response), response.Body.String())
		}
		if writer.calls != 1 {
			t.Fatalf("writer calls=%d after unknown replay, want 1", writer.calls)
		}
	})
}
