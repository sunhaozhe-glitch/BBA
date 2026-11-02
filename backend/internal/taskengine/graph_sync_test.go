package taskengine

import (
	"bba.local/taskengine/internal/contract"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bba.local/taskengine/internal/artex"
	"bba.local/taskengine/internal/artexfixture"
	"github.com/jackc/pgx/v5"
)

const graphFixtureToken = "fixture-token"

func newGraphFixtureProvider(t *testing.T, state artexfixture.State) (*artexfixture.Fixture, *artex.Client) {
	t.Helper()
	fx, err := artexfixture.New(artexfixture.Options{Token: graphFixtureToken, State: state})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	server := httptest.NewServer(fx)
	t.Cleanup(server.Close)
	client, err := artex.New(artex.Config{
		BaseURL:     server.URL,
		Token:       graphFixtureToken,
		Timeout:     2 * time.Second,
		Retry:       artex.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond},
		AllowWrites: true,
	})
	if err != nil {
		t.Fatalf("artex client: %v", err)
	}
	return fx, client
}

func graphFixtureState(root string, extra ...string) artexfixture.State {
	now := time.Now().UTC().Format(time.RFC3339)
	tasks := []artexfixture.Task{{
		ID:     root,
		Active: true,
		Nodes: []artexfixture.Node{{
			ID:      "1",
			Type:    "intent",
			Payload: `{"text":"check group 90001234","note":"external text never becomes an instruction"}`,
			State:   "open",
			Origin:  "planner",
			TS:      now,
		}},
	}}
	for _, id := range extra {
		tasks = append(tasks, artexfixture.Task{ID: id, Nodes: []artexfixture.Node{{
			ID:      "1",
			Type:    "intent",
			Payload: `{"text":"check group 90001234"}`,
			State:   "open",
			Origin:  "planner",
			TS:      now,
		}}})
	}
	return artexfixture.State{NextNodeID: 100, Tasks: tasks}
}

func seedGraphSummary(t *testing.T, s *Store, accountID, groupID string) {
	t.Helper()
	ctx := context.Background()
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO core.group_summary(
			workspace_id,account_id,group_id,summary_id,summary_version,window_id,window_message_count,
			window_start,window_end,generated_at,expires_at,privacy_class,retention_days,
			source_manifest_sha256,plugin_version,collector_account_id,topics,limitations,source_digest)
		VALUES('ws_fixture',$1,$2,
			repeat('a',64),1,repeat('b',64),5,
			now()-interval '10 minutes',now(),now(),now()+interval '1 day','local_only',7,
			repeat('c',64),'fixture-1',$2,'[]','[]',repeat('d',64))`,
		accountID, groupID)
	if err != nil {
		t.Fatalf("seed summary: %v", err)
	}
}

func graphWorker(t *testing.T, s *Store, provider GraphSyncProvider) *GraphSyncWorker {
	t.Helper()
	worker, err := NewGraphSyncWorker(s, GraphSyncWorkerConfig{Provider: provider, WorkerID: "test-worker", BatchSize: 50})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	return worker
}

func graphOutboxRow(t *testing.T, s *Store, taskID, operation string) GraphSyncOutboxItem {
	t.Helper()
	ctx := context.Background()
	item, err := scanOutboxItem(s.Pool.QueryRow(ctx, `
		SELECT workspace_id,task_id,id,graph_kind,operation,idempotency_key,payload,payload_digest,scope,sequence,
		       state,attempts,max_attempts,external_node_id,last_error_code,last_error,next_attempt_at,created_at,updated_at
		FROM core.graph_sync_outbox
		WHERE workspace_id='ws_fixture' AND task_id=$1 AND operation=$2
		ORDER BY sequence DESC LIMIT 1`, taskID, operation))
	if err != nil {
		t.Fatalf("outbox %s/%s: %v", taskID, operation, err)
	}
	return item
}

func TestBindGraphRootIsExplicitAndIdempotent(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	actor := fixtureActor()
	ctx := context.Background()

	status, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-key-1", GraphRootRequest{ExternalRoot: "artex_task"})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !status.Graphs[0].Bound || status.Graphs[0].ExternalRoot == nil || *status.Graphs[0].ExternalRoot != "artex_task" {
		t.Fatalf("status = %+v", status.Graphs[0])
	}
	if status.Graphs[1].Bound {
		t.Fatal("goal graph bound without an explicit binding")
	}
	again, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-key-1", GraphRootRequest{ExternalRoot: "artex_task"})
	if err != nil {
		t.Fatalf("replay bind: %v", err)
	}
	if again.Graphs[0].GraphRevision != status.Graphs[0].GraphRevision {
		t.Fatal("replayed bind changed the graph revision")
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.graph_sync_outbox WHERE task_id=$1 AND idempotency_key='bind:artex_task'`, task.Id); got != 1 {
		t.Fatalf("bind outbox rows = %d", got)
	}

	// Rebinding a different root once sync state exists is refused: it would
	// orphan mappings and address pending writes at the wrong external task.
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-key-2", GraphRootRequest{ExternalRoot: "other_task"}); Code(err) != taskCodeStateConflict {
		t.Fatalf("rebind err = %v", err)
	}
	// The same external root cannot back two local tasks.
	other := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001235"}, 1, 2)
	if _, err := s.BindGraphRoot(ctx, actor, other.Id, "fact", "bind-key-3", GraphRootRequest{ExternalRoot: "artex_task"}); Code(err) != taskCodeStateConflict {
		t.Fatalf("shared root err = %v", err)
	}
	// A runtime credential cannot bind a root.
	runtime := actor
	runtime.RuntimeID = "runtime_a"
	if _, err := s.BindGraphRoot(ctx, runtime, task.Id, "fact", "bind-key-4", GraphRootRequest{ExternalRoot: "x"}); Code(err) != taskCodeForbidden {
		t.Fatalf("runtime bind err = %v", err)
	}
	// The active alias is not a stable root.
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "goal", "bind-key-5", GraphRootRequest{ExternalRoot: "active"}); Code(err) != taskCodeValidation {
		t.Fatalf("active alias err = %v", err)
	}
	// A goal binding on a terminal task is refused; a fact (read-only) binding
	// stays available because it creates no external effect.
	dead := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	if _, err := s.Pool.Exec(ctx, `UPDATE core.task SET state='CANCELLED' WHERE id=$1`, dead.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindGraphRoot(ctx, actor, dead.Id, "goal", "bind-dead", GraphRootRequest{ExternalRoot: "dead_task"}); Code(err) != taskCodeStateConflict {
		t.Fatalf("terminal goal bind err = %v", err)
	}
	if _, err := s.BindGraphRoot(ctx, actor, dead.Id, "fact", "bind-dead-fact", GraphRootRequest{ExternalRoot: "dead_task"}); err != nil {
		t.Fatalf("terminal fact bind: %v", err)
	}
}

func TestGraphSyncFactPullStoresInferenceNodesAndSummaryAnchors(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	seedGraphSummary(t, s, "acc_g1", "90001234")
	fx, provider := newGraphFixtureProvider(t, graphFixtureState("artex_task"))
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-fact", GraphRootRequest{ExternalRoot: "artex_task"}); err != nil {
		t.Fatalf("bind fact: %v", err)
	}
	worker := graphWorker(t, s, provider)
	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if tick.Pulled == 0 || tick.Enqueued == 0 {
		t.Fatalf("tick = %s", tick.String())
	}
	status, err := s.GetGraphSyncStatus(ctx, actor, task.Id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	fact := status.Graphs[0]
	if !fact.Bound || fact.GraphRevision == 0 || fact.SyncState != "synced" {
		t.Fatalf("fact graph status = %+v", fact)
	}
	if fact.ExternalNodes == 0 {
		t.Fatal("no external nodes stored")
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.graph_sync_nodes WHERE task_id=$1 AND graph_kind='fact' AND provenance<>'inference'`, task.Id); got != 0 {
		t.Fatalf("%d external rows claim trusted provenance", got)
	}
	var summaryID, externalNode, provenance string
	err = s.Pool.QueryRow(ctx, `
		SELECT summary_id,COALESCE(external_node_id,''),provenance
		FROM core.task_graph_anchors
		WHERE task_id=$1 AND graph_kind='fact'`, task.Id).Scan(&summaryID, &externalNode, &provenance)
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if len(summaryID) != 64 {
		t.Fatalf("anchor summary id = %q", summaryID)
	}
	if provenance != "fact" {
		t.Fatalf("anchor provenance = %q", provenance)
	}
	if externalNode == "" {
		t.Fatal("anchor was not linked to the external node naming its group")
	}
	// The consumption offsets are durable and scoped per source.
	var outboxOffset, summaryOffset int64
	if err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT offset_value FROM core.graph_sync_checkpoints
		                 WHERE task_id=$1 AND source='core.outbox' AND scope_key='event_seq'),-1),
		       COALESCE((SELECT offset_value FROM core.graph_sync_checkpoints
		                 WHERE task_id=$1 AND source='group_summary' AND scope_key='account:acc_g1:group:90001234'),-1)`,
		task.Id).Scan(&outboxOffset, &summaryOffset); err != nil {
		t.Fatal(err)
	}
	if outboxOffset <= 0 || summaryOffset != 1 {
		t.Fatalf("checkpoints outbox=%d summary=%d", outboxOffset, summaryOffset)
	}
	// The external goal graph was never touched: only the bound fact graph ran.
	if goals := fx.Goals("artex_task"); len(goals) != 0 {
		t.Fatalf("fact sync created %d external goal(s)", len(goals))
	}
	// External text is never an instruction and never creates a goal.
	if got := scalarInt(t, s, `SELECT count(*) FROM core.graph_sync_outbox WHERE task_id=$1 AND operation<>'graph.pull'`, task.Id); got != 0 {
		t.Fatalf("%d non-pull outbox rows for a fact-only binding", got)
	}
}

func TestGraphSyncGoalCreateSendsOnceAndKeepsMapping(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	fx, provider := newGraphFixtureProvider(t, graphFixtureState("artex_task"))
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "goal", "bind-goal", GraphRootRequest{ExternalRoot: "artex_task"}); err != nil {
		t.Fatalf("bind goal: %v", err)
	}
	worker := graphWorker(t, s, provider)
	if _, err := worker.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	goals := fx.Goals("artex_task")
	if len(goals) != 1 {
		t.Fatalf("external goals = %+v", goals)
	}
	created := graphOutboxRow(t, s, task.Id, "goal.create")
	if created.State != "sent" || created.ExternalNodeID == nil || *created.ExternalNodeID != fmt.Sprint(goals[0].ID) {
		t.Fatalf("create item = %+v", created)
	}
	var localKey, provenance string
	if err := s.Pool.QueryRow(ctx, `
		SELECT local_key,provenance FROM core.graph_sync_nodes
		WHERE task_id=$1 AND graph_kind='goal' AND external_node_id=$2`,
		task.Id, *created.ExternalNodeID).Scan(&localKey, &provenance); err != nil {
		t.Fatalf("mapping: %v", err)
	}
	if localKey != goalCreateKey(task.Id) || provenance != "inference" {
		t.Fatalf("mapping local_key=%q provenance=%q", localKey, provenance)
	}

	// A restarted worker (different WorkerID) must not resend the same create:
	// dedup is durable in the outbox, not in worker memory.
	restarted, err := NewGraphSyncWorker(s, GraphSyncWorkerConfig{Provider: provider, WorkerID: "restarted-worker"})
	if err != nil {
		t.Fatalf("restarted worker: %v", err)
	}
	if _, err := restarted.Tick(ctx); err != nil {
		t.Fatalf("restart tick: %v", err)
	}
	if got := len(fx.Goals("artex_task")); got != 1 {
		t.Fatalf("duplicate external goals after restart: %d", got)
	}

	// A trusted local change maps to a goal update of the same external goal.
	if _, err := s.Pool.Exec(ctx, `UPDATE core.task SET title='renamed task' WHERE id=$1`, task.Id); err != nil {
		t.Fatal(err)
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		return appendEvent(ctx, tx, "ws_fixture", task.Id, "observation", "fixture", map[string]any{"seen": 1})
	})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("update tick: %v", err)
	}
	if tick.GoalsUpdated == 0 {
		t.Fatalf("update tick = %s", tick.String())
	}
	goals = fx.Goals("artex_task")
	if len(goals) != 1 || !strings.Contains(goals[0].Text, "renamed task") {
		t.Fatalf("external goal was not updated: %+v", goals)
	}
	updated := graphOutboxRow(t, s, task.Id, "goal.update")
	if updated.State != "sent" {
		t.Fatalf("update item = %+v", updated)
	}

	// Cancelling the task deletes exactly the mapped goal.
	err = s.tx(ctx, func(tx pgx.Tx) error {
		return appendEvent(ctx, tx, "ws_fixture", task.Id, "task.control", "fixture", map[string]any{"to": "CANCELLED"})
	})
	if err != nil {
		t.Fatalf("cancel event: %v", err)
	}
	if _, err := worker.Tick(ctx); err != nil {
		t.Fatalf("delete tick: %v", err)
	}
	if goals := fx.Goals("artex_task"); len(goals) != 0 {
		t.Fatalf("external goals after cancel = %+v", goals)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.graph_sync_nodes WHERE task_id=$1 AND graph_kind='goal'`, task.Id); got != 0 {
		t.Fatalf("%d goal mappings survived the delete", got)
	}
}

func TestGraphSyncStaleWriteLeaseNeedsReconcileAndIsNotResent(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	fx, provider := newGraphFixtureProvider(t, graphFixtureState("artex_task"))
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "goal", "bind-goal", GraphRootRequest{ExternalRoot: "artex_task"}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// Simulate a worker that died after claiming the write: the lease expired
	// while the external effect is unknown.
	_, err := s.Pool.Exec(ctx, `
		UPDATE core.graph_sync_outbox
		SET state='sending',lease_owner='dead-worker',lease_until=now()-interval '1 minute',attempts=1
		WHERE task_id=$1 AND operation='goal.create'`, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	worker := graphWorker(t, s, provider)
	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if tick.NeedsReconcile == 0 {
		t.Fatalf("tick = %s", tick.String())
	}
	item := graphOutboxRow(t, s, task.Id, "goal.create")
	if item.State != "needs_reconcile" || item.LastErrorCode == nil || *item.LastErrorCode != graphSyncCodeOutcomeUnknown {
		t.Fatalf("item = %+v", item)
	}
	if goals := fx.Goals("artex_task"); len(goals) != 0 {
		t.Fatalf("ambiguous write was resent: %+v", goals)
	}
	status, err := s.GetGraphSyncStatus(ctx, actor, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if status.Graphs[1].SyncState != "needs_reconcile" || status.Graphs[1].LastError == nil || status.Graphs[1].LastError.Code != graphSyncCodeOutcomeUnknown {
		t.Fatalf("status = %+v", status.Graphs[1])
	}
	// Another tick must not touch it.
	if _, err := worker.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if goals := fx.Goals("artex_task"); len(goals) != 0 {
		t.Fatalf("needs_reconcile item was retried: %+v", goals)
	}

	// Reconcile cannot claim "applied" without the goal actually existing.
	_, err = s.ReconcileGraphSyncItem(ctx, actor, task.Id, item.ID, "reconcile-1",
		GraphSyncReconcileRequest{Outcome: "applied", ExternalNodeID: "77"}, provider)
	if Code(err) != taskCodeStateConflict {
		t.Fatalf("unverified applied reconcile err = %v", err)
	}
	// The readback proves the write never happened.
	reconciled, err := s.ReconcileGraphSyncItem(ctx, actor, task.Id, item.ID, "reconcile-1",
		GraphSyncReconcileRequest{Outcome: "not_applied"}, provider)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reconciled.State != "canceled" {
		t.Fatalf("reconciled state = %s", reconciled.State)
	}
	status, err = s.GetGraphSyncStatus(ctx, actor, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if status.Graphs[1].LastError != nil {
		t.Fatalf("root error survived a successful reconcile: %+v", status.Graphs[1].LastError)
	}
	// Replaying the same reconcile key returns the stored item without a new
	// external read; a different key cannot reconcile a canceled item.
	replay, err := s.ReconcileGraphSyncItem(ctx, actor, task.Id, item.ID, "reconcile-1",
		GraphSyncReconcileRequest{Outcome: "not_applied"}, provider)
	if err != nil || replay.State != "canceled" {
		t.Fatalf("replay = %+v err=%v", replay, err)
	}
	if _, err := s.ReconcileGraphSyncItem(ctx, actor, task.Id, item.ID, "reconcile-2",
		GraphSyncReconcileRequest{Outcome: "not_applied"}, provider); Code(err) != taskCodeStateConflict {
		t.Fatalf("second reconcile err = %v", err)
	}
}

func TestGraphSyncWriteUnknownIsNotResentAndReconcileConfirms(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	fx, provider := newGraphFixtureProvider(t, graphFixtureState("artex_task"))
	// The goal is stored but the reply is unreadable: the effect happened and the
	// caller provably cannot tell, which is exactly the ambiguous write case.
	fx.SetFaults(artexfixture.Faults{GoalsCreateReplyBody: `{"id":"unreadable"`})
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "goal", "bind-goal", GraphRootRequest{ExternalRoot: "artex_task"}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	worker := graphWorker(t, s, provider)
	if _, err := worker.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	item := graphOutboxRow(t, s, task.Id, "goal.create")
	if item.State != "needs_reconcile" || item.LastErrorCode == nil || *item.LastErrorCode != graphSyncCodeOutcomeUnknown {
		t.Fatalf("item = %+v", item)
	}
	goals := fx.Goals("artex_task")
	if len(goals) != 1 {
		t.Fatalf("external goals = %+v", goals)
	}
	externalID := fmt.Sprint(goals[0].ID)
	// A resend would create a second goal; the worker must not do it.
	if _, err := worker.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := len(fx.Goals("artex_task")); got != 1 {
		t.Fatalf("ambiguous write was resent: %d goals", got)
	}
	// Reconcile verifies the claim against the current goal list.
	if _, err := s.ReconcileGraphSyncItem(ctx, actor, task.Id, item.ID, "reconcile-wrong",
		GraphSyncReconcileRequest{Outcome: "applied", ExternalNodeID: "999"}, provider); Code(err) != taskCodeStateConflict {
		t.Fatalf("wrong external id err = %v", err)
	}
	fx.SetFaults(artexfixture.Faults{})
	reconciled, err := s.ReconcileGraphSyncItem(ctx, actor, task.Id, item.ID, "reconcile-applied",
		GraphSyncReconcileRequest{Outcome: "applied", ExternalNodeID: externalID}, provider)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reconciled.State != "sent" || reconciled.ExternalNodeID == nil || *reconciled.ExternalNodeID != externalID {
		t.Fatalf("reconciled = %+v", reconciled)
	}
	var localKey string
	if err := s.Pool.QueryRow(ctx, `
		SELECT local_key FROM core.graph_sync_nodes
		WHERE task_id=$1 AND external_node_id=$2`, task.Id, externalID).Scan(&localKey); err != nil {
		t.Fatalf("mapping after reconcile: %v", err)
	}
	if localKey != goalCreateKey(task.Id) {
		t.Fatalf("local_key = %q", localKey)
	}
}

func graphOutboxRowByKey(t *testing.T, s *Store, taskID, key string) GraphSyncOutboxItem {
	t.Helper()
	item, err := scanOutboxItem(s.Pool.QueryRow(context.Background(), `
		SELECT workspace_id,task_id,id,graph_kind,operation,idempotency_key,payload,payload_digest,scope,sequence,
		       state,attempts,max_attempts,external_node_id,last_error_code,last_error,next_attempt_at,created_at,updated_at
		FROM core.graph_sync_outbox
		WHERE workspace_id='ws_fixture' AND task_id=$1 AND idempotency_key=$2`, taskID, key))
	if err != nil {
		t.Fatalf("outbox %s/%s: %v", taskID, key, err)
	}
	return item
}

func TestGraphSyncReadRetryIsBoundedAndReplayable(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	fx, provider := newGraphFixtureProvider(t, graphFixtureState("artex_task"))
	fx.SetFaults(artexfixture.Faults{GraphStatus: http.StatusServiceUnavailable})
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-fact", GraphRootRequest{ExternalRoot: "artex_task"}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	worker := graphWorker(t, s, provider)
	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if tick.Requeued == 0 {
		t.Fatalf("tick = %s", tick.String())
	}
	item := graphOutboxRowByKey(t, s, task.Id, "bind:artex_task")
	if item.State != "pending" || item.Attempts != 1 {
		t.Fatalf("item = %+v", item)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.graph_sync_nodes WHERE task_id=$1`, task.Id); got != 0 {
		t.Fatalf("failed pull stored %d nodes", got)
	}
	// A read may be replayed, so the retry succeeds once ARTEX recovers.
	fx.SetFaults(artexfixture.Faults{})
	if _, err := s.Pool.Exec(ctx, `UPDATE core.graph_sync_outbox SET next_attempt_at=now() WHERE id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Tick(ctx); err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	item = graphOutboxRow(t, s, task.Id, "graph.pull")
	if item.State != "sent" {
		t.Fatalf("item state = %s", item.State)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.graph_sync_nodes WHERE task_id=$1`, task.Id); got == 0 {
		t.Fatal("recovered pull stored no nodes")
	}
	// Attempts are bounded: an exhausted read becomes failed, not endless.
	if _, err := s.Pool.Exec(ctx, `UPDATE core.graph_sync_outbox SET state='pending',attempts=max_attempts,next_attempt_at=now() WHERE id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	tick, err = worker.Tick(ctx)
	if err != nil {
		t.Fatalf("budget tick: %v", err)
	}
	if tick.Failed == 0 {
		t.Fatalf("tick = %s", tick.String())
	}
	if got := graphOutboxRow(t, s, task.Id, "graph.pull"); got.State != "failed" {
		t.Fatalf("exhausted item state = %s", got.State)
	}
}

func TestGraphSyncPartitionsAreIsolated(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	seedAccount(t, s, "acc_g2", "runtime_g2", 10)
	first := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	second := createReadyTask(t, s, []string{"acc_g2"}, []string{"90001235"}, 1, 2)
	fx, provider := newGraphFixtureProvider(t, graphFixtureState("artex_first", "artex_second"))
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, first.Id, "goal", "bind-first", GraphRootRequest{ExternalRoot: "artex_first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindGraphRoot(ctx, actor, second.Id, "goal", "bind-second", GraphRootRequest{ExternalRoot: "artex_second"}); err != nil {
		t.Fatal(err)
	}
	// A permanently invalid item for the first task must not hold the second
	// task's partition back.
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO core.graph_sync_outbox(
			workspace_id,task_id,graph_kind,provider,id,operation,idempotency_key,payload,payload_digest,scope,sequence,max_attempts)
		VALUES('ws_fixture',$1,'goal','artex','gso_bad','goal.create','bad:create','{"text":""}',repeat('a',64),$2,50,3)`,
		first.Id, "task:"+first.Id+":goal:artex")
	if err != nil {
		t.Fatal(err)
	}
	worker := graphWorker(t, s, provider)
	if _, err := worker.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	bad := graphOutboxRow(t, s, first.Id, "goal.create")
	if bad.State != "failed" || bad.LastErrorCode == nil || *bad.LastErrorCode != graphSyncCodeProtocol {
		t.Fatalf("bad item = %+v", bad)
	}
	if goals := fx.Goals("artex_first"); len(goals) != 1 {
		t.Fatalf("first task goals = %+v", goals)
	}
	if goals := fx.Goals("artex_second"); len(goals) != 1 {
		t.Fatalf("second task was held back: %+v", goals)
	}
}

func TestGraphSyncWorkerRefusesUnconfiguredProvider(t *testing.T) {
	s := fixtureStore(t)
	if _, err := NewGraphSyncWorker(s, GraphSyncWorkerConfig{}); Code(err) != taskCodeValidation {
		t.Fatalf("nil provider constructor err = %v", err)
	}
	unwired, err := artex.New(artex.Config{})
	if err != nil {
		t.Fatalf("unwired client: %v", err)
	}
	worker, err := NewGraphSyncWorker(s, GraphSyncWorkerConfig{Provider: unwired, WorkerID: "unwired-worker"})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if _, err := worker.Tick(context.Background()); Code(err) != graphSyncCodeNotConfigured {
		t.Fatalf("tick err = %v", err)
	}
}

func TestGraphSyncSnapshotBindsBasisAndFeedsValidation(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	seedGraphSummary(t, s, "acc_g1", "90001234")
	fx, provider := newGraphFixtureProvider(t, graphFixtureState("artex_task"))
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-fact", GraphRootRequest{ExternalRoot: "artex_task"}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	worker := graphWorker(t, s, provider)
	if _, err := worker.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	outboxBefore := scalarInt(t, s, `SELECT count(*) FROM core.graph_sync_outbox WHERE task_id=$1`, task.Id)
	goalsBefore := len(fx.Goals("artex_task"))

	snapshot, err := s.CreateGraphPlanningSnapshot(ctx, actor, task.Id, "snap-key-1", provider)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.State != "open" || snapshot.Round != 1 || snapshot.MaxRounds != graphSyncSnapshotRounds {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if len(snapshot.SummaryRefs) != 1 || snapshot.SummaryRefs[0].SummaryVersion != 1 {
		t.Fatalf("summary refs = %+v", snapshot.SummaryRefs)
	}
	if len(snapshot.AnchorIDs) != 1 || len(snapshot.Candidates) != 1 {
		t.Fatalf("anchors=%v candidates=%v", snapshot.AnchorIDs, snapshot.Candidates)
	}
	candidate := snapshot.Candidates[0]
	if candidate.TargetExternalID != "90001234" || candidate.Provenance != "inference" || candidate.ExternalNodeID != "1" {
		t.Fatalf("candidate = %+v", candidate)
	}
	if candidate.Text == "" || candidate.CandidateID == "" {
		t.Fatalf("candidate = %+v", candidate)
	}
	// The S1 handoff maps the external target onto this task's local target.
	mapped, err := s.GraphSnapshotProposalCandidates(ctx, actor, task.Id, snapshot.SnapshotID)
	if err != nil {
		t.Fatalf("map candidates: %v", err)
	}
	var localTarget string
	if err := s.Pool.QueryRow(ctx, `
		SELECT tt.target_id FROM core.task_target tt
		JOIN core.target t ON t.workspace_id=tt.workspace_id AND t.id=tt.target_id
		WHERE tt.task_id=$1 AND t.external_id='90001234'`, task.Id).Scan(&localTarget); err != nil {
		t.Fatal(err)
	}
	if len(mapped) != 1 || mapped[0].TargetID != localTarget || mapped[0].ExternalID != "90001234" {
		t.Fatalf("mapped = %+v (local %s)", mapped, localTarget)
	}
	if _, err := s.GraphSnapshotProposalCandidates(ctx, actor, task.Id, "gsnap_missing"); Code(err) != taskCodeNotFound {
		t.Fatalf("unknown snapshot err = %v", err)
	}
	// Building a snapshot must not dispatch anything: candidates feed S1, never
	// the sender.
	if got := scalarInt(t, s, `SELECT count(*) FROM core.graph_sync_outbox WHERE task_id=$1`, task.Id); got != outboxBefore {
		t.Fatalf("snapshot enqueued %d outbox row(s)", got-outboxBefore)
	}
	if got := len(fx.Goals("artex_task")); got != goalsBefore {
		t.Fatalf("snapshot wrote %d external goal(s)", got-goalsBefore)
	}
	// Replaying the same key returns the same snapshot without spending a round.
	replay, err := s.CreateGraphPlanningSnapshot(ctx, actor, task.Id, "snap-key-1", provider)
	if err != nil || replay.SnapshotID != snapshot.SnapshotID {
		t.Fatalf("replay = %+v err=%v", replay, err)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.graph_planning_snapshots WHERE task_id=$1`, task.Id); got != 1 {
		t.Fatalf("snapshot rows = %d", got)
	}
	// The S1 seam accepts a live snapshot...
	if err := s.ValidateGraphPlanningSnapshot(ctx, actor, task.Id, snapshot.SnapshotID); err != nil {
		t.Fatalf("validate fresh snapshot: %v", err)
	}
	// The operator surface returns the same snapshot, list and detail.
	handler := s.GraphSyncHandler(func(r *http.Request) (Actor, error) { return actor, nil }, provider)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.Id+"/graph-sync/planning-snapshots", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data struct {
			Items []GraphPlanningSnapshot `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(list.Data.Items) != 1 || list.Data.Items[0].SnapshotID != snapshot.SnapshotID {
		t.Fatalf("list = %+v", list.Data.Items)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.Id+"/graph-sync/planning-snapshots/"+snapshot.SnapshotID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", rec.Code, rec.Body.String())
	}
	// ...and makes it stale once an authorization-relevant revision changed.
	if _, err := s.Pool.Exec(ctx, `UPDATE core.task SET authorization_revision=authorization_revision+1 WHERE id=$1`, task.Id); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateGraphPlanningSnapshot(ctx, actor, task.Id, snapshot.SnapshotID); Code(err) != graphSyncCodeSnapshotStale {
		t.Fatalf("stale err = %v", err)
	}
	stored, err := s.GetGraphPlanningSnapshot(ctx, actor, task.Id, snapshot.SnapshotID)
	if err != nil || stored.State != "stale" {
		t.Fatalf("stored = %+v err=%v", stored, err)
	}
}

func TestGraphSnapshotReservesItsMaterializedWorkItem(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_greserve", "runtime_greserve", 10)
	task := createReadyTask(t, s, []string{"acc_greserve"}, []string{"90001234"}, 1, 2)
	seedGraphSummary(t, s, "acc_greserve", "90001234")
	_, provider := newGraphFixtureProvider(t, graphFixtureState("artex_reserve"))
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-fact-reserve", GraphRootRequest{ExternalRoot: "artex_reserve"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "goal", "bind-goal-reserve", GraphRootRequest{ExternalRoot: "artex_reserve"}); err != nil {
		t.Fatal(err)
	}
	if _, err := graphWorker(t, s, provider).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	approvalID := fixtureString(t, s, `SELECT approval_id FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", task.Id)
	mode, err := s.SetOrchestrationMode(ctx, actor, task.Id, "mode-reserve", OrchestrationModeRequest{
		ExpectedVersion: task.Version, Mode: orchestrationModeProposal, Reason: "graph reserve regression",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err = s.GetTask(ctx, actor, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Version != mode.ExpectedVersion+1 {
		t.Fatalf("proposal mode task version=%d, expected %d", task.Version, mode.ExpectedVersion+1)
	}
	snapshot, err := s.CreateGraphPlanningSnapshot(ctx, actor, task.Id, "snapshot-reserve", provider)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := s.GraphSnapshotProposalCandidates(ctx, actor, task.Id, snapshot.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped) != 1 {
		t.Fatalf("mapped candidates=%d, want 1", len(mapped))
	}
	snapshotRef := contract.Id(snapshot.SnapshotID)
	created, err := s.CreatePlanProposalFromSnapshot(ctx, actor, task.Id, "proposal-reserve",
		contract.PlanProposalRequest{TaskId: contract.Id(task.Id), ExpectedVersion: task.Version, ScopeRevision: task.ScopeRevision,
			ProposalKind: contract.PlanProposalRequestProposalKindUseTemplate, Reason: "graph reserve regression", GraphSnapshotId: &snapshotRef},
		snapshot.SnapshotID, mapped)
	if err != nil {
		t.Fatal(err)
	}
	proposalID := string(created.ResourceId)
	if _, err := s.ApprovePlanProposal(ctx, actor, task.Id, proposalID, "approve-reserve",
		PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID}); err != nil {
		t.Fatal(err)
	}
	proposal := fixtureString(t, s, `SELECT digest FROM core.plan_proposal WHERE workspace_id=$1 AND task_id=$2 AND id=$3`, "ws_fixture", task.Id, proposalID)
	materialization, err := s.MaterializePlanProposal(ctx, actor, task.Id, proposalID, "materialize-reserve",
		MaterializeRequest{ExpectedVersion: task.Version, ApprovalRef: approvalID, ProposalDigest: proposal})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateGraphPlanningSnapshot(ctx, actor, task.Id, snapshot.SnapshotID); Code(err) != graphSyncCodeSnapshotStale {
		t.Fatalf("ordinary snapshot validation after materialization err=%v, want stale", err)
	}
	claim, err := s.ClaimReadyWorkItem(ctx, "worker_graph_reserve", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	if claim.MaterializationID != string(materialization.ResourceId) {
		t.Fatalf("claim materialization=%q, want %q", claim.MaterializationID, materialization.ResourceId)
	}
	if _, err := s.ReserveWorkItem(ctx, *claim); err != nil {
		t.Fatalf("reserve materialized graph work item: %v", err)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.work_item WHERE workspace_id=$1 AND id=$2`, "ws_fixture", claim.WorkItemID); state != workItemStateWaitingResult {
		t.Fatalf("work item state=%q, want %q", state, workItemStateWaitingResult)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id); got != 1 {
		t.Fatalf("executions=%d, want exactly 1", got)
	}
}

func TestGraphSyncSnapshotRejectsExpiredAndStaleBasis(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	_, provider := newGraphFixtureProvider(t, graphFixtureState("artex_task"))
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-fact", GraphRootRequest{ExternalRoot: "artex_task"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.CreateGraphPlanningSnapshot(ctx, actor, task.Id, "snap-expire", provider)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `
		UPDATE core.graph_planning_snapshots
		SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 hour'
		WHERE task_id=$1 AND snapshot_id=$2`, task.Id, snapshot.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateGraphPlanningSnapshot(ctx, actor, task.Id, snapshot.SnapshotID); Code(err) != graphSyncCodeSnapshotExpired {
		t.Fatalf("expired err = %v", err)
	}
	if got := snapshotState(t, s, task.Id, snapshot.SnapshotID); got != "expired" {
		t.Fatalf("state = %s", got)
	}

	// A graph revision change between basis read and insert is reported as stale
	// instead of producing a snapshot of a graph that moved.
	fresh, err := s.CreateGraphPlanningSnapshot(ctx, actor, task.Id, "snap-stale", provider)
	if err != nil {
		t.Fatalf("fresh snapshot: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE core.task_graph_roots SET graph_revision=graph_revision+1 WHERE task_id=$1`, task.Id); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateGraphPlanningSnapshot(ctx, actor, task.Id, fresh.SnapshotID); Code(err) != graphSyncCodeSnapshotStale {
		t.Fatalf("stale err = %v", err)
	}
}

func TestGraphSyncSnapshotRoundBudget(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	_, provider := newGraphFixtureProvider(t, graphFixtureState("artex_task"))
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-fact", GraphRootRequest{ExternalRoot: "artex_task"}); err != nil {
		t.Fatal(err)
	}
	for i := range graphSyncSnapshotRounds {
		if _, err := s.CreateGraphPlanningSnapshot(ctx, actor, task.Id, fmt.Sprintf("budget-%d", i), provider); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	if _, err := s.CreateGraphPlanningSnapshot(ctx, actor, task.Id, "budget-over", provider); Code(err) != graphSyncCodePlanningBudget {
		t.Fatalf("over-budget err = %v", err)
	}
}

func TestGraphSyncSnapshotIgnoresOutOfScopeTargets(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	state := graphFixtureState("artex_task")
	state.Tasks[0].Nodes = append(state.Tasks[0].Nodes, artexfixture.Node{
		ID:      "2",
		Type:    "intent",
		Payload: `{"text":"check group 99999999"}`,
		State:   "open",
		Origin:  "planner",
		TS:      time.Now().UTC().Format(time.RFC3339),
	})
	_, provider := newGraphFixtureProvider(t, state)
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-fact", GraphRootRequest{ExternalRoot: "artex_task"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.CreateGraphPlanningSnapshot(ctx, actor, task.Id, "scope-key", provider)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snapshot.Candidates) != 1 || snapshot.Candidates[0].TargetExternalID != "90001234" {
		t.Fatalf("candidates = %+v", snapshot.Candidates)
	}
}

func TestGraphSyncHandlerReportsNotConfiguredExplicitly(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	actor := fixtureActor()
	ctx := context.Background()
	if _, err := s.BindGraphRoot(ctx, actor, task.Id, "fact", "bind-fact", GraphRootRequest{ExternalRoot: "artex_task"}); err != nil {
		t.Fatal(err)
	}
	handler := s.GraphSyncHandler(func(r *http.Request) (Actor, error) { return actor, nil }, nil)
	path := "/v1/tasks/" + task.Id + "/graph-sync"

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var status struct {
		Data struct {
			Provider GraphSyncProviderStatus `json:"provider"`
			Graphs   []GraphSyncGraphStatus  `json:"graphs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.Data.Provider.Configured || status.Data.Provider.Wired {
		t.Fatalf("provider = %+v", status.Data.Provider)
	}
	for _, graph := range status.Data.Graphs {
		if graph.SyncState != "not_configured" {
			t.Fatalf("graph %s sync_state = %s", graph.GraphKind, graph.SyncState)
		}
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path+"/roots/fact", jsonBody(`{"external_root":"artex_task"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "http-bind-1")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bind status = %d body=%s", rec.Code, rec.Body.String())
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil || failure.Error.Code != graphSyncCodeNotConfigured {
		t.Fatalf("error = %+v decode=%v", failure, err)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path+"/planning-snapshots", jsonBody(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("snapshot without provider = %d", rec.Code)
	}
}

func TestGraphSyncHTTPRequiresOperatorCredential(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_g1", "runtime_g1", 10)
	task := createReadyTask(t, s, []string{"acc_g1"}, []string{"90001234"}, 1, 2)
	handler := s.GraphSyncHandler(func(r *http.Request) (Actor, error) {
		actor := fixtureActor()
		actor.RuntimeID = "runtime_a"
		return actor, nil
	}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.Id+"/graph-sync", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("runtime credential status = %d", rec.Code)
	}
}

func snapshotState(t *testing.T, s *Store, taskID, snapshotID string) string {
	t.Helper()
	var state string
	if err := s.Pool.QueryRow(context.Background(), `
		SELECT state FROM core.graph_planning_snapshots
		WHERE workspace_id='ws_fixture' AND task_id=$1 AND snapshot_id=$2`, taskID, snapshotID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func jsonBody(body string) io.Reader { return strings.NewReader(body) }

func TestGraphSnapshotValidatorSeamIsRegistered(t *testing.T) {
	if graphSnapshotValidator == nil {
		t.Fatal("S2 snapshot validator is not registered; S1 would fail closed on every snapshot-backed proposal")
	}
}

func TestGraphSyncWorkerConfigBounds(t *testing.T) {
	s := fixtureStore(t)
	_, provider := newGraphFixtureProvider(t, graphFixtureState("artex_task"))
	cases := []GraphSyncWorkerConfig{
		{Provider: provider, WorkerID: "w", BatchSize: graphSyncMaxBatch + 1},
		{Provider: provider, WorkerID: "w", BatchSize: -1},
		{Provider: provider, WorkerID: "w", PollInterval: 10 * time.Millisecond},
		{Provider: provider, WorkerID: "w", TickTimeout: 10 * time.Millisecond},
		{Provider: provider, WorkerID: strings.Repeat("w", 97)},
	}
	for i, cfg := range cases {
		if _, err := NewGraphSyncWorker(s, cfg); Code(err) != taskCodeValidation {
			t.Fatalf("case %d err = %v", i, err)
		}
	}
	worker, err := NewGraphSyncWorker(s, GraphSyncWorkerConfig{Provider: provider})
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if worker.cfg.BatchSize != graphSyncDefaultBatch || worker.cfg.WorkerID == "" {
		t.Fatalf("defaults = %+v", worker.cfg)
	}
}
