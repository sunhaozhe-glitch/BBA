package taskengine

import (
	"bba.local/taskengine/internal/contract"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ModelRegistry is opt-in trusted deployment configuration, not a second executor.
// Secrets use the same protected-file source as the existing startup provider.
// One process owns the file; a lifetime flock prevents concurrent writers.
type ModelProfile struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Model          string `json:"model"`
	BaseURL        string `json:"base_url"`
	Enabled        bool   `json:"enabled"`
	IsDefault      bool   `json:"is_default"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxConcurrent  int    `json:"max_concurrent"`
	Version        int    `json:"version"`
	HasSecret      bool   `json:"has_secret"`
}
type modelStoredProfile struct {
	ModelProfile
	APIKey string `json:"api_key"`
}
type modelRegistryDisk struct {
	Search       map[string]searchStored         `json:"web_search,omitempty"`
	SearchAudit  []searchAudit                   `json:"web_search_audit,omitempty"`
	SearchCalls  map[string]SearchReceipt        `json:"web_search_calls,omitempty"`
	Proxies      map[string]proxyStored          `json:"global_proxies,omitempty"`
	ProxyAudit   []proxyAudit                    `json:"global_proxy_audit,omitempty"`
	Schema       int                             `json:"schema"`
	Workspaces   map[string][]modelStoredProfile `json:"workspaces"`
	Agents       map[string][]AgentProfile       `json:"agents,omitempty"`
	Skills       map[string][]SkillProfile       `json:"skills,omitempty"`
	AgentHistory map[string][]AgentProfile       `json:"agent_history,omitempty"`
	SkillHistory map[string][]SkillProfile       `json:"skill_history,omitempty"`
	Receipts     map[string]AgentReceipt         `json:"agent_receipts,omitempty"`
}
type modelProfileWrite struct {
	Name            string `json:"name"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	Enabled         bool   `json:"enabled"`
	IsDefault       bool   `json:"is_default"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	MaxConcurrent   int    `json:"max_concurrent"`
	ExpectedVersion int    `json:"expected_version"`
	SecretAction    string `json:"secret_action"`
	APIKey          string `json:"api_key"`
}
type ModelRegistry struct {
	mu             sync.Mutex
	path, approved string
	data           modelRegistryDisk
	active         map[string]int
	searchActive   map[string]int
	lastTest       map[string]time.Time
	fallback       ModelProvider
	network        *modelNetwork // same-package isolated fixture seam; never deployment configuration
	client         *http.Client
	lock           *os.File
	persistFault   func(string) error // same-package failure injection; never configuration
	writeBlocked   bool               // rename succeeded but directory durability is uncertain; reopen required
}

func OpenModelRegistry(path, approved string, fallback ModelProvider, client *http.Client) (*ModelRegistry, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("registry path must be absolute")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return nil, errors.New("registry parent must be a private directory")
	}
	r := &ModelRegistry{path: path, approved: approved, fallback: fallback, client: client, active: map[string]int{}, searchActive: map[string]int{}, lastTest: map[string]time.Time{}, data: modelRegistryDisk{Schema: 1, Workspaces: map[string][]modelStoredProfile{}}}
	if err = r.acquireLock(); err != nil {
		return nil, err
	}
	fail := func(e error) (*ModelRegistry, error) { r.Close(); return nil, e }
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err = r.persist(r.data); err != nil {
			return fail(err)
		}
		return r, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4<<20 {
		return fail(errors.New("registry must be protected regular file at most 4MiB"))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fail(errors.New("registry read failed"))
	}
	if json.Unmarshal(b, &r.data) != nil || r.data.Schema != 1 || r.data.Workspaces == nil {
		return fail(errors.New("invalid registry"))
	}
	for _, p := range r.data.Search {
		if !validSearch(p) {
			return fail(errors.New("invalid search settings"))
		}
	}
	for _, p := range r.data.Proxies {
		if !validProxy(p) {
			return fail(errors.New("invalid proxy settings"))
		}
	}
	for _, items := range r.data.Workspaces {
		seen := map[string]bool{}
		defaults := 0
		for _, p := range items {
			if p.ID == "" || p.Version < 1 || seen[p.ID] || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 120 || p.MaxConcurrent < 1 || p.MaxConcurrent > 32 || strings.ContainsAny(p.APIKey, "\r\n") {
				return fail(errors.New("invalid registry identity"))
			}
			seen[p.ID] = true
			if p.IsDefault {
				defaults++
				if !p.Enabled || p.APIKey == "" {
					return fail(errors.New("invalid registry default"))
				}
			}
			if _, err = r.adapter(p); err != nil && p.APIKey != "" {
				return fail(errors.New("invalid registry provider"))
			}
		}
		if defaults > 1 {
			return fail(errors.New("multiple defaults"))
		}
	}
	// A new owner may be reopening after an uncertain directory Sync. Validate
	// above, then establish durability before accepting any subsequent writes.
	file, err := os.Open(path)
	if err != nil {
		return fail(errors.New("registry reopen failed"))
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return fail(errors.New("registry reopen sync failed"))
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fail(errors.New("registry directory reopen failed"))
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil || closeErr != nil {
		return fail(errors.New("registry directory reopen sync failed"))
	}
	return r, nil
}
func (r *ModelRegistry) Close() {
	if r.lock != nil {
		r.lock.Close()
		r.lock = nil
	}
}
func (r *ModelRegistry) persist(d modelRegistryDisk) error {
	if r.writeBlocked {
		return errors.New("registry requires safe reopen after uncertain durability")
	}
	fault := func(stage string) error {
		if r.persistFault != nil {
			return r.persistFault(stage)
		}
		return nil
	}
	b, err := json.Marshal(d)
	if err != nil || len(b) > 4<<20 {
		return errors.New("registry size exceeded")
	}
	f, err := os.CreateTemp(filepath.Dir(r.path), ".model-registry-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = fault("file-sync")
		if err == nil {
			err = f.Sync()
		}
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = fault("rename"); err != nil {
		return err
	}
	if err = os.Rename(tmp, r.path); err != nil {
		return err
	}
	// Rename is the visibility/commit boundary, even if directory durability fails.
	// Never roll back the visible file or allow a stale caller snapshot to overwrite it.
	r.data = d
	r.writeBlocked = true
	if err = fault("directory-open"); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(r.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = fault("directory-sync"); err == nil {
		err = dir.Sync()
	}
	if err != nil {
		return err
	}
	r.writeBlocked = false
	return nil
}
func (r *ModelRegistry) adapter(p modelStoredProfile) (ModelProvider, error) {
	return NewOpenAICompatibleProvider(OpenAICompatibleProviderConfig{BaseURL: p.BaseURL, ModelID: p.Model, ProviderID: p.ID, APIKey: p.APIKey, ApprovedPrivateHTTPBaseURL: r.approved}, r.client)
}
func (r *ModelRegistry) list(workspace string) []ModelProfile {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []ModelProfile{}
	for _, p := range r.data.Workspaces[workspace] {
		v := p.ModelProfile
		v.HasSecret = p.APIKey != ""
		out = append(out, v)
	}
	return out
}
func (r *ModelRegistry) save(workspace, id string, w modelProfileWrite, remove bool) (ModelProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := append([]modelStoredProfile(nil), r.data.Workspaces[workspace]...)
	idx := -1
	for i, p := range items {
		if p.ID == id {
			idx = i
		}
	}
	if id != "" && idx < 0 {
		return ModelProfile{}, problem(taskCodeNotFound)
	}
	old := modelStoredProfile{}
	if idx >= 0 {
		old = items[idx]
	}
	if w.ExpectedVersion != old.Version {
		return ModelProfile{}, problem(taskCodeVersionConflict)
	}
	if idx >= 0 && (remove || !w.Enabled || w.SecretAction == "clear") {
		for _, a := range r.data.Agents[workspace] {
			if a.ModelID == id {
				return ModelProfile{}, problem("CONFIG_REFERENCED")
			}
		}
	}
	if remove {
		if old.IsDefault || r.active[workspace] > 0 {
			return ModelProfile{}, problem(taskCodeStateConflict)
		}
		items = append(items[:idx], items[idx+1:]...)
	} else {
		if len(w.Name) < 1 || len(w.Name) > 100 || len(w.Model) < 1 || len(w.Model) > 200 || len(w.BaseURL) > 1000 || w.TimeoutSeconds < 1 || w.TimeoutSeconds > 120 || w.MaxConcurrent < 1 || w.MaxConcurrent > 32 {
			return ModelProfile{}, problem(taskCodeValidation)
		}
		key := old.APIKey
		switch w.SecretAction {
		case "keep":
			if w.APIKey != "" {
				return ModelProfile{}, problem(taskCodeValidation)
			}
		case "replace":
			key = w.APIKey
			if key == "" || len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
				return ModelProfile{}, problem(taskCodeValidation)
			}
		case "clear":
			if w.APIKey != "" {
				return ModelProfile{}, problem(taskCodeValidation)
			}
			key = ""
		default:
			return ModelProfile{}, problem(taskCodeValidation)
		}
		if old.IsDefault && (!w.Enabled || !w.IsDefault || key == "") {
			return ModelProfile{}, problem(taskCodeStateConflict)
		}
		if w.IsDefault && (!w.Enabled || key == "") {
			return ModelProfile{}, problem(taskCodeStateConflict)
		}
		if id == "" {
			if len(items) >= 100 {
				return ModelProfile{}, problem(taskCodeStateConflict)
			}
			id = newID("model")
		}
		old = modelStoredProfile{ModelProfile: ModelProfile{ID: id, Name: w.Name, Model: w.Model, BaseURL: w.BaseURL, Enabled: w.Enabled, IsDefault: w.IsDefault, TimeoutSeconds: w.TimeoutSeconds, MaxConcurrent: w.MaxConcurrent, Version: w.ExpectedVersion + 1, HasSecret: key != ""}, APIKey: key}
		probe := old
		if probe.APIKey == "" {
			probe.APIKey = "validation-only"
		}
		if _, err := r.adapter(probe); err != nil {
			return ModelProfile{}, problem(taskCodeValidation)
		}
		if w.IsDefault {
			for i := range items {
				if items[i].IsDefault && items[i].ID != id {
					items[i].IsDefault = false
					items[i].Version++
				}
			}
		}
		if idx < 0 {
			items = append(items, old)
		} else {
			items[idx] = old
		}
	}
	next := r.data
	next.Workspaces = make(map[string][]modelStoredProfile, len(r.data.Workspaces)+1)
	for k, v := range r.data.Workspaces {
		next.Workspaces[k] = v
	}
	next.Workspaces[workspace] = items
	if err := r.persist(next); err != nil {
		return ModelProfile{}, problem(taskCodeStoreUnavailable)
	}
	r.data = next
	return old.ModelProfile, nil
}
func (r *ModelRegistry) invoke(ctx context.Context, workspace, id string, call ModelCall, test bool) (ModelCompletion, error) {
	r.mu.Lock()
	items := r.data.Workspaces[workspace]
	var p modelStoredProfile
	found := false
	for _, v := range items {
		if (id != "" && v.ID == id) || (id == "" && v.IsDefault) {
			p = v
			found = true
			break
		}
	}
	if !found {
		r.mu.Unlock()
		if id == "" && len(items) == 0 && r.fallback != nil {
			snap, e := r.proxySnapshot(workspace)
			if e != nil {
				return ModelCompletion{}, e
			}
			provider, e := r.proxyProvider(r.fallback, snap)
			if e != nil {
				return ModelCompletion{}, problem(modelCodeProviderUnavailable)
			}
			return provider.Complete(ctx, call)
		}
		return ModelCompletion{}, problem(modelCodeProviderUnavailable)
	}
	if !p.Enabled || p.APIKey == "" {
		r.mu.Unlock()
		return ModelCompletion{}, problem(modelCodeProviderUnavailable)
	}
	if r.active[workspace] >= p.MaxConcurrent {
		r.mu.Unlock()
		return ModelCompletion{}, problem(taskCodeStateConflict)
	}
	if test && time.Since(r.lastTest[workspace]) < time.Second {
		r.mu.Unlock()
		return ModelCompletion{}, problem(taskCodeStateConflict)
	}
	if test {
		r.lastTest[workspace] = time.Now()
	}
	r.active[workspace]++
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.active[workspace]--; r.mu.Unlock() }()
	provider, err := r.adapterFor(ctx, workspace, p)
	if err != nil {
		if Code(err) == taskCodeStoreUnavailable {
			return ModelCompletion{}, err
		}
		return ModelCompletion{}, problem(modelCodeProviderUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	result, err := provider.Complete(ctx, call)
	if err != nil {
		return ModelCompletion{}, problem(modelCodeProviderFailed)
	}
	return result, nil
}
func (r *ModelRegistry) Complete(ctx context.Context, call ModelCall) (ModelCompletion, error) {
	return r.invoke(ctx, call.WorkspaceID, "", call, false)
}
func (r *ModelRegistry) Handler(auth func(*http.Request) (Actor, error), store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, code string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
		}
		actor, err := auth(req)
		if err != nil || actor.WorkspaceID == "" || actor.PrincipalID == "" {
			fail(401, "UNAUTHENTICATED")
			return
		}
		if actor.RuntimeID != "" || !actor.Permissions["operator.read"] {
			fail(403, "ACTION_FORBIDDEN")
			return
		}
		if store != nil {
			var exists bool
			err = store.Pool.QueryRow(req.Context(), `SELECT EXISTS(SELECT 1 FROM core.principal WHERE workspace_id=$1 AND id=$2)`, actor.WorkspaceID, actor.PrincipalID).Scan(&exists)
			if err != nil {
				fail(503, "STATE_STORE_UNAVAILABLE")
				return
			}
			if !exists {
				fail(403, "ACTION_FORBIDDEN")
				return
			}
		}
		base := "/v1/operator/models"
		suffix := strings.TrimPrefix(req.URL.Path, base)
		if req.URL.Path != base && !strings.HasPrefix(req.URL.Path, base+"/") {
			fail(404, "RESOURCE_NOT_FOUND")
			return
		}
		if req.Method == "GET" && suffix == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": r.list(actor.WorkspaceID), "can_configure": actor.Permissions["model.configure"], "route": "workspace_planning_default_v1"}})
			return
		}
		if !actor.Permissions["model.configure"] {
			fail(403, "ACTION_FORBIDDEN")
			return
		}
		id := strings.TrimPrefix(suffix, "/")
		test := strings.HasSuffix(id, "/test")
		if test {
			id = strings.TrimSuffix(id, "/test")
		}
		if strings.Contains(id, "/") {
			fail(404, "RESOURCE_NOT_FOUND")
			return
		}
		if test && req.Method == "POST" && id != "" {
			if !actor.Permissions[modelPermRequest] {
				fail(403, "ACTION_FORBIDDEN")
				return
			}
			result, e := r.invoke(req.Context(), actor.WorkspaceID, id, ModelCall{WorkspaceID: actor.WorkspaceID, Version: 1, ScopeRevision: 1, GoalCount: 1, PromptVersion: modelPromptVersion}, true)
			if e != nil {
				status := 502
				if Code(e) == taskCodeStateConflict {
					status = 409
				}
				fail(status, Code(e))
				return
			}
			probe := contract.ModelRequest{InputRef: "connection-test", SchemaId: "PlanProposal.v1"}
			probe.TaskRef.Set(struct {
				Id            string `json:"id"`
				ScopeRevision int    `json:"scope_revision"`
				Version       int    `json:"version"`
			}{Id: "connection-test", ScopeRevision: 1, Version: 1})
			if _, e = validateModelProposal(result.Content, probe, ModelCall{Version: 1, ScopeRevision: 1, GoalCount: 1}); e != nil {
				fail(502, modelCodeSchemaInvalid)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"connected": true, "model": result.ModelID, "provider": result.ProviderID}})
			return
		}
		if !((req.Method == "POST" && id == "") || ((req.Method == "PUT" || req.Method == "DELETE") && id != "" && !test)) {
			fail(405, "METHOD_NOT_ALLOWED")
			return
		}
		if !strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
			fail(415, "UNSUPPORTED_MEDIA_TYPE")
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, 16384)
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		var body modelProfileWrite
		if dec.Decode(&body) != nil || dec.Decode(new(any)) != io.EOF {
			fail(400, "VALIDATION_ERROR")
			return
		}
		result, e := r.save(actor.WorkspaceID, id, body, req.Method == "DELETE")
		if e != nil {
			status := 400
			switch Code(e) {
			case taskCodeVersionConflict, taskCodeStateConflict, "CONFIG_REFERENCED":
				status = 409
			case taskCodeNotFound:
				status = 404
			case taskCodeStoreUnavailable:
				status = 503
			}
			fail(status, Code(e))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": result})
	})
}
