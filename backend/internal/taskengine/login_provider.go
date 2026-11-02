package taskengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	loginProviderFileMaxBytes = 64 << 10
	loginTokenFileMaxBytes    = 4 << 10
	loginProviderHTTPTimeout  = 10 * time.Second
	loginProviderMaxBody      = 1 << 20
	loginFixtureImage         = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
)

// loginTTLProvider is an optional provider capability. The HTTP handler uses
// it so deployment configuration owns the session/QR deadlines; the interface
// stays out of LoginProvider so a test double needs only the five methods.
type loginTTLProvider interface {
	LoginTTLs() (time.Duration, time.Duration)
}

// LoginFixtureOptions makes the explicit fixture provider deterministic for
// tests and the browser fixture stack. Zero values select safe defaults.
type LoginFixtureOptions struct {
	IdentitySelfID   string
	IdentityNickname string
	// StatusPhases is the per-generation poll progression. It defaults to
	// scanned, authenticating, online and saturates on its last element.
	StatusPhases []LoginPhase
	// QRExpiresIn defaults to the store's QR deadline when zero.
	QRExpiresIn time.Duration
	// StartErr, RefreshErr, StatusErr and IdentityErr inject explicit provider
	// failures; they are only ever used by tests.
	StartErr    error
	RefreshErr  error
	StatusErr   error
	IdentityErr error
}

type loginFixtureProvider struct {
	opts  LoginFixtureOptions
	mu    sync.Mutex
	polls map[string]int
}

// NewLoginFixtureProvider returns the explicit offline provider. It performs no
// network call and never reaches QQ.
func NewLoginFixtureProvider() LoginProvider {
	return NewLoginFixtureProviderWithOptions(LoginFixtureOptions{})
}

// NewLoginFixtureProviderWithOptions is the deterministic fixture used by the
// permanent login regression tests and the browser fixture stack.
func NewLoginFixtureProviderWithOptions(opts LoginFixtureOptions) LoginProvider {
	if len(opts.StatusPhases) == 0 {
		opts.StatusPhases = []LoginPhase{LoginPhaseScanned, LoginPhaseAuthenticating, LoginPhaseOnline}
	}
	return &loginFixtureProvider{opts: opts, polls: map[string]int{}}
}

func (p *loginFixtureProvider) Name() string { return loginProviderFixture }

func (p *loginFixtureProvider) LoginTTLs() (time.Duration, time.Duration) {
	return loginDefaultSessionTTL, loginDefaultQRTTL
}

func (p *loginFixtureProvider) reset(account LoginAccount) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.polls[account.AccountID] = 0
}

func (p *loginFixtureProvider) qr() LoginQR {
	expires := p.opts.QRExpiresIn
	if expires <= 0 {
		expires = loginDefaultQRTTL
	}
	return LoginQR{ImageDataURL: loginFixtureImage, ExpiresAt: time.Now().UTC().Add(expires)}
}

func (p *loginFixtureProvider) Start(_ context.Context, account LoginAccount) (LoginQR, error) {
	if p.opts.StartErr != nil {
		return LoginQR{}, p.opts.StartErr
	}
	p.reset(account)
	return p.qr(), nil
}

func (p *loginFixtureProvider) Refresh(_ context.Context, account LoginAccount) (LoginQR, error) {
	if p.opts.RefreshErr != nil {
		return LoginQR{}, p.opts.RefreshErr
	}
	p.reset(account)
	return p.qr(), nil
}

func (p *loginFixtureProvider) Status(_ context.Context, account LoginAccount) (LoginStatus, error) {
	if p.opts.StatusErr != nil {
		return LoginStatus{}, p.opts.StatusErr
	}
	p.mu.Lock()
	index := p.polls[account.AccountID]
	p.polls[account.AccountID] = index + 1
	p.mu.Unlock()
	if index >= len(p.opts.StatusPhases) {
		index = len(p.opts.StatusPhases) - 1
	}
	return LoginStatus{Phase: p.opts.StatusPhases[index]}, nil
}

func (p *loginFixtureProvider) Identity(_ context.Context, account LoginAccount) (LoginIdentity, error) {
	if p.opts.IdentityErr != nil {
		return LoginIdentity{}, p.opts.IdentityErr
	}
	selfID := p.opts.IdentitySelfID
	if selfID == "" {
		selfID = account.ExpectedSelfID
	}
	if selfID == "" {
		selfID = "10001"
	}
	nickname := p.opts.IdentityNickname
	if nickname == "" {
		nickname = "fixture"
	}
	return LoginIdentity{SelfID: selfID, Nickname: nickname}, nil
}

// napcatLoginProviderInstance is the server-side binding for one adapter
// instance. Token values never leave this struct.
type napcatLoginProviderInstance struct {
	WebUIURL    string
	WebUIToken  string
	OneBotURL   string
	OneBotToken string
}

type napcatLoginProvider struct {
	instances  map[string]napcatLoginProviderInstance
	client     *http.Client
	sessionTTL time.Duration
	qrTTL      time.Duration
	mu         sync.Mutex
	credential map[string]string
}

func (p *napcatLoginProvider) Name() string { return loginProviderNapCat }

func (p *napcatLoginProvider) LoginTTLs() (time.Duration, time.Duration) {
	return p.sessionTTL, p.qrTTL
}

func (p *napcatLoginProvider) instance(account LoginAccount) (napcatLoginProviderInstance, error) {
	instance, ok := p.instances[account.AdapterInstanceID]
	if !ok || instance.WebUIURL == "" || instance.OneBotURL == "" {
		return napcatLoginProviderInstance{}, errors.New("login provider: adapter instance is not configured")
	}
	return instance, nil
}

func (p *napcatLoginProvider) Start(ctx context.Context, account LoginAccount) (LoginQR, error) {
	instance, err := p.instance(account)
	if err != nil {
		return LoginQR{}, err
	}
	return p.fetchQR(ctx, instance, "/QQLogin/GetQQLoginQrcode")
}

func (p *napcatLoginProvider) Refresh(ctx context.Context, account LoginAccount) (LoginQR, error) {
	instance, err := p.instance(account)
	if err != nil {
		return LoginQR{}, err
	}
	if _, err := p.webuiCall(ctx, instance, "/QQLogin/RefreshQRcode"); err != nil {
		return LoginQR{}, err
	}
	return p.fetchQR(ctx, instance, "/QQLogin/GetQQLoginQrcode")
}

type napcatQRData struct {
	QRCode    string `json:"qrcode"`
	QRCodeURL string `json:"qrcodeurl"`
}

func (p *napcatLoginProvider) fetchQR(ctx context.Context, instance napcatLoginProviderInstance, route string) (LoginQR, error) {
	data, err := p.webuiCall(ctx, instance, route)
	if err != nil {
		return LoginQR{}, err
	}
	var qr napcatQRData
	if err := json.Unmarshal(data, &qr); err != nil {
		return LoginQR{}, errors.New("login provider: QR response is malformed")
	}
	image := qr.QRCode
	if image == "" {
		image = qr.QRCodeURL
	}
	if !strings.HasPrefix(image, "data:image/") || len(image) > loginMaxQRImageBytes {
		return LoginQR{}, errors.New("login provider: QR response is not a bounded image")
	}
	return LoginQR{ImageDataURL: image, ExpiresAt: time.Now().UTC().Add(p.qrTTL)}, nil
}

type napcatStatusData struct {
	IsLogin    bool   `json:"isLogin"`
	IsOffline  bool   `json:"isOffline"`
	LoginPhase string `json:"loginPhase"`
	LoginError string `json:"loginError"`
}

func (p *napcatLoginProvider) Status(ctx context.Context, account LoginAccount) (LoginStatus, error) {
	instance, err := p.instance(account)
	if err != nil {
		return LoginStatus{}, err
	}
	data, err := p.webuiCall(ctx, instance, "/QQLogin/CheckLoginStatus")
	if err != nil {
		return LoginStatus{}, err
	}
	var status napcatStatusData
	if err := json.Unmarshal(data, &status); err != nil {
		return LoginStatus{}, errors.New("login provider: status response is malformed")
	}
	if status.IsLogin && !status.IsOffline {
		return LoginStatus{Phase: LoginPhaseOnline}, nil
	}
	switch strings.ToLower(strings.TrimSpace(status.LoginPhase)) {
	case "qrcode_scanned":
		return LoginStatus{Phase: LoginPhaseScanned}, nil
	case "initializing", "reconnecting":
		return LoginStatus{Phase: LoginPhaseAuthenticating}, nil
	case "ready":
		return LoginStatus{Phase: LoginPhaseOnline}, nil
	}
	if status.LoginError != "" {
		lowered := strings.ToLower(status.LoginError)
		for _, marker := range []string{"expired", "过期", "失效", "invalid"} {
			if strings.Contains(lowered, marker) {
				return LoginStatus{Phase: LoginPhaseExpired, Reason: loginFailureQRExpired}, nil
			}
		}
		return LoginStatus{Phase: LoginPhaseFailed, Reason: loginFailureProvider}, nil
	}
	return LoginStatus{Phase: LoginPhaseWaiting}, nil
}

type napcatIdentityEnvelope struct {
	Status  string          `json:"status"`
	Retcode int             `json:"retcode"`
	Data    json.RawMessage `json:"data"`
}

type napcatIdentityData struct {
	UserID   json.RawMessage `json:"user_id"`
	Nickname string          `json:"nickname"`
}

func (p *napcatLoginProvider) Identity(ctx context.Context, account LoginAccount) (LoginIdentity, error) {
	instance, err := p.instance(account)
	if err != nil {
		return LoginIdentity{}, err
	}
	body, err := p.post(ctx, instance.OneBotURL+"/get_login_info", instance.OneBotToken, []byte("{}"))
	if err != nil {
		return LoginIdentity{}, err
	}
	var envelope napcatIdentityEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return LoginIdentity{}, errors.New("login provider: identity response is malformed")
	}
	if envelope.Status != "" && envelope.Status != "ok" {
		return LoginIdentity{}, errors.New("login provider: identity readback was rejected")
	}
	if envelope.Retcode != 0 {
		return LoginIdentity{}, errors.New("login provider: identity readback was rejected")
	}
	var data napcatIdentityData
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		return LoginIdentity{}, errors.New("login provider: identity response is malformed")
	}
	selfID := strings.Trim(string(data.UserID), `"`)
	if !taskValidQQID(selfID) {
		return LoginIdentity{}, errors.New("login provider: identity readback is not a QQ id")
	}
	if taskBlank(data.Nickname) || len(data.Nickname) > 128 {
		return LoginIdentity{}, errors.New("login provider: identity readback has no bounded nickname")
	}
	return LoginIdentity{SelfID: selfID, Nickname: data.Nickname}, nil
}

type napcatEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type napcatAuthData struct {
	Credential string `json:"Credential"`
}

// webuiCall authenticates once, then posts the route with the Bearer session.
// An Unauthorized answer re-authenticates exactly once; there is no retry of
// the QQ-side action and no redirect following.
func (p *napcatLoginProvider) webuiCall(ctx context.Context, instance napcatLoginProviderInstance, route string) (json.RawMessage, error) {
	credential, err := p.ensureCredential(ctx, instance)
	if err != nil {
		return nil, err
	}
	data, unauthorized, err := p.postEnvelope(ctx, instance.WebUIURL+route, credential)
	if err == nil && !unauthorized {
		return data, nil
	}
	if err != nil && !unauthorized {
		return nil, err
	}
	p.forget(instance.WebUIURL)
	credential, err = p.authenticate(ctx, instance)
	if err != nil {
		return nil, err
	}
	data, unauthorized, err = p.postEnvelope(ctx, instance.WebUIURL+route, credential)
	if err != nil {
		return nil, err
	}
	if unauthorized {
		return nil, errors.New("login provider: webui session was rejected")
	}
	return data, nil
}

func (p *napcatLoginProvider) forget(webuiURL string) {
	p.mu.Lock()
	delete(p.credential, webuiURL)
	p.mu.Unlock()
}

func (p *napcatLoginProvider) ensureCredential(ctx context.Context, instance napcatLoginProviderInstance) (string, error) {
	p.mu.Lock()
	credential := p.credential[instance.WebUIURL]
	p.mu.Unlock()
	if credential != "" {
		return credential, nil
	}
	return p.authenticate(ctx, instance)
}

func (p *napcatLoginProvider) authenticate(ctx context.Context, instance napcatLoginProviderInstance) (string, error) {
	hash := sha256.Sum256([]byte(instance.WebUIToken + ".napcat"))
	payload, err := json.Marshal(map[string]string{"hash": hex.EncodeToString(hash[:]), "totpCode": ""})
	if err != nil {
		return "", err
	}
	body, err := p.post(ctx, instance.WebUIURL+"/auth/login", "", payload)
	if err != nil {
		return "", err
	}
	var envelope napcatEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", errors.New("login provider: webui authentication response is malformed")
	}
	if envelope.Code != 0 {
		return "", errors.New("login provider: webui authentication was rejected")
	}
	var data napcatAuthData
	if err := json.Unmarshal(envelope.Data, &data); err != nil || data.Credential == "" {
		return "", errors.New("login provider: webui authentication returned no credential")
	}
	p.mu.Lock()
	if p.credential == nil {
		p.credential = map[string]string{}
	}
	p.credential[instance.WebUIURL] = data.Credential
	p.mu.Unlock()
	return data.Credential, nil
}

func (p *napcatLoginProvider) postEnvelope(ctx context.Context, target, credential string) (json.RawMessage, bool, error) {
	body, err := p.post(ctx, target, credential, []byte("{}"))
	if err != nil {
		var statusErr *loginProviderStatusError
		if errors.As(err, &statusErr) && (statusErr.status == http.StatusUnauthorized || statusErr.status == http.StatusForbidden) {
			return nil, true, nil
		}
		return nil, false, err
	}
	var envelope napcatEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, errors.New("login provider: webui response is malformed")
	}
	if envelope.Code != 0 {
		if strings.EqualFold(strings.TrimSpace(envelope.Message), "Unauthorized") {
			return nil, true, nil
		}
		return nil, false, errors.New("login provider: webui call failed")
	}
	return envelope.Data, false, nil
}

type loginProviderStatusError struct{ status int }

func (e *loginProviderStatusError) Error() string {
	return fmt.Sprintf("login provider: HTTP %d", e.status)
}

func (p *napcatLoginProvider) post(ctx context.Context, target, credential string, payload []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("login provider: request could not be built")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return nil, errors.New("login provider: request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, loginProviderMaxBody))
		return nil, &loginProviderStatusError{status: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, loginProviderMaxBody+1))
	if err != nil || len(body) > loginProviderMaxBody {
		return nil, errors.New("login provider: response is unreadable or too large")
	}
	return body, nil
}

type loginProviderFile struct {
	Mode              string                 `json:"mode"`
	SessionTTLSeconds int                    `json:"session_ttl_seconds"`
	QRTTLSeconds      int                    `json:"qr_ttl_seconds"`
	Instances         []loginProviderFileRow `json:"instances"`
}

type loginProviderFileRow struct {
	AdapterInstanceID string `json:"adapter_instance_id"`
	WebUIURL          string `json:"webui_url"`
	WebUITokenFile    string `json:"webui_token_file"`
	OneBotURL         string `json:"onebot_url"`
	OneBotTokenFile   string `json:"onebot_token_file"`
}

// LoadLoginProvider is the explicit server wiring point. It never reads
// environment variables, never logs secrets and never performs a network call.
// A nil provider means the login routes stay mounted but answer 503
// FEATURE_DISABLED.
func LoadLoginProvider(path string, fixture bool) (LoginProvider, error) {
	if fixture {
		if path != "" {
			return nil, errors.New("login provider: -login-fixture cannot be combined with -login-provider-file")
		}
		return NewLoginFixtureProvider(), nil
	}
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("login provider: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > loginProviderFileMaxBytes {
		return nil, errors.New("login provider: config file must be regular, bounded and mode 0600 or stricter")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("login provider: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config loginProviderFile
	if err := decoder.Decode(&config); err != nil {
		return nil, errors.New("login provider: config file is not valid JSON with known fields")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("login provider: config file must contain exactly one JSON value")
	}
	if config.Mode != loginProviderNapCat {
		return nil, errors.New(`login provider: mode must be "napcat"`)
	}
	if len(config.Instances) == 0 {
		return nil, errors.New("login provider: at least one adapter instance is required")
	}
	sessionTTL := time.Duration(config.SessionTTLSeconds) * time.Second
	if config.SessionTTLSeconds == 0 {
		sessionTTL = loginDefaultSessionTTL
	}
	if sessionTTL < time.Second || sessionTTL > time.Hour {
		return nil, errors.New("login provider: session_ttl_seconds must be 1..3600")
	}
	qrTTL := time.Duration(config.QRTTLSeconds) * time.Second
	if config.QRTTLSeconds == 0 {
		qrTTL = loginDefaultQRTTL
	}
	if qrTTL < time.Second || qrTTL > 10*time.Minute || qrTTL > sessionTTL {
		return nil, errors.New("login provider: qr_ttl_seconds must be 1..600 and not exceed the session TTL")
	}
	provider := &napcatLoginProvider{
		instances:  map[string]napcatLoginProviderInstance{},
		client:     &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: loginProviderHTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		sessionTTL: sessionTTL,
		qrTTL:      qrTTL,
		credential: map[string]string{},
	}
	for index, row := range config.Instances {
		if !loginSessionIdentifier(row.AdapterInstanceID) {
			return nil, fmt.Errorf("login provider: instance %d has an invalid adapter_instance_id", index)
		}
		if _, duplicate := provider.instances[row.AdapterInstanceID]; duplicate {
			return nil, fmt.Errorf("login provider: instance %d repeats adapter_instance_id", index)
		}
		webuiURL, err := loginProviderURL(row.WebUIURL, true)
		if err != nil {
			return nil, fmt.Errorf("login provider: instance %d webui_url: %w", index, err)
		}
		onebotURL, err := loginProviderURL(row.OneBotURL, false)
		if err != nil {
			return nil, fmt.Errorf("login provider: instance %d onebot_url: %w", index, err)
		}
		webuiToken, err := loginReadTokenFile(row.WebUITokenFile)
		if err != nil {
			return nil, fmt.Errorf("login provider: instance %d webui token: %w", index, err)
		}
		onebotToken, err := loginReadTokenFile(row.OneBotTokenFile)
		if err != nil {
			return nil, fmt.Errorf("login provider: instance %d onebot token: %w", index, err)
		}
		provider.instances[row.AdapterInstanceID] = napcatLoginProviderInstance{
			WebUIURL:    webuiURL,
			WebUIToken:  webuiToken,
			OneBotURL:   onebotURL,
			OneBotToken: onebotToken,
		}
	}
	return provider, nil
}

func loginProviderURL(raw string, requireAPI bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("must be a plain URL without userinfo, query or fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("must use http or https")
	}
	host := parsed.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return "", errors.New("must target a loopback host")
	}
	trimmed := strings.TrimRight(parsed.Path, "/")
	if requireAPI && !strings.HasSuffix(trimmed, "/api") {
		return "", errors.New("path must end with /api")
	}
	if strings.Contains(trimmed, "..") {
		return "", errors.New("path must not traverse")
	}
	parsed.Path = trimmed
	return parsed.String(), nil
}

func loginReadTokenFile(path string) (string, error) {
	if taskBlank(path) {
		return "", errors.New("token file is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("token file is not readable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > loginTokenFileMaxBytes {
		return "", errors.New("token file must be regular, bounded and mode 0600 or stricter")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("token file is not readable")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || len(token) > loginTokenFileMaxBytes || strings.ContainsAny(token, "\r\n\x00") {
		return "", errors.New("token file must contain one bounded token line")
	}
	return token, nil
}
