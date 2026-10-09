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

## Quick start

Needs Linux with cgroup v2, rootless Podman (tested on 4.9.3 with crun), Go 1.27+
and a Postgres database. The sandbox image must already be pulled for the Podman user.

```sh
make build                      # bin/sbxd and a static bin/sbx-agent
systemctl --user enable --now podman.socket
docker run -d --rm --name sbx-dev-pg -e POSTGRES_PASSWORD=sbx -p 127.0.0.1:55432:5432 postgres:17-alpine
export SBX_DATABASE_URL='postgres://postgres:sbx@127.0.0.1:55432/postgres?sslmode=disable'
podman pull docker.io/library/python:3.12-alpine

bin/sbxd preflight
bin/sbxd admin tenant create --id acme --max-concurrent 20 --rate 50 --burst 100
bin/sbxd admin key create --tenant acme          # prints the key once
bin/sbxd admin template apply --id python --image docker.io/library/python:3.12-alpine \
    --memory 256m --workdir-size 64m --tmp-size 32m --network allowlist --warm-target 2
bin/sbxd serve                                   # 127.0.0.1:8080

curl -H "X-API-Key: $KEY" -d '{"template_id":"python"}' localhost:8080/v1/sandboxes
curl -N -H "X-Access-Token: $TOKEN" -d '{"cmd":"python3","args":["-c","print(6*7)"]}' \
    "localhost:8080/v1/sandboxes/$ID/commands?wait=stream"
```

The REST API is described in `api/openapi.yaml`.

## Measured on a 4-thread laptop

Podman 4.9.3, crun, network none, hardened spec. One machine, shared with other
work; see `bench/RESULTS.md` for method and limits.

| Measurement | Result |
|---|---|
| Warm claim (`POST /v1/sandboxes`) | p50 6-9 ms, p95 10-16 ms |
| Claim to first output byte | p50 11-17 ms |
| Create to agent ready (off the request path) | p50 87 ms |
| Idle memory per sandbox | about 2.4-2.8 MB |

## Layout

| Path | What it is |
|---|---|
| `cmd/sbxd` | Server: `serve`, `preflight`, `admin` |
| `cmd/sbx-agent`, `internal/agent` | In-sandbox agent: processes, PTYs, ring-buffer replay, files |
| `internal/api`, `api/openapi.yaml` | Public REST API: sandboxes, commands, streams, WebSocket PTY, files |
| `internal/server` | Wiring and shutdown policy |
| `internal/core` | Domain types: sandbox state machine, templates, resources, network policy |
| `internal/runtime` | Isolation backend interface, defined above Podman |
| `internal/runtime/podman` | Rootless Podman backend over the Libpod REST socket, hardened spec, seccomp profile |
| `internal/preflight` | Host checks: cgroup v2 delegation, lingering, subuid, sysctls |
| `internal/store` | Postgres schema, migrations and the atomic claim |
| `internal/pool` | Pool controller, refill, timers, sweeper and reconciler |
| `internal/agentapi`, `internal/agentclient` | Host-to-agent wire contract and client |
| `internal/egress` | Allowlist egress proxy with resolve-once IP validation |
| `test/e2e` | End-to-end test against real Podman and Postgres (`make e2e`) |
| `bench` | Benchmark harness and results |
| `docs/research` | The research notes and build plan this design follows |

## Build and test

```sh
make test     # unit tests
make e2e      # needs SBX_E2E_DATABASE_URL and the Podman socket
```

## Licence

Apache-2.0.
