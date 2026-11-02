package taskengine

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strings"
)

const WebSearchPath = "/v1/operator/system/web-search"

type searchBoundAgentProvider struct {
	boundAgentProvider
	query SearchQuery
}

func (p searchBoundAgentProvider) searchBinding() SearchQuery { return p.query }
func (p searchBoundAgentProvider) Complete(ctx context.Context, call ModelCall) (ModelCompletion, error) {
	return p.boundAgentProvider.complete(ctx, call, &p.query)
}
func searchWriteError(w http.ResponseWriter, err error) {
	code := Code(err)
	status := 503
	switch code {
	case taskCodeUnauthenticated:
		status = 401
	case taskCodeForbidden:
		status = 403
	case taskCodeValidation, "SEARCH_SOURCE_UNSUPPORTED":
		status = 400
	case taskCodeUnsupportedMedia:
		status = 415
	case taskCodePayloadTooLarge:
		status = 413
	case taskCodeVersionConflict, "SEARCH_DISABLED", "SEARCH_AGENT_DISABLED", "SEARCH_KEY_MISSING", "SEARCH_BUDGET_EXCEEDED":
		status = 409
	case "SEARCH_PROVIDER_TIMEOUT":
		status = 504
	case "SEARCH_PROVIDER_CANCELLED":
		status = 408
	default:
		if strings.HasPrefix(code, "SEARCH_") {
			status = 502
		} else {
			code = taskCodeStoreUnavailable
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
}
func (r *ModelRegistry) SearchSettingsHandler(auth func(*http.Request) (Actor, error), store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		fail := func(e error) { searchWriteError(w, e) }
		if auth == nil {
			fail(problem(taskCodeUnauthenticated))
			return
		}
		a, e := auth(req)
		if e != nil || a.WorkspaceID == "" || a.PrincipalID == "" {
			fail(problem(taskCodeUnauthenticated))
			return
		}
		test := req.URL.Path == WebSearchPath+"/test"
		perm := "system.read"
		if req.Method == "PUT" {
			perm = "system.configure"
		}
		if test {
			perm = modelPermRequest
		}
		if a.RuntimeID != "" || !a.Permissions[perm] || test && !a.Permissions["operator.read"] {
			fail(problem(taskCodeForbidden))
			return
		}
		if store == nil || r == nil {
			fail(problem(taskCodeStoreUnavailable))
			return
		}
		e = store.tx(req.Context(), func(tx pgx.Tx) error { return workSettingsAuthorize(req.Context(), tx, a, perm) })
		if e != nil {
			fail(e)
			return
		}
		if (req.URL.Path != WebSearchPath && !test) || req.URL.RawQuery != "" || req.URL.ForceQuery {
			fail(problem(taskCodeValidation))
			return
		}
		if test {
			if req.Method != "POST" {
				w.WriteHeader(405)
				return
			}
			var q SearchQuery
			raw, e := modelDecodeStrict(w, req, &q)
			if e != nil {
				fail(e)
				return
			}
			if len(raw) != 1 || !validSearchQuery(q.Query) {
				fail(problem(taskCodeValidation))
				return
			}
			r.mu.Lock()
			snap, e := r.startSearchLocked(a.WorkspaceID, true)
			r.mu.Unlock()
			if e != nil {
				fail(e)
				return
			}
			out, e := r.consumeSearch(req.Context(), a.WorkspaceID, newID("searchtest"), q.Query, snap)
			if e != nil {
				fail(e)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
			return
		}
		var out SearchSettings
		switch req.Method {
		case "GET":
			out, e = r.GetSearchSettings(a.WorkspaceID, a.Permissions["system.configure"])
		case "PUT":
			var body searchWrite
			_, e = modelDecodeStrict(w, req, &body)
			if e == nil {
				out, e = r.saveSearch(a.WorkspaceID, a.PrincipalID, body)
			}
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
