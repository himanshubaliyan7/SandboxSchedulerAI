/**
 * Lifecycle webhooks: managing them through the API ({@link Webhooks}) and
 * checking what an endpoint receives ({@link verifyWebhook}).
 *
 * Requests are signed as the Standard Webhooks specification describes
 * (https://www.standardwebhooks.com), so that project's libraries verify
 * them too.
 */

import { NotFoundError, SandboxError } from './errors.js'
import { ApiClient, type CallOptions, type ConnectionOptions } from './http.js'
import { decodeBase64, parseDate, utf8 } from './util.js'

/** An event type a webhook can subscribe to. `'*'` subscribes to all, including types added later. */
export type WebhookEventType =
  | 'sandbox.created'
  | 'sandbox.running'
  | 'sandbox.terminated'
  | 'sandbox.destroyed'
  | 'command.started'
  | 'command.finished'
  | '*'
  | (string & {})

/** The event types a webhook can subscribe to. */
export const WEBHOOK_EVENT_TYPES = [
  'sandbox.created',
  'sandbox.running',
  'sandbox.terminated',
  'sandbox.destroyed',
  'command.started',
  'command.finished',
] as const

/** A webhook's configuration and delivery state. */
export interface WebhookInfo {
  webhookId: string
  url: string
  eventTypes: WebhookEventType[]
  description: string
  enabled: boolean
  /** Why it is disabled: `user`, `failing` (by the server, after 72 hours of failures) or `endpoint_gone` (the endpoint answered 410). */
  disabledReason?: string
  disabledAt?: Date
  createdAt?: Date
  updatedAt?: Date
  /** Set while a rotated-out secret still signs. */
  previousSecretExpiresAt?: Date
  /** Failed attempts on the event that is currently blocking the webhook. */
  consecutiveFailures: number
  failingSince?: Date
  lastSuccessAt?: Date
  lastFailureAt?: Date
  lastError?: string
  nextAttemptAt?: Date
  /** Events that were given up on. */
  droppedEvents: number
}

/** What `create` and `rotateSecret` return: the webhook and, this once, its signing secret. */
export interface WebhookWithSecret extends WebhookInfo {
  /** `whsec_...`. Store it where the receiver can read it; the server will not show it again. */
  secret: string
}

export interface WebhookCreateOptions extends CallOptions {
  /** The events to receive. Default: all of them. */
  eventTypes?: readonly WebhookEventType[]
  description?: string
  /** `false` creates the webhook switched off. */
  enabled?: boolean
}

export interface WebhookUpdateOptions extends CallOptions {
  url?: string
  /** An empty array subscribes to every event. */
  eventTypes?: readonly WebhookEventType[]
  description?: string
  /** `true` on a disabled webhook starts it again at the present: events from the disabled period are not delivered. */
  enabled?: boolean
}

export interface WebhookRotateOptions extends CallOptions {
  /**
   * How long the previous secret keeps signing next to the new one.
   * Default 24 hours; 0 ends it at once; at most 7 days.
   */
  keepPreviousMs?: number
}

/** What the endpoint did with a `webhook.test` event. */
export interface WebhookTestResult {
  eventId: string
  /** The endpoint answered 2xx. */
  delivered: boolean
  /** The endpoint's HTTP status, if a response arrived. */
  status?: number
  durationMs: number
  errorCode?: string
  error?: string
}

/** One row of a webhook's delivery log. */
export interface WebhookDelivery {
  id: number
  /** The event's id, as sent in `Webhook-Id`. */
  eventId?: string
  eventType?: string
  attempt: number
  /** `failed` attempts are retried; `dropped` means the event (or `droppedEvents` events) will not be sent. */
  status: 'succeeded' | 'failed' | 'dropped' | (string & {})
  responseStatus?: number
  durationMs: number
  errorCode?: string
  error?: string
  droppedEvents?: number
  createdAt?: Date
}

/** A verified webhook payload. Ignore types and `data` members you do not know. */
export interface WebhookEvent {
  /** The same on every attempt and in the `Webhook-Id` header: deduplicate on it. */
  id: string
  type: WebhookEventType
  version: number
  /** When the event happened (not when it was sent). */
  createdAt?: Date
  sandboxId?: string
  templateId?: string
  /**
   * `state`, `reason` and `metadata` for sandbox events; the execution
   * record (`pid`, `cmd`, `args`, `exit_code`, `duration_ms`, ...) for
   * command events. Keys are as on the wire.
   */
  data: Record<string, unknown>
}

type Wire = Record<string, unknown>

function obj(v: unknown): Wire {
  return typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Wire) : {}
}
const str = (v: unknown): string => (typeof v === 'string' ? v : '')
const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0)

/** Assigns only what is present, so optional members stay absent rather than `undefined`. */
function put<T, K extends keyof T>(target: T, key: K, value: T[K] | undefined | ''): void {
  if (value !== undefined && value !== '') target[key] = value as T[K]
}

function webhookFromWire(v: unknown): WebhookInfo {
  const w = obj(v)
  const out: WebhookInfo = {
    webhookId: str(w.id),
    url: str(w.url),
    eventTypes: Array.isArray(w.event_types) ? w.event_types.filter((t): t is string => typeof t === 'string') : [],
    description: str(w.description),
    enabled: w.enabled !== false,
    consecutiveFailures: num(w.consecutive_failures),
    droppedEvents: num(w.dropped_events),
  }
  put(out, 'disabledReason', str(w.disabled_reason))
  put(out, 'disabledAt', parseDate(w.disabled_at))
  put(out, 'createdAt', parseDate(w.created_at))
  put(out, 'updatedAt', parseDate(w.updated_at))
  put(out, 'previousSecretExpiresAt', parseDate(w.previous_secret_expires_at))
  put(out, 'failingSince', parseDate(w.failing_since))
  put(out, 'lastSuccessAt', parseDate(w.last_success_at))
  put(out, 'lastFailureAt', parseDate(w.last_failure_at))
  put(out, 'lastError', str(w.last_error))
  put(out, 'nextAttemptAt', parseDate(w.next_attempt_at))
  return out
}

function withSecret(v: unknown, what: string): WebhookWithSecret {
  const secret = str(obj(v).secret)
  if (secret === '') throw new SandboxError(`${what}: the server answered without a secret`, { code: 'protocol_error' })
  const out = webhookFromWire(v) as WebhookWithSecret
  // Not enumerable: logging or serialising the object does not print it.
  Object.defineProperty(out, 'secret', { value: secret, enumerable: false, writable: false, configurable: false })
  return out
}

function deliveryFromWire(v: unknown): WebhookDelivery {
  const d = obj(v)
  const out: WebhookDelivery = { id: num(d.id), attempt: num(d.attempt) || 1, status: str(d.status), durationMs: num(d.duration_ms) }
  put(out, 'eventId', str(d.event_id))
  put(out, 'eventType', str(d.event_type))
  if (num(d.response_status) > 0) out.responseStatus = num(d.response_status)
  put(out, 'errorCode', str(d.error_code))
  put(out, 'error', str(d.error))
  if (num(d.dropped_events) > 0) out.droppedEvents = num(d.dropped_events)
  put(out, 'createdAt', parseDate(d.created_at))
  return out
}

function eventFromWire(v: unknown): WebhookEvent {
  const e = obj(v)
  const out: WebhookEvent = { id: str(e.id), type: str(e.type), version: num(e.version) || 1, data: obj(e.data) }
  put(out, 'createdAt', parseDate(e.created_at))
  put(out, 'sandboxId', str(e.sandbox_id))
  put(out, 'templateId', str(e.template_id))
  return out
}

function hookPath(id?: string, action?: string): string {
  if (id === undefined) return '/v1/webhooks'
  const p = `/v1/webhooks/${encodeURIComponent(id)}`
  return action ? `${p}/${action}` : p
}

/**
 * The webhooks of a tenant.
 *
 * Webhooks are control plane: every call needs the tenant API key
 * (`apiKey` or `SBX_API_KEY`); a sandbox access token is not accepted.
 *
 * ```ts
 * const webhooks = new Webhooks()
 * const hook = await webhooks.create('https://hooks.example.com/warmbox', { eventTypes: ['sandbox.terminated'] })
 * saveSomewhereSafe(hook.secret) // shown this once
 * ```
 */
export class Webhooks {
  readonly #api: ApiClient

  constructor(opts: ConnectionOptions = {}) {
    this.#api = new ApiClient(opts)
  }

  /**
   * Creates a webhook. The result carries `secret`, this once.
   *
   * The URL must be `https` with a host that resolves to a public address,
   * unless the operator has relaxed that. Not retried after a network
   * failure: the request may have been carried out, and a second one
   * would create a second webhook with another secret.
   */
  async create(url: string, opts: WebhookCreateOptions = {}): Promise<WebhookWithSecret> {
    const body: Wire = { url }
    if (opts.eventTypes && opts.eventTypes.length > 0) body.event_types = [...opts.eventTypes]
    if (opts.description) body.description = opts.description
    if (opts.enabled === false) body.enabled = false
    const res = await this.#api.json<unknown>('POST', hookPath(), {
      auth: 'control',
      retry: 'create',
      json: body,
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return withSecret(res, 'create')
  }

  /** The tenant's webhooks, oldest first. Secrets are never listed. */
  async list(opts: CallOptions = {}): Promise<WebhookInfo[]> {
    const res = await this.#api.json<unknown>('GET', hookPath(), {
      auth: 'control',
      retry: 'idempotent',
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    const items = obj(res).webhooks
    return Array.isArray(items) ? items.map(webhookFromWire) : []
  }

  /** One webhook: its configuration and delivery state. */
  async get(webhookId: string, opts: CallOptions = {}): Promise<WebhookInfo> {
    const res = await this.#api.json<unknown>('GET', hookPath(webhookId), {
      auth: 'control',
      retry: 'idempotent',
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return webhookFromWire(res)
  }

  /** Changes a webhook; members left out keep their value. */
  async update(webhookId: string, opts: WebhookUpdateOptions): Promise<WebhookInfo> {
    const body: Wire = {}
    if (opts.url !== undefined) body.url = opts.url
    if (opts.eventTypes !== undefined) body.event_types = [...opts.eventTypes]
    if (opts.description !== undefined) body.description = opts.description
    if (opts.enabled !== undefined) body.enabled = opts.enabled
    if (Object.keys(body).length === 0) {
      throw new SandboxError('nothing to change: give url, eventTypes, description or enabled', { code: 'invalid_argument' })
    }
    const res = await this.#api.json<unknown>('PATCH', hookPath(webhookId), {
      auth: 'control',
      retry: 'idempotent',
      json: body,
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return webhookFromWire(res)
  }

  /** Deletes a webhook and its delivery log. False if there is no such webhook. */
  async delete(webhookId: string, opts: CallOptions = {}): Promise<boolean> {
    try {
      await this.#api.empty('DELETE', hookPath(webhookId), {
        auth: 'control',
        retry: 'idempotent',
        signal: opts.signal,
        timeoutMs: opts.requestTimeoutMs,
      })
    } catch (e) {
      if (e instanceof NotFoundError) return false
      throw e
    }
    return true
  }

  /**
   * Replaces the signing secret; the result carries the new one, this
   * once. For `keepPreviousMs` requests are signed with both secrets, so
   * the receiver can be switched over without rejecting anything.
   */
  async rotateSecret(webhookId: string, opts: WebhookRotateOptions = {}): Promise<WebhookWithSecret> {
    if (opts.keepPreviousMs !== undefined && !(opts.keepPreviousMs >= 0)) {
      throw new SandboxError('keepPreviousMs must not be negative', { code: 'invalid_argument' })
    }
    const res = await this.#api.json<unknown>('POST', hookPath(webhookId, 'rotate-secret'), {
      auth: 'control',
      retry: 'rate-limit',
      json: opts.keepPreviousMs === undefined ? undefined : { keep_previous_seconds: Math.floor(opts.keepPreviousMs / 1000) },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return withSecret(res, 'rotateSecret')
  }

  /**
   * Sends a signed `webhook.test` event now and reports the endpoint's
   * answer. A failing endpoint is a result (`delivered: false`), not an
   * exception.
   */
  async test(webhookId: string, opts: CallOptions = {}): Promise<WebhookTestResult> {
    const res = obj(
      await this.#api.json<unknown>('POST', hookPath(webhookId, 'test'), {
        auth: 'control',
        retry: 'rate-limit',
        signal: opts.signal,
        timeoutMs: opts.requestTimeoutMs,
      }),
    )
    const out: WebhookTestResult = { eventId: str(res.event_id), delivered: res.delivered === true, durationMs: num(res.duration_ms) }
    if (num(res.status) > 0) out.status = num(res.status)
    put(out, 'errorCode', str(res.error_code))
    put(out, 'error', str(res.error))
    return out
  }

  /** Recent delivery attempts, newest first (50 unless `limit` says otherwise; at most 500). */
  async deliveries(webhookId: string, opts: CallOptions & { limit?: number } = {}): Promise<WebhookDelivery[]> {
    const res = await this.#api.json<unknown>('GET', hookPath(webhookId, 'deliveries'), {
      auth: 'control',
      retry: 'idempotent',
      query: { limit: opts.limit },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    const items = obj(res).deliveries
    return Array.isArray(items) ? items.map(deliveryFromWire) : []
  }
}

/** A received webhook is not authentic, or too old to trust. Answer 400 and do not act on the payload. */
export class WebhookVerificationError extends SandboxError {
  constructor(message: string) {
    super(message, { code: 'webhook_verification_failed' })
  }
}

/** Anything header-like: a `Headers` object, Node's `IncomingHttpHeaders`, or a plain record. */
export type WebhookHeaders =
  | { get(name: string): string | null | undefined }
  | Record<string, string | readonly string[] | undefined>
  | Iterable<readonly [string, string]>

export interface VerifyWebhookOptions {
  /**
   * How far the `Webhook-Timestamp` may be from the local clock, in either
   * direction, before the request is refused as a possible replay.
   * Default 300 seconds.
   */
  toleranceSeconds?: number
  /** The current time, for tests. */
  now?: Date | number
}

const SECRET_PREFIX = 'whsec_'

function header(headers: WebhookHeaders, name: string): string {
  const getter = (headers as { get?: unknown }).get
  if (typeof getter === 'function') {
    // `Headers.get` ignores case; a Map's does not, so a miss falls through to the scan below.
    const v = (getter as (n: string) => unknown).call(headers, name)
    if (typeof v === 'string') return v.trim()
  }
  const entries: Iterable<readonly [string, unknown]> =
    typeof (headers as Iterable<unknown>)[Symbol.iterator] === 'function'
      ? (headers as Iterable<readonly [string, unknown]>)
      : Object.entries(headers as Record<string, unknown>)
  for (const [k, v] of entries) {
    if (typeof k !== 'string' || k.toLowerCase() !== name) continue
    const first = Array.isArray(v) ? v[0] : v
    if (typeof first === 'string') return first.trim()
  }
  return ''
}

function decodeKey(b64: string): Uint8Array | undefined {
  if (b64 === '' || !/^[A-Za-z0-9+/]+={0,2}$/.test(b64) || b64.length % 4 !== 0) return undefined
  try {
    return decodeBase64(b64)
  } catch {
    return undefined
  }
}

/**
 * Checks the signature of a received webhook and returns its event.
 *
 * It needs no client and no network, only WebCrypto (`crypto.subtle`:
 * Node 20+, Deno, Bun, Cloudflare Workers, browsers).
 *
 * @param payload The raw request body, exactly as it arrived (a string or
 *   bytes). Do not pass a parsed object, and do not re-serialise: the
 *   signature covers the bytes.
 * @param headers The request headers; `Webhook-Id`, `Webhook-Timestamp`
 *   and `Webhook-Signature` are read, in any letter case.
 * @param secret The webhook's signing secret (`whsec_...`).
 * @throws WebhookVerificationError when a header is missing, the
 *   timestamp is outside the tolerance, no signature matches, or the
 *   body is not a JSON object.
 *
 * Signatures are compared by `crypto.subtle.verify`, which takes the same
 * time whether or not they match. During a secret rotation the header
 * carries two signatures; one match is enough.
 */
export async function verifyWebhook(
  payload: string | Uint8Array | ArrayBuffer,
  headers: WebhookHeaders,
  secret: string,
  opts: VerifyWebhookOptions = {},
): Promise<WebhookEvent> {
  if (typeof payload !== 'string' && !(payload instanceof Uint8Array) && !(payload instanceof ArrayBuffer)) {
    throw new WebhookVerificationError('the payload must be the raw request body (a string or bytes), not a parsed object')
  }
  const body = typeof payload === 'string' ? utf8(payload) : payload instanceof Uint8Array ? payload : new Uint8Array(payload)
  const key = typeof secret === 'string' && secret.startsWith(SECRET_PREFIX) ? decodeKey(secret.slice(SECRET_PREFIX.length)) : undefined
  if (key === undefined || key.length === 0) throw new WebhookVerificationError('the secret is not a whsec_ signing secret')

  const id = header(headers, 'webhook-id')
  const timestamp = header(headers, 'webhook-timestamp')
  const signatures = header(headers, 'webhook-signature')
  if (id === '' || timestamp === '' || signatures === '') {
    throw new WebhookVerificationError('a signature header is missing (Webhook-Id, Webhook-Timestamp, Webhook-Signature)')
  }
  if (!/^\d{1,15}$/.test(timestamp)) throw new WebhookVerificationError('the Webhook-Timestamp header is not a number')
  const sent = Number(timestamp)
  const now = (opts.now === undefined ? Date.now() : opts.now instanceof Date ? opts.now.getTime() : opts.now) / 1000
  const tolerance = opts.toleranceSeconds ?? 300
  if (now - sent > tolerance) throw new WebhookVerificationError("the webhook's timestamp is too old")
  if (sent - now > tolerance) throw new WebhookVerificationError("the webhook's timestamp is in the future")

  const subtle = globalThis.crypto?.subtle
  if (subtle === undefined) throw new SandboxError('no WebCrypto (crypto.subtle) in this runtime', { code: 'no_webcrypto' })
  const prefix = utf8(`${id}.${timestamp}.`)
  const signed = new Uint8Array(prefix.length + body.length)
  signed.set(prefix, 0)
  signed.set(body, prefix.length)
  const cryptoKey = await subtle.importKey('raw', key as BufferSource, { name: 'HMAC', hash: 'SHA-256' }, false, ['verify'])
  let matched = false
  for (const candidate of signatures.split(/\s+/)) {
    const comma = candidate.indexOf(',')
    if (comma < 0 || candidate.slice(0, comma) !== 'v1') continue
    const got = decodeKey(candidate.slice(comma + 1))
    if (got === undefined) continue
    // No early exit: every candidate is checked, each in constant time.
    const ok = await subtle.verify('HMAC', cryptoKey, got as BufferSource, signed as BufferSource)
    matched = ok || matched
  }
  if (!matched) throw new WebhookVerificationError('no signature matches: wrong secret, or the body was altered')

  let parsed: unknown
  try {
    parsed = JSON.parse(new TextDecoder().decode(body))
  } catch {
    throw new WebhookVerificationError('the signed body is not JSON')
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    throw new WebhookVerificationError('the signed body is not a JSON object')
  }
  return eventFromWire(parsed)
}
