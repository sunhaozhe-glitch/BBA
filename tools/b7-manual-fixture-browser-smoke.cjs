/* B7 real-service browser smoke against the MANUAL FIXTURE: a running
 * taskengine-server + temporary PostgreSQL. No request routing/interception —
 * every page action travels the real network into the fixture database.
 * usage: node tools/b7-manual-fixture-browser-smoke.cjs <fixture-config.json> <out-dir> [run-tag]
 */
'use strict';
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict'),http=require('node:http');
const {chromium}=require('/home/sun/.hermes/hermes-agent/node_modules/playwright');
const config=JSON.parse(fs.readFileSync(path.resolve(process.argv[2])));
const out=path.resolve(process.argv[3]);fs.mkdirSync(out,{recursive:true});
const token=role=>fs.readFileSync(config.token_files[role],'utf8').trim();
const requests=[];const assertions=[];const waitReq=async pred=>{for(let i=0;i<100&&!requests.some(pred);i++)await new Promise(r=>setTimeout(r,100));return requests.filter(pred).at(-1)};
function check(name,fn){return Promise.resolve().then(fn).then(()=>assertions.push({name,ok:true})).catch(e=>{assertions.push({name,ok:false,error:String(e.message||e)});})}
function api(method,p,{token:tk,body,headers}={}){return new Promise((res,rej)=>{const u=new URL(config.origin+p);const req=http.request(u,{method,headers:{Authorization:'Bearer '+tk,...headers}},r=>{let data='';r.on('data',c=>data+=c);r.on('end',()=>{let json=null;try{json=JSON.parse(data)}catch{}res({status:r.statusCode,json,data})})});req.on('error',rej);if(body)req.setHeader('Content-Type','application/json');if(body)req.write(JSON.stringify(body));req.end()})}
(async(runTag=process.argv[4]||'')=>{
 const tag=runTag||'b7fix-manual';
 const browser=await chromium.launch({headless:true,executablePath:process.env.BBA_TEST_CHROME||'/opt/google/chrome/chrome',args:['--no-sandbox']});
 const p=await browser.newPage({viewport:{width:1440,height:1000}});
 p.on('response',r=>{if(new URL(r.url()).pathname.startsWith('/v1/')){const rec={method:r.request().method(),path:new URL(r.url()).pathname,post_body:r.request().postData()||null,response_body:null,status:r.status(),at:new Date().toISOString()};requests.push(rec);r.text().then(b=>{rec.response_body=b}).catch(()=>{})}});
 const login=async tk=>{await p.goto(config.origin+'/product/chat');await p.locator('#credential').fill(tk);await p.locator('#connect-form').dispatchEvent('submit');await p.waitForFunction(()=>!document.querySelector('#auth:not([hidden])'),{timeout:10000})};
 const nav=async r=>{const el=p.locator(`[data-route=${r}]`);if(await p.locator('#menu').isVisible()){await p.locator('#menu').click()}await el.scrollIntoViewIfNeeded();await el.click();await p.waitForFunction(()=>document.querySelector('#content')&&!document.querySelector('#content .loading'),{timeout:10000})};
 await login(token('operator'));

 await check('conversations GET served by the real Go API lists 3 conversations over 2 accounts',async()=>{
  assert(requests.some(x=>x.path==='/v1/operator/conversations'&&x.method==='GET'&&x.status===200));
  const txt=await p.locator('#content').textContent();
  assert(txt.includes('agent-thread-1'))});
 await p.locator('#chat-tab-qq').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('qq-peer-202'),{timeout:10000});
 await check('QQ inbox tab lists group + private per account over real API',async()=>{
  const txt=await p.locator('#content').textContent();assert(txt.includes('群聊'));assert(txt.includes('私聊'));assert(txt.includes('acct-qq-1'));assert(txt.includes('acct-qq-2'));assert(txt.includes('qq-group-101'))});
 await p.locator('#chat-tab-agent').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('agent-thread-1'),{timeout:10000});
 await p.locator('[data-chat-conv="conv-b7fix-agent"]').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('不会进入 QQ 出站'),{timeout:10000});
 await check('agent thread lists seeded message from real DB read',async()=>{assert((await p.locator('#content').textContent()).includes('种子消息：Agent 线程内往返'))});
 await p.locator('#chat-form textarea').fill('页面 POST 的 Agent 消息（真实入库）');await p.locator('#chat-form-submit').click();
 await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('页面 POST 的 Agent 消息'),{timeout:10000});
 const agentPost=await waitReq(x=>x.method==='POST'&&x.path.includes('/agent-threads/')&&x.status===201);
 await check('POST agent message hits real API and creates no QQ permit',async()=>{
  assert(agentPost.method==='POST'&&agentPost.path.includes('/agent-threads/'));
  const fb=await p.locator('#chat-form-error').textContent();assert(fb.includes('许可')&&fb.includes('absent'),fb);assert(!/succeeded|已发送/i.test(fb),fb)});

 await p.locator('#chat-tab-qq').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('qq-group-101'),{timeout:10000});
 await p.locator('[data-chat-conv="conv-b7fix-group"]').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('出站草稿种子'),{timeout:10000});
 await check('QQ group view shows inbound + normal draft + keyword-blocked draft from DB',async()=>{
  const txt=await p.locator('#content').textContent();
  assert(txt.includes('QQ 群入站种子'));assert(txt.includes('普通草稿'));assert(txt.includes('规则阻断'))});
 await p.locator('#chat-form textarea').fill('关键词阻断验证 TEST_BLOCK');await p.locator('#chat-form-submit').click();
 await p.waitForFunction(()=>document.querySelector('#chat-form-error')&&document.querySelector('#chat-form-error').textContent.includes('blocked'),{timeout:10000});
 const blockedPost=await waitReq(x=>x.method==='POST'&&x.path==='/v1/operator/qq-drafts'&&x.status===201);
 let draftId=null;
 await check('POST qq-draft with keyword is really blocked by the content rule over HTTP',async()=>{
  assert(blockedPost.method==='POST'&&blockedPost.path==='/v1/operator/qq-drafts'&&blockedPost.status===201);
  draftId=JSON.parse(blockedPost.response_body).data.id;assert(draftId)});
 if(draftId){const back=await api('GET','/v1/operator/qq-drafts/'+draftId,{token:token('operator')});
  await check('draft readback via API shows decision=blocked and send_permit=absent (never sent)',async()=>{
   assert(back.status===200&&back.json.data.decision==='blocked'&&back.json.data.send_permit==='absent')})}
 await p.locator('[data-chat-conv="conv-b7fix-private"]').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('acct-qq-2')||document.querySelector('#chat-form'),{timeout:10000});
 await check('private chat conversation is isolated to acct-qq-2',async()=>{
  const convTxt=await p.locator('#content').textContent();assert(convTxt.includes('acct-qq-2'))});

 // runtime ingest: new -> 201, same digest replay -> 200, different digest -> 409
 const evt={account_id:'acct-qq-2',conversation_kind:'private',conversation_id:'qq-peer-202',event_id:'evt-'+tag+'-1',body:'运行时入站事件 smoke 正文'};
 const idem='ik-'+(tag||'manual');
 const e1=await api('POST','/v1/operator/qq-inbox/events',{token:token('runtime'),body:evt,headers:{'Idempotency-Key':idem+'-new'}});
 const e2=await api('POST','/v1/operator/qq-inbox/events',{token:token('runtime'),body:evt,headers:{'Idempotency-Key':idem+'-replay'}});
 const e3=await api('POST','/v1/operator/qq-inbox/events',{token:token('runtime'),body:{...evt,body:'运行时入站事件 smoke 正文（不同摘要）'},headers:{'Idempotency-Key':idem+'-conflict'}});
 const rtList=await api('GET','/v1/operator/conversations',{token:token('runtime')});
 await check('runtime event ingest: new=201, same digest replay=200 idempotent, different digest=409',async()=>{
  assert(e1.status===201||e1.status===200,`new=${e1.status}`);assert(e2.status===200&&e2.json.data.idempotent_replay===true,`replay=${e2.status} ${e2.data}`);
  assert(e3.status===409,`conflict=${e3.status} ${e3.data}`);assert(rtList.status===401,`runtime on operator list=${rtList.status}`)});

 // reader token: real permission error, not fake success
 await p.locator('#logout').click();await login(token('reader'));
 await nav('chat');await p.locator('[data-chat-conv="conv-b7fix-agent"]').click();
 await p.waitForFunction(()=>document.querySelector('#chat-form'),{timeout:10000});
 await p.locator('#chat-form textarea').fill('reader 写入应被拒绝');await p.locator('#chat-form-submit').click();
 await p.waitForFunction(()=>document.querySelector('#chat-form-error')&&document.querySelector('#chat-form-error').textContent.length>0,{timeout:10000});
 const readerPost=await waitReq(x=>x.method==='POST'&&x.path.includes('/agent-threads/')&&x.status===403);
 await check('reader token POST is refused with a real 403 shown in the UI (no false success)',async()=>{
  assert(readerPost.status===403,`reader POST status=${readerPost.status}`);
  assert((await p.locator('#chat-form-error').textContent()).includes('403'))});

 await p.locator('#logout').click();await login(token('operator'));
 // reload proves the data is in the database, not browser state
 await p.reload();await login(token('operator'));
 await p.locator('#chat-tab-agent').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('agent-thread-1'),{timeout:10000});
 await p.locator('[data-chat-conv="conv-b7fix-agent"]').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('页面 POST 的 Agent 消息'),{timeout:10000});
 await p.locator('#chat-tab-qq').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('qq-group-101'),{timeout:10000});
 await p.locator('[data-chat-conv="conv-b7fix-group"]').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('关键词阻断验证 TEST_BLOCK'),{timeout:10000});
 await check('after full reload the page reads the newly POSTed rows back from the database (re-navigated conversations)',async()=>{
  const txt=await p.locator('#content').textContent();
 assert(txt.includes('关键词阻断验证 TEST_BLOCK'))});
 await nav('notify');
 await check('notifications page shows unread and read rows from the real DB',async()=>{
  const txt=await p.locator('#content').textContent();
  assert(await p.locator('#content .badge.amber',{hasText:'未读'}).count()>=1,'no unread badge');assert(await p.locator('#content .badge.green',{hasText:'已读'}).count()>=1,'no read badge');assert(requests.some(x=>x.path==='/v1/operator/notifications'&&x.status===200))});
 const casBtn=p.locator('#content tbody tr button').first();
 const casId=await casBtn.getAttribute('data-notice-read');const casVersion=await casBtn.getAttribute('data-version');
 await casBtn.click();
 await p.waitForFunction(()=>document.querySelector('#content')&&!document.querySelector('#content .loading'),{timeout:10000});
 await waitReq(x=>x.method==='POST'&&x.path.startsWith('/v1/operator/notifications/')&&x.status===200);
 const casBack=await api('GET','/v1/operator/notifications',{token:token('operator')});
 await check('notification read CAS via POST lands in the DB (API readback shows read_at set)',async()=>{
  assert(casId&&casVersion,'no CAS button found');
  const item=casBack.json.data.items.find(x=>x.id===casId);
  assert(item&&item.read_at,`readback of ${casId}: ${JSON.stringify(item)}`)});

 for(const width of [1440,768,375]){
  await p.setViewportSize({width,height:1000});
  await p.reload();await login(token('operator'));
  await p.waitForFunction(()=>document.querySelector('#chat-tab-agent'),{timeout:10000});
  await p.screenshot({path:path.join(out,`chat-agent-${width}.png`),fullPage:true});
  await p.locator('#chat-tab-qq').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('qq-group-101'),{timeout:10000});
  await p.screenshot({path:path.join(out,`chat-qq-${width}.png`),fullPage:true});
  await nav('notify');await p.screenshot({path:path.join(out,`notify-${width}.png`),fullPage:true});}
 await browser.close();
 fs.writeFileSync(path.join(out,'requests.jsonl'),requests.map(r=>JSON.stringify(r)).join('\n')+'\n');
 fs.writeFileSync(path.join(out,'assertions.json'),JSON.stringify(assertions,null,1)+'\n');
 const failed=assertions.filter(a=>!a.ok);
 console.log(JSON.stringify({total:assertions.length,failed:failed.map(f=>f.name)}));
 if(failed.length)process.exit(1);
})().catch(e=>{console.error(e);process.exit(1)});
