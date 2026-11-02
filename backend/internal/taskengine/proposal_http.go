package taskengine

// Explicit, unmounted proposal HTTP surface.
//
// The normal Task Engine Handler does not mount these routes. A caller must
// wire ProposalHandler with trusted operator authentication and an explicit
// enabled flag, mirroring ModelHandler. Nothing here dispatches to QQ: create
// returns a receipt, approve binds an approval row, and apply only writes the
// task-kernel proposal ledger.

import (
	"encoding/json"
	"net/http"
	"strings"

	"bba.local/taskengine/internal/contract"
)

type proposalHTTP struct {
	store   *Store
	auth    func(*http.Request) (Actor, error)
	enabled bool
}

type proposalApprovalEnvelope struct {
	Data PlanProposalApprovalReceipt `json:"data"`
	Meta contract.RequestMeta        `json:"meta"`
}

type proposalEnvelope struct {
	Data PlanProposalView     `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

// ProposalHandler is not mounted by Handler. Wiring it is an explicit
// development/operator decision.
func (s *Store) ProposalHandler(auth func(*http.Request) (Actor, error), enabled bool) http.Handler {
	return &proposalHTTP{store: s, auth: auth, enabled: enabled}
}

func (h *proposalHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	case len(parts) == 2 && parts[1] == "plan-proposals":
		taskID := parts[0]
		if !taskValidID(taskID) {
			thWriteError(w, r, problem(taskCodeNotFound))
			return
		}
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.createPlanProposal(w, r, actor, taskID)
	case len(parts) == 3 && parts[1] == "plan-proposals":
		taskID, proposalID := parts[0], parts[2]
		if !taskValidID(taskID) || !taskValidID(proposalID) {
			thWriteError(w, r, problem(taskCodeNotFound))
			return
		}
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.getPlanProposal(w, r, actor, taskID, proposalID)
	case len(parts) == 4 && parts[1] == "plan-proposals" && parts[3] == "approve":
		taskID, proposalID := parts[0], parts[2]
		if !taskValidID(taskID) || !taskValidID(proposalID) {
			thWriteError(w, r, problem(taskCodeNotFound))
			return
		}
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.approvePlanProposal(w, r, actor, taskID, proposalID)
	case len(parts) == 4 && parts[1] == "plan-proposals" && parts[3] == "apply":
		taskID, proposalID := parts[0], parts[2]
		if !taskValidID(taskID) || !taskValidID(proposalID) {
			thWriteError(w, r, problem(taskCodeNotFound))
			return
		}
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.applyPlanProposal(w, r, actor, taskID, proposalID)
	default:
		thWriteError(w, r, problem(taskCodeNotFound))
	}
}

func (h *proposalHTTP) getPlanProposal(w http.ResponseWriter, r *http.Request, actor Actor, taskID, proposalID string) {
	view, err := h.store.GetPlanProposal(r.Context(), actor, taskID, proposalID)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, proposalEnvelope{Data: view, Meta: thMeta(r)})
}

func (h *proposalHTTP) createPlanProposal(w http.ResponseWriter, r *http.Request, actor Actor, taskID string) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	var request contract.PlanProposalRequest
	raw, err := thDecodeStrict(w, r, &request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := proposalValidateCreatePresence(raw); err != nil {
		thWriteError(w, r, err)
		return
	}
	receipt, err := h.store.CreatePlanProposal(r.Context(), actor, taskID, key, request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusAccepted, contract.ReceiptEnvelope{Data: receipt, Meta: thMeta(r)})
}

func (h *proposalHTTP) approvePlanProposal(w http.ResponseWriter, r *http.Request, actor Actor, taskID, proposalID string) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	var request PlanProposalApprovalRequest
	raw, err := thDecodeStrict(w, r, &request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := proposalValidateApprovalPresence(raw); err != nil {
		thWriteError(w, r, err)
		return
	}
	receipt, err := h.store.ApprovePlanProposal(r.Context(), actor, taskID, proposalID, key, request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusAccepted, proposalApprovalEnvelope{Data: receipt, Meta: thMeta(r)})
}

func (h *proposalHTTP) applyPlanProposal(w http.ResponseWriter, r *http.Request, actor Actor, taskID, proposalID string) {
	key, err := thIdempotencyKey(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	var request contract.PlanApplyRequest
	raw, err := thDecodeStrict(w, r, &request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if err := proposalValidateApplyPresence(raw); err != nil {
		thWriteError(w, r, err)
		return
	}
	receipt, err := h.store.ApplyPlanProposal(r.Context(), actor, taskID, proposalID, key, request)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusAccepted, contract.ReceiptEnvelope{Data: receipt, Meta: thMeta(r)})
}

func proposalValidateCreatePresence(raw map[string]json.RawMessage) error {
	if err := thRequireKeys(raw, "task_id", "expected_version", "scope_revision", "proposal_kind", "reason"); err != nil {
		return err
	}
	return thRejectNulls(raw, "task_id", "expected_version", "scope_revision", "proposal_kind", "reason")
}

func proposalValidateApprovalPresence(raw map[string]json.RawMessage) error {
	if err := thRequireKeys(raw, "expected_version", "approval_ref"); err != nil {
		return err
	}
	return thRejectNulls(raw, "expected_version", "approval_ref")
}

func proposalValidateApplyPresence(raw map[string]json.RawMessage) error {
	if err := thRequireKeys(raw, "expected_version", "approval_ref", "proposal_digest"); err != nil {
		return err
	}
	return thRejectNulls(raw, "expected_version", "approval_ref", "proposal_digest")
}
