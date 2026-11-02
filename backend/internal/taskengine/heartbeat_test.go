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
)

func hbRequest(epoch, sequence string, connection contract.HeartbeatRequestConnectionState, login contract.HeartbeatRequestLoginState, at time.Time) contract.HeartbeatRequest {
	return contract.HeartbeatRequest{
		WorkspaceId:     "ws_fixture",
		RuntimeId:       "rt_acc_hb",
		AccountId:       "acc_hb",
		OwnerEpoch:      epoch,
		Sequence:        sequence,
		ConnectionState: connection,
		LoginState:      login,
		HeartbeatAt:     at,
		Capabilities:    []contract.Capability{},
	}
}

func hbFixture(t *testing.T) (*Store, RuntimeActor) {
	t.Helper()
	store := fixtureStore(t)
	seedAccount(t, store, "acc_hb", "rt_acc_hb", 5)
	return store, fixtureRuntime("acc_hb", "rt_acc_hb")
}

func hbAccountState(t *testing.T, s *Store, account string) (int, string, string, string, *time.Time) {
	t.Helper()
	var version int
	var connection, login, control string
	var heartbeatAt *time.Time
	if err := s.Pool.QueryRow(context.Background(), `SELECT version,connection_state,login_state,control_state,heartbeat_at
		FROM core.account WHERE workspace_id='ws_fixture' AND id=$1`, account).Scan(&version, &connection, &login, &control, &heartbeatAt); err != nil {
		t.Fatalf("read account projection: %v", err)
	}
	return version, connection, login, control, heartbeatAt
}

func hbRowCount(t *testing.T, s *Store, disposition string) int {
	t.Helper()
	return scalarInt(t, s, `SELECT count(*) FROM core.runtime_heartbeat WHERE workspace_id='ws_fixture' AND account_id='acc_hb' AND disposition=$1`, disposition)
}

func hbAuth(actor RuntimeActor) func(*http.Request) (RuntimeActor, error) {
	return func(r *http.Request) (RuntimeActor, error) {
		if r.Header.Get("Authorization") != "Runtime hb-test-token" {
			return RuntimeActor{}, problem(taskCodeUnauthenticated)
		}
		return actor, nil
	}
}

func hbServe(t *testing.T, handler http.Handler, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func hbJSONBody(t *testing.T, req contract.HeartbeatRequest) []byte {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// A strictly newer heartbeat updates only the connection/login/time projection;
// control state stays server-owned and the receipt reports the applied version.
func TestHeartbeatAppliesNewerProjection(t *testing.T) {
	s, actor := hbFixture(t)
	at := time.Now().UTC().Add(time.Minute)
	receipt, err := s.ReceiveHeartbeat(context.Background(), actor, "rt_acc_hb", "hb-key-1",
		hbRequest("1", "2", contract.HeartbeatRequestConnectionStateDegraded, contract.HeartbeatRequestLoginStateLoggedIn, at))
	if err != nil {
		t.Fatalf("receive heartbeat: %v", err)
	}
	if receipt.AccountVersion != 2 || string(receipt.ControlState) != "active" ||
		receipt.RuntimeId != "rt_acc_hb" || receipt.AccountId != "acc_hb" ||
		receipt.OwnerEpoch != "1" || receipt.Sequence != "2" || !receipt.NextActionAt.IsNull() {
		t.Fatalf("receipt=%+v", receipt)
	}
	if receipt.AcceptedAt.IsZero() || receipt.AcceptedAt.Location() != time.UTC {
		t.Fatalf("accepted_at=%v", receipt.AcceptedAt)
	}
	version, connection, login, control, heartbeatAt := hbAccountState(t, s, "acc_hb")
	if version != 2 || connection != "degraded" || login != "logged_in" || control != "active" {
		t.Fatalf("projection version=%d connection=%s login=%s control=%s", version, connection, login, control)
	}
	if heartbeatAt == nil || !heartbeatAt.UTC().Truncate(time.Microsecond).Equal(at.Truncate(time.Microsecond)) {
		t.Fatalf("heartbeat_at=%v want %v", heartbeatAt, at)
	}
	if got := hbRowCount(t, s, "applied"); got != 1 {
		t.Fatalf("applied audit rows=%d", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.runtime_heartbeat WHERE workspace_id='ws_fixture' AND request_id='hb-key-1'`); got != 1 {
		t.Fatalf("request id not persisted: %d", got)
	}
}

// Stale sequences are recorded but never merged as newer; exact replay is
// idempotent; the same sequence with a different payload is a business conflict.
func TestHeartbeatStaleReplayAndConflict(t *testing.T) {
	s, actor := hbFixture(t)
	ctx := context.Background()
	newer := hbRequest("1", "5", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute))
	if _, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-newer", newer); err != nil {
		t.Fatalf("apply newer: %v", err)
	}
	before, _, _, _, beforeHeartbeat := hbAccountState(t, s, "acc_hb")

	older := hbRequest("1", "3", contract.HeartbeatRequestConnectionStateOffline, contract.HeartbeatRequestLoginStateLoggedOut, time.Now().UTC())
	receipt, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-older", older)
	if err != nil {
		t.Fatalf("stale heartbeat must be accepted and ignored: %v", err)
	}
	if receipt.Sequence != "3" || receipt.AccountVersion != before {
		t.Fatalf("stale receipt=%+v must echo request sequence 3 and keep version %d", receipt, before)
	}
	if version, connection, login, _, heartbeatAt := hbAccountState(t, s, "acc_hb"); version != before || connection != "online" || login != "logged_in" ||
		heartbeatAt == nil || beforeHeartbeat == nil || !heartbeatAt.Equal(*beforeHeartbeat) {
		t.Fatalf("stale heartbeat changed projection: version=%d connection=%s login=%s", version, connection, login)
	}
	if got := hbRowCount(t, s, "stale"); got != 1 {
		t.Fatalf("stale audit rows=%d", got)
	}

	replay, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-newer", newer)
	if err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	if replay.AccountVersion != before || replay.Sequence != "5" {
		t.Fatalf("replay receipt=%+v", replay)
	}
	if got := hbRowCount(t, s, "applied"); got != 1 {
		t.Fatalf("replay appended audit rows=%d", got)
	}

	conflict := hbRequest("1", "5", contract.HeartbeatRequestConnectionStateDegraded, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute))
	_, err = s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-conflict", conflict)
	if Code(err) != "HEARTBEAT_SEQUENCE_CONFLICT" {
		t.Fatalf("conflicting replay code=%q want HEARTBEAT_SEQUENCE_CONFLICT", Code(err))
	}
	if got := hbRowCount(t, s, "applied"); got != 1 {
		t.Fatalf("conflict appended audit rows=%d", got)
	}
}

func TestHeartbeatRejectsWrongIdentity(t *testing.T) {
	s, actor := hbFixture(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(time.Minute)

	futureEpoch := hbRequest("2", "2", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, at)
	if _, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-epoch", futureEpoch); Code(err) != "ACCOUNT_RESTRICTED" {
		t.Fatalf("epoch mismatch code=%q", Code(err))
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.runtime_heartbeat`); got != 0 {
		t.Fatalf("epoch mismatch wrote %d rows", got)
	}

	otherAccount := hbRequest("1", "2", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, at)
	otherAccount.AccountId = "acc_other"
	if _, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-account", otherAccount); Code(err) != taskCodeNotFound {
		t.Fatalf("foreign account code=%q", Code(err))
	}

	valid := hbRequest("1", "2", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, at)
	if _, err := s.ReceiveHeartbeat(ctx, actor, "rt_other", "hb-runtime", valid); Code(err) != taskCodeNotFound {
		t.Fatalf("wrong runtime path code=%q", Code(err))
	}
	crossed := valid
	crossed.RuntimeId = "rt_other"
	if _, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-runtime-body", crossed); Code(err) != taskCodeNotFound {
		t.Fatalf("wrong runtime body code=%q", Code(err))
	}
}

// A heartbeat never re-opens a paused or blocked account and reports when it
// may act again.
func TestHeartbeatKeepsAccountControl(t *testing.T) {
	s, actor := hbFixture(t)
	ctx := context.Background()
	next := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if _, err := s.Pool.Exec(ctx, `UPDATE core.account SET control_state='paused',next_action_at=$1 WHERE workspace_id='ws_fixture' AND id='acc_hb'`, next); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-paused",
		hbRequest("1", "2", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute)))
	if err != nil {
		t.Fatalf("receive heartbeat: %v", err)
	}
	if string(receipt.ControlState) != "paused" || receipt.NextActionAt.IsNull() {
		t.Fatalf("paused receipt=%+v", receipt)
	}
	if got := receipt.NextActionAt.MustGet(); !got.UTC().Equal(next) {
		t.Fatalf("next_action_at=%v want %v", got, next)
	}
	if _, _, _, control, _ := hbAccountState(t, s, "acc_hb"); control != "paused" {
		t.Fatalf("heartbeat changed control state to %s", control)
	}
}

func TestHeartbeatHTTPContract(t *testing.T) {
	s, actor := hbFixture(t)
	handler := s.RuntimeHandler(hbAuth(actor))
	valid := hbRequest("1", "2", contract.HeartbeatRequestConnectionStateDegraded, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute))
	body := hbJSONBody(t, valid)
	baseHeaders := map[string]string{
		"Authorization":   "Runtime hb-test-token",
		"Content-Type":    "application/json",
		"Idempotency-Key": "hb-http-1",
		"X-Request-Id":    "req-hb-1",
	}

	recorder := hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", body, baseHeaders)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope contract.HeartbeatEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Data.AccountVersion != 2 || envelope.Data.Sequence != "2" || envelope.Data.ControlState != "active" {
		t.Fatalf("envelope=%+v", envelope.Data)
	}
	if envelope.Meta.RequestId != "req-hb-1" {
		t.Fatalf("meta request id=%q", envelope.Meta.RequestId)
	}

	unauth := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "hb-http-2"}
	if recorder := hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", body, unauth); recorder.Code != http.StatusUnauthorized || hbErrorCode(t, recorder) != taskCodeUnauthenticated {
		t.Fatalf("unauthenticated status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	noKey := map[string]string{"Authorization": "Runtime hb-test-token", "Content-Type": "application/json"}
	if recorder := hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", body, noKey); recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing key status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var unknown map[string]any
	if err := json.Unmarshal(body, &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["unexpected"] = true
	unknownBody, _ := json.Marshal(unknown)
	if recorder := hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", unknownBody, baseHeaders); recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	if recorder := hbServe(t, handler, http.MethodGet, "/v1/internal/runtimes/rt_acc_hb/heartbeats", nil, baseHeaders); recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "POST" {
		t.Fatalf("method status=%d allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}

	if recorder := hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_other/heartbeats", body, baseHeaders); recorder.Code != http.StatusNotFound {
		t.Fatalf("wrong runtime status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	offset := valid
	offset.HeartbeatAt = time.Now().In(time.FixedZone("CST", 8*3600))
	if recorder := hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", hbJSONBody(t, offset), baseHeaders); recorder.Code != http.StatusBadRequest {
		t.Fatalf("local offset status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	oversized := valid
	oversized.AccountId = strings.Repeat("a", maxHeartbeatBodyBytes+1)
	if recorder := hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", hbJSONBody(t, oversized), baseHeaders); recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHeartbeatAuditIsAppendOnly(t *testing.T) {
	s, actor := hbFixture(t)
	if _, err := s.ReceiveHeartbeat(context.Background(), actor, "rt_acc_hb", "hb-audit",
		hbRequest("1", "2", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	rejectsSQL(t, s, `UPDATE core.runtime_heartbeat SET connection_state='offline' WHERE workspace_id='ws_fixture'`)
	rejectsSQL(t, s, `DELETE FROM core.runtime_heartbeat WHERE workspace_id='ws_fixture'`)
	rejectsSQL(t, s, `UPDATE core.heartbeat_idempotency SET digest=digest WHERE workspace_id='ws_fixture'`)
	rejectsSQL(t, s, `DELETE FROM core.heartbeat_idempotency WHERE workspace_id='ws_fixture'`)
}

func hbErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope contract.ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v (body %q)", err, recorder.Body.String())
	}
	return envelope.Error.Code
}

// Contract: openapi.yaml components/parameters/IdempotencyKey. Reusing one key
// with the same request must return the original receipt byte-for-byte, while a
// different request under that key is IDEMPOTENCY_CONFLICT.
func TestHeartbeatIdempotencyKeyReturnsOriginalReceipt(t *testing.T) {
	s, actor := hbFixture(t)
	ctx := context.Background()
	req := hbRequest("1", "2", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute))
	first, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-idem", req)
	if err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	replay, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-idem", req)
	if err != nil {
		t.Fatalf("same-key replay: %v", err)
	}
	if !replay.AcceptedAt.Equal(first.AcceptedAt) || replay.AccountVersion != first.AccountVersion ||
		replay.Sequence != first.Sequence || replay.OwnerEpoch != first.OwnerEpoch ||
		replay.RuntimeId != first.RuntimeId || replay.AccountId != first.AccountId ||
		replay.ControlState != first.ControlState || !replay.NextActionAt.IsNull() {
		t.Fatalf("replay receipt=%+v want original %+v", replay, first)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.runtime_heartbeat WHERE workspace_id='ws_fixture' AND request_id='hb-idem'`); got != 1 {
		t.Fatalf("idempotent replay rows=%d", got)
	}
	changed := req
	changed.HeartbeatAt = req.HeartbeatAt.Add(time.Second)
	if _, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-idem", changed); Code(err) != taskCodeIdempotency {
		t.Fatalf("same key different request code=%q want %s", Code(err), taskCodeIdempotency)
	}
}

func hbCapabilityJSON(t *testing.T, overrides map[string]any) []map[string]any {
	t.Helper()
	action := func() map[string]any {
		return map[string]any{"supported": false, "effect": "read", "pagination": "none", "fresh_read": true, "idempotency": "none"}
	}
	capability := map[string]any{
		"account_id":         "acc_hb",
		"adapter_id":         "adapter_p012",
		"adapter_version":    "1.0.0",
		"verification_state": "unverified",
		"verified_at":        nil,
		"evidence_ref":       nil,
		"actions": map[string]any{
			"search_groups": action(), "request_join": action(), "observe_membership": action(),
			"observe_join_request": action(), "cancel_join_request": action(), "send_text": action(),
		},
	}
	for key, value := range overrides {
		capability[key] = value
	}
	return []map[string]any{capability}
}

// Contract: common.json#/HeartbeatRequest.capabilities items -> Capability.
// A structurally invalid item must be rejected as a ContractError, never stored.
func TestHeartbeatRejectsInvalidCapabilitySchema(t *testing.T) {
	s, actor := hbFixture(t)
	handler := s.RuntimeHandler(hbAuth(actor))
	body := map[string]any{
		"workspace_id": "ws_fixture", "runtime_id": "rt_acc_hb", "account_id": "acc_hb",
		"owner_epoch": "1", "sequence": "2", "connection_state": "online", "login_state": "logged_in",
		"heartbeat_at": time.Now().UTC().Add(time.Minute).Format("2006-01-02T15:04:05Z"),
		"capabilities": []map[string]any{{}},
	}
	headers := map[string]string{"Authorization": "Runtime hb-test-token", "Content-Type": "application/json", "Idempotency-Key": "hb-cap-invalid"}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	recorder := hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", payload, headers)
	if recorder.Code != http.StatusBadRequest || hbErrorCode(t, recorder) != taskCodeValidation {
		t.Fatalf("invalid capability status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.runtime_heartbeat`); got != 0 {
		t.Fatalf("invalid capability persisted %d rows", got)
	}
	body["capabilities"] = hbCapabilityJSON(t, nil)
	payload, _ = json.Marshal(body)
	recorder = hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", payload, headers)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("valid capability status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// Contract: common.json#/HeartbeatRequest "owner_epoch and sequence are
// monotonic decimal strings". A newer epoch starts a new sequence space, and a
// receipt always echoes the sequence of this request.
func TestHeartbeatCrossEpochAppliesAndEchoesRequestSequence(t *testing.T) {
	s, actor := hbFixture(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(time.Minute)
	if _, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-epoch1", hbRequest("1", "5", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, at)); err != nil {
		t.Fatalf("epoch 1 heartbeat: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE core.account SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND id='acc_hb'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE core.runtime_credentials SET owner_epoch=2 WHERE workspace_id='ws_fixture' AND runtime_id='rt_acc_hb' AND account_id='acc_hb'`); err != nil {
		t.Fatal(err)
	}
	actor2 := actor
	actor2.OwnerEpoch = 2
	receipt, err := s.ReceiveHeartbeat(ctx, actor2, "rt_acc_hb", "hb-epoch2", hbRequest("2", "1", contract.HeartbeatRequestConnectionStateDegraded, contract.HeartbeatRequestLoginStateLoggedIn, at.Add(time.Second)))
	if err != nil {
		t.Fatalf("epoch 2 sequence 1 must apply: %v", err)
	}
	if receipt.Sequence != "1" || receipt.OwnerEpoch != "2" || receipt.AccountVersion != 3 {
		t.Fatalf("epoch 2 receipt=%+v", receipt)
	}
	version, connection, _, _, _ := hbAccountState(t, s, "acc_hb")
	if version != 3 || connection != "degraded" {
		t.Fatalf("epoch 2 not applied: version=%d connection=%s", version, connection)
	}
	if _, err := s.ReceiveHeartbeat(ctx, actor2, "rt_acc_hb", "hb-epoch2-newer", hbRequest("2", "3", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, at.Add(2*time.Second))); err != nil {
		t.Fatalf("epoch 2 sequence 3: %v", err)
	}
	stale, err := s.ReceiveHeartbeat(ctx, actor2, "rt_acc_hb", "hb-epoch2-stale", hbRequest("2", "2", contract.HeartbeatRequestConnectionStateOffline, contract.HeartbeatRequestLoginStateLoggedOut, at))
	if err != nil {
		t.Fatalf("stale delivery must be accepted: %v", err)
	}
	if stale.Sequence != "2" || stale.AccountVersion != 4 {
		t.Fatalf("stale receipt must echo request sequence 2 with version 4, got %+v", stale)
	}
	if version, connection, _, _, _ := hbAccountState(t, s, "acc_hb"); version != 4 || connection != "online" {
		t.Fatalf("stale delivery changed projection: version=%d connection=%s", version, connection)
	}
}

// Contract: openapi.yaml Heartbeat default -> components/responses/ContractError.
// A same-sequence/different-digest replay is a business conflict on that
// channel, not an opaque 500.
func TestHeartbeatSequenceConflictUsesContractErrorChannel(t *testing.T) {
	s, actor := hbFixture(t)
	handler := s.RuntimeHandler(hbAuth(actor))
	headers := map[string]string{"Authorization": "Runtime hb-test-token", "Content-Type": "application/json", "Idempotency-Key": "hb-conflict-http"}
	first := hbRequest("1", "2", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute))
	recorder := hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", hbJSONBody(t, first), headers)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	conflict := first
	conflict.ConnectionState = contract.HeartbeatRequestConnectionStateOffline
	headers["Idempotency-Key"] = "hb-conflict-http-2"
	recorder = hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", hbJSONBody(t, conflict), headers)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope contract.ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, recorder.Body.String())
	}
	if envelope.Error.Code != "HEARTBEAT_SEQUENCE_CONFLICT" || envelope.Error.Retryable {
		t.Fatalf("conflict envelope=%+v", envelope.Error)
	}
}

// Contract: openapi.yaml components/parameters/IdempotencyKey and design
// 02 §2.1. An alias key that successfully replays an event becomes bound to
// that event: reusing the alias with a different payload is a conflict and
// must not append a second heartbeat audit fact.
func TestHeartbeatAliasKeyBindsOriginalPayload(t *testing.T) {
	s, actor := hbFixture(t)
	ctx := context.Background()
	req := hbRequest("1", "1", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute))
	original, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-alias-a", req)
	if err != nil {
		t.Fatalf("original heartbeat: %v", err)
	}
	alias, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-alias-b", req)
	if err != nil {
		t.Fatalf("alias replay: %v", err)
	}
	if !alias.AcceptedAt.Equal(original.AcceptedAt) || alias.Sequence != original.Sequence || alias.AccountVersion != original.AccountVersion {
		t.Fatalf("alias receipt=%+v want original %+v", alias, original)
	}
	changed := req
	changed.Sequence = "2"
	if _, err := s.ReceiveHeartbeat(ctx, actor, "rt_acc_hb", "hb-alias-b", changed); Code(err) != taskCodeIdempotency {
		t.Fatalf("alias with different payload code=%q want %s", Code(err), taskCodeIdempotency)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.runtime_heartbeat WHERE workspace_id='ws_fixture' AND account_id='acc_hb'`); got != 1 {
		t.Fatalf("alias conflict left %d audit facts, want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.heartbeat_idempotency WHERE workspace_id='ws_fixture' AND account_id='acc_hb'`); got != 2 {
		t.Fatalf("alias bindings=%d want both keys bound", got)
	}
	if got := scalarInt(t, s, `SELECT count(DISTINCT (owner_epoch,sequence)) FROM core.heartbeat_idempotency WHERE workspace_id='ws_fixture' AND account_id='acc_hb'`); got != 1 {
		t.Fatalf("aliases span %d heartbeat facts, want 1", got)
	}
}

// Contract: common.json#/Capability required members are non-null. Raw
// validation must reject null before generated types erase it.
func TestHeartbeatRawCapabilityRequiredNulls(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]map[string]any)
	}{
		{"account_id", func(c []map[string]any) { c[0]["account_id"] = nil }},
		{"actions", func(c []map[string]any) { c[0]["actions"] = nil }},
		{"effect", func(c []map[string]any) {
			c[0]["actions"].(map[string]any)["search_groups"].(map[string]any)["effect"] = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, actor := hbFixture(t)
			req := hbRequest("1", "1", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute))
			var body map[string]any
			if err := json.Unmarshal(hbJSONBody(t, req), &body); err != nil {
				t.Fatal(err)
			}
			capabilities := hbCapabilityJSON(t, nil)
			tc.mutate(capabilities)
			body["capabilities"] = capabilities
			payload, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			headers := map[string]string{
				"Authorization":   "Runtime hb-test-token",
				"Content-Type":    "application/json",
				"Idempotency-Key": "raw-null-" + tc.name,
			}
			recorder := hbServe(t, s.RuntimeHandler(hbAuth(actor)), http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", payload, headers)
			if recorder.Code != http.StatusBadRequest || hbErrorCode(t, recorder) != taskCodeValidation {
				t.Fatalf("status=%d body=%s want 400 %s", recorder.Code, recorder.Body.String(), taskCodeValidation)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.runtime_heartbeat`); got != 0 {
				t.Fatalf("null required capability persisted %d audit rows", got)
			}
		})
	}
}

// Contract: common.json#/Capability required booleans. The raw request must
// be validated before generated types can re-serialize a missing or null field
// into false.
func TestHeartbeatRawCapabilityRequiredBooleans(t *testing.T) {
	for _, field := range []string{"supported", "fresh_read"} {
		for _, mode := range []string{"missing", "null"} {
			t.Run(field+"/"+mode, func(t *testing.T) {
				s, actor := hbFixture(t)
				req := hbRequest("1", "1", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute))
				var body map[string]any
				if err := json.Unmarshal(hbJSONBody(t, req), &body); err != nil {
					t.Fatal(err)
				}
				capabilities := hbCapabilityJSON(t, nil)
				action := capabilities[0]["actions"].(map[string]any)["search_groups"].(map[string]any)
				if mode == "missing" {
					delete(action, field)
				} else {
					action[field] = nil
				}
				body["capabilities"] = capabilities
				payload, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				handler := s.RuntimeHandler(hbAuth(actor))
				headers := map[string]string{
					"Authorization":   "Runtime hb-test-token",
					"Content-Type":    "application/json",
					"Idempotency-Key": "raw-cap-" + field + "-" + mode,
				}
				recorder := hbServe(t, handler, http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", payload, headers)
				if recorder.Code != http.StatusBadRequest || hbErrorCode(t, recorder) != taskCodeValidation {
					t.Fatalf("status=%d body=%s want 400 %s", recorder.Code, recorder.Body.String(), taskCodeValidation)
				}
				if got := scalarInt(t, s, `SELECT count(*) FROM core.runtime_heartbeat`); got != 0 {
					t.Fatalf("invalid capability persisted %d audit rows", got)
				}
			})
		}
	}
}

// Contract: common.json#/Capability strings declare maxLength 128 with no
// ASCII restriction, so the bound is Unicode code points, not UTF-8 bytes.
func TestHeartbeatCapabilityUnicodeLength(t *testing.T) {
	for _, field := range []string{"account_id", "adapter_id", "adapter_version", "evidence_ref"} {
		for _, tc := range []struct {
			name  string
			runes int
			code  int
			rows  int
		}{
			{"128", 128, http.StatusAccepted, 1},
			{"129", 129, http.StatusBadRequest, 0},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				s, actor := hbFixture(t)
				value := strings.Repeat("测", tc.runes)
				req := hbRequest("1", "1", contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestLoginStateLoggedIn, time.Now().UTC().Add(time.Minute))
				var body map[string]any
				if err := json.Unmarshal(hbJSONBody(t, req), &body); err != nil {
					t.Fatal(err)
				}
				body["capabilities"] = hbCapabilityJSON(t, map[string]any{field: value})
				payload, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				headers := map[string]string{
					"Authorization":   "Runtime hb-test-token",
					"Content-Type":    "application/json",
					"Idempotency-Key": "unicode-" + field + "-" + tc.name,
				}
				recorder := hbServe(t, s.RuntimeHandler(hbAuth(actor)), http.MethodPost, "/v1/internal/runtimes/rt_acc_hb/heartbeats", payload, headers)
				if recorder.Code != tc.code {
					t.Fatalf("status=%d body=%s want %d", recorder.Code, recorder.Body.String(), tc.code)
				}
				if got := scalarInt(t, s, `SELECT count(*) FROM core.runtime_heartbeat`); got != tc.rows {
					t.Fatalf("rows=%d want %d", got, tc.rows)
				}
			})
		}
	}
}
