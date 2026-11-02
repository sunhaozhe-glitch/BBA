import type { Bot, Context, Session } from 'koishi'
import { CollectorBoundaryError } from './errors.js'
import type { CollectorLogger } from './types.js'

const OUTBOUND_GUARD = Symbol.for('bba.analysis-collector.outbound-denied')

export type BoundaryRecord = (code: string, operation: string, detail: string) => void

export interface CollectorIngressOptions {
  selfId: string
  platform: string
  record: BoundaryRecord
  logger?: CollectorLogger
  onDispatchBlocked?: () => void
}

const UPSTREAM_COMMAND_NAMES = [
  '群分析',
  'group-analysis',
  '用户画像',
  'group-analysis.persona'
]

interface DetectedCommand {
  name: string
  operation: string
  detail: string
}

function detectUpstreamCommand(content: string): DetectedCommand | null {
  const trimmed = content.trim()
  if (!trimmed) return null
  const prefixed = /^[/!]/.test(trimmed)
  const stripped = trimmed.replace(/^[/!\s]+/, '')
  const first = stripped.split(/\s+/, 1)[0] ?? ''
  if (!first) return null

  const known =
    prefixed ||
    UPSTREAM_COMMAND_NAMES.some(
      (name) => first === name || first.startsWith(`${name}.`)
    )
  if (!known) return null

  // Any prefixed content is a command dispatch. Known upstream analysis and
  // persona command names are also denied without a prefix because Koishi may
  // run without a command prefix.
  return {
    name: first,
    operation: `command:${first}`,
    detail: prefixed
      ? 'all command dispatch is denied in P1a; upstream group-analysis/persona commands have no disable switch'
      : 'upstream group-analysis/persona command is registered unconditionally and denied in P1a'
  }
}

/**
 * Install the single controllable dispatch boundary.
 *
 * Koishi emits `message` listeners (including group-analysis persistence)
 * independently of the middleware queue. Returning without `next` therefore
 * preserves collection while preventing command dispatch, ChatLuna middleware,
 * and any model path from executing.
 */
export function installCollectorIngress(
  ctx: Context,
  options: CollectorIngressOptions
): () => void {
  return ctx.$processor.middleware(
    async (session: Session, _next) => {
      if (session.selfId !== options.selfId) {
        options.record(
          'cross_self_id_event',
          'collect',
          `ignored event selfId=${String(session.selfId)}; collector is bound to selfId=${options.selfId}`
        )
        return
      }
      if (session.platform && session.platform !== options.platform) {
        options.record(
          'cross_platform_event',
          'collect',
          `ignored event platform=${session.platform}; collector is bound to platform=${options.platform}`
        )
        return
      }

      const command = detectUpstreamCommand(session.content ?? '')
      if (command) {
        options.record('upstream_command_denied', command.operation, command.detail)
        throw new CollectorBoundaryError(command.operation, command.detail)
      }

      options.onDispatchBlocked?.()
      // Deliberately no next(): ordinary command/chat/model middleware must not
      // run. The upstream message listener has already received this event.
      return
    },
    { prepend: true, global: true }
  )
}

function deniedMethod(
  bot: Bot,
  method: string,
  record: BoundaryRecord,
  logger?: CollectorLogger
): (...args: unknown[]) => never {
  return (..._args: unknown[]): never => {
    const detail = `outbound ${method} denied for selfId=${String(bot.selfId)}; P1a has no send path`
    record('outbound_denied', `send:${method}`, detail)
    logger?.warn?.(detail)
    throw new CollectorBoundaryError('send', detail)
  }
}

interface RestoreEntry {
  property: string
  descriptor: PropertyDescriptor | undefined
  fallback: unknown
}

function restore(bot: Bot, entry: RestoreEntry): void {
  if (entry.descriptor) {
    Object.defineProperty(bot, entry.property, entry.descriptor)
    return
  }
  if (entry.fallback === undefined) {
    try {
      const record = bot as unknown as Record<string, unknown>
      delete record[entry.property]
    } catch {
      // Non-configurable adapter member; leave it in place.
    }
    return
  }
  Object.defineProperty(bot, entry.property, {
    configurable: true,
    enumerable: false,
    writable: true,
    value: entry.fallback
  })
}

/**
 * Deny every outbound and adapter-internal path on a Bot, including bots added
 * after the collector has started. Restores descriptors exactly on dispose.
 */
export function installOutboundDenial(
  bot: Bot,
  record: BoundaryRecord,
  logger?: CollectorLogger
): () => void {
  const marked = bot as Bot & { [OUTBOUND_GUARD]?: true }
  if (marked[OUTBOUND_GUARD]) return () => {}

  const methods = ['sendMessage', 'sendPrivateMessage', 'createMessage'] as const
  const restores: RestoreEntry[] = []
  const botRecord = bot as unknown as Record<string, unknown>
  for (const method of methods) {
    const current = botRecord[method]
    restores.push({
      property: method,
      descriptor: Object.getOwnPropertyDescriptor(bot, method),
      fallback: typeof current === 'function' ? current : undefined
    })
    Object.defineProperty(bot, method, {
      configurable: true,
      enumerable: false,
      writable: true,
      value: deniedMethod(bot, method, record, logger)
    })
  }

  const internalView = bot as unknown as { internal?: unknown }
  let internalTarget = internalView.internal
  const internalDescriptor = Object.getOwnPropertyDescriptor(bot, 'internal')
  const internalProxy = new Proxy(
    {},
    {
      get(_target, property) {
        if (property === 'then') return undefined
        const detail = `direct adapter internal access denied (${String(property)}) for selfId=${String(bot.selfId)}`
        record('outbound_denied', `internal:${String(property)}`, detail)
        logger?.warn?.(detail)
        throw new CollectorBoundaryError('send', detail)
      },
      set(_target, property, value) {
        // Adapters may assign their internal object during connect; assignment
        // is allowed so the adapter fails later on an actual operation. Reads
        // and method calls remain denied.
        internalTarget = value
        void property
        return true
      }
    }
  )
  Object.defineProperty(bot, 'internal', {
    configurable: true,
    enumerable: true,
    get: () => internalProxy,
    set: (value: unknown) => {
      internalTarget = value
    }
  })
  restores.push({
    property: 'internal',
    descriptor: internalDescriptor,
    fallback: internalTarget
  })

  Object.defineProperty(marked, OUTBOUND_GUARD, {
    configurable: true,
    enumerable: false,
    value: true
  })

  return () => {
    for (const entry of restores.reverse()) restore(bot, entry)
    const marker = marked as { [OUTBOUND_GUARD]?: true }
    delete marker[OUTBOUND_GUARD]
  }
}

export function isOutboundDenied(bot: Bot): boolean {
  return Boolean((bot as Bot & { [OUTBOUND_GUARD]?: true })[OUTBOUND_GUARD])
}
