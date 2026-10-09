# bench: phase-0 benchmark harness

Results and their interpretation for the first host measured are in `RESULTS.md`.

Measures the numbers the build plan could not source (`docs/research/build-plan.md`,
"What research could not source, and so must be measured in phase 0") on the host it
is run on. Python 3 standard library only; no `hyperfine`, no root, no changes to the
user's Podman configuration.

## Run it

```sh
# everything, default sample counts (52 minutes on the 4-thread laptop in RESULTS.md)
python3 bench/sbxbench.py

# the run that produced bench/RESULTS.md (adds the opt-in quota confirmation, see below)
python3 bench/sbxbench.py --push-to-limit

# smoke test in ~3 minutes; the numbers are not meaningful
python3 bench/sbxbench.py --quick

# a subset
python3 bench/sbxbench.py --only lifecycle,api --samples 100 --rounds 3

# the workload suite under another runtime (for the gVisor comparison later)
python3 bench/sbxbench.py --only workload --runtime runsc
```

Requirements: rootless Podman with the `alpine:3.20` and `python:3.12-alpine` images
already pulled, cgroup v2 with cpu/memory/pids delegated, and Go on `PATH` or at
`~/.local/opt/go/bin` (to build the static `sbx-agent`; without it the harness falls
back to `sleep` as the idle process and says so in the results).

## Output

`bench/results/<timestamp>/`:

| file | content |
|---|---|
| `host.json` | kernel, CPU model and count, RAM, podman/crun/runc/conmon versions, storage driver and graph status, filesystem of the graph root, cgroup controllers, relevant sysctls, load average at start and end, top CPU consumers at start |
| `results.json` | every raw sample (ms), per (benchmark, cell, operation, round), with the load average at the start and end of each row, every error message, and the structured results (density steps, parallel throughput, probes) under `extra` |
| `summary.csv` | one line per (benchmark, cell, operation): n, mean, stddev, min, p50, p95, p99, max, per-round medians, load range, error count |
| `summary.md` | the same as readable tables |

## What is measured

| `--only` name | what |
|---|---|
| `floor` | cost of the timing method: spawning `/bin/true`, `podman --version`, a no-op `podman ps` |
| `lifecycle` | CLI latency matrix on the default store: {crun, runc} x {none, pasta, slirp4netns} x {plain, hardened}; per cycle `create`, `start`, first and second `exec true`, `rm -f -t 0` of the running container, and a separate `run --rm IMAGE true` |
| `storage` | the same cycle for crun/none on isolated stores: {native overlay, fuse-overlayfs} x {persistent, `--transient-store`} |
| `api` | hardened crun/none with the real `sbx-agent` as PID 1, driven through the Libpod REST API on a Unix socket and through the CLI, interleaved; also the agent's own RPC (`/v1/init`, start a process and read its exit event) |
| `userns` | create and start for default(host), `keep-id`, `keep-id:uid=1000,gid=1000`, `nomap`, `auto`; first use in a brand-new store versus later uses, native overlay and fuse-overlayfs, two images; `auto` while other `auto` containers are alive; how many `auto` containers fit in the subuid budget |
| `pause` | `pause`/`unpause` (CLI and API), exec into an idle running sandbox (CLI, API, agent RPC), unpause-then-RPC, and raw `cgroup.freeze` written by the rootless user |
| `density` | ramp of idle hardened sandboxes in steps; per step: host `MemAvailable` delta per sandbox, cgroup `memory.current`, conmon PSS/RSS, processes, threads, fds, inotify instances, kernel keys, create latency; request-path latency at the peak; teardown rate |
| `parallel` | create throughput (create + start + agent ready) and teardown rate at concurrency 1, 2, 4, 8, API and CLI |
| `workload` | Python start, 30 stdlib imports, CPU loop, 2000 small files, fork-heavy shell loop, 100 subprocesses; timed inside plain and hardened sandboxes; `--runtime` selects the OCI runtime |
| `probes` | `cgroup.kill`, `memory.oom.group`, `--storage-opt size=`, re-attaching a detached exec, seccomp behaviour for a list of syscalls under four profiles, session-keyring quota, rootless checkpoint, cgroup delegation |

"Hardened" is the CLI equivalent of `podman.BuildCreateSpec`
(`internal/runtime/podman/spec.go`): cap-drop all, no-new-privileges, read-only root,
capped tmpfs on `/workspace`, `/tmp`, `/var/tmp`, `/run`, network none, 1 CPU, 256 MiB
memory with memory-swap equal to it, `memory.high` at 90%, `memory.oom.group=1`, 128 pids,
nofile 1024, user 1000:1000 with `keep-id:uid=1000,gid=1000`, the product seccomp profile
(`internal/runtime/podman/seccomp/sbx-seccomp.json`), log driver none, plus the run-dir and
agent bind mounts. The REST benchmarks send the same SpecGenerator JSON as `spec.go`.
"Plain" is `podman run --runtime R --network N` and nothing else.

## Statistics

Every latency cell is `--samples` timed cycles (default 50) after `--warmup` discarded
cycles (default 5), repeated in `--rounds` rounds (default 2). Rounds are interleaved
across cells, so a cell's two rounds are tens of minutes apart; the per-round medians in
the tables show how much the machine moved in between. Reported: n, mean, sample standard
deviation, min, p50, p95, p99, max (linear-interpolated percentiles). CLI timings are wall
clock around `subprocess.run`, which includes about 1 ms of fork/exec (see `floor`).

## Safety

- Every container is named `sbxbench-*`. The harness lists and removes by that prefix
  only (`podman ps -a --filter name=^sbxbench-`), in the default store and in its own
  isolated stores. It never runs a global `rm -a`, `prune` or `system reset`.
- Other storage configurations use isolated stores (`--root bench/.work/<id>/sN
  --runroot $XDG_RUNTIME_DIR/sbxb-<id>/rN`). They are deleted with `podman unshare rm -rf`
  at exit, also on SIGINT/SIGTERM. `~/.config/containers` is never read for writing.
- REST calls go to the harness's own `podman system service --time 0` on a private
  socket, not to the user's `podman.socket`.
- If that service process exits, the harness records its exit code and log under
  `extra.service_deaths` in `results.json` and starts a new one.
- The agent binary is built into the temp directory; nothing outside `bench/` changes.
- The density ramp stops when `MemAvailable` falls below `--density-mem-floor-mb` (2500),
  `--density-key-headroom` (20) keys short of `kernel.keys.maxkeys`, or
  `--density-inotify-headroom` (64) short of `fs.inotify.max_user_instances`.
- `--push-to-limit` is off by default. With it, after the last ramp the harness keeps
  starting minimal containers until the first real error, records the message and removes
  them at once. It exhausts a per-user kernel quota for a few seconds, which would fail
  anybody else's container start for that user in that window, so it refuses to run while
  containers that are not `sbxbench-*` exist.

If a run is killed with SIGKILL, clean up by hand:

```sh
podman ps -a --filter name=^sbxbench- --format '{{.Names}}' | xargs -r podman rm -f -t 0
podman unshare rm -rf bench/.work "$XDG_RUNTIME_DIR"/sbxb-*
```

## Not covered here

gVisor (`runsc` is selected by `--runtime runsc` for the workload suite but was not
installed where this was written), Podman 6, Firecracker, the egress tamper tests, and
any root-only setting (XFS project quotas, raising sysctls).
