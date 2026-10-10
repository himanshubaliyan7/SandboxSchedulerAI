import { describe, expect, it } from 'vitest'
import { ConflictError, ConnectionError, NotFoundError, SandboxTerminatedError, ValidationError } from '../src/index.js'
import { sendError } from './helpers/mock-server.js'
import { useMockServer } from './helpers/setup.js'

const { server, create } = useMockServer()

async function drain(stream: ReadableStream<Uint8Array>): Promise<Buffer> {
  const chunks: Uint8Array[] = []
  const reader = stream.getReader()
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    chunks.push(value)
  }
  return Buffer.concat(chunks)
}

function binary(n: number): Uint8Array {
  const b = new Uint8Array(n)
  for (let i = 0; i < n; i++) b[i] = (i * 31 + 7) % 256
  return b
}

describe('files.write', () => {
  it('uploads a string as UTF-8 with the default mode', async () => {
    const sb = await create()
    await sb.files.write('notes/héllo.txt', 'grüße 🚀')
    const f = server().sandbox(sb.sandboxId).files.get('/workspace/notes/héllo.txt')
    expect(f?.data.toString()).toBe('grüße 🚀')
    const put = server().requestsTo('PUT', '/files')[0]
    expect(put?.query.get('path')).toBe('notes/héllo.txt')
    expect(put?.query.has('mode')).toBe(false)
    expect(put?.headers['content-type']).toBe('application/octet-stream')
    expect(put?.headers['x-access-token']).toMatch(/^sbt_/)
  })

  it('uploads bytes, an ArrayBuffer and a Blob unchanged', async () => {
    const sb = await create()
    const data = binary(70_000)
    await sb.files.write('/tmp/a.bin', data)
    await sb.files.write('/tmp/b.bin', data.buffer.slice(0) as ArrayBuffer)
    await sb.files.write('/tmp/c.bin', new Blob([data as Uint8Array<ArrayBuffer>]))
    const files = server().sandbox(sb.sandboxId).files
    for (const name of ['a', 'b', 'c']) expect(files.get(`/tmp/${name}.bin`)?.data.equals(Buffer.from(data))).toBe(true)
  })

  it('streams a ReadableStream body without buffering it', async () => {
    const sb = await create()
    const parts = [binary(10_000), binary(1), binary(50_000)]
    let i = 0
    const stream = new ReadableStream<Uint8Array>({
      pull(c) {
        const p = parts[i++]
        if (p) c.enqueue(p)
        else c.close()
      },
    })
    await sb.files.write('big/stream.bin', stream)
    const got = server().sandbox(sb.sandboxId).files.get('/workspace/big/stream.bin')?.data
    expect(got?.equals(Buffer.concat(parts))).toBe(true)
    const put = server().requestsTo('PUT', '/files')[0]
    expect(put?.headers['transfer-encoding']).toBe('chunked')
  })

  it('sends the mode as octal, from a string or a number', async () => {
    const sb = await create()
    await sb.files.write('run.sh', '#!/bin/sh\n', { mode: 0o755 })
    await sb.files.write('secret', 'x', { mode: '0600' })
    const modes = server()
      .requestsTo('PUT', '/files')
      .map((r) => r.query.get('mode'))
    expect(modes).toEqual(['0755', '0600'])
    expect(server().sandbox(sb.sandboxId).files.get('/workspace/run.sh')?.mode).toBe('0755')
  })

  it('maps upload errors', async () => {
    const sb = await create()
    await sb.files.makeDir('adir')
    await expect(sb.files.write('adir', 'x')).rejects.toBeInstanceOf(ConflictError)
    await expect(sb.files.write('f', 'x', { mode: '999' })).rejects.toBeInstanceOf(ValidationError)
    server().intercept = (req, res) => {
      if (req.method !== 'PUT') return false
      sendError(res, 507, 'no_space', 'no space left on device')
      return true
    }
    await expect(sb.files.write('f', 'x')).rejects.toMatchObject({ name: 'NotEnoughSpaceError', code: 'no_space', status: 507 })
  })
})

describe('files.read', () => {
  it('reads text by default, bytes and a stream on request', async () => {
    const sb = await create()
    const data = binary(200_000)
    await sb.files.write('data.bin', data)
    await sb.files.write('t.txt', 'ünïcödé')
    expect(await sb.files.read('t.txt')).toBe('ünïcödé')
    expect(await sb.files.read('t.txt', { format: 'text' })).toBe('ünïcödé')
    const bytes = await sb.files.read('data.bin', { format: 'bytes' })
    expect(bytes).toBeInstanceOf(Uint8Array)
    expect(Buffer.from(bytes).equals(Buffer.from(data))).toBe(true)
    const stream = await sb.files.read('data.bin', { format: 'stream' })
    expect(stream).toBeInstanceOf(ReadableStream)
    expect((await drain(stream)).equals(Buffer.from(data))).toBe(true)
  })

  it('reads an empty file', async () => {
    const sb = await create()
    await sb.files.write('empty', '')
    expect(await sb.files.read('empty')).toBe('')
    expect((await sb.files.read('empty', { format: 'bytes' })).byteLength).toBe(0)
  })

  it('maps a missing file and a directory', async () => {
    const sb = await create()
    const err = await sb.files.read('nope.txt').catch((e) => e)
    expect(err).toBeInstanceOf(NotFoundError)
    expect(err.code).toBe('file_not_found')
    await sb.files.makeDir('d')
    await expect(sb.files.read('d')).rejects.toMatchObject({ code: 'is_directory' })
  })

  it('rejects a download that ends short of Content-Length, in every format', async () => {
    const sb = await create()
    server().intercept = (req, res) => {
      if (req.method !== 'GET' || !req.path.endsWith('/files')) return false
      res.writeHead(200, { 'Content-Type': 'application/octet-stream', 'Content-Length': '1000' })
      res.write(Buffer.alloc(400))
      setTimeout(() => res.socket?.destroy(), 10)
      return true
    }
    await expect(sb.files.read('x', { format: 'bytes' })).rejects.toBeInstanceOf(ConnectionError)
    await expect(sb.files.read('x')).rejects.toMatchObject({ code: 'download_interrupted' })
    const stream = await sb.files.read('x', { format: 'stream' })
    await expect(drain(stream)).rejects.toBeTruthy()
  })

  it('can be aborted mid-download', async () => {
    const sb = await create()
    const ac = new AbortController()
    server().intercept = (req, res) => {
      if (req.method !== 'GET' || !req.path.endsWith('/files')) return false
      res.writeHead(200, { 'Content-Length': '1000' })
      res.write(Buffer.alloc(10))
      setTimeout(() => ac.abort(), 10)
      return true
    }
    await expect(sb.files.read('x', { format: 'bytes', signal: ac.signal })).rejects.toMatchObject({ name: 'AbortError' })
  })
})

describe('files.list / exists / makeDir / remove', () => {
  it('lists a directory with typed entries', async () => {
    const sb = await create()
    await sb.files.write('b.txt', 'bb')
    await sb.files.write('a.txt', 'a', { mode: '0600' })
    await sb.files.makeDir('sub/deep')
    const entries = await sb.files.list()
    expect(entries.map((e) => [e.name, e.type, e.size, e.mode])).toEqual([
      ['a.txt', 'file', 1, '0600'],
      ['b.txt', 'file', 2, '0644'],
      ['sub', 'dir', 0, '0755'],
    ])
    expect(entries[0]?.path).toBe('/workspace/a.txt')
    expect(entries[0]?.modifiedTime).toBeInstanceOf(Date)
    expect(server().requestsTo('GET', '/files/list')[0]?.query.get('path')).toBe('.')
    expect((await sb.files.list('sub')).map((e) => e.name)).toEqual(['deep'])
    await expect(sb.files.list('missing')).rejects.toBeInstanceOf(NotFoundError)
  })

  it('exists() is true for files and directories and false for nothing', async () => {
    const sb = await create()
    await sb.files.write('f.txt', 'x'.repeat(100_000))
    await sb.files.makeDir('d')
    expect(await sb.files.exists('f.txt')).toBe(true)
    expect(await sb.files.exists('d')).toBe(true)
    expect(await sb.files.exists('ghost')).toBe(false)
  })

  it('exists() does not swallow other errors', async () => {
    const sb = await create()
    server().terminate(sb.sandboxId, 'idle_timeout')
    await expect(sb.files.exists('f')).rejects.toBeInstanceOf(SandboxTerminatedError)
  })

  it('makeDir() creates parents and is idempotent; remove() deletes files and trees', async () => {
    const sb = await create()
    await sb.files.makeDir('a/b/c')
    await sb.files.makeDir('a/b/c')
    await sb.files.write('a/b/c/f.txt', 'x')
    expect(server().requestsTo('POST', '/files/mkdir')[0]?.query.get('path')).toBe('a/b/c')
    await sb.files.remove('a/b/c/f.txt')
    expect(await sb.files.exists('a/b/c/f.txt')).toBe(false)
    await sb.files.remove('a')
    expect(await sb.files.exists('a/b')).toBe(false)
    await expect(sb.files.remove('a')).rejects.toMatchObject({ code: 'file_not_found' })
  })
})
