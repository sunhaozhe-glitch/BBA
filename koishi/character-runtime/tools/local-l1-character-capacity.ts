#!/usr/bin/env node
/**
 * Bounded 1/4/8/20 real-path capacity and correlation harness.
 *
 * Unlike `tools/p018-local-capacity.py`, this runner does not call the model
 * provider directly. Each synthetic session starts one real
 * `koishi-plugin-chatluna-character` app on the real pinned ChatLuna package,
 * with the opt-in `bba-local-l1` provider pointed at an explicitly approved
 * private endpoint. The Character service generates a reply, the transport
 * guard issues/consumes a backend permit, and an in-process fake transport
 * records the delivered text. No QQ connection, production backend, deployed
 * service or credential file is used.
 *
 * Required environment (the key is read from the process environment only and
 * is never written to evidence):
 *   BBA_LOCAL_L1_URL       approved base URL ending in /v1
 *   BBA_LOCAL_L1_MODEL     API model id, e.g. swift-1.5-iq3_s
 *   BBA_LOCAL_L1_API_KEY   bearer key
 * Optional:
 *   BBA_LOCAL_L1_REVISION       defaults to "unattested"
 *   BBA_LOCAL_L1_QUANTIZATION   defaults to "unattested"
 *
 * Usage:
 *   BBA_LOCAL_L1_URL=http://192.168.68.106:8166/v1 \
 *   BBA_LOCAL_L1_MODEL=swift-1.5-iq3_s \
 *   BBA_LOCAL_L1_API_KEY=... \
 *   node --import tsx tools/local-l1-character-capacity.ts \
 *     --output evals/results/<run> [--levels 1,4,8,20] [--deadline-seconds 30]
 *
 * Exit codes: 0 all sessions completed, 1 some session failed, 2 invalid
 * configuration or output path.
 */
import { createHash } from 'node:crypto'
import { mkdir, mkdtemp, open, rm, writeFile } from 'node:fs/promises'
import type { FileHandle } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import type { Bot, Session } from '../src/koishi.js'
import { createLocalL1ProviderPlugin } from '../src/local-l1.js'
import type { LocalL1ProviderOptions, LocalL1ProviderPlugin } from '../src/local-l1.js'
import { createCharacterAccountRuntimeManager } from '../src/runtime.js'
import type { CharacterAccountRuntimeManager } from '../src/runtime.js'
import type { CharacterAccountOptions } from '../src/types.js'
import { startFakeBackend } from '../test/harness/fake-backend.js'
import type { FakeBackendState } from '../test/harness/fake-backend.js'
import { createFakeTransportPlugin } from '../test/harness/fake-transport.js'
import type { SentRecord } from '../test/harness/fake-transport.js'

const PROMPT_VERSION = 'p016-p018-character-path-capacity.v1'
const DEFAULT_LEVELS = [1, 4, 8, 20]
const MAX_LEVEL = 20
const GROUP_ID = '90001'
const USER_ID = '90002'
const FIXTURE_RUNTIME_TOKEN = 'test-runtime-token'
const STARTUP_TIMEOUT_MS = 120_000
const TRIGGER_GRACE_MS = 15_000

interface Configuration {
  baseUrl: string
  approvedEndpoint: string | undefined
  model: string
  apiKey: string
  revision: string
  quantization: string
  output: string
  levels: number[]
  deadlineMs: number
}

interface CharacterService {
  triggerMessage(session: Session, message: unknown, reason: string): Promise<boolean>
}

interface CharacterServices {
  chatluna_character?: CharacterService
}

interface RunningSession {
  index: number
  marker: string
  text: string
  backend: FakeBackendState
  manager: CharacterAccountRuntimeManager
  runtimeDirectory: string
  sink: SentRecord[]
  character?: CharacterService
  session?: Session
  startError: string | null
}

interface SessionRecord {
  session: string
  workload: 'group_chat'
  evidence_level: 'live_synthetic'
  prompt_version: string
  input_sha256: string
  status: 'completed' | 'failed'
  character_path: 'koishi-plugin-chatluna-character'
  requested_model: string
  returned_model: null
  delivered_segments: number | null
  session_marker_matches: boolean | null
  permits_consumed: number | null
  latency_ms: number | null
  error: string | null
}

interface LevelSummary {
  sessions: number
  requests: number
  completed: number
  timeouts: number
  correlation_mismatch: number
  duplicate_delivery: number
  no_delivery: number
  character_path_errors: number
  output_unvalidated: number
  all_request_p95_ms: number | null
  startup_seconds: number
  burst_wall_seconds: number
  wall_seconds: number
  client_cpu_seconds: number
  client_rss_kib: number | null
  server_cpu: null
  server_rss: null
  gpu_vram: null
  gateway_queue_wait_ms: null
  provider_max_retries: 0
  provider_fallback: false
}

const SILENT_LOGGER = {
  debug(): void {},
  info(): void {},
  warn(): void {},
  error(): void {}
}

function fail(code: string): never {
  console.log(JSON.stringify({ error: code }))
  process.exit(2)
}

function option(name: string): string | undefined {
  const index = process.argv.indexOf(`--${name}`)
  if (index < 0) return undefined
  const value = process.argv[index + 1]
  if (!value || value.startsWith('--')) return fail(`missing_${name.replace(/-/g, '_')}`)
  return value
}

function requiredEnvironment(name: string): string {
  const value = process.env[name]
  if (typeof value !== 'string' || value.length === 0) return fail(`missing_environment_${name.toLowerCase()}`)
  return value
}

function parseLevels(raw: string | undefined): number[] {
  const levels = (raw ?? DEFAULT_LEVELS.join(','))
    .split(',')
    .map((entry) => Number(entry.trim()))
  if (
    levels.length === 0 ||
    levels.some((level) => !Number.isInteger(level) || level < 1 || level > MAX_LEVEL)
  ) {
    return fail('invalid_levels')
  }
  return [...new Set(levels)]
}

function loadConfiguration(): Configuration {
  const baseUrl = requiredEnvironment('BBA_LOCAL_L1_URL')
  const model = requiredEnvironment('BBA_LOCAL_L1_MODEL')
  const apiKey = requiredEnvironment('BBA_LOCAL_L1_API_KEY')
  if (apiKey.trim().length === 0 || apiKey.trim() !== apiKey || /[\r\n]/.test(apiKey)) {
    return fail('invalid_api_key')
  }
  if (model.trim().length === 0 || model.trim() !== model) return fail('invalid_model')

  let url: URL
  try {
    url = new URL(baseUrl)
  } catch {
    return fail('invalid_endpoint')
  }
  if (
    url.protocol !== 'http:' ||
    url.username !== '' || url.password !== '' || url.search !== '' || url.hash !== '' ||
    !url.pathname.replace(/\/$/, '').endsWith('/v1')
  ) {
    return fail('invalid_endpoint')
  }

  const deadlineSeconds = Number(option('deadline-seconds') ?? 30)
  if (!Number.isFinite(deadlineSeconds) || deadlineSeconds < 1 || deadlineSeconds > 120) {
    return fail('invalid_deadline')
  }
  const output = option('output')
  if (!output) return fail('missing_output')

  const loopback = ['127.0.0.1', '[::1]'].includes(url.hostname)
  return {
    baseUrl: url.href,
    // The operator supplies the exact URL as the approval. Loopback stays
    // usable without any approval, preserving the provider's default.
    approvedEndpoint: loopback ? undefined : url.origin,
    model,
    apiKey,
    revision: process.env.BBA_LOCAL_L1_REVISION || 'unattested',
    quantization: process.env.BBA_LOCAL_L1_QUANTIZATION || 'unattested',
    output: resolve(output),
    levels: parseLevels(option('levels')),
    deadlineMs: Math.round(deadlineSeconds * 1000)
  }
}

function markerFor(level: number, index: number): string {
  return `cap-L${level}-S${index}-${createHash('sha256')
    .update(`${level}:${index}:${Date.now()}:${process.pid}`)
    .digest('hex')
    .slice(0, 12)}`
}

function probeText(marker: string): string {
  return `Synthetic capacity probe. marker=${marker}. `
    + `Reply with exactly one short line and nothing else: <message>ACK=${marker}</message>`
}

function personaFor(level: number, index: number, marker: string): string {
  return [
    'You are a synthetic capacity probe persona, not a real conversation.',
    `This runtime belongs to synthetic session ${level}/${index}.`,
    `When the user message contains marker=${marker}, reply with exactly one short line:`,
    `<message>ACK=${marker}</message>`,
    'Never call tools. Never add explanations or additional messages.'
  ].join('\n')
}

function errorCode(error: unknown): string {
  let current: unknown = error
  for (let depth = 0; depth < 4 && current && typeof current === 'object'; depth += 1) {
    if ('code' in current && typeof current.code === 'string' && current.code.length > 0) {
      return current.code
    }
    // Locally raised timeouts use an Error whose message is the code.
    if (current instanceof Error && /^[a-z][a-z0-9_]{0,63}$/.test(current.message)) {
      return current.message
    }
    if (!('cause' in current)) break
    current = current.cause
  }
  return 'character_path_error'
}

function withTimeout<T>(promise: Promise<T>, timeoutMs: number, code: string): Promise<T> {
  const gate = Promise.withResolvers<never>()
  const timer = setTimeout(() => gate.reject(new Error(code)), timeoutMs)
  return Promise.race([promise, gate.promise]).finally(() => clearTimeout(timer))
}

async function waitForOpenTrigger(
  engine: CharacterAccountRuntimeManager,
  accountId: string,
  conversationId: string,
  timeoutMs: number
): Promise<void> {
  const runtime = engine.runtimes.get(accountId)
  if (!runtime) throw new Error('runtime_missing')
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const snapshot = await runtime.gate.snapshot()
    if (
      Object.values(snapshot.triggers).some(
        (trigger) =>
          trigger.state === 'open' &&
          trigger.kind === 'group' &&
          trigger.conversation_id === conversationId
      )
    ) {
      return
    }
    await new Promise((resolveDelay) => setTimeout(resolveDelay, 10))
  }
  throw new Error('trigger_open_timeout')
}

function probeSession(bot: Bot, text: string, marker: string): Session {
  const session = bot.session({
    type: 'message',
    timestamp: Date.now(),
    guild: { id: GROUP_ID },
    channel: { id: GROUP_ID, type: 0 },
    user: { id: USER_ID, name: 'capacity-probe' },
    message: { id: `capacity-${marker}` }
  } as never) as unknown as Session
  session.content = text
  return session
}

const CHARACTER_CONFIG: Record<string, unknown> = {
  globalPrivateConfig: {
    enableFixedIntervalTrigger: false,
    enableActivityScoreTrigger: false,
    idleTrigger: { enableLongWaitTrigger: false },
    toolCalling: false,
    messageWaitTime: 0,
    typingTime: 100
  },
  globalGroupConfig: {
    enableFixedIntervalTrigger: false,
    enableActivityScoreTrigger: false,
    idleTrigger: { enableLongWaitTrigger: false },
    toolCalling: false,
    messageWaitTime: 0,
    typingTime: 100
  }
}

async function createSession(
  level: number,
  index: number,
  config: Configuration,
  providerPlugin: LocalL1ProviderPlugin,
  stateRoot: string
): Promise<RunningSession> {
  const marker = markerFor(level, index)
  const text = probeText(marker)
  const qqSelfId = String(700_000 + index)
  const accountId = `acct-cap-${level}-${index}`
  const accountStateDir = await mkdtemp(join(stateRoot, `session-${level}-${index}-`))
  const backend = await startFakeBackend({ accountId, qqSelfId })
  const sink: SentRecord[] = []
  const account: CharacterAccountOptions = {
    accountId,
    qqSelfId,
    runtimeId: `runtime-cap-${level}-${index}`,
    characterRuntimeId: `character-runtime-cap-${level}-${index}`,
    characterDbId: `character-db-cap-${level}-${index}`,
    characterCacheNamespace: `character-cache-cap-${level}-${index}`,
    personaId: `CAPACITY_L${level}_S${index}`,
    persona: personaFor(level, index, marker),
    runtimeToken: FIXTURE_RUNTIME_TOKEN,
    transportPlugin: createFakeTransportPlugin({ selfId: qqSelfId, sink }),
    transportConfig: { selfId: qqSelfId },
    characterModel: `bba-local-l1/${config.model}`,
    characterConfig: CHARACTER_CONFIG,
    // Sessions are opened sequentially before the burst; keep every trigger
    // valid long enough that startup order cannot masquerade as a timeout.
    triggerTtlMs: 10 * 60_000
  }
  const manager = createCharacterAccountRuntimeManager({
    stateDir: accountStateDir,
    backendBaseUrl: backend.baseUrl,
    accounts: [account],
    startApps: true,
    providerPlugins: [{ plugin: providerPlugin }],
    characterConfig: CHARACTER_CONFIG,
    requestTimeoutMs: 15_000,
    logger: SILENT_LOGGER
  })

  const running: RunningSession = {
    index,
    marker,
    text,
    backend,
    manager,
    runtimeDirectory: accountStateDir,
    sink,
    startError: null
  }

  try {
    await withTimeout(manager.start(), STARTUP_TIMEOUT_MS, 'runtime_start_timeout')
    const runtime = manager.runtimes.get(accountId)
    if (!runtime) throw new Error('runtime_missing')
    if (!runtime.bot) throw new Error('runtime_bot_missing')
    // The App exposes injected services under their plugin names; Character is
    // loaded at runtime, so the concrete service type lives in this harness.
    const services = runtime.app as unknown as CharacterServices
    const character = services.chatluna_character
    if (!character) throw new Error('character_service_missing')

    running.character = character
    const session = probeSession(runtime.bot, text, marker)
    running.session = session

    runtime.bot.dispatch(session)
    await withTimeout(
      backend.waitForRequest('/v1/internal/chat-triggers'),
      STARTUP_TIMEOUT_MS,
      'trigger_request_timeout'
    )
    await withTimeout(
      waitForOpenTrigger(manager, accountId, GROUP_ID, STARTUP_TIMEOUT_MS),
      STARTUP_TIMEOUT_MS,
      'trigger_open_timeout'
    )
  } catch (error) {
    running.startError = errorCode(error)
  }
  return running
}

async function runProbe(
  running: RunningSession,
  config: Configuration,
  level: number
): Promise<SessionRecord> {
  const record: SessionRecord = {
    session: `p016_${level}_${running.index}`,
    workload: 'group_chat',
    evidence_level: 'live_synthetic',
    prompt_version: PROMPT_VERSION,
    input_sha256: createHash('sha256').update(running.text).digest('hex'),
    status: 'failed',
    character_path: 'koishi-plugin-chatluna-character',
    requested_model: config.model,
    returned_model: null,
    delivered_segments: null,
    session_marker_matches: null,
    permits_consumed: null,
    latency_ms: null,
    error: running.startError
  }
  if (running.startError) return record
  const character = running.character
  const session = running.session
  if (!character || !session) {
    record.error = 'runtime_not_ready'
    return record
  }

  const started = performance.now()
  try {
    const accepted = await withTimeout(
      character.triggerMessage(
        session,
        {
          content: running.text,
          name: 'capacity-probe',
          id: USER_ID,
          messageId: `capacity-${running.marker}`,
          timestamp: Date.now()
        },
        'synthetic capacity probe'
      ),
      config.deadlineMs + TRIGGER_GRACE_MS,
      'character_path_deadline_exceeded'
    )
    if (!accepted) record.error = 'character_declined'
  } catch (error) {
    record.error = errorCode(error)
  }

  record.latency_ms = Math.round((performance.now() - started) * 1000) / 1000
  record.delivered_segments = running.sink.length
  record.session_marker_matches = running.sink.length === 0
    ? null
    : running.sink.every((delivery) => delivery.text.includes(running.marker))
  record.permits_consumed = running.backend.consumeCount

  if (record.error === 'character_path_deadline_exceeded') record.error = 'deadline_exceeded'
  if (record.error === null && running.sink.length === 0) record.error = 'no_delivery'
  // A Character-internal wrapper can swallow the provider timeout code. If the
  // provider deadline elapsed with nothing delivered, classify it as a timeout.
  if (
    record.error === 'no_delivery' &&
    record.latency_ms !== null &&
    record.latency_ms >= config.deadlineMs
  ) {
    record.error = 'deadline_exceeded'
  }
  if (record.error === null && running.sink.length > 1) record.error = 'duplicate_delivery'
  if (record.error === null && record.session_marker_matches !== true) {
    record.error = 'correlation_mismatch'
  }
  if (record.error === null && running.backend.consumeCount !== 1) {
    record.error = 'permit_accounting_mismatch'
  }
  record.status = record.error === null ? 'completed' : 'failed'
  return record
}

function p95(values: number[]): number | null {
  if (values.length === 0) return null
  const sorted = [...values].sort((left, right) => left - right)
  return sorted[Math.ceil(sorted.length * 0.95) - 1] ?? null
}

async function runLevel(
  level: number,
  config: Configuration,
  stateRoot: string,
  output: FileHandle
): Promise<LevelSummary> {
  const started = performance.now()
  const cpuBefore = process.cpuUsage()
  const providerOptions: LocalL1ProviderOptions = {
    baseUrl: config.baseUrl,
    ...(config.approvedEndpoint ? { approvedEndpoint: config.approvedEndpoint } : {}),
    apiKey: config.apiKey,
    model: config.model,
    revision: config.revision,
    quantization: config.quantization,
    maxContextTokens: 32_768,
    maxOutputTokens: 512,
    requestTimeoutMs: config.deadlineMs,
    maxConcurrentRequests: level,
    maxQueuedRequests: level
  }
  // One shared admission object per level because every session receives the
  // same opt-in provider plugin instance.
  const providerPlugin = createLocalL1ProviderPlugin(providerOptions)
  const sessions: RunningSession[] = []
  const records: SessionRecord[] = []
  let startupSeconds = 0
  let burstSeconds = 0
  try {
    for (let index = 0; index < level; index += 1) {
      sessions.push(await createSession(level, index, config, providerPlugin, stateRoot))
    }
    startupSeconds = Math.round((performance.now() - started) * 1000) / 1000
    const burstStarted = performance.now()
    const settled = await Promise.all(sessions.map((running) => runProbe(running, config, level)))
    burstSeconds = Math.round((performance.now() - burstStarted) * 1000) / 1000
    records.push(...settled)
    for (const record of records) {
      await output.write(`${JSON.stringify(record)}\n`)
    }
  } finally {
    await Promise.allSettled(sessions.map(async (running) => {
      try {
        await withTimeout(running.manager.stop(), 30_000, 'runtime_stop_timeout')
      } catch {
        // The known ChatLuna teardown exception must not hide the measurements.
      } finally {
        await running.backend.close()
        await rm(running.runtimeDirectory, { recursive: true, force: true })
      }
    }))
  }

  const cpuAfter = process.cpuUsage()
  const cpuSeconds = (cpuAfter.user + cpuAfter.system - cpuBefore.user - cpuBefore.system) / 1_000_000
  const latencies = records
    .map((record) => record.latency_ms)
    .filter((latency): latency is number => latency !== null)
  return {
    sessions: level,
    requests: records.length,
    completed: records.filter((record) => record.status === 'completed').length,
    timeouts: records.filter((record) => record.error === 'deadline_exceeded').length,
    correlation_mismatch: records.filter((record) => record.error === 'correlation_mismatch').length,
    duplicate_delivery: records.filter((record) => record.error === 'duplicate_delivery').length,
    no_delivery: records.filter((record) => record.error === 'no_delivery').length,
    character_path_errors: records.filter(
      (record) =>
        record.error !== null &&
        !['deadline_exceeded', 'correlation_mismatch', 'duplicate_delivery', 'no_delivery'].includes(record.error)
    ).length,
    output_unvalidated: records.filter((record) => record.session_marker_matches !== true).length,
    all_request_p95_ms: p95(latencies),
    startup_seconds: startupSeconds,
    burst_wall_seconds: burstSeconds,
    wall_seconds: Math.round((performance.now() - started) * 1000) / 1000,
    client_cpu_seconds: Math.round(cpuSeconds * 1_000_000) / 1_000_000,
    client_rss_kib: Math.round(process.memoryUsage().rss / 1024),
    server_cpu: null,
    server_rss: null,
    gpu_vram: null,
    gateway_queue_wait_ms: null,
    provider_max_retries: 0,
    provider_fallback: false
  }
}

async function main(): Promise<number> {
  const config = loadConfiguration()
  const sessionsFile = join(config.output, 'sessions.jsonl')
  await mkdir(config.output, { recursive: true }).catch(() => fail('output_unavailable'))
  const output = await open(sessionsFile, 'wx').catch((error: NodeJS.ErrnoException) => {
    fail(error.code === 'EEXIST' ? 'output_exists' : 'output_unavailable')
  })
  try {
    const stateRoot = await mkdtemp(join(tmpdir(), 'bba-character-capacity-'))
    const levels: LevelSummary[] = []
    let allCompleted = true
    try {
      for (const level of config.levels) {
        const summary = await runLevel(level, config, stateRoot, output)
        levels.push(summary)
        allCompleted &&= summary.completed === summary.sessions
        console.log(JSON.stringify(summary))
      }
    } finally {
      await rm(stateRoot, { recursive: true, force: true })
    }

    const report = {
      evidence_level: 'live_synthetic',
      model: config.model,
      returned_model: null,
      prompt_version: PROMPT_VERSION,
      provider_only: false,
      character_runtime_exercised: true,
      chatluna_path: true,
      golden_holdout_exercised: false,
      qq_calls: 0,
      model_revision: config.revision === 'unattested' ? null : config.revision,
      weights_hash: null,
      chat_template_revision: null,
      approved_private_endpoint: config.approvedEndpoint !== undefined,
      deadline_seconds: config.deadlineMs / 1000,
      levels,
      limitations: [
        'One burst per level; not an SLO or a stability claim.',
        'Synthetic marker correlation only; no golden holdout, quality score or human review.',
        'No QQ, production backend, deployed service or credential file was accessed; backend/transport are in-process fixtures.',
        'Server CPU, server RSS, GPU/VRAM and model-server/Gateway queue wait remain null.',
        'Character can re-prompt once when model XML is unparseable; that extra provider call is included in latency and not counted separately.',
        'TTFT and token usage are not exposed through the Character path and remain null.'
      ]
    }
    await writeFile(join(config.output, 'summary.json'), `${JSON.stringify(report, null, 2)}\n`, { flag: 'wx' })
    return allCompleted ? 0 : 1
  } finally {
    await output.close()
  }
}

main().then(
  (code) => process.exit(code),
  () => fail('harness_failed')
)
