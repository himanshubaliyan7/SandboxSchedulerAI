# sbx: Python SDK for Warmbox

Run LLM-generated code in warm, self-hosted sandboxes. The SDK talks to an
`sbxd` server (see the repository root) and is shaped like E2B's Python SDK:
`Sandbox.create`, `sandbox.commands.run`, `sandbox.files`, `sandbox.pty`.

```python
from sbx import Sandbox

with Sandbox.create("python") as sandbox:  # killed when the block ends
    result = sandbox.commands.run('python3 -c "print(6*7)"')
    print(result.stdout)  # 42
```

- Blocking (`Sandbox`) and asyncio (`AsyncSandbox`) clients with the same surface.
- Streaming output with callbacks; streams resume at the right byte after a
  dropped connection and report any gap instead of hiding it.
- Interactive PTYs over WebSocket with automatic flow control and re-attach.
- Typed (`py.typed`), two runtime dependencies, Python 3.10+.

## Install

```sh
pip install ./sdk/python            # from a checkout of the repository
```

Runtime dependencies, and why:

| Package | Used for |
|---|---|
| `httpx` | HTTP in both blocking and asyncio form from one library, streamed request and response bodies (uploads, downloads, the NDJSON output stream), and pluggable transports for tests and proxies. |
| `websockets` | The PTY route. It also has a blocking and an asyncio client, answers protocol pings, and lets the frame size limit be set. |

## Configuration

| Setting | Argument | Environment | Default |
|---|---|---|---|
| Server URL | `base_url=` | `SBX_BASE_URL` | `http://127.0.0.1:8080` |
| Tenant API key | `api_key=` | `SBX_API_KEY` | none |

The API key is needed for lifecycle calls (create, list, kill, timeout,
network, events, stats). `Sandbox.create` returns a per-sandbox access token
(`sandbox.access_token`); the SDK then uses that token, not the API key, for
commands, files and PTYs. Neither secret is logged or shown in `repr`.

## Sandboxes

```python
from sbx import NetworkPolicy, Sandbox

sandbox = Sandbox.create(
    "python",
    timeout=300,  # seconds; capped by the template
    envs={"API_BASE": "https://example.com"},  # environment of every command
    metadata={"conversation": "c-123"},
    network=NetworkPolicy(mode="allowlist", allow_domains=["pypi.org", "*.pythonhosted.org"]),
)
print(sandbox.sandbox_id, sandbox.get_info().expires_at)

sandbox.set_timeout(600)  # ends 600 s from now
sandbox.update_network("none")  # cut egress on the live sandbox
sandbox.is_running()  # True
sandbox.stats().memory_bytes
[e.type for e in sandbox.events()]  # claimed, command_started, ...

Sandbox.list(metadata={"conversation": "c-123"})  # -> list[SandboxInfo]
Sandbox.list_templates()

sandbox.kill()  # True; False if there is no such sandbox
sandbox.kill(wait=True)  # returns only after the container is gone
Sandbox.kill("sb-0123456789abcdef")  # by id, without connecting (needs the API key)
```

`kill()` returns as soon as the server has fenced the sandbox (it is unusable
and no longer counts against the quota); the container is removed in the
background. Pass `wait=True`, or poll `get_info().state` for `"destroyed"`,
when that matters. States and reasons are plain strings and unknown values
are passed through, so a newer server does not break an older SDK.

### Connecting from somewhere else

The sandbox id and the access token are enough for the data plane, so a
worker can be handed a sandbox without ever seeing the tenant API key:

```python
worker = Sandbox.connect(sandbox_id, access_token=token)  # no API key
worker.commands.run("echo hi")
worker.files.read("out.txt")
```

Lifecycle calls on such an object raise `ConfigurationError`, and leaving a
`with` block does not kill the sandbox (it cannot). `Sandbox.connect(id)`
with an API key gives the full surface.

## Commands

```python
# A shell command line, as in E2B (run by /bin/sh -c):
result = sandbox.commands.run("ls -la | head -5 && echo done")
result.stdout, result.stderr, result.exit_code, result.reason

# Exactly this program and argument vector, no shell:
sandbox.commands.run("python3", args=["-c", "print('hi')"], envs={"X": "1"}, cwd="/tmp", timeout=30)

# Streaming:
sandbox.commands.run("pip install requests", on_stdout=print, on_stderr=print)
```

**`cmd` and `args`.** Without `args`, `cmd` is a command line for the shell.
With `args` (even `[]`), `cmd` is the executable and no shell is involved, so
nothing in the arguments is interpreted. Use the second form for anything
built from model output.

**Non-zero exit.** Like E2B's Python SDK, `run` raises `CommandExitError`
when the command exits with a non-zero status; the exception carries
`exit_code`, `stdout`, `stderr` and `reason`. A command killed by its
`timeout` raises `CommandTimeoutError` (a subclass, and also a
`TimeoutError`). Pass `check=False` to get the `CommandResult` back
whatever the status:

```python
result = sandbox.commands.run("grep -q needle haystack.txt", check=False)
if result.exit_code == 1:
    ...
```

**Timeouts.** `timeout` is the wall-clock limit of the command in seconds,
enforced inside the sandbox. The default is 60 seconds, as in E2B and in the
TypeScript SDK; pass `timeout=0` for no limit.

**Shell.** A command line without `args` is run by `/bin/sh -c`. E2B uses
`/bin/bash -l -c`; the default image here (Alpine) has no bash, and both
SDKs of this project use `/bin/sh`.

**Retrying a start.** If the connection fails while a command is being
started you cannot know whether it runs. Pass `idempotency_key="..."` and
call `run` again with the same key and command: the server starts it at most
once and the second call attaches to it (`sbxd` with `Idempotency-Key`
support; an older server ignores the key).

### Background commands

```python
server = sandbox.commands.run("python3 -m http.server 8000", background=True)
server.pid

for event in server:  # StdoutEvent, StderrEvent, TruncatedEvent, ExitEvent
    if isinstance(event, sbx.StdoutEvent) and "Serving" in event.text:
        break
server.disconnect()  # stop listening; the command keeps running

handle = sandbox.commands.run("cat", args=[], background=True, stdin=True)
handle.send_stdin("hello\n")
handle.close_stdin()
handle.wait().stdout  # "hello\n"

sandbox.commands.list()  # -> list[CommandInfo]
again = sandbox.commands.connect(server.pid)  # replay from the start, then live
sandbox.commands.kill(server.pid)  # True; False if it had already exited
```

`handle.wait()` raises `CommandExitError` like `run` does, unless the command
was started or connected with `check=False`.

### Streams that survive a broken connection

Output is a long-lived HTTP response, which proxies and networks cut. The
handle counts the bytes it has delivered; when the stream breaks it
reconnects with `from=<offset>` (up to `max_reconnects` times in a row, with
backoff) and continues without repeating or skipping a byte. The sandbox
keeps the last 1 MiB of each command's output for this. If more than that
was missed, the loss is explicit: iteration yields a `TruncatedEvent(offset,
dropped)` and the result has `truncated=True` and `dropped_bytes`. When the
stream cannot be resumed, `StreamError` is raised; the handle stays usable
and a later `wait()` resumes from the same offset.

## Files

Paths are absolute, or relative to `/workspace`.

```python
sandbox.files.write("data/in.csv", "a,b\n1,2\n")  # str, bytes or a file object
with open("big.parquet", "rb") as f:
    sandbox.files.write("data/big.parquet", f)  # streamed in 64 KiB chunks
sandbox.files.write_files([("a.txt", "A"), {"path": "b.bin", "data": b"\0", "mode": 0o600}])

sandbox.files.read("data/in.csv")  # str
sandbox.files.read("data/in.csv", format="bytes")  # bytes
for chunk in sandbox.files.read("data/big.parquet", format="stream"):
    ...  # never the whole file in memory

sandbox.files.list("data")  # -> list[EntryInfo]: name, path, size, mode, is_dir, modified_time
sandbox.files.exists("data/in.csv")
sandbox.files.get_info(
    "data/in.csv"
)  # EntryInfo(name, path, size, mode, is_dir, ...); PathNotFoundError if absent
sandbox.files.make_dir("out/plots")  # True if it created the directory, False if it was there
sandbox.files.remove("data")  # a file, or a directory and everything in it
```

## PTY

```python
import sys


def show(data: bytes) -> None:
    sys.stdout.buffer.write(data)
    sys.stdout.buffer.flush()


terminal = sandbox.pty.create(80, 24, show)  # /bin/sh on a pseudo-terminal
terminal.send_input("stty size\n")
terminal.resize(120, 40)
terminal.send_input(b"\x03")  # Ctrl+C
terminal.send_input("exit\n")
terminal.wait().exit_code

other = sandbox.pty.connect(terminal.pid, show)  # re-attach: replays, then live
sandbox.pty.send_input(pid, "ls\n")
sandbox.pty.resize(pid, 100, 30)
sandbox.pty.kill(pid)
```

`on_data` receives raw bytes on a background reader (a thread for `Sandbox`,
a task for `AsyncSandbox`, where it may also be a coroutine function). The
handle does the protocol work:

- **Flow control.** Output is acknowledged after `on_data` has returned, at
  least every 8192 bytes (the server needs steps below 10000), so a slow
  consumer pauses the command instead of losing output. `flow_control=False`
  never pauses the command; a slow consumer then gets gaps.
- **Keepalive.** Protocol pings and the application-level ping channel are
  answered; a silent connection is treated as dead after
  `stream_idle_timeout`.
- **Re-attach.** If the connection drops, the handle attaches again at the
  byte where output stopped. Lost output is reported through `on_truncated`
  and `result.truncated`. If the sandbox is gone, `wait()` raises the reason
  (`SandboxNotRunningError.reason`, e.g. `idle_timeout`).
- **Close codes.** The server ends a session with an application close code
  (4000-4099). The final ones (sandbox terminated, unauthorised, not found,
  bad request) become the matching typed error at once; the transient ones
  (server shutting down, agent unavailable, rate limited) make the handle
  attach again. A server without close codes is handled as before, through
  the HTTP status of the next handshake.
- The frame size limit is raised to 4 MiB (the server sends up to 32769 bytes).

`wait()` on a PTY returns the exit status and does not raise on non-zero: for
a shell that is just the status of the last command typed.

## Asyncio

Everything above with `await`:

```python
import asyncio
from sbx import AsyncSandbox


async def main() -> None:
    async with await AsyncSandbox.create("python") as sandbox:
        result = await sandbox.commands.run("echo hi", on_stdout=print)
        handle = await sandbox.commands.run("sleep 5; echo done", background=True)
        async for event in handle:
            print(event)
        await sandbox.files.write("a.txt", "x")
        async for chunk in await sandbox.files.read("a.txt", format="stream"):
            ...


asyncio.run(main())
```

## Webhooks

Instead of polling, let the server call you when a sandbox is created, starts
running, terminates, or a command finishes. Managing webhooks needs the tenant
API key:

```python
from sbx import Webhooks

with Webhooks() as webhooks:  # api_key / base_url as for Sandbox
    hook = webhooks.create(
        "https://hooks.example.com/warmbox",
        event_types=["sandbox.terminated", "command.finished"],  # omit for all
    )
    save_somewhere_safe(hook.secret)  # whsec_...; the server shows it this once

    webhooks.test(hook.id)  # sends a webhook.test event now -> .delivered, .status
    webhooks.update(hook.id, enabled=False)  # url, event_types, description, enabled
    webhooks.get(hook.id)  # consecutive_failures, failing_since, dropped_events, ...
    webhooks.deliveries(hook.id, limit=20)  # recent attempts: status, response_status, error
    webhooks.rotate_secret(hook.id)  # new .secret; the old one keeps signing for 24h
    webhooks.list()
    webhooks.delete(hook.id)
```

`AsyncWebhooks` is the same with `await`.

In the handler that receives the requests, verify each one before you use it.
`verify_webhook` needs no client and no network: pass it the **raw** body, the
headers and the secret.

```python
from sbx import WebhookVerificationError, verify_webhook


def handle(body: bytes, headers) -> int:  # your framework's raw body and headers
    try:
        event = verify_webhook(body, headers, WEBHOOK_SECRET)
    except WebhookVerificationError:
        return 400
    if event.type == "sandbox.terminated":
        print(event.sandbox_id, event.data.get("reason"))
    return 204
```

It checks the HMAC-SHA256 signature in constant time and refuses a
`Webhook-Timestamp` more than `tolerance` seconds (300 by default) from the
local clock. The signatures follow the
[Standard Webhooks](https://www.standardwebhooks.com) specification, so that
project's `standardwebhooks` package verifies them too. Deliveries are at
least once: deduplicate on `event.id`. The event catalogue, the retry schedule
and what happens when your endpoint is down are in
[`docs/webhooks.md`](../../docs/webhooks.md).

## Errors

Every exception derives from `SbxError`. Server errors are `ApiError`
subclasses with `status_code`, `code` (the server's stable code), `message`,
`request_id`, `details` and `retry_after`.

| Exception | When |
|---|---|
| `AuthenticationError` | 401: missing or wrong key or token |
| `SandboxNotFoundError`, `TemplateNotFoundError`, `CommandNotFoundError`, `PathNotFoundError` | 404 (all are `NotFoundError`). Another tenant's sandbox is "not found". |
| `InvalidArgumentError`, `PayloadTooLargeError` | 400 / 413: validation |
| `PermissionDeniedError` | 403: the sandbox user may not access the path |
| `SandboxNotRunningError` (`.state`, `.reason`) | 409: the sandbox ended: `released`, `idle_timeout`, `max_lifetime`, `oom_killed`, ... |
| `SandboxTerminatedError` | subclass: it died while the request or stream was being served |
| `ConflictError` | other 409s: `is_directory`, `stdin_closed`, `process_exited`, `tag_in_use`, `network_not_attached` |
| `CommandStartError` | the command could not start: `command_not_found`, `bad_cwd`, `exec_format` |
| `RateLimitError` (`.retry_after`, `.limit_class`) | 429 `rate_limited` |
| `QuotaExceededError` (`.retry_after`) | 429 `quota_exceeded`: too many concurrent sandboxes |
| `PoolEmptyError`, `AgentUnavailableError` | 503 / 502 (both `ServiceUnavailableError`) |
| `NoSpaceError` | 507: the sandbox's storage is full |
| `SandboxTimeoutError` | a client-side deadline or the server's `timeout`; also a `TimeoutError` |
| `TransportError`, `StreamError` | the connection failed / a stream could not be resumed |
| `CommandExitError`, `CommandTimeoutError` | a command exited non-zero / hit its timeout (not `ApiError`s) |
| `ConfigurationError` | no credentials for the call, bad `base_url` |

### Retries

Requests are retried automatically only when that is safe:

- `429 rate_limited` on every call (the limiter answers before anything happens);
- `503 pool_empty` / `shutting_down` on `Sandbox.create` and on idempotent calls;
- `502 agent_unavailable`, connection errors and timeouts on idempotent calls
  (reads, lists, `set_timeout`, `update_network`, `kill`, uploads of data that
  can be re-read). Starting a command and writing stdin are never repeated.

The server's `Retry-After` is honoured; otherwise the delay doubles from
0.25 s to 8 s with jitter. Three retries by default.

```python
from sbx import RetryPolicy, Sandbox

Sandbox.create("python", retry=RetryPolicy(max_retries=6, max_retry_after=60))
Sandbox.create("python", retry=RetryPolicy.none())  # off
Sandbox.create("python", retry=RetryPolicy(retry_quota_exceeded=True))
```

`429 quota_exceeded` is not retried unless asked: it clears only when a
sandbox is deleted or expires.

### Timeouts and transport hooks

```python
import httpx

Sandbox.create("python", request_timeout=20)  # seconds per ordinary request (default 60)
Sandbox.create("python", headers={"X-Trace": "abc"})
Sandbox.create("python", http_client=httpx.Client(verify="/etc/ssl/internal-ca.pem"))
Sandbox.create("python", transport=httpx.MockTransport(handler))  # tests
Sandbox.create("python", ws_base_url="wss://ws.example.com")  # WebSocket origin, if it differs
```

A client passed with `http_client=` is not closed by the SDK. Every request
carries `User-Agent: sbx-python/<version> (...)`.

## Compatibility with older servers

| Feature | With a server that predates it |
|---|---|
| `files.get_info` / `exists` (stat route) | falls back to listing the parent directory (also when only the sandbox's agent is older) |
| `files.make_dir` result | always `False` |
| WebSocket close codes | the reason comes from the HTTP status of the re-attach, as before |
| `idempotency_key` | ignored by the server: a repeated `run` starts a second command |

## Differences from E2B

| | E2B | sbx |
|---|---|---|
| Template | optional | required first argument of `create` |
| Command timeout default | 60 s | 60 s (`timeout=0` for none) |
| `commands.run(..., args=[...])` | not available | runs without a shell |
| Non-zero exit | raises `CommandExitException` | raises `CommandExitError`; `check=False` to opt out |
| `pid` | integer | opaque string |
| `files.make_dir` | returns whether it created the directory | the same |
| `pty.create` | `create(size, ...)`, output via `handle.wait(on_pty=...)` | `create(cols, rows, on_data, ...)` |
| Pause / resume, `get_host`, `files.watch_dir`, `files.rename` | yes | not offered by the server |

## Examples

`examples/` has three runnable scripts (set `SBX_API_KEY`, and `SBX_BASE_URL`
unless the server is on `127.0.0.1:8080`):

- `run_code.py`: run generated Python, turn failures into text for the model;
- `interactive_pty.py`: a shell in the sandbox attached to your terminal;
- `process_file.py`: upload a CSV, analyse it, download the result.

## Development

```sh
cd sdk/python
python3 -m venv .venv && .venv/bin/pip install -e '.[dev]'
.venv/bin/pytest                 # unit tests against an in-process fake server
.venv/bin/ruff check . && .venv/bin/ruff format --check .
.venv/bin/pyright
```

The asyncio client in `src/sbx/_async` is the source of truth. The blocking
client in `src/sbx/_sync`, and `tests/_sync`, are generated from it:

```sh
.venv/bin/python scripts/unasync.py          # after editing anything under _async
.venv/bin/python scripts/unasync.py --check  # what the test suite runs
```

Only `_compat.py` is written by hand for both (asyncio primitives on one
side, threads on the other), so the two clients cannot drift apart.

### Live tests

`tests/live` runs against a real server and is skipped unless
`SBX_LIVE_BASE_URL` and `SBX_LIVE_API_KEY` are set (optionally
`SBX_LIVE_OTHER_API_KEY`, a key of a second tenant, and `SBX_LIVE_TEMPLATE`).
The template needs `python3`, a POSIX shell and network mode `allowlist`.

`scripts/live_test.sh` does the whole session: it builds `sbxd` from this
repository into a temporary directory, starts a private Postgres, and then,
holding a lock file so that it never overlaps with another server run by the
same user, creates tenants, keys and a template, starts the server, runs
`pytest -m live`, stops the server and removes its containers. Set
`SBX_PODMAN_LOCK` to the lock file your team shares.

## Licence

Apache-2.0.
