/**
 * Typed errors. Every error thrown by the SDK for a server answer is a
 * {@link SandboxError} subclass chosen from the HTTP status and the stable
 * `code` of the server's error envelope
 * `{"error": {"code", "message", "request_id", "details"}}`.
 *
 * Unknown codes fall back to the class for their HTTP status, so a newer
 * server never produces an untyped error.
 */

/** Fields common to all SDK errors. */
export interface SandboxErrorInit {
  /** Stable machine-readable code, e.g. `sandbox_not_running`. */
  code?: string
  /** HTTP status, when the error came from an HTTP answer. */
  status?: number
  /** The server's `X-Request-Id`, useful in bug reports. */
  requestId?: string
  /** Code-specific details from the error envelope. */
  details?: Record<string, unknown>
  /** How long the server asked us to wait (from `Retry-After`). */
  retryAfterMs?: number
  cause?: unknown
}

/** Base class of every error the SDK throws on its own behalf. */
export class SandboxError extends Error {
  /** Stable machine-readable code (`unknown` when the server sent none). */
  readonly code: string
  readonly status: number | undefined
  readonly requestId: string | undefined
  readonly details: Record<string, unknown>
  /** Milliseconds the server asked the caller to wait, if it said so. */
  readonly retryAfterMs: number | undefined

  constructor(message: string, init: SandboxErrorInit = {}) {
    super(message, init.cause === undefined ? undefined : { cause: init.cause })
    this.name = new.target.name
    this.code = init.code ?? 'unknown'
    this.status = init.status
    this.requestId = init.requestId
    this.details = init.details ?? {}
    this.retryAfterMs = init.retryAfterMs
  }

  /** `Retry-After` in whole seconds, as the server sent it. */
  get retryAfter(): number | undefined {
    return this.retryAfterMs === undefined ? undefined : Math.ceil(this.retryAfterMs / 1000)
  }
}

/** 401: the API key or access token is missing, wrong or revoked. */
export class AuthenticationError extends SandboxError {}

/**
 * 404: the sandbox, template, command or file does not exist — or belongs
 * to another tenant, which the server deliberately does not distinguish.
 * `code` is `not_found`, `template_not_found`, `process_not_found` or
 * `file_not_found`.
 */
export class NotFoundError extends SandboxError {}

/** 400 / 413: the request was malformed or a value was out of range. */
export class ValidationError extends SandboxError {}

/**
 * The command could not be started: the executable does not exist in the
 * image (`command_not_found`), is not something the kernel can run
 * (`exec_format`), or the working directory is unusable (`bad_cwd`).
 * Nothing ran. It is a {@link ValidationError}, as these codes were
 * before the class existed, so older `instanceof` checks keep working.
 * (Python: `CommandStartError`.)
 */
export class CommandStartError extends ValidationError {}

/** 403: the sandbox user may not access the path or run the file. */
export class PermissionDeniedError extends SandboxError {}

/** 409 that is not about the sandbox's lifecycle (`tag_in_use`, `stdin_closed`, `process_exited`, `is_directory`, ...). */
export class ConflictError extends SandboxError {}

/** 429 `rate_limited`: the tenant's token bucket is empty. See `retryAfter`. */
export class RateLimitError extends SandboxError {
  /** Which bucket was empty: `create` or `default`. */
  get rateClass(): string | undefined {
    const c = this.details.class
    return typeof c === 'string' ? c : undefined
  }
}

/** 429 `quota_exceeded`: the tenant is at its concurrent-sandbox quota. */
export class QuotaExceededError extends SandboxError {}

/** 502/503: the server or the in-sandbox agent cannot serve right now. */
export class ServiceUnavailableError extends SandboxError {}

/** 503 `pool_empty`: no warm sandbox became available within the server's wait. */
export class PoolEmptyError extends ServiceUnavailableError {}

/** A request exceeded its deadline, on the client (`requestTimeoutMs`) or on the server (504 `timeout`). */
export class TimeoutError extends SandboxError {}

/**
 * The sandbox is not running any more (409 `sandbox_not_running` or
 * `sandbox_terminated`, or a stream/WebSocket that ended because of it).
 */
export class SandboxTerminatedError extends SandboxError {
  /** The sandbox's state as the server reported it, e.g. `destroyed`. */
  get state(): string | undefined {
    const s = this.details.state
    return typeof s === 'string' ? s : undefined
  }

  /** Why it ended: `released`, `idle_timeout`, `max_lifetime`, `oom_killed`, `instance_lost`, `host_drain`, ... */
  get reason(): string | undefined {
    const r = this.details.reason
    return typeof r === 'string' ? r : undefined
  }
}

/** 507 `no_space`: the sandbox's writable storage is full. */
export class NotEnoughSpaceError extends SandboxError {}

/** Any other 5xx. */
export class InternalServerError extends SandboxError {}

/**
 * The network failed: the request could not be sent, the response was cut,
 * or a stream could not be re-established within the retry budget.
 */
export class ConnectionError extends SandboxError {}

/** Fields of a finished command, shared by the result and the error. */
export interface CommandExit {
  pid: string
  exitCode: number
  /** `exited`, `killed` (by a signal), `timeout` (per-command limit) or `oom`. */
  reason: string
  /** Signal name when the command was ended by one, e.g. `SIGKILL`. */
  signal?: string
  stdout: string
  stderr: string
  /** True when output was lost (see `droppedBytes`). */
  truncated: boolean
  droppedBytes: number
}

/**
 * A command exited with a non-zero exit code. Thrown by `commands.run()` and
 * `CommandHandle.wait()` unless `throwOnNonZeroExit` is false. It carries the
 * complete result.
 */
export class CommandExitError extends SandboxError implements CommandExit {
  readonly pid: string
  readonly exitCode: number
  readonly reason: string
  readonly signal?: string
  readonly stdout: string
  readonly stderr: string
  readonly truncated: boolean
  readonly droppedBytes: number

  constructor(result: CommandExit) {
    const why =
      result.reason === 'exited'
        ? `exit code ${result.exitCode}`
        : `${result.reason}${result.signal ? ` (${result.signal})` : ''}, exit code ${result.exitCode}`
    const tail = result.stderr.trim().split('\n').slice(-5).join('\n')
    super(`command failed: ${why}${tail ? `\n${tail}` : ''}`, { code: 'command_exit' })
    this.pid = result.pid
    this.exitCode = result.exitCode
    this.reason = result.reason
    if (result.signal !== undefined) this.signal = result.signal
    this.stdout = result.stdout
    this.stderr = result.stderr
    this.truncated = result.truncated
    this.droppedBytes = result.droppedBytes
  }
}

/**
 * The command was killed because its per-command timeout (`timeoutMs`,
 * 60 s by default) expired: a {@link CommandExitError} whose `reason` is
 * `timeout`. (Python: `CommandTimeoutError`.)
 */
export class CommandTimeoutError extends CommandExitError {}

/** The error for a command that ended with a non-zero exit code: the timeout subclass when that is why. */
export function commandExitError(result: CommandExit): CommandExitError {
  return result.reason === 'timeout' ? new CommandTimeoutError(result) : new CommandExitError(result)
}

/** The body of the server's error envelope (also the payload of an NDJSON `error` event). */
export interface ErrorBody {
  code?: unknown
  message?: unknown
  request_id?: unknown
  details?: unknown
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

/**
 * Builds the typed error for an error body. `status` is the HTTP status, or
 * undefined for an error delivered in-band (NDJSON `error` event, WebSocket
 * status frame), in which case the class is chosen from the code alone.
 */
export function errorFromBody(
  status: number | undefined,
  body: ErrorBody | undefined,
  extra: { requestId?: string; retryAfterMs?: number } = {},
): SandboxError {
  const code = typeof body?.code === 'string' && body.code !== '' ? body.code : undefined
  const message =
    typeof body?.message === 'string' && body.message !== ''
      ? body.message
      : status !== undefined
        ? `request failed with HTTP ${status}`
        : 'request failed'
  const requestId = typeof body?.request_id === 'string' ? body.request_id : extra.requestId
  const init: SandboxErrorInit = { code: code ?? 'unknown' }
  if (status !== undefined) init.status = status
  if (requestId !== undefined) init.requestId = requestId
  if (isRecord(body?.details)) init.details = body.details
  if (extra.retryAfterMs !== undefined) init.retryAfterMs = extra.retryAfterMs

  // The code is the stable part of the contract: decide on it first.
  switch (code) {
    case 'unauthorized':
      return new AuthenticationError(message, init)
    case 'not_found':
    case 'template_not_found':
    case 'process_not_found':
    case 'file_not_found':
      return new NotFoundError(message, init)
    case 'rate_limited':
      return new RateLimitError(message, init)
    case 'quota_exceeded':
      return new QuotaExceededError(message, init)
    case 'pool_empty':
      return new PoolEmptyError(message, init)
    case 'shutting_down':
    case 'not_ready':
    case 'agent_unavailable':
      return new ServiceUnavailableError(message, init)
    case 'timeout':
      return new TimeoutError(message, init)
    case 'sandbox_not_running':
    case 'sandbox_terminated':
      return new SandboxTerminatedError(message, init)
    case 'bad_request':
    case 'invalid_timeout':
    case 'network_not_allowed':
    case 'payload_too_large':
    case 'too_large':
    case 'bad_signal':
    case 'bad_frame':
    case 'method_not_allowed':
      return new ValidationError(message, init)
    case 'command_not_found':
    case 'exec_format':
    case 'bad_cwd':
      return new CommandStartError(message, init)
    case 'permission_denied':
      return new PermissionDeniedError(message, init)
    case 'no_space':
      return new NotEnoughSpaceError(message, init)
    case 'conflict':
    case 'network_not_attached':
    case 'tag_in_use':
    case 'stdin_closed':
    case 'stdin_overflow':
    case 'process_exited':
    case 'is_a_pty':
    case 'not_a_pty':
    case 'is_directory':
      return new ConflictError(message, init)
    case 'internal':
    case 'io_error':
    case 'pty_failed':
    case 'start_failed':
    case 'not_implemented':
      return new InternalServerError(message, init)
  }

  // An unknown code (a newer server): fall back to the status.
  switch (status) {
    case 400:
    case 405:
    case 413:
    case 422:
      return new ValidationError(message, init)
    case 401:
      return new AuthenticationError(message, init)
    case 403:
      return new PermissionDeniedError(message, init)
    case 404:
      return new NotFoundError(message, init)
    case 408:
    case 504:
      return new TimeoutError(message, init)
    case 409:
      return new ConflictError(message, init)
    case 429:
      return new RateLimitError(message, init)
    case 502:
    case 503:
      return new ServiceUnavailableError(message, init)
    case 507:
      return new NotEnoughSpaceError(message, init)
  }
  if (status !== undefined && status >= 500) return new InternalServerError(message, init)
  return new SandboxError(message, init)
}

/** Parses a `Retry-After` header (delta-seconds or an HTTP date) into milliseconds. */
export function parseRetryAfter(value: string | null, now: number = Date.now()): number | undefined {
  if (value === null) return undefined
  const v = value.trim()
  if (v === '') return undefined
  if (/^\d+(\.\d+)?$/.test(v)) return Math.round(Number(v) * 1000)
  const at = Date.parse(v)
  if (Number.isNaN(at)) return undefined
  return Math.max(0, at - now)
}
