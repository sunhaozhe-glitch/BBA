package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bba.local/taskengine/internal/taskengine"
)

type napcatGateway struct {
	config    gatewayRuntimeConfig
	oneBotURL string
	webUIURL  string
	client    *http.Client
}

func (g napcatGateway) SendChatText(ctx context.Context, actor taskengine.RuntimeActor, kind, conversationID, text string) (string, error) {
	if len(actor.AccountIDs) != 1 || actor.AccountIDs[0] != g.config.AccountID || actor.RuntimeID != g.config.RuntimeID || actor.OwnerEpoch != g.config.OwnerEpoch || actor.WorkspaceID != g.config.WorkspaceID {
		return "", errors.New("runtime mapping mismatch")
	}
	var action string
	var payload map[string]any
	switch kind {
	case "group":
		action, payload = "send_group_msg", map[string]any{"group_id": json.Number(conversationID), "message": text, "auto_escape": true}
	case "private":
		action, payload = "send_private_msg", map[string]any{"user_id": json.Number(conversationID), "message": text, "auto_escape": true}
	default:
		return "", errors.New("unsupported chat conversation kind")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", errors.New("chat payload invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.oneBotURL+"/"+action, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("chat request invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+g.config.OneBotToken)
	response, err := g.client.Do(request)
	if err != nil {
		return "", errors.New("chat send outcome unknown")
	}
	defer response.Body.Close()
	result, err := gatewayReadBody(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		return "", errors.New("chat send outcome unknown")
	}
	var envelope struct {
		Status  string      `json:"status"`
		RetCode json.Number `json:"retcode"`
		Data    struct {
			MessageID json.RawMessage `json:"message_id"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(result)))
	decoder.UseNumber()
	if decoder.Decode(&envelope) != nil || envelope.Status != "ok" || envelope.RetCode.String() != "0" {
		return "", errors.New("chat send outcome unknown")
	}
	var id string
	if json.Unmarshal(envelope.Data.MessageID, &id) != nil {
		id = strings.TrimSpace(string(envelope.Data.MessageID))
	}
	if id == "" || id == "null" || len(id) > 128 {
		return "", errors.New("chat send response has no message id")
	}
	return id, nil
}

func (g napcatGateway) observeIdentity(ctx context.Context, withGroups bool) (taskengine.BridgeIdentity, error) {
	var out taskengine.BridgeIdentity
	login, err := g.oneBot(ctx, "get_login_info", map[string]any{})
	if err != nil {
		return out, err
	}
	var identity struct {
		UserID json.Number `json:"user_id"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(login)))
	decoder.UseNumber()
	if decoder.Decode(&identity) != nil || !validQQ(identity.UserID.String()) {
		return out, errors.New("authenticated identity readback invalid")
	}
	statusRaw, err := g.oneBot(ctx, "get_status", map[string]any{})
	if err != nil {
		return out, err
	}
	var status struct {
		Online *bool `json:"online"`
		Good   *bool `json:"good"`
	}
	if json.Unmarshal(statusRaw, &status) != nil || status.Online == nil || status.Good == nil {
		return out, errors.New("runtime status readback invalid")
	}
	out.SelfID, out.Online, out.Good = identity.UserID.String(), *status.Online, *status.Good
	if withGroups {
		raw, err := g.oneBot(ctx, "get_group_list", map[string]any{})
		if err != nil {
			return taskengine.BridgeIdentity{}, err
		}
		out.GroupIDs, err = gatewayGroupIDs(raw)
		if err != nil {
			return taskengine.BridgeIdentity{}, err
		}
	}
	return out, nil
}

func (g napcatGateway) cleanerAvailable(ctx context.Context) error {
	credential, err := g.webUICredential(ctx)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.webUIURL+"/api/Plugin/List", nil)
	if err != nil {
		return errors.New("Cleaner capability request invalid")
	}
	request.Header.Set("Authorization", credential)
	response, err := g.client.Do(request)
	if err != nil {
		return errors.New("Cleaner capability read failed")
	}
	defer response.Body.Close()
	body, err := gatewayReadBody(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		return errors.New("Cleaner capability read failed")
	}
	var result struct {
		Code *int `json:"code"`
		Data struct {
			ManagerMissing *bool `json:"pluginManagerNotFound"`
			Plugins        []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"plugins"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &result) != nil || result.Code == nil || *result.Code != 0 || result.Data.ManagerMissing == nil || *result.Data.ManagerMissing {
		return errors.New("Cleaner capability evidence unavailable")
	}
	for _, plugin := range result.Data.Plugins {
		if plugin.ID == "napcat-plugin-cleaner" && plugin.Status == "active" {
			return nil
		}
	}
	return errors.New("Cleaner capability evidence missing")
}

func (g napcatGateway) webUICredential(ctx context.Context) (string, error) {
	sum := sha256.Sum256([]byte(g.config.WebUIToken + ".napcat"))
	payload, _ := json.Marshal(map[string]string{"hash": hex.EncodeToString(sum[:])})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.webUIURL+"/api/auth/login", bytesReader(payload))
	if err != nil {
		return "", errors.New("WebUI authentication request invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := g.client.Do(request)
	if err != nil {
		return "", errors.New("WebUI authentication failed")
	}
	defer response.Body.Close()
	body, err := gatewayReadBody(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		return "", errors.New("WebUI authentication failed")
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Credential string `json:"Credential"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code != 0 || envelope.Data.Credential == "" {
		return "", errors.New("WebUI authentication failed")
	}
	return envelope.Data.Credential, nil
}

func (g napcatGateway) observeMembership(ctx context.Context, groupID string) (bool, error) {
	raw, err := g.oneBot(ctx, "get_group_list", map[string]any{})
	if err != nil {
		return false, err
	}
	groups, err := gatewayGroupIDs(raw)
	if err != nil {
		return false, err
	}
	return containsString(groups, groupID), nil
}

func (g napcatGateway) requestJoin(ctx context.Context, reservation taskengine.Reservation) (taskengine.SendOutcome, error) {
	// Reject a misrouted reservation before even authenticating to WebUI.
	if reservation.WorkspaceID != g.config.WorkspaceID || reservation.RuntimeID != g.config.RuntimeID ||
		reservation.AccountID != g.config.AccountID || reservation.OwnerEpoch != g.config.OwnerEpoch || !validQQ(reservation.GroupID) {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "no_effect"}, errors.New("Cleaner reservation runtime or payload mismatch")
	}
	payload, err := json.Marshal(map[string]any{"group_id": json.Number(reservation.GroupID), "confirm": "1", "reason": reservation.JoinReason, "answer": reservation.VerificationMessage})
	if err != nil {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "no_effect"}, errors.New("Cleaner payload invalid")
	}
	credential, err := g.webUICredential(ctx)
	if err != nil {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.webUIURL+"/api/Plugin/ext/napcat-plugin-cleaner/join-group", bytesReader(payload))
	if err != nil {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, errors.New("Cleaner request invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", credential)
	response, err := g.client.Do(request)
	if err != nil {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, errors.New("Cleaner outcome unknown")
	}
	defer response.Body.Close()
	body, err := gatewayReadBody(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, errors.New("Cleaner outcome unknown")
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(body, &envelope) != nil || !envelope.OK || strings.TrimSpace(string(envelope.Result)) != "1" {
		return taskengine.SendOutcome{Disposition: "unknown", EffectCertainty: "unknown"}, errors.New("Cleaner result classification unknown")
	}
	return taskengine.SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "task_bridge_cleaner_join_response"}, nil
}

func (g napcatGateway) oneBot(ctx context.Context, action string, payload any) (json.RawMessage, error) {
	switch action {
	case "get_login_info", "get_status", "get_group_list":
	default:
		return nil, errors.New("OneBot read action is not allowed")
	}
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.oneBotURL+"/"+action, bytesReader(body))
	if err != nil {
		return nil, errors.New("OneBot request invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+g.config.OneBotToken)
	response, err := g.client.Do(request)
	if err != nil {
		return nil, errors.New("OneBot read failed")
	}
	defer response.Body.Close()
	result, err := gatewayReadBody(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, errors.New("OneBot read failed")
	}
	var envelope struct {
		Status  string          `json:"status"`
		RetCode json.Number     `json:"retcode"`
		Data    json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(result)))
	decoder.UseNumber()
	if decoder.Decode(&envelope) != nil || envelope.Status != "ok" || envelope.RetCode.String() != "0" || len(envelope.Data) == 0 {
		return nil, errors.New("OneBot response invalid")
	}
	return envelope.Data, nil
}

func validateGatewayLoopbackURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid loopback URL")
	}
	ip := net.ParseIP(parsed.Hostname())
	port, err := strconv.Atoi(parsed.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return "", errors.New("invalid loopback URL")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}
func newGatewayHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, ForceAttemptHTTP2: false, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("Gateway endpoint is not literal loopback")
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(host, port))
	}}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func gatewayReadBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("Gateway response exceeds bounded size")
	}
	return body, nil
}
func gatewayGroupIDs(raw json.RawMessage) ([]string, error) {
	var groups []struct {
		ID      json.RawMessage `json:"group_id"`
		Name    *string         `json:"group_name"`
		Members *uint64         `json:"member_count"`
		Maximum *uint64         `json:"max_member_count"`
	}
	if json.Unmarshal(raw, &groups) != nil || groups == nil {
		return nil, errors.New("group list incomplete")
	}
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		id := strings.Trim(string(group.ID), `"`)
		if !validQQ(id) || group.Name == nil || group.Members == nil || group.Maximum == nil {
			return nil, errors.New("group record incomplete")
		}
		ids = append(ids, id)
	}
	return ids, nil
}
func validQQ(value string) bool {
	if value == "" || len(value) > 20 || value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func bytesReader(body []byte) io.Reader { return bytes.NewReader(body) }
