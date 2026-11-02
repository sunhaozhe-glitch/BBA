import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createRequire } from 'node:module'
import { existsSync, readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const load = createRequire(__filename)
test('default package main, exports and declarations resolve to the built gateway implementation', async () => {
  const pkg = load('../package.json')
  const entry = load.resolve('..')
  assert.equal(entry, resolve(__dirname, '..', 'dist/index.js'))
  assert.equal(load.resolve('bba-character-runtime'), entry)
  assert.equal(pkg.main, pkg.exports['.'].default)
  assert.equal(pkg.types, pkg.exports['.'].types)
  assert.ok(existsSync(resolve(__dirname, '..', pkg.types)))
  const api = load('..') as typeof import('../src/index.js')
  assert.equal(load('bba-character-runtime').CharacterSendGate, api.CharacterSendGate)
  assert.match(api.CharacterSendGate.prototype.sendText.toString(), /gatewaySend/)
  assert.doesNotMatch(api.CharacterSendGate.prototype.sendText.toString(), /input\.transport|consumePermit/)
  assert.doesNotMatch(readFileSync(resolve(__dirname, '..', 'dist/gate.js'), 'utf8'), /input\.transport/)
  const client = new api.ChatBackendClient({baseUrl:'http://127.0.0.1:1',runtimeToken:'fixture-unused'})
  await assert.rejects(client.consumePermit('fixture', {} as never), /CONTENT_FINAL_TEXT_REQUIRED/)
})
