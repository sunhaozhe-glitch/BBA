package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"bba.local/taskengine/internal/taskengine"
)

func newLoopbackHTTPClient() *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			ip := net.ParseIP(host)
			if err != nil || ip == nil || !ip.IsLoopback() {
				return nil, errors.New("Task Bridge endpoint is not a literal loopback address")
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(host, port))
		},
		DisableKeepAlives: true, ForceAttemptHTTP2: false,
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

}

type callBudget struct {
	mu    sync.Mutex
	limit int
	used  int
}

func (b *callBudget) take(label string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.limit {
		return fmt.Errorf("%s budget exhausted", label)
	}
	b.used++
	return nil
}

func (b *callBudget) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

type liveClient struct {
	bridge *taskBridgeClient
	reads  *callBudget
	writes *callBudget
}

func newLiveClient(cfg executionConfig, ev *evidence, _ bool) (*liveClient, error) {
	token, err := bridgeRuntimeTokenFromEnv()
	if err != nil {
		return nil, err
	}
	readsLimit := cfg.MaxQQReadCalls
	if readsLimit < 1 {
		readsLimit = 10
	}
	bridge, err := newTaskBridgeClient(cfg.TaskBridgeURL, token, readsLimit, ev)
	if err != nil {
		return nil, err
	}
	return &liveClient{bridge: bridge, reads: bridge.reads, writes: bridge.writes}, nil
}

type identityFacts struct {
	SelfID     string
	Nickname   string
	Online     bool
	Good       bool
	GroupIDs   []string
	ObservedAt time.Time
}

func (l *liveClient) observeIdentity(ctx context.Context, withGroups bool) (identityFacts, error) {
	return l.bridge.identity(ctx, withGroups)
}

func (l *liveClient) cleanerAvailable(ctx context.Context) error {
	return l.bridge.cleanerAvailable(ctx)
}

func (l *liveClient) observeMembership(ctx context.Context, groupID string) (bool, error) {
	return l.bridge.membership(ctx, groupID)
}

func decodeOneBotData(body []byte) (json.RawMessage, error) {
	var envelope struct {
		Status  string          `json:"status"`
		RetCode json.Number     `json:"retcode"`
		Data    json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, errors.New("invalid OneBot envelope")
	}
	if envelope.Status != "ok" || envelope.RetCode.String() != "0" {
		return nil, errors.New("OneBot rejected request")
	}
	if len(envelope.Data) == 0 {
		envelope.Data = json.RawMessage("null")
	}
	return envelope.Data, nil
}

func containsGroup(groups []string, target string) bool {
	for _, group := range groups {
		if group == target {
			return true
		}
	}
	return false
}

func (l *liveClient) executeJoin(ctx context.Context, reservation taskengine.Reservation) (taskengine.SendOutcome, error) {
	return l.bridge.executeJoin(ctx, reservation)
}

func (l *liveClient) sendChatText(ctx context.Context, permitID, sendID string, segment int, text string) (string, bool, error) {
	return l.bridge.sendChatText(ctx, permitID, sendID, segment, text)
}

func isUnknownOutcome(err error) bool {
	return err != nil && strings.Contains(err.Error(), "EXTERNAL_OUTCOME_UNKNOWN")
}
