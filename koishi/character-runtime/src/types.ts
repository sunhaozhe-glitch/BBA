import type { App, Bot } from './koishi.js'
import type { CharacterSendGate } from './gate.js'

export type ConversationKind = 'group' | 'private'
export interface GroupContextTopic {
  summary: string
  message_refs: string[]
  evidence_kind: 'member_statement' | 'deterministic_statistic' | 'model_inference'
}

export interface GroupContext {
  account_id: string
  group_id: string
  summary_ref: string | null
  /** Backend-assigned Summary identity; present with a stored Summary version. */
  summary_id?: string | null
  summary_version?: number | null
  /** Collector source-manifest digest that produced this Summary. */
  source_manifest_sha256?: string | null
  privacy_class?: 'local_only' | 'redacted_external_allowed' | 'none'
  topics: GroupContextTopic[]
  activity_window: { start: string; end: string; message_count: number } | null
  active_speaker_account_id: string | null
  cooldown_until: string | null
  daily_remaining: number | null
  participation_goal: string | null
  source: string
  generated_at: string
  expires_at: string
}

export interface GroupContextRequest {
  account: CharacterAccountOptions
  groupId: string
  senderId: string
}

export type GroupContextProvider = (request: GroupContextRequest) => Promise<GroupContext | null>


/**
 * Trusted event identity supplied to the final send gate.
 *
 * The gate is the last boundary before an adapter call. It is never allowed to
 * infer the account, runtime or adapter from a mutable session object.
 */
export interface CharacterEventContext {
  accountId: string
  runtimeId: string
  adapterId: string
  adapterInstanceId: string
  senderId: string
}

/**
 * Wire types for the backend chat gate.
 *
 * They are bound to the offline contract agreed with the backend gate owners
 * (ChatGateBackend trigger/context routes, Backend permit routes) and to
 * contracts/schemas/bridge.json ChatPermitRequest/ChatPermit.
 */
export interface ChatTriggerRequest {
  trigger_ref: string
  account_id: string
  conversation_kind: ConversationKind
  conversation_id: string
  source_event_id: string
  sender_id: string
  directed_account_id: string | null
  deadline_at: string
  segment_count: number
}

export interface ChatTrigger {
  trigger_ref: string
  account_id: string
  conversation_kind: ConversationKind
  conversation_id: string
  source_event_id: string
  sender_id: string
  directed_account_id: string | null
  deadline_at: string
  segment_count: number
  next_segment: number
  /** Backend trigger state; there is no 'closed' value on the wire. */
  state: 'open' | 'complete' | 'expired' | 'suppressed'
  created_at: string
}

export interface ChatContext {
  account_id: string
  qq_self_id: string
  conversation_kind: ConversationKind
  conversation_id: string
  active_speaker_account_id: string | null
  binding_epoch: string | null
  lease_expires_at: string | null
  speaker_active: boolean
  chat_enabled: boolean
  daily_limit: number
  issued_today: number
  managed_self_ids: string[]
}

export interface ChatPermitRequest {
  send_id: string
  account_id: string
  conversation_kind: ConversationKind
  conversation_id: string
  trigger_ref: string
  text_digest: string
  expected_binding_epoch: string
  deadline_at: string
  segment_index: number
  segment_count: number
}

export interface ChatPermit {
  permit_id: string
  send_id: string
  account_id: string
  conversation_kind: ConversationKind
  conversation_id: string
  trigger_ref: string
  text_digest: string
  expected_binding_epoch: string
  issued_at: string
  expires_at: string
  segment_index: number
  segment_count: number
  state: 'issued' | 'consumed' | 'expired'
}

export interface ChatDeliveryOutcome {
  send_id: string
  state: 'sent' | 'unknown'
  /** Always sent on the wire. A successful adapter call without an ID is sent+null. */
  message_id: string | null
  occurred_at: string
}

export interface CharacterAccountOptions {
  /** Stable internal account id. Never a label, port or nickname. */
  accountId: string
  /** Authenticated OneBot self id for the account's own bot. */
  qqSelfId: string
  /** Unique runtime partition id (one Koishi App per account). */
  runtimeId: string
  /** Unique Character runtime partition id reported to the backend. */
  characterRuntimeId: string
  /** Unique Character database partition id. */
  characterDbId: string
  /** Unique Character cache namespace, materialised as an isolated directory. */
  characterCacheNamespace: string
  /** Unique persona partition id. */
  personaId: string
  /** Persona content owned by this runtime only. */
  persona: string
  /** Runtime credential; never logged or persisted by this package. */
  runtimeToken: string
  /** Independently configured trusted gateway origin and credential. */
  gateway?: { baseUrl: string; token: string }
  /** Stable adapter family id used for the trusted event context. */
  adapterId?: string
  /** Unique adapter instance id; defaults to runtimeId. */
  adapterInstanceId?: string
  /**
   * Koishi plugin that provides the account's transport bot (adapter plugin or
   * an explicit test transport bot class). It is installed into the account's
   * own App only.
   */
  transportPlugin: unknown
  transportConfig?: unknown
  /** Optional per-account ChatLuna config overrides. */
  chatlunaConfig?: Record<string, unknown>
  /** Optional per-account Character config overrides. */
  characterConfig?: Record<string, unknown>
  /** Optional model name used by the actual Character plugin. */
  characterModel?: string
  /** Extra managed self ids ignored at ingress in addition to the backend list. */
  managedSelfIds?: string[]
  /** Maximum declared segment budget per trusted trigger (1..32). */
  maxSegmentCount?: number
  /** Deadline used when this runtime opens a proactive trigger. */
  triggerTtlMs?: number
}

export interface ChatBackendEndpoints {
  triggers: string
  context: string
  groupContext: string
  permits: string
  consume: (permitId: string) => string
  outcome: (permitId: string) => string
}

export interface CharacterRuntimeManagerOptions {
  stateDir: string
  backendBaseUrl: string
  accounts: CharacterAccountOptions[]
  /**
   * Plugins installed into every account App after the database and before the
   * transport. `chatlunaPlugin` defaults to the pinned actual ChatLuna package;
   * `characterPlugin` defaults to the pinned actual Character package. Tests
   * may supply explicit simulated plugins instead.
   */
  chatlunaPlugin?: unknown | null
  chatlunaConfig?: Record<string, unknown>
  characterPlugin?: unknown
  characterConfig?: Record<string, unknown>
  providerPlugins?: Array<{ plugin: unknown; config?: unknown }>
  /**
   * Explicit provider wins. Otherwise the runtime reads the backend
   * GroupContext route for every group prompt; set `enabled: false` to omit
   * group context entirely.
   */
  groupContextProvider?: GroupContextProvider
  groupContextBackend?: GroupContextBackendOptions
  databasePlugin?: unknown
  databaseConfig?: unknown
  endpoints?: Partial<Omit<ChatBackendEndpoints, 'consume' | 'outcome'>> & {
    consume?: (permitId: string) => string
    outcome?: (permitId: string) => string
  }
  /**
   * Start each account App (and therefore its injected transport). Defaults to
   * false: constructing and wiring the runtime never opens a live adapter.
   * An offline/test transport must opt in explicitly with `startApps: true`.
   */
  startApps?: boolean
  fetchImpl?: typeof fetch
  requestTimeoutMs?: number
  now?: () => number
  logger?: CharacterRuntimeLogger
}

export interface GroupContextBackendOptions {
  enabled?: boolean
  path?: string
  /** Maximum accepted generated_at future offset. Defaults to five minutes. */
  maxGeneratedSkewMs?: number
}

export interface CharacterRuntimeLogger {
  debug?(message: string, ...args: unknown[]): void
  info?(message: string, ...args: unknown[]): void
  warn?(message: string, ...args: unknown[]): void
  error?(message: string, ...args: unknown[]): void
}

export interface CharacterAccountRuntime {
  account: CharacterAccountOptions
  app: App
  /** Undefined until the App is started (startApps: true) and its bot connects. */
  bot?: Bot
  gate: CharacterSendGate
  directories: {
    root: string
    cache: string
    ledger: string
    database: string
    persona: string
    preset: string
  }
}
