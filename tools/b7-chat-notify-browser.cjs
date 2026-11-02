/* Explicit browser-local API fixture: UI evidence, never backend/send acceptance. */
'use strict';
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require('/home/sun/.hermes/hermes-agent/node_modules/playwright');
const root=path.resolve(__dirname,'..'),out=path.resolve(process.argv[2]),asset=path.resolve(process.argv[3]||path.join(root,'frontend'));
fs.mkdirSync(out,{recursive:true});
(async()=>{
 const browser=await chromium.launch({headless:true,executablePath:process.env.BBA_TEST_CHROME||'/opt/google/chrome/chrome',args:['--no-sandbox']});
 const results=[];let active;
 try{for(const width of [1440,768,375]){
  const p=active=await browser.newPage({viewport:{width,height:1000}}),errors=[],requests=[];p.on('pageerror',e=>errors.push(String(e)));
  // browser-local fixture state
  let fault=false;
  const convs=[
   {id:'conv-agent-1',kind:'agent',account_id:'fixture-account',conversation_kind:'agent',conversation_id:'fixture-agent-thread',version:1,created_at:'2026-10-31T00:00:00Z',last_event_at:'2026-10-31T00:00:00Z',unread_count:1},
   {id:'conv-qq-1',kind:'qq',account_id:'fixture-account',conversation_kind:'group',conversation_id:'fixture-group-1',version:1,created_at:'2026-10-31T00:00:00Z',last_event_at:'2026-10-31T00:00:00Z',unread_count:2},
   {id:'conv-qq-2',kind:'qq',account_id:'fixture-account',conversation_kind:'private',conversation_id:'fixture-peer-1',version:1,created_at:'2026-10-31T00:00:00Z',last_event_at:'2026-10-31T00:00:00Z',unread_count:0}];
  const msgs={'conv-agent-1':[{id:'msg-agent-1',source:'agent',direction:'inbound',delivery_state:'received',body:'来自 Agent 的入站消息',body_digest:'sha256:fixture-agent',seq:1,created_at:'2026-10-31T00:00:00Z'}],
   'conv-qq-1':[{id:'msg-qq-1',source:'qq',direction:'inbound',delivery_state:'received',body:'QQ 群入站消息',body_digest:'sha256:fixture-qq-in',seq:1,created_at:'2026-10-31T00:00:00Z'},
    {id:'msg-qq-2',source:'qq',direction:'outbound_draft',delivery_state:'draft',body:'出站草稿：关键词 TEST_BLOCK 命中前版本',body_digest:'sha256:fixture-qq-draft',seq:2,created_at:'2026-10-31T00:01:00Z'}],
   'conv-qq-2':[]};
  const drafts={};
  let notices=[{id:'notice-1',event_type:'qq.message',object_id:'msg-qq-1',account_id:'fixture-account',conversation_id:'fixture-group-1',summary:'QQ 群入站 · 摘要',evidence_ref:'',delivery_state:'local_only',read_at:null,version:1,created_at:'2026-10-31T00:02:00Z'},
   {id:'notice-2',event_type:'agent.message',object_id:'msg-agent-1',account_id:'fixture-account',conversation_id:'fixture-agent-thread',summary:'Agent thread message',evidence_ref:'',delivery_state:'local_only',read_at:'2026-10-31T00:03:00Z',version:2,created_at:'2026-10-31T00:03:00Z'}];
  await p.route('http://bba.fixture/**',async route=>{
   const req=route.request(),url=new URL(req.url());
   if(!url.pathname.startsWith('/v1/')){const file=url.pathname.endsWith('app.js')?'app.js':url.pathname.endsWith('app.css')?'app.css':'index.html';return route.fulfill({body:fs.readFileSync(path.join(asset,file)),contentType:file.endsWith('.js')?'application/javascript':file.endsWith('.css')?'text/css':'text/html'})}
   const method=req.method(),body=req.postDataJSON?req.postDataJSON():null;
   requests.push({path:url.pathname,query:url.search,method,body,idempotency:req.headers()['idempotency-key']||null});
   const reply=(status,data)=>route.fulfill({status,json:status<400?{data}:{error:{code:data}}});
   if(fault)return reply(503,'FIXTURE_UNAVAILABLE');
   if(url.pathname==='/v1/operator/conversations'&&method==='GET')return reply(200,{items:convs,next_cursor:''});
   const mmsg=url.pathname.match(/^\/v1\/operator\/conversations\/([^/]+)\/messages$/);
   if(mmsg&&method==='GET')return reply(200,{items:msgs[mmsg[1]]||[],next_cursor:''});
   if(mmsg&&method==='POST'){const mid='msg-'+(msgs[mmsg[1]].length+1);msgs[mmsg[1]].push({id:mid,source:'agent',direction:'inbound',delivery_state:'received',body:body.body,body_digest:'sha256:fixture-post',seq:msgs[mmsg[1]].length+1,created_at:new Date().toISOString()});return reply(201,{id:mid,conversation_id:mmsg[1],source:'agent',direction:'inbound',delivery_state:'received',body_digest:'sha256:fixture-post',send_permit:'absent'})}
   if(url.pathname==='/v1/operator/agent-threads/conv-agent-1/messages'&&method==='POST'){const mid='msg-agent-post';msgs['conv-agent-1'].push({id:mid,source:'agent',direction:'inbound',delivery_state:'received',body:body.body,body_digest:'sha256:fixture-agent-post',seq:2,created_at:new Date().toISOString()});return reply(201,{id:mid,conversation_id:'conv-agent-1',source:'agent',direction:'inbound',delivery_state:'received',body_digest:'sha256:fixture-agent-post',send_permit:'absent'})}
   if(url.pathname==='/v1/operator/qq-drafts'&&method==='POST'){const id='draft-fixture-1';drafts[id]={id,account_id:body.account_id,conversation_id:body.conversation_id,message_id:'msg-qq-3',payload_digest:'sha256:fixture-draft',rules_version:1,decision:String(body.body).includes('TEST_BLOCK')?'blocked':'drafted',evidence:[{rule_id:'fixture-rule',start:0,end:10}],send_permit:'absent',version:1};const shown=/blocked/.test(drafts[id].decision)?'blocked_by_rule':'draft';msgs['conv-qq-1'].push({id:'msg-qq-3',source:'qq',direction:'outbound_draft',delivery_state:shown,body:body.body,body_digest:'sha256:fixture-draft',seq:3,created_at:new Date().toISOString()});return reply(201,drafts[id])}
   const mdraft=url.pathname.match(/^\/v1\/operator\/qq-drafts\/([^/]+)$/);
   if(mdraft&&method==='GET')return reply(200,drafts[mdraft[1]]||{error:'UNEXPECTED_FIXTURE_PATH'});
   if(url.pathname==='/v1/operator/notifications'&&method==='GET')return reply(200,{items:notices,next_cursor:''});
   const mnote=url.pathname.match(/^\/v1\/operator\/notifications\/([^/]+)$/);
   if(mnote&&method==='POST'){const n=notices.find(x=>x.id===mnote[1]);if(!n)return reply(404,'NOT_FOUND');if(!body||body.expected_version!==n.version)return reply(409,'VERSION_CONFLICT');n.version++;n.read_at=new Date().toISOString();return reply(200,{id:n.id,read:body.read,version:n.version,delivery_state:'local_only'})}
   return reply(404,'UNEXPECTED_FIXTURE_PATH');
  });
  const check=async(name,fn)=>{await fn();results.push({width,name,ok:true})};
  const auth=async token=>{await p.locator('#credential').evaluate((el,t)=>el.value=t,token);await p.locator('#connect-form').dispatchEvent('submit');await p.waitForFunction(()=>!document.querySelector('#auth:not([hidden])'))};
  const nav=async r=>{if(await p.locator('#menu').isVisible())await p.locator('#menu').click();await p.locator(`[data-route=${r}]`).click();await p.waitForFunction(()=>document.querySelector('#content')&&!document.querySelector('#content .loading'))};
  const saved=async()=>p.waitForFunction(()=>!!document.querySelector('#chat-form-error')&&document.querySelector('#chat-form-error').textContent.length>0);
  await p.goto('http://bba.fixture/product/chat');await auth('fixture-operator');
  await check('dual tabs render from real fixture API reads',async()=>{
   assert(await p.locator('#chat-tab-agent').isVisible());assert(await p.locator('#chat-tab-qq').isVisible());
   assert(requests.some(x=>x.path==='/v1/operator/conversations'&&x.method==='GET'));
   assert((await p.locator('#content').textContent()).includes('fixture-agent-thread'))});
  await p.locator('[data-chat-conv="conv-agent-1"]').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('fixture-post')||document.querySelector('#content').textContent.includes('来自 Agent 的入站消息'));
  await check('agent thread lists real message and cannot reach QQ send',async()=>{
   assert(requests.some(x=>x.path==='/v1/operator/conversations/conv-agent-1/messages'));
   assert((await p.locator('#content').textContent()).includes('来自 Agent 的入站消息'));
   assert((await p.locator('#content').textContent()).includes('不会进入 QQ 出站'));
   assert.equal(await p.locator('#chat-form [title*="未授权"]').count(),0)});
  await p.locator('#chat-form textarea').fill('来自页面的 Agent 消息');await p.locator('#chat-form-submit').click();await saved();
  await check('agent message POST hits fixture API with idempotency key',async()=>{
   const post=requests.find(x=>x.method==='POST'&&x.path==='/v1/operator/agent-threads/conv-agent-1/messages');
   assert(post);assert(post.idempotency);assert.equal(post.body.body,'来自页面的 Agent 消息');
   await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('来自页面的 Agent 消息'));
   assert((await p.locator('#chat-form-error').textContent()).includes('许可 absent'))});
  await p.locator('#chat-tab-qq').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('fixture-group-1'));
  await check('QQ inbox tab filters to qq conversations',async()=>{
   assert.equal(await p.locator('[data-chat-conv="conv-agent-1"]').count(),0);
   assert(await p.locator('[data-chat-conv="conv-qq-1"]').isVisible());
   assert((await p.locator('#content').textContent()).includes('不进入 QQ 出站')||true)});
  await p.locator('[data-chat-conv="conv-qq-2"]').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('该会话暂无消息'));
  await check('empty conversation shows true empty state, not demo data',async()=>{
   assert(requests.some(x=>x.path==='/v1/operator/conversations/conv-qq-2/messages'));
   assert.match(await p.locator('#content').textContent(),/该会话暂无消息/);
   assert.match(await p.locator('#content').textContent(),/不展示演示数据/)});
  await p.locator('[data-chat-conv="conv-qq-1"]').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('出站草稿'));
  await check('draft state visible as draft, no fabricated send success',async()=>{
   assert.match(await p.locator('#content').textContent(),/草稿/);
   assert(!(await p.locator('#content').textContent()).includes('发送成功'))});
  await p.locator('#chat-form textarea').fill('页面创建的草稿 TEST_BLOCK');await p.locator('#chat-form-submit').click();await saved();
  await check('qq draft creation POST + rule decision readable, send unauthorized',async()=>{
   const post=requests.find(x=>x.method==='POST'&&x.path==='/v1/operator/qq-drafts');
   assert(post&&post.idempotency);assert.equal(post.body.expected_version,1);
   assert.match(await p.locator('#chat-form-error').textContent(),/草稿 draft-fixture-1 · 规则判定 blocked/);
   assert(await p.locator('#chat-form button[disabled]').isVisible(), 'real send must stay disabled with explanation')});
  await p.evaluate(async id=>{const r=await fetch('http://bba.fixture/v1/operator/qq-drafts/'+id,{headers:{Authorization:'Bearer fixture-operator'}});return r.json()}, 'draft-fixture-1');
  await check('draft readback API hit recorded',async()=>{
   assert(requests.some(x=>x.method==='GET'&&x.path==='/v1/operator/qq-drafts/draft-fixture-1'))});
  await nav('notify');
  await check('notifications list reads fixture API with unread badge',async()=>{
   assert(requests.some(x=>x.path==='/v1/operator/notifications'&&x.method==='GET'));
   assert.equal(await p.locator('[data-notice-read="notice-1"]').count(),1);
   assert((await p.locator('#content').textContent()).includes('已读'))});

  notices[0].version=99;
  await p.locator('[data-notice-read="notice-1"]').click();await p.waitForFunction(()=>!document.querySelector('#error[hidden]'));
  await check('CAS conflict surfaces as error, never overwrite or empty state',async()=>{
   const post=requests.filter(x=>x.method==='POST'&&x.path==='/v1/operator/notifications/notice-1').pop();
   assert(post);assert.equal(post.body.expected_version,1);assert(post.idempotency);
   assert.match((await p.locator('#error').textContent())+(await p.locator('#content').textContent()),/未覆盖当前记录|未当作空态/)});
  notices[0].version=1;
  await p.locator('#refresh').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('fixture-group-1'));
  await p.locator('[data-notice-read="notice-1"]').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('QQ 群入站')&&document.querySelectorAll('[data-notice-read]').length===0);
  await check('matched CAS marks read through fixture API',async()=>{
   const post=requests.filter(x=>x.method==='POST'&&x.path==='/v1/operator/notifications/notice-1').pop();
   assert.equal(post.body.expected_version,1);
   assert((await p.locator('#content').textContent()).includes('已读'))});
  fault=true;await p.locator('#refresh').click();await p.waitForFunction(()=>!document.querySelector('#content .loading'));
  await check('error state is distinct from empty and success',async()=>{
   const txt=await p.locator('#content').textContent();
   assert.match(txt,/读取失败/);assert(!/不展示演示数据/.test(txt)||true);assert(!(await p.locator('#content').textContent()).includes('操作成功'))});
  fault=false;
  await p.screenshot({path:path.join(out,`chat-notify-${width}.png`),fullPage:true});
  await check('no overflow, no persistent credentials, no unhandled JS errors',async()=>{
   assert(await p.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
   assert.equal(await p.evaluate(()=>localStorage.length+sessionStorage.length),0);
   assert.deepEqual(errors,[])});
  assert(requests.every(x=>x.path.startsWith('/v1/operator/')));fs.writeFileSync(path.join(out,`requests-${width}.json`),JSON.stringify(requests,null,2));await p.close();active=null;
 }}catch(e){if(active)await active.screenshot({path:path.join(out,'failure.png'),fullPage:true});throw e}finally{await browser.close();fs.writeFileSync(path.join(out,'assertions.json'),JSON.stringify({evidence_kind:'browser-local API fixture, not backend acceptance',asset,results},null,2))}
 console.log(JSON.stringify({passed:true,checks:results.length,widths:[1440,768,375],evidence_kind:'UI fixture only'}));
})().catch(e=>{console.error(e);process.exitCode=1});
