# Migrating from open-source Daytona to Warmbox

Checked on 2026-10-10 against the last open-source Daytona release, tag
`v0.190.0` of `daytonaio/daytona` (SDK source, documentation and Docker Compose
files in that tag), and against the Warmbox code in this repository. The
Warmbox snippets below were run against a live server unless marked otherwise.
[Sources](#sources) lists what was read.

## Who this is for

You self-host the open-source Daytona, and you need to decide what to do now
that it is frozen. The repository `daytonaio/daytona` is archived. Its README
on `main` says:

> This repository is no longer maintained. As of June 2026, Daytona's core
> development has moved to a private codebase. This repository will receive no
> further updates, fixes, or releases.

The last tag is `v0.190.0`, released on 2026-06-23. The platform is licensed
AGPL-3.0 (`LICENSE` at that tag); the SDKs under `libs/sdk-python` and
`libs/sdk-typescript` carry their own Apache-2.0 licence files.

This guide is not for users of Daytona's hosted service who are happy there.

Read the fit check first. Daytona and Warmbox have different ideas of what a
sandbox is, and a large part of Daytona's surface has no equivalent.

### What you get

- **One Linux box, one unprivileged user.** Rootless Podman, Postgres and three
  Go binaries. No root daemon, no privileged container, no Kubernetes, no
  `/dev/kvm`.
- **Warm claims.** Sandboxes are created ahead of time. A claim is a database
  update: p50 6-9 ms in the project's own measurements on a laptop
  ([`bench/RESULTS.md`](../bench/RESULTS.md)).
- **No network unless you ask.** A sandbox has no network interface. Egress goes
  through a proxy that allowlists hostnames.
- **No metering.** No per-second billing, no tiers. You pay for the machine.
- **Apache-2.0** for the server and the SDKs, in a repository that is
  maintained.

### What you lose today

| You use this in Daytona | In Warmbox |
|---|---|
| Sandboxes that persist: `stop`, `start`, `archive`, auto-stop that keeps the filesystem | **Not available.** Every sandbox is what Daytona calls ephemeral: used once, then destroyed with its files. |
| Volumes (`volumes=[VolumeMount...]`) | Not available. No persistent storage of any kind. |
| Preview links (`get_preview_link`, signed preview URLs), the proxy on `{port}-{sandboxId}.{domain}` | Not available. Nothing outside the sandbox can reach a port inside it. |
| Stateful code interpreter (`sandbox.code_interpreter.run_code`, contexts) | Not available. Every command starts a fresh process. |
| Sessions (`create_session`, `execute_session_command`): a shell whose directory and variables persist between commands | Not available. Each command is independent. Pass `cwd` and `envs` each time, or put the steps in one command line. |
| `sandbox.git`, LSP (`create_lsp_server`), computer use, SSH access, web terminal, VNC | Not available. |
| File tools beyond read and write: `find_files`, `search_files`, `replace_in_files`, `move_files`, `set_file_permissions` | Not available as API calls. Run `grep`, `sed`, `mv`, `chmod` as commands. |
| Creating a sandbox from an image at call time (`CreateSandboxFromImageParams`, `Image.base(...)`), the declarative builder, snapshots built by the platform from a Dockerfile, the internal registry | You build an OCI image yourself, pull it on the host and register a template by hand. |
| Per-sandbox resources (`Resources(cpu=, memory=, disk=)`), `resize` | Fixed per template. |
| `os_user`, running as root in the sandbox, `sudo`, `apt install` at runtime | Not available. Every process is uid 1000 with no capabilities on a read-only root filesystem. |
| Dashboard, organisations, OIDC login, audit logs | None. Administration is the `sbxd admin` command line; tenants and API keys are the only identities. |
| Webhooks (`sandbox.created`, `sandbox.state.updated`, ...) | Lifecycle webhooks: `sandbox.created`, `sandbox.running`, `sandbox.terminated`, `sandbox.destroyed`, `command.started`, `command.finished`, signed (Standard Webhooks) and retried; see [`webhooks.md`](webhooks.md). There is no single `sandbox.state.updated`: each transition has its own type. The per-sandbox event log you can poll is still there. |
| More than one runner, regions, GPU sandboxes | Single node only. No GPUs. |
| `pip install daytona`, `npm install @daytona/sdk` | The Warmbox SDKs are not on PyPI or npm yet. Install them from a checkout. |
| Go, Ruby and Java SDKs | Python and TypeScript only. The REST API is documented in [`api/openapi.yaml`](../api/openapi.yaml). |

On isolation, be clear about what you are moving between. Daytona's
architecture page at that tag describes each sandbox as "an isolated instance
with its own Linux namespaces", run by a runner; the Compose setup starts that
runner as a privileged container. Warmbox's default and only tested tier is
also a container: rootless, hardened, sharing the host kernel. Neither is a
microVM. Read the Warmbox [threat model](../README.md#threat-model) before
running code you do not trust.

Warmbox is an early project. It works end to end and is tested against real
Podman, but it has run on one development machine.

**If you depend on any row above, do not migrate yet.** In particular, if your
product is "a development environment the user comes back to", Warmbox is the
wrong shape. It fits "run this code now, return the result, throw the machine
away".

## Concept map

| Concept | Daytona (`v0.190.0`) | Warmbox |
|---|---|---|
| Client object | `Daytona()` / `new Daytona()`, `DaytonaConfig(api_key, api_url, target)` | None. Call class methods on `Sandbox`. |
| API key | `DAYTONA_API_KEY` | `SBX_API_KEY`. A tenant key, created with `sbxd admin key create`. |
| Server address | `DAYTONA_API_URL` | `SBX_BASE_URL`. Default `http://127.0.0.1:8080`. |
| Region / runner | `DAYTONA_TARGET`, `target` | No equivalent. One node. |
| Per-sandbox credential | None in the SDK | `sandbox.access_token` (`sbt_...`). Opens only that sandbox's commands and files. |
| Snapshot | A named image plus resources, built or pulled by the platform (`daytona snapshot create`, `daytona.snapshot.create`) | A **template**: image, resource limits, widest network mode, timers, warm pool size. Made with `sbxd admin template apply`. |
| Sandbox | A container that can be stopped, started and archived | A rootless container, used once and destroyed. |
| Create | `daytona.create(CreateSandboxFromSnapshotParams(...))` | `Sandbox.create("<template>", ...)`. The template is required. |
| `timeout` on create | How long to wait for the sandbox to start | The sandbox's **maximum lifetime**. Not the same thing. See below. |
| Lifecycle timers | `auto_stop_interval` (minutes, default 15), `auto_archive_interval`, `auto_delete_interval` | Idle timeout and maximum lifetime, both ending in destruction. See below. |
| Labels | `labels=`, `set_labels` | `metadata=` at create only; `Sandbox.list(metadata=...)` filters on it. |
| Environment | `env_vars=` on create, `env=` on `exec` | `envs=` on create and on each command. |
| Command | `sandbox.process.exec(command, cwd, env, timeout)` returns `exit_code` and `result` | `sandbox.commands.run(cmd, cwd=, envs=, timeout=)` returns `exit_code`, `stdout`, `stderr`. It **raises** on a non-zero exit unless told not to. |
| Run code, stateless | `sandbox.process.code_run(code)` | Write the code to a file and run `python3` on it. See the tool example below. |
| Run code, stateful | `sandbox.code_interpreter.run_code` | No equivalent. |
| Background command | `execute_session_command(session, SessionExecuteRequest(command=..., run_async=True))` | `commands.run(cmd, background=True)` returns a handle. |
| Streaming output | `get_session_command_logs_async(session, cmd_id, on_stdout, on_stderr)` | `on_stdout=` / `on_stderr=` on `run`, or iterate the handle. |
| Command input | `send_session_command_input` | `handle.send_stdin(...)` on a command started with `stdin=True`. |
| PTY | `process.create_pty_session(id, cwd, envs, pty_size)` | `sandbox.pty.create(cols, rows, on_data)` |
| Files | `sandbox.fs.upload_file`, `download_file`, `list_files`, `get_file_info`, `create_folder`, `delete_file` | `sandbox.files.write`, `read`, `list`, `get_info`, `make_dir`, `remove`, `exists` |
| Network policy | `network_block_all`, `network_allow_list` (IPv4 CIDRs), `domain_allow_list`; `update_network_settings` | `network=NetworkPolicy(mode=, allow_domains=, allow_cidrs=, deny_cidrs=)`; `update_network`. Off by default. See below. |
| Preview of a port | `get_preview_link(port)` | No equivalent. |
| Events and webhooks | Webhooks: `sandbox.created`, `sandbox.state.updated`, `snapshot.*`, `volume.*` | `sandbox.events()` returns the sandbox's event log (`claimed`, `transition`, `command_started`, `command_finished`). Lifecycle webhooks for sandbox and command events ([`webhooks.md`](webhooks.md)); nothing for snapshots or volumes, which Warmbox does not have. |
| Delete | `sandbox.delete()` / `daytona.delete(sandbox)` | `sandbox.kill()` |
| Self-hosting components | The services in `docker/docker-compose.yaml`: API, proxy, runner, SSH gateway, Postgres, Redis, Dex, a registry and its UI, MinIO, MailDev, Jaeger, an OpenTelemetry collector, PgAdmin | `sbxd`, `sbx-agent`, Postgres, rootless Podman. |

### Differences that will bite

**Everything is ephemeral.** There is no stopped state. When a Warmbox sandbox
ends, for any reason, the container and its files are gone. Download what you
need before that.

**`timeout` means something else.** In Daytona, `daytona.create(params,
timeout=60)` waits up to 60 seconds for the sandbox to start. In Warmbox,
`Sandbox.create("t", timeout=60)` creates a sandbox that is destroyed after 60
seconds. Do not carry the argument over.

**Three timers.**

| Timer | Set by | Ends | Reason reported |
|---|---|---|---|
| Idle timeout | Template, `--idle-timeout` (default 10 m, `0` disables) | The sandbox, after that long without activity | `idle_timeout` |
| Maximum lifetime | `timeout` at create and `set_timeout`, at most the template's `--max-lifetime` (default 1 h; more is refused) | The sandbox | `max_lifetime` |
| Warm TTL | Template, `--warm-ttl` (default 30 m) | A warm sandbox nobody claimed. You never see it. | none |

- The idle timeout is the closest thing to `auto_stop_interval`, with two
  differences: it destroys instead of stopping, and a client cannot change it.
  There is no `set_autostop_interval`. It belongs to the template.
- "Activity" means commands, file calls, delivered output and `set_timeout`.
  `get_info`, `is_running` and `list` do not count. Daytona has the same
  idea: only toolbox requests, previews and SSH reset its timer.
- Daytona's create parameters have no lifetime cap; with auto-stop set to `0`
  its documentation says the sandbox runs indefinitely. Warmbox templates
  default to a one-hour maximum. Set `--max-lifetime 0` to remove the cap.
- A lifetime past the template's maximum is refused with `400 invalid_timeout`,
  at create and by `set_timeout` alike; nothing is capped silently.
  `details.max_timeout_ms` is the most you could have asked for.

**A claim cannot change the template.** There is no `Resources(...)` and no
image at create time, because the sandbox already exists when you claim it.
Make one template per shape you need.

**Network is off by default and works through a proxy.** A sandbox has only a
loopback interface. When the mode is `allowlist` or `full`, the agent sets
`HTTP_PROXY`, `HTTPS_PROXY` and `ALL_PROXY` to `http://127.0.0.1:3128`. So:

- Programs that honour those variables work: `pip`, `curl`, `requests`, `npm`,
  `git` over HTTPS.
- Anything else does not: raw sockets, DNS lookups, UDP, `git` over SSH,
  database drivers.
- The allowlist is by hostname (`allow_domains`), exact or `*.example.com`.
  `allow_cidrs` only permits requests to IP-literal targets. A Daytona
  `network_allow_list` of CIDRs does not translate one to one; list the
  hostnames your code calls instead.
- There is no built-in set of "essential services". Package registries are
  blocked like everything else until you list them.
- A sandbox created without `network=` gets the template's mode and, for an
  `allowlist` template, the template's default list
  (`sbxd admin template apply --allow-domains ...`). A template without one
  allows nothing. A `network=` argument replaces the default list, it does
  not add to it.
- A sandbox created with mode `none` has no proxy and cannot be given one later
  (`409 network_not_attached`).
- A sandbox can ask for the template's mode or a stricter one, never a wider
  one (`400 network_not_allowed`).

**A non-zero exit raises.** `process.exec` returns the exit code and leaves the
check to you. `commands.run` raises `CommandExitError` unless you pass
`check=False` (Python) or `throwOnNonZeroExit: false` (TypeScript). Code ported
line by line will start throwing on failing commands.

**Commands have a 60-second default limit.** `process.exec` takes an optional
`timeout` with no default in the SDK. `commands.run` kills the command after 60
seconds unless you pass `timeout=0` (Python) or `timeoutMs: 0`.

**The shell is `/bin/sh -c`.** A command line is run by `/bin/sh -c`, and
Alpine images have no bash. Scripts that use bash syntax need `bash -c '...'`
and an image that contains bash. With `args=[...]` no shell is involved; use
that form for anything built from model output.

**The root filesystem is read-only.** Writable space is two size-capped tmpfs
mounts, `/workspace` and `/tmp`. They are charged to the sandbox's memory
limit, and they start empty. There is no disk quota in gigabytes; "disk" is
memory.

- `HOME` is `/workspace`, and commands start there. Relative file paths in the
  SDK resolve against `/workspace`.
- `pip install` works only if the network policy allows your package index
  (`pypi.org` and `files.pythonhosted.org` for PyPI) and the tmpfs is large
  enough. It installs into the user site, `/workspace/.local`, and the scripts
  it writes to `/workspace/.local/bin` are on `PATH`. A venv under `/workspace`
  also works. The operator can put the index on the template
  (`--allow-domains pypi.org,files.pythonhosted.org`), so that a sandbox
  created without `network=` can install.
- Files baked into the image are read-only. Copy what must change into
  `/workspace` at the start of a session.

**The process is uid 1000, not root, and cannot become root.** There is no
`sudo`. `apt-get install` fails with a permission error. Bake every system
dependency into the image. The image's `USER`, `ENTRYPOINT` and `CMD` are not
used: the container runs the Warmbox agent, and your commands are started by
it.

**Output is split.** `ExecuteResponse.result` becomes `result.stdout` and
`result.stderr`, separately.

## Step-by-step migration

### 1. Stand up Warmbox

Follow the [quick start](../README.md#quick-start) and the
[operating notes](operations.md). Run `bin/sbxd preflight` and fix every `FAIL`
line before going further.

### 2. Turn your snapshot into an image

A Warmbox template points at an OCI image that is already in the local store of
the user who runs `sbxd`. Nothing is built or pulled for you.

| Your Daytona snapshot was made from | Do this |
|---|---|
| A public image (`daytona snapshot create x --image python:3.11-slim`) | `podman pull docker.io/library/python:3.11-slim` |
| A Dockerfile (`daytona snapshot create x --dockerfile ./Dockerfile`) | `podman build -t localhost/x:1 .` with the same Dockerfile |
| The declarative builder (`Image.base(...)` with added steps) | Rewrite the steps as a Dockerfile, then build it |
| A local image (`daytona snapshot push`) | Build or load the same image into Podman |
| A snapshot `entrypoint` | No equivalent. Start the process after the claim with a background command. |

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

`python:3.12-slim` contains bash but not git. `python:3.12-alpine` is smaller
and contains neither. Daytona's default snapshots are based on
`daytonaio/sandbox`; that image was not tried under Warmbox, and its toolbox
daemon would not be used.

### 3. Register the template

```sh
export SBX_DATABASE_URL='postgres://postgres:sbx@127.0.0.1:55432/postgres?sslmode=disable'

bin/sbxd admin template apply pydata \
    --image localhost/pydata:1 \
    --cpus 1 --memory 1g --pids 256 \
    --workdir-size 256m --tmp-size 64m \
    --network allowlist \
    --idle-timeout 15m --max-lifetime 1h \
    --warm-target 2 --max-total 6
bin/sbxd admin template list
```

| Flag | Meaning | Default | Daytona counterpart |
|---|---|---|---|
| `--id` | Template id; what you pass to `Sandbox.create` | required | Snapshot name |
| `--image` | Image, already present for the Podman user | required | Snapshot image |
| `--cpus`, `--memory` | CPU and memory caps | `1`, `512m` | `Resources(cpu, memory)` |
| `--workdir-size`, `--tmp-size` | Size of the `/workspace` and `/tmp` tmpfs | `128m`, `64m` | Roughly `Resources(disk)`, but counted as memory |
| `--pids`, `--nofile` | Process and open-file limits | `256`, `4096` | |
| `--network` | Widest mode a sandbox may ask for: `none`, `allowlist`, `full` | `none` | |
| `--idle-timeout` | Destroy after this long without activity; `0` disables | `10m` | `auto_stop_interval` (which stops) |
| `--max-lifetime` | Hard cap on a sandbox's life; `0` disables | `1h` | |
| `--warm-ttl` | Recycle an unclaimed warm sandbox after this long | `30m` | |
| `--warm-target` | Sandboxes kept ready | `2` | |
| `--max-total` | Cap on all sandboxes of this template; `0` for none | `0` | |
| `--refill-rate` | Concurrent creates | `2` | |
| `--runtime` | `crun` or `runsc`. Only `crun` is enabled and tested. | `crun` | |
| `--file` | A JSON file with one template or an array, instead of the flags | | |

The two tmpfs sizes must fit inside `--memory` together, because tmpfs pages
count as memory. Leave room for the processes: with `--memory 1g` and
`--workdir-size 256m`, a full `/workspace` leaves about 700 MB for Python.

`sbxd admin template show pydata` prints the stored template in the
shape `--file` reads. Applying an id again creates a new version.

### 4. Create a tenant and an API key

```sh
bin/sbxd admin tenant create acme --max-concurrent 20 --rate 50 --burst 100
bin/sbxd admin key create acme               # prints the key once
```

A tenant is the unit of quota and of isolation between API users: one tenant
cannot see another's sandboxes. It stands in for a Daytona organisation, with
nothing else attached. `--max-concurrent` is its quota of live sandboxes.

### 5. Install the SDK

```sh
pip install ./sdk/python                                   # Python 3.10+, import name: sbx
(cd sdk/typescript && npm install && npm run build)        # package name: sbx-sdk
```

Neither is published yet. For TypeScript, make your project depend on the
built checkout: `npm install /path/to/warmbox/sdk/typescript`, or the tarball
that `npm pack` makes there.

```sh
export SBX_API_KEY=sbx_...
export SBX_BASE_URL=http://127.0.0.1:8080
```

### 6. Change the code: Python

Before, with Daytona:

```python
from daytona import (
    CreateSandboxFromSnapshotParams,
    Daytona,
    PtySize,
    SessionExecuteRequest,
)

daytona = Daytona()  # DAYTONA_API_KEY, DAYTONA_API_URL
sandbox = daytona.create(
    CreateSandboxFromSnapshotParams(
        snapshot="my-snapshot",
        env_vars={"MY_VAR": "1"},
        labels={"user_id": "u_123"},
        auto_stop_interval=15,
        network_allow_list="151.101.0.0/16",
    )
)

response = sandbox.process.exec("python3 -c 'import pandas; print(pandas.__version__)'", timeout=30)
print(response.result, response.exit_code)

# Background command with streamed output.
sandbox.process.create_session("s1")
command = sandbox.process.execute_session_command(
    "s1", SessionExecuteRequest(command="python3 -u -m http.server 8000", run_async=True)
)
logs = sandbox.process.get_session_command_logs("s1", command.cmd_id)
print(logs.stdout)
# Streaming: await sandbox.process.get_session_command_logs_async("s1", command.cmd_id, on_stdout, on_stderr)

sandbox.fs.upload_file(b"a,b\n1,2\n", "data/in.csv")
content = sandbox.fs.download_file("data/in.csv")

sandbox.set_autostop_interval(30)

pty = sandbox.process.create_pty_session(id="term", pty_size=PtySize(rows=24, cols=80))
pty.send_input("echo hello\nexit\n")
pty.wait(on_data=lambda data: print(data.decode(), end=""))

sandbox.delete()
```

After, with Warmbox. This script was run against a live server:

```python
import sys

import sbx
from sbx import NetworkPolicy, Sandbox

# Create. The template is required; network is "none" unless you ask.
sandbox = Sandbox.create(
    "pydata",
    timeout=300,  # max lifetime in seconds; NOT a start-up timeout
    envs={"MY_VAR": "1"},
    metadata={"user_id": "u_123"},
    network=NetworkPolicy(mode="allowlist", allow_domains=["pypi.org", "files.pythonhosted.org"]),
)
print(sandbox.sandbox_id)

# Run a command and read its output.
result = sandbox.commands.run("python3 -c 'import pandas; print(pandas.__version__)'", timeout=30)
print(result.stdout, result.exit_code)

# A non-zero exit raises. process.exec did not.
try:
    sandbox.commands.run("ls /nonexistent")
except sbx.CommandExitError as e:
    print(e.exit_code, e.stderr)

# To keep Daytona's behaviour, ask for the result whatever the status.
result = sandbox.commands.run("ls /nonexistent", check=False)

# Streaming callbacks.
sandbox.commands.run(
    "for i in 1 2 3; do echo line $i; sleep 0.2; done",
    on_stdout=lambda text: print("out:", text, end=""),
    on_stderr=lambda text: print("err:", text, end=""),
)

# Background command. timeout=0 lifts the 60 s per-command limit.
server = sandbox.commands.run("python3 -u -m http.server 8000", background=True, timeout=0)
print(server.pid)  # the command id, an opaque string
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

# Lifetime: the sandbox now ends 600 s from this call (more than the template's max lifetime is refused).
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

# Delete.
sandbox.kill()
```

What changed:

| Daytona (Python) | Warmbox (Python) |
|---|---|
| `from daytona import Daytona`; `daytona = Daytona()` | `from sbx import Sandbox`; no client object |
| `daytona.create(CreateSandboxFromSnapshotParams(snapshot="x"))` | `Sandbox.create("x")` |
| `daytona.create(CreateSandboxFromImageParams(image=..., resources=...))` | No equivalent; register a template first |
| `env_vars=`, `labels=` | `envs=`, `metadata=` |
| `network_block_all=True` | Leave `network` out on a `none` template, or pass `network="none"` |
| `network_allow_list="1.2.3.0/24"`, `domain_allow_list="a.com,b.com"` | `network=NetworkPolicy(mode="allowlist", allow_domains=["a.com", "b.com"])` |
| `sandbox.update_network_settings(network_block_all=True)` | `sandbox.update_network("none")` |
| `sandbox.process.exec(cmd, cwd, env, timeout)` | `sandbox.commands.run(cmd, cwd=, envs=, timeout=)` |
| `response.result`, `response.exit_code` | `result.stdout`, `result.stderr`, `result.exit_code`, `result.reason` |
| `process.code_run(code)` | `files.write` the code, then `commands.run("python3", args=[path])` |
| `create_session` + `execute_session_command(..., run_async=True)` | `commands.run(cmd, background=True)` |
| `command.cmd_id` | `handle.pid` |
| `get_session_command_logs(session, cmd_id)` | `handle.stdout`, `handle.stderr`, or `commands.connect(pid)` to replay |
| `get_session_command_logs_async(..., on_stdout, on_stderr)` | `on_stdout=` / `on_stderr=` on `run`, or iterate the handle |
| `send_session_command_input(session, cmd_id, data)` | `handle.send_stdin(data)`; start the command with `stdin=True` |
| `delete_session` | `handle.kill()` or `commands.kill(pid)` |
| `fs.upload_file(bytes_or_local_path, remote_path)` | `files.write(remote_path, data)`. Note the argument order. Pass an open file object to stream a local file. |
| `fs.download_file(remote_path)` returns `bytes` | `files.read(path, format="bytes")`; the default returns `str` |
| `fs.list_files`, `get_file_info`, `create_folder(path, mode)`, `delete_file(path, recursive)` | `files.list`, `get_info`, `make_dir(path)`, `remove(path)` (recursive) |
| `create_pty_session(id=, pty_size=PtySize(rows, cols))`, `handle.wait(on_data=...)` | `pty.create(cols, rows, on_data)`; the id is assigned, read it from `handle.pid` |
| `pty.resize(PtySize(...))` | `pty.resize(cols, rows)` |
| `set_autostop_interval(minutes)` | No call. The idle timeout is the template's. `set_timeout(seconds)` moves the lifetime cap. |
| `sandbox.stop()`, `start()`, `archive()` | No equivalent |
| `sandbox.delete()` | `sandbox.kill()` |
| `AsyncDaytona` | `AsyncSandbox`, same surface as the blocking client |

`with Sandbox.create("pydata") as sandbox:` kills the sandbox when the block
ends.

### 7. Change the code: TypeScript

Before, with Daytona:

```ts
import { Daytona } from '@daytona/sdk'

const daytona = new Daytona() // DAYTONA_API_KEY, DAYTONA_API_URL
const sandbox = await daytona.create({
  snapshot: 'my-snapshot',
  envVars: { MY_VAR: '1' },
  labels: { userId: 'u_123' },
  autoStopInterval: 15,
  networkAllowList: '151.101.0.0/16',
})

const response = await sandbox.process.executeCommand(
  "python3 -c 'import pandas; print(pandas.__version__)'",
  undefined,
  undefined,
  30,
)
console.log(response.result, response.exitCode)

// Background command with streamed output.
await sandbox.process.createSession('s1')
const command = await sandbox.process.executeSessionCommand('s1', {
  command: 'python3 -u -m http.server 8000',
  runAsync: true,
})
await sandbox.process.getSessionCommandLogs(
  's1',
  command.cmdId!,
  (chunk) => process.stdout.write(chunk),
  (chunk) => process.stderr.write(chunk),
)

await sandbox.fs.uploadFile(Buffer.from('a,b\n1,2\n'), 'data/in.csv')
const content = await sandbox.fs.downloadFile('data/in.csv')

await sandbox.setAutostopInterval(30)

const pty = await sandbox.process.createPty({
  id: 'term',
  cols: 80,
  rows: 24,
  onData: (data) => process.stdout.write(data),
})
await pty.waitForConnection()
await pty.sendInput('echo hello\nexit\n')
await pty.wait()

await sandbox.delete()
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

// A non-zero exit throws. executeCommand did not.
try {
  await sandbox.commands.run('ls /nonexistent')
} catch (e) {
  if (e instanceof CommandExitError) console.log(e.exitCode, e.stderr)
  else throw e
}

// To keep Daytona's behaviour, ask for the result whatever the status.
const r2 = await sandbox.commands.run('ls /nonexistent', { throwOnNonZeroExit: false })
console.log(r2.exitCode)

// Streaming callbacks.
await sandbox.commands.run('for i in 1 2 3; do echo line $i; sleep 0.2; done', {
  onStdout: (text) => process.stdout.write(`out: ${text}`),
  onStderr: (text) => process.stderr.write(`err: ${text}`),
})

// Background command. timeoutMs: 0 lifts the 60 s per-command limit.
const server = await sandbox.commands.run('python3 -u -m http.server 8000', { background: true, timeoutMs: 0 })
console.log(server.pid) // the command id, an opaque string
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

// Lifetime: the sandbox now ends 600 s from this call (more than the template's max lifetime is refused).
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

// Delete.
await sandbox.kill()
```

What changed:

| Daytona (TypeScript) | Warmbox (TypeScript) |
|---|---|
| `import { Daytona } from '@daytona/sdk'`; `new Daytona()` | `import { Sandbox } from 'sbx-sdk'`; no client object |
| `daytona.create({ snapshot: 'x', envVars, labels })` | `Sandbox.create('x', { envs, metadata })` |
| `networkBlockAll`, `networkAllowList`, `domainAllowList` | `network: { mode, allowDomains, allowCidrs, denyCidrs }` |
| `process.executeCommand(cmd, cwd, env, timeoutSeconds)` | `commands.run(cmd, { cwd, envs, timeoutMs })`. Milliseconds, not seconds. |
| `response.result`, `response.exitCode` | `result.stdout`, `result.stderr`, `result.exitCode`, `result.reason` |
| `process.codeRun(code)` | `commands.run('python3', { args: ['-'], stdin: code })` |
| `createSession` + `executeSessionCommand(id, { command, runAsync: true })` | `commands.run(cmd, { background: true })` |
| `getSessionCommandLogs(id, cmdId, onStdout, onStderr)` | `onStdout` / `onStderr` options, or `for await (const event of handle)` |
| `sendSessionCommandInput(id, cmdId, data)` | `handle.sendStdin(data)`; start with `stdin: true` |
| `fs.uploadFile(bufferOrLocalPath, remotePath)` | `files.write(remotePath, data)`. Note the argument order. A string is content, not a local path. |
| `fs.downloadFile(remotePath)` returns a `Buffer` | `files.read(path, { format: 'bytes' })` returns a `Uint8Array`; the default returns a string |
| `fs.listFiles`, `getFileDetails`, `createFolder(path, mode)`, `deleteFile(path, recursive)` | `files.list`, `getInfo`, `makeDir(path)`, `remove(path)` |
| `process.createPty({ id, cols, rows, onData })`, `waitForConnection()` | `pty.create({ cols, rows, onData })`; resolved when attached |
| `ptyHandle.resize(cols, rows)` | `pty.resize({ cols, rows })` |
| `setAutostopInterval(minutes)` | No call. `setTimeout(ms)` moves the lifetime cap. |
| `sandbox.stop()`, `start()`, `archive()`, `getPreviewLink(port)` | No equivalent |
| `sandbox.delete()` | `sandbox.kill()` |

`await using sandbox = await Sandbox.create(...)` kills the sandbox at the end of
the scope. On Node 20 the PTY needs a `WebSocket` implementation passed in; see
[`sdk/typescript/README.md`](../sdk/typescript/README.md).

### 8. Replace sessions and the code interpreter

A Daytona session keeps a shell alive, so `cd` and `export` in one command
affect the next. Warmbox has no such thing. Port a session like this:

- Directory: pass `cwd=` on each command.
- Variables: pass `envs=` at create for the whole sandbox, or on each command.
- A sequence that must share shell state: one command line,
  `sandbox.commands.run("cd repo && . ./env.sh && make test")`.

`code_interpreter.run_code` keeps Python variables between calls. There is no
equivalent: send the whole program each time and keep state in files under
`/workspace`. Chart capture (`artifacts.charts`) is not provided either. Save
the figure to a file and read it back with `files.read(path, format="bytes")`.

## Using Warmbox from an agent framework

### Without code: the MCP server

Daytona's MCP server is started through its CLI. Warmbox ships its own binary,
`sbx-mcp`, a stdio server with these tools: `list_templates`, `create_sandbox`,
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
`run_python` starts a fresh `python3` on each call, like Daytona's stateless
`code_run`. Output above 32 KiB per stream is cut in the middle
(`--max-output-bytes`). There are no git, preview or file-search tools.

### With code: a custom tool

Where you handed a Daytona sandbox to a framework, wrap the SDK in a function
and register that. Python:

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
  destroys it, and the error carries the reason (`idle_timeout`, `oom_killed`,
  `max_lifetime`). You cannot start it again; create a new one.

## Operational differences

There is no hosted control plane, no dashboard and no registry. You run the
server, the database and the host.

**Sizing the warm pool.** Warmbox keeps `--warm-target` sandboxes of each
template ready and hands one out. A claim takes one and the refill loop makes a replacement in the background
(about 90 ms each in the project's measurements, `--refill-rate` at a time).
Set the target to the burst of creates you want answered without waiting. When
the pool is empty a create waits up to `--claim-wait` (default 5 s) and then
fails with `503 pool_empty`. `--max-total` caps warm and claimed sandboxes of a
template together, and each tenant has its own `--max-concurrent` quota. An
idle warm sandbox costs a few megabytes of memory.

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
`--metrics-token` or `--metrics-listen`. There is no bundled tracing stack.

**Kernel updates are your job.** The sandbox boundary is the host kernel. The
threat model asks for a disposable host that runs nothing else and is patched
within days.

## Troubleshooting

Each of these was reproduced on a live server. Codes are the ones in
[`api/openapi.yaml`](../api/openapi.yaml).

| What you see | Why | Fix |
|---|---|---|
| `503 pool_empty` (`PoolEmptyError`): "no warm sandbox is available for this template right now" | No warm sandbox appeared within `--claim-wait`. The template is at `--max-total`, or creates are slower than claims. | Raise `--warm-target` or `--max-total`. Check `sbx_pool_sandboxes` and `sbx_pool_create_failures_total` in `/metrics`. The SDKs retry this error by default. |
| `429 quota_exceeded` (`QuotaExceededError`): "the tenant's concurrent sandbox quota is exhausted" | The tenant holds `--max-concurrent` live sandboxes. | Kill sandboxes you no longer need, or `sbxd admin tenant update --id <tenant> --max-concurrent <n>`. Not retried by default; it clears only when a sandbox ends. |
| `400 network_not_allowed`: "the template does not allow the requested network mode" | The sandbox asked for a wider mode than the template's `--network`. | Re-apply the template with `--network allowlist` or `full`, or ask for less. |
| `409 network_not_attached` on `update_network` | The sandbox was created with mode `none` and has no proxy. | Create it with `allowlist` (an empty list denies everything), then widen. |
| `Tunnel connection failed: 403 Blocked by sandbox network policy: <host> is not in the allowlist` from `pip`, `curl` or `urllib`; over plain HTTP the body starts with the same sentence | The hostname is not in the sandbox's `allow_domains`: the template's default list, or the list the create request passed instead of it. An `allowlist` template with no default list allows nothing. | Add the host to the template (`--allow-domains`) or to the request. PyPI needs `pypi.org` and `files.pythonhosted.org`. `*.example.com` matches subdomains, not the apex. |
| `pip` reports "Could not find a version that satisfies the requirement" in a `none` sandbox | No proxy and no network at all. | Use an `allowlist` template, or bake the package into the image. |
| `507 no_space` (`NoSpaceError`) on `files.write`; `No space left on device` from a command | `/workspace` or `/tmp` is full. They are tmpfs with a fixed size. | Raise `--workdir-size` or `--tmp-size` (and `--memory`, which they count against), or delete files. |
| `413 payload_too_large` on `files.write` | The upload exceeds the server's `--max-upload-size` (default 256 MiB). | Raise the flag or split the file. |
| `CommandExitError` with exit code 127 and `/bin/sh: 1: git: not found` (or `bash: not found`) | The image lacks the program. Alpine images have no bash. | Add it to the image. For bash syntax call `bash -c '...'`. |
| `400 command_not_found` (`CommandStartError` in both SDKs) | The executable given with `args=[...]`, or the TypeScript `shell` option, does not exist in the image. | The same. |
| `Read-only file system`, or `apt-get` failing with `are you root?` | The root filesystem is read-only and the process is uid 1000. | Write under `/workspace` or `/tmp`. Install system packages in the image. |
| `409 sandbox_not_running` (`SandboxNotRunningError`) with reason `idle_timeout` | Nothing touched the sandbox's commands or files for the template's idle timeout. It was destroyed, not stopped. | Raise `--idle-timeout` or set it to `0`. Create a new sandbox. |
| `400 invalid_timeout` at create or from `set_timeout` | The sandbox would outlive the template's `--max-lifetime`, counted from its start. `details.max_timeout_ms` is the most the call could have asked for. | Lower it or raise the template's limit. |
| A command ends with reason `timeout` after 60 s (`CommandTimeoutError`) | The per-command default. | Pass `timeout=0` (Python) or `timeoutMs: 0` for long-running commands. |
| A sandbox ends with reason `oom_killed` | It exceeded `--memory`. Files in `/workspace` and `/tmp` count. | Raise `--memory`, or write less to the tmpfs. |

## Not verified

- No Daytona code was executed for this guide. The "before" snippets follow
  the SDK source and documentation at tag `v0.190.0` and were not run against
  a Daytona server.
- How the open-source runner isolates a sandbox is taken from the architecture
  page and the Compose file, not from running it. The repository README at
  that tag also describes sandboxes as having "a dedicated kernel"; this guide
  does not try to settle which statement holds for a self-hosted deployment.
- The documentation says `network_allow_list` accepts IPv4 CIDRs only, while
  the SDK also has a `domain_allow_list` parameter. Its behaviour was not
  checked.
- That warm sandboxes are recycled after a template is re-applied comes from
  the README's lifecycle description. It was not tested here.
- The claim, refill and memory figures are the project's own measurements on
  one laptop, from [`bench/RESULTS.md`](../bench/RESULTS.md) and the README.

## Sources

All read on 2026-10-10. Daytona files are at tag `v0.190.0`
(`https://github.com/daytonaio/daytona/tree/v0.190.0`), except the archive
notice, which is on `main`.

Repository state and licence:

- https://github.com/daytonaio/daytona (archived; last push 2026-07-24)
- https://github.com/daytonaio/daytona/blob/main/README.md (archive notice)
- https://github.com/daytonaio/daytona/releases/tag/v0.190.0 (published 2026-06-23)
- https://github.com/daytonaio/daytona/blob/v0.190.0/LICENSE (AGPL-3.0)
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/LICENSE (Apache-2.0)
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-typescript/LICENSE (Apache-2.0)

SDK source:

- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/__init__.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/common/daytona.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/common/process.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/common/pty.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/common/snapshot.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/_sync/daytona.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/_sync/sandbox.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/_sync/process.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/_sync/filesystem.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/_sync/code_interpreter.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/_sync/snapshot.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-python/src/daytona/handle/pty_handle.py
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-typescript/src/Daytona.ts
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-typescript/src/Sandbox.ts
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-typescript/src/Process.ts
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-typescript/src/FileSystem.ts
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-typescript/src/PtyHandle.ts
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-typescript/src/types/Pty.ts
- https://github.com/daytonaio/daytona/blob/v0.190.0/libs/sdk-typescript/package.json

Documentation, under `apps/docs/src/content/docs/en/` at the same tag:

- `architecture.mdx`, `sandboxes.mdx`, `snapshots.mdx`, `network-limits.mdx`,
  `process-code-execution.mdx`, `pty.mdx`, `webhooks.mdx`, `mcp.mdx`,
  `getting-started.mdx`

Self-hosting:

- https://github.com/daytonaio/daytona/blob/v0.190.0/docker/README.md
- https://github.com/daytonaio/daytona/blob/v0.190.0/docker/docker-compose.yaml

Not read: the hosted documentation site (`daytona.io/docs`), which now tracks
the private codebase and may differ from the archived tag; the git, LSP,
volume and preview pages beyond the method names in the SDK source.

Warmbox side: [`README.md`](../README.md), [`operations.md`](operations.md),
[`api/openapi.yaml`](../api/openapi.yaml), `sdk/python/src/sbx`,
`sdk/typescript/src`, `cmd/sbxd/admin.go`, `internal/mcp/tools.go`,
`internal/runtime/podman/spec.go`, `internal/agent/egress.go`, and the help
output of `bin/sbxd` and `bin/sbx-mcp` built from this tree.
