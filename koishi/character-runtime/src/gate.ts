import { ChatBackendClient, validatePermit } from './backend.js'
import { assertText, sendIdFor, textDigest, triggerRefFor, MAX_SEGMENT_COUNT } from './digest.js'
import { BackendHttpError, CharacterRuntimeError } from './errors.js'
import type { CharacterSendLedger, LedgerTriggerRecord } from './ledger.js'
import type {
  CharacterAccountOptions,
  CharacterEventContext,
  CharacterRuntimeLogger,
  ChatPermitRequest,
  ChatTrigger,
  ConversationKind
} from './types.js'

export interface CharacterSendGateOptions {
  account: CharacterAccountOptions
  backend: ChatBackendClient
  ledger: CharacterSendLedger
  logger?: CharacterRuntimeLogger
  now?: () => number
  maxSegmentCount?: number
  triggerTtlMs?: number
  /** Extra self ids ignored at ingress (own account self id is always included). */
  managedSelfIds?: Iterable<string>
}

export interface TriggerRequestInput {
  kind: ConversationKind
  conversationId: string
  senderId: string
  sourceEventId: string
  directedAccountId?: string | null
  deadlineAt?: string
  segmentCount?: number
}

export interface SendTextInput {
  kind: ConversationKind
  conversationId: string
  text: string
  /** Trusted event context; defaults to this account's own runtime context. */
  context?: CharacterEventContext
  /** @deprecated Ignored. Only the final gateway may execute a writer. */
  transport?: (text: string) => Promise<string[] | void>
}

export interface IssuedPermit {
  send_id: string
  permit_id: string
  segment_index: number
  text_digest: string
}

const DEFAULT_TRIGGER_TTL_MS = 120_000

/**
 * Character send gate: trusted-trigger admission, text-only one-shot durable
 * permits and the explicit final authorization boundary.
 *
 * sendText/send submit exact text and scope to an explicitly configured final
 * gateway. Local consumption, client writers and client outcome reports are retired.
 */
export class CharacterSendGate {
  readonly account: CharacterAccountOptions
  private readonly backend: ChatBackendClient
  private readonly ledger: CharacterSendLedger
  private readonly logger?: CharacterRuntimeLogger
  private readonly clock: () => number
  private readonly maxSegmentCount: number
  private readonly triggerTtlMs: number
  private readonly managedSelfIds: Set<string>

  constructor(options: CharacterSendGateOptions) {
    this.account = options.account
    this.backend = options.backend
    this.ledger = options.ledger
    this.logger = options.logger
    this.clock = options.now ?? (() => Date.now())
    this.maxSegmentCount = options.maxSegmentCount ?? 1
    if (
      !Number.isInteger(this.maxSegmentCount) ||
      this.maxSegmentCount < 1 ||
      this.maxSegmentCount > MAX_SEGMENT_COUNT
    ) {
      throw new CharacterRuntimeError(
        'invalid_config',
        `maxSegmentCount must be an integer in 1..${MAX_SEGMENT_COUNT}`
      )
    }
    this.triggerTtlMs = options.triggerTtlMs ?? DEFAULT_TRIGGER_TTL_MS
    this.managedSelfIds = new Set([options.account.qqSelfId, ...(options.managedSelfIds ?? [])])
  }

  /** Persona is owned by exactly one runtime; it is never read from a shared store. */
  getPersona(): string {
    return this.account.persona
  }

  addManagedSelfIds(selfIds: Iterable<string>): void {
    for (const selfId of selfIds) {
      if (typeof selfId === 'string' && selfId.length > 0) this.managedSelfIds.add(selfId)
    }
  }

  isManagedSelfId(id: string | undefined | null): boolean {
    return typeof id === 'string' && this.managedSelfIds.has(id)
  }

  /** Trusted event context for this runtime, with the sending principal. */
  eventContext(senderId?: string): CharacterEventContext {
    return {
      accountId: this.account.accountId,
      runtimeId: this.account.runtimeId,
      adapterId: this.account.adapterId ?? 'koishi',
      adapterInstanceId: this.account.adapterInstanceId ?? this.account.runtimeId,
      senderId: senderId ?? this.account.qqSelfId
    }
  }

  /**
   * Register a trusted trigger for one inbound platform event.
   *
   * Returns the open trigger when this account is the active speaker, or null
   * when the event must be ignored (managed sender, paused chat, another
   * speaker, or closed backend trigger). Ingress drops the session on null.
   */
  async openTriggerForEvent(input: TriggerRequestInput): Promise<ChatTrigger | null> {
    if (input.kind !== 'group' && input.kind !== 'private') {
      throw new CharacterRuntimeError('invalid_config', `unknown conversation kind ${String(input.kind)}`)
    }
    if (!input.conversationId || !input.senderId || !input.sourceEventId) {
      throw new CharacterRuntimeError('invalid_binding', 'trigger event identity is incomplete')
    }
    if (this.isManagedSelfId(input.senderId)) {
      this.logger?.debug?.(`managed sender ignored account=${this.account.accountId} sender=${input.senderId}`)
      return null
    }

    const triggerRef = triggerRefFor(
      this.account.runtimeId,
      this.account.accountId,
      input.sourceEventId,
      input.kind,
      input.conversationId
    )
    if (this.ledger.getTrigger(triggerRef)) {
      this.logger?.debug?.(
        `duplicate trusted event ignored account=${this.account.accountId} trigger=${triggerRef}`
      )
      return null
    }

    if ((input.segmentCount ?? this.maxSegmentCount) !== 1) throw new CharacterRuntimeError('rich_content_rejected', 'final gateway supports one text segment only')
    const context = await this.backend.getContext({
      accountId: this.account.accountId,
      kind: input.kind,
      conversationId: input.conversationId
    })
    if (Array.isArray(context.managed_self_ids)) this.addManagedSelfIds(context.managed_self_ids)

    if (context.account_id !== this.account.accountId || context.qq_self_id !== this.account.qqSelfId) {
      throw new CharacterRuntimeError('cross_account_event', 'chat context belongs to another account')
    }
    if (this.isManagedSelfId(input.senderId)) {
      this.logger?.debug?.(`managed sender ignored account=${this.account.accountId} sender=${input.senderId}`)
      return null
    }
    if (!context.chat_enabled) {
      this.logger?.debug?.(`chat disabled for ${input.kind}:${input.conversationId}`)
      return null
    }

    let bindingEpoch: string | null
    if (input.kind === 'group') {
      if (
        !context.speaker_active ||
        context.active_speaker_account_id !== this.account.accountId ||
        !context.binding_epoch
      ) {
        this.logger?.debug?.(
          `speaker suppressed account=${this.account.accountId} conv=${input.conversationId} active=${String(
            context.active_speaker_account_id
          )}`
        )
        return null
      }
      bindingEpoch = context.binding_epoch
    } else {
      // A private conversation has no group speaker lease. The backend still
      // supplies the account owner epoch as binding_epoch; never invent one.
      bindingEpoch = context.binding_epoch
    }
    if (!bindingEpoch || !/^[1-9][0-9]*$/.test(bindingEpoch)) {
      throw new CharacterRuntimeError(
        'invalid_binding',
        'chat context returned no decimal binding epoch for this conversation'
      )
    }

    const deadlineAt = input.deadlineAt ?? new Date(this.clock() + this.triggerTtlMs).toISOString()
    if (Date.parse(deadlineAt) <= this.clock()) {
      throw new CharacterRuntimeError('trigger_expired', 'trigger deadline is not in the future')
    }
    const segmentCount = input.segmentCount ?? this.maxSegmentCount
    if (segmentCount !== 1) throw new CharacterRuntimeError('rich_content_rejected', 'final gateway supports one text segment only')
    if (
      !Number.isInteger(segmentCount) ||
      segmentCount < 1 ||
      segmentCount > this.maxSegmentCount ||
      segmentCount > MAX_SEGMENT_COUNT
    ) {
      throw new CharacterRuntimeError(
        'invalid_config',
        `segment count ${segmentCount} is outside 1..${this.maxSegmentCount}`
      )
    }

    const trigger = await this.backend.openTrigger({
      trigger_ref: triggerRef,
      account_id: this.account.accountId,
      conversation_kind: input.kind,
      conversation_id: input.conversationId,
      source_event_id: input.sourceEventId,
      sender_id: input.senderId,
      directed_account_id: input.directedAccountId ?? null,
      deadline_at: deadlineAt,
      segment_count: segmentCount
    })

    if (trigger.state !== 'open') return null

    await this.ledger.recordTrigger({
      trigger_ref: trigger.trigger_ref,
      kind: input.kind,
      conversation_id: input.conversationId,
      sender_id: input.senderId,
      source_event_id: input.sourceEventId,
      deadline_at: trigger.deadline_at,
      segment_count: trigger.segment_count,
      next_segment: trigger.next_segment,
      binding_epoch: bindingEpoch,
      state: 'open',
      pending_send_id: null
    })
    return trigger
  }

  /**
   * Scheduler-originated (proactive) send. The trigger still comes from the
   * trusted backend; there is no local "proactive bypass".
   */
  async beginProactive(input: TriggerRequestInput): Promise<ChatTrigger | null> {
    return await this.openTriggerForEvent(input)
  }

  /**
   * Issue the durable one-shot permit for the trigger's next text segment.
   *
   * Issuance alone is not permission for a local writer. sendText submits this
   * exact permit and payload to the final gateway for inspection and execution.
   */
  async issuePermitForText(
    context: CharacterEventContext,
    kind: ConversationKind,
    conversationId: string,
    text: string
  ): Promise<IssuedPermit> {
    this.assertEventContext(context)
    const value = assertText(text)
    const trigger = this.requireOpenTrigger(kind, conversationId)
    if (trigger.segment_count !== 1 || trigger.next_segment !== 0) {
      throw new CharacterRuntimeError('rich_content_rejected', 'final gateway supports one text segment only')
    }
    const segmentIndex = trigger.next_segment
    if (segmentIndex >= trigger.segment_count) {
      throw new CharacterRuntimeError(
        'segment_budget_exhausted',
        `trigger ${trigger.trigger_ref} already used ${segmentIndex}/${trigger.segment_count} segments`
      )
    }

    const digest = textDigest(value)
    const sendId = sendIdFor(trigger.trigger_ref, segmentIndex)
    const existing = this.ledger.getSend(sendId)
    if (existing && existing.state !== 'rejected') {
      if (existing.state === 'issued' && existing.permit_id && existing.text_digest === digest) {
        return {
          send_id: sendId,
          permit_id: existing.permit_id,
          segment_index: existing.segment_index,
          text_digest: digest
        }
      }
      throw new CharacterRuntimeError(
        'permit_already_used',
        `send ${sendId} is already recorded; a consumed or unknown segment is never resent`
      )
    }

    const request: ChatPermitRequest = {
      send_id: sendId,
      account_id: this.account.accountId,
      conversation_kind: kind,
      conversation_id: conversationId,
      trigger_ref: trigger.trigger_ref,
      text_digest: digest,
      expected_binding_epoch: trigger.binding_epoch,
      deadline_at: trigger.deadline_at,
      segment_index: segmentIndex,
      segment_count: trigger.segment_count
    }

    await this.ledger.beginSend({
      send_id: sendId,
      trigger_ref: trigger.trigger_ref,
      segment_index: segmentIndex,
      text_digest: digest,
      permit_id: null,
      state: 'issuing',
      message_ids: [],
      updated_at: new Date(this.clock()).toISOString()
    })

    try {
      const permit = await this.backend.issuePermit(request)
      validatePermit(permit, request, this.clock())
      if (permit.state !== 'issued') {
        throw new CharacterRuntimeError('permit_mismatch', `permit state is ${permit.state}, expected issued`)
      }
      await this.ledger.markIssued(sendId, permit.permit_id)
      return {
        send_id: sendId,
        permit_id: permit.permit_id,
        segment_index: segmentIndex,
        text_digest: digest
      }
    } catch (error) {
      const code = error instanceof CharacterRuntimeError ? error.code : 'permit_rejected'
      try {
        await this.ledger.markRejected(sendId, code)
      } catch (ledgerError) {
        this.logger?.error?.('cannot record rejected send', ledgerError)
      }
      throw error
    }
  }

  /** Retired: a local consume is never permission for a client writer. */
  async AuthorizeAndConsume(
    _context: CharacterEventContext, _permitID: string, _kind: ConversationKind,
    _conversationId: string, _text: string
  ): Promise<void> {
    throw new CharacterRuntimeError('backend_rejected', 'CONTENT_FINAL_TEXT_REQUIRED: use final gateway send')
  }

  /** Retired: clients cannot overwrite gateway-owned delivery facts. */
  async reportDelivery(_permitID: string, _sendID: string, _state: 'sent' | 'unknown', _messageIds: string[] = []): Promise<void> {
    throw new CharacterRuntimeError('backend_rejected', 'delivery facts belong to the final gateway')
  }

  async sendText(input: SendTextInput): Promise<string[]> {
    const text = assertText(input.text)
    const context = input.context ?? this.eventContext()
    const issued = await this.issuePermitForText(context, input.kind, input.conversationId, text)
    // Durable claim BEFORE any mutating network request. Concurrent duplicates
    // cannot claim it twice; a crash leaves unknown, never a new send id.
    await this.ledger.markConsumed(issued.send_id, issued.permit_id)
    let receipt: { state: 'sent'; message_id: string }
    try {
      receipt = await this.backend.gatewaySend(issued.permit_id, {
        send_id: issued.send_id, segment_index: issued.segment_index, text,
        account_id: this.account.accountId, conversation_kind: input.kind,
        conversation_id: input.conversationId
      })
    } catch (error) {
      const denied = error instanceof BackendHttpError && error.status >= 400 && error.status < 500 &&
        !['EXTERNAL_OUTCOME_UNKNOWN', 'CHAT_PERMIT_CONSUMED'].includes(error.backendCode ?? '')
      // Config/capability errors are explicit denials; transport/5xx/malformed
      // receipts can follow a successful writer and must remain unknown.
      const noGateway = error instanceof CharacterRuntimeError && error.code === 'backend_rejected' && !(error instanceof BackendHttpError)
      if (denied || noGateway) await this.ledger.markRejected(issued.send_id, 'gateway_rejected')
      else await this.ledger.markUnknown(issued.send_id, 'delivery_unknown')
      throw error
    }
    // Persist only the authoritative receipt; no client reportOutcome call.
    await this.ledger.markSent(issued.send_id, [receipt.message_id])
    return [receipt.message_id]
  }

  /** Alias kept explicit for callers reading as gate.send(...). */
  async send(input: SendTextInput): Promise<string[]> {
    return await this.sendText(input)
  }

  inferConversationKinds(conversationId: string): ConversationKind[] {
    return this.ledger.findConversationKinds(conversationId)
  }

  async snapshot() {
    return await this.ledger.snapshotNow()
  }

  private assertEventContext(context: CharacterEventContext): void {
    if (!context || typeof context !== 'object') {
      throw new CharacterRuntimeError('invalid_binding', 'trusted event context is required')
    }
    const expected = this.eventContext(context.senderId)
    if (
      context.accountId !== expected.accountId ||
      context.runtimeId !== expected.runtimeId ||
      context.adapterId !== expected.adapterId ||
      context.adapterInstanceId !== expected.adapterInstanceId ||
      typeof context.senderId !== 'string' ||
      context.senderId.length === 0
    ) {
      throw new CharacterRuntimeError(
        'cross_account_event',
        'event context does not match this account/runtime/adapter binding'
      )
    }
  }

  private async safeMarkRejected(sendId: string, code: string): Promise<void> {
    try {
      await this.ledger.markRejected(sendId, code)
    } catch (error) {
      this.logger?.error?.('cannot record rejected send', error)
    }
  }

  private requireOpenTrigger(kind: ConversationKind, conversationId: string): LedgerTriggerRecord {
    const trigger = this.ledger.findOpenTrigger(kind, conversationId)
    if (trigger) {
      if (Date.parse(trigger.deadline_at) <= this.clock()) {
        void this.ledger.closeTrigger(trigger.trigger_ref)
        throw new CharacterRuntimeError('trigger_expired', `trigger ${trigger.trigger_ref} expired`)
      }
      if (trigger.pending_send_id) {
        throw new CharacterRuntimeError(
          'delivery_unknown',
          `trigger ${trigger.trigger_ref} has an unresolved segment ${trigger.pending_send_id}`
        )
      }
      return trigger
    }

    const poisoned = this.ledger.findPoisonedTrigger(kind, conversationId)
    if (poisoned) {
      throw new CharacterRuntimeError(
        'trigger_poisoned',
        `trigger ${poisoned.trigger_ref} has an unknown delivery and will not send again`
      )
    }
    throw new CharacterRuntimeError(
      'no_trusted_trigger',
      `no open trusted trigger for ${kind}:${conversationId}`
    )
  }


}
