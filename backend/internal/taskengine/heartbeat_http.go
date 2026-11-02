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

// maxHeartbeatBodyBytes bounds the runtime heartbeat body. The contract's
// largest valid heartbeat (eight capability declarations) fits comfortably;
// anything larger is rejected before decoding instead of buffered.
const maxHeartbeatBodyBytes = 64 << 10

const heartbeatPathPrefix = "/v1/internal/runtimes/"
const heartbeatPathSuffix = "/heartbeats"

type runtimeHTTP struct {
	store *Store
	auth  func(*http.Request) (RuntimeActor, error)
}

// RuntimeHandler exposes only the runtime heartbeat surface. Credential and
// session verification belong to the trusted resolver; this handler never
// parses tokens and never invents a runtime identity.
func (s *Store) RuntimeHandler(auth func(*http.Request) (RuntimeActor, error)) http.Handler {
	return &runtimeHTTP{store: s, auth: auth}
}

func (h *runtimeHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	actor, err := h.authenticate(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if !strings.HasPrefix(r.URL.Path, heartbeatPathPrefix) || !strings.HasSuffix(r.URL.Path, heartbeatPathSuffix) {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	runtimeID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, heartbeatPathPrefix), heartbeatPathSuffix)
	if !taskValidID(runtimeID) || strings.Contains(runtimeID, "/") {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	if r.Method != http.MethodPost {
		thMethodNotAllowed(w, r, "POST")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if taskBlank(key) || len(key) > 128 {
		thWriteError(w, r, problem(taskCodeValidation))
		return
	}
	var req contract.HeartbeatRequest
	raw, err := hbDecodeStrict(w, r, &req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := hbValidatePresence(raw); err != nil {
		thWriteError(w, r, err)
		return
	}
	// Validate the raw nested capability documents. Decoding into generated
	// types first would turn a missing/null required boolean into false.
	if err := hbValidateCapabilities(raw["capabilities"]); err != nil {
		thWriteError(w, r, problem(taskCodeValidation))
		return
	}
	receipt, err := h.store.ReceiveHeartbeat(r.Context(), actor, runtimeID, key, req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusAccepted, contract.HeartbeatEnvelope{Data: receipt, Meta: thMeta(r)})
}

func (h *runtimeHTTP) authenticate(r *http.Request) (RuntimeActor, error) {
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

func hbDecodeStrict(w http.ResponseWriter, r *http.Request, dst any) (map[string]json.RawMessage, error) {
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
	r.Body = http.MaxBytesReader(w, r.Body, maxHeartbeatBodyBytes)
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

// hbValidatePresence enforces the required-member rules the generated types
// cannot express, plus the wire-level decimal-string and UTC-Z requirements.
func hbValidatePresence(raw map[string]json.RawMessage) error {
	required := []string{"workspace_id", "runtime_id", "account_id", "owner_epoch", "sequence",
		"connection_state", "login_state", "heartbeat_at", "capabilities"}
	if err := thRequireKeys(raw, required...); err != nil {
		return err
	}
	if err := thRejectNulls(raw, required...); err != nil {
		return err
	}
	var heartbeatAt string
	if err := json.Unmarshal(raw["heartbeat_at"], &heartbeatAt); err != nil || !strings.HasSuffix(heartbeatAt, "Z") {
		return problem(taskCodeValidation)
	}
	for _, field := range []string{"owner_epoch", "sequence"} {
		var decimal string
		if err := json.Unmarshal(raw[field], &decimal); err != nil {
			return problem(taskCodeValidation)
		}
		if _, err := hbParseDecimal(decimal); err != nil {
			return problem(taskCodeValidation)
		}
	}
	var capabilities []json.RawMessage
	if err := json.Unmarshal(raw["capabilities"], &capabilities); err != nil || capabilities == nil {
		return problem(taskCodeValidation)
	}
	if len(capabilities) > heartbeatMaxCapabilities {
		return problem(taskCodeValidation)
	}
	return nil
}
