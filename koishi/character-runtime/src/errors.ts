export type CharacterRuntimeErrorCode =
  | 'invalid_config'
  | 'invalid_binding'
  | 'duplicate_scope'
  | 'managed_bot_event'
  | 'cross_account_event'
  | 'conversation_not_authorized'
  | 'no_trusted_trigger'
  | 'trigger_suppressed'
  | 'trigger_expired'
  | 'trigger_poisoned'
  | 'segment_budget_exhausted'
  | 'speaker_not_active'
  | 'rich_content_rejected'
  | 'unguarded_internal_send'
  | 'invalid_text'
  | 'permit_rejected'
  | 'permit_mismatch'
  | 'permit_expired'
  | 'permit_already_used'
  | 'backend_unavailable'
  | 'backend_rejected'
  | 'delivery_unknown'
  | 'ledger_corrupt'

const MESSAGES: Partial<Record<CharacterRuntimeErrorCode, string>> = {
  invalid_config: 'character runtime configuration is invalid',
  invalid_binding: 'account binding is incomplete or malformed',
  duplicate_scope: 'a character isolation scope is assigned more than once',
  managed_bot_event: 'event originates from a managed bot and is ignored',
  cross_account_event: 'event does not belong to this account runtime',
  conversation_not_authorized: 'conversation has no trusted trigger for this account',
  no_trusted_trigger: 'no open trusted trigger for this conversation',
  trigger_suppressed: 'trusted trigger chose a different active speaker',
  trigger_expired: 'trusted trigger deadline has passed',
  trigger_poisoned: 'trigger has an unknown delivery and cannot send again',
  segment_budget_exhausted: 'trigger segment budget is exhausted',
  speaker_not_active: 'this account is not the active speaker',
  rich_content_rejected: 'only plain text may pass the character send gate',
  unguarded_internal_send: 'direct adapter internal sends are not permitted',
  invalid_text: 'outbound text is empty or exceeds the permitted size',
  permit_rejected: 'backend refused the chat send permit',
  permit_mismatch: 'backend permit does not match the requested text or scope',
  permit_expired: 'chat send permit expired before delivery',
  permit_already_used: 'send id already reached the adapter and cannot be reused',
  backend_unavailable: 'chat gate backend is unavailable',
  backend_rejected: 'chat gate backend rejected the request',
  delivery_unknown: 'adapter outcome is unknown; the message is not resent',
  ledger_corrupt: 'durable send ledger is unreadable'
}

export class CharacterRuntimeError extends Error {
  readonly code: CharacterRuntimeErrorCode
  readonly retryable: boolean

  constructor(
    code: CharacterRuntimeErrorCode,
    message?: string,
    options?: { cause?: unknown; retryable?: boolean }
  ) {
    super(message ?? MESSAGES[code] ?? code, options?.cause ? { cause: options.cause } : undefined)
    this.name = 'CharacterRuntimeError'
    this.code = code
    this.retryable = options?.retryable ?? false
  }
}

export class BackendHttpError extends CharacterRuntimeError {
  readonly status: number
  readonly backendCode: string | null

  constructor(
    status: number,
    backendCode: string | null,
    message: string,
    code: CharacterRuntimeErrorCode = status >= 500 ? 'backend_unavailable' : 'backend_rejected',
    options?: { cause?: unknown; retryable?: boolean }
  ) {
    super(code, message, options)
    this.name = 'BackendHttpError'
    this.status = status
    this.backendCode = backendCode
  }
}

export function isCharacterRuntimeError(error: unknown): error is CharacterRuntimeError {
  return error instanceof CharacterRuntimeError
}
