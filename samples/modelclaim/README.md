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
