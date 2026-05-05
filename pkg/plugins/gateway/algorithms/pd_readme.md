# PD Disaggregation Router

`pd_disaggregation.go` implements the **prefill-decode (PD) disaggregated routing algorithm** (`RouterPD = "pd"`). **This file (`pd_readme.md`) is the canonical documentation** for PD: request flow, scoring, engine behavior, metrics, and environment variables. PD-only settings are under **Environment Variables**; variables shared with the prefix-cache router (tokenizer, stddev factor, and related `AIBRIX_PREFIX_CACHE_*` knobs) are listed in **Inherited from Prefix Cache Router** in the same section.

In PD disaggregation, inference is split across two specialized pod roles:

- **Prefill pod** — processes the prompt (context), builds the KV-cache, then transfers it.
- **Decode pod** — generates tokens using the KV-cache transferred from the prefill pod.

The router is responsible for selecting one prefill pod and one decode pod per request, executing the prefill HTTP request synchronously (vLLM/TRT-LLM) or asynchronously (SGLang), and routing the decode request to the selected decode pod.

---

## High-Level Request Flow

```
Client Request
     │
     ▼
Route(ctx, readyPodList)
     │
     ├─► validateAndGetLLMEngine()
     │        Ensure all pods use the same engine (vllm / sglang / trtllm)
     │
     ├─► filterPrefillDecodePods()
     │        ┌─────────────────────────────────────────────────────┐
     │        │  collectAndBucketPods()                             │
     │        │    Phase 1: group by roleset (prefill / decode)     │
     │        │    Phase 2: apply prompt-length bucketing filter    │
     │        └─────────────────────────────────────────────────────┘
     │        │
     │        ├─► [Prompt-length bucketing ON]
     │        │       → combined pod path  (see Combined Pod section)
     │        │
     │        ├─► loadImbalanceSelectPrefillPod()   (fast path)
     │        ├─► loadImbalanceSelectDecodePod()    (fast path)
     │        ├─► scorePrefillPods()
     │        ├─► scoreDecodePods()
     │        └─► finalPDScore()  →  (prefillPod, decodePod)
     │
     ├─► [prefillPod != nil]
     │        pendingDecodeTracker.AddPendingDecode(requestID, decodePod)
     │        doPrefillRequest(ctx, prefillPod, engine)
     │              ├─ SGLang   → async goroutine (bootstrap handshake)
     │              ├─ vLLM     → sync, extract kv_transfer_params from response
     │              └─ TRT-LLM  → sync, extract disaggregated_params from response
     │
     └─► ctx.SetTargetPod(decodePod)
         return decodePod address
```

---

## Pod Classification

Pods are grouped using Kubernetes labels:

| Label | Values | Purpose |
|-------|--------|---------|
| `roleset-name` | any string | Groups prefill+decode pairs into a "roleset" |
| `role-name` | `prefill` / `decode` / other | Pod's role in PD disaggregation |
| `stormservice.orchestration.aibrix.ai/pod-group-index` | `"0"` or absent | Only pods with index `"0"` (or no label) host the HTTP server (multi-node TP) |
| `model.aibrix.ai/engine` | `vllm` / `sglang` / `trtllm` | LLM engine type |

Only rolesets with **both** at least one prefill pod and one decode pod are eligible. Incomplete rolesets are excluded.

---

## Pod Selection Flow

```
collectAndBucketPods()
        │
        ▼
┌───────────────────────────────┐
│  prefillPods / decodePods     │
│  (grouped by roleset)         │
└───────────────────────────────┘
        │
        ▼
loadImbalanceSelectPrefillPod()
  ┌─────────────────────────────────────────────────────────┐
  │  max(outstanding_prefill_reqs) - min > MIN_SPREAD?      │
  │  → YES: narrow to single least-loaded prefill pod       │
  │  → NO:  continue with all prefill pods                  │
  └─────────────────────────────────────────────────────────┘
        │
        ▼
loadImbalanceSelectDecodePod()  (3 ordered checks)
  ┌──────────────────────────────────────────────────────────────────┐
  │  Check 1 – Request count spread                                  │
  │    max(running_reqs+pending) - min >= DECODE_LOAD_SPREAD?        │
  │    → YES: return least-loaded decode pod                         │
  │                                                                  │
  │  Check 2 – Throughput spread                                     │
  │    max(throughput_tok/s) - min > THROUGHPUT_SPREAD?              │
  │    → YES: return lowest-throughput decode pod                    │
  │                                                                  │
  │  Check 3 – Drain rate score (soft path)                          │
  │    all pods have drain_rate > 0?                                 │
  │    score = running_reqs / drain_rate                             │
  │    max_score / min_score > DECODE_SCORE_RATIO?                   │
  │    → YES: return pod with lowest drain-rate score                │
  └──────────────────────────────────────────────────────────────────┘
        │
        ▼
scorePrefillPods()              (per roleset, best pod wins)
  ┌──────────────────────────────────────────────────────────────────┐
  │  Skip pods with req_count > mean + N * stddev                    │
  │                                                                  │
  │  Policy: prefix_cache (default)                                  │
  │    score = (100 - match_pct) * 0.1  +  req_cnt / max_req_cnt    │
  │                                                                  │
  │  Policy: least_request                                           │
  │    score = req_cnt                                               │
  └──────────────────────────────────────────────────────────────────┘
        │
        ▼
scoreDecodePods()  →  pd.DecodeScoreRun (per roleset best pod + MaxScore + policy metadata)
  ┌──────────────────────────────────────────────────────────────────┐
  │  Policy: load_balancing (default)                                │
  │    norm_reqs     = running_reqs / max_running_reqs               │
  │    norm_thru     = 1 - throughput / max_throughput               │
  │    norm_free_gpu = free_gpu_pct / max_free_gpu_pct               │
  │    score = (w_run*norm_reqs + w_thru*norm_thru) / norm_free_gpu   │
  │    (w_run, w_thru from AIBRIX_DECODE_LB_WEIGHT_*, default 1)      │
  │    NaN scores: skip; non-NaN Inf possible if norm_free_gpu = 0   │
  │                                                                  │
  │  Policy: least_request                                           │
  │    score = running_reqs (with pending)                           │
  │                                                                  │
  │  If primary policy yields NaN, one load_balancing retry per pod    │
  │                     lower is better                              │
  └──────────────────────────────────────────────────────────────────┘
        │
        ▼
finalPDScore()
  ┌──────────────────────────────────────────────────────────────────┐
  │  For each roleset with both prefill and decode scores:           │
  │    final = norm_prefill_score + norm_decode_score                │
  │  Pick roleset with minimum final score                           │
  │  → selectedPrefillPod, selectedDecodePod                         │
  └──────────────────────────────────────────────────────────────────┘
```

---

## Prefill Score Policies

Controlled by `AIBRIX_PREFILL_SCORE_POLICY`, or overridden per request via the model **`routingConfig`** in `model.aibrix.ai/config` (see [Config profile overrides](#config-profile-overrides-for-pd-score-policies)).

### `prefix_cache` (default)

Uses the shared `PrefixHashTable` to find how much of the request's token prefixes are already cached on each prefill pod.

```
tokens = tokenize(request.message)
matchedPods, hashes = prefixCacheIndexer.MatchPrefix(tokens, model, readyPodsMap)

score = (100 - match_percent) * 0.1  +  req_count / max_req_count
```

- Lower score = better (more cache hits, fewer outstanding requests).
- After routing, the prefix hashes are enqueued to `prefixUpdateCh` for asynchronous indexer update.
- Tokenizer is selected by `AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE` (same as the prefix-cache router): `tiktoken` or `character` (default).

### `least_request`

No prefix cache consultation. Score equals the raw running request count on the pod.

```
score = req_count
```

---

## Decode Score Policies

Implementation lives in `algorithms/pd/decode_scorer.go` (`package pd`). Policies are selected by **`pd.ResolveDecodePolicy`** from `AIBRIX_DECODE_SCORE_POLICY`. Extra names can be registered at init time with **`pd.RegisterDecodePolicy`**. Per-pass results use **`pd.DecodeScoreRun`** (map of roleset → best pod/score, `MaxScore` for normalization, optional `Err`, `FallbackUsed`, `Policy`).

Metrics are passed per pod via **`pd.DecodePodInput`** (running/throughput/free GPU and batch maxima). The router guarantees positive maxima from `loadImbalanceSelectDecodePod` as today.

Controlled by `AIBRIX_DECODE_SCORE_POLICY`, or overridden per request via **`routingConfig`** in the model config profile (see [Config profile overrides](#config-profile-overrides-for-pd-score-policies)).

Metrics are pulled from the cache per pod (same maps as `loadImbalanceSelectDecodePod`):
- `RealtimeNumRequestsRunning` + `PendingDecodeTracker` count
- `AvgGenerationThroughputToksPerS` (per model)
- `GPUCacheUsagePerc` (free = 100 − usage%)

### `load_balancing` (default)

Balances normalized queue depth, inverse throughput, and free GPU headroom:

```
norm_running_reqs  = running_reqs_with_pending / max_running_reqs
norm_throughput    = 1 - avg_gen_throughput_tok_s / max_throughput
norm_free_gpu      = free_gpu_percent / max_free_gpu_percent

decode_score = (w_run * norm_running_reqs + w_thru * norm_throughput) / norm_free_gpu
```

`w_run` and `w_thru` default to `1` (historical equal weighting). Lower score is better (fewer outstanding requests, higher throughput, more free GPU).

### `least_request`

No throughput or GPU terms. Score equals raw running decode request count (including pending decode), same units as `RealtimeNumRequestsRunning` + pending.

```
decode_score = running_reqs_with_pending
```

### Config profile overrides for PD score policies

When the gateway resolves a model config profile (`routingCtx.ConfigProfile` from `model.aibrix.ai/config` and the `config-profile` header), the **`routingConfig`** JSON may include:

| Field | Values | Effect |
|-------|--------|--------|
| `prefillScorePolicy` | `prefix_cache`, `least_request` | Overrides `AIBRIX_PREFILL_SCORE_POLICY` for that request |
| `decodeScorePolicy` | `load_balancing`, `least_request` (or any name registered via `pd.RegisterDecodePolicy`) | Overrides `AIBRIX_DECODE_SCORE_POLICY` for that request |

Example fragment inside a profile:

```json
"routingConfig": {
  "promptLenBucketMinLength": 0,
  "promptLenBucketMaxLength": 8192,
  "prefillScorePolicy": "prefix_cache",
  "decodeScorePolicy": "least_request"
}
```

Omitted fields keep the gateway defaults from environment variables.

---


## Engine-Specific Prefill Behaviour

### vLLM (`AIBRIX_KV_CONNECTOR_TYPE=shfs`, default)

```
Gateway ──POST /v1/... (max_tokens=1, kv_transfer_params={do_remote_decode:true})──► Prefill Pod
         ◄── response {kv_transfer_params: {remote_block_ids, ...}} ──────────────── Prefill Pod

Gateway updates ReqBody:
  kv_transfer_params.remote_host = prefill_pod_ip

Gateway ──POST /v1/... (with kv_transfer_params)──────────────────────────────────► Decode Pod
```

### vLLM (`AIBRIX_KV_CONNECTOR_TYPE=nixl`, Neuron)

```
Gateway ──POST /v1/... (max_tokens=1)──────────────────────────────────────────────► Prefill Pod
         ◄── response { entire prefill response } ──────────────────────────────── Prefill Pod

Gateway updates ReqBody:
  disagg_prefill_resp = <entire prefill response>

Gateway ──POST /v1/... (with disagg_prefill_resp)──────────────────────────────────► Decode Pod
```

### SGLang (async)

```
Gateway modifies ReqBody:
  bootstrap_host = prefill_pod_ip
  bootstrap_port = pod annotation / default 8998
  bootstrap_room = random int63

Gateway ──POST (async goroutine)──────────────────────────────────────────────────► Prefill Pod
         (no wait; prefill/decode coordinate via bootstrap protocol)

Gateway ──POST /v1/... (updated body)─────────────────────────────────────────────► Decode Pod
```

### TensorRT-LLM

```
Gateway adds disaggregated_params to prefill request:
  { request_type: "context_only", disagg_request_id: <snowflake_id> }

Gateway ──POST (sync)──────────────────────────────────────────────────────────────► Prefill Pod
         ◄── response { disaggregated_params: {...}, prompt_token_ids: [...] } ── Prefill Pod

Gateway updates ReqBody:
  disaggregated_params.request_type = "generation_only"
  prompt / prompt_token_ids from prefill response

Gateway ──POST /v1/... (with disaggregated_params)─────────────────────────────────► Decode Pod
```

TRT-LLM uses a Snowflake-style `disagg_request_id` (63-bit) to correlate prefill and decode:

```
[41-bit timestamp ms since 2023-01-01] [10-bit machine_id] [12-bit counter]
```

Machine ID is set via `AIBRIX_TRT_MACHINE_ID` (must be in `[0, 1024)`).

---

## Request Trackers

### PrefillRequestTracker

Tracks active **prefill** request counts per pod using `sync.Map` and `atomic.Int32`. Used by both prefill load-imbalance detection and `scorePrefillPods` (mean/stddev filter).

```
AddPrefillRequest(requestID, podName)   // on prefill start
RemovePrefillRequest(requestID)         // on prefill end (deferred)
GetPrefillRequestCountsForPods(pods)    // for scoring
```

### PendingDecodeTracker

Bridges the gap between **decode pod selection** and the actual decode request starting. Without this, concurrent requests could all route to the same decode pod (since `RealtimeNumRequestsRunning` hasn't updated yet).

```
AddPendingDecode(requestID, podName)    // after pod selection, before prefill
RemovePendingDecode(requestID)          // deferred in Route()
GetPendingDecodeCount(podName)          // added to running reqs in decode scoring
```

Timeline:
```
Route() called
  │
  ├─ filterPrefillDecodePods() selects decodePod
  ├─ AddPendingDecode(requestID, decodePod)   ← counts as +1 running
  ├─ doPrefillRequest()                        (may take seconds)
  ├─ ctx.SetTargetPod(decodePod)
  ├─ return address to caller
  └─ defer RemovePendingDecode(requestID)      ← cleaned up after Route returns
```

---

## Prompt Length Bucketing

Enabled by `AIBRIX_PROMPT_LENGTH_BUCKETING=true`.

Each pod can declare a prompt-length range via its config profile (`promptLenBucketMinLength` / `promptLenBucketMaxLength`). A pod is eligible only when the request's prompt length falls within its range.

```
collectAndBucketPods()
    ├─ Phase 1: build roleset → {prefills, decodes}
    └─ Phase 2: filter each roleset by prompt length range
                 → promptLengthBucketingPrefillPods
                 → promptLengthBucketingDecodePods

If bucket-filtered lists are non-empty, they replace the unfiltered lists.
```

### Combined Pods

When bucketing is enabled, pods with `combined=true` in their config profile can act as both prefill and decode. The router falls back to a combined pod when:

1. No bucket-matched prefill/decode pods exist for the request's prompt length, **or**
2. `shouldPickCombined()` returns `true`:
   - At least one combined pod has request rate < `0.25` (low load), **and**
   - At least one prefill pod **or** decode pod has request rate > `1.0` (high load).

```
shouldPickCombined():
    combinedLowLoad  = any combined pod request_rate < 0.25
    prefillHighLoad  = any prefill pod request_rate > 1.0
    decodeHighLoad   = any decode pod request_rate > 1.0
    return (prefillHighLoad OR decodeHighLoad) AND combinedLowLoad
```

When a combined pod is selected, `prefillPod` is `nil` (no prefill HTTP call) and `decodePod` is the selected combined pod.

---

## Environment Variables

### Core Behaviour

| Variable | Default | Description |
|----------|---------|-------------|
| `AIBRIX_PREFILL_SCORE_POLICY` | `prefix_cache` | Prefill pod scoring: `prefix_cache` or `least_request`. Any other value logs a warning and falls back to `prefix_cache`. |
| `AIBRIX_DECODE_SCORE_POLICY` | `load_balancing` | Decode pod scoring for `finalPDScore`: `load_balancing` or `least_request`. Any other value logs a warning and falls back to `load_balancing`. |
| `AIBRIX_KV_CONNECTOR_TYPE` | `shfs` | KV transfer backend: `shfs` (GPU/SHFS) or `nixl` (Neuron) |
| `AIBRIX_PREFILL_REQUEST_TIMEOUT` | `30` | Prefill HTTP request timeout in seconds |
| `AIBRIX_PROMPT_LENGTH_BUCKETING` | `false` | Enable prompt-length-based pod bucketing |

### Prefill Load Balancing

| Variable | Default | Description |
|----------|---------|-------------|
| `AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD` | `16` | Min `(max − min)` outstanding prefill request count to trigger imbalance routing |

### Decode Load Balancing

| Variable | Default | Description |
|----------|---------|-------------|
| `AIBRIX_DECODE_LB_WEIGHT_RUNNING` | `1.0` | Multiplier on normalized running-request term in `load_balancing` numerator |
| `AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT` | `1.0` | Multiplier on normalized inverse-throughput term in `load_balancing` numerator |
| `AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD` | `16.0` | Min `(max − min)` running request count spread to trigger decode imbalance routing |
| `AIBRIX_DECODE_THROUGHPUT_IMBALANCE_MIN_SPREAD` | `2048.0` | Min `(max − min)` token throughput spread (tok/s) to trigger throughput-based routing |
| `AIBRIX_DECODE_SCORE_RATIO_THRESHOLD` | `1.5` | `max_drain_score / min_drain_score` ratio threshold to trigger drain-rate routing |

### TensorRT-LLM

| Variable | Default | Description |
|----------|---------|-------------|
| `AIBRIX_TRT_MACHINE_ID` | `0` | 10-bit machine ID used in Snowflake disagg request ID generation (range: `[0, 1024)`) |

### Inherited from Prefix Cache Router

| Variable | Default | Description |
|----------|---------|-------------|
| `AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE` | `character` | Tokenizer for prefix-cache prefill scoring: `tiktoken` or `character` |
| `AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR` | `1` | Multiplier N: prefill pods with `req_count > mean + N*stddev` are skipped from per-roleset selection |

---

## SGLang Bootstrap Port

SGLang uses a bootstrap mechanism for prefill/decode coordination. The port is resolved in order:

1. Pod annotation `model.aibrix.ai/sglang-bootstrap-port`
2. Default: `8998`

---

## Metrics Emitted

| Metric | When |
|--------|------|
| `GatewayPrefillRequestFailTotal` | Engine validation fail, pod filter fail, prefill HTTP error |
| `GatewayPrefillRequestSuccessTotal` | Prefill HTTP succeeded |
| `PDSelectedPrefillPodTotal` | Prefill pod selected (per pod label) |
| `PDSelectedDecodePodTotal` | Decode pod selected (per pod label) |

---

## Response Headers Set

| Header | Value |
|--------|-------|
| `prefill-target-pod` | Name of the selected prefill pod |
| `prefill-target-pod-ip` | IP of the selected prefill pod |

---

## Prefix Cache Update Pipeline

After a prefill pod is selected, its prefix hashes are enqueued asynchronously to avoid blocking the routing path:

```
finalPDScore()
    └─ enqueuePrefixUpdate(hashes, model, pod)
              │
              ▼  (non-blocking send; dropped if channel full)
        prefixUpdateCh  (buffered, capacity 1024)
              │
              ▼  (single background goroutine)
        prefixCacheIndexer.AddPrefix(hashes, model, pod)
```

---

## Initialization

```go
NewPDRouter()
  │
  ├─ cache.Get()                          // shared metrics cache
  ├─ prefixcacheindexer.GetSharedPrefixHashTable()
  ├─ prefillScorePolicy from AIBRIX_PREFILL_SCORE_POLICY:
  │     least_request → LeastRequestPrefillPolicy
  │     prefix_cache  → PrefixCachePrefillPolicy (shared tokenizer + PrefixHashTable)
  │     (other)       → log warning, same as prefix_cache
  ├─ decode score policy (package algorithms/pd, decode_scorer.go) from AIBRIX_DECODE_SCORE_POLICY:
  │     least_request   → pd.LeastRequestDecodePolicy
  │     load_balancing  → pd.LoadBalancingDecodePolicy
  │     (other)         → log warning, same as load_balancing
  ├─ create HTTP client with connection pool
  │     MaxIdleConns=100, MaxIdleConnsPerHost=10, IdleConnTimeout=90s
  ├─ NewPrefillRequestTracker()
  ├─ NewPendingDecodeTracker()
  └─ startPrefixUpdater()                 // background goroutine
```
