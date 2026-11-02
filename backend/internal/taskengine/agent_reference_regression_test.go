package taskengine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBoundAgentInvalidRuntimeReferencesNeverCallProvider(t *testing.T) {
	cases := []struct {
		name, code string
		mutate     func(*ModelRegistry, string, ModelProfile, SkillProfile, AgentProfile)
	}{
		{"disabled model", "MODEL_REFERENCE_UNAVAILABLE", func(r *ModelRegistry, ws string, m ModelProfile, s SkillProfile, a AgentProfile) {
			r.data.Workspaces[ws][0].Enabled = false
		}},
		{"cleared model secret", "MODEL_REFERENCE_UNAVAILABLE", func(r *ModelRegistry, ws string, m ModelProfile, s SkillProfile, a AgentProfile) {
			r.data.Workspaces[ws][0].APIKey = ""
		}},
		{"missing model", "MODEL_REFERENCE_UNAVAILABLE", func(r *ModelRegistry, ws string, m ModelProfile, s SkillProfile, a AgentProfile) {
			r.data.Workspaces[ws] = nil
		}},
		{"disabled skill", "SKILL_REFERENCE_UNAVAILABLE", func(r *ModelRegistry, ws string, m ModelProfile, s SkillProfile, a AgentProfile) {
			r.data.Skills[ws][0].Enabled = false
		}},
		{"missing skill", "SKILL_REFERENCE_UNAVAILABLE", func(r *ModelRegistry, ws string, m ModelProfile, s SkillProfile, a AgentProfile) {
			r.data.Skills[ws] = nil
		}},
		{"missing historical skill", "SKILL_VERSION_UNAVAILABLE", func(r *ModelRegistry, ws string, m ModelProfile, s SkillProfile, a AgentProfile) {
			r.data.SkillHistory[configKey(ws, s.ID)] = nil
		}},
		{"unpublished historical skill", "SKILL_VERSION_UNAVAILABLE", func(r *ModelRegistry, ws string, m ModelProfile, s SkillProfile, a AgentProfile) {
			r.data.SkillHistory[configKey(ws, s.ID)][0].Published = false
		}},
		{"missing historical agent", "AGENT_REFERENCE_UNAVAILABLE", func(r *ModelRegistry, ws string, m ModelProfile, s SkillProfile, a AgentProfile) {
			r.data.AgentHistory[configKey(ws, a.ID)] = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := fixtureStore(t)
			task := modelTestTask(t, store, 3)
			actor := modelTestActor()
			r, m, s, a := configFixture(t)
			ws := actor.WorkspaceID
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			r.mu.Lock()
			r.client = server.Client()
			r.data.Workspaces[ws] = r.data.Workspaces["ws"]
			r.data.Workspaces[ws][0].BaseURL = server.URL + "/v1"
			r.data.Skills[ws] = r.data.Skills["ws"]
			r.data.Agents[ws] = r.data.Agents["ws"]
			r.data.AgentHistory[configKey(ws, a.ID)] = r.data.AgentHistory[configKey("ws", a.ID)]
			r.data.SkillHistory[configKey(ws, s.ID)] = r.data.SkillHistory[configKey("ws", s.ID)]
			tc.mutate(r, ws, m, s, a)
			r.mu.Unlock()
			ref := SkillBinding{a.ID, a.Version}
			p := boundAgentProvider{r, ws, actor.PrincipalID, "invalid-reference", ref}
			req := modelTestRequest(task)
			for i := 0; i < 2; i++ {
				if _, err := store.runBoundPlanModelRequest(context.Background(), actor, "invalid-reference", req, p, &ref); Code(err) != tc.code {
					t.Fatalf("replay=%d err=%v want=%s", i, err, tc.code)
				}
			}
			if calls.Load() != 0 {
				t.Fatal("invalid binding invoked TLS provider")
			}
			if n := scalarInt(t, store, `SELECT count(*) FROM core.model_request WHERE task_id=$1 AND state='failed' AND error_code=$2 AND attempts=1`, task.Id, tc.code); n != 1 {
				t.Fatal("invalid binding replay ledger", n)
			}
		})
	}
}

func TestModelRequestActualDatabaseFailureDoesNotBecomeProviderFailure(t *testing.T) {
	s := fixtureStore(t)
	task := modelTestTask(t, s, 3)
	p := &fixedModelProvider{}
	s.Pool.Close() // This fixture's actual database pool is no longer usable.
	if _, err := s.RunPlanModelRequest(context.Background(), modelTestActor(), "db-failure", modelTestRequest(task), p); Code(err) != taskCodeStoreUnavailable {
		t.Fatal("actual store failure", err)
	}
	if p.calls.Load() != 0 {
		t.Fatal("provider invoked despite failed ledger reservation")
	}
}
