package taskengine

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/oapi-codegen/nullable"
)

const (
	loginStatusPath        = "/v1/login/status"
	loginAccountsPath      = "/v1/accounts/"
	loginSessionsPath      = "/v1/login-sessions/"
	loginCreatePathSuffix  = "/login-sessions"
	loginMaxCachedQRImages = 64
)

type loginQRArtifact struct {
	Generation   int64
	ImageDataURL string
	ExpiresAt    time.Time
}

type loginOptions struct {
	sessionTTL time.Duration
	qrTTL      time.Duration
}

// loginHTTP is the console login surface. It is never mounted by Store.Handler:
// the caller must wire it explicitly with trusted operator authentication and a
// provider, mirroring ModelHandler. A nil provider keeps the routes mounted and
// answers 503 FEATURE_DISABLED, so a browser can show an explicit unavailable
// state instead of a fabricated success.
type loginHTTP struct {
	store      *Store
	auth       func(*http.Request) (Actor, error)
	provider   LoginProvider
	sessionTTL time.Duration
	qrTTL      time.Duration

	mu sync.Mutex
	qr map[string]loginQRArtifact
}

// LoginHandler returns the durable login-session surface. The QR image lives
// only in this handler's bounded memory: the database keeps the state machine
// and deadlines, never the short-lived login challenge.
func (s *Store) LoginHandler(auth func(*http.Request) (Actor, error), provider LoginProvider) http.Handler {
	handler := &loginHTTP{
		store:      s,
		auth:       auth,
		provider:   provider,
		sessionTTL: loginDefaultSessionTTL,
		qrTTL:      loginDefaultQRTTL,
		qr:         map[string]loginQRArtifact{},
	}
	if ttlProvider, ok := provider.(loginTTLProvider); ok {
		sessionTTL, qrTTL := ttlProvider.LoginTTLs()
		if sessionTTL > 0 {
			handler.sessionTTL = sessionTTL
		}
		if qrTTL > 0 {
			handler.qrTTL = qrTTL
		}
	}
	return handler
}

type loginGenerationRequest struct {
	ExpectedGeneration *int64 `json:"expected_generation"`
}

type loginStatusView struct {
	Provider          string `json:"provider"`
	Fixture           bool   `json:"fixture"`
	Live              bool   `json:"live"`
	SessionTTLSeconds int    `json:"session_ttl_seconds"`
	QRTTLSeconds      int    `json:"qr_ttl_seconds"`
	IdentityReadback  string `json:"identity_readback"`
}

type loginStatusEnvelope struct {
	Data loginStatusView      `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

type loginSessionEnvelope struct {
	Data LoginSessionView     `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

type loginSessionPage struct {
	Items []LoginSessionView `json:"items"`
}

type loginSessionPageEnvelope struct {
	Data loginSessionPage     `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

func (h *loginHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	actor, err := h.authenticate(r)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	switch {
	case r.URL.Path == loginStatusPath:
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, "GET")
			return
		}
		h.status(w, r)
		return
	case strings.HasPrefix(r.URL.Path, loginAccountsPath):
		rest := strings.TrimPrefix(r.URL.Path, loginAccountsPath)
		parts := strings.Split(rest, "/")
		if len(parts) != 2 || parts[1] != "login-sessions" || !loginSessionIdentifier(parts[0]) {
			loginWriteError(w, r, problem(taskCodeNotFound))
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.list(w, r, actor, parts[0])
		case http.MethodPost:
			h.create(w, r, actor, parts[0])
		default:
			thMethodNotAllowed(w, r, "GET, POST")
		}
		return
	case strings.HasPrefix(r.URL.Path, loginSessionsPath):
		rest := strings.TrimPrefix(r.URL.Path, loginSessionsPath)
		parts := strings.Split(rest, "/")
		if len(parts) == 1 && loginSessionIdentifier(parts[0]) {
			if r.Method != http.MethodGet {
				thMethodNotAllowed(w, r, "GET")
				return
			}
			h.get(w, r, actor, parts[0])
			return
		}
		if len(parts) != 2 || !loginSessionIdentifier(parts[0]) {
			loginWriteError(w, r, problem(taskCodeNotFound))
			return
		}
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, "POST")
			return
		}
		switch parts[1] {
		case "qr":
			h.issueQR(w, r, actor, parts[0])
		case "poll":
			h.poll(w, r, actor, parts[0])
		case "cancel":
			h.cancel(w, r, actor, parts[0])
		default:
			loginWriteError(w, r, problem(taskCodeNotFound))
		}
		return
	default:
		loginWriteError(w, r, problem(taskCodeNotFound))
	}
}

func (h *loginHTTP) authenticate(r *http.Request) (Actor, error) {
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

func (h *loginHTTP) options() loginOptions {
	return loginOptions{sessionTTL: h.sessionTTL, qrTTL: h.qrTTL}
}

func (h *loginHTTP) status(w http.ResponseWriter, r *http.Request) {
	view := loginStatusView{
		Provider:          loginProviderDisabled,
		IdentityReadback:  "none",
		SessionTTLSeconds: int(h.sessionTTL / time.Second),
		QRTTLSeconds:      int(h.qrTTL / time.Second),
	}
	if h.provider != nil {
		view.Provider = h.provider.Name()
		switch view.Provider {
		case loginProviderFixture:
			view.Fixture = true
			view.IdentityReadback = "fixture"
		case loginProviderNapCat:
			view.Live = true
			view.IdentityReadback = "onebot_get_login_info"
		}
	}
	thWriteJSON(w, http.StatusOK, loginStatusEnvelope{Data: view, Meta: thMeta(r)})
}

func (h *loginHTTP) get(w http.ResponseWriter, r *http.Request, actor Actor, id string) {
	row, err := h.store.loginReadSession(r.Context(), actor, id)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	view := loginSessionView(row, time.Now().UTC())
	h.decorate(&view)
	thWriteJSON(w, http.StatusOK, loginSessionEnvelope{Data: view, Meta: thMeta(r)})
}

func (h *loginHTTP) list(w http.ResponseWriter, r *http.Request, actor Actor, accountID string) {
	items, err := h.store.loginList(r.Context(), actor, accountID)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	for index := range items {
		h.decorate(&items[index])
	}
	thWriteJSON(w, http.StatusOK, loginSessionPageEnvelope{Data: loginSessionPage{Items: items}, Meta: thMeta(r)})
}

func (h *loginHTTP) create(w http.ResponseWriter, r *http.Request, actor Actor, accountID string) {
	if h.provider == nil {
		loginWriteError(w, r, problem(taskCodeFeatureDisabled))
		return
	}
	key, err := thIdempotencyKey(r)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	bodyDigest, err := loginDecodeEmptyBody(w, r)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	view, err := h.store.loginCreate(r.Context(), actor, h.provider, accountID, r.URL.Path, key, bodyDigest, h.options())
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	h.decorate(&view)
	thWriteJSON(w, http.StatusCreated, loginSessionEnvelope{Data: view, Meta: thMeta(r)})
}

func (h *loginHTTP) issueQR(w http.ResponseWriter, r *http.Request, actor Actor, id string) {
	if h.provider == nil {
		loginWriteError(w, r, problem(taskCodeFeatureDisabled))
		return
	}
	key, err := thIdempotencyKey(r)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	generation, bodyDigest, err := loginDecodeGeneration(w, r)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	view, artifact, err := h.store.loginIssueQR(r.Context(), actor, h.provider, id, r.URL.Path, key, bodyDigest, generation, h.options())
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	if artifact != nil {
		h.putQR(id, *artifact)
	}
	h.decorate(&view)
	thWriteJSON(w, http.StatusOK, loginSessionEnvelope{Data: view, Meta: thMeta(r)})
}

func (h *loginHTTP) poll(w http.ResponseWriter, r *http.Request, actor Actor, id string) {
	if h.provider == nil {
		loginWriteError(w, r, problem(taskCodeFeatureDisabled))
		return
	}
	key, err := thIdempotencyKey(r)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	generation, bodyDigest, err := loginDecodeGeneration(w, r)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	view, err := h.store.loginPoll(r.Context(), actor, h.provider, id, r.URL.Path, key, bodyDigest, generation)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	h.dropTerminal(&view)
	h.decorate(&view)
	thWriteJSON(w, http.StatusOK, loginSessionEnvelope{Data: view, Meta: thMeta(r)})
}

func (h *loginHTTP) cancel(w http.ResponseWriter, r *http.Request, actor Actor, id string) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	bodyDigest, err := loginDecodeEmptyBody(w, r)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	view, err := h.store.loginCancel(r.Context(), actor, id, r.URL.Path, key, bodyDigest)
	if err != nil {
		loginWriteError(w, r, err)
		return
	}
	h.dropQR(id)
	thWriteJSON(w, http.StatusOK, loginSessionEnvelope{Data: view, Meta: thMeta(r)})
}

// loginDecodeEmptyBody accepts exactly `{}` for create/cancel. The digest is
// the stable empty-object digest used by the idempotency record.
func loginDecodeEmptyBody(w http.ResponseWriter, r *http.Request) (string, error) {
	var body struct{}
	raw, err := thDecodeStrict(w, r, &body)
	if err != nil {
		return "", err
	}
	if len(raw) != 0 {
		return "", problem(taskCodeValidation)
	}
	return digest(body)
}

func loginDecodeGeneration(w http.ResponseWriter, r *http.Request) (int64, string, error) {
	var body loginGenerationRequest
	raw, err := thDecodeStrict(w, r, &body)
	if err != nil {
		return 0, "", err
	}
	if len(raw) != 1 || body.ExpectedGeneration == nil {
		return 0, "", problem(taskCodeValidation)
	}
	if *body.ExpectedGeneration < 0 {
		return 0, "", problem(taskCodeValidation)
	}
	bodyDigest, err := digest(map[string]int64{"expected_generation": *body.ExpectedGeneration})
	return *body.ExpectedGeneration, bodyDigest, err
}

func (h *loginHTTP) decorate(view *LoginSessionView) {
	view.QRAvailable = false
	view.QRPNGDataURL = nil
	if view.State != loginStateQRReady && view.State != loginStateScanned && view.State != loginStateAuthenticating {
		return
	}
	if view.QRExpiresAt == nil || time.Now().UTC().After(*view.QRExpiresAt) {
		return
	}
	h.mu.Lock()
	entry, found := h.qr[view.ID]
	h.mu.Unlock()
	if !found || entry.Generation != view.Generation || time.Now().UTC().After(entry.ExpiresAt) {
		return
	}
	image := entry.ImageDataURL
	view.QRPNGDataURL = &image
	view.QRAvailable = true
}

func (h *loginHTTP) putQR(id string, artifact loginQRArtifact) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now().UTC()
	for key, entry := range h.qr {
		if now.After(entry.ExpiresAt) {
			delete(h.qr, key)
		}
	}
	for len(h.qr) >= loginMaxCachedQRImages {
		for key := range h.qr {
			if key != id {
				delete(h.qr, key)
				break
			}
			return
		}
	}
	h.qr[id] = artifact
}

func (h *loginHTTP) dropQR(id string) {
	h.mu.Lock()
	delete(h.qr, id)
	h.mu.Unlock()
}

func (h *loginHTTP) dropTerminal(view *LoginSessionView) {
	if loginSessionTerminal(view.State) {
		h.dropQR(view.ID)
	}
}

// loginStatusForCode maps login failures to explicit HTTP statuses. The task
// table maps FEATURE_DISABLED to 403, but the login surface must report 503 so
// the console can render "provider not configured" instead of an authorization
// fault; provider outages are 502, not a client conflict.
func loginStatusForCode(code string) int {
	switch code {
	case taskCodeFeatureDisabled:
		return http.StatusServiceUnavailable
	case loginCodeProviderFailed:
		return http.StatusBadGateway
	case loginCodeGenerationStale, loginCodeStateConflict, loginCodeSessionActive,
		loginCodeIdentityMismatch, loginCodeIdentityAlreadyUsed, taskCodeIdempotency:
		return http.StatusConflict
	case taskCodeUnauthenticated:
		return http.StatusUnauthorized
	case taskCodeForbidden:
		return http.StatusForbidden
	case taskCodeNotFound:
		return http.StatusNotFound
	case taskCodeValidation:
		return http.StatusBadRequest
	case taskCodeUnsupportedMedia:
		return http.StatusUnsupportedMediaType
	case taskCodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case taskCodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	}
	return thStatusForCode(code)
}

var loginSafeMessages = map[string]string{
	taskCodeFeatureDisabled:      "login provider is not configured on this server",
	loginCodeProviderFailed:      "login provider request failed",
	loginCodeGenerationStale:     "login session generation is stale; re-read the session",
	loginCodeStateConflict:       "login session state does not allow this action",
	loginCodeSessionActive:       "an open login session already exists for this account",
	loginCodeIdentityMismatch:    "verified QQ identity does not match the account binding",
	loginCodeIdentityAlreadyUsed: "verified QQ identity is already bound to another account",
	taskCodeIdempotency:          "idempotency key was reused with a different request",
	taskCodeUnauthenticated:      "authentication required",
	taskCodeForbidden:            "action forbidden",
	taskCodeNotFound:             "resource not found",
	taskCodeValidation:           "request validation failed",
	taskCodeUnsupportedMedia:     "unsupported content type",
	taskCodePayloadTooLarge:      "request body too large",
	taskCodeMethodNotAllowed:     "method not allowed",
}

func loginWriteError(w http.ResponseWriter, r *http.Request, err error) {
	code := Code(err)
	if code == "" {
		code = taskCodeStoreUnavailable
	}
	message, ok := loginSafeMessages[code]
	if !ok {
		message, ok = thSafeMessages[code]
	}
	if !ok {
		message = "request failed"
	}
	thWriteJSON(w, loginStatusForCode(code), contract.ErrorEnvelope{
		Error: contract.Error{
			Code:         code,
			Details:      map[string]contract.Error_Details_AdditionalProperties{},
			Message:      message,
			RetryAfterMs: nullable.NewNullNullable[int](),
			Retryable:    false,
		},
		Meta: thMeta(r),
	})
}
