package main

// Seed mode writes exactly one product fixture. It is additive within the
// disposable database, refuses to overwrite an existing ws_fixture, and creates
// no group summary, login session, token or group message.
//
// Three running live tasks are created through the real CreateTask ->
// ExpandFixedPage -> CompleteBaseline -> ControlTask chain, then each is switched
// to proposal orchestration explicitly. None of them has a proposal or work item
// yet: the console/scenario create and approve their own.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oapi-codegen/nullable"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
)

type seededAccount struct {
	ID        string
	Runtime   string
	SelfID    string
	AdapterID string
}

var fixtureAccounts = []seededAccount{
	{ID: "cross-a", Runtime: "cross-runtime-a", SelfID: "11001", AdapterID: "fixture-adapter-a"},
	{ID: "cross-b", Runtime: "cross-runtime-b", SelfID: "11002", AdapterID: "fixture-adapter-b"},
}

var (
	proposalTargets = []string{"22001", "22002"}
	browserTargets  = []string{"23001", "23002"}
	graphTargets    = []string{"24001", "24002"}
)

const (
	proposalApprovalID = "ap_product_proposal_22001_22002"
	browserApprovalID  = "ap_product_browser_23001_23002"
	graphApprovalID    = "ap_product_graph_24001_24002"
)

type seedAccountView struct {
	ID                  string `json:"id"`
	RuntimeID           string `json:"runtime_id"`
	SelfID              string `json:"self_id"`
	AdapterInstanceID   string `json:"adapter_instance_id"`
	OwnerEpoch          int64  `json:"owner_epoch"`
	MembershipAuthority bool   `json:"membership_authority"`
	QuotaLimit          int    `json:"quota_limit"`
	HeartbeatPresent    bool   `json:"heartbeat_present"`
}

type seedPolicyView struct {
	AccountID  string `json:"account_id"`
	Enabled    bool   `json:"enabled"`
	DailyLimit int    `json:"daily_limit"`
	Version    int    `json:"version"`
}

type seedBindingView struct {
	GroupID                string    `json:"group_id"`
	ActiveSpeakerAccountID string    `json:"active_speaker_account_id"`
	BindingEpoch           string    `json:"binding_epoch"`
	LeaseExpiresAt         time.Time `json:"lease_expires_at"`
	Version                int       `json:"version"`
}

type seedTaskView struct {
	ID                    string   `json:"id"`
	Version               int      `json:"version"`
	State                 string   `json:"state"`
	Mode                  string   `json:"mode"`
	OrchestrationMode     string   `json:"orchestration_mode"`
	AuthorizationRevision int      `json:"authorization_revision"`
	ApprovalRef           string   `json:"approval_ref"`
	ApprovalScopeDigest   string   `json:"approval_scope_digest"`
	TargetGroupIDs        []string `json:"target_group_ids"`
	GoalCount             int      `json:"goal_count"`
	MaxModelAttempts      int      `json:"max_model_attempts"`
}

type seedResult struct {
	SchemaVersion       int                     `json:"schema_version"`
	WorkspaceID         string                  `json:"workspace_id"`
	PrincipalID         string                  `json:"principal_id"`
	ReadOnlyPrincipalID string                  `json:"read_only_principal_id"`
	Accounts            []seedAccountView       `json:"accounts"`
	ChatPolicies        []seedPolicyView        `json:"chat_policies"`
	GroupBindings       []seedBindingView       `json:"group_bindings"`
	Approvals           map[string]string       `json:"approvals"`
	Tasks               map[string]seedTaskView `json:"tasks"`
	ProposalTaskID      string                  `json:"proposal_task_id"`
	BrowserTaskID       string                  `json:"browser_task_id"`
	GraphTaskID         string                  `json:"graph_task_id"`
	ProposalTaskVersion int                     `json:"proposal_task_version"`
	BrowserTaskVersion  int                     `json:"browser_task_version"`
	GraphTaskVersion    int                     `json:"graph_task_version"`
	ProposalApprovalRef string                  `json:"proposal_approval_ref"`
	BrowserApprovalRef  string                  `json:"browser_approval_ref"`
	GraphApprovalRef    string                  `json:"graph_approval_ref"`
	ProposalTargets     []string                `json:"proposal_targets"`
	BrowserTargets      []string                `json:"browser_targets"`
	GraphTargets        []string                `json:"graph_targets"`
	GroupSummarySeeded  bool                    `json:"group_summary_seeded"`
}

func operatorActor() taskengine.Actor {
	return taskengine.Actor{
		WorkspaceID: workspaceID,
		PrincipalID: operatorPrincipalID,
		Permissions: map[string]bool{
			"task.create":          true,
			"task.read":            true,
			"task.control":         true,
			"task.approve":         true,
			"task.manage_all":      true,
			"account.chat.manage":  true,
			"group.binding.manage": true,
			"operator.read":        true,
		},
	}
}

func seedFixture(ctx context.Context, store *taskengine.Store, schemaVersion int) (*seedResult, error) {
	var exists bool
	must(store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.workspace WHERE id=$1)`, workspaceID).Scan(&exists),
		"check existing workspace")
	if exists {
		return nil, fmt.Errorf("workspace %q already exists; refusing to clobber an existing fixture", workspaceID)
	}
	actor := operatorActor()
	operatorPermissions, err := json.Marshal(actor.Permissions)
	must(err, "encode operator permissions")
	readerPermissions, err := json.Marshal(map[string]bool{"task.read": true, "operator.read": true})
	must(err, "encode read-only permissions")

	_, err = store.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES($1)`, workspaceID)
	must(err, "insert workspace")
	_, err = store.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id,permissions) VALUES($1,$2,$3)`,
		workspaceID, operatorPrincipalID, operatorPermissions)
	must(err, "insert operator principal")
	_, err = store.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id,permissions) VALUES($1,$2,$3)`,
		workspaceID, readOnlyPrincipalID, readerPermissions)
	must(err, "insert read-only principal")

	out := &seedResult{
		SchemaVersion:       schemaVersion,
		WorkspaceID:         workspaceID,
		PrincipalID:         operatorPrincipalID,
		ReadOnlyPrincipalID: readOnlyPrincipalID,
		Approvals:           map[string]string{},
		Tasks:               map[string]seedTaskView{},
		ProposalApprovalRef: proposalApprovalID,
		BrowserApprovalRef:  browserApprovalID,
		GraphApprovalRef:    graphApprovalID,
		ProposalTargets:     append([]string(nil), proposalTargets...),
		BrowserTargets:      append([]string(nil), browserTargets...),
		GraphTargets:        append([]string(nil), graphTargets...),
		GroupSummarySeeded:  false,
	}

	for _, account := range fixtureAccounts {
		_, err := store.Pool.Exec(ctx, `
			INSERT INTO core.account(
			 workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,
			 connection_state,login_state,control_state,risk_state,
			 request_join_verified,membership_verified,quota_limit,next_action_at,heartbeat_at)
			VALUES($1,$2,$3,$4,$5,'verified','online','logged_in','active','clear',true,true,20,clock_timestamp(),clock_timestamp())`,
			workspaceID, account.ID, account.Runtime, account.AdapterID, account.SelfID)
		must(err, "insert account "+account.ID)
		_, err = store.Pool.Exec(ctx, `
			INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority)
			VALUES($1,$2,$3,1,true)`, workspaceID, account.Runtime, account.ID)
		must(err, "insert runtime credentials "+account.ID)
		var heartbeat time.Time
		must(store.Pool.QueryRow(ctx, `SELECT heartbeat_at FROM core.account WHERE workspace_id=$1 AND id=$2`,
			workspaceID, account.ID).Scan(&heartbeat), "read account heartbeat")
		out.Accounts = append(out.Accounts, seedAccountView{
			ID: account.ID, RuntimeID: account.Runtime, SelfID: account.SelfID,
			AdapterInstanceID: account.AdapterID, OwnerEpoch: 1, MembershipAuthority: true,
			QuotaLimit: 20, HeartbeatPresent: !heartbeat.IsZero(),
		})
	}

	for _, account := range fixtureAccounts {
		policy, err := store.SetChatPolicy(ctx, actor, account.ID, "fixture-chat-policy-"+account.ID,
			taskengine.ChatPolicyRequest{Enabled: true, DailyLimit: 20})
		must(err, "set chat policy "+account.ID)
		require(policy.Enabled && policy.DailyLimit == 20,
			"chat policy %s enabled=%v limit=%d", account.ID, policy.Enabled, policy.DailyLimit)
		out.ChatPolicies = append(out.ChatPolicies, seedPolicyView{
			AccountID: account.ID, Enabled: policy.Enabled, DailyLimit: policy.DailyLimit, Version: policy.Version,
		})
	}

	leaseExpires := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	bindings := []struct{ groupID, accountID string }{
		{groupID: "21001", accountID: "cross-a"},
		{groupID: "21002", accountID: "cross-b"},
	}
	for _, binding := range bindings {
		lease, err := store.SetGroupSpeakerLease(ctx, actor, binding.groupID, "fixture-speaker-"+binding.groupID,
			taskengine.GroupSpeakerLeaseRequest{
				AccountID: binding.accountID, BindingEpoch: "1", LeaseExpiresAt: leaseExpires,
			})
		must(err, "set speaker lease "+binding.groupID)
		require(lease.ActiveSpeakerAccountID == binding.accountID && lease.BindingEpoch == "1",
			"speaker lease %s account=%s epoch=%s", binding.groupID, lease.ActiveSpeakerAccountID, lease.BindingEpoch)
		out.GroupBindings = append(out.GroupBindings, seedBindingView{
			GroupID: lease.GroupID, ActiveSpeakerAccountID: lease.ActiveSpeakerAccountID,
			BindingEpoch: lease.BindingEpoch, LeaseExpiresAt: lease.LeaseExpiresAt.UTC(), Version: lease.Version,
		})
	}

	// The product fixture never seeds group summaries; a pre-existing summary
	// would mean this is not the empty fixture we were told to own.
	require(scalarInt(ctx, store, `SELECT count(*) FROM core.group_summary`) == 0,
		"fixture database already contains group_summary rows")

	proposal, err := createRunningProposalTask(ctx, store, actor, "Product fixture proposal task", "proposal", proposalTargets)
	must(err, "seed proposal task")
	browser, err := createRunningProposalTask(ctx, store, actor, "Product fixture browser task", "browser", browserTargets)
	must(err, "seed browser task")
	graph, err := createRunningProposalTask(ctx, store, actor, "Product fixture graph task", "graph", graphTargets)
	must(err, "seed graph task")

	out.Tasks["proposal"] = proposal
	out.Tasks["browser"] = browser
	out.Tasks["graph"] = graph
	out.ProposalTaskID, out.ProposalTaskVersion = proposal.ID, proposal.Version
	out.BrowserTaskID, out.BrowserTaskVersion = browser.ID, browser.Version
	out.GraphTaskID, out.GraphTaskVersion = graph.ID, graph.Version
	out.Approvals["proposal"] = proposalApprovalID
	out.Approvals["browser"] = browserApprovalID
	out.Approvals["graph"] = graphApprovalID
	return out, nil
}

func createRunningProposalTask(ctx context.Context, store *taskengine.Store, actor taskengine.Actor, title, suffix string, targets []string) (seedTaskView, error) {
	accountIDs := make([]string, 0, len(fixtureAccounts))
	allowed := make([]contract.Id, 0, len(fixtureAccounts))
	for _, account := range fixtureAccounts {
		accountIDs = append(accountIDs, account.ID)
		allowed = append(allowed, contract.Id(account.ID))
	}
	approvalID := "ap_product_" + suffix + "_" + targets[0] + "_" + targets[1]
	_, err := store.Pool.Exec(ctx, `
		INSERT INTO core.approval(
		 workspace_id,id,principal_id,scope_revision,account_ids,target_ids,max_attempts,expires_at,join_reason,verification_message)
		VALUES($1,$2,$3,1,$4,$5,10,clock_timestamp()+interval '1 day',$6,$7)`,
		workspaceID, approvalID, operatorPrincipalID, accountIDs, targets, fixtureJoinReason, fixtureVerificationMessage)
	if err != nil {
		return seedTaskView{}, fmt.Errorf("insert approval: %w", err)
	}

	fixed := make([]contract.FixedCandidate, 0, len(targets))
	for _, target := range targets {
		fixed = append(fixed, contract.FixedCandidate{GroupId: contract.QQId(target), SourceRef: "fixture_product_import"})
	}
	mode := contract.ExecutionMode("live")
	request := contract.CreateTaskRequest{
		Type:                "group.acquire",
		Title:               title,
		JoinReason:          fixtureJoinReason,
		VerificationMessage: fixtureVerificationMessage,
		Goal: contract.Goal{
			Metric: "new_unique_groups_for_account_pool", Count: len(targets),
			StrictTargetCap: true, RequireFinalMembershipCheck: true,
		},
		AllowedAccountIds: allowed,
		CandidateSource:   contract.CandidateSource{Kind: "fixed", Groups: fixed},
		ExecutionMode:     &mode,
		DeadlineAt:        time.Now().UTC().Add(2 * time.Hour),
		Limits: contract.TaskLimits{
			MaxCandidates: len(targets), MaxAttemptsPerAction: 3,
			MaxTotalExternalAttempts: 10, MaxModelAttempts: 2,
		},
		ApprovalRef: nullable.NewNullableWithValue(approvalID),
		Submit:      true,
	}
	task, err := store.CreateTask(ctx, actor, "fixture-create-"+suffix, request)
	if err != nil {
		return seedTaskView{}, fmt.Errorf("create task: %w", err)
	}
	for {
		done, err := store.ExpandFixedPage(ctx, actor, string(task.Id), 100)
		if err != nil {
			return seedTaskView{}, fmt.Errorf("expand task: %w", err)
		}
		if done {
			break
		}
	}
	snapshots := make([]taskengine.BaselineSnapshot, 0, len(fixtureAccounts))
	for _, account := range fixtureAccounts {
		snapshots = append(snapshots, taskengine.BaselineSnapshot{
			AccountID: account.ID, EvidenceRef: "fixture_baseline_" + suffix + "_" + account.ID,
			ObservedAt: time.Now().UTC(), Complete: true, GroupIDs: []string{},
		})
	}
	if err := store.CompleteBaseline(ctx, actor, string(task.Id), snapshots); err != nil {
		return seedTaskView{}, fmt.Errorf("complete baseline: %w", err)
	}
	task, err = store.GetTask(ctx, actor, string(task.Id))
	if err != nil {
		return seedTaskView{}, fmt.Errorf("read task before start: %w", err)
	}
	if _, err := store.ControlTask(ctx, actor, string(task.Id), "fixture-start-"+suffix, contract.TaskControlRequest{
		Action: contract.TaskControlRequestActionStart, ExpectedVersion: task.Version, Reason: "product fixture start",
	}); err != nil {
		return seedTaskView{}, fmt.Errorf("start task: %w", err)
	}
	task, err = store.GetTask(ctx, actor, string(task.Id))
	if err != nil {
		return seedTaskView{}, fmt.Errorf("read task before mode switch: %w", err)
	}
	if _, err := store.SetOrchestrationMode(ctx, actor, string(task.Id), "fixture-mode-"+suffix,
		taskengine.OrchestrationModeRequest{
			ExpectedVersion: task.Version, Mode: "proposal", Reason: "product fixture proposal execution",
		}); err != nil {
		return seedTaskView{}, fmt.Errorf("switch task to proposal mode: %w", err)
	}

	view := readSeedTaskView(ctx, store, string(task.Id), approvalID, targets)
	require(view.State == "RUNNING" && view.Mode == "live" && view.OrchestrationMode == "proposal",
		"task %s state=%s mode=%s orchestration=%s, want RUNNING/live/proposal",
		view.ID, view.State, view.Mode, view.OrchestrationMode)
	return view, nil
}

func readSeedTaskView(ctx context.Context, store *taskengine.Store, taskID, approvalID string, targets []string) seedTaskView {
	var view seedTaskView
	var requestPayload []byte
	must(store.Pool.QueryRow(ctx, `
		SELECT id,version,state,mode,orchestration_mode,authorization_revision,COALESCE(approval_id,''),goal_count,request_payload
		FROM core.task WHERE workspace_id=$1 AND id=$2`, workspaceID, taskID).
		Scan(&view.ID, &view.Version, &view.State, &view.Mode, &view.OrchestrationMode,
			&view.AuthorizationRevision, &view.ApprovalRef, &view.GoalCount, &requestPayload), "read seeded task")
	var payload struct {
		Limits struct {
			MaxModelAttempts int `json:"max_model_attempts"`
		} `json:"limits"`
	}
	must(json.Unmarshal(requestPayload, &payload), "decode seeded task request payload")
	view.MaxModelAttempts = payload.Limits.MaxModelAttempts
	view.TargetGroupIDs = append([]string(nil), targets...)
	view.ApprovalScopeDigest = digestOf(map[string]any{
		"workspace_id":   workspaceID,
		"principal_id":   operatorPrincipalID,
		"scope_revision": 1,
		"account_ids":    []string{fixtureAccounts[0].ID, fixtureAccounts[1].ID},
		"target_ids":     targets,
		"max_attempts":   10,
	})
	require(view.ApprovalRef == approvalID, "task %s approval=%s, want %s", taskID, view.ApprovalRef, approvalID)
	require(view.MaxModelAttempts >= 2, "task %s max_model_attempts=%d, want >=2", taskID, view.MaxModelAttempts)
	return view
}
