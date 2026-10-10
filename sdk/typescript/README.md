# sbx-sdk

TypeScript SDK for [SandboxSchedulerAI](../../README.md) (`sbxd`): run
LLM-generated code in warm-pool sandboxes on your own machine. The API is
shaped like E2B's (`Sandbox.create`, `sandbox.commands.run`, `sandbox.files`,
`sandbox.pty`), so code written against E2B ports with small changes.

- Zero runtime dependencies: platform `fetch`, `ReadableStream`,
  `AbortController` and `WebSocket`.
- Runs on Node 20.19+, Deno, Bun, edge runtimes and browsers (data plane
  only, with a sandbox access token).
- Output streams reconnect by byte offset when a proxy cuts them; lost
  output is reported, never hidden.
- Typed errors, automatic retry honouring `Retry-After`, `AbortSignal` on
  every call.

```sh
npm install sbx-sdk
```

## Quick start

Start a server as described in the [repository README](../../README.md),
then:

```ts
import { Sandbox } from 'sbx-sdk'

// Reads SBX_API_KEY and SBX_BASE_URL (default http://127.0.0.1:8080).
await using sandbox = await Sandbox.create('python', { timeoutMs: 5 * 60_000 })

const result = await sandbox.commands.run('python3', { args: ['-c', 'print(6*7)'] })
console.log(result.stdout) // "42\n"
// `await using` kills the sandbox here. Without it: await sandbox.kill()
```

`examples/` has three runnable scripts (`npm run build` first, then
`node examples/01-run-code.ts` on Node 22.18+, which runs TypeScript
directly):

| Script | What it shows |
|---|---|
| `01-run-code.ts` | Run model-written Python and print the result or the traceback |
| `02-interactive-pty.ts` | A shell in your terminal: raw-mode stdin, resize on `SIGWINCH` |
| `03-files.ts` | Upload a file, process it, list and download the output |

## Sandboxes

```ts
const sandbox = await Sandbox.create('python', {
  timeoutMs: 10 * 60_000,              // max lifetime; capped by the template
  envs: { OPENAI_API_BASE: '...' },    // environment of every command
  metadata: { userId: 'u_123' },       // labels; usable as a list filter
  network: { mode: 'allowlist', allowDomains: ['pypi.org', '*.pythonhosted.org'] },
})

sandbox.sandboxId          // "sb-0123456789abcdef"
sandbox.accessToken        // "sbt_..." — the data-plane credential of this sandbox

await sandbox.getInfo()                    // { state, reason, expiresAt, network, metadata, ... }
await sandbox.isRunning()
await sandbox.setTimeout(60_000)           // remaining lifetime from now; returns the updated info
await sandbox.updateNetwork({ mode: 'none' })   // takes effect before it returns
await sandbox.events({ type: 'command_finished' })
await sandbox.getStats()                   // memoryBytes, cpuUsageUsec, oomKills, pidsLimitHits, ...
await sandbox.kill()                       // true; false if the server does not know the sandbox

await Sandbox.list({ metadata: { userId: 'u_123' } })   // SandboxInfo[], newest first
await Sandbox.connect(sandboxId)                        // by id, with the API key
await Sandbox.connect(sandboxId, { accessToken })       // by id + token, no API key needed
await Sandbox.kill(sandboxId)
await Sandbox.listTemplates()
```

`kill()` resolves as soon as the server has fenced the sandbox: it is
unusable and off your quota, and its container is destroyed in the
background within a few hundred milliseconds. Pass
`{ waitUntilDestroyed: true }` to wait for the state `destroyed`.

Three timers can end a sandbox, and the `reason` says which: the template's
idle timeout (`idle_timeout`), the maximum lifetime (`max_lifetime`, the
`timeoutMs` above), and — for a single command, not the sandbox — the
command's own `timeoutMs`.

## Commands

```ts
// A shell command line (run with /bin/sh -c):
await sandbox.commands.run('pip install requests && python3 main.py')

// An executable and its arguments, no shell, no quoting:
await sandbox.commands.run('python3', { args: ['-c', code] })

// Everything:
const result = await sandbox.commands.run('python3', {
  args: ['-'],
  stdin: code,                       // written to stdin, which is then closed
  envs: { DEBUG: '1' },
  cwd: '/workspace/project',
  timeoutMs: 30_000,                 // server-side kill; default 60000, 0 = none
  onStdout: (text) => process.stdout.write(text),
  onStderr: (text) => process.stderr.write(text),
  signal: abortController.signal,    // aborting kills the command
})
// { stdout, stderr, exitCode, reason, signal?, truncated, droppedBytes, pid }
```

`reason` is `exited`, `killed` (by a signal), `timeout` (the per-command
limit) or `oom`.

### Non-zero exit codes throw (as in E2B)

`run()` and `handle.wait()` reject with `CommandExitError` when the exit
code is not 0. The error carries the whole result (`exitCode`, `stdout`,
`stderr`, `reason`, `signal`). If you would rather inspect the result
yourself, switch it off per call or for the whole sandbox:

```ts
const r = await sandbox.commands.run('grep -q needle haystack.txt', { throwOnNonZeroExit: false })
if (r.exitCode !== 0) { /* not found */ }

const sandbox = await Sandbox.create('python', { throwOnNonZeroExit: false })
```

### Background commands

```ts
const server = await sandbox.commands.run('python3 -m http.server 8000', { background: true })
server.pid                          // opaque id (not a kernel pid)

for await (const event of server) { // stdout | stderr | truncated | exit
  if (event.type === 'stdout') process.stdout.write(event.text) // event.data has the raw bytes
}

await server.kill()                 // SIGKILL; server.kill('SIGTERM') for another signal
await server.wait()                 // the result (throws CommandExitError unless disabled)
server.disconnect()                 // stop listening; the command keeps running

const cat = await sandbox.commands.run('cat', { args: [], background: true, stdin: true })
await cat.sendStdin('hello\n')
await cat.closeStdin()

await sandbox.commands.list()                  // CommandInfo[]: running and exited
const again = await sandbox.commands.connect(pid)   // reattach from anywhere, also after exit
await sandbox.commands.kill(pid)
await sandbox.commands.sendStdin(pid, 'data')
```

`commands.connect(pid)` replays the output the server still buffers (the
last 1 MiB) and then follows live; pass `{ from: info.nextOffset }` to get
only new output.

### Differences from E2B worth knowing

| | E2B | sbx-sdk |
|---|---|---|
| `run('cmd line')` | `/bin/bash -l -c` | `/bin/sh -c` (`shell` option), or no shell at all with `args` |
| `pid` | number (kernel pid) | opaque string |
| `commands.connect(pid)` | new output only | replays buffered output first (`from` option) |
| Output lost by a slow reader | not reported | `truncated` / `droppedBytes` / `onTruncated` |
| `sandbox.setTimeout(ms)` | `void` | the updated `SandboxInfo` (the server may cap the value) |
| PTY `wait()` on non-zero exit | throws | returns the result |
| `files.exists/makeDir/getInfo` | `boolean` / `boolean` / `EntryInfo` | the same (`makeDir` is `true` only if it created the directory) |
| Not available | `pause`, `getHost`, `files.watchDir`, `files.rename` | the server has no such routes yet |

## Streaming robustness

Command output arrives over one long-lived NDJSON response. Such responses
get cut: by proxies with idle timeouts, by a laptop changing networks, by a
server restart. The SDK counts output bytes and, when the stream ends
without the command's exit, reconnects with `from=<offset>`:

- Nothing is delivered twice and nothing is silently skipped. If the server
  no longer has the missing bytes (it buffers the last 1 MiB per command)
  you get a `truncated` event, `onTruncated({ offset, dropped })`, and
  `result.truncated === true` with `result.droppedBytes`.
- A stream that is silent for 60 s is treated as dead (the server sends a
  keepalive every 25 s) and re-established.
- Reconnects are bounded: 5 attempts without progress, with exponential
  backoff, then a `ConnectionError` (`code: 'stream_lost'`) whose message
  tells you how to resume. The command keeps running on the server either
  way.
- Text passed to `onStdout`/`onStderr` is decoded with a streaming UTF-8
  decoder per channel, so a multi-byte character split across two chunks
  is delivered whole.

```ts
const sandbox = await Sandbox.create('python', {
  stream: { maxReconnects: 5, initialDelayMs: 200, maxDelayMs: 5000, idleTimeoutMs: 60_000 },
})
```

The one case the SDK does not resume by itself: the connection dies after
the start request was sent but before the server confirmed it. The SDK does
not know the command's id, and will not start it a second time; you get a
`ConnectionError` with `code: 'start_unconfirmed'`. The server sends the
confirmation (the `start` event with the pid) together with the response
headers, so the window is small. To close it, give the command an
`idempotencyKey` and simply call `run` again with the same key and command:
the server starts it at most once and the second call attaches to it.

```ts
const key = crypto.randomUUID()
const run = () => sandbox.commands.run('python3 train.py', { idempotencyKey: key })
const result = await run().catch((e) => (e.code === 'start_unconfirmed' ? run() : Promise.reject(e)))
```

### Compatibility with older servers

The SDK relies on a few things `sbxd` gained together with it, and degrades
as follows against a server without them:

| Feature | With an older server |
|---|---|
| `files.getInfo` / `exists` (stat route) | falls back to listing the parent directory (also when only the sandbox's agent is older) |
| `files.makeDir` result | always `false` (the server did not say whether it created the directory) |
| Terminal closed by the server (close codes 4000-4099) | a close without a code makes the SDK ask the server once what happened, as before |
| `commands.connect()` (immediate stream headers) | resolves only when the command next prints something, or after the 25 s keepalive; fails if `requestTimeoutMs` is shorter than that |
| `idempotencyKey` | ignored by the server: a repeated `run` starts a second command |

## Terminals (PTY)

```ts
const pty = await sandbox.pty.create({
  cols: 120,
  rows: 40,
  onData: (bytes) => process.stdout.write(bytes),
  // cmd: '/bin/sh', args: [], envs: {}, cwd: '/workspace', timeoutMs: 0
})

pty.sendInput('ls -la\n')        // '\x03' is Ctrl+C, '\x04' is Ctrl+D
pty.resize({ cols: 100, rows: 30 })
const { exitCode } = await pty.wait()

// By id, from anywhere:
const same = await sandbox.pty.connect(pty.pid, { onData })
await sandbox.pty.sendInput(pty.pid, 'echo hi\n')
await sandbox.pty.resize(pty.pid, { cols: 80, rows: 24 })
await sandbox.pty.kill(pty.pid)
```

The session runs over a WebSocket. The SDK handles the framing, answers
pings, reattaches at the right byte offset if the socket drops, and does
**flow control** for you: it acknowledges output only after your `onData`
has returned (or its promise has settled), in steps below the server's low
watermark. A command that prints faster than you can render (`yes`, a
runaway loop) is therefore paused by the server instead of filling memory —
provided `onData` returns a promise that reflects when the data was really
consumed.

Input sent while the socket is being re-established is queued (up to 1 MiB).
Input written in the instant before a dead connection is noticed can be
lost; the protocol does not acknowledge input.

### xterm.js

In the browser, connect with the sandbox's **access token**. Your backend
creates the sandbox with its API key and hands the page only
`sandboxId` and `accessToken`:

```ts
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { Sandbox } from 'sbx-sdk'

const term = new Terminal()
const fit = new FitAddon()
term.loadAddon(fit)
term.open(document.getElementById('terminal')!)
fit.fit()

const sandbox = await Sandbox.connect(sandboxId, { baseUrl: 'https://sbx.example.com', accessToken })

const pty = await sandbox.pty.create({
  cols: term.cols,
  rows: term.rows,
  // xterm calls back when it has parsed the chunk: that is the backpressure signal.
  onData: (bytes) => new Promise<void>((done) => term.write(bytes, done)),
})

term.onData((keys) => pty.sendInput(keys))
term.onBinary((data) => pty.sendInput(Uint8Array.from(data, (c) => c.charCodeAt(0))))
term.onResize(({ cols, rows }) => pty.resize({ cols, rows }))
new ResizeObserver(() => fit.fit()).observe(document.getElementById('terminal')!)

const { exitCode } = await pty.wait()
term.write(`\r\n[process exited with code ${exitCode}]\r\n`)
```

## Files

Paths are absolute, or relative to the work directory `/workspace`.

```ts
await sandbox.files.write('data/input.csv', csvText)            // string (UTF-8)
await sandbox.files.write('model.bin', uint8Array)              // bytes
await sandbox.files.write('upload.zip', fileFromInput)          // Blob / File, not buffered
await sandbox.files.write('big.tar', readableStream)            // streamed
await sandbox.files.write('run.sh', script, { mode: 0o755 })

const text = await sandbox.files.read('out/report.json')                         // string
const bytes = await sandbox.files.read('out/plot.png', { format: 'bytes' })      // Uint8Array
const stream = await sandbox.files.read('out/big.parquet', { format: 'stream' }) // ReadableStream

await sandbox.files.list('out')     // [{ name, path, type: 'file' | 'dir', size, mode, modifiedTime }]
await sandbox.files.exists('out/report.json')
await sandbox.files.getInfo('out')     // { name, path, type, size, mode, modifiedTime }; NotFoundError if absent
await sandbox.files.makeDir('a/b/c')   // parents too; true if it created it, false if it was there
await sandbox.files.remove('a')        // a file, or a directory and everything in it
```

A download whose body ends short of its `Content-Length` is rejected with a
`ConnectionError`; a truncated file is never returned as if it were whole.
A `ReadableStream` upload needs a runtime with streaming request bodies
(Node, Deno, Bun, workerd; in browsers only Chromium over HTTP/2) and cannot
be retried automatically — use a `Blob` in browsers.

## Errors

Every error from the server is a `SandboxError` subclass with `code` (the
server's stable code), `status`, `requestId` and `details`.

| Class | When | Extras |
|---|---|---|
| `AuthenticationError` | 401: key or token missing, wrong, revoked | |
| `NotFoundError` | 404: sandbox, template, command or file (`code` says which); also another tenant's sandbox | |
| `ValidationError` | 400/413: malformed request, `invalid_timeout`, `network_not_allowed`, `command_not_found` | `details` |
| `PermissionDeniedError` | 403: the sandbox user may not access the path | |
| `ConflictError` | 409: `tag_in_use`, `stdin_closed`, `process_exited`, `is_directory`, ... | |
| `SandboxTerminatedError` | 409: the sandbox is not running any more | `state`, `reason` |
| `RateLimitError` | 429 `rate_limited` | `retryAfter` (s), `retryAfterMs`, `rateClass` |
| `QuotaExceededError` | 429 `quota_exceeded`: concurrent-sandbox quota | `retryAfter` |
| `PoolEmptyError` | 503 `pool_empty` (a `ServiceUnavailableError`) | `retryAfter` |
| `ServiceUnavailableError` | 502/503: `shutting_down`, `agent_unavailable`, `not_ready` | `retryAfter` |
| `TimeoutError` | a request exceeded `requestTimeoutMs`, or 504 | |
| `NotEnoughSpaceError` | 507: the sandbox's storage is full | |
| `InternalServerError` | other 5xx | |
| `ConnectionError` | the network failed, or a stream could not be re-established | `cause` |
| `CommandExitError` | a command exited non-zero | `exitCode`, `stdout`, `stderr`, `reason`, `signal` |

```ts
import { CommandExitError, SandboxTerminatedError } from 'sbx-sdk'

try {
  await sandbox.commands.run('python3 train.py')
} catch (e) {
  if (e instanceof CommandExitError) console.error(e.exitCode, e.stderr)
  else if (e instanceof SandboxTerminatedError) console.error('sandbox ended:', e.reason) // e.g. oom_killed
  else throw e
}
```

An aborted call rejects with the signal's own reason (an `AbortError`
`DOMException` by default), not with a `SandboxError`.

### Retries

Retried automatically, at most 3 times, waiting for `Retry-After` when the
server sends it and with jittered exponential backoff otherwise:

| Call | Retried on |
|---|---|
| Idempotent calls (GET, PUT, DELETE, set timeout, signal, resize) | 429 `rate_limited`, 502, 503, network failure |
| `Sandbox.create` | 429 `rate_limited`, 503 (`pool_empty`, `shutting_down`) — the server answers these before claiming a sandbox. Not a network failure: the claim may have happened. |
| Starting a command, writing stdin | 429 `rate_limited` only |
| An upload from a `ReadableStream` | never (the body cannot be replayed) |

`quota_exceeded` is not retried by default: the quota frees up only when
one of your own sandboxes ends.

```ts
await Sandbox.create('python', {
  retry: { maxRetries: 5, initialDelayMs: 250, maxDelayMs: 8000, maxRetryAfterMs: 30_000, retryQuotaExceeded: true },
})
await Sandbox.create('python', { retry: false }) // off
```

## Credentials

| Credential | Looks like | Good for | Where it may live |
|---|---|---|---|
| Tenant API key | `sbx_...` | everything | your server, never a browser |
| Sandbox access token | `sbt_...` | `commands`, `pty`, `files` of one sandbox | a browser, an agent worker |

- The API key comes from the `apiKey` option or `SBX_API_KEY`. The access
  token is returned by `Sandbox.create` (`sandbox.accessToken`) and is the
  credential the SDK uses for data-plane calls whenever it has one.
- **Never ship an API key to a browser.** Create the sandbox on your
  server, give the page `sandboxId` and `accessToken`, and connect with
  `Sandbox.connect(sandboxId, { baseUrl, accessToken })`. The token is
  worthless once the sandbox is killed.
- The SDK never logs. Credentials are held in private fields, so
  `console.log(sandbox)` and `JSON.stringify(sandbox)` do not reveal them,
  and they are never put in a URL: on a WebSocket the token travels as a
  subprotocol (`sbx.token.<token>`).
- Browser `fetch` calls to the server are cross-origin requests with custom
  headers, so the server must answer CORS preflights: start `sbxd serve`
  with `--cors-allowed-origins https://your.app` (off by default). That
  opens the data plane only (`commands`, `pty`, `files`), and only for the
  sandbox access token: a tenant API key is refused from a browser origin.
  Without the flag only the WebSocket endpoint (`pty`) works from a page.

## Configuration

Every static method and `Sandbox.create/connect` accept:

| Option | Default | |
|---|---|---|
| `apiKey` | `SBX_API_KEY` | Tenant API key |
| `baseUrl` | `SBX_BASE_URL`, else `http://127.0.0.1:8080` | Server URL |
| `fetch` | global `fetch` | Inject for tests, proxies, edge runtimes |
| `WebSocket` | global `WebSocket` | Pass `ws`'s `WebSocket` on Node 20 |
| `requestTimeoutMs` | `30000` | Deadline of one non-streaming request attempt; 0 = none |
| `retry` | 3 retries | See above; `false` disables |
| `headers` | — | Extra headers for every request |
| `throwOnNonZeroExit` | `true` | See Commands |
| `commandTimeoutMs` | `60000` | Default per-command limit of `commands.run`; 0 = none |
| `stream` | see above | Reconnect and idle settings |

Every method also takes `{ signal, requestTimeoutMs }`.

Outside browsers the SDK sends `User-Agent: sbx-sdk-ts/<version>`.

### Node 20 and the WebSocket

Node 22+ has a global `WebSocket`; Node 20 does not. Only `sandbox.pty`
needs one. On Node 20:

```ts
import { WebSocket } from 'ws'
const sandbox = await Sandbox.create('python', { WebSocket })
```

When a sandbox object has no access token (`Sandbox.connect(id)` with only
an API key), a PTY has to authenticate with the key in a handshake header,
which the standard WebSocket API cannot set. Node's built-in WebSocket and
`ws` both can, and the SDK uses that; in a browser this combination is
refused with a clear error.

## Module format

The package is **ESM-only**, with type declarations. `require('sbx-sdk')`
still works on Node 20.19+ and 22.12+, which load ES modules synchronously.
One copy of each class means `instanceof` checks on the error hierarchy
cannot be broken by a dual-package split.

## Development

```sh
npm install
npm run typecheck      # tsc --noEmit, strict
npm run lint           # biome
npm test               # unit tests against an in-process mock server
npm run build          # dist/
npm run verify:dist    # packs the tarball and imports it from a scratch project
scripts/live-test.sh   # builds sbxd from this repo, starts it with Postgres and Podman, runs test/live
```

The live tests are skipped unless `SBX_LIVE_BASE_URL` and
`SBX_LIVE_API_KEY` are set (`SBX_LIVE_OTHER_API_KEY`, a second tenant's
key, enables the tenancy test).

## Licence

Apache-2.0.
