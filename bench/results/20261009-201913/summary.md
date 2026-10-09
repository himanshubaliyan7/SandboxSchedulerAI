# sbxbench summary (2026-10-09T20:19:14+0530)

All latencies are wall-clock milliseconds. `per-round p50` shows the median of each round separately (the spread between rounds is the noise from other work on the machine). `load1` is the range of the 1-minute load average seen while the row was being measured.

Run configuration: samples/round=50, rounds=2, warm-up cycles/round=5, quick=False, benchmarks=floor,lifecycle,storage,api,userns,pause,density,parallel,workload,probes.

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
- **load_average**: [0.83, 1.04, 1.18]
- **foreign_containers_at_start**: 0
- **storage_graph_status**: {"Backing Filesystem": "extfs", "Native Overlay Diff": "true", "Supports d_type": "true", "Supports shifting": "false", "Supports volatile": "true", "Using metacopy": "false"}
- **sysctl**: {"fs.inotify.max_user_instances": "512", "fs.inotify.max_user_watches": "524288", "kernel.keys.maxkeys": "200", "kernel.keys.maxbytes": "20000", "kernel.pid_max": "4194304", "kernel.threads-max": "81301", "user.max_user_namespaces": "40650", "kernel.apparmor_restrict_unprivileged_userns": "1", "kernel.unprivileged_bpf_disabled": "2", "vm.swappiness": "10"}
- **top CPU processes at start**: ['%CPU %MEM COMMAND', '10.7  0.2 python3', ' 1.8  4.0 claude', ' 1.2  4.1 gnome-shell', ' 0.6  0.5 gnome-terminal-', ' 0.5  1.3 Xorg', ' 0.1  0.0 irq/132-iwlwifi', ' 0.1  0.1 systemd', ' 0.1  0.0 kworker/u16:6-flush-8:0']

## 0. Harness floor

| cell | op | n | mean | sd | min | p50 | p95 | p99 | max | per-round p50 | load1 | errors |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---:|
| harness | podman_ps_noop | 100 | 26.4 | 1.0 | 24.9 | 26.3 | 28.0 | 29.0 | 31.3 | 26.3 / 26.3 | 0.83-0.84 | 0 |
| harness | podman_version | 100 | 21.4 | 1.1 | 19.5 | 21.1 | 24.0 | 25.3 | 25.7 | 21.1 / 21.1 | 0.83-0.84 | 0 |
| harness | spawn_bin_true | 100 | 1.0 | 0.1 | 0.9 | 1.0 | 1.2 | 1.4 | 2.1 | 1.0 / 1.0 | 0.83-0.84 | 0 |

## 1. Lifecycle latency matrix (podman CLI, default store)

Cell = runtime/network/spec. `create`..`rm_f_running` are one container's life; `run_rm_true` is a separate `podman run --rm IMAGE true`. Idle command `sleep 86400`, image alpine:3.20.

| cell | op | n | mean | sd | min | p50 | p95 | p99 | max | per-round p50 | load1 | errors |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---:|
| crun/none/hardened | run_rm_true | 100 | 181.5 | 20.3 | 165.5 | 175.9 | 226.5 | 281.7 | 282.5 | 175.0 / 177.9 | 1.52-2.79 | 0 |
| crun/none/hardened | create | 100 | 59.5 | 8.7 | 55.4 | 57.7 | 68.7 | 112.1 | 118.8 | 57.1 / 58.3 | 1.52-2.79 | 0 |
| crun/none/hardened | start | 100 | 100.3 | 15.5 | 88.4 | 95.3 | 131.5 | 158.6 | 196.7 | 94.8 / 98.9 | 1.52-2.79 | 0 |
| crun/none/hardened | create_plus_start | 100 | 159.8 | 22.3 | 145.1 | 153.3 | 191.0 | 277.4 | 308.8 | 152.0 / 157.5 | 1.52-2.79 | 0 |
| crun/none/hardened | exec_first | 100 | 162.8 | 16.1 | 131.5 | 167.9 | 185.7 | 197.0 | 212.2 | 170.4 / 165.2 | 1.52-2.79 | 0 |
| crun/none/hardened | exec_again | 100 | 157.7 | 17.5 | 117.1 | 161.7 | 178.0 | 215.3 | 219.0 | 161.8 / 161.3 | 1.52-2.79 | 0 |
| crun/none/hardened | rm_f_running | 100 | 123.1 | 17.3 | 103.2 | 118.0 | 151.6 | 196.8 | 200.8 | 114.3 / 123.7 | 1.52-2.79 | 0 |
| crun/none/plain | run_rm_true | 100 | 175.2 | 9.7 | 163.2 | 172.8 | 185.0 | 218.2 | 222.7 | 173.9 / 172.7 | 0.85-2.29 | 0 |
| crun/none/plain | create | 100 | 56.9 | 2.6 | 53.4 | 56.4 | 61.2 | 67.5 | 72.1 | 56.5 / 56.4 | 0.84-2.29 | 0 |
| crun/none/plain | start | 100 | 94.0 | 4.0 | 85.7 | 93.4 | 100.3 | 101.3 | 102.4 | 93.4 / 93.4 | 0.85-2.29 | 0 |
| crun/none/plain | create_plus_start | 100 | 150.9 | 5.0 | 140.3 | 150.9 | 159.7 | 165.8 | 169.8 | 150.9 / 150.9 | 0.85-2.29 | 0 |
| crun/none/plain | exec_first | 100 | 159.1 | 15.7 | 116.1 | 165.2 | 178.3 | 185.0 | 187.5 | 165.5 / 165.0 | 0.85-2.29 | 0 |
| crun/none/plain | exec_again | 100 | 158.8 | 15.8 | 122.6 | 161.0 | 177.5 | 215.2 | 219.8 | 161.0 / 160.9 | 0.85-2.29 | 0 |
| crun/none/plain | rm_f_running | 100 | 122.3 | 9.0 | 102.9 | 121.7 | 134.7 | 158.7 | 167.8 | 123.7 / 118.5 | 0.85-2.29 | 0 |
| crun/pasta/hardened | run_rm_true | 100 | 217.5 | 15.7 | 202.7 | 213.1 | 251.5 | 279.0 | 295.3 | 213.5 / 211.6 | 1.80-2.81 | 0 |
| crun/pasta/hardened | create | 100 | 59.0 | 7.8 | 54.6 | 57.7 | 62.5 | 103.0 | 120.1 | 57.4 / 57.8 | 1.80-2.81 | 0 |
| crun/pasta/hardened | start | 100 | 130.9 | 24.6 | 116.3 | 125.9 | 159.0 | 214.9 | 339.9 | 124.5 / 127.6 | 1.80-2.81 | 0 |
| crun/pasta/hardened | create_plus_start | 100 | 189.9 | 29.5 | 173.9 | 183.9 | 218.6 | 277.8 | 442.7 | 182.7 / 185.1 | 1.80-2.81 | 0 |
| crun/pasta/hardened | exec_first | 100 | 158.1 | 17.4 | 117.1 | 163.4 | 178.8 | 213.4 | 213.4 | 154.7 / 165.9 | 1.80-2.81 | 0 |
| crun/pasta/hardened | exec_again | 100 | 158.1 | 15.6 | 120.9 | 161.3 | 173.8 | 178.3 | 254.6 | 162.0 / 161.2 | 1.80-2.81 | 0 |
| crun/pasta/hardened | rm_f_running | 100 | 128.6 | 17.9 | 108.9 | 125.9 | 146.5 | 176.7 | 275.3 | 124.5 / 127.2 | 1.80-2.81 | 0 |
| crun/pasta/plain | run_rm_true | 100 | 204.8 | 8.7 | 193.4 | 203.5 | 221.4 | 233.7 | 241.7 | 202.9 / 203.7 | 1.87-3.29 | 0 |
| crun/pasta/plain | create | 100 | 57.1 | 2.9 | 53.4 | 56.7 | 59.4 | 64.3 | 81.7 | 56.8 / 56.3 | 1.87-3.29 | 0 |
| crun/pasta/plain | start | 100 | 144.8 | 10.0 | 129.7 | 143.5 | 169.6 | 182.1 | 183.6 | 144.2 / 141.6 | 1.87-3.29 | 0 |
| crun/pasta/plain | create_plus_start | 100 | 201.9 | 10.6 | 185.2 | 200.3 | 228.5 | 239.2 | 239.8 | 202.1 / 198.1 | 1.87-3.29 | 0 |
| crun/pasta/plain | exec_first | 100 | 155.6 | 13.2 | 118.7 | 160.7 | 171.2 | 175.5 | 177.3 | 161.8 / 159.2 | 1.87-3.29 | 0 |
| crun/pasta/plain | exec_again | 100 | 155.3 | 12.0 | 113.1 | 159.1 | 170.8 | 175.0 | 175.2 | 162.4 / 158.3 | 1.87-3.29 | 0 |
| crun/pasta/plain | rm_f_running | 100 | 125.3 | 5.6 | 114.9 | 124.7 | 135.1 | 141.0 | 145.4 | 123.5 / 125.5 | 1.87-3.29 | 0 |
| crun/slirp4netns/hardened | run_rm_true | 100 | 195.7 | 19.4 | 181.3 | 192.2 | 207.8 | 234.6 | 368.3 | 190.9 / 194.4 | 2.09-2.39 | 0 |
| crun/slirp4netns/hardened | create | 100 | 58.9 | 6.5 | 54.8 | 57.8 | 60.9 | 87.7 | 113.7 | 57.8 / 57.8 | 2.09-2.39 | 0 |
| crun/slirp4netns/hardened | start | 100 | 107.7 | 9.9 | 98.8 | 105.1 | 131.3 | 147.7 | 153.0 | 104.7 / 105.2 | 2.09-2.39 | 0 |
| crun/slirp4netns/hardened | create_plus_start | 100 | 166.6 | 13.1 | 154.9 | 163.0 | 191.6 | 210.2 | 252.3 | 162.8 / 163.1 | 2.09-2.39 | 0 |
| crun/slirp4netns/hardened | exec_first | 100 | 158.0 | 16.5 | 113.0 | 165.3 | 176.3 | 197.1 | 213.3 | 166.0 / 164.5 | 2.09-2.39 | 0 |
| crun/slirp4netns/hardened | exec_again | 100 | 156.5 | 15.6 | 121.5 | 159.2 | 176.2 | 179.5 | 243.6 | 150.3 / 160.0 | 2.09-2.39 | 0 |
| crun/slirp4netns/hardened | rm_f_running | 100 | 126.4 | 19.9 | 107.3 | 123.8 | 134.8 | 154.4 | 315.2 | 124.0 / 123.4 | 2.09-2.39 | 0 |
| crun/slirp4netns/plain | run_rm_true | 100 | 185.3 | 8.1 | 171.5 | 184.6 | 195.6 | 215.2 | 226.7 | 184.5 / 185.2 | 1.84-2.74 | 0 |
| crun/slirp4netns/plain | create | 100 | 56.6 | 1.9 | 53.5 | 56.5 | 58.8 | 62.8 | 68.6 | 56.5 / 56.5 | 1.84-2.74 | 0 |
| crun/slirp4netns/plain | start | 100 | 101.4 | 7.5 | 91.6 | 100.5 | 109.1 | 138.3 | 145.6 | 100.5 / 100.6 | 1.84-2.74 | 0 |
| crun/slirp4netns/plain | create_plus_start | 100 | 158.0 | 8.0 | 148.4 | 157.0 | 168.2 | 196.1 | 202.7 | 157.0 / 157.2 | 1.84-2.74 | 0 |
| crun/slirp4netns/plain | exec_first | 100 | 159.3 | 16.3 | 135.9 | 163.6 | 181.0 | 200.0 | 208.0 | 166.6 / 162.0 | 1.84-2.74 | 0 |
| crun/slirp4netns/plain | exec_again | 100 | 157.0 | 13.1 | 114.0 | 161.2 | 172.0 | 174.9 | 178.3 | 164.1 / 159.7 | 1.84-2.74 | 0 |
| crun/slirp4netns/plain | rm_f_running | 100 | 123.6 | 6.7 | 113.7 | 122.0 | 136.7 | 143.3 | 154.8 | 120.3 / 125.3 | 1.84-2.74 | 0 |
| runc/none/hardened | run_rm_true | 100 | 396.9 | 26.5 | 371.3 | 389.5 | 434.6 | 547.3 | 547.6 | 397.4 / 386.0 | 1.40-2.22 | 0 |
| runc/none/hardened | create | 100 | 59.6 | 8.5 | 54.4 | 57.6 | 68.6 | 96.6 | 110.4 | 57.4 / 57.7 | 1.40-2.22 | 0 |
| runc/none/hardened | start | 100 | 185.3 | 14.3 | 171.3 | 181.9 | 208.9 | 234.9 | 282.3 | 184.2 / 180.8 | 1.40-2.22 | 0 |
| runc/none/hardened | create_plus_start | 100 | 244.9 | 21.0 | 229.4 | 239.7 | 278.5 | 329.2 | 378.8 | 241.1 / 238.2 | 1.40-2.22 | 0 |
| runc/none/hardened | exec_first | 100 | 215.8 | 18.3 | 178.6 | 213.8 | 246.0 | 280.6 | 286.6 | 217.2 / 207.7 | 1.40-2.22 | 0 |
| runc/none/hardened | exec_again | 100 | 218.3 | 18.8 | 171.5 | 218.0 | 243.0 | 276.8 | 293.0 | 222.2 / 217.1 | 1.40-2.22 | 0 |
| runc/none/hardened | rm_f_running | 100 | 246.7 | 29.0 | 229.4 | 237.2 | 300.1 | 365.8 | 411.7 | 239.1 / 236.3 | 1.40-2.22 | 0 |
| runc/none/plain | run_rm_true | 100 | 382.6 | 10.7 | 363.3 | 381.4 | 403.1 | 413.9 | 426.9 | 386.0 / 377.6 | 1.65-2.16 | 0 |
| runc/none/plain | create | 100 | 56.8 | 1.9 | 53.0 | 56.6 | 59.3 | 62.4 | 67.6 | 56.2 / 57.0 | 1.65-2.10 | 0 |
| runc/none/plain | start | 100 | 177.1 | 7.0 | 161.3 | 177.3 | 186.9 | 192.8 | 212.2 | 179.8 / 174.4 | 1.65-2.10 | 0 |
| runc/none/plain | create_plus_start | 100 | 234.0 | 7.2 | 220.7 | 233.6 | 243.8 | 252.4 | 268.8 | 235.1 / 231.3 | 1.65-2.16 | 0 |
| runc/none/plain | exec_first | 100 | 216.6 | 13.0 | 194.5 | 217.7 | 236.5 | 241.0 | 247.4 | 223.9 / 214.7 | 1.65-2.10 | 0 |
| runc/none/plain | exec_again | 100 | 218.7 | 13.9 | 197.9 | 218.4 | 242.3 | 261.6 | 265.7 | 218.1 / 218.3 | 1.65-2.10 | 0 |
| runc/none/plain | rm_f_running | 100 | 234.5 | 8.3 | 220.6 | 233.3 | 242.9 | 271.1 | 275.8 | 232.7 / 234.4 | 1.65-2.10 | 0 |
| runc/pasta/hardened | run_rm_true | 100 | 446.2 | 57.1 | 399.8 | 432.0 | 529.5 | 621.9 | 860.1 | 432.2 / 431.0 | 1.39-3.36 | 0 |
| runc/pasta/hardened | create | 100 | 62.7 | 14.7 | 54.5 | 58.0 | 87.4 | 129.2 | 148.8 | 57.8 / 58.6 | 1.39-2.78 | 0 |
| runc/pasta/hardened | start | 100 | 223.0 | 28.8 | 190.0 | 215.8 | 287.6 | 359.0 | 390.1 | 214.2 / 217.6 | 1.39-2.78 | 0 |
| runc/pasta/hardened | create_plus_start | 100 | 285.7 | 39.2 | 252.0 | 273.2 | 372.7 | 470.5 | 476.4 | 272.8 / 276.9 | 1.39-3.36 | 0 |
| runc/pasta/hardened | exec_first | 100 | 224.6 | 36.9 | 174.9 | 218.7 | 258.9 | 361.5 | 479.7 | 217.7 / 220.9 | 1.39-2.78 | 0 |
| runc/pasta/hardened | exec_again | 100 | 223.7 | 33.5 | 164.3 | 219.0 | 257.1 | 369.2 | 443.9 | 216.5 / 221.2 | 1.39-2.78 | 0 |
| runc/pasta/hardened | rm_f_running | 100 | 255.5 | 35.8 | 225.2 | 244.6 | 352.8 | 427.4 | 436.3 | 244.0 / 247.9 | 1.39-2.78 | 0 |
| runc/pasta/plain | run_rm_true | 100 | 423.9 | 36.2 | 392.7 | 414.2 | 502.6 | 575.6 | 599.3 | 414.7 / 412.0 | 1.60-2.05 | 0 |
| runc/pasta/plain | create | 100 | 59.3 | 10.5 | 53.1 | 56.9 | 73.6 | 110.1 | 129.4 | 56.0 / 57.6 | 1.60-2.05 | 0 |
| runc/pasta/plain | start | 100 | 222.8 | 12.8 | 191.7 | 221.3 | 243.6 | 261.8 | 277.1 | 222.5 / 217.7 | 1.60-2.05 | 0 |
| runc/pasta/plain | create_plus_start | 100 | 282.1 | 20.0 | 247.5 | 277.8 | 320.9 | 367.7 | 371.5 | 278.6 / 275.1 | 1.60-2.05 | 0 |
| runc/pasta/plain | exec_first | 100 | 220.3 | 41.3 | 175.0 | 217.6 | 238.8 | 295.4 | 594.1 | 218.3 / 217.2 | 1.60-2.05 | 0 |
| runc/pasta/plain | exec_again | 100 | 228.9 | 97.0 | 173.2 | 219.8 | 263.6 | 357.9 | 1156.6 | 220.7 / 216.0 | 1.60-2.05 | 0 |
| runc/pasta/plain | rm_f_running | 100 | 254.5 | 46.6 | 218.4 | 245.5 | 289.0 | 331.4 | 690.5 | 245.2 / 247.1 | 1.60-2.05 | 0 |
| runc/slirp4netns/hardened | run_rm_true | 100 | 411.1 | 35.8 | 384.0 | 403.0 | 448.0 | 568.9 | 680.2 | 405.2 / 402.2 | 1.65-2.82 | 0 |
| runc/slirp4netns/hardened | create | 100 | 61.5 | 17.9 | 53.9 | 57.5 | 72.9 | 124.2 | 203.8 | 57.6 / 57.1 | 1.65-2.82 | 0 |
| runc/slirp4netns/hardened | start | 100 | 195.9 | 25.7 | 177.7 | 189.7 | 217.2 | 315.9 | 347.1 | 189.8 / 189.6 | 1.65-2.82 | 0 |
| runc/slirp4netns/hardened | create_plus_start | 100 | 257.4 | 42.5 | 235.7 | 247.3 | 290.0 | 440.1 | 551.0 | 248.1 / 246.4 | 1.65-2.82 | 0 |
| runc/slirp4netns/hardened | exec_first | 100 | 217.6 | 30.1 | 176.6 | 215.1 | 260.4 | 318.2 | 444.1 | 217.3 / 207.3 | 1.65-2.82 | 0 |
| runc/slirp4netns/hardened | exec_again | 100 | 221.0 | 20.3 | 174.9 | 219.5 | 255.5 | 277.2 | 348.8 | 218.6 / 220.3 | 1.65-2.82 | 0 |
| runc/slirp4netns/hardened | rm_f_running | 100 | 250.7 | 30.5 | 230.5 | 241.4 | 330.1 | 369.0 | 402.6 | 240.4 / 241.8 | 1.65-2.82 | 0 |
| runc/slirp4netns/plain | run_rm_true | 100 | 423.2 | 100.7 | 375.8 | 394.7 | 606.5 | 897.0 | 1033.4 | 394.7 / 394.6 | 1.41-3.25 | 0 |
| runc/slirp4netns/plain | create | 100 | 69.4 | 40.9 | 54.1 | 56.9 | 173.8 | 275.6 | 276.4 | 56.5 / 57.6 | 1.41-3.25 | 0 |
| runc/slirp4netns/plain | start | 100 | 204.8 | 80.6 | 167.7 | 183.3 | 327.7 | 626.0 | 719.0 | 182.9 / 185.3 | 1.41-3.25 | 0 |
| runc/slirp4netns/plain | create_plus_start | 100 | 274.1 | 119.9 | 224.2 | 240.7 | 501.6 | 901.6 | 995.4 | 240.4 / 242.1 | 1.41-3.25 | 0 |
| runc/slirp4netns/plain | exec_first | 100 | 226.2 | 48.0 | 187.8 | 216.2 | 316.8 | 456.6 | 466.2 | 214.1 / 217.6 | 1.41-3.25 | 0 |
| runc/slirp4netns/plain | exec_again | 100 | 227.7 | 51.1 | 175.8 | 219.0 | 281.6 | 505.1 | 535.4 | 210.8 / 224.7 | 1.41-3.25 | 0 |
| runc/slirp4netns/plain | rm_f_running | 100 | 267.0 | 73.9 | 231.9 | 244.1 | 420.1 | 559.8 | 698.9 | 244.1 / 244.4 | 1.41-3.25 | 0 |

## 1a. Storage driver x transient store (isolated stores, crun, network none)

| cell | op | n | mean | sd | min | p50 | p95 | p99 | max | per-round p50 | load1 | errors |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---:|
| fuse/persistent/crun/none/hardened | run_rm_true | 100 | 186.1 | 12.9 | 172.7 | 181.6 | 217.6 | 223.0 | 229.3 | 181.5 / 181.7 | 1.40-2.13 | 0 |
| fuse/persistent/crun/none/hardened | create | 100 | 57.0 | 3.5 | 53.7 | 56.5 | 58.7 | 64.1 | 88.6 | 56.1 / 56.9 | 1.40-2.13 | 0 |
| fuse/persistent/crun/none/hardened | start | 100 | 105.5 | 12.2 | 93.6 | 101.1 | 133.8 | 135.4 | 138.4 | 102.0 / 100.4 | 1.40-2.13 | 0 |
| fuse/persistent/crun/none/hardened | create_plus_start | 100 | 162.4 | 12.3 | 149.9 | 157.5 | 189.9 | 191.4 | 196.2 | 157.6 / 157.3 | 1.40-2.13 | 0 |
| fuse/persistent/crun/none/hardened | exec_first | 100 | 145.7 | 3.7 | 138.9 | 145.6 | 152.8 | 154.9 | 156.0 | 145.6 / 145.6 | 1.40-2.13 | 0 |
| fuse/persistent/crun/none/hardened | exec_again | 100 | 145.1 | 5.4 | 120.2 | 145.1 | 152.4 | 156.6 | 174.7 | 144.5 / 145.7 | 1.40-2.13 | 0 |
| fuse/persistent/crun/none/hardened | rm_f_running | 100 | 108.3 | 3.9 | 101.4 | 107.5 | 114.3 | 118.1 | 120.1 | 106.4 / 110.8 | 1.40-2.13 | 0 |
| fuse/persistent/crun/none/plain | run_rm_true | 100 | 181.1 | 13.5 | 161.5 | 177.0 | 214.0 | 222.6 | 239.7 | 176.8 / 178.1 | 1.43-1.72 | 0 |
| fuse/persistent/crun/none/plain | create | 100 | 55.8 | 1.3 | 52.5 | 55.8 | 57.9 | 58.4 | 59.3 | 55.8 / 55.7 | 1.43-1.72 | 0 |
| fuse/persistent/crun/none/plain | start | 100 | 100.2 | 8.7 | 85.1 | 98.3 | 107.2 | 135.6 | 142.9 | 98.3 / 98.4 | 1.43-1.72 | 0 |
| fuse/persistent/crun/none/plain | create_plus_start | 100 | 156.0 | 8.7 | 141.0 | 154.5 | 163.2 | 192.4 | 196.6 | 154.2 / 155.0 | 1.43-1.72 | 0 |
| fuse/persistent/crun/none/plain | exec_first | 100 | 145.9 | 4.6 | 138.8 | 146.0 | 150.5 | 153.2 | 179.1 | 146.1 / 145.9 | 1.43-1.72 | 0 |
| fuse/persistent/crun/none/plain | exec_again | 100 | 144.9 | 3.1 | 139.4 | 145.0 | 149.6 | 151.8 | 152.7 | 144.4 / 145.3 | 1.43-1.72 | 0 |
| fuse/persistent/crun/none/plain | rm_f_running | 100 | 107.5 | 5.2 | 94.3 | 106.9 | 117.3 | 123.5 | 124.3 | 105.3 / 108.3 | 1.43-1.72 | 0 |
| fuse/transient/crun/none/hardened | run_rm_true | 100 | 158.0 | 35.6 | 135.9 | 146.4 | 259.2 | 270.6 | 271.6 | 148.0 / 145.2 | 1.55-2.76 | 0 |
| fuse/transient/crun/none/hardened | create | 100 | 38.6 | 1.8 | 36.5 | 38.3 | 40.2 | 47.0 | 47.6 | 38.2 / 38.3 | 1.55-2.76 | 0 |
| fuse/transient/crun/none/hardened | start | 100 | 87.3 | 4.7 | 79.5 | 86.1 | 95.9 | 97.8 | 99.0 | 85.8 / 86.4 | 1.55-2.76 | 0 |
| fuse/transient/crun/none/hardened | create_plus_start | 100 | 125.8 | 5.1 | 117.2 | 124.9 | 135.4 | 137.3 | 142.7 | 123.9 / 125.0 | 1.55-2.76 | 0 |
| fuse/transient/crun/none/hardened | exec_first | 100 | 105.9 | 24.3 | 64.8 | 93.0 | 160.6 | 168.8 | 169.3 | 114.3 / 89.0 | 1.55-2.76 | 0 |
| fuse/transient/crun/none/hardened | exec_again | 100 | 99.2 | 16.7 | 67.4 | 90.2 | 121.4 | 127.4 | 176.6 | 89.7 / 91.9 | 1.55-2.76 | 0 |
| fuse/transient/crun/none/hardened | rm_f_running | 100 | 70.8 | 28.5 | 49.2 | 61.9 | 163.8 | 167.8 | 171.7 | 61.6 / 62.1 | 1.55-2.76 | 0 |
| fuse/transient/crun/none/plain | run_rm_true | 100 | 142.7 | 5.4 | 132.0 | 142.0 | 151.7 | 156.9 | 158.5 | 143.2 / 140.9 | 1.60-2.34 | 0 |
| fuse/transient/crun/none/plain | create | 100 | 37.6 | 1.0 | 35.8 | 37.6 | 38.9 | 39.1 | 43.6 | 37.8 / 37.6 | 1.60-2.34 | 0 |
| fuse/transient/crun/none/plain | start | 100 | 85.3 | 5.2 | 73.0 | 84.8 | 94.7 | 97.7 | 105.7 | 85.3 / 84.5 | 1.60-2.34 | 0 |
| fuse/transient/crun/none/plain | create_plus_start | 100 | 122.9 | 5.3 | 109.8 | 122.2 | 132.5 | 136.5 | 143.3 | 122.9 / 122.1 | 1.60-2.34 | 0 |
| fuse/transient/crun/none/plain | exec_first | 100 | 96.6 | 16.5 | 61.6 | 88.9 | 120.2 | 125.2 | 129.1 | 88.5 / 89.1 | 1.60-2.34 | 0 |
| fuse/transient/crun/none/plain | exec_again | 100 | 98.1 | 15.6 | 60.2 | 91.1 | 121.6 | 128.6 | 130.0 | 90.9 / 91.6 | 1.60-2.34 | 0 |
| fuse/transient/crun/none/plain | rm_f_running | 100 | 61.4 | 2.9 | 48.5 | 61.5 | 65.7 | 67.9 | 73.3 | 61.5 / 61.5 | 1.60-2.34 | 0 |
| native/persistent/crun/none/hardened | run_rm_true | 100 | 175.0 | 5.7 | 162.6 | 173.7 | 183.6 | 185.5 | 209.1 | 174.2 / 173.4 | 1.63-1.96 | 0 |
| native/persistent/crun/none/hardened | create | 100 | 56.7 | 1.3 | 54.2 | 56.7 | 58.8 | 60.8 | 61.0 | 56.8 / 56.5 | 1.63-2.04 | 0 |
| native/persistent/crun/none/hardened | start | 100 | 97.2 | 7.9 | 87.9 | 95.5 | 104.2 | 130.6 | 135.1 | 95.7 / 94.8 | 1.63-1.96 | 0 |
| native/persistent/crun/none/hardened | create_plus_start | 100 | 153.9 | 8.0 | 144.0 | 152.2 | 162.4 | 186.1 | 192.3 | 152.8 / 151.2 | 1.63-1.96 | 0 |
| native/persistent/crun/none/hardened | exec_first | 100 | 157.9 | 15.7 | 115.1 | 162.9 | 177.2 | 182.0 | 204.8 | 160.6 / 165.0 | 1.63-1.96 | 0 |
| native/persistent/crun/none/hardened | exec_again | 100 | 157.6 | 12.9 | 117.5 | 162.1 | 174.8 | 178.1 | 180.4 | 161.3 / 165.2 | 1.63-1.96 | 0 |
| native/persistent/crun/none/hardened | rm_f_running | 100 | 117.7 | 6.5 | 99.9 | 117.2 | 130.5 | 135.5 | 151.0 | 117.2 / 117.2 | 1.63-1.96 | 0 |
| native/persistent/crun/none/plain | run_rm_true | 100 | 173.5 | 9.3 | 155.0 | 172.0 | 191.7 | 208.0 | 211.1 | 172.5 / 170.9 | 1.79-2.62 | 0 |
| native/persistent/crun/none/plain | create | 100 | 55.7 | 1.3 | 52.8 | 55.6 | 57.8 | 58.7 | 60.1 | 55.6 / 55.6 | 1.79-2.62 | 0 |
| native/persistent/crun/none/plain | start | 100 | 95.0 | 7.5 | 84.6 | 93.8 | 100.8 | 132.9 | 135.5 | 93.1 / 93.9 | 1.79-2.62 | 0 |
| native/persistent/crun/none/plain | create_plus_start | 100 | 150.7 | 7.5 | 140.3 | 149.6 | 157.7 | 189.6 | 190.1 | 149.6 / 149.6 | 1.79-2.62 | 0 |
| native/persistent/crun/none/plain | exec_first | 100 | 158.9 | 14.4 | 138.0 | 164.1 | 178.3 | 186.2 | 187.1 | 162.2 / 166.2 | 1.79-2.62 | 0 |
| native/persistent/crun/none/plain | exec_again | 100 | 156.0 | 13.9 | 120.3 | 157.9 | 176.0 | 179.8 | 182.2 | 160.3 / 149.2 | 1.79-2.62 | 0 |
| native/persistent/crun/none/plain | rm_f_running | 100 | 117.6 | 7.5 | 101.8 | 116.9 | 129.2 | 134.7 | 153.3 | 112.3 / 120.6 | 1.79-2.62 | 0 |
| native/transient/crun/none/hardened | run_rm_true | 100 | 135.1 | 4.8 | 122.3 | 134.9 | 142.6 | 145.4 | 147.4 | 135.6 / 134.6 | 1.52-1.69 | 0 |
| native/transient/crun/none/hardened | create | 100 | 38.2 | 0.9 | 36.3 | 38.1 | 39.7 | 40.8 | 40.9 | 38.2 / 38.1 | 1.52-1.69 | 0 |
| native/transient/crun/none/hardened | start | 100 | 78.6 | 4.0 | 70.1 | 77.9 | 85.9 | 87.6 | 88.8 | 78.9 / 77.2 | 1.52-1.69 | 0 |
| native/transient/crun/none/hardened | create_plus_start | 100 | 116.9 | 4.1 | 108.1 | 116.3 | 123.8 | 126.4 | 127.8 | 117.8 / 115.3 | 1.52-1.69 | 0 |
| native/transient/crun/none/hardened | exec_first | 100 | 98.8 | 15.2 | 80.3 | 89.8 | 119.7 | 123.5 | 126.1 | 90.2 / 89.4 | 1.52-1.69 | 0 |
| native/transient/crun/none/hardened | exec_again | 100 | 96.1 | 15.3 | 62.8 | 88.6 | 120.6 | 123.4 | 125.6 | 88.3 / 88.7 | 1.52-1.69 | 0 |
| native/transient/crun/none/hardened | rm_f_running | 100 | 60.0 | 3.8 | 47.8 | 59.5 | 64.8 | 74.6 | 76.9 | 59.6 / 59.5 | 1.52-1.69 | 0 |
| native/transient/crun/none/plain | run_rm_true | 100 | 133.9 | 4.3 | 125.5 | 133.4 | 141.4 | 146.7 | 147.8 | 133.5 / 132.8 | 1.58-1.78 | 0 |
| native/transient/crun/none/plain | create | 100 | 37.6 | 1.6 | 35.7 | 37.5 | 38.8 | 43.6 | 50.0 | 37.6 / 37.3 | 1.58-1.78 | 0 |
| native/transient/crun/none/plain | start | 100 | 79.3 | 4.9 | 69.9 | 78.5 | 87.2 | 94.3 | 95.5 | 78.3 / 79.1 | 1.58-1.78 | 0 |
| native/transient/crun/none/plain | create_plus_start | 100 | 117.0 | 5.6 | 107.8 | 116.2 | 125.0 | 133.9 | 144.2 | 115.8 / 116.4 | 1.58-1.78 | 0 |
| native/transient/crun/none/plain | exec_first | 100 | 99.2 | 15.5 | 81.0 | 90.2 | 120.8 | 127.2 | 127.4 | 90.3 / 89.5 | 1.58-1.78 | 0 |
| native/transient/crun/none/plain | exec_again | 100 | 97.8 | 15.3 | 62.1 | 90.2 | 120.5 | 126.7 | 129.9 | 92.0 / 89.7 | 1.58-1.78 | 0 |
| native/transient/crun/none/plain | rm_f_running | 100 | 59.7 | 3.0 | 48.5 | 59.3 | 64.2 | 70.6 | 74.8 | 59.7 / 59.2 | 1.58-1.78 | 0 |

Isolated store facts: `{"iso1-native": {"graphDriverName": "overlay", "graphStatus": {"Backing Filesystem": "extfs", "Native Overlay Diff": "true", "Supports d_type": "true", "Supports shifting": "false", "Supports volatile": "true", "Using metacopy": "false"}, "transientStore": false, "graphRoot": "/home/user/warmbox/bench/.work/25ad6/s1", "runRoot": "/run/user/1000/sbxb-25ad6/r1", "graphOptions": {}}, "iso2-native-transient": {"graphDriverName": "overlay", "graphStatus": {"Backing Filesystem": "extfs", "Native Overlay Diff": "true", "Supports d_type": "true", "Supports shifting": "false", "Supports volatile": "true", "Using metacopy": "false"}, "transientStore": true, "graphRoot": "/home/user/warmbox/bench/.work/25ad6/s2", "runRoot": "/run/user/1000/sbxb-25ad6/r2", "graphOptions": {}}, "iso3-fuse": {"graphDriverName": "overlay", "graphStatus": {"Backing Filesystem": "extfs", "Native Overlay Diff": "false", "Supports d_type": "true", "Supports shifting": "true", "Supports volatile": "true", "Using metacopy": "false"}, "transientStore": false, "graphRoot": "/home/user/warmbox/bench/.work/25ad6/s3", "runRoot": "/run/user/1000/sbxb-25ad6/r3", "graphOptions": {"overlay.mount_program": {"Executable": "/usr/bin/fuse-overlayfs", "Package": "fuse-overlayfs_1.13-1_amd64", "Version": "fusermount3 version: 3.14.0\nfuse-overlayfs: version 1.13-dev\nFUSE library version 3.14.0\nusing FUSE kernel interface version 7.31"}}}, "iso4-fuse-transient": {"graphDriverName": "overlay", "graphStatus": {"Backing Filesystem": "extfs", "Native Overlay Diff": "false", "Supports d_type": "true", "Supports shifting": "true", "Supports volatile": "true", "Using metacopy": "false"}, "transientStore": true, "graphRoot": "/home/user/warmbox/bench/.work/25ad6/s4", "runRoot": "/run/user/1000/sbxb-25ad6/r4", "graphOptions": {"overlay.mount_program": {"Executable": "/usr/bin/fuse-overlayfs", "Package": "fuse-overlayfs_1.13-1_amd64", "Version": "fusermount3 version: 3.14.0\nfuse-overlayfs: version 1.13-dev\nFUSE library version 3.14.0\nusing FUSE kernel interface version 7.31"}}}}`

## 1b. REST API versus CLI, hardened crun/none with the real agent as PID 1

| cell | op | n | mean | sd | min | p50 | p95 | p99 | max | per-round p50 | load1 | errors |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---:|
| cli/crun/none/hardened+agent | create | 100 | 57.2 | 1.4 | 54.1 | 57.3 | 59.1 | 60.2 | 62.1 | 57.2 / 57.3 | 1.53-1.78 | 0 |
| cli/crun/none/hardened+agent | create_plus_start | 100 | 153.3 | 9.1 | 140.4 | 151.4 | 164.5 | 189.1 | 192.3 | 150.2 / 151.8 | 1.53-1.78 | 0 |
| cli/crun/none/hardened+agent | create_to_agent_ready | 100 | 154.2 | 9.1 | 141.4 | 152.2 | 165.4 | 190.0 | 193.1 | 151.1 / 152.8 | 1.53-1.78 | 0 |
| cli/crun/none/hardened+agent | podman_exec_true_again | 100 | 155.2 | 15.0 | 138.2 | 147.2 | 177.0 | 185.4 | 212.2 | 147.6 / 146.9 | 1.47-1.78 | 0 |
| cli/crun/none/hardened+agent | podman_exec_true_first | 100 | 157.2 | 15.8 | 137.5 | 148.7 | 178.0 | 184.1 | 212.3 | 147.9 / 149.2 | 1.53-1.78 | 0 |
| cli/crun/none/hardened+agent | rm_f_running | 100 | 118.2 | 6.2 | 103.5 | 117.6 | 127.4 | 143.3 | 147.7 | 116.0 / 118.4 | 1.47-1.78 | 0 |
| cli/crun/none/hardened+agent | start | 100 | 96.0 | 8.9 | 84.7 | 94.4 | 107.9 | 131.3 | 134.5 | 93.4 / 95.1 | 1.53-1.78 | 0 |
| cli/crun/none/hardened+agent | start_return_to_agent_ready | 100 | 0.9 | 0.1 | 0.7 | 0.9 | 1.1 | 1.1 | 1.1 | 0.9 / 0.9 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | agent_exec_true_again | 100 | 1.2 | 0.1 | 1.1 | 1.2 | 1.3 | 1.4 | 1.6 | 1.2 / 1.2 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | agent_exec_true_first | 100 | 1.5 | 0.1 | 1.3 | 1.5 | 1.6 | 1.7 | 1.9 | 1.5 / 1.5 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | agent_init_rpc | 100 | 0.5 | 0.1 | 0.4 | 0.5 | 0.6 | 0.6 | 0.6 | 0.5 / 0.5 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | create | 100 | 25.0 | 3.2 | 21.9 | 24.7 | 27.1 | 28.7 | 54.1 | 24.7 / 24.7 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | create_plus_start | 100 | 86.4 | 5.1 | 76.3 | 85.8 | 94.1 | 95.7 | 117.9 | 85.4 / 86.2 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | create_to_agent_ready | 100 | 87.2 | 5.1 | 77.1 | 86.6 | 94.9 | 96.4 | 118.7 | 86.2 / 86.9 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | podman_exec_true_again | 100 | 75.3 | 3.7 | 68.4 | 75.2 | 81.0 | 84.2 | 84.9 | 75.0 / 75.6 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | podman_exec_true_first | 100 | 76.5 | 4.8 | 68.4 | 76.2 | 82.0 | 89.3 | 104.2 | 76.2 / 76.2 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | rm_f_running | 100 | 91.1 | 6.4 | 84.2 | 90.0 | 95.8 | 123.5 | 126.2 | 88.7 / 91.8 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | start | 100 | 61.5 | 3.9 | 51.4 | 61.3 | 68.3 | 69.8 | 70.8 | 60.9 / 62.1 | 1.53-1.78 | 0 |
| rest-api/crun/none/hardened+agent | start_return_to_agent_ready | 100 | 0.7 | 0.0 | 0.6 | 0.7 | 0.8 | 0.9 | 0.9 | 0.8 / 0.7 | 1.53-1.78 | 0 |

## 2. User-namespace mapping cost (fresh isolated store per repetition)

Cell = storage/image/mapping. `first_*` is the first container of that image with that mapping in a brand-new store (n = repetitions); `subsequent_*` are later containers in the same store.

| cell | op | n | mean | sd | min | p50 | p95 | p99 | max | per-round p50 | load1 | errors |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---:|
| auto-exhaustion/alpine:3.20 | create_nth | 64 | 79.7 | 24.4 | 72.7 | 76.7 | 79.3 | 150.7 | 271.7 | 76.7 | 2.29-2.31 | 0 |
| fuse/alpine:3.20/auto | first_create | 3 | 73.8 | 0.7 | 73.2 | 73.7 | 74.4 | 74.5 | 74.5 | 73.7 / 73.2 / 74.5 | 2.28-2.79 | 0 |
| fuse/alpine:3.20/auto | first_start | 3 | 99.6 | 2.3 | 97.8 | 98.8 | 101.9 | 102.1 | 102.2 | 98.8 / 102.2 / 97.8 | 2.28-2.79 | 0 |
| fuse/alpine:3.20/auto | first_create_plus_start | 3 | 173.4 | 1.7 | 172.3 | 172.6 | 175.1 | 175.3 | 175.4 | 172.6 / 175.4 / 172.3 | 2.28-2.79 | 0 |
| fuse/alpine:3.20/auto | subsequent_create | 60 | 75.3 | 17.1 | 68.9 | 72.7 | 75.8 | 129.1 | 205.1 | 73.7 / 72.2 / 72.6 | 2.26-2.79 | 0 |
| fuse/alpine:3.20/auto | subsequent_start | 60 | 106.4 | 35.8 | 93.0 | 100.1 | 127.8 | 231.2 | 371.8 | 100.0 / 102.1 / 98.0 | 2.26-2.79 | 0 |
| fuse/alpine:3.20/auto | subsequent_create_plus_start | 60 | 181.7 | 39.0 | 163.9 | 173.6 | 201.8 | 359.6 | 444.0 | 173.7 / 174.3 / 172.4 | 2.26-2.79 | 0 |
| fuse/alpine:3.20/auto | auto_while_others_alive_create | 18 | 74.6 | 1.8 | 72.2 | 74.1 | 77.3 | 78.5 | 78.8 | 73.7 / 73.8 / 74.7 | 2.26-2.65 | 0 |
| fuse/alpine:3.20/host(default) | first_create | 3 | 56.6 | 0.4 | 56.2 | 56.5 | 56.9 | 57.0 | 57.0 | 57.0 / 56.2 / 56.5 | 2.27-2.59 | 0 |
| fuse/alpine:3.20/host(default) | first_start | 3 | 115.2 | 14.0 | 99.1 | 122.8 | 123.6 | 123.6 | 123.7 | 99.0 / 123.7 / 122.8 | 2.27-2.59 | 0 |
| fuse/alpine:3.20/host(default) | first_create_plus_start | 3 | 171.8 | 13.6 | 156.0 | 179.3 | 179.8 | 179.9 | 179.9 | 156.0 / 179.9 / 179.3 | 2.27-2.59 | 0 |
| fuse/alpine:3.20/host(default) | subsequent_create | 60 | 56.3 | 2.0 | 53.4 | 56.1 | 59.7 | 63.4 | 66.4 | 55.6 / 56.5 / 55.7 | 2.27-3.21 | 0 |
| fuse/alpine:3.20/host(default) | subsequent_start | 60 | 100.0 | 8.3 | 88.6 | 98.0 | 126.1 | 130.3 | 133.7 | 99.4 / 97.8 / 96.9 | 2.27-3.21 | 0 |
| fuse/alpine:3.20/host(default) | subsequent_create_plus_start | 60 | 156.4 | 8.6 | 144.5 | 153.7 | 181.2 | 186.3 | 190.2 | 154.8 / 154.8 / 152.9 | 2.27-3.21 | 0 |
| fuse/alpine:3.20/keep-id | first_create | 3 | 58.3 | 2.8 | 55.9 | 57.6 | 61.0 | 61.3 | 61.3 | 57.6 / 55.9 / 61.3 | 2.46-3.21 | 0 |
| fuse/alpine:3.20/keep-id | first_start | 3 | 113.8 | 14.3 | 105.6 | 105.6 | 127.8 | 129.8 | 130.3 | 105.6 / 105.6 / 130.3 | 2.46-3.21 | 0 |
| fuse/alpine:3.20/keep-id | first_create_plus_start | 3 | 172.1 | 16.9 | 161.5 | 163.2 | 188.8 | 191.0 | 191.6 | 163.2 / 161.5 / 191.6 | 2.46-3.21 | 0 |
| fuse/alpine:3.20/keep-id | subsequent_create | 60 | 57.4 | 5.0 | 53.2 | 56.7 | 60.0 | 76.6 | 92.2 | 57.3 / 56.4 / 56.1 | 2.24-3.21 | 0 |
| fuse/alpine:3.20/keep-id | subsequent_start | 60 | 109.5 | 15.6 | 88.2 | 102.4 | 139.1 | 148.4 | 154.5 | 104.5 / 103.8 / 99.6 | 2.24-3.21 | 0 |
| fuse/alpine:3.20/keep-id | subsequent_create_plus_start | 60 | 166.9 | 16.0 | 143.4 | 159.7 | 194.8 | 206.1 | 212.6 | 164.4 / 159.6 / 156.4 | 2.24-3.21 | 0 |
| fuse/alpine:3.20/keep-id:uid=1000,gid=1000 | first_create | 3 | 57.3 | 1.1 | 56.0 | 57.8 | 58.0 | 58.0 | 58.0 | 56.0 / 58.0 / 57.8 | 2.24-3.11 | 0 |
| fuse/alpine:3.20/keep-id:uid=1000,gid=1000 | first_start | 3 | 100.8 | 0.4 | 100.5 | 100.7 | 101.2 | 101.2 | 101.2 | 101.2 / 100.7 / 100.5 | 2.24-3.11 | 0 |
| fuse/alpine:3.20/keep-id:uid=1000,gid=1000 | first_create_plus_start | 3 | 158.1 | 0.8 | 157.2 | 158.3 | 158.7 | 158.8 | 158.8 | 157.2 / 158.8 / 158.2 | 2.24-3.11 | 0 |
| fuse/alpine:3.20/keep-id:uid=1000,gid=1000 | subsequent_create | 60 | 56.5 | 2.0 | 52.5 | 56.2 | 59.1 | 63.3 | 67.3 | 56.2 / 55.8 / 56.8 | 2.22-2.95 | 0 |
| fuse/alpine:3.20/keep-id:uid=1000,gid=1000 | subsequent_start | 60 | 106.0 | 13.7 | 95.0 | 100.0 | 136.1 | 142.5 | 145.0 | 99.6 / 102.0 / 99.0 | 2.22-2.95 | 0 |
| fuse/alpine:3.20/keep-id:uid=1000,gid=1000 | subsequent_create_plus_start | 60 | 162.5 | 13.6 | 148.5 | 156.9 | 193.5 | 198.2 | 200.8 | 156.8 / 158.6 / 156.4 | 2.22-2.95 | 0 |
| fuse/alpine:3.20/nomap | first_create | 3 | 56.9 | 2.5 | 54.1 | 57.5 | 58.9 | 59.0 | 59.1 | 57.5 / 59.1 / 54.1 | 2.22-2.95 | 0 |
| fuse/alpine:3.20/nomap | first_start | 3 | 100.8 | 1.9 | 98.6 | 101.9 | 102.0 | 102.0 | 102.0 | 98.6 / 101.9 / 102.0 | 2.22-2.95 | 0 |
| fuse/alpine:3.20/nomap | first_create_plus_start | 3 | 157.7 | 2.8 | 156.1 | 156.2 | 160.5 | 160.9 | 161.0 | 156.1 / 160.9 / 156.2 | 2.22-2.95 | 0 |
| fuse/alpine:3.20/nomap | subsequent_create | 60 | 56.8 | 4.1 | 52.8 | 56.1 | 60.2 | 74.0 | 84.1 | 56.1 / 56.3 / 56.1 | 2.22-2.95 | 0 |
| fuse/alpine:3.20/nomap | subsequent_start | 60 | 101.8 | 9.2 | 93.0 | 99.3 | 126.4 | 133.4 | 137.6 | 99.6 / 99.4 / 99.3 | 2.22-2.95 | 0 |
| fuse/alpine:3.20/nomap | subsequent_create_plus_start | 60 | 158.6 | 10.0 | 149.3 | 155.7 | 183.6 | 190.6 | 195.2 | 155.0 / 155.7 / 155.9 | 2.22-2.95 | 0 |
| fuse/python:3.12-alpine/auto | first_create | 3 | 77.8 | 0.7 | 77.2 | 77.7 | 78.4 | 78.5 | 78.5 | 77.7 / 78.5 / 77.2 | 2.29-2.64 | 0 |
| fuse/python:3.12-alpine/auto | first_start | 3 | 119.3 | 13.6 | 103.5 | 126.7 | 127.4 | 127.5 | 127.5 | 127.5 / 103.5 / 126.7 | 2.29-2.64 | 0 |
| fuse/python:3.12-alpine/auto | first_create_plus_start | 3 | 197.0 | 13.0 | 182.0 | 203.9 | 205.1 | 205.2 | 205.2 | 205.2 / 182.0 / 203.9 | 2.29-2.64 | 0 |
| fuse/python:3.12-alpine/auto | subsequent_create | 60 | 76.8 | 3.8 | 72.7 | 76.3 | 81.1 | 90.1 | 101.3 | 76.8 / 76.7 / 75.3 | 2.27-2.64 | 0 |
| fuse/python:3.12-alpine/auto | subsequent_start | 60 | 106.3 | 12.3 | 92.5 | 100.8 | 131.9 | 137.2 | 141.7 | 102.2 / 100.3 / 101.5 | 2.27-2.64 | 0 |
| fuse/python:3.12-alpine/auto | subsequent_create_plus_start | 60 | 183.1 | 13.6 | 165.2 | 178.3 | 209.2 | 223.1 | 223.5 | 179.8 / 175.4 / 176.1 | 2.27-2.64 | 0 |
| fuse/python:3.12-alpine/auto | auto_while_others_alive_create | 18 | 77.7 | 3.4 | 73.4 | 77.2 | 80.6 | 88.1 | 89.9 | 77.7 / 77.1 / 77.0 | 2.27-2.59 | 0 |
| fuse/python:3.12-alpine/host(default) | first_create | 3 | 57.6 | 1.0 | 56.7 | 57.6 | 58.5 | 58.6 | 58.6 | 58.6 / 56.7 / 57.6 | 2.93-3.01 | 0 |
| fuse/python:3.12-alpine/host(default) | first_start | 3 | 116.5 | 16.4 | 97.6 | 124.6 | 127.0 | 127.2 | 127.2 | 97.6 / 127.2 / 124.6 | 2.93-3.01 | 0 |
| fuse/python:3.12-alpine/host(default) | first_create_plus_start | 3 | 174.1 | 15.5 | 156.2 | 182.2 | 183.7 | 183.9 | 183.9 | 156.2 / 183.9 / 182.2 | 2.93-3.01 | 0 |
| fuse/python:3.12-alpine/host(default) | subsequent_create | 60 | 58.6 | 4.7 | 55.4 | 57.6 | 61.0 | 77.4 | 90.3 | 57.6 / 57.8 / 57.5 | 2.77-3.01 | 0 |
| fuse/python:3.12-alpine/host(default) | subsequent_start | 60 | 105.6 | 12.6 | 93.1 | 100.9 | 132.7 | 138.3 | 141.9 | 101.5 / 103.1 / 99.0 | 2.77-3.01 | 0 |
| fuse/python:3.12-alpine/host(default) | subsequent_create_plus_start | 60 | 164.2 | 13.4 | 148.9 | 158.8 | 192.9 | 197.3 | 201.7 | 159.8 / 161.4 / 156.7 | 2.77-3.01 | 0 |
| fuse/python:3.12-alpine/keep-id | first_create | 3 | 58.5 | 1.0 | 57.6 | 58.3 | 59.5 | 59.6 | 59.7 | 57.6 / 58.3 / 59.7 | 2.71-2.85 | 0 |
| fuse/python:3.12-alpine/keep-id | first_start | 3 | 119.9 | 17.0 | 100.5 | 126.8 | 131.8 | 132.3 | 132.4 | 126.8 / 132.4 / 100.5 | 2.71-2.85 | 0 |
| fuse/python:3.12-alpine/keep-id | first_create_plus_start | 3 | 178.4 | 16.1 | 160.1 | 184.4 | 190.0 | 190.5 | 190.7 | 184.4 / 190.7 / 160.1 | 2.71-2.85 | 0 |
| fuse/python:3.12-alpine/keep-id | subsequent_create | 60 | 57.8 | 1.3 | 55.6 | 57.6 | 60.5 | 61.7 | 61.9 | 57.5 / 58.0 / 57.5 | 2.58-2.94 | 0 |
| fuse/python:3.12-alpine/keep-id | subsequent_start | 60 | 111.2 | 13.4 | 96.7 | 103.9 | 133.1 | 135.2 | 136.8 | 103.9 / 102.2 / 123.9 | 2.58-2.94 | 0 |
| fuse/python:3.12-alpine/keep-id | subsequent_create_plus_start | 60 | 169.0 | 13.1 | 153.3 | 161.8 | 189.9 | 192.8 | 194.5 | 161.4 / 160.2 / 180.6 | 2.58-2.94 | 0 |
| fuse/python:3.12-alpine/keep-id:uid=1000,gid=1000 | first_create | 3 | 60.3 | 1.1 | 59.1 | 60.5 | 61.1 | 61.2 | 61.2 | 60.5 / 61.2 / 59.1 | 2.58-2.94 | 0 |
| fuse/python:3.12-alpine/keep-id:uid=1000,gid=1000 | first_start | 3 | 114.9 | 19.8 | 100.4 | 106.8 | 134.4 | 136.8 | 137.4 | 106.8 / 100.4 / 137.4 | 2.58-2.94 | 0 |
| fuse/python:3.12-alpine/keep-id:uid=1000,gid=1000 | first_create_plus_start | 3 | 175.1 | 18.7 | 161.6 | 167.3 | 193.6 | 195.9 | 196.5 | 167.3 / 161.6 / 196.5 | 2.58-2.94 | 0 |
| fuse/python:3.12-alpine/keep-id:uid=1000,gid=1000 | subsequent_create | 60 | 58.2 | 1.7 | 56.0 | 57.9 | 60.7 | 63.7 | 66.6 | 58.0 / 58.1 / 57.0 | 2.53-2.94 | 0 |
| fuse/python:3.12-alpine/keep-id:uid=1000,gid=1000 | subsequent_start | 60 | 106.7 | 13.2 | 94.0 | 100.9 | 133.3 | 137.8 | 140.9 | 100.5 / 101.4 / 100.9 | 2.53-2.94 | 0 |
| fuse/python:3.12-alpine/keep-id:uid=1000,gid=1000 | subsequent_create_plus_start | 60 | 164.9 | 13.4 | 150.8 | 159.2 | 192.3 | 195.8 | 199.8 | 159.3 / 160.2 / 157.8 | 2.53-2.94 | 0 |
| fuse/python:3.12-alpine/nomap | first_create | 3 | 62.2 | 7.4 | 57.7 | 58.1 | 69.5 | 70.5 | 70.7 | 58.1 / 70.7 / 57.7 | 2.53-2.94 | 0 |
| fuse/python:3.12-alpine/nomap | first_start | 3 | 104.1 | 2.3 | 102.4 | 103.1 | 106.3 | 106.6 | 106.7 | 106.7 / 103.1 / 102.4 | 2.53-2.94 | 0 |
| fuse/python:3.12-alpine/nomap | first_create_plus_start | 3 | 166.2 | 7.0 | 160.0 | 164.8 | 172.9 | 173.7 | 173.8 | 164.8 / 173.8 / 160.1 | 2.53-2.94 | 0 |
| fuse/python:3.12-alpine/nomap | subsequent_create | 60 | 58.0 | 1.6 | 55.6 | 57.9 | 60.9 | 62.1 | 62.3 | 58.1 / 58.8 / 56.7 | 2.41-2.79 | 0 |
| fuse/python:3.12-alpine/nomap | subsequent_start | 60 | 103.8 | 9.8 | 93.6 | 101.3 | 126.5 | 137.4 | 141.0 | 101.8 / 101.4 / 100.6 | 2.41-2.79 | 0 |
| fuse/python:3.12-alpine/nomap | subsequent_create_plus_start | 60 | 161.9 | 10.1 | 150.7 | 159.0 | 186.5 | 195.6 | 197.9 | 159.6 / 160.8 / 158.1 | 2.41-2.79 | 0 |
| native/alpine:3.20/auto | first_create | 3 | 242.9 | 12.6 | 228.4 | 248.9 | 251.2 | 251.4 | 251.4 | 228.4 / 251.4 / 248.9 | 2.49-3.36 | 0 |
| native/alpine:3.20/auto | first_start | 3 | 91.3 | 1.1 | 90.1 | 91.4 | 92.2 | 92.3 | 92.3 | 92.3 / 91.4 / 90.1 | 2.49-3.36 | 0 |
| native/alpine:3.20/auto | first_create_plus_start | 3 | 334.2 | 11.8 | 320.7 | 339.0 | 342.4 | 342.7 | 342.8 | 320.7 / 342.8 / 339.0 | 2.49-3.36 | 0 |
| native/alpine:3.20/auto | subsequent_create | 60 | 77.4 | 2.4 | 73.9 | 77.4 | 80.7 | 84.2 | 87.9 | 77.7 / 77.5 / 76.1 | 2.49-3.18 | 0 |
| native/alpine:3.20/auto | subsequent_start | 60 | 96.4 | 11.1 | 86.9 | 93.9 | 104.4 | 145.0 | 146.6 | 93.4 / 93.9 / 94.2 | 2.49-3.18 | 0 |
| native/alpine:3.20/auto | subsequent_create_plus_start | 60 | 173.8 | 11.2 | 163.7 | 171.3 | 185.4 | 222.8 | 224.5 | 170.3 / 172.7 / 171.5 | 2.49-3.18 | 0 |
| native/alpine:3.20/auto | auto_while_others_alive_create | 18 | 197.3 | 56.4 | 73.8 | 218.0 | 232.2 | 236.7 | 237.9 | 215.9 / 222.4 / 218.2 | 2.93-3.01 | 0 |
| native/alpine:3.20/host(default) | first_create | 3 | 56.5 | 1.3 | 55.1 | 56.9 | 57.5 | 57.5 | 57.5 | 57.5 / 56.9 / 55.0 | 2.46-3.21 | 0 |
| native/alpine:3.20/host(default) | first_start | 3 | 95.7 | 1.3 | 94.1 | 96.2 | 96.6 | 96.6 | 96.6 | 96.6 / 96.2 / 94.1 | 2.46-3.21 | 0 |
| native/alpine:3.20/host(default) | first_create_plus_start | 3 | 152.2 | 2.6 | 149.2 | 153.2 | 154.1 | 154.1 | 154.2 | 154.2 / 153.2 / 149.2 | 2.46-3.21 | 0 |
| native/alpine:3.20/host(default) | subsequent_create | 60 | 55.6 | 2.3 | 53.1 | 55.2 | 58.2 | 64.8 | 67.0 | 55.4 / 55.2 / 55.0 | 2.46-3.21 | 0 |
| native/alpine:3.20/host(default) | subsequent_start | 60 | 94.3 | 4.6 | 86.1 | 93.4 | 100.8 | 108.5 | 111.9 | 92.5 / 96.8 / 92.9 | 2.46-3.21 | 0 |
| native/alpine:3.20/host(default) | subsequent_create_plus_start | 60 | 149.9 | 5.6 | 139.2 | 149.2 | 158.8 | 169.1 | 173.1 | 148.9 / 151.4 / 147.9 | 2.46-3.21 | 0 |
| native/alpine:3.20/keep-id | first_create | 3 | 221.6 | 6.7 | 215.0 | 221.4 | 227.8 | 228.3 | 228.5 | 221.4 / 228.5 / 215.0 | 2.82-3.19 | 0 |
| native/alpine:3.20/keep-id | first_start | 3 | 102.3 | 6.6 | 97.2 | 100.0 | 108.8 | 109.6 | 109.8 | 97.2 / 109.8 / 100.0 | 2.82-3.19 | 0 |
| native/alpine:3.20/keep-id | first_create_plus_start | 3 | 323.9 | 12.5 | 315.0 | 318.6 | 336.3 | 337.8 | 338.2 | 318.6 / 338.2 / 315.0 | 2.82-3.19 | 0 |
| native/alpine:3.20/keep-id | subsequent_create | 60 | 56.8 | 2.6 | 53.1 | 56.4 | 62.0 | 66.2 | 70.0 | 56.8 / 55.5 / 56.4 | 2.68-3.41 | 0 |
| native/alpine:3.20/keep-id | subsequent_start | 60 | 102.3 | 6.5 | 92.9 | 101.2 | 108.9 | 123.6 | 140.6 | 102.4 / 102.2 / 100.1 | 2.68-3.41 | 0 |
| native/alpine:3.20/keep-id | subsequent_create_plus_start | 60 | 159.1 | 6.8 | 148.6 | 158.6 | 169.2 | 181.6 | 195.8 | 159.6 / 159.8 / 156.1 | 2.68-3.41 | 0 |
| native/alpine:3.20/keep-id:uid=1000,gid=1000 | first_create | 3 | 217.9 | 7.0 | 213.5 | 214.2 | 224.7 | 225.7 | 225.9 | 213.5 / 225.9 / 214.2 | 2.68-3.41 | 0 |
| native/alpine:3.20/keep-id:uid=1000,gid=1000 | first_start | 3 | 105.3 | 2.4 | 102.5 | 106.7 | 106.8 | 106.8 | 106.8 | 102.5 / 106.7 / 106.8 | 2.68-3.41 | 0 |
| native/alpine:3.20/keep-id:uid=1000,gid=1000 | first_create_plus_start | 3 | 323.2 | 8.5 | 316.0 | 321.1 | 331.5 | 332.4 | 332.6 | 316.0 / 332.6 / 321.1 | 2.68-3.41 | 0 |
| native/alpine:3.20/keep-id:uid=1000,gid=1000 | subsequent_create | 60 | 56.2 | 1.3 | 53.1 | 56.2 | 58.7 | 59.1 | 59.4 | 55.9 / 56.8 / 56.0 | 2.54-3.41 | 0 |
| native/alpine:3.20/keep-id:uid=1000,gid=1000 | subsequent_start | 60 | 101.4 | 7.4 | 94.1 | 99.9 | 109.4 | 126.6 | 148.0 | 99.9 / 101.1 / 98.1 | 2.54-3.41 | 0 |
| native/alpine:3.20/keep-id:uid=1000,gid=1000 | subsequent_create_plus_start | 60 | 157.6 | 7.6 | 148.4 | 156.2 | 165.2 | 184.6 | 203.9 | 156.3 / 157.2 / 154.6 | 2.54-3.41 | 0 |
| native/alpine:3.20/nomap | first_create | 3 | 217.0 | 3.2 | 213.5 | 217.6 | 219.6 | 219.8 | 219.8 | 217.6 / 219.8 / 213.5 | 2.54-3.30 | 0 |
| native/alpine:3.20/nomap | first_start | 3 | 107.6 | 23.2 | 92.7 | 95.8 | 130.4 | 133.5 | 134.3 | 92.7 / 95.8 / 134.3 | 2.54-3.30 | 0 |
| native/alpine:3.20/nomap | first_create_plus_start | 3 | 324.5 | 20.3 | 310.3 | 315.6 | 344.5 | 347.1 | 347.7 | 310.3 / 315.6 / 347.7 | 2.54-3.30 | 0 |
| native/alpine:3.20/nomap | subsequent_create | 60 | 56.2 | 2.1 | 53.2 | 55.7 | 58.9 | 63.5 | 66.0 | 56.2 / 56.1 / 55.4 | 2.49-3.36 | 0 |
| native/alpine:3.20/nomap | subsequent_start | 60 | 98.2 | 12.1 | 85.9 | 94.4 | 126.8 | 136.2 | 138.0 | 93.2 / 94.9 / 94.4 | 2.49-3.36 | 0 |
| native/alpine:3.20/nomap | subsequent_create_plus_start | 60 | 154.4 | 12.5 | 141.3 | 151.1 | 182.9 | 195.7 | 196.7 | 150.6 / 152.1 / 150.0 | 2.49-3.36 | 0 |
| native/python:3.12-alpine/auto | first_create | 3 | 2200.6 | 13.3 | 2190.5 | 2195.7 | 2213.6 | 2215.2 | 2215.6 | 2190.5 / 2215.6 / 2195.7 | 2.07-2.92 | 0 |
| native/python:3.12-alpine/auto | first_start | 3 | 93.8 | 1.7 | 92.8 | 92.8 | 95.5 | 95.7 | 95.8 | 92.8 / 95.8 / 92.8 | 2.07-2.92 | 0 |
| native/python:3.12-alpine/auto | first_create_plus_start | 3 | 2294.4 | 14.9 | 2283.3 | 2288.5 | 2309.1 | 2310.9 | 2311.4 | 2283.3 / 2311.4 / 2288.5 | 2.07-2.92 | 0 |
| native/python:3.12-alpine/auto | subsequent_create | 60 | 79.3 | 2.1 | 75.7 | 79.2 | 81.7 | 86.6 | 89.0 | 79.3 / 78.7 / 79.2 | 2.07-2.92 | 0 |
| native/python:3.12-alpine/auto | subsequent_start | 60 | 98.9 | 13.5 | 83.8 | 95.5 | 126.7 | 153.2 | 157.5 | 95.0 / 98.4 / 95.6 | 2.07-2.92 | 0 |
| native/python:3.12-alpine/auto | subsequent_create_plus_start | 60 | 178.2 | 13.8 | 163.8 | 175.0 | 208.2 | 234.3 | 237.9 | 174.1 / 176.9 / 175.2 | 2.07-2.92 | 0 |
| native/python:3.12-alpine/auto | auto_while_others_alive_create | 18 | 1762.7 | 796.2 | 77.4 | 2009.1 | 2516.7 | 2648.1 | 2680.9 | 2008.1 / 2015.2 / 2031.5 | 2.07-3.44 | 0 |
| native/python:3.12-alpine/host(default) | first_create | 3 | 58.9 | 0.6 | 58.3 | 59.0 | 59.4 | 59.5 | 59.5 | 59.5 / 58.3 / 59.0 | 1.47-2.65 | 0 |
| native/python:3.12-alpine/host(default) | first_start | 3 | 93.7 | 4.4 | 88.6 | 96.0 | 96.3 | 96.3 | 96.3 | 96.0 / 88.6 / 96.3 | 1.47-2.65 | 0 |
| native/python:3.12-alpine/host(default) | first_create_plus_start | 3 | 152.6 | 4.9 | 147.0 | 155.3 | 155.5 | 155.5 | 155.5 | 155.5 / 147.0 / 155.3 | 1.47-2.65 | 0 |
| native/python:3.12-alpine/host(default) | subsequent_create | 60 | 57.6 | 1.3 | 54.6 | 57.5 | 59.4 | 60.7 | 60.7 | 57.8 / 58.1 / 56.8 | 1.47-2.59 | 0 |
| native/python:3.12-alpine/host(default) | subsequent_start | 60 | 94.9 | 9.0 | 84.0 | 94.1 | 101.5 | 134.5 | 144.1 | 93.1 / 93.2 / 94.4 | 1.47-2.59 | 0 |
| native/python:3.12-alpine/host(default) | subsequent_create_plus_start | 60 | 152.6 | 9.2 | 143.3 | 151.3 | 160.1 | 192.1 | 200.8 | 151.3 / 151.1 / 151.5 | 1.47-2.59 | 0 |
| native/python:3.12-alpine/keep-id | first_create | 3 | 2217.1 | 21.2 | 2200.4 | 2209.9 | 2237.8 | 2240.2 | 2240.9 | 2240.9 / 2200.4 / 2209.9 | 2.11-2.55 | 0 |
| native/python:3.12-alpine/keep-id | first_start | 3 | 106.6 | 1.6 | 105.4 | 106.1 | 108.2 | 108.3 | 108.4 | 106.1 / 108.4 / 105.4 | 2.11-2.55 | 0 |
| native/python:3.12-alpine/keep-id | first_create_plus_start | 3 | 2323.7 | 20.4 | 2308.8 | 2315.3 | 2343.8 | 2346.3 | 2347.0 | 2347.0 / 2308.8 / 2315.3 | 2.11-2.55 | 0 |
| native/python:3.12-alpine/keep-id | subsequent_create | 60 | 58.7 | 2.3 | 56.0 | 58.5 | 62.3 | 66.3 | 71.5 | 59.0 / 57.9 / 58.8 | 2.10-2.90 | 0 |
| native/python:3.12-alpine/keep-id | subsequent_start | 60 | 104.9 | 13.7 | 94.5 | 102.3 | 115.7 | 166.0 | 172.9 | 100.0 / 99.2 / 106.4 | 2.10-2.90 | 0 |
| native/python:3.12-alpine/keep-id | subsequent_create_plus_start | 60 | 163.6 | 14.1 | 151.1 | 160.5 | 186.5 | 223.7 | 230.9 | 159.0 / 158.4 / 163.4 | 2.10-2.90 | 0 |
| native/python:3.12-alpine/keep-id:uid=1000,gid=1000 | first_create | 3 | 2298.6 | 203.4 | 2158.6 | 2205.3 | 2499.3 | 2525.5 | 2532.0 | 2158.6 / 2205.3 / 2532.0 | 2.09-2.75 | 0 |
| native/python:3.12-alpine/keep-id:uid=1000,gid=1000 | first_start | 3 | 101.1 | 4.5 | 97.9 | 99.2 | 105.5 | 106.1 | 106.2 | 97.9 / 99.2 / 106.2 | 2.09-2.75 | 0 |
| native/python:3.12-alpine/keep-id:uid=1000,gid=1000 | first_create_plus_start | 3 | 2399.7 | 207.9 | 2256.5 | 2304.5 | 2604.9 | 2631.6 | 2638.2 | 2256.5 / 2304.5 / 2638.2 | 2.09-2.75 | 0 |
| native/python:3.12-alpine/keep-id:uid=1000,gid=1000 | subsequent_create | 60 | 58.1 | 1.6 | 55.6 | 58.0 | 60.2 | 62.8 | 66.6 | 58.2 / 58.3 / 57.5 | 2.09-2.75 | 0 |
| native/python:3.12-alpine/keep-id:uid=1000,gid=1000 | subsequent_start | 60 | 103.1 | 11.5 | 91.4 | 100.5 | 109.4 | 158.9 | 159.3 | 102.2 / 100.6 / 100.0 | 2.09-2.75 | 0 |
| native/python:3.12-alpine/keep-id:uid=1000,gid=1000 | subsequent_create_plus_start | 60 | 161.2 | 11.5 | 150.0 | 159.0 | 173.5 | 216.1 | 217.5 | 159.9 / 159.2 / 158.0 | 2.09-2.75 | 0 |
| native/python:3.12-alpine/nomap | first_create | 3 | 2194.3 | 17.9 | 2178.9 | 2190.1 | 2211.6 | 2213.5 | 2214.0 | 2190.1 / 2178.8 / 2214.0 | 2.09-2.55 | 0 |
| native/python:3.12-alpine/nomap | first_start | 3 | 92.2 | 2.3 | 90.5 | 91.3 | 94.5 | 94.7 | 94.8 | 94.8 / 90.5 / 91.3 | 2.09-2.55 | 0 |
| native/python:3.12-alpine/nomap | first_create_plus_start | 3 | 2286.5 | 18.0 | 2269.3 | 2284.9 | 2303.2 | 2304.9 | 2305.3 | 2284.9 / 2269.3 / 2305.3 | 2.09-2.55 | 0 |
| native/python:3.12-alpine/nomap | subsequent_create | 60 | 58.0 | 1.2 | 55.4 | 58.0 | 60.1 | 60.7 | 61.0 | 57.7 / 58.4 / 57.9 | 2.08-2.55 | 0 |
| native/python:3.12-alpine/nomap | subsequent_start | 60 | 100.2 | 20.1 | 87.3 | 95.6 | 138.1 | 181.2 | 220.7 | 98.2 / 95.1 / 93.9 | 2.08-2.55 | 0 |
| native/python:3.12-alpine/nomap | subsequent_create_plus_start | 60 | 158.1 | 20.3 | 145.2 | 153.5 | 195.5 | 239.6 | 278.3 | 155.6 / 153.7 / 151.6 | 2.08-2.55 | 0 |

`--userns=auto` exhaustion: `{"store": "iso65-fuse", "created_before_error": 64, "first_error": "Error: creating container storage: not enough unused IDs in user namespace", "ceiling_tried": 80, "subid": ["/etc/subuid:user:100000:65536", "/etc/subgid:user:100000:65536"]}`

Store size (MB, overlay dir) after each cell: `{"native/python:3.12-alpine/host(default)#rep0": "60", "native/python:3.12-alpine/keep-id#rep0": "113", "native/python:3.12-alpine/keep-id:uid=1000,gid=1000#rep0": "113", "native/python:3.12-alpine/nomap#rep0": "113", "native/python:3.12-alpine/auto#rep0": "379", "native/alpine:3.20/host(default)#rep0": "9", "native/alpine:3.20/keep-id#rep0": "17", "native/alpine:3.20/keep-id:uid=1000,gid=1000#rep0": "17", "native/alpine:3.20/nomap#rep0": "17", "native/alpine:3.20/auto#rep0": "57", "fuse/python:3.12-alpine/host(default)#rep0": "60", "fuse/python:3.12-alpine/keep-id#rep0": "60", "fuse/python:3.12-alpine/keep-id:uid=1000,gid=1000#rep0": "60", "fuse/python:3.12-alpine/nomap#rep0": "60", "fuse/python:3.12-alpine/auto#rep0": "60", "fuse/alpine:3.20/host(default)#rep0": "9", "fuse/alpine:3.20/keep-id#rep0": "9", "fuse/alpine:3.20/keep-id:uid=1000,gid=1000#rep0": "9", "fuse/alpine:3.20/nomap#rep0": "9", "fuse/alpine:3.20/auto#rep0": "9", "native/python:3.12-alpine/host(default)#rep1": "60", "native/python:3.12-alpine/keep-id#rep1": "113", "native/python:3.12-alpine/keep-id:uid=1000,gid=1000#rep1": "113", "native/python:3.12-alpine/nomap#rep1": "113", "native/python:3.12-alpine/auto#rep1": "379", "native/alpine:3.20/host(default)#rep1": "9", "native/alpine:3.20/keep-id#rep1": "17", "native/alpine:3.20/keep-id:uid=1000,gid=1000#rep1": "17", "native/alpine:3.20/nomap#rep1": "17", "native/alpine:3.20/auto#rep1": "57", "fuse/python:3.12-alpine/host(default)#rep1": "60", "fuse/python:3.12-alpine/keep-id#rep1": "60", "fuse/python:3.12-alpine/keep-id:uid=1000,gid=1000#rep1": "60", "fuse/python:3.12-alpine/nomap#rep1": "60", "fuse/python:3.12-alpine/auto#rep1": "60", "fuse/alpine:3.20/host(default)#rep1": "9", "fuse/alpine:3.20/keep-id#rep1": "9", "fuse/alpine:3.20/keep-id:uid=1000,gid=1000#rep1": "9", "fuse/alpine:3.20/nomap#rep1": "9", "fuse/alpine:3.20/auto#rep1": "9", "native/python:3.12-alpine/host(default)#rep2": "60", "native/python:3.12-alpine/keep-id#rep2": "113", "native/python:3.12-alpine/keep-id:uid=1000,gid=1000#rep2": "113", "native/python:3.12-alpine/nomap#rep2": "113", "native/python:3.12-alpine/auto#rep2": "379", "native/alpine:3.20/host(default)#rep2": "9", "native/alpine:3.20/keep-id#rep2": "17", "native/alpine:3.20/keep-id:uid=1000,gid=1000#rep2": "17", "native/alpine:3.20/nomap#rep2": "17", "native/alpine:3.20/auto#rep2": "57", "fuse/python:3.12-alpine/host(default)#rep2": "60", "fuse/python:3.12-alpine/keep-id#rep2": "60", "fuse/python:3.12-alpine/keep-id:uid=1000,gid=1000#rep2": "60", "fuse/python:3.12-alpine/nomap#rep2": "60", "fuse/python:3.12-alpine/auto#rep2": "60", "fuse/alpine:3.20/host(default)#rep2": "9", "fuse/alpine:3.20/keep-id#rep2": "9", "fuse/alpine:3.20/keep-id:uid=1000,gid=1000#rep2": "9", "fuse/alpine:3.20/nomap#rep2": "9", "fuse/alpine:3.20/auto#rep2": "9"}`

## 3. Warm state: pause/unpause versus exec into an idle running sandbox

| cell | op | n | mean | sd | min | p50 | p95 | p99 | max | per-round p50 | load1 | errors |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---:|
| crun/none/hardened | agent_exec_true_idle_running | 100 | 1.3 | 0.1 | 1.2 | 1.3 | 1.5 | 1.9 | 2.2 | 1.3 / 1.2 | 1.93-2.34 | 0 |
| crun/none/hardened | api_exec_true_idle_running | 100 | 83.5 | 5.9 | 71.8 | 83.6 | 94.6 | 95.4 | 98.4 | 78.2 / 86.6 | 1.93-2.34 | 0 |
| crun/none/hardened | api_pause | 100 | 10.9 | 0.9 | 9.0 | 10.7 | 12.3 | 12.9 | 13.8 | 10.3 / 11.5 | 1.93-2.34 | 0 |
| crun/none/hardened | api_unpause | 100 | 10.6 | 1.1 | 9.2 | 10.4 | 11.7 | 12.9 | 19.7 | 10.2 / 10.8 | 1.93-2.34 | 0 |
| crun/none/hardened | api_unpause_then_agent_exec_true | 100 | 11.8 | 0.6 | 10.7 | 11.9 | 12.7 | 13.1 | 14.2 | 11.5 / 12.2 | 1.93-2.34 | 0 |
| crun/none/hardened | cgroup_freeze_write_until_frozen | 100 | 0.5 | 0.7 | 0.1 | 0.2 | 2.1 | 2.6 | 2.9 | 0.2 / 0.2 | 1.93-2.34 | 0 |
| crun/none/hardened | cgroup_thaw_write_until_thawed | 100 | 0.1 | 0.0 | 0.1 | 0.1 | 0.1 | 0.1 | 0.2 | 0.1 / 0.1 | 1.93-2.34 | 0 |
| crun/none/hardened | cli_exec_true_idle_running | 100 | 165.0 | 12.8 | 143.7 | 165.7 | 183.8 | 189.2 | 190.6 | 160.6 / 175.9 | 1.93-2.34 | 0 |
| crun/none/hardened | cli_pause | 100 | 41.5 | 1.2 | 39.2 | 41.3 | 43.4 | 44.1 | 45.7 | 40.7 / 42.4 | 1.93-2.34 | 0 |
| crun/none/hardened | cli_unpause | 100 | 41.6 | 1.1 | 38.7 | 41.7 | 43.5 | 43.9 | 44.6 | 41.0 / 42.2 | 1.93-2.34 | 0 |

- pause_exec_into_paused: `{"rc": 255, "stderr": "Error: can only create exec sessions on running containers: container state improper", "ms": 33.8}`
- pause_state_while_paused: `"paused"`
- pause_memory_current_bytes: `{"running": 7356416, "paused": 7249920}`

## 4. Density, round 1

Idle process: sbx-agent serve (real idle sandbox); image docker.io/library/alpine:3.20; hardened spec via REST API. Peak: **176** sandboxes. Stop reason: **safety stop 20 keys short of the per-user session-keyring quota (kernel.keys.maxkeys=200, 180 in use; each running container holds one key)**.

| n | MemAvailable MB | host delta/sbx kB | cgroup scope mem kB | conmon scope mem kB | conmon PSS kB | conmon RSS kB | PID1 PSS kB | pids/sbx | procs/sbx | threads/sbx | fds/sbx | inotify | keys | create p50 | start p50 | create-to-ready p50 | p95 | max | load1 |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 0 | 9157 | - | - | - | - | - | - | - | - | - | - | 42 | 4/200 | - | - | - | - | - | 1.77 |
| 20 | 9106 | 2585.8 | 1605.0 | 420.6 | 338.2 | 2116.4 | 1436.6 | 7.0 | 2.00 | 8.05 | 23.05 | 62 | 24/200 | 23.8 | 62.4 | 87.4 | 93.6 | 99.2 | 1.71 |
| 40 | 9045 | 2856.7 | 1577.7 | 414.1 | 337.1 | 2106.8 | 1267.8 | 7.0 | 2.00 | 8.00 | 23.02 | 82 | 44/200 | 24.6 | 59.6 | 85.1 | 95.7 | 98.8 | 1.65 |
| 60 | 8989 | 2868.1 | 1567.7 | 410.7 | 336.9 | 2104.0 | 1217.3 | 7.0 | 2.00 | 8.05 | 23.03 | 102 | 64/200 | 25.6 | 63.0 | 88.9 | 184.2 | 220.0 | 1.47 |
| 80 | 8927 | 2936.8 | 1558.0 | 408.8 | 336.2 | 2100.2 | 1186.0 | 7.0 | 2.00 | 8.03 | 23.01 | 122 | 84/200 | 26.9 | 66.1 | 94.5 | 119.1 | 123.9 | 1.35 |
| 100 | 8855 | 3089.9 | 1560.3 | 408.4 | 335.9 | 2103.3 | 1175.1 | 7.0 | 2.00 | 7.98 | 23.01 | 142 | 104/200 | 28.8 | 67.0 | 97.9 | 106.1 | 123.3 | 1.24 |
| 120 | 8784 | 3182.3 | 1559.0 | 407.6 | 335.8 | 2101.4 | 1164.7 | 7.0 | 2.00 | 7.99 | 23.02 | 162 | 124/200 | 28.9 | 65.1 | 94.7 | 114.2 | 135.0 | 1.22 |
| 140 | 8732 | 3103.9 | 1565.0 | 411.1 | 335.4 | 2102.5 | 1158.1 | 7.0 | 2.00 | 7.99 | 23.01 | 182 | 144/200 | 29.7 | 66.0 | 97.8 | 105.7 | 142.5 | 1.29 |
| 160 | 8729 | 2736.2 | 1558.0 | 406.6 | 334.9 | 2100.8 | 1149.5 | 7.0 | 2.00 | 8.00 | 23.01 | 202 | 164/200 | 30.6 | 69.0 | 100.8 | 130.7 | 132.6 | 1.16 |
| 176 | 8713 | 2581.9 | 1558.8 | 408.5 | 334.9 | 2100.3 | 1143.9 | 7.0 | 2.00 | 8.01 | 23.01 | 218 | 180/200 | 31.2 | 69.8 | 102.3 | 130.8 | 132.4 | 1.07 |

- host meminfo growth per sandbox at peak (kB): `{"MemFree": -2853.9, "Cached": 59.8, "AnonPages": 1472.3, "Shmem": 40.2, "Slab": 994.5, "SUnreclaim": 773.3, "SReclaimable": 221.2, "KernelStack": 127.8, "PageTables": 179.9, "Percpu": 41.5}`
- podman service RSS kB: baseline 55964, at peak 60084; free Podman locks at peak 1872
- create/start errors during the ramp: `[]` (total 0)
- teardown at peak: `{"n": 176, "concurrency": 4, "seconds": 13.305, "removals_per_second": 13.23, "errors": []}`
- after teardown: `{"mem_available_kb": 9321548, "keys_used": 4, "user_inotify_instances": 42}`
- limits on this host: `{"inotify_max_user_instances": 512, "keys_maxkeys": 200, "pid_max": 4194304, "user_slice_pids_max": "26829", "podman_num_locks_default": 2048}`
- limit confirmation (--push-to-limit): `null`

## 4. Density, round 2

Idle process: sbx-agent serve (real idle sandbox); image docker.io/library/alpine:3.20; hardened spec via REST API. Peak: **176** sandboxes. Stop reason: **safety stop 20 keys short of the per-user session-keyring quota (kernel.keys.maxkeys=200, 180 in use; each running container holds one key)**.

| n | MemAvailable MB | host delta/sbx kB | cgroup scope mem kB | conmon scope mem kB | conmon PSS kB | conmon RSS kB | PID1 PSS kB | pids/sbx | procs/sbx | threads/sbx | fds/sbx | inotify | keys | create p50 | start p50 | create-to-ready p50 | p95 | max | load1 |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 0 | 9118 | - | - | - | - | - | - | - | - | - | - | 42 | 4/200 | - | - | - | - | - | 1.98 |
| 20 | 9059 | 3027.6 | 1578.6 | 414.6 | 336.6 | 2108.6 | 1404.4 | 7.0 | 2.00 | 7.90 | 23.10 | 62 | 24/200 | 23.7 | 61.0 | 85.9 | 93.9 | 94.4 | 1.9 |
| 40 | 9011 | 2739.5 | 1566.4 | 405.9 | 336.0 | 2103.9 | 1258.0 | 7.0 | 2.00 | 7.92 | 23.05 | 82 | 44/200 | 24.3 | 62.9 | 88.4 | 100.2 | 137.1 | 1.83 |
| 60 | 8970 | 2525.3 | 1548.3 | 407.1 | 335.5 | 2103.5 | 1203.2 | 7.0 | 2.00 | 7.88 | 23.02 | 102 | 64/200 | 25.3 | 65.3 | 91.8 | 103.2 | 132.9 | 1.76 |
| 80 | 8916 | 2585.9 | 1559.2 | 411.1 | 334.9 | 2102.3 | 1177.0 | 7.0 | 2.00 | 7.92 | 23.01 | 122 | 84/200 | 25.8 | 62.8 | 90.5 | 97.9 | 128.5 | 1.56 |
| 100 | 8849 | 2761.6 | 1552.8 | 409.5 | 334.5 | 2102.7 | 1163.3 | 7.0 | 2.00 | 7.95 | 23.02 | 142 | 104/200 | 27.0 | 65.2 | 93.5 | 97.1 | 99.1 | 1.44 |
| 120 | 8793 | 2775.8 | 1552.5 | 407.1 | 334.6 | 2102.1 | 1154.9 | 7.0 | 2.00 | 7.95 | 23.01 | 162 | 124/200 | 27.8 | 65.5 | 94.6 | 133.4 | 146.6 | 1.32 |
| 140 | 8790 | 2404.1 | 1549.6 | 407.6 | 334.7 | 2103.7 | 1147.2 | 7.0 | 2.00 | 7.99 | 23.01 | 182 | 144/200 | 28.0 | 65.6 | 94.9 | 102.9 | 127.1 | 1.3 |
| 160 | 8767 | 2246.2 | 1549.5 | 406.4 | 334.7 | 2104.2 | 1142.3 | 7.0 | 2.00 | 7.96 | 23.01 | 202 | 164/200 | 29.0 | 65.5 | 95.6 | 105.7 | 141.8 | 1.35 |
| 176 | 8684 | 2528.2 | 1550.5 | 406.1 | 334.0 | 2103.2 | 1139.4 | 7.0 | 2.00 | 7.96 | 23.01 | 218 | 180/200 | 30.1 | 67.2 | 98.7 | 110.2 | 130.4 | 1.3 |

- host meminfo growth per sandbox at peak (kB): `{"MemFree": -2758.8, "Cached": 75.8, "AnonPages": 1420.7, "Shmem": 40.2, "Slab": 745.3, "SUnreclaim": 564.8, "SReclaimable": 180.5, "KernelStack": 127.3, "PageTables": 179.2, "Percpu": 34.8}`
- podman service RSS kB: baseline 59676, at peak 61964; free Podman locks at peak 1872
- create/start errors during the ramp: `[]` (total 0)
- teardown at peak: `{"n": 176, "concurrency": 4, "seconds": 12.945, "removals_per_second": 13.6, "errors": []}`
- after teardown: `{"mem_available_kb": 9246028, "keys_used": 4, "user_inotify_instances": 42}`
- limits on this host: `{"inotify_max_user_instances": 512, "keys_maxkeys": 200, "pid_max": 4194304, "user_slice_pids_max": "26829", "podman_num_locks_default": 2048}`
- limit confirmation (--push-to-limit): `{"extra_containers_started": 20, "keys_before": [180, 200], "keys_at_failure": [200, 200], "first_error": "Error: OCI runtime error: crun: create keyring `6146fe21e3774f2c864e1d25600d447f0dc6456bfbfc153d0c7069359b7205e5`: Disk quota exceeded"}`

Request-path latency with the host at peak density:

| cell | op | n | mean | sd | min | p50 | p95 | p99 | max | per-round p50 | load1 | errors |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---:|
| n=176 | agent_exec_true | 100 | 1.3 | 0.1 | 1.1 | 1.3 | 1.4 | 1.8 | 1.8 | 1.3 / 1.3 | 1.07-1.36 | 0 |
| n=176 | api_list_containers | 100 | 30.9 | 1.9 | 27.2 | 30.9 | 34.6 | 35.3 | 35.6 | 31.0 / 30.6 | 1.07-1.36 | 0 |
| n=176 | api_podman_exec_true | 100 | 103.8 | 8.6 | 74.5 | 103.3 | 113.9 | 121.4 | 150.6 | 100.8 / 106.2 | 1.07-1.36 | 0 |
| n=176 | cli_podman_exec_true | 100 | 179.4 | 11.7 | 131.6 | 180.8 | 194.6 | 198.6 | 214.1 | 177.7 / 184.0 | 1.07-1.36 | 0 |
| n=176 | cli_podman_ps | 100 | 75.3 | 1.6 | 72.5 | 75.0 | 77.8 | 78.9 | 83.6 | 75.3 / 74.9 | 1.07-1.36 | 0 |

Create latency drift during the ramp (all rounds pooled; cell = sandboxes alive at the end of the step):

| cell | op | n | mean | sd | min | p50 | p95 | p99 | max | per-round p50 | load1 | errors |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---:|
| n<=020 | create | 40 | 24.1 | 1.6 | 21.9 | 23.8 | 25.9 | 29.5 | 31.7 | 23.8 / 23.7 | 1.71-1.90 | 0 |
| n<=020 | start | 40 | 62.4 | 4.1 | 54.9 | 62.2 | 68.9 | 69.3 | 69.4 | 62.4 / 61.0 | 1.71-1.90 | 0 |
| n<=020 | ready | 40 | 0.6 | 0.0 | 0.6 | 0.6 | 0.7 | 0.7 | 0.7 | 0.6 / 0.6 | 1.71-1.90 | 0 |
| n<=020 | create_to_ready | 40 | 87.2 | 4.6 | 79.3 | 86.3 | 93.9 | 97.4 | 99.2 | 87.4 / 85.9 | 1.71-1.90 | 0 |
| n<=040 | create | 40 | 24.5 | 1.0 | 22.7 | 24.3 | 26.0 | 26.7 | 26.7 | 24.6 / 24.3 | 1.60-1.83 | 0 |
| n<=040 | start | 40 | 64.2 | 9.2 | 55.8 | 61.8 | 72.4 | 97.3 | 112.5 | 59.6 / 62.9 | 1.60-1.83 | 0 |
| n<=040 | ready | 40 | 0.6 | 0.1 | 0.5 | 0.6 | 0.7 | 0.8 | 0.8 | 0.6 / 0.6 | 1.60-1.83 | 0 |
| n<=040 | create_to_ready | 40 | 89.3 | 9.2 | 79.1 | 86.7 | 98.3 | 122.2 | 137.1 | 85.1 / 88.4 | 1.60-1.83 | 0 |
| n<=060 | create | 40 | 31.2 | 25.5 | 23.2 | 25.4 | 34.0 | 143.4 | 156.3 | 25.6 / 25.3 | 1.47-1.70 | 0 |
| n<=060 | start | 40 | 65.3 | 8.4 | 55.5 | 63.7 | 72.9 | 94.6 | 107.9 | 63.0 / 65.3 | 1.47-1.70 | 0 |
| n<=060 | ready | 40 | 0.6 | 0.1 | 0.5 | 0.6 | 0.7 | 0.9 | 1.0 | 0.6 / 0.6 | 1.47-1.70 | 0 |
| n<=060 | create_to_ready | 40 | 97.2 | 25.9 | 81.5 | 90.3 | 135.4 | 205.3 | 220.0 | 88.9 / 91.8 | 1.47-1.70 | 0 |
| n<=080 | create | 40 | 27.5 | 4.9 | 24.5 | 26.6 | 31.1 | 46.6 | 55.6 | 26.9 / 25.8 | 1.35-1.56 | 0 |
| n<=080 | start | 40 | 66.0 | 8.5 | 54.9 | 63.9 | 72.6 | 100.1 | 102.0 | 66.1 / 62.8 | 1.35-1.56 | 0 |
| n<=080 | ready | 40 | 0.6 | 0.1 | 0.5 | 0.6 | 0.7 | 0.8 | 0.9 | 0.6 / 0.6 | 1.35-1.56 | 0 |
| n<=080 | create_to_ready | 40 | 94.2 | 9.4 | 81.5 | 92.2 | 119.1 | 126.7 | 128.5 | 94.5 / 90.5 | 1.35-1.56 | 0 |
| n<=100 | create | 40 | 28.9 | 5.7 | 25.4 | 28.0 | 31.0 | 50.7 | 62.3 | 28.8 / 27.0 | 1.24-1.44 | 0 |
| n<=100 | start | 40 | 65.9 | 4.4 | 58.4 | 66.9 | 72.7 | 73.8 | 74.4 | 67.0 / 65.2 | 1.24-1.44 | 0 |
| n<=100 | ready | 40 | 0.6 | 0.1 | 0.5 | 0.6 | 0.7 | 0.9 | 0.9 | 0.6 / 0.6 | 1.24-1.44 | 0 |
| n<=100 | create_to_ready | 40 | 95.5 | 6.8 | 85.8 | 95.3 | 103.4 | 116.2 | 123.3 | 97.9 / 93.5 | 1.24-1.44 | 0 |
| n<=120 | create | 40 | 29.5 | 4.0 | 26.2 | 28.4 | 34.3 | 45.5 | 47.1 | 28.9 / 27.8 | 1.22-1.32 | 0 |
| n<=120 | start | 40 | 68.6 | 12.0 | 59.8 | 65.3 | 94.0 | 112.9 | 119.8 | 65.1 / 65.5 | 1.22-1.32 | 0 |
| n<=120 | ready | 40 | 0.6 | 0.0 | 0.5 | 0.6 | 0.7 | 0.7 | 0.7 | 0.6 / 0.6 | 1.22-1.32 | 0 |
| n<=120 | create_to_ready | 40 | 98.8 | 12.9 | 87.4 | 94.6 | 132.8 | 142.1 | 146.6 | 94.7 / 94.6 | 1.22-1.32 | 0 |
| n<=140 | create | 40 | 29.3 | 2.0 | 26.6 | 28.9 | 32.1 | 35.8 | 36.7 | 29.7 / 28.0 | 1.26-1.30 | 0 |
| n<=140 | start | 40 | 67.8 | 9.2 | 59.8 | 65.7 | 74.3 | 105.8 | 110.0 | 66.0 / 65.5 | 1.26-1.30 | 0 |
| n<=140 | ready | 40 | 0.6 | 0.0 | 0.5 | 0.6 | 0.7 | 0.7 | 0.7 | 0.6 / 0.6 | 1.26-1.30 | 0 |
| n<=140 | create_to_ready | 40 | 97.7 | 9.5 | 89.5 | 96.7 | 104.9 | 136.5 | 142.5 | 97.8 / 94.9 | 1.26-1.30 | 0 |
| n<=160 | create | 40 | 29.9 | 1.7 | 26.9 | 29.8 | 32.2 | 34.4 | 35.0 | 30.6 / 29.0 | 1.16-1.33 | 0 |
| n<=160 | start | 40 | 69.7 | 11.1 | 58.9 | 66.8 | 101.0 | 108.8 | 112.9 | 69.0 / 65.5 | 1.16-1.33 | 0 |
| n<=160 | ready | 40 | 0.6 | 0.0 | 0.5 | 0.6 | 0.7 | 0.7 | 0.7 | 0.6 / 0.6 | 1.16-1.33 | 0 |
| n<=160 | create_to_ready | 40 | 100.2 | 11.0 | 87.7 | 97.5 | 130.7 | 138.2 | 141.8 | 100.8 / 95.5 | 1.16-1.33 | 0 |
| n<=176 | create | 32 | 31.9 | 5.6 | 28.5 | 30.8 | 34.6 | 53.5 | 61.6 | 31.2 / 30.1 | 1.07-1.30 | 0 |
| n<=176 | start | 32 | 70.5 | 10.1 | 61.4 | 68.1 | 99.1 | 100.9 | 101.3 | 69.8 / 67.2 | 1.07-1.30 | 0 |
| n<=176 | ready | 32 | 0.6 | 0.0 | 0.5 | 0.6 | 0.7 | 0.7 | 0.7 | 0.6 / 0.6 | 1.07-1.30 | 0 |
| n<=176 | create_to_ready | 32 | 103.0 | 11.2 | 92.5 | 100.2 | 130.3 | 131.8 | 132.4 | 102.3 / 98.7 | 1.07-1.30 | 0 |

## 4a. Parallel create throughput and teardown rate

One create = create + start + agent ready, hardened spec. n per run = 32.

| transport | concurrency | round | ok | creates/s | per-create p50 ms | p95 ms | max ms | removals/s | create errors | load1 start-end |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| cli | 1 | 1 | 32/32 | 7.83 | 124.7 | 158.4 | 186.9 | 8.4 | 0 | 1.35-1.3 |
| cli | 1 | 2 | 32/32 | 7.7 | 125.1 | 159.3 | 161.8 | 8.75 | 0 | 1.75-1.96 |
| cli | 2 | 1 | 32/32 | 13.52 | 146.8 | 158.2 | 167.8 | 10.65 | 0 | 1.3-1.43 |
| cli | 2 | 2 | 32/32 | 13.2 | 149.7 | 162.2 | 163.4 | 10.85 | 0 | 1.96-2.04 |
| cli | 4 | 1 | 32/32 | 16.38 | 231.3 | 296.2 | 315.0 | 11.14 | 0 | 1.43-1.56 |
| cli | 4 | 2 | 32/32 | 16.08 | 229.4 | 305.9 | 526.3 | 11.08 | 0 | 2.04-2.28 |
| cli | 8 | 1 | 32/32 | 16.47 | 384.3 | 758.1 | 975.1 | 8.99 | 0 | 1.56-1.75 |
| cli | 8 | 2 | 32/32 | 17.25 | 369.3 | 859.6 | 1124.6 | 9.78 | 0 | 2.28-2.74 |
| rest-api | 1 | 1 | 32/32 | 11.55 | 84.7 | 95.0 | 95.3 | 11.8 | 0 | 1.51-1.47 |
| rest-api | 1 | 2 | 0/32 | 0.0 | - | - | - | 0.0 | 32 | 1.75-1.75 |
| rest-api | 2 | 1 | 32/32 | 17.6 | 109.3 | 131.8 | 133.6 | 13.83 | 0 | 1.47-1.35 |
| rest-api | 2 | 2 | 0/32 | 0.0 | - | - | - | 0.0 | 32 | 1.75-1.75 |
| rest-api | 4 | 1 | 0/32 | 0.0 | - | - | - | 0.0 | 32 | 1.35-1.35 |
| rest-api | 4 | 2 | 0/32 | 0.0 | - | - | - | 0.0 | 32 | 1.75-1.75 |
| rest-api | 8 | 1 | 0/32 | 0.0 | - | - | - | 0.0 | 32 | 1.35-1.35 |
| rest-api | 8 | 2 | 0/32 | 0.0 | - | - | - | 0.0 | 32 | 1.75-1.75 |

Create errors in total: 192 of 512 attempts. Distinct messages: `["create 599: [Errno 111] Connection refused"]`

## 5. Workload overhead (timed inside the sandbox)

| cell | op | n | mean | sd | min | p50 | p95 | p99 | max | per-round p50 | load1 | errors |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---:|
| crun/hardened, Podman default seccomp | cpu_loop_2M_iterations | 15 | 256.4 | 6.1 | 251.0 | 253.8 | 267.6 | 268.0 | 268.2 | 252.4 / 265.9 / 251.9 | 1.12-2.19 | 0 |
| crun/hardened, Podman default seccomp | fork_shell_300x_bin_true | 15 | 172.4 | 1.4 | 170.1 | 172.6 | 174.6 | 175.4 | 175.6 | 172.6 / 173.2 / 171.4 | 1.12-2.19 | 0 |
| crun/hardened, Podman default seccomp | python_import_30_stdlib_modules | 45 | 1165.7 | 14.2 | 1143.3 | 1169.5 | 1186.2 | 1187.7 | 1188.2 | 1173.4 / 1170.3 / 1151.0 | 1.12-2.19 | 0 |
| crun/hardened, Podman default seccomp | python_start_pass | 90 | 25.1 | 1.5 | 24.5 | 24.7 | 27.3 | 32.3 | 33.6 | 24.8 / 24.7 / 24.7 | 1.12-2.19 | 0 |
| crun/hardened, Podman default seccomp | python_subprocess_100x_true | 15 | 56.3 | 3.7 | 50.4 | 57.0 | 61.1 | 63.0 | 63.4 | 56.5 / 57.5 / 52.2 | 1.12-2.19 | 0 |
| crun/hardened, Podman default seccomp | small_files_2000x1KiB_write_read_unlink | 15 | 112.8 | 8.7 | 109.2 | 110.2 | 125.1 | 139.6 | 143.2 | 109.7 / 110.7 / 110.2 | 1.12-2.19 | 0 |
| crun/hardened, product seccomp + PYTHONPYCACHEPREFIX=/tmp/pycache | cpu_loop_2M_iterations | 15 | 259.2 | 8.7 | 251.5 | 256.1 | 274.4 | 274.7 | 274.8 | 251.5 / 256.1 / 272.1 | 1.07-1.72 | 0 |
| crun/hardened, product seccomp + PYTHONPYCACHEPREFIX=/tmp/pycache | fork_shell_300x_bin_true | 15 | 171.7 | 1.7 | 168.8 | 172.0 | 174.1 | 174.5 | 174.7 | 173.1 / 171.4 / 170.3 | 1.07-1.72 | 0 |
| crun/hardened, product seccomp + PYTHONPYCACHEPREFIX=/tmp/pycache | python_import_30_stdlib_modules | 45 | 175.1 | 3.3 | 171.7 | 173.8 | 182.2 | 183.7 | 184.0 | 173.7 / 173.5 / 174.2 | 1.07-1.72 | 0 |
| crun/hardened, product seccomp + PYTHONPYCACHEPREFIX=/tmp/pycache | python_start_pass | 90 | 17.2 | 0.8 | 16.8 | 17.1 | 17.8 | 19.3 | 24.4 | 17.1 / 17.1 / 17.1 | 1.07-1.72 | 0 |
| crun/hardened, product seccomp + PYTHONPYCACHEPREFIX=/tmp/pycache | python_subprocess_100x_true | 15 | 58.1 | 3.4 | 53.8 | 57.4 | 63.4 | 66.0 | 66.7 | 58.9 / 57.0 / 57.5 | 1.07-1.72 | 0 |
| crun/hardened, product seccomp + PYTHONPYCACHEPREFIX=/tmp/pycache | small_files_2000x1KiB_write_read_unlink | 15 | 112.4 | 3.4 | 108.9 | 111.2 | 119.1 | 120.1 | 120.3 | 111.2 / 110.0 / 112.7 | 1.07-1.72 | 0 |
| crun/hardened, product seccomp profile | cpu_loop_2M_iterations | 15 | 271.8 | 26.0 | 251.4 | 255.5 | 309.7 | 313.4 | 314.3 | 251.9 / 255.5 / 307.6 | 1.08-1.78 | 0 |
| crun/hardened, product seccomp profile | fork_shell_300x_bin_true | 15 | 170.9 | 2.4 | 167.5 | 170.3 | 175.2 | 176.3 | 176.6 | 171.3 / 169.9 / 171.7 | 1.08-1.78 | 0 |
| crun/hardened, product seccomp profile | python_import_30_stdlib_modules | 45 | 1155.2 | 8.6 | 1143.6 | 1152.6 | 1171.2 | 1178.6 | 1179.8 | 1153.6 / 1154.5 / 1149.7 | 1.08-1.78 | 0 |
| crun/hardened, product seccomp profile | python_start_pass | 90 | 25.1 | 1.3 | 24.4 | 24.7 | 26.6 | 29.4 | 34.8 | 24.8 / 24.7 / 24.7 | 1.08-1.78 | 0 |
| crun/hardened, product seccomp profile | python_subprocess_100x_true | 15 | 56.4 | 3.7 | 50.4 | 57.5 | 62.0 | 62.6 | 62.8 | 56.6 / 57.5 / 60.8 | 1.08-1.78 | 0 |
| crun/hardened, product seccomp profile | small_files_2000x1KiB_write_read_unlink | 15 | 112.0 | 2.1 | 109.9 | 111.3 | 116.5 | 116.9 | 117.1 | 111.0 / 111.3 / 111.7 | 1.08-1.78 | 0 |
| crun/plain (overlay workdir) | cpu_loop_2M_iterations | 15 | 258.6 | 6.1 | 251.7 | 257.0 | 269.9 | 270.2 | 270.2 | 264.9 / 258.7 / 252.2 | 1.20-2.55 | 0 |
| crun/plain (overlay workdir) | fork_shell_300x_bin_true | 15 | 155.5 | 2.6 | 151.0 | 155.3 | 159.9 | 160.6 | 160.8 | 154.3 / 155.3 / 155.3 | 1.20-2.55 | 0 |
| crun/plain (overlay workdir) | python_import_30_stdlib_modules | 45 | 174.2 | 3.1 | 171.6 | 173.0 | 181.2 | 182.6 | 183.5 | 174.1 / 172.7 / 173.0 | 1.20-2.55 | 0 |
| crun/plain (overlay workdir) | python_start_pass | 90 | 17.1 | 1.0 | 16.6 | 16.8 | 17.9 | 22.0 | 22.5 | 16.8 / 16.8 / 16.8 | 1.20-2.55 | 0 |
| crun/plain (overlay workdir) | python_subprocess_100x_true | 15 | 48.4 | 2.9 | 45.7 | 47.8 | 52.6 | 56.5 | 57.5 | 49.5 / 46.6 / 46.7 | 1.20-2.55 | 0 |
| crun/plain (overlay workdir) | small_files_2000x1KiB_write_read_unlink | 15 | 239.6 | 2.4 | 237.4 | 238.7 | 244.4 | 244.5 | 244.6 | 242.8 / 238.3 / 239.1 | 1.20-2.55 | 0 |
| crun/plain + tmpfs workdir | cpu_loop_2M_iterations | 15 | 257.8 | 4.9 | 251.9 | 257.0 | 265.3 | 266.4 | 266.7 | 264.2 / 252.3 / 257.3 | 1.17-2.43 | 0 |
| crun/plain + tmpfs workdir | fork_shell_300x_bin_true | 15 | 155.3 | 2.4 | 152.5 | 154.4 | 159.5 | 161.0 | 161.4 | 156.4 / 156.0 / 153.6 | 1.17-2.43 | 0 |
| crun/plain + tmpfs workdir | python_import_30_stdlib_modules | 45 | 174.1 | 3.2 | 171.4 | 172.7 | 181.5 | 183.2 | 184.2 | 174.0 / 172.3 / 172.7 | 1.17-2.43 | 0 |
| crun/plain + tmpfs workdir | python_start_pass | 90 | 16.9 | 0.6 | 16.6 | 16.8 | 17.6 | 19.2 | 21.5 | 16.8 / 16.8 / 16.8 | 1.17-2.43 | 0 |
| crun/plain + tmpfs workdir | python_subprocess_100x_true | 15 | 48.7 | 2.3 | 46.7 | 48.5 | 51.3 | 55.4 | 56.4 | 48.7 / 48.6 / 48.4 | 1.17-2.43 | 0 |
| crun/plain + tmpfs workdir | small_files_2000x1KiB_write_read_unlink | 15 | 111.4 | 2.6 | 109.5 | 110.5 | 116.7 | 118.1 | 118.4 | 112.9 / 109.7 / 110.9 | 1.17-2.43 | 0 |

## 6. Host capability probes

| probe | question | result | detail |
|---|---|---|---|
| cgroup_kill | Can the rootless user write cgroup.kill for a Podman-created container cgroup, and does it kill a fork-heavy tree? | **works** | 62 pids (60 sleeps + a fork loop) gone 4.0 ms after writing 1; Podman state 'stopped 137 false -> exited 137' after 60 ms |
| memory_oom_group | Does --cgroup-conf memory.oom.group=1 take effect? | **works** | oom.group=1: file reads 1, container ended 'exited 137 false' (status exitcode OOMKilled); control oom.group=0: ended 'exited 42 false' (42 = only the hog was killed and the shell carried on) |
| storage_opt_size | Does --storage-opt size= work rootless on this filesystem? | **fails** | Error: storage option overlay.size and overlay.inodes only supported for backingFS XFS. Found extfs |
| exec_detached_reattach | Can a detached `podman exec -d` session be re-attached? | **error** | probe raised: Expecting value: line 1 column 2 (char 1) |
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

