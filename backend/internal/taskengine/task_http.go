package taskengine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/oapi-codegen/nullable"

	"bba.local/taskengine/internal/contract"
)

// maxTaskBodyBytes bounds every task mutation body. The largest valid fixed
// import (5000 candidates with 128-byte IDs) fits comfortably; anything larger
// is rejected before decoding rather than buffered without limit.
const maxTaskBodyBytes = 4 << 20

type taskHTTP struct {
	store *Store
	auth  func(*http.Request) (Actor, error)
}

// Handler returns the narrow task surface only. Authentication is supplied by
// a trusted resolver; this handler never parses credentials or invents a user.
func (s *Store) Handler(auth func(*http.Request) (Actor, error)) http.Handler {
	return &taskHTTP{store: s, auth: auth}
}

func (h *taskHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Authentication runs before routing and before any idempotent replay is
	// considered, so a cached response can never be read unauthenticated.
	actor, err := h.authenticate(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}

	switch {
	case r.URL.Path == "/v1/accounts":
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, "GET")
			return
		}
		h.listAccounts(w, r, actor)
	case r.URL.Path == taskCreatePath:
		if r.Method == http.MethodGet {
			h.listTasks(w, r, actor)
			return
		}
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, "GET, POST")
			return
		}
		h.createTask(w, r, actor)
	case strings.HasPrefix(r.URL.Path, "/v1/tasks/"):
		rest := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
		if strings.HasSuffix(rest, "/graph") {
			taskID := strings.TrimSuffix(rest, "/graph")
			if !taskValidID(taskID) || strings.Contains(taskID, "/") {
				thWriteError(w, r, problem(taskCodeNotFound))
				return
			}
			if r.Method != http.MethodGet {
				thMethodNotAllowed(w, r, "GET")
				return
			}
			h.getTaskGraph(w, r, actor, taskID)
			return
		}
		if strings.HasSuffix(rest, "/controls") {
			taskID := strings.TrimSuffix(rest, "/controls")
			if !taskValidID(taskID) || strings.Contains(taskID, "/") {
				thWriteError(w, r, problem(taskCodeNotFound))
				return
			}
			if r.Method != http.MethodPost {
				thMethodNotAllowed(w, r, "POST")
				return
			}
			h.controlTask(w, r, actor, taskID)
			return
		}
		if strings.HasSuffix(rest, "/reconcile") {
			taskID := strings.TrimSuffix(rest, "/reconcile")
			if !taskValidID(taskID) || strings.Contains(taskID, "/") {
				thWriteError(w, r, problem(taskCodeNotFound))
				return
			}
			if r.Method != http.MethodPost {
				thMethodNotAllowed(w, r, "POST")
				return
			}
			h.reconcileTask(w, r, actor, taskID)
			return
		}
		if !taskValidID(rest) || strings.Contains(rest, "/") {
			thWriteError(w, r, problem(taskCodeNotFound))
			return
		}
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, "GET")
			return
		}
		h.getTask(w, r, actor, rest)
	default:
		thWriteError(w, r, problem(taskCodeNotFound))
	}
}

func (h *taskHTTP) listAccounts(w http.ResponseWriter, r *http.Request, actor Actor) {
	cursor, limit, err := p019PageValues(r.URL.Query())
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	items, next, more, err := h.store.ListAccounts(r.Context(), actor, cursor, limit)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	meta := thMeta(r)
	meta.HasMore = more
	if more {
		meta.NextCursor.Set(next)
	}
	thWriteJSON(w, http.StatusOK, contract.AccountPageEnvelope{Data: contract.AccountPage{Items: items}, Meta: meta})
}

func (h *taskHTTP) listTasks(w http.ResponseWriter, r *http.Request, actor Actor) {
	if actor.RuntimeID != "" {
		thWriteError(w, r, problem(taskCodeForbidden))
		return
	}
	cursor, limit, err := p019PageValues(r.URL.Query())
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	items, next, more, err := h.store.ListTasks(r.Context(), actor, cursor, limit)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	meta := thMeta(r)
	meta.HasMore = more
	if more {
		meta.NextCursor.Set(next)
	}
	thWriteJSON(w, http.StatusOK, contract.TaskPageEnvelope{Data: contract.TaskPage{Items: items}, Meta: meta})
}

func (h *taskHTTP) reconcileTask(w http.ResponseWriter, r *http.Request, actor Actor, taskID string) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	var req contract.ReconcileRequest
	raw, err := thDecodeStrict(w, r, &req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := thValidateReconcilePresence(raw); err != nil {
		thWriteError(w, r, err)
		return
	}
	receipt, err := h.store.ReconcileTask(r.Context(), actor, taskID, key, req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusAccepted, contract.ReceiptEnvelope{Data: receipt, Meta: thMeta(r)})
}

func (h *taskHTTP) authenticate(r *http.Request) (Actor, error) {
	if h.auth == nil {
		return Actor{}, problem(taskCodeUnauthenticated)
	}
	actor, err := h.auth(r)
	if err != nil {
		return Actor{}, err
	}
	if taskBlank(actor.WorkspaceID) || taskBlank(actor.PrincipalID) {
		return Actor{}, problem(taskCodeUnauthenticated)
	}
	if actor.RuntimeID != "" {
		// Runtime authors are task-entry identities. The trusted resolver owns
		// credential/session verification; this handler additionally requires
		// the originating account and sender from the authenticated event.
		accountID := r.Header.Get("X-QQ-Account-ID")
		senderID := r.Header.Get("X-QQ-Sender-ID")
		if taskBlank(accountID) || taskBlank(senderID) || taskBlank(actor.SenderID) ||
			senderID != actor.SenderID || !contains(actor.AccountIDs, accountID) {
			return Actor{}, problem(taskCodeUnauthenticated)
		}
	}
	return actor, nil
}

func (h *taskHTTP) createTask(w http.ResponseWriter, r *http.Request, actor Actor) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	var req contract.CreateTaskRequest
	raw, err := thDecodeStrict(w, r, &req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := thValidateCreatePresence(raw); err != nil {
		thWriteError(w, r, err)
		return
	}
	task, err := h.store.CreateTask(r.Context(), actor, key, req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusCreated, contract.TaskEnvelope{Data: task, Meta: thMeta(r)})
}

func (h *taskHTTP) getTask(w http.ResponseWriter, r *http.Request, actor Actor, taskID string) {
	task, err := h.store.GetTask(r.Context(), actor, taskID)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, contract.TaskEnvelope{Data: task, Meta: thMeta(r)})
}
func (h *taskHTTP) getTaskGraph(w http.ResponseWriter, r *http.Request, actor Actor, taskID string) {
	graph, err := h.store.GetTaskGraph(r.Context(), actor, taskID)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, struct {
		Data TaskGraph            `json:"data"`
		Meta contract.RequestMeta `json:"meta"`
	}{Data: graph, Meta: thMeta(r)})
}

func (h *taskHTTP) controlTask(w http.ResponseWriter, r *http.Request, actor Actor, taskID string) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	var req contract.TaskControlRequest
	raw, err := thDecodeStrict(w, r, &req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := thValidateControlPresence(raw); err != nil {
		thWriteError(w, r, err)
		return
	}
	receipt, err := h.store.ControlTask(r.Context(), actor, taskID, key, req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusAccepted, contract.ReceiptEnvelope{Data: receipt, Meta: thMeta(r)})
}

func thIdempotencyKey(r *http.Request) (string, error) {
	key := r.Header.Get("Idempotency-Key")
	if taskBlank(key) || len(key) > 128 {
		return "", problem(taskCodeValidation)
	}
	return key, nil
}

func thDecodeStrict(w http.ResponseWriter, r *http.Request, dst any) (map[string]json.RawMessage, error) {
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
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskBodyBytes)
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

func thIsJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func thRequireKeys(raw map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		if _, ok := raw[key]; !ok {
			return problem(taskCodeValidation)
		}
	}
	return nil
}

func thRejectNulls(raw map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		if value, ok := raw[key]; ok && thIsJSONNull(value) {
			return problem(taskCodeValidation)
		}
	}
	return nil
}

func thRawObject(raw map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw[key], &object); err != nil || object == nil {
		return nil, problem(taskCodeValidation)
	}
	return object, nil
}

// thValidateCreatePresence enforces the required-member and non-nullable rules
// that generated types cannot express. approval_ref is required but nullable;
// every other required member must be present and non-null.
func thValidateCreatePresence(raw map[string]json.RawMessage) error {
	if err := thRequireKeys(raw, "type", "title", "join_reason", "verification_message", "goal", "allowed_account_ids",
		"candidate_source", "deadline_at", "limits", "approval_ref", "submit"); err != nil {
		return err
	}
	if err := thRejectNulls(raw, "type", "title", "join_reason", "verification_message", "goal", "allowed_account_ids",
		"candidate_source", "deadline_at", "limits", "submit"); err != nil {
		return err
	}
	if err := thRejectNulls(raw, "join_reason", "verification_message"); err != nil {
		return err
	}
	if err := thRejectNulls(raw, "execution_mode", "keywords"); err != nil {
		return err
	}
	// UTCDateTime requires the trailing Z on the wire; a local offset is not a
	// contract-valid value even though time.Time itself would decode it.
	var deadline string
	if err := json.Unmarshal(raw["deadline_at"], &deadline); err != nil || !strings.HasSuffix(deadline, "Z") {
		return problem(taskCodeValidation)
	}

	goal, err := thRawObject(raw, "goal")
	if err != nil {
		return err
	}
	if err := thRequireKeys(goal, "metric", "count", "strict_target_cap", "require_final_membership_check"); err != nil {
		return err
	}
	if err := thRejectNulls(goal, "metric", "count", "strict_target_cap", "require_final_membership_check"); err != nil {
		return err
	}

	limits, err := thRawObject(raw, "limits")
	if err != nil {
		return err
	}
	if err := thRequireKeys(limits, "max_candidates", "max_attempts_per_action",
		"max_total_external_attempts", "max_model_attempts"); err != nil {
		return err
	}
	if err := thRejectNulls(limits, "max_candidates", "max_attempts_per_action",
		"max_total_external_attempts", "max_model_attempts", "max_search_pages_per_keyword"); err != nil {
		return err
	}

	source, err := thRawObject(raw, "candidate_source")
	if err != nil {
		return err
	}
	if err := thRequireKeys(source, "kind", "groups"); err != nil {
		return err
	}
	if err := thRejectNulls(source, "kind", "groups"); err != nil {
		return err
	}
	var groups []json.RawMessage
	if err := json.Unmarshal(source["groups"], &groups); err != nil || groups == nil {
		return problem(taskCodeValidation)
	}
	for _, groupRaw := range groups {
		var group map[string]json.RawMessage
		if err := json.Unmarshal(groupRaw, &group); err != nil || group == nil {
			return problem(taskCodeValidation)
		}
		if err := thRequireKeys(group, "group_id", "source_ref"); err != nil {
			return err
		}
		if err := thRejectNulls(group, "group_id", "source_ref"); err != nil {
			return err
		}
	}
	return nil
}

func thValidateControlPresence(raw map[string]json.RawMessage) error {
	if err := thRequireKeys(raw, "action", "expected_version", "reason"); err != nil {
		return err
	}
	return thRejectNulls(raw, "action", "expected_version", "reason")
}

func thValidateReconcilePresence(raw map[string]json.RawMessage) error {
	if err := thRequireKeys(raw, "expected_version", "reason"); err != nil {
		return err
	}
	return thRejectNulls(raw, "expected_version", "reason")
}
func thMeta(r *http.Request) contract.RequestMeta {
	requestID := r.Header.Get("X-Request-Id")
	if taskBlank(requestID) || len(requestID) > 128 {
		requestID = newID("req")
	}
	return contract.RequestMeta{
		HasMore:    false,
		NextCursor: nullable.NewNullNullable[string](),
		RequestId:  requestID,
	}
}

func thWriteJSON(w http.ResponseWriter, status int, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func thMethodNotAllowed(w http.ResponseWriter, r *http.Request, allowed string) {
	w.Header().Set("Allow", allowed)
	thWriteErrorCode(w, r, taskCodeMethodNotAllowed, http.StatusMethodNotAllowed)
}

func thWriteError(w http.ResponseWriter, r *http.Request, err error) {
	code := Code(err)
	status := thStatusForCode(code)
	if code == "" {
		code = taskCodeStoreUnavailable
		status = http.StatusInternalServerError
	}
	thWriteErrorCode(w, r, code, status)
}

func thStatusForCode(code string) int {
	if status, ok := chatErrorStatus[code]; ok {
		return status
	}
	switch code {
	case taskCodeValidation:
		return http.StatusBadRequest
	case taskCodeUnauthenticated:
		return http.StatusUnauthorized
	case taskCodeForbidden:
		return http.StatusForbidden
	case taskCodeNotFound:
		return http.StatusNotFound
	case taskCodeVersionConflict, taskCodeIdempotency, taskCodeStateConflict, "VERSION_CONFLICT", "HEARTBEAT_SEQUENCE_CONFLICT", "CONSTRAINT_CONFLICT", "STATE_CONSTRAINT", "TRANSACTION_CONFLICT":
		return http.StatusConflict
	case "EXTERNAL_OUTCOME_UNKNOWN":
		return http.StatusConflict
	case taskCodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case taskCodeUnsupportedMedia:
		return http.StatusUnsupportedMediaType
	case taskCodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case taskCodeCapabilityUnavailable, taskCodeFeatureDisabled:
		return http.StatusForbidden
	case taskCodeCursorExpired:
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}

var thSafeMessages = map[string]string{
	taskCodeValidation:            "request validation failed",
	taskCodeUnauthenticated:       "authentication required",
	taskCodeForbidden:             "action forbidden",
	taskCodeNotFound:              "resource not found",
	taskCodeVersionConflict:       "task version conflict",
	taskCodeIdempotency:           "idempotency key was reused with a different request",
	taskCodeStateConflict:         "task state does not allow this action",
	"HEARTBEAT_SEQUENCE_CONFLICT": "heartbeat sequence was reused with a different payload",
	taskCodePayloadTooLarge:       "request body too large",
	taskCodeUnsupportedMedia:      "unsupported content type",
	taskCodeMethodNotAllowed:      "method not allowed",
	"EXTERNAL_OUTCOME_UNKNOWN":    "external outcome is unknown; do not resend",
}

func thWriteErrorCode(w http.ResponseWriter, r *http.Request, code string, status int) {
	message, ok := thSafeMessages[code]
	if !ok {
		message, ok = chatSafeMessages[code]
	}
	if !ok {
		message = "request failed"
	}
	thWriteJSON(w, status, contract.ErrorEnvelope{
		Error: contract.Error{
			Code:         code,
			Details:      map[string]contract.Error_Details_AdditionalProperties{},
			Message:      message,
			RetryAfterMs: nullable.NewNullNullable[int](),
			Retryable:    code == taskCodeStoreUnavailable,
		},
		Meta: thMeta(r),
	})
}
