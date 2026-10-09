# Isolation technologies for executing untrusted LLM-generated Python/Bash on Linux

Research date: 2026-10-09. Scope: Linux hosts, short-to-medium-lived agent code execution.

Reading notes for the report writer:
- "Fetched" means I read the page itself. "Search summary" means the claim came from a search-result digest and the primary page was not opened; treat those as lower confidence.
- Several widely repeated numbers (125 ms, 5 MiB) have precise conditions attached. They are stated with their conditions below; do not quote them without those conditions.
- Anything I could not source is in a Gaps list, not in Cited Findings.

## 1. What rootless Podman protects against, what it does not, and the hardening layers required on top

### Takeaway
Rootless Podman removes the "root daemon" and "container root equals host root" failure modes and blunts most runtime (runc/crun) bugs, but every syscall from the sandboxed code still lands on the shared host kernel, and the user namespaces that make rootless possible also widen the reachable kernel attack surface. It is a defence-in-depth configuration, not a hostile multi-tenant boundary by itself.

### Cited Findings

What rootless buys you
- runc's own November 2025 advisory text says rootless containers limit the impact of runc bugs: for CVE-2025-52881 the privilege escalation "is entirely mitigated with rootless containers because the unprivileged user that the runc process is executing as cannot write to the aforementioned procfs files" (search summary of the advisory mirrored on Docker Scout) — [Docker Scout CVE-2025-52881](https://scout.docker.com/v/CVE-2025-52881)
- The same disclosure lists as mitigations (a) user namespaces with host root not mapped into the container, because procfs file permissions are Unix DAC, and (b) not running as root inside the container, including disabling setuid binaries via noNewPrivileges (search summary of the oss-security post) — [oss-security 2025-11-05](https://www.openwall.com/lists/oss-security/2025/11/05/3)
- Podman `--userns` modes (fetched): `auto` creates a unique user namespace per container using the caller's /etc/subuid and /etc/subgid ranges; `keep-id` maps the caller's UID:GID to the same values inside; `nomap` creates a namespace where the rootless user's UID:GID are not mapped into the container at all (rootless only); `host` runs in the caller's user namespace. Default resolution order is `PODMAN_USERNS`, then containers.conf `userns`, then `host` — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- Rootless prerequisites (fetched): a shadow-utils providing `newuidmap`/`newgidmap`, and entries for the user in /etc/subuid and /etc/subgid; fuse-overlayfs is recommended, otherwise storage falls back to the slower, disk-hungry vfs driver; default rootless networking is pasta — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)

What it does not protect against (shared kernel)
- The Firecracker NSDI paper states the structural problem: "In Linux containers, untrusted code calls the host kernel directly, possibly with the kernel surface area restricted (such as with seccomp-bpf). It also interacts directly with other services provided by the host kernel, like filesystems and the page cache." (fetched PDF, dated 2020) — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- The same paper names the trade-off: limiting syscalls improves security "at the cost of breaking code which requires the restricted calls" (fetched, dated 2020) — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- User namespaces expand reachable kernel surface. A vendor article (Edera, published 2026-04-07, fetched) reports that from an unprivileged container its probe reached 8 of 40 tested kernel operations without user namespaces and 27 with them, and cites 40+ kernel CVEs from 2020-2025 that it says required user namespaces, 18 of them in nf_tables. Vendor-run measurement on one EKS cluster; Edera sells an isolation product — [Edera: User Namespaces Are Not a Security Boundary](https://edera.dev/stories/user-namespaces-are-not-a-security-boundary)
- A directly relevant operational tension (fetched): Red Hat's workaround for the 2026 IPv6 fragmentation escape CVE-2026-53362 is `user.max_user_namespaces=0`, and Red Hat warns this "breaks rootless Podman" and some application sandboxes — [Red Hat RHSB-2026-009](https://access.redhat.com/security/vulnerabilities/RHSB-2026-009)

Hardening knobs and their documented defaults
- Default capability set in containers.conf (fetched): CHOWN, DAC_OVERRIDE, FOWNER, FSETID, KILL, NET_BIND_SERVICE, SETFCAP, SETGID, SETPCAP, SETUID, SYS_CHROOT. `--cap-drop=all` removes all of them — [containers.conf(5)](https://raw.githubusercontent.com/containers/common/main/docs/containers.conf.5.md), [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- Default seccomp profile path is /usr/share/containers/seccomp.json; `--security-opt seccomp=profile.json` supplies a custom filter and `seccomp=unconfined` disables it (fetched) — [containers.conf(5)](https://raw.githubusercontent.com/containers/common/main/docs/containers.conf.5.md), [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--security-opt no-new-privileges` stops processes gaining privileges through execve (setuid/setgid bits, file capabilities) (fetched) — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- Default masked paths (fetched): /proc/acpi, /proc/kcore, /proc/keys, /proc/latency_stats, /proc/sched_debug, /proc/scsi, /proc/timer_list, /proc/timer_stats, /sys/firmware, /sys/fs/selinux, /sys/devices/virtual/powercap. Default read-only paths: /proc/asound, /proc/bus, /proc/fs, /proc/irq, /proc/sys, /proc/sysrq-trigger, /sys/fs/cgroup. `--security-opt mask=` adds paths; `unmask=ALL` removes the protection — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--read-only` mounts the rootfs read-only; `--read-only-tmpfs` defaults to true and then mounts writable tmpfs on /dev, /dev/shm, /run, /tmp, /var/tmp (fetched) — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- pids limit default: podman-run(1) says 2048; containers.conf(5) says `pids_limit` default 1024. The two official docs disagree; verify with `podman info` / a test container on the target version — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html); contradicted by [containers.conf(5)](https://raw.githubusercontent.com/containers/common/main/docs/containers.conf.5.md)
- `--memory` and `--cpus` default to unlimited and are "not supported on cgroups V1 rootless systems" (fetched) — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- `--network none` creates a network namespace with no interfaces configured (fetched) — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
- AppArmor: default profile name is "container-default" (fetched). The runc November 2025 disclosure says an AppArmor profile that blocks unexpected writes to masked paths (as the default Docker/Podman profile does) blocks CVE-2025-31133, while SELinux is not a reliable fallback once CVE-2025-52881 (LSM label bypass) is considered (search summary) — [containers.conf(5)](https://raw.githubusercontent.com/containers/common/main/docs/containers.conf.5.md), [oss-security 2025-11-05](https://www.openwall.com/lists/oss-security/2025/11/05/3)
- SELinux: `--security-opt label=disable` turns off SELinux separation for the container; `label=type:` / `label=level:` select type and MCS level (fetched) — [podman-run(1)](https://docs.podman.io/en/latest/markdown/podman-run.1.html)

### Inferences
- A defensible hardened rootless baseline, assembled from the documented flags above, is roughly: dedicated unprivileged host user per pool (ideally per tenant) with its own subuid/subgid range; `--userns=auto` or `--userns=nomap`; `--cap-drop=all`; `--security-opt no-new-privileges`; a custom seccomp profile tighter than the default; `--read-only` with size-limited tmpfs; `--network none` (or a filtered egress proxy); `--pids-limit`, `--memory`, `--cpus` set explicitly (requires cgroup v2 with delegation); default masked paths left intact; AppArmor or SELinux enforcing; non-root user inside the container. This is my synthesis, not a single published recipe.
- The custom seccomp profile matters more than usual under rootless because the container can obtain namespaced CAP_NET_ADMIN / CAP_SYS_ADMIN by creating nested user and network namespaces, which is the path most 2022-2026 kernel escapes used (see section 2). Blocking `unshare`/`clone` with CLONE_NEWUSER, and socket families the workload does not need (AF_ALG, AF_NETLINK netfilter, AF_RXRPC, and arguably AF_UNIX SCM_RIGHTS is not filterable), is the highest-value addition for a Python/Bash workload. This is an inference from the CVE prerequisites, not a tested configuration.
- `--userns=auto` gives each container a distinct UID range, so a kernel-level file-permission bypass or a runtime escape from container A does not land as the same UID that owns container B's files. One host user for the whole pool with `keep-id` loses that property.
- Rootless cannot use `user.max_user_namespaces=0` as an emergency mitigation, so a rootless-Podman fleet is structurally more dependent on fast kernel patching than a rootful, userns-disabled one.

### Gaps
- I did not fetch the containers/common seccomp.json itself, so I cannot state the current count of allowed syscalls or confirm which of `unshare`, `clone3`, `keyctl`, `bpf`, `userfaultfd`, `io_uring_*` it allows without capabilities. This must be read from the installed profile.
- No fetched primary source for the default rootless capability *bounding* behaviour versus rootful, or for whether rootless AppArmor profile loading works on Ubuntu 24.04/26.04 with its unprivileged-userns AppArmor restriction.
- No source found quantifying rootless start-up latency (pasta, fuse-overlayfs vs native overlay) versus rootful Podman.

## 2. Container-escape and kernel CVEs 2019-2026, and which ones rootless or user namespaces would have mitigated

### Takeaway
There are two distinct classes. Runtime bugs (runc file-descriptor, symlink and procfs races) are largely blunted by rootless and unmapped-root user namespaces. Kernel bugs are not, and a large share of them are reachable *only because* unprivileged user namespaces exist. 2026 has been an unusually bad year for the second class, with at least two bugs in CISA's KEV catalogue.

### Cited Findings

Runtime-level (runc)
- CVE-2024-21626 "Leaky Vessels" (fetched Snyk write-up, 2024-01-31): runc "process.cwd and leaked file descriptors container breakout", triggered via WORKDIR by running a malicious image or building from a malicious Dockerfile/base image; fixed in runc 1.1.12. Three companion BuildKit CVEs (CVE-2024-23651 mount cache race, CVE-2024-23653 GRPC SecurityMode privilege check, CVE-2024-23652 build-time teardown arbitrary delete) fixed in BuildKit 0.12.5; Moby 25.0.2 / 24.0.9 — [Snyk Labs: Leaky Vessels](https://labs.snyk.io/resources/leaky-vessels-docker-runc-container-breakout-vulnerabilities/)
- November 2025 runc trio (search summaries of the oss-security disclosure): CVE-2025-31133 (masked-path handling; container /dev/null replaced by a symlink), CVE-2025-52565 (/dev/console bind-mount of /dev/pts/$n after pivot_root; breakout or host DoS via writable /proc/sysrq-trigger or /proc/sys/kernel/core_pattern), CVE-2025-52881 (a more sophisticated variant of CVE-2019-19921 that redirects procfs writes and bypasses LSM labels). All allow writes to arbitrary host /proc files. Fixed in runc 1.2.8, 1.3.3, 1.4.0-rc.3 — [oss-security 2025-11-05](https://www.openwall.com/lists/oss-security/2025/11/05/3), [seclists oss-sec 2025 Q4](https://seclists.org/oss-sec/2025/q4/155)
- Mitigation by rootless/userns for the 2025 trio (search summaries): user namespaces with host root unmapped and non-root-in-container both stop the procfs writes because those files are DAC-protected; CVE-2025-52881's escalation is "entirely mitigated" with rootless containers — [oss-security 2025-11-05](https://www.openwall.com/lists/oss-security/2025/11/05/3), [Docker Scout CVE-2025-52881](https://scout.docker.com/v/CVE-2025-52881)
- CVE-2026-41579 (search summary): malicious image with a /dev symlink causes limited host filesystem integrity violations; fixed in runc 1.3.6; SUSE rates it low, Tenable Medium 4.8. One aggregator (vulert) labels it "critical", which conflicts with the vendor ratings — [SUSE advisory via LinuxSecurity](https://linuxsecurity.com/advisories/suse/suse-2026-3433-1-runc), [Tenable](https://www.tenable.com/plugins/cloud-security/443778); contradicted on severity by [vulert](https://vulert.com/vuln-db/--4780)

Kernel-level, historical
- Dirty Pipe, CVE-2022-0847 (search summary): lets a local user write into files they can only read; affects kernels 5.8 up to but excluding 5.16.11 / 5.15.25 / 5.10.102. JFrog says it does not normally escape a container except where host files are mounted read-only into it, and it cannot write to device files or pseudo-filesystems — [JFrog: DirtyPipe](https://jfrog.com/blog/dirtypipe-cve-2022-0847-the-new-dirtycow)
- Palo Alto Networks reports that malicious containers can use Dirty Pipe to overwrite their image on the underlying host and that its researchers built an image that breaks out when run (details withheld). Public PoCs overwrite the runc binary through the container entrypoint, modelled on CVE-2019-5736 (search summary) — [Palo Alto Networks: DirtyPipe](https://www.paloaltonetworks.com/blog/cloud-security/dirty-pipe-linux-vulnerability.md), [PoC repo](https://github.com/JlSakuya/CVE-2022-0847-container-escape)
- CVE-2022-0185 (search summary): heap overflow in `legacy_parse_param` (fsconfig), reachable from Linux 5.1-rc1; needs CAP_SYS_ADMIN only within the current namespace, which an unprivileged process obtains with `unshare` CLONE_NEWNS|CLONE_NEWUSER. Mitigations: kernel 5.16.2 or later, disable unprivileged user namespaces; CrowdStrike presents a seccomp profile as a control protecting namespace boundaries — [CrowdStrike](https://www.crowdstrike.com/en-us/blog/cve-2022-0185-kubernetes-container-escape-using-linux-kernel-exploit/), [Aqua](https://aquasec.com/blog/cve-2022-0185-linux-kernel-container-escape-in-kubernetes)
- CVE-2024-1086 (nf_tables double-free in verdict handling) and CVE-2023-32233 (nf_tables use-after-free in anonymous sets) are cited by Edera as container-escape-capable bugs whose exploit chains require user namespaces; CVE-2023-2640 (Ubuntu overlayfs) is in the same dataset (fetched vendor article; the per-CVE userns dependency is the vendor's own classification) — [Edera](https://edera.dev/stories/user-namespaces-are-not-a-security-boundary)

Kernel-level, 2026 (beyond my training data; all from 2026 sources)
- CVE-2026-53362 "ipv6_frag_escape" (fetched primary): out-of-bounds write into skb_shared_info from an incorrect length calculation in `__ip6_append_data()`, chainable to arbitrary kernel read/write, credential overwrite, SELinux bypass and container escape. Red Hat: "A user with local access inside a container could escape to the host, bypassing SELinux enforcement, and gain system administrator (root) access." Requires the ability to create network namespaces, which default RHEL 10 grants to unprivileged users via user namespaces. Affects RHEL 10; published 2026-07-01 — [Red Hat RHSB-2026-009](https://access.redhat.com/security/vulnerabilities/RHSB-2026-009)
- CVE-2026-53362 was added to CISA KEV on 2026-08-27 after evidence of active exploitation (search summary) — [Gridinsoft](https://blog.gridinsoft.com/cve-2026-53362-linux-container-escape/)
- CVE-2026-31431 "Copy Fail" (AF_ALG / algif_aead): breaks container isolation on shared kernels; added to CISA KEV 2026-05-01 (search summary). A secondary article describes it as needing only an unprivileged local account, with no race or kernel-specific offset — [Wiz vulnerability DB](https://www.wiz.io/vulnerability-database/cve/cve-2026-31431), [simeononsecurity, 2026-09-28](https://simeononsecurity.com/articles/containers-no-longer-security-boundary-2026/)
- "Dirty Frag" CVE-2026-43284 (XFRM ESP; needs user namespace creation) and CVE-2026-43500 (RxRPC; no namespace privilege needed but the rxrpc module must be present, loaded by default on Ubuntu), and "Fragnesia" CVE-2026-46300 (XFRM ESP-in-TCP; needs unprivileged user plus network namespaces for CAP_NET_ADMIN; introduced by the Dirty Frag patch) (fetched secondary article) — [simeononsecurity, 2026-09-28](https://simeononsecurity.com/articles/containers-no-longer-security-boundary-2026/)
- CVE-2026-80521 (AF_UNIX SCM_RIGHTS garbage-collection use-after-free): upstream fix 2026-08-06; DepthFirst published research and an exploit against Ubuntu 26.04 LTS on 2026-09-22; Ubuntu's tracker still listed 26.04 and 24.04 kernels as vulnerable on 2026-09-23; not in KEV at that time (search summary). The secondary article says it also defeats nsjail, Firejail and Bubblewrap — [Linux Journal](https://www.linuxjournal.com/node/1341464), [simeononsecurity, 2026-09-28](https://simeononsecurity.com/articles/containers-no-longer-security-boundary-2026/)
- CVE-2026-43499 "GhostLock" (futex priority-inheritance use-after-free, described as a 15-year-old flaw): researchers demonstrated container breakout (search summary) — [Cloud Security Alliance research note](https://labs.cloudsecurityalliance.org/research/csa-research-note-ghostlock-linux-kernel-container-escape-20/)
- CVE-2026-23111 (nftables use-after-free, LPE to root): patched upstream 2026-02-05, triggered from within a network namespace that unprivileged users can create; Exodus Intelligence PoC published 2026-06-09 (search summary, aggregator source) — [threatcluster](https://threatcluster.io/cluster/linux-kernel-cve-2026-23111-enables-local-privilege-escalati-b2264acb)

### Inferences
Mitigation matrix (my synthesis from the prerequisites above; "userns" here means unprivileged user-namespace creation being available to the sandboxed code):

| CVE | Class | Rootless / unmapped root helps? | Disabling nested userns or seccomp-blocking it helps? | A VM / gVisor boundary helps? |
|---|---|---|---|---|
| CVE-2019-5736 runc /proc/self/exe | runtime | Yes (recalled, unverified; see Gaps) | n/a | Yes |
| CVE-2024-21626 Leaky Vessels | runtime | Partly: escape lands as the unprivileged host user rather than root (inference; Snyk does not address it) | n/a | Yes |
| CVE-2025-31133 / 52565 / 52881 | runtime | Yes per the advisory | n/a | Yes |
| CVE-2022-0847 Dirty Pipe | kernel | No (page-cache write primitive is independent of UID mapping); rootless limits what the overwritten file can do | No | Yes |
| CVE-2022-0185, CVE-2024-1086, CVE-2023-32233 | kernel | No; rootless makes the prerequisite present by design | Yes | Yes |
| CVE-2026-53362, 43284, 46300, 23111 | kernel | No | Yes (namespace creation is the stated prerequisite) | Yes |
| CVE-2026-31431, 43500, 80521, 43499 | kernel | No | No (no namespace prerequisite reported); only socket-family seccomp filtering or module blocklisting could help, and futex / AF_UNIX cannot realistically be filtered for Python | Yes |

- The last row is the important one for the build plan: AF_UNIX and futex are syscalls CPython cannot run without, so no seccomp profile compatible with "run arbitrary Python and Bash" closes those bugs. Only removing the shared kernel does.
- Cadence observation: the sources above list at least eight container-escape-capable kernel bugs with public exploits or KEV listings between February and September 2026. A shared-kernel sandbox for hostile code therefore needs a kernel patch SLA measured in days and a plan for the window before distro kernels ship (CVE-2026-80521 had a public exploit while Ubuntu LTS kernels were still unpatched).

### Gaps
- CVE-2019-5736: I could not fetch a primary advisory (the GHSA URL I tried returned 404). From memory, the bug overwrote the host runc binary via /proc/self/exe, was fixed in runc 1.0-rc7, and the advisory stated that user namespaces with host root unmapped block it. This is unverified in this session; cite NVD or the runc advisory after checking.
- The Snyk page does not say whether rootless or user namespaces mitigate CVE-2024-21626, and I did not fetch the runc GHSA for it.
- The 2026 kernel CVEs other than CVE-2026-53362 rest on secondary articles and search digests. Affected-version ranges, whether Podman's default seccomp profile blocks AF_ALG / AF_RXRPC / XFRM netlink, and KEV status should be confirmed against NVD, CISA KEV and distro trackers before the report states them as fact.
- The simeononsecurity article also mentions CVE-2026-52910 (second Ubuntu 24.04 exploit) with no details.
- I found no crun-specific escape CVE list; whether crun was affected by analogues of the November 2025 runc bugs was not researched.

## 3. Firecracker: boot time, memory overhead, snapshot/restore, the jailer, hosting requirements

### Takeaway
Firecracker gives each sandbox its own guest kernel behind KVM with a very small VMM, at roughly 125 ms to guest init and under 5 MiB of VMM overhead under specified conditions, and full-snapshot restore makes pre-warming practical. The costs are a hard dependency on /dev/kvm (bare metal or nested virtualization), owning the guest kernel/rootfs/network plumbing yourself, and snapshot-uniqueness hazards.

### Cited Findings

Boot and overhead (with conditions)
- Firecracker SPECIFICATION.md (fetched; measured on M5D.metal with hyperthreading disabled, or M6G.metal): boot is 125 ms or less from the InstanceStart API call to the start of guest `/sbin/init`, with serial console disabled and a minimal kernel and rootfs; VMM startup to API socket within 8 CPU ms, wall-clock 6-60 ms, typically about 12 ms; VMM memory overhead 5 MiB or less for a 1 vCPU / 128 MiB microVM with a Firecracker-tuned guest kernel (excludes MMDS data store; workload such as many vsock connections can raise it); guest CPU above 95% of bare metal — [Firecracker SPECIFICATION.md](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/SPECIFICATION.md)
- I/O envelope from the same spec: network up to 14.5 Gbps at 80% or less of one dedicated host core, 25 Gbps at 100%, about 0.06 ms added latency; storage up to 1 GiB/s at 70% or less of one host core (integration tests "pending") — [Firecracker SPECIFICATION.md](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/SPECIFICATION.md)
- NSDI 2020 paper (fetched PDF; DATED: Firecracker v0.20.0, QEMU 4.2.0, guest kernel 4.14.94, m5d.metal): "memory overhead of less than 5MB per container, boots to application code in less than 125ms, and allows creation of up to 150 MicroVMs per second per host" — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Measured VMM memory overhead in that paper: Firecracker about 3 MB, Cloud Hypervisor about 13 MB, QEMU about 131 MB per microVM (DATED 2020) — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Boot-time details in that paper (DATED 2020): pre-configured Firecracker and Cloud Hypervisor boot about twice as fast as QEMU; with 100 concurrent starts the 99th percentile for pre-configured Firecracker rose to 153 ms; a network interface adds about 20 ms for Firecracker and Cloud Hypervisor and about 35 ms for QEMU; in-guest kernel decompression adds about 40 ms; disabling serial console logging saves up to 70 ms; the recommended minimal compressed kernel is 4.0 MB with no modules versus Ubuntu 18.04's 6.7 MB plus 44 MB of modules — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Even at 125 ms, Lambda pre-warms: "While the 125ms boot times offered by Firecracker are fast, they are not fast enough for the scale-up path of Lambda", so it keeps a small local pool of ready microVMs (DATED 2020) — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Block I/O limits in 2020 (DATED; the paper said "We expect to fix both"): no flush-to-disk and serial IO handling, capping the guest at about 13,000 IOPS (52 MB/s at 4 kB) on hardware capable of over 340,000 read IOPS; network about 15 Gb/s versus about 44 Gb/s on the host tap — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- A separate 2021 academic measurement of full container start-up (not bare VM boot) reported about 1.9 s for Kata Containers and 2.4 s for Firecracker (search summary; DATED and measuring a different thing from the 125 ms figure) — [arXiv 2501.04580](https://arxiv.org/pdf/2501.04580)

Snapshot/restore (fetched, current main branch)
- Full snapshots "always create a complete, resume-able snapshot of the current microVM state and memory"; a snapshot is a guest memory file, a microVM state file and user-managed disk files, and can be loaded in a different Firecracker process — [snapshot-support.md](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/snapshotting/snapshot-support.md)
- "Diff snapshots are still in developer preview"; they are generally not resumable until merged into a base with `snapshot-editor`. Dirty-page tracking (`track_dirty_pages`) has a cost and "negates most of the benefits of huge pages"; without it Firecracker uses mincore(2), which "will only work if swap is disabled" — [snapshot-support.md](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/snapshotting/snapshot-support.md)
- Memory can be restored through a `File` backend (kernel page faults) or `Uffd` (user-space page-fault handler); explicit 2M huge pages require Uffd — [snapshot-support.md](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/snapshotting/snapshot-support.md)
- Stated limitations: "High snapshot restoration latency when cgroups V1 are in use"; "Guest network connectivity is not guaranteed to be preserved after resume"; vsock connections open at snapshot time are closed (listening sockets survive); snapshots taken in early boot may crash on resume; guest wall-clock continues from snapshot time; the memory file must stay immutable; restore requires matching software and hardware configuration, and restoring on a newer host kernel works only in limited tested cases and is not recommended for production — [snapshot-support.md](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/snapshotting/snapshot-support.md)
- Uniqueness and security: "we consider resuming execution from the same state more than once insecure". VMGenID (guest Linux 5.18+ with ACPI; 6.10 adds DeviceTree) makes the guest kernel reseed its PRNG, but application-level state such as unique IDs, cached random numbers and tokens "will still be replicated across multiple microVMs resumed from the same snapshot". Snapshot files are trusted input; Firecracker only checks a 64-bit CRC — [snapshot-support.md](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/snapshotting/snapshot-support.md)
- No restore latency figure is published in the doc; it only says performance depends on memory size, vCPU count and device count — [snapshot-support.md](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/snapshotting/snapshot-support.md)

Jailer and production host setup
- The jailer "implements an additional level of protection against unwanted VMM behavior (such as a hypothetical bug that allows the guest to inject code into the VMM)": chroot, pid and network namespaces, privilege drop, restrictive seccomp-bpf. In 2020 the profile whitelisted 24 syscalls with argument filtering and 30 ioctls, 22 of them required by KVM (DATED count) — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Current production guidance (fetched): Firecracker "should be started using the `jailer` binary"; give each instance its own UID/GID; keep the default (most restrictive) seccomp filters and do not use `--no-seccomp`; set cgroup resource limits yourself because none are aggressive by default; disable SMT where tenants must be separated; disable KSM; disable swap; apply microcode and verify hardware-vulnerability mitigations — [prod-host-setup.md](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/prod-host-setup.md)

Hosting
- AWS (fetched, posted 2026-02-16): nested virtualization on virtual (non-metal) EC2 instances, KVM and Hyper-V, on C8i, M8i and R8i, in all commercial regions; previously bare metal only — [AWS What's New 2026-02](https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual)
- AWS expanded this in June 2026 to further Intel families including C7i, M7i, R7i, I7i and X8i, and to GovCloud (search summary) — [AWS What's New 2026-06](https://aws.amazon.com/about-aws/whats-new/2026/06/nested-virtualization-intel-us-gov-cloud/)
- A February 2026 Classmethod test ran Firecracker on the new EC2 nested setup (search summary; third party, not an AWS endorsement) — [Classmethod](https://dev.classmethod.jp/en/articles/ec2-nested-virtualization-support-non-bare-metal/)
- GCP (search summary of official docs): nested virtualization supports only Linux KVM in the L1 VM; E2, memory-optimized, AMD, Arm and H4D VMs are excluded; Google warns of a 10% or greater slowdown for CPU-bound nested workloads — [GCE nested virtualization overview](https://docs.cloud.google.com/compute/docs/instances/nested-virtualization/overview)
- Fly.io has said it does not offer nested virtualization on its platform, citing security implications (search summary; community forum post, date not confirmed) — [Fly.io community](https://community.fly.io/t/nested-virtualization-on-fly-io/11778)

### Inferences
- For a pre-warmed pool the relevant latency is not the 125 ms boot but (a) time to claim an already-booted VM, or (b) snapshot restore time, neither of which Firecracker documents numerically; the team must benchmark restore on its own memory size.
- Cloning one warm snapshot into many sandboxes is exactly the pattern the docs call insecure without care. The agent runtime inside the guest must regenerate any IDs, session tokens and userspace RNG state after resume, and guest kernels should be 5.18+ for VMGenID.
- Snapshot restore latency is flagged as high on cgroup v1, so the host should be cgroup v2, which rootless Podman resource limits also require; the two back-ends share that host prerequisite.
- The nested-virtualization situation changed materially in 2026. Before February 2026 Firecracker on AWS implied .metal instances; now ordinary Intel 7th/8th-generation instances work, which lowers the entry cost for a small team. Nested performance overhead on AWS was not quantified in anything I read.
- Firecracker under the jailer requires root at setup (chroot, cgroups, tap devices), so it is not "rootless" in the Podman sense; the security argument rests on KVM plus a seccomp-confined per-VM VMM process running as a unique unprivileged UID.

### Gaps
- No current (2025-2026) published Firecracker snapshot-restore latency with methodology was found. Vendor figures for E2B, Modal and others were not retrieved from primary pages.
- Current Firecracker release number and its supported host/guest kernel matrix were not retrieved (a search result referenced a v1.16.1 docs tag, unverified as latest). prod-host-setup.md gives no formal kernel list.
- Whether the 2020 block-IO limits (no flush, serial IO) have been fixed, and the current seccomp syscall count, were not verified.
- Azure: official support for KVM-on-Linux nested virtualization is unclear in what I found; only third-party 2026 guides claim it works on specific sizes.
- Firecracker's own stance on running nested was not found in prod-host-setup.md.
- The 2021 start-up figure (2.4 s) is attributed via a search digest of an Edera paper citing earlier work; the original study and its methodology were not read.

## 4. gVisor (runsc): compatibility, performance, systrap, rootless, Podman

### Takeaway
gVisor removes direct guest access to the host kernel by reimplementing the syscall surface in a Go user-space kernel, needs no KVM when using the default systrap platform, and plugs in as an OCI runtime. Its costs are syscall-heavy and small-file I/O overhead, incomplete Linux compatibility, and a rootless-under-Podman path that is documented but thin.

### Cited Findings

Security model (fetched)
- Primary design goal is to minimise the System API attack vector; every supported syscall has an independent implementation in the Sentry and none is passed through to the host; the Sentry's own host syscalls are explicitly enumerated and it generally cannot open files or create sockets (exceptions: directfs, host networking) — [gVisor security model](https://gvisor.dev/docs/architecture_guide/security/)
- The Gofer process mediates filesystem access and hands file descriptors to the Sentry, which runs in an empty mount namespace; with directfs the Sentry accesses files directly — [gVisor security model](https://gvisor.dev/docs/architecture_guide/security/)
- Out of scope: "gVisor does not provide protection against hardware side channels"; resource exhaustion is left to host cgroups; "A sandbox is not a substitute for a secure architecture" — [gVisor security model](https://gvisor.dev/docs/architecture_guide/security/)
- Compatibility limits by design: only common functionality is implemented; specialised xattrs, raw sockets and ioctls are not passed through — [gVisor security model](https://gvisor.dev/docs/architecture_guide/security/)

Platforms
- Systrap intercepts syscalls using seccomp's trap mechanism (SIGSYS to the triggering thread), runs without virtualization, replaced ptrace as the default platform in mid-2023; ptrace is no longer supported (search summaries of official docs) — [gVisor: Releasing Systrap](https://gvisor.dev/blog/2023/04/28/systrap-release/), [gVisor platform user guide](https://gvisor.dev/docs/user_guide/platforms/)
- Systrap is the recommended platform inside a VM or without virtualization support, and often outperforms the KVM platform under nested virtualization (search summary) — [gVisor platform guide](https://gvisor.dev/docs/architecture_guide/platforms)

Performance (fetched; qualitative only)
- The official performance guide gives no numbers in text (charts only). Its statements: no overhead on memory access once mappings exist; CPU-bound work sees "minimal runtime overhead"; syscall cost is structural and platform-dependent; overhead is high for many small VFS operations and for minimal-work network requests; raw disk I/O has little fundamental overhead — [gVisor performance guide](https://gvisor.dev/docs/architecture_guide/performance/)
- That guide's benchmarks are DATED: run on the removed ptrace platform, on GCE n1-standard-4 VMs with Debian 9 and kernel 4.19. The page itself says the results are not representative and to use Systrap — [gVisor performance guide](https://gvisor.dev/docs/architecture_guide/performance/)
- A secondary blog cites a KubeBlocks benchmark: about 4% overhead on sysbench versus runc, 56-95% degradation on Redis operations depending on platform, and 125% overhead on a SQLite insert benchmark. Not verified against the original and not stated to be systrap-specific — [jacar.es](https://jacar.es/en/gvisor-sandboxing-for-multi-tenant-containers/)

Rootless and Podman (fetched)
- Three documented rootless methods. (1) `runsc --rootless`: "The `create` command is not supported", save/restore unsupported, Netstack unsupported (host network only), cgroup configuration errors ignored; suitable mainly for `runsc do`. (2) Caller-configured user namespace, where a rootless engine such as Docker or Podman sets up the userns and invokes runsc: no formal limitations listed; "Unsharing the network namespace is critical here to protect abstract UNIX sockets on the host"; Netstack usable via TAP plus a usermode stack such as Slirp; multi-UID mappings need setuid newuidmap. (3) "True" runsc rootless mode: "currently lacks support for network namespacing" — [gVisor rootless guide](https://gvisor.dev/docs/user_guide/rootless/)
- Podman storage caveat from the same page: with a single UID/GID mapping, set `ignore_chown_errors = "true"` in storage.conf — [gVisor rootless guide](https://gvisor.dev/docs/user_guide/rootless/)
- containers.conf lists runsc in the default runtime search order (crun, runc, runj, kata, runsc, ocijail) and in `runtime_supports_json` — [containers.conf(5)](https://raw.githubusercontent.com/containers/common/main/docs/containers.conf.5.md)
- Real-world rootless use: the Dangerzone project runs runsc rootless inside a Podman container using `--userns nomap` on Podman 4+, needing SYS_CHROOT in the outer container (SETFCAP later dropped) (search summary of project docs/commits) — [Dangerzone gVisor notes](https://git.notmyidea.org/almet/dangerzone/raw/branch/main/docs/developer/gvisor.md)
- Production precedent: a Modal-published guide states Modal uses gVisor containers while E2B uses Firecracker microVMs (search summary; vendor-authored) — [Modal](https://modal.com/resources/best-code-execution-sandboxes-tool-calling-ai-agents)

### Inferences
- gVisor is the only strong-isolation candidate here that needs neither /dev/kvm nor nested virtualization, which makes it the natural upgrade path for a rootless-Podman MVP running on ordinary cloud VMs.
- gVisor would have blocked most of the kernel CVEs in section 2 as far as direct reachability goes, because nf_tables, XFRM, AF_ALG, RxRPC and fsconfig are either not implemented in the Sentry or implemented independently in Go. This is an inference from the "no passthrough" principle, not a per-CVE statement by gVisor.
- For an agent workload, the overhead profile maps to: numeric or CPU-bound Python close to native; `pip install`, large `import` trees, git operations and many small file writes noticeably slower; shelling out repeatedly (fork/exec heavy Bash) slower. This is extrapolation from the qualitative guide.
- Method 2 (Podman sets up the user namespace, runsc is the OCI runtime) is the only rootless path compatible with a scheduler that drives `podman create/start`, since method 1 lacks `create`.

### Gaps
- No current, methodologically stated benchmark of gVisor systrap on Python workloads (interpreter start, import time, pip install) was found. This is the most decision-relevant missing number.
- No official statement of gVisor's sandbox start latency or per-sandbox memory overhead in figures.
- No current syscall-compatibility count was retrieved (the compatibility table page was not fetched).
- Maturity of rootless Podman plus runsc (cgroup v2 delegation with `--systemd-cgroup`, pasta networking, checkpoint/restore) is not documented beyond the short section above; a 2019 issue reported panics on Podman 1.3.2 and is too old to be informative. Needs a hands-on spike.
- gVisor checkpoint/restore as a pre-warming mechanism (which Modal reportedly uses) was not researched from primary sources.

## 5. Driving Podman with alternative OCI runtimes (runsc, kata, krun/libkrun) from one scheduler

### Takeaway
Podman's `--runtime` switch and containers.conf runtime table are real and first-class for crun, runc, runsc and krun, so a single scheduler can select isolation level per job at the OCI layer. Maturity is uneven: crun/runc are production-grade, runsc works with caveats, krun is functional but young and in API flux, and Kata under Podman is effectively unsupported.

### Cited Findings
- containers.conf (fetched): when `[engine] runtime` is empty the engine searches crun, runc, runj, kata, runsc, ocijail in that order; `runtime_supports_json` lists crun, crun-vm, runc, kata, runsc, youki, krun; `runtime_supports_kvm` lists kata and krun; `runtime_supports_nocgroups` lists crun, crun-vm, krun — [containers.conf(5)](https://raw.githubusercontent.com/containers/common/main/docs/containers.conf.5.md)
- krun (fetched): `krun` is a symlink to crun that switches it into a mode running the OCI container inside a libkrun KVM microVM; invoked as `podman run --runtime=krun`; tunables via annotations such as `krun.cpus` (max 16), `krun.ram_mib` (values under 128 ignored; default is the OCI memory limit or 1024 MiB), `krun.use_passt`, `krun.tap_name`, `krun.nested_virt`, `krun.custom_kernel`, `krun.variant` (sev, aws-nitro); a `.krun_vm.json` at the image root is also read, annotations taking precedence — [crun krun.1.md](https://github.com/containers/crun/blob/main/krun.1.md)
- crun hardened this path: an OCI bundle can no longer choose several sensitive krun settings that affect how the host creates and connects the VM (search summary) — [LinuxSecurity](https://linuxsecurity.com/features/crun-container-images-host-microvm-settings)
- libkrun maturity (search summary of the project README): main branch is libkrun 2.0, "not backwards compatible with the 1.x API/ABI" and still changing before a stable release; maintainers recommend the newest stable release branch for production. TSI networking needs the libkrunfw custom kernel and supports only SOCK_STREAM/SOCK_DGRAM over AF_INET, AF_INET6, AF_UNIX — [containers/libkrun mirror](https://upd.dev/containers/libkrun)
- A March 2026 issue reports rootless `podman run --network pasta --runtime krun` having no network with "vsock: connect filtered"; closed, resolution not visible in what I read (search summary) — [libkrun issue 569 mirror](https://upd.dev/containers/libkrun/issues/569)
- A Fedora discussion draft on running AI agents in microVMs notes pitfalls with CPU/RAM handling and an outdated libkrun in Fedora 44 (search summary; draft status) — [Fedora Discussion](https://discussion.fedoraproject.org/t/running-ai-agents-in-microvms/192323)
- Kata: its runtime is `containerd-shim-kata-v2` (shim v2 API, one shim per pod). In a 2020 Podman mailing-list thread Kata maintainers asked Podman to adopt the shim v2 API and a Podman maintainer replied no one on the team could work on it at that time (search summary; DATED 2020) — [Podman list, 2020](https://lists.podman.io/archives/list/podman@lists.podman.io/message/WINI4OP7MO7LMJHHJ7WW2PQ4F74O2WOK/), [Kata architecture doc](https://github.com/kata-containers/kata-containers/blob/51bf98073d0c2e7b83bf2320be827e0942b9316a/docs/design/architecture.md)
- runsc under a rootless engine is the gVisor project's documented "method 2" — [gVisor rootless guide](https://gvisor.dev/docs/user_guide/rootless/)

### Inferences
- A pluggable abstraction is realistic at two different seams, and the report should distinguish them:
  1. OCI-runtime seam (cheap): the scheduler always talks to Podman and varies `--runtime` among crun, runsc and krun. Image handling, rootfs, cgroups and lifecycle verbs stay identical. Covers "namespaces", "user-space kernel" and "libkrun microVM" tiers.
  2. Backend seam (expensive): a Firecracker backend is not an OCI runtime for Podman. It needs its own driver (API socket, jailer, tap/network setup, rootfs block images built from OCI images, guest agent over vsock, snapshot store). The scheduler interface must be defined above Podman (for example create / exec / copy-in / copy-out / snapshot / destroy plus a capability descriptor) for this to slot in.
- The abstraction leaks in predictable places: networking (pasta vs Netstack vs TSI vs tap), pause/checkpoint semantics (CRIU for crun, gVisor checkpoint, VM snapshot), resource-limit enforcement (cgroup vs VM sizing), file injection (bind mount vs virtiofs vs block device) and exec latency. A capability descriptor per backend is more honest than pretending they are uniform.
- krun is the lowest-effort way to get a hardware-virtualized tier inside the Podman model, but with the 2.0 API churn, distro packaging lag and the open-ended rootless networking report, it is a "prototype now, harden later" option rather than something to bet a multi-tenant launch on in October 2026.
- Kata is better treated as a containerd/Kubernetes-ecosystem option; choosing it implies moving the scheduler off Podman.

### Gaps
- The podman-run(1) `--runtime` section itself was not captured in the fetched excerpts, so the exact wording on kata/runsc/krun support there is unquoted.
- No current (2025-2026) statement from either project on Kata plus Podman; the only evidence is from 2020.
- crun's krun page does not address rootless operation; rootless krun works in reports but its /dev/kvm group-permission and cgroup requirements were not sourced.
- No boot-time or memory-overhead measurements for libkrun/krun were found.
- `crun-vm` (appears in Podman's runtime lists) was not researched.

## 6. Kata Containers, Cloud Hypervisor, nsjail/bubblewrap, and WebAssembly (brief)

### Takeaway
Kata is an orchestration layer that puts OCI containers in a VM using QEMU, Cloud Hypervisor or Firecracker and fits Kubernetes better than a custom Podman scheduler; Cloud Hypervisor sits between Firecracker and QEMU on overhead and features. nsjail and bubblewrap are the same shared-kernel primitives as a container with less tooling, so they share its kernel exposure. WebAssembly is a different contract (no arbitrary Bash or native wheels) and was not researched in depth.

### Cited Findings
- Kata targets Firecracker, QEMU and Cloud Hypervisor, and the project site states it requires nested virtualization or bare metal; Kata 3.0.0 (October 2022, DATED) added a Rust runtime and the built-in Dragonball hypervisor (search summary) — [katacontainers.io software](https://www.katacontainers.io/software/), [Kata 3.0.0 announcement](https://katacontainers.io/blog/kata-containers-3-0-0-arrives-faster-more-secure-support-for-new-environments/)
- Kata start-up measurements are inconsistent across sources: about 1.9 s full container start in one 2021-era academic comparison; about 600 ms versus about 100 ms for Docker in another study; a vendor blog claims 150-300 ms depending on VMM without citing a measurement (all search summaries) — [arXiv 2501.04580](https://arxiv.org/pdf/2501.04580), [arXiv 2110.11462](https://arxiv.org/pdf/2110.11462), [Northflank](https://northflank.com/blog/what-are-kata-containers)
- Cloud Hypervisor versus Firecracker (DATED 2020): VMM overhead about 13 MB versus about 3 MB; boot time comparable to pre-configured Firecracker and about twice as fast as QEMU; slightly higher network throughput and lower write latency than Firecracker; shared the no-flush and serial block-IO limitations at that time — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- bubblewrap's README says it is not a complete ready-made sandbox with a fixed policy: "The level of protection between the sandboxed processes and the host system is entirely determined by the arguments passed to bubblewrap"; anything mounted into the sandbox can be used to escalate; without seccomp filtering TIOCSTI, `--new-session` is needed (CVE-2017-5226) (search summary) — [bubblewrap README mirror](https://github.com/BuddySirJava/bubblewrap), [ArchWiki: Bubblewrap](https://wiki.archlinux.org/title/Bubblewrap)
- nsjail is described by Google as process isolation using namespaces, resource limits and seccomp-bpf filters (search summary) — [Google code sandboxing overview](https://developers.google.com/code-sandboxing?authuser=1)
- The 2026 AF_UNIX bug CVE-2026-80521 is reported to affect nsjail, Firejail and Bubblewrap sandboxes as well as containers (fetched secondary article) — [simeononsecurity, 2026-09-28](https://simeononsecurity.com/articles/containers-no-longer-security-boundary-2026/)
- The Firecracker paper places language-VM isolation in a third category and judged it unsuitable for Lambda/Fargate given the need to run arbitrary binaries (DATED 2020) — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)

### Inferences
- nsjail/bubblewrap are attractive for very low start latency and zero daemon, and nsjail's built-in seccomp policy language (Kafel) makes tight filters easier than writing OCI JSON, but they add nothing to the security boundary beyond what a hardened rootless container already has, and they lack image, storage and lifecycle tooling the scheduler would otherwise have to write. They are best seen as an inner layer (for example inside a microVM) rather than the tenant boundary.
- Cloud Hypervisor matters mainly if the workload later needs things Firecracker deliberately omits (virtiofs, device hotplug, larger device model); for short Python/Bash jobs Firecracker's smaller surface is the better default. This is a judgment based on the dated comparison.
- WebAssembly (Pyodide/WASI) cannot satisfy an "arbitrary Bash plus pip-installed native wheels" contract; it is only an option for a restricted pure-Python tier.

### Gaps
- No current Kata memory-overhead figure and no current, methodologically clear Kata or Cloud Hypervisor boot benchmark were found. One blog claims Cloud Hypervisor is now Kata's default hypervisor; unverified and it conflicts with my prior understanding that QEMU is the default.
- Current Cloud Hypervisor version, snapshot/restore capability and seccomp posture were not researched from primary sources.
- nsjail's own documentation on its threat model was not fetched; no primary "not a security boundary" statement from nsjail was found.
- WebAssembly: no sources were collected. Start-up, memory and compatibility numbers for Pyodide or WASI Python are absent from these notes.

## 7. What practitioners and published threat models recommend as minimum isolation, and what fits an MVP versus a production multi-tenant service

### Takeaway
Published guidance converges: for hostile multi-tenant code, a boundary that does not expose the shared host kernel directly (a microVM, or at least a user-space kernel such as gVisor) is the minimum, with hardened containers acceptable only for semi-trusted or single-tenant use and always combined with network egress control. That maps to hardened rootless Podman for a clearly scoped MVP and Firecracker (or gVisor where KVM is unavailable) for production, behind a runtime abstraction designed for it from day one.

### Cited Findings
- AWS's rationale: Lambda originally used Linux containers to isolate functions and virtualization to isolate customer accounts, so different customers always ran in different VMs; the paper calls the container trade-off between security and compatibility "unacceptable" for its multi-tenant use and chose hypervisor-based isolation with a minimal VMM (DATED 2020, but still the canonical published threat model) — [Agache et al., NSDI 2020](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)
- Firecracker's guidance treats even the VM boundary as insufficient alone: jailer, per-VM UID, default seccomp, SMT off for tenant separation, KSM off, swap off, microcode current — [prod-host-setup.md](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/prod-host-setup.md)
- gVisor's own model: it exists to stop untrusted code reaching host kernel bugs through the System API, explicitly does not address hardware side channels, and states "A sandbox is not a substitute for a secure architecture" — [gVisor security model](https://gvisor.dev/docs/architecture_guide/security/)
- A September 2026 practitioner article, written after the year's kernel-escape run, recommends moving untrusted workloads to microVMs with a kernel per workload, specifically "Notebooks, agent sandboxes, and serverless: use a microVM per tenant" and a VM per CI job for untrusted pull requests; it notes Firecracker has the smaller attack surface and Kata the easier migration path (fetched; single-author security blog) — [simeononsecurity, 2026-09-28](https://simeononsecurity.com/articles/containers-no-longer-security-boundary-2026/)
- Vendor position (Edera, 2026-04-07): user namespaces are not sufficient for multi-tenant isolation; disable unprivileged user namespaces where not needed and use an architectural boundary (fetched; vendor with a product in this category) — [Edera](https://edera.dev/stories/user-namespaces-are-not-a-security-boundary)
- 2026 agent-sandbox guides broadly tier it as: shared-kernel containers only for semi-trusted code with no network, no secrets and a read-only filesystem; gVisor where some syscall gaps are acceptable; Firecracker for the strongest per-tenant boundary. They also stress that kernel isolation does not stop data exfiltration if egress is open (search summary; mixed-quality blogs, some vendor-authored) — [manveerc substack](https://manveerc.substack.com/p/ai-agent-sandboxing-guide), [Modal](https://modal.com/resources/best-code-execution-sandboxes-tool-calling-ai-agents), [tembo](https://www.tembo.io/blog/ai-agent-sandbox)
- Industry practice as reported: E2B, AWS Lambda and Vercel Sandbox on Firecracker; Modal on gVisor (search summary; vendor-authored) — [Modal](https://modal.com/resources/best-code-execution-sandboxes-tool-calling-ai-agents)
- ArchWiki's blunt framing for namespace sandboxes: "running untrusted code is never safe, sandboxing cannot change this" (search summary) — [ArchWiki: Bubblewrap](https://wiki.archlinux.org/title/Bubblewrap)

### Inferences

Comparison summary (synthesis of sections 1-6; numbers only where sourced above)

| Layer | Boundary | Start-up | Per-sandbox overhead | Needs KVM | Rootless | Fits Podman `--runtime` |
|---|---|---|---|---|---|---|
| Rootless Podman + crun/runc, hardened | Shared host kernel; namespaces, seccomp, caps, LSM | Not sourced here (container-class) | Not sourced (process-class) | No | Yes | Native |
| nsjail / bubblewrap | Same shared-kernel primitives, no image tooling | Not sourced | Not sourced | No | Yes | No |
| gVisor runsc (systrap) | User-space kernel; host kernel reached only via the Sentry's limited syscalls | Not sourced | Not sourced | No | Yes, via engine-configured userns | Yes |
| krun / libkrun | KVM microVM per container | Not sourced | Not sourced | Yes | Reported working; thinly documented | Yes |
| Firecracker + jailer | KVM microVM, minimal VMM, seccomp-jailed | 125 ms or less to guest init under spec conditions | 5 MiB or less VMM (spec); about 3 MB measured in 2020 | Yes | No (jailer needs root to set up) | No, separate backend |
| Cloud Hypervisor | KVM microVM, larger device model | Comparable to Firecracker in 2020 | About 13 MB in 2020 | Yes | Not researched | No (via Kata) |
| Kata Containers | KVM VM per pod via QEMU/CLH/FC | 150 ms to 1.9 s depending on source and what is measured | Not sourced | Yes | Not researched | Effectively no |

MVP for a small team
- Rootless Podman with crun and the full hardening set from section 1 is the right first backend: no KVM dependency, runs on any cloud VM or laptop, mature tooling, and pre-warming is just keeping created or paused containers.
- It is only defensible if the MVP's threat model is stated honestly: code is LLM-generated on behalf of a trusted or single tenant (mistakes and prompt-injection-driven misbehaviour, not a skilled attacker with a kernel 0-day), no secrets or lateral network access in the sandbox, egress denied by default, a dedicated disposable host (the sandbox host is itself treated as untrusted and holds nothing else), and kernel updates applied within days. The 2026 CVE record means "we run a hardened container" should not be marketed as multi-tenant isolation.
- Cheapest meaningful upgrade inside the MVP: make `--runtime=runsc` a per-pool option early. It needs no KVM, removes direct host-kernel exposure, and exercises the abstraction with a second backend before Firecracker work begins.

Production multi-tenant service
- Minimum per the published guidance: one microVM per tenant session (Firecracker with the jailer and the production host settings), or gVisor where KVM is unavailable or density matters more, plus the same in-guest hardening (non-root, no-new-privileges, seccomp) as defence in depth, egress filtering, and per-tenant resource limits.
- Pre-warming with Firecracker means booted-idle pools or snapshot restore; the uniqueness caveats in section 3 make "one snapshot, many clones" a design item, not a free optimisation.
- Hosting is no longer bare-metal-only on AWS as of 2026, which makes Firecracker reachable for a small team, but nested performance and the Intel-only restriction need a benchmark before committing.

Is a pluggable runtime abstraction realistic?
- Yes, with the two-seam structure from section 5. Define the scheduler's backend interface above Podman, implement Podman (crun) first, add runsc as a near-free second implementation, and add Firecracker as a separate driver. Expect the interface to carry a capability descriptor (supports snapshot, supports pause, network modes, exec latency class) rather than assuming uniform behaviour.
- The main risk is designing the interface around Podman idioms (bind mounts, `podman exec`, cgroup-based limits) that have no direct microVM equivalent; a guest-agent-style exec and file-transfer API that Podman also implements avoids that.

### Gaps
- No formal standards-body guidance (NIST, CIS, OWASP) specific to LLM-agent code-execution sandboxes was retrieved; the recommendation rests on the AWS paper, project security docs and 2026 practitioner and vendor writing.
- E2B, Fly.io, Cloudflare and Northflank engineering posts were not read directly; platform claims come from a Modal-authored comparison and search digests, so per-vendor cold-start numbers are deliberately omitted.
- No published head-to-head 2026 benchmark covering rootless Podman, gVisor systrap, krun and Firecracker on the same hardware with a Python workload was found. The comparison table therefore has "not sourced" cells where the brief asked for cold-start and memory numbers; those need an in-house benchmark.
- Hardware side-channel posture for shared-kernel and gVisor tiers (SMT, core scheduling) was not researched beyond Firecracker's guidance.
