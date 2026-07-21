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
| `aibrix_modelclaim_pool_policy_valid` | `namespace`, `deployment` |

**Runtime sidecar** (warm-pool `aibrix-runtime` `/metrics` on port `runtime`):

| Metric | Labels |
|--------|--------|
| `aibrix:modelclaim_models_resident` | scrape labels only (`namespace`, `pod`, `pool`) |
| `aibrix:modelclaim_kv_used_bytes` | `model` (+ scrape labels) |
| `aibrix:modelclaim_kv_total_bytes` | `model` (+ scrape labels) |
| `aibrix:modelclaim_hbm_peak_bytes` | `model` (+ scrape labels) |

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

### Not available yet

The following operator questions need metrics from
[#2455](https://github.com/vllm-project/aibrix/issues/2455) and are intentionally
omitted from this dashboard:

- Sleeping / failed phase overview
- Engine restart attempts and restart-budget exhaustion
- Agent re-adoption outcomes
- Pool-policy applied / skipped / failed actions
- Sleep / wake activity and no-ready duration
- HBM headroom (free) remaining

Use the **Investigation Aids** panel for Kubernetes Events and log recipes until
those metrics land.
