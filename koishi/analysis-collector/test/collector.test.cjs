const assert = require('node:assert/strict')
const { mkdtemp, rm, writeFile } = require('node:fs/promises')
const { createHash } = require('node:crypto')
const { tmpdir } = require('node:os')
const { join } = require('node:path')
const test = require('node:test')
const { Bot, h } = require('koishi')
const {
  startCollector,
  createFixtureSemanticAnalyzer,
  createSemanticSummaryProducer,
  observedActivityFor,
  runSummaryAnalysisJob
} = require('../lib/index.js')

const { Service } = require('koishi')

const puppeteerFixture = (ctx) => {
  ctx.provide('puppeteer', {
    page: async () => {
      throw new Error('P1a fixture has no browser or report-rendering path')
    }
  })
}

class ChatLunaFixture extends Service {
  static [Service.provide] = 'chatluna'
  static inject = []

  constructor(ctx, config) {
    super(ctx, 'chatluna')
    this.config = config
  }
}

async function startFixture(stateDir, overrides = {}) {
  return startCollector({
    stateDir,
    selfId: '10001',
    platform: 'onebot',
    listenerGroups: [{ guildId: '20001', channelId: '20001' }],
    puppeteerPlugin: puppeteerFixture,
    startupTimeoutMs: 5_000,
    logger: { warn() {}, error() {} },
    ...overrides
  })
}

async function withStateDir(callback) {
  const stateDir = await mkdtemp(join(tmpdir(), 'bba-analysis-collector-'))
  try {
    await callback(stateDir)
  } finally {
    await rm(stateDir, { recursive: true, force: true })
  }
}

function emitMessage(runtime, { guildId, selfId = runtime.selfId, content, id }) {
  const bot = new Bot(runtime.app, {}, runtime.platform)
  const session = bot.session({
    type: 'message',
    selfId,
    platform: runtime.platform,
    user: { id: `user-${id}`, name: 'fixture-user' },
    channel: { id: guildId, type: 0 },
    guild: { id: guildId },
    timestamp: Date.now(),
    message: { id, elements: [h.text(content)] }
  })
  runtime.app.emit(session, 'message', session)
  return { bot, session }
}

async function createSummaryManifest(stateDir) {
  const path = join(stateDir, 'source-files.sha256')
  const contents = 'fixture-source.ts  ' + 'a'.repeat(64) + '\n'
  await writeFile(path, contents)
  return { path, digest: createHash('sha256').update(contents).digest('hex') }
}

function summaryFixture(digest, at, refs) {
  const scope = { account_id: '10001', conversation_kind: 'group', conversation_id: '20001' }
  return {
    schema_id: 'Summary.v1', window_start: at.toISOString(), window_end: new Date(at.getTime() + 5).toISOString(),
    collector_scope: scope, privacy_class: 'local_only', retention_days: 1,
    expires_at: new Date(Date.now() + 60_000).toISOString(),
    producer: { account_id: '10001', collector_scope: { ...scope }, plugin_version: '1.4.1', source_manifest_sha256: digest },
    topics: [{ summary: 'Fixture summary', message_refs: refs }], limitations: ['Fixture evidence only']
  }
}

test('P1a stores only listenerGroups messages and blocks every dispatch/send path', async () => {
  await withStateDir(async (stateDir) => {
    const runtime = await startFixture(stateDir, {
      analysisConfig: {
        cronSchedule: '* * * * *',
        registerTools: true,
        userTitleAnalysis: true,
        personaAnalysisMessageInterval: 1,
        enableAllGroupsByDefault: true,
        outputFormat: 'image'
      }
    })
    let downstreamDispatches = 0
    runtime.app.$processor.middleware(async (_session, next) => {
      downstreamDispatches++
      await next()
    })

    try {
      assert.equal(runtime.diagnostics.readyAt !== null, true)
      assert.deepEqual(
        Object.fromEntries(Object.entries(runtime.diagnostics.services).map(([name, service]) => [name, service.status])),
        { database: 'available', chatluna: 'available', puppeteer: 'available', analysis: 'available' }
      )
      assert.deepEqual(
        {
          cronSchedule: runtime.diagnostics.forcedConfig.groupAnalysis.cronSchedule,
          registerTools: runtime.diagnostics.forcedConfig.groupAnalysis.registerTools,
          userTitleAnalysis: runtime.diagnostics.forcedConfig.groupAnalysis.userTitleAnalysis,
          personaAnalysisMessageInterval: runtime.diagnostics.forcedConfig.groupAnalysis.personaAnalysisMessageInterval,
          enableAllGroupsByDefault: runtime.diagnostics.forcedConfig.groupAnalysis.enableAllGroupsByDefault,
          outputFormat: runtime.diagnostics.forcedConfig.groupAnalysis.outputFormat
        },
        {
          cronSchedule: '',
          registerTools: false,
          userTitleAnalysis: false,
          personaAnalysisMessageInterval: 0,
          enableAllGroupsByDefault: false,
          outputFormat: 'text'
        }
      )
      const matrix = new Map(runtime.diagnostics.matrix.map((row) => [row.capability, row]))
      for (const capability of [
        'automatic_analysis_schedule',
        'analysis_tools',
        'automatic_persona_analysis'
      ]) {
        assert.equal(matrix.get(capability).upstreamSwitch, true)
        assert.equal(matrix.get(capability).status, 'disabled')
      }
      for (const capability of [
        'group_analysis_command',
        'persona_manual_command',
        'prompt_variable_group_message_fetch',
        'prompt_variable_group_user_persona',
        'golden_quote_analysis',
        'tool_split'
      ]) {
        assert.equal(matrix.get(capability).upstreamSwitch, false)
        assert.equal(matrix.get(capability).status, 'denied')
      }
      assert.match(runtime.databaseFile, /analysis-10001\.json$/)
      assert.notEqual(runtime.databaseFile, join(stateDir, 'database', 'koishi-db.json'))

      const allowed = emitMessage(runtime, { guildId: '20001', content: 'allowed fixture message', id: 'allowed-1' })
      await runtime.waitForCollectedMessages({ filter: { guildId: '20001' }, minCount: 1, timeoutMs: 3_000 })
      const deniedGroup = emitMessage(runtime, { guildId: '29999', content: 'non-whitelist fixture message', id: 'denied-1' })
      const wrongAccount = emitMessage(runtime, { guildId: '20001', selfId: '10002', content: 'wrong self fixture message', id: 'wrong-account-1' })
      const command = emitMessage(runtime, { guildId: '20001', content: '/群分析', id: 'command-1' })
      const personaCommand = emitMessage(runtime, { guildId: '20001', content: '用户画像', id: 'persona-command-1' })
      await runtime.waitForCollectedMessages({ filter: { guildId: '20001' }, minCount: 3, timeoutMs: 3_000 })

      const allRows = await runtime.database.get('chatluna_messages', {})
      assert.equal(allRows.filter((row) => row.guildId === '20001').length, 3)
      assert.equal(allRows.filter((row) => row.guildId === '29999').length, 0)
      assert.equal(allRows.filter((row) => row.selfId === '10002').length, 0)
      assert.equal(runtime.diagnostics.boundary.dispatchBlocked, 2)
      assert.equal(runtime.diagnostics.boundary.commandsDenied, 2)
      assert.equal(downstreamDispatches, 0)

      for (const bot of [allowed.bot, deniedGroup.bot, wrongAccount.bot, command.bot, personaCommand.bot]) {
        for (const method of ['sendMessage', 'sendPrivateMessage', 'createMessage']) {
          assert.throws(() => bot[method]('20001', 'blocked'), (error) => error.code === 'boundary_denied')
        }
        assert.throws(() => bot.internal.sendMessage, (error) => error.code === 'boundary_denied')
        await bot.dispose()
      }
      assert.equal(runtime.diagnostics.boundary.outboundDenied, 20)
    } finally {
      await runtime.stop()
    }
  })
})
test('P1b message windows page deterministically, resume by timestamp and id, and exclude unstable references', async () => {
  await withStateDir(async (stateDir) => {
    const runtime = await startFixture(stateDir)
    try {
      const owner = await runtime.claimGroupOwner({ guildId: '20001', channelId: '20001', accountId: '10001', ownerToken: 'a'.repeat(48) })
      const at = new Date('2026-10-29T12:00:00.000Z')
      const rows = [
        { id: 'onebot_10001_20001_m-1', platform: 'onebot', selfId: '10001', channelId: '20001', guildId: '20001', userId: 'u2', username: 'B', content: 'second', timestamp: at, messageId: 'm-1' },
        { id: 'onebot_10001_20001_m-2', platform: 'onebot', selfId: '10001', channelId: '20001', guildId: '20001', userId: 'u1', username: 'A', content: 'first', timestamp: at, messageId: 'm-2' },
        { id: 'onebot_10001_20001_missing', platform: 'onebot', selfId: '10001', channelId: '20001', guildId: '20001', userId: 'u2', username: 'B', content: 'unstable', timestamp: new Date(at.getTime() + 1), messageId: null },
        { id: 'onebot_10001_20001_m-4', platform: 'onebot', selfId: '10001', channelId: '20001', guildId: '20001', userId: 'u1', username: 'A', content: 'fourth', timestamp: new Date(at.getTime() + 2), messageId: 'm-4' },
        { id: 'onebot_10001_29999_other', platform: 'onebot', selfId: '10001', channelId: '29999', guildId: '29999', userId: 'u3', username: 'C', content: 'outside', timestamp: at, messageId: 'm-x' }
      ]
      for (const row of rows) await runtime.database.create('chatluna_messages', row)
      const bounds = {
        accountId: '10001', ownerEpoch: owner.ownerEpoch, ownerToken: owner.ownerToken,
        guildId: '20001', channelId: '20001',
        startTime: new Date(at.getTime() - 1), endTime: new Date(at.getTime() + 5),
        pageSize: 1, maxMessages: 3
      }
      const first = await runtime.readCollectedMessageWindow(bounds)
      assert.equal(first.complete, false)
      assert.equal(first.messages.length, 3)
      assert.deepEqual(first.messages.map((message) => message.id), [
        'onebot_10001_20001_m-1', 'onebot_10001_20001_m-2', 'onebot_10001_20001_missing'
      ])
      assert.deepEqual(first.messageRefs, [
        'onebot:10001:20001:20001:m-1', 'onebot:10001:20001:20001:m-2'
      ])
      assert.deepEqual(first.statistics, {
        message_count: 3,
        unique_speaker_count: 2,
        messages_per_speaker: [{ user_id: 'u2', count: 2 }, { user_id: 'u1', count: 1 }],
        most_active_speaker_id: 'u2'
      })

      const resumed = await runtime.readCollectedMessageWindow({
        ...bounds, after: first.nextCursor, maxMessages: 1
      })
      assert.equal(resumed.complete, true)
      assert.deepEqual(resumed.messages.map((message) => message.id), ['onebot_10001_20001_m-4'])
      assert.deepEqual(resumed.messageRefs, ['onebot:10001:20001:20001:m-1', 'onebot:10001:20001:20001:m-2', 'onebot:10001:20001:20001:m-4'])
      assert.equal(resumed.statistics.message_count, 4)
      await assert.rejects(runtime.readCollectedMessageWindow({ ...bounds, after: { timestamp: at, id: 'wrong-cursor' } }), (error) => error.code === 'invalid_options')
      await runtime.stop()
      const restarted = await startFixture(stateDir)
      try {
        const restored = await restarted.claimGroupOwner({ guildId: '20001', channelId: '20001', accountId: '10001', ownerToken: owner.ownerToken })
        assert.equal(restored.ownerEpoch, owner.ownerEpoch)
        const recovered = await restarted.readCollectedMessageWindow({ ...bounds, maxMessages: 1 })
        assert.equal(recovered.complete, true)
        assert.equal(recovered.statistics.message_count, 4)
        assert.deepEqual(recovered.messages, [])
      } finally {
        await restarted.stop()
      }
      assert.equal(resumed.nextCursor, null)
    } finally {
      await runtime.stop()
    }
  })
})

test('Summary persistence binds manifest, window, references and expiry without storing raw message bodies', async () => {
  await withStateDir(async (stateDir) => {
    const manifest = await createSummaryManifest(stateDir)
    const runtime = await startFixture(stateDir, { sourceManifestFile: manifest.path })
    try {
      const owner = await runtime.claimGroupOwner({ guildId: '20001', channelId: '20001', accountId: '10001', ownerToken: '4'.repeat(48) })
      const at = new Date(Date.now() - 60_000)
      await runtime.database.create('chatluna_messages', { id: 'onebot_10001_20001_sum-1', platform: 'onebot', selfId: '10001', channelId: '20001', guildId: '20001', userId: 'u1', username: 'A', content: 'private source body', timestamp: at, messageId: 'sum-1' })
      const window = await runtime.readCollectedMessageWindow({ accountId: '10001', ownerEpoch: owner.ownerEpoch, ownerToken: owner.ownerToken, guildId: '20001', channelId: '20001', startTime: at, endTime: new Date(at.getTime() + 5) })
      assert.equal(window.complete, true)
      const summary = summaryFixture(manifest.digest, window.startTime, ['onebot:10001:20001:20001:sum-1'])
      summary.window_end = window.endTime.toISOString()
      const stored = await runtime.saveSummary(window.id, summary)
      assert.deepEqual((await runtime.listSummaries({ guildId: '20001' })).map((item) => item.id), [stored.id])
      const persistedMessages = await runtime.database.get('bba_collector_window', { id: window.id })
      assert.equal(JSON.stringify(persistedMessages).includes('private source body'), false)
      await assert.rejects(runtime.saveSummary(window.id, { ...summary, collector_scope: { ...summary.collector_scope, account_id: '10002' } }), (error) => error.code === 'summary_invalid')
      await assert.rejects(runtime.saveSummary(window.id, summaryFixture(manifest.digest, at, ['onebot:10001:20001:20001:other'])), (error) => error.code === 'summary_reference_invalid')
      await writeFile(manifest.path, 'changed')
      await assert.rejects(runtime.saveSummary(window.id, summary), (error) => error.code === 'summary_invalid')
    } finally {
      await runtime.stop()
    }
  })
})
test('P1b owner leases reject competing tokens and fence expired-owner windows', async () => {
  await withStateDir(async (stateDir) => {
    const runtime = await startFixture(stateDir)
    try {
      const first = await runtime.claimGroupOwner({ guildId: '20001', channelId: '20001', accountId: '10001', ownerToken: '1'.repeat(48), leaseDurationMs: 1_000 })
      await assert.rejects(runtime.claimGroupOwner({ guildId: '20001', channelId: '20001', accountId: '10001', ownerToken: '2'.repeat(48) }), { code: 'owner_lease_conflict' })
      await runtime.releaseGroupOwner({ guildId: '20001', channelId: '20001', accountId: '10001', ownerToken: first.ownerToken, ownerEpoch: first.ownerEpoch })
      const second = await runtime.claimGroupOwner({ guildId: '20001', channelId: '20001', accountId: '10001', ownerToken: '2'.repeat(48) })
      assert.equal(second.ownerEpoch, first.ownerEpoch + 1)
      await assert.rejects(runtime.renewGroupOwner({ guildId: '20001', channelId: '20001', accountId: '10001', ownerToken: first.ownerToken, ownerEpoch: first.ownerEpoch }), (error) => error.code === 'owner_lease_conflict')
    } finally {
      await runtime.stop()
    }
  })
})
test('P1b window rejects invalid bounds, limits, and continuation cursors', async () => {
  await withStateDir(async (stateDir) => {
    const runtime = await startFixture(stateDir)
    try {
      const owner = await runtime.claimGroupOwner({ guildId: '20001', channelId: '20001', accountId: '10001', ownerToken: '3'.repeat(48) })
      const valid = {
        accountId: '10001', ownerEpoch: owner.ownerEpoch, ownerToken: owner.ownerToken,
        guildId: '20001', channelId: '20001',
        startTime: new Date('2026-10-29T12:00:00.000Z'), endTime: new Date('2026-10-29T13:00:00.000Z')
      }
      for (const options of [
        { ...valid, startTime: new Date(Number.NaN) },
        { ...valid, startTime: valid.endTime, endTime: valid.startTime },
        { ...valid, pageSize: 0 },
        { ...valid, pageSize: 1001 },
        { ...valid, maxMessages: 0 },
        { ...valid, maxMessages: 50_001 },
        { ...valid, after: { timestamp: new Date(Number.NaN), id: 'row' } },
        { ...valid, after: { timestamp: valid.startTime, id: '' } },
        { ...valid, after: { timestamp: new Date(valid.startTime.getTime() - 1), id: 'row' } },
        { ...valid, after: { timestamp: new Date(valid.endTime.getTime() + 1), id: 'row' } }
      ]) {
        await assert.rejects(runtime.readCollectedMessageWindow(options), (error) => error.code === 'invalid_options')
      }
    } finally {
      await runtime.stop()
    }
  })
})

test('P1a state directory rejects concurrent and conflicting selfId use', async () => {
  await withStateDir(async (stateDir) => {
    const runtime = await startFixture(stateDir)
    try {
      await assert.rejects(startFixture(stateDir), (error) => error.code === 'state_dir_in_use')
    } finally {
      await runtime.stop()
    }
    await assert.rejects(
      startFixture(stateDir, { selfId: '10002' }),
      (error) => error.code === 'state_binding_conflict'
    )
  })
})

test('P1a reports missing analysis service injection with bounded diagnostics', async () => {
  await withStateDir(async (stateDir) => {
    await assert.rejects(
      startFixture(stateDir, {
        chatlunaPlugin: ChatLunaFixture,
        analysisPlugin: null,
        startupTimeoutMs: 3_000,
        pollIntervalMs: 5
      }),
      (error) => {
        assert.equal(error.code, 'service_injection_timeout')
        assert.deepEqual(error.details.missing, ['analysis'])
        assert.equal(error.diagnostics.injection.complete, false)
        return true
      }
    )
  })
})

function semanticWindow(overrides = {}) {
  const at = new Date('2026-10-29T12:00:00.000Z')
  const messages = [
    { id: 'onebot_10001_20001_sem-1', platform: 'onebot', selfId: '10001', channelId: '20001', guildId: '20001', userId: 'u1', username: 'A', content: '我们今天讨论本地模型部署', timestamp: at, messageId: 'sem-1' },
    { id: 'onebot_10001_20001_sem-2', platform: 'onebot', selfId: '10001', channelId: '20001', guildId: '20001', userId: 'u2', username: 'B', content: '群规：禁止广告', timestamp: new Date(at.getTime() + 1000), messageId: 'sem-2' }
  ]
  return {
    id: 'b'.repeat(64), platform: 'onebot', selfId: '10001', accountId: '10001',
    ownerEpoch: 1, guildId: '20001', channelId: '20001',
    startTime: at, endTime: new Date(at.getTime() + 5000),
    messages, messageRefs: [
      'onebot:10001:20001:20001:sem-1', 'onebot:10001:20001:20001:sem-2'
    ],
    statistics: {
      message_count: 2,
      unique_speaker_count: 2,
      messages_per_speaker: [{ user_id: 'u1', count: 1 }, { user_id: 'u2', count: 1 }],
      most_active_speaker_id: 'u1'
    },
    complete: true,
    nextCursor: null,
    ...overrides
  }
}

function semanticFixture(input, overrides = {}) {
  const observed = observedActivityFor(input)
  const [firstRef, secondRef] = input.messages.map((message) => message.ref)
  return {
    schema_id: 'SemanticAnalysis.v1',
    provider: 'fixture-semantic-analyzer',
    model: 'fixture-model',
    evidence_source: 'fixture',
    generated_at: input.now.toISOString(),
    expires_at: new Date(Math.min(Date.parse(input.summaryExpiresAt), input.now.getTime() + 3_600_000)).toISOString(),
    window: { start: input.windowStart, end: input.windowEnd, message_count: input.messageCount },
    observed_activity: observed,
    inferred_activity: [{
      start: input.windowStart,
      end: input.windowEnd,
      message_count: input.messageCount,
      basis: 'inferred',
      confidence: 0.62,
      message_refs: [firstRef]
    }],
    topics: [{ kind: 'topic', statement: '群成员在讨论本地模型部署', basis: 'inferred', confidence: 0.74, message_refs: [firstRef] }],
    rules: [{ kind: 'rule', statement: '群规禁止广告', basis: 'observed', confidence: 0.9, message_refs: [secondRef] }],
    pacing: [{ kind: 'pacing', statement: '讨论节奏较快', basis: 'inferred', confidence: 0.6, message_refs: [firstRef] }],
    clarification: [{ kind: 'clarification', statement: '需要澄清活跃时段', basis: 'inferred', confidence: 0.55, message_refs: [firstRef] }],
    limitations: ['fixture semantic analysis for offline tests'],
    execution_authority: 'none',
    ...overrides
  }
}

function semanticAnalyzer() {
  return createFixtureSemanticAnalyzer({
    name: 'fixture-semantic-analyzer',
    model: 'fixture-model',
    analyze: (input) => semanticFixture(input)
  })
}

test('semantic producer keeps deterministic baseline and typed observed/inferred evidence', async () => {
  const window = semanticWindow()
  const producer = createSemanticSummaryProducer({ analyzer: semanticAnalyzer() })
  const production = await producer.produce({
    window,
    now: new Date('2026-10-29T13:00:00.000Z'),
    privacyClass: 'redacted_external_allowed',
    retentionDays: 7,
    pluginVersion: '1.4.1',
    sourceManifestSha256: 'a'.repeat(64)
  })
  assert.equal(production.producerKind, 'hybrid')
  assert.equal(production.semantics.observed_activity.basis, 'observed')
  assert.equal(production.semantics.observed_activity.message_count, 2)
  assert.equal(production.semantics.topics[0].basis, 'inferred')
  assert.equal(production.semantics.rules[0].basis, 'observed')
  assert.equal(production.semantics.topics[0].confidence, 0.74)
  assert.ok(Date.parse(production.semantics.expires_at) <= Date.parse(production.summary.expires_at))
  assert.equal(production.summary.topics.length, production.topics.length)
  assert.ok(production.topics.some((topic) => topic.evidence_kind === 'deterministic_statistic'))
  assert.ok(production.topics.some((topic) => topic.evidence_kind === 'member_statement'))
  assert.ok(production.topics.some((topic) => topic.evidence_kind === 'model_inference'))
  const rendered = production.topics.map((topic) => topic.summary).join('\n')
  assert.match(rendered, /\[群规｜观察｜置信度0\.90\]/)
  assert.match(rendered, /\[话题｜推断｜置信度0\.74\]/)
  assert.match(rendered, /\[活跃时段｜推断/)
  assert.match(rendered, /\[互动节奏｜推断/)
  assert.match(rendered, /\[待澄清｜推断/)
  assert.ok(production.summary.limitations.some((line) => line.includes('显式fixture')))
})

test('semantic producer rejects out-of-window evidence, widened expiry, and local-only inference', async () => {
  const window = semanticWindow()
  const now = new Date('2026-10-29T13:00:00.000Z')
  const base = {
    window, now, privacyClass: 'redacted_external_allowed', retentionDays: 7,
    pluginVersion: '1.4.1', sourceManifestSha256: 'a'.repeat(64)
  }
  const outside = createSemanticSummaryProducer({
    analyzer: createFixtureSemanticAnalyzer({
      name: 'fixture-semantic-analyzer',
      model: 'fixture-model',
      analyze: (input) => {
        const analysis = semanticFixture(input)
        analysis.topics[0].message_refs = ['onebot:10001:20001:20001:other']
        return analysis
      }
    })
  })
  await assert.rejects(outside.produce(base), (error) => error.code === 'summary_invalid')

  const extended = createSemanticSummaryProducer({
    analyzer: createFixtureSemanticAnalyzer({
      name: 'fixture-semantic-analyzer',
      model: 'fixture-model',
      analyze: (input) => semanticFixture(input, {
        expires_at: new Date(Date.parse(input.summaryExpiresAt) + 60_000).toISOString()
      })
    })
  })
  await assert.rejects(extended.produce(base), (error) => error.code === 'summary_invalid')

  const producer = createSemanticSummaryProducer({ analyzer: semanticAnalyzer() })
  await assert.rejects(
    producer.produce({ ...base, privacyClass: 'local_only' }),
    (error) => error.code === 'summary_invalid'
  )
})

test('semantic job persists claims beside the Summary and deduplicates a repeated window', async () => {
  await withStateDir(async (stateDir) => {
    const manifest = await createSummaryManifest(stateDir)
    const runtime = await startFixture(stateDir, { sourceManifestFile: manifest.path })
    try {
      const owner = await runtime.claimGroupOwner({ guildId: '20001', channelId: '20001', accountId: '10001', ownerToken: '5'.repeat(48) })
      const at = new Date(Date.now() - 10 * 60_000)
      for (const [index, content] of ['我们今天讨论本地模型部署', '群规：禁止广告'].entries()) {
        await runtime.database.create('chatluna_messages', {
          id: `onebot_10001_20001_jsem-${index}`, platform: 'onebot', selfId: '10001',
          channelId: '20001', guildId: '20001', userId: `u${index}`, username: `U${index}`,
          content, timestamp: new Date(at.getTime() + index), messageId: `jsem-${index}`
        })
      }
      const window = await runtime.readCollectedMessageWindow({
        accountId: '10001', ownerEpoch: owner.ownerEpoch, ownerToken: owner.ownerToken,
        guildId: '20001', channelId: '20001', startTime: at, endTime: new Date(at.getTime() + 5000)
      })
      const jobOptions = {
        runtime,
        window,
        producer: createSemanticSummaryProducer({ analyzer: semanticAnalyzer() }),
        now: new Date(),
        privacyClass: 'redacted_external_allowed',
        retentionDays: 30
      }
      const first = await runSummaryAnalysisJob(jobOptions)
      assert.equal(first.status, 'stored')
      assert.equal(first.semanticStored, true)
      assert.equal(first.semanticFixture, true)
      assert.equal(first.semantics.rules[0].basis, 'observed')
      const persisted = await runtime.readSemanticAnalysis(first.summaryId)
      assert.equal(persisted.topics[0].confidence, 0.74)
      assert.equal(persisted.execution_authority, 'none')
      assert.equal(
        (await runtime.database.get('bba_semantic_analysis', {})).length,
        1
      )

      const replay = await runSummaryAnalysisJob(jobOptions)
      assert.equal(replay.status, 'stored')
      assert.equal(replay.summaryId, first.summaryId, 'duplicate window keeps the same Summary digest')
      assert.equal((await runtime.database.get('bba_semantic_analysis', {})).length, 1)
      const stored = await runtime.listSummaries({ guildId: '20001' })
      assert.equal(stored.length, 1)
      // Semantic claims persist evidence references, not raw message bodies.
      // Assert the durable citations point back to this exact window.
      assert.deepEqual(persisted.topics[0].message_refs, [window.messageRefs[0]])
      assert.deepEqual(persisted.rules[0].message_refs, [window.messageRefs[1]])
      assert.equal(persisted.window.start, window.startTime.toISOString())
      assert.equal(persisted.window.end, window.endTime.toISOString())

      const mismatch = semanticFixture({
        accountId: '10001', groupId: '20001',
        windowStart: window.startTime.toISOString(), windowEnd: window.endTime.toISOString(),
        messageCount: window.statistics.message_count,
        messages: window.messages.map((message) => ({
          ref: `onebot:10001:20001:20001:${message.messageId}`,
          userId: message.userId, content: message.content, at: message.timestamp.toISOString()
        })),
        summaryExpiresAt: new Date(window.startTime.getTime() + 30 * 86_400_000).toISOString(),
        now: new Date()
      }, { window: { start: window.startTime.toISOString(), end: new Date(window.endTime.getTime() + 1000).toISOString(), message_count: window.statistics.message_count } })
      await assert.rejects(
        runtime.saveSemanticAnalysis(first.summaryId, mismatch),
        (error) => error.code === 'summary_invalid'
      )
    } finally {
      await runtime.stop()
    }
  })
})
