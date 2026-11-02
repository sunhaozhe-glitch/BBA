import assert from 'node:assert/strict'
import test from 'node:test'
import { mkdtemp, mkdir, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { App } from '../src/koishi.js'
import { createDurableFileDatabase } from '../src/durable-db.js'

async function start(file: string) {
  const app = new App()
  app.model.extend('durable_fixture' as never, { id: 'string', value: 'string' } as never, { primary: 'id' } as never)
  app.model.extend('durable_timestamp' as never, { id: 'string', expiresAt: 'timestamp' } as never, { primary: 'id' } as never)
  app.plugin(createDurableFileDatabase(file), {})
  await app.start()
  return app
}

test('durable mutations survive restart and a failed commit blocks subsequent reads and writes', async () => {
  const root = await mkdtemp(join(tmpdir(), 'bba-db-'))
  const file = join(root, 'db.json')
  let app = await start(file)
  try {
    const driver = app.model.drivers[0]!
    const database = app.database
    await database.create('durable_fixture' as never, { id: 'one', value: 'persisted' } as never)
    await database.create('durable_timestamp' as never, { id: 'future', expiresAt: new Date(Date.now() + 60_000) } as never)
    await app.stop()
    await driver.stop()
    await assert.rejects(database.get('durable_fixture' as never, {}))
    app = await start(file)
    assert.deepEqual(await app.database.get('durable_fixture' as never, {}), [{ id: 'one', value: 'persisted' }])
    const unexpired = await app.database.get('durable_timestamp' as never, { expiresAt: { $gt: new Date() } } as never)
    assert.equal(unexpired.length, 1)
    const timestampRow: unknown = unexpired[0]
    assert.ok(timestampRow && typeof timestampRow === 'object' && 'expiresAt' in timestampRow)
    assert.equal(timestampRow.expiresAt instanceof Date, true)
    const previous = await readFile(file, 'utf8')
    await rm(file)
    await mkdir(file)
    await assert.rejects(app.database.set('durable_fixture' as never, {}, { value: 'lost' } as never))
    await assert.rejects(app.database.get('durable_fixture' as never, {}), /unavailable after a write failure/)
    await assert.rejects(app.database.create('durable_fixture' as never, { id: 'two', value: 'blocked' } as never), /unavailable after a write failure/)
    assert.equal(JSON.parse(previous).store.durable_fixture[0].value, 'persisted')
  } finally {
    await app.stop().catch(() => {})
    await rm(root, { recursive: true, force: true })
  }
})

test('durable transaction reads and writes through one atomic callback', async () => {
  const root = await mkdtemp(join(tmpdir(), 'bba-db-transaction-'))
  const file = join(root, 'db.json')
  const app = await start(file)
  const driver = app.model.drivers[0]!
  try {
    const database = app.database
    await database.withTransaction(async (transaction) => {
      const rows = await transaction.get('durable_fixture' as never, {} as never)
      assert.equal(rows.length, 0)
      await transaction.create('durable_fixture' as never, { id: 'atomic', value: 'committed' } as never)
    })
    assert.deepEqual(await database.get('durable_fixture' as never, {} as never), [{ id: 'atomic', value: 'committed' }])
    await assert.rejects(database.withTransaction(async (transaction) => {
      await transaction.set('durable_fixture' as never, { id: 'atomic' } as never, { value: 'rolled-back' } as never)
      throw new Error('abort transaction')
    }), /abort transaction/)
    assert.deepEqual(await database.get('durable_fixture' as never, {} as never), [{ id: 'atomic', value: 'committed' }])
  } finally {
    await app.stop().catch(() => {})
    await driver.stop().catch(() => {})
    await rm(root, { recursive: true, force: true })
  }
})
