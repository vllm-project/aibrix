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

Delete the claims before the pool, so that the controller can stop their
engines:

```bash
kubectl delete -f samples/modelclaim/modelclaims.yaml
kubectl delete -f samples/modelclaim/warm-runtime-pool.yaml
```

## Reuse models prepared by ModelWarmup

The pool explicitly mounts the native HF Hub cache and the existing object-store
downloader directory. Use the [model-cache preparation sample](../modelwarmup/model-cache.yaml)
on the same authorized nodes before creating the **unchanged** ModelClaims.
Both the preparation image and the pool runtime must include this feature.

The runtime discovers supported complete caches in the background and merges
URL hints into its existing `cached_artifacts` snapshot field. Existing placement
then prefers a feasible cached Pod; caches never bypass capacity or pool
constraints. Discovery refreshes every 10 seconds and is eventually visible,
not a guarantee that the first Claim after a Warmup succeeds will see a hint.
No new ModelClaim fields, controller changes, or activation protocol are needed.

For S3/TOS/GCS Claims, configure the runtime with the same endpoint/region and
normal backend credentials as the prepare Job. The existing downloader still
checks remote metadata and skips payload transfer only when its ETag and size
checks match. Cache hits are not an offline-access guarantee. See the
[preparation guide](../modelwarmup/model-cache.md) for layout, permission, and
version limitations.
