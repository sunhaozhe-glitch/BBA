// b3-integration-seed only prepares synthetic local integration reservations.
// It cannot issue a send permit, dispatch or load any provider credential.
package main
import(
 "context";"encoding/json";"fmt";"net/url";"os";"time"
 "bba.local/taskengine/internal/contract"
 "bba.local/taskengine/internal/taskengine"
 "github.com/oapi-codegen/nullable"
)
func must(e error){if e!=nil{panic(e)}}
func main(){
 ctx:=context.Background();u,e:=url.Parse(os.Getenv("BBA_TASKENGINE_DSN"));must(e)
 if os.Getenv("BBA_FIXTURE_ONLY")!="1"||u.Hostname()!="127.0.0.1"||u.Port()==""||u.Port()=="5432"||u.Path!="/bba_fixture"||u.User.Username()!="bba_fixture"||len(os.Args)!=2{panic("explicit own fixture only")}
 mode:=os.Args[1];if mode!="blocked"&&mode!="allowed"&&mode!="health"{panic("blocked, allowed or health required")}
 s,e:=taskengine.Open(ctx,u.String());must(e);defer s.Close()
 account:="acc_a";if mode=="blocked"{account="acc_b"};id:=fmt.Sprintf("b3_seed_%d",time.Now().UnixNano());reason:="allowed integration reason";group:="88001";if mode=="blocked"{reason="BLOCK_B3";group="88002"};if mode=="health"{account="acc_c";group="88003"}
 _,e=s.Pool.Exec(ctx,`INSERT INTO core.approval(workspace_id,id,principal_id,scope_revision,account_ids,target_ids,max_attempts,expires_at,join_reason,verification_message) VALUES('ws_fixture',$1,'principal_fixture',1,ARRAY[$4::text],ARRAY[$2],10,clock_timestamp()+interval '1 hour',$3,'integration answer')`,id,group,reason,account);must(e)
 a:=taskengine.Actor{WorkspaceID:"ws_fixture",PrincipalID:"principal_fixture",Permissions:map[string]bool{"task.create":true,"task.read":true,"task.control":true,"task.approve":true}}
 live:=contract.ExecutionMode("live");req:=contract.CreateTaskRequest{Type:"group.acquire",Title:"B3 local integration",JoinReason:reason,VerificationMessage:"integration answer",Goal:contract.Goal{Metric:"new_unique_groups_for_account_pool",Count:1,StrictTargetCap:true,RequireFinalMembershipCheck:true},AllowedAccountIds:[]string{account},CandidateSource:contract.CandidateSource{Kind:"fixed",Groups:[]contract.FixedCandidate{{GroupId:group,SourceRef:"isolated"}}},ExecutionMode:&live,DeadlineAt:time.Now().UTC().Add(time.Hour),Limits:contract.TaskLimits{MaxCandidates:1,MaxAttemptsPerAction:1,MaxTotalExternalAttempts:1,MaxModelAttempts:0},ApprovalRef:nullable.NewNullableWithValue(id),Submit:true}
 task,e:=s.CreateTask(ctx,a,id,req);must(e);_,e=s.ExpandFixedPage(ctx,a,task.Id,100);must(e);must(s.CompleteBaseline(ctx,a,task.Id,[]taskengine.BaselineSnapshot{{AccountID:account,EvidenceRef:"synthetic complete baseline",ObservedAt:time.Now().UTC(),Complete:true,GroupIDs:[]string{}}}))
 task,e=s.GetTask(ctx,a,task.Id);must(e);_,e=s.ControlTask(ctx,a,task.Id,id+"start",contract.TaskControlRequest{Action:"start",ExpectedVersion:task.Version,Reason:"isolated integration"});must(e)
 res,e:=s.Reserve(ctx,"ws_fixture",task.Id,account);must(e);must(json.NewEncoder(os.Stdout).Encode(res))
}
