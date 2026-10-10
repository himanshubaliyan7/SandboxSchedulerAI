import { describe, expect, it } from 'vitest'
import { WebSocket as WsWebSocket } from 'ws'
import {
  AuthenticationError,
  Channel,
  ConnectionError,
  FLOW_LOW_WATERMARK,
  NotFoundError,
  Sandbox,
  SandboxTerminatedError,
  type WebSocketConstructor,
} from '../src/index.js'
import { API_KEY, until } from './helpers/mock-server.js'
import { useMockServer } from './helpers/setup.js'

const { server, create } = useMockServer()
const text = (chunks: Uint8Array[]): string => Buffer.concat(chunks).toString()

describe('pty.create', () => {
  it('starts a PTY command and authenticates the socket with the token subprotocol', async () => {
    const sb = await create()
    const pty = await sb.pty.create({ cols: 100, rows: 30, cwd: '/tmp', envs: { FOO: 'bar' }, timeoutMs: 5000, tag: 'term' })
    const cmd = server().lastCommand(sb.sandboxId)
    expect(pty.pid).toBe(cmd.pid)
    expect(cmd.request).toEqual({
      cmd: '/bin/sh',
      pty: { cols: 100, rows: 30 },
      env: { TERM: 'xterm-256color', FOO: 'bar' },
      cwd: '/tmp',
      timeout_ms: 5000,
      tag: 'term',
    })
    const session = await until(() => server().wsSessions[0])
    expect(session.protocols).toEqual(['sbx.v1', `sbx.token.${sb.accessToken}`])
    expect(session.headers['x-api-key']).toBeUndefined()
    expect(session.from).toBe(0)
    // The token never travels in the URL.
    expect(server().requests.some((r) => r.query.has('token'))).toBe(false)
    pty.disconnect()
  })

  it('defaults to 80x24 and lets cmd/args/TERM be overridden', async () => {
    const sb = await create()
    const a = await sb.pty.create()
    expect(server().lastCommand(sb.sandboxId).request).toMatchObject({ pty: { cols: 80, rows: 24 } })
    const b = await sb.pty.create({ cmd: 'python3', args: ['-q'], envs: { TERM: 'dumb' } })
    expect(server().lastCommand(sb.sandboxId).request).toMatchObject({ cmd: 'python3', args: ['-q'], env: { TERM: 'dumb' } })
    a.disconnect()
    b.disconnect()
  })

  it('delivers output to onData, sends input on channel 0 and resolves wait() with the exit status', async () => {
    const sb = await create()
    const out: Uint8Array[] = []
    const pty = await sb.pty.create({ onData: (d) => void out.push(d) })
    pty.sendInput('echo hi\r')
    await until(() => text(out) === 'echo hi\r')
    pty.sendInput(new Uint8Array([0x03]))
    await until(() => text(out).endsWith('^C\r\n'))
    pty.sendInput('\x04')
    expect(await pty.wait()).toEqual({ pid: pty.pid, exitCode: 0, reason: 'exited', truncated: false, droppedBytes: 0 })
    const stdinFrames = server().wsSessions[0]?.frames.filter((f) => f.channel === Channel.Stdin) ?? []
    expect(stdinFrames.map((f) => f.payload.toString('latin1'))).toEqual(['echo hi\r', '\x03', '\x04'])
  })

  it('does not treat a non-zero exit as an error', async () => {
    const sb = await create()
    const pty = await sb.pty.create()
    server().lastCommand(sb.sandboxId).finish(130, 'killed', 'SIGINT')
    expect(await pty.wait()).toMatchObject({ exitCode: 130, reason: 'killed', signal: 'SIGINT' })
  })

  it('sends resize on channel 4 and signals on channel 5', async () => {
    const sb = await create()
    const pty = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    pty.resize({ cols: 132.7, rows: 43 })
    await until(() => cmd.size?.cols === 132)
    expect(cmd.size).toEqual({ cols: 132, rows: 43 })
    pty.sendSignal('SIGINT')
    expect(await pty.wait()).toMatchObject({ exitCode: 130 })
    const frames = server().wsSessions[0]?.frames ?? []
    expect(frames.find((f) => f.channel === Channel.Resize)?.payload.toString()).toBe('{"cols":132,"rows":43}')
    expect(frames.find((f) => f.channel === Channel.Signal)?.payload.toString()).toBe('{"signal":"SIGINT"}')
  })

  it('splits large input into frames the agent accepts', async () => {
    const sb = await create()
    const pty = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    const big = new Uint8Array(200_000).fill(0x61)
    pty.sendInput(big)
    await until(() => cmd.stdin.reduce((n, b) => n + b.length, 0) === big.length)
    expect(cmd.stdin.every((b) => b.length <= 64 * 1024)).toBe(true)
    expect(cmd.stdin.length).toBe(4)
    pty.disconnect()
  })
})

describe('flow control', () => {
  it('acknowledges output in steps smaller than the low watermark, up to the last byte received', async () => {
    const sb = await create()
    let received = 0
    const pty = await sb.pty.create({
      onData: (d) => {
        received += d.byteLength
      },
    })
    const cmd = server().lastCommand(sb.sandboxId)
    const total = 300_000
    // Frames of every size, including the 32768-byte maximum.
    for (const size of [1, 100, 5_000, 32_768, 32_768, 9_999, 1]) cmd.write('pty', Buffer.alloc(size, 0x78))
    cmd.write('pty', Buffer.alloc(total - cmd.next, 0x79))
    await until(() => received === total)
    const session = server().wsSessions[0]
    await until(() => (session?.acks.at(-1) ?? 0) > total - FLOW_LOW_WATERMARK)
    const acks = session?.acks ?? []
    // Offsets only ever move forward ...
    expect(acks).toEqual([...acks].sort((a, b) => a - b))
    // ... never beyond what was delivered ...
    expect(acks.at(-1)).toBeLessThanOrEqual(total)
    // ... and what is left unacknowledged after each ACK is below the low
    // watermark, so the agent always resumes the command.
    expect(total - (acks.at(-1) ?? 0)).toBeLessThan(FLOW_LOW_WATERMARK)
    // The client acknowledges at least once per maximum-size frame.
    const steps = acks.map((a, i) => a - (acks[i - 1] ?? 0))
    expect(Math.max(...steps)).toBeLessThanOrEqual(32_768 + 4_096)
    pty.disconnect()
  })

  it('does not acknowledge bytes before onData has finished with them', async () => {
    const sb = await create()
    let release: () => void = () => {}
    const gate = new Promise<void>((r) => {
      release = r
    })
    const pty = await sb.pty.create({ onData: () => gate })
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.write('pty', Buffer.alloc(20_000, 0x78))
    await new Promise((r) => setTimeout(r, 40))
    const session = server().wsSessions[0]
    expect(session?.acks).toEqual([]) // the consumer is stuck: the server must see no progress
    release()
    await until(() => session?.acks.at(-1) === 20_000)
    pty.disconnect()
  })

  it('sends no ACK at all with flowControl: false', async () => {
    const sb = await create()
    let received = 0
    const pty = await sb.pty.create({
      flowControl: false,
      onData: (d) => {
        received += d.byteLength
      },
    })
    server().lastCommand(sb.sandboxId).write('pty', Buffer.alloc(50_000, 0x78))
    await until(() => received === 50_000)
    await new Promise((r) => setTimeout(r, 20))
    expect(server().wsSessions[0]?.acks).toEqual([])
    pty.disconnect()
  })

  it('clamps ackEveryBytes below the low watermark', async () => {
    const sb = await create()
    let received = 0
    const pty = await sb.pty.create({
      ackEveryBytes: 1_000_000,
      onData: (d) => {
        received += d.byteLength
      },
    })
    const cmd = server().lastCommand(sb.sandboxId)
    for (let i = 0; i < 30; i++) cmd.write('pty', Buffer.alloc(1000, 0x78))
    await until(() => received === 30_000)
    const acks = server().wsSessions[0]?.acks ?? []
    await until(() => acks.length >= 2)
    expect(Math.max(...acks.map((a, i) => a - (acks[i - 1] ?? 0)))).toBeLessThan(FLOW_LOW_WATERMARK + 1000)
    pty.disconnect()
  })
})

describe('protocol details', () => {
  it('answers a channel-7 ping with the same payload on channel 8', async () => {
    const sb = await create()
    const pty = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    const pongs: Buffer[] = []
    cmd.on('ws-pong', (p: Buffer) => pongs.push(p))
    cmd.emit('ws-ping', Buffer.from('nonce-42'))
    await until(() => pongs.length === 1)
    expect(pongs[0]?.toString()).toBe('nonce-42')
    pty.disconnect()
  })

  it('receives a maximum-size binary frame intact', async () => {
    const sb = await create()
    const out: Uint8Array[] = []
    const pty = await sb.pty.create({ onData: (d) => void out.push(d) })
    const payload = Buffer.alloc(32_768)
    for (let i = 0; i < payload.length; i++) payload[i] = i % 251
    server().lastCommand(sb.sandboxId).write('pty', payload)
    await until(() => out.length === 1)
    expect(out[0]?.byteLength).toBe(32_768)
    expect(Buffer.from(out[0] as Uint8Array).equals(payload)).toBe(true)
    expect(pty.offset).toBe(32_768)
    pty.disconnect()
  })

  it('reports truncation announced by an offset frame', async () => {
    const sb = await create()
    const first = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.write('pty', 'x'.repeat(100))
    cmd.dropBefore(60)
    first.disconnect()
    const gaps: { offset: number; dropped: number }[] = []
    const out: Uint8Array[] = []
    const pty = await sb.pty.connect(cmd.pid, { onData: (d) => void out.push(d), onTruncated: (g) => void gaps.push(g) })
    await until(() => text(out).length === 40)
    expect(gaps).toEqual([{ offset: 60, dropped: 60 }])
    expect(pty.offset).toBe(100)
    cmd.finish(0)
    expect(await pty.wait()).toMatchObject({ truncated: true, droppedBytes: 60 })
  })

  it('rejects wait() with the typed error of an error status frame', async () => {
    const sb = await create()
    const pty = await sb.pty.create()
    await until(() => server().wsSessions[0])
    server().wsSessions[0]?.ws.send(
      Buffer.concat([Buffer.from([3]), Buffer.from('{"error":{"code":"stdin_overflow","message":"too much"}}')]),
    )
    await expect(pty.wait()).rejects.toMatchObject({ code: 'stdin_overflow', message: 'too much' })
  })

  it('ignores channels it does not know', async () => {
    const sb = await create()
    const out: Uint8Array[] = []
    const pty = await sb.pty.create({ onData: (d) => void out.push(d) })
    const session = await until(() => server().wsSessions[0])
    session.ws.send(Buffer.from([42, 1, 2, 3]))
    server().lastCommand(sb.sandboxId).write('pty', 'ok')
    await until(() => text(out) === 'ok')
    pty.disconnect()
  })
})

describe('pty reconnect and errors', () => {
  it('reattaches with from=<bytes received> when the socket drops, and keeps queued input', async () => {
    const sb = await create()
    const out: Uint8Array[] = []
    const pty = await sb.pty.create({ onData: (d) => void out.push(d) })
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.write('pty', 'before ')
    await until(() => text(out) === 'before ')
    server().cutSockets()
    cmd.write('pty', 'during ') // nobody attached
    // Once the client has noticed the loss (it asks the server what happened), input is queued for the next socket.
    await until(() => server().requestsTo('GET', `/commands/${pty.pid}`).length > 0)
    pty.sendInput('typed-while-down')
    await until(() => server().wsSessions.length === 2)
    expect(server().wsSessions[1]?.from).toBe(7)
    await until(() => text(out) === 'before during typed-while-down')
    cmd.finish(0)
    expect(await pty.wait()).toMatchObject({ exitCode: 0, truncated: false })
  })

  it('rejects with SandboxTerminatedError and the reason when the sandbox ends', async () => {
    const sb = await create()
    const pty = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    await until(() => server().wsSessions[0])
    server().terminate(sb.sandboxId, 'max_lifetime')
    cmd.emit('gone') // close 1001 "sandbox terminated"
    const err = await pty.wait().catch((e) => e)
    expect(err).toBeInstanceOf(SandboxTerminatedError)
    expect(err).toMatchObject({ state: 'destroyed', reason: 'max_lifetime' })
    expect(server().wsSessions).toHaveLength(1) // no pointless reconnect
  })

  it('gives up after maxReconnects', async () => {
    const sb = await create({ stream: { maxReconnects: 2, initialDelayMs: 1, maxDelayMs: 2 } })
    const pty = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.on('newListener', () => {}) // keep the command alive
    // Every new socket is dropped at once.
    const timer = setInterval(() => server().cutSockets(), 2)
    try {
      const err = await pty.wait().catch((e) => e)
      expect(err).toBeInstanceOf(ConnectionError)
      expect(err.code).toBe('stream_lost')
      expect(err.message).toContain('pty.connect')
    } finally {
      clearInterval(timer)
    }
  })

  it('connect() to an unknown pid rejects with NotFoundError (the refused handshake is diagnosed)', async () => {
    const sb = await create()
    await expect(sb.pty.connect('p-missing')).rejects.toBeInstanceOf(NotFoundError)
  })

  it('a wrong token is an AuthenticationError', async () => {
    const sb = await create()
    const pty = await sb.pty.create()
    const stranger = await Sandbox.connect(sb.sandboxId, { baseUrl: server().url, accessToken: sb.accessToken as string })
    expect(stranger.accessToken).toBe(sb.accessToken)
    const bad = Sandbox.connect(sb.sandboxId, { baseUrl: server().url, accessToken: `sbt_${'0'.repeat(64)}`, apiKey: '' })
    await expect(bad).rejects.toBeInstanceOf(AuthenticationError)
    pty.disconnect()
  })

  it('disconnect() leaves the program running and wait() rejects', async () => {
    const sb = await create()
    const pty = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    pty.disconnect()
    await expect(pty.wait()).rejects.toMatchObject({ code: 'disconnected' })
    expect(cmd.exit).toBeUndefined()
    pty.sendInput('ignored') // must not throw
  })

  it('aborting the signal closes the session with the abort reason', async () => {
    const sb = await create()
    const ac = new AbortController()
    const pty = await sb.pty.create({ signal: ac.signal })
    ac.abort(new Error('tab closed'))
    await expect(pty.wait()).rejects.toThrow('tab closed')
  })
})

describe('pty by pid, and other WebSocket implementations', () => {
  it('connect() attaches to an existing session and replays its output', async () => {
    const sb = await create()
    const first = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.write('pty', 'history')
    first.disconnect()
    const out: Uint8Array[] = []
    const second = await sb.pty.connect(first.pid, { onData: (d) => void out.push(d) })
    await until(() => text(out) === 'history')
    const third = await sb.pty.connect(first.pid, { from: 4, onData: () => {} })
    expect(server().wsSessions.at(-1)?.from).toBe(4)
    second.disconnect()
    third.disconnect()
  })

  it('sendInput / resize / kill by pid use the REST routes', async () => {
    const sb = await create()
    const pty = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    await sb.pty.sendInput(pty.pid, 'rest input')
    expect(Buffer.concat(cmd.stdin).toString()).toBe('rest input')
    await sb.pty.resize(pty.pid, { cols: 10, rows: 5 })
    expect(cmd.size).toEqual({ cols: 10, rows: 5 })
    expect(await sb.pty.kill(pty.pid)).toBe(true)
    expect(await pty.wait()).toMatchObject({ exitCode: 137, signal: 'SIGKILL' })
    expect(await pty.kill()).toBe(false)
  })

  it('with only an API key, sends it as a handshake header (Node’s WebSocket)', async () => {
    const sb = await create()
    const first = await sb.pty.create()
    first.disconnect()
    const byKey = await Sandbox.connect(sb.sandboxId, { apiKey: API_KEY, baseUrl: server().url })
    expect(byKey.accessToken).toBeUndefined()
    const out: Uint8Array[] = []
    const pty = await byKey.pty.connect(first.pid, { onData: (d) => void out.push(d) })
    const session = server().wsSessions.at(-1)
    expect(session?.headers['x-api-key']).toBe(API_KEY)
    expect(session?.protocols).toEqual(['sbx.v1'])
    server().lastCommand(sb.sandboxId).write('pty', 'via key')
    await until(() => text(out) === 'via key')
    pty.disconnect()
  })

  it('works with the `ws` package injected as the WebSocket implementation (Node 20)', async () => {
    const WS = WsWebSocket as unknown as WebSocketConstructor
    const sb = await create({ WebSocket: WS })
    const out: Uint8Array[] = []
    const pty = await sb.pty.create({ onData: (d) => void out.push(d) })
    pty.sendInput('via ws')
    await until(() => text(out) === 'via ws')
    pty.sendInput('\x04')
    expect((await pty.wait()).exitCode).toBe(0)

    const byKey = await Sandbox.connect(sb.sandboxId, { apiKey: API_KEY, baseUrl: server().url, WebSocket: WS })
    const again = await byKey.pty.connect(pty.pid)
    expect(server().wsSessions.at(-1)?.headers['x-api-key']).toBe(API_KEY)
    expect((await again.wait()).exitCode).toBe(0)
  })
})
