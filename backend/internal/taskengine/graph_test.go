package taskengine

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestTaskGraphProjectionIsScopedTrustedAndReplayable(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_graph", "rt_graph", 2)
	task := createReadyTask(t, s, []string{"acc_graph"}, []string{"900091", "900092"}, 1, 2)
	first, err := s.ProjectTaskGraph(context.Background(), "ws_fixture", string(task.Id))
	if err != nil {
		t.Fatalf("first projection: %v", err)
	}
	if first.Revision != 1 || len(first.Anchors) != 3 || len(first.Facts) != 0 || len(first.Edges) != 4 {
		t.Fatalf("initial graph anchors=%d facts=%d edges=%+v", len(first.Anchors), len(first.Facts), first.Edges)
	}
	second, err := s.ProjectTaskGraph(context.Background(), "ws_fixture", string(task.Id))
	if err != nil {
		t.Fatalf("replay projection: %v", err)
	}
	if second.Revision != first.Revision || second.SnapshotDigest != first.SnapshotDigest || len(second.Edges) != len(first.Edges) {
		t.Fatalf("replay changed graph revision or edges: first=%+v second=%+v", first, second)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_graph_snapshot WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id); got != 1 {
		t.Fatalf("snapshot count=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_graph_anchor WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id); got != 3 {
		t.Fatalf("anchor count=%d want 3", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_graph_edge WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id); got != 4 {
		t.Fatalf("stored graph edges=%d want 4", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id); got != 0 {
		t.Fatalf("graph created executions=%d", got)
	}
}

func TestTaskGraphExcludesUnknownAndInvalidResults(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_graph_unknown", "rt_graph_unknown", 2)
	task := createReadyTask(t, s, []string{"acc_graph_unknown"}, []string{"900093"}, 1, 2)
	r := reserveFixture(t, s, string(task.Id), "acc_graph_unknown")
	rt := obsTestAcceptPermit(t, s, r)
	obsTestDispatch(t, s, rt, r, SendOutcome{}, context.DeadlineExceeded, true)
	obsTestReceiveApply(t, s, r, obsTestObservation(r, "evt_graph_unknown", 1, "operation.observed", "unknown", false, "ev_graph_unknown", time.Now().UTC()))
	graph, err := s.ProjectTaskGraph(context.Background(), "ws_fixture", string(task.Id))
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Facts) != 0 {
		t.Fatalf("unknown execution created facts: %+v", graph.Facts)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_graph_fact WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id); got != 0 {
		t.Fatalf("graph fact rows=%d want 0", got)
	}

	s2, _, joined := remediationConfirmedJoin(t)
	before, err := s2.ProjectTaskGraph(context.Background(), joined.WorkspaceID, joined.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Facts) != 2 {
		t.Fatalf("trusted result and membership facts=%d want 2", len(before.Facts))
	}
	leave := obsTestObservation(joined, "evt_graph_leave", 2, "membership.observed", "not_joined", true, "ev_graph_leave", time.Now().UTC())
	obsTestReceiveApply(t, s2, joined, leave)
	after, err := s2.ProjectTaskGraph(context.Background(), joined.WorkspaceID, joined.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range after.Facts {
		if fact.Kind == "task_result" {
			t.Fatalf("invalid result projected as active fact: %+v", fact)
		}
	}
	if after.Revision <= before.Revision {
		t.Fatalf("invalidation did not advance graph revision: before=%d after=%d", before.Revision, after.Revision)
	}
}

func TestTaskGraphReadRequiresTaskAuthorization(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_graph_auth", "rt_graph_auth", 2)
	task := createReadyTask(t, s, []string{"acc_graph_auth"}, []string{"900094"}, 1, 2)
	if _, err := s.ProjectTaskGraph(context.Background(), "ws_fixture", string(task.Id)); err != nil {
		t.Fatal(err)
	}
	actor := fixtureActor()
	actor.Permissions = map[string]bool{"task.read": true}
	actor.PrincipalID = "other_principal"
	if _, err := s.GetTaskGraph(context.Background(), actor, string(task.Id)); err == nil || Code(err) != taskCodeNotFound {
		t.Fatalf("unauthorized graph read error=%v", err)
	}
	actor = fixtureActor()
	actor.RuntimeID = "rt_graph_auth"
	if _, err := s.GetTaskGraph(context.Background(), actor, string(task.Id)); err == nil || Code(err) != taskCodeForbidden {
		t.Fatalf("runtime graph read error=%v", err)
	}
}
func TestTaskGraphHTTPEndpointReturnsAuthorizedSnapshot(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_graph_http", "rt_graph_http", 2)
	task := createReadyTask(t, s, []string{"acc_graph_http"}, []string{"900095"}, 1, 2)
	actor := fixtureActor()
	handler := s.Handler(func(*http.Request) (Actor, error) { return actor, nil })
	response := ttServe(handler, http.MethodGet, "/v1/tasks/"+string(task.Id)+"/graph", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("authorized graph status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data TaskGraph `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.TaskID != string(task.Id) || envelope.Data.Revision != 1 || len(envelope.Data.Edges) != 3 {
		t.Fatalf("graph response=%+v", envelope.Data)
	}
	actor.RuntimeID = "rt_graph_http"
	response = ttServe(handler, http.MethodGet, "/v1/tasks/"+string(task.Id)+"/graph", nil, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("runtime graph status=%d body=%s", response.Code, response.Body.String())
	}
}
