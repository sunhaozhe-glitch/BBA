package main

// Graph chain scenario. It uses the real graph sync worker and the real ARTEX
// adapter against the in-process protocol fixture. A Summary.v1 for the task's
// own eligible target is ingested through the same public Store path the
// collector publish endpoint uses, so the snapshot anchors are real rows and
// candidates must cite them. Both fact and goal roots are bound: the first tick
// pulls facts and creates the external goal, the proposal chain closes with a
// trusted observation, and the second tick advances the goal text and the
// outbox checkpoint.
//
// The ARTEX fixture is a protocol boundary only: no live ARTEX and no fabricated
// local graph. Writes are confined to loopback; the bearer token is generated per
// run and never written to evidence.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"bba.local/taskengine/internal/artex"
	"bba.local/taskengine/internal/artexfixture"
	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
)

type graphScenarioResult struct {
	Status                string  `json:"status"`
	Reason                string  `json:"reason,omitempty"`
	TaskID                string  `json:"task_id,omitempty"`
	ExternalRoot          string  `json:"external_root,omitempty"`
	Provider              string  `json:"provider,omitempty"`
	SummaryID             string  `json:"summary_id,omitempty"`
	SummaryWindowID       string  `json:"summary_window_id,omitempty"`
	SummaryVersion        int     `json:"summary_version,omitempty"`
	SummarySource         string  `json:"summary_source,omitempty"`
	SummaryIngested       bool    `json:"summary_ingested"`
	SummaryManifest       string  `json:"summary_manifest,omitempty"`
	SummaryCheckpoint     int64   `json:"summary_checkpoint,omitempty"`
	GraphRevision         int64   `json:"graph_revision,omitempty"`
	ExternalNodes         int64   `json:"external_nodes,omitempty"`
	Anchors               int64   `json:"anchors,omitempty"`
	EvidenceRefs          int     `json:"evidence_refs,omitempty"`
	CheckpointOffsetStart int64   `json:"checkpoint_offset_start,omitempty"`
	SnapshotID            string  `json:"snapshot_id,omitempty"`
	SnapshotDigest        string  `json:"snapshot_digest,omitempty"`
	Candidates            int     `json:"candidates,omitempty"`
	GoalsCreated          int     `json:"goals_created,omitempty"`
	GoalsUpdated          int     `json:"goals_updated,omitempty"`
	ProposalID            string  `json:"proposal_id,omitempty"`
	ProposalDigest        string  `json:"proposal_digest,omitempty"`
	MaterializationID     string  `json:"materialization_id,omitempty"`
	WorkItemID            string  `json:"work_item_id,omitempty"`
	ExecutionID           string  `json:"execution_id,omitempty"`
	OperationID           string  `json:"operation_id,omitempty"`
	ObservationEventID    string  `json:"observation_event_id,omitempty"`
	WorkState             string  `json:"work_state,omitempty"`
	TaskState             string  `json:"task_state,omitempty"`
	CheckpointOffsetEnd   int64   `json:"checkpoint_offset_end,omitempty"`
	ExternalGoals         int     `json:"external_goals"`
	Checks                []check `json:"checks,omitempty"`
}

func runGraphScenario(ctx context.Context, store *taskengine.Store, actor taskengine.Actor, requireCollectorSummary bool) (*graphScenarioResult, error) {
	out := &graphScenarioResult{Status: "passed", Checks: []check{}}
	taskID := scalarString(ctx, store, `SELECT id FROM core.task WHERE workspace_id=$1 AND approval_id=$2`,
		workspaceID, graphApprovalID)
	task, err := store.GetTask(ctx, actor, taskID)
	must(err, "read graph task")
	require(task.State == contract.TaskState("RUNNING"), "graph task state=%s, want RUNNING", task.State)
	require(string(task.OrchestrationMode) == "proposal", "graph task orchestration=%s, want proposal", task.OrchestrationMode)
	out.TaskID = taskID
	originalTitle := task.Title

	targetExternal := scalarString(ctx, store, `
		SELECT t.external_id FROM core.task_target tt
		JOIN core.target t ON (t.workspace_id,t.id)=(tt.workspace_id,tt.target_id)
		WHERE tt.workspace_id=$1 AND tt.task_id=$2 AND tt.eligibility='eligible'
		ORDER BY t.external_id LIMIT 1`, workspaceID, taskID)
	require(targetExternal != "", "graph task %s has no eligible target", taskID)

	// A real cross-module run publishes the collector Summary before the
	// scenario. Reuse that live row when present; only a standalone fixture run
	// creates its own collector-shaped Summary. -require-collector-summary makes
	// the missing real row a hard failure.
	existingSummary, foundSummary, err := liveGraphSummary(ctx, store, "cross-a", targetExternal)
	must(err, "read live collector Summary")
	var summaryVersion int
	if foundSummary {
		require(len(existingSummary.ID) == 64, "collector Summary id length=%d, want 64", len(existingSummary.ID))
		_, idErr := hex.DecodeString(existingSummary.ID)
		require(idErr == nil, "collector Summary id is not hex: %v", idErr)
		require(len(existingSummary.WindowID) == 64, "collector Summary %s window length=%d, want 64",
			existingSummary.ID, len(existingSummary.WindowID))
		_, windowErr := hex.DecodeString(existingSummary.WindowID)
		require(windowErr == nil, "collector Summary %s window is not hex: %v", existingSummary.ID, windowErr)
		require(existingSummary.Version > 0, "collector Summary %s version=%d, want >0",
			existingSummary.ID, existingSummary.Version)
		require(len(existingSummary.Manifest) == 64, "collector Summary %s manifest length=%d, want 64",
			existingSummary.ID, len(existingSummary.Manifest))
		_, manifestErr := hex.DecodeString(existingSummary.Manifest)
		require(manifestErr == nil, "collector Summary %s manifest is not hex: %v", existingSummary.ID, manifestErr)
		out.SummaryID = existingSummary.ID
		out.SummaryWindowID = existingSummary.WindowID
		out.SummaryVersion = existingSummary.Version
		out.SummaryManifest = existingSummary.Manifest
		out.SummarySource = "collector"
		summaryVersion = existingSummary.Version
		out.Checks = append(out.Checks, check{Name: "reused the live collector Summary for the task's own target", OK: true})
	} else if requireCollectorSummary {
		return nil, fmt.Errorf("no live collector Summary for cross-a/%s; run the cross-module collector first with BBA_CROSS_GRAPH_GROUP=%s",
			targetExternal, targetExternal)
	}

	var tokenBytes [16]byte
	_, err = rand.Read(tokenBytes[:])
	must(err, "generate fixture token")
	token := hex.EncodeToString(tokenBytes[:])
	root := "artex_product_fixture"
	fixture, err := artexfixture.New(artexfixture.Options{
		Token: token,
		State: artexfixture.State{
			NextNodeID: 100,
			Tasks: []artexfixture.Task{{
				ID: root, Active: true,
				Nodes: []artexfixture.Node{{
					ID: "1", Type: "intent", State: "open", Origin: "planner",
					TS:      time.Now().UTC().Format(time.RFC3339),
					Payload: fmt.Sprintf(`{"text":"fixture graph candidate for group %s"}`, targetExternal),
				}},
			}},
		},
	})
	must(err, "build ARTEX protocol fixture")
	server := httptest.NewServer(fixture)
	defer server.Close()
	provider, err := artex.New(artex.Config{
		BaseURL: server.URL, Token: token, Timeout: 2 * time.Second,
		Retry: artex.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond}, AllowWrites: true,
	})
	must(err, "build ARTEX client")
	require(provider.Wired(), "ARTEX client is not wired against the fixture")

	// Bind the fact root before the Summary anchor exists, then ingest one real
	// Summary.v1 for this task's own account/target through the public path.
	rootStatus, err := store.BindGraphRoot(ctx, actor, taskID, "fact", "fixture-graph-bind",
		taskengine.GraphRootRequest{ExternalRoot: root})
	must(err, "bind fact graph root")
	require(graphBound(rootStatus, "fact"), "fact graph root is not bound")

	if !foundSummary {
		summaryRequest, err := graphSummaryRequest(ctx, store, targetExternal)
		must(err, "build Summary.v1 for graph target")
		summary, err := store.IngestGroupSummary(ctx, taskengine.RuntimeActor{
			WorkspaceID: workspaceID, RuntimeID: "cross-runtime-a", OwnerEpoch: 1, AccountIDs: []string{"cross-a"},
		}, summaryRequest)
		must(err, "ingest Summary.v1 for graph target")
		require(summary.Created && summary.SummaryVersion == 1,
			"Summary.v1 created=%v version=%d, want true/1", summary.Created, summary.SummaryVersion)
		out.SummaryID = summary.SummaryID
		out.SummaryWindowID = summary.WindowID
		out.SummaryVersion = summary.SummaryVersion
		out.SummaryManifest = summaryRequest.Summary.Producer.SourceManifestSHA256
		out.SummarySource = "fixture"
		out.SummaryIngested = true
		summaryVersion = summary.SummaryVersion
		out.Checks = append(out.Checks, check{Name: "real Summary.v1 ingest produced version 1 for the task's own target", OK: true})
	}

	goalStatus, err := store.BindGraphRoot(ctx, actor, taskID, "goal", "fixture-goal-bind",
		taskengine.GraphRootRequest{ExternalRoot: root})
	must(err, "bind goal graph root")
	require(graphBound(goalStatus, "goal"), "goal graph root is not bound")
	out.ExternalRoot = root

	graphWorker, err := taskengine.NewGraphSyncWorker(store, taskengine.GraphSyncWorkerConfig{
		WorkerID: "product-fixture-graph", Provider: provider, BatchSize: 10,
	})
	must(err, "new graph sync worker")
	graphTick, err := graphWorker.Tick(ctx)
	must(err, "graph sync tick")
	require(graphTick.Pulled >= 2, "graph sync pulled=%d, want fact + summary reads", graphTick.Pulled)
	require(graphTick.GoalsCreated >= 1, "graph sync goals_created=%d, want >=1", graphTick.GoalsCreated)
	out.GoalsCreated = graphTick.GoalsCreated
	require(graphTick.GoalsDeleted == 0, "graph sync deleted=%d goals on first tick", graphTick.GoalsDeleted)

	status, err := store.GetGraphSyncStatus(ctx, actor, taskID)
	must(err, "read graph sync status")
	fact := graphStatusFor(status, "fact")
	goal := graphStatusFor(status, "goal")
	require(fact != nil && fact.Bound, "fact graph status is not bound")
	require(goal != nil && goal.Bound, "goal graph status is not bound")
	require(fact.SyncState == "synced", "fact graph sync_state=%q, want synced", fact.SyncState)
	require(fact.GraphRevision >= 1, "fact graph revision=%d, want >=1", fact.GraphRevision)
	require(fact.ExternalNodes > 0, "fact graph external_nodes=0")
	require(fact.Anchors >= 1, "fact graph anchors=%d, want >=1 after Summary ingest", fact.Anchors)
	require(fact.PendingOutbox == 0 && fact.NeedsReconcileOutbox == 0 && fact.FailedOutbox == 0,
		"fact graph outbox pending=%d reconcile=%d failed=%d", fact.PendingOutbox, fact.NeedsReconcileOutbox, fact.FailedOutbox)
	require(goal.PendingOutbox == 0 && goal.NeedsReconcileOutbox == 0 && goal.FailedOutbox == 0,
		"goal graph outbox pending=%d reconcile=%d failed=%d", goal.PendingOutbox, goal.NeedsReconcileOutbox, goal.FailedOutbox)
	require(status.Provider.Provider == "artex", "graph provider=%q, want artex", status.Provider.Provider)
	require(len(fact.Checkpoints) > 0, "fact graph has no checkpoint")
	require(len(fixture.Goals(root)) == 1, "fixture external goals=%d after create, want 1", len(fixture.Goals(root)))
	out.Provider = status.Provider.Provider
	out.GraphRevision = fact.GraphRevision
	out.ExternalNodes = fact.ExternalNodes
	out.Anchors = fact.Anchors
	out.CheckpointOffsetStart = graphCheckpointOffset(ctx, store, taskID)
	require(out.CheckpointOffsetStart > 0, "graph outbox checkpoint offset=%d, want >0", out.CheckpointOffsetStart)
	out.SummaryCheckpoint = graphSummaryCheckpointOffset(ctx, store, taskID, "cross-a", targetExternal)
	require(out.SummaryCheckpoint == int64(summaryVersion),
		"summary checkpoint=%d, want version %d", out.SummaryCheckpoint, summaryVersion)
	out.Checks = append(out.Checks, check{Name: "dual roots synced: real fact pull plus Summary anchor and external goal create", OK: true})

	snapshot, err := store.CreateGraphPlanningSnapshot(ctx, actor, taskID, "fixture-graph-snapshot", provider)
	must(err, "create graph planning snapshot")
	require(snapshot.State == "open", "snapshot state=%q, want open", snapshot.State)
	require(snapshot.Digest != "", "snapshot digest is empty")
	require(len(snapshot.AnchorIDs) >= 1, "snapshot anchors=%d, want >=1", len(snapshot.AnchorIDs))
	require(len(snapshot.Candidates) >= 1, "graph snapshot has no candidate")
	anchorSet := map[string]bool{}
	for _, anchor := range snapshot.AnchorIDs {
		anchorSet[anchor] = true
	}
	evidenceFound := 0
	for _, candidate := range snapshot.Candidates {
		if candidate.TargetExternalID != targetExternal {
			continue
		}
		require(candidate.Text != "", "snapshot candidate for target %s has empty text", targetExternal)
		require(len(candidate.EvidenceRefs) > 0, "snapshot candidate for target %s has no evidence", targetExternal)
		for _, ref := range candidate.EvidenceRefs {
			require(anchorSet[ref], "candidate evidence %q is not in snapshot anchors", ref)
			evidenceFound++
		}
	}
	require(evidenceFound > 0, "no snapshot candidate cited the Summary anchor for target %s", targetExternal)
	out.SnapshotID = snapshot.SnapshotID
	out.SnapshotDigest = snapshot.Digest
	out.Candidates = len(snapshot.Candidates)
	out.EvidenceRefs = evidenceFound
	out.Checks = append(out.Checks, check{Name: "bounded snapshot cites the real Summary anchor for its in-scope target", OK: true})

	mapped, err := store.GraphSnapshotProposalCandidates(ctx, actor, taskID, snapshot.SnapshotID)
	must(err, "map snapshot candidates to local targets")
	require(len(mapped) > 0, "snapshot candidates mapped to zero local targets")
	require(mappedTarget(mapped, targetExternal), "snapshot did not map target %s", targetExternal)
	task, err = store.GetTask(ctx, actor, taskID)
	must(err, "read graph task before proposal")
	snapshotRef := contract.Id(snapshot.SnapshotID)
	proposalRequest := contract.PlanProposalRequest{
		TaskId:          contract.Id(taskID),
		ExpectedVersion: task.Version,
		ScopeRevision:   task.ScopeRevision,
		ProposalKind:    contract.PlanProposalRequestProposalKindUseTemplate,
		Reason:          "product fixture graph chain",
		GraphSnapshotId: &snapshotRef,
	}
	created, err := store.CreatePlanProposalFromSnapshot(ctx, actor, taskID, "fixture-graph-proposal",
		proposalRequest, snapshot.SnapshotID, mapped)
	must(err, "create proposal from graph snapshot")
	proposalID := string(created.ResourceId)
	out.ProposalID = proposalID
	_, err = store.ApprovePlanProposal(ctx, actor, taskID, proposalID, "fixture-graph-approve",
		taskengine.PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: graphApprovalID})
	must(err, "approve graph proposal")
	proposal, err := store.GetPlanProposal(ctx, actor, taskID, proposalID)
	must(err, "read graph proposal")
	digest := string(proposal.Digest)
	out.ProposalDigest = digest
	materialization, err := store.MaterializePlanProposal(ctx, actor, taskID, proposalID, "fixture-graph-materialize",
		taskengine.MaterializeRequest{ExpectedVersion: task.Version, ApprovalRef: graphApprovalID, ProposalDigest: digest})
	must(err, "materialize graph proposal")
	out.MaterializationID = string(materialization.ResourceId)
	out.Checks = append(out.Checks, check{Name: "snapshot proposal was approved and materialized into durable work", OK: true})

	adapter := &countingAdapter{outcome: taskengine.SendOutcome{
		Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fixture_graph_receipt",
	}}
	proposalWorker, err := taskengine.NewWorker(store, taskengine.WorkerConfig{
		WorkerID: "product-fixture-graph-proposal", BatchSize: 10,
		Steps: taskengine.WorkerSteps{Proposal: true}, ProposalExecution: true,
		Transport: taskengine.WorkerExternalTransport, AllowExternalWrites: true, Adapter: adapter,
	})
	must(err, "new graph proposal worker")
	tick, err := proposalWorker.Tick(ctx)
	must(err, "graph proposal worker tick")
	if tick.ProposalDispatched != 1 || adapter.count() != 1 {
		chain := loadWorkChain(ctx, store, taskID, false)
		lockHeld := scalarInt(ctx, store, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND account_id='cross-a' AND held`, workspaceID)
		task, taskErr := store.GetTask(ctx, actor, taskID)
		must(taskErr, "read graph task after failed dispatch")
		require(false, "graph proposal claimed=%d reserved=%d dispatched=%d adapter_calls=%d work_state=%q account=%q cross_a_lock=%d task_state=%q",
			tick.ProposalClaimed, tick.ProposalReserved, tick.ProposalDispatched, adapter.count(), chain.State, chain.AccountID, lockHeld, task.State)
	}

	chain := loadWorkChain(ctx, store, taskID, false)
	require(chain.State == "WAITING_RESULT" && chain.ExecutionID != "" && chain.OperationID != "",
		"graph work chain state=%q execution=%q operation=%q", chain.State, chain.ExecutionID, chain.OperationID)
	out.WorkItemID = chain.WorkItemID
	out.ExecutionID = chain.ExecutionID
	out.OperationID = chain.OperationID

	runtimeID := scalarString(ctx, store, `SELECT runtime_id FROM core.account WHERE workspace_id=$1 AND id=$2`,
		workspaceID, chain.AccountID)
	observation := taskengine.Observation{
		EventID:          "evt_" + chain.ExecutionID,
		OperationID:      chain.OperationID,
		AccountID:        chain.AccountID,
		TargetID:         chain.TargetID,
		Kind:             "membership.observed",
		State:            "joined",
		EvidenceRef:      "evidence_" + chain.ExecutionID,
		SourceEpoch:      1,
		SourceSequence:   nextObservationSequence(ctx, store, runtimeID, chain.AccountID),
		ObservedAt:       time.Now().UTC().Add(2 * time.Second),
		SnapshotComplete: true,
	}
	runtimeActor := taskengine.RuntimeActor{
		WorkspaceID: workspaceID, RuntimeID: runtimeID, OwnerEpoch: 1, AccountIDs: []string{chain.AccountID},
	}
	must(store.ReceiveObservation(ctx, runtimeActor, observation), "receive graph trusted observation")
	must(store.ApplyObservation(ctx, workspaceID, runtimeID, observation.EventID), "apply graph trusted observation")
	out.ObservationEventID = observation.EventID

	applied := loadWorkChain(ctx, store, taskID, false)
	out.WorkState = applied.State
	require(applied.State == "SUCCEEDED" && applied.AttemptOutcome == "SUCCEEDED",
		"graph work state=%q attempt=%q, want SUCCEEDED", applied.State, applied.AttemptOutcome)
	require(applied.ObservationID == observation.EventID, "graph attempt observation=%q, want %q",
		applied.ObservationID, observation.EventID)
	out.Checks = append(out.Checks, check{Name: "trusted observation closed the exact graph work/attempt/execution chain", OK: true})

	// The trusted local result makes the progress text differ from the create
	// text, so the next tick must enqueue and dispatch one goal.update behind the
	// existing goal.create in the same partition.
	secondTick, err := graphWorker.Tick(ctx)
	must(err, "graph sync update tick")
	require(secondTick.GoalsUpdated >= 1, "graph sync goals_updated=%d, want >=1", secondTick.GoalsUpdated)
	require(secondTick.GoalsDeleted == 0, "graph sync deleted=%d goals", secondTick.GoalsDeleted)
	out.GoalsUpdated = secondTick.GoalsUpdated
	out.CheckpointOffsetEnd = graphCheckpointOffset(ctx, store, taskID)
	require(out.CheckpointOffsetEnd > out.CheckpointOffsetStart,
		"graph checkpoint did not advance: %d -> %d", out.CheckpointOffsetStart, out.CheckpointOffsetEnd)

	status, err = store.GetGraphSyncStatus(ctx, actor, taskID)
	must(err, "read graph sync status after update")
	fact = graphStatusFor(status, "fact")
	goal = graphStatusFor(status, "goal")
	require(fact != nil && goal != nil, "graph status lost a bound graph")
	require(fact.PendingOutbox == 0 && goal.PendingOutbox == 0 &&
		fact.NeedsReconcileOutbox == 0 && goal.NeedsReconcileOutbox == 0 &&
		fact.FailedOutbox == 0 && goal.FailedOutbox == 0,
		"graph outbox not settled: fact pending=%d reconcile=%d failed=%d goal pending=%d reconcile=%d failed=%d",
		fact.PendingOutbox, fact.NeedsReconcileOutbox, fact.FailedOutbox,
		goal.PendingOutbox, goal.NeedsReconcileOutbox, goal.FailedOutbox)

	goalCreate, goalUpdate := graphGoalOutboxCounts(ctx, store, taskID)
	require(goalCreate == 1, "graph outbox goal.create=%d, want 1", goalCreate)
	require(goalUpdate >= 1, "graph outbox goal.update=%d, want >=1", goalUpdate)
	require(graphGoalOutboxUnsent(ctx, store, taskID) == 0, "graph outbox has unsent goal operations")

	goals := fixture.Goals(root)
	require(len(goals) == 1, "fixture external goals=%d, want 1", len(goals))
	require(goals[0].Text != originalTitle, "external goal text still equals the create text %q", originalTitle)
	require(strings.Contains(goals[0].Text, "已确认"), "external goal text=%q does not contain progress", goals[0].Text)
	out.ExternalGoals = len(goals)
	out.TaskState = scalarString(ctx, store, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, workspaceID, taskID)
	out.Checks = append(out.Checks, check{Name: "trusted result produced one goal.update after the create, all sent, checkpoint advanced", OK: true})
	return out, nil
}

// graphSummaryRequest builds one collector-shaped Summary.v1 for the account's
// own QQ id and the task's own eligible target group. The content is fixture
// data, but it passes the real ingest validation and storage path, including the
// live expiry and target/account scope checks.
type graphSummaryRef struct {
	ID       string
	WindowID string
	Version  int
	Manifest string
}

// liveGraphSummary reads the newest live Summary for the task's own account and
// target. It is evidence of the real collector row; the scenario never mutates or
// repairs it.
func liveGraphSummary(ctx context.Context, store *taskengine.Store, accountID, groupID string) (graphSummaryRef, bool, error) {
	var ref graphSummaryRef
	err := store.Pool.QueryRow(ctx, `
		SELECT summary_id,window_id,summary_version,source_manifest_sha256
		FROM core.group_summary
		WHERE workspace_id=$1 AND account_id=$2 AND group_id=$3
		  AND expires_at>clock_timestamp()
		ORDER BY summary_version DESC LIMIT 1`, workspaceID, accountID, groupID).
		Scan(&ref.ID, &ref.WindowID, &ref.Version, &ref.Manifest)
	if errors.Is(err, pgx.ErrNoRows) {
		return graphSummaryRef{}, false, nil
	}
	if err != nil {
		return graphSummaryRef{}, false, err
	}
	return ref, true, nil
}

func graphSummaryRequest(ctx context.Context, store *taskengine.Store, targetExternal string) (taskengine.GroupSummaryIngestRequest, error) {
	selfID := scalarString(ctx, store, `SELECT self_id FROM core.account WHERE workspace_id=$1 AND id=$2`,
		workspaceID, "cross-a")
	if selfID == "" {
		return taskengine.GroupSummaryIngestRequest{}, fmt.Errorf("account cross-a has no self_id")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := taskengine.SummaryScope{
		AccountID: selfID, ConversationKind: "group", ConversationID: targetExternal,
	}
	request := taskengine.GroupSummaryIngestRequest{
		AccountID:          "cross-a",
		SummaryID:          randomHex(),
		WindowID:           randomHex(),
		WindowMessageCount: 3,
		GeneratedAt:        now,
		TopicEvidence:      []string{"deterministic_statistic"},
		Summary: taskengine.SummaryPayload{
			SchemaID:       "Summary.v1",
			WindowStart:    now.Add(-30 * time.Minute),
			WindowEnd:      now.Add(-time.Minute),
			CollectorScope: scope,
			PrivacyClass:   "local_only",
			RetentionDays:  7,
			ExpiresAt:      now.Add(24 * time.Hour),
			Producer: taskengine.SummaryProducer{
				AccountID:            selfID,
				CollectorScope:       scope,
				PluginVersion:        "fixture-graph-summary",
				SourceManifestSHA256: randomHex(),
			},
			Topics: []taskengine.SummaryTopicPayload{{
				Summary: "fixture deterministic statistic for the graph target",
				MessageRefs: []string{
					"msg_fixture_graph_1",
				},
			}},
			Limitations: []string{"fixture deterministic summary; no model inference"},
		},
	}
	return request, nil
}

func randomHex() string {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}

func graphBound(status taskengine.GraphSyncStatus, kind string) bool {
	for _, graph := range status.Graphs {
		if graph.GraphKind == kind {
			return graph.Bound
		}
	}
	return false
}

func graphStatusFor(status taskengine.GraphSyncStatus, kind string) *taskengine.GraphSyncGraphStatus {
	for index := range status.Graphs {
		if status.Graphs[index].GraphKind == kind {
			return &status.Graphs[index]
		}
	}
	return nil
}

func mappedTarget(candidates []taskengine.GraphProposalCandidate, externalID string) bool {
	for _, candidate := range candidates {
		if candidate.ExternalID == externalID || candidate.TargetID == externalID {
			return true
		}
	}
	return false
}

func graphGoalOutboxCounts(ctx context.Context, store *taskengine.Store, taskID string) (int, int) {
	var created, updated int
	must(store.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE operation='goal.create'),
		       count(*) FILTER (WHERE operation='goal.update')
		FROM core.graph_sync_outbox
		WHERE workspace_id=$1 AND task_id=$2`, workspaceID, taskID).Scan(&created, &updated),
		"count graph goal outbox")
	return created, updated
}

func graphGoalOutboxUnsent(ctx context.Context, store *taskengine.Store, taskID string) int {
	return scalarInt(ctx, store, `SELECT count(*) FROM core.graph_sync_outbox
		WHERE workspace_id=$1 AND task_id=$2 AND operation IN ('goal.create','goal.update') AND state<>'sent'`,
		workspaceID, taskID)
}

func graphSummaryCheckpointOffset(ctx context.Context, store *taskengine.Store, taskID, accountID, groupID string) int64 {
	var offset int64
	must(store.Pool.QueryRow(ctx, `
		SELECT offset_value FROM core.graph_sync_checkpoints
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind='fact' AND provider='artex'
		  AND source='group_summary' AND scope_key=$3`, workspaceID, taskID, "account:"+accountID+":group:"+groupID).Scan(&offset),
		"read summary checkpoint")
	return offset
}

func graphCheckpointOffset(ctx context.Context, store *taskengine.Store, taskID string) int64 {
	var offset int64
	must(store.Pool.QueryRow(ctx, `
		SELECT offset_value FROM core.graph_sync_checkpoints
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind='fact' AND provider='artex'
		  AND source='core.outbox' AND scope_key='event_seq'`, workspaceID, taskID).Scan(&offset),
		"read graph checkpoint")
	return offset
}
