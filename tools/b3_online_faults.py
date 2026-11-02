"""Online probes against ONLY the caller-owned live fixture and real service APIs."""
import concurrent.futures,datetime,hashlib,json,subprocess,threading,time,urllib.request,urllib.error

def exercise(*,out,origin,gateway,admin,sql,container,writer_calls,lock,controls,health_res):
 evidence=[]
 def http(base,path,token,body=None,method='POST',key=None):
  headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'}
  if key:headers['Idempotency-Key']=key
  req=urllib.request.Request(base+path,headers=headers,data=json.dumps(body).encode() if body is not None else None,method=method)
  try:
   with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req,timeout=15) as r:return r.status,json.load(r)
  except urllib.error.HTTPError as e:return e.code,json.load(e)
 def count():
  with lock:return len(writer_calls)
 def wait(query,label):
  end=time.monotonic()+8
  while time.monotonic()<end:
   value=sql(query)
   if value and value!='0':return value
  raise AssertionError('DB barrier timeout: '+label+' activity='+sql("SELECT jsonb_agg(to_jsonb(x))::text FROM (SELECT pid,application_name,wait_event,query FROM pg_stat_activity WHERE datname='bba_fixture') x"))
 def issue(text,tag,kind='group',target='777'):
  ref='online_fault_'+tag;deadline=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(minutes=1)).isoformat().replace('+00:00','Z')
  body={'trigger_ref':ref,'account_id':'acc_a','conversation_kind':kind,'conversation_id':target,'source_event_id':ref,'sender_id':'20001','directed_account_id':None,'deadline_at':deadline,'segment_count':1}
  code,r=http(origin,'/v1/internal/chat-triggers','b3-issue-only',body,key=ref);assert code==201,(code,r)
  body={'send_id':ref,'account_id':'acc_a','conversation_kind':kind,'conversation_id':target,'trigger_ref':ref,'text_digest':hashlib.sha256(text.encode()).hexdigest(),'expected_binding_epoch':'1','deadline_at':deadline,'segment_index':0,'segment_count':1}
  code,r=http(origin,'/v1/internal/chat-send-permits','b3-issue-only',body,key=ref);assert code==200,(code,r)
  permit=r['data']['permit_id']
  return permit,{'send_id':ref,'segment_index':0,'text':text,'account_id':'acc_a','conversation_kind':kind,'conversation_id':target}
 def send(item,token='b3-gateway-only',body=None):
  permit,payload=item
  return http(gateway,'/v1/internal/chat-send-permits/'+permit+'/gateway-send',token,payload if body is None else body,key=payload['send_id'])
 def rule(text):
  return http(origin,'/v1/operator/content-rules',admin,{'name':'online barrier '+text,'kind':'keyword','expression':text,'case_sensitive':True,'enabled':True,'expected_version':0})
 def held(query):
  p=subprocess.Popen(['docker','exec','-i',container,'psql','-U','bba_fixture','-d','bba_fixture','-At','-v','ON_ERROR_STOP=1'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,bufsize=1)
  p.stdin.write("BEGIN;"+query+";SELECT 'BARRIER_READY';\n");p.stdin.flush()
  while True:
   line=p.stdout.readline()
   if line.strip()=='BARRIER_READY':break
   if not line:raise RuntimeError('SQL lock failed: '+p.stderr.read())
  return p
 def release(p):
  p.stdin.write('COMMIT;\n');p.stdin.flush();stdout,stderr=p.communicate(timeout=8);assert p.returncode==0,(stdout,stderr)
 def record(row):
  evidence.append(row);(out/'online-faults.json').write_text(json.dumps(evidence,indent=2))
 def arm(text):
  c={'entered':threading.Event(),'release':threading.Event()}
  with lock:controls[text]=c
  return c
 def guard_pid():
  raw=sql("SELECT pid FROM pg_stat_activity WHERE datname='bba_fixture' AND application_name='bba_content_guard'")
  ids=[int(v) for v in raw.splitlines() if v];assert len(ids)==1,ids
  return ids[0]
 with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
  # Valid, unconsumed permits for both group and private are independently checked below.
  item=issue('PRE_UPDATE_BLOCK','pre_update');n=count()
  barrier=held("SELECT pg_advisory_xact_lock(hashtextextended('ws_fixture',732613))")
  try:
   update=pool.submit(rule,'PRE_UPDATE_BLOCK')
   first=wait("SELECT pid FROM pg_stat_activity WHERE datname='bba_fixture' AND wait_event='advisory' AND query LIKE '%pg_advisory_xact_lock%' AND application_name='bba_content_guard'",'update queued')
   dispatched=pool.submit(send,item)
   second=wait("SELECT pid FROM pg_stat_activity WHERE datname='bba_fixture' AND wait_event='advisory' AND application_name='bba_content_guard' AND pid!="+first,'send queued')
  finally:release(barrier)
  assert update.result()[0]==200
  result=dispatched.result();assert result[0]==403 and result[1]['error']['code']=='CONTENT_BLOCKED',result;assert count()==n
  record({'case':'update_before_review','waiter_pids':[first,second],'result':result,'writer_delta':0})
  # A private permit cannot transfer account/workspace/text/peer either.
  item=issue('PRIVATE_BOUND_NEGATIVE','private_bound','private','9001');n=count();neg=[]
  for name,token,body in [('account','b3-gateway-blocked',item[1]),('workspace','b3-gateway-other',item[1]),('text','b3-gateway-only',{**item[1],'text':'changed'}),('peer','b3-gateway-only',{**item[1],'conversation_id':'9002'})]:
   result=send(item,token,body);assert result[0]>=400,result;assert count()==n;neg.append({'case':name,'result':result,'writer_delta':0})
  result=send(item);assert result[0]==200 and count()==n+1,result
  replay=send(item);assert replay[0]==200 and count()==n+1,replay
  record({'case':'private_issued_permit_binding','negative':neg,'valid_result':result,'valid_writer_delta':1,'replay_writer_delta':0})
  # Probe transfer/tamper with an ISSUED permit, then prove the intact permit still works.
  item=issue('POST_WRITER_RULE','post_writer');n=count()
  neg=[]
  for name,token,body in [('account','b3-gateway-blocked',item[1]),('workspace','b3-gateway-other',item[1]),('text','b3-gateway-only',{**item[1],'text':'changed'}),('target','b3-gateway-only',{**item[1],'conversation_id':'778'})]:
   r=send(item,token,body);assert r[0]>=400,r;assert count()==n;neg.append({'case':name,'status':r[0],'reply':r[1],'writer_delta':0})
  c=arm('POST_WRITER_RULE');dispatched=pool.submit(send,item)
  try:
   assert c['entered'].wait(8),'writer entry barrier'
   pid=guard_pid();update=pool.submit(rule,'POST_WRITER_RULE')
   waiter=wait("SELECT u.pid FROM pg_stat_activity u WHERE u.datname='bba_fixture' AND u.wait_event='advisory' AND "+str(pid)+"=ANY(pg_blocking_pids(u.pid))",'rule waits for in-flight writer')
   assert not update.done(),'rule must wait behind sender guard'
  finally:c['release'].set()
  result=dispatched.result();assert result[0]==200,result;assert update.result()[0]==200;assert count()==n+1
  replay=send(item);assert replay[0]==200 and count()==n+1,replay
  next_item=issue('POST_WRITER_RULE','post_writer_next');blocked=send(next_item);assert blocked[0]==403 and count()==n+1,blocked
  record({'case':'update_after_writer_entry','guard_pid':pid,'rule_waiter_pid':waiter,'result':result,'writer_delta':1,'replay_writer_delta':0,'next_blocked_writer_delta':0,'issued_permit_negative':neg})
  # Hold a real database table to expose the pre-writer health boundary.
  item=issue('HEALTH_PRE_WRITER','health_pre');n=count();barrier=held('LOCK TABLE core.content_review IN ACCESS EXCLUSIVE MODE')
  try:
   dispatched=pool.submit(send,item)
   waiter=wait("SELECT pid FROM pg_stat_activity WHERE datname='bba_fixture' AND wait_event_type='Lock' AND query LIKE '%INSERT INTO core.content_review%'",'review blocked before writer')
   pid=guard_pid();assert sql('SELECT pg_terminate_backend('+str(pid)+')')=='t'
  finally:release(barrier)
  result=dispatched.result();assert result[0]>=400 and count()==n,result
  replay=send(item);assert replay[0]>=400 and count()==n,replay
  record({'case':'guard_health_before_writer','fixture_container':container,'database':'bba_fixture','terminated_guard_pid':pid,'review_waiter_pid':waiter,'result':result,'replay':replay,'writer_delta':0,'replay_writer_delta':0})
  # A real writer is entered, then its exact fixture guard is terminated. A late
  # success response must not be persisted as sent. No guessed sleeps or global kills.
  item=issue('HEALTH_IN_FLIGHT','health_in_flight');n=count();c=arm('HEALTH_IN_FLIGHT');dispatched=pool.submit(send,item)
  try:
   assert c['entered'].wait(8),'writer did not enter';pid=guard_pid()
   assert sql('SELECT pg_terminate_backend('+str(pid)+')')=='t'
   assert sql("SELECT count(*) FROM pg_stat_activity WHERE datname='bba_fixture' AND pid="+str(pid))=='0'
  finally:c['release'].set()
  result=dispatched.result();assert result[0]==409 and result[1]['error']['code']=='EXTERNAL_OUTCOME_UNKNOWN',result
  assert count()==n+1
  permit=item[0];outcome=sql("SELECT state FROM core.chat_send_outcome WHERE permit_id='"+permit+"'");assert outcome=='unknown',outcome
  replay=send(item);assert replay[0]==409 and count()==n+1,replay
  record({'case':'guard_health_in_flight','fixture_container':container,'database':'bba_fixture','terminated_guard_pid':pid,'result':result,'durable_outcome':outcome,'writer_delta':1,'replay_writer_delta':0})
  # Transport result unavailable: local writer records entry but deliberately closes
  # without a receipt. The real gateway must persist unknown and never resend.
  item=issue('DROP_WRITER_RESULT','drop_result');n=count();c=arm('DROP_WRITER_RESULT');c['drop']=True;dispatched=pool.submit(send,item)
  try:assert c['entered'].wait(8)
  finally:c['release'].set()
  result=dispatched.result();assert result[0]==409 and result[1]['error']['code']=='EXTERNAL_OUTCOME_UNKNOWN',result
  assert sql("SELECT state FROM core.chat_send_outcome WHERE permit_id='"+item[0]+"'")=='unknown'
  replay=send(item);assert replay[0]==409 and count()==n+1,replay
  record({'case':'writer_result_unknown','result':result,'durable_outcome':'unknown','writer_delta':1,'replay_writer_delta':0})
  # Bridge one-shot join also fails closed on the exact fixture guard loss.
  op=health_res['OperationID'];task=health_res['TaskID'];token='b3-gateway-health'
  def bridge(action):return http(gateway,'/v1/internal/bridge/operations/'+op+'/'+action,token)
  for action in ['accept','permit']:
   r=bridge(action);assert r[0]==200,r
  c=arm('join:88003');n=count();dispatched=pool.submit(bridge,'dispatch')
  try:
   assert c['entered'].wait(8),'join writer entry barrier';pid=guard_pid()
   assert sql('SELECT pg_terminate_backend('+str(pid)+')')=='t'
  finally:c['release'].set()
  result=dispatched.result();assert result[0]>=400 and result[1]['error']['code']=='EXTERNAL_OUTCOME_UNKNOWN',result
  assert count()==n+1
  state=sql("SELECT state FROM bridge.gateway_operation WHERE operation_id='"+op+"'");assert state=='UNKNOWN',state
  certainty=sql("SELECT effect_certainty FROM core.execution WHERE task_id='"+task+"'");assert certainty=='unknown',certainty
  occupation=sql("SELECT count(*) FROM core.account_action_lock WHERE account_id='acc_c' AND held");assert occupation=='1',occupation
  replay=bridge('dispatch');assert replay[0]==200 and replay[1]['data']['disposition']=='unknown' and replay[1]['data']['effect_certainty']=='unknown' and count()==n+1,replay
  record({'case':'bridge_guard_health_in_flight','fixture_container':container,'database':'bba_fixture','terminated_guard_pid':pid,'operation_id':op,'task_id':task,'account_id':'acc_c','durable_operation_state':state,'effect_certainty':certainty,'held_account_locks':occupation,'result':result,'replay':replay,'writer_delta':1,'replay_writer_delta':0})
 return evidence
