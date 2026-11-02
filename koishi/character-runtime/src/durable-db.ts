import { randomBytes } from 'node:crypto'
import { AsyncLocalStorage } from 'node:async_hooks'
import { mkdir, open, readFile, rename, rm } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { Database } from 'minato'
import { MemoryDriver } from '@minatojs/driver-memory'

export interface DurableSetResult {
  matched: number
}

export interface DurableRemoveResult {
  removed: number
  matched: number
}

export interface DurableUpsertResult {
  inserted: number
  matched: number
}

/** Per-account Minato memory semantics with an atomic, fsync-backed snapshot. */
export function createDurableFileDatabase(file: string): typeof MemoryDriver {
  class DurableFileDriver extends MemoryDriver {
    static override name = 'bba-durable-file'

    private persistQueue: Promise<void> = Promise.resolve()
    private mutationQueue: Promise<void> = Promise.resolve()
    private mutationContext = new AsyncLocalStorage<{ active: boolean }>()
    private loaded = false
    private writeFailure?: unknown
    private stopping = false
    private stopTask?: Promise<void>
    private transactionDepth = 0

    override async start(): Promise<void> {
      this.stopping = false
      this.stopTask = undefined
      await super.start()
      let raw: string
      try {
        raw = await readFile(file, 'utf8')
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error
        raw = ''
      }
      if (raw) {
        let parsed: unknown
        try {
          parsed = JSON.parse(raw)
        } catch (error) {
          throw new Error(`durable character database ${file} is not JSON`, { cause: error })
        }
        const snapshot = parsed as { version?: unknown; store?: unknown; indexes?: unknown }
        if (
          !snapshot ||
          typeof snapshot !== 'object' ||
          snapshot.version !== 1 ||
          !snapshot.store ||
          typeof snapshot.store !== 'object' ||
          !snapshot.indexes ||
          typeof snapshot.indexes !== 'object'
        ) {
          throw new Error(`durable character database ${file} has an unknown format`)
        }
        const store = snapshot.store as Record<string, unknown>
        for (const [table, value] of Object.entries(store)) {
          const fields = this.database.tables[table]?.fields
          if (!Array.isArray(value) || !fields) continue
          const timestamps = Object.entries(fields)
            .filter(([, field]) => field?.type.type === 'timestamp')
            .map(([name]) => name)
          for (const row of value) {
            if (!row || typeof row !== 'object' || Array.isArray(row)) continue
            for (const field of timestamps) {
              const stored = (row as Record<string, unknown>)[field]
              if (typeof stored !== 'string') continue
              const restored = new Date(stored)
              if (Number.isFinite(restored.getTime())) (row as Record<string, unknown>)[field] = restored
            }
          }
        }
        this._store = store as typeof this._store
        this._indexes = snapshot.indexes as typeof this._indexes

      }
      this.loaded = true
      await this.persist()
    }

    // MemoryDriver does not await $save(). Public mutation APIs below perform
    // the durable commit themselves after MemoryDriver has applied the change.
    override $save(_name: string): Promise<void> {
      return Promise.resolve()
    }

    override async get(...args: Parameters<MemoryDriver['get']>): Promise<any[]> {
      const activeMutation = this.mutationContext.getStore()?.active === true || this.inTransaction()
      if (!activeMutation) await this.mutationQueue
      if ((this.stopping && !activeMutation) || !this.loaded) {
        throw new Error(`durable character database ${file} is not available`)
      }
      this.assertFailureFree()
      return super.get(...args)
    }

    override async eval(...args: Parameters<MemoryDriver['eval']>): Promise<any> {
      const activeMutation = this.mutationContext.getStore()?.active === true || this.inTransaction()
      if (!activeMutation) await this.mutationQueue
      if ((this.stopping && !activeMutation) || !this.loaded) {
        throw new Error(`durable character database ${file} is not available`)
      }
      this.assertFailureFree()
      return super.eval(...args)
    }

    override set(...args: Parameters<MemoryDriver['set']>): Promise<DurableSetResult> {
      return this.mutate(() => super.set(...args))
    }

    override remove(...args: Parameters<MemoryDriver['remove']>): Promise<DurableRemoveResult> {
      return this.mutate(() => super.remove(...args))
    }

    override create(...args: Parameters<MemoryDriver['create']>): Promise<any> {
      return this.mutate(() => super.create(...args))
    }

    override upsert(...args: Parameters<MemoryDriver['upsert']>): Promise<DurableUpsertResult> {
      return this.mutate(() => super.upsert(...args))
    }
    override drop(...args: Parameters<MemoryDriver['drop']>): Promise<void> {
      return this.mutate(() => super.drop(...args))
    }

    override dropAll(...args: Parameters<MemoryDriver['dropAll']>): Promise<void> {
      return this.mutate(() => super.dropAll(...args))
    }

    override createIndex(...args: Parameters<MemoryDriver['createIndex']>): Promise<void> {
      return this.mutate(() => super.createIndex(...args))
    }

    override dropIndex(...args: Parameters<MemoryDriver['dropIndex']>): Promise<void> {
      return this.mutate(() => super.dropIndex(...args))
    }

    override withTransaction(callback: () => Promise<void>): Promise<void> {
      return this.mutate(async () => {
        this.transactionDepth++
        try {
          await this.mutationContext.run({ active: true }, () => super.withTransaction(() =>
            this.mutationContext.run({ active: true }, callback)))
        } finally {
          this.transactionDepth--
        }
      })
    }

    override stop(): Promise<void> {
      if (this.stopTask) return this.stopTask
      this.stopping = true
      this.stopTask = (async () => {
        await this.mutationQueue
        if (this.loaded) await this.persist()
        this.loaded = false
        await super.stop()
      })()
      return this.stopTask
    }

    private inTransaction(): boolean {
      const database = this.database as typeof this.database & { constructor: { transact?: symbol } }
      const marker = database.constructor.transact
      return marker !== undefined && (database as unknown as Record<symbol, unknown>)[marker] === true
    }

    private mutate<T>(operation: () => Promise<T>): Promise<T> {
      if (this.mutationContext.getStore()?.active === true || this.inTransaction()) return operation()
      if (this.stopping || !this.loaded) {
        return Promise.reject(new Error(`durable character database ${file} is not available`))
      }
      const next = this.mutationQueue.then(() =>
        this.mutationContext.run({ active: true }, async () => {
          try {
            this.assertFailureFree()
            const result = await operation()
            if (this.transactionDepth === 0) await this.persist()
            this.assertFailureFree()
            return result
          } finally {
            const context = this.mutationContext.getStore()
            if (context) context.active = false
          }
        })
      )
      this.mutationQueue = next.then(
        () => undefined,
        () => undefined
      )
      return next
    }

    private assertFailureFree(): void {
      if (this.writeFailure !== undefined) {
        throw new Error(`durable character database ${file} is unavailable after a write failure`, {
          cause: this.writeFailure
        })
      }
    }

    private persist(): Promise<void> {
      const next = this.persistQueue.then(async () => {
        this.assertFailureFree()
        const directory = dirname(file)
        await mkdir(directory, { recursive: true, mode: 0o700 })
        const body = JSON.stringify({ version: 1, store: this._store, indexes: this._indexes })
        const temp = join(directory, `.${randomBytes(8).toString('hex')}.tmp`)
        try {
          const handle = await open(temp, 'wx', 0o600)
          try {
            await handle.writeFile(body, 'utf8')
            await handle.sync()
          } finally {
            await handle.close()
          }
          await rename(temp, file)
          const dirHandle = await open(directory, 'r')
          try {
            await dirHandle.sync()
          } finally {
            await dirHandle.close()
          }
        } catch (error) {
          await rm(temp, { force: true }).catch(() => {})
          this.writeFailure ??= error
          throw error
        }
      })
      this.persistQueue = next.catch((error: unknown) => {
        this.writeFailure ??= error
      })
      return next
    }
  }

  return DurableFileDriver
}
