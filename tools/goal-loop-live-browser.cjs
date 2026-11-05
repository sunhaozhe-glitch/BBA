/* Opt-in browser integration with tools/goal-loop-fixture.py, no API mocks.
 * node tools/goal-loop-live-browser.cjs <fixture-config.json> <evidence-directory>
 * Uses existing Playwright/Chrome only; reads private fixture files, never logs tokens. */
'use strict';
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
let playwright;
for(const candidate of [process.env.BBA_TEST_PLAYWRIGHT,'playwright','/home/sun/.hermes/hermes-agent/node_modules/playwright'].filter(Boolean)){
 try{playwright=require(candidate);break;}catch(e){if(e.code!=='MODULE_NOT_FOUND')throw e;}
}
if(!playwright)throw Error('Set BBA_TEST_PLAYWRIGHT to an existing installation.');
(async()=>{
 const configPath=path.resolve(process.argv[2]),config=JSON.parse(fs.readFileSync(configPath,'utf8'));
 const origin=new URL(config.origin);assert.equal(origin.protocol,'http:');assert.equal(origin.hostname,'127.0.0.1');
 const privateFile=path.resolve(config.token_file);assert.equal(path.dirname(privateFile),path.dirname(configPath));
 for(const file of [configPath,privateFile]){const st=fs.lstatSync(file);assert(st.isFile());assert.equal(st.mode&0o777,0o600);}
 const token=fs.readFileSync(privateFile,'utf8').trim(),out=path.resolve(process.argv[3]);fs.mkdirSync(out,{recursive:true});
 const browser=await playwright.chromium.launch({headless:true,executablePath:process.env.BBA_TEST_CHROME||'/opt/google/chrome/chrome',args:['--no-sandbox']});
 const records=[];
 try{
  for(const width of [1440,375]){
   const page=await browser.newPage({viewport:{width,height:1000}});page.setDefaultTimeout(12000);
   const errors=[],mutations=[];page.on('pageerror',e=>errors.push(String(e)));
   await page.route('**/*',async route=>{
    const req=route.request(),url=new URL(req.url());
    if(url.origin!==origin.origin){errors.push('external request blocked');return route.abort();}
    if(req.method()==='POST'){
     mutations.push(url.pathname);
     if(!/^\/v1\/operator\/goal-runs(?:\/[^/]+\/(?:fixture-windows|confirm|stop))?$/.test(url.pathname)){errors.push('unexpected mutation blocked');return route.abort();}
    }
    return route.continue();
   });
   await page.goto(origin.origin+'/product/chat');await page.locator('#credential').fill(token);await page.locator('#connect-form').dispatchEvent('submit');
   await page.waitForFunction(()=>!!document.querySelector('#goal-start')&&!document.querySelector('#goal-loop [data-goal-loading]'));
   await page.locator('#goal-start [name=thread_id]').fill('browser-'+Date.now()+'-'+width);
   await page.locator('#goal-start [name=group_id]').fill(config.group_id);
   await page.locator('#goal-start [name=goal]').fill('分析指定测试群的近期需求，证据不足时列出信息缺口。');
   await page.locator('#goal-start [name=planner]').selectOption(config.planner_ref.id);
   await page.locator('#goal-start [name=analyst]').selectOption(config.analyst_ref.id);
   await page.locator('#goal-start button[type=submit]').click();
   const stateIs=state=>page.waitForFunction(s=>!document.querySelector('#goal-loop [data-goal-loading]')&&!document.querySelector('#goal-loop [data-goal-pending]')&&document.querySelector('#goal-run-state')?.textContent.includes(s),state);
   await stateIs('WAITING_INPUT');
   await page.locator('[data-goal-window=sparse]').click();await stateIs('WAITING_INPUT');
   const sparse=await page.locator('#goal-loop').textContent();assert.match(sparse,/请补充预算/);assert.match(sparse,/请补充时间/);
   await page.locator('[data-goal-window=complete]').click();await stateIs('AWAITING_CONFIRMATION');
   const complete=await page.locator('#goal-loop').textContent();assert.match(complete,/复核上一轮缺口/);assert.match(complete,/资产图/);assert.match(complete,/探索图/);
   await page.screenshot({path:path.join(out,`ready-${width}.png`),fullPage:true});
   await page.locator('[data-goal-window=expired]').click();await page.waitForFunction(()=>document.querySelector('#goal-error')?.textContent.includes('WINDOW_EXPIRED'));
   await page.locator('#goal-confirm').click();await stateIs('CONFIRMED');
   assert.equal(errors.length,0,errors.join('\n'));assert.equal(mutations.length,5);
   records.push({width,ok:true,final_state:'CONFIRMED',checks:['real HTTP run','two evidence windows','persisted replan reason','expired rejection','operator confirmation','same-origin only'],mutations});
   await page.close();
  }
 }finally{await browser.close();}
 fs.writeFileSync(path.join(out,'assertions.json'),JSON.stringify({passed:true,evidence_kind:'isolated real server + PostgreSQL + browser; deterministic fixture',records},null,2));
 console.log(JSON.stringify({passed:true,widths:[1440,375],evidence_kind:'isolated actual HTTP UI'}));
})().catch(e=>{console.error(String(e));process.exitCode=1;});
