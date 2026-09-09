# Service Discovery Providers

AIBrix was originally built on Kubernetes, and service discovery was tightly coupled to K8s informers. This made it impossible to run the gateway in non-K8s environments (bare metal, Docker Compose, VM-based deployments). This package defines the `Provider` interface for pluggable service discovery in the AIBrix gateway. It decouples the routing layer from any specific infrastructure — Kubernetes, Consul, etcd, or static configuration can all serve as backends.

## Interface

```go
type EventHandler func(event WatchEvent)

type Provider interface {
    Watch(handler EventHandler, stopCh <-chan struct{}) error
    Type() string
}
```

### `Watch(handler EventHandler, stopCh <-chan struct{}) error`

Registers a callback for resource changes and starts watching. The provider calls `handler` directly — there is no intermediate channel or buffer. This design allows K8s informers to invoke the handler on the informer goroutine without backpressure concerns.

- **StaticProvider**: reads config, delivers all endpoints as `EventAdd` via the handler, returns. No ongoing changes.
- **KubernetesProvider**: wires the handler directly into informer callbacks. Events (including the initial list phase) flow to the handler immediately. After `WaitForCacheSync`, does a post-sync reconcile to fix ordering, then returns. Informer callbacks continue invoking the handler for ongoing changes.
- **EtcdProvider**: loads the initial snapshot, then watches the next revision in a goroutine and calls the handler for changes.

`Watch` should return once the provider has reached a consistent ready state (e.g., initial sync complete, config loaded). This ensures the cache is warm before the gateway starts accepting traffic.

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

### EtcdProvider (`etcd.go`)

Watches an etcd v3 key prefix for inference worker registrations. Select it in
standalone mode with a YAML or JSON configuration file:

```bash
bin/gateway-plugins --standalone \
  --etcd-config=deployment/local/configs/etcd.yaml
```

`--etcd-config` requires `--standalone` and is mutually exclusive with
`--endpoints-config`. Static discovery remains selected when
`--endpoints-config` is supplied. The gateway still needs an Envoy proxy for
inference requests; the [local deployment](../../../deployment/local) provides
an Envoy configuration.

```yaml
endpoints:
  - "https://etcd.example:2379"
prefix: "/aibrix/endpoints/"
dialTimeout: "5s"
username: gateway
password: "replace-with-your-password"
tlsCA: ca.pem
# Supply both fields for mutual TLS:
# tlsCert: gateway-client.pem
# tlsKey: gateway-client-key.pem
```

The file requires at least one HTTP(S) endpoint. `prefix` defaults to
`/aibrix/endpoints/`, and `dialTimeout` defaults to `5s`. Unknown fields and
invalid types are rejected. Credentials belong in the config file, not endpoint
URLs or CLI arguments. Restrict access to files containing credentials, for
example with `chmod 600`. Certificate paths are relative to the configuration
file unless absolute. TLS settings require HTTPS endpoints, and `tlsCert` and
`tlsKey` must be supplied together. Server certificates and hostnames are
verified; omitting `tlsCA` uses the system trust roots.

Each key below the prefix represents one worker. Its value is a JSON object:

| Field | Meaning |
| --- | --- |
| `model` | Required model name used for routing. |
| `address` | Required worker `host:port`; use `[IPv6]:port` for IPv6. Port must be 1–65535. |
| `engine` | Optional engine label, such as `vllm`, `sglang`, or `trtllm`. |
| `role` | Optional `prefill` or `decode`; requires `roleset`. |
| `roleset` | Optional P/D pairing group; requires `role`. |

Workers or an external controller own these registrations; the gateway only
reads and watches them. Register each worker under a stable, unique key.
Addresses must be reachable from the gateway. The bundled standalone Envoy uses
`ORIGINAL_DST`, so register numeric IPv4 or IPv6 addresses when using that
configuration; DNS names require a downstream proxy configured to resolve them.

For a local etcd instance, register, update, and delete a worker:

```bash
export ETCDCTL_API=3
export ETCDCTL_ENDPOINTS=http://127.0.0.1:2379

etcdctl put /aibrix/endpoints/worker-0 \
  '{"model":"Qwen/Qwen3.5-4B","address":"127.0.0.1:8000","engine":"vllm"}'

# Updating the same key replaces its registration without restarting the gateway.
etcdctl put /aibrix/endpoints/worker-0 \
  '{"model":"Qwen/Qwen3.5-4B","address":"127.0.0.1:8001","engine":"vllm"}'

etcdctl del /aibrix/endpoints/worker-0
```

A P/D pair uses two keys with the same model and roleset:

```bash
etcdctl put /aibrix/endpoints/prefill-0 \
  '{"model":"Qwen/Qwen2.5-72B","address":"127.0.0.1:8100","engine":"vllm","role":"prefill","roleset":"pair-0"}'
etcdctl put /aibrix/endpoints/decode-0 \
  '{"model":"Qwen/Qwen2.5-72B","address":"127.0.0.1:8200","engine":"vllm","role":"decode","roleset":"pair-0"}'
```

To remove a registration automatically when its owner stops renewing it, attach
an etcd lease:

```bash
lease_id=$(etcdctl lease grant 30 | awk '{print $2}')
etcdctl put /aibrix/endpoints/worker-0 \
  '{"model":"Qwen/Qwen3.5-4B","address":"127.0.0.1:8000","engine":"vllm"}' \
  --lease="$lease_id"

# Run this in the worker's registration process while it is healthy.
etcdctl lease keep-alive "$lease_id"
# To remove its registrations immediately, stop keep-alive and revoke the lease:
# etcdctl lease revoke "$lease_id"
```

If renewal stops, etcd removes the key when the lease expires and the provider
removes its route after observing the delete. A lease tracks the registration
owner's liveness; the provider does not probe the inference endpoint. Registrations
without leases remain until explicitly deleted.

At startup the provider lists the prefix and populates the cache before
`Watch` returns. It starts watching at the snapshot revision plus one, so changes
between the list and watch are not lost. Disconnects are retried; a compacted
watch revision triggers a new snapshot and reconciliation, including removal of
registrations deleted during the outage. Until etcd is reachable again, routing
uses the last observed registrations.

An engine-only change updates the existing endpoint. Changing a model, address,
role, or roleset removes the old routing identity and adds the new one. An invalid
registration is excluded without interrupting other keys; if it replaces a
previously valid value, that old route is removed. A later valid value restores
the key. Closing the gateway stop channel stops the watch and closes the etcd
client.

Integration tests using `AIBRIX_TEST_ETCD_ENDPOINT` require a dedicated,
disposable etcd instance: the live compaction test compacts that instance's
history. Never point this variable at a production or shared etcd instance.

### KubernetesProvider (`kubernetes.go`)

Watches Pods and ModelAdapters via K8s informers. This is the default when no `DiscoveryProvider` is set in `InitOptions`.

The handler is wired directly into K8s informer callbacks — events flow from the start, including during the initial list phase. No intermediate channel, no buffer, no snapshot replay.

1. Registers handler on Pod and ModelAdapter informers.
2. Starts informers — initial objects arrive via `AddFunc` as part of the informer's list+watch.
3. Waits for cache sync (`WaitForCacheSync`).
4. Post-sync reconcile: re-emits all ModelAdapters as `EventAdd` to fix ordering (Pod and ModelAdapter informers list concurrently, so an adapter may arrive before its pods).
5. Returns — informer callbacks continue delivering ongoing changes.

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
    │ Static     │  │ Kubernetes │  │ Etcd         │
    │ Provider   │  │ Provider   │  │ Provider     │
    │            │  │            │  │              │
    │ YAML file  │  │ Informers  │  │ Snapshot     │
    │ → Load()   │  │ → Watch()  │  │ + watch      │
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

- **Consul provider** — another backend for non-K8s service discovery.
- **Platform-agnostic `Endpoint` type** — replace `*v1.Pod` as the internal representation to remove the K8s dependency from routing algorithms.
