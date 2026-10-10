import { describe, expect, it } from 'vitest'
import { ConnectionError } from '../src/errors.js'
import { readNdjson } from '../src/ndjson.js'
import { decodeBase64 } from '../src/util.js'

/** A body that delivers exactly the given network chunks. */
function body(chunks: Uint8Array[], opts: { hangAtEnd?: boolean } = {}): ReadableStream<Uint8Array> {
  let i = 0
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      const c = chunks[i++]
      if (c) controller.enqueue(c)
      else if (!opts.hangAtEnd) controller.close()
    },
  })
}

function splitAt(bytes: Uint8Array, ...cuts: number[]): Uint8Array[] {
  const out: Uint8Array[] = []
  let prev = 0
  for (const c of [...cuts, bytes.length]) {
    out.push(bytes.slice(prev, c))
    prev = c
  }
  return out
}

async function collect(stream: ReadableStream<Uint8Array>): Promise<unknown[]> {
  const out: unknown[] = []
  for await (const ev of readNdjson(stream)) out.push(ev)
  return out
}

const enc = new TextEncoder()

describe('readNdjson', () => {
  it('parses one event per line and skips blank lines', async () => {
    const text = '{"type":"start","offset":0}\n\n{"type":"exit","offset":3}\n'
    expect(await collect(body([enc.encode(text)]))).toEqual([
      { type: 'start', offset: 0 },
      { type: 'exit', offset: 3 },
    ])
  })

  it('reassembles a line split across every possible chunk boundary', async () => {
    const payload = Buffer.from('héllo wörld ✓ 🚀\n').toString('base64')
    const text = `{"type":"stdout","offset":0,"data":"${payload}"}\n{"type":"exit","offset":25,"exit":{"exit_code":0,"reason":"exited"}}\n`
    const bytes = enc.encode(text)
    for (let cut = 1; cut < bytes.length; cut++) {
      const events = (await collect(body(splitAt(bytes, cut)))) as { type: string; data?: string }[]
      expect(events).toHaveLength(2)
      expect(new TextDecoder().decode(decodeBase64(events[0]?.data ?? ''))).toBe('héllo wörld ✓ 🚀\n')
    }
  })

  it('handles a chunk boundary inside a multi-byte character of the JSON text itself', async () => {
    // Raw (non-base64) UTF-8 in a JSON string, cut in the middle of the 4-byte emoji.
    const bytes = enc.encode('{"type":"error","offset":0,"error":{"code":"x","message":"désolé 🚀"}}\n')
    const emoji = bytes.indexOf(0xf0)
    const events = (await collect(body(splitAt(bytes, emoji + 2)))) as { error: { message: string } }[]
    expect(events[0]?.error.message).toBe('désolé 🚀')
  })

  it('delivers many lines from one chunk and one line from many chunks', async () => {
    const lines = Array.from({ length: 50 }, (_, i) => `{"n":${i}}\n`).join('')
    const bytes = enc.encode(lines)
    const oneByOne = Array.from(bytes, (b) => Uint8Array.of(b))
    expect(await collect(body([bytes]))).toHaveLength(50)
    const events = (await collect(body(oneByOne))) as { n: number }[]
    expect(events.map((e) => e.n)).toEqual(Array.from({ length: 50 }, (_, i) => i))
  })

  it('drops an incomplete last line instead of parsing it', async () => {
    const events = await collect(body([enc.encode('{"type":"stdout","offset":0}\n{"type":"std')]))
    expect(events).toEqual([{ type: 'stdout', offset: 0 }])
  })

  it('reports a complete line that is not JSON as a ConnectionError', async () => {
    await expect(collect(body([enc.encode('<html>bad gateway</html>\n')]))).rejects.toBeInstanceOf(ConnectionError)
  })

  it('fails with stream_idle when the stream goes silent', async () => {
    const it = readNdjson(body([enc.encode('{"a":1}\n')], { hangAtEnd: true }), { idleTimeoutMs: 30 })
    expect((await it.next()).value).toEqual({ a: 1 })
    await expect(it.next()).rejects.toMatchObject({ code: 'stream_idle' })
  })

  it('stops reading when the signal aborts', async () => {
    const ac = new AbortController()
    const it = readNdjson(body([], { hangAtEnd: true }), { signal: ac.signal })
    const next = it.next()
    ac.abort()
    await expect(next).rejects.toMatchObject({ name: 'AbortError' })
  })
})

describe('decodeBase64', () => {
  it('decodes padded base64 of every length class', () => {
    for (const s of ['', 'a', 'ab', 'abc', 'abcd', '\u0000ÿ binary']) {
      const bytes = Buffer.from(s, 'latin1')
      expect(Buffer.from(decodeBase64(bytes.toString('base64')))).toEqual(bytes)
    }
  })

  it('falls back to atob when Uint8Array.fromBase64 is missing', () => {
    const ctor = Uint8Array as unknown as { fromBase64?: unknown }
    const native = ctor.fromBase64
    try {
      ctor.fromBase64 = undefined
      expect(Array.from(decodeBase64('AP8Q'))).toEqual([0, 255, 16])
    } finally {
      ctor.fromBase64 = native
    }
  })
})
