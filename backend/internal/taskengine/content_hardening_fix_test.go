package taskengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Observer uses the same isolated database but not the one-connection subject
// pool. Every release is driven by pg_stat_activity, never a guessed sleep.
func hardOne(t *testing.T, s *Store) *Store {
	t.Helper()
	one, e := Open(context.Background(), s.Pool.Config().ConnString()+"&pool_max_conns=1")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(one.Close)
	return one
}
func hardRuleRequest(ctx context.Context, h http.Handler, op string) *httptest.ResponseRecorder {
	if op == "list" {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/operator/content-rules", nil).WithContext(ctx))
		return w
	}
	if op == "preview" {
		return hardServe(ctx, h, "/v1/operator/content-rules/preview", map[string]any{"text": "fixture"})
	}
	return hardServe(ctx, h, "/v1/operator/content-rules", map[string]any{"name": "fixture", "kind": "keyword", "expression": "fixture", "case_sensitive": true, "enabled": true, "expected_version": 0})
}
func hardRuleWaiting(t *testing.T, s *Store) {
	t.Helper()
	hardWait(t, s, `SELECT u.pid FROM pg_stat_activity u WHERE u.datname=current_database() AND u.wait_event='advisory' AND EXISTS(SELECT 1 FROM pg_stat_activity g WHERE g.application_name='bba_content_guard' AND g.datname=current_database() AND g.pid=ANY(pg_blocking_pids(u.pid)))`)
}
func hardWithin(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case e := <-done:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("normal request did not finish within 2s (not the 20s send deadline)")
		return nil
	}
}
func TestB3HardFixPoolOrder(t *testing.T) {
	for _, window := range []string{"pre_review", "post_writer"} {
		for _, op := range []string{"preview", "list", "update"} {
			t.Run(window+"/"+op, func(t *testing.T) {
				observer, a, path, body := hardChat(t)
				s := hardOne(t, observer)
				ctx, c := context.WithTimeout(context.Background(), 4*time.Second)
				defer c()
				h := s.ContentHandler(func(*http.Request) (Actor, error) { return contentOperator(), nil })
				ruleDone := make(chan error, 1)
				sendDone := make(chan error, 1)
				startRule := func() {
					go func() {
						w := hardRuleRequest(ctx, h, op)
						if w.Code != 200 {
							ruleDone <- problem("RULE_HTTP_" + w.Body.String())
							return
						}
						var out map[string]any
						if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
							ruleDone <- e
							return
						}
						ruleDone <- nil
					}()
					hardRuleWaiting(t, observer)
				}
				if window == "pre_review" {
					// Exact production gate + review operations, with an HTTP rule waiter
					// observed before attempting to borrow the single review connection.
					guard, e := s.acquireContentGuard(ctx, a.WorkspaceID)
					if e != nil {
						t.Fatal(e)
					}
					defer guard.close()
					startRule()
					go func() {
						decision, e := s.contentInspect(ctx, a.WorkspaceID, "acc_chat", "group", "424242", "ordered_review", "", "fixture", "digest", false)
						if e == nil && decision != "allowed" {
							e = problem("WRONG_REVIEW")
						}
						sendDone <- e
					}()
					e = hardWithin(t, sendDone)
					guard.close()
					if e != nil {
						t.Fatal(e)
					}
				} else {
					entered := make(chan struct{})
					release := make(chan struct{})
					defer func() {
						select {
						case <-release:
						default:
							close(release)
						}
					}()
					writer := &hardChatWriter{entered: entered, release: release}
					gateway := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return a, nil }, writer)
					go func() {
						w := hardServe(ctx, gateway, path, body)
						if w.Code != 200 {
							sendDone <- problem("SEND_HTTP_" + w.Body.String())
						} else {
							sendDone <- nil
						}
					}()
					select {
					case <-entered:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					startRule()
					close(release)
					if e := hardWithin(t, sendDone); e != nil {
						t.Fatal(e)
					}
					if writer.calls.Load() != 1 {
						t.Fatal("wrong writer count")
					}
				}
				if e := hardWithin(t, ruleDone); e != nil {
					t.Fatal(e)
				}
				want := 0
				if op == "update" {
					want = 1
				}
				if got := scalarInt(t, observer, `SELECT COALESCE((SELECT version FROM core.content_revision WHERE workspace_id='ws_fixture'),0)`); got != want {
					t.Fatalf("version=%d want=%d", got, want)
				}
				if got := scalarInt(t, observer, `SELECT count(*) FROM core.content_review WHERE rules_version=0 AND decision='allowed'`); got != 1 {
					t.Fatalf("send-before-update revision evidence=%d", got)
				}
				hardCleanup(t, observer)
				t.Log("DB advisory wait barrier; MaxConns=1; review/outcome and HTTP both complete <2s; old revision reviewed before update")
			})
		}
	}
}

type hardStubborn struct {
	calls    atomic.Int32
	entered  chan struct{}
	kill     bool
	noEffect bool
	canceled atomic.Bool
}

func (w *hardStubborn) finish(ctx context.Context) {
	w.calls.Add(1)
	close(w.entered)
	if w.kill {
		<-ctx.Done()
		w.canceled.Store(true)
	}
	// Intentionally ignore cancellation and report a definite result.
}
func (w *hardStubborn) RequestJoin(ctx context.Context, _ Reservation) (SendOutcome, error) {
	w.finish(ctx)
	if w.noEffect {
		return SendOutcome{Disposition: "failed_no_effect", EffectCertainty: "no_effect", EvidenceRef: "fixture_no_effect"}, nil
	}
	return SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fixture_submitted"}, nil
}
func (w *hardStubborn) SendChatText(ctx context.Context, _ RuntimeActor, _, _, _ string) (string, error) {
	w.finish(ctx)
	return "stubborn_success_id", nil
}
func TestB3HardFixStubbornWriter(t *testing.T) {
	for _, route := range []string{"bridge", "chat"} {
		for _, mode := range []string{"health_success", "health_no_effect", "normal_stop"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				var s *Store
				var a RuntimeActor
				var res Reservation
				var path string
				var body map[string]any
				if route == "bridge" {
					s, a, res = hardJoin(t, "fixture reason", "fixture answer")
				} else {
					s, a, path, body = hardChat(t)
				}
				ctx, c := context.WithTimeout(context.Background(), 4*time.Second)
				defer c()
				w := &hardStubborn{entered: make(chan struct{}), kill: mode != "normal_stop", noEffect: mode == "health_no_effect"}
				done := make(chan error, 1)
				h := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return a, nil }, w)
				go func() {
					if route == "bridge" {
						_, e := s.Dispatch(ctx, a, res.OperationID, identityOK, w)
						done <- e
					} else {
						r := hardServe(ctx, h, path, body)
						want := 409
						if mode == "normal_stop" {
							want = 200
						}
						if r.Code != want {
							done <- problem("WRONG_HTTP_" + r.Body.String())
						} else if mode != "normal_stop" && strings.Contains(r.Body.String(), "stubborn_success_id") {
							done <- problem("SENT_RECEIPT_LEAK")
						} else {
							done <- nil
						}
					}
				}()
				select {
				case <-w.entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if w.kill {
					pid := hardKill(t, s)
					t.Logf("terminated exact fixture guard pid=%d after writer entry", pid)
				}
				e := hardWithin(t, done)
				if route == "bridge" {
					if mode == "normal_stop" {
						if e != nil || gatewayState(t, s, res.OperationID) != "OBSERVED" {
							t.Fatalf("normal stop error=%v state=%s", e, gatewayState(t, s, res.OperationID))
						}
					} else {
						if Code(e) != "EXTERNAL_OUTCOME_UNKNOWN" || gatewayState(t, s, res.OperationID) != "UNKNOWN" {
							t.Errorf("health result error=%v state=%s", e, gatewayState(t, s, res.OperationID))
						}
						if n := scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock WHERE held`); n != 1 {
							t.Errorf("uncertain account occupation released: %d", n)
						}
						if n := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE state='reserved'`); n != 1 {
							t.Errorf("uncertain goal occupation released: %d", n)
						}
						if n := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE effect_certainty='no_effect'`); n != 0 {
							t.Errorf("false no_effect=%d", n)
						}
						_, _ = s.Dispatch(ctx, a, res.OperationID, identityOK, w)
					}
					if n := activeLocalDispatches.count(res.WorkspaceID, res.TaskID); n != 0 {
						t.Errorf("flight=%d", n)
					}
				} else {
					if e != nil {
						t.Error(e)
					}
					if mode != "normal_stop" {
						r := hardServe(ctx, h, path, body)
						if r.Code != 409 {
							t.Errorf("replay=%d %s", r.Code, r.Body)
						}
						if n := scalarInt(t, s, `SELECT count(*) FROM core.chat_send_outcome WHERE state='sent'`); n != 0 {
							t.Errorf("false sent=%d", n)
						}
					}
				}
				if w.calls.Load() != 1 {
					t.Errorf("writer calls=%d", w.calls.Load())
				}
				if mode != "normal_stop" && !w.canceled.Load() {
					t.Error("watch did not cancel writer")
				}
				hardCleanup(t, s)
				t.Logf("writer_calls=%d canceled=%v mode=%s; watch joined before readback", w.calls.Load(), w.canceled.Load(), mode)
			})
		}
	}
}
