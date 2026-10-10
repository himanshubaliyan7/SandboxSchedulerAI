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

## Operating notes

- **Host id.** Every container carries an `sbx.host_id` label and a server only
  ever touches containers and database rows with its own id; containers of another
  id are left alone, and `sbx.managed` containers with no id (made by an older
  version) are left alone with a warning. The default id is
  `<hostname>-u<uid>-<hash of the run dir>`; set `--host-id` / `SBX_HOST_ID` to pin
  it. It must stay the same across restarts, and each server needs its own
  `--run-dir` (a second server on a run dir in use refuses to start). Several
  servers can then share one Podman user.
- **Density ceiling.** Each running container holds one kernel key of the user's
  `kernel.keys.maxkeys` quota (200 by default), so about 190 sandboxes per user
  before creates fail with `Disk quota exceeded`. The Libpod API has no
  per-container switch; set `keyring = false` under `[containers]` in
  `~/.config/containers/containers.conf` and restart the Podman service, or raise
  the sysctl as root. `sbxd preflight` and server start-up report the real ceiling
  against the templates' `max_total`, and the same for inotify instances.
- **Metrics.** `/metrics` is unauthenticated on the API listener, which is only
  allowed while `--listen` is a loopback address: otherwise `sbxd serve` refuses to
  start unless `--metrics-token` (bearer token) or `--metrics-listen` (separate
  listener, route removed from the API listener) is set.
- **Delete.** `DELETE /v1/sandboxes/{id}` answers once the sandbox is fenced and
  destroys the container in the background; `?wait=true` waits for it.
- **Shutdown.** On SIGTERM or SIGINT `sbxd serve` closes the API (open streams and
  WebSockets end with `shutting_down`), then stops the refill loop and waits for the
  creates in flight, and only then touches a sandbox, so a stop never starts a
  container or leaves one half made. What remains is decided by `--shutdown-policy`:

  | Policy | Tenant sandboxes | Warm sandboxes | Containers of this host id after the stop |
  |---|---|---|---|
  | `drain` (default) | released, reason `host_drain` | **left running** | exactly the warm pool |
  | `leave` | left running, unreachable | left running | warm and tenant sandboxes |
  | `destroy` | destroyed, reason `host_drain` | destroyed | none |

  Leaving the warm pool is deliberate: a warm sandbox holds no tenant data, and the
  next start re-adopts it after one readiness probe, so a restart does not pay for a
  cold pool. It also means that after a default stop `podman ps` still shows
  `warm-target` containers per template. They are not leaked: they are labelled with
  the host id, recorded in the database, and adopted or reaped by the next start.
  With `leave`, the next start recycles the tenant sandboxes (`host_drain`). To stop a
  server for good use `--shutdown-policy destroy`, or afterwards
  `sbxd admin host destroy --run-dir <its run dir>` (add `--host-id` if it was set),
  which removes the containers, run dirs and live sandbox rows of a host id without
  starting the server and refuses to run while that server is up. Neither ever touches
  a container of another host id. A second signal during the stop kills the process.
- **Browsers (CORS).** Off by default. `--cors-allowed-origins https://app.example.com`
  (comma-separated exact origins, or `*`) lets pages on those origins call the
  data-plane routes (commands, files, the PTY WebSocket) with a sandbox access token.
  The control plane never takes part, and a tenant API key is refused from a browser
  origin (`403 api_key_not_allowed`) unless `--cors-allow-api-key` is also given. With
  the flag set, a request or WebSocket handshake from any other origin is refused.
  Details are in `api/openapi.yaml` under "Browsers and CORS".
- **Retrying a command start.** A start that carries `Idempotency-Key` runs at most
  once per sandbox and key; a repeat answers for the first command. See "Not knowing
  whether a command started" in `api/openapi.yaml`.
- **`oom` file.** When a sandbox is OOM-killed, conmon creates an empty file named
  `oom` in the working directory of the Podman service (the user's home for the
  systemd-activated service). Podman 4.9 has no option for it; a systemd drop-in
  with `WorkingDirectory=` for `podman.service` moves it.

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
| `cmd/sbxd` | Server: `serve`, `preflight`, `admin` (tenants, keys, templates, `host destroy`) |
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
| `sdk/python`, `sdk/typescript` | E2B-shaped client SDKs (`sbx`, `sbx-sdk`) |
| `test/e2e` | End-to-end test against real Podman and Postgres (`make e2e`) |
| `bench` | Benchmark harness and results |
| `docs/research` | The research notes and build plan this design follows |

## Build and test

```sh
make test     # unit tests; with SBX_TEST_DATABASE_URL also the store tests
make e2e      # needs SBX_E2E_DATABASE_URL and the Podman socket
```

`.github/workflows/ci.yml` runs the Go checks (with a Postgres service) and both SDKs'
checks on every push and pull request. The Podman integration tests and `make e2e`
need rootless Podman with cgroup v2 delegation and are run locally; CI only tries
them in a separate job that cannot fail the build.

## Licence

Apache-2.0.
