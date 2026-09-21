# ModelWarmup

`ModelWarmup` preloads container images on explicitly named nodes or nodes
selected by labels. It does not download model weights, start an inference
engine, or gate workloads.

Apply the sample after labeling at least one target node:

```sh
kubectl label node <node> resource-pool.aibrix.ai/name=latency-pool
kubectl apply -f samples/modelwarmup/modelwarmup.yaml
kubectl get modelwarmup runtime-image-warmup -w
```

Each image requires a command that exits safely after Kubernetes pulls it. Use
immutable image digests in production when cache reproducibility matters.
`Succeeded` is an observation at completion time; kubelet image garbage
collection can evict the image later.
