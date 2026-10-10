import { describe, expect, it } from 'vitest'
import {
  type CommandEvent,
  CommandExitError,
  ConflictError,
  ConnectionError,
  NotFoundError,
  SandboxTerminatedError,
  ValidationError,
} from '../src/index.js'
import { sendError, until } from './helpers/mock-server.js'
import { useMockServer } from './helpers/setup.js'

const { server, create } = useMockServer()

describe('commands.run (foreground)', () => {
  it('runs a shell line through /bin/sh -c and returns the result', async () => {
    const sb = await create()
    const r = await sb.commands.run('echo hello world')
    expect(r).toMatchObject({ stdout: 'hello world\n', stderr: '', exitCode: 0, reason: 'exited', truncated: false, droppedBytes: 0 })
    expect(r.pid).toMatch(/^p/)
    const req = server().lastCommand(sb.sandboxId).request
    expect(req).toMatchObject({ cmd: '/bin/sh', args: ['-c', 'echo hello world'], timeout_ms: 60000 })
  })

  it('execs directly when args are given, and maps every option onto the request', async () => {
    const sb = await create()
    await sb.commands.run('echo', { args: ['a b', 'c'], envs: { K: 'v' }, cwd: '/tmp', timeoutMs: 1500, tag: 't1', stdin: true })
    expect(server().lastCommand(sb.sandboxId).request).toEqual({
      cmd: 'echo',
      args: ['a b', 'c'],
      env: { K: 'v' },
      cwd: '/tmp',
      timeout_ms: 1500,
      tag: 't1',
      stdin: true,
    })
    const [post] = server().requestsTo('POST', '/commands')
    expect(post?.query.get('wait')).toBe('stream')
    expect(post?.headers['x-access-token']).toMatch(/^sbt_/)
    expect(post?.headers['x-api-key']).toBeUndefined()
  })

  it('omits timeout_ms when the timeout is 0, and honours the sandbox default', async () => {
    const sb = await create({ commandTimeoutMs: 0 })
    await sb.commands.run('echo x')
    expect(server().lastCommand(sb.sandboxId).request.timeout_ms).toBeUndefined()
    await sb.commands.run('echo x', { timeoutMs: 250 })
    expect(server().lastCommand(sb.sandboxId).request.timeout_ms).toBe(250)
  })

  it('throws CommandExitError with the full result on a non-zero exit', async () => {
    const sb = await create()
    const err = await sb.commands.run('fail 3').catch((e) => e)
    expect(err).toBeInstanceOf(CommandExitError)
    expect(err).toMatchObject({ exitCode: 3, stdout: 'partial\n', stderr: 'boom\n', reason: 'exited' })
    expect(err.message).toContain('exit code 3')
    expect(err.message).toContain('boom')
  })

  it('returns the result instead when throwOnNonZeroExit is false (per call or per sandbox)', async () => {
    const strict = await create()
    expect(await strict.commands.run('fail 4', { throwOnNonZeroExit: false })).toMatchObject({ exitCode: 4, stderr: 'boom\n' })
    const lenient = await create({ throwOnNonZeroExit: false })
    expect((await lenient.commands.run('fail 5')).exitCode).toBe(5)
    await expect(lenient.commands.run('fail 5', { throwOnNonZeroExit: true })).rejects.toBeInstanceOf(CommandExitError)
  })

  it('reports a per-command timeout with reason and signal', async () => {
    const sb = await create()
    const err = await sb.commands.run('timeout', { timeoutMs: 10 }).catch((e) => e)
    expect(err).toBeInstanceOf(CommandExitError)
    expect(err).toMatchObject({ exitCode: 137, reason: 'timeout', signal: 'SIGKILL' })
  })

  it('streams output to onStdout and onStderr in order', async () => {
    const sb = await create()
    const seen: string[] = []
    const p = sb.commands.run('script', { args: [], onStdout: (d) => void seen.push(`o:${d}`), onStderr: (d) => void seen.push(`e:${d}`) })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    cmd.write('stdout', 'one\n')
    await until(() => seen.length === 1)
    cmd.write('stderr', 'two\n')
    cmd.write('stdout', 'three\n')
    cmd.finish(0)
    const r = await p
    expect(seen).toEqual(['o:one\n', 'e:two\n', 'o:three\n'])
    expect(r).toMatchObject({ stdout: 'one\nthree\n', stderr: 'two\n' })
  })

  it('waits for an async callback before delivering the next chunk', async () => {
    const sb = await create()
    const order: string[] = []
    const p = sb.commands.run('script', {
      args: [],
      onStdout: async (d) => {
        order.push(`start ${d}`)
        await new Promise((r) => setTimeout(r, 20))
        order.push(`end ${d}`)
      },
    })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    cmd.write('stdout', 'a')
    cmd.write('stdout', 'b')
    cmd.finish(0)
    await p
    expect(order).toEqual(['start a', 'end a', 'start b', 'end b'])
  })

  it('rejects with the callback’s own error when a callback throws', async () => {
    const sb = await create()
    const boom = new Error('callback exploded')
    const p = sb.commands.run('echo hi', {
      onStdout: () => {
        throw boom
      },
    })
    await expect(p).rejects.toBe(boom)
    expect(server().requestsTo('GET', '/stream')).toHaveLength(0) // not mistaken for a network failure
  })

  it('writes stdin data, closes stdin and returns what the command printed', async () => {
    const sb = await create()
    const r = await sb.commands.run('cat', { args: [], stdin: 'héllo stdin' })
    expect(r.stdout).toBe('héllo stdin')
    expect(server().lastCommand(sb.sandboxId).request.stdin).toBe(true)
    expect(server().requestsTo('POST', '/stdin')[0]?.headers['content-type']).toBe('application/octet-stream')
    expect(server().requestsTo('POST', '/close-stdin')).toHaveLength(1)
  })

  it('surfaces a start error from the server as a typed error', async () => {
    const sb = await create()
    await expect(sb.commands.run('nosuch', { args: [] })).rejects.toBeInstanceOf(ValidationError)
    await expect(sb.commands.run('nosuch', { args: [] })).rejects.toMatchObject({ code: 'command_not_found', status: 400 })
  })

  it('kills the command when a foreground run is aborted', async () => {
    const sb = await create()
    const ac = new AbortController()
    const p = sb.commands.run('hang', { args: [], signal: ac.signal })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    ac.abort()
    await expect(p).rejects.toMatchObject({ name: 'AbortError' })
    await until(() => cmd.signals.includes('SIGKILL'))
  })
})

describe('UTF-8 across event boundaries', () => {
  it('does not corrupt a character split across two events', async () => {
    const sb = await create()
    const chunks: string[] = []
    const p = sb.commands.run('script', { args: [], onStdout: (d) => void chunks.push(d) })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    const bytes = Buffer.from('aé🚀z')
    // One event per byte: every multi-byte character is split.
    for (const b of bytes) {
      cmd.write('stdout', Buffer.from([b]))
      await new Promise((r) => setTimeout(r, 1))
    }
    cmd.finish(0)
    const r = await p
    expect(r.stdout).toBe('aé🚀z')
    expect(chunks.join('')).toBe('aé🚀z')
    expect(chunks.every((c) => !c.includes('�'))).toBe(true)
  })

  it('keeps separate decoders for stdout and stderr', async () => {
    const sb = await create()
    const p = sb.commands.run('script', { args: [] })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    const e = Buffer.from('é')
    cmd.write('stdout', e.subarray(0, 1))
    cmd.write('stderr', Buffer.from('ü').subarray(0, 1))
    cmd.write('stdout', e.subarray(1))
    cmd.write('stderr', Buffer.from('ü').subarray(1))
    cmd.finish(0)
    expect(await p).toMatchObject({ stdout: 'é', stderr: 'ü' })
  })

  it('survives NDJSON lines split at arbitrary network chunk boundaries', async () => {
    const sb = await create()
    server().chunkBytes = 7
    const r = await sb.commands.run('echo ✓ unicode ✓ über 🚀')
    expect(r.stdout).toBe('✓ unicode ✓ über 🚀\n')
  })

  it('flushes an incomplete trailing character as U+FFFD at exit', async () => {
    const sb = await create()
    const p = sb.commands.run('script', { args: [] })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    cmd.write('stdout', Buffer.from('é').subarray(0, 1))
    cmd.finish(0)
    expect((await p).stdout).toBe('�')
  })
})

describe('stream robustness', () => {
  it('reconnects with from=<offset> after the stream is cut, without loss or duplication', async () => {
    const sb = await create()
    const seen: string[] = []
    const p = sb.commands.run('script', { args: [], onStdout: (d) => void seen.push(d) })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    cmd.write('stdout', 'first-')
    await until(() => seen.join('') === 'first-')
    expect(server().cutStreams()).toBe(1)
    cmd.write('stdout', 'second-') // produced while nobody is connected
    await until(() => server().openStreams === 1)
    cmd.write('stdout', 'third')
    await until(() => seen.join('') === 'first-second-third')
    server().cutStreams()
    cmd.finish(0)
    const r = await p
    expect(r.stdout).toBe('first-second-third')
    expect(r.truncated).toBe(false)
    const froms = server()
      .requestsTo('GET', '/stream')
      .map((q) => q.query.get('from'))
    expect(froms).toEqual(['6', '18'])
  })

  it('reconnects when the stream ends cleanly without an exit event', async () => {
    const sb = await create()
    let ended = false
    server().intercept = (req, res) => {
      if (ended || req.method !== 'POST' || !req.path.endsWith('/commands')) return false
      ended = true
      // Start the command for real, but answer with a stream that stops after one output event.
      const real = server().sandbox(sb.sandboxId)
      server().intercept = undefined
      void fetch(`${server().url}${req.path}`, {
        method: 'POST',
        headers: { 'X-API-Key': String(req.headers['x-api-key'] ?? ''), 'X-Access-Token': String(req.headers['x-access-token']) },
        body: new Uint8Array(req.body),
      })
        .then((r) => r.json())
        .then((info: { pid: string }) => {
          const cmd = real.commands.get(info.pid)
          cmd?.write('stdout', 'abc')
          res.writeHead(200, { 'Content-Type': 'application/x-ndjson' })
          res.write(`${JSON.stringify({ type: 'start', offset: 0, command: cmd?.view() })}\n`)
          res.write(`${JSON.stringify({ type: 'stdout', offset: 0, data: Buffer.from('abc').toString('base64') })}\n`)
          res.end()
          setTimeout(() => {
            cmd?.write('stdout', 'def')
            cmd?.finish(0)
          }, 30)
        })
      return true
    }
    const r = await sb.commands.run('script', { args: [] })
    expect(r.stdout).toBe('abcdef')
    expect(server().requestsTo('GET', '/stream')[0]?.query.get('from')).toBe('3')
  })

  it('surfaces a truncated gap instead of hiding it', async () => {
    const sb = await create()
    const gaps: { offset: number; dropped: number }[] = []
    const seen: string[] = []
    const p = sb.commands.run('script', { args: [], onStdout: (d) => void seen.push(d), onTruncated: (g) => void gaps.push(g) })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    cmd.write('stdout', 'AAAA')
    await until(() => seen.length === 1)
    server().cutStreams()
    cmd.write('stdout', 'BBBBBBBBBB') // offsets 4..13
    cmd.write('stdout', 'CCCC') // offsets 14..17
    cmd.dropBefore(14) // the replay buffer no longer holds the Bs
    cmd.finish(0)
    const r = await p
    expect(gaps).toEqual([{ offset: 14, dropped: 10 }])
    expect(r).toMatchObject({ stdout: 'AAAACCCC', truncated: true, droppedBytes: 10 })
  })

  it('detects output the server skipped without announcing it', async () => {
    const sb = await create()
    server().intercept = (req, res) => {
      if (req.method !== 'POST' || !req.path.endsWith('/commands')) return false
      res.writeHead(200, { 'Content-Type': 'application/x-ndjson' })
      const line = (o: unknown): void => void res.write(`${JSON.stringify(o)}\n`)
      line({ type: 'start', offset: 0, command: { pid: 'p1', cmd: 'x', pty: false, next_offset: 0 } })
      line({ type: 'stdout', offset: 0, data: Buffer.from('ab').toString('base64') })
      line({ type: 'stdout', offset: 10, data: Buffer.from('yz').toString('base64') })
      line({ type: 'some_future_event', offset: 12 })
      line({ type: 'exit', offset: 20, exit: { exit_code: 0, reason: 'exited' } })
      res.end()
      return true
    }
    const r = await sb.commands.run('x', { args: [] })
    expect(r).toMatchObject({ stdout: 'abyz', truncated: true, droppedBytes: 16 })
  })

  it('drops the overlap when a replay repeats bytes already delivered', async () => {
    const sb = await create()
    server().intercept = (req, res) => {
      if (req.method !== 'POST' || !req.path.endsWith('/commands')) return false
      res.writeHead(200, { 'Content-Type': 'application/x-ndjson' })
      const line = (o: unknown): void => void res.write(`${JSON.stringify(o)}\n`)
      line({ type: 'start', offset: 0, command: { pid: 'p1', cmd: 'x', pty: false, next_offset: 0 } })
      line({ type: 'stdout', offset: 0, data: Buffer.from('abcd').toString('base64') })
      line({ type: 'stdout', offset: 2, data: Buffer.from('cdef').toString('base64') })
      line({ type: 'stdout', offset: 0, data: Buffer.from('abc').toString('base64') })
      line({ type: 'exit', offset: 6, exit: { exit_code: 0, reason: 'exited' } })
      res.end()
      return true
    }
    const r = await sb.commands.run('x', { args: [] })
    expect(r).toMatchObject({ stdout: 'abcdef', truncated: false })
  })

  it('ignores keepalive events', async () => {
    const sb = await create()
    const p = sb.commands.run('script', { args: [] })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    cmd.emit('keepalive')
    cmd.write('stdout', 'x')
    cmd.emit('keepalive')
    cmd.finish(0)
    expect((await p).stdout).toBe('x')
  })

  it('treats a silent stream as dead and reconnects (idle timeout)', async () => {
    const sb = await create({ stream: { idleTimeoutMs: 40, initialDelayMs: 5, maxDelayMs: 10 } })
    const p = sb.commands.run('script', { args: [] })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    cmd.write('stdout', 'x')
    await until(() => server().requestsTo('GET', '/stream').length >= 1)
    cmd.finish(0)
    expect((await p).stdout).toBe('x')
    expect(server().requestsTo('GET', '/stream')[0]?.query.get('from')).toBe('1')
  })

  it('gives up after maxReconnects with a ConnectionError that says how to resume', async () => {
    const sb = await create({ stream: { maxReconnects: 2, initialDelayMs: 1, maxDelayMs: 2 }, retry: false })
    const p = sb.commands.run('script', { args: [] })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    cmd.write('stdout', 'abc')
    p.catch(() => {})
    await new Promise((r) => setTimeout(r, 30))
    server().intercept = (req, res) => {
      if (!req.path.endsWith('/stream')) return false
      res.socket?.destroy()
      return true
    }
    server().cutStreams()
    const err = await p.catch((e) => e)
    expect(err).toBeInstanceOf(ConnectionError)
    expect(err.code).toBe('stream_lost')
    expect(err.message).toContain('from: 3')
    expect(server().requestsTo('GET', '/stream')).toHaveLength(2)
  })

  it('rejects with SandboxTerminatedError (and its reason) when the sandbox dies under the command', async () => {
    const sb = await create()
    const p = sb.commands.run('hang', { args: [] })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    p.catch(() => {})
    await new Promise((r) => setTimeout(r, 20))
    cmd.emit('gone')
    const err = await p.catch((e) => e)
    expect(err).toBeInstanceOf(SandboxTerminatedError)
    expect(err.reason).toBe('released')
  })

  it('reconnects after a shutting_down error event, and reports the sandbox state if it is gone', async () => {
    const sb = await create()
    const p = sb.commands.run('script', { args: [] })
    const cmd = await until(() => server().sandboxes.get(sb.sandboxId)?.commands.values().next().value)
    cmd.write('stdout', 'a')
    await new Promise((r) => setTimeout(r, 20))
    cmd.emit('stream-error', { code: 'shutting_down', message: 'the server is shutting down; reconnect with from=<last offset>' })
    await until(() => server().requestsTo('GET', '/stream').length === 1)
    cmd.write('stdout', 'b')
    cmd.finish(0)
    expect((await p).stdout).toBe('ab')

    const p2 = sb.commands.run('script', { args: [] })
    const cmd2 = await until(() => (server().lastCommand(sb.sandboxId) !== cmd ? server().lastCommand(sb.sandboxId) : undefined))
    p2.catch(() => {})
    await new Promise((r) => setTimeout(r, 20))
    server().terminate(sb.sandboxId, 'idle_timeout')
    cmd2.emit('stream-error', { code: 'shutting_down', message: 'bye' })
    const err = await p2.catch((e) => e)
    expect(err).toBeInstanceOf(SandboxTerminatedError)
    expect(err).toMatchObject({ status: 409, state: 'destroyed', reason: 'idle_timeout' })
  })

  it('does not restart a command whose start was never confirmed', async () => {
    const sb = await create()
    server().intercept = (req, res) => {
      if (req.method !== 'POST' || !req.path.endsWith('/commands')) return false
      res.writeHead(200, { 'Content-Type': 'application/x-ndjson' })
      res.flushHeaders()
      setTimeout(() => res.socket?.destroy(), 10)
      return true
    }
    const err = await sb.commands.run('echo hi').catch((e) => e)
    expect(err).toBeInstanceOf(ConnectionError)
    expect(err.code).toBe('start_unconfirmed')
    expect(server().requestsTo('POST', '/commands')).toHaveLength(1)
  })

  it('retries a rate-limited start, since the server rejected it before running anything', async () => {
    const sb = await create()
    let n = 0
    server().intercept = (req, res) => {
      if (req.method !== 'POST' || !req.path.endsWith('/commands') || n++ > 0) return false
      sendError(res, 429, 'rate_limited', 'slow down', { class: 'default' }, '0.01')
      return true
    }
    expect((await sb.commands.run('echo ok')).stdout).toBe('ok\n')
    expect(server().requestsTo('POST', '/commands')).toHaveLength(2)
    expect(server().sandbox(sb.sandboxId).commands.size).toBe(1)
  })
})

describe('background commands', () => {
  it('returns a handle with a pid; wait() gives the result', async () => {
    const sb = await create()
    const h = await sb.commands.run('script', { args: [], background: true })
    expect(h.pid).toBe(server().lastCommand(sb.sandboxId).pid)
    expect(h.exitCode).toBeUndefined()
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.write('stdout', 'bg\n')
    cmd.finish(0)
    expect(await h.wait()).toMatchObject({ stdout: 'bg\n', exitCode: 0, pid: h.pid })
    expect(h.exitCode).toBe(0)
    expect(h.stdout).toBe('bg\n')
  })

  it('kill() ends the command; wait() reports the signal', async () => {
    const sb = await create()
    const h = await sb.commands.run('hang', { args: [], background: true })
    expect(await h.kill()).toBe(true)
    const err = await h.wait().catch((e) => e)
    expect(err).toBeInstanceOf(CommandExitError)
    expect(err).toMatchObject({ exitCode: 137, reason: 'killed', signal: 'SIGKILL' })
    expect(await h.kill()).toBe(false) // already exited
    expect(await h.wait({ throwOnNonZeroExit: false })).toMatchObject({ exitCode: 137 })
  })

  it('sendStdin() and closeStdin() round-trip through the command', async () => {
    const sb = await create()
    const h = await sb.commands.run('cat', { args: [], background: true, stdin: true })
    await h.sendStdin('ping ')
    await h.sendStdin(new Uint8Array([0xf0, 0x9f, 0x9a, 0x80]))
    await h.closeStdin()
    expect((await h.wait()).stdout).toBe('ping 🚀')
    await expect(h.sendStdin('late')).rejects.toBeInstanceOf(ConflictError)
  })

  it('rejects stdin writes to a command started without stdin', async () => {
    const sb = await create()
    const h = await sb.commands.run('hang', { args: [], background: true })
    await expect(h.sendStdin('x')).rejects.toMatchObject({ code: 'stdin_closed', status: 409 })
    await h.kill()
  })

  it('iterates events, including bytes, text, offsets and the exit', async () => {
    const sb = await create()
    const h = await sb.commands.run('script', { args: [], background: true })
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.write('stdout', 'ab')
    cmd.write('stderr', 'cd')
    setTimeout(() => {
      cmd.write('stdout', 'ef')
      cmd.finish(2)
    }, 20)
    const events: CommandEvent[] = []
    for await (const ev of h) events.push(ev)
    expect(events.map((e) => e.type)).toEqual(['stdout', 'stderr', 'stdout', 'exit'])
    expect(events[1]).toMatchObject({ type: 'stderr', text: 'cd', offset: 2 })
    expect(Array.from((events[1] as { data: Uint8Array }).data)).toEqual([0x63, 0x64])
    expect(events[3]).toEqual({ type: 'exit', exitCode: 2, reason: 'exited' })
  })

  it('iteration ends early without disturbing wait()', async () => {
    const sb = await create()
    const h = await sb.commands.run('script', { args: [], background: true })
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.write('stdout', 'one')
    for await (const ev of h) {
      expect(ev.type).toBe('stdout')
      break
    }
    cmd.write('stdout', 'two')
    cmd.finish(0)
    expect((await h.wait()).stdout).toBe('onetwo')
  })

  it('iteration throws the stream error', async () => {
    const sb = await create()
    const h = await sb.commands.run('hang', { args: [], background: true })
    setTimeout(() => server().lastCommand(sb.sandboxId).emit('gone'), 10)
    const run = async (): Promise<void> => {
      for await (const _ of h) {
        // drain
      }
    }
    await expect(run()).rejects.toBeInstanceOf(SandboxTerminatedError)
  })

  it('disconnect() stops the stream but not the command', async () => {
    const sb = await create()
    const h = await sb.commands.run('script', { args: [], background: true })
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.write('stdout', 'abc')
    await until(() => h.stdout === 'abc')
    h.disconnect()
    await expect(h.wait()).rejects.toMatchObject({ code: 'disconnected' })
    await until(() => server().openStreams === 0)
    expect(cmd.exit).toBeUndefined()
    expect(cmd.signals).toEqual([])
    // Reattach from where the first handle stopped.
    const h2 = await sb.commands.connect(h.pid, { from: h.offset })
    cmd.write('stdout', 'def')
    cmd.finish(0)
    expect((await h2.wait()).stdout).toBe('def')
  })
})

describe('commands.list / get / connect / kill / sendStdin', () => {
  it('lists and gets commands with camelCase fields', async () => {
    const sb = await create()
    await sb.commands.run('echo', { args: ['x'], tag: 'done' })
    const h = await sb.commands.run('hang', { args: [], background: true, tag: 'live' })
    const all = await sb.commands.list()
    expect(all).toHaveLength(2)
    expect(all[0]).toMatchObject({
      cmd: 'echo',
      args: ['x'],
      pty: false,
      nextOffset: 2,
      tag: 'done',
      exit: { exitCode: 0, reason: 'exited' },
    })
    expect(all[0]?.startedAt).toBeInstanceOf(Date)
    expect(all[1]).toMatchObject({ pid: h.pid, tag: 'live' })
    expect(all[1]?.exit).toBeUndefined()
    expect(await sb.commands.get(h.pid)).toMatchObject({ pid: h.pid, cmd: 'hang' })
    await expect(sb.commands.get('nope')).rejects.toMatchObject({ code: 'process_not_found' })
  })

  it('connect() replays buffered output, then follows live', async () => {
    const sb = await create()
    const h = await sb.commands.run('script', { args: [], background: true })
    const cmd = server().lastCommand(sb.sandboxId)
    cmd.write('stdout', 'early ')
    const seen: string[] = []
    const second = await sb.commands.connect(h.pid, { onStdout: (d) => void seen.push(d) })
    expect(second.pid).toBe(h.pid)
    cmd.write('stdout', 'late')
    cmd.finish(0)
    expect((await second.wait()).stdout).toBe('early late')
    expect(seen.join('')).toBe('early late')
    expect(server().requestsTo('GET', '/stream')[0]?.query.get('from')).toBe('0')
  })

  it('connect() works after the command has exited, and honours `from`', async () => {
    const sb = await create()
    const r = await sb.commands.run('echo 0123456789')
    const again = await sb.commands.connect(r.pid, { from: 5 })
    expect((await again.wait()).stdout).toBe('56789\n')
  })

  it('connect() to an unknown pid rejects with NotFoundError', async () => {
    const sb = await create()
    await expect(sb.commands.connect('p-missing')).rejects.toBeInstanceOf(NotFoundError)
  })

  it('kill() returns false for unknown and exited commands, and can send another signal', async () => {
    const sb = await create()
    expect(await sb.commands.kill('p-missing')).toBe(false)
    const h = await sb.commands.run('hang', { args: [], background: true })
    expect(await sb.commands.kill(h.pid, { killSignal: 'SIGTERM' })).toBe(true)
    expect(await h.wait({ throwOnNonZeroExit: false })).toMatchObject({ exitCode: 143, signal: 'SIGTERM' })
    expect(await sb.commands.kill(h.pid)).toBe(false)
  })

  it('sendStdin() and closeStdin() by pid', async () => {
    const sb = await create()
    const h = await sb.commands.run('cat', { args: [], background: true, stdin: true })
    await sb.commands.sendStdin(h.pid, 'by pid')
    await sb.commands.closeStdin(h.pid)
    expect((await h.wait()).stdout).toBe('by pid')
  })

  it('reports a tag that is already in use', async () => {
    const sb = await create()
    const h = await sb.commands.run('hang', { args: [], background: true, tag: 'dup' })
    await expect(sb.commands.run('hang', { args: [], background: true, tag: 'dup' })).rejects.toMatchObject({ code: 'tag_in_use' })
    await h.kill()
  })
})
