import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import type { IncomingMessage, ServerResponse } from 'node:http'
import type { AddressInfo } from 'node:net'
import test from 'node:test'
import { HumanMessage, SystemMessage } from '@langchain/core/messages'
import type { BaseMessage } from '@langchain/core/messages'
import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  createLocalL1ProviderPlugin,
  LocalL1Admission,
  LocalL1Error,
  type LocalL1ProviderOptions
} from '../src/index.js'
import { createCharacterAccountRuntimeManager } from '../src/runtime.js'
import { createFakeTransportPlugin } from './harness/fake-transport.js'
import { startFakeBackend } from './harness/fake-backend.js'

type ResponseMode = 'text' | 'tool-call' | 'http-error'

interface CapturedRequest {
  path: string
  authorization: string | undefined
  body: Record<string, unknown>
  status: number
}

function localL1Options(overrides: Partial<LocalL1ProviderOptions> = {}): LocalL1ProviderOptions {
  return {
    baseUrl: 'http://127.0.0.1:8166/v1',
    apiKey: 'fixture-only-key',
    model: 'fixture-model',
    revision: 'fixture-revision',
    quantization: 'fixture-quantization',
    maxContextTokens: 4096,
    maxOutputTokens: 64,
    requestTimeoutMs: 5000,
    maxConcurrentRequests: 1,
    maxQueuedRequests: 0,
    ...overrides
  }
}

async function readBody(request: IncomingMessage): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = []
  for await (const chunk of request) chunks.push(Buffer.from(chunk))
  return JSON.parse(Buffer.concat(chunks).toString('utf8')) as Record<string, unknown>
}

interface FixtureModelServer {
  baseUrl: string
  requests: CapturedRequest[]
  failNextWithToolCall(): void
  failNextWithHttpError(): void
  close(): Promise<void>
}

async function startFixtureModelServer(): Promise<FixtureModelServer> {
  const requests: CapturedRequest[] = []
  let mode: ResponseMode = 'text'
  const server = createServer(async (request: IncomingMessage, response: ServerResponse) => {
    const body = await readBody(request)
    const captured: CapturedRequest = {
      path: request.url ?? '',
      authorization: request.headers.authorization,
      body,
      status: 200
    }
    requests.push(captured)
    response.setHeader('content-type', 'application/json')
    if (mode === 'http-error') {
      mode = 'text'
      response.statusCode = 503
      captured.status = response.statusCode
      response.end(JSON.stringify({ error: 'fixture provider failure' }))
      return
    }
    if (mode === 'tool-call') {
      mode = 'text'
      response.end(JSON.stringify({
        choices: [{
          index: 0,
          message: {
            role: 'assistant',
            content: null,
            tool_calls: [{ id: 'fixture-call', type: 'function', function: { name: 'forbidden', arguments: '{}' } }]
          },
          finish_reason: 'tool_calls'
        }]
      }))
      return
    }
    response.end(JSON.stringify({
      choices: [{ index: 0, message: { role: 'assistant', content: '<message>simulated local L1 reply</message>' }, finish_reason: 'stop' }],
      usage: { prompt_tokens: 12, completion_tokens: 8, total_tokens: 20 }
    }))
  })
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  const address = server.address() as AddressInfo
  return {
    baseUrl: `http://127.0.0.1:${address.port}/v1`,
    requests,
    failNextWithToolCall(): void { mode = 'tool-call' },
    failNextWithHttpError(): void { mode = 'http-error' },
    close: () => new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()))
  }
}

interface ChatLunaHarness {
  createChatModel(platform: string, model: string): Promise<{
    value?: { invoke(messages: BaseMessage[]): Promise<BaseMessage> }
  }>
}

interface ChatLunaModelFixture {
  modelServer: FixtureModelServer
  model: { invoke(messages: BaseMessage[]): Promise<BaseMessage> }
  close(): Promise<void>
}

async function startChatLunaModelFixture(): Promise<ChatLunaModelFixture> {
  const modelServer = await startFixtureModelServer()
  const backend = await startFakeBackend({ accountId: 'acct-local-l1', qqSelfId: '10001' })
  const stateDir = await mkdtemp(join(tmpdir(), 'bba-local-l1-'))
  const options = {
    baseUrl: modelServer.baseUrl,
    apiKey: 'fixture-only-key',
    model: 'fixture-swift-iq3s',
    revision: 'simulated-revision',
    quantization: 'simulated-iq3_s',
    maxContextTokens: 4096,
    maxOutputTokens: 64,
    requestTimeoutMs: 5000,
    maxConcurrentRequests: 1,
    maxQueuedRequests: 1
  }
  const account = {
    accountId: 'acct-local-l1',
    qqSelfId: '10001',
    runtimeId: 'runtime-local-l1',
    characterRuntimeId: 'character-local-l1',
    characterDbId: 'db-local-l1',
    characterCacheNamespace: 'cache-local-l1',
    personaId: 'PERSONA_LOCAL_L1',
    persona: 'Fixture-only persona. No real model output is used.',
    runtimeToken: 'fixture-runtime-token',
    transportPlugin: createFakeTransportPlugin({ selfId: '10001', sink: [] }),
    transportConfig: { selfId: '10001' },
    characterModel: 'bba-local-l1/fixture-swift-iq3s'
  }
  const manager = createCharacterAccountRuntimeManager({
    stateDir,
    backendBaseUrl: backend.baseUrl,
    accounts: [account],
    startApps: true,
    providerPlugins: [{ plugin: createLocalL1ProviderPlugin(options) }]
  })
  try {
    await manager.start()
    const runtime = manager.runtimes.get(account.accountId)
    if (!runtime) throw new Error('Character runtime did not start')
    const { chatluna } = runtime.app as unknown as { chatluna: ChatLunaHarness }
    const modelRef = await chatluna.createChatModel('bba-local-l1', options.model)
    if (!modelRef.value) throw new Error('registered provider did not resolve through ChatLuna platform service')
    return {
      modelServer,
      model: modelRef.value,
      async close() {
        await manager.stop()
        await backend.close()
        await modelServer.close()
      }
    }
  } catch (error) {
    await manager.stop()
    await backend.close()
    await modelServer.close()
    throw error
  }
}

test('ChatLuna local L1 provider sends bounded text requests and rejects tool output without retry', { timeout: 30_000 }, async (t) => {
  const fixture = await startChatLunaModelFixture()
  t.after(() => fixture.close())
  const { modelServer, model } = fixture

  const output = await model.invoke([
    new SystemMessage('You are a fixture persona.'),
    new HumanMessage('Say hello.')
  ])
  assert.equal(output.content, '<message>simulated local L1 reply</message>')
  assert.equal(modelServer.requests.length, 1)
  assert.equal(modelServer.requests[0]?.path, '/v1/chat/completions')
  assert.equal(modelServer.requests[0]?.authorization, 'Bearer fixture-only-key')
  assert.equal(modelServer.requests[0]?.body.model, 'fixture-swift-iq3s')
  assert.equal(modelServer.requests[0]?.body.max_tokens, 64)
  assert.equal(modelServer.requests[0]?.body.stream, false)
  assert.deepEqual(modelServer.requests[0]?.body.messages, [
    { role: 'system', content: 'You are a fixture persona.' },
    { role: 'user', content: 'Say hello.' }
  ])

  modelServer.failNextWithToolCall()
  await assert.rejects(model.invoke([new HumanMessage('Return a tool call.')]))
  assert.equal(modelServer.requests.length, 2, 'one provider attempt; no retry after a tool-call response')
})

test('ChatLuna local L1 provider does not retry an HTTP failure', { timeout: 30_000 }, async (t) => {
  const fixture = await startChatLunaModelFixture()
  t.after(() => fixture.close())
  fixture.modelServer.failNextWithHttpError()
  await assert.rejects(fixture.model.invoke([new HumanMessage('Trigger an HTTP failure.')]))
  assert.equal(fixture.modelServer.requests.length, 1, 'one provider attempt; no retry or fallback after an HTTP failure')
  assert.equal(fixture.modelServer.requests[0]?.status, 503, 'fixture returned an HTTP failure')
})

test('local L1 admission bounds concurrency and rejects full or expired queue entries', async () => {
  const admission = new LocalL1Admission(1, 1)
  const hold = Promise.withResolvers<void>()
  let queuedOperations = 0
  const active = admission.run(Date.now() + 5000, undefined, () => hold.promise)
  const queued = admission.run(Date.now() + 5000, undefined, async () => { queuedOperations += 1 })
  await assert.rejects(
    admission.run(Date.now() + 5000, undefined, async () => undefined),
    (error: unknown) => error instanceof LocalL1Error && error.code === 'queue_full'
  )
  hold.resolve()
  await Promise.all([active, queued])
  assert.equal(queuedOperations, 1, 'only the admitted FIFO queue entry runs')

  const expiryAdmission = new LocalL1Admission(1, 1)
  const secondHold = Promise.withResolvers<void>()
  const activeAgain = expiryAdmission.run(Date.now() + 5000, undefined, () => secondHold.promise)
  let expiredOperationCalls = 0
  const expired = expiryAdmission.run(Date.now() + 30, undefined, async () => { expiredOperationCalls += 1 })
  await assert.rejects(expired, (error: unknown) => error instanceof LocalL1Error && error.code === 'deadline_exceeded')
  secondHold.resolve()
  await activeAgain
  assert.equal(expiredOperationCalls, 0, 'expired queued work is discarded, not sent later')
})

test('local L1 rejects non-loopback provider endpoints before registration', () => {
  assert.throws(() => createLocalL1ProviderPlugin({
    baseUrl: 'https://example.invalid/v1',
    apiKey: 'fixture-only-key',
    model: 'fixture-model',
    revision: 'fixture-revision',
    quantization: 'fixture-quantization',
    maxContextTokens: 4096,
    maxOutputTokens: 64,
    requestTimeoutMs: 5000,
    maxConcurrentRequests: 1,
    maxQueuedRequests: 0
  }), (error: unknown) => error instanceof LocalL1Error && error.code === 'invalid_config')
})

test('local L1 rejects HTTPS even for a loopback endpoint', () => {
  assert.throws(() => createLocalL1ProviderPlugin({
    baseUrl: 'https://127.0.0.1:8166/v1',
    apiKey: 'fixture-only-key',
    model: 'fixture-model',
    revision: 'fixture-revision',
    quantization: 'fixture-quantization',
    maxContextTokens: 4096,
    maxOutputTokens: 64,
    requestTimeoutMs: 5000,
    maxConcurrentRequests: 1,
    maxQueuedRequests: 0
  }), (error: unknown) => error instanceof LocalL1Error && error.code === 'invalid_config')
})

function rejectsInvalidConfig(overrides: Partial<LocalL1ProviderOptions>): void {
  assert.throws(
    () => createLocalL1ProviderPlugin(localL1Options(overrides)),
    (error: unknown) => error instanceof LocalL1Error && error.code === 'invalid_config'
  )
}

test('local L1 keeps loopback-only default and requires an exact private endpoint approval', () => {
  const privateBase = 'http://192.168.68.106:8166/v1'
  // No approval: a private non-loopback endpoint is still rejected.
  rejectsInvalidConfig({ baseUrl: privateBase })
  // The approval must match the exact origin: different port, different host,
  // implicit port 80 and a loopback approval cannot unlock it.
  rejectsInvalidConfig({ baseUrl: privateBase, approvedEndpoint: 'http://192.168.68.106:8167' })
  rejectsInvalidConfig({ baseUrl: privateBase, approvedEndpoint: 'http://192.168.68.107:8166' })
  rejectsInvalidConfig({ baseUrl: privateBase, approvedEndpoint: 'http://192.168.68.106' })
  rejectsInvalidConfig({ baseUrl: privateBase, approvedEndpoint: 'http://127.0.0.1:8166' })
  // The exact private origin is accepted; an optional /v1 suffix is tolerated.
  for (const approvedEndpoint of [
    'http://192.168.68.106:8166',
    'http://192.168.68.106:8166/',
    'http://192.168.68.106:8166/v1',
    'http://192.168.68.106:8166/v1/'
  ]) {
    const plugin = createLocalL1ProviderPlugin(localL1Options({ baseUrl: privateBase, approvedEndpoint }))
    assert.equal(plugin.name, 'bba-local-l1-provider')
    assert.deepEqual(plugin.inject, ['chatluna'])
  }
  // RFC1918 boundaries are inclusive; adjacent unapproved ranges are rejected.
  for (const origin of ['http://10.0.0.1:8166', 'http://172.16.0.1:8166', 'http://172.31.255.1:8166']) {
    assert.doesNotThrow(() => createLocalL1ProviderPlugin(localL1Options({
      baseUrl: `${origin}/v1`,
      approvedEndpoint: origin
    })))
  }
  for (const origin of ['http://172.15.0.1:8166', 'http://172.32.0.1:8166', 'http://192.169.0.1:8166']) {
    rejectsInvalidConfig({ baseUrl: `${origin}/v1`, approvedEndpoint: origin })
  }
  // An approval for the already-allowed loopback origin is inert, not an error.
  assert.doesNotThrow(() => createLocalL1ProviderPlugin(localL1Options({
    baseUrl: 'http://127.0.0.1:8166/v1',
    approvedEndpoint: 'http://127.0.0.1:8166'
  })))
})

test('local L1 rejects unsafe, public and non-private endpoint approvals', () => {
  const privateBase = 'http://192.168.68.106:8166/v1'
  for (const approvedEndpoint of [
    'https://192.168.68.106:8166',
    'http://model.internal:8166',
    'http://8.8.8.8:8166',
    'http://169.254.1.1:8166',
    'http://[2001:4860:4860::8888]:8166',
    'http://192.168.68.106:8166/other',
    'http://192.168.68.106:8166/?x=1',
    'http://192.168.68.106:8166/#fragment',
    'http://user:pass@192.168.68.106:8166',
    'http://*:8166',
    '',
    '   '
  ]) {
    rejectsInvalidConfig({ baseUrl: privateBase, approvedEndpoint })
  }
  rejectsInvalidConfig({ baseUrl: privateBase, approvedEndpoint: 42 as unknown as string })
  // An approval cannot unlock a public, HTTPS or differently-addressed base.
  rejectsInvalidConfig({
    baseUrl: 'http://8.8.8.8:8166/v1',
    approvedEndpoint: 'http://8.8.8.8:8166'
  })
  rejectsInvalidConfig({
    baseUrl: 'https://192.168.68.106:8166/v1',
    approvedEndpoint: 'http://192.168.68.106:8166'
  })
})

test('local L1 applies base URL guards even when the origin is approved', () => {
  const approvedEndpoint = 'http://192.168.68.106:8166'
  rejectsInvalidConfig({ baseUrl: 'http://192.168.68.106:8166/v2', approvedEndpoint })
  rejectsInvalidConfig({ baseUrl: 'http://192.168.68.106:8166/v1?x=1', approvedEndpoint })
  rejectsInvalidConfig({ baseUrl: 'http://192.168.68.106:8166/v1#fragment', approvedEndpoint })
  rejectsInvalidConfig({ baseUrl: 'http://user:pass@192.168.68.106:8166/v1', approvedEndpoint })
})

test('local L1 accepts an exact IPv6 unique-local approval but not link-local or mapped IPv4', () => {
  const plugin = createLocalL1ProviderPlugin(localL1Options({
    baseUrl: 'http://[fd00::1]:8166/v1',
    approvedEndpoint: 'http://[fd00::1]:8166'
  }))
  assert.equal(plugin.name, 'bba-local-l1-provider')
  rejectsInvalidConfig({
    baseUrl: 'http://[fe80::1]:8166/v1',
    approvedEndpoint: 'http://[fe80::1]:8166'
  })
  rejectsInvalidConfig({
    baseUrl: 'http://[::ffff:192.168.1.1]:8166/v1',
    approvedEndpoint: 'http://[::ffff:192.168.1.1]:8166'
  })
})
