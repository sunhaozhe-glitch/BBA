package taskengine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"bba.local/taskengine/internal/contract"
)

const bridgeRuntimeBasePath = "/v1/internal/bridge"

type BridgeIdentity struct {
	SelfID   string   `json:"self_id"`
	Online   bool     `json:"online"`
	Good     bool     `json:"good"`
	GroupIDs []string `json:"group_ids"`
}

type BridgeRuntime interface {
	ObserveIdentity(context.Context, RuntimeActor, bool) (BridgeIdentity, error)
	CleanerAvailable(context.Context, RuntimeActor) error
	ObserveMembership(context.Context, RuntimeActor, string) (bool, error)
	CheckIdentity(context.Context, RuntimeActor, Reservation) error
	RequestJoin(context.Context, RuntimeActor, Reservation) (SendOutcome, error)
}

type BridgeReservation struct {
	OperationID         string    `json:"operation_id"`
	ExecutionID         string    `json:"execution_id"`
	Digest              string    `json:"request_digest"`
	AccountID           string    `json:"account_id"`
	TargetID            string    `json:"target_id"`
	GroupID             string    `json:"group_id"`
	JoinReason          string    `json:"join_reason"`
	VerificationMessage string    `json:"verification_message"`
	OwnerEpoch          int64     `json:"owner_epoch"`
	Fence               int64     `json:"fence_token"`
	ScopeRevision       int       `json:"scope_revision"`
	Attempt             int       `json:"attempt"`
	Deadline            time.Time `json:"deadline_at"`
}

type bridgeReservationHTTPEnvelope struct {
	Data BridgeReservation    `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

type BridgeOperationEnvelope struct {
	Data struct {
		Operation contract.Operation       `json:"operation"`
		Permit    *contract.DispatchPermit `json:"permit"`
	} `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

type BridgeOutcomeEnvelope struct {
	Data BridgeOutcome        `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

type BridgeOutcome struct {
	Disposition     string `json:"disposition"`
	EffectCertainty string `json:"effect_certainty"`
	EvidenceRef     string `json:"evidence_ref,omitempty"`
	RequestRef      string `json:"request_ref,omitempty"`
}

type bridgeRuntimeHTTP struct {
	store   *Store
	auth    func(*http.Request) (RuntimeActor, error)
	gateway BridgeRuntime
}

// BridgeRuntimeHandler exposes only authenticated, bounded task bridge operations.
// NapCat credentials and provider endpoints remain in the Gateway implementation.
func (s *Store) BridgeRuntimeHandler(auth func(*http.Request) (RuntimeActor, error), gateway BridgeRuntime) http.Handler {
	return &bridgeRuntimeHTTP{store: s, auth: auth, gateway: gateway}
}

func (h *bridgeRuntimeHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	actor, err := h.authenticate(r)
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if h.gateway == nil {
		thWriteError(w, r, problem(taskCodeCapabilityUnavailable))
		return
	}
	if r.URL.Path == bridgeRuntimeBasePath+"/identity" {
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, "GET")
			return
		}
		withGroups := r.URL.Query().Get("with_groups")
		if (withGroups != "true" && withGroups != "false") || len(r.URL.Query()["with_groups"]) != 1 {
			thWriteError(w, r, problem(taskCodeValidation))
			return
		}
		identity, err := h.gateway.ObserveIdentity(r.Context(), actor, withGroups == "true")
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		thWriteJSON(w, http.StatusOK, struct {
			Data BridgeIdentity       `json:"data"`
			Meta contract.RequestMeta `json:"meta"`
		}{identity, thMeta(r)})
		return
	}
	if r.URL.Path == bridgeRuntimeBasePath+"/cleaner-capability" {
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, "GET")
			return
		}
		if err := h.gateway.CleanerAvailable(r.Context(), actor); err != nil {
			thWriteError(w, r, err)
			return
		}
		thWriteJSON(w, http.StatusOK, struct {
			Data struct {
				Available bool `json:"available"`
			} `json:"data"`
			Meta contract.RequestMeta `json:"meta"`
		}{Data: struct {
			Available bool `json:"available"`
		}{Available: true}, Meta: thMeta(r)})
		return
	}
	if r.URL.Path == bridgeRuntimeBasePath+"/membership" {
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, "GET")
			return
		}
		groupID := r.URL.Query().Get("group_id")
		if !taskValidQQID(groupID) || len(r.URL.Query()["group_id"]) != 1 {
			thWriteError(w, r, problem(taskCodeValidation))
			return
		}
		joined, err := h.gateway.ObserveMembership(r.Context(), actor, groupID)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		thWriteJSON(w, http.StatusOK, struct {
			Data struct {
				GroupID string `json:"group_id"`
				Joined  bool   `json:"joined"`
			} `json:"data"`
			Meta contract.RequestMeta `json:"meta"`
		}{Data: struct {
			GroupID string `json:"group_id"`
			Joined  bool   `json:"joined"`
		}{GroupID: groupID, Joined: joined}, Meta: thMeta(r)})
		return
	}
	prefix := bridgeRuntimeBasePath + "/operations/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	operationID, action, ok := strings.Cut(rest, "/")
	if !ok || !taskValidID(operationID) || strings.Contains(action, "/") {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	switch action {
	case "accept":
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, "POST")
			return
		}
		if err := bridgeRequireEmptyBody(r); err != nil {
			thWriteError(w, r, err)
			return
		}
		reservation, err := h.store.GetUnacceptedReservation(r.Context(), actor, operationID)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		accepted, err := h.store.AcceptOperation(r.Context(), actor, reservation)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		thWriteJSON(w, http.StatusOK, bridgeReservationResponse(accepted, thMeta(r)))
	case "permit":
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, "POST")
			return
		}
		if err := bridgeRequireEmptyBody(r); err != nil {
			thWriteError(w, r, err)
			return
		}
		_, err := h.store.IssuePermit(r.Context(), actor, operationID, 30*time.Second)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		permitContract, err := h.store.GetPermit(r.Context(), actor, operationID)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		thWriteJSON(w, http.StatusOK, contract.DispatchPermitEnvelope{Data: permitContract, Meta: thMeta(r)})
	case "dispatch":
		if r.Method != http.MethodPost {
			thMethodNotAllowed(w, r, "POST")
			return
		}
		if err := bridgeRequireEmptyBody(r); err != nil {
			thWriteError(w, r, err)
			return
		}
		outcome, err := h.store.Dispatch(r.Context(), actor, operationID,
			func(ctx context.Context, reservation Reservation) error {
				return h.gateway.CheckIdentity(ctx, actor, reservation)
			}, bridgeRuntimeAdapter{gateway: h.gateway, actor: actor})
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		thWriteJSON(w, http.StatusOK, BridgeOutcomeEnvelope{Data: BridgeOutcome{
			Disposition: outcome.Disposition, EffectCertainty: outcome.EffectCertainty,
			EvidenceRef: outcome.EvidenceRef, RequestRef: outcome.RequestRef,
		}, Meta: thMeta(r)})
	case "status":
		if r.Method != http.MethodGet {
			thMethodNotAllowed(w, r, "GET")
			return
		}
		operation, err := h.store.GetOperation(r.Context(), actor, operationID)
		if err != nil {
			thWriteError(w, r, err)
			return
		}
		data := BridgeOperationEnvelope{}
		data.Data.Operation = operation
		permit, err := h.store.GetPermit(r.Context(), actor, operationID)
		if err == nil {
			data.Data.Permit = &permit
		} else if Code(err) != "RESOURCE_NOT_FOUND" {
			thWriteError(w, r, err)
			return
		}
		data.Meta = thMeta(r)
		thWriteJSON(w, http.StatusOK, data)
	default:
		thWriteError(w, r, problem(taskCodeNotFound))
	}
}

func bridgeReservationResponse(res Reservation, meta contract.RequestMeta) bridgeReservationHTTPEnvelope {
	return bridgeReservationHTTPEnvelope{Data: BridgeReservation{
		OperationID: res.OperationID, ExecutionID: res.ExecutionID, Digest: res.Digest,
		AccountID: res.AccountID, TargetID: res.TargetID, GroupID: res.GroupID,
		JoinReason: res.JoinReason, VerificationMessage: res.VerificationMessage,
		OwnerEpoch: res.OwnerEpoch, Fence: res.Fence, ScopeRevision: res.ScopeRevision,
		Attempt: res.Attempt, Deadline: res.Deadline.UTC(),
	}, Meta: meta}
}

func (h *bridgeRuntimeHTTP) authenticate(r *http.Request) (RuntimeActor, error) {
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
	if taskBlank(actor.WorkspaceID) || taskBlank(actor.RuntimeID) || actor.OwnerEpoch <= 0 || len(actor.AccountIDs) == 0 {
		return RuntimeActor{}, problem(taskCodeUnauthenticated)
	}
	return actor, nil
}

type bridgeRuntimeAdapter struct {
	gateway BridgeRuntime
	actor   RuntimeActor
}

func (a bridgeRuntimeAdapter) RequestJoin(ctx context.Context, reservation Reservation) (SendOutcome, error) {
	return a.gateway.RequestJoin(ctx, a.actor, reservation)
}

func bridgeRequireEmptyBody(r *http.Request) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		return problem(taskCodeValidation)
	}
	return nil
}
