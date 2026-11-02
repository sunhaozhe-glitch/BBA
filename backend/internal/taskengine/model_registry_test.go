package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelRegistryLifecycleAndConsumer(t *testing.T) {
	s := fixtureStore(t)
	task := modelTestTask(t, s, 3)
	actor := modelTestActor()
	actor.Permissions["operator.read"] = true
	actor.Permissions["model.configure"] = true
	var calls atomic.Int32
	var slow atomic.Bool
	var seen sync.Map
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Error("wrong adapter path or secret")
			w.WriteHeader(401)
			return
		}
		var raw map[string]any
		if json.NewDecoder(r.Body).Decode(&raw) != nil {
			t.Error("invalid body")
			return
		}
		if strings.Contains(raw["messages"].([]any)[0].(map[string]any)["content"].(string), "fixture-secret") {
			t.Error("secret leaked into prompt")
		}
		model := raw["model"].(string)
		seen.Store(model, true)
		if slow.Load() {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": modelTestProposal(t, task)}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "models.json")
	registry, err := OpenModelRegistry(path, "", nil, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { registry.Close() }()
	if _, err := OpenModelRegistry(path, "", nil, server.Client()); err == nil {
		t.Fatal("second process lock accepted")
	}
	selected := actor
	auth := func(*http.Request) (Actor, error) { return selected, nil }
	handler := registry.Handler(auth, s)
	call := func(method, url string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var response map[string]any
		if json.Unmarshal(rec.Body.Bytes(), &response) != nil {
			t.Fatalf("invalid response %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "fixture-secret") || strings.Contains(rec.Body.String(), "api_key") {
			t.Fatal("GET/write response leaked secret")
		}
		return rec.Code, response
	}
	write := modelProfileWrite{Name: "本地fixture", Model: "model-A", BaseURL: server.URL + "/v1", Enabled: true, IsDefault: true, TimeoutSeconds: 1, MaxConcurrent: 1, SecretAction: "replace", APIKey: "fixture-secret"}
	status, res := call("POST", "/v1/operator/models", write)
	if status != 200 {
		t.Fatalf("create %d %#v", status, res)
	}
	id := res["data"].(map[string]any)["id"].(string)
	status, _ = call("GET", "/v1/operator/models", nil)
	if status != 200 {
		t.Fatal(status)
	}
	selected.Permissions = map[string]bool{"operator.read": true}
	status, _ = call("POST", "/v1/operator/models", write)
	if status != 403 {
		t.Fatal("reader write", status)
	}
	status, _ = call("POST", "/v1/operator/models/"+id+"/test", nil)
	if status != 403 {
		t.Fatal("reader test", status)
	}
	selected = actor
	status, _ = call("PUT", "/v1/operator/models/"+id, write)
	if status != 409 {
		t.Fatal("stale version", status)
	}
	registry.Close()
	registry, err = OpenModelRegistry(path, "", nil, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	handler = registry.Handler(auth, s)
	if len(registry.list(actor.WorkspaceID)) != 1 {
		t.Fatal("restart lost config")
	}
	request := modelTestRequest(task)
	response, err := s.RunPlanModelRequest(context.Background(), actor, "registry-consume", request, registry)
	if err != nil {
		t.Fatalf("real planning consumer: %v", err)
	}
	_ = response
	if _, ok := seen.Load("model-A"); !ok {
		t.Fatal("selected model was not consumed")
	}
	before := calls.Load()
	_, err = s.RunPlanModelRequest(context.Background(), actor, "registry-consume", request, registry)
	if err != nil {
		t.Fatal("idempotent replay failed", err)
	}
	if calls.Load() != before {
		t.Fatal("replay called provider")
	}
	status, _ = call("POST", "/v1/operator/models/"+id+"/test", nil)
	if status != 200 {
		t.Fatal("connection test", status)
	}
	if _, e := s.Pool.Exec(context.Background(), `INSERT INTO core.workspace(id) VALUES('b1-other'); INSERT INTO core.principal(workspace_id,id) VALUES('b1-other','b1-reader')`); e != nil {
		t.Fatal(e)
	}
	selected.WorkspaceID = "b1-other"
	selected.PrincipalID = "b1-reader"
	status, res = call("GET", "/v1/operator/models", nil)
	if status != 200 || len(res["data"].(map[string]any)["items"].([]any)) != 0 {
		t.Fatal("cross workspace list", status, res)
	}
	status, _ = call("PUT", "/v1/operator/models/"+id, write)
	if status != 404 {
		t.Fatal("cross workspace update", status)
	}
	selected = actor
	selected.WorkspaceID = "absent-workspace"
	status, _ = call("GET", "/v1/operator/models", nil)
	if status != 403 {
		t.Fatal("workspace membership", status)
	}
	if len(registry.list("other")) != 0 {
		t.Fatal("cross workspace")
	}
	selected = actor
	write.ExpectedVersion = 1
	write.SecretAction = "keep"
	write.APIKey = ""
	write.Enabled = false
	status, _ = call("PUT", "/v1/operator/models/"+id, write)
	if status != 409 {
		t.Fatal("default disabled", status)
	}
	status, _ = call("DELETE", "/v1/operator/models/"+id, map[string]int{"expected_version": 1})
	if status != 409 {
		t.Fatal("default deleted", status)
	}
	write.Enabled = true
	write.IsDefault = false
	write.Model = "model-B"
	write.ExpectedVersion = 0
	write.SecretAction = "replace"
	write.APIKey = "fixture-secret"
	status, res = call("POST", "/v1/operator/models", write)
	if status != 200 {
		t.Fatal(status, res)
	}
	second := res["data"].(map[string]any)["id"].(string)
	write.IsDefault = true
	write.ExpectedVersion = 1
	write.SecretAction = "keep"
	write.APIKey = ""
	status, _ = call("PUT", "/v1/operator/models/"+second, write)
	if status != 200 {
		t.Fatal(status)
	}
	_, err = registry.Complete(context.Background(), ModelCall{WorkspaceID: actor.WorkspaceID, Version: task.Version, ScopeRevision: task.ScopeRevision, GoalCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := seen.Load("model-B"); !ok {
		t.Fatal("default switch unused")
	}
	first := registry.list(actor.WorkspaceID)[0]
	write.ExpectedVersion = first.Version
	write.Model = first.Model
	write.IsDefault = false
	write.Enabled = false
	write.SecretAction = "clear"
	status, _ = call("PUT", "/v1/operator/models/"+id, write)
	if status != 200 {
		t.Fatal("clear nondefault", status)
	}
	_, err = registry.invoke(context.Background(), actor.WorkspaceID, id, ModelCall{}, false)
	if err == nil {
		t.Fatal("disabled still called")
	}
	slow.Store(true)
	start := time.Now()
	_, err = registry.Complete(context.Background(), ModelCall{WorkspaceID: actor.WorkspaceID})
	if err == nil || time.Since(start) > 1800*time.Millisecond {
		t.Fatal("timeout not effective", err, time.Since(start))
	}
	slow.Store(false)
	write.ExpectedVersion = 2
	write.Model = "model-B"
	write.Enabled = true
	write.IsDefault = true
	write.SecretAction = "keep"
	var wins atomic.Int32
	var conflicts atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := registry.save(actor.WorkspaceID, second, write, false)
			if e == nil {
				wins.Add(1)
			} else if Code(e) == taskCodeVersionConflict {
				conflicts.Add(1)
			} else {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 1 {
		t.Fatal("concurrent edits not serialized", wins.Load(), conflicts.Load())
	}
	slow.Store(true)
	done := make(chan error, 1)
	go func() {
		_, e := registry.Complete(context.Background(), ModelCall{WorkspaceID: actor.WorkspaceID})
		done <- e
	}()
	for i := 0; i < 100; i++ {
		registry.mu.Lock()
		busy := registry.active[actor.WorkspaceID] > 0
		registry.mu.Unlock()
		if busy {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_, e := registry.invoke(context.Background(), actor.WorkspaceID, second, ModelCall{}, true)
	if Code(e) != taskCodeStateConflict {
		t.Fatal("test bypassed runtime shared concurrency", e)
	}
	if <-done == nil {
		t.Fatal("slow request timeout expected")
	}
	slow.Store(false)
	p0 := registry.list(actor.WorkspaceID)[0]
	status, _ = call("DELETE", "/v1/operator/models/"+id, map[string]int{"expected_version": p0.Version})
	if status != 200 {
		t.Fatal("delete unreferenced", status)
	}
	if len(registry.list(actor.WorkspaceID)) != 1 {
		t.Fatal("delete did not persist")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("registry mode", err)
	}
}
