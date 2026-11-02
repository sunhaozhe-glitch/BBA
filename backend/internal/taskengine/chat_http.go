package taskengine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"bba.local/taskengine/internal/contract"
)

const (
	chatTriggerPath       = "/v1/internal/chat-triggers"
	chatContextPath       = "/v1/internal/chat-context"
	maxChatBodyBytes      = 64 << 10
	chatPolicyPathSuffix  = "/chat-policy"
	chatSpeakerPathSuffix = "/speaker-lease"
)

type chatEnvelope struct {
	Data any                  `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

type chatRuntimeHTTP struct {
	store *Store
	auth  func(*http.Request) (RuntimeActor, error)
}

// ChatRuntimeHandler exposes trigger registration and the read-only chat
// context only. Permit issue/consume live in ChatPermitRuntimeHandler so the
// two handlers can be mounted independently without sharing route ownership.
func (s *Store) ChatRuntimeHandler(auth func(*http.Request) (RuntimeActor, error)) http.Handler {
	return &chatRuntimeHTTP{store: s, auth: auth}
}

func (h *chatRuntimeHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == chatTriggerPath:
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, http.MethodPost)
			return
		}
	case r.URL.Path == chatContextPath:
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, http.MethodGet)
			return
		}
	default:
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	actor, err := h.authenticate(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if r.URL.Path == chatTriggerPath {
		h.registerTrigger(w, r, actor)
		return
	}
	h.context(w, r, actor)
}

func (h *chatRuntimeHTTP) authenticate(r *http.Request) (RuntimeActor, error) {
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

func (h *chatRuntimeHTTP) registerTrigger(w http.ResponseWriter, r *http.Request, actor RuntimeActor) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	var req ChatTriggerRequest
	raw, err := chatDecodeStrict(w, r, &req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := chatValidateTriggerPresence(raw); err != nil {
		thWriteError(w, r, err)
		return
	}
	trigger, created, err := h.store.RegisterChatTrigger(r.Context(), actor, key, req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	thWriteJSON(w, status, chatEnvelope{Data: trigger, Meta: thMeta(r)})
}

func (h *chatRuntimeHTTP) context(w http.ResponseWriter, r *http.Request, actor RuntimeActor) {
	accountID, kind, conversationID, err := chatContextQuery(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	out, err := h.store.ChatContextForRuntime(r.Context(), actor, accountID, kind, conversationID)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, chatEnvelope{Data: out, Meta: thMeta(r)})
}

type chatAdminHTTP struct {
	store *Store
	auth  func(*http.Request) (Actor, error)
}

// ChatAdminHandler exposes trusted account chat policy and group speaker lease
// administration. It never accepts a runtime credential.
func (s *Store) ChatAdminHandler(auth func(*http.Request) (Actor, error)) http.Handler {
	return &chatAdminHTTP{store: s, auth: auth}
}

func (h *chatAdminHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	accountID, groupID, operation, ok := chatAdminRoute(r.URL.Path)
	if !ok {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	if r.Method != http.MethodPut {
		thMethodNotAllowed(w, r, http.MethodPut)
		return
	}
	actor, err := h.authenticate(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	switch operation {
	case "policy":
		var req ChatPolicyRequest
		raw, err := chatDecodeStrict(w, r, &req)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		if err := chatValidatePolicyPresence(raw); err != nil {
			thWriteError(w, r, err)
			return
		}
		out, err := h.store.SetChatPolicy(r.Context(), actor, accountID, key, req)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		thWriteJSON(w, http.StatusOK, chatEnvelope{Data: out, Meta: thMeta(r)})
	case "lease":
		var req GroupSpeakerLeaseRequest
		raw, err := chatDecodeStrict(w, r, &req)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		if err := chatValidateLeasePresence(raw); err != nil {
			thWriteError(w, r, err)
			return
		}
		out, err := h.store.SetGroupSpeakerLease(r.Context(), actor, groupID, key, req)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		thWriteJSON(w, http.StatusOK, chatEnvelope{Data: out, Meta: thMeta(r)})
	}
}

func (h *chatAdminHTTP) authenticate(r *http.Request) (Actor, error) {
	if h.auth == nil {
		return Actor{}, problem(taskCodeUnauthenticated)
	}
	actor, err := h.auth(r)
	if err != nil {
		var coded *Error
		if errors.As(err, &coded) {
			return Actor{}, coded
		}
		return Actor{}, problem(taskCodeUnauthenticated)
	}
	if taskBlank(actor.WorkspaceID) || taskBlank(actor.PrincipalID) {
		return Actor{}, problem(taskCodeUnauthenticated)
	}
	return actor, nil
}

func chatAdminRoute(path string) (accountID, groupID, operation string, ok bool) {
	if strings.HasPrefix(path, "/v1/accounts/") && strings.HasSuffix(path, chatPolicyPathSuffix) {
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/accounts/"), chatPolicyPathSuffix)
		if !chatValidID(id) || strings.Contains(id, "/") {
			return "", "", "", false
		}
		return id, "", "policy", true
	}
	if strings.HasPrefix(path, "/v1/groups/") && strings.HasSuffix(path, chatSpeakerPathSuffix) {
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/groups/"), chatSpeakerPathSuffix)
		if !taskValidQQID(id) || strings.Contains(id, "/") {
			return "", "", "", false
		}
		return "", id, "lease", true
	}
	return "", "", "", false
}

func chatContextQuery(r *http.Request) (accountID, kind, conversationID string, err error) {
	values := r.URL.Query()
	for _, key := range []string{"account_id", "conversation_kind", "conversation_id"} {
		if items, ok := values[key]; !ok || len(items) != 1 {
			return "", "", "", problem(taskCodeValidation)
		}
	}
	for key := range values {
		if key != "account_id" && key != "conversation_kind" && key != "conversation_id" {
			return "", "", "", problem(taskCodeValidation)
		}
	}
	accountID, kind, conversationID = values.Get("account_id"), values.Get("conversation_kind"), values.Get("conversation_id")
	if !chatValidID(accountID) || !taskValidQQID(conversationID) ||
		(kind != chatConversationGroup && kind != chatConversationPrivate) {
		return "", "", "", problem(taskCodeValidation)
	}
	return accountID, kind, conversationID, nil
}

func chatDecodeStrict(w http.ResponseWriter, r *http.Request, dst any) (map[string]json.RawMessage, error) {
	ct := strings.TrimSpace(r.Header.Get("Content-Type"))
	if ct == "" {
		return nil, problem(taskCodeUnsupportedMedia)
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != "application/json" {
		return nil, problem(taskCodeUnsupportedMedia)
	}
	if r.Body == nil {
		return nil, problem(taskCodeValidation)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, problem(taskCodePayloadTooLarge)
		}
		return nil, problem(taskCodeValidation)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, problem(taskCodeValidation)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return nil, problem(taskCodeValidation)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, problem(taskCodeValidation)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, problem(taskCodeValidation)
	}
	return raw, nil
}

func chatValidateTriggerPresence(raw map[string]json.RawMessage) error {
	required := []string{"trigger_ref", "account_id", "conversation_kind", "conversation_id", "source_event_id",
		"sender_id", "directed_account_id", "deadline_at", "segment_count"}
	if err := thRequireKeys(raw, required...); err != nil {
		return err
	}
	if err := thRejectNulls(raw, "trigger_ref", "account_id", "conversation_kind", "conversation_id",
		"source_event_id", "sender_id", "deadline_at", "segment_count"); err != nil {
		return err
	}
	var deadline string
	if err := json.Unmarshal(raw["deadline_at"], &deadline); err != nil || !strings.HasSuffix(deadline, "Z") {
		return problem(taskCodeValidation)
	}
	return nil
}

func chatValidatePolicyPresence(raw map[string]json.RawMessage) error {
	if err := thRequireKeys(raw, "enabled", "daily_limit", "expected_version"); err != nil {
		return err
	}
	if err := thRejectNulls(raw, "enabled", "daily_limit"); err != nil {
		return err
	}
	if version, ok := raw["expected_version"]; ok && !thIsJSONNull(version) {
		var value int
		if err := json.Unmarshal(version, &value); err != nil || value < 0 {
			return problem(taskCodeValidation)
		}
	}
	return nil
}

func chatValidateLeasePresence(raw map[string]json.RawMessage) error {
	if err := thRequireKeys(raw, "account_id", "binding_epoch", "lease_expires_at", "expected_version"); err != nil {
		return err
	}
	if err := thRejectNulls(raw, "account_id", "binding_epoch", "lease_expires_at"); err != nil {
		return err
	}
	var epoch, lease string
	if err := json.Unmarshal(raw["binding_epoch"], &epoch); err != nil || epoch == "" {
		return problem(taskCodeValidation)
	}
	if _, err := hbParseDecimal(epoch); err != nil {
		return problem(taskCodeValidation)
	}
	if err := json.Unmarshal(raw["lease_expires_at"], &lease); err != nil || !strings.HasSuffix(lease, "Z") {
		return problem(taskCodeValidation)
	}
	if version, ok := raw["expected_version"]; ok && !thIsJSONNull(version) {
		var value int
		if err := json.Unmarshal(version, &value); err != nil || value < 0 {
			return problem(taskCodeValidation)
		}
	}
	return nil
}
