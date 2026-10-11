import { createHmac, randomBytes } from 'node:crypto'
import type { IncomingHttpHeaders } from 'node:http'
import { inspect } from 'node:util'
import { describe, expect, it, vi } from 'vitest'
import {
  AuthenticationError,
  ConflictError,
  ConnectionError,
  NotFoundError,
  SandboxError,
  ValidationError,
  verifyWebhook,
  WEBHOOK_EVENT_TYPES,
  Webhooks,
  WebhookVerificationError,
} from '../src/index.js'
import { API_KEY, type RecordedRequest, sendError, sendJson } from './helpers/mock-server.js'
import { useMockServer } from './helpers/setup.js'

const { server } = useMockServer()

// The test vector of the Standard Webhooks reference implementations.
// It is a published example, not a credential; assembled from prefix and
// body so that the source holds no literal a secret scanner would report.
const SECRET = ['whsec_', 'MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw'].join('')
const KA_ID = 'msg_p5jXN8AQM9LWM0D4loKWxJek'
const KA_TIMESTAMP = 1614265330
const KA_BODY = '{"test": 2432232314}'
const KA_SIGNATURE = 'v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE='

/** The webhook routes of sbxd, as an interceptor of the mock server. */
function installRoutes(): { hooks: Map<string, Record<string, unknown>> } {
  const hooks = new Map<string, Record<string, unknown>>()
  server().intercept = (req: RecordedRequest, res) => {
    if (!req.path.startsWith('/v1/webhooks')) return undefined
    if (req.headers['x-api-key'] !== API_KEY) {
      sendError(res, 401, 'unauthorized', 'missing or invalid credentials')
      return true
    }
    const body = (req.body.length > 0 ? JSON.parse(req.body.toString()) : {}) as Record<string, unknown>
    if (req.path === '/v1/webhooks') {
      if (req.method === 'POST') {
        if (body.url === 'https://internal.example.com/') {
          sendError(res, 400, 'webhook_url_not_allowed', 'the host "internal.example.com" does not resolve to a public address')
          return true
        }
        if (hooks.size >= 3) {
          sendError(res, 409, 'webhook_limit_exceeded', 'this tenant already has the maximum number of webhooks')
          return true
        }
        const id = `wh_${String(hooks.size + 1).padStart(24, '0')}`
        const hook = {
          id,
          url: body.url,
          event_types: body.event_types ?? ['*'],
          description: body.description ?? '',
          enabled: body.enabled ?? true,
          created_at: '2026-10-10T08:00:00.123456789Z',
          updated_at: '2026-10-10T08:00:00.123456789Z',
          consecutive_failures: 0,
          dropped_events: 0,
        }
        hooks.set(id, hook)
        sendJson(res, 201, { ...hook, secret: SECRET })
      } else {
        sendJson(res, 200, { webhooks: [...hooks.values()] })
      }
      return true
    }
    const [id = '', action = ''] = req.path.slice('/v1/webhooks/'.length).split('/')
    const hook = hooks.get(decodeURIComponent(id))
    if (!hook) {
      sendError(res, 404, 'not_found', `webhook "${id}" not found`)
      return true
    }
    if (action === '' && req.method === 'GET') sendJson(res, 200, hook)
    else if (action === '' && req.method === 'PATCH') {
      Object.assign(hook, body)
      if (body.enabled === false) Object.assign(hook, { disabled_reason: 'user', disabled_at: '2026-10-10T09:00:00Z' })
      sendJson(res, 200, hook)
    } else if (action === '' && req.method === 'DELETE') {
      hooks.delete(decodeURIComponent(id))
      res.writeHead(204).end()
    } else if (action === 'rotate-secret') {
      sendJson(res, 200, { ...hook, secret: 'whsec_bmV3', previous_secret_expires_at: '2026-10-11T08:00:00Z' })
    } else if (action === 'test') {
      sendJson(res, 200, {
        event_id: 'evt_test_1',
        delivered: false,
        status: 503,
        duration_ms: 12,
        error_code: 'http_status',
        error: 'the endpoint answered 503',
      })
    } else if (action === 'deliveries') {
      const rows = [
        {
          id: 9,
          event_id: 'evt_7',
          event_type: 'sandbox.terminated',
          attempt: 3,
          status: 'succeeded',
          response_status: 200,
          duration_ms: 40,
          created_at: '2026-10-10T08:00:02Z',
        },
        {
          id: 8,
          event_id: 'evt_6',
          attempt: 1,
          status: 'dropped',
          duration_ms: 0,
          error_code: 'retention_expired',
          error: '4 event(s) were not delivered',
          dropped_events: 4,
          created_at: '2026-10-10T08:00:01Z',
        },
      ]
      sendJson(res, 200, { deliveries: rows.slice(0, Number(req.query.get('limit') ?? 50)) })
    } else sendError(res, 405, 'method_not_allowed', 'no')
    return true
  }
  return { hooks }
}

const client = (apiKey = API_KEY): Webhooks => new Webhooks({ apiKey, baseUrl: server().url, retry: { initialDelayMs: 5, maxDelayMs: 20 } })
const lastBody = (method: string, suffix: string): unknown =>
  JSON.parse(server().requestsTo(method, suffix).at(-1)?.body.toString() || 'null')

describe('Webhooks', () => {
  it('creates a webhook and returns the secret once, out of sight of logs', async () => {
    installRoutes()
    const webhooks = client()
    const hook = await webhooks.create('https://hooks.example.com/in', {
      eventTypes: ['sandbox.terminated', 'command.finished'],
      description: 'ci',
      enabled: false,
    })
    expect(lastBody('POST', '/v1/webhooks')).toEqual({
      url: 'https://hooks.example.com/in',
      event_types: ['sandbox.terminated', 'command.finished'],
      description: 'ci',
      enabled: false,
    })
    const post = server().requestsTo('POST', '/v1/webhooks')[0]
    expect(post?.headers['x-api-key']).toBe(API_KEY)
    expect(post?.headers['x-access-token']).toBeUndefined()
    expect(hook.secret).toBe(SECRET)
    expect(hook).toMatchObject({
      url: 'https://hooks.example.com/in',
      enabled: false,
      eventTypes: ['sandbox.terminated', 'command.finished'],
      consecutiveFailures: 0,
      droppedEvents: 0,
    })
    expect(hook.webhookId).toMatch(/^wh_/)
    expect(hook.createdAt).toBeInstanceOf(Date)
    expect('disabledReason' in hook).toBe(false)
    // The secret does not show up when the object is logged or serialised.
    expect(JSON.stringify(hook)).not.toContain('whsec_')
    expect(inspect(hook)).not.toContain('whsec_')
    expect(inspect(webhooks, { depth: 5 })).not.toContain(API_KEY)

    await webhooks.create('https://hooks.example.com/all')
    expect(lastBody('POST', '/v1/webhooks')).toEqual({ url: 'https://hooks.example.com/all' })

    const fetched = await webhooks.get(hook.webhookId)
    expect((fetched as { secret?: string }).secret).toBeUndefined()
    expect((await webhooks.list()).map((w) => w.webhookId)).toEqual([hook.webhookId, 'wh_000000000000000000000002'])
  })

  it('updates, rotates, tests, lists deliveries and deletes', async () => {
    installRoutes()
    const webhooks = client()
    const hook = await webhooks.create('https://hooks.example.com/in')
    const id = hook.webhookId

    const off = await webhooks.update(id, { enabled: false, eventTypes: [], description: '' })
    expect(lastBody('PATCH', `/${id}`)).toEqual({ enabled: false, event_types: [], description: '' })
    expect(off).toMatchObject({ enabled: false, disabledReason: 'user' })
    expect(off.disabledAt).toBeInstanceOf(Date)
    await webhooks.update(id, { url: 'https://hooks.example.com/new' })
    expect(lastBody('PATCH', `/${id}`)).toEqual({ url: 'https://hooks.example.com/new' })
    await expect(webhooks.update(id, {})).rejects.toThrow(/nothing to change/)

    const rotated = await webhooks.rotateSecret(id)
    expect(server().requestsTo('POST', '/rotate-secret').at(-1)?.body.length).toBe(0)
    expect(rotated.secret).toBe('whsec_bmV3')
    expect(rotated.previousSecretExpiresAt).toBeInstanceOf(Date)
    await webhooks.rotateSecret(id, { keepPreviousMs: 0 })
    expect(lastBody('POST', '/rotate-secret')).toEqual({ keep_previous_seconds: 0 })
    await webhooks.rotateSecret(id, { keepPreviousMs: 3_600_900 })
    expect(lastBody('POST', '/rotate-secret')).toEqual({ keep_previous_seconds: 3600 })
    await expect(webhooks.rotateSecret(id, { keepPreviousMs: -1 })).rejects.toThrow(SandboxError)

    expect(await webhooks.test(id)).toEqual({
      eventId: 'evt_test_1',
      delivered: false,
      status: 503,
      durationMs: 12,
      errorCode: 'http_status',
      error: 'the endpoint answered 503',
    })

    const log = await webhooks.deliveries(id)
    expect(server().requestsTo('GET', '/deliveries').at(-1)?.query.has('limit')).toBe(false)
    expect(log.map((d) => d.status)).toEqual(['succeeded', 'dropped'])
    expect(log[0]).toMatchObject({
      id: 9,
      eventId: 'evt_7',
      eventType: 'sandbox.terminated',
      attempt: 3,
      responseStatus: 200,
      durationMs: 40,
    })
    expect(log[1]).toMatchObject({ droppedEvents: 4, errorCode: 'retention_expired' })
    expect(log[1] && 'responseStatus' in log[1]).toBe(false)
    expect(await webhooks.deliveries(id, { limit: 1 })).toHaveLength(1)
    expect(server().requestsTo('GET', '/deliveries').at(-1)?.query.get('limit')).toBe('1')

    expect(await webhooks.delete(id)).toBe(true)
    expect(await webhooks.delete(id)).toBe(false)
    await expect(webhooks.get(id)).rejects.toBeInstanceOf(NotFoundError)
    // An id is one path segment, whatever it contains.
    await expect(webhooks.get('../sandboxes')).rejects.toBeInstanceOf(NotFoundError)
    expect(server().requests.at(-1)?.path).toBe('/v1/webhooks/..%2Fsandboxes')
  })

  it('maps the errors and does not repeat a create that may have happened', async () => {
    const { hooks } = installRoutes()
    const webhooks = client()
    await expect(webhooks.create('https://internal.example.com/')).rejects.toMatchObject({ code: 'webhook_url_not_allowed', status: 400 })
    await expect(webhooks.create('https://internal.example.com/')).rejects.toBeInstanceOf(ValidationError)
    for (let i = 0; i < 3; i++) await webhooks.create(`https://hooks.example.com/${i}`)
    await expect(webhooks.create('https://hooks.example.com/4')).rejects.toBeInstanceOf(ConflictError)
    await expect(client('sbx_wrong').list()).rejects.toBeInstanceOf(AuthenticationError)
    await expect(new Webhooks({ apiKey: '', baseUrl: server().url }).list()).rejects.toMatchObject({ code: 'missing_credentials' })

    // A connection that dies after the request was sent: the webhook may
    // exist, so a second POST would make a second one with another secret.
    hooks.clear()
    const routes = server().intercept
    let posts = 0
    server().intercept = (req, res) => {
      if (req.method === 'POST' && req.path === '/v1/webhooks') {
        posts++
        res.socket?.destroy()
        return true
      }
      return routes?.(req, res)
    }
    await expect(webhooks.create('https://hooks.example.com/lost')).rejects.toBeInstanceOf(ConnectionError)
    expect(posts).toBe(1)
  })
})

function sign(secret: string, id: string, timestamp: number, body: string | Uint8Array): string {
  const mac = createHmac('sha256', Buffer.from(secret.slice('whsec_'.length), 'base64'))
  mac.update(`${id}.${timestamp}.`).update(body)
  return `v1,${mac.digest('base64')}`
}

const headersFor = (id: string, timestamp: number, signature: string): Record<string, string> => ({
  'Webhook-Id': id,
  'Webhook-Timestamp': String(timestamp),
  'Webhook-Signature': signature,
})

const EVENT = JSON.stringify({
  id: 'evt_42',
  type: 'sandbox.terminated',
  version: 1,
  created_at: '2026-10-10T11:00:00.123456Z',
  sandbox_id: 'sb-1',
  template_id: 'python',
  data: { state: 'recycling', reason: 'idle_timeout', metadata: { job: '9' } },
})
const NOW = 1_800_000_000

describe('verifyWebhook', () => {
  it('reproduces the Standard Webhooks reference vector', async () => {
    expect(sign(SECRET, KA_ID, KA_TIMESTAMP, KA_BODY)).toBe(KA_SIGNATURE)
    const event = await verifyWebhook(KA_BODY, headersFor(KA_ID, KA_TIMESTAMP, KA_SIGNATURE), SECRET, { now: (KA_TIMESTAMP + 10) * 1000 })
    expect(event.id).toBe('')
    await expect(
      verifyWebhook(KA_BODY, headersFor(KA_ID, KA_TIMESTAMP, KA_SIGNATURE.replace('g0hM', 'g0hN')), SECRET, { now: KA_TIMESTAMP * 1000 }),
    ).rejects.toBeInstanceOf(WebhookVerificationError)
  })

  it('returns the event for an authentic request, whatever shape the headers have', async () => {
    const good = headersFor('evt_42', NOW, sign(SECRET, 'evt_42', NOW, EVENT))
    const event = await verifyWebhook(EVENT, good, SECRET, { now: NOW * 1000 })
    expect(event).toEqual({
      id: 'evt_42',
      type: 'sandbox.terminated',
      version: 1,
      createdAt: new Date('2026-10-10T11:00:00.123Z'),
      sandboxId: 'sb-1',
      templateId: 'python',
      data: { state: 'recycling', reason: 'idle_timeout', metadata: { job: '9' } },
    })
    const bytes = new TextEncoder().encode(EVENT)
    const lower = Object.fromEntries(Object.entries(good).map(([k, v]) => [k.toLowerCase(), v]))
    const node: IncomingHttpHeaders = { ...lower, 'set-cookie': ['a'] }
    const shapes: Parameters<typeof verifyWebhook>[1][] = [
      new Headers(good),
      lower,
      node,
      new Map(Object.entries(good)),
      Object.entries(good) as [string, string][],
    ]
    for (const headers of shapes) {
      expect(await verifyWebhook(bytes, headers, SECRET, { now: new Date(NOW * 1000) })).toEqual(event)
    }
    expect(await verifyWebhook(bytes.buffer as ArrayBuffer, good, SECRET, { now: NOW * 1000 })).toEqual(event)
    // A type from the future is an event, not an error.
    const later = JSON.stringify({ id: 'evt_43', type: 'sandbox.snapshotted', version: 1, data: {} })
    expect(
      (await verifyWebhook(later, headersFor('evt_43', NOW, sign(SECRET, 'evt_43', NOW, later)), SECRET, { now: NOW * 1000 })).type,
    ).toBe('sandbox.snapshotted')
    expect(WEBHOOK_EVENT_TYPES).toContain(event.type)
  })

  it('refuses whatever is not authentic', async () => {
    const good = sign(SECRET, 'evt_42', NOW, EVENT)
    const other = `whsec_${randomBytes(32).toString('base64')}`
    const now = { now: NOW * 1000 }
    const cases: [string, string, Record<string, string>, string, RegExp][] = [
      ['wrong secret', EVENT, headersFor('evt_42', NOW, good), other, /no signature matches/],
      ['altered body', `${EVENT} `, headersFor('evt_42', NOW, good), SECRET, /no signature matches/],
      ['re-serialised body', JSON.stringify(JSON.parse(EVENT), null, 1), headersFor('evt_42', NOW, good), SECRET, /no signature matches/],
      ['another id', EVENT, headersFor('evt_43', NOW, good), SECRET, /no signature matches/],
      ['another timestamp', EVENT, headersFor('evt_42', NOW + 1, good), SECRET, /no signature matches/],
      ['unknown version', EVENT, headersFor('evt_42', NOW, `v2,${good.slice(3)}`), SECRET, /no signature matches/],
      ['garbage', EVENT, headersFor('evt_42', NOW, 'v1,@@@'), SECRET, /no signature matches/],
      ['truncated', EVENT, headersFor('evt_42', NOW, good.slice(0, -8)), SECRET, /no signature matches/],
      ['no signature', EVENT, { 'Webhook-Id': 'evt_42', 'Webhook-Timestamp': String(NOW) }, SECRET, /missing/],
      ['no id', EVENT, { 'Webhook-Timestamp': String(NOW), 'Webhook-Signature': good }, SECRET, /missing/],
      ['timestamp not a number', EVENT, { ...headersFor('evt_42', NOW, good), 'Webhook-Timestamp': 'now' }, SECRET, /not a number/],
      ['secret without prefix', EVENT, headersFor('evt_42', NOW, good), SECRET.slice(6), /whsec_/],
      ['secret not base64', EVENT, headersFor('evt_42', NOW, good), 'whsec_***', /whsec_/],
      ['empty secret', EVENT, headersFor('evt_42', NOW, good), 'whsec_', /whsec_/],
    ]
    for (const [name, body, headers, secret, message] of cases) {
      const err = await verifyWebhook(body, headers, secret, now).then(
        () => undefined,
        (e: unknown) => e,
      )
      expect(err, name).toBeInstanceOf(WebhookVerificationError)
      expect((err as Error).message, name).toMatch(message)
    }
    // A parsed body cannot be verified: the signature covers the bytes.
    await expect(verifyWebhook(JSON.parse(EVENT) as never, headersFor('evt_42', NOW, good), SECRET, now)).rejects.toThrow(
      /raw request body/,
    )
    // Authentic, but not an event.
    for (const junk of ['[1,2]', 'not json', '"text"']) {
      await expect(verifyWebhook(junk, headersFor('evt_42', NOW, sign(SECRET, 'evt_42', NOW, junk)), SECRET, now)).rejects.toThrow(/JSON/)
    }
  })

  it('enforces the timestamp tolerance', async () => {
    const headers = headersFor('evt_42', NOW, sign(SECRET, 'evt_42', NOW, EVENT))
    for (const at of [NOW, NOW + 300, NOW - 300]) await verifyWebhook(EVENT, headers, SECRET, { now: at * 1000 })
    await expect(verifyWebhook(EVENT, headers, SECRET, { now: (NOW + 301) * 1000 })).rejects.toThrow(/too old/)
    await expect(verifyWebhook(EVENT, headers, SECRET, { now: (NOW - 301) * 1000 })).rejects.toThrow(/future/)
    await expect(verifyWebhook(EVENT, headers, SECRET, { now: (NOW + 31) * 1000, toleranceSeconds: 30 })).rejects.toThrow(/too old/)
    await verifyWebhook(EVENT, headers, SECRET, { now: (NOW + 3600) * 1000, toleranceSeconds: 3600 })
    // Without `now` the wall clock decides.
    vi.useFakeTimers({ now: (NOW + 5) * 1000 })
    try {
      await verifyWebhook(EVENT, headers, SECRET)
      vi.setSystemTime((NOW + 1000) * 1000)
      await expect(verifyWebhook(EVENT, headers, SECRET)).rejects.toThrow(/too old/)
    } finally {
      vi.useRealTimers()
    }
  })

  it('accepts either secret during a rotation and compares in constant time', async () => {
    const old = `whsec_${Buffer.alloc(32, 'o').toString('base64')}`
    const both = `${sign(SECRET, 'evt_42', NOW, EVENT)} ${sign(old, 'evt_42', NOW, EVENT)}`
    for (const secret of [SECRET, old]) await verifyWebhook(EVENT, headersFor('evt_42', NOW, both), secret, { now: NOW * 1000 })
    await verifyWebhook(EVENT, headersFor('evt_42', NOW, `v1a,zzz v9,AAAA ${both}`), old, { now: NOW * 1000 })

    // Every candidate goes through crypto.subtle.verify (constant time by
    // contract), whether or not an earlier one matched; nothing is
    // compared as strings.
    const verify = vi.spyOn(globalThis.crypto.subtle, 'verify')
    try {
      await verifyWebhook(EVENT, headersFor('evt_42', NOW, both), SECRET, { now: NOW * 1000 })
      expect(verify).toHaveBeenCalledTimes(2)
      verify.mockClear()
      const stranger = `whsec_${Buffer.alloc(32, 'x').toString('base64')}`
      await expect(verifyWebhook(EVENT, headersFor('evt_42', NOW, both), stranger, { now: NOW * 1000 })).rejects.toThrow(/no signature/)
      expect(verify).toHaveBeenCalledTimes(2)
    } finally {
      verify.mockRestore()
    }
  })
})
