import { createHash } from 'node:crypto'
import { CharacterRuntimeError } from './errors.js'

export const MAX_TEXT_LENGTH = 4000
export const MAX_SEGMENT_COUNT = 32

/**
 * Lowercase hex SHA-256 of the exact UTF-8 bytes of one outbound text segment.
 * This is the digest required by ChatPermitRequest.text_digest.
 */
export function textDigest(text: string): string {
  return createHash('sha256').update(text, 'utf8').digest('hex')
}

/**
 * Stable trigger reference derived from the runtime's own event identity.
 * The backend treats the value as caller-stable and requires 1..128 chars.
 */
export function triggerRefFor(
  runtimeId: string,
  accountId: string,
  sourceEventId: string,
  kind: string,
  conversationId: string
): string {
  const digest = createHash('sha256')
    .update(`${runtimeId}\u0000${accountId}\u0000${kind}\u0000${conversationId}\u0000${sourceEventId}`, 'utf8')
    .digest('hex')
  return `trg_${digest.slice(0, 48)}`
}

export function sendIdFor(triggerRef: string, segmentIndex: number): string {
  const id = `${triggerRef}:${segmentIndex}`
  if (id.length > 128) {
    throw new CharacterRuntimeError(
      'invalid_config',
      `send id would exceed the 128 character backend limit: ${id.length}`
    )
  }
  return id
}

export function assertText(text: string): string {
  if (typeof text !== 'string') {
    throw new CharacterRuntimeError('invalid_text', 'outbound content is not text')
  }
  if (text.length < 1) {
    throw new CharacterRuntimeError('invalid_text', 'outbound text is empty')
  }
  if (text.length > MAX_TEXT_LENGTH) {
    throw new CharacterRuntimeError(
      'invalid_text',
      `outbound text is ${text.length} characters; the contract permits ${MAX_TEXT_LENGTH}`
    )
  }
  return text
}
