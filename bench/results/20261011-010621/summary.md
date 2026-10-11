# tierbench summary: run 20261011-010621

402 blocks. Each cell is p50 (p95) of the samples pooled over the rounds; `rounds` is the per-round p50, in round order; ratio is runsc p50 over crun p50. Rows with cell `image=host` are the host itself with no sandbox and no proxy, shown in the runsc column. Raw samples are in `results.json`.


## lifecycle

| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |
|---|---|---|---|---|---|---|---|---|
| claim_cycle | image=alpine regime=quiet | http_floor_healthz_ms | 0.51 (0.77) | 0.50 (0.72) | 0.99x | 90/90 | 0.49 / 0.51 / 0.53 | 0.47 / 0.51 / 0.53 |
| claim_cycle | image=alpine regime=quiet | claim_ms | 10.5 (30.1) | 12.7 (31.9) | 1.21x | 90/90 | 11.9 / 9.33 / 10.9 | 13.1 / 11.9 / 12.9 |
| claim_cycle | image=alpine regime=quiet | exec_first_byte_ms | 7.02 (11.2) | 13.0 (22.3) | 1.85x | 90/90 | 8.24 / 6.07 / 7.40 | 13.5 / 11.5 / 13.6 |
| claim_cycle | image=alpine regime=quiet | exec_echo_total_ms | 7.09 (11.4) | 13.1 (22.4) | 1.84x | 90/90 | 8.35 / 6.19 / 7.49 | 13.8 / 11.7 / 13.7 |
| claim_cycle | image=alpine regime=quiet | claim_to_first_byte_ms | 18.6 (38.0) | 26.8 (54.8) | 1.44x | 90/90 | 22.5 / 16.0 / 18.7 | 30.9 / 23.5 / 27.5 |
| claim_cycle | image=alpine regime=quiet | delete_answered_ms | 4.25 (6.24) | 4.08 (5.89) | 0.96x | 90/90 | 4.41 / 4.08 / 4.25 | 4.00 / 4.06 / 4.20 |
| claim_cycle | image=alpine regime=quiet | delete_finished_ms | 135 (187) | 150 (216) | 1.11x | 90/90 | 142 / 131 / 134 | 178 / 135 / 149 |
| claim_cycle | image=alpine regime=quiet | claims | 17.0 (17.0) | 17.0 (17.0) | 1.00x | 6/6 | 17.0 / 17.0 / 17.0 | 17.0 / 17.0 / 17.0 |
| claim_cycle | image=alpine regime=quiet | claims_waited | 0.00 (0.00) | 0.00 (0.00) |  | 6/6 | 0.00 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |
| claim_cycle | image=alpine regime=after_refill | http_floor_healthz_ms | 0.54 (0.92) | 0.57 (1.05) | 1.06x | 90/90 | 0.51 / 0.55 / 0.56 | 0.56 / 0.59 / 0.56 |
| claim_cycle | image=alpine regime=after_refill | claim_ms | 32.9 (40.7) | 32.4 (41.9) | 0.99x | 90/90 | 32.9 / 33.2 / 32.9 | 31.5 / 38.2 / 32.0 |
| claim_cycle | image=alpine regime=after_refill | exec_first_byte_ms | 6.50 (9.54) | 15.1 (25.7) | 2.33x | 90/90 | 6.82 / 5.73 / 7.64 | 16.8 / 11.4 / 17.2 |
| claim_cycle | image=alpine regime=after_refill | exec_echo_total_ms | 6.78 (9.82) | 15.3 (26.5) | 2.25x | 90/90 | 6.92 / 5.86 / 7.77 | 17.1 / 11.5 / 17.3 |
| claim_cycle | image=alpine regime=after_refill | claim_to_first_byte_ms | 40.3 (48.1) | 48.4 (61.7) | 1.20x | 90/90 | 40.0 / 39.3 / 41.1 | 47.8 / 49.8 / 48.2 |
| claim_cycle | image=alpine regime=after_refill | delete_answered_ms | 4.21 (5.75) | 4.27 (5.99) | 1.01x | 90/90 | 4.46 / 3.99 / 4.20 | 4.76 / 3.73 / 4.62 |
| claim_cycle | image=alpine regime=after_refill | delete_finished_ms | 133 (185) | 165 (221) | 1.24x | 90/90 | 148 / 121 / 136 | 185 / 134 / 168 |
| claim_cycle | image=alpine regime=after_refill | claims | 17.0 (17.0) | 17.0 (17.0) | 1.00x | 6/6 | 17.0 / 17.0 / 17.0 | 17.0 / 17.0 / 17.0 |
| claim_cycle | image=alpine regime=after_refill | claims_waited | 0.00 (0.00) | 0.00 (0.00) |  | 6/6 | 0.00 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |
| create_to_warm | image=alpine refill=1 | create_to_warm_ms | 102 (134) | 205 (280) | 2.01x | 72/72 | 101 / 95.5 / 108 | 201 / 197 / 255 |
| create_to_warm | image=alpine refill=1 | creates_per_s | 9.46 (10.1) | 4.88 (5.02) | 0.52x | 6/6 | 9.46 / 10.1 / 8.66 | 4.91 / 4.97 / 3.85 |
| create_to_warm | image=alpine refill=1 | retries | 0.00 (0.00) | 0.00 (0.00) |  | 6/6 | 0.00 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |
| create_to_warm | image=alpine refill=1 | failures | 0.00 (0.00) | 0.00 (0.00) |  | 6/6 | 0.00 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |
| create_to_warm | image=alpine refill=2 | create_to_warm_ms | 123 (154) | 266 (356) | 2.17x | 72/72 | 120 / 116 / 143 | 245 / 237 / 331 |
| create_to_warm | image=alpine refill=2 | creates_per_s | 15.5 (15.8) | 7.50 (8.21) | 0.48x | 6/6 | 15.5 / 15.8 / 13.5 | 7.59 / 8.09 / 5.96 |
| create_to_warm | image=alpine refill=2 | retries | 0.00 (0.00) | 0.00 (0.00) |  | 6/6 | 0.00 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |
| create_to_warm | image=alpine refill=2 | failures | 0.00 (0.00) | 0.00 (0.00) |  | 6/6 | 0.00 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |
| create_to_warm | image=alpine refill=4 | create_to_warm_ms | 223 (346) | 477 (622) | 2.13x | 72/72 | 237 / 217 / 222 | 477 / 382 / 543 |
| create_to_warm | image=alpine refill=4 | creates_per_s | 16.3 (17.3) | 8.05 (10.2) | 0.49x | 6/6 | 14.6 / 16.7 / 15.7 | 8.08 / 9.62 / 6.96 |
| create_to_warm | image=alpine refill=4 | retries | 0.00 (0.00) | 0.00 (0.00) |  | 6/6 | 0.00 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |
| create_to_warm | image=alpine refill=4 | failures | 0.00 (0.00) | 0.00 (0.00) |  | 6/6 | 0.00 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |

## throughput

| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |
|---|---|---|---|---|---|---|---|---|
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | claim_ms | 34.3 (148) | 31.8 (143) | 0.93x | 90/90 | 33.4 / 33.7 / 34.7 | 31.7 / 31.0 / 32.4 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | arrivals | 30.0 (30.0) | 30.0 (30.0) | 1.00x | 3/3 | 30.0 / 30.0 / 30.0 | 30.0 / 30.0 / 30.0 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | ok | 30.0 (30.0) | 30.0 (30.0) | 1.00x | 3/3 | 30.0 / 30.0 / 30.0 | 30.0 / 30.0 / 30.0 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | wall_s | 29.0 (29.0) | 29.0 (29.1) | 1.00x | 3/3 | 29.0 / 29.0 / 29.0 | 29.1 / 29.0 / 29.0 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | sent_s | 29.0 (29.0) | 29.0 (29.0) | 1.00x | 3/3 | 29.0 / 29.0 / 29.0 | 29.0 / 29.0 / 29.0 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | server_claims | 30.0 (30.0) | 30.0 (30.0) | 1.00x | 3/3 | 30.0 / 30.0 / 30.0 | 30.0 / 30.0 / 30.0 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | server_claims_waited | 0.00 (0.00) | 0.00 (0.00) |  | 3/3 | 0.00 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | warm_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 3/3 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | served_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 3/3 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | under_50ms_fraction | 0.83 (0.98) | 0.97 (1.00) | 1.16x | 3/3 | 0.83 / 0.77 / 1.00 | 0.97 / 1.00 / 0.80 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | start_lateness_p95_ms | 0.50 (0.76) | 0.79 (0.79) | 1.57x | 3/3 | 0.50 / 0.78 / 0.35 | 0.79 / 0.79 / 0.38 |
| claim_rate | image=alpine rate=1.0 warm=8 refill=2 | achieved_rate | 1.03 (1.03) | 1.03 (1.03) | 1.00x | 3/3 | 1.03 / 1.03 / 1.03 | 1.03 / 1.03 / 1.03 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | claim_ms | 30.3 (136) | 30.9 (63.6) | 1.02x | 180/180 | 25.0 / 30.7 / 32.2 | 30.3 / 33.9 / 29.7 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | arrivals | 60.0 (60.0) | 60.0 (60.0) | 1.00x | 3/3 | 60.0 / 60.0 / 60.0 | 60.0 / 60.0 / 60.0 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | ok | 60.0 (60.0) | 60.0 (60.0) | 1.00x | 3/3 | 60.0 / 60.0 / 60.0 | 60.0 / 60.0 / 60.0 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | wall_s | 29.5 (29.5) | 29.5 (29.5) | 1.00x | 3/3 | 29.5 / 29.5 / 29.5 | 29.5 / 29.5 / 29.5 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | sent_s | 29.5 (29.5) | 29.5 (29.5) | 1.00x | 3/3 | 29.5 / 29.5 / 29.5 | 29.5 / 29.5 / 29.5 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | server_claims | 60.0 (60.0) | 60.0 (60.0) | 1.00x | 3/3 | 60.0 / 60.0 / 60.0 | 60.0 / 60.0 / 60.0 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | server_claims_waited | 0.00 (0.00) | 0.00 (0.00) |  | 3/3 | 0.00 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | warm_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 3/3 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | served_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 3/3 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | under_50ms_fraction | 0.93 (0.95) | 0.97 (0.98) | 1.04x | 3/3 | 0.93 / 0.95 / 0.85 | 0.88 / 0.97 / 0.98 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | start_lateness_p95_ms | 0.34 (0.72) | 0.66 (0.79) | 1.91x | 3/3 | 0.76 / 0.34 / 0.29 | 0.66 / 0.81 / 0.27 |
| claim_rate | image=alpine rate=2.0 warm=8 refill=2 | achieved_rate | 2.03 (2.03) | 2.03 (2.03) | 1.00x | 3/3 | 2.03 / 2.03 / 2.03 | 2.03 / 2.03 / 2.03 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | claim_ms | 31.8 (92.6) | 30.9 (39.4) | 0.97x | 270/270 | 21.5 / 32.1 / 32.4 | 31.4 / 32.4 / 13.3 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | arrivals | 90.0 (90.0) | 90.0 (90.0) | 1.00x | 3/3 | 90.0 / 90.0 / 90.0 | 90.0 / 90.0 / 90.0 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | ok | 90.0 (90.0) | 90.0 (90.0) | 1.00x | 3/3 | 90.0 / 90.0 / 90.0 | 90.0 / 90.0 / 90.0 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | wall_s | 29.7 (29.7) | 29.7 (29.7) | 1.00x | 3/3 | 29.7 / 29.7 / 29.7 | 29.7 / 29.7 / 29.7 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | sent_s | 29.7 (29.7) | 29.7 (29.7) | 1.00x | 3/3 | 29.7 / 29.7 / 29.7 | 29.7 / 29.7 / 29.7 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | server_claims | 90.0 (90.0) | 90.0 (90.0) | 1.00x | 3/3 | 90.0 / 90.0 / 90.0 | 90.0 / 90.0 / 90.0 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | server_claims_waited | 0.00 (11.7) | 0.00 (0.00) |  | 3/3 | 13.0 / 0.00 / 0.00 | 0.00 / 0.00 / 0.00 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | warm_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 3/3 | 0.86 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | served_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 3/3 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | under_50ms_fraction | 0.98 (0.98) | 0.98 (0.99) | 1.00x | 3/3 | 0.86 / 0.98 / 0.98 | 0.98 / 0.98 / 0.99 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | start_lateness_p95_ms | 0.81 (0.84) | 0.78 (0.80) | 0.96x | 3/3 | 0.84 / 0.81 / 0.33 | 0.78 / 0.81 / 0.33 |
| claim_rate | image=alpine rate=3.0 warm=8 refill=2 | achieved_rate | 3.03 (3.03) | 3.03 (3.03) | 1.00x | 3/3 | 3.03 / 3.03 / 3.03 | 3.03 / 3.03 / 3.03 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | claim_ms | 8.61 (40.1) | 19.4 (2608) | 2.25x | 450/444 | 9.80 / 7.26 / 8.28 | 488 / 8.91 / 312 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | arrivals | 150 (150) | 150 (150) | 1.00x | 3/3 | 150 / 150 / 150 | 150 / 150 / 150 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | ok | 150 (150) | 150 (150) | 1.00x | 3/3 | 150 / 150 / 150 | 144 / 150 / 150 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | wall_s | 29.8 (29.8) | 31.2 (32.7) | 1.05x | 3/3 | 29.8 / 29.8 / 29.8 | 32.9 / 29.8 / 31.2 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | sent_s | 29.8 (29.8) | 29.8 (29.8) | 1.00x | 3/3 | 29.8 / 29.8 / 29.8 | 29.8 / 29.8 / 29.8 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | server_claims | 150 (150) | 150 (150) | 1.00x | 3/3 | 150 / 150 / 150 | 150 / 150 / 150 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | server_claims_waited | 0.00 (0.00) | 79.0 (92.5) |  | 3/3 | 0.00 / 0.00 / 0.00 | 79.0 / 0.00 / 94.0 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | warm_fraction | 1.00 (1.00) | 0.43 (0.94) | 0.43x | 3/3 | 1.00 / 1.00 / 1.00 | 0.43 / 1.00 / 0.37 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | served_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 3/3 | 1.00 / 1.00 / 1.00 | 0.96 / 1.00 / 1.00 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | under_50ms_fraction | 0.99 (0.99) | 0.41 (0.94) | 0.42x | 3/3 | 0.99 / 0.99 / 0.99 | 0.41 / 1.00 / 0.38 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | start_lateness_p95_ms | 0.56 (0.78) | 2.23 (10.0) | 3.98x | 3/3 | 0.56 / 0.81 / 0.36 | 10.9 / 0.49 / 2.23 |
| claim_rate | image=alpine rate=5.0 warm=8 refill=2 | achieved_rate | 5.03 (5.03) | 4.81 (5.01) | 0.96x | 3/3 | 5.03 / 5.03 / 5.03 | 4.38 / 5.03 / 4.81 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | claim_ms | 1795 (4374) | 2669 (4945) | 1.49x | 711/493 | 1810 / 1787 / 1795 | 3511 / 2511 / 2690 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | arrivals | 300 (300) | 300 (300) | 1.00x | 3/3 | 300 / 300 / 300 | 300 / 300 / 300 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | ok | 230 (251) | 164 (200) | 0.71x | 3/3 | 228 / 253 / 230 | 125 / 204 / 164 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | wall_s | 33.5 (33.6) | 34.5 (34.7) | 1.03x | 3/3 | 33.4 / 33.5 / 33.6 | 34.7 / 34.1 / 34.5 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | sent_s | 29.9 (29.9) | 29.9 (29.9) | 1.00x | 3/3 | 29.9 / 29.9 / 29.9 | 29.9 / 29.9 / 29.9 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | server_claims | 300 (300) | 300 (300) | 1.00x | 3/3 | 300 / 300 / 300 | 300 / 300 / 300 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | server_claims_waited | 218 (236) | 154 (186) | 0.71x | 3/3 | 212 / 238 / 218 | 115 / 190 / 154 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | warm_fraction | 0.05 (0.05) | 0.03 (0.05) | 0.67x | 3/3 | 0.05 / 0.05 / 0.04 | 0.03 / 0.05 / 0.03 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | served_fraction | 0.77 (0.84) | 0.55 (0.67) | 0.71x | 3/3 | 0.76 / 0.84 / 0.77 | 0.42 / 0.68 / 0.55 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | under_50ms_fraction | 0.05 (0.06) | 0.03 (0.04) | 0.67x | 3/3 | 0.06 / 0.05 / 0.04 | 0.03 / 0.04 / 0.03 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | start_lateness_p95_ms | 1.36 (1.37) | 4.68 (8.04) | 3.45x | 3/3 | 1.37 / 0.97 / 1.36 | 8.41 / 3.41 / 4.68 |
| claim_rate | image=alpine rate=10.0 warm=8 refill=2 | achieved_rate | 6.85 (7.49) | 4.75 (5.86) | 0.69x | 3/3 | 6.83 / 7.56 / 6.85 | 3.60 / 5.99 / 4.75 |

## stream

| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |
|---|---|---|---|---|---|---|---|---|
| pty_ws_yes_256k_ack | image=alpine | seconds | 0.69 (0.81) | 10.3 (16.8) | 14.95x | 12/12 | 0.68 / 0.65 / 0.75 | 10.3 / 9.03 / 16.6 |
| pty_ws_yes_256k_ack | image=alpine | delivered_mb_s | 0.57 (0.61) | 0.04 (0.04) | 0.07x | 12/12 | 0.58 / 0.60 / 0.53 | 0.04 / 0.04 / 0.02 |
| pty_ws_yes_256k_ack | image=alpine | delivered_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 12/12 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| pty_ws_yes_256k_ack | image=alpine | received_mb | 0.39 (0.39) | 0.39 (0.39) | 1.00x | 12/12 | 0.39 / 0.39 / 0.39 | 0.39 / 0.39 / 0.39 |
| pty_ws_yes_256k_ack | image=alpine | bytes_per_frame | 90.2 (151) | 13.1 (15.6) | 0.15x | 12/12 | 87.5 / 86.2 / 138 | 14.7 / 13.0 / 13.0 |
| pty_ws_yes_256k_noack | image=alpine | seconds | 0.70 (0.83) | 8.75 (13.4) | 12.44x | 12/12 | 0.74 / 0.67 / 0.74 | 8.75 / 7.72 / 12.6 |
| pty_ws_yes_256k_noack | image=alpine | delivered_mb_s | 0.56 (0.60) | 0.04 (0.05) | 0.08x | 12/12 | 0.54 / 0.59 / 0.53 | 0.04 / 0.05 / 0.03 |
| pty_ws_yes_256k_noack | image=alpine | delivered_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 12/12 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| pty_ws_yes_256k_noack | image=alpine | received_mb | 0.39 (0.39) | 0.39 (0.39) | 1.00x | 12/12 | 0.39 / 0.39 / 0.39 | 0.39 / 0.39 / 0.39 |
| pty_ws_yes_256k_noack | image=alpine | bytes_per_frame | 85.4 (129) | 14.5 (17.6) | 0.17x | 12/12 | 80.6 / 80.8 / 124 | 16.0 / 12.6 / 14.5 |
| pty_ws_one_line_8m_ack | image=alpine | seconds | 0.54 (0.57) | 6.16 (8.53) | 11.38x | 12/12 | 0.54 / 0.41 / 0.56 | 6.16 / 5.67 / 8.04 |
| pty_ws_one_line_8m_ack | image=alpine | delivered_mb_s | 15.5 (21.2) | 1.36 (1.49) | 0.09x | 12/12 | 15.5 / 20.5 / 15.0 | 1.36 / 1.48 / 1.04 |
| pty_ws_one_line_8m_ack | image=alpine | delivered_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 12/12 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| pty_ws_one_line_8m_ack | image=alpine | received_mb | 8.39 (8.39) | 8.39 (8.39) | 1.00x | 12/12 | 8.39 / 8.39 / 8.39 | 8.39 / 8.39 / 8.39 |
| pty_ws_one_line_8m_ack | image=alpine | bytes_per_frame | 6833 (7515) | 1436 (1503) | 0.21x | 12/12 | 6666 / 7248 / 6812 | 1477 / 1428 / 1435 |
| ndjson_pipe_yes_50m | image=alpine | seconds | 3.01 (3.46) | 13.8 (21.4) | 4.59x | 12/12 | 3.01 / 2.70 / 3.39 | 13.8 / 12.9 / 21.2 |
| ndjson_pipe_yes_50m | image=alpine | delivered_mb_s | 17.5 (19.6) | 3.80 (4.07) | 0.22x | 12/12 | 17.5 / 19.4 / 15.5 | 3.80 / 4.06 / 2.47 |
| ndjson_pipe_yes_50m | image=alpine | delivered_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 12/12 | 0.99 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| ndjson_pipe_yes_50m | image=alpine | received_mb | 52.4 (52.4) | 52.4 (52.4) | 1.00x | 12/12 | 51.8 / 52.4 / 52.4 | 52.4 / 52.4 / 52.4 |
| ndjson_pipe_yes_50m | image=alpine | bytes_per_frame | 5768 (7065) | 3223 (3545) | 0.56x | 12/12 | 5666 / 5493 / 6858 | 3451 / 3233 / 2915 |
| ndjson_pty_yes_256k | image=alpine | seconds | 0.72 (1.12) | 10.3 (13.1) | 14.29x | 12/12 | 0.75 / 0.66 / 0.75 | 10.3 / 7.30 / 12.4 |
| ndjson_pty_yes_256k | image=alpine | delivered_mb_s | 0.55 (0.63) | 0.04 (0.06) | 0.07x | 12/12 | 0.53 / 0.60 / 0.52 | 0.04 / 0.05 / 0.03 |
| ndjson_pty_yes_256k | image=alpine | delivered_fraction | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 12/12 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| ndjson_pty_yes_256k | image=alpine | received_mb | 0.39 (0.39) | 0.39 (0.39) | 1.00x | 12/12 | 0.39 / 0.39 / 0.39 | 0.39 / 0.39 / 0.39 |
| ndjson_pty_yes_256k | image=alpine | bytes_per_frame | 96.8 (119) | 14.8 (17.9) | 0.15x | 12/12 | 88.6 / 86.7 / 117 | 16.5 / 14.2 / 14.5 |

## limits

| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |
|---|---|---|---|---|---|---|---|---|
| memory_bomb | image=alpine memory=256m | bomb_to_reported_ms | 415 (591) | 683 (911) | 1.65x | 24/24 | 502 / 322 / 428 | 821 / 497 / 683 |
| fork_bomb | image=alpine pids=64 | echo_before_ms | 7.82 (12.1) | 14.7 (15.2) | 1.89x | 6/6 | 8.99 / 5.76 / 10.1 | 15.0 / 12.6 / 13.7 |
| fork_bomb | image=alpine pids=64 | pids_current_during | 17.0 (18.0) | 152 (179) | 8.97x | 6/6 | 17.5 / 16.5 / 17.0 | 168 / 158 / 147 |
| fork_bomb | image=alpine pids=64 | pids_limit_hits | 124 (463) | 0.00 (0.00) | 0.00x | 6/6 | 76.5 / 80.5 / 414 | 0.00 / 0.00 / 0.00 |
| fork_bomb | image=alpine pids=64 | echo_during_ok_ms | 4.83 (43.2) | 67.0 (305) | 13.88x | 30/13 | 6.43 / 2.98 / 4.31 | 39.3 / 156 / 218 |
| fork_bomb | image=alpine pids=64 | echo_during_ok_of_5 | 5.00 (5.00) | 2.50 (4.50) | 0.50x | 6/6 | 5.00 / 5.00 / 5.00 | 4.00 / 1.00 / 1.50 |
| fork_bomb | image=alpine pids=64 | usable_after_kill_ms | 5.37 (8.12) | 112 (1105) | 20.82x | 6/6 | 7.60 / 3.88 / 5.37 | 8.66 / 111 / 811 |
| fork_bomb | image=alpine pids=64 | pids_current_after_kill | 17.0 (18.0) | 152 (179) | 8.97x | 6/6 | 17.5 / 16.5 / 17.0 | 168 / 158 / 147 |
| fork_bomb | image=alpine pids=64 | delete_finished_ms | 123 (148) | 150 (284) | 1.23x | 6/6 | 143 / 119 / 118 | 150 / 234 / 150 |
| cpu_throttle | image=alpine cpus=1.0 | cgroup_cpus_used | 1.00 (1.01) | 1.00 (1.01) | 1.00x | 6/6 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| cpu_throttle | image=alpine cpus=1.0 | cgroup_vs_limit | 1.00 (1.01) | 1.00 (1.01) | 1.00x | 6/6 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| cpu_throttle | image=alpine cpus=1.0 | app_cpus_seen | 1.00 (1.01) | 1.51 (1.63) | 1.51x | 6/6 | 1.00 / 1.00 / 1.00 | 1.52 / 1.58 / 1.46 |
| cpu_throttle | image=alpine cpus=1.0 | app_vs_limit | 1.00 (1.01) | 1.51 (1.63) | 1.51x | 6/6 | 1.00 / 1.00 / 1.00 | 1.52 / 1.58 / 1.46 |
| cpu_throttle | image=alpine cpus=1.0 | throttled_fraction_of_wall | 0.43 (0.87) | 1.07 (1.20) | 2.46x | 6/6 | 0.87 / 0.43 / 0.11 | 1.06 / 1.20 / 1.06 |
| cpu_throttle | image=alpine cpus=0.5 | cgroup_cpus_used | 0.50 (0.50) | 0.50 (0.50) | 1.00x | 6/6 | 0.50 / 0.50 / 0.50 | 0.50 / 0.50 / 0.50 |
| cpu_throttle | image=alpine cpus=0.5 | cgroup_vs_limit | 1.00 (1.01) | 1.00 (1.00) | 1.00x | 6/6 | 1.01 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| cpu_throttle | image=alpine cpus=0.5 | app_cpus_seen | 0.50 (0.50) | 1.11 (1.27) | 2.24x | 6/6 | 0.50 / 0.50 / 0.50 | 0.99 / 1.25 / 1.04 |
| cpu_throttle | image=alpine cpus=0.5 | app_vs_limit | 1.00 (1.00) | 2.23 (2.55) | 2.24x | 6/6 | 1.00 / 1.00 / 0.99 | 1.98 / 2.51 / 2.08 |
| cpu_throttle | image=alpine cpus=0.5 | throttled_fraction_of_wall | 1.18 (1.31) | 1.67 (1.80) | 1.41x | 6/6 | 1.30 / 1.18 / 0.39 | 1.64 / 1.79 / 1.62 |

## workload

| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |
|---|---|---|---|---|---|---|---|---|
| py_start | image=alpine | first_inner_ms | 17.7 (20.9) | 45.3 (80.1) | 2.56x | 3/3 | 21.2 / 17.7 / 17.4 | 83.9 / 45.3 / 45.3 |
| py_start | image=alpine | inner_ms | 18.5 (22.7) | 74.0 (90.3) | 4.00x | 30/30 | 21.1 / 17.9 / 17.4 | 83.1 / 71.3 / 72.3 |
| py_start | image=alpine | api_ms | 21.1 (25.3) | 80.0 (92.3) | 3.78x | 30/30 | 23.8 / 20.7 / 19.9 | 84.6 / 79.1 / 78.2 |
| import30_cold | image=alpine | first_inner_ms | 1232 (1248) | 4161 (4257) | 3.38x | 3/3 | 1198 / 1250 / 1232 | 4128 / 4161 / 4268 |
| import30_cold | image=alpine | inner_ms | 1281 (1495) | 4177 (4650) | 3.26x | 30/30 | 1265 / 1225 / 1399 | 4156 / 4173 / 4211 |
| import30_cold | image=alpine | api_ms | 1200 (2437) | 4169 (4241) | 3.48x | 9/9 | 2232 / 1192 / 1200 | 4131 / 4153 / 4181 |
| import30_cached | image=alpine | first_inner_ms | 1192 (1959) | 3311 (3602) | 2.78x | 3/3 | 2044 / 979 / 1192 | 3635 / 3311 / 3289 |
| import30_cached | image=alpine | inner_ms | 214 (402) | 523 (670) | 2.44x | 30/30 | 382 / 180 / 214 | 597 / 516 / 516 |
| import30_cached | image=alpine | api_ms | 184 (316) | 545 (857) | 2.96x | 30/30 | 287 / 181 / 181 | 820 / 525 / 535 |
| cpu_loop | image=alpine | first_inner_ms | 320 (322) | 448 (453) | 1.40x | 3/3 | 322 / 320 / 318 | 448 / 441 / 453 |
| cpu_loop | image=alpine | first_core_loop_ms | 268 (269) | 291 (292) | 1.09x | 3/3 | 269 / 268 / 265 | 291 / 286 / 292 |
| cpu_loop | image=alpine | inner_ms | 380 (436) | 466 (586) | 1.23x | 30/30 | 408 / 314 / 384 | 514 / 465 / 462 |
| cpu_loop | image=alpine | core_loop_ms | 317 (366) | 292 (368) | 0.92x | 30/30 | 340 / 261 / 318 | 336 / 292 / 286 |
| cpu_loop | image=alpine | api_ms | 391 (482) | 487 (594) | 1.24x | 15/15 | 446 / 321 / 391 | 581 / 464 / 476 |
| numpy | image=alpine | first_inner_ms | 1326 (1333) | 2281 (2282) | 1.72x | 3/3 | 1334 / 1154 / 1326 | 2282 / 2281 / 2273 |
| numpy | image=alpine | first_core_import_ms | 597 (632) | 1323 (1331) | 2.22x | 3/3 | 597 / 507 / 636 | 1332 / 1323 / 1311 |
| numpy | image=alpine | first_core_matmul_ms | 291 (379) | 390 (396) | 1.34x | 3/3 | 389 / 278 / 291 | 382 / 396 / 390 |
| numpy | image=alpine | first_core_svd_ms | 198 (200) | 205 (208) | 1.04x | 3/3 | 198 / 193 / 200 | 209 / 203 / 205 |
| numpy | image=alpine | inner_ms | 806 (881) | 1301 (2088) | 1.61x | 30/30 | 849 / 799 / 800 | 1397 / 1292 / 1300 |
| numpy | image=alpine | core_import_ms | 188 (203) | 342 (388) | 1.82x | 30/30 | 188 / 188 / 189 | 352 / 337 / 342 |
| numpy | image=alpine | core_matmul_ms | 266 (327) | 378 (645) | 1.42x | 30/30 | 306 / 252 / 262 | 398 / 357 / 334 |
| numpy | image=alpine | core_svd_ms | 192 (200) | 283 (605) | 1.47x | 30/30 | 195 / 193 / 163 | 295 / 210 / 281 |
| numpy | image=alpine | api_ms | 802 (893) | 1285 (2001) | 1.60x | 15/15 | 856 / 801 / 795 | 1285 / 1292 / 1281 |
| json_re_sha | image=alpine | first_inner_ms | 447 (462) | 739 (741) | 1.65x | 3/3 | 463 / 447 / 445 | 741 / 739 / 715 |
| json_re_sha | image=alpine | first_core_json_ms | 102 (106) | 199 (214) | 1.94x | 3/3 | 106 / 101 / 102 | 215 / 199 / 170 |
| json_re_sha | image=alpine | first_core_re_ms | 115 (115) | 155 (158) | 1.35x | 3/3 | 115 / 116 / 112 | 158 / 155 / 151 |
| json_re_sha | image=alpine | first_core_sha256_64MiB_ms | 170 (178) | 178 (183) | 1.05x | 3/3 | 179 / 169 / 170 | 178 / 173 / 184 |
| json_re_sha | image=alpine | inner_ms | 469 (591) | 731 (753) | 1.56x | 30/30 | 559 / 451 / 525 | 748 / 729 / 722 |
| json_re_sha | image=alpine | core_json_ms | 108 (140) | 200 (210) | 1.85x | 30/30 | 123 / 105 / 117 | 207 / 199 / 196 |
| json_re_sha | image=alpine | core_re_ms | 118 (152) | 148 (161) | 1.25x | 30/30 | 141 / 112 / 130 | 158 / 147 / 144 |
| json_re_sha | image=alpine | core_sha256_64MiB_ms | 182 (233) | 175 (185) | 0.96x | 30/30 | 219 / 170 / 210 | 179 / 174 / 174 |
| json_re_sha | image=alpine | api_ms | 464 (486) | 743 (773) | 1.60x | 15/15 | 471 / 456 / 450 | 761 / 734 / 737 |
| files_ws_2000 | image=alpine | first_inner_ms | 157 (193) | 720 (760) | 4.58x | 3/3 | 197 / 157 / 157 | 720 / 706 / 765 |
| files_ws_2000 | image=alpine | first_core_write_ms | 49.9 (62.5) | 208 (213) | 4.17x | 3/3 | 63.9 / 49.9 / 48.0 | 213 / 203 / 208 |
| files_ws_2000 | image=alpine | first_core_stat_ms | 6.20 (7.40) | 13.7 (58.0) | 2.20x | 3/3 | 7.53 / 6.20 / 5.96 | 13.7 / 13.3 / 62.9 |
| files_ws_2000 | image=alpine | first_core_read_ms | 38.1 (41.0) | 212 (261) | 5.57x | 3/3 | 41.4 / 35.4 / 38.1 | 212 / 266 / 210 |
| files_ws_2000 | image=alpine | first_core_delete_ms | 9.02 (10.9) | 30.5 (75.1) | 3.38x | 3/3 | 11.1 / 9.02 / 8.90 | 80.0 / 24.6 / 30.5 |
| files_ws_2000 | image=alpine | first_core_all_ms | 101 (122) | 512 (518) | 5.07x | 3/3 | 124 / 101 / 101 | 519 / 507 / 512 |
| files_ws_2000 | image=alpine | inner_ms | 155 (201) | 761 (1036) | 4.92x | 30/30 | 195 / 154 / 151 | 843 / 726 / 719 |
| files_ws_2000 | image=alpine | core_write_ms | 47.4 (64.6) | 200 (317) | 4.23x | 30/30 | 61.0 / 46.9 / 47.1 | 270 / 202 / 191 |
| files_ws_2000 | image=alpine | core_stat_ms | 6.22 (8.72) | 16.8 (80.8) | 2.70x | 30/30 | 7.85 / 6.16 / 6.07 | 69.9 / 15.1 / 14.6 |
| files_ws_2000 | image=alpine | core_read_ms | 35.1 (45.4) | 214 (309) | 6.10x | 30/30 | 43.4 / 35.1 / 33.7 | 291 / 209 / 212 |
| files_ws_2000 | image=alpine | core_delete_ms | 9.06 (12.6) | 80.0 (86.6) | 8.84x | 30/30 | 11.6 / 8.91 / 8.86 | 46.4 / 80.6 / 81.8 |
| files_ws_2000 | image=alpine | core_all_ms | 97.9 (128) | 534 (772) | 5.45x | 30/30 | 125 / 97.0 / 95.7 | 658 / 509 / 505 |
| files_ws_2000 | image=alpine | api_ms | 160 (202) | 775 (797) | 4.85x | 15/15 | 194 / 157 / 160 | 783 / 773 / 778 |
| files_rootfs_1500 | image=alpine | first_inner_ms | 95.9 (116) | 549 (659) | 5.72x | 3/3 | 95.9 / 118 / 91.6 | 549 / 500 / 672 |
| files_rootfs_1500 | image=alpine | first_core_read_ms | 31.6 (32.2) | 274 (362) | 8.67x | 3/3 | 32.3 / 31.6 / 31.2 | 237 / 274 / 371 |
| files_rootfs_1500 | image=alpine | inner_ms | 86.9 (98.6) | 479 (492) | 5.52x | 30/30 | 88.4 / 87.0 / 86.6 | 485 / 480 / 471 |
| files_rootfs_1500 | image=alpine | core_read_ms | 27.1 (28.2) | 218 (260) | 8.04x | 30/30 | 27.1 / 27.0 / 27.4 | 219 / 218 / 215 |
| files_rootfs_1500 | image=alpine | api_ms | 90.5 (95.2) | 481 (495) | 5.32x | 15/15 | 92.6 / 89.0 / 89.9 | 489 / 478 / 481 |
| seq_100mb | image=alpine | first_inner_ms | 160 (173) | 443 (451) | 2.77x | 3/3 | 174 / 155 / 160 | 443 / 452 / 405 |
| seq_100mb | image=alpine | first_core_write_ms | 34.5 (35.2) | 87.3 (93.4) | 2.53x | 3/3 | 35.2 / 32.2 / 34.5 | 87.3 / 77.0 / 94.1 |
| seq_100mb | image=alpine | first_core_read_ms | 57.3 (62.6) | 128 (133) | 2.24x | 3/3 | 63.2 / 56.3 / 57.3 | 128 / 133 / 118 |
| seq_100mb | image=alpine | first_core_write_mb_s | 2897 (3082) | 1146 (1283) | 0.40x | 3/3 | 2838 / 3103 / 2897 | 1146 / 1299 / 1063 |
| seq_100mb | image=alpine | first_core_read_mb_s | 1745 (1773) | 780 (840) | 0.45x | 3/3 | 1581 / 1776 / 1745 | 780 / 750 / 846 |
| seq_100mb | image=alpine | inner_ms | 159 (172) | 415 (448) | 2.61x | 30/30 | 166 / 155 / 158 | 415 / 413 / 417 |
| seq_100mb | image=alpine | core_write_ms | 32.4 (37.0) | 84.5 (94.4) | 2.61x | 30/30 | 33.0 / 31.4 / 32.8 | 84.6 / 79.2 / 85.8 |
| seq_100mb | image=alpine | core_read_ms | 57.6 (62.9) | 130 (144) | 2.26x | 30/30 | 61.1 / 56.0 / 57.6 | 134 / 134 / 126 |
| seq_100mb | image=alpine | core_write_mb_s | 3083 (3218) | 1183 (1548) | 0.38x | 30/30 | 3028 / 3187 / 3052 | 1182 / 1263 / 1165 |
| seq_100mb | image=alpine | core_read_mb_s | 1737 (1786) | 768 (841) | 0.44x | 30/30 | 1638 / 1784 / 1737 | 749 / 748 / 793 |
| seq_100mb | image=alpine | api_ms | 162 (171) | 421 (458) | 2.61x | 9/9 | 170 / 162 / 158 | 429 / 416 / 424 |
| fork_exec_500 | image=alpine | first_inner_ms | 275 (283) | 3114 (3138) | 11.34x | 3/3 | 269 / 275 / 284 | 3140 / 3086 / 3114 |
| fork_exec_500 | image=alpine | inner_ms | 279 (286) | 3118 (3353) | 11.18x | 30/30 | 271 / 278 / 284 | 3183 / 3098 / 3118 |
| fork_exec_500 | image=alpine | api_ms | 278 (294) | 3176 (3226) | 11.43x | 9/9 | 274 / 276 / 286 | 3176 / 3166 / 3184 |
| subprocess_200 | image=alpine | first_inner_ms | 169 (175) | 1187 (1295) | 7.02x | 3/3 | 169 / 167 / 175 | 1187 / 1307 / 1170 |
| subprocess_200 | image=alpine | first_core_run_ms | 116 (121) | 984 (1078) | 8.52x | 3/3 | 114 / 116 / 122 | 984 / 1089 / 980 |
| subprocess_200 | image=alpine | inner_ms | 174 (184) | 1181 (1313) | 6.77x | 30/30 | 176 / 173 / 174 | 1183 / 1184 / 1180 |
| subprocess_200 | image=alpine | core_run_ms | 118 (129) | 976 (1062) | 8.25x | 30/30 | 116 / 119 / 120 | 979 / 974 / 973 |
| subprocess_200 | image=alpine | api_ms | 179 (182) | 1195 (1203) | 6.69x | 9/9 | 179 / 178 / 179 | 1188 / 1196 / 1197 |
| pipeline | image=alpine | first_inner_ms | 881 (898) | 1525 (1548) | 1.73x | 3/3 | 881 / 837 / 900 | 1525 / 1512 / 1550 |
| pipeline | image=alpine | inner_ms | 878 (1128) | 1580 (1598) | 1.80x | 30/30 | 947 / 866 / 871 | 1591 / 1583 / 1548 |
| pipeline | image=alpine | api_ms | 893 (1029) | 1582 (1599) | 1.77x | 15/15 | 930 / 894 / 861 | 1595 / 1582 / 1575 |
| threads_io | image=alpine | first_inner_ms | 1577 (1857) | 5815 (6242) | 3.69x | 3/3 | 1889 / 1577 / 1351 | 6289 / 5815 / 5718 |
| threads_io | image=alpine | first_core_pool_ms | 1495 (1681) | 5566 (5680) | 3.72x | 3/3 | 1702 / 1495 / 1246 | 5693 / 5566 / 5479 |
| threads_io | image=alpine | inner_ms | 1386 (1514) | 5942 (6496) | 4.29x | 30/30 | 1431 / 1386 / 1352 | 5896 / 5991 / 5918 |
| threads_io | image=alpine | core_pool_ms | 1308 (1416) | 5680 (6231) | 4.34x | 30/30 | 1346 / 1308 / 1277 | 5629 / 5742 / 5674 |
| threads_io | image=alpine | api_ms | 1380 (1459) | 5915 (6033) | 4.29x | 15/15 | 1429 / 1377 / 1350 | 5899 / 5991 / 5915 |
| git_200_commits | image=alpine | inner_ms | 3004 (3826) | 22630 (37453) | 7.53x | 9/9 | 3730 / 2580 / 3204 | 22549 / 23510 / 22427 |
| git_200_commits | image=alpine | api_ms | 3321 (3497) | 28297 (29358) | 8.52x | 3/2 | 2013 / 3517 / 3321 | 29476 / 27118 |
| git_200_noauto | image=alpine | first_inner_ms | 1070 (1096) | 8579 (8602) | 8.02x | 3/3 | 1033 / 1099 / 1070 | 8577 / 8579 / 8605 |
| git_200_noauto | image=alpine | inner_ms | 1066 (1144) | 8593 (8864) | 8.06x | 12/12 | 1010 / 1069 / 1095 | 8637 / 8633 / 8545 |
| py_start | image=slim | first_inner_ms | 14.4 (14.4) | 48.1 (49.2) | 3.35x | 3/3 | 13.8 / 14.4 / 14.4 | 48.1 / 25.5 / 49.3 |
| py_start | image=slim | inner_ms | 13.9 (14.7) | 24.6 (52.3) | 1.77x | 30/30 | 13.9 / 14.2 / 13.9 | 24.4 / 25.4 / 24.4 |
| py_start | image=slim | api_ms | 16.0 (18.4) | 29.0 (59.8) | 1.81x | 30/30 | 15.9 / 16.7 / 15.8 | 28.6 / 29.0 / 28.7 |
| import30_cold | image=slim | first_inner_ms | 758 (780) | 1154 (1162) | 1.52x | 3/3 | 783 / 758 / 748 | 1047 / 1163 / 1154 |
| import30_cold | image=slim | inner_ms | 756 (927) | 1038 (1054) | 1.37x | 30/30 | 753 / 757 / 757 | 1033 / 1049 / 1033 |
| import30_cold | image=slim | api_ms | 776 (940) | 1051 (1058) | 1.35x | 9/9 | 761 / 854 / 932 | 1051 / 1049 / 1053 |
| import30_cached | image=slim | first_inner_ms | 633 (639) | 886 (889) | 1.40x | 3/3 | 624 / 633 / 640 | 878 / 886 / 890 |
| import30_cached | image=slim | inner_ms | 147 (158) | 301 (325) | 2.05x | 30/30 | 147 / 148 / 144 | 298 / 302 / 300 |
| import30_cached | image=slim | api_ms | 148 (159) | 307 (324) | 2.07x | 30/30 | 150 / 148 / 147 | 310 / 306 / 306 |
| cpu_loop | image=slim | first_inner_ms | 314 (392) | 377 (379) | 1.20x | 3/3 | 401 / 314 / 301 | 367 / 377 / 379 |
| cpu_loop | image=slim | first_core_loop_ms | 271 (347) | 285 (300) | 1.05x | 3/3 | 355 / 271 / 260 | 301 / 284 / 285 |
| cpu_loop | image=slim | inner_ms | 310 (418) | 365 (398) | 1.18x | 30/30 | 386 / 364 / 301 | 367 / 367 / 362 |
| cpu_loop | image=slim | core_loop_ms | 268 (366) | 286 (318) | 1.06x | 30/30 | 334 / 318 / 261 | 285 / 286 / 285 |
| cpu_loop | image=slim | api_ms | 341 (390) | 441 (472) | 1.29x | 15/15 | 307 / 367 / 378 | 450 / 464 / 382 |
| numpy | image=slim | first_inner_ms | 920 (1051) | 1090 (1118) | 1.19x | 3/3 | 804 / 920 / 1066 | 1122 / 1090 / 1068 |
| numpy | image=slim | first_core_import_ms | 439 (552) | 519 (538) | 1.18x | 3/3 | 352 / 439 / 565 | 540 / 519 / 500 |
| numpy | image=slim | first_core_matmul_ms | 192 (192) | 239 (240) | 1.25x | 3/3 | 192 / 130 / 192 | 180 / 241 / 239 |
| numpy | image=slim | first_core_svd_ms | 115 (178) | 110 (177) | 0.96x | 3/3 | 106 / 185 / 115 | 185 / 110 / 109 |
| numpy | image=slim | inner_ms | 686 (710) | 813 (899) | 1.18x | 30/30 | 699 / 610 / 649 | 809 / 808 / 878 |
| numpy | image=slim | core_import_ms | 188 (200) | 245 (271) | 1.31x | 30/30 | 191 / 187 / 189 | 250 / 244 / 244 |
| numpy | image=slim | core_matmul_ms | 170 (181) | 189 (239) | 1.11x | 30/30 | 174 / 162 / 168 | 183 / 190 / 189 |
| numpy | image=slim | core_svd_ms | 112 (192) | 111 (188) | 0.99x | 30/30 | 189 / 108 / 108 | 111 / 111 / 148 |
| numpy | image=slim | api_ms | 685 (696) | 883 (2062) | 1.29x | 15/15 | 686 / 605 / 689 | 813 / 891 / 883 |
| json_re_sha | image=slim | first_inner_ms | 387 (400) | 477 (489) | 1.23x | 3/3 | 380 / 402 / 387 | 491 / 477 / 472 |
| json_re_sha | image=slim | first_core_json_ms | 88.7 (89.8) | 111 (120) | 1.25x | 3/3 | 86.3 / 89.9 / 88.7 | 121 / 108 / 111 |
| json_re_sha | image=slim | first_core_re_ms | 73.6 (75.7) | 81.9 (82.4) | 1.11x | 3/3 | 76.0 / 73.6 / 73.3 | 81.3 / 82.5 / 81.9 |
| json_re_sha | image=slim | first_core_sha256_64MiB_ms | 170 (174) | 171 (175) | 1.01x | 3/3 | 169 / 170 / 175 | 175 / 171 / 171 |
| json_re_sha | image=slim | inner_ms | 387 (481) | 467 (481) | 1.21x | 30/30 | 391 / 387 / 387 | 468 / 467 / 466 |
| json_re_sha | image=slim | core_json_ms | 93.0 (111) | 112 (125) | 1.20x | 30/30 | 93.3 / 89.9 / 92.9 | 109 / 111 / 114 |
| json_re_sha | image=slim | core_re_ms | 74.0 (94.2) | 77.4 (86.1) | 1.05x | 30/30 | 78.0 / 73.3 / 74.5 | 76.7 / 77.5 / 77.6 |
| json_re_sha | image=slim | core_sha256_64MiB_ms | 169 (215) | 172 (177) | 1.02x | 30/30 | 170 / 172 / 169 | 175 / 172 / 173 |
| json_re_sha | image=slim | api_ms | 392 (482) | 478 (488) | 1.22x | 15/15 | 398 / 431 / 390 | 484 / 483 / 474 |
| files_ws_2000 | image=slim | first_inner_ms | 124 (125) | 507 (542) | 4.10x | 3/3 | 121 / 125 / 124 | 469 / 507 / 546 |
| files_ws_2000 | image=slim | first_core_write_ms | 37.0 (38.9) | 164 (167) | 4.42x | 3/3 | 35.8 / 39.1 / 37.0 | 164 / 163 / 168 |
| files_ws_2000 | image=slim | first_core_stat_ms | 5.87 (5.91) | 7.26 (9.05) | 1.23x | 3/3 | 5.75 / 5.87 / 5.92 | 7.26 / 7.07 / 9.25 |
| files_ws_2000 | image=slim | first_core_read_ms | 27.4 (28.0) | 181 (184) | 6.61x | 3/3 | 26.3 / 27.4 / 28.1 | 128 / 185 / 181 |
| files_ws_2000 | image=slim | first_core_delete_ms | 8.73 (8.85) | 76.7 (87.5) | 8.79x | 3/3 | 8.67 / 8.86 / 8.73 | 74.7 / 76.7 / 88.7 |
| files_ws_2000 | image=slim | first_core_all_ms | 79.8 (81.1) | 431 (445) | 5.41x | 3/3 | 76.6 / 81.3 / 79.8 | 373 / 431 / 447 |
| files_ws_2000 | image=slim | inner_ms | 123 (128) | 504 (572) | 4.11x | 30/30 | 122 / 123 / 123 | 501 / 512 / 503 |
| files_ws_2000 | image=slim | core_write_ms | 36.1 (39.2) | 164 (186) | 4.54x | 30/30 | 35.7 / 36.0 / 36.2 | 173 / 159 / 163 |
| files_ws_2000 | image=slim | core_stat_ms | 5.77 (6.65) | 8.06 (62.0) | 1.40x | 30/30 | 5.80 / 5.77 / 5.75 | 8.23 / 8.57 / 7.60 |
| files_ws_2000 | image=slim | core_read_ms | 26.9 (28.8) | 133 (194) | 4.94x | 30/30 | 27.0 / 27.1 / 26.8 | 130 / 186 / 155 |
| files_ws_2000 | image=slim | core_delete_ms | 8.77 (9.35) | 77.3 (83.1) | 8.81x | 30/30 | 8.77 / 8.78 / 8.76 | 76.1 / 76.5 / 79.4 |
| files_ws_2000 | image=slim | core_all_ms | 77.9 (81.7) | 395 (443) | 5.07x | 30/30 | 77.8 / 78.5 / 78.3 | 388 / 402 / 391 |
| files_ws_2000 | image=slim | api_ms | 126 (131) | 513 (578) | 4.07x | 15/15 | 127 / 126 / 124 | 513 / 505 / 509 |
| files_rootfs_1500 | image=slim | first_inner_ms | 63.0 (69.4) | 207 (385) | 3.28x | 3/3 | 62.8 / 70.1 / 63.0 | 198 / 207 / 405 |
| files_rootfs_1500 | image=slim | first_core_read_ms | 16.4 (16.4) | 94.2 (223) | 5.74x | 3/3 | 16.4 / 16.5 / 16.0 | 84.1 / 94.2 / 237 |
| files_rootfs_1500 | image=slim | inner_ms | 59.9 (64.2) | 185 (194) | 3.09x | 30/30 | 60.6 / 61.1 / 59.2 | 184 / 185 / 186 |
| files_rootfs_1500 | image=slim | core_read_ms | 13.9 (15.1) | 63.6 (77.7) | 4.57x | 30/30 | 13.9 / 14.2 / 13.7 | 63.8 / 60.7 / 64.9 |
| files_rootfs_1500 | image=slim | api_ms | 62.7 (66.1) | 189 (198) | 3.02x | 15/15 | 62.4 / 63.1 / 64.6 | 187 / 193 / 189 |
| seq_100mb | image=slim | first_inner_ms | 106 (106) | 196 (196) | 1.86x | 3/3 | 106 / 105 / 106 | 196 / 196 / 187 |
| seq_100mb | image=slim | first_core_write_ms | 34.5 (34.5) | 83.1 (89.0) | 2.41x | 3/3 | 34.6 / 33.3 / 34.5 | 80.2 / 89.7 / 83.1 |
| seq_100mb | image=slim | first_core_read_ms | 16.2 (16.5) | 18.4 (18.5) | 1.14x | 3/3 | 15.7 / 16.5 / 16.2 | 18.5 / 16.9 / 18.4 |
| seq_100mb | image=slim | first_core_write_mb_s | 2901 (2991) | 1204 (1242) | 0.41x | 3/3 | 2894 / 3001 / 2901 | 1246 / 1115 / 1204 |
| seq_100mb | image=slim | first_core_read_mb_s | 6170 (6332) | 5430 (5855) | 0.88x | 3/3 | 6350 / 6064 / 6170 | 5405 / 5902 / 5430 |
| seq_100mb | image=slim | inner_ms | 105 (111) | 210 (221) | 2.00x | 30/30 | 103 / 107 / 104 | 211 / 210 / 209 |
| seq_100mb | image=slim | core_write_ms | 33.1 (34.9) | 78.8 (95.2) | 2.38x | 30/30 | 33.0 / 33.5 / 32.9 | 78.7 / 78.8 / 78.2 |
| seq_100mb | image=slim | core_read_ms | 16.1 (17.5) | 18.6 (29.6) | 1.15x | 30/30 | 14.7 / 17.2 / 16.1 | 18.4 / 18.4 / 19.6 |
| seq_100mb | image=slim | core_write_mb_s | 3019 (3083) | 1269 (1593) | 0.42x | 30/30 | 3030 / 2984 / 3037 | 1270 / 1268 / 1279 |
| seq_100mb | image=slim | core_read_mb_s | 6204 (6974) | 5374 (6106) | 0.87x | 30/30 | 6798 / 5806 / 6204 | 5427 / 5431 / 5113 |
| seq_100mb | image=slim | api_ms | 107 (109) | 216 (233) | 2.03x | 9/9 | 104 / 107 / 107 | 207 / 229 / 216 |
| fork_exec_500 | image=slim | first_inner_ms | 271 (282) | 2554 (2557) | 9.41x | 3/3 | 271 / 284 / 271 | 2557 / 2554 / 2552 |
| fork_exec_500 | image=slim | inner_ms | 272 (288) | 2536 (2739) | 9.31x | 30/30 | 270 / 280 / 271 | 2526 / 2588 / 2513 |
| fork_exec_500 | image=slim | api_ms | 273 (296) | 2606 (2648) | 9.54x | 9/9 | 272 / 282 / 272 | 2598 / 2615 / 2597 |
| subprocess_200 | image=slim | first_inner_ms | 176 (179) | 1192 (1213) | 6.76x | 3/3 | 176 / 180 / 175 | 1192 / 1215 / 1180 |
| subprocess_200 | image=slim | first_core_run_ms | 135 (137) | 1101 (1114) | 8.13x | 3/3 | 135 / 138 / 135 | 1101 / 1115 / 1077 |
| subprocess_200 | image=slim | inner_ms | 182 (191) | 1204 (1258) | 6.62x | 30/30 | 181 / 184 / 180 | 1205 / 1210 / 1198 |
| subprocess_200 | image=slim | core_run_ms | 139 (149) | 1094 (1147) | 7.86x | 30/30 | 139 / 143 / 137 | 1097 / 1100 / 1087 |
| subprocess_200 | image=slim | api_ms | 183 (190) | 1227 (1276) | 6.69x | 9/9 | 182 / 187 / 180 | 1227 / 1271 / 1219 |
| pipeline | image=slim | first_inner_ms | 84.6 (86.3) | 266 (282) | 3.14x | 3/3 | 83.9 / 86.5 / 84.6 | 284 / 257 / 266 |
| pipeline | image=slim | inner_ms | 106 (146) | 293 (297) | 2.77x | 30/30 | 113 / 102 / 104 | 294 / 294 / 292 |
| pipeline | image=slim | api_ms | 109 (122) | 294 (300) | 2.69x | 15/15 | 116 / 106 / 109 | 294 / 297 / 294 |
| git_200_commits | image=slim | first_inner_ms | 1520 (1576) | 12334 (12379) | 8.12x | 3/3 | 1448 / 1520 / 1582 | 12334 / 12384 / 12167 |
| git_200_commits | image=slim | inner_ms | 1494 (1661) | 12252 (12387) | 8.20x | 12/12 | 1462 / 1595 / 1494 | 12223 / 12358 / 12202 |
| git_200_commits | image=slim | api_ms | 1478 (1621) | 12374 (12421) | 8.37x | 3/3 | 1466 / 1637 / 1478 | 12271 / 12426 / 12374 |
| git_200_noauto | image=slim | first_inner_ms | 1013 (1076) | 6715 (6779) | 6.63x | 3/3 | 999 / 1013 / 1082 | 6715 / 6787 / 6671 |
| git_200_noauto | image=slim | inner_ms | 1017 (1119) | 6777 (7096) | 6.66x | 12/12 | 981 / 1020 / 1107 | 6777 / 6813 / 6707 |
| threads_io | image=slim | first_inner_ms | 1458 (1485) | 3840 (3882) | 2.63x | 3/3 | 1488 / 1451 / 1458 | 3834 / 3887 / 3840 |
| threads_io | image=slim | first_core_pool_ms | 1348 (1380) | 3657 (3693) | 2.71x | 3/3 | 1383 / 1348 / 1340 | 3657 / 3697 / 3655 |
| threads_io | image=slim | inner_ms | 1449 (1487) | 3807 (3894) | 2.63x | 30/30 | 1477 / 1449 / 1420 | 3803 / 3847 / 3800 |
| threads_io | image=slim | core_pool_ms | 1374 (1415) | 3678 (3764) | 2.68x | 30/30 | 1381 / 1376 / 1365 | 3660 / 3692 / 3664 |
| threads_io | image=slim | api_ms | 1464 (1504) | 3812 (3891) | 2.60x | 15/15 | 1424 / 1477 / 1429 | 3805 / 3812 / 3812 |

## network

| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |
|---|---|---|---|---|---|---|---|---|
| https | image=host | get_new_conn_ms |  | 78.4 (89.1) |  | 120 |  | 75.8 / 78.1 / 82.0 |
| https | image=host | get_keep_conn_ms |  | 24.4 (29.7) |  | 120 |  | 23.9 / 23.7 / 26.7 |
| https | image=host | download_mb_s |  | 8.28 (10.3) |  | 18 |  | 9.82 / 8.49 / 1.89 |
| https | image=host | download_s |  | 4.94 (23.2) |  | 18 |  | 4.16 / 4.82 / 21.6 |
| https | image=host | big_bytes |  | 40849331 (40849331) |  | 6 |  | 40849331 / 40849331 / 40849331 |
| pip | image=alpine | requests_cold_s | 4.07 (4.17) | 10.0 (10.1) | 2.46x | 3/3 | 4.07 / 3.97 / 4.18 | 10.0 / 9.95 / 10.1 |
| pip | image=alpine | requests_noop_s | 0.57 (0.57) | 1.71 (1.71) | 3.01x | 3/3 | 0.57 / 0.57 / 0.57 | 1.71 / 1.70 / 1.71 |
| pip | image=alpine | requests_repeat_cached_s | 1.47 (1.52) | 3.90 (3.91) | 2.65x | 3/3 | 1.46 / 1.47 / 1.53 | 3.90 / 3.91 / 3.89 |
| pip | image=alpine | numpy_cold_s | 7.18 (14.1) | 15.2 (15.7) | 2.12x | 3/3 | 7.01 / 7.18 / 14.9 | 15.2 / 15.1 / 15.7 |
| pip | image=alpine | numpy_repeat_cached_s | 4.90 (4.92) | 14.4 (14.5) | 2.94x | 3/3 | 4.92 / 4.90 / 4.88 | 14.4 / 14.5 / 14.3 |
| https | image=alpine | get_new_conn_ms | 81.7 (91.3) | 85.2 (94.6) | 1.04x | 60/60 | 80.1 / 79.4 / 83.1 | 81.0 / 81.0 / 89.7 |
| https | image=alpine | get_keep_conn_ms | 25.8 (28.9) | 24.9 (27.9) | 0.97x | 60/60 | 27.9 / 24.3 / 25.7 | 24.1 / 24.6 / 25.6 |
| https | image=alpine | download_mb_s | 8.86 (10.6) | 9.79 (10.4) | 1.11x | 9/9 | 10.4 / 9.15 / 7.33 | 9.93 / 9.92 / 5.29 |
| https | image=alpine | download_s | 4.61 (18.2) | 4.17 (14.6) | 0.90x | 9/9 | 3.93 / 4.46 / 5.57 | 4.11 / 4.12 / 7.72 |
| pip | image=slim | requests_cold_s | 2.76 (2.80) | 3.78 (3.81) | 1.37x | 3/3 | 2.73 / 2.80 / 2.76 | 3.81 / 3.73 / 3.78 |
| pip | image=slim | requests_noop_s | 0.46 (0.51) | 0.91 (1.01) | 1.97x | 3/3 | 0.46 / 0.52 / 0.46 | 1.02 / 0.90 / 0.91 |
| pip | image=slim | requests_repeat_cached_s | 1.16 (1.26) | 1.82 (1.96) | 1.57x | 3/3 | 1.16 / 1.27 / 1.15 | 1.98 / 1.82 / 1.81 |
| pip | image=slim | numpy_cold_s | 5.29 (8.24) | 6.48 (7.97) | 1.23x | 3/3 | 5.17 / 5.29 / 8.57 | 6.36 / 6.48 / 8.13 |
| pip | image=slim | numpy_repeat_cached_s | 3.27 (3.30) | 4.81 (4.81) | 1.47x | 3/3 | 3.24 / 3.30 / 3.27 | 4.78 / 4.81 / 4.81 |
| https | image=slim | get_new_conn_ms | 80.7 (90.7) | 83.3 (91.5) | 1.03x | 60/60 | 78.2 / 76.0 / 85.8 | 81.9 / 81.6 / 85.0 |
| https | image=slim | get_keep_conn_ms | 24.9 (28.4) | 25.3 (29.7) | 1.01x | 60/60 | 24.2 / 25.3 / 25.5 | 24.9 / 24.7 / 28.8 |

## density

| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |
|---|---|---|---|---|---|---|---|---|
| ramp | image=alpine | idle_claimed_mb | 2.39 (2.58) | 17.3 (17.6) | 7.22x | 60/60 | 2.39 / 2.38 / 2.41 | 17.3 / 17.3 / 17.2 |
| ramp | image=alpine | after_python_mb | 3.13 (3.21) | 19.8 (20.1) | 6.33x | 60/60 | 3.13 / 3.11 / 3.15 | 19.8 / 19.8 / 19.7 |
| ramp | image=alpine | first_use_growth_mb | 0.73 (0.82) | 2.59 (2.85) | 3.55x | 60/60 | 0.74 / 0.73 / 0.73 | 2.64 / 2.60 / 2.52 |
| ramp | image=alpine | after_python_anon_mb | 1.87 (1.90) | 13.4 (13.7) | 7.15x | 60/60 | 1.86 / 1.86 / 1.88 | 13.4 / 13.4 / 13.3 |
| ramp | image=alpine | after_python_file_mb | 0.45 (0.45) | 3.54 (3.66) | 7.78x | 60/60 | 0.45 / 0.45 / 0.45 | 3.50 / 3.55 / 3.55 |
| ramp | image=alpine | after_python_shmem_mb | 0.45 (0.45) | 3.53 (3.62) | 7.77x | 60/60 | 0.45 / 0.45 / 0.45 | 3.50 / 3.55 / 3.54 |
| ramp | image=alpine | first_use_mem_avail_delta_mb_per_sandbox | -4.68 (1.13) | 3.04 (8.34) | -0.65x | 3/3 | -4.68 / -12.7 / 1.78 | 8.93 / 2.27 / 3.04 |
| ramp | image=alpine | teardown_s | 28.8 (34.0) | 13.4 (15.7) | 0.46x | 3/3 | 28.5 / 28.8 / 34.5 | 13.3 / 13.4 / 16.0 |
| ramp | image=alpine | teardown_per_s | 11.1 (11.2) | 11.9 (12.0) | 1.08x | 3/3 | 11.2 / 11.1 / 9.27 | 12.0 / 11.9 / 10.0 |
| ramp | image=alpine | mem_avail_after_teardown_kb | 7429472 (7584081) | 7561656 (7651242) | 1.02x | 3/3 | 7429472 / 7342472 / 7601260 | 7561656 / 7534024 / 7661196 |

## startup

| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |
|---|---|---|---|---|---|---|---|---|
| cold_start | image=alpine warm_target=4 | exec_to_ready_ms | 197 (257) | 204 (226) | 1.03x | 6/6 | 197 / 197 / 244 | 199 / 202 / 225 |
| cold_start | image=alpine warm_target=4 | exec_to_first_claim_served_ms | 358 (425) | 496 (766) | 1.38x | 6/6 | 347 / 351 / 408 | 484 / 495 / 694 |
| cold_start | image=alpine warm_target=4 | exec_to_first_output_ms | 368 (439) | 512 (784) | 1.39x | 6/6 | 357 / 364 / 422 | 499 / 512 / 713 |
| cold_start | image=alpine warm_target=4 | first_claim_ms | 157 (170) | 294 (542) | 1.88x | 6/6 | 150 / 154 / 163 | 285 / 293 / 469 |
| cold_start | image=alpine warm_target=4 | claims_2_to_6_ms | 107 (226) | 134 (282) | 1.25x | 30/30 | 64.5 / 120 / 114 | 87.0 / 162 / 143 |
| cold_start | image=alpine warm_target=4 | exec_to_pool_full_ms | 1290 (1445) | 1769 (2332) | 1.37x | 6/6 | 1290 / 1235 / 1431 | 1706 / 1724 / 2243 |
| cold_start | image=alpine warm_target=4 | first_create_ms | 139 (144) | 271 (418) | 1.95x | 6/6 | 127 / 143 / 138 | 265 / 271 / 380 |
| cold_start | image=alpine warm_target=4 | later_creates_ms | 202 (372) | 290 (443) | 1.44x | 54/54 | 209 / 163 / 226 | 282 / 269 / 354 |
| cold_start | image=alpine warm_target=4 | steady_claim_ms | 12.0 (115) | 13.4 (134) | 1.12x | 30/30 | 11.1 / 11.1 / 26.4 | 12.4 / 21.4 / 21.1 |
| cold_start | image=alpine warm_target=4 | stop_destroy_ms | 365 (425) | 345 (375) | 0.95x | 6/6 | 406 / 366 / 348 | 339 / 331 / 368 |
| restart_readopt | image=alpine warm=10 | cold_fill_s | 0.92 (1.07) | 1.20 (1.43) | 1.30x | 3/3 | 1.09 / 0.92 / 0.91 | 1.20 / 1.16 / 1.46 |
| restart_readopt | image=alpine warm=10 | stop_drain_ms | 10.8 (17.3) | 12.8 (21.4) | 1.18x | 9/9 | 10.8 / 10.7 / 11.6 | 10.8 / 10.8 / 21.3 |
| restart_readopt | image=alpine warm=10 | exec_to_ready_ms | 198 (233) | 209 (288) | 1.06x | 9/9 | 198 / 195 / 232 | 209 / 209 / 286 |
| restart_readopt | image=alpine warm=10 | exec_to_all_warm_ms | 200 (236) | 211 (291) | 1.06x | 9/9 | 200 / 197 / 234 | 210 / 211 / 289 |
| restart_readopt | image=alpine warm=10 | first_claim_after_restart_ms | 34.6 (144) | 39.1 (45.7) | 1.13x | 9/9 | 32.6 / 32.8 / 38.7 | 37.1 / 42.5 / 36.7 |
| restart_readopt | image=alpine warm=10 | exec_to_first_claim_served_ms | 235 (356) | 252 (334) | 1.07x | 9/9 | 232 / 232 / 274 | 247 / 252 / 326 |
| restart_readopt | image=alpine warm=10 | readopted | 10.0 (10.0) | 10.0 (10.0) | 1.00x | 9/9 | 10.0 / 10.0 / 10.0 | 10.0 / 10.0 / 10.0 |
| restart_readopt | image=alpine warm=10 | created_after_restart | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 9/9 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| restart_readopt | image=alpine warm=10 | host_destroy_s | 0.92 (1.14) | 1.08 (1.28) | 1.17x | 3/3 | 1.16 / 0.88 / 0.92 | 0.95 / 1.30 / 1.08 |
| restart_readopt | image=alpine warm=10 | host_destroy_per_s | 10.8 (11.3) | 9.26 (10.4) | 0.86x | 3/3 | 8.59 / 11.4 / 10.8 | 10.5 / 7.69 / 9.26 |
| restart_readopt | image=alpine warm=100 | cold_fill_s | 5.98 (6.55) | 10.1 (13.4) | 1.69x | 3/3 | 5.96 / 5.98 / 6.62 | 10.1 / 9.10 / 13.8 |
| restart_readopt | image=alpine warm=100 | stop_drain_ms | 10.9 (21.2) | 21.1 (21.2) | 1.94x | 9/9 | 10.9 / 10.7 / 21.1 | 10.8 / 11.0 / 21.2 |
| restart_readopt | image=alpine warm=100 | exec_to_ready_ms | 260 (365) | 429 (598) | 1.65x | 9/9 | 257 / 260 / 359 | 409 / 401 / 596 |
| restart_readopt | image=alpine warm=100 | exec_to_all_warm_ms | 263 (367) | 430 (602) | 1.64x | 9/9 | 260 / 262 / 360 | 412 / 405 / 599 |
| restart_readopt | image=alpine warm=100 | first_claim_after_restart_ms | 39.3 (186) | 72.6 (217) | 1.85x | 9/9 | 152 / 35.9 / 39.3 | 72.6 / 37.9 / 155 |
| restart_readopt | image=alpine warm=100 | exec_to_first_claim_served_ms | 400 (447) | 617 (797) | 1.54x | 9/9 | 415 / 298 / 400 | 562 / 443 / 760 |
| restart_readopt | image=alpine warm=100 | readopted | 100 (100) | 100 (100) | 1.00x | 9/9 | 100 / 100 / 100 | 100 / 100 / 100 |
| restart_readopt | image=alpine warm=100 | created_after_restart | 1.00 (1.00) | 1.00 (1.00) | 1.00x | 9/9 | 1.00 / 1.00 / 1.00 | 1.00 / 1.00 / 1.00 |
| restart_readopt | image=alpine warm=100 | host_destroy_s | 7.87 (9.05) | 9.15 (9.72) | 1.16x | 3/3 | 7.54 / 7.87 / 9.18 | 8.21 / 9.15 / 9.78 |
| restart_readopt | image=alpine warm=100 | host_destroy_per_s | 12.7 (13.2) | 10.9 (12.1) | 0.86x | 3/3 | 13.3 / 12.7 / 10.9 | 12.2 / 10.9 / 10.2 |

## knobs

| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |
|---|---|---|---|---|---|---|---|---|
| runsc_flags | image=alpine knob=default | exec_to_ready_ms |  | 213 (270) |  | 3 |  | 198 / 276 / 213 |
| runsc_flags | image=alpine knob=default | create_to_warm_ms |  | 209 (290) |  | 18 |  | 204 / 282 / 193 |
| runsc_flags | image=alpine knob=default | idle_scope_mb |  | 16.9 (16.9) |  | 3 |  | 16.9 / 16.8 / 16.9 |
| runsc_flags | image=alpine knob=default | py_start_first_ms |  | 62.6 (70.8) |  | 3 |  | 62.6 / 50.6 / 71.8 |
| runsc_flags | image=alpine knob=default | py_start_ms |  | 74.1 (80.0) |  | 30 |  | 71.6 / 78.3 / 73.0 |
| runsc_flags | image=alpine knob=default | import30_cold_first_ms |  | 4171 (5086) |  | 3 |  | 4171 / 5188 / 4150 |
| runsc_flags | image=alpine knob=default | import30_cold_ms |  | 4166 (5608) |  | 30 |  | 4166 / 5579 / 4109 |
| runsc_flags | image=alpine knob=default | import30_cached_first_ms |  | 3316 (4357) |  | 3 |  | 3316 / 4473 / 3294 |
| runsc_flags | image=alpine knob=default | import30_cached_ms |  | 543 (732) |  | 30 |  | 524 / 719 / 524 |
| runsc_flags | image=alpine knob=default | files_ws_2000_first_ms |  | 759 (1056) |  | 3 |  | 714 / 1088 / 759 |
| runsc_flags | image=alpine knob=default | files_ws_2000_ms |  | 770 (1094) |  | 30 |  | 746 / 1077 / 716 |
| runsc_flags | image=alpine knob=default | files_ws_2000_core_write_ms |  | 204 (322) |  | 30 |  | 199 / 310 / 192 |
| runsc_flags | image=alpine knob=default | files_ws_2000_core_stat_ms |  | 22.8 (77.0) |  | 30 |  | 16.6 / 73.3 / 16.4 |
| runsc_flags | image=alpine knob=default | files_ws_2000_core_read_ms |  | 262 (365) |  | 30 |  | 223 / 322 / 211 |
| runsc_flags | image=alpine knob=default | files_ws_2000_core_delete_ms |  | 80.6 (89.3) |  | 30 |  | 78.8 / 86.9 / 52.8 |
| runsc_flags | image=alpine knob=default | files_ws_2000_core_all_ms |  | 551 (799) |  | 30 |  | 532 / 787 / 495 |
| runsc_flags | image=alpine knob=default | files_rootfs_1500_first_ms |  | 497 (678) |  | 3 |  | 497 / 698 / 480 |
| runsc_flags | image=alpine knob=default | files_rootfs_1500_ms |  | 477 (753) |  | 30 |  | 476 / 734 / 467 |
| runsc_flags | image=alpine knob=default | files_rootfs_1500_core_read_ms |  | 224 (407) |  | 30 |  | 212 / 372 / 207 |
| runsc_flags | image=alpine knob=default | fork_exec_500_first_ms |  | 3435 (4279) |  | 3 |  | 3435 / 4373 / 3088 |
| runsc_flags | image=alpine knob=default | fork_exec_500_ms |  | 3103 (4347) |  | 30 |  | 3097 / 4312 / 3090 |
| runsc_flags | image=alpine knob=default | stop_destroy_ms |  | 710 (832) |  | 3 |  | 604 / 710 / 846 |
| runsc_flags | image=alpine knob=directfs_off | exec_to_ready_ms |  | 199 (227) |  | 3 |  | 198 / 230 / 199 |
| runsc_flags | image=alpine knob=directfs_off | create_to_warm_ms |  | 217 (239) |  | 18 |  | 216 / 226 / 204 |
| runsc_flags | image=alpine knob=directfs_off | idle_scope_mb |  | 19.7 (19.8) |  | 3 |  | 19.7 / 19.8 / 19.4 |
| runsc_flags | image=alpine knob=directfs_off | py_start_first_ms |  | 52.2 (72.4) |  | 3 |  | 41.3 / 52.2 / 74.6 |
| runsc_flags | image=alpine knob=directfs_off | py_start_ms |  | 73.9 (79.0) |  | 30 |  | 72.8 / 76.4 / 74.2 |
| runsc_flags | image=alpine knob=directfs_off | import30_cold_first_ms |  | 4240 (4606) |  | 3 |  | 4240 / 4647 / 4220 |
| runsc_flags | image=alpine knob=directfs_off | import30_cold_ms |  | 4153 (5559) |  | 30 |  | 4153 / 4372 / 4109 |
| runsc_flags | image=alpine knob=directfs_off | import30_cached_first_ms |  | 3350 (3352) |  | 3 |  | 3350 / 3352 / 3288 |
| runsc_flags | image=alpine knob=directfs_off | import30_cached_ms |  | 529 (590) |  | 30 |  | 523 / 518 / 578 |
| runsc_flags | image=alpine knob=directfs_off | files_ws_2000_first_ms |  | 769 (778) |  | 3 |  | 769 / 779 / 706 |
| runsc_flags | image=alpine knob=directfs_off | files_ws_2000_ms |  | 770 (1220) |  | 30 |  | 720 / 942 / 706 |
| runsc_flags | image=alpine knob=directfs_off | files_ws_2000_core_write_ms |  | 205 (396) |  | 30 |  | 198 / 250 / 203 |
| runsc_flags | image=alpine knob=directfs_off | files_ws_2000_core_stat_ms |  | 17.7 (67.8) |  | 30 |  | 17.7 / 64.6 / 16.4 |
| runsc_flags | image=alpine knob=directfs_off | files_ws_2000_core_read_ms |  | 224 (372) |  | 30 |  | 217 / 322 / 209 |
| runsc_flags | image=alpine knob=directfs_off | files_ws_2000_core_delete_ms |  | 75.2 (88.1) |  | 30 |  | 73.5 / 83.1 / 51.1 |
| runsc_flags | image=alpine knob=directfs_off | files_ws_2000_core_all_ms |  | 539 (898) |  | 30 |  | 513 / 729 / 519 |
| runsc_flags | image=alpine knob=directfs_off | files_rootfs_1500_first_ms |  | 580 (766) |  | 3 |  | 533 / 787 / 580 |
| runsc_flags | image=alpine knob=directfs_off | files_rootfs_1500_ms |  | 474 (581) |  | 30 |  | 471 / 522 / 451 |
| runsc_flags | image=alpine knob=directfs_off | files_rootfs_1500_core_read_ms |  | 211 (281) |  | 30 |  | 209 / 277 / 209 |
| runsc_flags | image=alpine knob=directfs_off | fork_exec_500_first_ms |  | 3102 (4731) |  | 3 |  | 3096 / 4912 / 3102 |
| runsc_flags | image=alpine knob=directfs_off | fork_exec_500_ms |  | 3160 (4368) |  | 30 |  | 3089 / 4126 / 3099 |
| runsc_flags | image=alpine knob=directfs_off | stop_destroy_ms |  | 711 (793) |  | 3 |  | 589 / 711 / 802 |
| runsc_flags | image=alpine knob=overlay2_none | exec_to_ready_ms |  | 199 (199) |  | 3 |  | 194 / 199 / 199 |
| runsc_flags | image=alpine knob=overlay2_none | create_to_warm_ms |  | 193 (213) |  | 18 |  | 194 / 192 / 194 |
| runsc_flags | image=alpine knob=overlay2_none | idle_scope_mb |  | 17.0 (17.1) |  | 3 |  | 17.1 / 16.9 / 17.0 |
| runsc_flags | image=alpine knob=overlay2_none | py_start_first_ms |  | 71.4 (73.5) |  | 3 |  | 71.4 / 43.5 / 73.7 |
| runsc_flags | image=alpine knob=overlay2_none | py_start_ms |  | 71.5 (78.2) |  | 30 |  | 73.2 / 71.7 / 70.3 |
| runsc_flags | image=alpine knob=overlay2_none | import30_cold_first_ms |  | 4163 (4176) |  | 3 |  | 4177 / 4163 / 4149 |
| runsc_flags | image=alpine knob=overlay2_none | import30_cold_ms |  | 4127 (4305) |  | 30 |  | 4166 / 4152 / 4108 |
| runsc_flags | image=alpine knob=overlay2_none | import30_cached_first_ms |  | 3293 (3310) |  | 3 |  | 3312 / 3293 / 3281 |
| runsc_flags | image=alpine knob=overlay2_none | import30_cached_ms |  | 520 (546) |  | 30 |  | 524 / 521 / 512 |
| runsc_flags | image=alpine knob=overlay2_none | files_ws_2000_first_ms |  | 748 (751) |  | 3 |  | 752 / 748 / 702 |
| runsc_flags | image=alpine knob=overlay2_none | files_ws_2000_ms |  | 718 (784) |  | 30 |  | 714 / 718 / 750 |
| runsc_flags | image=alpine knob=overlay2_none | files_ws_2000_core_write_ms |  | 194 (243) |  | 30 |  | 193 / 194 / 197 |
| runsc_flags | image=alpine knob=overlay2_none | files_ws_2000_core_stat_ms |  | 16.1 (69.8) |  | 30 |  | 14.9 / 17.4 / 17.4 |
| runsc_flags | image=alpine knob=overlay2_none | files_ws_2000_core_read_ms |  | 214 (270) |  | 30 |  | 214 / 212 / 216 |
| runsc_flags | image=alpine knob=overlay2_none | files_ws_2000_core_delete_ms |  | 78.0 (85.9) |  | 30 |  | 78.0 / 75.6 / 81.0 |
| runsc_flags | image=alpine knob=overlay2_none | files_ws_2000_core_all_ms |  | 512 (562) |  | 30 |  | 505 / 508 / 545 |
| runsc_flags | image=alpine knob=overlay2_none | files_rootfs_1500_first_ms |  | 485 (497) |  | 3 |  | 485 / 499 / 481 |
| runsc_flags | image=alpine knob=overlay2_none | files_rootfs_1500_ms |  | 451 (478) |  | 30 |  | 474 / 452 / 428 |
| runsc_flags | image=alpine knob=overlay2_none | files_rootfs_1500_core_read_ms |  | 212 (255) |  | 30 |  | 210 / 213 / 208 |
| runsc_flags | image=alpine knob=overlay2_none | fork_exec_500_first_ms |  | 3094 (3098) |  | 3 |  | 3098 / 3094 / 3071 |
| runsc_flags | image=alpine knob=overlay2_none | fork_exec_500_ms |  | 3094 (3709) |  | 30 |  | 3094 / 3097 / 3092 |
| runsc_flags | image=alpine knob=overlay2_none | stop_destroy_ms |  | 807 (968) |  | 3 |  | 751 / 986 / 807 |
| runsc_flags | image=alpine knob=overlay2_root_memory | exec_to_ready_ms |  | 198 (221) |  | 3 |  | 223 / 198 / 196 |
| runsc_flags | image=alpine knob=overlay2_root_memory | create_to_warm_ms |  | 197 (238) |  | 18 |  | 229 / 193 / 194 |
| runsc_flags | image=alpine knob=overlay2_root_memory | idle_scope_mb |  | 16.9 (16.9) |  | 3 |  | 16.9 / 16.8 / 16.9 |
| runsc_flags | image=alpine knob=overlay2_root_memory | py_start_first_ms |  | 41.0 (67.6) |  | 3 |  | 40.0 / 70.5 / 41.0 |
| runsc_flags | image=alpine knob=overlay2_root_memory | py_start_ms |  | 72.9 (77.6) |  | 30 |  | 73.3 / 73.4 / 62.0 |
| runsc_flags | image=alpine knob=overlay2_root_memory | import30_cold_first_ms |  | 4158 (4181) |  | 3 |  | 4158 / 4183 / 4104 |
| runsc_flags | image=alpine knob=overlay2_root_memory | import30_cold_ms |  | 4156 (4456) |  | 30 |  | 4158 / 4152 / 4172 |
| runsc_flags | image=alpine knob=overlay2_root_memory | import30_cached_first_ms |  | 3322 (3449) |  | 3 |  | 3322 / 3314 / 3463 |
| runsc_flags | image=alpine knob=overlay2_root_memory | import30_cached_ms |  | 528 (621) |  | 30 |  | 523 / 528 / 553 |
| runsc_flags | image=alpine knob=overlay2_root_memory | files_ws_2000_first_ms |  | 787 (790) |  | 3 |  | 787 / 791 / 720 |
| runsc_flags | image=alpine knob=overlay2_root_memory | files_ws_2000_ms |  | 768 (807) |  | 30 |  | 752 / 750 / 770 |
| runsc_flags | image=alpine knob=overlay2_root_memory | files_ws_2000_core_write_ms |  | 206 (261) |  | 30 |  | 202 / 206 / 220 |
| runsc_flags | image=alpine knob=overlay2_root_memory | files_ws_2000_core_stat_ms |  | 15.8 (72.1) |  | 30 |  | 15.4 / 15.9 / 20.4 |
| runsc_flags | image=alpine knob=overlay2_root_memory | files_ws_2000_core_read_ms |  | 214 (276) |  | 30 |  | 217 / 212 / 211 |
| runsc_flags | image=alpine knob=overlay2_root_memory | files_ws_2000_core_delete_ms |  | 79.1 (84.3) |  | 30 |  | 79.1 / 80.2 / 73.1 |
| runsc_flags | image=alpine knob=overlay2_root_memory | files_ws_2000_core_all_ms |  | 550 (588) |  | 30 |  | 550 / 551 / 551 |
| runsc_flags | image=alpine knob=overlay2_root_memory | files_rootfs_1500_first_ms |  | 491 (491) |  | 3 |  | 491 / 491 / 483 |
| runsc_flags | image=alpine knob=overlay2_root_memory | files_rootfs_1500_ms |  | 476 (511) |  | 30 |  | 451 / 450 / 501 |
| runsc_flags | image=alpine knob=overlay2_root_memory | files_rootfs_1500_core_read_ms |  | 227 (282) |  | 30 |  | 212 / 228 / 272 |
| runsc_flags | image=alpine knob=overlay2_root_memory | fork_exec_500_first_ms |  | 3097 (3958) |  | 3 |  | 3085 / 3097 / 4053 |
| runsc_flags | image=alpine knob=overlay2_root_memory | fork_exec_500_ms |  | 3104 (4470) |  | 30 |  | 3102 / 3094 / 3669 |
| runsc_flags | image=alpine knob=overlay2_root_memory | stop_destroy_ms |  | 700 (816) |  | 3 |  | 698 / 700 / 829 |

## workload

| block | cell | metric | crun | runsc | ratio | n | crun rounds | runsc rounds |
|---|---|---|---|---|---|---|---|---|
| git_200_commits | image=alpine | first_inner_ms | 2128 (2128) | 27221 (30179) | 12.79x | 1/2 | 2128 | 30508 / 23934 |

## Errors

- `workload | git_200_commits | image=alpine | crun`: 5 (r1: inner rc=128 error: unable to create temporary file: No such file or directory fatal: failed to write commit object |)
- `workload | git_200_commits | image=alpine | runsc`: 5 (r1: inner rc=128 error: bad tree object HEAD |)

## Blocks started under high load (load1 >= 3.0)

- `lifecycle | claim_cycle | image=alpine regime=quiet | crun`: 2
- `lifecycle | claim_cycle | image=alpine regime=quiet | runsc`: 3
- `lifecycle | create_to_warm | image=alpine refill=2 | crun`: 2
- `lifecycle | create_to_warm | image=alpine refill=4 | runsc`: 4
- `lifecycle | create_to_warm | image=alpine refill=4 | crun`: 2
- `throughput | claim_rate | image=alpine rate=1.0 warm=8 refill=2 | crun`: 2
- `throughput | claim_rate | image=alpine rate=2.0 warm=8 refill=2 | runsc`: 2
- `throughput | claim_rate | image=alpine rate=2.0 warm=8 refill=2 | crun`: 2
- `throughput | claim_rate | image=alpine rate=3.0 warm=8 refill=2 | runsc`: 2
- `throughput | claim_rate | image=alpine rate=5.0 warm=8 refill=2 | runsc`: 2
- `throughput | claim_rate | image=alpine rate=5.0 warm=8 refill=2 | crun`: 2
- `throughput | claim_rate | image=alpine rate=10.0 warm=8 refill=2 | crun`: 3
- `throughput | claim_rate | image=alpine rate=10.0 warm=8 refill=2 | runsc`: 2
- `stream | pty_ws_yes_256k_ack | image=alpine | crun`: 2
- `stream | pty_ws_yes_256k_ack | image=alpine | runsc`: 3
- `stream | pty_ws_yes_256k_noack | image=alpine | runsc`: 2
- `stream | pty_ws_yes_256k_noack | image=alpine | crun`: 2
- `stream | pty_ws_one_line_8m_ack | image=alpine | crun`: 3
- `stream | pty_ws_one_line_8m_ack | image=alpine | runsc`: 3
- `stream | ndjson_pipe_yes_50m | image=alpine | runsc`: 2
- `stream | ndjson_pipe_yes_50m | image=alpine | crun`: 2
- `stream | ndjson_pty_yes_256k | image=alpine | crun`: 3
- `stream | ndjson_pty_yes_256k | image=alpine | runsc`: 3
- `limits | memory_bomb | image=alpine memory=256m | crun`: 6
- `limits | memory_bomb | image=alpine memory=256m | runsc`: 6
- `limits | fork_bomb | image=alpine pids=64 | crun`: 3
- `limits | fork_bomb | image=alpine pids=64 | runsc`: 3
- `limits | cpu_throttle | image=alpine cpus=1.0 | crun`: 3
- `limits | cpu_throttle | image=alpine cpus=1.0 | runsc`: 3
- `limits | cpu_throttle | image=alpine cpus=0.5 | runsc`: 2
- `workload | json_re_sha | image=alpine | runsc`: 1
- `workload | git_200_noauto | image=alpine | runsc`: 2
- `density | ramp | image=alpine | runsc`: 3
- `density | ramp | image=alpine | crun`: 2
- `limits | cpu_throttle | image=alpine cpus=0.5 | crun`: 2
- `knobs | runsc_flags | image=alpine knob=directfs_off | runsc`: 1
- `knobs | runsc_flags | image=alpine knob=default | runsc`: 1
- `lifecycle | claim_cycle | image=alpine regime=after_refill | crun`: 1
- `lifecycle | claim_cycle | image=alpine regime=after_refill | runsc`: 1
- `startup | cold_start | image=alpine warm_target=4 | runsc`: 2
- `startup | cold_start | image=alpine warm_target=4 | crun`: 1
- `startup | restart_readopt | image=alpine warm=100 | crun`: 1
- `startup | restart_readopt | image=alpine warm=100 | runsc`: 1
- `throughput | claim_rate | image=alpine rate=1.0 warm=8 refill=2 | runsc`: 1
- `throughput | claim_rate | image=alpine rate=3.0 warm=8 refill=2 | crun`: 1
- `workload | git_200_noauto | image=alpine | crun`: 1
