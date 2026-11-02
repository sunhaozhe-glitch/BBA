package taskengine

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

func TestCharacterLegacyMigrationAlwaysDenied(t *testing.T) {
	for _, rules := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty-rules", true: "enabled-rules"}[rules], func(t *testing.T) {
			s, a, d := chatFixture(t, 10)
			ctx := context.Background()
			chatSeedTrigger(t, s, "migration", "rt_chat", "acc_chat", "group", "424242", "20001", nil, d, 1)
			p, e := s.IssueChatSendPermit(ctx, a, "migration", chatPermitRequest("migration:0", "acc_chat", "migration", "group", "424242", 0, 1, d))
			if e != nil {
				t.Fatal(e)
			}
			if rules {
				contentTestRule(t, s, "unmatched")
			}
			req := ChatConsumeRequest{SendId: string(p.SendId), SegmentIndex: 0, TextDigest: string(p.TextDigest)}
			for i := 0; i < 2; i++ {
				if _, e = s.ConsumeChatSendPermit(ctx, a, string(p.PermitId), req); Code(e) != "CONTENT_FINAL_TEXT_REQUIRED" {
					t.Fatalf("legacy grant: %v", e)
				}
			}
			if e = s.ConsumeChatPermitText(ctx, a, string(p.PermitId), string(p.TextDigest)); Code(e) != "CONTENT_FINAL_TEXT_REQUIRED" {
				t.Fatalf("legacy alias: %v", e)
			}
			other := a
			other.AccountIDs = []string{"other"}
			if _, e = s.ConsumeChatSendPermit(ctx, other, string(p.PermitId), req); Code(e) != chatCodePermitUnknown {
				t.Fatalf("foreign permit: %v", e)
			}
			wrong := req
			wrong.TextDigest = ChatTextDigest("changed")
			if _, e = s.ConsumeChatSendPermit(ctx, a, string(p.PermitId), wrong); Code(e) != chatCodeSendDigestConflict {
				t.Fatalf("changed: %v", e)
			}
			still, e := s.GetChatSendPermit(ctx, a, string(p.PermitId))
			if e != nil || still.State != "issued" {
				t.Fatalf("legacy consumed permit: %+v %v", still, e)
			}
			writer := &contentWriter{}
			h := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return a, nil }, writer)
			body := map[string]any{"send_id": p.SendId, "segment_index": 0, "text": "fixture text", "account_id": "acc_chat", "conversation_kind": "group", "conversation_id": "424242"}
			path := chatPermitPath + "/" + string(p.PermitId) + "/gateway-send"
			w := contentRequest(t, h, path, "POST", body)
			if w.Code != 200 || writer.calls.Load() != 1 {
				t.Fatalf("migration writer %d %s %d", w.Code, w.Body, writer.calls.Load())
			}
			t.Logf("rules=%v legacy=denied gateway=sent writer=1", rules)
		})
	}
}

func TestCharacterGatewayScopeAndConcurrentReplay(t *testing.T) {
	for _, mode := range []string{"account", "group", "private", "concurrent", "outcome-storage-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, a, d := chatFixture(t, 10)
			ctx := context.Background()
			chatSeedTrigger(t, s, "scope", "rt_chat", "acc_chat", "group", "424242", "20001", nil, d, 1)
			p, e := s.IssueChatSendPermit(ctx, a, "scope", chatPermitRequest("scope:0", "acc_chat", "scope", "group", "424242", 0, 1, d))
			if e != nil {
				t.Fatal(e)
			}
			writer := &contentWriter{}
			h := s.ChatSendGatewayHandler(func(*http.Request) (RuntimeActor, error) { return a, nil }, writer)
			body := map[string]any{"send_id": p.SendId, "segment_index": 0, "text": "fixture text", "account_id": "acc_chat", "conversation_kind": "group", "conversation_id": "424242"}
			path := chatPermitPath + "/" + string(p.PermitId) + "/gateway-send"
			switch mode {
			case "account":
				body["account_id"] = "other"
			case "group":
				body["conversation_id"] = "99999"
			case "private":
				body["conversation_kind"] = "private"
			case "outcome-storage-failure":
				_, e = s.Pool.Exec(ctx, `ALTER TABLE core.chat_send_outcome RENAME TO fixture_outcome_offline`)
				if e != nil {
					t.Fatal(e)
				}
			}
			if mode == "concurrent" {
				var wg sync.WaitGroup
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						w := contentRequest(t, h, path, "POST", body)
						if w.Code != 200 {
							t.Errorf("concurrent: %d %s", w.Code, w.Body)
						}
					}()
				}
				wg.Wait()
				if writer.calls.Load() != 1 {
					t.Fatal(writer.calls.Load())
				}
				return
			}
			w := contentRequest(t, h, path, "POST", body)
			if mode == "outcome-storage-failure" {
				if w.Code != 409 || writer.calls.Load() != 1 {
					t.Fatalf("storage: %d %s %d", w.Code, w.Body, writer.calls.Load())
				}
				w = contentRequest(t, h, path, "POST", body)
				if w.Code != 409 || writer.calls.Load() != 1 {
					t.Fatal("replayed writer after storage failure")
				}
			} else if w.Code != 409 || writer.calls.Load() != 0 {
				t.Fatalf("scope: %d %s writer=%d", w.Code, w.Body, writer.calls.Load())
			}
		})
	}
}
