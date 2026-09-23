# ModelWarmup

`ModelWarmup` preloads container images on explicitly named nodes or nodes
selected by labels. It does not download model weights, start an inference
engine, or gate workloads.

Apply the sample after labeling at least one target node:

```sh
kubectl label namespace default resource-pool.aibrix.ai/name=latency-pool
kubectl label node <node> \
  resource-pool.aibrix.ai/name=latency-pool \
  model.aibrix.ai/warmup-enabled=true
kubectl apply -f samples/modelwarmup/modelwarmup.yaml
kubectl get modelwarmup runtime-image-warmup -w
```

The namespace and node ``resource-pool.aibrix.ai/name`` values form the
authorization boundary. ``model.aibrix.ai/warmup-enabled=true`` is a separate
explicit node opt-in. The controller writes ``model.aibrix.ai/warmup`` and
``model.aibrix.ai/revision`` labels plus the
``model.aibrix.ai/target-node`` Job annotation; users should not set those
three keys.

Each image requires a command that exits safely after Kubernetes pulls it. Use
immutable image digests in production when cache reproducibility matters.
The spec is immutable. A ``Once`` warmup accepts matching nodes only until it
reaches a terminal phase; create a new resource for later scale-out nodes.
`Succeeded` is an observation at completion time, not a durable pin: kubelet
image garbage collection can evict the image later.
