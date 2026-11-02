package main

// P0-14 engine smoke: four live accounts, one isolated PostgreSQL fixture, and
// real loopback HTTP surfaces. The smoke proves identity/runtime mapping,
// heartbeat application, and dry-run task creation/read isolation without ever
// dispatching a QQ action or starting a scheduler/worker.
//
// The database must be the disposable `/fixture` Unix socket. Every run creates
// a fresh database under the fixture admin connection, migrates it to schema
// 11, and leaves that database in place so an external evidence step can run
// independent SQL. Tokens are generated per run in memory and never written to
// evidence.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
)

const (
	engineWorkspaceID = "ws_p014_engine"
	enginePrincipalID = "principal_p014_engine"
	engineSenderID    = "sender_p014_engine"
	engineEvidence    = "engine-db-evidence.json"
)

type engineAccount struct {
	OneBotPort int
	InstanceID string
	AccountID  string
	RuntimeID  string
	AdapterID  string
	SelfID     string
	Token      string
}

type engineSmoke struct {
	accounts []engineAccount
	client   *http.Client
	baseURL  string
	server   *http.Server
}

type engineHeartbeatEvidence struct {
	Status         int    `json:"status"`
	AccountVersion int    `json:"account_version"`
	ControlState   string `json:"control_state"`
	ReplayStatus   int    `json:"replay_status"`
	ReplayExact    bool   `json:"replay_exact"`
	Rows           int    `json:"rows"`
}

type engineTaskEvidence struct {
	TaskID            string   `json:"task_id"`
	TargetGroupID     string   `json:"target_group_id"`
	AllowedAccountIDs []string `json:"allowed_account_ids"`
	CreateStatus      int      `json:"create_status"`
	State             string   `json:"state"`
	Mode              string   `json:"mode"`
	ReplayStatus      int      `json:"replay_status"`
	ReplayExact       bool     `json:"replay_exact"`
	ReadStatus        int      `json:"read_status"`
	ReadState         string   `json:"read_state"`
	TaskAccountRows   int      `json:"task_account_rows"`
}

type engineAccountDatabaseEvidence struct {
	RuntimeID           string `json:"runtime_id"`
	SelfID              string `json:"self_id"`
	IdentityStatus      string `json:"identity_status"`
	ConnectionState     string `json:"connection_state"`
	LoginState          string `json:"login_state"`
	ControlState        string `json:"control_state"`
	Version             int    `json:"version"`
	RequestJoinVerified bool   `json:"request_join_verified"`
	MembershipVerified  bool   `json:"membership_verified"`
	MembershipAuthority bool   `json:"membership_authority"`
	CredentialActive    bool   `json:"credential_active"`
	HeartbeatRows       int    `json:"heartbeat_rows"`
}

type engineAccountEvidence struct {
	OneBotPort     int                           `json:"onebot_port"`
	InstanceID     string                        `json:"instance_id"`
	AccountID      string                        `json:"account_id"`
	RuntimeID      string                        `json:"runtime_id"`
	AdapterID      string                        `json:"adapter_id"`
	ExpectedSelfID string                        `json:"expected_self_id"`
	ObservedSelfID string                        `json:"observed_self_id"`
	Heartbeat      engineHeartbeatEvidence       `json:"heartbeat"`
	Task           engineTaskEvidence            `json:"task"`
	Database       engineAccountDatabaseEvidence `json:"database"`
}

type engineMappingEvidence struct {
	TaskID    string `json:"task_id"`
	AccountID string `json:"account_id"`
	RuntimeID string `json:"runtime_id"`
	SelfID    string `json:"self_id"`
	Mode      string `json:"mode"`
	State     string `json:"state"`
}

// validateEngineSmokeEnv is the side-effect-free preflight. The runner calls it
// before opening the NapCat state file or issuing any QQ request, and
// runEngineSmoke repeats the same guard independently.
func validateEngineSmokeEnv() error {
	if os.Getenv("BBA_FIXTURE_ONLY") != "1" {
		return errors.New("BBA_FIXTURE_ONLY=1 is required")
	}
	dsn := strings.TrimSpace(os.Getenv("BBA_FIXTURE_DSN"))
	if dsn == "" {
		return errors.New("BBA_FIXTURE_DSN is required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("BBA_FIXTURE_DSN is invalid")
	}
	if cfg.User != "bba_fixture" || cfg.Database != "bba_fixture_admin" {
		return errors.New("BBA_FIXTURE_DSN must name the bba_fixture fixture admin database")
	}
	if cfg.Password != "" {
		return errors.New("BBA_FIXTURE_DSN must not carry a password")
	}
	if cfg.Host != "/fixture" {
		return errors.New("BBA_FIXTURE_DSN must use the /fixture Unix socket, not TCP")
	}
	if cfg.Port != 5432 {
		return errors.New("BBA_FIXTURE_DSN must not override the PostgreSQL port")
	}
	if len(cfg.Fallbacks) != 0 {
		return errors.New("BBA_FIXTURE_DSN must not define fallback hosts or TCP addresses")
	}
	return nil
}

func validateEngineAccounts(accounts []accountResult) error {
	if len(accounts) != 4 {
		return fmt.Errorf("engine smoke requires exactly four verified accounts, got %d", len(accounts))
	}
	ports := make(map[int]bool, 4)
	instances := make(map[string]bool, 4)
	selfIDs := make(map[string]bool, 4)
	for index, account := range accounts {
		if account.OneBotPort < 1 || account.OneBotPort > 65535 || ports[account.OneBotPort] {
			return fmt.Errorf("account %d has an invalid or duplicate OneBot port", index)
		}
		ports[account.OneBotPort] = true
		instanceID := strings.TrimSpace(account.InstanceID)
		if instanceID == "" || instances[instanceID] {
			return fmt.Errorf("account %d has a blank or duplicate instance id", index)
		}
		instances[instanceID] = true
		if !validID(account.ExpectedSelfID) || selfIDs[account.ExpectedSelfID] {
			return fmt.Errorf("account %d has an invalid or duplicate expected self_id", index)
		}
		if !validID(account.ObservedSelfID) {
			return fmt.Errorf("account %d has an invalid observed self_id", index)
		}
		if account.ObservedSelfID != account.ExpectedSelfID {
			return fmt.Errorf("account %d observed self_id %s does not match binding %s", index, account.ObservedSelfID, account.ExpectedSelfID)
		}
		selfIDs[account.ExpectedSelfID] = true
		if !account.Online || !account.Good {
			return fmt.Errorf("account %d is not online and good", index)
		}
		if account.GroupCount < 0 {
			return fmt.Errorf("account %d has a negative group count", index)
		}
		if strings.TrimSpace(account.Error) != "" {
			return fmt.Errorf("account %d carries a read error", index)
		}
	}
	return nil
}

func prepareEngineAccounts(accounts []accountResult) ([]engineAccount, error) {
	if err := validateEngineAccounts(accounts); err != nil {
		return nil, err
	}
	sorted := append([]accountResult(nil), accounts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].OneBotPort < sorted[j].OneBotPort })
	out := make([]engineAccount, 0, len(sorted))
	for _, account := range sorted {
		token, err := engineNewToken()
		if err != nil {
			return nil, fmt.Errorf("generate runtime token: %w", err)
		}
		selfID := account.ObservedSelfID
		out = append(out, engineAccount{
			OneBotPort: account.OneBotPort,
			InstanceID: strings.TrimSpace(account.InstanceID),
			AccountID:  "acct_p014_" + selfID,
			RuntimeID:  "rt_p014_" + selfID,
			AdapterID:  "adapter_p014_" + selfID,
			SelfID:     selfID,
			Token:      token,
		})
	}
	return out, nil
}

func engineNewToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func engineBearer(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func (h *engineSmoke) lookupToken(token string) (engineAccount, bool) {
	if token == "" {
		return engineAccount{}, false
	}
	for _, account := range h.accounts {
		if subtle.ConstantTimeCompare([]byte(token), []byte(account.Token)) == 1 {
			return account, true
		}
	}
	return engineAccount{}, false
}

func (h *engineSmoke) authenticateRuntime(r *http.Request) (taskengine.RuntimeActor, error) {
	account, ok := h.lookupToken(engineBearer(r))
	if !ok {
		return taskengine.RuntimeActor{}, &taskengine.Error{Code: "UNAUTHENTICATED"}
	}
	return taskengine.RuntimeActor{
		WorkspaceID: engineWorkspaceID,
		RuntimeID:   account.RuntimeID,
		OwnerEpoch:  1,
		AccountIDs:  []string{account.AccountID},
	}, nil
}

func (h *engineSmoke) authenticateTask(r *http.Request) (taskengine.Actor, error) {
	account, ok := h.lookupToken(engineBearer(r))
	if !ok {
		return taskengine.Actor{}, &taskengine.Error{Code: "UNAUTHENTICATED"}
	}
	return taskengine.Actor{
		WorkspaceID: engineWorkspaceID,
		PrincipalID: enginePrincipalID,
		RuntimeID:   account.RuntimeID,
		SenderID:    engineSenderID,
		AccountIDs:  []string{account.AccountID},
		Permissions: map[string]bool{"task.create": true, "task.read": true},
	}, nil
}

func newEngineSmoke(store *taskengine.Store, accounts []engineAccount) (*engineSmoke, error) {
	h := &engineSmoke{accounts: accounts}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen loopback: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/internal/runtimes/", store.RuntimeHandler(h.authenticateRuntime))
	taskHandler := store.Handler(h.authenticateTask)
	mux.Handle("/v1/tasks", taskHandler)
	mux.Handle("/v1/tasks/", taskHandler)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	h.server = server
	h.baseURL = "http://" + listener.Addr().String()
	h.client = loopbackClient()
	return h, nil
}

func (h *engineSmoke) close() {
	if h.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = h.server.Shutdown(ctx)
}

func (h *engineSmoke) do(ctx context.Context, method, path string, body []byte, token string, headers map[string]string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := h.client.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: loopback transport failed", method, path)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: response read failed", method, path)
	}
	if len(payload) > maxResponseBytes {
		return 0, nil, fmt.Errorf("%s %s: response too large", method, path)
	}
	return response.StatusCode, payload, nil
}

func engineErrorCode(payload []byte) string {
	var envelope contract.ErrorEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ""
	}
	return envelope.Error.Code
}

func engineHeartbeatRequest(account engineAccount, at time.Time) contract.HeartbeatRequest {
	action := contract.CapabilityAction{
		Effect:      contract.CapabilityActionEffectNone,
		FreshRead:   false,
		Idempotency: contract.CapabilityActionIdempotencyNone,
		Pagination:  contract.CapabilityActionPaginationNone,
		Supported:   false,
	}
	capability := contract.Capability{
		AccountId:         account.AccountID,
		AdapterId:         account.AdapterID,
		AdapterVersion:    "p014-engine-smoke",
		VerificationState: contract.CapabilityVerificationStateUnverified,
		VerifiedAt:        nullable.NewNullNullable[time.Time](),
		EvidenceRef:       nullable.NewNullNullable[string](),
	}
	capability.Actions.SearchGroups = action
	capability.Actions.RequestJoin = action
	capability.Actions.ObserveMembership = action
	capability.Actions.ObserveJoinRequest = action
	capability.Actions.CancelJoinRequest = action
	capability.Actions.SendText = action
	return contract.HeartbeatRequest{
		WorkspaceId:     engineWorkspaceID,
		RuntimeId:       account.RuntimeID,
		AccountId:       account.AccountID,
		OwnerEpoch:      "1",
		Sequence:        "1",
		ConnectionState: contract.HeartbeatRequestConnectionStateOnline,
		LoginState:      contract.HeartbeatRequestLoginStateLoggedIn,
		HeartbeatAt:     at.UTC(),
		Capabilities:    []contract.Capability{capability},
	}
}

func engineTaskRequest(account engineAccount, target string) contract.CreateTaskRequest {
	mode := contract.DryRun
	return contract.CreateTaskRequest{
		Type:                "group.acquire",
		Title:               "p014 engine smoke " + strconv.Itoa(account.OneBotPort),
		JoinReason:          "fixture reason",
		VerificationMessage: "fixture answer",
		Goal: contract.Goal{
			Metric:                      contract.NewUniqueGroupsForAccountPool,
			Count:                       1,
			StrictTargetCap:             true,
			RequireFinalMembershipCheck: true,
		},
		AllowedAccountIds: []string{account.AccountID},
		CandidateSource: contract.CandidateSource{
			Kind:   "fixed",
			Groups: []contract.FixedCandidate{{GroupId: target, SourceRef: "p014_engine_import"}},
		},
		ExecutionMode: &mode,
		DeadlineAt:    time.Now().UTC().Add(time.Hour),
		Limits: contract.TaskLimits{
			MaxCandidates:            1,
			MaxAttemptsPerAction:     1,
			MaxTotalExternalAttempts: 1,
			MaxModelAttempts:         0,
		},
		ApprovalRef: nullable.NewNullNullable[string](),
		Submit:      false,
	}
}

func engineTargetGroup(index int) string {
	return fmt.Sprintf("93%06d", 100+index)
}

func engineTaskHeaders(account engineAccount) map[string]string {
	return map[string]string{
		"X-QQ-Account-ID": account.AccountID,
		"X-QQ-Sender-ID":  engineSenderID,
	}
}

func engineScalar(ctx context.Context, store *taskengine.Store, query string, args ...any) (int, error) {
	var value int
	if err := store.Pool.QueryRow(ctx, query, args...).Scan(&value); err != nil {
		return 0, err
	}
	return value, nil
}

func engineDatabaseName() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "p014_engine_" + hex.EncodeToString(raw[:]), nil
}

func seedEngineAccounts(ctx context.Context, store *taskengine.Store, accounts []engineAccount) error {
	if _, err := store.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES($1)`, engineWorkspaceID); err != nil {
		return fmt.Errorf("seed workspace: %w", err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id,permissions) VALUES($1,$2,'{}'::jsonb)`, engineWorkspaceID, enginePrincipalID); err != nil {
		return fmt.Errorf("seed principal: %w", err)
	}
	for _, account := range accounts {
		if _, err := store.Pool.Exec(ctx, `
			INSERT INTO core.account(
				workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,
				connection_state,login_state,control_state,risk_state,
				request_join_verified,membership_verified,quota_limit,heartbeat_at)
			VALUES($1,$2,$3,$4,$5,'verified','offline','logged_out','paused','unknown',false,false,1,NULL)`,
			engineWorkspaceID, account.AccountID, account.RuntimeID, account.InstanceID, account.SelfID); err != nil {
			return fmt.Errorf("seed account %s: %w", account.AccountID, err)
		}
		if _, err := store.Pool.Exec(ctx, `
			INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority,active)
			VALUES($1,$2,$3,1,false,true)`,
			engineWorkspaceID, account.RuntimeID, account.AccountID); err != nil {
			return fmt.Errorf("seed runtime credential %s: %w", account.RuntimeID, err)
		}
	}
	return nil
}

func accountProjection(ctx context.Context, store *taskengine.Store, account engineAccount) (engineAccountDatabaseEvidence, error) {
	var out engineAccountDatabaseEvidence
	err := store.Pool.QueryRow(ctx, `
		SELECT a.runtime_id,a.self_id,a.identity_status,a.connection_state,a.login_state,a.control_state,a.version,
			a.request_join_verified,a.membership_verified,rc.membership_authority,rc.active,
			(SELECT count(*) FROM core.runtime_heartbeat h
			 WHERE h.workspace_id=a.workspace_id AND h.runtime_id=a.runtime_id AND h.account_id=a.id)
		FROM core.account a
		JOIN core.runtime_credentials rc
		  ON (rc.workspace_id,rc.runtime_id,rc.account_id)=(a.workspace_id,a.runtime_id,a.id)
		WHERE a.workspace_id=$1 AND a.id=$2`,
		engineWorkspaceID, account.AccountID).Scan(
		&out.RuntimeID, &out.SelfID, &out.IdentityStatus, &out.ConnectionState, &out.LoginState, &out.ControlState, &out.Version,
		&out.RequestJoinVerified, &out.MembershipVerified, &out.MembershipAuthority, &out.CredentialActive,
		&out.HeartbeatRows)
	if err != nil {
		return out, fmt.Errorf("read account projection %s: %w", account.AccountID, err)
	}
	return out, nil
}

func verifyAccountProjection(account engineAccount, got engineAccountDatabaseEvidence) error {
	if got.RuntimeID != account.RuntimeID || got.SelfID != account.SelfID || got.IdentityStatus != "verified" {
		return fmt.Errorf("account %s identity projection mismatch", account.AccountID)
	}
	if got.ConnectionState != "online" || got.LoginState != "logged_in" || got.ControlState != "paused" {
		return fmt.Errorf("account %s projection connection=%s login=%s control=%s", account.AccountID, got.ConnectionState, got.LoginState, got.ControlState)
	}
	if got.Version != 2 {
		return fmt.Errorf("account %s version=%d, want 2 after one applied heartbeat", account.AccountID, got.Version)
	}
	if got.RequestJoinVerified || got.MembershipVerified || got.MembershipAuthority || !got.CredentialActive {
		return fmt.Errorf("account %s was seeded with an unexpected write capability", account.AccountID)
	}
	if got.HeartbeatRows != 1 {
		return fmt.Errorf("account %s heartbeat rows=%d, want 1", account.AccountID, got.HeartbeatRows)
	}
	return nil
}

func runEngineSmoke(ctx context.Context, evidenceDir string, accounts []accountResult) (proof map[string]any, err error) {
	proof = map[string]any{
		"round":       "R22",
		"mode":        "engine_smoke",
		"writes":      0,
		"qq_writes":   0,
		"tokens":      "generated in memory only; never persisted",
		"utc_started": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if strings.TrimSpace(evidenceDir) == "" {
		proof["outcome"] = "FAIL"
		proof["error"] = "engine smoke evidence directory is required"
		proof["utc_finished"] = time.Now().UTC().Format(time.RFC3339Nano)
		return proof, errors.New("engine smoke evidence directory is required")
	}
	defer func() {
		if err != nil {
			proof["outcome"] = "FAIL"
			proof["error"] = err.Error()
		} else {
			proof["outcome"] = "PASS"
		}
		proof["utc_finished"] = time.Now().UTC().Format(time.RFC3339Nano)
		if mkErr := os.MkdirAll(evidenceDir, 0o755); mkErr != nil {
			if err == nil {
				err = fmt.Errorf("create engine evidence directory: %w", mkErr)
				proof["outcome"] = "FAIL"
				proof["error"] = err.Error()
			}
			return
		}
		if writeErr := writeJSON(filepath.Join(evidenceDir, engineEvidence), proof); writeErr != nil && err == nil {
			err = fmt.Errorf("write engine evidence: %w", writeErr)
		}
	}()
	err = runEngineSmokeProof(ctx, proof, accounts)
	return proof, err
}

func runEngineSmokeProof(ctx context.Context, proof map[string]any, accounts []accountResult) error {
	if err := validateEngineSmokeEnv(); err != nil {
		return err
	}
	engineAccounts, err := prepareEngineAccounts(accounts)
	if err != nil {
		return err
	}
	live := make([]map[string]any, 0, len(engineAccounts))
	for _, account := range engineAccounts {
		live = append(live, map[string]any{
			"onebot_port": account.OneBotPort,
			"instance_id": account.InstanceID,
			"account_id":  account.AccountID,
			"runtime_id":  account.RuntimeID,
			"self_id":     account.SelfID,
		})
	}
	proof["live_inputs"] = live

	dsn := os.Getenv("BBA_FIXTURE_DSN")
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("BBA_FIXTURE_DSN is invalid")
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect fixture admin: %w", err)
	}
	defer admin.Close(ctx)
	database, err := engineDatabaseName()
	if err != nil {
		return fmt.Errorf("generate fixture database name: %w", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		return fmt.Errorf("create fixture database: %w", err)
	}
	proof["fixture"] = map[string]any{
		"host":        "/fixture",
		"database":    database,
		"unix_socket": true,
		"tcp":         false,
		"retained":    true,
	}
	parsed := &url.URL{Scheme: "postgres", User: url.User(cfg.User), Path: "/" + database}
	query := parsed.Query()
	query.Set("host", cfg.Host)
	query.Set("port", strconv.Itoa(int(cfg.Port)))
	query.Set("sslmode", "disable")
	parsed.RawQuery = query.Encode()
	store, err := taskengine.Open(ctx, parsed.String())
	if err != nil {
		return fmt.Errorf("open engine fixture: %w", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate engine fixture: %w", err)
	}
	schema, err := engineScalar(ctx, store, `SELECT COALESCE(max(version),0) FROM core.schema_version`)
	if err != nil {
		return fmt.Errorf("read fixture schema version: %w", err)
	}
	if head := taskengine.SchemaHead(); schema != head {
		return fmt.Errorf("engine fixture schema=%d, want %d", schema, head)
	}
	proof["schema_version"] = schema
	return runEngineProof(ctx, proof, store, engineAccounts)
}

func runEngineProof(ctx context.Context, proof map[string]any, store *taskengine.Store, accounts []engineAccount) error {
	if err := seedEngineAccounts(ctx, store, accounts); err != nil {
		return err
	}
	proof["seeded"] = map[string]any{
		"workspace_id":                 engineWorkspaceID,
		"principal_id":                 enginePrincipalID,
		"principal_permissions":        map[string]any{},
		"account_state":                "offline/logged_out/paused",
		"request_join_verified":        false,
		"membership_verified":          false,
		"runtime_membership_authority": false,
		"runtime_credential_active":    true,
		"declared_capabilities":        "six actions unverified and unsupported",
	}

	h, err := newEngineSmoke(store, accounts)
	if err != nil {
		return err
	}
	defer h.close()
	proof["loopback"] = map[string]any{
		"address":         h.baseURL,
		"runtime_handler": "/v1/internal/runtimes/{runtime_id}/heartbeats",
		"task_handler":    "/v1/tasks",
		"redirects":       false,
	}

	evidence := make([]engineAccountEvidence, len(accounts))
	proof["accounts"] = evidence
	for index, account := range accounts {
		evidence[index] = engineAccountEvidence{
			OneBotPort:     account.OneBotPort,
			InstanceID:     account.InstanceID,
			AccountID:      account.AccountID,
			RuntimeID:      account.RuntimeID,
			AdapterID:      account.AdapterID,
			ExpectedSelfID: account.SelfID,
			ObservedSelfID: account.SelfID,
		}
	}

	// Real loopback heartbeats, applied once and replayed with the same
	// idempotency key. The replay must return the exact same receipt and must
	// not add a second audit row or bump the account version again.
	for index, account := range accounts {
		request := engineHeartbeatRequest(account, time.Now().UTC())
		body, err := json.Marshal(request)
		if err != nil {
			return err
		}
		path := "/v1/internal/runtimes/" + account.RuntimeID + "/heartbeats"
		key := "p014-engine-heartbeat-" + account.AccountID
		status, payload, err := h.do(ctx, http.MethodPost, path, body, account.Token, map[string]string{"Idempotency-Key": key})
		if err != nil {
			return err
		}
		if status != http.StatusAccepted {
			return fmt.Errorf("account %s heartbeat status=%d code=%s, want 202", account.AccountID, status, engineErrorCode(payload))
		}
		var envelope contract.HeartbeatEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return fmt.Errorf("account %s heartbeat receipt is invalid", account.AccountID)
		}
		receipt := envelope.Data
		if receipt.AccountId != account.AccountID || receipt.RuntimeId != account.RuntimeID ||
			receipt.OwnerEpoch != "1" || receipt.Sequence != "1" || receipt.AccountVersion != 2 ||
			string(receipt.ControlState) != "paused" {
			return fmt.Errorf("account %s heartbeat receipt mismatch", account.AccountID)
		}
		replayStatus, replayPayload, err := h.do(ctx, http.MethodPost, path, body, account.Token, map[string]string{"Idempotency-Key": key})
		if err != nil {
			return err
		}
		if replayStatus != http.StatusAccepted {
			return fmt.Errorf("account %s heartbeat replay status=%d code=%s", account.AccountID, replayStatus, engineErrorCode(replayPayload))
		}
		var replayEnvelope contract.HeartbeatEnvelope
		if err := json.Unmarshal(replayPayload, &replayEnvelope); err != nil {
			return fmt.Errorf("account %s heartbeat replay receipt is invalid", account.AccountID)
		}
		if !reflect.DeepEqual(replayEnvelope.Data, receipt) {
			return fmt.Errorf("account %s heartbeat replay receipt changed", account.AccountID)
		}
		evidence[index].Heartbeat = engineHeartbeatEvidence{
			Status:         status,
			AccountVersion: receipt.AccountVersion,
			ControlState:   string(receipt.ControlState),
			ReplayStatus:   replayStatus,
			ReplayExact:    true,
		}
	}

	// Crossed identities must be rejected before any projection is touched.
	crossA, crossB := accounts[0], accounts[1]
	crossRuntimeBody, err := json.Marshal(engineHeartbeatRequest(crossB, time.Now().UTC()))
	if err != nil {
		return err
	}
	status, payload, err := h.do(ctx, http.MethodPost, "/v1/internal/runtimes/"+crossB.RuntimeID+"/heartbeats",
		crossRuntimeBody, crossA.Token, map[string]string{"Idempotency-Key": "p014-engine-cross-runtime-" + crossA.AccountID})
	if err != nil {
		return err
	}
	if status != http.StatusNotFound || engineErrorCode(payload) != "RESOURCE_NOT_FOUND" {
		return fmt.Errorf("cross-runtime heartbeat status=%d code=%s, want 404/RESOURCE_NOT_FOUND", status, engineErrorCode(payload))
	}
	crossRuntime := map[string]any{"status": status, "code": engineErrorCode(payload)}

	mixed := engineHeartbeatRequest(crossB, time.Now().UTC())
	mixed.RuntimeId = crossA.RuntimeID
	crossAccountBody, err := json.Marshal(mixed)
	if err != nil {
		return err
	}
	status, payload, err = h.do(ctx, http.MethodPost, "/v1/internal/runtimes/"+crossA.RuntimeID+"/heartbeats",
		crossAccountBody, crossA.Token, map[string]string{"Idempotency-Key": "p014-engine-cross-account-" + crossA.AccountID})
	if err != nil {
		return err
	}
	if status != http.StatusNotFound || engineErrorCode(payload) != "RESOURCE_NOT_FOUND" {
		return fmt.Errorf("cross-account heartbeat status=%d code=%s, want 404/RESOURCE_NOT_FOUND", status, engineErrorCode(payload))
	}
	crossAccount := map[string]any{"status": status, "code": engineErrorCode(payload)}

	for index, account := range accounts {
		got, err := accountProjection(ctx, store, account)
		if err != nil {
			return err
		}
		if err := verifyAccountProjection(account, got); err != nil {
			return err
		}
		evidence[index].Database = got
		evidence[index].Heartbeat.Rows = got.HeartbeatRows
	}

	// Dry-run task create/read through the real task HTTP surface. The task is
	// allowed for exactly its own account. A create replay with the same key
	// and body must return the exact same projection without a second row.
	senderHeaders := make([]map[string]string, len(accounts))
	for index, account := range accounts {
		target := engineTargetGroup(index)
		request := engineTaskRequest(account, target)
		body, err := json.Marshal(request)
		if err != nil {
			return err
		}
		headers := engineTaskHeaders(account)
		headers["Idempotency-Key"] = "p014-engine-task-" + account.AccountID
		senderHeaders[index] = engineTaskHeaders(account)
		status, payload, err := h.do(ctx, http.MethodPost, "/v1/tasks", body, account.Token, headers)
		if err != nil {
			return err
		}
		if status != http.StatusCreated {
			return fmt.Errorf("account %s task create status=%d code=%s", account.AccountID, status, engineErrorCode(payload))
		}
		var envelope contract.TaskEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return fmt.Errorf("account %s task create response is invalid", account.AccountID)
		}
		task := envelope.Data
		if task.Id == "" || task.State != contract.TaskStateDRAFT || task.Type != contract.TaskTypeGroupAcquire ||
			task.ExecutionMode != contract.DryRun || task.WorkspaceId != engineWorkspaceID ||
			task.GoalCount != 1 || task.Title != "p014 engine smoke "+strconv.Itoa(account.OneBotPort) {
			return fmt.Errorf("account %s task projection mismatch", account.AccountID)
		}
		replayStatus, replayPayload, err := h.do(ctx, http.MethodPost, "/v1/tasks", body, account.Token, headers)
		if err != nil {
			return err
		}
		if replayStatus != http.StatusCreated {
			return fmt.Errorf("account %s task create replay status=%d code=%s", account.AccountID, replayStatus, engineErrorCode(replayPayload))
		}
		var replayEnvelope contract.TaskEnvelope
		if err := json.Unmarshal(replayPayload, &replayEnvelope); err != nil {
			return fmt.Errorf("account %s task create replay response is invalid", account.AccountID)
		}
		if !reflect.DeepEqual(replayEnvelope.Data, task) {
			return fmt.Errorf("account %s task create replay changed or created another task", account.AccountID)
		}
		readStatus, readPayload, err := h.do(ctx, http.MethodGet, "/v1/tasks/"+task.Id, nil, account.Token, senderHeaders[index])
		if err != nil {
			return err
		}
		if readStatus != http.StatusOK {
			return fmt.Errorf("account %s task read status=%d code=%s", account.AccountID, readStatus, engineErrorCode(readPayload))
		}
		var readEnvelope contract.TaskEnvelope
		if err := json.Unmarshal(readPayload, &readEnvelope); err != nil {
			return fmt.Errorf("account %s task read response is invalid", account.AccountID)
		}
		read := readEnvelope.Data
		if read.Id != task.Id || read.State != task.State || read.ExecutionMode != task.ExecutionMode ||
			read.WorkspaceId != task.WorkspaceId || read.GoalCount != task.GoalCount || read.Version != task.Version {
			return fmt.Errorf("account %s task read projection mismatch", account.AccountID)
		}
		evidence[index].Task = engineTaskEvidence{
			TaskID:            task.Id,
			TargetGroupID:     target,
			AllowedAccountIDs: []string{account.AccountID},
			CreateStatus:      status,
			State:             string(task.State),
			Mode:              string(task.ExecutionMode),
			ReplayStatus:      replayStatus,
			ReplayExact:       true,
			ReadStatus:        readStatus,
			ReadState:         string(read.State),
		}
	}

	// A second runtime may not read the first runtime's task, and a create
	// naming a foreign account must fail closed without adding a task row.
	foreignReadStatus, foreignReadPayload, err := h.do(ctx, http.MethodGet, "/v1/tasks/"+evidence[0].Task.TaskID,
		nil, crossB.Token, senderHeaders[1])
	if err != nil {
		return err
	}
	if foreignReadStatus != http.StatusNotFound || engineErrorCode(foreignReadPayload) != "RESOURCE_NOT_FOUND" {
		return fmt.Errorf("cross-account task read status=%d code=%s, want 404/RESOURCE_NOT_FOUND", foreignReadStatus, engineErrorCode(foreignReadPayload))
	}
	foreignRead := map[string]any{"status": foreignReadStatus, "code": engineErrorCode(foreignReadPayload)}

	foreignRequest := engineTaskRequest(crossA, engineTargetGroup(98))
	foreignRequest.AllowedAccountIds = []string{crossB.AccountID}
	foreignBody, err := json.Marshal(foreignRequest)
	if err != nil {
		return err
	}
	foreignHeaders := engineTaskHeaders(crossA)
	foreignHeaders["Idempotency-Key"] = "p014-engine-cross-create-" + crossA.AccountID
	foreignCreateStatus, foreignCreatePayload, err := h.do(ctx, http.MethodPost, "/v1/tasks", foreignBody, crossA.Token, foreignHeaders)
	if err != nil {
		return err
	}
	if foreignCreateStatus != http.StatusForbidden || engineErrorCode(foreignCreatePayload) != "ACTION_FORBIDDEN" {
		return fmt.Errorf("cross-account task create status=%d code=%s, want 403/ACTION_FORBIDDEN", foreignCreateStatus, engineErrorCode(foreignCreatePayload))
	}
	foreignCreate := map[string]any{"status": foreignCreateStatus, "code": engineErrorCode(foreignCreatePayload)}

	// DB proof: exact task ownership, duplicate-free counts, and no engine
	// execution/gateway/permit/result rows anywhere in the fresh fixture.
	rows, err := store.Pool.Query(ctx, `
		SELECT t.id,t.mode,t.state,t.principal_id,ta.account_id,a.runtime_id,a.self_id
		FROM core.task t
		JOIN core.task_account ta ON (ta.workspace_id,ta.task_id)=(t.workspace_id,t.id)
		JOIN core.account a ON (a.workspace_id,a.id)=(ta.workspace_id,ta.account_id)
		WHERE t.workspace_id=$1
		ORDER BY t.id`, engineWorkspaceID)
	if err != nil {
		return fmt.Errorf("read task mapping: %w", err)
	}
	defer rows.Close()
	mappings := make([]engineMappingEvidence, 0, len(accounts))
	for rows.Next() {
		var mapping engineMappingEvidence
		var principal, mode, state string
		if err := rows.Scan(&mapping.TaskID, &mode, &state, &principal, &mapping.AccountID, &mapping.RuntimeID, &mapping.SelfID); err != nil {
			return fmt.Errorf("scan task mapping: %w", err)
		}
		if principal != enginePrincipalID || mode != "dry_run" || state != "DRAFT" {
			return fmt.Errorf("task %s principal/mode/state=%s/%s/%s", mapping.TaskID, principal, mode, state)
		}
		mapping.Mode, mapping.State = mode, state
		mappings = append(mappings, mapping)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read task mapping: %w", err)
	}
	if len(mappings) != len(accounts) {
		return fmt.Errorf("task mapping rows=%d, want %d", len(mappings), len(accounts))
	}
	seenTasks := map[string]bool{}
	for _, mapping := range mappings {
		if seenTasks[mapping.TaskID] {
			return fmt.Errorf("task %s is mapped to more than one account row", mapping.TaskID)
		}
		seenTasks[mapping.TaskID] = true
		matched := false
		for index, account := range accounts {
			if mapping.AccountID == account.AccountID && mapping.RuntimeID == account.RuntimeID && mapping.SelfID == account.SelfID &&
				mapping.TaskID == evidence[index].Task.TaskID {
				matched = true
				count, err := engineScalar(ctx, store, `SELECT count(*) FROM core.task_account WHERE workspace_id=$1 AND task_id=$2`, engineWorkspaceID, mapping.TaskID)
				if err != nil {
					return fmt.Errorf("count task account rows: %w", err)
				}
				if count != 1 {
					return fmt.Errorf("task %s task_account rows=%d, want 1", mapping.TaskID, count)
				}
				evidence[index].Task.TaskAccountRows = count
				break
			}
		}
		if !matched {
			return fmt.Errorf("task %s maps to account %s/%s/%s outside the live binding", mapping.TaskID, mapping.AccountID, mapping.RuntimeID, mapping.SelfID)
		}
	}

	var taskRows, taskAccountRows, idempotencyRows, heartbeatRows, heartbeatIdempotencyRows, outboxRows, eventRows int
	var accountRows, credentialRows, goalSlotRows, goalSlotReservedRows int
	if err := store.Pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM core.task),
			(SELECT count(*) FROM core.task_account),
			(SELECT count(*) FROM core.idempotency),
			(SELECT count(*) FROM core.runtime_heartbeat),
			(SELECT count(*) FROM core.heartbeat_idempotency),
			(SELECT count(*) FROM core.outbox),
			(SELECT count(*) FROM core.task_event),
			(SELECT count(*) FROM core.account),
			(SELECT count(*) FROM core.runtime_credentials),
			(SELECT count(*) FROM core.goal_slot),
			(SELECT count(*) FROM core.goal_slot WHERE state IN ('reserved','fulfilled'))`).Scan(
		&taskRows, &taskAccountRows, &idempotencyRows, &heartbeatRows, &heartbeatIdempotencyRows, &outboxRows, &eventRows,
		&accountRows, &credentialRows, &goalSlotRows, &goalSlotReservedRows); err != nil {
		return fmt.Errorf("read fixture counts: %w", err)
	}
	if taskRows != 4 || taskAccountRows != 4 || idempotencyRows != 4 || heartbeatRows != 4 || heartbeatIdempotencyRows != 4 ||
		accountRows != 4 || credentialRows != 4 || goalSlotRows != 4 || goalSlotReservedRows != 0 {
		return fmt.Errorf("unexpected fixture rows task=%d task_account=%d idempotency=%d heartbeat=%d heartbeat_idempotency=%d account=%d credential=%d goal_slot=%d goal_slot_active=%d",
			taskRows, taskAccountRows, idempotencyRows, heartbeatRows, heartbeatIdempotencyRows, accountRows, credentialRows, goalSlotRows, goalSlotReservedRows)
	}

	var executionRows, gatewayRows, permitRows, resultRows, subtaskRows, targetClaimRows,
		accountUsageRows, actionLockRows, inboxRows, membershipRows, approvalRows, targetRows,
		taskTargetRows, baselineAccountRows, baselineRows, retryRows, conflictRows, bridgeOutboxRows int
	if err := store.Pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM core.execution),
			(SELECT count(*) FROM bridge.gateway_operation),
			(SELECT count(*) FROM core.dispatch_permit),
			(SELECT count(*) FROM core.result),
			(SELECT count(*) FROM core.subtask),
			(SELECT count(*) FROM core.target_claim),
			(SELECT count(*) FROM core.account_usage),
			(SELECT count(*) FROM core.account_action_lock),
			(SELECT count(*) FROM core.inbox),
			(SELECT count(*) FROM core.membership),
			(SELECT count(*) FROM core.approval),
			(SELECT count(*) FROM core.target),
			(SELECT count(*) FROM core.task_target),
			(SELECT count(*) FROM core.baseline_account),
			(SELECT count(*) FROM core.baseline),
			(SELECT count(*) FROM core.retry),
			(SELECT count(*) FROM core.observation_conflict),
			(SELECT count(*) FROM bridge.outbox)`).Scan(
		&executionRows, &gatewayRows, &permitRows, &resultRows, &subtaskRows, &targetClaimRows,
		&accountUsageRows, &actionLockRows, &inboxRows, &membershipRows, &approvalRows, &targetRows,
		&taskTargetRows, &baselineAccountRows, &baselineRows, &retryRows, &conflictRows, &bridgeOutboxRows); err != nil {
		return fmt.Errorf("read zero-row proof: %w", err)
	}
	if executionRows != 0 || gatewayRows != 0 || permitRows != 0 || resultRows != 0 ||
		subtaskRows != 0 || targetClaimRows != 0 || accountUsageRows != 0 || actionLockRows != 0 ||
		inboxRows != 0 || membershipRows != 0 || approvalRows != 0 || targetRows != 0 ||
		taskTargetRows != 0 || baselineAccountRows != 0 || baselineRows != 0 ||
		retryRows != 0 || conflictRows != 0 || bridgeOutboxRows != 0 {
		return fmt.Errorf("engine/bridge/result rows were created: execution=%d gateway=%d permit=%d result=%d subtask=%d claim=%d usage=%d lock=%d inbox=%d membership=%d approval=%d target=%d task_target=%d baseline_account=%d baseline=%d retry=%d conflict=%d bridge_outbox=%d",
			executionRows, gatewayRows, permitRows, resultRows, subtaskRows, targetClaimRows,
			accountUsageRows, actionLockRows, inboxRows, membershipRows, approvalRows, targetRows,
			taskTargetRows, baselineAccountRows, baselineRows, retryRows, conflictRows, bridgeOutboxRows)
	}

	proof["accounts"] = evidence
	proof["mappings"] = mappings
	proof["rejections"] = map[string]any{
		"cross_runtime_heartbeat":   crossRuntime,
		"cross_account_heartbeat":   crossAccount,
		"cross_account_task_read":   foreignRead,
		"cross_account_task_create": foreignCreate,
	}
	proof["duplicates"] = map[string]any{
		"task_create_replay_exact":   true,
		"heartbeat_replay_exact":     true,
		"task_rows":                  taskRows,
		"task_account_rows":          taskAccountRows,
		"idempotency_rows":           idempotencyRows,
		"heartbeat_rows":             heartbeatRows,
		"heartbeat_idempotency_rows": heartbeatIdempotencyRows,
		"outbox_rows":                outboxRows,
		"task_event_rows":            eventRows,
	}
	proof["db"] = map[string]any{
		"task_rows":                  taskRows,
		"task_account_rows":          taskAccountRows,
		"idempotency_rows":           idempotencyRows,
		"heartbeat_rows":             heartbeatRows,
		"heartbeat_idempotency_rows": heartbeatIdempotencyRows,
		"outbox_rows":                outboxRows,
		"task_event_rows":            eventRows,
		"account_rows":               accountRows,
		"credential_rows":            credentialRows,
		"goal_slot_rows":             goalSlotRows,
		"goal_slot_active_rows":      goalSlotReservedRows,
		"execution_rows":             executionRows,
		"gateway_operation_rows":     gatewayRows,
		"dispatch_permit_rows":       permitRows,
		"result_rows":                resultRows,
		"subtask_rows":               subtaskRows,
		"target_claim_rows":          targetClaimRows,
		"account_usage_rows":         accountUsageRows,
		"account_action_lock_rows":   actionLockRows,
		"inbox_rows":                 inboxRows,
		"membership_rows":            membershipRows,
		"approval_rows":              approvalRows,
		"target_rows":                targetRows,
		"task_target_rows":           taskTargetRows,
		"baseline_account_rows":      baselineAccountRows,
		"baseline_rows":              baselineRows,
		"retry_rows":                 retryRows,
		"observation_conflict_rows":  conflictRows,
		"bridge_outbox_rows":         bridgeOutboxRows,
	}
	proof["writes"] = map[string]any{
		"qq":                0,
		"dispatch":          0,
		"engine_executions": 0,
		"llm":               0,
	}
	return nil
}
