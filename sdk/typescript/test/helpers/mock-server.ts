/**
 * An in-process imitation of sbxd's REST API and WebSocket endpoint, close
 * enough to the real server (api/openapi.yaml, internal/api) to exercise
 * every SDK method, with hooks for injecting faults.
 */

import { EventEmitter } from 'node:events'
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http'
import type { AddressInfo, Socket } from 'node:net'
import { type WebSocket, WebSocketServer } from 'ws'

export const API_KEY = 'sbx_test_0123456789abcdef'
export const OTHER_KEY = 'sbx_other_fedcba9876543210'

type Channel = 'stdout' | 'stderr' | 'pty'

export class MockCommand extends EventEmitter {
  readonly chunks: { type: Channel; offset: number; data: Buffer }[] = []
  /** Offset of the oldest byte still replayable (the ring buffer's base). */
  base = 0
  next = 0
  exit: { exit_code: number; reason: string; signal?: string } | undefined
  stdin: Buffer[] = []
  stdinOpen: boolean
  signals: string[] = []
  size: { cols: number; rows: number } | undefined
  readonly startedAt = new Date().toISOString()

  constructor(
    readonly pid: string,
    readonly request: Record<string, unknown>,
  ) {
    super()
    this.setMaxListeners(50)
    this.stdinOpen = request.stdin === true || request.pty !== undefined
    this.size = request.pty as { cols: number; rows: number } | undefined
  }

  get isPty(): boolean {
    return this.request.pty !== undefined
  }

  write(type: Channel, data: string | Buffer): void {
    const buf = typeof data === 'string' ? Buffer.from(data) : data
    // Like the agent: one event carries at most 32768 bytes.
    for (let at = 0; at < buf.length; at += 32768) {
      const part = buf.subarray(at, at + 32768)
      this.chunks.push({ type: this.isPty ? 'pty' : type, offset: this.next, data: part })
      this.next += part.length
    }
    this.emit('update')
  }

  finish(exitCode: number, reason = 'exited', signal?: string): void {
    if (this.exit) return
    this.exit = { exit_code: exitCode, reason }
    if (signal) this.exit.signal = signal
    this.stdinOpen = false
    this.emit('update')
  }

  /** Forgets output before `offset`, as the agent's ring buffer does when it overflows. */
  dropBefore(offset: number): void {
    this.base = offset
  }

  view(): Record<string, unknown> {
    const v: Record<string, unknown> = {
      pid: this.pid,
      cmd: this.request.cmd,
      args: this.request.args ?? [],
      pty: this.isPty,
      started_at: this.startedAt,
      next_offset: this.next,
    }
    if (this.request.tag) v.tag = this.request.tag
    if (this.exit) v.exit = this.exit
    return v
  }

  /** Events from `from` on, as the stream route would send them. Returns the new cursor. */
  eventsFrom(from: number): { events: Record<string, unknown>[]; cursor: number } {
    const events: Record<string, unknown>[] = []
    let cursor = Math.min(from, this.next)
    if (cursor < this.base) {
      events.push({ type: 'truncated', offset: this.base, dropped: this.base - cursor })
      cursor = this.base
    }
    for (const c of this.chunks) {
      const end = c.offset + c.data.length
      if (end <= cursor) continue
      const data = c.offset < cursor ? c.data.subarray(cursor - c.offset) : c.data
      events.push({ type: c.type, offset: cursor, data: data.toString('base64') })
      cursor = end
    }
    return { events, cursor }
  }
}

export interface MockSandbox {
  id: string
  tenant: string
  token: string
  state: string
  reason?: string
  templateId: string
  createdAt: string
  expiresAt: string
  env: Record<string, string>
  metadata: Record<string, string>
  network?: Record<string, unknown>
  files: Map<string, { data: Buffer; mode: string }>
  dirs: Set<string>
  commands: Map<string, MockCommand>
}

export interface RecordedRequest {
  method: string
  path: string
  query: URLSearchParams
  headers: IncomingMessage['headers']
  body: Buffer
}

export interface WsSession {
  ws: WebSocket
  command: MockCommand
  from: number
  /** Offsets acknowledged on channel 6, in order. */
  acks: number[]
  /** Every client frame: channel and payload. */
  frames: { channel: number; payload: Buffer }[]
  protocols: string[]
  headers: IncomingMessage['headers']
}

type Interceptor = (req: RecordedRequest, res: ServerResponse) => boolean | undefined

export class MockServer {
  readonly sandboxes = new Map<string, MockSandbox>()
  readonly requests: RecordedRequest[] = []
  readonly wsSessions: WsSession[] = []
  /** Called before the default handling; return true when it answered. */
  intercept: Interceptor | undefined
  /** How a command behaves once started. Default: the built-in programs. */
  program: (cmd: MockCommand, sandbox: MockSandbox) => void = builtinPrograms
  /** Size of one NDJSON flush, to split lines across network chunks. 0: whole lines. */
  chunkBytes = 0
  /** What DELETE /v1/sandboxes/{id} answers: 204 synchronously, or 202 and a later state change. */
  asyncDelete = false
  /**
   * Behave like a server from before the close-out changes: stream headers
   * only with the first event, no stat route, mkdir always 204, refused
   * WebSocket handshakes as HTTP errors only, and a bare 1001 close when
   * the sandbox goes away.
   */
  legacy = false
  /** Send application close codes without the status frame before them (a client that only sees the close). */
  closeWithoutStatusFrame = false
  url = ''
  wsCount = 0
  #server: Server
  #wss: WebSocketServer
  #sockets = new Set<Socket>()
  #streams = new Set<ServerResponse>()
  #seq = 0

  constructor() {
    this.#server = createServer((req, res) => void this.#handle(req, res))
    this.#wss = new WebSocketServer({
      noServer: true,
      maxPayload: (1 << 20) + 1,
      handleProtocols: (protocols) => (protocols.has('sbx.v1') ? 'sbx.v1' : false),
    })
    this.#server.on('connection', (s) => {
      this.#sockets.add(s)
      s.on('close', () => this.#sockets.delete(s))
    })
    this.#server.on('upgrade', (req, socket, head) => this.#upgrade(req, socket as Socket, head))
  }

  async start(): Promise<this> {
    await new Promise<void>((resolve) => this.#server.listen(0, '127.0.0.1', resolve))
    this.url = `http://127.0.0.1:${(this.#server.address() as AddressInfo).port}`
    return this
  }

  async stop(): Promise<void> {
    for (const s of this.wsSessions) s.ws.terminate()
    for (const s of this.#sockets) s.destroy()
    await new Promise<void>((resolve) => this.#server.close(() => resolve()))
  }

  /** Cuts every open NDJSON stream at the TCP level, as a proxy would. */
  cutStreams(): number {
    const n = this.#streams.size
    for (const res of this.#streams) res.socket?.destroy()
    this.#streams.clear()
    return n
  }

  /** Drops every WebSocket without a close frame. */
  cutSockets(): void {
    for (const s of this.wsSessions) s.ws.terminate()
  }

  get openStreams(): number {
    return this.#streams.size
  }

  sandbox(id: string): MockSandbox {
    const sb = this.sandboxes.get(id)
    if (!sb) throw new Error(`no mock sandbox ${id}`)
    return sb
  }

  /** The most recently started command of a sandbox. */
  lastCommand(sandboxId: string): MockCommand {
    const cmds = [...this.sandbox(sandboxId).commands.values()]
    const last = cmds[cmds.length - 1]
    if (!last) throw new Error('no command started')
    return last
  }

  requestsTo(method: string, pathSuffix: string): RecordedRequest[] {
    return this.requests.filter((r) => r.method === method && r.path.endsWith(pathSuffix))
  }

  terminate(id: string, reason: string): void {
    const sb = this.sandbox(id)
    sb.state = 'destroyed'
    sb.reason = reason
  }

  // --- plumbing ------------------------------------------------------------

  #id(prefix: string): string {
    this.#seq++
    return `${prefix}${this.#seq.toString(16).padStart(16, '0')}`
  }

  async #handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const url = new URL(req.url ?? '/', 'http://mock')
    const chunks: Buffer[] = []
    for await (const c of req) chunks.push(c as Buffer)
    const rec: RecordedRequest = {
      method: req.method ?? 'GET',
      path: url.pathname,
      query: url.searchParams,
      headers: req.headers,
      body: Buffer.concat(chunks),
    }
    this.requests.push(rec)
    res.setHeader('X-Request-Id', `req-${this.requests.length}`)
    try {
      if (this.intercept?.(rec, res)) return
      this.#route(rec, res)
    } catch (e) {
      if (e instanceof HttpError) {
        sendError(res, e.status, e.code, e.message, e.details, e.retryAfter)
      } else {
        sendError(res, 500, 'internal', String(e))
      }
    }
  }

  #tenant(rec: RecordedRequest): string | undefined {
    const bearer = /^Bearer\s+(.+)$/i.exec(String(rec.headers.authorization ?? ''))?.[1]
    const key = String(rec.headers['x-api-key'] ?? (bearer && !bearer.startsWith('sbt_') ? bearer : ''))
    if (key === API_KEY) return 'acme'
    if (key === OTHER_KEY) return 'other'
    return undefined
  }

  #token(rec: RecordedRequest): string {
    const bearer = /^Bearer\s+(.+)$/i.exec(String(rec.headers.authorization ?? ''))?.[1]
    const fromProtocol = String(rec.headers['sec-websocket-protocol'] ?? '')
      .split(',')
      .map((p) => p.trim())
      .find((p) => p.startsWith('sbx.token.'))
      ?.slice('sbx.token.'.length)
    return String(
      rec.headers['x-access-token'] ?? (bearer?.startsWith('sbt_') ? bearer : undefined) ?? rec.query.get('token') ?? fromProtocol ?? '',
    )
  }

  #owned(rec: RecordedRequest, id: string): MockSandbox {
    const tenant = this.#tenant(rec)
    if (!tenant) throw new HttpError(401, 'unauthorized', 'missing or invalid credentials')
    const sb = this.sandboxes.get(id)
    if (!sb || sb.tenant !== tenant) throw new HttpError(404, 'not_found', `sandbox "${id}" not found`)
    return sb
  }

  /** Data-plane authorisation: API key or the sandbox's token; the sandbox must be running. */
  authorizeData(rec: RecordedRequest, id: string): MockSandbox {
    let sb: MockSandbox | undefined
    if (this.#tenant(rec)) {
      sb = this.#owned(rec, id)
    } else {
      const token = this.#token(rec)
      sb = this.sandboxes.get(id)
      if (!token || !sb || sb.token !== token) throw new HttpError(401, 'unauthorized', 'missing or invalid credentials')
    }
    if (sb.state !== 'running') throw notRunning(sb)
    return sb
  }

  #route(rec: RecordedRequest, res: ServerResponse): void {
    const { method, path } = rec
    const parts = path.split('/').filter(Boolean) // v1, sandboxes, id, ...
    if (path === '/v1/templates' && method === 'GET') {
      if (!this.#tenant(rec)) throw new HttpError(401, 'unauthorized', 'missing or invalid credentials')
      return sendJson(res, 200, {
        templates: [
          {
            id: 'python',
            version: 3,
            image: 'docker.io/library/python:3.12-alpine',
            runtime: 'crun',
            shared_kernel: true,
            resources: {
              cpus: 1,
              memory_bytes: 268435456,
              pids_limit: 256,
              workdir_bytes: 67108864,
              tmp_bytes: 33554432,
              nofile_limit: 1024,
            },
            network: 'allowlist',
            allow_domains: ['pypi.org', 'files.pythonhosted.org'],
            allow_cidrs: [],
            idle_timeout_ms: 600000,
            max_lifetime_ms: 3600000,
            some_future_field: true,
          },
        ],
      })
    }
    if (parts[0] !== 'v1' || parts[1] !== 'sandboxes') throw new HttpError(404, 'not_found', `no such route: ${method} ${path}`)

    if (parts.length === 2) {
      const tenant = this.#tenant(rec)
      if (!tenant) throw new HttpError(401, 'unauthorized', 'missing or invalid credentials')
      if (method === 'POST') return this.#create(rec, res, tenant)
      if (method === 'GET') {
        const states = rec.query.get('state')
        const wanted = states === 'all' ? undefined : (states ?? 'assigned,running').split(',')
        const meta = rec.query.getAll('metadata').map((p) => p.split('=') as [string, string])
        const out = [...this.sandboxes.values()]
          .filter((s) => s.tenant === tenant)
          .filter((s) => !wanted || wanted.includes(s.state))
          .filter((s) => !rec.query.get('template_id') || s.templateId === rec.query.get('template_id'))
          .filter((s) => meta.every(([k, v]) => s.metadata[k] === v))
          .reverse()
          .slice(0, Number(rec.query.get('limit') ?? 100))
        return sendJson(res, 200, { sandboxes: out.map(sandboxView) })
      }
    }
    const id = parts[2] ?? ''
    if (parts.length === 3) {
      const sb = this.#owned(rec, id)
      if (method === 'GET') return sendJson(res, 200, sandboxView(sb))
      if (method === 'DELETE') {
        if (this.asyncDelete) {
          sb.state = 'recycling'
          setTimeout(() => {
            sb.state = 'destroyed'
            sb.reason = 'released'
          }, 120)
          res.writeHead(202).end()
          return
        }
        sb.state = 'destroyed'
        sb.reason = 'released'
        for (const c of sb.commands.values()) c.emit('gone')
        res.writeHead(204).end()
        return
      }
    }
    const section = parts[3]
    if (section === 'timeout' && method === 'POST') {
      const sb = this.#owned(rec, id)
      const body = parseBody(rec)
      if (typeof body.timeout_ms !== 'number' || body.timeout_ms <= 0) {
        throw new HttpError(400, 'invalid_timeout', 'timeout_ms must be positive')
      }
      if (sb.state !== 'running') throw notRunning(sb)
      // As the server: past the template's max lifetime (1 h here) is refused, not capped.
      if (body.timeout_ms > 3_600_000) {
        throw new HttpError(400, 'invalid_timeout', "timeout_ms would end the sandbox after the template's max lifetime", {
          max_timeout_ms: 3_600_000,
          max_lifetime_ms: 3_600_000,
        })
      }
      sb.expiresAt = new Date(Date.now() + body.timeout_ms).toISOString()
      return sendJson(res, 200, sandboxView(sb))
    }
    if (section === 'network' && method === 'PUT') {
      const sb = this.#owned(rec, id)
      const body = parseBody(rec)
      if (body.mode === 'full') throw new HttpError(400, 'network_not_allowed', 'the template does not allow the requested network mode')
      sb.network = body
      return sendJson(res, 200, sandboxView(sb))
    }
    if (section === 'events' && method === 'GET') {
      const sb = this.#owned(rec, id)
      const all = [
        { id: 1, type: 'claimed', to_state: 'assigned', at: sb.createdAt },
        { id: 2, type: 'transition', from_state: 'assigned', to_state: 'running', at: sb.createdAt },
        ...[...sb.commands.values()].map((c, i) => ({
          id: 3 + i,
          type: 'command_started',
          at: c.startedAt,
          data: { pid: c.pid, cmd: c.request.cmd, timeout_ms: 0 },
        })),
      ]
      const want = rec.query.get('type')
      return sendJson(res, 200, { events: all.filter((e) => !want || e.type === want) })
    }
    if (section === 'stats' && method === 'GET') {
      const sb = this.#owned(rec, id)
      if (sb.state !== 'running') throw notRunning(sb)
      return sendJson(res, 200, {
        memory_bytes: 2_500_000,
        oom_kills: 0,
        cpu_usage_usec: 1234,
        cpu_throttled_periods: 1,
        cpu_throttled_usec: 2,
        pids_current: 3,
        pids_limit_hits: 0,
        memory_high_hits: 0,
      })
    }
    if (section === 'commands') return this.#commands(rec, res, id, parts.slice(4))
    if (section === 'files') return this.#files(rec, res, id, parts[4])
    throw new HttpError(404, 'not_found', `no such route: ${method} ${path}`)
  }

  #create(rec: RecordedRequest, res: ServerResponse, tenant: string): void {
    const body = parseBody(rec)
    const allowed = new Set(['template_id', 'timeout_ms', 'env', 'metadata', 'network'])
    for (const k of Object.keys(body)) {
      if (!allowed.has(k)) throw new HttpError(400, 'bad_request', `invalid JSON body: json: unknown field "${k}"`)
    }
    if (body.template_id !== 'python') throw new HttpError(404, 'template_not_found', `template "${String(body.template_id)}" not found`)
    const timeout = typeof body.timeout_ms === 'number' ? body.timeout_ms : 3_600_000
    if (timeout > 3_600_000) {
      throw new HttpError(400, 'invalid_timeout', "timeout_ms exceeds the template's max lifetime of 3600000 ms", {
        max_timeout_ms: 3_600_000,
      })
    }
    const now = new Date()
    const sb: MockSandbox = {
      id: this.#id('sb-'),
      tenant,
      token: `sbt_${'ab'.repeat(32 - 2)}${this.#seq.toString(16).padStart(4, '0')}`,
      state: 'running',
      templateId: 'python',
      createdAt: now.toISOString(),
      expiresAt: new Date(now.getTime() + timeout).toISOString(),
      env: (body.env as Record<string, string>) ?? {},
      metadata: (body.metadata as Record<string, string>) ?? {},
      files: new Map(),
      dirs: new Set(['/workspace']),
      commands: new Map(),
    }
    if (body.network) sb.network = body.network as Record<string, unknown>
    this.sandboxes.set(sb.id, sb)
    res.setHeader('Location', `/v1/sandboxes/${sb.id}`)
    sendJson(res, 201, { ...sandboxView(sb), access_token: sb.token })
  }

  #command(sb: MockSandbox, pid: string | undefined): MockCommand {
    const c = sb.commands.get(pid ?? '')
    if (!c) throw new HttpError(404, 'process_not_found', 'process not found')
    return c
  }

  #commands(rec: RecordedRequest, res: ServerResponse, id: string, rest: string[]): void {
    const sb = this.authorizeData(rec, id)
    const { method } = rec
    if (rest.length === 0 && method === 'GET') return sendJson(res, 200, { commands: [...sb.commands.values()].map((c) => c.view()) })
    if (rest.length === 0 && method === 'POST') {
      const body = parseBody(rec)
      const allowed = new Set(['cmd', 'args', 'env', 'cwd', 'timeout_ms', 'stdin', 'tag', 'pty'])
      for (const k of Object.keys(body)) {
        if (!allowed.has(k)) throw new HttpError(400, 'bad_request', `invalid JSON body: json: unknown field "${k}"`)
      }
      if (typeof body.cmd !== 'string' || body.cmd === '') throw new HttpError(400, 'bad_request', 'cmd is required')
      if (body.cmd === 'nosuch') throw new HttpError(400, 'command_not_found', 'executable file not found in $PATH')
      if (body.tag && [...sb.commands.values()].some((c) => !c.exit && c.request.tag === body.tag)) {
        throw new HttpError(409, 'tag_in_use', 'tag is in use')
      }
      const cmd = new MockCommand(this.#id('p'), body)
      sb.commands.set(cmd.pid, cmd)
      const wait = rec.query.get('wait')
      if (wait === 'stream') {
        this.#stream(res, cmd, 0, true)
        this.program(cmd, sb)
        return
      }
      this.program(cmd, sb)
      if (wait === 'true') {
        const done = (): void => {
          if (!cmd.exit) return
          cmd.off('update', done)
          const out = (t: Channel): string =>
            Buffer.concat(
              cmd.chunks.filter((c) => (t === 'stdout' ? c.type !== 'stderr' : c.type === 'stderr')).map((c) => c.data),
            ).toString('base64')
          sendJson(res, 200, { ...cmd.view(), stdout: out('stdout'), stderr: out('stderr'), truncated: false })
        }
        cmd.on('update', done)
        done()
        return
      }
      res.setHeader('Location', `${rec.path}/${cmd.pid}`)
      return sendJson(res, 201, cmd.view())
    }
    const cmd = this.#command(sb, rest[0])
    const action = rest[1]
    if (action === undefined && method === 'GET') return sendJson(res, 200, cmd.view())
    if (action === 'stream' && method === 'GET') {
      const from = Number(rec.query.get('from') ?? 0)
      if (!Number.isInteger(from) || from < 0) throw new HttpError(400, 'bad_request', 'from must be a non-negative integer')
      return this.#stream(res, cmd, from, false)
    }
    if (action === 'stdin' && method === 'POST') {
      if (cmd.exit) throw new HttpError(409, 'process_exited', 'process has exited')
      if (!cmd.stdinOpen) throw new HttpError(409, 'stdin_closed', 'stdin is not open (start the process with stdin=true or a pty)')
      cmd.stdin.push(rec.body)
      cmd.emit('stdin', rec.body)
      res.writeHead(204).end()
      return
    }
    if (action === 'close-stdin' && method === 'POST') {
      if (cmd.isPty) throw new HttpError(409, 'is_a_pty', 'close-stdin applies to pipe mode only; send Ctrl+D (0x04) to a pty')
      if (cmd.exit) throw new HttpError(409, 'process_exited', 'process has exited')
      if (!cmd.stdinOpen) throw new HttpError(409, 'stdin_closed', 'stdin is not open')
      cmd.stdinOpen = false
      cmd.emit('stdin-closed')
      res.writeHead(204).end()
      return
    }
    if (action === 'signal' && method === 'POST') {
      const body = parseBody(rec)
      if (!['SIGINT', 'SIGTERM', 'SIGKILL', 'SIGHUP', 'SIGQUIT'].includes(String(body.signal))) {
        throw new HttpError(400, 'bad_signal', `unknown signal ${String(body.signal)}`)
      }
      if (cmd.exit) throw new HttpError(409, 'process_exited', 'process has exited')
      cmd.signals.push(String(body.signal))
      cmd.emit('signal', body.signal)
      res.writeHead(204).end()
      return
    }
    if (action === 'resize' && method === 'POST') {
      const body = parseBody(rec)
      if (!cmd.isPty) throw new HttpError(409, 'not_a_pty', 'process does not run on a pty')
      cmd.size = { cols: Number(body.cols), rows: Number(body.rows) }
      cmd.emit('resize', cmd.size)
      res.writeHead(204).end()
      return
    }
    throw new HttpError(404, 'not_found', `no such route: ${method} ${rec.path}`)
  }

  /** The NDJSON stream of a command: replay from `from`, then live, then exit. */
  #stream(res: ServerResponse, cmd: MockCommand, from: number, withStart: boolean): void {
    let cursor = from
    let begun = false
    const begin = (): void => {
      if (begun) return
      begun = true
      res.writeHead(200, { 'Content-Type': 'application/x-ndjson', 'Cache-Control': 'no-store' })
      res.flushHeaders()
      this.#streams.add(res)
    }
    // The real server sends the headers at once; the legacy one with the first event.
    if (!this.legacy) begin()
    const write = (events: Record<string, unknown>[]): void => {
      if (events.length === 0) return
      begin()
      const text = Buffer.from(events.map((e) => `${JSON.stringify(e)}\n`).join(''))
      if (this.chunkBytes > 0) {
        for (let at = 0; at < text.length; at += this.chunkBytes) res.write(text.subarray(at, at + this.chunkBytes))
      } else {
        res.write(text)
      }
    }
    const end = (): void => {
      cmd.off('update', pump)
      cmd.off('gone', gone)
      cmd.off('keepalive', keepalive)
      cmd.off('stream-error', streamError)
      this.#streams.delete(res)
      res.end()
    }
    const pump = (): void => {
      if (res.destroyed) return end()
      const r = cmd.eventsFrom(cursor)
      cursor = r.cursor
      write(r.events)
      if (cmd.exit) {
        write([{ type: 'exit', offset: cursor, exit: cmd.exit }])
        end()
      }
    }
    const gone = (): void => {
      write([
        {
          type: 'error',
          offset: 0,
          error: {
            code: 'sandbox_terminated',
            message: 'sandbox has terminated: released',
            details: { state: 'destroyed', reason: 'released' },
          },
        },
      ])
      end()
    }
    const keepalive = (): void => write([{ type: 'keepalive', offset: cursor }])
    const streamError = (error: Record<string, unknown>): void => {
      write([{ type: 'error', offset: 0, error }])
      end()
    }
    res.on('close', () => {
      cmd.off('update', pump)
      cmd.off('gone', gone)
      cmd.off('keepalive', keepalive)
      cmd.off('stream-error', streamError)
      this.#streams.delete(res)
    })
    cmd.on('update', pump)
    cmd.on('gone', gone)
    cmd.on('keepalive', keepalive)
    cmd.on('stream-error', streamError)
    if (withStart) write([{ type: 'start', offset: 0, command: cmd.view() }])
    pump()
  }

  #files(rec: RecordedRequest, res: ServerResponse, id: string, action: string | undefined): void {
    const sb = this.authorizeData(rec, id)
    const { method } = rec
    const raw = rec.query.get('path')
    const abs = (p: string): string => (p.startsWith('/') ? p : `/workspace/${p}`).replace(/\/+$/, '').replace(/\/\.$/, '') || '/'
    if (action === 'list' && method === 'GET') {
      const dir = abs(raw ?? '.')
      if (!sb.dirs.has(dir)) throw new HttpError(404, 'file_not_found', 'no such file or directory')
      const entries: Record<string, unknown>[] = []
      const child = (p: string): string | undefined => {
        if (!p.startsWith(`${dir}/`)) return undefined
        const rest = p.slice(dir.length + 1)
        return rest.includes('/') ? undefined : rest
      }
      for (const d of sb.dirs) {
        const name = child(d)
        if (name) entries.push({ name, path: d, size: 0, mode: '0755', is_dir: true, mod_time: '2026-10-10T00:00:00Z' })
      }
      for (const [p, f] of sb.files) {
        const name = child(p)
        if (name)
          entries.push({ name, path: p, size: f.data.length, mode: f.mode, is_dir: false, mod_time: '2026-10-10T00:00:00.123456789Z' })
      }
      entries.sort((a, b) => String(a.name).localeCompare(String(b.name)))
      return sendJson(res, 200, { files: entries })
    }
    if (!raw) throw new HttpError(400, 'bad_request', 'the path query parameter is required')
    const path = abs(raw)
    if (action === 'mkdir' && method === 'POST') {
      if (sb.files.has(path)) throw new HttpError(409, 'conflict', 'mkdir: file exists')
      const created = !sb.dirs.has(path)
      let p = ''
      for (const seg of path.split('/').filter(Boolean)) {
        p += `/${seg}`
        sb.dirs.add(p)
      }
      res.writeHead(created && !this.legacy ? 201 : 204).end()
      return
    }
    if (action === 'stat' && method === 'GET' && !this.legacy) {
      const name = path.slice(path.lastIndexOf('/') + 1)
      if (sb.dirs.has(path)) {
        return sendJson(res, 200, { name, path, size: 0, mode: '0755', is_dir: true, mod_time: '2026-10-10T00:00:00Z' })
      }
      const f = sb.files.get(path)
      if (!f) throw new HttpError(404, 'file_not_found', 'no such file or directory')
      return sendJson(res, 200, {
        name,
        path,
        size: f.data.length,
        mode: f.mode,
        is_dir: false,
        mod_time: '2026-10-10T00:00:00.123456789Z',
      })
    }
    if (action !== undefined) throw new HttpError(404, 'not_found', `no such route: ${method} ${rec.path}`)
    if (method === 'GET') {
      if (sb.dirs.has(path)) throw new HttpError(409, 'is_directory', 'path is a directory')
      const f = sb.files.get(path)
      if (!f) throw new HttpError(404, 'file_not_found', 'no such file or directory')
      res.writeHead(200, { 'Content-Type': 'application/octet-stream', 'Content-Length': String(f.data.length) })
      res.end(f.data)
      return
    }
    if (method === 'PUT') {
      if (sb.dirs.has(path)) throw new HttpError(409, 'is_directory', 'path is a directory')
      const mode = rec.query.get('mode') ?? '0644'
      if (!/^[0-7]{3,4}$/.test(mode)) throw new HttpError(400, 'bad_request', 'mode must be octal permission bits, e.g. 0644')
      let p = ''
      for (const seg of path.split('/').filter(Boolean).slice(0, -1)) {
        p += `/${seg}`
        sb.dirs.add(p)
      }
      sb.files.set(path, { data: rec.body, mode: mode.padStart(4, '0') })
      res.writeHead(204).end()
      return
    }
    if (method === 'DELETE') {
      const had = sb.files.delete(path) || sb.dirs.has(path)
      if (!had) throw new HttpError(404, 'file_not_found', 'no such file or directory')
      for (const d of [...sb.dirs]) if (d === path || d.startsWith(`${path}/`)) sb.dirs.delete(d)
      for (const f of [...sb.files.keys()]) if (f.startsWith(`${path}/`)) sb.files.delete(f)
      res.writeHead(204).end()
      return
    }
    throw new HttpError(405, 'method_not_allowed', `method ${method} is not allowed on ${rec.path}`)
  }

  #upgrade(req: IncomingMessage, socket: Socket, head: Buffer): void {
    const url = new URL(req.url ?? '/', 'http://mock')
    const rec: RecordedRequest = { method: 'GET', path: url.pathname, query: url.searchParams, headers: req.headers, body: Buffer.alloc(0) }
    this.requests.push(rec)
    const refuse = (e: HttpError): void => {
      const body = JSON.stringify({ error: { code: e.code, message: e.message, details: e.details } })
      socket.end(
        `HTTP/1.1 ${e.status} Error\r\nContent-Type: application/json\r\nContent-Length: ${Buffer.byteLength(body)}\r\nConnection: close\r\n\r\n${body}`,
      )
    }
    let cmd: MockCommand
    try {
      const m = /^\/v1\/sandboxes\/([^/]+)\/commands\/([^/]+)\/ws$/.exec(url.pathname)
      if (!m) throw new HttpError(404, 'not_found', 'no such route')
      const sb = this.authorizeData(rec, decodeURIComponent(m[1] ?? ''))
      cmd = this.#command(sb, decodeURIComponent(m[2] ?? ''))
    } catch (e) {
      const err = e instanceof HttpError ? e : new HttpError(500, 'internal', String(e))
      if (!this.legacy && url.searchParams.get('handshake_errors') === 'close') {
        // Accept, say why on the status channel, close with the application code.
        const codes: Record<string, number> = {
          sandbox_terminated: 4000,
          sandbox_not_running: 4000,
          unauthorized: 4001,
          shutting_down: 4002,
          agent_unavailable: 4003,
          not_found: 4004,
          process_not_found: 4004,
          bad_request: 4005,
          rate_limited: 4029,
        }
        this.#wss.handleUpgrade(req, socket, head, (ws) => {
          if (!this.closeWithoutStatusFrame) {
            ws.send(Buffer.concat([Buffer.from([3]), Buffer.from(JSON.stringify({ error: { code: err.code, message: err.message } }))]))
          }
          ws.close(codes[err.code] ?? 4099, err.code)
        })
        return
      }
      refuse(err)
      return
    }
    this.wsCount++
    this.#wss.handleUpgrade(req, socket, head, (ws) => this.#attach(ws, cmd, Number(url.searchParams.get('from') ?? 0), req))
  }

  #attach(ws: WebSocket, cmd: MockCommand, from: number, req: IncomingMessage): void {
    const session: WsSession = {
      ws,
      command: cmd,
      from,
      acks: [],
      frames: [],
      protocols: String(req.headers['sec-websocket-protocol'] ?? '')
        .split(',')
        .map((p) => p.trim()),
      headers: req.headers,
    }
    this.wsSessions.push(session)
    const sendJsonFrame = (ch: number, v: unknown): void => ws.send(Buffer.concat([Buffer.from([ch]), Buffer.from(JSON.stringify(v))]))
    let cursor = Math.min(from, cmd.next)
    let dropped = 0
    if (cursor < cmd.base) {
      dropped = cmd.base - cursor
      cursor = cmd.base
    }
    const offsetFrame: Record<string, number> = { offset: cursor }
    if (dropped > 0) offsetFrame.dropped = dropped
    sendJsonFrame(9, offsetFrame)

    const pump = (): void => {
      if (ws.readyState !== ws.OPEN) return
      for (const c of cmd.chunks) {
        const end = c.offset + c.data.length
        if (end <= cursor) continue
        const data = c.offset < cursor ? c.data.subarray(cursor - c.offset) : c.data
        ws.send(Buffer.concat([Buffer.from([c.type === 'stderr' ? 2 : 1]), data]))
        cursor = end
      }
      if (cmd.exit) {
        cmd.off('update', pump)
        sendJsonFrame(3, { exit: cmd.exit })
        ws.close(1000)
      }
    }
    const ping = (payload: Buffer): void => ws.send(Buffer.concat([Buffer.from([7]), payload]))
    const gone = (): void => {
      if (this.legacy) return ws.close(1001, 'sandbox terminated')
      if (!this.closeWithoutStatusFrame) {
        const sb = [...this.sandboxes.values()].find((s) => s.commands.get(cmd.pid) === cmd)
        const details = { state: sb?.state ?? 'destroyed', reason: sb?.reason ?? 'released' }
        sendJsonFrame(3, { error: { code: 'sandbox_terminated', message: `sandbox has terminated: ${details.reason}`, details } })
      }
      ws.close(4000, 'sandbox_terminated')
    }
    const closeWith = (code: number, reason: string): void => ws.close(code, reason)
    cmd.on('ws-close', closeWith)
    cmd.on('update', pump)
    cmd.on('ws-ping', ping)
    cmd.on('gone', gone)
    ws.on('close', () => {
      cmd.off('update', pump)
      cmd.off('ws-ping', ping)
      cmd.off('gone', gone)
      cmd.off('ws-close', closeWith)
    })
    ws.on('message', (data, isBinary) => {
      const buf = data as Buffer
      if (!isBinary || buf.length === 0) {
        sendJsonFrame(3, { error: { code: 'bad_frame', message: 'frames must be binary with a one-byte channel prefix' } })
        ws.close(1000)
        return
      }
      const channel = buf[0] as number
      const payload = buf.subarray(1)
      session.frames.push({ channel, payload: Buffer.from(payload) })
      switch (channel) {
        case 0:
          cmd.stdin.push(Buffer.from(payload))
          cmd.emit('stdin', Buffer.from(payload))
          break
        case 4:
          cmd.size = JSON.parse(payload.toString()) as { cols: number; rows: number }
          cmd.emit('resize', cmd.size)
          break
        case 5: {
          const sig = (JSON.parse(payload.toString()) as { signal: string }).signal
          cmd.signals.push(sig)
          cmd.emit('signal', sig)
          break
        }
        case 6:
          session.acks.push((JSON.parse(payload.toString()) as { offset: number }).offset)
          break
        case 7:
          ws.send(Buffer.concat([Buffer.from([8]), payload]))
          break
        case 8:
          cmd.emit('ws-pong', Buffer.from(payload))
          break
      }
    })
    pump()
  }
}

/** The programs the mock sandbox can "run", chosen by the command line. */
export function builtinPrograms(cmd: MockCommand, sandbox: MockSandbox): void {
  const req = cmd.request
  const args = (req.args as string[] | undefined) ?? []
  // `/bin/sh -c "<line>"` runs the line; anything else runs cmd + args.
  const argv = req.cmd === '/bin/sh' && args[0] === '-c' ? String(args[1] ?? '').split(' ') : [String(req.cmd), ...args]
  const name = argv[0]
  const rest = argv.slice(1)
  cmd.on('signal', (sig: string) => {
    if (sig === 'SIGKILL') cmd.finish(137, 'killed', 'SIGKILL')
    else if (sig === 'SIGTERM') cmd.finish(143, 'killed', 'SIGTERM')
    else if (sig === 'SIGINT') cmd.finish(130, 'killed', 'SIGINT')
  })
  switch (name) {
    case 'echo':
      cmd.write('stdout', `${rest.join(' ')}\n`)
      cmd.finish(0)
      break
    case 'env':
      cmd.write('stdout', `${JSON.stringify({ ...sandbox.env, ...((req.env as Record<string, string>) ?? {}) })}\n`)
      cmd.finish(0)
      break
    case 'fail':
      cmd.write('stdout', 'partial\n')
      cmd.write('stderr', 'boom\n')
      cmd.finish(Number(rest[0] ?? 3))
      break
    case 'cat': {
      cmd.on('stdin', (data: Buffer) => cmd.write('stdout', data))
      cmd.on('stdin-closed', () => cmd.finish(0))
      break
    }
    case 'timeout':
      setTimeout(() => cmd.finish(137, 'timeout', 'SIGKILL'), 20)
      break
    case 'hang':
    case 'script':
      break // the test drives it
    default:
      if (cmd.isPty) {
        // A tiny terminal: echoes input, Ctrl+D exits, Ctrl+C prints ^C.
        cmd.on('stdin', (data: Buffer) => {
          if (data.includes(0x04)) cmd.finish(0)
          else if (data.includes(0x03)) cmd.write('pty', '^C\r\n')
          else cmd.write('pty', data)
        })
        break
      }
      cmd.finish(0)
  }
}

class HttpError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly details?: Record<string, unknown>,
    readonly retryAfter?: number,
  ) {
    super(message)
  }
}

function notRunning(sb: MockSandbox): HttpError {
  const details: Record<string, unknown> = { state: sb.state }
  if (sb.reason) details.reason = sb.reason
  return new HttpError(409, 'sandbox_not_running', `sandbox ${sb.id} is ${sb.state}${sb.reason ? ` (${sb.reason})` : ''}`, details)
}

export function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', ...headers })
  res.end(`${JSON.stringify(body)}\n`)
}

export function sendError(
  res: ServerResponse,
  status: number,
  code: string,
  message: string,
  details?: Record<string, unknown>,
  retryAfter?: number | string,
): void {
  const headers: Record<string, string> = {}
  if (retryAfter !== undefined) headers['Retry-After'] = String(retryAfter)
  if (status === 401) headers['WWW-Authenticate'] = 'Bearer realm="sbx"'
  const error: Record<string, unknown> = { code, message, request_id: String(res.getHeader('X-Request-Id') ?? '') }
  if (details) error.details = details
  sendJson(res, status, { error }, headers)
}

function parseBody(rec: RecordedRequest): Record<string, unknown> {
  try {
    const v = JSON.parse(rec.body.toString() || 'null') as unknown
    if (typeof v !== 'object' || v === null) throw new Error('not an object')
    return v as Record<string, unknown>
  } catch {
    throw new HttpError(400, 'bad_request', 'a JSON request body is required')
  }
}

function sandboxView(sb: MockSandbox): Record<string, unknown> {
  const v: Record<string, unknown> = {
    id: sb.id,
    template_id: sb.templateId,
    template_version: 3,
    state: sb.state,
    created_at: sb.createdAt,
    started_at: sb.createdAt,
    last_activity_at: sb.createdAt,
    expires_at: sb.expiresAt,
  }
  if (sb.reason) v.reason = sb.reason
  if (sb.network) v.network = sb.network
  if (Object.keys(sb.metadata).length > 0) v.metadata = sb.metadata
  return v
}

/** Polls until `fn` returns a truthy value. */
export async function until<T>(fn: () => T | undefined | false | null, timeoutMs = 3000): Promise<T> {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const v = fn()
    if (v) return v
    if (Date.now() > deadline) throw new Error('condition not met in time')
    await new Promise((r) => setTimeout(r, 5))
  }
}
