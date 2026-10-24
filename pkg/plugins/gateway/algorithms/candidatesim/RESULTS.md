# Candidate-selection simulation results

## What the candidate stage ships with

The gateway's candidate stage (`AIBRIX_ROUTING_CANDIDATE_K`, `AIBRIX_ROUTING_CANDIDATE_PREFIX_M`) is policy "A random+prefix" below: K pods drawn at random plus up to M prefix holders, seeded at random among equal matches, scored with exact running counts read for those pods only ("live stage 2"). It needs no index. Defaults simulated: K=16, M=16.

Output of `CANDIDATE_SIM=live go test -run TestCandidateSelectionLiveStage2 -v ./pkg/plugins/gateway/algorithms/candidatesim/` (500 pods, 4 gateways, utilization 0.85, 3 seeds) and the same with `CANDIDATE_SIM_PODS=2000 CANDIDATE_SIM_QUICK=1` (1 seed). Mean request latency relative to today's full scoring on exact counts, same seed: mean (min to max); prefix hit ratio after the slash. Lower is better; 1.00 = same as today.

| Fleet | Prefix caching off | Prefix caching on |
| --- | --- | --- |
| 500 pods | 1.06 (1.06 to 1.07) | 0.93 (0.92 to 0.95) / 99.5% (full: 99.6%) |
| 2,000 pods | 1.06 | 1.01 / 99.9% (full: 99.8%) |

The stage reads no index, so it does not depend on how fresh any background signal is. The rows labelled "C" and "C+A" below add a running-count index refreshed from Redis every 100 ms, a possible follow-up: it closes the 6% prefix-off gap but degrades when the refresh falls behind.

M has to grow with the fleet: at 2,000 pods a hot prefix is held by about 32 pods (8 at 500), and full scoring picks the least loaded of them. With M=4 the stage was 1.30x full scoring with prefix caching on at 2,000 pods (table "Live stage 2 at N=2,000" below), which is why M defaults to 16.

Full live stage-2 output at the shipped defaults (500 pods, 3 seeds):

| Policy | Index refresh | Prefix off | Prefix on |
| --- | --- | --- | --- |
| full, exact counts | every request | 0.051 s | 0.040 s / 99.6% |
| A random+prefix K=16, live stage 2 | 100ms | 1.06 (1.06 to 1.07) | 0.93 (0.92 to 0.95) / 99.5% |
| C count index K=16, live stage 2 | 100ms | 1.01 (1.00 to 1.02) | 0.92 (0.92 to 0.92) / 99.2% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 100ms | 1.00 (0.99 to 1.01) | 0.95 (0.90 to 0.98) / 99.4% |
| C count index K=16 | 100ms | 1.85 (1.85 to 1.86) | 1.77 (1.75 to 1.80) / 96.0% |
| A random+prefix K=16, live stage 2 | 1s | 1.06 (1.06 to 1.07) | 0.86 (0.84 to 0.86) / 99.7% |
| C count index K=16, live stage 2 | 1s | 8.77 (8.31 to 9.13) | 0.90 (0.88 to 0.92) / 99.6% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 1s | 1.03 (1.02 to 1.04) | 0.86 (0.84 to 0.90) / 99.6% |
| C count index K=16 | 1s | 11.73 (11.48 to 12.21) | 7.53 (7.48 to 7.57) / 83.7% |

The sections below are the design exploration behind RFC #3188: every policy considered, with M=4 and the least loaded holders seeded first unless stated.

## Main comparison (seed 42)

Output of `CANDIDATE_SIM=1 go test -run TestCandidateSelectionQuality ./pkg/plugins/gateway/algorithms/candidatesim/` (seed 42). See the package comment in sim_test.go for the model.

### prefix=false signal delay=0s (N=500, 4 gateways, utilization 0.85)

| Policy | Agreement | Mean latency (s) | P99 latency (s) | Max/mean running | Prefix hit | Gate fired | Pods scored/req |
| --- | --- | --- | --- | --- | --- | --- | --- |
| full | 100.0% | 0.051 | 0.246 | 2.22 | - | 0.0% | 500 |
| A random K=8 | 18.7% | 0.059 | 0.281 | 2.91 | - | 0.0% | 8 |
| A random K=16 | 25.4% | 0.053 | 0.252 | 2.35 | - | 0.0% | 16 |
| A random K=32 | 31.1% | 0.051 | 0.245 | 2.22 | - | 0.0% | 32 |
| A random+prefix K=16 | 25.4% | 0.053 | 0.252 | 2.35 | - | 0.0% | 16 |
| C count index K=16 | 70.3% | 0.044 | 0.221 | 1.34 | - | 0.0% | 16 |
| C+A index K/2 + random K/2, K=16 | 53.7% | 0.046 | 0.227 | 2.52 | - | 0.0% | 16 |
| D snapshot K=16 T=5ms | 62.2% | 0.074 | 0.301 | 3.17 | - | 0.0% | 16 |
| D snapshot K=8 T=50ms | 52.7% | 0.066 | 0.335 | 6.63 | - | 80.9% | 8 |
| D snapshot K=16 T=50ms | 53.6% | 0.071 | 0.352 | 6.16 | - | 76.3% | 16 |
| D snapshot K=32 T=50ms | 39.2% | 0.099 | 0.420 | 4.20 | - | 47.5% | 30 |

### prefix=false signal delay=100ms (N=500, 4 gateways, utilization 0.85)

| Policy | Agreement | Mean latency (s) | P99 latency (s) | Max/mean running | Prefix hit | Gate fired | Pods scored/req |
| --- | --- | --- | --- | --- | --- | --- | --- |
| full | 100.0% | 0.100 | 0.425 | 3.99 | - | 43.8% | 301 |
| A random K=8 | 48.3% | 0.109 | 0.461 | 3.82 | - | 54.1% | 8 |
| A random K=16 | 61.8% | 0.108 | 0.449 | 3.88 | - | 57.4% | 15 |
| A random K=32 | 67.7% | 0.105 | 0.438 | 3.88 | - | 52.8% | 28 |
| A random+prefix K=16 | 61.8% | 0.108 | 0.449 | 3.88 | - | 57.4% | 15 |
| C count index K=16 | 86.3% | 0.092 | 0.415 | 3.40 | - | 4.4% | 16 |
| C+A index K/2 + random K/2, K=16 | 72.1% | 0.093 | 0.413 | 3.59 | - | 7.3% | 16 |
| D snapshot K=16 T=5ms | 100.0% | 0.101 | 0.432 | 3.97 | - | 45.1% | 15 |
| D snapshot K=8 T=50ms | 72.1% | 0.104 | 0.469 | 6.35 | - | 94.4% | 7 |
| D snapshot K=16 T=50ms | 73.1% | 0.113 | 0.523 | 5.67 | - | 91.6% | 14 |
| D snapshot K=32 T=50ms | 78.9% | 0.113 | 0.492 | 4.71 | - | 84.8% | 25 |

### prefix=false signal delay=1s (N=500, 4 gateways, utilization 0.85)

| Policy | Agreement | Mean latency (s) | P99 latency (s) | Max/mean running | Prefix hit | Gate fired | Pods scored/req |
| --- | --- | --- | --- | --- | --- | --- | --- |
| full | 100.0% | 0.540 | 2.367 | 3.29 | - | 87.7% | 132 |
| A random K=8 | 60.2% | 0.555 | 2.534 | 3.20 | - | 87.9% | 7 |
| A random K=16 | 48.8% | 0.501 | 2.034 | 3.33 | - | 60.7% | 14 |
| A random K=32 | 76.0% | 0.542 | 2.345 | 3.34 | - | 82.3% | 25 |
| A random+prefix K=16 | 48.8% | 0.501 | 2.034 | 3.33 | - | 60.7% | 14 |
| C count index K=16 | 78.4% | 0.631 | 2.966 | 2.87 | - | 91.6% | 13 |
| C+A index K/2 + random K/2, K=16 | 72.7% | 0.600 | 2.790 | 3.13 | - | 87.0% | 13 |
| D snapshot K=16 T=5ms | 100.0% | 0.544 | 2.453 | 3.19 | - | 82.5% | 13 |
| D snapshot K=8 T=50ms | 90.8% | 0.607 | 2.827 | 3.17 | - | 97.9% | 7 |
| D snapshot K=16 T=50ms | 81.4% | 0.551 | 2.463 | 3.37 | - | 85.7% | 13 |
| D snapshot K=32 T=50ms | 82.3% | 0.498 | 2.196 | 3.08 | - | 84.5% | 25 |

### prefix=true signal delay=0s (N=500, 4 gateways, utilization 0.85)

| Policy | Agreement | Mean latency (s) | P99 latency (s) | Max/mean running | Prefix hit | Gate fired | Pods scored/req |
| --- | --- | --- | --- | --- | --- | --- | --- |
| full | 100.0% | 0.040 | 0.208 | 5.65 | 99.8% | 0.0% | 500 |
| A random K=8 | 10.3% | 0.054 | 0.273 | 3.57 | 66.0% | 0.0% | 8 |
| A random K=16 | 13.5% | 0.049 | 0.244 | 3.80 | 74.9% | 0.0% | 16 |
| A random K=32 | 15.8% | 0.049 | 0.240 | 4.02 | 81.6% | 0.0% | 32 |
| A random+prefix K=16 | 99.4% | 0.039 | 0.196 | 4.94 | 99.3% | 0.4% | 19 |
| C count index K=16 | 98.3% | 0.034 | 0.167 | 4.31 | 98.3% | 0.0% | 20 |
| C+A index K/2 + random K/2, K=16 | 99.1% | 0.039 | 0.193 | 4.62 | 99.1% | 0.0% | 19 |
| D snapshot K=16 T=5ms | 95.9% | 0.038 | 0.182 | 3.84 | 95.8% | 0.0% | 19 |
| D snapshot K=8 T=50ms | 98.0% | 0.040 | 0.192 | 3.94 | 97.9% | 0.0% | 12 |
| D snapshot K=16 T=50ms | 97.9% | 0.040 | 0.193 | 3.99 | 97.9% | 0.0% | 20 |
| D snapshot K=32 T=50ms | 98.4% | 0.039 | 0.188 | 4.10 | 98.3% | 0.0% | 35 |

### prefix=true signal delay=100ms (N=500, 4 gateways, utilization 0.85)

| Policy | Agreement | Mean latency (s) | P99 latency (s) | Max/mean running | Prefix hit | Gate fired | Pods scored/req |
| --- | --- | --- | --- | --- | --- | --- | --- |
| full | 100.0% | 0.072 | 0.298 | 4.47 | 95.0% | 24.6% | 406 |
| A random K=8 | 29.2% | 0.087 | 0.395 | 3.90 | 64.7% | 40.4% | 8 |
| A random K=16 | 33.9% | 0.082 | 0.364 | 4.03 | 72.7% | 35.0% | 16 |
| A random K=32 | 40.4% | 0.079 | 0.345 | 4.18 | 79.6% | 31.9% | 32 |
| A random+prefix K=16 | 98.0% | 0.072 | 0.299 | 4.49 | 95.4% | 28.2% | 19 |
| C count index K=16 | 96.7% | 0.071 | 0.294 | 4.49 | 94.9% | 20.8% | 19 |
| C+A index K/2 + random K/2, K=16 | 98.0% | 0.072 | 0.297 | 4.52 | 95.5% | 27.2% | 19 |
| D snapshot K=16 T=5ms | 97.9% | 0.072 | 0.302 | 4.46 | 92.5% | 28.0% | 18 |
| D snapshot K=8 T=50ms | 97.7% | 0.072 | 0.298 | 4.52 | 95.1% | 25.9% | 11 |
| D snapshot K=16 T=50ms | 97.7% | 0.072 | 0.298 | 4.46 | 94.8% | 26.5% | 19 |
| D snapshot K=32 T=50ms | 98.0% | 0.072 | 0.298 | 4.49 | 94.9% | 25.9% | 35 |

### prefix=true signal delay=1s (N=500, 4 gateways, utilization 0.85)

| Policy | Agreement | Mean latency (s) | P99 latency (s) | Max/mean running | Prefix hit | Gate fired | Pods scored/req |
| --- | --- | --- | --- | --- | --- | --- | --- |
| full | 100.0% | 0.313 | 1.337 | 2.95 | 84.8% | 66.1% | 227 |
| A random K=8 | 46.6% | 0.361 | 1.443 | 2.59 | 61.9% | 80.3% | 7 |
| A random K=16 | 56.0% | 0.337 | 1.357 | 2.66 | 68.0% | 78.7% | 14 |
| A random K=32 | 60.4% | 0.321 | 1.377 | 2.95 | 74.7% | 67.0% | 27 |
| A random+prefix K=16 | 98.6% | 0.318 | 1.366 | 2.86 | 83.5% | 72.9% | 16 |
| C count index K=16 | 97.9% | 0.312 | 1.318 | 2.86 | 84.1% | 71.6% | 16 |
| C+A index K/2 + random K/2, K=16 | 98.2% | 0.308 | 1.268 | 2.73 | 82.8% | 79.9% | 16 |
| D snapshot K=16 T=5ms | 97.4% | 0.318 | 1.319 | 2.85 | 82.1% | 67.6% | 16 |
| D snapshot K=8 T=50ms | 97.4% | 0.315 | 1.312 | 2.79 | 81.7% | 78.1% | 9 |
| D snapshot K=16 T=50ms | 97.6% | 0.323 | 1.328 | 2.77 | 81.7% | 76.5% | 16 |
| D snapshot K=32 T=50ms | 98.1% | 0.323 | 1.381 | 2.99 | 84.0% | 64.8% | 28 |

## Robustness sweep

Output of `CANDIDATE_SIM=robust go test -run TestCandidateSelectionRobustness ...`.

### N=500, exponential sizes, 5 seeds

Mean latency relative to full scoring on the same seed: mean (min to max). Full scoring's own mean latency in seconds in the first row; prefix hit ratio after the slash when prefix caching is on.

| Policy | prefix=false delay=0s | prefix=false delay=100ms | prefix=false delay=1s | prefix=true delay=0s | prefix=true delay=100ms | prefix=true delay=1s |
| --- | --- | --- | --- | --- | --- | --- |
| full | 0.051 s | 0.101 s | 0.523 s | 0.041 s / 99.7% | 0.072 s / 94.8% | 0.316 s / 83.2% |
| A random+prefix K=16 | 1.06 (1.05 to 1.07) | 1.08 (1.06 to 1.08) | 1.03 (1.00 to 1.06) | 0.98 (0.95 to 1.00) / 99.4% | 1.00 (0.98 to 1.00) / 95.8% | 0.97 (0.93 to 1.03) / 83.0% |
| C count index K=16 | 0.88 (0.87 to 0.88) | 0.92 (0.91 to 0.93) | 1.14 (1.07 to 1.18) | 0.83 (0.82 to 0.84) / 98.0% | 0.99 (0.97 to 0.99) / 94.7% | 0.98 (0.94 to 1.02) / 83.6% |
| C+A index K/2 + random K/2, K=16 | 0.90 (0.89 to 0.91) | 0.93 (0.92 to 0.94) | 1.03 (0.98 to 1.07) | 0.94 (0.91 to 0.96) / 99.0% | 1.00 (0.98 to 1.00) / 95.8% | 0.98 (0.95 to 1.03) / 83.4% |
| D snapshot K=16 T=50ms | 1.42 (1.40 to 1.44) | 1.16 (1.13 to 1.19) | 1.03 (1.01 to 1.07) | 0.94 (0.92 to 0.98) / 97.9% | 1.00 (1.00 to 1.01) / 94.6% | 1.00 (0.97 to 1.04) / 81.3% |

### N=500, heavy-tailed sizes, 3 seeds

Mean latency relative to full scoring on the same seed: mean (min to max). Full scoring's own mean latency in seconds in the first row; prefix hit ratio after the slash when prefix caching is on.

| Policy | prefix=false delay=0s | prefix=false delay=100ms | prefix=false delay=1s | prefix=true delay=0s | prefix=true delay=100ms | prefix=true delay=1s |
| --- | --- | --- | --- | --- | --- | --- |
| full | 0.050 s | 0.082 s | 0.230 s | 0.041 s / 99.7% | 0.059 s / 96.9% | 0.142 s / 84.1% |
| A random+prefix K=16 | 1.07 (1.04 to 1.09) | 1.08 (1.08 to 1.09) | 1.01 (0.98 to 1.04) | 0.95 (0.88 to 1.00) / 99.4% | 0.99 (0.99 to 1.00) / 97.9% | 1.07 (0.98 to 1.13) / 81.0% |
| C count index K=16 | 0.89 (0.88 to 0.91) | 0.91 (0.90 to 0.92) | 1.01 (0.97 to 1.07) | 0.81 (0.78 to 0.82) / 97.9% | 0.99 (0.98 to 0.99) / 96.3% | 1.03 (0.99 to 1.07) / 83.3% |
| C+A index K/2 + random K/2, K=16 | 0.91 (0.90 to 0.93) | 0.94 (0.93 to 0.94) | 1.00 (0.99 to 1.01) | 0.92 (0.90 to 0.93) / 98.8% | 1.00 (1.00 to 1.00) / 97.5% | 1.06 (1.02 to 1.12) / 81.4% |
| D snapshot K=16 T=50ms | 1.35 (1.33 to 1.37) | 1.23 (1.22 to 1.25) | 1.08 (1.07 to 1.10) | 0.95 (0.92 to 0.97) / 97.6% | 1.01 (1.01 to 1.02) / 95.7% | 1.05 (1.03 to 1.09) / 81.4% |

### N=2000, exponential sizes, 1 seed

Mean latency relative to full scoring on the same seed: mean (min to max). Full scoring's own mean latency in seconds in the first row; prefix hit ratio after the slash when prefix caching is on.

| Policy | prefix=false delay=0s | prefix=false delay=100ms | prefix=false delay=1s | prefix=true delay=0s | prefix=true delay=100ms | prefix=true delay=1s |
| --- | --- | --- | --- | --- | --- | --- |
| full | 0.051 s | 0.099 s | 0.478 s | 0.036 s / 99.6% | 0.070 s / 94.5% | 0.300 s / 88.3% |
| A random+prefix K=16 | 1.05 | 1.05 | 1.00 | 1.00 / 99.7% | 0.99 / 96.6% | 1.02 / 88.2% |
| C count index K=16 | 0.87 | 0.88 | 1.03 | 0.90 / 98.8% | 0.98 / 95.9% | 1.00 / 88.1% |
| C+A index K/2 + random K/2, K=16 | 0.89 | 0.91 | 1.04 | 1.00 / 99.4% | 0.98 / 96.9% | 0.98 / 87.7% |
| D snapshot K=16 T=50ms | 1.16 | 0.96 | 1.06 | 1.09 / 98.8% | 1.00 / 95.0% | 1.02 / 87.9% |

## Live stage 2

Output of `CANDIDATE_SIM=live CANDIDATE_SIM_M=4 CANDIDATE_SIM_SEED=load go test -run TestCandidateSelectionLiveStage2 ...`: the index is refreshed every "Index refresh" (plus this gateway's own picks live), and the candidates are scored with exact counts, as a gateway that reads only the candidates from Redis would.

Mean latency relative to full scoring on exact counts (today with Redis), same seed: mean (min to max). Prefix hit ratio after the slash.

| Policy | Index refresh | Prefix off | Prefix on |
| --- | --- | --- | --- |
| full, exact counts | every request | 0.051 s | 0.040 s / 99.6% |
| A random+prefix K=16, live stage 2 | 100ms | 1.06 (1.06 to 1.07) | 1.08 (1.05 to 1.10) / 99.5% |
| C count index K=16, live stage 2 | 100ms | 1.01 (1.00 to 1.02) | 1.06 (1.04 to 1.09) / 99.1% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 100ms | 1.00 (0.99 to 1.01) | 1.07 (1.03 to 1.11) / 99.4% |
| C count index K=16 | 100ms | 1.85 (1.85 to 1.86) | 1.74 (1.71 to 1.77) / 94.6% |
| A random+prefix K=16, live stage 2 | 1s | 1.06 (1.06 to 1.07) | 1.02 (0.98 to 1.10) / 99.5% |
| C count index K=16, live stage 2 | 1s | 8.77 (8.31 to 9.13) | 6.43 (6.25 to 6.55) / 84.3% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 1s | 1.03 (1.02 to 1.04) | 1.36 (1.03 to 2.02) / 97.3% |
| C count index K=16 | 1s | 11.73 (11.48 to 12.21) | 7.61 (7.47 to 7.82) / 83.8% |

## Live stage 2 at N=2,000

`CANDIDATE_SIM=live CANDIDATE_SIM_QUICK=1 CANDIDATE_SIM_PODS=2000 CANDIDATE_SIM_M=4|16 [CANDIDATE_SIM_SEED=load] go test -run TestCandidateSelectionLiveStage2 ...`: one seed, 100 ms index refresh. Mean latency relative to full scoring on exact counts; prefix hit ratio after the slash. Full scoring: 0.050 s with prefix off, 0.035 s / 99.8% with prefix on.

| Policy | M | Prefix holders seeded | Prefix off | Prefix on |
| --- | --- | --- | --- | --- |
| A random+prefix K=16, live stage 2 | 4 | least loaded first | 1.06 | 1.30 / 99.8% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 4 | least loaded first | 1.00 | 1.24 / 99.8% |
| A random+prefix K=16, live stage 2 | 16 | least loaded first | 1.06 | 1.04 / 99.9% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 16 | least loaded first | 1.00 | 1.01 / 99.9% |
| A random+prefix K=16, live stage 2 | 16 | random (as shipped) | 1.06 | 1.01 / 99.9% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 16 | random (as shipped) | 1.00 | 1.01 / 99.8% |

With 2,000 pods each prefix group is held by about 32 pods (8 at N=500). Full scoring picks the least loaded holder; with M=4 the candidates hold only four, which costs about a quarter of the mean latency. M=16 closes the gap. Seeding holders at random does at least as well as ranking them by the gateway's possibly stale view, so SeedCandidates needs no load signal to pick its seeds.
