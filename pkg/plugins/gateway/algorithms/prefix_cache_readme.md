# Prefix Cache Aware Routing

Prefix Cache Aware routing improves KV cache reuse across GPU pods by steering requests whose
prompt shares a common prefix to the pod that already has those KV blocks cached. Pod selection
among prefix-matched candidates is still load-aware (see Step 2 below), and this router itself
is purely about prefix caching — it does not gate or restrict candidates based on cluster-wide
load imbalance. That concern is instead handled centrally by the gateway: `selectTargetPod` in
`gateway.go` applies the [load-balance router](load_balance.go)'s `ApplyLoadImbalanceGate` once,
ahead of whichever strategy actually routes the request (prefix-cache included), narrowing
`readyPods` before this router ever sees them. This replaced an older prefix-cache-specific gate
that lived in this file; see [ENV_VARS.md](../ENV_VARS.md#variable-dependency-notes) for the
migration note. The gate applies the same way when combining strategies via multi-strategy
soft-scoring (`AIBRIX_ROUTING_ALGORITHM=prefix-cache:W1,load-balance:W2`, see
[multi_router_readme.md](multi_router_readme.md)): `selectTargetPod` narrows `readyPods` once,
before resolving and calling into the multi-strategy router, so every sub-strategy's `ScoreAll`
— this one included — only ever sees the already-gated pod list. It is skipped only for
exclusive strategies (`pd`, `slo*`), which manage their own pod subsets.

Two routing modes are supported, selected automatically at startup:

| Mode | When active | Prefix index |
|------|-------------|--------------|
| **Standard** | `AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED=false` (default) | Local hash table updated by the router after each request |
| **KV Sync** | `AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED=true` | Distributed sync indexer fed by real-time block-stored/removed events from vLLM pods |

Both modes share the same stddev-based pod selection logic described below.

When the optional candidate stage seeds prefix holders, it retains the raw match
percentages and prefix hashes for that routing decision. Scoring, standalone
routing and post-route updates reuse this result for covered pod subsets in the
same index/model partition, including pods that had no prefix match. Blended
scores are still normalized on the final candidate pool. Index changes during
the decision are observed by the next decision; retries and pooled context reuse
clear the retained result. Tokenization or index errors are not cached, and
ordinary routing without seeding does not retain a match snapshot.

The index contract requires each pod's raw match to be independent of the other
queried pods, and the ready-set input map to remain unchanged. Hash elements
returned by the router are read-only; slice capacity is restricted so an append
cannot overwrite the array retained by the decision.

### Observing match reuse

`aibrix_prefix_cache_match_operations_total{model,result}` records each prefix
matching operation. The fixed `result` values are:

| Result | Meaning |
| --- | --- |
| `lookup` | Read the index without a retained result, including initial seeding. |
| `reuse` | Project a retained result onto a covered subset. |
| `fallback_identity` | Read the index because its identity, model or adapter changed. |
| `fallback_uncovered` | Read the index because the requested set contains an uncovered pod. |

All results except `reuse` perform an index lookup. Ordinary routing with the
candidate stage disabled also reports `lookup`; compare reuse rates only for
traffic where candidate seeding is enabled. Replacing prefix tokens clears the
snapshot, so subsequent operations report `lookup` until seeding runs again.
Identity and coverage fallbacks also emit a V(4) log with the request ID and
reason, without logging prompt contents.

## Routing Flow

```
      Incoming Request
             │
             ▼
┌─────────────────────────────┐
│  Tokenize & hash prompt     │  splits prompt into fixed-size blocks,
│  into prefix blocks         │  computes hash per block
└────────────┬────────────────┘
             │
             ▼
┌─────────────────────────────┐
│  Match prefix hashes        │
│  against ready pods         │
└────────────┬────────────────┘
             │
    ┌────────┴────────┐
    │ match_pods      │ NO match_pods
    │ found?          │──────────────────────────────────────────────┐
    └────────┬────────┘                                              │
             │ YES                                                   │
┌─────────────────────────────┐                                      │
│  Sort match_pods by:        │                                      │
│  1. prefix_match% DESC      │                                      │
│  2. running_requests ASC    │                                      │
└────────────┬────────────────┘                                      │
             │                                                       │
┌─────────────────────────────┐          NO pod within threshold     │
│  Select first pod where:    │─────────────────────────────────────►│
│  running_req <=             │                                      │
│  mean + load_factor * σ     │                                      │
└────────────┬────────────────┘                                      │
             │                                                       │
             │                                                       ▼
             │                                Least-request fallback among all ready pods
             │                                                       │
             ▼                                                       ▼
                                Target Pod Selected
```

## Algorithm Details

### Step 1 — Tokenize & Hash

The prompt is tokenized and split into fixed-size blocks. A hash is computed for each block.
Two tokenizer backends are supported:

| Tokenizer  | How it works                                          |
|------------|-------------------------------------------------------|
| `character`| Splits raw text into individual characters (default)  |
| `tiktoken` | Uses the OpenAI [tiktoken](https://github.com/openai/tiktoken) BPE tokenizer |

Block hashes are used as cache keys — a pod that has processed a request with an identical
prefix will have those KV blocks hot in GPU memory.

### Step 2 — Prefix Match & Pod Selection

Each candidate pod is scored by how many of the request's prefix blocks it already holds
(`prefix_match_percent`). Matched pods are sorted:

1. **Descending** by `prefix_match_percent` — prefer higher cache hit rate.
2. **Ascending** by `running_requests` — break ties by choosing the less loaded pod.

The first pod in this sorted list that satisfies the load threshold is selected:

```
pod.running_requests <= mean_running_requests + load_factor × std_dev
pod.running_requests <= max(load_cap_factor, 1) × (mean_running_requests + 1)
```

`load_factor` (= `AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR`) controls how aggressively
the router skips overloaded cache-holding pods. If no matched pod is within the threshold, the
router falls back to the global least-loaded pod across all ready pods. `load_cap_factor`
(= `AIBRIX_PREFIX_CACHE_LOAD_CAP_FACTOR`) bounds load directly: no value in n pods sits more than
(n−1)/√n standard deviations above the mean, so at `load_factor` 2 the first check cannot skip
any pod in a pool of five or fewer.

## Configuration

| Environment Variable | Description | Default |
|---|---|---|
| `AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE` | Tokenizer used to split the prompt. Options: `character`, `tiktoken`. | `character` |
| `AIBRIX_PREFIX_CACHE_BLOCK_SIZE` | Number of tokens per prefix block. Smaller blocks = finer-grained matching but more hash overhead. | `128` |
| `AIBRIX_PREFIX_CACHE_BLOCK_NUMBER` | Maximum number of prefix cache blocks tracked per pod. | `200000` |
| `AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR` | `load_factor` in `mean + load_factor × σ`. Higher value tolerates more load on a high-cache-hit pod. | `1` |
| `AIBRIX_PREFIX_CACHE_LOAD_CAP_FACTOR` | `load_cap_factor` in `max(load_cap_factor, 1) × (mean + 1)`. Higher value tolerates more load on a high-cache-hit pod; set a large value such as `100` to effectively disable it. Applies only when `prefix-cache` routes on its own, with the auto-blend off (`AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT=0`). | `1.5` |
| `AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED` | Enable KV sync routing mode (requires remote tokenizer). | `false` |
| `AIBRIX_PREFIX_CACHE_INCLUDE_TOOLS` | Prepend the canonicalized chat request `tools` to the prompt text before hashing, so requests that share messages but carry different tools do not look like a full prefix match. Token estimates keep using the messages only. KV-sync mode tokenizing through the remote chat template forwards the raw `tools` to the tokenizer regardless of this flag, so the template places them. Set to `false` to hash the messages only. | `true` |

See [ENV_VARS.md](../ENV_VARS.md#load-balance-router-algorithmsload_balancego) for the
load-imbalance gate's `AIBRIX_LOAD_BALANCE_IMBALANCE_FACTOR` /
`AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP` variables — they belong to the load-balance router, not
this one.

### Tuning guidance

- **Block size**: use `128` for `character` tokenizer and `16` for `tiktoken`. Larger blocks
  reduce hash overhead but require longer identical prefixes for a match.
- **Standard deviation factor**: `1` (default) keeps all pods within one stddev of mean load.
  Increase to `2` to allow higher-loaded pods to still be selected for cache-hit requests.
