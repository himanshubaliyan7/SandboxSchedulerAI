# Warm pool, sandbox lifecycle and multi-host scheduler for an untrusted-code sandbox service

Research date: 2026-10-09. Scope: control plane, pooling, lifecycle, scheduling. Isolation security, egress policy and the streaming protocol are out of scope.

Reading guide for the report writer:
- "Cited Findings" are things a source says. Source quality is flagged inline: **[primary]** = project docs, repo or paper; **[vendor]** = vendor's own claim, not independently measured; **[weak]** = blog, forum or aggregator with no stated methodology.
- "Inferences" are my design recommendations derived from the findings. They are not sourced facts and none of them was tested.
- The biggest hole in this research: **I found no published, methodology-backed benchmark of rootless Podman start latency broken down by runtime, storage driver and network mode.** Every number of that kind below is either for a sub-component or from a weak source. The team must measure this themselves; a recipe is given in the first section.

---

## 1. Cold-start times for rootless Podman, what dominates, and what cuts it

### Takeaway
The OCI runtime itself is a few milliseconds (crun about 5 ms); the rest of a `podman run` (Podman's own setup, storage layer mount, network namespace plus pasta, conmon, cgroup setup) is what costs hundreds of milliseconds, and no rigorous public breakdown of it exists for rootless mode. The reliable techniques are to move creation off the request path (pre-created idle containers) and, on the Firecracker path, to restore snapshots with lazily loaded memory; CRIU is not a realistic option for rootless Podman.

### Cited Findings

**Runtime (crun vs runc)**
- crun alone, measured with hyperfine over 101 runs on a Raspberry Pi 4 with a precompiled seccomp profile: mean 5.6 ms ± 3.0 ms (min 4.2 ms, max 26.8 ms). Compiling the seccomp profile used to cost 159 ms (2017 figure); crun now reuses a cached precompiled BPF filter. **[primary, Red Hat/Podman engineers; hardware is a Pi, not a server]** — [Red Hat blog](https://www.redhat.com/en/blog/speed-containers-podman-raspberry-pi)
- The same work took overall Podman container start on that hardware "from almost 2 seconds to under 0.3 seconds". **[primary]** — [Red Hat blog](https://www.redhat.com/en/blog/speed-containers-podman-raspberry-pi)
- Podman's own performance guide says only "The fastest OCI runtime is probably crun" and gives no runc figure. **[primary]** — [Podman performance tutorial](https://github.com/containers/podman/blob/main/docs/tutorials/performance.md)
- One blog reports about 0.45 s per container rootless vs about 0.35 s rootful for `podman run --rm alpine true` in a loop, with no hardware, runtime or variance stated. **[weak — do not rely on]** — [lucaberton.com](https://lucaberton.com/blog/podman-vs-docker-2026/)

**Storage driver**
- Order from fastest to slowest: native overlayfs, fuse-overlayfs, vfs. Rootless native overlayfs needs Podman 3.1+ and kernel 5.13+ (5.11 without SELinux); RHEL 8.5+ has it. **[primary]** — [Podman performance tutorial](https://github.com/containers/podman/blob/main/docs/tutorials/performance.md)
- Important exception that directly affects a warm pool: with rootless Podman, a modified UID/GID mapping, native overlayfs, and no earlier container from that image with that mapping, *container creation is slower with native overlayfs than with fuse-overlayfs* (runtime speed is unaffected). `--userns auto` is hit hardest because the mapping can change on every run. **[primary]** — [Podman performance tutorial](https://github.com/containers/podman/blob/main/docs/tutorials/performance.md)
- Switching the storage driver requires `podman system reset`, which erases all of that user's containers and images. **[primary]** — [Podman performance tutorial](https://github.com/containers/podman/blob/main/docs/tutorials/performance.md)
- fuse-overlayfs pegs the CPU under filesystem-heavy workloads; writes to volumes largely bypass it. **[primary but dated — the post predates native rootless overlay]** — [Red Hat blog](https://www.redhat.com/sysadmin/podman-rootless-overlay)

**Networking**
- pasta is the default and, since **Podman 6.0.0, the only** rootless network driver, so a pasta-vs-slirp4netns choice no longer exists on Podman 6. The guide states pasta "carries a performance penalty" (relative to host/bridge networking). **[primary]** — [Podman performance tutorial](https://github.com/containers/podman/blob/main/docs/tutorials/performance.md)
- `--network=none` (or host) avoids starting netavark and sometimes aardvark-dns, which is one of the listed start-time savings. **[primary]** — [Red Hat blog](https://www.redhat.com/en/blog/speed-containers-podman-raspberry-pi)
- Claims that pasta starts "faster" than slirp4netns are qualitative only; the throughput tables in circulation (for example about 1 Gbps vs 5+ Gbps) carry no methodology. **[weak]** — [OneUptime](https://oneuptime.com/blog/post/2026-03-17-switch-slirp4netns-pasta-podman/view)

**Other start-time levers named by the Podman team**
- `--transient-store` (or `transient_store = "true"` in containers-storage.conf) moves locks, JSON files and the Podman database onto tmpfs under `/run`; described as "dramatically" faster, with no number. **[primary]** — [Red Hat blog](https://www.redhat.com/en/blog/speed-containers-podman-raspberry-pi)
- `--log-driver=none` if container logs are not needed. **[primary]** — [Podman performance tutorial](https://github.com/containers/podman/blob/main/docs/tutorials/performance.md)
- Rootless overhead in general: the Podman team reports an anecdotal about 20% slowdown for `podman build` rootless vs root, attributed to FUSE storage and user-mode networking; it gives no start-time figures. **[primary, anecdotal, 2023]** — [Podman blog](https://blog.podman.io/2023/07/podman-performance-root-and-rootless/)

**CRIU checkpoint/restore**
- Podman checkpointing "only works for containers run by root users". **[primary-ish, Fedora Magazine; may be dated]** — [Fedora Magazine](https://fedoramagazine.org/podman-checkpoint-in-fedora-linux/)
- CRIU restore timing in public data is at the scale of seconds, not milliseconds: a small 11-task container showed about 1.9 s restore time in a live-migration test. **[primary but old, hardware and CRIU version not given]** — [CRIU wiki](https://www.criu.org/Performance_research)

**Firecracker boot and snapshot restore**
- Firecracker boots to application code in under 125 ms, with under 5 MB memory overhead per microVM, and can create up to 150 microVMs per second per host. **[primary, 2020 paper — dated but canonical]** — [NSDI '20 paper](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Firecracker's snapshot docs give no latency figure; restore cost depends on memory size, vCPU count and device count. With the `File` backend memory is a `MAP_PRIVATE` mapping loaded on demand; with the `Uffd` backend a userspace process serves page faults over a Unix socket. Diff snapshots are still "developer preview". cgroups v1 causes high restore latency, so cgroups v2 is recommended. **[primary]** — [Firecracker snapshot docs](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/snapshotting/snapshot-support.md)
- Resuming one snapshot more than once is treated as insecure unless unique state is regenerated: Linux 5.18+ reseeds its PRNG via VMGenID, but cached random values, unique IDs and tokens are duplicated and are the user's problem. Snapshots must resume on essentially identical hardware and software. **[primary]** — [Firecracker snapshot docs](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/snapshotting/snapshot-support.md)
- AWS Lambda SnapStart stores snapshots in S3 in 512 KB chunks behind a tiered cache: worker-local L1 about 1 ms per chunk, per-AZ L2 single-digit ms per chunk, S3 up to hundreds of ms per chunk. It records the chunks each invocation touches and preloads that working set in access order on later cold starts. **[primary, AWS]** — [AWS Compute Blog](https://aws.amazon.com/blogs/compute/under-the-hood-how-aws-lambda-snapstart-optimizes-function-startup-latency)
- SnapStart launch claim: cold starts falling from over 6 seconds to under 200 ms for the Java example. **[vendor]** — [AWS News Blog](https://aws.amazon.com/blogs/aws/new-accelerate-your-lambda-functions-with-lambda-snapstart/)
- CodeSandbox (now Together Code Sandbox) publishes: fresh VM within 4 s, snapshot restore under 2 s, live clone of a VM within 3 s, using copy-on-write memory. **[vendor; I am not certain which of the Together docs pages carries the exact sentence]** — [Together docs](https://docs.together.ai/docs/together-code-sandbox)
- A shared base memory file mapped copy-on-write into each sandbox and filled lazily with userfaultfd lets clean pages share page cache while writes get private pages. **[weak-to-medium, engineering blog]** — [Cortex builders blog](https://builders.cortex.io/blog/sandboxing-agents-part-2.md)

### Inferences
- **What dominates a rootless `podman run`** is, by elimination, everything except the runtime: crun is about 5 ms, yet the best published end-to-end figure on slow hardware is "under 0.3 s". The listed optimizations (transient store, skipping netavark, seccomp cache) indicate the cost centres are Podman's database and lock I/O, network setup, and seccomp compilation. This is a deduction from the fix list, not a measured breakdown.
- **Pre-created idle containers are the correct primary technique for rootless Podman.** They take all of creation off the request path, and CRIU (the other "skip startup" option for containers) is effectively root-only and second-scale.
- **Use a fixed, pre-declared UID/GID mapping per pool rather than `--userns auto`.** The documented native-overlay penalty applies the first time an image is used with a new mapping. A pool that always creates with the same mapping pays it once per image per host. If per-sandbox distinct mappings are required for isolation, benchmark fuse-overlayfs against native overlay for *create* time specifically, because this is the one case where the usual ordering inverts.
- **"Idle running" is likely better than "paused" for the warm state** when the sandbox contains an agent daemon, because assignment is then one RPC with no unfreeze step. I found no published timing for rootless `podman unpause`, so this is a judgement call to settle by measurement.
- **Benchmark recipe the team should run** (because nothing public answers the question): on the target host, `hyperfine --warmup 5 --runs 100` over the matrix {crun, runc} x {native overlay, fuse-overlayfs} x {`--network=none`, pasta} x {transient store on/off} for (a) `podman run --rm IMG true`, (b) `podman create` alone, (c) `podman start` on a pre-created container, (d) `podman exec` into an idle running container, (e) `podman rm -f`. Item (d) is the request-path cost with a warm pool; (b)+(c)+(e) set the replenish and recycle rate.
- On the Firecracker path, snapshot restore with userfaultfd plus a recorded-working-set prefetch (the SnapStart and E2B pattern) is the state of the art; plain 125 ms boot is the fallback.

### Gaps
- No published rootless Podman start benchmark with stated methodology comparing crun vs runc, native overlay vs fuse-overlayfs, or pasta vs slirp4netns. All three comparisons are qualitative in primary sources.
- No runc millisecond figure from a primary source.
- No published timing for `podman create` vs `podman start` vs `podman exec`, for `podman pause`/`unpause` in rootless cgroups v2, or for `podman rm`.
- No authoritative Firecracker snapshot-restore latency; a blog claim of "under 5 ms" resume exists but has no methodology and I did not include it as a finding ([source](https://mohakchugh.is-a.dev/blog/firecracker-microvm-snapshot-restore-serverless-cold-start)).
- Current state of rootless CRIU in Podman 6 was not confirmed against Podman's own docs; the two sources found are a 2021-era article and a wiki diff.
- Reflink/btrfs/XFS copy-on-write rootfs clone timings for containers: not found.

---

## 2. How existing systems implement warm pools, sizing and replenishment

### Takeaway
Two families exist: (a) keep N pre-booted instances per template and replace each one as it is claimed (Lambda's local pool, Kubernetes agent-sandbox, Tensorlake, Fly user-built pools), and (b) make creation itself cheap enough to skip the pool by restoring a snapshot per request (E2B, SnapStart, Modal, CodeSandbox). Pools are almost always per image/template with a fixed target, and the only published sizing formula is Lambda's use of Little's law.

### Cited Findings

**AWS Lambda / Firecracker**
- Each worker's MicroManager "keeps a small pool of pre-booted MicroVMs, ready to be used when Placement requests a new slot", because 125 ms boot is "not fast enough for the scale-up path". **[primary]** — [NSDI '20 paper](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Sizing rule: "The required mean pool size can be calculated with Little's law: the pool size is the product of creation rate and creation latency... at 125ms creation time, one pooled MicroVM is required for every 8 creations per second." **[primary]** — [NSDI '20 paper](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- A slot is only ever used for a single function and a single concurrent invocation, but is reused for many serial invocations of that function. Slot states are initializing, idle and busy. Each slot exists for at most 12 hours before being recycled. **[primary]** — [NSDI '20 paper](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- SnapStart: one immutable encrypted snapshot is reused to resume many execution environments; Lambda periodically regenerates snapshots to apply patches. **[primary]** — [AWS Compute Blog](https://aws.amazon.com/blogs/compute/under-the-hood-how-aws-lambda-snapstart-optimizes-function-startup-latency)

**E2B (open-source `e2b-dev/infra`)**
- No pool of pre-booted VMs is described. "A sandbox is a resumed snapshot": templates hold memory, disk and VM state in object storage, keyed by build ID (`memfile`, `rootfs.ext4`, `snapfile`, `metadata.json`). **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- Memory is served on page fault by a userfaultfd handler from the template memfile, with an optional prefetcher for known-hot pages; a build "optimize" phase records touched pages to produce prefetch hints. The rootfs is read-only with a per-sandbox copy-on-write cache exposed as an NBD device from an in-process userspace server. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- What E2B *does* pool is the slow-to-create host plumbing: network slots (namespace, veth pair, tap device, host-side IP from a /16, NAT, nftables firewall) are pooled and reused, and leftover namespaces are cleaned at orchestrator startup. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- Templates are faster and more resource-efficient than snapshots of live sandboxes; prefetch is much less effective for live snapshots because of memory fragmentation. **[primary]** — [E2B docs](https://docs.e2b.dev/sandbox/snapshots)
- E2B's stated target is that sandboxes take no more than 200 ms to start. This is from a job posting and is a goal, not a measurement. **[weak]** — [job post](https://startup.jobs/platform-engineer-e2b-8145058)

**Kubernetes agent-sandbox (kubernetes-sigs)**
- CRDs: `Sandbox` (core, `agents.x-k8s.io/v1beta1`; a single stateful pod with stable identity and storage, with scheduled deletion, pause and resume) plus extensions `SandboxTemplate`, `SandboxWarmPool` ("pre-warms N sandboxes") and `SandboxClaim` (adopts a sandbox from the pool). The README's pinning example uses release `v1.0.2`. gVisor and Kata are supported via RuntimeClass. **[primary]** — [GitHub README](https://github.com/kubernetes-sigs/agent-sandbox), [warm pool quickstart](https://agent-sandbox.sigs.k8s.io/docs/use-cases/examples/warmpool-quickstart/)
- Published warm-claim performance (kops, Kubernetes 1.35.6): about 300 claims/s per cluster at p90 ≤ 200 ms for a 12-second burst of 3,600 claims into a 3,700-replica pool. Sustained adoption with concurrent refill tops out around 70–85 sandboxes/s per `SandboxWarmPool`. **[primary, stated methodology]** — [performance-tuning.md](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/docs/performance-tuning.md)
- At 45 claims/s Poisson across 4 pools: sustained-tuned config gave p50 92 ms, p90 182 ms, p99 376 ms; the burst-tuned config gave about 57 ms p50 for roughly 10 s, then fell to about 1.4 s cold once the pool drained (p90 2.28 s). **[primary]** — [performance-tuning.md](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/docs/performance-tuning.md)
- When the pool is exhausted the floor is the Kubernetes cold path: in the GKE live test (Kubernetes 1.36.2, 20 e2-standard-16 workers, 75-claim burst against a 150-sandbox pool) p50 was about 41–51 s and p99 82–102 s across configurations. Note the doc's own inconsistency: these runs had a pool twice the burst size yet still show cold-scale latencies, which suggests warm adoption was not occurring in that test; the doc lists "confirmed warm adoption" as a precondition for the burst profile. **[primary; interpret with care]** — [performance-tuning.md](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/docs/performance-tuning.md)
- Replenishment knobs: `--sandbox-warm-pool-max-refill-rate` (recommended 100 per pool), `--sandbox-warm-pool-replenish-delay` (recommended 0; 15–20 s is "fragile under any API failure"), `--sandbox-warm-pool-max-batch-size` (default 300). Sizing rule given: per-pool refill rate ≥ per-pool claim arrival rate. **[primary]** — [performance-tuning.md](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/docs/performance-tuning.md)

**Tensorlake (commercial sandbox pools)**
- A pool = one template (image, cpus, memory, entrypoint, timeout, network policy) + `warm_containers` (idle pre-booted buffer; 0 scales to zero) + `max_containers` (cap on warm plus claimed). After each claim the pool boots a replacement. With no warm container available the claim cold-starts on demand; at `max_containers` new sandboxes stay `pending` until a slot frees. Claims cannot override the template. Snapshot restores cold-boot and do not use warm containers. **[primary vendor docs]** — [Tensorlake docs](https://docs.tensorlake.ai/sandboxes/pools)

**Daytona**
- Architecture: stateless API server, Go "runner" per compute node managing Docker containers, proxy, in-sandbox daemon (toolbox port 21111, terminal 22222, SSH 22). Snapshots are stored as container images with a runner-local LRU cache; "sub-90ms sandbox creation" is claimed from a cached snapshot. A cleanup loop runs every 5 minutes. **[vendor; fetched from a third-party docs mirror, not daytona.io]** — [docs mirror](https://mintlify.wiki/daytonaio/daytona/concepts/architecture)
- The 90 ms figure is not consistent across Daytona's own material: a third-party review notes the GitHub README cites about 200 ms. **[weak]** — [rywalker.com](https://rywalker.com/research/daytona)

**Fly Machines**
- No platform-managed warm pool; users build one from stopped or suspended machines. States — persistent: `created`, `started`, `stopped`, `suspended`, `failed`; transient: `creating`, `starting`, `stopping`, `restarting`, `suspending`, `destroying`, `launch_failed`, `updating`, `replacing`. Suspended machines keep memory and disk and resume faster than a start from stopped. A machine in a transient state for more than 5 minutes (10 for `updating`) may be stuck. **[primary]** — [Fly docs](https://docs.fly.io/machines/machine-states)
- Fly staff advice on the forum: creates can be slow and unpredictable, so keep "a pool of pre-created machines so the impact of slow creates happens outside your critical path", call the wait endpoint with a deadline, and on timeout force-destroy and create another. One user measured 400–600 ms wake from fully stopped and a 3–8 s window when hitting a machine mid-shutdown. **[weak, forum; my attribution of quotes to specific threads is approximate]** — [thread 11244](https://community.fly.io/t/fly-machines-create-and-forget-and-limitations/11244), [thread 28143](https://community.fly.io/t/machine-stuck-in-created-for-2h20m-before-booting-iad-sibling-machines-started-in-seconds/28143), [thread 7022](https://community.fly.io/t/machines-take-3-8s-to-start-when-hitting-them-during-shutdown/7022)

**Modal**
- Uses gVisor (`runsc`) checkpoint/restore rather than CRIU; snapshot taken just before the container accepts its first request; "cold start latency ... can be more than halved". **[vendor, Jan 2025]** — [Modal blog](https://modal.com/blog/mem-snapshots)
- Sandbox memory snapshots are experimental, need the V2 Sandbox backend, expire after 7 days, and reset live TCP streams on restore. **[primary vendor docs]** — [Modal docs](https://modal.com/docs/guide/sandbox-memory-snapshots)

**Cloudflare Containers**
- Pre-schedules instances and pre-fetches images globally; a request goes to the nearest location that already has the image, even if that is farther from the user. Cold start typically 1–3 s depending on image size. Unused pre-warmed images are not billed. **[primary vendor docs]** — [Cloudflare docs](https://developers.cloudflare.com/containers/concepts/architecture/)

**OpenFaaS**
- Default min replicas 1, max 20 (Pro); scale-to-zero is opt-in with a 15-minute idle default. On scale-from-zero the gateway holds the HTTP connection, scales up, then proxies. Recommended cold-start avoidance is simply min replicas ≥ 1 and pre-pulled images. No latency figures. **[primary]** — [OpenFaaS docs](https://docs.openfaas.com/architecture/autoscaling/)

### Inferences
- **Sizing formula to adopt:** per (host, template) warm target `W = ceil(lambda * T_create * safety) + burst_reserve`, where `lambda` is the claim rate and `T_create` the create-to-ready latency. This is Lambda's Little's-law rule plus a burst term. Worked example with a hypothetical 0.5 s rootless create and 20 claims/s: mean in-flight creations = 10, so a floor of about 10 before the safety factor. The agent-sandbox data shows why the burst term matters: a pool sized only for the mean went from 57 ms to 1.4 s the moment it drained.
- **Replenish immediately and rate-limit rather than delay.** Agent-sandbox found a replenish delay fragile and recommends refill rate ≥ claim rate with a cap; Tensorlake replaces one-for-one on claim. Refill should be capped per host so replenishment CPU does not starve running sandboxes.
- **Pools should be per template (image + resource shape + network profile), and claims must not be able to mutate the template.** Every system surveyed does this (Lambda slot per function, agent-sandbox per SandboxTemplate, Tensorlake per pool). Anything a claim needs to vary (env, mounts, tenant identity) must be injectable after creation via the in-sandbox agent.
- **Pool the slow plumbing separately even if you do not pool sandboxes** — E2B pools network namespaces rather than VMs. For rootless Podman the analogue is pre-pulled images and, if needed, pre-created network namespaces.
- **Predictive sizing is not documented by anyone surveyed.** All public material describes fixed targets plus reactive refill. A simple EWMA on `lambda` per template is a reasonable first step, but it would be the team's own design.

### Gaps
- Modal's scheduler and pool internals: nothing public found.
- Daytona's warm-pool implementation and runner-selection algorithm: not described in the docs fetched; the `daytonaio/daytona` source was not read.
- AWS's sizing for SnapStart or Provisioned Concurrency internals: not public.
- Replit: no relevant material found.
- Cloudflare pool sizing numbers: not published.
- microsandbox and firecracker-containerd: not researched (ran out of budget).

---

## 3. Sandbox state machine and guaranteeing no cross-tenant leaks

### Takeaway
Production systems converge on single-tenant, single-use instances with a hard maximum lifetime; none of the systems surveyed resets an instance in place and hands it to a different tenant. For this service the safe default is destroy-and-recreate after every assignment, with reuse permitted only within the same tenant session.

### Cited Findings
- Lambda: a slot serves exactly one function and one concurrent invocation, cycles initializing → idle ↔ busy, and is recycled after at most 12 hours. The Placement service owns lifetime limits and terminates idle or redundant slots. **[primary]** — [NSDI '20 paper](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Lambda idle slots hold memory only; memory is about 40% of server capital cost, so an idle slot should cost about 40% of a busy one, which requires CPU to be soft-allocated and oversubscribed. **[primary]** — [NSDI '20 paper](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- E2B lifecycle: create (place → fetch template → acquire network slot, overlay, memory → resume VM → call in-VM agent `/init`, retried until ready; the API stores the record only after success) → running (`MarkRunning` writes a routing record) → stopping (`MarkStopping` on kill, pause, checkpoint or crash deletes the routing record, guarded by `execution_id` in a Lua script). Idle sandboxes auto-pause; traffic auto-resumes them. The routing record expires after `sandbox_max_length_in_hours`. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- E2B timeouts: pause has a detached 80 s budget, the Redis transition key a 95 s TTL, terminal status writes 10 s, and the auto-pause overstay budget defaults to 120 s. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- Fly's state list distinguishes persistent from transient states and documents a stuck-state threshold (5 min in a transient state). **[primary]** — [Fly docs](https://docs.fly.io/machines/machine-states)
- Daytona marks sandboxes on a failed runner `unknown` and failed ones `error`; new sandboxes go to healthy runners. **[vendor, via mirror]** — [docs mirror](https://mintlify.wiki/daytonaio/daytona/concepts/architecture)
- Tensorlake: `timeout_secs` is set on the pool, not per sandbox; at capacity, sandboxes are `pending`. **[primary vendor docs]** — [Tensorlake docs](https://docs.tensorlake.ai/sandboxes/pools)
- Cloning from one snapshot duplicates PRNG-derived state, IDs and tokens unless regenerated. **[primary]** — [Firecracker snapshot docs](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/snapshotting/snapshot-support.md)
- Docker/Podman pause uses the cgroup freezer, which the process cannot observe or intercept, unlike SIGSTOP. **[primary, Docker man page; Podman rootless behaviour not confirmed]** — [docker-container-pause(1)](https://manpages.debian.org/testing/docker.io/docker-container-pause.1)

### Inferences

Proposed state machine (my design, assembled from the Lambda, E2B and Fly models):

| State | Kind | Meaning | Exits |
|---|---|---|---|
| `CREATING` | transient | host agent is creating the container/VM | → `WARM` on agent health check; → `FAILED` on error or deadline |
| `WARM` | persistent | idle, belongs to no tenant, counted in the pool | → `ASSIGNED` on claim; → `RECYCLING` on warm-age TTL or template version change |
| `ASSIGNED` | transient | bound to tenant, per-session config being injected | → `RUNNING`; → `RECYCLING` on inject failure |
| `RUNNING` | persistent | executing or awaiting commands for one tenant session | → `PAUSED` on idle timeout (optional); → `RECYCLING` on release, max TTL, or host drain |
| `PAUSED` | persistent | frozen, still owned by the same tenant | → `RUNNING` on resume (same tenant only); → `RECYCLING` on paused TTL |
| `RECYCLING` | transient | kill, remove container, delete writable layer, volumes, netns | → `DESTROYED` |
| `DESTROYED` | terminal | row kept for audit, then garbage-collected | — |
| `FAILED` / `UNKNOWN` | persistent | create failed, or host agent unreachable | → `RECYCLING` by reconciler |

Rules that follow from the evidence:
- **`WARM → ASSIGNED` is a one-way door; there is no `RUNNING → WARM` edge.** Lambda reuses a slot only for the same function; nobody surveyed returns a used instance to a shared pool. In-place reset would have to clear the writable layer, every tmpfs, all processes (including ones that escaped the process group), cgroup counters, network namespace state, and kernel-held state such as SysV IPC and POSIX message queues; a single missed item is a cross-tenant leak. Destroy-and-recreate makes the leak surface the image and the host, not the previous tenant. The price is one create per assignment, paid in the background by the replenisher.
- **Every transient state needs a deadline and an owner that enforces it** (Fly's 5-minute "stuck" threshold, E2B's explicit budgets). Suggested starting values to tune: `CREATING` 30 s, `ASSIGNED` 5 s, `RECYCLING` 30 s.
- **Three independent timers per sandbox:** idle timeout (no command for N minutes), max lifetime (hard cap; Lambda uses 12 h), and warm-age TTL (so pooled instances do not outlive image patches; SnapStart regenerates snapshots for the same reason).
- **Fence stale actors with a generation token.** E2B guards routing-record deletion with `execution_id`; do the same so a late `release` or a recovering agent cannot act on a sandbox ID that has since been reassigned.
- **Record the sandbox as assignable only after the readiness check passes** (E2B stores the record only after `/init` succeeds).
- **Filesystem wipe = delete, not clean.** Give each sandbox its own writable layer and tmpfs for `/tmp` and the work dir, sized by the memory or storage quota, and remove them with the container. Named volumes should be per-tenant and never attached to a warm instance before assignment.
- If Firecracker snapshot clones are used, warm clones must regenerate unique state (machine-id, SSH host keys, any agent tokens) at assignment time.

### Gaps
- No public source describes a production system that does reset-in-place across tenants, so there is no evidence on how such an approach fails in practice; the recommendation against it rests on absence plus first-principles reasoning.
- No measured cost of `podman rm` plus recreate vs an in-place wipe was found.
- Behaviour of `podman pause` under rootless cgroups v2 (does it need the freezer controller delegated to the user slice?) was not confirmed from Podman docs.

---

## 4. Placing sandboxes across multiple worker hosts

### Takeaway
The common shape is a central control plane that decides *where* plus a per-host agent that owns *how*, with the agent holding the right to refuse. E2B's open-source scheduler is the most directly reusable reference: power-of-K-choices on a load score, retry with jittered backoff on refusal, and prefer the host that already has the data cached.

### Cited Findings
- E2B division of labour: the API "decides where a sandbox runs and tracks that it runs"; the per-node orchestrator (one Go binary, gRPC) "owns how it runs". **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- E2B placement algorithm: best-of-K — sample K ready nodes, score each by the higher of its CPU load and hugepage-pool load, pick the lowest. A node returning `ResourceExhausted` stays eligible; each refusal waits a full-jitter backoff that doubles up to a cap. K, the overcommit ratio and a smoothing alpha are adjustable at runtime via feature flags. A node that reports no pool scores 0.5. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- E2B locality: resume prefers the origin node while its snapshot is still cached locally; forks are placed as new sandboxes and therefore spread. Node states `Draining` (reversible) and `ShuttingDown` (terminal) both exclude new placement. The API caches each node's `outstanding_work` and `max_sandboxes`. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- E2B backpressure to clients: per-team, per-method, per-route budgets in Redis; exhaustion returns HTTP 429 with `Retry-After`. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- Lambda: the Worker Manager is a stateful sticky router (millions of requests per second, under 10 ms at p99.9) that sends a function's events to as few workers as possible for cache locality. When no slot is free it asks the Placement service, which balances CPU, memory, network and storage across the fleet and minimizes correlated allocation per worker; the placement decision typically takes under 20 ms. Placement leases the slot to the Worker Manager with a time-based lease so the router can act autonomously for a fixed period. **[primary]** — [NSDI '20 paper](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Lambda overcommit reasoning: oversubscription is "a statistical bet". Pick a compliance goal X (for example 99.99%) for the fraction of time functions get all requested resources without contention; efficiency is the ratio of the Xth-percentile to mean resource use, and that ratio falls roughly with √N for N *uncorrelated* workloads on a worker. Workloads from the same application or customer are correlated and count as one. **[primary]** — [NSDI '20 paper](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Cloudflare trades distance for readiness: it places on the nearest location that already has the image. **[primary vendor docs]** — [Cloudflare docs](https://developers.cloudflare.com/containers/concepts/architecture/)
- Exhaustion behaviour in commercial pools: cold-start on demand when no warm instance exists, and `pending` (queue) once the hard cap is reached. **[primary vendor docs]** — [Tensorlake docs](https://docs.tensorlake.ai/sandboxes/pools)
- OpenFaaS holds the client connection while scaling from zero, and offers an async route to decouple callers. **[primary]** — [OpenFaaS docs](https://docs.openfaas.com/architecture/autoscaling/)
- Kubernetes control-plane throughput is itself a limit for pool churn: per-pool sustained adoption about 70–85/s, bounded partly by kube-scheduler's default `--kube-api-qps=50`. **[primary]** — [performance-tuning.md](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/docs/performance-tuning.md)

### Inferences
- **Two decisions, two algorithms.** (1) *Claim routing* — which host's warm sandbox to hand out — is on the request path and should be an O(1) atomic pop with best-of-K over hosts that have warm stock for the template. (2) *Replenish placement* — where to create the next warm sandbox — is off the request path and can afford bin-packing by reserved CPU and memory.
- **Spread, do not pack, for warm stock; pack for long-running sessions.** Lambda's √N argument says overcommit safety comes from mixing uncorrelated tenants on a host, so a per-host cap on sandboxes per tenant is both a fairness and an overcommit-safety control.
- **Overcommit CPU, not memory.** Lambda soft-allocates and oversubscribes CPU while idle slots keep their memory. No source gives a universal ratio; E2B exposes it as a runtime flag. Start conservative (suggested: CPU 2–4x against requested limits, memory 1.0x against limits with a host reserve), measure the p99.x of real usage, then raise. These starting ratios are my suggestion, not sourced.
- **Admission control in three layers:** per-tenant token bucket and concurrent-sandbox quota at the API (HTTP 429 + `Retry-After`, as E2B does); a bounded per-template wait queue with a deadline when the pool is empty (hold briefly, then either cold-create or reject); and a host-level veto (`ResourceExhausted`) that the scheduler treats as retryable with full-jitter backoff.
- **Fairness when the pool is short:** serve the wait queue by weighted round-robin or deficit round-robin across tenants rather than FIFO, so one tenant's burst cannot drain the shared pool. Optionally reserve a slice of warm stock per paying tier. No surveyed source documents its fairness algorithm, so this is a design choice.
- **Give the scheduler stale-tolerant inputs.** Agents should push heartbeats with counts (warm per template, running, reserved CPU/memory, real load); the scheduler scores from this cache and relies on the agent's veto for correctness, exactly as best-of-K with refusals does.
- **Leases, not locks, between scheduler and agent** (Lambda's time-based lease): a host's authority over its warm stock expires if it stops heartbeating, which bounds how long a partitioned host can hold capacity.

### Gaps
- No public numbers for CPU or memory overcommit ratios used in production by any sandbox vendor.
- E2B's actual K, backoff cap and overcommit values are feature-flag driven and not in the doc.
- No source describing per-tenant fairness algorithms for a shared warm pool.
- Queueing theory sizing (for example Erlang-C for wait probability) applied to sandbox pools: no practitioner source found.

---

## 5. Driving Podman programmatically, and rootless density limits

### Takeaway
Use the Podman REST API over the per-user Unix socket through the official Go bindings, with one long-lived `podman system service` per sandbox-owning user; shelling out pays process start and re-initialization on every call. Note the Go module path has moved to `go.podman.io/podman/v6`. Published per-host density limits for rootless Podman essentially do not exist, so limits must be found by load test.

### Cited Findings
- `podman system service` exposes two APIs on one socket: a Docker-compatible layer (documented as Docker v1.40) and the native Libpod API ([reference](https://docs.podman.io/en/latest/_static/api.html)). Default sockets: rootful `unix:///run/podman/podman.sock`, rootless `unix://$XDG_RUNTIME_DIR/podman/podman.sock` (for example `/run/user/1000/podman/podman.sock`). **[primary]** — [podman-system-service(1)](https://docs.podman.io/en/latest/markdown/podman-system-service.1.html)
- The service exits after 5 seconds of inactivity by default (`--time`; 0 disables; also `service_timeout` in containers.conf) and is restarted by systemd socket activation on the next connection. **[primary]** — [podman-system-service(1)](https://docs.podman.io/en/latest/markdown/podman-system-service.1.html)
- Access to the socket equals arbitrary code execution as the owning user; access control is the socket's file permissions; TCP exposure without mutual TLS is strongly discouraged. **[primary]** — [podman-system-service(1)](https://docs.podman.io/en/latest/markdown/podman-system-service.1.html)
- Go bindings on the `main` branch now import as **`go.podman.io/podman/v6/pkg/bindings`** (with `/containers`, `/images`, pods, networks, manifests). The brief's `github.com/containers/podman/v5/pkg/bindings` is the previous major's path. Connect with `bindings.NewConnection(ctx, "unix:///run/podman/podman.sock")`; create with `containers.CreateWithSpec(conn, specgen.NewSpecGenerator(image, false), nil)`; start with `containers.Start(conn, id, nil)`. Build with the `remote` tag to exclude local-only code and shrink the binary. Debug with `podman --log-level=debug system service -t 0`. **[primary]** — [bindings README](https://raw.githubusercontent.com/containers/podman/main/pkg/bindings/README.md)
- Podman documentation is published for v6.0.2, confirming a 6.x line is current as of this research. **[primary]** — [Podman v6.0.2 docs](https://docs.podman.io/en/v6.0.2/markdown/podman-systemd.unit.5.html)
- Quadlet: `foo@.container` generates a `foo@.service` template that is instantiated as `foo@bar.service`; drop-ins are read from both the template and instance directories. Rootless Quadlets cannot be made by setting `User=`/`Group=`/`DynamicUser=`; the unit must live in the target user's own unit search path. Image pulls can exceed systemd's default 90 s start timeout. **[primary]** — [podman-systemd.unit(5)](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html)
- Subordinate IDs: the default allocation is 65,536 per user, set by `SUB_UID_COUNT`/`SUB_GID_COUNT` in `/etc/login.defs`, adjustable by root in `/etc/subuid` and `/etc/subgid`. **[primary]** — [Red Hat rootless Podman](https://www.redhat.com/sysadmin/rootless-podman)
- `user.max_user_namespaces` must be above zero for rootless Podman; one write-up shows 16000 on its example system. **[weak]** — [CSDN write-up](https://aicoding.csdn.net/6a5ddf4f662f9a54cb91350d.html)
- inotify: a forum thread reports the default per-user instance limit as 128 and containers failing with "too many open files" until it was raised (512, and 8192 in another report). This is about LXC on Proxmox, not Podman, but the sysctl is per host user and applies equally. **[weak]** — [Proxmox forum](https://forum.proxmox.com/goto/post?id=144326)
- Rootless containers inherit the launching user's ulimits. **[weak]** — [OneUptime](https://oneuptime.com/blog/post/2026-03-18-fix-too-many-open-files-errors-podman/view)
- One anecdote of rootless Podman hanging beyond about 15 containers until storage was wiped; commenters blamed configuration. **[weak, single report, not evidence of a limit]** — [Lemmy thread](https://deddit.petersanchez.com/g/selfhosted@lemmy.world/p/2ZRJk7J3LCcnGb7zQ1-Podman-running-container-limits)

### Inferences
- **Recommended stack:** per-host agent in Go, using `go.podman.io/podman/v6/pkg/bindings` against the Libpod endpoints (not the Docker-compatible layer, which is pinned to an old Docker API version and lacks Podman-specific options). Run the service with `--time 0` under a user systemd unit so the 5-second idle exit never puts a service cold start on the request path. Pin the Podman version per host image and treat the bindings version as coupled to it.
- **Shelling out** is acceptable for rare administrative operations (`podman system reset`, `podman system migrate`) but not for create/start/exec, because each CLI invocation re-opens the store and takes its locks; this follows from the start-time work described by Red Hat, though no source gives a head-to-head number.
- **Quadlet is a poor fit for the pool itself.** It is declarative and unit-file driven, so thousands of short-lived, uniquely named sandboxes would mean thousands of generated units and `daemon-reload`s. Use Quadlet/systemd for the long-lived pieces: the host agent, the `podman.socket`/service, and image pre-pull.
- **Subuid arithmetic is the first hard density wall if each sandbox gets its own ID range.** With the default 65,536 subordinate IDs under one user, giving each sandbox a full 65,536-wide mapping allows exactly one distinct range; giving each a 1,024-wide range allows 64. Options: allocate a much larger range to the sandbox user (root can assign millions of IDs), give sandboxes narrow ranges, share one mapping across sandboxes (cheapest and avoids the native-overlay create penalty, but weakens separation between sandboxes), or run several host users each with their own range and Podman service. This choice interacts with the isolation research and should be decided jointly.
- **Per-host sysctls and limits to raise and monitor** (values must be found by test): `fs.inotify.max_user_instances` and `max_user_watches`, `user.max_user_namespaces`, `kernel.pid_max` and the user slice's `pids.max`, `nofile` for the agent and the Podman service, `kernel.keys.maxkeys`. Each container also carries a conmon process and, when networked, a pasta process, so process and fd counts scale at a small multiple of sandbox count.
- **cgroup delegation matters for rootless resource limits:** CPU, memory and pids limits only work if those controllers are delegated to the user's systemd slice. This is general Linux/systemd knowledge and was not confirmed from a source in this research.

### Gaps
- conmon and pasta per-container memory overhead: no sourced figure found.
- No published scale test of rootless Podman at hundreds or thousands of containers per host.
- Whether the Libpod REST URL prefix on Podman 6 is `/v6.0.0/libpod/...` was not confirmed (the man page fetched does not show the path format).
- Concurrency behaviour of the Podman API service under many parallel creates (store lock contention) is undocumented in what I found.
- The inotify default of 128 comes from a forum post about a different container system; verify per distribution.

---

## 6. State store, coordination and failure handling

### Takeaway
The one open-source reference at production scale (E2B) uses Postgres for durable entities, Redis for the live running-sandbox and routing records with TTLs, and ClickHouse for events and metrics, with gRPC to per-node agents and no etcd. Correctness comes from reconciliation and fencing rather than from the store.

### Cited Findings
- E2B store split: **Postgres** = durable state (teams, templates, builds, snapshots, volumes, clusters); **Redis** = running-sandbox store, routing catalog, caches, rate limiting, peer registry; **ClickHouse** = sandbox events, host stats, metrics; object storage for template artefacts. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- E2B routing record `sandbox:routing:{id}` holds `orchestrator_id`, `orchestrator_ip`, `execution_id`, `sandbox_started_at`, `sandbox_max_length_in_hours`; it is written by the node once the in-VM agent is ready, expires by TTL, and is deleted through a Lua script guarded by `execution_id`. A routing miss at the edge proxy triggers the resume path. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- E2B crash hygiene: on startup the orchestrator reclaims leftover network namespaces; workload definitions are persisted so they survive orchestrator re-sync; node-scoped flags refresh every 30 s; node discovery supports Kubernetes, DNS, static list, or legacy Nomad. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- Lambda uses time-based leases between Placement and the Worker Manager so that ownership of a slot is unambiguous and bounded in time. **[primary]** — [NSDI '20 paper](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Daytona: runners register with the API on startup; a 5-minute cleanup loop; sandboxes on a failed runner become `unknown`. **[vendor, via mirror]** — [docs mirror](https://mintlify.wiki/daytonaio/daytona/concepts/architecture)
- Agent-sandbox measured the cost of optimistic concurrency in a Kubernetes-style store: 5,447 HTTP 409 conflicts in one 300-claim burst plus 45/s sustained test; batching writes cut conflicts to 3,077 but raised sustained p50 from 320 ms to 507 ms. **[primary]** — [performance-tuning.md](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/docs/performance-tuning.md)
- Fly's remedy for stuck creates: wait with a deadline, then force-destroy and create anew. **[weak, forum]** — [Fly community](https://community.fly.io/t/machine-stuck-in-created-for-2h20m-before-booting-iad-sibling-machines-started-in-seconds/28143)

### Inferences

Recommended starting point (smallest thing that is correct): **Postgres as the single source of truth, a per-host agent over gRPC, and Redis added only when claim throughput demands it.**

- **Claim as one atomic statement.** `UPDATE sandboxes SET state='ASSIGNED', tenant_id=$1, generation=generation+1 WHERE id = (SELECT id FROM sandboxes WHERE template_id=$2 AND state='WARM' AND host_id = ANY($3) ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING ...`. `SKIP LOCKED` gives contention-free pops without the 409 retry storms agent-sandbox measured under optimistic concurrency. If Postgres round-trips become the bottleneck, move the warm free-list to a Redis list per (template, host) with an atomic pop and keep Postgres as the durable record, which is E2B's split.
- **Minimal data model.** `hosts(id, addr, state{ready,draining,down}, cpu_total, mem_total, cpu_reserved, mem_reserved, last_heartbeat, agent_epoch)`; `templates(id, image_digest, cpu, mem, pids, net_profile, version)`; `pools(template_id, host_id or scope, warm_target, max_total, refill_rate)`; `sandboxes(id, template_id, template_version, host_id, state, tenant_id, generation, created_at, assigned_at, last_activity_at, expires_at, deadline_at, container_id)`; `tenants(id, max_concurrent, rate_limit, weight)`; `events` (append-only audit of transitions).
- **Label every container with `sandbox_id`, `generation` and `agent_epoch`** so the host agent can rebuild its view from the runtime alone after a crash.
- **Failure modes and handling:**

| Failure | Detection | Handling |
|---|---|---|
| Host agent crashes, containers keep running | missed heartbeats | On restart the agent lists containers by label and diffs against the control plane's desired set: unknown containers are destroyed (orphans); rows with no container go to `FAILED` then `RECYCLING`. Never re-adopt a container into `WARM` unless its row says `WARM` with a matching generation. |
| Host dies or is partitioned | heartbeat lease expiry | Mark host `down`, sandboxes `UNKNOWN`; stop routing; do not reissue IDs. When the host returns, its agent must destroy anything whose lease lapsed before taking new work (fencing by `agent_epoch`). |
| Control plane crashes mid-claim | transient-state deadline | A sweeper moves `ASSIGNED`/`CREATING`/`RECYCLING` rows past `deadline_at` to `RECYCLING`. All agent RPCs are idempotent and keyed by `(sandbox_id, generation)`. |
| Create hangs | per-state deadline | Force-destroy and create another (Fly's advice); count toward a per-host failure rate that trips a circuit breaker and drains the host. |
| Leaked host resources (netns, overlay mounts, cgroups, volumes) | startup scan and periodic scan | Agent-side garbage collection keyed on labels and name prefixes, as E2B does for network namespaces. |
| Stale release or duplicate callback | generation mismatch | Reject; E2B's `execution_id` guard is the model. |
| Template or image updated | version compare | Warm instances with an old `template_version` go straight to `RECYCLING`; refill uses the new version. |

- **Reconciliation loop shape:** level-triggered, not edge-triggered. Every few seconds per host: desired = rows for this host in non-terminal states; actual = labelled containers; converge. The control-plane pool controller separately compares `count(WARM)` to `warm_target` per pool and issues creates up to `refill_rate`.
- **etcd is unnecessary unless you adopt Kubernetes;** Postgres advisory locks or a leased row suffice for single-leader election of the pool controller. **NATS** (or Redis streams) is useful as an event bus for lifecycle events and agent fan-out but should not be the source of truth. These are judgement calls; I found no comparative source for sandbox control planes specifically.

### Gaps
- No public comparison of Postgres vs Redis vs etcd vs NATS for a sandbox control plane; the recommendation rests on one reference architecture (E2B) and general reasoning.
- No published throughput figure for `SKIP LOCKED` claim queues at the relevant scale was gathered here.
- Daytona's actual store and reconciliation code were not read.
- E2B's detailed orphan-reconciliation logic beyond network namespaces is not described in the architecture doc.

---

## 7. Kubernetes or Nomad instead of a custom scheduler

### Takeaway
Kubernetes now has a purpose-built warm pool API (agent-sandbox, v1.x) that delivers sub-200 ms warm claims, but its cold path is tens of seconds and it cannot schedule rootless Podman containers as such; Nomad is used by some microVM platforms mainly to run the per-node agents, not to schedule each sandbox. For a rootless-Podman-first design, a small custom scheduler is the natural fit, and the evidence on "teams that tried both" is thin and mostly second-hand.

### Cited Findings
- agent-sandbox warm claims: about 300 claims/s per cluster at p90 ≤ 200 ms in a burst; sustained p50 92 ms / p99 376 ms at 45 claims/s when refill keeps up. **[primary]** — [performance-tuning.md](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/docs/performance-tuning.md)
- The same document's cold-path floor on GKE was about 42–50 s p50, and reaching the good numbers required specific controller tuning (worker counts, disabling events and observability annotations, API Priority and Fairness insulation, raising API server inflight limits to 1500–3000 read and 500–1000 mutating). **[primary]** — [performance-tuning.md](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/docs/performance-tuning.md)
- agent-sandbox isolates via RuntimeClass (gVisor, Kata), i.e. the kubelet/CRI path, not rootless Podman. **[primary]** — [GitHub README](https://github.com/kubernetes-sigs/agent-sandbox)
- E2B lists Nomad only as a *legacy* service-discovery backend and as an autoscaler plugin (`nomad-nodepool-apm`); sandbox placement is done by E2B's own API with best-of-K, not by Nomad's scheduler. **[primary]** — [E2B ARCHITECTURE.md](https://raw.githubusercontent.com/e2b-dev/infra/main/docs/ARCHITECTURE.md)
- Koyeb (2021) moved from Kubernetes to Nomad + Firecracker + Kuma on bare metal, citing Kubernetes complexity, scalability and multi-zone limits, and Nomad's simplicity and extensibility. **[weak: third-party summary of Koyeb's post, dated 2021, no latency numbers]** — [Plushcap summary](https://www.plushcap.com/companies/koyeb/blog/summaries/2021/07)
- Trigger.dev reported about 3 s average from start to code executing on Kubernetes pods, with a stated goal of p95 under 500 ms via Firecracker. **[weak: feature-request page; goal, not result]** — [Trigger.dev feedback](https://feedback.trigger.dev/p/make-runs-startresume-faster-using-firecracker)
- Adapt (July 2026) moved agent sandboxes to Firecracker and wrote its own orchestrator, noting "Firecracker hands you a VM and not much else. There's no GKE-style layer doing the scheduling, networking, and lifecycle orchestration." **[weak: seen only through an aggregator]** — [aggregator copy](https://youmind.com/de-DE/landing/x-viral-articles/scaling-ai-agent-sandboxes-firecracker)
- Netlify reportedly replaced a Kubernetes build system with Firecracker microVMs and cut p95 queue time from 39.7 s to 1.9 s. **[weak: third-party directory page, not Netlify's own post; unverified]** — [createwith.com](https://www.createwith.com/tool/netlify/updates/netlify-replaces-entire-build-infrastructure-with-firecracker-microvms-cuts-queu)
- Kubernetes control-plane limits show up directly in pool behaviour: per-pool sustained throughput about 70–85/s, bounded by workqueue serialization and kube-scheduler's default `--kube-api-qps=50`. **[primary]** — [performance-tuning.md](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/docs/performance-tuning.md)

### Inferences
- **Kubernetes + agent-sandbox is a sensible alternative only if the team drops the rootless-Podman requirement** in favour of gVisor or Kata under containerd/CRI-O, already operates Kubernetes, and sizes pools so the tens-of-seconds cold path is never hit. In exchange it supplies the pool controller, claim API, reconciliation and node lifecycle.
- **Nomad's fit is as the fleet manager for host agents** (deploying, draining, autoscaling nodes), which is how E2B used it, rather than as the per-sandbox scheduler. Per-request placement through a general scheduler's evaluation pipeline adds latency and ties claim throughput to that scheduler's API limits.
- **The custom part is smaller than it looks with a warm pool.** On the request path the scheduler only pops a pre-created instance; the hard general-scheduler problems (bin packing, preemption, affinity) apply only to background refill, where latency is not critical. E2B's choice to write its own best-of-K placement while delegating node management is evidence that this split is workable.
- **A pragmatic hybrid:** custom control plane and host agent for sandboxes; an off-the-shelf tool (Nomad, systemd plus Ansible, or Kubernetes DaemonSets) to deploy and upgrade the host agents themselves.
- The pattern in the (weak) migration reports is consistent: teams leave Kubernetes for this workload because of pod start latency and control-plane overhead, and those who adopt Firecracker end up writing an orchestrator. Treat this as a tendency, not an established result.

### Gaps
- No detailed first-hand postmortem comparing Kubernetes, Nomad and a custom scheduler for agent sandboxes with before/after latency was found. All migration accounts here are second-hand or undated summaries.
- No evidence found of anyone running rootless Podman as a Nomad task driver at scale for this workload; the state of the Nomad Podman driver was not researched.
- Whether agent-sandbox works with a rootless or user-namespaced kubelet setup was not checked.
- The agent-sandbox GKE test results look inconsistent with its kops results (cold-scale latencies despite a pool larger than the burst); the cause is not explained in the doc.
