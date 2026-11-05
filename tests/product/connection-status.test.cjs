/* Real frontend + loopback HTTP fixture. No QQ, model, database or deployed service.
 * NODE_PATH=<playwright modules> BBA_TEST_CHROME=<chrome> node --test tests/product/connection-status.test.cjs
 */
'use strict';
const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const fs = require('node:fs/promises');
const path = require('node:path');
const { chromium } = require('playwright');
const root = path.resolve(__dirname, '../..');
let browser;
before(async () => { browser = await chromium.launch({ headless: true, executablePath: process.env.BBA_TEST_CHROME, args: ['--no-sandbox'] }); });
after(async () => { if (browser) await browser.close(); });

async function fixture(t, route = 'chat') {
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(4000);
  const requests = [], errors = [], holds = [];
  let next;
  page.on('pageerror', e => errors.push(e.message));
  // Allow deliberately retained responses to complete after logout. The production
  // session guard must remain sufficient even when abort cannot cancel delivery.
  await context.addInitScript(() => {
    const fetch = window.fetch.bind(window);
    window.__connectionCalls = [];
    window.fetch = async (url, options) => {
      const call = { done: false };
      window.__connectionCalls.push(call);
      if (window.__retainConnectionRequest) {
        window.__retainConnectionRequest = false;
        options = { ...options, signal: undefined };
      }
      try {
        const response = await fetch(url, options), json = response.json.bind(response);
        response.json = async () => { try { return await json(); } finally { call.done = true; } };
        return response;
      } catch (e) { call.done = true; throw e; }
    };
  });
  const server = http.createServer(async (req, res) => {
    try {
      const pathname = new URL(req.url, 'http://localhost').pathname;
      if (pathname.startsWith('/product/')) {
        const name = ['app.js', 'app.css'].includes(path.basename(pathname)) ? path.basename(pathname) : 'index.html';
        res.setHeader('Content-Type', { 'app.js': 'text/javascript', 'app.css': 'text/css', 'index.html': 'text/html' }[name]);
        res.end(await fs.readFile(path.join(root, 'frontend', name))); return;
      }
      if (!pathname.startsWith('/v1/')) { res.statusCode = 404; res.end(); return; }
      requests.push({ method: req.method, path: pathname });
      const fault = next; if (!fault?.disconnect) next = undefined;
      if (fault?.wait) await fault.wait;
      if (fault?.disconnect) { res.destroy(); return; }
      res.statusCode = fault?.status || 200;
      res.setHeader('Content-Type', 'application/json');
      if (fault?.malformed) { res.end('not-json'); return; }
      const data = { items: [], next_cursor: '', can_configure: false };
      const payload = pathname === '/v1/tasks' ? { data, meta: { has_more: false, next_cursor: '' } }
        : pathname === '/v1/operator/tools' ? { entries: [], entry_count: 0, unique_count: 0, duplicates: {}, alias_candidates: [] }
        : { data };
      res.end(JSON.stringify(res.statusCode >= 400 ? { error: { code: 'FIXTURE_ERROR' } } : payload));
    } catch (e) { errors.push(e.message); res.destroy(); }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => {
    holds.forEach(release => release());
    await context.close();
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
    assert.deepEqual(errors, [], 'no browser or fixture errors');
    assert(requests.every(r => r.method === 'GET'), 'status checks never create work or external actions');
  });
  await page.goto(`http://127.0.0.1:${server.address().port}/product/${route}`);
  async function settled() {
    await page.waitForFunction(() => window.__connectionCalls.every(c => c.done));
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  }
  async function login() {
    await page.locator('#credential').fill('synthetic-operator-token');
    await page.locator('#connect-form button').click();
    await settled();
  }
  async function label(expected) { await page.waitForFunction(text => document.querySelector('#connection').textContent === text, expected); }
  async function hold(status = 200) {
    const call = await page.evaluate(() => window.__connectionCalls.length);
    let release;
    next = { status, wait: new Promise(resolve => { release = resolve; }) };
    holds.push(release);
    await page.locator('#refresh').click();
    await page.waitForFunction(n => window.__connectionCalls.length > n, call);
    // Wait until the server has consumed the fault, not just fetch() creation.
    const deadline = Date.now() + 3000;
    while (next && Date.now() < deadline) await new Promise(resolve => setImmediate(resolve));
    assert.equal(next, undefined, 'fixture must receive the held request');
    return async () => { release(); await settled(); };
  }
  return { page, login, label, settled, hold, fault: value => { next = value; }, requests };
}

for (const route of ['chat', 'notify', 'llm', 'agents', 'skills', 'intercept', 'approvals', 'tasks', 'tools']) {
  test(`${route}: successful API read updates the shared console status`, { timeout: 15000 }, async t => {
    const f = await fixture(t, route);
    await f.label('尚未连接');
    await f.login();
    await f.label('控制台已连接');
    assert.equal(await f.page.locator('#auth').isHidden(), true);
    assert.match(await f.page.locator('#connection').getAttribute('title'), /不代表 QQ 在线/);
    await f.page.locator('#logout').click();
    await f.label('尚未连接');
    assert.equal(await f.page.locator('#auth').isVisible(), true);
  });
}

test('a placeholder page cannot verify credentials without an API read', async t => {
  const f = await fixture(t, 'assets');
  await f.login();
  await f.label('凭据待验证');
  assert.equal(f.requests.length, 0);
});

for (const fault of [{ status: 403 }, { status: 500 }, { disconnect: true }, { malformed: true }]) {
  test(`current request failure ${JSON.stringify(fault)} remains visible until explicit successful retry`, async t => {
    const f = await fixture(t);
    await f.login();
    f.fault(fault);
    await f.page.locator('#refresh').click(); await f.settled();
    await f.label('控制台请求失败');
    assert.equal(await f.page.locator('#auth').isHidden(), true);
    f.fault(undefined);
    await f.page.locator('#refresh').click(); await f.settled();
    await f.label('控制台已连接');
  });
}

test('current 401 resets authentication and the header', async t => {
  const f = await fixture(t); await f.login();
  f.fault({ status: 401 });
  await f.page.locator('#refresh').click(); await f.settled();
  await f.label('尚未连接');
  assert.equal(await f.page.locator('#auth').isVisible(), true);
});

for (const status of [200, 401, 500]) {
  test(`old ${status} after route change cannot overwrite the new route status`, async t => {
    const f = await fixture(t); await f.login();
    const release = await f.hold(status);
    await f.page.locator('[data-route="notify"]').click();
    await f.page.waitForFunction(() => document.querySelector('#content').textContent.includes('暂无通知'));
    await f.label('控制台已连接');
    await release(); await f.label('控制台已连接');
    assert.equal(await f.page.locator('#auth').isHidden(), true);
  });
  test(`old ${status} after logout cannot restore the connected label`, async t => {
    const f = await fixture(t); await f.login();
    await f.page.evaluate(() => { window.__retainConnectionRequest = true; });
    const release = await f.hold(status);
    await f.page.locator('#logout').click(); await f.label('尚未连接');
    await release(); await f.label('尚未连接');
  });
}

for (const status of [200, 401, 500]) {
  test(`old ${status} after same-route refresh cannot overwrite a new result`, async t => {
    const f = await fixture(t); await f.login();
    const release = await f.hold(status);
    await f.page.locator('#refresh').click();
    await f.page.waitForFunction(() => window.__connectionCalls.at(-1).done);
    await f.label('控制台已连接');
    await release(); await f.label('控制台已连接');
    assert.equal(await f.page.locator('#auth').isHidden(), true);
  });
}

test('submitting credentials stays pending until the first current response succeeds', async t => {
  const f = await fixture(t, 'assets'); await f.login();
  await f.label('凭据待验证');
  // Reuse the held-refresh path on a read page while a fresh session is pending.
  await f.page.locator('[data-route="chat"]').click(); await f.settled();
  await f.page.locator('#logout').click();
  await f.page.locator('#credential').fill('new-synthetic-token');
  const reply = Promise.withResolvers();
  await f.page.route('**/v1/operator/conversations?*', async route => {
    await reply.promise;
    await route.continue();
  });
  try {
    await f.page.locator('#connect-form button').click();
    await f.page.waitForFunction(() => window.__connectionCalls.length >= 2);
    await f.label('凭据待验证');
  } finally { reply.resolve(); }
  await f.settled(); await f.label('控制台已连接');
});

test('whitespace credentials leave authentication open without claiming a pending connection', async t => {
  const f = await fixture(t);
  await f.page.locator('#credential').fill('   ');
  await f.page.locator('#connect-form button').click();
  await f.label('尚未连接');
  assert.equal(await f.page.locator('#auth').isVisible(), true);
  assert.equal(f.requests.length, 0);
});
