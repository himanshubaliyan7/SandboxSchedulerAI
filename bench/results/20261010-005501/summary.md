# sbxbench summary (2026-10-10T00:55:02+0530)

All latencies are wall-clock milliseconds. `per-round p50` shows the median of each round separately (the spread between rounds is the noise from other work on the machine). `load1` is the range of the 1-minute load average seen while the row was being measured.

Run configuration: samples/round=50, rounds=3, warm-up cycles/round=5, quick=False, benchmarks=parallel.

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
- **load_average**: [4.0, 1.95, 0.78]
- **foreign_containers_at_start**: 0
- **storage_graph_status**: {"Backing Filesystem": "extfs", "Native Overlay Diff": "true", "Supports d_type": "true", "Supports shifting": "false", "Supports volatile": "true", "Using metacopy": "false"}
- **sysctl**: {"fs.inotify.max_user_instances": "512", "fs.inotify.max_user_watches": "524288", "kernel.keys.maxkeys": "200", "kernel.keys.maxbytes": "20000", "kernel.pid_max": "4194304", "kernel.threads-max": "81301", "user.max_user_namespaces": "40650", "kernel.apparmor_restrict_unprivileged_userns": "1", "kernel.unprivileged_bpf_disabled": "2", "vm.swappiness": "10"}
- **top CPU processes at start**: ['%CPU %MEM COMMAND', ' 9.0  0.2 python3', ' 4.8  0.4 podman', ' 1.4  4.0 claude', ' 1.0  4.2 gnome-shell', ' 0.4  0.5 gnome-terminal-', ' 0.3  1.3 Xorg', ' 0.2  0.0 kworker/u16:8-ext4-rsv-conversion', ' 0.1  0.6 gjs']

## 4a. Parallel create throughput and teardown rate

One create = create + start + agent ready, hardened spec. n per run = 64.

| transport | concurrency | round | ok | creates/s | per-create p50 ms | p95 ms | max ms | removals/s | create errors | load1 start-end |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| cli | 4 | 1 | 64/64 | 15.91 | 243.8 | 323.7 | 361.1 | 10.89 | 0 | 3.64-3.62 |
| cli | 4 | 2 | 64/64 | 14.79 | 253.2 | 373.7 | 482.5 | 8.79 | 0 | 3.89-3.91 |
| cli | 4 | 3 | 64/64 | 15.19 | 234.9 | 412.3 | 652.8 | 7.19 | 0 | 4.41-5.33 |
| cli | 8 | 1 | 64/64 | 17.33 | 382.9 | 804.2 | 2236.2 | 8.01 | 0 | 3.62-3.75 |
| cli | 8 | 2 | 64/64 | 13.67 | 517.4 | 959.1 | 1270.5 | 7.87 | 0 | 3.91-4.83 |
| cli | 8 | 3 | 64/64 | 11.17 | 509.6 | 1854.7 | 2310.2 | 7.21 | 0 | 5.33-4.65 |
| rest-api | 4 | 1 | 63/64 | 19.13 | 176.8 | 364.3 | 491.2 | 13.38 | 1 | 4.0-3.84 |
| rest-api | 4 | 2 | 64/64 | 18.37 | 191.1 | 364.6 | 785.1 | 13.63 | 0 | 3.75-3.71 |
| rest-api | 4 | 3 | 64/64 | 19.29 | 171.9 | 398.0 | 816.8 | 12.83 | 0 | 4.83-4.54 |
| rest-api | 8 | 1 | 64/64 | 20.14 | 224.8 | 1264.6 | 1804.8 | 13.28 | 0 | 3.84-3.64 |
| rest-api | 8 | 2 | 64/64 | 17.04 | 295.3 | 1107.0 | 2300.9 | 8.92 | 0 | 3.71-3.89 |
| rest-api | 8 | 3 | 63/64 | 18.82 | 291.1 | 950.2 | 1222.7 | 12.86 | 1 | 4.54-4.41 |

Create errors in total: 2 of 768 attempts. Distinct messages: `["start 500: {\"cause\":\"OCI runtime error\",\"message\":\"crun: write to `/proc/sys/net/ipv4/ping_group_range`: Invalid argument: OCI runtime error\",\"response\":500}\n"]`

