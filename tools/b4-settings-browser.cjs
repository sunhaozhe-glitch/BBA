const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require('/home/sun/.hermes/hermes-agent/node_modules/playwright');
(async()=>{
 const out=process.argv[2],phase=process.argv[3],cfg=JSON.parse(fs.readFileSync(path.join(out,'fixture-config.json'))),tokens=JSON.parse(fs.readFileSync(path.join(out,'private/tokens.json'))),base=cfg.origin,endpoint='/v1/operator/system/work-slots';
 const browser=await chromium.launch({headless:true,executablePath:'/opt/google/chrome/chrome',args:['--no-sandbox']});const results=[],network=[],errors=[];
 const ctx=await browser.newContext({viewport:{width:1440,height:1000}});const page=await ctx.newPage();
 page.on('pageerror',e=>errors.push(e.message));page.on('response',r=>{if(r.url().includes('/v1/operator/system/'))network.push({method:r.request().method(),path:new URL(r.url()).pathname,status:r.status()})});
 const check=(label,data={})=>results.push({label,...data});
 async function login(token){await page.locator('#credential').fill(token);await page.locator('#connect-form button').click();await page.waitForFunction(()=>!!document.querySelector('#work-slots'));}
 async function request(token,method='GET',body){const res=await ctx.request.fetch(base+endpoint,{method,headers:{Authorization:'Bearer '+token,'Content-Type':'application/json'},data:body});return {status:res.status(),body:await res.json()}}
 async function save(n){await page.locator('#work-slots').fill(String(n));await page.locator('#work-save').click();await page.waitForFunction(()=>document.querySelector('#work-settings-message')?.textContent.includes('保存成功'));}
 try{
  await page.goto(base+'/product/settings');if(phase==='unavailable'){await page.locator('#credential').fill(tokens.admin);await page.locator('#connect-form button').click();await page.waitForFunction(()=>document.querySelector('#content').textContent.includes('读取不可用'));assert.equal(await page.locator('#work-slots').count(),0);assert.match(await page.locator('#content').innerText(),/503/);check('database_unavailable_no_fallback_or_save');return}await login(tokens.admin);
  if(phase==='save1'){assert.equal(await page.locator('#work-slots').inputValue(),'1');assert.match(await page.locator('#work-source').innerText(),/CLI/);await save(1);check('page_save_1_then_GET',{...(await request(tokens.admin)).body.data});}
  else if(phase==='save2'){assert.equal(await page.locator('#work-slots').inputValue(),'1');await save(2);await page.reload();await login(tokens.admin);assert.equal(await page.locator('#work-slots').inputValue(),'2');check('page_save_2_refresh_readback',{...(await request(tokens.admin)).body.data});}
  else if(phase==='verify'){
   assert.equal(await page.locator('#work-slots').inputValue(),'2');assert.match(await page.locator('#work-runtime').innerText(),/未启用/);check('process_restart_saved2_beats_cli8',{...(await request(tokens.admin)).body.data});
   // Read each actual consumer task through the ordinary detail UI. The
   // workspace default is 2, while the first task must still display 1.
   await page.locator('[data-route="tasks"]').click();
   await page.waitForSelector('[data-task]');
   const taskIds=await page.locator('[data-task]').evaluateAll(xs=>xs.map(x=>x.dataset.task));
   assert.equal(taskIds.length,2);
   const actualSlots=[];
   for(const id of taskIds){
    const res=await ctx.request.get(base+'/v1/operator/system/task-work-slots/'+encodeURIComponent(id),{headers:{Authorization:'Bearer '+tokens.admin}});
    assert.equal(res.status(),200);const snap=(await res.json()).data;
    await page.locator('[data-task]').evaluateAll((xs,id)=>xs.find(x=>x.dataset.task===id).click(),id);
    await page.waitForFunction(n=>document.querySelector('#detail-body')?.textContent.includes(n+' 个 / 任务（已固化）'),snap.work_slots);
    const text=await page.locator('#detail-body').innerText();actualSlots.push(snap.work_slots);
    await page.screenshot({path:path.join(out,`task-snapshot-${snap.work_slots}.png`),fullPage:true});
    check('task_detail_actual_frozen_slots',{id,slots:snap.work_slots,text});
    await page.locator('#close-detail').click();
   }
   assert.deepEqual(actualSlots.sort(),[1,2]);
   await page.locator('[data-route="settings"]').click();await page.waitForSelector('#work-slots');
   for(const [w,h] of [[1440,1000],[1024,900],[390,844]]){await page.setViewportSize({width:w,height:h});await page.screenshot({path:path.join(out,`settings-${w}.png`),fullPage:true});assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'horizontal overflow '+w);check('viewport',{width:w,overflow:false});}
   await page.setViewportSize({width:1440,height:1000});
   // A competing real API update makes the form stale: preserve 409.
   const prior=await request(tokens.admin);assert.equal((await request(tokens.admin,'PUT',{work_slots:2,expected_version:prior.body.data.version})).status,200);
   await page.locator('#work-slots').fill('3');await page.locator('#work-save').click();await page.waitForFunction(()=>document.querySelector('#work-settings-message')?.textContent.includes('409'));check('real409_preserved');assert.equal((await request(tokens.admin)).body.data.work_slots,2);
   await page.locator('#logout').click();await login(tokens.reader);assert(await page.locator('#work-save').isDisabled());assert(await page.locator('#work-slots').isDisabled());assert.equal((await request(tokens.reader,'PUT',{work_slots:4,expected_version:0})).status,403);await page.screenshot({path:path.join(out,'settings-readonly.png'),fullPage:true});check('readonly_UI_and_API403');
   await page.locator('#logout').click();await login(tokens.other);let p=await request(tokens.other);assert.equal(p.body.data.source,'cli_fallback');assert.equal((await request(tokens.other,'PUT',{work_slots:1,expected_version:0})).status,200);await page.locator('#refresh').click();await page.waitForFunction(()=>document.querySelector('#work-slots')?.value==='1');check('other_workspace1_main2',{main:(await request(tokens.admin)).body.data,other:(await request(tokens.other)).body.data});
   // Delay consumption of ORIGINAL real responses, not route.fulfill or fake API.
   // The response has completed before logout aborts; it can arrive late at UI.
   for(const status of [200,409])for(const switchAccount of [false,true]){
    await page.locator('#logout').click();await login(tokens.admin);
    if(status===409){p=await request(tokens.admin);assert.equal((await request(tokens.admin,'PUT',{work_slots:2,expected_version:p.body.data.version})).status,200);}
    await page.evaluate(()=>{const original=window.fetch;window.b4Gate=null;window.b4Ready=null;window.fetch=async (...args)=>{const res=await original(...args);if(args[1]?.method==='PUT'&&String(args[0]).includes('/system/work-slots')){window.fetch=original;await res.clone().text();window.b4Ready={status:res.status};await new Promise(resolve=>window.b4Gate=resolve)}return res}});
    await page.locator('#work-save').click();await page.waitForFunction(()=>window.b4Ready!==null);assert.equal(await page.evaluate(()=>window.b4Ready.status),status);
    await page.locator('#logout').click();assert.equal(await page.locator('#credential').inputValue(),'');
    if(switchAccount)await login(tokens.other);
    const before=await page.locator('#content').innerText();await page.evaluate(()=>window.b4Gate());await page.waitForTimeout(120);assert.equal(await page.locator('#content').innerText(),before);assert.equal(await page.locator('#work-settings-message').count(),switchAccount?1:0);if(switchAccount)assert.equal(await page.locator('#work-slots').inputValue(),'1');check('late_original_response_isolated',{status,switchAccount});
   }
   await page.locator('#logout').click();await page.locator('#credential').fill(tokens.none);await page.locator('#connect-form button').click();await page.waitForFunction(()=>document.querySelector('#content').textContent.includes('读取不可用'));assert.equal(await page.locator('#work-save').count(),0);check('missing_read_permission_no_fallback');
   assert(errors.length===0,JSON.stringify(errors));
  }else if(phase==='unavailable'){
   // login helper expects successful input: this phase uses direct auth submission.
  }
 }finally{fs.writeFileSync(path.join(out,'browser-'+phase+'.json'),JSON.stringify({phase,checks:results,network,errors,response_substitution:false},null,2));await browser.close()}
})().catch(e=>{console.error(e);process.exit(1)});
