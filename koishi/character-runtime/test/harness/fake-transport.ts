import type { Guild } from '@satorijs/protocol'
import { Adapter as KoishiAdapter, Bot as KoishiBot, h as koishiH } from '../../src/koishi.js'
import type { Context, Fragment } from 'koishi'

/**
 * FIXTURE ONLY - an in-process adapter that never opens a socket. It records
 * the exact text that reached the adapter boundary so tests can prove the send
 * gate allowed exactly one text segment.
 */
export interface SentRecord {
  channelId: string
  text: string
  at: number
}

export interface FakeTransportOptions {
  selfId: string
  sink: SentRecord[]
  /** Return adapter message ids (default false, i.e. sent with message_id null). */
  returnIds?: boolean
  /** Optional hook used to hold one adapter call open during concurrency tests. */
  beforeSend?: (record: SentRecord) => Promise<void> | void
  /** Optional deterministic notification instead of polling the sink. */
  notify?: (record: SentRecord) => void
}

interface FakeBotConfig {
  selfId: string
  hooks: FakeTransportOptions
}

function plainText(content: unknown): string {
  const elements = koishiH.normalize(content as Fragment)
  return elements
    .map((element) =>
      element.type === 'text' ? String(element.attrs['content'] ?? '') : `<${element.type}>`
    )
    .join('')
}

class FakeBot extends KoishiBot<Context, FakeBotConfig> {
  /** The real bots collection this bot registered into in the constructor. */
  private readonly registeredBots: unknown[]

  constructor(ctx: Context, config: FakeBotConfig, adapter: InstanceType<typeof KoishiAdapter>) {
    super(ctx, config, 'fake')
    this.registeredBots = ctx.bots as unknown as unknown[]
    this.selfId = config.selfId
    this.platform = 'fake'
    this.adapter = adapter as typeof this.adapter
    this.user = { id: config.selfId, name: 'offline-fixture-bot' }
  }
  override async getGuild(guildId: string): Promise<Guild> {
    return { id: guildId, name: `Guild ${guildId}` }
  }

  override async dispose(): Promise<void> {
    const index = this.registeredBots.indexOf(this)
    if (index >= 0) this.registeredBots.splice(index, 1)
    await this.stop()
  }

  override async sendMessage(channelId: string, content: Fragment): Promise<string[]> {
    const record: SentRecord = {
      channelId: String(channelId),
      text: plainText(content),
      at: Date.now()
    }
    await this.config.hooks.beforeSend?.(record)
    this.config.hooks.sink.push(record)
    this.config.hooks.notify?.(record)
    return this.config.hooks.returnIds
      ? [`${this.selfId}-${this.config.hooks.sink.length}`]
      : []
  }

  override async sendPrivateMessage(userId: string, content: Fragment): Promise<string[]> {
    const record: SentRecord = {
      channelId: `private:${userId}`,
      text: plainText(content),
      at: Date.now()
    }
    await this.config.hooks.beforeSend?.(record)
    this.config.hooks.sink.push(record)
    this.config.hooks.notify?.(record)
    return this.config.hooks.returnIds
      ? [`${this.selfId}-${this.config.hooks.sink.length}`]
      : []
  }
}

class FakeAdapter extends KoishiAdapter<Context, FakeBot> {
  override async connect(bot: FakeBot): Promise<void> {
    bot.online()
  }

  override async disconnect(bot: FakeBot): Promise<void> {
    bot.offline()
  }
}

export function createFakeTransportPlugin(
  options: FakeTransportOptions
): (ctx: Context) => void {
  return (ctx: Context): void => {
    const adapter = new FakeAdapter(ctx)
    const bot = new FakeBot(
      ctx,
      {
        selfId: options.selfId,
        hooks: options
      },
      adapter
    )
    // Adapter.fork is the real Satori registration path: it assigns the
    // adapter and removes the bot from this list when its scope disposes.
    adapter.fork(ctx, bot)
  }
}
