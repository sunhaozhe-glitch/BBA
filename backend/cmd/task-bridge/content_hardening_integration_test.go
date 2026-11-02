package main

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "net/http"
 "net/http/httptest"
 "net/url"
 "os"
 "sync/atomic"
 "testing"
 "time"

 "bba.local/taskengine/internal/contract"
 "bba.local/taskengine/internal/taskengine"
 "github.com/jackc/pgx/v5"
 "github.com/oapi-codegen/nullable"
)

// Full final gate -> production gateway -> fake HTTP payload. No QQ service.
func TestB3HardJoinFinalHTTP(t *testing.T){
 for _,tc:=range []struct{name,reason,answer,expr,kind string;blocked bool}{
 {"unicode_newline","原始\n e\u0301 ","答案\n\"\\","不存在","keyword",false},
 {"empty_answer","原因","","不存在","keyword",false},
 {"empty_both","","","不存在","keyword",false},
 {"cross_field","违","禁","违禁","keyword",false},
 {"anchor_reason","违禁","safe","^违禁$","pattern",true},
 {"anchor_answer","safe","违禁","^违禁$","pattern",true},
 {"both","违禁","违禁","^违禁$","pattern",true},
 }{t.Run(tc.name,func(t *testing.T){
 ctx,cancel:=context.WithTimeout(context.Background(),15*time.Second);defer cancel()
 dsn:=os.Getenv("BBA_FIXTURE_DSN");cfg,e:=pgx.ParseConfig(dsn);if e!=nil{t.Fatal(e)}
 if os.Getenv("BBA_FIXTURE_ONLY")!="1"||cfg.Host!="/fixture"||cfg.Database!="bba_fixture_admin"||cfg.User!="bba_fixture"{t.Fatal("isolated fixture required")}
 admin,e:=pgx.ConnectConfig(ctx,cfg);if e!=nil{t.Fatal(e)};defer admin.Close(context.Background())
 db:=fmt.Sprintf("fixture_hard_join_%d",time.Now().UnixNano());if _,e=admin.Exec(ctx,"CREATE DATABASE "+pgx.Identifier{db}.Sanitize());e!=nil{t.Fatal(e)}
 defer func(){if _,e:=admin.Exec(context.Background(),"DROP DATABASE "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)");e!=nil{t.Error(e)}}()
 u,_:=url.Parse(dsn);u.Path="/"+db;s,e:=taskengine.Open(ctx,u.String());if e!=nil{t.Fatal(e)};defer s.Close();if e=s.Migrate(ctx);e!=nil{t.Fatal(e)}
 _,e=s.Pool.Exec(ctx,`INSERT INTO core.workspace(id) VALUES('ws_fixture');INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','operator');
 INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit,heartbeat_at) VALUES('ws_fixture','acc_a','rt_a','adapter_a','10001','verified','online','logged_in','active','clear',true,true,10,clock_timestamp());
 INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority) VALUES('ws_fixture','rt_a','acc_a',1,true)`);if e!=nil{t.Fatal(e)}
 _,e=s.Pool.Exec(ctx,`INSERT INTO core.approval(workspace_id,id,principal_id,scope_revision,account_ids,target_ids,max_attempts,expires_at,join_reason,verification_message) VALUES('ws_fixture','approval','operator',1,ARRAY['acc_a'],ARRAY['20001'],10,clock_timestamp()+interval '1 hour',$1,$2)`,tc.reason,tc.answer);if e!=nil{t.Fatal(e)}
 doc,_:=json.Marshal(taskengine.ContentRule{Name:"fixture",Kind:tc.kind,Expression:tc.expr,CaseSensitive:true,Enabled:true})
 if _,e=s.Pool.Exec(ctx,`INSERT INTO core.content_rule(workspace_id,id,version,document) VALUES('ws_fixture','rule_hard',1,$1)`,doc);e!=nil{t.Fatal(e)}
 actor:=taskengine.Actor{WorkspaceID:"ws_fixture",PrincipalID:"operator",Permissions:map[string]bool{"task.create":true,"task.read":true,"task.control":true,"task.approve":true}}
 rt:=taskengine.RuntimeActor{WorkspaceID:"ws_fixture",RuntimeID:"rt_a",OwnerEpoch:1,AccountIDs:[]string{"acc_a"}}
 mode:=contract.ExecutionMode("live")
 req:=contract.CreateTaskRequest{Type:"group.acquire",Title:"isolated hardening",JoinReason:tc.reason,VerificationMessage:tc.answer,Goal:contract.Goal{Metric:"new_unique_groups_for_account_pool",Count:1,StrictTargetCap:true,RequireFinalMembershipCheck:true},AllowedAccountIds:[]string{"acc_a"},CandidateSource:contract.CandidateSource{Kind:"fixed",Groups:[]contract.FixedCandidate{{GroupId:"20001",SourceRef:"fixture"}}},ExecutionMode:&mode,DeadlineAt:time.Now().UTC().Add(time.Hour),Limits:contract.TaskLimits{MaxCandidates:1,MaxAttemptsPerAction:1,MaxTotalExternalAttempts:10},ApprovalRef:nullable.NewNullableWithValue("approval"),Submit:true}
 task,e:=s.CreateTask(ctx,actor,"fixture_create",req);if e!=nil{t.Fatal(e)}
 if _,e=s.ExpandFixedPage(ctx,actor,task.Id,100);e!=nil{t.Fatal(e)}
 if e=s.CompleteBaseline(ctx,actor,task.Id,[]taskengine.BaselineSnapshot{{AccountID:"acc_a",EvidenceRef:"fixture",ObservedAt:time.Now().UTC(),Complete:true,GroupIDs:[]string{}}});e!=nil{t.Fatal(e)}
 task,e=s.GetTask(ctx,actor,task.Id);if e!=nil{t.Fatal(e)}
 if _,e=s.ControlTask(ctx,actor,task.Id,"fixture_start",contract.TaskControlRequest{Action:"start",ExpectedVersion:task.Version,Reason:"fixture"});e!=nil{t.Fatal(e)}
 res,e:=s.Reserve(ctx,"ws_fixture",task.Id,"acc_a");if e!=nil{t.Fatal(e)}
 if _,e=s.AcceptOperation(ctx,rt,res);e!=nil{t.Fatal(e)}
 if _,e=s.IssuePermit(ctx,rt,res.OperationID,10*time.Second);e!=nil{t.Fatal(e)}
 tampered:=res;tampered.VerificationMessage+="changed"
 if _,e=s.AcceptOperation(ctx,rt,tampered);taskengine.Code(e)!="OPERATION_PAYLOAD_CONFLICT"{t.Fatalf("tamper=%v",e)}
 var authCalls,writer atomic.Int32
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
 if r.URL.Path=="/api/auth/login"{authCalls.Add(1);fmt.Fprint(w,`{"code":0,"data":{"Credential":"fixture-only"}}`);return}
 writer.Add(1);if r.URL.Path!="/api/Plugin/ext/napcat-plugin-cleaner/join-group"{t.Error("wrong writer route")}
 var payload struct{Reason string `json:"reason"`;Answer string `json:"answer"`};if e:=json.NewDecoder(r.Body).Decode(&payload);e!=nil{t.Error(e)}
 if payload.Reason!=tc.reason||payload.Answer!=tc.answer{t.Error("fake HTTP payload differs from reviewed raw fields")}
 fmt.Fprint(w,`{"ok":true,"result":1}`)
 }));defer server.Close()
 runtime:=napcatGateway{config:gatewayRuntimeConfig{WorkspaceID:"ws_fixture",RuntimeID:"rt_a",AccountID:"acc_a",OwnerEpoch:1},webUIURL:server.URL,client:newGatewayHTTPClient()}
 gateway:=gatewayServer{runtimes:map[string]napcatGateway{gatewayRuntimeKey("ws_fixture","rt_a"):runtime}}
 _,e=s.Dispatch(ctx,rt,res.OperationID,func(context.Context,taskengine.Reservation)error{return nil},hardHTTPJoinAdapter{actor:rt,gateway:&gateway})
 want:=int32(1);if tc.blocked{want=0;if taskengine.Code(e)!="CONTENT_BLOCKED"{t.Fatal(e)}}else if e!=nil{t.Fatal(e)}
 if writer.Load()!=want||authCalls.Load()!=want{t.Fatalf("writer=%d auth=%d want=%d",writer.Load(),authCalls.Load(),want)}
 // Independently reconstruct the canonical digest from the exact reviewed Reservation.
 b,_:=json.Marshal(res);var v any;_ =json.Unmarshal(b,&v);b,_=json.Marshal(v);sum:=sha256.Sum256(b)
 var dg string;if e=s.Pool.QueryRow(ctx,`SELECT payload_digest FROM core.content_review WHERE send_id=$1`,res.OperationID).Scan(&dg);e!=nil{t.Fatal(e)}
 if dg!=hex.EncodeToString(sum[:]){t.Error("payload digest mismatch")}
 t.Logf("reviewed_fields_equal_final_HTTP=true tamper_rejected=true writer_calls=%d auth_calls=%d",writer.Load(),authCalls.Load())
 })}
}
