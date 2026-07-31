# ModelClaim Manual GPU Validation Runbook

This is the single manual acceptance procedure for the merged ModelClaim
stack. It is organized by observable capability rather than implementation
phase. Record the source revision used to build the runtime and the actual
control-plane image digests used by the cluster.
For direct, control-plane-independent T1-T7 checks of kvcached and vLLM sleep,
use the [upstream mechanism test guide](kvcached-vllm-sleep-test-guide.md).

The default path assumes SSH access to an existing Lambda A10 host with a
working single-node minikube cluster. The run deploys one warm runtime Pod and
two independent single-GPU vLLM engines:

- `Qwen/Qwen3-0.6B`, served as `qwen3-0.6b`;
- `Qwen/Qwen2.5-0.5B-Instruct`, served as `qwen2.5-0.5b`;
- kvcached owns elastic KV allocation, so neither engine uses
  `--gpu-memory-utilization`;
- `ModelClaim.spec.replicas` is optional and only the value `1` is supported;
- the pool policy is one JSON annotation on the warm-pool Deployment.

## What This Run Establishes

The required acceptance sequence establishes:

1. activation, readiness gating, per-model routing, snapshot reporting, and
   deletion cleanup;
2. two co-resident engines with independent ports, IPC names, and failure
   domains;
3. idempotent KV-limit, sleep, and wake control operations;
4. request-driven KV **limit** redistribution and idle sleep/request wake;
5. agent restart with engine re-adoption, isolated engine restart, and local
   restart-budget exhaustion.

The following claims require the optional load experiments later in this
document and must not be inferred from a successful functional run:

- that a selected KV limit prevents OOM for every workload;
- that 3.2/0.8 GiB is physically occupied HBM rather than a configured
  kvcached capacity limit;
- that the policy improves throughput, TTFT, TPOT, goodput, SM activity, or
  cost;
- that current HBM observations provide hard admission control.

## Acceptance Matrix

| ID | Capability | Gate | Existing evidence | Pass condition |
|---|---|---|---|---|
| F1 | Activation, observation, density | required | A10 passed | both models ready; distinct port/IPC/PID |
| F2 | Direct and gateway inference | required | A10 passed | both models return HTTP 200 |
| F3 | API and topology validation | required | A10 passed | fixed-KV flag fails; TP=2 stays off one GPU |
| F4 | Manual controls | required | A10 passed | KV limit, sleep, and wake are idempotent |
| P1 | Dynamic KV policy | required | control passed | demand moves limits safely in both directions |
| P2 | Idle sleep and request wake | required | A10 passed | route removed, retryable 503, route restored |
| R1 | Agent re-adoption | required | A10 passed | agent changes; engine PID/port/IPC do not |
| R2 | Engine crash isolation | required | A10 passed | target restarts; peer remains available |
| R3 | Terminal local failure | extended | A10 passed | target becomes `Failed`; peer remains available |
| S1 | KV pressure behavior | optional | **not run** | chosen load completes without engine OOM/restart |
| B1 | Policy benefit | optional | **not run** | repeated data supports the stated metric claim |

Use bounded polling for every wait. A timeout is a failed or inconclusive test,
not a reason to add an arbitrary long sleep.

## 1. Existing Cluster Preflight

The normal path reuses an existing Lambda GPU cluster. It does not create,
restart, or terminate the Lambda instance or minikube cluster. Use
[Appendix A](#appendix-a-optional-lambda-cluster-setup) only when no reusable
cluster is available.

On the Lambda host, confirm that the existing cluster and GPU are ready:

```bash
nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv
minikube status
kubectl cluster-info
kubectl wait --for=condition=Ready node --all --timeout=5m

export TEST_NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
export GPU_COUNT=$(kubectl get node "$TEST_NODE" \
  -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')
test "${GPU_COUNT:-0}" -ge 1
kubectl get pods -A
```

Stop if the context points to a shared production cluster, the GPU is already
reserved by another test, or the cluster owner has not approved disruptive
fault injection. Section R3 intentionally drives one engine to terminal
failure.

## 2. Prepare the Revision and Evidence

Clone or update the repository on the Lambda host, then record the exact
revision before building anything:

```bash
git clone https://github.com/vllm-project/aibrix.git
cd aibrix
git checkout <commit-or-branch>
export TEST_COMMIT=$(git rev-parse HEAD)
git status --short
printf 'commit=%s\n' "$TEST_COMMIT" | tee /tmp/modelclaim-test-build.txt
```

## 3. Install the Control Plane and Build the Runtime

The normal merged-feature path uses the public controller and gateway nightly
images from `main`. Build only the dedicated experimental kvcached runtime as
`dev`, which matches the sample manifest. It is intentionally not part of the
public nightly image workflow.

```bash
IMAGE_TAG=dev IS_MAIN_BRANCH=false make docker-build-kvcached-runtime
docker save aibrix/kvcached-runtime:dev | docker exec -i minikube docker load
```

Install the current CRD and standard nightly control plane, then wait for the
relevant components. If the reusable cluster is centrally managed and already
current, these apply operations are idempotent.

```bash
kubectl apply -k config/dependency --server-side
kubectl apply -k config/crd --server-side
kubectl apply -k config/default

kubectl -n aibrix-system rollout status \
  deployment/aibrix-controller-manager --timeout=10m
kubectl -n aibrix-system rollout status \
  deployment/aibrix-gateway-plugins --timeout=10m
kubectl get pods -A
```

Building custom controller or gateway images is required only when validating
unmerged control-plane changes; that is outside the normal merged-feature
acceptance path.

Create an evidence directory and record versions. Never store cloud
credentials in it.

```bash
export RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-${TEST_COMMIT:0:12}"
export EVIDENCE="$HOME/modelclaim-evidence/$RUN_ID"
mkdir -p "$EVIDENCE"
git show -s --format=fuller HEAD >"$EVIDENCE/commit.txt"
kubectl version >"$EVIDENCE/kubectl-version.txt"
minikube version >"$EVIDENCE/minikube-version.txt"
nvidia-smi -q >"$EVIDENCE/nvidia-smi-before.txt"
kubectl -n aibrix-system get pods \
  -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[*].image,IMAGE_ID:.status.containerStatuses[*].imageID' \
  >"$EVIDENCE/control-plane-images.txt"
```

## 4. Deploy the Warm Pool

Start without an automatic policy so manual controls can be tested in
isolation:

```bash
kubectl apply -f samples/modelclaim/warm-runtime-pool.yaml
kubectl rollout status deployment/warm-runtime-pool-b300 --timeout=10m

export NAMESPACE=default
export POD=$(kubectl -n "$NAMESPACE" get pod \
  -l app=warm-runtime-pool-b300 \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$NAMESPACE" exec "$POD" -c aibrix-runtime -- nvidia-smi -L
```

In a second terminal, keep the runtime port-forward running:

```bash
POD=$(kubectl -n default get pod -l app=warm-runtime-pool-b300 \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n default port-forward "pod/$POD" 8080:8080
```

In a third terminal, expose Envoy using its stable ownership labels rather
than a generated Service suffix:

```bash
export ENVOY_SERVICE=$(kubectl -n envoy-gateway-system get service \
  --selector=gateway.envoyproxy.io/owning-gateway-namespace=aibrix-system,gateway.envoyproxy.io/owning-gateway-name=aibrix-eg \
  -o jsonpath='{.items[0].metadata.name}')
test -n "$ENVOY_SERVICE"
kubectl -n envoy-gateway-system port-forward \
  "service/$ENVOY_SERVICE" 8888:80
```

## 5. Functional Acceptance

### F1: Activation, Readiness, and Routing

Apply the sample claims and wait independently for each model:

```bash
kubectl apply -f samples/modelclaim/modelclaims.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Active \
  modelclaim/qwen3-0-6b --timeout=15m
kubectl wait --for=jsonpath='{.status.phase}'=Active \
  modelclaim/qwen25-0-5b --timeout=15m
```

Capture actual state:

```bash
curl -fsS localhost:8080/v1/runtime/snapshot \
  | tee "$EVIDENCE/snapshot-active.json" | jq .
kubectl get modelclaims -o yaml >"$EVIDENCE/modelclaims-active.yaml"
kubectl get pod "$POD" -o json \
  | tee "$EVIDENCE/warm-pod-active.json" \
  | jq '.metadata.annotations | with_entries(select(.key | startswith("modelclaim.aibrix.ai/")))'
kubectl exec "$POD" -c aibrix-runtime -- \
  cat /var/run/aibrix/engines.json \
  | tee "$EVIDENCE/registry-active.json" | jq .
```

Pass criteria:

- both claim and instance phases are `Active`, with `readyReplicas: 1`;
- both runtime models have `alive:true`, `ready:true`, and `phase:"active"`;
- each Pod annotation has `state:"active"` and a non-zero port;
- the two engines have distinct PIDs, ports, and normalized IPC names;
- the snapshot contains accelerator HBM fields, per-model HBM/KV fields, and
  request activity fields.

### F2: Direct and Gateway Inference

Query each engine directly from the warm Pod:

```bash
for model in qwen3-0.6b qwen2.5-0.5b; do
  port=$(curl -fsS localhost:8080/v1/runtime/snapshot \
    | jq -er --arg model "$model" \
      '.models[] | select(.model_name == $model) | .port')
  kubectl exec "$POD" -c aibrix-runtime -- curl -fsS \
    "http://127.0.0.1:${port}/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with OK.\"}],\"max_tokens\":16}" \
    | jq -e '.choices | length > 0'
done
```

Then query both through Envoy:

```bash
for model in qwen3-0.6b qwen2.5-0.5b; do
  curl -fsS http://127.0.0.1:8888/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -H 'routing-strategy: random' \
    -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with OK.\"}],\"max_tokens\":16}" \
    | jq -e '.choices | length > 0'
done
```

### F3: API and Fixed-Topology Validation

`replicas` may be omitted and defaults to one. A value other than one must be
rejected by API validation. Also verify that kvcached claims reject vLLM's
fixed KV allocator flag before activation:

```bash
if kubectl apply -f - <<'YAML'
apiVersion: model.aibrix.ai/v1alpha1
kind: ModelClaim
metadata:
  name: invalid-replicas
spec:
  modelName: invalid-replicas
  podSelector:
    matchLabels:
      pool.aibrix.ai/name: b300-pool-a
  artifactURL: huggingface://Qwen/Qwen3-0.6B
  engine: vllm
  replicas: 2
YAML
then
  echo 'replicas=2 was accepted unexpectedly' >&2
  kubectl delete modelclaim invalid-replicas --ignore-not-found
  false
fi

kubectl apply -f - <<'YAML'
apiVersion: model.aibrix.ai/v1alpha1
kind: ModelClaim
metadata:
  name: invalid-fixed-kv
spec:
  modelName: invalid-fixed-kv
  podSelector:
    matchLabels:
      pool.aibrix.ai/name: b300-pool-a
  artifactURL: huggingface://Qwen/Qwen3-0.6B
  engine: vllm
  engineConfig:
    args:
      --gpu-memory-utilization: "0.45"
YAML
kubectl wait --for=jsonpath='{.status.phase}'=Failed \
  modelclaim/invalid-fixed-kv --timeout=1m
kubectl get modelclaim invalid-fixed-kv -o json \
  | jq -e '.status.conditions[] | select(.reason == "InvalidEngineConfig")'
kubectl delete modelclaim invalid-fixed-kv
```

On this one-GPU Pod, a temporary TP=2 claim must remain unassigned and must not
appear in the runtime snapshot:

```bash
kubectl apply -f - <<'YAML'
apiVersion: model.aibrix.ai/v1alpha1
kind: ModelClaim
metadata:
  name: invalid-topology
spec:
  modelName: invalid-topology
  podSelector:
    matchLabels:
      pool.aibrix.ai/name: b300-pool-a
  artifactURL: huggingface://Qwen/Qwen3-0.6B
  engine: vllm
  engineConfig:
    args:
      --tensor-parallel-size: "2"
      --pipeline-parallel-size: "1"
YAML

violation=0
deadline=$((SECONDS + 30))
while (( SECONDS < deadline )); do
  if [[ -n $(kubectl get modelclaim invalid-topology \
      -o jsonpath='{.status.instances}' 2>/dev/null) ]] || \
      curl -fsS localhost:8080/v1/runtime/snapshot \
      | jq -e '.models[] | select(.model_name == "invalid-topology")' \
        >/dev/null; then
    violation=1
    break
  fi
  sleep 2
done
[[ "$violation" == 0 ]]
kubectl delete modelclaim invalid-topology
```

A positive TP=2 test requires a separate warm pool whose Pod requests exactly
two GPUs. Do not mix TP=1 and TP=2 engines in one pool.

### F4: Idempotent Manual Controls

Use a unique operation ID per intended state transition. Repeating the same
request must return `applied:false`:

```bash
for attempt in 1 2; do
  curl -fsS localhost:8080/v1/runtime/models/kv-limit \
    -H 'Content-Type: application/json' \
    -d "{\"model_name\":\"qwen3-0.6b\",\"limit_bytes\":1073741824,\"operation_id\":\"${RUN_ID}-limit-1\"}" \
    | tee "$EVIDENCE/manual-limit-${attempt}.json" | jq .
done

for attempt in 1 2; do
  curl -fsS localhost:8080/v1/runtime/models/sleep \
    -H 'Content-Type: application/json' \
    -d "{\"model_name\":\"qwen3-0.6b\",\"level\":1,\"operation_id\":\"${RUN_ID}-sleep-1\"}" \
    | tee "$EVIDENCE/manual-sleep-${attempt}.json" | jq .
done
kubectl wait --for=jsonpath='{.status.phase}'=Sleeping \
  modelclaim/qwen3-0-6b --timeout=2m

for attempt in 1 2; do
  curl -fsS localhost:8080/v1/runtime/models/wake \
    -H 'Content-Type: application/json' \
    -d "{\"model_name\":\"qwen3-0.6b\",\"operation_id\":\"${RUN_ID}-wake-1\"}" \
    | tee "$EVIDENCE/manual-wake-${attempt}.json" | jq .
done
kubectl wait --for=jsonpath='{.status.phase}'=Active \
  modelclaim/qwen3-0-6b --timeout=5m

jq -e '.applied == true' "$EVIDENCE/manual-limit-1.json"
jq -e '.applied == false' "$EVIDENCE/manual-limit-2.json"
jq -e '.applied == true' "$EVIDENCE/manual-sleep-1.json"
jq -e '.applied == false' "$EVIDENCE/manual-sleep-2.json"
jq -e '.applied == true' "$EVIDENCE/manual-wake-1.json"
jq -e '.applied == false' "$EVIDENCE/manual-wake-2.json"
```

While sleeping, the assignment remains but claim status is `Sleeping`,
`readyReplicas` is zero or omitted, and the route is
`port:0/state:"sleeping"`. A route becomes active again only after runtime
readiness.

## 6. Pool Policy Acceptance

### P1: Request-Driven KV Limit Redistribution

Enable reclaim without lifecycle actions:

```bash
kubectl annotate deployment/warm-runtime-pool-b300 \
  'pool.aibrix.ai/policy={"reclaim":{"mode":"kv-first","capacityBytes":4294967296,"guaranteedFloorPercent":20}}' \
  --overwrite
```

Establish demand for only Qwen3, then poll until the controller applies a safe
plan:

```bash
export PORT_A=$(curl -fsS localhost:8080/v1/runtime/snapshot \
  | jq -er '.models[] | select(.model_name == "qwen3-0.6b") | .port')
export PORT_B=$(curl -fsS localhost:8080/v1/runtime/snapshot \
  | jq -er '.models[] | select(.model_name == "qwen2.5-0.5b") | .port')

for _ in $(seq 1 20); do
  kubectl exec "$POD" -c aibrix-runtime -- curl -fsS \
    "http://127.0.0.1:${PORT_A}/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen3-0.6b","prompt":"Explain KV pooling briefly.","max_tokens":32}' \
    >/dev/null
done

deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  snapshot=$(curl -fsS localhost:8080/v1/runtime/snapshot)
  a=$(jq -r '.models[] | select(.model_name == "qwen3-0.6b") | .kv_capacity_bytes' <<<"$snapshot")
  b=$(jq -r '.models[] | select(.model_name == "qwen2.5-0.5b") | .kv_capacity_bytes' <<<"$snapshot")
  (( a > b )) && break
  sleep 2
done
(( a > b ))
printf '%s\n' "$snapshot" | tee "$EVIDENCE/kv-qwen3-hot.json" | jq .
```

Move demand to Qwen2.5 and require the limits to reverse:

```bash
for _ in $(seq 1 20); do
  kubectl exec "$POD" -c aibrix-runtime -- curl -fsS \
    "http://127.0.0.1:${PORT_B}/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen2.5-0.5b","prompt":"Explain KV pooling briefly.","max_tokens":32}' \
    >/dev/null
done

deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  snapshot=$(curl -fsS localhost:8080/v1/runtime/snapshot)
  a=$(jq -r '.models[] | select(.model_name == "qwen3-0.6b") | .kv_capacity_bytes' <<<"$snapshot")
  b=$(jq -r '.models[] | select(.model_name == "qwen2.5-0.5b") | .kv_capacity_bytes' <<<"$snapshot")
  (( b > a )) && break
  sleep 2
done
(( b > a ))
printf '%s\n' "$snapshot" | tee "$EVIDENCE/kv-qwen25-hot.json" | jq .
kubectl exec "$POD" -c aibrix-runtime -- kvctl list \
  | tee "$EVIDENCE/kvctl-after-policy.txt"
```

Pass criteria:

- only the model receiving requests gains `request_success_total` in that
  observation window;
- both limits remain at or above the larger of observed KV use and the
  configured floor;
- the hot model receives the safe remainder and the direction reverses when
  demand reverses;
- snapshot and `kvctl list` agree;
- controller logs contain no policy-application error.

When both observed uses are below the floor, a 4 GiB budget with a 20% floor
normally yields 3,435,973,837 and 858,993,459 bytes. This proves only that the
limit actuator and policy loop work in both directions. It does not prove that
either engine occupies that much HBM, experiences KV pressure, avoids OOM, or
runs faster.

### P2: Idle Sleep and Request-Triggered Wake

Enable the lifecycle sibling. Keep Qwen2.5 active while Qwen3 is idle:

```bash
POLICY='{"reclaim":{"mode":"kv-first","capacityBytes":4294967296,'
POLICY+='"guaranteedFloorPercent":20},"lifecycle":{"sleepAfterSeconds":60}}'
kubectl annotate deployment/warm-runtime-pool-b300 \
  "pool.aibrix.ai/policy=$POLICY" --overwrite

(
  while true; do
    kubectl exec "$POD" -c aibrix-runtime -- curl -fsS \
      "http://127.0.0.1:${PORT_B}/v1/completions" \
      -H 'Content-Type: application/json' \
      -d '{"model":"qwen2.5-0.5b","prompt":"keep alive","max_tokens":4}' \
      >/dev/null || exit
    sleep 10
  done
) &
export PEER_KEEPALIVE_PID=$!

kubectl exec "$POD" -c aibrix-runtime -- \
  nvidia-smi --query-compute-apps=pid,used_memory --format=csv,noheader \
  >"$EVIDENCE/process-hbm-before-sleep.txt"
kubectl wait --for=jsonpath='{.status.phase}'=Sleeping \
  modelclaim/qwen3-0-6b --timeout=3m
curl -fsS localhost:8080/v1/runtime/snapshot \
  | tee "$EVIDENCE/snapshot-sleeping.json" | jq .
kubectl get pod "$POD" -o json \
  | jq '.metadata.annotations["modelclaim.aibrix.ai/qwen3-0-6b"]'
kubectl exec "$POD" -c aibrix-runtime -- \
  nvidia-smi --query-compute-apps=pid,used_memory --format=csv,noheader \
  >"$EVIDENCE/process-hbm-after-sleep.txt"
```

The target must be `Sleeping`, not routable, and lower its current process HBM
after vLLM level-1 sleep. The peer must remain `Active` and continue serving.

Send 20 concurrent requests to the sleeping model. Each observed request
before readiness must receive 503; the first response includes
`Retry-After: 10`, and concurrent wake attempts are deduplicated:

```bash
seq 1 20 | xargs -P20 -I{} sh -c '
  curl -sS -D "'$EVIDENCE'/wake-{}.headers" \
    -o "'$EVIDENCE'/wake-{}.body" -w "%{http_code}\n" \
    http://127.0.0.1:8888/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "routing-strategy: random" \
    -d "{\"model\":\"qwen3-0.6b\",\"messages\":[{\"role\":\"user\",\"content\":\"wake\"}],\"max_tokens\":8}"
' | tee "$EVIDENCE/wake-statuses.txt"
grep -qi '^Retry-After: 10' "$EVIDENCE"/wake-*.headers
```

Retry with a deadline until the route is ready:

```bash
deadline=$((SECONDS + 300))
code=000
while (( SECONDS < deadline )); do
  code=$(curl -sS -o "$EVIDENCE/wake-retry.json" -w '%{http_code}' \
    http://127.0.0.1:8888/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -H 'routing-strategy: random' \
    -d '{"model":"qwen3-0.6b","messages":[{"role":"user","content":"awake?"}],"max_tokens":8}')
  [[ "$code" == 200 ]] && break
  sleep 2
done
[[ "$code" == 200 ]]
kubectl wait --for=jsonpath='{.status.phase}'=Active \
  modelclaim/qwen3-0-6b --timeout=2m
wake_count=$(kubectl -n aibrix-system logs \
  deployment/aibrix-gateway-plugins --since=5m \
  | grep 'ModelClaim request-triggered wake completed' \
  | grep -c 'qwen3-0.6b')
[[ "$wake_count" == 1 ]]

unknown=$(curl -sS -o "$EVIDENCE/unknown-model.json" -w '%{http_code}' \
  http://127.0.0.1:8888/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'routing-strategy: random' \
  -d '{"model":"does-not-exist","messages":[{"role":"user","content":"test"}],"max_tokens":1}')
[[ "$unknown" == 400 ]]
kill "$PEER_KEEPALIVE_PID"
```

The gateway does not hold the original request. Its contract is an immediate,
retryable 503 while waking, followed by a successful client retry after the
controller restores the real port.

## 7. Runtime Reliability Acceptance

Disable automatic sleep while injecting faults, ensure both claims are
`Active`, then save a new baseline registry and snapshot.

```bash
kubectl annotate deployment/warm-runtime-pool-b300 \
  'pool.aibrix.ai/policy={"reclaim":{"mode":"kv-first","capacityBytes":4294967296,"guaranteedFloorPercent":20}}' \
  --overwrite
kubectl wait --for=jsonpath='{.status.phase}'=Active \
  modelclaim/qwen3-0-6b --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Active \
  modelclaim/qwen25-0-5b --timeout=5m
kubectl exec "$POD" -c aibrix-runtime -- \
  cat /var/run/aibrix/engines.json \
  | tee "$EVIDENCE/registry-before-faults.json" | jq .
```

### R1: Agent Restart and Engine Re-adoption

Inspect the process tree and identify the `aibrix_runtime` agent child, not
`tini`, the supervisor shell, or a vLLM engine:

```bash
kubectl exec "$POD" -c aibrix-runtime -- ps -eo pid,ppid,pgid,args
export AGENT_PID=<aibrix_runtime-pid>
kubectl exec "$POD" -c aibrix-runtime -- kill -KILL "$AGENT_PID"
```

Poll for the runtime API to return, then compare the registry:

```bash
deadline=$((SECONDS + 60))
until curl -fsS localhost:8080/v1/runtime/snapshot >/dev/null; do
  (( SECONDS >= deadline )) && exit 1
  sleep 1
done
kubectl exec "$POD" -c aibrix-runtime -- \
  cat /var/run/aibrix/engines.json \
  | tee "$EVIDENCE/registry-after-agent-restart.json" | jq .
```

Pass criteria: the supervisor launches a new agent, both engines are re-adopted
with the same PID, port, and IPC name, no duplicate engine appears, and both
claims remain or return `Active` without restarting the Pod.

### R2: Isolated Engine Crash and Local Restart

Choose Qwen3 as the target and Qwen2.5 as the peer:

```bash
export TARGET_MODEL=qwen3-0.6b
export TARGET_PID=$(kubectl exec "$POD" -c aibrix-runtime -- \
  cat /var/run/aibrix/engines.json \
  | jq -er --arg model "$TARGET_MODEL" \
    '.engines[] | select(.model_name == $model) | .pid')
kubectl exec "$POD" -c aibrix-runtime -- kill -KILL "$TARGET_PID"
```

During restart, the target must be de-routed with `port:0`; the peer must keep
serving. The old process group must disappear before replacement:

```bash
kubectl exec "$POD" -c aibrix-runtime -- ps -eo pgid= \
  | awk -v pgid="$TARGET_PID" \
    '$1 == pgid {found=1} END {exit found ? 1 : 0}'

kubectl exec "$POD" -c aibrix-runtime -- curl -fsS \
  "http://127.0.0.1:${PORT_B}/v1/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-0.5b","prompt":"peer health","max_tokens":8}' \
  | jq -e '.choices | length > 0'

kubectl wait --for=jsonpath='{.status.phase}'=Active \
  modelclaim/qwen3-0-6b --timeout=5m
kubectl exec "$POD" -c aibrix-runtime -- \
  cat /var/run/aibrix/engines.json \
  | tee "$EVIDENCE/registry-after-engine-restart.json" | jq .
```

The target gets a new PID with the same port and IPC name and an incremented
`restart_count`. The peer keeps its PID and restart count.

### R3: Terminal Local Failure (Extended and Destructive)

Run this last. Repeatedly kill the target only after its previous replacement
is fully `Active`. Backoff is 2, 4, 8, 16, and 32 seconds. After the process
launched by the fifth local restart dies, no further local launch occurs.

R2 consumed the first crash and left `restart_count:1`. Drive restart counts
2 through 5, always obtaining the current PID after convergence, then kill the
count-5 process once more:

```bash
for expected_restart in 2 3 4 5; do
  kubectl wait --for=jsonpath='{.status.phase}'=Active \
    modelclaim/qwen3-0-6b --timeout=5m
  current_pid=$(kubectl exec "$POD" -c aibrix-runtime -- \
    cat /var/run/aibrix/engines.json \
    | jq -er '.engines[] | select(.model_name == "qwen3-0.6b") | .pid')
  kubectl exec "$POD" -c aibrix-runtime -- kill -KILL "$current_pid"

  deadline=$((SECONDS + 300))
  until curl -fsS localhost:8080/v1/runtime/snapshot \
    | jq -e --argjson count "$expected_restart" \
      '.models[] | select(.model_name == "qwen3-0.6b" and .phase == "active" and .restart_count == $count)' \
      >/dev/null; do
    (( SECONDS >= deadline )) && exit 1
    sleep 2
  done
done

current_pid=$(kubectl exec "$POD" -c aibrix-runtime -- \
  cat /var/run/aibrix/engines.json \
  | jq -er '.engines[] | select(.model_name == "qwen3-0.6b") | .pid')
kubectl exec "$POD" -c aibrix-runtime -- kill -KILL "$current_pid"
kubectl wait --for=jsonpath='{.status.phase}'=Failed \
  modelclaim/qwen3-0-6b --timeout=2m
```

Pass criteria:

- runtime target: `phase:"failed"`, `alive:false`, `ready:false`,
  `restart_count:5`, and non-empty `last_error`;
- claim and instance phase: `Failed`;
- Ready condition: `False` with reason `EngineFailed`;
- target route: `port:0/state:"failed"`;
- the peer continues returning HTTP 200.

Capture the final state:

```bash
curl -fsS localhost:8080/v1/runtime/snapshot \
  | tee "$EVIDENCE/snapshot-terminal-failure.json" | jq .
kubectl get modelclaim qwen3-0-6b -o yaml \
  >"$EVIDENCE/modelclaim-terminal-failure.yaml"
kubectl get pod "$POD" -o json \
  >"$EVIDENCE/warm-pod-terminal-failure.json"
kubectl logs "$POD" -c aibrix-runtime \
  >"$EVIDENCE/runtime.log"
kubectl -n aibrix-system logs deployment/aibrix-controller-manager \
  >"$EVIDENCE/controller.log"
kubectl -n aibrix-system logs deployment/aibrix-gateway-plugins \
  >"$EVIDENCE/gateway.log"
```

This is local fault convergence only. The current implementation does not move
a terminally failed claim to another warm Pod.

## 8. Optional KV Pressure Acceptance

Run this before R3, or recreate the failed claim first. It validates one
recorded workload, not an OOM-proof guarantee for arbitrary models and inputs.

1. Set both models to a fixed 2 GiB baseline with unique manual operation IDs.
2. Port-forward both engine ports from the warm Pod.
3. Drive concurrent long-context requests until `kv_used_bytes` approaches the
   configured limit.
4. Repeat with a 0.8 GiB target limit and then a 3.2 GiB target limit.
5. Record request success/error counts, waiting requests, preemptions or
   recomputations, latency, KV use/capacity, engine restarts, and GPU HBM.

Under a restrictive KV limit, expected engine behavior is increased queueing,
preemption, or recomputation when supported by the engine. Client timeouts or
rejections are possible. The acceptance condition for a chosen workload is
that the engine remains alive without a CUDA OOM or runtime restart and that
all observed degradation is recorded. A failed request is not silently counted
as a pass.

## 9. Optional Policy Benefit Benchmark

Do not publish a performance or utilization claim without this comparison.
Use the same cached weights, engine arguments, prompt trace, warmup, arrival
rates, and run duration for both configurations:

1. **Fixed baseline:** remove the reclaim annotation and manually set 2/2 GiB.
2. **Dynamic policy:** restore the 4 GiB reclaim policy with a 20% floor.
3. Replay a skewed trace against both engines, then reverse the skew.
4. Repeat each configuration at least three times in alternating order.
5. Save detailed request results and one-second GPU telemetry.

The kvcached runtime image contains the matching vLLM benchmark client. Follow
the [vLLM bench serve CLI reference](https://docs.vllm.ai/en/stable/cli/bench/serve/),
disable kvcached autopatch in the client process, and confirm image-specific
flags with `vllm bench serve --help`. A representative direct-engine
invocation is:

```bash
ENABLE_KVCACHED=false KVCACHED_AUTOPATCH=0 vllm bench serve \
  --backend openai-chat \
  --base-url http://127.0.0.1:20000 \
  --endpoint /v1/chat/completions \
  --model qwen3-0.6b \
  --dataset-name random \
  --random-input-len 1024 \
  --random-output-len 128 \
  --num-prompts 200 \
  --request-rate 4 \
  --percentile-metrics ttft,tpot,e2el \
  --metric-percentiles 50,95,99 \
  --save-result --save-detailed \
  --result-dir "$EVIDENCE/benchmark"
```

Collect at least:

- completed and failed requests, request and token throughput;
- p50/p95/p99 TTFT, TPOT, and end-to-end latency;
- SLO goodput for a declared TTFT/TPOT threshold;
- KV used/capacity, waiting requests, preemptions/recomputations;
- HBM, SM active, memory activity, and power over time;
- sleep/wake count and wake latency if lifecycle policy is included.

Report confidence intervals or at least median and range across repetitions.
Capacity limits switching in the intended direction is a functional result,
not a benefit result. If confidence intervals overlap or errors increase, state
that the test found no demonstrated benefit for that workload.

## Reference Functional Result

The required functional sequence was exercised on 2026-07-16 on one Lambda
A10 with minikube, Qwen3-0.6B, and Qwen2.5-0.5B-Instruct. It covered
activation, gateway HTTP 200, dual-model co-residency, manual operation
idempotency, agent re-adoption, isolated engine restart, terminal local failure,
automatic idle sleep, request-triggered wake, peer isolation, and cleanup.

Hardware- and version-specific observations from that run were:

- Qwen3 process HBM changed from 3134 to 1932 MiB after level-1 sleep;
- Qwen2.5 process HBM changed from 2516 to 1514 MiB after level-1 sleep;
- 20 concurrent requests to a sleeping model returned 503 and produced one
  deduplicated wake operation, then a retry returned 200;
- the 4 GiB configured KV budget changed 2/2 -> 3.2/0.8 GiB and then
  0.8/3.2 GiB as observed request demand reversed.

These are reference functional observations. No KV saturation test or
baseline-versus-policy performance experiment was completed in that run, so
they do not establish OOM safety, throughput improvement, latency improvement,
SM utilization improvement, or cost reduction.

## Troubleshooting Guardrails

- Keep `ENABLE_KVCACHED=false` and `KVCACHED_AUTOPATCH=0` on the runtime
  sidecar. The launcher enables kvcached only in child engine processes;
  autopatching the agent can import CUDA/vLLM state and cause an empty-log OOM.
- Every engine needs a unique normalized IPC name. Prefer `[a-z0-9_]`; dots and
  other characters may be normalized by kvcached.
- Do not add `--gpu-memory-utilization`. A kvcached pool controls elastic KV
  capacity through the runtime API and `kvctl`.
- vLLM sleep requires both `VLLM_SERVER_DEV_MODE=1` and
  `--enable-sleep-mode`; the dedicated runtime launcher supplies both.
- Keep a sufficiently large memory-backed `/dev/shm`. The sample uses 16 GiB.
- Level-1 sleep keeps weights in host DRAM. Check node DRAM before increasing
  resident-model density, especially for larger models.
- HBM release can lag the sleep response by several seconds. The first request
  after wake can include rebuild overhead; use later requests for steady-state
  latency.
- Automatic reclaim/lifecycle policy currently applies only to verified
  single-GPU vLLM pools. Multi-GPU and SGLang runtimes are skipped.
- `hbm_peak_bytes` and free-HBM fields are advisory placement observations,
  not hard capacity reservations or launch guarantees.

## 10. Test Resource Cleanup

Delete claims before the pool so finalizers can stop engines cleanly:

```bash
kubectl delete -f samples/modelclaim/modelclaims.yaml --ignore-not-found
kubectl wait --for=delete modelclaim/qwen3-0-6b --timeout=5m || true
kubectl wait --for=delete modelclaim/qwen25-0-5b --timeout=5m || true
kubectl delete -f samples/modelclaim/warm-runtime-pool.yaml --ignore-not-found
kubectl get modelclaims,pods -A >"$EVIDENCE/cluster-after-cleanup.txt"
```

Do not run `minikube delete`, stop the host, or terminate the Lambda instance
when using the normal reusable-cluster path. If this run created an ephemeral
instance through Appendix A, complete the additional teardown recorded there.

## Result Record

| ID | Result | Evidence or key number | Notes |
|---|---|---|---|
| F1 activation/observation/density | pass / fail | startup; PIDs/ports/IPCs ___ | |
| F2 direct/gateway inference | pass / fail | HTTP results ___ | |
| F3 validation/topology | pass / fail | reasons ___ | |
| F4 manual idempotency | pass / fail | second `applied:false` ___ | |
| P1 dynamic KV limits | pass / fail | A/B limits both directions ___ | limit only |
| P2 sleep/wake | pass / fail | HBM delta ___; wake ___ s | |
| R1 agent re-adoption | pass / fail | unchanged engine PIDs ___ | |
| R2 engine isolation | pass / fail | restart count ___ | |
| R3 terminal failure | pass / fail / skipped | final state ___ | destructive |
| S1 KV pressure | pass / fail / not run | load and limit ___ | no generalization |
| B1 policy benefit | pass / fail / not run | repeated metrics ___ | no claim if not run |
| Kubernetes resource cleanup | pass / fail | claims and pool absent ___ | mandatory |
| Ephemeral instance cleanup | pass / fail / not applicable | instance absent at ___ UTC | conditional |

## Appendix A: Optional Lambda Cluster Setup

Use this appendix only when an existing Lambda GPU cluster is unavailable.
The main acceptance procedure does not require creating a new instance.

1. Provision one A10 instance using the Lambda console or API and a temporary
   SSH key. Record the instance ID, owner, creation time, and teardown deadline
   outside the repository.
2. Arm an independent expiration or watchdog immediately. Never place a cloud
   API key in the repository, shell history, evidence directory, or logs.
3. Follow the
   [AIBrix Lambda Cloud guide](https://aibrix.readthedocs.io/latest/getting_started/installation/lambda.html)
   through NVIDIA container runtime and minikube setup. Skip any release-pinned
   AIBrix installation; section 3 installs the current nightly control plane.
4. Run the preflight checks in section 1 before continuing.
5. After section 10, delete the ephemeral minikube cluster, terminate the exact
   instance through the Lambda console or API, and verify that its instance ID
   is absent. Delete the temporary SSH key afterward.

Never use `shutdown`, `poweroff`, or an OS halt as cloud cleanup. Those actions
do not terminate the billable Lambda resource. Consult the
[Lambda Cloud API documentation](https://docs.lambda.ai/public-cloud/cloud-api/)
for API-based lifecycle operations.
