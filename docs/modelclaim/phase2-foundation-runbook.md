# ModelClaim Phase-2 Foundation Runbook

This runbook validates the Phase-2 foundation in increasing-cost order. It
covers runtime snapshots, manual KV/lifecycle controls, snapshot-aware
placement, and the non-routable Sleeping route gate. It does not test an
automatic idle policy or gateway hold because neither is implemented.

Do not put cloud API keys, Hugging Face tokens, or private registry credentials
in manifests, shell history shared with others, or this repository.

## 1. Local Unit Verification

From the Phase-2 branch:

```bash
go test ./pkg/controller/modelclaim -count=1
make build-controller-manager

cd python/aibrix
ruff check \
  aibrix/openapi/protocol.py \
  aibrix/runtime/model_runtime.py \
  aibrix/runtime/model_runtime_api.py \
  tests/runtime/test_model_runtime.py
pytest tests/runtime/test_model_runtime.py tests/runtime/test_model_runtime_metrics.py -q
```

Expected:

- Go controller tests cover snapshot placement and sleeping-route transitions.
- Python tests cover idempotent `kv-limit`, sleep, wake, error mapping, and
  runtime state reporting.

## 2. Mock Runtime and Controller on Minikube

Follow the controller/image setup in
[the Phase-1 runbook](phase1-test-runbook.md#4-controller--modelclaim-on-minikube),
using `AIBRIX_MODEL_RUNTIME_MOCK=1`. On a CPU-only minikube, use that runbook's
inline mock warm-pool Deployment because the checked-in sample requests a GPU.
On a GPU-enabled minikube, apply the sample and override it for mock mode:

```bash
kubectl apply -f samples/modelclaim/warm-runtime-pool.yaml
kubectl set env deployment/warm-runtime-pool-b300 \
  AIBRIX_MODEL_RUNTIME_MOCK=1 ENABLE_KVCACHED=false KVCACHED_AUTOPATCH=0
kubectl rollout status deployment/warm-runtime-pool-b300 --timeout=180s
kubectl apply -f samples/modelclaim/modelclaims.yaml
kubectl get modelclaims -w
```

Find the runtime pod and inspect controller state:

```bash
POD=$(kubectl get pod -l app=warm-runtime-pool-b300 -o jsonpath='{.items[0].metadata.name}')
kubectl get modelclaim qwen3-0-6b -o json | jq '{phase:.status.phase,ready:.status.readyReplicas,instances:.status.instances}'
kubectl get pod "$POD" -o json | jq '.metadata.annotations'
```

Expected before lifecycle operations:

- The claim reaches `Active` with one ready replica.
- The corresponding `modelclaim.aibrix.ai/qwen3-0-6b` annotation contains the
  real runtime port.

Port-forward only for this manual test, then exercise the sidecar API:

```bash
kubectl port-forward pod/"$POD" 8080:8080
```

In another terminal:

```bash
RUNTIME=http://127.0.0.1:8080
MODEL=qwen3-0.6b

curl -sS "$RUNTIME/v1/runtime/snapshot" | jq .

curl -sS -X POST "$RUNTIME/v1/runtime/models/kv-limit" \
  -H 'content-type: application/json' \
  -d "{\"model_name\":\"$MODEL\",\"limit_bytes\":1073741824,\"operation_id\":\"manual-kv-$(date +%s)\"}" | jq .

curl -sS -X POST "$RUNTIME/v1/runtime/models/sleep" \
  -H 'content-type: application/json' \
  -d "{\"model_name\":\"$MODEL\",\"level\":1,\"operation_id\":\"manual-sleep-$(date +%s)\"}" | jq .
```

Watch the claim until it is `Sleeping`, then verify `port:0`:

```bash
kubectl get modelclaim qwen3-0-6b -w
kubectl get pod "$POD" -o json | jq -r '.metadata.annotations["modelclaim.aibrix.ai/qwen3-0-6b"]'
```

Wake it and wait for `Active` again:

```bash
curl -sS -X POST "$RUNTIME/v1/runtime/models/wake" \
  -H 'content-type: application/json' \
  -d "{\"model_name\":\"$MODEL\",\"operation_id\":\"manual-wake-$(date +%s)\"}" | jq .

kubectl get modelclaim qwen3-0-6b -w
```

The mock runtime returns ready immediately. On a real engine, expect a period
of `Activating` with `port:0` before the health probe restores the real port.

## 3. Real A10 Runtime Validation

Use an A10 instance with Docker, NVIDIA drivers, and a private test network.
The base engine image is convenient because it includes kvcached, `kvctl`, and
vLLM:

```bash
docker pull ghcr.io/ovg-project/kvcached-vllm:latest
```

Build or obtain an `aibrix-runtime` image layered on that base image. The
overlay example in [the Phase-1 runbook](phase1-test-runbook.md#3-runtime-api--vllmkvcached)
is suitable. Run the sidecar with GPU access, host networking, a sufficiently
large `/dev/shm`, and a persistent Hugging Face cache mount.

Set `ENABLE_KVCACHED=false` and `KVCACHED_AUTOPATCH=0` on the sidecar parent
process. `SubprocessEngineLauncher` sets both values for each child engine
process along with its unique IPC name. This avoids autopatching the sidecar
supervisor itself when using the kvcached image.

Activate a small model through the sidecar, not by calling vLLM's development
endpoints directly:

```bash
RUNTIME=http://127.0.0.1:8080
cat >/tmp/activate-qwen.json <<'JSON'
{
  "model_name": "qwen3-0.6b",
  "artifact_url": "hf://Qwen/Qwen3-0.6B",
  "engine": "vllm",
  "ipc_name": "kvc_qwen3-0.6b",
  "engine_config": {
    "args": {
      "--max-model-len": "2048"
    }
  }
}
JSON

curl -sS -X POST "$RUNTIME/v1/runtime/models/activate" \
  -H 'content-type: application/json' \
  -d @/tmp/activate-qwen.json | tee /tmp/activate-qwen-response.json | jq .
PORT=$(jq -r '.port' /tmp/activate-qwen-response.json)
until curl -sf "http://127.0.0.1:$PORT/health" >/dev/null; do sleep 3; done
```

Validate runtime observation and a normal inference request:

```bash
curl -sS "$RUNTIME/v1/runtime/snapshot" | jq .
curl -sS "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H 'content-type: application/json' \
  -d '{"model":"qwen3-0.6b","messages":[{"role":"user","content":"Say hi in one short sentence."}],"max_tokens":16}' | jq .
ls -l /dev/shm | grep kvc
```

Then validate the manual controls through the sidecar:

```bash
curl -sS -X POST "$RUNTIME/v1/runtime/models/kv-limit" \
  -H 'content-type: application/json' \
  -d '{"model_name":"qwen3-0.6b","limit_bytes":1073741824,"operation_id":"a10-kv-1"}' | jq .

curl -sS -X POST "$RUNTIME/v1/runtime/models/sleep" \
  -H 'content-type: application/json' \
  -d '{"model_name":"qwen3-0.6b","level":1,"operation_id":"a10-sleep-1"}' | jq .
curl -sS "$RUNTIME/v1/runtime/models" | jq .

curl -sS -X POST "$RUNTIME/v1/runtime/models/wake" \
  -H 'content-type: application/json' \
  -d '{"model_name":"qwen3-0.6b","operation_id":"a10-wake-1"}' | jq .
until curl -sf "http://127.0.0.1:$PORT/health" >/dev/null; do sleep 3; done
```

Expected:

- The IPC name in `/dev/shm` is normalized; dots become dashes.
- `kv-limit` returns `applied: true` and targets that normalized IPC name.
- Sleep makes the sidecar list report `phase: sleeping` and `ready: false`.
- Wake returns to `active`; the engine must pass `/health` before routing is
  restored by a Kubernetes controller.

Keep `VLLM_SERVER_DEV_MODE=1` and the vLLM lifecycle endpoints private to the
pod. The AIBrix sidecar calls them through localhost; do not expose them as a
public inference API.

## 4. Real Kubernetes A10 Route-Gate Validation

Run the same runtime image in a one-GPU minikube or equivalent Kubernetes
cluster. Wait for the NVIDIA device plugin to advertise `nvidia.com/gpu` before
creating the warm pool. Apply one ModelClaim, wait for `Active`, then use the
port-forward sequence from section 2 to issue sleep and wake operations.

Record these observations:

| Step | Claim phase | Ready replicas | Pod annotation |
| --- | --- | --- | --- |
| Engine ready | `Active` | 1 | real engine port |
| Sidecar reports sleeping | `Sleeping` | 0 | `port:0` |
| Wake in progress, health false | `Activating` | 0 | `port:0` |
| Wake complete, health true | `Active` | 1 | real engine port |

Also verify that the number of runtime activation calls does not increase over
the sleep/wake cycle. A sleeping instance remains assigned; this foundation
must not start a duplicate engine.

## 5. Cleanup

```bash
kubectl delete -f samples/modelclaim/modelclaims.yaml --ignore-not-found=true
kubectl delete -f samples/modelclaim/warm-runtime-pool.yaml --ignore-not-found=true
docker rm -f aibrix-runtime-test 2>/dev/null || true
```

Terminate rented GPU instances immediately after the real-hardware run. The
current support boundary is one GPU per warm runtime pod; do not use this
runbook as validation for TP, PP, or multi-node inference.
