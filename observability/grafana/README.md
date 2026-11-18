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

`gateway_pod_outstanding_requests` reports each gateway's local count of
unfinished requests routed to an inference pod. Scrape each gateway plugin
replica's `/metrics` endpoint once. The shipped ServiceMonitor selects Services
labeled `app: gateway-plugins` in `aibrix-system`. For Helm, add that label to the
gateway plugin Service and set `spec.namespaceSelector.matchNames` to the release
namespace. [Gateway plugin ServiceMonitor](../monitor/service_monitor_gateway_plugin.yaml)

### PromQL aggregation

With the shipped ServiceMonitor, `namespace` and `pod` identify the gateway
scrape target. Group inference pods by `exported_namespace` and `pod_name`.
[Prometheus scrape configuration](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#scrape_config)

```promql
# Total outstanding requests per inference pod across gateway replicas
sum by (exported_namespace, pod_name) (gateway_pod_outstanding_requests)

# Outstanding requests per gateway and inference pod
sum by (gateway_pod, exported_namespace, pod_name) (gateway_pod_outstanding_requests)

# Total by inference namespace and model, for single-model pods only
sum by (exported_namespace, model) (gateway_pod_outstanding_requests)
```

With `honorLabels: true` on the `metrics` endpoint, replace
`exported_namespace` with `namespace`. This setting affects all metrics on
the endpoint. `spec.overrideHonorLabels: true` on the Prometheus resource
forces the default behavior.
[Prometheus Operator API reference](https://prometheus-operator.dev/docs/api-reference/api/#monitoring.coreos.com/v1.CommonPrometheusFields)

Keep your cluster label when querying multiple clusters. During a rolling
upgrade from `target_namespace` and `target_pod`, normalize both label sets
before summing across gateway replicas.

### Metric semantics

- `gateway_pod` identifies the reporting gateway. On `/metrics`, `namespace`
  and `pod_name` identify the inference pod.
- `model` comes from the pod's `model.aibrix.ai/name` label, then its annotation,
  and defaults to empty. The gauge counts requests across the pod's models and
  adapters.
- Each gateway emits one series per cached inference pod, with zero for idle
  pods. Removing a pod drops its series even if requests are still pending.
- In prefill/decode deployments, requests count toward the decode pod. This
  gauge shares inference pod labels with `pd_token_load_decode_tokens`, so
  dashboards can show request counts and estimated prompt-token load for the
  same decode pod. Use `gateway_prefill_outstanding_requests` for prefill counts
  per gateway.

## Query Cross-Gateway Running-Request Estimates

`realtime_num_requests_running` estimates in-flight requests for an inference
pod by adding the reporting gateway's local count to recent counts available
from other gateways. The value is a pod total across all models and adapters.
`model` comes from the pod's `model.aibrix.ai/name` label, falling back to the
same annotation or an empty value.

Unlike the local counts in `gateway_pod_outstanding_requests`, these estimates
overlap across reporting gateways and can differ with snapshot freshness.
Do not sum them across gateway replicas: doing so can double-count requests.
Select a reporting gateway with `gateway_pod` when inspecting an estimate.
Use this gauge to inspect the cross-gateway load estimate available to a
particular gateway, including its remote-freshness fallbacks. To count locally
tracked unfinished requests across gateways, sum
`gateway_pod_outstanding_requests` instead.

This gauge follows the shared gateway metric label schema: on the raw plugin
`/metrics` endpoint, `namespace` and `pod` identify the inference pod, and
`gateway_pod` identifies the reporting gateway. It retains this schema rather
than adopting the separate labels of `gateway_pod_outstanding_requests`;
aligning those schemas requires a separate compatibility decision.

With the shipped ServiceMonitor and effective `honor_labels: false`, the
gateway's `namespace` and `pod` scrape target labels collide with the inference
pod labels. Prometheus stores the inference pod identity as
`exported_namespace` and `exported_pod`. Each collision is resolved independently.
With effective `honor_labels: true`, the sample's `namespace` and `pod` labels
remain the inference pod identity. For example, when both collisions occur
under the default scrape policy:

```promql
realtime_num_requests_running{exported_namespace="inference", exported_pod="model-0", gateway_pod="gateway-0"}
```

The collector waits until its active remote-count source is initialized:
the snapshot must be published or, when the remote reader is enabled, the
pod must have been read at least once. Once remote counts have been read,
expired contributions are excluded at scrape time; the gauge can therefore
report only the local count under the same metric name. An initialized source
with no remote requests still produces a sample, including a zero total.

The gateway exports this gauge through a pod collector. At scrape
time, its running-request calculation adds the local atomic counter to fresh
remote contributions for ready, non-quarantined pods currently in the cache.
Pods without an IP or with Ready other than true, terminating pods, and
draining pods are omitted, matching the sync path's eligibility checks.
Quarantined pods are also omitted until the quarantine ends. Omission does
not mean zero load.
Removed pods and old model label combinations stop appearing on the next
scrape after the cache processes their removal or update.
A pod update removes and re-adds the cache entry, so a concurrent scrape can
temporarily omit the pod. Subsequent scrapes use the replacement's labels and
do not need another metric sync once the remote source is initialized.
A removed pod stops contributing even if some of its requests remain unfinished;
this drop does not mean those requests completed.

Collection performs only in-memory reads and does not depend on successful
engine metrics fetches or metric-record timestamps. Remote freshness uses the
active reader's age limit or the snapshot's per-contribution age limit; there
is no additional age limit on the combined estimate. When remote data expires,
the local count remains visible, including zero. Routing cache update and
freshness policies are unchanged.
Label values are cached per pod object and rebuilt when a pod update replaces
it. Each scrape still visits the cached pods, so collection work grows with
the pod count and repeats for each reporting gateway.

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
