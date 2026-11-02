import { createServer } from 'node:http'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { randomUUID } from 'node:crypto'
import type { AddressInfo } from 'node:net'

/**
 * FIXTURE ONLY - in-process HTTP handler implementing the aligned chat-gate
 * contract for offline tests. It is not a deployed backend service and mounts
 * no production routes.
 */
export interface FakeBackendOptions {
  accountId: string
  qqSelfId: string
  managedSelfIds?: string[]
  bindingEpoch?: string
  chatEnabled?: boolean
  speakerActive?: boolean
  outcomeStatus?: number
  beforeGateway?: () => Promise<void>
}

export interface FakeBackendRequest {
  method: string
  path: string
  body: unknown
  authorization: string | null
  idempotencyKey: string | null
}

export interface FakeBackendState {
  baseUrl: string
  requests: FakeBackendRequest[]
  outcomes: Array<Record<string, unknown>>
  permits: Map<string, Record<string, unknown>>
  consumeCount: number
  writerCalls: Array<{ text: string }>
  waitForRequest(path: string, count?: number): Promise<void>
  waitForResponse(path: string, count?: number): Promise<void>
  waitForOutcome(count?: number): Promise<void>
  close(): Promise<void>
}

interface Envelope {
  data: unknown
  meta: Record<string, unknown>
}

function json(res: ServerResponse, status: number, body: unknown): void {
  const raw = JSON.stringify(body)
  res.writeHead(status, { 'content-type': 'application/json' })
  res.end(raw)
}

async function readBody(req: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = []
  for await (const chunk of req) chunks.push(Buffer.from(chunk))
  const raw = Buffer.concat(chunks).toString('utf8')
  return raw.length > 0 ? (JSON.parse(raw) as unknown) : null
}

function utc(ms: number): string {
  return new Date(ms).toISOString()
}

export async function startFakeBackend(options: FakeBackendOptions): Promise<FakeBackendState> {
  const requests: FakeBackendRequest[] = []
  const outcomes: Array<Record<string, unknown>> = []
  const permits = new Map<string, Record<string, unknown>>()
  const bookingEpoch = options.bindingEpoch ?? '1'
  let consumeCount = 0
  const writerCalls: Array<{ text: string }> = []
  const responses: string[] = []
  const responseWaiters: Array<{ path: string; count: number; resolve: () => void }> = []
  const requestWaiters: Array<{
    path: string
    count: number
    resolve: () => void
    reject: (error: Error) => void
  }> = []
  const outcomeWaiters: Array<{ count: number; resolve: () => void }> = []

  function notifyRequests(): void {
    for (let index = requestWaiters.length - 1; index >= 0; index -= 1) {
      const waiter = requestWaiters[index]!
      if (requests.filter((request) => request.path === waiter.path).length >= waiter.count) {
        requestWaiters.splice(index, 1)
        waiter.resolve()
      }
    }
  }

  function notifyResponse(path: string): void {
    responses.push(path)
    for (let index = responseWaiters.length - 1; index >= 0; index -= 1) {
      const waiter = responseWaiters[index]!
      if (responses.filter((entry) => entry === path).length >= waiter.count) {
        responseWaiters.splice(index, 1)
        waiter.resolve()
      }
    }
  }

  function notifyOutcomes(): void {
    for (let index = outcomeWaiters.length - 1; index >= 0; index -= 1) {
      const waiter = outcomeWaiters[index]!
      if (outcomes.length >= waiter.count) {
        outcomeWaiters.splice(index, 1)
        waiter.resolve()
      }
    }
  }

  const server = createServer(async (req, res) => {
    try {
      const method = req.method ?? 'GET'
      const url = new URL(req.url ?? '/', 'http://127.0.0.1')
      const body = method === 'POST' ? await readBody(req) : null
      requests.push({
        method,
        path: url.pathname,
        body,
        authorization: req.headers.authorization ?? null,
        idempotencyKey: (req.headers['idempotency-key'] as string | undefined) ?? null
      })
      notifyRequests()

      if (req.headers.authorization !== `Bearer test-runtime-token`) {
        json(res, 401, { error: { code: 'unauthorized', message: 'missing fixture bearer token' } })
        return
      }

      if (method === 'GET' && url.pathname.endsWith('/capabilities')) {
        json(res, 200, {data:{api_version:'character-gateway-v1', single_text: true}}); return
      }
      if (method === 'POST' && url.pathname.endsWith('/gateway-send')) {
        if (options.outcomeStatus === 404) { json(res,404,{error:{code:'NOT_FOUND'}}); return }
        const p = permits.get(decodeURIComponent(url.pathname.split('/')[4]!))
        const input = body as Record<string, unknown>
        if (!p || p.state !== 'issued' || p.segment_count !== 1) { json(res,409,{error:{code:'EXTERNAL_OUTCOME_UNKNOWN'}}); return }
        p.state = 'consumed'; consumeCount++
        await options.beforeGateway?.()
        writerCalls.push({text:String(input.text)})
        const outcome = {permit_id:p.permit_id,send_id:p.send_id,state:'sent',message_id:'gateway-msg-1'}
        outcomes.push(outcome); notifyOutcomes()
        json(res,200,{data:{...p,...outcome,api_version:'character-gateway-v1'}}); return
      }
      if (method === 'GET' && url.pathname === '/v1/internal/chat-context') {
        const kind = url.searchParams.get('conversation_kind')
        const conversationId = url.searchParams.get('conversation_id')
        const envelope: Envelope = {
          data: {
            account_id: options.accountId,
            qq_self_id: options.qqSelfId,
            conversation_kind: kind,
            conversation_id: conversationId,
            active_speaker_account_id:
              kind === 'group' && options.speakerActive !== false ? options.accountId : null,
            binding_epoch: bookingEpoch,
            lease_expires_at: utc(Date.now() + 60_000),
            speaker_active: kind === 'private' ? true : options.speakerActive !== false,
            chat_enabled: options.chatEnabled !== false,
            daily_limit: 100,
            issued_today: 0,
            managed_self_ids: options.managedSelfIds ?? []
          },
          meta: { request_id: randomUUID() }
        }
        json(res, 200, envelope)
        return
      }

      if (method === 'POST' && url.pathname === '/v1/internal/chat-triggers') {
        const input = body as Record<string, unknown>
        const envelope: Envelope = {
          data: {
            trigger_ref: input.trigger_ref,
            account_id: input.account_id,
            conversation_kind: input.conversation_kind,
            conversation_id: input.conversation_id,
            source_event_id: input.source_event_id,
            sender_id: input.sender_id,
            directed_account_id: input.directed_account_id,
            deadline_at: input.deadline_at,
            segment_count: input.segment_count,
            next_segment: 0,
            state: 'open',
            created_at: utc(Date.now())
          },
          meta: { request_id: randomUUID() }
        }
        json(res, 201, envelope)
        notifyResponse('/v1/internal/chat-triggers')
        return
      }

      if (method === 'POST' && url.pathname === '/v1/internal/chat-send-permits') {
        const input = body as Record<string, unknown>
        const permitId = `permit-${randomUUID()}`
        const permit = {
          permit_id: permitId,
          send_id: input.send_id,
          account_id: input.account_id,
          conversation_kind: input.conversation_kind,
          conversation_id: input.conversation_id,
          trigger_ref: input.trigger_ref,
          text_digest: input.text_digest,
          expected_binding_epoch: input.expected_binding_epoch,
          issued_at: utc(Date.now()),
          expires_at: utc(Date.now() + 60_000),
          segment_index: input.segment_index,
          segment_count: input.segment_count,
          state: 'issued'
        }
        permits.set(permitId, permit)
        json(res, 200, { data: permit, meta: { request_id: randomUUID() } })
        return
      }

      const consumeMatch = /^\/v1\/internal\/chat-send-permits\/([^/]+)\/consume$/.exec(url.pathname)
      if (method === 'POST' && consumeMatch) {
        const permit = permits.get(decodeURIComponent(consumeMatch[1]!))
        if (!permit || permit.state !== 'issued') {
          json(res, 409, { error: { code: 'permit_already_consumed', message: 'permit not issued' } })
          return
        }
        permit.state = 'consumed'
        consumeCount += 1
        json(res, 200, { data: permit, meta: { request_id: randomUUID() } })
        return
      }

      const outcomeMatch = /^\/v1\/internal\/chat-send-permits\/([^/]+)\/outcome$/.exec(url.pathname)
      if (method === 'POST' && outcomeMatch) {
        if (options.outcomeStatus && options.outcomeStatus !== 201) {
          json(res, options.outcomeStatus, {
            error: { code: 'outcome_unavailable', message: 'fixture forced outcome failure' }
          })
          return
        }
        const permitId = decodeURIComponent(outcomeMatch[1]!)
        const input = body as Record<string, unknown>
        if (input.state === 'sent' && input.message_id !== null && typeof input.message_id !== 'string') {
          json(res, 400, { error: { code: 'invalid_outcome', message: 'message_id must be string|null' } })
          return
        }
        if (input.state === 'unknown' && input.message_id !== null) {
          json(res, 400, { error: { code: 'invalid_outcome', message: 'unknown requires null message_id' } })
          return
        }
        outcomes.push({ permit_id: permitId, ...input })
        notifyOutcomes()
        json(res, 201, {
          data: {
            permit_id: permitId,
            send_id: input.send_id,
            state: input.state,
            message_id: input.message_id,
            occurred_at: input.occurred_at,
            created_at: utc(Date.now())
          },
          meta: { request_id: randomUUID() }
        })
        return
      }

    } catch (error) {
      json(res, 500, { error: { code: 'fixture_error', message: String(error) } })
      for (let index = requestWaiters.length - 1; index >= 0; index -= 1) {
        requestWaiters[index]!.reject(error instanceof Error ? error : new Error(String(error)))
        requestWaiters.splice(index, 1)
      }
    }
  })

  const listening = Promise.withResolvers<void>()
  server.once('error', listening.reject)
  server.listen(0, '127.0.0.1', () => listening.resolve())
  await listening.promise
  const address = server.address() as AddressInfo

  return {
    baseUrl: `http://127.0.0.1:${address.port}`,
    requests,
    outcomes,
    permits,
    writerCalls,
    get consumeCount() {
      return consumeCount
    },
    async waitForRequest(path: string, count = 1): Promise<void> {
      if (requests.filter((request) => request.path === path).length >= count) return
      const { promise, resolve, reject } = Promise.withResolvers<void>()
      requestWaiters.push({ path, count, resolve, reject })
      await promise
    },
    async waitForResponse(path: string, count = 1): Promise<void> {
      if (responses.filter((entry) => entry === path).length >= count) return
      const { promise, resolve } = Promise.withResolvers<void>()
      responseWaiters.push({ path, count, resolve })
      await promise
    },
    async waitForOutcome(count = 1): Promise<void> {
      if (outcomes.length >= count) return
      const { promise, resolve } = Promise.withResolvers<void>()
      outcomeWaiters.push({ count, resolve })
      await promise
    },
    async close() {
      const { promise, resolve, reject } = Promise.withResolvers<void>()
      server.close((error) => (error ? reject(error) : resolve()))
      await promise
    }
  }
}
