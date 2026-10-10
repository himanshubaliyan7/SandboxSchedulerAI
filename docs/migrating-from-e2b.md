# Migrating from E2B to Warmbox

Checked on 2026-10-10 against E2B's SDK source (`e2b` 2.55 for Python and
JavaScript), its documentation and its runtime repository, and against the
Warmbox code in this repository. The Warmbox snippets below were run against a
live server unless marked otherwise. [Sources](#sources) lists what was read.

## Who this is for

You run agent code in E2B sandboxes, on E2B Cloud or on a self-hosted E2B, and
you want to run it on a Linux machine you control instead, with fewer moving
parts.

Read the fit check before anything else. Warmbox is a smaller thing than E2B,
and several E2B features have no equivalent.

### What you get

- **One Linux box, one unprivileged user.** Rootless Podman, Postgres and three
  Go binaries. No root daemon, no privileged container, no Kubernetes, no Nomad,
  no `/dev/kvm`.
- **Warm claims.** Sandboxes are created ahead of time. A claim is a database
  update: p50 6-9 ms in the project's own measurements on a laptop
  ([`bench/RESULTS.md`](../bench/RESULTS.md)).
- **No network unless you ask.** A sandbox has no network interface. Egress goes
  through an allowlisting proxy, and a template sets the widest mode a sandbox
  may request.
- **No metering.** There is no per-second billing and no plan limit. You pay for
  the machine.
- **Apache-2.0**, and SDKs shaped like E2B's, so most call sites keep their
  names.

### What you lose today

| You use this in E2B | In Warmbox |
|---|---|
| MicroVM isolation (one Firecracker VM per sandbox) | **Not available.** The default and only tested tier is a hardened rootless container that shares the host kernel. Read the [threat model](../README.md#threat-model). gVisor and Firecracker tiers are planned, not shipped. |
| `pause`, `connect` to resume, `lifecycle={"on_timeout": "pause"}`, `create_snapshot`, `fork` | Not available. No memory or filesystem snapshots. |
| Sandboxes that outlive a session, volumes (`volume_mounts`) | Not available. A sandbox is destroyed when it ends, and its files with it. |
| `get_host(port)` / `getHost(port)`, public sandbox URLs | Not available. Nothing outside the sandbox can reach a port inside it. |
| Code interpreter (`e2b_code_interpreter`, `run_code`, code contexts, charts) | Not available. Every command starts a fresh process. Variables do not carry over between calls; files in `/workspace` do. |
| `sandbox.git` | Not available. Use the `git` binary if your image has it. |
| `files.watch_dir`, `files.rename`, `upload_url` / `download_url` | Not available. |
| `user="root"` on commands, `sudo`, `apt install` at runtime | Not available. Every process is uid 1000 with no capabilities, on a read-only root filesystem. |
| Templates built by the platform (`Template()` builder, `e2b template create`, start and ready commands) | You build an OCI image yourself and pull it on the host. There is no start command; the image's `ENTRYPOINT` and `CMD` are not run. |
| Desktop and browser templates | Out of scope. |
| Secrets, workload identity, per-host request transforms, bring-your-own proxy | Not available. |
| More than one node | Single node only. |
| `pip install e2b`, `npm install e2b` | The SDKs are not on PyPI or npm yet. Install them from a checkout. |
| A dashboard | None. Administration is the `sbxd admin` command line. |

Also know that Warmbox is an early project. It works end to end and is tested
against real Podman, but it has run on one development machine.

**If you depend on any row above, do not migrate yet.**

### If your goal is only "self-hosted E2B"

Check E2B's own option first. The E2B runtime repository now ships
[E2B Embed](https://github.com/e2b-dev/runtime/blob/main/embed/README.md): the
whole stack on a single Linux host with `docker compose up -d --wait`,
Apache-2.0, same SDK and API as E2B Cloud. That is the lower-effort move if your
host qualifies, and it keeps microVM isolation and pause/resume.

Embed's Compose guide lists what it needs: KVM (`/dev/kvm`, so bare metal or a
VM with nested virtualisation) and `/dev/net/tun`, Docker Engine 27 or newer,
12 GiB RAM recommended and 20 GiB of disk. Its compose file runs the
orchestrator as a privileged container on the host network.

Warmbox is the option when you have no KVM, cannot or will not run privileged
containers, or want something small enough to read. It is not a stronger
sandbox than E2B. It is a weaker isolation boundary with a lighter footprint.

## Concept map

| Concept | E2B | Warmbox |
|---|---|---|
| API key | `E2B_API_KEY`, `api_key=` | `SBX_API_KEY`, `api_key=`. A tenant key, created with `sbxd admin key create`. |
| Server address | `E2B_DOMAIN`, `E2B_API_URL` | `SBX_BASE_URL`, `base_url=`. Default `http://127.0.0.1:8080`. |
| Per-sandbox credential | Handled inside the SDK | `sandbox.access_token` (`sbt_...`), returned by create. Opens only that sandbox's commands and files. You can hand it to a worker or a browser instead of the API key. |
| Template | Built by E2B from a recipe; optional in `Sandbox.create()` (default `base`) | A row made with `sbxd admin template apply`: image, resource limits, widest network mode, timers, warm pool size. **Required** first argument of `Sandbox.create`. |
| Snapshot of a running sandbox | `create_snapshot` | No equivalent. |
| Sandbox | A microVM | A rootless container, used once and destroyed. |
| Sandbox timeout | `timeout` (Python, seconds) / `timeoutMs`; default 300 s; `set_timeout` | `timeout` / `timeoutMs` is the **maximum lifetime**. A separate **idle timeout** also applies. See below. |
| What happens at timeout | Kill, or pause if `lifecycle.on_timeout` says so | Always destroyed. |
| Metadata | `metadata=` | `metadata=`, and `Sandbox.list(metadata=...)` filters on it. |
| Environment | `envs=` on create and on `commands.run` | The same. |
| Command | `sandbox.commands.run(cmd)` | The same name. Different shell, user and working directory. See below. |
| Background command | `background=True`, `CommandHandle` | The same. `pid` is an opaque string, not an integer. |
| PTY | `sandbox.pty` | `sandbox.pty`, with a different `create` signature in Python. |
| Files | `sandbox.files` | `sandbox.files`: `read`, `write`, `list`, `exists`, `get_info`, `make_dir`, `remove`. Python also has `write_files`. No `rename`, no `watch_dir`. |
| Network policy | Internet on by default; `allow_internet_access`, `network={"allow_out", "deny_out"}`, `update_network` | Off by default. `network=NetworkPolicy(mode=..., allow_domains=[...])`, `update_network`. See below. |
| Events and webhooks | A lifecycle events API and webhooks (listed in the documentation index; not read in detail) | `sandbox.events()` returns the sandbox's event log (`claimed`, `transition`, `command_started`, `command_finished`). Lifecycle webhooks (`sandbox.created`, `sandbox.running`, `sandbox.terminated`, `sandbox.destroyed`, `command.started`, `command.finished`) push the same events to an HTTPS endpoint, signed and retried; see [`webhooks.md`](webhooks.md). |
| Resource usage | `get_metrics` | `sandbox.stats()` (Python), `sandbox.getStats()` (TypeScript): cgroup counters. |
| Port exposure | `get_host(port)` | No equivalent. |
| Code interpreter | `e2b_code_interpreter`, `run_code` | No equivalent. |
| Self-hosting components | API, orchestrator with Firecracker, client proxy, dashboard, Postgres, Redis, ClickHouse, Vector, OpenTelemetry collector (the services in Embed's `compose.yaml`) | `sbxd`, `sbx-agent`, Postgres, rootless Podman. |

### Differences that will bite

**Three timers, not one.** E2B has one timeout: the sandbox ends that long after
create or after the last `set_timeout`. Warmbox has three:

| Timer | Set by | Ends | Reason reported |
|---|---|---|---|
| Idle timeout | Template, `--idle-timeout` (default 10 m, `0` disables) | The sandbox, after that long without activity | `idle_timeout` |
| Maximum lifetime | `timeout` at create and `set_timeout`, capped by the template's `--max-lifetime` (default 1 h) | The sandbox | `max_lifetime` |
| Warm TTL | Template, `--warm-ttl` (default 30 m) | A warm sandbox nobody claimed. You never see it. | none |

Consequences:

- "Activity" means commands, file calls and delivered output. `set_timeout`
  and `get_info` are **not** activity. An E2B keep-alive loop that calls
  `set_timeout` every minute does not stop the idle timer. Either set
  `--idle-timeout 0` on the template, or keep the sandbox busy with a cheap
  data call such as `sandbox.commands.list()`.
- If you omit `timeout`, E2B gives you 300 s. Warmbox gives you the template's
  maximum lifetime.
- A `timeout` above the template's maximum is refused at create with
  `400 invalid_timeout`. The same value passed to `set_timeout` is accepted and
  silently capped; read `expires_at` from the result.
- A client cannot change the idle timeout. It belongs to the template.

**A claim cannot change the template.** CPU, memory, image and the widest
network mode are fixed per template, because the sandbox already exists when
you claim it. Make one template per shape you need.

**A sandbox is never reused and never paused.** `Sandbox.connect(id)` attaches
to a running sandbox. It does not resume anything.

**Network is off by default and works through a proxy.** A sandbox has only a
loopback interface. When the mode is `allowlist` or `full`, the agent sets
`HTTP_PROXY`, `HTTPS_PROXY` and `ALL_PROXY` to `http://127.0.0.1:3128`. So:

- Programs that honour those variables work: `pip`, `curl`, `requests`, `npm`,
  `git` over HTTPS.
- Anything else does not: raw sockets, DNS lookups, UDP, `git` over SSH,
  database drivers. E2B filters real network traffic; Warmbox has no route at
  all.
- A template with `--network allowlist` allows nothing by itself. A sandbox
  created without `network=` gets mode `allowlist` with an empty list. Pass the
  hostnames at create or with `update_network`.
- A sandbox created with mode `none` has no proxy and cannot be given one later
  (`409 network_not_attached`). Create it with `allowlist`, then tighten.
- A sandbox can ask for the template's mode or a stricter one, never a wider
  one (`400 network_not_allowed`).
- Private, loopback, link-local and cloud metadata addresses are always denied.

**The shell is `/bin/sh -c`, not a login bash.** E2B runs every command as
`/bin/bash -l -c`. Warmbox runs a command line with `/bin/sh -c`, and Alpine
images have no bash at all. Scripts that use bash syntax (`[[ ]]`, arrays,
`source`) need `bash -c '...'` and an image that contains bash. Nothing reads
`.bashrc` or `.profile`. With `args=[...]` no shell is involved; use that form
for anything built from model output.

**The root filesystem is read-only.** Writable space is two size-capped tmpfs
mounts, `/workspace` and `/tmp`. They are charged to the sandbox's memory
limit, and they start empty.

- `HOME` is `/workspace`, and commands start there. E2B's `/home/user` does
  not exist as a writable place.
- `pip install` works only if the network policy allows your package index
  (`pypi.org` and `files.pythonhosted.org` for PyPI) and the tmpfs is large
  enough. It installs into the user site, `/workspace/.local`. Scripts land in
  `/workspace/.local/bin`, which is not on `PATH`. A venv under `/workspace`
  also works.
- Files your image puts somewhere writable in E2B are read-only here. Copy
  what must change into `/workspace` at the start of a session.

**The process is uid 1000, not root, and cannot become root.** There is no
`sudo`. `apt-get install` fails with a permission error. Bake every system
dependency into the image. Do not rely on the user's name: it depends on the
image and the host.

**A non-zero exit raises**, as in E2B: `CommandExitError` here,
`CommandExitException` (Python) or `CommandExitError` (JavaScript) there.
Warmbox adds `check=False` (Python) and `throwOnNonZeroExit: false`
(TypeScript) to get the result back instead.

## Step-by-step migration

### 1. Stand up Warmbox

Follow the [quick start](../README.md#quick-start) and the
[operating notes](operations.md). Run `bin/sbxd preflight` and fix every `FAIL`
line before going further.

### 2. Turn your E2B template into an image

A Warmbox template points at an OCI image that is already in the local store of
the user who runs `sbxd`. Nothing is built or pulled for you.

Translate your E2B recipe into a Dockerfile. If you already have an
`e2b.Dockerfile`, start from it.

| E2B template step | Here |
|---|---|
| `from_python_image("3.12")`, `from_image(...)`, `from_dockerfile(...)` | `FROM` |
| `pip_install([...])`, `apt_install([...])`, `run_cmd(...)` | `RUN` |
| `copy(src, dest)` | `COPY`. Put files outside `/workspace` and `/tmp`; those are mounted over. |
| `set_envs({...})` | `ENV` |
| `set_start_cmd(...)`, ready command | No equivalent. Start the process after the claim with a background command. |
| `set_user(...)`, `set_workdir(...)` | Not used. Processes run as uid 1000 and start in `/workspace`. |
| `cpu_count`, `memory_mb` | `--cpus`, `--memory` on the template |

A worked example, a Python image with numpy and pandas:

```dockerfile
# Dockerfile
FROM docker.io/library/python:3.12-slim
RUN pip install --no-cache-dir numpy pandas
```

```sh
podman build -t localhost/pydata:1 .       # as the user that runs sbxd
podman images localhost/pydata             # the image must be listed here
```

An image from a registry is pulled the same way: `podman pull <image>`.

`python:3.12-slim` contains bash but not git. `python:3.12-alpine` is smaller
and contains neither.

### 3. Register the template

```sh
export SBX_DATABASE_URL='postgres://postgres:sbx@127.0.0.1:55432/postgres?sslmode=disable'

bin/sbxd admin template apply --id pydata \
    --image localhost/pydata:1 \
    --cpus 1 --memory 1g --pids 256 \
    --workdir-size 256m --tmp-size 64m \
    --network allowlist \
    --idle-timeout 10m --max-lifetime 1h \
    --warm-target 2 --max-total 6
bin/sbxd admin template list
```

| Flag | Meaning | Default |
|---|---|---|
| `--id` | Template id; what you pass to `Sandbox.create` | required |
| `--image` | Image, already present for the Podman user | required |
| `--cpus`, `--memory`, `--pids`, `--nofile` | cgroup and rlimit caps | `1`, `512m`, `256`, `4096` |
| `--workdir-size`, `--tmp-size` | Size of the `/workspace` and `/tmp` tmpfs | `128m`, `64m` |
| `--network` | Widest mode a sandbox may ask for: `none`, `allowlist`, `full` | `none` |
| `--idle-timeout`, `--max-lifetime`, `--warm-ttl` | The three timers; `0` disables | `10m`, `1h`, `30m` |
| `--warm-target` | Sandboxes kept ready | `2` |
| `--max-total` | Cap on all sandboxes of this template; `0` for none | `0` |
| `--refill-rate` | Concurrent creates | `2` |
| `--runtime` | `crun` or `runsc`. Only `crun` is enabled and tested. | `crun` |
| `--file` | A JSON file with one template or an array, instead of the flags | |

The two tmpfs sizes must fit inside `--memory` together, because tmpfs pages
count as memory. Leave room for the processes: with `--memory 1g` and
`--workdir-size 256m`, a full `/workspace` leaves about 700 MB for Python.

`sbxd admin template show --id pydata --json` prints the stored template in the
shape `--file` reads. Applying an id again creates a new version.

### 4. Create a tenant and an API key

```sh
bin/sbxd admin tenant create --id acme --max-concurrent 20 --rate 50 --burst 100
bin/sbxd admin key create --tenant acme      # prints the key once
```

`--max-concurrent` is the tenant's quota of live sandboxes. `--rate` and
`--burst` size its request buckets.

### 5. Install the SDK

```sh
pip install ./sdk/python                                   # Python 3.10+, import name: sbx
(cd sdk/typescript && npm install && npm run build)        # package name: sbx-sdk
```

Neither is published yet. For TypeScript, make your project depend on the
built checkout (a path dependency or a link to `sdk/typescript`).

```sh
export SBX_API_KEY=sbx_...
export SBX_BASE_URL=http://127.0.0.1:8080
```

### 6. Change the code: Python

Before, with E2B:

```python
from e2b import CommandExitException, PtySize, Sandbox

sandbox = Sandbox.create(
    "my-template",
    timeout=300,
    envs={"MY_VAR": "1"},
    metadata={"user_id": "u_123"},
    network={"deny_out": lambda ctx: [ctx.all_traffic],
             "allow_out": ["pypi.org", "files.pythonhosted.org"]},
)

result = sandbox.commands.run("python3 -c 'import pandas; print(pandas.__version__)'")
print(result.stdout, result.exit_code)

try:
    sandbox.commands.run("ls /nonexistent")
except CommandExitException as e:
    print(e.exit_code, e.stderr)

sandbox.commands.run("for i in 1 2 3; do echo line $i; done",
                     on_stdout=print, on_stderr=print)

server = sandbox.commands.run("python3 -u -m http.server 8000", background=True, timeout=0)
for stdout, stderr, _ in server:
    if stdout and "Serving" in stdout:
        break
server.kill()

sandbox.files.write("/home/user/data/in.csv", "a,b\n1,2\n")
text = sandbox.files.read("/home/user/data/in.csv")

sandbox.set_timeout(600)

terminal = sandbox.pty.create(PtySize(rows=24, cols=80))
sandbox.pty.send_stdin(terminal.pid, b"echo hello\nexit\n")
terminal.wait(on_pty=lambda data: print(data.decode(), end=""))

sandbox.kill()
```

After, with Warmbox. This script was run against a live server:

```python
import sys

import sbx
from sbx import NetworkPolicy, Sandbox

# Create. The template is required; network is "none" unless you ask.
sandbox = Sandbox.create(
    "pydata",
    timeout=300,  # max lifetime in seconds, not an idle timeout
    envs={"MY_VAR": "1"},
    metadata={"user_id": "u_123"},
    network=NetworkPolicy(mode="allowlist", allow_domains=["pypi.org", "files.pythonhosted.org"]),
)
print(sandbox.sandbox_id)

# Run a command and read its output.
result = sandbox.commands.run("python3 -c 'import pandas; print(pandas.__version__)'")
print(result.stdout, result.exit_code)

# A non-zero exit raises, as in E2B.
try:
    sandbox.commands.run("ls /nonexistent")
except sbx.CommandExitError as e:
    print(e.exit_code, e.stderr)

# Or get the result back whatever the status.
result = sandbox.commands.run("ls /nonexistent", check=False)

# Streaming callbacks.
sandbox.commands.run(
    "for i in 1 2 3; do echo line $i; sleep 0.2; done",
    on_stdout=lambda text: print("out:", text, end=""),
    on_stderr=lambda text: print("err:", text, end=""),
)

# Background command. timeout=0 lifts the 60 s per-command limit.
server = sandbox.commands.run("python3 -u -m http.server 8000", background=True, timeout=0)
print(server.pid)  # an opaque string, not a kernel pid
for event in server:
    if isinstance(event, sbx.StdoutEvent) and "Serving" in event.text:
        break
server.disconnect()  # stop listening; the command keeps running
print([c.pid for c in sandbox.commands.list()])
server.kill()

# Files.
sandbox.files.write("data/in.csv", "a,b\n1,2\n")  # relative to /workspace
with open(__file__, "rb") as f:
    sandbox.files.write("/workspace/upload.bin", f)  # streamed
text = sandbox.files.read("data/in.csv")
raw = sandbox.files.read("/workspace/upload.bin", format="bytes")
print([entry.name for entry in sandbox.files.list("data")])

# Timeout: the sandbox now ends 600 s from this call (capped by the template).
info = sandbox.set_timeout(600)
print(info.expires_at)


# PTY.
def show(data: bytes) -> None:
    sys.stdout.buffer.write(data)
    sys.stdout.buffer.flush()


terminal = sandbox.pty.create(80, 24, show)  # cols, rows, on_data; runs /bin/sh
terminal.send_input("echo hello from the pty\n")
terminal.resize(120, 40)
terminal.send_input("exit\n")
print(terminal.wait().exit_code)

# Kill.
sandbox.kill()
```

What changed, line by line:

| E2B (Python) | Warmbox (Python) |
|---|---|
| `from e2b import Sandbox` | `from sbx import Sandbox` |
| `Sandbox.create()` with no template | `Sandbox.create("pydata")`; the template is required |
| `allow_internet_access=False` | Leave `network` out on a `none` template, or pass `network="none"` |
| `network={"allow_out": [...], "deny_out": [...]}` | `network=NetworkPolicy(mode="allowlist", allow_domains=[...], allow_cidrs=[...], deny_cidrs=[...])` |
| `CommandExitException` | `sbx.CommandExitError`; a command timeout is `sbx.CommandTimeoutError` |
| `commands.run(cmd, user="root")` | No `user`. Adds `args=[...]`, `check=`, `tag=`, `idempotency_key=`. |
| `for stdout, stderr, pty in handle:` | `for event in handle:` yields `StdoutEvent`, `StderrEvent`, `TruncatedEvent`, `ExitEvent` (`.text`, `.data`) |
| `handle.pid` is an `int` | `handle.pid` is a `str` |
| `commands.connect(pid)` | The same; it replays buffered output first. `from_offset=` skips it. |
| `pty.create(PtySize(rows, cols))`, output through `handle.wait(on_pty=...)` | `pty.create(cols, rows, on_data)`; output goes to `on_data` |
| `pty.send_stdin(pid, data)` | `handle.send_input(data)` or `sandbox.pty.send_input(pid, data)` |
| `pty.resize(pid, PtySize(...))` | `handle.resize(cols, rows)` or `sandbox.pty.resize(pid, cols, rows)` |
| PTY `wait()` raises on a non-zero exit | PTY `wait()` returns the result |
| `set_timeout(n)` returns `None` | Returns the updated `SandboxInfo` |
| `sandbox.get_host(8000)` | No equivalent |
| `sandbox.pause()`, `Sandbox.connect(id)` to resume | No equivalent; `connect` only attaches |
| `AsyncSandbox` | `AsyncSandbox`, same surface as the blocking client |

`with Sandbox.create("pydata") as sandbox:` kills the sandbox when the block
ends, as E2B's context manager does.

A note on the background example. `python3 -m http.server` buffers its stdout
when it is not a terminal, so without `-u` the "Serving" line does not arrive
and the loop waits. That is Python's behaviour, not the sandbox's; it applies in
E2B as well.

### 7. Change the code: TypeScript

Before, with E2B:

```ts
import { CommandExitError, Sandbox } from 'e2b'

const sandbox = await Sandbox.create('my-template', {
  timeoutMs: 5 * 60_000,
  envs: { MY_VAR: '1' },
  metadata: { userId: 'u_123' },
  network: {
    denyOut: ({ allTraffic }) => [allTraffic],
    allowOut: ['pypi.org', 'files.pythonhosted.org'],
  },
})

const result = await sandbox.commands.run("python3 -c 'import pandas; print(pandas.__version__)'")
console.log(result.stdout, result.exitCode)

try {
  await sandbox.commands.run('ls /nonexistent')
} catch (e) {
  if (e instanceof CommandExitError) console.log(e.exitCode, e.stderr)
}

await sandbox.commands.run('for i in 1 2 3; do echo line $i; done', {
  onStdout: (text) => process.stdout.write(text),
  onStderr: (text) => process.stderr.write(text),
})

const server = await sandbox.commands.run('python3 -u -m http.server 8000', {
  background: true,
  timeoutMs: 0,
})
await server.kill()

await sandbox.files.write('/home/user/data/in.csv', 'a,b\n1,2\n')
const text = await sandbox.files.read('/home/user/data/in.csv')

await sandbox.setTimeout(600_000)

const pty = await sandbox.pty.create({
  cols: 80,
  rows: 24,
  onData: (data) => process.stdout.write(data),
})
await sandbox.pty.sendInput(pty.pid, new TextEncoder().encode('echo hello\nexit\n'))
await pty.wait()

await sandbox.kill()
```

After, with Warmbox. This script passed `tsc --strict` against the SDK and was
run as written on Node 24:

```ts
import { CommandExitError, Sandbox } from 'sbx-sdk'

// Create. The template is required; network is "none" unless you ask.
const sandbox = await Sandbox.create('pydata', {
  timeoutMs: 5 * 60_000, // max lifetime, not an idle timeout
  envs: { MY_VAR: '1' },
  metadata: { userId: 'u_123' },
  network: { mode: 'allowlist', allowDomains: ['pypi.org', 'files.pythonhosted.org'] },
})
console.log(sandbox.sandboxId)

// Run a command and read its output.
const result = await sandbox.commands.run("python3 -c 'import pandas; print(pandas.__version__)'")
console.log(result.stdout, result.exitCode)

// A non-zero exit throws, as in E2B.
try {
  await sandbox.commands.run('ls /nonexistent')
} catch (e) {
  if (e instanceof CommandExitError) console.log(e.exitCode, e.stderr)
  else throw e
}

// Or get the result back whatever the status.
const r2 = await sandbox.commands.run('ls /nonexistent', { throwOnNonZeroExit: false })
console.log(r2.exitCode)

// Streaming callbacks.
await sandbox.commands.run('for i in 1 2 3; do echo line $i; sleep 0.2; done', {
  onStdout: (text) => process.stdout.write(`out: ${text}`),
  onStderr: (text) => process.stderr.write(`err: ${text}`),
})

// Background command. timeoutMs: 0 lifts the 60 s per-command limit.
const server = await sandbox.commands.run('python3 -u -m http.server 8000', { background: true, timeoutMs: 0 })
console.log(server.pid) // an opaque string, not a kernel pid
for await (const event of server) {
  if (event.type === 'stdout' && event.text.includes('Serving')) break
}
server.disconnect() // stop listening; the command keeps running
console.log((await sandbox.commands.list()).map((c) => c.pid))
await server.kill()

// Files.
await sandbox.files.write('data/in.csv', 'a,b\n1,2\n') // relative to /workspace
await sandbox.files.write('/workspace/upload.bin', new Uint8Array([1, 2, 3]))
const text = await sandbox.files.read('data/in.csv')
const bytes = await sandbox.files.read('/workspace/upload.bin', { format: 'bytes' })
console.log((await sandbox.files.list('data')).map((entry) => entry.name), text, bytes.length)

// Timeout: the sandbox now ends 600 s from this call (capped by the template).
const info = await sandbox.setTimeout(600_000)
console.log(info.expiresAt)

// PTY.
const pty = await sandbox.pty.create({
  cols: 80,
  rows: 24,
  onData: (data) => process.stdout.write(data),
})
pty.sendInput('echo hello from the pty\n')
pty.resize({ cols: 120, rows: 40 })
pty.sendInput('exit\n')
console.log((await pty.wait()).exitCode)

// Kill.
await sandbox.kill()
```

What changed:

| E2B (JavaScript) | Warmbox (TypeScript) |
|---|---|
| `import { Sandbox } from 'e2b'` | `import { Sandbox } from 'sbx-sdk'` |
| `Sandbox.create(opts)` with no template | `Sandbox.create('pydata', opts)` |
| `allowInternetAccess`, `network: { allowOut, denyOut }` | `network: { mode, allowDomains, allowCidrs, denyCidrs }` |
| `commands.run(cmd, { user })` | No `user`. Adds `args`, `shell`, `stdin` as a string or bytes, `throwOnNonZeroExit`, `tag`, `idempotencyKey`, `signal`. |
| `handle.pid` is a `number` | A `string` |
| `files.write(path, data)` returns `WriteInfo`; `files.write([entries])` | Returns `void`; one file per call |
| `files.read(path, { format: 'blob' })` | `text`, `bytes` and `stream` only |
| `sandbox.pty.sendInput(pid, bytes)` | `pty.sendInput(textOrBytes)` on the handle, or `sandbox.pty.sendInput(pid, data)` |
| PTY `wait()` throws on a non-zero exit | Returns the result |
| `setTimeout(ms)` returns nothing | Returns the updated `SandboxInfo` |
| `Sandbox.list()` returns a paginator | Returns `SandboxInfo[]` |
| `getHost`, `pause`, `createSnapshot`, `files.watchDir`, `files.rename` | No equivalent |

`await using sandbox = await Sandbox.create(...)` kills the sandbox at the end of
the scope. On Node 20 the PTY needs a `WebSocket` implementation passed in; see
[`sdk/typescript/README.md`](../sdk/typescript/README.md).

### 8. Replace `run_code`

There is no stateful interpreter. `run_code("x = 1")` followed by
`run_code("print(x)")` has no equivalent: the second call would be a new
process. Two patterns cover most uses:

- Send the whole program each time. Keep state in files under `/workspace`.
- Keep the conversation's code in one growing script and rerun it. This is
  only acceptable when reruns are cheap and have no side effects.

Charts and rich results (`execution.results`) are not captured for you. Write
the image to a file and read it with `files.read(path, format="bytes")`.

## Using Warmbox from an agent framework

### Without code: the MCP server

If your framework speaks the Model Context Protocol, point it at `sbx-mcp`. It
is a stdio server with these tools: `list_templates`, `create_sandbox`,
`list_sandboxes`, `kill_sandbox`, `run_command`, `run_python`, `write_file`,
`read_file`, `list_files`, `set_network`.

```json
{
  "mcpServers": {
    "sbx": {
      "command": "/path/to/bin/sbx-mcp",
      "args": ["--template", "pydata"],
      "env": { "SBX_API_KEY": "sbx_...", "SBX_BASE_URL": "http://127.0.0.1:8080" }
    }
  }
}
```

`--template` is the template used when the model does not name one. The API key
is read from the environment only. `run_command` runs a line with `/bin/sh -c`;
`run_python` starts a fresh `python3` on each call. Output above 32 KiB per
stream is cut in the middle (`--max-output-bytes`).

This differs from E2B's MCP gateway, which runs MCP servers inside a sandbox.
`sbx-mcp` runs next to your client and exposes sandboxes as tools.

### With code: a custom tool

Where you handed an E2B sandbox to a framework's "code interpreter" tool, wrap
the SDK in a function and register that. Python:

```python
import sbx
from sbx import Sandbox

sandbox = Sandbox.create("pydata", timeout=900)


def run_python(code: str) -> str:
    """Run Python source in the sandbox and return what the model should see."""
    try:
        sandbox.files.write("/tmp/snippet.py", code)
        r = sandbox.commands.run("python3", args=["/tmp/snippet.py"], timeout=60, check=False)
    except sbx.SandboxNotRunningError as e:
        return f"sandbox ended: {e.reason}"
    out = r.stdout[-8000:] + (("\nstderr:\n" + r.stderr[-4000:]) if r.stderr else "")
    return f"exit_code={r.exit_code} reason={r.reason}\n{out}"
```

TypeScript, run as written:

```ts
import { Sandbox, SandboxTerminatedError } from 'sbx-sdk'

const sandbox = await Sandbox.create('pydata', { timeoutMs: 15 * 60_000, throwOnNonZeroExit: false })

export async function runPython(code: string): Promise<string> {
  try {
    const r = await sandbox.commands.run('python3', { args: ['-'], stdin: code, timeoutMs: 60_000 })
    return `exit_code=${r.exitCode} reason=${r.reason}\n${r.stdout.slice(-8000)}${r.stderr ? `\nstderr:\n${r.stderr.slice(-4000)}` : ''}`
  } catch (e) {
    if (e instanceof SandboxTerminatedError) return `sandbox ended: ${e.reason}`
    throw e
  }
}
```

Three things to keep from these examples:

- Pass model output as a file or on stdin with `args`, not inside a shell
  string.
- Return the exit code and `reason` to the model. `reason` is `exited`,
  `killed`, `timeout` or `oom`.
- Handle the sandbox ending under you. The idle timer or an out-of-memory kill
  ends it, and the error carries the reason (`idle_timeout`, `oom_killed`,
  `max_lifetime`).

## Operational differences

There is no hosted control plane and no dashboard. You run the server, the
database and the host.

**Sizing the warm pool.** `--warm-target` is how many sandboxes of a template
sit ready. A claim takes one and the refill loop makes a replacement in the
background (about 90 ms each in the project's measurements, `--refill-rate` at
a time). Set the target to the burst of creates you want answered without
waiting. When the pool is empty a create waits up to `--claim-wait` (default
5 s) and then fails with `503 pool_empty`. `--max-total` caps warm and claimed
sandboxes of a template together, and each tenant has its own `--max-concurrent`
quota. An idle warm sandbox costs a few megabytes of memory.

**Density ceiling.** On a default host about 190 containers fit per user,
because each holds one key of the kernel keyring quota. `sbxd preflight`
reports the real number. [`operations.md`](operations.md) explains how to lift
it.

**Lingering.** Without `loginctl enable-linger <user>` every sandbox dies when
the user's last login session ends. `sbxd preflight` warns about it.

**Restart.** A restart of `sbxd` ends every tenant session; those sandboxes are
released with reason `host_drain`. Warm sandboxes are left running and are
adopted by the next start, so the pool is not cold. `--shutdown-policy` changes
this; see [`operations.md`](operations.md). Clients must be ready to create a
new sandbox after a restart. Nothing can be resumed.

**Metrics.** `/metrics` serves Prometheus text: pool state per template, claim
waits, terminations by reason, egress decisions, HTTP requests. It is
unauthenticated, which `sbxd` only allows on a loopback listener; otherwise set
`--metrics-token` or `--metrics-listen`.

**Kernel updates are your job.** The sandbox boundary is the host kernel. The
threat model asks for a disposable host that runs nothing else and is patched
within days.

**Egress goes out from your host's address.** There is no bring-your-own-proxy
option.

## Troubleshooting

Each of these was reproduced on a live server. Codes are the ones in
[`api/openapi.yaml`](../api/openapi.yaml).

| What you see | Why | Fix |
|---|---|---|
| `503 pool_empty` (`PoolEmptyError`): "no warm sandbox is available for this template right now" | No warm sandbox appeared within `--claim-wait`. The template is at `--max-total`, or creates are slower than claims. | Raise `--warm-target` or `--max-total`. Check `sbx_pool_sandboxes` and `sbx_pool_create_failures_total` in `/metrics`. The SDKs retry this error by default. |
| `429 quota_exceeded` (`QuotaExceededError`): "the tenant's concurrent sandbox quota is exhausted" | The tenant holds `--max-concurrent` live sandboxes. | Kill sandboxes you no longer need, or `sbxd admin tenant update --id <tenant> --max-concurrent <n>`. Not retried by default; it clears only when a sandbox ends. |
| `400 network_not_allowed`: "the template does not allow the requested network mode" | The sandbox asked for a wider mode than the template's `--network`. | Re-apply the template with `--network allowlist` or `full`, or ask for less. |
| `409 network_not_attached` on `update_network` | The sandbox was created with mode `none` and has no proxy. | Create it with `allowlist` (an empty list denies everything), then widen. |
| `Tunnel connection failed: 403 Forbidden` from `pip`, `curl` or `urllib`; over plain HTTP the body is `egress proxy: denied: host_not_allowed: <host> is not in the allowlist` | The hostname is not in `allow_domains`. An `allowlist` template with no list allows nothing. | Add the host. PyPI needs `pypi.org` and `files.pythonhosted.org`. `*.example.com` matches subdomains, not the apex. |
| `pip` reports "Could not find a version that satisfies the requirement" in a `none` sandbox | No proxy and no network at all. | Use an `allowlist` template, or bake the package into the image. |
| `507 no_space` (`NoSpaceError`) on `files.write`; `No space left on device` from a command | `/workspace` or `/tmp` is full. They are tmpfs with a fixed size. | Raise `--workdir-size` or `--tmp-size` (and `--memory`, which they count against), or delete files. |
| `413 payload_too_large` on `files.write` | The upload exceeds the server's `--max-upload-size` (default 256 MiB). | Raise the flag or split the file. |
| `CommandExitError` with exit code 127 and `/bin/sh: 1: git: not found` (or `bash: not found`) | The image lacks the program. Alpine images have no bash. | Add it to the image. For bash syntax call `bash -c '...'`. |
| `400 command_not_found` (`CommandStartError` in Python, `ValidationError` in TypeScript) | The executable given with `args=[...]`, or the TypeScript `shell` option, does not exist in the image. | The same. |
| `Read-only file system`, or `apt-get` failing with `are you root?` | The root filesystem is read-only and the process is uid 1000. | Write under `/workspace` or `/tmp`. Install system packages in the image. |
| `409 sandbox_not_running` (`SandboxNotRunningError`) with reason `idle_timeout` | Nothing touched the sandbox's commands or files for the template's idle timeout. | Raise `--idle-timeout` or set it to `0`. `set_timeout` does not count as activity. |
| `400 invalid_timeout` at create | `timeout` exceeds the template's `--max-lifetime`. `details.max_timeout_ms` has the limit. | Lower it or raise the template's limit. |
| A command ends with reason `timeout` after 60 s (`CommandTimeoutError`) | The per-command default, the same as E2B's. | Pass `timeout=0` (Python) or `timeoutMs: 0` for long-running commands. |
| A sandbox ends with reason `oom_killed` | It exceeded `--memory`. Files in `/workspace` and `/tmp` count. | Raise `--memory`, or write less to the tmpfs. |

## Not verified

- No E2B code was executed for this guide. The "before" snippets follow E2B's
  SDK source and documentation as read on 2026-10-10 and were not run against
  E2B.
- E2B Embed's requirements are quoted from its README. It was not installed.
- That warm sandboxes are recycled after a template is re-applied comes from
  the README's lifecycle description. It was not tested here.
- The claim, refill and memory figures are the project's own measurements on
  one laptop, from [`bench/RESULTS.md`](../bench/RESULTS.md) and the README.

## Sources

All read on 2026-10-10.

E2B SDK source, `e2b-dev/E2B` at `main` (commit `df6c368`; Python package
`e2b` 2.55.1, JavaScript package `e2b` 2.55.0):

- https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/sandbox_sync/main.py
- https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/sandbox/main.py
- https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/sandbox/sandbox_api.py
- https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/sandbox_sync/commands/command.py
- https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/sandbox_sync/commands/command_handle.py
- https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/sandbox/commands/command_handle.py
- https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/sandbox_sync/commands/pty.py
- https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/sandbox_sync/filesystem/filesystem.py
- https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/connection_config.py
- https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/template/main.py
- https://github.com/e2b-dev/E2B/blob/main/packages/js-sdk/src/sandbox/index.ts
- https://github.com/e2b-dev/E2B/blob/main/packages/js-sdk/src/sandbox/sandboxApi.ts
- https://github.com/e2b-dev/E2B/blob/main/packages/js-sdk/src/sandbox/commands/index.ts
- https://github.com/e2b-dev/E2B/blob/main/packages/js-sdk/src/sandbox/commands/commandHandle.ts
- https://github.com/e2b-dev/E2B/blob/main/packages/js-sdk/src/sandbox/commands/pty.ts
- https://github.com/e2b-dev/E2B/blob/main/packages/js-sdk/src/sandbox/filesystem/index.ts

E2B documentation:

- https://docs.e2b.dev/llms.txt (index)
- https://docs.e2b.dev/sandbox.md (lifecycle, timeouts)
- https://docs.e2b.dev/sandbox/persistence.md (index entry only)
- https://docs.e2b.dev/sandbox/metadata.md
- https://docs.e2b.dev/network/internet-access.md
- https://docs.e2b.dev/network/public-url.md
- https://docs.e2b.dev/commands/background.md
- https://docs.e2b.dev/template/quickstart.md
- https://docs.e2b.dev/template/base-image.md
- https://docs.e2b.dev/template/defining-template.md
- https://docs.e2b.dev/template/user-and-workdir.md
- https://docs.e2b.dev/sdk-reference/cli/v2.21.1/template.md
- https://docs.e2b.dev/code-interpreting/analyze-data-with-ai.md
- https://docs.e2b.dev/code-interpreting/contexts.md

E2B self-hosting. `github.com/e2b-dev/infra` now redirects to
`github.com/e2b-dev/runtime`:

- https://github.com/e2b-dev/runtime/blob/main/README.md
- https://github.com/e2b-dev/runtime/blob/main/embed/README.md
- https://github.com/e2b-dev/runtime/blob/main/embed/compose/README.md
- https://github.com/e2b-dev/runtime/blob/main/embed/compose/compose.yaml

Not reached: the source of the code-interpreter package
(`github.com/e2b-dev/code-interpreter`). The repository exists and is
Apache-2.0, but the file path tried returned nothing. What this guide says
about `run_code` and code contexts rests on the two documentation pages above.

Warmbox side: [`README.md`](../README.md), [`operations.md`](operations.md),
[`api/openapi.yaml`](../api/openapi.yaml), `sdk/python/src/sbx`,
`sdk/typescript/src`, `cmd/sbxd/admin.go`, `internal/mcp/tools.go`,
`internal/runtime/podman/spec.go`, `internal/agent/egress.go`, and the help
output of `bin/sbxd` and `bin/sbx-mcp` built from this tree.
