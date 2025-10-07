# KV Cache Package

This package provides ZMQ-based client functionality for synchronizing KV cache events between vLLM pods and the AIBrix cache system.

## Overview

The kvcache package implements:
- ZMQ client for subscribing to vLLM KV cache events
- MessagePack encoding/decoding for efficient event serialization
- Event type definitions for block storage operations
- Metrics collection for monitoring

## Components

### ZMQ Client (`zmq_client.go`)

The ZMQ client connects to vLLM pods and subscribes to their KV cache event streams.

**Key Features:**
- Automatic reconnection with exponential backoff
- Event replay support for reliability
- Sequence tracking to detect missed events
- Configurable timeouts and buffer sizes

**Usage:**
```go
config := &ZMQClientConfig{
    PodName:      "vllm-pod-1",
    PodIP:        "10.0.0.1",
    PubPort:      5557,
    RouterPort:   5558,
    PollTimeout:  100 * time.Millisecond,
}

handler := &MyEventHandler{}
client := NewZMQClient(config, handler)

// Start the client
go client.Start(ctx)

// Stop when done
client.Stop()
```

### Event Types (`event_types.go`)

Defines the event types for KV cache operations:

- **BlockStoredEvent**: Emitted when new KV cache blocks are stored
- **BlockRemovedEvent**: Emitted when blocks are removed
- **AllBlocksClearedEvent**: Emitted when all blocks are cleared

### MessagePack Codec (`msgpack_encoder.go`, `msgpack_decoder.go`)

Provides efficient serialization for events:

**Encoder:**
```go
data, err := EncodeEventBatch(events)
```

**Decoder:**
```go
events, err := DecodeEventBatch(data)
```

### Metrics (`metrics.go`)

Prometheus metrics for monitoring:
- Events received/processed
- Connection status
- Error counts
- Processing latency

## Configuration

Environment variables:
- `AIBRIX_ZMQ_POLL_TIMEOUT`: Poll timeout (default: 100ms)
- `AIBRIX_ZMQ_REPLAY_TIMEOUT`: Replay request timeout (default: 5s)
- `AIBRIX_ZMQ_RECONNECT_INTERVAL`: Initial reconnect interval (default: 1s)

## Dependencies

- `github.com/pebbe/zmq4`: ZMQ Go bindings
- `github.com/shamaton/msgpack/v2`: MessagePack serialization
- System requirement: libzmq3 or higher

## Testing

Run tests with ZMQ support:
```bash
go test -tags="zmq" ./pkg/cache/kvcache/
```

## Example

See `zmq_client_test.go` for comprehensive examples including mock publishers for testing.