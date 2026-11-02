package taskengine

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Called by the explicit product runner, against the SAME real executable,
// resolver, registry and locally counted TLS providers. No handler-only mux.
func b6ProductRefusals(t *testing.T, store *Store, origin string, tokens map[string]string, _ *http.Client, searches, models *atomic.Int32, entered, release chan struct{}, write func(string, any)) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 8 * time.Second}
	call := func(token, method, path string, body any, key string) (int, map[string]any) {
		t.Helper()
		data := ""
		if body != nil {
			b, e := json.Marshal(body)
			if e != nil {
				t.Fatal(e)
			}
			data = string(b)
		}
		req, e := http.NewRequest(method, origin+path, strings.NewReader(data))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, e := io.ReadAll(res.Body)
		if e != nil {
			t.Fatal(e)
		}
		var out map[string]any
		if json.Unmarshal(b, &out) != nil {
			t.Fatal("invalid API JSON")
		}
		return res.StatusCode, out
	}
	admin := tokens["admin"]
	status, listing := call(admin, "GET", "/v1/operator/models", nil, "")
	if status != 200 {
		t.Fatal(status)
	}
	model := listing["data"].(map[string]any)["items"].([]any)[0].(map[string]any)
	status, created := call(admin, "POST", "/v1/operator/agents", map[string]any{"name": "B6 refusal boundary", "description": "isolated", "enabled": true, "system_prompt": "No actions", "model_id": model["id"], "skills": []any{}, "tools": []any{}, "web_search": true, "expected_version": 0}, "")
	if status != 200 {
		t.Fatal(status, created)
	}
	agent := created["data"].(map[string]any)
	setAgent := func(enabled bool) {
		t.Helper()
		body := map[string]any{}
		for k, v := range agent {
			if k != "id" && k != "version" && k != "updated_by" && k != "updated_at" {
				body[k] = v
			}
		}
		body["expected_version"] = agent["version"]
		body["web_search"] = enabled
		status, out := call(admin, "PUT", "/v1/operator/agents/"+agent["id"].(string), body, "")
		if status != 200 {
			t.Fatal(status, out)
		}
		agent = out["data"].(map[string]any)
	}
	setSearch := func(enabled, key bool) {
		t.Helper()
		status, got := call(admin, "GET", WebSearchPath, nil, "")
		if status != 200 {
			t.Fatal(status)
		}
		action, value := "clear", ""
		if key {
			action, value = "replace", "fixture-search-key"
		}
		status, saved := call(admin, "PUT", WebSearchPath, map[string]any{"enabled": enabled, "source": "brave-free", "expected_version": got["data"].(map[string]any)["version"], "secret_action": action, "api_key": value, "proxy": map[string]any{"enabled": false, "url": "", "secret_action": "keep"}}, "")
		if status != 200 {
			t.Fatal(status, saved)
		}
	}
	evidence := []any{}
	for _, name := range []string{"global_off", "agent_off", "missing_key", "invalid_query", "no_permission", "search_budget", "model_budget"} {
		setSearch(name != "global_off", name != "missing_key")
		setAgent(name != "agent_off")
		req := ttRequest([]string{"930000101"}, []string{"acc_model"}, 1)
		req.Limits.MaxModelAttempts = 3
		if name == "model_budget" {
			req.Limits.MaxModelAttempts = 0
		}
		task, e := store.CreateTask(t.Context(), fixtureActor(), "b6-refusal-task-"+name, req)
		if e != nil {
			t.Fatal(name, e)
		}
		request := modelTestRequest(task)
		request.MaxProviderAttempts = 1
		q := "B6 explicit query"
		token := admin
		wantStatus, wantCode := 409, "SEARCH_DISABLED"
		switch name {
		case "agent_off":
			wantCode = "SEARCH_AGENT_DISABLED"
		case "missing_key":
			wantCode = "SEARCH_KEY_MISSING"
		case "invalid_query":
			q = " "
			wantStatus, wantCode = 400, taskCodeValidation
		case "no_permission":
			token = tokens["reader"]
			wantStatus, wantCode = 403, taskCodeForbidden
		case "search_budget":
			wantCode = "SEARCH_BUDGET_EXCEEDED"
		case "model_budget":
			wantStatus, wantCode = 500, modelCodeLocalBudgetExhausted
		}
		type result struct {
			status int
			body   map[string]any
		}
		done := make(chan result, 2)
		if name == "search_budget" {
			for i := 0; i < 2; i++ {
				go func() {
					s, b := call(admin, "POST", WebSearchPath+"/test", map[string]any{"query": "B6 budget hold"}, "")
					done <- result{s, b}
				}()
			}
			for i := 0; i < 2; i++ {
				select {
				case <-entered:
				case <-time.After(4 * time.Second):
					t.Fatal("budget request not in real TLS fixture")
				}
			}
		}
		beforeSearch, beforeModel := searches.Load(), models.Load()
		status, body := call(token, "POST", agentRequestPath, map[string]any{"agent_ref": map[string]any{"id": agent["id"], "version": agent["version"]}, "request": request, "search": map[string]any{"query": q}}, "b6-refuse-"+name)
		ds, dm := searches.Load()-beforeSearch, models.Load()-beforeModel
		errBody, ok := body["error"].(map[string]any)
		if !ok {
			t.Fatal(name, status, body)
		}
		if status != wantStatus || errBody["code"] != wantCode || ds != 0 || dm != 0 {
			t.Fatal(name, status, body, "deltas", ds, dm)
		}
		evidence = append(evidence, map[string]any{"case": name, "status": status, "code": wantCode, "search_delta": ds, "model_delta": dm, "real_binary_http": true})
		if name == "search_budget" {
			release <- struct{}{}
			release <- struct{}{}
			for i := 0; i < 2; i++ {
				select {
				case got := <-done:
					if got.status != 200 {
						t.Fatal(got)
					}
				case <-time.After(4 * time.Second):
					t.Fatal("hold release timeout")
				}
			}
		}
	}
	write("http-refusals.json", map[string]any{"cases": evidence, "admitted_budget_holds": 2, "all_denied_provider_deltas_zero": true, "same_production_binary": true})
}
