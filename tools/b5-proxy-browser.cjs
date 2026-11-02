const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require('/home/sun/.hermes/hermes-agent/node_modules/playwright');
(async()=>{
 const out=process.argv[2],phase=process.argv[3],cfg=JSON.parse(fs.readFileSync(path.join(out,'fixture-config.json'))),tokens=JSON.parse(fs.readFileSync(path.join(out,'private/tokens.json'))),base=cfg.origin,endpoint='/v1/operator/system/global-proxy';
 const browser=await chromium.launch({headless:true,executablePath:'/opt/google/chrome/chrome',args:['--no-sandbox']});const checks=[],network=[],errors=[];const ctx=await browser.newContext({viewport:{width:1440,height:1000}}),page=await ctx.newPage();
 page.on('pageerror',e=>errors.push(e.message));page.on('response',r=>{if(r.url().includes('/v1/operator/system/'))network.push({method:r.request().method(),path:new URL(r.url()).pathname,status:r.status()})});
 async function login(token){await page.locator('#credential').fill(token);await page.locator('#connect-form button').click();await page.waitForSelector('#proxy-save');}
 async function request(token,method='GET',data){const res=await ctx.request.fetch(base+endpoint,{method,headers:{Authorization:'Bearer '+token,'Content-Type':'application/json'},data});return {status:res.status(),body:await res.json()}}
 function body(p,extra={}){return {enabled:p.enabled,url:p.url,expected_version:p.version,secret_action:'keep',...extra}}
 async function save(){await page.locator('#proxy-save').click();await page.waitForFunction(()=>document.querySelector('#proxy-message')?.textContent.includes('保存成功'));}
 try{
 await page.goto(base+'/product/settings');await login(tokens.admin);
 if(phase==='save'){
  assert.equal((await request(tokens.admin)).body.data.version,0);await page.locator('#proxy-enabled').check();await page.locator('#proxy-url').fill('http://127.0.0.1:'+cfg.proxy_port);await page.locator('#proxy-action').selectOption('replace');await page.locator('#proxy-username').fill('fixture-proxy-user');await page.locator('#proxy-password').fill('fixture-proxy-password');await save();assert.equal(await page.locator('#proxy-password').inputValue(),'');assert.equal(await page.locator('#proxy-username').inputValue(),'');assert.match(await page.locator('#proxy-secret-state').innerText(),/已保存/);
  // Real configured and authorized model; not an arbitrary URL probe.
  const res=await ctx.request.post(base+'/v1/operator/models',{headers:{Authorization:'Bearer '+tokens.admin},data:{name:'B5 TLS fixture',model:'fixture',base_url:'https://model.bba.example:'+cfg.provider_port+'/v1',enabled:true,is_default:true,timeout_seconds:3,max_concurrent:3,expected_version:0,secret_action:'replace',api_key:'fixture-model-key'}});assert.equal(res.status(),200);
  await page.reload();await login(tokens.admin);assert.equal(await page.locator('#proxy-url').inputValue(),'http://127.0.0.1:'+cfg.proxy_port);checks.push({label:'page_save_realAPI_protected_store_refresh',data:(await request(tokens.admin)).body.data});
 }else if(phase==='verify'){
  let p=(await request(tokens.admin)).body.data;assert.equal(p.version,1);assert.equal(p.enabled,true);assert.equal(p.has_credentials,true);checks.push({label:'process_restart_keeps_browser_saved_proxy',data:p});
  for(const [width,height] of [[1440,1000],[1024,900],[390,844]]){await page.setViewportSize({width,height});await page.screenshot({path:path.join(out,'proxy-settings-'+width+'.png'),fullPage:true});assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'overflow '+width);checks.push({label:'responsive',width,overflow:false})}await page.setViewportSize({width:1440,height:1000});
  assert.equal((await request(tokens.admin,'PUT',body(p))).status,200);await page.locator('#proxy-save').click();await page.waitForFunction(()=>document.querySelector('#proxy-message')?.textContent.includes('409'));checks.push({label:'real409_no_auto_overwrite'});
  await page.locator('#refresh').click();await page.waitForFunction(()=>document.querySelector('#proxy-version')?.textContent==='v2');await page.locator('#proxy-action').selectOption('clear');await save();assert.match(await page.locator('#proxy-secret-state').innerText(),/未保存/);checks.push({label:'clear_credentials_page_and_API',data:(await request(tokens.admin)).body.data});
  await page.locator('#logout').click();await login(tokens.reader);assert(await page.locator('#proxy-save').isDisabled());assert.equal((await request(tokens.reader,'PUT',body((await request(tokens.reader)).body.data))).status,403);checks.push({label:'readonly_UI_and_API403'});
  await page.locator('#logout').click();await login(tokens.other);assert.equal((await request(tokens.other)).body.data.version,0);assert.equal(await page.locator('#proxy-url').inputValue(),'');checks.push({label:'workspace_isolation'});
  for(const status of [200,409])for(const switchAccount of [false,true]){
   await page.locator('#logout').click();await login(tokens.admin);p=(await request(tokens.admin)).body.data;if(status===409)assert.equal((await request(tokens.admin,'PUT',body(p))).status,200);
   await page.evaluate(()=>{const original=window.fetch;window.b5Gate=null;window.b5Ready=null;window.fetch=async(...args)=>{const res=await original(...args);if(args[1]?.method==='PUT'&&String(args[0]).includes('/system/global-proxy')){window.fetch=original;await res.clone().text();window.b5Ready={status:res.status};await new Promise(resolve=>window.b5Gate=resolve)}return res}});
   await page.locator('#proxy-save').click();await page.waitForFunction(()=>window.b5Ready!==null);assert.equal(await page.evaluate(()=>window.b5Ready.status),status);await page.locator('#logout').click();if(switchAccount)await login(tokens.other);const before=await page.locator('#content').innerText();await page.evaluate(()=>window.b5Gate());await page.waitForTimeout(100);assert.equal(await page.locator('#content').innerText(),before);checks.push({label:'late_original_response_isolated',status,switchAccount});
  }
  const read=await request(tokens.admin);fs.writeFileSync(path.join(out,'proxy-api-readback.json'),JSON.stringify(read,null,2));assert(!JSON.stringify(read).includes('fixture-proxy-'));
 }else throw Error('unknown phase');assert.equal(errors.length,0,JSON.stringify(errors));
 }finally{fs.writeFileSync(path.join(out,'browser-'+phase+'.json'),JSON.stringify({checks,network,errors,response_substitution:false},null,2));await browser.close()}
})().catch(e=>{console.error(e);process.exit(1)});
