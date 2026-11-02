import type { Context, Session } from './koishi.js'
import type { CharacterAccountOptions, CharacterRuntimeLogger, GroupContext, GroupContextProvider } from './types.js'

interface PromptRenderer {
  registerFunctionProvider(
    name: string,
    handler: (_args: string[], _variables: Record<string, unknown>, configurable: Record<string, unknown>) => Promise<string>
  ): () => void
}

interface ChatLunaContext extends Context {
  chatluna?: { promptRenderer?: PromptRenderer }
}


function groupIdFromSession(session: Session): string | null {
  if (session.isDirect || typeof session.guildId !== 'string' || session.guildId.length === 0) return null
  if (session.channelId !== session.guildId) return null
  return session.guildId
}

function installGroupContextPrompt(
  ctx: Context,
  account: CharacterAccountOptions,
  provider: GroupContextProvider | undefined,
  logger?: CharacterRuntimeLogger
): () => void {
  const renderer = (ctx as ChatLunaContext).chatluna?.promptRenderer
  if (!renderer) throw new Error('ChatLuna prompt renderer unavailable')
  return renderer.registerFunctionProvider('bba_group_context', async (_args, _variables, configurable) => {
    const session = configurable.session as Session | undefined
    const groupId = session && groupIdFromSession(session)
    const senderId = session?.userId
    if (!groupId || typeof senderId !== 'string' || !provider) return ''
    try {
      const context = await provider({ account, groupId, senderId })
      if (!validContext(context, account, groupId, Date.now())) return ''
      return promptJson(context)
    } catch (error) {
      logger?.warn?.('GroupContext provider unavailable; prompt context omitted', error)
      return ''
    }
  })
}
function isIsoDate(value: unknown): value is string {
  return typeof value === 'string' && Number.isFinite(Date.parse(value))
}

/** A context generated further in the future than this is refused, not shown. */
export const MAX_GROUP_CONTEXT_FUTURE_SKEW_MS = 5 * 60 * 1000

function validContext(
  value: GroupContext | null,
  account: CharacterAccountOptions,
  groupId: string,
  now: number
): value is GroupContext {
  return Boolean(
    value &&
    value.account_id === account.accountId &&
    value.group_id === groupId &&
    value.active_speaker_account_id === account.accountId &&
    typeof value.source === 'string' && value.source.length > 0 && value.source.length <= 256 &&
    isIsoDate(value.generated_at) &&
    isIsoDate(value.expires_at) &&
    Date.parse(value.generated_at) <= now + MAX_GROUP_CONTEXT_FUTURE_SKEW_MS &&
    Date.parse(value.expires_at) > now &&
    Array.isArray(value.topics) && value.topics.length <= 20 &&
    value.topics.every((topic) =>
      typeof topic.summary === 'string' && topic.summary.length > 0 && topic.summary.length <= 4000 &&
      Array.isArray(topic.message_refs) && topic.message_refs.length <= 500 &&
      topic.message_refs.every((ref) => typeof ref === 'string' && ref.length > 0 && ref.length <= 256) &&
      ['member_statement', 'deterministic_statistic', 'model_inference'].includes(topic.evidence_kind)
    ) &&
    (value.summary_ref == null || typeof value.summary_ref === 'string') &&
    (value.summary_id === null || value.summary_id === undefined || typeof value.summary_id === 'string') &&
    (value.summary_version === null || value.summary_version === undefined ||
      (Number.isInteger(value.summary_version) && value.summary_version >= 1)) &&
    (value.source_manifest_sha256 === null || value.source_manifest_sha256 === undefined ||
      (typeof value.source_manifest_sha256 === 'string' && /^[0-9a-f]{64}$/.test(value.source_manifest_sha256))) &&
    (value.privacy_class === undefined || ['local_only', 'redacted_external_allowed', 'none'].includes(value.privacy_class)) &&
    (value.cooldown_until == null || isIsoDate(value.cooldown_until)) &&
    (value.daily_remaining === null || value.daily_remaining === undefined ||
      (Number.isInteger(value.daily_remaining) && value.daily_remaining >= 0)) &&
    (value.participation_goal === null || value.participation_goal === undefined ||
      (typeof value.participation_goal === 'string' && value.participation_goal.length <= 500)) &&
    (value.activity_window === null || value.activity_window === undefined || (
      isIsoDate(value.activity_window.start) &&
      isIsoDate(value.activity_window.end) &&
      Date.parse(value.activity_window.start) <= Date.parse(value.activity_window.end) &&
      Number.isInteger(value.activity_window.message_count) && value.activity_window.message_count >= 0
    ))
  )
}

function promptJson(context: GroupContext): string {
  const bounded = {
    // The block is reference data, never an instruction or a send decision.
    data_trust: 'untrusted_reference_data',
    account_id: context.account_id,
    group_id: context.group_id,
    summary_ref: context.summary_ref,
    summary_id: context.summary_id ?? null,
    summary_version: context.summary_version ?? null,
    source_manifest_sha256: context.source_manifest_sha256 ?? null,
    privacy_class: context.privacy_class ?? null,
    activity_window: context.activity_window,
    topics: context.topics.slice(0, 6).map((topic) => ({
      summary: topic.summary.slice(0, 500),
      message_refs: topic.message_refs.slice(0, 5),
      evidence_kind: topic.evidence_kind
    })),
    active_speaker_account_id: context.active_speaker_account_id,
    cooldown_until: context.cooldown_until,
    daily_remaining: context.daily_remaining,
    participation_goal: context.participation_goal,
    source: context.source,
    generated_at: context.generated_at,
    expires_at: context.expires_at
  }
  return JSON.stringify(bounded).replaceAll('<', '\\u003c').replaceAll('>', '\\u003e')
}

export function createGroupContextPromptPlugin(
  account: CharacterAccountOptions,
  provider: GroupContextProvider | undefined,
  logger?: CharacterRuntimeLogger
) {
  return {
    name: 'bba-group-context-prompt',
    inject: ['chatluna'],
    schema: false,
    apply(ctx: Context): void {
      const unregister = installGroupContextPrompt(ctx, account, provider, logger)
      ctx.on('dispose', unregister)
    }
  }
}
