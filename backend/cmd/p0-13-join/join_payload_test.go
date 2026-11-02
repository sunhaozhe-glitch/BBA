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

func TestJoinPayloadHTTPFields(t *testing.T) {
	for _, answer := range []string{"答案独立\n\"\\", ""} {
		t.Run(answer, func(t *testing.T) {
			reservation := taskengine.Reservation{GroupID: "829117089", JoinReason: "申请原因独立\n\"\\", VerificationMessage: answer}
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/Plugin/ext/napcat-plugin-cleaner/join-group" {
					t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL.Path)
				}
				var fields map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
					t.Error(err)
					return
				}
				if len(fields) != 4 {
					t.Errorf("payload field count=%d", len(fields))
				}
				if string(fields["group_id"]) != reservation.GroupID {
					t.Errorf("group_id=%s", fields["group_id"])
				}
				for key, want := range map[string]string{"confirm": "1", "reason": reservation.JoinReason, "answer": reservation.VerificationMessage} {
					var got string
					if err := json.Unmarshal(fields[key], &got); err != nil || got != want {
						t.Errorf("%s=%q want=%q err=%v", key, got, want, err)
					}
				}
				fmt.Fprint(w, `{"ok":true,"result":1}`)
			}))
			defer server.Close()
			adapter := &cleanerAdapter{runner: p013TestRunner(t, server.URL), cred: "fixture", groupID: reservation.GroupID, reason: reservation.JoinReason, answer: reservation.VerificationMessage}
			outcome, err := adapter.RequestJoin(context.Background(), reservation)
			if err != nil || outcome.Disposition != "submitted" || calls.Load() != 1 {
				t.Fatalf("outcome=%+v err=%v calls=%d", outcome, err, calls.Load())
			}
		})
	}
}

func TestJoinPayloadMismatchNoHTTP(t *testing.T) {
	for _, field := range []string{"group_id", "reason", "answer", "redistributed_text", "empty_reservation"} {
		t.Run(field, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"ok":true,"result":1}`) }))
			defer server.Close()
			adapter := &cleanerAdapter{runner: p013TestRunner(t, server.URL), cred: "fixture", groupID: "829117089", reason: "ab", answer: "c"}
			res := taskengine.Reservation{GroupID: adapter.groupID, JoinReason: adapter.reason, VerificationMessage: adapter.answer}
			switch field {
			case "group_id":
				res.GroupID = "829117090"
			case "reason":
				res.JoinReason = "changed"
			case "answer":
				res.VerificationMessage = "changed"
			case "redistributed_text":
				res.JoinReason = "a"
				res.VerificationMessage = "bc"
			case "empty_reservation":
				res = taskengine.Reservation{}
			}
			outcome, err := adapter.RequestJoin(context.Background(), res)
			if err == nil || outcome.Disposition == "submitted" || calls.Load() != 0 || adapter.runner.qqCalls != 0 {
				t.Fatalf("mismatch=%s outcome=%+v err=%v HTTP calls=%d budget=%d; want calls=0", field, outcome, err, calls.Load(), adapter.runner.qqCalls)
			}
			t.Log("mismatch rejected; HTTP calls=0")
		})
	}
}
