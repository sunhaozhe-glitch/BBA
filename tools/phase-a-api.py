#!/usr/bin/env python3
from pathlib import Path
import datetime,hashlib,json,subprocess,sys,urllib.request,urllib.error
r=Path(__file__).resolve().parents[1];out=Path(sys.argv[1]);cfg=json.loads((out/'fixture-config.json').read_text());assert cfg['name'].startswith('sol-artex-')
base='http://127.0.0.1:'+str(cfg['http_port']);assert cfg['http_port'] not in [18421,6300,8787,19300]
token=(r/'build/dev-stack'/cfg['name']/'console-token').read_text().strip()
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}));results=[]
def call(path,auth=True,method='GET'):
 headers={'Authorization':'Bearer '+token} if auth else {}
 req=urllib.request.Request(base+path,headers=headers,method=method)
 try:
  response=opener.open(req,timeout=10);status=response.status;raw=response.read()
 except urllib.error.HTTPError as e:status=e.code;raw=e.read()
 try:body=json.loads(raw)
 except ValueError:body=raw.decode()
 results.append(dict(path=path,method=method,authenticated=auth,status=status,response=body))
 return status,body
assert call('/v1/operator/dashboard',False)[0]==401
assert call('/v1/operator/tools',False)[0]==401
assert call('/v1/operator/dashboard',method='POST')[0]==405
assert call('/v1/operator/tools',method='POST')[0]==405
status,tasks=call('/v1/tasks?limit=100');assert status==200 and len(tasks['data']['items'])==1
task=tasks['data']['items'][0];assert task['execution_mode']=='dry_run'
status,detail=call('/v1/tasks/'+task['id']);assert status==200 and detail['data']==task
status,accounts=call('/v1/accounts');assert status==200 and len(accounts['data']['items'])==1
status,initial=call('/v1/operator/dashboard');assert status==200
d=initial['data'];assert d['customers']['value'] is None and d['assets']['value'] is None
assert d['tasks']['total']==len(tasks['data']['items'])
assert d['accounts']['total']==len(accounts['data']['items'])
assert sum(d['accounts']['counts'].values())==d['accounts']['total']
assert (d['tokens']['availability']=='unavailable' and d['tokens']['total'] is None) or d['tokens']['total']==168
# Explicit, local fixture database only. Never accept a caller-provided DSN.
container='bba-dev-stack-'+cfg['name']+'-db'
workspace=accounts['data']['items'][0]['workspace_id']
quote=lambda s: "'"+s.replace("'","''")+"'"
sql=f"INSERT INTO core.model_request(workspace_id,id,principal_id,task_id,idempotency_key,request_digest,operation_kind,schema_id,prompt_version,input_ref,deadline_at,max_provider_attempts,state,response,completed_at) VALUES({quote(workspace)},'phase_a_usage','dev-operator',{quote(task['id'])},'phase-a-usage',repeat('a',64),'complex_plan','PlanProposal.v1','planning.v1',{quote(task['id'])},clock_timestamp()+interval '1 hour',1,'completed','{{\"usage\":{{\"input_tokens\":123,\"output_tokens\":45}}}}'::jsonb,clock_timestamp()) ON CONFLICT DO NOTHING;"
# SQL literal needs JSON double quotes, not shell-escaped quotes.
sql=sql.replace('\\"','"')
p=subprocess.run(['docker','exec','-i',container,'psql','-v','ON_ERROR_STOP=1','-U','bba_dev','-d','bba_dev'],input=sql,capture_output=True,text=True)
(out/'fixture-usage-seed.log').write_text(p.stdout+p.stderr+'\nEXIT_CODE='+str(p.returncode));assert p.returncode==0,p.stderr
status,final=call('/v1/operator/dashboard');assert status==200
assert final['data']['tokens']['total']==168 and final['data']['tokens']['input']==123 and final['data']['tokens']['output']==45
status,tools=call('/v1/operator/tools');assert status==200
ids=[x['id'] for x in tools['entries']];assert tools['entry_count']==len(ids)==180 and tools['unique_count']==len(set(ids))==180
assert all(x in ids for x in ['.ocr_image','.handle_quick_operation','ArkShareGroup','ArkSharePeer'])
assert not tools['duplicates'] and all(not x['enabled'] and x['schema'] is None for x in tools['entries'])
assert all(k not in json.dumps(final).lower() for k in ['api_key','access_token','quicklist','credentials'])
(out/'api-responses.json').write_text(json.dumps(results,ensure_ascii=False,indent=2))
(out/'api-assertions.json').write_text(json.dumps({'exit_code':0,'request_count':len(results),'fixture_task':task['id'],'fixture_token_total':168,'registry_count':len(ids),'real_qq_writes':False},ensure_ascii=False,indent=2))
print('PASS authenticated dashboard/accounts/task details, method guards, unavailable metrics, recorded token usage and lossless tool registry')
