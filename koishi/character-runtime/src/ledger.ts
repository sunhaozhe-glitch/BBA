import { mkdir, open, readFile, rename, rm } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { randomBytes } from 'node:crypto'
import { CharacterRuntimeError } from './errors.js'
import type { ConversationKind } from './types.js'
import type { CharacterRuntimeLogger } from './types.js'

export type LedgerSendState = 'issuing' | 'issued' | 'consumed' | 'sent' | 'unknown' | 'rejected'
export type LedgerTriggerState = 'open' | 'closed' | 'poisoned'

export interface LedgerTriggerRecord {
  trigger_ref: string
  kind: ConversationKind
  conversation_id: string
  sender_id: string
  source_event_id: string
  deadline_at: string
  segment_count: number
  next_segment: number
  binding_epoch: string
  state: LedgerTriggerState
  pending_send_id: string | null
}

export interface LedgerSendRecord {
  send_id: string
  trigger_ref: string
  segment_index: number
  text_digest: string
  permit_id: string | null
  state: LedgerSendState
  message_ids: string[]
  updated_at: string
  error_code?: string
}

export interface LedgerSnapshot {
  version: 1
  account_id: string
  runtime_id: string
  triggers: Record<string, LedgerTriggerRecord>
  sends: Record<string, LedgerSendRecord>
}

const EMPTY = (accountId: string, runtimeId: string): LedgerSnapshot => ({
  version: 1,
  account_id: accountId,
  runtime_id: runtimeId,
  triggers: {},
  sends: {}
})

/**
 * Durable, per-account send ledger.
 *
 * Every state transition is written with a temporary file, fsync, rename and
 * directory fsync before the caller is allowed to continue. A restarted
 * runtime therefore observes consumed/unknown send ids and refuses to resend.
 */
export class CharacterSendLedger {
  private snapshot: LedgerSnapshot
  private queue: Promise<void> = Promise.resolve()
  private loaded = false
  private failure: unknown = null

  constructor(
    readonly file: string,
    private readonly accountId: string,
    private readonly runtimeId: string,
    private readonly logger?: CharacterRuntimeLogger
  ) {
    this.snapshot = EMPTY(accountId, runtimeId)
  }

  get path(): string {
    return this.file
  }

  async load(): Promise<void> {
    await this.serialize(async () => {
      await mkdir(dirname(this.file), { recursive: true, mode: 0o700 })
      let raw: string
      try {
        raw = await readFile(this.file, 'utf8')
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === 'ENOENT') {
          this.snapshot = EMPTY(this.accountId, this.runtimeId)
          this.loaded = true
          return
        }
        throw new CharacterRuntimeError('ledger_corrupt', `cannot read send ledger ${this.file}`, {
          cause: error
        })
      }

      let parsed: LedgerSnapshot
      try {
        parsed = JSON.parse(raw) as LedgerSnapshot
      } catch (error) {
        throw new CharacterRuntimeError('ledger_corrupt', `send ledger ${this.file} is not JSON`, {
          cause: error
        })
      }
      if (
        parsed.version !== 1 ||
        parsed.account_id !== this.accountId ||
        parsed.runtime_id !== this.runtimeId ||
        typeof parsed.triggers !== 'object' ||
        typeof parsed.sends !== 'object'
      ) {
        throw new CharacterRuntimeError('ledger_corrupt', `send ledger ${this.file} has a foreign scope`)
      }
      this.snapshot = parsed

      // Fail closed after a crash: any send that reached the backend but whose
      // adapter outcome is not known poisons its trigger permanently.
      let repaired = false
      for (const send of Object.values(this.snapshot.sends)) {
        if (send.state === 'issuing' || send.state === 'issued' || send.state === 'consumed') {
          send.state = 'unknown'
          send.error_code = 'runtime_restart_before_outcome'
          send.updated_at = new Date().toISOString()
          const trigger = this.snapshot.triggers[send.trigger_ref]
          if (trigger) {
            trigger.state = 'poisoned'
            trigger.pending_send_id = null
          }
          repaired = true
        }
      }
      this.loaded = true
      if (repaired) {
        this.logger?.warn?.(`send ledger ${this.file}: repaired unresolved sends as unknown/poisoned`)
        await this.flushLocked()
      }
    })
  }

  async recordTrigger(record: LedgerTriggerRecord): Promise<void> {
    await this.serialize(async () => {
      for (const existing of Object.values(this.snapshot.triggers)) {
        if (
          existing.state === 'open' &&
          !existing.pending_send_id &&
          existing.kind === record.kind &&
          existing.conversation_id === record.conversation_id
        ) {
          existing.state = 'closed'
        }
      }
      this.snapshot.triggers[record.trigger_ref] = record
      await this.flushLocked()
    })
  }

  getTrigger(triggerRef: string): LedgerTriggerRecord | undefined {
    return this.snapshot.triggers[triggerRef]
  }

  findOpenTrigger(kind: ConversationKind, conversationId: string): LedgerTriggerRecord | undefined {
    return Object.values(this.snapshot.triggers).find(
      (record) => record.state === 'open' && record.kind === kind && record.conversation_id === conversationId
    )
  }

  findConversationKinds(conversationId: string): ConversationKind[] {
    const kinds = new Set<ConversationKind>()
    for (const record of Object.values(this.snapshot.triggers)) {
      if (record.conversation_id === conversationId) kinds.add(record.kind)
    }
    return [...kinds]
  }

  findPoisonedTrigger(kind: ConversationKind, conversationId: string): LedgerTriggerRecord | undefined {
    return Object.values(this.snapshot.triggers).find(
      (record) => record.state === 'poisoned' && record.kind === kind && record.conversation_id === conversationId
    )
  }

  listTriggers(): LedgerTriggerRecord[] {
    return Object.values(this.snapshot.triggers).map((record) => ({ ...record }))
  }

  getSend(sendId: string): LedgerSendRecord | undefined {
    return this.snapshot.sends[sendId]
  }

  async beginSend(record: LedgerSendRecord): Promise<void> {
    await this.serialize(async () => {
      const existing = this.snapshot.sends[record.send_id]
      if (existing && existing.state !== 'rejected') {
        throw new CharacterRuntimeError(
          'permit_already_used',
          `send id ${record.send_id} already exists in the durable ledger`
        )
      }
      this.snapshot.sends[record.send_id] = record
      await this.flushLocked()
    })
  }

  async markIssued(sendId: string, permitId: string): Promise<void> {
    await this.mutateSend(sendId, (record) => {
      record.state = 'issued'
      record.permit_id = permitId
    })
  }

  async markConsumed(sendId: string, permitId: string): Promise<void> {
    await this.mutateSend(sendId, (record) => {
      if (record.state !== 'issued' || record.permit_id !== permitId) {
        throw new CharacterRuntimeError('permit_already_used', 'send is already claimed')
      }
      record.state = 'consumed'
      record.permit_id = permitId
      const trigger = this.snapshot.triggers[record.trigger_ref]
      if (trigger) {
        trigger.pending_send_id = sendId
        trigger.next_segment = Math.max(trigger.next_segment, record.segment_index + 1)
      }
    })
  }

  async markSent(sendId: string, messageIds: string[]): Promise<void> {
    await this.mutateSend(sendId, (record) => {
      record.state = 'sent'
      record.message_ids = [...messageIds]
      const trigger = this.snapshot.triggers[record.trigger_ref]
      if (trigger && trigger.pending_send_id === sendId) trigger.pending_send_id = null
    })
  }

  async markRejected(sendId: string, errorCode: string): Promise<void> {
    await this.mutateSend(sendId, (record) => {
      record.state = 'rejected'
      record.error_code = errorCode
      const trigger = this.snapshot.triggers[record.trigger_ref]
      if (trigger) { trigger.state = 'closed'; trigger.pending_send_id = null }
    })
  }

  async markUnknown(sendId: string, errorCode: string): Promise<void> {
    await this.mutateSend(sendId, (record) => {
      record.state = 'unknown'
      record.error_code = errorCode
      const trigger = this.snapshot.triggers[record.trigger_ref]
      if (trigger) {
        trigger.state = 'poisoned'
        trigger.pending_send_id = null
      }
    })
  }

  /**
   * Keep the send's terminal state (for example `sent`) but poison its trigger
   * so a plugin-level retry after a failed mandatory outcome can never obtain a
   * new permit or reach the adapter twice.
   */
  async poisonTriggerForSend(sendId: string, errorCode: string): Promise<void> {
    await this.serialize(async () => {
      const send = this.snapshot.sends[sendId]
      if (!send) return
      const trigger = this.snapshot.triggers[send.trigger_ref]
      if (!trigger) return
      trigger.state = 'poisoned'
      trigger.pending_send_id = null
      send.updated_at = new Date().toISOString()
      send.error_code = errorCode
      await this.flushLocked()
    })
  }

  async closeTrigger(triggerRef: string): Promise<void> {
    await this.serialize(async () => {
      const trigger = this.snapshot.triggers[triggerRef]
      if (trigger && trigger.state === 'open') {
        trigger.state = 'closed'
        await this.flushLocked()
      }
    })
  }

  async snapshotNow(): Promise<LedgerSnapshot> {
    await this.serialize(async () => {})
    return JSON.parse(JSON.stringify(this.snapshot)) as LedgerSnapshot
  }

  private async mutateSend(sendId: string, mutate: (record: LedgerSendRecord) => void): Promise<void> {
    await this.serialize(async () => {
      const record = this.snapshot.sends[sendId]
      if (!record) {
        throw new CharacterRuntimeError('ledger_corrupt', `send ${sendId} is missing from the ledger`)
      }
      mutate(record)
      record.updated_at = new Date().toISOString()
      await this.flushLocked()
    })
  }

  private serialize(task: () => Promise<void>): Promise<void> {
    const checked = async () => {
      if (this.failure) throw new CharacterRuntimeError('ledger_corrupt', 'send ledger unavailable after persistence failure', { cause: this.failure })
      await task()
    }
    const next = this.queue.then(checked, checked)
    this.queue = next.catch(() => {})
    return next
  }

  private async flushLocked(): Promise<void> {
    if (!this.loaded) return
    const data = JSON.stringify(this.snapshot)
    const temp = join(dirname(this.file), `.${randomBytes(8).toString('hex')}.tmp`)
    try {
      const handle = await open(temp, 'wx', 0o600)
      try {
        await handle.writeFile(data, 'utf8')
        await handle.sync()
      } finally {
        await handle.close()
      }
      await rename(temp, this.file)
      const directory = await open(dirname(this.file), 'r')
      try {
        await directory.sync()
      } finally {
        await directory.close()
      }
    } catch (error) {
      this.failure = error
      await rm(temp, { force: true }).catch(() => {})
      throw new CharacterRuntimeError('ledger_corrupt', `cannot persist send ledger ${this.file}`, {
        cause: error
      })
    }
  }
}
