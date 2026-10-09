# Resource limits and network egress locking for rootless Podman / Firecracker sandboxes

Research date: 2026-10-09. All pages were fetched on that date; "latest"/"main"/"stable" doc URLs are rolling, so the version in force is whatever was live on that day. Explicit versions are given where the source stated one. Items I could not source are in the Gaps subsections, and anything that is my own synthesis is in Inferences, never in Cited Findings.

Source-quality note: page contents were extracted through a summarising fetch tool. Short quoted strings are as returned by that tool; long passages are paraphrased. The Podman `podman-run(1)` page was only partly readable (it is about 144k characters), so `--storage-opt`, `--timeout` details and the slirp4netns sub-options were not retrieved from it and are flagged as gaps.

---

## 1. Resource limits: cgroup v2 delegation for rootless Podman, flag-to-cgroup mapping, what cannot be limited

### Takeaway
Rootless limits only work on cgroup v2 with a systemd user session, and only for controllers that systemd delegates to `user@.service`; upstream systemd now ships `Delegate=pids memory cpu`, so `cpuset` and `io` still need a drop-in. Every Podman CPU/memory/IO flag is documented as "not supported on cgroups V1 rootless systems", and the failure mode when a controller is missing is a hard error, not a silent no-op.

### Cited Findings

Delegation mechanics
- Upstream systemd `units/user@.service.in` (main branch, fetched 2026-10-09) contains `Delegate=pids memory cpu`, `DelegateSubgroup=init.scope`, `TasksMax=infinity`, `OOMScoreAdjust=100`. So `cpuset` and `io` are not delegated by upstream default. — [systemd user@.service.in](https://raw.githubusercontent.com/systemd/systemd/main/units/user@.service.in)
- Podman's troubleshooting guide shows a user whose `cgroup.controllers` contains only `memory pids`, in which case `cpu` and `cpuset` limits cannot be set. This is older than the upstream unit above and conflicts with it for `cpu`. — [Podman troubleshooting.md](https://github.com/containers/podman/blob/main/troubleshooting.md); differs from [systemd user@.service.in](https://raw.githubusercontent.com/systemd/systemd/main/units/user@.service.in)
- Check what a user actually has: `cat "/sys/fs/cgroup/user.slice/user-$(id -u).slice/user@$(id -u).service/cgroup.controllers"`. — [Podman troubleshooting.md](https://github.com/containers/podman/blob/main/troubleshooting.md)
- Enable for all users: create `/etc/systemd/system/user@.service.d/delegate.conf` with `[Service]` / `Delegate=memory pids cpu cpuset`; the user must log out and back in. — [Podman troubleshooting.md](https://github.com/containers/podman/blob/main/troubleshooting.md)
- runc's cgroup v2 doc recommends the same drop-in with `Delegate=cpu cpuset io memory pids` followed by `systemctl daemon-reload`, and says systemd 244 or later is recommended because older systemd cannot delegate `cpuset`. (Taken from a search-result summary of a mirror of the runc doc, not a direct read of the page.) — [runc docs/cgroup-v2.md (mirror)](https://cos.googlesource.com/third_party/runc/+show/refs/heads/release-R129/docs/cgroup-v2.md)
- Error text when a controller is not delegated: for `--cpus`, `--cpu-period`, `--cpu-quota`, `--cpu-shares`: "the requested cgroup controller `cpu` is not available"; for `--cpuset-cpus`/`--cpuset-mems`: "the requested cgroup controller `cpuset` is not available". Cause given: "resource limit delegation is not enabled for the current user." — [Podman troubleshooting.md](https://github.com/containers/podman/blob/main/troubleshooting.md)
- Rootless cgroup v2 needs a valid systemd login session; running Podman via `su`/`sudo` causes `XDG_RUNTIME_DIR` errors. Suggested fixes: `systemd-run --user`, `machinectl shell`, `loginctl enable-linger` (lingering also keeps containers alive after logout). — [Podman troubleshooting.md](https://github.com/containers/podman/blob/main/troubleshooting.md)
- RHEL 8.10 (systemd 239) bug: after reboot the delegated controllers are not enabled until the systemd manager configuration is reloaded; AlmaLinux 8 reports the same. (Search-result summary only.) — [RHEL-99876](https://issues.redhat.com/browse/RHEL-99876); [AlmaLinux bug 153](https://bugs.almalinux.org/view.php?id=153)
- Kernel rules: a cgroup is delegated by granting write access to the directory and its `cgroup.procs`, `cgroup.threads` and `cgroup.subtree_control`; the delegatee must not be able to write the resource-control files of the delegated root itself because those govern the parent's distribution. With the `nsdelegate` mount option the kernel rejects writes to namespace-root files except those in `/sys/kernel/cgroup/delegate`. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- "No internal process" constraint: a non-root cgroup that holds processes cannot enable domain controllers in `cgroup.subtree_control`; a controller can be enabled in a child only if the parent has it enabled. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- Rootless Podman does not support resource limits at all on cgroups v1 hosts. — [Podman rootless.md](https://github.com/containers/podman/blob/main/rootless.md)

Podman flags (from `podman-run(1)`, rolling "latest" docs)
- `--cpus`: default `0.0` (no limit); shorthand for `--cpu-period` + `--cpu-quota`, cannot be combined with them. Not supported on cgroups V1 rootless. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--cpu-period`: CFS period in microseconds, default `100000`. `--cpu-quota`: CFS quota in microseconds, default no limit. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--cpu-shares`: relative weight, default `1024`, only matters under contention. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--cpuset-cpus`: list/range such as `0-3,7,11-15`. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--memory`: units b/k/m/g, `0` = no limit. `--memory-swap`: memory plus swap, requires `-m`, defaults to double the memory value, `-1` = unlimited swap. `--memory-reservation`: soft limit under contention. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--pids-limit`: default `2048` where the pids controller is available; `-1` = unlimited. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--blkio-weight` (10 to 1000), `--device-read-bps`/`--device-write-bps` (e.g. `/dev/sda:1mb`), `--device-read-iops`/`--device-write-iops` (e.g. `/dev/sda:1000`): all "not supported on cgroups V1 rootless systems". — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--cgroup-conf`: cgroup v2 only; writes an arbitrary value to a cgroup file, e.g. `memory.high`. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--cgroups`: default `enabled`; other values `disabled`, `no-conmon`, `split`. `--cgroup-parent`: relative paths resolve from the init process's cgroup. `--cgroupns` defaults to `private` on v2. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--oom-kill-disable`: "This flag is not supported on cgroups V2 systems." — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)

Kernel-side semantics of the target files
- `cpu.max`: format `$MAX $PERIOD`, default `max 100000`. `cpu.weight`: range 1 to 10000, default 100. `cpu.max.burst`: default 0. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- `memory.max`: hard limit; when reached and not reducible the OOM killer runs inside the cgroup. `memory.high`: throttles with heavy reclaim, "never invokes the OOM killer". `memory.swap.max`: default `max`. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- `pids.max`: hard limit; fork/clone returns `-EAGAIN`; `pids.events` `max` counts hits. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- `io.max`: keyed by `$MAJ:$MIN` with `rbps`, `wbps`, `riops`, `wiops`, e.g. `echo "8:16 rbps=2097152 wiops=120" > io.max`. `io.weight`: default 100, range 1 to 10000. Absolute limits are enforced by blk-throttle; weights need iocost or BFQ. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- Buffered-write (writeback) attribution to the cgroup only works on ext2, ext4, btrfs, f2fs and xfs; on other filesystems all writeback IO is charged to the root cgroup. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)

Other things rootless cannot do
- Cannot bind ports below 1024 without lowering `net.ipv4.ip_unprivileged_port_start`; cannot create device nodes (no `CAP_MKNOD`); `podman container checkpoint/restore` do not work (CRIU needs root); only overlay (optionally fuse-overlayfs) and VFS storage drivers; no NFS/GPFS home directories. — [Podman rootless.md](https://github.com/containers/podman/blob/main/rootless.md)
- `--oom-score-adj` in rootless mode cannot go below the current process's value and is clamped to it. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)

### Inferences
- Likely flag-to-file mapping on cgroup v2 (derived from the flag descriptions plus the kernel file semantics above, not from a single page that lists it): `--cpus N` and `--cpu-quota/--cpu-period` write `cpu.max` (e.g. `--cpus 0.5` gives `50000 100000`); `--cpu-shares` is converted to `cpu.weight`; `--cpuset-cpus` writes `cpuset.cpus`; `--memory` writes `memory.max`; `--memory-swap` minus `--memory` writes `memory.swap.max`; `--memory-reservation` writes `memory.low`; `--pids-limit` writes `pids.max`; `--device-*-bps/iops` write `io.max`; `--blkio-weight` writes `io.weight`. The report writer should treat the exact conversion formulas as unverified.
- Because `--memory-swap` defaults to double `--memory`, a sandbox given `--memory 512m` can use another 512m of swap unless `--memory-swap` is set equal to `--memory`. For a hard cap set both to the same value.
- A scheduler should verify delegation at start-up by reading `cgroup.controllers` for its own user slice and refuse to start if `cpu`, `memory`, `pids` (and `io`/`cpuset` if used) are missing, since Podman fails per container otherwise.
- IO limits are the weakest rootless control: they need the `io` controller delegated by a root-installed drop-in, a real block device major:minor, and a filesystem with cgroup writeback support. With rootless overlay on a shared home filesystem, expect IO throttling to be approximate.

### Gaps
- Per-distro shipped `Delegate=` values for current Ubuntu (24.04/26.04), Debian 13, Fedora 43/44 and RHEL 9/10 were not confirmed from distro sources. Only the upstream unit (`pids memory cpu`) and Podman's older example (`memory pids`) were found. Verify per host with `systemctl show user@$(id -u).service -p DelegateControllers`.
- The systemd version in which `cpu` was added to the upstream `Delegate=` line was not found.
- No source was read for crun-specific conversion formulas (shares to weight) or for `cpu.idle`, `memory.oom.group` support flags in Podman.
- The exact Podman version of the "latest" docs was not shown on the page.

---

## 2. Disk bounds and other exhaustion vectors (fork bombs, fds, inodes, /dev/shm, ulimits, OOM, throttling, noisy neighbours)

### Takeaway
Per-container writable-layer quotas in containers/storage depend on XFS project quotas (`pquota` mount), which is a root-level host setup; the portable rootless fallback is a read-only rootfs plus size-capped tmpfs mounts, which are charged to the memory cgroup. `pids.max` is the fork-bomb control, and `nproc` ulimits should not be used because they are per-user, not per-container.

### Cited Findings

Disk
- containers-storage overlay options: `size` = "Maximum size of a read/write layer" (b/k/m/g); `inodes` = "Maximum inodes in a read/write layer". — [containers-storage.conf(5)](https://github.com/containers/storage/blob/main/docs/containers-storage.conf.5.md)
- Quota requirement: "The directory used to store the containers must be an `XFS` file system and be mounted with the `pquota` option." Example fstab: `/dev/podman/podman-var /var xfs defaults,x-systemd.device-timeout=0,pquota 1 2`. — [containers-storage.conf(5)](https://github.com/containers/storage/blob/main/docs/containers-storage.conf.5.md)
- "Container storage implements `XFS project quota controls` for overlay storage containers and volumes"; project IDs are assigned per container and per built-in volume and must be unique on the filesystem; the doc's example uses `/etc/projects` and `/etc/projid` with start offsets 100000 (storage) and 200000 (volumes). — [containers-storage.conf(5)](https://github.com/containers/storage/blob/main/docs/containers-storage.conf.5.md)
- In rootless mode without `CAP_SYS_ADMIN`, many kernels prevent mounting overlay, requiring a `mount_program` (e.g. `/usr/bin/fuse-overlayfs`). Native rootless overlayfs needs Podman 3.1+ and kernel 5.12+. — [containers-storage.conf(5)](https://github.com/containers/storage/blob/main/docs/containers-storage.conf.5.md); [Podman rootless.md](https://github.com/containers/podman/blob/main/rootless.md)
- `--read-only` mounts the rootfs read-only; `--read-only-tmpfs` (default true) adds writable tmpfs on `/run`, `/tmp`, `/var/tmp`. `--tmpfs` default options are `rw,noexec,nosuid,nodev`. A `--mount type=tmpfs` example uses `tmpfs-size=64m`. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--shm-size`: default `64m`; `0` = no limit; conflicts with `--ipc=host`. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- tmpfs and shared memory are charged to the memory controller (`file`/`shmem` in `memory.stat`), as are kernel structures (dentries, inodes, kernel stacks, page tables, slab) and TCP socket buffers (`sock`). — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- Firecracker equivalents: jailer `--resource-limit` with `fsize` (max file size in bytes) and `no-file` (default 4096 if unset); block-device rate limiters; note that host page-cache writeback is asynchronous so heavy guest writes can slow other guests. — [Firecracker prod-host-setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)

Processes, fds, ulimits
- `--pids-limit` default 2048. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--ulimit`: "If nofile is unset, a default value of 1048576 will be used, unless overridden in containers.conf(5)"; if that exceeds the rootless user's hard limit, the hard limit is used. "Don't use nproc with the ulimit flag" because Linux applies nproc per user, not per container; use `--pids-limit`. `host` copies the host's values. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- Upstream `user@.service` sets `TasksMax=infinity`, so the user manager itself does not cap total tasks across all sandboxes of that user. — [systemd user@.service.in](https://raw.githubusercontent.com/systemd/systemd/main/units/user@.service.in)

OOM and throttling
- `memory.oom.group` (default 0): when set, all tasks in the cgroup are killed together or not at all; tasks with `oom_score_adj` -1000 are never killed. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- `memory.events` fields: `oom` (limit reached, allocation about to fail), `oom_kill` (processes killed), `max` (times usage was about to exceed `memory.max`), plus `high`, `low`, `oom_group_kill`. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- `cpu.stat` reports `nr_periods`, `nr_throttled`, `throttled_usec`, `nr_bursts`, `burst_usec` when the cpu controller is enabled; these are non-hierarchical. `cpu.stat.local` `throttled_usec` includes throttling inherited from ancestors. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- `memory.min` is hard protection and `memory.low` best-effort protection, which is the kernel's mechanism for protecting a workload from neighbours. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- Upstream `user@.service` runs with `OOMScoreAdjust=100`, and rootless `--oom-score-adj` cannot go below the caller's value, so a rootless supervisor cannot make itself less killable than its sandboxes by lowering its own score below what it inherited. — [systemd user@.service.in](https://raw.githubusercontent.com/systemd/systemd/main/units/user@.service.in); [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- Firecracker host guidance: disable swap, disable SMT where tenants are separated, disable KSM, one tenant per Firecracker process, bound log/serial output storage. — [Firecracker prod-host-setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)
- Reference sizing from one production provider: Claude Code cloud sessions get 4 vCPUs, 16 GB RAM, 30 GB disk per VM. — [Claude Code cloud environments](https://code.claude.com/docs/en/cloud-environments)

### Inferences
- Portable rootless disk bound without XFS pquota: `--read-only` plus `--tmpfs /tmp:rw,size=...,nr_inodes=...` and a size-capped `--shm-size`. Since tmpfs is charged to `memory.max`, the tmpfs size must be budgeted inside the memory limit, and filling `/tmp` can trigger the cgroup OOM killer rather than `ENOSPC` if the tmpfs cap exceeds remaining memory.
- If XFS pquota is available, the host admin must set it up once as root (mount option), after which rootless `--storage-opt size=` should be usable; whether it actually works for a non-root user with native overlay and with fuse-overlayfs is unverified (see Gaps).
- Setting `memory.oom.group=1` via `--cgroup-conf memory.oom.group=1` is the natural way to make an OOM kill terminate the whole sandbox instead of leaving a half-dead process tree, assuming the cgroup file is writable by the rootless runtime (unverified).
- Use `--cgroup-conf memory.high=<~90% of max>` to get reclaim throttling before the hard kill, and export `cpu.stat nr_throttled/throttled_usec` and `memory.events oom_kill` per sandbox as the scheduler's noisy-neighbour and failure-reason signals.
- Lower `--ulimit nofile` explicitly (the 1048576 default is far above what generated scripts need), and add a slice-level `TasksMax=` / `MemoryMax=` on the sandbox user's slice as a second, root-configured ceiling across all sandboxes.

### Gaps
- The `--storage-opt size=` text in `podman-run(1)` and whether it works rootless (native overlay vs fuse-overlayfs, XFS vs btrfs) could not be retrieved. The containers-storage page does not say whether quotas work under fuse-overlayfs.
- No source read for named-volume quotas (`podman volume create --opt o=size=`), ext4 project quotas, or btrfs qgroups.
- tmpfs `nr_inodes` option and inode exhaustion behaviour on the image store were not sourced.
- No measured data on CFS throttling latency or burst tuning for interpreter workloads.

---

## 3. Wall-clock timeouts and reliably killing everything in a sandbox

### Takeaway
Podman has a built-in `--timeout` enforced by conmon, and cgroup v2 provides `cgroup.kill`, an atomic, race-free SIGKILL of the whole subtree; Firecracker's own guidance is an external overwatcher that SIGKILLs the VMM process.

### Cited Findings
- `--timeout`: maximum runtime, after which conmon sends the kill signal; by default containers run until they exit or are stopped. `--stop-timeout` default is 10 seconds. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `cgroup.kill`: write-only, accepts `1`; SIGKILLs all processes in the cgroup tree and is concurrency-safe (handles concurrent forks); fails with `EOPNOTSUPP` in threaded cgroups. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- `cgroup.freeze`: writing `1` stops all processes in the cgroup and descendants; `frozen` in `cgroup.events` reflects state. — [Kernel cgroup v2 doc](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- pasta: "without a PID namespace, processes started by the command are not terminated once the command exits" (relevant to `--no-pidns`). — [passt(1)](https://passt.top/builds/latest/web/passt.1.html)
- Lingering (`loginctl enable-linger`) is required for containers to outlive the login session; without it, session teardown kills them. — [Podman troubleshooting.md](https://github.com/containers/podman/blob/main/troubleshooting.md)
- Firecracker: run an overwatcher that finds unresponsive Firecracker processes and kills them with SIGKILL, because signal-handler logging can deadlock. — [Firecracker prod-host-setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)
- Claude Code (as a reference design): foreground commands default to a 2-minute timeout, extendable to 10 minutes; commands that time out are moved to the background and stopped at a 30-minute background limit. — [Claude Code cloud environments](https://code.claude.com/docs/en/cloud-environments)

### Inferences
- Layered kill path for a rootless scheduler: (1) per-exec soft deadline in the scheduler; (2) `podman run --timeout N` as the conmon-enforced container deadline; (3) scheduler-side hard deadline that runs `podman kill -s KILL` / `podman rm -f`; (4) last resort, write `1` to the container cgroup's `cgroup.kill`. Because the container's PID 1 dying tears down the PID namespace, (3) is normally sufficient; (4) covers a hung runtime.
- Do not rely on the in-sandbox process to honour SIGTERM; go straight to SIGKILL (or set `--stop-timeout 0`/small) for untrusted code.
- Freeze-then-kill (`cgroup.freeze` then `cgroup.kill`) is unnecessary on kernels with `cgroup.kill`, since the latter is already fork-safe.
- CPU-time limits (`RLIMIT_CPU`) are not a substitute for wall-clock limits because a sleeping or IO-blocked process consumes no CPU time.

### Gaps
- The full `--timeout` text (which signal, interaction with `--stop-signal`, behaviour with `--rm`) was not retrieved.
- The kernel version that introduced `cgroup.kill` was not confirmed from the page read.
- No source read on whether a rootless user can write `cgroup.kill` for a Podman-created container scope in all configurations, or on `podman kill`/conmon failure modes under pids exhaustion.

---

## 4. Rootless networking modes and what egress control each allows; nftables inside the rootless netns

### Takeaway
`--network=none` is the only mode that is deny-by-default with no moving parts. pasta (default since Podman 5.0) is a user-mode forwarder with no destination filtering: it can disable protocols and host-loopback mapping but cannot express an allowlist, so allowlisting must be done by a proxy or by packet filtering in a namespace the sandbox cannot administer.

### Cited Findings

Modes
- `none`: "Creates a network namespace with no interfaces" (no connectivity). `pasta[:OPTIONS]`: default for rootless, only valid rootless, user-mode networking, copies host addresses and routes, options passed to pasta(1). `bridge`: default for rootful. `private`: bridge for rootful, pasta for rootless. `host`: uses host netns, flagged as a possible security vulnerability. Also `container:id`, `ns:path`, named networks. — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- "Since Podman 5.0, pasta is the default network tool." Connections from containers to the host's main IP fail unless there is more than one interface or a pasta network is configured explicitly; the tutorial says the pasta container-to-host limitation "has been fixed in Podman 5.3." — [Podman rootless.md](https://github.com/containers/podman/blob/main/rootless.md); [Rootless tutorial](https://github.com/containers/podman/blob/main/docs/tutorials/rootless_tutorial.md)
- Recommended way to reach the host from a pasta container: `host.containers.internal`, `host.docker.internal`, or `--add-host name:host-gateway`. — [Podman troubleshooting.md](https://github.com/containers/podman/blob/main/troubleshooting.md)
- Rootless bridge networks use `rootlessport` for port forwarding by default (does not preserve source IPs); `rootless_port_forwarder="pasta"` in containers.conf is experimental. — [Podman rootless.md](https://github.com/containers/podman/blob/main/rootless.md)
- Rootless `macvlan`/`ipvlan` have no access to host interfaces because rootless networking runs in a separate network namespace. — [podman-network-create(1)](https://docs.podman.io/en/latest/markdown/podman-network-create.1.html)

pasta knobs relevant to egress
- Default: the guest's default-gateway address is translated to the host. `--no-map-gw`: "Don't remap TCP connections and untracked UDP traffic, with the gateway address as destination, to the host." `--map-host-loopback none` disables the mapping, implies `--no-map-gw`, and loopback-addressed packets are dropped. — [passt(1)](https://passt.top/builds/latest/web/passt.1.html)
- `--map-guest-addr` can expose host-global services to the guest because the guest address "shadows that of the host". — [passt(1)](https://passt.top/builds/latest/web/passt.1.html)
- `-t`/`-u` (host to namespace): default `none` for passt and `auto` for pasta. `-T`/`-U` (namespace to init namespace): "Default is auto", which forwards ports bound on the other side. — [passt(1)](https://passt.top/builds/latest/web/passt.1.html)
- `--no-tcp`, `--no-udp`, `--no-icmp` (implies `--no-ndp`) drop the respective protocol. — [passt(1)](https://passt.top/builds/latest/web/passt.1.html)
- DNS: `--dns-forward` maps a guest-visible resolver address to the host resolver and "Maps only UDP and TCP traffic to port 53 or port 853"; `--dns-host` picks the upstream; `-D none` disables DNS address usage. — [passt(1)](https://passt.top/builds/latest/web/passt.1.html)
- `-o/--outbound`, `--outbound-if4`, `--outbound-if6` bind outbound sockets to a source address or interface. These select the source, they do not filter destinations. — [passt(1)](https://passt.top/builds/latest/web/passt.1.html)
- The man page has no option that restricts outbound destinations by address or name. — [passt(1)](https://passt.top/builds/latest/web/passt.1.html)

slirp4netns
- Defaults: network 10.0.2.0/24, gateway/host 10.0.2.2, built-in DNS 10.0.2.3, `--configure` address 10.0.2.100. By default "ports listening on INADDR_LOOPBACK (127.0.0.1) on the host are accessible from the child namespace" via 10.0.2.2. — [slirp4netns(1)](https://github.com/rootless-containers/slirp4netns/blob/master/slirp4netns.1.md)
- `--disable-host-loopback` (v0.3.0+) prohibits "connecting to 127.0.0.1:* on the host namespace"; `--enable-sandbox` (v0.4.0+); `--enable-seccomp` (v0.4.0+, experimental); `--disable-dns` (v1.1.0+); `--outbound-addr` (v1.1.0+, experimental). — [slirp4netns(1)](https://github.com/rootless-containers/slirp4netns/blob/master/slirp4netns.1.md)
- Caveat: if the host's `/etc/resolv.conf` points to a loopback address, the built-in DNS may still reach host loopback services; the man page advises "You may want to set up iptables for limiting access to the built-in DNS" and gives rules allowing UDP 53 to 10.0.2.3 and dropping everything else to it. — [slirp4netns(1)](https://github.com/rootless-containers/slirp4netns/blob/master/slirp4netns.1.md)

Rootless bridges (netavark)
- `--internal`: "Restrict external access of this network"; for bridge, IP forwarding is disabled on the bridge interface rather than done by firewall, no default route is added to the container, and aardvark-dns resolves only container names (other queries get `NXDOMAIN`). — [podman-network-create(1)](https://docs.podman.io/en/latest/markdown/podman-network-create.1.html)
- `isolate` option: per the page as read, default `strict` blocks traffic to and from all other bridge networks; `true` blocks only between networks that also have isolation enabled; must be written `isolate=<value>`. `--disable-dns` disables the DNS plugin; `no_default_route=true` suppresses the default route. — [podman-network-create(1)](https://docs.podman.io/en/latest/markdown/podman-network-create.1.html)

Other rootless facts
- Unprivileged ICMP echo needs `net.ipv4.ping_group_range` widened; `--sysctl` can set namespaced sysctls only. — [Podman troubleshooting.md](https://github.com/containers/podman/blob/main/troubleshooting.md); [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- A working example of the "no netns route out, proxy over a bind-mounted Unix socket" pattern in unprivileged user namespaces: Anthropic's sandbox-runtime uses bubblewrap with the network namespace unshared, host-side HTTP and SOCKS5 proxies listening on Unix sockets bind-mounted into the sandbox, and `socat` bridges inside. — [anthropic-experimental/sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime)

### Inferences
- Tamper-resistance of in-namespace nftables (reasoned from the namespace/capability model, not from a page read today): netfilter tables are per network namespace and require `CAP_NET_ADMIN` in the user namespace that owns that netns. A rootless user can therefore install nft rules in a netns it owns. Rules are tamper-proof against the sandboxed process only if that process lacks `CAP_NET_ADMIN` over the netns: Podman's default capability set does not include `NET_ADMIN`, so keep it that way, add `--cap-drop=ALL --security-opt no-new-privileges`, and do not allow nested user namespaces to gain capabilities over the existing netns (a new userns does not own the pre-existing netns, so it cannot edit its rules). The stronger arrangement is to put the rules in a netns the sandbox is not in at all: the rootless-netns that hosts a netavark bridge, or a separate "gateway" container that owns the only route out.
- Practical rootless egress topology that needs no host root: sandbox container on an `--internal` bridge (no default route, no forwarding) shared only with an egress-proxy container that is also attached to a second, pasta-connected network. The sandbox can then reach only the proxy's IP. This turns the forced-proxy pattern into a routing fact rather than an environment-variable convention.
- With plain pasta, the sandbox can reach the host's LAN, RFC1918 peers, the cloud metadata address and (via the gateway mapping) host services. `--map-host-loopback none`/`--no-map-gw` closes only the host-loopback path. pasta alone is not an egress lock.
- `--network=none` plus a bind-mounted Unix socket to a host proxy (the sandbox-runtime pattern) is the tightest option, at the cost of requiring an in-sandbox bridge (socat) and proxy-aware clients.

### Gaps
- Podman's own documentation of slirp4netns sub-options (`allow_host_loopback`, `cidr`, `outbound_addr`, `port_handler`) and pasta defaults that Podman passes (for example whether Podman adds `--no-map-gw` or `-T none` by default) were not retrieved; check `podman-run(1)` `--network` on the installed version.
- No authoritative source was read on which firewall driver netavark uses in the rootless network namespace (nftables vs iptables) in Podman 5.x, or on a supported hook for adding custom egress rules there.
- I found no primary source stating explicitly that nftables rules in a rootless-owned netns are safe against a process in a child user namespace; the reasoning above should be validated with a test (attempt `nft flush ruleset` from inside the sandbox, with and without `unshare -Ur`).
- Whether slirp4netns is still packaged/supported in Podman 5.x/6.x releases was not confirmed.

---

## 5. Egress locking patterns: default-deny, forced proxy, SNI/DNS filtering, eBPF, blocking metadata/loopback/RFC1918, DNS exfiltration and rebinding

### Takeaway
The pattern production systems converge on is default-deny at the network layer plus a host-side proxy that allowlists by hostname and then validates the resolved IP against a denied-address set; SNI-only filtering is cheap but every vendor documents that it is bypassable by domain fronting unless TLS is terminated. DNS must be controlled separately or it remains an exfiltration channel.

### Cited Findings

Proxy-side destination validation
- Smokescreen is an HTTP CONNECT proxy; it applies a per-role hostname ACL, resolves each requested domain and requires the resulting IP to be publicly routable (not internal). Flags: `--deny-range`, `--allow-range` (CIDR), `--deny-address`, `--allow-address`. Explicit allows can bypass default IP safety blocks. — [stripe/smokescreen](https://github.com/stripe/smokescreen)
- Smokescreen identifies clients by mTLS client-certificate common name (replaceable via `RoleFromRequest`); ACL is YAML with `version`, `services`, `default`; per-service policy is `open`, `report` (allow but log unlisted hosts) or `enforce`; `allowed_domains` per role; `global_allow_list`/`global_deny_list` with deny taking priority. The global deny list only blocks hostnames, not the IPs they resolve to. — [stripe/smokescreen](https://github.com/stripe/smokescreen)
- sandbox-runtime: before dialing an allowed hostname the proxy resolves the name once and drops denied addresses. Default denied set: loopback, unspecified, link-local, multicast, broadcast, cloud metadata endpoints (`169.254.169.254`, `100.100.100.200`, `168.63.129.16`), and all of the host's own interface addresses. RFC 1918 space is not denied by default; add it through `deniedResolvedAddresses` (example: `["10.0.0.0/8", "fc00::/7"]`). — [anthropic-experimental/sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime)
- Same source, residual risks: "whoever controls a permitted name's DNS (or any label under a permitted wildcard) controls what it resolves to"; allowlisted IP literals bypass the resolved-address check; public NAT addresses, router port-forwards and host-gateway aliases are not covered automatically; the check does not apply to connections sent through `parentProxy`/`mitmProxy`. — [anthropic-experimental/sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime)
- Claude Code's built-in proxy: after a hostname passes the allowlist it is resolved and refused when it resolves only to local addresses (loopback, link-local including `169.254.169.254`, the machine's own addresses); an allowed intranet name resolving to `10.0.0.0/8` connects; the check applies to hostnames, not IP literals. — [Claude Code sandboxing](https://code.claude.com/docs/en/sandboxing)

SNI filtering and domain fronting
- Claude Code: the proxy decides from the client-supplied hostname without inspecting TLS, so sandboxed code "can potentially use domain fronting or similar techniques"; allowing broad domains such as `github.com` "can create paths for data exfiltration"; remedy is a custom proxy that terminates TLS with its CA installed in the sandbox. — [Claude Code sandboxing](https://code.claude.com/docs/en/sandboxing)
- sandbox-runtime mitigation: `network.requireHostMatch` answers `421` when the Host header, TLS SNI, or absolute-form authority does not match the destination; `tlsTerminate` (experimental) terminates TLS at the proxy. — [anthropic-experimental/sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime)
- Modal: matching uses TLS SNI, TLS is not decrypted, ECH is not supported (ECH connections are matched on the outer name), and two domains sharing a TLS endpoint allow fronting; Modal recommends a sidecar proxy for protection. — [Modal sandbox networking](https://modal.com/docs/guide/sandbox-networking)
- Vercel: HTTPS is matched on SNI only and the firewall "does not prevent the mismatch by default"; advice is to allowlist narrow single-purpose hostnames, or use a `transform` rule (which terminates TLS) to pin the `Host` header. Plain HTTP cannot be domain-filtered. — [Vercel Sandbox firewall](https://vercel.com/docs/vercel-sandbox/concepts/firewall)
- E2B filters on the HTTP Host header (port 80) and TLS SNI (port 443); other ports use CIDR rules only; QUIC/HTTP3 is not supported for domain filtering. — [E2B internet access](https://docs.e2b.dev/sandbox/internet-access)

Forced-proxy caveats
- Tools that ignore `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY` (plain `ssh`, most database drivers) cannot connect when there is no direct route; `curl --noproxy '*' https://example.com` fails with `Could not resolve host`. — [Claude Code sandboxing](https://code.claude.com/docs/en/sandboxing)
- Daytona warns that with `outboundProxyUrl`, clients that ignore `HTTP_PROXY` can bypass the proxy, so a `domainAllowList` must also be configured. — [Daytona network limits](https://www.daytona.io/docs/en/network-limits/)
- sandbox-runtime runs a SOCKS5 proxy alongside the HTTP proxy for non-HTTP TCP (SSH, databases) under the same lists, and blocks `socket(AF_UNIX, ...)` with seccomp (plus `io_uring_setup/enter/register`, since `IORING_OP_SOCKET` on Linux 5.19+ would bypass it). Seccomp does not stop use of inherited fds or fds passed via `SCM_RIGHTS`. — [anthropic-experimental/sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime)

DNS
- Vercel: `deny-all` blocks DNS too; a policy with only `subnets.allow` "leaves DNS unrestricted", and code "can use those lookups to send data over DNS"; use domain rules to restrict resolution. — [Vercel Sandbox firewall](https://vercel.com/docs/vercel-sandbox/concepts/firewall)
- Cloudflare: with `enableInternet = false`, DNS is the sole non-HTTP exception and resolves only through Cloudflare's DNS servers, "which limits DNS-based exfiltration". — [Cloudflare Sandbox outbound traffic](https://developers.cloudflare.com/sandbox/guides/outbound-traffic/)
- E2B: when any domain rule is used, nameserver `8.8.8.8` is automatically allowed. — [E2B internet access](https://docs.e2b.dev/sandbox/internet-access)
- Claude Code: with no route around the proxy, the sandbox cannot resolve names itself (resolution happens at the proxy). — [Claude Code sandboxing](https://code.claude.com/docs/en/sandboxing)
- Cilium (v1.20.2 per the page's manifest URLs): `toFQDNs` with `matchName` (exact) or `matchPattern` (`*.github.com` matches subdomains, not `github.com`); requires an egress rule to kube-dns port 53 with `rules: dns: matchPattern: "*"` so the in-agent DNS proxy can inspect lookups and track DNS-to-IP mappings. Standalone DNS proxy is alpha. — [Cilium DNS-based policy](https://docs.cilium.io/en/stable/security/dns/)

Packet-level blocking of metadata
- Firecracker does no filtering; drop IMDS on the host: `sudo nft add rule firecracker filter iifname "tap*" ip daddr 169.254.169.254 counter drop` or `sudo iptables-nft -I FORWARD -i tap+ -d 169.254.169.254 -j DROP`. — [Firecracker prod-host-setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)

### Inferences
- Minimum deny set for any allow path (union of what the sources block plus standard special-purpose ranges; the list itself is my synthesis): `127.0.0.0/8`, `::1`, `0.0.0.0/8`, `169.254.0.0/16` and `fe80::/10` (link-local, covers `169.254.169.254`), `100.100.100.200` (Alibaba) and `168.63.129.16` (Azure) as named by sandbox-runtime, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10` (CGNAT), `fc00::/7`, multicast/broadcast, every address on the host's own interfaces, and the pasta/slirp gateway alias. IPv4-mapped IPv6 forms must be normalised before the check.
- DNS rebinding defence is "resolve once at the proxy, validate, then connect to that exact IP" (what Smokescreen and sandbox-runtime describe). A design in which the sandbox resolves and the firewall separately learns IPs (Cilium FQDN, DNS-allowlist ipsets) has a time-of-check/time-of-use window and depends on TTL handling.
- DNS exfiltration is closed only when the sandbox has no resolver at all (`--network=none`, or `--dns none` plus no route to port 53) and the proxy resolves on its behalf. Any design that lets the sandbox query a recursive resolver for arbitrary names leaks data through query labels even if all TCP is blocked. If a resolver must be exposed, make it answer only allowlisted names and return NXDOMAIN otherwise.
- Ranking for a rootless host: (1) `--network=none`; (2) none plus Unix-socket proxy; (3) `--internal` bridge plus proxy container; (4) pasta plus proxy env vars (advisory only, not a control). cgroup-bpf / eBPF egress filters (`BPF_PROG_TYPE_CGROUP_SKB`, `cgroup/connect4`) need `CAP_BPF`/`CAP_NET_ADMIN` on the host and are therefore a rootful option.
- Choice of proxy: Smokescreen gives the SSRF-safe resolve-and-check and role ACLs out of the box; Squid/Envoy can do CONNECT host ACLs and SNI peeking but the resolved-IP deny list must be configured explicitly; mitmproxy or Envoy with TLS termination is needed for Host/path-level policy and credential injection.
- Per-sandbox policy with a shared proxy needs a per-sandbox identity the sandbox cannot forge: source IP on a dedicated bridge, a dedicated Unix socket per sandbox, or mTLS. sandbox-runtime's note that a proxy-username token must carry at least 128 bits of randomness applies to token-in-proxy-auth designs.

### Gaps
- Squid, Envoy and mitmproxy documentation was not read; no configuration-level claims about them are sourced here.
- No source read on cgroup-bpf egress filtering, on systemd `IPAddressDeny=`/`IPAddressAllow=` (which uses cgroup-bpf) or its availability in user units.
- Cilium TTL handling (`tofqdns-min-ttl`), `toCIDRSet`/`except` syntax for excluding `169.254.169.254`, and Kubernetes NetworkPolicy semantics were not retrieved (the policy-language page only returned a redirect).
- No security write-ups on real sandbox exfiltration incidents (SSRF/metadata abuse) were read; the threat descriptions here come from vendor docs.
- Smokescreen's README does not list its default denied ranges or describe rebinding handling explicitly.

---

## 6. Firecracker equivalents: tap devices, per-VM network namespaces, host nftables, rate limiters

### Takeaway
Firecracker does no filtering itself: each microVM gets a tap device (ideally in its own network namespace joined by a veth pair), and all egress policy is host nftables on the tap/veth plus API-configured token-bucket rate limiters. This requires root (or `CAP_NET_ADMIN`) on the host for setup.

### Cited Findings
- Basic host setup: `sudo ip tuntap add tap0 mode tap`; `sudo ip addr add 172.16.0.1/30 dev tap0`; `sudo ip link set tap0 up`; enable `/proc/sys/net/ipv4/ip_forward`. Guest: `ip addr add 172.16.0.2/30 dev eth0`, `ip route add default via 172.16.0.1 dev eth0`. — [Firecracker network-setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md)
- nft NAT (recommended): `sudo nft add table firecracker`; `sudo nft 'add chain firecracker postrouting { type nat hook postrouting priority srcnat; policy accept; }'`; `sudo nft 'add chain firecracker filter { type filter hook forward priority filter; policy accept; }'`; `sudo nft add rule firecracker postrouting ip saddr 172.16.0.2 oifname eth0 counter masquerade`; `sudo nft add rule firecracker filter iifname tap0 oifname eth0 accept`. The iptables-nft form is described as no longer recommended. — [Firecracker network-setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md)
- Per-VM namespace (for snapshot clones): `sudo ip netns add fc0`; `sudo ip netns exec fc0 ip tuntap add name vmtap0 mode tap`; `sudo ip netns exec fc0 ip addr add 192.168.241.1/29 dev vmtap0`; `sudo ip link add name veth1 type veth peer name veth0 netns fc0`; `sudo ip netns exec fc0 ip addr add 10.0.0.2/24 dev veth0`; `sudo ip addr add 10.0.0.1/24 dev veth1`; `sudo ip netns exec fc0 ip route add default via 10.0.0.1`. — [Firecracker network-for-clones](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/network-for-clones.md)
- NAT in that design: inside the netns `iptables -t nat -A POSTROUTING -s 192.168.241.1/29 -o veth0 -j MASQUERADE`; on the host `iptables -t nat -A POSTROUTING -s 10.0.0.0/30 -o $UPSTREAM -j MASQUERADE`. Every clone keeps the same guest address (192.168.241.2/29); the host-side veth for clone `idx` is `10.<idx / 30>.<(idx % 30) * 8>.1/24`. The jailer takes `--netns` and the namespace must exist before launch. — [Firecracker network-for-clones](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/network-for-clones.md)
- The docs' example chains use `policy accept` / `iptables -P FORWARD ACCEPT`; i.e. the reference setup is allow-all and must be tightened by the operator. — [Firecracker network-setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md); [Firecracker network-for-clones](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/network-for-clones.md)
- Rate limiters: each network interface accepts optional `rx_rate_limiter` and `tx_rate_limiter`, each with a `bandwidth` token bucket (`size`, `one_time_burst`, `refill_time`) and an `ops` bucket (`size`, `refill_time`); set via `PUT /network-interfaces/{id}` and updatable at runtime via `PATCH`; PATCH merges, and `size`/`refill_time` of 0 disables a limiter. — [Firecracker patch-network-interface](https://github.com/firecracker-microvm/firecracker/blob/main/docs/api_requests/patch-network-interface.md)
- Production guidance: use the jailer with a unique uid/gid per instance; use its cgroup and `--resource-limit` features; flood mitigation via interface rate limiters, `tc qdisc`, and iptables `connlimit`; keep default seccomp filters; block IMDS with the nft rule quoted in section 5. The cgroup file names in that page are cgroup v1 names (`memory.limit_in_bytes`, `cpu.cfs_quota_us`, `blkio.throttle.*`). — [Firecracker prod-host-setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)

### Inferences
- A locked-down Firecracker egress path is: per-VM netns, forward chain `policy drop`, explicit accept only from the tap to a host-side proxy address, drops for metadata/RFC1918/link-local, and no general masquerade rule. The sandbox cannot tamper with this because the rules live in the host kernel outside the guest.
- The identical guest IP per clone means a shared proxy cannot identify a VM by guest IP; use the per-VM veth address or a per-netns proxy listener as identity.
- Firecracker is not a rootless option in the same sense as Podman: tap creation, netns creation and nft rules need privilege, so a "rootless preferred" scheduler needs a small privileged helper (or pre-provisioned pool of netns/tap devices) if it takes the microVM path.
- Because the Firecracker process is an ordinary host process, the same cgroup v2 files from section 1 (`cpu.max`, `memory.max`, `pids.max`, `io.max`) bound the VMM plus guest; guest memory is fixed by machine config so `memory.max` mainly protects against VMM overhead.

### Gaps
- Rate limiter units (bytes for bandwidth `size`, milliseconds for `refill_time`) are defined in the OpenAPI spec, which was not read.
- No source read for MMDS configuration, jailer cgroup v2 flags (`--cgroup-version 2`), or vhost-user/virtio-net specifics.
- How E2B, Fly.io or others actually wire Firecracker networking in production was not found in primary sources during this research.

---

## 7. How sandbox products expose and implement network policy

### Takeaway
Every provider reviewed offers the same three tiers (allow-all, deny-all, allowlist), most now support hostname allowlists implemented by SNI/Host inspection, several allow live policy updates, and the newer ones (Vercel, Cloudflare, E2B beta, Claude Code) fold credential injection into the same egress proxy. Defaults are mostly allow-all, which is the wrong default for untrusted code.

### Cited Findings

E2B
- `allowInternetAccess` / `allow_internet_access`: boolean at creation, default enabled; false equals denying `0.0.0.0/0`. `network.allowOut`/`allow_out` accepts IPs, CIDRs or domains (wildcards like `*.mydomain.com`); `network.denyOut`/`deny_out` accepts IPs/CIDRs only. Allow rules take precedence over deny rules. — [E2B internet access](https://docs.e2b.dev/sandbox/internet-access)
- Domain allowlisting requires denying all in `denyOut`; `allowOut: ['*']` without it returns HTTP 400. Blocked TCP connections can appear to succeed from inside the sandbox because the firewall accepts before deciding. — [E2B internet access](https://docs.e2b.dev/sandbox/internet-access)
- `updateNetwork`/`update_network` replaces the egress configuration on a running sandbox; `network.rules` (public beta) supports per-host transforms such as injecting headers into intercepted HTTPS requests, and the host must also be in `allowOut`. — [E2B internet access](https://docs.e2b.dev/sandbox/internet-access)

Modal
- Default: outbound to any public IP. `block_network=True` drops all outbound and cannot be combined with allowlists. `outbound_cidr_allowlist=["52.0.0.0/8", "10.0.1.0/24"]` (any protocol). `outbound_domain_allowlist=["api.openai.com", "*.github.com"]` (Beta): TLS on port 443 only; `*.example.com` also matches `example.com`; non-TLS traffic to IPs not on a CIDR allowlist is blocked. The two allowlists are additive. — [Modal sandbox networking](https://modal.com/docs/guide/sandbox-networking)
- Runtime update (Alpha): `_experimental_set_outbound_network_policy` / `updateNetworkPolicy`; established connections no longer permitted are terminated; each allowlist type must have been set at creation. `proxy_traffic_via_sidecar` (Experimental) relays port-443 traffic through a sidecar. Applies to both gVisor and VM runtimes. — [Modal sandbox networking](https://modal.com/docs/guide/sandbox-networking)

Daytona
- `networkBlockAll` (`network_block_all`, CLI `--network-block-all`); `networkAllowList`: IPv4 CIDRs only, max 10 entries; `domainAllowList`: hostnames and wildcards, max 100 entries; the three are mutually exclusive at create time (400 on conflict). `outboundProxyUrl`: create-time only, http/https, host cannot be localhost or a private/loopback/link-local literal. — [Daytona network limits](https://www.daytona.io/docs/en/network-limits/)
- Tier 1 and 2 organisations have restricted access that cannot be overridden per sandbox, with an "essential services" allowlist (package registries, git hosts, AI providers, container registries); Tier 3 and 4 default to full internet and a custom list replaces the default without auto-allowing essential services. Policies can be updated on a running sandbox. — [Daytona network limits](https://www.daytona.io/docs/en/network-limits/)

Vercel Sandbox (doc last updated 2026-09-16)
- Modes `allow-all` (default), `deny-all` (blocks DNS too), and user-defined: `networkPolicy: { allow: [...] }` or `allow: { "domain": [rules] }`, plus `subnets.allow` / `subnets.deny` CIDR lists; denied ranges take precedence but only remove access an allow rule granted. An empty policy behaves as deny-all. — [Vercel Sandbox firewall](https://vercel.com/docs/vercel-sandbox/concepts/firewall)
- Wildcards replace whole DNS labels; `*.example.com` matches any depth of subdomain but not the apex. Domain and CIDR rules apply independently: code can reach any IP in an allowed range by literal IP and bypass SNI filtering. — [Vercel Sandbox firewall](https://vercel.com/docs/vercel-sandbox/concepts/firewall)
- Live updates: `sandbox.update({ networkPolicy: 'deny-all' })` / `sandbox config network-policy ... --network-policy deny-all`; documented pattern is install with internet, then lock down before running the untrusted workload. Postgres with TLS is domain-filterable; plaintext Postgres is not. — [Vercel Sandbox firewall](https://vercel.com/docs/vercel-sandbox/concepts/firewall)

Cloudflare Sandbox (guide for SDK 0.x, last updated 2026-09-30; a 1.0 migration guide exists)
- `enableInternet = false` blocks public internet at start; `allowedHosts` makes the sandbox deny-by-default; `deniedHosts` accepts hosts or CIDRs (e.g. `"141.101.64.0/18"`); `interceptHttps = true` is needed for these to apply to HTTPS (default false means lists and handlers apply only to HTTP), using an ephemeral CA written to `/etc/cloudflare/certs/cloudflare-containers-ca.crt`. — [Cloudflare Sandbox outbound traffic](https://developers.cloudflare.com/sandbox/guides/outbound-traffic/)
- `outbound` (catch-all) and `outboundByHost` handlers run in the Workers runtime outside the sandbox; runtime methods include `setAllowedHosts()`, `setDeniedHosts()`, `allowHost()`, `denyHost()`, `setOutboundByHost()`. Precedence: `deniedHosts`, then `allowedHosts`, then instance and class handlers. Only HTTP/HTTPS are intercepted; with internet disabled other ports are denied. Local dev uses a sidecar in the sandbox's netns with TPROXY rules. — [Cloudflare Sandbox outbound traffic](https://developers.cloudflare.com/sandbox/guides/outbound-traffic/)

Anthropic sandbox-runtime (`@anthropic-ai/sandbox-runtime`, "Beta Research Preview", notes reference v0.0.76 to v0.0.79+)
- Config: `network.allowedDomains` (empty = no network; wildcards and optional `:port`), `deniedDomains` (checked first), `allowUnixSockets` (ignored on Linux; seccomp cannot filter by path), `allowAllUnixSockets`, `allowLocalBinding`, `deniedResolvedAddresses`, plus `tlsTerminate`, `filterRequest`, `requireHostMatch`, `refuseOpaqueTunnels`, `httpProxyPort`, `parentProxy`. — [anthropic-experimental/sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime)
- Linux dependencies: bubblewrap (0.4.0+), socat, ripgrep; pre-built seccomp helper for x64/arm64. Ubuntu 24.04+ `kernel.apparmor_restrict_unprivileged_userns` breaks bubblewrap unless disabled or an AppArmor profile is added. — [anthropic-experimental/sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime)

Claude Code
- Local Bash sandbox: no direct route out; a proxy outside the sandbox checks each host against `sandbox.network.allowedDomains` (starts empty) and `deniedDomains`; commands are pointed at it via `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`; an entry with no port allows every port on that host. `strictAllowlist` denies instead of prompting; `allowManagedDomainsOnly` restricts to managed-settings entries. Linux/WSL2 use bubblewrap and socat. — [Claude Code sandboxing](https://code.claude.com/docs/en/sandboxing)
- `enableWeakerNestedSandbox` is needed when bubblewrap cannot mount a fresh `/proc` inside an unprivileged container, and "considerably weakens security". — [Claude Code sandboxing](https://code.claude.com/docs/en/sandboxing)
- Cloud sessions: each runs in an isolated Anthropic-managed VM; access levels None, Trusted (default; allowlisted package registries, GitHub, cloud SDKs), Full, Custom (own domain list, `*.` prefix matches subdomains). All outbound traffic passes through an HTTP/HTTPS security proxy that also does rate limiting and abuse prevention. Even at None the session can reach the Anthropic API, "which may allow data to exit the VM". — [Claude Code cloud environments](https://code.claude.com/docs/en/cloud-environments); [Claude Code on the web](https://code.claude.com/docs/en/claude-code-on-the-web)

OpenAI Codex
- Linux and WSL2 sandboxing uses bubblewrap (first `bwrap` on `PATH`, else a bundled helper needing unprivileged user namespaces; Ubuntu 24.04 may need an AppArmor profile). `sandbox_mode` values: `read-only`, `workspace-write`, `danger-full-access`; `approval_policy` values `on-request`, `never`. In ChatGPT Work, a "Work network access" setting restricts commands to a managed hostname allowlist when "Allow public internet access" is off. — [Codex sandboxing docs](https://learn.chatgpt.com/docs/sandboxing) (redirect target of developers.openai.com/codex/sandboxing)

Kubernetes / Cilium
- Cilium FQDN policy details are in section 5. — [Cilium DNS-based policy](https://docs.cilium.io/en/stable/security/dns/)

### Inferences
- A scheduler API that matches market convention would expose: `network: "none" | "allowlist" | "full"`, `allow_domains[]` (label-wise wildcards, explicit apex), `allow_cidrs[]`, `deny_cidrs[]` (deny wins, default-populated with the special-purpose ranges), and a live `update_network_policy()` so that "install dependencies, then lock" is one sandbox rather than two.
- Vendors disagree on wildcard semantics (Modal: `*.example.com` includes the apex; Vercel and Cilium: it does not) and on precedence (E2B: allow beats deny; Vercel, Cloudflare, sandbox-runtime: deny beats allow). Pick one and document it; deny-wins is the safer choice for metadata/RFC1918 blocks.
- E2B's "connection appears to succeed" behaviour and Modal's "terminated established connections" indicate transparent in-path proxies/firewalls outside the guest rather than in-guest agents, consistent with enforcement the sandbox cannot tamper with.
- Default-open is common commercially; Claude Code (empty allowlist locally, Trusted list in cloud) and Daytona's lower tiers are the examples of restrictive defaults.

### Gaps
- Codex details not confirmed from the pages read: Landlock/seccomp use on Linux, the `sandbox_workspace_write.network_access` key, and Codex cloud internet-access phases/presets/HTTP-method limits. The Codex doc URL redirected to a different domain (learn.chatgpt.com); treat it as lower-confidence.
- E2B, Modal, Daytona and Vercel implementation internals (nftables vs eBPF vs userspace proxy) are not documented on the pages read.
- Fly.io Machines/Sprites network policy docs and Kubernetes NetworkPolicy docs were not read.
- Cloudflare Sandbox SDK 1.0 API (which supersedes the 0.x guide read here) was not reviewed.

---

## 8. Secret injection without credentials entering the sandbox

### Takeaway
The standard design is a TLS-terminating egress proxy outside the sandbox that swaps a placeholder (or adds a header) for the real credential only on requests to an allowlisted host; it requires a per-sandbox CA trusted inside the sandbox and breaks certificate-pinned clients. The credential's destination scope matters as much as hiding it, because a sandbox that can send the authenticated request anywhere can still abuse or reflect it.

### Cited Findings
- Vercel "credentials brokering": per-domain `transform: [{ headers: { "Authorization": "Bearer ..." } }]` rules inject headers at the firewall; "The secrets never enter the sandbox". TLS is terminated only for domains with `transform` or `forwardURL` rules, using a unique per-sandbox CA added to system certificates with standard env vars set. Matchers (path, method, query, headers) select which requests are transformed but never block; unmatched requests go through without the credential. — [Vercel Sandbox firewall](https://vercel.com/docs/vercel-sandbox/concepts/firewall)
- Vercel `forwardURL` sends matching requests to a customer proxy with `vercel-forwarded-host/scheme/port/path` and a `vercel-sandbox-oidc-token` whose `aud` is the forward URL and which carries `team_id`, `project_id`, `sandbox_id`, `sandbox_name`; this is the documented way to restrict a domain to specific paths. — [Vercel Sandbox firewall](https://vercel.com/docs/vercel-sandbox/concepts/firewall)
- Cloudflare: outbound handlers run in the Worker and can set auth headers before `fetch`; the secret stays in the Worker's `env`; `ctx.containerId` allows per-sandbox credentials (e.g. `env.KEYS.get(ctx.containerId)`); rotation takes effect on the next request. — [Cloudflare Sandbox outbound traffic](https://developers.cloudflare.com/sandbox/guides/outbound-traffic/)
- E2B `network.rules` (public beta) injects headers into intercepted HTTPS requests per host. — [E2B internet access](https://docs.e2b.dev/sandbox/internet-access)
- Claude Code credential masking: sandboxed commands see a per-session placeholder ("sentinel") and the proxy substitutes the real value on outbound requests to allowed hosts; requires `network.tlsTerminate`; each `mask` entry can list `injectHosts`, and with none the value is substituted on every host in `allowedDomains`. Without TLS termination masking fails closed (the sentinel reaches the server and auth fails). Mask settings are honoured only from user/managed settings, not from a repository's settings. — [Claude Code sandboxing](https://code.claude.com/docs/en/sandboxing)
- Claude Code AWS: mask `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` together; the proxy detects the SigV4 request by the access key's sentinel and re-signs with real values (v2.1.221+). Streaming uploads, presigned URLs and SigV4A cannot be re-signed and are failed by the proxy. — [Claude Code sandboxing](https://code.claude.com/docs/en/sandboxing)
- Claude Code cloud: a dedicated GitHub proxy keeps real GitHub credentials outside the VM; the git client uses a scoped credential that the proxy verifies and swaps for the real token; `GH_TOKEN`/`GITHUB_TOKEN` read as the placeholder string `proxy-injected` inside the session. "Network secrets" (Pro/Max) are attached by the agent proxy to requests for listed hosts after the request leaves the VM. — [Claude Code cloud environments](https://code.claude.com/docs/en/cloud-environments)
- Fly.io Tokenizer: stateless HTTP proxy; clients hold secrets sealed to the proxy's public key and send them in a `Proxy-Tokenizer` header; the proxy decrypts and injects (e.g. `Authorization: Bearer`) and upgrades to HTTPS upstream. Secrets can carry `allowed_hosts`/`allowed_host_pattern` and a `bearer_auth` digest so only a client presenting the matching `Proxy-Authorization: Bearer` token can use them. Processors include `inject_processor`, `inject_hmac_processor`, `sigv4_processor`, `jwt_processor`, `client_credentials_processor`, `github_app_processor`. — [superfly/tokenizer](https://github.com/superfly/tokenizer)
- Tokenizer limitations: client-to-proxy is plain HTTP so it needs a private channel; without host allowlists a compromised client can extract a secret via an echo service; it should not have a public IP because it forwards to private addresses. — [superfly/tokenizer](https://github.com/superfly/tokenizer)
- sandbox-runtime: hosts in `tlsTerminate.excludeDomains` are tunnelled opaquely so `filterRequest` and credential injection do not apply (needed for mTLS and pinning clients). — [anthropic-experimental/sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime)
- Smokescreen's mTLS client identity is one way to bind a proxy policy to a specific caller. — [stripe/smokescreen](https://github.com/stripe/smokescreen)

### Inferences
- Design rules that follow from the above: (1) bind every credential to an exact host list and prefer exact hosts over wildcards; (2) terminate TLS for those hosts and pin the outgoing `Host`/SNI so fronting cannot redirect the credential; (3) inject on a default-deny allowlist so unmatched requests are refused rather than forwarded unauthenticated; (4) strip credential-bearing response headers and avoid upstreams that echo request headers; (5) use short-lived, narrowly scoped upstream tokens so a reflected token has limited value; (6) authenticate the sandbox to the proxy with an unforgeable per-sandbox identity.
- For a rootless Podman build this is the same proxy container as the egress allowlist in section 4/5: generate a per-sandbox CA, mount only the CA certificate into the sandbox, set `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`/`NODE_EXTRA_CA_CERTS`, and keep the CA private key and real secrets in the proxy container.
- The sealed-token approach (Tokenizer) suits cases where the proxy must be stateless or shared across tenants; the placeholder-substitution approach (Claude Code, Vercel) is simpler when the scheduler already holds per-sandbox state.

### Gaps
- No documentation was read for Envoy `credential_injector` or mitmproxy add-on based injection, so no configuration is given for them.
- Details of the Claude Code GitHub proxy's push restrictions (branch scoping) were not captured from the page.
- No independent security evaluation of any of these injection proxies was found; all descriptions are vendor-authored.
