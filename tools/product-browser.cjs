#!/usr/bin/env node
/*
 * Permanent product browser acceptance runner.
 *
 * Drives the real embedded console (backend/cmd/taskengine-server/web) in a
 * real Chrome through puppeteer-core against an isolated fixture backend:
 *
 *   connect/readback -> task + graph reads -> operator runtime -> proposal
 *   orchestration -> proposal create/approve -> materialize -> work trace ->
 *   graph sync status/bind -> login session create/QR generation/poll/online/
 *   cancel/expiry -> reload GET-only -> read-only 403, stale 409, injected
 *   5xx/offline/empty/unknown/expired-approval -> pause/cancel readback ->
 *   keyboard + narrow viewport.
 *
 * Only HTTP-boundary responses are ever injected, and every injected response
 * is labelled source="fixture-injection" in the report. Nothing between the
 * browser and the server is mocked; reads and mutations use the real routes.
 *
 * Refuses to run without BBA_BROWSER_* configuration, refuses non-loopback
 * origins, and refuses to mutate unless the server reports a fixture worker
 * transport and the explicit fixture login provider. Tokens, QR bytes and
 * response bodies are never written to the report. Chrome is always closed.
 *
 * Usage: node tools/product-browser.cjs   (configuration from environment)
 * Optional CLI overrides: --output --url --task --account
 */
'use strict'

const fs = require('node:fs/promises')
const path = require('node:path')
const { createRequire } = require('node:module')

const ROOT = path.resolve(__dirname, '..')
const SCHEMA = 'bba-product-browser/1'
const LOOPBACK = new Set(['127.0.0.1', 'localhost', '::1', '[::1]'])
const CHROME_DEFAULT = '/opt/google/chrome/google-chrome'

class FatalError extends Error {}

function fail(message) {
  throw new FatalError(message)
}

function utc() {
  return new Date().toISOString()
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

function env(name) {
  const value = process.env[name]
  if (typeof value !== 'string' || value.trim().length === 0) fail(`missing required environment variable ${name}`)
  return value.trim()
}

function optional(name, fallback) {
  const value = process.env[name]
  return typeof value === 'string' && value.trim().length > 0 ? value.trim() : fallback
}

function parseArgs(argv) {
  const map = { '--output': 'BBA_BROWSER_OUTPUT', '--url': 'BBA_BROWSER_URL', '--task': 'BBA_BROWSER_TASK_ID', '--account': 'BBA_BROWSER_ACCOUNT_ID' }
  for (let index = 0; index < argv.length; index += 1) {
    const key = map[argv[index]]
    if (!key) continue
    const value = argv[index + 1]
    if (typeof value !== 'string' || value.length === 0) fail(`${argv[index]} requires a value`)
    process.env[key] = value
    index += 1
  }
}

function readConfig() {
  const rawURL = env('BBA_BROWSER_URL')
  let parsed
  try {
    parsed = new URL(rawURL)
  } catch {
    fail(`BBA_BROWSER_URL is not a URL: ${rawURL}`)
  }
  if (parsed.protocol !== 'http:') fail('BBA_BROWSER_URL must be the plain http loopback origin')
  if (!LOOPBACK.has(parsed.hostname)) fail(`BBA_BROWSER_URL must be loopback, got ${parsed.hostname}`)
  if (parsed.pathname !== '/' && parsed.pathname !== '') fail('BBA_BROWSER_URL must be an origin without a path')

  const token = env('BBA_BROWSER_TOKEN')
  const readToken = env('BBA_BROWSER_READ_TOKEN')
  if (token === readToken) fail('BBA_BROWSER_TOKEN and BBA_BROWSER_READ_TOKEN must be different credentials')

  const timeout = Number(optional('BBA_BROWSER_TIMEOUT_MS', '25000'))
  if (!Number.isFinite(timeout) || timeout < 1000 || timeout > 180000) fail('BBA_BROWSER_TIMEOUT_MS must be 1000..180000')

  return {
    base: parsed.origin,
    token,
    readToken,
    taskId: env('BBA_BROWSER_TASK_ID'),
    accountId: env('BBA_BROWSER_ACCOUNT_ID'),
    output: path.resolve(env('BBA_BROWSER_OUTPUT')),
    approvalRef: optional('BBA_BROWSER_APPROVAL_REF', 'ap_product_browser_23001_23002'),
    graphRoot: optional('BBA_BROWSER_GRAPH_ROOT', 'product-fact'),
    proposalId: optional('BBA_BROWSER_PROPOSAL_ID', ''),
    groupId: optional('BBA_BROWSER_GROUP_ID', '21001'),
    summaryId: optional('BBA_BROWSER_SUMMARY_ID', ''),
    summaryVersion: optional('BBA_BROWSER_SUMMARY_VERSION', ''),
    chrome: optional('BBA_CHROME_PATH', CHROME_DEFAULT),
    headless: optional('BBA_BROWSER_HEADLESS', '1') !== '0',
    viewportWidth: Number(optional('BBA_BROWSER_VIEWPORT_WIDTH', '1440')),
    viewportHeight: Number(optional('BBA_BROWSER_VIEWPORT_HEIGHT', '900')),
    timeout
  }
}

async function prepareOutput(output) {
  await fs.mkdir(output, { recursive: true, mode: 0o700 })
  const entries = await fs.readdir(output)
  if (entries.length > 0) fail(`BBA_BROWSER_OUTPUT must be a new or empty directory: ${output}`)
}

function loadPuppeteer() {
  const candidates = [
    optional('BBA_PUPPETEER_PATH', ''),
    path.join(ROOT, 'build', 'node22-stage', 'koishi', 'analysis-collector', 'node_modules', 'puppeteer-core'),
    'puppeteer-core'
  ].filter(Boolean)
  const anchor = path.join(ROOT, 'package.json')
  const resolver = createRequire(anchor)
  const failures = []
  for (const candidate of candidates) {
    try {
      return resolver(candidate)
    } catch (error) {
      failures.push(`${candidate}: ${error.message}`)
    }
  }
  fail(`puppeteer-core is unavailable (${failures.join('; ')}); set BBA_PUPPETEER_PATH`)
}

/* ------------------------------------------------------------------ */
/* Recording / waits                                                   */
/* ------------------------------------------------------------------ */

function createContext(cfg, page, browser) {
  return {
    cfg,
    page,
    browser,
    assertions: new Map(),
    notes: new Map(),
    failures: [],
    scenarios: [],
    requests: [],
    requestSeq: 0,
    byRequest: new Map(),
    captures: new Map(),
    captureSeq: 0,
    injections: [],
    injectionSeq: 0,
    currentScenario: 'startup',
    pageErrors: [],
    dialogs: [],
    report: {
      schema: SCHEMA,
      mode: 'fixture',
      started_utc: utc(),
      target: {
        url: cfg.base,
        task_id: cfg.taskId,
        account_id: cfg.accountId,
        group_id: cfg.groupId,
        approval_ref: cfg.approvalRef,
        headless: cfg.headless,
        viewport: [cfg.viewportWidth, cfg.viewportHeight]
      },
      environment: {
        node: process.version,
        chrome_path: cfg.chrome,
        chrome_version: null
      },
      requests: [],
      captures: {},
      scenarios: [],
      assertions: {},
      assertion_notes: {},
      failed_assertions: [],
      fixture_injections: [],
      screenshots: [],
      login_observed: {},
      graph_observed: {},
      task_observed: {},
      page_errors: [],
      dialogs: [],
      fatal: null
    }
  }
}

function check(ctx, name, ok, detail) {
  if (typeof ok !== 'boolean') throw new Error(`assertion ${name} did not produce a boolean`)
  ctx.assertions.set(name, ok)
  if (detail !== undefined) ctx.notes.set(name, typeof detail === 'string' ? detail : JSON.stringify(detail))
  if (ok) ctx.failures = ctx.failures.filter((failure) => failure !== name)
  else if (!ctx.failures.includes(name)) ctx.failures.push(name)
  return ok
}

function checkEqual(ctx, name, actual, expected) {
  return check(ctx, name, actual === expected, `observed=${JSON.stringify(actual)} expected=${JSON.stringify(expected)}`)
}

async function scenario(ctx, name, fn) {
  const row = { name, ok: true, detail: '', started_utc: utc(), finished_utc: null }
  ctx.report.scenarios.push(row)
  ctx.currentScenario = name
  try {
    const detail = await fn()
    if (detail) row.detail = String(detail)
  } catch (error) {
    row.ok = false
    row.detail = error && error.message ? error.message : String(error)
    check(ctx, `${name}.completed`, false, row.detail)
    if (error instanceof FatalError) throw error
  } finally {
    row.finished_utc = utc()
  }
}

async function waitFor(fn, label, timeout) {
  const deadline = Date.now() + timeout
  for (;;) {
    const value = await fn()
    if (value) return value
    if (Date.now() >= deadline) fail(`timed out waiting for ${label}`)
    await sleep(50)
  }
}

function pathOf(request) {
  try {
    return new URL(request.url()).pathname
  } catch {
    return request.url()
  }
}

function captureTarget(pathname, method) {
  if (pathname === '/v1/accounts') return 'accounts'
  if (pathname === '/v1/tasks') return 'tasks'
  if (/^\/v1\/accounts\/[^/]+\/groups\/[^/]+\/context$/.test(pathname) && method === 'GET') return 'context'
  if (/^\/v1\/tasks\/[^/]+$/.test(pathname)) return 'task'
  if (/^\/v1\/tasks\/[^/]+\/graph$/.test(pathname)) return 'graph'
  if (/^\/v1\/tasks\/[^/]+\/work-items$/.test(pathname)) return 'workItems'
  if (/^\/v1\/tasks\/[^/]+\/plan-proposals\/[^/]+$/.test(pathname) && method === 'GET') return 'proposal'
  if (/^\/v1\/tasks\/[^/]+\/plan-proposals$/.test(pathname) && method === 'POST') return 'proposalCreate'
  if (/\/plan-proposals\/[^/]+\/approve$/.test(pathname)) return 'proposalApprove'
  if (/\/plan-proposals\/[^/]+\/materialize$/.test(pathname)) return 'materialize'
  if (/\/orchestration$/.test(pathname)) return 'orchestration'
  if (/\/controls$/.test(pathname)) return 'control'
  if (pathname === '/v1/login/status') return 'loginStatus'
  if (/^\/v1\/accounts\/[^/]+\/login-sessions$/.test(pathname)) return method === 'GET' ? 'loginList' : 'login'
  if (/^\/v1\/login-sessions\/[^/]+(\/(qr|poll|cancel))?$/.test(pathname)) return 'login'
  if (/^\/v1\/tasks\/[^/]+\/graph-sync\/roots\//.test(pathname)) return 'graphBind'
  if (/^\/v1\/tasks\/[^/]+\/graph-sync$/.test(pathname)) return 'graphSync'
  if (pathname === '/v1/operator/runtime') return 'runtime'
  return null
}

async function installRecorder(ctx) {
  const { page } = ctx

  page.on('request', (request) => {
    const pathname = pathOf(request)
    const entry = {
      seq: ++ctx.requestSeq,
      method: request.method(),
      path: pathname,
      status: undefined,
      error: null,
      source: 'server',
      injection: null,
      scenario: ctx.currentScenario
    }
    ctx.requests.push(entry)
    if (ctx.requests.length > 600) ctx.requests.splice(0, ctx.requests.length - 600)
    ctx.byRequest.set(request, entry)

    const rule = ctx.injections.find((candidate) => !candidate.used && candidate.method === request.method() && candidate.match(pathname))
    if (rule) {
      rule.used = true
      rule.count = (rule.count || 0) + 1
      entry.source = 'fixture-injection'
      entry.injection = rule.id
      if (rule.abort) {
        request.abort('failed').catch(() => {})
        return
      }
      const body = typeof rule.body === 'string' ? rule.body : JSON.stringify(rule.body)
      request
        .respond({
          status: rule.status,
          contentType: rule.contentType || 'application/json; charset=utf-8',
          headers: { 'cache-control': 'no-store', 'x-bba-fixture-injection': rule.id },
          body
        })
        .catch(() => {})
      return
    }
    request.continue().catch(() => {})
  })

  page.on('response', (response) => {
    const entry = ctx.byRequest.get(response.request())
    if (entry) entry.status = response.status()
    if (!entry || entry.source === 'fixture-injection') return
    const key = captureTarget(pathOf(response.request()), response.request().method())
    if (!key) return
    response
      .json()
      .then((body) => {
        ctx.captures.set(key, { seq: ++ctx.captureSeq, status: response.status(), body, method: response.request().method() })
      })
      .catch(() => {})
  })

  page.on('requestfailed', (request) => {
    const entry = ctx.byRequest.get(request)
    if (!entry) return
    entry.error = (request.failure() && request.failure().errorText) || 'request failed'
  })

  page.on('pageerror', (error) => {
    ctx.pageErrors.push(error && error.message ? error.message : String(error))
  })

  page.on('dialog', (dialog) => {
    ctx.dialogs.push({ type: dialog.type(), message: dialog.message() })
    dialog.accept().catch(() => {})
  })

  await page.setRequestInterception(true)
}

function markCaptures(ctx) {
  return ctx.captureSeq
}

async function waitCapture(ctx, key, after = 0, predicate = () => true) {
  return waitFor(() => {
    const capture = ctx.captures.get(key)
    if (!capture || capture.seq <= after) return null
    if (!predicate(capture.body, capture)) return null
    return capture
  }, `capture ${key}`, ctx.cfg.timeout)
}

function dataOf(capture) {
  if (!capture || !capture.body) return null
  return capture.body.data !== undefined ? capture.body.data : capture.body
}

async function waitRequest(ctx, { method, path: matcher, after = 0, status = null, allowFailure = false }) {
  return waitFor(() => {
    return ctx.requests.find((entry) => {
      if (entry.seq <= after) return false
      if (entry.method !== method) return false
      if (typeof matcher === 'string' ? entry.path !== matcher : !matcher.test(entry.path)) return false
      if (status !== null && entry.status !== status) return false
      if (allowFailure && entry.status === undefined && entry.error) return true
      return entry.status !== undefined && (status === null || entry.status === status)
    })
  }, `${method} ${matcher}`, ctx.cfg.timeout)
}

/* ------------------------------------------------------------------ */
/* DOM helpers                                                          */
/* ------------------------------------------------------------------ */

async function selector(page, testid) {
  return `[data-testid="${testid}"]`
}

async function clickTestId(ctx, testid) {
  const target = await selector(ctx.page, testid)
  await ctx.page.waitForSelector(target, { visible: true, timeout: ctx.cfg.timeout })
  await ctx.page.click(target)
}

async function click(ctx, target) {
  await ctx.page.waitForSelector(target, { visible: true, timeout: ctx.cfg.timeout })
  await ctx.page.click(target)
}

async function text(ctx, target) {
  return ctx.page.$eval(target, (element) => (element.textContent || '').trim()).catch(() => null)
}

async function bodyText(ctx) {
  return ctx.page.evaluate(() => document.body.innerText || '')
}

async function waitText(ctx, target, needle) {
  await ctx.page.waitForFunction(
    (sel, wanted) => {
      const element = document.querySelector(sel)
      return Boolean(element && element.textContent && element.textContent.includes(wanted))
    },
    { timeout: ctx.cfg.timeout },
    target,
    needle
  )
}

async function fill(ctx, target, value) {
  await ctx.page.waitForSelector(target, { timeout: ctx.cfg.timeout })
  await ctx.page.$eval(
    target,
    (element, next) => {
      element.value = next
      element.dispatchEvent(new Event('input', { bubbles: true }))
      element.dispatchEvent(new Event('change', { bubbles: true }))
    },
    String(value)
  )
}

async function countTestId(ctx, testid) {
  const target = await selector(ctx.page, testid)
  return ctx.page.$$eval(target, (nodes) => nodes.length).catch(() => 0)
}

async function isDisabled(ctx, testid) {
  const target = await selector(ctx.page, testid)
  return ctx.page.$eval(target, (element) => Boolean(element.disabled)).catch(() => true)
}

async function waitEnabled(ctx, testid) {
  const target = await selector(ctx.page, testid)
  await ctx.page.waitForFunction(
    (sel) => {
      const element = document.querySelector(sel)
      return Boolean(element && !element.disabled)
    },
    { timeout: ctx.cfg.timeout },
    target
  )
}

async function waitEnabledSelector(ctx, target) {
  await ctx.page.waitForFunction(
    (sel) => {
      const element = document.querySelector(sel)
      return Boolean(element && !element.disabled)
    },
    { timeout: ctx.cfg.timeout },
    target
  )
}

async function errorCount(ctx) {
  return ctx.page.$$eval('#error-list .error-item', (nodes) => nodes.length).catch(() => 0)
}

async function waitError(ctx, needle, before) {
  await ctx.page.waitForFunction(
    (wanted, previous) => {
      const items = Array.from(document.querySelectorAll('#error-list .error-item'))
      return items.length > previous && items.some((item) => (item.textContent || '').includes(wanted))
    },
    { timeout: ctx.cfg.timeout },
    needle,
    before
  )
}

async function enableDevMode(ctx) {
  const checked = await ctx.page.$eval('#dev-mode', (element) => element.checked).catch(() => false)
  if (!checked) await click(ctx, '#dev-mode')
  await ctx.page.waitForFunction(() => {
    const fieldset = document.querySelector('#ops-fieldset')
    return fieldset && fieldset.disabled === false
  }, { timeout: ctx.cfg.timeout })
}

async function selectTask(ctx, taskId) {
  for (let attempt = 0; attempt < 12; attempt += 1) {
    const row = await ctx.page.$(`#tasks [data-task-id="${taskId.replace(/"/g, '\\"')}"]`)
    if (row) {
      const mark = markCaptures(ctx)
      const requestMark = ctx.requestSeq
      await row.click()
      await waitRequest(ctx, { method: 'GET', path: `/v1/tasks/${taskId}`, after: requestMark, status: 200 })
      const capture = await waitCapture(ctx, 'task', mark, (body) => dataOf({ body }) && dataOf({ body }).id === taskId)
      return dataOf(capture)
    }
    const more = await ctx.page.$('#tasks-more:not([hidden])')
    if (!more) break
    await more.click()
    await sleep(200)
  }
  fail(`seeded task ${taskId} is not present in the task list`)
}

async function latestTask(ctx) {
  const capture = ctx.captures.get('task')
  return capture ? dataOf(capture) : null
}

async function focusByTab(page, id) {
  await page.evaluate(() => document.body.focus())
  for (let index = 0; index < 20; index += 1) {
    const active = await page.evaluate(() => (document.activeElement && document.activeElement.id) || '')
    if (active === id) return
    await page.keyboard.press('Tab')
  }
  fail(`keyboard focus never reached #${id}`)
}

async function screenshot(ctx, file, { fullPage = true } = {}) {
  const qr = await ctx.page.$$('[data-testid="qr-image"]')
  const target = path.join(ctx.cfg.output, file)
  await ctx.page.screenshot({ path: target, fullPage, mask: qr })
  ctx.report.screenshots.push({
    file,
    path: target,
    full_page: fullPage,
    viewport: ctx.page.viewport() ? [ctx.page.viewport().width, ctx.page.viewport().height] : null,
    masked_qr_images: qr.length,
    at_utc: utc()
  })
}

/* ------------------------------------------------------------------ */
/* Flow                                                                 */
/* ------------------------------------------------------------------ */

async function stepConnect(ctx) {
  const { page, cfg } = ctx
  await page.goto(`${cfg.base}/`, { waitUntil: 'domcontentloaded', timeout: cfg.timeout })
  await page.waitForSelector('#token', { timeout: cfg.timeout })
  const initial = await page.evaluate(() => document.body.dataset.connection)
  check(ctx, 'boot_disconnected', initial === 'disconnected', initial)

  await focusByTab(page, 'token')
  await page.keyboard.type(cfg.token)
  await page.keyboard.press('Enter')
  const mark = markCaptures(ctx)
  try {
    await page.waitForFunction(() => document.body.dataset.ready === 'true', { timeout: cfg.timeout })
  } catch {
    fail('connect never reached body[data-ready=true]; API/token/login surface unavailable')
  }
  check(ctx, 'connect_ready', true, 'Tab + Enter connected with the operator token')
  check(ctx, 'keyboard_connect', true)

  const accounts = await waitCapture(ctx, 'accounts', mark)
  const tasks = await waitCapture(ctx, 'tasks', mark)
  checkEqual(ctx, 'accounts_read_status', accounts.status, 200)
  checkEqual(ctx, 'tasks_read_status', tasks.status, 200)
  const accountItems = (dataOf(accounts) && dataOf(accounts).items) || []
  const taskItems = (dataOf(tasks) && dataOf(tasks).items) || []
  check(ctx, 'seeded_account_visible', accountItems.some((item) => item.id === cfg.accountId), accountItems.map((item) => item.id))
  check(ctx, 'seeded_task_visible', taskItems.some((item) => item.id === cfg.taskId), taskItems.map((item) => item.id))

  const connection = await page.evaluate(() => document.body.dataset.connection)
  checkEqual(ctx, 'connection_connected', connection, 'connected')
  check(ctx, 'token_memory_only_initial', await page.evaluate(() => localStorage.length === 0 && sessionStorage.length === 0 && document.cookie === ''))
  await screenshot(ctx, '01-connected.png')
}

async function stepRuntime(ctx) {
  const mark = markCaptures(ctx)
  const requestMark = ctx.requestSeq
  await clickTestId(ctx, 'runtime-load')
  const request = await waitRequest(ctx, { method: 'GET', path: '/v1/operator/runtime', after: requestMark, status: 200 })
  const capture = await waitCapture(ctx, 'runtime', mark)
  checkEqual(ctx, 'runtime_read_status', request.status, 200)
  const data = dataOf(capture) || {}
  check(ctx, 'runtime_schema_version', Number.isInteger(data.schema_version), data.schema_version)
  check(ctx, 'runtime_worker_reported', Boolean(data.worker), data.worker)
  const transport = data.worker && data.worker.transport
  checkEqual(ctx, 'runtime_worker_fixture_transport', transport, 'fixture')
  check(ctx, 'runtime_proposal_flag', Boolean(data.flags && data.flags.proposal_execution === true), data.flags)
  check(ctx, 'runtime_model_status', Boolean(data.model && typeof data.model === 'object'), data.model && data.model.configured)
  await ctx.page.waitForSelector('[data-testid="runtime-card"]', { timeout: ctx.cfg.timeout })
  ctx.report.task_observed.runtime = {
    schema_version: data.schema_version,
    environment: data.environment,
    worker: data.worker,
    flags: data.flags,
    model: data.model ? { configured: data.model.configured, enabled: data.model.enabled, provider_id: data.model.provider_id, model_id: data.model.model_id } : null
  }
  if (transport !== 'fixture') fail(`refusing mutations: worker transport is ${JSON.stringify(transport)}, expected fixture`)
}

async function stepTaskRead(ctx) {
  const task = await selectTask(ctx, ctx.cfg.taskId)
  checkEqual(ctx, 'task_selected', task && task.id, ctx.cfg.taskId)
  try {
    await ctx.page.waitForFunction(
      (id) => (document.querySelector('#detail-task-id') || {}).textContent === id,
      { timeout: ctx.cfg.timeout },
      ctx.cfg.taskId
    )
  } catch (error) {
    ctx.report.task_observed.detail_failure = await ctx.page.evaluate(() => ({
      task_id_text: document.querySelector('#detail-task-id')?.textContent || '',
      detail_text: (document.querySelector('#detail')?.innerText || '').slice(0, 1000),
      errors: Array.from(document.querySelectorAll('#error-list .error-item')).map((item) => (item.innerText || '').slice(0, 500)).slice(0, 5)
    }))
    fail(`task detail did not render: ${JSON.stringify(ctx.report.task_observed.detail_failure)} (${error.message})`)
  }
  const graph = ctx.captures.get('graph')
  checkEqual(ctx, 'graph_read_status', graph && graph.status, 200)
  check(ctx, 'task_detail_rendered', Boolean(await text(ctx, '#detail')), await text(ctx, '#detail-task-id'))
  ctx.report.task_observed.initial = {
    id: task.id,
    state: task.state,
    version: task.version,
    mode: task.mode || task.execution_mode,
    orchestration_mode: task.orchestration_mode,
    scope_revision: task.scope_revision,
    approval_id: task.approval_id
  }
  checkEqual(ctx, 'task_live_mode', task.mode || task.execution_mode, 'live')

  if (task.state !== 'RUNNING') {
    if (!['DRAFT', 'VALIDATING', 'READY'].includes(task.state)) fail(`seed task state ${task.state} cannot be made RUNNING`)
    await enableDevMode(ctx)
    const mark = markCaptures(ctx)
    const requestMark = ctx.requestSeq
    await fill(ctx, '#controls-version', task.version)
    await fill(ctx, '#controls-reason', 'browser fixture start before proposal flow')
    await waitEnabledSelector(ctx, '[data-control="start"]')
  await ctx.page.click('[data-control="start"]')
    await waitRequest(ctx, { method: 'POST', path: `/v1/tasks/${task.id}/controls`, after: requestMark, status: 202 })
    const running = await waitCapture(ctx, 'task', mark, (body) => dataOf({ body }).state === 'RUNNING')
    checkEqual(ctx, 'task_started_for_flow', dataOf(running).state, 'RUNNING')
  } else {
    check(ctx, 'task_started_for_flow', true, 'seed task already RUNNING')
  }
}

async function stepReportContext(ctx) {
  const { cfg, page } = ctx
  await page.waitForSelector('[data-testid="report-context-account"]', { timeout: cfg.timeout })
  await page.waitForFunction(
    (accountId) => {
      const select = document.querySelector('[data-testid="report-context-account"]')
      return Boolean(select && Array.from(select.options || []).some((option) => option.value === accountId))
    },
    { timeout: cfg.timeout },
    cfg.accountId
  )
  await page.select('[data-testid="report-context-account"]', cfg.accountId)
  await fill(ctx, '[data-testid="report-context-group"]', cfg.groupId)
  if (cfg.summaryId) {
    await fill(ctx, '#report-summary-id', cfg.summaryId)
    await fill(ctx, '#report-summary-version', cfg.summaryVersion || '1')
  }

  const mark = markCaptures(ctx)
  const requestMark = ctx.requestSeq
  await clickTestId(ctx, 'report-context-load')
  const request = await waitRequest(ctx, {
    method: 'GET',
    path: `/v1/accounts/${cfg.accountId}/groups/${cfg.groupId}/context`,
    after: requestMark,
    status: 200
  })
  const capture = await waitCapture(ctx, 'context', mark)
  const data = dataOf(capture) || {}
  checkEqual(ctx, 'report_context_200', request.status, 200)
  checkEqual(ctx, 'report_context_account', data.account_id, cfg.accountId)
  checkEqual(ctx, 'report_context_group', data.group_id, cfg.groupId)
  checkEqual(ctx, 'report_context_source', data.source, 'collector.summary.v1')
  check(ctx, 'report_context_summary_id', /^[a-f0-9]{64}$/.test(String(data.summary_id || '')), String(data.summary_id || '').slice(0, 8) + '…')
  check(
    ctx,
    'report_context_summary_ref',
    data.summary_version > 0 && data.summary_ref === `${data.summary_id}@v${data.summary_version}`,
    { summary_ref: data.summary_ref, summary_id: data.summary_id, summary_version: data.summary_version }
  )
  check(
    ctx,
    'report_context_manifest',
    /^[a-f0-9]{64}$/.test(String(data.source_manifest_sha256 || '')),
    String(data.source_manifest_sha256 || '').slice(0, 8) + '…'
  )
  const topics = Array.isArray(data.topics) ? data.topics : []
  check(ctx, 'report_context_topics', topics.length >= 1, `${topics.length} topics`)
  check(
    ctx,
    'report_context_topic_shape',
    topics.length >= 1 && topics.every((topic) => {
      const refs = Array.isArray(topic.message_refs) ? topic.message_refs : []
      return refs.length > 0 && String(topic.summary || '').trim().length > 0 && String(topic.evidence_kind || '').trim().length > 0
    }),
    topics.map((topic) => ({ summary: topic.summary, evidence_kind: topic.evidence_kind, refs: Array.isArray(topic.message_refs) ? topic.message_refs.length : 0 }))
  )

  await page.waitForSelector('[data-testid="report-context-card"][data-state="ok"]', { timeout: cfg.timeout })
  const cardText = await text(ctx, '[data-testid="report-context-card"]')
  check(ctx, 'report_context_card_source', /collector\.summary\.v1/.test(cardText || ''))
  check(ctx, 'report_context_card_manifest', /Source manifest SHA-256/i.test(cardText || ''))
  const renderedTopics = await page.$$eval('[data-testid="report-context-card"] .topic-card', (nodes) => nodes.length).catch(() => 0)
  checkEqual(ctx, 'report_context_card_topics', renderedTopics, topics.length)
  const renderedRefs = await page.$$eval('[data-testid="report-context-card"] .topic-refs code', (nodes) => nodes.length).catch(() => 0)
  check(ctx, 'report_context_card_refs', renderedRefs >= 1, renderedRefs)
  const refText = await text(ctx, '[data-testid="report-context-ref"]')
  check(
    ctx,
    'report_context_ref_readback',
    Boolean(refText && !/No context loaded/i.test(refText) && refText.includes(`${topics.length} topic`)),
    refText
  )
  const statusText = await text(ctx, '#report-context-status')
  check(ctx, 'report_context_status_readback', /read back from the server/i.test(statusText || ''), statusText)

  ctx.report.report_context_observed = {
    account_id: data.account_id,
    group_id: data.group_id,
    source: data.source,
    summary_id: data.summary_id,
    summary_version: data.summary_version,
    summary_ref: data.summary_ref,
    manifest_prefix: String(data.source_manifest_sha256 || '').slice(0, 8),
    privacy_class: data.privacy_class,
    generated_at: data.generated_at,
    expires_at: data.expires_at,
    topics: topics.map((topic) => ({
      summary: topic.summary,
      evidence_kind: topic.evidence_kind,
      message_refs: Array.isArray(topic.message_refs) ? topic.message_refs.length : 0
    })),
    rendered_topics: renderedTopics,
    rendered_refs: renderedRefs
  }
  await screenshot(ctx, '02-report-context.png')
  return `read ${data.source} for ${data.account_id}/${data.group_id} (${topics.length} topics)`
}

async function stepOrchestration(ctx) {
  const task = await latestTask(ctx)
  const modeText = await text(ctx, '#orchestration-mode')
  const current = task.orchestration_mode
  if (current === 'proposal') {
    check(ctx, 'orchestration_proposal_mode', true, 'seed already reports orchestration_mode=proposal; no write issued')
  } else {
    await enableDevMode(ctx)
    const mark = markCaptures(ctx)
    const requestMark = ctx.requestSeq
    await ctx.page.select('#orchestration-mode-input', 'proposal')
    await fill(ctx, '#orchestration-version', task.version)
    await fill(ctx, '#orchestration-reason', 'browser fixture enable proposal execution')
    await clickTestId(ctx, 'orchestration-submit')
    await waitRequest(ctx, { method: 'POST', path: `/v1/tasks/${task.id}/orchestration`, after: requestMark, status: 202 })
    const updated = await waitCapture(ctx, 'task', mark, (body) => dataOf({ body }).orchestration_mode === 'proposal')
    check(ctx, 'orchestration_proposal_mode', dataOf(updated).orchestration_mode === 'proposal', `version ${dataOf(updated).version}`)
  }
  const modeNow = await text(ctx, '#orchestration-mode')
  check(ctx, 'orchestration_ui_readback', /proposal/i.test(modeNow || ''), modeNow || modeText)
}

async function createProposal(ctx, reason) {
  const task = await latestTask(ctx)
  const mark = markCaptures(ctx)
  const requestMark = ctx.requestSeq
  await fill(ctx, '#proposal-create-version', task.version)
  await fill(ctx, '#proposal-create-scope', task.scope_revision)
  await fill(ctx, '#proposal-create-reason', reason)
  await ctx.page.select('#proposal-create-kind', 'use_template')
  await ctx.page.click('#proposal-create-submit')
  const request = await waitRequest(ctx, { method: 'POST', path: /\/plan-proposals$/, after: requestMark, status: 202 })
  const capture = await waitCapture(ctx, 'proposalCreate', mark)
  await waitCapture(ctx, 'task', mark)
  const receipt = dataOf(capture) || {}
  const id = receipt.resource_id || receipt.id
  if (!id) fail('proposal create receipt did not include a resource id')
  return { id, request, receipt }
}

async function approveProposal(ctx, proposalId) {
  const task = await latestTask(ctx)
  const mark = markCaptures(ctx)
  const requestMark = ctx.requestSeq
  await fill(ctx, '#proposal-approve-id', proposalId)
  await fill(ctx, '#proposal-approve-version', task.version)
  await fill(ctx, '#proposal-approve-ref', ctx.cfg.approvalRef)
  await ctx.page.click('#proposal-approve-submit')
  const request = await waitRequest(ctx, { method: 'POST', path: /\/plan-proposals\/[^/]+\/approve$/, after: requestMark, status: 202 })
  const capture = await waitCapture(ctx, 'proposalApprove', mark)
  await waitCapture(ctx, 'task', mark)
  return { request, receipt: dataOf(capture) || {} }
}

async function readProposalDigest(ctx, proposalId) {
  const mark = markCaptures(ctx)
  const requestMark = ctx.requestSeq
  await fill(ctx, '#report-proposal-ids', proposalId)
  await ctx.page.click('#report-load-submit')
  const request = await waitRequest(ctx, { method: 'GET', path: `/v1/tasks/${ctx.cfg.taskId}/plan-proposals/${proposalId}`, after: requestMark, status: 200 })
  const capture = await waitCapture(ctx, 'proposal', mark, (body) => Boolean(dataOf({ body }) && dataOf({ body }).digest))
  const view = dataOf(capture)
  return { view, request }
}

async function stepProposal(ctx) {
  await enableDevMode(ctx)
  let pid = ctx.cfg.proposalId
  let view = null
  if (pid) {
    check(ctx, 'proposal_precreated', true, pid)
    view = (await readProposalDigest(ctx, pid)).view
    if (view.state !== 'approved' && view.state !== 'applied') {
      const approved = await approveProposal(ctx, pid)
      checkEqual(ctx, 'proposal_approve_202', approved.request.status, 202)
      checkEqual(ctx, 'proposal_approval_ref', approved.receipt.approval_ref || ctx.cfg.approvalRef, ctx.cfg.approvalRef)
      view = (await readProposalDigest(ctx, pid)).view
    }
  } else {
    const created = await createProposal(ctx, 'browser fixture materialize workflow')
    pid = created.id
    checkEqual(ctx, 'proposal_create_202', created.request.status, 202)
    check(ctx, 'proposal_created', Boolean(pid), pid)
    const approved = await approveProposal(ctx, pid)
    checkEqual(ctx, 'proposal_approve_202', approved.request.status, 202)
    checkEqual(ctx, 'proposal_approval_ref', approved.receipt.approval_ref || ctx.cfg.approvalRef, ctx.cfg.approvalRef)
    view = (await readProposalDigest(ctx, pid)).view
  }
  checkEqual(ctx, 'proposal_state_approved', view.state, 'approved')
  check(ctx, 'proposal_digest_read', /^[a-f0-9]{64}$/.test(String(view.digest || '')), String(view.digest || '').slice(0, 8) + '…')
  ctx.report.task_observed.proposal = {
    id: pid,
    state: view.state,
    expected_task_version: view.expected_task_version,
    scope_revision: view.scope_revision,
    digest_prefix: String(view.digest || '').slice(0, 8),
    expires_at: view.expires_at
  }
  return { id: pid, digest: view.digest, expectedVersion: view.expected_task_version }
}

async function submitMaterialize(ctx, { proposalId, digest, version }) {
  await fill(ctx, '#materialize-proposal', proposalId)
  await fill(ctx, '#materialize-version', version)
  await fill(ctx, '#materialize-approval', ctx.cfg.approvalRef)
  await fill(ctx, '#materialize-digest', digest)
  await clickTestId(ctx, 'materialize-submit')
}

async function stepMaterialize(ctx, proposal) {
  const task = await latestTask(ctx)
  if (proposal.expectedVersion !== task.version) {
    fail(`proposal expected_task_version ${proposal.expectedVersion} does not match live task version ${task.version}`)
  }

  // Real stale conflict: no materialization exists yet, so the server reaches
  // its expected_version check and answers 409 without writing anything.
  const staleBefore = await errorCount(ctx)
  const wrongVersion = task.version > 1 ? task.version - 1 : task.version + 1
  const staleMark = ctx.requestSeq
  await submitMaterialize(ctx, { proposalId: proposal.id, digest: proposal.digest, version: wrongVersion })
  const stale = await waitRequest(ctx, { method: 'POST', path: /\/materialize$/, after: staleMark, status: 409 })
  await waitError(ctx, 'HTTP 409', staleBefore)
  checkEqual(ctx, 'stale_materialize_409', stale.status, 409)

  const mark = markCaptures(ctx)
  const requestMark = ctx.requestSeq
  const nextVersion = (await latestTask(ctx)).version
  await submitMaterialize(ctx, { proposalId: proposal.id, digest: proposal.digest, version: nextVersion })
  const materialized = await waitRequest(ctx, { method: 'POST', path: /\/materialize$/, after: requestMark, status: 202 })
  checkEqual(ctx, 'materialize_202', materialized.status, 202)
  await waitText(ctx, '#materialize-status', 'Materialize accepted')

  await waitCapture(ctx, 'task', mark, (body) => dataOf({ body }).version > nextVersion)
  const work = await waitCapture(ctx, 'workItems', mark, (body) => {
    const data = dataOf({ body })
    return Boolean(data && Array.isArray(data.items) && data.items.length >= 1)
  })
  const items = dataOf(work).items || []
  check(ctx, 'work_items_present', items.length >= 1, `${items.length} rows`)
  check(
    ctx,
    'work_items_linked',
    items.every((item) => Boolean(item.id && item.state && item.proposal_id && item.application_id)),
    items.map((item) => ({ id: item.id, state: item.state, proposal_id: item.proposal_id }))
  )
  const rendered = await countTestId(ctx, 'work-item')
  check(ctx, 'work_items_rendered', rendered >= 1, rendered)
  ctx.report.task_observed.work_items = items.map((item) => ({
    id: item.id,
    kind: item.kind,
    state: item.state,
    step_no: item.step_no,
    account_id: item.account_id,
    target_id: item.target_id,
    proposal_id: item.proposal_id,
    application_id: item.application_id,
    depends_on: item.depends_on || []
  }))
  await screenshot(ctx, '03-work-trace.png')
}

async function stepGraph(ctx) {
  const mark = markCaptures(ctx)
  const requestMark = ctx.requestSeq
  await clickTestId(ctx, 'graph-sync-load')
  const request = await waitRequest(ctx, { method: 'GET', path: `/v1/tasks/${ctx.cfg.taskId}/graph-sync`, after: requestMark, status: 200 })
  const capture = await waitCapture(ctx, 'graphSync', mark)
  const data = dataOf(capture) || {}
  const provider = data.provider || {}
  checkEqual(ctx, 'graph_status_read', request.status, 200)
  check(ctx, 'graph_provider_reported', Boolean(data.provider), provider)
  await ctx.page.waitForSelector('[data-testid="graph-sync-provider"]', { timeout: ctx.cfg.timeout })
  check(ctx, 'graph_provider_card_rendered', (await countTestId(ctx, 'graph-sync-provider')) >= 1)
  ctx.report.graph_observed.status = {
    provider: {
      provider: provider.provider,
      configured: provider.configured,
      wired: provider.wired,
      writes_allowed: provider.writes_allowed
    },
    graphs: Array.isArray(data.graphs)
      ? data.graphs.map((graph) => ({
          graph_kind: graph.graph_kind,
          bound: graph.bound,
          external_root: graph.external_root,
          sync_state: graph.sync_state,
          local_revision: graph.local_revision,
          pending_outbox: graph.pending_outbox,
          failed_outbox: graph.failed_outbox
        }))
      : []
  }

  const configured = provider.configured === true
  const wired = provider.wired === true
  const disabled = await isDisabled(ctx, 'graph-bind-submit')
  if (!configured || !wired) {
    check(ctx, 'graph_bind_disabled_without_provider', disabled === true, `configured=${configured} wired=${wired}`)
    const panel = await text(ctx, '#graph-sync-body')
    check(ctx, 'graph_not_configured_readback', /not configured|not_configured|not reported/i.test(panel || ''), panel && panel.slice(0, 120))
    await screenshot(ctx, '04-graph-sync.png')
    return 'provider not configured/wired; bind correctly disabled'
  }

  if (disabled) fail('graph provider is wired but graph-bind-submit stayed disabled')
  if (!ctx.cfg.graphRoot) fail('graph provider is wired but BBA_BROWSER_GRAPH_ROOT is missing; refusing to invent an external root')
  await ctx.page.select('#graph-bind-kind', 'fact').catch(async () => {
    await fill(ctx, '#graph-bind-kind', 'fact')
  })
  await fill(ctx, '#graph-bind-root', ctx.cfg.graphRoot)
  const bindMark = markCaptures(ctx)
  const bindRequestMark = ctx.requestSeq
  await waitEnabled(ctx, 'graph-bind-submit')
  await clickTestId(ctx, 'graph-bind-submit')
  const bind = await waitRequest(ctx, { method: 'PUT', path: `/v1/tasks/${ctx.cfg.taskId}/graph-sync/roots/fact`, after: bindRequestMark, status: 200 })
  checkEqual(ctx, 'graph_bind_200', bind.status, 200)
  const bound = await waitCapture(ctx, 'graphBind', bindMark)
  const boundData = dataOf(bound) || {}
  const graphs = Array.isArray(boundData.graphs) ? boundData.graphs : []
  check(
    ctx,
    'graph_bind_readback',
    graphs.some((graph) => graph.graph_kind === 'fact' && graph.bound === true && graph.external_root === ctx.cfg.graphRoot),
    graphs.map((graph) => ({ graph_kind: graph.graph_kind, bound: graph.bound, external_root: graph.external_root }))
  )
  await ctx.page.waitForSelector('[data-testid="graph-sync-graph"]', { timeout: ctx.cfg.timeout })
  await screenshot(ctx, '04-graph-sync.png')
  return `bound fact root ${ctx.cfg.graphRoot}`
}

async function stepLogin(ctx) {
  await waitFor(() => ctx.captures.get('loginStatus'), 'login status response', ctx.cfg.timeout)
  await ctx.page.waitForFunction(
    () => {
      const status = document.querySelector('#login-provider-status')
      return Boolean(status && status.textContent && !/not checked/i.test(status.textContent))
    },
    { timeout: ctx.cfg.timeout }
  )
  const statusCapture = ctx.captures.get('loginStatus')
  const status = statusCapture ? dataOf(statusCapture) : null
  check(ctx, 'login_status_fixture', Boolean(status && status.fixture === true), status)
  if (!status || status.fixture !== true) fail('login provider is not the explicit fixture; refusing login mutations')
  ctx.report.login_observed.status = status

  await ctx.page.select('#login-account', ctx.cfg.accountId)
  await ctx.page.waitForFunction(
    () => {
      const button = document.querySelector('[data-testid="login-create"]')
      return button && !button.disabled
    },
    { timeout: ctx.cfg.timeout }
  )
  await ctx.page.waitForFunction(
    (needle) => {
      const status = document.querySelector('#login-provider-status')
      return status && status.textContent.includes(needle)
    },
    { timeout: ctx.cfg.timeout },
    'fixture'
  )

  /* session A: generation increments, poll to ONLINE, identity readback */
  let mark = markCaptures(ctx)
  let requestMark = ctx.requestSeq
  await waitEnabled(ctx, 'login-create')
  await clickTestId(ctx, 'login-create')
  await waitRequest(ctx, { method: 'POST', path: `/v1/accounts/${ctx.cfg.accountId}/login-sessions`, after: requestMark, status: 201 })
  let capture = await waitCapture(ctx, 'login', mark)
  let session = dataOf(capture)
  checkEqual(ctx, 'login_create_201', capture.status, 201)
  checkEqual(ctx, 'login_created_state', session.state, 'CREATED')
  await waitText(ctx, '[data-testid="login-state"]', 'CREATED')
  check(ctx, 'login_session_card_rendered', (await countTestId(ctx, 'login-session')) === 1)
  const createdNote = await text(ctx, '[data-testid="action-note"]')
  check(ctx, 'login_action_note_created', /created/i.test(createdNote || ''), createdNote)
  const createdGeneration = session.generation

  mark = markCaptures(ctx)
  requestMark = ctx.requestSeq
  await waitEnabled(ctx, 'login-qr')
  await clickTestId(ctx, 'login-qr')
  await waitRequest(ctx, { method: 'POST', path: new RegExp(`^/v1/login-sessions/${session.id}/qr$`), after: requestMark, status: 200 })
  capture = await waitCapture(ctx, 'login', mark)
  const firstQR = dataOf(capture)
  checkEqual(ctx, 'login_qr_200', capture.status, 200)
  check(ctx, 'login_qr_generation_increment', firstQR.generation > createdGeneration, `${createdGeneration} -> ${firstQR.generation}`)
  await ctx.page.waitForSelector('[data-testid="qr-image"]', { timeout: ctx.cfg.timeout })
  check(ctx, 'login_qr_image_present', (await countTestId(ctx, 'qr-image')) === 1)
  await waitText(ctx, '[data-testid="login-state"]', 'QR_READY')

  mark = markCaptures(ctx)
  requestMark = ctx.requestSeq
  await waitEnabled(ctx, 'login-qr')
  await clickTestId(ctx, 'login-qr')
  await waitRequest(ctx, { method: 'POST', path: new RegExp(`^/v1/login-sessions/${session.id}/qr$`), after: requestMark, status: 200 })
  capture = await waitCapture(ctx, 'login', mark)
  const refreshedQR = dataOf(capture)
  check(ctx, 'login_qr_refresh_generation', refreshedQR.generation > firstQR.generation, `${firstQR.generation} -> ${refreshedQR.generation}`)

  for (const expected of ['SCANNED', 'AUTHENTICATING', 'ONLINE']) {
    mark = markCaptures(ctx)
    requestMark = ctx.requestSeq
    await waitEnabled(ctx, 'login-poll')
  await clickTestId(ctx, 'login-poll')
    await waitRequest(ctx, { method: 'POST', path: new RegExp(`^/v1/login-sessions/${session.id}/poll$`), after: requestMark, status: 200 })
    capture = await waitCapture(ctx, 'login', mark)
    const polled = dataOf(capture)
    checkEqual(ctx, `login_poll_${expected.toLowerCase()}`, polled.state, expected)
    await waitText(ctx, '[data-testid="login-state"]', expected)
    if (expected === 'ONLINE') session = polled
  }
  await ctx.page.waitForSelector('[data-testid="login-identity"]', { timeout: ctx.cfg.timeout })
  const identityText = await text(ctx, '[data-testid="login-identity"]')
  check(ctx, 'login_online_identity_readback', /11001/.test(identityText || '') && /binding checked\s*yes/i.test(identityText || ''), identityText)
  check(ctx, 'login_online_qr_cleared', (await countTestId(ctx, 'qr-image')) === 0)
  ctx.report.login_observed.online = {
    id: session.id,
    state: session.state,
    generation: session.generation,
    self_id: session.identity ? session.identity.self_id : null,
    nickname: session.identity ? session.identity.nickname : null,
    binding_checked: session.identity ? session.identity.binding_checked : null,
    projected: session.projection ? session.projection.projected : null
  }
  await screenshot(ctx, '05-login-online.png')

  /* session B: QR then cancel readback; actions become unavailable */
  mark = markCaptures(ctx)
  requestMark = ctx.requestSeq
  await waitEnabled(ctx, 'login-create')
  await clickTestId(ctx, 'login-create')
  await waitRequest(ctx, { method: 'POST', path: `/v1/accounts/${ctx.cfg.accountId}/login-sessions`, after: requestMark, status: 201 })
  capture = await waitCapture(ctx, 'login', mark)
  const cancelSession = dataOf(capture)
  await waitText(ctx, '[data-testid="login-state"]', 'CREATED')
  mark = markCaptures(ctx)
  requestMark = ctx.requestSeq
  await waitEnabled(ctx, 'login-qr')
  await clickTestId(ctx, 'login-qr')
  await waitRequest(ctx, { method: 'POST', path: new RegExp(`^/v1/login-sessions/${cancelSession.id}/qr$`), after: requestMark, status: 200 })
  await waitCapture(ctx, 'login', mark)
  mark = markCaptures(ctx)
  requestMark = ctx.requestSeq
  await waitEnabled(ctx, 'login-cancel')
  await clickTestId(ctx, 'login-cancel')
  await waitRequest(ctx, { method: 'POST', path: new RegExp(`^/v1/login-sessions/${cancelSession.id}/cancel$`), after: requestMark, status: 200 })
  capture = await waitCapture(ctx, 'login', mark)
  const cancelled = dataOf(capture)
  checkEqual(ctx, 'login_cancel_200', capture.status, 200)
  checkEqual(ctx, 'login_cancelled_readback', cancelled.state, 'CANCELLED')
  await waitText(ctx, '[data-testid="login-state"]', 'CANCELLED')
  check(ctx, 'login_cancelled_actions_disabled', (await isDisabled(ctx, 'login-poll')) === true)
  check(ctx, 'login_cancelled_qr_cleared', (await countTestId(ctx, 'qr-image')) === 0)
  ctx.report.login_observed.cancelled = { id: cancelled.id, state: cancelled.state, generation: cancelled.generation }
  await screenshot(ctx, '06-login-cancelled.png')

  /* session C: expiry readback through a labelled boundary injection */
  mark = markCaptures(ctx)
  requestMark = ctx.requestSeq
  await waitEnabled(ctx, 'login-create')
  await clickTestId(ctx, 'login-create')
  await waitRequest(ctx, { method: 'POST', path: `/v1/accounts/${ctx.cfg.accountId}/login-sessions`, after: requestMark, status: 201 })
  capture = await waitCapture(ctx, 'login', mark)
  let expirySession = dataOf(capture)
  await waitText(ctx, '[data-testid="login-state"]', 'CREATED')
  mark = markCaptures(ctx)
  requestMark = ctx.requestSeq
  await waitEnabled(ctx, 'login-qr')
  await clickTestId(ctx, 'login-qr')
  await waitRequest(ctx, { method: 'POST', path: new RegExp(`^/v1/login-sessions/${expirySession.id}/qr$`), after: requestMark, status: 200 })
  capture = await waitCapture(ctx, 'login', mark)
  expirySession = dataOf(capture)
  const expiredView = {
    ...expirySession,
    state: 'EXPIRED',
    expired: true,
    qr_available: false,
    qr_png_data_url: null,
    next_action: 'create_new_session',
    failure_reason: 'expired'
  }
  const injection = {
    id: `injection-${++ctx.injectionSeq}`,
    label: 'expired login session readback',
    description: 'poll returns a fixture-labelled expired session view; durable server state is unchanged',
    method: 'POST',
    match: (pathname) => new RegExp(`^/v1/login-sessions/${expirySession.id}/poll$`).test(pathname),
    status: 200,
    body: { data: expiredView, meta: { request_id: 'fixture-injection-expired-session' } }
  }
  ctx.injections.push(injection)
  try {
    const pollMark = ctx.requestSeq
    await waitEnabled(ctx, 'login-poll')
  await clickTestId(ctx, 'login-poll')
    await waitRequest(ctx, { method: 'POST', path: new RegExp(`^/v1/login-sessions/${expirySession.id}/poll$`), after: pollMark, status: 200 })
    await waitText(ctx, '[data-testid="action-note"]', 'Session reached EXPIRED')
    await waitText(ctx, '[data-testid="login-state"]', 'QR_READY')
    check(ctx, 'login_expired_readback', (await text(ctx, '[data-testid="action-note"]')).includes('Session reached EXPIRED'), 'fixture-injected terminal response was shown before durable QR_READY reload')
    check(ctx, 'login_expired_durable_state_preserved', (await text(ctx, '[data-testid="login-state"]')) === 'QR_READY')
    check(ctx, 'login_expired_actions_read_back', (await isDisabled(ctx, 'login-poll')) === false, 'the durable QR_READY session remains pollable')
  } finally {
    ctx.injections = ctx.injections.filter((candidate) => candidate !== injection)
    ctx.report.fixture_injections.push({
      id: injection.id,
      label: injection.label,
      description: injection.description,
      method: injection.method,
      path_pattern: `/v1/login-sessions/{id}/poll`,
      status: injection.status,
      used: injection.used === true
    })
  }
  ctx.report.login_observed.expired_injection = { session_id: expirySession.id, injection: injection.id }
}

async function stepReloadGetOnly(ctx) {
  const before = ctx.requests.length
  await ctx.page.reload({ waitUntil: 'domcontentloaded', timeout: ctx.cfg.timeout })
  await ctx.page.waitForSelector('#token', { timeout: ctx.cfg.timeout })
  await fill(ctx, '#token', ctx.cfg.token)
  await click(ctx, '#connect')
  await ctx.page.waitForFunction(() => document.body.dataset.ready === 'true', { timeout: ctx.cfg.timeout })
  const writes = ctx.requests
    .slice(before)
    .filter((entry) => entry.path.startsWith('/v1/') && entry.method !== 'GET')
  check(ctx, 'reload_get_only', writes.length === 0, writes.map((entry) => `${entry.method} ${entry.path}`))
  check(
    ctx,
    'token_memory_only_after_reload',
    await ctx.page.evaluate(() => localStorage.length === 0 && sessionStorage.length === 0 && document.cookie === '')
  )

  // The EXPIRED view above was UI-only; reload shows the durable QR_READY row.
  // Cancel it for real so the fixture leaves no open login session.
  await ctx.page.waitForFunction(
    () => {
      const button = document.querySelector('[data-testid="login-cancel"]')
      return button && !button.disabled
    },
    { timeout: ctx.cfg.timeout }
  ).catch(() => {})
  const cancel = await ctx.page.$eval('[data-testid="login-cancel"]', (element) => !element.disabled).catch(() => false)
  if (cancel) {
    const requestMark = ctx.requestSeq
    await waitEnabled(ctx, 'login-cancel')
  await clickTestId(ctx, 'login-cancel')
    await waitRequest(ctx, { method: 'POST', path: /\/login-sessions\/[^/]+\/cancel$/, after: requestMark, status: 200 })
    await waitText(ctx, '[data-testid="login-state"]', 'CANCELLED')
    check(ctx, 'login_reloaded_session_cancelled', true)
  } else {
    check(ctx, 'login_reloaded_session_cancelled', true, 'no open session after reload')
  }
}

async function stepReadOnly403(ctx) {
  await click(ctx, '#disconnect')
  await ctx.page.waitForSelector('#token', { timeout: ctx.cfg.timeout })
  const readMark = markCaptures(ctx)
  const readRequestMark = ctx.requestSeq
  await fill(ctx, '#token', ctx.cfg.readToken)
  await click(ctx, '#connect')
  await ctx.page.waitForFunction(() => document.body.dataset.ready === 'true', { timeout: ctx.cfg.timeout })
  const read = await waitRequest(ctx, { method: 'GET', path: '/v1/tasks', after: readRequestMark, status: 200 })
  checkEqual(ctx, 'read_token_reads_allowed', read.status, 200)
  void readMark

  const task = await selectTask(ctx, ctx.cfg.taskId)
  const proposal = ctx.report.task_observed.proposal
  if (!proposal) fail('proposal details missing for read-only 403 check')
  const before = await errorCount(ctx)
  const requestMark = ctx.requestSeq
  await submitMaterialize(ctx, { proposalId: proposal.id, digest: ctx.captures.get('proposal') ? dataOf(ctx.captures.get('proposal')).digest : '', version: task.version })
  const denied = await waitRequest(ctx, { method: 'POST', path: /\/materialize$/, after: requestMark, status: 403 })
  await waitError(ctx, 'HTTP 403', before)
  checkEqual(ctx, 'read_only_403', denied.status, 403)
  await waitText(ctx, '#materialize-status', '403').catch(() => {})

  await click(ctx, '#disconnect')
  await ctx.page.waitForSelector('#token', { timeout: ctx.cfg.timeout })
  await fill(ctx, '#token', ctx.cfg.token)
  await click(ctx, '#connect')
  await ctx.page.waitForFunction(() => document.body.dataset.ready === 'true', { timeout: ctx.cfg.timeout })
  await selectTask(ctx, ctx.cfg.taskId)
}

async function stepInjectedFailures(ctx) {
  const task = await latestTask(ctx)
  const proposal = ctx.report.task_observed.proposal
  if (!proposal) fail('proposal details missing for injected failure checks')
  const proposalView = dataOf(ctx.captures.get('proposal'))
  const digest = proposalView && proposalView.digest
  if (!digest) fail('proposal digest missing for injected failure checks')
  const baseBody = (code, message) => ({ error: { code, message, retryable: false }, meta: { request_id: 'fixture-injection' } })

  /* 5xx at the read boundary */
  {
    const injection = {
      id: `injection-${++ctx.injectionSeq}`,
      label: 'task read 5xx',
      description: 'GET task answered with 503 at the HTTP boundary; the UI must surface it',
      method: 'GET',
      match: (pathname) => pathname === `/v1/tasks/${task.id}`,
      status: 503,
      body: baseBody('STATE_STORE_UNAVAILABLE', 'fixture-injected storage failure')
    }
    ctx.injections.push(injection)
    const before = await errorCount(ctx)
    try {
      await ctx.page.click(`#tasks [data-task-id="${task.id}"]`)
      await waitError(ctx, 'HTTP 503', before)
      check(ctx, 'injected_5xx_surfaced', true)
    } finally {
      ctx.injections = ctx.injections.filter((candidate) => candidate !== injection)
      ctx.report.fixture_injections.push({ id: injection.id, label: injection.label, description: injection.description, method: 'GET', path_pattern: `/v1/tasks/${task.id}`, status: 503, used: injection.used === true })
    }
  }

  /* offline transport boundary */
  {
    const injection = {
      id: `injection-${++ctx.injectionSeq}`,
      label: 'task read offline',
      description: 'GET task aborted at the HTTP boundary; the UI must report NETWORK_ERROR',
      method: 'GET',
      match: (pathname) => pathname === `/v1/tasks/${task.id}`,
      abort: true
    }
    ctx.injections.push(injection)
    const before = await errorCount(ctx)
    try {
      await ctx.page.click(`#tasks [data-task-id="${task.id}"]`)
      await waitError(ctx, 'NETWORK_ERROR', before)
      check(ctx, 'injected_offline_surfaced', true)
    } finally {
      ctx.injections = ctx.injections.filter((candidate) => candidate !== injection)
      ctx.report.fixture_injections.push({ id: injection.id, label: injection.label, description: injection.description, method: 'GET', path_pattern: `/v1/tasks/${task.id}`, status: null, used: injection.used === true })
    }
    // Recover the read so later checks use server truth again.
    const mark = markCaptures(ctx)
    await ctx.page.click(`#tasks [data-task-id="${task.id}"]`)
    await waitCapture(ctx, 'task', mark)
  }

  /* empty work trace boundary */
  {
    const injection = {
      id: `injection-${++ctx.injectionSeq}`,
      label: 'empty work items',
      description: 'GET work-items returns an empty page; the UI must show its explicit empty state',
      method: 'GET',
      match: (pathname) => pathname === `/v1/tasks/${task.id}/work-items`,
      status: 200,
      body: { data: { items: [] }, meta: { request_id: 'fixture-injection-empty' } }
    }
    ctx.injections.push(injection)
    try {
      await clickTestId(ctx, 'work-items-load')
      await waitText(ctx, '#work-items', 'No work items')
      check(ctx, 'injected_empty_work_state', true)
    } finally {
      ctx.injections = ctx.injections.filter((candidate) => candidate !== injection)
      ctx.report.fixture_injections.push({ id: injection.id, label: injection.label, description: injection.description, method: 'GET', path_pattern: `/v1/tasks/${task.id}/work-items`, status: 200, used: injection.used === true })
    }
    const mark = markCaptures(ctx)
    await clickTestId(ctx, 'work-items-load')
    await waitCapture(ctx, 'workItems', mark)
  }

  /* unknown delivery outcome (aborted mutation) */
  {
    const injection = {
      id: `injection-${++ctx.injectionSeq}`,
      label: 'materialize delivery unknown',
      description: 'POST materialize aborted after the request left the page; the UI must say delivery is unknown and keep the idempotency key',
      method: 'POST',
      match: (pathname) => pathname === `/v1/tasks/${task.id}/plan-proposals/${proposal.id}/materialize`,
      abort: true
    }
    ctx.injections.push(injection)
    try {
      await submitMaterialize(ctx, { proposalId: proposal.id, digest, version: task.version })
      await waitText(ctx, '#materialize-status', 'unknown')
      check(ctx, 'unknown_delivery_reported', true)
    } finally {
      ctx.injections = ctx.injections.filter((candidate) => candidate !== injection)
      ctx.report.fixture_injections.push({ id: injection.id, label: injection.label, description: injection.description, method: 'POST', path_pattern: `/v1/tasks/${task.id}/plan-proposals/{id}/materialize`, status: null, used: injection.used === true })
    }
  }

  /* expired approval at the mutation boundary */
  {
    const injection = {
      id: `injection-${++ctx.injectionSeq}`,
      label: 'expired approval',
      description: 'materialize returns 409 STATE_CONFLICT approval expired at the HTTP boundary',
      method: 'POST',
      match: (pathname) => pathname === `/v1/tasks/${task.id}/plan-proposals/${proposal.id}/materialize`,
      status: 409,
      body: baseBody('STATE_CONFLICT', 'approval expired')
    }
    ctx.injections.push(injection)
    const before = await errorCount(ctx)
    try {
      await submitMaterialize(ctx, { proposalId: proposal.id, digest, version: task.version })
      await waitText(ctx, '#materialize-status', 'approval expired')
      await waitError(ctx, 'HTTP 409', before)
      check(ctx, 'expired_approval_409_surfaced', true)
    } finally {
      ctx.injections = ctx.injections.filter((candidate) => candidate !== injection)
      ctx.report.fixture_injections.push({ id: injection.id, label: injection.label, description: injection.description, method: 'POST', path_pattern: `/v1/tasks/${task.id}/plan-proposals/{id}/materialize`, status: 409, used: injection.used === true })
    }
  }
}

async function stepTaskControls(ctx) {
  const task = await selectTask(ctx, ctx.cfg.taskId)
  await enableDevMode(ctx)
  if (task.state !== 'RUNNING') fail(`pause readback needs RUNNING, observed ${task.state}`)

  let mark = markCaptures(ctx)
  let requestMark = ctx.requestSeq
  await fill(ctx, '#controls-version', task.version)
  await fill(ctx, '#controls-reason', 'browser fixture pause readback')
  await waitEnabledSelector(ctx, '[data-control="pause"]')
  await ctx.page.click('[data-control="pause"]')
  await waitRequest(ctx, { method: 'POST', path: `/v1/tasks/${task.id}/controls`, after: requestMark, status: 202 })
  const paused = await waitCapture(ctx, 'task', mark, (body) => dataOf({ body }).state === 'PAUSED')
  checkEqual(ctx, 'task_pause_readback', dataOf(paused).state, 'PAUSED')
  await waitText(ctx, '#detail', 'PAUSED').catch(() => {})
  await screenshot(ctx, '07-task-paused.png')

  const pausedTask = dataOf(paused)
  mark = markCaptures(ctx)
  requestMark = ctx.requestSeq
  await fill(ctx, '#controls-version', pausedTask.version)
  await fill(ctx, '#controls-reason', 'browser fixture resume readback')
  await waitEnabledSelector(ctx, '[data-control="resume"]')
  await ctx.page.click('[data-control="resume"]')
  await waitRequest(ctx, { method: 'POST', path: `/v1/tasks/${task.id}/controls`, after: requestMark, status: 202 })
  const resumed = await waitCapture(ctx, 'task', mark, (body) => dataOf({ body }).state === 'RUNNING')
  checkEqual(ctx, 'task_resume_readback', dataOf(resumed).state, 'RUNNING')

  const resumedTask = dataOf(resumed)
  mark = markCaptures(ctx)
  requestMark = ctx.requestSeq
  await fill(ctx, '#controls-version', resumedTask.version)
  await fill(ctx, '#controls-reason', 'browser fixture cancel readback')
  await waitEnabledSelector(ctx, '[data-control="cancel"]')
  await ctx.page.click('[data-control="cancel"]')
  await waitRequest(ctx, { method: 'POST', path: `/v1/tasks/${task.id}/controls`, after: requestMark, status: 202 })
  const cancelled = await waitCapture(ctx, 'task', mark, (body) => ['CANCELLED', 'CANCELLING'].includes(dataOf({ body }).state))
  const state = dataOf(cancelled).state
  check(ctx, 'task_cancel_readback', ['CANCELLED', 'CANCELLING'].includes(state), state)
  ctx.report.task_observed.controls = {
    paused_version: pausedTask.version,
    resumed_version: resumedTask.version,
    cancelled_state: state,
    cancel_dialogs: ctx.dialogs.filter((dialog) => dialog.type === 'confirm').length
  }
  await screenshot(ctx, '08-task-cancelled.png')
}

async function stepKeyboardAndNarrow(ctx) {
  const requestMark = ctx.requestSeq
  await ctx.page.focus('[data-testid="runtime-load"]')
  await ctx.page.keyboard.press('Enter')
  const runtime = await waitRequest(ctx, { method: 'GET', path: '/v1/operator/runtime', after: requestMark, status: 200 })
  checkEqual(ctx, 'keyboard_runtime_activation', runtime.status, 200)
  await screenshot(ctx, '09-keyboard-focus.png', { fullPage: false })

  await ctx.page.setViewport({ width: 360, height: 800, deviceScaleFactor: 1 })
  await sleep(300)
  const overflow = await ctx.page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    innerWidth: window.innerWidth
  }))
  check(ctx, 'narrow_viewport_no_horizontal_overflow', overflow.scrollWidth <= overflow.innerWidth + 2, overflow)
  const visible = {}
  for (const testid of ['login-create', 'materialize-submit', 'runtime-load']) {
    const target = await selector(ctx.page, testid)
    visible[testid] = await ctx.page.$eval(target, (element) => {
      const rect = element.getBoundingClientRect()
      return rect.width > 0 && rect.height > 0 && rect.right <= window.innerWidth + 1
    }).catch(() => false)
  }
  check(ctx, 'narrow_viewport_controls_visible', Object.values(visible).every(Boolean), visible)
  await screenshot(ctx, '10-narrow-viewport.png')
  ctx.report.task_observed.narrow = { overflow, visible }
}

/* ------------------------------------------------------------------ */
/* Report                                                               */
/* ------------------------------------------------------------------ */

function apiRequests(ctx) {
  return ctx.requests
    .filter((entry) => entry.path.startsWith('/v1/'))
    .map((entry) => ({
      seq: entry.seq,
      method: entry.method,
      path: entry.path,
      status: entry.status === undefined ? null : entry.status,
      error: entry.error,
      source: entry.source,
      injection: entry.injection,
      scenario: entry.scenario
    }))
}

async function persistReport(ctx) {
  const serialized = JSON.stringify(ctx.report)
  const secrets = [ctx.cfg.token, ctx.cfg.readToken].filter((value) => value && value.length >= 6)
  for (const secret of secrets) {
    if (serialized.includes(secret)) fail('refusing to persist a report containing a bearer token')
  }
  if (serialized.includes('data:image/png;base64,')) fail('refusing to persist a report containing QR image bytes')

  const assertions = {}
  const notes = {}
  for (const [name, value] of ctx.assertions) {
    assertions[name] = value
    if (ctx.notes.has(name)) notes[name] = ctx.notes.get(name)
  }
  const ok = ctx.failures.length === 0
  ctx.report.finished_utc = utc()
  ctx.report.assertions = assertions
  ctx.report.assertion_notes = notes
  ctx.report.checks = [...ctx.assertions].map(([name, value]) => ({
    name,
    ok: value,
    detail: ctx.notes.get(name) || ''
  }))
  ctx.report.failed_assertions = [...ctx.failures]
  ctx.report.state = ok ? 'PASS' : 'FAIL'
  ctx.report.ok = ok
  ctx.report.requests = apiRequests(ctx)
  ctx.report.screenshot_paths = ctx.report.screenshots.map((entry) => entry.path)
  ctx.report.page_errors = ctx.pageErrors.slice(0, 20)
  ctx.report.dialogs = ctx.dialogs.slice(0, 20)
  ctx.report.scenarios = ctx.report.scenarios

  const reportText = JSON.stringify(ctx.report, null, 2) + '\n'
  await fs.writeFile(path.join(ctx.cfg.output, 'report.json'), reportText, { mode: 0o600 })
  await fs.writeFile(
    path.join(ctx.cfg.output, 'assertions.json'),
    JSON.stringify({ state: ctx.report.state, ok, assertions, assertion_notes: notes, failed: ctx.report.failed_assertions }, null, 2) + '\n',
    { mode: 0o600 }
  )
  return ok
}

async function main() {
  parseArgs(process.argv.slice(2))
  const cfg = readConfig()
  await prepareOutput(cfg.output)

  const puppeteer = loadPuppeteer()
  let browser = null
  let ctx = null
  try {
    browser = await puppeteer.launch({
      executablePath: cfg.chrome,
      headless: cfg.headless,
      args: [
        '--no-sandbox',
        '--disable-setuid-sandbox',
        '--disable-dev-shm-usage',
        '--disable-gpu',
        '--no-first-run',
        '--no-default-browser-check',
        '--disable-background-networking',
        '--disable-sync',
        '--metrics-recording-only',
        '--disable-features=Translate,MediaRouter'
      ]
    })
    const page = await browser.newPage()
    await page.setViewport({ width: cfg.viewportWidth, height: cfg.viewportHeight, deviceScaleFactor: 1 })
    page.setDefaultTimeout(cfg.timeout)
    page.setDefaultNavigationTimeout(cfg.timeout)
    ctx = createContext(cfg, page, browser)
    ctx.report.environment.chrome_version = await browser.version()
    await installRecorder(ctx)

    await scenario(ctx, 'connect', () => stepConnect(ctx))
    await scenario(ctx, 'runtime', () => stepRuntime(ctx))
    if (!ctx.assertions.get('runtime_worker_fixture_transport')) {
      fail('refusing mutations: the server did not report the fixture worker transport')
    }
    await scenario(ctx, 'task-read', () => stepTaskRead(ctx))
    await scenario(ctx, 'report-context', () => stepReportContext(ctx))
    await scenario(ctx, 'orchestration', () => stepOrchestration(ctx))
    const proposal = await (async () => {
      let result
      await scenario(ctx, 'proposal', async () => {
        result = await stepProposal(ctx)
      })
      return result
    })()
    if (!proposal) fail('proposal workflow did not produce a proposal')
    await scenario(ctx, 'materialize', () => stepMaterialize(ctx, proposal))
    await scenario(ctx, 'graph', () => stepGraph(ctx))
    await scenario(ctx, 'login', () => stepLogin(ctx))
    await scenario(ctx, 'reload-get-only', () => stepReloadGetOnly(ctx))
    await scenario(ctx, 'read-only-403', () => stepReadOnly403(ctx))
    await scenario(ctx, 'injected-failures', () => stepInjectedFailures(ctx))
    await scenario(ctx, 'task-controls', () => stepTaskControls(ctx))
    await scenario(ctx, 'keyboard-narrow', () => stepKeyboardAndNarrow(ctx))
    check(ctx, 'run_completed', true)
  } catch (error) {
    if (ctx) {
      ctx.report.fatal = { message: error && error.message ? error.message : String(error), kind: error instanceof FatalError ? 'fatal' : 'error' }
      check(ctx, 'run_completed', false, ctx.report.fatal.message)
    } else {
      console.error(`product-browser: ${error && error.message ? error.message : error}`)
    }
  } finally {
    if (browser) {
      try {
        await browser.close()
      } catch {
        /* Chrome is already gone; the report records the run, not the exit path. */
      }
    }
    if (ctx) {
      try {
        const ok = await persistReport(ctx)
        console.log(`product-browser: ${ctx.report.state} -> ${path.join(ctx.cfg.output, 'report.json')}`)
        if (!ok) {
          console.error(`product-browser: failed assertions: ${ctx.report.failed_assertions.join(', ')}`)
        }
        process.exitCode = ok ? 0 : 1
      } catch (error) {
        console.error(`product-browser: report write failed: ${error.message}`)
        process.exitCode = 1
      }
    } else {
      process.exitCode = 2
    }
  }
}

main().catch((error) => {
  console.error(`product-browser: ${error && error.message ? error.message : error}`)
  process.exitCode = 2
})
