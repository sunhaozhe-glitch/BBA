package taskengine

import (
	"bba.local/taskengine/internal/contract"
	"context"
	"encoding/json"
	"fmt"
	"github.com/oapi-codegen/nullable"
	"sync"
	"testing"
	"time"
)

// Three explicitly authorized independent account/target pairs. Only proposal
// authorship is fixture data; approval, materialization and execution are real.
func b4SlotTask(t *testing.T, s *Store) string {
	return b4SlotTaskNamed(t, s, "", []string{"9014101", "9014102", "9014103"})
}
func b4SlotTaskNamed(t *testing.T, s *Store, suffix string, groups []string) string {
	t.Helper()
	ctx := context.Background()
	accounts := []string{"acc_b4a" + suffix, "acc_b4b" + suffix, "acc_b4c" + suffix}
	for _, a := range accounts {
		seedAccount(t, s, a, "rt_"+a, 20)
	}
	task := createReadyTask(t, s, accounts, groups, 3, 3)
	approval := fixtureString(t, s, `SELECT approval_id FROM core.task WHERE id=$1`, task.Id)
	proposalWorkMode(t, s, task.Id, "b4 independent fixture")
	task, e := s.GetTask(ctx, fixtureActor(), task.Id)
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.CreatePlanProposal(ctx, fixtureActor(), task.Id, newID("prop"), contract.PlanProposalRequest{TaskId: task.Id, ExpectedVersion: task.Version, ScopeRevision: task.ScopeRevision, ProposalKind: "use_template", Reason: "b4 fixture proposal"})
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
	if _, e = s.ApprovePlanProposal(ctx, fixtureActor(), task.Id, r.ResourceId, newID("approve"), PlanProposalApprovalRequest{ExpectedVersion: task.Version, ApprovalRef: approval}); e != nil {
		t.Fatal(e)
	}
	proposalWorkMaterialize(t, s, task, approval, r.ResourceId, d)
	return task.Id
}

type b4SlotBarrier struct {
	mu          sync.Mutex
	entered     chan Reservation
	release     chan struct{}
	active, max int
	events      []string
}

func (a *b4SlotBarrier) RequestJoin(ctx context.Context, r Reservation) (SendOutcome, error) {
	a.mu.Lock()
	a.active++
	if a.active > a.max {
		a.max = a.active
	}
	a.events = append(a.events, "start:"+r.AccountID+":"+r.TargetID)
	a.mu.Unlock()
	a.entered <- r
	var e error
	select {
	case <-a.release:
	case <-ctx.Done():
		e = ctx.Err()
	}
	a.mu.Lock()
	a.active--
	a.events = append(a.events, "end:"+r.AccountID)
	a.mu.Unlock()
	return SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "offline_b4_barrier"}, e
}
func TestB4SlotConsumerBarrier(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s := fixtureStore(t)
			s.WorkSlotsDefault = n
			task := b4SlotTask(t, s)
			a := &b4SlotBarrier{entered: make(chan Reservation, 3), release: make(chan struct{})}
			w := proposalWorkWorker(t, s, a, WorkerSteps{Proposal: true})
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			done := make(chan error, 1)
			var tick WorkerTick
			go func() { var e error; tick, e = w.Tick(ctx); done <- e }()
			released := false
			defer func() {
				if !released {
					close(a.release)
				}
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("worker failed to join")
				}
			}()
			for i := 0; i < n; i++ {
				select {
				case <-a.entered:
				case <-ctx.Done():
					t.Fatalf("slot %d/%d not entered", i+1, n)
				}
			}
			// Real DB handshake: all admitted slots reached adapter; no queued claims.
			if got := scalarInt(t, s, `SELECT count(*) FROM core.work_item WHERE task_id=$1 AND state='READY'`, task); got != 3-n {
				t.Fatalf("READY=%d want %d", got, 3-n)
			}
			if got := scalarInt(t, s, `SELECT count(*) FROM core.work_item WHERE task_id=$1 AND state='CLAIMED'`, task); got != 0 {
				t.Fatalf("preclaimed %d", got)
			}
			other, e := acquireTaskLease(ctx, s.Pool, "ws_fixture", task)
			if e != nil {
				t.Fatal(e)
			}
			if other != nil {
				other.release()
				t.Fatal("second coordinator admitted")
			}
			select {
			case r := <-a.entered:
				t.Fatalf("overflow entered: %+v", r)
			default:
			}
			close(a.release)
			released = true
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
				done <- nil
			case <-ctx.Done():
				t.Fatal("no progress after release")
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.active != 0 || a.max != n {
				t.Fatalf("active=%d max=%d want=%d", a.active, a.max, n)
			}
			if tick.ProposalDispatched != 3 {
				t.Fatalf("tick=%+v", tick)
			}
			data, _ := json.Marshal(map[string]any{"slots": n, "maximum_inflight": a.max, "final_inflight": a.active, "events": a.events, "tick": tick})
			t.Log(string(data))
		})
	}
}
