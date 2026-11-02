package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"bba.local/taskengine/internal/taskengine"
)

func TestGatewayJoinPayloadHTTPFields(t *testing.T) {
	for _, answer := range []string{"独立答案\n\"\\", ""} {
		t.Run(answer, func(t *testing.T) {
			res := taskengine.Reservation{WorkspaceID: "ws", RuntimeID: "rt", AccountID: "acc", OwnerEpoch: 1, GroupID: "829117089", JoinReason: "独立原因\n\"\\", VerificationMessage: answer}
			var authCalls, joinCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/auth/login" {
					authCalls.Add(1)
					fmt.Fprint(w, `{"code":0,"data":{"Credential":"fixture"}}`)
					return
				}
				joinCalls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/Plugin/ext/napcat-plugin-cleaner/join-group" {
					t.Errorf("endpoint=%s %s", r.Method, r.URL.Path)
				}
				var fields map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
					t.Error(err)
					return
				}
				if len(fields) != 4 {
					t.Errorf("fields=%d", len(fields))
				}
				if string(fields["group_id"]) != res.GroupID {
					t.Errorf("group_id=%s", fields["group_id"])
				}
				for key, want := range map[string]string{"confirm": "1", "reason": res.JoinReason, "answer": res.VerificationMessage} {
					var got string
					if err := json.Unmarshal(fields[key], &got); err != nil || got != want {
						t.Errorf("%s=%q want=%q err=%v", key, got, want, err)
					}
				}
				fmt.Fprint(w, `{"ok":true,"result":1}`)
			}))
			defer server.Close()
			runtime := napcatGateway{config: gatewayRuntimeConfig{WorkspaceID: "ws", RuntimeID: "rt", AccountID: "acc", OwnerEpoch: 1}, webUIURL: server.URL, client: newGatewayHTTPClient()}
			gateway := gatewayServer{runtimes: map[string]napcatGateway{gatewayRuntimeKey("ws", "rt"): runtime}}
			actor := taskengine.RuntimeActor{WorkspaceID: "ws", RuntimeID: "rt", AccountIDs: []string{"acc"}, OwnerEpoch: 1}
			out, err := gateway.RequestJoin(context.Background(), actor, res)
			if err != nil || out.Disposition != "submitted" || authCalls.Load() != 1 || joinCalls.Load() != 1 {
				t.Fatalf("out=%+v err=%v auth=%d join=%d", out, err, authCalls.Load(), joinCalls.Load())
			}
		})
	}
}

func TestGatewayJoinMismatchNoHTTP(t *testing.T) {
	for _, field := range []string{"workspace", "runtime", "account", "owner_epoch", "invalid_group"} {
		t.Run(field, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, `{"code":0,"data":{"Credential":"fixture"},"ok":true,"result":1}`)
			}))
			defer server.Close()
			runtime := napcatGateway{config: gatewayRuntimeConfig{WorkspaceID: "ws", RuntimeID: "rt", AccountID: "acc", OwnerEpoch: 1}, webUIURL: server.URL, client: newGatewayHTTPClient()}
			gateway := gatewayServer{runtimes: map[string]napcatGateway{gatewayRuntimeKey("ws", "rt"): runtime}}
			actor := taskengine.RuntimeActor{WorkspaceID: "ws", RuntimeID: "rt", AccountIDs: []string{"acc"}, OwnerEpoch: 1}
			res := taskengine.Reservation{WorkspaceID: "ws", RuntimeID: "rt", AccountID: "acc", OwnerEpoch: 1, GroupID: "829117089", JoinReason: "reason", VerificationMessage: "answer"}
			switch field {
			case "workspace":
				res.WorkspaceID = "other"
			case "runtime":
				res.RuntimeID = "other"
			case "account":
				res.AccountID = "other"
			case "owner_epoch":
				res.OwnerEpoch++
			case "invalid_group":
				res.GroupID = "invalid"
			}
			out, err := gateway.RequestJoin(context.Background(), actor, res)
			if err == nil || out.Disposition == "submitted" || calls.Load() != 0 {
				t.Fatalf("out=%+v err=%v HTTP calls=%d want calls=0", out, err, calls.Load())
			}
			t.Log("mismatch rejected; HTTP calls=0")
		})
	}
}
