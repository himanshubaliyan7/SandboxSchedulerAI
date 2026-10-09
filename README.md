# SandboxSchedulerAI

A self-hostable sandbox scheduler for running LLM-generated Python and Bash:
a pre-warmed pool of rootless Podman sandboxes with cgroup v2 limits,
default-deny egress, one-way recycling and streaming exec/PTY. One Linux box,
no root daemon, no Kubernetes, no `/dev/kvm` required.

Status: early development. Nothing here is ready to run untrusted code.

## Threat model

The default tier (rootless Podman + crun) shares the host kernel. It is meant
for LLM-generated code run on behalf of a trusted or single tenant, on a
disposable host that holds nothing else, with kernel updates applied within
days. It is not hostile multi-tenant isolation. The isolation engine is a
per-pool setting so that gVisor (`runsc`) and, later, Firecracker can be
selected without changing the scheduler.

## Layout

| Path | What it is |
|---|---|
| `internal/core` | Domain types: sandbox state machine, templates, resources, network policy |
| `internal/runtime` | Isolation backend interface, defined above Podman |
| `internal/runtime/podman` | Rootless Podman backend over the Libpod REST socket, hardened spec |
| `internal/preflight` | Host checks: cgroup v2 delegation, lingering, subuid, sysctls |
| `internal/store` | Postgres schema, migrations and the atomic claim |
| `internal/pool` | Pool controller, refill, timers, sweeper and reconciler |
| `internal/agentapi` | Wire contract between the host and the in-sandbox agent |
| `internal/agent`, `cmd/sbx-agent` | In-sandbox agent: processes, PTYs, ring-buffer replay, files |
| `internal/agentclient` | Host-side client for the agent |
| `internal/egress` | Allowlist egress proxy with resolve-once IP validation |
| `docs/research` | The research notes and build plan this design follows |

## Build

Requires Go 1.27+.

```sh
make test
make build
```

## Licence

Apache-2.0.
