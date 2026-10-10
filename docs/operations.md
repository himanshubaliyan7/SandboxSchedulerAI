# Operating notes

- **Host id.** Every container carries an `sbx.host_id` label and a server only
  ever touches containers and database rows with its own id; containers of another
  id are left alone, and `sbx.managed` containers with no id (made by an older
  version) are left alone with a warning. The default id is
  `<hostname>-u<uid>-<hash of the run dir>`; set `--host-id` / `SBX_HOST_ID` to pin
  it. It must stay the same across restarts, and each server needs its own
  `--run-dir` (a second server on a run dir in use refuses to start). Several
  servers can then share one Podman user.
- **Podman service.** By default (`--podman-mode managed`) `sbxd serve` does not use
  your Podman socket. It starts its own `podman system service` on
  `<run-dir>/podman.sock` (mode 0600), with a `containers.conf` it writes to
  `<run-dir>/podman/` and passes as `CONTAINERS_CONF_OVERRIDE`, supervises it, and
  stops it on shutdown. Your own `containers.conf` is neither read for these settings
  nor changed. The service shares your image and container store, so `podman pull`
  is how images get there and `podman ps` shows the sandboxes. What the generated
  file sets: `keyring = false`, `default_sysctls = []`, `log_driver = "none"`,
  `events_logger = "none"`, `cgroup_manager = "systemd"` and the runtime table.
  - If the service dies, the supervisor restarts it with backoff (200 ms doubling to
    10 s); sandboxes are conmon's children, not the service's, and keep running.
    `/readyz` fails while it is down. Its output is in `<run-dir>/podman/service.log`.
  - If `sbxd` is killed outright, the service gets SIGTERM from the kernel
    (`PR_SET_PDEATHSIG`), and the next start replaces one that survived anyway (pid
    file). The sandboxes stay, and the next start re-adopts the warm ones as after
    any restart.
  - `--podman-socket <path>` (or `--podman-mode external`) keeps the old behaviour:
    an existing service, its own configuration, nothing supervised. The limits under
    "External Podman service" below then apply, and only the `crun` tier is available.
  - `sbxd admin host destroy` starts a throwaway service of the same kind (with the
    configuration the server left in the run dir) when it is not given a socket.
  - Do not delete `<run-dir>/podman*/containers.conf` or `<run-dir>/podman/runsc`
    while sandboxes of that run dir are alive: each container's cleanup command
    refers to them. To use the `podman` CLI on sandboxes of the gVisor tier, run it
    with `CONTAINERS_CONF_OVERRIDE=<run-dir>/podman/containers.conf` (your own
    configuration does not know that runtime, and Podman says so).
- **Density.** With the managed service a sandbox holds no kernel key, so
  `kernel.keys.maxkeys` no longer limits anything: 1,200 warm sandboxes ran on an
  11 GB laptop with `/proc/key-users` unchanged (6 of 200) and no failed create
  (about 3.3 MB of host memory each for a 64 MiB alpine template under crun; a
  gVisor sandbox idles at about 19 MB). What limits a host now, in the order you
  will meet it: memory; Podman's lock table (`num_locks`, 2,048 containers per user
  by default; raising it needs `podman system renumber`); and per-user limits such
  as `fs.inotify.max_user_instances`, which `sbxd preflight` reports.
- **External Podman service** (`--podman-socket`). Each running container holds one
  kernel key of the user's `kernel.keys.maxkeys` quota (200 by default), so about
  190 sandboxes per user before creates fail with `Disk quota exceeded`. The Libpod
  API has no per-container switch; set `keyring = false` under `[containers]` in
  `~/.config/containers/containers.conf` and restart the Podman service, or raise
  the sysctl as root. `sbxd preflight --podman-socket ...` and server start-up report
  the real ceiling against the templates' `max_total`. The service's
  `default_sysctls` also stay in force (one `ping_group_range` write per start).
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
- **Out-of-memory kills.** When a sandbox is OOM-killed, conmon creates an empty
  file named `oom` in the working directory of the Podman service that started it.
  With the managed service that is `<run-dir>/podman/cwd` (and `podman-runsc/cwd`),
  and `sbxd` consumes the file: a sandbox whose main process ended with SIGKILL is
  reported `oom_killed` only if such a file appeared within two seconds before, or
  one second after, the container's recorded end. So a `kill -9` of a sandbox's
  process from the host is now reported as `instance_lost` (with `exit_code` 137 in
  the event), not as `oom_killed`, unless another sandbox of the same server was
  OOM-killed in those three seconds. With an external service the file lands in
  that service's directory (your home, for the systemd-activated one), `sbxd`
  does not read it, and exit code 137 alone is taken as an OOM kill, as before.
- **Isolation tiers.** A template's `runtime` is its tier: `crun` (a hardened
  container on the host kernel) or `runsc` (gVisor: the sandbox's system calls are
  served by a user-space kernel; no KVM needed). Enable the gVisor tier on the server
  and on the admin command with the same setting:

  ```sh
  export SBX_RUNTIMES=crun,runsc=/opt/gvisor/runsc    # or --runtimes; a bare "runsc" is looked up in PATH
  sbxd preflight                                      # says "runsc ... enabled and usable"
  sbxd admin template apply --id py-gv --runtime runsc --image ... --memory 512m ...
  sbxd serve
  ```

  A template that names a runtime the server has not enabled is refused by
  `template apply` and stops `sbxd serve` at start; one applied to a running server
  gets no pool and its creates answer `503 runtime_not_enabled`. `GET /v1/templates`
  reports `runtime` and `shared_kernel` per template. Both tiers run side by side in
  one server; each has its own Podman service (`podman.sock`, `podman-runsc.sock`),
  because Podman 4.9 runs every container of a service with that service's default
  runtime and ignores a per-container one. `sbxd` checks after every create that
  Podman used the runtime that was asked for. gVisor needs managed mode, the systemd
  cgroup manager and `systemd-run`; extra runsc flags go in `--runsc-flags`
  (`SBX_RUNSC_FLAGS`), for example `--platform=kvm`. Keep the unpacked
  gVisor release together (`runsc` next to its `gvisor-bin/`); it was only tested that way.

  What differs under `runsc`, all of it checked by the test suite:

  | | crun | runsc (gVisor) |
  |---|---|---|
  | Kernel the sandbox sees | the host's | gVisor's (`uname -r`: `4.19.0-gvisor`) |
  | Seccomp profile | the hardened profile (`Seccomp: 2`) | not applied (`Seccomp: 0`): system calls are the sentry's own implementation; `--runsc-flags=--oci-seccomp` applies it, with `EPERM` for every denied call |
  | Nested user namespace, `AF_NETLINK`, `ptrace` | refused | allowed, inside the sentry only; `AF_ALG`, `io_uring`, `userfaultfd`, `keyctl` do not exist; `bpf`, `mount`, `AF_PACKET` are refused |
  | Process limit (`pids_limit`) | cgroup `pids.max`; `pids_limit_hits` counts | `RLIMIT_NPROC` inside the sandbox (fork fails with `EAGAIN` the same way); the cgroup's `pids.max` is `4 x limit + 256` for the sentry's threads, `pids_current` counts those, `pids_limit_hits` stays 0 |
  | Memory limit | the sandbox's processes | also holds the sentry and gofer: about 16 MiB of it is used by an idle sandbox, and processes cost more |
  | `/proc/meminfo`, `nproc` in the sandbox | the host's | the host's as well (the sentry is not told the limits; they are enforced on its cgroup) |
  | Host process of the sandbox | runs as the pool user (keep-id) | the sentry, as a subordinate UID |
  | `/sys/class/net` | `lo` | absent (use `/proc/net/dev`: `lo` only) |
  | Pause | cgroup freezer | `runsc pause` (same API) |
  | Cost | baseline | see the README's table: about 2x create, 3-4x interpreter start, 10x fork-heavy shell |
