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
- **Consul/etcd providers**: starts a watch/poll loop in a goroutine, calls handler from that goroutine.

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

### KubernetesProvider (`kubernetes.go`)

Watches Pods and ModelAdapters only in `InitOptions.InstallationNamespace`. This
namespace is required when no `DiscoveryProvider` is set. Kubernetes gateways
must pass `--installation-namespace=<workload-namespace>`; there is no default or
cluster-wide fallback. Standalone file discovery does not require this setting.

Startup rejects empty or invalid namespace names, verifies that the namespace
exists, and requires Pod and ModelAdapter LIST and WATCH access there. Initial
watch checks and cache synchronization share a one-minute deadline. No gateway
listeners open until initialization succeeds. Failed initialization stops its
informers; after readiness, informers reconnect within the same namespace until
the caller closes `stopCh`.

This uses the existing Kubernetes namespace validator, typed namespaced clients and shared
informers, with standard Go contexts for cancellation and deadlines. No
new dependency or custom Kubernetes protocol implementation is needed.

Pod model names come from the nonempty `model.aibrix.ai/name` label, falling back
to the annotation of the same name. Values are preserved exactly, including `/`
in annotations. ModelAdapters use their Kubernetes object name. Routing and the
gateway's `GET /v1/models` handler both use the same cache index. Controller-manager
cache initialization is separate and remains cluster-wide.

The handler is wired directly into K8s informer callbacks — events flow from the start, including during the initial list phase. No intermediate channel, no buffer, no snapshot replay.

1. Validates the namespace and checks namespace GET and resource WATCH access.
2. Registers handler on Pod and ModelAdapter informers.
3. Starts informers — initial objects arrive via `AddFunc` as part of the informer's list+watch.
4. Waits for the registered handlers to process the initial lists.
5. Post-sync reconcile: re-emits all ModelAdapters as `EventAdd` to fix ordering (Pod and ModelAdapter informers list concurrently, so an adapter may arrive before its pods).
6. Returns — informer callbacks continue delivering ongoing changes.

### Deployment and migration

The default manifest passes the gateway Pod's namespace using the existing
`POD_NAMESPACE` Downward API variable. It grants Pod and ModelAdapter GET, LIST,
and WATCH through a discovery Role and RoleBinding in that namespace. A separate
HTTPRoute reader Role and RoleBinding grant GET in `aibrix-system`, where the
model-router controller creates routes and the gateway validates them. Moving
workload discovery does not move these route permissions. A separate ClusterRole
permits GET of the single installation Namespace object; it grants no namespace
LIST/WATCH or workload access. This narrow check makes a misspelled, nonexistent
namespace a startup error even though Kubernetes can return an empty Pod list for
it.

For a workload namespace different from the gateway's namespace, set
`--installation-namespace` explicitly, place the discovery Role and RoleBinding
there, and explicitly set the binding's subject to the gateway ServiceAccount's
rendered name and namespace (`aibrix-gateway-plugins` in `aibrix-system` for the
default installation). Patch both subject fields when moving the RoleBinding in
a Kustomize overlay: changing its namespace can prevent Kustomize from resolving
the original ServiceAccount reference. Set the namespace-reader ClusterRole's
`resourceNames` to that exact workload namespace.
Leave the HTTPRoute reader Role and RoleBinding in `aibrix-system`. An overlay
that changes the workload namespace must also replace that `resourceNames` value.

For Helm, `gatewayPlugin.installationNamespace` selects the workload namespace;
its empty default resolves to the release namespace. The chart always passes a
concrete namespace to the gateway and grants matching discovery permissions in
both Envoy Gateway and Envoy sidecar modes. Install the control plane in
`aibrix-system`; changing its namespace is not supported by the existing
model-router controller. To use a separate workload namespace, create it before
installing, then pass `--set gatewayPlugin.installationNamespace=workloads` to
`helm install` or `helm upgrade`. An invalid namespace value fails chart
validation. The runtime still requires an existing, accessible namespace.

The quickstart models, Services, and referenced Secrets use `aibrix-system`.
Change them together if you select another workload namespace.

When upgrading an existing default installation, apply the new permissions and
remove the old cluster-wide grants before declaring discovery confined:

```sh
kubectl delete clusterrolebinding aibrix-gateway-plugins-rolebinding
kubectl delete clusterrole aibrix-gateway-plugins-role
```

Use the installed name prefix if it differs from `aibrix-`. Applying a Role does
not remove an existing ClusterRoleBinding. Audit any additional bindings granted
to the gateway ServiceAccount. Workloads outside the configured namespace will
no longer be discovered; move them or select their namespace before rollout.

Helm upgrades remove the chart's previous `aibrix-gateway-plugins` ClusterRole
and ClusterRoleBinding when replacing them with namespaced resources. Verify
that obsolete or manually added cluster-wide grants are gone before declaring
discovery confined.

The packaged `/v1/models` HTTPRoute targets the gateway HTTP port (8080).
Both `/v1/models` and `/v1/models/` use the same handler. The Helm Envoy sidecar
routes these requests to the local gateway HTTP port with external processing
disabled. In Envoy Gateway mode, `/v1/files` and `/v1/batches` continue to target
the metadata service. These changes
confine gateway discovery only; they do not establish multi-installation safety.

### Verification

Focused tests cover namespace validation, denied access, bounded startup,
foreign-event rejection, exact model names, routing, and the model-list handler:

```sh
go test -race ./pkg/cache/discovery ./pkg/cache ./pkg/plugins/gateway ./cmd/plugins
```

With `kubectl`, `helm`, and envtest binaries available, test the rendered permissions
against a local Kubernetes API server:

```sh
KUBEBUILDER_ASSETS=/path/to/envtest/binaries go test -race ./test/integration/discovery
```

## Architecture

```text
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
