import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { mkdtemp, readFile, rm, stat } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { describe, test } from 'node:test'
import { h } from '../src/koishi.js'
import { CharacterRuntimeError } from '../src/errors.js'
import { CharacterAccountRuntimeManager } from '../src/runtime.js'
import { isTransportGuarded } from '../src/transport.js'
import type { CharacterRuntimeLogger, CharacterRuntimeManagerOptions } from '../src/types.js'
import { FakeChatBackend, type ContextConfig } from './helpers/fake-backend.js'
import { createFakeTransportPlugin, FakeTransportBot } from './helpers/fake-transport.js'
import { ACCOUNT_A, ACCOUNT_B, account, dispatchMessage, waitFor } from './helpers/fixtures.js'
import { simulatedCharacter } from './helpers/simulated-character.js'

const MANAGED = ['19999']

function contexts(): ContextConfig[] {
  return [
    {
      account_id: 'acc_a',
      qq_self_id: '10001',
      conversation_kind: 'group',
      conversation_id: '777',
      active_speaker_account_id: 'acc_a',
      binding_epoch: '7',
      speaker_active: true,
      managed_self_ids: MANAGED
    },
    {
      account_id: 'acc_b',
      qq_self_id: '10002',
      conversation_kind: 'group',
      conversation_id: '777',
      active_speaker_account_id: 'acc_a',
      binding_epoch: '7',
      speaker_active: false,
      managed_self_ids: MANAGED
    },
    {
      account_id: 'acc_a',
      qq_self_id: '10001',
      conversation_kind: 'private',
      conversation_id: '9001',
      binding_epoch: '11',
      speaker_active: true,
      managed_self_ids: MANAGED
    },
    {
      account_id: 'acc_b',
      qq_self_id: '10002',
      conversation_kind: 'private',
      conversation_id: '9002',
      binding_epoch: '12',
      speaker_active: true,
      managed_self_ids: MANAGED
    },
    {
      account_id: 'acc_a',
      qq_self_id: '10001',
      conversation_kind: 'group',
      conversation_id: '778',
      active_speaker_account_id: 'acc_b',
      binding_epoch: '8',
      speaker_active: false,
      managed_self_ids: MANAGED
    },
    {
      account_id: 'acc_b',
      qq_self_id: '10002',
      conversation_kind: 'group',
      conversation_id: '778',
      active_speaker_account_id: 'acc_b',
      binding_epoch: '8',
      speaker_active: true,
      managed_self_ids: MANAGED
    }
  ]
}

interface Harness {
  root: string
  backend: FakeChatBackend
  manager: CharacterAccountRuntimeManager
  logs: string[]
}

async function harness(
  accounts: CharacterRuntimeManagerOptions['accounts'],
  characterConfig: Record<string, unknown>,
  extra: Partial<CharacterRuntimeManagerOptions> = {}
): Promise<Harness> {
  const root = await mkdtemp(join(tmpdir(), 'bba-character-runtime-'))
  const backend = await new FakeChatBackend(
    contexts(),
    new Map(accounts.map((entry) => [entry.accountId, entry.runtimeToken]))
  ).start()
  const logs: string[] = []
  const logger: CharacterRuntimeLogger = {
    debug: (message) => logs.push(message),
    info: (message) => logs.push(message),
    warn: (message) => logs.push(message),
    error: (message) => logs.push(message)
  }
  const manager = new CharacterAccountRuntimeManager({
    stateDir: join(root, 'state'),
    backendBaseUrl: backend.baseUrl,
    accounts: accounts.map(a => ({ ...a, gateway: { baseUrl: backend.baseUrl, token: a.runtimeToken } })),
    startApps: true,
    chatlunaPlugin: null,
    characterPlugin: simulatedCharacter,
    characterConfig,
    logger,
    ...extra
  })
  return { root, backend, manager, logs }
}

async function close(harnessValue: Harness, managerStopped = false): Promise<void> {
  if (!managerStopped) await harnessValue.manager.stop()
  await harnessValue.backend.stop()
  await rm(harnessValue.root, { recursive: true, force: true })
}

function bodyOf(record: { body: Record<string, unknown> | null } | undefined): Record<string, unknown> {
  assert.ok(record)
  assert.ok(record.body)
  return record.body
}

describe('per-account Character runtime and transport gate', () => {
  test('does not start any adapter unless startApps is explicitly true', async () => {
    const value = await harness([ACCOUNT_A], { replies: {} }, { startApps: false })
    try {
      await value.manager.start()
      const runtime = value.manager.runtimes.get('acc_a')
      assert.ok(runtime)
      assert.equal(runtime.bot, undefined)
      assert.equal(value.backend.requests().length, 0)
    } finally {
      await close(value)
    }
  })

  test('allows one text segment through the real Koishi send path and keeps accounts isolated', async () => {
    const value = await harness([ACCOUNT_A, ACCOUNT_B], {
      replies: {
        'group:777': 'hello from A',
        'private:9002': 'private from B'
      }
    })
    try {
      await value.manager.start()
      assert.equal(value.manager.runtimes.size, 2)

      const runtimeA = value.manager.runtimes.get('acc_a')
      const runtimeB = value.manager.runtimes.get('acc_b')
      assert.ok(runtimeA && runtimeB)
      assert.notEqual(runtimeA.app.database, runtimeB.app.database)
      assert.notEqual(runtimeA.directories.root, runtimeB.directories.root)
      assert.notEqual(runtimeA.directories.ledger, runtimeB.directories.ledger)
      const personaA: unknown = JSON.parse(await readFile(join(runtimeA.directories.persona, 'persona.json'), 'utf8'))
      const personaB: unknown = JSON.parse(await readFile(join(runtimeB.directories.persona, 'persona.json'), 'utf8'))
      assert.ok(personaA && typeof personaA === 'object' && 'persona_id' in personaA)
      assert.ok(personaB && typeof personaB === 'object' && 'persona_id' in personaB)
      assert.equal(personaA.persona_id, 'persona_a')
      assert.equal(personaB.persona_id, 'persona_b')
      await stat(runtimeA.directories.cache)
      await stat(runtimeB.directories.cache)

      const botA = runtimeA.bot! as FakeTransportBot
      const botB = runtimeB.bot! as FakeTransportBot
      assert.ok(isTransportGuarded(botA))
      assert.ok(isTransportGuarded(botB))

      dispatchMessage(botA, {
        userId: '20001',
        guildId: '777',
        content: 'hello A',
        messageId: 'evt_a_1'
      })
      await waitFor(() => value.backend.writerCalls.filter(c => c.account === 'acc_a').length === 1)

      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a').length, 1)
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_b').length, 0)
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a')[0]!.method, 'sendMessage')
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a')[0]!.target, '777')
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a')[0]!.text, 'hello from A')

      const trigger = bodyOf(value.backend.requests('trigger')[0])
      assert.equal(trigger.account_id, 'acc_a')
      assert.equal(trigger.conversation_kind, 'group')
      assert.equal(trigger.conversation_id, '777')
      assert.equal(trigger.sender_id, '20001')
      assert.equal(trigger.segment_count, 1)

      const permit = bodyOf(value.backend.requests('permit')[0])
      const digest = createHash('sha256').update('hello from A', 'utf8').digest('hex')
      assert.equal(permit.text_digest, digest)
      assert.equal(permit.expected_binding_epoch, '7')
      assert.equal(permit.segment_index, 0)
      assert.equal(permit.account_id, 'acc_a')
      const gateway = bodyOf(value.backend.requests('gateway')[0])
      assert.equal(gateway.send_id, permit.send_id)
      assert.equal(gateway.text, 'hello from A')
      assert.equal(value.backend.requests('consume').length, 0)
      assert.equal(value.backend.requests('outcome').length, 0)
      await new Promise(resolve => setTimeout(resolve, 30))

      const snapshotA = await runtimeA.gate.snapshot()
      assert.equal(Object.values(snapshotA.sends)[0]?.state, 'sent')
      assert.equal(Object.values(snapshotA.sends)[0]?.message_ids[0], 'fake_msg_1')
      const snapshotB = await runtimeB.gate.snapshot()
      assert.equal(Object.keys(snapshotB.sends).length, 0)

      // The same platform event is not regenerated or sent again.
      dispatchMessage(botA, {
        userId: '20001',
        guildId: '777',
        content: 'hello A',
        messageId: 'evt_a_1'
      })
      await waitFor(() => value.logs.some((line) => line.includes('duplicate trusted event ignored')))
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a').length, 1)
      assert.equal(value.backend.requests('trigger').length, 1)

      // A second consume of the same permit is rejected locally and never reaches the transport.
      const issuedPermit = [...Object.values(snapshotA.sends)][0]!
      await assert.rejects(
        runtimeA.gate.AuthorizeAndConsume(
          runtimeA.gate.eventContext('20001'),
          issuedPermit.permit_id!,
          'group',
          '777',
          'hello from A'
        ),
        (error: unknown) => error instanceof CharacterRuntimeError && error.code === 'backend_rejected'
      )
      assert.equal(value.backend.requests('consume').length, 0)
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a').length, 1)
    } finally {
      await close(value)
    }
  })

  test('private chat uses the private binding epoch and stays on its own account bot', async () => {
    const value = await harness([ACCOUNT_A, ACCOUNT_B], {
      replies: { 'private:9002': 'private from B' }
    })
    try {
      await value.manager.start()
      const runtimeA = value.manager.runtimes.get('acc_a')!
      const runtimeB = value.manager.runtimes.get('acc_b')!
      const botA = runtimeA.bot! as FakeTransportBot
      const botB = runtimeB.bot! as FakeTransportBot

      dispatchMessage(botB, {
        userId: '9002',
        content: 'hello B privately',
        messageId: 'evt_b_private',
        isDirect: true
      })
      await waitFor(() => value.backend.writerCalls.filter(c => c.account === 'acc_b').length === 1)

      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a').length, 0)
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_b').length, 1)
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_b')[0]!.target, 'private:9002')
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_b')[0]!.text, 'private from B')

      const trigger = bodyOf(value.backend.requests('trigger')[0])
      assert.equal(trigger.account_id, 'acc_b')
      assert.equal(trigger.conversation_kind, 'private')
      assert.equal(trigger.conversation_id, '9002')
      assert.equal(trigger.directed_account_id, 'acc_b')

      const permit = bodyOf(value.backend.requests('permit')[0])
      assert.equal(permit.expected_binding_epoch, '12')
      assert.equal(permit.conversation_kind, 'private')
    } finally {
      await close(value)
    }
  })

  test('drops managed bots and cross-account events before any Character generation', async () => {
    const value = await harness([ACCOUNT_A, ACCOUNT_B], { replies: { 'group:777': 'must not send' } })
    try {
      await value.manager.start()
      const runtimeA = value.manager.runtimes.get('acc_a')!
      const botA = runtimeA.bot! as FakeTransportBot

      dispatchMessage(botA, {
        userId: '19999',
        guildId: '777',
        content: 'managed bot',
        messageId: 'evt_managed'
      })
      const managedTick = Promise.withResolvers<void>()
      setImmediate(managedTick.resolve)
      await managedTick.promise
      assert.equal(value.backend.requests('context').length, 0)
      assert.equal(value.backend.requests('trigger').length, 0)
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a').length, 0)

      // Another managed account's self id is blocked locally, without a backend read.
      dispatchMessage(botA, {
        userId: '10002',
        guildId: '777',
        content: 'other managed bot',
        messageId: 'evt_cross'
      })
      const crossTick = Promise.withResolvers<void>()
      setImmediate(crossTick.resolve)
      await crossTick.promise
      assert.equal(value.backend.requests('trigger').length, 0)
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a').length, 0)
    } finally {
      await close(value)
    }
  })

  test('honours the group speaker lease per account', async () => {
    const value = await harness([ACCOUNT_A, ACCOUNT_B], {
      replies: { 'group:778': 'B is the speaker' }
    })
    try {
      await value.manager.start()
      const botA = value.manager.runtimes.get('acc_a')!.bot! as FakeTransportBot
      const botB = value.manager.runtimes.get('acc_b')!.bot! as FakeTransportBot

      dispatchMessage(botA, {
        userId: '20002',
        guildId: '778',
        content: 'not the speaker',
        messageId: 'evt_778_a'
      })
      await waitFor(() => value.logs.some((message) => message.includes('speaker suppressed account=acc_a')))
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a').length, 0)
      assert.equal(value.backend.requests('trigger').length, 0)

      dispatchMessage(botB, {
        userId: '20002',
        guildId: '778',
        content: 'the speaker replies',
        messageId: 'evt_778_b'
      })
      await waitFor(() => value.backend.requests('gateway').length === 1)
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_b')[0]!.text, 'B is the speaker')
      assert.equal(bodyOf(value.backend.requests('trigger')[0]).account_id, 'acc_b')
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_a').length, 0)
    } finally {
      await close(value)
    }
  })

  test('rejects rich outbound content before permit issuance and adapter call', async () => {
    const accountC = account(ACCOUNT_A, {
      accountId: 'acc_c',
      qqSelfId: '10003',
      runtimeId: 'rt_c',
      characterRuntimeId: 'cr_c',
      characterDbId: 'db_c',
      characterCacheNamespace: 'cache_c',
      personaId: 'persona_c',
      persona: 'Persona C',
      runtimeToken: 'token_c',
      adapterInstanceId: 'adapter_c',
      transportPlugin: createFakeTransportPlugin({ selfId: '10003' })
    })
    let outputs = 0
    const value = await harness([accountC], {
      richReplies: { 'group:888': h.image('https://example.invalid/image.png') },
      onOutput: () => outputs += 1
    })
    // The rich group needs its own context.
    value.backend.setContext({
      account_id: 'acc_c',
      qq_self_id: '10003',
      conversation_kind: 'group',
      conversation_id: '888',
      active_speaker_account_id: 'acc_c',
      binding_epoch: '9',
      speaker_active: true,
      managed_self_ids: MANAGED
    })
    try {
      await value.manager.start()
      const runtimeC = value.manager.runtimes.get('acc_c')!
      const botC = runtimeC.bot! as FakeTransportBot

      dispatchMessage(botC, {
        userId: '20003',
        guildId: '888',
        content: 'make it rich',
        messageId: 'evt_rich'
      })
      await waitFor(() => outputs === 1)
      assert.equal(botC.transportCalls.length, 0)
      assert.equal(outputs, 1)
      assert.equal(value.backend.requests('permit').length, 0)
      assert.equal(value.backend.requests('consume').length, 0)
      assert.equal(value.backend.requests('outcome').length, 0)
      const snapshot = await runtimeC.gate.snapshot()
      assert.equal(Object.keys(snapshot.sends).length, 0)
    } finally {
      await close(value)
    }
  })

  test('denies direct adapter internals until the guard restores them', async () => {
    const value = await harness([ACCOUNT_A], { replies: {} })
    try {
      await value.manager.start()
      const runtime = value.manager.runtimes.get('acc_a')!
      const bot = runtime.bot! as FakeTransportBot
      assert.ok(isTransportGuarded(bot))
      const internal = (bot as unknown as { internal: { _request: () => unknown } }).internal
      assert.throws(
        () => internal._request(),
        (error: unknown) => error instanceof CharacterRuntimeError && error.code === 'unguarded_internal_send'
      )

      await value.manager.stop()
      assert.equal(isTransportGuarded(bot), false)
      assert.equal(bot.internal, null)
    } finally {
      await close(value, true)
    }
  })

  test('records an adapter failure as terminal unknown and never retries it', async () => {
    const accountD = account(ACCOUNT_A, {
      accountId: 'acc_d',
      qqSelfId: '10004',
      runtimeId: 'rt_d',
      characterRuntimeId: 'cr_d',
      characterDbId: 'db_d',
      characterCacheNamespace: 'cache_d',
      personaId: 'persona_d',
      persona: 'Persona D',
      runtimeToken: 'token_d',
      adapterInstanceId: 'adapter_d',
      transportPlugin: createFakeTransportPlugin({ selfId: '10004', outcomes: [{ type: 'error', message: 'transport down' }] })
    })
    const value = await harness([accountD], { replies: { 'group:999': 'will fail' } })
    value.backend.unknownWriter = true
    value.backend.setContext({
      account_id: 'acc_d',
      qq_self_id: '10004',
      conversation_kind: 'group',
      conversation_id: '999',
      active_speaker_account_id: 'acc_d',
      binding_epoch: '13',
      speaker_active: true,
      managed_self_ids: MANAGED
    })
    try {
      await value.manager.start()
      const runtimeD = value.manager.runtimes.get('acc_d')!
      const botD = runtimeD.bot! as FakeTransportBot

      dispatchMessage(botD, {
        userId: '20004',
        guildId: '999',
        content: 'fail once',
        messageId: 'evt_unknown'
      })
      await waitFor(() => value.backend.writerCalls.filter(c => c.account === 'acc_d').length === 1)
      await waitFor(() => value.backend.requests('gateway').length === 1)

      await new Promise(resolve => setTimeout(resolve, 30))
      assert.equal(value.backend.requests('outcome').length, 0)
      const snapshot = await runtimeD.gate.snapshot()
      assert.equal(Object.values(snapshot.sends)[0]?.state, 'unknown')
      assert.equal(Object.values(snapshot.triggers)[0]?.state, 'poisoned')

      await assert.rejects(
        botD.sendMessage('999', 'retry after unknown'),
        (error: unknown) => error instanceof CharacterRuntimeError && error.code === 'trigger_poisoned'
      )
      assert.equal(value.backend.writerCalls.filter(c => c.account === 'acc_d').length, 1)
      assert.equal(value.backend.requests('permit').length, 1)
    } finally {
      await close(value)
    }
  })
})
