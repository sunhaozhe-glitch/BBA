/* Real frontend + releasable loopback HTTP fixture; no Go/QQ authorization or persistence claim.
 * NODE_PATH=<playwright modules> BBA_TEST_CHROME=<chrome> node --test tests/product/notification-request-scope.test.cjs
 * Timeout tests explicitly fire only the frontend's captured 8000ms timer; no sleep or automatic write retry.
 */
'use strict';
const { test: nodeTest, before, after } = require('node:test');
const test = (name, fn) => nodeTest(name, { timeout: 15000 }, fn);
const assert = require('node:assert/strict');
const http = require('node:http');
const fs = require('node:fs/promises');
const path = require('node:path');
const { chromium } = require('playwright');
const root = path.resolve(__dirname, '../..');
const noticesPath = '/v1/operator/notifications';
const writePath = id => `${noticesPath}/${id}`;
const list = items => ({ data: { items, next_cursor: '' } });
const notice = (id, summary = 'NOTICE_' + id.toUpperCase()) => ({ id, summary, version: 1, event_type: 'message', delivery_state: 'local_only', created_at: '2026-11-04T00:00:00Z', read_at: null });
const error = status => ({ status, body: { error: { code: 'FIXTURE_ERROR_' + status } } });
let browser;
before(async () => { browser = await chromium.launch({ headless: true, executablePath: process.env.BBA_TEST_CHROME, args: ['--no-sandbox'] }); });
after(async () => { if (browser) await browser.close(); });

async function fixture(t) {
  const holds = [], errors = [], requests = [], rows = [notice('n1'), notice('n2'), notice('n3')];
  const context = await browser.newContext();
  await context.addInitScript(() => {
    // Observe completion and selectively retain a real HTTP request across logout so a late
    // response tests session validity independently of AbortController cancellation.
    const nativeFetch = window.fetch.bind(window), nativeSet = window.setTimeout.bind(window), nativeClear = window.clearTimeout.bind(window);
    const timers = new Map(); let lastTimer;
    window.__scope = { calls: [], retainNext: false, fire(id) { const c = this.calls[id], timer = timers.get(c.timer); if (!timer) throw Error('missing 8000ms API timer'); nativeClear(c.timer); timer(); } };
    window.setTimeout = (fn, ms, ...args) => { const id = nativeSet(fn, ms, ...args); if (ms === 8000) { timers.set(id, fn); lastTimer = id; } return id; };
    window.clearTimeout = id => { timers.delete(id); nativeClear(id); };
    window.fetch = async (url, options) => {
      const s = window.__scope, call = { path: new URL(url, location.href).pathname, method: options?.method || 'GET', timer: lastTimer, done: false };
      s.calls.push(call);
      if (s.retainNext) { s.retainNext = false; options = { ...options, signal: undefined }; call.retained = true; }
      try {
        const res = await nativeFetch(url, options), json = res.json.bind(res);
        res.json = async () => { try { return await json(); } finally { call.done = true; } };
        return res;
      } catch (e) { call.done = true; call.failure = e.name; throw e; }
    };
  });
  const page = await context.newPage(); page.setDefaultTimeout(4000);
  page.on('pageerror', e => errors.push(e.message));
  const server = http.createServer(async (req, res) => {
    try {
      const url = new URL(req.url, 'http://localhost');
      if (url.pathname.startsWith('/product/')) {
        const name = ['app.js', 'app.css'].includes(path.basename(url.pathname)) ? path.basename(url.pathname) : 'index.html';
        res.setHeader('Content-Type', { 'app.js': 'text/javascript', 'app.css': 'text/css', 'index.html': 'text/html' }[name]);
        res.end(await fs.readFile(path.join(root, 'frontend', name))); return;
      }
      let body = ''; for await (const chunk of req) body += chunk;
      const payload = body ? JSON.parse(body) : null;
      requests.push({ method: req.method, path: url.pathname, payload, token: req.headers.authorization, key: req.headers['idempotency-key'] });
      let status = 200, result, persist;
      if (req.method === 'GET' && url.pathname === noticesPath) result = list(structuredClone(rows));
      else if (req.method === 'POST' && url.pathname.startsWith(noticesPath + '/')) {
        const row = rows.find(n => writePath(n.id) === url.pathname); assert(row);
        assert.deepEqual(payload, { expected_version: row.version, read: true });
        assert(req.headers['idempotency-key']);
        persist = () => { row.read_at = '2026-11-04T01:00:00Z'; row.version++; };
        result = { data: { ...row, read_at: '2026-11-04T01:00:00Z', version: row.version + 1 } };
      } else if (url.pathname === '/v1/operator/conversations') result = list([{ id: 'a', kind: 'agent', conversation_kind: 'agent', conversation_id: 'THREAD_A', version: 1 }]);
      else if (url.pathname === '/v1/operator/conversations/a/messages') result = list([{ id: 'm', body: 'CHAT_MESSAGE', seq: 1, source: 'agent', direction: 'inbound', delivery_state: 'local_record', created_at: '2026-11-04T00:00:00Z' }]);
      else { status = 404; result = { error: { code: 'UNEXPECTED_ROUTE' } }; }
      const h = holds.find(h => !h.used && h.method === req.method && h.path === url.pathname);
      if (h) { h.used = true; h.arrive(); await h.ready; if (h.response) ({ status, body: result } = h.response); }
      if (status < 300 && persist) persist(); // Commit can outlive an ignored/aborted client receipt.
      if (h?.disconnect) { res.writeHead(status, { 'Content-Type': 'application/json' }); res.write('{"data":', () => res.destroy()); }
      else { res.writeHead(status, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(result)); }
      if (h) h.finish();
    } catch (e) { errors.push(e.stack); res.writeHead(500); res.end('{}'); }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => { holds.forEach(h => h.release()); await context.close(); server.closeAllConnections(); await new Promise(r => server.close(r)); assert.deepEqual(errors, []); });
  function hold(method, pathname, response, disconnect = false) {
    let release, arrive, finish;
    const h = { method, path: pathname, response, disconnect, used: false, ready: new Promise(r => { release = r; }), arrived: new Promise(r => { arrive = r; }), finished: new Promise(r => { finish = r; }), release: () => release(), arrive: () => arrive(), finish: () => finish() };
    holds.push(h); return h;
  }
  async function capture(h) { await h.arrived; h.client = await page.evaluate(({ method, path }) => window.__scope.calls.findLastIndex(c => !c.done && c.method === method && c.path === path), { method: h.method, path: h.path }); assert(h.client >= 0); }
  async function drain(h) { await page.waitForFunction(id => window.__scope.calls[id].done, h.client); await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))); }
  async function settle(h) { h.release(); await h.finished; await drain(h); }
  async function login(token = 'fixture-session-one') { await page.locator('#credential').fill(token); await page.locator('#connect-form').dispatchEvent('submit'); await page.getByText('NOTICE_N1', { exact: true }).waitFor(); }
  async function notify() { await page.locator('[data-route="notify"]').click(); await page.getByText('NOTICE_N1', { exact: true }).waitFor(); }
  async function chat() { await page.locator('[data-route="chat"]').click(); await page.locator('[data-chat-conv="a"]').click(); await page.getByText('CHAT_MESSAGE', { exact: true }).waitFor(); }
  async function start(method, outcome, retain = false) {
    const h = hold(method, method === 'GET' ? noticesPath : writePath('n1'), typeof outcome === 'number' && outcome !== 200 ? error(outcome) : undefined, outcome === 'disconnect');
    if (retain) await page.evaluate(() => { window.__scope.retainNext = true; });
    if (method === 'GET') await page.locator('#refresh').click();
    else await page.locator('[data-notice-read="n1"]').click();
    await capture(h); return h;
  }
  await page.goto(`http://127.0.0.1:${server.address().port}/product/notify`); await login();
  return { page, hold, capture, drain, settle, login, notify, chat, start, requests, rows };
}
async function snapshot(page) { return page.evaluate(() => ({ content: document.querySelector('#content').innerHTML, error: document.querySelector('#error').textContent, errorHidden: document.querySelector('#error').hidden, auth: document.querySelector('#auth').hidden, updated: document.querySelector('#updated').textContent, connection: document.querySelector('#connection').textContent })); }

// Each old read/write receives a real held response after a newer page is ready.
for (const method of ['GET', 'POST']) for (const transition of ['chat', 'roundtrip', 'refresh', 'newlogin']) for (const outcome of [200, 403, 409, 401, 500, 'timeout', 'disconnect']) {
  test(`old ${method} ${outcome} after ${transition} cannot change the new view/session`, async t => {
    const f = await fixture(t), h = await f.start(method, outcome, transition === 'newlogin' && outcome !== 'timeout');
    if (transition === 'chat') await f.chat();
    if (transition === 'roundtrip') { await f.chat(); await f.notify(); }
    if (transition === 'refresh') { await f.page.locator('#refresh').click(); await f.page.getByText('NOTICE_N1', { exact: true }).waitFor(); }
    if (transition === 'newlogin') {
      if (outcome === 'timeout') {
        // Fire timeout then change sessions in the same task, before its rejection runs.
        await f.page.evaluate(id => { window.__scope.fire(id); document.querySelector('#logout').click(); document.querySelector('#credential').value = 'fixture-session-two'; document.querySelector('#connect-form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); }, h.client);
        await f.page.getByText('NOTICE_N1', { exact: true }).waitFor();
      } else { await f.page.locator('#logout').click(); await f.login('fixture-session-two'); }
    }
    const before = await snapshot(f.page), reads = f.requests.filter(r => r.method === 'GET' && r.path === noticesPath).length;
    if (outcome === 'timeout' && transition !== 'newlogin') { await f.page.evaluate(id => window.__scope.fire(id), h.client); await f.drain(h); }
    await f.settle(h);
    assert.deepEqual(await snapshot(f.page), before, 'stale receipt must leave the new DOM, error and session intact');
    assert.equal(f.requests.filter(r => r.method === 'GET' && r.path === noticesPath).length, reads, 'stale write must not launch a new read');
    if (method === 'POST') {
      assert.equal(f.requests.filter(r => r.method === 'POST').length, 1, 'no automatic write retry');
      if (outcome === 200 || outcome === 'timeout' || outcome === 'disconnect') { await f.notify(); assert(f.rows[0].read_at); assert.equal(await f.page.locator('[data-notice-read="n1"]').count(), 0, 're-entry reads the committed fixture state'); }
    }
  });
}

// A real sibling success starts a new notification generation with the same route seq.
for (const outcome of [200, 401, 403, 500, 'timeout']) test(`same-route generation rejects old POST ${outcome} and preserves the new button`, async t => {
  const f = await fixture(t), h = await f.start('POST', outcome);
  await f.page.locator('[data-notice-read="n2"]').click();
  await f.page.waitForFunction(() => !document.querySelector('[data-notice-read="n2"]') && document.querySelector('[data-notice-read="n3"]')); 
  const next = f.hold('POST', writePath('n3'), error(403));
  await f.page.locator('[data-notice-read="n3"]').click(); await f.capture(next);
  const before = await snapshot(f.page), reads = f.requests.filter(r => r.method === 'GET' && r.path === noticesPath).length;
  if (outcome === 'timeout') { await f.page.evaluate(id => window.__scope.fire(id), h.client); await f.drain(h); }
  await f.settle(h);
  assert.deepEqual(await snapshot(f.page), before);
  assert.equal(await f.page.locator('[data-notice-read="n3"]').isDisabled(), true, 'old finally must not enable the new busy button');
  assert.equal(f.requests.filter(r => r.method === 'GET' && r.path === noticesPath).length, reads);
  await f.settle(next);
  assert.equal(await f.page.locator('[data-notice-read="n3"]').isEnabled(), true);
});

test('first sibling success invalidates the other button before its receipt can start a second same-seq read', async t => {
  const f = await fixture(t), first = f.hold('POST', writePath('n1')), second = f.hold('POST', writePath('n2'));
  await f.page.locator('[data-notice-read="n1"]').click(); await f.capture(first);
  await f.page.locator('[data-notice-read="n2"]').click(); await f.capture(second);
  const read = f.hold('GET', noticesPath);
  await f.settle(first); await f.capture(read);
  const before = await snapshot(f.page), count = f.requests.filter(r => r.method === 'GET' && r.path === noticesPath).length;
  await f.settle(second);
  assert.deepEqual(await snapshot(f.page), before, 'the current held read stays in charge');
  assert.equal(f.requests.filter(r => r.method === 'GET' && r.path === noticesPath).length, count, 'an invalidated button cannot initiate a second read');
  await f.settle(read); await f.page.getByText('NOTICE_N1', { exact: true }).waitFor();
  await f.page.locator('#refresh').click(); await f.page.getByText('NOTICE_N1', { exact: true }).waitFor();
  assert.equal(await f.page.locator('[data-notice-read="n2"]').count(), 0, 'the ignored receipt still committed on the fixture server');
});

for (const method of ['GET', 'POST']) for (const outcome of [403, 409, 500, 'timeout', 'disconnect']) test(`current ${method} ${outcome} reports failure and permits explicit retry/readback`, async t => {
  const f = await fixture(t), h = await f.start(method, outcome);
  if (outcome === 'timeout') { await f.page.evaluate(id => window.__scope.fire(id), h.client); await f.drain(h); }
  else await f.settle(h);
  assert.equal(await f.page.locator('#auth').isVisible(), false);
  assert.equal(await f.page.locator('#error').isVisible(), true);
  if (typeof outcome === 'number') assert((await f.page.locator('#error').textContent()).includes('FIXTURE_ERROR_' + outcome));
  if (method === 'POST') assert.equal(await f.page.locator('[data-notice-read="n1"]').isEnabled(), true);
  assert.equal(f.requests.filter(r => r.method === 'POST').length, method === 'POST' ? 1 : 0, 'failure never automatically resends a write');
  if (outcome === 'timeout') await f.settle(h);
  if (method === 'POST' && typeof outcome === 'number') {
    await f.page.locator('[data-notice-read="n1"]').click();
    await f.page.waitForFunction(() => !document.querySelector('[data-notice-read="n1"]'));
    await f.page.getByText('NOTICE_N1', { exact: true }).waitFor();
    assert.equal(f.requests.filter(r => r.method === 'POST').length, 2);
  } else {
    await f.page.locator('#refresh').click(); await f.page.getByText('NOTICE_N1', { exact: true }).waitFor();
  }
  assert.equal(await f.page.locator('#error').isVisible(), false, 'successful reload clears the old error');
});

for (const method of ['GET', 'POST']) test(`current ${method} 401 still requires authentication`, async t => {
  const f = await fixture(t), h = await f.start(method, 401); await f.settle(h);
  assert.equal(await f.page.locator('#auth').isVisible(), true);
  assert.equal(await f.page.locator('#error').isVisible(), false);
});

for (const method of ['GET', 'POST']) test(`logout aborts current ${method} without polluting login or the next session`, async t => {
  const f = await fixture(t), h = await f.start(method, 200);
  await f.page.locator('#logout').click(); await f.drain(h);
  assert.equal(await f.page.locator('#auth').isVisible(), true);
  assert.equal(await f.page.locator('#error').isVisible(), false);
  assert.equal(await f.page.evaluate(id => window.__scope.calls[id].failure, h.client), 'AbortError');
  await f.login('fixture-session-two'); const before = await snapshot(f.page);
  await f.settle(h); assert.deepEqual(await snapshot(f.page), before);
});

test('current successful POST refreshes the list exactly once', async t => {
  const f = await fixture(t), before = f.requests.length, h = await f.start('POST', 200);
  await f.settle(h); await f.page.getByText('NOTICE_N1', { exact: true }).waitFor();
  assert.equal(await f.page.locator('[data-notice-read="n1"]').count(), 0);
  assert.equal(f.requests.slice(before).filter(r => r.path === noticesPath).length, 1);
  assert.equal(await f.page.locator('#error').isVisible(), false);
});

test('the same pending button submits once even for repeated dispatched clicks', async t => {
  const f = await fixture(t), h = f.hold('POST', writePath('n1'), error(409));
  await f.page.locator('[data-notice-read="n1"]').evaluate(b => { b.click(); b.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
  await f.capture(h);
  assert.equal(await f.page.locator('[data-notice-read="n1"]').isDisabled(), true);
  assert.equal(f.requests.filter(r => r.method === 'POST').length, 1);
  await f.settle(h);
  assert.equal(await f.page.locator('[data-notice-read="n1"]').isEnabled(), true);
});

for (const outcome of [200, 401, 403, 500]) test(`detached mutation button rejects ${outcome} even in the same generation`, async t => {
  const f = await fixture(t), h = await f.start('POST', outcome);
  await f.page.locator('[data-notice-read="n1"]').evaluate(b => { const replacement = b.cloneNode(true); replacement.disabled = true; b.replaceWith(replacement); });
  const before = await snapshot(f.page); await f.settle(h);
  assert.deepEqual(await snapshot(f.page), before, 'receipt is bound to its original button DOM instance');
  assert.equal(await f.page.locator('[data-notice-read="n1"]').isDisabled(), true);
});
