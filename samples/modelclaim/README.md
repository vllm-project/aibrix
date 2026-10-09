# ModelClaim High-Density Runtime Pool

This sample attaches two `ModelClaim` objects to one warm GPU runtime pool.
Each claim runs as an independent, kvcached-enabled engine process inside the
same Pod. The AIBrix Gateway routes requests by served model name to the port
assigned to that engine.

Run all commands from the repository root. This guide assumes the Kubernetes
cluster, GPU support, AIBrix control plane, Gateway, and ModelClaim CRD are
already available.

For API details, automatic pool policies, extended failure tests, and
performance experiments, see the
[ModelClaim feature guide](../../docs/source/features/modelclaim.rst) and the
[manual GPU validation runbook](../../development/tutorials/modelclaim/manual-validation-runbook.md).

## Architecture

```text
ModelClaim qwen3-0-6b ──────┐
                            ├──> ModelClaim Controller
ModelClaim qwen25-0-5b ─────┘              │
                                           │ select Pod and call :8080
                                           ▼
                              warm-runtime-pool-b300 Pod
                              ┌─────────────────────────────────┐
                              │ AIBrix Runtime Agent :8080      │
                              │                                 │
                              │ vLLM engine A :20000            │
                              │ └─ kvcached IPC kvc_qwen3-0-6b  │
                              │                                 │
                              │ vLLM engine B :20001            │
                              │ └─ kvcached IPC kvc_qwen25-0-5b │
                              └─────────────────────────────────┘
                                           ▲
                                           │ Pod routing annotations
Client ──> Envoy Gateway ──> Gateway plugin┘
```

The sample deploys one container named `aibrix-runtime`, using the image
`aibrix/kvcached-runtime:nightly` that CI publishes from `main`. That image
layers the AIBrix Runtime Agent on a kvcached-enabled vLLM base image.
kvcached is therefore not a separate Pod or sidecar. The Runtime Agent starts
one child engine process per ModelClaim, and each child enables kvcached with
a unique shared-memory IPC name.

The Runtime Agent itself keeps `ENABLE_KVCACHED=false` and
`KVCACHED_AUTOPATCH=0`. It does not serve inference and should not create an
unrelated CUDA context. The launcher overrides those values for every child
engine process and sets a distinct `KVCACHED_IPC_NAME`.

## Deploy the warm runtime pool

Create the Deployment and its metrics Service:

```bash
export NAMESPACE=default
kubectl -n "$NAMESPACE" apply -f samples/modelclaim/warm-runtime-pool.yaml
kubectl -n "$NAMESPACE" rollout status \
  deployment/warm-runtime-pool-b300 --timeout=10m
```

Discover the Pod through the stable sample label:

```bash
export POD=$(kubectl -n "$NAMESPACE" get pod \
  -l app=warm-runtime-pool-b300 \
  -o jsonpath='{.items[0].metadata.name}')

kubectl -n "$NAMESPACE" get pod "$POD" -o wide
kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
  curl -fsS http://127.0.0.1:8080/healthz
```

The Pod must be `Running` and Ready before creating ModelClaims. If the
runtime image was loaded directly into a local cluster, keep the sample's
`imagePullPolicy: IfNotPresent` setting.

## Deploy the two ModelClaims

Each sample claim declares `spec.perGPU`, what one instance costs on a GPU:
`maximumFootprint: 6Gi` and `kvFloor: 1Gi`. The controller places a claim only
on a Pod whose GPU has room for both, by the size the Runtime snapshot reports
and the costs the claims declare. A claim without `perGPU` is not placed.
Measure both figures for your own model and engine arguments, as the
[feature guide](../../docs/source/features/modelclaim.rst) describes.

```bash
kubectl -n "$NAMESPACE" apply -f samples/modelclaim/modelclaims.yaml
kubectl -n "$NAMESPACE" get modelclaims -w
```

In another terminal, wait for each claim independently:

```bash
export NAMESPACE=default
kubectl -n "$NAMESPACE" wait --for=jsonpath='{.status.phase}'=Active \
  modelclaim/qwen3-0-6b --timeout=15m
kubectl -n "$NAMESPACE" wait --for=jsonpath='{.status.phase}'=Active \
  modelclaim/qwen25-0-5b --timeout=15m

kubectl -n "$NAMESPACE" get modelclaims -o wide
```

Both claims must report `PHASE=Active`, `DESIRED=1`, and `READY=1`. During
startup they normally pass through `Pending`, `Scheduling`, `Loading`, and
`Activating`.

Inspect the routing annotations written by the controller:

```bash
kubectl -n "$NAMESPACE" get pod "$POD" -o json \
  | jq '.metadata.annotations
      | with_entries(select(.key | startswith("route.claim.model.aibrix.ai/")))'
```

An active entry has the served model name, a non-zero engine port, and
`state:"active"`. `port:0` means the claim is known but is not currently
routable, for example while it is activating, unhealthy, sleeping, or failed.

## Test each engine directly

Read the assigned ports from the Runtime snapshot and call each
OpenAI-compatible endpoint from inside the Pod:

```bash
snapshot=$(kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
  curl -fsS http://127.0.0.1:8080/v1/runtime/snapshot)

for model in qwen3-0.6b qwen2.5-0.5b; do
  port=$(jq -er --arg model "$model" \
    '.models[] | select(.model_name == $model) | .port' \
    <<<"$snapshot")
  payload=$(jq -nc --arg model "$model" \
    '{model:$model,
      messages:[{role:"user",content:"Reply with OK."}],
      max_tokens:16}')

  kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
    curl -fsS "http://127.0.0.1:${port}/v1/chat/completions" \
      -H 'Content-Type: application/json' \
      -d "$payload" \
    | jq -e 'select(.choices | length > 0)
        | {model, content:.choices[0].message.content, usage}'
done
```

Both responses must contain a non-empty `choices` array.

## Test both models through the Gateway

Discover the generated Envoy Service through its ownership labels:

```bash
export ENVOY_SERVICE=$(kubectl -n envoy-gateway-system get service \
  --selector=gateway.envoyproxy.io/owning-gateway-namespace=aibrix-system,gateway.envoyproxy.io/owning-gateway-name=aibrix-eg \
  -o jsonpath='{.items[0].metadata.name}')
test -n "$ENVOY_SERVICE"

kubectl -n envoy-gateway-system port-forward \
  "service/$ENVOY_SERVICE" 8888:80
```

Keep the port-forward running. From another terminal:

```bash
for model in qwen3-0.6b qwen2.5-0.5b; do
  payload=$(jq -nc --arg model "$model" \
    '{model:$model,
      messages:[{role:"user",content:"Reply with OK."}],
      max_tokens:16}')

  curl -fsS http://127.0.0.1:8888/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -H 'routing-strategy: random' \
    -d "$payload" \
    | jq -e 'select(.choices | length > 0)
        | {model, content:.choices[0].message.content, usage}'
done
```

The Gateway cache watches ModelClaims and the Pod routing annotations. It
resolves the request's `model` field to the selected Pod and the
model-specific engine port.

## Prove that two engines and kvcached are running

No single command proves the complete runtime state. Use the process tree,
engine registry, Runtime snapshot, shared-memory entries, and routing
annotations together.

### Process tree

```bash
kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
  ps -eo pid,ppid,stat,etimes,cmd --forest
```

Expect one `python3 -m aibrix.app` Runtime Agent, two independent
`vllm.entrypoints.openai.api_server` processes, and one EngineCore below each
vLLM API server. The two API servers must use different model names and ports.

### Engine registry

```bash
kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
  cat /var/run/aibrix/engines.json \
  | jq '.engines[]
      | {model_name,pid,port,ipc_name,phase,restart_count}'
```

Expect two records with distinct `pid`, `port`, and `ipc_name` values. Both
records should have `phase:"active"` and no unexpected restarts.

### Runtime snapshot

```bash
kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
  curl -fsS http://127.0.0.1:8080/v1/runtime/snapshot \
  | jq '.models[]
      | {model_name,phase,ready,alive,port,
         kv_used_bytes,kv_capacity_bytes,request_success_total}'
```

Expect both models to be `active`, `ready:true`, and `alive:true`, with
different non-zero ports. `kv_used_bytes` and `kv_capacity_bytes` are
per-model kvcached observations; they are not whole-device memory usage.

### kvcached tools and shared memory

```bash
kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
  sh -c 'command -v kvctl'

kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
  find /dev/shm -maxdepth 1 -name 'kvc_*' -print
```

Expect `kvctl` in the image and one `kvc_*` shared-memory entry per engine.
kvcached is loaded into each child engine through Python autopatching, so a
separate `kvcached` Pod or a process literally named `kvcached` is not
required.

## Test manual sleep and wake

The sample launcher enables the engine sleep API. This procedure sleeps one
model, confirms that its peer remains active, and restores the sleeping model
before finishing.

Use a unique operation ID. Repeating the same request demonstrates API
idempotency: the first response should contain `applied:true`, and the second
should contain `applied:false`.

```bash
export OP_ID="manual-$(date -u +%Y%m%dT%H%M%SZ)"

for attempt in 1 2; do
  kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
    curl -fsS http://127.0.0.1:8080/v1/runtime/models/sleep \
      -H 'Content-Type: application/json' \
      -d "{\"model_name\":\"qwen3-0.6b\",\"level\":1,\"operation_id\":\"${OP_ID}-sleep\"}" \
    | jq '{model_name,operation_id,applied,phase}'
done

kubectl -n "$NAMESPACE" wait --for=jsonpath='{.status.phase}'=Sleeping \
  modelclaim/qwen3-0-6b --timeout=2m
kubectl -n "$NAMESPACE" get modelclaims -o wide
```

At this point Qwen3 should be `Sleeping` with no ready replica, while Qwen2.5
remains `Active` and Ready. Verify that the peer still serves requests:

```bash
snapshot=$(kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
  curl -fsS http://127.0.0.1:8080/v1/runtime/snapshot)
peer_port=$(jq -er \
  '.models[] | select(.model_name == "qwen2.5-0.5b") | .port' \
  <<<"$snapshot")
payload=$(jq -nc \
  '{model:"qwen2.5-0.5b",
    messages:[{role:"user",content:"Reply with PEER_OK."}],
    max_tokens:16}')

kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
  curl -fsS "http://127.0.0.1:${peer_port}/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d "$payload" \
  | jq -e '.choices | length > 0'
```

Wake Qwen3 and restore the original state:

```bash
for attempt in 1 2; do
  kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- \
    curl -fsS http://127.0.0.1:8080/v1/runtime/models/wake \
      -H 'Content-Type: application/json' \
      -d "{\"model_name\":\"qwen3-0.6b\",\"operation_id\":\"${OP_ID}-wake\"}" \
    | jq '{model_name,operation_id,applied,phase}'
done

kubectl -n "$NAMESPACE" wait --for=jsonpath='{.status.phase}'=Active \
  modelclaim/qwen3-0-6b --timeout=5m
kubectl -n "$NAMESPACE" get modelclaims -o wide
```

Manual sleep is not automatic idle policy. Without a pool lifecycle policy,
both engines normally remain Active at the same time.

## Observability

The warm-pool sample includes a metrics Service labeled
`aibrix.ai/metrics=modelclaim-runtime`. Apply
`observability/monitor/service_monitor_modelclaim_runtime.yaml` and import
`observability/grafana/AIBrix_ModelClaim_Runtime_Dashboard.json` to monitor
lifecycle, resident density, KV use, request activity, and HBM observations.
See [`observability/grafana/README.md`](../../observability/grafana/README.md)
for the metric contract and import steps.

## Troubleshooting

### The warm Pod is Pending

Check scheduling events, the runtime image, and the node's allocatable GPU
resource:

```bash
kubectl -n "$NAMESPACE" describe pod "$POD"
kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu
```

### A claim stays Pending or Scheduling

Confirm the warm Pod is Ready and carries both required labels:

```bash
kubectl -n "$NAMESPACE" get pod "$POD" --show-labels
```

The Pod must match the claim's `podSelector` and have
`claim.model.aibrix.ai/enabled: "true"`.

Then read the claim's `Scheduled` condition, which says why it waits:

```bash
kubectl -n "$NAMESPACE" get modelclaim qwen3-0-6b \
  -o jsonpath='{.status.conditions[?(@.type=="Scheduled")]}{"\n"}'
```

`InvalidPerGPU` means that `perGPU` is missing or invalid. `NoMatchingPods`
about GPU memory means that no GPU has room for the claim's footprint plus its
KV floor. Its message names the Pod that came closest, and why it was turned
away.

### A claim stays Loading or Activating

Inspect the Runtime Agent and controller logs:

```bash
kubectl -n "$NAMESPACE" logs "$POD" -c aibrix-runtime --tail=200
kubectl -n aibrix-system logs deployment/aibrix-controller-manager --tail=200
```

Check model artifact access, engine startup errors, and the Runtime snapshot.
A timeout is an inconclusive or failed test; do not replace bounded polling
with an arbitrary long sleep.

### kvcached evidence is missing

Confirm that the Deployment uses the dedicated kvcached runtime image, that
`kvctl` exists, and that child engine logs contain kvcached patch messages.
Keep autopatch disabled on the Runtime Agent itself. The launcher enables it
only for child engines and assigns their unique IPC names.

### The routing annotation has port zero

`port:0` intentionally removes the model from routing while it is activating,
unhealthy, sleeping, or failed. Inspect the ModelClaim condition, Runtime
snapshot, and controller logs instead of routing directly to port zero.

### The Gateway returns 503 for a sleeping model

Request-driven wake is asynchronous. A request to a sleeping model can return
HTTP 503 with `Retry-After`; the Gateway does not hold the original request.
Retry after the ModelClaim returns to `Active`.

## Limitations

- `ModelClaim.spec.replicas` currently supports only one engine replica.
- The sample uses independent single-GPU engines in one fixed-topology pool.
- Fixed TP/PP requires a separate homogeneous pool whose Pod GPU limit equals
  `TP x PP`.
- Do not add `--gpu-memory-utilization`; kvcached owns elastic KV allocation.
- KV capacity values are configured limits and observations, not proof of
  physically occupied device memory.
- Successful functional validation does not prove OOM safety, throughput,
  latency, utilization, or cost improvements for arbitrary workloads.

## Cleanup

Delete claims before deleting their warm runtime pool:

```bash
export NAMESPACE=default
kubectl -n "$NAMESPACE" delete -f samples/modelclaim/modelclaims.yaml
kubectl -n "$NAMESPACE" delete -f samples/modelclaim/warm-runtime-pool.yaml
```

Confirm that the sample resources are gone:

```bash
kubectl -n "$NAMESPACE" get modelclaims
kubectl -n "$NAMESPACE" get deployment,service \
  -l claim.model.aibrix.ai/pool=b300-pool-a
```
