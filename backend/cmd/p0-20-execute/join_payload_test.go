package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"bba.local/taskengine/internal/taskengine"
)

func TestJoinApprovalMismatchNoDispatchHTTP(t *testing.T) {
	for _, issued := range []bool{false, true} {
		for _, field := range []string{"reason", "answer", "redistributed_text", "scope", "target", "execution"} {
			t.Run(fmt.Sprintf("issued=%t/%s", issued, field), func(t *testing.T) {
				res := taskengine.Reservation{OperationID: "op", ExecutionID: "exec", Digest: "digest", AccountID: "acc", TargetID: "target", GroupID: "829117089", JoinReason: "ab", VerificationMessage: "c", OwnerEpoch: 1, Fence: 2, ScopeRevision: 3}
				accepted := taskengine.BridgeReservation{OperationID: res.OperationID, ExecutionID: res.ExecutionID, Digest: res.Digest, AccountID: res.AccountID, TargetID: res.TargetID, GroupID: res.GroupID, JoinReason: res.JoinReason, VerificationMessage: res.VerificationMessage, OwnerEpoch: res.OwnerEpoch, Fence: res.Fence, ScopeRevision: res.ScopeRevision}
				switch field {
				case "reason":
					accepted.JoinReason = "changed"
				case "answer":
					accepted.VerificationMessage = "changed"
				case "redistributed_text":
					accepted.JoinReason = "a"
					accepted.VerificationMessage = "bc"
				case "scope":
					accepted.ScopeRevision++
				case "target":
					accepted.TargetID = "other"
				case "execution":
					accepted.ExecutionID = "other"
				}
				var dispatchCalls, permitCalls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/status"):
						if issued {
							fmt.Fprint(w, `{"data":{"operation":{"operation_id":"op"},"permit":{"state":"issued"}}}`)
						} else {
							fmt.Fprint(w, `{"data":{"operation":{"operation_id":"op"},"permit":null}}`)
						}
					case strings.HasSuffix(r.URL.Path, "/accept"):
						json.NewEncoder(w).Encode(map[string]any{"data": accepted})
					case strings.HasSuffix(r.URL.Path, "/permit"):
						permitCalls.Add(1)
						http.Error(w, "unexpected permit", http.StatusConflict)
					case strings.HasSuffix(r.URL.Path, "/dispatch"):
						dispatchCalls.Add(1)
						fmt.Fprint(w, `{"data":{"disposition":"submitted","effect_certainty":"confirmed_submitted"}}`)
					default:
						t.Errorf("unexpected path=%s", r.URL.Path)
					}
				}))
				defer server.Close()
				c, err := newTaskBridgeClient(server.URL, "fixture", 10, nil)
				if err != nil {
					t.Fatal(err)
				}
				out, err := c.executeJoin(context.Background(), res)
				if err == nil || !strings.Contains(err.Error(), "payload mismatch") || out.Disposition == "submitted" || dispatchCalls.Load() != 0 || permitCalls.Load() != 0 {
					t.Fatalf("out=%+v err=%v dispatch calls=%d permit calls=%d; want calls=0", out, err, dispatchCalls.Load(), permitCalls.Load())
				}
				t.Log("field mismatch rejected; dispatch calls=0 permit calls=0")
			})
		}
	}
}
