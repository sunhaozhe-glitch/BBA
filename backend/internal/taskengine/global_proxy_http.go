package taskengine

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"

	"github.com/jackc/pgx/v5"
)

const GlobalProxyPath = "/v1/operator/system/global-proxy"

// No URL probe endpoint: the only test remains the authorized saved model test.
func (r *ModelRegistry) ProxySettingsHandler(auth func(*http.Request) (Actor, error), store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(err error) {
			code := Code(err)
			status := 503
			switch code {
			case taskCodeUnauthenticated:
				status = 401
			case taskCodeForbidden:
				status = 403
			case taskCodeValidation:
				status = 400
			case taskCodeVersionConflict:
				status = 409
			default:
				code = taskCodeStoreUnavailable
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
		}
		if auth == nil {
			fail(problem(taskCodeUnauthenticated))
			return
		}
		a, e := auth(req)
		if e != nil || a.WorkspaceID == "" || a.PrincipalID == "" {
			fail(problem(taskCodeUnauthenticated))
			return
		}
		permission := "system.read"
		if req.Method == "PUT" {
			permission = "system.configure"
		}
		if e = a.require(permission); e != nil || a.RuntimeID != "" {
			fail(problem(taskCodeForbidden))
			return
		}
		if store == nil {
			fail(problem(taskCodeStoreUnavailable))
			return
		}
		e = store.tx(req.Context(), func(tx pgx.Tx) error { return workSettingsAuthorize(req.Context(), tx, a, permission) })
		if e != nil {
			fail(e)
			return
		}
		if req.URL.Path != GlobalProxyPath || req.URL.RawQuery != "" || req.URL.ForceQuery {
			fail(problem(taskCodeValidation))
			return
		}
		if r == nil {
			fail(problem(taskCodeStoreUnavailable))
			return
		}
		var out ProxySettings
		switch req.Method {
		case "GET":
			out, e = r.GetProxySettings(a.WorkspaceID, a.Permissions["system.configure"])
		case "PUT":
			mt, _, me := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if me != nil || mt != "application/json" {
				w.WriteHeader(415)
				return
			}
			dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8192))
			var fields = map[string]json.RawMessage{}
			tok, de := dec.Token()
			if de != nil || tok != json.Delim('{') {
				fail(problem(taskCodeValidation))
				return
			}
			for dec.More() {
				tok, de = dec.Token()
				key, ok := tok.(string)
				if de != nil || !ok || fields[key] != nil {
					fail(problem(taskCodeValidation))
					return
				}
				var raw json.RawMessage
				if dec.Decode(&raw) != nil {
					fail(problem(taskCodeValidation))
					return
				}
				fields[key] = raw
			}
			tok, de = dec.Token()
			if de != nil || tok != json.Delim('}') || dec.Decode(new(any)) != io.EOF {
				fail(problem(taskCodeValidation))
				return
			}
			b, _ := json.Marshal(fields)
			var body proxyWrite
			// Validate the exact key set and require all non-secret fields (no nulls).
			for k := range fields {
				switch k {
				case "enabled", "url", "expected_version", "secret_action", "username", "password":
				default:
					fail(problem(taskCodeValidation))
					return
				}
			}
			if json.Unmarshal(b, &body) != nil {
				fail(problem(taskCodeValidation))
				return
			}
			out, e = r.saveProxy(a.WorkspaceID, a.PrincipalID, body)
		default:
			w.WriteHeader(405)
			return
		}
		if e != nil {
			fail(e)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	})
}
