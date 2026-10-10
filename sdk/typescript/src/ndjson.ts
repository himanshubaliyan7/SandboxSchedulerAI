/**
 * NDJSON reader for the command stream.
 *
 * Lines are split on the byte 0x0A *before* any text decoding, so a network
 * chunk boundary can fall anywhere — in the middle of a line, of a base64
 * payload or of a multi-byte UTF-8 character — without corrupting anything.
 */

import { ConnectionError } from './errors.js'
import { abortReason, concatBytes } from './util.js'

export interface NdjsonOptions {
  /**
   * Fail with a ConnectionError when no byte arrives for this long. The
   * server sends a keepalive line every 25 s, so silence much longer than
   * that means the connection is dead. 0 disables.
   */
  idleTimeoutMs?: number
  signal?: AbortSignal
}

const NEWLINE = 0x0a

/**
 * Yields one parsed JSON value per complete line. An incomplete last line
 * (the connection was cut mid-line) is dropped, not parsed: the caller
 * resumes from the last complete event. A complete line that is not JSON
 * is a ConnectionError.
 */
export async function* readNdjson(body: ReadableStream<Uint8Array>, opts: NdjsonOptions = {}): AsyncGenerator<unknown, void, void> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  const idleMs = opts.idleTimeoutMs ?? 0
  const signal = opts.signal
  let pending: Uint8Array[] = []
  let pendingBytes = 0

  const parse = (line: Uint8Array): unknown => {
    const text = decoder.decode(line)
    if (text.trim() === '') return undefined
    try {
      return JSON.parse(text) as unknown
    } catch (e) {
      throw new ConnectionError('the event stream contained a line that is not JSON', { code: 'bad_stream', cause: e })
    }
  }

  const read = (): Promise<ReadableStreamReadResult<Uint8Array>> => {
    if (idleMs <= 0 && !signal) return reader.read()
    return new Promise((resolve, reject) => {
      let timer: ReturnType<typeof setTimeout> | undefined
      const done = (): void => {
        if (timer !== undefined) clearTimeout(timer)
        signal?.removeEventListener('abort', onAbort)
      }
      const onAbort = (): void => {
        done()
        reject(abortReason(signal as AbortSignal))
      }
      if (signal?.aborted) {
        onAbort()
        return
      }
      signal?.addEventListener('abort', onAbort, { once: true })
      if (idleMs > 0) {
        timer = setTimeout(() => {
          done()
          reject(new ConnectionError(`the event stream was silent for ${idleMs} ms`, { code: 'stream_idle' }))
        }, idleMs)
      }
      reader.read().then(
        (r) => {
          done()
          resolve(r)
        },
        (e: unknown) => {
          done()
          reject(e)
        },
      )
    })
  }

  try {
    for (;;) {
      const { done, value } = await read()
      if (done) return
      if (!value || value.byteLength === 0) continue
      let start = 0
      for (;;) {
        const nl = value.indexOf(NEWLINE, start)
        if (nl === -1) break
        let line = value.subarray(start, nl)
        if (pendingBytes > 0) {
          pending.push(line)
          line = concatBytes(pending, pendingBytes + line.byteLength)
          pending = []
          pendingBytes = 0
        }
        start = nl + 1
        const ev = parse(line)
        if (ev !== undefined) yield ev
      }
      if (start < value.byteLength) {
        const rest = value.subarray(start)
        pending.push(rest)
        pendingBytes += rest.byteLength
      }
    }
  } finally {
    // Stops the download if the consumer leaves early; a no-op after EOF.
    reader.cancel().catch(() => {})
  }
}
