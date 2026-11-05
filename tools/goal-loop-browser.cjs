/* Browser-local API fixture only: no backend, model, QQ or release acceptance.
 * node tools/goal-loop-browser.cjs [output-directory] [frontend-directory]
 * Uses an existing Playwright installation; never installs dependencies. */
'use strict';
const fs = require('node:fs'), path = require('node:path'), os = require('node:os'), assert = require('node:assert/strict');
let playwright;
for (const candidate of [process.env.BBA_TEST_PLAYWRIGHT, 'playwright', '/home/sun/.hermes/hermes-agent/node_modules/playwright'].filter(Boolean)) {
 try { playwright = require(candidate); break; } catch (e) { if (e.code !== 'MODULE_NOT_FOUND') throw e; }
}
if (!playwright) throw Error('Use an existing Playwright installation via BBA_TEST_PLAYWRIGHT.');
const root = path.resolve(__dirname, '..'), asset = path.resolve(process.argv[3] || path.join(root, 'frontend'));
const out = process.argv[2] ? path.resolve(process.argv[2]) : fs.mkdtempSync(path.join(os.tmpdir(), 'bba-goal-browser-'));
fs.mkdirSync(out, { recursive: true });
const results = [];
(async () => {
 const browser = await playwright.chromium.launch({ headless: true, executablePath: process.env.BBA_TEST_CHROME || '/opt/google/chrome/chrome', args: ['--no-sandbox'] });
 try {
  for (const width of [1440, 375]) {
   const page = await browser.newPage({ viewport: { width, height: 1000 } }), requests = [], errors = [];
   page.setDefaultTimeout(7000);
   page.on('pageerror', e => errors.push(String(e)));
   const model = { id: 'model-local', name: '已保存模型', enabled: true, version: 3 };
   const skill = { id: 'skill-local', name: '已保存技能', version: 2, enabled: true, published: true };
   const agents = ['planner', 'analyst'].map(id => ({ id, name: id, version: 4, enabled: true, model_id: model.id, skills: [{ id: skill.id, version: 2 }] }));
   const jobs = new Map();
   const receipt = i => ({ agent: agents[i], model, skills: [skill] });
   let enabled = true, runs = [], detailReads = 0, windows = 0, hold = null, pending = null, conflict = false, failList = false;
   let afterHold;
   const waitHeld = () => new Promise(resolve => { afterHold = resolve; });
   const release = async () => { const fn = pending; pending = null; if (fn) await fn(); };
   const graph = { assets: [{ id: 'asset-group', type: 'group', label: '单群测试资源' }], nodes: [{ id: 'plan-analyze', kind: 'analyst', label: '复核需求', state: 'WAITING_INPUT' }], edges: [], anchors: [{ node_id: 'plan-analyze', asset_id: 'asset-group' }] };
   await page.route('**/*', async route => {
    const req = route.request(), url = new URL(req.url());
    assert.equal(url.origin, 'http://bba.fixture', 'external network forbidden');
    if (!url.pathname.startsWith('/v1/')) {
     const file = url.pathname.endsWith('app.js') ? 'app.js' : url.pathname.endsWith('app.css') ? 'app.css' : 'index.html';
     return route.fulfill({ body: fs.readFileSync(path.join(asset, file)), contentType: file.endsWith('.js') ? 'application/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html' });
    }
    const body = req.postDataJSON(), method = req.method(), key = req.headers()['idempotency-key'];
    requests.push({ path: url.pathname, method, body, key });
    const reply = (data, meta = {}) => route.fulfill({ status: 200, json: { data, meta } });
    const reject = (code, status = 409) => route.fulfill({ status, json: { error: { code } } });
    if (url.pathname === '/v1/operator/conversations') return reply({ items: [{ id: 'qq-conv', kind: 'qq', conversation_kind: 'group', conversation_id: 'qq-inbox-preserved', version: 1 }] });
    if (url.pathname === '/v1/operator/conversations/qq-conv/messages') return reply({ items: [] });
    if (url.pathname === '/v1/operator/agents') return reply({ items: agents });
    if (url.pathname === '/v1/operator/models') return reply({ items: [model] });
    if (url.pathname === '/v1/operator/skills') return reply({ items: [skill] });
    if (url.pathname === '/v1/operator/goal-runs' && method === 'GET') {
     if (failList) return reject('FIXTURE_UNAVAILABLE', 503);
     const data = structuredClone(runs), meta = { fixture_enabled: enabled };
     if (hold === 'list') { hold = null; pending = () => reply(data, meta); afterHold(); return; }
     return reply(data, meta);
    }
    if (url.pathname === '/v1/operator/goal-runs' && method === 'POST') {
     assert(key); assert.equal(body.group_id, '100000000000000001'); assert.equal(typeof body.group_id, 'string'); assert.equal(body.planner_ref.version, 4); assert.equal(body.analyst_ref.version, 4);
     const r = { id: 'run-' + (runs.length + 1), ...body, state: 'WAITING_INPUT', version: 1, round: 0, reason: '等待新窗口', implementation: 'deterministic_fixture', configuration: { planner: receipt(0), analyst: receipt(1) }, memory: {}, rounds: [], evidence: [], graph };
     runs.push(r); return reply(r);
    }
    const match = url.pathname.match(/^\/v1\/operator\/goal-runs\/([^/]+)(?:\/(fixture-windows|stop|confirm))?$/);
    if (match) {
     const r = runs.find(x => x.id === match[1]); if (!r) return reject('NOT_FOUND', 404);
     if (method === 'GET') {
      detailReads++;
      const job = jobs.get(r.id);
      if (job && job.reads++ >= 2) { Object.assign(r, job.run); jobs.delete(r.id); } else if (job && job.reads === 2) r.state = 'RUNNING';
      const snap = structuredClone(r);
      if (hold === 'detail' || hold === 'unauthorized') { const unauthorized = hold === 'unauthorized'; hold = null; pending = () => unauthorized ? reject('UNAUTHENTICATED', 401) : reply({ ...snap, goal: 'STALE_DETAIL_MUST_NOT_RENDER' }); afterHold(); return; }
      return reply(snap);
     }
     assert(key);
     if (match[2] === 'fixture-windows') {
      assert.equal(body.group_id, r.group_id); assert(body.event_id); assert(body.messages[0].id);
      if (Date.parse(body.expires_at) <= Date.now()) return reject('WINDOW_EXPIRED');
      const before = structuredClone(r);
      windows++; r.version++; r.round++; r.state = windows === 1 ? 'RUNNING' : 'AWAITING_CONFIRMATION';
      r.reason = windows === 1 ? '缺少预算与时间，等待补充' : '新增预算与时间证据，计划调整为待确认';
      r.memory = { missing: windows === 1 ? ['预算', '时间'] : [] };
      r.evidence.push({ id: 'ev-' + windows, kind: windows === 1 ? 'raw' : 'model_judgment', text: body.messages[0].text + '<script>throw Error("unsafe")</script>', refs: ['synthetic:' + body.event_id + ':' + body.messages[0].id, body.messages[0].id], round: r.round, expires_at: body.expires_at });
      r.rounds.push({ number: r.round, state: 'COMPLETED', change_reason: r.reason, consumed_evidence_ids: r.evidence.map(e => e.id), steps: [{ id: 'step-' + windows, role: windows === 1 ? 'analyst' : 'planner', state: 'COMPLETED', attempt: 1, output: { reason: r.reason } }] });
      const processed = structuredClone(r); processed.state = windows === 1 ? 'WAITING_INPUT' : 'AWAITING_CONFIRMATION'; processed.version++;
      jobs.set(r.id, { run: processed, reads: 0 });
      Object.assign(r, before, { version: before.version + 1, state: 'WAITING_INPUT' });
      return reply(r);
     }
     assert.equal(body.expected_version, r.version);
     if (conflict) return reject('VERSION_CONFLICT');
     r.version++; r.state = match[2] === 'stop' ? 'STOPPED' : 'CONFIRMED';
     if (hold === 'control') { hold = null; const snap = structuredClone(r); pending = () => reply({ ...snap, goal: 'STALE_MUTATION_MUST_NOT_RENDER' }); afterHold(); return; }
     return reply(r);
    }
    return reject('UNEXPECTED_FIXTURE_PATH', 404);
   });
   const check = async (name, fn) => { await fn(); results.push({ width, name, ok: true }); };
   const authenticate = async () => { await page.locator('#credential').fill('local-test-token'); await page.locator('#connect-form').dispatchEvent('submit'); };
   const ready = () => page.waitForFunction(() => !!document.querySelector('#goal-start') && !document.querySelector('#goal-loop [data-goal-loading]'));
   const stateIs = async s => { try { await page.waitForFunction(v => !document.querySelector('#goal-loop [data-goal-loading]') && !document.querySelector('#goal-loop [data-goal-pending]') && document.querySelector('#goal-run-state')?.textContent.includes(v), s); } catch (e) { console.error({ panelError: await page.locator('#goal-error').textContent(), errors }); await page.screenshot({ path: path.join(out, 'failure.png'), fullPage: true }); throw e; } };
   const tab = async name => { await page.locator(`[data-chat-tab="${name}"]`).click(); await page.locator('#chat-tabs').waitFor(); };
   await page.goto('http://bba.fixture/product/chat'); await authenticate(); await ready();
   await check('fixture badge, sequential demo and existing scope retained', async () => {
    assert.match(await page.locator('#goal-loop').textContent(), /确定性测试实现，未调用真实模型\/QQ/);
    assert.equal(await page.locator('#goal-loop ol li').count(), 4);
    assert.match(await page.locator('#content').textContent(), /Agent 流式输出、停止与可追溯工具结果/);
    assert.equal(await page.locator('#goal-start option[value="planner"]').count(), 2);
   });
   await page.locator('#goal-start [name=thread_id]').fill('new-offline-thread');
   await page.locator('#goal-start [name=group_id]').fill('100000000000000001');
   await page.locator('#goal-start [name=goal]').fill('收集明确需求与预算');
   await page.locator('#goal-start [name=planner]').selectOption('planner');
   await page.locator('#goal-start [name=analyst]').selectOption('analyst');
   await page.locator('#goal-start button[type=submit]').click(); await stateIs('WAITING_INPUT');
   await page.locator('[data-goal-window=sparse]').click(); await stateIs('WAITING_INPUT');
   await check('first window triggers bounded polling and evidence display', async () => {
    assert(detailReads > 0); assert.match(await page.locator('#goal-loop').textContent(), /缺少预算与时间/);
    assert.match(await page.locator('#goal-loop').textContent(), /ev-1/);
    assert.match(await page.locator('#goal-loop').textContent(), /资产图/); assert.match(await page.locator('#goal-loop').textContent(), /探索图/);
    assert.equal(await page.locator('#goal-loop script').count(), 0);
   });
   const beforeExpiry = structuredClone(runs[0]);
   await page.locator('[data-goal-window=expired]').click();
   await page.waitForFunction(() => document.querySelector('#goal-error')?.textContent.includes('WINDOW_EXPIRED'));
   await check('expired window rejection preserves evidence and conclusion', async () => { assert.deepEqual(runs[0], beforeExpiry); assert.equal(windows, 1); });
   await page.locator('[data-goal-window=complete]').click(); await stateIs('AWAITING_CONFIRMATION');
   await check('second window exposes change reason, consumption and receipts', async () => {
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await page.screenshot({ path: path.join(out, `goal-loop-detail-${width}.png`), fullPage: true });
    const text = await page.locator('#goal-loop').textContent(); assert.match(text, /ev-2/); assert.match(text, /新增预算与时间/); assert.match(text, /已消费证据/); assert.match(text, /已保存技能/); assert.match(text, /v4/);
   });
   await check('awaiting confirmation still permits new synthetic evidence', async () => { assert(await page.locator('[data-goal-window=complete]').isEnabled()); });
   await page.locator('[data-goal-window=complete]').click(); await stateIs('AWAITING_CONFIRMATION');
   conflict = true; await page.locator('#goal-confirm').click();
   await page.waitForFunction(() => document.querySelector('#goal-error')?.textContent.includes('VERSION_CONFLICT'));
   assert.match(await page.locator('#goal-run-state').textContent(), /AWAITING_CONFIRMATION/);
   conflict = false; await page.locator('#goal-confirm').click(); await stateIs('CONFIRMED');
   await check('confirm uses current CAS and unique mutation keys', async () => {
    const posts = requests.filter(x => x.method === 'POST'); assert(posts.every(x => x.key)); assert.equal(new Set(posts.map(x => x.key)).size, posts.length);
    assert.equal(posts.filter(x => x.path.endsWith('/confirm')).at(-1).body.expected_version, runs[0].version - 1);
   });
   await page.locator('#goal-start button[type=submit]').click(); await stateIs('WAITING_INPUT');
   await page.locator('#goal-stop').click(); await stateIs('STOPPED');
   await check('stop is explicit and terminal polling ceases', async () => { const n = detailReads; await page.waitForTimeout(1800); assert.equal(detailReads, n); });
   const heldDetail = waitHeld(); hold = 'detail'; await page.locator('#goal-refresh').click(); await heldDetail;
   await tab('qq'); await tab('agent'); await ready(); await release();
   await check('A to B to A discards late detail and keeps QQ inbox', async () => {
    assert(!(await page.locator('#content').textContent()).includes('STALE_DETAIL_MUST_NOT_RENDER'));
    await tab('qq'); assert.equal(await page.locator('#goal-loop').count(), 0); assert.match(await page.locator('#content').textContent(), /qq-inbox-preserved/); await tab('agent'); await ready();
   });
   const held401 = waitHeld(); hold = 'unauthorized'; await page.locator('#goal-refresh').click(); await held401;
   await tab('qq'); await tab('agent'); await ready(); await release();
   await check('old view 401 cannot clear the current authenticated view', async () => { await page.waitForTimeout(80); assert(await page.locator('#auth').isHidden()); assert(await page.locator('#goal-start').isVisible()); });
   await page.locator('#goal-start button[type=submit]').click(); await stateIs('WAITING_INPUT');
   runs[2].state = 'AWAITING_CONFIRMATION'; await page.locator('#goal-refresh').click(); await stateIs('AWAITING_CONFIRMATION');
   const heldControl = waitHeld(); hold = 'control'; await page.locator('#goal-confirm').click(); await heldControl;
   await tab('qq'); await tab('agent'); await ready(); await release();
   await check('late mutation receipt cannot replace a reopened view', async () => { await page.waitForTimeout(80); assert(!(await page.locator('#goal-loop').textContent()).includes('STALE_MUTATION_MUST_NOT_RENDER')); });
   runs[2].state = 'RUNNING'; await page.locator('#goal-refresh').click(); await stateIs('RUNNING');
   const nav = async name => { if (await page.locator('#menu').isVisible()) await page.locator('#menu').click(); await page.locator(`[data-route="${name}"]`).click(); };
   await nav('schedules');
   await check('leaving the route cancels active polling', async () => { const n = detailReads; await page.waitForTimeout(1800); assert.equal(detailReads, n); });
   runs[2].state = 'WAITING_INPUT'; await nav('chat'); await ready();
   if (width === 1440) {
    runs[2].state = 'RUNNING'; await page.locator('#goal-refresh').click(); await stateIs('RUNNING'); const n = detailReads;
    await page.waitForFunction(() => document.querySelector('#goal-loop')?.textContent.includes('自动刷新已达 20 次上限'), null, { timeout: 25000 });
    await check('automatic polling stops at a finite budget and manual refresh stays available', async () => { await page.waitForTimeout(1800); assert.equal(detailReads - n, 20); assert(await page.locator('#goal-refresh').isEnabled()); });
   }
   runs[2].state = 'AWAITING_CONFIRMATION'; enabled = false; await page.locator('#goal-reload').click(); await ready();
   await check('disabled fixture gate hides injection and blocks every mutation on existing runs', async () => { assert.equal(await page.locator('[data-goal-window]').count(), 0); assert(await page.locator('#goal-confirm').isDisabled()); assert(await page.locator('#goal-stop').isDisabled()); });
   enabled = true; await page.locator('#goal-reload').click(); await ready();
   const heldList = waitHeld(); hold = 'list'; await page.locator('#refresh').click(); await heldList;
   await page.locator('#logout').click(); enabled = false; runs = []; await authenticate(); await ready(); await release();
   await check('auth reset ignores old fixture capability and runs', async () => {
    assert.equal(await page.locator('[data-goal-window]').count(), 0); assert(await page.locator('#goal-start button[type=submit]').isDisabled());
    assert(!(await page.locator('#goal-loop').textContent()).includes('run-1'));
   });
   enabled = true; failList = true; await page.locator('#refresh').click();
   await page.waitForFunction(() => document.querySelector('#goal-error')?.textContent.includes('503'));
   await check('list failure is explicit and fails closed', async () => { assert.equal(await page.locator('[data-goal-window]').count(), 0); assert(await page.locator('#goal-start button[type=submit]').isDisabled()); });
   await check('responsive, no stored credentials, no page errors or external writes', async () => {
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)); assert.equal(await page.evaluate(() => localStorage.length + sessionStorage.length), 0); assert.deepEqual(errors, []);
    assert(requests.filter(x => x.method === 'POST').every(x => x.path.startsWith('/v1/operator/goal-runs')));
   });
   await page.screenshot({ path: path.join(out, `goal-loop-${width}.png`), fullPage: true }); await page.close();
  }
 } finally { await browser.close(); fs.writeFileSync(path.join(out, 'assertions.json'), JSON.stringify({ evidence_kind: 'browser-local API fixture only', results }, null, 2)); }
 console.log(JSON.stringify({ passed: true, checks: results.length, out, evidence_kind: 'UI fixture only' }));
})().catch(e => { console.error(e); process.exitCode = 1; });
