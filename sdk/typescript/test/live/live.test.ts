/**
 * Live integration tests against a real sbxd. Skipped unless
 * SBX_LIVE_BASE_URL and SBX_LIVE_API_KEY are set; scripts/live-test.sh
 * starts a server and runs them.
 */

import { createHash } from 'node:crypto'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import {
  CommandExitError,
  type FetchLike,
  NotFoundError,
  Sandbox,
  type SandboxCreateOptions,
  SandboxTerminatedError,
} from '../../src/index.js'

const baseUrl = process.env.SBX_LIVE_BASE_URL
const apiKey = process.env.SBX_LIVE_API_KEY
const otherKey = process.env.SBX_LIVE_OTHER_API_KEY
const template = process.env.SBX_LIVE_TEMPLATE ?? 'python'
const live = Boolean(baseUrl && apiKey)

const conn = (): { apiKey: string; baseUrl: string } => ({ apiKey: apiKey as string, baseUrl: baseUrl as string })
const create = (opts: SandboxCreateOptions = {}): Promise<Sandbox> => Sandbox.create(template, { ...conn(), ...opts })
const sha256 = (b: Uint8Array | string): string => createHash('sha256').update(b).digest('hex')
const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms))

async function until(fn: () => boolean, what: string, timeoutMs = 10_000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (!fn()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`)
    await sleep(20)
  }
}

function pseudoRandom(n: number): Uint8Array {
  const out = new Uint8Array(n)
  let x = 0x9e3779b9
  for (let i = 0; i < n; i++) {
    x ^= x << 13
    x ^= x >>> 17
    x ^= x << 5
    out[i] = x & 0xff
  }
  return out
}

describe.skipIf(!live)('live: against a real sbxd', { timeout: 60_000 }, () => {
  let sb: Sandbox
  const extra: Sandbox[] = []

  beforeAll(async () => {
    sb = await create({ envs: { SBX_TEST_ENV: 'from-create' }, metadata: { suite: 'ts-live', run: String(process.pid) } })
  })

  afterAll(async () => {
    for (const s of [sb, ...extra]) await s?.kill().catch(() => {})
  })

  describe('sandbox', () => {
    it('create returns a running sandbox with an access token', () => {
      expect(sb.sandboxId).toMatch(/^sb-[0-9a-f]{16}$/)
      expect(sb.accessToken).toMatch(/^sbt_[0-9a-f]{64}$/)
      expect(sb.info).toMatchObject({ templateId: template, state: 'running', metadata: { suite: 'ts-live' } })
      expect(sb.info?.expiresAt).toBeInstanceOf(Date)
      expect(sb.info?.startedAt).toBeInstanceOf(Date)
    })

    it('getInfo and isRunning', async () => {
      const info = await sb.getInfo()
      expect(info).toMatchObject({ sandboxId: sb.sandboxId, state: 'running' })
      expect(info.reason).toBeUndefined()
      expect(await sb.isRunning()).toBe(true)
    })

    it('setTimeout moves expiresAt', async () => {
      const info = await sb.setTimeout(10 * 60_000)
      const remaining = (info.expiresAt?.getTime() ?? 0) - Date.now()
      expect(remaining).toBeGreaterThan(9 * 60_000)
      expect(remaining).toBeLessThan(11 * 60_000)
      await expect(sb.setTimeout(0)).rejects.toMatchObject({ code: 'invalid_timeout', status: 400 })
    })

    it('list finds the sandbox, also by metadata', async () => {
      const all = await Sandbox.list(conn())
      expect(all.map((s) => s.sandboxId)).toContain(sb.sandboxId)
      const mine = await Sandbox.list({ ...conn(), metadata: { suite: 'ts-live', run: String(process.pid) } })
      expect(mine.map((s) => s.sandboxId)).toEqual([sb.sandboxId])
      expect(await Sandbox.list({ ...conn(), metadata: { suite: 'no-such-suite' } })).toEqual([])
    })

    it('listTemplates describes the template', async () => {
      const t = (await Sandbox.listTemplates(conn())).find((x) => x.templateId === template)
      expect(t).toBeDefined()
      expect(t?.resources.memoryBytes).toBeGreaterThan(0)
      expect(t?.image).toContain('python')
    })

    it('getStats reads the cgroup', async () => {
      const stats = await sb.getStats()
      expect(stats.memoryBytes).toBeGreaterThan(0)
      expect(stats.pidsCurrent).toBeGreaterThan(0)
    })

    it('updateNetwork tightens the policy and reverts it', async () => {
      const locked = await sb.updateNetwork({ mode: 'none' })
      expect(locked.network?.mode).toBe('none')
      const open = await sb.updateNetwork({ mode: 'allowlist', allowDomains: ['example.com'] })
      expect(open.network).toMatchObject({ mode: 'allowlist', allowDomains: ['example.com'] })
      await expect(sb.updateNetwork({ mode: 'full' })).rejects.toMatchObject({ code: 'network_not_allowed' })
    })
  })

  describe('commands', () => {
    it('runs python: print(6*7) is 42', async () => {
      const r = await sb.commands.run('python3', { args: ['-c', 'print(6*7)'] })
      expect(r).toMatchObject({ stdout: '42\n', stderr: '', exitCode: 0, reason: 'exited', truncated: false })
    })

    it('runs a shell line, with the sandbox env, per-command envs and cwd', async () => {
      const r = await sb.commands.run('echo "$SBX_TEST_ENV $PER_CMD"; pwd', { envs: { PER_CMD: 'per-command' }, cwd: '/tmp' })
      expect(r.stdout).toBe('from-create per-command\n/tmp\n')
      expect((await sb.commands.run('pwd')).stdout).toBe('/workspace\n')
    })

    it('reports stderr and a non-zero exit as CommandExitError', async () => {
      const err = await sb.commands.run('echo out; echo err >&2; exit 3').catch((e) => e)
      expect(err).toBeInstanceOf(CommandExitError)
      expect(err).toMatchObject({ exitCode: 3, stdout: 'out\n', stderr: 'err\n', reason: 'exited' })
      const r = await sb.commands.run('exit 4', { throwOnNonZeroExit: false })
      expect(r.exitCode).toBe(4)
    })

    it('reports an unknown executable as a typed error', async () => {
      await expect(sb.commands.run('no-such-binary-xyz', { args: [] })).rejects.toMatchObject({ code: 'command_not_found', status: 400 })
    })

    it('streams stdout and stderr to the callbacks while the command runs', async () => {
      const seen: { at: number; ch: string; data: string }[] = []
      const t0 = Date.now()
      const code = [
        'import sys, time',
        'for i in range(3):',
        '    print(f"out{i}", flush=True)',
        '    print(f"err{i}", file=sys.stderr, flush=True)',
        '    time.sleep(0.3)',
      ].join('\n')
      const r = await sb.commands.run('python3', {
        args: ['-c', code],
        onStdout: (d) => void seen.push({ at: Date.now() - t0, ch: 'o', data: d }),
        onStderr: (d) => void seen.push({ at: Date.now() - t0, ch: 'e', data: d }),
      })
      const took = Date.now() - t0
      expect(r.stdout).toBe('out0\nout1\nout2\n')
      expect(r.stderr).toBe('err0\nerr1\nerr2\n')
      expect(
        seen
          .filter((s) => s.ch === 'o')
          .map((s) => s.data)
          .join(''),
      ).toBe(r.stdout)
      // The first chunk arrived long before the command finished: it was streamed, not collected.
      expect(seen[0]?.at).toBeLessThan(took - 400)
      expect(took).toBeGreaterThanOrEqual(850)
    })

    it('keeps multi-byte characters intact when they are split across events', async () => {
      const code = [
        'import sys, time',
        'data = "aé✓🚀z\\n".encode()',
        'for b in data:',
        '    sys.stdout.buffer.write(bytes([b])); sys.stdout.buffer.flush(); time.sleep(0.03)',
      ].join('\n')
      const chunks: string[] = []
      const r = await sb.commands.run('python3', { args: ['-c', code], onStdout: (d) => void chunks.push(d) })
      expect(r.stdout).toBe('aé✓🚀z\n')
      expect(chunks.join('')).toBe('aé✓🚀z\n')
      expect(chunks.length).toBeGreaterThan(2) // really arrived in pieces
    })

    it('background command: handle, list, wait, kill', async () => {
      const h = await sb.commands.run('sleep', { args: ['30'], background: true, tag: 'sleeper' })
      expect(h.pid).not.toBe('')
      const listed = (await sb.commands.list()).find((c) => c.pid === h.pid)
      expect(listed).toMatchObject({ cmd: 'sleep', args: ['30'], tag: 'sleeper', pty: false })
      expect(listed?.exit).toBeUndefined()
      expect(await h.kill()).toBe(true)
      const r = await h.wait({ throwOnNonZeroExit: false })
      expect(r).toMatchObject({ exitCode: 137, reason: 'killed', signal: 'SIGKILL' })
      expect(await sb.commands.kill(h.pid)).toBe(false)
      expect((await sb.commands.get(h.pid)).exit).toMatchObject({ exitCode: 137 })
    })

    it('background command: iterate events', async () => {
      const h = await sb.commands.run('echo one; sleep 0.2; echo two >&2; exit 5', { background: true })
      const types: string[] = []
      let text = ''
      for await (const ev of h) {
        types.push(ev.type)
        if (ev.type === 'stdout' || ev.type === 'stderr') text += ev.text
      }
      expect(types).toEqual(['stdout', 'stderr', 'exit'])
      expect(text).toBe('one\ntwo\n')
      await expect(h.wait()).rejects.toBeInstanceOf(CommandExitError)
    })

    it('stdin round-trip: sendStdin, closeStdin', async () => {
      const h = await sb.commands.run('cat', { args: [], background: true, stdin: true })
      await h.sendStdin('hello ')
      await h.sendStdin(new TextEncoder().encode('wörld 🚀\n'))
      await sb.commands.sendStdin(h.pid, 'by pid\n')
      await h.closeStdin()
      expect((await h.wait()).stdout).toBe('hello wörld 🚀\nby pid\n')
    })

    it('stdin as data: python reads its program from stdin', async () => {
      const r = await sb.commands.run('python3', { args: ['-'], stdin: 'import sys\nprint(sum(range(101)))\n' })
      expect(r.stdout).toBe('5050\n')
    })

    it('writing to a command without stdin is a conflict', async () => {
      const h = await sb.commands.run('sleep', { args: ['5'], background: true })
      await expect(h.sendStdin('x')).rejects.toMatchObject({ code: 'stdin_closed', status: 409 })
      await h.kill()
    })

    it('enforces the per-command timeout', async () => {
      const t0 = Date.now()
      const err = await sb.commands.run('sleep', { args: ['20'], timeoutMs: 500 }).catch((e) => e)
      expect(err).toBeInstanceOf(CommandExitError)
      expect(err).toMatchObject({ reason: 'timeout', exitCode: 137 })
      expect(Date.now() - t0).toBeLessThan(5000)
    })

    it('connect() to a silent command returns promptly and then follows it', async () => {
      const h = await sb.commands.run('sleep 1.5; echo late', { background: true })
      const t0 = Date.now()
      const second = await sb.commands.connect(h.pid)
      expect(Date.now() - t0).toBeLessThan(1000)
      expect((await second.wait()).stdout).toBe('late\n')
      // After the exit the output can still be replayed, from any offset.
      const third = await sb.commands.connect(h.pid, { from: 2 })
      expect((await third.wait()).stdout).toBe('te\n')
      await expect(sb.commands.connect('no-such-pid')).rejects.toBeInstanceOf(NotFoundError)
    })

    it('the stream route of a silent command sends its response headers at once', async () => {
      const h = await sb.commands.run('sleep 2; echo x', { background: true })
      const t0 = Date.now()
      const res = await fetch(`${baseUrl}/v1/sandboxes/${sb.sandboxId}/commands/${h.pid}/stream`, {
        headers: { 'X-Access-Token': sb.accessToken as string },
      })
      const headersAfter = Date.now() - t0
      await res.body?.cancel()
      console.log(`[measured] GET .../stream of a command silent for 2 s: response headers after ${headersAfter} ms`)
      expect(res.status).toBe(200)
      expect(headersAfter).toBeLessThan(500)
      await h.wait()
    })

    it('an Idempotency-Key makes a repeated start attach to the first command', async () => {
      const key = `live-${Date.now()}`
      const first = await sb.commands.run('echo once; sleep 0.3', { idempotencyKey: key })
      const again = await sb.commands.run('echo once; sleep 0.3', { idempotencyKey: key })
      expect(again.pid).toBe(first.pid)
      expect(again.stdout).toBe('once\n')
      await expect(sb.commands.run('echo other', { idempotencyKey: key })).rejects.toMatchObject({ code: 'idempotency_key_reused' })
    })

    it('reconnects with from=<offset> when the stream is cut mid-command, without loss or duplication', async () => {
      // A fetch that breaks the first NDJSON response shortly after it starts, as a proxy would.
      const requests: string[] = []
      let cut = 0
      const flaky: FetchLike = async (url, init) => {
        requests.push(`${init?.method ?? 'GET'} ${url.replace(/^.*\/commands/, '/commands')}`)
        const res = await fetch(url, init)
        const isStream = res.headers.get('content-type')?.includes('ndjson')
        if (!isStream || !res.body || cut >= 2) return res
        cut++
        let lines = 0
        const breaker = new TransformStream<Uint8Array, Uint8Array>({
          transform(chunk, controller) {
            controller.enqueue(chunk)
            lines += chunk.filter((b) => b === 0x0a).length
            if (lines >= 3) controller.error(new TypeError('terminated: connection cut by test'))
          },
        })
        return new Response(res.body.pipeThrough(breaker), { status: res.status, headers: res.headers })
      }
      const client = await Sandbox.connect(sb.sandboxId, {
        ...conn(),
        accessToken: sb.accessToken as string,
        fetch: flaky,
        stream: { initialDelayMs: 20, maxDelayMs: 50 },
      })
      const code = 'import time\nfor i in range(12):\n    print(f"line-{i:02d}", flush=True)\n    time.sleep(0.05)\n'
      const r = await client.commands.run('python3', { args: ['-c', code] })
      const expected = Array.from({ length: 12 }, (_, i) => `line-${String(i).padStart(2, '0')}\n`).join('')
      expect(r.stdout).toBe(expected)
      expect(r.truncated).toBe(false)
      expect(cut).toBe(2)
      const resumes = requests.filter((q) => q.includes('/stream?from='))
      expect(resumes.length).toBeGreaterThanOrEqual(2)
      expect(resumes.every((q) => Number(/from=(\d+)/.exec(q)?.[1]) % 8 === 0)).toBe(true) // whole lines of 8 bytes
    })

    it('records commands in the event log', async () => {
      await sb.commands.run('true', { args: [] })
      // The finished event is written by the server's watcher just after the exit.
      let finished = await sb.events({ type: 'command_finished' })
      for (let i = 0; i < 20 && finished.length === 0; i++) {
        await sleep(100)
        finished = await sb.events({ type: 'command_finished' })
      }
      expect(finished.length).toBeGreaterThan(0)
      expect(finished[0]?.data).toHaveProperty('exit_code')
      const all = await sb.events()
      expect(all.map((e) => e.type)).toContain('command_started')
      expect(all[0]?.at).toBeInstanceOf(Date)
    })
  })

  describe('files', () => {
    it('writes and reads text, lists, checks existence, removes', async () => {
      await sb.files.write('dir/héllo.txt', 'grüße 🚀\n')
      expect(await sb.files.read('dir/héllo.txt')).toBe('grüße 🚀\n')
      expect(await sb.files.read('/workspace/dir/héllo.txt', { format: 'text' })).toBe('grüße 🚀\n')
      expect((await sb.commands.run('cat dir/héllo.txt')).stdout).toBe('grüße 🚀\n')
      const entries = await sb.files.list('dir')
      expect(entries).toHaveLength(1)
      expect(entries[0]).toMatchObject({ name: 'héllo.txt', path: '/workspace/dir/héllo.txt', type: 'file', size: 13, mode: '0644' })
      expect(entries[0]?.modifiedTime).toBeInstanceOf(Date)
      expect((await sb.files.list()).find((e) => e.name === 'dir')?.type).toBe('dir')
      expect(await sb.files.exists('dir/héllo.txt')).toBe(true)
      expect(await sb.files.exists('dir')).toBe(true)
      expect(await sb.files.exists('dir/nope')).toBe(false)
      await sb.files.remove('dir/héllo.txt')
      expect(await sb.files.exists('dir/héllo.txt')).toBe(false)
      await expect(sb.files.read('dir/héllo.txt')).rejects.toMatchObject({ code: 'file_not_found', status: 404 })
      await sb.files.remove('dir')
      expect(await sb.files.exists('dir')).toBe(false)
    })

    it('round-trips a 5 MiB binary file, as bytes and as a stream', async () => {
      const data = pseudoRandom(5 * 1024 * 1024 + 123)
      const digest = sha256(data)
      await sb.files.write('big.bin', data)
      // The sandbox sees exactly the same bytes.
      const inside = await sb.commands.run('python3', {
        args: ['-c', 'import hashlib,sys; print(hashlib.sha256(open("big.bin","rb").read()).hexdigest())'],
      })
      expect(inside.stdout.trim()).toBe(digest)
      const back = await sb.files.read('big.bin', { format: 'bytes' })
      expect(back.byteLength).toBe(data.byteLength)
      expect(sha256(back)).toBe(digest)
      const stream = await sb.files.read('big.bin', { format: 'stream' })
      const hash = createHash('sha256')
      let n = 0
      const reader = stream.getReader()
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        hash.update(value)
        n += value.byteLength
      }
      expect(n).toBe(data.byteLength)
      expect(hash.digest('hex')).toBe(digest)
      expect((await sb.files.list()).find((e) => e.name === 'big.bin')?.size).toBe(data.byteLength)
    })

    it('uploads from a ReadableStream and a Blob', async () => {
      const data = pseudoRandom(1024 * 1024 + 7)
      let at = 0
      const stream = new ReadableStream<Uint8Array>({
        pull(c) {
          if (at >= data.byteLength) return c.close()
          c.enqueue(data.subarray(at, at + 64 * 1024))
          at += 64 * 1024
        },
      })
      await sb.files.write('streamed.bin', stream)
      expect(sha256(await sb.files.read('streamed.bin', { format: 'bytes' }))).toBe(sha256(data))
      await sb.files.write('blob.bin', new Blob([data as Uint8Array<ArrayBuffer>]))
      expect(sha256(await sb.files.read('blob.bin', { format: 'bytes' }))).toBe(sha256(data))
      await sb.files.remove('streamed.bin')
      await sb.files.remove('blob.bin')
      await sb.files.remove('big.bin')
    })

    it('makeDir, mode, and errors', async () => {
      expect(await sb.files.makeDir('a/b/c')).toBe(true)
      expect(await sb.files.makeDir('a/b/c')).toBe(false)
      expect(await sb.files.makeDir('a')).toBe(false)
      expect((await sb.files.list('a/b')).map((e) => [e.name, e.type])).toEqual([['c', 'dir']])
      expect(await sb.files.getInfo('a/b')).toMatchObject({ name: 'b', path: '/workspace/a/b', type: 'dir' })
      await expect(sb.files.getInfo('a/nope')).rejects.toMatchObject({ code: 'file_not_found', status: 404 })
      await sb.files.write('a/run.sh', '#!/bin/sh\necho ran\n', { mode: 0o755 })
      expect((await sb.files.list('a')).find((e) => e.name === 'run.sh')?.mode).toBe('0755')
      expect((await sb.commands.run('./a/run.sh', { args: [] })).stdout).toBe('ran\n')
      await expect(sb.files.read('a')).rejects.toMatchObject({ code: 'is_directory', status: 409 })
      await expect(sb.files.list('a/nope')).rejects.toMatchObject({ code: 'file_not_found' })
      await sb.files.remove('a')
    })
  })

  describe('pty', () => {
    const term = (): { out: () => string; onData: (d: Uint8Array) => void; clear: () => void } => {
      const dec = new TextDecoder()
      let buf = ''
      return {
        out: () => buf,
        onData: (d) => {
          buf += dec.decode(d, { stream: true })
        },
        clear: () => {
          buf = ''
        },
      }
    }

    it('interactive shell: stty size, resize, Ctrl+C of `yes`, exit code', async () => {
      const t = term()
      const pty = await sb.pty.create({ cols: 100, rows: 30, onData: t.onData })
      expect(pty.pid).not.toBe('')
      expect((await sb.commands.get(pty.pid)).pty).toBe(true)

      pty.sendInput('stty size\n')
      await until(() => /\b30 100\b/.test(t.out()), 'the initial terminal size')

      pty.resize({ cols: 120, rows: 50 })
      t.clear()
      pty.sendInput('stty size\n')
      await until(() => /\b50 120\b/.test(t.out()), 'the resized terminal size')

      // `yes` floods the terminal; flow control keeps it bounded and Ctrl+C stops it.
      t.clear()
      pty.sendInput('yes\n')
      await until(() => t.out().length > 200_000, 'output from yes')
      pty.sendInput('\x03')
      const marker = `done-${Date.now()}`
      pty.sendInput(`echo ${marker}-$((40+2))\n`)
      await until(() => t.out().includes(`${marker}-42`), 'the shell prompt after Ctrl+C', 20_000)

      pty.sendInput('exit 7\n')
      expect(await pty.wait()).toMatchObject({ pid: pty.pid, exitCode: 7, reason: 'exited' })
      expect(pty.offset).toBeGreaterThan(200_000)
    })

    it('reattaches to a session by pid, and drives it over REST', async () => {
      const t1 = term()
      const first = await sb.pty.create({ onData: t1.onData })
      first.sendInput('echo first-$((1+1))\n')
      await until(() => t1.out().includes('first-2'), 'output on the first socket')
      first.disconnect()
      await expect(first.wait()).rejects.toMatchObject({ code: 'disconnected' })

      const t2 = term()
      const second = await sb.pty.connect(first.pid, { onData: t2.onData })
      await until(() => t2.out().includes('first-2'), 'the replayed history')
      await sb.pty.sendInput(first.pid, 'echo rest-$((2+2))\n')
      await until(() => t2.out().includes('rest-4'), 'output of input sent over REST')
      await sb.pty.resize(first.pid, { cols: 77, rows: 22 })
      second.sendInput('stty size\n')
      await until(() => /\b22 77\b/.test(t2.out()), 'the size set over REST')
      expect(await sb.pty.kill(first.pid)).toBe(true)
      expect(await second.wait()).toMatchObject({ exitCode: 137, reason: 'killed' })
    })

    it('works with only an API key (the key travels in a handshake header)', async () => {
      const byKey = await Sandbox.connect(sb.sandboxId, conn())
      expect(byKey.accessToken).toBeUndefined()
      const t = term()
      const pty = await byKey.pty.create({ onData: t.onData })
      pty.sendInput('echo key-$((3+3)); exit\n')
      expect((await pty.wait()).exitCode).toBe(0)
      expect(t.out()).toContain('key-6')
    })

    it('an unknown pid is a NotFoundError', async () => {
      await expect(sb.pty.connect('no-such-pid')).rejects.toBeInstanceOf(NotFoundError)
    })
  })

  describe('credentials and tenancy', () => {
    it('a second client connects with id + access token only', async () => {
      const guest = await Sandbox.connect(sb.sandboxId, { baseUrl: baseUrl as string, accessToken: sb.accessToken as string, apiKey: '' })
      expect((await guest.commands.run('echo guest')).stdout).toBe('guest\n')
      await guest.files.write('guest.txt', 'from the guest')
      expect(await sb.files.read('guest.txt')).toBe('from the guest')
      expect(await guest.isRunning()).toBe(true)
      // The token cannot reach the control plane.
      await expect(guest.kill()).rejects.toMatchObject({ code: 'missing_credentials' })
    })

    it('a wrong access token is rejected', async () => {
      const bad = Sandbox.connect(sb.sandboxId, { baseUrl: baseUrl as string, accessToken: `sbt_${'0'.repeat(64)}`, apiKey: '' })
      await expect(bad).rejects.toMatchObject({ name: 'AuthenticationError', status: 401 })
      await expect(Sandbox.list({ baseUrl: baseUrl as string, apiKey: 'sbx_nope_nope' })).rejects.toMatchObject({ status: 401 })
    })

    it.skipIf(!otherKey)('another tenant’s key gets not-found for this sandbox', async () => {
      const other = { apiKey: otherKey as string, baseUrl: baseUrl as string }
      await expect(Sandbox.connect(sb.sandboxId, other)).rejects.toBeInstanceOf(NotFoundError)
      await expect(Sandbox.getInfo(sb.sandboxId, other)).rejects.toMatchObject({ code: 'not_found', status: 404 })
      expect(await Sandbox.kill(sb.sandboxId, other)).toBe(false)
      expect((await Sandbox.list({ ...other, state: 'all' })).map((s) => s.sandboxId)).not.toContain(sb.sandboxId)
      // The other tenant's key plus this sandbox's id on the data plane: also not found.
      const viaKey = Sandbox.connect(sb.sandboxId, { ...other, accessToken: undefined as unknown as string })
      await expect(viaKey).rejects.toBeInstanceOf(NotFoundError)
      expect(await sb.isRunning()).toBe(true) // untouched
    })
  })

  describe('kill', () => {
    it('a terminal learns from the close code that its sandbox was killed', async () => {
      const victim = await create({ timeoutMs: 120_000 })
      extra.push(victim)
      const pty = await victim.pty.create()
      const waiting = pty.wait().catch((e) => e)
      await victim.kill()
      const err = await waiting
      expect(err).toBeInstanceOf(SandboxTerminatedError)
      expect(err).toMatchObject({ code: 'sandbox_terminated', reason: 'released' })
    })

    it('kill ends the sandbox; later calls report why', async () => {
      const victim = await create({ timeoutMs: 120_000 })
      extra.push(victim)
      const h = await victim.commands.run('sleep', { args: ['60'], background: true })
      expect(await victim.kill({ waitUntilDestroyed: true })).toBe(true)
      const info = await victim.getInfo()
      expect(info.state).toBe('destroyed')
      expect(info.reason).toBe('released')
      expect(await victim.isRunning()).toBe(false)
      const err = await victim.commands.run('echo x').catch((e) => e)
      expect(err).toBeInstanceOf(SandboxTerminatedError)
      expect(err).toMatchObject({ reason: 'released' })
      await expect(h.wait()).rejects.toBeInstanceOf(SandboxTerminatedError)
      expect(await victim.kill()).toBe(true) // idempotent
      // A terminal on a dead sandbox is refused with a close code the SDK types.
      await expect(victim.pty.connect(h.pid)).rejects.toBeInstanceOf(SandboxTerminatedError)
      expect((await Sandbox.list(conn())).map((s) => s.sandboxId)).not.toContain(victim.sandboxId)
    })

    it('`await using` kills the sandbox at the end of the scope', async () => {
      let id = ''
      {
        await using scoped = await create()
        id = scoped.sandboxId
        expect((await scoped.commands.run('echo scoped')).stdout).toBe('scoped\n')
      }
      const info = await Sandbox.getInfo(id, conn())
      expect(['recycling', 'destroyed']).toContain(info.state)
    })

    it('Sandbox.kill by id, and an unknown id', async () => {
      const s = await create()
      extra.push(s)
      expect(await Sandbox.kill(s.sandboxId, conn())).toBe(true)
      expect(await Sandbox.kill('sb-0000000000000000', conn())).toBe(false)
    })
  })
})

describe.skipIf(live)('live tests', () => {
  it.skip('are skipped: set SBX_LIVE_BASE_URL and SBX_LIVE_API_KEY, or run scripts/live-test.sh', () => {})
})
