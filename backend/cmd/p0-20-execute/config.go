package main

// Configuration for the P0-20 bounded controlled execution entrypoint.
//
// The file intentionally contains identifiers and budgets only. It never
// contains OneBot/WebUI credentials. The raw file bytes are the exact owner
// authorization object: every writing subcommand must echo their SHA-256
// digest via -confirm-config (or BBA_EXEC_CONFIRM). No account, runtime,
// heartbeat or capability row is created or promoted by this command.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	configVersion         = 1
	maxConfigFileBytes    = 64 << 10
	maxApprovalWindow     = time.Hour
	maxExecutionWindow    = 6 * time.Hour
	maxEvidenceDirRunes   = 1024
	maxJoinCopyRunes      = 500
	maxChatTextFileBytes  = 4096
	maxChatTextRunes      = 2000
	maxTaskTitleRunes     = 200
	maxConfigurationIDLen = 128
)

// utcTime requires the same trailing-Z UTC form as the wire contract. Local
// offsets are rejected instead of silently normalized, so the digest the
// owner approves names exactly the instants the run will use.
type utcTime struct{ time.Time }

func (t *utcTime) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	if !strings.HasSuffix(text, "Z") {
		return errors.New("timestamp must be RFC3339 UTC with a trailing Z")
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return err
	}
	t.Time = parsed.UTC()
	return nil
}

func (t *utcTime) value() time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

type chatSendConfig struct {
	TriggerRef   string `json:"trigger_ref"`
	SendID       string `json:"send_id"`
	SegmentIndex int    `json:"segment_index"`
	TextFile     string `json:"text_file"`
}

type executionConfig struct {
	Version     int    `json:"version"`
	WorkspaceID string `json:"workspace_id"`
	PrincipalID string `json:"principal_id"`
	RuntimeID   string `json:"runtime_id"`
	AccountID   string `json:"account_id"`
	OwnerEpoch  int64  `json:"owner_epoch"`

	TargetGroupID string `json:"target_group_id"`

	TaskID     string `json:"task_id,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	TaskTitle  string `json:"task_title,omitempty"`

	ApprovalExpiresAt *utcTime `json:"approval_expires_at,omitempty"`
	TaskDeadlineAt    *utcTime `json:"task_deadline_at,omitempty"`

	GoalCount                int `json:"goal_count"`
	MaxCandidates            int `json:"max_candidates"`
	MaxAttemptsPerAction     int `json:"max_attempts_per_action"`
	MaxTotalExternalAttempts int `json:"max_total_external_attempts"`
	MaxQQReadCalls           int `json:"max_qq_read_calls"`

	TaskBridgeURL string          `json:"task_bridge_url"`
	InstanceLabel string          `json:"instance_label"`
	JoinReason    string          `json:"join_reason"`
	JoinAnswer    string          `json:"join_answer,omitempty"`
	EvidenceDir   string          `json:"evidence_dir"`
	Chat          *chatSendConfig `json:"chat,omitempty"`
}

type loadedConfig struct {
	cfg    executionConfig
	raw    []byte
	digest string
}

func loadExecutionConfig(path string) (loadedConfig, error) {
	var out loadedConfig
	info, err := os.Lstat(path)
	if err != nil {
		return out, fmt.Errorf("stat config file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return out, errors.New("config file must be a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return out, errors.New("config file must be mode 0600 or stricter")
	}
	if info.Size() > maxConfigFileBytes {
		return out, errors.New("config file exceeds the 64 KiB bound")
	}
	file, err := os.Open(path)
	if err != nil {
		return out, fmt.Errorf("open config file: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxConfigFileBytes+1))
	if err != nil {
		return out, fmt.Errorf("read config file: %w", err)
	}
	if len(raw) > maxConfigFileBytes {
		return out, errors.New("config file exceeds the 64 KiB bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out.cfg); err != nil {
		return out, fmt.Errorf("decode config file: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return out, errors.New("config file must contain exactly one JSON value")
	}
	if err := out.cfg.validateShape(); err != nil {
		return out, err
	}
	sum := sha256.Sum256(raw)
	out.raw = raw
	out.digest = hex.EncodeToString(sum[:])
	return out, nil
}

func (c executionConfig) validateShape() error {
	if c.Version != configVersion {
		return fmt.Errorf("config version must be %d", configVersion)
	}
	for name, value := range map[string]string{
		"workspace_id": c.WorkspaceID,
		"principal_id": c.PrincipalID,
		"runtime_id":   c.RuntimeID,
		"account_id":   c.AccountID,
	} {
		if !validConfigID(value) {
			return fmt.Errorf("%s must be 1..%d printable characters", name, maxConfigurationIDLen)
		}
	}
	if c.OwnerEpoch <= 0 {
		return errors.New("owner_epoch must be positive")
	}
	if !validConfigID(c.InstanceLabel) {
		return errors.New("instance_label must be 1..128 printable characters")
	}
	if c.EvidenceDir == "" || !filepathIsAbs(c.EvidenceDir) || utf8.RuneCountInString(c.EvidenceDir) > maxEvidenceDirRunes {
		return errors.New("evidence_dir must be an absolute bounded path")
	}
	if c.TaskBridgeURL != "" {
		if _, err := normalizeLoopbackURL(c.TaskBridgeURL); err != nil {
			return fmt.Errorf("task_bridge_url: %w", err)
		}
	}
	if c.Chat != nil {
		if !validConfigID(c.Chat.TriggerRef) || !validConfigID(c.Chat.SendID) {
			return errors.New("chat trigger_ref and send_id must be 1..128 printable characters")
		}
		if c.Chat.SegmentIndex < 0 || c.Chat.SegmentIndex > 31 {
			return errors.New("chat segment_index must be 0..31")
		}
		if c.Chat.TextFile == "" || !filepathIsAbs(c.Chat.TextFile) {
			return errors.New("chat text_file must be an absolute path")
		}
	}
	return nil
}

// validateTaskConfig checks the bounded task scope. Exactly one account and one
// target are intentional: this entrypoint is a single-writer composition, not
// an account-pool scheduler.
func (c executionConfig) validateTaskConfig(now time.Time) error {
	if c.TaskBridgeURL == "" {
		return errors.New("task_bridge_url is required for a task execution run")
	}
	if !validQQID(c.TargetGroupID) {
		return errors.New("target_group_id must be a positive unsigned decimal QQ group ID")
	}
	if c.GoalCount != 1 {
		return errors.New("goal_count must be exactly 1 for a single-writer controlled run")
	}
	if c.MaxCandidates != 1 {
		return errors.New("max_candidates must be exactly 1 for a single-writer controlled run")
	}
	if c.MaxAttemptsPerAction != 1 {
		return errors.New("max_attempts_per_action must be exactly 1 for a single-writer controlled run")
	}
	if c.MaxTotalExternalAttempts != 1 {
		return errors.New("max_total_external_attempts must be exactly 1 for a single-writer controlled run")
	}
	if c.MaxQQReadCalls < 7 || c.MaxQQReadCalls > 30 {
		return errors.New("max_qq_read_calls must be 7..30 for a task run (preflight, pre-write gate and post-write membership read)")
	}
	if strings.TrimSpace(c.JoinReason) == "" || utf8.RuneCountInString(c.JoinReason) > maxJoinCopyRunes {
		return errors.New("join_reason must be 1..500 Unicode characters")
	}
	if strings.TrimSpace(c.JoinAnswer) == "" || utf8.RuneCountInString(c.JoinAnswer) > maxJoinCopyRunes {
		return errors.New("join_answer must be 1..500 Unicode characters")
	}
	if c.TaskID != "" {
		if !validConfigID(c.TaskID) {
			return errors.New("task_id must be 1..128 printable characters")
		}
		if c.ApprovalID != "" && !validConfigID(c.ApprovalID) {
			return errors.New("approval_id must be 1..128 printable characters")
		}
		return nil
	}
	if strings.TrimSpace(c.TaskTitle) == "" || utf8.RuneCountInString(c.TaskTitle) > maxTaskTitleRunes {
		return errors.New("task_title must be 1..200 Unicode characters when creating a task")
	}
	deadline := c.TaskDeadlineAt.value()
	if deadline.IsZero() || !deadline.After(now) {
		return errors.New("task_deadline_at must be a future UTC timestamp when creating a task")
	}
	if deadline.After(now.Add(maxExecutionWindow)) {
		return errors.New("task_deadline_at exceeds the six-hour execution window")
	}
	if c.ApprovalID == "" {
		expires := c.ApprovalExpiresAt.value()
		if expires.IsZero() || !expires.After(now) {
			return errors.New("approval_expires_at must be a future UTC timestamp when creating an approval")
		}
		if expires.After(deadline) {
			return errors.New("approval_expires_at must not outlive task_deadline_at")
		}
		if expires.After(now.Add(maxApprovalWindow)) {
			return errors.New("approval_expires_at exceeds the one-hour approval window")
		}
	} else if !validConfigID(c.ApprovalID) {
		return errors.New("approval_id must be 1..128 printable characters")
	}
	return nil
}

func (c executionConfig) validateChatConfig() error {
	if c.Chat == nil {
		return errors.New("chat section is required for chat-send")
	}
	if c.TaskBridgeURL == "" {
		return errors.New("task_bridge_url is required for chat-send")
	}
	// A chat run never joins and never reads target/budget members as a task.
	return nil
}

func confirmConfigDigest(digest, expected string) error {
	expected = strings.TrimSpace(expected)
	if len(expected) != sha256.Size*2 || expected != strings.ToLower(expected) {
		return errors.New("-confirm-config must be the lowercase SHA-256 of the exact config file")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return errors.New("-confirm-config must be the lowercase SHA-256 of the exact config file")
	}
	if !constantTimeHexEqual(digest, expected) {
		return errors.New("config digest does not match -confirm-config; the approved configuration was not supplied")
	}
	return nil
}

func constantTimeHexEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func validConfigID(value string) bool {
	if strings.TrimSpace(value) != value || value == "" || utf8.RuneCountInString(value) > maxConfigurationIDLen {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func validQQID(value string) bool {
	if len(value) == 0 || len(value) > 20 || value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	if _, err := strconv.ParseUint(value, 10, 64); err != nil {
		return false
	}
	return true
}

// normalizeLoopbackURL accepts only a literal-loopback HTTP origin. No DNS
// name, userinfo, path, query or fragment is allowed; the caller cannot point
// an existing permit at another host.
func normalizeLoopbackURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid URL")
	}
	if parsed.Scheme != "http" || parsed.Opaque != "" || parsed.User != nil {
		return "", errors.New("must be an http URL without userinfo")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("must not contain a query or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("must not contain a path")
	}
	host := parsed.Hostname()
	if host == "" || parsed.Port() == "" {
		return "", errors.New("must include a literal loopback IP and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("must use a literal loopback IP")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("must include a valid port")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// filepathIsAbs avoids importing filepath in every caller while keeping the
// config validation self-contained.
func filepathIsAbs(path string) bool {
	return strings.HasPrefix(path, "/")
}

func readChatText(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("stat chat text file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("chat text file must be a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return "", errors.New("chat text file must be mode 0600 or stricter")
	}
	if info.Size() > maxChatTextFileBytes {
		return "", fmt.Errorf("chat text file exceeds %d bytes", maxChatTextFileBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read chat text file: %w", err)
	}
	if len(raw) == 0 || !utf8.Valid(raw) {
		return "", errors.New("chat text must be non-empty valid UTF-8")
	}
	if utf8.RuneCount(raw) > maxChatTextRunes {
		return "", fmt.Errorf("chat text exceeds %d Unicode characters", maxChatTextRunes)
	}
	return string(raw), nil
}
