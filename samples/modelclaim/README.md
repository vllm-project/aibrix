# ModelClaim High-Density Runtime Pool

These examples attach two `ModelClaim` objects to one warm GPU runtime pool.
Each claim is started by the `aibrix-runtime` sidecar as a separate
kvcached-enabled engine process; the gateway routes by served model name.

The runtime image must be layered on a kvcached-enabled vLLM image. The
controller uses the runtime's snapshot endpoint to prefer an existing weight
cache and lower live memory pressure when it chooses among warm pods.
Keep kvcached autopatch disabled for the runtime sidecar process itself; the
launcher enables it only for the child engine process it starts.

```bash
kubectl apply -f samples/modelclaim/warm-runtime-pool.yaml
kubectl apply -f samples/modelclaim/modelclaims.yaml

kubectl get modelclaims -w
kubectl get pods -l pool.aibrix.ai/name=b300-pool-a -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations}{"\n"}{end}'
```

The controller writes `modelclaim.aibrix.ai/<claim-name>` annotations with
`{"model":"<served-name>","port":<engine-port>}`. `port:0` means the claim is
not routable. It is used while an engine is booting, unhealthy, or sleeping.
The controller flips the annotation to the real engine port only after the
runtime reports `active` and ready.

`engineConfig.args` is the structured pass-through for engine flags. It does
not express GPU resources or parallelism. With kvcached enabled, do not add
`--gpu-memory-utilization`; kvcached owns elastic KV allocation.

The Phase-2 foundation supports manual sidecar `kv-limit`, vLLM sleep, and
wake operations, but it does not run an automatic idle policy or gateway hold.
Use [the Phase-2 runbook](../../docs/modelclaim/phase2-foundation-runbook.md)
for mock, minikube, and A10 validation. The supported path is one GPU per warm
runtime pod and independent single-GPU engines; TP/PP is not supported by this
sample.
