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
| `AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR` | int | `1` | Factor multiplied by the standard deviation of pod loads when selecting among prefix-matched pods (`pod.req ≤ mean + factor × σ`). | [algorithms/prefix_cache.go](algorithms/prefix_cache.go) |
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

## Load Balance Router (`algorithms/load_balance.go`)

The load-imbalance gate below is applied centrally by the gateway (`selectTargetPod` in
`gateway.go`), once per request, ahead of whichever strategy actually routes it — not just when
`load-balance` is selected. It restricts candidates to the least-loaded pods before that
strategy runs when load is severely skewed, and it **is** applied to the Prefix Cache and
Preble routers (this replaces the prefix-cache-specific gate that used to live in
`prefix_cache.go`; see the deprecation note under [Variable Dependency
Notes](#variable-dependency-notes)). It is exempted only for exclusive strategies (`pd`,
`slo*`), which manage their own pod subsets and would have that management disrupted by a
blanket running-request-count filter applied ahead of time. It does not participate in
multi-strategy soft-scoring (`ScoreAll`) for strategies blended alongside `load-balance` — see
`AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT` below for how `load-balance` itself gets
silently blended into other strategies to compensate.

When `load-balance` is the (sole, non-blended) strategy that routes the request and multiple
pods tie on the lowest pending-time score, `Route()` breaks the tie using least combined
GPU+CPU KV-cache usage (falling back to a random pick if cache metrics are unavailable for the
tied pods) — the same secondary-signal pattern the Prefix Cache router uses to break ties in
prefix-match percentage via request count.

| Variable | Type | Default | Description | Source |
|---|---|---|---|---|
| `AIBRIX_LOAD_BALANCE_IMBALANCE_FACTOR` | float64 | `2.0` | Gate multiplier for 3+ pods: gate fires when `max_req > factor × (mean_req + 1)`. Ignored for 2-pod clusters (the relative check never holds there). | [algorithms/load_balance.go](algorithms/load_balance.go) |
| `AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP` | int | `8` | Minimum absolute gap (`max_req − min_req`) required to trigger the load-imbalance gate. For 2 pods this is the sole trigger; for 3+ pods it is required alongside the factor check. | [algorithms/load_balance.go](algorithms/load_balance.go) |

---

## Router Selection / Auto-Blend (`algorithms/router.go`)

`RouterManager.Select` silently blends `load-balance` (and, when needed, `least-request`)
behind whatever strategy a caller actually asked for, so no single strategy can keep steering
traffic at an already-hot pod even without going through the central load-imbalance gate above.
The caller never sees this: `ctx.Algorithm`, response headers, and `Validate()` all continue to
reflect exactly what was requested. The blend is skipped entirely for exclusive strategies
(`pd`, `slo*`) and for an explicit, standalone `load-balance` selection, both of which must keep
running their own dedicated routing logic.

| Variable | Type | Default | Description | Source |
|---|---|---|---|---|
| `AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT` | int | `1` | Weight coefficient for the silently-appended `load-balance` scorer. Set to `0` to disable the whole auto-blend feature. | [algorithms/router.go](algorithms/router.go) |
| `AIBRIX_ROUTING_AUTO_BLEND_LEAST_REQUEST_WEIGHT` | int | `1` | Weight coefficient for the silently-appended `least-request` scorer (only added when not already present; needed so multi-port/data-parallel pod routing keeps working under the blend). Set to `0` to omit it from the blend. | [algorithms/router.go](algorithms/router.go) |

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

- **`AIBRIX_PREFIX_CACHE_POD_RUNNING_REQUEST_IMBALANCE_ABS_COUNT` is removed.** The
  prefix-cache-specific load-imbalance gate it used to tune was replaced by the centralized gate
  described under [Load Balance Router](#load-balance-router-algorithmsload_balancego), which
  now applies to prefix-cache (and every other non-exclusive strategy) via
  `AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP` / `AIBRIX_LOAD_BALANCE_IMBALANCE_FACTOR` instead. The
  trigger condition also changed: the old gate fired on absolute gap alone; the new one
  additionally requires a relative `factor × (mean + 1)` check for 3+ pod clusters, so it fires
  less often on larger fleets. Setting the old variable now only logs a startup warning.

## OpenTelemetry

The gateway plugins feature built-in support for distributed tracing via OpenTelemetry (OTel), empowering you to monitor and trace end-to-end requests across the entire external processing pipeline.

**Tracing is opt-in by default.** Telemetry components will only initialize if you explicitly configure either ``OTEL_EXPORTER_OTLP_ENDPOINT`` or ``OTEL_EXPORTER_OTLP_TRACES_ENDPOINT``. If both are omitted, all tracing capabilities remain disabled to conserve system resources.

| Variable | Type | Default | Description                                                                                                                                                                                                                       | Source                                                                                                             |
|---|---|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------|
| `OTEL_EXPORTER_OTLP_PROTOCOL` | string | `grpc`  | The transport protocol for OTLP data. Valid options are `grpc`, `http`, or `http/protobuf`.                                                                                                                                       | [cmd/plugins/main.go](../../../cmd/plugins/main.go)                                                                |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | string | `""`    | Base URL for all OTLP signals. **⚠️ Note:** ensure your ENDPOINT URLs include the `http://` or `https://` prefix. The SDK automatically appends signal-specific paths to this URL (e.g., appending `/v1/traces` for HTTP exports). | [cmd/plugins/main.go](../../../cmd/plugins/main.go)                                                                |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | string | `""`    | Target URL specifically for traces. Takes precedence over the global `ENDPOINT`. **⚠️ Note:** This URL is used exactly **as-is**. The SDK will **NOT** automatically append `/v1/traces` to it.                                   | [cmd/plugins/main.go](../../../cmd/plugins/main.go)                                                                |
| `OTEL_EXPORTER_OTLP_HEADERS` | string | `""`    | Key-value pairs used as headers for OTLP requests (e.g., `key1=value1,key2=value2`). Useful for passing auth tokens to backends like Datadog or Honeycomb.                                                                        | [cmd/plugins/main.go](../../../cmd/plugins/main.go)                                                                |
| `OTEL_EXPORTER_OTLP_TIMEOUT` | string | `10s`   | Maximum time the OTLP exporter will wait for each batch export.                                                                                                                                                                   | [cmd/plugins/main.go](../../../cmd/plugins/main.go)                                                                |
| `OTEL_EXPORTER_OTLP_INSECURE` | bool | `false` | Set to `true` to disable TLS/HTTPS for the exporter. Force downgrade to HTTP.                                                                                                                                                     | [cmd/plugins/main.go](../../../cmd/plugins/main.go)                                                                |
| `OTEL_EXPORTER_OTLP_INSECURE_SKIP_VERIFY` | bool | `false` |  Keeps TLS active but skips server certificate validation (useful for self-signed certs). Set to "true" to enable. | [cmd/plugins/main.go](../../../cmd/plugins/main.go)                                                                      |

> **Note on OpenTelemetry Configuration:**
> Aibrix supports the standard OpenTelemetry Protocol Exporter environment variables. For advanced OTLP configurations (such as `_CERTIFICATE`, `_CLIENT_KEY`, or specific `_COMPRESSION` settings), please refer to the [OpenTelemetry Protocol Exporter](https://opentelemetry.io/docs/specs/otel/protocol/exporter/)
