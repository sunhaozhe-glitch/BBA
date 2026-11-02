import { resolve } from 'node:path'
import { CollectorError } from './errors.js'
import type {
  AnalysisCollectorOptions,
  AnalysisCollectorPluginEntry,
  CollectorCapabilityRow,
  CollectorDiagnostics,
  CollectorForcedChatLunaConfig,
  CollectorForcedGroupAnalysisConfig,
  CollectorListenerGroup,
  CollectorListenerGroupInput,
  CollectorLogger,
  CollectorServiceName
} from './types.js'
import { PINNED_VERSIONS } from './types.js'

export const REQUIRED_SERVICES: Record<
  CollectorServiceName,
  { property: string; plugin: string }
> = {
  database: { property: 'database', plugin: 'bba-character-runtime/durable-file' },
  chatluna: { property: 'chatluna', plugin: 'koishi-plugin-chatluna@1.4.4' },
  puppeteer: { property: 'puppeteer', plugin: 'koishi-plugin-puppeteer@3.9.0' },
  analysis: {
    property: 'chatluna_group_analysis',
    plugin: 'koishi-plugin-chatluna-group-analysis@1.4.1'
  }
}

export const SERVICE_ORDER: CollectorServiceName[] = [
  'database',
  'chatluna',
  'puppeteer',
  'analysis'
]

export interface ResolvedCollectorOptions {
  stateDir: string
  databaseFile: string
  appDir: string
  selfId: string
  platform: string
  listenerGroups: CollectorListenerGroup[]
  sourceManifestFile?: string
  startupTimeoutMs: number
  pollIntervalMs: number
  databasePlugin: unknown | null
  databaseConfig: unknown
  httpPlugin: unknown
  httpConfig: unknown
  chatlunaPlugin: unknown | null
  chatlunaConfig: Record<string, unknown>
  puppeteerPlugin: unknown
  puppeteerConfig: unknown
  analysisPlugin: unknown | null
  analysisConfig: Record<string, unknown>
  extraPlugins: AnalysisCollectorPluginEntry[]
  pluginModes: CollectorPluginModes
  logger?: CollectorLogger
}

export interface CollectorPluginModes {
  database: 'injected' | 'character-durable-file' | 'disabled'
  chatluna: 'real' | 'injected' | 'disabled'
  puppeteer: 'real' | 'injected'
  analysis: 'real' | 'injected' | 'disabled'
}

function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {}
}

function requireText(value: unknown, field: string): string {
  if (typeof value !== 'string' || value.trim().length === 0) {
    throw new CollectorError('invalid_options', `${field} must be a non-empty string`)
  }
  return value.trim()
}

function requirePositiveInteger(value: unknown, field: string, fallback: number): number {
  if (value === undefined) return fallback
  if (typeof value !== 'number' || !Number.isInteger(value) || value < 1) {
    throw new CollectorError('invalid_options', `${field} must be a positive integer`)
  }
  return value
}

function normalizeListenerGroups(
  raw: readonly CollectorListenerGroupInput[],
  selfId: string,
  platform: string
): CollectorListenerGroup[] {
  if (!Array.isArray(raw) || raw.length === 0) {
    throw new CollectorError(
      'invalid_options',
      'listenerGroups must contain at least one explicit enabled group bound to the single selfId'
    )
  }
  const seen = new Set<string>()
  const groups: CollectorListenerGroup[] = []
  let enabledCount = 0
  for (const [index, entry] of raw.entries()) {
    if (!entry || typeof entry !== 'object') {
      throw new CollectorError('invalid_options', `listenerGroups[${index}] must be an object`)
    }
    if (entry.selfId !== undefined && entry.selfId !== selfId) {
      throw new CollectorError(
        'invalid_binding',
        `listenerGroups[${index}].selfId=${String(entry.selfId)} does not match the single collector selfId=${selfId}`
      )
    }
    if (entry.platform !== undefined && entry.platform !== platform) {
      throw new CollectorError(
        'invalid_binding',
        `listenerGroups[${index}].platform=${String(entry.platform)} does not match the single collector platform=${platform}`
      )
    }
    const guildId =
      typeof entry.guildId === 'string' && entry.guildId.trim().length > 0
        ? entry.guildId.trim()
        : undefined
    const channelIdInput =
      typeof entry.channelId === 'string' && entry.channelId.trim().length > 0
        ? entry.channelId.trim()
        : undefined
    if (!guildId && !channelIdInput) {
      throw new CollectorError(
        'invalid_options',
        `listenerGroups[${index}] requires a non-empty guildId or channelId`
      )
    }
    const channelId = channelIdInput ?? guildId!
    const key = `${platform}\u0000${selfId}\u0000${guildId ?? ''}\u0000${channelId}`
    if (seen.has(key)) {
      throw new CollectorError('invalid_options', `listenerGroups[${index}] is a duplicate binding`)
    }
    seen.add(key)
    const enabled = entry.enabled !== false
    if (enabled) enabledCount++
    const group: CollectorListenerGroup = {
      platform,
      selfId,
      channelId,
      enabled
    }
    // exactOptionalPropertyTypes is disabled, but avoid writing undefined.
    if (guildId) group.guildId = guildId
    groups.push(group)
  }
  if (enabledCount === 0) {
    throw new CollectorError(
      'invalid_options',
      'listenerGroups must contain at least one enabled group bound to the single selfId'
    )
  }
  return groups
}

function normalizeExtraPlugins(raw: unknown): AnalysisCollectorPluginEntry[] {
  if (raw === undefined) return []
  if (!Array.isArray(raw)) {
    throw new CollectorError('invalid_options', 'extraPlugins must be an array')
  }
  return raw.map((entry, index) => {
    if (!entry || typeof entry !== 'object') {
      throw new CollectorError('invalid_options', `extraPlugins[${index}] must be an object`)
    }
    const record = entry as { plugin?: unknown; config?: unknown }
    if (record.plugin === undefined || record.plugin === null) {
      throw new CollectorError('invalid_options', `extraPlugins[${index}].plugin is required`)
    }
    return { plugin: record.plugin, config: record.config }
  })
}

/**
 * Validate and materialise one collector configuration.
 *
 * The P1a group-analysis fields are forced after caller options; no caller can
 * enable automatic analysis, tools, persona analysis, or all-group listening.
 */
export function resolveCollectorOptions(options: AnalysisCollectorOptions): ResolvedCollectorOptions {
  if (!options || typeof options !== 'object') {
    throw new CollectorError('invalid_options', 'collector options are required')
  }
  const stateDir = resolve(requireText(options.stateDir, 'stateDir'))
  const selfId = requireText(options.selfId, 'selfId')
  if (!/^\d+$/.test(selfId)) {
    throw new CollectorError('invalid_binding', 'selfId must be a decimal QQ id string')
  }
  const platform =
    options.platform === undefined ? 'onebot' : requireText(options.platform, 'platform')
  const listenerGroups = normalizeListenerGroups(options.listenerGroups, selfId, platform)
  const startupTimeoutMs = requirePositiveInteger(options.startupTimeoutMs, 'startupTimeoutMs', 30_000)
  const pollIntervalMs = requirePositiveInteger(options.pollIntervalMs, 'pollIntervalMs', 25)

  if (options.puppeteerPlugin === null) {
    throw new CollectorError(
      'invalid_options',
      'puppeteerPlugin cannot be null: the real koishi-plugin-puppeteer service is mandatory in P1a'
    )
  }

  const httpPlugin = loadDefaultPlugin('@koishijs/plugin-http')
  const chatlunaPlugin =
    options.chatlunaPlugin === undefined ? loadDefaultPlugin('koishi-plugin-chatluna') : options.chatlunaPlugin
  const puppeteerPlugin =
    options.puppeteerPlugin === undefined
      ? loadDefaultPlugin('koishi-plugin-puppeteer')
      : options.puppeteerPlugin
  const analysisPlugin =
    options.analysisPlugin === undefined
      ? loadDefaultPlugin('koishi-plugin-chatluna-group-analysis')
      : options.analysisPlugin

  const databasePlugin =
    options.databasePlugin === undefined ? undefined : options.databasePlugin
  const pluginModes: CollectorPluginModes = {
    database:
      databasePlugin === undefined
        ? 'character-durable-file'
        : databasePlugin === null
          ? 'disabled'
          : 'injected',
    chatluna:
      options.chatlunaPlugin === undefined
        ? 'real'
        : options.chatlunaPlugin === null
          ? 'disabled'
          : 'injected',
    puppeteer: options.puppeteerPlugin === undefined ? 'real' : 'injected',
    analysis:
      options.analysisPlugin === undefined
        ? 'real'
        : options.analysisPlugin === null
          ? 'disabled'
          : 'injected'
  }
  return {
    stateDir,
    databaseFile: resolve(stateDir, 'database', `analysis-${selfId}.json`),
    appDir: resolve(stateDir, 'app'),
    selfId,
    platform,
    listenerGroups,
    startupTimeoutMs,
    pollIntervalMs,
    databasePlugin: databasePlugin === undefined ? null : databasePlugin,
    databaseConfig: options.databaseConfig,
    httpPlugin,
    httpConfig: options.httpConfig,
    chatlunaPlugin,
    chatlunaConfig: asRecord(options.chatlunaConfig),
    puppeteerPlugin,
    puppeteerConfig: options.puppeteerConfig,
    analysisPlugin,
    analysisConfig: asRecord(options.analysisConfig),
    sourceManifestFile: options.sourceManifestFile ? resolve(requireText(options.sourceManifestFile, 'sourceManifestFile')) : undefined,
    extraPlugins: normalizeExtraPlugins(options.extraPlugins),
    pluginModes,
    logger: options.logger
  }
}

export function buildForcedGroupAnalysisConfig(
  options: ResolvedCollectorOptions
): CollectorForcedGroupAnalysisConfig {
  const forced: CollectorForcedGroupAnalysisConfig = {
    cronSchedule: '',
    registerTools: false,
    userTitleAnalysis: false,
    personaAnalysisMessageInterval: 0,
    enableAllGroupsByDefault: false,
    outputFormat: 'text',
    alwaysPersistMessages: true,
    listenerGroups: options.listenerGroups
  }
  return forced
}

export function buildGroupAnalysisConfig(options: ResolvedCollectorOptions): Record<string, unknown> {
  // Caller fields such as filters/retention may be useful for fixtures, but the
  // P1a switches are applied last and cannot be overridden.
  return {
    ...options.analysisConfig,
    ...buildForcedGroupAnalysisConfig(options)
  }
}

export function buildForcedChatLunaConfig(): CollectorForcedChatLunaConfig {
  return {
    botNames: [],
    isNickname: false,
    isNickNameWithContent: false,
    allowPrivate: false,
    allowAtReply: false,
    allowQuoteReply: false,
    privateChatWithoutCommand: false,
    randomReplyFrequency: 0,
    defaultModel: '无',
    enablePresetKeywordTrigger: false
  }
}

export function buildChatLunaConfig(options: ResolvedCollectorOptions): Record<string, unknown> {
  // ChatLuna config is defence in depth only. The dispatch middleware is the
  // actual boundary that prevents model invocation, because upstream has no
  // supported "collection only" switch.
  return {
    ...options.chatlunaConfig,
    ...buildForcedChatLunaConfig(),
    messageQueue: false,
    agentTaskAutoWakeup: false,
    sendThinkingMessage: false
  }
}

export function buildCapabilityMatrix(): CollectorCapabilityRow[] {
  return [
    {
      capability: 'collect_group_messages',
      status: 'enabled',
      upstreamSwitch: null,
      reason: 'explicit listenerGroups persist matching message events'
    },
    {
      capability: 'persist_group_messages',
      status: 'enabled',
      upstreamSwitch: null,
      reason: 'alwaysPersistMessages=true and the independent durable database'
    },
    {
      capability: 'automatic_analysis_schedule',
      status: 'disabled',
      upstreamSwitch: true,
      reason: 'forced cronSchedule is empty'
    },
    {
      capability: 'analysis_tools',
      status: 'disabled',
      upstreamSwitch: true,
      reason: 'forced registerTools=false'
    },
    {
      capability: 'automatic_persona_analysis',
      status: 'disabled',
      upstreamSwitch: true,
      reason: 'forced personaAnalysisMessageInterval=0'
    },
    {
      capability: 'group_analysis_command',
      status: 'denied',
      upstreamSwitch: false,
      reason: 'upstream registers the command unconditionally; prepended dispatch guard denies it'
    },
    {
      capability: 'persona_manual_command',
      status: 'denied',
      upstreamSwitch: false,
      reason: 'upstream registers 用户画像 unconditionally; prepended dispatch guard raises an explicit boundary error'
    },
    {
      capability: 'prompt_variable_group_message_fetch',
      status: 'denied',
      upstreamSwitch: false,
      reason: 'upstream registers the prompt function unconditionally; P1a model execution is denied'
    },
    {
      capability: 'prompt_variable_group_user_persona',
      status: 'denied',
      upstreamSwitch: false,
      reason: 'upstream registers the prompt function unconditionally; P1a model execution is denied'
    },
    {
      capability: 'golden_quote_analysis',
      status: 'denied',
      upstreamSwitch: false,
      reason: 'golden quote analysis has no dedicated upstream switch; all analysis execution is denied in P1a'
    },
    {
      capability: 'tool_split',
      status: 'denied',
      upstreamSwitch: false,
      reason: 'upstream tool split is not configurable; registerTools controls whether the combined tools are registered at all'
    },
    {
      capability: 'manual_analysis_service',
      status: 'denied',
      upstreamSwitch: false,
      reason: 'the collector exposes no analysis entry; direct upstream service calls are outside the boundary'
    },
    {
      capability: 'model_execution',
      status: 'denied',
      upstreamSwitch: false,
      reason: 'no model/provider is configured and all dispatch that could reach a model is blocked'
    },
    {
      capability: 'report_render',
      status: 'denied',
      upstreamSwitch: false,
      reason: 'no analysis/report path is reachable; outputFormat is forced to text for defence in depth'
    },
    {
      capability: 'outbound_send',
      status: 'denied',
      upstreamSwitch: false,
      reason: 'all bot send methods and adapter internal access are denied'
    },
    {
      capability: 'adapter',
      status: 'not_loaded',
      upstreamSwitch: null,
      reason: 'P1a installs no adapter; the caller emits fixture events on the App'
    },
    {
      capability: 'p1b_analysis_execution',
      status: 'not_implemented',
      upstreamSwitch: null,
      reason: 'P1b is a gate only in this task and is not implemented'
    },
    {
      capability: 'p1c_report_execution',
      status: 'not_implemented',
      upstreamSwitch: null,
      reason: 'P1c is a gate only in this task and is not implemented'
    }
  ]
}

export function buildLimitations(): string[] {
  return [
    'Upstream koishi-plugin-chatluna-group-analysis 1.4.1 has no configuration switch that disables its manual 群分析/group-analysis/用户画像 commands or its group_message_fetch/group_user_persona prompt-variable providers. The collector denies command dispatch and model execution at the App boundary; this is a boundary limitation, not an upstream switch.',
    'No upstream package is forked, patched, or monkeypatched. A caller that directly retains and invokes an upstream service method bypasses this collector boundary and is outside P1a.',
    'P1a starts no QQ adapter. The caller must emit fixture events on runtime.app with the single bound selfId and platform; events for another selfId are ignored.',
    'P1b analysis execution and P1c report execution are not implemented. Their matrix rows are gates only.',
    'The real ChatLuna plugin starts with no model, no provider, and all conversation triggers forced off. This is defence in depth, not a guarantee about future upstream versions.',
    'Puppeteer is loaded because group-analysis declares it as a required service; report rendering is still denied and no report path is exposed.'
  ]
}

export function createBaseDiagnostics(
  options: ResolvedCollectorOptions,
  plugins: CollectorPluginModes,
  startedAtMs: number
): CollectorDiagnostics {
  const services = {} as CollectorDiagnostics['services']
  for (const name of SERVICE_ORDER) {
    const meta = REQUIRED_SERVICES[name]
    services[name] = {
      name,
      required: true,
      property: meta.property,
      injected: false,
      status: 'missing',
      plugin: meta.plugin,
      waitedMs: 0
    }
  }
  return {
    phase: 'P1a',
    collectionOnly: true,
    stateDir: options.stateDir,
    databaseFile: options.databaseFile,
    selfId: options.selfId,
    platform: options.platform,
    startedAt: new Date(startedAtMs).toISOString(),
    readyAt: null,
    startupDurationMs: 0,
    startupTimeoutMs: options.startupTimeoutMs,
    pollIntervalMs: options.pollIntervalMs,
    pinnedVersions: PINNED_VERSIONS,
    plugins,
    services,
    injection: {
      required: [...SERVICE_ORDER],
      complete: false,
      missing: [...SERVICE_ORDER],
      timeoutMs: options.startupTimeoutMs,
      waitedMs: 0
    },
    forcedConfig: {
      groupAnalysis: buildForcedGroupAnalysisConfig(options),
      chatLuna: buildForcedChatLunaConfig()
    },
    upstream: {
      groupAnalysisCommands: {
        registered: true,
        switchable: false,
        p1a: 'dispatch_denied'
      },
      personaManualCommand: {
        registered: true,
        switchable: false,
        p1a: 'explicit_boundary_error'
      },
      promptVariables: {
        registered: true,
        switchable: false,
        variables: ['group_message_fetch', 'group_user_persona'],
        p1a: 'model_execution_denied'
      },
      manualAnalysisService: {
        present: true,
        switchable: false,
        p1a: 'no_collector_entry'
      }
    },
    safeguards: {
      outboundSend: 'denied',
      modelExecution: 'denied',
      analysisExecution: 'denied',
      reportRendering: 'denied',
      commandDispatch: 'denied',
      adapter: 'none'
    },
    adapter: {
      loaded: false,
      botsObserved: []
    },
    matrix: buildCapabilityMatrix(),
    limitations: buildLimitations(),
    boundary: {
      dispatchGuard: 'prepended_app_middleware',
      outboundGuard: 'all_bot_send_and_internal_paths',
      dispatchBlocked: 0,
      commandsDenied: 0,
      outboundDenied: 0,
      violations: [],
      droppedViolations: 0
    }
  }
}

/**
 * Load the pinned real package by default. Tests may still inject a plugin via
 * options; null is used only for explicit missing-injection failure cases.
 */
function loadDefaultPlugin(specifier: string): unknown {
  try {
    // The pinned packages publish CommonJS entry points.
    const module = require(specifier) as { default?: unknown }
    return module.default ?? module
  } catch (error) {
    throw new CollectorError(
      'plugin_load_failed',
      `cannot load required plugin ${specifier}; install the pinned package in koishi/analysis-collector`,
      { cause: error }
    )
  }
}
