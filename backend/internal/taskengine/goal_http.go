package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
)

// ValidateGoalFixtureDeployment fails before serving a synthetic mutation route.
// Flags alone do not establish an isolated database. No caller DSN is logged.
func ValidateGoalFixtureDeployment(ctx context.Context, s *Store, enabled, development bool) error {
	if !enabled {
		return nil
	}
	if !development || os.Getenv("BBA_FIXTURE_ONLY") != "1" {
		return errors.New("offline-goal-fixture requires explicit isolated development fixture")
	}
	cfg := s.Pool.Config().ConnConfig
	ip := net.ParseIP(cfg.Host)
	if cfg.Host != "/fixture" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("offline-goal-fixture requires a local isolated database")
	}
	var head int
	if e := s.Pool.QueryRow(ctx, "SELECT max(version) FROM core.schema_version").Scan(&head); e != nil {
		return e
	}
	if head != SchemaHead() {
		return errors.New("offline-goal-fixture requires current schema; apply explicit migration first")
	}
	var user, db string
	if e := s.Pool.QueryRow(ctx, "SELECT current_user,current_database()").Scan(&user, &db); e != nil {
		return e
	}
	if user != "bba_fixture" && (user != "bba_dev" || db != "bba_dev") {
		return errors.New("offline-goal-fixture database identity is not an isolated fixture")
	}
	return nil
}

type goalControlRequest struct {
	ExpectedVersion int `json:"expected_version"`
}

func (g *GoalService) Handler(auth func(*http.Request) (Actor, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if auth == nil {
			goalHTTPError(w, r, problem(taskCodeUnauthenticated))
			return
		}
		a, e := auth(r)
		if e != nil {
			goalHTTPError(w, r, e)
			return
		}
		write := r.Method != http.MethodGet
		if e = g.authorize(r.Context(), a, write); e != nil {
			goalHTTPError(w, r, e)
			return
		}
		const base = "/v1/operator/goal-runs"
		path := r.URL.Path
		if path != base && !strings.HasPrefix(path, base+"/") {
			goalHTTPError(w, r, problem(taskCodeNotFound))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, "GET, POST")
			return
		}
		key := ""
		if write {
			key, e = thIdempotencyKey(r)
			if e != nil {
				goalHTTPError(w, r, e)
				return
			}
			if !g.fixture {
				goalHTTPError(w, r, problem("GOAL_FIXTURE_DISABLED"))
				return
			}
		}
		reply := func(data any) {
			thWriteJSON(w, http.StatusOK, map[string]any{"data": data, "meta": map[string]any{"fixture_enabled": g.fixture, "implementation": "deterministic_fixture"}})
		}
		if path == base {
			if r.Method == http.MethodGet {
				v, e := g.List(r.Context(), a)
				if e != nil {
					goalHTTPError(w, r, e)
					return
				}
				reply(v)
				return
			}
			var b GoalStart
			if !goalHTTPDecode(w, r, &b) {
				return
			}
			v, e := g.Start(r.Context(), a, key, b)
			if e != nil {
				goalHTTPError(w, r, e)
				return
			}
			reply(v)
			return
		}
		parts := strings.Split(strings.TrimPrefix(path, base+"/"), "/")
		if len(parts) > 2 || !taskValidID(parts[0]) {
			goalHTTPError(w, r, problem(taskCodeNotFound))
			return
		}
		id := parts[0]
		if len(parts) == 1 {
			if r.Method != http.MethodGet {
				thMethodNotAllowed(w, r, "GET")
				return
			}
			v, e := g.Get(r.Context(), a, id)
			if e != nil {
				goalHTTPError(w, r, e)
				return
			}
			reply(v)
			return
		}
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, "POST")
			return
		}
		var result GoalRun
		switch parts[1] {
		case "fixture-windows":
			var b GoalWindow
			if !goalHTTPDecode(w, r, &b) {
				return
			}
			result, e = g.Inject(r.Context(), a, id, key, b)
		case "stop", "confirm":
			var b goalControlRequest
			if !goalHTTPDecode(w, r, &b) {
				return
			}
			result, e = g.Control(r.Context(), a, id, parts[1], b.ExpectedVersion, key)
		default:
			goalHTTPError(w, r, problem(taskCodeNotFound))
			return
		}
		if e != nil {
			goalHTTPError(w, r, e)
			return
		}
		reply(result)
	})
}
func goalHTTPDecode(w http.ResponseWriter, r *http.Request, out any) bool {
	raw, e := modelDecodeStrict(w, r, out)
	if e != nil || raw == nil {
		goalHTTPError(w, r, problem(taskCodeValidation))
		return false
	}
	if !goalRequiredMembers(raw, out) {
		goalHTTPError(w, r, problem(taskCodeValidation))
		return false
	}
	var values any
	if json.Unmarshal(goalJSON(raw), &values) != nil || !goalWireValues(values) {
		goalHTTPError(w, r, problem(taskCodeValidation))
		return false
	}
	return true
}
func goalHTTPError(w http.ResponseWriter, r *http.Request, e error) {
	status := http.StatusConflict
	code := Code(e)
	switch code {
	case taskCodeUnauthenticated:
		status = 401
	case taskCodeForbidden:
		status = 403
	case taskCodeNotFound:
		status = 404
	case taskCodeValidation:
		status = 400
	case taskCodeStoreUnavailable:
		status = 503
	case "GOAL_FIXTURE_DISABLED":
		status = 403
	}
	thWriteErrorCode(w, r, code, status)
}

// Goal request contracts have no nullable fields. Recursively enforce that
// constraint and canonical UTC spelling before typed semantic validation.
func goalWireValues(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case map[string]any:
		for key, child := range v {
			switch key {
			case "window_start", "window_end", "expires_at", "at":
				text, ok := child.(string)
				if !ok || !strings.HasSuffix(text, "Z") {
					return false
				}
			}
			if !goalWireValues(child) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !goalWireValues(child) {
				return false
			}
		}
	}
	return true
}

func goalRequiredMembers(raw map[string]json.RawMessage, out any) bool {
	has := func(object map[string]json.RawMessage, keys ...string) bool {
		for _, key := range keys {
			if _, ok := object[key]; !ok {
				return false
			}
		}
		return true
	}
	switch b := out.(type) {
	case *GoalStart:
		if !has(raw, "thread_id", "group_id", "goal", "planner_ref", "analyst_ref") || b.PlannerRef.Version < 1 || b.AnalystRef.Version < 1 {
			return false
		}
		for _, name := range []string{"planner_ref", "analyst_ref"} {
			var ref map[string]json.RawMessage
			if json.Unmarshal(raw[name], &ref) != nil || !has(ref, "id", "version") {
				return false
			}
		}
	case *GoalWindow:
		if !has(raw, "event_id", "group_id", "window_start", "window_end", "expires_at", "messages") {
			return false
		}
		var messages []map[string]json.RawMessage
		if json.Unmarshal(raw["messages"], &messages) != nil {
			return false
		}
		for _, message := range messages {
			if !has(message, "id", "at", "text") {
				return false
			}
		}
	case *goalControlRequest:
		return has(raw, "expected_version") && b.ExpectedVersion > 0
	default:
		return false
	}
	return true
}
