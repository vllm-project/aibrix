# Service Discovery Providers

AIBrix was originally built on Kubernetes, and service discovery was tightly coupled to K8s informers. This made it impossible to run the gateway in non-K8s environments (bare metal, Docker Compose, VM-based deployments). This package defines the `Provider` interface for pluggable service discovery in the AIBrix gateway. It decouples the routing layer from any specific infrastructure — Kubernetes, Consul, etcd, or static configuration can all serve as backends.

## Interface

```go
type EventHandler func(event WatchEvent)

type Provider interface {
    Load() ([]any, error)
    Watch(handler EventHandler, stopCh <-chan struct{}) error
    Type() string
}
```

### `Load() ([]any, error)`

Returns all known resources as a snapshot. Currently returns `*v1.Pod` objects (and potentially `*modelv1alpha1.ModelAdapter`).

- **StaticProvider**: reads YAML file, returns synthetic pods.
- **KubernetesProvider**: returns nil — all data is delivered via `Watch()`.
- **Consul/etcd providers**: may return initial snapshot, or nil if they deliver everything via `Watch()`.

### `Watch(handler EventHandler, stopCh <-chan struct{}) error`

Registers a callback for resource changes and starts watching. The provider calls `handler` directly — there is no intermediate channel or buffer. This design allows K8s informers to invoke the handler on the informer goroutine without backpressure concerns.

- **StaticProvider**: no-op (static config never changes).
- **KubernetesProvider**: starts informers, blocks until initial list+sync completes, delivers all existing objects as `EventAdd` via the handler, then returns. After return, informer callbacks continue invoking the handler for ongoing changes.
- **Consul/etcd providers**: starts a watch/poll loop in a goroutine, calls handler from that goroutine.

`Watch` should block until the initial state is fully delivered, then return. This ensures the cache is warm before the gateway starts accepting traffic.

### `Type() string`

Returns a string identifier for logging: `"static"`, `"kubernetes"`, `"etcd"`, etc.

## Existing Providers

### StaticProvider (`static.go`)

Loads endpoints from a YAML config file. No dynamic updates.

**Non-disaggregated config:**

```yaml
models:
  - name: "Qwen/Qwen2.5-1.5B-Instruct"
    endpoints:
      - "vllm-0:8000"
      - "vllm-1:8000"
```

**Disaggregated (P/D) config:**

```yaml
models:
  - name: "Qwen/Qwen2.5-72B"
    engine: vllm
    rolesets:
      - name: default
        prefill:
          - "prefill-0:8000"
          - "prefill-1:8000"
        decode:
          - "decode-0:8000"
```

The `rolesets` structure expresses the pairing between prefill and decode workers. The PD routing algorithm selects the roleset first and then chooses the best prefill+decode pair within the same roleset. `endpoints` and `rolesets` are mutually exclusive per model.

### KubernetesProvider (`kubernetes.go`)

Watches Pods and ModelAdapters via K8s informers. This is the default when no `DiscoveryProvider` is set in `InitOptions`.

All data flows through `Watch()`:
1. Starts informers, waits for initial list+sync.
2. Delivers all existing Pods as `EventAdd` (ordered before ModelAdapters).
3. Delivers all existing ModelAdapters as `EventAdd`.
4. Returns — informer callbacks continue invoking the handler for ongoing changes.

The handler is wired directly into informer callbacks. No intermediate channel, no buffer, no backpressure concerns.

## Architecture

```
                    ┌──────────────────┐
                    │  Provider        │
                    │  Interface       │
                    └────────┬─────────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
    ┌─────────▼──┐  ┌───────▼────┐  ┌──────▼───────┐
    │ Static     │  │ Kubernetes │  │ Consul/etcd  │
    │ Provider   │  │ Provider   │  │ Provider     │
    │            │  │            │  │              │
    │ YAML file  │  │ Informers  │  │ Blocking     │
    │ → Load()   │  │ → Watch()  │  │ query / poll │
    └─────┬──────┘  └─────┬──────┘  └──────┬───────┘
          │               │                │
          │  synthetic *v1.Pod objects      │
          └───────────────┼────────────────┘
                          │
                          ▼
                ┌─────────────────┐
                │  Cache Store    │
                │  (metaPods,     │
                │   metaModels)   │
                └────────┬────────┘
                         │
                         ▼
                ┌─────────────────┐
                │  Routing        │
                │  Algorithms     │
                │  (unchanged)    │
                └─────────────────┘
```

All providers produce synthetic `*v1.Pod` objects. The cache store and routing algorithms are completely unaware of which discovery backend is in use.

## Future Work

- **Consul/etcd providers** — first-class support for non-K8s service discovery.
- **Platform-agnostic `Endpoint` type** — replace `*v1.Pod` as the internal representation to remove the K8s dependency from routing algorithms.
