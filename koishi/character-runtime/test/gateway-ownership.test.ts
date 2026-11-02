import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { ChatBackendClient } from '../src/backend.js'
import { CharacterSendGate } from '../src/gate.js'
import { CharacterSendLedger } from '../src/ledger.js'

test('B3 missing final gateway never falls back to consume then local transport', async () => {
  let local = 0, consume = 0
  const backend = new ChatBackendClient({ baseUrl: 'http://127.0.0.1:1', runtimeToken: 'isolated-test', fetchImpl: async (url) => {
    const path = String(url)
    if (path.endsWith('/consume')) { consume++; return Response.json({ data: { state: 'consumed' } }) }
    return Response.json({ error: { code: 'NOT_FOUND' } }, { status: 404 })
  } })
  const dir = await mkdtemp(join(tmpdir(), 'b3-owner-'))
  const ledger = new CharacterSendLedger(join(dir, 'ledger.json'), 'a', 'r')
  await ledger.load()
  const gate = new CharacterSendGate({ account: { accountId: 'a', runtimeId: 'r', qqSelfId: '10001', persona: 'test' } as never, backend, ledger, maxSegmentCount: 1 })
  await ledger.recordTrigger({ trigger_ref: 't', kind: 'group', conversation_id: '20001', sender_id: '30001', source_event_id: 'e', deadline_at: new Date(Date.now()+60000).toISOString(), segment_count: 1, next_segment: 0, binding_epoch: '1', state: 'open', pending_send_id: null })
  await assert.rejects(gate.sendText({ kind: 'group', conversationId: '20001', text: 'allowed', transport: async () => { local++; return ['wrong'] } }))
  assert.equal(local, 0)
  assert.equal(consume, 0)
  // A legacy consume success is not an authorization API anymore.
  await assert.rejects(backend.consumePermit('permit-test', { send_id: 's', segment_index: 0, text_digest: '0'.repeat(64) }))
  assert.equal(consume, 0)
})
