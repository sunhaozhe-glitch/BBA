import type { Context, Session } from './koishi.js'
import { CharacterRuntimeError, isCharacterRuntimeError } from './errors.js'
import type { CharacterSendGate } from './gate.js'
import type { CharacterAccountOptions, CharacterRuntimeLogger } from './types.js'

export interface IngressOptions {
  account: CharacterAccountOptions
  gate: CharacterSendGate
  logger?: CharacterRuntimeLogger
}

function sourceEventId(session: Session): string {
  const messageId = session.messageId
  if (typeof messageId === 'string' && messageId.length > 0) return messageId
  const event = session.event as { id?: unknown } | undefined
  if (event && typeof event.id === 'string' && event.id.length > 0) return event.id
  // A synthesised identity (for example from Date.now) cannot be trusted as a
  // trigger/event id: a retry would create a second trigger. Fail closed.
  throw new CharacterRuntimeError(
    'invalid_binding',
    'inbound event has no stable messageId/event id; refusing to synthesise one'
  )
}

/**
 * Prepend the ingress filter for one account runtime.
 *
 * Managed bots, the account's own echo, events from another bot instance and
 * events this account is not selected to speak for never reach Character.
 */
export function installCharacterIngress(ctx: Context, options: IngressOptions): () => void {
  const { account, gate, logger } = options
  return ctx.$processor.middleware(async (session, next) => {
    try {
      if (session.selfId !== account.qqSelfId) {
        logger?.debug?.(`cross-account event ignored selfId=${session.selfId} account=${account.accountId}`)
        return
      }
      const sender = session.userId
      if (!sender || sender === session.selfId) return
      if (gate.isManagedSelfId(sender)) {
        logger?.debug?.(`managed bot ingress ignored sender=${sender}`)
        return
      }

      const isDirect = session.isDirect || !session.guildId
      const kind = isDirect ? 'private' : 'group'
      const conversationId = String(isDirect ? sender : session.guildId)
      const trigger = await gate.openTriggerForEvent({
        kind,
        conversationId,
        senderId: String(sender),
        sourceEventId: sourceEventId(session),
        directedAccountId: isDirect ? account.accountId : null
      })
      if (!trigger) return
      return next()
    } catch (error) {
      if (isCharacterRuntimeError(error)) {
        logger?.warn?.(`ingress rejected account=${account.accountId} code=${error.code} message=${error.message}`)
      } else {
        logger?.error?.('ingress failed closed', error)
      }
      return
    }
  }, { prepend: true, global: true })
}
