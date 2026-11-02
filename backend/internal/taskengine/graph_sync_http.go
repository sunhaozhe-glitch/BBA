package taskengine

// Unmounted ARTEX graph-sync HTTP surface.
//
// Handler does not mount these routes. A caller must wire GraphSyncHandler with
// trusted operator authentication, mirroring ProposalHandler. Every route is
// operator-scoped (a runtime credential is rejected), reads never call ARTEX, and
// no route dispatches to QQ: the only external mutation is an ARTEX goal write
// queued through the outbox worker.
//
// A missing provider is a first-class state, not an error: GET status answers 200
// with provider.configured=false and sync_state="not_configured". Mutating routes
// answer GRAPH_SYNC_NOT_CONFIGURED (503). There is no local-graph fallback.

import (
	"net/http"
	"strings"

	"bba.local/taskengine/internal/artex"
	"bba.local/taskengine/internal/contract"
	"github.com/oapi-codegen/nullable"
)

type graphSyncHTTP struct {
	store    *Store
	auth     func(*http.Request) (Actor, error)
	provider GraphSyncProvider
}

// GraphSyncHandler is not mounted by Handler. Wiring it is an explicit
// development/operator decision; provider may be nil to expose the
// not-configured state.
func (s *Store) GraphSyncHandler(auth func(*http.Request) (Actor, error), provider GraphSyncProvider) http.Handler {
	return &graphSyncHTTP{store: s, auth: auth, provider: provider}
}

func (h *graphSyncHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		graphSyncWriteError(w, r, problem(taskCodeUnauthenticated))
		return
	}
	actor, err := h.auth(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if taskBlank(actor.WorkspaceID) || taskBlank(actor.PrincipalID) {
		graphSyncWriteError(w, r, problem(taskCodeUnauthenticated))
		return
	}
	if actor.RuntimeID != "" {
		graphSyncWriteError(w, r, problem(taskCodeForbidden))
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/tasks/")
	if !ok {
		graphSyncWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || taskBlank(parts[0]) || parts[1] != "graph-sync" {
		graphSyncWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	taskID := parts[0]
	if !taskValidID(taskID) {
		graphSyncWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	switch {
	case len(parts) == 2:
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.status(w, r, actor, taskID)
	case len(parts) == 4 && parts[2] == "roots":
		if r.Method != http.MethodPut {
			thMethodNotAllowed(w, r, http.MethodPut)
			return
		}
		h.bind(w, r, actor, taskID, parts[3])
	case len(parts) == 3 && parts[2] == "planning-snapshots":
		switch r.Method {
		case http.MethodGet:
			h.listSnapshots(w, r, actor, taskID)
		case http.MethodPost:
			h.createSnapshot(w, r, actor, taskID)
		default:
			thMethodNotAllowed(w, r, "GET, POST")
		}
	case len(parts) == 4 && parts[2] == "planning-snapshots":
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.getSnapshot(w, r, actor, taskID, parts[3])
	case len(parts) == 5 && parts[2] == "outbox" && parts[4] == "reconcile":
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.reconcile(w, r, actor, taskID, parts[3])
	default:
		graphSyncWriteError(w, r, problem(taskCodeNotFound))
	}
}

func (h *graphSyncHTTP) status(w http.ResponseWriter, r *http.Request, actor Actor, taskID string) {
	status, err := h.store.GetGraphSyncStatus(r.Context(), actor, taskID)
	if err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, struct {
		Data GraphSyncStatus      `json:"data"`
		Meta contract.RequestMeta `json:"meta"`
	}{Data: withProvider(status, h.provider), Meta: thMeta(r)})
}

func (h *graphSyncHTTP) bind(w http.ResponseWriter, r *http.Request, actor Actor, taskID, kind string) {
	if h.provider == nil || !h.provider.Wired() {
		graphSyncWriteError(w, r, problem(graphSyncCodeNotConfigured))
		return
	}
	if kind == graphSyncKindGoal && !h.provider.Capabilities().Writes {
		graphSyncWriteError(w, r, problem(graphSyncCodeWritesDisabled))
		return
	}
	key, err := thIdempotencyKey(r)
	if err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	var request GraphRootRequest
	if _, err := thDecodeStrict(w, r, &request); err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	status, err := h.store.BindGraphRoot(r.Context(), actor, taskID, kind, key, request)
	if err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, struct {
		Data GraphSyncStatus      `json:"data"`
		Meta contract.RequestMeta `json:"meta"`
	}{Data: withProvider(status, h.provider), Meta: thMeta(r)})
}

func (h *graphSyncHTTP) createSnapshot(w http.ResponseWriter, r *http.Request, actor Actor, taskID string) {
	if h.provider == nil || !h.provider.Wired() {
		graphSyncWriteError(w, r, problem(graphSyncCodeNotConfigured))
		return
	}
	key, err := thIdempotencyKey(r)
	if err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	snapshot, err := h.store.CreateGraphPlanningSnapshot(r.Context(), actor, taskID, key, h.provider)
	if err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusCreated, struct {
		Data GraphPlanningSnapshot `json:"data"`
		Meta contract.RequestMeta  `json:"meta"`
	}{Data: snapshot, Meta: thMeta(r)})
}

func (h *graphSyncHTTP) listSnapshots(w http.ResponseWriter, r *http.Request, actor Actor, taskID string) {
	snapshots, err := h.store.ListGraphPlanningSnapshots(r.Context(), actor, taskID, 20)
	if err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	if snapshots == nil {
		snapshots = []GraphPlanningSnapshot{}
	}
	thWriteJSON(w, http.StatusOK, struct {
		Data struct {
			Items []GraphPlanningSnapshot `json:"items"`
		} `json:"data"`
		Meta contract.RequestMeta `json:"meta"`
	}{Data: struct {
		Items []GraphPlanningSnapshot `json:"items"`
	}{Items: snapshots}, Meta: thMeta(r)})
}

func (h *graphSyncHTTP) getSnapshot(w http.ResponseWriter, r *http.Request, actor Actor, taskID, snapshotID string) {
	snapshot, err := h.store.GetGraphPlanningSnapshot(r.Context(), actor, taskID, snapshotID)
	if err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, struct {
		Data GraphPlanningSnapshot `json:"data"`
		Meta contract.RequestMeta  `json:"meta"`
	}{Data: snapshot, Meta: thMeta(r)})
}

func (h *graphSyncHTTP) reconcile(w http.ResponseWriter, r *http.Request, actor Actor, taskID, itemID string) {
	if h.provider == nil || !h.provider.Wired() {
		graphSyncWriteError(w, r, problem(graphSyncCodeNotConfigured))
		return
	}
	key, err := thIdempotencyKey(r)
	if err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	var request GraphSyncReconcileRequest
	if _, err := thDecodeStrict(w, r, &request); err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	item, err := h.store.ReconcileGraphSyncItem(r.Context(), actor, taskID, itemID, key, request, h.provider)
	if err != nil {
		graphSyncWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, struct {
		Data GraphSyncOutboxItem  `json:"data"`
		Meta contract.RequestMeta `json:"meta"`
	}{Data: item, Meta: thMeta(r)})
}

// withProvider overlays the real client state. A nil or unwired provider reports
// every graph as not_configured instead of leaving a stale "synced" label.
func withProvider(status GraphSyncStatus, provider GraphSyncProvider) GraphSyncStatus {
	info := GraphSyncProviderStatus{Provider: graphSyncProviderArtex, Configured: provider != nil}
	if provider != nil {
		info.Wired = provider.Wired()
		caps := provider.Capabilities()
		info.WritesAllowed = caps.Writes
	}
	status.Provider = info
	if !info.Configured || !info.Wired {
		for i := range status.Graphs {
			status.Graphs[i].SyncState = "not_configured"
		}
	}
	return status
}

var graphSyncStatusByCode = map[string]int{
	graphSyncCodeNotConfigured:   http.StatusServiceUnavailable,
	graphSyncCodeRootNotBound:    http.StatusConflict,
	graphSyncCodeWritesDisabled:  http.StatusForbidden,
	graphSyncCodeUnauthorized:    http.StatusBadGateway,
	graphSyncCodeForbidden:       http.StatusBadGateway,
	graphSyncCodeNotFound:        http.StatusBadGateway,
	graphSyncCodeProtocol:        http.StatusBadGateway,
	graphSyncCodeUnavailable:     http.StatusBadGateway,
	graphSyncCodeRejected:        http.StatusBadGateway,
	graphSyncCodeOutcomeUnknown:  http.StatusConflict,
	graphSyncCodeGoalUnmapped:    http.StatusConflict,
	graphSyncCodeSnapshotStale:   http.StatusConflict,
	graphSyncCodeSnapshotExpired: http.StatusConflict,
	graphSyncCodePlanningBudget:  http.StatusTooManyRequests,
}

var graphSyncMessageByCode = map[string]string{
	graphSyncCodeNotConfigured:   "ARTEX graph provider is not configured",
	graphSyncCodeRootNotBound:    "task has no external graph root for this graph kind",
	graphSyncCodeWritesDisabled:  "ARTEX writes are disabled for this deployment",
	graphSyncCodeUnauthorized:    "ARTEX rejected the configured credential",
	graphSyncCodeForbidden:       "ARTEX refused this credential for the requested write",
	graphSyncCodeNotFound:        "the external graph root or goal was not found",
	graphSyncCodeProtocol:        "ARTEX answered with an unexpected protocol payload",
	graphSyncCodeUnavailable:     "ARTEX is unreachable or failing",
	graphSyncCodeRejected:        "ARTEX rejected the request",
	graphSyncCodeOutcomeUnknown:  "external outcome is unknown; reconcile by readback, do not resend",
	graphSyncCodeGoalUnmapped:    "the local goal is not mapped to an external goal id",
	graphSyncCodeSnapshotStale:   "the planning snapshot is stale; its basic facts changed",
	graphSyncCodeSnapshotExpired: "the planning snapshot has expired",
	graphSyncCodePlanningBudget:  "planning round budget exhausted for this window",
}

func graphSyncWriteError(w http.ResponseWriter, r *http.Request, err error) {
	code := Code(err)
	status, ok := graphSyncStatusByCode[code]
	if !ok {
		thWriteError(w, r, err)
		return
	}
	message, ok := graphSyncMessageByCode[code]
	if !ok {
		message = "graph sync request failed"
	}
	thWriteJSON(w, status, contract.ErrorEnvelope{
		Error: contract.Error{
			Code:         code,
			Details:      map[string]contract.Error_Details_AdditionalProperties{},
			Message:      message,
			RetryAfterMs: nullable.NewNullNullable[int](),
			// Only a transport/provider outage is worth retrying; a 401/404/protocol
			// answer, a disabled write or an ambiguous outcome never becomes success
			// by repeating the same request.
			Retryable: code == graphSyncCodeUnavailable,
		},
		Meta: thMeta(r),
	})
}

// The real client satisfies the worker/store interface directly; this assertion
// keeps a signature drift from compiling silently.
var _ GraphSyncProvider = (*artex.Client)(nil)
