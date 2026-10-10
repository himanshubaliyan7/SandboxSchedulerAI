/**
 * The behaviours that depend on the server's close-out changes (immediate
 * stream headers, the stat route, mkdir's created flag, WebSocket close
 * codes, Idempotency-Key), each also against a server without them
 * (`server().legacy = true`).
 */
import { describe, expect, it } from 'vitest'
import {
  AuthenticationError,
  CloseCode,
  errorFromClose,
  MAX_CLIENT_FRAME_PAYLOAD_BYTES,
  MAX_SERVER_FRAME_BYTES,
  NotFoundError,
  SandboxTerminatedError,
  ValidationError,
} from '../src/index.js'
import { until } from './helpers/mock-server.js'
import { useMockServer } from './helpers/setup.js'

const { server, create } = useMockServer()

describe('files.getInfo / exists / makeDir with the stat route', () => {
  it('getInfo() describes files and directories through the stat route', async () => {
    const sb = await create()
    await sb.files.write('d/f.txt', 'hello', { mode: '0600' })
    expect(await sb.files.getInfo('d/f.txt')).toMatchObject({
      name: 'f.txt',
      path: '/workspace/d/f.txt',
      type: 'file',
      size: 5,
      mode: '0600',
    })
    expect(await sb.files.stat('d')).toMatchObject({ name: 'd', type: 'dir' })
    await expect(sb.files.getInfo('d/ghost')).rejects.toMatchObject({ code: 'file_not_found' })
    await expect(sb.files.getInfo('d/ghost')).rejects.toBeInstanceOf(NotFoundError)
    // One request each, and none of them a download or a listing.
    expect(server().requestsTo('GET', '/files/stat')).toHaveLength(4)
    expect(server().requestsTo('GET', '/files/list')).toHaveLength(0)
    expect(server().requestsTo('GET', '/files')).toHaveLength(0)
  })

  it('exists() asks the stat route', async () => {
    const sb = await create()
    await sb.files.write('f.txt', 'x')
    expect(await sb.files.exists('f.txt')).toBe(true)
    expect(await sb.files.exists('ghost')).toBe(false)
    expect(server().requestsTo('GET', '/files/stat')).toHaveLength(2)
    expect(server().requestsTo('GET', '/files')).toHaveLength(0)
  })

  it('makeDir() says whether it created the directory', async () => {
    const sb = await create()
    expect(await sb.files.makeDir('a/b')).toBe(true)
    expect(await sb.files.makeDir('a/b')).toBe(false)
    expect(await sb.files.makeDir('a')).toBe(false)
  })

  it('falls back to listing the parent on a server without the stat route', async () => {
    server().legacy = true
    const sb = await create()
    await sb.files.write('d/f.txt', 'hello')
    expect(await sb.files.getInfo('d/f.txt')).toMatchObject({ name: 'f.txt', type: 'file', size: 5 })
    expect(await sb.files.getInfo('d')).toMatchObject({ name: 'd', type: 'dir' })
    expect(await sb.files.getInfo('.')).toMatchObject({ type: 'dir' })
    expect(await sb.files.exists('d/f.txt')).toBe(true)
    expect(await sb.files.exists('d/ghost')).toBe(false)
    expect(await sb.files.exists('nodir/ghost')).toBe(false)
    await expect(sb.files.getInfo('d/ghost')).rejects.toMatchObject({ code: 'file_not_found' })
    // An old server cannot tell: makeDir reports false both times.
    expect(await sb.files.makeDir('x')).toBe(false)
    expect(server().sandbox(sb.sandboxId).dirs.has('/workspace/x')).toBe(true)
  })

  it('falls back when the sandbox runs an agent older than the server (501 not_implemented)', async () => {
    const sb = await create()
    await sb.files.write('f.txt', 'x')
    server().intercept = (rec, res) => {
      if (!rec.path.endsWith('/files/stat')) return false
      res.writeHead(501, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ error: { code: 'not_implemented', message: 'old agent' } }))
      return true
    }
    expect(await sb.files.exists('f.txt')).toBe(true)
    expect(await sb.files.exists('ghost')).toBe(false)
  })
})

describe('commands.connect with immediate stream headers', () => {
  it('resolves for a silent command without any other request', async () => {
    const sb = await create()
    const h = await sb.commands.run('hang', { args: [], background: true })
    const before = server().requests.length
    const again = await sb.commands.connect(h.pid)
    expect(again.pid).toBe(h.pid)
    // Exactly the stream request: no probe of the command first.
    const mine = server().requests.slice(before)
    expect(mine.map((r) => `${r.method} ${r.path.replace(/.*\/commands/, '/commands')}`)).toEqual([`GET /commands/${h.pid}/stream`])
    again.disconnect()
    h.disconnect()
  })

  it('rejects with NotFoundError for an unknown pid', async () => {
    const sb = await create()
    await expect(sb.commands.connect('p-nope')).rejects.toBeInstanceOf(NotFoundError)
  })

  it('applies the ordinary request deadline to the stream headers', async () => {
    server().legacy = true // headers only with the first event: the old behaviour
    const sb = await create()
    const h = await sb.commands.run('hang', { args: [], background: true })
    const cmd = server().lastCommand(sb.sandboxId)
    // Against such a server connect() waits for the command's next output ...
    const pending = sb.commands.connect(h.pid, { from: h.offset })
    let settled = false
    void pending.then(
      () => {
        settled = true
      },
      () => {
        settled = true
      },
    )
    await new Promise((r) => setTimeout(r, 50))
    expect(settled).toBe(false)
    cmd.write('stdout', 'x')
    const again = await pending
    cmd.finish(0)
    expect((await again.wait()).stdout).toBe('x')
    h.disconnect()
  })

  it('sends Idempotency-Key when asked to', async () => {
    const sb = await create()
    await sb.commands.run('echo', { args: ['x'], idempotencyKey: 'job-1' })
    await sb.commands.run('echo', { args: ['x'] })
    const starts = server().requestsTo('POST', '/commands')
    expect(starts[0]?.headers['idempotency-key']).toBe('job-1')
    expect(starts[1]?.headers['idempotency-key']).toBeUndefined()
  })
})

describe('WebSocket close codes', () => {
  it('maps close codes to typed errors', () => {
    expect(errorFromClose(CloseCode.SandboxTerminated, 'sandbox_not_running')).toBeInstanceOf(SandboxTerminatedError)
    expect(errorFromClose(CloseCode.SandboxTerminated, '')).toMatchObject({ code: 'sandbox_terminated' })
    expect(errorFromClose(CloseCode.Unauthorized, 'unauthorized')).toBeInstanceOf(AuthenticationError)
    expect(errorFromClose(CloseCode.NotFound, 'process_not_found')).toBeInstanceOf(NotFoundError)
    expect(errorFromClose(CloseCode.NotFound, undefined)).toMatchObject({ code: 'process_not_found' })
    expect(errorFromClose(CloseCode.BadRequest, 'bad_request')).toBeInstanceOf(ValidationError)
    // A reason that is not a code token is not trusted as one.
    expect(errorFromClose(CloseCode.Unauthorized, 'Not A Token!')).toMatchObject({ code: 'unauthorized' })
    // Codes to reattach after, and everything that is not an application code.
    for (const code of [
      CloseCode.ShuttingDown,
      CloseCode.AgentUnavailable,
      CloseCode.RateLimited,
      CloseCode.Error,
      4321,
      1000,
      1001,
      1006,
      undefined,
    ]) {
      expect(errorFromClose(code, 'whatever')).toBeUndefined()
    }
  })

  it('agrees with the server on the frame sizes', () => {
    expect(MAX_CLIENT_FRAME_PAYLOAD_BYTES).toBe(1 << 20)
    expect(MAX_SERVER_FRAME_BYTES).toBe(32 << 10)
  })

  it('asks for refused handshakes as close codes', async () => {
    const sb = await create()
    const pty = await sb.pty.create()
    const session = await until(() => server().wsSessions[0])
    expect(
      server()
        .requests.find((r) => r.path.endsWith('/ws'))
        ?.query.get('handshake_errors'),
    ).toBe('close')
    expect(session.from).toBe(0)
    pty.disconnect()
  })

  it('connect() to an unknown command rejects from the close code, without probing', async () => {
    const sb = await create()
    const before = server().requests.length
    await expect(sb.pty.connect('p-nope')).rejects.toBeInstanceOf(NotFoundError)
    expect(
      server()
        .requests.slice(before)
        .map((r) => r.path.replace(/.*\/commands/, '/commands')),
    ).toEqual(['/commands/p-nope/ws'])
  })

  it('finishes from the close code alone when no status frame arrives', async () => {
    server().closeWithoutStatusFrame = true
    const sb = await create()
    const pty = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    await until(() => server().wsSessions[0])
    const before = server().requests.length
    server().terminate(sb.sandboxId, 'idle_timeout')
    cmd.emit('gone') // close 4000 "sandbox_terminated", no status frame
    const err = await pty.wait().catch((e) => e)
    expect(err).toBeInstanceOf(SandboxTerminatedError)
    expect(err.code).toBe('sandbox_terminated')
    expect(server().requests.slice(before)).toHaveLength(0) // no probe, no reconnect
  })

  it('reattaches after a transient close code without probing', async () => {
    const sb = await create({ stream: { initialDelayMs: 1, maxDelayMs: 2 } })
    const out: Uint8Array[] = []
    const pty = await sb.pty.create({ onData: (d) => void out.push(d) })
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.write('pty', 'one ')
    await until(() => Buffer.concat(out).toString() === 'one ')
    const before = server().requests.length
    cmd.emit('ws-close', CloseCode.AgentUnavailable, 'agent_unavailable')
    await until(() => server().wsSessions.length === 2)
    expect(server().wsSessions[1]?.from).toBe(4)
    expect(
      server()
        .requests.slice(before)
        .every((r) => r.path.endsWith('/ws')),
    ).toBe(true)
    cmd.write('pty', 'two')
    cmd.finish(0)
    expect(await pty.wait()).toMatchObject({ exitCode: 0 })
    expect(Buffer.concat(out).toString()).toBe('one two')
  })

  it('still tells a dead sandbox from a blip on a server without close codes', async () => {
    server().legacy = true
    const sb = await create()
    const pty = await sb.pty.create()
    const cmd = server().lastCommand(sb.sandboxId)
    await until(() => server().wsSessions[0])
    server().terminate(sb.sandboxId, 'max_lifetime')
    cmd.emit('gone') // close 1001, as an old server does
    const err = await pty.wait().catch((e) => e)
    expect(err).toBeInstanceOf(SandboxTerminatedError)
    expect(err).toMatchObject({ state: 'destroyed', reason: 'max_lifetime' })
    // The old way: one plain request to find out.
    expect(server().requestsTo('GET', `/commands/${pty.pid}`).length).toBeGreaterThan(0)
    // And a refused handshake is still a typed error there.
    await expect(sb.pty.connect('p-nope')).rejects.toBeInstanceOf(SandboxTerminatedError)
  })
})
