package taskengine

// Phase A: read-only projections, never a second task engine.
import (
	"bba.local/taskengine/internal/contract"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

type DashboardAccount struct {
	ID         string     `json:"id"`
	InstanceID string     `json:"instance_id"`
	SelfID     string     `json:"self_id,omitempty"`
	Status     string     `json:"status"`
	Reason     string     `json:"reason"`
	ObservedAt *time.Time `json:"observed_at"`
}
type DashboardAccountSource func(context.Context, Actor, []contract.Account) ([]DashboardAccount, string, string, error)
type DashboardMetric struct {
	Availability string `json:"availability"`
	Reason       string `json:"reason"`
	Value        any    `json:"value"`
}

func unavailable(reason string) DashboardMetric { return DashboardMetric{"unavailable", reason, nil} }

// ProjectionAccounts intentionally requires workspace-manager authority. The
// legacy ListAccounts API is workspace-wide; ordinary operators are NOT given
// that whole list here. The local6300 adapter additionally requires explicit
// operator/workspace/account/instance bindings even for managers.
func ProjectionAccounts(ctx context.Context, a Actor, items []contract.Account) ([]DashboardAccount, string, string, error) {
	if !a.Permissions["task.manage_all"] {
		return nil, "projection", "ACCOUNT_SCOPE_MAPPING_REQUIRED", nil
	}
	now := time.Now().UTC()
	out := make([]DashboardAccount, 0, len(items))
	for _, v := range items {
		x := DashboardAccount{ID: v.Id, InstanceID: v.RuntimeId, Status: "abnormal", Reason: "UNKNOWN_STATE"}
		if v.SelfId.IsSpecified() && !v.SelfId.IsNull() {
			x.SelfID = v.SelfId.MustGet()
		}
		if v.HeartbeatAt.IsSpecified() && !v.HeartbeatAt.IsNull() {
			t := v.HeartbeatAt.MustGet()
			x.ObservedAt = &t
		}
		if x.ObservedAt == nil || now.Sub(*x.ObservedAt) > 90*time.Second || x.ObservedAt.After(now.Add(5*time.Second)) {
			x.Reason = "HEARTBEAT_STALE_OR_UNKNOWN"
		} else if v.ConnectionState == "offline" || v.LoginState == "logged_out" {
			x.Status = "offline"
			x.Reason = "OFFLINE"
		} else if v.ConnectionState == "online" && v.LoginState == "logged_in" && v.RiskState == "clear" {
			x.Status = "online"
			x.Reason = "FRESH_HEARTBEAT"
		}
		out = append(out, x)
	}
	return out, "projection", "", nil
}

// AccountSummary counts registered instances, with mutually exclusive states.
// Same-QQ instances are marked as conflicts, not silently double-counted QQs.
func AccountSummary(items []DashboardAccount) map[string]any {
	seen := map[string]int{}
	unique := map[string]bool{}
	for _, a := range items {
		if a.SelfID != "" {
			seen[a.SelfID]++
			unique[a.SelfID] = true
		}
	}
	counts := map[string]int{"online": 0, "offline": 0, "abnormal": 0}
	conflicts := 0
	for i := range items {
		a := &items[i]
		if a.SelfID != "" && seen[a.SelfID] > 1 {
			a.Status = "abnormal"
			a.Reason = "DUPLICATE_QQ_INSTANCE"
			conflicts++
		}
		if _, ok := counts[a.Status]; !ok {
			a.Status = "abnormal"
			a.Reason = "UNKNOWN_STATE"
		}
		counts[a.Status]++
	}
	return map[string]any{"availability": "available", "reason": "", "instances": items, "counts": counts, "total": len(items), "unique_known_accounts": len(unique), "conflict_instances": conflicts, "counting": "registered_instances; duplicate QQ instances are abnormal"}
}

func (s *Store) DashboardHandler(auth func(*http.Request) (Actor, error), source DashboardAccountSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		actor, err := (&taskHTTP{auth: auth}).authenticate(r)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		if actor.RuntimeID != "" || !actor.Permissions["operator.read"] || !actor.Permissions["task.read"] {
			thWriteError(w, r, problem(taskCodeForbidden))
			return
		}
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, "GET")
			return
		}
		data, err := s.Dashboard(r.Context(), actor, source)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		thWriteJSON(w, 200, map[string]any{"data": data, "meta": thMeta(r)})
	})
}

func (s *Store) Dashboard(ctx context.Context, actor Actor, source DashboardAccountSource) (map[string]any, error) {
	if actor.RuntimeID != "" {
		return nil, problem(taskCodeForbidden)
	}
	if err := actor.require("operator.read"); err != nil {
		return nil, err
	}
	if err := actor.require("task.read"); err != nil {
		return nil, err
	}
	tasks := []contract.Task{}
	cursor := ""
	for {
		page, next, more, err := s.ListTasks(ctx, actor, cursor, 100)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, page...)
		if !more {
			break
		}
		if len(tasks) >= 10000 {
			return nil, problem(taskCodeStoreUnavailable)
		}
		cursor = next
	}
	accounts := []contract.Account{}
	cursor = ""
	for {
		page, next, more, err := s.ListAccounts(ctx, actor, cursor, 100)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, page...)
		if !more {
			break
		}
		if len(accounts) >= 10000 {
			return nil, problem(taskCodeStoreUnavailable)
		}
		cursor = next
	}
	if source == nil {
		source = ProjectionAccounts
	}
	projected, sourceName, reason, err := source(ctx, actor, accounts)
	var accountData map[string]any
	if err != nil || reason != "" {
		if reason == "" {
			reason = "ACCOUNT_SOURCE_UNAVAILABLE"
		}
		accountData = map[string]any{"availability": "unavailable", "reason": reason, "total": nil, "counts": nil, "instances": nil}
	} else {
		accountData = AccountSummary(projected)
	}
	accountData["source"] = sourceName
	accountData["cache_age_seconds"] = 0
	accountData["observed_at"] = time.Now().UTC()
	active := 0
	byState := map[string]int{}
	for _, t := range tasks {
		byState[string(t.State)]++
		switch t.State {
		case "RUNNING", "WAITING_EXTERNAL", "VERIFYING", "PAUSING", "RECONCILING":
			active++
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	recent := tasks
	if len(recent) > 5 {
		recent = recent[:5]
	}
	token, err := s.dashboardTokens(ctx, actor)
	if err != nil {
		return nil, err
	}
	return map[string]any{"scope": map[string]any{"workspace_id": actor.WorkspaceID, "principal_id": actor.PrincipalID, "tasks": "existing ListTasks authority"}, "tasks": map[string]any{"total": len(tasks), "active": active, "by_state": byState, "recent": recent}, "accounts": accountData, "customers": unavailable("CRM_INTENT_EVIDENCE_AND_FRIEND_RELATION_NOT_IMPLEMENTED"), "assets": unavailable("CUSTOMER_STATUS_AND_SOURCE_PROJECTION_NOT_IMPLEMENTED"), "tokens": token, "system": map[string]any{"database": "available", "qq_events": unavailable("NO_SCOPED_EVENT_HEALTH_SOURCE"), "automatic_intent_rules": "not_configured"}, "generated_at": time.Now().UTC(), "cache_age_seconds": 0}, nil
}
func (s *Store) dashboardTokens(ctx context.Context, a Actor) (map[string]any, error) {
	// Only completed, persisted model responses owned by this operator, and only
	// tasks still in this workspace. This is planning-gateway usage, NOT all LLMs.
	rows, err := s.Pool.Query(ctx, `SELECT m.created_at,m.response FROM core.model_request m JOIN core.task t ON t.workspace_id=m.workspace_id AND t.id=m.task_id WHERE m.workspace_id=$1 AND m.principal_id=$2 AND m.state='completed' ORDER BY m.created_at`, a.WorkspaceID, a.PrincipalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type bucket struct {
		Date   string `json:"date"`
		Input  int64  `json:"input"`
		Output int64  `json:"output"`
	}
	daily := map[string]*bucket{}
	var input, output int64
	records := 0
	invalid := 0
	for rows.Next() {
		var at time.Time
		var raw []byte
		if err := rows.Scan(&at, &raw); err != nil {
			return nil, err
		}
		var v struct {
			Usage struct {
				Input  *int64 `json:"input_tokens"`
				Output *int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Usage.Input == nil || v.Usage.Output == nil || *v.Usage.Input < 0 || *v.Usage.Output < 0 {
			invalid++
			continue
		}
		records++
		input += *v.Usage.Input
		output += *v.Usage.Output
		day := at.UTC().Format("2006-01-02")
		if daily[day] == nil {
			daily[day] = &bucket{Date: day}
		}
		daily[day].Input += *v.Usage.Input
		daily[day].Output += *v.Usage.Output
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := map[string]any{"availability": "unavailable", "reason": "NO_RECORDED_MODEL_USAGE", "input": nil, "output": nil, "total": nil, "daily": nil, "source": "core.model_request.response.usage", "scope": "current operator planning gateway only; not global LLM billing", "cache_read": nil, "cache_write": nil}
	if invalid > 0 {
		result["reason"] = fmt.Sprintf("INCOMPLETE_USAGE_RECORDS:%d", invalid)
		return result, nil
	}
	if records == 0 {
		return result, nil
	}
	days := []string{}
	for day := range daily {
		days = append(days, day)
	}
	sort.Strings(days)
	series := []*bucket{}
	for _, day := range days {
		series = append(series, daily[day])
	}
	result["availability"] = "available"
	result["reason"] = ""
	result["input"] = input
	result["output"] = output
	result["total"] = input + output
	result["daily"] = series
	result["records"] = records
	return result, nil
}
