import type { Context, Fragment, Session } from '../../src/koishi.js'

export interface SimulatedCharacterConfig {
  /** Exact reply per conversation key, e.g. group:123 or private:456. */
  replies?: Record<string, string>
  /** Rich reply per conversation key, used to prove the text-only gate. */
  richReplies?: Record<string, Fragment>
  defaultReply?: string
  /** Delay before emitting the simulated Character output. */
  delayMs?: number
  /** Test seam: called after the simulated send settles. */
  onOutput?: (session: Session) => void
}

export const name = 'simulated-character-provider'

function conversationKey(session: Session): string {
  return session.isDirect
    ? `private:${String(session.userId ?? '')}`
    : `group:${String(session.guildId ?? '')}`
}

/**
 * Test-only simulated Character provider.
 *
 * It is a real Koishi plugin and real middleware/message consumer: the ingress
 * gate must admit the event before this plugin runs, and its output travels
 * through the real `session.send` -> guarded `bot.sendMessage` path. It is not
 * a fake transport and it does not echo inbound text.
 */
export const simulatedCharacter = {
  name: 'simulated-character-provider',
  schema: false,
  apply(ctx: Context, config: SimulatedCharacterConfig = {}): void {
    ctx.$processor.middleware(async (session: Session, next) => {
      const key = conversationKey(session)
      const payload = config.richReplies?.[key] ?? config.replies?.[key] ?? config.defaultReply
      await next()
      if (!payload) return
      if (config.delayMs) {
        const { promise, resolve } = Promise.withResolvers<void>()
        setTimeout(resolve, config.delayMs)
        await promise
      }
      try {
        await session.send(payload)
      } finally {
        config.onOutput?.(session)
      }
    }, { global: true })
  }
}
