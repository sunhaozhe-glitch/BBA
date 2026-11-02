(() => {
  'use strict'

  const CONTROL_STATES = {
    start: ['DRAFT', 'VALIDATING', 'READY'],
    pause: ['RUNNING', 'WAITING_EXTERNAL', 'VERIFYING'],
    resume: ['PAUSED', 'PAUSING', 'BLOCKED'],
    cancel: ['DRAFT', 'VALIDATING', 'READY', 'RUNNING', 'PAUSING', 'PAUSED', 'WAITING_EXTERNAL', 'VERIFYING', 'BLOCKED', 'CANCELLING']
  }

  const state = {
    token: '',
    devMode: false,
    busy: false,
    selectedTaskId: '',
    accounts: [],
    tasks: [],
    accountCursor: '',
    taskCursor: '',
    task: null,
    graph: null,
    errors: [],
    errorSeq: 0,
    refreshGeneration: 0,
    taskLoadSeq: 0,
    createAccountIds: new Set(),
    proposalIds: new Map(),
    proposalCache: new Map(),
    proposalApprovals: new Map(),
    proposalReceipts: new Map(),
    report: { taskId: '', at: '', entries: [] },
    reportSeq: 0,
    proposalDetail: null,
    proposalDetailSeq: 0,
    login: {
      provider: null,
      providerError: '',
      accountId: '',
      sessions: [],
      selectedId: '',
      session: null,
      note: '',
      noteTone: '',
      listSeq: 0,
      statusSeq: 0
    },
    work: { taskId: '', seq: 0, items: [], hasMore: false, error: '' },
    graphSync: { taskId: '', seq: 0, data: null, writesAllowed: false, error: '' },
    snapshots: { taskId: '', seq: 0, items: [], error: '' },
    runtime: { seq: 0, data: null, error: '' }
  }

  const attempts = {
    create: newAttempt(),
    control: newAttempt(),
    reconcile: newAttempt(),
    proposalCreate: newAttempt(),
    proposalApprove: newAttempt(),
    proposalApply: newAttempt(),
    proposalDetailApprove: newAttempt(),
    proposalDetailApply: newAttempt(),
    loginCreate: newAttempt(),
    loginQR: newAttempt(),
    loginPoll: newAttempt(),
    loginCancel: newAttempt(),
    orchestrationMode: newAttempt(),
    materialize: newAttempt(),
    graphBind: newAttempt()
  }

  const el = {
    tokenInput: document.querySelector('#token'),
    loginPanel: document.querySelector('#login'),
    workspace: document.querySelector('#workspace'),
    status: document.querySelector('#status'),
    refresh: document.querySelector('#refresh'),
    modeIndicator: document.querySelector('#mode-indicator'),
    errorPanel: document.querySelector('#error-panel'),
    errorList: document.querySelector('#error-list'),
    clearErrors: document.querySelector('#clear-errors'),
    devMode: document.querySelector('#dev-mode'),
    devState: document.querySelector('#dev-state'),
    opsFieldset: document.querySelector('#ops-fieldset'),
    opsLockedNote: document.querySelector('#ops-locked-note'),
    accountRows: document.querySelector('#accounts'),
    taskRows: document.querySelector('#tasks'),
    detail: document.querySelector('#detail'),
    detailTaskId: document.querySelector('#detail-task-id'),
    accountsMore: document.querySelector('#accounts-more'),
    tasksMore: document.querySelector('#tasks-more'),
    createForm: document.querySelector('#create-form'),
    createTitle: document.querySelector('#create-title-field'),
    createMode: document.querySelector('#create-mode'),
    createGoal: document.querySelector('#create-goal'),
    createDeadline: document.querySelector('#create-deadline'),
    createCandidates: document.querySelector('#create-candidates'),
    createAccounts: document.querySelector('#create-accounts'),
    createAccountsEmpty: document.querySelector('#create-accounts-empty'),
    createJoinReason: document.querySelector('#create-join-reason'),
    createVerification: document.querySelector('#create-verification'),
    createMaxCandidates: document.querySelector('#create-max-candidates'),
    createMaxAttempts: document.querySelector('#create-max-attempts'),
    createMaxTotal: document.querySelector('#create-max-total'),
    createMaxModel: document.querySelector('#create-max-model'),
    createSubmitCheckbox: document.querySelector('#create-submit-checkbox'),
    createKey: document.querySelector('#create-key'),
    controlsTask: document.querySelector('#controls-task'),
    controlsVersion: document.querySelector('#controls-version'),
    controlsReason: document.querySelector('#controls-reason'),
    controlsHint: document.querySelector('#controls-hint'),
    controlsKey: document.querySelector('#controls-key'),
    reconcileButton: document.querySelector('#reconcile-button'),
    proposalCreateForm: document.querySelector('#proposal-create-form'),
    proposalCreateVersion: document.querySelector('#proposal-create-version'),
    proposalCreateScope: document.querySelector('#proposal-create-scope'),
    proposalCreateKind: document.querySelector('#proposal-create-kind'),
    proposalCreateReason: document.querySelector('#proposal-create-reason'),
    proposalCreateKey: document.querySelector('#proposal-create-key'),
    proposalApproveForm: document.querySelector('#proposal-approve-form'),
    proposalApproveId: document.querySelector('#proposal-approve-id'),
    proposalApproveVersion: document.querySelector('#proposal-approve-version'),
    proposalApproveRef: document.querySelector('#proposal-approve-ref'),
    proposalApproveKey: document.querySelector('#proposal-approve-key'),
    proposalApplyForm: document.querySelector('#proposal-apply-form'),
    proposalApplyId: document.querySelector('#proposal-apply-id'),
    proposalApplyVersion: document.querySelector('#proposal-apply-version'),
    proposalApplyRef: document.querySelector('#proposal-apply-ref'),
    proposalApplyDigest: document.querySelector('#proposal-apply-digest'),
    proposalApplyKey: document.querySelector('#proposal-apply-key'),
    reportTask: document.querySelector('#report-task'),
    reportForm: document.querySelector('#report-load-form'),
    reportProposalIds: document.querySelector('#report-proposal-ids'),
    reportStatus: document.querySelector('#report-status'),
    reportBody: document.querySelector('#report'),
    reportGraphBody: document.querySelector('#report-graph'),
    reportGraphLoad: document.querySelector('#report-graph-load'),
    reportContextForm: document.querySelector('#report-context-form'),
    reportContextAccount: document.querySelector('#report-account'),
    reportContextGroup: document.querySelector('#report-group'),
    reportContextSummaryId: document.querySelector('#report-summary-id'),
    reportContextSummaryVersion: document.querySelector('#report-summary-version'),
    reportContextStatus: document.querySelector('#report-context-status'),
    reportContextRef: document.querySelector('#report-context-ref'),
    reportContextBody: document.querySelector('#report-context'),
    reportContextLoad: document.querySelector('#report-context-load'),
    reportDisclaimer: document.querySelector('#report-disclaimer'),
    proposalDetailId: document.querySelector('#proposal-detail-id'),
    proposalDetailBody: document.querySelector('#proposal-detail'),
    proposalDetailFieldset: document.querySelector('#proposal-detail-ops'),
    proposalDetailLockedNote: document.querySelector('#proposal-detail-locked-note'),
    proposalDetailApproveForm: document.querySelector('#proposal-detail-approve-form'),
    proposalDetailApproveRef: document.querySelector('#proposal-detail-approve-ref'),
    proposalDetailApproveVersion: document.querySelector('#proposal-detail-approve-version'),
    proposalDetailApproveKey: document.querySelector('#proposal-detail-approve-key'),
    proposalDetailApplyForm: document.querySelector('#proposal-detail-apply-form'),
    proposalDetailApplyRef: document.querySelector('#proposal-detail-apply-ref'),
    proposalDetailApplyDigest: document.querySelector('#proposal-detail-apply-digest'),
    proposalDetailApplyVersion: document.querySelector('#proposal-detail-apply-version'),
    proposalDetailApplyKey: document.querySelector('#proposal-detail-apply-key'),
    proposalDetailActionNote: document.querySelector('#proposal-detail-action-note'),
    outcomePanel: document.querySelector('#outcome-panel'),
    outcomeTitle: document.querySelector('#outcome-title'),
    outcomeBadge: document.querySelector('#outcome-badge'),
    outcomeContext: document.querySelector('#outcome-context'),
    outcomeFields: document.querySelector('#outcome-fields'),
    outcomeNote: document.querySelector('#outcome-note'),
    loginProviderStatus: document.querySelector('#login-provider-status'),
    loginAccount: document.querySelector('#login-account'),
    accountReadiness: document.querySelector('#account-readiness'),
    loginSessionList: document.querySelector('#login-session-list'),
    loginCreate: document.querySelector('#login-create'),
    loginQR: document.querySelector('#login-qr'),
    loginPoll: document.querySelector('#login-poll'),
    loginCancel: document.querySelector('#login-cancel'),
    loginActionNote: document.querySelector('#login-action-note'),
    loginSessionBody: document.querySelector('#login-session-body'),
    orchestrationMode: document.querySelector('#orchestration-mode'),
    orchestrationForm: document.querySelector('#orchestration-form'),
    orchestrationModeInput: document.querySelector('#orchestration-mode-input'),
    orchestrationVersion: document.querySelector('#orchestration-version'),
    orchestrationReason: document.querySelector('#orchestration-reason'),
    orchestrationStatus: document.querySelector('#orchestration-status'),
    materializeForm: document.querySelector('#materialize-form'),
    materializeProposal: document.querySelector('#materialize-proposal'),
    materializeVersion: document.querySelector('#materialize-version'),
    materializeApproval: document.querySelector('#materialize-approval'),
    materializeDigest: document.querySelector('#materialize-digest'),
    materializeStatus: document.querySelector('#materialize-status'),
    workItemsLoad: document.querySelector('#work-items-load'),
    workItems: document.querySelector('#work-items'),
    graphSyncLoad: document.querySelector('#graph-sync-load'),
    graphSyncBody: document.querySelector('#graph-sync-body'),
    graphBindForm: document.querySelector('#graph-bind-form'),
    graphBindKind: document.querySelector('#graph-bind-kind'),
    graphBindRoot: document.querySelector('#graph-bind-root'),
    graphBindSubmit: document.querySelector('#graph-bind-submit'),
    graphBindNote: document.querySelector('#graph-bind-note'),
    graphSnapshotsLoad: document.querySelector('#graph-snapshots-load'),
    graphSnapshots: document.querySelector('#graph-snapshots'),
    runtimeLoad: document.querySelector('#runtime-load'),
    runtimeBody: document.querySelector('#runtime-body')
  }

  class ApiError extends Error {
    constructor(status, code, message, requestId) {
      super(message || code)
      this.name = 'ApiError'
      this.status = status
      this.code = code
      this.requestId = requestId || ''
      this.idempotencyKey = ''
      this.expectedVersion = ''
    }
  }

  function newAttempt() {
    return { signature: '', key: '' }
  }

  function escapeHtml(value) {
    return String(value).replace(/[&<>"']/g, (character) => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    })[character])
  }

  const text = (value) => value == null || value === '' ? '—' : String(value)
  const field = (label, value) => `<div><dt>${escapeHtml(label)}</dt><dd>${escapeHtml(text(value))}</dd></div>`

  function randomHex(bytes) {
    const values = new Uint8Array(bytes)
    if (globalThis.crypto && typeof globalThis.crypto.getRandomValues === 'function') {
      try {
        globalThis.crypto.getRandomValues(values)
        return Array.from(values, (value) => value.toString(16).padStart(2, '0')).join('')
      } catch {
        // Fall through to the local fallback below.
      }
    }
    for (let index = 0; index < values.length; index += 1) {
      values[index] = Math.floor(Math.random() * 256)
    }
    return Array.from(values, (value) => value.toString(16).padStart(2, '0')).join('')
  }

  function newIdempotencyKey() {
    if (globalThis.crypto && typeof globalThis.crypto.randomUUID === 'function') {
      try {
        return `web-${globalThis.crypto.randomUUID()}`
      } catch {
        // Fall through to getRandomValues.
      }
    }
    return `web-${randomHex(16)}`
  }

  function newRequestId() {
    return `web-req-${randomHex(8)}`
  }

  function setStatus(message, tone) {
    el.status.textContent = message
    if (tone) {
      el.status.dataset.tone = tone
    } else {
      delete el.status.dataset.tone
    }
  }

  function normalizeError(error) {
    if (error instanceof ApiError) return error
    if (error && typeof error === 'object' && typeof error.message === 'string') {
      return new ApiError(0, 'CLIENT_ERROR', error.message)
    }
    return new ApiError(0, 'CLIENT_ERROR', String(error))
  }

  function annotateError(error, metadata) {
    const info = normalizeError(error)
    for (const [key, value] of Object.entries(metadata || {})) {
      if (value !== undefined && value !== null && value !== '' && !info[key]) info[key] = value
    }
    return info
  }

  function errorHint(info) {
    switch (info.code) {
      case 'TASK_VERSION_CONFLICT':
        return 'The expected version is stale. The task was re-read, but this request was not applied. Review the current version before trying again.'
      case 'STATE_CONFLICT':
        return 'The current task state does not allow this action. No mutation was applied. Refresh and review the state before retrying.'
      case 'IDEMPOTENCY_CONFLICT':
        return 'This key is bound to a different request body. The earlier request may already have been applied; verify the task and latest receipt before using a new key.'
      case 'EXTERNAL_OUTCOME_UNKNOWN':
        return 'The external outcome is unknown. Do not resend the action; use observation or reconcile evidence instead.'
      case 'UNAUTHENTICATED':
        return 'The token was rejected. The session was disconnected; the token was not persisted.'
      case 'FEATURE_DISABLED':
        return 'This route is disabled on the server. Start the server with the explicit development route enabled before retrying.'
      case 'RESOURCE_NOT_FOUND':
        return 'The route or resource was not found. The server deployment allowlist may not expose this development write route, or optional proposal routes may be unmounted.'
      case 'ACTION_FORBIDDEN':
        return 'The configured resolver token does not grant the permission required by this action.'
      case 'VALIDATION_FAILED':
        return 'The Task Engine rejected the request shape or values. Correct the form and submit again.'
      case 'NETWORK_ERROR':
        return 'Delivery is uncertain. Retry only with the same Idempotency-Key while the request body is unchanged.'
      case 'CLIENT_ERROR':
        return 'The console hit a client-side failure. If a mutation request may already have been sent, keep its Idempotency-Key and verify task state before retrying; form validation failures did not send a request.'
      default:
        if (info.status === 404) return 'The route or resource was not found. The server deployment allowlist may not expose this development write route, or optional proposal routes may be unmounted.'
        if (info.status === 401) return 'The token was rejected. The session was disconnected; the token was not persisted.'
        if (info.status === 403) return 'The server denied this request. Check the resolver permissions and whether the development route is enabled.'
        if (info.status === 409) return 'A conflict was reported. The request was not treated as successful; review the task state and receipt before retrying.'
        return ''
    }
  }

  function inlineErrorText(info) {
    switch (info.code) {
      case 'FEATURE_DISABLED':
        return 'Proposal routes are unmounted on this server. Start it with -development-operations to enable the authenticated proposal reads and writes; the report cannot read basis without that route.'
      case 'UNAUTHENTICATED':
        return 'The token was rejected. Reconnect with a token whose SHA-256 digest is present in the local resolver file; this fixture console cannot refresh a token on its own.'
      case 'ACTION_FORBIDDEN':
        return 'The token authenticated but does not grant the permission this read or write needs. Basis reads require task.read; approve requires task.approve and apply requires task.control.'
      case 'IDEMPOTENCY_CONFLICT':
        return 'This Idempotency-Key is already bound to a different request body. Verify the proposal state and the earlier receipt before using a new key.'
      case 'TASK_VERSION_CONFLICT':
        return 'The task version moved since this page read it. Reload the task and the proposal, then decide again; this console never retries automatically.'
      case 'STATE_CONFLICT':
        return 'The proposal or task state does not allow this action. Nothing was changed by this request; reload the proposal before deciding again.'
      case 'RESOURCE_NOT_FOUND':
        return 'The proposal route or the proposal ID was not found for this task. Check the task selection and the durable proposal ID; the console does not guess IDs.'
      case 'NETWORK_ERROR':
        return 'Delivery is uncertain. Keep the same Idempotency-Key and verify task and proposal state before retrying.'
      default:
        if (info.status === 401) return 'Authentication required. The token was rejected and the session was disconnected; reconnect to continue.'
        if (info.status === 403) return 'The local server denied this request. The proposal routes may be unmounted, or the token may lack the required permission.'
        if (info.status === 404) return 'Nothing was found at this proposal or task path. Check the selected task and the proposal ID.'
        if (info.status === 409) return 'A conflict was reported. The request was not treated as successful; review the durable proposal state before retrying.'
        return info.message || 'The request failed.'
    }
  }

  function reportError(context, error) {
    const info = normalizeError(error)
    state.errors.unshift({
      id: ++state.errorSeq,
      at: new Date().toISOString(),
      context,
      status: info.status,
      code: info.code,
      message: info.message,
      requestId: info.requestId,
      idempotencyKey: info.idempotencyKey,
      expectedVersion: info.expectedVersion,
      hint: errorHint(info)
    })
    state.errors = state.errors.slice(0, 20)
    renderErrors()
    setStatus(`${context} failed: ${info.code}`, 'error')
    if (info.status === 401 && state.token) disconnect()
  }

  function renderErrors() {
    el.errorPanel.hidden = state.errors.length === 0
    el.errorList.replaceChildren()
    for (const entry of state.errors) {
      const item = document.createElement('li')
      item.className = 'error-item'
      if (entry.status === 409) item.classList.add('conflict')

      const head = document.createElement('div')
      head.className = 'error-head'

      const context = document.createElement('strong')
      context.textContent = entry.context

      const code = document.createElement('span')
      code.className = 'error-code'
      code.textContent = `${entry.status ? `HTTP ${entry.status} · ` : ''}${entry.code}`

      const dismiss = document.createElement('button')
      dismiss.type = 'button'
      dismiss.className = 'text-button'
      dismiss.dataset.dismissError = String(entry.id)
      dismiss.textContent = 'Dismiss'

      head.append(context, code, dismiss)

      const message = document.createElement('p')
      message.className = 'error-message'
      message.textContent = entry.message

      item.append(head, message)

      if (entry.hint) {
        const hint = document.createElement('p')
        hint.className = 'error-hint'
        hint.textContent = entry.hint
        item.append(hint)
      }

      const meta = []
      if (entry.requestId) meta.push(`request ${entry.requestId}`)
      if (entry.idempotencyKey) meta.push(`Idempotency-Key ${entry.idempotencyKey}`)
      if (entry.expectedVersion) meta.push(`expected version ${entry.expectedVersion}`)
      meta.push(new Date(entry.at).toLocaleTimeString())
      const metaLine = document.createElement('p')
      metaLine.className = 'error-meta'
      metaLine.textContent = meta.join(' · ')
      item.append(metaLine)

      el.errorList.append(item)
    }
  }

  async function api(path, options = {}) {
    const method = options.method || 'GET'
    const headers = {
      Authorization: `Bearer ${state.token}`,
      Accept: 'application/json',
      'X-Request-Id': newRequestId()
    }
    const init = {
      method,
      headers,
      cache: 'no-store',
      credentials: 'same-origin'
    }
    if (options.body !== undefined) {
      headers['Content-Type'] = 'application/json'
      init.body = JSON.stringify(options.body)
    }
    if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey

    let response
    try {
      response = await fetch(path, init)
    } catch (error) {
      throw new ApiError(0, 'NETWORK_ERROR', error && error.message ? error.message : 'Network request failed')
    }

    let raw
    try {
      raw = await response.text()
    } catch (error) {
      throw new ApiError(0, 'NETWORK_ERROR', error && error.message ? error.message : 'Response body could not be read')
    }
    let payload = null
    if (raw) {
      try {
        payload = JSON.parse(raw)
      } catch {
        payload = null
      }
    }

    if (!response.ok) {
      const error = payload && typeof payload.error === 'object' && payload.error !== null ? payload.error : {}
      const code = error.code || `HTTP_${response.status}`
      const message = error.message || response.statusText || `HTTP ${response.status}`
      const requestId = payload && payload.meta && payload.meta.request_id
        ? payload.meta.request_id
        : (response.headers.get('X-Request-Id') || '')
      throw new ApiError(response.status, code, message, requestId)
    }
    return payload
  }

  // A key is retained after uncertain delivery and reused only while the exact
  // request body is unchanged, so a manual retry cannot duplicate a write.
  function getAttemptKey(attempt, signature) {
    if (!attempt.key || attempt.signature !== signature) {
      attempt.signature = signature
      attempt.key = newIdempotencyKey()
    }
    return attempt.key
  }

  function clearAttempt(attempt) {
    attempt.signature = ''
    attempt.key = ''
  }

  function clearTaskScopedState() {
    for (const attempt of [attempts.control, attempts.reconcile, attempts.proposalCreate, attempts.proposalApprove, attempts.proposalApply, attempts.proposalDetailApprove, attempts.proposalDetailApply]) {
      clearAttempt(attempt)
    }
    el.controlsKey.textContent = ''
    el.proposalCreateKey.textContent = ''
    el.proposalApproveKey.textContent = ''
    el.proposalApplyKey.textContent = ''
    el.proposalApproveId.value = ''
    el.proposalApproveRef.value = ''
    el.proposalApplyId.value = ''
    el.proposalApplyRef.value = ''
    el.proposalApplyDigest.value = ''
    resetProposalDetail()
    resetReport()
    resetWorkItems()
    resetGraphSync()
    resetSnapshots()
    renderOrchestration()
    el.orchestrationStatus.textContent = ''
    el.materializeStatus.textContent = ''
  }

  function noteAttemptKey(element, key, expectedVersion) {
    const parts = [`Idempotency-Key ${key}`]
    if (expectedVersion) parts.push(`expected version ${expectedVersion}`)
    parts.push('retries reuse this key only while the request body is unchanged')
    element.textContent = parts.join(' · ')
  }

  function setBusy(busy, message) {
    state.busy = Boolean(busy)
    document.body.classList.toggle('is-busy', state.busy)
    el.workspace.setAttribute('aria-busy', String(state.busy))
    el.opsFieldset.disabled = state.busy || !state.devMode
    el.proposalDetailFieldset.disabled = state.busy || !state.devMode
    refreshProposalActionAvailability()
    renderLoginAvailability()
    renderGraphBindAvailability()
    if (message) setStatus(message)
  }

  function setDevMode(enabled) {
    state.devMode = Boolean(enabled)
    el.devMode.checked = state.devMode
    el.opsFieldset.disabled = state.busy || !state.devMode
    el.proposalDetailFieldset.disabled = state.busy || !state.devMode
    refreshProposalActionAvailability()
    el.opsLockedNote.hidden = state.devMode
    el.proposalDetailLockedNote.hidden = state.devMode
    el.modeIndicator.textContent = state.devMode ? 'Dev ops ON' : 'Dev ops off'
    el.modeIndicator.dataset.state = state.devMode ? 'on' : 'off'
    el.devState.textContent = state.devMode ? 'Enabled' : 'Disabled'
    el.devState.dataset.tone = state.devMode ? 'warn' : 'off'
  }

  function renderAccounts() {
    renderReportAccountOptions()
    document.querySelector('#account-count').textContent = String(state.accounts.length)
    el.accountRows.replaceChildren()
    if (!state.accounts.length) {
      el.accountRows.innerHTML = '<p class="subtle">No accounts.</p>'
      return
    }
    for (const account of state.accounts) {
      const row = document.createElement('div')
      row.className = 'row static-row'
      row.innerHTML = `<span class="row-title">${escapeHtml(account.id)}</span><span class="row-meta"><span>${escapeHtml(text(account.platform))}</span><span>${escapeHtml(text(account.connection_state))} / ${escapeHtml(text(account.login_state))}</span><span>risk ${escapeHtml(text(account.risk_state))}</span><span>control ${escapeHtml(text(account.control_state))}</span><span>v${escapeHtml(text(account.version))}</span></span>`
      el.accountRows.append(row)
    }
  }

  function renderTasks() {
    document.querySelector('#task-count').textContent = String(state.tasks.length)
    el.taskRows.replaceChildren()
    if (!state.tasks.length) {
      el.taskRows.innerHTML = '<p class="subtle">No tasks.</p>'
      return
    }
    for (const task of state.tasks) {
      const row = document.createElement('button')
      row.type = 'button'
      row.className = 'row'
      row.dataset.taskId = task.id
      row.setAttribute('aria-current', task.id === state.selectedTaskId ? 'true' : 'false')
      row.innerHTML = `<span class="row-title">${escapeHtml(text(task.title))}</span><span class="row-meta"><span>${escapeHtml(text(task.state))}</span><span>${escapeHtml(text(task.type))}</span><span>${escapeHtml(text(task.confirmed_unique_count))} / ${escapeHtml(text(task.goal_count))} confirmed</span><span>${escapeHtml(text(task.pending_external_count))} pending</span><span>${escapeHtml(text(task.unknown_execution_count))} unknown</span><span>v${escapeHtml(text(task.version))}</span></span>`
      row.addEventListener('click', () => {
        if (state.busy) return
        void loadTask(task.id)
      })
      el.taskRows.append(row)
    }
  }

  function markSelectedTask() {
    for (const row of el.taskRows.querySelectorAll('[data-task-id]')) {
      row.setAttribute('aria-current', row.dataset.taskId === state.selectedTaskId ? 'true' : 'false')
    }
  }

  function renderCreateAccounts() {
    const available = new Set(state.accounts.map((account) => account.id))
    state.createAccountIds = new Set(Array.from(state.createAccountIds).filter((id) => available.has(id)))

    el.createAccounts.replaceChildren()
    el.createAccountsEmpty.hidden = state.accounts.length > 0
    if (!state.accounts.length) return

    for (const account of state.accounts) {
      const label = document.createElement('label')
      label.className = 'check-row account-option'

      const input = document.createElement('input')
      input.type = 'checkbox'
      input.name = 'allowed-account'
      input.value = account.id
      input.checked = state.createAccountIds.has(account.id)

      const body = document.createElement('span')
      const name = document.createElement('strong')
      name.textContent = account.id
      const meta = document.createElement('span')
      meta.className = 'row-meta'
      meta.textContent = `${text(account.platform)} · ${text(account.connection_state)} / ${text(account.login_state)} · risk ${text(account.risk_state)}`
      body.append(name, meta)
      label.append(input, body)
      el.createAccounts.append(label)
    }
  }

  function humanize(key) {
    const value = String(key).replace(/[_-]+/g, ' ').trim()
    return value ? value.charAt(0).toUpperCase() + value.slice(1) : String(key)
  }

  function formatValue(value) {
    if (value == null) return ''
    if (typeof value === 'string') return value
    if (typeof value === 'number' || typeof value === 'boolean') return String(value)
    if (Array.isArray(value)) {
      return value.map((item) => formatValue(item)).filter(Boolean).join(', ')
    }
    try {
      const serialized = JSON.stringify(value)
      if (serialized && serialized.length > 240) return `${serialized.slice(0, 237)}…`
      return serialized || ''
    } catch {
      return '[unserializable value]'
    }
  }

  function isObject(value) {
    return value !== null && typeof value === 'object' && !Array.isArray(value)
  }

  function collectStatusRows(sources, pattern, limit = 16) {
    const rows = []
    const seen = new Set()

    function add(label, value) {
      const formatted = formatValue(value)
      if (!formatted) return
      const key = `${label}\u0000${formatted}`
      if (seen.has(key)) return
      seen.add(key)
      rows.push({ label, value: formatted })
    }

    function walk(label, value, depth = 0) {
      if (value == null || rows.length >= limit) return
      if (Array.isArray(value)) {
        add(label, value)
        return
      }
      if (isObject(value)) {
        if (depth >= 4) {
          add(label, value)
          return
        }
        for (const [childKey, childValue] of Object.entries(value)) {
          walk(`${label} · ${humanize(childKey)}`, childValue, depth + 1)
          if (rows.length >= limit) break
        }
        return
      }
      add(label, value)
    }

    for (const [prefix, source] of sources) {
      if (!isObject(source)) continue
      for (const [key, value] of Object.entries(source)) {
        if (!pattern.test(key)) continue
        walk(prefix ? `${prefix} · ${humanize(key)}` : humanize(key), value)
        if (rows.length >= limit) break
      }
    }
    return rows.slice(0, limit)
  }

  function renderStatusRows(rows, emptyMessage) {
    if (!rows.length) return `<p class="subtle">${escapeHtml(emptyMessage)}</p>`
    return `<dl class="detail-grid compact">${rows.map((row) => field(row.label, row.value)).join('')}</dl>`
  }

  function graphSyncRows(task, graph) {
    if (!graph) return []
    const rows = []
    const taskVersion = Number(task.version)
    const graphVersion = Number(graph.task_version)
    const versionLag = Number.isFinite(taskVersion) && Number.isFinite(graphVersion)
      ? taskVersion - graphVersion
      : null
    const reportedState = firstDefined(
      graph.sync_state,
      graph.graph_sync_state,
      graph.sync_status,
      graph.projection_state
    )
    const reportedLag = firstDefined(
      graph.sync_lag_seconds,
      graph.sync_lag,
      graph.lag_seconds,
      graph.graph_lag_seconds
    )

    if (reportedState != null && reportedState !== '') {
      rows.push({ label: 'Sync state', value: formatValue(reportedState), source: 'reported' })
    } else if (versionLag != null) {
      rows.push({ label: 'Sync state', value: versionLag === 0 ? 'in_sync' : versionLag > 0 ? 'lagging' : 'ahead', source: 'derived' })
    }
    if (reportedLag != null && reportedLag !== '') {
      const lag = Number(reportedLag)
      rows.push({ label: 'Sync lag', value: Number.isFinite(lag) ? formatAge(lag) : formatValue(reportedLag), source: 'reported' })
    } else if (versionLag != null) {
      const lagValue = versionLag === 0 ? '0 (in sync)' : versionLag > 0 ? `${versionLag} versions behind` : `${-versionLag} versions ahead`
      rows.push({ label: 'Graph sync lag (task versions)', value: lagValue, source: 'derived' })
    }
    if (Number.isFinite(graphVersion)) {
      rows.push({ label: 'Graph task version', value: `${graphVersion} (task ${Number.isFinite(taskVersion) ? taskVersion : '—'})` })
    }
    if (graph.scope_revision != null) {
      rows.push({ label: 'Graph scope revision', value: formatValue(graph.scope_revision) })
    }
    if (graph.revision != null) {
      rows.push({ label: 'Graph revision', value: formatValue(graph.revision) })
    }
    if (graph.projected_at) {
      rows.push({ label: 'Projected at', value: formatValue(graph.projected_at) })
      const projected = new Date(graph.projected_at)
      if (!Number.isNaN(projected.getTime()) && projected.getTime() > 0) {
        rows.push({ label: 'Projection age (client clock)', value: formatAge((Date.now() - projected.getTime()) / 1000), source: 'derived' })
      }
    }
    if (graph.snapshot_digest) {
      rows.push({ label: 'Snapshot digest', value: formatValue(graph.snapshot_digest) })
    }
    return rows
  }

  function formatAge(seconds) {
    if (!Number.isFinite(seconds)) return '—'
    const value = Math.max(0, Math.round(seconds))
    if (value < 60) return `${value}s`
    if (value < 3600) return `${Math.floor(value / 60)}m ${value % 60}s`
    return `${Math.floor(value / 3600)}h ${Math.floor((value % 3600) / 60)}m`
  }

  function firstDefined(...values) {
    for (const value of values) {
      if (value !== undefined && value !== null) return value
    }
    return undefined
  }

  function statusRowsHtml(rows) {
    return rows.map((row) => {
      const source = row.source ? `<span class="field-source">${escapeHtml(row.source)}</span>` : ''
      return `<div><dt>${escapeHtml(row.label)}${source}</dt><dd>${escapeHtml(text(row.value))}</dd></div>`
    }).join('')
  }

  function metric(label, value, note, tone) {
    return `<div class="metric" data-tone="${tone}"><span class="metric-label">${escapeHtml(label)}</span><span class="metric-value">${escapeHtml(text(value))}</span><span class="metric-note">${escapeHtml(note)}</span></div>`
  }

  function renderTaskDetail(task, graph) {
    const anchors = graph ? (graph.anchors || []) : []
    const facts = graph ? (graph.facts || []) : []
    const nodes = new Map(graph ? (graph.nodes || []).map((node) => [node.id, node]) : [])
    const edges = graph ? (graph.edges || []) : []

    const graphRows = [
      ...anchors.slice(0, 200).map((anchor) => `<div class="graph-row"><strong>${escapeHtml(text(anchor.kind))}</strong><span>${escapeHtml(text(anchor.account_id || anchor.target_id))}</span><span>${escapeHtml(text(anchor.eligibility || (anchor.baseline_covered ? 'baseline' : 'in_scope')))}</span></div>`),
      ...facts.slice(0, 200).map((fact) => `<div class="graph-row"><strong>${escapeHtml(text(fact.kind))} · ${escapeHtml(text(fact.data && fact.data.state ? fact.data.state : 'confirmed'))}</strong><span>${escapeHtml(text(fact.account_id))} → ${escapeHtml(text(fact.target_id))}</span><span>${escapeHtml(text(fact.evidence_ref))}</span></div>`)
    ].join('')

    const edgeRows = edges.slice(0, 200).map((edge) => {
      const from = nodes.get(edge.from)
      const to = nodes.get(edge.to)
      return `<div class="graph-row"><strong>${escapeHtml(text(edge.kind))}</strong><span>${escapeHtml(text(from && from.kind))} → ${escapeHtml(text(to && to.kind))}</span><span>${escapeHtml(text(from && from.data && (from.data.account_id || from.data.target_id || from.data.fact_id)))} → ${escapeHtml(text(to && to.data && (to.data.account_id || to.data.target_id || to.data.fact_id)))}</span></div>`
    }).join('')

    const graphTruncated = anchors.length > 200 || facts.length > 200 || edges.length > 200

    const sources = [['', task], ['Graph', graph]]
    for (const [prefix, source] of [['Task', task], ['Graph', graph]]) {
      if (!isObject(source)) continue
      for (const [key, value] of Object.entries(source)) {
        if (isObject(value) && /status|readiness/i.test(key)) {
          sources.push([`${prefix} ${humanize(key)}`, value])
        }
      }
    }
    let nodeSources = 0
    if (graph) {
      for (const node of graph.nodes || []) {
        if (nodeSources >= 200) break
        if (!isObject(node.data) || !Object.keys(node.data).some((key) => /analysis|summary|model|sync|lag/i.test(key))) continue
        sources.push([`Node ${node.id}`, node.data])
        nodeSources += 1
      }
    }
    const analysisRows = collectStatusRows(sources, /analysis|summary/i)
    const modelRows = collectStatusRows(sources, /model/i)
    const syncRows = collectStatusRows(sources, /sync|lag/i)
    const reportedSync = graphSyncRows(task, graph)

    const metrics = [
      metric('Goal', task.goal_count, 'target slots', 'goal'),
      metric('Confirmed unique', task.confirmed_unique_count, 'valid evidence', 'confirmed'),
      metric('Pending external', task.pending_external_count, 'not success', 'pending'),
      metric('Unknown execution', task.unknown_execution_count, 'never auto-retry', 'unknown'),
      metric('Reserved', task.reserved_target_count, 'still possible', 'reserved')
    ].join('')

    const graphOnlySync = []
    const seenSync = new Set()
    for (const row of [
      ...reportedSync,
      ...syncRows.map((entry) => ({ label: entry.label, value: entry.value, source: 'returned' }))
    ]) {
      const key = `${String(row.label).toLowerCase()}\u0000${row.value}`
      if (seenSync.has(key)) continue
      seenSync.add(key)
      graphOnlySync.push(row)
    }

    el.detailTaskId.textContent = task.id
    el.detail.dataset.testid = 'task-detail'
    el.detail.dataset.taskState = task.state || ''
    el.detail.dataset.unknownExecution = String(task.unknown_execution_count == null ? '' : task.unknown_execution_count)
    el.detail.innerHTML = `
      <div class="metric-grid">${metrics}</div>
      <dl class="detail-grid">
        ${field('Task ID', task.id)}
        ${field('Title', task.title)}
        ${field('State', task.state)}
        ${field('Type', task.type)}
        ${field('Version', task.version)}
        ${field('Execution mode', task.execution_mode)}
        ${field('Goal', task.goal_count)}
        ${field('Confirmed unique', task.confirmed_unique_count)}
        ${field('Reserved', task.reserved_target_count)}
        ${field('Pending external', task.pending_external_count)}
        ${field('Unknown execution', task.unknown_execution_count)}
        ${field('Scope revision', task.scope_revision)}
        ${field('Deadline', task.deadline_at)}
        ${field('Created', task.created_at)}
        ${field('Blocked reason', task.blocked_reason)}
      </dl>
      <section class="status-section" aria-labelledby="sync-title">
        <div class="section-heading"><h2 id="sync-title">Analysis, model and graph sync</h2></div>
        <div class="status-columns">
          <div>
            <h3>Analysis</h3>
            ${renderStatusRows(analysisRows, 'No analysis status fields were returned by this API response.')}
          </div>
          <div>
            <h3>Model</h3>
            ${renderStatusRows(modelRows, 'No model status fields were returned by this API response.')}
          </div>
        </div>
        <h3>Graph sync</h3>
        ${graphOnlySync.length ? `<dl class="detail-grid compact">${statusRowsHtml(graphOnlySync)}</dl>` : '<p class="subtle">Graph sync fields are unavailable because the graph response was not loaded.</p>'}
      </section>
      <section class="graph-section" aria-labelledby="graph-title">
        <div class="section-heading">
          <h2 id="graph-title">Fact and goal graph</h2>
          <span class="count">${graph ? `r${escapeHtml(text(graph.revision))} · v${escapeHtml(text(graph.task_version))} · scope ${escapeHtml(text(graph.scope_revision))}` : 'unavailable'}</span>
        </div>
        ${graph
          ? `<p class="subtle">Digest ${escapeHtml(text(graph.snapshot_digest))} · ${facts.length} trusted facts · ${edges.length} relations · ${anchors.length} scope anchors · ${nodes.size} nodes${graphTruncated ? ' · rendering first 200 rows per list' : ''}</p>
             <div class="graph-rows">${graphRows || '<p class="subtle">No anchors or trusted facts.</p>'}</div>
             <h3>Relations</h3>
             <div class="graph-rows">${edgeRows || '<p class="subtle">No relations.</p>'}</div>`
          : '<p class="subtle">The graph projection could not be loaded. Task counters above remain the authoritative task projection.</p>'}
      </section>`
  }

  function syncTaskFormDefaults(task) {
    el.controlsTask.textContent = `${task.title} · ${task.id}`
    el.controlsVersion.value = String(task.version)
    el.proposalCreateVersion.value = String(task.version)
    el.proposalCreateScope.value = String(task.scope_revision)
    el.proposalApproveVersion.value = String(task.version)
    el.proposalApplyVersion.value = String(task.version)
    renderControlAvailability()
  }

  function renderControlAvailability() {
    const task = state.task
    for (const button of document.querySelectorAll('[data-control]')) {
      const action = button.dataset.control
      const allowed = Boolean(task) && CONTROL_STATES[action].includes(task.state)
      button.disabled = !allowed
      button.title = allowed ? '' : `Action not available in state ${task ? task.state : 'none'}`
    }
    el.reconcileButton.disabled = !task
    if (!task) {
      el.controlsTask.textContent = 'No task selected'
      el.controlsHint.textContent = 'Select a task to enable state-aware controls. Start, pause and resume remain subject to server scope and permission checks.'
    } else {
      el.controlsHint.textContent = `Loaded ${task.state} v${task.version}. Start, pause and resume are enabled only for states the Task Engine contract allows; the server still revalidates scope, deadline and permissions.`
    }
  }

  function resetDetail() {
    state.task = null
    state.graph = null
    state.taskLoadSeq += 1
    el.detailTaskId.textContent = ''
    el.detail.innerHTML = '<p class="subtle">Select a task.</p>'
    el.controlsVersion.value = '1'
    el.proposalCreateVersion.value = '1'
    el.proposalCreateScope.value = '1'
    el.proposalApproveVersion.value = '1'
    el.proposalApplyVersion.value = '1'
    renderControlAvailability()
    resetProposalDetail()
    resetReport()
  }

  async function loadTask(id, options = {}) {
    if (state.selectedTaskId !== id) {
      clearTaskScopedState()
    }
    state.selectedTaskId = id
    markSelectedTask()
    const sequence = ++state.taskLoadSeq
    if (!options.quiet) {
      el.detailTaskId.textContent = id
      el.detail.innerHTML = '<p class="subtle">Loading task…</p>'
    }
    try {
      const [taskResult, graphResult] = await Promise.allSettled([
        api(`/v1/tasks/${encodeURIComponent(id)}`),
        api(`/v1/tasks/${encodeURIComponent(id)}/graph`)
      ])
      if (sequence !== state.taskLoadSeq) return false
      if (taskResult.status === 'rejected') throw taskResult.reason

      const task = taskResult.value.data
      const graph = graphResult.status === 'fulfilled' && graphResult.value ? graphResult.value.data : null
      state.task = task
      state.graph = graph
      renderTaskDetail(task, graph)
      renderReportGraph()
      syncTaskFormDefaults(task)
      syncReportForTask(task)
      renderOrchestration()
      resetWorkItems()
      resetGraphSync()
      resetSnapshots()
      void loadWorkItems({ quiet: true })
      void loadGraphSync({ quiet: true })

      if (graphResult.status === 'rejected') {
        reportError('Load task graph', graphResult.reason)
      }
      return true
    } catch (error) {
      if (sequence !== state.taskLoadSeq) return false
      state.task = null
      state.graph = null
      el.detailTaskId.textContent = ''
      el.detail.innerHTML = `<p class="error">${escapeHtml(normalizeError(error).message)}</p>`
      renderControlAvailability()
      resetProposalDetail()
      resetReport()
      reportError('Load task', error)
      if (normalizeError(error).status === 404) {
        state.selectedTaskId = ''
        clearTaskScopedState()
        markSelectedTask()
      }
      return false
    }
  }

  async function loadAccountsPage(cursor = '') {
    const query = new URLSearchParams({ limit: '50' })
    if (cursor) query.set('cursor', cursor)
    return api(`/v1/accounts?${query}`)
  }

  async function loadTasksPage(cursor = '') {
    const query = new URLSearchParams({ limit: '50' })
    if (cursor) query.set('cursor', cursor)
    return api(`/v1/tasks?${query}`)
  }

  // Refresh is GET-only by construction. It never reposts a mutation or replays
  // a retained Idempotency-Key; only explicit form submits can create writes.
  async function refresh() {
    if (!state.token) return
    const generation = ++state.refreshGeneration
    setStatus('Loading accounts and tasks…')
    const [accountsResult, tasksResult] = await Promise.allSettled([
      loadAccountsPage(''),
      loadTasksPage('')
    ])
    if (generation !== state.refreshGeneration) return

    const accountsPage = accountsResult.status === 'fulfilled' ? accountsResult.value : null
    const tasksPage = tasksResult.status === 'fulfilled' ? tasksResult.value : null
    state.accounts = accountsPage ? (accountsPage.data && accountsPage.data.items) || [] : []
    state.tasks = tasksPage ? (tasksPage.data && tasksPage.data.items) || [] : []
    state.accountCursor = accountsPage && accountsPage.meta && accountsPage.meta.has_more ? (accountsPage.meta.next_cursor || '') : ''
    state.taskCursor = tasksPage && tasksPage.meta && tasksPage.meta.has_more ? (tasksPage.meta.next_cursor || '') : ''
    el.accountsMore.hidden = !state.accountCursor
    el.tasksMore.hidden = !state.taskCursor

    renderAccounts()
    renderTasks()
    renderCreateAccounts()
    renderLoginAccounts()
    if (!state.login.provider) void loadLoginStatus({ quiet: true })
    if (state.login.accountId) await loadLoginSessions(state.login.accountId, { quiet: true })

    if (state.selectedTaskId) {
      await loadTask(state.selectedTaskId, { quiet: true })
    } else {
      resetDetail()
    }
    if (generation !== state.refreshGeneration || !state.token) return

    for (const [label, result] of [['Load accounts', accountsResult], ['Load tasks', tasksResult]]) {
      if (result.status === 'rejected') reportError(label, result.reason)
    }
    if (!accountsPage && !tasksPage) {
      document.body.dataset.connection = 'error'
      document.body.dataset.ready = 'false'
      return
    }

    const requestId = accountsPage && accountsPage.meta && accountsPage.meta.request_id ? accountsPage.meta.request_id : ''
    setStatus(`Connected · ${state.accounts.length} accounts · ${state.tasks.length} tasks${requestId ? ` · request ${requestId}` : ''}`)
    document.body.dataset.connection = 'connected'
    document.body.dataset.ready = 'true'
  }

  async function loadMoreAccounts() {
    try {
      const page = await loadAccountsPage(state.accountCursor)
      state.accounts = state.accounts.concat((page.data && page.data.items) || [])
      state.accountCursor = page.meta && page.meta.has_more ? (page.meta.next_cursor || '') : ''
      el.accountsMore.hidden = !state.accountCursor
      renderAccounts()
      renderCreateAccounts()
      setStatus(`Loaded ${state.accounts.length} accounts`)
    } catch (error) {
      reportError('Load more accounts', error)
    }
  }

  async function loadMoreTasks() {
    try {
      const page = await loadTasksPage(state.taskCursor)
      state.tasks = state.tasks.concat((page.data && page.data.items) || [])
      state.taskCursor = page.meta && page.meta.has_more ? (page.meta.next_cursor || '') : ''
      el.tasksMore.hidden = !state.taskCursor
      renderTasks()
      setStatus(`Loaded ${state.tasks.length} tasks`)
    } catch (error) {
      reportError('Load more tasks', error)
    }
  }

  // Group analysis report and proposal trace.
  //
  // A proposal's basis is provenance recorded at generation time. It is never
  // an approval, an apply capability or proof of verified group semantics, and
  // this console only reads proposal IDs the operator supplied or this tab
  // created. There is no proposal list route, so nothing is synthesized.

  function proposalKey(taskId, proposalId) {
    return `${taskId}\u0000${proposalId}`
  }

  function rememberProposalId(taskId, proposalId) {
    if (!taskId || !proposalId) return
    const ids = state.proposalIds.get(taskId) || []
    if (!ids.includes(proposalId)) {
      ids.push(proposalId)
      state.proposalIds.set(taskId, ids)
    }
  }

  function rememberedProposalIds(taskId) {
    return (state.proposalIds.get(taskId) || []).slice()
  }

  function parseProposalIds(raw) {
    const seen = new Set()
    const ids = []
    for (const value of String(raw).split(/[\s,]+/).map((part) => part.trim()).filter(Boolean)) {
      if (value.length > 128 || /[/?#]/.test(value)) {
        throw new ApiError(0, 'FORM_INVALID', `"${value.slice(0, 40)}" is not a single path-safe proposal ID of 1–128 characters.`)
      }
      if (seen.has(value)) continue
      seen.add(value)
      ids.push(value)
    }
    if (!ids.length) {
      throw new ApiError(0, 'FORM_INVALID', 'Enter at least one proposal ID. This console has no proposal list route and will not guess IDs.')
    }
    if (ids.length > 20) {
      throw new ApiError(0, 'FORM_INVALID', 'Read at most 20 proposal IDs at a time.')
    }
    return ids
  }

  function proposalTone(value) {
    switch (String(value || '').toLowerCase()) {
      case 'applied':
        return 'positive'
      case 'approved':
        return 'info'
      case 'pending':
        return 'warn'
      case 'rejected':
      case 'expired':
        return 'danger'
      default:
        return 'neutral'
    }
  }

  function basisHasSummary(basis) {
    return basis.summary_id != null || basis.summary_version != null || basis.evidence_kinds != null || basis.source_manifest_sha256 != null
  }

  function graphAnchorSummary(anchorId) {
    if (!state.graph) {
      return { found: false, text: 'The graph projection is not loaded, so this anchor cannot be checked against it.' }
    }
    const anchor = (state.graph.anchors || []).find((item) => item.id === anchorId)
    if (!anchor) {
      return { found: false, text: `Not present in the graph projection read at revision ${text(state.graph.revision)}.` }
    }
    const parts = [`kind ${text(anchor.kind)}`]
    if (anchor.target_id) parts.push(`target ${anchor.target_id}`)
    if (anchor.account_id) parts.push(`account ${anchor.account_id}`)
    if (anchor.eligibility) parts.push(`eligibility ${anchor.eligibility}`)
    if (anchor.baseline_covered != null) parts.push(anchor.baseline_covered ? 'baseline covered' : 'baseline not covered')
    return { found: true, text: `Present in the graph projection (${parts.join(' · ')}).` }
  }

  function renderBasisRow(basis, index) {
    const hasSummary = basisHasSummary(basis)
    const partialSummary = hasSummary && (
      basis.summary_id == null ||
      basis.summary_version == null ||
      basis.evidence_kinds == null ||
      basis.source_manifest_sha256 == null
    )
    const anchor = basis.anchor_id ? graphAnchorSummary(String(basis.anchor_id)) : null
    const notes = []
    if (!hasSummary) {
      notes.push('No live Summary at generation time: summary_id, summary_version, evidence_kinds and source_manifest_sha256 are all null. The recorded selection rule says generation used task-ledger truth only; no Summary version may be inferred.')
    } else if (partialSummary) {
      notes.push('This basis row is incomplete against the documented four-field grouping (all four are null or all four are present). The values are shown exactly as returned; do not infer the missing Summary fields.')
    } else {
      notes.push(`Recorded Summary reference: id ${basis.summary_id}, version ${basis.summary_version}. These are the values generation consulted; this console has no operator Summary read route to re-verify them.`)
    }
    if (!basis.anchor_id) {
      notes.push('No graph anchor was recorded for this target. That is not evidence that the target is absent from the current graph projection.')
    }
    return `<div class="basis-row">
      <div class="basis-heading">
        <strong>${escapeHtml(text(basis.account_id))} → ${escapeHtml(text(basis.group_id))}</strong>
        <span class="row-meta"><span>target ${escapeHtml(text(basis.target_id))}</span><span>basis row ${index + 1}</span></span>
      </div>
      <dl class="detail-grid compact">
        ${field('Account', basis.account_id)}
        ${field('Group', basis.group_id)}
        ${field('Target', basis.target_id)}
        ${field('Anchor', basis.anchor_id == null ? 'none recorded' : basis.anchor_id)}
        ${field('Summary ID', basis.summary_id == null ? 'none recorded' : basis.summary_id)}
        ${field('Summary version', basis.summary_version == null ? 'none recorded' : basis.summary_version)}
        ${field('Evidence kinds', basis.evidence_kinds == null ? 'none recorded' : formatValue(basis.evidence_kinds))}
        ${field('Source manifest SHA-256', basis.source_manifest_sha256 == null ? 'none recorded' : basis.source_manifest_sha256)}
        ${field('Recorded at', basis.created_at)}
      </dl>
      ${anchor ? `<p class="basis-anchor">${escapeHtml(anchor.text)}</p>` : ''}
      <p class="basis-reason"><strong>Selection reason:</strong> ${escapeHtml(text(basis.selection_reason))}</p>
      ${notes.map((note) => `<p class="basis-note">${escapeHtml(note)}</p>`).join('')}
    </div>`
  }

  function renderReportEntry(entry) {
    const proposalId = entry.proposalId
    if (!entry.view) {
      const info = entry.error ? normalizeError(entry.error) : new ApiError(0, 'CLIENT_ERROR', 'The proposal was not read.')
      return `<article class="report-card report-error" data-report-proposal="${escapeHtml(proposalId)}">
        <div class="section-heading"><h3>Proposal ${escapeHtml(proposalId)}</h3><span class="badge" data-tone="danger">${escapeHtml(info.code)}</span></div>
        <p class="error-message">${escapeHtml(info.message || info.code)}</p>
        <p class="hint">${escapeHtml(inlineErrorText(info))}</p>
      </article>`
    }
    const view = entry.view
    const basis = Array.isArray(view.basis) ? view.basis : []
    const basisAt = basis.length > 0 && basis[0].created_at ? basis[0].created_at : null
    const resolvedId = view.proposal_id || proposalId
    return `<article class="report-card" data-report-proposal="${escapeHtml(resolvedId)}">
      <div class="section-heading">
        <h3>Proposal ${escapeHtml(resolvedId)}</h3>
        <span class="badge" data-tone="${proposalTone(view.state)}">${escapeHtml(text(view.state))}</span>
      </div>
      <dl class="detail-grid compact">
        ${field('Proposal ID', resolvedId)}
        ${field('Task ID', view.task_id)}
        ${field('Proposal kind', view.proposal && view.proposal.proposal_kind)}
        ${field('Digest', view.digest)}
        ${field('Expected task version', view.expected_task_version)}
        ${field('Scope revision', view.scope_revision)}
        ${field('Expires at', view.expires_at)}
        ${field('Basis recorded at', basisAt)}
        ${field('Basis rows', basis.length)}
      </dl>
      <p class="subtle">The rows below are what generation consulted. They are not a decision record and grant no approve or apply authority.</p>
      ${basis.length
        ? basis.map((row, index) => renderBasisRow(row, index)).join('')
        : '<p class="basis-note">The proposal response returned an empty basis array, so no generation-time Summary or anchor reference is recorded here. The console shows the gap instead of guessing.</p>'}
      <div class="button-row"><button class="text-button" type="button" data-open-proposal="${escapeHtml(resolvedId)}">Open proposal detail and trace</button></div>
    </article>`
  }

  let reportGraphBusy = false
  let reportContextBusy = false

  function renderReportAccountOptions() {
    if (!el.reportContextAccount) return
    const previous = el.reportContextAccount.value
    const options = ['<option value="">Select an account</option>']
    for (const account of state.accounts) {
      const label = [account.id, account.self_id ? `self ${account.self_id}` : '', text(account.connection_state), text(account.login_state)].filter(Boolean).join(' · ')
      options.push(`<option value="${escapeHtml(account.id)}">${escapeHtml(label)}</option>`)
    }
    el.reportContextAccount.innerHTML = options.join('')
    if (state.accounts.some((account) => account.id === previous)) el.reportContextAccount.value = previous
  }

  function reportContextField(label, value, source = 'reported') {
    return `<tr><th scope="row">${escapeHtml(label)}</th><td>${escapeHtml(value)}</td><td class="subtle">${escapeHtml(source)}</td></tr>`
  }

  function renderReportContext(data, error) {
    if (!el.reportContextBody) return
    if (error) {
      const code = error && error.code ? error.code : 'REQUEST_FAILED'
      el.reportContextBody.innerHTML = `<article class="report-card" data-testid="report-context-card" data-state="error"><h3>GroupContext report unavailable</h3><p><code>${escapeHtml(code)}</code> — ${escapeHtml(error && error.message ? error.message : 'The server rejected or failed this read. No fallback content is shown.')}</p><p class="basis-note">401 requires reconnecting; 403 means the token lacks operator.read/task.read or a runtime credential was supplied; 404 means the account, group or requested Summary version is outside this workspace or not live; 5xx means retry later. Nothing was sent.</p></article>`
      if (el.reportContextRef) el.reportContextRef.textContent = `Read failed · ${code}`
      return
    }
    if (!data) {
      el.reportContextBody.innerHTML = '<p class="subtle">Select an account and enter a group ID, then load the read-only GroupContext report.</p>'
      if (el.reportContextRef) el.reportContextRef.textContent = 'No context loaded'
      return
    }
    const rows = []
    rows.push(reportContextField('Account', text(data.account_id)))
    rows.push(reportContextField('Group', text(data.group_id)))
    rows.push(reportContextField('Source', text(data.source)))
    if (data.summary_ref) rows.push(reportContextField('Summary reference', text(data.summary_ref)))
    if (data.summary_id) rows.push(reportContextField('Summary ID', text(data.summary_id)))
    if (data.summary_version != null) rows.push(reportContextField('Summary version', text(data.summary_version)))
    if (data.source_manifest_sha256) rows.push(reportContextField('Source manifest SHA-256', text(data.source_manifest_sha256)))
    rows.push(reportContextField('Privacy class', text(data.privacy_class)))
    rows.push(reportContextField('Generated at', formatDateTime(data.generated_at)))
    rows.push(reportContextField('Expires at', formatDateTime(data.expires_at)))
    const expires = new Date(data.expires_at)
    if (!Number.isNaN(expires.getTime())) {
      rows.push(reportContextField('Expiry state', expires.getTime() <= Date.now() ? 'expired by server timestamp' : 'live at client read time', 'derived (client clock)'))
    }
    if (data.activity_window) {
      const window = data.activity_window
      rows.push(reportContextField('Activity window', `${formatDateTime(window.start)} → ${formatDateTime(window.end)}`, 'reported'))
      rows.push(reportContextField('Window messages', text(window.message_count)))
    }
    if (data.active_speaker_account_id) rows.push(reportContextField('Active speaker account', text(data.active_speaker_account_id)))
    if (data.cooldown_until) rows.push(reportContextField('Speaker cooldown until', formatDateTime(data.cooldown_until)))
    if (data.daily_remaining != null) rows.push(reportContextField('Daily remaining', text(data.daily_remaining)))
    if (data.participation_goal) rows.push(reportContextField('Participation goal', text(data.participation_goal)))
    if (data.confidence != null) {
      rows.push(reportContextField('Confidence', text(data.confidence)))
    } else {
      rows.push(reportContextField('Confidence', 'not reported by GroupContext.v1', 'absent'))
    }
    const topics = Array.isArray(data.topics) ? data.topics : []
    const topicCards = topics.length
      ? topics.map((topic, index) => {
        const refs = Array.isArray(topic.message_refs) ? topic.message_refs : []
        const shownRefs = refs.slice(0, 20)
        const refsHtml = shownRefs.length
          ? `<ul class="topic-refs">${shownRefs.map((ref) => `<li><code>${escapeHtml(text(ref))}</code></li>`).join('')}</ul>${refs.length > shownRefs.length ? `<p class="subtle">…and ${refs.length - shownRefs.length} more message refs.</p>` : ''}`
          : '<p class="subtle">No message refs reported for this topic.</p>'
        return `<article class="topic-card" data-topic-index="${index}"><h4>Topic ${index + 1}</h4><p>${escapeHtml(text(topic.summary))}</p><p class="subtle">Evidence kind: ${escapeHtml(text(topic.evidence_kind))} · confidence: ${escapeHtml(topic.confidence != null ? text(topic.confidence) : 'not reported')}</p>${refsHtml}</article>`
      }).join('')
      : `<p class="subtle">${text(data.source) === 'backend.chat-policy' ? 'No live Summary is available, so the GroupContext degrades to the chat policy alone; no topics are reported.' : 'The Summary reports an empty topic list.'}</p>`
    el.reportContextBody.innerHTML = `<article class="report-card" data-testid="report-context-card" data-state="ok">
      <header><h3>GroupContext · ${escapeHtml(text(data.account_id))} / ${escapeHtml(text(data.group_id))}</h3><p class="subtle">Read from <code>GET /v1/accounts/{account_id}/groups/{group_id}/context</code>. Untrusted reference data; no send, permit or execution was created.</p></header>
      <div class="table-wrap"><table class="field-table"><tbody>${rows.join('')}</tbody></table></div>
      <h4>Summary topics (${topics.length})</h4>
      ${topicCards}
      <p class="basis-note">Confidence is shown only if the response contains it; GroupContext.v1 currently has no confidence field, so absence is displayed rather than guessed. Topic text and message refs are the stored Summary content, never generated in the browser.</p>
    </article>`
    if (el.reportContextRef) el.reportContextRef.textContent = `${data.summary_ref || data.summary_id || 'chat-policy'} · ${topics.length} topic${topics.length === 1 ? '' : 's'}`
  }

  async function loadReportContext(event) {
    if (event) event.preventDefault()
    if (reportContextBusy) return
    const accountId = el.reportContextAccount ? el.reportContextAccount.value : ''
    const groupId = el.reportContextGroup ? el.reportContextGroup.value.trim() : ''
    const summaryId = el.reportContextSummaryId ? el.reportContextSummaryId.value.trim() : ''
    const summaryVersion = el.reportContextSummaryVersion ? el.reportContextSummaryVersion.value.trim() : ''
    if (!accountId) {
      el.reportContextStatus.textContent = 'Select an account from the server list.'
      el.reportContextAccount.focus()
      return
    }
    if (!/^[0-9]+$/.test(groupId)) {
      el.reportContextStatus.textContent = 'Group ID must be the numeric QQ group ID bound to this account.'
      el.reportContextGroup.focus()
      return
    }
    if (summaryId && !/^[1-9][0-9]*$/.test(summaryVersion)) {
      el.reportContextStatus.textContent = 'A summary ID requires its exact summary_version (>= 1).'
      el.reportContextSummaryVersion.focus()
      return
    }
    if (!summaryId && summaryVersion) {
      el.reportContextStatus.textContent = 'summary_version alone is not a selector; provide the stored summary_id too or leave both blank for the newest live Summary.'
      el.reportContextSummaryId.focus()
      return
    }
    reportContextBusy = true
    if (el.reportContextLoad) el.reportContextLoad.disabled = true
    el.reportContextStatus.textContent = 'Reading GroupContext…'
    try {
      const query = summaryId ? `?summary_id=${encodeURIComponent(summaryId)}&summary_version=${encodeURIComponent(summaryVersion)}` : ''
      const payload = await api(`/v1/accounts/${encodeURIComponent(accountId)}/groups/${encodeURIComponent(groupId)}/context${query}`)
      renderReportContext(payload.data)
      el.reportContextStatus.textContent = 'GroupContext read back from the server. No message was sent.'
    } catch (error) {
      renderReportContext(null, error)
      el.reportContextStatus.textContent = `GroupContext read failed: ${error.code || 'REQUEST_FAILED'}.`
      if (error.status === 401) reportError(error, 'GroupContext report')
    } finally {
      reportContextBusy = false
      if (el.reportContextLoad) el.reportContextLoad.disabled = false
    }
  }

  function graphAnalysisReportRows(graph) {
    const rows = []
    if (!graph || !Array.isArray(graph.nodes)) return rows
    for (const node of graph.nodes) {
      if (!node || typeof node !== 'object') continue
      const label = node.title || node.label || node.id || node.kind || 'graph node'
      collectStatusRows([[label, node]], /analysis|summary/i).forEach((row) => {
        rows.push({ label: `${label} · ${row.label}`, value: row.value, source: row.source })
      })
      if (rows.length >= 40) break
    }
    return rows
  }

  function renderReportGraph() {
    if (!el.reportGraphBody) return
    const task = state.task
    if (el.reportGraphLoad) {
      el.reportGraphLoad.disabled = !task || reportGraphBusy
      el.reportGraphLoad.textContent = reportGraphBusy ? 'Loading graph report…' : 'Load task graph report'
    }
    if (!task) {
      el.reportGraphBody.innerHTML = '<p class="subtle">Select a task to read its graph projection.</p>'
      return
    }
    const graph = state.graph
    if (!graph) {
      el.reportGraphBody.innerHTML = '<p class="subtle">The task graph read returned no projection for this task. Use Load task graph report to retry the same GET; nothing is fabricated locally.</p>'
      return
    }
    const rows = []
    const fields = [
      ['revision', 'Graph revision'],
      ['task_version', 'Graph task version'],
      ['scope_revision', 'Scope revision'],
      ['manifest_digest', 'Manifest digest'],
      ['digest', 'Reported digest'],
      ['projected_at', 'Projected at']
    ]
    for (const [key, label] of fields) {
      if (graph[key] != null && graph[key] !== '') rows.push({ label, value: formatValue(graph[key]) })
    }
    for (const [key, label] of [['anchors', 'Anchors'], ['facts', 'Facts'], ['edges', 'Edges'], ['nodes', 'Nodes']]) {
      if (Array.isArray(graph[key])) rows.push({ label: `${label} reported`, value: String(graph[key].length) })
    }
    const fieldTable = rows.length
      ? `<table class="field-table"><tbody>${rows.map((row) => `<tr><th scope="row">${escapeHtml(row.label)}</th><td>${escapeHtml(row.value)}</td></tr>`).join('')}</tbody></table>`
      : '<p class="subtle">The graph response reports no revision, scope or anchor fields.</p>'
    const analysisRows = graphAnalysisReportRows(graph)
    const analysisTable = analysisRows.length
      ? `<div class="table-wrap"><table><thead><tr><th scope="col">Scope / field</th><th scope="col">Reported value</th><th scope="col">Source</th></tr></thead><tbody>${analysisRows.map((row) => `<tr><td>${escapeHtml(row.label)}</td><td>${escapeHtml(row.value)}</td><td>${escapeHtml(row.source || 'reported')}</td></tr>`).join('')}</tbody></table></div>`
      : '<p class="subtle">The graph projection reports no summary or analysis status fields. The console does not substitute statistical keywords or a model-generated report.</p>'
    el.reportGraphBody.innerHTML = `<article class="report-card" data-testid="report-graph-card">
      <header><h3>Task graph projection</h3><p class="subtle">Task ${escapeHtml(task.id)} · read from GET /v1/tasks/{task_id}/graph</p></header>
      ${fieldTable}
      <h4>Analysis and summary states reported by the graph</h4>
      ${analysisTable}
      <p class="basis-note">Observation and inference are shown only as reported in the graph projection. A reported analysis state is not an execution permit, and this report does not call GroupContext, ARTEX or a model.</p>
    </article>`
  }

  async function loadReportGraph() {
    const task = state.task
    if (!task || reportGraphBusy) return
    const taskId = task.id
    reportGraphBusy = true
    renderReportGraph()
    try {
      const payload = await api(`/v1/tasks/${encodeURIComponent(taskId)}/graph`)
      if (!state.task || state.task.id !== taskId) return
      state.graph = payload.data || null
      renderTaskDetail(state.task, state.graph)
      renderReportGraph()
    } catch (error) {
      reportError(error, 'Task graph report')
    } finally {
      reportGraphBusy = false
      renderReportGraph()
    }
  }

  function renderReport() {
    renderReportGraph()
    const task = state.task
    el.reportTask.textContent = task ? `${task.title} · ${task.id} · v${task.version}` : 'No task selected'
    if (!task) {
      el.reportBody.innerHTML = '<p class="subtle">Select a task to report its proposal basis.</p>'
      return
    }
    const entries = state.report.taskId === task.id ? state.report.entries : []
    if (!entries.length) {
      el.reportBody.innerHTML = '<p class="subtle">No proposal has been read for this task yet. Paste one or more proposal IDs above and select Load proposal basis; the console only reads IDs you supply or that this tab created.</p>'
      return
    }
    el.reportBody.innerHTML = entries.map(renderReportEntry).join('')
  }

  function resetReport() {
    state.reportSeq += 1
    state.report = { taskId: '', at: '', entries: [] }
    el.reportProposalIds.value = ''
    el.reportStatus.textContent = ''
    renderReport()
  }

  function updateReportEntry(taskId, proposalId, view) {
    if (state.report.taskId !== taskId) return
    const entry = state.report.entries.find((item) => item.proposalId === proposalId || (item.view && item.view.proposal_id === proposalId))
    if (!entry) return
    entry.proposalId = view.proposal_id || entry.proposalId
    entry.view = view
    entry.error = null
    renderReport()
  }

  function syncReportForTask(task) {
    if (!task) {
      resetReport()
      return
    }
    el.reportTask.textContent = `${task.title} · ${task.id} · v${task.version}`
    if (state.report.taskId === task.id) {
      renderReport()
      return
    }
    state.reportSeq += 1
    state.report = { taskId: task.id, at: '', entries: [] }
    const remembered = rememberedProposalIds(task.id)
    el.reportProposalIds.value = remembered.join('\n')
    el.reportStatus.textContent = remembered.length
      ? 'Remembered proposal IDs restored for this task. Load them to re-read basis from the API.'
      : ''
    renderReport()
  }

  async function readProposal(taskId, proposalId) {
    const payload = await api(`/v1/tasks/${encodeURIComponent(taskId)}/plan-proposals/${encodeURIComponent(proposalId)}`)
    const view = payload && payload.data
    if (!view || typeof view !== 'object') {
      throw new ApiError(0, 'STATE_STORE_UNAVAILABLE', 'The proposal response did not include a data object.')
    }
    state.proposalCache.set(proposalKey(taskId, proposalId), { view, at: new Date().toISOString() })
    rememberProposalId(taskId, view.proposal_id || proposalId)
    return { view, meta: payload.meta || {} }
  }

  async function loadReportEntries(ids, options = {}) {
    const task = state.task
    if (!task) {
      el.reportStatus.textContent = 'Select a task before reading proposals.'
      renderReport()
      return false
    }
    const taskId = task.id
    const sequence = ++state.reportSeq
    state.report = {
      taskId,
      at: new Date().toISOString(),
      entries: ids.map((proposalId) => ({ proposalId, view: null, error: null }))
    }
    renderReport()
    el.reportStatus.textContent = `Reading ${ids.length} proposal${ids.length === 1 ? '' : 's'}…`
    const results = await Promise.allSettled(ids.map((proposalId) => readProposal(taskId, proposalId)))
    if (sequence !== state.reportSeq || !state.task || state.task.id !== taskId) return false
    let failures = 0
    results.forEach((result, index) => {
      const entry = state.report.entries[index]
      if (!entry) return
      if (result.status === 'fulfilled') {
        entry.view = result.value.view
        return
      }
      entry.error = normalizeError(result.reason)
      failures += 1
      if (!options.quiet) reportError(`Load proposal ${ids[index]}`, result.reason)
    })
    if (!state.token) return false
    renderReport()
    el.reportStatus.textContent = failures
      ? `Read ${ids.length - failures}/${ids.length} proposals; ${failures} failed. Each failed card shows its exact status.`
      : `Read ${ids.length} proposal${ids.length === 1 ? '' : 's'} at ${new Date().toLocaleTimeString()}.`
    return failures === 0
  }

  async function refreshReportForTask(taskId, options = {}) {
    if (!state.token || !state.task || state.task.id !== taskId) return
    const ids = rememberedProposalIds(taskId)
    if (!ids.length) {
      renderReport()
      return
    }
    el.reportProposalIds.value = ids.join('\n')
    await loadReportEntries(ids, options)
  }

  async function submitReportLoad(event) {
    event.preventDefault()
    if (!state.token) return
    if (!state.task) {
      el.reportStatus.textContent = 'Select a task before reading proposals.'
      return
    }
    let ids
    try {
      ids = parseProposalIds(el.reportProposalIds.value)
    } catch (error) {
      const info = normalizeError(error)
      el.reportStatus.textContent = info.message
      reportError('Load proposal basis form', info)
      return
    }
    const button = el.reportForm.querySelector('button[type="submit"]')
    if (button) button.disabled = true
    try {
      await loadReportEntries(ids)
    } finally {
      if (button) button.disabled = false
    }
  }

  function renderProposalSteps(view) {
    const steps = view.proposal && Array.isArray(view.proposal.steps) ? view.proposal.steps : []
    if (!steps.length) {
      return '<p class="subtle">The proposal body returned no steps.</p>'
    }
    return `<div class="step-rows">${steps.map((step) => {
      const parameters = step.parameters || {}
      const details = []
      if (parameters.group_id) details.push(`group ${parameters.group_id}`)
      if (parameters.limit != null) details.push(`limit ${parameters.limit}`)
      if (parameters.keyword) details.push(`keyword ${parameters.keyword}`)
      if (parameters.note) details.push(`note ${parameters.note}`)
      if (parameters.text) details.push(`text ${parameters.text}`)
      if (parameters.verification_message) details.push(`verification ${parameters.verification_message}`)
      return `<div class="step-row">
        <strong>${escapeHtml(text(step.step_no))}. ${escapeHtml(text(step.action))}</strong>
        <span>${escapeHtml(text(step.account_id))} → ${escapeHtml(text(step.target_id))}</span>
        <span>${escapeHtml(details.join(' · ') || 'no parameters returned')}</span>
      </div>`
    }).join('')}</div>`
  }

  function matchingGraphFacts(detail) {
    const steps = detail.view.proposal && Array.isArray(detail.view.proposal.steps) ? detail.view.proposal.steps : []
    const wanted = steps
      .filter((step) => step.account_id != null && step.target_id != null)
      .map((step) => ({ accountId: String(step.account_id), targetId: String(step.target_id), stepNo: step.step_no }))
    if (!wanted.length || !state.graph) return []
    return (state.graph.facts || []).filter((fact) => wanted.some((entry) => (
      String(fact.account_id) === entry.accountId && String(fact.target_id) === entry.targetId
    )))
  }

  function renderTrace(detail) {
    const view = detail.view
    const proposalState = String(view.state || '')
    const key = proposalKey(detail.taskId, view.proposal_id || detail.proposalId)
    const approval = state.proposalApprovals.get(key)
    const receipt = state.proposalReceipts.get(key)
    const items = []

    items.push(`<li class="trace-item">
      <span class="trace-title">Proposal ledger</span>
      <span class="badge" data-tone="${proposalTone(proposalState)}">${escapeHtml(text(proposalState))}</span>
      <p>Read from <code>GET /v1/tasks/{task_id}/plan-proposals/{proposal_id}</code>: digest ${escapeHtml(text(view.digest))}, expected task version ${escapeHtml(text(view.expected_task_version))}, scope revision ${escapeHtml(text(view.scope_revision))}, expires ${escapeHtml(text(view.expires_at))}.</p>
      <p class="trace-note">Durable proposal state only. It is not an execution result and carries no dispatch claim.</p>
    </li>`)

    if (proposalState === 'pending') {
      items.push(`<li class="trace-item"><span class="trace-title">Approval binding</span><span class="badge" data-tone="neutral">not recorded</span><p>No approval binding is recorded: the durable proposal state is still pending.</p></li>`)
    } else if (proposalState === 'approved' || proposalState === 'applied') {
      const receiptLine = approval
        ? ` This session's approve receipt: reference ${escapeHtml(text(approval.approvalRef))}, accepted ${escapeHtml(text(approval.acceptedAt))}${approval.requestId ? `, request ${escapeHtml(approval.requestId)}` : ''}. That is a session receipt, not a re-read of the ledger.`
        : ''
      items.push(`<li class="trace-item"><span class="trace-title">Approval binding</span><span class="badge" data-tone="info">bound in ledger</span><p>The ledger reports state ${escapeHtml(proposalState)}, which means an approval binding exists. The approval reference, approver and approval timestamp are not fields of the proposal read projection, so they are not shown unless this tab received the approve receipt.${receiptLine}</p></li>`)
    } else {
      items.push(`<li class="trace-item"><span class="trace-title">Approval binding</span><span class="badge" data-tone="danger">not applicable</span><p>Not recorded: the proposal state is ${escapeHtml(proposalState)}, which is terminal for this chain.</p></li>`)
    }

    if (proposalState === 'applied') {
      const receiptLine = receipt
        ? ` This session's apply receipt: id ${escapeHtml(text(receipt.id))}, state ${escapeHtml(text(receipt.state))}, accepted ${escapeHtml(text(receipt.acceptedAt))}${receipt.requestId ? `, request ${escapeHtml(receipt.requestId)}` : ''} (HTTP 202; a session receipt, not a re-read).`
        : ''
      items.push(`<li class="trace-item"><span class="trace-title">Application intent record</span><span class="badge" data-tone="positive">intent recorded</span><p>The durable state is applied: the ledger recorded intent only. No proposal work item is enqueued, and the worker does not consume this application. Existing task scheduling operates independently. Application ID and timestamps are not exposed by the proposal read endpoint.${receiptLine}</p></li>`)
    } else if (proposalState === 'approved') {
      items.push(`<li class="trace-item"><span class="trace-title">Application record</span><span class="badge" data-tone="neutral">not recorded</span><p>No application is recorded: the proposal is approved but not applied.</p></li>`)
    } else {
      items.push(`<li class="trace-item"><span class="trace-title">Application record</span><span class="badge" data-tone="neutral">not recorded</span><p>No application is recorded for state ${escapeHtml(proposalState)}. Apply requires an approved, unexpired proposal and a running task.</p></li>`)
    }

    const facts = matchingGraphFacts(detail)
    if (!state.graph) {
      items.push(`<li class="trace-item"><span class="trace-title">Independent task facts</span><span class="badge" data-tone="neutral">not loaded</span><p>Proposal-driven dispatch is not implemented. The task graph projection is not loaded. Task work items, executions and results have no recorded causal link to this proposal.</p></li>`)
    } else if (facts.length) {
      items.push(`<li class="trace-item"><span class="trace-title">Independent task facts</span><span class="badge" data-tone="info">${facts.length} scope reference${facts.length === 1 ? '' : 's'}</span>
        <div class="trace-facts">${facts.map((fact) => `<div class="trace-fact"><strong>${escapeHtml(text(fact.kind))}</strong><span>${escapeHtml(text(fact.data && fact.data.state))}</span><span>evidence ${escapeHtml(text(fact.evidence_ref))}</span><span>observed ${escapeHtml(text(fact.observed_at))}</span></div>`).join('')}</div>
        <p class="trace-note">Scope matches only, read from the BBA graph at revision ${escapeHtml(text(state.graph.revision))}. Matching account/target does not link an execution or result to this proposal. Existing task scheduling is independent; proposal-driven dispatch is not implemented.</p></li>`)
    } else {
      items.push(`<li class="trace-item"><span class="trace-title">Independent task facts</span><span class="badge" data-tone="neutral">no scope references</span><p>No graph fact matches the account/target at revision ${escapeHtml(text(state.graph.revision))}. Apply records intent only. Existing task scheduling runs independently; proposal-driven dispatch and result correlation are not implemented.</p></li>`)
    }

    return items.join('')
  }

  function renderProposalDetail() {
    const detail = state.proposalDetail
    if (!detail) {
      el.proposalDetailId.textContent = 'No proposal loaded'
      el.proposalDetailBody.innerHTML = '<p class="subtle">Load a proposal from the report above, or use a remembered proposal ID after creating one, to inspect it here.</p>'
      el.proposalDetailFieldset.hidden = true
      return
    }
    el.proposalDetailId.textContent = detail.proposalId
    if (!detail.view) {
      const info = detail.error || new ApiError(0, 'CLIENT_ERROR', 'The proposal was not read.')
      el.proposalDetailBody.innerHTML = `<article class="report-card report-error">
        <p class="error-message">${escapeHtml(info.message || info.code)}</p>
        <p class="hint">${escapeHtml(inlineErrorText(info))}</p>
        <div class="button-row"><button class="text-button" type="button" data-reload-proposal="${escapeHtml(detail.proposalId)}">Retry the read</button></div>
      </article>`
      el.proposalDetailFieldset.hidden = true
      return
    }

    const view = detail.view
    const proposal = view.proposal || {}
    const basis = Array.isArray(view.basis) ? view.basis : []
    const basisAt = basis.length > 0 && basis[0].created_at ? basis[0].created_at : null
    const questions = Array.isArray(proposal.questions) ? proposal.questions : []
    const expiresAt = view.expires_at ? new Date(view.expires_at) : null
    const expiredByClock = expiresAt && !Number.isNaN(expiresAt.getTime()) && expiresAt.getTime() <= Date.now()
    const expiryWarning = expiredByClock
      ? `<p class="session-warning" data-testid="proposal-expiry-warning">The server-recorded expires_at (${escapeHtml(formatDateTime(view.expires_at))}) has passed. Approval and apply are rejected server-side; this console infers no success and does not retry automatically.</p>`
      : ''

    el.proposalDetailBody.innerHTML = `
      <div class="trace-state-row">
        <span class="badge" data-tone="${proposalTone(view.state)}">${escapeHtml(text(view.state))}</span>
        <span class="count">read ${escapeHtml(detail.loadedAt ? new Date(detail.loadedAt).toLocaleTimeString() : '—')}</span>
      </div>
      ${expiryWarning}
      <dl class="detail-grid compact">
        ${field('Proposal ID', view.proposal_id)}
        ${field('Task ID', view.task_id)}
        ${field('State', view.state)}
        ${field('Proposal kind', proposal.proposal_kind)}
        ${field('Schema', proposal.schema_id)}
        ${field('Digest', view.digest)}
        ${field('Expected task version', view.expected_task_version)}
        ${field('Scope revision', view.scope_revision)}
        ${field('Expires at', view.expires_at)}
        ${field('Basis recorded at', basisAt)}
        ${field('Basis rows', basis.length)}
      </dl>
      <h3>Rationale returned by the proposal</h3>
      <p class="proposal-rationale">${escapeHtml(text(proposal.rationale_summary))}</p>
      ${questions.length ? `<h3>Questions</h3><ul class="question-list">${questions.map((question) => `<li>${escapeHtml(question)}</li>`).join('')}</ul>` : ''}
      <h3>Steps</h3>
      ${renderProposalSteps(view)}
      <section class="basis-section" aria-labelledby="proposal-basis-title">
        <div class="section-heading">
          <h3 id="proposal-basis-title">Recorded basis (generation-time reference)</h3>
          <span class="count">${basis.length} row${basis.length === 1 ? '' : 's'}</span>
        </div>
        <p class="subtle">These rows record which live Summary version and graph anchor generation consulted, plus the rule that selected them. They are reference data: not a decision, no approve or apply authority, and a statistical Summary is not verified group semantic understanding. The console does not re-verify a Summary version because no operator Summary read route is used here.</p>
        ${basis.length
          ? basis.map((row, index) => renderBasisRow(row, index)).join('')
          : '<p class="basis-note">The proposal response returned an empty basis array. No generation-time Summary or anchor reference is recorded for this proposal; the gap is shown rather than filled in.</p>'}
      </section>
      <section class="trace-section" aria-labelledby="proposal-trace-title">
        <div class="section-heading"><h3 id="proposal-trace-title">Proposal → approval → intent record</h3></div>
        <ol class="trace-list">${renderTrace(detail)}</ol>
        <p class="subtle">Apply records intent only; it does not enqueue proposal steps for the worker. Existing task scheduling runs independently. Proposal-driven dispatch and execution/result correlation are not implemented. The proposal read endpoint does not expose approval_id, approved_at, applied_at or application fields. HTTP 202 confirms intent acceptance only.</p>
      </section>
      <div class="button-row">
        <button class="text-button" type="button" data-reload-proposal="${escapeHtml(view.proposal_id || detail.proposalId)}">Re-read proposal from GET</button>
        <button class="text-button" type="button" data-show-report="${escapeHtml(view.proposal_id || detail.proposalId)}">Show its row in the report</button>
      </div>`

    el.proposalDetailFieldset.hidden = false
    el.proposalDetailFieldset.disabled = state.busy || !state.devMode
    const key = proposalKey(detail.taskId, view.proposal_id || detail.proposalId)
    const approval = state.proposalApprovals.get(key)
    const taskVersion = state.task ? state.task.version : view.expected_task_version
    el.proposalDetailApproveRef.value = approval ? approval.approvalRef : ''
    el.proposalDetailApplyRef.value = approval ? approval.approvalRef : ''
    el.proposalDetailApproveVersion.value = String(taskVersion || 1)
    el.proposalDetailApplyVersion.value = String(taskVersion || 1)
    el.proposalDetailApplyDigest.value = view.digest || ''
    refreshProposalActionAvailability()
  }

  function refreshProposalActionAvailability() {
    const detail = state.proposalDetail
    const loaded = Boolean(detail && detail.view)
    const proposalState = loaded ? String(detail.view.state || '') : ''
    const locked = state.busy || !state.devMode || !loaded
    const approve = el.proposalDetailApproveForm.querySelector('button[type="submit"]')
    const apply = el.proposalDetailApplyForm.querySelector('button[type="submit"]')
    if (approve) {
      const allowed = proposalState === 'pending'
      approve.disabled = locked || !allowed
      approve.title = !locked && !allowed ? `Approve is available only while the durable proposal state is pending (now ${proposalState || 'unknown'}).` : ''
    }
    if (apply) {
      const allowed = proposalState === 'approved'
      apply.disabled = locked || !allowed
      apply.title = !locked && !allowed ? `Apply is available only while the durable proposal state is approved (now ${proposalState || 'unknown'}).` : ''
    }
  }

  function resetProposalDetail() {
    state.proposalDetailSeq += 1
    state.proposalDetail = null
    el.proposalDetailApproveRef.value = ''
    el.proposalDetailApplyRef.value = ''
    el.proposalDetailApplyDigest.value = ''
    el.proposalDetailApproveKey.textContent = ''
    el.proposalDetailApplyKey.textContent = ''
    el.proposalDetailActionNote.textContent = ''
    renderProposalDetail()
  }

  async function openProposal(proposalId, options = {}) {
    const task = state.task
    if (!task) {
      el.proposalDetailId.textContent = 'No task selected'
      el.proposalDetailBody.innerHTML = '<p class="subtle">Select a task before opening a proposal.</p>'
      el.proposalDetailFieldset.hidden = true
      return
    }
    const taskId = task.id
    rememberProposalId(taskId, proposalId)
    const sequence = ++state.proposalDetailSeq
    el.proposalDetailId.textContent = proposalId
    el.proposalDetailBody.innerHTML = '<p class="subtle">Reading proposal…</p>'
    el.proposalDetailFieldset.hidden = true
    let view
    let loadedAt = ''
    const cached = state.proposalCache.get(proposalKey(taskId, proposalId))
    if (cached) {
      view = cached.view
      loadedAt = cached.at
    } else {
      try {
        const result = await readProposal(taskId, proposalId)
        view = result.view
        loadedAt = (state.proposalCache.get(proposalKey(taskId, proposalId)) || {}).at || ''
      } catch (error) {
        if (sequence !== state.proposalDetailSeq) return
        state.proposalDetail = { taskId, proposalId, view: null, error: normalizeError(error), loadedAt: '' }
        renderProposalDetail()
        reportError(`Load proposal ${proposalId}`, error)
        return
      }
    }
    if (sequence !== state.proposalDetailSeq || !state.task || state.task.id !== taskId) return
    state.proposalDetail = { taskId, proposalId, view, error: null, loadedAt }
    renderProposalDetail()
    if (options.focus) el.proposalDetailBody.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }

  async function reloadProposalFromApi(proposalId) {
    const task = state.task
    if (!task) return
    const sequence = ++state.proposalDetailSeq
    try {
      const { view } = await readProposal(task.id, proposalId)
      if (sequence !== state.proposalDetailSeq) return
      const loadedAt = (state.proposalCache.get(proposalKey(task.id, proposalId)) || {}).at || ''
      state.proposalDetail = { taskId: task.id, proposalId, view, error: null, loadedAt }
      updateReportEntry(task.id, proposalId, view)
      renderProposalDetail()
      el.proposalDetailActionNote.textContent = 'Proposal re-read from GET. This does not replay any mutation and does not prove an external outcome.'
    } catch (error) {
      if (sequence !== state.proposalDetailSeq) return
      state.proposalDetail = { taskId: task.id, proposalId, view: null, error: normalizeError(error) }
      renderProposalDetail()
      reportError(`Reload proposal ${proposalId}`, error)
    }
  }

  async function reloadProposalAfterWrite(taskId, proposalId, options = {}) {
    try {
      const { view } = await readProposal(taskId, proposalId)
      if (!state.task || state.task.id !== taskId) return
      const loadedAt = (state.proposalCache.get(proposalKey(taskId, proposalId)) || {}).at || ''
      state.proposalDetail = { taskId, proposalId: view.proposal_id || proposalId, view, error: null, loadedAt }
      updateReportEntry(taskId, proposalId, view)
      renderProposalDetail()
    } catch (error) {
      if (!options.quiet) reportError(`Reload proposal ${proposalId}`, error)
    }
  }

  async function submitDetailApprove(event) {
    event.preventDefault()
    if (!state.devMode || state.busy) return
    const detail = state.proposalDetail
    const task = state.task
    if (!detail || !detail.view || !task) {
      reportError('Approve proposal', new ApiError(0, 'FORM_INVALID', 'Load a proposal for the selected task before approving.'))
      return
    }
    const proposalId = detail.view.proposal_id || detail.proposalId
    let body
    try {
      body = {
        expected_version: readInteger(el.proposalDetailApproveVersion, 'Expected task version', 1, Number.MAX_SAFE_INTEGER),
        approval_ref: readText(el.proposalDetailApproveRef, 'Approval reference', 128)
      }
    } catch (error) {
      const info = normalizeError(error)
      el.proposalDetailActionNote.textContent = info.message
      reportError('Approve proposal form', info)
      return
    }
    const path = `/v1/tasks/${encodeURIComponent(task.id)}/plan-proposals/${encodeURIComponent(proposalId)}/approve`
    const key = getAttemptKey(attempts.proposalDetailApprove, JSON.stringify({ path, body }))
    noteAttemptKey(el.proposalDetailApproveKey, key, body.expected_version)
    el.proposalDetailActionNote.textContent = ''
    setBusy(true, `Approving proposal ${proposalId}…`)
    try {
      const payload = await api(path, { method: 'POST', body, idempotencyKey: key })
      const receipt = payload && payload.data
      if (!receipt) throw new ApiError(0, 'STATE_STORE_UNAVAILABLE', 'The approval response did not include a receipt.')
      clearAttempt(attempts.proposalDetailApprove)
      state.proposalApprovals.set(proposalKey(task.id, proposalId), {
        approvalRef: receipt.approval_ref || body.approval_ref,
        acceptedAt: receipt.accepted_at || '',
        requestId: payload.meta && payload.meta.request_id ? payload.meta.request_id : ''
      })
      rememberProposalId(task.id, proposalId)
      showProposalApproval(receipt, `approve ${proposalId}`, payload.meta && payload.meta.request_id)
      el.proposalDetailActionNote.textContent = 'Approval accepted (HTTP 202). It binds the approval reference; it is not a QQ outcome and does not apply the proposal.'
      await reloadProposalAfterWrite(task.id, proposalId, { quiet: true })
      await refreshReportForTask(task.id, { quiet: true })
      setStatus(`Proposal ${proposalId} approval accepted`)
    } catch (error) {
      const info = annotateError(error, { idempotencyKey: key, expectedVersion: body.expected_version })
      el.proposalDetailActionNote.textContent = inlineErrorText(info)
      reportError('Approve proposal', info)
      if (info.status === 409) {
        const typedRef = el.proposalDetailApproveRef.value
        const typedApplyRef = el.proposalDetailApplyRef.value
        await reloadProposalAfterWrite(task.id, proposalId, { quiet: true })
        el.proposalDetailApproveRef.value = typedRef
        el.proposalDetailApplyRef.value = typedApplyRef
      }
    } finally {
      setBusy(false)
    }
  }

  async function submitDetailApply(event) {
    event.preventDefault()
    if (!state.devMode || state.busy) return
    const detail = state.proposalDetail
    const task = state.task
    if (!detail || !detail.view || !task) {
      reportError('Apply proposal', new ApiError(0, 'FORM_INVALID', 'Load a proposal for the selected task before applying.'))
      return
    }
    const proposalId = detail.view.proposal_id || detail.proposalId
    let body
    try {
      body = {
        expected_version: readInteger(el.proposalDetailApplyVersion, 'Expected task version', 1, Number.MAX_SAFE_INTEGER),
        approval_ref: readText(el.proposalDetailApplyRef, 'Approval reference', 128),
        proposal_digest: readText(el.proposalDetailApplyDigest, 'Proposal digest', 64)
      }
      if (!/^[a-f0-9]{64}$/.test(body.proposal_digest)) {
        throw new ApiError(0, 'FORM_INVALID', 'Proposal digest must be exactly 64 lowercase hexadecimal characters.')
      }
    } catch (error) {
      const info = normalizeError(error)
      el.proposalDetailActionNote.textContent = info.message
      reportError('Apply proposal form', info)
      return
    }
    const path = `/v1/tasks/${encodeURIComponent(task.id)}/plan-proposals/${encodeURIComponent(proposalId)}/apply`
    const key = getAttemptKey(attempts.proposalDetailApply, JSON.stringify({ path, body }))
    noteAttemptKey(el.proposalDetailApplyKey, key, body.expected_version)
    el.proposalDetailActionNote.textContent = ''
    setBusy(true, `Applying proposal ${proposalId}…`)
    try {
      const payload = await api(path, { method: 'POST', body, idempotencyKey: key })
      const receipt = payload && payload.data
      if (!receipt) throw new ApiError(0, 'STATE_STORE_UNAVAILABLE', 'The apply response did not include a receipt.')
      clearAttempt(attempts.proposalDetailApply)
      rememberProposalId(task.id, proposalId)
      state.proposalReceipts.set(proposalKey(task.id, proposalId), {
        id: receipt.id,
        state: receipt.state,
        action: receipt.action,
        resourceId: receipt.resource_id,
        acceptedAt: receipt.accepted_at,
        expectedVersion: receipt.expected_version,
        requestId: payload.meta && payload.meta.request_id ? payload.meta.request_id : ''
      })
      showReceipt(receipt, `apply ${proposalId}`, payload.meta && payload.meta.request_id, 'Proposal apply receipt')
      el.proposalDetailActionNote.textContent = 'Apply accepted (HTTP 202): intent recorded only. No proposal work is enqueued or consumed by the worker. Existing task scheduling runs independently; proposal-driven dispatch is not implemented.'
      await loadTask(task.id, { quiet: true })
      await reloadProposalAfterWrite(task.id, proposalId, { quiet: true })
      await refreshReportForTask(task.id, { quiet: true })
      setStatus(`Proposal ${proposalId} apply receipt accepted`)
    } catch (error) {
      const info = annotateError(error, { idempotencyKey: key, expectedVersion: body.expected_version })
      el.proposalDetailActionNote.textContent = inlineErrorText(info)
      reportError('Apply proposal', info)
      if (info.status === 409) {
        const typedRef = el.proposalDetailApplyRef.value
        await loadTask(task.id, { quiet: true })
        await reloadProposalAfterWrite(task.id, proposalId, { quiet: true })
        el.proposalDetailApplyRef.value = typedRef
        el.proposalDetailApplyDigest.value = body.proposal_digest
      }
    } finally {
      setBusy(false)
    }
  }

  function readInteger(element, label, min, max) {
    const raw = element.value.trim()
    const value = Number(raw)
    if (!Number.isInteger(value) || value < min || value > max) {
      throw new ApiError(0, 'FORM_INVALID', `${label} must be an integer from ${min} to ${max}.`)
    }
    return value
  }

  function readText(element, label, maximum) {
    const value = element.value.trim()
    if (!value) throw new ApiError(0, 'FORM_INVALID', `${label} is required.`)
    if (maximum && value.length > maximum) {
      throw new ApiError(0, 'FORM_INVALID', `${label} must be ${maximum} characters or fewer.`)
    }
    return value
  }

  function parseCandidates(value) {
    const lines = value.split(/\r?\n/).map((line) => line.trim()).filter(Boolean)
    if (!lines.length) throw new ApiError(0, 'FORM_INVALID', 'At least one candidate group line is required.')
    if (lines.length > 5000) throw new ApiError(0, 'FORM_INVALID', 'At most 5000 candidate groups are allowed.')
    const seen = new Set()
    return lines.map((line, index) => {
      const parts = line.split(/[\s,]+/).filter(Boolean)
      if (parts.length !== 2) {
        throw new ApiError(0, 'FORM_INVALID', `Candidate line ${index + 1} must be group_id,source_ref.`)
      }
      const groupId = parts[0]
      const sourceRef = parts[1]
      if (!/^[1-9][0-9]*$/.test(groupId)) {
        throw new ApiError(0, 'FORM_INVALID', `Candidate line ${index + 1} has an invalid decimal group_id.`)
      }
      if (!sourceRef || sourceRef.length > 128) {
        throw new ApiError(0, 'FORM_INVALID', `Candidate line ${index + 1} needs a source_ref of 1–128 characters.`)
      }
      if (seen.has(groupId)) {
        throw new ApiError(0, 'FORM_INVALID', `Candidate group ${groupId} is duplicated.`)
      }
      seen.add(groupId)
      return { group_id: groupId, source_ref: sourceRef }
    })
  }

  function buildCreateBody() {
    const title = readText(el.createTitle, 'Title', 200)
    const mode = el.createMode.value
    if (mode !== 'dry_run' && mode !== 'read_only') {
      throw new ApiError(0, 'FORM_INVALID', 'Execution mode must be dry_run or read_only in the development console.')
    }
    const goalCount = readInteger(el.createGoal, 'Goal count', 1, 10000)
    if (!el.createDeadline.value) throw new ApiError(0, 'FORM_INVALID', 'Deadline is required.')
    const deadline = new Date(el.createDeadline.value)
    if (Number.isNaN(deadline.getTime())) throw new ApiError(0, 'FORM_INVALID', 'Deadline is not a valid date.')
    if (deadline.getTime() <= Date.now()) throw new ApiError(0, 'FORM_INVALID', 'Deadline must be in the future.')
    const groups = parseCandidates(el.createCandidates.value)
    if (goalCount > groups.length) {
      throw new ApiError(0, 'FORM_INVALID', 'Goal count cannot exceed the number of candidate groups.')
    }

    const allowedAccountIds = Array.from(state.createAccountIds)
    if (allowedAccountIds.length < 1 || allowedAccountIds.length > 20) {
      throw new ApiError(0, 'FORM_INVALID', 'Choose between 1 and 20 allowed accounts.')
    }

    const joinReason = readText(el.createJoinReason, 'Join reason', 500)
    const verificationMessage = readText(el.createVerification, 'Verification message', 500)
    const maxCandidates = readInteger(el.createMaxCandidates, 'Max candidates', 1, 5000)
    const maxAttempts = readInteger(el.createMaxAttempts, 'Max attempts per action', 1, 10)
    const maxTotal = readInteger(el.createMaxTotal, 'Max total external attempts', 1, 10000)
    const maxModel = readInteger(el.createMaxModel, 'Max model attempts', 0, 10)
    if (maxCandidates < groups.length) {
      throw new ApiError(0, 'FORM_INVALID', 'Max candidates cannot be smaller than the candidate list.')
    }
    if (maxTotal < goalCount || maxTotal < maxAttempts) {
      throw new ApiError(0, 'FORM_INVALID', 'Max total external attempts must be at least the goal count and max attempts per action.')
    }

    return {
      type: 'group.acquire',
      title,
      execution_mode: mode,
      goal: {
        metric: 'new_unique_groups_for_account_pool',
        count: goalCount,
        strict_target_cap: true,
        require_final_membership_check: true
      },
      allowed_account_ids: allowedAccountIds,
      candidate_source: { kind: 'fixed', groups },
      deadline_at: deadline.toISOString(),
      limits: {
        max_candidates: maxCandidates,
        max_attempts_per_action: maxAttempts,
        max_total_external_attempts: maxTotal,
        max_model_attempts: maxModel
      },
      join_reason: joinReason,
      verification_message: verificationMessage,
      approval_ref: null,
      submit: el.createSubmitCheckbox.checked
    }
  }

  function showOutcome(outcome) {
    el.outcomePanel.hidden = false
    el.outcomeTitle.textContent = outcome.title
    el.outcomeBadge.textContent = text(outcome.state)
    el.outcomeBadge.dataset.tone = outcomeTone(outcome.state)
    el.outcomeContext.textContent = outcome.context
    el.outcomeFields.innerHTML = outcome.fields.map((entry) => field(entry[0], entry[1])).join('')
    el.outcomeNote.textContent = outcome.note
  }

  function outcomeTone(value) {
    const normalized = String(value || '').toLowerCase()
    if (['confirmed', 'succeeded', 'success'].includes(normalized)) return 'positive'
    if (['unknown', 'failed', 'error', 'blocked', 'cancelled'].includes(normalized)) return 'danger'
    if (['pending', 'waiting_external', 'pausing', 'verifying', 'reconciling', 'partial'].includes(normalized)) return 'warn'
    if (['accepted', 'approved', 'applied', 'draft', 'validating', 'ready', 'running', 'paused'].includes(normalized)) return 'info'
    return 'neutral'
  }

  function showReceipt(receipt, context, requestId, title) {
    const data = receipt || {}
    showOutcome({
      title: title || 'Mutation receipt',
      state: data.state || '—',
      context: `${context} · HTTP 202`,
      fields: [
        ['Receipt state', data.state],
        ['Action', data.action],
        ['Receipt ID', data.id],
        ['Resource ID', data.resource_id],
        ['Expected version', data.expected_version],
        ['Accepted at', data.accepted_at],
        ['Request ID', requestId]
      ],
      note: 'HTTP 202 means the request was durably accepted. It is not evidence that an external action completed, failed or was withdrawn.'
    })
  }

  function showTaskCreated(task, requestId) {
    showOutcome({
      title: 'Task created',
      state: task.state,
      context: 'POST /v1/tasks · HTTP 201',
      fields: [
        ['Task ID', task.id],
        ['State', task.state],
        ['Version', task.version],
        ['Execution mode', task.execution_mode],
        ['Goal', task.goal_count],
        ['Confirmed unique', task.confirmed_unique_count],
        ['Pending external', task.pending_external_count],
        ['Unknown execution', task.unknown_execution_count],
        ['Request ID', requestId]
      ],
      note: 'Creation is persisted in the local Task Engine. A task that remains DRAFT or VALIDATING needs an explicit start control; refresh only re-reads state and never resends this create.'
    })
  }

  function showProposalApproval(receipt, context, requestId) {
    const data = receipt || {}
    showOutcome({
      title: 'Proposal approval',
      state: data.state || '—',
      context: `${context} · HTTP 202`,
      fields: [
        ['Approval state', data.state],
        ['Proposal ID', data.proposal_id],
        ['Approval reference', data.approval_ref],
        ['Accepted at', data.accepted_at],
        ['Request ID', requestId]
      ],
      note: 'Approval binds a trusted approval reference to the proposal. It is not a QQ outcome and does not apply the proposal automatically.'
    })
  }

  async function submitCreate(event) {
    event.preventDefault()
    if (!state.devMode || state.busy) return
    let body
    try {
      body = buildCreateBody()
    } catch (error) {
      reportError('Create task form', error)
      return
    }
    const path = '/v1/tasks'
    const signature = JSON.stringify({ path, body })
    const key = getAttemptKey(attempts.create, signature)
    noteAttemptKey(el.createKey, key)
    setBusy(true, 'Creating task…')
    try {
      const payload = await api(path, { method: 'POST', body, idempotencyKey: key })
      const task = payload && payload.data
      if (!task || !task.id) throw new ApiError(0, 'STATE_STORE_UNAVAILABLE', 'Create response did not include a task.')
      clearAttempt(attempts.create)
      showTaskCreated(task, payload.meta && payload.meta.request_id)
      state.selectedTaskId = task.id
      clearTaskScopedState()
      await refresh()
      if (state.token) setStatus(`Task ${task.id} created · ${task.state} v${task.version}`)
    } catch (error) {
      const info = annotateError(error, { idempotencyKey: key })
      if (info.status === 409) await refresh()
      reportError('Create task', info)
    } finally {
      setBusy(false)
    }
  }

  async function submitControl(action) {
    if (!state.devMode || state.busy) return
    const task = state.task
    if (!task) {
      reportError('Task control', new ApiError(0, 'FORM_INVALID', 'Select a task before issuing a control.'))
      return
    }
    let body
    const path = `/v1/tasks/${encodeURIComponent(task.id)}/controls`
    try {
      const expectedVersion = readInteger(el.controlsVersion, 'Expected version', 1, Number.MAX_SAFE_INTEGER)
      const reason = readText(el.controlsReason, 'Reason', 500)
      body = { action, expected_version: expectedVersion, reason }
    } catch (error) {
      reportError(`${action} control form`, error)
      return
    }
    if (action === 'cancel' && !window.confirm(`Cancel task ${task.id}? This is an authenticated development mutation.`)) {
      return
    }
    const key = getAttemptKey(attempts.control, JSON.stringify({ path, body }))
    noteAttemptKey(el.controlsKey, key, body.expected_version)
    setBusy(true, `Requesting ${action} for ${task.id}…`)
    try {
      const payload = await api(path, { method: 'POST', body, idempotencyKey: key })
      if (!payload || !payload.data) throw new ApiError(0, 'STATE_STORE_UNAVAILABLE', 'Control response did not include a receipt.')
      clearAttempt(attempts.control)
      showReceipt(payload.data, `${action} ${task.id}`, payload.meta && payload.meta.request_id)
      setStatus(`${action} accepted for ${task.id}; refreshing state…`)
      await refresh()
      if (state.token) setStatus(`${action} receipt accepted for ${task.id}`)
    } catch (error) {
      const info = annotateError(error, { idempotencyKey: key, expectedVersion: body.expected_version })
      reportError(`${action} control`, info)
      if (info.status === 409) await loadTask(task.id, { quiet: true })
    } finally {
      setBusy(false)
    }
  }

  async function submitReconcile() {
    if (!state.devMode || state.busy) return
    const task = state.task
    if (!task) {
      reportError('Reconcile', new ApiError(0, 'FORM_INVALID', 'Select a task before requesting reconcile.'))
      return
    }
    let body
    const path = `/v1/tasks/${encodeURIComponent(task.id)}/reconcile`
    try {
      const expectedVersion = readInteger(el.controlsVersion, 'Expected version', 1, Number.MAX_SAFE_INTEGER)
      const reason = readText(el.controlsReason, 'Reason', 500)
      body = { expected_version: expectedVersion, reason }
    } catch (error) {
      reportError('Reconcile form', error)
      return
    }
    const key = getAttemptKey(attempts.reconcile, JSON.stringify({ path, body }))
    noteAttemptKey(el.controlsKey, key, body.expected_version)
    setBusy(true, `Requesting reconcile for ${task.id}…`)
    try {
      const payload = await api(path, { method: 'POST', body, idempotencyKey: key })
      if (!payload || !payload.data) throw new ApiError(0, 'STATE_STORE_UNAVAILABLE', 'Reconcile response did not include a receipt.')
      clearAttempt(attempts.reconcile)
      showReceipt(payload.data, `reconcile ${task.id}`, payload.meta && payload.meta.request_id, 'Reconcile receipt')
      setStatus(`Reconcile accepted for ${task.id}; refreshing state…`)
      await refresh()
      if (state.token) setStatus(`Reconcile request accepted for ${task.id}`)
    } catch (error) {
      const info = annotateError(error, { idempotencyKey: key, expectedVersion: body.expected_version })
      reportError('Reconcile', info)
      if (info.status === 409) await loadTask(task.id, { quiet: true })
    } finally {
      setBusy(false)
    }
  }

  async function submitProposalCreate(event) {
    event.preventDefault()
    if (!state.devMode || state.busy) return
    const task = state.task
    if (!task) {
      reportError('Create proposal', new ApiError(0, 'FORM_INVALID', 'Select a task before creating a proposal.'))
      return
    }
    let body
    try {
      body = {
        task_id: task.id,
        expected_version: readInteger(el.proposalCreateVersion, 'Expected task version', 1, Number.MAX_SAFE_INTEGER),
        scope_revision: readInteger(el.proposalCreateScope, 'Scope revision', 1, Number.MAX_SAFE_INTEGER),
        proposal_kind: el.proposalCreateKind.value,
        reason: readText(el.proposalCreateReason, 'Proposal reason', 500)
      }
    } catch (error) {
      reportError('Create proposal form', error)
      return
    }
    const path = `/v1/tasks/${encodeURIComponent(task.id)}/plan-proposals`
    const key = getAttemptKey(attempts.proposalCreate, JSON.stringify({ path, body }))
    noteAttemptKey(el.proposalCreateKey, key, body.expected_version)
    setBusy(true, `Creating proposal for ${task.id}…`)
    try {
      const payload = await api(path, { method: 'POST', body, idempotencyKey: key })
      const receipt = payload && payload.data
      if (!receipt) throw new ApiError(0, 'STATE_STORE_UNAVAILABLE', 'Proposal response did not include a receipt.')
      clearAttempt(attempts.proposalCreate)
      showReceipt(receipt, `create proposal ${task.id}`, payload.meta && payload.meta.request_id, 'Proposal create receipt')
      if (receipt && receipt.resource_id) {
        el.proposalApproveId.value = receipt.resource_id
        el.proposalApplyId.value = receipt.resource_id
        rememberProposalId(task.id, receipt.resource_id)
        el.reportProposalIds.value = rememberedProposalIds(task.id).join('\n')
      }
      await refresh()
      if (state.token) {
        setStatus(`Proposal receipt accepted for ${task.id}`)
        if (receipt && receipt.resource_id) await refreshReportForTask(task.id, { quiet: true })
      }
    } catch (error) {
      const info = annotateError(error, { idempotencyKey: key, expectedVersion: body.expected_version })
      reportError('Create proposal', info)
    } finally {
      setBusy(false)
    }
  }

  async function submitProposalApprove(event) {
    event.preventDefault()
    if (!state.devMode || state.busy) return
    const task = state.task
    if (!task) {
      reportError('Approve proposal', new ApiError(0, 'FORM_INVALID', 'Select a task before approving a proposal.'))
      return
    }
    let proposalId
    let body
    try {
      proposalId = readText(el.proposalApproveId, 'Proposal ID', 128)
      body = {
        expected_version: readInteger(el.proposalApproveVersion, 'Expected task version', 1, Number.MAX_SAFE_INTEGER),
        approval_ref: readText(el.proposalApproveRef, 'Approval reference', 128)
      }
    } catch (error) {
      reportError('Approve proposal form', error)
      return
    }
    const path = `/v1/tasks/${encodeURIComponent(task.id)}/plan-proposals/${encodeURIComponent(proposalId)}/approve`
    const key = getAttemptKey(attempts.proposalApprove, JSON.stringify({ path, body }))
    noteAttemptKey(el.proposalApproveKey, key, body.expected_version)
    setBusy(true, `Approving proposal ${proposalId}…`)
    try {
      const payload = await api(path, { method: 'POST', body, idempotencyKey: key })
      const receipt = payload && payload.data
      if (!receipt) throw new ApiError(0, 'STATE_STORE_UNAVAILABLE', 'Approval response did not include a receipt.')
      clearAttempt(attempts.proposalApprove)
      showProposalApproval(receipt, `approve ${proposalId}`, payload.meta && payload.meta.request_id)
      if (receipt.proposal_id) el.proposalApplyId.value = receipt.proposal_id
      if (receipt.approval_ref) el.proposalApplyRef.value = receipt.approval_ref
      const approvedId = receipt.proposal_id || proposalId
      rememberProposalId(task.id, approvedId)
      state.proposalApprovals.set(proposalKey(task.id, approvedId), {
        approvalRef: receipt.approval_ref || '',
        acceptedAt: receipt.accepted_at || '',
        requestId: payload.meta && payload.meta.request_id ? payload.meta.request_id : ''
      })
      await refresh()
      if (state.token) {
        setStatus(`Proposal ${proposalId} approval accepted`)
        if (state.proposalDetail && state.proposalDetail.proposalId === approvedId) {
          await reloadProposalAfterWrite(task.id, approvedId, { quiet: true })
        }
        await refreshReportForTask(task.id, { quiet: true })
      }
    } catch (error) {
      const info = annotateError(error, { idempotencyKey: key, expectedVersion: body.expected_version })
      reportError('Approve proposal', info)
    } finally {
      setBusy(false)
    }
  }

  async function submitProposalApply(event) {
    event.preventDefault()
    if (!state.devMode || state.busy) return
    const task = state.task
    if (!task) {
      reportError('Apply proposal', new ApiError(0, 'FORM_INVALID', 'Select a task before applying a proposal.'))
      return
    }
    let proposalId
    let body
    try {
      proposalId = readText(el.proposalApplyId, 'Proposal ID', 128)
      body = {
        expected_version: readInteger(el.proposalApplyVersion, 'Expected task version', 1, Number.MAX_SAFE_INTEGER),
        approval_ref: readText(el.proposalApplyRef, 'Approval reference', 128),
        proposal_digest: readText(el.proposalApplyDigest, 'Proposal digest', 64)
      }
      if (!/^[a-f0-9]{64}$/.test(body.proposal_digest)) {
        throw new ApiError(0, 'FORM_INVALID', 'Proposal digest must be exactly 64 lowercase hexadecimal characters.')
      }
    } catch (error) {
      reportError('Apply proposal form', error)
      return
    }
    const path = `/v1/tasks/${encodeURIComponent(task.id)}/plan-proposals/${encodeURIComponent(proposalId)}/apply`
    const key = getAttemptKey(attempts.proposalApply, JSON.stringify({ path, body }))
    noteAttemptKey(el.proposalApplyKey, key, body.expected_version)
    setBusy(true, `Applying proposal ${proposalId}…`)
    try {
      const payload = await api(path, { method: 'POST', body, idempotencyKey: key })
      if (!payload || !payload.data) throw new ApiError(0, 'STATE_STORE_UNAVAILABLE', 'Apply response did not include a receipt.')
      clearAttempt(attempts.proposalApply)
      showReceipt(payload.data, `apply ${proposalId}`, payload.meta && payload.meta.request_id, 'Proposal apply receipt')
      rememberProposalId(task.id, proposalId)
      state.proposalReceipts.set(proposalKey(task.id, proposalId), {
        id: payload.data.id,
        state: payload.data.state,
        action: payload.data.action,
        resourceId: payload.data.resource_id,
        acceptedAt: payload.data.accepted_at,
        expectedVersion: payload.data.expected_version,
        requestId: payload.meta && payload.meta.request_id ? payload.meta.request_id : ''
      })
      await refresh()
      if (state.token) {
        setStatus(`Proposal ${proposalId} apply receipt accepted`)
        if (state.proposalDetail && state.proposalDetail.proposalId === proposalId) {
          await reloadProposalAfterWrite(task.id, proposalId, { quiet: true })
        }
        await refreshReportForTask(task.id, { quiet: true })
      }
    } catch (error) {
      const info = annotateError(error, { idempotencyKey: key, expectedVersion: body.expected_version })
      reportError('Apply proposal', info)
    } finally {
      setBusy(false)
    }
  }

  // Fixture session only: this stores the token and issues the explicit GET
  // reads below. There is no QR login, live-session refresh or polling here,
  // so connecting can never start a real scan or refresh an external session.
  function connect() {
    const candidate = el.tokenInput.value.trim()
    if (!candidate) {
      reportError('Connect', new ApiError(0, 'FORM_INVALID', 'Enter a bearer token.'))
      return
    }
    state.token = candidate
    document.body.dataset.connection = 'connecting'
    document.body.dataset.ready = 'false'
    el.tokenInput.value = ''
    el.loginPanel.hidden = true
    el.workspace.hidden = false
    setDevMode(false)
    if (!el.createDeadline.value || new Date(el.createDeadline.value).getTime() <= Date.now()) setDefaultDeadline()
    void refresh()
  }

  function disconnect() {
    state.token = ''
    state.devMode = false
    state.busy = false
    state.selectedTaskId = ''
    state.accounts = []
    state.tasks = []
    state.accountCursor = ''
    state.taskCursor = ''
    state.task = null
    state.graph = null
    state.createAccountIds.clear()
    state.proposalIds.clear()
    state.proposalCache.clear()
    state.proposalApprovals.clear()
    state.proposalReceipts.clear()
    state.proposalDetail = null
    state.report = { taskId: '', at: '', entries: [] }
    state.refreshGeneration += 1
    state.taskLoadSeq += 1
    state.reportSeq += 1
    state.proposalDetailSeq += 1
    for (const attempt of Object.values(attempts)) clearAttempt(attempt)
    clearTaskScopedState()

    state.login.provider = null
    state.login.providerError = ''
    state.login.accountId = ''
    state.login.sessions = []
    state.login.selectedId = ''
    state.login.session = null
    state.login.note = ''
    state.login.noteTone = ''
    state.login.listSeq += 1
    state.login.statusSeq += 1
    state.runtime = { seq: state.runtime.seq + 1, data: null, error: '' }
    renderLoginProvider()
    renderLoginSessions()
    renderLoginSession()
    renderRuntime()

    document.body.dataset.connection = 'disconnected'
    document.body.dataset.ready = 'false'
    el.tokenInput.value = ''
    el.workspace.hidden = true
    el.loginPanel.hidden = false
    el.accountsMore.hidden = true
    el.tasksMore.hidden = true
    el.outcomePanel.hidden = true
    el.outcomeFields.replaceChildren()
    el.outcomeBadge.textContent = '—'
    el.outcomeContext.textContent = ''
    el.outcomeNote.textContent = ''
    el.createKey.textContent = ''
    el.controlsReason.value = ''
    el.detailTaskId.textContent = ''
    el.detail.innerHTML = '<p class="subtle">Select a task.</p>'
    renderAccounts()
    renderTasks()
    renderCreateAccounts()
    setDevMode(false)
    setStatus('Not connected')
    document.body.classList.remove('is-busy')
  }


  // Account login sessions, work trace, external graph sync, and runtime
  // readback. Every POST/PUT below is an explicit operator action; refresh and
  // the automatic panel loads are GET-only and never replay a retained key.
  const LOGIN_OPEN_STATES = ['CREATED', 'QR_READY', 'SCANNED', 'AUTHENTICATING']
  const LOGIN_TERMINAL_STATES = ['ONLINE', 'EXPIRED', 'CANCELLED', 'FAILED']

  function loginStateOpen(value) {
    return LOGIN_OPEN_STATES.includes(String(value || '').toUpperCase())
  }

  function loginStateTerminal(value) {
    return LOGIN_TERMINAL_STATES.includes(String(value || '').toUpperCase())
  }

  function loginTone(value) {
    switch (String(value || '').toUpperCase()) {
      case 'ONLINE': return 'positive'
      case 'FAILED': return 'danger'
      case 'EXPIRED': return 'warn'
      case 'CANCELLED': return 'warn'
      case 'AUTHENTICATING':
      case 'SCANNED': return 'info'
      default: return 'neutral'
    }
  }

  function formatDateTime(value) {
    if (value == null || value === '') return ''
    const date = new Date(value)
    return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString()
  }

  function loginEnabled() {
    const provider = state.login.provider
    return Boolean(provider && (provider.fixture || provider.live) && !state.login.providerError)
  }

  function setLoginNote(message, tone) {
    state.login.note = message || ''
    state.login.noteTone = tone || ''
    el.loginActionNote.textContent = state.login.note
    if (state.login.noteTone) {
      el.loginActionNote.dataset.tone = state.login.noteTone
    } else {
      delete el.loginActionNote.dataset.tone
    }
  }

  function renderLoginProvider() {
    const provider = state.login.provider
    if (state.login.providerError) {
      el.loginProviderStatus.textContent = state.login.providerError
      delete el.loginProviderStatus.dataset.tone
    } else if (!provider) {
      el.loginProviderStatus.textContent = 'Login provider not checked'
      delete el.loginProviderStatus.dataset.tone
    } else {
      const parts = []
      if (provider.fixture) parts.push('fixture')
      if (provider.live) parts.push('live proxy')
      if (!provider.fixture && !provider.live) parts.push('disabled')
      parts.push(`identity readback ${provider.identity_readback === 'none' ? 'none' : (provider.identity_readback || 'unknown')}`)
      if (provider.session_ttl_seconds) parts.push(`session TTL ${provider.session_ttl_seconds}s`)
      if (provider.qr_ttl_seconds) parts.push(`QR TTL ${provider.qr_ttl_seconds}s`)
      el.loginProviderStatus.textContent = parts.join(' · ')
      el.loginProviderStatus.dataset.tone = provider.fixture || provider.live ? '' : 'warn'
    }
    renderLoginAvailability()
  }

  function renderLoginAccounts() {
    const current = state.login.accountId
    el.loginAccount.replaceChildren()
    if (!state.accounts.length) {
      const option = document.createElement('option')
      option.textContent = 'No accounts loaded'
      option.value = ''
      el.loginAccount.append(option)
      state.login.accountId = ''
      state.login.sessions = []
      state.login.selectedId = ''
      state.login.session = null
      setLoginNote('No accounts are loaded; account login is unavailable.', '')
      renderAccountReadiness()
      renderLoginSessions()
      renderLoginSession()
      return
    }
    for (const account of state.accounts) {
      const option = document.createElement('option')
      option.value = account.id
      option.textContent = `${account.id} · ${text(account.platform)} · ${text(account.login_state)}`
      el.loginAccount.append(option)
    }
    const exists = state.accounts.some((account) => account.id === current)
    if (!exists) state.login.accountId = state.accounts[0].id
    el.loginAccount.value = state.login.accountId
    renderAccountReadiness()
  }

  function renderAccountReadiness() {
    const account = state.accounts.find((item) => item.id === state.login.accountId) || null
    if (!account) {
      el.accountReadiness.textContent = 'Account readiness not loaded.'
      delete el.accountReadiness.dataset.tone
      return
    }
    const parts = [
      `identity ${text(account.identity_status)}`,
      `self ${text(account.self_id)}`,
      `login ${text(account.login_state)}`,
      `connection ${text(account.connection_state)}`,
      `risk ${text(account.risk_state)}`,
      `control ${text(account.control_state)}`,
      `v${text(account.version)}`
    ]
    el.accountReadiness.textContent = `Account row readback: ${parts.join(' · ')}. Login never changes risk, control, or quota; only a verified ONLINE readback projects login/connection/identity.`
    if (account.risk_state && account.risk_state !== 'clear') {
      el.accountReadiness.dataset.tone = 'warn'
    } else {
      delete el.accountReadiness.dataset.tone
    }
  }

  function renderLoginSessions() {
    el.loginSessionList.replaceChildren()
    if (!state.login.sessions.length) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'No durable login sessions for this account.'
      el.loginSessionList.append(empty)
      return
    }
    for (const session of state.login.sessions) {
      const row = document.createElement('button')
      row.type = 'button'
      row.className = 'row'
      row.dataset.loginSession = session.id
      row.dataset.testid = 'login-session-row'
      row.setAttribute('aria-current', session.id === state.login.selectedId ? 'true' : 'false')
      const title = document.createElement('span')
      title.className = 'row-title'
      title.textContent = `${session.state} · generation ${session.generation}`
      const meta = document.createElement('span')
      meta.className = 'row-meta'
      meta.textContent = `${session.id} · created ${formatDateTime(session.created_at)}${session.failure_reason ? ` · ${session.failure_reason}` : ''}`
      row.append(title, meta)
      row.addEventListener('click', () => {
        state.login.selectedId = session.id
        state.login.session = session
        setLoginNote('Selected a durable login session. Actions apply to this session only.', '')
        renderLoginSessions()
        renderLoginSession()
      })
      el.loginSessionList.append(row)
    }
  }

  function renderLoginSession() {
    el.loginSessionBody.replaceChildren()
    const session = state.login.session
    if (!session) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'No login session selected. Create one for the selected account to begin.'
      el.loginSessionBody.append(empty)
      renderLoginAvailability()
      return
    }
    const card = document.createElement('article')
    card.className = 'session-card'
    card.dataset.testid = 'login-session'

    const head = document.createElement('div')
    head.className = 'session-head'
    const badge = document.createElement('span')
    badge.className = 'badge'
    badge.dataset.tone = loginTone(session.state)
    badge.dataset.testid = 'login-state'
    badge.textContent = session.state || 'UNKNOWN'
    const id = document.createElement('span')
    id.className = 'session-id'
    id.textContent = session.id
    head.append(badge, id)
    if (session.expired) {
      const expired = document.createElement('span')
      expired.className = 'badge'
      expired.dataset.tone = 'warn'
      expired.textContent = 'expired by clock'
      head.append(expired)
    }
    card.append(head)

    const grid = document.createElement('dl')
    grid.className = 'detail-grid'
    grid.innerHTML = [
      field('Account', session.account_id),
      field('Adapter instance', session.adapter_instance_id),
      field('Runtime', session.runtime_id),
      field('Provider', session.provider),
      field('Generation', session.generation),
      field('Version', session.version),
      field('Expires', formatDateTime(session.expires_at)),
      field('QR expires', formatDateTime(session.qr_expires_at)),
      field('Next action', session.next_action),
      field('Failure reason', session.failure_reason),
      field('Projected', session.projection && session.projection.projected ? 'yes' : 'no'),
      field('Account readback', session.projection ? `${session.projection.account_login_state || '—'} / ${session.projection.account_connection_state || '—'} / identity ${session.projection.identity_status || '—'}` : ''),
      field('Updated', formatDateTime(session.updated_at))
    ].join('')
    card.append(grid)

    if (session.identity) {
      const identity = document.createElement('dl')
      identity.className = 'detail-grid compact'
      identity.dataset.testid = 'login-identity'
      identity.innerHTML = [
        field('QQ self id', session.identity.self_id),
        field('QQ nickname', session.identity.nickname),
        field('Verified at', formatDateTime(session.identity.verified_at)),
        field('Binding checked', session.identity.binding_checked ? 'yes' : 'no')
      ].join('')
      card.append(identity)
    } else if (session.state === 'ONLINE') {
      const warning = document.createElement('p')
      warning.className = 'session-warning'
      warning.dataset.testid = 'login-identity-warning'
      warning.textContent = 'The server reported ONLINE without identity readback. Do not treat this account as ready until the readback shows the verified QQ identity.'
      card.append(warning)
    }

    const qr = document.createElement('div')
    qr.className = 'qr-block'
    if (session.qr_png_data_url && /^data:image\//.test(session.qr_png_data_url)) {
      const figure = document.createElement('figure')
      figure.className = 'qr-frame'
      const image = document.createElement('img')
      image.id = 'login-qr-image'
      image.dataset.testid = 'qr-image'
      image.alt = 'QQ login QR code (short-term, not stored)'
      image.width = 240
      image.height = 240
      image.decoding = 'async'
      image.src = session.qr_png_data_url
      const caption = document.createElement('figcaption')
      caption.textContent = `Short-term QR · expires ${formatDateTime(session.qr_expires_at)}`
      figure.append(image, caption)
      qr.append(figure)
    } else {
      const empty = document.createElement('p')
      empty.className = 'qr-empty'
      empty.dataset.testid = 'login-qr-empty'
      if (loginStateOpen(session.state)) {
        empty.textContent = session.qr_available
          ? 'The server holds a QR image, but this response did not include its bytes. Load the session again.'
          : 'No QR image is cached for this handler. Use “Load QR code” to request a fresh generation; the durable session state remains unchanged by a read.'
      } else {
        empty.textContent = 'No QR image is available for a terminal session.'
      }
      qr.append(empty)
    }
    card.append(qr)

    const note = document.createElement('p')
    note.className = 'session-note'
    note.textContent = 'The QR image is not persisted and is never written to browser storage. The durable row above is the session truth; the account projection is projected only by the server after identity verification.'
    card.append(note)
    el.loginSessionBody.append(card)
    renderLoginSessionListSelection()
    renderLoginAvailability()
  }

  function renderLoginSessionListSelection() {
    for (const row of el.loginSessionList.querySelectorAll('[data-login-session]')) {
      row.setAttribute('aria-current', row.dataset.loginSession === state.login.selectedId ? 'true' : 'false')
    }
  }

  function renderLoginAvailability() {
    const enabled = loginEnabled()
    const session = state.login.session
    const open = Boolean(session && loginStateOpen(session.state))
    const hasAccount = Boolean(state.login.accountId)
    el.loginAccount.disabled = state.busy || !state.accounts.length
    el.loginCreate.disabled = state.busy || !enabled || !hasAccount
    el.loginQR.disabled = state.busy || !enabled || !open
    el.loginPoll.disabled = state.busy || !enabled || !open
    // Cancel is a durable local transition and remains available without a
    // provider, so an operator can always close an open session.
    el.loginCancel.disabled = state.busy || !open
    let reason = ''
    if (!state.login.provider && !state.login.providerError) reason = 'Login provider status has not loaded yet.'
    else if (state.login.providerError) reason = `${state.login.providerError}. Login actions are disabled.`
    else if (!enabled) reason = 'The server reports the login provider as disabled; create, QR, and poll are unavailable. An existing open session can still be cancelled.'
    else if (!hasAccount) reason = 'No account is loaded; select an account to create a login session.'
    else if (!session) reason = 'No login session selected. Create a session explicitly; this console never starts one on refresh.'
    else if (!open) reason = `Session ${session.state} is terminal; create a new session to log in again.`
    else if (session.state === 'CREATED') reason = 'Session created. Load a QR code, then scan it.'
    else if (session.state === 'QR_READY') reason = 'QR ready. Scan it, then press Poll status once.'
    else if (session.state === 'SCANNED') reason = 'QR scanned. Press Poll status once to continue; polling is never automatic.'
    else reason = 'Authenticating. Press Poll status once to read the next server-observed step.'
    if (state.login.providerError || (state.login.provider && !enabled)) {
      setLoginNote(reason, '')
    } else if (reason && !state.login.note) {
      setLoginNote(reason, '')
    }
    el.loginActionNote.dataset.testid = 'action-note'
  }

  async function loadLoginStatus(options = {}) {
    if (!state.token) return
    const sequence = ++state.login.statusSeq
    try {
      const envelope = await api('/v1/login/status')
      if (sequence !== state.login.statusSeq) return
      state.login.provider = envelope.data || null
      state.login.providerError = ''
    } catch (error) {
      if (sequence !== state.login.statusSeq) return
      const info = normalizeError(error)
      state.login.provider = null
      state.login.providerError = info.status === 404 || info.code === 'FEATURE_DISABLED'
        ? 'Login API is not mounted on this server'
        : `${info.code}: ${info.message}`
      if (!options.quiet && info.status !== 404) reportError('Load login provider', error)
    }
    renderLoginProvider()
  }

  async function loadLoginSessions(accountId, options = {}) {
    const sequence = ++state.login.listSeq
    if (!accountId) {
      state.login.sessions = []
      state.login.session = null
      state.login.selectedId = ''
      renderLoginSessions()
      renderLoginSession()
      return
    }
    try {
      const envelope = await api(`/v1/accounts/${encodeURIComponent(accountId)}/login-sessions?limit=20`)
      if (sequence !== state.login.listSeq) return
      state.login.sessions = (envelope.data && envelope.data.items) || []
      const selected = state.login.sessions.find((session) => session.id === state.login.selectedId)
      const fallback = state.login.sessions.find((session) => loginStateOpen(session.state)) || state.login.sessions[0] || null
      state.login.session = selected || fallback
      state.login.selectedId = state.login.session ? state.login.session.id : ''
      renderLoginSessions()
      renderLoginSession()
    } catch (error) {
      if (sequence !== state.login.listSeq) return
      const info = normalizeError(error)
      state.login.sessions = []
      state.login.session = null
      state.login.selectedId = ''
      renderLoginSessions()
      renderLoginSession()
      if (info.status === 404 || info.code === 'FEATURE_DISABLED') {
        setLoginNote('Login session routes are not mounted on this server.', '')
      } else if (!options.quiet) {
        setLoginNote(`${info.code}: ${info.message}`, 'error')
        reportError('Load login sessions', error)
      }
    }
  }

  function cacheLoginSession(session) {
    if (!session || !session.id) return
    state.login.sessions = [session, ...state.login.sessions.filter((item) => item.id !== session.id)].slice(0, 20)
    state.login.selectedId = session.id
    state.login.session = session
    renderLoginSessions()
    renderLoginSession()
  }

  async function reloadLoginSession(sessionId) {
    if (!sessionId) return
    try {
      const envelope = await api(`/v1/login-sessions/${encodeURIComponent(sessionId)}`)
      if (state.login.selectedId !== sessionId) return
      cacheLoginSession(envelope.data)
    } catch (error) {
      const info = normalizeError(error)
      if (info.status === 404) {
        state.login.session = null
        state.login.selectedId = ''
        state.login.sessions = state.login.sessions.filter((session) => session.id !== sessionId)
        renderLoginSessions()
        renderLoginSession()
      }
      setLoginNote(`${info.code}: ${info.message}`, 'error')
    }
  }

  function loginWriteError(context, error) {
    const info = normalizeError(error)
    setLoginNote(`${info.code}: ${info.message}`, 'error')
    reportError(context, error)
    return info
  }

  async function createLoginSession() {
    const accountId = state.login.accountId
    if (!accountId || !loginEnabled()) return
    const key = getAttemptKey(attempts.loginCreate, accountId)
    setBusy(true, 'Creating login session…')
    try {
      const envelope = await api(`/v1/accounts/${encodeURIComponent(accountId)}/login-sessions`, {
        method: 'POST',
        body: {},
        idempotencyKey: key
      })
      clearAttempt(attempts.loginCreate)
      cacheLoginSession(envelope.data)
      setLoginNote('Login session created. Load a QR code explicitly; nothing is polled automatically.', '')
    } catch (error) {
      const info = normalizeError(error)
      if (info.code === 'LOGIN_SESSION_ACTIVE') {
        clearAttempt(attempts.loginCreate)
        await loadLoginSessions(accountId, { quiet: true })
        setLoginNote('An open login session already exists for this account; it was selected. Nothing was created.', '')
      } else if (info.code === 'NETWORK_ERROR') {
        loginWriteError('Create login session', error)
      } else {
        clearAttempt(attempts.loginCreate)
        loginWriteError('Create login session', error)
      }
    } finally {
      setBusy(false)
      renderLoginAvailability()
    }
  }

  async function issueLoginQR() {
    const session = state.login.session
    if (!session || !loginStateOpen(session.state) || !loginEnabled()) return
    const expected = Number(session.generation)
    const signature = `${session.id}:${expected}`
    const key = getAttemptKey(attempts.loginQR, signature)
    setBusy(true, 'Requesting a login QR code…')
    try {
      const envelope = await api(`/v1/login-sessions/${encodeURIComponent(session.id)}/qr`, {
        method: 'POST',
        body: { expected_generation: expected },
        idempotencyKey: key
      })
      clearAttempt(attempts.loginQR)
      cacheLoginSession(envelope.data)
      setLoginNote(`QR ready at generation ${envelope.data.generation}. Scan it, then press Poll status once.`, '')
    } catch (error) {
      const info = normalizeError(error)
      clearAttempt(attempts.loginQR)
      if (info.code === 'LOGIN_GENERATION_STALE') {
        setLoginNote('The QR generation moved before this request. The server refused it without overwriting the newer state; the session was reloaded. Load the current QR generation before scanning.', 'error')
        await reloadLoginSession(session.id)
      } else if (info.code === 'LOGIN_PROVIDER_FAILED') {
        setLoginNote('The login provider failed. The server recorded FAILED durably and did not project the account; it was reloaded.', 'error')
        await reloadLoginSession(session.id)
      } else {
        loginWriteError('Load login QR', error)
        await reloadLoginSession(session.id)
      }
    } finally {
      setBusy(false)
      renderLoginAvailability()
    }
  }

  async function pollLoginSession() {
    const session = state.login.session
    if (!session || !loginStateOpen(session.state) || !loginEnabled()) return
    const expected = Number(session.generation)
    const signature = `${session.id}:${expected}`
    const key = getAttemptKey(attempts.loginPoll, signature)
    setBusy(true, 'Polling login status once…')
    try {
      const envelope = await api(`/v1/login-sessions/${encodeURIComponent(session.id)}/poll`, {
        method: 'POST',
        body: { expected_generation: expected },
        idempotencyKey: key
      })
      clearAttempt(attempts.loginPoll)
      cacheLoginSession(envelope.data)
      const next = envelope.data
      if (loginStateTerminal(next.state)) {
        setLoginNote(`Session reached ${next.state}. The account projection was re-read from the server${next.state === 'ONLINE' ? ' and shows the verified QQ identity' : ''}. This console never reopens the session.`, next.state === 'ONLINE' ? '' : 'warn')
        await refresh()
      } else if (next.state === 'SCANNED') {
        setLoginNote('QR scanned at this generation. Press Poll status once to continue; this console never polls automatically.', '')
      } else if (next.state === 'AUTHENTICATING') {
        setLoginNote('The provider is authenticating. Press Poll status once again to read the next observed step.', '')
      } else if (next.state === 'QR_READY') {
        setLoginNote('The provider still reports QR_READY. Scan the current QR, then press Poll status once.', '')
      } else {
        setLoginNote(`Server state is ${next.state}. Review the session and press Poll status once when ready.`, '')
      }
    } catch (error) {
      const info = normalizeError(error)
      if (info.code === 'LOGIN_GENERATION_STALE') {
        clearAttempt(attempts.loginPoll)
        setLoginNote('A newer QR generation already exists, so this reply was refused and could not overwrite it. The session was reloaded; scan the current QR.', 'error')
        await reloadLoginSession(session.id)
      } else if (info.code === 'LOGIN_PROVIDER_FAILED') {
        clearAttempt(attempts.loginPoll)
        setLoginNote('The provider status call failed; the durable session state was not changed by this request. Reload and retry explicitly.', 'error')
        await reloadLoginSession(session.id)
      } else if (info.code === 'NETWORK_ERROR') {
        loginWriteError('Poll login status', error)
      } else {
        clearAttempt(attempts.loginPoll)
        loginWriteError('Poll login status', error)
        await reloadLoginSession(session.id)
      }
    } finally {
      setBusy(false)
      renderLoginAvailability()
    }
  }

  async function cancelLoginSession() {
    const session = state.login.session
    if (!session || !loginStateOpen(session.state)) return
    const key = getAttemptKey(attempts.loginCancel, session.id)
    setBusy(true, 'Cancelling login session…')
    try {
      const envelope = await api(`/v1/login-sessions/${encodeURIComponent(session.id)}/cancel`, {
        method: 'POST',
        body: {},
        idempotencyKey: key
      })
      clearAttempt(attempts.loginCancel)
      cacheLoginSession(envelope.data)
      setLoginNote('Session cancelled at a new generation. Older asynchronous replies are fenced and cannot reopen it.', 'warn')
      await refresh()
    } catch (error) {
      const info = normalizeError(error)
      if (info.code === 'NETWORK_ERROR') {
        loginWriteError('Cancel login session', error)
      } else {
        clearAttempt(attempts.loginCancel)
        loginWriteError('Cancel login session', error)
        await reloadLoginSession(session.id)
      }
    } finally {
      setBusy(false)
      renderLoginAvailability()
    }
  }

  function selectedAccount() {
    return state.accounts.find((account) => account.id === state.login.accountId) || null
  }

  // Work trace from the S1 denormalized plan. A legacy apply row never creates
  // work, so the empty state names that difference instead of implying failure.
  function resetWorkItems() {
    state.work = { taskId: '', seq: state.work.seq + 1, items: [], hasMore: false, error: '' }
    renderWorkItems()
  }

  function renderWorkItems() {
    el.workItems.replaceChildren()
    if (!state.work.taskId) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'Select a task, then load its work items.'
      el.workItems.append(empty)
      return
    }
    if (state.work.error) {
      const error = document.createElement('p')
      error.className = 'subtle'
      error.textContent = state.work.error
      el.workItems.append(error)
      return
    }
    if (!state.work.items.length) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'No work items for this task. A legacy task, an approval, or an intent-only apply does not create work; materialize an approved proposal to create the execution plan.'
      el.workItems.append(empty)
      return
    }
    for (const item of state.work.items) {
      const row = document.createElement('article')
      row.className = 'work-row'
      row.dataset.testid = 'work-item'
      const attemptText = item.max_attempts != null
        ? `attempts ${text(item.attempt_count)}/${text(item.max_attempts)}`
        : (item.attempt_count != null ? `attempts ${text(item.attempt_count)}` : '')
      const depends = Array.isArray(item.depends_on) && item.depends_on.length ? `depends on ${item.depends_on.join(', ')}` : ''
      const attempts = Array.isArray(item.attempts) ? item.attempts : []
      const attemptRows = attempts.map((attempt) => {
        const outcome = firstDefined(attempt.outcome, attempt.execution_state)
        const refs = [
          attempt.execution_id ? `execution ${attempt.execution_id}` : '',
          attempt.operation_id ? `operation ${attempt.operation_id}` : '',
          attempt.evidence_ref ? `evidence ${attempt.evidence_ref}` : '',
          attempt.observation_event_id ? `observation ${attempt.observation_event_id}` : '',
          attempt.error_code ? `error ${attempt.error_code}` : ''
        ].filter(Boolean).join(' · ')
        return `<div class="attempt-row" data-testid="work-attempt">#${escapeHtml(text(attempt.attempt_no))} · lease ${escapeHtml(text(attempt.lease_epoch))} · ${escapeHtml(text(outcome))}${refs ? `<br>${escapeHtml(refs)}` : ''}<br><span class="row-meta">${escapeHtml(text(formatDateTime(attempt.started_at)))} → ${escapeHtml(text(formatDateTime(attempt.finished_at)))}</span></div>`
      }).join('')
      const unknown = item.state === 'UNKNOWN'
        ? '<br><span class="work-error">External outcome unknown. Do not resend; reconcile or observe evidence first.</span>'
        : ''
      row.innerHTML = [
        `<span><strong>${escapeHtml(text(item.kind))}</strong><span class="row-meta">${escapeHtml(text(item.state))}${attemptText ? ` · ${escapeHtml(attemptText)}` : ''}</span></span>`,
        `<span>work ${escapeHtml(text(item.id))}<br>step ${escapeHtml(text(item.step_no))} · target ${escapeHtml(text(item.target_id))}${depends ? `<br>${escapeHtml(depends)}` : ''}</span>`,
        `<span>proposal ${escapeHtml(text(item.proposal_id))}<br>application ${escapeHtml(text(item.application_id))}<br>materialization ${escapeHtml(text(item.materialization_id))}</span>`,
        `<span>account ${escapeHtml(text(item.account_id))}${item.execution_id ? `<br>execution ${escapeHtml(text(item.execution_id))}` : ''}${item.blocked_reason ? `<br><span class="work-error">${escapeHtml(text(item.blocked_reason))}</span>` : ''}${item.last_error ? `<br><span class="work-error">${escapeHtml(text(item.last_error))}</span>` : ''}${unknown}</span>`,
        attempts.length ? `<details class="work-attempts" data-testid="work-attempts"><summary>${attempts.length} durable attempt${attempts.length === 1 ? '' : 's'}</summary>${attemptRows}</details>` : ''
      ].join('')
      el.workItems.append(row)
    }
    if (state.work.hasMore) {
      const more = document.createElement('p')
      more.className = 'hint'
      more.textContent = 'The task has more than the first 100 work items; this console shows the first page and does not auto-page.'
      el.workItems.append(more)
    }
  }

  async function loadWorkItems(options = {}) {
    const taskId = state.selectedTaskId
    if (!taskId) {
      resetWorkItems()
      return
    }
    const sequence = ++state.work.seq
    state.work.taskId = taskId
    try {
      const envelope = await api(`/v1/tasks/${encodeURIComponent(taskId)}/work-items?limit=100`)
      if (sequence !== state.work.seq || state.selectedTaskId !== taskId) return
      state.work.items = (envelope.data && (envelope.data.items || envelope.data.work_items)) || []
      state.work.hasMore = Boolean(envelope.meta && envelope.meta.has_more)
      state.work.error = ''
    } catch (error) {
      if (sequence !== state.work.seq) return
      const info = normalizeError(error)
      state.work.items = []
      state.work.error = info.status === 404 || info.code === 'FEATURE_DISABLED'
        ? 'Work item routes are not mounted on this server.'
        : `${info.code}: ${info.message}`
      if (!options.quiet && info.status !== 404) reportError('Load work items', error)
    }
    renderWorkItems()
  }

  function resetGraphSync() {
    state.graphSync = { taskId: '', seq: state.graphSync.seq + 1, data: null, writesAllowed: false, error: '' }
    renderGraphSync()
    renderGraphBindAvailability()
  }

  function renderGraphSync() {
    el.graphSyncBody.replaceChildren()
    if (!state.graphSync.taskId) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'Select a task, then load its external graph status.'
      el.graphSyncBody.append(empty)
      return
    }
    if (state.graphSync.error) {
      const error = document.createElement('p')
      error.className = 'subtle'
      error.dataset.testid = 'graph-sync-error'
      error.textContent = state.graphSync.error
      el.graphSyncBody.append(error)
      return
    }
    const data = state.graphSync.data
    if (!data) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'Graph sync has not been loaded.'
      el.graphSyncBody.append(empty)
      return
    }
    const provider = isObject(data.provider) ? data.provider : data
    const providerCard = document.createElement('div')
    providerCard.className = 'graph-sync-card'
    providerCard.dataset.testid = 'graph-sync-provider'
    const providerRows = []
    for (const key of ['provider', 'configured', 'wired', 'writes_allowed']) {
      if (provider[key] !== undefined) providerRows.push({ label: humanize(key), value: formatValue(provider[key]), source: 'reported' })
    }
    providerCard.innerHTML = `<div class="section-heading"><h3>External provider</h3><span class="count">${provider.configured ? 'configured' : 'not configured'}${provider.wired ? ' · wired' : ' · not wired'}</span></div>${renderStatusRows(providerRows, 'The server did not report external provider details.')}`
    el.graphSyncBody.append(providerCard)

    const graphs = Array.isArray(data.graphs) ? data.graphs : Array.isArray(data.external_graphs) ? data.external_graphs : []
    if (!graphs.length) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'No external graphs are reported for this task. Local task and proposal graphs remain authoritative and usable without ARTEX.'
      el.graphSyncBody.append(empty)
      return
    }
    for (const graph of graphs) {
      if (!isObject(graph)) continue
      const card = document.createElement('div')
      card.className = 'graph-sync-card'
      card.dataset.testid = 'graph-sync-graph'
      const rows = []
      const report = (label, ...values) => {
        const value = firstDefined(...values)
        if (value !== undefined && value !== null && value !== '') rows.push({ label, value: formatValue(value), source: 'reported' })
      }
      report('Bound', graph.bound ? 'yes' : 'no')
      report('External root', graph.external_root)
      report('Sync state', graph.sync_state)
      report('Task version', graph.task_version)
      report('Scope revision', graph.scope_revision)
      report('ARTEX revision', graph.graph_revision)
      report('Last sync', formatDateTime(graph.last_sync_at))
      report('Sync lag seconds', graph.sync_lag_seconds)
      report('Anchors', graph.anchors)
      report('External nodes', graph.external_nodes)
      report('Outbox pending', graph.pending_outbox)
      report('Outbox needs reconcile', graph.needs_reconcile_outbox)
      report('Outbox failed', graph.failed_outbox)
      if (isObject(graph.last_error)) {
        report('Last error', graph.last_error.code, graph.last_error.message)
        report('Last error at', formatDateTime(graph.last_error.at))
      }
      const checkpoints = Array.isArray(graph.checkpoints) ? graph.checkpoints : []
      if (checkpoints.length) {
        report('Checkpoints', checkpoints.map((checkpoint) => `${formatValue(checkpoint.source)} ${formatValue(checkpoint.scope_key)} @${formatValue(checkpoint.offset)}`).join(' | '))
      }
      card.innerHTML = `<div class="section-heading"><h3>${escapeHtml(text(graph.graph_kind))} graph</h3><span class="count">${escapeHtml(text(graph.sync_state))}</span></div>${renderStatusRows(rows, 'The server did not report details for this external graph.')}`
      el.graphSyncBody.append(card)
    }
  }

  function renderGraphBindAvailability() {
    const data = state.graphSync.data
    const provider = data && isObject(data.provider) ? data.provider : data
    const wired = Boolean(provider && provider.configured === true && provider.wired === true)
    const kind = el.graphBindKind.value
    const allowed = Boolean(state.task) && wired && (kind === 'fact' || state.graphSync.writesAllowed)
    el.graphBindSubmit.disabled = state.busy || !allowed
    if (!state.task) {
      el.graphBindNote.textContent = 'Select a task first.'
    } else if (!data) {
      el.graphBindNote.textContent = 'Load graph sync to confirm the server capability; binding stays disabled until then.'
    } else if (!wired) {
      el.graphBindNote.textContent = 'The server reports the external provider is not configured and wired; binding is unavailable and local graphs stay authoritative.'
    } else if (kind === 'goal' && !state.graphSync.writesAllowed) {
      el.graphBindNote.textContent = 'Goal binding requires the server to allow external writes; this deployment reports writes disabled.'
    } else {
      el.graphBindNote.textContent = 'Binding changes only the external root mapping; local graphs stay authoritative.'
    }
  }

  async function loadGraphSync(options = {}) {
    const taskId = state.selectedTaskId
    if (!taskId) {
      resetGraphSync()
      return
    }
    const sequence = ++state.graphSync.seq
    state.graphSync.taskId = taskId
    try {
      const envelope = await api(`/v1/tasks/${encodeURIComponent(taskId)}/graph-sync`)
      if (sequence !== state.graphSync.seq || state.selectedTaskId !== taskId) return
      state.graphSync.data = envelope.data || null
      const provider = state.graphSync.data && isObject(state.graphSync.data.provider) ? state.graphSync.data.provider : state.graphSync.data
      state.graphSync.writesAllowed = Boolean(provider && provider.writes_allowed === true)
      state.graphSync.error = ''
    } catch (error) {
      if (sequence !== state.graphSync.seq) return
      const info = normalizeError(error)
      state.graphSync.data = null
      state.graphSync.writesAllowed = false
      state.graphSync.error = info.status === 404 || info.code === 'FEATURE_DISABLED'
        ? 'Graph sync routes are not mounted on this server.'
        : `${info.code}: ${info.message}`
      if (!options.quiet && info.status !== 404) reportError('Load graph sync', error)
    }
    renderGraphSync()
    renderGraphBindAvailability()
  }

  function resetSnapshots() {
    state.snapshots = { taskId: '', seq: state.snapshots.seq + 1, items: [], error: '' }
    renderSnapshots()
  }

  function candidateSummary(candidate) {
    if (!isObject(candidate)) return String(candidate || '')
    const parts = []
    const target = firstDefined(candidate.target_external_id, candidate.external_node_id)
    if (target) parts.push(`target ${formatValue(target)}`)
    if (candidate.text) parts.push(formatValue(candidate.text))
    if (Array.isArray(candidate.evidence_refs) && candidate.evidence_refs.length) parts.push(`evidence ${candidate.evidence_refs.length}`)
    return parts.join(' · ') || formatValue(candidate)
  }

  function renderSnapshots() {
    el.graphSnapshots.replaceChildren()
    if (!state.snapshots.taskId) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'Select a task, then load planning snapshots.'
      el.graphSnapshots.append(empty)
      return
    }
    if (state.snapshots.error) {
      const error = document.createElement('p')
      error.className = 'subtle'
      error.textContent = state.snapshots.error
      el.graphSnapshots.append(error)
      return
    }
    if (!state.snapshots.items.length) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'No planning snapshots for this task.'
      el.graphSnapshots.append(empty)
      return
    }
    for (const snapshot of state.snapshots.items) {
      const row = document.createElement('article')
      row.className = 'static-row row'
      row.dataset.testid = 'planning-snapshot'
      const candidates = Array.isArray(snapshot.candidates) ? snapshot.candidates : []
      const fields = []
      const report = (label, ...values) => {
        const value = firstDefined(...values)
        if (value !== undefined && value !== null && value !== '') fields.push(`<span>${escapeHtml(label)} ${escapeHtml(formatValue(value))}</span>`)
      }
      report('state', snapshot.state)
      report('graph revision', snapshot.graph_revision)
      report('task version', snapshot.task_version)
      report('scope revision', snapshot.scope_revision)
      report('authorization', snapshot.authorization_revision)
      report('model version', snapshot.model_version)
      report('template version', snapshot.template_version)
      report('round', snapshot.max_rounds != null ? `${snapshot.round}/${snapshot.max_rounds}` : snapshot.round)
      report('created', formatDateTime(snapshot.created_at))
      report('expires', formatDateTime(snapshot.expires_at))
      report('digest', snapshot.digest)
      const refs = Array.isArray(snapshot.summary_refs) ? snapshot.summary_refs : []
      const refText = refs.length
        ? refs.map((ref) => `${ref.summary_id} v${ref.summary_version} (${ref.account_id}/${ref.group_id})`).join(' | ')
        : ''
      const anchors = Array.isArray(snapshot.anchor_ids) ? snapshot.anchor_ids.join(', ') : ''
      const candidateText = candidates.length
        ? `Candidates (${candidates.map((candidate) => candidate.provenance || 'inference').filter((value, index, all) => all.indexOf(value) === index).join('/')}): ${candidates.map(candidateSummary).join(' | ')}`
        : 'No candidates recorded in this snapshot.'
      row.innerHTML = `<span class="row-title">Snapshot ${escapeHtml(text(snapshot.snapshot_id))}</span><span class="row-meta">${fields.join('')}</span>${refText ? `<span class="row-meta"><span>summary ${escapeHtml(refText)}</span></span>` : ''}${anchors ? `<span class="row-meta"><span>anchors ${escapeHtml(anchors)}</span></span>` : ''}<p class="hint">${escapeHtml(candidateText)}</p>`
      el.graphSnapshots.append(row)
    }
  }

  async function loadSnapshots(options = {}) {
    const taskId = state.selectedTaskId
    if (!taskId) {
      resetSnapshots()
      return
    }
    const sequence = ++state.snapshots.seq
    state.snapshots.taskId = taskId
    try {
      const envelope = await api(`/v1/tasks/${encodeURIComponent(taskId)}/graph-sync/planning-snapshots`)
      if (sequence !== state.snapshots.seq || state.selectedTaskId !== taskId) return
      const data = envelope.data || {}
      state.snapshots.items = Array.isArray(data) ? data : (data.items || data.snapshots || [])
      state.snapshots.error = ''
    } catch (error) {
      if (sequence !== state.snapshots.seq) return
      const info = normalizeError(error)
      state.snapshots.items = []
      state.snapshots.error = info.status === 404 || info.code === 'FEATURE_DISABLED'
        ? 'Planning snapshot routes are not mounted on this server.'
        : `${info.code}: ${info.message}`
      if (!options.quiet && info.status !== 404) reportError('Load planning snapshots', error)
    }
    renderSnapshots()
  }

  function renderOrchestration() {
    const task = state.task
    if (!task) {
      el.orchestrationMode.textContent = 'No task selected'
      el.orchestrationVersion.value = ''
      el.materializeVersion.value = ''
      return
    }
    const mode = firstDefined(
      task.orchestration_mode,
      task.plan_mode,
      task.authorization && task.authorization.orchestration_mode,
      task.authorization && task.authorization.mode
    )
    el.orchestrationMode.textContent = mode
      ? `Mode ${mode} · task v${text(task.version)} · scope r${text(task.scope_revision)} · auth r${text(task.authorization_revision)}`
      : 'Orchestration mode not reported by this server'
    if (document.activeElement !== el.orchestrationVersion) el.orchestrationVersion.value = String(task.version)
    if (document.activeElement !== el.materializeVersion) el.materializeVersion.value = String(task.version)
    if (mode === 'proposal' || mode === 'materialize') el.orchestrationModeInput.value = 'proposal'
    if (mode === 'legacy') el.orchestrationModeInput.value = 'legacy'
  }

  async function submitOrchestrationMode(event) {
    event.preventDefault()
    const task = state.task
    if (!task) return
    const mode = el.orchestrationModeInput.value
    const version = Number(el.orchestrationVersion.value)
    const reason = el.orchestrationReason.value.trim()
    if (!Number.isInteger(version) || version < 1) {
      el.orchestrationStatus.textContent = 'Expected task version must be a positive integer.'
      return
    }
    if (!reason) {
      el.orchestrationStatus.textContent = 'A reason is required for an authorization-mode change.'
      return
    }
    const key = getAttemptKey(attempts.orchestrationMode, `${task.id}:${mode}:${version}:${reason}`)
    el.orchestrationStatus.textContent = `Submitting with Idempotency-Key ${key}…`
    setBusy(true)
    try {
      await api(`/v1/tasks/${encodeURIComponent(task.id)}/orchestration`, {
        method: 'POST',
        body: { mode, expected_version: version, reason },
        idempotencyKey: key
      })
      clearAttempt(attempts.orchestrationMode)
      el.orchestrationStatus.textContent = 'Mode change accepted. The task and work readback were reloaded; approval and materialize remain separate steps.'
      await refresh()
    } catch (error) {
      const info = normalizeError(error)
      el.orchestrationStatus.textContent = `${info.code}: ${info.message}`
      if (info.code === 'NETWORK_ERROR') {
        el.orchestrationStatus.textContent += ' Delivery is unknown; retrying with the same key and body is safe.'
      } else {
        clearAttempt(attempts.orchestrationMode)
      }
      reportError('Change orchestration mode', error)
      if (info.status === 409) await refresh()
    } finally {
      setBusy(false)
    }
  }

  async function submitMaterialize(event) {
    event.preventDefault()
    const task = state.task
    if (!task) {
      el.materializeStatus.textContent = 'Select a task first.'
      return
    }
    const version = Number(el.materializeVersion.value)
    const proposalId = el.materializeProposal.value.trim()
    const approvalRef = el.materializeApproval.value.trim()
    const digest = el.materializeDigest.value.trim()
    if (!Number.isInteger(version) || version < 1) {
      el.materializeStatus.textContent = 'Expected task version must be a positive integer.'
      return
    }
    if (!proposalId) {
      el.materializeStatus.textContent = 'The approved proposal ID is required: the materialize route is scoped to that exact proposal.'
      return
    }
    if (!approvalRef || !digest) {
      el.materializeStatus.textContent = 'Approval reference and proposal digest are required and must match the approved proposal row.'
      return
    }
    const key = getAttemptKey(attempts.materialize, `${task.id}:${proposalId}:${version}:${approvalRef}:${digest}`)
    el.materializeStatus.textContent = `Submitting with Idempotency-Key ${key}…`
    setBusy(true)
    try {
      const envelope = await api(`/v1/tasks/${encodeURIComponent(task.id)}/plan-proposals/${encodeURIComponent(proposalId)}/materialize`, {
        method: 'POST',
        body: { expected_version: version, approval_ref: approvalRef, proposal_digest: digest },
        idempotencyKey: key
      })
      clearAttempt(attempts.materialize)
      const data = envelope && envelope.data ? envelope.data : {}
      const stateValue = firstDefined(data.state, data.proposal_state, data.work_state)
      el.materializeStatus.textContent = `Materialize accepted${stateValue ? ` · state ${stateValue}` : ''}. The task, proposals, and work items were reloaded. The legacy apply path remains intent-only.`
      await refresh()
      await loadWorkItems({ quiet: true })
    } catch (error) {
      const info = normalizeError(error)
      el.materializeStatus.textContent = `${info.code}: ${info.message}`
      if (info.code === 'NETWORK_ERROR') {
        el.materializeStatus.textContent += ' Delivery is unknown; keep the same key and verify the durable proposal and work state before retrying.'
      } else {
        clearAttempt(attempts.materialize)
      }
      reportError('Materialize proposal', error)
      if (info.status === 409) await refresh()
    } finally {
      setBusy(false)
    }
  }

  async function submitGraphBind(event) {
    event.preventDefault()
    const task = state.task
    const provider = state.graphSync.data && isObject(state.graphSync.data.provider) ? state.graphSync.data.provider : state.graphSync.data
    const wired = Boolean(provider && provider.configured === true && provider.wired === true)
    const kind = el.graphBindKind.value
    if (!task || !wired || (kind === 'goal' && !state.graphSync.writesAllowed)) return
    const root = el.graphBindRoot.value.trim()
    if (!kind || !root) {
      el.graphBindNote.textContent = 'External graph kind and root id are required.'
      return
    }
    const key = getAttemptKey(attempts.graphBind, `${task.id}:${kind}:${root}`)
    el.graphBindNote.textContent = `Binding with Idempotency-Key ${key}…`
    setBusy(true)
    try {
      const envelope = await api(`/v1/tasks/${encodeURIComponent(task.id)}/graph-sync/roots/${encodeURIComponent(kind)}`, {
        method: 'PUT',
        body: { external_root: root },
        idempotencyKey: key
      })
      clearAttempt(attempts.graphBind)
      if (envelope && envelope.data) {
        state.graphSync.data = envelope.data
        const updated = isObject(envelope.data.provider) ? envelope.data.provider : envelope.data
        state.graphSync.writesAllowed = Boolean(updated && updated.writes_allowed === true)
      }
      el.graphBindNote.textContent = 'Binding accepted. The server readback was updated; local graphs remain authoritative.'
      renderGraphSync()
      renderGraphBindAvailability()
      await loadGraphSync({ quiet: true })
    } catch (error) {
      const info = normalizeError(error)
      el.graphBindNote.textContent = `${info.code}: ${info.message}`
      if (info.code === 'NETWORK_ERROR') {
        el.graphBindNote.textContent += ' Delivery is unknown; verify the binding readback before retrying.'
      } else {
        clearAttempt(attempts.graphBind)
      }
      reportError('Bind external graph root', error)
    } finally {
      setBusy(false)
    }
  }

  const SENSITIVE_RUNTIME_KEY = /token|secret|password|credential|authorization|bearer|private|api[_-]?key|digest|hash/i

  function runtimeRows(value, prefix, rows, depth) {
    if (rows.length >= 60 || value == null) return
    if (Array.isArray(value)) {
      rows.push({ label: prefix, value: formatValue(value) })
      return
    }
    if (!isObject(value) || depth >= 3) {
      rows.push({ label: prefix, value: formatValue(value) })
      return
    }
    for (const [key, child] of Object.entries(value)) {
      if (rows.length >= 60) break
      const label = prefix ? `${prefix} · ${humanize(key)}` : humanize(key)
      if (SENSITIVE_RUNTIME_KEY.test(key)) {
        rows.push({ label, value: '[redacted]' })
        continue
      }
      runtimeRows(child, label, rows, depth + 1)
    }
  }

  function renderRuntime() {
    el.runtimeBody.replaceChildren()
    if (state.runtime.error) {
      const error = document.createElement('p')
      error.className = 'subtle'
      error.dataset.testid = 'runtime-error'
      error.textContent = state.runtime.error
      el.runtimeBody.append(error)
      return
    }
    const data = state.runtime.data
    if (!data || !isObject(data)) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'Runtime not loaded.'
      el.runtimeBody.append(empty)
      return
    }
    const entries = Object.entries(data)
    if (!entries.length) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'The runtime route returned no fields.'
      el.runtimeBody.append(empty)
      return
    }
    let cardCount = 0
    for (const [key, value] of entries) {
      if (SENSITIVE_RUNTIME_KEY.test(key)) continue
      const rows = []
      if (isObject(value)) {
        runtimeRows(value, '', rows, 0)
      } else {
        rows.push({ label: humanize(key), value: formatValue(value) })
      }
      if (!rows.length) continue
      const card = document.createElement('div')
      card.className = 'runtime-card'
      card.dataset.testid = 'runtime-card'
      card.dataset.runtimeKey = key
      card.innerHTML = `<h3>${escapeHtml(humanize(key))}</h3>${renderStatusRows(rows, 'No fields reported.')}`
      el.runtimeBody.append(card)
      cardCount += 1
    }
    if (!cardCount) {
      const empty = document.createElement('p')
      empty.className = 'subtle'
      empty.textContent = 'The runtime route returned only values this console withholds from display.'
      el.runtimeBody.append(empty)
    }
  }

  async function loadRuntime(options = {}) {
    const sequence = ++state.runtime.seq
    try {
      const envelope = await api('/v1/operator/runtime')
      if (sequence !== state.runtime.seq) return
      state.runtime.data = envelope.data || null
      state.runtime.error = ''
    } catch (error) {
      if (sequence !== state.runtime.seq) return
      const info = normalizeError(error)
      state.runtime.data = null
      state.runtime.error = info.status === 404 || info.code === 'FEATURE_DISABLED'
        ? 'The operator runtime route is not mounted on this server.'
        : `${info.code}: ${info.message}`
      if (!options.quiet && info.status !== 404) reportError('Load runtime', error)
    }
    renderRuntime()
  }

  function setDefaultDeadline() {
    const date = new Date(Date.now() + 24 * 60 * 60 * 1000)
    const pad = (value) => String(value).padStart(2, '0')
    el.createDeadline.value = `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
  }

  el.createAccounts.addEventListener('change', (event) => {
    const input = event.target
    if (!input || input.name !== 'allowed-account') return
    if (input.checked) {
      state.createAccountIds.add(input.value)
    } else {
      state.createAccountIds.delete(input.value)
    }
  })

  el.errorList.addEventListener('click', (event) => {
    const button = event.target.closest('[data-dismiss-error]')
    if (!button) return
    const id = Number(button.dataset.dismissError)
    state.errors = state.errors.filter((entry) => entry.id !== id)
    renderErrors()
  })

  el.clearErrors.addEventListener('click', () => {
    state.errors = []
    renderErrors()
  })

  el.devMode.addEventListener('change', () => setDevMode(el.devMode.checked))

  el.createForm.addEventListener('submit', (event) => { void submitCreate(event) })
  for (const button of document.querySelectorAll('[data-control]')) {
    button.addEventListener('click', () => { void submitControl(button.dataset.control) })
  }
  el.reconcileButton.addEventListener('click', () => { void submitReconcile() })
  el.proposalCreateForm.addEventListener('submit', (event) => { void submitProposalCreate(event) })
  el.proposalApproveForm.addEventListener('submit', (event) => { void submitProposalApprove(event) })
  el.proposalApplyForm.addEventListener('submit', (event) => { void submitProposalApply(event) })

  el.reportForm.addEventListener('submit', (event) => { void submitReportLoad(event) })
  if (el.reportGraphLoad) el.reportGraphLoad.addEventListener('click', () => { void loadReportGraph() })
  if (el.reportContextForm) el.reportContextForm.addEventListener('submit', (event) => { void loadReportContext(event) })
  el.reportBody.addEventListener('click', (event) => {
    const button = event.target.closest('[data-open-proposal]')
    if (!button) return
    void openProposal(button.dataset.openProposal, { focus: true })
  })
  el.proposalDetailBody.addEventListener('click', (event) => {
    const reload = event.target.closest('[data-reload-proposal]')
    if (reload) {
      void reloadProposalFromApi(reload.dataset.reloadProposal)
      return
    }
    const show = event.target.closest('[data-show-report]')
    if (!show) return
    const target = Array.from(el.reportBody.querySelectorAll('[data-report-proposal]'))
      .find((card) => card.dataset.reportProposal === show.dataset.showReport)
    if (target) {
      target.scrollIntoView({ behavior: 'smooth', block: 'start' })
      el.reportStatus.textContent = `Showing the report row for proposal ${show.dataset.showReport}.`
    } else {
      el.reportStatus.textContent = 'This proposal is not in the current report. Load its ID above to add the row.'
      el.reportBody.scrollIntoView({ behavior: 'smooth', block: 'start' })
    }
  })
  el.proposalDetailApproveForm.addEventListener('submit', (event) => { void submitDetailApprove(event) })
  el.proposalDetailApplyForm.addEventListener('submit', (event) => { void submitDetailApply(event) })

  el.loginAccount.addEventListener('change', () => {
    state.login.accountId = el.loginAccount.value
    renderAccountReadiness()
    state.login.selectedId = ''
    state.login.session = null
    state.login.sessions = []
    state.login.note = ''
    setLoginNote('Account changed. Loading durable sessions for this account.', '')
    renderLoginSessions()
    renderLoginSession()
    void loadLoginSessions(state.login.accountId, { quiet: true })
  })
  el.loginCreate.addEventListener('click', () => { void createLoginSession() })
  el.loginQR.addEventListener('click', () => { void issueLoginQR() })
  el.loginPoll.addEventListener('click', () => { void pollLoginSession() })
  el.loginCancel.addEventListener('click', () => { void cancelLoginSession() })
  el.orchestrationForm.addEventListener('submit', (event) => { void submitOrchestrationMode(event) })
  el.materializeForm.addEventListener('submit', (event) => { void submitMaterialize(event) })
  el.workItemsLoad.addEventListener('click', () => { void loadWorkItems() })
  el.graphSyncLoad.addEventListener('click', () => { void loadGraphSync() })
  el.graphSnapshotsLoad.addEventListener('click', () => { void loadSnapshots() })
  el.graphBindForm.addEventListener('submit', (event) => { void submitGraphBind(event) })
  el.graphBindKind.addEventListener('change', () => renderGraphBindAvailability())
  el.runtimeLoad.addEventListener('click', () => { void loadRuntime() })

  document.querySelector('#connect').addEventListener('click', connect)
  el.tokenInput.addEventListener('keydown', (event) => {
    if (event.key === 'Enter') connect()
  })
  el.refresh.addEventListener('click', () => { void refresh() })
  document.querySelector('#disconnect').addEventListener('click', disconnect)
  el.accountsMore.addEventListener('click', () => { void loadMoreAccounts() })
  el.tasksMore.addEventListener('click', () => { void loadMoreTasks() })

  document.body.dataset.connection = 'disconnected'
  document.body.dataset.ready = 'false'
  setDefaultDeadline()
  setDevMode(false)
  renderErrors()
  renderControlAvailability()
  renderReport()
  renderProposalDetail()
})()
