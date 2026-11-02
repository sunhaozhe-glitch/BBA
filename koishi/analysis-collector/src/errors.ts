import type { CollectorDiagnostics, CollectorErrorCode } from './types.js'

const MESSAGES: Record<CollectorErrorCode, string> = {
  invalid_options: 'analysis collector options are invalid',
  invalid_binding: 'analysis collector binding is incomplete or conflicted',
  state_dir_in_use: 'the collector state directory is already active',
  state_binding_conflict: 'the collector state directory is bound to a different self id or platform',
  state_lock_failed: 'the collector state directory lock could not be acquired',
  plugin_load_failed: 'a required collector plugin could not be loaded',
  app_start_failed: 'the collector App failed to start',
  startup_timeout: 'the collector App did not finish starting before the deadline',
  service_injection_timeout: 'required collector services were not injected before the deadline',
  collection_timeout: 'collected messages did not reach the requested count before the deadline',
  boundary_denied: 'the operation is denied at the P1a collector boundary',
  owner_lease_conflict: 'the requested group owner lease is held by another owner or epoch',
  summary_invalid: 'the Summary.v1 payload is invalid or lacks verifiable provenance',
  summary_reference_invalid: 'a Summary.v1 message reference is outside the collected account, group, or window',
  summary_publish_failed: 'the backend Summary store rejected or could not receive the validated Summary'
}
export interface CollectorErrorOptions {
  cause?: unknown
  diagnostics?: CollectorDiagnostics | null
  details?: Record<string, unknown>
}

/** Explicit, diagnosable failure. Never a silent wait. */
export class CollectorError extends Error {
  readonly code: CollectorErrorCode
  readonly diagnostics: CollectorDiagnostics | null
  readonly details: Record<string, unknown>

  constructor(code: CollectorErrorCode, message?: string, options: CollectorErrorOptions = {}) {
    super(message ?? MESSAGES[code], options.cause === undefined ? undefined : { cause: options.cause })
    this.name = 'CollectorError'
    this.code = code
    this.diagnostics = options.diagnostics ?? null
    this.details = options.details ?? {}
  }
}

/** Boundary denial raised from the collector's controllable App entry points. */
export class CollectorBoundaryError extends CollectorError {
  readonly operation: string

  constructor(operation: string, detail?: string, options: CollectorErrorOptions = {}) {
    super(
      'boundary_denied',
      detail ??
        `P1a is collection-only: ${operation} is denied at the collector boundary; the upstream package has no configuration switch for it`,
      options
    )
    this.name = 'CollectorBoundaryError'
    this.operation = operation
  }
}

export function isCollectorError(error: unknown): error is CollectorError {
  return error instanceof CollectorError
}

export function isCollectorBoundaryError(error: unknown): error is CollectorBoundaryError {
  return error instanceof CollectorBoundaryError
}
