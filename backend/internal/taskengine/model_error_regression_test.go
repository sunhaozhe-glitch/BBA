package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestModelProviderErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"plain network", errors.New("connection refused"), modelCodeProviderFailed},
		{"plain store spelling is not authority", errors.New(taskCodeStoreUnavailable), modelCodeProviderFailed},
		{"wrapped network", fmt.Errorf("provider: %w", errors.New("offline")), modelCodeProviderFailed},
		{"agent domain", problem("AGENT_REFERENCE_UNAVAILABLE"), "AGENT_REFERENCE_UNAVAILABLE"},
		{"wrapped skill version", fmt.Errorf("bound: %w", problem("SKILL_VERSION_UNAVAILABLE")), "SKILL_VERSION_UNAVAILABLE"},
		{"wrapped model domain", fmt.Errorf("bound: %w", problem("MODEL_REFERENCE_UNAVAILABLE")), "MODEL_REFERENCE_UNAVAILABLE"},
		{"wrapped store domain", fmt.Errorf("persist: %w", problem(taskCodeStoreUnavailable)), taskCodeStoreUnavailable},
		{"wrapped conflict", fmt.Errorf("capacity: %w", problem(taskCodeStateConflict)), taskCodeStateConflict},
		{"unknown domain", problem("UNKNOWN_PROVIDER_CODE"), modelCodeProviderFailed},
		{"wrapped schema", fmt.Errorf("validation: %w", errModelSchemaInvalid), modelCodeSchemaInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := modelProviderErrorCode(c.err); got != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
	// The shared storage API retains its existing semantics; this change is local.
	if Code(errors.New("ordinary store caller")) != taskCodeStoreUnavailable {
		t.Fatal("global Code semantics changed")
	}
}

func TestModelFailureReplayLedgerAndBudgetClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ordinary", errors.New("offline fixture"), modelCodeProviderFailed},
		{"wrapped domain", fmt.Errorf("binding: %w", problem("SKILL_VERSION_UNAVAILABLE")), "SKILL_VERSION_UNAVAILABLE"},
		{"store domain", fmt.Errorf("persist: %w", problem(taskCodeStoreUnavailable)), taskCodeStoreUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := fixtureStore(t)
			task := modelTestTask(t, s, 4)
			actor := modelTestActor()
			ctx := context.Background()
			p := &fixedModelProvider{err: c.err}
			for i := 0; i < 3; i++ {
				req := modelTestRequest(task)
				req.RequestId = fmt.Sprintf("failure-%d", i)
				key := req.RequestId
				for replay := 0; replay < 2; replay++ {
					if _, err := s.RunPlanModelRequest(ctx, actor, key, req, p); Code(err) != c.want {
						t.Fatalf("i=%d replay=%d err=%v want=%s", i, replay, err, c.want)
					}
					if p.calls.Load() != int32(i+1) {
						t.Fatalf("replay called provider: %d", p.calls.Load())
					}
				}
				if got := scalarInt(t, s, `SELECT count(*) FROM core.model_request WHERE task_id=$1 AND state='failed' AND error_code=$2 AND attempts=1 AND response IS NULL AND completed_at IS NOT NULL`, task.Id, c.want); got != i+1 {
					t.Fatalf("ledger rows=%d", got)
				}
			}
			if _, err := s.RunPlanModelRequest(ctx, actor, "fourth", modelTestRequest(task), p); Code(err) != modelCodeLocalBudgetExhausted {
				t.Fatal("budget", err)
			}
			if p.calls.Load() != 3 || scalarInt(t, s, `SELECT count(*) FROM core.model_request WHERE task_id=$1`, task.Id) != 3 {
				t.Fatal("budget or ledger changed")
			}
		})
	}
}

func TestBoundAgentRealPersistenceFailureReplay(t *testing.T) {
	s := fixtureStore(t)
	task := modelTestTask(t, s, 3)
	actor := modelTestActor()
	r, _, skill, agent := configFixture(t)
	ws := actor.WorkspaceID
	r.mu.Lock()
	r.data.Workspaces[ws] = r.data.Workspaces["ws"]
	r.data.Agents[ws] = r.data.Agents["ws"]
	r.data.Skills[ws] = r.data.Skills["ws"]
	r.data.AgentHistory[configKey(ws, agent.ID)] = r.data.AgentHistory[configKey("ws", agent.ID)]
	r.data.SkillHistory[configKey(ws, skill.ID)] = r.data.SkillHistory[configKey("ws", skill.ID)]
	before, _ := json.Marshal(r.data)
	// A genuine filesystem persist failure, not a synthetic provider error. The
	// adapter must never reach the TLS URL because receipt durability fails first.
	r.path = filepath.Join(t.TempDir(), "missing-directory", "registry.json")
	r.mu.Unlock()
	ref := SkillBinding{agent.ID, agent.Version}
	p := boundAgentProvider{r, ws, actor.PrincipalID, "store-failure", ref}
	req := modelTestRequest(task)
	for i := 0; i < 2; i++ {
		if _, e := s.runBoundPlanModelRequest(context.Background(), actor, "store-failure", req, p, &ref); Code(e) != taskCodeStoreUnavailable {
			t.Fatal(e)
		}
	}
	r.mu.Lock()
	after, _ := json.Marshal(r.data)
	active := r.active[ws]
	r.mu.Unlock()
	if string(before) != string(after) || active != 0 {
		t.Fatal("persist failure published state or acquired capacity")
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.model_request WHERE task_id=$1 AND state='failed' AND error_code='STATE_STORE_UNAVAILABLE' AND attempts=1`, task.Id); got != 1 {
		t.Fatalf("storage replay ledger=%d", got)
	}
}

func TestReplaySemanticDigestPreservesNumbersAndAllFields(t *testing.T) {
	a := json.RawMessage(`{"result":{"n":9007199254740993,"decimal":0.123456789012345678901},"binding":{"version":2},"usage":3}`)
	b := json.RawMessage(`{"usage":3,"binding":{"version":2},"result":{"decimal":0.123456789012345678901,"n":9007199254740993}}`)
	da, e := digest(a)
	if e != nil {
		t.Fatal(e)
	}
	db, e := digest(b)
	if e != nil || da != db {
		t.Fatal("JSON object order matters", e)
	}
	for _, raw := range []string{
		`{"result":{"n":9007199254740992,"decimal":0.123456789012345678901},"binding":{"version":2},"usage":3}`,
		`{"result":{"n":9007199254740993,"decimal":0.123456789012345678902},"binding":{"version":2},"usage":3}`,
		`{"result":{"n":9007199254740993,"decimal":0.123456789012345678901},"binding":{"version":1},"usage":3}`,
		`{"result":{"n":9007199254740993,"decimal":0.123456789012345678901},"binding":{"version":2},"usage":4}`,
	} {
		got, e := digest(json.RawMessage(raw))
		if e != nil || got == da {
			t.Fatal("field/precision change lost", raw, e)
		}
	}
}
