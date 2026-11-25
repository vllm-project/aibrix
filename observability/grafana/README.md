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

On `/metrics`, labels `namespace` and `pod` identify the target inference
pod; `gateway_pod` identifies the reporting gateway. `model` comes from the pod's
`model.aibrix.ai/name` label, falling back to the same annotation or an empty
value. The count is a pod total across all models and adapters, not a per-model
request count. This uses the same inference pod identity labels as the shared
gateway metric exporter.

For prefill/decode (PD) disaggregated models, the ledger tracks the selected
decode pod, not the prefill pod. Prefill-only pods therefore emit zero even
while serving prefill requests; this metric cannot diagnose load imbalance
between prefill pods. `gateway_prefill_outstanding_requests` tracks prefill
requests separately per gateway, without a target pod label.
`pd_token_load_decode_tokens` provides estimated prompt-token load for decode
pods, but uses `pod_name` for their names; joining it with this metric requires
label normalization.

Cardinality is one series per gateway per cached inference pod, including idle
pods: G gateways caching the same N pods emit G × N series. Each scrape visits
all cached pods, so collection work and response size grow with N.

When a pod is removed from the cache, its series disappears even if unfinished
requests remain in the recently deleted pod snapshot during the grace period.
Pod deletion can therefore cause a dip in the aggregate without requests having
drained; a disappearing series is not evidence of request completion.

The shipped ServiceMonitor uses Prometheus' default `honor_labels: false`.
The operator adds the gateway's `namespace` and `pod` target labels. At ingestion,
the inference pod's labels become `exported_namespace` and `exported_pod`;
`namespace` and `pod` identify the gateway scrape target. `gateway_pod` still
identifies the reporting gateway. The existing
[gateway plugin dashboard](AIBrix_Envoy_Gateway_Plugins_Dashboard.json) uses
`exported_pod` for the same inference pod dimension. The `model` label follows
the existing gateway metric convention.
[Prometheus scrape configuration](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#scrape_config)

```promql
# Total outstanding requests per inference pod across gateway replicas
sum by (exported_namespace, exported_pod) (gateway_pod_outstanding_requests)

# Identify which gateway retains a count after traffic has drained
sum by (gateway_pod, exported_namespace, exported_pod) (gateway_pod_outstanding_requests)

# Aggregate by model only for single-model pods
sum by (exported_namespace, model) (gateway_pod_outstanding_requests)
```

These are gateway-local counts, so sum them across gateway replicas rather
than using `max` to deduplicate repeated engine observations. Scrape each
replica once to avoid double-counting.

With effective `honorLabels: true` on the `metrics` endpoint, replace
`exported_namespace` and `exported_pod` with `namespace` and `pod` in these
queries. This setting affects all metrics on the endpoint.
`spec.overrideHonorLabels: true` on the Prometheus resource forces the default
behavior. Conflicts are resolved independently for each label: custom scrape
configurations that do not supply a conflicting target label keep the original
inference label name. Use the labels produced by your scrape configuration;
this change does not modify the ServiceMonitor settings.
[Prometheus Operator API reference](https://prometheus-operator.dev/docs/api-reference/api/#monitoring.coreos.com/v1.CommonPrometheusFields)

For multiple clusters, also retain your cluster label.

A persistent nonzero value reflects the gateway's local accounting state; it
does not prove that the engine is still executing requests.

## Query Gateway Per-Pod Output Rate

The same endpoint exports, per target pod and with the same labels, what each
gateway measured from the requests it routed:

| Metric | Meaning |
|---|---|
| `gateway_pod_output_tokens_per_request_second` | Output tokens per second of one request's time on the pod, smoothed over ~1-minute windows. |
| `gateway_pod_output_rate_age_seconds` | Seconds since that value was last updated. |
| `gateway_pod_requests_without_output_tokens_fraction` | Share of the pod's requests that finished without output tokens in the last window in which any finished. |
| `gateway_pod_requests_without_output_tokens_fraction_age_seconds` | Seconds since that value was last updated. |

No router reads these values. Read them with their age:

- **They keep their last value.** The rate is only updated by a window in which
  `AIBRIX_OUTPUT_RATE_MIN_SAMPLES` (default 8) requests with output tokens
  completed, and the fraction only by a window in which a request finished. An
  idle or low-traffic pod reports an old value until it is deleted.
- **The fraction is not a failure rate.** A streaming request sent without
  `stream_options.include_usage` reports no output tokens, so it counts here and
  not in the rate.
- **The rate covers the whole request**, including queueing and prompt
  processing, so pods serving different request mixes read differently on the
  same hardware.
- **One value per pod.** On a pod that serves several models it covers all of them.

```promql
# Per-request output rate per pod, only where it was updated in the last 2 minutes
gateway_pod_output_tokens_per_request_second
  and on (gateway_pod, target_namespace, target_pod) gateway_pod_output_rate_age_seconds < 120
```

Gateway cache initialization through `InitWithOptions` is guarded by
`sync.Once` and supports one store per process. The collector retains that
store until it is unregistered after `stopCh` closes. Registration failures are
logged and initialization continues; a duplicate registration leaves the
existing collector attached to its original store. In-process store replacement
is unsupported; tests that initialize collectors directly must use an isolated
registry or wait for the previous collector to unregister before registering
another store in the same registry.

The collector registers with `prometheus.DefaultRegisterer`. Wrapping the
registerer with a `namespace` or `pod` const label is unsupported: it duplicates
the collector's variable labels, so registration fails and the metric is not
exposed.
This registration-time conflict is separate from Prometheus' scrape-time label
handling described above.

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
