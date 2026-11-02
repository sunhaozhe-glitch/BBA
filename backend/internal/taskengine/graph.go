package taskengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

type TaskGraph struct {
	WorkspaceID    string            `json:"workspace_id"`
	TaskID         string            `json:"task_id"`
	RootID         string            `json:"root_id"`
	Revision       int64             `json:"revision"`
	TaskVersion    int               `json:"task_version"`
	ScopeRevision  int               `json:"scope_revision"`
	GoalCount      int               `json:"goal_count"`
	TaskState      string            `json:"task_state"`
	SnapshotDigest string            `json:"snapshot_digest"`
	ProjectedAt    time.Time         `json:"projected_at"`
	Anchors        []TaskGraphAnchor `json:"anchors"`
	Nodes          []TaskGraphNode   `json:"nodes"`
	Facts          []TaskGraphFact   `json:"facts"`
	Edges          []TaskGraphEdge   `json:"edges"`
}
type TaskGraphEdge struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type TaskGraphAnchor struct {
	ID              string  `json:"id"`
	Kind            string  `json:"kind"`
	AccountID       *string `json:"account_id,omitempty"`
	TargetID        *string `json:"target_id,omitempty"`
	SourceRef       *string `json:"source_ref,omitempty"`
	Eligibility     *string `json:"eligibility,omitempty"`
	BaselineCovered *bool   `json:"baseline_covered,omitempty"`
}

type TaskGraphNode struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	AnchorID *string        `json:"anchor_id,omitempty"`
	State    string         `json:"state"`
	Data     map[string]any `json:"data"`
}

type TaskGraphFact struct {
	ID              string         `json:"id"`
	NodeID          string         `json:"node_id"`
	Kind            string         `json:"kind"`
	AccountID       string         `json:"account_id"`
	TargetID        string         `json:"target_id"`
	SourceRuntimeID string         `json:"source_runtime_id"`
	SourceEventID   string         `json:"source_event_id"`
	EvidenceRef     string         `json:"evidence_ref"`
	ObservedAt      time.Time      `json:"observed_at"`
	Data            map[string]any `json:"data"`
}

type graphSnapshot struct {
	TaskVersion   int               `json:"task_version"`
	ScopeRevision int               `json:"scope_revision"`
	GoalCount     int               `json:"goal_count"`
	TaskState     string            `json:"task_state"`
	Anchors       []TaskGraphAnchor `json:"anchors"`
	Nodes         []TaskGraphNode   `json:"nodes"`
	Facts         []TaskGraphFact   `json:"facts"`
	Edges         []TaskGraphEdge   `json:"edges"`
}

func graphID(prefix, workspace, taskID, key string) string {
	sum := sha256.Sum256([]byte(workspace + "\x00" + taskID + "\x00" + key))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func graphDigest(value any) (string, []byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), payload, nil
}

// ProjectTaskGraph materializes a replayable BBA-owned projection. It reads
// core execution truth and never creates or advances an execution.
func (s *Store) ProjectTaskGraph(ctx context.Context, workspace, taskID string) (TaskGraph, error) {
	if workspace == "" || !taskValidID(taskID) {
		return TaskGraph{}, problem(taskCodeValidation)
	}
	var out TaskGraph
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, workspace, taskID)
		if err != nil {
			return err
		}
		out, err = projectTaskGraphTx(ctx, tx, task)
		return err
	})
	return out, err
}

func projectTaskGraphTx(ctx context.Context, tx pgx.Tx, task TaskRow) (TaskGraph, error) {
	graph, err := buildTaskGraph(ctx, tx, task)
	if err != nil {
		return TaskGraph{}, err
	}
	content := graphSnapshot{
		TaskVersion: task.Version, ScopeRevision: task.ScopeRevision, GoalCount: task.GoalCount,
		TaskState: task.State, Anchors: graph.Anchors, Nodes: graph.Nodes, Facts: graph.Facts, Edges: graph.Edges,
	}
	graph.SnapshotDigest, _, err = graphDigest(content)
	if err != nil {
		return TaskGraph{}, err
	}

	var rootID, storedDigest string
	var revision int64
	err = tx.QueryRow(ctx, `
		INSERT INTO core.task_graph_root(workspace_id,task_id,root_id,task_version,scope_revision)
		VALUES($1,$2,$3,$4,$5)
		ON CONFLICT (workspace_id,task_id) DO UPDATE
		SET task_version=EXCLUDED.task_version,scope_revision=EXCLUDED.scope_revision
		RETURNING root_id,graph_revision,snapshot_digest`,
		task.WorkspaceID, task.ID, graphID("graph", task.WorkspaceID, task.ID, "root"), task.Version, task.ScopeRevision,
	).Scan(&rootID, &revision, &storedDigest)
	if err != nil {
		return TaskGraph{}, err
	}
	if storedDigest != graph.SnapshotDigest {
		revision++
		if err := persistGraphProjection(ctx, tx, task, graph, revision); err != nil {
			return TaskGraph{}, err
		}
		graph.ProjectedAt = time.Now().UTC()
		graph.Revision = revision
		graph.RootID = rootID
		graph.WorkspaceID = task.WorkspaceID
		graph.TaskID = task.ID
		payload, err := json.Marshal(graph)
		if err != nil {
			return TaskGraph{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE core.task_graph_root
			SET graph_revision=$3,snapshot_digest=$4,projected_at=$5,task_version=$6,scope_revision=$7
			WHERE workspace_id=$1 AND task_id=$2`, task.WorkspaceID, task.ID, revision, graph.SnapshotDigest, graph.ProjectedAt, task.Version, task.ScopeRevision); err != nil {
			return TaskGraph{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO core.task_graph_snapshot(workspace_id,task_id,graph_revision,task_version,scope_revision,snapshot_digest,snapshot_json)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, task.WorkspaceID, task.ID, revision, task.Version, task.ScopeRevision, graph.SnapshotDigest, payload); err != nil {
			return TaskGraph{}, err
		}
	} else {
		graph.RootID, graph.Revision = rootID, revision
		graph.WorkspaceID, graph.TaskID = task.WorkspaceID, task.ID
		if err := tx.QueryRow(ctx, `SELECT projected_at FROM core.task_graph_root WHERE workspace_id=$1 AND task_id=$2`, task.WorkspaceID, task.ID).Scan(&graph.ProjectedAt); err != nil {
			return TaskGraph{}, err
		}
	}
	return graph, nil
}

func buildTaskGraph(ctx context.Context, tx pgx.Tx, task TaskRow) (TaskGraph, error) {
	graph := TaskGraph{WorkspaceID: task.WorkspaceID, TaskID: task.ID, TaskVersion: task.Version, ScopeRevision: task.ScopeRevision, GoalCount: task.GoalCount, TaskState: task.State, Anchors: []TaskGraphAnchor{}, Nodes: []TaskGraphNode{}, Facts: []TaskGraphFact{}, Edges: []TaskGraphEdge{}}
	rows, err := tx.Query(ctx, `SELECT account_id FROM core.task_account WHERE workspace_id=$1 AND task_id=$2 ORDER BY account_id`, task.WorkspaceID, task.ID)
	if err != nil {
		return graph, err
	}
	for rows.Next() {
		var accountID string
		if err := rows.Scan(&accountID); err != nil {
			rows.Close()
			return graph, err
		}
		anchorID := graphID("anchor", task.WorkspaceID, task.ID, "account:"+accountID)
		account := accountID
		graph.Anchors = append(graph.Anchors, TaskGraphAnchor{ID: anchorID, Kind: "account", AccountID: &account})
		graph.Nodes = append(graph.Nodes, TaskGraphNode{ID: graphID("node", task.WorkspaceID, task.ID, "account:"+accountID), Kind: "account", AnchorID: &anchorID, State: "in_scope", Data: map[string]any{"account_id": accountID}})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return graph, err
	}
	rows.Close()

	rows, err = tx.Query(ctx, `SELECT target_id,source_ref,eligibility,baseline_covered FROM core.task_target WHERE workspace_id=$1 AND task_id=$2 ORDER BY target_id`, task.WorkspaceID, task.ID)
	if err != nil {
		return graph, err
	}
	for rows.Next() {
		var targetID, sourceRef, eligibility string
		var baseline bool
		if err := rows.Scan(&targetID, &sourceRef, &eligibility, &baseline); err != nil {
			rows.Close()
			return graph, err
		}
		anchorID := graphID("anchor", task.WorkspaceID, task.ID, "target:"+targetID)
		target, source, eligible, covered := targetID, sourceRef, eligibility, baseline
		graph.Anchors = append(graph.Anchors, TaskGraphAnchor{ID: anchorID, Kind: "target", TargetID: &target, SourceRef: &source, Eligibility: &eligible, BaselineCovered: &covered})
		graph.Nodes = append(graph.Nodes, TaskGraphNode{ID: graphID("node", task.WorkspaceID, task.ID, "target:"+targetID), Kind: "target", AnchorID: &anchorID, State: eligibility, Data: map[string]any{"target_id": targetID, "baseline_covered": baseline}})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return graph, err
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
		SELECT r.id,r.account_id,r.target_id,r.source_runtime_id,r.source_event_id,r.evidence_ref,r.confirmed_at
		FROM core.result r
		JOIN core.execution e ON (e.workspace_id,e.id)=(r.workspace_id,r.execution_id)
		JOIN core.inbox i ON (i.workspace_id,i.source_runtime_id,i.event_id)=(r.workspace_id,r.source_runtime_id,r.source_event_id)
		JOIN core.membership m ON (m.workspace_id,m.account_id,m.target_id)=(r.workspace_id,r.account_id,r.target_id)
        JOIN core.account a ON (a.workspace_id,a.id)=(i.workspace_id,i.source_account_id)
		JOIN core.runtime_credentials rc ON (rc.workspace_id,rc.runtime_id,rc.account_id)=(i.workspace_id,i.source_runtime_id,i.source_account_id)
		WHERE r.workspace_id=$1 AND r.task_id=$2 AND r.valid
		  AND e.state='OBSERVED' AND e.disposition='confirmed' AND e.effect_certainty='confirmed' AND e.scope_revision=$3
          AND e.runtime_id=r.source_runtime_id AND e.account_id=r.account_id AND e.target_id=r.target_id
          AND e.operation_id=i.operation_id AND i.source_account_id=r.account_id AND i.target_id=r.target_id AND i.evidence_ref=r.evidence_ref
          AND a.runtime_id=i.source_runtime_id AND a.owner_epoch=i.source_epoch
		  AND i.kind='membership.observed' AND i.state='joined' AND i.snapshot_complete AND i.apply_state='applied'
		  AND m.state='joined' AND m.source_runtime_id=i.source_runtime_id AND m.source_event_id=i.event_id AND m.evidence_ref=r.evidence_ref
		  AND rc.active AND rc.membership_authority AND rc.owner_epoch=i.source_epoch
          AND e.owner_epoch=i.source_epoch
		ORDER BY r.id`, task.WorkspaceID, task.ID, task.ScopeRevision)
	if err != nil {
		return graph, err
	}
	for rows.Next() {
		var resultID, accountID, targetID, runtimeID, eventID, evidenceRef string
		var observedAt time.Time
		if err := rows.Scan(&resultID, &accountID, &targetID, &runtimeID, &eventID, &evidenceRef, &observedAt); err != nil {
			rows.Close()
			return graph, err
		}
		key := "task-result:" + accountID + ":" + targetID + ":" + runtimeID + ":" + eventID
		factID, nodeID := graphID("fact", task.WorkspaceID, task.ID, key), graphID("node", task.WorkspaceID, task.ID, "fact:"+key)
		graph.Facts = append(graph.Facts, TaskGraphFact{ID: factID, NodeID: nodeID, Kind: "task_result", AccountID: accountID, TargetID: targetID, SourceRuntimeID: runtimeID, SourceEventID: eventID, EvidenceRef: evidenceRef, ObservedAt: observedAt.UTC(), Data: map[string]any{"result_id": resultID, "state": "joined", "task_version": task.Version, "scope_revision": task.ScopeRevision}})
		graph.Nodes = append(graph.Nodes, TaskGraphNode{ID: nodeID, Kind: "fact", State: "confirmed", Data: map[string]any{"fact_id": factID, "target_id": targetID, "account_id": accountID}})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return graph, err
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
		SELECT m.account_id,m.target_id,m.source_runtime_id,m.source_event_id,m.evidence_ref,m.observed_at,m.state
		FROM core.membership m
		JOIN core.task_account ta ON (ta.workspace_id,ta.account_id)=(m.workspace_id,m.account_id) AND ta.task_id=$2
		JOIN core.task_target tt ON (tt.workspace_id,tt.target_id)=(m.workspace_id,m.target_id) AND tt.task_id=$2
		JOIN core.inbox i ON (i.workspace_id,i.source_runtime_id,i.event_id)=(m.workspace_id,m.source_runtime_id,m.source_event_id)
        JOIN core.account a ON (a.workspace_id,a.id)=(i.workspace_id,i.source_account_id)
		WHERE m.workspace_id=$1 AND i.kind='membership.observed' AND i.snapshot_complete AND i.apply_state='applied'
          AND i.source_account_id=m.account_id AND i.target_id=m.target_id AND i.evidence_ref=m.evidence_ref
          AND a.runtime_id=i.source_runtime_id AND a.owner_epoch=i.source_epoch
		ORDER BY m.account_id,m.target_id`, task.WorkspaceID, task.ID)
	if err != nil {
		return graph, err
	}
	for rows.Next() {
		var accountID, targetID, runtimeID, eventID, evidenceRef, state string
		var observedAt time.Time
		if err := rows.Scan(&accountID, &targetID, &runtimeID, &eventID, &evidenceRef, &observedAt, &state); err != nil {
			rows.Close()
			return graph, err
		}
		key := "membership:" + accountID + ":" + targetID + ":" + runtimeID + ":" + eventID
		factID, nodeID := graphID("fact", task.WorkspaceID, task.ID, key), graphID("node", task.WorkspaceID, task.ID, "fact:"+key)
		graph.Facts = append(graph.Facts, TaskGraphFact{ID: factID, NodeID: nodeID, Kind: "membership", AccountID: accountID, TargetID: targetID, SourceRuntimeID: runtimeID, SourceEventID: eventID, EvidenceRef: evidenceRef, ObservedAt: observedAt.UTC(), Data: map[string]any{"state": state, "task_version": task.Version, "scope_revision": task.ScopeRevision}})
		graph.Nodes = append(graph.Nodes, TaskGraphNode{ID: nodeID, Kind: "fact", State: state, Data: map[string]any{"fact_id": factID, "target_id": targetID, "account_id": accountID}})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return graph, err
	}
	rows.Close()
	confirmed := 0
	for _, fact := range graph.Facts {
		if fact.Kind == "task_result" {
			confirmed++
		}
	}
	goalState := "open"
	if confirmed >= task.GoalCount {
		goalState = "satisfied"
	}
	graph.Nodes = append(graph.Nodes,
		TaskGraphNode{ID: graphID("node", task.WorkspaceID, task.ID, "task"), Kind: "task", State: task.State, Data: map[string]any{"task_version": task.Version, "scope_revision": task.ScopeRevision}},
		TaskGraphNode{ID: graphID("node", task.WorkspaceID, task.ID, "goal"), Kind: "goal", State: goalState, Data: map[string]any{"goal_count": task.GoalCount, "confirmed_count": confirmed}},
	)
	taskNodeID := graphID("node", task.WorkspaceID, task.ID, "task")
	goalNodeID := graphID("node", task.WorkspaceID, task.ID, "goal")
	graph.Edges = append(graph.Edges, TaskGraphEdge{ID: graphID("edge", task.WorkspaceID, task.ID, "task-goal"), From: taskNodeID, To: goalNodeID, Kind: "has_goal"})
	for _, anchor := range graph.Anchors {
		nodeKind, key := anchor.Kind, ""
		if anchor.Kind == "account" {
			key = "account:" + *anchor.AccountID
		} else {
			key = "target:" + *anchor.TargetID
		}
		nodeID := graphID("node", task.WorkspaceID, task.ID, key)
		graph.Edges = append(graph.Edges, TaskGraphEdge{ID: graphID("edge", task.WorkspaceID, task.ID, "task-"+key), From: taskNodeID, To: nodeID, Kind: "has_" + nodeKind})
	}
	for _, fact := range graph.Facts {
		edgeKind := "supports_result"
		if fact.Kind == "membership" {
			edgeKind = "observes_membership"
		}
		for _, endpoint := range []struct{ kind, value string }{{"account", fact.AccountID}, {"target", fact.TargetID}} {
			anchorNodeID := graphID("node", task.WorkspaceID, task.ID, endpoint.kind+":"+endpoint.value)
			edgeID := graphID("edge", task.WorkspaceID, task.ID, "fact:"+fact.ID+":"+endpoint.kind)
			graph.Edges = append(graph.Edges, TaskGraphEdge{ID: edgeID, From: fact.NodeID, To: anchorNodeID, Kind: edgeKind})
		}
	}
	return graph, nil
}

func persistGraphProjection(ctx context.Context, tx pgx.Tx, task TaskRow, graph TaskGraph, revision int64) error {
	for _, anchor := range graph.Anchors {
		if anchor.Kind == "account" {
			if _, err := tx.Exec(ctx, `INSERT INTO core.task_graph_anchor(workspace_id,task_id,anchor_id,anchor_kind,account_id) VALUES($1,$2,$3,'account',$4) ON CONFLICT DO NOTHING`, task.WorkspaceID, task.ID, anchor.ID, *anchor.AccountID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `INSERT INTO core.task_graph_anchor(workspace_id,task_id,anchor_id,anchor_kind,target_id,source_ref,eligibility,baseline_covered) VALUES($1,$2,$3,'target',$4,$5,$6,$7) ON CONFLICT DO NOTHING`, task.WorkspaceID, task.ID, anchor.ID, *anchor.TargetID, *anchor.SourceRef, *anchor.Eligibility, *anchor.BaselineCovered); err != nil {
				return err
			}
		}
	}
	for _, node := range graph.Nodes {
		payload, err := json.Marshal(node.Data)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO core.task_graph_node(workspace_id,task_id,node_id,node_kind,anchor_id,state,node_json,updated_revision)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (workspace_id,task_id,node_id) DO UPDATE
			SET anchor_id=EXCLUDED.anchor_id,state=EXCLUDED.state,node_json=EXCLUDED.node_json,updated_revision=EXCLUDED.updated_revision`, task.WorkspaceID, task.ID, node.ID, node.Kind, node.AnchorID, node.State, payload, revision); err != nil {
			return err
		}
	}
	for _, fact := range graph.Facts {
		payload, err := json.Marshal(fact.Data)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO core.task_graph_fact(workspace_id,task_id,fact_id,node_id,fact_kind,account_id,target_id,source_runtime_id,source_event_id,evidence_ref,observed_at,fact_json,updated_revision)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (workspace_id,task_id,fact_id) DO NOTHING`, task.WorkspaceID, task.ID, fact.ID, fact.NodeID, fact.Kind, fact.AccountID, fact.TargetID, fact.SourceRuntimeID, fact.SourceEventID, fact.EvidenceRef, fact.ObservedAt, payload, revision); err != nil {
			return err
		}
	}
	for _, edge := range graph.Edges {
		if _, err := tx.Exec(ctx, `INSERT INTO core.task_graph_edge(workspace_id,task_id,edge_id,from_node_id,to_node_id,edge_kind,edge_json,updated_revision)
            VALUES($1,$2,$3,$4,$5,$6,'{}',$7) ON CONFLICT (workspace_id,task_id,edge_id) DO NOTHING`, task.WorkspaceID, task.ID, edge.ID, edge.From, edge.To, edge.Kind, revision); err != nil {
			return err
		}
	}
	return nil
}

// GetTaskGraph authorizes against the same task scope as GetTask, then refreshes only the internal read projection.
func (s *Store) GetTaskGraph(ctx context.Context, actor Actor, taskID string) (TaskGraph, error) {
	if actor.RuntimeID != "" {
		return TaskGraph{}, problem(taskCodeForbidden)
	}
	if !taskValidID(taskID) {
		return TaskGraph{}, problem(taskCodeValidation)
	}
	if err := actor.require("task.read"); err != nil {
		return TaskGraph{}, err
	}
	var out TaskGraph
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.read"); err != nil {
			return err
		}
		out, err = projectTaskGraphTx(ctx, tx, task)
		return err
	})
	return out, err
}
