#!/usr/bin/env python3
from pathlib import Path
import datetime,hashlib,http.server,json,os,secrets,signal,socket,ssl,subprocess,sys,threading,time,urllib.request,urllib.error
ROOT=Path(__file__).resolve().parents[1]
out=Path(sys.argv[1]).resolve();out.mkdir(parents=True,exist_ok=True)
assert out.is_relative_to(ROOT/'evals/results')
assert not os.environ.get('BBA_TASKENGINE_DSN'),'refuse inherited DSN'
def port():
 with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
name='sol-artex-b2-'+secrets.token_hex(3);hp,dp,pp=port(),port(),port();state=ROOT/'build/dev-stack'/name
env={**os.environ,'BBA_DEV_STACK_NAME':name,'BBA_DEV_STACK_DB_PORT':str(dp),'BBA_DEV_STACK_HTTP_PORT':str(hp),'BBA_DEV_STACK_WORKER_POLL':'60s'}
exits=[]
def run(label,cmd,**kw):
 with (out/(label+'.log')).open('w') as log:p=subprocess.run(cmd,cwd=ROOT,env=env,stdout=log,stderr=subprocess.STDOUT,**kw)
 exits.append({'label':label,'command':cmd,'exit_code':p.returncode});(out/'integration-exits.json').write_text(json.dumps(exits,indent=2));p.check_returncode()
def request(path,body=None,method='GET',token=None,key=None):
 headers={'Authorization':'Bearer '+(token or (state/'console-token').read_text().strip())}
 if body is not None:headers['Content-Type']='application/json'
 if key:headers['Idempotency-Key']=key
 req=urllib.request.Request('http://127.0.0.1:'+str(hp)+path,data=json.dumps(body).encode() if body is not None else None,method=method,headers=headers)
 try:
  with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req,timeout=12) as res:return res.status,json.load(res)
 except urllib.error.HTTPError as e:return e.code,json.load(e)
provider_calls=[]
class Provider(http.server.BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_POST(self):
  raw=json.loads(self.rfile.read(int(self.headers['Content-Length'])));assert self.path=='/v1/chat/completions';assert self.headers.get('Authorization')=='Bearer fixture-secret';assert 'fixture-secret' not in json.dumps(raw)
  provider_calls.append({'model':raw['model'],'path':self.path,'secret_in_prompt':False,'tls_version':self.connection.version(),'messages':raw['messages']});(out/'fixture-provider-calls.json').write_text(json.dumps(provider_calls,indent=2))
  proposal={'schema_id':'PlanProposal.v1','task_id':'task_ref','expected_task_version':1,'scope_revision':1,'proposal_kind':'request_clarification','steps':[],'questions':['Please provide approved application text.'],'rationale_summary':'Local fixture does not grant authority.'}
  payload=json.dumps({'model':raw['model'],'choices':[{'finish_reason':'stop','message':{'content':json.dumps(proposal)}}],'usage':{'prompt_tokens':3,'completion_tokens':4}}).encode();self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(payload)));self.end_headers();self.wfile.write(payload)
server=None;process=None;started=False
try:
 run('build-product',['python3','tools/build-product.py']);started=True;run('fixture-start',['bash','tools/dev-stack.sh','start'])
 pid=int((state/'server.pid').read_text());assert str(state/'bin/taskengine-server').encode() in Path('/proc/'+str(pid)+'/cmdline').read_bytes();os.kill(pid,signal.SIGTERM)
 for _ in range(100):
  try:os.kill(pid,0)
  except ProcessLookupError:break
  time.sleep(.05)
 resolver=json.loads((state/'resolver.json').read_text());resolver['credentials'][0]['permissions']+=['content.read','content.configure','content.review','model.request','model.configure','agent.configure','skill.configure'];reader=secrets.token_hex(24);(state/'reader-token').write_text(reader);os.chmod(state/'reader-token',0o600);resolver['credentials'].append({'token_sha256':hashlib.sha256(reader.encode()).hexdigest(),'workspace_id':'dev-stack-'+name,'principal_id':'dev-operator','permissions':['operator.read','task.read']});(state/'resolver.json').write_text(json.dumps(resolver))
 outsider=secrets.token_hex(24);(state/'outsider-token').write_text(outsider);os.chmod(state/'outsider-token',0o600)
 resolver['credentials'].append({'token_sha256':hashlib.sha256(outsider.encode()).hexdigest(),'workspace_id':'other-'+name,'principal_id':'other-operator','permissions':['operator.read','model.configure','agent.configure','skill.configure','model.request']});(state/'resolver.json').write_text(json.dumps(resolver))
 # Resolver credentials do not create database principals. Seed only this own
 # disposable fixture so the second workspace is authorized, yet isolated.
 run('fixture-other-principal',['docker','exec','bba-dev-stack-'+name+'-db','psql','-U','bba_dev','-d','bba_dev','-v','ON_ERROR_STOP=1','-c',"INSERT INTO core.workspace(id) VALUES('other-"+name+"'); INSERT INTO core.principal(workspace_id,id) VALUES('other-"+name+"','other-operator');"])
 secure=state/'model-private';secure.mkdir(mode=0o700);os.chmod(secure,0o700)
 run('fixture-certificate',['openssl','req','-x509','-newkey','rsa:2048','-nodes','-keyout',str(secure/'key.pem'),'-out',str(secure/'cert.pem'),'-days','1','-subj','/CN=127.0.0.1','-addext','subjectAltName=IP:127.0.0.1']);os.chmod(secure/'key.pem',0o600)
 server=http.server.ThreadingHTTPServer(('127.0.0.1',pp),Provider);tls=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);tls.load_cert_chain(secure/'cert.pem',secure/'key.pem');server.socket=tls.wrap_socket(server.socket,server_side=True);threading.Thread(target=server.serve_forever,daemon=True).start()
 cfg={'name':name,'http_port':hp,'db_port':dp,'provider_url':'https://127.0.0.1:'+str(pp)+'/v1'};(out/'fixture-config.json').write_text(json.dumps(cfg,indent=2))
 def start():
  e={**env,'BBA_TASKENGINE_DSN':(state/'dsn').read_text().strip(),'SSL_CERT_FILE':str(secure/'cert.pem')};log=(out/'fixture-server.log').open('a');p=subprocess.Popen([str(state/'bin/taskengine-server'),'-listen','127.0.0.1:'+str(hp),'-origin','http://127.0.0.1:'+str(hp),'-resolver-file',str(state/'resolver.json'),'-development-operations','-model-registry-file',str(secure/'registry.json')],env=e,stdout=log,stderr=subprocess.STDOUT);(state/'server.pid').write_text(str(p.pid))
  for _ in range(100):
   if p.poll() is not None:raise RuntimeError('server exited '+str(p.returncode))
   try:
    if request('/v1/operator/models')[0]==200:return p
   except (urllib.error.URLError,ConnectionError):pass
   time.sleep(.1)
  raise RuntimeError('server startup timeout')
 process=start()
 examples=json.loads((ROOT/'contracts/examples/tasks.json').read_text());task=next(x['value'] for x in examples if x.get('schema_ref')=='tasks.json#/CreateTaskRequest' and x.get('valid') and 'candidate_source' in x.get('value',{}));task=json.loads(json.dumps(task));task.update(title='隔离B1任务',allowed_account_ids=['dev-account-'+name],submit=False,deadline_at=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(hours=1)).isoformat().replace('+00:00','Z'));task['goal']['count']=1
 status,data=request('/v1/tasks',task,'POST',key='b1-create');assert status in (200,201,202),(status,data);t=data['data'];(out/'fixture-task-create.json').write_text(json.dumps(data,ensure_ascii=False,indent=2))
 exec(compile((ROOT/'tools/b2-integration-checks.py').read_text(),str(ROOT/'tools/b2-integration-checks.py'),'exec'))

finally:
 cleanup_errors=[]
 if process is not None and process.poll() is None:
  try:process.terminate();process.wait(timeout=15)
  except Exception as e:cleanup_errors.append('server: '+str(e))
 if server is not None:
  try:server.shutdown();server.server_close()
  except Exception as e:cleanup_errors.append('provider: '+str(e))
 if started:
  for label,cmd in [('fixture-stop',['bash','tools/dev-stack.sh','stop']),('fixture-remove-container',['docker','rm','bba-dev-stack-'+name+'-db']),('fixture-remove-volume',['docker','volume','rm','bba-dev-stack-'+name+'-pgdata'])]:
   try:run(label,cmd)
   except Exception as e:cleanup_errors.append(label+': '+str(e))
  probes={}
  for kind,target in [('container','bba-dev-stack-'+name+'-db'),('volume','bba-dev-stack-'+name+'-pgdata')]:
   cmd=['docker']+(['inspect'] if kind=='container' else ['volume','inspect'])+[target]
   probe=subprocess.run(cmd,capture_output=True,text=True)
   probes[kind]={'command':cmd,'exit_code':probe.returncode,'stdout':probe.stdout,'stderr':probe.stderr}
  (out/'cleanup-probes.json').write_text(json.dumps(probes,indent=2))
  ports={}
  for label,pn in [('http',hp),('database',dp),('provider',pp)]:
   with socket.socket() as sock:ports[label]=sock.connect_ex(('127.0.0.1',pn))!=0
  checks={'container_absent':probes['container']['exit_code']!=0 and 'no such' in probes['container']['stderr'].lower(),'volume_absent':probes['volume']['exit_code']!=0 and 'no such' in probes['volume']['stderr'].lower(),'own_server_stopped':process is None or process.poll() is not None,'own_ports_closed':all(ports.values())}
  (out/'cleanup-verification.json').write_text(json.dumps({**checks,'ports_closed':ports,'errors':cleanup_errors},indent=2))
  if cleanup_errors or not all(checks.values()):raise RuntimeError('fixture cleanup failed: '+str(checks)+str(cleanup_errors))
  acceptance=out/'acceptance-checks.json'
  if acceptance.exists():
   summary=json.loads(acceptance.read_text());summary['cleanup_verified']=True
   (out/'integration-summary.json').write_text(json.dumps(summary,indent=2))
