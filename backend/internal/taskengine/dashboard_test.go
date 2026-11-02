package taskengine

import (
	"bba.local/taskengine/internal/contract"
	"context"
	"encoding/json"
	"github.com/oapi-codegen/nullable"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDashboardProjectionFreshStaleUnknownDuplicates(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	base := contract.Account{Id: "a", RuntimeId: "r", ConnectionState: "online", LoginState: "logged_in", RiskState: "clear", SelfId: nullable.NewNullableWithValue("10001"), HeartbeatAt: nullable.NewNullableWithValue(now)}
	a := Actor{Permissions: map[string]bool{"task.manage_all": true}}
	for _, tc := range []struct {
		name   string
		item   contract.Account
		status string
	}{{"fresh", base, "online"}, {"stale", func() contract.Account { v := base; v.HeartbeatAt.Set(old); return v }(), "abnormal"}, {"offline", func() contract.Account { v := base; v.ConnectionState = "offline"; return v }(), "offline"}, {"unknown", func() contract.Account { v := base; v.ConnectionState = "alien"; return v }(), "abnormal"}} {
		t.Run(tc.name, func(t *testing.T) {
			items, _, _, e := ProjectionAccounts(context.Background(), a, []contract.Account{tc.item})
			if e != nil || items[0].Status != tc.status {
				t.Fatalf("%+v %v", items, e)
			}
		})
	}
	items := []DashboardAccount{{ID: "a", SelfID: "1", Status: "online"}, {ID: "b", SelfID: "1", Status: "offline"}, {ID: "c", SelfID: "2", Status: "offline"}}
	v := AccountSummary(items)
	counts := v["counts"].(map[string]int)
	if counts["online"] != 0 || counts["abnormal"] != 2 || counts["offline"] != 1 || v["unique_known_accounts"] != 2 {
		t.Fatal(v)
	}
	items, _, reason, _ := ProjectionAccounts(context.Background(), Actor{}, []contract.Account{base})
	if len(items) != 0 || reason != "ACCOUNT_SCOPE_MAPPING_REQUIRED" {
		t.Fatal("ordinary operator leaked accounts")
	}
}
func TestDashboardScopedDatabaseAndAuth(t *testing.T) {
	s := fixtureStore(t)
	a := fixtureActor()
	a.Permissions["operator.read"] = true
	seedAccount(t, s, "dash_a", "dash_r", 5)
	task := createReadyTask(t, s, []string{"dash_a"}, []string{"90001234"}, 1, 1)
	ctx := context.Background()
	detail, de := s.GetTask(ctx, a, task.Id)
	if de != nil {
		t.Fatal(de)
	}
	if detail.CreatedAt.Location() != time.UTC || detail.DeadlineAt.Location() != time.UTC {
		t.Fatal("task detail timestamps must be UTC")
	}
	d, e := s.Dashboard(ctx, a, nil)
	if e != nil {
		t.Fatal(e)
	}
	if d["tasks"].(map[string]any)["total"] != 1 {
		t.Fatal(d)
	}
	if d["customers"].(DashboardMetric).Value != nil || d["tokens"].(map[string]any)["total"] != nil {
		t.Fatal("missing data fabricated")
	}
	accounts := d["accounts"].(map[string]any)
	if accounts["total"] != 1 {
		t.Fatal(accounts)
	}
	other := a
	other.PrincipalID = "other_dash"
	other.Permissions = map[string]bool{"operator.read": true, "task.read": true}
	if _, e = s.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id) VALUES($1,$2)`, a.WorkspaceID, other.PrincipalID); e != nil {
		t.Fatal(e)
	}
	d, e = s.Dashboard(ctx, other, nil)
	if e != nil {
		t.Fatal(e)
	}
	if d["tasks"].(map[string]any)["total"] != 0 || d["accounts"].(map[string]any)["total"] != nil {
		t.Fatal("cross principal leak")
	}
	foreign := a
	foreign.WorkspaceID = "foreign_ws"
	d, e = s.Dashboard(ctx, foreign, nil)
	if e != nil {
		t.Fatal(e)
	}
	if d["tasks"].(map[string]any)["total"] != 0 || d["accounts"].(map[string]any)["total"] != 0 {
		t.Fatal("cross workspace leak")
	}
	// Seed real persisted response format, not a dashboard demonstration constant.
	_, e = s.Pool.Exec(ctx, `INSERT INTO core.model_request(workspace_id,id,principal_id,task_id,idempotency_key,request_digest,operation_kind,schema_id,prompt_version,input_ref,deadline_at,max_provider_attempts,state,response,completed_at) VALUES($1,'dash_model',$2,$3,'dash-key',repeat('a',64),'complex_plan','PlanProposal.v1','planning.v1',$3,clock_timestamp()+interval '1 hour',1,'completed',$4::jsonb,clock_timestamp())`, a.WorkspaceID, a.PrincipalID, task.Id, `{"usage":{"input_tokens":123,"output_tokens":45}}`)
	if e != nil {
		t.Fatal(e)
	}
	usage, e := s.dashboardTokens(ctx, a)
	if e != nil || usage["total"] != int64(168) {
		t.Fatalf("%+v %v", usage, e)
	}
	usage, e = s.dashboardTokens(ctx, other)
	if e != nil || usage["total"] != nil {
		t.Fatal("model usage leaked")
	}
	h := s.DashboardHandler(func(r *http.Request) (Actor, error) {
		if r.Header.Get("Authorization") == "Bearer good" {
			return a, nil
		}
		return Actor{}, problem("UNAUTHENTICATED")
	}, nil)
	for _, tc := range []struct {
		method, token string
		status        int
	}{{"GET", "", 401}, {"GET", "good", 200}, {"POST", "good", 405}} {
		r := httptest.NewRequest(tc.method, "/v1/operator/dashboard", nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s status %d body %s", tc.method, w.Code, w.Body.String())
		}
		if tc.status == 200 {
			var out map[string]any
			if json.Unmarshal(w.Body.Bytes(), &out) != nil {
				t.Fatal("invalid json")
			}
		}
	}
}
