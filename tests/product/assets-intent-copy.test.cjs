/* Product-only UI regression. No backend, QQ, model or database is used. */
'use strict';
const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const fs = require('node:fs/promises');
const path = require('node:path');
const { chromium } = require('playwright');
const root = path.resolve(__dirname, '../..');
let browser, server, origin;
before(async () => {
  browser = await chromium.launch({ headless: true, executablePath: process.env.BBA_TEST_CHROME, args: ['--no-sandbox'] });
  server = http.createServer(async (req, res) => {
    const name = path.basename(new URL(req.url, 'http://localhost').pathname);
    const file = ['app.js', 'app.css'].includes(name) ? name : 'index.html';
    res.setHeader('Content-Type', file.endsWith('.js') ? 'text/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html');
    res.end(await fs.readFile(path.join(root, 'frontend', file)));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  origin = `http://127.0.0.1:${server.address().port}`;
});
after(async () => { if (server) await new Promise(resolve => server.close(resolve)); if (browser) await browser.close(); });

test('assets page states that clear intent counts independently from friend status', async t => {
  const context = await browser.newContext();
  const page = await context.newPage();
  t.after(() => context.close());
  await page.goto(`${origin}/product/assets`);
  await page.locator('#credential').fill('fixture-token');
  await page.locator('#connect-form button').click();
  const text = await page.locator('#content').innerText();
  assert.match(text, /明确意向可计入引流成功/);
  assert.match(text, /好友关系单独记录/);
  assert.doesNotMatch(text, /至少一个运营 QQ 好友关系才算引流成功/);
  assert.match(text, /待接入/);
});
