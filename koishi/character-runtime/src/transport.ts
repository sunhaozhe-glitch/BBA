import { h as koishiH } from './koishi.js'
import type { Fragment } from '@koishijs/core'
import type { Bot, Session } from './koishi.js'
import type { h as KoishiElement } from 'koishi'
import { CharacterRuntimeError } from './errors.js'
import type { CharacterSendGate } from './gate.js'
import type { CharacterRuntimeLogger, ConversationKind } from './types.js'

const GUARD = Symbol.for('bba.character.transport-guard')

export interface TransportGuardOptions {
  logger?: CharacterRuntimeLogger
}

interface ResolvedConversation {
  kind: ConversationKind
  conversationId: string
}

/**
 * Convert outbound content into exactly one plain-text segment.
 *
 * Any rich fragment (image, at, face, quote/reply, markdown, file, video,
 * audio, forward, ...) is rejected. There is no degraded "send only the text
 * part" mode: partially guarding a rich message would let the rich action out.
 */
export function outboundText(content: unknown): string {
  let elements: KoishiElement[]
  try {
    elements = koishiH.normalize(content as Fragment)
  } catch (error) {
    throw new CharacterRuntimeError('rich_content_rejected', 'outbound content is not a valid fragment', {
      cause: error
    })
  }

  const richTypes = new Set<string>()
  const parts: string[] = []
  for (const element of elements) {
    if (element.type !== 'text') {
      richTypes.add(element.type)
      continue
    }
    const value = element.attrs['content']
    parts.push(typeof value === 'string' ? value : String(element))
  }
  if (richTypes.size > 0) {
    throw new CharacterRuntimeError(
      'rich_content_rejected',
      `rich content is not permitted at the character boundary: ${[...richTypes].join(',')}`
    )
  }
  return parts.join('')
}

function splitPrefix(channelId: string): ResolvedConversation | null {
  if (channelId.startsWith('private:')) {
    return { kind: 'private', conversationId: channelId.slice('private:'.length) }
  }
  if (channelId.startsWith('group:')) {
    return { kind: 'group', conversationId: channelId.slice('group:'.length) }
  }
  return null
}

function sessionOf(options: unknown): Session | undefined {
  if (!options || typeof options !== 'object' || !('session' in options)) return undefined
  const session = options.session
  return session && typeof session === 'object' ? (session as Session) : undefined
}

function resolveConversation(
  channelId: string,
  session: Session | undefined,
  gate: CharacterSendGate
): ResolvedConversation {
  if (session) {
    if (session.isDirect) {
      const userId = String(session.userId ?? '')
      if (!userId) {
        throw new CharacterRuntimeError('conversation_not_authorized', 'direct session has no user id')
      }
      return { kind: 'private', conversationId: userId }
    }
    if (session.guildId) {
      return { kind: 'group', conversationId: String(session.guildId) }
    }
  }
  const prefixed = splitPrefix(channelId)
  if (prefixed) return prefixed
  const kinds = gate.inferConversationKinds(channelId)
  if (kinds.length === 1) {
    return { kind: kinds[0]!, conversationId: channelId }
  }
  throw new CharacterRuntimeError(
    'conversation_not_authorized',
    kinds.length === 0
      ? `channel ${channelId} has no trusted trigger for this account`
      : `channel ${channelId} is ambiguous between group and private triggers`
  )
}

function asIds(result: unknown): string[] {
  if (Array.isArray(result)) return result.map((id) => String(id))
  if (result === undefined || result === null) return []
  return [String(result)]
}

/**
 * Replace the final adapter send methods with permit-gated text-only versions.
 *
 * Character session/proactive sends funnel through Bot.sendMessage,
 * Bot.sendPrivateMessage and Bot.createMessage to the final gateway. None calls
 * the original client transport. Direct bot.internal methods are always denied.
 */
export function installTransportGuard(
  bot: Bot,
  gate: CharacterSendGate,
  options: TransportGuardOptions = {}
): () => void {
  const marked = bot as Bot & { [GUARD]?: true }
  if (marked[GUARD]) return () => {}

  const originalSendMessage = bot.sendMessage.bind(bot)
  const originalSendPrivateMessage = bot.sendPrivateMessage.bind(bot)
  const originalCreateMessage = bot.createMessage.bind(bot)
  let originalInternal = (bot as unknown as { internal?: unknown }).internal

  const originalDescriptors = {
    sendMessage: Object.getOwnPropertyDescriptor(bot, 'sendMessage'),
    sendPrivateMessage: Object.getOwnPropertyDescriptor(bot, 'sendPrivateMessage'),
    createMessage: Object.getOwnPropertyDescriptor(bot, 'createMessage'),
    internal: Object.getOwnPropertyDescriptor(bot, 'internal')
  }

  const guardedSendMessage = async (
    channelId: string,
    content: unknown,
    referrer?: unknown,
    sendOptions?: unknown
  ): Promise<string[]> => {
    const session = sessionOf(sendOptions)
    const resolved = resolveConversation(String(channelId), session, gate)
    const text = outboundText(content)
    const context = gate.eventContext(session?.userId ? String(session.userId) : undefined)
    return await gate.sendText({ kind: resolved.kind, conversationId: resolved.conversationId, text, context })
  }

  const guardedSendPrivateMessage = async (
    userId: string,
    content: unknown,
    guildId?: unknown,
    sendOptions?: unknown
  ): Promise<string[]> => {
    const prefixed = splitPrefix(String(userId))
    if (prefixed && prefixed.kind !== 'private') {
      throw new CharacterRuntimeError(
        'conversation_not_authorized',
        `sendPrivateMessage received a group channel ${String(userId)}`
      )
    }
    const conversationId = prefixed ? prefixed.conversationId : String(userId)
    if (!conversationId) {
      throw new CharacterRuntimeError('conversation_not_authorized', 'private target is empty')
    }
    const session = sessionOf(sendOptions)
    const text = outboundText(content)
    const context = gate.eventContext(
      session?.userId ? String(session.userId) : conversationId
    )
    return await gate.sendText({ kind: 'private', conversationId, text, context })
  }

  const guardedCreateMessage = async (
    channelId: string,
    content: unknown,
    referrer?: unknown,
    sendOptions?: unknown
  ): Promise<unknown[]> => {
    const session = sessionOf(sendOptions)
    const resolved = resolveConversation(String(channelId), session, gate)
    const text = outboundText(content)
    const context = gate.eventContext(session?.userId ? String(session.userId) : undefined)
    const ids = await gate.sendText({ kind: resolved.kind, conversationId: resolved.conversationId, text, context })
    return ids.map((id) => ({ id }))
  }

  Object.defineProperty(bot, 'sendMessage', {
    configurable: true,
    enumerable: false,
    writable: true,
    value: guardedSendMessage
  })
  Object.defineProperty(bot, 'sendPrivateMessage', {
    configurable: true,
    enumerable: false,
    writable: true,
    value: guardedSendPrivateMessage
  })
  Object.defineProperty(bot, 'createMessage', {
    configurable: true,
    enumerable: false,
    writable: true,
    value: guardedCreateMessage
  })

  // Adapter internals are never a local writer escape hatch. Adapters may
  // update the target while connecting, but every guarded method read is denied.
  const internalProxy = new Proxy(
    {},
    {
      get(_placeholder, property) {
        if (property === 'then') return undefined
        return () => denyInternal(property)
      },
      set(_placeholder, property, value) {
        return denyInternal(property)
      }
    }
  )
  Object.defineProperty(bot, 'internal', {
    configurable: true,
    enumerable: true,
    get: () => internalProxy,
    set: (value: unknown) => {
      originalInternal = value
    }
  })

  Object.defineProperty(marked, GUARD, { configurable: true, value: true, enumerable: false })

  options.logger?.debug?.(`transport guard installed selfId=${bot.selfId}`)

  return () => {
    restore(bot, 'sendMessage', originalDescriptors.sendMessage, originalSendMessage)
    restore(bot, 'sendPrivateMessage', originalDescriptors.sendPrivateMessage, originalSendPrivateMessage)
    restore(bot, 'createMessage', originalDescriptors.createMessage, originalCreateMessage)
    restore(bot, 'internal', originalDescriptors.internal, originalInternal)
    delete (marked as { [GUARD]?: true })[GUARD]
  }
}

function denyInternal(property: string | symbol): never {
  throw new CharacterRuntimeError(
    'unguarded_internal_send',
    `direct adapter internal access is not permitted at the character boundary: ${String(property)}`
  )
}

function restore(
  bot: Bot,
  property: 'sendMessage' | 'sendPrivateMessage' | 'createMessage' | 'internal',
  descriptor: PropertyDescriptor | undefined,
  fallback: unknown
): void {
  if (descriptor) {
    Object.defineProperty(bot, property, descriptor)
    return
  }
  if (fallback === undefined) {
    try {
      delete (bot as unknown as Record<string, unknown>)[property]
    } catch {
      // Non-configurable property installed by an adapter; leave it in place.
    }
    return
  }
  Object.defineProperty(bot, property, {
    configurable: true,
    enumerable: false,
    writable: true,
    value: fallback
  })
}

export function isTransportGuarded(bot: Bot): boolean {
  return Boolean((bot as Bot & { [GUARD]?: true })[GUARD])
}
