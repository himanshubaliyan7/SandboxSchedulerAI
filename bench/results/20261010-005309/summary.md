# sbxbench summary (2026-10-10T00:53:10+0530)

All latencies are wall-clock milliseconds. `per-round p50` shows the median of each round separately (the spread between rounds is the noise from other work on the machine). `load1` is the range of the 1-minute load average seen while the row was being measured.

Run configuration: samples/round=50, rounds=2, warm-up cycles/round=5, quick=False, benchmarks=parallel.

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
- **load_average**: [1.7, 0.67, 0.26]
- **foreign_containers_at_start**: 9
- **storage_graph_status**: {"Backing Filesystem": "extfs", "Native Overlay Diff": "true", "Supports d_type": "true", "Supports shifting": "false", "Supports volatile": "true", "Using metacopy": "false"}
- **sysctl**: {"fs.inotify.max_user_instances": "512", "fs.inotify.max_user_watches": "524288", "kernel.keys.maxkeys": "200", "kernel.keys.maxbytes": "20000", "kernel.pid_max": "4194304", "kernel.threads-max": "81301", "user.max_user_namespaces": "40650", "kernel.apparmor_restrict_unprivileged_userns": "1", "kernel.unprivileged_bpf_disabled": "2", "vm.swappiness": "10"}
- **top CPU processes at start**: ['%CPU %MEM COMMAND', ' 8.5  0.2 python3', ' 3.0  0.4 podman', ' 1.8  0.2 go', ' 1.8  0.1 sbxd', ' 1.4  4.0 claude', ' 1.0  4.2 gnome-shell', ' 0.9  0.9 e2e.test', ' 0.6  0.1 postgres']

## 4a. Parallel create throughput and teardown rate

One create = create + start + agent ready, hardened spec. n per run = 32.

| transport | concurrency | round | ok | creates/s | per-create p50 ms | p95 ms | max ms | removals/s | create errors | load1 start-end |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| cli | 1 | 1 | 32/32 | 4.83 | 187.8 | 395.1 | 509.2 | 5.62 | 0 | 2.1-2.33 |
| cli | 1 | 2 | 32/32 | 7.8 | 126.1 | 136.9 | 178.8 | 8.1 | 0 | 5.14-4.66 |
| cli | 2 | 1 | 32/32 | 12.77 | 153.8 | 178.6 | 186.4 | 10.83 | 0 | 2.33-2.62 |
| cli | 2 | 2 | 32/32 | 12.97 | 152.4 | 170.5 | 173.2 | 8.45 | 0 | 4.66-4.44 |
| cli | 4 | 1 | 32/32 | 15.77 | 219.1 | 376.3 | 390.2 | 8.97 | 0 | 2.62-3.54 |
| cli | 4 | 2 | 32/32 | 10.21 | 314.7 | 744.3 | 1132.6 | 9.22 | 0 | 4.44-4.49 |
| cli | 8 | 1 | 32/32 | 15.06 | 436.1 | 911.9 | 1315.8 | 8.18 | 0 | 3.54-3.81 |
| cli | 8 | 2 | 32/32 | 11.75 | 508.4 | 1310.7 | 1753.9 | 6.23 | 0 | 4.49-4.86 |
| rest-api | 1 | 1 | 32/32 | 10.8 | 88.4 | 129.8 | 146.1 | 11.75 | 0 | 1.7-1.8 |
| rest-api | 1 | 2 | 32/32 | 10.62 | 89.2 | 114.1 | 174.1 | 12.19 | 0 | 3.81-3.61 |
| rest-api | 2 | 1 | 32/32 | 17.5 | 112.5 | 137.4 | 147.3 | 11.39 | 0 | 1.8-1.9 |
| rest-api | 2 | 2 | 32/32 | 16.76 | 115.6 | 141.4 | 157.4 | 14.01 | 0 | 3.61-3.61 |
| rest-api | 4 | 1 | 32/32 | 10.05 | 269.0 | 846.5 | 1059.8 | 12.13 | 0 | 1.9-1.85 |
| rest-api | 4 | 2 | 32/32 | 15.22 | 206.2 | 535.2 | 1275.5 | 12.38 | 0 | 3.61-4.28 |
| rest-api | 8 | 1 | 32/32 | 14.76 | 269.1 | 1691.5 | 2030.2 | 10.74 | 0 | 1.85-2.1 |
| rest-api | 8 | 2 | 32/32 | 20.39 | 263.1 | 956.9 | 1113.4 | 11.71 | 0 | 4.28-5.14 |

Create errors in total: 0 of 512 attempts. Distinct messages: `[]`

