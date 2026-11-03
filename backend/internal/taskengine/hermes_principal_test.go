package taskengine

import (
 "context"
 "net/http"
 "testing"
)

func TestHermesPrincipalRequestIdentity(t *testing.T) {
 for _,draft:=range []bool{false,true} {
  t.Run(map[bool]string{false:"agent",true:"draft"}[draft],func(t *testing.T){
   s:=fixtureStore(t);a:=conversationActor()
   _,err:=s.Pool.Exec(context.Background(),`INSERT INTO core.principal(workspace_id,id,permissions) VALUES('ws_fixture','second_operator','{}')`);if err!=nil{t.Fatal(err)}
   path:="/v1/operator/agent-threads/principal_scope/messages";body:=`{"body":"synthetic"}`
   if draft {seedAccount(t,s,"principal_account","principal_runtime",1);path="/v1/operator/qq-drafts";body=`{"account_id":"principal_account","conversation_kind":"private","conversation_id":"principal_chat","body":"synthetic","expected_version":1}`}
   h:=s.OperatorConversationHandler(func(*http.Request)(Actor,error){return a,nil})
   first:=conversationRequest(h,http.MethodPost,path,body,a);if first.Code!=201{t.Fatalf("first=%d %s",first.Code,first.Body.String())}
   a.PrincipalID="second_operator"
   second:=conversationRequest(h,http.MethodPost,path,body,a);if second.Code!=201{t.Fatalf("different principal same key=%d %s",second.Code,second.Body.String())}
   if first.Body.String()==second.Body.String(){t.Fatal("distinct principals collapsed")}
   replay:=conversationRequest(h,http.MethodPost,path,body,a);if replay.Code!=201||replay.Body.String()!=second.Body.String(){t.Fatal("second principal replay differs")}
   if n:=scalarInt(t,s,`SELECT count(*) FROM core.operator_message`);n!=2{t.Fatalf("messages=%d",n)}
   if n:=scalarInt(t,s,`SELECT count(*) FROM core.operator_notification`);n!=2{t.Fatalf("notifications=%d",n)}
   if n:=scalarInt(t,s,`SELECT count(*) FROM core.chat_send_permit`);n!=0{t.Fatalf("permits=%d",n)}
  })
 }
}
