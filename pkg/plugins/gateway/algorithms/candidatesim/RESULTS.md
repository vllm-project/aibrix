# Candidate-selection simulation results

## K-only default-on evaluation (K=16, 32, 64)

These results evaluate the shipped K-only candidate stage (`AIBRIX_ROUTING_CANDIDATE_K`, 4K+holders scout, best K/2 ranked by in-memory estimate + K/2 random, live stage 2, gate on sample) at `SignalDelay=1s` (matching the default-on `aibrix:rrq:*` remote running reader interval from #3532) against exact-count full scoring (`K=0`, `SignalDelay=0`, matching production `K=0` reading Redis on every request).

Output of `CANDIDATE_SIM=defaultk go test -run 'TestCandidateSelectionDefaultK|TestCandidateSelectionInflightCap|TestCandidateSelectionBlacklist|TestCandidateSelectionMidFleet10Seed' -v ./pkg/plugins/gateway/algorithms/candidatesim/` (4 gateways, 3 seeds unless noted). Mean and P99 latency ratios are relative to exact `full (K=0)` on the same seed; lower is better (`1.00x` = same as exact full scoring).

**What this means for the default:** across `prefix=false` and large fleets (`N=2000`, where `8K < N`), `K=64` closes nearly all of the gap to exact full scoring (`1.01–1.02x` prefix-off, `0.96–0.98x` prefix-on) while cutting Redis reads `31×` and leaving every deployment with `N <= 64` untouched. On mid-size fleets (`N=300–500`) with `prefix=true` where `8K >= N`, the shipped whole-fleet scout causes mild stale-estimate herding even without a blacklist, so over 10 seeds `K=64` is no better than `K=32` (`1.146x` vs `1.058x` at `N=300, util=0.85`, and `1.100x` vs `1.095x` at `util=0.92`; see the 10-seed table below) and has higher tail variance under heavy correlated blacklists (`BL=40%`). That tradeoff is worth tracking explicitly when choosing `K=64` vs `K=32` (or whether to keep a random `4K` scout when `8K >= N`).

### Default-K sweep across fleet size and utilization (delay=1s vs exact full K=0)

#### Utilization = 0.85

| N | Policy | prefix=false (mean / P99) | prefix=true (mean / P99 / hit%) |
| --- | --- | --- | --- |
| 200 | full (K=0, exact) | 0.050s / 0.239s | 0.045s / 0.238s / 97.7% |
| 200 | K-only K=16 (1s RRQ) | 1.07x (1.06..1.09) / 1.06x | 1.16x (1.12..1.19) / 1.12x / 97.4% |
| 200 | K-only K=32 (1s RRQ) | 1.07x (1.06..1.07) / 1.06x | 1.15x (1.12..1.19) / 1.11x / 97.5% |
| 200 | K-only K=64 (1s RRQ) | 1.04x (1.01..1.07) / 1.04x | 1.07x (1.01..1.14) / 1.04x / 97.8% |
| 300 | full (K=0, exact) | 0.051s / 0.244s | 0.043s / 0.225s / 98.9% |
| 300 | K-only K=16 (1s RRQ) | 1.07x (1.04..1.09) / 1.06x | 1.09x (1.06..1.12) / 1.06x / 98.3% |
| 300 | K-only K=32 (1s RRQ) | 1.03x (1.02..1.04) / 1.02x | 1.08x (1.03..1.13) / 1.06x / 98.7% |
| 300 | K-only K=64 (1s RRQ) | 1.01x (1.00..1.02) / 1.01x | 1.14x (1.13..1.15) / 1.14x / 98.7% |
| 500 | full (K=0, exact) | 0.050s / 0.240s | 0.041s / 0.212s / 99.5% |
| 500 | K-only K=16 (1s RRQ) | 1.07x (1.06..1.08) / 1.06x | 1.15x (1.08..1.21) / 1.12x / 99.1% |
| 500 | K-only K=32 (1s RRQ) | 1.03x (1.02..1.04) / 1.03x | 1.07x (0.97..1.18) / 1.04x / 99.3% |
| 500 | K-only K=64 (1s RRQ) | 1.03x (1.02..1.04) / 1.02x | 1.08x (0.97..1.14) / 1.06x / 99.4% |
| 2000 | full (K=0, exact) | 0.050s / 0.241s | 0.036s / 0.184s / 99.8% |
| 2000 | K-only K=16 (1s RRQ) | 1.06x (1.05..1.06) / 1.04x | 1.14x (1.13..1.15) / 1.12x / 99.6% |
| 2000 | K-only K=32 (1s RRQ) | 1.03x (1.02..1.04) / 1.02x | 1.07x (1.00..1.15) / 1.05x / 99.7% |
| 2000 | K-only K=64 (1s RRQ) | 1.02x (1.01..1.02) / 1.01x | 0.96x (0.90..1.02) / 0.93x / 99.9% |

#### Utilization = 0.92

| N | Policy | prefix=false (mean / P99) | prefix=true (mean / P99 / hit%) |
| --- | --- | --- | --- |
| 200 | full (K=0, exact) | 0.059s / 0.276s | 0.045s / 0.229s / 98.0% |
| 200 | K-only K=16 (1s RRQ) | 1.12x (1.02..1.22) / 1.16x | 1.21x (1.17..1.24) / 1.21x / 97.7% |
| 200 | K-only K=32 (1s RRQ) | 1.01x (1.00..1.04) / 1.01x | 1.16x (1.11..1.20) / 1.17x / 97.9% |
| 200 | K-only K=64 (1s RRQ) | 1.01x (0.97..1.04) / 1.02x | 1.16x (1.10..1.21) / 1.17x / 97.9% |
| 300 | full (K=0, exact) | 0.058s / 0.274s | 0.044s / 0.226s / 99.2% |
| 300 | K-only K=16 (1s RRQ) | 1.11x (1.09..1.13) / 1.11x | 1.17x (1.13..1.20) / 1.15x / 98.6% |
| 300 | K-only K=32 (1s RRQ) | 1.03x (1.01..1.05) / 1.03x | 1.13x (1.07..1.20) / 1.12x / 98.8% |
| 300 | K-only K=64 (1s RRQ) | 1.01x (0.99..1.03) / 1.00x | 1.13x (1.09..1.18) / 1.13x / 98.9% |
| 500 | full (K=0, exact) | 0.057s / 0.269s | 0.042s / 0.216s / 99.6% |
| 500 | K-only K=16 (1s RRQ) | 1.06x (1.05..1.07) / 1.07x | 1.17x (1.13..1.20) / 1.16x / 99.2% |
| 500 | K-only K=32 (1s RRQ) | 1.05x (1.03..1.07) / 1.04x | 1.12x (0.97..1.22) / 1.12x / 99.4% |
| 500 | K-only K=64 (1s RRQ) | 1.02x (1.01..1.03) / 1.02x | 1.10x (1.07..1.12) / 1.10x / 99.4% |
| 2000 | full (K=0, exact) | 0.057s / 0.267s | 0.037s / 0.186s / 99.9% |
| 2000 | K-only K=16 (1s RRQ) | 1.06x (1.06..1.07) / 1.07x | 1.22x (1.14..1.31) / 1.21x / 99.7% |
| 2000 | K-only K=32 (1s RRQ) | 1.01x (1.00..1.02) / 1.01x | 1.13x (1.08..1.20) / 1.13x / 99.8% |
| 2000 | K-only K=64 (1s RRQ) | 1.01x (0.99..1.02) / 1.01x | 0.98x (0.92..1.04) / 0.97x / 99.9% |

### Per-replica `requestsInflight` cap and false-429 evaluation (delay=1s, 3 seeds)

Models the soft in-memory scout exclusion (#3467) followed by the authoritative exact-count cap filter on the K candidates:

| Fleet | Scenario | Policy | 429% | False-429% | Mean Lat Ratio | P99 Ratio |
| --- | --- | --- | --- | --- | --- | --- |
| N=300 | util=0.85 cap=2 prefix=false | K=16 / K=32 / K=64 | 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% | 1.05x / 1.02x / 1.00x | 1.05x / 1.02x / 1.01x |
| N=300 | util=0.92 cap=4 prefix=false | K=16 / K=32 / K=64 | 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% | 1.09x / 1.02x / 1.01x | 1.08x / 1.01x / 1.00x |
| N=300 | util=0.92 cap=12 prefix=true | K=16 / K=32 / K=64 | 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% | 1.17x / 1.12x / 1.13x | 1.15x / 1.11x / 1.13x |
| N=300 | util=1.05 cap=4 prefix=false | full / K=16 / K=32 / K=64 | 6.316% / 6.233% / 6.329% / 6.495% | 0.000% / 6.233% / 6.308% / 6.026% | 1.00x / 0.97x / 0.99x / 1.00x | 1.00x / 0.95x / 0.98x / 1.01x |
| N=500 | util=0.85 cap=2 prefix=false | K=16 / K=32 / K=64 | 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% | 1.06x / 1.03x / 1.02x | 1.06x / 1.03x / 1.03x |
| N=500 | util=0.92 cap=4 prefix=false | K=16 / K=32 / K=64 | 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% | 1.08x / 1.02x / 1.03x | 1.09x / 1.02x / 1.03x |
| N=500 | util=0.92 cap=12 prefix=true | K=16 / K=32 / K=64 | 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% | 1.17x / 1.12x / 1.10x | 1.16x / 1.12x / 1.10x |
| N=500 | util=1.05 cap=4 prefix=false | full / K=16 / K=32 / K=64 | 6.296% / 5.836% / 6.126% / 6.092% | 0.000% / 5.836% / 6.126% / 6.079% | 1.00x / 0.96x / 0.99x / 0.99x | 1.00x / 0.94x / 0.98x / 0.99x |

Across all sustainable loads tested (`util=0.85` and `util=0.92`), 429 and false-429 rates are `0.000%`. Under `105%` overload (`util=1.05, cap=4, prefix=false`), the total rejection rate matches `full (K=0)` (~`6.1–6.5%` vs `6.3%`), but nearly all K-only rejections are false 429s (another pod elsewhere in the fleet had room at that moment, so the stage rejects different requests than full scoring would; full scoring has none by construction). In production, `aibrix_routing_inflight_rejections_total{scope="candidates"|"fleet"}` records which set the cap was checked on (`fleet` before the default flips or when `N <= K`, `candidates` once the stage narrows the pool; the gateway does not read the rest of the fleet to separate true vs false 429s at runtime), so comparing the overall inflight 429 rate before (`scope="fleet"`) and after (`scope="candidates"`) flipping the default validates this prediction on live traffic.

### Pod blacklist × K-only evaluation (prefix=true, util=0.85, delay=1s)

Models the cap-aware scout blacklist filter plus post-selection `excludeBlacklistedCandidates` and `O(N)` `ReselectCandidates` (#3543), across 3 seeds at `N=300/500` and 2 seeds at `N=2000`:

| Scenario | Policy | Mean Lat Ratio | P99 Ratio | Prefix Hit% | O(N) Reselect% |
| --- | --- | --- | --- | --- | --- |
| N=300 BL=15% (uniform) | K=16 / K=32 / K=64 | 1.07x / 1.06x / 1.17x | 1.05x / 1.05x / 1.18x | 98.0% / 98.4% / 98.3% | 0.00% / 0.00% / 0.00% |
| N=300 BL=40% (uniform) | K=16 / K=32 / K=64 | 1.01x / 0.99x / 1.06x | 0.97x / 0.96x / 1.06x | 96.9% / 97.2% / 97.1% | 0.49% / 0.02% / 0.00% |
| N=300 BL=40% (correlated hot holders) | K=16 / K=32 / K=64 | 1.31x / 1.28x / 1.46x | 1.47x / 1.28x / 1.80x | 93.5% / 94.2% / 90.8% | 0.94% / 0.04% / 0.00% |
| N=500 BL=15% (uniform) | K=16 / K=32 / K=64 | 1.10x / 1.02x / 1.06x | 1.05x / 0.98x / 1.04x | 98.9% / 99.1% / 99.2% | 0.00% / 0.00% / 0.00% |
| N=500 BL=40% (uniform) | K=16 / K=32 / K=64 | 1.06x / 0.94x / 1.08x | 1.04x / 0.92x / 1.07x | 98.3% / 98.7% / 98.7% | 0.50% / 0.06% / 0.00% |
| N=500 BL=40% (correlated hot holders) | K=16 / K=32 / K=64 | 1.28x / 1.16x / 1.62x | 1.33x / 1.14x / 2.77x | 96.1% / 96.7% / 92.8% | 1.59% / 0.26% / 0.00% |
| N=2000 BL=40% (correlated hot holders) | K=16 / K=32 / K=64 | 1.39x / 1.30x / 1.24x | 1.48x / 1.39x / 1.28x | 97.9% / 97.4% / 98.4% | 2.74% / 0.68% / 0.15% |
| N=2000 BL=40% (correlated, random-only 4K cap) | K=32 / K=64 | 1.33x / 1.16x | 1.69x / 1.20x | 95.6% / 98.2% | 0.46% / 0.01% |

### Mid-fleet 10-seed check & random-scout comparison (`N=300` and `N=500`, `prefix=true`, `delay=1s`)

On 3-seed `prefix=true` runs, differences under roughly `0.1x` are within seed noise (for example, `TestCandidateSelectionKOnly` and the 3-seed sweep above flip the `K=32` vs `K=64` ordering at `N=300, prefix=true, delay=1s` purely due to RNG consumption differences). To separate signal from noise and test keeping a random scout when `8K >= N` (dropping the `2*size >= len(readyPods)` whole-fleet shortcut in `candidateScout`), `TestCandidateSelectionMidFleet10Seed` runs 10 seeds (`1..10`, reporting mean ± population standard deviation):

| Scenario (`prefix=true, delay=1s`) | Policy | Mean Lat Ratio | P99 Ratio | Prefix Hit% | O(N) Reselect% |
| --- | --- | --- | --- | --- | --- |
| N=300 util=0.85 no BL | K=32 (shipped) | 1.058x ± 0.105 | 1.042x ± 0.122 | 98.7% | 0.00% |
| N=300 util=0.85 no BL | K=64 (shipped, 8K>=N whole-fleet) | 1.146x ± 0.056 | 1.143x ± 0.074 | 98.7% | 0.00% |
| N=300 util=0.85 no BL | K=64 (random 4K scout) | 1.116x ± 0.049 | 1.110x ± 0.061 | 98.8% | 0.00% |
| N=300 util=0.85 no BL | K=64 (random min(4K,N/2) scout) | 1.137x ± 0.061 | 1.130x ± 0.067 | 98.8% | 0.00% |
| N=300 util=0.92 no BL | K=32 (shipped) | 1.095x ± 0.089 | 1.075x ± 0.104 | 98.8% | 0.00% |
| N=300 util=0.92 no BL | K=64 (shipped, 8K>=N whole-fleet) | 1.100x ± 0.060 | 1.091x ± 0.068 | 98.9% | 0.00% |
| N=300 util=0.92 no BL | K=64 (random 4K scout) | 1.108x ± 0.060 | 1.098x ± 0.059 | 98.9% | 0.00% |
| N=300 util=0.92 no BL | K=64 (random min(4K,N/2) scout) | 1.126x ± 0.036 | 1.118x ± 0.046 | 98.9% | 0.00% |
| N=300 util=0.85 BL=40% corr | K=32 (shipped) | 1.281x ± 0.088 | 1.321x ± 0.132 | 94.0% | 0.03% |
| N=300 util=0.85 BL=40% corr | K=64 (shipped, 8K>=N whole-fleet) | 1.350x ± 0.165 | 1.508x ± 0.382 | 92.8% | 0.00% |
| N=300 util=0.85 BL=40% corr | K=64 (random 4K scout) | 1.339x ± 0.103 | 1.373x ± 0.113 | 94.5% | 0.00% |
| N=300 util=0.85 BL=40% corr | K=64 (random min(4K,N/2) scout) | 1.319x ± 0.218 | 1.419x ± 0.392 | 93.5% | 0.01% |
| N=500 util=0.85 no BL | K=32 (shipped) | 1.058x ± 0.088 | 1.032x ± 0.099 | 99.3% | 0.00% |
| N=500 util=0.85 no BL | K=64 (shipped, 8K>=N whole-fleet) | 1.096x ± 0.054 | 1.078x ± 0.057 | 99.3% | 0.00% |
| N=500 util=0.85 no BL | K=64 (random 4K scout) | 1.088x ± 0.087 | 1.072x ± 0.093 | 99.4% | 0.00% |
| N=500 util=0.85 no BL | K=64 (random min(4K,N/2) scout) | 1.067x ± 0.061 | 1.058x ± 0.069 | 99.4% | 0.00% |
| N=500 util=0.92 no BL | K=32 (shipped) | 1.107x ± 0.092 | 1.097x ± 0.100 | 99.4% | 0.00% |
| N=500 util=0.92 no BL | K=64 (shipped, 8K>=N whole-fleet) | 1.068x ± 0.069 | 1.058x ± 0.076 | 99.4% | 0.00% |
| N=500 util=0.92 no BL | K=64 (random 4K scout) | 1.112x ± 0.082 | 1.111x ± 0.091 | 99.4% | 0.00% |
| N=500 util=0.92 no BL | K=64 (random min(4K,N/2) scout) | 1.118x ± 0.047 | 1.110x ± 0.051 | 99.4% | 0.00% |
| N=500 util=0.85 BL=40% corr | K=32 (shipped) | 1.215x ± 0.132 | 1.314x ± 0.417 | 95.8% | 0.21% |
| N=500 util=0.85 BL=40% corr | K=64 (shipped, 8K>=N whole-fleet) | 1.785x ± 0.788 | 3.009x ± 2.662 | 91.5% | 0.00% |
| N=500 util=0.85 BL=40% corr | K=64 (random 4K scout) | 1.419x ± 0.272 | 1.855x ± 1.044 | 93.7% | 0.00% |
| N=500 util=0.85 BL=40% corr | K=64 (random min(4K,N/2) scout) | 1.533x ± 0.509 | 2.254x ± 1.819 | 93.2% | 0.00% |

Note on the `8K >= N` whole-fleet scout regime: when `8K >= N` (e.g., `K=64` at `N=300` or `N=500`), `candidateScout` estimates the entire ready fleet rather than drawing a per-request `4K` random scout. With `prefix=true` and `delay=1s`, all gateways rank the same whole-fleet stale estimates into the top `K/2` slots before filling the random half. That affects both unblacklisted traffic (`N=300, util=0.85`: `1.146x` vs `K=32`'s `1.058x`; `N=500, util=0.85`: `1.096x` vs `1.058x`, while `N=500, util=0.92` favors `K=64` at `1.068x` vs `1.107x`) and heavy correlated hot-holder blacklists (`BL=40%`, where `K=64` tail latency spikes to `1.508x` P99 at `N=300` and `3.009x` P99 at `N=500` even though `O(N)` reselects stay `0.00%`). Keeping a random `4K` scout instead of the whole-fleet shortcut leaves no-blacklist latency mostly unchanged (`±0.03–0.04x`), but materially damps the correlated-blacklist tail (`N=300` P99 `1.508x → 1.373x`, `N=500` P99 `3.009x → 1.855x`, hit `+1.7–2.2%`) while keeping `O(N)` reselects at `0.00%`. Once `8K < N` (`N=2000`), `K=64` already uses a per-request `4K` scout and outperforms both `K=16` and `K=32` (`0.96–0.98x` no-BL, `1.24x` / `0.15%` reselect at `BL=40%` corr).

### Fixed `TestCandidateSelectionKOnly` baseline (`N=300` and `N=500`, 3 seeds)

Output of `CANDIDATE_SIM=konly CANDIDATE_SIM_PODS=300` and `500` after fixing `TestCandidateSelectionKOnly` to run `policyFull` with exact live counts (`SignalDelay=0`) instead of stale `sc.delay` (note: on 3 seeds with `prefix=true`, differences under ~`0.1x` are within seed noise; see the 10-seed table above for `K=32` vs `K=64` at `N=300/500`):

| Fleet | Policy | prefix=false delay=100ms | prefix=false delay=1s | prefix=true delay=100ms | prefix=true delay=1s |
| --- | --- | --- | --- | --- | --- |
| N=300 | full, live stage 2 | 0.051 s | 0.051 s | 0.043 s / 98.9% | 0.043 s / 98.9% |
| N=300 | K-only K=16 | 1.04 (1.04 to 1.05); 1.01 | 1.04 (1.04 to 1.05); 1.04 | 1.11 (1.08 to 1.15); 1.08 / 98.2% | 1.11 (1.09 to 1.15); 1.08 / 98.4% |
| N=300 | K-only K=32 | 1.02 (1.01 to 1.03); 0.99 | 1.03 (1.03 to 1.04); 1.03 | 1.05 (0.99 to 1.16); 1.01 / 98.5% | 1.23 (1.20 to 1.26); 1.21 / 98.6% |
| N=300 | K-only K=64 | 1.01 (1.00 to 1.02); 0.99 | 1.03 (1.01 to 1.06); 1.02 | 1.17 (1.16 to 1.17); 1.14 / 98.6% | 1.10 (1.07 to 1.12); 1.09 / 98.8% |
| N=500 | full, live stage 2 | 0.050 s | 0.050 s | 0.041 s / 99.5% | 0.041 s / 99.5% |
| N=500 | K-only K=16 | 1.05 (1.04 to 1.06); 1.03 | 1.07 (1.06 to 1.08); 1.06 | 1.16 (1.10 to 1.21); 1.11 / 98.9% | 1.16 (1.12 to 1.22); 1.13 / 99.1% |
| N=500 | K-only K=32 | 1.03 (1.02 to 1.04); 1.01 | 1.03 (1.02 to 1.03); 1.03 | 0.97 (0.92 to 1.02); 0.93 / 99.3% | 1.08 (0.96 to 1.17); 1.05 / 99.3% |
| N=500 | K-only K=64 | 1.02 (1.02 to 1.04); 1.01 | 1.03 (1.02 to 1.04); 1.03 | 1.12 (0.92 to 1.22); 1.10 / 99.3% | 1.17 (1.15 to 1.18); 1.16 / 99.3% |

---

## What the candidate stage first shipped with

These results predate the K-only stage. The first candidate stage (#3292: `AIBRIX_ROUTING_CANDIDATE_K` plus `AIBRIX_ROUTING_CANDIDATE_PREFIX_M`, since removed) is policy "A random+prefix" below: K pods drawn at random plus up to M prefix holders, seeded at random among equal matches, then the load-imbalance gate and scoring on those pods only, with exact running counts read for them ("live stage 2, gate on sample"). It needs no index. Defaults simulated: K=16, M=16. The K-only stage that replaced it, policy "K-only" in `TestCandidateSelectionKOnly`, is compared there with this one.

Output of `CANDIDATE_SIM=live go test -run TestCandidateSelectionLiveStage2 -v ./pkg/plugins/gateway/algorithms/candidatesim/` (500 pods, 4 gateways, utilization 0.85, 3 seeds) and policy A's row of the same with `CANDIDATE_SIM_PODS=2000` (3 seeds; add `CANDIDATE_SIM_QUICK=1` for seed 1 only). Mean request latency relative to today's full scoring on exact counts, same seed: mean (min to max); prefix hit ratio after the slash. Lower is better; 1.00 = same as today.

| Fleet | Prefix caching off | Prefix caching on |
| --- | --- | --- |
| 500 pods | 1.06 (1.06 to 1.07) | 1.07 (1.05 to 1.09) / 99.4% (full: 99.6%) |
| 2,000 pods | 1.06 (1.05 to 1.06) | 1.01 (0.99 to 1.02) / 99.9% on seed 1 (full: 99.8%) |

The stage reads no index, so it does not depend on how fresh any background signal is: its rows are the same at 100 ms and 1 s refresh. The rows labelled "C" and "C+A" below add a running-count index refreshed from Redis every 100 ms, a possible follow-up: it closes the 6% prefix-off gap but degrades when the refresh falls behind. Their prefix-on cells are not comparable with A's; see the gate-order note below the table.

M has to grow with the fleet: at 2,000 pods a hot prefix is held by about 32 pods (8 at 500), and full scoring picks the least loaded of them. With M=4 the stage was 1.30x full scoring with prefix caching on at 2,000 pods (table "Live stage 2 at N=2,000" below), which is why M defaults to 16.

Full live stage-2 output at the shipped defaults (500 pods, 3 seeds):

| Policy | Index refresh | Prefix off | Prefix on |
| --- | --- | --- | --- |
| full, exact counts | every request | 0.051 s | 0.040 s / 99.6% |
| A random+prefix K=16, live stage 2, gate on sample | 100ms | 1.06 (1.06 to 1.07) | 1.07 (1.05 to 1.09) / 99.4% |
| C count index K=16, live stage 2 | 100ms | 1.01 (1.00 to 1.02) | 0.92 (0.92 to 0.92) / 99.2% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 100ms | 1.00 (0.99 to 1.01) | 0.95 (0.90 to 0.98) / 99.4% |
| C count index K=16 | 100ms | 1.85 (1.85 to 1.86) | 1.77 (1.75 to 1.80) / 96.0% |
| A random+prefix K=16, live stage 2, gate on sample | 1s | 1.06 (1.06 to 1.07) | 1.07 (1.05 to 1.09) / 99.4% |
| C count index K=16, live stage 2 | 1s | 8.77 (8.31 to 9.13) | 0.90 (0.88 to 0.92) / 99.6% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 1s | 1.03 (1.02 to 1.04) | 0.86 (0.84 to 0.90) / 99.6% |
| C count index K=16 | 1s | 11.73 (11.48 to 12.21) | 7.53 (7.48 to 7.57) / 83.7% |

Gate order matters with prefix caching on. An earlier version of this simulation ran the gate on the whole fleet before drawing the candidates, which the shipped stage cannot do because it reads only the candidates' counts. That version reported 0.93 (100 ms) and 0.86 (1 s) for the prefix-on cell at 500 pods; `CANDIDATE_SIM_GATE=fleet` still reproduces it. The fleet gate fired on a few percent of requests, almost all during warmup, and sent them to the globally least loaded pods, which shaped where prefixes settled. Prefix off and 2,000 pods are unchanged by the gate order.

The C and C+A rows above keep the fleet gate, because they model the index follow-up, where the index backs a fleet-wide gate. Since gate placement alone moves A's prefix-on cell from 1.07 to 0.86, the gap between A and C or C+A in the prefix-on column is not the index's own value. With `CANDIDATE_SIM_GATE=sample` all three policies gate their sample (500 pods, 3 seeds):

| Policy, gate on sample | Index refresh | Prefix off | Prefix on |
| --- | --- | --- | --- |
| C count index K=16, live stage 2 | 100ms | 1.01 (1.00 to 1.02) | 1.06 (1.06 to 1.07) / 99.1% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 100ms | 1.00 (0.99 to 1.01) | 1.03 (0.97 to 1.14) / 99.3% |
| C count index K=16, live stage 2 | 1s | 8.85 (8.36 to 9.34) | 1.04 (1.00 to 1.08) / 99.3% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 1s | 1.03 (1.02 to 1.04) | 0.96 (0.95 to 0.98) / 99.4% |

So the index's own gain is in the prefix-off column (1.06 to 1.00); with prefix caching on it is small and noisy.

The sections below are the design exploration behind RFC #3188: every policy considered, with M=4, the least loaded holders seeded first and the gate on the whole fleet unless stated. The Quality and Robustness tests still default to M=4 (`PrefixSeedMax: 4`); only the live stage-2 test defaults to the shipped M=16.

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

Output of `CANDIDATE_SIM=live CANDIDATE_SIM_M=4 CANDIDATE_SIM_SEED=load CANDIDATE_SIM_GATE=fleet go test -run TestCandidateSelectionLiveStage2 ...`: the index is refreshed every "Index refresh" (plus this gateway's own picks live), and the candidates are scored with exact counts, as a gateway that reads only the candidates from Redis would.

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

`CANDIDATE_SIM=live CANDIDATE_SIM_QUICK=1 CANDIDATE_SIM_PODS=2000 CANDIDATE_SIM_M=4|16 [CANDIDATE_SIM_SEED=load] [CANDIDATE_SIM_GATE=fleet] go test -run TestCandidateSelectionLiveStage2 ...`: one seed, 100 ms index refresh, gate on the whole fleet except the last A row. Mean latency relative to full scoring on exact counts; prefix hit ratio after the slash. Full scoring: 0.050 s with prefix off, 0.035 s / 99.8% with prefix on.

| Policy | M | Prefix holders seeded | Prefix off | Prefix on |
| --- | --- | --- | --- | --- |
| A random+prefix K=16, live stage 2 | 4 | least loaded first | 1.06 | 1.30 / 99.8% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 4 | least loaded first | 1.00 | 1.24 / 99.8% |
| A random+prefix K=16, live stage 2 | 16 | least loaded first | 1.06 | 1.04 / 99.9% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 16 | least loaded first | 1.00 | 1.01 / 99.9% |
| A random+prefix K=16, live stage 2, gate on sample (as shipped) | 16 | random (as shipped) | 1.06 | 1.01 / 99.9% |
| C+A index K/2 + random K/2, K=16, live stage 2 | 16 | random (as shipped) | 1.00 | 1.01 / 99.8% |

With 2,000 pods each prefix group is held by about 32 pods (8 at N=500). Full scoring picks the least loaded holder; with M=4 the candidates hold only four, which costs about a quarter of the mean latency. M=16 closes the gap. Seeding holders at random does at least as well as ranking them by the gateway's possibly stale view, so SeedCandidates needs no load signal to pick its seeds.
