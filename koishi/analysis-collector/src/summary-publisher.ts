import { CollectorError } from './errors.js'
import type { CollectedMessageWindow, StoredSummary } from './types.js'
import type { ProducedTopic, SummaryEvidenceKind } from './summary-producer.js'

export interface SummaryPublishInput {
  stored: StoredSummary
  window: CollectedMessageWindow
  topics: readonly ProducedTopic[]
  generatedAt: Date
}

export interface SummaryPublishResult {
  summaryId: string
  summaryVersion: number
  created: boolean
}

/**
 * Backend Summary write boundary. The backend owns version assignment,
 * cross-account checks, and expiry enforcement; this publisher only forwards a
 * Summary that already passed the collector's own `saveSummary` validation.
 */
export interface SummaryPublisher {
  publish(input: SummaryPublishInput): Promise<SummaryPublishResult>
}

export interface SummaryBackendPublisherOptions {
  baseUrl: string
  runtimeToken: string
  /** Backend account id; the backend matches it to the Summary's collector account. */
  accountId: string
  path?: string
  requestTimeoutMs?: number
  fetchImpl?: typeof fetch
}

export const DEFAULT_SUMMARY_PUBLISH_PATH = '/v1/internal/group-summaries'

export class SummaryBackendPublisher implements SummaryPublisher {
  private readonly baseUrl: string
  private readonly path: string
  private readonly timeoutMs: number
  private readonly fetchImpl: typeof fetch

  constructor(private readonly options: SummaryBackendPublisherOptions) {
    if (!options || typeof options !== 'object') {
      throw new CollectorError('invalid_options', 'a summary publisher requires options')
    }
    this.baseUrl = String(options.baseUrl ?? '').replace(/\/+$/, '')
    if (!this.baseUrl) throw new CollectorError('invalid_options', 'summary publisher baseUrl is empty')
    if (!options.runtimeToken) throw new CollectorError('invalid_options', 'summary publisher runtimeToken is empty')
    if (!options.accountId) throw new CollectorError('invalid_options', 'summary publisher accountId is empty')
    this.path = options.path ?? DEFAULT_SUMMARY_PUBLISH_PATH
    this.timeoutMs = options.requestTimeoutMs ?? 5_000
    this.fetchImpl = options.fetchImpl ?? fetch
  }

  async publish(input: SummaryPublishInput): Promise<SummaryPublishResult> {
    if (input.topics.length !== input.stored.summary.topics.length) {
      throw new CollectorError('summary_invalid', 'topic evidence does not align with the stored Summary')
    }
    const body = {
      account_id: this.options.accountId,
      summary_id: input.stored.id,
      window_id: input.stored.windowId,
      window_message_count: input.window.statistics.message_count,
      generated_at: input.generatedAt.toISOString(),
      summary: input.stored.summary,
      topic_evidence: input.topics.map((topic): SummaryEvidenceKind => topic.evidence_kind)
    }

    let response: Response
    try {
      response = await this.fetchImpl(`${this.baseUrl}${this.path}`, {
        method: 'POST',
        headers: {
          authorization: `Bearer ${this.options.runtimeToken}`,
          'content-type': 'application/json',
          accept: 'application/json'
        },
        body: JSON.stringify(body),
        signal: AbortSignal.timeout(this.timeoutMs)
      })
    } catch (error) {
      throw new CollectorError('summary_publish_failed', 'backend Summary ingest request failed', {
        cause: error
      })
    }
    const raw = await response.text()
    let parsed: unknown = null
    if (raw.length > 0) {
      try {
        parsed = JSON.parse(raw)
      } catch (error) {
        throw new CollectorError('summary_publish_failed', 'backend Summary ingest returned non-JSON', {
          cause: error
        })
      }
    }
    if (!response.ok) {
      const envelope = parsed as { error?: { code?: unknown; message?: unknown } } | null
      throw new CollectorError('summary_publish_failed', 'backend rejected the Summary ingest', {
        details: {
          status: response.status,
          code: typeof envelope?.error?.code === 'string' ? envelope.error.code : null
        }
      })
    }
    const data = (parsed as { data?: unknown } | null)?.data as
      | { summary_id?: unknown; summary_version?: unknown; window_id?: unknown; created?: unknown }
      | undefined
    if (
      !data ||
      data.summary_id !== input.stored.id ||
      data.window_id !== input.stored.windowId ||
      !Number.isInteger(data.summary_version) ||
      (data.summary_version as number) < 1
    ) {
      throw new CollectorError('summary_publish_failed', 'backend Summary ingest echo does not match the request')
    }
    return {
      summaryId: data.summary_id as string,
      summaryVersion: data.summary_version as number,
      created: data.created === true
    }
  }
}
