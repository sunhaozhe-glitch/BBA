/* Real-service notification navigation regression + remaining known-gap evidence.
 * Exit 0 requires the positive navigation regression and all evidence checks to pass.
 * Pagination/header gaps still keep overall M1 PARTIAL; they are separate work.
 * Exit nonzero on any check/setup/transport error; never treat collection as PASS.
 * Real Go + caller-owned manual fixture only. Usage:
 * <node> tests/product/b7-real-service-acceptance.cjs <fixture-config.json> <new-outdir>
 * Credentials travel file -> memory -> HTTP headers only. No route()/mock responses.
 * The caller owns launcher lifecycle; this script closes its browser/CDP sessions.
 * Unique synthetic rows remain in the temporary DB (messages are append-only).
 * validation_checks describe evidence collection; product_findings/acceptance
 * deliberately distinguish reproduced gaps from a complete product PASS.
 */
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const {execFileSync} = require('node:child_process');
const {chromium} = require('/home/sun/.hermes/hermes-agent/node_modules/playwright');
const cfg = JSON.parse(fs.readFileSync(path.resolve(process.argv[2]), 'utf8'));
const out = path.resolve(process.argv[3]);
assert(!fs.existsSync(out), 'evidence directory must be new');
fs.mkdirSync(out, {recursive:true, mode:0o700});
const prefix = 'b7acc-' + Date.now().toString(36) + '-' + crypto.randomBytes(4).toString('hex');
const checks = [], findings = [], network = [], pending = new Set();
const tokens = Object.fromEntries(['operator','reader','runtime'].map(r=>[r, fs.readFileSync(cfg.token_files[r], 'utf8').trim()]));
const dsn = new URL(fs.readFileSync(cfg.dsn_file, 'utf8').trim());
const owner = JSON.parse(fs.readFileSync(cfg.state_file, 'utf8'));
const fixtureDir = path.dirname(path.resolve(cfg.state_file));
assert.equal(cfg.origin, 'http://127.0.0.1:' + owner.http_port, 'origin must be the owned server port');
assert.equal(cfg.db_container, owner.container);
assert.equal(cfg.db_container, 'bba-b7fix-' + owner.instance);
assert.equal(path.resolve(owner.output), fixtureDir);
assert.equal(fs.realpathSync(owner.binary), fs.realpathSync(path.join(fixtureDir,'bin/taskengine-server')));
assert.equal(fs.realpathSync('/proc/'+owner.server_pid+'/exe'), fs.realpathSync(owner.binary), 'owned PID executable');
const serverArgv = fs.readFileSync('/proc/'+owner.server_pid+'/cmdline','utf8').split('\0').filter(Boolean);
assert.equal(serverArgv[0], owner.binary);
for (const [flag,value] of [['-listen','127.0.0.1:'+owner.http_port],['-origin',cfg.origin],['-resolver-file',cfg.resolver_file]]) {
  assert.equal(serverArgv[serverArgv.indexOf(flag)+1],value,'owned server '+flag);
}
assert(!serverArgv.some(a=>a.startsWith('-worker') || a.startsWith('-development-operations')));
assert.equal(dsn.hostname,'127.0.0.1');assert.equal(dsn.port,String(owner.db_port));
assert.equal(decodeURIComponent(dsn.username),'b7fix_'+owner.instance);
assert.equal(dsn.pathname,'/b7fix_'+owner.instance+'_db');
const labels = JSON.parse(execFileSync('docker',['inspect','--format','{{json .Config.Labels}}',cfg.db_container],{encoding:'utf8'}));
assert.equal(labels['bba.b7-manual-fixture'], owner.instance, 'caller-owned fixture label');
const ports = JSON.parse(execFileSync('docker',['inspect','--format','{{json .NetworkSettings.Ports}}',cfg.db_container],{encoding:'utf8'}));
assert(ports['5432/tcp'].some(p=>p.HostIp==='127.0.0.1' && p.HostPort===String(owner.db_port)), 'DB port belongs to labeled container');
function sql(query) {
  // No DSN/password/token in argv. psql uses the container's local socket.
  return execFileSync('docker',['exec','-i',cfg.db_container,'psql','-X','-v','ON_ERROR_STOP=1','-U',decodeURIComponent(dsn.username),'-d',dsn.pathname.slice(1),'-At'], {input:query,encoding:'utf8',timeout:10000,killSignal:'SIGKILL'}).trim();
}
function save(name, value) { fs.writeFileSync(path.join(out,name), typeof value==='string'?value:JSON.stringify(value,null,2)+'\n', {mode:0o600}); }
function zeroFacts() { return JSON.parse(sql("SELECT json_build_object('schema_max',(SELECT max(version) FROM core.schema_version),'task',(SELECT count(*) FROM core.task),'chat_send_permit',(SELECT count(*) FROM core.chat_send_permit),'dispatch_permit',(SELECT count(*) FROM core.dispatch_permit),'model_request',(SELECT count(*) FROM core.model_request));")); }
function assertZero(f) { for(const k of ['task','chat_send_permit','dispatch_permit','model_request']) assert.equal(f[k],0,k); }
async function api(method, url, role='operator', body) {
  return new Promise((resolve,reject)=>{
    const u = new URL(url,cfg.origin);
    const req = http.request(u,{method,headers:{Authorization:'Bearer '+tokens[role],...(body?{'Content-Type':'application/json','Idempotency-Key':prefix+'-'+crypto.randomUUID()}:{})}},r=>{
      let text='';r.on('data',c=>text+=c);r.on('end',()=>{
        let json;try {json=JSON.parse(text)} catch {json=null}
        network.push({channel:'aux-api',method,path:u.pathname+u.search,status:r.statusCode,response:json});
        resolve({status:r.statusCode,json});
      });
    }); req.setTimeout(10000,()=>req.destroy(new Error('HTTP timeout')));req.on('error',reject);
    req.end(body?JSON.stringify(body):undefined);
  });
}
async function check(name, fn) {
  try { await fn(); checks.push({name,status:'PASS'}); }
  catch(e) {checks.push({name,status:'FAIL',error:e.message,...(e.verification?{verification:e.verification}:{})}); console.error(name+': '+e.message);}
}
const sleep = ms => new Promise(r=>setTimeout(r,ms));
function deadline(promise, label, ms=10000) {
  return new Promise((resolve,reject)=>{
    const timer=setTimeout(()=>reject(Error(label+' timed out after '+ms+'ms')),ms);
    Promise.resolve(promise).then(value=>{clearTimeout(timer);resolve(value)},error=>{clearTimeout(timer);reject(error)});
  });
}
// Resolve both success and failure from creation: a waiter must never reject
// unobserved while a different operation fails or CDP release is still pending.
const observed = promise => promise.then(value=>({value}),error=>({error}));
let browser, page, cdp, hold, restoredActive=false;
const notificationCheckName='real committed notification POST after navigation preserves chat/session/error and read version';
const emptyId=prefix+'-empty', msgId=prefix+'-messages', noticeId=prefix+'-cas';
async function login(role='operator', route='chat') {
  await page.goto(cfg.origin+'/product/'+route);
  if(!(await page.locator('#auth').isVisible())) await page.locator('#logout').click();
  await page.locator('#credential').fill(tokens[role]);
  await page.locator('#connect-form').dispatchEvent('submit');
  await page.waitForFunction(()=>document.querySelector('#auth').hidden);
  await loaded();
}
async function loaded() { await page.waitForFunction(()=>!document.querySelector('#content .loading') && document.querySelector('#content').textContent.length>0); }
async function nav(route) {
  if(await page.locator('#menu').isVisible()) await page.locator('#menu').click();
  await page.locator(`[data-route="${route}"]`).click(); await loaded();
}
async function refresh() {
  const response=page.waitForResponse(r=>r.request().method()==='GET' && new URL(r.url()).pathname==='/v1/operator/notifications');
  await page.locator('#refresh').click();await response;await loaded();
}
async function shot(name) {await page.screenshot({path:path.join(out,name+'.png'),fullPage:true,timeout:10000});}
async function holdResponse(url) {
  let arrive, rejectArrival, release, done;
  const arrived=new Promise((resolve,reject)=>{arrive=resolve;rejectArrival=reject});
  const released=new Promise(resolve=>release=resolve), finished=new Promise(resolve=>done=resolve);
  // Attach immediately so transport failures cannot become unhandled rejections.
  arrived.catch(()=>{});
  cdp=await deadline(page.context().newCDPSession(page),'CDP session');
  const session=cdp;
  let matched=false;
  hold={release,finished};
  session.on('Fetch.requestPaused',async event=>{
    try {
      // Only hold this POST at Response stage with a real Go 200. GETs and
      // other statuses continue untouched and cannot satisfy the arrival gate.
      const exact=event.request.method==='POST' && event.request.url===cfg.origin+url;
      if(!exact || event.responseStatusCode!==200) {
        await deadline(session.send('Fetch.continueRequest',{requestId:event.requestId}),'CDP continue response');
        if(exact)throw Error('notification POST expected real 200, got '+event.responseStatusCode);
        return;
      }
      assert(!matched,'only one notification POST may be held');matched=true;
      const body=await deadline(session.send('Fetch.getResponseBody',{requestId:event.requestId}),'CDP response body');
      save('delayed-real-response.json',{method:event.request.method,path:url,status:event.responseStatusCode,body:JSON.parse(body.base64Encoded?Buffer.from(body.body,'base64').toString():body.body)});
      arrive(event);await released;
      // Continue the original response; no fulfillRequest or synthetic body.
      await deadline(session.send('Fetch.continueRequest',{requestId:event.requestId}),'CDP continue response');done();
    } catch(e) {
      rejectArrival(e);done({error:e.message});
      checks.push({name:'CDP hold transport',status:'FAIL',error:e.message});
    }
  });
  await deadline(session.send('Fetch.enable',{patterns:[{urlPattern:cfg.origin+url,requestStage:'Response'}]}),'CDP enable');
  return {arrived,release,finished};
}
async function closeHold() {
  const closingHold=hold, session=cdp, errors=[];
  hold=null;cdp=null;
  // Every cleanup step has its own deadline; one failure must not skip detach
  // or the caller's browser/DB/result finalization.
  if(closingHold){
    closingHold.release();
    try{await deadline(closingHold.finished,'CDP release cleanup',1000)}catch(e){errors.push(e.message)}
  }
  if(session){
    try{await deadline(session.send('Fetch.disable'),'CDP disable',2000)}catch(e){errors.push(e.message)}
    try{await deadline(session.detach(),'CDP detach',2000)}catch(e){errors.push(e.message)}
  }
  if(errors.length)throw Error(errors.join('; '));
}
async function chatSnapshot() {
  return page.evaluate(()=>({
    pathname:location.pathname,content:document.querySelector('#content').innerHTML,
    auth_hidden:document.querySelector('#auth').hidden,
    logout_visible:!!document.querySelector('#logout').getClientRects().length,
    error_hidden:document.querySelector('#error').hidden,error:document.querySelector('#error').textContent,
    connection:document.querySelector('#connection').textContent,
    chat_tabs:!!document.querySelector('#chat-tabs'),chat_form:!!document.querySelector('#chat-form'),
    loading:!!document.querySelector('#content .loading')
  }));
}
(async()=>{
  try {
    save('owner-check.json',{origin:cfg.origin,server_pid:owner.server_pid,binary:owner.binary,binary_sha256:crypto.createHash('sha256').update(fs.readFileSync(owner.binary)).digest('hex'),container:cfg.db_container,container_port_verified:true,server_argv:serverArgv});
    const initial=zeroFacts();save('db-before.json',initial);assertZero(initial);
    // Only the authorized fixture runtime; derive its literal identity from resolver.
    const resolver=JSON.parse(fs.readFileSync(cfg.resolver_file,'utf8'));
    const runtime=resolver.credentials.find(c=>c.token_sha256===crypto.createHash('sha256').update(tokens.runtime).digest('hex'));
    assert(runtime && runtime.account_ids.includes('acct-qq-2'));
    save('runtime-identity.json',{runtime_id:runtime.runtime_id,owner_epoch:runtime.owner_epoch,account_ids:runtime.account_ids});
    await check('runtime real HTTP 201/new, 200/replay, 409/different digest', async()=>{
      const b={account_id:'acct-qq-2',conversation_kind:'private',conversation_id:'qq-peer-202',event_id:prefix+'-runtime',body:prefix+' runtime ingress'};
      const fresh=await api('POST','/v1/operator/qq-inbox/events','runtime',b);
      const replay=await api('POST','/v1/operator/qq-inbox/events','runtime',b);
      const conflict=await api('POST','/v1/operator/qq-inbox/events','runtime',{...b,body:b.body+' changed'});
      assert.equal(fresh.status,201);assert.equal(replay.status,200);assert.equal(replay.json.data.idempotent_replay,true);assert.equal(conflict.status,409);
      const rid=runtime.runtime_id.replaceAll("'","''");
      try {
        sql(`UPDATE core.runtime_credentials SET active=false WHERE workspace_id='b7fix-ws' AND runtime_id='${rid}' AND account_id='acct-qq-2';`);restoredActive=true;
        const revokedNew=await api('POST','/v1/operator/qq-inbox/events','runtime',{...b,event_id:prefix+'-revoked'});
        const revokedReplay=await api('POST','/v1/operator/qq-inbox/events','runtime',b);
        assert.equal(revokedNew.status,404);assert.equal(revokedReplay.status,404);
        assert.equal(revokedNew.json.error.code,'RESOURCE_NOT_FOUND');
        assert.equal(Number(sql(`SELECT count(*) FROM core.operator_message WHERE event_id='${prefix}-revoked';`)),0);
      } finally {
        sql(`UPDATE core.runtime_credentials SET active=true WHERE workspace_id='b7fix-ws' AND runtime_id='${rid}' AND account_id='acct-qq-2';`);restoredActive=false;
      }
      const boundary=JSON.parse(sql("SELECT json_agg(json_build_object('id',a.id,'runtime_id',a.runtime_id,'control',a.control_state,'identity',a.identity_status,'join_verified',a.request_join_verified,'membership_verified',a.membership_verified,'membership_authority',c.membership_authority)) FROM core.account a JOIN core.runtime_credentials c ON (c.workspace_id,c.account_id,c.runtime_id)=(a.workspace_id,a.id,a.runtime_id) WHERE a.workspace_id='b7fix-ws';"));
      save('runtime-boundary.json',boundary);assert.equal(boundary.length,2);
      for(const a of boundary){assert.equal(a.control,'paused');assert.equal(a.identity,'unverified');assert.equal(a.membership_authority,false);assert.equal(a.join_verified,false);assert.equal(a.membership_verified,false);}
    });
    // Self-owned synthetic test rows; no seed source edits or message updates/deletes.
    sql(`BEGIN;
INSERT INTO core.operator_conversation(workspace_id,id,kind,conversation_kind,conversation_id) VALUES ('b7fix-ws','${emptyId}','agent','agent','${emptyId}'),('b7fix-ws','${msgId}','agent','agent','${msgId}');
INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,summary) VALUES ('b7fix-ws','${noticeId}','${noticeId}','fixture.cas','${noticeId}','${noticeId}');
COMMIT;`);
    browser=await chromium.launch({headless:true,executablePath:process.env.BBA_TEST_CHROME||'/opt/google/chrome/chrome',args:['--no-sandbox']});
    page=await browser.newPage({viewport:{width:1440,height:1000}});page.setDefaultTimeout(10000);
    page.on('response',r=>{
      const u=new URL(r.url());if(!u.pathname.startsWith('/v1/'))return;
      const rec={channel:'browser',method:r.request().method(),path:u.pathname+u.search,status:r.status()};network.push(rec);
      const p=r.json().then(b=>rec.response=b).catch(()=>{}).finally(()=>pending.delete(p));pending.add(p);
    });
    await login();
    await check('browser empty real Agent conversation shows zero messages',async()=>{
      await page.locator(`[data-chat-conv="${emptyId}"]`).click();
      await page.getByText('该会话暂无消息',{exact:true}).waitFor();
      const back=await api('GET','/v1/operator/conversations/'+emptyId+'/messages?limit=1');
      assert.equal(back.status,200);assert.deepEqual(back.json.data.items,[]);await shot('empty-conversation');
    });
    await check('browser reader write receives real 403 and displays error',async()=>{
      await login('reader');await page.locator(`[data-chat-conv="${emptyId}"]`).click();await page.locator('#chat-form').waitFor();
      await page.locator('#chat-form textarea').fill(prefix+' reader denied');
      const result=page.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname.includes('/agent-threads/'));
      await page.locator('#chat-form-submit').click();assert.equal((await result).status(),403);
      await page.waitForFunction(()=>document.querySelector('#chat-form-error').textContent.includes('403'));await shot('reader-403');
      assert.equal(Number(sql(`SELECT count(*) FROM core.operator_message WHERE conversation_id='${emptyId}';`)),0);
    });
    await login();await nav('notify');
    await check('browser stale notification CAS returns real 409 then refresh recovers',async()=>{
      const b=page.locator(`[data-notice-read="${noticeId}"]`);await b.waitFor();const v=Number(await b.getAttribute('data-version'));
      const other=await api('POST','/v1/operator/notifications/'+noticeId,'operator',{expected_version:v,read:false});assert.equal(other.status,200);assert.equal(other.json.data.version,v+1);
      const response=page.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname.endsWith('/'+noticeId));
      await b.click();assert.equal((await response).status(),409);
      await page.waitForFunction(()=>document.querySelector('#error').textContent.includes('409'));await shot('cas-real-409');
      assert.equal(Number(sql(`SELECT version FROM core.operator_notification WHERE id='${noticeId}';`)),v+1);
      await refresh();assert.equal(Number(await b.getAttribute('data-version')),v+1);
      const success=page.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname.endsWith('/'+noticeId));await b.click();assert.equal((await success).status(),200);
      await page.waitForFunction(id=>!document.querySelector(`[data-notice-read="${id}"]`),noticeId);
      assert.equal(sql(`SELECT (read_at IS NOT NULL)::text FROM core.operator_notification WHERE id='${noticeId}';`),'true');await shot('cas-refresh-recovered');
    });
    await check('aux API notification read:false persists unread with incremented version',async()=>{
      const v=Number(sql(`SELECT version FROM core.operator_notification WHERE id='${noticeId}';`));
      const back=await api('POST','/v1/operator/notifications/'+noticeId,'operator',{expected_version:v,read:false});assert.equal(back.status,200);assert.equal(back.json.data.version,v+1);
      assert.equal(sql(`SELECT (read_at IS NULL)::text FROM core.operator_notification WHERE id='${noticeId}';`),'true');await refresh();await page.locator(`[data-notice-read="${noticeId}"]`).waitFor();
    });
    await check(notificationCheckName,async()=>{
      let failure;
      try {
        const b=page.locator(`[data-notice-read="${noticeId}"]`);
        const beforeVersion=Number(await b.getAttribute('data-version'));
        const h=await holdResponse('/v1/operator/notifications/'+noticeId);
        await b.click();
        const event=await deadline(h.arrived,'real notification response arrival');
        assert.equal(event.request.method,'POST');assert.equal(event.responseStatusCode,200);
        // DB + a separate HTTP GET prove commit before navigation/release.
        const committed=JSON.parse(sql(`SELECT json_build_object('read',read_at IS NOT NULL,'version',version) FROM core.operator_notification WHERE workspace_id='b7fix-ws' AND id='${noticeId}';`));
        const readback=await api('GET','/v1/operator/notifications?limit=100');
        assert.equal(readback.status,200);
        const persisted=readback.json.data.items.find(n=>n.id===noticeId);
        save('delayed-commit-before-release.json',{before_version:beforeVersion,db:committed,api:persisted});
        assert.deepEqual(committed,{read:true,version:beforeVersion+1});
        assert(persisted?.read_at);assert.equal(persisted.version,beforeVersion+1);
        await nav('chat');await page.locator(`[data-chat-conv="${emptyId}"]`).click();
        await page.getByText('该会话暂无消息',{exact:true}).waitFor();await page.locator('#chat-form').waitFor();
        // Playwright screenshots may restore textarea style as style="". Take
        // the evidence image first, then freeze the unmodified comparison oracle.
        await shot('delayed-before-release-chat');
        const before=await chatSnapshot();save('delayed-chat-before-release.json',before);
        assert.equal(before.auth_hidden,true);assert.equal(before.error_hidden,true);assert.equal(before.loading,false);
        const response=observed(deadline(page.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname==='/v1/operator/notifications/'+noticeId,{timeout:10000}),'browser notification response'));
        const release=observed(deadline(h.finished,'CDP notification release'));
        h.release();
        // Join both bounded outcomes before throwing. No orphan waiter remains
        // when Fetch.continueRequest fails, times out or the page is closed.
        const [transport,received]=await Promise.all([release,response]);
        if(transport.error)throw transport.error;
        if(received.error)throw received.error;
        assert(!transport.value?.error,transport.value?.error);
        const delivered=received.value;assert.equal(delivered.status(),200);
        assert.equal(await deadline(delivered.finished(),'browser response body completion'),null);
        // Observe the actual browser handler after delivery, recording the
        // complete content/session/error state throughout a bounded window.
        const samples=[];
        for(let i=0;i<15;i++){await sleep(50);samples.push(await chatSnapshot());}
        save('delayed-chat-after-release.json',{before,samples});await shot('delayed-after-release-chat');
        const afterReadback=await api('GET','/v1/operator/notifications?limit=100');assert.equal(afterReadback.status,200);
        const afterPersisted=afterReadback.json.data.items.find(n=>n.id===noticeId);
        assert(afterPersisted?.read_at);assert.equal(afterPersisted.version,beforeVersion+1);
        // Return to notify even on the old buggy UI, proving the committed write
        // was not cancelled and its new version survives route changes.
        await nav('notify');
        const row=page.getByRole('row').filter({has:page.getByRole('cell',{name:noticeId,exact:true})});
        await row.waitFor();assert((await row.textContent()).includes('已读'));
        assert.equal(await page.locator(`[data-notice-read="${noticeId}"]`).count(),0);
        const returned=JSON.parse(sql(`SELECT json_build_object('read',read_at IS NOT NULL,'version',version) FROM core.operator_notification WHERE workspace_id='b7fix-ws' AND id='${noticeId}';`));
        save('delayed-return-notify.json',{db:returned,api:afterPersisted,row_text:await row.textContent()});
        assert.deepEqual(returned,committed);await shot('delayed-return-notify');
        // Only an observed UI state mismatch establishes this product bug.
        // CDP/DB/API/navigation/screenshot errors remain verification failures.
        try {
          for(const sample of samples)assert.deepEqual(sample,before,'late committed notification receipt must preserve chat content, login and error state');
        } catch(e) {
          e.productRegression=true;
          findings.push({id:'late-notification-post-navigation',kind:'bug',priority:'P2',status:'REGRESSION_FAILED',acceptance:'FAIL',detail:e.message});
          throw e;
        }
      } catch(e) {
        failure=e;
        if(!e.productRegression)e.verification='NOT_VERIFIED';
        throw e;
      } finally {
        try{await closeHold()}catch(e){
          checks.push({name:'notification CDP cleanup',status:'FAIL',error:e.message});
          if(!failure){e.verification='NOT_VERIFIED';throw e;}
        }
      }
    });
    // Recover the test session after a failed navigation regression so other
    // permissions, pagination, header and execution-fact evidence still runs.
    await login();
    // 51 same-type messages and notifications: known UI pagination gap, not M1 PASS.
    sql(`BEGIN;
INSERT INTO core.operator_message(workspace_id,id,conversation_id,source,direction,delivery_state,body,body_digest,event_id,seq)
SELECT 'b7fix-ws','${prefix}-m-'||g,'${msgId}','agent','inbound','received','${prefix} message '||g,repeat('a',64),'${prefix}-evt-'||g,g FROM generate_series(1,51) g;
INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,summary,created_at)
SELECT 'b7fix-ws','${prefix}-n-'||g,'${prefix}-n-'||g,'fixture.pagination','${prefix}-n-'||g,'${prefix} notice '||g,clock_timestamp()-((52-g)*interval '1 second') FROM generate_series(1,51) g;
COMMIT;`);
    await check('aux API limit=1 cursor pagination for conversations/messages/notifications',async()=>{
      for(const endpoint of ['/v1/operator/conversations','/v1/operator/conversations/'+msgId+'/messages','/v1/operator/notifications']){
        const a=await api('GET',endpoint+'?limit=1');assert.equal(a.status,200);assert.equal(a.json.data.items.length,1);assert(a.json.data.next_cursor);
        const b=await api('GET',endpoint+'?limit=1&cursor='+encodeURIComponent(a.json.data.next_cursor));assert.equal(b.status,200);assert.equal(b.json.data.items.length,1);assert.notEqual(b.json.data.items[0].id,a.json.data.items[0].id);
      }
    });
    await check('browser 51 messages: next_cursor present but tail invisible and unconsumed',async()=>{
      await nav('chat');const start=network.length;await page.locator(`[data-chat-conv="${msgId}"]`).click();await page.locator('#chat-form').waitFor();
      const endpoint='/v1/operator/conversations/'+msgId+'/messages';
      const first=await api('GET',endpoint+'?limit=50');assert.equal(first.json.data.items.length,50);assert.equal(first.json.data.next_cursor,'50');
      const last=await api('GET',endpoint+'?limit=50&cursor=50');assert.equal(last.json.data.items.length,1);assert.equal(last.json.data.items[0].seq,1);
      assert.equal(Number(sql(`SELECT count(*) FROM core.operator_message WHERE conversation_id='${msgId}';`)),51);
      const text=await page.locator('#content').textContent();assert(text.includes(prefix+' message 51'));// Exact cell matching distinguishes message 1 from message 10..19.
      assert.equal(await page.getByRole('cell',{name:prefix+' message 1',exact:true}).count(),0);
      await page.locator('#content').evaluate(el=>el.scrollTo(0,el.scrollHeight));await sleep(200);
      assert.equal(await page.getByRole('button',{name:/加载更多|下一页/}).count(),0);
      assert(!network.slice(start).some(r=>r.channel==='browser'&&r.path.startsWith(endpoint)&&r.path.includes('cursor=')));await shot('pagination-messages-tail-missing');
      findings.push({id:'message-pagination',kind:'gap',priority:'P2',status:'CONFIRMED',acceptance:'PARTIAL',detail:'51 Agent messages persisted; API returns 50 + next_cursor=50, second API page contains seq=1; browser renders 50 and does not consume cursor, oldest message unavailable.'});
    });
    await check('browser 51 notifications: next_cursor present but tail invisible and unconsumed',async()=>{
      const start=network.length;await nav('notify');const first=await api('GET','/v1/operator/notifications?limit=50');assert.equal(first.json.data.items.length,50);assert(first.json.data.next_cursor);
      const hidden=prefix+'-n-1';assert(!first.json.data.items.some(x=>x.id===hidden));
      const second=await api('GET','/v1/operator/notifications?limit=100&cursor='+first.json.data.next_cursor);assert(second.json.data.items.some(x=>x.id===hidden));
      assert.equal(Number(sql(`SELECT count(*) FROM core.operator_notification WHERE id LIKE '${prefix}-n-%';`)),51);
      assert.equal(await page.getByRole('cell',{name:prefix+' notice 1',exact:true}).count(),0);
      await page.locator('#content').evaluate(el=>el.scrollTo(0,el.scrollHeight));await sleep(200);
      assert.equal(await page.getByRole('button',{name:/加载更多|下一页/}).count(),0);assert(!network.slice(start).some(r=>r.channel==='browser'&&r.path.startsWith('/v1/operator/notifications?')&&r.path.includes('cursor=')));await shot('pagination-notifications-tail-missing');
      findings.push({id:'notification-pagination',kind:'gap',priority:'P2',status:'CONFIRMED',acceptance:'PARTIAL',detail:'51 same-type synthetic notifications persisted, API tail contains oldest synthetic item; UI stops at first 50 and consumes no next_cursor.'});
    });
    await check('authenticated chat header connection text observation',async()=>{
      await login();const text=await page.locator('#connection').textContent();save('connection-label.json',{auth_hidden:await page.locator('#auth').isHidden(),text});await shot('authenticated-header');
      if(text==='尚未连接')findings.push({id:'authenticated-header-label',kind:'gap',priority:'P3',status:'CONFIRMED',acceptance:'PARTIAL',detail:'Chat authenticated and real API loaded, header still says 尚未连接.'});
    });
  } catch(e) {checks.push({name:'acceptance setup/flow',status:'FAIL',error:e.message});console.error(e.stack);}
  finally {
    try{await closeHold();}catch(e){checks.push({name:'CDP cleanup',status:'FAIL',error:e.message});}
    try{if(browser)await deadline(browser.close(),'browser close',10000)}catch(e){checks.push({name:'browser cleanup',status:'FAIL',error:e.message});}
    try{await deadline(Promise.allSettled([...pending]),'network evidence completion',10000)}catch(e){checks.push({name:'network evidence cleanup',status:'FAIL',error:e.message});}
    try {const facts=zeroFacts();save('db-after.json',facts);assertZero(facts);checks.push({name:'DB readback zero task/chat permit/dispatch permit/model request',status:'PASS'});}catch(e){checks.push({name:'DB zero external execution readback',status:'FAIL',error:e.message});}
    try{save('network.jsonl',network.map(r=>JSON.stringify(r)).join('\n')+'\n')}catch(e){checks.push({name:'network evidence save',status:'FAIL',error:e.message});}
    const notificationCheck=checks.find(c=>c.name===notificationCheckName);
    const notificationNavigation=notificationCheck?.status==='PASS'?'PASS':notificationCheck?.verification==='NOT_VERIFIED'?'NOT_VERIFIED':notificationCheck?.status||'NOT_VERIFIED';
    const result={prefix,scope:'ISOLATED_FIXTURE',validation_checks:checks,product_findings:findings,acceptance:{M1:'PARTIAL',real_runtime_ingress:checks[0]?.status||'NOT_RUN',pagination:'PARTIAL',notification_navigation:notificationNavigation},credential_restoration_pending:restoredActive,synthetic_rows:'Retained in temporary DB; no append-only message deletion; caller must finally stop launcher.',at:new Date().toISOString()};
    save('result.json',result);
    console.log(JSON.stringify({validation_checks:checks.length,failed:checks.filter(c=>c.status==='FAIL').map(c=>c.name),product_findings:findings.length,acceptance:result.acceptance,out}));
    process.exitCode=checks.some(c=>c.status==='FAIL')?1:0;
  }
})();
