package artex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newWriteClient(t *testing.T, rt http.RoundTripper, allow bool) *Client {
	t.Helper()
	client, err := New(Config{
		BaseURL:     testBaseURL,
		Token:       testToken,
		Timeout:     2 * time.Second,
		Retry:       RetryPolicy{MaxAttempts: 3, BaseDelay: time.Nanosecond},
		AllowWrites: allow,
		Transport:   rt,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.retry.BaseDelay = 0
	return client
}

const goalJSON = `{"id":"41","text":"观察 90001234 群","vulnclass":"v1","state":"open","origin":"human","ts":"2026-10-20T03:04:05Z"}`

func TestGoalReadsMapARTEXList(t *testing.T) {
	rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"goals":[`+goalJSON+`]}`), nil
	}}
	client := newWriteClient(t, rt, true)
	goals, err := client.Goals(context.Background(), "artex_task")
	if err != nil {
		t.Fatalf("Goals: %v", err)
	}
	if len(goals) != 1 || goals[0].ID != 41 || goals[0].Text != "观察 90001234 群" || goals[0].VulnClass != "v1" || goals[0].State != "open" {
		t.Fatalf("unexpected goal: %+v", goals)
	}
	if got := rt.last().URL.Path; got != "/api/tasks/artex_task/goals" {
		t.Fatalf("path = %q", got)
	}
	if got := rt.last().AuthValue; got != "Bearer "+testToken {
		t.Fatalf("auth header = %q", got)
	}
}

func TestGoalsRejectsMalformedWire(t *testing.T) {
	for _, body := range []string{
		`{"goals":[{"id":"x","text":"a","state":"open","ts":"2026-10-20T03:04:05Z"}]}`,
		`{"goals":[{"id":"1","text":"","state":"open","ts":"2026-10-20T03:04:05Z"}]}`,
		`{"goals":[{"id":"1","text":"a","state":"","ts":"2026-10-20T03:04:05Z"}]}`,
		`{"goals":[{"id":"1","text":"a","state":"open","ts":"yesterday"}]}`,
		`{"goals":[{"id":"1","text":"a","state":"open","ts":"2026-10-20T03:04:05Z"}],"extra":1}`,
	} {
		rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
			return bodyReply(http.StatusOK, body), nil
		}}
		client := newWriteClient(t, rt, true)
		if _, err := client.Goals(context.Background(), "artex_task"); !HasCode(err, CodeInvalidResponse) {
			t.Fatalf("body %s: err = %v, want CodeInvalidResponse", body, err)
		}
	}
}

func TestWritesAreRefusedLocallyWhenDisabled(t *testing.T) {
	rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, goalJSON), nil
	}}
	client := newWriteClient(t, rt, false)
	if client.WritesAllowed() {
		t.Fatal("WritesAllowed true on a disabled client")
	}
	caps := client.Capabilities()
	if caps.Writes || caps.IdempotentWrites {
		t.Fatalf("capabilities %+v", caps)
	}
	ctx := context.Background()
	if _, err := client.AddGoal(ctx, "artex_task", "x", ""); !HasCode(err, CodeWriteNotAllowed) {
		t.Fatalf("AddGoal err = %v", err)
	}
	if _, err := client.UpdateGoal(ctx, "artex_task", 41, "x", ""); !HasCode(err, CodeWriteNotAllowed) {
		t.Fatalf("UpdateGoal err = %v", err)
	}
	if err := client.DeleteGoal(ctx, "artex_task", 41); !HasCode(err, CodeWriteNotAllowed) {
		t.Fatalf("DeleteGoal err = %v", err)
	}
	if rt.count() != 0 {
		t.Fatalf("disabled writes made %d request(s)", rt.count())
	}
}

func TestAddGoalSendsExactlyOneRequest(t *testing.T) {
	var body map[string]string
	rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request body: %v", err)
		}
		return bodyReply(http.StatusOK, goalJSON), nil
	}}
	client := newWriteClient(t, rt, true)
	goal, err := client.AddGoal(context.Background(), "artex_task", "观察 90001234 群", "v1")
	if err != nil {
		t.Fatalf("AddGoal: %v", err)
	}
	if goal.ID != 41 {
		t.Fatalf("goal id = %d", goal.ID)
	}
	if rt.count() != 1 {
		t.Fatalf("requests = %d, want 1", rt.count())
	}
	request := rt.last()
	if request.Method != http.MethodPost || request.URL.Path != "/api/tasks/artex_task/goals" {
		t.Fatalf("request = %s %s", request.Method, request.URL.Path)
	}
	if body["text"] != "观察 90001234 群" || body["vulnclass"] != "v1" {
		t.Fatalf("body = %v", body)
	}
}

func TestAddGoalEmptyTextIsRejectedBeforeSend(t *testing.T) {
	rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, goalJSON), nil
	}}
	client := newWriteClient(t, rt, true)
	if _, err := client.AddGoal(context.Background(), "artex_task", "   ", ""); !HasCode(err, CodeInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
	if rt.count() != 0 {
		t.Fatalf("invalid write made %d request(s)", rt.count())
	}
}

func TestWriteUpstreamErrorIsUnknownAndNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusFound} {
		rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
			return bodyReply(status, `{"error":"fault"}`), nil
		}}
		client := newWriteClient(t, rt, true)
		_, err := client.AddGoal(context.Background(), "artex_task", "x", "")
		if !HasCode(err, CodeOutcomeUnknown) {
			t.Fatalf("status %d: err = %v, want CodeOutcomeUnknown", status, err)
		}
		if rt.count() != 1 {
			t.Fatalf("status %d: requests = %d, want 1 (never replay a write)", status, rt.count())
		}
	}
}

func TestWriteDefiniteRefusalsAreNotUnknown(t *testing.T) {
	cases := []struct {
		status int
		code   Code
	}{
		{http.StatusUnauthorized, CodeUnauthorized},
		{http.StatusForbidden, CodeForbidden},
		{http.StatusNotFound, CodeNotFound},
		{http.StatusConflict, CodeRequestRejected},
		{http.StatusBadRequest, CodeRequestRejected},
	}
	for _, tc := range cases {
		rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
			return bodyReply(tc.status, `{"error":"refused"}`), nil
		}}
		client := newWriteClient(t, rt, true)
		_, err := client.AddGoal(context.Background(), "artex_task", "x", "")
		if !HasCode(err, tc.code) {
			t.Fatalf("status %d: err = %v, want %s", tc.status, err, tc.code)
		}
		if IsOutcomeUnknown(err) {
			t.Fatalf("status %d marked unknown", tc.status)
		}
	}
}

func TestWriteTransportFailureIsUnknown(t *testing.T) {
	rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	}}
	client := newWriteClient(t, rt, true)
	_, err := client.AddGoal(context.Background(), "artex_task", "x", "")
	if !HasCode(err, CodeOutcomeUnknown) || !IsOutcomeUnknown(err) {
		t.Fatalf("err = %v", err)
	}
	if rt.count() != 1 {
		t.Fatalf("requests = %d", rt.count())
	}
}

func TestWriteTimeoutIsUnknown(t *testing.T) {
	rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(200 * time.Millisecond):
			return bodyReply(http.StatusOK, goalJSON), nil
		}
	}}
	client, err := New(Config{BaseURL: testBaseURL, Token: testToken, Timeout: 20 * time.Millisecond, AllowWrites: true, Transport: rt})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AddGoal(context.Background(), "artex_task", "x", "")
	if !HasCode(err, CodeOutcomeUnknown) {
		t.Fatalf("err = %v, want CodeOutcomeUnknown", err)
	}
}

func TestCallerCanceledBeforeSendIsDefinite(t *testing.T) {
	rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, goalJSON), nil
	}}
	client := newWriteClient(t, rt, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.AddGoal(ctx, "artex_task", "x", "")
	if !HasCode(err, CodeCanceled) {
		t.Fatalf("err = %v, want CodeCanceled", err)
	}
	if rt.count() != 0 {
		t.Fatalf("canceled write made %d request(s)", rt.count())
	}
}

func TestWriteMalformedReplyIsUnknown(t *testing.T) {
	for _, body := range []string{`{"id":"x"}`, `not json`, `{"id":"41","text":"a","state":"open","ts":"2026-10-20T03:04:05Z"} trailing`} {
		rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
			return bodyReply(http.StatusOK, body), nil
		}}
		client := newWriteClient(t, rt, true)
		if _, err := client.AddGoal(context.Background(), "artex_task", "x", ""); !HasCode(err, CodeOutcomeUnknown) {
			t.Fatalf("body %q: err = %v", body, err)
		}
		if rt.count() != 1 {
			t.Fatalf("body %q: requests = %d", body, rt.count())
		}
	}
}

func TestUpdateGoalRejectsMismatchedID(t *testing.T) {
	rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"id":"42","text":"new","state":"open","ts":"2026-10-20T03:04:05Z"}`), nil
	}}
	client := newWriteClient(t, rt, true)
	_, err := client.UpdateGoal(context.Background(), "artex_task", 41, "new", "")
	if !HasCode(err, CodeOutcomeUnknown) {
		t.Fatalf("err = %v", err)
	}
	if got := rt.last().Method; got != http.MethodPatch {
		t.Fatalf("method = %s", got)
	}
}

func TestDeleteGoalRequiresOKTrue(t *testing.T) {
	rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"ok":true}`), nil
	}}
	client := newWriteClient(t, rt, true)
	if err := client.DeleteGoal(context.Background(), "artex_task", 41); err != nil {
		t.Fatalf("DeleteGoal: %v", err)
	}
	if got := rt.last().Method; got != http.MethodDelete {
		t.Fatalf("method = %s", got)
	}
	if rt.last().URL.Path != "/api/tasks/artex_task/goals/41" {
		t.Fatalf("path = %s", rt.last().URL.Path)
	}

	rt2 := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, `{"ok":false}`), nil
	}}
	client2 := newWriteClient(t, rt2, true)
	if err := client2.DeleteGoal(context.Background(), "artex_task", 41); !HasCode(err, CodeOutcomeUnknown) {
		t.Fatalf("err = %v", err)
	}
}

func TestGoalTaskAliasesAreRejectedBeforeSend(t *testing.T) {
	rt := &replayTransport{respond: func(attempt int, r *http.Request) (*http.Response, error) {
		return bodyReply(http.StatusOK, goalJSON), nil
	}}
	client := newWriteClient(t, rt, true)
	for _, taskID := range []string{"", " active ", strings.Repeat("t", 129)} {
		if _, err := client.Goals(context.Background(), taskID); !HasCode(err, CodeInvalidRequest) {
			t.Fatalf("task %q: err = %v", taskID, err)
		}
		if _, err := client.AddGoal(context.Background(), taskID, "x", ""); !HasCode(err, CodeInvalidRequest) {
			t.Fatalf("task %q add: err = %v", taskID, err)
		}
	}
	if rt.count() != 0 {
		t.Fatalf("alias task ids made %d request(s)", rt.count())
	}
}
