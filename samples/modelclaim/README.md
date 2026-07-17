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
kubectl get pods -l pool.aibrix.ai/name=b300-pool-a \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations}{"\n"}{end}'
```

The warm-pool sample includes a metrics `Service` labeled
`aibrix.ai/metrics=modelclaim-runtime`. Apply
`observability/monitor/service_monitor_modelclaim_runtime.yaml` and import
`observability/grafana/AIBrix_ModelClaim_Runtime_Dashboard.json` to monitor
lifecycle, resident density, KV use, and HBM peak. See
[`observability/grafana/README.md`](../../observability/grafana/README.md) for
the metric contract and import steps.

The controller writes `modelclaim.aibrix.ai/<claim-name>` annotations with
`{"model":"<served-name>","port":<engine-port>,"state":"<state>"}`. `port:0`
means the claim is known but not routable. It is used while an engine is
activating, unhealthy, sleeping, or failed. The controller restores the real
engine port only after the runtime reports `active` and ready.

`engineConfig.args` is the structured pass-through for engine flags. It does
not express GPU resources or parallelism. With kvcached enabled, do not add
`--gpu-memory-utilization`; kvcached owns elastic KV allocation.

An optional JSON policy on the warm-pool Deployment drives request-based KV
limit redistribution and idle sleep. A request for a sleeping model triggers
an asynchronous wake and receives HTTP 503 with `Retry-After`; the gateway does
not hold the original request. See the
[manual GPU validation runbook](../../docs/modelclaim/manual-validation-runbook.md)
for the complete minikube and A10 procedure, reliability fault injection, and
the boundary between functional acceptance and performance evidence. Use the
[kvcached and vLLM sleep mechanism guide](../../docs/modelclaim/kvcached-vllm-sleep-test-guide.md)
when validating those upstream primitives without the AIBrix control plane.

This sample uses one GPU and independent single-GPU engines. Fixed TP/PP is
supported only in a separate topology-homogeneous pool whose Pod GPU limit
equals `TP x PP`. `ModelClaim.spec.replicas` is optional and currently accepts
only one.
