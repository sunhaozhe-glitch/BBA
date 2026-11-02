package taskengine

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

const workSettingsPath = "/v1/operator/system/work-slots"
const taskWorkSlotsPath = "/v1/operator/system/task-work-slots/"

// WorkSettingsHandler receives startup facts by value. Query parameters cannot
// select another workspace (or override flags); actor scope is authoritative.
func (s *Store) WorkSettingsHandler(auth func(*http.Request) (Actor, error), runtime WorkSettingsRuntime) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, code string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
		}
		failure := func(err error) {
			code := Code(err)
			status := 503
			switch code {
			case taskCodeUnauthenticated:
				status = 401
			case taskCodeForbidden:
				status = 403
			case taskCodeNotFound:
				status = 404
			case taskCodeVersionConflict:
				status = 409
			case taskCodeValidation:
				status = 400
			default:
				code = taskCodeStoreUnavailable
			}
			fail(status, code)
		}
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		if auth == nil {
			fail(401, taskCodeUnauthenticated)
			return
		}
		actor, err := auth(r)
		if err != nil || actor.WorkspaceID == "" || actor.PrincipalID == "" {
			fail(401, taskCodeUnauthenticated)
			return
		}
		if actor.RuntimeID != "" {
			fail(403, taskCodeForbidden)
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			fail(400, taskCodeValidation)
			return
		}
		if strings.HasPrefix(r.URL.Path, taskWorkSlotsPath) {
			id := strings.TrimPrefix(r.URL.Path, taskWorkSlotsPath)
			if !taskValidID(id) || strings.Contains(id, "/") {
				fail(404, taskCodeNotFound)
				return
			}
			if r.Method != http.MethodGet {
				fail(405, taskCodeMethodNotAllowed)
				return
			}
			out, err := s.GetTaskWorkSlots(r.Context(), actor, id)
			if err != nil {
				failure(err)
				return
			}
			reply(out)
			return
		}
		if r.URL.Path != workSettingsPath {
			fail(404, taskCodeNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			out, err := s.GetWorkSettings(r.Context(), actor)
			if err != nil {
				failure(err)
				return
			}
			out.WorkSettingsRuntime = runtime
			reply(out)
		case http.MethodPut:
			if err := actor.require("system.configure"); err != nil {
				failure(err)
				return
			}
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				fail(415, taskCodeUnsupportedMedia)
				return
			}
			// Pointer fields reject omitted/null; Go integer decoding rejects floats,
			// exponent notation, strings, bools and overflow. Tokens also reject duplicate keys.
			d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			tok, err := d.Token()
			if err != nil || tok != json.Delim('{') {
				fail(400, taskCodeValidation)
				return
			}
			var slots *int
			var version *int64
			seen := map[string]bool{}
			for d.More() {
				tok, err = d.Token()
				key, ok := tok.(string)
				if err != nil || !ok || seen[key] {
					fail(400, taskCodeValidation)
					return
				}
				seen[key] = true
				switch key {
				case "work_slots":
					err = d.Decode(&slots)
				case "expected_version":
					err = d.Decode(&version)
				default:
					fail(400, taskCodeValidation)
					return
				}
				if err != nil {
					fail(400, taskCodeValidation)
					return
				}
			}
			tok, err = d.Token()
			if err != nil || tok != json.Delim('}') || d.Decode(new(any)) != io.EOF || slots == nil || version == nil {
				fail(400, taskCodeValidation)
				return
			}
			out, err := s.SaveWorkSettings(r.Context(), actor, *slots, *version)
			if err != nil {
				failure(err)
				return
			}
			out.WorkSettingsRuntime = runtime
			reply(out)
		default:
			fail(405, taskCodeMethodNotAllowed)
		}
	})
}
