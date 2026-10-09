# sbxbench summary (2026-10-10T00:51:56+0530)

All latencies are wall-clock milliseconds. `per-round p50` shows the median of each round separately (the spread between rounds is the noise from other work on the machine). `load1` is the range of the 1-minute load average seen while the row was being measured.

Run configuration: samples/round=50, rounds=2, warm-up cycles/round=5, quick=False, benchmarks=probes.

## Host

- **kernel**: 7.0.0-28-generic
- **os**: Ubuntu 24.04.4 LTS
- **cpu_model**: Intel(R) Core(TM) i5-7200U CPU @ 2.50GHz
- **cpus**: 4
- **mem_total_kb**: 12021128
- **swap_total_kb**: 10204860
- **podman_version**: 4.9.3
- **crun_version**: crun version 1.14.1
- **runc_version**: runc version 1.4.3
- **runsc_version**: not installed
- **conmon_version**: conmon version 2.1.10, commit: unknown
- **pasta_package**: passt_0.0~git20240220.1e6f92b-1_amd64
- **slirp4netns**: slirp4netns version 1.2.1
- **fuse_overlayfs**: fusermount3 version: 3.14.0
- **storage_driver**: overlay
- **graph_root_fs_type**: ext2/ext3
- **transient_store**: False
- **database_backend**: sqlite
- **cgroup_version**: v2
- **cgroup_manager**: systemd
- **podman_info_cgroup_controllers**: ['memory', 'pids']
- **user_service_cgroup_subtree_control**: ['cpu', 'memory', 'pids']
- **default_log_driver**: journald
- **hyperfine**: None
- **load_average**: [0.31, 0.09, 0.05]
- **foreign_containers_at_start**: 0
- **storage_graph_status**: {"Backing Filesystem": "extfs", "Native Overlay Diff": "true", "Supports d_type": "true", "Supports shifting": "false", "Supports volatile": "true", "Using metacopy": "false"}
- **sysctl**: {"fs.inotify.max_user_instances": "512", "fs.inotify.max_user_watches": "524288", "kernel.keys.maxkeys": "200", "kernel.keys.maxbytes": "20000", "kernel.pid_max": "4194304", "kernel.threads-max": "81301", "user.max_user_namespaces": "40650", "kernel.apparmor_restrict_unprivileged_userns": "1", "kernel.unprivileged_bpf_disabled": "2", "vm.swappiness": "10"}
- **top CPU processes at start**: ['%CPU %MEM COMMAND', ' 9.0  0.2 python3', ' 1.4  4.1 claude', ' 1.0  4.2 gnome-shell', ' 0.4  0.5 gnome-terminal-', ' 0.3  1.3 Xorg', ' 0.1  0.6 gjs', ' 0.1  0.0 dbus-daemon', ' 0.1  0.0 dbus-daemon']

## 6. Host capability probes

| probe | question | result | detail |
|---|---|---|---|
| cgroup_kill | Can the rootless user write cgroup.kill for a Podman-created container cgroup, and does it kill a fork-heavy tree? | **works** | 62 pids (60 sleeps + a fork loop) gone 4.1 ms after writing 1; Podman state 'stopped 137 false -> exited 137' after 119 ms |
| memory_oom_group | Does --cgroup-conf memory.oom.group=1 take effect? | **works** | oom.group=1: file reads 1, container ended 'exited 137 false' (status exitcode OOMKilled); control oom.group=0: ended 'exited 42 false' (42 = only the hog was killed and the shell carried on) |
| storage_opt_size | Does --storage-opt size= work rootless on this filesystem? | **fails** | Error: storage option overlay.size and overlay.inodes only supported for backingFS XFS. Found extfs |
| exec_detached_reattach | Can a detached `podman exec -d` session be re-attached? | **fails** | second start of a detached, still-running exec session -> HTTP 500: bb28a01dc5a499d8f561b81ae580a2d5cdd4866a88cfd7fb83d6ca51c57 session e634fdacc29c2d6211e452c61f19e518ca3145a0b8bd30aa7a8e9935b79b2c03 state is \"running\": exec session state improper","response":500} |
| seccomp_syscalls | What do the installed default seccomp profile and the product profile do with unshare, bpf, io_uring_setup, AF_ALG and friends? | **recorded** | default container: unshare(CLONE_NEWUSER)=OK, bpf(BPF_MAP_CREATE)=EPERM, io_uring_setup(1)=ENOSYS, socket(AF_ALG, SOCK_SEQPACKET)=OK |
| session_keyring_quota | Does every container consume one kernel key from the user's quota, and does keyring=false avoid it? | **recorded** | 3 containers add 3 keys by default and 0 with keyring=false; per-user quota kernel.keys.maxkeys=200 |
| checkpoint_rootless | Does podman container checkpoint (CRIU) work rootless? | **fails** | Error: checkpointing a container requires root |
| cgroup_delegation | Which cgroup v2 controllers are delegated to the rootless user, and do cpu/memory/pids limits land in the container cgroup? | **works** | {"cpu.max": "100000 100000", "memory.max": "268435456", "memory.high": "241590272", "memory.swap.max": "0", "memory.oom.group": "1", "pids.max": "128"} |

### Seccomp probe matrix (errno seen from inside the container; OK = the call succeeded)

| call | default container (default seccomp, default caps, root in userns) | default seccomp, cap-drop all, no-new-privileges, uid 1000 (hardened without the product profile) | product profile (hardened spec) | seccomp=unconfined, default caps (control: what the kernel itself refuses) |
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

Process status per variant: `{"default container (default seccomp, default caps, root in userns)": {"Uid": "0\t0\t0\t0", "CapEff": "00000000800405fb", "CapBnd": "00000000800405fb", "NoNewPrivs": "0", "Seccomp": "2", "Seccomp_filters": "1"}, "default seccomp, cap-drop all, no-new-privileges, uid 1000 (hardened without the product profile)": {"Uid": "1000\t1000\t1000\t1000", "CapEff": "0000000000000000", "CapBnd": "0000000000000000", "NoNewPrivs": "1", "Seccomp": "2", "Seccomp_filters": "1"}, "product profile (hardened spec)": {"Uid": "1000\t1000\t1000\t1000", "CapEff": "0000000000000000", "CapBnd": "0000000000000000", "NoNewPrivs": "1", "Seccomp": "2", "Seccomp_filters": "1"}, "seccomp=unconfined, default caps (control: what the kernel itself refuses)": {"Uid": "0\t0\t0\t0", "CapEff": "00000000800405fb", "CapBnd": "00000000800405fb", "NoNewPrivs": "0", "Seccomp": "0", "Seccomp_filters": "0"}}`

Host sysctls that also gate these calls: `{"kernel.unprivileged_bpf_disabled": "2", "kernel.io_uring_disabled": "0", "vm.unprivileged_userfaultfd": "0", "kernel.apparmor_restrict_unprivileged_userns": "1", "kernel.unprivileged_userns_clone": "1", "kernel.perf_event_paranoid": "4", "kernel.yama.ptrace_scope": "1", "user.max_user_namespaces": "40650"}`

Static reading of the two profiles (rules that name each syscall):

| syscall | installed default profile | product profile |
|---|---|---|
| unshare | `[{"action": "SCMP_ACT_ALLOW"}]` | `[{"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_MASKED_EQ 2114060416"]}]` |
| bpf | `[{"action": "SCMP_ACT_ALLOW", "only_with_caps": ["CAP_SYS_ADMIN"]}, {"action": "SCMP_ACT_ERRNO", "only_without_caps": ["CAP_SYS_ADMIN"], "errnoRet": 1}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| io_uring_setup | `"not listed (default action applies)"` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 38}]` |
| userfaultfd | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| keyctl | `[{"action": "SCMP_ACT_ALLOW"}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| add_key | `"not listed (default action applies)"` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| ptrace | `[{"action": "SCMP_ACT_ALLOW"}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| mount | `[{"action": "SCMP_ACT_ALLOW"}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| perf_event_open | `[{"action": "SCMP_ACT_ALLOW", "only_with_caps": ["CAP_SYS_ADMIN"]}, {"action": "SCMP_ACT_ERRNO", "only_without_caps": ["CAP_SYS_ADMIN"], "errnoRet": 1}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| clone3 | `[{"action": "SCMP_ACT_ALLOW"}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 38}]` |
| personality | `[{"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 0"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 8"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 131072"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 131080"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 4294967295"]}]` | `[{"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 0"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 8"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 131072"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 131080"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 4294967295"]}]` |
| open_by_handle_at | `[{"action": "SCMP_ACT_ALLOW", "only_with_caps": ["CAP_DAC_READ_SEARCH"]}, {"action": "SCMP_ACT_ERRNO", "only_without_caps": ["CAP_DAC_READ_SEARCH"], "errnoRet": 1}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| kexec_load | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| init_module | `[{"action": "SCMP_ACT_ALLOW", "only_with_caps": ["CAP_SYS_MODULE"]}, {"action": "SCMP_ACT_ERRNO", "only_without_caps": ["CAP_SYS_MODULE"], "errnoRet": 1}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| setns | `[{"action": "SCMP_ACT_ALLOW"}, {"action": "SCMP_ACT_ALLOW", "only_with_caps": ["CAP_SYS_ADMIN"]}, {"action": "SCMP_ACT_ERRNO", "only_without_caps": ["CAP_SYS_ADMIN"], "errnoRet": 1}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| pivot_root | `[{"action": "SCMP_ACT_ALLOW"}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| process_vm_readv | `[{"action": "SCMP_ACT_ALLOW"}]` | `[{"action": "SCMP_ACT_ERRNO", "errnoRet": 1}]` |
| socket | `[{"action": "SCMP_ACT_ERRNO", "args": ["arg0 SCMP_CMP_EQ 16", "arg2 SCMP_CMP_EQ 9"], "only_without_caps": ["CAP_AUDIT_WRITE"], "errnoRet": 22}, {"action": "SCMP_ACT_ALLOW", "args": ["arg2 SCMP_CMP_NE 9"], "only_without_caps": ["CAP_AUDIT_WRITE"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_NE 16"], "only_without_caps": ["CAP_AUDIT_WRITE"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg2 SCMP_CMP_NE 9"], "only_without_caps": ["CAP_AUDIT_WRITE"]}, {"action": "SCMP_ACT_ALLOW", "only_with_caps": ["CAP_AUDIT_WRITE"]}]` | `[{"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 1"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 2"]}, {"action": "SCMP_ACT_ALLOW", "args": ["arg0 SCMP_CMP_EQ 10"]}]` |

Default actions: installed profile `SCMP_ACT_ERRNO` errno 38; product profile `SCMP_ACT_ERRNO` errno 38.

