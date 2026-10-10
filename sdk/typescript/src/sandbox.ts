/// <reference lib="esnext.disposable" preserve="true" />
/**
 * The Sandbox class: the E2B-shaped entry point of the SDK.
 *
 * Control-plane operations (create, list, kill, timeout, network, events,
 * stats) need the tenant API key. Data-plane operations (`commands`, `pty`,
 * `files`) work with either the API key or the sandbox's access token,
 * which is what a browser or an untrusted worker should be given.
 */

import { Commands, DEFAULT_STREAM, type SandboxContext, type StreamOptions } from './commands.js'
import { NotFoundError, SandboxError, SandboxTerminatedError, TimeoutError } from './errors.js'
import { Files } from './files.js'
import { ApiClient, type CallOptions, type ConnectionOptions } from './http.js'
import { Pty } from './pty.js'
import {
  list,
  type NetworkPolicy,
  networkPolicyToWire,
  type SandboxEvent,
  type SandboxInfo,
  type SandboxState,
  type SandboxStats,
  sandboxEventFromWire,
  sandboxInfoFromWire,
  statsFromWire,
  type TemplateInfo,
  templateFromWire,
} from './types.js'
import { sleep } from './util.js'

// `await using` needs Symbol.asyncDispose, which older engines lack.
const symbols = Symbol as { asyncDispose?: symbol }
if (typeof symbols.asyncDispose !== 'symbol') {
  Object.defineProperty(Symbol, 'asyncDispose', { value: Symbol.for('Symbol.asyncDispose') })
}

/** Behaviour of a Sandbox object, on top of where and how to connect. */
export interface SandboxOptions extends ConnectionOptions {
  /**
   * Whether `commands.run()` and `CommandHandle.wait()` reject with
   * `CommandExitError` when a command exits non-zero. Default true, which
   * is what E2B does; pass false to get the result instead.
   */
  throwOnNonZeroExit?: boolean
  /** Default per-command wall-clock limit for `commands.run()`. Default 60000 ms; 0 means none. */
  commandTimeoutMs?: number
  /** How dropped output streams are re-established. */
  stream?: StreamOptions
}

export interface SandboxCreateOptions extends SandboxOptions, CallOptions {
  /**
   * Maximum lifetime of the sandbox in milliseconds. At most the template's
   * limit; omitted or 0 means the template's.
   */
  timeoutMs?: number
  /** Environment of every command in the sandbox. */
  envs?: Record<string, string>
  /** Free-form labels, returned with the sandbox and usable as a list filter. */
  metadata?: Record<string, string>
  /** Egress policy; the mode may be the template's or stricter. */
  network?: NetworkPolicy
}

export interface SandboxConnectOptions extends SandboxOptions, CallOptions {
  /**
   * The sandbox's access token (`sbt_...`), as returned by create. With it
   * the data plane works without an API key.
   */
  accessToken?: string
}

export interface SandboxListOptions extends ConnectionOptions, CallOptions {
  /** States to include, or `'all'`. Default: the live ones. */
  state?: SandboxState[] | 'all'
  templateId?: string
  /** Only sandboxes whose metadata contains every pair. */
  metadata?: Record<string, string>
  /** 1 to 1000. Default 100. */
  limit?: number
}

export interface SandboxKillOptions extends CallOptions {
  /**
   * Also wait until the container and its storage are gone (state
   * `destroyed`). Default false: resolve as soon as the server has fenced
   * the sandbox, which already makes it unusable and frees its quota slot;
   * the teardown then finishes in the background.
   */
  waitUntilDestroyed?: boolean
  /** Budget for `waitUntilDestroyed`. Default 30000 ms. */
  waitTimeoutMs?: number
}

function sandboxPath(id: string, action?: string): string {
  const p = `/v1/sandboxes/${encodeURIComponent(id)}`
  return action ? `${p}/${action}` : p
}

function contextFor(api: ApiClient, sandboxId: string, opts: SandboxOptions): SandboxContext {
  const stream = { ...DEFAULT_STREAM }
  for (const [k, v] of Object.entries(opts.stream ?? {})) {
    if (typeof v === 'number' && k in stream) stream[k as keyof StreamOptions] = v
  }
  return {
    api,
    sandboxId,
    throwOnNonZeroExit: opts.throwOnNonZeroExit ?? true,
    commandTimeoutMs: opts.commandTimeoutMs ?? 60_000,
    stream,
  }
}

async function killById(api: ApiClient, sandboxId: string, opts: SandboxKillOptions): Promise<boolean> {
  try {
    // The delete is asynchronous by default: 204 means the sandbox is fenced
    // (unusable, off the quota) and is being destroyed. `wait=true` asks the
    // server to answer after the teardown; any 2xx means "accepted".
    await api.empty('DELETE', sandboxPath(sandboxId), {
      auth: 'control',
      retry: 'idempotent',
      query: { wait: opts.waitUntilDestroyed ? 'true' : undefined },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
  } catch (e) {
    if (e instanceof NotFoundError) return false
    throw e
  }
  if (!opts.waitUntilDestroyed) return true
  const budget = opts.waitTimeoutMs ?? 30_000
  const deadline = Date.now() + budget
  for (let delay = 50; ; delay = Math.min(delay * 2, 1_000)) {
    let state: SandboxState
    try {
      state = (await getInfoById(api, sandboxId, opts)).state
    } catch (e) {
      if (e instanceof NotFoundError) return true
      throw e
    }
    if (state === 'destroyed') return true
    if (Date.now() + delay > deadline) {
      throw new TimeoutError(`sandbox ${sandboxId} was still ${state} ${budget} ms after it was killed`, { code: 'kill_timeout' })
    }
    await sleep(delay, opts.signal)
  }
}

async function getInfoById(api: ApiClient, sandboxId: string, opts: CallOptions): Promise<SandboxInfo> {
  const res = await api.json<unknown>('GET', sandboxPath(sandboxId), {
    auth: 'control',
    retry: 'idempotent',
    signal: opts.signal,
    timeoutMs: opts.requestTimeoutMs,
  })
  return sandboxInfoFromWire(res)
}

/**
 * A sandbox.
 *
 * ```ts
 * await using sandbox = await Sandbox.create('python')
 * const { stdout } = await sandbox.commands.run('python3', { args: ['-c', 'print(6*7)'] })
 * ```
 */
export class Sandbox implements AsyncDisposable {
  readonly sandboxId: string
  /** Run, stream, reattach and signal commands. */
  readonly commands: Commands
  /** Interactive terminals. */
  readonly pty: Pty
  /** Read and write files. */
  readonly files: Files
  readonly #api: ApiClient
  #info: SandboxInfo | undefined

  private constructor(api: ApiClient, sandboxId: string, opts: SandboxOptions, info?: SandboxInfo) {
    this.#api = api
    this.sandboxId = sandboxId
    this.#info = info
    const ctx = contextFor(api, sandboxId, opts)
    this.commands = new Commands(ctx)
    this.pty = new Pty(ctx)
    this.files = new Files(ctx)
  }

  /**
   * Claims a warm sandbox of `template` and returns it running.
   *
   * `429 rate_limited` and `503 pool_empty` are retried automatically,
   * honouring `Retry-After` (see the `retry` option).
   */
  static async create(template: string, opts: SandboxCreateOptions = {}): Promise<Sandbox> {
    const api = new ApiClient(opts)
    const body: Record<string, unknown> = { template_id: template }
    if (opts.timeoutMs !== undefined && opts.timeoutMs > 0) body.timeout_ms = Math.ceil(opts.timeoutMs)
    if (opts.envs && Object.keys(opts.envs).length > 0) body.env = opts.envs
    if (opts.metadata && Object.keys(opts.metadata).length > 0) body.metadata = opts.metadata
    if (opts.network) body.network = networkPolicyToWire(opts.network)
    const res = await api.json<Record<string, unknown>>('POST', '/v1/sandboxes', {
      auth: 'control',
      retry: 'create',
      json: body,
      signal: opts.signal,
      // The server may queue the claim on an empty pool before answering.
      timeoutMs: opts.requestTimeoutMs ?? Math.max(api.requestTimeoutMs, api.requestTimeoutMs > 0 ? 60_000 : 0),
    })
    const info = sandboxInfoFromWire(res)
    if (info.sandboxId === '') throw new SandboxError('the server answered a create without a sandbox id', { code: 'bad_response' })
    const token = typeof res?.access_token === 'string' && res.access_token !== '' ? res.access_token : undefined
    return new Sandbox(api.withAccessToken(token), info.sandboxId, opts, info)
  }

  /**
   * Connects to an existing sandbox.
   *
   * With an API key the sandbox is looked up first (so an unknown id fails
   * here, with `NotFoundError`). With only an `accessToken` — the browser
   * case — the token is checked with one data-plane request, and only
   * `commands`, `pty` and `files` are usable.
   */
  static async connect(sandboxId: string, opts: SandboxConnectOptions = {}): Promise<Sandbox> {
    const api = new ApiClient(opts).withAccessToken(opts.accessToken)
    const sandbox = new Sandbox(api, sandboxId, opts)
    if (api.hasApiKey) {
      sandbox.#info = await getInfoById(api, sandboxId, opts)
    } else {
      await sandbox.commands.list(opts)
    }
    return sandbox
  }

  /** Lists the tenant's sandboxes, newest first. */
  static async list(opts: SandboxListOptions = {}): Promise<SandboxInfo[]> {
    const api = new ApiClient(opts)
    const res = await api.json<unknown>('GET', '/v1/sandboxes', {
      auth: 'control',
      retry: 'idempotent',
      query: {
        state: opts.state === undefined ? undefined : opts.state === 'all' ? 'all' : opts.state.join(','),
        template_id: opts.templateId,
        metadata: opts.metadata ? Object.entries(opts.metadata).map(([k, v]) => `${k}=${v}`) : undefined,
        limit: opts.limit,
      },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return list(res, 'sandboxes').map(sandboxInfoFromWire)
  }

  /** Kills a sandbox by id. False if it does not exist (for this tenant). */
  static kill(sandboxId: string, opts: ConnectionOptions & SandboxKillOptions = {}): Promise<boolean> {
    return killById(new ApiClient(opts), sandboxId, opts)
  }

  /** Returns a sandbox by id, also after it has terminated. */
  static getInfo(sandboxId: string, opts: ConnectionOptions & CallOptions = {}): Promise<SandboxInfo> {
    return getInfoById(new ApiClient(opts), sandboxId, opts)
  }

  /** Lists the templates sandboxes can be created from. */
  static async listTemplates(opts: ConnectionOptions & CallOptions = {}): Promise<TemplateInfo[]> {
    const res = await new ApiClient(opts).json<unknown>('GET', '/v1/templates', {
      auth: 'control',
      retry: 'idempotent',
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return list(res, 'templates').map(templateFromWire)
  }

  /**
   * The sandbox's access token: the credential for its data plane, and the
   * only one that may be handed to a browser. Undefined on a sandbox
   * connected without one.
   */
  get accessToken(): string | undefined {
    return this.#api.accessToken
  }

  /** The sandbox as last seen by create, connect, getInfo, setTimeout or updateNetwork. */
  get info(): SandboxInfo | undefined {
    return this.#info
  }

  /**
   * Kills the sandbox and destroys its storage. Idempotent. False if the
   * server does not know the sandbox.
   */
  kill(opts: SandboxKillOptions = {}): Promise<boolean> {
    return killById(this.#api, this.sandboxId, opts)
  }

  /**
   * Sets the remaining lifetime, counted from now; it may shorten or extend
   * it. The server caps it at the template's limit: read `expiresAt` of
   * the result for the effective value.
   */
  async setTimeout(timeoutMs: number, opts: CallOptions = {}): Promise<SandboxInfo> {
    const res = await this.#api.json<unknown>('POST', sandboxPath(this.sandboxId, 'timeout'), {
      auth: 'control',
      // Setting the same deadline twice is harmless.
      retry: 'idempotent',
      json: { timeout_ms: Math.ceil(timeoutMs) },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    this.#info = sandboxInfoFromWire(res)
    return this.#info
  }

  /** Fetches the sandbox's current state. Works after it has terminated. */
  async getInfo(opts: CallOptions = {}): Promise<SandboxInfo> {
    this.#info = await getInfoById(this.#api, this.sandboxId, opts)
    return this.#info
  }

  /**
   * Whether the sandbox is running. With only an access token this is
   * answered by a data-plane request (which counts as activity for the
   * idle timer).
   */
  async isRunning(opts: CallOptions = {}): Promise<boolean> {
    try {
      if (this.#api.hasApiKey) return (await this.getInfo(opts)).state === 'running'
      await this.commands.list(opts)
      return true
    } catch (e) {
      if (e instanceof NotFoundError || e instanceof SandboxTerminatedError) return false
      // With a token alone, a sandbox that is gone and a wrong token look the same.
      if (!this.#api.hasApiKey && e instanceof SandboxError && e.status === 401) return false
      throw e
    }
  }

  /**
   * Replaces the egress policy of the running sandbox. It takes effect
   * before the call returns; connections it no longer permits are closed.
   */
  async updateNetwork(policy: NetworkPolicy, opts: CallOptions = {}): Promise<SandboxInfo> {
    const res = await this.#api.json<unknown>('PUT', sandboxPath(this.sandboxId, 'network'), {
      auth: 'control',
      retry: 'idempotent',
      json: networkPolicyToWire(policy),
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    this.#info = sandboxInfoFromWire(res)
    return this.#info
  }

  /**
   * The sandbox's event log, oldest first: state changes and one
   * `command_started` / `command_finished` pair per command. Available
   * after the sandbox has terminated.
   */
  async events(opts: CallOptions & { type?: string } = {}): Promise<SandboxEvent[]> {
    const res = await this.#api.json<unknown>('GET', sandboxPath(this.sandboxId, 'events'), {
      auth: 'control',
      retry: 'idempotent',
      query: { type: opts.type },
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return list(res, 'events').map(sandboxEventFromWire)
  }

  /** Resource counters of the running sandbox, read from its cgroup. */
  async getStats(opts: CallOptions = {}): Promise<SandboxStats> {
    const res = await this.#api.json<unknown>('GET', sandboxPath(this.sandboxId, 'stats'), {
      auth: 'control',
      retry: 'idempotent',
      signal: opts.signal,
      timeoutMs: opts.requestTimeoutMs,
    })
    return statsFromWire(res)
  }

  /** `await using sandbox = await Sandbox.create(...)` kills the sandbox when the scope ends. */
  async [Symbol.asyncDispose](): Promise<void> {
    await this.kill()
  }

  /** Shown by `JSON.stringify` and loggers: never the credentials. */
  toJSON(): { sandboxId: string } {
    return { sandboxId: this.sandboxId }
  }
}
