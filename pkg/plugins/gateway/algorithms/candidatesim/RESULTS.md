# Candidate-selection simulation results

## K-only default-on evaluation (K=16, 32, 64)

These results evaluate the shipped K-only candidate stage (`AIBRIX_ROUTING_CANDIDATE_K`, 4K+holders scout — or the whole ready set when `8K >= N` — best K/2 ranked by in-memory estimate + K/2 random, live stage 2, gate on sample) at `SignalDelay=1s` (matching the default-on `aibrix:rrq:*` remote running reader interval from #3532) against exact-count full scoring (`K=0`, `SignalDelay=0`, matching production `K=0` reading Redis on every request).

Output of `CANDIDATE_SIM=defaultk go test -run 'TestCandidateSelectionDefaultK|TestCandidateSelectionInflightCap|TestCandidateSelectionBlacklist|TestCandidateSelectionMidFleet10Seed|TestCandidateSelectionHeavyTail' -v ./pkg/plugins/gateway/algorithms/candidatesim/` (4 gateways; 15s runs at `N <= 500`, 10s runs at `N = 2000`; seed counts noted per table). Mean and P99 latency ratios are relative to exact `full (K=0)` on the same seed; lower is better (`1.00x` = same as exact full scoring). Note that `util` is nominal in every `prefix=true` cell because `sim` sizes arrival rate for a 50% prefix hit rate while measured hit rates are 91–99%; reader lag/expiration (`3×interval` fallback to local counts) and request-path refresh are not modelled.

**What this means for the default:**
- **Quality:** with `prefix=false`, `K=64` is within `1.01–1.04x` of exact full scoring across `N=200..2000`. At `N=2000` (`8K < N`), `K=64` is statistically indistinguishable from exact full scoring on 3 seeds (`0.96–0.98x`, per-seed ranges `0.90..1.04` straddling `1.0` and overlapping `K=32`). On mid-size fleets (`N=300–500`) with `prefix=true`, however, `K=64` is **no better than `K=32`** over 10 seeds (`N=300 util=0.85`: median `1.147x` vs `1.048x`; `N=300 util=0.92`: `1.107x` vs `1.099x`; `N=500 util=0.85`: `1.118x` vs `1.020x`; `N=500 util=0.92`: `1.085x` vs `1.120x`; and under `HeavyTail=true` at `N=500`, `1.30x` vs `1.09x`), and none of the experiments below identifies a cheap fix for that gap.
- **CPU / allocations & Redis reads:** for `64 < N <= 512`, `K=64` hits the `8K >= N` whole-fleet scout path in `candidateScout` (`candidates.go:298`), so **it reduces per-request Redis reads (e.g., `300 → 64` or `500 → 64`, though the background `aibrix:rrq:*` reader's fixed per-interval sweep does not shrink with `K`) but does not save gateway CPU or GC allocations** (`~73%` of full in-process time and `~87–90%` of full allocations at `N=300–500` on `least-request`, vs `~22–36%` for `K=32`; see the CPU/alloc table below). Only when `N > 8K` (`N > 512` for `K=64`, `N > 256` for `K=32`) does the stage bound in-process work by `K`.

### In-process CPU and allocation cost by K (`BenchmarkCandidateSelection_ByK`)

Output of `go test -run='^$' -bench='^BenchmarkCandidateSelection_ByK$' -benchmem -benchtime=500ms -count=3 ./pkg/plugins/gateway/algorithms/` (`darwin/arm64`, median of 3 runs; single invocation so ratios compare like with like; `stage` isolates `selectCandidatesFromPodList`, `e2e` runs `routeFleetCandidatesOnce` through stage + gate + router, `full` runs `routeFleetOnce` over every ready pod):

| N | Strategy | Policy | Time/op | B/op | allocs/op | vs full (time / allocs) |
| --- | --- | --- | --- | --- | --- | --- |
| 300 | least-request | full (K=0) | 231.7 µs | 223.3 KB | 2,824 | 1.00x / 1.00x |
| 300 | least-request | stage / e2e K=16 | 39.2 µs / 42.8 µs | 40.4 KB / 42.6 KB | 535 / 643 | 0.18x / 0.23x |
| 300 | least-request | stage / e2e K=32 | 72.6 µs / 84.4 µs | 71.6 KB / 82.9 KB | 967 / 1,176 | 0.36x / 0.42x |
| 300 | least-request | stage / e2e K=64 (`8K>=N`) | 151.7 µs / 170.0 µs | 145.2 KB / 170.8 KB | 2,173 / 2,555 | 0.73x / 0.90x |
| 300 | prefix-cache | full (K=0) | 290.3 µs | 183.4 KB | 2,319 | 1.00x / 1.00x |
| 300 | prefix-cache | stage / e2e K=16 | 32.2 µs / 133.2 µs | 33.5 KB / 53.4 KB | 451 / 673 | 0.46x / 0.29x |
| 300 | prefix-cache | stage / e2e K=32 | 57.5 µs / 168.1 µs | 56.6 KB / 89.2 KB | 837 / 1,201 | 0.58x / 0.52x |
| 300 | prefix-cache | stage / e2e K=64 (`8K>=N`) | 95.5 µs / 224.5 µs | 90.5 KB / 139.4 KB | 1,266 / 1,910 | 0.77x / 0.82x |
| 500 | least-request | full (K=0) | 391.2 µs | 378.9 KB | 4,630 | 1.00x / 1.00x |
| 500 | least-request | stage / e2e K=16 | 38.8 µs / 42.0 µs | 40.4 KB / 42.6 KB | 535 / 643 | 0.11x / 0.14x |
| 500 | least-request | stage / e2e K=32 | 73.6 µs / 85.4 µs | 71.5 KB / 83.0 KB | 967 / 1,177 | 0.22x / 0.25x |
| 500 | least-request | stage / e2e K=64 (`8K>=N`) | 251.8 µs / 283.7 µs | 243.1 KB / 276.0 KB | 3,572 / 4,041 | 0.73x / 0.87x |
| 500 | prefix-cache | full (K=0) | 399.7 µs | 299.5 KB | 3,767 | 1.00x / 1.00x |
| 500 | prefix-cache | stage / e2e K=16 | 33.2 µs / 136.4 µs | 33.5 KB / 53.8 KB | 451 / 677 | 0.34x / 0.18x |
| 500 | prefix-cache | stage / e2e K=32 | 59.7 µs / 170.9 µs | 56.6 KB / 89.5 KB | 837 / 1,205 | 0.43x / 0.32x |
| 500 | prefix-cache | stage / e2e K=64 (`8K>=N`) | 157.8 µs / 294.8 µs | 132.7 KB / 185.7 KB | 2,066 / 2,738 | 0.74x / 0.73x |
| 2000 | least-request | full (K=0) | 1,704.9 µs | 1,616.1 KB | 18,184 | 1.00x / 1.00x |
| 2000 | least-request | stage / e2e K=16 | 41.6 µs / 45.1 µs | 40.4 KB / 42.6 KB | 535 / 642 | 0.03x / 0.04x |
| 2000 | least-request | stage / e2e K=32 | 79.2 µs / 91.8 µs | 71.5 KB / 83.0 KB | 967 / 1,177 | 0.05x / 0.06x |
| 2000 | least-request | stage / e2e K=64 (`8K<N`) | 157.7 µs / 179.0 µs | 133.6 KB / 158.3 KB | 1,866 / 2,239 | 0.10x / 0.12x |
| 2000 | prefix-cache | full (K=0) | 1,281.4 µs | 1,182.4 KB | 14,656 | 1.00x / 1.00x |
| 2000 | prefix-cache | stage / e2e K=16 | 35.9 µs / 137.3 µs | 33.5 KB / 53.0 KB | 451 / 672 | 0.11x / 0.05x |
| 2000 | prefix-cache | stage / e2e K=32 | 65.4 µs / 176.7 µs | 56.6 KB / 90.3 KB | 837 / 1,211 | 0.14x / 0.08x |
| 2000 | prefix-cache | stage / e2e K=64 (`8K<N`) | 121.8 µs / 263.8 µs | 100.0 KB / 163.2 KB | 1,608 / 2,289 | 0.21x / 0.16x |

Because `K=64` crosses the `8K >= N` whole-fleet scout threshold at `N <= 512`, `K=64` at `N=500` actually allocates more and runs slower in-process (`283.7 µs / 276.0 KB / 4,041 allocs`) than `K=64` at `N=2000` (`179.0 µs / 158.3 KB / 2,239 allocs`, where the `4K=256` random scout runs). Note also that per-request Redis read reductions (e.g., `2000 → 64`, `31×`) exclude the fixed background cost of the remote running reader (`aibrix:rrq:*` sweep: one `HGETALL` per ready pod per gateway per interval).

### Default-K sweep across fleet size and utilization (delay=1s vs exact full K=0, 4 gateways, 3 seeds; 15s at N<=500, 10s at N=2000)

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

### Heavy-tailed request sizes (`HeavyTail=true`, util=0.85, delay=1s, 4 gateways, 5 seeds)

Generated directly by `TestCandidateSelectionHeavyTail` (`full` row shows mean / P99 latency in seconds; K-only rows show median / mean ± std of mean-latency ratio and median P99 ratio):

| N | Policy | prefix=false (med / mean ± std; P99 med) | prefix=true (med / mean ± std; P99 med / hit%) |
| --- | --- | --- | --- |
| 300 | full (K=0, exact) | 0.050s / 0.623s | 0.043s / 0.557s / 98.8% |
| 300 | K-only K=16 (1s RRQ) | 1.06x / 1.06x ± 0.02; P99 1.07x | 1.23x / 1.26x ± 0.07; P99 1.22x / 98.3% |
| 300 | K-only K=32 (1s RRQ) | 1.02x / 1.03x ± 0.02; P99 1.02x | 1.15x / 1.18x ± 0.08; P99 1.15x / 98.5% |
| 300 | K-only K=64 (1s RRQ) | 1.02x / 1.01x ± 0.02; P99 0.99x | 1.15x / 1.19x ± 0.09; P99 1.14x / 98.7% |
| 500 | full (K=0, exact) | 0.049s / 0.617s | 0.041s / 0.528s / 99.6% |
| 500 | K-only K=16 (1s RRQ) | 1.07x / 1.06x ± 0.02; P99 1.06x | 1.25x / 1.24x ± 0.10; P99 1.23x / 99.0% |
| 500 | K-only K=32 (1s RRQ) | 1.04x / 1.04x ± 0.01; P99 1.04x | 1.09x / 1.09x ± 0.11; P99 1.09x / 99.2% |
| 500 | K-only K=64 (1s RRQ) | 1.03x / 1.03x ± 0.02; P99 1.03x | 1.30x / 1.27x ± 0.10; P99 1.31x / 99.2% |

### Per-replica `requestsInflight` cap and false-429 evaluation (4 gateways, delay=1s, 3 seeds)

Models the soft in-memory scout exclusion (#3467) followed by the authoritative exact-count cap filter on the K candidates. Complete output of `TestCandidateSelectionInflightCap` (including `full` baselines and all 8 scenarios per N):

| Fleet | Scenario | 429% (full / K=16 / K=32 / K=64) | False-429% (full / K=16 / K=32 / K=64) | Mean Lat Ratio (K=16 / K=32 / K=64) | P99 Ratio (K=16 / K=32 / K=64) |
| --- | --- | --- | --- | --- | --- |
| N=300 | util=0.85 cap=2 prefix=false | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 1.05x / 1.02x / 1.00x | 1.05x / 1.02x / 1.01x |
| N=300 | util=0.85 cap=2 prefix=true | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 0.96x / 0.96x / 0.99x | 0.98x / 0.98x / 1.00x |
| N=300 | util=0.92 cap=4 prefix=false | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 1.09x / 1.02x / 1.01x | 1.08x / 1.01x / 1.00x |
| N=300 | util=0.92 cap=4 prefix=true | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 0.86x / 0.90x / 0.93x | 0.87x / 0.91x / 0.94x |
| N=300 | util=0.92 cap=12 prefix=false | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 1.11x / 1.03x / 1.01x | 1.11x / 1.03x / 1.00x |
| N=300 | util=0.92 cap=12 prefix=true | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 1.17x / 1.12x / 1.13x | 1.15x / 1.11x / 1.13x |
| N=300 | util=1.05 cap=4 prefix=false | 6.316% / 6.233% / 6.329% / 6.495% | 0.000% / 6.233% / 6.308% / 6.026% | 0.97x / 0.99x / 1.00x | 0.95x / 0.98x / 1.01x |
| N=300 | util=1.05 cap=4 prefix=true | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 0.90x / 0.90x / 0.90x | 0.88x / 0.90x / 0.90x |
| N=500 | util=0.85 cap=2 prefix=false | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 1.06x / 1.03x / 1.02x | 1.06x / 1.03x / 1.03x |
| N=500 | util=0.85 cap=2 prefix=true | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 0.94x / 0.95x / 0.97x | 0.95x / 0.96x / 0.98x |
| N=500 | util=0.92 cap=4 prefix=false | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 1.08x / 1.02x / 1.03x | 1.09x / 1.02x / 1.03x |
| N=500 | util=0.92 cap=4 prefix=true | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 0.85x / 0.87x / 0.91x | 0.86x / 0.88x / 0.92x |
| N=500 | util=0.92 cap=12 prefix=false | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 1.06x / 1.05x / 1.02x | 1.07x / 1.04x / 1.02x |
| N=500 | util=0.92 cap=12 prefix=true | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 1.17x / 1.12x / 1.10x | 1.16x / 1.12x / 1.10x |
| N=500 | util=1.05 cap=4 prefix=false | 6.296% / 5.836% / 6.126% / 6.092% | 0.000% / 5.836% / 6.126% / 6.079% | 0.96x / 0.99x / 0.99x | 0.94x / 0.98x / 0.99x |
| N=500 | util=1.05 cap=4 prefix=true | 0.000% / 0.000% / 0.000% / 0.000% | 0.000% / 0.000% / 0.000% / 0.000% | 0.88x / 0.89x / 0.90x | 0.88x / 0.89x / 0.89x |

Across all sustainable loads tested (`util=0.85` and `util=0.92`), 429 and false-429 rates are `0.000%`. Overload is exercised only with `prefix=false` (`util=1.05, cap=4`): `prefix=true` at nominal `util=1.05` is not actually overloaded (`0.000%` 429s at every K including `full`) because the sim sizes arrival rate for a 50% prefix hit rate while the measured hit rate is ~98%. Under `prefix=false` overload, the total rejection rate matches `full (K=0)` (~`5.8–6.5%` vs `6.3%`), but nearly all K-only rejections are false 429s (another pod elsewhere in the fleet had room at that moment, so the stage rejects different requests than full scoring would; full scoring has none by construction). In production, `aibrix_routing_inflight_rejections_total{scope="candidates"|"fleet"}` records which set the cap was checked on (`fleet` before the default flips or when `N <= K`, `candidates` once the stage narrows the pool; the gateway does not read the rest of the fleet to separate true vs false 429s at runtime), so comparing the overall inflight 429 rate before (`scope="fleet"`) and after (`scope="candidates"`) flipping the default validates this prediction on live traffic.

### Pod blacklist × K-only evaluation (policy-independent blacklist, true remaining-capacity sizing, prefix=true, util=0.85, delay=1s, 3 seeds; 15s at N<=500, 10s at N=2000)

Models the cap-aware scout blacklist filter plus post-selection `excludeBlacklistedCandidates` and `O(N)` `ReselectCandidates` (#3543), including `BL=48%` just below the 50% exclusion cap knee. Each `(scenario, seed)` uses a single policy-independent blacklist mask (for correlated hot holders, derived from one shared `full (K=0)` warmup) and sizes the post-blacklist arrival rate from the exact `Σ capacity` of the remaining healthy pods so every policy sees the same remaining capacity. Complete output of `TestCandidateSelectionBlacklist`:

| Scenario | Policy | Mean Lat (med / mean) | P99 Lat (med / mean) | Prefix Hit% | O(N) Reselect% | Fallback% |
| --- | --- | --- | --- | --- | --- | --- |
| N=300 BL=15% (uniform) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 98.5% | 0.00% | 0.00% |
| N=300 BL=15% (uniform) | K=16 | 1.08x / 1.08x | 1.06x / 1.06x | 98.0% | 0.00% | 0.00% |
| N=300 BL=15% (uniform) | K=32 | 1.16x / 1.15x | 1.14x / 1.15x | 98.1% | 0.00% | 0.00% |
| N=300 BL=15% (uniform) | K=64 | 1.14x / 1.12x | 1.12x / 1.11x | 98.5% | 0.00% | 0.00% |
| N=300 BL=40% (uniform) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 97.0% | 0.00% | 0.00% |
| N=300 BL=40% (uniform) | K=16 | 1.01x / 1.03x | 0.95x / 0.98x | 96.9% | 0.48% | 0.00% |
| N=300 BL=40% (uniform) | K=32 | 1.09x / 1.07x | 1.04x / 1.03x | 97.2% | 0.03% | 0.00% |
| N=300 BL=40% (uniform) | K=64 | 1.10x / 1.11x | 1.07x / 1.08x | 97.3% | 0.00% | 0.00% |
| N=300 BL=48% (uniform) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 96.4% | 0.00% | 0.00% |
| N=300 BL=48% (uniform) | K=16 | 1.03x / 1.05x | 1.00x / 1.02x | 96.3% | 9.14% | 0.00% |
| N=300 BL=48% (uniform) | K=32 | 1.11x / 1.03x | 1.08x / 1.01x | 96.6% | 10.88% | 0.00% |
| N=300 BL=48% (uniform) | K=64 | 1.10x / 1.09x | 1.08x / 1.08x | 96.8% | 0.00% | 0.00% |
| N=300 BL=40% (correlated hot holders) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 97.2% | 0.00% | 0.00% |
| N=300 BL=40% (correlated hot holders) | K=16 | 1.08x / 1.09x | 1.01x / 1.04x | 96.2% | 0.88% | 0.00% |
| N=300 BL=40% (correlated hot holders) | K=32 | 1.13x / 1.10x | 1.11x / 1.07x | 96.6% | 0.03% | 0.00% |
| N=300 BL=40% (correlated hot holders) | K=64 | 1.11x / 1.15x | 1.10x / 1.13x | 96.7% | 0.00% | 0.00% |
| N=300 BL=48% (correlated hot holders) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 96.6% | 0.00% | 0.00% |
| N=300 BL=48% (correlated hot holders) | K=16 | 1.09x / 1.10x | 1.10x / 1.11x | 95.4% | 18.62% | 0.00% |
| N=300 BL=48% (correlated hot holders) | K=32 | 1.10x / 1.10x | 1.10x / 1.09x | 95.7% | 17.89% | 0.00% |
| N=300 BL=48% (correlated hot holders) | K=64 | 1.20x / 1.16x | 1.22x / 1.18x | 95.9% | 0.00% | 0.00% |
| N=500 BL=15% (uniform) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 99.5% | 0.00% | 0.00% |
| N=500 BL=15% (uniform) | K=16 | 1.07x / 1.08x | 1.03x / 1.05x | 98.8% | 0.00% | 0.00% |
| N=500 BL=15% (uniform) | K=32 | 0.98x / 0.97x | 0.95x / 0.94x | 99.1% | 0.00% | 0.00% |
| N=500 BL=15% (uniform) | K=64 | 1.06x / 1.08x | 1.07x / 1.08x | 99.2% | 0.00% | 0.00% |
| N=500 BL=40% (uniform) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 99.0% | 0.00% | 0.00% |
| N=500 BL=40% (uniform) | K=16 | 1.02x / 1.03x | 0.98x / 0.98x | 98.3% | 0.40% | 0.00% |
| N=500 BL=40% (uniform) | K=32 | 0.90x / 0.99x | 0.86x / 0.95x | 98.6% | 0.06% | 0.00% |
| N=500 BL=40% (uniform) | K=64 | 1.17x / 1.18x | 1.18x / 1.18x | 98.7% | 0.00% | 0.00% |
| N=500 BL=48% (uniform) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 98.7% | 0.00% | 0.00% |
| N=500 BL=48% (uniform) | K=16 | 1.02x / 1.02x | 0.99x / 0.99x | 97.9% | 7.24% | 0.00% |
| N=500 BL=48% (uniform) | K=32 | 0.95x / 0.93x | 0.91x / 0.90x | 98.4% | 8.70% | 0.00% |
| N=500 BL=48% (uniform) | K=64 | 1.11x / 1.07x | 1.09x / 1.06x | 98.5% | 0.00% | 0.00% |
| N=500 BL=40% (correlated hot holders) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 98.8% | 0.00% | 0.00% |
| N=500 BL=40% (correlated hot holders) | K=16 | 1.17x / 1.13x | 1.15x / 1.13x | 97.7% | 1.17% | 0.00% |
| N=500 BL=40% (correlated hot holders) | K=32 | 1.03x / 1.06x | 1.02x / 1.07x | 98.0% | 0.12% | 0.00% |
| N=500 BL=40% (correlated hot holders) | K=64 | 1.16x / 1.10x | 1.15x / 1.10x | 98.1% | 0.00% | 0.00% |
| N=500 BL=48% (correlated hot holders) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 98.5% | 0.00% | 0.00% |
| N=500 BL=48% (correlated hot holders) | K=16 | 1.07x / 1.07x | 1.06x / 1.06x | 97.1% | 20.16% | 0.00% |
| N=500 BL=48% (correlated hot holders) | K=32 | 0.95x / 1.02x | 0.96x / 1.01x | 97.5% | 22.37% | 0.00% |
| N=500 BL=48% (correlated hot holders) | K=64 | 1.11x / 1.08x | 1.10x / 1.07x | 97.5% | 0.00% | 0.00% |
| N=2000 BL=40% (correlated hot holders) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 99.2% | 0.00% | 0.00% |
| N=2000 BL=40% (correlated hot holders) | K=16 | 1.20x / 1.16x | 1.19x / 1.15x | 98.9% | 1.61% | 0.00% |
| N=2000 BL=40% (correlated hot holders) | K=32 | 1.10x / 1.08x | 1.09x / 1.07x | 99.2% | 0.33% | 0.00% |
| N=2000 BL=40% (correlated hot holders) | K=64 | 0.97x / 0.96x | 0.96x / 0.94x | 99.5% | 0.01% | 0.00% |
| N=2000 BL=40% (correlated hot holders) | K=32 (random-only 4K cap) | 1.10x / 1.09x | 1.11x / 1.08x | 99.2% | 0.40% | 0.00% |
| N=2000 BL=40% (correlated hot holders) | K=64 (random-only 4K cap) | 0.97x / 0.97x | 0.96x / 0.94x | 99.5% | 0.01% | 0.00% |
| N=2000 BL=48% (correlated hot holders) | full (K=0) | 1.00x / 1.00x | 1.00x / 1.00x | 99.0% | 0.00% | 0.00% |
| N=2000 BL=48% (correlated hot holders) | K=16 | 1.15x / 1.17x | 1.13x / 1.16x | 98.6% | 22.01% | 0.00% |
| N=2000 BL=48% (correlated hot holders) | K=32 | 1.12x / 1.09x | 1.15x / 1.09x | 99.0% | 24.98% | 0.00% |
| N=2000 BL=48% (correlated hot holders) | K=64 | 0.99x / 0.98x | 0.98x / 0.96x | 99.3% | 19.81% | 0.00% |
| N=2000 BL=48% (correlated hot holders) | K=32 (random-only 4K cap) | 1.14x / 1.11x | 1.16x / 1.11x | 99.0% | 23.12% | 0.00% |
| N=2000 BL=48% (correlated hot holders) | K=64 (random-only 4K cap) | 0.99x / 0.97x | 0.99x / 0.96x | 99.3% | 18.97% | 0.00% |

Key observations from the blacklist table:
- **Structural `0.00%` reselect for `K=64` at `N <= 512`:** when `8K >= N`, the scout is the whole fleet, so `filterBlacklistedScout` sees the exact fleet exclusion fraction. Whenever the fleet is below the 50% cap, all `K/2` ranked slots come from non-excluded pods, so at most `K/2 <= 50%` of the `K` candidates can be excluded; `ExclusionOverCap` (strictly `> 50%`) is therefore structurally unreachable for `N <= 512` below the cap.
- **Cap-knee behaviour (`BL=48%`):** when `8K < N` (random scout active), binomial sampling around `48%` pushes the scout over `50%` on a significant fraction of requests, so the `O(N)` `ReselectCandidates` path fires on `7–11%` (uniform) and `18–25%` (correlated) of requests.
- **Shipped whole-scout cap vs random-only `4K` cap (`N=2000`, 3 seeds):** with policy-independent blacklists and true remaining-capacity sizing, the two behave identically at `BL=40%` (`K=64`: `0.97x` med / `0.01%` reselect for both; `K=32`: `1.10x` med / `0.33%` vs `0.40%`) and differ by ~`0.8–1.9%` in reselect rate at `BL=48%` (`K=64`: `19.81%` vs `18.97%`; `K=32`: `24.98%` vs `23.12%`).

### Mid-fleet 10-seed check & random-scout comparison (`N=300` and `N=500`, `prefix=true`, `delay=1s`)

On 3-seed `prefix=true` runs, differences under roughly `0.1x` are within seed noise (for example, `TestCandidateSelectionKOnly` and the 3-seed sweep above flip the `K=32` vs `K=64` ordering at `N=300, prefix=true, delay=1s` purely due to RNG consumption differences). `TestCandidateSelectionMidFleet10Seed` runs 10 seeds (`1..10`) with policy-independent blacklists and true remaining-capacity sizing, reporting median alongside mean ± population standard deviation:

| Scenario (`prefix=true, delay=1s`) | Policy | Mean Lat (med / mean ± std) | P99 Lat (med / mean ± std) | Prefix Hit% | O(N) Reselect% |
| --- | --- | --- | --- | --- | --- |
| N=300 util=0.85 no BL | K=32 (shipped) | 1.048x / 1.058x ± 0.105 | 1.016x / 1.042x ± 0.122 | 98.7% | 0.00% |
| N=300 util=0.85 no BL | K=64 (shipped, 8K>=N whole-fleet) | 1.147x / 1.146x ± 0.056 | 1.140x / 1.143x ± 0.074 | 98.7% | 0.00% |
| N=300 util=0.85 no BL | K=64 (random 4K scout) | 1.124x / 1.116x ± 0.049 | 1.121x / 1.110x ± 0.061 | 98.8% | 0.00% |
| N=300 util=0.85 no BL | K=64 (random min(4K,N/2) scout) | 1.122x / 1.137x ± 0.061 | 1.124x / 1.130x ± 0.067 | 98.8% | 0.00% |
| N=300 util=0.92 no BL | K=32 (shipped) | 1.099x / 1.095x ± 0.089 | 1.067x / 1.075x ± 0.104 | 98.8% | 0.00% |
| N=300 util=0.92 no BL | K=64 (shipped, 8K>=N whole-fleet) | 1.107x / 1.100x ± 0.060 | 1.098x / 1.091x ± 0.068 | 98.9% | 0.00% |
| N=300 util=0.92 no BL | K=64 (random 4K scout) | 1.101x / 1.108x ± 0.060 | 1.105x / 1.098x ± 0.059 | 98.9% | 0.00% |
| N=300 util=0.92 no BL | K=64 (random min(4K,N/2) scout) | 1.129x / 1.126x ± 0.036 | 1.124x / 1.118x ± 0.046 | 98.9% | 0.00% |
| N=300 util=0.85 BL=40% corr | K=32 (shipped) | 1.075x / 1.072x ± 0.107 | 1.046x / 1.061x ± 0.112 | 96.6% | 0.03% |
| N=300 util=0.85 BL=40% corr | K=64 (shipped, 8K>=N whole-fleet) | 1.168x / 1.169x ± 0.041 | 1.174x / 1.169x ± 0.059 | 96.5% | 0.00% |
| N=300 util=0.85 BL=40% corr | K=64 (random 4K scout) | 1.170x / 1.156x ± 0.062 | 1.183x / 1.163x ± 0.075 | 96.6% | 0.00% |
| N=300 util=0.85 BL=40% corr | K=64 (random min(4K,N/2) scout) | 1.184x / 1.155x ± 0.069 | 1.176x / 1.151x ± 0.081 | 96.6% | 0.01% |
| N=500 util=0.85 no BL | K=32 (shipped) | 1.020x / 1.058x ± 0.088 | 0.993x / 1.032x ± 0.099 | 99.3% | 0.00% |
| N=500 util=0.85 no BL | K=64 (shipped, 8K>=N whole-fleet) | 1.118x / 1.096x ± 0.054 | 1.093x / 1.078x ± 0.057 | 99.3% | 0.00% |
| N=500 util=0.85 no BL | K=64 (random 4K scout) | 1.119x / 1.088x ± 0.087 | 1.099x / 1.072x ± 0.093 | 99.4% | 0.00% |
| N=500 util=0.85 no BL | K=64 (random min(4K,N/2) scout) | 1.072x / 1.067x ± 0.061 | 1.075x / 1.058x ± 0.069 | 99.4% | 0.00% |
| N=500 util=0.92 no BL | K=32 (shipped) | 1.120x / 1.107x ± 0.092 | 1.116x / 1.097x ± 0.100 | 99.4% | 0.00% |
| N=500 util=0.92 no BL | K=64 (shipped, 8K>=N whole-fleet) | 1.085x / 1.068x ± 0.069 | 1.080x / 1.058x ± 0.076 | 99.4% | 0.00% |
| N=500 util=0.92 no BL | K=64 (random 4K scout) | 1.136x / 1.112x ± 0.082 | 1.132x / 1.111x ± 0.091 | 99.4% | 0.00% |
| N=500 util=0.92 no BL | K=64 (random min(4K,N/2) scout) | 1.130x / 1.118x ± 0.047 | 1.130x / 1.110x ± 0.051 | 99.4% | 0.00% |
| N=500 util=0.85 BL=40% corr | K=32 (shipped) | 1.031x / 1.044x ± 0.088 | 1.023x / 1.040x ± 0.101 | 98.0% | 0.12% |
| N=500 util=0.85 BL=40% corr | K=64 (shipped, 8K>=N whole-fleet) | 1.133x / 1.101x ± 0.099 | 1.135x / 1.098x ± 0.103 | 98.1% | 0.00% |
| N=500 util=0.85 BL=40% corr | K=64 (random 4K scout) | 1.099x / 1.094x ± 0.087 | 1.110x / 1.088x ± 0.091 | 98.1% | 0.00% |
| N=500 util=0.85 BL=40% corr | K=64 (random min(4K,N/2) scout) | 1.079x / 1.070x ± 0.105 | 1.071x / 1.066x ± 0.111 | 98.1% | 0.00% |

Takeaway from the 10-seed check: at `N=300–500` with `prefix=true` and `delay=1s`, `K=64` is no better than `K=32` in this model (and worse at `util=0.85` both with and without a blacklist, while slightly better at `N=500 util=0.92`). Removing the `8K >= N` whole-fleet scout shortcut (keeping a random `4K` or `min(4K, N/2)` scout) does **not** close the `K=32` vs `K=64` gap (`1.147x → 1.124x` med at `N=300 util=0.85`; `1.168x → 1.170x` at `N=300 BL=40%`; `1.085x → 1.136x` at `N=500 util=0.92`). The cause of the `K=32` vs `K=64` difference at `N=300–500` with prefix caching on remains unexplained by these experiments, and keeping a random `4K` scout is not a demonstrated fix.

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
