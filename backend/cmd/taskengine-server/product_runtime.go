package main

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "net/http"
 "os"
 "strings"
 "time"

 "bba.local/taskengine/internal/taskengine"
)

type modelFileConfig struct {
 ProviderID string `json:"provider_id"`
 ModelID string `json:"model_id"`
 BaseURL string `json:"base_url"`
 APIKeyFile string `json:"api_key_file"`
 ApprovedPrivateHTTPBaseURL string `json:"approved_private_http_base_url"`
 TimeoutSeconds int `json:"timeout_seconds"`
 MaxConcurrency int `json:"max_concurrency"`
}

type configuredModel struct {
 Provider taskengine.ModelProvider
 ProviderID string
 ModelID string
 Fixture bool
 TimeoutSeconds int
 MaxConcurrency int
}

func readProtectedJSON(path string, target any) error {
 info, err := os.Lstat(path)
 if err != nil { return fmt.Errorf("read protected configuration: %w", err) }
 if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
  return errors.New("configuration must be regular, mode 0600 or stricter and at most 64KiB")
 }
 file, err := os.Open(path)
 if err != nil { return err }
 defer file.Close()
 decoder := json.NewDecoder(io.LimitReader(file,64<<10+1))
 decoder.DisallowUnknownFields()
 if err := decoder.Decode(target); err != nil { return errors.New("invalid protected configuration") }
 if decoder.Decode(new(any)) != io.EOF { return errors.New("configuration must contain one JSON object") }
 return nil
}

func loadConfiguredModel(path string, fixture bool) (configuredModel,error) {
 if path != "" && fixture { return configuredModel{},errors.New("model provider file and fixture mode are mutually exclusive") }
 if fixture { return configuredModel{Provider:fixturePlanningModel{},ProviderID:"fixture-openai",ModelID:"planning-fixture",Fixture:true,TimeoutSeconds:10,MaxConcurrency:1},nil }
 if path == "" { return configuredModel{},nil }
 var cfg modelFileConfig
 if err:=readProtectedJSON(path,&cfg); err!=nil { return configuredModel{},err }
 if cfg.TimeoutSeconds<1 || cfg.TimeoutSeconds>120 || cfg.MaxConcurrency<1 || cfg.MaxConcurrency>32 { return configuredModel{},errors.New("model timeout must be 1..120 seconds and concurrency 1..32") }
 info,err:=os.Lstat(cfg.APIKeyFile)
 if err!=nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077!=0 || info.Size()>4096 { return configuredModel{},errors.New("model API key file must be protected, regular and bounded") }
 raw,err:=os.ReadFile(cfg.APIKeyFile); if err!=nil { return configuredModel{},errors.New("cannot read model API key file") }
 key:=strings.TrimSpace(string(raw)); if key=="" || strings.ContainsAny(key,"\r\n") { return configuredModel{},errors.New("model API key must be one non-empty line") }
 timeout:=time.Duration(cfg.TimeoutSeconds)*time.Second
 provider,err:=taskengine.NewOpenAICompatibleProvider(taskengine.OpenAICompatibleProviderConfig{BaseURL:cfg.BaseURL,ModelID:cfg.ModelID,ProviderID:cfg.ProviderID,APIKey:key,ApprovedPrivateHTTPBaseURL:cfg.ApprovedPrivateHTTPBaseURL},&http.Client{Timeout:timeout})
 if err!=nil { return configuredModel{},err }
 return configuredModel{Provider:&boundedPlanningModel{provider:provider,slots:make(chan struct{},cfg.MaxConcurrency),timeout:timeout},ProviderID:cfg.ProviderID,ModelID:cfg.ModelID,TimeoutSeconds:cfg.TimeoutSeconds,MaxConcurrency:cfg.MaxConcurrency},nil
}

type boundedPlanningModel struct { provider taskengine.ModelProvider; slots chan struct{}; timeout time.Duration }
func (p *boundedPlanningModel) Complete(ctx context.Context,call taskengine.ModelCall)(taskengine.ModelCompletion,error) {
 ctx,cancel:=context.WithTimeout(ctx,p.timeout); defer cancel()
 select { case p.slots<-struct{}{}: defer func(){<-p.slots}(); case <-ctx.Done(): return taskengine.ModelCompletion{},ctx.Err() }
 return p.provider.Complete(ctx,call)
}

type fixturePlanningModel struct{}
func (fixturePlanningModel) Complete(ctx context.Context,call taskengine.ModelCall)(taskengine.ModelCompletion,error) {
 if err:=ctx.Err();err!=nil{return taskengine.ModelCompletion{},err}
 body,err:=json.Marshal(map[string]any{"schema_id":"PlanProposal.v1","task_id":"task_ref","expected_task_version":call.Version,"scope_revision":call.ScopeRevision,"proposal_kind":"request_clarification","steps":[]any{},"questions":[]string{"Please confirm the operator-approved scope before requesting executable work."},"rationale_summary":"Fixture model exercises the bounded planning gateway; it grants no authority."})
 if err!=nil{return taskengine.ModelCompletion{},err}
 return taskengine.ModelCompletion{Content:string(body),ProviderID:"fixture-openai",ModelID:"planning-fixture",InputTokens:24,OutputTokens:48,Latency:time.Millisecond},nil
}

type runtimeSurfaceConfig struct {
 Environment string
 DevelopmentOperations bool
 ProposalExecution bool
 Login bool
 WorkerEnabled bool
 WorkerID string
 WorkerPoll time.Duration
 WorkerBatch int
 Model configuredModel
}

func operatorRuntimeHandler(store *taskengine.Store,auth func(*http.Request)(taskengine.Actor,error),cfg runtimeSurfaceConfig) http.Handler {
 return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  w.Header().Set("Content-Type","application/json")
  fail:=func(status int,code string){w.WriteHeader(status);_ = json.NewEncoder(w).Encode(map[string]any{"error":map[string]any{"code":code,"message":code,"retryable":false}})}
  if r.URL.Path!="/v1/operator/runtime" {fail(404,"RESOURCE_NOT_FOUND");return}
  if r.Method!=http.MethodGet{w.Header().Set("Allow","GET");fail(405,"METHOD_NOT_ALLOWED");return}
  actor,err:=auth(r);if err!=nil{fail(401,"UNAUTHENTICATED");return}
  if actor.RuntimeID!="" || !actor.Permissions["operator.read"] {fail(403,"ACTION_FORBIDDEN");return}
  var schema int
  if err:=store.Pool.QueryRow(r.Context(),`SELECT max(version) FROM core.schema_version`).Scan(&schema);err!=nil{fail(503,"STATE_STORE_UNAVAILABLE");return}
  var last any
  rows,err:=store.Pool.Query(r.Context(),`SELECT id,state,COALESCE(error_code,''),created_at FROM core.model_request WHERE workspace_id=$1 AND principal_id=$2 ORDER BY created_at DESC,id DESC LIMIT 1`,actor.WorkspaceID,actor.PrincipalID)
  if err!=nil{fail(503,"STATE_STORE_UNAVAILABLE");return}
  for rows.Next(){var id,state,code string;var created time.Time;if rows.Scan(&id,&state,&code,&created)!=nil{rows.Close();fail(503,"STATE_STORE_UNAVAILABLE");return};if state=="completed"{state="succeeded"};if state=="started"{state="unknown"};last=map[string]any{"id":id,"state":state,"code":code,"created_at":created}}
  rows.Close();if rows.Err()!=nil{fail(503,"STATE_STORE_UNAVAILABLE");return}
  sum:=sha256.Sum256([]byte(actor.WorkspaceID+"\x00"+actor.PrincipalID+"\x00"+time.Now().UTC().Format(time.RFC3339Nano)))
  _=json.NewEncoder(w).Encode(map[string]any{"data":map[string]any{"schema_version":schema,"environment":cfg.Environment,"development_operations":cfg.DevelopmentOperations,"worker":map[string]any{"enabled":cfg.WorkerEnabled,"id":cfg.WorkerID,"poll_ms":cfg.WorkerPoll.Milliseconds(),"batch":cfg.WorkerBatch,"transport":"fixture"},"flags":map[string]bool{"proposal_execution":cfg.ProposalExecution,"login":cfg.Login,"model":cfg.Model.Provider!=nil},"model":map[string]any{"configured":cfg.Model.Provider!=nil,"enabled":cfg.Model.Provider!=nil,"provider_id":cfg.Model.ProviderID,"model_id":cfg.Model.ModelID,"prompt_version":"planning.v1","last_request":last,"max_concurrency":cfg.Model.MaxConcurrency,"timeout_seconds":cfg.Model.TimeoutSeconds},"limits":map[string]int{"max_candidates":5000,"max_total_external_attempts":10000}},"meta":map[string]any{"request_id":"runtime_"+hex.EncodeToString(sum[:8]),"server_time":time.Now().UTC()}})
 })
}
