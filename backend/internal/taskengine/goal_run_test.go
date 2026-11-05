package taskengine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func goalFixture(t *testing.T) (*GoalService, Actor, GoalRun) {
	t.Helper()
	s := fixtureStore(t)
	r, m, skill, agent := configFixture(t)
	// Use the existing registry and version validators in this workspace.
	r.mu.Lock()
	r.data.Workspaces["ws_fixture"] = []modelStoredProfile{{ModelProfile: m, APIKey: "fixture-secret"}}
	r.data.Skills["ws_fixture"] = []SkillProfile{skill}
	r.data.SkillHistory[configKey("ws_fixture", skill.ID)] = []SkillProfile{skill}
	r.data.Agents["ws_fixture"] = []AgentProfile{agent}
	r.data.AgentHistory[configKey("ws_fixture", agent.ID)] = []AgentProfile{agent}
	r.mu.Unlock()
	actor := fixtureActor()
	actor.Permissions["conversation.read"] = true
	actor.Permissions["conversation.write"] = true
	g := NewGoalService(s, r, true)
	run, err := g.Start(context.Background(), actor, "start", GoalStart{ThreadID: "thread_goal", GroupID: "20001", Goal: "分析指定测试群的近期需求，证据不足时列出信息缺口。", PlannerRef: SkillBinding{agent.ID, agent.Version}, AnalystRef: SkillBinding{agent.ID, agent.Version}})
	if err != nil {
		t.Fatal(err)
	}
	return g, actor, run
}
func goalWindow(id, text string) GoalWindow {
	now := time.Now().UTC()
	return GoalWindow{EventID: id, GroupID: "20001", WindowStart: now.Add(-time.Minute), WindowEnd: now, ExpiresAt: now.Add(10 * time.Minute), Messages: []GoalMessage{{ID: id + "_msg", At: now.Add(-time.Second), Text: text}}}
}
func goalDrain(t *testing.T, g *GoalService) {
	t.Helper()
	for i := 0; i < 12; i++ {
		if err := g.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
func TestGoalOfflineLoopConsumesNewEvidenceAndOperatorConfirms(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	if _, err := g.Inject(ctx, a, r.ID, "sparse", goalWindow("sparse", "需求：了解课程")); err != nil {
		t.Fatal(err)
	}
	goalDrain(t, g)
	first, err := g.Get(ctx, a, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "WAITING_INPUT" || first.Round != 1 || len(first.Rounds) != 1 {
		t.Fatalf("first=%+v", first)
	}
	if _, err := g.Inject(ctx, a, r.ID, "full", goalWindow("full", "需求：课程方案；预算：1000；时间：本周")); err != nil {
		t.Fatal(err)
	}
	goalDrain(t, NewGoalService(g.store, g.registry, true))
	next, err := g.Get(ctx, a, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next.State != "AWAITING_CONFIRMATION" || next.Round != 2 || len(next.Evidence) < 6 || len(next.Graph.Anchors) < 2 {
		t.Fatalf("next=%+v", next)
	}
	if len(next.Rounds[1].ConsumedEvidenceIDs) == 0 || next.Rounds[1].ChangeReason == first.Rounds[0].ChangeReason {
		t.Fatal("second plan did not consume new evidence")
	}
	done, err := g.Control(ctx, a, r.ID, "confirm", next.Version, "confirm")
	if err != nil || done.State != "CONFIRMED" {
		t.Fatalf("confirm %+v %v", done, err)
	}
	for _, table := range []string{"execution", "dispatch_permit", "chat_send_permit", "work_item", "task"} {
		// New orchestration has no QQ writer or Task side effects.
		var n int
		if err := g.store.Pool.QueryRow(ctx, "SELECT count(*) FROM core."+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("unexpected %s writes: %d", table, n)
		}
	}
}
func TestGoalConcurrentDuplicateWakeupAndRecovery(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	w := goalWindow("event", "需求：课程")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := g.Inject(ctx, a, r.ID, "event", w); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	// State commit before event publication survives an instance restart.
	g = NewGoalService(g.store, g.registry, true)
	if err := g.publish(ctx); err != nil {
		t.Fatal(err)
	}
	// Publication is replay-safe, including concurrent consumers.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := g.publish(ctx); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	job, err := g.claim(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim %v %v", job, err)
	}
	old := *job
	// Crash after claim: expired lease creates a new attempt of the SAME logical step.
	if _, err = g.store.Pool.Exec(ctx, "UPDATE core.goal_step SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	g = NewGoalService(g.store, g.registry, true)
	retry, err := g.claim(ctx)
	if err != nil || retry == nil || retry.ID != old.ID || retry.Attempt != old.Attempt+1 {
		t.Fatalf("retry %+v %v", retry, err)
	}
	stale, _ := deterministicGoalModel(ctx, old)
	if err = g.submit(ctx, old, stale); err != nil {
		t.Fatal(err)
	}
	if err = g.apply(ctx); err != nil {
		t.Fatal(err)
	}
	valid, _ := deterministicGoalModel(ctx, *retry)
	if err = g.submit(ctx, *retry, valid); err != nil {
		t.Fatal(err)
	}
	// Crash after receiving model result, before reducer commit.
	g = NewGoalService(g.store, g.registry, true)
	goalDrain(t, g)
	view, err := g.Get(ctx, a, r.ID)
	if err != nil || view.Round != 1 || len(view.Rounds[0].Steps) != 2 {
		t.Fatalf("view %+v %v", view, err)
	}
	if _, err = g.Inject(ctx, a, r.ID, "event", w); err != nil {
		t.Fatal(err)
	}
	goalDrain(t, g)
	again, _ := g.Get(ctx, a, r.ID)
	if again.Round != 1 {
		t.Fatal("event replay advanced twice")
	}
}
func TestGoalScopeExpiryStopAndLateResult(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	other := a
	other.WorkspaceID = "other"
	if _, err := g.store.Pool.Exec(ctx, "INSERT INTO core.workspace(id) VALUES('other'); INSERT INTO core.principal(workspace_id,id) VALUES('other','principal_fixture')"); err != nil {
		t.Fatal(err)
	}
	if _, e := g.Get(ctx, other, r.ID); Code(e) != "RESOURCE_NOT_FOUND" {
		t.Fatal("workspace escape", e)
	}
	bad := goalWindow("wrong", "anything")
	bad.GroupID = "20002"
	if _, e := g.Inject(ctx, a, r.ID, "wrong", bad); Code(e) != "GOAL_SCOPE_MISMATCH" {
		t.Fatal(e)
	}
	expired := goalWindow("expired", "anything")
	expired.ExpiresAt = time.Now().Add(-time.Second)
	if _, e := g.Inject(ctx, a, r.ID, "expired", expired); Code(e) != "WINDOW_EXPIRED" {
		t.Fatal(e)
	}
	w := goalWindow("pending", "需求：课程")
	if _, e := g.Inject(ctx, a, r.ID, "pending", w); e != nil {
		t.Fatal(e)
	}
	if e := g.publish(ctx); e != nil {
		t.Fatal(e)
	}
	job, e := g.claim(ctx)
	if e != nil || job == nil {
		t.Fatal(e)
	}
	before, _ := g.Get(ctx, a, r.ID)
	if _, e = g.Control(ctx, a, r.ID, "stop", before.Version, "stop"); e != nil {
		t.Fatal(e)
	}
	out, _ := deterministicGoalModel(ctx, *job)
	if e = g.submit(ctx, *job, out); e != nil {
		t.Fatal(e)
	}
	goalDrain(t, g)
	stopped, _ := g.Get(ctx, a, r.ID)
	if stopped.State != "STOPPED" {
		t.Fatal("late result restarted run")
	}
}

func TestGoalAcceptedWindowExpiryDoesNotInventResults(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	w := goalWindow("expires_after_commit", "需求：课程")
	if _, e := g.Inject(ctx, a, r.ID, "expiry", w); e != nil {
		t.Fatal(e)
	}
	// Simulate expiry at the state-commit/event-publication boundary.
	w.ExpiresAt = time.Now().Add(-time.Second)
	if _, e := g.store.Pool.Exec(ctx, `UPDATE core.goal_event SET payload=$2 WHERE run_id=$1`, r.ID, goalJSON(w)); e != nil {
		t.Fatal(e)
	}
	goalDrain(t, g)
	view, e := g.Get(ctx, a, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if view.State != "WAITING_INPUT" || view.Reason != "WINDOW_EXPIRED" || len(view.Evidence) != 0 || view.Round != 0 {
		t.Fatalf("expired %+v", view)
	}
	if _, e = g.Inject(ctx, a, r.ID, "valid", goalWindow("valid", "需求：课程")); e != nil {
		t.Fatal(e)
	}
	if e = g.publish(ctx); e != nil {
		t.Fatal(e)
	}
	j, e := g.claim(ctx)
	if e != nil || j == nil {
		t.Fatal(e)
	}
	output, _ := deterministicGoalModel(ctx, *j)
	if e = g.submit(ctx, *j, output); e != nil {
		t.Fatal(e)
	}
	// Simulate expiry at result-receive/reducer boundary without changing the result.
	j.Window.ExpiresAt = time.Now().Add(-time.Second)
	if _, e = g.store.Pool.Exec(ctx, `UPDATE core.goal_step SET input=$2 WHERE id=$1`, j.ID, goalJSON(goalInput{Window: j.Window, Memory: j.Memory, Goal: j.Goal, Plan: j.Plan})); e != nil {
		t.Fatal(e)
	}
	goalDrain(t, g)
	view, e = g.Get(ctx, a, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if view.State != "WAITING_INPUT" || len(view.Evidence) != 0 || !strings.Contains(view.Reason, "WINDOW_EXPIRED") {
		t.Fatalf("expired result %+v", view)
	}
}
func TestGoalOutputCannotExpressToolsOrQQActions(t *testing.T) {
	j := GoalJob{Role: "planner", Window: goalWindow("out", "需求：课程")}
	raw, e := deterministicGoalModel(context.Background(), j)
	if e != nil {
		t.Fatal(e)
	}
	var payload map[string]any
	if e = json.Unmarshal(raw, &payload); e != nil {
		t.Fatal(e)
	}
	for _, field := range []string{"tool_calls", "request_join", "send_message"} {
		payload[field] = []string{"forbidden"}
		if _, e = validateGoalOutput(j, goalJSON(payload)); Code(e) != "GOAL_OUTPUT_INVALID" {
			t.Fatalf("accepted %s: %v", field, e)
		}
		delete(payload, field)
	}
}
