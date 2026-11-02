import { CollectorError } from './errors.js'
import type { CollectedMessageWindow, SummaryV1 } from './types.js'

/**
 * Grounded semantic group analysis.
 *
 * The deterministic statistics producer remains the baseline. A semantic
 * provider only adds typed claims that must cite stable references from the
 * same collected window; every claim distinguishes an observed member
 * statement from an inference, carries a self-reported confidence, and expires
 * with the Summary. Nothing here can authorize a send: the Character permit
 * gate remains the sole authority.
 */
export const SEMANTIC_ANALYSIS_SCHEMA_ID = 'SemanticAnalysis.v1' as const

export type SemanticClaimKind = 'activity' | 'topic' | 'rule' | 'pacing' | 'clarification'
export type SemanticBasis = 'observed' | 'inferred'
export type SemanticEvidenceSource = 'fixture' | 'provider'

export interface SemanticClaim {
  kind: SemanticClaimKind
  statement: string
  basis: SemanticBasis
  /** Provider-self-reported confidence in 0..1. Never treated as a verified fact. */
  confidence: number
  /** Stable references inside the same account/group/window; never empty. */
  message_refs: string[]
}

export interface SemanticActivityWindow {
  start: string
  end: string
  message_count: number
  basis: SemanticBasis
  confidence: number
  message_refs: string[]
}

export interface SemanticAnalysis {
  schema_id: typeof SEMANTIC_ANALYSIS_SCHEMA_ID
  provider: string
  model: string | null
  evidence_source: SemanticEvidenceSource
  generated_at: string
  /** Semantic claims never outlive the Summary that carries them. */
  expires_at: string
  window: {
    start: string
    end: string
    message_count: number
  }
  /** Exactly the collected window; the provider cannot widen it. */
  observed_activity: SemanticActivityWindow
  inferred_activity: SemanticActivityWindow[]
  topics: SemanticClaim[]
  rules: SemanticClaim[]
  pacing: SemanticClaim[]
  clarification: SemanticClaim[]
  limitations: string[]
  /** Explicit marker consumed by reports and prompts: data only. */
  execution_authority: 'none'
}

export interface SemanticMessage {
  ref: string
  userId: string
  content: string
  /** ISO timestamp used only to derive bounded activity buckets. */
  at: string
}

export interface SemanticAnalysisInput {
  accountId: string
  groupId: string
  windowStart: string
  windowEnd: string
  messageCount: number
  messages: SemanticMessage[]
  /** The Summary expiry; a provider may not extend claims past it. */
  summaryExpiresAt: string
  now: Date
}

export interface SemanticAnalyzer {
  readonly name: string
  readonly model?: string
  /** `fixture` results are explicitly labelled and never presented as real quality. */
  readonly evidenceSource: SemanticEvidenceSource
  analyze(input: SemanticAnalysisInput): Promise<SemanticAnalysis>
}

const MAX_STATEMENT_CHARS = 500
const MAX_REFS_PER_CLAIM = 20
const MAX_CLAIMS_PER_KIND = 6
const MAX_INFERRED_ACTIVITY = 3
const MAX_LIMITATIONS = 10
const MAX_LIMITATION_CHARS = 500
const MAX_FUTURE_SKEW_MS = 5 * 60 * 1000
const CLAIM_KINDS: readonly SemanticClaimKind[] = ['activity', 'topic', 'rule', 'pacing', 'clarification']

function isIsoDate(value: unknown): value is string {
  return typeof value === 'string' && Number.isFinite(Date.parse(value))
}

function validStatement(value: string): boolean {
  if (typeof value !== 'string') return false
  const chars = [...value]
  if (chars.length === 0 || chars.length > MAX_STATEMENT_CHARS) return false
  return !chars.some((char) => char.charCodeAt(0) < 0x20 && char !== '\n' && char !== '\t')
}

function validRefs(value: unknown, allowed: ReadonlySet<string>): value is string[] {
  return (
    Array.isArray(value) &&
    value.length >= 1 &&
    value.length <= MAX_REFS_PER_CLAIM &&
    value.every((ref) => typeof ref === 'string' && ref.length > 0 && ref.length <= 256 && allowed.has(ref))
  )
}

function validConfidence(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1
}

function validClaim(value: unknown, allowed: ReadonlySet<string>): value is SemanticClaim {
  if (!value || typeof value !== 'object') return false
  const claim = value as Partial<SemanticClaim>
  return (
    typeof claim.kind === 'string' &&
    CLAIM_KINDS.includes(claim.kind as SemanticClaimKind) &&
    claim.kind !== 'activity' &&
    typeof claim.statement === 'string' &&
    validStatement(claim.statement) &&
    (claim.basis === 'observed' || claim.basis === 'inferred') &&
    validConfidence(claim.confidence) &&
    validRefs(claim.message_refs, allowed)
  )
}

function validActivity(
  value: unknown,
  allowed: ReadonlySet<string>,
  windowStart: number,
  windowEnd: number
): value is SemanticActivityWindow {
  if (!value || typeof value !== 'object') return false
  const activity = value as Partial<SemanticActivityWindow>
  return Boolean(
    isIsoDate(activity.start) &&
      isIsoDate(activity.end) &&
      Date.parse(activity.start) >= windowStart &&
      Date.parse(activity.end) <= windowEnd &&
      Date.parse(activity.start) <= Date.parse(activity.end) &&
      Number.isInteger(activity.message_count) &&
      (activity.message_count as number) >= 0 &&
      (activity.basis === 'observed' || activity.basis === 'inferred') &&
      validConfidence(activity.confidence) &&
      validRefs(activity.message_refs, allowed)
  )
}

function uniqueRefs(refs: readonly string[]): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const ref of refs) {
    if (seen.has(ref)) continue
    seen.add(ref)
    out.push(ref)
    if (out.length === MAX_REFS_PER_CLAIM) break
  }
  return out
}

/**
 * Validate an analyzer result against the exact window that produced it.
 *
 * A provider is untrusted: out-of-window references, widened activity bounds,
 * unbounded/empty statements, missing classifications, or an expiry beyond the
 * Summary are rejected instead of being repaired.
 */
export function validateSemanticAnalysis(
  value: unknown,
  input: SemanticAnalysisInput
): SemanticAnalysis {
  if (!value || typeof value !== 'object') {
    throw new CollectorError('summary_invalid', 'semantic analysis must be an object')
  }
  const analysis = value as Partial<SemanticAnalysis>
  const now = input.now.getTime()
  const windowStart = Date.parse(input.windowStart)
  const windowEnd = Date.parse(input.windowEnd)
  const summaryExpires = Date.parse(input.summaryExpiresAt)
  if (
    analysis.schema_id !== SEMANTIC_ANALYSIS_SCHEMA_ID ||
    typeof analysis.provider !== 'string' ||
    !validStatement(analysis.provider) ||
    analysis.provider.length > 128 ||
    (analysis.model != null && (typeof analysis.model !== 'string' || analysis.model.length < 1 || analysis.model.length > 128)) ||
    (analysis.evidence_source !== 'fixture' && analysis.evidence_source !== 'provider') ||
    !isIsoDate(analysis.generated_at) ||
    !isIsoDate(analysis.expires_at) ||
    Date.parse(analysis.generated_at) > now + MAX_FUTURE_SKEW_MS ||
    Date.parse(analysis.expires_at) <= now ||
    Date.parse(analysis.expires_at) > summaryExpires ||
    analysis.execution_authority !== 'none'
  ) {
    throw new CollectorError('summary_invalid', 'semantic analysis envelope is invalid or outlives its Summary')
  }
  if (
    !analysis.window ||
    !isIsoDate(analysis.window.start) ||
    !isIsoDate(analysis.window.end) ||
    Date.parse(analysis.window.start) !== windowStart ||
    Date.parse(analysis.window.end) !== windowEnd ||
    analysis.window.message_count !== input.messageCount
  ) {
    throw new CollectorError('summary_invalid', 'semantic analysis window does not match the collected window')
  }
  const allowed = new Set(input.messages.map((message) => message.ref))
  if (
    !validActivity(analysis.observed_activity, allowed, windowStart, windowEnd) ||
    analysis.observed_activity?.basis !== 'observed' ||
    Date.parse(analysis.observed_activity.start) !== windowStart ||
    Date.parse(analysis.observed_activity.end) !== windowEnd ||
    analysis.observed_activity?.message_count !== input.messageCount
  ) {
    throw new CollectorError('summary_invalid', 'observed activity must be exactly the collected window')
  }
  if (
    !Array.isArray(analysis.inferred_activity) ||
    analysis.inferred_activity.length > MAX_INFERRED_ACTIVITY ||
    !analysis.inferred_activity.every(
      (entry) =>
        validActivity(entry, allowed, windowStart, windowEnd) &&
        entry.basis === 'inferred' &&
        entry.confidence <= 1
    )
  ) {
    throw new CollectorError('summary_invalid', 'inferred activity is malformed or outside the window')
  }
  for (const key of ['topics', 'rules', 'pacing', 'clarification'] as const) {
    const claims = analysis[key]
    if (
      !Array.isArray(claims) ||
      claims.length > MAX_CLAIMS_PER_KIND ||
      !claims.every((claim) => validClaim(claim, allowed))
    ) {
      throw new CollectorError('summary_invalid', `semantic ${key} claims are malformed or cite outside the window`)
    }
  }
  if (
    !Array.isArray(analysis.limitations) ||
    analysis.limitations.length < 1 ||
    analysis.limitations.length > MAX_LIMITATIONS ||
    !analysis.limitations.every(
      (entry) => typeof entry === 'string' && entry.length > 0 && entry.length <= MAX_LIMITATION_CHARS
    )
  ) {
    throw new CollectorError('summary_invalid', 'semantic limitations must be 1..10 bounded strings')
  }
  // The loop above validates all four arrays, but indexing by a dynamic key
  // does not retain that narrowing on the optional properties.
  const claims: SemanticClaim[] = [
    ...analysis.topics!,
    ...analysis.rules!,
    ...analysis.pacing!,
    ...analysis.clarification!
  ]
  for (const claim of claims) {
    claim.message_refs = uniqueRefs(claim.message_refs)
  }
  for (const activity of analysis.inferred_activity) {
    activity.message_refs = uniqueRefs(activity.message_refs)
  }
  analysis.observed_activity.message_refs = uniqueRefs(analysis.observed_activity.message_refs)
  return analysis as SemanticAnalysis
}

/**
 * Build the exact observed activity block from the collector window. Providers
 * cannot substitute a wider or invented observed window.
 */
export function observedActivityFor(input: SemanticAnalysisInput): SemanticActivityWindow {
  return {
    start: input.windowStart,
    end: input.windowEnd,
    message_count: input.messageCount,
    basis: 'observed',
    confidence: 1,
    message_refs: uniqueRefs(input.messages.map((message) => message.ref))
  }
}

export interface FixtureSemanticAnalyzerOptions {
  name?: string
  model?: string
  /** Explicit offline analysis. It must only cite refs present in the input. */
  analyze: (input: SemanticAnalysisInput) => Promise<SemanticAnalysis> | SemanticAnalysis
}

/**
 * Offline double for the semantic provider boundary. It is explicitly marked
 * `fixture`, so a Summary can never claim real model semantic quality.
 */
export function createFixtureSemanticAnalyzer(options: FixtureSemanticAnalyzerOptions): SemanticAnalyzer {
  if (!options || typeof options.analyze !== 'function') {
    throw new CollectorError('invalid_options', 'a fixture semantic analyzer requires an analyze function')
  }
  return {
    name: options.name ?? 'fixture-semantic-analyzer',
    model: options.model,
    evidenceSource: 'fixture',
    async analyze(input) {
      return await options.analyze(input)
    }
  }
}

export interface ProviderSemanticAnalyzerOptions {
  name: string
  model?: string
  /** The caller owns the real transport; this boundary only shapes/validates. */
  analyze: (input: SemanticAnalysisInput) => Promise<SemanticAnalysis>
}

/** Boundary for an actual semantic model provider. */
export function createProviderSemanticAnalyzer(options: ProviderSemanticAnalyzerOptions): SemanticAnalyzer {
  if (!options || typeof options.analyze !== 'function' || !options.name) {
    throw new CollectorError('invalid_options', 'a provider semantic analyzer requires a name and analyze function')
  }
  return {
    name: options.name,
    model: options.model,
    evidenceSource: 'provider',
    async analyze(input) {
      return await options.analyze(input)
    }
  }
}

const KIND_LABEL: Record<SemanticClaimKind, string> = {
  activity: '活跃时段',
  topic: '话题',
  rule: '群规',
  pacing: '互动节奏',
  clarification: '待澄清'
}

function renderClaim(kind: SemanticClaimKind, claim: SemanticClaim): string {
  const basis = claim.basis === 'observed' ? '观察' : '推断'
  const evidence = `证据:${claim.message_refs.join(',')}`
  return `[${KIND_LABEL[kind]}｜${basis}｜置信度${claim.confidence.toFixed(2)}] ${claim.statement} ${evidence}`
}

/** Flatten validated claims to `Summary.v1` topic rows with evidence kinds. */
export function semanticTopics(
  analysis: SemanticAnalysis
): Array<{
  summary: string
  message_refs: string[]
  evidence_kind: 'member_statement' | 'deterministic_statistic' | 'model_inference'
}> {
  const topics: Array<{
    summary: string
    message_refs: string[]
    evidence_kind: 'member_statement' | 'deterministic_statistic' | 'model_inference'
  }> = []
  const push = (kind: SemanticClaimKind, claim: SemanticClaim) => {
    topics.push({
      summary: renderClaim(kind, claim),
      message_refs: [...claim.message_refs],
      evidence_kind: claim.basis === 'observed' ? 'member_statement' : 'model_inference'
    })
  }
  for (const activity of analysis.inferred_activity) {
    push('activity', {
      kind: 'activity',
      statement: `${activity.start} → ${activity.end} 共 ${activity.message_count} 条消息`,
      basis: activity.basis,
      confidence: activity.confidence,
      message_refs: activity.message_refs
    })
  }
  for (const claim of analysis.topics) push('topic', claim)
  for (const claim of analysis.rules) push('rule', claim)
  for (const claim of analysis.pacing) push('pacing', claim)
  for (const claim of analysis.clarification) push('clarification', claim)
  return topics
}

export function semanticLimitations(analysis: SemanticAnalysis): string[] {
  const source =
    analysis.evidence_source === 'fixture'
      ? `语义分析来自显式fixture（${analysis.provider}），非真实模型产物，不得作为质量或事实证据。`
      : `语义分析来自provider ${analysis.provider}（模型 ${analysis.model ?? '未声明'}），属于不可信推断。`
  return [
    ...analysis.limitations,
    source,
    `观察与推断已区分；置信度为提供者自报（0..1），不是验证结论。语义分析到期时间 ${analysis.expires_at}。`,
    '语义分析不授予发送、入群、审批或执行权限；发送仍由文本许可门与运行时策略决定。'
  ].slice(0, MAX_LIMITATIONS)
}

/** Summary expiry is derived once so analyzers cannot extend it. */
export function summaryExpiry(window: CollectedMessageWindow, retentionDays: number): Date {
  return new Date(window.startTime.getTime() + retentionDays * 86_400_000)
}

export function assertSemanticPrivacy(
  privacyClass: SummaryV1['privacy_class'],
  analysis: SemanticAnalysis
): void {
  if (privacyClass === 'local_only' && analysis.evidence_source === 'provider') {
    throw new CollectorError(
      'summary_invalid',
      'a local_only Summary cannot carry a provider semantic analysis: message bodies would cross the collector boundary'
    )
  }
  if (
    privacyClass === 'local_only' &&
    (analysis.inferred_activity.length > 0 ||
      [...analysis.topics, ...analysis.rules, ...analysis.pacing, ...analysis.clarification].some(
        (claim) => claim.basis === 'inferred'
      ))
  ) {
    throw new CollectorError(
      'summary_invalid',
      'a local_only Summary cannot carry inferred semantic claims'
    )
  }
}

export const SEMANTIC_BOUNDS = {
  MAX_CLAIMS_PER_KIND,
  MAX_INFERRED_ACTIVITY,
  MAX_STATEMENT_CHARS,
  MAX_REFS_PER_CLAIM
} as const
