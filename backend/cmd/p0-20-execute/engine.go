package main

// P0-20 execution chain. This file composes the existing Task Engine methods
// (Reserve -> AcceptOperation -> IssuePermit -> Dispatch -> bridge outbox ->
// inbox -> ApplyObservation) and the existing recovery methods. It never
// inserts or promotes an account, identity, heartbeat or capability fact.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
	"github.com/oapi-codegen/nullable"
)

type accountFacts struct {
	RuntimeID            string
	SelfID               string
	IdentityStatus       string
	ConnectionState      string
	LoginState           string
	ControlState         string
	RiskState            string
	IdentityGeneration   int64
	AccountOwnerEpoch    int64
	CredentialOwnerEpoch int64
	CredentialActive     bool
	MembershipAuthority  bool
	RequestJoinVerified  bool
	MembershipVerified   bool
	QuotaLimit           int
	HeartbeatAt          *time.Time
}

type approvalFacts struct {
	PrincipalID   string
	ScopeRevision int
	Action        string
	AccountIDs    []string
	TargetIDs     []string
	MaxAttempts   int
	ExpiresAt     time.Time
	Revoked       bool
}

type taskFacts struct {
	WorkspaceID, ID, PrincipalID, State, Mode string
	Version, ScopeRevision, GoalCount         int
	MaxAttempts, MaxTotalAttempts             int
	Deadline                                  time.Time
	ApprovalID                                *string
	ImportDone, BaselineComplete              bool
	Accounts                                  []string
	Targets                                   []string
	Candidate                                 contract.CandidateSource
}

type preflightResult struct {
	Account          accountFacts
	Task             *taskFacts
	Identity         identityFacts
	CleanerAvailable bool
	Credential       string
	TargetGroupID    string
	Ready            bool
	Blockers         []string
}

func schemaVersion(ctx context.Context, store *taskengine.Store) (int, error) {
	var version int
	if err := store.Pool.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read Task Engine schema version: %w", err)
	}
	return version, nil
}

func loadAccountFacts(ctx context.Context, store *taskengine.Store, workspace, accountID string) (accountFacts, error) {
	var out accountFacts
	err := store.Pool.QueryRow(ctx, `
		SELECT a.runtime_id,COALESCE(a.self_id,''),a.identity_status,a.connection_state,a.login_state,
		       a.control_state,a.risk_state,a.identity_generation,a.owner_epoch,
		       rc.owner_epoch,rc.active,rc.membership_authority,
		       a.request_join_verified,a.membership_verified,a.quota_limit,a.heartbeat_at
		  FROM core.account a
		  JOIN core.runtime_credentials rc
		    ON (rc.workspace_id,rc.runtime_id,rc.account_id)=(a.workspace_id,a.runtime_id,a.id)
		 WHERE a.workspace_id=$1 AND a.id=$2`, workspace, accountID).
		Scan(&out.RuntimeID, &out.SelfID, &out.IdentityStatus, &out.ConnectionState, &out.LoginState,
			&out.ControlState, &out.RiskState, &out.IdentityGeneration, &out.AccountOwnerEpoch,
			&out.CredentialOwnerEpoch, &out.CredentialActive, &out.MembershipAuthority,
			&out.RequestJoinVerified, &out.MembershipVerified, &out.QuotaLimit, &out.HeartbeatAt)
	return out, err
}

func loadTaskFacts(ctx context.Context, store *taskengine.Store, workspace, taskID string) (taskFacts, error) {
	var out taskFacts
	var candidateRaw []byte
	err := store.Pool.QueryRow(ctx, `
		SELECT workspace_id,id,principal_id,state,mode,version,scope_revision,goal_count,
		       max_attempts,max_total_attempts,deadline_at,approval_id,import_done,baseline_complete,
		       candidate_source
		  FROM core.task WHERE workspace_id=$1 AND id=$2`, workspace, taskID).
		Scan(&out.WorkspaceID, &out.ID, &out.PrincipalID, &out.State, &out.Mode, &out.Version,
			&out.ScopeRevision, &out.GoalCount, &out.MaxAttempts, &out.MaxTotalAttempts, &out.Deadline,
			&out.ApprovalID, &out.ImportDone, &out.BaselineComplete, &candidateRaw)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(candidateRaw, &out.Candidate); err != nil {
		return out, fmt.Errorf("decode persisted candidate source: %w", err)
	}
	rows, err := store.Pool.Query(ctx, `SELECT account_id FROM core.task_account
		WHERE workspace_id=$1 AND task_id=$2 ORDER BY account_id`, workspace, taskID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		out.Accounts = append(out.Accounts, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows, err = store.Pool.Query(ctx, `SELECT t.external_id
		  FROM core.task_target tt
		  JOIN core.target t ON (t.workspace_id,t.id)=(tt.workspace_id,tt.target_id)
		 WHERE tt.workspace_id=$1 AND tt.task_id=$2 ORDER BY t.external_id`, workspace, taskID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		out.Targets = append(out.Targets, id)
	}
	rows.Close()
	return out, rows.Err()
}

func loadApprovalFacts(ctx context.Context, store *taskengine.Store, workspace, approvalID string) (approvalFacts, error) {
	var out approvalFacts
	err := store.Pool.QueryRow(ctx, `
		SELECT principal_id,scope_revision,action,account_ids,target_ids,max_attempts,expires_at,revoked
		  FROM core.approval WHERE workspace_id=$1 AND id=$2`, workspace, approvalID).
		Scan(&out.PrincipalID, &out.ScopeRevision, &out.Action, &out.AccountIDs, &out.TargetIDs,
			&out.MaxAttempts, &out.ExpiresAt, &out.Revoked)
	return out, err
}

func principalExists(ctx context.Context, store *taskengine.Store, workspace, principal string) (bool, error) {
	var exists bool
	err := store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.principal WHERE workspace_id=$1 AND id=$2)`,
		workspace, principal).Scan(&exists)
	return exists, err
}

func (p *preflightResult) block(format string, args ...any) {
	p.Blockers = append(p.Blockers, fmt.Sprintf(format, args...))
}

func validateAccount(cfg executionConfig, facts accountFacts, taskMode bool) []string {
	var blockers []string
	add := func(format string, args ...any) { blockers = append(blockers, fmt.Sprintf(format, args...)) }
	if facts.RuntimeID != cfg.RuntimeID {
		add("persisted account runtime %q does not match configured runtime", facts.RuntimeID)
	}
	if facts.SelfID == "" || !validQQID(facts.SelfID) {
		add("persisted account has no valid authenticated self_id")
	}
	if facts.IdentityStatus != "verified" {
		add("persisted identity_status=%s, want verified", facts.IdentityStatus)
	}
	if facts.ConnectionState != "online" {
		add("persisted connection_state=%s, want online", facts.ConnectionState)
	}
	if facts.LoginState != "logged_in" {
		add("persisted login_state=%s, want logged_in", facts.LoginState)
	}
	if facts.ControlState != "active" {
		add("persisted control_state=%s, want active", facts.ControlState)
	}
	if facts.RiskState != "clear" {
		add("persisted risk_state=%s, want clear", facts.RiskState)
	}
	if facts.AccountOwnerEpoch != cfg.OwnerEpoch || facts.CredentialOwnerEpoch != cfg.OwnerEpoch {
		add("owner epoch mismatch: account=%d credential=%d config=%d",
			facts.AccountOwnerEpoch, facts.CredentialOwnerEpoch, cfg.OwnerEpoch)
	}
	if !facts.CredentialActive {
		add("runtime credential is inactive")
	}
	if facts.IdentityGeneration <= 0 {
		add("identity generation is not positive")
	}
	if facts.QuotaLimit <= 0 {
		add("account quota_limit is not positive")
	}
	if taskMode && (facts.HeartbeatAt == nil || facts.HeartbeatAt.IsZero()) {
		add("persisted runtime heartbeat is missing")
	}
	if taskMode {
		if !facts.RequestJoinVerified {
			add("request_join capability is not proven in persisted account facts")
		}
		if !facts.MembershipVerified {
			add("membership observation capability is not proven in persisted account facts")
		}
		if !facts.MembershipAuthority {
			add("runtime credential lacks membership authority")
		}
	}
	return blockers
}

func validateTaskFacts(cfg executionConfig, task taskFacts, now time.Time) []string {
	var blockers []string
	add := func(format string, args ...any) { blockers = append(blockers, fmt.Sprintf(format, args...)) }
	if task.PrincipalID != cfg.PrincipalID {
		add("task principal %q does not match configured principal", task.PrincipalID)
	}
	if task.Mode != "live" {
		add("task mode=%s, want live", task.Mode)
	}
	switch task.State {
	case "DRAFT", "VALIDATING", "READY", "RUNNING":
	default:
		add("task state=%s is not executable by this bounded entrypoint", task.State)
	}
	if !task.Deadline.After(now) {
		add("task deadline already passed")
	}
	if len(task.Accounts) != 1 || task.Accounts[0] != cfg.AccountID {
		add("task account scope must be exactly [%s]", cfg.AccountID)
	}
	if task.GoalCount != cfg.GoalCount {
		add("task goal_count=%d, configured %d", task.GoalCount, cfg.GoalCount)
	}
	if task.MaxAttempts < 1 || task.MaxAttempts > cfg.MaxAttemptsPerAction {
		add("task max_attempts=%d exceeds configured per-action budget %d", task.MaxAttempts, cfg.MaxAttemptsPerAction)
	}
	if task.MaxTotalAttempts < 1 || task.MaxTotalAttempts > cfg.MaxTotalExternalAttempts {
		add("task max_total_attempts=%d exceeds configured budget %d", task.MaxTotalAttempts, cfg.MaxTotalExternalAttempts)
	}
	if len(task.Candidate.Groups) != 1 || string(task.Candidate.Groups[0].GroupId) != cfg.TargetGroupID {
		add("task fixed candidate source is not exactly the configured target")
	}
	if len(task.Targets) > 1 || (len(task.Targets) == 1 && task.Targets[0] != cfg.TargetGroupID) {
		add("task imported target scope is not exactly the configured target")
	}
	if task.ApprovalID == nil || *task.ApprovalID == "" {
		add("task has no persisted approval")
	} else if cfg.ApprovalID != "" && *task.ApprovalID != cfg.ApprovalID {
		add("task approval %q does not match configured approval %q", *task.ApprovalID, cfg.ApprovalID)
	}
	return blockers
}

func validateApprovalFacts(cfg executionConfig, task *taskFacts, approval approvalFacts, now time.Time) []string {
	var blockers []string
	add := func(format string, args ...any) { blockers = append(blockers, fmt.Sprintf(format, args...)) }
	if approval.PrincipalID != cfg.PrincipalID {
		add("approval principal %q does not match configured principal", approval.PrincipalID)
	}
	if approval.Action != "request_join" {
		add("approval action=%s, want request_join", approval.Action)
	}
	if approval.Revoked {
		add("approval is revoked")
	}
	if !approval.ExpiresAt.After(now) {
		add("approval expired")
	}
	if len(approval.AccountIDs) != 1 || approval.AccountIDs[0] != cfg.AccountID {
		add("approval account scope must be exactly [%s]", cfg.AccountID)
	}
	if len(approval.TargetIDs) != 1 || approval.TargetIDs[0] != cfg.TargetGroupID {
		add("approval target scope must be exactly [%s]", cfg.TargetGroupID)
	}
	if approval.MaxAttempts < cfg.MaxAttemptsPerAction {
		add("approval max_attempts=%d is below configured per-action budget %d", approval.MaxAttempts, cfg.MaxAttemptsPerAction)
	}
	if task != nil {
		if approval.ScopeRevision != task.ScopeRevision {
			add("approval scope_revision=%d does not match task scope_revision=%d", approval.ScopeRevision, task.ScopeRevision)
		}
		if approval.MaxAttempts < task.MaxAttempts {
			add("approval max_attempts=%d is below task max_attempts=%d", approval.MaxAttempts, task.MaxAttempts)
		}
	}
	return blockers
}

func unresolvedBlockers(ctx context.Context, store *taskengine.Store, cfg executionConfig) ([]string, error) {
	var blockers []string
	var targetID string
	var targetDisabled bool
	err := store.Pool.QueryRow(ctx, `SELECT id,disabled FROM core.target WHERE workspace_id=$1 AND external_id=$2`,
		cfg.WorkspaceID, cfg.TargetGroupID).Scan(&targetID, &targetDisabled)
	if err != nil {
		if taskengine.Code(err) != "RESOURCE_NOT_FOUND" {
			return nil, err
		}
		targetID = ""
	} else {
		if targetDisabled {
			blockers = append(blockers, fmt.Sprintf("target %s is disabled in the persisted target table", cfg.TargetGroupID))
		}
	}
	if targetID != "" {
		var unresolved int
		if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM core.execution
			WHERE workspace_id=$1 AND target_id=$2
			  AND (state IN ('DISPATCHING','UNKNOWN') OR effect_certainty IN ('unknown','confirmed_submitted'))`,
			cfg.WorkspaceID, targetID).Scan(&unresolved); err != nil {
			return nil, err
		}
		if unresolved > 0 {
			blockers = append(blockers, fmt.Sprintf("target has %d unresolved external effect(s); reconciliation is required", unresolved))
		}
		var claimTask string
		err = store.Pool.QueryRow(ctx, `SELECT task_id FROM core.target_claim
			WHERE workspace_id=$1 AND target_id=$2 AND action_family='group.join' AND state='active'`,
			cfg.WorkspaceID, targetID).Scan(&claimTask)
		if err == nil && claimTask != cfg.TaskID {
			blockers = append(blockers, fmt.Sprintf("target claim is held by task %s", claimTask))
		} else if err != nil && taskengine.Code(err) != "RESOURCE_NOT_FOUND" {
			return nil, err
		}
	}
	var held bool
	err = store.Pool.QueryRow(ctx, `SELECT held FROM core.account_action_lock
		WHERE workspace_id=$1 AND account_id=$2 AND action_family='group.join'`,
		cfg.WorkspaceID, cfg.AccountID).Scan(&held)
	if err == nil && held {
		blockers = append(blockers, "account action lock is held; another send is still unresolved")
	} else if err != nil && taskengine.Code(err) != "RESOURCE_NOT_FOUND" {
		return nil, err
	}
	var reserved, consumed, quota int
	err = store.Pool.QueryRow(ctx, `SELECT
		COALESCE((SELECT reserved FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2
			AND action_family='group.join' AND window_start=current_date),0),
		COALESCE((SELECT consumed FROM core.account_usage WHERE workspace_id=$1 AND account_id=$2
			AND action_family='group.join' AND window_start=current_date),0),
		(SELECT quota_limit FROM core.account WHERE workspace_id=$1 AND id=$2)`,
		cfg.WorkspaceID, cfg.AccountID).Scan(&reserved, &consumed, &quota)
	if err != nil {
		return nil, err
	}
	if quota > 0 && reserved+consumed >= quota {
		blockers = append(blockers, fmt.Sprintf("account action quota is exhausted (%d/%d)", reserved+consumed, quota))
	}
	return blockers, nil
}

func runPreflight(ctx context.Context, store *taskengine.Store, loaded loadedConfig, live *liveClient, taskMode bool) (out preflightResult, runErr error) {
	defer func() {
		if len(out.Blockers) > 0 {
			out.Blockers = []string{"preflight validation failed; consult restricted run diagnostics"}
		}
	}()
	now := time.Now().UTC()
	cfg := loaded.cfg
	out.TargetGroupID = cfg.TargetGroupID
	facts, err := loadAccountFacts(ctx, store, cfg.WorkspaceID, cfg.AccountID)
	if err != nil {
		if taskengine.Code(err) == "RESOURCE_NOT_FOUND" {
			out.block("persisted account %s does not exist", cfg.AccountID)
			return out, nil
		}
		return out, err
	}
	out.Account = facts

	identity, err := live.observeIdentity(ctx, taskMode)
	if err != nil {
		out.block("authenticated readback failed: %v", err)
		return out, nil
	}
	out.Identity = identity
	if facts.SelfID != "" && identity.SelfID != facts.SelfID {
		out.block("observed self_id %s does not match persisted self_id %s", identity.SelfID, facts.SelfID)
	}
	if taskMode {
		if containsGroup(identity.GroupIDs, cfg.TargetGroupID) {
			out.block("target %s is already present in the complete group baseline", cfg.TargetGroupID)
		}
		if err := live.cleanerAvailable(ctx); err != nil {
			out.block("cleaner capability preflight failed: %v", err)
		} else {
			out.CleanerAvailable = true
		}
	}

	if cfg.TaskID != "" {
		task, err := loadTaskFacts(ctx, store, cfg.WorkspaceID, cfg.TaskID)
		if err != nil {
			if taskengine.Code(err) == "RESOURCE_NOT_FOUND" {
				out.block("persisted task %s does not exist", cfg.TaskID)
				return out, nil
			}
			return out, err
		}
		out.Task = &task
		out.Blockers = append(out.Blockers, validateTaskFacts(cfg, task, now)...)
		freshBlockers, err := freshAttemptBlockers(ctx, store, cfg.WorkspaceID, cfg.TaskID)
		if err != nil {
			return out, err
		}
		out.Blockers = append(out.Blockers, freshBlockers...)
		if task.ApprovalID == nil || *task.ApprovalID == "" {
			out.block("task has no approval to re-read")
		} else {
			approval, err := loadApprovalFacts(ctx, store, cfg.WorkspaceID, *task.ApprovalID)
			if err != nil {
				if taskengine.Code(err) == "RESOURCE_NOT_FOUND" {
					out.block("task approval %s does not exist", *task.ApprovalID)
				} else {
					return out, err
				}
			} else {
				out.Blockers = append(out.Blockers, validateApprovalFacts(cfg, &task, approval, now)...)
			}
		}
	} else {
		if ok, err := principalExists(ctx, store, cfg.WorkspaceID, cfg.PrincipalID); err != nil {
			return out, err
		} else if !ok {
			out.block("principal %s does not exist", cfg.PrincipalID)
		}
		if cfg.ApprovalID != "" {
			approval, err := loadApprovalFacts(ctx, store, cfg.WorkspaceID, cfg.ApprovalID)
			if err != nil {
				if taskengine.Code(err) == "RESOURCE_NOT_FOUND" {
					out.block("approval %s does not exist", cfg.ApprovalID)
				} else {
					return out, err
				}
			} else {
				out.Blockers = append(out.Blockers, validateApprovalFacts(cfg, nil, approval, now)...)
			}
		} else if cfg.ApprovalExpiresAt == nil {
			out.block("approval_expires_at is required when creating the bounded approval")
		}
	}

	extra, err := unresolvedBlockers(ctx, store, cfg)
	if err != nil {
		return out, err
	}
	out.Blockers = append(out.Blockers, extra...)
	out.Ready = len(out.Blockers) == 0
	return out, nil
}

type recoverySummary struct {
	RecoveredDispatching int `json:"recovered_dispatching"`
	ExpiredPermits       int `json:"expired_permits"`
	BridgeOutboxAccepted int `json:"bridge_outbox_accepted"`
	InboxApplied         int `json:"inbox_applied"`
}

// runRecovery first quarantines this runtime's DISPATCHING operations as
// UNKNOWN, expires provably unconsumed permits, then reduces every persisted
// bridge/inbox fact. It performs no QQ call and creates no new work.
func runRecovery(ctx context.Context, store *taskengine.Store, rt taskengine.RuntimeActor, ev *evidence) (recoverySummary, error) {
	var out recoverySummary
	recovered, err := store.RecoverDispatching(ctx, rt)
	if err != nil {
		return out, fmt.Errorf("recover dispatching: %w", err)
	}
	out.RecoveredDispatching = recovered
	expired, err := store.ExpirePermits(ctx, rt)
	if err != nil {
		return out, fmt.Errorf("expire permits: %w", err)
	}
	out.ExpiredPermits = expired
	accepted, err := store.PumpBridgeOutboxBatch(ctx, rt, 100)
	if err != nil {
		return out, fmt.Errorf("pump bridge outbox: %w", err)
	}
	out.BridgeOutboxAccepted = accepted
	applied, err := applyPendingInbox(ctx, store, rt)
	if err != nil {
		return out, err
	}
	out.InboxApplied = applied
	ev.step("recovery", map[string]any{
		"recovered_dispatching":  recovered,
		"expired_permits":        expired,
		"bridge_outbox_accepted": accepted,
		"inbox_applied":          applied,
	})
	return out, nil
}

func applyPendingInbox(ctx context.Context, store *taskengine.Store, rt taskengine.RuntimeActor) (int, error) {
	rows, err := store.Pool.Query(ctx, `SELECT event_id FROM core.inbox
		WHERE workspace_id=$1 AND source_runtime_id=$2 AND apply_state='pending'
		ORDER BY received_at,event_id LIMIT 100`, rt.WorkspaceID, rt.RuntimeID)
	if err != nil {
		return 0, err
	}
	var events []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, eventID := range events {
		if err := store.ApplyObservation(ctx, rt.WorkspaceID, rt.RuntimeID, eventID); err != nil {
			return 0, fmt.Errorf("apply inbox event %s: %w", eventID, err)
		}
	}
	return len(events), nil
}

func ensurePrincipal(ctx context.Context, store *taskengine.Store, cfg executionConfig) error {
	ok, err := principalExists(ctx, store, cfg.WorkspaceID, cfg.PrincipalID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("principal %s does not exist; this command never creates principals", cfg.PrincipalID)
	}
	return nil
}

func createApproval(ctx context.Context, store *taskengine.Store, loaded loadedConfig) (string, error) {
	cfg := loaded.cfg
	approvalID := randomCode("appr_p020")
	if len(approvalID) > maxConfigurationIDLen {
		return "", errors.New("generated approval ID is too long")
	}
	_, err := store.Pool.Exec(ctx, `INSERT INTO core.approval(
		workspace_id,id,principal_id,scope_revision,account_ids,target_ids,max_attempts,expires_at,join_reason,verification_message)
		VALUES($1,$2,$3,1,$4,$5,$6,$7,$8,$9)`,
		cfg.WorkspaceID, approvalID, cfg.PrincipalID, []string{cfg.AccountID}, []string{cfg.TargetGroupID},
		cfg.MaxAttemptsPerAction, cfg.ApprovalExpiresAt.value(), cfg.JoinReason, cfg.JoinAnswer)

	if err != nil {
		return "", fmt.Errorf("persist bounded approval: %w", err)
	}
	return approvalID, nil
}

func prepareTask(ctx context.Context, store *taskengine.Store, actor taskengine.Actor, loaded loadedConfig, identity identityFacts, ev *evidence) (string, error) {
	cfg := loaded.cfg
	taskID := cfg.TaskID
	if taskID == "" {
		if err := ensurePrincipal(ctx, store, cfg); err != nil {
			return "", err
		}
		approvalID := cfg.ApprovalID
		if approvalID == "" {
			created, err := createApproval(ctx, store, loaded)
			if err != nil {
				return "", err
			}
			approvalID = created
			ev.step("approval_created", map[string]any{"approval_id": approvalID, "expires_at": cfg.ApprovalExpiresAt.value()})
		}
		liveMode := contract.ExecutionMode("live")
		request := contract.CreateTaskRequest{
			JoinReason:          cfg.JoinReason,
			VerificationMessage: cfg.JoinAnswer,
			Type:                "group.acquire", Title: cfg.TaskTitle,
			Goal: contract.Goal{Metric: "new_unique_groups_for_account_pool", Count: cfg.GoalCount,
				StrictTargetCap: true, RequireFinalMembershipCheck: true},
			AllowedAccountIds: []string{cfg.AccountID},
			CandidateSource: contract.CandidateSource{Kind: "fixed",
				Groups: []contract.FixedCandidate{{GroupId: contract.QQId(cfg.TargetGroupID), SourceRef: "p020_owner_approved"}}},
			ExecutionMode: &liveMode,
			DeadlineAt:    cfg.TaskDeadlineAt.value(),
			Limits: contract.TaskLimits{
				MaxCandidates:            cfg.MaxCandidates,
				MaxAttemptsPerAction:     cfg.MaxAttemptsPerAction,
				MaxTotalExternalAttempts: cfg.MaxTotalExternalAttempts,
				MaxModelAttempts:         0,
			},
			ApprovalRef: nullable.NewNullableWithValue(approvalID),
			Submit:      true,
		}
		task, err := store.CreateTask(ctx, actor, randomCode("create_p020"), request)
		if err != nil {
			return "", fmt.Errorf("create persisted task: %w", err)
		}
		taskID = task.Id
		ev.step("task_created", map[string]any{"task_id": taskID, "approval_id": approvalID})
	}

	for page := 0; page < 8; page++ {
		done, err := store.ExpandFixedPage(ctx, actor, taskID, 100)
		if err != nil {
			return "", fmt.Errorf("expand fixed candidate page: %w", err)
		}
		if done {
			break
		}
		if page == 7 {
			return "", errors.New("fixed candidate expansion did not finish in bounded pages")
		}
	}
	if err := store.CompleteBaseline(ctx, actor, taskID, []taskengine.BaselineSnapshot{{
		AccountID:   cfg.AccountID,
		EvidenceRef: "p020_baseline_group_list",
		ObservedAt:  identity.ObservedAt,
		Complete:    true,
		GroupIDs:    identity.GroupIDs,
	}}); err != nil {
		return "", fmt.Errorf("persist complete baseline: %w", err)
	}
	task, err := store.GetTask(ctx, actor, taskID)
	if err != nil {
		return "", fmt.Errorf("read task before start: %w", err)
	}
	if task.State != contract.TaskState("RUNNING") {
		if _, err := store.ControlTask(ctx, actor, taskID, randomCode("start_p020"), contract.TaskControlRequest{
			Action: "start", ExpectedVersion: task.Version, Reason: "P0-20 bounded controlled execution",
		}); err != nil {
			return "", fmt.Errorf("start task: %w", err)
		}
	}
	ev.step("task_ready", map[string]any{"task_id": taskID})
	return taskID, nil
}

func freshAttemptBlockers(ctx context.Context, store *taskengine.Store, workspace, taskID string) ([]string, error) {
	var executions, retries, attempts int
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM core.execution
		WHERE workspace_id=$1 AND task_id=$2`, workspace, taskID).Scan(&executions); err != nil {
		return nil, err
	}
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM core.retry
		WHERE workspace_id=$1 AND task_id=$2 AND state='scheduled'`, workspace, taskID).Scan(&retries); err != nil {
		return nil, err
	}
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM core.subtask
		WHERE workspace_id=$1 AND task_id=$2 AND attempt_count>0`, workspace, taskID).Scan(&attempts); err != nil {
		return nil, err
	}
	var blockers []string
	if executions > 0 {
		blockers = append(blockers, fmt.Sprintf("task %s already has %d execution(s); this entrypoint never executes a retry", taskID, executions))
	}
	if tries := retries + attempts; tries > 0 {
		blockers = append(blockers, fmt.Sprintf("task %s has %d scheduled retry or prior attempt indicator(s); this entrypoint never executes a retry", taskID, tries))
	}
	return blockers, nil
}

func ensureFreshAttempt(ctx context.Context, store *taskengine.Store, workspace, taskID string) error {
	blockers, err := freshAttemptBlockers(ctx, store, workspace, taskID)
	if err != nil {
		return err
	}
	if len(blockers) > 0 {
		return errors.New(strings.Join(blockers, "; "))
	}
	return nil
}

func reduceOperation(ctx context.Context, store *taskengine.Store, rt taskengine.RuntimeActor, operationID string, ev *evidence) (int, error) {
	accepted, err := store.PumpBridgeOutboxBatch(ctx, rt, 100)
	if err != nil {
		return 0, fmt.Errorf("pump bridge outbox: %w", err)
	}
	rows, err := store.Pool.Query(ctx, `SELECT event_id FROM core.inbox
		WHERE workspace_id=$1 AND source_runtime_id=$2 AND operation_id=$3 AND apply_state='pending'
		ORDER BY received_at,event_id`, rt.WorkspaceID, rt.RuntimeID, operationID)
	if err != nil {
		return accepted, err
	}
	var events []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return accepted, err
		}
		events = append(events, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return accepted, err
	}
	for _, eventID := range events {
		if err := store.ApplyObservation(ctx, rt.WorkspaceID, rt.RuntimeID, eventID); err != nil {
			return accepted, fmt.Errorf("apply operation inbox event %s: %w", eventID, err)
		}
	}
	ev.step("reduce_operation", map[string]any{"bridge_outbox_accepted": accepted, "inbox_applied": len(events)})
	return accepted, nil
}

func observationSequence(ctx context.Context, store *taskengine.Store, workspace, runtime, account string) (int64, error) {
	var sequence int64
	err := store.Pool.QueryRow(ctx, `SELECT COALESCE(max(source_sequence),0)+1 FROM core.inbox
		WHERE workspace_id=$1 AND source_runtime_id=$2 AND source_account_id=$3`,
		workspace, runtime, account).Scan(&sequence)
	return sequence, err
}

// observeMembershipOnce performs exactly one complete group-list read. Only
// positive membership is recorded; absence remains pending/unknown and is
// never converted into a rejection or a retry.
func observeMembershipOnce(ctx context.Context, store *taskengine.Store, live *liveClient, rt taskengine.RuntimeActor, reservation taskengine.Reservation) (bool, error) {
	joined, err := live.observeMembership(ctx, reservation.GroupID)
	if err != nil {
		return false, err
	}
	if !joined {
		return false, nil
	}
	sequence, err := observationSequence(ctx, store, rt.WorkspaceID, rt.RuntimeID, reservation.AccountID)
	if err != nil {
		return false, err
	}
	eventID := randomCode("evt_p020")
	evidenceRef := randomCode("membership_p020")
	observation := taskengine.Observation{
		EventID:          eventID,
		OperationID:      reservation.OperationID,
		AccountID:        reservation.AccountID,
		TargetID:         reservation.TargetID,
		Kind:             "membership.observed",
		State:            "joined",
		EvidenceRef:      evidenceRef,
		SourceEpoch:      rt.OwnerEpoch,
		SourceSequence:   sequence,
		ObservedAt:       time.Now().UTC(),
		SnapshotComplete: true,
	}
	if err := store.ReceiveObservation(ctx, rt, observation); err != nil {
		return false, fmt.Errorf("receive membership observation: %w", err)
	}
	if err := store.ApplyObservation(ctx, rt.WorkspaceID, rt.RuntimeID, eventID); err != nil {
		return false, fmt.Errorf("apply membership observation: %w", err)
	}
	return true, nil
}

type finalFacts struct {
	ExecutionState     string
	Disposition        string
	EffectCertainty    string
	RequestRef         string
	EvidenceRef        string
	TaskState          string
	ValidResults       int
	MembershipState    string
	GoalSlotsReserved  int
	GoalSlotsFulfilled int
}

func loadFinalFacts(ctx context.Context, store *taskengine.Store, reservation taskengine.Reservation) (finalFacts, error) {
	var out finalFacts
	err := store.Pool.QueryRow(ctx, `SELECT
		e.state,COALESCE(e.disposition,''),e.effect_certainty,COALESCE(e.external_request_ref,''),
		COALESCE(e.evidence_ref,''),t.state,
		(SELECT count(*) FROM core.result r WHERE r.workspace_id=e.workspace_id AND r.task_id=e.task_id AND r.target_id=e.target_id AND r.valid),
		COALESCE((SELECT m.state FROM core.membership m
			WHERE m.workspace_id=e.workspace_id AND m.account_id=e.account_id AND m.target_id=e.target_id),''),
		(SELECT count(*) FROM core.goal_slot gs WHERE gs.workspace_id=e.workspace_id AND gs.task_id=e.task_id AND gs.state='reserved'),
		(SELECT count(*) FROM core.goal_slot gs WHERE gs.workspace_id=e.workspace_id AND gs.task_id=e.task_id AND gs.state='fulfilled')
		FROM core.execution e JOIN core.task t ON (t.workspace_id,t.id)=(e.workspace_id,e.task_id)
		WHERE e.workspace_id=$1 AND e.id=$2`, reservation.WorkspaceID, reservation.ExecutionID).
		Scan(&out.ExecutionState, &out.Disposition, &out.EffectCertainty, &out.RequestRef, &out.EvidenceRef,
			&out.TaskState, &out.ValidResults, &out.MembershipState, &out.GoalSlotsReserved, &out.GoalSlotsFulfilled)
	return out, err
}

func executeTask(ctx context.Context, store *taskengine.Store, actor taskengine.Actor, rt taskengine.RuntimeActor, loaded loadedConfig, live *liveClient, pre preflightResult, ev *evidence) (map[string]any, error) {
	cfg := loaded.cfg
	summary := map[string]any{}
	taskID, err := func() (string, error) {
		if cfg.TaskID != "" {
			if err := ensureFreshAttempt(ctx, store, cfg.WorkspaceID, cfg.TaskID); err != nil {
				return "", err
			}
		}
		return prepareTask(ctx, store, actor, loaded, pre.Identity, ev)
	}()
	if err != nil {
		return summary, err
	}
	summary["task_id"] = taskID
	// A retry is never an automatic side effect of this entrypoint. A task
	// that already produced an execution or has a scheduled retry is left for
	// explicit reconciliation instead of being mutated again.
	if err := ensureFreshAttempt(ctx, store, cfg.WorkspaceID, taskID); err != nil {
		return summary, err
	}
	// Re-read the persisted scope after preparation; it is the authoritative
	// input to Reserve, not the preflight snapshot.
	task, err := loadTaskFacts(ctx, store, cfg.WorkspaceID, taskID)
	if err != nil {
		return summary, err
	}
	if blockers := validateTaskFacts(cfg, task, time.Now().UTC()); len(blockers) > 0 {
		return summary, fmt.Errorf("prepared task failed bounded scope validation: %s", strings.Join(blockers, "; "))
	}

	reservation, err := store.Reserve(ctx, cfg.WorkspaceID, taskID, cfg.AccountID)
	if err != nil {
		return summary, fmt.Errorf("reserve controlled attempt: %w", err)
	}
	summary["operation_id"] = reservation.OperationID
	summary["execution_id"] = reservation.ExecutionID
	summary["target_id"] = reservation.TargetID
	summary["attempt"] = reservation.Attempt
	if reservation.AccountID != cfg.AccountID || reservation.RuntimeID != cfg.RuntimeID ||
		reservation.OwnerEpoch != cfg.OwnerEpoch || reservation.TaskID != taskID ||
		reservation.GroupID != cfg.TargetGroupID {
		return summary, fmt.Errorf("reservation escaped configured writer scope: account=%s runtime=%s epoch=%d task=%s group=%s",
			reservation.AccountID, reservation.RuntimeID, reservation.OwnerEpoch, reservation.TaskID, reservation.GroupID)
	}
	if reservation.Attempt != 1 {
		return summary, fmt.Errorf("reservation attempt=%d; this entrypoint never executes a second attempt", reservation.Attempt)
	}
	ev.step("reserved", map[string]any{"operation_id": reservation.OperationID, "target_id": reservation.TargetID, "attempt": reservation.Attempt})

	accepted, err := live.bridge.accept(ctx, reservation.OperationID)
	if err != nil {
		return summary, fmt.Errorf("accept operation through Task Bridge: %w", err)
	}
	if accepted.OperationID != reservation.OperationID || accepted.Digest != reservation.Digest ||
		accepted.AccountID != reservation.AccountID || accepted.GroupID != reservation.GroupID ||
		accepted.JoinReason != reservation.JoinReason || accepted.VerificationMessage != reservation.VerificationMessage ||
		accepted.OwnerEpoch != reservation.OwnerEpoch || accepted.Fence != reservation.Fence {
		return summary, errors.New("Task Bridge accepted operation does not match immutable reservation")
	}
	permitID, err := live.bridge.permit(ctx, reservation.OperationID, accepted)
	if err != nil {
		return summary, fmt.Errorf("issue dispatch permit through Task Bridge: %w", err)
	}
	summary["permit_id"] = permitID
	ev.step("permitted", map[string]any{"permit_id": permitID})
	outcome, dispatchErr := live.bridge.dispatch(ctx, reservation.OperationID)
	if dispatchErr != nil {
		// Once a dispatch may have reached the Gateway, only query status.
		if status, statusErr := live.bridge.status(ctx, reservation.OperationID); statusErr == nil && status.Data.Permit != nil &&
			string(status.Data.Permit.State) != "issued" {
			outcome = taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}
		}
	}
	summary["dispatch_outcome"] = map[string]any{"disposition": outcome.Disposition, "effect_certainty": outcome.EffectCertainty,
		"request_ref": outcome.RequestRef, "evidence_ref": outcome.EvidenceRef}
	ev.step("dispatched", map[string]any{"disposition": outcome.Disposition, "effect_certainty": outcome.EffectCertainty,
		"error": errorText(dispatchErr)})

	if _, reduceErr := reduceOperation(ctx, store, rt, reservation.OperationID, ev); reduceErr != nil {
		return summary, reduceErr
	}
	joined, membershipErr := observeMembershipOnce(ctx, store, live, rt, reservation)
	if membershipErr != nil {
		if dispatchErr != nil {
			// Keep the original dispatch outcome; the read failure must not
			// turn an unknown send into a retry or a success.
			summary["membership_error"] = membershipErr.Error()
		} else {
			return summary, membershipErr
		}
	}
	summary["membership_observed"] = joined

	final, err := loadFinalFacts(ctx, store, reservation)
	if err != nil {
		return summary, err
	}
	summary["execution"] = map[string]any{
		"state": final.ExecutionState, "disposition": final.Disposition,
		"effect_certainty": final.EffectCertainty, "request_ref": final.RequestRef,
		"evidence_ref": final.EvidenceRef,
	}
	summary["task_state"] = final.TaskState
	summary["valid_results"] = final.ValidResults
	summary["membership_state"] = final.MembershipState
	summary["goal_slots_reserved"] = final.GoalSlotsReserved
	summary["goal_slots_fulfilled"] = final.GoalSlotsFulfilled
	switch {
	case dispatchErr != nil:
		summary["result"] = "UNKNOWN_OR_REFUSED"
		return summary, dispatchErr
	case joined:
		summary["result"] = "CONFIRMED_MEMBERSHIP"
		return summary, nil
	case outcome.Disposition == "submitted":
		summary["result"] = "SUBMITTED_PENDING_MEMBERSHIP"
		return summary, nil
	case outcome.Disposition == "rejected":
		summary["result"] = "REJECTED_BEFORE_SEND"
		return summary, nil
	case outcome.Disposition == "failed_no_effect":
		summary["result"] = "FAILED_NO_EFFECT"
		return summary, nil
	default:
		summary["result"] = "EXTERNAL_OUTCOME_UNRESOLVED"
		return summary, nil
	}
	return summary, nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
