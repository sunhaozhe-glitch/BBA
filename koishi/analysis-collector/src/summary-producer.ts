import { CollectorError } from './errors.js'
import { stableMessageRef } from './runtime.js'
import { PINNED_VERSIONS } from './types.js'
import type {
  AnalysisCollectorRuntime,
  CollectedMessageWindow,
  CollectedMessageWindowOptions,
  StoredSummary,
  SummaryV1
} from './types.js'
import type { SummaryPublisher, SummaryPublishResult } from './summary-publisher.js'
import {
  SEMANTIC_ANALYSIS_SCHEMA_ID,
  assertSemanticPrivacy,
  observedActivityFor,
  semanticLimitations,
  semanticTopics,
  summaryExpiry,
  validateSemanticAnalysis
} from './semantic-analysis.js'
import type {
  SemanticAnalysis,
  SemanticAnalysisInput,
  SemanticAnalyzer,
  SemanticMessage
} from './semantic-analysis.js'

/**
 * Evidence vocabulary shared with the backend Summary store and the Character
 * `GroupContext` prompt. `Summary.v1` itself intentionally has no per-topic
 * evidence field; the producer keeps the classification beside the payload so
 * the publisher can hand it to the backend without rewriting the frozen
 * contract.
 */
export type SummaryEvidenceKind = 'member_statement' | 'deterministic_statistic' | 'model_inference'

export interface ProducedTopic {
  summary: string
  message_refs: string[]
  evidence_kind: SummaryEvidenceKind
}

export interface SummaryProducerInput {
  /** A complete materialized collector window for exactly one account/group. */
  window: CollectedMessageWindow
  now: Date
  privacyClass: SummaryV1['privacy_class']
  retentionDays: number
  pluginVersion: string
  sourceManifestSha256: string
}

export interface SummaryProduction {
  summary: SummaryV1
  /** Evidence annotation aligned 1:1 with `summary.topics`. */
  topics: ProducedTopic[]
  producerKind: 'deterministic' | 'model' | 'hybrid'
  /** Present only for the injectable semantic producer. */
  semantics?: SemanticAnalysis
  /** Explicit degradation notes; never used to hide a failed step. */
  notes: string[]
}

export interface SummaryProducer {
  readonly name: string
  produce(input: SummaryProducerInput): Promise<SummaryProduction>
}

export const DEFAULT_SUMMARY_RETENTION_DAYS = 7
export const SUMMARY_TOPIC_LIMIT = 20
export const SUMMARY_REFS_PER_TOPIC = 500
export const SUMMARY_TOPIC_TEXT_LIMIT = 4000
/** Messages visible to a model adapter per window; bodies never persist. */
export const SUMMARY_MODEL_MESSAGE_LIMIT = 200
export const SUMMARY_MODEL_MESSAGE_TEXT_LIMIT = 500

const DETERMINISTIC_LIMITATIONS = [
  '确定性统计摘要：仅陈述窗口内的消息数量与发言活跃度，不包含语义或话题推断。',
  '引用只指向本账号、本群、本时间窗口内的采集消息；原文删除后引用失效，摘要不得再作为证据。'
]

function truncateCodePoints(value: string, limit: number): string {
  const codePoints = [...value]
  if (codePoints.length <= limit) return value
  return codePoints.slice(0, limit - 1).join('') + '…'
}

function uniqueRefs(refs: readonly string[]): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const ref of refs) {
    if (typeof ref !== 'string' || ref.length === 0 || ref.length > 256 || seen.has(ref)) continue
    seen.add(ref)
    out.push(ref)
    if (out.length === SUMMARY_REFS_PER_TOPIC) break
  }
  return out
}

function topic(summary: string, messageRefs: readonly string[], evidenceKind: SummaryEvidenceKind): ProducedTopic {
  return {
    summary: truncateCodePoints(summary, SUMMARY_TOPIC_TEXT_LIMIT),
    message_refs: uniqueRefs(messageRefs),
    evidence_kind: evidenceKind
  }
}

/**
 * Pure deterministic producer. It cannot be described as a semantic topic
 * summary: every topic is a count/active-speaker statistic derived from the
 * collector's own window statistics, and every topic cites window references.
 */
export function createDeterministicSummaryProducer(): SummaryProducer {
  return {
    name: 'deterministic-statistics',
    async produce(input: SummaryProducerInput): Promise<SummaryProduction> {
      const { window } = input
      const topics: ProducedTopic[] = []
      const total = window.statistics.message_count
      const speakerCount = window.statistics.unique_speaker_count
      topics.push(
        topic(
          `统计：本窗口共采集 ${total} 条消息，来自 ${speakerCount} 位成员。`,
          window.messageRefs,
          'deterministic_statistic'
        )
      )

      // Per-speaker references only exist while the source rows are loaded; a
      // resumed window replays statistics but not message bodies, so the
      // producer must not invent references for those speakers.
      const bySpeaker = new Map<string, string[]>()
      for (const message of window.messages) {
        const ref = stableMessageRef(message)
        if (!ref) continue
        const refs = bySpeaker.get(message.userId) ?? []
        if (refs.length < SUMMARY_REFS_PER_TOPIC) refs.push(ref)
        bySpeaker.set(message.userId, refs)
      }
      for (const row of window.statistics.messages_per_speaker.slice(0, 8)) {
        if (topics.length >= SUMMARY_TOPIC_LIMIT) break
        const refs = bySpeaker.get(row.user_id) ?? []
        if (refs.length === 0) continue
        topics.push(
          topic(
            `统计：成员 ${row.user_id} 在本窗口发言 ${row.count} 条。`,
            refs,
            'deterministic_statistic'
          )
        )
      }
      if (window.statistics.most_active_speaker_id && topics.length < SUMMARY_TOPIC_LIMIT) {
        const mostActive = window.statistics.messages_per_speaker[0]
        if (mostActive && (bySpeaker.get(mostActive.user_id)?.length ?? 0) > 0) {
          topics.push(
            topic(
              `统计：本窗口最活跃成员为 ${mostActive.user_id}，共 ${mostActive.count} 条消息。`,
              bySpeaker.get(mostActive.user_id) ?? [],
              'deterministic_statistic'
            )
          )
        }
      }

      if (topics.some((entry) => entry.message_refs.length === 0)) {
        throw new CollectorError(
          'summary_reference_invalid',
          'every produced topic must cite at least one stable message reference inside the Summary bounds'
        )
      }

      return {
        summary: buildSummaryV1(input, topics),
        topics,
        producerKind: 'deterministic',
        notes: []
      }
    }
  }
}

function buildSummaryV1(input: SummaryProducerInput, topics: readonly ProducedTopic[]): SummaryV1 {
  const { window, privacyClass, retentionDays, pluginVersion, sourceManifestSha256 } = input
  const scope: SummaryV1['collector_scope'] = {
    account_id: window.accountId,
    conversation_kind: 'group',
    conversation_id: window.guildId
  }
  return {
    schema_id: 'Summary.v1',
    window_start: window.startTime.toISOString(),
    window_end: window.endTime.toISOString(),
    collector_scope: scope,
    privacy_class: privacyClass,
    retention_days: retentionDays,
    expires_at: new Date(window.startTime.getTime() + retentionDays * 86_400_000).toISOString(),
    producer: {
      account_id: scope.account_id,
      collector_scope: { ...scope },
      plugin_version: pluginVersion,
      source_manifest_sha256: sourceManifestSha256
    },
    topics: topics.map((entry) => ({ summary: entry.summary, message_refs: [...entry.message_refs] })),
    limitations: [...DETERMINISTIC_LIMITATIONS]
  }
}

/** One bounded message handed to a model adapter; never persisted. */
export interface SummaryModelMessage {
  ref: string
  userId: string
  content: string
}

export interface SummaryModelRequest {
  accountId: string
  groupId: string
  windowStart: string
  windowEnd: string
  messageCount: number
  messages: SummaryModelMessage[]
  maxTopics: number
}

/**
 * Explicit boundary for topic summarisation. `evidenceSource` must be
 * `'fixture'` for offline doubles so a generated Summary can never be mistaken
 * for a real model result.
 */
export interface SummaryModelAdapter {
  readonly name: string
  readonly evidenceSource: 'fixture' | 'provider'
  complete(request: SummaryModelRequest): Promise<string>
}

/**
 * Offline double. The explicit `fixture` source marker is reported in the
 * analysis result and must not be presented as a production model.
 */
export function createFixtureSummaryModelAdapter(options: {
  topics?: Array<{ summary: string; message_refs: string[] }>
  name?: string
} = {}): SummaryModelAdapter {
  return {
    name: options.name ?? 'fixture-summary-model',
    evidenceSource: 'fixture',
    async complete(request) {
      const fallbackRefs = request.messages.slice(0, 1).map((message) => message.ref)
      const topics = options.topics ?? [
        {
          summary: `Fixture topic summary over ${request.messageCount} messages.`,
          message_refs: fallbackRefs
        }
      ]
      return JSON.stringify({ topics })
    }
  }
}

/**
 * Boundary for an actual model provider. The caller owns the transport; this
 * adapter only keeps the request/response shaping and the provenance marker.
 */
export function createProviderSummaryModelAdapter(options: {
  name: string
  complete: (prompt: string) => Promise<string>
}): SummaryModelAdapter {
  if (!options || typeof options.complete !== 'function') {
    throw new CollectorError('invalid_options', 'a provider summary model requires a completion function')
  }
  return {
    name: options.name,
    evidenceSource: 'provider',
    async complete(request) {
      return await options.complete(renderModelPrompt(request))
    }
  }
}

function renderModelPrompt(request: SummaryModelRequest): string {
  const body = request.messages
    .map((message) => `${message.ref}\t${truncateCodePoints(message.content, SUMMARY_MODEL_MESSAGE_TEXT_LIMIT)}`)
    .join('\n')
  return [
    '你是群聊话题摘要器。只输出 JSON：{"topics":[{"summary":"...","message_refs":["..."]}]}。',
    '每条 message_refs 必须来自输入行首的引用，不得编造；无法从消息文本确认的结论不要输出。',
    `窗口 ${request.windowStart} → ${request.windowEnd}，共 ${request.messageCount} 条消息。`,
    '消息（引用<TAB>内容）：',
    body
  ].join('\n')
}

/**
 * Deterministic statistics plus model topic summaries. Refuses to send message
 * bodies when the Summary is local-only; a degraded run is explicit, never
 * silent.
 */
export function createHybridSummaryProducer(options: {
  model: SummaryModelAdapter
  fallbackToDeterministic?: boolean
}): SummaryProducer {
  const deterministic = createDeterministicSummaryProducer()
  return {
    name: `hybrid:${options.model.name}`,
    async produce(input) {
      const base = await deterministic.produce(input)
      if (input.privacyClass !== 'redacted_external_allowed') {
        throw new CollectorError(
          'summary_invalid',
          'a model topic summary requires privacy_class=redacted_external_allowed'
        )
      }
      if (input.window.messages.length === 0) {
        throw new CollectorError(
          'summary_invalid',
          'a model topic summary requires collected message bodies in the window'
        )
      }
      const refs = new Set(input.window.messageRefs)
      const messages: SummaryModelMessage[] = []
      for (const message of input.window.messages) {
        if (messages.length >= SUMMARY_MODEL_MESSAGE_LIMIT) break
        const ref = stableMessageRef(message)
        if (!ref || !refs.has(ref)) continue
        messages.push({
          ref,
          userId: message.userId,
          content: truncateCodePoints(message.content, SUMMARY_MODEL_MESSAGE_TEXT_LIMIT)
        })
      }
      if (messages.length === 0) {
        throw new CollectorError('summary_reference_invalid', 'no window message resolves to a stable reference')
      }
      const notes: string[] = []
      let modelTopics: ProducedTopic[]
      try {
        const raw = await options.model.complete({
          accountId: input.window.accountId,
          groupId: input.window.guildId,
          windowStart: input.window.startTime.toISOString(),
          windowEnd: input.window.endTime.toISOString(),
          messageCount: input.window.statistics.message_count,
          messages,
          maxTopics: SUMMARY_TOPIC_LIMIT - base.topics.length
        })
        modelTopics = parseModelTopics(raw, refs, SUMMARY_TOPIC_LIMIT - base.topics.length)
      } catch (error) {
        if (!options.fallbackToDeterministic) throw error
        notes.push(
          `模型话题摘要失败，本版本仅保留确定性统计：${
            error instanceof Error ? error.message : String(error)
          }`
        )
        return {
          ...base,
          producerKind: 'deterministic',
          notes: [...base.notes, ...notes]
        }
      }
      if (options.model.evidenceSource === 'fixture') {
        notes.push('模型适配器为显式fixture，本次话题摘要不是真实模型产物。')
      }
      const topics = [...base.topics, ...modelTopics].slice(0, SUMMARY_TOPIC_LIMIT)
      const summary = buildSummaryV1(input, topics)
      summary.limitations = [
        ...DETERMINISTIC_LIMITATIONS,
        `话题摘要来自 ${options.model.name}（${
          options.model.evidenceSource === 'fixture' ? 'fixture模型，非真实模型产物' : 'provider'
        }），属于不可信引用数据，不得升级为确定事实。`
      ]
      return {
        summary,
        topics,
        producerKind: 'hybrid',
        notes
      }
    }
  }
}

/**
 * Deterministic statistics plus an injectable semantic analyzer.
 *
 * The analyzer only proposes typed claims. Every claim is validated against
 * the exact window, keeps observed/inferred classification, carries its own
 * confidence, and expires no later than the Summary. Provider failures are
 * surfaced unless the caller explicitly opts into deterministic degradation.
 */
export function createSemanticSummaryProducer(options: {
  analyzer: SemanticAnalyzer
  fallbackToDeterministic?: boolean
}): SummaryProducer {
  if (!options?.analyzer) {
    throw new CollectorError('invalid_options', 'a semantic summary producer requires an analyzer')
  }
  const deterministic = createDeterministicSummaryProducer()
  return {
    name: `semantic:${options.analyzer.name}`,
    async produce(input) {
      const base = await deterministic.produce(input)
      const notes: string[] = []
      if (input.window.messages.length === 0) {
        throw new CollectorError(
          'summary_invalid',
          'a semantic analysis requires collected message bodies in the window'
        )
      }
      const refs = new Set(input.window.messageRefs)
      const messages: SemanticMessage[] = []
      for (const message of input.window.messages) {
        if (messages.length >= SUMMARY_MODEL_MESSAGE_LIMIT) break
        const ref = stableMessageRef(message)
        if (!ref || !refs.has(ref)) continue
        messages.push({
          ref,
          userId: message.userId,
          content: truncateCodePoints(message.content, SUMMARY_MODEL_MESSAGE_TEXT_LIMIT),
          at: message.timestamp.toISOString()
        })
      }
      if (messages.length === 0) {
        throw new CollectorError('summary_reference_invalid', 'no window message resolves to a stable reference')
      }
      const expiresAt = summaryExpiry(input.window, input.retentionDays)
      const analysisInput: SemanticAnalysisInput = {
        accountId: input.window.accountId,
        groupId: input.window.guildId,
        windowStart: input.window.startTime.toISOString(),
        windowEnd: input.window.endTime.toISOString(),
        messageCount: input.window.statistics.message_count,
        messages,
        summaryExpiresAt: expiresAt.toISOString(),
        now: input.now
      }
      let raw: SemanticAnalysis
      try {
        raw = await options.analyzer.analyze(analysisInput)
      } catch (error) {
        if (!options.fallbackToDeterministic) throw error
        notes.push(
          `语义分析失败，本版本仅保留确定性统计：${error instanceof Error ? error.message : String(error)}`
        )
        return { ...base, producerKind: 'deterministic', notes: [...base.notes, ...notes] }
      }
      if (
        raw?.evidence_source !== options.analyzer.evidenceSource ||
        raw?.provider !== options.analyzer.name
      ) {
        throw new CollectorError(
          'summary_invalid',
          'semantic analysis provenance does not match the configured analyzer'
        )
      }
      // The analyzer owns the inferred activity; the observed block is always
      // derived from the collector window and cannot be widened.
      raw.observed_activity = observedActivityFor(analysisInput)
      if (raw.schema_id !== SEMANTIC_ANALYSIS_SCHEMA_ID) {
        throw new CollectorError('summary_invalid', 'semantic analysis schema id is not SemanticAnalysis.v1')
      }
      const analysis = validateSemanticAnalysis(raw, analysisInput)
      assertSemanticPrivacy(input.privacyClass, analysis)
      if (options.analyzer.evidenceSource === 'fixture') {
        notes.push('语义分析提供者为显式fixture，本次推断不是真实模型产物。')
      }
      const semantic = semanticTopics(analysis)
      // Always keep the deterministic baseline; semantic claims fill the rest
      // of the contract's 20-topic bound. Truncation is explicit.
      const capacity = SUMMARY_TOPIC_LIMIT - base.topics.length
      const topics = [...base.topics, ...semantic.slice(0, capacity)]
      if (semantic.length > capacity) {
        notes.push(`语义条目 ${semantic.length} 条超出 Summary 容量，已截断 ${semantic.length - capacity} 条。`)
      }
      const summary = buildSummaryV1(input, topics)
      summary.limitations = [
        ...DETERMINISTIC_LIMITATIONS,
        ...semanticLimitations(analysis)
      ].slice(0, 20)
      return {
        summary,
        topics,
        producerKind: 'hybrid',
        semantics: analysis,
        notes
      }
    }
  }
}

function parseModelTopics(raw: string, allowedRefs: ReadonlySet<string>, maxTopics: number): ProducedTopic[] {
  if (typeof raw !== 'string' || raw.trim().length === 0) {
    throw new CollectorError('summary_invalid', 'model adapter returned an empty response')
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch (error) {
    throw new CollectorError('summary_invalid', 'model adapter returned non-JSON output', { cause: error })
  }
  const topics = (parsed as { topics?: unknown } | null)?.topics
  if (!Array.isArray(topics) || topics.length === 0 || topics.length > maxTopics) {
    throw new CollectorError('summary_invalid', 'model adapter output must contain 1..maxTopics topics')
  }
  const out: ProducedTopic[] = []
  for (const entry of topics) {
    const record = entry as { summary?: unknown; message_refs?: unknown } | null
    if (
      !record ||
      typeof record.summary !== 'string' ||
      record.summary.trim().length === 0 ||
      !Array.isArray(record.message_refs) ||
      record.message_refs.length === 0
    ) {
      throw new CollectorError('summary_invalid', 'model topic is missing a summary or references')
    }
    const refs = record.message_refs
    if (refs.some((ref) => typeof ref !== 'string' || !allowedRefs.has(ref))) {
      throw new CollectorError(
        'summary_reference_invalid',
        'model topic reference is outside the collected window'
      )
    }
    out.push(topic(record.summary, refs as string[], 'model_inference'))
  }
  return out
}

export interface SummaryAnalysisJobOptions {
  runtime: Pick<AnalysisCollectorRuntime, 'sourceManifestSha256'> & {
    readCollectedMessageWindow(options: CollectedMessageWindowOptions): Promise<CollectedMessageWindow>
    saveSummary(windowId: string, summary: SummaryV1): Promise<StoredSummary>
    saveSemanticAnalysis?(summaryId: string, analysis: SemanticAnalysis): Promise<void>
  }
  /**
   * A complete materialized window may be supplied by a caller that already
   * read it (for example a duplicate-window replay); otherwise the job reads
   * the persisted window through the collector owner token.
   */
  window: CollectedMessageWindowOptions | CollectedMessageWindow
  producer: SummaryProducer
  publisher?: SummaryPublisher
  now?: Date
  privacyClass?: SummaryV1['privacy_class']
  retentionDays?: number
  pluginVersion?: string
}

export type SummaryAnalysisResult =
  | { status: 'empty'; windowId: string; messageCount: number }
  | { status: 'retention_expired'; windowId: string; expiresAt: string }
  | {
      status: 'stored'
      windowId: string
      summaryId: string
      generatedAt: string
      expiresAt: string
      producerKind: SummaryProduction['producerKind']
      topicEvidence: SummaryEvidenceKind[]
      notes: string[]
      published: SummaryPublishResult | null
      /** Validated semantic claims, or null for a deterministic Summary. */
      semantics: SemanticAnalysis | null
      semanticStored: boolean
      /** True when the semantic analyzer was an explicit offline fixture. */
      semanticFixture: boolean
    }

function isMaterializedWindow(value: SummaryAnalysisJobOptions['window']): value is CollectedMessageWindow {
  return Boolean(value && typeof value === 'object' && 'complete' in value && 'statistics' in value)
}

/**
 * Real analysis job: read one complete persisted window, produce a Summary,
 * and hand it to the collector's own `saveSummary` validation path. A caller
 * that supplies a publisher then forwards the stored Summary to the backend.
 * Semantic claims are persisted beside the Summary only after the Summary's
 * own window/reference/owner validation succeeds.
 */
export async function runSummaryAnalysisJob(options: SummaryAnalysisJobOptions): Promise<SummaryAnalysisResult> {
  const now = options.now ?? new Date()
  const retentionDays = options.retentionDays ?? DEFAULT_SUMMARY_RETENTION_DAYS
  if (!Number.isInteger(retentionDays) || retentionDays < 1 || retentionDays > 365) {
    throw new CollectorError('invalid_options', 'summary retention must be 1..365 days')
  }
  const window = isMaterializedWindow(options.window)
    ? options.window
    : await options.runtime.readCollectedMessageWindow(options.window)
  if (!window.complete) {
    throw new CollectorError('summary_invalid', 'summary requires a complete collected window')
  }
  if (window.statistics.message_count === 0) {
    return { status: 'empty', windowId: window.id, messageCount: 0 }
  }
  const expiresAt = new Date(window.startTime.getTime() + retentionDays * 86_400_000)
  if (expiresAt.getTime() <= now.getTime()) {
    return { status: 'retention_expired', windowId: window.id, expiresAt: expiresAt.toISOString() }
  }
  const production = await options.producer.produce({
    window,
    now,
    privacyClass: options.privacyClass ?? 'local_only',
    retentionDays,
    pluginVersion: options.pluginVersion ?? PINNED_VERSIONS.analysis,
    sourceManifestSha256: options.runtime.sourceManifestSha256
  })
  if (production.topics.length !== production.summary.topics.length) {
    throw new CollectorError('summary_invalid', 'producer topic evidence does not align with Summary topics')
  }
  const stored = await options.runtime.saveSummary(window.id, production.summary)
  let semanticStored = false
  if (production.semantics) {
    if (!options.runtime.saveSemanticAnalysis) {
      throw new CollectorError(
        'summary_invalid',
        'the collector runtime cannot persist semantic analysis beside the Summary'
      )
    }
    await options.runtime.saveSemanticAnalysis(stored.id, production.semantics)
    semanticStored = true
  }
  const published = options.publisher
    ? await options.publisher.publish({ stored, window, topics: production.topics, generatedAt: now })
    : null
  return {
    status: 'stored',
    windowId: window.id,
    summaryId: stored.id,
    generatedAt: now.toISOString(),
    expiresAt: stored.expiresAt.toISOString(),
    producerKind: production.producerKind,
    topicEvidence: production.topics.map((entry) => entry.evidence_kind),
    notes: production.notes,
    published,
    semantics: production.semantics ?? null,
    semanticStored,
    semanticFixture: production.semantics?.evidence_source === 'fixture'
  }
}
