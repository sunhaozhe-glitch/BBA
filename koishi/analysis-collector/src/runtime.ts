import { createHash, randomBytes } from 'node:crypto'
import { createReadStream } from 'node:fs'
import { mkdir, open, readFile, rm, writeFile } from 'node:fs/promises'
import { basename, join } from 'node:path'
import { App as KoishiApp } from 'koishi'
import type { App, Bot, Context, Database } from 'koishi'
import { createDurableFileDatabase } from 'bba-character-runtime'
import { installCollectorIngress, installOutboundDenial } from './boundary.js'
import type { BoundaryRecord } from './boundary.js'
import {
  buildChatLunaConfig,
  buildGroupAnalysisConfig,
  createBaseDiagnostics,
  REQUIRED_SERVICES,
  resolveCollectorOptions,
  SERVICE_ORDER
} from './config.js'
import type { ResolvedCollectorOptions } from './config.js'
import { CollectorBoundaryError, CollectorError } from './errors.js'
import type {
  AnalysisCollectorOptions,
  AnalysisCollectorRuntime,
  CollectedMessage,
  CollectedMessageCursor,
  CollectedMessageFilter,
  CollectedMessageWindow,
  CollectedMessageWindowOptions,
  CollectorDiagnostics,
  CollectorListenerGroup,
  CollectorServiceName,
  WaitForCollectedMessagesOptions,
  GroupOwnerClaim,
  GroupOwnerLease,
  StoredSummary,
  SummaryV1
} from './types.js'
import { PINNED_VERSIONS } from './types.js'
import type { SemanticAnalysis } from './semantic-analysis.js'

const DEFAULT_STOP_TIMEOUT_MS = 5_000
const MAX_BOUNDARY_VIOLATIONS = 100

interface StateLock {
  path: string
  token: string
  release(): Promise<void>
}

interface LockFileBody {
  pid: number
  token: string
  selfId: string
  platform: string
  startedAt: string
}

interface BindingFileBody {
  version: 1
  selfId: string
  platform: string
  createdAt: string
}

interface RuntimeParts {
  app: App
  database: Database
  diagnostics: CollectorDiagnostics
  options: ResolvedCollectorOptions
  lock: StateLock
  ingressDispose: () => void
  botUninstallers: Map<string, () => void>
  record: BoundaryRecord
  sourceManifestSha256: string
  logger?: ResolvedCollectorOptions['logger']
}


interface CollectorModelRegistrar {
  model: {
    extend(name: string, fields: Record<string, string>, config: { primary: string; unique?: string[][] }): void
  }
}

interface CollectorDatabase {
  get(table: string, query: Record<string, unknown>, cursor?: Record<string, unknown>): Promise<Array<Record<string, unknown>>>
  create(table: string, data: Record<string, unknown>): Promise<unknown>
  set(table: string, query: Record<string, unknown>, update: Record<string, unknown>): Promise<{ matched?: number }>
  upsert(table: string, data: Record<string, unknown>[], keys?: string | string[]): Promise<unknown>
  withTransaction(callback: (database: CollectorDatabase) => Promise<unknown>): Promise<unknown>
}

function registerCollectorModels(app: App): void {
  const model = (app as unknown as CollectorModelRegistrar).model
  model.extend('bba_group_owner', {
    id: 'string', platform: 'string', guildId: 'string', channelId: 'string',
    accountId: 'string', ownerSelfId: 'string', ownerToken: 'string', ownerEpoch: 'integer', leaseExpiresAt: 'timestamp'
  }, { primary: 'id' })
  model.extend('bba_collector_window', {
    id: 'string', platform: 'string', selfId: 'string', accountId: 'string', ownerEpoch: 'integer',
    guildId: 'string', channelId: 'string', startTime: 'timestamp', endTime: 'timestamp',
    cursorTimestamp: 'timestamp', cursorId: 'string', messageRefs: 'json', messageCount: 'integer',
    speakerCounts: 'json', complete: 'boolean', updatedAt: 'timestamp'
  }, { primary: 'id' })
  model.extend('bba_summary', {
    id: 'string', windowId: 'string', ownerEpoch: 'integer', accountId: 'string', guildId: 'string',
    channelId: 'string', createdAt: 'timestamp', expiresAt: 'timestamp', summaryJson: 'json'
  }, { primary: 'id' })
  model.extend('bba_semantic_analysis', {
    id: 'string', windowId: 'string', accountId: 'string', guildId: 'string',
    generatedAt: 'timestamp', expiresAt: 'timestamp', schemaId: 'string',
    evidenceSource: 'string', provider: 'string', analysisJson: 'json'
  }, { primary: 'id' })
}
async function sha256File(path: string): Promise<string> {
  const hash = createHash('sha256')
  for await (const chunk of createReadStream(path)) hash.update(chunk)
  return hash.digest('hex')
}
const activeStateDirs = new Set<string>()

function delay(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>()
  setTimeout(resolve, ms)
  return promise
}

function isProcessAlive(pid: number): boolean {
  if (!Number.isInteger(pid) || pid <= 0) return false
  try {
    process.kill(pid, 0)
    return true
  } catch (error) {
    const nodeError = error as NodeJS.ErrnoException
    return nodeError.code === 'EPERM'
  }
}

async function readJsonFile(path: string): Promise<unknown> {
  try {
    return JSON.parse(await readFile(path, 'utf8')) as unknown
  } catch (error) {
    const nodeError = error as NodeJS.ErrnoException
    if (nodeError.code === 'ENOENT') return undefined
    throw error
  }
}

async function acquireStateLock(
  options: ResolvedCollectorOptions
): Promise<StateLock> {
  if (activeStateDirs.has(options.stateDir)) {
    throw new CollectorError(
      'state_dir_in_use',
      `stateDir ${options.stateDir} is already active in this process`
    )
  }
  await mkdir(options.stateDir, { recursive: true, mode: 0o700 })
  const lockPath = join(options.stateDir, 'collector.lock')
  const token = randomBytes(16).toString('hex')
  const body: LockFileBody = {
    pid: process.pid,
    token,
    selfId: options.selfId,
    platform: options.platform,
    startedAt: new Date().toISOString()
  }

  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      const handle = await open(lockPath, 'wx', 0o600)
      try {
        await handle.writeFile(JSON.stringify(body), 'utf8')
        await handle.sync()
      } finally {
        await handle.close()
      }
      activeStateDirs.add(options.stateDir)
      return {
        path: lockPath,
        token,
        async release(): Promise<void> {
          try {
            const parsed = (await readJsonFile(lockPath)) as Partial<LockFileBody> | undefined
            if (parsed?.token === token) await rm(lockPath, { force: true })
          } catch (error) {
            options.logger?.warn?.('cannot release analysis collector state lock', error)
          } finally {
            activeStateDirs.delete(options.stateDir)
          }
        }
      }
    } catch (error) {
      const nodeError = error as NodeJS.ErrnoException
      if (nodeError.code !== 'EEXIST') {
        throw new CollectorError(
          'state_lock_failed',
          `cannot create state lock ${lockPath}`,
          { cause: error }
        )
      }
      let existing: Partial<LockFileBody> | undefined
      try {
        existing = (await readJsonFile(lockPath)) as Partial<LockFileBody> | undefined
      } catch (readError) {
        throw new CollectorError(
          'state_dir_in_use',
          `stateDir ${options.stateDir} has an unreadable active lock`,
          { cause: readError }
        )
      }
      if (existing && typeof existing.pid === 'number' && isProcessAlive(existing.pid)) {
        throw new CollectorError(
          'state_dir_in_use',
          `stateDir ${options.stateDir} is locked by live pid ${existing.pid} (selfId=${String(existing.selfId)})`
        )
      }
      if (existing === undefined) {
        throw new CollectorError(
          'state_dir_in_use',
          `stateDir ${options.stateDir} has a lock that appeared concurrently`
        )
      }
      // Stale lock from a dead process: remove and retry once.
      await rm(lockPath, { force: true })
    }
  }
  throw new CollectorError('state_lock_failed', `cannot acquire state lock ${lockPath}`)
}

async function ensureStateBinding(options: ResolvedCollectorOptions): Promise<void> {
  const bindingPath = join(options.stateDir, 'binding.json')
  let existing: unknown
  try {
    existing = await readJsonFile(bindingPath)
  } catch (error) {
    throw new CollectorError(
      'state_binding_conflict',
      `stateDir ${options.stateDir} has an unreadable binding file`,
      { cause: error }
    )
  }
  if (existing !== undefined) {
    if (!existing || typeof existing !== 'object') {
      throw new CollectorError(
        'state_binding_conflict',
        `stateDir ${options.stateDir} has an invalid binding file`
      )
    }
    const record = existing as Partial<BindingFileBody>
    if (record.selfId !== options.selfId || record.platform !== options.platform) {
      throw new CollectorError(
        'state_binding_conflict',
        `stateDir ${options.stateDir} is bound to selfId=${String(record.selfId)} platform=${String(record.platform)}; requested selfId=${options.selfId} platform=${options.platform}`
      )
    }
    return
  }
  const binding: BindingFileBody = {
    version: 1,
    selfId: options.selfId,
    platform: options.platform,
    createdAt: new Date().toISOString()
  }
  await writeFile(bindingPath, JSON.stringify(binding), { encoding: 'utf8', mode: 0o600 })
}

function readContextProperty(ctx: Context, property: string): unknown {
  // Bound service getters may throw while the App is disposing. A missing
  // service is an expected diagnostic, not a crash.
  const record = ctx as unknown as Record<string, unknown>
  return record[property]
}

function observeServices(
  ctx: Context,
  diagnostics: CollectorDiagnostics,
  startedAtMs: number
): void {
  const missing: CollectorServiceName[] = []
  for (const name of SERVICE_ORDER) {
    const meta = REQUIRED_SERVICES[name]
    let injected = false
    let detail: string | undefined
    try {
      const value = readContextProperty(ctx, meta.property)
      injected = value !== undefined && value !== null
    } catch (error) {
      detail = error instanceof Error ? error.message : String(error)
    }
    diagnostics.services[name] = {
      name,
      required: true,
      property: meta.property,
      injected,
      status: injected ? 'available' : 'missing',
      plugin: meta.plugin,
      waitedMs: Date.now() - startedAtMs,
      ...(detail ? { detail } : {})
    }
    if (!injected) missing.push(name)
  }
  diagnostics.injection.missing = missing
  diagnostics.injection.complete = missing.length === 0
  diagnostics.injection.waitedMs = Date.now() - startedAtMs
}

async function waitForServices(
  ctx: Context,
  diagnostics: CollectorDiagnostics,
  options: ResolvedCollectorOptions,
  startedAtMs: number,
  deadline: number
): Promise<void> {
  for (;;) {
    observeServices(ctx, diagnostics, startedAtMs)
    if (diagnostics.injection.complete) return
    const remaining = deadline - Date.now()
    if (remaining <= 0) {
      const missing = diagnostics.injection.missing
      throw new CollectorError(
        'service_injection_timeout',
        `missing required service injection after ${diagnostics.injection.waitedMs}ms (timeout=${options.startupTimeoutMs}ms): ${missing.join(', ')}`,
        {
          diagnostics,
          details: {
            missing,
            timeoutMs: options.startupTimeoutMs,
            waitedMs: diagnostics.injection.waitedMs
          }
        }
      )
    }
    await delay(Math.min(options.pollIntervalMs, remaining))
  }
}

function createBoundaryRecorder(diagnostics: CollectorDiagnostics): BoundaryRecord {
  let sequence = 0
  return (code: string, operation: string, detail: string): void => {
    sequence++
    const { boundary } = diagnostics
    if (code === 'upstream_command_denied') boundary.commandsDenied++
    if (code === 'outbound_denied') boundary.outboundDenied++
    if (boundary.violations.length >= MAX_BOUNDARY_VIOLATIONS) {
      boundary.droppedViolations++
      return
    }
    boundary.violations.push({
      seq: sequence,
      code,
      operation,
      detail,
      at: new Date().toISOString()
    })
  }
}

interface DriverLike {
  stop?(): Promise<void> | void
}

interface AppModelView {
  model?: { drivers?: Iterable<unknown> }
}

function snapshotDrivers(app: App): DriverLike[] {
  const view = app as unknown as AppModelView
  const drivers = view.model?.drivers
  if (!drivers) return []
  const result: DriverLike[] = []
  for (const driver of drivers) {
    if (driver && typeof driver === 'object') result.push(driver as DriverLike)
  }
  return result
}

async function stopWithTimeout(
  task: Promise<unknown>,
  timeoutMs: number,
  label: string,
  logger?: ResolvedCollectorOptions['logger']
): Promise<void> {
  const timeout = Promise.withResolvers<void>()
  const timer = setTimeout(timeout.resolve, timeoutMs)
  try {
    await Promise.race([task, timeout.promise])
  } catch (error) {
    logger?.warn?.(`${label} failed during collector stop`, error)
  } finally {
    clearTimeout(timer)
  }
}

async function cleanupApp(
  app: App | undefined,
  ingressDispose: (() => void) | undefined,
  botUninstallers: Map<string, () => void> | undefined,
  logger?: ResolvedCollectorOptions['logger']
): Promise<void> {
  try {
    ingressDispose?.()
  } catch (error) {
    logger?.warn?.('cannot dispose collector ingress', error)
  }
  if (botUninstallers) {
    for (const uninstall of botUninstallers.values()) {
      try {
        uninstall()
      } catch (error) {
        logger?.warn?.('cannot restore bot outbound methods', error)
      }
    }
    botUninstallers.clear()
  }
  if (!app) return
  const drivers = snapshotDrivers(app)
  await stopWithTimeout(app.stop(), DEFAULT_STOP_TIMEOUT_MS, 'app.stop', logger)
  for (const driver of drivers) {
    const stopDriver = driver.stop
    if (typeof stopDriver !== 'function') continue
    try {
      await stopWithTimeout(
        Promise.resolve(stopDriver.call(driver)),
        DEFAULT_STOP_TIMEOUT_MS,
        'database driver stop',
        logger
      )
    } catch (error) {
      logger?.warn?.('cannot stop collector database driver', error)
    }
  }
}

function rowToMessage(row: unknown): CollectedMessage | null {
  if (!row || typeof row !== 'object') return null
  const record = row as Record<string, unknown>
  if (typeof record.id !== 'string') return null
  const timestamp = record.timestamp instanceof Date
    ? record.timestamp
    : new Date(typeof record.timestamp === 'string' ? record.timestamp : String(record.timestamp))
  if (!Number.isFinite(timestamp.getTime())) return null
  const messageId = typeof record.messageId === 'string' && record.messageId.length > 0
    ? record.messageId
    : undefined
  return {
    ...record,
    id: record.id,
    platform: String(record.platform ?? ''),
    selfId: String(record.selfId ?? ''),
    channelId: String(record.channelId ?? ''),
    guildId: record.guildId === undefined ? undefined : String(record.guildId),
    userId: String(record.userId ?? ''),
    username: String(record.username ?? ''),
    content: String(record.content ?? ''),
    timestamp,
    ...(messageId ? { messageId } : {})
  } as CollectedMessage
}
function storedDate(value: unknown): Date {
  return value instanceof Date ? new Date(value.getTime()) : new Date(String(value))
}

export function stableMessageRef(message: CollectedMessage): string | null {
  if (!message.messageId) return null
  return `${message.platform}:${message.selfId}:${message.guildId ?? ''}:${message.channelId}:${message.messageId}`
}

function parseSummaryDates(summary: SummaryV1): { start: Date; end: Date; expiresAt: Date } {
  if (!summary || typeof summary !== 'object' || Object.keys(summary).sort().join(',') !== 'collector_scope,expires_at,limitations,privacy_class,producer,retention_days,schema_id,topics,window_end,window_start' || !summary.collector_scope || !summary.producer || !Array.isArray(summary.topics) || !Array.isArray(summary.limitations) || !summary.producer.collector_scope || Object.keys(summary.collector_scope).sort().join(',') !== 'account_id,conversation_id,conversation_kind' || Object.keys(summary.producer).sort().join(',') !== 'account_id,collector_scope,plugin_version,source_manifest_sha256') {
    throw new CollectorError('summary_invalid', 'Summary.v1 structure contains missing or unknown fields')
  }
  const start = new Date(summary.window_start)
  const end = new Date(summary.window_end)
  const expiresAt = new Date(summary.expires_at)
  const retentionMs = summary.retention_days * 86_400_000
  if (
    summary.schema_id !== 'Summary.v1' || !Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime()) || !Number.isFinite(expiresAt.getTime()) || start > end ||
    !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$/.test(summary.window_start) || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$/.test(summary.window_end) || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$/.test(summary.expires_at) ||
    summary.collector_scope.conversation_kind !== 'group' || !summary.collector_scope.account_id || !/^[1-9][0-9]*$/.test(summary.collector_scope.conversation_id) || summary.producer.account_id !== summary.collector_scope.account_id ||
    summary.producer.collector_scope.account_id !== summary.collector_scope.account_id || summary.producer.collector_scope.conversation_kind !== summary.collector_scope.conversation_kind || summary.producer.collector_scope.conversation_id !== summary.collector_scope.conversation_id ||
    !Number.isInteger(summary.retention_days) || summary.retention_days < 1 || summary.retention_days > 365 || expiresAt <= new Date() || expiresAt.getTime() > Date.now() + retentionMs ||
    !['local_only', 'redacted_external_allowed'].includes(summary.privacy_class) || !/^[0-9a-f]{64}$/.test(summary.producer.source_manifest_sha256) || !summary.producer.plugin_version || summary.topics.length > 20 || summary.limitations.length > 20
  ) throw new CollectorError('summary_invalid', 'Summary.v1 structure or bounds are invalid')
  for (const topic of summary.topics) {
    if (!topic || Object.keys(topic).sort().join(',') !== 'message_refs,summary' || typeof topic.summary !== 'string' || !topic.summary || [...topic.summary].length > 4000 || !Array.isArray(topic.message_refs) || topic.message_refs.length > 500 || topic.message_refs.some((ref) => typeof ref !== 'string' || !ref || ref.length > 256)) throw new CollectorError('summary_invalid', 'Summary.v1 topic bounds are invalid')
  }
  if (summary.limitations.some((item) => typeof item !== 'string' || !item || [...item].length > 1000)) throw new CollectorError('summary_invalid', 'Summary.v1 limitations are invalid')
  return { start, end, expiresAt }
}

function collectorWindowId(platform: string, accountId: string, guildId: string, channelId: string, start: Date, end: Date, ownerEpoch: number): string {
  return createHash('sha256').update(JSON.stringify([platform, accountId, guildId, channelId, start.toISOString(), end.toISOString(), ownerEpoch])).digest('hex')
}

function validWindowOptions(options: CollectedMessageWindowOptions): void {
  if (!options.accountId || !/^[a-f0-9]{48}$/.test(options.ownerToken) || !Number.isInteger(options.ownerEpoch) || options.ownerEpoch < 1 || !options.guildId || !options.channelId) {
    throw new CollectorError('invalid_options', 'window requires account, owner token, positive epoch, group, and channel')
  }
  if (!(options.startTime instanceof Date) || !(options.endTime instanceof Date) || !Number.isFinite(options.startTime.getTime()) || !Number.isFinite(options.endTime.getTime()) || options.startTime > options.endTime) {
    throw new CollectorError('invalid_options', 'window requires an ordered finite startTime/endTime')
  }
  if (options.after) {
    const cursorTime = options.after.timestamp
    if (
      !(cursorTime instanceof Date) ||
      !Number.isFinite(cursorTime.getTime()) ||
      typeof options.after.id !== 'string' ||
      options.after.id.length === 0 ||
      cursorTime < options.startTime ||
      cursorTime > options.endTime
    ) {
      throw new CollectorError('invalid_options', 'window cursor must have a non-empty id and a timestamp within the window')
    }
  }
  const pageSize = options.pageSize ?? 250
  const maxMessages = options.maxMessages ?? 5_000
  if (!Number.isInteger(pageSize) || pageSize < 1 || pageSize > 1_000 || !Number.isInteger(maxMessages) || maxMessages < 1 || maxMessages > 50_000) {
    throw new CollectorError('invalid_options', 'window pageSize must be 1..1000 and maxMessages must be 1..50000')
  }

}
class CollectorRuntime implements AnalysisCollectorRuntime {
  readonly app: App
  readonly database: Database
  readonly diagnostics: CollectorDiagnostics
  readonly selfId: string
  readonly platform: string
  readonly stateDir: string
  readonly databaseFile: string
  readonly sourceManifestSha256: string
  readonly listenerGroups: readonly CollectorListenerGroup[]

  private stopped = false
  private readonly lock: StateLock
  private readonly ingressDispose: () => void
  private readonly botUninstallers: Map<string, () => void>
  private readonly record: BoundaryRecord
  private readonly logger?: ResolvedCollectorOptions['logger']

  private readonly manifestPath?: string
  private ownerEpochs = new Map<string, number>()

  constructor(parts: RuntimeParts) {
    this.app = parts.app
    this.database = parts.database
    this.diagnostics = parts.diagnostics
    this.selfId = parts.options.selfId
    this.platform = parts.options.platform
    this.stateDir = parts.options.stateDir
    this.databaseFile = parts.options.databaseFile
    this.sourceManifestSha256 = parts.sourceManifestSha256
    this.manifestPath = parts.options.sourceManifestFile
    this.listenerGroups = parts.options.listenerGroups
    this.lock = parts.lock
    this.ingressDispose = parts.ingressDispose
    this.botUninstallers = parts.botUninstallers
    this.record = parts.record
    this.logger = parts.logger
  }

  async stop(): Promise<void> {
    if (this.stopped) return
    this.stopped = true
    await cleanupApp(this.app, this.ingressDispose, this.botUninstallers, this.logger)
    await this.lock.release()
    this.logger?.info?.(
      `analysis collector stopped selfId=${this.selfId} stateDir=${this.stateDir}`
    )
  }

  assertCollectionOnly(operation: string): never {
    const detail = `P1a is collection-only: ${operation} is denied at the collector entry; direct upstream service references are not intercepted`
    this.record('p1a_operation_denied', operation, detail)
    throw new CollectorBoundaryError(operation, detail)
  }

  async readCollectedMessages(filter: CollectedMessageFilter = {}): Promise<CollectedMessage[]> {
    if (!this.database || typeof this.database.get !== 'function') {
      throw new CollectorError('collection_timeout', 'the isolated collection database is unavailable', {
        diagnostics: this.diagnostics
      })
    }
    const where: Record<string, unknown> = {
      platform: this.platform,
      selfId: this.selfId
    }
    if (filter.guildId !== undefined) where.guildId = filter.guildId
    if (filter.channelId !== undefined) where.channelId = filter.channelId
    if (filter.userId !== undefined) where.userId = filter.userId
    if (filter.messageId !== undefined) where.messageId = filter.messageId
    if (filter.startTime || filter.endTime) {
      where.timestamp = {
        ...(filter.startTime ? { $gte: filter.startTime } : {}),
        ...(filter.endTime ? { $lte: filter.endTime } : {})
      }
    }
    if (filter.after) {
      where.$or = [
        { timestamp: { $gt: filter.after.timestamp } },
        { timestamp: filter.after.timestamp, id: { $gt: filter.after.id } }
      ]
    }
    const limit = filter.limit ?? 10_000
    if (!Number.isInteger(limit) || limit < 1 || limit > 10_000) {
      throw new CollectorError('invalid_options', 'message read limit must be 1..10000')
    }
    const collectionDatabase = this.database as unknown as {
      get(table: string, query: Record<string, unknown>, cursor: { limit: number; sort: Record<string, 'asc' | 'desc'> }): Promise<unknown[]>
    }
    const rows = await collectionDatabase.get('chatluna_messages', where, {
      limit,
      sort: { timestamp: 'asc', id: 'asc' }
    })
    return rows.map(rowToMessage).filter((message): message is CollectedMessage => message !== null)
  }

  async readCollectedMessageWindow(options: CollectedMessageWindowOptions): Promise<CollectedMessageWindow> {
    validWindowOptions(options)
    const ownerId = this.ownerRecordId(options.guildId, options.channelId)
    const owner = (await this.databaseStore().get('bba_group_owner', { id: ownerId }))[0]
    if (!owner || owner.ownerSelfId !== this.selfId || owner.ownerToken !== options.ownerToken || owner.accountId !== options.accountId || Number(owner.ownerEpoch) !== options.ownerEpoch || new Date(String(owner.leaseExpiresAt)).getTime() <= Date.now()) {
      throw new CollectorError('owner_lease_conflict', 'window requires the active owner token and epoch')
    }
    this.ownerEpochs.set(`${options.guildId}:${options.channelId}`, options.ownerEpoch)
    const id = collectorWindowId(this.platform, options.accountId, options.guildId, options.channelId, options.startTime, options.endTime, options.ownerEpoch)
    const database = this.databaseStore()
    let window = (await database.get('bba_collector_window', { id }))[0]
    if (window && (window.accountId !== options.accountId || Number(window.ownerEpoch) !== options.ownerEpoch)) throw new CollectorError('owner_lease_conflict', 'persisted window belongs to another owner epoch')
    if (options.after && (!window || String(window.cursorId ?? '') !== options.after.id || storedDate(window.cursorTimestamp).getTime() !== options.after.timestamp.getTime())) {
      throw new CollectorError('invalid_options', 'continuation cursor does not match the persisted checkpoint')
    }
    if (window?.complete) return this.materializeWindow(window, [])
    const messages: CollectedMessage[] = []
    let after: CollectedMessageCursor | undefined = window ? { timestamp: storedDate(window.cursorTimestamp), id: String(window.cursorId) } : undefined
    let complete = false
    const pageSize = options.pageSize ?? 250
    const maxMessages = options.maxMessages ?? 5_000
    while (messages.length < maxMessages) {
      const remaining = maxMessages - messages.length
      const page = await this.readCollectedMessages({ guildId: options.guildId, channelId: options.channelId, startTime: options.startTime, endTime: options.endTime, after, limit: Math.min(pageSize, remaining) })
      messages.push(...page)
      if (page.length < Math.min(pageSize, remaining)) { complete = true; break }
      const last = page.at(-1)!
      after = { timestamp: last.timestamp, id: last.id }
    }
    if (!complete && messages.length >= maxMessages && after) {
      const more = await this.readCollectedMessages({ guildId: options.guildId, channelId: options.channelId, startTime: options.startTime, endTime: options.endTime, after, limit: 1 })
      complete = more.length === 0
    }
    const refs = messages.map(stableMessageRef).filter((ref): ref is string => ref !== null)
    const allRefs = [...new Set([...(Array.isArray(window?.messageRefs) ? window.messageRefs as string[] : []), ...refs])]
    const priorMessageCount = Number(window?.messageCount ?? 0)
    const lastMessage = messages.at(-1)
    const cursorTimestamp = lastMessage?.timestamp ?? (window?.cursorTimestamp ? storedDate(window.cursorTimestamp) : null)
    const cursorId = lastMessage?.id ?? (window?.cursorId ? String(window.cursorId) : '')
    const speakerMap = new Map<string, number>()
    for (const prior of Array.isArray(window?.speakerCounts) ? window.speakerCounts as Array<{ user_id: string; count: number }> : []) speakerMap.set(prior.user_id, prior.count)
    for (const message of messages) speakerMap.set(message.userId, (speakerMap.get(message.userId) ?? 0) + 1)
    const speakerCounts = [...speakerMap.entries()].map(([user_id, count]) => ({ user_id, count })).sort((a, b) => b.count - a.count || a.user_id.localeCompare(b.user_id))
    const data = { id, platform: this.platform, selfId: this.selfId, accountId: options.accountId, ownerEpoch: options.ownerEpoch, guildId: options.guildId, channelId: options.channelId, startTime: options.startTime, endTime: options.endTime, cursorTimestamp, cursorId, messageRefs: allRefs, messageCount: priorMessageCount + messages.length, speakerCounts, complete, updatedAt: new Date() }
    const { id: _windowId, ...update } = data
    if (window) await database.set('bba_collector_window', { id, ownerEpoch: options.ownerEpoch }, update)
    else await database.create('bba_collector_window', data)
    window = data
    return this.materializeWindow(window, messages)
  }

  private materializeWindow(window: Record<string, unknown>, messages: CollectedMessage[]): CollectedMessageWindow {
    const speakerCounts = Array.isArray(window.speakerCounts) ? window.speakerCounts as Array<{ user_id: string; count: number }> : []
    const refs = Array.isArray(window.messageRefs) ? window.messageRefs as string[] : []
    return { id: String(window.id), platform: String(window.platform), selfId: String(window.selfId), accountId: String(window.accountId), ownerEpoch: Number(window.ownerEpoch), guildId: String(window.guildId), channelId: String(window.channelId), startTime: storedDate(window.startTime), endTime: storedDate(window.endTime), messages, messageRefs: refs, statistics: { message_count: Number(window.messageCount), unique_speaker_count: speakerCounts.length, messages_per_speaker: speakerCounts, most_active_speaker_id: speakerCounts[0]?.user_id ?? null }, complete: Boolean(window.complete), nextCursor: window.complete || !window.cursorId ? null : { timestamp: storedDate(window.cursorTimestamp), id: String(window.cursorId) } }
  }

  async waitForCollectedMessages(
    options: WaitForCollectedMessagesOptions = {}
  ): Promise<CollectedMessage[]> {
    const minCount = options.minCount ?? 1
    const timeoutMs = options.timeoutMs ?? 5_000
    const pollIntervalMs = options.pollIntervalMs ?? 10
    const deadline = Date.now() + timeoutMs
    let lastCount = 0
    for (;;) {
      const messages = await this.readCollectedMessages(options.filter)
      lastCount = messages.length
      if (messages.length >= minCount) return messages
      const remaining = deadline - Date.now()
      if (remaining <= 0) {
        throw new CollectorError(
          'collection_timeout',
          `collected message count ${lastCount} did not reach ${minCount} within ${timeoutMs}ms`,
          {
            diagnostics: this.diagnostics,
            details: {
              lastCount,
              minCount,
              timeoutMs,
              filter: options.filter ?? {}
            }
          }
        )
      }
      await delay(Math.min(pollIntervalMs, remaining))
    }
  }

  private databaseStore(): CollectorDatabase {
    return this.database as unknown as CollectorDatabase
  }

  private ownerRecordId(guildId: string, channelId: string): string {
    return `${this.platform}:${guildId}:${channelId}`
  }

  async claimGroupOwner(claim: GroupOwnerClaim): Promise<GroupOwnerLease> {
    if (!claim.accountId || !this.listenerGroups.some((group) => group.enabled && group.guildId === claim.guildId && group.channelId === claim.channelId)) {
      throw new CollectorError('invalid_binding', 'owner claim must match an enabled listener group')
    }
    const leaseDurationMs = claim.leaseDurationMs ?? 30_000
    if (!Number.isInteger(leaseDurationMs) || leaseDurationMs < 1_000 || leaseDurationMs > 300_000) {
      throw new CollectorError('invalid_options', 'owner lease duration must be 1000..300000 ms')
    }
    const id = this.ownerRecordId(claim.guildId, claim.channelId)
    const token = claim.ownerToken ?? randomBytes(24).toString('hex')
    const leaseExpiresAt = new Date(Date.now() + leaseDurationMs)
    let lease!: GroupOwnerLease
    const database = this.databaseStore()
    await database.withTransaction(async (transaction) => {
      const current = (await transaction.get('bba_group_owner', { id }))[0]
      const active = Boolean(current && new Date(String(current.leaseExpiresAt)).getTime() > Date.now())
      const sameOwner = Boolean(active && current && current.ownerSelfId === this.selfId && current.accountId === claim.accountId && current.ownerToken === token)
      if (active && !sameOwner) throw new CollectorError('owner_lease_conflict', 'another owner token or account holds this group lease')
      const ownerEpoch = current ? Number(current.ownerEpoch) + (sameOwner ? 0 : 1) : 1
      const data = { id, platform: this.platform, guildId: claim.guildId, channelId: claim.channelId, accountId: claim.accountId, ownerSelfId: this.selfId, ownerToken: token, ownerEpoch, leaseExpiresAt }
      await transaction.upsert('bba_group_owner', [data], ['id'])
      lease = { ...data }
    })
    this.ownerEpochs.set(`${claim.guildId}:${claim.channelId}`, lease.ownerEpoch)
    return lease
  }

  async renewGroupOwner(claim: GroupOwnerClaim & { ownerEpoch: number; ownerToken: string }): Promise<GroupOwnerLease> {
    const id = this.ownerRecordId(claim.guildId, claim.channelId)
    const current = (await this.databaseStore().get('bba_group_owner', { id }))[0]
    if (!current || current.ownerSelfId !== this.selfId || current.ownerToken !== claim.ownerToken || current.accountId !== claim.accountId || Number(current.ownerEpoch) !== claim.ownerEpoch || new Date(String(current.leaseExpiresAt)).getTime() <= Date.now()) {
      throw new CollectorError('owner_lease_conflict', 'owner lease expired or its epoch changed')
    }
    const duration = claim.leaseDurationMs ?? 30_000
    if (!Number.isInteger(duration) || duration < 1_000 || duration > 300_000) throw new CollectorError('invalid_options', 'owner lease duration must be 1000..300000 ms')
    const leaseExpiresAt = new Date(Date.now() + duration)
    await this.databaseStore().set('bba_group_owner', { id, ownerEpoch: claim.ownerEpoch, ownerSelfId: this.selfId, ownerToken: claim.ownerToken }, { leaseExpiresAt })
    return { guildId: claim.guildId, channelId: claim.channelId, accountId: claim.accountId, ownerSelfId: this.selfId, ownerToken: claim.ownerToken, ownerEpoch: claim.ownerEpoch, leaseExpiresAt }
  }

  async releaseGroupOwner(claim: Pick<GroupOwnerClaim, 'guildId' | 'channelId' | 'accountId'> & { ownerEpoch: number; ownerToken: string }): Promise<void> {
    const id = this.ownerRecordId(claim.guildId, claim.channelId)
    const current = (await this.databaseStore().get('bba_group_owner', { id }))[0]
    if (!current || current.ownerSelfId !== this.selfId || current.ownerToken !== claim.ownerToken || current.accountId !== claim.accountId || Number(current.ownerEpoch) !== claim.ownerEpoch) {
      throw new CollectorError('owner_lease_conflict', 'cannot release an owner lease from a stale epoch')
    }
    await this.databaseStore().set('bba_group_owner', { id, ownerEpoch: claim.ownerEpoch, ownerToken: claim.ownerToken }, { leaseExpiresAt: new Date(0) })
    this.ownerEpochs.delete(`${claim.guildId}:${claim.channelId}`)
  }
  async saveSummary(windowId: string, summary: SummaryV1): Promise<StoredSummary> {
    if (!this.manifestPath || !this.sourceManifestSha256) throw new CollectorError('summary_invalid', 'a configured external source manifest is required')
    if (await sha256File(this.manifestPath) !== this.sourceManifestSha256) throw new CollectorError('summary_invalid', 'source manifest changed since collector startup')
    const dates = parseSummaryDates(summary)
    const window = (await this.databaseStore().get('bba_collector_window', { id: windowId }))[0]
    if (!window || !window.complete || dates.start.getTime() !== storedDate(window.startTime).getTime() || dates.end.getTime() !== storedDate(window.endTime).getTime()) throw new CollectorError('summary_reference_invalid', 'Summary must match a complete materialized window and its time bounds')
    const scope = summary.collector_scope
    const producerScope = summary.producer.collector_scope
    if (scope.account_id !== String(window.accountId) || scope.conversation_id !== String(window.guildId) || summary.producer.account_id !== String(window.accountId) || summary.producer.plugin_version !== PINNED_VERSIONS.analysis || summary.producer.source_manifest_sha256 !== this.sourceManifestSha256 || producerScope.account_id !== scope.account_id || producerScope.conversation_kind !== scope.conversation_kind || producerScope.conversation_id !== scope.conversation_id || summary.retention_days * 86_400_000 < dates.expiresAt.getTime() - dates.start.getTime()) throw new CollectorError('summary_invalid', 'Summary account, scope, producer, digest, window, or retention does not match the collector')
    const allowedRefs = new Set(Array.isArray(window.messageRefs) ? window.messageRefs as string[] : [])
    const refs = summary.topics.flatMap((topic) => topic.message_refs)
    if (refs.some((ref) => !allowedRefs.has(ref))) throw new CollectorError('summary_reference_invalid')
    const messages = await this.readCollectedMessages({ guildId: String(window.guildId), channelId: String(window.channelId), startTime: storedDate(window.startTime), endTime: storedDate(window.endTime), limit: 10_000 })
    const actualRefs = new Set(messages.map(stableMessageRef).filter((ref): ref is string => ref !== null))
    if (refs.some((ref) => !actualRefs.has(ref))) throw new CollectorError('summary_reference_invalid', 'Summary reference no longer resolves to a source row in its account/group/window')
    const owner = (await this.databaseStore().get('bba_group_owner', { id: this.ownerRecordId(String(window.guildId), String(window.channelId)) }))[0]
    if (!owner || owner.ownerSelfId !== this.selfId || owner.accountId !== String(window.accountId) || Number(owner.ownerEpoch) !== Number(window.ownerEpoch) || new Date(String(owner.leaseExpiresAt)).getTime() <= Date.now()) throw new CollectorError('owner_lease_conflict', 'Summary persistence requires the active window owner')
    const id = createHash('sha256').update(`${windowId}:${JSON.stringify(summary)}`).digest('hex')
    const createdAt = new Date()
    const stored: StoredSummary = { id, windowId, ownerEpoch: Number(window.ownerEpoch), accountId: String(window.accountId), guildId: String(window.guildId), channelId: String(window.channelId), createdAt, expiresAt: dates.expiresAt, summary }
    await this.databaseStore().upsert('bba_summary', [{ id, windowId, ownerEpoch: stored.ownerEpoch, accountId: stored.accountId, guildId: stored.guildId, channelId: stored.channelId, createdAt, expiresAt: dates.expiresAt, summaryJson: summary }], ['id'])
    return stored
  }

  async listSummaries(options: { guildId?: string; limit?: number } = {}): Promise<StoredSummary[]> {
    const limit = options.limit ?? 100
    if (!Number.isInteger(limit) || limit < 1 || limit > 500) throw new CollectorError('invalid_options', 'summary limit must be 1..500')
    const query: Record<string, unknown> = { expiresAt: { $gt: new Date() } }
    if (options.guildId !== undefined) query.guildId = options.guildId
    const rows = await this.databaseStore().get('bba_summary', query, { limit, sort: { createdAt: 'desc', id: 'asc' } })
    return rows.map((row) => ({ id: String(row.id), windowId: String(row.windowId), ownerEpoch: Number(row.ownerEpoch), accountId: String(row.accountId), guildId: String(row.guildId), channelId: String(row.channelId), createdAt: new Date(String(row.createdAt)), expiresAt: new Date(String(row.expiresAt)), summary: row.summaryJson as SummaryV1 }))
  }

  /**
   * Persist semantic claims beside the Summary that carried them. The row is
   * keyed by the Summary digest, so a duplicate window replay upserts the same
   * analysis instead of appending a second copy. Bounds, account/group/window
   * and expiry are re-checked against the already-stored Summary.
   */
  async saveSemanticAnalysis(summaryId: string, analysis: SemanticAnalysis): Promise<void> {
    if (!/^[0-9a-f]{64}$/.test(summaryId)) {
      throw new CollectorError('invalid_options', 'semantic analysis requires a Summary digest id')
    }
    if (
      !analysis ||
      analysis.schema_id !== 'SemanticAnalysis.v1' ||
      analysis.execution_authority !== 'none' ||
      typeof analysis.provider !== 'string' ||
      (analysis.evidence_source !== 'fixture' && analysis.evidence_source !== 'provider')
    ) {
      throw new CollectorError('summary_invalid', 'semantic analysis envelope is invalid')
    }
    const row = (await this.databaseStore().get('bba_summary', { id: summaryId }))[0]
    if (!row) throw new CollectorError('summary_invalid', 'semantic analysis must reference a stored Summary')
    const summary = row.summaryJson as SummaryV1
    const summaryExpires = new Date(String(row.expiresAt)).getTime()
    const generated = Date.parse(analysis.generated_at)
    const expires = Date.parse(analysis.expires_at)
    const windowStart = Date.parse(String(summary.window_start))
    const windowEnd = Date.parse(String(summary.window_end))
    const semanticWindowStart = Date.parse(analysis.window?.start ?? '')
    const semanticWindowEnd = Date.parse(analysis.window?.end ?? '')
    const expiryMismatch = !Number.isFinite(generated) || !Number.isFinite(expires) || expires <= generated || expires > summaryExpires
    const windowMismatch = semanticWindowStart !== windowStart || semanticWindowEnd !== windowEnd || analysis.window?.start !== summary.window_start || analysis.window?.end !== summary.window_end || analysis.observed_activity?.start !== summary.window_start || analysis.observed_activity?.end !== summary.window_end
    if (expiryMismatch || windowMismatch) {
      throw new CollectorError(
        'summary_invalid',
        'semantic analysis window or expiry does not match the stored Summary',
        { details: {
          expiry_mismatch: expiryMismatch,
          window_mismatch: windowMismatch,
          generated_at: analysis.generated_at,
          expires_at: analysis.expires_at,
          summary_expires_at: String(row.expiresAt),
          summary_window_start: summary.window_start,
          summary_window_end: summary.window_end,
          semantic_window_start: analysis.window?.start,
          semantic_window_end: analysis.window?.end,
          observed_window_start: analysis.observed_activity?.start,
          observed_window_end: analysis.observed_activity?.end,
          generated_ms: generated,
          expires_ms: expires,
          summary_expires_ms: summaryExpires,
          summary_window_start_ms: windowStart,
          summary_window_end_ms: windowEnd,
          semantic_window_start_ms: semanticWindowStart,
          semantic_window_end_ms: semanticWindowEnd
        } }
      )
    }
    if (analysis.observed_activity.message_count > 50_000) {
      throw new CollectorError('summary_invalid', 'semantic observed activity exceeds the window bound')
    }
    if (String(row.accountId) !== summary.collector_scope.account_id || String(row.guildId) !== summary.collector_scope.conversation_id) {
      throw new CollectorError('summary_invalid', 'stored Summary scope does not match its row')
    }
    await this.databaseStore().upsert('bba_semantic_analysis', [{
      id: summaryId,
      windowId: String(row.windowId),
      accountId: String(row.accountId),
      guildId: String(row.guildId),
      generatedAt: new Date(generated),
      expiresAt: new Date(expires),
      schemaId: analysis.schema_id,
      evidenceSource: analysis.evidence_source,
      provider: analysis.provider,
      analysisJson: analysis
    }], ['id'])
  }

  async readSemanticAnalysis(summaryId: string): Promise<SemanticAnalysis | null> {
    if (!/^[0-9a-f]{64}$/.test(summaryId)) {
      throw new CollectorError('invalid_options', 'semantic analysis requires a Summary digest id')
    }
    const row = (await this.databaseStore().get('bba_semantic_analysis', { id: summaryId }))[0]
    if (!row) return null
    const analysis = row.analysisJson as SemanticAnalysis | undefined
    if (!analysis || analysis.schema_id !== 'SemanticAnalysis.v1') {
      throw new CollectorError('summary_invalid', 'persisted semantic analysis is malformed')
    }
    return analysis
  }
}

export async function startCollector(
  options: AnalysisCollectorOptions
): Promise<AnalysisCollectorRuntime> {
  const resolved = resolveCollectorOptions(options)
  const startedAtMs = Date.now()
  const deadline = startedAtMs + resolved.startupTimeoutMs
  const diagnostics = createBaseDiagnostics(resolved, resolved.pluginModes, startedAtMs)
  const logger = resolved.logger

  let lock: StateLock | undefined
  let app: App | undefined
  let ingressDispose: (() => void) | undefined
  const botUninstallers = new Map<string, () => void>()

  try {
    lock = await acquireStateLock(resolved)
    await ensureStateBinding(resolved)
    await mkdir(resolved.appDir, { recursive: true, mode: 0o700 })

    app = new KoishiApp({ prefix: '.', autoAssign: true })
    registerCollectorModels(app)
    app.baseDir = resolved.appDir

    if (resolved.pluginModes.database === 'character-durable-file') {
      app.plugin(createDurableFileDatabase(resolved.databaseFile) as never, (resolved.databaseConfig ?? {}) as never)
    } else if (resolved.databasePlugin !== null) {
      app.plugin(resolved.databasePlugin as never, (resolved.databaseConfig ?? {}) as never)
    }

    if (resolved.chatlunaPlugin !== null) {
      app.plugin(resolved.chatlunaPlugin as never, buildChatLunaConfig(resolved) as never)
    }

    app.plugin(resolved.httpPlugin as never, (resolved.httpConfig ?? {}) as never)
    app.plugin(resolved.puppeteerPlugin as never, (resolved.puppeteerConfig ?? {}) as never)

    for (const entry of resolved.extraPlugins) {
      app.plugin(entry.plugin as never, (entry.config ?? {}) as never)
    }

    if (resolved.analysisPlugin !== null) {
      app.plugin(resolved.analysisPlugin as never, buildGroupAnalysisConfig(resolved) as never)
    }

    const record = createBoundaryRecorder(diagnostics)
    ingressDispose = installCollectorIngress(app as unknown as Context, {
      selfId: resolved.selfId,
      platform: resolved.platform,
      record,
      logger,
      onDispatchBlocked: () => {
        diagnostics.boundary.dispatchBlocked++
      }
    })

    let botSequence = 0
    const guardBot = (bot: Bot): void => {
      if (!bot || typeof bot !== 'object') return
      const label = `${String(bot.platform ?? resolved.platform)}:${String(bot.selfId ?? bot.sn ?? ++botSequence)}`
      if (!diagnostics.adapter.botsObserved.includes(label)) {
        diagnostics.adapter.botsObserved.push(label)
      }
      if (botUninstallers.has(label)) return
      botUninstallers.set(label, installOutboundDenial(bot, record, logger))
    }
    app.on('bot-added', guardBot)
    app.on('bot-connect', guardBot)

    const startupTimeout = Promise.withResolvers<void>()
    const startupTimer = setTimeout(() => {
      startupTimeout.reject(
        new CollectorError(
          'startup_timeout',
          `collector App did not start within ${resolved.startupTimeoutMs}ms`,
          {
            diagnostics,
            details: { timeoutMs: resolved.startupTimeoutMs }
          }
        )
      )
    }, resolved.startupTimeoutMs)
    try {
      await Promise.race([app.start(), startupTimeout.promise])
    } catch (error) {
      if (error instanceof CollectorError) throw error
      throw new CollectorError(
        'app_start_failed',
        `collector App failed to start: ${error instanceof Error ? error.message : String(error)}`,
        { cause: error, diagnostics }
      )
    } finally {
      clearTimeout(startupTimer)
    }

    for (const bot of app.bots) guardBot(bot)

    await waitForServices(app as unknown as Context, diagnostics, resolved, startedAtMs, deadline)

    const database = readContextProperty(app as unknown as Context, 'database')
    if (!database || typeof database !== 'object') {
      throw new CollectorError(
        'service_injection_timeout',
        'database service was reported injected but is not an object',
        { diagnostics }
      )
    }

    diagnostics.readyAt = new Date().toISOString()
    diagnostics.startupDurationMs = Date.now() - startedAtMs
    diagnostics.injection.complete = true
    diagnostics.injection.missing = []
    diagnostics.injection.waitedMs = Date.now() - startedAtMs

    logger?.info?.(
      `analysis collector ready selfId=${resolved.selfId} platform=${resolved.platform} groups=${resolved.listenerGroups.length} stateDir=${resolved.stateDir}`
    )

    return new CollectorRuntime({
      app,
      database: database as Database,
      diagnostics,
      options: resolved,
      lock,
      ingressDispose,
      botUninstallers,
      record,
      sourceManifestSha256: resolved.sourceManifestFile ? await sha256File(resolved.sourceManifestFile) : '',
      logger
    })
  } catch (error) {
    diagnostics.startupDurationMs = Date.now() - startedAtMs
    const classified =
      error instanceof CollectorError
        ? error
        : new CollectorError(
            'app_start_failed',
            `analysis collector failed to start: ${error instanceof Error ? error.message : String(error)}`,
            { cause: error, diagnostics }
          )
    if (classified.diagnostics === null) {
      // CollectorError exposes a readonly field; replace the instance only
      // when the original had no diagnostics snapshot.
      const withDiagnostics = new CollectorError(classified.code, classified.message, {
        cause: classified,
        diagnostics,
        details: classified.details
      })
      await cleanupApp(app, ingressDispose, botUninstallers, logger)
      if (lock) await lock.release()
      throw withDiagnostics
    }
    await cleanupApp(app, ingressDispose, botUninstallers, logger)
    if (lock) await lock.release()
    throw classified
  }
}

export async function stopCollector(
  runtime: AnalysisCollectorRuntime | null | undefined
): Promise<void> {
  if (!runtime) return
  await runtime.stop()
}

/** Short alias used by callers that import the pair as start/stop. */
export const stop = stopCollector
