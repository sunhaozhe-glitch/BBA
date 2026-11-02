import { CharacterRuntimeError, BackendHttpError } from './errors.js'
import type {
  ChatContext,
  ChatPermit,
  ChatPermitRequest,
  ChatTrigger,
  ChatTriggerRequest,
  CharacterRuntimeLogger,
  ConversationKind,
  GroupContext
} from './types.js'

export interface ChatBackendClientOptions {
  baseUrl: string
  runtimeToken: string
  /** Explicit trusted final gateway; never inherits the issue-service credential. */
  gateway?: { baseUrl: string; token: string }
  fetchImpl?: typeof fetch
  requestTimeoutMs?: number
  logger?: CharacterRuntimeLogger
  endpoints?: {
    triggers?: string
    context?: string
    groupContext?: string
    permits?: string
    consume?: (permitId: string) => string
    outcome?: (permitId: string) => string
  }
}

export const DEFAULT_ENDPOINTS = {
  triggers: '/v1/internal/chat-triggers',
  context: '/v1/internal/chat-context',
  groupContext: '/v1/internal/group-context',
  permits: '/v1/internal/chat-send-permits',
  consume: (permitId: string) => `/v1/internal/chat-send-permits/${encodeURIComponent(permitId)}/consume`,
  outcome: (permitId: string) => `/v1/internal/chat-send-permits/${encodeURIComponent(permitId)}/outcome`
}

interface Envelope<T> {
  data?: T
  meta?: unknown
}

/**
 * Minimal, fail-closed HTTP client for the backend chat gate.
 *
 * It never retries a mutating request: the caller owns one-shot semantics and a
 * retried consume must not lead to a second adapter call.
 */
export class ChatBackendClient {
  private readonly fetchImpl: typeof fetch
  private readonly baseUrl: string
  private readonly timeoutMs: number
  private readonly consumePath: (permitId: string) => string
  private readonly outcomePath: (permitId: string) => string

  constructor(private readonly options: ChatBackendClientOptions) {
    this.baseUrl = options.baseUrl.replace(/\/+$/, '')
    if (!this.baseUrl) throw new CharacterRuntimeError('invalid_config', 'backendBaseUrl is empty')
    if (!options.runtimeToken) throw new CharacterRuntimeError('invalid_config', 'runtimeToken is empty')
    this.fetchImpl = options.fetchImpl ?? fetch
    this.timeoutMs = options.requestTimeoutMs ?? 5_000
    if ((options.endpoints as { outcome?: unknown } | undefined)?.outcome === null) {
      throw new CharacterRuntimeError(
        'invalid_config',
        'the chat send outcome endpoint is mandatory and cannot be disabled'
      )
    }
    this.consumePath = options.endpoints?.consume ?? DEFAULT_ENDPOINTS.consume
    this.outcomePath = options.endpoints?.outcome ?? DEFAULT_ENDPOINTS.outcome
  }

  get triggerPath(): string {
    return this.options.endpoints?.triggers ?? DEFAULT_ENDPOINTS.triggers
  }

  get contextPath(): string {
    return this.options.endpoints?.context ?? DEFAULT_ENDPOINTS.context
  }

  get permitPath(): string {
    return this.options.endpoints?.permits ?? DEFAULT_ENDPOINTS.permits
  }

  get groupContextPath(): string {
    return this.options.endpoints?.groupContext ?? DEFAULT_ENDPOINTS.groupContext
  }

  async getContext(input: {
    accountId: string
    kind: ConversationKind
    conversationId: string
  }): Promise<ChatContext> {
    const params = new URLSearchParams({
      account_id: input.accountId,
      conversation_kind: input.kind,
      conversation_id: input.conversationId
    })
    const data = await this.request<ChatContext>(`${this.contextPath}?${params.toString()}`, {
      method: 'GET'
    })
    assertContext(data, input)
    return data
  }

  /**
   * Read-only Summary-backed GroupContext. The backend owns account scope,
   * group scope, Summary version and expiry; this client rejects a response
   * that does not echo the requested account/group.
   */
  async getGroupContext(input: { accountId: string; groupId: string }): Promise<GroupContext> {
    const params = new URLSearchParams({
      account_id: input.accountId,
      group_id: input.groupId
    })
    const data = await this.request<GroupContext>(`${this.groupContextPath}?${params.toString()}`, {
      method: 'GET'
    })
    assertGroupContext(data, input)
    return data
  }

  async openTrigger(request: ChatTriggerRequest): Promise<ChatTrigger> {
    if (request.trigger_ref.length < 1 || request.trigger_ref.length > 128) {
      throw new CharacterRuntimeError('invalid_config', 'trigger_ref must be 1..128 characters')
    }
    const data = await this.request<ChatTrigger>(this.triggerPath, {
      method: 'POST',
      body: request,
      idempotencyKey: request.trigger_ref
    })
    assertTrigger(data, request)
    return data
  }

  async issuePermit(request: ChatPermitRequest): Promise<ChatPermit> {
    const data = await this.request<ChatPermit>(this.permitPath, {
      method: 'POST',
      body: request,
      idempotencyKey: request.send_id
    })
    return data
  }

  async consumePermit(
    permitId: string,
    body: { send_id: string; segment_index: number; text_digest: string }
  ): Promise<ChatPermit> {
    throw new CharacterRuntimeError('backend_rejected', 'CONTENT_FINAL_TEXT_REQUIRED: digest-only consumption is retired; use final gateway')
  }

  /**
   * Retired compatibility API: clients cannot overwrite final gateway facts.
   */
  async reportOutcome(
    permitId: string,
    body: {
      send_id: string
      state: 'sent' | 'unknown'
      message_id: string | null
      occurred_at: string
    }
  ): Promise<void> {
    throw new CharacterRuntimeError('backend_rejected', 'CONTENT_FINAL_TEXT_REQUIRED: delivery facts belong to final gateway')
  }

  async gatewaySend(permitId: string, body: {
    send_id: string; segment_index: number; text: string;
    account_id: string; conversation_kind: ConversationKind; conversation_id: string
  }): Promise<{ state: 'sent'; message_id: string }> {
    const gateway = this.options.gateway
    if (!gateway?.baseUrl || !gateway.token) {
      throw new CharacterRuntimeError('backend_rejected', 'final gateway must be explicitly configured')
    }
    const capability = await this.request<{ api_version: string; single_text: boolean }>(
      '/v1/internal/chat-send-permits/capabilities', { method: 'GET' }, gateway)
    if (capability.api_version !== 'character-gateway-v1' || capability.single_text !== true) {
      throw new CharacterRuntimeError('backend_rejected', 'final gateway capability unavailable')
    }
    const receipt = await this.request<Record<string, unknown>>(
      `/v1/internal/chat-send-permits/${encodeURIComponent(permitId)}/gateway-send`,
      { method: 'POST', body, idempotencyKey: body.send_id }, gateway)
    if (receipt.api_version !== 'character-gateway-v1' || receipt.permit_id !== permitId ||
        receipt.send_id !== body.send_id || receipt.segment_index !== body.segment_index ||
        receipt.account_id !== body.account_id || receipt.conversation_kind !== body.conversation_kind ||
        receipt.conversation_id !== body.conversation_id || receipt.state !== 'sent' ||
        typeof receipt.message_id !== 'string' || !receipt.message_id) {
      throw new CharacterRuntimeError('delivery_unknown', 'gateway returned no bound terminal receipt')
    }
    return { state: 'sent', message_id: receipt.message_id }
  }

  private async request<T>(
    path: string,
    options: { method: 'GET' | 'POST'; body?: unknown; idempotencyKey?: string },
    destination: { baseUrl: string; token: string } = { baseUrl: this.baseUrl, token: this.options.runtimeToken }
  ): Promise<T> {
    const base = new URL(destination.baseUrl)
    const target = new URL(path, base)
    if (!path.startsWith('/') || path.startsWith('//') || target.origin !== base.origin ||
        base.username || base.password || base.search || base.hash || base.pathname !== '/' ||
        (base.protocol !== 'https:' && !(base.protocol === 'http:' && ['127.0.0.1', '[::1]', 'localhost'].includes(base.hostname)))) {
      throw new CharacterRuntimeError('invalid_config', 'untrusted backend endpoint')
    }
    const headers: Record<string, string> = {
      authorization: `Bearer ${destination.token}`,
      accept: 'application/json'
    }
    if (options.body !== undefined) headers['content-type'] = 'application/json'
    if (options.idempotencyKey !== undefined) headers['idempotency-key'] = options.idempotencyKey

    let response: Response
    try {
      response = await this.fetchImpl(target.href, {
        redirect: 'error',
        method: options.method,
        headers,
        body: options.body === undefined ? undefined : JSON.stringify(options.body),
        signal: AbortSignal.timeout(this.timeoutMs)
      })
    } catch (error) {
      throw new CharacterRuntimeError('backend_unavailable', `chat gate backend request failed: ${path}`, {
        cause: error
      })
    }

    const raw = await response.text()
    let parsed: unknown = null
    if (raw.length > 0) {
      try {
        parsed = JSON.parse(raw)
      } catch (error) {
        throw new BackendHttpError(
          response.status,
          null,
          `chat gate backend returned non-JSON for ${path}`,
          response.ok ? 'backend_rejected' : 'backend_unavailable',
          { cause: error }
        )
      }
    }

    if (!response.ok) {
      const envelope = parsed as { error?: { code?: unknown; message?: unknown } } | null
      const code = typeof envelope?.error?.code === 'string' ? envelope.error.code : null
      const message =
        typeof envelope?.error?.message === 'string'
          ? envelope.error.message
          : `chat gate backend returned HTTP ${response.status} for ${path}`
      throw new BackendHttpError(
        response.status,
        code,
        message,
        response.status >= 500 ? 'backend_unavailable' : 'backend_rejected'
      )
    }

    const envelope = parsed as Envelope<T> | T | null
    if (envelope === null) {
      throw new BackendHttpError(response.status, null, `chat gate backend returned an empty body for ${path}`)
    }
    if (typeof envelope === 'object' && envelope !== null && 'data' in envelope) {
      return (envelope as Envelope<T>).data as T
    }
    return envelope as T
  }
}

function assertContext(data: ChatContext, expected: { accountId: string; kind: ConversationKind; conversationId: string }): void {
  if (
    !data ||
    data.account_id !== expected.accountId ||
    data.conversation_kind !== expected.kind ||
    String(data.conversation_id) !== String(expected.conversationId)
  ) {
    throw new CharacterRuntimeError('cross_account_event', 'chat context does not match the requesting account')
  }
  if (typeof data.qq_self_id !== 'string') {
    throw new CharacterRuntimeError('invalid_binding', 'chat context has no qq_self_id')
  }
  if (typeof data.speaker_active !== 'boolean' || typeof data.chat_enabled !== 'boolean') {
    throw new CharacterRuntimeError('backend_rejected', 'chat context is missing speaker/chat flags')
  }
  if (!Array.isArray(data.managed_self_ids) || data.managed_self_ids.some((id) => typeof id !== 'string')) {
    throw new CharacterRuntimeError('backend_rejected', 'chat context has no managed self id deny list')
  }
}

const GROUP_CONTEXT_EVIDENCE = ['member_statement', 'deterministic_statistic', 'model_inference']

function assertGroupContext(data: GroupContext, expected: { accountId: string; groupId: string }): void {
  const isDate = (value: unknown): value is string =>
    typeof value === 'string' && Number.isFinite(Date.parse(value))
  if (
    !data ||
    data.account_id !== expected.accountId ||
    String(data.group_id) !== String(expected.groupId)
  ) {
    throw new CharacterRuntimeError('cross_account_event', 'group context does not match the requesting account/group')
  }
  if (!isDate(data.generated_at) || !isDate(data.expires_at)) {
    throw new CharacterRuntimeError('backend_rejected', 'group context has no usable generation/expiry time')
  }
  if (typeof data.source !== 'string' || data.source.length === 0 || data.source.length > 256) {
    throw new CharacterRuntimeError('backend_rejected', 'group context has no bounded source')
  }
  if (
    (data.summary_ref != null && typeof data.summary_ref !== 'string') ||
    (data.summary_id !== undefined && data.summary_id !== null && typeof data.summary_id !== 'string') ||
    (data.summary_version !== undefined && data.summary_version !== null &&
      (!Number.isInteger(data.summary_version) || data.summary_version < 1)) ||
    (data.source_manifest_sha256 !== undefined && data.source_manifest_sha256 !== null &&
      !/^[0-9a-f]{64}$/.test(data.source_manifest_sha256)) ||
    (data.privacy_class !== undefined && !['local_only', 'redacted_external_allowed', 'none'].includes(data.privacy_class))
  ) {
    throw new CharacterRuntimeError('backend_rejected', 'group context Summary identity is malformed')
  }
  if (!Array.isArray(data.topics) || data.topics.length > 20) {
    throw new CharacterRuntimeError('backend_rejected', 'group context topics are missing or unbounded')
  }
  for (const topic of data.topics) {
    if (
      !topic ||
      typeof topic.summary !== 'string' || topic.summary.length === 0 || topic.summary.length > 4000 ||
      !Array.isArray(topic.message_refs) || topic.message_refs.length > 100 ||
      topic.message_refs.some((ref) => typeof ref !== 'string' || ref.length === 0 || ref.length > 256) ||
      !GROUP_CONTEXT_EVIDENCE.includes(topic.evidence_kind)
    ) {
      throw new CharacterRuntimeError('backend_rejected', 'group context topic is malformed')
    }
  }
  if (data.activity_window != null && (
    !isDate(data.activity_window.start) ||
    !isDate(data.activity_window.end) ||
    Date.parse(data.activity_window.start) > Date.parse(data.activity_window.end) ||
    !Number.isInteger(data.activity_window.message_count) || data.activity_window.message_count < 0
  )) {
    throw new CharacterRuntimeError('backend_rejected', 'group context activity window is malformed')
  }
  if (data.cooldown_until != null && !isDate(data.cooldown_until)) {
    throw new CharacterRuntimeError('backend_rejected', 'group context cooldown is malformed')
  }
  if (data.daily_remaining != null &&
    (!Number.isInteger(data.daily_remaining) || data.daily_remaining < 0)) {
    throw new CharacterRuntimeError('backend_rejected', 'group context daily budget is malformed')
  }
  if (data.active_speaker_account_id != null &&
    typeof data.active_speaker_account_id !== 'string') {
    throw new CharacterRuntimeError('backend_rejected', 'group context speaker is malformed')
  }
}

function assertTrigger(data: ChatTrigger, request: ChatTriggerRequest): void {
  if (
    !data ||
    data.trigger_ref !== request.trigger_ref ||
    data.account_id !== request.account_id ||
    data.conversation_kind !== request.conversation_kind ||
    String(data.conversation_id) !== String(request.conversation_id) ||
    data.sender_id !== request.sender_id ||
    data.segment_count !== request.segment_count ||
    !Number.isInteger(data.next_segment) ||
    data.next_segment < 0 ||
    data.next_segment > request.segment_count ||
    !['open', 'complete', 'suppressed', 'expired'].includes(data.state)
  ) {
    throw new CharacterRuntimeError('backend_rejected', 'chat trigger echo does not match the request')
  }
}

export function validatePermit(
  permit: ChatPermit,
  request: ChatPermitRequest,
  nowMs: number
): ChatPermit {
  if (
    !permit ||
    typeof permit.permit_id !== 'string' ||
    permit.permit_id.length < 1 ||
    permit.permit_id.length > 128 ||
    permit.send_id !== request.send_id ||
    permit.account_id !== request.account_id ||
    permit.conversation_kind !== request.conversation_kind ||
    String(permit.conversation_id) !== String(request.conversation_id) ||
    permit.trigger_ref !== request.trigger_ref ||
    permit.text_digest !== request.text_digest ||
    permit.expected_binding_epoch !== request.expected_binding_epoch ||
    permit.segment_index !== request.segment_index ||
    permit.segment_count !== request.segment_count
  ) {
    throw new CharacterRuntimeError('permit_mismatch', 'chat send permit does not match the requested segment')
  }
  const expiresAt = Date.parse(permit.expires_at)
  if (!Number.isFinite(expiresAt) || expiresAt <= nowMs) {
    throw new CharacterRuntimeError('permit_expired', 'chat send permit is already expired')
  }
  if (Date.parse(request.deadline_at) < nowMs) {
    throw new CharacterRuntimeError('permit_expired', 'trigger deadline passed before permit issuance')
  }
  return permit
}
