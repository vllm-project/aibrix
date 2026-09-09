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
```

`load_factor` (= `AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR`) controls how aggressively
the router skips overloaded cache-holding pods. If no matched pod is within the threshold, the
router falls back to the global least-loaded pod across all ready pods.

## Configuration

| Environment Variable | Description | Default |
|---|---|---|
| `AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE` | Tokenizer used to split the prompt. Options: `character`, `tiktoken`. | `character` |
| `AIBRIX_PREFIX_CACHE_BLOCK_SIZE` | Number of tokens per prefix block. Smaller blocks = finer-grained matching but more hash overhead. | `128` |
| `AIBRIX_PREFIX_CACHE_BLOCK_NUMBER` | Maximum number of prefix cache blocks tracked per pod. | `200000` |
| `AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR` | `load_factor` in `mean + load_factor × σ`. Higher value tolerates more load on a high-cache-hit pod. | `1` |
| `AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED` | Enable KV sync routing mode (requires remote tokenizer). | `false` |

See [ENV_VARS.md](../ENV_VARS.md#load-balance-router-algorithmsload_balancego) for the
load-imbalance gate's `AIBRIX_LOAD_BALANCE_IMBALANCE_FACTOR` /
`AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP` variables — they belong to the load-balance router, not
this one.

### Tuning guidance

- **Block size**: use `128` for `character` tokenizer and `16` for `tiktoken`. Larger blocks
  reduce hash overhead but require longer identical prefixes for a match.
- **Standard deviation factor**: `1` (default) keeps all pods within one stddev of mean load.
  Increase to `2` to allow higher-loaded pods to still be selected for cache-hit requests.
