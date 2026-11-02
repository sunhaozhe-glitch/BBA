import { AIMessageChunk, type BaseMessage } from '@langchain/core/messages'
import { ChatGenerationChunk } from '@langchain/core/outputs'
import { ChatLunaPlugin } from 'koishi-plugin-chatluna/services/chat'
import { ModelRequester, type ModelRequestParams } from 'koishi-plugin-chatluna/llm-core/platform/api'
import { PlatformModelClient } from 'koishi-plugin-chatluna/llm-core/platform/client'
import type { ClientConfigPool } from 'koishi-plugin-chatluna/llm-core/platform/config'
import { ChatLunaChatModel } from 'koishi-plugin-chatluna/llm-core/platform/model'
import { ModelCapabilities, ModelType } from 'koishi-plugin-chatluna/llm-core/platform/types'
import type { Context } from 'koishi'
import { isIP } from 'node:net'
import Logger from 'reggol'

const PLATFORM = 'bba-local-l1'
const MAX_RESPONSE_BYTES = 64 * 1024
const MAX_OUTPUT_CHARS = 8_000

export interface LocalL1ProviderOptions {
  /**
   * OpenAI-compatible API base ending in /v1. Loopback is always allowed.
   * A non-loopback base requires the exact origin in `approvedEndpoint`.
   */
  baseUrl: string
  /**
   * Exact private HTTP origin explicitly approved for this provider instance,
   * for example `http://192.168.68.106:8166` (an optional `/v1` suffix is
   * accepted and ignored). Omitted/empty keeps the provider loopback-only.
   * Wildcards, hostnames, credentials, queries, fragments, paths and HTTPS are
   * rejected. This option is an operator approval, not a fallback or discovery
   * mechanism: a mismatched origin fails configuration.
   */
  approvedEndpoint?: string
  apiKey: string
  model: string
  revision: string
  quantization: string
  maxContextTokens: number
  maxOutputTokens: number
  requestTimeoutMs: number
  maxConcurrentRequests: number
  maxQueuedRequests: number
}

export class LocalL1Error extends Error {
  constructor(readonly code: 'invalid_config' | 'queue_full' | 'deadline_exceeded' | 'provider_error') {
    super(code)
    this.name = 'LocalL1Error'
  }
}

interface QueueEntry {
  deadlineAt: number
  signal?: AbortSignal
  resolve: (release: () => void) => void
  reject: (error: Error) => void
  abort?: () => void
  timer?: ReturnType<typeof setTimeout>
}

/** Shared FIFO admission control for every account plugin created by this provider. */
export class LocalL1Admission {
  private active = 0
  private readonly queue: QueueEntry[] = []

  constructor(
    private readonly maxConcurrentRequests: number,
    private readonly maxQueuedRequests: number
  ) {
    if (!Number.isInteger(maxConcurrentRequests) || maxConcurrentRequests < 1 || maxConcurrentRequests > 32) {
      throw new LocalL1Error('invalid_config')
    }
    if (!Number.isInteger(maxQueuedRequests) || maxQueuedRequests < 0 || maxQueuedRequests > 256) {
      throw new LocalL1Error('invalid_config')
    }
  }

  async run<T>(deadlineAt: number, signal: AbortSignal | undefined, operation: () => Promise<T>): Promise<T> {
    const release = await this.acquire(deadlineAt, signal)
    try {
      if (Date.now() >= deadlineAt || signal?.aborted) throw new LocalL1Error('deadline_exceeded')
      return await operation()
    } finally {
      release()
    }
  }

  private acquire(deadlineAt: number, signal?: AbortSignal): Promise<() => void> {
    const result = Promise.withResolvers<() => void>()
    if (Date.now() >= deadlineAt || signal?.aborted) {
      result.reject(new LocalL1Error('deadline_exceeded'))
      return result.promise
    }

    const entry: QueueEntry = { deadlineAt, signal, resolve: result.resolve, reject: result.reject }
    if (this.active < this.maxConcurrentRequests && this.queue.length === 0) {
      this.grant(entry)
      return result.promise
    }
    if (this.queue.length >= this.maxQueuedRequests) {
      result.reject(new LocalL1Error('queue_full'))
      return result.promise
    }

    entry.abort = () => {
      const index = this.queue.indexOf(entry)
      if (index < 0) return
      this.queue.splice(index, 1)
      clearTimeout(entry.timer)
      signal?.removeEventListener('abort', entry.abort!)
      result.reject(new LocalL1Error('deadline_exceeded'))
      this.pump()
    }
    signal?.addEventListener('abort', entry.abort, { once: true })
    this.queue.push(entry)
    const delay = Math.max(0, deadlineAt - Date.now())
    entry.timer = setTimeout(entry.abort, delay)
    if (signal?.aborted) entry.abort()
    return result.promise
  }

  private grant(entry: QueueEntry): void {
    clearTimeout(entry.timer)
    if (entry.deadlineAt <= Date.now() || entry.signal?.aborted) {
      entry.signal?.removeEventListener('abort', entry.abort!)
      entry.reject(new LocalL1Error('deadline_exceeded'))
      this.pump()
      return
    }
    entry.signal?.removeEventListener('abort', entry.abort!)
    this.active += 1
    let released = false
    entry.resolve(() => {
      if (released) return
      released = true
      this.active -= 1
      this.pump()
    })
  }

  private pump(): void {
    while (this.active < this.maxConcurrentRequests && this.queue.length > 0) {
      const entry = this.queue.shift()!
      this.grant(entry)
    }
  }
}

const LOOPBACK_HOSTS = ['127.0.0.1', '[::1]']

/** IP-literal check for an approval: private, unique-local or loopback only. */
function isPrivateIpLiteral(hostname: string): boolean {
  const address = hostname.startsWith('[') && hostname.endsWith(']') ? hostname.slice(1, -1) : hostname
  switch (isIP(address)) {
    case 4: {
      const [first = -1, second = -1] = address.split('.').map(Number)
      return first === 10
        || first === 127
        || (first === 172 && second >= 16 && second <= 31)
        || (first === 192 && second === 168)
    }
    case 6: {
      if (address === '::1') return true
      const firstGroup = Number.parseInt(address.split(':')[0] ?? '', 16)
      return Number.isInteger(firstGroup) && (firstGroup & 0xfe00) === 0xfc00
    }
    default:
      return false
  }
}

/**
 * Normalize the single explicit approval. Only an exact private HTTP origin is
 * accepted; an optional `/v1` suffix is allowed for operator ergonomics.
 */
function approvedOrigin(value: LocalL1ProviderOptions['approvedEndpoint']): string | null {
  if (value === undefined) return null
  if (typeof value !== 'string' || value.length === 0 || value.trim() !== value) {
    throw new LocalL1Error('invalid_config')
  }
  let url: URL
  try {
    url = new URL(value)
  } catch {
    throw new LocalL1Error('invalid_config')
  }
  const path = url.pathname.replace(/\/$/, '')
  if (
    url.protocol !== 'http:' ||
    url.username !== '' || url.password !== '' || url.search !== '' || url.hash !== '' ||
    (path !== '' && path !== '/v1') ||
    !isPrivateIpLiteral(url.hostname)
  ) {
    throw new LocalL1Error('invalid_config')
  }
  return url.origin
}

function validateOptions(options: LocalL1ProviderOptions): void {
  if (!options || typeof options !== 'object') throw new LocalL1Error('invalid_config')
  let base: URL
  try {
    base = new URL(options.baseUrl)
  } catch {
    throw new LocalL1Error('invalid_config')
  }
  const approved = approvedOrigin(options.approvedEndpoint)
  if (
    base.protocol !== 'http:' ||
    base.username !== '' || base.password !== '' || base.search !== '' || base.hash !== '' ||
    !base.pathname.replace(/\/$/, '').endsWith('/v1') ||
    (!LOOPBACK_HOSTS.includes(base.hostname) && (approved === null || approved !== base.origin))
  ) {
    throw new LocalL1Error('invalid_config')
  }
  if (!options.apiKey.trim() || !options.model.trim() || !options.revision.trim() || !options.quantization.trim()) {
    throw new LocalL1Error('invalid_config')
  }
  if (!Number.isInteger(options.maxContextTokens) || options.maxContextTokens < 1 || options.maxContextTokens > 1_000_000) {
    throw new LocalL1Error('invalid_config')
  }
  if (!Number.isInteger(options.maxOutputTokens) || options.maxOutputTokens < 1 || options.maxOutputTokens > 2048 || options.maxOutputTokens > options.maxContextTokens) {
    throw new LocalL1Error('invalid_config')
  }
  if (!Number.isInteger(options.requestTimeoutMs) || options.requestTimeoutMs < 100 || options.requestTimeoutMs > 120_000) {
    throw new LocalL1Error('invalid_config')
  }
}

function messageContent(message: BaseMessage): string {
  if (typeof message.content === 'string') return message.content
  if (!Array.isArray(message.content)) throw new LocalL1Error('provider_error')
  let text = ''
  for (const part of message.content) {
    if (!part || typeof part !== 'object' || part.type !== 'text' || typeof part.text !== 'string') {
      throw new LocalL1Error('provider_error')
    }
    text += part.text
  }
  return text
}

function roleOf(message: BaseMessage): 'system' | 'user' | 'assistant' {
  switch (message.getType()) {
    case 'system': return 'system'
    case 'human': return 'user'
    case 'ai': return 'assistant'
    default: throw new LocalL1Error('provider_error')
  }
}

async function readJsonBounded(response: Response): Promise<Record<string, unknown>> {
  const advertised = Number(response.headers.get('content-length') ?? 0)
  if (advertised > MAX_RESPONSE_BYTES || !response.body) {
    await response.body?.cancel()
    throw new LocalL1Error('provider_error')
  }
  const reader = response.body.getReader()
  const chunks: Uint8Array[] = []
  let size = 0
  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      size += value.byteLength
      if (size > MAX_RESPONSE_BYTES) throw new LocalL1Error('provider_error')
      chunks.push(value)
    }
  } catch (error) {
    await reader.cancel().catch(() => undefined)
    throw error
  } finally {
    reader.releaseLock()
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(Buffer.concat(chunks).toString('utf8'))
  } catch {
    throw new LocalL1Error('provider_error')
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new LocalL1Error('provider_error')
  return parsed as Record<string, unknown>
}

function integerField(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : undefined
}

class LocalL1Requester extends ModelRequester {
  readonly logger: Logger
  private readonly lifetime = new AbortController()
  constructor(
    ctx: Context,
    pool: ClientConfigPool,
    plugin: ChatLunaPlugin,
    private readonly options: LocalL1ProviderOptions,
    private readonly admission: LocalL1Admission
  ) {
    super(ctx, pool, plugin.config, plugin)
    this.logger = new Logger('bba/local-l1')
    ctx.on('dispose', () => this.lifetime.abort())
  }

  protected async *completionStreamInternal(params: ModelRequestParams): AsyncGenerator<ChatGenerationChunk> {
    if (params.tools?.length || Object.keys(params.overrideRequestParams ?? {}).length > 0) {
      throw new LocalL1Error('provider_error')
    }
    const deadlineAt = Date.now() + this.options.requestTimeoutMs
    const deadline = new AbortController()
    const timer = setTimeout(() => deadline.abort(), this.options.requestTimeoutMs)
    const signal = AbortSignal.any([deadline.signal, this.lifetime.signal, ...(params.signal ? [params.signal] : [])])
    try {
      const maxTokens = params.maxTokens === undefined || params.maxTokens < 1
        ? this.options.maxOutputTokens
        : Math.min(params.maxTokens, this.options.maxOutputTokens)
      const messages = params.input.map((message) => ({ role: roleOf(message), content: messageContent(message) }))
      const chunk = await this.admission.run(deadlineAt, signal, async () => {
        const response = await this.post('chat/completions', {
          model: this.options.model,
          messages,
          max_tokens: maxTokens,
          temperature: params.temperature,
          top_p: params.topP,
          stop: params.stop,
          stream: false
        }, { signal, redirect: 'error' })
        if (!response.ok) {
          await response.body?.cancel()
          throw new LocalL1Error('provider_error')
        }
        const payload = await readJsonBounded(response)
        const choices = payload.choices
        if (!Array.isArray(choices) || choices.length !== 1) throw new LocalL1Error('provider_error')
        const first = choices[0]
        if (!first || typeof first !== 'object') throw new LocalL1Error('provider_error')
        const choice = first as Record<string, unknown>
        const message = choice.message
        if (!message || typeof message !== 'object') throw new LocalL1Error('provider_error')
        const messageValue = message as Record<string, unknown>
        if (messageValue.tool_calls != null || messageValue.function_call != null) {
          throw new LocalL1Error('provider_error')
        }
        const content = messageValue.content
        if (typeof content !== 'string' || content.trim().length === 0 || content.length > MAX_OUTPUT_CHARS) {
          throw new LocalL1Error('provider_error')
        }
        const usage = payload.usage && typeof payload.usage === 'object'
          ? payload.usage as Record<string, unknown>
          : undefined
        const inputTokens = integerField(usage?.prompt_tokens)
        const outputTokens = integerField(usage?.completion_tokens)
        const totalTokens = integerField(usage?.total_tokens)
        return new ChatGenerationChunk({
          text: content,
          message: new AIMessageChunk({
            content,
            ...(inputTokens !== undefined && outputTokens !== undefined && totalTokens !== undefined
              ? { usage_metadata: { input_tokens: inputTokens, output_tokens: outputTokens, total_tokens: totalTokens } }
              : {})
          }),
          generationInfo: { finish_reason: choice.finish_reason }
        })
      })
      yield chunk
    } catch (error) {
      if (signal.aborted) throw new LocalL1Error('deadline_exceeded')
      if (error instanceof LocalL1Error) throw error
      throw new LocalL1Error('provider_error')
    } finally {
      clearTimeout(timer)
      deadline.abort()
    }
  }
}

class LocalL1Client extends PlatformModelClient {
  readonly platform = PLATFORM
  private readonly plugin: ChatLunaPlugin

  constructor(
    ctx: Context,
    pool: ClientConfigPool,
    plugin: ChatLunaPlugin,
    private readonly options: LocalL1ProviderOptions,
    private readonly admission: LocalL1Admission
  ) {
    super(ctx, pool)
    this.plugin = plugin
  }
  async refreshModels(): Promise<Array<{
    name: string
    type: ModelType
    maxTokens: number
    capabilities: ModelCapabilities[]
  }>> {
    return [{
      name: this.options.model,
      type: ModelType.llm,
      maxTokens: this.options.maxContextTokens,
      capabilities: [ModelCapabilities.TextInput]
    }]
  }

  protected _createModel(model: string): ChatLunaChatModel {
    const requester = new LocalL1Requester(
      this.ctx,
      this.configPool,
      this.plugin,
      this.options,
      this.admission
    )
    return new ChatLunaChatModel({
      model,
      modelInfo: {
        name: this.options.model,
        type: ModelType.llm,
        maxTokens: this.options.maxContextTokens,
        capabilities: [ModelCapabilities.TextInput]
      },
      modelMaxContextSize: this.options.maxContextTokens,
      maxRetries: 0,
      maxTokens: this.options.maxOutputTokens,
      requester,
      usageReporter: undefined
    })
  }


}
export interface LocalL1ProviderPlugin {
  name: string
  inject: string[]
  schema: false
  apply(ctx: Context): void
}

export function createLocalL1ProviderPlugin(options: LocalL1ProviderOptions): LocalL1ProviderPlugin {
  validateOptions(options)
  options = { ...options }
  const admission = new LocalL1Admission(options.maxConcurrentRequests, options.maxQueuedRequests)
  return {
    name: 'bba-local-l1-provider',
    inject: ['chatluna'],
    schema: false,
    apply(ctx): void {
      // Endpoint approval only changes reachability: retries and provider
      // fallback remain disabled for loopback and approved origins alike.
      const plugin = new ChatLunaPlugin(ctx, {
        chatConcurrentMaxSize: options.maxConcurrentRequests,
        chatTimeLimit: 100_000,
        configMode: 'default',
        maxRetries: 0,
        timeout: options.requestTimeoutMs,
        proxyMode: 'off',
        proxyAddress: ''
      }, PLATFORM, true)
      plugin.parseConfig(() => [{
        apiKey: options.apiKey,
        platform: PLATFORM,
        apiEndpoint: options.baseUrl.replace(/\/$/, ''),
        maxRetries: 0,
        concurrentMaxSize: options.maxConcurrentRequests,
        timeout: options.requestTimeoutMs,
        chatLimit: 100_000
      }])
      plugin.registerClient(() => new LocalL1Client(
        ctx,
        plugin.platformConfigPool,
        plugin,
        options,
        admission
      ))
      ctx.on('ready', () => plugin.initClient())
    }
  }
}
