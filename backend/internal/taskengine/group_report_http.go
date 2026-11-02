package taskengine

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// groupReportPathPrefix scopes the read-only operator report surface. It reads the
// same durable GroupContext projection the Character runtime sees, but it never
// accepts a runtime credential and never writes, sends, issues a permit or
// contacts QQ/ARTEX/model providers.
const groupReportPathPrefix = "/v1/accounts/"

type groupReportHTTP struct {
	store *Store
	auth  func(*http.Request) (Actor, error)
}

// GroupReportHandler returns the operator GroupContext report endpoint:
//
//	GET /v1/accounts/{account_id}/groups/{group_id}/context?summary_id=...&summary_version=N
//
// The handler is not mounted by Store.Handler; the server must wire it with the
// trusted operator authenticator, exactly like LoginHandler/ModelHandler.
// Authentication failures are UNAUTHENTICATED, runtime credentials are
// ACTION_FORBIDDEN, and account/group/version misses are RESOURCE_NOT_FOUND;
// the HTTP layer maps these to 401/403/404. Omitting both query parameters
// reads the newest live Summary; supplying summary_id requires
// summary_version >= 1, matching the runtime GroupContext semantics.
func (s *Store) GroupReportHandler(auth func(*http.Request) (Actor, error)) http.Handler {
	return &groupReportHTTP{store: s, auth: auth}
}

func (h *groupReportHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	accountID, groupID, ref, err := groupReportRoute(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if r.Method != http.MethodGet {
		thMethodNotAllowed(w, r, http.MethodGet)
		return
	}
	actor, err := h.authenticate(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	out, err := h.store.GroupContextForOperator(r.Context(), actor, accountID, groupID, ref)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	thWriteJSON(w, http.StatusOK, chatEnvelope{Data: out, Meta: thMeta(r)})
}

func (h *groupReportHTTP) authenticate(r *http.Request) (Actor, error) {
	if h.auth == nil {
		return Actor{}, problem(taskCodeUnauthenticated)
	}
	actor, err := h.auth(r)
	if err != nil {
		var coded *Error
		if errors.As(err, &coded) {
			return Actor{}, coded
		}
		return Actor{}, problem(taskCodeUnauthenticated)
	}
	if taskBlank(actor.WorkspaceID) || taskBlank(actor.PrincipalID) {
		return Actor{}, problem(taskCodeUnauthenticated)
	}
	return actor, nil
}

// GroupContextForOperator projects one account/group GroupContext for an
// operator report. It rejects runtime actors (no credential impersonation),
// requires operator.read, task.read or task.manage_all, enforces the actor's
// workspace and optional account scope, then reuses groupContextRead: the same
// validation, transaction, expiry/retention filters and Summary projection as
// GroupContextForRuntime. It performs no write.
func (s *Store) GroupContextForOperator(ctx context.Context, actor Actor, accountID, groupID string, ref SummaryRef) (GroupContext, error) {
	out := GroupContext{AccountID: accountID, GroupID: groupID, Topics: []GroupContextTopic{}}
	if err := groupReportReadAllowed(actor); err != nil {
		return out, err
	}
	if len(actor.AccountIDs) > 0 && !contains(actor.AccountIDs, accountID) {
		return out, problem(taskCodeNotFound)
	}
	return s.groupContextRead(ctx, actor.WorkspaceID, accountID, groupID, ref, nil)
}

func groupReportReadAllowed(actor Actor) error {
	if taskBlank(actor.WorkspaceID) || taskBlank(actor.PrincipalID) {
		return problem(taskCodeUnauthenticated)
	}
	if actor.RuntimeID != "" {
		return problem(taskCodeForbidden)
	}
	if !actor.Permissions["operator.read"] && !actor.Permissions["task.read"] && !actor.Permissions["task.manage_all"] {
		return problem(taskCodeForbidden)
	}
	return nil
}

func groupReportRoute(r *http.Request) (accountID, groupID string, ref SummaryRef, err error) {
	if !strings.HasPrefix(r.URL.Path, groupReportPathPrefix) {
		return "", "", ref, problem(taskCodeNotFound)
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, groupReportPathPrefix), "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] != "groups" || parts[2] == "" || parts[3] != "context" {
		return "", "", ref, problem(taskCodeNotFound)
	}
	accountID, groupID = parts[0], parts[2]
	if !chatValidID(accountID) || !taskValidQQID(groupID) {
		return "", "", ref, problem(taskCodeValidation)
	}
	values := r.URL.Query()
	for key := range values {
		if key != "summary_id" && key != "summary_version" {
			return "", "", ref, problem(taskCodeValidation)
		}
		if len(values[key]) != 1 {
			return "", "", ref, problem(taskCodeValidation)
		}
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
