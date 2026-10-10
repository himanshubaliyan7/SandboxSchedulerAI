/** Files: streamed upload and download, directory listing, mkdir, remove. */

import type { SandboxContext } from './commands.js'
import { ConnectionError, SandboxError } from './errors.js'
import type { CallOptions } from './http.js'
import { type EntryInfo, entryFromWire, list } from './types.js'
import { concatBytes, utf8 } from './util.js'

export interface FileReadOptions extends CallOptions {
  /**
   * `text` (default): a UTF-8 string. `bytes`: a Uint8Array. `stream`: a
   * ReadableStream of Uint8Array chunks, for files too large to hold in
   * memory.
   */
  format?: 'text' | 'bytes' | 'stream'
}

export interface FileWriteOptions extends CallOptions {
  /** Octal permission bits, as a string (`'0755'`) or a number (`0o755`). Default `0644`. */
  mode?: string | number
}

export type FileData = string | Uint8Array | ArrayBuffer | Blob | ReadableStream<Uint8Array>

function filesPath(sandboxId: string, action?: string): string {
  const p = `/v1/sandboxes/${encodeURIComponent(sandboxId)}/files`
  return action ? `${p}/${action}` : p
}

function modeString(mode: string | number): string {
  return typeof mode === 'number' ? `0${(mode & 0o777).toString(8).padStart(3, '0')}` : mode
}

/** `sandbox.files`. Paths are absolute, or relative to the work directory `/workspace`. */
export class Files {
  readonly #ctx: SandboxContext

  /** @internal */
  constructor(ctx: SandboxContext) {
    this.#ctx = ctx
  }

  /** Downloads a file. */
  read(path: string, opts?: FileReadOptions & { format?: 'text' }): Promise<string>
  read(path: string, opts: FileReadOptions & { format: 'bytes' }): Promise<Uint8Array>
  read(path: string, opts: FileReadOptions & { format: 'stream' }): Promise<ReadableStream<Uint8Array>>
  read(path: string, opts?: FileReadOptions): Promise<string | Uint8Array | ReadableStream<Uint8Array>>
  async read(path: string, opts: FileReadOptions = {}): Promise<string | Uint8Array | ReadableStream<Uint8Array>> {
    const api = this.#ctx.api
    // The body is streamed in every format, so the request deadline only
    // covers the wait for the headers and a large file is not cut off by it.
    const res = await api.request('GET', filesPath(this.#ctx.sandboxId), {
      auth: 'data',
      retry: 'idempotent',
      stream: true,
      query: { path },
      headers: { Accept: 'application/octet-stream' },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    const body = res.body ?? new Response(new Uint8Array(0)).body
    if (!body) throw new ConnectionError('the server answered a download without a body', { code: 'bad_stream' })
    const declared = res.headers.get('content-length')
    // With a content coding (a compressing proxy) the length describes the encoded body, not what fetch hands us.
    const encoded = (res.headers.get('content-encoding') ?? 'identity') !== 'identity'
    const expected = !encoded && declared !== null && /^\d+$/.test(declared) ? Number(declared) : undefined

    if (opts.format === 'stream') {
      // Pass the bytes through while checking that the file arrives whole.
      let received = 0
      const check = new TransformStream<Uint8Array, Uint8Array>({
        transform(chunk, controller) {
          received += chunk.byteLength
          controller.enqueue(chunk)
        },
        flush() {
          api.release(res)
          if (expected !== undefined && received !== expected) throw incomplete(path, received, expected)
        },
      })
      return body.pipeThrough(check)
    }

    const chunks: Uint8Array[] = []
    let received = 0
    const reader = body.getReader()
    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        chunks.push(value)
        received += value.byteLength
      }
    } catch (e) {
      if (opts.signal?.aborted) throw e
      throw new ConnectionError(`the download of ${path} was interrupted after ${received} bytes`, {
        code: 'download_interrupted',
        cause: e,
      })
    } finally {
      api.release(res)
    }
    // The server aborts the connection when a transfer fails midway; a
    // short body is never a complete file.
    if (expected !== undefined && received !== expected) throw incomplete(path, received, expected)
    const bytes = concatBytes(chunks, received)
    return opts.format === 'bytes' ? bytes : new TextDecoder().decode(bytes)
  }

  /**
   * Uploads a file, creating parent directories and replacing an existing
   * file atomically. `data` may be a string, bytes, a Blob/File or a
   * ReadableStream; streams and Blobs are sent without being buffered.
   *
   * A ReadableStream body needs a runtime with streaming request bodies
   * (Node, Deno, Bun, workerd; in browsers only Chromium over HTTP/2) and
   * cannot be retried automatically.
   */
  async write(path: string, data: FileData, opts: FileWriteOptions = {}): Promise<void> {
    const body = typeof data === 'string' ? utf8(data) : data instanceof ArrayBuffer ? new Uint8Array(data) : data
    await this.#ctx.api.empty('PUT', filesPath(this.#ctx.sandboxId), {
      auth: 'data',
      retry: 'idempotent',
      query: { path, mode: opts.mode === undefined ? undefined : modeString(opts.mode) },
      body,
      signal: opts.signal,
      // An upload takes as long as it takes; only an explicit deadline applies.
      timeoutMs: opts.requestTimeoutMs ?? 0,
    })
  }

  /** Lists a directory (not recursive), sorted by name. Default: the work directory. */
  async list(path = '.', opts: CallOptions = {}): Promise<EntryInfo[]> {
    const res = await this.#ctx.api.json<unknown>('GET', filesPath(this.#ctx.sandboxId, 'list'), {
      auth: 'data',
      retry: 'idempotent',
      query: { path },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return list(res, 'files').map(entryFromWire)
  }

  /** Whether a file or directory exists at `path`. */
  async exists(path: string, opts: CallOptions = {}): Promise<boolean> {
    // The API has no stat route. Opening the file and dropping the body
    // costs one request and transfers next to nothing.
    try {
      const res = await this.#ctx.api.request('GET', filesPath(this.#ctx.sandboxId), {
        auth: 'data',
        retry: 'idempotent',
        stream: true,
        query: { path },
        signal: opts.signal,
        timeoutMs: opts.requestTimeoutMs,
      })
      await res.body?.cancel().catch(() => {})
      this.#ctx.api.release(res)
      return true
    } catch (e) {
      if (e instanceof SandboxError) {
        if (e.code === 'file_not_found') return false
        if (e.code === 'is_directory') return true
      }
      throw e
    }
  }

  /** Creates a directory and any missing parents. Succeeds if it already exists. */
  async makeDir(path: string, opts: CallOptions = {}): Promise<void> {
    await this.#ctx.api.empty('POST', filesPath(this.#ctx.sandboxId, 'mkdir'), {
      auth: 'data',
      retry: 'idempotent',
      query: { path },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
  }

  /** Deletes a file, or a directory and everything in it. */
  async remove(path: string, opts: CallOptions = {}): Promise<void> {
    await this.#ctx.api.empty('DELETE', filesPath(this.#ctx.sandboxId), {
      auth: 'data',
      retry: 'idempotent',
      query: { path },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
  }
}

function incomplete(path: string, received: number, expected: number): ConnectionError {
  return new ConnectionError(`the download of ${path} is incomplete: ${received} of ${expected} bytes`, {
    code: 'download_interrupted',
  })
}
