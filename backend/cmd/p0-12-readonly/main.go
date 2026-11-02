// Command p0-12-readonly performs the authorized single-account read-only
// chain against one loopback NapCat OneBot endpoint and records the runtime
// heartbeat through the local receiver. It never logs in, sends messages,
// requests joins, writes plugin state, or contacts a non-loopback address.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
)

const (
	maxAttempts         = 10
	maxResponseBytes    = 256 << 10
	oneBotTimeout       = 5 * time.Second
	heartbeatTimeout    = 5 * time.Second
	allowedActionLogin  = "get_login_info"
	allowedActionStatus = "get_status"
	allowedActionGroups = "get_group_list"
)

type callRecord struct {
	Sequence       int             `json:"sequence"`
	Call           string          `json:"call"`
	Attempt        int             `json:"attempt"`
	UTCStart       string          `json:"utc_start"`
	UTCEnd         string          `json:"utc_end"`
	Method         string          `json:"method"`
	URL            string          `json:"url"`
	RequestBody    json.RawMessage `json:"request_body,omitempty"`
	ResponseStatus int             `json:"response_status,omitempty"`
	ResponseBody   json.RawMessage `json:"response_body,omitempty"`
	ResponseBytes  int             `json:"response_bytes,omitempty"`
	Truncated      bool            `json:"truncated,omitempty"`
	ResponseText   string          `json:"response_text,omitempty"`
	Error          string          `json:"error,omitempty"`
	Retry          bool            `json:"retry,omitempty"`
}

type evidence struct {
	dir      string
	requests []callRecord
}

func (e *evidence) add(record callRecord) error {
	e.requests = append(e.requests, record)
	return e.flushRequests()
}

func (e *evidence) flushRequests() error {
	target := filepath.Join(e.dir, "requests.jsonl")
	file, err := os.CreateTemp(e.dir, "requests-*.jsonl")
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, record := range e.requests {
		if err := encoder.Encode(record); err != nil {
			file.Close()
			os.Remove(file.Name())
			return err
		}
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return err
	}
	return os.Rename(file.Name(), target)
}

type runner struct {
	ctx           context.Context
	endpoint      *url.URL
	token         string
	client        *http.Client
	store         *taskengine.Store
	actor         taskengine.RuntimeActor
	workspaceID   string
	runtimeID     string
	accountID     string
	expectedSelf  string
	evidence      *evidence
	attempts      int
	retryUsed     bool
	heartbeatAddr string
	runtimeToken  string
}

func (r *runner) remaining() int { return maxAttempts - r.attempts }

func (r *runner) takeAttempt() error {
	if r.attempts >= maxAttempts {
		return errors.New("read request budget exhausted")
	}
	r.attempts++
	return nil
}

func (r *runner) oneBotCall(call, action string, params any) (json.RawMessage, error) {
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	target := r.endpoint.String() + "/" + action
	attempt := 0
	for {
		attempt++
		if err := r.takeAttempt(); err != nil {
			return nil, err
		}
		record := callRecord{Call: call, Method: http.MethodPost, URL: target, RequestBody: payload,
			Attempt: attempt, Retry: attempt > 1, Sequence: r.attempts}
		record.UTCStart = time.Now().UTC().Format(time.RFC3339Nano)
		request, err := http.NewRequestWithContext(r.ctx, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Authorization", "Bearer "+r.token)
		response, err := r.client.Do(request)
		if err != nil {
			record.Error = "transport: " + err.Error()
			record.UTCEnd = time.Now().UTC().Format(time.RFC3339Nano)
			if err := r.evidence.add(record); err != nil {
				return nil, err
			}
			if attempt == 1 && !r.retryUsed {
				r.retryUsed = true
				continue
			}
			return nil, errors.New("onebot read failed after one retry")
		}
		limited := io.LimitReader(response.Body, maxResponseBytes+1)
		body, readErr := io.ReadAll(limited)
		response.Body.Close()
		record.ResponseStatus = response.StatusCode
		record.ResponseBytes = len(body)
		if len(body) > maxResponseBytes {
			record.Truncated = true
			body = body[:maxResponseBytes]
		}
		if json.Valid(body) {
			record.ResponseBody = append(json.RawMessage(nil), body...)
		} else {
			record.ResponseText = string(body)
		}
		record.UTCEnd = time.Now().UTC().Format(time.RFC3339Nano)
		if err := r.evidence.add(record); err != nil {
			return nil, err
		}
		if response.StatusCode >= 500 && attempt == 1 && !r.retryUsed {
			r.retryUsed = true
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read body: %w", readErr)
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s returned HTTP %d", action, response.StatusCode)
		}
		var envelope struct {
			Status  string          `json:"status"`
			RetCode json.Number     `json:"retcode"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&envelope); err != nil {
			return nil, fmt.Errorf("decode %s envelope", action)
		}
		if envelope.Status != "ok" || envelope.RetCode.String() != "0" {
			return nil, fmt.Errorf("%s rejected: status=%s retcode=%s message=%s", action, envelope.Status, envelope.RetCode.String(), envelope.Message)
		}
		return envelope.Data, nil
	}
}

func (r *runner) heartbeat(connection, login string, at time.Time) (contract.HeartbeatReceipt, []byte, error) {
	var receipt contract.HeartbeatReceipt
	request := contract.HeartbeatRequest{
		WorkspaceId:     r.workspaceID,
		RuntimeId:       r.runtimeID,
		AccountId:       r.accountID,
		OwnerEpoch:      "1",
		Sequence:        "1",
		ConnectionState: contract.HeartbeatRequestConnectionState(connection),
		LoginState:      contract.HeartbeatRequestLoginState(login),
		HeartbeatAt:     at.UTC(),
		Capabilities:    []contract.Capability{},
	}
	body, err := json.Marshal(request)
	if err != nil {
		return receipt, nil, err
	}
	target := "http://" + r.heartbeatAddr + "/v1/internal/runtimes/" + r.runtimeID + "/heartbeats"
	record := callRecord{Call: "heartbeat", Attempt: 1, Method: http.MethodPost, URL: target, RequestBody: body}
	record.Sequence = r.attempts + 1
	record.UTCStart = time.Now().UTC().Format(time.RFC3339Nano)
	ctx, cancel := context.WithTimeout(r.ctx, heartbeatTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return receipt, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "p012-live-heartbeat-1")
	req.Header.Set("Authorization", "Bearer "+r.runtimeToken)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		record.Error = "transport: " + err.Error()
		record.UTCEnd = time.Now().UTC().Format(time.RFC3339Nano)
		if persistErr := r.evidence.add(record); persistErr != nil {
			return receipt, nil, persistErr
		}
		return receipt, nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	record.ResponseStatus = response.StatusCode
	if json.Valid(responseBody) {
		record.ResponseBody = append(json.RawMessage(nil), responseBody...)
	} else {
		record.ResponseText = string(responseBody)
	}
	record.ResponseBytes = len(responseBody)
	record.UTCEnd = time.Now().UTC().Format(time.RFC3339Nano)
	if persistErr := r.evidence.add(record); persistErr != nil {
		return receipt, responseBody, persistErr
	}
	if err != nil {
		return receipt, responseBody, err
	}
	if response.StatusCode != http.StatusAccepted {
		return receipt, responseBody, fmt.Errorf("heartbeat returned HTTP %d: %s", response.StatusCode, string(responseBody))
	}
	var envelope contract.HeartbeatEnvelope
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return receipt, responseBody, err
	}
	return envelope.Data, responseBody, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func loadOneBotConfig(path string) (endpoint string, token string, err error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", "", errors.New("cannot read napcatd state")
	}
	var state struct {
		Instances []struct {
			Label      string `json:"label"`
			WebuiPort  int    `json:"webui_port"`
			OnebotPort int    `json:"onebot_port"`
			Token      string `json:"onebot_token"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return "", "", errors.New("invalid napcatd state")
	}
	matches := 0
	for _, instance := range state.Instances {
		if instance.Label == "36374700" && instance.WebuiPort == 6211 && instance.OnebotPort == 3211 {
			matches++
			token = instance.Token
		}
	}
	if matches != 1 || strings.TrimSpace(token) == "" {
		return "", "", errors.New("approved 6211/3211 account binding missing or ambiguous")
	}
	return "http://127.0.0.1:3211", token, nil
}

func loopbackClient() *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(ips) == 0 {
				return nil, errors.New("endpoint did not resolve")
			}
			for _, resolved := range ips {
				if !resolved.IP.IsLoopback() {
					return nil, errors.New("endpoint resolved outside loopback")
				}
			}
			dialer := net.Dialer{Timeout: 3 * time.Second}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
	return &http.Client{Timeout: oneBotTimeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func newRuntimeToken() string {
	var buffer [24]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buffer[:])
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "p0-12 readonly:", err)
		os.Exit(1)
	}
}

func run() error {
	evidenceDir := os.Getenv("P012_EVIDENCE_DIR")
	if evidenceDir == "" {
		return errors.New("P012_EVIDENCE_DIR is required")
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return err
	}
	dsn := os.Getenv("BBA_FIXTURE_DSN")
	if dsn == "" {
		return errors.New("BBA_FIXTURE_DSN is required")
	}
	expectedSelf := envOr("P012_EXPECTED_SELF_ID", "36374700")
	endpointValue, token, err := loadOneBotConfig(os.Getenv("P012_STATE_FILE"))
	if err != nil {
		return fmt.Errorf("napcatd state: %w", err)
	}
	parsed, err := url.Parse(endpointValue)
	if err != nil || parsed.Hostname() == "" {
		return errors.New("invalid onebot endpoint")
	}
	ip := net.ParseIP(parsed.Hostname())
	if parsed.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("onebot endpoint must be loopback")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := taskengine.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	workspaceID := envOr("P012_WORKSPACE_ID", "ws_p012")
	runtimeID := envOr("P012_RUNTIME_ID", "runtime_36374700")
	accountID := envOr("P012_ACCOUNT_ID", "acc_36374700")
	if _, err = store.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES($1) ON CONFLICT DO NOTHING`, workspaceID); err != nil {
		return err
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id) VALUES($1,'principal_p012') ON CONFLICT DO NOTHING`, workspaceID); err != nil {
		return err
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit,heartbeat_at)
		VALUES($1,$2,$3,'adapter_p012',$4,'verified','unknown','unknown','active','unknown',false,false,1,NULL)`,
		workspaceID, accountID, runtimeID, expectedSelf); err != nil {
		return err
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority)
		VALUES($1,$3,$2,1,false) ON CONFLICT (workspace_id,runtime_id,account_id) DO NOTHING`,
		workspaceID, accountID, runtimeID); err != nil {
		return err
	}

	actor := taskengine.RuntimeActor{WorkspaceID: workspaceID, RuntimeID: runtimeID, OwnerEpoch: 1, AccountIDs: []string{accountID}}
	runtimeToken := newRuntimeToken()
	handler := store.RuntimeHandler(func(request *http.Request) (taskengine.RuntimeActor, error) {
		if request.Header.Get("Authorization") != "Bearer "+runtimeToken {
			return taskengine.RuntimeActor{}, fmt.Errorf("unauthenticated")
		}
		return actor, nil
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	r := &runner{
		ctx: ctx, endpoint: parsed, token: token, client: loopbackClient(),
		store: store, actor: actor, workspaceID: workspaceID, runtimeID: runtimeID,
		accountID: accountID, expectedSelf: expectedSelf, evidence: &evidence{dir: evidenceDir},
		heartbeatAddr: listener.Addr().String(), runtimeToken: runtimeToken,
	}

	summary := map[string]any{
		"utc_started":      time.Now().UTC().Format(time.RFC3339Nano),
		"endpoint":         parsed.String(),
		"workspace_id":     workspaceID,
		"runtime_id":       runtimeID,
		"account_id":       accountID,
		"expected_self_id": expectedSelf,
		"max_attempts":     maxAttempts,
		"code_sha":         os.Getenv("P012_CODE_SHA"),
		"environment":      os.Getenv("P012_ENVIRONMENT"),
		"token_present":    token != "",
	}

	loginData, err := r.oneBotCall("get_login_info", allowedActionLogin, map[string]any{})
	if err != nil {
		return writeFailure(evidenceDir, summary, r, err)
	}
	var login struct {
		UserID   any    `json:"user_id"`
		Nickname string `json:"nickname"`
	}
	if err := decodeData(loginData, &login); err != nil {
		return writeFailure(evidenceDir, summary, r, err)
	}
	selfID := ""
	switch value := login.UserID.(type) {
	case json.Number:
		selfID = value.String()
	case string:
		selfID = value
	}
	summary["observed_self_id"] = selfID
	summary["nickname"] = login.Nickname
	if selfID != expectedSelf {
		return writeFailure(evidenceDir, summary, r, fmt.Errorf("self_id mismatch: observed %q expected %q", selfID, expectedSelf))
	}

	statusData, err := r.oneBotCall("get_status", allowedActionStatus, map[string]any{})
	if err != nil {
		return writeFailure(evidenceDir, summary, r, err)
	}
	var status map[string]any
	if err := decodeData(statusData, &status); err != nil {
		return writeFailure(evidenceDir, summary, r, err)
	}
	summary["status"] = status

	groupData, err := r.oneBotCall("get_group_list", allowedActionGroups, map[string]any{})
	if err != nil {
		return writeFailure(evidenceDir, summary, r, err)
	}
	var groups []json.RawMessage
	if err := decodeData(groupData, &groups); err != nil {
		return writeFailure(evidenceDir, summary, r, err)
	}
	summary["group_list_entries"] = len(groups)

	connection := "unknown"
	if online, ok := status["online"].(bool); ok && online {
		connection = "online"
	}
	heartbeatAt := time.Now().UTC()
	receipt, receiptBody, err := r.heartbeat(connection, string(contract.HeartbeatRequestLoginStateLoggedIn), heartbeatAt)
	if err != nil {
		return writeFailure(evidenceDir, summary, r, err)
	}
	summary["heartbeat_receipt"] = receipt
	summary["heartbeat_response"] = json.RawMessage(receiptBody)
	summary["attempts_used"] = r.attempts
	summary["remaining_budget"] = r.remaining()
	summary["utc_finished"] = time.Now().UTC().Format(time.RFC3339Nano)

	if err := r.evidence.flushRequests(); err != nil {
		return err
	}
	if err := writeDBEvidence(ctx, r, evidenceDir); err != nil {
		return err
	}
	return writeJSON(filepath.Join(evidenceDir, "summary.json"), summary)
}

func decodeData(raw json.RawMessage, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(dst)
}

func writeFailure(dir string, summary map[string]any, r *runner, cause error) error {
	summary["error"] = cause.Error()
	summary["attempts_used"] = r.attempts
	summary["remaining_budget"] = r.remaining()
	summary["utc_finished"] = time.Now().UTC().Format(time.RFC3339Nano)
	_ = r.evidence.flushRequests()
	_ = writeJSON(filepath.Join(dir, "summary.json"), summary)
	return cause
}

func writeDBEvidence(ctx context.Context, r *runner, dir string) error {
	var version int
	var connection, login, control, selfID string
	var heartbeatAt *time.Time
	if err := r.store.Pool.QueryRow(ctx, `SELECT version,connection_state,login_state,control_state,self_id,heartbeat_at
		FROM core.account WHERE workspace_id=$1 AND id=$2`, r.workspaceID, r.accountID).
		Scan(&version, &connection, &login, &control, &selfID, &heartbeatAt); err != nil {
		return err
	}
	rows, err := r.store.Pool.Query(ctx, `SELECT owner_epoch,sequence,connection_state,login_state,heartbeat_at,disposition,request_id,digest,received_at
		FROM core.runtime_heartbeat WHERE workspace_id=$1 AND account_id=$2 ORDER BY received_at`, r.workspaceID, r.accountID)
	if err != nil {
		return err
	}
	defer rows.Close()
	heartbeats := make([]map[string]any, 0, 2)
	for rows.Next() {
		var epoch, sequence int64
		var conn, login, disposition, requestID, digest string
		var at, received time.Time
		if err := rows.Scan(&epoch, &sequence, &conn, &login, &at, &disposition, &requestID, &digest, &received); err != nil {
			return err
		}
		heartbeats = append(heartbeats, map[string]any{
			"owner_epoch": epoch, "sequence": sequence, "connection_state": conn, "login_state": login,
			"heartbeat_at": at.UTC().Format(time.RFC3339Nano), "disposition": disposition,
			"request_id": requestID, "digest": digest, "received_at": received.UTC().Format(time.RFC3339Nano),
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	proof := map[string]any{
		"utc_read":     time.Now().UTC().Format(time.RFC3339Nano),
		"account":      map[string]any{"version": version, "connection_state": connection, "login_state": login, "control_state": control, "self_id": selfID, "heartbeat_at": heartbeatAt},
		"heartbeats":   heartbeats,
		"schema_check": "select max(version) from core.schema_version",
	}
	var schema int
	if err := r.store.Pool.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&schema); err != nil {
		return err
	}
	proof["schema_version"] = schema
	return writeJSON(filepath.Join(dir, "db-evidence.json"), proof)
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}
