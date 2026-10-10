/**
 * The HTTP transport: credentials, timeouts, abort, error mapping and
 * retries. It uses only the platform `fetch` and never logs anything.
 */

import { ConnectionError, type ErrorBody, errorFromBody, parseRetryAfter, SandboxError, TimeoutError } from './errors.js'
import { abortReason, backoffDelay, env, isBrowser, linkedAbort, sleep, throwIfAborted } from './util.js'
import { VERSION } from './version.js'

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>

/** How automatic retries behave. */
export interface RetryOptions {
  /** Retries after the first attempt. Default 3; 0 disables. */
  maxRetries?: number
  /** First backoff delay when the server sent no `Retry-After`. Default 250 ms. */
  initialDelayMs?: number
  /** Upper bound of one backoff delay. Default 8000 ms. */
  maxDelayMs?: number
  /**
   * The longest `Retry-After` the SDK will sit out. If the server asks for
   * more, the error is thrown at once. Default 30000 ms.
   */
  maxRetryAfterMs?: number
  /**
   * Also retry `429 quota_exceeded` on create. Off by default: the quota is
   * only freed when one of the tenant's own sandboxes ends.
   */
  retryQuotaExceeded?: boolean
}

/** Options shared by every entry point that talks to the server. */
export interface ConnectionOptions {
  /** Tenant API key (`sbx_...`). Default: `SBX_API_KEY` where `process.env` exists. Never ship it to a browser. */
  apiKey?: string
  /** Server URL. Default: `SBX_BASE_URL` where `process.env` exists, else `http://127.0.0.1:8080`. */
  baseUrl?: string
  /** `fetch` implementation. Default: the global one. */
  fetch?: FetchLike
  /** Deadline of one non-streaming request attempt. Default 30000 ms; 0 disables. */
  requestTimeoutMs?: number
  /** Retry policy, or `false` to switch retries off. */
  retry?: RetryOptions | false
  /** Extra headers sent with every request (for a proxy in front of the server, for example). */
  headers?: Record<string, string>
  /**
   * WebSocket constructor for PTY sessions. Default: the global `WebSocket`
   * (browsers, Node 22+, Deno, Bun, workerd). On Node 20 pass the `ws`
   * package's `WebSocket`.
   */
  WebSocket?: WebSocketConstructor
}

/** The part of the WebSocket API the SDK uses; the global `WebSocket` and the `ws` package both satisfy it. */
export interface WebSocketLike {
  binaryType: string
  readonly readyState: number
  send(data: Uint8Array | ArrayBuffer): void
  close(code?: number, reason?: string): void
  addEventListener(type: string, listener: (event: never) => void): void
  removeEventListener(type: string, listener: (event: never) => void): void
}

/**
 * A WebSocket constructor. The third argument is only ever passed when the
 * session must authenticate with an API key (no access token), which needs
 * a request header: the `ws` package takes `{ headers }` there.
 */
export type WebSocketConstructor = new (url: string, protocols?: string | string[], options?: never) => WebSocketLike

export const DEFAULT_BASE_URL = 'http://127.0.0.1:8080'
export const DEFAULT_REQUEST_TIMEOUT_MS = 30_000

const DEFAULT_RETRY: Required<RetryOptions> = {
  maxRetries: 3,
  initialDelayMs: 250,
  maxDelayMs: 8_000,
  maxRetryAfterMs: 30_000,
  retryQuotaExceeded: false,
}

/**
 * Which failures may be retried for a request:
 *
 * - `idempotent`: 429 `rate_limited`, 502/503, and network failures.
 * - `create`: 429 `rate_limited` and 503 (`pool_empty`, `shutting_down`);
 *   the server answers these before it claims a sandbox. A network failure
 *   is not retried, because the claim may have happened.
 * - `rate-limit`: only 429 `rate_limited`, which the server answers before
 *   it does anything (used for non-idempotent data-plane calls).
 * - `none`: never.
 */
export type RetryPolicy = 'idempotent' | 'create' | 'rate-limit' | 'none'

export type AuthMode = 'control' | 'data'

export interface RequestOptions {
  query?: Record<string, string | number | boolean | readonly string[] | undefined>
  /** JSON request body. */
  json?: unknown
  /** Raw request body. A ReadableStream cannot be retried. */
  body?: string | Uint8Array | Blob | ReadableStream<Uint8Array>
  headers?: Record<string, string>
  signal?: AbortSignal | undefined
  /** Overrides `requestTimeoutMs` for this request. */
  timeoutMs?: number | undefined
  /** `control`: API key required. `data`: access token, or API key. */
  auth: AuthMode
  retry: RetryPolicy
  /**
   * Return as soon as the response headers are in and leave the body to
   * the caller. The timeout then only covers the wait for the headers; the
   * body stays abortable through `signal`.
   */
  stream?: boolean
}

/** Per-call options every public method accepts. */
export interface CallOptions {
  /** Aborts the call. */
  signal?: AbortSignal
  /** Deadline of one request attempt, overriding `requestTimeoutMs`. */
  requestTimeoutMs?: number
}

function stripTrailingSlash(u: string): string {
  return u.replace(/\/+$/, '')
}

/** The resolved connection of one client: where to send requests and how to sign them. */
export class ApiClient {
  readonly baseUrl: string
  readonly requestTimeoutMs: number
  readonly retry: Required<RetryOptions>
  readonly webSocketCtor: WebSocketConstructor | undefined
  // Credentials live in private fields so that logging or serialising a
  // client (or a Sandbox holding one) can never print them.
  readonly #apiKey: string | undefined
  readonly #accessToken: string | undefined
  readonly #fetch: FetchLike
  readonly #headers: Record<string, string>
  readonly #streamCleanups = new WeakMap<Response, () => void>()

  constructor(opts: ConnectionOptions & { accessToken?: string | undefined } = {}) {
    this.baseUrl = stripTrailingSlash(opts.baseUrl ?? env('SBX_BASE_URL') ?? DEFAULT_BASE_URL)
    // An empty string means "no key", as an unset variable does.
    this.#apiKey = opts.apiKey || env('SBX_API_KEY')
    this.#accessToken = opts.accessToken || undefined
    this.requestTimeoutMs = opts.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS
    this.retry = opts.retry === false ? { ...DEFAULT_RETRY, maxRetries: 0 } : { ...DEFAULT_RETRY, ...definedOnly(opts.retry ?? {}) }
    this.webSocketCtor = opts.WebSocket
    this.#headers = { ...(opts.headers ?? {}) }
    const custom = opts.fetch
    if (custom) {
      this.#fetch = custom
    } else {
      if (typeof globalThis.fetch !== 'function') {
        throw new SandboxError('no global fetch in this runtime; pass the `fetch` option', { code: 'no_fetch' })
      }
      // Called through a wrapper: browsers reject `fetch` invoked as a method of another object.
      this.#fetch = (input, init) => globalThis.fetch(input, init)
    }
  }

  get hasApiKey(): boolean {
    return this.#apiKey !== undefined
  }

  get hasAccessToken(): boolean {
    return this.#accessToken !== undefined
  }

  /** The access token, for handing to a browser or another process. */
  get accessToken(): string | undefined {
    return this.#accessToken
  }

  /** A client for the same server that also carries a sandbox's access token. */
  withAccessToken(accessToken: string | undefined): ApiClient {
    const opts: ConnectionOptions & { accessToken?: string | undefined } = {
      baseUrl: this.baseUrl,
      fetch: this.#fetch,
      requestTimeoutMs: this.requestTimeoutMs,
      retry: this.retry,
      headers: this.#headers,
      accessToken,
    }
    if (this.#apiKey !== undefined) opts.apiKey = this.#apiKey
    if (this.webSocketCtor) opts.WebSocket = this.webSocketCtor
    return new ApiClient(opts)
  }

  /** Authentication headers for a request of the given kind. */
  authHeaders(auth: AuthMode): Record<string, string> {
    if (auth === 'data' && this.#accessToken !== undefined) {
      // Least privilege: a data-plane call needs only the sandbox's token.
      return { 'X-Access-Token': this.#accessToken }
    }
    if (this.#apiKey !== undefined) return { 'X-API-Key': this.#apiKey }
    throw new SandboxError(
      auth === 'control'
        ? 'this operation needs a tenant API key: pass `apiKey` or set SBX_API_KEY (a sandbox access token is not enough)'
        : 'no credentials: pass `apiKey` (or set SBX_API_KEY) or an `accessToken`',
      { code: 'missing_credentials' },
    )
  }

  /** Subprotocols and, when unavoidable, headers that authenticate a WebSocket. */
  webSocketAuth(): { protocols: string[]; headers?: Record<string, string> } {
    if (this.#accessToken !== undefined) return { protocols: ['sbx.v1', `sbx.token.${this.#accessToken}`] }
    return { protocols: ['sbx.v1'], headers: { ...this.#headers, ...this.authHeaders('data') } }
  }

  url(path: string, query?: RequestOptions['query']): string {
    let u = this.baseUrl + path
    if (query) {
      const params = new URLSearchParams()
      for (const [k, v] of Object.entries(query)) {
        if (v === undefined) continue
        if (Array.isArray(v)) for (const item of v as readonly string[]) params.append(k, item)
        else params.append(k, String(v))
      }
      const qs = params.toString()
      if (qs) u += `?${qs}`
    }
    return u
  }

  /**
   * Sends a request and returns the successful (2xx) response. Any other
   * status is thrown as a typed {@link SandboxError}.
   */
  async request(method: string, path: string, opts: RequestOptions): Promise<Response> {
    const url = this.url(path, opts.query)
    const headers: Record<string, string> = { Accept: 'application/json', ...this.#headers, ...this.authHeaders(opts.auth) }
    if (!isBrowser()) headers['User-Agent'] = `sbx-sdk-ts/${VERSION}`
    let body: RequestInit['body'] | undefined
    let streamingBody = false
    if (opts.json !== undefined) {
      headers['Content-Type'] = 'application/json'
      body = JSON.stringify(opts.json)
    } else if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/octet-stream'
      body = opts.body as NonNullable<RequestInit['body']>
      streamingBody = typeof ReadableStream !== 'undefined' && opts.body instanceof ReadableStream
    }
    Object.assign(headers, opts.headers)

    const policy: RetryPolicy = streamingBody ? 'none' : opts.retry
    const timeoutMs = opts.timeoutMs ?? this.requestTimeoutMs
    for (let attempt = 0; ; attempt++) {
      throwIfAborted(opts.signal)
      let failure: SandboxError
      try {
        return await this.#attempt(url, method, headers, body, streamingBody, timeoutMs, opts)
      } catch (e) {
        if (!(e instanceof SandboxError)) throw e // an abort: the caller's own reason
        failure = e
      }
      const delay = this.#retryDelay(failure, policy, attempt)
      if (delay === undefined) throw failure
      await sleep(delay, opts.signal)
    }
  }

  /** Detaches a finished `stream: true` response from the caller's AbortSignal. */
  release(res: Response): void {
    this.#streamCleanups.get(res)?.()
    this.#streamCleanups.delete(res)
  }

  /** Sends a request and parses the JSON answer. */
  async json<T>(method: string, path: string, opts: RequestOptions): Promise<T> {
    const res = await this.request(method, path, opts)
    try {
      return (await res.json()) as T
    } catch (e) {
      throw new ConnectionError(`${method} ${path}: the response body is not valid JSON`, { cause: e, status: res.status })
    }
  }

  /** Sends a request and discards the (normally empty) body. */
  async empty(method: string, path: string, opts: RequestOptions): Promise<void> {
    const res = await this.request(method, path, opts)
    await res.body?.cancel().catch(() => {})
  }

  async #attempt(
    url: string,
    method: string,
    headers: Record<string, string>,
    body: RequestInit['body'] | undefined,
    streamingBody: boolean,
    timeoutMs: number,
    opts: RequestOptions,
  ): Promise<Response> {
    const { controller, dispose } = linkedAbort(opts.signal)
    let timedOut = false
    const timer =
      timeoutMs > 0
        ? setTimeout(() => {
            timedOut = true
            controller.abort()
          }, timeoutMs)
        : undefined
    const finish = (): void => {
      if (timer !== undefined) clearTimeout(timer)
    }
    const fail = (e: unknown): never => {
      finish()
      dispose()
      if (opts.signal?.aborted) throw abortReason(opts.signal)
      if (timedOut) {
        throw new TimeoutError(`${method} ${redact(url)} timed out after ${timeoutMs} ms`, { code: 'request_timeout', cause: e })
      }
      if (e instanceof SandboxError) throw e
      throw new ConnectionError(`${method} ${redact(url)} failed: ${describe(e)}`, { code: 'connection_failed', cause: e })
    }

    let res: Response
    try {
      const init: RequestInit & { duplex?: 'half' } = { method, headers, signal: controller.signal, redirect: 'follow' }
      if (body !== undefined) init.body = body
      if (streamingBody) init.duplex = 'half'
      res = await this.#fetch(url, init)
    } catch (e) {
      return fail(e)
    }

    if (res.status >= 200 && res.status < 300) {
      if (opts.stream) {
        // Headers are in: the deadline has done its job. The body stays
        // tied to the caller's signal through the linked controller.
        finish()
        this.#streamCleanups.set(res, dispose)
        return res
      }
      // Buffer the body under the deadline so that a stalled download cannot hang the call.
      try {
        const buf = await res.arrayBuffer()
        finish()
        dispose()
        return new Response(res.status === 204 || res.status === 205 || res.status === 304 ? null : buf, {
          status: res.status,
          statusText: res.statusText,
          headers: res.headers,
        })
      } catch (e) {
        return fail(e)
      }
    }

    // An error status: read the envelope (bounded by the same deadline).
    let text = ''
    try {
      text = await res.text()
    } catch (e) {
      if (opts.signal?.aborted || timedOut) return fail(e)
    }
    finish()
    dispose()
    let envelope: ErrorBody | undefined
    try {
      const parsed = JSON.parse(text) as { error?: ErrorBody } | null
      if (parsed && typeof parsed === 'object' && parsed.error && typeof parsed.error === 'object') envelope = parsed.error
    } catch {
      // Not the server's envelope (a proxy's HTML page, say): fall back to the status.
    }
    const extra: { requestId?: string; retryAfterMs?: number } = {}
    const requestId = res.headers.get('x-request-id')
    if (requestId) extra.requestId = requestId
    const retryAfterMs = parseRetryAfter(res.headers.get('retry-after'))
    if (retryAfterMs !== undefined) extra.retryAfterMs = retryAfterMs
    throw errorFromBody(res.status, envelope, extra)
  }

  /** The delay before the next attempt, or undefined if the failure must be thrown. */
  #retryDelay(e: SandboxError, policy: RetryPolicy, attempt: number): number | undefined {
    if (policy === 'none' || attempt >= this.retry.maxRetries) return undefined
    let retryable = false
    if (e.status === 429) {
      retryable = e.code === 'quota_exceeded' ? policy === 'create' && this.retry.retryQuotaExceeded : true
    } else if (e.status === 502 || e.status === 503) {
      retryable = policy === 'idempotent' || (policy === 'create' && e.status === 503)
    } else if (e instanceof ConnectionError) {
      retryable = policy === 'idempotent'
    }
    if (!retryable) return undefined
    if (e.retryAfterMs !== undefined) {
      return e.retryAfterMs > this.retry.maxRetryAfterMs ? undefined : e.retryAfterMs
    }
    return backoffDelay(attempt, this.retry.initialDelayMs, this.retry.maxDelayMs)
  }
}

function definedOnly<T extends object>(o: T): Partial<T> {
  const out: Partial<T> = {}
  for (const [k, v] of Object.entries(o)) if (v !== undefined) (out as Record<string, unknown>)[k] = v
  return out
}

/** Error messages carry the path only: never the query string, which could hold caller data. */
function redact(url: string): string {
  const q = url.indexOf('?')
  return q === -1 ? url : url.slice(0, q)
}

function describe(e: unknown): string {
  if (e instanceof Error) {
    const cause = (e as { cause?: unknown }).cause
    const inner = cause instanceof Error && cause.message ? ` (${cause.message})` : ''
    return `${e.message}${inner}`
  }
  return String(e)
}
