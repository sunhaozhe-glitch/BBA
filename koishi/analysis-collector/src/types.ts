import type { App, Database } from 'koishi'
import type { SemanticAnalysis } from './semantic-analysis.js'

/** Pinned upstream packages. The package manifest carries the same versions. */
export const PINNED_VERSIONS = {
  koishi: '4.18.11',
  chatluna: '1.4.4',
  analysis: '1.4.1',
  puppeteer: '3.9.0'
} as const

export type CollectorServiceName = 'database' | 'chatluna' | 'puppeteer' | 'analysis'

export type CollectorErrorCode =
  | 'invalid_options'
  | 'invalid_binding'
  | 'state_dir_in_use'
  | 'state_binding_conflict'
  | 'state_lock_failed'
  | 'plugin_load_failed'
  | 'app_start_failed'
  | 'startup_timeout'
  | 'service_injection_timeout'
  | 'collection_timeout'
  | 'boundary_denied'
  | 'owner_lease_conflict'
  | 'summary_invalid'
  | 'summary_reference_invalid'
  | 'summary_publish_failed'

export interface CollectorLogger {
  debug?(message: string, ...args: unknown[]): void
  info?(message: string, ...args: unknown[]): void
  warn?(message: string, ...args: unknown[]): void
  error?(message: string, ...args: unknown[]): void
}

/** Exactly one self id and platform are bound to a collector instance. */
export interface CollectorListenerGroupInput {
  /** QQ group id. One of guildId/channelId is required. */
  guildId?: string
  /** OneBot channel id; defaults to guildId when only guildId is supplied. */
  channelId?: string
  /**
   * Accepted for compatibility with the upstream listener shape. It must be
   * absent or equal to the collector's single selfId.
   */
  selfId?: string
  /** Must be absent or equal to the collector's single platform. */
  platform?: string
  /** At least one listener group must be enabled. Defaults to true. */
  enabled?: boolean
}

export interface CollectorListenerGroup {
  platform: string
  selfId: string
  channelId: string
  guildId?: string
  enabled: boolean
}

export interface AnalysisCollectorPluginEntry {
  plugin: unknown
  config?: unknown
}

export interface AnalysisCollectorOptions {
  /** Dedicated state directory. Concurrent active use by another collector is rejected. */
  stateDir: string
  /** The one QQ self id this collector is allowed to observe. */
  selfId: string
  /** Defaults to onebot. */
  platform?: string
  /** At least one explicit group binding is required. */
  listenerGroups: CollectorListenerGroupInput[]
  /** Optional external file-level source manifest; Summary persistence is disabled without it. */
  sourceManifestFile?: string
  /** Bounds App start plus required-service injection. Defaults to 30000 ms. */
  startupTimeoutMs?: number
  /** Poll interval while waiting for required services. Defaults to 25 ms. */
  pollIntervalMs?: number
  /** Defaults to the durable Character database driver (independent db file). */
  databasePlugin?: unknown
  databaseConfig?: unknown
  /** Koishi HTTP service config used by the real Puppeteer plugin. */
  httpConfig?: unknown
  /** Defaults to the real koishi-plugin-chatluna 1.4.4 package. null disables it for failure tests. */
  chatlunaPlugin?: unknown | null
  chatlunaConfig?: Record<string, unknown>
  /** Must be a real/injectable puppeteer service plugin; null is rejected. */
  puppeteerPlugin?: unknown
  puppeteerConfig?: unknown
  /** Defaults to the real koishi-plugin-chatluna-group-analysis 1.4.1 package. null disables it for failure tests. */
  analysisPlugin?: unknown | null
  /** Non-forced group-analysis options. Forced P1a fields always win. */
  analysisConfig?: Record<string, unknown>
  /** Extra plugins loaded after puppeteer and before the analysis plugin. */
  extraPlugins?: AnalysisCollectorPluginEntry[]
  logger?: CollectorLogger
}

export interface CollectorServiceDiagnostic {
  name: CollectorServiceName
  required: true
  /** Koishi Context property inspected for injection. */
  property: string
  injected: boolean
  status: 'available' | 'missing'
  plugin: string
  /** Milliseconds from collector start to this observation. */
  waitedMs: number
  detail?: string
}

export interface CollectorInjectionDiagnostic {
  required: CollectorServiceName[]
  complete: boolean
  missing: CollectorServiceName[]
  timeoutMs: number
  waitedMs: number
}

export type CollectorCapabilityStatus =
  | 'enabled'
  | 'disabled'
  | 'denied'
  | 'not_loaded'
  | 'not_implemented'

export interface CollectorCapabilityRow {
  capability: string
  status: CollectorCapabilityStatus
  /** null means the upstream package has no configuration switch for this capability. */
  upstreamSwitch: boolean | null
  reason: string
}

export interface CollectorBoundaryViolation {
  seq: number
  code: string
  operation: string
  detail: string
  at: string
}

export interface CollectorBoundaryDiagnostic {
  /** Prepended App middleware is the only controllable dispatch boundary. */
  dispatchGuard: 'prepended_app_middleware'
  outboundGuard: 'all_bot_send_and_internal_paths'
  dispatchBlocked: number
  commandsDenied: number
  outboundDenied: number
  violations: CollectorBoundaryViolation[]
  droppedViolations: number
}

export interface CollectorForcedGroupAnalysisConfig {
  cronSchedule: ''
  registerTools: false
  userTitleAnalysis: false
  personaAnalysisMessageInterval: 0
  enableAllGroupsByDefault: false
  outputFormat: 'text'
  alwaysPersistMessages: true
  listenerGroups: CollectorListenerGroup[]
}

export interface CollectorForcedChatLunaConfig {
  botNames: string[]
  isNickname: false
  isNickNameWithContent: false
  allowPrivate: false
  allowAtReply: false
  allowQuoteReply: false
  privateChatWithoutCommand: false
  randomReplyFrequency: 0
  defaultModel: string
  enablePresetKeywordTrigger: false
}

export interface CollectorUpstreamDiagnostic {
  groupAnalysisCommands: {
    registered: true
    switchable: false
    p1a: 'dispatch_denied'
  }
  personaManualCommand: {
    registered: true
    switchable: false
    p1a: 'explicit_boundary_error'
  }
  promptVariables: {
    registered: true
    switchable: false
    variables: ['group_message_fetch', 'group_user_persona']
    p1a: 'model_execution_denied'
  }
  manualAnalysisService: {
    present: true
    switchable: false
    p1a: 'no_collector_entry'
  }
}

export interface CollectorSafeguardDiagnostic {
  outboundSend: 'denied'
  modelExecution: 'denied'
  analysisExecution: 'denied'
  reportRendering: 'denied'
  commandDispatch: 'denied'
  adapter: 'none'
}

export interface CollectorAdapterDiagnostic {
  /** P1a never installs an adapter plugin. */
  loaded: false
  botsObserved: string[]
}

export interface CollectorDiagnostics {
  phase: 'P1a'
  collectionOnly: true
  stateDir: string
  databaseFile: string
  selfId: string
  platform: string
  startedAt: string
  readyAt: string | null
  startupDurationMs: number
  startupTimeoutMs: number
  pollIntervalMs: number
  pinnedVersions: typeof PINNED_VERSIONS
  plugins: {
    database: 'injected' | 'character-durable-file' | 'disabled'
    chatluna: 'real' | 'injected' | 'disabled'
    puppeteer: 'real' | 'injected'
    analysis: 'real' | 'injected' | 'disabled'
  }
  services: Record<CollectorServiceName, CollectorServiceDiagnostic>
  injection: CollectorInjectionDiagnostic
  forcedConfig: {
    groupAnalysis: CollectorForcedGroupAnalysisConfig
    chatLuna: CollectorForcedChatLunaConfig
  }
  upstream: CollectorUpstreamDiagnostic
  safeguards: CollectorSafeguardDiagnostic
  adapter: CollectorAdapterDiagnostic
  matrix: CollectorCapabilityRow[]
  limitations: string[]
  boundary: CollectorBoundaryDiagnostic
}

export interface GroupOwnerClaim {
  guildId: string
  channelId: string
  accountId: string
  ownerToken?: string
  leaseDurationMs?: number
}
export interface GroupOwnerLease {
  guildId: string
  channelId: string
  accountId: string
  ownerSelfId: string
  ownerToken: string
  ownerEpoch: number
  leaseExpiresAt: Date
}

export interface SummaryV1 {
  schema_id: 'Summary.v1'
  window_start: string
  window_end: string
  collector_scope: {
    account_id: string
    conversation_kind: 'group'
    conversation_id: string
  }
  privacy_class: 'local_only' | 'redacted_external_allowed'
  retention_days: number
  expires_at: string
  producer: {
    account_id: string
    collector_scope: SummaryV1['collector_scope']
    plugin_version: string
    source_manifest_sha256: string
  }
  topics: Array<{ summary: string; message_refs: string[] }>
  limitations: string[]
}

export interface StoredSummary {
  id: string
  windowId: string
  ownerEpoch: number
  accountId: string
  guildId: string
  channelId: string
  createdAt: Date
  expiresAt: Date
  summary: SummaryV1
}

export interface CollectedMessageCursor {
  timestamp: Date
  id: string
}

export interface CollectedMessageFilter {
  guildId?: string
  channelId?: string
  userId?: string
  messageId?: string
  startTime?: Date
  endTime?: Date
  limit?: number
  after?: CollectedMessageCursor
}

export type CollectedMessage = {
  id: string
  platform: string
  selfId: string
  channelId: string
  guildId?: string
  userId: string
  username: string
  content: string
  timestamp: Date
  messageId?: string
} & Record<string, unknown>

export interface CollectedMessageWindowOptions {
  accountId: string
  ownerEpoch: number
  ownerToken: string
  guildId: string
  channelId: string
  startTime: Date
  endTime: Date
  after?: CollectedMessageCursor
  pageSize?: number
  maxMessages?: number
}

export interface CollectedMessageWindow {
  id: string
  platform: string
  selfId: string
  accountId: string
  ownerEpoch: number
  guildId: string
  channelId: string
  startTime: Date
  endTime: Date
  messages: CollectedMessage[]
  messageRefs: string[]
  statistics: {
    message_count: number
    unique_speaker_count: number
    messages_per_speaker: Array<{ user_id: string; count: number }>
    most_active_speaker_id: string | null
  }
  complete: boolean
  nextCursor: CollectedMessageCursor | null
}

export interface WaitForCollectedMessagesOptions {
  filter?: CollectedMessageFilter
  minCount?: number
  timeoutMs?: number
  pollIntervalMs?: number
}

/**
 * P1a runtime. Collection is the only enabled behavior. `assertCollectionOnly`
 * is an explicit denial entry for tests/callers; it never claims that a direct
 * reference to an upstream service method is intercepted.
 */
export interface AnalysisCollectorRuntime {
  readonly app: App
  readonly database: Database
  readonly diagnostics: CollectorDiagnostics
  readonly selfId: string
  readonly platform: string
  readonly stateDir: string
  readonly databaseFile: string
  readonly sourceManifestSha256: string
  readonly listenerGroups: readonly CollectorListenerGroup[]
  stop(): Promise<void>
  assertCollectionOnly(operation: string): never
  readCollectedMessages(filter?: CollectedMessageFilter): Promise<CollectedMessage[]>
  readCollectedMessageWindow(options: CollectedMessageWindowOptions): Promise<CollectedMessageWindow>
  waitForCollectedMessages(options?: WaitForCollectedMessagesOptions): Promise<CollectedMessage[]>
  claimGroupOwner(claim: GroupOwnerClaim): Promise<GroupOwnerLease>
  renewGroupOwner(claim: GroupOwnerClaim & { ownerEpoch: number; ownerToken: string }): Promise<GroupOwnerLease>
  releaseGroupOwner(claim: Pick<GroupOwnerClaim, 'guildId' | 'channelId' | 'accountId'> & { ownerEpoch: number; ownerToken: string }): Promise<void>
  saveSummary(windowId: string, summary: SummaryV1): Promise<StoredSummary>
  listSummaries(options?: { guildId?: string; limit?: number }): Promise<StoredSummary[]>
  /** Persist validated semantic claims beside one stored Summary. */
  saveSemanticAnalysis(summaryId: string, analysis: SemanticAnalysis): Promise<void>
  /** Read the persisted semantic claims for one stored Summary, if any. */
  readSemanticAnalysis(summaryId: string): Promise<SemanticAnalysis | null>
}

/** Operations that are never executed in P1a. */
export type P1aDeniedOperation =
  | 'persona-analysis'
  | 'prompt-variable'
  | 'model'
  | 'report'
  | 'send'
  | 'manual-analysis'
