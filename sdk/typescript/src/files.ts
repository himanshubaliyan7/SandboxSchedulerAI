/** Files: streamed upload and download, directory listing, mkdir, remove. */

import type { SandboxContext } from './commands.js'
import { ConnectionError, NotFoundError, SandboxError } from './errors.js'
import type { CallOptions } from './http.js'
import { type EntryInfo, entryFromWire, list, type WriteInfo } from './types.js'
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

/** One file for {@link Files.writeFiles}. */
export interface WriteEntry {
  path: string
  data: FileData
  /** Octal permission bits, as for {@link FileWriteOptions.mode}. */
  mode?: string | number
}

/** The work directory relative paths are resolved against. */
const WORKDIR = '/workspace'

/** The absolute, normalised form of a sandbox path, as the server resolves it. */
function absolutePath(path: string): string {
  const parts: string[] = []
  for (const part of (path.startsWith('/') ? path : `${WORKDIR}/${path}`).split('/')) {
    if (part === '' || part === '.') continue
    if (part === '..') parts.pop()
    else parts.push(part)
  }
  return `/${parts.join('/')}`
}

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
   *
   * Resolves with a {@link WriteInfo}: the file's name, its absolute
   * path and the number of bytes written (E2B's `WriteInfo`, plus `size`).
   */
  async write(path: string, data: FileData, opts: FileWriteOptions = {}): Promise<WriteInfo> {
    let body: Exclude<FileData, string | ArrayBuffer> =
      typeof data === 'string' ? utf8(data) : data instanceof ArrayBuffer ? new Uint8Array(data) : data
    let size = 0
    if (body instanceof Uint8Array) size = body.byteLength
    else if (body instanceof Blob) size = body.size
    else {
      // A stream's length is only known once it has been sent.
      body = body.pipeThrough(
        new TransformStream<Uint8Array, Uint8Array>({
          transform(chunk, controller) {
            size += chunk.byteLength
            controller.enqueue(chunk)
          },
        }),
      )
    }
    await this.#ctx.api.empty('PUT', filesPath(this.#ctx.sandboxId), {
      auth: 'data',
      retry: 'idempotent',
      query: { path, mode: opts.mode === undefined ? undefined : modeString(opts.mode) },
      body,
      signal: opts.signal,
      // An upload takes as long as it takes; only an explicit deadline applies.
      timeoutMs: opts.requestTimeoutMs ?? 0,
    })
    const abs = absolutePath(path)
    return { name: abs.slice(abs.lastIndexOf('/') + 1), type: 'file', path: abs, size }
  }

  /**
   * Uploads several files, one after another, and resolves with one
   * {@link WriteInfo} per file, in order (E2B: `write([...])`; Python:
   * `write_files`). It stops at the first failure: the files before it
   * have been written, the ones after it have not.
   */
  async writeFiles(files: Iterable<WriteEntry>, opts: CallOptions = {}): Promise<WriteInfo[]> {
    const out: WriteInfo[] = []
    for (const f of files) {
      out.push(await this.write(f.path, f.data, f.mode === undefined ? opts : { ...opts, mode: f.mode }))
    }
    return out
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

  /**
   * Describes the file or directory at `path` (E2B: `getInfo`). A symbolic
   * link is followed. Rejects with `NotFoundError` (`file_not_found`) if
   * nothing is there.
   *
   * Uses the server's stat route; against a server (or a sandbox agent)
   * that predates it, falls back to listing the parent directory.
   */
  async getInfo(path: string, opts: CallOptions = {}): Promise<EntryInfo> {
    try {
      const res = await this.#ctx.api.json<unknown>('GET', filesPath(this.#ctx.sandboxId, 'stat'), {
        auth: 'data',
        retry: 'idempotent',
        query: { path },
        signal: opts.signal,
        timeoutMs: opts.requestTimeoutMs,
      })
      return entryFromWire(res)
    } catch (e) {
      if (!statUnsupported(e)) throw e
    }
    const clean = path.replace(/\/+$/, '')
    const cut = clean.lastIndexOf('/')
    const name = cut === -1 ? clean : clean.slice(cut + 1)
    if (name === '' || name === '.' || name === '..') {
      // The path names a directory by itself: listing it proves it exists.
      await this.list(path, opts)
      return { name: name || '/', path, type: 'dir', size: 0, mode: '0755' }
    }
    const parent = cut === -1 ? '.' : cut === 0 ? '/' : clean.slice(0, cut)
    const entry = (await this.list(parent, opts)).find((e) => e.name === name)
    if (!entry) throw new NotFoundError(`${path}: no such file or directory`, { code: 'file_not_found', status: 404 })
    return entry
  }

  /** Alias of {@link Files.getInfo}. */
  stat(path: string, opts: CallOptions = {}): Promise<EntryInfo> {
    return this.getInfo(path, opts)
  }

  /** Whether a file or directory exists at `path`. */
  async exists(path: string, opts: CallOptions = {}): Promise<boolean> {
    try {
      await this.getInfo(path, opts)
      return true
    } catch (e) {
      if (e instanceof SandboxError && e.code === 'file_not_found') return false
      // A component of the path is a file: nothing can exist below it.
      if (e instanceof SandboxError && e.code === 'conflict') return false
      throw e
    }
  }

  /**
   * Creates a directory and any missing parents. Resolves with `true` if
   * this call created it and `false` if it already existed (as in E2B).
   *
   * A server that predates the distinction always answers "already
   * existed", so there the result is always `false`.
   */
  async makeDir(path: string, opts: CallOptions = {}): Promise<boolean> {
    const res = await this.#ctx.api.request('POST', filesPath(this.#ctx.sandboxId, 'mkdir'), {
      auth: 'data',
      retry: 'idempotent',
      query: { path },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    await res.body?.cancel().catch(() => {})
    return res.status === 201
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

/** True when the answer means "this server or this sandbox's agent has no stat route". */
function statUnsupported(e: unknown): boolean {
  if (!(e instanceof SandboxError)) return false
  // An old server: 404 `not_found` ("no such route"); `file_not_found` is the real answer of a new one.
  if (e.status === 404 && e.code !== 'file_not_found') return true
  if (e.status === 405) return true
  // A new server in front of an old agent.
  return e.code === 'not_implemented' || e.code === 'no_route'
}
