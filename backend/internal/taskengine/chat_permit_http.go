package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"bba.local/taskengine/internal/contract"
)

const chatPermitPath = "/v1/internal/chat-send-permits"

type chatPermitHTTP struct {
	store      *Store
	auth       func(*http.Request) (RuntimeActor, error)
	chatWriter ChatSendGatewayWriter
}

type ChatSendGatewayWriter interface {
	SendChatText(context.Context, RuntimeActor, string, string, string) (string, error)
}

// ChatPermitRuntimeHandler exposes runtime-authenticated permit issue, consume, and outcome routes.
func (s *Store) ChatPermitRuntimeHandler(auth func(*http.Request) (RuntimeActor, error)) http.Handler {
	return &chatPermitHTTP{store: s, auth: auth}
}

// ChatSendGatewayHandler adds the one-shot Gateway-owned send route.
func (s *Store) ChatSendGatewayHandler(auth func(*http.Request) (RuntimeActor, error), writer ChatSendGatewayWriter) http.Handler {
	return &chatPermitHTTP{store: s, auth: auth, chatWriter: writer}
}
func (h *chatPermitHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	actor, err := h.authenticate(r)
	if err != nil {
		chatWriteError(w, r, err)
		return
	}
	switch {
	case r.URL.Path == chatPermitPath+"/capabilities":
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, "GET")
			return
		}
		if h.chatWriter == nil {
			thWriteErrorCode(w, r, "CONTENT_FINAL_TEXT_REQUIRED", http.StatusNotImplemented)
			return
		}
		thWriteJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"api_version": "character-gateway-v1", "single_text": true}})
	case r.URL.Path == chatPermitPath:
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, "POST")
			return
		}
		h.issue(w, r, actor)
	case strings.HasPrefix(r.URL.Path, chatPermitPath+"/"):
		rest := strings.TrimPrefix(r.URL.Path, chatPermitPath+"/")
		permitID, action, ok := strings.Cut(rest, "/")
		if !ok || strings.Contains(action, "/") || !taskValidID(permitID) {
			thWriteError(w, r, problem(chatCodePermitUnknown))
			return
		}
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, "POST")
			return
		}
		switch action {
		case "consume":
			h.consume(w, r, actor, permitID)
		case "outcome":
			h.outcome(w, r, actor, permitID)
		case "gateway-send":
			h.gatewaySend(w, r, actor, permitID)
		default:
			thWriteError(w, r, problem(chatCodePermitUnknown))
		}
	default:
		thWriteError(w, r, problem(taskCodeNotFound))
	}
}

func (h *chatPermitHTTP) authenticate(r *http.Request) (RuntimeActor, error) {
	if h.auth == nil {
		return RuntimeActor{}, problem(taskCodeUnauthenticated)
	}
	actor, err := h.auth(r)
	if err != nil {
		var coded *Error
		if errors.As(err, &coded) {
			return RuntimeActor{}, coded
		}
		return RuntimeActor{}, problem(taskCodeUnauthenticated)
	}
	if taskBlank(actor.WorkspaceID) || taskBlank(actor.RuntimeID) || actor.OwnerEpoch <= 0 {
		return RuntimeActor{}, problem(taskCodeUnauthenticated)
	}
	return actor, nil
}

func (h *chatPermitHTTP) issue(w http.ResponseWriter, r *http.Request, actor RuntimeActor) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		chatWriteError(w, r, err)
		return
	}
	var req contract.ChatPermitRequest
	raw, err := thDecodeStrict(w, r, &req)
	if err != nil {
		chatWriteError(w, r, err)
		return
	}
	if err := chatValidateIssuePresence(raw); err != nil {
		chatWriteError(w, r, err)
		return
	}
	permit, err := h.store.IssueChatSendPermit(r.Context(), actor, key, req)
	if err != nil {
		chatWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, contract.ChatPermitEnvelope{Data: permit, Meta: thMeta(r)})
}

func (h *chatPermitHTTP) consume(w http.ResponseWriter, r *http.Request, actor RuntimeActor, permitID string) {
	if _, err := thIdempotencyKey(r); err != nil {
		chatWriteError(w, r, err)
		return
	}
	var req ChatConsumeRequest
	raw, err := thDecodeStrict(w, r, &req)
	if err != nil {
		chatWriteError(w, r, err)
		return
	}
	if err := chatValidateConsumePresence(raw); err != nil {
		chatWriteError(w, r, err)
		return
	}
	permit, err := h.store.ConsumeChatSendPermit(r.Context(), actor, permitID, req)
	if err != nil {
		chatWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, contract.ChatPermitEnvelope{Data: permit, Meta: thMeta(r)})
}

func (h *chatPermitHTTP) outcome(w http.ResponseWriter, r *http.Request, actor RuntimeActor, permitID string) {
	if _, err := thIdempotencyKey(r); err != nil {
		chatWriteError(w, r, err)
		return
	}
	thWriteErrorCode(w, r, "CONTENT_FINAL_TEXT_REQUIRED", http.StatusForbidden)
}

func (h *chatPermitHTTP) gatewaySend(w http.ResponseWriter, r *http.Request, actor RuntimeActor, permitID string) {
	ctx, cancel := context.WithTimeout(r.Context(), contentSendTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	if r.Method != http.MethodPost {
		thMethodNotAllowed(w, r, "POST")
		return
	}
	if _, err := thIdempotencyKey(r); err != nil {
		chatWriteError(w, r, err)
		return
	}
	var req struct {
		SendID           string `json:"send_id"`
		SegmentIndex     int    `json:"segment_index"`
		Text             string `json:"text"`
		AccountID        string `json:"account_id,omitempty"`
		ConversationKind string `json:"conversation_kind,omitempty"`
		ConversationID   string `json:"conversation_id,omitempty"`
	}
	raw, err := thDecodeStrict(w, r, &req)
	if err != nil {
		chatWriteError(w, r, err)
		return
	}
	if err := thRequireKeys(raw, "send_id", "segment_index", "text"); err != nil {
		chatWriteError(w, r, err)
		return
	}
	if err := thRejectNulls(raw, "send_id", "segment_index", "text"); err != nil {
		chatWriteError(w, r, err)
		return
	}
	if req.Text == "" || len(req.Text) > 8192 || !taskValidID(req.SendID) || req.SegmentIndex < 0 || req.SegmentIndex >= chatPermitSegmentCountMax {
		chatWriteError(w, r, problem(taskCodeValidation))
		return
	}
	permit, err := h.store.GetChatSendPermit(r.Context(), actor, permitID)
	if err != nil {
		chatWriteError(w, r, err)
		return
	}
	if string(permit.SendId) != req.SendID || int(permit.SegmentIndex) != req.SegmentIndex || string(permit.TextDigest) != ChatTextDigest(req.Text) {
		chatWriteError(w, r, problem(chatCodeSendDigestConflict))
		return
	}
	if (req.AccountID != "" || req.ConversationKind != "" || req.ConversationID != "") &&
		(req.AccountID != string(permit.AccountId) || req.ConversationKind != string(permit.ConversationKind) || req.ConversationID != string(permit.ConversationId)) {
		chatWriteError(w, r, problem(chatCodeSendDigestConflict))
		return
	}
	if h.chatWriter == nil {
		chatWriteError(w, r, problem(taskCodeForbidden))
		return
	}
	if string(permit.State) == "consumed" {
		outcome, err := h.store.GetChatSendOutcome(r.Context(), actor, permitID)
		if err != nil {
			chatWriteError(w, r, problem("EXTERNAL_OUTCOME_UNKNOWN"))
			return
		}
		if outcome.State != chatPermitOutcomeSent || outcome.MessageId == nil {
			chatWriteError(w, r, problem("EXTERNAL_OUTCOME_UNKNOWN"))
			return
		}
		writeChatGatewayReceipt(w, r, permit, outcome.State, *outcome.MessageId)
		return
	}
	guard, err := h.store.acquireContentSendGuard(r.Context(), actor.WorkspaceID)
	if err != nil {
		chatWriteError(w, r, problem(taskCodeStoreUnavailable))
		return
	}
	defer guard.close()
	// Shared workspace rules must not turn concurrent replays of ONE permit
	// into competing senders. Serialize that permit on the same bounded session.
	// Rules never acquire this key, so the workspace -> permit order cannot invert.
	if _, err := guard.tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,732614))`, actor.WorkspaceID+"."+permitID); err != nil {
		chatWriteError(w, r, problem(taskCodeStoreUnavailable))
		return
	}
	permit, err = h.store.GetChatSendPermit(r.Context(), actor, permitID)
	if err != nil {
		chatWriteError(w, r, err)
		return
	}
	if string(permit.State) == "consumed" {
		outcome, e := h.store.GetChatSendOutcome(r.Context(), actor, permitID)
		if e != nil || outcome.State != chatPermitOutcomeSent || outcome.MessageId == nil {
			chatWriteError(w, r, problem("EXTERNAL_OUTCOME_UNKNOWN"))
			return
		}
		writeChatGatewayReceipt(w, r, permit, outcome.State, *outcome.MessageId)
		return
	}
	// Bind the exact final adapter payload, account, conversation and send id.
	payloadDigest, err := digest(map[string]any{"workspace_id": actor.WorkspaceID, "account_id": permit.AccountId, "conversation_kind": permit.ConversationKind, "conversation_id": permit.ConversationId, "send_id": req.SendID, "segment_index": req.SegmentIndex, "text": req.Text})
	if err != nil {
		chatWriteError(w, r, problem(taskCodeValidation))
		return
	}
	decision, err := h.store.contentInspect(r.Context(), actor.WorkspaceID, string(permit.AccountId), string(permit.ConversationKind), string(permit.ConversationId), req.SendID, "", req.Text, payloadDigest, int(permit.SegmentCount) != 1)
	if err != nil {
		chatWriteError(w, r, problem(taskCodeStoreUnavailable))
		return
	}
	if decision != "allowed" {
		code := "CONTENT_BLOCKED"
		if decision == "unsupported" {
			code = "CONTENT_TYPE_UNSUPPORTED"
		}
		thWriteErrorCode(w, r, code, http.StatusForbidden)
		return
	}
	consumed, err := h.store.consumeChatSendPermit(r.Context(), actor, permitID, ChatConsumeRequest{
		SendId: req.SendID, SegmentIndex: req.SegmentIndex, TextDigest: ChatTextDigest(req.Text),
	})
	if err != nil {
		chatWriteError(w, r, err)
		return
	}
	state := chatPermitOutcomeUnknown
	var messageRef *string
	if err := guard.healthy(r.Context()); err != nil {
		chatWriteError(w, r, problem(taskCodeStoreUnavailable))
		return
	}
	sendCtx, stopWatch := guard.watch(r.Context())
	defer stopWatch()
	messageID, sendErr := h.chatWriter.SendChatText(sendCtx, actor, string(consumed.ConversationKind), string(consumed.ConversationId), req.Text)
	if healthErr := stopWatch(); healthErr != nil {
		sendErr = healthErr
	}
	if sendErr == nil && messageID != "" {
		state = chatPermitOutcomeSent
		messageRef = &messageID
	}
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer persistCancel()
	_, _, recordErr := h.store.RecordChatSendOutcome(persistCtx, actor, permitID, ChatSendOutcomeRequest{
		SendId: req.SendID, State: state, MessageId: messageRef, OccurredAt: time.Now().UTC(),
	})
	if recordErr != nil || state != chatPermitOutcomeSent {
		chatWriteError(w, r, problem("EXTERNAL_OUTCOME_UNKNOWN"))
		return
	}
	writeChatGatewayReceipt(w, r, consumed, state, messageID)
}

func writeChatGatewayReceipt(w http.ResponseWriter, r *http.Request, p contract.ChatPermit, state, messageID string) {
	thWriteJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"api_version": "character-gateway-v1", "permit_id": p.PermitId, "send_id": p.SendId,
		"segment_index": p.SegmentIndex, "account_id": p.AccountId, "conversation_kind": p.ConversationKind,
		"conversation_id": p.ConversationId, "state": state, "message_id": messageID}, "meta": thMeta(r)})
}

func chatValidateIssuePresence(raw map[string]json.RawMessage) error {
	required := []string{"send_id", "account_id", "conversation_kind", "conversation_id", "trigger_ref",
		"text_digest", "expected_binding_epoch", "deadline_at", "segment_index", "segment_count"}
	if err := thRequireKeys(raw, required...); err != nil {
		return err
	}
	if err := thRejectNulls(raw, required...); err != nil {
		return err
	}
	var deadline string
	if err := json.Unmarshal(raw["deadline_at"], &deadline); err != nil || !strings.HasSuffix(deadline, "Z") {
		return problem(taskCodeValidation)
	}
	var epoch string
	if err := json.Unmarshal(raw["expected_binding_epoch"], &epoch); err != nil {
		return problem(taskCodeValidation)
	}
	if _, err := chatParseDecimal(epoch); err != nil {
		return problem(taskCodeValidation)
	}
	var textDigest string
	if err := json.Unmarshal(raw["text_digest"], &textDigest); err != nil || !chatDigestPattern.MatchString(textDigest) {
		return problem(taskCodeValidation)
	}
	var kind, conversationID string
	if err := json.Unmarshal(raw["conversation_kind"], &kind); err != nil {
		return problem(taskCodeValidation)
	}
	if err := json.Unmarshal(raw["conversation_id"], &conversationID); err != nil {
		return problem(taskCodeValidation)
	}
	if err := chatValidateCommonConversation(kind, conversationID); err != nil {
		return err
	}
	return nil
}

func chatValidateConsumePresence(raw map[string]json.RawMessage) error {
	required := []string{"send_id", "segment_index", "text_digest"}
	if err := thRequireKeys(raw, required...); err != nil {
		return err
	}
	if err := thRejectNulls(raw, required...); err != nil {
		return err
	}
	var textDigest string
	if err := json.Unmarshal(raw["text_digest"], &textDigest); err != nil || !chatDigestPattern.MatchString(textDigest) {
		return problem(taskCodeValidation)
	}
	return nil
}

func chatValidateOutcomePresence(raw map[string]json.RawMessage) error {
	required := []string{"send_id", "state", "occurred_at"}
	if err := thRequireKeys(raw, required...); err != nil {
		return err
	}
	if err := thRejectNulls(raw, required...); err != nil {
		return err
	}
	var occurredAt string
	if err := json.Unmarshal(raw["occurred_at"], &occurredAt); err != nil || !strings.HasSuffix(occurredAt, "Z") {
		return problem(taskCodeValidation)
	}
	var state string
	if err := json.Unmarshal(raw["state"], &state); err != nil {
		return problem(taskCodeValidation)
	}
	switch state {
	case chatPermitOutcomeSent:
		if value, ok := raw["message_id"]; ok && !thIsJSONNull(value) {
			var messageID string
			if json.Unmarshal(value, &messageID) != nil || !taskValidID(messageID) {
				return problem(taskCodeValidation)
			}
		}
	case chatPermitOutcomeUnknown:
		if value, ok := raw["message_id"]; ok && !thIsJSONNull(value) {
			return problem(taskCodeValidation)
		}
	default:
		return problem(taskCodeValidation)
	}
	return nil
}

var chatErrorStatus = map[string]int{
	"CONTENT_FINAL_TEXT_REQUIRED": http.StatusForbidden,
	chatCodeTriggerConflict:       http.StatusConflict,
	chatCodeTriggerUnknown:        http.StatusNotFound,
	chatCodeTriggerExpired:        http.StatusConflict,
	chatCodePermitConsumed:        http.StatusConflict,
	chatCodePermitExpired:         http.StatusConflict,
	chatCodePermitUnknown:         http.StatusNotFound,
	chatCodeSendDigestConflict:    http.StatusConflict,
	chatCodeSegmentOutOfOrder:     http.StatusConflict,
	chatCodePolicyConflict:        http.StatusConflict,
	chatCodePolicyDisabled:        http.StatusForbidden,
	chatCodeSpeakerNotActive:      http.StatusForbidden,
	chatCodeBindingConflict:       http.StatusConflict,
	chatCodeBudgetExhausted:       http.StatusConflict,
	chatCodeOutcomeConflict:       http.StatusConflict,
	chatCodePermitNotConsumed:     http.StatusConflict,
	chatCodeAccountRestricted:     http.StatusConflict,
	chatCodeManagedSelfID:         http.StatusForbidden,
}

func chatWriteError(w http.ResponseWriter, r *http.Request, err error) {
	code := Code(err)
	if status, ok := chatErrorStatus[code]; ok {
		thWriteErrorCode(w, r, code, status)
		return
	}
	thWriteError(w, r, err)
}
