/**
 * Interactive terminals over the WebSocket endpoint.
 *
 * Wire format (see api/openapi.yaml): every message is binary; the first
 * byte is the channel, the rest the payload. The handle counts output bytes
 * from the server's offset frames, acknowledges them for flow control, and
 * reattaches with `from=<count>` if the socket drops.
 */

import { _commandsPath, _signalCommand, _writeStdin, type SandboxContext } from './commands.js'
import { ConnectionError, type ErrorBody, errorFromBody, SandboxError, TimeoutError } from './errors.js'
import type { CallOptions, WebSocketLike } from './http.js'
import { commandInfoFromWire, type ExitReason, exitFromWire, type Signal } from './types.js'
import { abortReason, backoffDelay, type Deferred, deferred, isBrowser, linkedAbort, sleep, utf8 } from './util.js'

/** Channel bytes of the WebSocket framing. */
export const Channel = {
  Stdin: 0,
  Stdout: 1,
  Stderr: 2,
  Status: 3,
  Resize: 4,
  Signal: 5,
  Ack: 6,
  Ping: 7,
  Pong: 8,
  Offset: 9,
} as const

/** The server pauses the command above this many unacknowledged bytes ... */
export const FLOW_HIGH_WATERMARK = 100_000
/** ... and resumes it below this many, so acknowledgements must come in smaller steps. */
export const FLOW_LOW_WATERMARK = 10_000
/** Largest output payload of one server frame (the frame is one channel byte longer). */
export const MAX_SERVER_FRAME_BYTES = 32_768
/**
 * Largest payload of one client frame the server accepts: 1 MiB, after
 * the channel byte (so the frame may be 1048577 bytes). A larger frame is
 * answered with close code 1009.
 */
export const MAX_CLIENT_FRAME_PAYLOAD_BYTES = 1_048_576

/**
 * Application close codes of the WebSocket route (see "How a WebSocket
 * ends" in api/openapi.yaml). The close reason is the API error code.
 */
export const CloseCode = {
  SandboxTerminated: 4000,
  Unauthorized: 4001,
  ShuttingDown: 4002,
  AgentUnavailable: 4003,
  NotFound: 4004,
  BadRequest: 4005,
  RateLimited: 4029,
  Error: 4099,
} as const

/** The API error code a close code stands for when the reason is missing. */
const CLOSE_DEFAULT_REASON: Record<number, string> = {
  [CloseCode.SandboxTerminated]: 'sandbox_terminated',
  [CloseCode.Unauthorized]: 'unauthorized',
  [CloseCode.ShuttingDown]: 'shutting_down',
  [CloseCode.AgentUnavailable]: 'agent_unavailable',
  [CloseCode.NotFound]: 'process_not_found',
  [CloseCode.BadRequest]: 'bad_request',
  [CloseCode.RateLimited]: 'rate_limited',
  [CloseCode.Error]: 'internal',
}

/** Close codes after which reattaching cannot succeed. */
const FINAL_CLOSE_CODES = new Set<number>([CloseCode.SandboxTerminated, CloseCode.Unauthorized, CloseCode.NotFound, CloseCode.BadRequest])

/** Error codes of a status frame that end one connection but not the session. */
const TRANSIENT_STATUS_CODES = new Set(['agent_unavailable', 'shutting_down', 'rate_limited', 'timeout', 'internal'])

/**
 * The typed error for an application close code, or undefined when the
 * close is one to reattach after (or not an application code at all).
 */
export function errorFromClose(code: number | undefined, reason: string | undefined): SandboxError | undefined {
  if (code === undefined || !FINAL_CLOSE_CODES.has(code)) return undefined
  const token = reason && /^[a-z_]{1,64}$/.test(reason) ? reason : (CLOSE_DEFAULT_REASON[code] ?? 'unknown')
  return errorFromBody(undefined, { code: token, message: `the server closed the connection: ${token} (close code ${code})` })
}

const DEFAULT_ACK_EVERY_BYTES = 4_096
// One stdin frame: far below MAX_CLIENT_FRAME_PAYLOAD_BYTES, so that
// keystrokes interleave well with a large paste.
const STDIN_FRAME_BYTES = 64 * 1024
// Input typed while the socket is reconnecting is kept up to this size.
const MAX_PENDING_INPUT_BYTES = 1 << 20

export interface PtySize {
  cols: number
  rows: number
}

export interface PtyOutputOptions {
  /**
   * Called with terminal output. May return a promise: the next chunk is
   * not delivered, and the bytes are not acknowledged to the server, until
   * it settles — which is what throttles a command that prints faster than
   * the terminal can render.
   */
  onData?: (data: Uint8Array) => unknown
  /** Called when output was lost (the client fell behind the server's 1 MiB replay buffer). */
  onTruncated?: (info: { offset: number; dropped: number }) => unknown
  /**
   * Acknowledge output so that the server pauses a command whose output is
   * not being consumed. Default true.
   */
  flowControl?: boolean
  /** Acknowledge after at most this many bytes. Must stay below 10000 (the server's low watermark). Default 4096. */
  ackEveryBytes?: number
}

export interface PtyCreateOptions extends PtyOutputOptions, CallOptions {
  /** Terminal width. Default 80. */
  cols?: number
  /** Terminal height. Default 24. */
  rows?: number
  /** Program to run on the terminal. Default `/bin/sh`. No shell is involved in starting it. */
  cmd?: string
  args?: string[]
  /** Environment for the session. `TERM` defaults to `xterm-256color`. */
  envs?: Record<string, string>
  cwd?: string
  /** Wall-clock limit of the session, enforced by the server. Default 0: none. */
  timeoutMs?: number
  tag?: string
}

export interface PtyConnectOptions extends PtyOutputOptions, CallOptions {
  /** Output offset to start from. Default 0: replay what the server still buffers, then follow live. */
  from?: number
}

export interface PtyResult {
  pid: string
  exitCode: number
  reason: ExitReason
  signal?: string
  truncated: boolean
  droppedBytes: number
}

interface MessageEventLike {
  data: unknown
}
interface CloseEventLike {
  code?: number
  reason?: string
}

function frame(channel: number, payload: Uint8Array): Uint8Array {
  const out = new Uint8Array(payload.byteLength + 1)
  out[0] = channel
  out.set(payload, 1)
  return out
}

function jsonFrame(channel: number, value: unknown): Uint8Array {
  return frame(channel, utf8(JSON.stringify(value)))
}

function toBytes(data: unknown): Uint8Array | undefined {
  if (data instanceof ArrayBuffer) return new Uint8Array(data)
  if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
  return undefined // text frames are not part of the protocol
}

const OPEN = 1

/**
 * A terminal session (or any command attached over the WebSocket).
 *
 * `sendInput` and `resize` are synchronous and safe to call from UI event
 * handlers; input sent while the socket is reconnecting is queued.
 */
export class PtyHandle {
  readonly pid: string
  readonly #ctx: SandboxContext
  readonly #opts: PtyOutputOptions
  readonly #abort: AbortController
  readonly #disposeAbort: () => void
  readonly #done: Deferred<PtyResult> = deferred()
  readonly #ready: Deferred<void> = deferred()
  readonly #ackEvery: number
  #socket: WebSocketLike | undefined
  #offset: number
  #acked: number
  #dropped = 0
  #truncated = false
  #failures = 0
  #disconnected = false
  // Frames waiting for an open socket.
  #pending: Uint8Array[] = []
  #pendingBytes = 0
  // Serialises the handling of incoming frames (onData may be async).
  #chain: Promise<void> = Promise.resolve()

  /** @internal */
  constructor(ctx: SandboxContext, pid: string, from: number, opts: PtyOutputOptions & CallOptions) {
    this.#ctx = ctx
    this.pid = pid
    this.#opts = opts
    this.#offset = from
    this.#acked = from
    this.#ackEvery = Math.max(1, Math.min(opts.ackEveryBytes ?? DEFAULT_ACK_EVERY_BYTES, FLOW_LOW_WATERMARK - 1))
    const linked = linkedAbort(opts.signal)
    this.#abort = linked.controller
    this.#disposeAbort = linked.dispose
    this.#abort.signal.addEventListener(
      'abort',
      () => {
        const reason = this.#disconnected
          ? new ConnectionError('disconnected from the terminal', { code: 'disconnected' })
          : abortReason(this.#abort.signal)
        this.#finish(undefined, reason)
      },
      { once: true },
    )
  }

  /** @internal Opens the socket; resolves when it is connected. */
  async open(): Promise<this> {
    if (this.#abort.signal.aborted) throw abortReason(this.#abort.signal)
    this.#connect()
    await this.#ready.promise
    return this
  }

  /** Offset of the next output byte: what has been received so far. */
  get offset(): number {
    return this.#offset
  }

  /** Sends keystrokes (or any bytes) to the terminal. `\x03` is Ctrl+C, `\x04` is Ctrl+D. */
  sendInput(data: string | Uint8Array): void {
    const bytes = typeof data === 'string' ? utf8(data) : data
    for (let at = 0; at < bytes.byteLength; at += STDIN_FRAME_BYTES) {
      this.#send(frame(Channel.Stdin, bytes.subarray(at, at + STDIN_FRAME_BYTES)), true)
    }
  }

  /** Tells the terminal its new size. */
  resize(size: PtySize): void {
    this.#send(jsonFrame(Channel.Resize, { cols: Math.floor(size.cols), rows: Math.floor(size.rows) }), true)
  }

  /** Sends a signal to the session's process group over the socket (no round trip). */
  sendSignal(signal: Signal): void {
    this.#send(jsonFrame(Channel.Signal, { signal }), true)
  }

  /** Kills the session (default SIGKILL). False if it had already exited. */
  kill(signal: Signal = 'SIGKILL', opts: CallOptions = {}): Promise<boolean> {
    return _signalCommand(this.#ctx, this.pid, signal, opts)
  }

  /**
   * Resolves when the session's program exits. Unlike `commands.run()`, a
   * non-zero exit code is not an error here: an interactive shell exits
   * with the status of its last command.
   */
  wait(): Promise<PtyResult> {
    return this.#done.promise
  }

  /**
   * Closes the socket. The program keeps running; reattach with
   * `pty.connect(pid, { from: handle.offset })`.
   */
  disconnect(): void {
    if (this.#done.settled) return
    this.#disconnected = true
    this.#abort.abort()
  }

  // --- socket lifecycle ----------------------------------------------------

  #finish(result: PtyResult | undefined, error?: unknown): void {
    if (this.#done.settled) return
    this.#disposeAbort()
    if (result) this.#done.resolve(result)
    else this.#done.reject(error)
    if (!this.#ready.settled) {
      if (result) this.#ready.resolve()
      else this.#ready.reject(error)
    }
    this.#pending = []
    this.#pendingBytes = 0
    const socket = this.#socket
    this.#socket = undefined
    try {
      socket?.close(1000)
    } catch {
      // already closing
    }
  }

  #send(data: Uint8Array, queueIfDown: boolean): void {
    if (this.#done.settled) return
    const socket = this.#socket
    if (socket && socket.readyState === OPEN) {
      try {
        socket.send(data)
        return
      } catch {
        // The socket died under us; its close event triggers the reconnect.
      }
    }
    if (!queueIfDown) return
    if (this.#pendingBytes + data.byteLength > MAX_PENDING_INPUT_BYTES) return
    this.#pending.push(data)
    this.#pendingBytes += data.byteLength
  }

  #connect(): void {
    const { api, sandboxId } = this.#ctx
    // handshake_errors=close: a refused handshake arrives as a status
    // frame and a close code, which is the only form a browser can read.
    // (A server that predates the parameter ignores it.)
    const query = { from: this.#offset, handshake_errors: 'close' }
    const url = `${api.url(_commandsPath(sandboxId, this.pid, 'ws'), query).replace(/^http/, 'ws')}`
    let socket: WebSocketLike
    try {
      socket = openSocket(this.#ctx, url)
    } catch (e) {
      this.#finish(undefined, e)
      return
    }
    socket.binaryType = 'arraybuffer'
    this.#socket = socket
    let closed = false

    const onOpen = (): void => {
      if (this.#socket !== socket) return
      // Not "ready" yet: with handshake_errors=close a refused handshake
      // opens too. The first frame tells (see #handle).
      const queued = this.#pending
      this.#pending = []
      this.#pendingBytes = 0
      for (const f of queued) this.#send(f, true)
    }
    const onMessage = (ev: MessageEventLike): void => {
      const bytes = toBytes(ev.data)
      if (!bytes || bytes.byteLength === 0) return
      this.#chain = this.#chain.then(() => this.#handle(socket, bytes)).catch((e: unknown) => this.#finish(undefined, e))
    }
    const onClose = (ev: CloseEventLike): void => {
      if (closed) return
      closed = true
      socket.removeEventListener('open', onOpen as never)
      socket.removeEventListener('message', onMessage as never)
      socket.removeEventListener('close', onClose as never)
      socket.removeEventListener('error', onError as never)
      // Frames received before the close are handled first: the status frame may be among them.
      this.#chain = this.#chain.then(() => this.#lost(socket, ev))
    }
    const onError = (): void => {
      // Browsers and Node report a failed handshake as `error` (then `close`); make sure the loss is handled once.
      try {
        socket.close()
      } catch {
        // not open yet
      }
      onClose({ code: 1006, reason: '' })
    }
    socket.addEventListener('open', onOpen as never)
    socket.addEventListener('message', onMessage as never)
    socket.addEventListener('close', onClose as never)
    socket.addEventListener('error', onError as never)
  }

  async #handle(socket: WebSocketLike, msg: Uint8Array): Promise<void> {
    if (this.#done.settled || this.#socket !== socket) return
    const channel = msg[0]
    const payload = msg.subarray(1)
    // Any frame but a status frame means the session is attached (the
    // server's first frame is the offset frame).
    if (channel !== Channel.Status && !this.#ready.settled) this.#ready.resolve()
    switch (channel) {
      case Channel.Stdout:
      case Channel.Stderr: {
        if (payload.byteLength === 0) return
        this.#offset += payload.byteLength
        this.#failures = 0
        await this.#opts.onData?.(payload)
        if (this.#opts.flowControl !== false && this.#offset - this.#acked >= this.#ackEvery) this.#ack()
        return
      }
      case Channel.Offset: {
        const f = parseJson(payload)
        const offset = typeof f.offset === 'number' ? f.offset : this.#offset
        const dropped = typeof f.dropped === 'number' ? f.dropped : 0
        this.#offset = offset
        if (this.#acked < offset) this.#acked = offset
        if (dropped > 0) {
          this.#truncated = true
          this.#dropped += dropped
          this.#opts.onTruncated?.({ offset, dropped })
        }
        return
      }
      case Channel.Status: {
        const f = parseJson(payload)
        const exit = exitFromWire(f.exit)
        if (exit) {
          const result: PtyResult = {
            pid: this.pid,
            exitCode: exit.exitCode,
            reason: exit.reason,
            truncated: this.#truncated,
            droppedBytes: this.#dropped,
          }
          if (exit.signal !== undefined) result.signal = exit.signal
          this.#finish(result)
          return
        }
        const err = errorFromBody(undefined, typeof f.error === 'object' && f.error !== null ? (f.error as ErrorBody) : undefined)
        // A broken link to the agent or a stopping server is worth another
        // attempt (the close that follows triggers it); anything else is final.
        if (!TRANSIENT_STATUS_CODES.has(err.code)) this.#finish(undefined, err)
        return
      }
      case Channel.Ping:
        this.#send(frame(Channel.Pong, payload), false)
        return
      default:
        return // pong, and channels a newer server may add
    }
  }

  #ack(): void {
    this.#acked = this.#offset
    this.#send(jsonFrame(Channel.Ack, { offset: this.#offset }), false)
  }

  /** The socket closed without a final status frame: give up or reattach, as the close code says. */
  async #lost(socket: WebSocketLike, ev: CloseEventLike): Promise<void> {
    if (this.#done.settled || this.#socket !== socket) return
    this.#socket = undefined
    const cfg = this.#ctx.stream
    const signal = this.#abort.signal

    const appCode = ev.code !== undefined && ev.code >= 4000 && ev.code <= 4999
    if (appCode) {
      // The server said why: no need to ask.
      const final = errorFromClose(ev.code, ev.reason)
      if (final) {
        this.#finish(undefined, final)
        return
      }
    } else {
      // No reason given: a network failure, or a server that predates the
      // close codes refusing the handshake (which the WebSocket API shows
      // as a bare 1006). A plain request tells a dead sandbox or a bad
      // credential (typed error) from a blip (reattach).
      try {
        await this.#ctx.api.empty('GET', _commandsPath(this.#ctx.sandboxId, this.pid), {
          auth: 'data',
          retry: 'none',
          signal,
        })
      } catch (e) {
        if (this.#done.settled) return
        if (e instanceof SandboxError && !(e instanceof ConnectionError) && !(e instanceof TimeoutError) && !isTransient(e)) {
          this.#finish(undefined, e)
          return
        }
      }
    }
    if (this.#done.settled) return
    if (this.#failures >= cfg.maxReconnects) {
      const detail = ev.code !== undefined ? ` (close code ${ev.code}${ev.reason ? `: ${ev.reason}` : ''})` : ''
      this.#finish(
        undefined,
        new ConnectionError(
          `the terminal connection was lost and could not be re-established after ${this.#failures} attempts${detail}; ` +
            `the program may still be running (reattach with pty.connect(pid, { from: ${this.#offset} }))`,
          { code: 'stream_lost' },
        ),
      )
      return
    }
    try {
      await sleep(backoffDelay(this.#failures, cfg.initialDelayMs, cfg.maxDelayMs), signal)
    } catch {
      return // aborted: the abort listener has finished the handle
    }
    this.#failures++
    this.#connect()
  }
}

function isTransient(e: SandboxError): boolean {
  return e.status === 429 || e.status === 502 || e.status === 503 || e.status === 504
}

function parseJson(payload: Uint8Array): Record<string, unknown> {
  try {
    const v = JSON.parse(new TextDecoder().decode(payload)) as unknown
    return typeof v === 'object' && v !== null ? (v as Record<string, unknown>) : {}
  } catch {
    return {}
  }
}

function openSocket(ctx: SandboxContext, url: string): WebSocketLike {
  const custom = ctx.api.webSocketCtor
  const Ctor = custom ?? (globalThis as { WebSocket?: unknown }).WebSocket
  if (typeof Ctor !== 'function') {
    throw new SandboxError(
      'no global WebSocket in this runtime (Node 20?): pass the `WebSocket` option, e.g. the one exported by the `ws` package',
      { code: 'no_websocket' },
    )
  }
  const auth = ctx.api.webSocketAuth()
  type AnyCtor = new (url: string, a?: unknown, b?: unknown) => WebSocketLike
  if (!auth.headers) return new (Ctor as AnyCtor)(url, auth.protocols)
  // Only an API key is available, and it can only travel in a header.
  if (isBrowser()) {
    throw new SandboxError(
      'a browser WebSocket cannot send an API key; connect with the sandbox access token (and never ship an API key to a browser)',
      { code: 'missing_credentials' },
    )
  }
  // `ws`: new WebSocket(url, protocols, { headers }).
  if (custom) return new (Ctor as AnyCtor)(url, auth.protocols, { headers: auth.headers })
  // Node (undici), Bun: new WebSocket(url, { protocols, headers }).
  return new (Ctor as AnyCtor)(url, { protocols: auth.protocols, headers: auth.headers })
}

/** `sandbox.pty`. */
export class Pty {
  readonly #ctx: SandboxContext

  /** @internal */
  constructor(ctx: SandboxContext) {
    this.#ctx = ctx
  }

  /** Starts a program on a new pseudo-terminal and attaches to it. */
  async create(opts: PtyCreateOptions = {}): Promise<PtyHandle> {
    const body: Record<string, unknown> = {
      cmd: opts.cmd ?? '/bin/sh',
      // A terminal that reports no size (0, or undefined when piped) gets the default.
      pty: { cols: Math.floor(opts.cols || 80), rows: Math.floor(opts.rows || 24) },
      env: { TERM: 'xterm-256color', ...(opts.envs ?? {}) },
    }
    if (opts.args !== undefined) body.args = opts.args
    if (opts.cwd !== undefined) body.cwd = opts.cwd
    if (opts.timeoutMs !== undefined && opts.timeoutMs > 0) body.timeout_ms = Math.ceil(opts.timeoutMs)
    if (opts.tag !== undefined) body.tag = opts.tag
    const res = await this.#ctx.api.json<unknown>('POST', _commandsPath(this.#ctx.sandboxId), {
      auth: 'data',
      retry: 'rate-limit',
      json: body,
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    const info = commandInfoFromWire(res)
    const handle = new PtyHandle(this.#ctx, info.pid, 0, opts)
    try {
      return await handle.open()
    } catch (e) {
      // Do not leave a terminal nobody can reach.
      void _signalCommand(this.#ctx, info.pid, 'SIGKILL', {}).catch(() => {})
      throw e
    }
  }

  /** Attaches to an existing terminal session (or any command) by id. */
  connect(pid: string, opts: PtyConnectOptions = {}): Promise<PtyHandle> {
    return new PtyHandle(this.#ctx, pid, opts.from ?? 0, opts).open()
  }

  /** Sends input to a session without holding a socket to it. */
  sendInput(pid: string, data: string | Uint8Array, opts: CallOptions = {}): Promise<void> {
    return _writeStdin(this.#ctx, pid, data, opts)
  }

  /** Resizes a session without holding a socket to it. */
  async resize(pid: string, size: PtySize, opts: CallOptions = {}): Promise<void> {
    await this.#ctx.api.empty('POST', _commandsPath(this.#ctx.sandboxId, pid, 'resize'), {
      auth: 'data',
      retry: 'idempotent',
      json: { cols: Math.floor(size.cols), rows: Math.floor(size.rows) },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
  }

  /** Kills a session (default SIGKILL). False if it is unknown or has already exited. */
  kill(pid: string, opts: CallOptions & { killSignal?: Signal } = {}): Promise<boolean> {
    return _signalCommand(this.#ctx, pid, opts.killSignal ?? 'SIGKILL', opts)
  }
}
