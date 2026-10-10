import { readFileSync } from 'node:fs'
import { inspect } from 'node:util'
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  AuthenticationError,
  ConnectionError,
  PoolEmptyError,
  QuotaExceededError,
  RateLimitError,
  SandboxError,
  ServiceUnavailableError,
  TimeoutError,
} from '../src/errors.js'
import { ApiClient, type FetchLike } from '../src/http.js'
import { VERSION } from '../src/version.js'

type Call = { url: string; init: RequestInit }

function errorResponse(status: number, code: string, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify({ error: { code, message: `${code}!`, request_id: 'rid-1', details: { class: 'default' } } }), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  })
}

function ok(body: unknown = { ok: true }): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
}

/** A fetch that answers from a script and records what it was asked. */
function scripted(...script: (Response | Error | ((call: Call) => Response | Promise<Response>))[]): { fetch: FetchLike; calls: Call[] } {
  const calls: Call[] = []
  const fetch: FetchLike = async (url, init = {}) => {
    const call = { url, init }
    calls.push(call)
    const step = script[Math.min(calls.length - 1, script.length - 1)]
    if (step instanceof Error) throw step
    if (typeof step === 'function') return step(call)
    return (step as Response).clone()
  }
  return { fetch, calls }
}

const fast = { initialDelayMs: 1, maxDelayMs: 2 }

/** For `await p.then(unexpected, asError)`: the rejection of a call that must fail. */
const asError = (e: unknown): SandboxError => e as SandboxError
const unexpected = (): never => {
  throw new Error('expected the call to fail')
}

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllEnvs()
})

describe('ApiClient credentials', () => {
  it('sends the API key on control-plane calls and the token on data-plane calls', async () => {
    const { fetch, calls } = scripted(ok())
    const api = new ApiClient({ apiKey: 'sbx_k', accessToken: 'sbt_t', fetch, baseUrl: 'http://h/' })
    await api.json('GET', '/v1/sandboxes', { auth: 'control', retry: 'none' })
    await api.json('GET', '/v1/sandboxes/sb-1/commands', { auth: 'data', retry: 'none' })
    const h0 = calls[0]?.init.headers as Record<string, string>
    const h1 = calls[1]?.init.headers as Record<string, string>
    expect(calls[0]?.url).toBe('http://h/v1/sandboxes')
    expect(h0['X-API-Key']).toBe('sbx_k')
    expect(h0['X-Access-Token']).toBeUndefined()
    expect(h1['X-Access-Token']).toBe('sbt_t')
    expect(h1['X-API-Key']).toBeUndefined()
    expect(h0['User-Agent']).toBe(`sbx-sdk-ts/${VERSION}`)
  })

  it('falls back to the API key on the data plane when there is no token', async () => {
    const { fetch, calls } = scripted(ok())
    await new ApiClient({ apiKey: 'sbx_k', fetch }).json('GET', '/x', { auth: 'data', retry: 'none' })
    expect((calls[0]?.init.headers as Record<string, string> | undefined)?.['X-API-Key']).toBe('sbx_k')
  })

  it('reads SBX_API_KEY and SBX_BASE_URL from the environment', async () => {
    vi.stubEnv('SBX_API_KEY', 'sbx_env')
    vi.stubEnv('SBX_BASE_URL', 'http://from-env:9/')
    const { fetch, calls } = scripted(ok())
    await new ApiClient({ fetch }).json('GET', '/v1/templates', { auth: 'control', retry: 'none' })
    expect(calls[0]?.url).toBe('http://from-env:9/v1/templates')
    expect((calls[0]?.init.headers as Record<string, string> | undefined)?.['X-API-Key']).toBe('sbx_env')
  })

  it('refuses a control-plane call with only a token, without touching the network', async () => {
    vi.stubEnv('SBX_API_KEY', '')
    const { fetch, calls } = scripted(ok())
    const api = new ApiClient({ accessToken: 'sbt_t', fetch })
    await expect(api.json('GET', '/v1/sandboxes', { auth: 'control', retry: 'none' })).rejects.toMatchObject({
      code: 'missing_credentials',
    })
    expect(calls).toHaveLength(0)
  })

  it('never exposes credentials through inspection, serialisation or error messages', async () => {
    const { fetch } = scripted(errorResponse(401, 'unauthorized'))
    const api = new ApiClient({ apiKey: 'sbx_SECRETKEY', accessToken: 'sbt_SECRETTOKEN', fetch })
    const dump = inspect(api, { depth: 10, showHidden: true }) + JSON.stringify(api)
    expect(dump).not.toContain('SECRET')
    const err = await api
      .json('GET', '/v1/x', { auth: 'control', retry: 'none', query: { token: 'sbt_SECRETTOKEN' } })
      .then(unexpected, asError)
    expect(err).toBeInstanceOf(AuthenticationError)
    expect(inspect(err, { depth: 10 })).not.toContain('SECRET')
  })

  it('offers the token as a WebSocket subprotocol, and the API key only as a header', () => {
    const { fetch } = scripted(ok())
    expect(new ApiClient({ apiKey: 'sbx_k', accessToken: 'sbt_t', fetch }).webSocketAuth()).toEqual({
      protocols: ['sbx.v1', 'sbx.token.sbt_t'],
    })
    expect(new ApiClient({ apiKey: 'sbx_k', fetch }).webSocketAuth()).toEqual({ protocols: ['sbx.v1'], headers: { 'X-API-Key': 'sbx_k' } })
  })
})

describe('ApiClient request building', () => {
  it('encodes query parameters, repeating arrays and skipping undefined', async () => {
    const { fetch, calls } = scripted(ok())
    await new ApiClient({ apiKey: 'k', fetch, baseUrl: 'http://h' }).json('GET', '/p', {
      auth: 'control',
      retry: 'none',
      query: { a: 'x y', b: undefined, m: ['k=v', 'k2=v&2'], n: 5 },
    })
    expect(calls[0]?.url).toBe('http://h/p?a=x+y&m=k%3Dv&m=k2%3Dv%262&n=5')
  })

  it('sends JSON and raw bodies with the right content type, and custom headers', async () => {
    const { fetch, calls } = scripted(ok())
    const api = new ApiClient({ apiKey: 'k', fetch, headers: { 'X-Proxy': 'p' } })
    await api.json('POST', '/a', { auth: 'control', retry: 'none', json: { x: 1 } })
    await api.json('PUT', '/b', { auth: 'control', retry: 'none', body: new Uint8Array([1, 2]) })
    expect(calls[0]?.init.body).toBe('{"x":1}')
    expect(calls[0]?.init.headers).toMatchObject({ 'Content-Type': 'application/json', 'X-Proxy': 'p' })
    expect(calls[1]?.init.headers).toMatchObject({ 'Content-Type': 'application/octet-stream' })
  })

  it('keeps the SDK version equal to package.json', () => {
    const pkg = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8')) as { version: string }
    expect(VERSION).toBe(pkg.version)
  })
})

describe('ApiClient retries', () => {
  it('waits for Retry-After on 429 and then succeeds', async () => {
    vi.useFakeTimers()
    const { fetch, calls } = scripted(errorResponse(429, 'rate_limited', { 'Retry-After': '2' }), ok({ n: 1 }))
    const p = new ApiClient({ apiKey: 'k', fetch }).json<{ n: number }>('GET', '/x', { auth: 'control', retry: 'idempotent' })
    await vi.advanceTimersByTimeAsync(1900)
    expect(calls).toHaveLength(1) // still waiting out the server's 2 s
    await vi.advanceTimersByTimeAsync(200)
    expect(await p).toEqual({ n: 1 })
    expect(calls).toHaveLength(2)
  })

  it('retries 503 pool_empty on create, and gives up after maxRetries with the typed error', async () => {
    const { fetch, calls } = scripted(errorResponse(503, 'pool_empty'))
    const api = new ApiClient({ apiKey: 'k', fetch, retry: { ...fast, maxRetries: 2 } })
    const err = await api.json('POST', '/v1/sandboxes', { auth: 'control', retry: 'create', json: {} }).then(unexpected, asError)
    expect(err).toBeInstanceOf(PoolEmptyError)
    expect(err).toMatchObject({ status: 503, requestId: 'rid-1' })
    expect(calls).toHaveLength(3)
  })

  it('does not retry when retries are switched off', async () => {
    const { fetch, calls } = scripted(errorResponse(429, 'rate_limited', { 'Retry-After': '1' }))
    const api = new ApiClient({ apiKey: 'k', fetch, retry: false })
    await expect(api.json('GET', '/x', { auth: 'control', retry: 'idempotent' })).rejects.toBeInstanceOf(RateLimitError)
    expect(calls).toHaveLength(1)
  })

  it('throws at once when the server asks for a longer wait than maxRetryAfterMs', async () => {
    const { fetch, calls } = scripted(errorResponse(429, 'rate_limited', { 'Retry-After': '120' }))
    const err = await new ApiClient({ apiKey: 'k', fetch })
      .json('GET', '/x', { auth: 'control', retry: 'idempotent' })
      .then(unexpected, asError)
    expect(err).toBeInstanceOf(RateLimitError)
    expect(err.retryAfter).toBe(120)
    expect(calls).toHaveLength(1)
  })

  it('does not retry quota_exceeded unless asked to', async () => {
    const a = scripted(errorResponse(429, 'quota_exceeded', { 'Retry-After': '0.001' }))
    await expect(
      new ApiClient({ apiKey: 'k', fetch: a.fetch }).json('POST', '/v1/sandboxes', { auth: 'control', retry: 'create' }),
    ).rejects.toBeInstanceOf(QuotaExceededError)
    expect(a.calls).toHaveLength(1)
    const b = scripted(errorResponse(429, 'quota_exceeded', { 'Retry-After': '0.001' }), ok())
    await new ApiClient({ apiKey: 'k', fetch: b.fetch, retry: { retryQuotaExceeded: true } }).json('POST', '/v1/sandboxes', {
      auth: 'control',
      retry: 'create',
    })
    expect(b.calls).toHaveLength(2)
  })

  it('retries a network failure for idempotent calls only', async () => {
    const a = scripted(new TypeError('fetch failed'), ok())
    await new ApiClient({ apiKey: 'k', fetch: a.fetch, retry: fast }).json('GET', '/x', { auth: 'control', retry: 'idempotent' })
    expect(a.calls).toHaveLength(2)
    for (const policy of ['create', 'rate-limit', 'none'] as const) {
      const b = scripted(new TypeError('fetch failed'), ok())
      const err = await new ApiClient({ apiKey: 'k', fetch: b.fetch, retry: fast })
        .json('POST', '/x', { auth: 'control', retry: policy })
        .then(unexpected, asError)
      expect(err).toBeInstanceOf(ConnectionError)
      expect(b.calls).toHaveLength(1)
    }
  })

  it('retries only 429 rate_limited under the rate-limit policy', async () => {
    const a = scripted(errorResponse(429, 'rate_limited', { 'Retry-After': '0.001' }), ok())
    await new ApiClient({ apiKey: 'k', fetch: a.fetch }).json('POST', '/x', { auth: 'data', retry: 'rate-limit' })
    expect(a.calls).toHaveLength(2)
    const b = scripted(errorResponse(502, 'agent_unavailable', { 'Retry-After': '0.001' }), ok())
    await expect(
      new ApiClient({ apiKey: 'k', fetch: b.fetch }).json('POST', '/x', { auth: 'data', retry: 'rate-limit' }),
    ).rejects.toBeInstanceOf(ServiceUnavailableError)
    expect(b.calls).toHaveLength(1)
  })

  it('never retries 4xx errors', async () => {
    const { fetch, calls } = scripted(errorResponse(404, 'not_found'))
    await expect(new ApiClient({ apiKey: 'k', fetch }).json('GET', '/x', { auth: 'control', retry: 'idempotent' })).rejects.toMatchObject({
      status: 404,
    })
    expect(calls).toHaveLength(1)
  })

  it('never retries a request whose body is a stream', async () => {
    const { fetch, calls } = scripted(errorResponse(503, 'shutting_down', { 'Retry-After': '0.001' }), ok())
    const body = new ReadableStream<Uint8Array>({ start: (c) => c.close() })
    await expect(
      new ApiClient({ apiKey: 'k', fetch }).json('PUT', '/x', { auth: 'data', retry: 'idempotent', body }),
    ).rejects.toBeInstanceOf(ServiceUnavailableError)
    expect(calls).toHaveLength(1)
    expect((calls[0]?.init as { duplex?: string } | undefined)?.duplex).toBe('half')
  })
})

describe('ApiClient abort and timeout', () => {
  const hanging: FetchLike = (_url, init) =>
    new Promise((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
    })

  it('rejects with the caller’s abort reason', async () => {
    const ac = new AbortController()
    const p = new ApiClient({ apiKey: 'k', fetch: hanging }).json('GET', '/x', { auth: 'control', retry: 'idempotent', signal: ac.signal })
    ac.abort(new Error('user cancelled'))
    await expect(p).rejects.toThrow('user cancelled')
  })

  it('rejects immediately if the signal is already aborted', async () => {
    const { fetch, calls } = scripted(ok())
    const p = new ApiClient({ apiKey: 'k', fetch }).json('GET', '/x', { auth: 'control', retry: 'none', signal: AbortSignal.abort() })
    await expect(p).rejects.toMatchObject({ name: 'AbortError' })
    expect(calls).toHaveLength(0)
  })

  it('aborts the wait between retries', async () => {
    const { fetch, calls } = scripted(errorResponse(429, 'rate_limited', { 'Retry-After': '20' }))
    const ac = new AbortController()
    const p = new ApiClient({ apiKey: 'k', fetch }).json('GET', '/x', { auth: 'control', retry: 'idempotent', signal: ac.signal })
    setTimeout(() => ac.abort(), 20)
    await expect(p).rejects.toMatchObject({ name: 'AbortError' })
    expect(calls).toHaveLength(1)
  })

  it('turns an expired deadline into a TimeoutError', async () => {
    const api = new ApiClient({ apiKey: 'k', fetch: hanging, requestTimeoutMs: 20, retry: false })
    const err = await api.json('GET', '/x?secret=1', { auth: 'control', retry: 'none' }).then(unexpected, asError)
    expect(err).toBeInstanceOf(TimeoutError)
    expect(err.code).toBe('request_timeout')
    expect(err.message).not.toContain('secret')
  })

  it('wraps a network failure in a ConnectionError with the cause', async () => {
    const cause = new TypeError('fetch failed', { cause: new Error('ECONNREFUSED') })
    const err = await new ApiClient({ apiKey: 'k', fetch: scripted(cause).fetch, retry: false })
      .json('GET', '/x', { auth: 'control', retry: 'idempotent' })
      .then(unexpected, asError)
    expect(err).toBeInstanceOf(ConnectionError)
    expect(err.cause).toBe(cause)
    expect(err.message).toContain('ECONNREFUSED')
  })

  it('maps a non-JSON error page from a proxy by its status', async () => {
    const { fetch } = scripted(new Response('<html>502</html>', { status: 502 }))
    const err = await new ApiClient({ apiKey: 'k', fetch, retry: false })
      .json('GET', '/x', { auth: 'control', retry: 'none' })
      .then(unexpected, asError)
    expect(err).toBeInstanceOf(ServiceUnavailableError)
    expect(err).toBeInstanceOf(SandboxError)
  })
})
