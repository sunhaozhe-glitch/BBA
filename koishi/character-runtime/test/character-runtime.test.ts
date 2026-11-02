import type { BaseMessage } from '@langchain/core/messages'
import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, readFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { h } from '../src/koishi.js'
import type { Bot, Session } from '../src/koishi.js'
import { startFakeBackend } from './harness/fake-backend.js'
import type { FakeBackendState } from './harness/fake-backend.js'
import { createFakeProviderPlugin } from './harness/fake-provider.js'
import { createFakeTransportPlugin } from './harness/fake-transport.js'
import type { FakeTransportOptions, SentRecord } from './harness/fake-transport.js'
import { createCharacterAccountRuntimeManager } from '../src/runtime.js'
import type { CharacterAccountRuntimeManager } from '../src/runtime.js'
import { BackendHttpError, CharacterRuntimeError } from '../src/errors.js'
import type { CharacterSendGate } from '../src/gate.js'
import type { GroupContext, GroupContextProvider } from '../src/types.js'
import type { CharacterAccountOptions } from '../src/types.js'

const ACCOUNT_ID = 'acct-offline-a'
const SELF_ID = '10001'
const GROUP_ID = '20001'
const USER_ID = '30001'

interface OfflineCharacterService {
  preset?: {
    getPreset(name: string, fromDisk?: boolean): Promise<{ name: string; system: { rawString: string } }>
    resolvePresetDir(): string
  }
  triggerMessage(session: Session, message: unknown, reason: string): Promise<boolean>
}

interface OfflineServices {
  chatluna?: { platform?: unknown }
  chatluna_character?: OfflineCharacterService
  loader?: {
    baseDir?: string
    filename?: string
    writeConfig?: unknown
  }
}

/**
 * Assert the denial invariant by the typed error code, not by message wording.
 * Error text is operator-facing and may be translated or reworded upstream.
 */
function deniesWith(code: CharacterRuntimeError['code'], status?: number) {
  return (error: unknown): boolean => {
    assert.ok(
      error instanceof CharacterRuntimeError,
      `expected CharacterRuntimeError(${code}), got ${String(error)}`
    )
    assert.equal(error.code, code)
    if (status !== undefined) {
      assert.ok(error instanceof BackendHttpError, `expected BackendHttpError, got ${error.name}`)
      assert.equal(error.status, status)
    }
    return true
  }
}

async function waitForOpenTrigger(
  gate: CharacterSendGate,
  kind: 'group' | 'private',
  conversationId: string,
  timeoutMs = 10_000
): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const snapshot = await gate.snapshot()
    if (
      Object.values(snapshot.triggers).some(
        (trigger) =>
          trigger.state === 'open' &&
          trigger.kind === kind &&
          trigger.conversation_id === conversationId
      )
    ) {
      return
    }
    await new Promise((resolve) => setTimeout(resolve, 10))
  }
  throw new Error(`timed out waiting for an open ${kind}:${conversationId} trigger`)
}

function messageSession(bot: Bot, groupId = GROUP_ID, messageId = 'msg-1'): Session {
  const session = bot.session({
    type: 'message',
    timestamp: Date.now(),
    guild: { id: groupId },
    channel: { id: groupId, type: 0 },
    user: { id: USER_ID, name: 'tester' },
    message: { id: messageId }
  } as never) as unknown as Session
  session.content = '你好'
  return session
}

function fakeAccount(
  transport: FakeTransportOptions
): CharacterAccountOptions {
  return {
    accountId: ACCOUNT_ID,
    qqSelfId: SELF_ID,
    runtimeId: 'runtime-offline-a',
    characterRuntimeId: 'character-runtime-offline-a',
    characterDbId: 'character-db-offline-a',
    characterCacheNamespace: 'character-cache-offline-a',
    personaId: 'PERSONA_OFFLINE_A',
    persona: '你是离线测试角色，只输出一条短文本。',
    runtimeToken: 'test-runtime-token',
    transportPlugin: createFakeTransportPlugin(transport),
    transportConfig: { selfId: SELF_ID },
    characterModel: 'fake/fake-model',
    characterConfig: {
      globalPrivateConfig: {
        enableFixedIntervalTrigger: false,
        enableActivityScoreTrigger: false,
        idleTrigger: { enableLongWaitTrigger: false },
        toolCalling: false,
        messageWaitTime: 0,
        typingTime: 100
      },
      globalGroupConfig: {
        enableFixedIntervalTrigger: false,
        enableActivityScoreTrigger: false,
        idleTrigger: { enableLongWaitTrigger: false },
        toolCalling: false,
        messageWaitTime: 0,
        typingTime: 100
      }
    }
  }
}

function createManager(
  stateDir: string,
  backend: FakeBackendState,
  account: CharacterAccountOptions,
  extra: {
    groupContextProvider?: GroupContextProvider
    onGenerate?: (messages: BaseMessage[]) => void
  } = {}
): CharacterAccountRuntimeManager {
  return createCharacterAccountRuntimeManager({
    stateDir,
    backendBaseUrl: backend.baseUrl,
    accounts: [{...account, gateway:{baseUrl:backend.baseUrl,token:account.runtimeToken}}],
    startApps: true,
    groupContextProvider: extra.groupContextProvider,
    providerPlugins: [
      {
        plugin: createFakeProviderPlugin({
          replies: ['<message>离线测试回复</message>'],
          onGenerate: extra.onGenerate
        })
      }
    ]
  })
}

test('offline Character integration sends exactly one gated text with mandatory outcome', { timeout: 30_000 }, async (t) => {
  const backend = await startFakeBackend({
    accountId: ACCOUNT_ID,
    qqSelfId: SELF_ID,
    managedSelfIds: ['99999']
  })
  t.after(() => backend.close())
  const stateDir = await mkdtemp(join(tmpdir(), 'bba-character-offline-'))
  const sink: SentRecord[] = []
  const transport: FakeTransportOptions = {
    selfId: SELF_ID,
    sink,
  }
  const manager = createManager(stateDir, backend, fakeAccount(transport))
  t.after(() => manager.stop())

  await manager.start()
  const runtime = manager.runtimes.get(ACCOUNT_ID)
  assert.ok(runtime, 'account runtime is registered')
  assert.ok(runtime.bot, 'fake transport bot connected')
  if (!runtime.bot) return
  const bot = runtime.bot

  // Koishi service injection: the concrete service types live in ChatLuna and
  // Character packages, so the harness narrows only the members it exercises.
  const services = runtime.app as unknown as OfflineServices
  assert.ok(services.chatluna?.platform, 'actual ChatLuna service is loaded')
  const character = services.chatluna_character
  assert.ok(character, 'actual Character service is loaded')
  if (!character) return
  // Character persists its config migration through this service. It must be
  // the real loader bound to this account's own base directory and file.
  assert.equal(typeof services.loader?.writeConfig, 'function', 'genuine per-account Loader is installed')
  assert.equal(services.loader?.baseDir, runtime.directories.root)
  assert.equal(services.loader?.filename, join(runtime.directories.root, 'koishi.config.json'))
  assert.equal(
    character.preset?.resolvePresetDir(),
    join(runtime.directories.root, 'data', 'chathub', 'character', 'presets')
  )
  assert.ok((await readFile(runtime.directories.preset, 'utf8')).includes('PERSONA_OFFLINE_A'))
  const preset = await character.preset?.getPreset('PERSONA_OFFLINE_A', true)
  assert.match(preset?.system.rawString ?? '', /离线测试角色/)

  const session = messageSession(bot)
  bot.dispatch(session)
  await backend.waitForRequest('/v1/internal/chat-triggers')
  await waitForOpenTrigger(runtime.gate, 'group', GROUP_ID)
  assert.equal(
    backend.requests.filter((request) => request.path === '/v1/internal/chat-triggers').length,
    1
  )

  const triggered = await character.triggerMessage(
    session,
    { content: '你好', name: 'tester', id: USER_ID, messageId: 'msg-1', timestamp: Date.now() },
    'offline fixture'
  )
  assert.equal(triggered, true)
  assert.equal(sink.length, 0, 'local adapter is never called')
  await backend.waitForOutcome(1)

  assert.equal(sink.length, 0)
  assert.equal(backend.writerCalls.length, 1)
  assert.equal(backend.writerCalls[0]?.text, '离线测试回复')
  assert.equal(backend.consumeCount, 1, 'exactly one permit consumed')
  assert.equal(backend.outcomes.length, 1, 'mandatory outcome reported')
  assert.equal(backend.outcomes[0]?.state, 'sent')
  assert.equal(backend.outcomes[0]?.message_id, 'gateway-msg-1')
  assert.equal(backend.requests.filter(r => r.path.endsWith('/outcome') || r.path.endsWith('/consume')).length, 0)
  assert.equal(
    backend.requests.filter((request) => request.path === '/v1/internal/chat-send-permits').length,
    1
  )

  // Durable per-account database file exists and is valid.
  const databaseFile = join(runtime.directories.database, 'koishi-db.json')
  const snapshot = JSON.parse(await readFile(databaseFile, 'utf8')) as {
    version?: number
    store?: Record<string, unknown>
  }
  assert.equal(snapshot.version, 1)
  assert.ok(snapshot.store && typeof snapshot.store === 'object', 'Character database snapshot persisted')
})

test('actual Character prompt receives only fresh scoped GroupContext for its group', { timeout: 30_000 }, async (t) => {
  const backend = await startFakeBackend({ accountId: ACCOUNT_ID, qqSelfId: SELF_ID })
  t.after(() => backend.close())
  const stateDir = await mkdtemp(join(tmpdir(), 'bba-character-group-context-'))
  const sink: SentRecord[] = []
  const prompts: string[][] = []
  const contextFor = (groupId: string, topic: string, ref: string): GroupContext => ({
    account_id: ACCOUNT_ID,
    group_id: groupId,
    summary_ref: ref,
    topics: [{ summary: topic, message_refs: [`message-${groupId}`], evidence_kind: 'member_statement' }],
    activity_window: { start: '2026-10-29T10:00:00Z', end: '2026-10-29T11:00:00Z', message_count: 4 },
    active_speaker_account_id: ACCOUNT_ID,
    cooldown_until: null,
    daily_remaining: 2,
    participation_goal: null,
    source: 'summary-store',
    generated_at: new Date().toISOString(),
    expires_at: new Date(Date.now() + 60_000).toISOString()
  })
  const contexts: Record<string, GroupContext> = {
    '20001': contextFor('20001', 'Group A discusses local models.', 'summary-20001-v3'),
    '20002': contextFor('20002', 'Group B discusses robotics.', 'summary-20002-v1')
  }
  const provider: GroupContextProvider = async ({ account, groupId }) => {
    assert.equal(account.accountId, ACCOUNT_ID)
    return contexts[groupId] ?? null
  }
  const manager = createManager(stateDir, backend, fakeAccount({ selfId: SELF_ID, sink }), {
    groupContextProvider: provider,
    onGenerate: (messages) => prompts.push(messages.map((message) => String(message.content)))
  })
  t.after(() => manager.stop())
  await manager.start()
  const runtime = manager.runtimes.get(ACCOUNT_ID)!
  const character = (runtime.app as unknown as OfflineServices).chatluna_character!

  for (const [index, groupId] of ['20001', '20002'].entries()) {
    const eventId = `group-context-${index}`
    const session = messageSession(runtime.bot!, groupId, eventId)
    session.guildId = groupId
    session.channelId = groupId
    await runtime.gate.openTriggerForEvent({ kind: 'group', conversationId: groupId, senderId: USER_ID, sourceEventId: eventId, segmentCount: 1 })
    await character.triggerMessage(session, { content: 'context fixture message', name: 'tester', id: USER_ID, messageId: eventId, timestamp: Date.now() }, 'group context fixture')
  }

  assert.equal(prompts.length, 2)
  assert.match(prompts[0]!.join('\n'), /Group A discusses local models\./)
  assert.doesNotMatch(prompts[0]!.join('\n'), /Group B discusses robotics\./)
  assert.match(prompts[0]!.join('\n'), /summary-20001-v3/)
  assert.match(prompts[1]!.join('\n'), /Group B discusses robotics\./)
  assert.doesNotMatch(prompts[1]!.join('\n'), /Group A discusses local models\./)
  assert.match(prompts[1]!.join('\n'), /summary-20002-v1/)

  contexts['20001'] = { ...contexts['20001']!, expires_at: new Date(Date.now() - 1).toISOString() }
  const eventId = 'group-context-expired'
  const expiredSession = messageSession(runtime.bot!, '20001', eventId)
  expiredSession.guildId = '20001'
  expiredSession.channelId = '20001'
  await runtime.gate.openTriggerForEvent({ kind: 'group', conversationId: '20001', senderId: USER_ID, sourceEventId: eventId, segmentCount: 1 })
  await character.triggerMessage(expiredSession, { content: 'expired context', name: 'tester', id: USER_ID, messageId: eventId, timestamp: Date.now() }, 'expired context fixture')
  assert.equal(prompts.length, 3)
  const latestGroupContext = prompts[2]!.join('\n').split('# 有效群上下文').at(-1) ?? ''
  assert.doesNotMatch(latestGroupContext, /Group A discusses local models\.|summary-20001-v3/)
})
test('concurrent direct and internal sends cannot bypass the pending permit', async (t) => {
  const reached = Promise.withResolvers<void>()
  const release = Promise.withResolvers<void>()
  const backend = await startFakeBackend({ accountId: ACCOUNT_ID, qqSelfId: SELF_ID, beforeGateway: async () => { reached.resolve(); await release.promise } })
  t.after(() => backend.close())
  const stateDir = await mkdtemp(join(tmpdir(), 'bba-character-race-'))
  const sink: SentRecord[] = []
  let hold = false
  const transport: FakeTransportOptions = {
    selfId: SELF_ID,
    sink,
    beforeSend: async () => {
      if (!hold) return
      hold = false
      reached.resolve()
      await release.promise
    }
  }
  const manager = createManager(stateDir, backend, fakeAccount(transport))
  t.after(() => manager.stop())
  await manager.start()
  const runtime = manager.runtimes.get(ACCOUNT_ID)
  assert.ok(runtime?.bot)
  if (!runtime?.bot) return
  const bot = runtime.bot
  const session = messageSession(bot)

  bot.dispatch(session)
  await backend.waitForRequest('/v1/internal/chat-triggers')
  await waitForOpenTrigger(runtime.gate, 'group', GROUP_ID)

  hold = true
  const first = bot.sendMessage(GROUP_ID, '合法发送', undefined, { session })
  await reached.promise

  await assert.rejects(
    () => bot.sendMessage(GROUP_ID, '并发直发', undefined, { session }),
    deniesWith('delivery_unknown')
  )
  await assert.rejects(
    () => bot.sendMessage(GROUP_ID, h('img', { src: 'https://fixture.invalid/a.png' }), undefined, { session }),
    deniesWith('rich_content_rejected')
  )
  // Named cast: the guard replaces the adapter's internal client with a proxy;
  // the fixture reaches for it directly to prove unguarded internals fail.
  const guardedInternal = bot as unknown as {
    internal: { send_msg(payload: unknown): Promise<unknown> }
  }
  assert.throws(() => guardedInternal.internal.send_msg({}), deniesWith('unguarded_internal_send'))

  release.resolve()
  assert.deepEqual(await first, ['gateway-msg-1'])

  assert.equal(sink.length, 0, 'no local transport, including concurrent/internal entry')
  assert.equal(backend.writerCalls.length, 1)
  assert.equal(backend.writerCalls[0]?.text, '合法发送')
  await backend.waitForOutcome(1)
  assert.equal(backend.outcomes[0]?.state, 'sent')
  assert.equal(backend.outcomes[0]?.message_id, 'gateway-msg-1')
})

test('missing final gateway fails closed without a client writer or outcome report', async (t) => {
  const backend = await startFakeBackend({
    accountId: ACCOUNT_ID,
    qqSelfId: SELF_ID,
    outcomeStatus: 404
  })
  t.after(() => backend.close())
  const stateDir = await mkdtemp(join(tmpdir(), 'bba-character-outcome-'))
  const sink: SentRecord[] = []
  const manager = createManager(
    stateDir,
    backend,
    fakeAccount({ selfId: SELF_ID, sink })
  )
  t.after(() => manager.stop())
  await manager.start()
  const runtime = manager.runtimes.get(ACCOUNT_ID)
  assert.ok(runtime?.bot)
  if (!runtime?.bot) return
  runtime.bot.dispatch(messageSession(runtime.bot))
  await backend.waitForRequest('/v1/internal/chat-triggers')
  await waitForOpenTrigger(runtime.gate, 'group', GROUP_ID)

  await assert.rejects(
    () =>
      runtime.gate.sendText({
        kind: 'group',
        conversationId: GROUP_ID,
        text: '一次性发送',
        transport: async () => {
          sink.push({ channelId: GROUP_ID, text: '一次性发送', at: Date.now() })
          return []
        }
      }),
    deniesWith('backend_rejected', 404)
  )
  assert.equal(sink.length, 0, 'missing capability never falls back to local adapter')
  const ledger = await runtime.gate.snapshot()
  assert.equal(Object.values(ledger.sends)[0]?.state, 'rejected')
  assert.equal(backend.writerCalls.length, 0)
})
