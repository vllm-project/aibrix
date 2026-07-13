# ModelClaim high-density runtime pool

These examples show two `ModelClaim` objects attached to one warm GPU runtime
pool. Each claim is started by the `aibrix-runtime` sidecar as a separate
kvcached-enabled engine process; the gateway routes by the served model name.

```bash
kubectl apply -f samples/modelclaim/warm-runtime-pool.yaml
kubectl apply -f samples/modelclaim/modelclaims.yaml

kubectl get modelclaims -w
kubectl get pods -l pool.aibrix.ai/name=b300-pool-a -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations}{"\n"}{end}'
```

The controller writes `modelclaim.aibrix.ai/<claim-name>` annotations with
`{"model":"<served-name>","port":<engine-port>}`. `port:0` means the claim is
not yet routable while the engine is booting. The controller flips the
annotation to the real engine port after the runtime reports the model ready.
