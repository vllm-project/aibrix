# ModelClaim Samples

These samples run two small Qwen models in one warm GPU Pod, in the `default`
namespace:

- `warm-runtime-pool.yaml` creates a warm pool of one Pod with one GPU, and a
  Service for the runtime's metrics.
- `modelclaims.yaml` creates two ModelClaims that share the Pod's GPU.

Each claim declares a 6 GiB footprint and a 1 GiB KV floor on the GPU, so the
GPU needs at least 14 GiB, and the Pod requests 16 GiB of host memory. From the
repository root:

```bash
kubectl apply -f samples/modelclaim/warm-runtime-pool.yaml
kubectl -n default rollout status deployment/warm-runtime-pool --timeout=15m
kubectl apply -f samples/modelclaim/modelclaims.yaml
kubectl -n default get modelclaims -w
```

Both claims become `Active` once their engines are ready. The
[ModelClaim guide](https://aibrix.readthedocs.io/latest/features/model-serving/model-claim.html)
explains how to send requests and let idle models sleep, what the samples set,
and the limitations of ModelClaim. The
[validation runbook](../../development/tutorials/modelclaim/manual-validation-runbook.md)
checks the feature on real GPUs with these samples.

For ModelClaim routes, the Gateway independently scrapes each active binding's
engine port, even when the Pod sets `model.aibrix.ai/metric-port`. This path
collects model-scoped metrics directly from the engine. Pod-scoped engine
metrics such as KV-cache capacity and PromQL queries belong to ordinary Pod
collection. A pure warm Pod with no active bindings has no scrape targets.
When a binding sleeps, is removed, or changes ports, the Gateway drops its old
samples and exported series.

A Pod advertising a model through its Pod metadata and carrying no ModelClaim
route annotations uses its `model.aibrix.ai/metric-port` label or port `8000`, even
when it carries the warm-pool label.

Delete the claims before the pool, so that the controller can stop their
engines:

```bash
kubectl delete -f samples/modelclaim/modelclaims.yaml
kubectl delete -f samples/modelclaim/warm-runtime-pool.yaml
```

## Requesting a stable inference port

Add `spec.servingPort: 18081` to a ModelClaim to request that port on every
selected Pod. Omit it to keep automatic allocation. The port is immutable,
like the engine configuration; create a new claim to change it. Valid values
are 1 through 65535, excluding runtime management port 8080. Do not set `--port` in
`spec.engineConfig.args`; new claims reject it and direct you to `servingPort`.
Existing claims using that argument can still receive metadata updates.

The controller skips occupied ports reported by runtime snapshots, including
sleeping and starting engines. The runtime checks again before launching.
An explicit pre-launch `409` response with reason `PortInUse` lets the
controller release its provisional placement and try another Pod. If no
placement is available and ports conflict, `Scheduled=False` reports
`PortInUse`; the claim is retried. An ambiguous timeout does not trigger a
second launch elsewhere. An OS port check cannot reserve the port against
unrelated processes that bind it after the check.

Upgrade both controller and runtime sidecars for conflict-aware placement.
Older runtime images do not provide the typed pre-launch conflict contract.

Serving ports do not fix same-name ownership across
claims: use distinct served model names. A different claim with the same
served name but another port can still be rejected by the runtime rather
than retried as a port conflict. A rejected launch can leave neighboring
engines at the KV limits planned for it until the next card-division pass.
