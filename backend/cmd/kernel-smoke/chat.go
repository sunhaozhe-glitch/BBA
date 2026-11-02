package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"bba.local/taskengine/internal/taskengine"
)

// smokeChat exercises real handlers and PostgreSQL with synthetic identities.
// The transport is a counter; this command has no QQ transport.
func smokeChat(ctx context.Context, s *taskengine.Store, runtime taskengine.RuntimeActor) {
	auth := func(r *http.Request) (taskengine.RuntimeActor, error) {
		if r.Header.Get("Authorization") != "Bearer fixture-runtime" {
			return taskengine.RuntimeActor{}, fmt.Errorf("unauthenticated")
		}
		return runtime, nil
	}
	mux := http.NewServeMux()
	chat := s.ChatRuntimeHandler(auth)
	writer := &smokeChatWriter{}
	permits := s.ChatSendGatewayHandler(auth, writer)
	mux.Handle("/v1/internal/chat-context", chat)
	mux.Handle("/v1/internal/chat-triggers", chat)
	mux.Handle("/v1/internal/chat-send-permits", permits)
	mux.Handle("/v1/internal/chat-send-permits/", permits)
	server := httptest.NewServer(mux)
	defer server.Close()
	call := func(method, path, key string, body any, status int) map[string]any {
		var encoded []byte
		var err error
		if body != nil {
			encoded, err = json.Marshal(body)
			must(err)
		}
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, bytes.NewReader(encoded))
		must(err)
		req.Header.Set("Authorization", "Bearer fixture-runtime")
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		response, err := server.Client().Do(req)
		must(err)
		defer response.Body.Close()
		var envelope map[string]any
		must(json.NewDecoder(response.Body).Decode(&envelope))
		check(response.StatusCode == status, fmt.Sprintf("chat %s status=%d want=%d body=%v", path, response.StatusCode, status, envelope))
		if data, ok := envelope["data"].(map[string]any); ok {
			return data
		}
		return envelope
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO core.chat_send_policy(workspace_id,account_id,enabled,daily_limit) VALUES('ws_fixture','acc_a',true,10)`)
	must(err)
	projection := call("GET", "/v1/internal/chat-context?account_id=acc_a&conversation_kind=private&conversation_id=30001", "", nil, 200)
	check(projection["binding_epoch"] == "1" && projection["chat_enabled"] == true, "chat context authority mismatch")
	deadline := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond).Format(time.RFC3339Nano)
	trigger := map[string]any{"trigger_ref": "chat_smoke", "account_id": "acc_a", "conversation_kind": "private", "conversation_id": "30001", "source_event_id": "chat_event", "sender_id": "30001", "directed_account_id": "acc_a", "deadline_at": deadline, "segment_count": 1}
	call("POST", "/v1/internal/chat-triggers", "chat_smoke", trigger, 201)
	text := "offline plain text"
	digest := taskengine.ChatTextDigest(text)
	issue := map[string]any{"send_id": "chat_smoke:0", "account_id": "acc_a", "conversation_kind": "private", "conversation_id": "30001", "trigger_ref": "chat_smoke", "text_digest": digest, "expected_binding_epoch": "1", "deadline_at": deadline, "segment_index": 0, "segment_count": 1}
	permit := call("POST", "/v1/internal/chat-send-permits", "chat_smoke:0", issue, 200)
	path := "/v1/internal/chat-send-permits/" + permit["permit_id"].(string)
	consume := map[string]any{"send_id": "chat_smoke:0", "segment_index": 0, "text_digest": digest}
	call("POST", path+"/consume", "chat_smoke:0", consume, 403)
	body := map[string]any{"send_id":"chat_smoke:0","segment_index":0,"text":text,"account_id":"acc_a","conversation_kind":"private","conversation_id":"30001"}
	call("POST",path+"/gateway-send","chat_smoke:0",body,200)
	call("POST",path+"/gateway-send","chat_smoke:0",body,200)
	check(writer.calls==1,"gateway replay duplicated writer")
	trigger["trigger_ref"],trigger["source_event_id"]="chat_paused","event_paused"
	call("POST","/v1/internal/chat-triggers","chat_paused",trigger,201)
	issue["send_id"],issue["trigger_ref"]="chat_paused:0","chat_paused"
	second:=call("POST","/v1/internal/chat-send-permits","chat_paused:0",issue,200)
	_,err=s.Pool.Exec(ctx,`UPDATE core.account SET control_state='paused' WHERE workspace_id='ws_fixture' AND id='acc_a'`);must(err)
	body["send_id"]="chat_paused:0"
	call("POST","/v1/internal/chat-send-permits/"+second["permit_id"].(string)+"/gateway-send","chat_paused:0",body,409)
	check(writer.calls==1,"paused gateway reached writer")
	var consumedRows, outcomeRows int
	must(s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM core.chat_send_permit WHERE state='consumed'),(SELECT count(*) FROM core.chat_send_outcome)`).Scan(&consumedRows, &outcomeRows))
	check(consumedRows == 1 && outcomeRows == 1, "chat durable rows duplicated")
	fmt.Printf("PASS: chat HTTP context->trigger->issue->final-gateway->recording-writer; adapter_calls=%d consumed=%d outcomes=%d; replay no writer; legacy denied; paused denied; real_QQ_calls=0\n", writer.calls, consumedRows, outcomeRows)
}

type smokeChatWriter struct {calls int}
func(w *smokeChatWriter) SendChatText(context.Context,taskengine.RuntimeActor,string,string,string)(string,error){w.calls++;return "smoke-message",nil}
