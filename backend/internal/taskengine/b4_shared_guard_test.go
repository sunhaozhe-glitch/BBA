package taskengine

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Both real gateways share one workspace and independent authorized accounts.
func b4MixedFixture(t *testing.T, kind string) (*Store, RuntimeActor, Reservation, RuntimeActor, string, map[string]any) {
	t.Helper()
	s, chat, d := chatFixture(t, 20)
	seedAccount(t, s, "acc_shared_join", "rt_shared_join", 10)
	task := createReadyTask(t, s, []string{"acc_shared_join"}, []string{"991111"}, 1, 3)
	r := reserveFixture(t, s, task.Id, "acc_shared_join")
	join := fixtureRuntime("acc_shared_join", "rt_shared_join")
	acceptFixture(t, s, join, r)
	if _, e := s.IssuePermit(context.Background(), join, r.OperationID, 30*time.Second); e != nil {
		t.Fatal(e)
	}
	chatSeedTrigger(t, s, "trig_mixed", "rt_chat", "acc_chat", kind, "424242", "20001", nil, d, 1)
	p, e := s.IssueChatSendPermit(context.Background(), chat, "mixed", chatPermitRequest("send_mixed", "acc_chat", "trig_mixed", kind, "424242", 0, 1, d))
	if e != nil {
		t.Fatal(e)
	}
	return s, join, r, chat, chatPermitPath + "/" + string(p.PermitId) + "/gateway-send", map[string]any{"send_id": p.SendId, "segment_index": 0, "text": "fixture text"}
}
func b4Signal(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
func b4Close(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}
func TestB4SharedMixedGate(t *testing.T) {
	for _, kind := range []string{"group", "private"} {
		for _, mode := range []string{"update_waits_both", "one_guard_lost", "chat_guard_lost"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				s, join, r, chat, path, body := b4MixedFixture(t, kind)
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				ja := &hardJoinBlock{entered: make(chan struct{}), release: make(chan struct{})}
				cw := &hardChatWriter{entered: make(chan struct{}), release: make(chan struct{})}
				defer b4Close(ja.release)
				defer b4Close(cw.release)
				jd := make(chan error, 1)
				cd := make(chan int, 1)
				go func() { _, e := s.Dispatch(ctx, join, r.OperationID, identityOK, ja); jd <- e }()
				b4Signal(t, ctx, ja.entered)
				pid := hardWait(t, s, `SELECT a.pid FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.datname=current_database() AND a.application_name='bba_content_guard' AND l.locktype='advisory' AND l.granted AND l.mode='ShareLock'`)
				h := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return chat, nil }, cw)
				go func() { cd <- hardServe(ctx, h, path, body).Code }()
				b4Signal(t, ctx, cw.entered)
				if n := scalarInt(t, s, `SELECT count(*) FROM pg_locks WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND locktype='advisory' AND granted AND mode='ShareLock'`); n != 2 {
					t.Fatalf("shared holders=%d", n)
				}
				if mode == "chat_guard_lost" {
					chatPID := hardWait(t, s, `SELECT a.pid FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.datname=current_database() AND a.application_name='bba_content_guard' AND a.pid<>$1 AND l.locktype='advisory' AND l.granted AND l.mode='ShareLock'`, pid)
					var killed bool
					if e := s.Pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, chatPID).Scan(&killed); e != nil || !killed {
						t.Fatal(e, killed)
					}
					select {
					case code := <-cd:
						if code != 409 {
							t.Fatal("lost chat status", code)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if n := scalarInt(t, s, `SELECT count(*) FROM core.chat_send_outcome WHERE state='unknown'`); n != 1 {
						t.Fatal("chat UNKNOWN not persisted", n)
					}
					resp := hardServe(ctx, h, path, body)
					if resp.Code != 409 || cw.calls.Load() != 1 {
						t.Fatal("chat replay", resp.Code, cw.calls.Load())
					}
					b4Close(ja.release)
					if e := b3ContentDone(t, jd); e != nil || gatewayState(t, s, r.OperationID) != "OBSERVED" {
						t.Fatal("healthy join normal stop", e)
					}
					t.Logf("chat guard pid=%d lost with join healthy: unknown persisted replay=0; join normal stop OBSERVED", chatPID)
				} else if mode == "one_guard_lost" {
					var killed bool
					if e := s.Pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&killed); e != nil || !killed {
						t.Fatal(e, killed)
					}
					e := b3ContentDone(t, jd)
					if Code(e) != "EXTERNAL_OUTCOME_UNKNOWN" || gatewayState(t, s, r.OperationID) != "UNKNOWN" {
						t.Fatal(e, gatewayState(t, s, r.OperationID))
					}
					if n := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE task_id=$1 AND state='reserved'`, r.TaskID); n != 1 {
						t.Fatal("unknown business slot released", n)
					}
					_, _ = s.Dispatch(ctx, join, r.OperationID, identityOK, ja)
					if ja.calls != 1 {
						t.Fatal("unknown replay")
					}
					b4Close(cw.release)
					select {
					case code := <-cd:
						if code != 200 {
							t.Fatal("healthy sibling normal stop", code)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					t.Logf("join guard pid=%d lost while chat guard healthy: UNKNOWN persisted, reserved=1 replay=0; sibling normal stop=200", pid)
				} else {
					ud := make(chan int, 1)
					op := s.ContentHandler(func(*http.Request) (Actor, error) { return contentOperator(), nil })
					go func() { ud <- hardRuleRequest(ctx, op, "update").Code }()
					hardWait(t, s, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND cardinality(pg_blocking_pids(pid))=2`)
					if n := scalarInt(t, s, `SELECT count(*) FROM core.content_rule`); n != 0 {
						t.Fatal("updated inside two writers")
					}
					b4Close(ja.release)
					if e := b3ContentDone(t, jd); e != nil {
						t.Fatal(e)
					}
					hardWait(t, s, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND cardinality(pg_blocking_pids(pid))=1`)
					if n := scalarInt(t, s, `SELECT count(*) FROM core.content_rule`); n != 0 {
						t.Fatal("updated while second writer active")
					}
					select {
					case code := <-ud:
						t.Fatalf("early update=%d", code)
					default:
					}
					b4Close(cw.release)
					select {
					case code := <-cd:
						if code != 200 {
							t.Fatal(code)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					select {
					case code := <-ud:
						if code != 200 {
							t.Fatal(code)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					d := chatTestDeadline(t)
					chatSeedTrigger(t, s, "trig_after_mixed", "rt_chat", "acc_chat", kind, "424242", "20001", nil, d, 1)
					p, e := s.IssueChatSendPermit(ctx, chat, "after_mixed", chatPermitRequest("send_after_mixed", "acc_chat", "trig_after_mixed", kind, "424242", 0, 1, d))
					if e != nil {
						t.Fatal(e)
					}
					resp := hardServe(ctx, h, chatPermitPath+"/"+string(p.PermitId)+"/gateway-send", map[string]any{"send_id": p.SendId, "segment_index": 0, "text": "fixture text"})
					if resp.Code != 403 || cw.calls.Load() != 1 {
						t.Fatal(resp.Code, resp.Body.String(), cw.calls.Load())
					}
					if n := scalarInt(t, s, `SELECT count(*) FROM core.content_review WHERE rules_version=1 AND decision='blocked'`); n != 1 {
						t.Fatal(n)
					}
					t.Log(fmt.Sprintf("%s+join: holders=2 -> update blockers=2 -> join finished blockers=1 revision=0 -> chat finished update=200 revision=1 -> next send=403 writer delta=0", kind))
				}
				hardCleanup(t, s)
			})
		}
	}
}
