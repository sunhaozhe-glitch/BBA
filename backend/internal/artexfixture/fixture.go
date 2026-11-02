// Package artexfixture is a protocol boundary fixture for the ARTEX REST API
// that BBA's artex adapter speaks. It is NOT an ARTEX implementation: it serves
// only the routes and shapes verified against the local ARTEX source, so a real
// artex.Client (or the real GraphSyncWorker) can be pointed at it over loopback
// without any live ARTEX, model, QQ or database dependency.
//
// Verified surface (local ARTEX tree, commit recorded in
// local://GraphSync-integration.md):
//
//   - GET  /api/health                     -> {"ok":true,"service":"artex",…}; no auth
//                                            (ARTEX/server/server.go:1038-1040)
//   - every other /api/* route requires a bearer token; missing is 401
//     {"error":"未授权"} and a wrong token is 401 {"error":"token 无效或已过期"}
//     (ARTEX/server/auth.go:120-137)
//   - GET  /api/exploration/graph?task=     -> {"nodes":[…],"edges":[…]} capped at
//                                            2000 nodes / 5000 edges; an unknown
//                                            task is 200 with empty arrays
//                                            (ARTEX/server/server.go:2630-2674)
//   - GET  /api/exploration/nodes?…         -> items/total/page/size/edges/refs/
//                                            assets (ARTEX/server/server.go:2676-2741)
//   - GET  /api/tasks/{id}/goals            -> {"goals":[GoalDTO,…]} or 404
//                                            {"error":"task not found"} (goals_api.go:18)
//   - POST /api/tasks/{id}/goals            -> GoalDTO; 400 on empty text/invalid
//                                            JSON (goals_api.go:34-81)
//   - PATCH /api/tasks/{id}/goals/{gid}     -> GoalDTO; 404 for a non-goal
//                                            (goals_api.go:83-137)
//   - DELETE /api/tasks/{id}/goals/{gid}    -> {"ok":true} (goals_api.go:139-171)
//
// Faults exist so failure tests can exercise the real adapter against protocol
// anomalies (a malformed envelope, an upstream 500, a slow reply). They are
// explicit options, never an environment default.
package artexfixture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Node is one ARTEX exploration node as the wire publishes it
// (ARTEX/server/dto.go:183-211). Payload is the raw payload JSON *text* that
// ARTEX puts inside a JSON string; it must be valid JSON when non-empty.
type Node struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Payload      string `json:"payload,omitempty"`
	Priority     int    `json:"priority,omitempty"`
	State        string `json:"state"`
	Origin       string `json:"origin,omitempty"`
	TS           string `json:"ts"`
	SourceTaskID string `json:"source_task_id,omitempty"`
	Inherited    bool   `json:"inherited,omitempty"`
	DeleteReason string `json:"delete_reason,omitempty"`
}

// Edge is one ARTEX exploration edge (ARTEX/server/dto.go:268-277).
type Edge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	Rel string `json:"rel"`
}

// Task is one ARTEX task and its exploration graph.
type Task struct {
	ID     string `json:"id"`
	Active bool   `json:"active,omitempty"`
	Nodes  []Node `json:"nodes,omitempty"`
	Edges  []Edge `json:"edges,omitempty"`
}

// State is the durable fixture state. It is the file written to
// <data-dir>/artex-state.json after every mutation.
type State struct {
	NextNodeID int64  `json:"next_node_id,omitempty"`
	Tasks      []Task `json:"tasks"`
}

// Faults injects one protocol anomaly. Every field is opt-in and zero means the
// faithful reply.
type Faults struct {
	// Delay is applied before answering every authenticated route (a slow
	// ARTEX, used to exercise the adapter's timeout path).
	Delay time.Duration
	// GraphStatus forces GET /api/exploration/graph to answer that status with
	// {"error":"fixture fault"} instead of the graph.
	GraphStatus int
	// GraphBody replaces the whole graph response body (malformed protocol).
	GraphBody string
	// GoalsStatus forces the goal routes to answer that status.
	GoalsStatus int
	// GoalsBody replaces the goal-list body (malformed protocol).
	GoalsBody string
	// GoalsCreateReplyBody replaces the POST reply *after* the goal was stored,
	// which is the real "ambiguous outcome" shape: the external effect happened
	// but the caller cannot prove it from the reply.
	GoalsCreateReplyBody string
}

// Options configures one fixture handler.
type Options struct {
	// Token is the only accepted bearer token. Empty is refused by New: a
	// fixture that accepts any token would hide an authentication regression.
	Token string
	// State is the initial graph state (copied by New).
	State State
	// Faults injects a protocol anomaly.
	Faults Faults
	// Path, when set, is the state file written after every mutation.
	Path string
}

// Fixture is an http.Handler serving the verified ARTEX protocol shape.
type Fixture struct {
	mu     sync.Mutex
	token  string
	state  State
	faults Faults
	path   string
}

// New builds a fixture handler. It refuses an empty token, duplicate task ids,
// an empty task id, and a goal/edge graph that references unknown nodes, so a
// misconfigured seed cannot produce a protocol shape the real ARTEX never emits.
func New(opts Options) (*Fixture, error) {
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, fmt.Errorf("artexfixture: token is required")
	}
	state := opts.State
	seenTasks := map[string]bool{}
	seenNodes := map[string]bool{}
	if state.NextNodeID == 0 {
		var max int64
		for _, task := range state.Tasks {
			for _, node := range task.Nodes {
				if id, err := strconv.ParseInt(node.ID, 10, 64); err == nil && id > max {
					max = id
				}
			}
		}
		state.NextNodeID = max + 1
	}
	for _, task := range state.Tasks {
		if strings.TrimSpace(task.ID) == "" || task.ID == "active" {
			return nil, fmt.Errorf("artexfixture: task id is required and must not be the active alias")
		}
		if seenTasks[task.ID] {
			return nil, fmt.Errorf("artexfixture: duplicate task id %q", task.ID)
		}
		seenTasks[task.ID] = true
		for _, node := range task.Nodes {
			if id, err := strconv.ParseInt(node.ID, 10, 64); err != nil || id <= 0 {
				return nil, fmt.Errorf("artexfixture: task %q node id %q is not positive decimal", task.ID, node.ID)
			}
			if strings.TrimSpace(node.Type) == "" || strings.TrimSpace(node.State) == "" || strings.TrimSpace(node.TS) == "" {
				return nil, fmt.Errorf("artexfixture: task %q node %q needs type/state/ts", task.ID, node.ID)
			}
			if payload := strings.TrimSpace(node.Payload); payload != "" && payload != "null" && !json.Valid([]byte(payload)) {
				return nil, fmt.Errorf("artexfixture: task %q node %q payload is not JSON", task.ID, node.ID)
			}
			seenNodes[task.ID+"/"+node.ID] = true
		}
	}
	for _, task := range state.Tasks {
		for _, edge := range task.Edges {
			if !seenNodes[task.ID+"/"+edge.Src] || !seenNodes[task.ID+"/"+edge.Dst] {
				return nil, fmt.Errorf("artexfixture: task %q edge %s->%s references an unknown node", task.ID, edge.Src, edge.Dst)
			}
			if strings.TrimSpace(edge.Rel) == "" {
				return nil, fmt.Errorf("artexfixture: task %q edge %s->%s has no rel", task.ID, edge.Src, edge.Dst)
			}
		}
	}
	if opts.Path != "" {
		if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
			return nil, fmt.Errorf("artexfixture: create data dir: %w", err)
		}
	}
	return &Fixture{token: token, state: state, faults: opts.Faults, path: opts.Path}, nil
}

// LoadState reads a state file written by a previous fixture run. A missing file
// is an empty state, not an error: the caller decides whether a seed is needed.
func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("artexfixture: decode %s: %w", path, err)
	}
	return state, nil
}

// ServeHTTP implements the verified protocol. It never answers a route the real
// ARTEX does not publish, so a client that passes against this fixture is not
// relying on a convenience invented here.
func (f *Fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/health":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "artex", "version": "fixture"})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if !f.authorized(w, r) {
		return
	}
	f.mu.Lock()
	faults := f.faults
	f.mu.Unlock()
	if faults.Delay > 0 {
		select {
		case <-time.After(faults.Delay):
		case <-r.Context().Done():
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/exploration/graph":
		f.graph(w, r, faults)
	case r.Method == http.MethodGet && r.URL.Path == "/api/exploration/nodes":
		f.nodes(w, r)
	default:
		id, gid, goals, ok := parseGoalPath(r.URL.Path)
		if !ok {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		switch {
		case goals && r.Method == http.MethodGet:
			f.listGoals(w, r, id, faults)
		case goals && r.Method == http.MethodPost:
			f.addGoal(w, r, id, faults)
		case !goals && gid != "" && r.Method == http.MethodPatch:
			f.editGoal(w, r, id, gid, faults)
		case !goals && gid != "" && r.Method == http.MethodDelete:
			f.deleteGoal(w, r, id, gid, faults)
		default:
			w.Header().Set("Allow", allowedMethod(goals))
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func allowedMethod(goals bool) string {
	if goals {
		return "GET, POST"
	}
	return "PATCH, DELETE"
}

func (f *Fixture) authorized(w http.ResponseWriter, r *http.Request) bool {
	header := r.Header.Get("Authorization")
	token := ""
	if strings.HasPrefix(header, "Bearer ") {
		token = strings.TrimPrefix(header, "Bearer ")
	}
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "未授权")
		return false
	}
	if token != f.token {
		writeErr(w, http.StatusUnauthorized, "token 无效或已过期")
		return false
	}
	return true
}

// parseGoalPath splits /api/tasks/{id}/goals[/{gid}]. It accepts an escaped id
// segment, matching net/http's own path decoding for the real router.
func parseGoalPath(path string) (taskID, goalID string, goals bool, ok bool) {
	rest, found := strings.CutPrefix(path, "/api/tasks/")
	if !found {
		return "", "", false, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[1] != "goals" {
		return "", "", false, false
	}
	if parts[0] == "" || parts[0] == "active" {
		return "", "", false, false
	}
	if len(parts) == 2 {
		return parts[0], "", true, true
	}
	if strings.TrimSpace(parts[2]) == "" {
		return "", "", false, false
	}
	return parts[0], parts[2], false, true
}

func (f *Fixture) graph(w http.ResponseWriter, r *http.Request, faults Faults) {
	if faults.GraphStatus != 0 {
		writeErr(w, faults.GraphStatus, "fixture fault")
		return
	}
	if faults.GraphBody != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(faults.GraphBody))
		return
	}
	taskID := strings.TrimSpace(r.URL.Query().Get("task"))
	f.mu.Lock()
	task, ok := f.taskLocked(taskID)
	nodes := make([]Node, 0)
	edges := make([]Edge, 0)
	if ok {
		nodes = append(nodes, task.Nodes...)
		edges = append(edges, task.Edges...)
	}
	f.mu.Unlock()
	if len(nodes) > 2000 {
		nodes = nodes[:2000]
	}
	if len(edges) > 5000 {
		edges = edges[:5000]
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "edges": edges})
}

func (f *Fixture) nodes(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page := atoiDefault(query.Get("page"), 1)
	size := atoiDefault(query.Get("size"), 20)
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	kinds := csvValues(query.Get("kind"))
	states := csvValues(query.Get("state"))
	needle := strings.ToLower(strings.TrimSpace(query.Get("q")))

	f.mu.Lock()
	task, ok := f.taskLocked(strings.TrimSpace(query.Get("task")))
	items := make([]Node, 0)
	edges := make([]Edge, 0)
	total := 0
	if ok {
		matched := make([]Node, 0, len(task.Nodes))
		for _, node := range task.Nodes {
			if len(kinds) > 0 && !containsString(kinds, node.Type) {
				continue
			}
			if len(states) > 0 && !containsString(states, node.State) {
				continue
			}
			if needle != "" && !strings.Contains(strings.ToLower(node.Payload), needle) && node.ID != needle {
				continue
			}
			matched = append(matched, node)
		}
		sort.Slice(matched, func(i, j int) bool {
			if query.Get("order") == "asc" {
				return matched[i].ID < matched[j].ID
			}
			return matched[i].ID > matched[j].ID
		})
		total = len(matched)
		start := (page - 1) * size
		if start < total {
			end := start + size
			if end > total {
				end = total
			}
			items = append(items, matched[start:end]...)
		}
		onPage := map[string]bool{}
		for _, node := range items {
			onPage[node.ID] = true
		}
		neighbours := map[string]bool{}
		for _, edge := range task.Edges {
			if onPage[edge.Src] || onPage[edge.Dst] {
				edges = append(edges, edge)
				if !onPage[edge.Src] {
					neighbours[edge.Src] = true
				}
				if !onPage[edge.Dst] {
					neighbours[edge.Dst] = true
				}
			}
		}
		refs := map[string]Node{}
		for _, node := range task.Nodes {
			if neighbours[node.ID] {
				refs[node.ID] = node
			}
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"items": items, "total": total, "page": page, "size": size,
			"edges": edges, "refs": refs, "assets": map[string]any{},
		})
		return
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"items": []Node{}, "total": 0, "page": page, "size": size,
		"edges": []Edge{}, "refs": map[string]any{}, "assets": map[string]any{},
	})
}

func (f *Fixture) listGoals(w http.ResponseWriter, r *http.Request, taskID string, faults Faults) {
	f.mu.Lock()
	task, ok := f.taskLocked(taskID)
	f.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if faults.GoalsStatus != 0 {
		writeErr(w, faults.GoalsStatus, "fixture fault")
		return
	}
	if faults.GoalsBody != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(faults.GoalsBody))
		return
	}
	f.mu.Lock()
	goals := goalDTOs(task)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"goals": goals})
}

func (f *Fixture) addGoal(w http.ResponseWriter, r *http.Request, taskID string, faults Faults) {
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, "目标内容不能为空")
		return
	}
	if faults.GoalsStatus != 0 {
		writeErr(w, faults.GoalsStatus, "fixture fault")
		return
	}
	f.mu.Lock()
	task, ok := f.taskLocked(taskID)
	if !ok {
		f.mu.Unlock()
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	id := f.state.NextNodeID
	f.state.NextNodeID++
	node := Node{ID: fmt.Sprint(id), Type: "goal", Payload: goalPayload(text, strings.TrimSpace(body.VulnClass)), State: "open", Origin: "human", TS: time.Now().UTC().Format(time.RFC3339)}
	task.Nodes = append(task.Nodes, node)
	f.replaceTaskLocked(task)
	goal := goalDTO(node)
	if err := f.persistLocked(); err != nil {
		f.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, "fixture persist: "+err.Error())
		return
	}
	f.mu.Unlock()
	if faults.GoalsCreateReplyBody != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(faults.GoalsCreateReplyBody))
		return
	}
	writeJSON(w, http.StatusOK, goal)
}

func (f *Fixture) editGoal(w http.ResponseWriter, r *http.Request, taskID, rawGoalID string, faults Faults) {
	goalID, err := strconv.ParseInt(rawGoalID, 10, 64)
	if err != nil || goalID <= 0 {
		writeErr(w, http.StatusBadRequest, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, "目标内容不能为空")
		return
	}
	if faults.GoalsStatus != 0 {
		writeErr(w, faults.GoalsStatus, "fixture fault")
		return
	}
	f.mu.Lock()
	task, ok := f.taskLocked(taskID)
	if !ok {
		f.mu.Unlock()
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	for i, node := range task.Nodes {
		if node.ID != rawGoalID {
			continue
		}
		if node.Type != "goal" {
			f.mu.Unlock()
			writeErr(w, http.StatusNotFound, "目标不存在")
			return
		}
		task.Nodes[i].Payload = goalPayload(text, strings.TrimSpace(body.VulnClass))
		updated := task.Nodes[i]
		f.replaceTaskLocked(task)
		if err := f.persistLocked(); err != nil {
			f.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, "fixture persist: "+err.Error())
			return
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, goalDTO(updated))
		return
	}
	f.mu.Unlock()
	writeErr(w, http.StatusNotFound, "目标不存在")
}

func (f *Fixture) deleteGoal(w http.ResponseWriter, r *http.Request, taskID, rawGoalID string, faults Faults) {
	goalID, err := strconv.ParseInt(rawGoalID, 10, 64)
	if err != nil || goalID <= 0 {
		writeErr(w, http.StatusBadRequest, "bad goal id")
		return
	}
	if faults.GoalsStatus != 0 {
		writeErr(w, faults.GoalsStatus, "fixture fault")
		return
	}
	f.mu.Lock()
	task, ok := f.taskLocked(taskID)
	if !ok {
		f.mu.Unlock()
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	for i, node := range task.Nodes {
		if node.ID != rawGoalID {
			continue
		}
		if node.Type != "goal" {
			f.mu.Unlock()
			writeErr(w, http.StatusNotFound, "目标不存在")
			return
		}
		task.Nodes = append(task.Nodes[:i], task.Nodes[i+1:]...)
		kept := task.Edges[:0]
		for _, edge := range task.Edges {
			if edge.Src != rawGoalID && edge.Dst != rawGoalID {
				kept = append(kept, edge)
			}
		}
		task.Edges = kept
		f.replaceTaskLocked(task)
		if err := f.persistLocked(); err != nil {
			f.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, "fixture persist: "+err.Error())
			return
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	f.mu.Unlock()
	writeErr(w, http.StatusNotFound, "目标不存在")
}

// SetFaults replaces the injected anomalies. Tests use it to clear a fault and
// prove the recovery path; it is not part of the protocol surface.
func (f *Fixture) SetFaults(faults Faults) {
	f.mu.Lock()
	f.faults = faults
	f.mu.Unlock()
}

// Goals returns the currently stored goals of one task (test assertion helper).
func (f *Fixture) Goals(taskID string) []Goal {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, ok := f.taskLocked(taskID)
	if !ok {
		return nil
	}
	return goalDTOs(task)
}

// Goal is the goal DTO shape (ARTEX/server/dto.go:222-248).
type Goal struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
	State     string `json:"state"`
	Origin    string `json:"origin,omitempty"`
	TS        string `json:"ts"`
}

func goalDTO(node Node) Goal {
	var payload struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	_ = json.Unmarshal([]byte(node.Payload), &payload)
	return Goal{ID: node.ID, Text: payload.Text, VulnClass: payload.VulnClass, State: node.State, Origin: node.Origin, TS: node.TS}
}

func goalDTOs(task *Task) []Goal {
	out := make([]Goal, 0)
	for _, node := range task.Nodes {
		if node.Type != "goal" {
			continue
		}
		out = append(out, goalDTO(node))
	}
	return out
}

func goalPayload(text, vulnClass string) string {
	payload := map[string]any{"text": text}
	if vulnClass != "" {
		payload["vulnclass"] = vulnClass
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

func (f *Fixture) taskLocked(id string) (*Task, bool) {
	for i := range f.state.Tasks {
		if f.state.Tasks[i].ID == id {
			return &f.state.Tasks[i], true
		}
	}
	return nil, false
}

func (f *Fixture) replaceTaskLocked(task *Task) {
	for i := range f.state.Tasks {
		if f.state.Tasks[i].ID == task.ID {
			f.state.Tasks[i] = *task
			return
		}
	}
}

// persistLocked writes the state file atomically (temp file + rename) so a
// restart reads either the previous or the new state, never a torn document.
func (f *Fixture) persistLocked() error {
	if f.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(f.state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".artex-state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, f.path)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func atoiDefault(raw string, fallback int) int {
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	return fallback
}

func csvValues(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
