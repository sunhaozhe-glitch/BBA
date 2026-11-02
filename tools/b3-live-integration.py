#!/usr/bin/env python3
"""Own PostgreSQL, current product/server + task-bridge, browser and default package.
No production DSN accepted; no fixture responses in browser; exact cleanup set.
"""
from pathlib import Path
import hashlib,http.server,json,os,secrets,socket,subprocess,sys,threading,time,urllib.request,urllib.error
ROOT=Path(__file__).resolve().parents[1]
NODE='/home/sun/.hermes/tools/node-26.12.1-linux-x64/bin'

def main():
 assert not os.environ.get('BBA_TASKENGINE_DSN'),'inherited DSN refused'
 out=Path(sys.argv[1]).resolve();assert out.is_relative_to(ROOT/'evals/results');out.mkdir(parents=True,exist_ok=False)
 private=out/'private';private.mkdir(mode=0o700);os.chmod(private,0o700)
 name='sol-b3-live-'+secrets.token_hex(5);container=name+'-db';volume=name+'-data'
 def port():
  with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
 dp,hp,gp,wp,wp_b,wp_c=port(),port(),port(),port(),port(),port();origin='http://127.0.0.1:'+str(hp);gateway='http://127.0.0.1:'+str(gp)
 env={**os.environ,'PATH':NODE+':'+os.environ['PATH']};exits=[];processes=[];created=[];writer_calls=[];identity_calls=[];controls={};lock=threading.Lock();server=None;server_b=None;server_c=None
 def run(label,cmd,**kwargs):
  print('RUN',label,flush=True)
  with (out/(label+'.log')).open('w') as log:p=subprocess.run(cmd,cwd=ROOT,env=env,stdout=log,stderr=subprocess.STDOUT,**kwargs)
  exits.append({'label':label,'command':cmd,'exit_code':p.returncode});(out/'exits.json').write_text(json.dumps(exits,indent=2));p.check_returncode();return p
 def sql(query):
  p=subprocess.run(['docker','exec','-i',container,'psql','-U','bba_fixture','-d','bba_fixture','-At','-v','ON_ERROR_STOP=1'],input=query,text=True,capture_output=True)
  with (out/'sql-commands.jsonl').open('a') as f:f.write(json.dumps({'command':p.args,'query':query,'exit_code':p.returncode,'stdout':p.stdout,'stderr':p.stderr})+'\n')
  if p.returncode:raise RuntimeError(p.stderr)
  return p.stdout.strip()
 def request(path,token,body=None,method='GET'):
  req=urllib.request.Request(origin+path,headers={'Authorization':'Bearer '+token,**({'Content-Type':'application/json'} if body else {})},data=json.dumps(body).encode() if body else None,method=method)
  try:
   with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req,timeout=10) as r:return r.status,json.load(r)
  except urllib.error.HTTPError as e:return e.code,json.load(e)
 def protected(path,data):path.write_text(json.dumps(data));os.chmod(path,0o600)
 try:
  # Build once, ordinary user, no network. These exact artifacts serve the browser.
  run('embed',['python3','tools/build-product.py'])
  run('go-build',['docker','run','--rm','--network','none','--user',str(os.getuid())+':'+str(os.getgid()),'-e','GOPATH=/work/.cache/gopath','-e','GOMODCACHE=/work/.cache/gomod','-e','GOCACHE=/work/.cache/gobuild','-e','GOPROXY=off','-e','GOSUMDB=off','-v',str(ROOT)+':/work','-w','/work/backend','golang:1.26','sh','-c','set -e; go build -o /work/'+str(out.relative_to(ROOT))+'/private/taskengine-server ./cmd/taskengine-server; go build -o /work/'+str(out.relative_to(ROOT))+'/private/task-bridge ./cmd/task-bridge; go build -o /work/'+str(out.relative_to(ROOT))+'/private/migrate ./cmd/taskengine-migrate; go build -o /work/'+str(out.relative_to(ROOT))+'/private/seed ./cmd/b3-integration-seed'])
  run('volume',['docker','volume','create',volume]);created.append(('volume',volume));(out/'resources.json').write_text(json.dumps(created))
  run('postgres',['docker','run','-d','--name',container,'--network','bridge','-e','POSTGRES_USER=bba_fixture','-e','POSTGRES_DB=bba_fixture','-e','POSTGRES_HOST_AUTH_METHOD=trust','-p','127.0.0.1:'+str(dp)+':5432','-v',volume+':/var/lib/postgresql/data','postgres:16-bookworm']);created.append(('container',container));(out/'resources.json').write_text(json.dumps(created))
  for _ in range(150):
   p=subprocess.run(['docker','exec',container,'psql','-U','bba_fixture','-d','bba_fixture','-Atc','SELECT 1'],capture_output=True)
   if p.returncode==0:break
   time.sleep(.2)
  else:raise RuntimeError('PostgreSQL not ready')
  env['BBA_TASKENGINE_DSN']='postgres://bba_fixture@127.0.0.1:'+str(dp)+'/bba_fixture?sslmode=disable'
  # Old SQL sequence, not a downgrade. Record schema21 before public upgrade.
  for file in sorted((ROOT/'backend/internal/taskengine/migrations').glob('*.sql')):
   if file.name.startswith('0013_'):break
   sql(file.read_text())
  assert sql('SELECT max(version) FROM core.schema_version')=='21'
  sql("INSERT INTO core.workspace(id) VALUES('ws_fixture'),('ws_other');INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','principal_fixture'),('ws_other','principal_other');INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit,heartbeat_at) VALUES('ws_fixture','acc_a','rt_a','adapter_a','10001','verified','online','logged_in','active','clear',true,true,100,clock_timestamp());INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority) VALUES('ws_fixture','rt_a','acc_a',1,true);INSERT INTO core.chat_send_policy(workspace_id,account_id,enabled,daily_limit) VALUES('ws_fixture','acc_a',true,100);INSERT INTO core.group_binding(workspace_id,group_id,active_speaker_account_id,binding_epoch,lease_expires_at) VALUES('ws_fixture','777','acc_a',1,clock_timestamp()+interval '1 hour')")
  sql("INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit,heartbeat_at) VALUES('ws_fixture','acc_b','rt_b','adapter_b','10002','verified','online','logged_in','active','clear',true,true,100,clock_timestamp());INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority) VALUES('ws_fixture','rt_b','acc_b',1,true)")
  sql("INSERT INTO core.chat_send_policy(workspace_id,account_id,enabled,daily_limit) VALUES('ws_fixture','acc_b',true,100);INSERT INTO core.group_binding(workspace_id,group_id,active_speaker_account_id,binding_epoch,lease_expires_at) VALUES('ws_fixture','778','acc_b',1,clock_timestamp()+interval '1 hour')")
  sql("INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit,heartbeat_at) VALUES('ws_fixture','acc_c','rt_c','adapter_c','10003','verified','online','logged_in','active','clear',true,true,100,clock_timestamp());INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority) VALUES('ws_fixture','rt_c','acc_c',1,true)")
  identity_before=sql("SELECT row_to_json(a)::text FROM core.account a")
  run('upgrade21',[''+str(private/'migrate'),'-upgrade','-expected-version','21','-expect-head','24']);run('migrate-idempotent',[str(private/'migrate'),'-upgrade','-expected-version','24','-expect-head','24']);assert sql('SELECT row_to_json(a)::text FROM core.account a')==identity_before
  admin,reader,other=(secrets.token_hex(24) for _ in range(3));perms=['operator.read','task.read','content.read','content.configure','content.review']
  credentials=[{'token_sha256':hashlib.sha256(t.encode()).hexdigest(),'workspace_id':ws,'principal_id':principal,'permissions':p} for t,ws,principal,p in [(admin,'ws_fixture','principal_fixture',perms),(reader,'ws_fixture','principal_fixture',['operator.read','content.read']),(other,'ws_other','principal_other',perms)]]
  credentials.append({'token_sha256':hashlib.sha256(b'b3-issue-only').hexdigest(),'workspace_id':'ws_fixture','runtime_id':'rt_a','owner_epoch':1,'account_ids':['acc_a']});credentials.extend([{'token_sha256':hashlib.sha256(t.encode()).hexdigest(),'workspace_id':ws,'runtime_id':rt,'owner_epoch':1,'account_ids':[a]} for t,ws,rt,a in [('b3-issue-b','ws_fixture','rt_b','acc_b'),('b3-issue-other','ws_other','rt_a','acc_a')]]);protected(private/'resolver.json',{'credentials':credentials})
  class Writer(http.server.BaseHTTPRequestHandler):
   def log_message(self,*args):pass
   def do_GET(self):
    if self.path=='/api/Plugin/List':
     with lock:identity_calls.append({'account_id':{wp:'acc_a',wp_b:'acc_b',wp_c:'acc_c'}[self.server.server_port],'endpoint':self.server.server_port,'action':self.path})
     assert self.headers.get('Authorization')=='local-session';self.send_response(200);self.end_headers();self.wfile.write(b'{"code":0,"data":{"pluginManagerNotFound":false,"plugins":[{"id":"napcat-plugin-cleaner","status":"active"}]}}');return
    assert self.path=='/counts';self.send_response(200);self.end_headers()
    with lock:self.wfile.write(json.dumps({'writer':len(writer_calls)}).encode())
   def do_POST(self):
    account,self_id={wp:('acc_a',10001),wp_b:('acc_b',10002),wp_c:('acc_c',10003)}[self.server.server_port]
    if self.path in ['/get_login_info','/get_status','/get_group_list','/api/auth/login']:
     payload=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
     data={'/get_group_list':{'status':'ok','retcode':0,'data':[]},'/get_login_info':{'status':'ok','retcode':0,'data':{'user_id':self_id}},'/get_status':{'status':'ok','retcode':0,'data':{'online':True,'good':True}},'/api/auth/login':{'code':0,'data':{'Credential':'local-session'}}}[self.path]
     with lock:identity_calls.append({'account_id':account,'self_id':str(self_id),'endpoint':self.server.server_port,'action':self.path})
     self.send_response(200);self.end_headers();self.wfile.write(json.dumps(data).encode());return
    if self.path=='/api/Plugin/ext/napcat-plugin-cleaner/join-group':
     assert self.headers.get('Authorization')=='local-session';payload=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
     assert payload['reason']=='allowed integration reason' and payload['answer']=='integration answer'
     with lock:writer_calls.append({'account_id':account,'self_id':str(self_id),'endpoint':self.server.server_port,'action':self.path,'payload':payload})
     with lock:control=controls.get('join:'+str(payload['group_id']))
     if control:control['entered'].set();assert control['release'].wait(10),'join control timeout'
     try:self.send_response(200);self.end_headers();self.wfile.write(b'{"ok":true,"result":1}')
     except (BrokenPipeError,ConnectionResetError) as e:
      with lock:identity_calls.append({'account_id':account,'action':'join-response-cancelled','error':type(e).__name__})
     return
    assert self.path in ['/send_group_msg','/send_private_msg'];assert self.headers.get('Authorization')=='Bearer local-only-writer';payload=json.loads(self.rfile.read(int(self.headers['Content-Length'])));assert payload['auto_escape'] is True
    with lock:writer_calls.append({'account_id':account,'self_id':str(self_id),'endpoint':self.server.server_port,'action':self.path,'payload':payload});number=len(writer_calls)
    with lock:control=controls.get(payload.get('message',''))
    if control:
     control['entered'].set();assert control['release'].wait(10),'writer control timeout'
     if control.get('drop'):self.close_connection=True;return
    try:
     self.send_response(200);self.end_headers();self.wfile.write(json.dumps({'status':'ok','retcode':0,'data':{'message_id':number}}).encode())
    except (BrokenPipeError,ConnectionResetError) as e:
     with lock:identity_calls.append({'account_id':account,'action':'writer-response-cancelled','error':type(e).__name__})
  server=http.server.ThreadingHTTPServer(('127.0.0.1',wp),Writer);threading.Thread(target=server.serve_forever,daemon=True).start()
  server_b=http.server.ThreadingHTTPServer(('127.0.0.1',wp_b),Writer);threading.Thread(target=server_b.serve_forever,daemon=True).start()
  server_c=http.server.ThreadingHTTPServer(('127.0.0.1',wp_c),Writer);threading.Thread(target=server_c.serve_forever,daemon=True).start()
  writer_c='http://127.0.0.1:'+str(wp_c)
  writer_b='http://127.0.0.1:'+str(wp_b)
  writer='http://127.0.0.1:'+str(wp);protected(private/'gateway.json',{'runtimes':[{'token_sha256':hashlib.sha256(b'b3-gateway-only').hexdigest(),'workspace_id':'ws_fixture','runtime_id':'rt_a','account_id':'acc_a','owner_epoch':1,'onebot_url':writer,'onebot_token':'local-only-writer','webui_url':writer,'webui_token':'unused-fixture'}]})
  gateway_config=json.loads((private/'gateway.json').read_text());entry=dict(gateway_config['runtimes'][0]);entry.update(runtime_id='rt_b',account_id='acc_b',onebot_url=writer_b,webui_url=writer_b,token_sha256=hashlib.sha256(b'b3-gateway-blocked').hexdigest());gateway_config['runtimes'].append(entry);entry_other=dict(gateway_config['runtimes'][0]);entry_other.update(workspace_id='ws_other',token_sha256=hashlib.sha256(b'b3-gateway-other').hexdigest());gateway_config['runtimes'].append(entry_other);entry_c=dict(entry);entry_c.update(runtime_id='rt_c',account_id='acc_c',onebot_url=writer_c,webui_url=writer_c,token_sha256=hashlib.sha256(b'b3-gateway-health').hexdigest());gateway_config['runtimes'].append(entry_c);protected(private/'gateway.json',gateway_config)
  cfg={'origin':origin,'gateway':gateway,'writer':writer,'admin':admin,'reader':reader,'other':other};protected(private/'browser.json',cfg)
  def start(binary,port,flags):
   log=(out/(binary+'.log')).open('a');p=subprocess.Popen([str(private/binary),'-listen','127.0.0.1:'+str(port),'-origin','http://127.0.0.1:'+str(port)]+flags,env=env,stdout=log,stderr=subprocess.STDOUT);processes.append(p)
   for _ in range(100):
    if p.poll() is not None:raise RuntimeError(binary+' exited '+str(p.returncode))
    with socket.socket() as s:
     if s.connect_ex(('127.0.0.1',port))==0:return p
    time.sleep(.1)
   raise RuntimeError('startup timeout')
  product=start('taskengine-server',hp,['-resolver-file',str(private/'resolver.json')]);bridge=start('task-bridge',gp,['-config',str(private/'gateway.json')])
  run('browser-rules',['node','tools/b3-live-browser.cjs',str(private/'browser.json'),'rules',str(out/'browser-rules')])
  assert request('/v1/operator/content-rules',admin)[0]==200;assert sql("SELECT count(*) FROM core.content_rule WHERE document->>'enabled'='true' AND document->>'deleted'='false'")=='1'
  with lock:assert not writer_calls
  rules_before=request('/v1/operator/content-rules',admin)[1]
  product.terminate();product.wait(timeout=15);product=start('taskengine-server',hp,['-resolver-file',str(private/'resolver.json')]);assert request('/v1/operator/content-rules',admin)[1]==rules_before
  run('package-build',['npm','--prefix','koishi/character-runtime','run','build'])
  # subprocess cwd belongs to package for tsx resolution.
  print('RUN online-character',flush=True)
  cmd=['node','--import','tsx','tools/b3-product-online.ts',str(private/'browser.json')]
  with (out/'online-character.log').open('w') as log:p=subprocess.run(cmd,cwd=ROOT/'koishi/character-runtime',env=env,stdout=log,stderr=subprocess.STDOUT)
  exits.append({'label':'online-character','command':cmd,'exit_code':p.returncode});(out/'exits.json').write_text(json.dumps(exits,indent=2));p.check_returncode()
  with lock:assert len(writer_calls)==4
  run('browser-reviews',['node','tools/b3-live-browser.cjs',str(private/'browser.json'),'reviews',str(out/'browser-reviews')])
  with lock:assert len(writer_calls)==4
  assert int(sql('SELECT count(*) FROM core.content_review_note'))==3
  env['BBA_FIXTURE_ONLY']='1'
  bridge_evidence=[]
  def bridge_request(op,action):
   req=urllib.request.Request(gateway+'/v1/internal/bridge/operations/'+op+'/'+action,method='POST',headers={'Authorization':'Bearer '+('b3-gateway-blocked' if mode=='blocked' else 'b3-gateway-only')})
   try:
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req,timeout=15) as res:return res.status,json.load(res)
   except urllib.error.HTTPError as e:return e.code,json.load(e)
  for mode in ['blocked','allowed']:
   run('seed-'+mode,[str(private/'seed'),mode]);res=json.loads((out/('seed-'+mode+'.log')).read_text());op=res['OperationID']
   for action in ['accept','permit']:
    status,reply=bridge_request(op,action);assert status==200,(action,status,reply)
   with lock:before=len(writer_calls)
   status,reply=bridge_request(op,'dispatch')
   with lock:delta=len(writer_calls)-before
   assert delta==(1 if mode=='allowed' else 0),(mode,status,reply,delta)
   if mode=='allowed':assert status==200 and reply['data']['disposition']=='submitted' and reply['data']['effect_certainty']=='confirmed_submitted',reply
   else:assert status==500 and reply['error']['code']=='CONTENT_BLOCKED',(status,reply) # preserve original 500 contract
   replay_status,replay=bridge_request(op,'dispatch')
   with lock:assert len(writer_calls)==before+delta
   taskid=res['TaskID'];assert sql("SELECT count(*) FROM core.execution WHERE disposition='confirmed' AND task_id='"+taskid+"'")=='0'
   bridge_evidence.append({'mode':mode,'status':status,'reply':reply,'writer_delta':delta,'replay_status':replay_status,'replay_writer_delta':0,'confirmed_joined':0})
  (out/'bridge-online.json').write_text(json.dumps(bridge_evidence,indent=2))
  run('seed-health',[str(private/'seed'),'health']);health_res=json.loads((out/'seed-health.log').read_text())
  from b3_online_faults import exercise
  exercise(out=out,origin=origin,gateway=gateway,admin=admin,sql=sql,container=container,writer_calls=writer_calls,lock=lock,controls=controls,health_res=health_res)
  durable_before=sql("SELECT jsonb_agg(to_jsonb(x) ORDER BY id)::text FROM core.content_review_note x")
  product.terminate();product.wait(timeout=15);product=start('taskengine-server',hp,['-resolver-file',str(private/'resolver.json')]);assert sql("SELECT jsonb_agg(to_jsonb(x) ORDER BY id)::text FROM core.content_review_note x")==durable_before
  status,reviews=request('/v1/operator/content-reviews?limit=100',admin);assert status==200;detail=request('/v1/operator/content-reviews/'+reviews['data']['items'][0]['id'],admin);assert detail[0]==200
  (out/'api-review-readback.json').write_text(json.dumps(reviews,indent=2));(out/'writer-captures.json').write_text(json.dumps(writer_calls,indent=2));(out/'database-readback.json').write_text(json.dumps({'schema':sql('SELECT max(version) FROM core.schema_version'),'rules':json.loads(sql('SELECT jsonb_agg(to_jsonb(x))::text FROM core.content_rule x')),'notes':json.loads(durable_before)},indent=2))
  accounts=json.loads(sql("SELECT jsonb_agg(to_jsonb(x) ORDER BY id)::text FROM (SELECT workspace_id,id,runtime_id,adapter_instance_id,self_id FROM core.account) x"))
  assert [(a['id'],a['self_id']) for a in accounts]==[('acc_a','10001'),('acc_b','10002'),('acc_c','10003')],accounts
  for capture in writer_calls:
   assert capture['self_id']=={'acc_a':'10001','acc_b':'10002','acc_c':'10003'}[capture['account_id']]
   if capture['action']=='/send_private_msg':assert (capture['account_id'],capture['payload']['user_id']) in [('acc_a',9001),('acc_b',9002)]
   if capture['action']=='/send_group_msg':assert (capture['account_id'],capture['payload']['group_id']) in [('acc_a',777),('acc_b',778)]
  (out/'account-bindings.json').write_text(json.dumps({'accounts':accounts,'gateway_mappings':[{k:v for k,v in x.items() if k in ['workspace_id','runtime_id','account_id','owner_epoch','onebot_url','webui_url']} for x in gateway_config['runtimes']],'no_cross_account_writer':True},indent=2))
  (out/'summary.json').write_text(json.dumps({'passed':True,'real_product_service':True,'response_substitution':False,'rule_restart':True,'notes_restart':True,'misreport_grants_authority':False,'writer_calls':len(writer_calls),'schema21_upgrade':True,'fixture_only':True,'astra_acceptance':False},indent=2))
 finally:
  errors=[]
  for p in reversed(processes):
   if p.poll() is None:p.terminate();p.wait(timeout=15)
  if server:server.shutdown();server.server_close()
  if server_b:server_b.shutdown();server_b.server_close()
  if server_c:server_c.shutdown();server_c.server_close()
  (out/'identity-captures.json').write_text(json.dumps(identity_calls,indent=2));(out/'writer-captures.json').write_text(json.dumps(writer_calls,indent=2))
  (out/'cleanup-targets.json').write_text(json.dumps(created,indent=2));print('CLEANUP EXACT',created,flush=True)
  for kind,target in reversed(created):
   cmd=['docker','rm','-f',target] if kind=='container' else ['docker','volume','rm',target]
   try:run('cleanup-'+kind,cmd)
   except subprocess.CalledProcessError as e:errors.append(str(e))
  probes=[]
  for kind,target in created:
   p=subprocess.run(['docker',kind,'inspect',target],capture_output=True,text=True)
   expected='Error response from daemon: No such container: '+target if kind=='container' else 'Error response from daemon: get '+target+': no such volume'
   probes.append({'kind':kind,'target':target,'exit_code':p.returncode,'stdout':p.stdout,'stderr':p.stderr,'absent':p.returncode==1 and p.stdout.strip()=='[]' and p.stderr.strip()==expected})
  closed={}
  for label,pn in [('product',hp),('gateway',gp),('database',dp),('writer',wp),('writer_b',wp_b),('writer_c',wp_c)]:
   with socket.socket() as s:closed[label]=s.connect_ex(('127.0.0.1',pn))!=0
  (out/'cleanup-readback.json').write_text(json.dumps({'resources':probes,'processes':[{'pid':p.pid,'exit':p.returncode} for p in processes],'ports_closed':closed,'errors':errors},indent=2))
  if errors or not probes or not all(x['absent'] for x in probes) or not all(closed.values()):raise RuntimeError('cleanup verification failed')

if __name__=='__main__':main()
