package taskengine

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func configStatus(err error) int {
	switch Code(err) {
	case taskCodeNotFound:
		return 404
	case taskCodeVersionConflict, taskCodeStateConflict, "CONFIG_REFERENCED", "MODEL_REFERENCE_UNAVAILABLE", "SKILL_REFERENCE_UNAVAILABLE", "SKILL_VERSION_UNAVAILABLE", "SKILL_UNPUBLISHED", "AGENT_REFERENCE_UNAVAILABLE":
		return 409
	case taskCodeStoreUnavailable:
		return 503
	default:
		return 400
	}
}
func (r *ModelRegistry) ConfigHandler(auth func(*http.Request) (Actor, error), store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, code string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
		}
		a, e := auth(req)
		if e != nil || a.WorkspaceID == "" || a.PrincipalID == "" {
			fail(401, "UNAUTHENTICATED")
			return
		}
		if a.RuntimeID != "" || !a.Permissions["operator.read"] {
			fail(403, "ACTION_FORBIDDEN")
			return
		}
		if store != nil {
			var exists bool
			e = store.Pool.QueryRow(req.Context(), `SELECT EXISTS(SELECT 1 FROM core.principal WHERE workspace_id=$1 AND id=$2)`, a.WorkspaceID, a.PrincipalID).Scan(&exists)
			if e != nil {
				fail(503, taskCodeStoreUnavailable)
				return
			}
			if !exists {
				fail(403, "ACTION_FORBIDDEN")
				return
			}
		}
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/operator/"), "/")
		if len(parts) < 1 || len(parts) > 3 || (parts[0] != "agents" && parts[0] != "skills") {
			fail(404, taskCodeNotFound)
			return
		}
		kind := parts[0]
		id := ""
		if len(parts) > 1 {
			id = parts[1]
			if !taskValidID(id) {
				fail(404, taskCodeNotFound)
				return
			}
		}
		history := len(parts) == 3 && parts[2] == "history"
		if len(parts) == 3 && !history {
			fail(404, taskCodeNotFound)
			return
		}
		permission := strings.TrimSuffix(kind, "s") + ".configure"
		can := a.Permissions[permission]
		if req.Method == "GET" {
			r.mu.Lock()
			defer r.mu.Unlock()
			d := r.cloneConfig()
			var result any
			if kind == "agents" {
				items := d.Agents[a.WorkspaceID]
				if items == nil {
					items = []AgentProfile{}
				}
				result = items
				if history {
					result = d.AgentHistory[configKey(a.WorkspaceID, id)]
					if len(d.AgentHistory[configKey(a.WorkspaceID, id)]) == 0 {
						fail(404, taskCodeNotFound)
						return
					}
				} else if id != "" {
					found := false
					for _, x := range items {
						if x.ID == id {
							result = x
							found = true
						}
					}
					if !found {
						fail(404, taskCodeNotFound)
						return
					}
				}
			} else {
				items := d.Skills[a.WorkspaceID]
				if items == nil {
					items = []SkillProfile{}
				}
				result = items
				if history {
					result = d.SkillHistory[configKey(a.WorkspaceID, id)]
					if len(d.SkillHistory[configKey(a.WorkspaceID, id)]) == 0 {
						fail(404, taskCodeNotFound)
						return
					}
				} else if id != "" {
					found := false
					for _, x := range items {
						if x.ID == id {
							result = x
							found = true
						}
					}
					if !found {
						fail(404, taskCodeNotFound)
						return
					}
				}
			}
			if id == "" || history {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": result, "can_configure": can}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": result})
			}
			return
		}
		if !can {
			fail(403, "ACTION_FORBIDDEN")
			return
		}
		if history || !((req.Method == "POST" && id == "") || ((req.Method == "PUT" || req.Method == "DELETE") && id != "")) {
			fail(405, "METHOD_NOT_ALLOWED")
			return
		}
		if !strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
			fail(415, "UNSUPPORTED_MEDIA_TYPE")
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, 64<<10)
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		var result any
		if kind == "agents" {
			var body struct {
				AgentProfile
				ExpectedVersion int `json:"expected_version"`
			}
			if dec.Decode(&body) != nil || dec.Decode(new(any)) != io.EOF {
				fail(400, taskCodeValidation)
				return
			}
			if body.ID != "" || body.Version != 0 || body.UpdatedBy != "" || body.UpdatedAt != "" || body.Deleted {
				fail(400, taskCodeValidation)
				return
			}
			result, e = r.saveAgent(a.WorkspaceID, a.PrincipalID, id, body.AgentProfile, body.ExpectedVersion, req.Method == "DELETE")
		} else {
			var body struct {
				SkillProfile
				ExpectedVersion int `json:"expected_version"`
			}
			if dec.Decode(&body) != nil || dec.Decode(new(any)) != io.EOF {
				fail(400, taskCodeValidation)
				return
			}
			if body.ID != "" || body.Version != 0 || body.UpdatedBy != "" || body.UpdatedAt != "" || body.Deleted {
				fail(400, taskCodeValidation)
				return
			}
			result, e = r.saveSkill(a.WorkspaceID, a.PrincipalID, id, body.SkillProfile, body.ExpectedVersion, req.Method == "DELETE")
		}
		if e != nil {
			fail(configStatus(e), Code(e))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": result})
	})
}
