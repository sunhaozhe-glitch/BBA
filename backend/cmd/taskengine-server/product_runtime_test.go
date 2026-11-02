package main

import (
 "context"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "os"
 "path/filepath"
 "sync/atomic"
 "testing"
 "time"

 "bba.local/taskengine/internal/taskengine"
)

func TestModelConfigurationRejectsAmbiguityAndExposedSecrets(t *testing.T) {
 if _,err:=loadConfiguredModel("configured",true);err==nil{t.Fatal("fixture and configured model accepted together")}
 dir:=t.TempDir()
 key:=filepath.Join(dir,"key")
 if err:=os.WriteFile(key,[]byte("synthetic_model_key"),0644);err!=nil{t.Fatal(err)}
 cfg:=modelFileConfig{ProviderID:"fixture",ModelID:"m",BaseURL:"https://model.example/v1",APIKeyFile:key,TimeoutSeconds:10,MaxConcurrency:1}
 raw,_:=json.Marshal(cfg);config:=filepath.Join(dir,"provider.json")
 if err:=os.WriteFile(config,raw,0600);err!=nil{t.Fatal(err)}
 if _,err:=loadConfiguredModel(config,false);err==nil{t.Fatal("readable key accepted")}
 if err:=os.Chmod(key,0600);err!=nil{t.Fatal(err)}
 if err:=os.Chmod(config,0644);err!=nil{t.Fatal(err)}
 if _,err:=loadConfiguredModel(config,false);err==nil{t.Fatal("readable provider configuration accepted")}
 if err:=os.Chmod(config,0600);err!=nil{t.Fatal(err)}
 if _,err:=loadConfiguredModel(config,false);err!=nil{t.Fatalf("protected HTTPS provider rejected: %v",err)}
}

type blockingPlanningModel struct{calls atomic.Int32;entered chan struct{};release chan struct{}}
func (p *blockingPlanningModel) Complete(ctx context.Context,_ taskengine.ModelCall)(taskengine.ModelCompletion,error){
 p.calls.Add(1);select{case p.entered<-struct{}{}:default:};select{case <-p.release:return taskengine.ModelCompletion{Content:"finished"},nil;case <-ctx.Done():return taskengine.ModelCompletion{},ctx.Err()}
}

func TestModelConcurrencyWaitHonorsCallerDeadline(t *testing.T){
 inner:=&blockingPlanningModel{entered:make(chan struct{},1),release:make(chan struct{})}
 provider:=&boundedPlanningModel{provider:inner,slots:make(chan struct{},1),timeout:time.Second}
 done:=make(chan error,1)
 go func(){_,err:=provider.Complete(context.Background(),taskengine.ModelCall{});done<-err}()
 <-inner.entered
 ctx,cancel:=context.WithTimeout(context.Background(),20*time.Millisecond);defer cancel()
 if _,err:=provider.Complete(ctx,taskengine.ModelCall{});err==nil{t.Fatal("queued request ignored deadline")}
 if inner.calls.Load()!=1{t.Fatal("provider concurrency cap exceeded")}
 close(inner.release);if err:=<-done;err!=nil{t.Fatal(err)}
}

func TestOperatorRuntimeRejectsRuntimeAndReadlessActor(t *testing.T){
 for _,actor:=range []taskengine.Actor{{WorkspaceID:"w",RuntimeID:"r"},{WorkspaceID:"w",PrincipalID:"p",Permissions:map[string]bool{"task.control":true}}}{
  handler:=operatorRuntimeHandler(nil,func(*http.Request)(taskengine.Actor,error){return actor,nil},runtimeSurfaceConfig{})
  response:=httptest.NewRecorder();handler.ServeHTTP(response,httptest.NewRequest(http.MethodGet,"/v1/operator/runtime",nil))
  if response.Code!=http.StatusForbidden{t.Fatalf("actor received %d",response.Code)}
 }
}
