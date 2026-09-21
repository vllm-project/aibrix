# Load Balance Router

`load-balance` routes each request to the replica with the lowest **effective load**: the work
already committed to it, divided by how fast it has been observed to drain work, inflated as its
KV cache fills. It needs no hardware labels. A mixed B40 / A100 / H20 pool teaches the gateway
its relative speeds from observed output-token throughput, and faster replicas receive
proportionally more traffic.

Source: [load_balance.go](load_balance.go). Environment variables:
[ENV_VARS.md](../ENV_VARS.md#load-balance-router-algorithmsload_balancego).

`load-balance` is also blended in behind other strategies by default (see
`AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT` and [multi_router_readme.md](multi_router_readme.md)),
so its `ScoreAll` runs on most requests, not only when it is selected explicitly.

## Score

```
                   running + λ · queued
score  =  ─────────────────────────────────────  ×  (1 + α · (1 − kvFree)²)
           EWMA(output tokens completed / sec)

kvFree < kvCritical   ⇒   score = +Inf
```

The lowest score wins. The three parts answer three separate questions, and each is used for one
purpose only:

| Part | Question | Source |
|---|---|---|
| `EWMA(output tokens/sec)` | How fast does this replica drain work? (**capacity**) | `RealtimeOutputTokenRateEWMA`, derived in the cache from completed output tokens |
| `running + λ·queued` | How much work is already committed to it? (**load**) | `GetPodsRunningRequests` (live cross-gateway count); `num_requests_waiting` when `λ > 0` |
| `1 + α(1 − kvFree)²` | How close is it to memory pressure? (**headroom**) | `kv_cache_usage_perc` (`KVCacheUsagePerc`) |

Capacity and load are kept apart on purpose. A saturated GPU has high token throughput *because*
it is busy; if throughput were used as "load", the scheduler would send it even more traffic.
Here throughput only sets the denominator, and load comes from the committed work.

### Defaults

| Symbol | Meaning | Default | Variable |
|---|---|---|---|
| `λ` | Weight of engine-queued requests | `0` | `AIBRIX_LOAD_BALANCE_QUEUED_WEIGHT` |
| `α` | KV-pressure penalty strength | `2.0` | `AIBRIX_LOAD_BALANCE_KV_PRESSURE_ALPHA` |
| `kvCritical` | Free-KV fraction below which a pod is excluded | `0.10` | `AIBRIX_LOAD_BALANCE_KV_CRITICAL_FREE` |

`λ` defaults to `0` because the gateway's running count already includes requests that are
queued inside the engine; adding `num_requests_waiting` on top would count them twice. Values
`<= 0` for these variables fall back to the default (the loader rejects non-positive numbers).

### Worked example

| Replica | Running | Tokens/s | KV free | Calculation | Score |
|---|---|---|---|---|---|
| B40 | 30 | 6000 | 50% | 30/6000 × (1 + 2·0.50²) | **0.0075** |
| A100 | 20 | 4000 | 30% | 20/4000 × (1 + 2·0.70²) | 0.0099 |
| H20 | 25 | 3000 | 60% | 25/3000 × (1 + 2·0.40²) | 0.0110 |

The B40 wins despite carrying the most requests, because it drains them fastest. This example is
asserted in `TestLoadBalanceScoreAll_HeterogeneousExample`.

## Where the capacity signal comes from

```
gateway: DoneRequestTrace(outputTokens) ──► Pod.completedOutputTokens   (monotonic counter)
                                                    │  every metrics refresh (default 1s)
                                                    ▼
                          calculateRate1m: tokens/sec over ~1 minute window
                                                    │
                                                    ▼
                          EWMA, time constant 10s ──► RealtimeOutputTokenRateEWMA (pod-scoped)
```

- The counter is **gateway-tracked**, like `completedRequests`, so it works for every engine
  (vLLM, SGLang, TRT-LLM, xLLM) without depending on engine metric names. Code:
  `donePodStats` in `pkg/cache/cache_trace.go`, `updateRealtimeOutputTokenRateEWMA` in
  `pkg/cache/cache_metrics.go`.
- Output tokens are the request's completion-token usage. A request that finishes through the
  count-only path (no usage available) adds one completion and **zero** tokens.
- The counter survives a brief pod delete/re-add (informer flap) the same way `completedRequests`
  does.
- A window with no completed tokens carries no information about speed (the replica was idle,
  not slow), so the previous estimate is **kept**, not decayed toward zero. Decaying would make a
  briefly idle replica look weak and starve it of the traffic that would re-measure it.

## Behavior in edge cases

| Situation | Behavior |
|---|---|
| Pod has no token-rate estimate yet (new pod, or no usage reported) | Scored at the **mean** estimate of the pods that have one, so it competes as an average replica. A fixed fallback such as `1.0` would be off by the tokens/sec scale (thousands) and lock the pod out of the traffic it needs to be measured. |
| No pod has an estimate | Every pod gets capacity `1.0`; the score reduces to `running × KV penalty`. |
| Engine reports no KV usage, or `NaN` | Treated as fully free: no penalty and no guardrail. |
| `kvFree < kvCritical` | Score `+Inf`; the pod is skipped until it recovers. Exactly at the threshold is still usable. |
| **Every** pod is below `kvCritical` | The request is **not** failed. Routing falls back to the tie-break over all pods, which picks the one with the least KV usage. |
| Pod is idle (`running = 0`) | Score `0` regardless of capacity, so idle pods are preferred. |
| Several pods share the lowest score | Ties are broken by least combined GPU+CPU KV-cache usage (`least-kv-cache` scorer), falling back to random. |

KV usage is clamped to `[0, 1]` before use, so an engine that reports a value above 1 is treated
as full rather than producing a negative free fraction.

## Interaction with the rest of the gateway

- **Load-imbalance gate.** The gateway applies `ApplyLoadImbalanceGate` once, ahead of whichever
  strategy routes the request, so `Route` receives an already-narrowed pod list. If that narrowed
  set is entirely KV-critical, the all-critical fallback above applies.
- **Multi-strategy blending.** `ScoreAll` returns `+Inf` for excluded pods. The aggregator ignores
  non-finite scores when normalizing, so such a pod gets `0` from `load-balance` (the worst
  score). In a blend this is a strong penalty, not a hard exclusion: another strategy with enough
  weight (for example a prefix-cache hit) can still select the pod. The hard guarantee holds when
  `load-balance` routes alone.
- **PD disaggregation.** The PD router's decode path keeps its own request drain-rate scoring
  (`RealtimeRunningRequestsDrainRate1m`) and is unchanged.

## Known limitations

- **Multiple gateway replicas.** The token counter is local to each gateway, while the running
  count is the cross-gateway total. Each gateway therefore measures roughly `1/N` of a pod's
  output. Relative ranking between pods is preserved as long as the gateways spread traffic
  similarly, but absolute score values are not comparable across deployments with different
  gateway counts.
- **Estimate lag.** The estimate follows a ~1 minute window smoothed with a 10 second time
  constant, so a sudden change in a replica's speed (for example a workload shifting from
  decode-heavy to prefill-heavy) shows up within tens of seconds, not immediately.
- **No prefill/decode split.** Capacity is a single output-token rate. Hardware whose relative
  performance differs between prompt-heavy and decode-heavy workloads is not modeled; a natural
  extension is `capacity = α·prefill_tokens/s + β·decode_tokens/s`.
- **Deliberately not used:** GPU utilization / SM-active. They are useful for observability but
  make a generic scheduler unstable; the score uses only drain rate, committed work, and KV
  pressure.

## Debugging

At `-v=4` the gateway logs `load_balance_score` once per pod (`running_requests`,
`capacity`, `kv_free`, and the resulting `score`) and `load_balance_selected` (with the
winning `score`). `+Inf` scores are logged as strings so klog's JSON formatter does not
fail. The cache logs `Updating output token rate metric` with the completed token count,
the 1-minute rate, and the EWMA for each pod. The estimate is stored on the pod as
`realtime_output_token_rate_ewma`; it is not currently exported as a Prometheus series.
