// Command p0-13-join performs the owner-authorized single-account controlled
// group join. It reads the baseline group list immediately before writing,
// uses only the D1 cleaner route, records every step with UTC source times,
// and never retries an ambiguous write. Membership is confirmed only from a
// complete group-list observation, never from the join response itself.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
	"github.com/oapi-codegen/nullable"
)

const (
	maxQQCalls       = 10
	maxResponseBytes = 1 << 20
	httpTimeout      = 20 * time.Second
	webuiPort        = 6211
	onebotPort       = 3211
	accountSelfID    = "36374700"
	applyText        = "我是ai机器人"
	applyAnswer      = "我是小肥猪"
)

type callRecord struct {
	Sequence       int             `json:"sequence"`
	Step           string          `json:"step"`
	Kind           string          `json:"kind"`
	Attempt        int             `json:"attempt"`
	UTCStart       string          `json:"utc_start"`
	UTCEnd         string          `json:"utc_end"`
	Method         string          `json:"method"`
	URL            string          `json:"url"`
	RequestBody    json.RawMessage `json:"request_body,omitempty"`
	ResponseStatus int             `json:"response_status,omitempty"`
	ResponseBody   json.RawMessage `json:"response_body,omitempty"`
	ResponseText   string          `json:"response_text,omitempty"`
	Error          string          `json:"error,omitempty"`
	Retry          bool            `json:"retry,omitempty"`
	CountsBudget   bool            `json:"counts_budget"`
}

type stepRecord struct {
	UTC    string         `json:"utc"`
	Step   string         `json:"step"`
	Detail map[string]any `json:"detail,omitempty"`
}

type evidence struct {
	dir     string
	records []callRecord
	steps   []stepRecord
}

func (e *evidence) addCall(r callRecord) error {
	e.records = append(e.records, r)
	return e.flushCalls()
}

func (e *evidence) flushCalls() error {
	file, err := os.CreateTemp(e.dir, "requests-*.jsonl")
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, record := range e.records {
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
	return os.Rename(file.Name(), filepath.Join(e.dir, "requests.jsonl"))
}

func (e *evidence) addStep(step string, detail map[string]any) error {
	e.steps = append(e.steps, stepRecord{UTC: time.Now().UTC().Format(time.RFC3339Nano), Step: step, Detail: detail})
	file, err := os.CreateTemp(e.dir, "steps-*.jsonl")
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, record := range e.steps {
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
	return os.Rename(file.Name(), filepath.Join(e.dir, "steps.jsonl"))
}

type napcatState struct {
	Label      string
	WebUIPort  int
	OneBotPort int
	Token      string
	WebUIToken string
}

func loadState(path string) (napcatState, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return napcatState{}, errors.New("cannot read napcatd state")
	}
	var state struct {
		Instances []struct {
			Label      string `json:"label"`
			WebUIPort  int    `json:"webui_port"`
			OneBotPort int    `json:"onebot_port"`
			Token      string `json:"onebot_token"`
			WebUIToken string `json:"webui_token"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return napcatState{}, errors.New("invalid napcatd state")
	}
	matches := 0
	out := napcatState{Label: accountSelfID}
	for _, instance := range state.Instances {
		if instance.Label == accountSelfID && instance.WebUIPort == webuiPort && instance.OneBotPort == onebotPort {
			matches++
			out.Token = instance.Token
			out.WebUIToken = instance.WebUIToken
		}
	}
	if matches != 1 || strings.TrimSpace(out.Token) == "" || strings.TrimSpace(out.WebUIToken) == "" {
		return napcatState{}, errors.New("approved 6211/3211 account binding missing or ambiguous")
	}
	return out, nil
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
			dialer := net.Dialer{Timeout: 5 * time.Second}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
	return &http.Client{Timeout: httpTimeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type runner struct {
	ctx         context.Context
	client      *http.Client
	onebot      string
	token       string
	webuiToken  string
	webui       string
	targetGroup string
	evidence    *evidence
	qqCalls     int
	retries     int
}

func (r *runner) step(name string, detail map[string]any) error {
	return r.evidence.addStep(name, detail)
}

func (r *runner) do(step, kind, method, target string, payload any, bearer string, budget, allowRetry bool) (json.RawMessage, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	attempt := 0
	for {
		attempt++
		if budget {
			if r.qqCalls >= maxQQCalls {
				return nil, errors.New("QQ call budget exhausted")
			}
			r.qqCalls++
		}
		record := callRecord{Sequence: r.qqCalls, Step: step, Kind: kind, Attempt: attempt, Method: method, URL: target,
			RequestBody: body, CountsBudget: budget, Retry: attempt > 1}
		// Authentication material is used in memory only, including malformed
		// or error responses that might echo the login hash or credential.
		if step == "webui_auth" || step == "cleaner_join" {
			record.RequestBody = nil
		}
		record.UTCStart = time.Now().UTC().Format(time.RFC3339Nano)
		request, err := http.NewRequestWithContext(r.ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if payload != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		request.Header.Set("Accept", "application/json")
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := r.client.Do(request)
		if err != nil {
			record.Error = "transport failed"
			record.UTCEnd = time.Now().UTC().Format(time.RFC3339Nano)
			if err := r.evidence.addCall(record); err != nil {
				return nil, err
			}
			if allowRetry && attempt == 1 && r.retries < 1 {
				r.retries++
				continue
			}
			return nil, errors.New("transport failed")
		}
		limited := io.LimitReader(response.Body, maxResponseBytes+1)
		responseBody, readErr := io.ReadAll(limited)
		response.Body.Close()
		record.ResponseStatus = response.StatusCode
		if len(responseBody) > maxResponseBytes {
			responseBody = responseBody[:maxResponseBytes]
			record.Error = "response truncated"
		}
		if step != "webui_auth" && step != "cleaner_join" {
			if json.Valid(responseBody) {
				record.ResponseBody = append(json.RawMessage(nil), responseBody...)
			} else {
				record.ResponseText = string(responseBody)
			}
		}
		record.UTCEnd = time.Now().UTC().Format(time.RFC3339Nano)
		if err := r.evidence.addCall(record); err != nil {
			return nil, err
		}
		if readErr != nil {
			return nil, readErr
		}
		if response.StatusCode >= 500 && allowRetry && attempt == 1 && r.retries < 1 {
			r.retries++
			continue
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s returned HTTP %d", step, response.StatusCode)
		}
		return responseBody, nil
	}
}

func decodeEnvelope(raw json.RawMessage) (json.RawMessage, error) {
	var envelope struct {
		Status  string      `json:"status"`
		RetCode json.Number `json:"retcode"`
		Message string      `json:"message"`
		Data    json.RawMessage
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, err
	}
	if envelope.Status != "ok" || envelope.RetCode.String() != "0" {
		return nil, fmt.Errorf("onebot rejected: status=%s retcode=%s message=%s", envelope.Status, envelope.RetCode.String(), envelope.Message)
	}
	return envelope.Data, nil
}
func parseStatus(raw json.RawMessage) (map[string]any, error) {
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, err
	}
	return status, nil
}
func validateStatus(status map[string]any) error {
	online, ok := status["online"].(bool)
	if !ok || !online {
		return errors.New("status.online must be boolean true")
	}
	good, ok := status["good"].(bool)
	if !ok || !good {
		return errors.New("status.good must be boolean true")
	}
	return nil
}

func (r *runner) oneBot(step, action string, params any) (json.RawMessage, error) {
	raw, err := r.do(step, "qq", http.MethodPost, r.onebot+"/"+action, params, r.token, true, true)
	if err != nil {
		return nil, err
	}
	return decodeEnvelope(raw)
}

func (r *runner) webuiCredential() (string, error) {
	sum := sha256.Sum256([]byte(r.webuiToken + ".napcat"))
	login := map[string]any{"hash": hex.EncodeToString(sum[:])}
	raw, err := r.do("webui_auth", "webui", http.MethodPost, r.webui+"/api/auth/login", login, "", false, false)
	if err != nil {
		return "", err
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Credential string `json:"Credential"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Code != 0 || envelope.Data.Credential == "" {
		return "", errors.New("webui authentication failed")
	}
	return envelope.Data.Credential, nil
}

type cleanerAdapter struct {
	runner  *runner
	cred    string
	groupID string
	reason  string
	answer  string
}

// pluginAvailable requires authenticated runtime evidence that the approved
// cleaner plugin is active. A missing join route is never capability proof.
func (r *runner) pluginAvailable(credential string) error {
	raw, err := r.do("cleaner_preflight", "webui", http.MethodGet,
		r.webui+"/api/Plugin/List", nil, credential, false, false)
	if err != nil {
		return err
	}
	var response struct {
		Code *int `json:"code"`
		Data struct {
			ManagerMissing *bool `json:"pluginManagerNotFound"`
			Plugins        []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"plugins"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || response.Code == nil || *response.Code != 0 ||
		response.Data.ManagerMissing == nil || *response.Data.ManagerMissing {
		return errors.New("cleaner capability evidence unavailable")
	}
	matches := 0
	for _, plugin := range response.Data.Plugins {
		if plugin.ID == "napcat-plugin-cleaner" {
			if plugin.Status != "active" {
				return errors.New("cleaner plugin is not active")
			}
			matches++
		}
	}
	if matches != 1 {
		return errors.New("cleaner capability evidence missing or ambiguous")
	}
	return nil
}

func (a *cleanerAdapter) RequestJoin(ctx context.Context, reservation taskengine.Reservation) (taskengine.SendOutcome, error) {
	// Bind each final field to the engine's approved reservation. Comparing
	// concatenated text would permit moving bytes between reason and answer.
	if reservation.GroupID == "" || reservation.GroupID != a.groupID ||
		reservation.JoinReason != a.reason || reservation.VerificationMessage != a.answer {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "no_effect"}, errors.New("cleaner approval payload mismatch")
	}
	body := map[string]any{"group_id": json.Number(reservation.GroupID), "confirm": "1", "reason": reservation.JoinReason, "answer": reservation.VerificationMessage}
	raw, err := a.runner.do("cleaner_join", "plugin_write", http.MethodPost,
		a.runner.webui+"/api/Plugin/ext/napcat-plugin-cleaner/join-group", body, a.cred, true, false)
	if err != nil {
		// The transport outcome is unknown after dispatch. Never retry here.
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, err
	}
	var response struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Code   string          `json:"code"`
		Error  string          `json:"error"`
		Raw    json.RawMessage `json:"raw"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, err
	}
	if !response.OK {
		// The cleaner's rejection/no-effect classification is unverified.
		// Record it as unknown rather than inventing a retry.
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, fmt.Errorf("cleaner error code=%s error=%s", response.Code, response.Error)
	}
	if string(response.Result) != "1" {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, errors.New("cleaner result is not verified submitted code 1")
	}
	requestRef := ""
	if len(response.Raw) > 0 {
		var rawObject map[string]any
		if json.Unmarshal(response.Raw, &rawObject) == nil {
			for _, key := range []string{"request_id", "request_ref", "req_id"} {
				if value, ok := rawObject[key].(string); ok && strings.TrimSpace(value) != "" {
					requestRef = value
					break
				}
			}
		}
	}
	return taskengine.SendOutcome{
		Disposition: "submitted", EffectCertainty: "confirmed_submitted",
		EvidenceRef: "p013_cleaner_join_group_response", RequestRef: requestRef,
	}, nil
}

func newCode(prefix string) string {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(buffer[:])
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "p0-13 join:", err)
		os.Exit(1)
	}
}

func run() error {
	evidenceDir := os.Getenv("P013_EVIDENCE_DIR")
	if evidenceDir == "" {
		return errors.New("P013_EVIDENCE_DIR is required")
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return err
	}
	dsn := os.Getenv("BBA_FIXTURE_DSN")
	if dsn == "" {
		return errors.New("BBA_FIXTURE_DSN is required")
	}
	state, err := loadState(os.Getenv("P013_STATE_FILE"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store, err := taskengine.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	targetGroup := strings.TrimSpace(os.Getenv("P013_TARGET_GROUP"))
	if targetGroup == "" {
		return errors.New("P013_TARGET_GROUP is required")
	}
	parsedTarget, err := strconv.ParseUint(targetGroup, 10, 64)
	if err != nil || parsedTarget == 0 || targetGroup == accountSelfID {
		return errors.New("P013_TARGET_GROUP is invalid or equals account identity")
	}
	workspaceID := envOr("P013_WORKSPACE_ID", "ws_p013")
	runtimeID := envOr("P013_RUNTIME_ID", "runtime_36374700")
	accountID := envOr("P013_ACCOUNT_ID", "acc_36374700")
	actor := taskengine.Actor{WorkspaceID: workspaceID, PrincipalID: "principal_p013", Permissions: map[string]bool{
		"task.create": true, "task.read": true, "task.control": true, "task.manage_all": true, "task.approve": true,
	}}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES($1) ON CONFLICT DO NOTHING`, workspaceID); err != nil {
		return err
	}
	if _, err = store.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id) VALUES($1,'principal_p013') ON CONFLICT DO NOTHING`, workspaceID); err != nil {
		return err
	}
	_, err = store.Pool.Exec(ctx, `INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit,heartbeat_at)
		VALUES($1,$2,$3,'napcat-plugin-cleaner',$4,'verified','online','logged_in','active','clear',true,true,1,clock_timestamp())`,
		workspaceID, accountID, runtimeID, accountSelfID)
	if err != nil {
		return err
	}
	_, err = store.Pool.Exec(ctx, `INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority)
		VALUES($1,$2,$3,1,true) ON CONFLICT (workspace_id,runtime_id,account_id) DO NOTHING`, workspaceID, runtimeID, accountID)
	if err != nil {
		return err
	}
	rt := taskengine.RuntimeActor{WorkspaceID: workspaceID, RuntimeID: runtimeID, OwnerEpoch: 1, AccountIDs: []string{accountID}}

	ev := &evidence{dir: evidenceDir}
	r := &runner{ctx: ctx, client: loopbackClient(), onebot: "http://127.0.0.1:" + strconv.Itoa(onebotPort),
		token: state.Token, webuiToken: state.WebUIToken, webui: "http://127.0.0.1:" + strconv.Itoa(webuiPort), targetGroup: targetGroup, evidence: ev}
	summary := map[string]any{
		"utc_started":  time.Now().UTC().Format(time.RFC3339Nano),
		"target_group": targetGroup, "account_id": accountID,
		"onebot": r.onebot, "plugin_route": r.webui + "/api/Plugin/ext/napcat-plugin-cleaner/join-group",
	}
	finish := func(outcome string, cause error) error {
		summary["outcome"] = outcome
		summary["qq_calls"] = r.qqCalls
		summary["remaining_budget"] = maxQQCalls - r.qqCalls
		summary["retries"] = r.retries
		summary["utc_finished"] = time.Now().UTC().Format(time.RFC3339Nano)
		if cause != nil {
			summary["error"] = cause.Error()
		}
		_ = ev.addStep("finish", map[string]any{"outcome": outcome, "qq_calls": r.qqCalls, "retries": r.retries})
		_ = ev.flushCalls()
		_ = writeJSON(filepath.Join(evidenceDir, "summary.json"), summary)
		return cause
	}
	if err := ev.addStep("start", map[string]any{"target_group": targetGroup}); err != nil {
		return err
	}

	loginData, err := r.oneBot("get_login_info", "get_login_info", map[string]any{})
	if err != nil {
		return finish("STOP_READ_FAILED", err)
	}
	var login struct {
		UserID   json.Number `json:"user_id"`
		Nickname string      `json:"nickname"`
	}
	decoder := json.NewDecoder(bytes.NewReader(loginData))
	decoder.UseNumber()
	if err := decoder.Decode(&login); err != nil {
		return finish("STOP_READ_FAILED", err)
	}
	selfID := login.UserID.String()
	summary["observed_self_id"] = selfID
	summary["nickname"] = login.Nickname
	if selfID != accountSelfID {
		return finish("STOP_IDENTITY_MISMATCH", fmt.Errorf("self_id %q != account %q", selfID, accountSelfID))
	}
	if err := ev.addStep("identity", map[string]any{"self_id": selfID, "matched": true}); err != nil {
		return err
	}

	statusData, err := r.oneBot("get_status", "get_status", map[string]any{})
	if err != nil {
		return finish("STOP_READ_FAILED", err)
	}
	status, err := parseStatus(statusData)
	if err != nil {
		return finish("STOP_READ_FAILED", err)
	}
	if err := validateStatus(status); err != nil {
		return finish("STOP_STATUS_INVALID", err)
	}
	summary["status"] = status

	// Baseline is read immediately before the write and is the only source of
	// pre-existing memberships.
	baselineRaw, err := r.oneBot("baseline_group_list", "get_group_list", map[string]any{})
	if err != nil {
		return finish("STOP_BASELINE_FAILED", err)
	}
	baselineIDs, err := groupIDs(baselineRaw)
	if err != nil {
		return finish("STOP_BASELINE_FAILED", err)
	}
	alreadyMember := false
	for _, id := range baselineIDs {
		if id == targetGroup {
			alreadyMember = true
			break
		}
	}
	summary["baseline_group_count"] = len(baselineIDs)
	summary["target_in_baseline"] = alreadyMember
	if err := ev.addStep("baseline", map[string]any{"group_count": len(baselineIDs), "target_present": alreadyMember}); err != nil {
		return err
	}
	if alreadyMember {
		return finish("STOP_BASELINE_ALREADY_MEMBER", nil)
	}

	credential, err := r.webuiCredential()
	if err != nil {
		return finish("STOP_WEBUI_AUTH_FAILED", err)
	}
	if err := r.pluginAvailable(credential); err != nil {
		return finish("STOP_CLEANER_UNAVAILABLE", err)
	}
	if err := ev.addStep("cleaner_preflight", map[string]any{"available": true}); err != nil {
		return err
	}

	// Engine-side task: fixed target only, complete baseline, one attempt.
	approvalID := newCode("appr_p013_")
	if _, err := store.Pool.Exec(ctx, `INSERT INTO core.approval(workspace_id,id,principal_id,scope_revision,account_ids,target_ids,max_attempts,expires_at,join_reason,verification_message)
		VALUES($1,$2,$3,1,$4,$5,1,clock_timestamp()+interval '1 hour',$6,$7)`,
		workspaceID, approvalID, actor.PrincipalID, []string{accountID}, []string{targetGroup}, applyText, applyAnswer); err != nil {
		return finish("STOP_SETUP_FAILED", err)
	}
	mode := contract.ExecutionMode("live")
	fixed := []contract.FixedCandidate{{GroupId: targetGroup, SourceRef: "p013_owner_authorized"}}
	request := contract.CreateTaskRequest{
		Type: "group.acquire", Title: "P0-13 controlled group join", JoinReason: applyText, VerificationMessage: applyAnswer, Goal: contract.Goal{
			Metric: "new_unique_groups_for_account_pool", Count: 1, StrictTargetCap: true, RequireFinalMembershipCheck: true,
		},
		AllowedAccountIds: []string{accountID}, CandidateSource: contract.CandidateSource{Kind: "fixed", Groups: fixed},
		ExecutionMode: &mode, DeadlineAt: time.Now().UTC().Add(30 * time.Minute),
		Limits:      contract.TaskLimits{MaxCandidates: 1, MaxAttemptsPerAction: 1, MaxTotalExternalAttempts: 1, MaxModelAttempts: 0},
		ApprovalRef: nullable.NewNullableWithValue(approvalID), Submit: true,
	}
	task, err := store.CreateTask(ctx, actor, newCode("p013_create_"), request)
	if err != nil {
		return finish("STOP_SETUP_FAILED", err)
	}
	for {
		done, err := store.ExpandFixedPage(ctx, actor, task.Id, 100)
		if err != nil {
			return finish("STOP_SETUP_FAILED", err)
		}
		if done {
			break
		}
	}
	if err := store.CompleteBaseline(ctx, actor, task.Id, []taskengine.BaselineSnapshot{{
		AccountID: accountID, EvidenceRef: "p013_baseline_group_list", ObservedAt: time.Now().UTC(), Complete: true, GroupIDs: baselineIDs,
	}}); err != nil {
		return finish("STOP_SETUP_FAILED", err)
	}
	task, err = store.GetTask(ctx, actor, task.Id)
	if err != nil {
		return finish("STOP_SETUP_FAILED", err)
	}
	if _, err := store.ControlTask(ctx, actor, task.Id, newCode("p013_start_"), contract.TaskControlRequest{
		Action: "start", ExpectedVersion: task.Version, Reason: "owner-authorized P0-13",
	}); err != nil {
		return finish("STOP_SETUP_FAILED", err)
	}
	task, err = store.GetTask(ctx, actor, task.Id)
	if err != nil {
		return finish("STOP_SETUP_FAILED", err)
	}
	summary["task_id"] = task.Id
	summary["baseline_complete"] = true

	reservation, err := store.Reserve(ctx, workspaceID, task.Id, accountID)
	if err != nil {
		return finish("STOP_RESERVE_FAILED", err)
	}
	summary["operation_id"] = reservation.OperationID
	summary["target_id"] = reservation.TargetID
	if _, err := store.AcceptOperation(ctx, rt, reservation); err != nil {
		return finish("STOP_ACCEPT_FAILED", err)
	}
	if _, err := store.IssuePermit(ctx, rt, reservation.OperationID, 30*time.Second); err != nil {
		return finish("STOP_PERMIT_FAILED", err)
	}
	if err := ev.addStep("permit", map[string]any{"operation_id": reservation.OperationID, "target_id": reservation.TargetID}); err != nil {
		return err
	}
	adapter := &cleanerAdapter{runner: r, cred: credential, groupID: targetGroup, reason: applyText, answer: applyAnswer}
	outcome, dispatchErr := store.Dispatch(ctx, rt, reservation.OperationID, func(context.Context, taskengine.Reservation) error {
		raw, err := r.oneBot("pre_write_group_list", "get_group_list", map[string]any{})
		if err != nil {
			return err
		}
		ids, err := groupIDs(raw)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if id == targetGroup {
				return errors.New("target became a member before write")
			}
		}
		return r.step("pre_write_baseline", map[string]any{"group_count": len(ids), "target_present": false})
	}, adapter)
	summary["dispatch_outcome"] = outcome
	if dispatchErr != nil {
		summary["dispatch_error"] = dispatchErr.Error()
	}
	if err := ev.addStep("dispatch", map[string]any{"outcome": outcome, "error": errorString(dispatchErr)}); err != nil {
		return err
	}
	if dispatchErr != nil {
		return finish("STOP_DISPATCH_UNKNOWN", dispatchErr)
	}

	// Reduce the persisted adapter receipt through the inbox. This is a local
	// persistence step; it does not call QQ.
	if n, err := store.PumpBridgeOutbox(ctx, rt); err != nil {
		return finish("STOP_REDUCE_FAILED", err)
	} else {
		summary["bridge_events_pumped"] = n
	}
	var eventID string
	if err := store.Pool.QueryRow(ctx, `SELECT event_id FROM core.inbox WHERE workspace_id=$1 AND operation_id=$2 AND apply_state='pending'`, workspaceID, reservation.OperationID).Scan(&eventID); err == nil {
		if err := store.ApplyObservation(ctx, workspaceID, runtimeID, eventID); err != nil {
			return finish("STOP_REDUCE_FAILED", err)
		}
		summary["bridge_event_applied"] = eventID
	}
	if err := ev.addStep("reduce", map[string]any{"execution": executionRecord(ctx, store, reservation)}); err != nil {
		return err
	}

	// Observation chain: a complete group list is the only membership proof.
	observed, err := observeMembership(ctx, r, accountID, reservation)
	if err != nil {
		return finish("STOP_OBSERVATION_FAILED", err)
	}
	summary["membership_observed"] = observed
	if observed {
		now := time.Now().UTC()
		observation := taskengine.Observation{
			EventID: newCode("evt_p013_joined_"), OperationID: reservation.OperationID,
			AccountID: accountID, TargetID: reservation.TargetID, Kind: "membership.observed",
			State: "joined", EvidenceRef: "p013_post_join_group_list", SourceEpoch: 1, SourceSequence: 1,
			ObservedAt: now, SnapshotComplete: true,
		}
		if err := store.ReceiveObservation(ctx, rt, observation); err != nil {
			return finish("STOP_OBSERVATION_FAILED", err)
		}
		if err := store.ApplyObservation(ctx, workspaceID, runtimeID, observation.EventID); err != nil {
			return finish("STOP_OBSERVATION_FAILED", err)
		}
		summary["membership_event"] = observation.EventID
	}
	summary["execution"] = executionRecord(ctx, store, reservation)
	summary["result_valid"] = scalar(ctx, store, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND valid`, workspaceID, task.Id, reservation.TargetID)
	summary["task_state"] = stringScalar(ctx, store, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, workspaceID, task.Id)
	if err := writeDBEvidence(ctx, store, evidenceDir, workspaceID, task.Id, reservation); err != nil {
		return finish("STOP_EVIDENCE_FAILED", err)
	}
	if observed {
		return finish("CONFIRMED", nil)
	}
	return finish("SUBMITTED_PENDING_MEMBERSHIP", nil)
}

func groupIDs(raw json.RawMessage) ([]string, error) {
	// Only the complete OneBot get_group_list shape is accepted. Silently
	// skipping an unfamiliar record could hide an existing target membership.
	var groups []struct {
		ID        json.RawMessage `json:"group_id"`
		Name      *string         `json:"group_name"`
		Members   *uint64         `json:"member_count"`
		Maximum   *uint64         `json:"max_member_count"`
		Alternate json.RawMessage `json:"groupCode"`
	}
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, err
	}
	if groups == nil {
		return nil, errors.New("group list must be a complete JSON array")
	}
	ids := make([]string, 0, len(groups))
	for index, group := range groups {
		id := string(group.ID)
		if len(id) == 0 || id[0] < '1' || id[0] > '9' || len(id) > 20 || group.Name == nil ||
			group.Members == nil || group.Maximum == nil || len(group.Alternate) != 0 {
			return nil, fmt.Errorf("incomplete or mixed group record at index %d", index)
		}
		for _, digit := range id {
			if digit < '0' || digit > '9' {
				return nil, fmt.Errorf("invalid group identifier at index %d", index)
			}
		}
		if _, err := strconv.ParseUint(id, 10, 64); err != nil {
			return nil, fmt.Errorf("invalid group identifier at index %d", index)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func observeMembership(ctx context.Context, r *runner, accountID string, reservation taskengine.Reservation) (bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := r.oneBot("post_join_group_list", "get_group_list", map[string]any{})
		if err != nil {
			return false, err
		}
		ids, err := groupIDs(raw)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			if id == r.targetGroup {
				return true, r.step("membership_observed", map[string]any{"group_count": len(ids), "present": true, "attempt": attempt + 1})
			}
		}
		if err := r.step("membership_not_present", map[string]any{"group_count": len(ids), "attempt": attempt + 1}); err != nil {
			return false, err
		}
		if attempt == 0 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return false, nil
}

func executionRecord(ctx context.Context, store *taskengine.Store, r taskengine.Reservation) map[string]any {
	out := map[string]any{}
	var state, disposition, effect, requestRef, observedAt string
	err := store.Pool.QueryRow(ctx, `SELECT state,COALESCE(disposition,''),effect_certainty,COALESCE(external_request_ref,''),COALESCE(observed_at::text,'')
		FROM core.execution WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.ExecutionID).
		Scan(&state, &disposition, &effect, &requestRef, &observedAt)
	if err == nil {
		out["state"], out["disposition"], out["effect_certainty"], out["external_request_ref"], out["observed_at"] = state, disposition, effect, requestRef, observedAt
	}
	return out
}

func scalar(ctx context.Context, store *taskengine.Store, sql string, args ...any) int {
	var value int
	_ = store.Pool.QueryRow(ctx, sql, args...).Scan(&value)
	return value
}

func stringScalar(ctx context.Context, store *taskengine.Store, sql string, args ...any) string {
	var value string
	_ = store.Pool.QueryRow(ctx, sql, args...).Scan(&value)
	return value
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writeDBEvidence(ctx context.Context, store *taskengine.Store, dir, workspaceID, taskID string, r taskengine.Reservation) error {
	proof := map[string]any{"utc_read": time.Now().UTC().Format(time.RFC3339Nano)}
	rows, err := store.Pool.Query(ctx, `SELECT id,state,disposition,effect_certainty,external_request_ref,evidence_ref,started_at,observed_at
		FROM core.execution WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at`, workspaceID, taskID)
	if err == nil {
		var executions []map[string]any
		for rows.Next() {
			var id, state, effect string
			var disposition, requestRef, evidence *string
			var started, observed *time.Time
			if err := rows.Scan(&id, &state, &disposition, &effect, &requestRef, &evidence, &started, &observed); err != nil {
				break
			}
			executions = append(executions, map[string]any{"id": id, "state": state, "disposition": disposition, "effect_certainty": effect,
				"external_request_ref": requestRef, "evidence_ref": evidence, "started_at": started, "observed_at": observed})
		}
		rows.Close()
		proof["executions"] = executions
	}
	rows, err = store.Pool.Query(ctx, `SELECT kind,detail,created_at FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at`, workspaceID, taskID)
	if err == nil {
		var events []map[string]any
		for rows.Next() {
			var kind string
			var detail []byte
			var created time.Time
			if err := rows.Scan(&kind, &detail, &created); err != nil {
				break
			}
			events = append(events, map[string]any{"kind": kind, "detail": json.RawMessage(detail), "created_at": created.UTC()})
		}
		rows.Close()
		proof["task_events"] = events
	}
	proof["result"] = scalar(ctx, store, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND valid`, workspaceID, taskID, r.TargetID)
	proof["membership"] = stringScalar(ctx, store, `SELECT COALESCE(state,'') FROM core.membership WHERE workspace_id=$1 AND account_id=$2 AND target_id=$3`, workspaceID, r.AccountID, r.TargetID)
	proof["task"] = stringScalar(ctx, store, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, workspaceID, taskID)
	var schema int
	_ = store.Pool.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&schema)
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
