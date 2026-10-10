import { afterEach, beforeEach } from 'vitest'
import { Sandbox, type SandboxCreateOptions } from '../../src/index.js'
import { API_KEY, MockServer } from './mock-server.js'

/** Starts a fresh mock server for each test and returns accessors for it. */
export function useMockServer(): { server: () => MockServer; create: (opts?: SandboxCreateOptions) => Promise<Sandbox> } {
  let server: MockServer
  beforeEach(async () => {
    server = await new MockServer().start()
  })
  afterEach(async () => {
    await server.stop()
  })
  return {
    server: () => server,
    create: (opts = {}) =>
      Sandbox.create('python', {
        apiKey: API_KEY,
        baseUrl: server.url,
        // Fast reconnects keep the fault-injection tests quick.
        stream: { initialDelayMs: 5, maxDelayMs: 20, ...(opts.stream ?? {}) },
        retry: { initialDelayMs: 5, maxDelayMs: 20 },
        ...opts,
      }),
  }
}
