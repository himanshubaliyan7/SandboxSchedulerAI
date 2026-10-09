# Phase-0 benchmark results

Measured on one machine with `bench/sbxbench.py`. Every number below is copied from the
result files named in "Runs"; nothing is estimated unless the sentence says so. All
latencies are wall-clock milliseconds.

## Headlines

- **Request path.** One command through the in-sandbox agent (new connection, start
  `true`, read the exit event) takes **1.49 ms p50, 1.62 ms p95, 1.70 ms p99** (n=100).
  The same through `podman exec` costs **76.2 ms p50 / 89.3 ms p99** over the REST socket
  and **148.7 ms p50 / 184.1 ms p99** through the CLI. The agent is the request path;
  `podman exec` is not.
- **Create to ready** (hardened spec, crun, network none, real agent as PID 1, REST API,
  one at a time): **86.6 ms p50, 94.9 ms p95, 96.4 ms p99, 118.7 ms max**, mean 87.2 ms
  (n=100). With 176 idle sandboxes already on the host it is about 100 ms p50.
- **Throughput.** 10.6 to 11.6 creates/s serial, about 17 at concurrency 2, and 10 to 20
  at concurrency 4 to 8, where it stops scaling on 4 hardware threads and the tail goes
  past 1 s. Removal: 9 to 14 per second.
- **Idle memory.** 2.4 to 2.8 MB of host memory per idle sandbox (regression slope over
  the ramp), which is roughly 370 to 430 idle sandboxes per GB.
- **First density limit is not memory.** It is `kernel.keys.maxkeys=200`: every running
  container holds one session keyring charged to the host user. The 197th container
  failed with `crun: create keyring ...: Disk quota exceeded`.
- **User namespace.** Any mapping other than the default costs a one-time 2.2 s copy of a
  57 MB image on native overlay; after that, `create` is the same 58 ms. `--userns=auto`
  pays that 2 s on every create while other sandboxes are alive, and stops at 64
  containers.
- **Surprise.** The read-only root filesystem makes Python recompile every module on
  every start: 30 stdlib imports take 1153 ms instead of 173 ms. Setting
  `PYTHONPYCACHEPREFIX=/tmp/pycache` brings it back to 174 ms.

## Method

- **Host.** Lenovo laptop, Intel Core i5-7200U (2 cores, 4 threads) at 2.5 GHz, 11.5 GB
  RAM, 9.7 GB swap, Ubuntu 24.04, kernel 7.0.0-28-generic. Rootless Podman 4.9.3, crun
  1.14.1, runc 1.4.3, conmon 2.1.10, netavark 1.4.0, pasta (passt 2024-02-20),
  slirp4netns 1.2.1, fuse-overlayfs present, runsc absent. Storage: overlay (native
  diff, no ID-shifting support, not transient) on ext4 on `/dev/sda`; SQLite database
  backend; cgroup v2, systemd manager, cpu/memory/pids delegated. `hyperfine` is not
  installed; the harness does its own timing.
- **Sampling.** Each latency cell is 50 timed cycles after 5 discarded warm-up cycles,
  in 2 rounds, 100 samples in all. Rounds are interleaved across cells, so the two
  rounds of a cell are 10 to 20 minutes apart; where a table shows two round medians,
  their difference is the drift of the machine. Statistics are n, mean, sample standard
  deviation, min, p50, p95, p99, max. With n=100 the p99 is close to the second-worst
  sample and should be read as an indication.
- **Timing.** CLI numbers are wall time around the `podman` process, which includes
  about 1 ms for spawning it. REST numbers are wall time of the HTTP calls on a kept-alive
  Unix-socket connection to the harness's own `podman system service --time 0`.
- **Specs.** "plain" is `podman run --runtime R --network N`. "hardened" is the
  product's create spec from `internal/runtime/podman/spec.go` (cap-drop all,
  no-new-privileges, read-only root, capped tmpfs mounts, network none, 1 CPU, 256 MiB
  with swap disabled, `memory.high`, `memory.oom.group=1`, 128 pids, uid 1000 with
  `keep-id:uid=1000,gid=1000`, the product seccomp profile, log driver none). Images:
  alpine:3.20 for latency and density, python:3.12-alpine for the workload suite.
- **Removal** is always `rm -f -t 0` (or `force=true&timeout=0`): with Podman's default
  10 s stop timeout, removing a container whose PID 1 ignores SIGTERM takes 10 s.
- **Isolated stores.** fuse-overlayfs, the transient store and the first-use
  user-namespace measurements ran in throwaway stores (`--root` under `bench/.work`,
  `--runroot` under `$XDG_RUNTIME_DIR`), on the same ext4 filesystem as the real graph
  root. The user's storage configuration was not touched.

### Runs

| run | directory | when | content | load |
|---|---|---|---|---|
| A | `bench/results/20261009-201913` | 9 Oct, 20:19 to 21:11 (3108 s) | every benchmark, `--push-to-limit` | load1 0.83-3.44 across all rows |
| probes | `bench/results/20261010-005155` | 10 Oct, 00:51 (20 s) | host probes again | load1 0.31 |
| B | `bench/results/20261010-005309` | 10 Oct, 00:53 (99 s) | parallel throughput again | load1 1.7 to 5.1 |
| C | `bench/results/20261010-005501` | 10 Oct, 00:55 (126 s) | parallel throughput, n=64, concurrency 4 and 8, 3 rounds | load1 3.6 to 5.3 |

Run A recorded 23999 samples and 0 failed operations in the latency
benchmarks. Its host facts: `{"load_average": [0.83, 1.04, 1.18], "load_average_at_end": [1.73, 1.7, 1.9], "wall_seconds": 3107.8, "top_cpu_processes_at_start": ["%CPU %MEM COMMAND", "10.7  0.2 python3", " 1.8  4.0 claude", " 1.2  4.1 gnome-shell", " 0.6  0.5 gnome-terminal-", " 0.5  1.3 Xorg", " 0.1  0.0 irq/132-iwlwifi", " 0.1  0.1 systemd", " 0.1  0.0 kworker/u16:6-flush-8:0"]}`. Load1 at the end of each benchmark: `{"floor": [0.84, 1.04, 1.18], "lifecycle": [2.44, 2.41, 2.02], "storage": [1.57, 1.85, 1.91], "api": [1.47, 1.77, 1.87], "userns": [2.34, 2.44, 2.19], "pause": [1.93, 2.32, 2.16], "density": [1.51, 1.86, 2.0], "parallel": [2.74, 2.07, 2.06], "workload": [1.07, 1.57, 1.86], "probes": [1.73, 1.7, 1.9]}`.
The harness itself keeps load1 near 1 to 1.5, so most of the 0.8 to 3.4 range in run A
is the benchmark. Another engineer was working in the same repository and no foreign
containers existed when run A started. Runs B and C overlapped that engineer's Podman
end-to-end tests (9 foreign containers at the start of B), which is why their numbers
are noisier.

Two things went wrong in run A and were re-run:

- The harness's API service process had exited by the time the parallel benchmark
  reached concurrency 4, so the REST rows for concurrency 4 and 8 and all of round 2
  failed with `Connection refused`. The service log was in the temporary directory and
  was deleted, so the cause is not known. The harness now records the exit code and log
  of a dead service and restarts it. It did not recur in runs B and C (640 REST creates).
- The same dead service made the `exec_detached_reattach` probe fail on a JSON parse.
  The probes were re-run in full.

## 0. Harness floor

| operation | n | mean | sd | min | p50 | p95 | p99 | max | p50 round 1 / round 2 | errors |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|
| `spawn_bin_true` | 100 | 1.0 | 0.1 | 0.9 | 1.0 | 1.2 | 1.4 | 2.1 | 1.0 / 1.0 | 0 |
| `podman_version` | 100 | 21.4 | 1.1 | 19.5 | 21.1 | 24.0 | 25.3 | 25.7 | 21.1 / 21.1 | 0 |
| `podman_ps_noop` | 100 | 26.4 | 1.0 | 24.9 | 26.3 | 28.0 | 29.0 | 31.3 | 26.3 / 26.3 | 0 |

A `podman` CLI invocation that touches the store costs about 26 ms before it does any
work. That is the floor under every CLI number below.

## 1. Lifecycle latency matrix (CLI, default store)

Cell is runtime/network/spec. Each entry is **p50 / p95 / max**, n=100 per entry, no
failed operation in any cell. `create` to `rm -f` are one container's life with
`sleep 86400` as its command; `run --rm true` is a separate container.

| cell | `run --rm true` | `create` | `start` | create+start | `exec true` (first) | `rm -f` (running) | load1 range |
|---|---:|---:|---:|---:|---:|---:|---|
| crun/none/plain | 173 / 185 / 223 | 56 / 61 / 72 | 93 / 100 / 102 | 151 / 160 / 170 | 165 / 178 / 187 | 122 / 135 / 168 | 0.84-2.29 |
| crun/none/hardened | 176 / 227 / 282 | 58 / 69 / 119 | 95 / 131 / 197 | 153 / 191 / 309 | 168 / 186 / 212 | 118 / 152 / 201 | 1.52-2.79 |
| crun/pasta/plain | 203 / 221 / 242 | 57 / 59 / 82 | 143 / 170 / 184 | 200 / 228 / 240 | 161 / 171 / 177 | 125 / 135 / 145 | 1.87-3.29 |
| crun/pasta/hardened | 213 / 251 / 295 | 58 / 62 / 120 | 126 / 159 / 340 | 184 / 219 / 443 | 163 / 179 / 213 | 126 / 146 / 275 | 1.80-2.81 |
| crun/slirp4netns/plain | 185 / 196 / 227 | 57 / 59 / 69 | 101 / 109 / 146 | 157 / 168 / 203 | 164 / 181 / 208 | 122 / 137 / 155 | 1.84-2.74 |
| crun/slirp4netns/hardened | 192 / 208 / 368 | 58 / 61 / 114 | 105 / 131 / 153 | 163 / 192 / 252 | 165 / 176 / 213 | 124 / 135 / 315 | 2.09-2.39 |
| runc/none/plain | 381 / 403 / 427 | 57 / 59 / 68 | 177 / 187 / 212 | 234 / 244 / 269 | 218 / 236 / 247 | 233 / 243 / 276 | 1.65-2.16 |
| runc/none/hardened | 390 / 435 / 548 | 58 / 69 / 110 | 182 / 209 / 282 | 240 / 279 / 379 | 214 / 246 / 287 | 237 / 300 / 412 | 1.40-2.22 |
| runc/pasta/plain | 414 / 503 / 599 | 57 / 74 / 129 | 221 / 244 / 277 | 278 / 321 / 372 | 218 / 239 / 594 | 245 / 289 / 690 | 1.60-2.05 |
| runc/pasta/hardened | 432 / 530 / 860 | 58 / 87 / 149 | 216 / 288 / 390 | 273 / 373 / 476 | 219 / 259 / 480 | 245 / 353 / 436 | 1.39-3.36 |
| runc/slirp4netns/plain | 395 / 606 / 1033 | 57 / 174 / 276 | 183 / 328 / 719 | 241 / 502 / 995 | 216 / 317 / 466 | 244 / 420 / 699 | 1.41-3.25 |
| runc/slirp4netns/hardened | 403 / 448 / 680 | 58 / 73 / 204 | 190 / 217 / 347 | 247 / 290 / 551 | 215 / 260 / 444 | 241 / 330 / 403 | 1.65-2.82 |

Full statistics for the cell closest to the product (crun, none, hardened):

| operation | n | mean | sd | min | p50 | p95 | p99 | max | p50 round 1 / round 2 | errors |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|
| `run_rm_true` | 100 | 181.5 | 20.3 | 165.5 | 175.9 | 226.5 | 281.7 | 282.5 | 175.0 / 177.9 | 0 |
| `create` | 100 | 59.5 | 8.7 | 55.4 | 57.7 | 68.7 | 112.1 | 118.8 | 57.1 / 58.3 | 0 |
| `start` | 100 | 100.3 | 15.5 | 88.4 | 95.3 | 131.5 | 158.6 | 196.7 | 94.8 / 98.9 | 0 |
| `create_plus_start` | 100 | 159.8 | 22.3 | 145.1 | 153.3 | 191.0 | 277.4 | 308.8 | 152.0 / 157.5 | 0 |
| `exec_first` | 100 | 162.8 | 16.1 | 131.5 | 167.9 | 185.7 | 197.0 | 212.2 | 170.4 / 165.2 | 0 |
| `exec_again` | 100 | 157.7 | 17.5 | 117.1 | 161.7 | 178.0 | 215.3 | 219.0 | 161.8 / 161.3 | 0 |
| `rm_f_running` | 100 | 123.1 | 17.3 | 103.2 | 118.0 | 151.6 | 196.8 | 200.8 | 114.3 / 123.7 | 0 |

What the matrix shows:

- **crun versus runc.** runc adds about 85 ms to `start` (182 versus 95 ms p50), about
  120 ms to `rm -f` and more than doubles `run --rm` (390 versus 176 ms).
- **Network.** `create` does not depend on it. pasta adds 31 ms to `start` in the
  hardened cell (126 versus 95 ms) and 50 ms in the plain cell; slirp4netns adds about
  10 ms. On this Podman 4.9.3 host slirp4netns starts faster than pasta.
- **Hardening is free at this level.** Hardened and plain differ by 2 ms on
  create+start for crun/none (153 versus 151 ms), inside the round-to-round drift.
- **CLI `exec` is slower than `start`**: 165 to 168 ms for `exec true` into an idle
  container, more than creating and starting one (151 to 153 ms).

### 1a. Storage driver and transient store (isolated stores, crun, network none)

Same cycle, same notation. "native/persistent" is the control for the default store
and matches it (create+start 152 versus 153 ms).

| cell | `run --rm true` | `create` | `start` | create+start | `exec true` (first) | `rm -f` (running) | load1 range |
|---|---:|---:|---:|---:|---:|---:|---|
| native/persistent/crun/none/plain | 172 / 192 / 211 | 56 / 58 / 60 | 94 / 101 / 135 | 150 / 158 / 190 | 164 / 178 / 187 | 117 / 129 / 153 | 1.79-2.62 |
| native/persistent/crun/none/hardened | 174 / 184 / 209 | 57 / 59 / 61 | 96 / 104 / 135 | 152 / 162 / 192 | 163 / 177 / 205 | 117 / 130 / 151 | 1.63-2.04 |
| native/transient/crun/none/plain | 133 / 141 / 148 | 37 / 39 / 50 | 78 / 87 / 96 | 116 / 125 / 144 | 90 / 121 / 127 | 59 / 64 / 75 | 1.58-1.78 |
| native/transient/crun/none/hardened | 135 / 143 / 147 | 38 / 40 / 41 | 78 / 86 / 89 | 116 / 124 / 128 | 90 / 120 / 126 | 60 / 65 / 77 | 1.52-1.69 |
| fuse/persistent/crun/none/plain | 177 / 214 / 240 | 56 / 58 / 59 | 98 / 107 / 143 | 154 / 163 / 197 | 146 / 150 / 179 | 107 / 117 / 124 | 1.43-1.72 |
| fuse/persistent/crun/none/hardened | 182 / 218 / 229 | 57 / 59 / 89 | 101 / 134 / 138 | 158 / 190 / 196 | 146 / 153 / 156 | 107 / 114 / 120 | 1.40-2.13 |
| fuse/transient/crun/none/plain | 142 / 152 / 158 | 38 / 39 / 44 | 85 / 95 / 106 | 122 / 133 / 143 | 89 / 120 / 129 | 61 / 66 / 73 | 1.60-2.34 |
| fuse/transient/crun/none/hardened | 146 / 259 / 272 | 38 / 40 / 48 | 86 / 96 / 99 | 125 / 135 / 143 | 93 / 161 / 169 | 62 / 164 / 172 | 1.55-2.76 |

- **Transient store** cuts `create` from 57 to 38 ms, `start` from 96 to 78 ms,
  create+start from 152 to 116 ms (24%), `exec` from 163 to 90 ms and `rm -f` from 117 to
  60 ms. It is the largest single lever measured here.
- **fuse-overlayfs** costs 5 ms on `start` (101 versus 96 ms) for this tiny image. It was
  not measured under filesystem load. `exec` and `rm -f` came out 10 to 17 ms faster in
  the fuse-overlayfs persistent store than in the native one, in both rounds; this was
  not investigated.

### 1b. REST API versus CLI (hardened, crun, none, real agent as PID 1)

REST and CLI cycles were interleaved sample by sample, n=100 each, no errors.

| operation | REST p50 | REST p95 | REST p99 | REST max | CLI p50 | CLI p95 | CLI p99 | CLI max | CLI minus REST (p50) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `create` | 24.7 | 27.1 | 28.7 | 54.1 | 57.3 | 59.1 | 60.2 | 62.1 | 32.7 |
| `start` | 61.3 | 68.3 | 69.8 | 70.8 | 94.4 | 107.9 | 131.3 | 134.5 | 33.1 |
| `create_plus_start` | 85.8 | 94.1 | 95.7 | 117.9 | 151.4 | 164.5 | 189.1 | 192.3 | 65.5 |
| `start_return_to_agent_ready` | 0.7 | 0.8 | 0.9 | 0.9 | 0.9 | 1.1 | 1.1 | 1.1 | 0.2 |
| `create_to_agent_ready` | 86.6 | 94.9 | 96.4 | 118.7 | 152.2 | 165.4 | 190.0 | 193.1 | 65.7 |
| `podman_exec_true_first` | 76.2 | 82.0 | 89.3 | 104.2 | 148.7 | 178.0 | 184.1 | 212.3 | 72.5 |
| `podman_exec_true_again` | 75.2 | 81.0 | 84.2 | 84.9 | 147.2 | 177.0 | 185.4 | 212.2 | 72.0 |
| `rm_f_running` | 90.0 | 95.8 | 123.5 | 126.2 | 117.6 | 127.4 | 143.3 | 147.7 | 27.6 |

The CLI costs 27 to 33 ms more than the socket for `create`, `start` and `rm`, which is
the 26 ms floor from section 0, and 72 ms more for `exec`. The agent answers its health
check less than 1 ms after `start` returns, so "running" and "ready" are the same moment.

Full statistics for the REST path, including the agent's own RPCs:

| operation | n | mean | sd | min | p50 | p95 | p99 | max | p50 round 1 / round 2 | errors |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|
| `create` | 100 | 24.95 | 3.22 | 21.85 | 24.66 | 27.07 | 28.73 | 54.07 | 24.66 / 24.66 | 0 |
| `start` | 100 | 61.47 | 3.92 | 51.41 | 61.34 | 68.32 | 69.84 | 70.79 | 60.86 / 62.10 | 0 |
| `create_to_agent_ready` | 100 | 87.16 | 5.09 | 77.05 | 86.60 | 94.89 | 96.44 | 118.67 | 86.17 / 86.93 | 0 |
| `agent_init_rpc` | 100 | 0.49 | 0.05 | 0.38 | 0.48 | 0.57 | 0.58 | 0.63 | 0.48 / 0.49 | 0 |
| `agent_exec_true_first` | 100 | 1.50 | 0.08 | 1.32 | 1.49 | 1.62 | 1.70 | 1.91 | 1.49 / 1.49 | 0 |
| `agent_exec_true_again` | 100 | 1.23 | 0.07 | 1.10 | 1.22 | 1.33 | 1.40 | 1.62 | 1.22 / 1.22 | 0 |
| `podman_exec_true_first` | 100 | 76.50 | 4.84 | 68.42 | 76.23 | 82.01 | 89.26 | 104.21 | 76.23 / 76.17 | 0 |
| `rm_f_running` | 100 | 91.08 | 6.44 | 84.21 | 90.01 | 95.82 | 123.47 | 126.24 | 88.66 / 91.84 | 0 |

`agent_exec_true_first` is the claim's first command: connect to the agent socket,
`POST /v1/processes {"cmd":"true"}`, read the event stream to the exit event.

## 2. User-namespace mapping cost

Each repetition used a brand-new isolated store, so "first" is the first container ever
created from that image with that mapping. The table shows `create`; `start` samples are
in `results.json`.

| storage | image | mapping | first `create` in a fresh store, 3 repetitions | later `create` p50 (max), n=60 | later create+start p50 | store size after (MB) |
|---|---|---|---|---:|---:|---:|
| native | python:3.12-alpine | `host(default)` | 59 / 58 / 59 | 57.5 (60.7) | 151.3 | 60 |
| native | python:3.12-alpine | `keep-id` | 2241 / 2200 / 2210 | 58.5 (71.5) | 160.5 | 113 |
| native | python:3.12-alpine | `keep-id:uid=1000,gid=1000` | 2159 / 2205 / 2532 | 58.0 (66.6) | 159.0 | 113 |
| native | python:3.12-alpine | `nomap` | 2190 / 2179 / 2214 | 58.0 (61.0) | 153.5 | 113 |
| native | python:3.12-alpine | `auto` | 2190 / 2216 / 2196 | 79.2 (89.0) | 175.0 | 379 |
| native | alpine:3.20 | `host(default)` | 58 / 57 / 55 | 55.2 (67.0) | 149.2 | 9 |
| native | alpine:3.20 | `keep-id` | 221 / 228 / 215 | 56.4 (70.0) | 158.6 | 17 |
| native | alpine:3.20 | `keep-id:uid=1000,gid=1000` | 213 / 226 / 214 | 56.2 (59.4) | 156.2 | 17 |
| native | alpine:3.20 | `nomap` | 218 / 220 / 213 | 55.7 (66.0) | 151.1 | 17 |
| native | alpine:3.20 | `auto` | 228 / 251 / 249 | 77.4 (87.9) | 171.3 | 57 |
| fuse | python:3.12-alpine | `host(default)` | 59 / 57 / 58 | 57.6 (90.3) | 158.8 | 60 |
| fuse | python:3.12-alpine | `keep-id` | 58 / 58 / 60 | 57.6 (61.9) | 161.8 | 60 |
| fuse | python:3.12-alpine | `keep-id:uid=1000,gid=1000` | 61 / 61 / 59 | 57.9 (66.6) | 159.2 | 60 |
| fuse | python:3.12-alpine | `nomap` | 58 / 71 / 58 | 57.9 (62.3) | 159.0 | 60 |
| fuse | python:3.12-alpine | `auto` | 78 / 78 / 77 | 76.3 (101.3) | 178.3 | 60 |
| fuse | alpine:3.20 | `host(default)` | 57 / 56 / 57 | 56.1 (66.4) | 153.7 | 9 |
| fuse | alpine:3.20 | `keep-id` | 58 / 56 / 61 | 56.7 (92.2) | 159.7 | 9 |
| fuse | alpine:3.20 | `keep-id:uid=1000,gid=1000` | 56 / 58 / 58 | 56.2 (67.3) | 156.9 | 9 |
| fuse | alpine:3.20 | `nomap` | 57 / 59 / 54 | 56.1 (84.1) | 155.7 | 9 |
| fuse | alpine:3.20 | `auto` | 74 / 73 / 75 | 72.7 (205.1) | 173.6 | 9 |

`--userns=auto` when other `auto` containers are alive (each one needs a range nobody
holds, so each one is a "first use"):

| storage | image | `create` latency of 6 consecutive `--userns=auto` containers kept alive (ms), repetition 1 / 2 / 3 |
|---|---|---|
| native | python:3.12-alpine | 81, 2006, 2009, 2050, 2007, 2093 / 80, 2009, 2049, 2008, 2021, 2056 / 77, 1950, 2681, 2488, 2008, 2055 |
| native | alpine:3.20 | 78, 214, 215, 217, 224, 227 / 76, 219, 231, 226, 238, 216 / 74, 206, 222, 231, 214, 223 |
| fuse | python:3.12-alpine | 76, 77, 79, 75, 78, 78 / 76, 73, 78, 76, 78, 79 / 76, 78, 77, 76, 77, 90 |
| fuse | alpine:3.20 | 73, 75, 77, 73, 74, 73 / 72, 76, 77, 74, 73, 73 / 74, 76, 73, 74, 75, 79 |

- On **native overlay**, the first container under any mapping other than the default
  pays for an ID-shifted copy of the image: 2.16 to 2.53 s for the 57 MB Python image,
  213 to 251 ms for Alpine, and the store grows by one full image copy (60 to 113 MB).
  After that the cost is gone: 58 ms, the same as the default mapping.
- `keep-id`, `keep-id:uid=1000,gid=1000` and `nomap` cost the same.
- `--userns=auto` on native overlay pays the copy on **every** create once a previous
  `auto` container is alive (5 of 6 creates at about 2.0 s; the store reached 379 MB,
  six image copies on top of the 60 MB original). When each container was removed before the next was created,
  Podman reused the same range and the cost did not show.
- On **fuse-overlayfs** there is no first-use cost and no store growth for any mapping;
  `auto` costs a constant 19 ms extra on `create` (76 versus 57 ms).
- **Subordinate IDs.** With the default 65,536 IDs, the 65th `--userns=auto` container
  failed: `Error: creating container storage: not enough unused IDs in user namespace`
  (64 created, 1,024 IDs each).

## 3. Warm state: pause and unpause

Rootless `podman pause` and `unpause` work on this host (cgroup v2 freezer). n=100.

| operation | n | mean | sd | min | p50 | p95 | p99 | max | p50 round 1 / round 2 | errors |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|
| `cli_pause` | 100 | 41.55 | 1.21 | 39.21 | 41.27 | 43.35 | 44.10 | 45.74 | 40.66 / 42.44 | 0 |
| `cli_unpause` | 100 | 41.61 | 1.14 | 38.73 | 41.70 | 43.49 | 43.90 | 44.55 | 41.04 / 42.23 | 0 |
| `api_pause` | 100 | 10.87 | 0.85 | 9.00 | 10.74 | 12.26 | 12.94 | 13.79 | 10.26 / 11.48 | 0 |
| `api_unpause` | 100 | 10.62 | 1.10 | 9.23 | 10.43 | 11.72 | 12.88 | 19.65 | 10.15 / 10.79 | 0 |
| `cgroup_freeze_write_until_frozen` | 100 | 0.48 | 0.69 | 0.13 | 0.16 | 2.10 | 2.63 | 2.87 | 0.16 / 0.17 | 0 |
| `cgroup_thaw_write_until_thawed` | 100 | 0.11 | 0.01 | 0.09 | 0.11 | 0.13 | 0.14 | 0.15 | 0.11 / 0.11 | 0 |
| `cli_exec_true_idle_running` | 100 | 165.05 | 12.76 | 143.66 | 165.75 | 183.78 | 189.22 | 190.56 | 160.63 / 175.88 | 0 |
| `api_exec_true_idle_running` | 100 | 83.47 | 5.87 | 71.82 | 83.56 | 94.56 | 95.42 | 98.36 | 78.16 / 86.61 | 0 |
| `agent_exec_true_idle_running` | 100 | 1.29 | 0.14 | 1.16 | 1.26 | 1.50 | 1.87 | 2.20 | 1.28 / 1.25 | 0 |
| `api_unpause_then_agent_exec_true` | 100 | 11.84 | 0.62 | 10.66 | 11.85 | 12.73 | 13.10 | 14.15 | 11.53 / 12.17 | 0 |

- Pause and unpause each cost 10.7 and 10.4 ms p50 over the socket, 41 ms through the CLI.
  The kernel primitive itself, `cgroup.freeze` written by the rootless user, is 0.16 ms
  to frozen and 0.11 ms to thawed.
- A paused sandbox costs 11.9 ms to its first agent command (unpause plus RPC) against
  1.26 ms for an idle running one.
- `podman exec` into a paused container fails: `Error: can only create exec sessions on
  running containers: container state improper`.
- Pausing frees nothing: the sandbox's `memory.current` was 7,356,416 bytes running and
  7,249,920 bytes one second after pausing.

## 4. Density

Idle sandboxes with the hardened spec and the real static `sbx-agent serve` as PID 1,
created one at a time through the REST API in steps of 20. Two ramps, 3 s settle before
each reading. Both stopped at the harness's safety margin, 20 keys short of the
keyring quota.

Ramp 1:

| sandboxes | MemAvailable MB | host delta per sandbox kB | cgroup scope `memory.current` kB | conmon scope `memory.current` kB | conmon PSS / RSS kB | agent PSS / RSS kB | procs / threads / fds per sandbox | inotify | keys | create p50 | start p50 | create-to-ready p50 / p95 / max | load1 |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 0 | 9157 | - | - | - | - | - | - | 42 | 4/200 | - | - | - | 1.77 |
| 20 | 9106 | 2586 | 1605 | 421 | 338 / 2116 | 1437 / 7104 | 2.00 / 8.05 / 23.05 | 62 | 24/200 | 23.8 | 62.4 | 87.4 / 93.6 / 99.2 | 1.71 |
| 40 | 9045 | 2857 | 1578 | 414 | 337 / 2107 | 1268 / 7084 | 2.00 / 8.00 / 23.02 | 82 | 44/200 | 24.6 | 59.6 | 85.1 / 95.7 / 98.8 | 1.65 |
| 60 | 8989 | 2868 | 1568 | 411 | 337 / 2104 | 1217 / 7083 | 2.00 / 8.05 / 23.03 | 102 | 64/200 | 25.6 | 63.0 | 88.9 / 184.2 / 220.0 | 1.47 |
| 80 | 8927 | 2937 | 1558 | 409 | 336 / 2100 | 1186 / 7077 | 2.00 / 8.03 / 23.01 | 122 | 84/200 | 26.9 | 66.1 | 94.5 / 119.1 / 123.9 | 1.35 |
| 100 | 8855 | 3090 | 1560 | 408 | 336 / 2103 | 1175 / 7081 | 2.00 / 7.98 / 23.01 | 142 | 104/200 | 28.8 | 67.0 | 97.9 / 106.1 / 123.3 | 1.24 |
| 120 | 8784 | 3182 | 1559 | 408 | 336 / 2101 | 1165 / 7080 | 2.00 / 7.99 / 23.02 | 162 | 124/200 | 28.9 | 65.1 | 94.7 / 114.2 / 135.0 | 1.22 |
| 140 | 8732 | 3104 | 1565 | 411 | 335 / 2102 | 1158 / 7081 | 2.00 / 7.99 / 23.01 | 182 | 144/200 | 29.7 | 66.0 | 97.8 / 105.7 / 142.5 | 1.29 |
| 160 | 8729 | 2736 | 1558 | 407 | 335 / 2101 | 1150 / 7078 | 2.00 / 8.00 / 23.01 | 202 | 164/200 | 30.6 | 69.0 | 100.8 / 130.7 / 132.6 | 1.16 |
| 176 | 8713 | 2582 | 1559 | 408 | 335 / 2100 | 1144 / 7076 | 2.00 / 8.01 / 23.01 | 218 | 180/200 | 31.2 | 69.8 | 102.3 / 130.8 / 132.4 | 1.07 |

Ramp 2:

| sandboxes | MemAvailable MB | host delta per sandbox kB | cgroup scope `memory.current` kB | conmon scope `memory.current` kB | conmon PSS / RSS kB | agent PSS / RSS kB | procs / threads / fds per sandbox | inotify | keys | create p50 | start p50 | create-to-ready p50 / p95 / max | load1 |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 0 | 9118 | - | - | - | - | - | - | 42 | 4/200 | - | - | - | 1.98 |
| 20 | 9059 | 3028 | 1579 | 415 | 337 / 2109 | 1404 / 7071 | 2.00 / 7.90 / 23.10 | 62 | 24/200 | 23.7 | 61.0 | 85.9 / 93.9 / 94.4 | 1.9 |
| 40 | 9011 | 2740 | 1566 | 406 | 336 / 2104 | 1258 / 7074 | 2.00 / 7.92 / 23.05 | 82 | 44/200 | 24.3 | 62.9 | 88.4 / 100.2 / 137.1 | 1.83 |
| 60 | 8970 | 2525 | 1548 | 407 | 336 / 2104 | 1203 / 7068 | 2.00 / 7.88 / 23.02 | 102 | 64/200 | 25.3 | 65.3 | 91.8 / 103.2 / 132.9 | 1.76 |
| 80 | 8916 | 2586 | 1559 | 411 | 335 / 2102 | 1177 / 7067 | 2.00 / 7.92 / 23.01 | 122 | 84/200 | 25.8 | 62.8 | 90.5 / 97.9 / 128.5 | 1.56 |
| 100 | 8849 | 2762 | 1553 | 410 | 334 / 2103 | 1163 / 7068 | 2.00 / 7.95 / 23.02 | 142 | 104/200 | 27.0 | 65.2 | 93.5 / 97.1 / 99.1 | 1.44 |
| 120 | 8793 | 2776 | 1552 | 407 | 335 / 2102 | 1155 / 7070 | 2.00 / 7.95 / 23.01 | 162 | 124/200 | 27.8 | 65.5 | 94.6 / 133.4 / 146.6 | 1.32 |
| 140 | 8790 | 2404 | 1550 | 408 | 335 / 2104 | 1147 / 7070 | 2.00 / 7.99 / 23.01 | 182 | 144/200 | 28.0 | 65.6 | 94.9 / 102.9 / 127.1 | 1.3 |
| 160 | 8767 | 2246 | 1550 | 406 | 335 / 2104 | 1142 / 7070 | 2.00 / 7.96 / 23.01 | 202 | 164/200 | 29.0 | 65.5 | 95.6 / 105.7 / 141.8 | 1.35 |
| 176 | 8684 | 2528 | 1550 | 406 | 334 / 2103 | 1139 / 7071 | 2.00 / 7.96 / 23.01 | 218 | 180/200 | 30.1 | 67.2 | 98.7 / 110.2 / 130.4 | 1.3 |

- **Memory.** The regression slope of `MemAvailable` over the ramp is 2,793 kB per
  sandbox in ramp 1 and 2,402 kB in ramp 2; the end-point deltas are 2,582 and 2,528 kB.
  The cgroups account for 1.56 MB (sandbox scope, almost all of it the Go agent) plus
  0.41 MB (conmon scope). The remaining 0.5 to 0.8 MB is kernel memory that no sandbox
  cgroup is charged for. Where the host-level growth sat at the peak, kB per sandbox,
  ramp 1: `{"MemFree": -2853.9, "Cached": 59.8, "AnonPages": 1472.3, "Shmem": 40.2, "Slab": 994.5, "SUnreclaim": 773.3, "SReclaimable": 221.2, "KernelStack": 127.8, "PageTables": 179.9, "Percpu": 41.5}`; ramp 2: `{"MemFree": -2758.8, "Cached": 75.8, "AnonPages": 1420.7, "Shmem": 40.2, "Slab": 745.3, "SUnreclaim": 564.8, "SReclaimable": 180.5, "KernelStack": 127.3, "PageTables": 179.2, "Percpu": 34.8}`. `MemAvailable` moved by other
  processes too (after teardown it was 54 MB and 89 MB lower than before ramps 1 and 2), so
  treat these as plus or minus 15%.
- **Per sandbox**: 2 host processes (conmon and the agent), 8 threads, 23 file
  descriptors (16 of them conmon's), 1 inotify instance, 1 kernel key, 1 Podman lock,
  7 tasks in the pids cgroup.
- **Create latency drift**: create-to-ready p50 went from 87.4 ms (first 20) to 102.3 ms
  (at 176) in ramp 1 and from 85.9 to 98.7 ms in ramp 2. No create or start failed in
  either ramp.
- **Teardown** of 176 running sandboxes at concurrency 4: 13.3 s and 12.9 s, that is 13.2
  and 13.6 removals per second, no errors. Keys and inotify instances returned to the
  baseline (4 and 42).
- **The API service** grew from 56 to 60 MB RSS over a ramp.

Request-path latency with 176 sandboxes alive (n=100 over both ramps):

| operation | n | mean | sd | min | p50 | p95 | p99 | max | p50 round 1 / round 2 | errors |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|
| `agent_exec_true` | 100 | 1.29 | 0.11 | 1.14 | 1.27 | 1.42 | 1.76 | 1.76 | 1.28 / 1.27 | 0 |
| `api_podman_exec_true` | 100 | 103.80 | 8.56 | 74.48 | 103.30 | 113.94 | 121.43 | 150.58 | 100.77 / 106.17 | 0 |
| `cli_podman_exec_true` | 100 | 179.44 | 11.65 | 131.64 | 180.78 | 194.61 | 198.62 | 214.09 | 177.71 / 184.00 | 0 |
| `api_list_containers` | 100 | 30.93 | 1.89 | 27.24 | 30.90 | 34.62 | 35.33 | 35.59 | 31.01 / 30.60 | 0 |
| `cli_podman_ps` | 100 | 75.33 | 1.59 | 72.54 | 75.04 | 77.79 | 78.86 | 83.62 | 75.29 / 74.93 | 0 |

The agent RPC is unchanged by density (1.27 ms). `podman exec` over the socket rose from
76 to 103 ms, and listing containers costs 31 ms over the socket and 75 ms with
`podman ps`.

### The first limit: session keyrings

Each running container takes one key from the host user's quota
(`/proc/key-users`), whatever its user-namespace mapping. The quota is
`kernel.keys.maxkeys=200` for a non-root user and this desktop session already held 4.
After ramp 2, with `--push-to-limit`, the harness started minimal containers until the
first error: 20 more started (keys 200/200), and the next one failed with

```
Error: OCI runtime error: crun: create keyring `6146fe21...`: Disk quota exceeded
```

So the ceiling on this host is **196 running containers per user**, at about 0.5 GB of
memory. Raising it needs root (`sysctl kernel.keys.maxkeys`). Without root,
`keyring = false` in `containers.conf` avoids it: with that setting, 3 containers added 0
keys instead of 3. That was verified for the CLI through `CONTAINERS_CONF_OVERRIDE`
only; the API service reads its configuration at start and was not tested with it.

Limits not reached: inotify (218 of 512 `max_user_instances` at the peak, 42 of them the
desktop's; projected to run out near 470 sandboxes), Podman locks (1,872 of 2,048 free;
runs out at 2,048 containers), pids (user slice `pids.max` 26,829 against about 8 tasks
per sandbox), memory (8.7 GB still available at the peak).

### 4a. Parallel create throughput and teardown rate

One create is create + start + agent ready with the hardened spec; then all containers
are removed at the same concurrency. Run A and B: 32 creates per row; run C: 64.

| run | transport | concurrency | round | ok | creates/s | per-create p50 ms | p95 ms | max ms | removals/s | create errors | load1 start-end |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| A | cli | 1 | 1 | 32/32 | 7.83 | 125 | 158 | 187 | 8.4 | 0 | 1.35-1.3 |
| A | cli | 1 | 2 | 32/32 | 7.7 | 125 | 159 | 162 | 8.75 | 0 | 1.75-1.96 |
| A | cli | 2 | 1 | 32/32 | 13.52 | 147 | 158 | 168 | 10.65 | 0 | 1.3-1.43 |
| A | cli | 2 | 2 | 32/32 | 13.2 | 150 | 162 | 163 | 10.85 | 0 | 1.96-2.04 |
| A | cli | 4 | 1 | 32/32 | 16.38 | 231 | 296 | 315 | 11.14 | 0 | 1.43-1.56 |
| A | cli | 4 | 2 | 32/32 | 16.08 | 229 | 306 | 526 | 11.08 | 0 | 2.04-2.28 |
| A | cli | 8 | 1 | 32/32 | 16.47 | 384 | 758 | 975 | 8.99 | 0 | 1.56-1.75 |
| A | cli | 8 | 2 | 32/32 | 17.25 | 369 | 860 | 1125 | 9.78 | 0 | 2.28-2.74 |
| A | rest-api | 1 | 1 | 32/32 | 11.55 | 85 | 95 | 95 | 11.8 | 0 | 1.51-1.47 |
| A | rest-api | 1 | 2 | 0/32 | - | - | - | - | - | 32 | 1.75-1.75 |
| A | rest-api | 2 | 1 | 32/32 | 17.6 | 109 | 132 | 134 | 13.83 | 0 | 1.47-1.35 |
| A | rest-api | 2 | 2 | 0/32 | - | - | - | - | - | 32 | 1.75-1.75 |
| A | rest-api | 4 | 1 | 0/32 | - | - | - | - | - | 32 | 1.35-1.35 |
| A | rest-api | 4 | 2 | 0/32 | - | - | - | - | - | 32 | 1.75-1.75 |
| A | rest-api | 8 | 1 | 0/32 | - | - | - | - | - | 32 | 1.35-1.35 |
| A | rest-api | 8 | 2 | 0/32 | - | - | - | - | - | 32 | 1.75-1.75 |
| B | cli | 1 | 1 | 32/32 | 4.83 | 188 | 395 | 509 | 5.62 | 0 | 2.1-2.33 |
| B | cli | 1 | 2 | 32/32 | 7.8 | 126 | 137 | 179 | 8.1 | 0 | 5.14-4.66 |
| B | cli | 2 | 1 | 32/32 | 12.77 | 154 | 179 | 186 | 10.83 | 0 | 2.33-2.62 |
| B | cli | 2 | 2 | 32/32 | 12.97 | 152 | 170 | 173 | 8.45 | 0 | 4.66-4.44 |
| B | cli | 4 | 1 | 32/32 | 15.77 | 219 | 376 | 390 | 8.97 | 0 | 2.62-3.54 |
| B | cli | 4 | 2 | 32/32 | 10.21 | 315 | 744 | 1133 | 9.22 | 0 | 4.44-4.49 |
| B | cli | 8 | 1 | 32/32 | 15.06 | 436 | 912 | 1316 | 8.18 | 0 | 3.54-3.81 |
| B | cli | 8 | 2 | 32/32 | 11.75 | 508 | 1311 | 1754 | 6.23 | 0 | 4.49-4.86 |
| B | rest-api | 1 | 1 | 32/32 | 10.8 | 88 | 130 | 146 | 11.75 | 0 | 1.7-1.8 |
| B | rest-api | 1 | 2 | 32/32 | 10.62 | 89 | 114 | 174 | 12.19 | 0 | 3.81-3.61 |
| B | rest-api | 2 | 1 | 32/32 | 17.5 | 113 | 137 | 147 | 11.39 | 0 | 1.8-1.9 |
| B | rest-api | 2 | 2 | 32/32 | 16.76 | 116 | 141 | 157 | 14.01 | 0 | 3.61-3.61 |
| B | rest-api | 4 | 1 | 32/32 | 10.05 | 269 | 847 | 1060 | 12.13 | 0 | 1.9-1.85 |
| B | rest-api | 4 | 2 | 32/32 | 15.22 | 206 | 535 | 1276 | 12.38 | 0 | 3.61-4.28 |
| B | rest-api | 8 | 1 | 32/32 | 14.76 | 269 | 1691 | 2030 | 10.74 | 0 | 1.85-2.1 |
| B | rest-api | 8 | 2 | 32/32 | 20.39 | 263 | 957 | 1113 | 11.71 | 0 | 4.28-5.14 |
| C | cli | 4 | 1 | 64/64 | 15.91 | 244 | 324 | 361 | 10.89 | 0 | 3.64-3.62 |
| C | cli | 4 | 2 | 64/64 | 14.79 | 253 | 374 | 482 | 8.79 | 0 | 3.89-3.91 |
| C | cli | 4 | 3 | 64/64 | 15.19 | 235 | 412 | 653 | 7.19 | 0 | 4.41-5.33 |
| C | cli | 8 | 1 | 64/64 | 17.33 | 383 | 804 | 2236 | 8.01 | 0 | 3.62-3.75 |
| C | cli | 8 | 2 | 64/64 | 13.67 | 517 | 959 | 1270 | 7.87 | 0 | 3.91-4.83 |
| C | cli | 8 | 3 | 64/64 | 11.17 | 510 | 1855 | 2310 | 7.21 | 0 | 5.33-4.65 |
| C | rest-api | 4 | 1 | 63/64 | 19.13 | 177 | 364 | 491 | 13.38 | 1 | 4.0-3.84 |
| C | rest-api | 4 | 2 | 64/64 | 18.37 | 191 | 365 | 785 | 13.63 | 0 | 3.75-3.71 |
| C | rest-api | 4 | 3 | 64/64 | 19.29 | 172 | 398 | 817 | 12.83 | 0 | 4.83-4.54 |
| C | rest-api | 8 | 1 | 64/64 | 20.14 | 225 | 1265 | 1805 | 13.28 | 0 | 3.84-3.64 |
| C | rest-api | 8 | 2 | 64/64 | 17.04 | 295 | 1107 | 2301 | 8.92 | 0 | 3.71-3.89 |
| C | rest-api | 8 | 3 | 63/64 | 18.82 | 291 | 950 | 1223 | 12.86 | 1 | 4.54-4.41 |

- Serial: 10.6 to 11.6 creates/s over the socket, 7.7 to 7.8 through the CLI (4.8 in the
  one row disturbed by the other engineer's tests).
- Concurrency 2 gives 16.8 to 17.6 creates/s over the socket. Concurrency 4 and 8 give
  10 to 20, with per-create p95 between 0.36 and 1.7 s: four hardware threads are
  saturated and the rest is queueing.
- Removal over the socket: 10.7 to 14 per second at any concurrency, 8.9 in one row.
- **One intermittent failure.** 2 of the 640 REST creates in runs B and C failed at
  `start`, both at concurrency 4 or 8, with
  `crun: write to /proc/sys/net/ipv4/ping_group_range: Invalid argument: OCI runtime
  error`. It also appeared once in a smoke test at concurrency 2. It never appeared in
  the roughly 490 serial REST creates of run A or in any CLI create. A retry of the failed
  sandbox is enough, but the pool must expect it.

## 5. Workload overhead (crun)

Timed inside the sandbox by a driver script carried in with one `podman exec`, so no
Podman cost is in these numbers. python:3.12-alpine, 3 rounds, one discarded warm-up call
per workload. Each entry is **p50 ± sd [min-max]**.

| workload (n) | plain (overlay workdir) | plain + tmpfs workdir | hardened, Podman default seccomp | hardened, product seccomp profile | hardened, product seccomp + PYTHONPYCACHEPREFIX=/tmp/pycache | hardened (product) / plain |
|---|---:|---:|---:|---:|---:|---:|
| `python_start_pass` (90) | 16.8 ± 1.0 [16.6-22.5] | 16.8 ± 0.6 [16.6-21.5] | 24.7 ± 1.5 [24.5-33.6] | 24.7 ± 1.3 [24.4-34.8] | 17.1 ± 0.8 [16.8-24.4] | 1.47x |
| `python_import_30_stdlib_modules` (45) | 173.0 ± 3.1 [171.6-183.5] | 172.7 ± 3.2 [171.4-184.2] | 1169.5 ± 14.2 [1143.3-1188.2] | 1152.6 ± 8.6 [1143.6-1179.8] | 173.8 ± 3.3 [171.7-184.0] | 6.66x |
| `cpu_loop_2M_iterations` (15) | 257.0 ± 6.1 [251.7-270.2] | 257.0 ± 4.9 [251.9-266.7] | 253.8 ± 6.2 [251.0-268.2] | 255.5 ± 26.0 [251.4-314.3] | 256.1 ± 8.7 [251.5-274.8] | 0.99x |
| `small_files_2000x1KiB_write_read_unlink` (15) | 238.7 ± 2.4 [237.4-244.6] | 110.5 ± 2.6 [109.5-118.4] | 110.2 ± 8.7 [109.2-143.2] | 111.3 ± 2.1 [109.9-117.1] | 111.2 ± 3.4 [108.9-120.3] | 0.47x |
| `fork_shell_300x_bin_true` (15) | 155.3 ± 2.6 [151.0-160.8] | 154.4 ± 2.4 [152.5-161.4] | 172.6 ± 1.4 [170.1-175.6] | 170.3 ± 2.4 [167.5-176.6] | 172.0 ± 1.7 [168.8-174.7] | 1.10x |
| `python_subprocess_100x_true` (15) | 47.8 ± 2.9 [45.7-57.5] | 48.5 ± 2.3 [46.7-56.4] | 57.0 ± 3.7 [50.4-63.4] | 57.5 ± 3.7 [50.4-62.8] | 57.4 ± 3.4 [53.8-66.7] | 1.20x |

- **Read-only root and Python bytecode.** In the hardened sandbox the interpreter cannot
  write `__pycache__`, and the official image ships no `.pyc` files, so every process
  recompiles what it imports: interpreter start 24.7 versus 16.8 ms, 30 stdlib imports
  1153 versus 173 ms. With `PYTHONPYCACHEPREFIX=/tmp/pycache` the hardened sandbox
  matches plain (17.1 and 173.8 ms). The alternative is to precompile in the template
  image.
- **Seccomp profile.** The product profile and Podman's default profile perform the
  same in every workload.
- **Process creation** is 10% slower hardened for the shell loop (170 versus 155 ms for
  300 fork+exec) and 20% slower for Python `subprocess` (57.5 versus 47.8 ms for 100).
  The cause was not isolated; the 1-CPU quota, the user-namespace mapping and
  no-new-privileges all differ between the two.
- **CPU-bound code** is unaffected (255.5 versus 257.0 ms).
- **Small files** are twice as fast on the tmpfs workdir as on the overlay root (111
  versus 239 ms), which is a property of tmpfs, as the "plain + tmpfs" column shows.

This suite takes `--runtime`; it was run under crun only. gVisor could not be measured:
`runsc` is not installed and installing it was out of scope here.

## 6. Host capability probes

From the probes run (load1 0.31). Identical outcomes in run A except the re-attach probe
(see "Runs").

| question | result | evidence |
|---|---|---|
| Can the rootless user write `cgroup.kill` for a Podman-created cgroup, and does it kill a fork-heavy tree? | **works** | `cgroup.kill` in the container's leaf cgroup is owned by uid 1000, mode 0200. Writing `1` emptied a cgroup of 62 tasks (60 sleeps and a tight fork loop) in 4.1 ms; Podman reported `exited 137` 119 ms after the write |
| Does `--cgroup-conf memory.oom.group=1` take effect? | **works** | The leaf cgroup's `memory.oom.group` reads 1 and an OOM in one process killed the whole container (exit 137). Control with 0: only the hog died, the shell went on and exited 42. `State.OOMKilled` stayed `false` in both cases |
| Does `--storage-opt size=` work rootless here? | **fails** | `Error: storage option overlay.size and overlay.inodes only supported for backingFS XFS. Found extfs` |
| Can a detached exec session be re-attached? | **fails** | `podman exec` has no attach subcommand. Over the API, a second `POST /exec/{id}/start` on a detached, running session returns HTTP 500: `can only start created exec sessions, while container ... session ... state is "running": exec session state improper` |
| Are cpu, memory and pids limits applied? | **works** | Leaf cgroup: `cpu.max=100000 100000`, `memory.max=268435456`, `memory.high=241590272`, `memory.swap.max=0`, `memory.oom.group=1`, `pids.max=128`. Note that `podman info` lists only `memory` and `pids` as controllers although `cpu` is delegated and enforced |
| Does every container use one key of the user's quota? | **yes** | 3 containers, 3 keys; 0 with `keyring=false` |
| Does rootless checkpoint work? | **fails** | `Error: checkpointing a container requires root` |

### Seccomp: what the profiles do

The errno seen from inside a python:3.12-alpine container (ctypes probe); `OK` means the
call succeeded. Host sysctls that also gate some of these: `{"kernel.unprivileged_bpf_disabled": "2", "kernel.io_uring_disabled": "0", "vm.unprivileged_userfaultfd": "0", "kernel.apparmor_restrict_unprivileged_userns": "1", "kernel.unprivileged_userns_clone": "1", "kernel.perf_event_paranoid": "4", "kernel.yama.ptrace_scope": "1", "user.max_user_namespaces": "40650"}`.

| call | default seccomp, default caps, root | default seccomp, cap-drop all, nnp, uid 1000 | product profile (hardened spec) | seccomp=unconfined (control) |
|---|---|---|---|---|
| `unshare(CLONE_NEWUSER)` | OK | OK | ENOSYS | OK |
| `unshare(CLONE_NEWNS)` | EPERM | EPERM | ENOSYS | EPERM |
| `unshare(CLONE_NEWNET)` | EPERM | EPERM | ENOSYS | EPERM |
| `bpf(BPF_MAP_CREATE)` | EPERM | EPERM | EPERM | EINVAL |
| `io_uring_setup(1)` | ENOSYS | ENOSYS | ENOSYS | OK |
| `userfaultfd(0)` | EPERM | EPERM | EPERM | EPERM |
| `keyctl(GET_KEYRING_ID, session)` | OK | OK | EPERM | OK |
| `add_key(user)` | ENOSYS | ENOSYS | EPERM | OK |
| `ptrace(PTRACE_TRACEME)` | OK | OK | EPERM | OK |
| `mount(tmpfs on /tmp)` | EPERM | EPERM | EPERM | EPERM |
| `perf_event_open` | EPERM | EPERM | EPERM | EACCES |
| `clone3(NULL)` | EINVAL | EINVAL | ENOSYS | EINVAL |
| `personality(query)` | OK | OK | OK | OK |
| `open_by_handle_at` | EPERM | EPERM | EPERM | EFAULT |
| `kexec_load` | EPERM | EPERM | EPERM | EPERM |
| `init_module` | EPERM | EPERM | EPERM | EPERM |
| `setns(-1)` | EBADF | EBADF | EPERM | EBADF |
| `pivot_root` | EPERM | EPERM | EPERM | EPERM |
| `process_vm_readv(pid 1)` | OK | OK | EPERM | OK |
| `socket(AF_ALG, SOCK_SEQPACKET)` | OK | OK | ENOSYS | OK |
| `socket(AF_NETLINK, SOCK_RAW)` | OK | OK | ENOSYS | OK |
| `socket(AF_PACKET, SOCK_RAW)` | EPERM | EPERM | ENOSYS | EPERM |
| `socket(AF_INET, SOCK_RAW, ICMP)` | EPERM | EPERM | EPERM | EPERM |
| `socket(AF_VSOCK, SOCK_STREAM)` | OK | OK | ENOSYS | OK |
| `socket(AF_UNIX, SOCK_STREAM)` | OK | OK | OK | OK |

- **`unshare(CLONE_NEWUSER)` succeeds under the installed default profile**, even with
  every capability dropped, no-new-privileges and uid 1000, and even though
  `kernel.apparmor_restrict_unprivileged_userns=1` (rootless Podman containers are not
  AppArmor-confined here). The default profile lists `unshare` with no condition. Only
  the product profile blocks it (ENOSYS, via an argument filter).
- **`bpf`** returns EPERM under both profiles (the default profile allows it only with
  CAP_SYS_ADMIN).
- **`io_uring_setup`** is not listed in the installed default profile and returns
  ENOSYS under it; unconfined, it succeeds.
- **AF_ALG sockets** can be created under the default profile. The product profile
  allows only AF_UNIX, AF_INET and AF_INET6 and returns ENOSYS for the rest (AF_ALG,
  AF_NETLINK, AF_VSOCK, AF_PACKET).
- The default profile also allows `keyctl`, `ptrace`, `process_vm_readv` and `clone3`;
  the product profile denies all four.

## What this means for the design

### Is exec into an idle sandbox fast enough to be the request path?

Through the agent, yes: 1.5 ms p50 and 1.7 ms p99 for the first command of a claim, plus
0.5 ms for `/v1/init`, unchanged with 176 sandboxes on the host. Through Podman, no:
`podman exec` is 76 ms over the socket (103 ms on a full host) and 150 to 170 ms through
the CLI, and a detached exec cannot be re-attached. This confirms the plan: the agent is
mandatory, and Podman is only on the create and destroy paths.

Keep the warm state idle-running, not paused. Unpause adds 10.4 ms to the first command
and pausing saves no memory.

### Pool size from Little's law

Pool in flight = claim rate x create-to-ready time. Measured create-to-ready over the
socket: 87.2 ms mean and 96.4 ms p99 serial on an empty host, about 100 ms p50 and
131 ms p95 with 176 sandboxes present.

| claims/s | creation time used | sandboxes being created on average | what it means here |
|---:|---|---:|---|
| 1 | 87 ms mean (96 ms p99) | 0.09 (0.10) | One refill worker is 9% busy. A pool of 1 covers the arithmetic; the pool size is set entirely by the burst reserve |
| 5 | 87 ms mean (131 ms p95 on a full host) | 0.44 (0.66) | Creation takes 44% of one serial worker and removal of the used sandboxes (90 ms each) another 45%, so run two workers. Still under 1 in flight |
| 20 | 225 to 295 ms p50, 0.95 to 1.7 s p95 at concurrency 8 | 4.5 to 5.9 (19 to 34 at p95) | Not sustainable on this host: 20/s is the top of the create throughput measured at concurrency 8 (14.8 to 20.4/s) with nothing left for the 20 removals/s it also needs (9 to 14/s measured). A burst of 20/s has to come out of stock |

So on this class of machine the Little's-law term is below 1 up to several claims per
second and the pool is sized by burst: stock = (peak claim rate - sustained refill rate
of roughly 10/s with removals) x burst duration. The 196-container ceiling bounds
stock plus running sandboxes per host user until the keyring quota is dealt with.

Each claim costs a create (87 ms) and a destroy (90 ms) of Podman work, about 0.18 s of
single-threaded service time, so one refill worker tops out near 5.5 full recycles per
second.

### Which user-namespace mapping?

A fixed mapping per pool, as `spec.go` already does with `keep-id`. Every fixed mapping
costs the same: nothing per sandbox, and one image copy per image per host (2.2 s and
53 MB for the Python image). Pre-warm it: create and remove one throwaway container when
a template image is pulled, so the first pool refill does not take 2 s.

`nomap` has the same cost as `keep-id` and stronger isolation (an escaped process owns
nothing on the host). The benchmark gives no performance reason against it; the reason
is the run-directory ownership described in `config.go`.

`--userns=auto` should not be used on native overlay: about 2 s and 53 MB of disk per
create, and 64 sandboxes per host user. On fuse-overlayfs it costs only 19 ms per create,
but the 64-container limit stays unless root assigns a larger subuid range, and
fuse-overlayfs was not measured under I/O load. Per-sandbox UID ranges remain a root-side
provisioning decision, not something the default host gives.

### Idle memory and sandboxes per GB

About 2.4 to 2.8 MB per idle sandbox, so 370 to 430 idle sandboxes per GB. Of that,
1.56 MB is charged to the sandbox's own cgroup (the idle Go agent), 0.41 MB to conmon,
and the rest is kernel memory outside both. This is the idle floor with a static agent
and an Alpine image. A sandbox in use is bounded by its 256 MiB limit instead, and tmpfs
content counts against it.

On this host memory would allow roughly 3,000 idle sandboxes (extrapolated) and the
keyring quota allowed 196.

### Changes this suggests

1. Set `PYTHONPYCACHEPREFIX` (for example `/tmp/pycache`) in the sandbox environment, or
   precompile in template images. Without it, Python start is 47% slower and imports are
   6.7 times slower in the hardened sandbox.
2. Add `kernel.keys.maxkeys` (and `maxbytes`) to the host preflight, next to
   `fs.inotify.max_user_instances`, or run with `keyring = false`.
3. Do not trust `podman info` for delegated controllers; read
   `cgroup.subtree_control` of the user service, as the probe does.
4. Retry a failed `start` once: the `ping_group_range` error occurs under concurrent
   starts at roughly 3 per 1,000.
5. Consider a transient store for the pool's Podman: 24% off create+start. Its
   consequence (container state does not survive a reboot) fits sandboxes that are never
   reused.
6. Use crun; runc is 85 ms slower to start and 120 ms slower to remove.
7. `rm` with `timeout=0`; `cgroup.kill` works rootless for a faster hard kill (4 ms).

### Unknowns from the plan

| unknown | status after this run |
|---|---|
| Rootless latency for create, start, exec, rm | **Settled for this host and Podman 4.9.3**: sections 1, 1a, 1b |
| Cost of `--userns=auto` versus a fixed mapping | **Settled**: section 2. Go for a fixed mapping |
| Density and the first limit hit | **Partly settled**: first limit is the keyring quota at 196; inotify, locks and memory are projections beyond that because the ramp could not pass it without root |
| Store lock contention under parallel creates | **Partly settled**: no lock errors; throughput flattens at 17 to 20/s on 4 threads. Whether that is CPU or lock contention was not separated, and a host with more cores is needed to tell |
| `cgroup.kill`, `memory.oom.group`, `--storage-opt size=` rootless | **Settled here**: works, works, fails on ext4 |
| Delegated controllers | **Settled here**: cpu, memory, pids. Per-distribution preflight still needed |
| Rootless pause and unpause | **Settled**: works, 10 ms each, no memory benefit |
| Detached exec re-attach | **Settled on 4.9.3**: not possible. Not tested on Podman 6 |
| Installed seccomp profile | **Settled**: section 6. Workload suite passes under the product profile with no overhead |
| AppArmor under the unprivileged-userns restriction | **Partly**: nested user namespaces are reachable under the default profile despite the sysctl. No AppArmor profile was written or tested |
| gVisor overhead, start latency, memory; rootless Podman plus runsc | **Open**: runsc not installed. Re-run `--only workload,lifecycle,density --runtime runsc` once it is (the lifecycle and density benchmarks would need the runtime wired through; only the workload suite takes `--runtime` today) |
| Podman 6 | **Open**: everything here is 4.9.3. pasta start cost, exec cost and the first-use copy may differ |
| A server-class host | **Open**: see limits |
| Egress tamper tests, Firecracker | **Open**: not in this harness |

## Limits of these numbers

- One machine: a 2-core, 4-thread 2016 laptop CPU, running a desktop
  session. Absolute latencies will be lower and parallel throughput much higher on a
  server; the ratios (CLI versus socket, crun versus runc, transient versus persistent,
  first use versus later use) are more likely to carry over.
- One full run. The two rounds inside it agree within a few percent for medians, but
  there is no day-to-day repetition, and the parallel benchmark was repeated only under
  heavier load.
- The machine was shared. Tails (p95 and above) include other work. Run A's lifecycle
  matrix saw load1 up to 3.4.
- Podman 4.9.3 from Ubuntu 24.04, not 6.x.
- Density was measured to 176 sandboxes (196 with the confirmation step), not to the
  memory limit. Sandboxes per GB is an extrapolation of a slope measured over 0.5 GB.
- Idle sandboxes only. Nothing here measures a host full of working sandboxes.
- The image is small. First-use copy time scales with image size and was measured for
  9 MB and 60 MB of layers only.
- The cause of the API service exit in run A is unknown.
