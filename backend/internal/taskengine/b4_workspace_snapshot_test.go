package taskengine

import (
	"bba.local/taskengine/internal/contract"
	"context"
	"encoding/json"
	"fmt"
	"github.com/oapi-codegen/nullable"
	"testing"
	"time"
)

func b4SeedAccount(t *testing.T, s *Store, id, runtime string, quota int, actor Actor) {
	t.Helper()
	ctx := context.Background()
	var num int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM core.account`).Scan(&num)
	if err != nil {
		t.Fatal(err)
	}
	self := fmt.Sprint(10001 + num)
	_, err = s.Pool.Exec(ctx, `INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit,heartbeat_at) VALUES($6,$1,$2,$3,$4,'verified','online','logged_in','active','clear',true,true,$5,clock_timestamp())`, id, runtime, "adapter_"+id, self, quota, actor.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority) VALUES($3,$1,$2,1,true)`, runtime, id, actor.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
}
func b4ReadyTask(t *testing.T, s *Store, accounts, groups []string, goal, maxAttempts int, reason, answer string, actor Actor, beforeStart func()) contract.Task {
	t.Helper()
	ctx := context.Background()
	approval := newID("approval")
	_, err := s.Pool.Exec(ctx, `INSERT INTO core.approval(workspace_id,id,principal_id,scope_revision,account_ids,target_ids,max_attempts,expires_at,join_reason,verification_message) VALUES($1,$2,$3,1,$4,$5,10000,clock_timestamp()+interval '1 day',$6,$7)`, actor.WorkspaceID, approval, actor.PrincipalID, accounts, groups, reason, answer)
	if err != nil {
		t.Fatal(err)
	}
	fixed := make([]contract.FixedCandidate, 0, len(groups))
	for _, id := range groups {
		fixed = append(fixed, contract.FixedCandidate{GroupId: id, SourceRef: "fixture_import"})
	}
	mode := contract.ExecutionMode("live")
	req := contract.CreateTaskRequest{Type: "group.acquire", Title: "fixture task", JoinReason: reason, VerificationMessage: answer, Goal: contract.Goal{Metric: "new_unique_groups_for_account_pool", Count: goal, StrictTargetCap: true, RequireFinalMembershipCheck: true}, AllowedAccountIds: accounts, CandidateSource: contract.CandidateSource{Kind: "fixed", Groups: fixed}, ExecutionMode: &mode, DeadlineAt: time.Now().UTC().Add(time.Hour), Limits: contract.TaskLimits{MaxCandidates: len(groups), MaxAttemptsPerAction: maxAttempts, MaxTotalExternalAttempts: 100, MaxModelAttempts: 0}, ApprovalRef: nullable.NewNullableWithValue(approval), Submit: true}
	task, err := s.CreateTask(ctx, actor, newID("create"), req)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	for {
		done, err := s.ExpandFixedPage(ctx, actor, task.Id, 100)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	snapshots := make([]BaselineSnapshot, 0, len(accounts))
	for _, id := range accounts {
		snapshots = append(snapshots, BaselineSnapshot{AccountID: id, EvidenceRef: "fixture_complete_baseline", ObservedAt: time.Now().UTC(), Complete: true, GroupIDs: []string{}})
	}
	if err := s.CompleteBaseline(ctx, actor, task.Id, snapshots); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	task, err = s.GetTask(ctx, actor, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if beforeStart != nil {
		beforeStart()
	}
	_, err = s.ControlTask(ctx, actor, task.Id, newID("start"), contract.TaskControlRequest{Action: "start", ExpectedVersion: task.Version, Reason: "fixture start"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err = s.GetTask(ctx, actor, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func b4WorkspaceSlotTask(t *testing.T, s *Store, suffix string, groups []string, actor Actor) string {
	t.Helper()
	ctx := context.Background()
	accounts := []string{"acc_b4a" + suffix, "acc_b4b" + suffix, "acc_b4c" + suffix}
	for _, a := range accounts {
		b4SeedAccount(t, s, a, "rt_"+a, 20, actor)
	}
	task := b4ReadyTask(t, s, accounts, groups, 3, 3, "fixture reason", "fixture answer", actor, nil)
	approval := fixtureString(t, s, `SELECT approval_id FROM core.task WHERE id=$1`, task.Id)
	if _, e := s.SetOrchestrationMode(ctx, actor, task.Id, newID("mode"), OrchestrationModeRequest{ExpectedVersion: task.Version, Mode: "proposal", Reason: "b4 workspace fixture"}); e != nil {
		t.Fatal(e)
	}
	task, e := s.GetTask(ctx, actor, task.Id)
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.CreatePlanProposal(ctx, actor, task.Id, newID("prop"), contract.PlanProposalRequest{TaskId: task.Id, ExpectedVersion: task.Version, ScopeRevision: task.ScopeRevision, ProposalKind: "use_template", Reason: "b4 fixture proposal"})
	if e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e = s.Pool.QueryRow(ctx, `SELECT steps FROM core.plan_proposal WHERE id=$1`, r.ResourceId).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var steps []contract.PlanStep
	if e = json.Unmarshal(raw, &steps); e != nil {
		t.Fatal(e)
	}
	template := steps[0]
	steps = nil
	for i, a := range accounts {
		step := template
		step.StepNo = i + 1
		step.AccountId = nullable.NewNullableWithValue(contract.Id(a))
		step.TargetId = nullable.NewNullableWithValue(contract.Id(fixtureString(t, s, `SELECT id FROM core.target WHERE external_id=$1`, groups[i])))
		step.Parameters.GroupId = nullable.NewNullableWithValue(groups[i])
		deps := []int{}
		step.DependsOn = &deps
		steps = append(steps, step)
	}
	raw, e = json.Marshal(steps)
	if e != nil {
		t.Fatal(e)
	}
	// Preserve the proposal digest's canonical envelope after fixture authorship.
	var p contract.PlanProposal
	p.SchemaId = contract.PlanProposalV1
	p.TaskId = task.Id
	p.ExpectedTaskVersion = task.Version
	p.ScopeRevision = task.ScopeRevision
	p.ProposalKind = "use_template"
	p.Steps = steps
	p.Questions = []string{}
	p.RationaleSummary = fixtureString(t, s, `SELECT rationale_summary FROM core.plan_proposal WHERE id=$1`, r.ResourceId)
	d, e := digest(p)
	if e != nil {
		t.Fatal(e)
	}
	authored := newID("proposal")
	if _, e = s.Pool.Exec(ctx, `INSERT INTO core.plan_proposal(workspace_id,id,task_id,principal_id,expected_task_version,scope_revision,template,proposal_kind,state,digest,steps,questions,rationale_summary,reason,expires_at,created_at) SELECT workspace_id,$4,task_id,principal_id,expected_task_version,scope_revision,template,proposal_kind,'pending',$3,$2,questions,rationale_summary,reason,expires_at,created_at FROM core.plan_proposal WHERE id=$1`, r.ResourceId, raw, d, authored); e != nil {
		t.Fatal(e)
	}
	r.ResourceId = authored
	if _, e = s.ApprovePlanProposal(ctx, actor, task.Id, r.ResourceId, newID("approve"), PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: approval}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.MaterializePlanProposal(ctx, actor, task.Id, r.ResourceId, newID("materialize"), MaterializeRequest{ExpectedVersion: task.Version, ApprovalRef: approval, ProposalDigest: d}); e != nil {
		t.Fatal(e)
	}
	return task.Id
}

func TestB4WorkspaceConsumers(t *testing.T) {
	s := fixtureStore(t)
	ctx, c := context.WithTimeout(context.Background(), 12*time.Second)
	defer c()
	other := fixtureActor()
	other.WorkspaceID = "ws_b4_other"
	other.PrincipalID = "principal_other"
	if _, e := s.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES($1);`, other.WorkspaceID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id) VALUES($1,$2)`, other.WorkspaceID, other.PrincipalID); e != nil {
		t.Fatal(e)
	}
	first := b4WorkspaceSlotTask(t, s, "_first", []string{"9015101", "9015102", "9015103"}, fixtureActor())
	second := b4WorkspaceSlotTask(t, s, "_second", []string{"9015201", "9015202", "9015203"}, other)
	if _, e := s.GetTask(ctx, fixtureActor(), second); e == nil {
		t.Fatal("cross workspace task read allowed")
	}
	a := &b4SlotBarrier{entered: make(chan Reservation, 6), release: make(chan struct{})}
	defer b4Close(a.release)
	w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
	done := make(chan error, 1)
	go func() { _, e := w.Tick(ctx); done <- e }()
	r1 := b4Wait(t, a.entered, ctx)
	r2 := b4Wait(t, a.entered, ctx)
	if r1.WorkspaceID == r2.WorkspaceID || r1.TaskID == r2.TaskID {
		t.Fatal("workspace consumers serialized", r1, r2)
	}
	for _, r := range []Reservation{r1, r2} {
		if (r.TaskID == first) != (r.WorkspaceID == "ws_fixture") {
			t.Fatal("wrong workspace", r)
		}
		bad := runtimeActorFor(r)
		if bad.WorkspaceID == "ws_fixture" {
			bad.WorkspaceID = other.WorkspaceID
		} else {
			bad.WorkspaceID = "ws_fixture"
		}
		if _, e := s.AcceptOperation(ctx, bad, r); e == nil {
			t.Fatal("cross workspace authorization allowed")
		}
	}
	b4Close(a.release)
	b4Done(t, done, ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.max != 2 || a.active != 0 || len(a.events) != 12 {
		t.Fatal(a.max, a.active, a.events)
	}
	t.Logf("workspace consumers independent max=2, six real work items; cross-workspace task read and operation authorization rejected; tasks=%s,%s", first, second)
}
func TestB4SnapshotDefaultTimeline(t *testing.T) {
	s := fixtureStore(t)
	s.WorkSlotsDefault = 2
	actor := fixtureActor()
	b4SeedAccount(t, s, "acc_timeline", "rt_timeline", 20, actor)
	old := b4ReadyTask(t, s, []string{"acc_timeline"}, []string{"9015301"}, 1, 3, "fixture reason", "fixture answer", actor, nil)
	s.WorkSlotsDefault = 3
	next := b4ReadyTask(t, s, []string{"acc_timeline"}, []string{"9015302"}, 1, 3, "fixture reason", "fixture answer", actor, func() {
		if n := scalarInt(t, s, `SELECT count(*) FROM core.task WHERE state='READY' AND work_slots IS NULL`); n != 1 {
			t.Fatal("pre-start already snapshotted", n)
		}
		s.WorkSlotsDefault = 1
	})
	snap := func(store *Store, id string) int {
		return scalarInt(t, store, `SELECT work_slots FROM core.task WHERE id=$1`, id)
	}
	if snap(s, old.Id) != 2 || snap(s, next.Id) != 1 {
		t.Fatal("first start default timeline")
	}
	b4Control(t, s, old.Id, "pause")
	b4Control(t, s, old.Id, "resume")
	if snap(s, old.Id) != 2 {
		t.Fatal("resume changed snapshot")
	}
	dsn := s.Pool.Config().ConnString()
	s.Close()
	reopened, e := Open(context.Background(), dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	reopened.WorkSlotsDefault = 1
	if snap(reopened, old.Id) != 2 || snap(reopened, next.Id) != 1 {
		t.Fatal("reopen changed snapshots")
	}
	t.Log("before start default 3->1 captured 1; old running=2, pause/resume=2, close/reopen=2; new task=1")
}
