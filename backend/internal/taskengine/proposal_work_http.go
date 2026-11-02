package taskengine

// Operator HTTP surface for proposal-driven execution. It is unmounted by the
// default Handler; wiring calls OrchestrationHandler(auth, enabled) explicitly,
// exactly like ProposalHandler. The enabled flag is the same feature gate as the
// worker: disabled means 403 FEATURE_DISABLED for every route, including reads.

import (
	"net/http"
	"strings"

	"bba.local/taskengine/internal/contract"
	"github.com/oapi-codegen/nullable"
)

type orchestrationHTTP struct {
	store   *Store
	auth    func(*http.Request) (Actor, error)
	enabled bool
}

type workItemListData struct {
	Items []WorkItemView `json:"items"`
}

type workItemListEnvelope struct {
	Data workItemListData     `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

// OrchestrationHandler serves proposal-mode operator routes:
//
//	POST /v1/tasks/{id}/orchestration
//	POST /v1/tasks/{id}/plan-proposals/{pid}/materialize
//	GET  /v1/tasks/{id}/work-items
func (s *Store) OrchestrationHandler(auth func(*http.Request) (Actor, error), enabled bool) http.Handler {
	return &orchestrationHTTP{store: s, auth: auth, enabled: enabled}
}

func (h *orchestrationHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.enabled {
		thWriteError(w, r, problem(taskCodeFeatureDisabled))
		return
	}
	if h.auth == nil {
		thWriteError(w, r, problem(taskCodeUnauthenticated))
		return
	}
	actor, err := h.auth(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if taskBlank(actor.WorkspaceID) || taskBlank(actor.PrincipalID) {
		thWriteError(w, r, problem(taskCodeUnauthenticated))
		return
	}
	if actor.RuntimeID != "" {
		thWriteError(w, r, problem(taskCodeForbidden))
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	if rest == r.URL.Path {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	parts := strings.Split(rest, "/")
	switch {
	case len(parts) == 2 && parts[1] == "orchestration":
		taskID := parts[0]
		if !taskValidID(taskID) {
			thWriteError(w, r, problem(taskCodeNotFound))
			return
		}
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.setOrchestrationMode(w, r, actor, taskID)
	case len(parts) == 2 && parts[1] == "work-items":
		taskID := parts[0]
		if !taskValidID(taskID) {
			thWriteError(w, r, problem(taskCodeNotFound))
			return
		}
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.listWorkItems(w, r, actor, taskID)
	case len(parts) == 4 && parts[1] == "plan-proposals" && parts[3] == "materialize":
		taskID, proposalID := parts[0], parts[2]
		if !taskValidID(taskID) || !taskValidID(proposalID) {
			thWriteError(w, r, problem(taskCodeNotFound))
			return
		}
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.materialize(w, r, actor, taskID, proposalID)
	default:
		thWriteError(w, r, problem(taskCodeNotFound))
	}
}

func (h *orchestrationHTTP) setOrchestrationMode(w http.ResponseWriter, r *http.Request, actor Actor, taskID string) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	var request OrchestrationModeRequest
	raw, err := thDecodeStrict(w, r, &request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := thRequireKeys(raw, "expected_version", "mode", "reason"); err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := thRejectNulls(raw, "expected_version", "mode", "reason"); err != nil {
		thWriteError(w, r, err)
		return
	}
	receipt, err := h.store.SetOrchestrationMode(r.Context(), actor, taskID, key, request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusAccepted, contract.ReceiptEnvelope{Data: receipt, Meta: thMeta(r)})
}

func (h *orchestrationHTTP) materialize(w http.ResponseWriter, r *http.Request, actor Actor, taskID, proposalID string) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	var request MaterializeRequest
	raw, err := thDecodeStrict(w, r, &request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := thRequireKeys(raw, "expected_version", "approval_ref", "proposal_digest"); err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := thRejectNulls(raw, "expected_version", "approval_ref", "proposal_digest"); err != nil {
		thWriteError(w, r, err)
		return
	}
	receipt, err := h.store.MaterializePlanProposal(r.Context(), actor, taskID, proposalID, key, request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusAccepted, contract.ReceiptEnvelope{Data: receipt, Meta: thMeta(r)})
}

func (h *orchestrationHTTP) listWorkItems(w http.ResponseWriter, r *http.Request, actor Actor, taskID string) {
	cursor, limit, err := p019PageValues(r.URL.Query())
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	items, next, hasMore, err := h.store.ListWorkItems(r.Context(), actor, taskID, cursor, limit)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	meta := thMeta(r)
	meta.HasMore = hasMore
	meta.NextCursor = nullable.NewNullNullable[string]()
	if next != "" {
		meta.NextCursor = nullable.NewNullableWithValue(next)
	}
	thWriteJSON(w, http.StatusOK, workItemListEnvelope{
		Data: workItemListData{Items: items},
		Meta: meta,
	})
}
