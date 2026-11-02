package artex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Code classifies every failure this package returns. Callers must branch on the
// code (or HasCode) rather than on error strings.
type Code string

const (
	// CodeNotConfigured means the adapter was never wired: no base URL and/or no
	// token. No I/O was attempted. This is NOT "ARTEX is unreachable", and it must
	// never be reported to an operator as a working ARTEX connection.
	CodeNotConfigured Code = "artex_not_configured"
	// CodeInvalidRequest means the call itself was rejected locally (empty task
	// id, non-positive node id, …) before any request was built.
	CodeInvalidRequest Code = "artex_invalid_request"
	// CodeCanceled means the caller's context ended the call; no retry was made.
	CodeCanceled Code = "artex_canceled"
	// CodeUnauthorized is ARTEX's 401: missing/invalid/expired JWT
	// (ARTEX/server/auth.go:128-135). Never retried.
	CodeUnauthorized Code = "artex_unauthorized"
	// CodeForbidden is ARTEX's 403.
	CodeForbidden Code = "artex_forbidden"
	// CodeNotFound is ARTEX's 404.
	CodeNotFound Code = "artex_not_found"
	// CodeIndeterminate means the adapter cannot decide whether the requested
	// node exists (the numeric-q filter saturated a whole page, see Node).
	CodeIndeterminate Code = "artex_indeterminate"
	// CodeRequestRejected is a non-retryable 4xx other than 401/403/404.
	CodeRequestRejected Code = "artex_request_rejected"
	// CodeUpstreamError is a non-retryable 5xx (notably 500): ARTEX itself failed.
	CodeUpstreamError Code = "artex_upstream_error"
	// CodeUnavailable means the transport failed or a retryable status persisted
	// until the attempt budget ran out.
	CodeUnavailable Code = "artex_unavailable"
	// CodeInvalidResponse means a reply was received but violated the mapped
	// protocol (bad JSON, missing required field, trailing content, 3xx, …).
	// Never retried: the answer was already received.
	CodeInvalidResponse Code = "artex_invalid_response"
	// CodeResponseTooLarge means the reply exceeded maxResponseBytes. Never
	// retried, for the same reason.
	CodeResponseTooLarge Code = "artex_response_too_large"
	// CodeWriteNotAllowed means the client was built with AllowWrites=false, so
	// a mutating call was refused locally without any I/O.
	CodeWriteNotAllowed Code = "artex_write_not_allowed"
	// CodeOutcomeUnknown means a mutating request may or may not have been
	// applied (transport failure, timeout, unreadable reply, 3xx, 5xx). The
	// caller must reconcile by reading and must never resend blindly.
	CodeOutcomeUnknown Code = "artex_outcome_unknown"
)

// ErrNotConfigured is the sentinel every not-wired failure unwraps to.
var ErrNotConfigured = errors.New("artex adapter is not configured (no base URL and/or no token)")

// errResponseTooLarge is the internal signal for a reply over maxResponseBytes.
var errResponseTooLarge = errors.New("response exceeds the adapter size limit")

const (
	defaultTimeout     = 10 * time.Second
	defaultMaxAttempts = 3
	maxAttempts        = 5
	defaultBaseDelay   = 100 * time.Millisecond
	maxBaseDelay       = 2 * time.Second
	maxResponseBytes   = 8 << 20
	maxPageSize        = 200 // ARTEX clamps size to 200: db/exploration.go:877-879

	pathGraph = "/api/exploration/graph"
	pathNodes = "/api/exploration/nodes"
)

// Error is the single error type produced by this package.
type Error struct {
	Code       Code
	HTTPStatus int    // 0 when no response was received
	Attempts   int    // how many requests were made
	Detail     string // ARTEX's {"error": …} message, when the reply carried one
	Err        error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("artex ")
	b.WriteString(string(e.Code))
	if e.HTTPStatus != 0 {
		fmt.Fprintf(&b, " (http %d)", e.HTTPStatus)
	}
	if e.Attempts > 0 {
		fmt.Fprintf(&b, " after %d attempt(s)", e.Attempts)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// HasCode reports whether err (or any error it wraps) carries code.
func HasCode(err error, code Code) bool {
	var artexErr *Error
	return errors.As(err, &artexErr) && artexErr.Code == code
}

// RetryPolicy bounds how often one read is replayed.
type RetryPolicy struct {
	// MaxAttempts counts the first request, so 1 disables retries. The zero value
	// selects the default (3); values are clamped to 1..5.
	MaxAttempts int
	// BaseDelay is the wait before attempt n>1, doubled per attempt and capped.
	// The zero value selects the default (100ms); there is no jitter.
	BaseDelay time.Duration
}

func (p RetryPolicy) normalize() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = defaultMaxAttempts
	}
	if p.MaxAttempts > maxAttempts {
		p.MaxAttempts = maxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = defaultBaseDelay
	}
	if p.BaseDelay > maxBaseDelay {
		p.BaseDelay = maxBaseDelay
	}
	return p
}

func (p RetryPolicy) delay(beforeAttempt int) time.Duration {
	if beforeAttempt < 2 {
		return 0
	}
	d := p.BaseDelay
	for i := 2; i < beforeAttempt; i++ {
		d *= 2
		if d >= maxBaseDelay {
			return maxBaseDelay
		}
	}
	return d
}

// Config wires one ARTEX client. Graph reads are always available; goal writes
// additionally require AllowWrites.
type Config struct {
	// BaseURL is ARTEX's HTTP origin or prefix, e.g. "http://127.0.0.1:8787".
	// Empty means "not wired" (see Wired). ARTEX's own default is :8787
	// (ARTEX/cmd/artex/main.go:55); this adapter has no default host or port.
	BaseURL string
	// Token is the ARTEX JWT sent as "Authorization: Bearer …"
	// (ARTEX/server/auth.go:108-118). Empty means "not wired": ARTEX requires it
	// for every /api/* read (ARTEX/server/auth.go:120-137).
	Token string
	// Timeout bounds one attempt. Zero selects 10s.
	Timeout time.Duration
	// Retry bounds the attempt budget. Zero value selects the defaults.
	Retry RetryPolicy
	// AllowWrites authorizes the mutating goal routes. It is false by default:
	// a read-only deployment (and every unconfigured one) must be impossible to
	// turn into a writer by omission.
	AllowWrites bool
	// Transport is the HTTP round tripper. Nil selects an independent direct transport;
	// tests inject a replaying transport so no network is touched.
	Transport http.RoundTripper
}

// Client is the ARTEX adapter. Reads are GET-only; goal writes are gated by
// AllowWrites and never retried. It is safe for concurrent use.
type Client struct {
	baseURL     string
	token       string
	timeout     time.Duration
	retry       RetryPolicy
	allowWrites bool
	http        *http.Client
}

// New builds a client. A blank BaseURL and/or Token yields a client that is not
// wired: it issues no I/O and every method fails with CodeNotConfigured wrapping
// ErrNotConfigured. That is deliberate — an unconfigured adapter must be
// impossible to mistake for a connected one. An invalid BaseURL (non-http(s),
// userinfo, query, fragment) is the only constructor error.
func New(cfg Config) (*Client, error) {
	transport := cfg.Transport
	if transport == nil {
		transport = &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second}
	}
	c := &Client{
		token:       strings.TrimSpace(cfg.Token),
		timeout:     cfg.Timeout,
		retry:       cfg.Retry.normalize(),
		allowWrites: cfg.AllowWrites,
	}
	if c.timeout <= 0 {
		c.timeout = defaultTimeout
	}
	// Redirects are never followed: an ARTEX API read has no legitimate redirect,
	// and following one would move the bearer token to an unknown host.
	c.http = &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return c, nil
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("artex base URL %q is not a URL: %w", base, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("artex base URL %q must be an absolute http(s) URL", base)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("artex base URL %q must not carry userinfo, query, or fragment", base)
	}
	c.baseURL = strings.TrimRight(base, "/")
	return c, nil
}

// Wired reports whether the adapter has both a base URL and a token, i.e.
// whether a call would attempt I/O at all. False is the "not wired" state and
// must be surfaced to operators as such, never as a working connection.
func (c *Client) Wired() bool {
	return c.baseURL != "" && c.token != ""
}

// NodeQuery selects one page of exploration nodes.
type NodeQuery struct {
	// TaskID is ARTEX's task id (the "task" query parameter). Required: ARTEX
	// would otherwise fall back to its currently active task
	// (ARTEX/server/manager.go:1731-1736), which is not a stable read.
	TaskID string
	// Kinds filters exploration_nodes.kind (Kinds is OR-ed, CSV on the wire).
	Kinds []string
	// States filters exploration_nodes.state (OR-ed, CSV on the wire).
	States []string
	// Query is the case-insensitive payload/origin substring filter; a purely
	// numeric value additionally matches the node id exactly
	// (ARTEX/db/exploration.go:904-906).
	Query string
	// Asc asks for oldest-first ordering (ARTEX "order=asc").
	Asc bool
	// Page is the 1-based offset page. 0 lets ARTEX apply its default (1).
	Page int
	// Size is the page size. 0 lets ARTEX apply its default (20); values above
	// 200 are clamped to ARTEX's ceiling.
	Size int
}

func (q NodeQuery) validate() error {
	if strings.TrimSpace(q.TaskID) == "" {
		return &Error{Code: CodeInvalidRequest, Err: errors.New("task id is required: ARTEX resolves an empty task to its active task")}
	}
	if q.Page < 0 || q.Size < 0 {
		return &Error{Code: CodeInvalidRequest, Err: errors.New("page and size must not be negative")}
	}
	return nil
}

// Graph reads the whole exploration graph of one task
// (GET /api/exploration/graph, ARTEX/server/server.go:2630-2674).
//
// An empty Graph is a valid answer: ARTEX returns HTTP 200 with empty arrays for
// a task it cannot resolve, so emptiness must not be read as "task missing".
func (c *Client) Graph(ctx context.Context, taskID string) (Graph, error) {
	if strings.TrimSpace(taskID) == "" {
		return Graph{}, &Error{Code: CodeInvalidRequest, Err: errors.New("task id is required: ARTEX resolves an empty task to its active task")}
	}
	query := url.Values{}
	query.Set("task", taskID)
	body, err := c.get(ctx, pathGraph, query)
	if err != nil {
		return Graph{}, err
	}
	var wire graphWire
	if err := decodeStrict(body, &wire); err != nil {
		return Graph{}, invalidResponse(err)
	}
	graph, err := wire.toGraph()
	if err != nil {
		return Graph{}, invalidResponse(err)
	}
	return graph, nil
}

// Nodes reads one offset page of exploration nodes
// (GET /api/exploration/nodes, ARTEX/server/server.go:2676-2741).
func (c *Client) Nodes(ctx context.Context, q NodeQuery) (NodePage, error) {
	if err := q.validate(); err != nil {
		return NodePage{}, err
	}
	query := url.Values{}
	query.Set("task", q.TaskID)
	if kinds := csv(q.Kinds); kinds != "" {
		query.Set("kind", kinds)
	}
	if states := csv(q.States); states != "" {
		query.Set("state", states)
	}
	if v := strings.TrimSpace(q.Query); v != "" {
		query.Set("q", v)
	}
	if q.Asc {
		query.Set("order", "asc")
	}
	if q.Page > 0 {
		query.Set("page", strconv.Itoa(q.Page))
	}
	if q.Size > 0 {
		query.Set("size", strconv.Itoa(min(q.Size, maxPageSize)))
	}
	body, err := c.get(ctx, pathNodes, query)
	if err != nil {
		return NodePage{}, err
	}
	var wire nodesPageWire
	if err := decodeStrict(body, &wire); err != nil {
		return NodePage{}, invalidResponse(err)
	}
	page, err := wire.toPage()
	if err != nil {
		return NodePage{}, invalidResponse(err)
	}
	return page, nil
}

// Node reads a single node by id.
//
// ARTEX publishes no "get one node" route, so this uses the documented numeric-q
// id filter (ARTEX/db/exploration.go:904-906) and then verifies the returned id,
// which keeps a payload-substring hit from being mistaken for the node. That
// filter is an OR with a substring match, so its result set can saturate a page;
// when that happens and the id is absent, the answer is CodeIndeterminate rather
// than a false CodeNotFound.
func (c *Client) Node(ctx context.Context, taskID string, nodeID int64) (Node, error) {
	if nodeID <= 0 {
		return Node{}, &Error{Code: CodeInvalidRequest, Err: fmt.Errorf("node id %d is not positive", nodeID)}
	}
	page, err := c.Nodes(ctx, NodeQuery{
		TaskID: taskID,
		Query:  strconv.FormatInt(nodeID, 10),
		Page:   1,
		Size:   maxPageSize,
	})
	if err != nil {
		return Node{}, err
	}
	for _, node := range page.Items {
		if node.ID == nodeID {
			return node, nil
		}
	}
	if len(page.Items) < maxPageSize {
		return Node{}, &Error{Code: CodeNotFound, Err: errNoNode}
	}
	return Node{}, &Error{
		Code: CodeIndeterminate,
		Err:  fmt.Errorf("node id %d not in a saturated %d-row page of numeric-q matches", nodeID, maxPageSize),
	}
}

// get performs the read with bounded retries and returns the raw 200 body.
func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if !c.Wired() {
		return nil, &Error{Code: CodeNotConfigured, Err: ErrNotConfigured}
	}
	endpoint := c.baseURL + path
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := c.wait(ctx, c.retry.delay(attempt)); err != nil {
				return nil, &Error{Code: CodeCanceled, Attempts: attempt - 1, Err: err}
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, c.timeout)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			cancel()
			return nil, &Error{Code: CodeInvalidRequest, Attempts: attempt, Err: err}
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		response, err := c.http.Do(req)
		if err != nil {
			cancel()
			// Caller cancellation ends the loop; a transport failure is replayable
			// because every request this package issues is a bodyless GET.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, &Error{Code: CodeCanceled, Attempts: attempt, Err: ctxErr}
			}
			if attempt < c.retry.MaxAttempts {
				continue
			}
			return nil, &Error{Code: CodeUnavailable, Attempts: attempt, Err: err}
		}
		body, readErr := readBody(response.Body)
		response.Body.Close()
		cancel()
		if readErr != nil {
			// A reply was already received: never replay it. The caller must see
			// the anomaly instead of a silently substituted second answer.
			if errors.Is(readErr, errResponseTooLarge) {
				return nil, &Error{Code: CodeResponseTooLarge, HTTPStatus: response.StatusCode, Attempts: attempt, Err: readErr}
			}
			return nil, &Error{Code: CodeUnavailable, HTTPStatus: response.StatusCode, Attempts: attempt, Detail: "response body could not be read", Err: readErr}
		}
		switch status := response.StatusCode; {
		case status == http.StatusOK:
			return body, nil
		case status == http.StatusUnauthorized:
			return nil, statusError(CodeUnauthorized, status, attempt, body)
		case status == http.StatusForbidden:
			return nil, statusError(CodeForbidden, status, attempt, body)
		case status == http.StatusNotFound:
			return nil, statusError(CodeNotFound, status, attempt, body)
		case retryableStatus(status):
			if attempt < c.retry.MaxAttempts {
				continue
			}
			return nil, statusError(CodeUnavailable, status, attempt, body)
		case status >= 400 && status < 500:
			return nil, statusError(CodeRequestRejected, status, attempt, body)
		case status >= 500:
			return nil, statusError(CodeUpstreamError, status, attempt, body)
		default:
			return nil, &Error{Code: CodeInvalidResponse, HTTPStatus: status, Attempts: attempt, Detail: "unexpected status"}
		}
	}
	// Unreachable: the loop returns on the final attempt.
	return nil, &Error{Code: CodeUnavailable, Attempts: c.retry.MaxAttempts, Err: errors.New("retry budget exhausted")}
}

// retryableStatus lists the statuses worth replaying: the request was not
// accepted (408/429) or the upstream is transiently unavailable (502/503/504).
// 500 is deliberately excluded — that is ARTEX failing on our id.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func statusError(code Code, status, attempts int, body []byte) *Error {
	e := &Error{Code: code, HTTPStatus: status, Attempts: attempts}
	var envelope struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		e.Detail = strings.TrimSpace(envelope.Error)
	}
	return e
}

func invalidResponse(err error) *Error {
	return &Error{Code: CodeInvalidResponse, Err: err}
}

func readBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, errResponseTooLarge
	}
	return data, nil
}

// decodeStrict rejects trailing content after the JSON document so a malformed
// or concatenated reply is reported instead of silently half-consumed.
func decodeStrict(body []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing content after JSON document")
	}
	return nil
}

// decodeClosed decodes a reply whose DTO is fully modelled and rejects any field
// this adapter does not understand: protocol drift must fail loudly instead of
// silently dropping a value the caller would later report as truth.
func decodeClosed(body []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing content after JSON document")
	}
	return nil
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// csv joins non-empty values for the comma-separated kind/state filters
// (ARTEX/server/server.go:2688-2692).
func csv(values []string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, ",")
}
