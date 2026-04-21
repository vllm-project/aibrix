#2032 squashed and DCO fixed

## Summary
This PR adds **multi-strategy routing** to AIBrix Gateway using a **batch soft-scoring** pipeline. Multiple routing algorithms can be combined in a single request/config string with integer **weight coefficients**. Each strategy scores all ready pods in one batch (`ScoreAll`), the router normalizes scores to `[0,1]` based on polarity, aggregates by weight, and picks the top pod.

## Motivation
The existing router selects a pod using a single strategy at a time. In practice we often want to blend multiple signals (e.g. load + latency + cache affinity) without “hard filtering” pods early. Soft scoring keeps the full candidate set and makes routing decisions smoother.

## User-facing configuration
Multi-strategy is configured via the existing header/env value (comma-separated list of `strategy[:coefficient]`):

- Header: `routing-strategy: least-request:2,throughput:1`
- Env: `AIBRIX_ROUTING_ALGORITHM="least-request:2,throughput:1"`

Rules:
- the weight `coefficient` is **integer only** in `[0, 1000000]` (following [gateway-api](https://gateway-api.sigs.k8s.io/reference/spec/#backendref))
- omitted coefficient defaults to `1`
- coefficient `0` means the strategy is skipped
- If routign strategy: [a:2,b,c], then final weights are **[0.5, 0.25, 0.25]** after normalization

## Design overview (Batch Soft Scoring)
Pipeline:
1. **Parse config** → `[]RouterItem{Name,Coefficient}`
2. **Batch score** each strategy over the same `readyPodList`
3. **Normalize** each strategy’s raw scores to `[0,1]` using strategy polarity
4. **Weighted aggregation** using `coefficient / sum(coefficients)`
5. **Pick top-1** (deterministic tie-break)

### Handling missing metrics
If a pod cannot be scored for a strategy (`scored=false` or `NaN`), its normalized score for that strategy is **0.0** (worst score).

## Unit Test and E2E Test Added
Extensive unit tests and E2E tests have been added to ensure the reliability and correctness of the multi-strategy routing architecture:
- **Unit Tests (`router_test.go`)**:
  - Validated single strategies (both `HigherIsBetter` and `LowerIsBetter` polarities).
  - Validated multi-strategy weighted aggregation with varying coefficients.
  - Handled missing metrics (`scored=false` or `NaN`) gracefully, assigning a normalized score of `0.0` without discarding the pod.
  - Handled edge cases like `max == min` scores (assigning a normalized score of `1.0` to all scored pods).
- **E2E Tests (`routing_strategy_test.go`)**:
  - `ValidMultiStrategy_Partial_Weights`: Tested partial coefficient configs (e.g., `least-request,throughput:2`).
  - `ValidMultiStrategy_No_Weights`: Tested configs with omitted coefficients, defaulting to `1`.
  - `ExclusiveStrategy_FallbackToSelf_*`: Ensured exclusive strategies (like `pd` and `slo-*`) safely strip out unsupported strategies and fallback to their own legacy paths.
  - `InvalidStrategy_FallbackToRandom`: Verified that invalid or unregistered strategies gracefully degrade to `random` routing, preventing 500 errors.
- **Diagnostic Logging (`klog`)**: Added detailed, tabular `request_start` logging that breaks down the raw score, normalized score, and weighted contribution of every candidate pod for each strategy, making debugging transparent.

## Strategy Adaptations (ScoreAll Implementation)
To support the new batch soft-scoring pipeline, existing strategy implementations in `pkg/plugins/gateway/algorithms/*.go` were refactored to implement the `PodScorer` interface (specifically the `ScoreAll` method). 

The adaptation strategy varied based on the complexity of the algorithm:
- **Refactored `Route()` to reuse `ScoreAll()`**: 
  For most metrics-based strategies, the metric-fetching loop inside `Route()` was extracted into `ScoreAll()`. The original `Route()` method was then updated to simply call `ScoreAll()`, pick the best pod, and return. This ensures backwards compatibility for single-strategy paths while enabling multi-strategy batching. Affected files:
  - `least_request.go`
  - `least_util.go`
  - `least_gpu_cache.go`
  - `least_kv_cache.go`
  - `least_busy_time.go`
  - `throughput.go`

- **Newly generated `ScoreAll()` logic**: 
  For strategies that involve complex state or tree traversal (rather than just fetching a single Prometheus metric), a dedicated `ScoreAll()` method was implemented alongside the original `Route()` to extract a quantifiable float score representing the pod's suitability. Affected files:
  - `prefix_cache.go`: `ScoreAll` traverses the Radix Tree to calculate the `matchRatio` (matched tokens / total tokens) for every pod.
  - `prefix_cache_preble.go`: Similar to standard prefix cache, but computes scores based on the depth of the matched prefix cache nodes.

- **Unchanged (Exclusive / Random Strategies)**:
  Some strategies like `pd` (disaggregated prefill/decode), `slo-*`, and `random` do not currently implement `ScoreAll` and bypass the multi-strategy aggregator.

## Notes
- Single-strategy behavior is preserved: when the config resolves to a single strategy name, gateway routing follows the existing path.
- Multi-strategy pick tie-break is deterministic (stable w.r.t. original pod order), while some single-strategy routers may still use randomized tie-break.