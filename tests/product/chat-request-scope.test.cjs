/* Browser regression of the real frontend with a deterministic HTTP fixture.
 * This does not test Go authorization, persistence, or real QQ effects.
 * NODE_PATH=<playwright node_modules> BBA_TEST_CHROME=<chrome> node --test tests/product/chat-request-scope.test.cjs
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
const messagesPath = id => `/v1/operator/conversations/${id}/messages`;
const agentPath = id => `/v1/operator/agent-threads/${id}/messages`;
const message = (body, id = body) => ({ id, body, seq: 1, source: 'agent', direction: 'inbound', delivery_state: 'local_record', created_at: '2026-11-04T00:00:00Z' });
const list = items => ({ data: { items, next_cursor: '' } });
const conversations = [
  { id: 'a', kind: 'agent', conversation_kind: 'agent', conversation_id: '线程 A', version: 1 },
  { id: 'b', kind: 'agent', conversation_kind: 'agent', conversation_id: '线程 B', version: 1 },
  { id: 'q1', kind: 'qq', conversation_kind: 'group', account_id: 'acct-1', conversation_id: '群聊 A', version: 1 },
  { id: 'q2', kind: 'qq', conversation_kind: 'private', account_id: 'acct-2', conversation_id: '私聊 B', version: 1 },
];
let browser;
before(async () => { browser = await chromium.launch({ headless: true, ...(process.env.BBA_TEST_CHROME ? { executablePath: process.env.BBA_TEST_CHROME } : {}), args: ['--no-sandbox'] }); });
after(async () => { if (browser) await browser.close(); });

async function fixture(t) {
  const holds = [];
  const errors = [];
  const rows = Object.fromEntries(conversations.map(c => [c.id, [message('MESSAGE_' + c.id.toUpperCase())]]));
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(5000);
  page.on('pageerror', e => errors.push(e.message));
  const server = http.createServer(async (req, res) => {
    try {
      const url = new URL(req.url, 'http://localhost');
      if (url.pathname.startsWith('/product/')) {
        const name = ['app.js', 'app.css'].includes(path.basename(url.pathname)) ? path.basename(url.pathname) : 'index.html';
        res.setHeader('Content-Type', { 'app.js': 'text/javascript', 'app.css': 'text/css', 'index.html': 'text/html' }[name]);
        res.end(await fs.readFile(path.join(root, 'frontend', name))); return;
      }
      let body = '';
      for await (const chunk of req) body += chunk;
      const payload = body ? JSON.parse(body) : null;
      let status = 200, result, persist;
      if (url.pathname === '/v1/operator/conversations') result = list(conversations);
      else if (req.method === 'GET' && /^\/v1\/operator\/conversations\/[^/]+\/messages$/.test(url.pathname)) result = list([...rows[url.pathname.split('/')[4]]]);
      else if (req.method === 'POST' && /^\/v1\/operator\/agent-threads\/[^/]+\/messages$/.test(url.pathname)) {
        const id = url.pathname.split('/')[4];
        persist = () => rows[id].push(message(payload.body, 'saved-agent'));
        status = 201; result = { data: { id: 'saved-agent', delivery_state: 'local_record', send_permit: 'absent' } };
      } else if (req.method === 'POST' && url.pathname === '/v1/operator/qq-drafts') {
        const c = conversations.find(c => c.account_id === payload.account_id && c.conversation_id === payload.conversation_id);
        assert(c, 'draft must target the selected conversation');
        persist = () => rows[c.id].push(message(payload.body, 'saved-draft'));
        status = 201; result = { data: { id: 'saved-draft', decision: 'review', send_permit: 'absent' } };
      } else if (url.pathname === '/v1/operator/notifications') result = list([]);
      else { status = 404; result = { error: { code: 'FIXTURE_UNEXPECTED_ROUTE' } }; }
      const hold = holds.find(h => !h.used && h.method === req.method && h.path === url.pathname);
      if (hold) {
        hold.used = true; hold.arrive({ payload });
        await hold.ready;
        if (hold.response) ({ status, body: result } = hold.response);
      }
      if (status < 300 && persist) persist();
      res.writeHead(status, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(result));
    } catch (e) { errors.push(e.message); res.writeHead(500); res.end('{}'); }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  t.after(async () => {
    holds.forEach(h => h.release());
    await context.close();
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
    assert.deepEqual(errors, [], 'no unhandled browser or fixture errors');
  });
  function hold(method, pathname, response) {
    let release, arrive;
    const h = { method, path: pathname, response, used: false, ready: new Promise(r => { release = r; }), arrived: new Promise(r => { arrive = r; }), release: () => release(), arrive: v => arrive(v) };
    holds.push(h); return h;
  }
  async function settle(h) {
    const response = page.waitForResponse(r => new URL(r.url()).pathname === h.path && r.request().method() === h.method);
    h.release();
    await response;
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  }
  async function select(id) {
    await page.locator(`[data-chat-conv="${id}"]`).click();
    await page.waitForFunction(text => document.querySelector('#content').textContent.includes(text), 'MESSAGE_' + id.toUpperCase());
  }
  async function login() {
    await page.locator('#credential').fill('test-only-operator');
    await page.locator('#connect-form').dispatchEvent('submit');
    await page.locator('[data-chat-conv="a"]').waitFor();
  }
  await page.goto(origin + '/product/chat');
  await login();
  return { page, hold, settle, select, login, rows };
}

function chatText(page) { return page.locator('#content').textContent(); }

test('selecting another conversation immediately hides old messages and its form during loading', async t => {
  const f = await fixture(t); await f.select('a');
  const h = f.hold('GET', messagesPath('b'));
  await f.page.locator('[data-chat-conv="b"]').click(); await h.arrived;
  assert(!(await chatText(f.page)).includes('MESSAGE_A'));
  assert.equal(await f.page.locator('#chat-form').count(), 0);
  assert((await chatText(f.page)).includes('正在读取'));
  await f.settle(h);
  assert((await chatText(f.page)).includes('MESSAGE_B'));
});

test('late messages from A cannot replace selected B', async t => {
  const f = await fixture(t); const h = f.hold('GET', messagesPath('a'));
  await f.page.locator('[data-chat-conv="a"]').click(); await h.arrived;
  await f.select('b'); await f.settle(h);
  const text = await chatText(f.page);
  assert(text.includes('MESSAGE_B')); assert(!text.includes('MESSAGE_A'));
});

test('A to B to A uses the newest request even for the same conversation ID', async t => {
  const f = await fixture(t); const h = f.hold('GET', messagesPath('a'), { status: 200, body: list([message('STALE_A')]) });
  await f.page.locator('[data-chat-conv="a"]').click(); await h.arrived;
  await f.select('b'); await f.select('a'); await f.settle(h);
  const text = await chatText(f.page);
  assert(text.includes('MESSAGE_A')); assert(!text.includes('STALE_A'));
});

for (const status of [500, 401]) test(`late ${status} from old conversation does not change B or authentication`, async t => {
  const f = await fixture(t); const h = f.hold('GET', messagesPath('a'), { status, body: { error: { code: 'OLD_VIEW_ERROR' } } });
  await f.page.locator('[data-chat-conv="a"]').click(); await h.arrived;
  await f.select('b'); await f.settle(h);
  assert((await chatText(f.page)).includes('MESSAGE_B'));
  assert.equal(await f.page.locator('#error').isVisible(), false);
  assert.equal(await f.page.locator('#auth').isVisible(), false);
});

test('current message failure shows an error rather than an empty conversation or stale content', async t => {
  const f = await fixture(t); await f.select('a');
  const h = f.hold('GET', messagesPath('b'), { status: 500, body: { error: { code: 'CURRENT_VIEW_ERROR' } } });
  await f.page.locator('[data-chat-conv="b"]').click(); await h.arrived; await f.settle(h);
  const text = await chatText(f.page);
  assert(text.includes('CURRENT_VIEW_ERROR')); assert(!text.includes('MESSAGE_A')); assert(!text.includes('该会话暂无消息'));
  assert.equal(await f.page.locator('#chat-form').count(), 0);
});

for (const kind of ['agent', 'qq']) for (const status of [201, 403, 401]) test(`late ${kind} save ${status} cannot update another conversation`, async t => {
  const f = await fixture(t);
  const from = kind === 'agent' ? 'a' : 'q1', to = kind === 'agent' ? 'b' : 'q2';
  if (kind === 'qq') await f.page.locator('#chat-tab-qq').click();
  await f.select(from);
  const pathname = kind === 'agent' ? agentPath(from) : '/v1/operator/qq-drafts';
  const h = f.hold('POST', pathname, status === 201 ? undefined : { status, body: { error: { code: 'OLD_SAVE_ERROR' } } });
  await f.page.locator('#chat-form textarea').fill('SAVE_ONLY_IN_OLD_CONVERSATION');
  await f.page.locator('#chat-form-submit').click(); await h.arrived;
  await f.select(to); await f.settle(h);
  const text = await chatText(f.page);
  assert(text.includes('MESSAGE_' + to.toUpperCase()));
  assert(!text.includes('SAVE_ONLY_IN_OLD_CONVERSATION')); assert(!text.includes('OLD_SAVE_ERROR'));
  assert.equal(await f.page.locator('#auth').isVisible(), false);
  assert.equal(await f.page.locator('#chat-form-error').textContent(), '');
  if (status === 201) { // A hidden receipt is not a cancellation: refetch the original conversation.
    await f.select(from);
    assert((await chatText(f.page)).includes('SAVE_ONLY_IN_OLD_CONVERSATION'));
  }
});

test('late message read cannot overwrite notifications after navigation', async t => {
  const f = await fixture(t); const h = f.hold('GET', messagesPath('a'), { status: 500, body: { error: { code: 'OLD_CHAT_ERROR' } } });
  await f.page.locator('[data-chat-conv="a"]').click(); await h.arrived;
  await f.page.locator('[data-route="notify"]').click(); await f.page.getByText('暂无通知', { exact: true }).waitFor();
  await f.settle(h);
  assert((await chatText(f.page)).includes('暂无通知'));
  assert.equal(await f.page.locator('#error').isVisible(), false);
});

test('tab list requests cannot clear a newer selected conversation', async t => {
  const f = await fixture(t); const h = f.hold('GET', '/v1/operator/conversations');
  await f.page.locator('#chat-tab-qq').click(); await h.arrived;
  // Re-enter Agent while the QQ list is still in flight, using navigation then back.
  await f.page.locator('[data-route="notify"]').click();
  await f.page.locator('[data-route="chat"]').click();
  await f.page.locator('#chat-tab-agent').click();
  await f.select('b'); await f.settle(h);
  assert((await chatText(f.page)).includes('MESSAGE_B'));
});

test('refresh invalidates a pending read even when the same conversation is selected again', async t => {
  const f = await fixture(t); const h = f.hold('GET', messagesPath('a'), { status: 200, body: list([message('BEFORE_REFRESH')]) });
  await f.page.locator('[data-chat-conv="a"]').click(); await h.arrived;
  await f.page.locator('#refresh').click(); await f.select('a'); await f.settle(h);
  const text = await chatText(f.page);
  assert(text.includes('MESSAGE_A')); assert(!text.includes('BEFORE_REFRESH'));
});

test('logging out aborts a read without showing its old error on the login view', async t => {
  const f = await fixture(t); const h = f.hold('GET', messagesPath('a'));
  await f.page.locator('[data-chat-conv="a"]').click(); await h.arrived;
  const aborted = f.page.waitForEvent('requestfailed', r => new URL(r.url()).pathname === messagesPath('a'));
  await f.page.locator('#logout').click(); await aborted;
  await f.page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await f.page.locator('#auth').isVisible(), true);
  assert.equal(await f.page.locator('#error').isVisible(), false);
});

for (const kind of ['agent', 'qq']) test(`${kind} save in the current view still renders its receipt`, async t => {
  const f = await fixture(t); if (kind === 'qq') await f.page.locator('#chat-tab-qq').click();
  await f.select(kind === 'qq' ? 'q1' : 'a');
  await f.page.locator('#chat-form textarea').fill('CURRENT_SAVE');
  await f.page.locator('#chat-form-submit').click();
  await f.page.waitForFunction(() => document.querySelector('#chat-form-error')?.textContent.includes('absent'));
  assert((await chatText(f.page)).includes('CURRENT_SAVE'));
});

for (const kind of ['agent', 'qq']) test(`${kind} save failure in the current view remains visible and allows another attempt`, async t => {
  const f = await fixture(t); if (kind === 'qq') await f.page.locator('#chat-tab-qq').click();
  await f.select(kind === 'qq' ? 'q1' : 'a');
  const h = f.hold('POST', kind === 'qq' ? '/v1/operator/qq-drafts' : agentPath('a'), { status: 403, body: { error: { code: 'CURRENT_SAVE_DENIED' } } });
  await f.page.locator('#chat-form textarea').fill('REJECTED_SAVE');
  await f.page.locator('#chat-form-submit').click(); await h.arrived; await f.settle(h);
  assert((await f.page.locator('#chat-form-error').textContent()).includes('CURRENT_SAVE_DENIED'));
  assert.equal(await f.page.locator('#chat-form-submit').isEnabled(), true);
  assert(!(await chatText(f.page)).includes('saved-agent'));
});

test('current 401 still requires authentication', async t => {
  const f = await fixture(t); const h = f.hold('GET', messagesPath('a'), { status: 401, body: { error: { code: 'UNAUTHORIZED' } } });
  await f.page.locator('[data-chat-conv="a"]').click(); await h.arrived; await f.settle(h);
  assert.equal(await f.page.locator('#auth').isVisible(), true);
  assert.equal(await f.page.locator('#error').isVisible(), false);
});
