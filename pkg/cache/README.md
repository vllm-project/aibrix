# Cache Package

The cache package provides centralized caching and routing capabilities for the AIBrix LLM inference system, including pod management, model routing, and KV cache event synchronization.

## Overview

This package implements:
- Pod and model metadata caching
- Request routing and load tracking
- KV cache event synchronization
- GPU profile management
- Output prediction for performance optimization

## Core Components

### Cache Store (`cache.go`)

The central cache that maintains:
- Pod metadata and status
- Model-to-pod mappings
- Request tracking and metrics
- GPU profiles and capabilities

### KV Event Management

#### Event Manager (`kv_event_manager.go`)

Manages KV cache event synchronization between vLLM pods and the routing system:

```go
// The event manager is created automatically when the cache is initialized
// It subscribes to eligible pods and processes their KV cache events

// Configuration via environment variables:
// AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED=true
// AIBRIX_PREFIX_CACHE_USE_REMOTE_TOKENIZER=true
```

**Features:**
- Automatic pod discovery and subscription
- Lifecycle management (pod add/update/delete)
- Integration with the prefix cache indexer
- Support for multi-model deployments

#### Event Handler (`kv_event_handler.go`)

Processes incoming KV cache events:
- Routes events to the appropriate indexer
- Handles block stored/removed events
- Maintains consistency across pod restarts

### Request Tracking

#### Request Trace (`trace.go`)

Tracks individual requests through the system:
- Request timing and latency
- Token counts and model information
- GPU utilization metrics

#### Output Predictor (`output_predictor.go`)

Predicts output token counts based on historical data:
- Maintains sliding window of recent requests
- Groups by input token buckets
- Provides weighted predictions for capacity planning

### Informers (`informers.go`)

Kubernetes informers for watching:
- Pod lifecycle events
- ModelAdapter resources
- Configuration updates

## KV Cache Event Flow

1. **Pod Discovery**: Cache watches for pods with KV event labels
2. **Subscription**: Event manager creates ZMQ client for eligible pods
3. **Event Processing**: Incoming events update the prefix cache indexer
4. **Query Routing**: Router queries indexer for prefix matches
5. **Request Dispatch**: Requests routed to pods with matching prefixes

## Configuration

### Environment Variables

**Core Cache:**
- `AIBRIX_POD_DEPLOYMENT_LABEL`: Label for deployment identification
- `AIBRIX_POD_RAYCLUSTERFLEET_LABEL`: Label for fleet identification

**KV Event Sync:**
- `AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED`: Enable KV event synchronization
- `AIBRIX_PREFIX_CACHE_USE_REMOTE_TOKENIZER`: Enable remote tokenizer (required for KV sync)

**Performance:**
- `AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS`: Metric refresh interval
- `AIBRIX_MODEL_GPU_PROFILE_CACHING_FLAG`: Enable GPU profile caching

## Usage Example

```go
// Create cache with KV event sync enabled
os.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
os.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")

store := cache.NewStore()

// Add a pod - KV sync will start automatically if eligible
store.AddPod(pod)

// Query pods for a model
pods := store.GetPodsForModel("llama-2-7b")

// Track a request
store.AddRequest(pod.Name, model, inputTokens)
defer store.DoneRequest(pod.Name, model, outputTokens)
```

## Integration Points

### With Routing Algorithms
The cache provides data for routing decisions:
- Pod availability and load
- Prefix cache hit rates
- GPU utilization metrics

### With Gateway Plugins
Gateway plugins query the cache for:
- Available pods per model
- Current request counts
- Performance predictions

## Monitoring

Prometheus metrics are exposed for:
- Cache hit/miss rates
- Request queue depths
- KV event processing rates
- Pod synchronization status

## Testing

Run tests:
```bash
# All cache tests
go test ./pkg/cache/

# KV event specific tests  
go test -v ./pkg/cache/ -run ".*KV.*"

# With ZMQ support
go test -tags="zmq" ./pkg/cache/
```

## Thread Safety

All cache operations are thread-safe through:
- Read-write mutexes for data structures
- Atomic operations for counters
- Channel-based event processing
## Routing metric records and freshness

`Store` implements `MetricRecordCache`, which returns a metric value together
with the local successful-collection timestamp. The pair is stored atomically.
Scrape failures and metrics omitted from a successful scrape retain their old
timestamps. A new pod UID starts without the previous pod's records.

Value-only getters still return `metrics.MetricValue` and do not enforce an age
limit. Freshness-sensitive routing uses the record getters through
`routeutil.FreshKVUsage`. The exported `Pod.Metrics` / `Pod.ModelMetrics` maps now
store `MetricRecord`, so code accessing those maps directly must adapt; use the
value getters when age does not matter. Value-only custom adapters are supported
with a once-per-process warning that freshness checks are unavailable.

`AIBRIX_ROUTING_METRIC_STALE_THRESHOLD_MS` is a positive millisecond override read
at startup. Its default is `max(5s, 3 * refreshInterval, fetchTimeout + refreshInterval)`
(6s with the default 1s refresh and 5s timeout). Zero is not a disable switch.
The default does not guarantee freshness at arbitrary pod counts; bounded
collection capacity and backoff must be considered when tuning it. See the
[gateway freshness configuration](../plugins/gateway/ENV_VARS.md#kv-metric-freshness-algorithmsrouteutilkv_freshnessgo)
for routing fallback policies, diagnostic counters and tuning limitations.

The opt-in fake HTTP collection experiment exercises the real bounded queue,
workers and record writes for 1k/3k/5k logical pods at 20/50/200ms latency:

```sh
AIBRIX_TEST_METRIC_FRESHNESS_SCALE=1 go test ./pkg/cache \
  -run '^TestMetricFreshnessCollectionScale$' -count=1 -parallel 9 -timeout 15m -v
```

It reports missing/stale fractions and eligibility transitions in two observation
windows after warmup. These are local collection measurements, not Kubernetes,
GPU serving performance, or proof that the scheduler converges to a fair sweep.
