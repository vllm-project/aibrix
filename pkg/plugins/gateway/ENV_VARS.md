# Gateway Plugin Environment Variables

This document covers all environment variables used in the `pkg/plugins/gateway` package and its sub-packages.

---

## General Gateway

| Variable | Type | Default | Description | Source |
|---|---|---|---|---|
| `POD_NAME` | string | `""` | Kubernetes pod name. Used for logging and metric label tagging. | [gateway.go](gateway.go), [util.go](util.go) |
| `ROUTING_ALGORITHM` | string | _(none)_ | Default routing algorithm when no per-request override is set. | [types.go](types.go), [util.go](util.go) |

---

## Response Processing

| Variable | Type | Default | Description | Source |
|---|---|---|---|---|
| `AIBRIX_TTFT_THRESHOLD_S` | int (seconds) | `1` | Time-to-first-token threshold in seconds. Requests exceeding this are flagged in response processing. | [gateway_rsp_body.go](gateway_rsp_body.go) |

---

## Redis Sync (`statesync/`)

| Variable | Type | Default | Description | Source |
|---|---|---|---|---|
| `AIBRIX_STATESYNC_ENABLED` | bool | `false` | Enable cross-replica state sync via Redis. Must be `true` to activate the statesync manager. | [cmd/plugins/main.go](../../../cmd/plugins/main.go) |
| `AIBRIX_STATESYNC_SYNC_PERIOD` | duration | `10s` | Interval at which gateway state is synced to Redis across replicas. | [statesync/redissync.go](statesync/redissync.go) |

---

## Prefix Cache Router (`algorithms/prefix_cache.go`)

| Variable | Type | Default | Description | Source |
|---|---|---|---|---|
| `AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE` | string | `"character"` | Tokenizer type for prefix cache hashing. Options: `character`, `tiktoken`, `remote`. | [algorithms/prefix_cache.go](algorithms/prefix_cache.go) |
| `AIBRIX_PREFIX_CACHE_POD_RUNNING_REQUEST_IMBALANCE_ABS_COUNT` | int | `8` | Absolute running-request count difference threshold that triggers load-imbalance routing. | [algorithms/prefix_cache.go](algorithms/prefix_cache.go) |
| `AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR` | int | `1` | Factor multiplied by the standard deviation of pod loads during imbalance calculation. | [algorithms/prefix_cache.go](algorithms/prefix_cache.go) |
| `AIBRIX_PREFIX_CACHE_LOCAL_ROUTER_METRICS_ENABLED` | bool | `false` | Enable Prometheus metrics collection for the prefix cache local router. | [algorithms/prefix_cache.go](algorithms/prefix_cache.go) |
| `AIBRIX_PREFIX_CACHE_USE_REMOTE_TOKENIZER` | bool | `false` | Use a remote HTTP tokenizer service instead of the local tokenizer. Requires `AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE=remote`. | [algorithms/prefix_cache.go](algorithms/prefix_cache.go) |
| `AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED` | bool | `false` | Enable KV cache event synchronization across gateway replicas. When `true`, also requires `AIBRIX_PREFIX_CACHE_USE_REMOTE_TOKENIZER=true`. | [algorithms/prefix_cache.go](algorithms/prefix_cache.go) |
| `AIBRIX_PREFIX_CACHE_REMOTE_TOKENIZER_ENDPOINT` | string | `""` | Remote tokenizer service endpoint URL. Required when `AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED=true`. | [pkg/constants/kv_event_sync.go](../../constants/kv_event_sync.go) |
| `AIBRIX_PREFIX_CACHE_KV_EVENT_PUBLISH_ADDR` | string | `""` | ZMQ publish address for KV cache events. Used when KV event sync is enabled. | [pkg/constants/kv_event_sync.go](../../constants/kv_event_sync.go) |
| `AIBRIX_PREFIX_CACHE_KV_EVENT_SUBSCRIBE_ADDRS` | string | `""` | ZMQ subscribe addresses for KV cache events (comma-separated). Used when KV event sync is enabled. | [pkg/constants/kv_event_sync.go](../../constants/kv_event_sync.go) |

### Remote Tokenizer Pool (`algorithms/prefix_cache.go`)

These configure the pool of remote tokenizer connections used when `AIBRIX_PREFIX_CACHE_USE_REMOTE_TOKENIZER=true`.

| Variable | Type | Default | Description |
|---|---|---|---|
| `AIBRIX_VLLM_TOKENIZER_ENDPOINT_TEMPLATE` | string | `"http://%s:8000"` | HTTP endpoint template for tokenizer pods. `%s` is replaced with the pod name. |
| `AIBRIX_TOKENIZER_HEALTH_CHECK_PERIOD` | duration | `30s` | How often to health-check tokenizer pool members. |
| `AIBRIX_TOKENIZER_TTL` | duration | `300s` | TTL for cached tokenizer connections in the pool. |
| `AIBRIX_MAX_TOKENIZERS_PER_POOL` | int | `100` | Maximum number of tokenizer connections in the pool. |
| `AIBRIX_TOKENIZER_REQUEST_TIMEOUT` | duration | `5s` | Timeout for individual remote tokenizer requests. |

---

## Preble (Prefix Cache with Histogram) Router (`algorithms/prefix_cache_preble.go`)

| Variable | Type | Default | Description |
|---|---|---|---|
| `AIBRIX_ROUTER_PREBLE_TARGET_GPU` | string | `"V100"` | GPU model used for hardware-specific latency estimates in the Preble algorithm. |
| `AIBRIX_ROUTER_PREBLE_DECODING_LENGTH` | int | `45` | Expected decode sequence length used for cache allocation decisions. |
| `AIBRIX_ROUTER_PREBLE_SLIDING_WINDOW_PERIOD` | int (minutes) | `3` | Sliding window length in minutes for histogram metrics collection. |
| `AIBRIX_ROUTER_PREBLE_EVICTION_LOOP_INTERVAL` | int (ms) | `1000` | Interval in milliseconds between cache eviction loop executions. |

---

## VTC (Virtual Token Counter) Router

### Token Tracker (`algorithms/vtc/token_tracker.go`)

| Variable | Type | Default | Description |
|---|---|---|---|
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_WINDOW_SIZE` | int | `5` | Sliding window size (in `TIME_UNIT` units) for token usage tracking. |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_TIME_UNIT` | string | `"minutes"` | Time unit for the sliding window. Options: `minutes`, `seconds`, `milliseconds`. |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MIN_TOKENS` | float64 | `1000.0` | Minimum token count threshold for adaptive load normalization. |
| `AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MAX_TOKENS` | float64 | `8000.0` | Maximum token count threshold for adaptive load normalization. |

### VTC Basic Scorer (`algorithms/vtc/vtc_basic.go`)

Scoring formula: `score = (fairnessWeight * normFairness + utilizationWeight * normUtilization) / normFreeGPU`

| Variable | Type | Default | Description |
|---|---|---|---|
| `AIBRIX_ROUTER_VTC_BASIC_MAX_POD_LOAD` | float64 | `100.0` | Load value at which a pod is considered fully saturated. |
| `AIBRIX_ROUTER_VTC_BASIC_INPUT_TOKEN_WEIGHT` | float64 | `1.0` | Weight applied to input tokens when computing pod load. |
| `AIBRIX_ROUTER_VTC_BASIC_OUTPUT_TOKEN_WEIGHT` | float64 | `2.0` | Weight applied to output tokens when computing pod load (typically higher than input). |
| `AIBRIX_ROUTER_VTC_BASIC_FAIRNESS_WEIGHT` | float64 | `1.0` | Weight of the fairness component in the routing score. |
| `AIBRIX_ROUTER_VTC_BASIC_UTILIZATION_WEIGHT` | float64 | `1.0` | Weight of the utilization component in the routing score. |

---

## PD (Prefill-Decode) Disaggregation Router (`algorithms/pd_disaggregation.go`)

| Variable | Type | Default | Description |
|---|---|---|---|
| `AIBRIX_PREFILL_REQUEST_TIMEOUT` | int (seconds) | `30` | HTTP request timeout for prefill pod calls. |
| `AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD` | int32 | `16` | Minimum (max − min) running-request spread across prefill pods to trigger load-imbalance routing. |
| `AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD` | float64 | `16.0` | Minimum (max − min) running-request spread across decode pods to trigger load-imbalance routing. |
| `AIBRIX_DECODE_THROUGHPUT_IMBALANCE_MIN_SPREAD` | float64 | `2048.0` | Minimum (max − min) token-throughput spread (tokens/s) across decode pods to trigger throughput-imbalance routing. |
| `AIBRIX_DECODE_SCORE_RATIO_THRESHOLD` | float64 | `1.5` | Max/min drain-rate score ratio above which the slowest decode pod is excluded from selection. |
| `AIBRIX_PROMPT_LENGTH_BUCKETING` | bool | `false` | Route requests to prefill pods whose prompt-length bucket matches the request length. |
| `AIBRIX_KV_CONNECTOR_TYPE` | string | `"shfs"` | KV cache transfer backend. Options: `shfs` (GPU shared memory), `nixl` (Neuron). |
| `AIBRIX_PREFILL_SCORE_POLICY` | string | `"prefix_cache"` | Strategy for selecting the prefill pod. Options: `prefix_cache`, `least_request`. |
| `AIBRIX_DECODE_SCORE_POLICY` | string | `"load_balancing"` | Strategy for selecting the decode pod. Options: `load_balancing`, `least_request`. |

### Decode Load Balancer Scorer (`algorithms/pd/decode_scorer.go`)

Scoring formula: `score = (wRun × normRunning + wThroughput × normInvThroughput) / normFreeGPU`

| Variable | Type | Default | Description |
|---|---|---|---|
| `AIBRIX_DECODE_LB_WEIGHT_RUNNING` | float64 | `1.0` | Weight for the normalized running-request term in the decode LB score. |
| `AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT` | float64 | `1.0` | Weight for the normalized inverse-throughput term in the decode LB score. |

---

## Utilities (`algorithms/util.go`)

| Variable | Type | Default | Description |
|---|---|---|---|
| `AIBRIX_TRT_MACHINE_ID` | int64 | `0` | 10-bit machine ID (0–1023) used in Snowflake-style disaggregation request ID generation: `[timestamp:41b][machineID:10b][counter:12b]`. Panics on init if out of range. |

---

## Variable Dependency Notes

The following variables have interdependencies that must be satisfied together:

- **KV event sync** requires all three to be set consistently:
  ```
  AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED=true
  AIBRIX_PREFIX_CACHE_USE_REMOTE_TOKENIZER=true
  AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE=remote
  AIBRIX_PREFIX_CACHE_REMOTE_TOKENIZER_ENDPOINT=<url>
  ```

- **Remote tokenizer pool** is initialized whenever `AIBRIX_PREFIX_CACHE_USE_REMOTE_TOKENIZER=true`. The pool variables (`AIBRIX_VLLM_TOKENIZER_ENDPOINT_TEMPLATE`, `AIBRIX_TOKENIZER_*`, `AIBRIX_MAX_TOKENIZERS_PER_POOL`) all apply in that case.
