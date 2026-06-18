# Prefix Cache Aware Routing

Prefix Cache Aware routing improves KV cache reuse across GPU pods by steering requests whose
prompt shares a common prefix to the pod that already has those KV blocks cached. When the
cluster is evenly loaded, cache locality is the primary signal; when load is severely skewed,
the router restricts prefix matching to the least-loaded pods to prevent hot-spots.

Two routing modes are supported, selected automatically at startup:

| Mode | When active | Prefix index |
|------|-------------|--------------|
| **Standard** | `AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED=false` (default) | Local hash table updated by the router after each request |
| **KV Sync** | `AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED=true` | Distributed sync indexer fed by real-time block-stored/removed events from vLLM pods |

Both modes share the same load-imbalance gate and stddev-based pod selection logic described below.

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
┌──────────────────────────────────────────┐        YES (gate fires)
│  Severe load imbalance?                  │──────────────────────────────────┐
│  max_req > IMBALANCE_FACTOR*(mean+1)     │                                  │
│  AND max_req - min_req >= IMBALANCE_GAP  │                                  ▼
└────────────┬─────────────────────────────┘     ┌──────────────────────────────────┐
             │ NO                                │  Restrict candidate set to        │
             │                                  │  least-loaded pod(s) only          │
             │◄─────────────────────────────────┘
             ▼
┌─────────────────────────────┐
│  Match prefix hashes        │
│  against candidate pods     │
└────────────┬────────────────┘
             │
    ┌────────┴────────┐
    │ match_pods      │ NO match_pods
    │ found?          │──────────────────────────────────────────────┐
    └────────┬────────┘                                              │
             │ YES                                                    │
             ▼                                                        │
┌─────────────────────────────┐                                      │
│  Sort match_pods by:        │                                      │
│  1. prefix_match% DESC      │                                      │
│  2. running_requests ASC    │                                      │
└────────────┬────────────────┘                                      │
             │                                                        │
             ▼                                                        │
┌─────────────────────────────┐        NO pod within threshold       │
│  Select first pod where:    │─────────────────────────────────────►│
│  running_req <=             │                                      │
│  mean + load_factor * σ     │                                      │
└────────────┬────────────────┘                                      │
             │                                                        │
             └──────────────────────┬───────────────────────────────┘
                                    │
                                    ▼
                    Least-request fallback among all ready pods
                                    │
                                    ▼
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

### Step 2 — Load Imbalance Gate

Before prefix matching, the router checks whether load is severely skewed across pods:

```
imbalanced = max_running_requests > IMBALANCE_FACTOR × (mean_running_requests + 1)
             AND (max_running_requests - min_running_requests) >= IMBALANCE_GAP
```

Both conditions must hold. When the gate fires, the candidate set for prefix matching is
**narrowed to only the least-loaded pods**. This prevents a heavily-cached pod from
monopolising all traffic while other pods sit idle. The subsequent prefix match and stddev
selection then run only over those least-loaded candidates.

### Step 3 — Prefix Match & Pod Selection

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
| `AIBRIX_PREFIX_CACHE_LOAD_IMBALANCE_FACTOR` | Gate multiplier: gate fires when `max > factor × (mean + 1)`. | `2.0` |
| `AIBRIX_PREFIX_CACHE_LOAD_IMBALANCE_MIN_GAP` | Minimum absolute gap (`max − min`) required alongside the factor check to trigger the gate. | `8` |
| `AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR` | `load_factor` in `mean + load_factor × σ`. Higher value tolerates more load on a high-cache-hit pod. | `1` |
| `AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED` | Enable KV sync routing mode (requires remote tokenizer). | `false` |

### Tuning guidance

- **Block size**: use `128` for `character` tokenizer and `16` for `tiktoken`. Larger blocks
  reduce hash overhead but require longer identical prefixes for a match.
- **Imbalance factor / gap**: lower the factor (e.g. `1.5`) or gap (e.g. `4`) to make the gate
  fire earlier and rebalance more aggressively. Raise them on hardware with large KV caches
  where cache reuse outweighs the cost of slight imbalance.
- **Standard deviation factor**: `1` (default) keeps all pods within one stddev of mean load.
  Increase to `2` to allow higher-loaded pods to still be selected for cache-hit requests.
