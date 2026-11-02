import { createHash } from 'node:crypto'
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http'
import type { AddressInfo } from 'node:net'

export type RouteName = 'context' | 'trigger' | 'permit' | 'consume' | 'outcome' | 'gateway' | 'capabilities'

export interface FailureSpec {
  status?: number
  code?: string
  message?: string
  /** Delay before answering with the configured status. */
  delayMs?: number
  /** Destroy the socket instead of answering. */
  reset?: boolean
}

export interface RequestRecord {
  route: RouteName
  method: string
  path: string
  headers: Record<string, string>
  body: Record<string, unknown> | null
  at: number
}

export interface ContextConfig {
  account_id: string
  qq_self_id: string
  conversation_kind: 'group' | 'private'
  conversation_id: string
  active_speaker_account_id?: string | null
  binding_epoch?: string | null
  lease_expires_at?: string | null
  speaker_active?: boolean
  chat_enabled?: boolean
  daily_limit?: number
  issued_today?: number
  managed_self_ids?: string[]
}

interface TriggerRow {
  trigger_ref: string
  account_id: string
  conversation_kind: 'group' | 'private'
  conversation_id: string
  source_event_id: string
  sender_id: string
  directed_account_id: string | null
  deadline_at: string
  segment_count: number
  next_segment: number
  state: 'open' | 'complete' | 'expired' | 'suppressed'
  created_at: string
  requestHash: string
}

interface PermitRow {
  permit_id: string
  send_id: string
  account_id: string
  conversation_kind: 'group' | 'private'
  conversation_id: string
  trigger_ref: string
  text_digest: string
  expected_binding_epoch: string
  issued_at: string
  expires_at: string
  segment_index: number
  segment_count: number
  state: 'issued' | 'consumed' | 'expired'
  requestHash: string
}

interface OutcomeRow {
  permit_id: string
  send_id: string
  state: 'sent' | 'unknown'
  message_id: string | null
  occurred_at: string
  created_at: string
  requestHash: string
}

const REQUIRED_TRIGGER_FIELDS = [
  'trigger_ref',
  'account_id',
  'conversation_kind',
  'conversation_id',
  'source_event_id',
  'sender_id',
  'directed_account_id',
  'deadline_at',
  'segment_count'
] as const

const REQUIRED_PERMIT_FIELDS = [
  'send_id',
  'account_id',
  'conversation_kind',
  'conversation_id',
  'trigger_ref',
  'text_digest',
  'expected_binding_epoch',
  'deadline_at',
  'segment_index',
  'segment_count'
] as const

function digest(value: unknown): string {
  return createHash('sha256').update(JSON.stringify(value), 'utf8').digest('hex')
}

function meta(): Record<string, unknown> {
  return { request_id: `req_${Math.random().toString(36).slice(2, 10)}`, next_cursor: null, has_more: false }
}

function ok(res: ServerResponse, data: unknown, status = 200): void {
  res.writeHead(status, { 'content-type': 'application/json' })
  res.end(JSON.stringify({ data, meta: meta() }))
}

function fail(
  res: ServerResponse,
  status: number,
  code: string,
  message: string,
  details: Record<string, unknown> = {}
): void {
  res.writeHead(status, { 'content-type': 'application/json' })
  res.end(
    JSON.stringify({
      error: { code, message, retryable: false, retry_after_ms: null, details },
      meta: meta()
    })
  )
}

async function readJson(req: IncomingMessage, limit = 1_000_000): Promise<Record<string, unknown> | null> {
  const chunks: Buffer[] = []
  let total = 0
  for await (const chunk of req) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)
    total += buffer.length
    if (total > limit) throw new Error('request body too large')
    chunks.push(buffer)
  }
  if (chunks.length === 0) return null
  const value: unknown = JSON.parse(Buffer.concat(chunks).toString('utf8'))
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('body must be an object')
  return value as Record<string, unknown>
}

function hasExactFields(body: Record<string, unknown>, required: readonly string[]): boolean {
  const keys = Object.keys(body)
  if (keys.length !== required.length) return false
  return required.every((field) => keys.includes(field))
}

function isDecimal(value: unknown): value is string {
  return typeof value === 'string' && /^[1-9][0-9]*$/.test(value)
}

function isDigest(value: unknown): value is string {
  return typeof value === 'string' && /^[a-f0-9]{64}$/.test(value)
}

function isTimestamp(value: unknown): value is string {
  return (
    typeof value === 'string' &&
    /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$/.test(value) &&
    Number.isFinite(Date.parse(value))
  )
}

/**
 * Scripted, fail-closed chat-gate backend for offline tests.
 *
 * It is not an echo server: it enforces the exact wire fields, idempotency,
 * single-consumption, daily budget, digest and trigger/segment ordering rules
 * agreed with the backend owners, and records every request for assertions.
 */
export class FakeChatBackend {
  private readonly server: Server
  private readonly contexts = new Map<string, ContextConfig>()
  private readonly triggers = new Map<string, TriggerRow>()
  private readonly permits = new Map<string, PermitRow>()
  private readonly outcomes = new Map<string, OutcomeRow>()
  private readonly idempotency = new Map<string, string>()
  private readonly failures = new Map<RouteName, FailureSpec[]>()
  private readonly recorded: RequestRecord[] = []
  readonly writerCalls: Array<{account: string; method: string; target: string; text: string}> = []
  unknownWriter = false
  private permitSeq = 0
  private closed = false

  constructor(
    contexts: ContextConfig[],
    private readonly tokens: string | Map<string, string> = 'test-runtime-token'
  ) {
    for (const context of contexts) this.setContext(context)
    this.server = createServer((req, res) => {
      void this.dispatch(req, res).catch((error: unknown) => {
        if (!res.headersSent) fail(res, 500, 'INTERNAL', error instanceof Error ? error.message : 'internal')
        else res.destroy()
      })
    })
  }

  get baseUrl(): string {
    const address = this.server.address() as AddressInfo | null
    if (!address) throw new Error('fake backend is not listening')
    return `http://127.0.0.1:${address.port}`
  }

  async start(): Promise<this> {
    const { promise, resolve } = Promise.withResolvers<void>()
    this.server.listen(0, '127.0.0.1', resolve)
    await promise
    return this
  }

  async stop(): Promise<void> {
    if (this.closed) return
    this.closed = true
    const { promise, resolve, reject } = Promise.withResolvers<void>()
    this.server.close((error) => (error ? reject(error) : resolve()))
    await promise
  }

  setContext(context: ContextConfig): void {
    this.contexts.set(this.contextKey(context.account_id, context.conversation_kind, context.conversation_id), {
      speaker_active: context.conversation_kind === 'group',
      chat_enabled: true,
      daily_limit: 100,
      issued_today: 0,
      active_speaker_account_id:
        context.conversation_kind === 'group' ? context.account_id : null,
      binding_epoch: context.conversation_kind === 'group' ? '7' : '11',
      lease_expires_at: new Date(Date.now() + 3_600_000).toISOString(),
      managed_self_ids: [],
      ...context
    })
  }

  failNext(route: RouteName, failure: FailureSpec): void {
    const queue = this.failures.get(route) ?? []
    queue.push(failure)
    this.failures.set(route, queue)
  }

  requests(route?: RouteName): RequestRecord[] {
    return route ? this.recorded.filter((request) => request.route === route) : [...this.recorded]
  }

  lastRequest(route: RouteName): RequestRecord | undefined {
    return [...this.recorded].reverse().find((request) => request.route === route)
  }

  trigger(triggerRef: string): TriggerRow | undefined {
    return this.triggers.get(triggerRef)
  }

  permit(permitId: string): PermitRow | undefined {
    return this.permits.get(permitId)
  }

  outcome(permitId: string): OutcomeRow | undefined {
    return this.outcomes.get(permitId)
  }

  private contextKey(accountId: string, kind: string, conversationId: string): string {
    return `${accountId}\u0000${kind}\u0000${conversationId}`
  }

  private async dispatch(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const url = new URL(req.url ?? '/', 'http://127.0.0.1')
    const route = this.routeOf(req.method ?? 'GET', url.pathname)
    if (!route) {
      fail(res, 404, 'RESOURCE_NOT_FOUND', `no route for ${req.method} ${url.pathname}`)
      return
    }
    const failure = this.failures.get(route)?.shift()
    if (failure) {
      if (failure.reset) {
        req.socket.destroy()
        return
      }
      if (failure.delayMs) {
        const { promise, resolve } = Promise.withResolvers<void>()
        setTimeout(resolve, failure.delayMs)
        await promise
      }
      fail(res, failure.status ?? 503, failure.code ?? 'STATE_STORE_UNAVAILABLE', failure.message ?? 'injected failure')
      return
    }

    const headers: Record<string, string> = {}
    for (const [key, value] of Object.entries(req.headers)) {
      if (typeof value === 'string') headers[key.toLowerCase()] = value
    }
    const token = headers['authorization']?.startsWith('Bearer ')
      ? headers['authorization'].slice('Bearer '.length)
      : ''
    const accepted = typeof this.tokens === 'string' ? [this.tokens] : [...this.tokens.values()]
    if (!accepted.includes(token)) {
      fail(res, 401, 'UNAUTHENTICATED', 'missing runtime credential')
      return
    }

    let body: Record<string, unknown> | null = null
    if (req.method !== 'GET') {
      try {
        body = await readJson(req)
      } catch (error) {
        fail(res, 400, 'VALIDATION_FAILED', error instanceof Error ? error.message : 'invalid JSON')
        return
      }
      if (!headers['idempotency-key']) {
        fail(res, 400, 'VALIDATION_FAILED', 'Idempotency-Key is required')
        return
      }
    }

    if (this.tokens instanceof Map) {
      const accountId = route === 'capabilities' ? [...this.tokens.keys()][0] : route === 'context'
        ? new URLSearchParams(url.search).get('account_id')
        : typeof body?.account_id === 'string'
          ? body.account_id
          : this.permits.get(url.pathname.split('/')[4] ?? '')?.account_id
      if (route !== 'capabilities' && (!accountId || this.tokens.get(accountId) !== token)) {
        fail(res, 401, 'UNAUTHENTICATED', 'credential does not match account')
        return
      }
    }


    this.recorded.push({
      route,
      method: req.method ?? 'GET',
      path: `${url.pathname}${url.search}`,
      headers,
      body,
      at: Date.now()
    })

    const segments = url.pathname.split('/').filter(Boolean)
    switch (route) {
      case 'capabilities':
        ok(res, { api_version: 'character-gateway-v1', single_text: true }); return
      case 'gateway': {
        const p = this.permits.get(segments[3] ?? '')
        if (!p || !body || p.account_id !== body.account_id || p.conversation_kind !== body.conversation_kind || p.conversation_id !== body.conversation_id || p.send_id !== body.send_id || p.text_digest !== createHash('sha256').update(String(body.text)).digest('hex')) { fail(res,409,'CHAT_SEND_DIGEST_CONFLICT','mismatch'); return }
        if (p.segment_count !== 1) { fail(res,403,'CONTENT_TYPE_UNSUPPORTED','single text only'); return }
        if (p.state !== 'issued') { fail(res,409,'EXTERNAL_OUTCOME_UNKNOWN','already used'); return }
        p.state = 'consumed'
        this.writerCalls.push({account:p.account_id,method:'sendMessage',target:p.conversation_kind === 'private' ? `private:${p.conversation_id}` : p.conversation_id,text:String(body.text)})
        const outcome = { permit_id:p.permit_id,send_id:p.send_id,state:this.unknownWriter ? 'unknown' : 'sent',message_id:this.unknownWriter ? null : 'fake_msg_1',occurred_at:new Date().toISOString(),created_at:new Date().toISOString(),requestHash:'' } as OutcomeRow
        this.outcomes.set(p.permit_id,outcome)
        if (this.unknownWriter) { fail(res,409,'EXTERNAL_OUTCOME_UNKNOWN','writer timeout'); return }
        ok(res,{...p,...outcome,api_version:'character-gateway-v1'}); return
      }
      case 'context':
        this.handleContext(url, res)
        return
      case 'trigger':
        this.handleTrigger(req, res, body)
        return
      case 'permit':
        this.handlePermit(req, res, body)
        return
      case 'consume':
        this.handleConsume(req, res, body, segments[3] ?? '')
        return
      case 'outcome':
        this.handleOutcome(req, res, body, segments[3] ?? '')
        return
    }
  }

  private routeOf(method: string, pathname: string): RouteName | null {
    if (method === 'GET' && pathname.endsWith('/capabilities')) return 'capabilities'
    if (method === 'POST' && pathname.endsWith('/gateway-send')) return 'gateway'
    if (method === 'GET' && pathname === '/v1/internal/chat-context') return 'context'
    if (method === 'POST' && pathname === '/v1/internal/chat-triggers') return 'trigger'
    if (method === 'POST' && pathname === '/v1/internal/chat-send-permits') return 'permit'
    if (method === 'POST' && /^\/v1\/internal\/chat-send-permits\/[^/]+\/consume$/.test(pathname)) return 'consume'
    if (method === 'POST' && /^\/v1\/internal\/chat-send-permits\/[^/]+\/outcome$/.test(pathname)) return 'outcome'
    return null
  }

  private handleContext(url: URL, res: ServerResponse): void {
    const accountId = url.searchParams.get('account_id') ?? ''
    const kind = url.searchParams.get('conversation_kind') ?? ''
    const conversationId = url.searchParams.get('conversation_id') ?? ''
    if (!accountId || (kind !== 'group' && kind !== 'private') || !conversationId) {
      fail(res, 400, 'VALIDATION_FAILED', 'account_id, conversation_kind and conversation_id are required')
      return
    }
    const context = this.contexts.get(this.contextKey(accountId, kind, conversationId))
    if (!context) {
      fail(res, 404, 'RESOURCE_NOT_FOUND', 'chat context is not configured')
      return
    }
    ok(res, { ...context })
  }

  private handleTrigger(
    req: IncomingMessage,
    res: ServerResponse,
    body: Record<string, unknown> | null
  ): void {
    if (!body || !hasExactFields(body, REQUIRED_TRIGGER_FIELDS)) {
      fail(res, 400, 'VALIDATION_FAILED', 'trigger body has missing or unknown fields')
      return
    }
    const triggerRef = body.trigger_ref
    const accountId = body.account_id
    const kind = body.conversation_kind
    const conversationId = body.conversation_id
    const senderId = body.sender_id
    if (
      typeof triggerRef !== 'string' ||
      triggerRef.length < 1 ||
      triggerRef.length > 128 ||
      typeof accountId !== 'string' ||
      (kind !== 'group' && kind !== 'private') ||
      !isDecimal(conversationId) ||
      typeof body.source_event_id !== 'string' ||
      !body.source_event_id ||
      !isDecimal(senderId) ||
      !isTimestamp(body.deadline_at) ||
      !Number.isInteger(body.segment_count) ||
      Number(body.segment_count) < 1 ||
      Number(body.segment_count) > 32 ||
      (body.directed_account_id !== null && typeof body.directed_account_id !== 'string')
    ) {
      fail(res, 400, 'VALIDATION_FAILED', 'trigger body is not contract-valid')
      return
    }
    const context = this.contexts.get(this.contextKey(accountId, kind, conversationId))
    if (!context) {
      fail(res, 404, 'RESOURCE_NOT_FOUND', 'chat context is not configured')
      return
    }
    if (context.managed_self_ids?.includes(senderId) || context.managed_self_ids?.includes(conversationId)) {
      fail(res, 403, 'MANAGED_SELF_ID', 'managed bot cannot trigger Character')
      return
    }
    if (Date.parse(body.deadline_at) <= Date.now()) {
      fail(res, 409, 'CHAT_TRIGGER_EXPIRED', 'trigger deadline has passed')
      return
    }

    const requestHash = digest(body)
    const idempotencyKey = req.headers['idempotency-key']
    if (typeof idempotencyKey === 'string') {
      const bound = this.idempotency.get(`trigger:${idempotencyKey}`)
      if (bound && bound !== requestHash) {
        fail(res, 409, 'IDEMPOTENCY_CONFLICT', 'idempotency key is bound to a different payload')
        return
      }
      this.idempotency.set(`trigger:${idempotencyKey}`, requestHash)
    }

    const existing = this.triggers.get(triggerRef)
    if (existing) {
      if (existing.requestHash !== requestHash) {
        fail(res, 409, 'CHAT_TRIGGER_CONFLICT', 'trigger_ref is bound to a different payload')
        return
      }
      ok(res, this.triggerWire(existing), 200)
      return
    }

    const row: TriggerRow = {
      trigger_ref: triggerRef,
      account_id: accountId,
      conversation_kind: kind,
      conversation_id: conversationId,
      source_event_id: body.source_event_id as string,
      sender_id: senderId,
      directed_account_id: (body.directed_account_id as string | null) ?? null,
      deadline_at: body.deadline_at as string,
      segment_count: Number(body.segment_count),
      next_segment: 0,
      state: 'open',
      created_at: new Date().toISOString(),
      requestHash
    }
    this.triggers.set(triggerRef, row)
    ok(res, this.triggerWire(row), 201)
  }

  private handlePermit(
    req: IncomingMessage,
    res: ServerResponse,
    body: Record<string, unknown> | null
  ): void {
    if (!body || !hasExactFields(body, REQUIRED_PERMIT_FIELDS)) {
      fail(res, 400, 'VALIDATION_FAILED', 'permit body has missing or unknown fields')
      return
    }
    const sendId = body.send_id
    const accountId = body.account_id
    const kind = body.conversation_kind
    const conversationId = body.conversation_id
    const triggerRef = body.trigger_ref
    const segmentIndex = body.segment_index
    const segmentCount = body.segment_count
    if (
      typeof sendId !== 'string' ||
      typeof accountId !== 'string' ||
      (kind !== 'group' && kind !== 'private') ||
      !isDecimal(conversationId) ||
      typeof triggerRef !== 'string' ||
      !isDigest(body.text_digest) ||
      !isDecimal(body.expected_binding_epoch) ||
      !isTimestamp(body.deadline_at) ||
      !Number.isInteger(segmentIndex) ||
      !Number.isInteger(segmentCount) ||
      Number(segmentIndex) < 0 ||
      Number(segmentIndex) >= Number(segmentCount) ||
      Number(segmentCount) < 1 ||
      Number(segmentCount) > 32
    ) {
      fail(res, 400, 'VALIDATION_FAILED', 'permit body is not contract-valid')
      return
    }

    const trigger = this.triggers.get(triggerRef)
    if (!trigger) {
      fail(res, 404, 'RESOURCE_NOT_FOUND', 'trigger is unknown')
      return
    }
    if (trigger.account_id !== accountId || trigger.conversation_kind !== kind || trigger.conversation_id !== conversationId) {
      fail(res, 409, 'STATE_CONFLICT', 'permit scope does not match the trigger')
      return
    }
    if (trigger.state !== 'open' || Date.parse(trigger.deadline_at) <= Date.now()) {
      fail(res, 409, 'CHAT_TRIGGER_EXPIRED', 'trigger is not open')
      return
    }
    if (trigger.segment_count !== Number(segmentCount) || trigger.next_segment !== Number(segmentIndex)) {
      fail(res, 409, 'CHAT_SEGMENT_OUT_OF_ORDER', 'segment index is not the next trigger segment')
      return
    }

    const context = this.contexts.get(this.contextKey(accountId, kind, conversationId))
    if (!context) {
      fail(res, 404, 'RESOURCE_NOT_FOUND', 'chat context is not configured')
      return
    }
    if (!context.chat_enabled) {
      fail(res, 403, 'CHAT_POLICY_DISABLED', 'chat policy is disabled')
      return
    }
    if (kind === 'group' && !context.speaker_active) {
      fail(res, 403, 'CHAT_SPEAKER_NOT_ACTIVE', 'account is not the active group speaker')
      return
    }
    if (context.binding_epoch !== body.expected_binding_epoch) {
      fail(res, 409, 'STATE_CONFLICT', 'binding epoch does not match')
      return
    }
    if ((context.issued_today ?? 0) >= (context.daily_limit ?? 100)) {
      fail(res, 409, 'CHAT_BUDGET_EXHAUSTED', 'daily chat budget is exhausted')
      return
    }

    const requestHash = digest(body)
    const idempotencyKey = req.headers['idempotency-key']
    if (typeof idempotencyKey === 'string') {
      const bound = this.idempotency.get(`permit:${idempotencyKey}`)
      if (bound && bound !== requestHash) {
        fail(res, 409, 'IDEMPOTENCY_CONFLICT', 'idempotency key is bound to a different payload')
        return
      }
      this.idempotency.set(`permit:${idempotencyKey}`, requestHash)
    }

    const existing = [...this.permits.values()].find(
      (permit) => permit.send_id === sendId && permit.segment_index === Number(segmentIndex)
    )
    if (existing) {
      if (existing.requestHash !== requestHash) {
        fail(res, 409, 'CHAT_SEND_DIGEST_CONFLICT', 'send id is bound to different text')
        return
      }
      ok(res, this.permitWire(existing), 200)
      return
    }

    const now = Date.now()
    const row: PermitRow = {
      permit_id: `permit_${++this.permitSeq}`,
      send_id: sendId,
      account_id: accountId,
      conversation_kind: kind,
      conversation_id: conversationId,
      trigger_ref: triggerRef,
      text_digest: body.text_digest,
      expected_binding_epoch: body.expected_binding_epoch,
      issued_at: new Date(now).toISOString(),
      expires_at: new Date(Math.min(now + 15_000, Date.parse(trigger.deadline_at))).toISOString(),
      segment_index: Number(segmentIndex),
      segment_count: Number(segmentCount),
      state: 'issued',
      requestHash
    }
    this.permits.set(row.permit_id, row)
    context.issued_today = (context.issued_today ?? 0) + 1
    ok(res, this.permitWire(row), 200)
  }

  private handleConsume(
    req: IncomingMessage,
    res: ServerResponse,
    body: Record<string, unknown> | null,
    permitId: string
  ): void {
    if (
      !body ||
      !hasExactFields(body, ['send_id', 'segment_index', 'text_digest']) ||
      typeof body.send_id !== 'string' ||
      !Number.isInteger(body.segment_index) ||
      !isDigest(body.text_digest)
    ) {
      fail(res, 400, 'VALIDATION_FAILED', 'consume body is not contract-valid')
      return
    }
    const permit = this.permits.get(permitId)
    if (!permit) {
      fail(res, 404, 'RESOURCE_NOT_FOUND', 'permit is unknown')
      return
    }
    if (permit.state === 'consumed') {
      fail(res, 409, 'CHAT_PERMIT_CONSUMED', 'permit was already consumed')
      return
    }
    if (permit.state === 'expired' || Date.parse(permit.expires_at) <= Date.now()) {
      permit.state = 'expired'
      fail(res, 409, 'CHAT_PERMIT_EXPIRED', 'permit expired')
      return
    }
    if (
      permit.send_id !== body.send_id ||
      permit.segment_index !== body.segment_index ||
      permit.text_digest !== body.text_digest
    ) {
      fail(res, 409, 'CHAT_SEND_DIGEST_CONFLICT', 'consume does not match the permit')
      return
    }
    const trigger = this.triggers.get(permit.trigger_ref)
    if (!trigger || trigger.state !== 'open' || Date.parse(trigger.deadline_at) <= Date.now()) {
      fail(res, 409, 'CHAT_TRIGGER_EXPIRED', 'trigger is not open')
      return
    }
    const context = this.contexts.get(
      this.contextKey(permit.account_id, permit.conversation_kind, permit.conversation_id)
    )
    if (!context || !context.chat_enabled) {
      fail(res, 403, 'CHAT_POLICY_DISABLED', 'chat policy is disabled')
      return
    }
    if (permit.conversation_kind === 'group' && !context.speaker_active) {
      fail(res, 403, 'CHAT_SPEAKER_NOT_ACTIVE', 'account is not the active group speaker')
      return
    }

    permit.state = 'consumed'
    trigger.next_segment = Math.max(trigger.next_segment, permit.segment_index + 1)
    if (trigger.next_segment >= trigger.segment_count) trigger.state = 'complete'
    ok(res, this.permitWire(permit), 200)
  }

  private handleOutcome(
    req: IncomingMessage,
    res: ServerResponse,
    body: Record<string, unknown> | null,
    permitId: string
  ): void {
    if (
      !body ||
      !hasExactFields(body, ['send_id', 'state', 'message_id', 'occurred_at']) ||
      typeof body.send_id !== 'string' ||
      (body.state !== 'sent' && body.state !== 'unknown') ||
      (body.message_id !== null && typeof body.message_id !== 'string') ||
      !isTimestamp(body.occurred_at)
    ) {
      fail(res, 400, 'VALIDATION_FAILED', 'outcome body is not contract-valid')
      return
    }
    if (body.state === 'unknown' && body.message_id !== null) {
      fail(res, 400, 'VALIDATION_FAILED', 'unknown requires a null message_id')
      return
    }
    if (body.state === 'sent' && body.message_id !== null && typeof body.message_id !== 'string') {
      fail(res, 400, 'VALIDATION_FAILED', 'sent requires a message_id or null')
      return
    }
    const permit = this.permits.get(permitId)
    if (!permit) {
      fail(res, 404, 'RESOURCE_NOT_FOUND', 'permit is unknown')
      return
    }
    if (permit.state !== 'consumed') {
      fail(res, 409, 'STATE_CONFLICT', 'outcome requires a consumed permit')
      return
    }
    const requestHash = digest(body)
    const existing = this.outcomes.get(permitId)
    if (existing) {
      if (existing.requestHash !== requestHash) {
        fail(res, 409, 'CHAT_OUTCOME_CONFLICT', 'permit already has a different outcome')
        return
      }
      ok(res, this.outcomeWire(existing), 200)
      return
    }
    const row: OutcomeRow = {
      permit_id: permitId,
      send_id: body.send_id,
      state: body.state,
      message_id: (body.message_id as string | null) ?? null,
      occurred_at: body.occurred_at,
      created_at: new Date().toISOString(),
      requestHash
    }
    this.outcomes.set(permitId, row)
    ok(res, this.outcomeWire(row), 201)
  }

  private triggerWire(row: TriggerRow): Record<string, unknown> {
    return {
      trigger_ref: row.trigger_ref,
      account_id: row.account_id,
      conversation_kind: row.conversation_kind,
      conversation_id: row.conversation_id,
      source_event_id: row.source_event_id,
      sender_id: row.sender_id,
      directed_account_id: row.directed_account_id,
      deadline_at: row.deadline_at,
      segment_count: row.segment_count,
      next_segment: row.next_segment,
      state: row.state,
      created_at: row.created_at
    }
  }

  private permitWire(row: PermitRow): Record<string, unknown> {
    return {
      permit_id: row.permit_id,
      send_id: row.send_id,
      account_id: row.account_id,
      conversation_kind: row.conversation_kind,
      conversation_id: row.conversation_id,
      trigger_ref: row.trigger_ref,
      text_digest: row.text_digest,
      expected_binding_epoch: row.expected_binding_epoch,
      issued_at: row.issued_at,
      expires_at: row.expires_at,
      segment_index: row.segment_index,
      segment_count: row.segment_count,
      state: row.state
    }
  }

  private outcomeWire(row: OutcomeRow): Record<string, unknown> {
    return {
      permit_id: row.permit_id,
      send_id: row.send_id,
      state: row.state,
      message_id: row.message_id,
      occurred_at: row.occurred_at,
      created_at: row.created_at
    }
  }
}
