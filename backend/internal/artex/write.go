package artex

// Write half of the ARTEX adapter: the goal graph.
//
// Everything here is gated behind Config.AllowWrites. With writes disabled the
// methods fail locally with CodeWriteNotAllowed and issue no I/O, so a read-only
// deployment cannot mutate another system by accident.
//
// # Verified protocol (source of truth: local ARTEX tree, commit recorded in
// # local://GraphSync-integration.md)
//
//   - GET    /api/tasks/{id}/goals          -> {"goals":[GoalDTO,…]}
//     ARTEX/server/goals_api.go:18-32; an unknown task is 404 {"error":"task not found"}.
//   - POST   /api/tasks/{id}/goals          body {"text","vulnclass"} -> GoalDTO
//     ARTEX/server/goals_api.go:34-81; empty text is 400 {"error":"目标内容不能为空"},
//     a task being deleted is 409. The goal descends from the task's origin fact
//     and wakes the planner, so this is a real write with an external effect.
//   - PATCH  /api/tasks/{id}/goals/{gid}    body {"text","vulnclass"} -> GoalDTO
//     ARTEX/server/goals_api.go:83-137; unknown goal/kind is 404.
//   - DELETE /api/tasks/{id}/goals/{gid}    -> {"ok":true}
//     ARTEX/server/goals_api.go:139-171.
//
// GoalDTO is ARTEX/server/dto.go:222-248: {"id","text","vulnclass","state",
// "origin","ts"} where id is a decimal *string* and ts is RFC3339.
//
// # Why a write is never replayed automatically
//
// No ARTEX write route accepts an idempotency key, a client token, or a unique
// caller label, and a goal's payload has only "text" and "vulnclass" — both
// user-visible, neither unique. There is therefore no way to prove that a
// replayed POST created one goal instead of two. Every write is a single
// attempt; an outcome the server did not answer unambiguously is
// CodeOutcomeUnknown and the caller must reconcile by reading the goal list,
// never resend.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Goal is one ARTEX goal node (exploration_nodes.kind='goal') unpacked into the
// shape the goal-management routes publish: ARTEX/server/dto.go:222-248.
type Goal struct {
	// ID is ARTEX's decimal node id.
	ID int64
	// Text is payload.text (required by ARTEX on every write).
	Text string
	// VulnClass is payload.vulnclass, empty when the goal carries none.
	VulnClass string
	// State is the exploration node state, e.g. "open".
	State string
	// Origin is the producing label, e.g. "human" for the UI routes.
	Origin string
	// TS is exploration_nodes.created_at, RFC3339 on the wire, UTC here.
	TS time.Time
}

// Capabilities reports what one wired ARTEX deployment can actually do. It is
// deliberately data, not an assumption: the caller must read it instead of
// inferring write/idempotency support from a successful constructor.
type Capabilities struct {
	// Writes is true only when Config.AllowWrites was set, i.e. the operator
	// explicitly authorized external goal writes.
	Writes bool
	// IdempotentWrites is always false: ARTEX's goal routes accept no
	// idempotency key and no unique external label (see the package comment).
	IdempotentWrites bool
	// MaxGraphNodes and MaxGraphEdges are ARTEX's own read caps
	// (ARTEX/server/server.go:2637-2642).
	MaxGraphNodes int
	MaxGraphEdges int
}

// Capabilities implements the graph-sync provider surface.
func (c *Client) Capabilities() Capabilities {
	return Capabilities{Writes: c.allowWrites, IdempotentWrites: false, MaxGraphNodes: 2000, MaxGraphEdges: 5000}
}

// WritesAllowed reports whether external writes are enabled on this client.
func (c *Client) WritesAllowed() bool { return c.allowWrites }

type goalWire struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
	State     string `json:"state"`
	Origin    string `json:"origin"`
	TS        string `json:"ts"`
}

type goalsWire struct {
	Goals []goalWire `json:"goals"`
}

func (w goalWire) toGoal() (Goal, error) {
	id, err := parseWireID("goal id", w.ID)
	if err != nil {
		return Goal{}, err
	}
	text := strings.TrimSpace(w.Text)
	if text == "" {
		return Goal{}, fmt.Errorf("goal %d has no text", id)
	}
	if strings.TrimSpace(w.State) == "" {
		return Goal{}, fmt.Errorf("goal %d has no state", id)
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(w.TS))
	if err != nil {
		return Goal{}, fmt.Errorf("goal %d ts %q is not RFC3339", id, w.TS)
	}
	return Goal{ID: id, Text: text, VulnClass: w.VulnClass, State: w.State, Origin: w.Origin, TS: ts.UTC()}, nil
}

func goalPath(taskID string) (string, error) {
	if err := validateTaskID(taskID); err != nil {
		return "", err
	}
	return "/api/tasks/" + pathEscape(taskID) + "/goals", nil
}

func goalItemPath(taskID string, goalID int64) (string, error) {
	base, err := goalPath(taskID)
	if err != nil {
		return "", err
	}
	if goalID <= 0 {
		return "", &Error{Code: CodeInvalidRequest, Err: fmt.Errorf("goal id %d is not positive", goalID)}
	}
	return base + "/" + fmt.Sprint(goalID), nil
}

// validateTaskID rejects the aliases ARTEX resolves specially. An empty or
// "active" task id would follow whatever task ARTEX currently has active instead
// of the bound root (ARTEX/server/manager.go:1731-1736), which is not a stable
// read and would attribute another task's graph to this one.
func validateTaskID(taskID string) error {
	trimmed := strings.TrimSpace(taskID)
	if trimmed == "" {
		return &Error{Code: CodeInvalidRequest, Err: errors.New("task id is required: ARTEX resolves an empty task to its active task")}
	}
	if trimmed == "active" {
		return &Error{Code: CodeInvalidRequest, Err: errors.New("task id must not be the ARTEX 'active' alias")}
	}
	if len(trimmed) > 128 {
		return &Error{Code: CodeInvalidRequest, Err: errors.New("task id exceeds 128 characters")}
	}
	return nil
}

// pathEscape percent-encodes a task id for a URL path segment (ARTEX task ids
// are opaque to this adapter).
func pathEscape(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "/", "%2F")
}

// Goals reads the goal graph of one ARTEX task
// (GET /api/tasks/{id}/goals, ARTEX/server/goals_api.go:18-32).
func (c *Client) Goals(ctx context.Context, taskID string) ([]Goal, error) {
	path, err := goalPath(taskID)
	if err != nil {
		return nil, err
	}
	body, err := c.get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	var wire goalsWire
	if err := decodeClosed(body, &wire); err != nil {
		return nil, invalidResponse(err)
	}
	goals := make([]Goal, 0, len(wire.Goals))
	for _, w := range wire.Goals {
		goal, err := w.toGoal()
		if err != nil {
			return nil, invalidResponse(err)
		}
		goals = append(goals, goal)
	}
	return goals, nil
}

// AddGoal creates one goal on the ARTEX task and returns the goal ARTEX now
// publishes (POST /api/tasks/{id}/goals, ARTEX/server/goals_api.go:34-81).
//
// The request is issued once. An ambiguous outcome is CodeOutcomeUnknown; the
// caller must read Goals back to decide what happened.
func (c *Client) AddGoal(ctx context.Context, taskID, text, vulnClass string) (Goal, error) {
	path, err := goalPath(taskID)
	if err != nil {
		return Goal{}, err
	}
	body, err := goalBody(text, vulnClass)
	if err != nil {
		return Goal{}, err
	}
	reply, err := c.write(ctx, http.MethodPost, path, body)
	if err != nil {
		return Goal{}, err
	}
	var wire goalWire
	if err := decodeClosed(reply, &wire); err != nil {
		return Goal{}, outcomeUnknown(err)
	}
	goal, err := wire.toGoal()
	if err != nil {
		return Goal{}, outcomeUnknown(err)
	}
	return goal, nil
}

// UpdateGoal edits one goal's text/vulnclass
// (PATCH /api/tasks/{id}/goals/{gid}, ARTEX/server/goals_api.go:83-137).
func (c *Client) UpdateGoal(ctx context.Context, taskID string, goalID int64, text, vulnClass string) (Goal, error) {
	path, err := goalItemPath(taskID, goalID)
	if err != nil {
		return Goal{}, err
	}
	body, err := goalBody(text, vulnClass)
	if err != nil {
		return Goal{}, err
	}
	reply, err := c.write(ctx, http.MethodPatch, path, body)
	if err != nil {
		return Goal{}, err
	}
	var wire goalWire
	if err := decodeClosed(reply, &wire); err != nil {
		return Goal{}, outcomeUnknown(err)
	}
	goal, err := wire.toGoal()
	if err != nil {
		return Goal{}, outcomeUnknown(err)
	}
	if goal.ID != goalID {
		return Goal{}, outcomeUnknown(fmt.Errorf("PATCH goal %d answered with goal %d", goalID, goal.ID))
	}
	return goal, nil
}

// DeleteGoal removes one goal from the ARTEX task
// (DELETE /api/tasks/{id}/goals/{gid}, ARTEX/server/goals_api.go:139-171).
func (c *Client) DeleteGoal(ctx context.Context, taskID string, goalID int64) error {
	path, err := goalItemPath(taskID, goalID)
	if err != nil {
		return err
	}
	reply, err := c.write(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	var ack struct {
		OK bool `json:"ok"`
	}
	if err := decodeStrict(reply, &ack); err != nil {
		return outcomeUnknown(err)
	}
	if !ack.OK {
		return outcomeUnknown(errors.New("delete goal answered without ok=true"))
	}
	return nil
}

func goalBody(text, vulnClass string) ([]byte, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, &Error{Code: CodeInvalidRequest, Err: errors.New("goal text is required (ARTEX rejects an empty 目标内容)")}
	}
	payload := map[string]string{"text": trimmed}
	if class := strings.TrimSpace(vulnClass); class != "" {
		payload["vulnclass"] = class
	}
	return json.Marshal(payload)
}

// write performs exactly one mutating request. There is no retry loop: see the
// package comment on replay safety.
func (c *Client) write(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if !c.Wired() {
		return nil, &Error{Code: CodeNotConfigured, Err: ErrNotConfigured}
	}
	if !c.allowWrites {
		return nil, &Error{Code: CodeWriteNotAllowed, Err: ErrWriteNotAllowed}
	}
	if err := ctx.Err(); err != nil {
		// The caller was already done before any byte was written, so this is a
		// definite no-effect, not an unknown outcome.
		return nil, &Error{Code: CodeCanceled, Attempts: 0, Err: err}
	}
	attemptCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(attemptCtx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, &Error{Code: CodeInvalidRequest, Attempts: 1, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		// The request may have reached ARTEX before the connection failed or the
		// attempt deadline fired, so the effect is undecided either way.
		return nil, &Error{Code: CodeOutcomeUnknown, Attempts: 1, Detail: method + " " + path, Err: err}
	}
	reply, readErr := readBody(response.Body)
	response.Body.Close()
	if readErr != nil {
		// A reply arrived but could not be consumed, so the local caller cannot
		// know whether the mutation was applied. Never replay, never claim success.
		return nil, &Error{Code: CodeOutcomeUnknown, HTTPStatus: response.StatusCode, Attempts: 1, Detail: "response body could not be read", Err: readErr}
	}
	switch status := response.StatusCode; {
	case status == http.StatusOK:
		return reply, nil
	case status == http.StatusUnauthorized:
		return nil, statusError(CodeUnauthorized, status, 1, reply)
	case status == http.StatusForbidden:
		return nil, statusError(CodeForbidden, status, 1, reply)
	case status == http.StatusNotFound:
		return nil, statusError(CodeNotFound, status, 1, reply)
	case status == http.StatusConflict:
		// ARTEX answers 409 only for "task is being deleted" before the write
		// (goals_api.go:42-45, 91-94, 147-150), i.e. a definite no-effect.
		return nil, statusError(CodeRequestRejected, status, 1, reply)
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests:
		// ARTEX publishes no authoritative "not applied" answer for these; the
		// caller must reconcile by reading, exactly like a 5xx or 3xx.
		return nil, statusError(CodeOutcomeUnknown, status, 1, reply)
	case status >= 400 && status < 500:
		return nil, statusError(CodeRequestRejected, status, 1, reply)
	default:
		// 3xx (not followed) and every 5xx/408/429: the mutation may have been
		// applied. The caller must reconcile by reading; it must not resend.
		return nil, statusError(CodeOutcomeUnknown, status, 1, reply)
	}
}

// ErrWriteNotAllowed is the sentinel every disabled-write failure unwraps to.
var ErrWriteNotAllowed = errors.New("artex writes are disabled (Config.AllowWrites is false)")

func outcomeUnknown(err error) *Error {
	return &Error{Code: CodeOutcomeUnknown, Err: err}
}

// IsOutcomeUnknown reports whether err leaves the external effect undecided.
// Callers must mark such an operation for reconciliation and never replay it.
func IsOutcomeUnknown(err error) bool {
	var artexErr *Error
	if errors.As(err, &artexErr) {
		return artexErr.Code == CodeOutcomeUnknown
	}
	return false
}

// IsRejected reports whether ARTEX answered with a definite refusal, i.e. the
// write had no effect and replaying the same request is safe in principle (the
// caller still decides whether the request is worth repeating).
func IsRejected(err error) bool {
	var artexErr *Error
	if errors.As(err, &artexErr) {
		switch artexErr.Code {
		case CodeUnauthorized, CodeForbidden, CodeNotFound, CodeRequestRejected, CodeInvalidRequest, CodeWriteNotAllowed, CodeNotConfigured:
			return true
		}
	}
	return false
}
