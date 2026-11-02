import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import test from 'node:test'
import { createCharacterAccountRuntimeManager } from '../src/runtime.js'
import { createFakeProviderPlugin } from './harness/fake-provider.js'
import { createFakeTransportPlugin } from './harness/fake-transport.js'
import type { CharacterAccountOptions } from '../src/types.js'

const packageRoot = dirname(__dirname)
const PROCESS_EVENTS = ['uncaughtException', 'unhandledRejection', 'warning'] as const

function processListenerCounts(): Record<string, number> {
  return Object.fromEntries(PROCESS_EVENTS.map((event) => [event, process.listenerCount(event)]))
}

function account(): CharacterAccountOptions {
  return {
    accountId: 'acc_lifecycle',
    qqSelfId: '10001',
    runtimeId: 'rt_lifecycle',
    characterRuntimeId: 'cr_lifecycle',
    characterDbId: 'db_lifecycle',
    characterCacheNamespace: 'cache_lifecycle',
    personaId: 'PERSONA_LIFECYCLE',
    persona: '你是生命周期测试角色，不发送任何消息。',
    runtimeToken: 'lifecycle-token',
    transportPlugin: createFakeTransportPlugin({ selfId: '10001', sink: [] }),
    transportConfig: { selfId: '10001' },
    characterModel: 'fake/fake-model',
    characterConfig: {
      globalPrivateConfig: {
        enableFixedIntervalTrigger: false,
        enableActivityScoreTrigger: false,
        idleTrigger: { enableLongWaitTrigger: false },
        toolCalling: false,
        messageWaitTime: 0
      },
      globalGroupConfig: {
        enableFixedIntervalTrigger: false,
        enableActivityScoreTrigger: false,
        idleTrigger: { enableLongWaitTrigger: false },
        toolCalling: false,
        messageWaitTime: 0
      }
    }
  }
}

function isChatLunaTeardownError(error: unknown): boolean {
  if (!(error instanceof Error)) return false
  return (
    (error.name === 'TypeError' && /reading '(success|remove|uninstallPlugin)'/.test(error.message)) ||
    /uninstallPlugin/.test(error.message)
  )
}

test('pinned ChatLuna patch is idempotent and keeps the exact tested version', () => {
  const output = execFileSync(process.execPath, [join(packageRoot, 'tools', 'patch-chatluna.cjs')], {
    cwd: packageRoot,
    encoding: 'utf8'
  })
  assert.match(output, /chatluna-patch: (patched|.*already patched|1\.4\.4 teardown guards verified)/)
  assert.doesNotMatch(output, /pattern not found|expected koishi-plugin-chatluna/)
})

test('repeated Character start/stop emits no ChatLuna teardown TypeError and leaks no listeners', { timeout: 60_000 }, async () => {
  const stateDir = await mkdtemp(join(tmpdir(), 'bba-character-lifecycle-'))
  const before = processListenerCounts()

  for (let cycle = 0; cycle < 3; cycle += 1) {
    const manager = createCharacterAccountRuntimeManager({
      stateDir,
      backendBaseUrl: 'http://127.0.0.1:1',
      accounts: [account()],
      startApps: true,
      groupContextBackend: { enabled: false },
      providerPlugins: [
        {
          plugin: createFakeProviderPlugin({ replies: ['<message>不应发送</message>'] })
        }
      ]
    })
    const errors: unknown[] = []
    try {
      await manager.start()
      const runtime = manager.runtimes.get('acc_lifecycle')
      assert.ok(runtime, `cycle ${cycle}: runtime registered`)
      // Cordis reports teardown failures through internal/error. Capture it
      // instead of ignoring stderr so an unexpected lifecycle fault fails here.
      runtime!.app.on('internal/error', (error: unknown) => errors.push(error))
      assert.equal(manager.runtimes.size, 1)
    } finally {
      await manager.stop()
    }
    await new Promise((resolve) => setImmediate(resolve))
    assert.equal(manager.runtimes.size, 0, `cycle ${cycle}: runtimes released`)
    assert.equal(
      errors.filter(isChatLunaTeardownError).length,
      0,
      `cycle ${cycle}: ChatLuna teardown TypeError: ${errors.map(String).join('\n')}`
    )
    assert.deepEqual(processListenerCounts(), before, `cycle ${cycle}: process listener counts unchanged`)
  }
})
