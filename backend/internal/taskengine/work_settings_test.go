package taskengine

import (
	"bba.local/taskengine/internal/contract"
	"context"
	"encoding/json"
	"fmt"
	"github.com/oapi-codegen/nullable"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func workSettingsActor() Actor {
	a := fixtureActor()
	a.Permissions = map[string]bool{"system.read": true, "system.configure": true}
	return a
}
func workSettingsRequest(s *Store, a Actor, method, path, body string) *httptest.ResponseRecorder {
	h := s.WorkSettingsHandler(func(*http.Request) (Actor, error) { return a, nil }, WorkSettingsRuntime{WorkerEnabled: true, ProposalEnabled: true, Transport: "fixture"})
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func workSettingsSave(t *testing.T, s *Store, a Actor, n int, v int64) WorkSettings {
	t.Helper()
	w := workSettingsRequest(s, a, "PUT", workSettingsPath, fmt.Sprintf(`{"work_slots":%d,"expected_version":%d}`, n, v))
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	var out struct {
		Data WorkSettings `json:"data"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	return out.Data
}
func TestWorkSettingsHTTPPersistenceCASAndScope(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	a := workSettingsActor()
	w := workSettingsRequest(s, a, "GET", workSettingsPath, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"source":"cli_fallback"`) || !strings.Contains(w.Body.String(), `"version":0`) {
		t.Fatal(w.Code, w.Body)
	}
	other, e := Open(ctx, s.Pool.Config().ConnString())
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	b := a
	b.PrincipalID = "principal_second"
	if _, e = s.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','principal_second')`); e != nil {
		t.Fatal(e)
	}
	gate := make(chan struct{})
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for i, store := range []*Store{s, other} {
		wg.Add(1)
		go func(i int, store *Store) {
			defer wg.Done()
			<-gate
			who := a
			if i == 1 {
				who = b
			}
			results <- workSettingsRequest(store, who, "PUT", workSettingsPath, fmt.Sprintf(`{"work_slots":%d,"expected_version":0}`, i+1)).Code
		}(i, store)
	}
	close(gate)
	wg.Wait()
	close(results)
	codes := map[int]int{}
	for code := range results {
		codes[code]++
	}
	if codes[200] != 1 || codes[409] != 1 {
		t.Fatal(codes)
	}
	var principal string
	var old, new, ov, v int
	if e = s.Pool.QueryRow(ctx, `SELECT principal_id,old_work_slots,new_work_slots,old_version,version FROM core.workspace_work_settings_audit`).Scan(&principal, &old, &new, &ov, &v); e != nil || old != 1 || ov != 0 || v != 1 || !((new == 1 && principal == a.PrincipalID) || (new == 2 && principal == b.PrincipalID)) {
		t.Fatal(principal, old, new, ov, v, e)
	}
	a2 := a
	a2.WorkspaceID = "ws_settings_two"
	if _, e = s.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES('ws_settings_two');INSERT INTO core.principal(workspace_id,id) VALUES('ws_settings_two','principal_fixture')`); e != nil {
		t.Fatal(e)
	}
	workSettingsSave(t, s, a, 1, 1)
	workSettingsSave(t, other, a2, 2, 0)
	for _, x := range []struct {
		a Actor
		n int
	}{{a, 1}, {a2, 2}} {
		out, e := other.GetWorkSettings(ctx, x.a)
		if e != nil || out.WorkSlots != x.n {
			t.Fatal(out, e)
		}
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.workspace_work_settings_audit`); got != 3 {
		t.Fatal(got)
	}
	// Runtime flags and source are observations, never accepted writes.
	bad := []string{`{}`, `null`, `[]`, `{"work_slots":2}`, `{"work_slots":null,"expected_version":2}`, `{"work_slots":2,"expected_version":null}`, `{"work_slots":2.0,"expected_version":2}`, `{"work_slots":2e0,"expected_version":2}`, `{"work_slots":"2","expected_version":2}`, `{"work_slots":true,"expected_version":2}`, `{"work_slots":0,"expected_version":2}`, `{"work_slots":9,"expected_version":2}`, `{"work_slots":2,"expected_version":-1}`, `{"work_slots":2,"expected_version":2.0}`, `{"work_slots":2,"expected_version":9223372036854775808}`, `{"work_slots":2,"expected_version":2,"workspace_id":"ws_settings_two"}`, `{"work_slots":2,"expected_version":2,"worker_enabled":true}`, `{"work_slots":2,"work_slots":3,"expected_version":2}`, `{"work_slots":2,"expected_version":2} {}`}
	for _, body := range bad {
		w := workSettingsRequest(s, a, "PUT", workSettingsPath, body)
		if w.Code != 400 {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	for _, method := range []string{"GET", "PUT"} {
		w := workSettingsRequest(s, a, method, workSettingsPath+"?workspace_id=ws_settings_two", `{"work_slots":2,"expected_version":2}`)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
	readonly := a
	readonly.Permissions = map[string]bool{"system.read": true}
	w = workSettingsRequest(s, readonly, "GET", workSettingsPath, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"can_configure":false`) {
		t.Fatal(w.Code, w.Body)
	}
	for _, actor := range []Actor{readonly, fixtureActor(), {WorkspaceID: a.WorkspaceID, PrincipalID: a.PrincipalID, RuntimeID: "runtime", Permissions: a.Permissions}} {
		w = workSettingsRequest(s, actor, "PUT", workSettingsPath, `{"work_slots":2,"expected_version":2}`)
		if w.Code != 403 {
			t.Fatal(w.Code, w.Body)
		}
	}
	unknown := a
	unknown.PrincipalID = "missing"
	w = workSettingsRequest(s, unknown, "PUT", workSettingsPath, `{"work_slots":2,"expected_version":2}`)
	if w.Code != 401 {
		t.Fatal(w.Code, w.Body)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.workspace_work_settings_audit`); got != 3 {
		t.Fatal("invalid request audited", got)
	}
	if s.WorkSlotsDefault != 0 {
		t.Fatal("mutable startup fallback")
	}
	reopened, e := Open(ctx, s.Pool.Config().ConnString())
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	out, e := reopened.GetWorkSettings(ctx, a2)
	if e != nil || out.WorkSlots != 2 || out.Version != 1 {
		t.Fatal(out, e)
	}
}

func TestWorkSettingsAPISavedConsumerOldAndNew(t *testing.T) {
	s := fixtureStore(t)
	a := workSettingsActor()
	workSettingsSave(t, s, a, 1, 0)
	old := b4SlotTaskNamed(t, s, "_settings_old", []string{"9024101", "9024102", "9024103"})
	workSettingsSave(t, s, a, 2, 1)
	next := b4SlotTaskNamed(t, s, "_settings_new", []string{"9024201", "9024202", "9024203"})
	b4Control(t, s, old, "pause")
	b4Control(t, s, old, "resume")
	reopened, e := Open(context.Background(), s.Pool.Config().ConnString())
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	for _, x := range []struct {
		id string
		n  int
	}{{old, 1}, {next, 2}} {
		w := workSettingsRequest(reopened, fixtureActor(), "GET", taskWorkSlotsPath+x.id, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"work_slots":%d`, x.n)) {
			t.Fatal(w.Code, w.Body)
		}
	}
	barrier := &b4SlotBarrier{entered: make(chan Reservation, 6), release: make(chan struct{})}
	defer b4Close(barrier.release)
	worker := proposalWorkWorker(t, reopened, barrier, WorkerSteps{Proposal: true})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := worker.Tick(ctx); done <- e }()
	defer func() {
		b4Close(barrier.release)
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("worker did not exit")
		}
	}()
	counts := map[string]int{}
	for i := 0; i < 3; i++ {
		r := b4Wait(t, barrier.entered, ctx)
		counts[r.TaskID]++
	}
	if counts[old] != 1 || counts[next] != 2 {
		t.Fatal(counts)
	}
	for _, x := range []struct {
		id    string
		ready int
	}{{old, 2}, {next, 1}} {
		if got := scalarInt(t, s, `SELECT count(*) FROM core.work_item WHERE task_id=$1 AND state='READY'`, x.id); got != x.ready {
			t.Fatal(x.id, got)
		}
	}
	select {
	case r := <-barrier.entered:
		t.Fatal("overflow", r)
	default:
	}
	b4Close(barrier.release)
	b4Done(t, done, ctx)
	done <- nil
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	if barrier.max != 3 || barrier.active != 0 || len(barrier.events) != 12 {
		t.Fatal(barrier.max, barrier.active, barrier.events)
	}
}

// SQL lock observer: no timing-only assumption that a concurrent save/start is waiting.
func workSettingsWaitLock(t *testing.T, s *Store, ctx context.Context, sqlFragment string) {
	t.Helper()
	for {
		var waiting bool
		e := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%' || $1 || '%')`, sqlFragment).Scan(&waiting)
		if e != nil {
			t.Fatal(e)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestWorkSettingsStartSaveLinearization(t *testing.T) {
	for _, saveFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(saveFirst), func(t *testing.T) {
			s := fixtureStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			a := workSettingsActor()
			seedAccount(t, s, "acc_order", "rt_order", 20)
			task := workSettingsReadyTask(t, s, []string{"acc_order"}, []string{"9024301"}, 1, 2, "fixture reason", "fixture answer", fixtureActor())
			w := workSettingsRequest(s, fixtureActor(), "GET", taskWorkSlotsPath+task.Id, "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"work_slots":null`) {
				t.Fatal(w.Code, w.Body)
			}
			tx, e := s.Pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			if e = workSettingsLock(ctx, tx, a.WorkspaceID); e != nil {
				t.Fatal(e)
			}
			saved := make(chan *httptest.ResponseRecorder, 1)
			started := make(chan error, 1)
			save := func() {
				saved <- workSettingsRequest(s, a, "PUT", workSettingsPath, `{"work_slots":2,"expected_version":0}`)
			}
			start := func() {
				_, e := s.ControlTask(ctx, fixtureActor(), task.Id, newID("start"), contract.TaskControlRequest{Action: "start", ExpectedVersion: task.Version, Reason: "ordered start"})
				started <- e
			}
			if saveFirst {
				go save()
			} else {
				go start()
			}
			workSettingsWaitLock(t, s, ctx, "pg_advisory_xact_lock")
			if saveFirst {
				go start()
			} else {
				go save()
			}
			for {
				var n int
				if e = s.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&n); e != nil {
					t.Fatal(e)
				}
				if n == 2 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(5 * time.Millisecond):
				}
			}
			if e = tx.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			select {
			case e := <-started:
				if e != nil {
					t.Fatal(e)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case w := <-saved:
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			want := 1
			if saveFirst {
				want = 2
			}
			snapshot, e := s.GetTaskWorkSlots(ctx, fixtureActor(), task.Id)
			if e != nil || snapshot.WorkSlots == nil || *snapshot.WorkSlots != want {
				t.Fatal(snapshot, e, want)
			}
		})
	}
}

func workSettingsReadyTask(t *testing.T, s *Store, accounts, groups []string, goal, maxAttempts int, reason, answer string, actor Actor) contract.Task {
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
	return task
}
