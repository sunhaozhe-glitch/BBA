package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGoalHTTPFixtureGateScopeAndSnapshots(t *testing.T) {
	g, a, r := goalFixture(t)
	handler := g.Handler(func(*http.Request) (Actor, error) { return a, nil })
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(goalJSON(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "http-key")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := call("GET", "/v1/operator/goal-runs/"+r.ID, nil); rec.Code != 200 || strings.Contains(rec.Body.String(), "fixture-secret") {
		t.Fatalf("snapshot %d %s", rec.Code, rec.Body.String())
	}
	oldPrompt := r.Configuration.Planner.Agent.SystemPrompt
	g.registry.mu.Lock()
	g.registry.data.Agents[a.WorkspaceID][0].SystemPrompt = "changed after start"
	g.registry.mu.Unlock()
	saved, e := g.Get(context.Background(), a, r.ID)
	if e != nil || saved.Configuration.Planner.Agent.SystemPrompt != oldPrompt {
		t.Fatal("snapshot drift", e)
	}
	handler = NewGoalService(g.store, g.registry, false).Handler(func(*http.Request) (Actor, error) { return a, nil })
	if rec := call("POST", "/v1/operator/goal-runs/"+r.ID+"/fixture-windows", goalWindow("http", "anything")); rec.Code != 403 || !strings.Contains(rec.Body.String(), "GOAL_FIXTURE_DISABLED") {
		t.Fatalf("fixture gate %d %s", rec.Code, rec.Body.String())
	}
	handler = g.Handler(func(*http.Request) (Actor, error) { b := a; b.RuntimeID = "runtime"; return b, nil })
	if rec := call("GET", "/v1/operator/goal-runs", nil); rec.Code != 403 {
		t.Fatal("runtime allowed", rec.Code)
	}
	handler = g.Handler(func(*http.Request) (Actor, error) { return a, nil })
	if rec := call("POST", "/v1/operator/goal-runs/"+r.ID+"/fixture-windows", map[string]any{"event_id": "bad", "group_id": "20001", "unknown": "tool"}); rec.Code != 400 {
		t.Fatal("unknown input accepted", rec.Code)
	}
	if e := ValidateGoalFixtureDeployment(context.Background(), g.store, true, false); e == nil {
		t.Fatal("development gate missing")
	}
	t.Setenv("BBA_FIXTURE_ONLY", "0")
	if e := ValidateGoalFixtureDeployment(context.Background(), g.store, true, true); e == nil {
		t.Fatal("env gate missing")
	}
	t.Setenv("BBA_FIXTURE_ONLY", "1")
	if e := ValidateGoalFixtureDeployment(context.Background(), g.store, true, true); e != nil {
		t.Fatal(e)
	}
}
func TestGoalBoundsAndLateOldRound(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	var old GoalJob
	var oldOutput json.RawMessage
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("round%d", i)
		if _, e := g.Inject(ctx, a, r.ID, id, goalWindow(id, "需求：课程")); e != nil {
			t.Fatal(e)
		}
		if e := g.publish(ctx); e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			j, e := g.claim(ctx)
			if e != nil || j == nil {
				t.Fatal(e)
			}
			old = *j
			oldOutput, _ = deterministicGoalModel(ctx, old)
			if e = g.submit(ctx, old, oldOutput); e != nil {
				t.Fatal(e)
			}
		}
		goalDrain(t, g)
	}
	view, e := g.Get(ctx, a, r.ID)
	if e != nil || view.State != "LIMIT_REACHED" || view.Round != 4 {
		t.Fatalf("limits %+v %v", view, e)
	}
	if e = g.submit(ctx, old, oldOutput); e != nil {
		t.Fatal(e)
	}
	goalDrain(t, g)
	next, _ := g.Get(ctx, a, r.ID)
	if next.Version != view.Version {
		t.Fatal("old round rewrote current state")
	}
	if _, e = g.Inject(ctx, a, r.ID, "afterlimit", goalWindow("afterlimit", "anything")); Code(e) != "GOAL_STATE_CONFLICT" {
		t.Fatal(e)
	}
	g, a, r = goalFixture(t)
	if _, e = g.store.Pool.Exec(ctx, "UPDATE core.goal_run SET deadline_at=created_at+interval '1 microsecond' WHERE id=$1", r.ID); e != nil {
		t.Fatal(e)
	}
	time.Sleep(time.Millisecond)
	goalDrain(t, g)
	view, _ = g.Get(ctx, a, r.ID)
	if view.State != "TIMED_OUT" {
		t.Fatal("waiting deadline not enforced", view.State)
	}
}

func TestGoalCommandIdempotencyAndConfirmationBarrier(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	w := goalWindow("first", "需求：课程；预算：1000；时间：本周")
	if _, e := g.Inject(ctx, a, r.ID, "input-key", w); e != nil {
		t.Fatal(e)
	}
	changed := goalWindow("different", "需求：另一个问题")
	if _, e := g.Inject(ctx, a, r.ID, "input-key", changed); Code(e) != taskCodeIdempotency {
		t.Fatal("input key reused", e)
	}
	goalDrain(t, g)
	ready, _ := g.Get(ctx, a, r.ID)
	if ready.State != "AWAITING_CONFIRMATION" {
		t.Fatal(ready.State)
	}
	if _, e := g.Inject(ctx, a, r.ID, "second-key", changed); e != nil {
		t.Fatal(e)
	}
	if _, e := g.Control(ctx, a, r.ID, "confirm", ready.Version, "stale-confirm"); Code(e) != "GOAL_STATE_CONFLICT" {
		t.Fatal("confirmed with unconsumed evidence", e)
	}
	// Write permission alone must not commit a mutation that its caller cannot read.
	b := a
	b.Permissions = map[string]bool{"conversation.write": true}
	if _, e := g.Inject(ctx, b, r.ID, "forbidden", goalWindow("forbidden", "anything")); Code(e) != taskCodeForbidden {
		t.Fatal(e)
	}
	var count int
	if e := g.store.Pool.QueryRow(ctx, "SELECT count(*) FROM core.goal_event WHERE id='forbidden'").Scan(&count); e != nil || count != 0 {
		t.Fatal("mutation before auth", count, e)
	}
}

func TestGoalResultInboxIsImmutable(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	if _, e := g.Inject(ctx, a, r.ID, "inbox", goalWindow("inbox", "需求：课程")); e != nil {
		t.Fatal(e)
	}
	if e := g.publish(ctx); e != nil {
		t.Fatal(e)
	}
	j, e := g.claim(ctx)
	if e != nil || j == nil {
		t.Fatal(e)
	}
	raw, _ := deterministicGoalModel(ctx, *j)
	if e = g.submit(ctx, *j, raw); e != nil {
		t.Fatal(e)
	}
	var output goalOutput
	_ = json.Unmarshal(raw, &output)
	output.ChangeReason = "changed result for same attempt"
	if e = g.submit(ctx, *j, goalJSON(output)); Code(e) != taskCodeIdempotency {
		t.Fatal("inbox overwritten or silently ignored", e)
	}
}

func TestGoalExpiredConclusionCannotBeConfirmed(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	if _, e := g.Inject(ctx, a, r.ID, "complete", goalWindow("complete", "需求：课程；预算：1000；时间：本周")); e != nil {
		t.Fatal(e)
	}
	goalDrain(t, g)
	view, _ := g.Get(ctx, a, r.ID)
	if _, e := g.store.Pool.Exec(ctx, "UPDATE core.goal_evidence SET expires_at=clock_timestamp()-interval '1 second' WHERE workspace_id=$1 AND run_id=$2", a.WorkspaceID, r.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := g.Control(ctx, a, r.ID, "confirm", view.Version, "expired-confirm"); Code(e) != "WINDOW_EXPIRED" {
		t.Fatal("expired evidence confirmed", e)
	}
}
func TestGoalConcurrentTicksCreateTwoLogicalSteps(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	if _, e := g.Inject(ctx, a, r.ID, "tick", goalWindow("tick", "需求：课程")); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 8; n++ {
				if e := g.Tick(ctx); e != nil {
					t.Error(e)
					return
				}
			}
		}()
	}
	wg.Wait()
	goalDrain(t, g)
	v, e := g.Get(ctx, a, r.ID)
	if e != nil || v.Round != 1 || len(v.Rounds[0].Steps) != 2 || v.State != "WAITING_INPUT" {
		t.Fatalf("%+v %v", v, e)
	}
	for _, s := range v.Rounds[0].Steps {
		if s.Attempt != 1 {
			t.Fatal("duplicate attempt", s)
		}
	}
}
func TestGoalOutputRequiredFields(t *testing.T) {
	j := GoalJob{Role: "analyst", Window: goalWindow("required", "anything")}
	planner := j
	planner.Role = "planner"
	planRaw, _ := deterministicGoalModel(context.Background(), planner)
	var plan goalOutput
	_ = json.Unmarshal(planRaw, &plan)
	j.Plan = &plan
	raw, _ := deterministicGoalModel(context.Background(), j)
	var object map[string]any
	_ = json.Unmarshal(raw, &object)
	delete(object, "suggested_complete")
	if _, e := validateGoalOutput(j, goalJSON(object)); Code(e) != "GOAL_OUTPUT_INVALID" {
		t.Fatal("missing required output accepted", e)
	}
}

func TestGoalAnalystRequiresPlannerDispatch(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	if _, e := g.Inject(ctx, a, r.ID, "dispatch", goalWindow("dispatch", "需求：课程")); e != nil {
		t.Fatal(e)
	}
	if e := g.publish(ctx); e != nil {
		t.Fatal(e)
	}
	planner, e := g.claim(ctx)
	if e != nil || planner == nil {
		t.Fatal(e)
	}
	out, _ := deterministicGoalModel(ctx, *planner)
	if e = g.submit(ctx, *planner, out); e != nil {
		t.Fatal(e)
	}
	if e = g.apply(ctx); e != nil {
		t.Fatal(e)
	}
	analyst, e := g.claim(ctx)
	if e != nil || analyst == nil || analyst.Role != "analyst" {
		t.Fatal(e)
	}
	var input []byte
	if e = g.store.Pool.QueryRow(ctx, "SELECT input FROM core.goal_step WHERE id=$1", analyst.ID).Scan(&input); e != nil {
		t.Fatal(e)
	}
	var persisted map[string]json.RawMessage
	_ = json.Unmarshal(input, &persisted)
	if len(persisted["plan"]) == 0 || string(persisted["plan"]) == "null" {
		t.Fatal("planner output not dispatched to analyst")
	}
}

func TestGoalMaxLengthMessageIdentitiesRemainPersistable(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	w := goalWindow(strings.Repeat("e", 128), "需求：课程")
	w.Messages[0].ID = strings.Repeat("m", 128)
	if _, e := g.Inject(ctx, a, r.ID, "long-ids", w); e != nil {
		t.Fatal(e)
	}
	goalDrain(t, g)
	v, e := g.Get(ctx, a, r.ID)
	if e != nil || len(v.Evidence) == 0 {
		t.Fatal("valid input failed to persist", e)
	}
}

func TestGoalHTTPRejectsNonUTCAndNestedNull(t *testing.T) {
	g, a, r := goalFixture(t)
	h := g.Handler(func(*http.Request) (Actor, error) { return a, nil })
	w := goalWindow("offset", "anything")
	w.WindowStart = w.WindowStart.In(time.FixedZone("test", 8*3600))
	call := func(body []byte) {
		req := httptest.NewRequest("POST", "/v1/operator/goal-runs/"+r.ID+"/fixture-windows", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "utc")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("invalid wire accepted %d", rec.Code)
		}
	}
	call(goalJSON(w))
	var object map[string]any
	_ = json.Unmarshal(goalJSON(goalWindow("null", "anything")), &object)
	object["messages"].([]any)[0].(map[string]any)["id"] = nil
	call(goalJSON(object))
}

func TestGoalAttemptBudgetAndRollbackBoundaries(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	w := goalWindow("attempts", "需求：课程")
	if _, e := g.Inject(ctx, a, r.ID, "attempts", w); e != nil {
		t.Fatal(e)
	}
	// A publication failure rolls back the entire round/steps/outbox transition.
	if _, e := g.store.Pool.Exec(ctx, `CREATE FUNCTION core.goal_test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture rollback'; END $$; CREATE TRIGGER goal_fail BEFORE INSERT ON core.goal_step FOR EACH ROW EXECUTE FUNCTION core.goal_test_fail()`); e != nil {
		t.Fatal(e)
	}
	if e := g.publish(ctx); e == nil {
		t.Fatal("injected publication should fail")
	}
	if scalarInt(t, g.store, "SELECT count(*) FROM core.goal_round") != 0 || scalarInt(t, g.store, "SELECT count(*) FROM core.goal_event WHERE state='pending'") != 1 {
		t.Fatal("partial publication commit")
	}
	if _, e := g.store.Pool.Exec(ctx, "DROP TRIGGER goal_fail ON core.goal_step; DROP FUNCTION core.goal_test_fail()"); e != nil {
		t.Fatal(e)
	}
	if e := g.publish(ctx); e != nil {
		t.Fatal(e)
	}
	var id string
	for attempt := 1; attempt <= 2; attempt++ {
		j, e := g.claim(ctx)
		if e != nil || j == nil || j.Attempt != attempt {
			t.Fatal("claim", e)
		}
		if id != "" && j.ID != id {
			t.Fatal("new logical step on retry")
		}
		id = j.ID
		if _, e = g.store.Pool.Exec(ctx, "UPDATE core.goal_step SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", j.ID); e != nil {
			t.Fatal(e)
		}
	}
	if j, e := g.claim(ctx); e != nil || j != nil {
		t.Fatal("attempt budget bypass", j, e)
	}
	v, e := g.Get(ctx, a, r.ID)
	if e != nil || v.State != "WAITING_INPUT" || !strings.Contains(v.Reason, "ATTEMPT_LIMIT") {
		t.Fatal(v.State, v.Reason, e)
	}
}

func TestGoalWorkerResumesAfterPublicationFailure(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, e := g.Inject(ctx, a, r.ID, "worker-resume", goalWindow("worker-resume", "需求：课程")); e != nil {
		t.Fatal(e)
	}
	if _, e := g.store.Pool.Exec(ctx, `CREATE FUNCTION core.goal_worker_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture transient failure'; END $$; CREATE TRIGGER goal_fail BEFORE INSERT ON core.goal_step FOR EACH ROW EXECUTE FUNCTION core.goal_worker_fail()`); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()
	select {
	case e := <-done:
		t.Fatal("worker exited on recoverable transaction failure", e)
	case <-time.After(300 * time.Millisecond):
	}
	if _, e := g.store.Pool.Exec(ctx, "DROP TRIGGER goal_fail ON core.goal_step; DROP FUNCTION core.goal_worker_fail()"); e != nil {
		t.Fatal(e)
	}
	for {
		v, e := g.Get(ctx, a, r.ID)
		if e != nil {
			t.Fatal(e)
		}
		if v.Round == 1 && v.State == "WAITING_INPUT" {
			break
		}
		select {
		case e := <-done:
			t.Fatal("worker stopped", e)
		case <-ctx.Done():
			t.Fatal("worker did not recover")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}

func TestGoalStartReplayUsesSavedSnapshot(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	b := GoalStart{ThreadID: r.ThreadID, GroupID: r.GroupID, Goal: r.Goal, PlannerRef: SkillBinding{r.Configuration.Planner.Agent.ID, r.Configuration.Planner.Agent.Version}, AnalystRef: SkillBinding{r.Configuration.Analyst.Agent.ID, r.Configuration.Analyst.Agent.Version}}
	g.registry.mu.Lock()
	g.registry.data.Agents[a.WorkspaceID][0].Enabled = false
	g.registry.mu.Unlock()
	replay, e := g.Start(ctx, a, "start", b)
	if e != nil || replay.ID != r.ID {
		t.Fatal("committed start could not be replayed", e)
	}
	if _, e = g.Start(ctx, a, "new-start", b); Code(e) != "AGENT_REFERENCE_UNAVAILABLE" {
		t.Fatal("new run used disabled config", e)
	}
}

func TestGoalHTTPRequiredMembersAndExpiredConfirmation(t *testing.T) {
	g, a, r := goalFixture(t)
	ctx := context.Background()
	h := g.Handler(func(*http.Request) (Actor, error) { return a, nil })
	call := func(path string, body any) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, bytes.NewReader(goalJSON(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", newID("http"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := call("/v1/operator/goal-runs/"+r.ID+"/stop", map[string]any{}); rec.Code != 400 {
		t.Fatal("missing version", rec.Code)
	}
	body := map[string]any{"thread_id": "newthread", "group_id": "20001", "goal": "anything", "planner_ref": map[string]any{"id": r.Configuration.Planner.Agent.ID}, "analyst_ref": map[string]any{"id": r.Configuration.Analyst.Agent.ID, "version": 1}}
	if rec := call("/v1/operator/goal-runs", body); rec.Code != 400 {
		t.Fatal("missing ref version", rec.Code)
	}
	if _, e := g.Inject(ctx, a, r.ID, "expired-wire", goalWindow("expired-wire", "需求：课程；预算：1000；时间：本周")); e != nil {
		t.Fatal(e)
	}
	goalDrain(t, g)
	ready, _ := g.Get(ctx, a, r.ID)
	if _, e := g.store.Pool.Exec(ctx, "UPDATE core.goal_evidence SET expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1", r.ID); e != nil {
		t.Fatal(e)
	}
	rec := call("/v1/operator/goal-runs/"+r.ID+"/confirm", map[string]any{"expected_version": ready.Version})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "WINDOW_EXPIRED") {
		t.Fatal("expired confirmation wire", rec.Code)
	}
}

func TestGoalDeploymentRejectsRemoteFallbackBeforeQuery(t *testing.T) {
	g, _, _ := goalFixture(t)
	t.Setenv("BBA_FIXTURE_ONLY", "1")
	cfg := g.store.Pool.Config()
	cfg.ConnConfig.Fallbacks = append(cfg.ConnConfig.Fallbacks, &pgconn.FallbackConfig{Host: "203.0.113.19", Port: 5432})
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := ValidateGoalFixtureDeployment(context.Background(), &Store{Pool: pool}, true, true); err == nil {
		t.Fatal("remote fallback accepted as isolated fixture")
	}
}
