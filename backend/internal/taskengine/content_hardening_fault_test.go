package taskengine

import (
	"context"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func hardCleanup(t *testing.T, s *Store) {
	t.Helper()
	// Socket close/cancel is asynchronous at PostgreSQL. Observe release via DB,
	// not a guessed sleep and not a claim that Close synchronously removes a PID.
	remediationWait(t, "fixture guard sessions and locks released", func() bool {
		return scalarInt(t, s, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name='bba_content_guard'`) == 0
	})
	if n := scalarInt(t, s, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`); n != 0 {
		t.Errorf("leaked locks=%d", n)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name='bba_content_guard'`); n != 0 {
		t.Errorf("leaked guard connections=%d", n)
	}
	if n := s.Pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("leaked pooled connections=%d", n)
	}
	t.Log("readback: guard_connections=0 advisory_locks=0 acquired_pool_connections=0")
}
func hardKill(t *testing.T, s *Store) int {
	t.Helper()
	pid := hardWait(t, s, `SELECT a.pid FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.datname=current_database() AND l.locktype='advisory' AND l.granted AND l.classid=((hashtextextended('ws_fixture',732613)>>32)&4294967295)::oid AND l.objid=(hashtextextended('ws_fixture',732613)&4294967295)::oid`)
	var killed bool
	if e := s.Pool.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, pid).Scan(&killed); e != nil || !killed {
		t.Fatalf("terminate fixture pid=%d %v", pid, e)
	}
	return pid
}
func TestB3HardChatLockLoss(t *testing.T) {
	s, a, path, body := hardChat(t)
	ctx, c := context.WithTimeout(context.Background(), 12*time.Second)
	defer c()
	hold, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer hold.Rollback(context.Background())
	var blocker int
	if e = hold.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); e != nil {
		t.Fatal(e)
	}
	if _, e = hold.Exec(ctx, `SELECT 1 FROM core.chat_trigger WHERE trigger_ref='trig_hard' FOR UPDATE`); e != nil {
		t.Fatal(e)
	}
	w := &contentWriter{}
	h := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return a, nil }, w)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- hardServe(ctx, h, path, body) }()
	hardWait(t, s, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1::int=ANY(pg_blocking_pids(pid)) AND query LIKE '%core.chat_trigger%'`, blocker)
	pid := hardKill(t, s)
	if e = hold.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case r := <-done:
		if r.Code != 500 || w.calls.Load() != 0 {
			t.Errorf("status=%d writer=%d %s", r.Code, w.calls.Load(), r.Body)
		}
		t.Logf("terminated_fixture_pid=%d status=%d writer_calls=%d", pid, r.Code, w.calls.Load())
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	replay := hardServe(context.Background(), h, path, body)
	if replay.Code != 409 || w.calls.Load() != 0 {
		t.Errorf("replay=%d writer=%d", replay.Code, w.calls.Load())
	}
	hardCleanup(t, s)
}
func TestB3HardPreSendFaults(t *testing.T) {
	for _, route := range []string{"bridge", "chat"} {
		for _, mode := range []string{"rule_load", "review_write", "lock_timeout", "cancel", "pool_one", "pool_one_review_failure"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				var s *Store
				var a RuntimeActor
				var r Reservation
				var path string
				var body map[string]any
				if route == "bridge" {
					s, a, r = hardJoin(t, "fixture reason", "fixture answer")
				} else {
					s, a, path, body = hardChat(t)
				}
				if mode == "pool_one" || mode == "pool_one_review_failure" {
					dsn := s.Pool.Config().ConnString() + "&pool_max_conns=1"
					one, e := Open(context.Background(), dsn)
					if e != nil {
						t.Fatal(e)
					}
					defer one.Close()
					s = one
					if s.Pool.Config().MaxConns != 1 {
						t.Fatal("MaxConns not 1")
					}
				}
				if mode == "rule_load" {
					b3ContentExec(t, s, `ALTER TABLE core.content_rule RENAME TO fixture_rule_offline`)
				}
				if mode == "review_write" || mode == "pool_one_review_failure" {
					b3ContentExec(t, s, `ALTER TABLE core.content_review RENAME TO fixture_review_offline`)
				}
				ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
				defer c()
				started := time.Now()
				var hold pgx.Tx
				var blocker int
				if mode == "lock_timeout" || mode == "cancel" {
					var e error
					hold, e = s.Pool.Begin(ctx)
					if e != nil {
						t.Fatal(e)
					}
					defer hold.Rollback(context.Background())
					if e = contentLock(ctx, hold, "ws_fixture"); e != nil {
						t.Fatal(e)
					}
					if e = hold.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); e != nil {
						t.Fatal(e)
					}
				}
				writer := &contentWriter{}
				adapter := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted"}}
				done := make(chan error, 1)
				status := make(chan int, 1)
				go func() {
					if route == "bridge" {
						_, e := s.Dispatch(ctx, a, r.OperationID, identityOK, adapter)
						done <- e
					} else {
						h := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return a, nil }, writer)
						w := hardServe(ctx, h, path, body)
						status <- w.Code
						done <- nil
					}
				}()
				if hold != nil {
					hardWait(t, s, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND $1::int=ANY(pg_blocking_pids(pid))`, blocker)
					if mode == "cancel" {
						c()
					}
				}
				e := b3ContentDone(t, done)
				calls := adapter.count() + int(writer.calls.Load())
				want := 0
				if mode == "pool_one" {
					want = 1
				}
				if calls != want {
					t.Errorf("writer=%d want=%d error=%v", calls, want, e)
				}
				if route == "bridge" {
					if (e == nil) != (want == 1) {
						t.Errorf("error=%v want calls=%d", e, want)
					}
					if activeLocalDispatches.count(r.WorkspaceID, r.TaskID) != 0 {
						t.Error("flight leaked")
					}
				} else {
					code := <-status
					if (code == 200) != (want == 1) {
						t.Errorf("status=%d want calls=%d", code, want)
					}
					t.Logf("status=%d", code)
				}
				if hold != nil {
					if rollbackErr := hold.Rollback(context.Background()); rollbackErr != nil {
						t.Fatal(rollbackErr)
					}
				}
				hardCleanup(t, s)
				t.Logf("route=%s fault=%s writer_calls=%d elapsed=%s error=%v", route, mode, calls, time.Since(started), e)
			})
		}
	}
}

// Update-first is proven by the real sender waiting behind the update tx.
func TestB3HardUpdateFirst(t *testing.T) {
	for _, route := range []string{"bridge", "chat"} {
		t.Run(route, func(t *testing.T) {
			var s *Store
			var a RuntimeActor
			var r Reservation
			var path string
			var body map[string]any
			if route == "bridge" {
				s, a, r = hardJoin(t, "fixture reason", "fixture answer")
			} else {
				s, a, path, body = hardChat(t)
			}
			ctx, c := context.WithTimeout(context.Background(), 12*time.Second)
			defer c()
			hold, e := s.Pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer hold.Rollback(context.Background())
			if e = contentLock(ctx, hold, "ws_fixture"); e != nil {
				t.Fatal(e)
			}
			var pid int
			_ = hold.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
			expr := "fixture"
			if _, e = hold.Exec(ctx, `INSERT INTO core.content_rule(workspace_id,id,version,document) VALUES('ws_fixture','rule_update',1,jsonb_build_object('name','fixture','kind','keyword','expression',$1::text,'enabled',true))`, expr); e != nil {
				t.Fatal(e)
			}
			if _, e = hold.Exec(ctx, `INSERT INTO core.content_revision(workspace_id,version) VALUES('ws_fixture',1)`); e != nil {
				t.Fatal(e)
			}
			adapter := &fakeJoinAdapter{}
			writer := &contentWriter{}
			done := make(chan error, 1)
			go func() {
				if route == "bridge" {
					_, e := s.Dispatch(ctx, a, r.OperationID, identityOK, adapter)
					done <- e
				} else {
					h := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return a, nil }, writer)
					w := hardServe(ctx, h, path, body)
					if w.Code != 403 {
						done <- problem("WRONG_STATUS")
					} else {
						done <- nil
					}
				}
			}()
			hardWait(t, s, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND $1::int=ANY(pg_blocking_pids(pid))`, pid)
			if e = hold.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			e = b3ContentDone(t, done)
			if route == "bridge" && Code(e) != "CONTENT_BLOCKED" {
				t.Fatal(e)
			}
			if route == "chat" && e != nil {
				t.Fatal(e)
			}
			if adapter.count() != 0 || writer.calls.Load() != 0 {
				t.Fatal("update-first writer invoked")
			}
			if n := scalarInt(t, s, `SELECT count(*) FROM core.content_review WHERE rules_version=1 AND decision='blocked'`); n != 1 {
				t.Errorf("revision evidence=%d", n)
			}
			hardCleanup(t, s)
			t.Log("DB wait barrier -> update commit -> new revision blocked; writer_calls=0")
		})
	}
}

type hardJoinBlock struct {
	entered, release chan struct{}
	calls            int
}

func (w *hardJoinBlock) RequestJoin(ctx context.Context, _ Reservation) (SendOutcome, error) {
	w.calls++
	close(w.entered)
	select {
	case <-w.release:
		return SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "offline_shared_barrier"}, nil
	case <-ctx.Done():
		return SendOutcome{}, ctx.Err()
	}
}
func TestB3HardSendFirst(t *testing.T) {
	for _, route := range []string{"bridge", "chat"} {
		for _, mode := range []string{"update_waits", "kill_in_writer"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				var s *Store
				var a RuntimeActor
				var r Reservation
				var path string
				var body map[string]any
				if route == "bridge" {
					s, a, r = hardJoin(t, "fixture reason", "fixture answer")
				} else {
					s, a, path, body = hardChat(t)
				}
				ctx, c := context.WithTimeout(context.Background(), 12*time.Second)
				defer c()
				entered := make(chan struct{})
				release := make(chan struct{})
				adapter := &hardJoinBlock{entered: entered, release: release}
				writer := &hardChatWriter{entered: entered, release: release}
				done := make(chan error, 1)
				h := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return a, nil }, writer)
				go func() {
					if route == "bridge" {
						_, e := s.Dispatch(ctx, a, r.OperationID, identityOK, adapter)
						done <- e
					} else {
						resp := hardServe(ctx, h, path, body)
						if (mode == "kill_in_writer" && resp.Code != 409) || (mode == "update_waits" && resp.Code != 200) {
							done <- problem("WRONG_STATUS")
						} else {
							done <- nil
						}
					}
				}()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if mode == "update_waits" {
					update := make(chan int, 1)
					go func() {
						op := s.ContentHandler(func(*http.Request) (Actor, error) { return contentOperator(), nil })
						resp := hardServe(ctx, op, "/v1/operator/content-rules", map[string]any{"name": "fixture", "kind": "keyword", "expression": "fixture", "case_sensitive": true, "enabled": true, "expected_version": 0})
						update <- resp.Code
					}()
					hardWait(t, s, `SELECT u.pid FROM pg_stat_activity u WHERE u.datname=current_database() AND u.wait_event='advisory' AND EXISTS(SELECT 1 FROM pg_stat_activity g WHERE g.application_name='bba_content_guard' AND g.datname=current_database() AND g.pid=ANY(pg_blocking_pids(u.pid)))`)
					if n := scalarInt(t, s, `SELECT count(*) FROM core.content_rule`); n != 0 {
						t.Fatal("update committed inside active writer")
					}
					close(release)
					if code := <-update; code != 200 {
						t.Fatalf("update status=%d", code)
					}
				} else {
					pid := hardKill(t, s)
					t.Logf("writer already started; terminated only fixture guard pid=%d", pid)
				}
				e := b3ContentDone(t, done)
				calls := adapter.calls + int(writer.calls.Load())
				if calls != 1 {
					t.Fatalf("writer=%d", calls)
				}
				if mode == "kill_in_writer" {
					if route == "bridge" {
						if Code(e) != "EXTERNAL_OUTCOME_UNKNOWN" || gatewayState(t, s, r.OperationID) != "UNKNOWN" {
							t.Fatalf("unknown=%v state=%s", e, gatewayState(t, s, r.OperationID))
						}
						_, _ = s.Dispatch(context.Background(), a, r.OperationID, identityOK, adapter)
					} else {
						if e != nil {
							t.Fatal(e)
						}
						resp := hardServe(context.Background(), h, path, body)
						if resp.Code != 409 {
							t.Fatal(resp.Body)
						}
					}
					if adapter.calls+int(writer.calls.Load()) != 1 {
						t.Fatal("unknown retried")
					}
				} else if e != nil {
					t.Fatal(e)
				}
				hardCleanup(t, s)
				t.Logf("route=%s mode=%s writer_calls=1 replay_additional=0", route, mode)
			})
		}
	}
}
