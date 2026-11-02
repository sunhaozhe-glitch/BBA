package main

import (
	"bba.local/taskengine/internal/taskengine"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Real Go handlers + real production napcatGateway HTTP writer + real Koishi
// backend client/transport guard. The only platform is an isolated fake OneBot.
func TestB3CharacterRealGateway(t *testing.T) {
	if os.Getenv("BBA_CHARACTER_INTEGRATION") != "1" {
		t.Skip("run tools/b3-character-check.py for Node + isolated PostgreSQL integration")
	}
	ctx := context.Background()
	dsn := os.Getenv("BBA_FIXTURE_DSN")
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	if os.Getenv("BBA_FIXTURE_ONLY") != "1" || cfg.Host != "/fixture" || cfg.Database != "bba_fixture_admin" || cfg.User != "bba_fixture" {
		t.Fatal("non-fixture refused")
	}
	admin, e := pgx.ConnectConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	db := fmt.Sprintf("fixture_character_%d", time.Now().UnixNano())
	_, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize())
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if _, e := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	u, _ := url.Parse(dsn)
	u.Path = "/" + db
	s, e := taskengine.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	_, e = s.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES('ws_fixture');
 INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','principal_fixture');
 INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit,heartbeat_at) VALUES('ws_fixture','acc_a','rt_a','adapter_a','10001','verified','online','logged_in','active','clear',true,true,100,clock_timestamp());
 INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority) VALUES('ws_fixture','rt_a','acc_a',1,true);
 INSERT INTO core.chat_send_policy(workspace_id,account_id,enabled,daily_limit) VALUES('ws_fixture','acc_a',true,100);
 INSERT INTO core.group_binding(workspace_id,group_id,active_speaker_account_id,binding_epoch,lease_expires_at) VALUES('ws_fixture','777','acc_a',1,clock_timestamp()+interval '1 hour');
 INSERT INTO core.content_rule(workspace_id,id,version,document) VALUES('ws_fixture','rule_b3',1,'{"name":"B3 local only","kind":"keyword","expression":"BLOCK_B3","case_sensitive":true,"group":"fixture","priority":1,"enabled":true,"deleted":false}'::jsonb)`)
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	mode := "normal"
	writer := 0
	counts := map[string]int{}
	captures := []map[string]any{}
	timeoutRelease := make(chan struct{})
	var timeoutOnce sync.Once
	releaseTimeout := func() { timeoutOnce.Do(func() { close(timeoutRelease) }) }
	onebot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer b3-onebot-only" {
			t.Error("wrong OneBot credential")
			w.WriteHeader(401)
			return
		}
		var p map[string]any
		if e := json.NewDecoder(r.Body).Decode(&p); e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return
		}
		if p["auto_escape"] != true {
			t.Error("not plain text")
		}
		if r.URL.Path != "/send_group_msg" && r.URL.Path != "/send_private_msg" {
			t.Error("unexpected platform route")
		}
		mu.Lock()
		writer++
		n := writer
		captures = append(captures, map[string]any{"action": r.URL.Path, "payload": p})
		mu.Unlock()
		fmt.Fprintf(w, `{"status":"ok","retcode":0,"data":{"message_id":%d}}`, n)
	}))
	defer onebot.Close()
	actor := taskengine.RuntimeActor{WorkspaceID: "ws_fixture", RuntimeID: "rt_a", OwnerEpoch: 1, AccountIDs: []string{"acc_a"}}
	// Separate gateway credential comes from explicit mapping, not issue runtime token.
	tokenDigest := sha256.Sum256([]byte("b3-gateway-only"))
	server, auth, e := newGateway(gatewayConfig{Runtimes: []gatewayRuntimeConfig{{WorkspaceID: "ws_fixture", RuntimeID: "rt_a", AccountID: "acc_a", OwnerEpoch: 1, TokenSHA256: hex.EncodeToString(tokenDigest[:]), OneBotToken: "b3-onebot-only", OneBotURL: onebot.URL, WebUIToken: "fixture-unused", WebUIURL: onebot.URL}}})
	if e != nil {
		t.Fatal(e)
	}
	handler := s.ChatSendGatewayHandler(auth.authenticate, server)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts["gateway "+r.Method+" "+r.URL.Path]++
		m := mode
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			if m == "cap404" || m == "cap405" {
				status := 404
				if m == "cap405" {
					status = 405
				}
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"code":"OLD_SERVICE"}}`)
				return
			}
			if m == "capmissing" {
				fmt.Fprint(w, `{"data":{}}`)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/gateway-send") {
			if m == "send404" || m == "send405" {
				status := 404
				if m == "send405" {
					status = 405
				}
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"code":"OLD_SERVICE"}}`)
				return
			}
			if m == "timeout" {
				select {
				case <-r.Context().Done():
					t.Log("FAULT timeout ended by request cancellation")
				case <-timeoutRelease:
					t.Log("FAULT timeout ended by explicit control barrier")
				case <-time.After(6 * time.Second):
					t.Error("timeout injection barrier not released within bound")
				}
				return
			}
			if m == "drop" {
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, r)
				if rr.Code != 200 {
					t.Errorf("drop before sent: %d %s", rr.Code, rr.Body)
				}
				c, _, e := w.(http.Hijacker).Hijack()
				if e != nil {
					t.Error(e)
				} else {
					c.Close()
				}
				mu.Lock()
				mode = "normal"
				mu.Unlock()
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	defer releaseTimeout()
	issueAuth := func(r *http.Request) (taskengine.RuntimeActor, error) {
		if r.Header.Get("Authorization") != "Bearer b3-issue-only" {
			return taskengine.RuntimeActor{}, fmt.Errorf("unauthenticated")
		}
		return actor, nil
	}
	mux := http.NewServeMux()
	ch := s.ChatRuntimeHandler(issueAuth)
	ph := s.ChatPermitRuntimeHandler(issueAuth)
	mux.Handle("/v1/internal/chat-context", ch)
	mux.Handle("/v1/internal/chat-triggers", ch)
	mux.Handle("/v1/internal/chat-send-permits", ph)
	mux.Handle("/v1/internal/chat-send-permits/", ph)
	issue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts["issue "+r.Method+" "+r.URL.Path]++
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	defer issue.Close()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		m := strings.TrimPrefix(r.URL.Path, "/")
		if m != "counts" {
			mode = m
		}
		if m == "normal" {
			releaseTimeout()
		}
		json.NewEncoder(w).Encode(map[string]any{"writer": writer})
	}))
	defer control.Close()
	root := os.Getenv("CONTRACT_ROOT")
	root = filepath.Dir(root)
	cmd := exec.CommandContext(ctx, os.Getenv("BBA_CHARACTER_NODE"), "--import", "tsx", "tools/b3-gateway-integration.ts", issue.URL, gateway.URL, control.URL)
	cmd.Dir = filepath.Join(root, "koishi/character-runtime")
	out, e := cmd.CombinedOutput()
	t.Logf("Koishi integration output:\n%s", out)
	mu.Lock()
	evidence := map[string]any{"http_counts": counts, "writer": writer, "captures": captures, "issue_origin": issue.URL, "gateway_origin": gateway.URL, "onebot_origin": onebot.URL}
	mu.Unlock()
	b, _ := json.Marshal(evidence)
	t.Logf("HTTP EVIDENCE %s", b)
	rows, qe := s.Pool.Query(ctx, `SELECT send_id,decision,rules_version FROM core.content_review ORDER BY created_at`)
	if qe != nil {
		t.Fatal(qe)
	}
	defer rows.Close()
	for rows.Next() {
		var send, decision string
		var version int64
		if qe = rows.Scan(&send, &decision, &version); qe != nil {
			t.Fatal(qe)
		}
		t.Logf("REVIEW send=%s decision=%s rules_version=%d", send, decision, version)
	}
	if e != nil {
		t.Fatalf("real Koishi integration: %v", e)
	}
}
