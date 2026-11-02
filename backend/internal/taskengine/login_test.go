package taskengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedLoginAccount inserts a login-test account with an explicit binding. The
// projection tests assert that login never touches risk/control/quota.
func seedLoginAccount(t *testing.T, s *Store, id, runtime string, selfID *string) {
	t.Helper()
	ctx := context.Background()
	identity := "unverified"
	if selfID != nil {
		identity = "verified"
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,quota_limit)
		VALUES('ws_fixture',$1,$2,$3,$4,$5,'offline','unknown','paused','restricted',10)`,
		id, runtime, "adapter_"+id, selfID, identity)
	if err != nil {
		t.Fatal(err)
	}
}

func loginTestProvider(opts LoginFixtureOptions) LoginProvider {
	return NewLoginFixtureProviderWithOptions(opts)
}

func loginDigest(t *testing.T, value any) string {
	t.Helper()
	digestValue, err := digest(value)
	if err != nil {
		t.Fatal(err)
	}
	return digestValue
}

func loginFindSession(t *testing.T, s *Store, actor Actor, id string) loginSessionRow {
	t.Helper()
	row, err := s.loginReadSession(context.Background(), actor, id)
	if err != nil {
		t.Fatalf("read session %s: %v", id, err)
	}
	return row
}

func TestLoginSessionFixtureLifecycleProjectsVerifiedIdentity(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	selfID := "20001"
	seedLoginAccount(t, s, "acc_login_life", "rt_login_life", &selfID)
	provider := loginTestProvider(LoginFixtureOptions{IdentityNickname: "fixture-life"})
	ctx := context.Background()

	view, err := s.loginCreate(ctx, actor, provider, "acc_login_life", "/v1/accounts/acc_login_life/login-sessions", "create-1", loginDigest(t, struct{}{}), loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if view.State != loginStateCreated || view.Generation != 0 || view.NextAction != loginNextRequestQR || view.QRAvailable {
		t.Fatalf("created view=%+v", view)
	}

	qrDigest := loginDigest(t, map[string]int64{"expected_generation": 0})
	issued, artifact, err := s.loginIssueQR(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/qr", "qr-1", qrDigest, 0, loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second})
	if err != nil {
		t.Fatalf("issue qr: %v", err)
	}
	if issued.State != loginStateQRReady || issued.Generation != 1 || issued.QRExpiresAt == nil || artifact == nil || artifact.Generation != 1 || !strings.HasPrefix(artifact.ImageDataURL, "data:image/") {
		t.Fatalf("qr view=%+v artifact=%+v", issued, artifact)
	}

	// A stale generation cannot overwrite the current QR state.
	if _, _, err := s.loginIssueQR(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/qr", "qr-stale", loginDigest(t, map[string]int64{"expected_generation": 0}), 0, loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second}); Code(err) != loginCodeGenerationStale {
		t.Fatalf("stale qr err=%v code=%s", err, Code(err))
	}

	// Refresh increments the fence again.
	refreshed, refreshArtifact, err := s.loginIssueQR(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/qr", "qr-2", loginDigest(t, map[string]int64{"expected_generation": 1}), 1, loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second})
	if err != nil {
		t.Fatalf("refresh qr: %v", err)
	}
	if refreshed.Generation != 2 || refreshArtifact == nil || refreshArtifact.Generation != 2 {
		t.Fatalf("refreshed=%+v artifact=%+v", refreshed, refreshArtifact)
	}

	// Default fixture progression: scanned -> authenticating -> online.
	scanned, err := s.loginPoll(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/poll", "poll-1", loginDigest(t, map[string]int64{"expected_generation": 2}), 2)
	if err != nil {
		t.Fatalf("poll scanned: %v", err)
	}
	if scanned.State != loginStateScanned || scanned.Generation != 2 {
		t.Fatalf("scanned=%+v", scanned)
	}
	authenticating, err := s.loginPoll(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/poll", "poll-2", loginDigest(t, map[string]int64{"expected_generation": 2}), 2)
	if err != nil {
		t.Fatalf("poll authenticating: %v", err)
	}
	if authenticating.State != loginStateAuthenticating || authenticating.Generation != 2 {
		t.Fatalf("authenticating=%+v", authenticating)
	}
	online, err := s.loginPoll(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/poll", "poll-3", loginDigest(t, map[string]int64{"expected_generation": 2}), 2)
	if err != nil {
		t.Fatalf("poll online: %v", err)
	}
	if online.State != loginStateOnline || online.Generation != 3 || online.Identity == nil || online.Identity.SelfID != selfID || !online.Projection.Projected {
		t.Fatalf("online=%+v", online)
	}
	if online.Projection.AccountLoginState != "logged_in" || online.Projection.AccountConnectionState != "online" || online.Projection.IdentityStatus != "verified" {
		t.Fatalf("projection=%+v", online.Projection)
	}

	var loginState, connection, identity, control, risk string
	var bound string
	err = s.Pool.QueryRow(ctx, `SELECT login_state,connection_state,identity_status,self_id,control_state,risk_state FROM core.account WHERE workspace_id='ws_fixture' AND id='acc_login_life'`).
		Scan(&loginState, &connection, &identity, &bound, &control, &risk)
	if err != nil {
		t.Fatal(err)
	}
	if loginState != "logged_in" || connection != "online" || identity != "verified" || bound != selfID {
		t.Fatalf("account projection login=%s connection=%s identity=%s self=%s", loginState, connection, identity, bound)
	}
	if control != "paused" || risk != "restricted" {
		t.Fatalf("login must not change control/risk: control=%s risk=%s", control, risk)
	}

	// ONLINE is terminal and already bound; cancel cannot reopen or withdraw it.
	if _, err := s.loginCancel(ctx, actor, view.ID, "/v1/login-sessions/"+view.ID+"/cancel", "cancel-online", loginDigest(t, struct{}{})); Code(err) != loginCodeStateConflict {
		t.Fatalf("cancel online err=%v code=%s", err, Code(err))
	}
}

func TestLoginCancelFencesLatePollAndReplays(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	selfID := "20002"
	seedLoginAccount(t, s, "acc_login_cancel", "rt_login_cancel", &selfID)
	provider := loginTestProvider(LoginFixtureOptions{})
	ctx := context.Background()

	view, err := s.loginCreate(ctx, actor, provider, "acc_login_cancel", "/v1/accounts/acc_login_cancel/login-sessions", "create-c", loginDigest(t, struct{}{}), loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	issued, _, err := s.loginIssueQR(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/qr", "qr-c", loginDigest(t, map[string]int64{"expected_generation": 0}), 0, loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.loginCancel(ctx, actor, view.ID, "/v1/login-sessions/"+view.ID+"/cancel", "cancel-c", loginDigest(t, struct{}{}))
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.State != loginStateCancelled || cancelled.Generation != issued.Generation+1 {
		t.Fatalf("cancelled=%+v", cancelled)
	}

	// A late poll carrying the pre-cancel generation must not reopen the session.
	if _, err := s.loginPoll(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/poll", "poll-late", loginDigest(t, map[string]int64{"expected_generation": issued.Generation}), issued.Generation); Code(err) != loginCodeGenerationStale {
		t.Fatalf("late poll err=%v code=%s", err, Code(err))
	}
	// Even the current generation cannot leave a terminal state.
	if _, err := s.loginPoll(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/poll", "poll-terminal", loginDigest(t, map[string]int64{"expected_generation": cancelled.Generation}), cancelled.Generation); Code(err) != loginCodeStateConflict {
		t.Fatalf("terminal poll err=%v code=%s", err, Code(err))
	}
	row := loginFindSession(t, s, actor, view.ID)
	if row.State != loginStateCancelled || row.Generation != cancelled.Generation {
		t.Fatalf("cancel was overwritten: %+v", row)
	}

	// Same-key cancel replay is a readback, not a second transition.
	replay, err := s.loginCancel(ctx, actor, view.ID, "/v1/login-sessions/"+view.ID+"/cancel", "cancel-c", loginDigest(t, struct{}{}))
	if err != nil {
		t.Fatalf("cancel replay: %v", err)
	}
	if replay.State != loginStateCancelled || replay.Generation != cancelled.Generation {
		t.Fatalf("cancel replay advanced state: %+v", replay)
	}

	// A replayed QR request returns the stored outcome and does not call the
	// provider or bump the generation.
	replayedQR, artifact, err := s.loginIssueQR(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/qr", "qr-c", loginDigest(t, map[string]int64{"expected_generation": 0}), 0, loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second})
	if err != nil || artifact != nil {
		t.Fatalf("qr replay err=%v artifact=%+v", err, artifact)
	}
	if replayedQR.State != loginStateCancelled {
		t.Fatalf("qr replay must return current durable state, got %+v", replayedQR)
	}
}

func TestLoginReadNeverWritesAndExpiryIsDurable(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	selfID := "20003"
	seedLoginAccount(t, s, "acc_login_expire", "rt_login_expire", &selfID)
	provider := loginTestProvider(LoginFixtureOptions{})
	ctx := context.Background()

	view, err := s.loginCreate(ctx, actor, provider, "acc_login_expire", "/v1/accounts/acc_login_expire/login-sessions", "create-e", loginDigest(t, struct{}{}), loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	issued, _, err := s.loginIssueQR(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/qr", "qr-e", loginDigest(t, map[string]int64{"expected_generation": 0}), 0, loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	before := loginFindSession(t, s, actor, view.ID)
	// Expire the session by moving both deadlines just past created_at rather
	// than before it. The schema requires expires_at>created_at and any stored
	// QR deadline to be >created_at and <=expires_at; a QR is issued after the
	// session exists, so a QR timestamp before created_at would be impossible
	// data. created_at remains immutable (the transition trigger enforces it).
	_, err = s.Pool.Exec(ctx, `UPDATE core.login_session SET expires_at=created_at+interval '1 microsecond', qr_expires_at=created_at+interval '1 microsecond' WHERE workspace_id='ws_fixture' AND id=$1`, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	read := loginFindSession(t, s, actor, view.ID)
	if read.State != loginStateQRReady || read.Generation != before.Generation || read.Version != before.Version {
		t.Fatalf("read wrote state: before=%+v after=%+v", before, read)
	}
	derived := loginSessionView(read, time.Now().UTC())
	if !derived.Expired || derived.State != loginStateQRReady {
		t.Fatalf("derived expiry=%+v", derived)
	}

	polled, err := s.loginPoll(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/poll", "poll-expire", loginDigest(t, map[string]int64{"expected_generation": issued.Generation}), issued.Generation)
	if err != nil {
		t.Fatalf("poll expiry: %v", err)
	}
	if polled.State != loginStateExpired || polled.FailureReason == nil || *polled.FailureReason != loginFailureTimeout {
		t.Fatalf("expired poll=%+v", polled)
	}
	// Expired is terminal: no QR refresh and no cancel.
	if _, _, err := s.loginIssueQR(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/qr", "qr-after-expiry", loginDigest(t, map[string]int64{"expected_generation": polled.Generation}), polled.Generation, loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second}); Code(err) != loginCodeStateConflict {
		t.Fatalf("qr after expiry err=%v code=%s", err, Code(err))
	}
	// A new session is allowed once the terminal row is no longer open.
	if _, err := s.loginCreate(ctx, actor, provider, "acc_login_expire", "/v1/accounts/acc_login_expire/login-sessions", "create-e2", loginDigest(t, struct{}{}), loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second}); err != nil {
		t.Fatalf("create after expiry: %v", err)
	}
}

func TestLoginIdentityVerificationGatesProjection(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	ctx := context.Background()
	boundSelf := "20010"
	seedLoginAccount(t, s, "acc_login_bound", "rt_login_bound", &boundSelf)

	start := func(account string, provider LoginProvider) LoginSessionView {
		t.Helper()
		view, err := s.loginCreate(ctx, actor, provider, account, "/v1/accounts/"+account+"/login-sessions", "create-"+account, loginDigest(t, struct{}{}), loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.loginIssueQR(ctx, actor, provider, view.ID, "/v1/login-sessions/"+view.ID+"/qr", "qr-"+account, loginDigest(t, map[string]int64{"expected_generation": 0}), 0, loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second}); err != nil {
			t.Fatal(err)
		}
		return view
	}

	// A different QQ id must fail closed and leave the account untouched.
	mismatchProvider := loginTestProvider(LoginFixtureOptions{IdentitySelfID: "29999", StatusPhases: []LoginPhase{LoginPhaseOnline}})
	mismatchView := start("acc_login_bound", mismatchProvider)
	if _, err := s.loginPoll(ctx, actor, mismatchProvider, mismatchView.ID, "/v1/login-sessions/"+mismatchView.ID+"/poll", "poll-mm", loginDigest(t, map[string]int64{"expected_generation": 1}), 1); Code(err) != loginCodeIdentityMismatch {
		t.Fatalf("mismatch err=%v code=%s", err, Code(err))
	}
	failed := loginFindSession(t, s, actor, mismatchView.ID)
	if failed.State != loginStateFailed || failed.FailureReason == nil || *failed.FailureReason != loginFailureIdentityMismatch {
		t.Fatalf("failed session=%+v", failed)
	}
	var loginState, connection, identity string
	if err := s.Pool.QueryRow(ctx, `SELECT login_state,connection_state,identity_status FROM core.account WHERE workspace_id='ws_fixture' AND id='acc_login_bound'`).Scan(&loginState, &connection, &identity); err != nil {
		t.Fatal(err)
	}
	if loginState != "unknown" || connection != "offline" || identity != "verified" {
		t.Fatalf("mismatch projected account: login=%s connection=%s identity=%s", loginState, connection, identity)
	}

	// An unbound account binds the verified identity exactly once.
	otherSelf := "20011"
	seedLoginAccount(t, s, "acc_login_other", "rt_login_other", &otherSelf)
	unboundProvider := loginTestProvider(LoginFixtureOptions{IdentitySelfID: "20012", IdentityNickname: "unbound", StatusPhases: []LoginPhase{LoginPhaseOnline}})
	seedLoginAccount(t, s, "acc_login_unbound2", "rt_login_unbound2", nil)
	unboundView := start("acc_login_unbound2", unboundProvider)
	online, err := s.loginPoll(ctx, actor, unboundProvider, unboundView.ID, "/v1/login-sessions/"+unboundView.ID+"/poll", "poll-unbound", loginDigest(t, map[string]int64{"expected_generation": 1}), 1)
	if err != nil {
		t.Fatalf("unbound online: %v", err)
	}
	if online.State != loginStateOnline || online.Identity == nil || online.Identity.SelfID != "20012" {
		t.Fatalf("unbound online=%+v", online)
	}
	var bound *string
	if err := s.Pool.QueryRow(ctx, `SELECT self_id FROM core.account WHERE workspace_id='ws_fixture' AND id='acc_login_unbound2'`).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound == nil || *bound != "20012" {
		t.Fatalf("unbound account was not bound: %v", bound)
	}

	// A verified identity already bound to a different account fails closed.
	seedLoginAccount(t, s, "acc_login_taken", "rt_login_taken", nil)
	takenProvider := loginTestProvider(LoginFixtureOptions{IdentitySelfID: otherSelf, StatusPhases: []LoginPhase{LoginPhaseOnline}})
	takenView := start("acc_login_taken", takenProvider)
	if _, err := s.loginPoll(ctx, actor, takenProvider, takenView.ID, "/v1/login-sessions/"+takenView.ID+"/poll", "poll-taken", loginDigest(t, map[string]int64{"expected_generation": 1}), 1); Code(err) != loginCodeIdentityAlreadyUsed {
		t.Fatalf("taken err=%v code=%s", err, Code(err))
	}
	row := loginFindSession(t, s, actor, takenView.ID)
	if row.State != loginStateFailed || row.FailureReason == nil || *row.FailureReason != loginFailureIdentityBound {
		t.Fatalf("taken session=%+v", row)
	}
	var takenSelf *string
	if err := s.Pool.QueryRow(ctx, `SELECT self_id FROM core.account WHERE workspace_id='ws_fixture' AND id='acc_login_taken'`).Scan(&takenSelf); err != nil {
		t.Fatal(err)
	}
	if takenSelf != nil {
		t.Fatalf("taken account must stay unbound, got %v", *takenSelf)
	}
}

type countingLoginProvider struct {
	LoginProvider
	mu           sync.Mutex
	statusCalls  int
	startCalls   int
	refreshCalls int
}

func (p *countingLoginProvider) Start(ctx context.Context, account LoginAccount) (LoginQR, error) {
	p.mu.Lock()
	p.startCalls++
	p.mu.Unlock()
	return p.LoginProvider.Start(ctx, account)
}

func (p *countingLoginProvider) Refresh(ctx context.Context, account LoginAccount) (LoginQR, error) {
	p.mu.Lock()
	p.refreshCalls++
	p.mu.Unlock()
	return p.LoginProvider.Refresh(ctx, account)
}

func (p *countingLoginProvider) Status(ctx context.Context, account LoginAccount) (LoginStatus, error) {
	p.mu.Lock()
	p.statusCalls++
	p.mu.Unlock()
	return p.LoginProvider.Status(ctx, account)
}

func (p *countingLoginProvider) counts() (int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startCalls, p.refreshCalls, p.statusCalls
}

func TestLoginIdempotentReplaysNeverAdvanceOrRecallProvider(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	selfID := "20020"
	seedLoginAccount(t, s, "acc_login_idem", "rt_login_idem", &selfID)
	counting := &countingLoginProvider{LoginProvider: loginTestProvider(LoginFixtureOptions{})}
	ctx := context.Background()
	options := loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second}

	created, err := s.loginCreate(ctx, actor, counting, "acc_login_idem", "/v1/accounts/acc_login_idem/login-sessions", "create-i", loginDigest(t, struct{}{}), options)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.loginCreate(ctx, actor, counting, "acc_login_idem", "/v1/accounts/acc_login_idem/login-sessions", "create-i", loginDigest(t, struct{}{}), options)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != created.ID {
		t.Fatalf("create replay id=%s want %s", replayed.ID, created.ID)
	}
	var sessions int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM core.login_session WHERE workspace_id='ws_fixture' AND account_id='acc_login_idem'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("create replay created %d sessions", sessions)
	}
	if _, err := s.loginCreate(ctx, actor, counting, "acc_login_idem", "/v1/accounts/acc_login_idem/login-sessions", "create-other", loginDigest(t, struct{}{}), options); Code(err) != loginCodeSessionActive {
		t.Fatalf("second open session err=%v code=%s", err, Code(err))
	}

	issued, _, err := s.loginIssueQR(ctx, actor, counting, created.ID, "/v1/login-sessions/"+created.ID+"/qr", "qr-i", loginDigest(t, map[string]int64{"expected_generation": 0}), 0, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.loginIssueQR(ctx, actor, counting, created.ID, "/v1/login-sessions/"+created.ID+"/qr", "qr-i", loginDigest(t, map[string]int64{"expected_generation": 0}), 0, options); err != nil {
		t.Fatalf("qr replay: %v", err)
	}
	startCalls, _, _ := counting.counts()
	if startCalls != 1 {
		t.Fatalf("qr replay called provider %d times", startCalls)
	}

	pollDigest := loginDigest(t, map[string]int64{"expected_generation": issued.Generation})
	first, err := s.loginPoll(ctx, actor, counting, created.ID, "/v1/login-sessions/"+created.ID+"/poll", "poll-i", pollDigest, issued.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != loginStateScanned || first.Generation != issued.Generation {
		t.Fatalf("first poll=%+v", first)
	}
	_, _, callsBefore := counting.counts()
	replayedPoll, err := s.loginPoll(ctx, actor, counting, created.ID, "/v1/login-sessions/"+created.ID+"/poll", "poll-i", pollDigest, issued.Generation)
	if err != nil {
		t.Fatalf("poll replay: %v", err)
	}
	_, _, callsAfter := counting.counts()
	if callsAfter != callsBefore {
		t.Fatalf("poll replay called provider: before=%d after=%d", callsBefore, callsAfter)
	}
	if replayedPoll.Generation != first.Generation || replayedPoll.State != first.State {
		t.Fatalf("poll replay advanced: first=%+v replay=%+v", first, replayedPoll)
	}
	// The same key bound to a different body is a conflict, not a second poll.
	if _, err := s.loginPoll(ctx, actor, counting, created.ID, "/v1/login-sessions/"+created.ID+"/poll", "poll-i", loginDigest(t, map[string]int64{"expected_generation": issued.Generation + 1}), issued.Generation); Code(err) != taskCodeIdempotency {
		t.Fatalf("poll body conflict err=%v code=%s", err, Code(err))
	}
}

func TestLoginScopeAndPermissionBoundaries(t *testing.T) {
	s := fixtureStore(t)
	actor := fixtureActor()
	selfID := "20030"
	seedLoginAccount(t, s, "acc_login_scope", "rt_login_scope", &selfID)
	provider := loginTestProvider(LoginFixtureOptions{})
	ctx := context.Background()
	options := loginOptions{sessionTTL: time.Minute, qrTTL: 30 * time.Second}

	view, err := s.loginCreate(ctx, actor, provider, "acc_login_scope", "/v1/accounts/acc_login_scope/login-sessions", "create-s", loginDigest(t, struct{}{}), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.loginReadSession(ctx, Actor{WorkspaceID: "ws_other", PrincipalID: "principal_fixture", Permissions: actor.Permissions}, view.ID); Code(err) != taskCodeNotFound {
		t.Fatalf("cross-workspace read err=%v code=%s", err, Code(err))
	}
	runtimeActor := Actor{WorkspaceID: "ws_fixture", PrincipalID: "principal_fixture", RuntimeID: "rt_login_scope", Permissions: actor.Permissions}
	if _, err := s.loginReadSession(ctx, runtimeActor, view.ID); Code(err) != taskCodeForbidden {
		t.Fatalf("runtime read err=%v code=%s", err, Code(err))
	}
	reader := Actor{WorkspaceID: "ws_fixture", PrincipalID: "principal_fixture", Permissions: map[string]bool{"operator.read": true}}
	if _, err := s.loginReadSession(ctx, reader, view.ID); err != nil {
		t.Fatalf("operator.read read: %v", err)
	}
	if _, err := s.loginCancel(ctx, reader, view.ID, "/v1/login-sessions/"+view.ID+"/cancel", "cancel-reader", loginDigest(t, struct{}{})); Code(err) != taskCodeForbidden {
		t.Fatalf("read-only cancel err=%v code=%s", err, Code(err))
	}
	noLoginPermission := Actor{WorkspaceID: "ws_fixture", PrincipalID: "principal_fixture", Permissions: map[string]bool{"task.create": true}}
	if _, err := s.loginReadSession(ctx, noLoginPermission, view.ID); Code(err) != taskCodeForbidden {
		t.Fatalf("unrelated permission read err=%v code=%s", err, Code(err))
	}
	if _, err := s.loginReadSession(ctx, actor, "login_missing"); Code(err) != taskCodeNotFound {
		t.Fatalf("missing session err=%v code=%s", err, Code(err))
	}
}

func TestLoadLoginProviderIsExplicit(t *testing.T) {
	if provider, err := LoadLoginProvider("", false); err != nil || provider != nil {
		t.Fatalf("no provider: %v %v", provider, err)
	}
	provider, err := LoadLoginProvider("", true)
	if err != nil || provider == nil || provider.Name() != loginProviderFixture {
		t.Fatalf("fixture provider: %v %v", provider, err)
	}
	if _, err := LoadLoginProvider(filepath.Join(t.TempDir(), "missing.json"), true); err == nil {
		t.Fatal("fixture plus provider file must be rejected")
	}
}

func TestLoginProviderFileValidation(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "webui.token")
	onebotToken := filepath.Join(dir, "onebot.token")
	for _, path := range []string{token, onebotToken} {
		if err := os.WriteFile(path, []byte("secret-token\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig := func(t *testing.T, body string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "provider.json")
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	valid := `{"mode":"napcat","session_ttl_seconds":60,"qr_ttl_seconds":30,"instances":[{"adapter_instance_id":"adapter_x","webui_url":"http://127.0.0.1:6099/api","webui_token_file":"` + token + `","onebot_url":"http://127.0.0.1:3010","onebot_token_file":"` + onebotToken + `"}]}`
	provider, err := LoadLoginProvider(writeConfig(t, valid, 0600), false)
	if err != nil || provider == nil || provider.Name() != loginProviderNapCat {
		t.Fatalf("valid provider: %v %v", provider, err)
	}
	if _, err := LoadLoginProvider(writeConfig(t, valid, 0644), false); err == nil {
		t.Fatal("world-readable provider file must be rejected")
	}
	nonLoopback := strings.Replace(valid, "http://127.0.0.1:6099/api", "http://192.0.2.1:6099/api", 1)
	if _, err := LoadLoginProvider(writeConfig(t, nonLoopback, 0600), false); err == nil {
		t.Fatal("non-loopback provider URL must be rejected")
	}
	missingAPI := strings.Replace(valid, "http://127.0.0.1:6099/api", "http://127.0.0.1:6099", 1)
	if _, err := LoadLoginProvider(writeConfig(t, missingAPI, 0600), false); err == nil {
		t.Fatal("webui URL without /api must be rejected")
	}
	unknownField := strings.Replace(valid, `"mode":"napcat"`, `"mode":"napcat","token":"x"`, 1)
	if _, err := LoadLoginProvider(writeConfig(t, unknownField, 0600), false); err == nil {
		t.Fatal("unknown config field must be rejected")
	}
	if err := os.Chmod(token, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLoginProvider(writeConfig(t, valid, 0600), false); err == nil {
		t.Fatal("world-readable token file must be rejected")
	}
}
