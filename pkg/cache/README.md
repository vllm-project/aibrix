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
// AIBRIX_KV_EVENT_SYNC_ENABLED=true
// AIBRIX_USE_REMOTE_TOKENIZER=true
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
- `AIBRIX_KV_EVENT_SYNC_ENABLED`: Enable KV event synchronization
- `AIBRIX_USE_REMOTE_TOKENIZER`: Enable remote tokenizer (required for KV sync)

**Performance:**
- `AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS`: Metric refresh interval
- `AIBRIX_Model_GPU_PROFILE_CACHING_FLAG`: Enable GPU profile caching

## Usage Example

```go
// Create cache with KV event sync enabled
os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")

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