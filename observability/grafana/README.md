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

## Import

1. Open Grafana → **Dashboards** → **New** → **Import**.
2. Upload a JSON file from this directory (or paste the raw GitHub URL).
3. Select your Prometheus datasource when prompted (`DS_PROMETHEUS`).

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
# ServiceMonitor (attaches bounded `pool` label from pool.aibrix.ai/name)
kubectl apply -f observability/monitor/service_monitor_modelclaim_runtime.yaml

# Sample warm pool includes a Service labeled aibrix.ai/metrics=modelclaim-runtime
kubectl apply -f samples/modelclaim/warm-runtime-pool.yaml
```

Dashboard variables cascade as **namespace → pool → pod → model**. The `pool`
label is produced by the ServiceMonitor relabel rule (Prometheus cannot use
`pool.aibrix.ai/name` as a label name). The **Pool Policy Valid** tile is keyed
by `namespace`/`deployment` (not `pool`/`pod`/`model`), so it follows only the
`namespace` variable and reports the worst-case validity across warm-pool
Deployments in the selected namespace(s).

### Operational panels

- **Engine Operations** covers availability, engine state, recovery, and
  sleep/wake activity.
- **Pool Policy & KV Control** covers policy decisions, actions, and KV limits.

Operational labels use bounded values; detailed errors and identifiers remain
in logs and Kubernetes Events. No-ready duration is zero while an engine is
ready, and KV-limit gauges disappear when their model is removed.
