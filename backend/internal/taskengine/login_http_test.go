package taskengine

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func loginHTTPHandler(s *Store, actor Actor, provider LoginProvider) http.Handler {
	return s.LoginHandler(func(*http.Request) (Actor, error) { return actor, nil }, provider)
}

func loginEnvelope(t *testing.T, recorder interface{ Bytes() []byte }) loginSessionEnvelope {
	t.Helper()
	var envelope loginSessionEnvelope
	if err := json.Unmarshal(recorder.Bytes(), &envelope); err != nil {
		t.Fatalf("decode login envelope: %v (body %q)", err, recorder.Bytes())
	}
	return envelope
}

func loginBody(expectedGeneration int64) []byte {
	return []byte(`{"expected_generation":` + strconv.FormatInt(expectedGeneration, 10) + `}`)
}

func loginHeaders(key string) map[string]string {
	headers := map[string]string{"Content-Type": "application/json"}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	return headers
}

func TestLoginHTTPDisabledAuthAndValidationPaths(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	selfID := "21001"
	seedLoginAccount(t, s, "acc_http_auth", "rt_http_auth", &selfID)

	disabled := loginHTTPHandler(s, actor, nil)
	status := ttServe(disabled, http.MethodGet, loginStatusPath, nil, nil)
	if status.Code != http.StatusOK {
		t.Fatalf("disabled status=%d body=%s", status.Code, status.Body.String())
	}
	var statusEnvelope loginStatusEnvelope
	if err := json.Unmarshal(status.Body.Bytes(), &statusEnvelope); err != nil {
		t.Fatal(err)
	}
	if statusEnvelope.Data.Provider != loginProviderDisabled || statusEnvelope.Data.Fixture || statusEnvelope.Data.Live || statusEnvelope.Data.IdentityReadback != "none" {
		t.Fatalf("disabled status=%+v", statusEnvelope.Data)
	}
	create := ttServe(disabled, http.MethodPost, "/v1/accounts/acc_http_auth/login-sessions", []byte(`{}`), loginHeaders("k-disabled"))
	if create.Code != http.StatusServiceUnavailable || ttErrorCode(t, create) != taskCodeFeatureDisabled {
		t.Fatalf("disabled create=%d body=%s", create.Code, create.Body.String())
	}

	unauthenticated := s.LoginHandler(func(*http.Request) (Actor, error) { return Actor{}, problem(taskCodeUnauthenticated) }, NewLoginFixtureProvider())
	if recorder := ttServe(unauthenticated, http.MethodGet, loginStatusPath, nil, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", recorder.Code)
	}

	handler := loginHTTPHandler(s, actor, NewLoginFixtureProvider())
	noKey := ttServe(handler, http.MethodPost, "/v1/accounts/acc_http_auth/login-sessions", []byte(`{}`), loginHeaders(""))
	if noKey.Code != http.StatusBadRequest || ttErrorCode(t, noKey) != taskCodeValidation {
		t.Fatalf("missing key=%d body=%s", noKey.Code, noKey.Body.String())
	}
	noContentType := ttServe(handler, http.MethodPost, "/v1/accounts/acc_http_auth/login-sessions", []byte(`{}`), map[string]string{"Idempotency-Key": "k-no-ct"})
	if noContentType.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("missing content type=%d body=%s", noContentType.Code, noContentType.Body.String())
	}
	unknownMember := ttServe(handler, http.MethodPost, "/v1/accounts/acc_http_auth/login-sessions", []byte(`{"unexpected":true}`), loginHeaders("k-unknown-member"))
	if unknownMember.Code != http.StatusBadRequest {
		t.Fatalf("unknown member=%d body=%s", unknownMember.Code, unknownMember.Body.String())
	}
	reader := actor
	reader.Permissions = map[string]bool{"operator.read": true}
	readOnly := loginHTTPHandler(s, reader, NewLoginFixtureProvider())
	if recorder := ttServe(readOnly, http.MethodPost, "/v1/accounts/acc_http_auth/login-sessions", []byte(`{}`), loginHeaders("k-reader")); recorder.Code != http.StatusForbidden {
		t.Fatalf("read-only create=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := ttServe(readOnly, http.MethodGet, loginStatusPath, nil, nil); recorder.Code != http.StatusOK {
		t.Fatalf("read-only status=%d", recorder.Code)
	}
	if recorder := ttServe(handler, http.MethodGet, "/v1/login-sessions/login_missing", nil, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("missing session=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := ttServe(handler, http.MethodGet, "/v1/unknown", nil, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown route=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestLoginHTTPLifecycleReadbackIsFencedAndSecretFree(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	selfID := "21002"
	seedLoginAccount(t, s, "acc_http_life", "rt_http_life", &selfID)
	handler := loginHTTPHandler(s, actor, NewLoginFixtureProvider())

	created := ttServe(handler, http.MethodPost, "/v1/accounts/acc_http_life/login-sessions", []byte(`{}`), loginHeaders("http-create"))
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d body=%s", created.Code, created.Body.String())
	}
	if got := created.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control=%q", got)
	}
	createdEnvelope := loginEnvelope(t, created.Body)
	view := createdEnvelope.Data
	if view.State != loginStateCreated || view.Generation != 0 || view.AccountID != "acc_http_life" || view.QRAvailable || view.QRPNGDataURL != nil {
		t.Fatalf("created=%+v", view)
	}
	for _, secret := range []string{"webui", "token", "onebot"} {
		if strings.Contains(strings.ToLower(created.Body.String()), secret) {
			t.Fatalf("create response leaked %q: %s", secret, created.Body.String())
		}
	}

	list := ttServe(handler, http.MethodGet, "/v1/accounts/acc_http_life/login-sessions", nil, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list=%d body=%s", list.Code, list.Body.String())
	}
	var page loginSessionPageEnvelope
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data.Items) != 1 || page.Data.Items[0].ID != view.ID {
		t.Fatalf("list items=%+v", page.Data.Items)
	}

	qrPath := "/v1/login-sessions/" + view.ID + "/qr"
	qr := ttServe(handler, http.MethodPost, qrPath, loginBody(0), loginHeaders("http-qr"))
	if qr.Code != http.StatusOK {
		t.Fatalf("qr=%d body=%s", qr.Code, qr.Body.String())
	}
	qrEnvelope := loginEnvelope(t, qr.Body)
	if qrEnvelope.Data.State != loginStateQRReady || qrEnvelope.Data.Generation != 1 || !qrEnvelope.Data.QRAvailable || qrEnvelope.Data.QRPNGDataURL == nil || !strings.HasPrefix(*qrEnvelope.Data.QRPNGDataURL, "data:image/") {
		t.Fatalf("qr view=%+v", qrEnvelope.Data)
	}

	getPath := "/v1/login-sessions/" + view.ID
	read := ttServe(handler, http.MethodGet, getPath, nil, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("get=%d body=%s", read.Code, read.Body.String())
	}
	firstRead := loginEnvelope(t, read.Body).Data
	secondRead := loginEnvelope(t, ttServe(handler, http.MethodGet, getPath, nil, nil).Body).Data
	if firstRead.Version != secondRead.Version || firstRead.Generation != secondRead.Generation || firstRead.UpdatedAt != secondRead.UpdatedAt {
		t.Fatalf("GET wrote session state: first=%+v second=%+v", firstRead, secondRead)
	}
	if !firstRead.QRAvailable || firstRead.QRPNGDataURL == nil {
		t.Fatalf("GET must return the cached short-term QR: %+v", firstRead)
	}

	stale := ttServe(handler, http.MethodPost, "/v1/login-sessions/"+view.ID+"/poll", loginBody(0), loginHeaders("http-poll-stale"))
	if stale.Code != http.StatusConflict || ttErrorCode(t, stale) != loginCodeGenerationStale {
		t.Fatalf("stale poll=%d body=%s", stale.Code, stale.Body.String())
	}

	pollPath := "/v1/login-sessions/" + view.ID + "/poll"
	scanned := ttServe(handler, http.MethodPost, pollPath, loginBody(1), loginHeaders("http-poll"))
	if scanned.Code != http.StatusOK {
		t.Fatalf("poll=%d body=%s", scanned.Code, scanned.Body.String())
	}
	scannedView := loginEnvelope(t, scanned.Body).Data
	if scannedView.State != loginStateScanned || scannedView.Generation != 1 {
		t.Fatalf("scanned=%+v", scannedView)
	}
	replay := ttServe(handler, http.MethodPost, pollPath, loginBody(1), loginHeaders("http-poll"))
	if replay.Code != http.StatusOK {
		t.Fatalf("poll replay=%d body=%s", replay.Code, replay.Body.String())
	}
	if replayed := loginEnvelope(t, replay.Body).Data; replayed.State != loginStateScanned || replayed.Generation != 1 || replayed.Version != scannedView.Version {
		t.Fatalf("poll replay advanced state: scanned=%+v replay=%+v", scannedView, replayed)
	}

	authenticating := ttServe(handler, http.MethodPost, pollPath, loginBody(1), loginHeaders("http-poll-2"))
	if authenticating.Code != http.StatusOK {
		t.Fatalf("authenticating=%d body=%s", authenticating.Code, authenticating.Body.String())
	}
	if view := loginEnvelope(t, authenticating.Body).Data; view.State != loginStateAuthenticating || view.Generation != 1 {
		t.Fatalf("authenticating view=%+v", view)
	}
	online := ttServe(handler, http.MethodPost, pollPath, loginBody(1), loginHeaders("http-poll-3"))
	if online.Code != http.StatusOK {
		t.Fatalf("online=%d body=%s", online.Code, online.Body.String())
	}
	onlineView := loginEnvelope(t, online.Body).Data
	if onlineView.State != loginStateOnline || onlineView.Generation != 2 || onlineView.Identity == nil || onlineView.Identity.SelfID != selfID || !onlineView.Projection.Projected || onlineView.QRPNGDataURL != nil {
		t.Fatalf("online=%+v", onlineView)
	}
	var loginState string
	if err := s.Pool.QueryRow(context.Background(), `SELECT login_state FROM core.account WHERE workspace_id='ws_fixture' AND id='acc_http_life'`).Scan(&loginState); err != nil {
		t.Fatal(err)
	}
	if loginState != "logged_in" {
		t.Fatalf("account login_state=%q", loginState)
	}
}

func TestLoginHTTPCancelStalePollAndShortTermQRDoesNotSurviveHandler(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	selfID := "21003"
	seedLoginAccount(t, s, "acc_http_cancel", "rt_http_cancel", &selfID)
	provider := NewLoginFixtureProvider()
	handler := loginHTTPHandler(s, actor, provider)

	created := loginEnvelope(t, ttServe(handler, http.MethodPost, "/v1/accounts/acc_http_cancel/login-sessions", []byte(`{}`), loginHeaders("http-c-create")).Body).Data
	qr := loginEnvelope(t, ttServe(handler, http.MethodPost, "/v1/login-sessions/"+created.ID+"/qr", loginBody(0), loginHeaders("http-c-qr")).Body).Data
	if qr.Generation != 1 || qr.QRPNGDataURL == nil {
		t.Fatalf("qr=%+v", qr)
	}

	// A fresh handler has no QR cache: the image is short-term by design.
	restarted := loginHTTPHandler(s, actor, provider)
	afterRestart := loginEnvelope(t, ttServe(restarted, http.MethodGet, "/v1/login-sessions/"+created.ID, nil, nil).Body).Data
	if afterRestart.QRAvailable || afterRestart.QRPNGDataURL != nil || afterRestart.State != loginStateQRReady || afterRestart.Generation != 1 {
		t.Fatalf("restart read=%+v", afterRestart)
	}

	cancelled := ttServe(handler, http.MethodPost, "/v1/login-sessions/"+created.ID+"/cancel", []byte(`{}`), loginHeaders("http-c-cancel"))
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
	cancelledView := loginEnvelope(t, cancelled.Body).Data
	if cancelledView.State != loginStateCancelled || cancelledView.Generation != 2 || cancelledView.QRAvailable {
		t.Fatalf("cancelled=%+v", cancelledView)
	}
	late := ttServe(handler, http.MethodPost, "/v1/login-sessions/"+created.ID+"/poll", loginBody(1), loginHeaders("http-c-late"))
	if late.Code != http.StatusConflict || ttErrorCode(t, late) != loginCodeGenerationStale {
		t.Fatalf("late poll=%d body=%s", late.Code, late.Body.String())
	}
	readback := loginEnvelope(t, ttServe(handler, http.MethodGet, "/v1/login-sessions/"+created.ID, nil, nil).Body).Data
	if readback.State != loginStateCancelled || readback.Generation != 2 {
		t.Fatalf("cancel was overwritten: %+v", readback)
	}
	// Cancel replay with the same key returns the durable state.
	replay := ttServe(handler, http.MethodPost, "/v1/login-sessions/"+created.ID+"/cancel", []byte(`{}`), loginHeaders("http-c-cancel"))
	if replay.Code != http.StatusOK || loginEnvelope(t, replay.Body).Data.State != loginStateCancelled {
		t.Fatalf("cancel replay=%d body=%s", replay.Code, replay.Body.String())
	}
}

func TestLoginHTTPProviderFailures(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	selfID := "21004"
	seedLoginAccount(t, s, "acc_http_fail", "rt_http_fail", &selfID)

	failingStart := loginHTTPHandler(s, actor, NewLoginFixtureProviderWithOptions(LoginFixtureOptions{StartErr: errLoginFixtureFailure}))
	created := loginEnvelope(t, ttServe(failingStart, http.MethodPost, "/v1/accounts/acc_http_fail/login-sessions", []byte(`{}`), loginHeaders("http-f-create")).Body).Data
	failed := ttServe(failingStart, http.MethodPost, "/v1/login-sessions/"+created.ID+"/qr", loginBody(0), loginHeaders("http-f-qr"))
	if failed.Code != http.StatusBadGateway || ttErrorCode(t, failed) != loginCodeProviderFailed {
		t.Fatalf("provider failure=%d body=%s", failed.Code, failed.Body.String())
	}
	failedView := loginEnvelope(t, ttServe(failingStart, http.MethodGet, "/v1/login-sessions/"+created.ID, nil, nil).Body).Data
	if failedView.State != loginStateFailed || failedView.FailureReason == nil || *failedView.FailureReason != loginFailureProvider || failedView.Generation != 1 {
		t.Fatalf("failed view=%+v", failedView)
	}
	var loginState string
	if err := s.Pool.QueryRow(context.Background(), `SELECT login_state FROM core.account WHERE workspace_id='ws_fixture' AND id='acc_http_fail'`).Scan(&loginState); err != nil {
		t.Fatal(err)
	}
	if loginState != "unknown" {
		t.Fatalf("provider failure projected account: %s", loginState)
	}

	// A status failure returns 502 without changing durable state.
	statusSelfID := "21005"
	seedLoginAccount(t, s, "acc_http_status", "rt_http_status", &statusSelfID)
	good := loginHTTPHandler(s, actor, NewLoginFixtureProvider())
	statusSession := loginEnvelope(t, ttServe(good, http.MethodPost, "/v1/accounts/acc_http_status/login-sessions", []byte(`{}`), loginHeaders("http-s-create")).Body).Data
	loginEnvelope(t, ttServe(good, http.MethodPost, "/v1/login-sessions/"+statusSession.ID+"/qr", loginBody(0), loginHeaders("http-s-qr")).Body)
	bad := loginHTTPHandler(s, actor, NewLoginFixtureProviderWithOptions(LoginFixtureOptions{StatusErr: errLoginFixtureFailure}))
	statusFailure := ttServe(bad, http.MethodPost, "/v1/login-sessions/"+statusSession.ID+"/poll", loginBody(1), loginHeaders("http-s-poll"))
	if statusFailure.Code != http.StatusBadGateway || ttErrorCode(t, statusFailure) != loginCodeProviderFailed {
		t.Fatalf("status failure=%d body=%s", statusFailure.Code, statusFailure.Body.String())
	}
	unchanged := loginEnvelope(t, ttServe(bad, http.MethodGet, "/v1/login-sessions/"+statusSession.ID, nil, nil).Body).Data
	if unchanged.State != loginStateQRReady || unchanged.Generation != 1 {
		t.Fatalf("status failure changed state: %+v", unchanged)
	}
}

func TestLoginHTTPAcceptMethodAndEncodingRules(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	handler := loginHTTPHandler(s, actor, NewLoginFixtureProvider())
	if recorder := ttServe(handler, http.MethodGet, "/v1/login-sessions/x/qr", nil, nil); recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "POST" {
		t.Fatalf("method=%d allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}
	seedLoginAccount(t, s, "acc_http_enc", "rt_http_enc", nil)
	created := loginEnvelope(t, ttServe(handler, http.MethodPost, "/v1/accounts/acc_http_enc/login-sessions", []byte(`{}`), loginHeaders("http-e-create")).Body).Data
	missingGeneration := ttServe(handler, http.MethodPost, "/v1/login-sessions/"+created.ID+"/qr", []byte(`{}`), loginHeaders("http-e-qr"))
	if missingGeneration.Code != http.StatusBadRequest || ttErrorCode(t, missingGeneration) != taskCodeValidation {
		t.Fatalf("missing generation=%d body=%s", missingGeneration.Code, missingGeneration.Body.String())
	}
	unknownGenerationMember := ttServe(handler, http.MethodPost, "/v1/login-sessions/"+created.ID+"/qr", []byte(`{"expected_generation":0,"other":1}`), loginHeaders("http-e-qr-2"))
	if unknownGenerationMember.Code != http.StatusBadRequest {
		t.Fatalf("unknown generation member=%d body=%s", unknownGenerationMember.Code, unknownGenerationMember.Body.String())
	}
}

var errLoginFixtureFailure = &loginFixtureTestError{}

type loginFixtureTestError struct{}

func (*loginFixtureTestError) Error() string { return "fixture provider failure" }
