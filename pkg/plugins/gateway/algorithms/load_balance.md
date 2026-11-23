# Load Balance Router

`load-balance` routes each request to the replica with the lowest **effective load**: the work
already committed to it, divided by how fast it has been observed to drain work, inflated as its
KV cache fills. It needs no hardware labels. A mixed B40 / A100 / H20 pool teaches the gateway
its relative speeds from how fast each replica's requests produce output tokens, and faster
replicas receive more traffic.

Source: [load_balance.go](load_balance.go). Environment variables:
[ENV_VARS.md](../ENV_VARS.md#load-balance-router-algorithmsload_balancego).

`load-balance` is also blended in behind other strategies by default (see
`AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT` and [multi_router_readme.md](multi_router_readme.md)),
so its `ScoreAll` runs on most requests, not only when it is selected explicitly.

Related: periodic load-outlier **exclusion** (blacklist until the next sweep) lives in
[`blacklist/pod_load_blacklist.go`](../blacklist/pod_load_blacklist.go) and is documented in
[blacklist/README.md](../blacklist/README.md#4-load-outlier-flag-and-flaky-re-add). That path is not
the imbalance gate described below, and the two are independent: the gate runs with the pod
blacklist on or off, and when the blacklist has excluded pods it narrows the pods that are left.

## Score

```
                   running + λ · queued
score  =  ─────────────────────────────────────  ×  (1 + α · (1 − kvFree)²)
        EWMA(output tokens per request-second)

kvFree < kvCritical   ⇒   score = +Inf
```

The lowest score wins. The three parts answer three separate questions, and each is used for one
purpose only:

| Part | Question | Source |
|---|---|---|
| `EWMA(output tokens per request-second)` | How fast does this replica drain work? (**capacity**) | `RealtimeOutputTokensPerRequestSecondEWMA`, derived in the cache from completed output tokens and the time those requests spent on the pod |
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
| `kvCritical` | Free-KV fraction below which a pod scores `+Inf` from `load-balance` (excluded when `load-balance` routes alone; only strongly penalized in a blend — see "Interaction with the rest of the gateway" below) | `0.10` | `AIBRIX_LOAD_BALANCE_KV_CRITICAL_FREE` |

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
gateway: DoneRequestTrace(outputTokens) ──► Pod.completedOutputTokens      (monotonic counters)
                                            Pod.completedOutputBusyNanos   time on the pod
                                            Pod.completedOutputRequests    requests counted
                                                    │  every metrics refresh (default 1s)
                                                    ▼
             outputRateDeltas1m: tokens and time on the pod over the same ~1 minute window
                                                    │  only if enough requests completed in it
                                                    ▼
             tokens / time, EWMA with time constant 10s
                                ──► RealtimeOutputTokensPerRequestSecondEWMA (pod-scoped)
```

- **What it measures.** Output tokens divided by the time those requests spent on the pod, from
  routing to the end of the response. A replica that is sent twice the traffic completes twice
  the tokens in twice the request time, so the value does not move. A faster replica finishes
  each request sooner and reads higher. When requests queue on a saturated replica their time on
  the pod grows and the value falls.
- **Why not the pod's total tokens per second.** That is what `load-balance` used before
  (`RealtimeOutputTokenRateEWMA`, still computed). Below saturation it is the traffic the replica
  receives, not its speed: a replica that got more requests looked faster and kept getting more,
  and two identical replicas could stay split 73:27 (#3450).
  `AIBRIX_LOAD_BALANCE_CAPACITY_SIGNAL=total` selects it again.
- The counters are **gateway-tracked**, like `completedRequests`, so this works for every engine
  (vLLM, SGLang, TRT-LLM, xLLM) without depending on engine metric names. Code: `donePodStats`
  in `pkg/cache/cache_trace.go`, `updateRealtimeOutputTokensPerRequestSecondEWMA` in
  `pkg/cache/cache_metrics.go`.
- Output tokens are the request's completion-token usage. A request that finishes without usage
  (aborted, failed, or streamed without `stream_options.include_usage`) adds neither tokens nor
  time. A replica whose slowest requests are abandoned therefore reads faster than it is;
  `gateway_pod_requests_without_output_tokens_fraction` shows how many requests that is.
- A window updates the estimate only if at least `AIBRIX_OUTPUT_RATE_MIN_SAMPLES` (default 8)
  requests with output tokens completed in it. Otherwise the previous estimate is **kept**, not
  decayed toward zero: the replica was idle, not slow.
- **An estimate older than 3 minutes is not used.** A replica that has been idle, or below that
  request rate, for that long is scored like one without an estimate (see the table below).
  `gateway_pod_output_rate_age_seconds` shows each estimate's age.
- The time is the whole request, so it includes queueing and prompt processing. Replicas on the
  same hardware that serve different request mixes read differently: 6-9% in the runs on #3497
  with a 0.5B model, and more where the time to the first token is a larger part of a request.

## Behavior in edge cases

| Situation | Behavior |
|---|---|
| Pod has no usable estimate (new pod, no usage reported, or an estimate last updated more than 3 minutes ago) | Scored at the **mean** estimate of the pods that have one, so it competes as an average replica. A fixed fallback such as `1.0` would be off by the scale of the estimates (tens to hundreds) and lock the pod out of the traffic it needs to be measured. |
| No pod has an estimate | Every pod gets capacity `1.0`; the score reduces to `running × KV penalty`. |
| Any candidate became routable less than `AIBRIX_LOAD_BALANCE_CAPACITY_WARMUP` (default `60s`) ago | Every candidate gets capacity `1.0`, as when no pod has an estimate. A fresh engine's first requests are slower than its steady state, so its first estimates read low; with the total-rate signal, a pod with a shorter history also looked faster (#3450). This lasts for a whole rolling update or scale-up plus the window; a mixed-hardware pool routes without the capacity term for that long. `0` disables it. |
| Engine reports no fresh KV usage, or `NaN` | Treated as fully free: no penalty and no guardrail. |
| `kvFree < kvCritical` | Score `+Inf` from `load-balance`. When `load-balance` routes alone this skips the pod entirely until it recovers. When `load-balance` is blended with other strategies (the default — see below), it is only a strong penalty, not a guaranteed exclusion. |
| **Every** pod is below `kvCritical` | The request is **not** failed. Routing falls back to the tie-break over all pods, which picks the one with the least KV usage. This fallback is specific to `load-balance` routing alone; see below for the blended case. |
| Pod is idle (`running = 0`) | Score `0` regardless of capacity, so idle pods are preferred. |
| Several pods share the lowest score | Ties are broken by least combined GPU+CPU KV-cache usage (`least-kv-cache` scorer), falling back to random. |

The primary KV penalty/guardrail and the tie-breaker share the
[KV freshness check](../ENV_VARS.md#kv-metric-freshness-algorithmsrouteutilkv_freshnessgo).
Stale primary KV usage receives a `+Inf` score; when load-balance routes alone it
prefers a fresh peer, with a fallback if all candidates are unscorable. In a blend,
it is a penalty rather than a hard exclusion. Never-reported KV remains neutral.

KV usage is clamped to `[0, 1]` before use, so an engine that reports a value above 1 is treated
as full rather than producing a negative free fraction.

## Interaction with the rest of the gateway

- **Load-imbalance gate.** The gateway applies `ApplyLoadImbalanceGate` once, ahead of whichever
  strategy routes the request, so `Route` receives an already-narrowed pod list. If that narrowed
  set is entirely KV-critical, the all-critical fallback above applies. **TODO / known
  limitation:** the gate compares raw running-request count, with no notion of a pod's capacity —
  a replica that's simply faster than its peers is expected to carry more concurrent requests
  without being more loaded (the same capacity signal `load-balance`'s own score uses above), but
  this gate can still flag it as a hotspot and exclude it before `load-balance`'s own
  capacity-aware score ever gets to run. See `getTargetPodListOnLoadImbalance` in
  [load_balance.go](load_balance.go).
- **Multi-strategy blending: the guardrail is soft here, not hard.** `kvCritical` is a hard
  exclusion only when `load-balance` routes alone. In a blend, `ScoreAll` still returns `+Inf` for
  a critical pod, but `multiStrategyRouter.normalizeScoresArray` treats a non-finite score the
  same as "not scored" and assigns it `0` — the worst possible score from `load-balance`, not an
  exclusion from routing. If another blended strategy scores that same pod highly enough, it can
  still be selected.

  Concretely: `load-balance` is blended in behind other strategies by default (see
  `AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT`), so the blended path, not the standalone one, is
  what most requests actually take. With the default `prefix-cache:5,load-balance:4` blend, a
  KV-critical pod that has the best prefix-cache score still gets `5/9` of the combined weight
  versus `4/9` for a healthy pod that only wins on `load-balance`, so the critical pod wins the
  route. `kvCritical` should therefore be read as "how hard `load-balance` argues against routing
  to this pod," not as a safety limit that blocks it from being chosen — it does not, on its own,
  prevent a KV-critical pod from receiving more traffic. Something that must never route to a
  KV-critical pod needs a filter ahead of scoring (comparable to `ApplyLoadImbalanceGate`), which
  does not currently exist for KV pressure.
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
- **No prefill/decode split.** Capacity is a single output-token rate over the whole request. Hardware whose relative
  performance differs between prompt-heavy and decode-heavy workloads is not modeled; a natural
  extension is `capacity = α·prefill_tokens/s + β·decode_tokens/s`.
- **Deliberately not used:** GPU utilization / SM-active. They are useful for observability but
  make a generic scheduler unstable; the score uses only drain rate, committed work, and KV
  pressure.

## Debugging

At `-v=4` the gateway logs `load_balance_score` once per pod (`running_requests`,
`capacity`, `kv_free`, and the resulting `score`) and `load_balance_selected` (with the
winning `score`). `+Inf` scores are logged as strings so klog's JSON formatter does not
fail. The cache logs `Updating per-request output token rate metric` with the window's tokens,
time on the pod and request count, the 1-minute rate, and the EWMA for each pod. The estimate is
stored on the pod as `realtime_output_tokens_per_request_second_ewma` and exported as
`gateway_pod_output_tokens_per_request_second`, with its age as
`gateway_pod_output_rate_age_seconds`.
