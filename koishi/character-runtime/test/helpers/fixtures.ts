import type { Bot } from '../../src/koishi.js'
import type { CharacterAccountOptions } from '../../src/types.js'
import { createFakeTransportPlugin } from './fake-transport.js'

export const ACCOUNT_A: CharacterAccountOptions = {
  accountId: 'acc_a',
  qqSelfId: '10001',
  runtimeId: 'rt_a',
  characterRuntimeId: 'cr_a',
  characterDbId: 'db_a',
  characterCacheNamespace: 'cache_a',
  personaId: 'persona_a',
  persona: 'You are account A, a calm test persona.',
  runtimeToken: 'token_a',
  adapterId: 'fake',
  adapterInstanceId: 'adapter_a',
  transportPlugin: createFakeTransportPlugin({ selfId: '10001' }),
  transportConfig: { selfId: '10001', nickname: 'A' }
}

export const ACCOUNT_B: CharacterAccountOptions = {
  accountId: 'acc_b',
  qqSelfId: '10002',
  runtimeId: 'rt_b',
  characterRuntimeId: 'cr_b',
  characterDbId: 'db_b',
  characterCacheNamespace: 'cache_b',
  personaId: 'persona_b',
  persona: 'You are account B, a different test persona.',
  runtimeToken: 'token_b',
  adapterId: 'fake',
  adapterInstanceId: 'adapter_b',
  transportPlugin: createFakeTransportPlugin({ selfId: '10002' }),
  transportConfig: { selfId: '10002', nickname: 'B' }
}

export const MANAGED_SELF_IDS = ['10001', '10002', '19999']

export function account(
  base: CharacterAccountOptions,
  overrides: Partial<CharacterAccountOptions> = {}
): CharacterAccountOptions {
  return { ...base, ...overrides }
}

export function conversationKey(kind: 'group' | 'private', id: string): string {
  return `${kind}:${id}`
}

/** Wait until an asynchronous Koishi dispatch or scripted bot outcome settles. */
export async function waitFor(predicate: () => boolean, timeoutMs = 2_000): Promise<void> {
  const started = Date.now()
  while (!predicate()) {
    if (Date.now() - started > timeoutMs) throw new Error('waitFor timed out')
    const { promise, resolve } = Promise.withResolvers<void>()
    setImmediate(resolve)
    await promise
  }
}

export interface DispatchOptions {
  userId: string
  guildId?: string
  conversationId?: string
  content?: string
  messageId?: string
  isDirect?: boolean
}

/** Build and dispatch a real Koishi Session through the app's middleware chain. */
export function dispatchMessage(bot: Bot, options: DispatchOptions): void {
  const isDirect = options.isDirect ?? !options.guildId
  const content = options.content ?? 'hello'
  const session = bot.session({
    type: 'message',
    user: { id: options.userId },
    message: { id: options.messageId ?? `msg_${Math.random().toString(36).slice(2, 9)}` },
    timestamp: Date.now()
  })
  session.content = content
  session.selfId = bot.selfId
  session.userId = options.userId
  if (isDirect) {
    session.channelId = `private:${options.conversationId ?? options.userId}`
    session.isDirect = true
  } else {
    session.channelId = options.conversationId ?? options.guildId ?? ''
    session.guildId = options.guildId ?? options.conversationId
    session.isDirect = false
  }
  bot.dispatch(session)
}
