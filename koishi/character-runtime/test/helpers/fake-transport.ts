import { Adapter as KoishiAdapter, Bot as KoishiBot, h as koishiH } from '../../src/koishi.js'
import type { Context } from '../../src/koishi.js'
import type { Guild } from '@satorijs/protocol'
import type { Fragment } from '@koishijs/core'

export type TransportOutcome =
  | { type: 'ok'; messageId?: string }
  | { type: 'error'; message?: string }
  | { type: 'timeout'; delayMs?: number }

export interface FakeTransportConfig {
  selfId: string
  nickname?: string
  /** Scripted outcomes consumed once per call. Default is ok. */
  outcomes?: TransportOutcome[]
}

export interface TransportCall {
  method: 'sendMessage' | 'sendPrivateMessage' | 'createMessage'
  target: string
  content: unknown
  text: string
  at: number
}

export interface InternalCall {
  method: string
  args: unknown[]
  at: number
}

export function contentText(content: unknown): string {
  const parts: string[] = []
  for (const element of koishiH.normalize(content as Fragment)) {
    if (element.type === 'text') {
      const value = element.attrs['content']
      parts.push(typeof value === 'string' ? value : '')
    } else {
      parts.push(`[${element.type}]`)
    }
  }
  return parts.join('')
}

/**
 * Explicit fake transport: it records real Koishi Bot calls and returns
 * scripted success/failure/timeout outcomes. It is not an echo bot and it never
 * re-enters the message pipeline.
 */
export class FakeTransportBot extends KoishiBot<Context, FakeTransportConfig> {
  readonly transportCalls: TransportCall[] = []
  readonly internalCalls: InternalCall[] = []
  private readonly registeredBots: unknown[]
  private readonly outcomes: TransportOutcome[]
  private messageSeq = 0
  constructor(ctx: Context, config: FakeTransportConfig) {
    super(ctx, config, 'fake')
    const adapter = new FakeAdapter(ctx)
    adapter.fork(ctx, this)
    const bots: unknown = Reflect.get(ctx, 'bots')
    if (!Array.isArray(bots)) throw new Error('fake transport adapter bot collection is unavailable')
    this.registeredBots = bots
    this.selfId = config.selfId
    this.platform = 'fake'
    this.outcomes = [...(config.outcomes ?? [])]
    // The guard replaces this property with a denying proxy; direct access in
    // an unguarded runtime would hit these recorders.
    ;(this as unknown as { internal: unknown }).internal = {
      _request: async (method: string, ...args: unknown[]) => {
        this.internalCalls.push({ method, args, at: Date.now() })
        throw new Error(`unexpected direct adapter call ${method}`)
      },
      sendPrivateMessage: async (userId: string, payload: unknown) => {
        this.internalCalls.push({ method: 'sendPrivateMessage', args: [userId, payload], at: Date.now() })
        throw new Error('unexpected direct private adapter call')
      }
    }
  }
  override async getGuild(guildId: string): Promise<Guild> {
    return { id: guildId, name: `Guild ${guildId}` }
  }
  override async dispose(): Promise<void> {
    const index = this.registeredBots.indexOf(this)
    if (index >= 0) this.registeredBots.splice(index, 1)
    await this.stop()
  }
  override async sendMessage(
    channelId: string,
    content: Fragment,
    _referrer?: unknown,
    _options?: unknown
  ): Promise<string[]> {
    this.transportCalls.push({
      method: 'sendMessage',
      target: String(channelId),
      content,
      text: contentText(content),
      at: Date.now()
    })
    return await this.outcome()
  }

  override async sendPrivateMessage(
    userId: string,
    content: Fragment,
    _guildId?: string,
    _options?: unknown
  ): Promise<string[]> {
    this.transportCalls.push({
      method: 'sendPrivateMessage',
      target: String(userId),
      content,
      text: contentText(content),
      at: Date.now()
    })
    return await this.outcome()
  }

  override async createMessage(
    channelId: string,
    content: Fragment,
    _referrer?: unknown,
    _options?: unknown
  ): Promise<never[]> {
    this.transportCalls.push({
      method: 'createMessage',
      target: String(channelId),
      content,
      text: contentText(content),
      at: Date.now()
    })
    await this.outcome()
    return []
  }

  private async outcome(): Promise<string[]> {
    const outcome = this.outcomes.shift() ?? { type: 'ok' }
    if (outcome.type === 'error') {
      throw new Error(outcome.message ?? 'scripted transport failure')
    }
    if (outcome.type === 'timeout') {
      const { promise, resolve } = Promise.withResolvers<void>()
      setTimeout(resolve, outcome.delayMs ?? 25)
      await promise
      throw new Error('scripted transport timeout')
    }
    this.messageSeq += 1
    return [outcome.messageId ?? `fake_msg_${this.messageSeq}`]
  }
}

class FakeAdapter extends KoishiAdapter<Context, FakeTransportBot> {
  override async connect(bot: FakeTransportBot): Promise<void> {
    bot.online()
  }

  override async disconnect(bot: FakeTransportBot): Promise<void> {
    bot.offline()
  }
}

export function createFakeTransportPlugin(config: FakeTransportConfig): (ctx: Context) => void {
  return (ctx: Context): void => {
    new FakeTransportBot(ctx, config)
  }
}
