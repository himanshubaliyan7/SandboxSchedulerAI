# Warmbox

Warmbox is a self-hostable scheduler for running LLM-generated code. It keeps a pool of
pre-warmed, locked-down sandboxes on your own Linux box and hands one to an AI
agent in a few milliseconds, then destroys it when the session ends.

- **One box, nothing exotic.** Rootless Podman, Postgres and three Go binaries.
  No root daemon, no Kubernetes, no `/dev/kvm`.
- **Warm starts.** Sandboxes are created ahead of time, so a claim is a database
  pop: p50 6-9 ms on a laptop.
- **Limits enforced from outside.** CPU, memory, process and disk caps through
  cgroup v2; a fork bomb or memory bomb ends with a recorded reason.
- **No network by default.** A sandbox has no interfaces. Egress, when allowed,
  goes through a proxy that allowlists hostnames and blocks private and cloud
  metadata addresses.
- **Never reused.** A sandbox serves one session and is destroyed. Nothing is
  wiped and handed to the next tenant.
- **Streaming exec and terminals.** Commands stream output over HTTP and can be
  reconnected; interactive terminals run over WebSocket with flow control.
- **Clients included.** Python and TypeScript SDKs shaped like E2B's, and an MCP
  server so Claude Code, Claude Desktop or Cursor can use sandboxes as tools.

> **Status: early.** The single-node system works end to end and is tested
> against real Podman, but it has run on one development machine only. Read the
> [threat model](#threat-model) before pointing it at code you do not trust.

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Using it](#using-it): [Python](#python), [TypeScript](#typescript), [MCP](#mcp-server), [REST](#rest-api)
- [Threat model](#threat-model)
- [Measured performance](#measured-performance)
- [Roadmap](#roadmap)
- [Repository layout](#repository-layout)
- [Build and test](#build-and-test)

## How it works

### Components

```mermaid
flowchart LR
    subgraph clients ["Clients"]
        SDK["Python / TypeScript SDK"]
        MCP["sbx-mcp<br/>MCP server"]
        CURL["REST client"]
    end

    subgraph host ["One Linux host, one unprivileged user"]
        subgraph sbxd ["sbxd"]
            API["REST API<br/>auth, rate limits, streams"]
            POOL["Pool controller<br/>refill, timers, reconciler"]
            EGRESS["Egress proxy<br/>allowlist, deny set"]
        end
        PG[("Postgres<br/>state, events")]
        PODMAN["Rootless Podman<br/>REST socket"]

        subgraph sb ["Sandbox container: no network, read-only root"]
            AGENT["sbx-agent<br/>PID 1"]
            CODE["Untrusted code"]
        end
    end

    SDK --> API
    MCP --> API
    CURL --> API
    API --> POOL
    POOL --> PG
    POOL -->|"create / destroy"| PODMAN
    PODMAN -.-> sb
    API <-->|"exec, PTY, files<br/>Unix socket"| AGENT
    AGENT --> CODE
    CODE -->|"127.0.0.1:3128"| AGENT
    AGENT -->|"Unix socket"| EGRESS
    EGRESS -->|"validated public IPs only"| NET((Internet))
```

| Component | Role |
|---|---|
| `sbxd` | The server. Owns the REST API, the pool, the egress proxy and the reconciler. |
| `sbx-agent` | A small static binary that runs as PID 1 in every sandbox. It starts processes, owns terminals, buffers output for replay and serves files. All exec traffic goes through it, never through `podman exec`. |
| Postgres | The source of truth for tenants, templates, sandboxes and an append-only event log. |
| Rootless Podman | Creates and destroys containers. Used for lifecycle only. |
| Egress proxy | The only way out of a sandbox. One Unix socket per sandbox, so the socket itself identifies the caller. |

A **template** describes a pool: an image, a resource shape, a network mode and
how many sandboxes to keep warm. A claim cannot change the template; anything
that varies per session (environment, files) is injected after the claim.

### Sandbox lifecycle

The lifecycle only moves forward. There is no edge from a used sandbox back to
the warm pool, so one tenant's leftovers can never reach another.

```mermaid
stateDiagram-v2
    [*] --> CREATING: refill loop
    CREATING --> WARM: agent is ready
    CREATING --> FAILED: error or deadline
    WARM --> ASSIGNED: claim (atomic)
    WARM --> RECYCLING: warm TTL / template changed
    ASSIGNED --> RUNNING: session config injected
    ASSIGNED --> RECYCLING: inject failed
    RUNNING --> RECYCLING: released / idle / max lifetime / OOM
    FAILED --> RECYCLING: reconciler
    RECYCLING --> DESTROYED: container and storage removed
    DESTROYED --> [*]
```

Every move to `RECYCLING` records a reason (`released`, `idle_timeout`,
`max_lifetime`, `oom_killed`, `host_drain` and so on), so a sandbox never
disappears without one. Transient states carry a deadline that a sweeper
enforces, and a reconciler compares the database with what Podman actually runs,
destroying orphans and failing rows whose container vanished.

### What happens on a request

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant A as sbxd API
    participant P as Postgres
    participant G as sbx-agent (in sandbox)
    participant R as Refill loop

    C->>A: POST /v1/sandboxes {template}
    A->>P: UPDATE ... FOR UPDATE SKIP LOCKED
    P-->>A: one WARM sandbox, now ASSIGNED
    A->>G: init (env, egress on/off)
    A-->>C: sandbox id + access token (about 6-9 ms)
    A-)R: pool is one short
    R-)P: create a replacement in the background (about 90 ms)

    C->>A: POST /commands?wait=stream
    A->>G: start process
    G-->>A: stdout / stderr events
    A-->>C: NDJSON stream, ends with exit code and reason

    C->>A: DELETE /v1/sandboxes/{id}
    A-->>C: 204 once the sandbox is fenced
    A-)R: destroy container in the background
```

The claim is a single SQL statement, so fifty concurrent claims on a pool of
twenty get exactly twenty distinct sandboxes. Creation never happens on the
request path unless the pool is empty.

### How a sandbox is locked down

```mermaid
flowchart TB
    subgraph sandbox ["Sandbox"]
        direction TB
        P["Process, uid 1000<br/>no capabilities, no-new-privileges"]
        FS["Read-only root<br/>size-capped tmpfs: /workspace, /tmp"]
        NS["Network namespace with loopback only"]
    end
    P --> SEC["Seccomp profile<br/>blocks nested user namespaces, bpf,<br/>io_uring, mount, AF_NETLINK, AF_ALG ..."]
    P --> CG["cgroup v2<br/>cpu.max, memory.max, no swap,<br/>pids.max, oom.group"]
    NS --> BR["Agent bridge on 127.0.0.1:3128"]
    BR --> PX{"Egress proxy on the host"}
    PX -->|"hostname not allowed"| X1["403"]
    PX -->|"resolves to private, loopback,<br/>link-local or metadata address"| X2["403"]
    PX -->|"allowed name, public IP"| OK["Connect to that exact IP"]
```

The proxy resolves each name itself, once, checks the result against the deny
set and connects to that exact address, which is the defence against DNS
rebinding. The sandbox has no resolver of its own. The policy can be replaced
on a live sandbox, so "install dependencies, then cut the network" is one
sandbox, not two.

## Quick start

You need Linux with cgroup v2, rootless Podman (tested on 4.9.3 with crun),
Go 1.27+ and a Postgres database.

```sh
make build                                   # bin/sbxd, bin/sbx-mcp and a static bin/sbx-agent
systemctl --user enable --now podman.socket
docker run -d --rm --name sbx-dev-pg -e POSTGRES_PASSWORD=sbx \
    -p 127.0.0.1:55432:5432 postgres:17-alpine
export SBX_DATABASE_URL='postgres://postgres:sbx@127.0.0.1:55432/postgres?sslmode=disable'
podman pull docker.io/library/python:3.12-alpine

bin/sbxd preflight                           # checks the host; fix any FAIL line first
bin/sbxd admin tenant create --id acme --max-concurrent 20 --rate 50 --burst 100
bin/sbxd admin key create --tenant acme      # prints the API key once
bin/sbxd admin template apply --id python \
    --image docker.io/library/python:3.12-alpine \
    --memory 256m --workdir-size 64m --tmp-size 32m \
    --network allowlist --warm-target 2
bin/sbxd serve                               # listens on 127.0.0.1:8080
```

Then, from another terminal:

```sh
export SBX_API_KEY=sbx_...                   # the key printed above
curl -s -H "X-API-Key: $SBX_API_KEY" -d '{"template_id":"python"}' \
    localhost:8080/v1/sandboxes              # returns "id"
curl -N -H "X-API-Key: $SBX_API_KEY" \
    -d '{"cmd":"python3","args":["-c","print(6*7)"]}' \
    "localhost:8080/v1/sandboxes/$ID/commands?wait=stream"
```

`sbxd preflight` warns when lingering is off. Without it your sandboxes die
when you log out: `sudo loginctl enable-linger $USER`.

## Using it

### Python

```sh
pip install ./sdk/python
```

```python
from sbx import Sandbox

with Sandbox.create("python") as sandbox:          # killed when the block ends
    sandbox.files.write("data.csv", "a,b\n1,2\n")
    result = sandbox.commands.run("python3 -c 'print(open(\"data.csv\").read())'")
    print(result.stdout, result.exit_code)
```

There is an `AsyncSandbox` with the same surface. See
[`sdk/python/README.md`](sdk/python/README.md).

### TypeScript

```sh
cd sdk/typescript && npm install && npm run build
```

```ts
import { Sandbox } from 'sbx-sdk'

await using sandbox = await Sandbox.create('python', { timeoutMs: 5 * 60_000 })
const result = await sandbox.commands.run('python3', { args: ['-c', 'print(6*7)'] })
console.log(result.stdout) // "42\n"
```

No runtime dependencies; the data-plane parts also run in a browser with a
sandbox access token. See [`sdk/typescript/README.md`](sdk/typescript/README.md).

Neither SDK is published to PyPI or npm yet; install them from a checkout.

### MCP server

`sbx-mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server
over stdio. It gives an MCP client these tools:

| Tool | What it does |
|---|---|
| `create_sandbox`, `list_sandboxes`, `kill_sandbox` | Sandbox lifecycle |
| `list_templates` | What can be created |
| `run_command` | Run a shell line; returns exit code, stdout, stderr |
| `run_python` | Run Python source in a fresh interpreter |
| `write_file`, `read_file`, `list_files` | Move files in and out |
| `set_network` | Replace the egress policy of a live sandbox |

Claude Code:

```sh
claude mcp add sbx --env SBX_API_KEY=sbx_... --env SBX_BASE_URL=http://127.0.0.1:8080 \
    -- /path/to/bin/sbx-mcp --template python
```

Claude Desktop, Cursor and other clients that read an `mcpServers` block:

```json
{
  "mcpServers": {
    "sbx": {
      "command": "/path/to/bin/sbx-mcp",
      "args": ["--template", "python"],
      "env": { "SBX_API_KEY": "sbx_...", "SBX_BASE_URL": "http://127.0.0.1:8080" }
    }
  }
}
```

The API key is read from the environment only. Long output is cut in the middle
so the model keeps the start and the end, and the per-sandbox access token never
appears in a tool result.

### REST API

Everything above is a client of the REST API, described in
[`api/openapi.yaml`](api/openapi.yaml): sandboxes, commands (NDJSON streams with
reconnect by byte offset), a WebSocket terminal, files, live network policy,
events and Prometheus metrics. Two credentials exist: a tenant API key for the
control plane, and a per-sandbox access token, returned at create, that only
works on that sandbox's data-plane routes.

Sandbox and command events can also be pushed to you: **lifecycle webhooks**
are signed HTTPS callbacks (Standard Webhooks signatures), delivered in order
and at least once, with retries. The event catalogue, verification examples
and delivery guarantees are in [`docs/webhooks.md`](docs/webhooks.md).

Running a server for real (host ids, shutdown policies, density limits, metrics,
CORS) is covered in [`docs/operations.md`](docs/operations.md).

Coming from another sandbox product? See the migration guides from [E2B](docs/migrating-from-e2b.md) and from [open-source Daytona](docs/migrating-from-daytona.md).

## Threat model

Be clear about what the default tier is and is not.

**What it is for.** LLM-generated code run on behalf of a trusted or single
tenant: the risks are mistakes and prompt-injected misbehaviour. The sandbox
should hold no secrets, sit on a disposable host that runs nothing else, and
that host should get kernel updates within days.

**What it is not.** Hostile multi-tenant isolation. A rootless container shares
the host kernel, and 2026 produced several kernel bugs that escape containers.
Some are reached through system calls Python cannot run without, so no seccomp
profile closes them. Only a boundary that removes the shared kernel does.

For that reason the isolation engine is designed as a per-template setting, not
a fixed choice:

| Tier | Boundary | Needs KVM | State |
|---|---|---|---|
| Podman + crun, hardened | Shared host kernel | No | Default; tested |
| Podman + runsc (gVisor) | User-space kernel | No | Next; the template field exists but the tier is not enabled or tested |
| Firecracker | MicroVM with its own kernel | Yes | Planned |

Other limits worth knowing:

- The proxy filters on the hostname the client asks for and does not terminate
  TLS, so two sites behind one TLS endpoint can be confused (domain fronting).
  Allowlist narrow, single-purpose hostnames.
- Any allowlisted host is a possible exfiltration target.
- An out-of-memory kill is inferred from exit code 137 on Podman 4.9, so a
  manual `kill -9` of a sandbox is reported the same way.
- Disk is bounded by size-capped tmpfs, which is charged to the memory limit.

## Measured performance

One 4-thread laptop, Podman 4.9.3, crun, shared with other work. Treat these as
an order of magnitude, not a benchmark of your hardware. Method, raw data and
limits are in [`bench/RESULTS.md`](bench/RESULTS.md).

| Measurement | Result |
|---|---|
| Warm claim (`POST /v1/sandboxes`) | p50 6-9 ms, p95 10-16 ms |
| Claim to first output byte | p50 11-17 ms |
| First command through the agent | p50 1.5 ms (`podman exec` would be 76 ms) |
| Create to agent ready, off the request path | p50 87 ms |
| Delete answered | p50 4 ms; container gone about 165 ms later |
| Memory bomb to reported `oom_killed` | 0.4-0.7 s |
| Idle memory per sandbox | about 2.4-2.8 MB |

The first density limit on a default host is not memory but the kernel keyring
quota: about 190 sandboxes per user. `docs/operations.md` explains how to lift it.

## Roadmap

Done so far: the benchmark spike, the single-node core and the MVP (API, agent,
egress, SDKs, MCP server).

**Next: the hardened tier.** The goal is to make the "containers are not
enough" objection answerable without changing the scheduler.

1. Install gVisor, enable `--runtime runsc` templates in the server and run the
   whole test suite on them; fix what breaks (the agent socket, the egress
   bridge, cgroup delegation).
2. Measure gVisor's cost on real agent workloads (interpreter start, imports,
   `pip install`, fork-heavy shell) and publish it next to the crun numbers.
3. Give `sbxd` its own private Podman service and config, which removes the
   190-sandbox ceiling and two smaller Podman quirks.
4. ~~Lifecycle webhooks~~ (done: [`docs/webhooks.md`](docs/webhooks.md)), and
   migration guides from archived Daytona and from self-hosted E2B.
5. Re-run everything on Podman 6 and on a server-class host.

**After that: scale and a KVM tier.** Multi-host placement with leases and
per-tenant caps; a Firecracker backend with snapshot restore; a TLS-terminating
proxy that can inject credentials so secrets never enter a sandbox; preview URLs
for ports.

**Deliberately out of scope for now:** memory snapshot and fork, GPUs,
multi-region, a hosted cloud, non-Linux guests, browser or desktop images.

The reasoning behind this order is in
[`docs/research/build-plan.md`](docs/research/build-plan.md).

## Repository layout

| Path | What it is |
|---|---|
| `cmd/sbxd` | Server: `serve`, `preflight`, `admin` |
| `cmd/sbx-agent`, `internal/agent` | In-sandbox agent: processes, terminals, ring-buffer replay, files |
| `cmd/sbx-mcp`, `internal/mcp` | MCP server over stdio |
| `internal/api`, `api/openapi.yaml` | Public REST API |
| `internal/server` | Wiring and shutdown policy |
| `internal/core` | Domain types: state machine, templates, resources, network policy |
| `internal/runtime` | Isolation backend interface, defined above Podman |
| `internal/runtime/podman` | Rootless Podman backend, hardened spec, seccomp profile |
| `internal/preflight` | Host checks: cgroup delegation, lingering, subuid, quotas |
| `internal/store` | Postgres schema, migrations and the atomic claim |
| `internal/pool` | Pool controller, refill, timers, sweeper and reconciler |
| `internal/agentapi`, `internal/agentclient` | Host-to-agent wire contract and client |
| `internal/egress` | Allowlist egress proxy |
| `internal/webhook` | Lifecycle webhooks: delivery worker, signing, SSRF-safe sender |
| `sdk/python`, `sdk/typescript` | Client SDKs (`sbx`, `sbx-sdk`) |
| `test/e2e` | End-to-end tests against real Podman and Postgres |
| `bench` | Benchmark harness and results |
| `docs/operations.md` | Running a server |
| `docs/research` | The research notes and build plan this design follows |

## Build and test

```sh
make test     # unit tests; with SBX_TEST_DATABASE_URL also the store tests
make e2e      # needs SBX_E2E_DATABASE_URL and the Podman socket
```

`.github/workflows/ci.yml` runs the Go checks (with a Postgres service) and both
SDKs' checks on every push and pull request. The Podman integration tests and
`make e2e` need rootless Podman with cgroup v2 delegation and are run locally;
CI only tries them in a separate job that cannot fail the build.

## Licence

Apache-2.0.
