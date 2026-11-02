// Command p0-14-four performs the read-only four-account health probe used to
// admit four real NapCat instances into the P0-14 evidence chain. It reads
// login identity, online/good status, and the complete group list once per
// account. It never sends a message, joins a group, refreshes a QR code,
// restarts an instance, or writes to the NapCat state file.
//
// The account binding is by OneBot port and expected self_id, never by the
// supervisor label: labels in the current deployment are known to be stale.
package main

import (
	"bytes"
	"context"
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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxResponseBytes = 4 << 20
	httpTimeout      = 20 * time.Second
	maxQQCalls       = 20
	mode             = "read_only"
)

type binding struct {
	Port   int    `json:"onebot_port"`
	SelfID string `json:"expected_self_id"`
}

type stateInstance struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	OneBotPort  int    `json:"onebot_port"`
	OneBotToken string `json:"onebot_token"`
	WebUIPort   int    `json:"webui_port"`
	WebUIToken  string `json:"webui_token"`
}

type selectedInstance struct {
	binding
	InstanceID string
	Token      string
}

type callRecord struct {
	Sequence       int             `json:"sequence"`
	Account        string          `json:"account"`
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

func (e *evidence) addCall(record callRecord) error {
	e.records = append(e.records, record)
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

func parseBindings(raw string) ([]binding, error) {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) != 4 {
		return nil, errors.New("P014_BINDINGS must contain exactly four port:self_id entries")
	}
	out := make([]binding, 0, 4)
	ports := map[int]bool{}
	ids := map[string]bool{}
	for _, part := range parts {
		pair := strings.Split(strings.TrimSpace(part), ":")
		if len(pair) != 2 {
			return nil, errors.New("each binding must be port:self_id")
		}
		port, err := strconv.Atoi(strings.TrimSpace(pair[0]))
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("binding port is invalid")
		}
		selfID := strings.TrimSpace(pair[1])
		if !validID(selfID) {
			return nil, errors.New("binding self_id is invalid")
		}
		if ports[port] || ids[selfID] {
			return nil, errors.New("binding ports and self_ids must be unique")
		}
		ports[port] = true
		ids[selfID] = true
		out = append(out, binding{Port: port, SelfID: selfID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

func validID(value string) bool {
	if len(value) == 0 || len(value) > 20 || value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func loadState(path string) ([]stateInstance, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read napcatd state")
	}
	var state struct {
		Instances []stateInstance `json:"instances"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, errors.New("invalid napcatd state")
	}
	return state.Instances, nil
}

func selectInstances(instances []stateInstance, bindings []binding) ([]selectedInstance, error) {
	out := make([]selectedInstance, 0, len(bindings))
	for _, want := range bindings {
		matches := 0
		var found stateInstance
		for _, instance := range instances {
			if instance.OneBotPort == want.Port {
				matches++
				found = instance
			}
		}
		if matches != 1 {
			return nil, fmt.Errorf("onebot port %d has %d state bindings", want.Port, matches)
		}
		token := strings.TrimSpace(found.OneBotToken)
		if token == "" {
			return nil, fmt.Errorf("onebot port %d has no token", want.Port)
		}
		out = append(out, selectedInstance{binding: want, InstanceID: found.ID, Token: token})
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
	ctx      context.Context
	client   *http.Client
	evidence *evidence
	calls    int
	retries  int
}

func (r *runner) do(account, step, method, target string, payload any, bearer string, allowRetry bool) (json.RawMessage, error) {
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
		if r.calls >= maxQQCalls {
			return nil, errors.New("QQ call budget exhausted")
		}
		r.calls++
		record := callRecord{
			Sequence: r.calls, Account: account, Step: step, Kind: "qq", Attempt: attempt,
			Method: method, URL: target, RequestBody: body, CountsBudget: true, Retry: attempt > 1,
		}
		record.UTCStart = time.Now().UTC().Format(time.RFC3339Nano)
		request, err := http.NewRequestWithContext(r.ctx, method, target, bytes.NewReader(body))
		if err != nil {
			record.Error = "invalid request"
			record.UTCEnd = time.Now().UTC().Format(time.RFC3339Nano)
			_ = r.evidence.addCall(record)
			return nil, errors.New("invalid request")
		}
		if payload != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		request.Header.Set("Accept", "application/json")
		// The bearer is used in memory only. It is never copied into a call
		// record, an error string, or a response file.
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := r.client.Do(request)
		if err != nil {
			record.Error = "transport failed"
			record.UTCEnd = time.Now().UTC().Format(time.RFC3339Nano)
			if addErr := r.evidence.addCall(record); addErr != nil {
				return nil, addErr
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
		if json.Valid(responseBody) {
			record.ResponseBody = append(json.RawMessage(nil), responseBody...)
		} else if len(responseBody) > 0 {
			record.ResponseText = string(responseBody)
		}
		record.UTCEnd = time.Now().UTC().Format(time.RFC3339Nano)
		if err := r.evidence.addCall(record); err != nil {
			return nil, err
		}
		if readErr != nil {
			return nil, errors.New("response read failed")
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
		Data    json.RawMessage
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, errors.New("invalid onebot envelope")
	}
	if envelope.Status != "ok" || envelope.RetCode.String() != "0" {
		return nil, fmt.Errorf("onebot rejected status=%s retcode=%s", envelope.Status, envelope.RetCode.String())
	}
	if len(envelope.Data) == 0 {
		return nil, errors.New("onebot envelope has no data")
	}
	return envelope.Data, nil
}

func (r *runner) oneBot(account string, port int, token, step, action string) (json.RawMessage, error) {
	switch action {
	case "get_login_info", "get_status", "get_group_list":
	default:
		return nil, errors.New("action is outside the read-only smoke allowlist")
	}
	target := fmt.Sprintf("http://127.0.0.1:%d/%s", port, action)
	raw, err := r.do(account, step, http.MethodPost, target, map[string]any{}, token, true)
	if err != nil {
		return nil, err
	}
	return decodeEnvelope(raw)
}

func parseLogin(raw json.RawMessage) (string, string, error) {
	var login struct {
		UserID   json.Number `json:"user_id"`
		Nickname string      `json:"nickname"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&login); err != nil {
		return "", "", errors.New("invalid login info")
	}
	selfID := login.UserID.String()
	if !validID(selfID) {
		return "", "", errors.New("login info has invalid user_id")
	}
	return selfID, login.Nickname, nil
}

func parseStatus(raw json.RawMessage) (bool, bool, error) {
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		return false, false, errors.New("invalid status")
	}
	online, onlineOK := status["online"].(bool)
	good, goodOK := status["good"].(bool)
	if !onlineOK || !online || !goodOK || !good {
		return online, good, errors.New("status.online and status.good must both be boolean true")
	}
	return online, good, nil
}

func groupList(raw json.RawMessage) (int, string, error) {
	var groups []struct {
		ID        json.RawMessage `json:"group_id"`
		Name      *string         `json:"group_name"`
		Members   *uint64         `json:"member_count"`
		Maximum   *uint64         `json:"max_member_count"`
		Alternate json.RawMessage `json:"groupCode"`
	}
	if err := json.Unmarshal(raw, &groups); err != nil {
		return 0, "", errors.New("invalid group list")
	}
	if groups == nil {
		return 0, "", errors.New("group list must be a complete JSON array")
	}
	ids := make([]string, 0, len(groups))
	seen := map[string]bool{}
	for index, group := range groups {
		id := string(group.ID)
		if !validID(id) || group.Name == nil || group.Members == nil || group.Maximum == nil || len(group.Alternate) != 0 {
			return 0, "", fmt.Errorf("incomplete or mixed group record at index %d", index)
		}
		if seen[id] {
			return 0, "", fmt.Errorf("duplicate group identifier at index %d", index)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return len(ids), hex.EncodeToString(sum[:]), nil
}

func uniqueIDs(results []accountResult) int {
	seen := map[string]bool{}
	for _, result := range results {
		if result.ObservedSelfID != "" {
			seen[result.ObservedSelfID] = true
		}
	}
	return len(seen)
}

type accountResult struct {
	OneBotPort     int    `json:"onebot_port"`
	InstanceID     string `json:"instance_id"`
	ExpectedSelfID string `json:"expected_self_id"`
	ObservedSelfID string `json:"observed_self_id"`
	Nickname       string `json:"nickname"`
	Online         bool   `json:"online"`
	Good           bool   `json:"good"`
	GroupCount     int    `json:"group_count"`
	GroupIDsSHA256 string `json:"group_ids_sha256"`
	QQCallStart    int    `json:"qq_call_start"`
	QQCallEnd      int    `json:"qq_call_end"`
	Retries        int    `json:"retries"`
	Error          string `json:"error,omitempty"`
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "p0-14 four:", err)
		os.Exit(1)
	}
}

func run() error {
	evidenceDir := os.Getenv("P014_EVIDENCE_DIR")
	if evidenceDir == "" {
		return errors.New("P014_EVIDENCE_DIR is required")
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return err
	}
	engineMode := os.Getenv("P014_ENGINE_SMOKE")
	if engineMode != "" && engineMode != "0" && engineMode != "1" {
		return errors.New("P014_ENGINE_SMOKE must be 0 or 1")
	}
	if engineMode == "1" {
		if err := validateEngineSmokeEnv(); err != nil {
			return fmt.Errorf("engine smoke preflight: %w", err)
		}
	}
	bindings, err := parseBindings(os.Getenv("P014_BINDINGS"))
	if err != nil {
		return err
	}
	instances, err := loadState(os.Getenv("P014_STATE_FILE"))
	if err != nil {
		return err
	}
	selected, err := selectInstances(instances, bindings)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ev := &evidence{dir: evidenceDir}
	r := &runner{ctx: ctx, client: loopbackClient(), evidence: ev}
	summary := map[string]any{
		"round": "R20", "mode": mode, "writes": 0,
		"bindings": bindings, "utc_started": time.Now().UTC().Format(time.RFC3339Nano),
		"code_sha": os.Getenv("P014_CODE_SHA"),
	}
	if engineMode == "1" {
		summary["round"] = "R22"
		summary["engine_smoke_requested"] = true
	}
	results := make([]accountResult, 0, len(selected))
	errs := []string{}
	finish := func() error {
		summary["accounts"] = results
		summary["unique_observed_self_ids"] = uniqueIDs(results)
		summary["qq_calls"] = r.calls
		summary["retries"] = r.retries
		summary["errors"] = errs
		summary["utc_finished"] = time.Now().UTC().Format(time.RFC3339Nano)
		outcome := "PASS"
		if len(errs) != 0 || len(results) != 4 || uniqueIDs(results) != 4 {
			outcome = "FAIL"
		}
		summary["outcome"] = outcome
		_ = ev.addStep("finish", map[string]any{"outcome": outcome, "qq_calls": r.calls, "retries": r.retries})
		_ = ev.flushCalls()
		if err := writeJSON(filepath.Join(evidenceDir, "summary.json"), summary); err != nil {
			return err
		}
		if outcome != "PASS" {
			return fmt.Errorf("four-account read-only probe failed: %s", strings.Join(errs, "; "))
		}
		return nil
	}
	if err := ev.addStep("start", map[string]any{"mode": mode, "bindings": bindings}); err != nil {
		return err
	}

	for _, account := range selected {
		result := accountResult{
			OneBotPort: account.Port, InstanceID: account.InstanceID,
			ExpectedSelfID: account.SelfID, QQCallStart: r.calls + 1,
		}
		label := fmt.Sprintf("%d/%s", account.Port, account.SelfID)
		retriesBefore := r.retries
		appendResult := func() {
			result.Retries = r.retries - retriesBefore
			results = append(results, result)
		}
		loginRaw, err := r.oneBot(label, account.Port, account.Token, "get_login_info", "get_login_info")
		if err != nil {
			result.Error = err.Error()
			result.QQCallEnd = r.calls
			errs = append(errs, fmt.Sprintf("port %d login: %v", account.Port, err))
			appendResult()
			_ = ev.addStep("account_error", map[string]any{"onebot_port": account.Port, "step": "get_login_info"})
			continue
		}
		selfID, nickname, err := parseLogin(loginRaw)
		if err != nil {
			result.Error = err.Error()
			result.QQCallEnd = r.calls
			errs = append(errs, fmt.Sprintf("port %d login: %v", account.Port, err))
			appendResult()
			continue
		}
		result.ObservedSelfID = selfID
		result.Nickname = nickname
		if selfID != account.SelfID {
			result.Error = fmt.Sprintf("self_id %s does not match binding %s", selfID, account.SelfID)
			result.QQCallEnd = r.calls
			errs = append(errs, fmt.Sprintf("port %d identity mismatch", account.Port))
			appendResult()
			_ = ev.addStep("account_identity", map[string]any{"onebot_port": account.Port, "self_id": selfID, "matched": false})
			continue
		}
		_ = ev.addStep("account_identity", map[string]any{"onebot_port": account.Port, "self_id": selfID, "matched": true})

		statusRaw, err := r.oneBot(label, account.Port, account.Token, "get_status", "get_status")
		if err != nil {
			result.Error = err.Error()
			result.QQCallEnd = r.calls
			errs = append(errs, fmt.Sprintf("port %d status: %v", account.Port, err))
			appendResult()
			continue
		}
		online, good, err := parseStatus(statusRaw)
		result.Online, result.Good = online, good
		if err != nil {
			result.Error = err.Error()
			result.QQCallEnd = r.calls
			errs = append(errs, fmt.Sprintf("port %d status: %v", account.Port, err))
			appendResult()
			continue
		}
		_ = ev.addStep("account_status", map[string]any{"onebot_port": account.Port, "online": true, "good": true})

		groupsRaw, err := r.oneBot(label, account.Port, account.Token, "get_group_list", "get_group_list")
		if err != nil {
			result.Error = err.Error()
			result.QQCallEnd = r.calls
			errs = append(errs, fmt.Sprintf("port %d groups: %v", account.Port, err))
			appendResult()
			continue
		}
		count, digest, err := groupList(groupsRaw)
		if err != nil {
			result.Error = err.Error()
			result.QQCallEnd = r.calls
			errs = append(errs, fmt.Sprintf("port %d groups: %v", account.Port, err))
			appendResult()
			continue
		}
		result.GroupCount = count
		result.GroupIDsSHA256 = digest
		result.QQCallEnd = r.calls
		appendResult()
		_ = ev.addStep("account_groups", map[string]any{
			"onebot_port": account.Port, "group_count": count, "group_ids_sha256": digest,
		})
	}

	if engineMode == "1" && len(errs) == 0 && len(results) == 4 && uniqueIDs(results) == 4 {
		proof, err := runEngineSmoke(ctx, evidenceDir, results)
		if err != nil {
			errs = append(errs, "engine smoke: "+err.Error())
		} else {
			summary["engine_smoke"] = proof
		}
	}

	return finish()
}

func writeJSON(path string, value any) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".summary-*.json")
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		file.Close()
		os.Remove(file.Name())
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return err
	}
	return os.Rename(file.Name(), path)
}
