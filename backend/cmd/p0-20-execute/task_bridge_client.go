package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"bba.local/taskengine/internal/taskengine"
)

type taskBridgeClient struct {
	base    string
	token   string
	client  *http.Client
	reads   *callBudget
	writes  *callBudget
	maxBody int64
}

type bridgeIdentityResponse struct {
	Data taskengine.BridgeIdentity `json:"data"`
}
type bridgeCapabilityResponse struct {
	Data struct {
		Available bool `json:"available"`
	} `json:"data"`
}
type bridgeMembershipResponse struct {
	Data struct {
		GroupID string `json:"group_id"`
		Joined  bool   `json:"joined"`
	} `json:"data"`
}
type bridgeReservationResponse struct {
	Data taskengine.BridgeReservation `json:"data"`
}
type bridgePermitResponse struct {
	Data struct {
		PermitID      string    `json:"permit_id"`
		OperationID   string    `json:"operation_id"`
		RequestDigest string    `json:"request_digest"`
		State         string    `json:"state"`
		ExpiresAt     time.Time `json:"expires_at"`
		AccountID     string    `json:"account_id"`
		OwnerEpoch    string    `json:"owner_epoch"`
		FenceToken    string    `json:"fence_token"`
		ScopeRevision int       `json:"scope_revision"`
	} `json:"data"`
}

type chatGatewaySendRequest struct {
	SendID       string `json:"send_id"`
	SegmentIndex int    `json:"segment_index"`
	Text         string `json:"text"`
}
type chatGatewaySendResponse struct {
	Data struct {
		State     string  `json:"state"`
		MessageID *string `json:"message_id"`
	} `json:"data"`
}

// The worker has only the authenticated Task Bridge endpoint and runtime token.
func newTaskBridgeClient(origin, token string, maxReads int, ev *evidence) (*taskBridgeClient, error) {
	base, err := normalizeLoopbackURL(origin)
	if err != nil {
		return nil, fmt.Errorf("task_bridge_url: %w", err)
	}
	if strings.TrimSpace(token) != token || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("task bridge runtime token is required")
	}
	return &taskBridgeClient{base: base, token: token, client: newLoopbackHTTPClient(),
		reads: &callBudget{limit: maxReads}, writes: &callBudget{limit: 4}, maxBody: 1 << 20}, nil
}

func (c *taskBridgeClient) request(ctx context.Context, step, method, path string, body []byte, write bool) ([]byte, error) {
	budget := c.reads
	if write {
		budget = c.writes
	}
	if err := budget.take("Task Bridge"); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("Task Bridge request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("X-Request-Id", "p020-"+step)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, errors.New("Task Bridge transport result unknown")
	}
	defer response.Body.Close()
	data, err := readBoundedResponse(response.Body, c.maxBody)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		if failure.Error.Code == "EXTERNAL_OUTCOME_UNKNOWN" {
			return data, errors.New("EXTERNAL_OUTCOME_UNKNOWN")
		}
		return data, fmt.Errorf("Task Bridge %s failed", step)
	}
	return data, nil
}

func readBoundedResponse(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("Task Bridge response unavailable")
	}
	return data, nil
}

func (c *taskBridgeClient) identity(ctx context.Context, withGroups bool) (identityFacts, error) {
	var response bridgeIdentityResponse
	path := "/v1/internal/bridge/identity?with_groups=" + strconv.FormatBool(withGroups)
	body, err := c.request(ctx, "identity", http.MethodGet, path, nil, false)
	if err != nil {
		return identityFacts{}, err
	}
	if json.Unmarshal(body, &response) != nil || !validQQID(response.Data.SelfID) || !response.Data.Online || !response.Data.Good || (withGroups && response.Data.GroupIDs == nil) {
		return identityFacts{}, errors.New("Task Bridge identity evidence incomplete")
	}
	return identityFacts{SelfID: response.Data.SelfID, Online: response.Data.Online, Good: response.Data.Good,
		GroupIDs: response.Data.GroupIDs, ObservedAt: time.Now().UTC()}, nil
}

func (c *taskBridgeClient) cleanerAvailable(ctx context.Context) error {
	var response bridgeCapabilityResponse
	body, err := c.request(ctx, "cleaner_capability", http.MethodGet, "/v1/internal/bridge/cleaner-capability", nil, false)
	if err != nil {
		return err
	}
	if json.Unmarshal(body, &response) != nil || !response.Data.Available {
		return errors.New("Task Bridge Cleaner capability unavailable")
	}
	return nil
}

func (c *taskBridgeClient) membership(ctx context.Context, groupID string) (bool, error) {
	var response bridgeMembershipResponse
	path := "/v1/internal/bridge/membership?group_id=" + url.QueryEscape(groupID)
	body, err := c.request(ctx, "membership", http.MethodGet, path, nil, false)
	if err != nil {
		return false, err
	}
	if json.Unmarshal(body, &response) != nil || response.Data.GroupID != groupID {
		return false, errors.New("Task Bridge membership response invalid")
	}
	return response.Data.Joined, nil
}

func (c *taskBridgeClient) accept(ctx context.Context, operationID string) (taskengine.BridgeReservation, error) {
	var response bridgeReservationResponse
	path := "/v1/internal/bridge/operations/" + url.PathEscape(operationID) + "/accept"
	body, err := c.request(ctx, "accept_operation", http.MethodPost, path, []byte{}, true)
	if err != nil {
		return taskengine.BridgeReservation{}, err
	}
	if json.Unmarshal(body, &response) != nil || response.Data.OperationID != operationID {
		return taskengine.BridgeReservation{}, errors.New("Task Bridge accepted operation mismatch")
	}
	return response.Data, nil
}

func (c *taskBridgeClient) permit(ctx context.Context, operationID string, expected taskengine.BridgeReservation) (string, error) {
	var response bridgePermitResponse
	path := "/v1/internal/bridge/operations/" + url.PathEscape(operationID) + "/permit"
	body, err := c.request(ctx, "issue_permit", http.MethodPost, path, []byte{}, true)
	if err != nil {
		return "", err
	}
	if json.Unmarshal(body, &response) != nil || response.Data.OperationID != operationID || response.Data.RequestDigest != expected.Digest ||
		response.Data.AccountID != expected.AccountID || response.Data.OwnerEpoch != strconv.FormatInt(expected.OwnerEpoch, 10) ||
		response.Data.FenceToken != strconv.FormatInt(expected.Fence, 10) || response.Data.ScopeRevision != expected.ScopeRevision ||
		response.Data.State != "issued" || !response.Data.ExpiresAt.After(time.Now()) {
		return "", errors.New("Task Bridge permit does not match the approved operation")
	}
	return response.Data.PermitID, nil
}

func (c *taskBridgeClient) dispatch(ctx context.Context, operationID string) (taskengine.SendOutcome, error) {
	var response taskengine.BridgeOutcomeEnvelope
	path := "/v1/internal/bridge/operations/" + url.PathEscape(operationID) + "/dispatch"
	body, err := c.request(ctx, "dispatch", http.MethodPost, path, []byte{}, true)
	if err != nil {
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &failure) == nil && failure.Error.Code == "EXTERNAL_OUTCOME_UNKNOWN" {
			return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, err
		}
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, err
	}
	if json.Unmarshal(body, &response) != nil {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, errors.New("Task Bridge outcome response invalid")
	}
	return taskengine.SendOutcome{Disposition: response.Data.Disposition, EffectCertainty: response.Data.EffectCertainty,
		EvidenceRef: response.Data.EvidenceRef, RequestRef: response.Data.RequestRef}, nil
}

func (c *taskBridgeClient) status(ctx context.Context, operationID string) (taskengine.BridgeOperationEnvelope, error) {
	var response taskengine.BridgeOperationEnvelope
	path := "/v1/internal/bridge/operations/" + url.PathEscape(operationID) + "/status"
	body, err := c.request(ctx, "operation_status", http.MethodGet, path, nil, false)
	if err != nil {
		return response, err
	}
	if json.Unmarshal(body, &response) != nil || string(response.Data.Operation.OperationId) != operationID {
		return taskengine.BridgeOperationEnvelope{}, errors.New("Task Bridge operation status invalid")
	}
	return response, nil
}

func (c *taskBridgeClient) executeJoin(ctx context.Context, reservation taskengine.Reservation) (taskengine.SendOutcome, error) {
	status, err := c.status(ctx, reservation.OperationID)
	if err != nil {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, err
	}
	if status.Data.Permit != nil && string(status.Data.Permit.State) != "issued" {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, errors.New("operation permit is terminal; dispatch not repeated")
	}
	// Re-read and compare the canonical reservation even when resuming an
	// issued permit. The status envelope does not contain reason or answer.
	accepted, err := c.accept(ctx, reservation.OperationID)
	if err != nil {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, err
	}
	if accepted.OperationID != reservation.OperationID || accepted.ExecutionID != reservation.ExecutionID ||
		accepted.Digest != reservation.Digest || accepted.AccountID != reservation.AccountID ||
		accepted.TargetID != reservation.TargetID || accepted.GroupID != reservation.GroupID ||
		accepted.JoinReason != reservation.JoinReason || accepted.VerificationMessage != reservation.VerificationMessage ||
		accepted.OwnerEpoch != reservation.OwnerEpoch || accepted.Fence != reservation.Fence || accepted.ScopeRevision != reservation.ScopeRevision {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, errors.New("Task Bridge approval payload mismatch")
	}
	if status.Data.Permit == nil {
		if _, err := c.permit(ctx, reservation.OperationID, accepted); err != nil {
			return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, err
		}
	}
	outcome, err := c.dispatch(ctx, reservation.OperationID)
	if err != nil {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, err
	}
	return outcome, nil
}

func (c *taskBridgeClient) sendChatText(ctx context.Context, permitID, sendID string, segment int, text string) (string, bool, error) {
	payload, _ := json.Marshal(chatGatewaySendRequest{SendID: sendID, SegmentIndex: segment, Text: text})
	path := "/v1/internal/chat-send-permits/" + url.PathEscape(permitID) + "/gateway-send"
	body, err := c.request(ctx, "chat_gateway_send", http.MethodPost, path, payload, true)
	if err != nil {
		return "", false, errors.New("EXTERNAL_OUTCOME_UNKNOWN")
	}
	var response chatGatewaySendResponse
	if json.Unmarshal(body, &response) != nil || response.Data.State != "sent" || response.Data.MessageID == nil || *response.Data.MessageID == "" {
		return "", false, errors.New("EXTERNAL_OUTCOME_UNKNOWN")
	}
	return *response.Data.MessageID, true, nil
}

func bridgeRuntimeTokenFromEnv() (string, error) {
	token := strings.TrimSpace(os.Getenv("BBA_TASK_BRIDGE_TOKEN"))
	if token == "" {
		return "", errors.New("BBA_TASK_BRIDGE_TOKEN is required")
	}
	return token, nil
}

func bridgeTokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}
