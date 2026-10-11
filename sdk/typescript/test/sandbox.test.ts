import { inspect } from 'node:util'
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  AuthenticationError,
  NotFoundError,
  PoolEmptyError,
  QuotaExceededError,
  Sandbox,
  SandboxError,
  SandboxTerminatedError,
  TimeoutError,
  ValidationError,
} from '../src/index.js'
import { API_KEY, OTHER_KEY, sendError, sendJson } from './helpers/mock-server.js'
import { useMockServer } from './helpers/setup.js'

const { server, create } = useMockServer()
const conn = (): { apiKey: string; baseUrl: string } => ({ apiKey: API_KEY, baseUrl: server().url })

afterEach(() => vi.unstubAllEnvs())

describe('Sandbox.create', () => {
  it('claims a sandbox and maps every option onto the request', async () => {
    const sb = await create({
      timeoutMs: 120_000,
      envs: { FOO: 'bar' },
      metadata: { user: 'u1' },
      network: {
        mode: 'allowlist',
        allowDomains: ['pypi.org', '*.pythonhosted.org'],
        allowCidrs: ['1.1.1.1/32'],
        denyCidrs: ['8.8.8.8/32'],
      },
    })
    const post = server().requestsTo('POST', '/v1/sandboxes')[0]
    expect(JSON.parse(post?.body.toString() ?? '')).toEqual({
      template_id: 'python',
      timeout_ms: 120000,
      env: { FOO: 'bar' },
      metadata: { user: 'u1' },
      network: {
        mode: 'allowlist',
        allow_domains: ['pypi.org', '*.pythonhosted.org'],
        allow_cidrs: ['1.1.1.1/32'],
        deny_cidrs: ['8.8.8.8/32'],
      },
    })
    expect(post?.headers['x-api-key']).toBe(API_KEY)
    expect(post?.headers['user-agent']).toMatch(/^sbx-sdk-ts\/\d+\.\d+\.\d+/)
    expect(sb.sandboxId).toMatch(/^sb-[0-9a-f]{16}$/)
    expect(sb.accessToken).toBe(server().sandbox(sb.sandboxId).token)
    expect(sb.info).toMatchObject({
      sandboxId: sb.sandboxId,
      templateId: 'python',
      templateVersion: 3,
      state: 'running',
      metadata: { user: 'u1' },
      network: { mode: 'allowlist', allowDomains: ['pypi.org', '*.pythonhosted.org'] },
    })
    expect(sb.info?.expiresAt).toBeInstanceOf(Date)
    expect(sb.info?.createdAt).toBeInstanceOf(Date)
  })

  it('sends only template_id when no options are given', async () => {
    await create()
    expect(JSON.parse(server().requestsTo('POST', '/v1/sandboxes')[0]?.body.toString() ?? '')).toEqual({ template_id: 'python' })
  })

  it('uses SBX_API_KEY and SBX_BASE_URL when no options are given', async () => {
    vi.stubEnv('SBX_API_KEY', API_KEY)
    vi.stubEnv('SBX_BASE_URL', server().url)
    const sb = await Sandbox.create('python')
    expect(sb.info?.state).toBe('running')
  })

  it('maps create errors', async () => {
    await expect(Sandbox.create('nope', conn())).rejects.toMatchObject({ name: 'NotFoundError', code: 'template_not_found' })
    const tooLong = await Sandbox.create('python', { ...conn(), timeoutMs: 10 * 3_600_000 }).catch((e) => e)
    expect(tooLong).toBeInstanceOf(ValidationError)
    expect(tooLong).toMatchObject({ code: 'invalid_timeout', details: { max_timeout_ms: 3_600_000 } })
    await expect(Sandbox.create('python', { apiKey: 'sbx_wrong', baseUrl: server().url })).rejects.toBeInstanceOf(AuthenticationError)
  })

  it('retries 429 rate_limited and 503 pool_empty, honouring Retry-After', async () => {
    let n = 0
    const at: number[] = []
    server().intercept = (req, res) => {
      if (req.method !== 'POST' || req.path !== '/v1/sandboxes') return false
      at.push(Date.now())
      n++
      if (n === 1) sendError(res, 429, 'rate_limited', 'slow down', { class: 'create' }, '0.15')
      else if (n === 2) sendError(res, 503, 'pool_empty', 'no warm sandbox', undefined, '0.1')
      else return false
      return true
    }
    const sb = await Sandbox.create('python', conn())
    expect(sb.sandboxId).toMatch(/^sb-/)
    expect(n).toBe(3)
    expect((at[1] ?? 0) - (at[0] ?? 0)).toBeGreaterThanOrEqual(140)
    expect((at[2] ?? 0) - (at[1] ?? 0)).toBeGreaterThanOrEqual(90)
    expect(server().sandboxes.size).toBe(1)
  })

  it('throws the typed error when retries are off or exhausted', async () => {
    server().intercept = (req, res) => {
      if (req.method !== 'POST' || req.path !== '/v1/sandboxes') return false
      sendError(res, 503, 'pool_empty', 'no warm sandbox', undefined, 1)
      return true
    }
    const err = await Sandbox.create('python', { ...conn(), retry: false }).catch((e) => e)
    expect(err).toBeInstanceOf(PoolEmptyError)
    expect(err).toMatchObject({ retryAfter: 1, retryAfterMs: 1000 })
    expect(server().requestsTo('POST', '/v1/sandboxes')).toHaveLength(1)
  })

  it('does not retry quota_exceeded', async () => {
    server().intercept = (req, res) => {
      if (req.method !== 'POST' || req.path !== '/v1/sandboxes') return false
      sendError(res, 429, 'quota_exceeded', 'quota', undefined, 5)
      return true
    }
    const err = await Sandbox.create('python', conn()).catch((e) => e)
    expect(err).toBeInstanceOf(QuotaExceededError)
    expect(err.retryAfter).toBe(5)
    expect(server().requestsTo('POST', '/v1/sandboxes')).toHaveLength(1)
  })

  it('can be aborted', async () => {
    server().intercept = (req) => req.method === 'POST' // never answers
    const ac = new AbortController()
    const p = Sandbox.create('python', { ...conn(), signal: ac.signal })
    setTimeout(() => ac.abort(), 20)
    await expect(p).rejects.toMatchObject({ name: 'AbortError' })
  })

  it('times out per requestTimeoutMs', async () => {
    server().intercept = (req) => req.method === 'POST'
    const err = await Sandbox.create('python', { ...conn(), requestTimeoutMs: 30, retry: false }).catch((e) => e)
    expect(err).toBeInstanceOf(TimeoutError)
  })

  it('tolerates unknown fields and unknown enum values from a newer server', async () => {
    server().intercept = (req, res) => {
      if (req.method !== 'POST' || req.path !== '/v1/sandboxes') return false
      sendJson(res, 201, {
        id: 'sb-00000000000000aa',
        template_id: 'python',
        template_version: 9,
        state: 'hibernating',
        reason: 'some_new_reason',
        created_at: 'not a date',
        access_token: 'sbt_x',
        network: { mode: 'airgapped', allow_domains: ['a'], future: 1 },
        default_env: { A: 'b' },
        metadata: { a: 'b', weird: 5 },
      })
      return true
    }
    const sb = await Sandbox.create('python', conn())
    expect(sb.info).toEqual({
      sandboxId: 'sb-00000000000000aa',
      templateId: 'python',
      templateVersion: 9,
      state: 'hibernating',
      reason: 'some_new_reason',
      metadata: { a: 'b' },
      network: { mode: 'airgapped', allowDomains: ['a'] },
    })
  })

  it('never reveals credentials when a sandbox is logged or serialised', async () => {
    const sb = await create()
    const dump = inspect(sb, { depth: 10, showHidden: true }) + JSON.stringify(sb) + JSON.stringify({ sb }) + String(sb)
    expect(dump).not.toContain(API_KEY)
    expect(dump).not.toContain(sb.accessToken as string)
    expect(JSON.parse(JSON.stringify(sb))).toEqual({ sandboxId: sb.sandboxId })
  })
})

describe('Sandbox.connect', () => {
  it('with an API key looks the sandbox up and can do everything', async () => {
    const first = await create()
    const sb = await Sandbox.connect(first.sandboxId, conn())
    expect(sb.info?.state).toBe('running')
    expect((await sb.commands.run('echo via key')).stdout).toBe('via key\n')
    expect(server().requestsTo('POST', '/commands').at(-1)?.headers['x-api-key']).toBe(API_KEY)
    expect(await sb.getInfo()).toMatchObject({ sandboxId: first.sandboxId })
  })

  it('with only a token uses the data plane and never sends an API key', async () => {
    vi.stubEnv('SBX_API_KEY', '')
    const first = await create()
    const before = server().requests.length
    const sb = await Sandbox.connect(first.sandboxId, { baseUrl: server().url, accessToken: first.accessToken as string })
    expect((await sb.commands.run('echo via token')).stdout).toBe('via token\n')
    await sb.files.write('t', 'x')
    expect(await sb.isRunning()).toBe(true)
    for (const r of server().requests.slice(before)) {
      expect(r.headers['x-api-key']).toBeUndefined()
      expect(r.headers['x-access-token']).toBe(first.accessToken)
    }
    // Control-plane calls are refused locally with a clear message.
    await expect(sb.kill()).rejects.toMatchObject({ code: 'missing_credentials' })
    await expect(sb.setTimeout(1000)).rejects.toThrow(/API key/)
  })

  it('rejects an unknown id and another tenant’s sandbox with NotFoundError', async () => {
    const mine = await create()
    await expect(Sandbox.connect('sb-ffffffffffffffff', conn())).rejects.toBeInstanceOf(NotFoundError)
    await expect(Sandbox.connect(mine.sandboxId, { apiKey: OTHER_KEY, baseUrl: server().url })).rejects.toBeInstanceOf(NotFoundError)
  })

  it('rejects a wrong token with AuthenticationError', async () => {
    vi.stubEnv('SBX_API_KEY', '')
    const mine = await create()
    const p = Sandbox.connect(mine.sandboxId, { baseUrl: server().url, accessToken: `sbt_${'f'.repeat(64)}` })
    await expect(p).rejects.toBeInstanceOf(AuthenticationError)
  })
})

describe('Sandbox.list and static helpers', () => {
  it('lists sandboxes and passes the filters', async () => {
    const a = await create({ metadata: { user: 'u1', env: 'test' } })
    const b = await create({ metadata: { user: 'u2' } })
    await b.kill()
    expect((await Sandbox.list(conn())).map((s) => s.sandboxId)).toEqual([a.sandboxId])
    const all = await Sandbox.list({ ...conn(), state: 'all' })
    expect(all.map((s) => [s.sandboxId, s.state, s.reason])).toEqual([
      [b.sandboxId, 'destroyed', 'released'],
      [a.sandboxId, 'running', undefined],
    ])
    await Sandbox.list({
      ...conn(),
      state: ['running', 'destroyed'],
      templateId: 'python',
      metadata: { user: 'u1', env: 'test' },
      limit: 5,
    })
    const q = server().requestsTo('GET', '/v1/sandboxes').at(-1)?.query
    expect(q?.get('state')).toBe('running,destroyed')
    expect(q?.get('template_id')).toBe('python')
    expect(q?.getAll('metadata')).toEqual(['user=u1', 'env=test'])
    expect(q?.get('limit')).toBe('5')
    expect(await Sandbox.list({ apiKey: OTHER_KEY, baseUrl: server().url, state: 'all' })).toEqual([])
  })

  it('Sandbox.kill / Sandbox.getInfo / Sandbox.listTemplates work by id', async () => {
    const sb = await create()
    expect((await Sandbox.getInfo(sb.sandboxId, conn())).state).toBe('running')
    expect(await Sandbox.kill(sb.sandboxId, conn())).toBe(true)
    expect(await Sandbox.kill('sb-ffffffffffffffff', conn())).toBe(false)
    expect(await Sandbox.getInfo(sb.sandboxId, conn())).toMatchObject({ state: 'destroyed', reason: 'released' })
    const [t] = await Sandbox.listTemplates(conn())
    expect(t).toEqual({
      templateId: 'python',
      version: 3,
      image: 'docker.io/library/python:3.12-alpine',
      runtime: 'crun',
      sharedKernel: true,
      resources: { cpus: 1, memoryBytes: 268435456, pidsLimit: 256, workdirBytes: 67108864, tmpBytes: 33554432, nofileLimit: 1024 },
      network: 'allowlist',
      allowDomains: ['pypi.org', 'files.pythonhosted.org'],
      allowCidrs: [],
      idleTimeoutMs: 600000,
      maxLifetimeMs: 3600000,
    })
  })
})

describe('sandbox lifecycle methods', () => {
  it('kill() is idempotent and returns false once the sandbox is unknown', async () => {
    const sb = await create()
    expect(await sb.kill()).toBe(true)
    // Documented: true means "the server knows this sandbox and it is ended", also when an earlier kill ended it.
    expect(await sb.kill()).toBe(true)
    server().sandboxes.delete(sb.sandboxId)
    expect(await sb.kill()).toBe(false)
  })

  it('kill() accepts an asynchronous delete (202), and can wait until the sandbox is gone', async () => {
    server().asyncDelete = true
    const a = await create()
    expect(await a.kill()).toBe(true)
    expect((await a.getInfo()).state).toBe('recycling') // accepted, not finished
    const b = await create()
    expect(await b.kill({ waitUntilDestroyed: true })).toBe(true)
    expect(server().sandbox(b.sandboxId).state).toBe('destroyed')
    expect(server().requestsTo('GET', `/v1/sandboxes/${b.sandboxId}`).length).toBeGreaterThan(1)
  })

  it('kill({ waitUntilDestroyed }) times out with a TimeoutError', async () => {
    const sb = await create()
    server().intercept = (req, res) => {
      if (req.method !== 'DELETE') return false
      res.writeHead(202).end() // accepted, but the sandbox never goes away
      return true
    }
    const err = await sb.kill({ waitUntilDestroyed: true, waitTimeoutMs: 120 }).catch((e) => e)
    expect(err).toBeInstanceOf(TimeoutError)
    expect(err.code).toBe('kill_timeout')
  })

  it('setTimeout() posts timeout_ms and returns the updated sandbox', async () => {
    const sb = await create()
    const before = sb.info?.expiresAt?.getTime() ?? 0
    const info = await sb.setTimeout(10_000)
    expect(JSON.parse(server().requestsTo('POST', '/timeout')[0]?.body.toString() ?? '')).toEqual({ timeout_ms: 10000 })
    expect(info.expiresAt?.getTime()).toBeLessThan(before)
    expect(sb.info).toBe(info)
    await expect(sb.setTimeout(0)).rejects.toMatchObject({ code: 'invalid_timeout' })
    // Past the template's max lifetime: refused with the limit, never capped.
    const tooFar = await sb.setTimeout(2 * 3_600_000).catch((e) => e)
    expect(tooFar).toBeInstanceOf(ValidationError)
    expect(tooFar).toMatchObject({ code: 'invalid_timeout', details: { max_timeout_ms: 3_600_000 } })
    expect(sb.info).toBe(info)
    await sb.kill()
    const err = await sb.setTimeout(1000).catch((e) => e)
    expect(err).toBeInstanceOf(SandboxTerminatedError)
    expect(err).toMatchObject({ state: 'destroyed', reason: 'released' })
  })

  it('getInfo() and isRunning() follow the sandbox through its end', async () => {
    const sb = await create()
    expect(await sb.isRunning()).toBe(true)
    await sb.kill()
    expect(await sb.isRunning()).toBe(false)
    expect(await sb.getInfo()).toMatchObject({ state: 'destroyed', reason: 'released' })
    server().sandboxes.delete(sb.sandboxId)
    expect(await sb.isRunning()).toBe(false)
    await expect(sb.getInfo()).rejects.toBeInstanceOf(NotFoundError)
  })

  it('isRunning() with only a token is false once the sandbox has ended', async () => {
    vi.stubEnv('SBX_API_KEY', '')
    const owner = await create()
    const guest = await Sandbox.connect(owner.sandboxId, { baseUrl: server().url, accessToken: owner.accessToken as string })
    expect(await guest.isRunning()).toBe(true)
    server().terminate(owner.sandboxId, 'idle_timeout')
    expect(await guest.isRunning()).toBe(false)
  })

  it('updateNetwork() puts the policy in snake_case and returns the updated sandbox', async () => {
    const sb = await create()
    const info = await sb.updateNetwork({ mode: 'none' })
    expect(info.network).toEqual({ mode: 'none' })
    await sb.updateNetwork({ allowDomains: ['example.com'], denyCidrs: [] })
    expect(JSON.parse(server().requestsTo('PUT', '/network').at(-1)?.body.toString() ?? '')).toEqual({
      allow_domains: ['example.com'],
      deny_cidrs: [],
    })
    await expect(sb.updateNetwork({ mode: 'full' })).rejects.toMatchObject({ code: 'network_not_allowed', name: 'ValidationError' })
  })

  it('events() returns the log and filters by type', async () => {
    const sb = await create()
    await sb.commands.run('echo hi')
    const events = await sb.events()
    expect(events.map((e) => e.type)).toEqual(['claimed', 'transition', 'command_started'])
    expect(events[1]).toMatchObject({ id: 2, fromState: 'assigned', toState: 'running' })
    expect(events[1]?.at).toBeInstanceOf(Date)
    expect(events[2]?.data).toMatchObject({ cmd: '/bin/sh', timeout_ms: 0 })
    expect(events[0]?.data).toEqual({})
    const only = await sb.events({ type: 'command_started' })
    expect(only).toHaveLength(1)
    expect(server().requestsTo('GET', '/events').at(-1)?.query.get('type')).toBe('command_started')
  })

  it('getStats() returns the counters in camelCase', async () => {
    const sb = await create()
    expect(await sb.getStats()).toEqual({
      memoryBytes: 2_500_000,
      oomKills: 0,
      cpuUsageUsec: 1234,
      cpuThrottledPeriods: 1,
      cpuThrottledUsec: 2,
      pidsCurrent: 3,
      pidsLimitHits: 0,
      memoryHighHits: 0,
    })
    await sb.kill()
    await expect(sb.getStats()).rejects.toBeInstanceOf(SandboxTerminatedError)
  })

  it('`await using` kills the sandbox at the end of the scope', async () => {
    let id = ''
    {
      await using sb = await create()
      id = sb.sandboxId
      expect(server().sandbox(id).state).toBe('running')
    }
    expect(server().sandbox(id).state).toBe('destroyed')
    expect(typeof Sandbox.prototype[Symbol.asyncDispose]).toBe('function')
  })

  it('every error is a SandboxError with the request id', async () => {
    const err = await Sandbox.getInfo('sb-ffffffffffffffff', conn()).catch((e) => e)
    expect(err).toBeInstanceOf(SandboxError)
    expect(err.requestId).toMatch(/^req-\d+$/)
    expect(err.status).toBe(404)
  })

  it('data-plane calls on a terminated sandbox reject with its state and reason', async () => {
    const sb = await create()
    server().terminate(sb.sandboxId, 'oom_killed')
    const calls: (() => Promise<unknown>)[] = [
      () => sb.commands.run('echo x'),
      () => sb.commands.list(),
      () => sb.files.read('f'),
      () => sb.files.write('f', 'x'),
      () => sb.pty.create(),
    ]
    for (const call of calls) {
      const err = await call().catch((e) => e)
      expect(err).toBeInstanceOf(SandboxTerminatedError)
      expect(err).toMatchObject({ code: 'sandbox_not_running', state: 'destroyed', reason: 'oom_killed' })
    }
  })
})
