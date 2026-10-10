import { describe, expect, it } from 'vitest'
import {
  AuthenticationError,
  CommandExitError,
  ConflictError,
  errorFromBody,
  InternalServerError,
  NotEnoughSpaceError,
  NotFoundError,
  PermissionDeniedError,
  PoolEmptyError,
  parseRetryAfter,
  QuotaExceededError,
  RateLimitError,
  SandboxError,
  SandboxTerminatedError,
  ServiceUnavailableError,
  TimeoutError,
  ValidationError,
} from '../src/errors.js'

describe('errorFromBody', () => {
  const cases: [number, string, new (...a: never[]) => SandboxError][] = [
    [401, 'unauthorized', AuthenticationError],
    [404, 'not_found', NotFoundError],
    [404, 'template_not_found', NotFoundError],
    [404, 'process_not_found', NotFoundError],
    [404, 'file_not_found', NotFoundError],
    [429, 'rate_limited', RateLimitError],
    [429, 'quota_exceeded', QuotaExceededError],
    [503, 'pool_empty', PoolEmptyError],
    [503, 'shutting_down', ServiceUnavailableError],
    [502, 'agent_unavailable', ServiceUnavailableError],
    [504, 'timeout', TimeoutError],
    [409, 'sandbox_not_running', SandboxTerminatedError],
    [409, 'sandbox_terminated', SandboxTerminatedError],
    [400, 'bad_request', ValidationError],
    [400, 'invalid_timeout', ValidationError],
    [400, 'network_not_allowed', ValidationError],
    [400, 'command_not_found', ValidationError],
    [413, 'payload_too_large', ValidationError],
    [403, 'permission_denied', PermissionDeniedError],
    [409, 'tag_in_use', ConflictError],
    [409, 'stdin_closed', ConflictError],
    [409, 'is_directory', ConflictError],
    [409, 'network_not_attached', ConflictError],
    [507, 'no_space', NotEnoughSpaceError],
    [500, 'internal', InternalServerError],
    [500, 'start_failed', InternalServerError],
    [501, 'not_implemented', InternalServerError],
  ]
  it.each(cases)('%i %s', (status, code, cls) => {
    const e = errorFromBody(status, { code, message: 'm', request_id: 'r1' })
    expect(e).toBeInstanceOf(cls)
    expect(e).toBeInstanceOf(SandboxError)
    expect(e).toBeInstanceOf(Error)
    expect(e.name).toBe(cls.name)
    expect(e).toMatchObject({ code, status, requestId: 'r1', message: 'm' })
  })

  it('PoolEmptyError is a ServiceUnavailableError', () => {
    expect(errorFromBody(503, { code: 'pool_empty', message: 'm' })).toBeInstanceOf(ServiceUnavailableError)
  })

  it('exposes the termination state and reason', () => {
    const e = errorFromBody(409, {
      code: 'sandbox_terminated',
      message: 'sandbox sb-1 has terminated: oom_killed',
      details: { state: 'failed', reason: 'oom_killed' },
    }) as SandboxTerminatedError
    expect(e.state).toBe('failed')
    expect(e.reason).toBe('oom_killed')
  })

  it('exposes retryAfter in seconds and milliseconds, and the rate class', () => {
    const e = errorFromBody(
      429,
      { code: 'rate_limited', message: 'm', details: { class: 'create' } },
      { retryAfterMs: 2000 },
    ) as RateLimitError
    expect(e.retryAfter).toBe(2)
    expect(e.retryAfterMs).toBe(2000)
    expect(e.rateClass).toBe('create')
  })

  it('falls back to the status for a code it does not know', () => {
    expect(errorFromBody(404, { code: 'volume_not_found', message: 'm' })).toBeInstanceOf(NotFoundError)
    expect(errorFromBody(409, { code: 'new_conflict', message: 'm' })).toBeInstanceOf(ConflictError)
    expect(errorFromBody(429, { code: 'new_limit', message: 'm' })).toBeInstanceOf(RateLimitError)
    expect(errorFromBody(503, { code: 'new_outage', message: 'm' })).toBeInstanceOf(ServiceUnavailableError)
    expect(errorFromBody(599, { code: 'weird', message: 'm' })).toBeInstanceOf(InternalServerError)
    expect(errorFromBody(418, { code: 'teapot', message: 'm' }).constructor).toBe(SandboxError)
  })

  it('survives a missing or malformed envelope', () => {
    const e = errorFromBody(502, undefined, { requestId: 'from-header' })
    expect(e).toBeInstanceOf(ServiceUnavailableError)
    expect(e).toMatchObject({ code: 'unknown', requestId: 'from-header', message: 'request failed with HTTP 502', details: {} })
    expect(errorFromBody(400, { code: 7, message: null, details: 'x' }).details).toEqual({})
  })

  it('maps in-band errors (no HTTP status) by code alone', () => {
    const e = errorFromBody(undefined, { code: 'sandbox_terminated', message: 'gone', details: { reason: 'idle_timeout' } })
    expect(e).toBeInstanceOf(SandboxTerminatedError)
    expect(e.status).toBeUndefined()
    expect((e as SandboxTerminatedError).reason).toBe('idle_timeout')
  })
})

describe('parseRetryAfter', () => {
  it('parses seconds, fractions, HTTP dates and garbage', () => {
    expect(parseRetryAfter('3')).toBe(3000)
    expect(parseRetryAfter(' 0.5 ')).toBe(500)
    expect(parseRetryAfter(null)).toBeUndefined()
    expect(parseRetryAfter('')).toBeUndefined()
    expect(parseRetryAfter('soon')).toBeUndefined()
    const now = Date.parse('2026-10-10T00:00:00Z')
    expect(parseRetryAfter('Sat, 10 Oct 2026 00:00:07 GMT', now)).toBe(7000)
    expect(parseRetryAfter('Sat, 10 Oct 2020 00:00:00 GMT', now)).toBe(0)
  })
})

describe('CommandExitError', () => {
  it('carries the whole result and a readable message', () => {
    const e = new CommandExitError({
      pid: 'p1',
      exitCode: 137,
      reason: 'timeout',
      signal: 'SIGKILL',
      stdout: 'out',
      stderr: 'line1\nline2\n',
      truncated: false,
      droppedBytes: 0,
    })
    expect(e).toBeInstanceOf(SandboxError)
    expect(e.message).toContain('timeout (SIGKILL), exit code 137')
    expect(e.message).toContain('line2')
    expect(e).toMatchObject({ exitCode: 137, reason: 'timeout', signal: 'SIGKILL', stdout: 'out', pid: 'p1', code: 'command_exit' })
  })
})
