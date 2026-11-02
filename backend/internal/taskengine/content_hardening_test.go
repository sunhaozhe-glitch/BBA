package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func hardJoin(t *testing.T, reason, answer string) (*Store, RuntimeActor, Reservation) {
	t.Helper()
	s := fixtureStore(t)
	seedAccount(t, s, "acc_hard", "rt_hard", 5)
	task := createReadyTaskWithText(t, s, []string{"acc_hard"}, []string{"998877"}, 1, 3, reason, answer)
	r := reserveFixture(t, s, task.Id, "acc_hard")
	a := fixtureRuntime("acc_hard", "rt_hard")
	acceptFixture(t, s, a, r)
	if _, e := s.IssuePermit(context.Background(), a, r.OperationID, 30*time.Second); e != nil {
		t.Fatal(e)
	}
	return s, a, r
}
func hardRule(t *testing.T, s *Store, kind, expr string) {
	t.Helper()
	doc, _ := json.Marshal(ContentRule{Name: "fixture", Kind: kind, Expression: expr, CaseSensitive: true, Enabled: true})
	if _, e := s.Pool.Exec(context.Background(), `INSERT INTO core.content_rule(workspace_id,id,version,document) VALUES('ws_fixture',$1,1,$2)`, newID("rule"), doc); e != nil {
		t.Fatal(e)
	}
}
func TestB3HardJoinFields(t *testing.T) {
	for _, c := range []struct {
		name, reason, answer, kind, expr string
		hits                             int
	}{
		{"anchored_reason", "违禁", "安全", "pattern", "^违禁$", 1},
		{"anchored_answer", "安全", "违禁", "pattern", "^违禁$", 1},
		{"cross_field", "违", "禁", "keyword", "违禁", 0},
		{"both", "违禁", "违禁", "pattern", "^违禁$", 2},
		{"optional_answer", "安全", "", "keyword", "违禁", 0},
		{"optional_reason", "", "安全", "keyword", "违禁", 0},
		{"optional_both", "", "", "keyword", "违禁", 0},
		{"unicode_newline", "前\n违禁", "安全", "keyword", "违禁", 1},
		{"no_normalize", "e\u0301", "é", "pattern", "^é$", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, a, r := hardJoin(t, c.reason, c.answer)
			hardRule(t, s, c.kind, c.expr)
			w := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted"}}
			_, e := s.Dispatch(context.Background(), a, r.OperationID, identityOK, w)
			t.Logf("error=%v writer_calls=%d", e, w.count())
			want := 1
			if c.hits > 0 {
				want = 0
			}
			if w.count() != want {
				t.Errorf("writer=%d want=%d", w.count(), want)
			}
			var evidence []byte
			var dg string
			if er := s.Pool.QueryRow(context.Background(), `SELECT evidence,payload_digest FROM core.content_review WHERE send_id=$1`, r.OperationID).Scan(&evidence, &dg); er != nil {
				t.Fatal(er)
			}
			var ev struct {
				Hits []struct {
					Field string `json:"field"`
					Start int    `json:"byte_start"`
					End   int    `json:"byte_end"`
				} `json:"hits"`
			}
			if er := json.Unmarshal(evidence, &ev); er != nil {
				t.Fatal(er)
			}
			if len(ev.Hits) != c.hits {
				t.Errorf("hits=%d want=%d", len(ev.Hits), c.hits)
			}
			for _, h := range ev.Hits {
				text := c.reason
				if h.Field == "answer" {
					text = c.answer
				} else if h.Field != "reason" {
					t.Errorf("field=%q", h.Field)
				}
				if h.Start < 0 || h.End > len(text) || h.End <= h.Start {
					t.Errorf("offset=%+v", h)
				}
			}
			wantDG, _ := digest(r)
			if dg != wantDG {
				t.Error("final reservation digest differs")
			}
		})
	}
}

// Real DB barrier: park a statement AFTER review while the content connection
// remains idle in transaction. Killing only that PID used to let writer run.
func hardWait(t *testing.T, s *Store, sql string, args ...any) int {
	t.Helper()
	ctx, c := context.WithTimeout(context.Background(), 8*time.Second)
	defer c()
	for ctx.Err() == nil {
		var pid int
		e := s.Pool.QueryRow(ctx, sql, args...).Scan(&pid)
		if e == nil && pid > 0 {
			return pid
		}
		if e != nil && e != pgx.ErrNoRows {
			t.Fatal(e)
		}
	}
	t.Fatal("database barrier not reached")
	return 0
}
func TestB3HardBridgeLockLoss(t *testing.T) {
	s, a, r := hardJoin(t, "fixture reason", "fixture answer")
	hold, e := s.Pool.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer hold.Rollback(context.Background())
	var blocker int
	if e = hold.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&blocker); e != nil {
		t.Fatal(e)
	}
	if _, e = hold.Exec(context.Background(), `SELECT 1 FROM core.subtask WHERE id=$1 FOR UPDATE`, r.SubtaskID); e != nil {
		t.Fatal(e)
	}
	w := &fakeJoinAdapter{}
	done := make(chan error, 1)
	ctx, c := context.WithTimeout(context.Background(), 12*time.Second)
	defer c()
	go func() { _, e := s.Dispatch(ctx, a, r.OperationID, identityOK, w); done <- e }()
	hardWait(t, s, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1::int=ANY(pg_blocking_pids(pid)) AND query LIKE '%SELECT state,attempt_count,current_execution_id%'`, blocker)
	pid := hardWait(t, s, `SELECT a.pid FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.datname=current_database() AND l.locktype='advisory' AND l.granted AND l.classid=((hashtextextended('ws_fixture',732613)>>32)&4294967295)::oid AND l.objid=(hashtextextended('ws_fixture',732613)&4294967295)::oid`)
	var killed bool
	if e = s.Pool.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, pid).Scan(&killed); e != nil || !killed {
		t.Fatalf("terminate exact fixture pid=%d %v", pid, e)
	}
	if e = hold.Rollback(context.Background()); e != nil {
		t.Fatal(e)
	}
	e = b3ContentDone(t, done)
	t.Logf("terminated_fixture_pid=%d error=%v writer_calls=%d flight=%d", pid, e, w.count(), activeLocalDispatches.count(r.WorkspaceID, r.TaskID))
	if e == nil || w.count() != 0 {
		t.Errorf("lock lost before writer: error=%v writer=%d", e, w.count())
	}
}

func hardChat(t *testing.T) (*Store, RuntimeActor, string, map[string]any) {
	t.Helper()
	s, a, d := chatFixture(t, 20)
	chatSeedTrigger(t, s, "trig_hard", "rt_chat", "acc_chat", "group", "424242", "20001", nil, d, 1)
	p, e := s.IssueChatSendPermit(context.Background(), a, "hard", chatPermitRequest("send_hard", "acc_chat", "trig_hard", "group", "424242", 0, 1, d))
	if e != nil {
		t.Fatal(e)
	}
	return s, a, chatPermitPath + "/" + string(p.PermitId) + "/gateway-send", map[string]any{"send_id": p.SendId, "segment_index": 0, "text": "fixture text"}
}
func hardServe(ctx context.Context, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, bytes.NewReader(b)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "hard")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type hardChatWriter struct {
	calls            atomic.Int32
	entered, release chan struct{}
}

func (w *hardChatWriter) SendChatText(ctx context.Context, _ RuntimeActor, _, _, _ string) (string, error) {
	w.calls.Add(1)
	if w.entered != nil {
		close(w.entered)
		select {
		case <-w.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "hard_message", nil
}
