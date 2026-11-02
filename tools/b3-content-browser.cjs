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
  let rules=[],history=[],revision=0,delay=null,fault=false,notes=[];
  const review={id:'review-fixture',decision:'blocked',account_id:'fixture-account',conversation_kind:'group',conversation_id:'fixture-group',send_id:'fixture-send',task_id:'',payload_digest:'sha256:fixture-redacted',rules_version:1,evidence:[{rule_id:'fixture-rule',start:0,end:4}],created_at:'2026-10-31T00:00:00Z'};
  await p.route('http://bba.fixture/**',async route=>{
   const req=route.request(),url=new URL(req.url());
   if(!url.pathname.startsWith('/v1/')){const file=url.pathname.endsWith('app.js')?'app.js':url.pathname.endsWith('app.css')?'app.css':'index.html';return route.fulfill({body:fs.readFileSync(path.join(asset,file)),contentType:file.endsWith('.js')?'application/javascript':file.endsWith('.css')?'text/css':'text/html'})}
   const method=req.method(),reader=req.headers().authorization==='Bearer fixture-reader',body=req.postDataJSON();requests.push({path:url.pathname,query:url.search,method,reader,body});
   const reply=(status,data)=>route.fulfill({status,json:status<400?{data}:{error:{code:data}}});
   if(delay&&delay.match(url,method)){const d=delay;delay=null;d.started();await d.wait;return reply(d.status,d.data)}
   if(fault)return reply(503,'FIXTURE_UNAVAILABLE');
   if(url.pathname.includes('content-rules')){
    if(url.pathname.endsWith('/preview'))return reply(200,{decision:rules.some(x=>x.enabled&&String(body.text).includes(x.expression))?'blocked':'allowed',rules_version:revision,grants_send_authority:false});
    if(url.pathname.endsWith('/history'))return reply(200,{items:history});
    if(method==='GET')return reply(200,{items:rules,version:revision,can_configure:!reader});
    if(reader)return reply(403,'FORBIDDEN');
    const id=url.pathname.split('/')[4],old=rules.find(x=>x.id===id);
    if(id&&(!old||body.expected_version!==old.version))return reply(409,'VERSION_CONFLICT');
    const value={...body,id:id||'fixture-rule',version:(old?.version||0)+1,updated_by:'fixture-admin',updated_at:new Date().toISOString()};delete value.expected_version;
    if(method==='DELETE'){rules=rules.filter(x=>x.id!==id);history.push({...value,deleted:true})}else{rules=old?rules.map(x=>x.id===id?value:x):[...rules,value];history.push({...value})}revision++;return reply(200,value);
   }
   if(url.pathname.includes('content-reviews')){
    if(url.pathname.endsWith('/notes')){if(reader)return reply(403,'FORBIDDEN');notes.push({note:body.note,updated_by:'fixture-admin'});return reply(200,{saved:true})}
    if(url.pathname.endsWith('/review-fixture'))return reply(200,{...review,notes,can_note:!reader});
    return reply(200,{items:[review],offset:Number(url.searchParams.get('offset')||0),has_more:!url.searchParams.get('offset')||url.searchParams.get('offset')==='0',next_offset:20});
   }
   return reply(404,'UNEXPECTED_FIXTURE_PATH');
  });
  const check=async(name,fn)=>{await fn();results.push({width,name,ok:true})};
  const auth=async token=>{await p.locator('#credential').evaluate((el,t)=>el.value=t,token);await p.locator('#connect-form').dispatchEvent('submit');await p.waitForSelector('#rule-preview,#review-filter')};
  const close=async()=>p.locator('#close-detail').click();
  const saved=async()=>p.waitForFunction(()=>!document.querySelector('#details').open&&!document.querySelector('.loading'));
  const nav=async route=>{if(await p.locator('#menu').isVisible())await p.locator('#menu').click();await p.locator(`[data-route=${route}]`).click();await p.waitForSelector(route==='intercept'?'#rule-preview':'#review-filter')};
  const delayed=(match,status,data)=>{let started,release;const begun=new Promise(r=>started=r),wait=new Promise(r=>release=r);delay={match,status,data,started,wait};return {begun,release}};
  await p.goto('http://bba.fixture/product/intercept');await auth('fixture-admin');
  await check('empty rules are explicit, no automatic send',async()=>{assert.match(await p.locator('#content').textContent(),/暂无拦截规则/);assert.match(await p.locator('#content').textContent(),/不自动采集或发送/)});
  await p.locator('#rule-new').click();const f=p.locator('#rule-form');assert.equal(await f.locator('[name=enabled]').isChecked(),false);await f.locator('[name=name]').fill('隔离测试规则');await f.locator('[name=expression]').fill('TEST_BLOCK');await f.locator('[name=group]').fill('UI fixture');await f.locator('[name=enabled]').check();await f.locator('button').click();await saved();
  await check('create enabled rule with expected_version=0',async()=>{assert.equal(rules[0].version,1);assert.equal(requests.find(x=>x.method==='POST'&&x.path.endsWith('content-rules')).body.expected_version,0)});
  await p.locator('[data-rule-edit]').click();await f.locator('[name=priority]').fill('200');await f.locator('button').click();await saved();assert.equal(rules[0].priority,200);
  await p.locator('[data-rule-edit]').click();rules[0].version++;await f.locator('button').click();await p.waitForFunction(()=>document.querySelector('#content-form-error').textContent.includes('409'));
  await check('409 keeps editor and never overwrites',async()=>{assert(await p.locator('#details').evaluate(x=>x.open));assert.equal(rules[0].version,3)});await close();await p.locator('#refresh').click();await p.locator('[data-rule-edit]').waitFor();
  await p.locator('[data-rule-history]').click();await p.waitForSelector('#detail-body details');await check('history is read only',async()=>{assert.equal(await p.locator('#detail-body button').count(),0);assert.equal(await p.locator('#detail-body details').count(),2)});await close();
  await p.locator('#rule-preview').click();await p.locator('#preview-form textarea').fill('TEST_BLOCK');await p.locator('#preview-form button').click();await p.waitForFunction(()=>document.querySelector('#preview-result').textContent.includes('blocked'));await check('preview cannot grant send authority',async()=>assert.match(await p.locator('#preview-result').textContent(),/"grants_send_authority": false/));await close();
  await p.locator('[data-rule-toggle]').click();await p.locator('#rule-confirm').click();await saved();assert.equal(rules[0].enabled,false);
  await p.screenshot({path:path.join(out,`rules-${width}.png`),fullPage:true});
  await auth('fixture-reader');await check('reader cannot mutate rule',async()=>{assert.equal(await p.locator('#rule-new,[data-rule-toggle],[data-rule-delete]').count(),0);await p.locator('[data-rule-edit]').click();assert(await f.locator('fieldset').evaluate(x=>x.disabled));assert.equal(await f.locator('button').count(),0)});await close();
  await auth('fixture-admin');const lateHistory=delayed((u,m)=>u.pathname.endsWith('/history'),200,{items:[{version:999,name:'LATE_SECRET'}]});await p.locator('[data-rule-history]').click();await lateHistory.begun;await close();await nav('approvals');lateHistory.release();await p.waitForTimeout(80);assert.equal(await p.locator('#detail-body').textContent(),'');results.push({width,name:'late history cannot refill closed dialog across navigation',ok:true});
  await p.locator('#review-filter [name=account]').fill('fixture-account');await p.locator('#review-filter [name=send]').fill('fixture-send');await p.locator('#review-filter button').click();await p.locator('[data-review]').waitFor();assert(requests.some(x=>x.query.includes('account_id=fixture-account')&&x.query.includes('send_id=fixture-send')));
  await p.locator('#reviews-next').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('已偏移 20'));assert(await p.locator('#reviews-next').isDisabled());await p.locator('#reviews-prev').click();await p.waitForFunction(()=>document.querySelector('#content').textContent.includes('已偏移 0'));
  await p.locator('[data-review]').click();await p.locator('#review-note').waitFor();assert.match(await p.locator('#detail-body').textContent(),/不重发/);await p.locator('#review-note textarea').fill('仅测试说明，不含原文');await p.locator('#review-note button').click();await saved();await p.locator('[data-review]').click();await p.locator('#review-note').waitFor();assert.match(await p.locator('#detail-body').textContent(),/仅测试说明，不含原文/);await p.screenshot({path:path.join(out,`review-detail-${width}.png`),fullPage:true});await close();
  await auth('fixture-reader');await p.locator('[data-review]').click();await p.waitForFunction(()=>document.querySelector('#detail-body').textContent.includes('只读权限'));assert.equal(await p.locator('#review-note').count(),0);await close();results.push({width,name:'review filters paging note persistence and reader lock',ok:true});
  await auth('fixture-admin');const lateReview=delayed((u,m)=>u.pathname.endsWith('/review-fixture'),200,{...review,notes:[{note:'LATE_REVIEW_SECRET',updated_by:'fixture'}],can_note:true});await p.locator('[data-review]').click();await lateReview.begun;await close();await nav('intercept');lateReview.release();await p.waitForTimeout(80);assert.equal(await p.locator('#detail-body').textContent(),'');results.push({width,name:'late review detail cannot refill after close and navigation',ok:true});
  await p.locator('#rule-preview').click();await p.locator('#preview-form textarea').fill('TEST_BLOCK');const latePreview=delayed((u,m)=>u.pathname.endsWith('/preview'),200,{decision:'LATE_PREVIEW_SECRET'});await p.locator('#preview-form button').click();await latePreview.begun;await close();await p.locator('[data-rule-edit]').click();latePreview.release();await p.waitForTimeout(80);assert.equal(await p.locator('#preview-result').count(),0);assert(!(await p.locator('#detail-body').textContent()).includes('LATE_PREVIEW_SECRET'));await close();results.push({width,name:'late preview cannot replace a newer rule editor',ok:true});
  await p.locator('[data-rule-edit]').click();const late401=delayed((u,m)=>m==='PUT',401,'UNAUTHORIZED');await f.locator('button').click();await late401.begun;await close();await auth('fixture-reader');late401.release();await p.waitForTimeout(80);assert.equal(await p.locator('#auth').isVisible(),false);assert.equal(await p.locator('#rule-new').count(),0);assert.equal(await p.locator('#detail-body').textContent(),'');results.push({width,name:'old save 401 cannot logout new reader session',ok:true});
  await auth('fixture-admin');fault=true;await p.locator('#refresh').click();await p.waitForSelector('#error:not([hidden])');assert.match(await p.locator('#content').textContent(),/读取失败不会当成空规则集或允许发送/);fault=false;await p.locator('#refresh').click();await p.locator('[data-rule-delete]').waitFor();await p.locator('[data-rule-delete]').click();await p.locator('#rule-confirm').click();await saved();assert.equal(rules.length,0);assert(history.some(x=>x.deleted));
  await nav('approvals');await p.screenshot({path:path.join(out,`reviews-${width}.png`),fullPage:true});await check('no overflow, no persistent credentials, no unhandled JS errors',async()=>{assert(await p.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));assert.equal(await p.evaluate(()=>localStorage.length+sessionStorage.length),0);assert.deepEqual(errors,[])});
  assert(requests.every(x=>x.path.startsWith('/v1/operator/content-')));fs.writeFileSync(path.join(out,`requests-${width}.json`),JSON.stringify(requests,null,2));await p.close();active=null;
 }}catch(e){if(active)await active.screenshot({path:path.join(out,'failure.png'),fullPage:true});throw e}finally{await browser.close();fs.writeFileSync(path.join(out,'assertions.json'),JSON.stringify({evidence_kind:'browser-local API fixture, not backend acceptance',asset,results},null,2))}
 console.log(JSON.stringify({passed:true,checks:results.length,widths:[1440,768,375],evidence_kind:'UI fixture only'}));
})().catch(e=>{console.error(e);process.exitCode=1});
