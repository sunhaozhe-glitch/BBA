#!/usr/bin/env python3
"""Owned loopback DB + real embedded product + browser + proxy consumers.
No production credentials, QQ writer, provider calls, or response substitution.
"""
from pathlib import Path
import hashlib,json,os,secrets,socket,subprocess,sys,time,urllib.request,urllib.error
ROOT=Path(__file__).resolve().parents[1]

def main():
 assert not os.environ.get('BBA_TASKENGINE_DSN'),'inherited DSN refused'
 raw=Path(sys.argv[1]);assert '..' not in raw.parts,'path traversal refused'
 out=raw.absolute();resolved=out.resolve();root=(ROOT/'evals/results').resolve();assert out==resolved and resolved!=root and resolved.is_relative_to(root),'non-owned output root/symlink refused';out.mkdir(parents=True,exist_ok=False)
 private=out/'private';private.mkdir(mode=0o700)
 name='sol-b5-proxy-'+secrets.token_hex(5);container=name+'-db';volume=name+'-data'
 def port():
  with socket.socket() as sock:sock.bind(('127.0.0.1',0));return sock.getsockname()[1]
 dp,hp,pp,xp=port(),port(),port(),port();origin='http://127.0.0.1:'+str(hp)
 env={**os.environ,'PATH':'/home/sun/.hermes/tools/node-26.12.1-linux-x64/bin:'+os.environ['PATH']};exits=[];created=[];processes=[]
 def run(label,command,**kw):
  print('RUN',label,flush=True)
  with (out/(label+'.log')).open('w') as log:p=subprocess.run(command,cwd=ROOT,env=kw.pop('env',env),stdout=log,stderr=subprocess.STDOUT,**kw)
  exits.append({'label':label,'command':command,'exit_code':p.returncode});(out/'exits.json').write_text(json.dumps(exits,indent=2));p.check_returncode();return p
 def sql(query):
  p=subprocess.run(['docker','exec','-i',container,'psql','-U','bba_fixture','-d','bba_fixture','-At','-v','ON_ERROR_STOP=1'],input=query,text=True,capture_output=True)
  with (out/'sql-commands.jsonl').open('a') as f:f.write(json.dumps({'query':query,'exit_code':p.returncode,'stdout':p.stdout,'stderr':p.stderr})+'\n')
  if p.returncode:raise RuntimeError(p.stderr)
  return p.stdout.strip()
 def req(token):
  q=urllib.request.Request(origin+'/v1/operator/system/work-slots',headers={'Authorization':'Bearer '+token})
  try:
   with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(q,timeout=4) as res:return res.status,json.load(res)
  except urllib.error.HTTPError as e:return e.code,json.load(e)
 def protected(path,data):path.write_text(json.dumps(data));os.chmod(path,0o600)
 server=None;ok=False
 try:
  run('embed',['python3','tools/build-product.py'])
  rel=str(private.relative_to(ROOT))
  run('go-build',['docker','run','--rm','--network','none','--user',str(os.getuid())+':'+str(os.getgid()),'-e','HOME=/scratch','-e','TMPDIR=/scratch','-v',os.environ['TMPDIR']+':/scratch','-e','GOPATH=/work/.cache/gopath','-e','GOMODCACHE=/work/.cache/gomod','-e','GOCACHE=/work/.cache/gobuild','-e','GOPROXY=off','-e','GOSUMDB=off','-v',str(ROOT)+':/work','-w','/work/backend','golang:1.26','sh','-c',f'set -e; go build -o /work/{rel}/taskengine-server ./cmd/taskengine-server; go build -o /work/{rel}/migrate ./cmd/taskengine-migrate; go test -race -c -o /work/{rel}/settings.test ./internal/taskengine'])
  run('volume',['docker','volume','create',volume]);created.append(('volume',volume));(out/'resources.json').write_text(json.dumps(created))
  run('postgres',['docker','run','-d','--name',container,'--network','bridge','-e','POSTGRES_USER=bba_fixture','-e','POSTGRES_DB=bba_fixture','-e','POSTGRES_HOST_AUTH_METHOD=trust','-p','127.0.0.1:'+str(dp)+':5432','-v',volume+':/var/lib/postgresql/data','postgres:16-bookworm']);created.append(('container',container));(out/'resources.json').write_text(json.dumps(created))
  for _ in range(150):
   p=subprocess.run(['docker','exec',container,'psql','-U','bba_fixture','-d','bba_fixture','-Atc','SELECT 1'],capture_output=True)
   if p.returncode==0:break
   time.sleep(.2)
  else:raise RuntimeError('DB not ready')
  env['BBA_TASKENGINE_DSN']='postgres://bba_fixture@127.0.0.1:'+str(dp)+'/bba_fixture?sslmode=disable'
  run('explicit-new-schema24',[str(private/'migrate')]);run('check-schema24',[str(private/'migrate'),'-check','-expected-version','24','-expect-head','24'])
  sql("INSERT INTO core.workspace(id) VALUES('ws_fixture'),('ws_other');INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','principal_fixture'),('ws_fixture','principal_reader'),('ws_other','principal_other')")
  tokens={k:secrets.token_hex(24) for k in ['admin','reader','other','none']};protected(private/'tokens.json',tokens)
  creds=[]
  for k,ws,principal,perms in [('admin','ws_fixture','principal_fixture',['operator.read','task.read','task.manage_all','system.read','system.configure','model.configure','model.request']),('reader','ws_fixture','principal_reader',['operator.read','task.read','system.read']),('other','ws_other','principal_other',['operator.read','system.read','system.configure','model.configure','model.request']),('none','ws_fixture','principal_reader',['operator.read'])]:creds.append({'token_sha256':hashlib.sha256(tokens[k].encode()).hexdigest(),'workspace_id':ws,'principal_id':principal,'permissions':perms})
  protected(private/'resolver.json',{'credentials':creds});(out/'fixture-config.json').write_text(json.dumps({'name':name,'origin':origin,'db_port':dp,'http_port':hp,'provider_port':pp,'proxy_port':xp}))
  def start(fallback):
   log=(out/'server.log').open('a');p=subprocess.Popen([str(private/'taskengine-server'),'-listen','127.0.0.1:'+str(hp),'-origin',origin,'-resolver-file',str(private/'resolver.json'),'-task-work-slots',str(fallback),'-model-registry-file',str(private/'registry.json')],cwd=ROOT,env=env,stdout=log,stderr=subprocess.STDOUT);processes.append(p)
   for _ in range(120):
    if p.poll() is not None:raise RuntimeError('server exited '+str(p.returncode))
    try:
     if req(tokens['admin'])[0]==200:return p
    except (urllib.error.URLError,ConnectionError):pass
    time.sleep(.1)
   raise RuntimeError('server startup timeout')
  def stop(p):
   p.terminate();p.wait(timeout=15);assert p.returncode==0,p.returncode
   with (out/'process-exits.jsonl').open('a') as f:f.write(json.dumps({'pid':p.pid,'exit_code':p.returncode})+'\n')
  def browser(phase):run('browser-'+phase,['node','tools/b5-proxy-browser.cjs',str(out),phase])
  owner=secrets.token_hex(32);protected(private/'fixture-owner.json',{'nonce':owner,'dsn':env['BBA_TASKENGINE_DSN'],'provider_port':str(pp),'proxy_port':str(xp)})
  env['BBA_B5_OWNER']=owner
  server=start(1);browser('save')
  stop(server)
  run('browser-saved-real-consumer',[str(private/'settings.test'),'-test.run','^TestB5BrowserSavedConsumer$','-test.v','-test.timeout=40s'],env={**env,'BBA_FIXTURE_ONLY':'1','BBA_B5_LIVE_DSN':env['BBA_TASKENGINE_DSN'],'BBA_B5_LIVE_OUT':str(out),'BBA_B5_PROVIDER_PORT':str(pp),'BBA_B5_PROXY_PORT':str(xp)})
  server=start(8);browser('verify')
  status,data=req(tokens['admin']);assert status==200
  (out/'final-work-api-readback.json').write_text(json.dumps(data,indent=2));ok=True
 finally:
  errors=[]
  for p in processes:
   if p.poll() is None:
    try:stop(p)
    except Exception as e:errors.append('process '+str(e))
  for kind,target in reversed(created):
   try:run('cleanup-'+kind,['docker','rm','-f',target] if kind=='container' else ['docker','volume','rm',target])
   except Exception as e:errors.append(str(e))
  probes=[]
  for kind,target in created:
   q=subprocess.run(['docker',kind,'inspect',target],capture_output=True,text=True);probes.append({'kind':kind,'target':target,'exit_code':q.returncode,'stdout':q.stdout,'stderr':q.stderr,'absent':q.returncode==1 and 'no such' in q.stderr.lower()})
  ports={}
  for key,pn in [('db',dp),('http',hp),('provider',pp),('proxy',xp)]:
   with socket.socket() as sock:ports[key]=sock.connect_ex(('127.0.0.1',pn))!=0
  clean={'probes':probes,'ports_closed':ports,'processes':[{'pid':p.pid,'exit_code':p.poll()} for p in processes],'errors':errors};(out/'cleanup-readback.json').write_text(json.dumps(clean,indent=2))
  assert not errors and len(probes)==2 and all(x['absent'] for x in probes) and all(ports.values()) and all(p.poll()==0 for p in processes),clean
  (out/'summary.json').write_text(json.dumps({'passed':ok,'fixture_only':True,'real_product_service':True,'response_substitution':False,'browser_saved_proxy_consumed':ok,'astra_acceptance':False,'cleanup_verified':True},indent=2))
if __name__=='__main__':main()
