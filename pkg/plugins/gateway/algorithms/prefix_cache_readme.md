# Prefix Cache Aware Routing

Prefix Cache Aware routing improves KV cache reuse across GPU pods by steering requests whose
prompt shares a common prefix to the pod that already has those KV blocks cached. When the
cluster is evenly loaded, cache locality is the primary signal; when load is skewed, the router
falls back to least-loaded routing to prevent hot-spots.

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
┌─────────────────────────────┐        YES
│  Load imbalance?            │─────────────────────────────────────┐
│  max_req - min_req >        │                                     │
│  IMBALANCE_ABS_COUNT        │                                     ▼
└────────────┬────────────────┘              ┌────────────────────────────────┐
             │ NO                            │  Route to pod with least       │
             ▼                               │  running requests (fallback)   │
┌─────────────────────────────┐              └────────────────────┬───────────┘
│  Match prefix hashes        │                                   │
│  against ready pods         │                                   │
└────────────┬────────────────┘                                   │
             │                                                     │
    ┌────────┴────────┐                                           │
    │ match_pods      │ NO match_pods                             │
    │ found?          │──────────────────────────────────────────►│
    └────────┬────────┘                                           │
             │ YES                                                 │
             ▼                                                     │
┌─────────────────────────────┐                                   │
│  Sort match_pods by:        │                                   │
│  1. prefix_match% DESC      │                                   │
│  2. running_requests ASC    │                                   │
└────────────┬────────────────┘                                   │
             │                                                     │
             ▼                                                     │
┌─────────────────────────────┐        NO pod within threshold    │
│  Select first pod where:    │──────────────────────────────────►│
│  running_req <=             │                                   │
│  mean + load_factor * σ     │                                   │
└────────────┬────────────────┘                                   │
             │                                                     │
             └──────────────────────┬──────────────────────────────┘
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

### Step 2 — Load Imbalance Check

Before attempting prefix matching, the router checks whether the cluster is under severe load
imbalance:

```
imbalanced = (max_running_requests - min_running_requests) > IMBALANCE_ABS_COUNT
```

If imbalanced, prefix locality is ignored and the request is sent to the least-loaded pod
immediately. This prevents a heavily-cached pod from becoming a bottleneck.

### Step 3 — Prefix Match & Pod Selection

Each ready pod is scored by how many of the request's prefix blocks it already holds
(`prefix_match_percent`). Matched pods are sorted:

1. **Descending** by `prefix_match_percent` — prefer higher cache hit rate.
2. **Ascending** by `running_requests` — break ties by choosing the less loaded pod.

The first pod in this sorted list that satisfies the load threshold is selected:

```
pod.running_requests <= mean_running_requests + load_factor × std_dev
```

`load_factor` (= `STANDARD_DEVIATION_FACTOR`) controls how aggressively the router avoids
over-loading a high-cache-hit pod. If no matched pod is within the threshold, the router falls
back to the global least-loaded pod.

## Configuration

| Environment Variable                                    | Description                                                                                                   | Default     |
|---------------------------------------------------------|---------------------------------------------------------------------------------------------------------------|-------------|
| `AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE`                   | Tokenizer used to split the prompt. Options: `character`, `tiktoken`.                                        | `character` |
| `AIBRIX_PREFIX_CACHE_BLOCK_SIZE`                       | Number of tokens per prefix block. Smaller blocks = finer-grained matching but more hash overhead.           | `128`       |
| `AIBRIX_PREFIX_CACHE_BLOCK_NUMBER`                     | Maximum number of prefix cache blocks tracked per pod.                                                        | `200000`    |
| `AIBRIX_PREFIX_CACHE_POD_RUNNING_REQUEST_IMBALANCE_ABS_COUNT` | Absolute difference between max and min running requests that triggers the imbalance fallback.        | `16`        |
| `AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR`        | `load_factor` in `mean + load_factor × σ`. Higher value tolerates more load on a high-cache-hit pod.        | `2`         |

### Tuning guidance

- **Block size**: use `128` for `character` tokenizer and `16` for `tiktoken`. Larger blocks
  reduce hash overhead but require longer identical prefixes for a match.
- **Imbalance threshold**: lower values (e.g. `8`) make the router more aggressive about
  rebalancing; raise it (e.g. `32`) on hardware with large KV caches where cache reuse saves
  more than rebalancing gains.
- **Standard deviation factor**: `2` covers ~95% of pods under normal load. Reduce to `1` if
  you want stricter load balancing at the cost of slightly lower cache hit rates.
