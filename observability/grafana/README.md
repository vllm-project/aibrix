# AIBrix Grafana Dashboards

Import these JSON files into Grafana after Prometheus is scraping AIBrix
metrics (typically via `kube-prometheus-stack` and the ServiceMonitors under
[`../monitor/`](../monitor/)).

## Dashboards

| File | Purpose |
|------|---------|
| `AIBrix_Control_Plane_Runtime_Dashboard.json` | Controller-runtime reconcile / workqueue / process resources |
| `AIBrix_Envoy_Gateway_Dashboard.json` | Envoy Gateway proxy traffic |
| `AIBrix_vLLM_Engine_Dashboard.json` | vLLM engine latency and throughput |
| `AIBrix_ModelClaim_Runtime_Dashboard.json` | ModelClaim lifecycle + warm-pool KV / HBM density |

Official import URLs are also listed in
[Observability](../../docs/source/production/observability.rst).

For requests with an identified model, gRPC Canceled preserves an already-recorded
response outcome. Otherwise, completed requests record `gateway_request_success`/200
and incomplete requests record `client_cancelled`/499. Outside shutdown,
incomplete EOF without a recorded outcome records `client_cancelled_eof`/499.
These are metric classifications; the Canceled branch still returns gRPC Canceled.
A completed ImmediateResponse failure (for example, malformed non-streaming
upstream JSON) also prevents a later success increment at proactive completion
or in the completed-EOF fallback. A recorded response outcome also suppresses
later incomplete-EOF, gRPC receive-error, and pre-receive context-cancel counters.
Send-failure and shutdown accounting are unchanged.

## Import

1. Open Grafana → **Dashboards** → **New** → **Import**.
2. Upload a JSON file from this directory (or paste the raw GitHub URL).
3. Select your Prometheus datasource when prompted (`DS_PROMETHEUS`).

## Query Gateway Pod Outstanding Requests

Scrape each gateway plugin replica's `/metrics` endpoint once to collect
`gateway_pod_outstanding_requests`, using the shipped
[`service_monitor_gateway_plugin.yaml`](../monitor/service_monitor_gateway_plugin.yaml).
This recipe assumes the Kustomize installation in `aibrix-system`: the monitor
selects Services labeled `app: gateway-plugins`. For a Helm installation, add
`app: gateway-plugins` to the gateway plugin Service's `metadata.labels` to use
this monitor, and set `spec.namespaceSelector.matchNames` to the release's
namespace. The chart's default Service labels do not match this selector.
The Envoy `/stats/prometheus` endpoint
configured in [`service_monitor_gateway.yaml`](../monitor/service_monitor_gateway.yaml)
does not expose this metric.

This Gauge counts requests tracked locally by a gateway that were routed to a
target inference pod and have not been finalized. It excludes requests sent
directly to the engine or still waiting for pod selection. It is not the engine's
GPU-running request count or the estimate from `realtime_num_requests_running`.

Labels `target_namespace` and `target_pod` identify the target inference pod;
`gateway_pod` identifies the reporting gateway. `model` comes from the pod's
`model.aibrix.ai/name` label, falling back to the same annotation or an empty
value. The count is a pod total across all models and adapters, not a per-model
request count.

For prefill/decode (PD) disaggregated models, the ledger tracks the selected
decode pod, not the prefill pod. Prefill-only pods therefore emit zero even
while serving prefill requests; this metric cannot diagnose load imbalance
between prefill pods. `gateway_prefill_outstanding_requests` tracks prefill
requests separately per gateway, without a target pod label.

Cardinality is one series per gateway per cached inference pod, including idle
pods: G gateways caching the same N pods emit G × N series. Each scrape visits
all cached pods, so collection work and response size grow with N.

When a pod is removed from the cache, its series disappears even if unfinished
requests remain in the recently deleted pod snapshot during the grace period.
Pod deletion can therefore cause a dip in the aggregate without requests having
drained; a disappearing series is not evidence of request completion.

The shipped ServiceMonitor uses Prometheus' default `honor_labels: false`.
The operator adds the gateway's `namespace` and `pod` target labels;
`target_namespace` and `target_pod` remain the inference pod's identity without
colliding with those labels. Using `namespace` and `pod` for the inference pod
would rename them to `exported_namespace` and `exported_pod` at ingestion.
The `model` label follows the existing gateway metric convention.

```promql
# Total outstanding requests per target pod across gateway replicas
sum by (target_namespace, target_pod) (gateway_pod_outstanding_requests)

# Identify which gateway retains a count after traffic has drained
sum by (gateway_pod, target_namespace, target_pod) (gateway_pod_outstanding_requests)

# Aggregate by model only for single-model pods
sum by (target_namespace, model) (gateway_pod_outstanding_requests)
```

For multiple clusters, also retain your cluster label.

A persistent nonzero value reflects the gateway's local accounting state; it
does not prove that the engine is still executing requests.

Gateway cache initialization through `InitWithOptions` is guarded by
`sync.Once` and supports one store per process. The collector retains that
store until it is unregistered after `stopCh` closes. Registration failures are
logged and initialization continues; a duplicate registration leaves the
existing collector attached to its original store. In-process store replacement
is unsupported; tests that initialize collectors directly must use an isolated
registry or wait for the previous collector to unregister before registering
another store in the same registry.

## ModelClaim Runtime Dashboard

### Required metrics

**Controller** (controller-manager `/metrics`, scraped by
`service_monitor_controller_manager.yaml`):

| Metric | Labels |
|--------|--------|
| `aibrix_modelclaim_desired_replicas` | `namespace`, `model` |
| `aibrix_modelclaim_ready_replicas` | `namespace`, `model` |
| `aibrix_modelclaim_activating` | `namespace`, `model` |
| `aibrix_modelclaim_activation_total` | `namespace`, `model`, `result` |
| `aibrix_modelclaim_no_ready_duration_seconds` | `namespace`, `model` |
| `aibrix_modelclaim_pool_policy_valid` | `namespace`, `deployment` |
| `aibrix_modelclaim_pool_policy_evaluations_total` | `namespace`, `pool`, `result`, `reason` |
| `aibrix_modelclaim_pool_policy_actions_total` | `namespace`, `pool`, `action`, `result`, `reason` |

**Runtime sidecar** (warm-pool `aibrix-runtime` `/metrics` on port `runtime`):

| Metric | Labels |
|--------|--------|
| `aibrix:modelclaim_models_resident` | scrape labels only (`namespace`, `pod`, `pool`) |
| `aibrix:modelclaim_kv_used_bytes` | `model` (+ scrape labels) |
| `aibrix:modelclaim_kv_total_bytes` | `model` (+ scrape labels) |
| `aibrix:modelclaim_hbm_peak_bytes` | `model` (+ scrape labels) |
| `aibrix:modelclaim_engine_state` | `phase` (+ scrape labels) |
| `aibrix:modelclaim_engine_restart_attempts_total` | `model` (+ scrape labels) |
| `aibrix:modelclaim_engine_restart_budget_exhausted_total` | `model` (+ scrape labels) |
| `aibrix:modelclaim_engine_re_adoptions_total` | `result` (+ scrape labels) |
| `aibrix:modelclaim_kv_limit_operations_total` | `model`, `result` (+ scrape labels) |
| `aibrix:modelclaim_kv_limit_requested_bytes` | `model` (+ scrape labels) |
| `aibrix:modelclaim_kv_limit_applied_bytes` | `model` (+ scrape labels) |
| `aibrix:modelclaim_lifecycle_operations_total` | `model`, `action`, `result` (+ scrape labels) |
| `aibrix:modelclaim_lifecycle_operation_duration_seconds` | `model`, `action`, `result` (+ scrape labels) |

Note the naming split: Go controller metrics use underscores
(`aibrix_modelclaim_*`); the Python runtime collector uses colons
(`aibrix:modelclaim_*`).

### Enable runtime scraping

```bash
# ServiceMonitor (attaches bounded `pool` label from claim.model.aibrix.ai/pool)
kubectl apply -f observability/monitor/service_monitor_modelclaim_runtime.yaml

# Sample warm pool includes a Service labeled aibrix.ai/metrics=modelclaim-runtime
kubectl apply -f samples/modelclaim/warm-runtime-pool.yaml
```

Dashboard variables cascade as **namespace → pool → pod → model**. The `pool`
label is produced by the ServiceMonitor relabel rule (Prometheus cannot use
`claim.model.aibrix.ai/pool` as a label name). The **Pool Policy Valid** tile
is keyed by `namespace`/`deployment` (not `pool`/`pod`/`model`), so it follows
only the `namespace` variable and reports the worst-case validity across
warm-pool Deployments in the selected namespace(s).

### Operational panels

- **Engine Operations** covers availability, engine state, recovery, and
  sleep/wake activity.
- **Pool Policy & KV Control** covers policy decisions, actions, and KV limits.

Operational labels use bounded values; detailed errors and identifiers remain
in logs and Kubernetes Events. No-ready duration is zero while an engine is
ready, and KV-limit gauges disappear when their model is removed.
