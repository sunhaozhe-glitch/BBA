import type { ChatBackendClient } from './backend.js'
import { CharacterRuntimeError } from './errors.js'
import { MAX_GROUP_CONTEXT_FUTURE_SKEW_MS } from './group-context.js'
import type { CharacterRuntimeLogger, GroupContext, GroupContextProvider } from './types.js'

export interface BackendGroupContextProviderOptions {
  client: Pick<ChatBackendClient, 'getGroupContext'>
  now?: () => number
  /** Maximum accepted generated_at future offset. */
  maxGeneratedSkewMs?: number
  logger?: CharacterRuntimeLogger
}

/**
 * Actual GroupContextProvider: reads the backend Summary projection for the
 * requesting account/group. It never caches, so a previous group's or a
 * previous round's Summary cannot leak into a later prompt. Expired or
 * future-dated context is dropped (prompt omits the block); a mismatched
 * account/group is a hard failure. This provider is data only: the permit
 * gate remains the sole send authority.
 */
export function createBackendGroupContextProvider(
  options: BackendGroupContextProviderOptions
): GroupContextProvider {
  if (!options || !options.client || typeof options.client.getGroupContext !== 'function') {
    throw new CharacterRuntimeError('invalid_config', 'a backend GroupContext provider requires a chat backend client')
  }
  const now = options.now ?? Date.now
  const maxSkew = options.maxGeneratedSkewMs ?? MAX_GROUP_CONTEXT_FUTURE_SKEW_MS
  if (!Number.isInteger(maxSkew) || maxSkew < 0 || maxSkew > 300_000) {
    throw new CharacterRuntimeError('invalid_config', 'group context generated-at skew must be 0..300000 ms')
  }
  const logger = options.logger
  return async ({ account, groupId }): Promise<GroupContext | null> => {
    const context = await options.client.getGroupContext({ accountId: account.accountId, groupId })
    if (context.account_id !== account.accountId || String(context.group_id) !== String(groupId)) {
      throw new CharacterRuntimeError(
        'cross_account_event',
        'backend group context does not match the requesting account/group'
      )
    }
    const at = now()
    if (Date.parse(context.generated_at) > at + maxSkew) {
      logger?.warn?.('backend group context is future dated; omitting it from the prompt')
      return null
    }
    if (Date.parse(context.expires_at) <= at) {
      logger?.warn?.('backend group context is expired; omitting it from the prompt')
      return null
    }
    return context
  }
}
