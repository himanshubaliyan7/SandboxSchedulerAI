/**
 * Commands: start, stream, reattach, signal and feed processes in a sandbox.
 *
 * Output arrives over a long-lived NDJSON response. The handle counts output
 * bytes, and when the response is cut (a proxy, a network change, a server
 * restart) it reconnects with `from=<offset>` so that nothing is lost or
 * delivered twice. If the server no longer has the missing bytes it says so
 * with a `truncated` event, which the handle surfaces instead of hiding.
 */

import { type CommandExit, CommandExitError, ConnectionError, type ErrorBody, errorFromBody, SandboxError, TimeoutError } from './errors.js'
import type { ApiClient, CallOptions } from './http.js'
import { readNdjson } from './ndjson.js'
import { type CommandInfo, commandInfoFromWire, type ExitReason, exitFromWire, list, type Signal } from './types.js'
import { abortReason, backoffDelay, type Deferred, decodeBase64, deferred, linkedAbort, sleep, utf8 } from './util.js'

/** Settings shared by the modules of one sandbox. */
export interface SandboxContext {
  readonly api: ApiClient
  readonly sandboxId: string
  readonly throwOnNonZeroExit: boolean
  readonly commandTimeoutMs: number
  readonly stream: Required<StreamOptions>
}

/** How a dropped output stream (NDJSON or WebSocket) is re-established. */
export interface StreamOptions {
  /** Reconnect attempts without any progress before giving up. Default 5; 0 disables reconnecting. */
  maxReconnects?: number
  /** First reconnect delay. Default 200 ms. */
  initialDelayMs?: number
  /** Upper bound of one reconnect delay. Default 5000 ms. */
  maxDelayMs?: number
  /**
   * Treat an NDJSON stream as dead when nothing arrives for this long. The
   * server sends a keepalive every 25 s. Default 60000 ms; 0 disables.
   */
  idleTimeoutMs?: number
}

export const DEFAULT_STREAM: Required<StreamOptions> = {
  maxReconnects: 5,
  initialDelayMs: 200,
  maxDelayMs: 5_000,
  idleTimeoutMs: 60_000,
}

/** The outcome of a finished command. */
export interface CommandResult extends CommandExit {
  reason: ExitReason
}

/** One item of `for await (const event of handle)`. */
export type CommandEvent =
  | {
      type: 'stdout' | 'stderr' | 'pty'
      /** The raw bytes of this chunk. */
      data: Uint8Array
      /**
       * The chunk as text, decoded with a streaming UTF-8 decoder: a
       * character split across two chunks appears whole in the second one
       * (so `text` can be empty).
       */
      text: string
      /** Offset of the first byte in the command's combined output. */
      offset: number
    }
  | {
      /** `dropped` bytes before `offset` were lost: the reader fell behind the server's 1 MiB replay buffer. */
      type: 'truncated'
      offset: number
      dropped: number
    }
  | { type: 'exit'; exitCode: number; reason: ExitReason; signal?: string }

/** Called with each piece of output. May return a promise; the stream waits for it (backpressure). */
export type OutputCallback = (data: string) => void | Promise<void>

export interface CommandOutputOptions {
  /** Called with stdout text as it arrives (PTY output is delivered here too). */
  onStdout?: OutputCallback
  /** Called with stderr text as it arrives. */
  onStderr?: OutputCallback
  /** Called when output was lost; see {@link CommandEvent}. */
  onTruncated?: (info: { offset: number; dropped: number }) => void | Promise<void>
  /** Overrides the sandbox's `throwOnNonZeroExit` for this command. */
  throwOnNonZeroExit?: boolean
}

export interface CommandStartOptions extends CommandOutputOptions, CallOptions {
  /**
   * Arguments. When given, `cmd` is the executable and no shell is
   * involved. When omitted, `cmd` is a shell command line run with
   * `shell -c cmd`, as in E2B.
   */
  args?: string[]
  /** Shell for the no-`args` form. Default `/bin/sh`. */
  shell?: string
  /** Environment variables added to the sandbox's for this command. */
  envs?: Record<string, string>
  /** Working directory. Default `/workspace`. */
  cwd?: string
  /**
   * Wall-clock limit of the command, enforced by the server: on expiry the
   * process group is killed and the exit reason is `timeout`. Default
   * 60000 ms (E2B's default); 0 means no limit.
   */
  timeoutMs?: number
  /**
   * `true` keeps stdin open so that `sendStdin()` works. A string or bytes
   * are written to stdin, which is then closed (foreground and background).
   * Default: stdin is `/dev/null`.
   */
  stdin?: boolean | string | Uint8Array
  /** Optional name, unique among the sandbox's running commands. */
  tag?: string
  /** Return a {@link CommandHandle} as soon as the command has started instead of waiting for it. */
  background?: boolean
}

export interface CommandConnectOptions extends CommandOutputOptions, CallOptions {
  /**
   * Output offset to start from. Default 0: replay what the server still
   * buffers (the last 1 MiB), then follow live. Pass `CommandInfo.nextOffset`
   * to get only new output.
   */
  from?: number
}

interface WireEvent {
  type?: unknown
  offset?: unknown
  data?: unknown
  dropped?: unknown
  exit?: unknown
  command?: unknown
  error?: unknown
}

/** Server-side conditions that end one connection but not the command. */
const RECONNECTABLE_CODES = new Set(['shutting_down', 'agent_unavailable', 'timeout', 'internal'])

function commandsPath(sandboxId: string, pid?: string, action?: string): string {
  let p = `/v1/sandboxes/${encodeURIComponent(sandboxId)}/commands`
  if (pid !== undefined) p += `/${encodeURIComponent(pid)}`
  if (action !== undefined) p += `/${action}`
  return p
}

/** Carries an exception thrown by a caller's callback out of the stream loop untouched. */
class CallbackFailure {
  constructor(readonly error: unknown) {}
}

async function callback<A>(fn: ((arg: A) => void | Promise<void>) | undefined, arg: A): Promise<void> {
  if (!fn) return
  try {
    await fn(arg)
  } catch (e) {
    throw new CallbackFailure(e)
  }
}

type Source = { kind: 'start'; body: Record<string, unknown> } | { kind: 'attach'; pid: string; from: number }

/**
 * A running (or finished) command.
 *
 * Output can be consumed three ways, which may be combined: the
 * `onStdout`/`onStderr` callbacks given at start, `for await (const event of
 * handle)`, and the accumulated `stdout`/`stderr` of `wait()`.
 */
export class CommandHandle implements AsyncIterable<CommandEvent> {
  readonly #ctx: SandboxContext
  readonly #opts: CommandOutputOptions
  readonly #abort: AbortController
  readonly #disposeAbort: () => void
  readonly #started: Deferred<void> = deferred()
  readonly #done: Deferred<CommandResult> = deferred()
  readonly #decoders = { stdout: new TextDecoder(), stderr: new TextDecoder() }
  readonly #stdout: string[] = []
  readonly #stderr: string[] = []
  #pid = ''
  #offset = 0
  #dropped = 0
  #truncated = false
  #exit: CommandResult | undefined
  #disconnected = false
  // Events buffered for `for await`. Null when nobody can be iterating
  // (foreground runs, or after wait() was called without an iterator).
  #queue: CommandEvent[] | null
  #iterating = false
  #wake: (() => void) | undefined

  private constructor(ctx: SandboxContext, opts: CommandOutputOptions & CallOptions, buffered: boolean) {
    this.#ctx = ctx
    this.#opts = opts
    this.#queue = buffered ? [] : null
    const linked = linkedAbort(opts.signal)
    this.#abort = linked.controller
    this.#disposeAbort = linked.dispose
  }

  /** @internal Starts a command with `?wait=stream` and resolves once it has a pid. */
  static async start(
    ctx: SandboxContext,
    body: Record<string, unknown>,
    opts: CommandOutputOptions & CallOptions,
    buffered: boolean,
  ): Promise<CommandHandle> {
    const h = new CommandHandle(ctx, opts, buffered)
    void h.#pump({ kind: 'start', body }, opts)
    await h.#started.promise
    return h
  }

  /** @internal Attaches to an existing command and resolves once the server has accepted the stream. */
  static async attach(ctx: SandboxContext, pid: string, opts: CommandConnectOptions): Promise<CommandHandle> {
    // Checked with a plain request first: the stream route sends its
    // response headers only with the first event, which for a silent
    // command is the keepalive 25 s later.
    await ctx.api.empty('GET', commandsPath(ctx.sandboxId, pid), {
      auth: 'data',
      retry: 'idempotent',
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    const h = new CommandHandle(ctx, opts, true)
    h.#pid = pid
    h.#offset = opts.from ?? 0
    h.#started.resolve()
    void h.#pump({ kind: 'attach', pid, from: h.#offset }, opts)
    return h
  }

  /** The command id. Opaque; not a kernel pid. */
  get pid(): string {
    return this.#pid
  }

  /** stdout received so far. */
  get stdout(): string {
    return this.#stdout.join('')
  }

  /** stderr received so far. */
  get stderr(): string {
    return this.#stderr.join('')
  }

  /** The exit code once the command has exited. */
  get exitCode(): number | undefined {
    return this.#exit?.exitCode
  }

  /** Offset of the next output byte: what has been received so far. */
  get offset(): number {
    return this.#offset
  }

  /**
   * Waits for the command to exit and returns its result. Rejects with
   * {@link CommandExitError} on a non-zero exit code unless
   * `throwOnNonZeroExit` is false, and with the stream's error if the
   * output stream could not be kept alive.
   */
  async wait(opts: { throwOnNonZeroExit?: boolean } = {}): Promise<CommandResult> {
    if (!this.#iterating) this.#queue = null
    const result = await this.#done.promise
    const strict = opts.throwOnNonZeroExit ?? this.#opts.throwOnNonZeroExit ?? this.#ctx.throwOnNonZeroExit
    if (strict && result.exitCode !== 0) throw new CommandExitError(result)
    return result
  }

  /** Sends a signal (default SIGKILL) to the command's process group. False if it had already exited. */
  kill(signal: Signal = 'SIGKILL', opts: CallOptions = {}): Promise<boolean> {
    return signalCommand(this.#ctx, this.#pid, signal, opts)
  }

  /** Writes to the command's stdin. The command must have been started with `stdin: true`. */
  sendStdin(data: string | Uint8Array, opts: CallOptions = {}): Promise<void> {
    return writeStdin(this.#ctx, this.#pid, data, opts)
  }

  /** Closes the command's stdin (EOF). */
  closeStdin(opts: CallOptions = {}): Promise<void> {
    return closeStdin(this.#ctx, this.#pid, opts)
  }

  /**
   * Stops receiving output. The command keeps running; reattach with
   * `commands.connect(pid, { from: handle.offset })`. A pending `wait()`
   * rejects and iteration ends.
   */
  disconnect(): void {
    if (this.#done.settled) return
    this.#disconnected = true
    this.#abort.abort()
  }

  [Symbol.asyncIterator](): AsyncIterator<CommandEvent> {
    this.#iterating = true
    const queue = this.#queue ?? []
    this.#queue = queue
    return {
      next: async (): Promise<IteratorResult<CommandEvent>> => {
        for (;;) {
          const ev = queue.shift()
          if (ev) return { value: ev, done: false }
          if (this.#done.settled) {
            if (this.#exit || this.#disconnected) return { value: undefined, done: true }
            await this.#done.promise // rejects with the stream's error
          }
          await new Promise<void>((resolve) => {
            this.#wake = resolve
          })
        }
      },
      return: async (): Promise<IteratorResult<CommandEvent>> => {
        this.#iterating = false
        this.#queue = null
        return { value: undefined, done: true }
      },
    }
  }

  // --- the stream pump -----------------------------------------------------

  #emit(ev: CommandEvent): void {
    this.#queue?.push(ev)
    this.#notify()
  }

  #notify(): void {
    const wake = this.#wake
    this.#wake = undefined
    wake?.()
  }

  #finish(result: CommandResult | undefined, error?: unknown): void {
    if (this.#done.settled) return
    this.#disposeAbort()
    if (result) {
      this.#exit = result
      this.#done.resolve(result)
    } else {
      this.#done.reject(error)
    }
    if (!this.#started.settled) this.#started.reject(error)
    this.#notify()
  }

  async #open(source: Source, opts: CallOptions): Promise<Response> {
    const { api, sandboxId } = this.#ctx
    if (source.kind === 'start') {
      return api.request('POST', commandsPath(sandboxId), {
        auth: 'data',
        retry: 'rate-limit',
        stream: true,
        query: { wait: 'stream' },
        json: source.body,
        headers: { Accept: 'application/x-ndjson' },
        signal: this.#abort.signal,
        timeoutMs: opts.requestTimeoutMs,
      })
    }
    // The response headers of the stream route arrive with its first
    // event, at the latest the keepalive, so the deadline for them is the
    // stream's idle timeout rather than the (shorter) request timeout.
    const idle = this.#ctx.stream.idleTimeoutMs
    const base = opts.requestTimeoutMs ?? api.requestTimeoutMs
    return api.request('GET', commandsPath(sandboxId, source.pid, 'stream'), {
      auth: 'data',
      retry: 'idempotent',
      stream: true,
      query: { from: source.from },
      headers: { Accept: 'application/x-ndjson' },
      signal: this.#abort.signal,
      timeoutMs: idle <= 0 || base <= 0 ? 0 : Math.max(idle, base),
    })
  }

  async #pump(first: Source, opts: CallOptions): Promise<void> {
    const cfg = this.#ctx.stream
    let source: Source = first
    let failures = 0
    for (;;) {
      let cause: unknown
      try {
        const res = await this.#open(source, opts)
        try {
          const before = this.#offset
          const finished = await this.#consume(res)
          if (finished) return
          if (this.#offset > before) failures = 0
          cause = new ConnectionError('the output stream ended before the command exited', { code: 'stream_ended' })
        } finally {
          this.#ctx.api.release(res)
        }
      } catch (e) {
        cause = e
      }

      if (cause instanceof CallbackFailure) {
        this.#abort.abort()
        this.#finish(undefined, cause.error)
        return
      }
      if (this.#abort.signal.aborted) {
        const reason = this.#disconnected
          ? new ConnectionError('disconnected from the command', { code: 'disconnected' })
          : abortReason(this.#abort.signal)
        this.#finish(undefined, reason)
        return
      }
      if (!isReconnectable(cause)) {
        this.#finish(undefined, cause)
        return
      }
      if (this.#pid === '' && cause instanceof SandboxError && cause.status !== undefined) {
        this.#finish(undefined, cause) // the server refused the start: nothing is running
        return
      }
      if (this.#pid === '') {
        // The stream died before the start event: there is no pid to resume
        // from, and starting again could run the command twice.
        this.#finish(
          undefined,
          new ConnectionError('the connection was lost before the server confirmed the command; it may or may not be running', {
            code: 'start_unconfirmed',
            cause,
          }),
        )
        return
      }
      if (failures >= cfg.maxReconnects) {
        this.#finish(
          undefined,
          new ConnectionError(
            `the output stream of command ${this.#pid} was lost and could not be re-established after ${failures} attempts; ` +
              `the command may still be running (reattach with commands.connect(pid, { from: ${this.#offset} }))`,
            { code: 'stream_lost', cause },
          ),
        )
        return
      }
      try {
        await sleep(backoffDelay(failures, cfg.initialDelayMs, cfg.maxDelayMs), this.#abort.signal)
      } catch {
        continue // aborted while waiting: handled at the top of the next round
      }
      failures++
      source = { kind: 'attach', pid: this.#pid, from: this.#offset }
    }
  }

  /** Reads one response to its end. True when the command's exit was seen. */
  async #consume(res: Response): Promise<boolean> {
    if (!res.body) throw new ConnectionError('the server answered a stream request without a body', { code: 'bad_stream' })
    const events = readNdjson(res.body, { idleTimeoutMs: this.#ctx.stream.idleTimeoutMs, signal: this.#abort.signal })
    for await (const raw of events) {
      if (typeof raw !== 'object' || raw === null) continue
      const ev = raw as WireEvent
      const offset = typeof ev.offset === 'number' ? ev.offset : this.#offset
      switch (ev.type) {
        case 'start': {
          const info = commandInfoFromWire(ev.command)
          if (this.#pid === '') this.#pid = info.pid
          if (!this.#started.settled) this.#started.resolve()
          break
        }
        case 'stdout':
        case 'stderr':
        case 'pty':
          await this.#output(ev.type, offset, typeof ev.data === 'string' ? decodeBase64(ev.data) : new Uint8Array(0))
          break
        case 'truncated':
          await this.#gap(offset, typeof ev.dropped === 'number' ? ev.dropped : Math.max(0, offset - this.#offset))
          break
        case 'exit': {
          // `offset` is the total output size: anything beyond what was received is lost output.
          if (offset > this.#offset) await this.#gap(offset, offset - this.#offset)
          await this.#flush()
          const exit = exitFromWire(ev.exit) ?? { exitCode: -1, reason: 'exited' }
          const result: CommandResult = {
            pid: this.#pid,
            exitCode: exit.exitCode,
            reason: exit.reason,
            stdout: this.stdout,
            stderr: this.stderr,
            truncated: this.#truncated,
            droppedBytes: this.#dropped,
          }
          if (exit.signal !== undefined) result.signal = exit.signal
          const exitEvent: CommandEvent = { type: 'exit', exitCode: exit.exitCode, reason: exit.reason }
          if (exit.signal !== undefined) exitEvent.signal = exit.signal
          this.#emit(exitEvent)
          this.#finish(result)
          return true
        }
        case 'error':
          throw errorFromBody(undefined, typeof ev.error === 'object' && ev.error !== null ? (ev.error as ErrorBody) : undefined)
        default:
          // keepalive, and event types a newer server may add.
          break
      }
    }
    return false
  }

  async #output(type: 'stdout' | 'stderr' | 'pty', offset: number, bytes: Uint8Array): Promise<void> {
    let data = bytes
    let at = offset
    if (at > this.#offset) {
      // The server skipped bytes without saying so: make the gap visible.
      await this.#gap(at, at - this.#offset)
    } else if (at < this.#offset) {
      // A replay overlapping what was already delivered: drop the overlap.
      const overlap = this.#offset - at
      if (overlap >= data.byteLength) return
      data = data.subarray(overlap)
      at = this.#offset
    }
    this.#offset = at + data.byteLength
    const channel = type === 'stderr' ? 'stderr' : 'stdout'
    const text = this.#decoders[channel].decode(data, { stream: true })
    this.#emit({ type, data, text, offset: at })
    await this.#deliver(channel, text)
  }

  async #deliver(channel: 'stdout' | 'stderr', text: string): Promise<void> {
    if (text === '') return
    if (channel === 'stdout') {
      this.#stdout.push(text)
      await callback(this.#opts.onStdout, text)
    } else {
      this.#stderr.push(text)
      await callback(this.#opts.onStderr, text)
    }
  }

  /** Emits what the decoders still hold (an incomplete character becomes U+FFFD). */
  async #flush(): Promise<void> {
    await this.#deliver('stdout', this.#decoders.stdout.decode())
    await this.#deliver('stderr', this.#decoders.stderr.decode())
  }

  async #gap(offset: number, dropped: number): Promise<void> {
    // Bytes are missing, so a character pending in a decoder can never complete.
    await this.#flush()
    this.#offset = Math.max(this.#offset, offset)
    if (dropped <= 0) return
    this.#truncated = true
    this.#dropped += dropped
    this.#emit({ type: 'truncated', offset, dropped })
    await callback(this.#opts.onTruncated, { offset, dropped })
  }
}

function isReconnectable(e: unknown): boolean {
  if (e instanceof CallbackFailure) return false
  if (e instanceof ConnectionError) return true
  // A client-side deadline while (re)opening the stream, or a 5xx/429 that outlived the transport's own retries.
  if (e instanceof TimeoutError && e.status === undefined) return true
  if (e instanceof SandboxError && e.status !== undefined)
    return e.status === 429 || e.status === 502 || e.status === 503 || e.status === 504
  if (e instanceof SandboxError) return RECONNECTABLE_CODES.has(e.code)
  // A failure from the body stream itself (a reset connection surfaces as a TypeError).
  return e instanceof Error && e.name !== 'AbortError'
}

async function signalCommand(ctx: SandboxContext, pid: string, signal: Signal, opts: CallOptions): Promise<boolean> {
  try {
    await ctx.api.empty('POST', commandsPath(ctx.sandboxId, pid, 'signal'), {
      auth: 'data',
      // Signalling twice is harmless, so a lost answer may be retried.
      retry: 'idempotent',
      json: { signal },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return true
  } catch (e) {
    if (e instanceof SandboxError && (e.code === 'process_exited' || e.code === 'process_not_found')) return false
    throw e
  }
}

async function writeStdin(ctx: SandboxContext, pid: string, data: string | Uint8Array, opts: CallOptions): Promise<void> {
  await ctx.api.empty('POST', commandsPath(ctx.sandboxId, pid, 'stdin'), {
    auth: 'data',
    retry: 'rate-limit',
    body: typeof data === 'string' ? utf8(data) : data,
    signal: opts.signal,
    timeoutMs: opts.requestTimeoutMs,
  })
}

async function closeStdin(ctx: SandboxContext, pid: string, opts: CallOptions): Promise<void> {
  await ctx.api.empty('POST', commandsPath(ctx.sandboxId, pid, 'close-stdin'), {
    auth: 'data',
    retry: 'rate-limit',
    signal: opts.signal,
    timeoutMs: opts.requestTimeoutMs,
  })
}

/** `sandbox.commands`. */
export class Commands {
  readonly #ctx: SandboxContext

  /** @internal */
  constructor(ctx: SandboxContext) {
    this.#ctx = ctx
  }

  /**
   * Runs a command.
   *
   * ```ts
   * const r = await sandbox.commands.run('python3 -c "print(6*7)"')    // via /bin/sh -c
   * const r = await sandbox.commands.run('python3', { args: ['-c', code] }) // no shell
   * ```
   *
   * In the foreground (default) it resolves with the result when the
   * command exits, and rejects with {@link CommandExitError} on a non-zero
   * exit code unless `throwOnNonZeroExit` is false. Aborting `signal`
   * kills the command. With `background: true` it resolves with a
   * {@link CommandHandle} as soon as the command has started.
   */
  run(cmd: string, opts: CommandStartOptions & { background: true }): Promise<CommandHandle>
  run(cmd: string, opts?: CommandStartOptions & { background?: false }): Promise<CommandResult>
  run(cmd: string, opts?: CommandStartOptions): Promise<CommandResult | CommandHandle>
  async run(cmd: string, opts: CommandStartOptions = {}): Promise<CommandResult | CommandHandle> {
    const body: Record<string, unknown> =
      opts.args === undefined ? { cmd: opts.shell ?? '/bin/sh', args: ['-c', cmd] } : { cmd, args: opts.args }
    if (opts.envs && Object.keys(opts.envs).length > 0) body.env = opts.envs
    if (opts.cwd !== undefined) body.cwd = opts.cwd
    const timeoutMs = opts.timeoutMs ?? this.#ctx.commandTimeoutMs
    if (timeoutMs > 0) body.timeout_ms = Math.ceil(timeoutMs)
    if (opts.stdin !== undefined && opts.stdin !== false) body.stdin = true
    if (opts.tag !== undefined) body.tag = opts.tag

    const background = opts.background === true
    const handle = await CommandHandle.start(this.#ctx, body, opts, background)

    const input = typeof opts.stdin === 'string' || opts.stdin instanceof Uint8Array ? opts.stdin : undefined
    const feed = async (): Promise<void> => {
      if (input === undefined) return
      try {
        if (input.length > 0) await handle.sendStdin(input, opts)
        await handle.closeStdin(opts)
      } catch (e) {
        // A command that exits without reading its input is not an error
        // of the caller; the exit status tells the story.
        if (e instanceof SandboxError && (e.code === 'process_exited' || e.code === 'stdin_closed')) return
        throw e
      }
    }

    if (background) {
      await feed()
      return handle
    }
    try {
      await feed()
      return await handle.wait()
    } catch (e) {
      if (opts.signal?.aborted) {
        // A foreground caller has no handle to clean up with: do not leave the command running.
        void handle.kill('SIGKILL').catch(() => {})
      } else if (!(e instanceof CommandExitError)) {
        handle.disconnect()
      }
      throw e
    }
  }

  /** Lists the sandbox's commands, running and exited, in start order. */
  async list(opts: CallOptions = {}): Promise<CommandInfo[]> {
    const res = await this.#ctx.api.json<unknown>('GET', commandsPath(this.#ctx.sandboxId), {
      auth: 'data',
      retry: 'idempotent',
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return list(res, 'commands').map(commandInfoFromWire)
  }

  /** Returns one command. */
  async get(pid: string, opts: CallOptions = {}): Promise<CommandInfo> {
    const res = await this.#ctx.api.json<unknown>('GET', commandsPath(this.#ctx.sandboxId, pid), {
      auth: 'data',
      retry: 'idempotent',
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return commandInfoFromWire(res)
  }

  /**
   * Attaches to a command started earlier (by this or another client), also
   * after it has exited. Unlike E2B, the buffered output is replayed first;
   * see `from`.
   */
  connect(pid: string, opts: CommandConnectOptions = {}): Promise<CommandHandle> {
    return CommandHandle.attach(this.#ctx, pid, opts)
  }

  /** Signals a command's process group (default SIGKILL). False if the command is unknown or has already exited. */
  kill(pid: string, opts: CallOptions & { signal?: AbortSignal; killSignal?: Signal } = {}): Promise<boolean> {
    return signalCommand(this.#ctx, pid, opts.killSignal ?? 'SIGKILL', opts)
  }

  /** Writes to the stdin of a command started with `stdin: true` (or of a PTY). */
  sendStdin(pid: string, data: string | Uint8Array, opts: CallOptions = {}): Promise<void> {
    return writeStdin(this.#ctx, pid, data, opts)
  }

  /** Closes a command's stdin (EOF). Pipe mode only; send Ctrl+D (`\x04`) to a PTY. */
  closeStdin(pid: string, opts: CallOptions = {}): Promise<void> {
    return closeStdin(this.#ctx, pid, opts)
  }
}

export { commandsPath as _commandsPath, signalCommand as _signalCommand, writeStdin as _writeStdin }
