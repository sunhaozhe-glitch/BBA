package main

// P0-20 controlled execution entrypoint.
//
// The command is deliberately separate from taskengine-server. The read-only
// HTTP server keeps its existing surface; this process is the explicitly
// configured single writer that composes the persisted Task Engine chain.
//
// Usage:
//   p0-20-execute -config /secure/run.json preflight
//   p0-20-execute -config /secure/run.json -confirm-config <sha256> execute
//   p0-20-execute -config /secure/run.json -confirm-config <sha256> recover
//   p0-20-execute -config /secure/chat.json -confirm-config <sha256> chat-send
//
// The worker requires only BBA_TASK_BRIDGE_TOKEN; QQ credentials remain in Task Bridge.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"bba.local/taskengine/internal/taskengine"
)

const requiredSchemaVersion = 14

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "p0-20-execute:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) (runErr error) {
	flags := flag.NewFlagSet("p0-20-execute", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "mode-0600 exact execution configuration JSON")
	confirmConfig := flags.String("confirm-config", "", "lowercase SHA-256 of the exact config file (or BBA_EXEC_CONFIRM)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: p0-20-execute -config <path> [-confirm-config <sha256>] preflight|execute|recover|chat-send")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("-config is required")
	}
	if flags.NArg() != 1 {
		return errors.New("exactly one mode is required: preflight | execute | recover | chat-send")
	}
	mode := flags.Arg(0)
	switch mode {
	case "preflight", "execute", "recover", "chat-send":
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	loaded, err := loadExecutionConfig(*configPath)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	switch mode {
	case "preflight", "execute":
		if err := loaded.cfg.validateTaskConfig(now); err != nil {
			return err
		}
	case "chat-send":
		if err := loaded.cfg.validateChatConfig(); err != nil {
			return err
		}
	case "recover":
		// Recovery addresses the persisted runtime/account only; it does not
		// need a target or an approval.
	}
	if mode == "recover" {
		if loaded.cfg.MaxQQReadCalls < 0 {
			return errors.New("max_qq_read_calls cannot be negative")
		}
	}
	if mode != "preflight" {
		confirmation := strings.TrimSpace(*confirmConfig)
		if confirmation == "" {
			confirmation = strings.TrimSpace(os.Getenv("BBA_EXEC_CONFIRM"))
		}
		if err := confirmConfigDigest(loaded.digest, confirmation); err != nil {
			return err
		}
	}

	dsn := os.Getenv("BBA_TASKENGINE_DSN")
	if dsn == "" {
		return errors.New("BBA_TASKENGINE_DSN is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	store, err := taskengine.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open Task Engine store: %w", err)
	}
	defer store.Close()
	version, err := schemaVersion(ctx, store)
	if err != nil {
		return err
	}
	if version != requiredSchemaVersion {
		return fmt.Errorf("Task Engine requires schema %d; found %d", requiredSchemaVersion, version)
	}
	ev, err := newEvidence(loaded.cfg.EvidenceDir, mode)
	if err != nil {
		return err
	}
	summary := map[string]any{
		"utc_started":     time.Now().UTC().Format(time.RFC3339Nano),
		"mode":            mode,
		"config_digest":   loaded.digest,
		"workspace_id":    loaded.cfg.WorkspaceID,
		"principal_id":    loaded.cfg.PrincipalID,
		"runtime_id":      loaded.cfg.RuntimeID,
		"account_id":      loaded.cfg.AccountID,
		"owner_epoch":     loaded.cfg.OwnerEpoch,
		"target_group_id": loaded.cfg.TargetGroupID,
		"task_id":         loaded.cfg.TaskID,
		"evidence_dir":    ev.dir,
	}
	defer func() {
		if err := ev.finish(summary); err != nil && runErr == nil {
			runErr = err
		}
	}()

	actor := taskengine.Actor{
		WorkspaceID: loaded.cfg.WorkspaceID,
		PrincipalID: loaded.cfg.PrincipalID,
		Permissions: map[string]bool{
			"task.create": true, "task.read": true, "task.control": true, "task.manage_all": true,
		},
	}
	rt := taskengine.RuntimeActor{
		WorkspaceID: loaded.cfg.WorkspaceID,
		RuntimeID:   loaded.cfg.RuntimeID,
		OwnerEpoch:  loaded.cfg.OwnerEpoch,
		AccountIDs:  []string{loaded.cfg.AccountID},
	}

	switch mode {
	case "preflight":
		live, err := newLiveClient(loaded.cfg, ev, true)
		if err != nil {
			return err
		}
		pre, err := runPreflight(ctx, store, loaded, live, true)
		if err != nil {
			return err
		}
		summary["preflight"] = preflightSummary(pre)
		summary["qq_read_calls"] = live.reads.count()
		summary["qq_write_calls"] = live.writes.count()
		if err := writeJSON(stdout, summary); err != nil {
			return err
		}
		if !pre.Ready {
			return fmt.Errorf("preflight not ready: %s", strings.Join(pre.Blockers, "; "))
		}
		return nil
	case "execute":
		live, err := newLiveClient(loaded.cfg, ev, true)
		if err != nil {
			return err
		}
		release, err := acquireWriterLock(ctx, store, writerLockKey(loaded.cfg))
		if err != nil {
			return err
		}
		defer release()
		recovery, err := runRecovery(ctx, store, rt, ev)
		if err != nil {
			return err
		}
		summary["recovery"] = recovery
		pre, err := runPreflight(ctx, store, loaded, live, true)
		if err != nil {
			return err
		}
		summary["preflight"] = preflightSummary(pre)
		if !pre.Ready {
			if err := writeJSON(stdout, summary); err != nil {
				return err
			}
			return fmt.Errorf("execution not started; preflight not ready: %s", strings.Join(pre.Blockers, "; "))
		}
		execSummary, execErr := executeTask(ctx, store, actor, rt, loaded, live, pre, ev)
		for key, value := range execSummary {
			summary[key] = value
		}
		summary["qq_read_calls"] = live.reads.count()
		summary["qq_write_calls"] = live.writes.count()
		if err := writeJSON(stdout, summary); err != nil && execErr == nil {
			execErr = err
		}
		return execErr
	case "recover":
		release, err := acquireWriterLock(ctx, store, writerLockKey(loaded.cfg))
		if err != nil {
			return err
		}
		defer release()
		recovery, err := runRecovery(ctx, store, rt, ev)
		if err != nil {
			return err
		}
		summary["recovery"] = recovery
		summary["result"] = "RECOVERY_ONLY"
		if err := writeJSON(stdout, summary); err != nil {
			return err
		}
		return nil
	case "chat-send":
		live, err := newLiveClient(loaded.cfg, ev, false)
		if err != nil {
			return err
		}
		release, err := acquireWriterLock(ctx, store, writerLockKey(loaded.cfg))
		if err != nil {
			return err
		}
		defer release()
		chatSummary, chatErr := runChatSend(ctx, store, rt, loaded, live, ev)
		for key, value := range chatSummary {
			summary[key] = value
		}
		summary["qq_read_calls"] = live.reads.count()
		summary["qq_write_calls"] = live.writes.count()
		if err := writeJSON(stdout, summary); err != nil && chatErr == nil {
			chatErr = err
		}
		return chatErr
	}
	return errors.New("unreachable mode")
}

func preflightSummary(pre preflightResult) map[string]any {
	out := map[string]any{
		"ready":         pre.Ready,
		"blocker_count": len(pre.Blockers),
		"account": map[string]any{
			"identity_status":       pre.Account.IdentityStatus,
			"connection_state":      pre.Account.ConnectionState,
			"login_state":           pre.Account.LoginState,
			"control_state":         pre.Account.ControlState,
			"risk_state":            pre.Account.RiskState,
			"request_join_verified": pre.Account.RequestJoinVerified,
			"membership_verified":   pre.Account.MembershipVerified,
			"membership_authority":  pre.Account.MembershipAuthority,
			"credential_active":     pre.Account.CredentialActive,
			"identity_generation":   pre.Account.IdentityGeneration,
			"quota_limit":           pre.Account.QuotaLimit,
			"heartbeat_at":          pre.Account.HeartbeatAt,
		},
		"observed": map[string]any{
			"online":         pre.Identity.Online,
			"good":           pre.Identity.Good,
			"group_count":    len(pre.Identity.GroupIDs),
			"target_present": containsGroup(pre.Identity.GroupIDs, pre.TargetGroupID),
			"observed_at":    pre.Identity.ObservedAt,
		},
	}
	if pre.Task != nil {
		out["task"] = map[string]any{
			"task_id":            pre.Task.ID,
			"state":              pre.Task.State,
			"mode":               pre.Task.Mode,
			"approval_id":        pre.Task.ApprovalID,
			"import_done":        pre.Task.ImportDone,
			"baseline_complete":  pre.Task.BaselineComplete,
			"max_attempts":       pre.Task.MaxAttempts,
			"max_total_attempts": pre.Task.MaxTotalAttempts,
			"goal_count":         pre.Task.GoalCount,
			"deadline_at":        pre.Task.Deadline,
			"accounts":           pre.Task.Accounts,
			"targets":            pre.Task.Targets,
		}
	}
	return out
}

func acquireWriterLock(ctx context.Context, store *taskengine.Store, key string) (func(), error) {
	conn, err := store.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire writer-lock connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, key).Scan(&locked); err != nil {
		conn.Release()
		return nil, fmt.Errorf("try writer lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, errors.New("another controlled writer already holds this account/action lock")
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key)
		conn.Release()
	}, nil
}

func writerLockKey(cfg executionConfig) string {
	return "p0-20-execute|" + cfg.WorkspaceID + "|" + cfg.AccountID
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
