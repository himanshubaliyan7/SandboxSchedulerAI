# crun versus gVisor: what the hardened tier costs

Measured through the product: one real `sbxd` in managed Podman mode with both
isolation tiers enabled, two templates per experiment that differ only in
`--runtime`, driven over the public REST API on keep-alive connections. The
harness is `bench/tierbench.py` (see `bench/README.md`); every number below comes
from `bench/results/20261011-010621/`, where `summary.md` has all 447 rows and
`results.json` every raw sample.

Phase-0 numbers (Podman driven directly, crun only) are in `RESULTS.md`.

## Headlines

1. **Claiming a warm sandbox costs the same on both tiers.** `POST /v1/sandboxes`
   is p50 10.5 ms on crun and 12.7 ms on gVisor; the claim is a database pop and
   never touches the runtime. The first command's output arrives about 6 ms later
   on gVisor (first byte 7 ms against 13 ms).
2. **Creating a sandbox takes twice as long on gVisor** (create to WARM p50
   102 ms against 205 ms, one at a time), so the pool refills at half the rate:
   about 16 creates/s against 8 at the best concurrency. With 8 warm sandboxes and
   2 refill workers, crun keeps every claim warm up to 5 claims/s; gVisor does up
   to 3/s and fell behind at 5/s in two rounds of three.
3. **A gVisor sandbox uses 7 times the idle memory**: 17.1 MB of cgroup memory
   against 2.3 MB, and 9 host processes against 1. By `MemAvailable` the gap is
   about 4 times (16-17 MB against 3.6-4.3 MB), which is roughly 60 sandboxes per
   GB against 250.
4. **What the code does decides the overhead.** Pure computation is 1.0 to 1.3
   times slower; importing modules and reading or writing many small files is 2
   to 6 times slower; creating processes is 7 to 11 times slower. 200 git commits
   take 1.0 s on crun and 6.7 to 8.6 s on gVisor.
5. **glibc images are cheaper under gVisor than musl ones.** `python3 -c pass`
   is 4.0 times slower on `python:3.12-alpine` and 1.8 times on
   `python:3.12-slim`; `pip install requests` is 2.5 times against 1.4 times.
   Everything in the suite ran correctly on the Debian image under gVisor.
6. **Terminal output is the weak point.** `yes | head -c 256k` through a PTY
   takes 0.7 s on crun and about 10 s on gVisor (15 times). Output through a
   pipe is 4.6 times slower. Nothing was lost in either case.
7. **None of the gVisor flags tried made a difference worth configuring**
   (`--directfs=false`, `--overlay2=none`, `--overlay2=root:memory`): every
   workload stayed within 8% of the default.
8. **CPU limits are enforced on both tiers, but gVisor's own work is charged to
   the sandbox.** With a 0.5 CPU limit the cgroup used exactly 0.50 CPUs on both;
   the program inside saw 0.50 on crun and believed it had used 1.1 on gVisor.

## Method

- **Host:** Intel i5-7200U (2 cores, 4 threads), 11.5 GB RAM, Ubuntu 24.04,
  rootless Podman 4.9.3, crun 1.14.1, gVisor release-20261005.0 on the systrap
  platform (no KVM), governor `powersave`. Full facts are in `host.json`.
- **Build:** `sbxd` and `sbx-agent` built once from commit `fe5655d` with
  `git archive`, never from the working tree.
- **Blocks:** one tier, one cell, one round per block. Every block records the
  load average, `MemAvailable` and the host's busy share at its start and end.
  Tiers alternate inside a round (crun, runsc, runsc, crun), so drift hits both.
- **Rounds:** three rounds for everything, run hours apart. The tables give the
  p50 of all samples pooled over the rounds, the p95 in brackets where it adds
  something, and the range of the three per-round medians where it is wide.
- **Workload timing:** each workload is timed three ways: by a driver inside the
  sandbox (`inner`), end to end through the API (`api`), and, for the Python
  workloads, around the work itself with interpreter start excluded (`core`).
  The API adds 2 to 6 ms, so the tables use `inner`.

### What weakens these numbers

- **One laptop, one gVisor release, one platform.** Systrap without KVM is
  gVisor's slowest configuration for system calls; a server with KVM will differ.
- **The run was interrupted by a power cut.** Rounds 1 and 2 and most of round 3
  ran before it on kernel 7.0.0-28. Round 3 of the workloads, network and flag
  blocks ran after the reboot on kernel 7.0.0-38, with a fresh server and
  database but the same binaries (`host.resumed.json`). Of 80 workload, network
  and flag cells checked, 75 have a round-3 median within 10% of the range of
  rounds 1 and 2 and the other 5 are within 18%, so the rounds are pooled.
- **46 of 402 blocks started with a load average of 3 or more** on 4 threads,
  most of it left over from the block before (density ramps and the 10 claims/s
  cell). They are listed at the end of `summary.md` and were not excluded.
- **Download throughput is noise.** The 40 MB download depends on the internet
  link; it varied between 2 and 10 MB/s on the bare host from round to round.
- **The Alpine git workload with automatic maintenance on is unreliable.** See
  "Process-heavy workloads".

## 1. Lifecycle

`python:3.12-alpine`, 1 CPU, 256 MiB, n = 90 per tier.

| Measurement (ms) | crun p50 (p95) | gVisor p50 (p95) | ratio |
|---|---|---|---|
| Warm claim, pool at rest | 10.5 (30.1) | 12.7 (31.9) | 1.2 |
| Warm claim, while the pool refills | 32.9 (40.7) | 32.4 (41.9) | 1.0 |
| First output byte of `echo` after the claim | 7.0 (11.2) | 13.0 (22.3) | 1.9 |
| Claim to first output byte | 18.6 (38.0) | 26.8 (54.8) | 1.4 |
| Delete answered | 4.3 (6.2) | 4.1 (5.9) | 1.0 |
| Delete finished (container gone) | 135 (187) | 150 (216) | 1.1 |
| Memory bomb to reported `oom_killed` (n = 24) | 415 (591) | 683 (911) | 1.6 |

A claim made while the pool is refilling costs about 20 ms more on both tiers:
the refill's container create competes for the same 4 threads.

### Create to WARM and refill rate

| Refill concurrency | crun p50 ms | gVisor p50 ms | crun creates/s | gVisor creates/s |
|---|---|---|---|---|
| 1 | 102 | 205 | 9.5 | 4.9 |
| 2 | 123 | 266 | 15.5 | 7.5 |
| 4 | 223 | 477 | 16.3 | 8.1 |

Going from 2 to 4 workers buys almost nothing on this machine and doubles the
latency of each create, so 2 is the right setting for 4 threads.

### Sustained claims

8 warm sandboxes, 2 refill workers, claims at a fixed rate for 30 s, each
released at once. "Warm" is the share of claims served from the pool; three
values are the three rounds.

| Claims/s | crun warm | gVisor warm | crun p95 ms | gVisor p95 ms |
|---|---|---|---|---|
| 1 | 100% | 100% | 39-152 | 38-148 |
| 2 | 100% | 100% | 103-143 | 37-143 |
| 3 | 86%, 100%, 100% | 100% | 39-619 | 39 |
| 5 | 100% | 43%, 100%, 37% | 40-42 | 13-3913 |
| 10 | 4-5% | 3-5% | 3743-4605 | 4642-4960 |

At 10 claims/s neither tier keeps up: crun answered 76 to 84% of the requests
within the 5 s claim wait and gVisor 42 to 68%; the rest got `503`. The crun dip
at 3/s in round 1 and the gVisor result at 5/s are the same thing seen from two
sides: gVisor's refill rate (7.5/s) is close enough to 5 claims/s that any other
load on the machine tips it over.

### Server start

| Measurement (ms) | crun p50 | gVisor p50 |
|---|---|---|
| Process start to ready, empty host | 197 | 204 |
| Process start to first claim served, empty host | 358 | 496 |
| Process start to pool of 4 full | 1290 | 1769 |
| Restart to ready, re-adopting 10 warm sandboxes | 198 | 209 |
| Restart to ready, re-adopting 100 warm sandboxes | 260 | 429 |
| First claim after that restart (100 sandboxes) | 39 | 73 |

All 10 and all 100 sandboxes were re-adopted in every restart on both tiers.

## 2. Memory and density

Idle warm sandboxes in steps of 0, 20, 40, 80, 160 (and 320 on crun; the gVisor
ramp stops at 160 to keep 2 GB free). Values are per sandbox at the top step and
were the same within 2% in all three rounds.

| Per sandbox | crun | gVisor |
|---|---|---|
| cgroup `memory.current` of the sandbox scope | 2.32-2.34 MB | 17.1-17.2 MB |
| conmon | 0.41 MB | 0.23 MB |
| PSS of the sandbox's processes and conmon | 1.9 MB | 21.3 MB |
| Drop in host `MemAvailable` | 3.6-4.3 MB | 16.1-17.0 MB |
| After one Python command (cgroup) | 3.1 MB | 19.8 MB |
| Host processes / threads | 1 / 17 | 9 / 42 |
| Create latency at the top step (p50) | 249-287 ms | 386-569 ms |

**Sandboxes per GB:** by `MemAvailable`, the number that decides when a host
runs out, about 250 on crun and 60 on gVisor. By cgroup accounting plus conmon it
is about 365 and 58.

The memory is linear in the number of sandboxes on both tiers; nothing is shared
between gVisor sandboxes that would make the hundredth cheaper than the first.
The second Podman service that the gVisor tier needs costs about 40 MB (PSS)
once per host.

A gVisor sandbox's 17 MB counts against its own memory limit: a 256 MiB template
leaves the tenant about 239 MiB.

## 3. Agent workloads

Time inside the sandbox, p50 in ms, n = 30 per tier and image (12 for git).
Templates: 1 CPU, 1 GiB.

### Computation

| Workload | alpine crun | alpine gVisor | ratio | slim crun | slim gVisor | ratio |
|---|---|---|---|---|---|---|
| Pure-Python loop (work only) | 317 | 292 | 0.9 | 268 | 286 | 1.1 |
| SHA-256 of 64 MiB (work only) | 182 | 175 | 1.0 | 169 | 172 | 1.0 |
| `re` micro-workload (work only) | 118 | 148 | 1.3 | 74 | 77 | 1.1 |
| `json` micro-workload (work only) | 108 | 200 | 1.9 | 93 | 112 | 1.2 |
| numpy matrix multiply (work only) | 266 | 378 | 1.4 | 170 | 189 | 1.1 |
| numpy SVD (work only) | 192 | 283 | 1.5 | 112 | 111 | 1.0 |

Code that stays in user space runs at native speed. The Alpine `json` and numpy
ratios are higher than the Debian ones for the same work; the likely cause is
musl's allocator making more system calls than glibc's, which was not isolated
here.

### Starting Python and importing

| Workload | alpine crun | alpine gVisor | ratio | slim crun | slim gVisor | ratio |
|---|---|---|---|---|---|---|
| `python3 -c pass` | 18.5 | 74.0 | 4.0 | 13.9 | 24.6 | 1.8 |
| 30 stdlib imports, no bytecode cache | 1281 | 4177 | 3.3 | 756 | 1038 | 1.4 |
| 30 stdlib imports, cached | 214 | 523 | 2.4 | 147 | 301 | 2.1 |
| `import numpy` (work only) | 188 | 342 | 1.8 | 188 | 245 | 1.3 |

### Files

| Workload | alpine crun | alpine gVisor | ratio | slim crun | slim gVisor | ratio |
|---|---|---|---|---|---|---|
| 2,000 small files in `/workspace`: write, stat, read, delete (work only) | 98 | 534 | 5.5 | 78 | 395 | 5.1 |
| 1,500 files read from the image (work only) | 27 | 218 | 8.0 | 14 | 64 | 4.6 |
| 100 MB sequential write to `/workspace` | 3083 MB/s | 1183 MB/s | 0.38 | 3019 MB/s | 1269 MB/s | 0.42 |
| 100 MB sequential read back | 1737 MB/s | 768 MB/s | 0.44 | 6204 MB/s | 5374 MB/s | 0.87 |

`/workspace` is a tmpfs, so these are memory speeds, not disk speeds.

### Process-heavy workloads

| Workload | alpine crun | alpine gVisor | ratio | slim crun | slim gVisor | ratio |
|---|---|---|---|---|---|---|
| Shell loop, 500 x fork+exec of `/bin/true` | 279 | 3118 | 11.2 | 272 | 2536 | 9.3 |
| 200 x `subprocess.run` from Python | 174 | 1181 | 6.8 | 182 | 1204 | 6.6 |
| `seq 200000 \| sort -rn \| uniq \| wc -l` | 878 | 1580 | 1.8 | 106 | 293 | 2.8 |
| 8 threads doing file I/O | 1386 | 5942 | 4.3 | 1449 | 3807 | 2.6 |
| 200 git commits, automatic maintenance off | 1066 | 8593 | 8.1 | 1017 | 6777 | 6.7 |
| 200 git commits, git's defaults | unreliable | unreliable | | 1494 | 12252 | 8.2 |

That is 5 to 6 ms per process on gVisor against 0.55 ms on crun.

The Alpine row marked unreliable: git 2.54 in that image starts a detached
maintenance process after every commit, which races with the next commit and
with the workload's clean-up. Over the three rounds it produced 5 errors on
each tier, such as `bad tree object HEAD`, so it is a property of the workload
and not of either runtime. The runs that finished took 3.0 s on crun and 22.6 s
on gVisor. An agent that runs git in a gVisor sandbox should set
`maintenance.auto false`: no run failed with it, and it was 1.5 to 2.8 times
faster.

## 4. Network and `pip install`

Through the egress proxy with an allowlist, against the bare host with no proxy.

| Measurement | host | crun | gVisor |
|---|---|---|---|
| HTTPS GET, new connection (p50 ms, n = 60) | 78 | 82 | 85 |
| HTTPS GET, kept connection (p50 ms) | 24 | 26 | 25 |

The proxy and gVisor's network stack add 3 to 7 ms to a new connection and
nothing measurable to a kept one.

| `pip install` (seconds, p50 of 3) | alpine crun | alpine gVisor | ratio | slim crun | slim gVisor | ratio |
|---|---|---|---|---|---|---|
| `requests`, cold | 4.1 | 10.0 | 2.5 | 2.8 | 3.8 | 1.4 |
| `requests` again, nothing to do | 0.57 | 1.71 | 3.0 | 0.46 | 0.91 | 2.0 |
| `requests`, from pip's cache | 1.5 | 3.9 | 2.7 | 1.2 | 1.8 | 1.6 |
| `numpy`, cold | 7.2 | 15.2 | 2.1 | 5.3 | 6.5 | 1.2 |
| `numpy`, from pip's cache | 4.9 | 14.4 | 2.9 | 3.3 | 4.8 | 1.5 |

Both packages imported correctly afterwards on both tiers and both images.

## 5. Output streaming

`python:3.12-alpine`, n = 12. Every byte arrived in every run.

| Stream | crun | gVisor | ratio |
|---|---|---|---|
| PTY over WebSocket, 256 KiB of short lines, with acknowledgements | 0.69 s | 10.3 s | 15 |
| The same without acknowledgements | 0.70 s | 8.8 s | 12 |
| PTY over WebSocket, one 8 MiB line | 15.5 MB/s | 1.4 MB/s | 11 |
| PTY over NDJSON, 256 KiB of short lines | 0.72 s | 10.3 s | 14 |
| Pipe over NDJSON, 50 MiB | 17.5 MB/s | 3.8 MB/s | 4.6 |

A gVisor PTY delivers 13 to 15 bytes per frame on short lines against 85 to 97
on crun, so each line travels as its own frame. That points at per-write cost
rather than bandwidth, and at coalescing reads in the agent for a few
milliseconds as the first thing to try if terminal-heavy use on gVisor matters;
that change has not been tried.

## 6. Limits under attack

| Measurement | crun | gVisor |
|---|---|---|
| Memory bomb at 256 MiB: reported `oom_killed` in | 0.42 s | 0.68 s |
| CPU hog at 1.0 CPU: cgroup usage | 1.00 | 1.00 |
| CPU hog at 0.5 CPU: cgroup usage | 0.50 | 0.50 |
| The same, CPU time the program believes it used | 0.50 | 1.11 |
| Fork bomb at `pids` 64: host tasks in the sandbox's cgroup | 17 | 152 |
| Fork bomb: `echo` still answers (of 5 tries) | 5, 5, 5 | 4, 1, 1.5 |
| Fork bomb: usable again after the kill (p50) | 5 ms | 112 ms |

Two things differ on gVisor and both are worth knowing:

- **The pids limit does not bound a fork bomb the way it does on crun.** On crun
  the kernel refused forks at the limit (76 to 414 refusals per run) and the
  sandbox stayed responsive. On gVisor the guest's processes are not host
  processes, the host counter never reached a limit, and the sandbox mostly
  stopped answering until the bomb was killed. The other sandboxes and the host
  were not measured during the bomb; the CPU and memory blocks above show those
  two limits holding on gVisor.
- **CPU time inside the sandbox is not what the cgroup charges.** A program that
  measures its own CPU use sees up to twice the limit.

## 7. gVisor flags

Each variant on its own server, runsc only, the six file-heavy and start-up
workloads, n = 30. p50 in ms.

| Workload | default | `--directfs=false` | `--overlay2=none` | `--overlay2=root:memory` |
|---|---|---|---|---|
| Create to WARM | 209 | 217 | 193 | 197 |
| Idle memory per sandbox (MB) | 16.9 | 19.7 | 17.0 | 16.9 |
| `python3 -c pass` | 74 | 74 | 72 | 73 |
| 30 imports, uncached | 4166 | 4153 | 4127 | 4156 |
| 2,000 small files | 770 | 770 | 718 | 768 |
| 1,500 image files | 477 | 474 | 451 | 476 |
| 500 x fork+exec | 3103 | 3160 | 3094 | 3104 |

`--overlay2=none` is 5 to 8% faster on files and creates; `--directfs=false`
costs 3 MB per sandbox and gains nothing. Neither is worth a configuration knob.

## What this means for the product

- **Keep crun as the default and gVisor as the per-template choice.** The claim
  path is unaffected, so the latency promise holds on both; the cost of gVisor is
  paid by tenant code and by density.
- **Size a gVisor pool differently.** It refills at half the rate and each warm
  sandbox costs 17 MB, so the same burst tolerance needs about twice the warm
  count and seven times the memory.
- **Recommend Debian-based images for gVisor templates.** The gap between musl
  and glibc is larger than any flag measured here.
- **Document the fork-bomb and CPU-accounting differences** in the threat model:
  on gVisor the pids limit is not a usable bound on guest processes.
- **PTY coalescing in the agent** is the one code change these numbers suggest
  trying.

## Not measured

- A host with KVM, a server-class CPU, or Podman 6.
- More than 160 gVisor sandboxes (memory safety margin on an 11.5 GB machine).
- Disk-backed workspaces; `/workspace` is a tmpfs here.
- Long-running sandboxes: every measurement is minutes old at most.
