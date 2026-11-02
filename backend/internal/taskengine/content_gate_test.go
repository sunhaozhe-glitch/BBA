package taskengine

import (
	"bba.local/taskengine/internal/contract"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type contentWriter struct {
	calls   atomic.Int32
	unknown bool
}

func (w *contentWriter) SendChatText(context.Context, RuntimeActor, string, string, string) (string, error) {
	w.calls.Add(1)
	if w.unknown {
		return "", errors.New("fixture timeout")
	}
	return "fixture_message", nil
}
func contentRequest(t *testing.T, h http.Handler, path, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "fixture-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestB3FinalWriterHit(t *testing.T) {
	s, a, d := chatFixture(t, 10)
	ctx := context.Background()
	// RED deliberately exercises the actual gateway HTTP handler, not matcher mocks.
	_, e := s.Pool.Exec(ctx, `INSERT INTO core.content_rule(workspace_id,id,version,document) VALUES('ws_fixture','rule_fixture',1,'{"name":"隔离测试","kind":"keyword","expression":"fixture text","case_sensitive":true,"group":"fixture","priority":1,"enabled":true,"deleted":false}'::jsonb)`)
	if e != nil {
		t.Fatal(e)
	}
	chatSeedTrigger(t, s, "trig_b3", "rt_chat", "acc_chat", "group", "424242", "20001", nil, d, 1)
	p, e := s.IssueChatSendPermit(ctx, a, "b3-hit", chatPermitRequest("b3-hit", "acc_chat", "trig_b3", "group", "424242", 0, 1, d))
	if e != nil {
		t.Fatal(e)
	}
	writer := &contentWriter{}
	h := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return a, nil }, writer)
	w := contentRequest(t, h, chatPermitPath+"/"+string(p.PermitId)+"/gateway-send", "POST", map[string]any{"send_id": p.SendId, "segment_index": 0, "text": "fixture text"})
	if w.Code != 403 || writer.calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, writer.calls.Load(), w.Body)
	}
	if n := scalarInt(t, s, `SELECT count(*) FROM core.content_review WHERE workspace_id='ws_fixture' AND decision='blocked'`); n != 1 {
		t.Fatalf("reviews=%d", n)
	}
}

func contentOperator() Actor {
	a := fixtureActor()
	a.Permissions["content.read"] = true
	a.Permissions["content.configure"] = true
	a.Permissions["content.review"] = true
	return a
}
func contentTestRule(t *testing.T, s *Store, expression string) ContentRule {
	t.Helper()
	h := s.ContentHandler(func(*http.Request) (Actor, error) { return contentOperator(), nil })
	w := contentRequest(t, h, "/v1/operator/content-rules", "POST", map[string]any{"name": "fixture rule", "kind": "keyword", "expression": expression, "case_sensitive": true, "group": "fixture", "priority": 100, "enabled": true, "expected_version": 0})
	if w.Code != 200 {
		t.Fatalf("rule status=%d %s", w.Code, w.Body)
	}
	var b struct {
		Data ContentRule `json:"data"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &b); e != nil {
		t.Fatal(e)
	}
	return b.Data
}
func TestB3GatewayScenarios(t *testing.T) {
	for _, mode := range []string{"allowed", "unknown", "changed-payload", "preview-then-rule-change", "cross-account", "cross-workspace", "storage-failure", "multisegment", "cq-media", "legacy-digest"} {
		t.Run(mode, func(t *testing.T) {
			s, a, d := chatFixture(t, 20)
			ctx := context.Background()
			text := "fixture text"
			segments := 1
			if mode == "multisegment" {
				segments = 2
			}
			if mode == "cq-media" {
				text = "[CQ:image,file=fixture]"
			}
			chatSeedTrigger(t, s, "trig_case", "rt_chat", "acc_chat", "group", "424242", "20001", nil, d, segments)
			req := chatPermitRequest("send_case", "acc_chat", "trig_case", "group", "424242", 0, segments, d)
			req.TextDigest = contract.Digest(ChatTextDigest(text))
			p, e := s.IssueChatSendPermit(ctx, a, "case", req)
			if e != nil {
				t.Fatal(e)
			}
			contentTestRule(t, s, "DO_NOT_MATCH_fixture")
			op := s.ContentHandler(func(*http.Request) (Actor, error) { return contentOperator(), nil })
			pre := contentRequest(t, op, "/v1/operator/content-rules/preview", "POST", map[string]any{"text": text})
			if pre.Code != 200 {
				t.Fatal(pre.Body)
			}
			if scalarInt(t, s, `SELECT count(*) FROM core.content_review`) != 0 {
				t.Fatal("preview wrote review")
			}
			writer := &contentWriter{unknown: mode == "unknown"}
			want := 200
			switch mode {
			case "changed-payload":
				text = "changed text"
				want = 409
			case "preview-then-rule-change":
				contentTestRule(t, s, "fixture text")
				want = 403
			case "cross-account":
				a.AccountIDs = []string{"other"}
				want = 404
			case "cross-workspace":
				a.WorkspaceID = "other"
				want = 404
			case "storage-failure":
				if _, e = s.Pool.Exec(ctx, `ALTER TABLE core.content_review RENAME TO fixture_review_offline`); e != nil {
					t.Fatal(e)
				}
				want = 500
			case "multisegment", "cq-media", "legacy-digest":
				want = 403
			case "unknown":
				want = 409
			}
			h := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return a, nil }, writer)
			if mode == "legacy-digest" {
				_, e = s.ConsumeChatSendPermit(ctx, a, string(p.PermitId), ChatConsumeRequest{SendId: string(p.SendId), SegmentIndex: 0, TextDigest: string(p.TextDigest)})
				if Code(e) != "CONTENT_FINAL_TEXT_REQUIRED" {
					t.Fatalf("legacy=%v", e)
				}
				return
			}
			path := chatPermitPath + "/" + string(p.PermitId) + "/gateway-send"
			body := map[string]any{"send_id": p.SendId, "segment_index": 0, "text": text}
			w := contentRequest(t, h, path, "POST", body)
			if w.Code != want {
				t.Fatalf("status=%d want=%d %s", w.Code, want, w.Body)
			}
			expected := int32(0)
			if mode == "allowed" || mode == "unknown" {
				expected = 1
				w = contentRequest(t, h, path, "POST", body)
				if w.Code != want {
					t.Fatalf("replay=%d %s", w.Code, w.Body)
				}
			}
			if writer.calls.Load() != expected {
				t.Fatalf("writer calls=%d want=%d", writer.calls.Load(), expected)
			}
			t.Logf("mode=%s status=%d writer_calls=%d", mode, w.Code, writer.calls.Load())
		})
	}
}
func TestB3RuleCRUDIsolationAndPersistence(t *testing.T) {
	s := fixtureStore(t)
	a := contentOperator()
	h := s.ContentHandler(func(*http.Request) (Actor, error) { return a, nil })
	r := contentTestRule(t, s, "中间词")
	path := "/v1/operator/content-rules/" + r.ID
	// Concurrent edits: one committed history, one conflict.
	body := map[string]any{"name": "changed", "kind": "keyword", "expression": "中间词", "case_sensitive": true, "group": "分组", "priority": 10, "enabled": false, "expected_version": 1}
	ch := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { ch <- contentRequest(t, h, path, "PUT", body).Code }()
	}
	x, y := <-ch, <-ch
	if !((x == 200 && y == 409) || (x == 409 && y == 200)) {
		t.Fatalf("concurrent statuses=%d %d", x, y)
	}
	hist := contentRequest(t, h, path+"/history", "GET", nil)
	if hist.Code != 200 {
		t.Fatal(hist.Body)
	}
	reopened, e := Open(context.Background(), s.Pool.Config().ConnString())
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	h2 := reopened.ContentHandler(func(*http.Request) (Actor, error) { return a, nil })
	if got := contentRequest(t, h2, path+"/history", "GET", nil); got.Body.String() != hist.Body.String() {
		t.Fatalf("restart history differs %s %s", got.Body, hist.Body)
	}
	a.Permissions = map[string]bool{"content.read": true}
	if w := contentRequest(t, h, path, "PUT", body); w.Code != 403 {
		t.Fatal(w.Code)
	}
	_, e = s.Pool.Exec(context.Background(), `INSERT INTO core.workspace(id) VALUES('other');INSERT INTO core.principal(workspace_id,id) VALUES('other','principal_fixture')`)
	if e != nil {
		t.Fatal(e)
	}
	a.WorkspaceID = "other"
	a.Permissions["content.configure"] = true
	if w := contentRequest(t, h, path+"/history", "GET", nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := contentRequest(t, h, path, "PUT", body); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func TestB3MatcherSemantics(t *testing.T) {
	for _, c := range []struct {
		expr, text, kind string
		sensitive, hit   bool
	}{{"甲乙", "甲乙", "keyword", true, true}, {"甲乙", "甲\n乙", "keyword", true, false}, {"abc", "ABC", "keyword", false, true}, {"a.b", "a.b", "keyword", true, true}, {"a.b", "axb", "keyword", true, false}, {"a.b", "a\nb", "pattern", true, false}, {"(?s)a.b", "a\nb", "pattern", true, true}, {"é", "e\u0301", "keyword", true, false}} {
		r := ContentRule{Name: "test", Kind: c.kind, Expression: c.expr, CaseSensitive: c.sensitive, Enabled: true}
		hits, e := contentMatch([]ContentRule{r}, c.text)
		if e != nil || (len(hits) > 0) != c.hit {
			t.Fatalf("%+v hits=%v err=%v", c, hits, e)
		}
	}
	for _, expr := range []string{"(?<=a)b", "(a)\\1", "a*"} {
		if _, e := contentMatcher(ContentRule{Name: "test", Kind: "pattern", Expression: expr}); e == nil {
			t.Fatalf("accepted %s", expr)
		}
	}
}
