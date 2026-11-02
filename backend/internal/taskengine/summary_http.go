package taskengine

import (
	"errors"
	"net/http"
	"strconv"
)

const (
	groupSummaryIngestPath = "/v1/internal/group-summaries"
	groupContextPath       = "/v1/internal/group-context"
)

type groupSummaryHTTP struct {
	store *Store
	auth  func(*http.Request) (RuntimeActor, error)
}

// GroupSummaryRuntimeHandler exposes the collector Summary write and the
// Character GroupContext read. Both routes require a runtime credential; the
// store re-checks the account, group, provenance, and expiry for every call.
func (s *Store) GroupSummaryRuntimeHandler(auth func(*http.Request) (RuntimeActor, error)) http.Handler {
	return &groupSummaryHTTP{store: s, auth: auth}
}

func (h *groupSummaryHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == groupSummaryIngestPath:
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, http.MethodPost)
			return
		}
	case r.URL.Path == groupContextPath:
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, http.MethodGet)
			return
		}
	default:
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	actor, err := h.authenticate(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if r.URL.Path == groupSummaryIngestPath {
		h.ingest(w, r, actor)
		return
	}
	h.context(w, r, actor)
}

func (h *groupSummaryHTTP) authenticate(r *http.Request) (RuntimeActor, error) {
	if h.auth == nil {
		return RuntimeActor{}, problem(taskCodeUnauthenticated)
	}
	actor, err := h.auth(r)
	if err != nil {
		var coded *Error
		if errors.As(err, &coded) {
			return RuntimeActor{}, coded
		}
		return RuntimeActor{}, problem(taskCodeUnauthenticated)
	}
	if taskBlank(actor.WorkspaceID) || taskBlank(actor.RuntimeID) || actor.OwnerEpoch <= 0 {
		return RuntimeActor{}, problem(taskCodeUnauthenticated)
	}
	return actor, nil
}

func (h *groupSummaryHTTP) ingest(w http.ResponseWriter, r *http.Request, actor RuntimeActor) {
	var req GroupSummaryIngestRequest
	if _, err := chatDecodeStrict(w, r, &req); err != nil {
		thWriteError(w, r, err)
		return
	}
	out, err := h.store.IngestGroupSummary(r.Context(), actor, req)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	status := http.StatusOK
	if out.Created {
		status = http.StatusCreated
	}
	thWriteJSON(w, status, chatEnvelope{Data: out, Meta: thMeta(r)})
}

func (h *groupSummaryHTTP) context(w http.ResponseWriter, r *http.Request, actor RuntimeActor) {
	accountID, groupID, ref, err := groupContextQuery(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	out, err := h.store.GroupContextForRuntime(r.Context(), actor, accountID, groupID, ref)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, chatEnvelope{Data: out, Meta: thMeta(r)})
}

func groupContextQuery(r *http.Request) (accountID, groupID string, ref SummaryRef, err error) {
	values := r.URL.Query()
	for key := range values {
		if key != "account_id" && key != "group_id" && key != "summary_id" && key != "summary_version" {
			return "", "", ref, problem(taskCodeValidation)
		}
		if len(values[key]) != 1 {
			return "", "", ref, problem(taskCodeValidation)
		}
	}
	accountID, groupID = values.Get("account_id"), values.Get("group_id")
	if !chatValidID(accountID) || !taskValidQQID(groupID) {
		return "", "", ref, problem(taskCodeValidation)
	}
	summaryID := values.Get("summary_id")
	if summaryID == "" {
		if values.Get("summary_version") != "" {
			return "", "", ref, problem(taskCodeValidation)
		}
		return accountID, groupID, ref, nil
	}
	version, convErr := strconv.Atoi(values.Get("summary_version"))
	if convErr != nil || version < 1 {
		return "", "", ref, problem(taskCodeValidation)
	}
	return accountID, groupID, SummaryRef{SummaryID: summaryID, SummaryVersion: version}, nil
}
