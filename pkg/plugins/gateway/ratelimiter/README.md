# ratelimiter

Package `ratelimiter` provides a distributed, Redis-backed fixed-window rate limiter used by the Aibrix gateway to enforce per-user and per-model request limits across multiple gateway instances.

## Interface

```go
type RateLimiter interface {
    Get(ctx context.Context, key string) (int64, error)
    GetLimit(ctx context.Context, key string) (int64, error)
    Incr(ctx context.Context, key string, val int64) (int64, error)
}
```

| Method     | Description |
|------------|-------------|
| `Get`      | Returns the current counter value for a key in the active time window. |
| `GetLimit` | Returns the configured maximum for a key (used for limit lookups, not window counters). |
| `Incr`     | Atomically increments the counter by `val` and returns the new value. Sets the key TTL to the window size. |

## Implementations

### `redisRateLimiter` — `NewRedisAccountRateLimiter(name, client, windowSize)`

A fixed-window counter backed by Redis. Suitable for cluster-wide enforcement when multiple gateway replicas share the same Redis instance.

**Key scheme:** `{name}:{key}:{timebin}`

The time bin is computed as:

```
timebin = (time.Now().Unix() / windowSeconds) % 64
```

This cycles through 64 buckets, so at most 64 keys exist per logical counter. The TTL on each key is set to `windowSize`, so stale bins expire automatically.

- Minimum `windowSize` is 1 second (smaller values are clamped up).
- `Incr` uses a Redis pipeline (`INCRBY` + `EXPIRE`) for atomicity within a single round-trip.
- A missing key (`redis.Nil`) is treated as `0`, not an error.

### `noopRateLimiter` — `NewNoopRateLimiter()`

A no-op implementation that always allows requests. Used when Redis is unavailable (e.g., local development without a Redis sidecar). `GetLimit` returns `math.MaxInt64`.

## Usage in the gateway

Two limiter instances are created at server startup:

| Instance           | Constructor arg `name` | `windowSize` | Purpose                          |
|--------------------|------------------------|--------------|----------------------------------|
| `ratelimiter`      | `"aibrix"`             | 1 minute     | Per-user RPM and TPM enforcement |
| `modelRateLimiter` | `"aibrix_model"`       | 1 second     | Per-model RPS enforcement        |

The separate `name` prefix ensures the two limiters' keys never collide in Redis.

### Per-user limits (RPM / TPM)

Keys follow the pattern `aibrix:{username}_{RPM|TPM}_CURRENT:{timebin}`. Limits are read from the user record resolved during request header processing.

### Per-model RPS

Keys follow the pattern `aibrix_model:{modelName}_MODEL_RPS_CURRENT:{timebin}`. The RPS limit is configured per model via the `requestsPerSecond` field in a config profile:

```json
{
  "profiles": {
    "rps-limited": {
      "routingStrategy": "least-request",
      "requestsPerSecond": 1
    }
  }
}
```

The active profile is selected by passing the `config-profile` request header. If no profile is active or `requestsPerSecond` is unset, the RPS check is skipped entirely.

#### Two-phase enforcement

Per-model RPS enforcement is split across two call sites in `HandleRequestBody` to avoid charging quota for requests that fail during routing:

1. **`checkModelRPS`** — called before routing. Uses `Get` to read the current counter and rejects with HTTP 429 if `current >= limit`. Does not write.
2. **`incrModelRPS`** — called after routing succeeds (after `AddRequestCount`). Uses `Incr` to charge the quota only once a pod has been selected and the request is being forwarded.

This means requests rejected by routing errors (no available pods, invalid strategy, etc.) do not consume the per-model RPS budget.

**Tradeoff:** the check-then-increment sequence has a small TOCTOU race — concurrent requests that both read the same counter value before either increments can both pass the gate. In practice the race window is a single Redis round-trip and the over-admission is bounded to the number of concurrent inflight requests, which is acceptable for typical RPS values.
