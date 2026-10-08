# ModelClaim Validation Runbook

This runbook checks ModelClaim end to end on real GPUs: placement and card
division, sleep and wake, and recovery from engine faults. Run it before a
release, or after a change to the controller, the gateway or the runtime. Each
check lists its commands and pass criteria, every wait is bounded, and the
commands save evidence under one directory.

The checks need one node with an NVIDIA GPU of at least 24 GB. The optional
TP=2 check needs a second GPU. Use a cluster you may disrupt: check R3 kills an
engine until it fails for good.

A passing run does not show that the feature improves throughput or latency,
that a KV limit prevents OOM under heavy load, or anything about SGLang.

## 1. Set Up

### Revision and evidence

```bash
git clone https://github.com/vllm-project/aibrix.git
cd aibrix
git checkout <commit-or-branch>
export TEST_COMMIT=$(git rev-parse HEAD)
export NS=default
export RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-${TEST_COMMIT:0:12}"
export EVIDENCE="$HOME/modelclaim-evidence/$RUN_ID"
mkdir -p "$EVIDENCE"
git show -s --format=fuller HEAD >"$EVIDENCE/commit.txt"
```

The samples create their objects in the `default` namespace, and every command
below names it.

### Control plane

`config/default` installs the nightly controller and gateway built from
`main`. To test changes that are not on `main`, build and deploy your own
images instead.

```bash
kubectl apply -k config/dependency --server-side
kubectl apply -k config/crd --server-side
kubectl apply -k config/default
kubectl -n aibrix-system rollout status deployment/aibrix-controller-manager --timeout=10m
kubectl -n aibrix-system rollout status deployment/aibrix-gateway-plugins --timeout=10m
kubectl -n aibrix-system get pods \
  -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[*].image,IMAGE_ID:.status.containerStatuses[*].imageID' \
  >"$EVIDENCE/control-plane-images.txt"
```

### Runtime image

The warm-pool sample uses `aibrix/kvcached-runtime:nightly`, which is built
from `main`. To test another commit, build the runtime from it. Then make the
image available to the node, for example with `minikube image load`, or push
it to a registry the node can reach:

```bash
IMAGE_TAG=dev IS_MAIN_BRANCH=false make docker-build-kvcached-runtime
export RUNTIME_IMAGE=aibrix/kvcached-runtime:dev
```

Otherwise, use the nightly image:

```bash
export RUNTIME_IMAGE=aibrix/kvcached-runtime:nightly
```

### Warm pool

```bash
sed "s#aibrix/kvcached-runtime:nightly#$RUNTIME_IMAGE#" samples/modelclaim/warm-runtime-pool.yaml \
  | kubectl -n "$NS" apply -f -
kubectl -n "$NS" rollout status deployment/warm-runtime-pool-b300 --timeout=15m
export POD=$(kubectl -n "$NS" get pod -l app=warm-runtime-pool-b300 -o jsonpath='{.items[0].metadata.name}')
```

Keep a port-forward to Envoy running in another terminal:

```bash
ENVOY=$(kubectl -n envoy-gateway-system get service \
  --selector=gateway.envoyproxy.io/owning-gateway-namespace=aibrix-system,gateway.envoyproxy.io/owning-gateway-name=aibrix-eg \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n envoy-gateway-system port-forward "service/$ENVOY" 8888:80
```

### Helpers

Define these in the main terminal:

```bash
GIB=$((1024 ** 3))
runtime() {
  local path=$1; shift
  kubectl -n "$NS" exec "$POD" -c aibrix-runtime -- curl -fsS "localhost:8080$path" "$@"
}
snapshot() { runtime /v1/runtime/snapshot; }
model() { snapshot | jq --arg m "$1" '.models[] | select(.model_name == $m)'; }
registry() {
  kubectl -n "$NS" exec "$POD" -c aibrix-runtime -- cat /var/run/aibrix/engines.json
}
pid() { registry | jq -r --arg m "$1" '.engines[] | select(.model_name == $m) | .pid'; }
annotation() {
  kubectl -n "$NS" get pod "$POD" -o json | jq -r --arg k "$1" '.metadata.annotations[$k] // empty'
}
route() { annotation "route.claim.model.aibrix.ai/$1"; }
wake_request() { annotation "wake.modelclaim.aibrix.ai/$1"; }
conditions() {
  kubectl -n "$NS" get modelclaim "$1" \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
}
record() {
  kubectl -n "$NS" get modelclaim "$1" -o jsonpath='{.status.instances[0].kvLimitBytes}'
}
events() {
  kubectl -n "$NS" get events --sort-by=.lastTimestamp \
    --field-selector "involvedObject.kind=ModelClaim,involvedObject.name=$1" \
    -o custom-columns='TIME:.lastTimestamp,TYPE:.type,REASON:.reason,MESSAGE:.message'
}
chat() {
  curl -sS -o /dev/null -w '%{http_code}\n' localhost:8888/v1/chat/completions \
    -H 'Content-Type: application/json' -H 'routing-strategy: random' \
    -d "{\"model\":\"$1\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with OK.\"}],\"max_tokens\":8}"
}
serves() { [[ $(chat "$1") == 200 ]]; }
wait_phase() { kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'="$2" "modelclaim/$1" --timeout="${3:-10m}"; }
wait_reason() {
  kubectl -n "$NS" wait --for=jsonpath="{.status.conditions[?(@.type==\"$2\")].reason}=$3" \
    "modelclaim/$1" --timeout="${4:-2m}"
}
eventually() {
  local deadline=$((SECONDS + $1)); shift
  until "$@"; do (( SECONDS < deadline )) || return 1; sleep 2; done
}
claim() {
  kubectl -n "$NS" apply -f - <<EOF
apiVersion: model.aibrix.ai/v1alpha1
kind: ModelClaim
metadata:
  name: $1
spec:
  podSelector:
    matchLabels:
      claim.model.aibrix.ai/pool: b300-pool-a
  artifactURL: huggingface://Qwen/Qwen2.5-0.5B-Instruct
$2
EOF
}
```

`runtime` calls the runtime agent from inside the Pod, since a port-forward to
the agent would end when R1 kills it. `claim` creates a claim on the sample
pool. Its second argument holds more spec fields, indented by two spaces.

## 2. Activation and Routing

### A1: Two models share the GPU

```bash
kubectl -n "$NS" apply -f samples/modelclaim/modelclaims.yaml
wait_phase qwen3-0-6b Active 15m
wait_phase qwen25-0-5b Active 15m
export CARD=$(snapshot | jq '[.accelerators[].hbm_usable_bytes] | min')
snapshot >"$EVIDENCE/a1-snapshot.json"
kubectl -n "$NS" get modelclaims -o yaml >"$EVIDENCE/a1-claims.yaml"
for m in qwen3-0.6b qwen2.5-0.5b; do
  model "$m" | jq '{model_name, phase, alive, ready, port, ipc_name, kv_capacity_bytes}'
done
route qwen3-0-6b; route qwen25-0-5b
registry | tee "$EVIDENCE/a1-registry.json" | jq '.engines[] | {model_name, pid, port, ipc_name}'
```

`CARD` is the usable memory of the GPU, which the controller divides among the
engines on it.

Pass criteria:

- both claims are `Active` with `readyReplicas: 1`, and each has one instance
  on the warm Pod;
- both engines are `active`, alive and ready, with distinct PIDs, ports and IPC
  names;
- each route reads `{"model": ..., "port": <engine port>, "state": "active",
  "wakeByRequest": true}`.

### A2: Requests reach each engine

```bash
for m in qwen3-0.6b qwen2.5-0.5b; do
  port=$(model "$m" | jq -r .port)
  kubectl -n "$NS" exec "$POD" -c aibrix-runtime -- curl -fsS "http://127.0.0.1:$port/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with OK.\"}],\"max_tokens\":8}" \
    | jq -e '.choices | length > 0'
  eventually 60 serves "$m"
done
[[ $(chat does-not-exist) == 400 ]]
```

Pass criteria: both models answer directly and through the gateway, and the
gateway answers 400 for a model it does not know.

## 3. Validation

Each claim below must not run.

```bash
# V1: only one replica is allowed, so the API server refuses this claim.
! claim two-replicas "  replicas: 2
  perGPU: {maximumFootprint: 6Gi, kvFloor: 1Gi}"

# V2: a name longer than 63 characters is refused.
! claim "$(printf 'm%.0s' {1..64})" "  perGPU: {maximumFootprint: 6Gi, kvFloor: 1Gi}"

# V3: kvcached owns the KV memory, so a fixed vLLM allocation fails the claim.
claim fixed-kv "  perGPU: {maximumFootprint: 6Gi, kvFloor: 1Gi}
  engineConfig: {args: {\"--gpu-memory-utilization\": \"0.45\"}}"
wait_phase fixed-kv Failed 1m
wait_reason fixed-kv Ready InvalidEngineConfig

# V4: a claim that declares no per-GPU cost is not placed.
claim no-cost ""
wait_reason no-cost Scheduled InvalidPerGPU

# V5: a TP=2 claim needs a two-GPU Pod, so the one-GPU Pod is no candidate.
claim tp2-on-one-gpu "  perGPU: {maximumFootprint: 6Gi, kvFloor: 1Gi}
  engineConfig: {args: {\"--tensor-parallel-size\": \"2\"}}"
wait_reason tp2-on-one-gpu Scheduled NoMatchingPods

# V6: a claim larger than the card is refused for good.
claim too-large "  perGPU: {maximumFootprint: $((2 * CARD)), kvFloor: 1Gi}"
wait_reason too-large Scheduled TooLargeForAnyCard

# V7: only perGPU may change after a claim is created.
! kubectl -n "$NS" patch modelclaim qwen3-0-6b --type merge \
  -p '{"spec":{"artifactURL":"huggingface://Qwen/Qwen2.5-0.5B-Instruct"}}'

kubectl -n "$NS" get modelclaim no-cost tp2-on-one-gpu too-large \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase'
snapshot | jq '[.models[].model_name]'
kubectl -n "$NS" delete modelclaim fixed-kv no-cost tp2-on-one-gpu too-large
```

Pass criteria: every command succeeds, `no-cost`, `tp2-on-one-gpu` and
`too-large` stay `Pending`, and the snapshot lists only the two sample models.

## 4. Card Division

### C1: The card is divided among the engines

Each engine's KV limit is its share of the card. The declared footprints and
the limits add up to the whole card. The sample declares a 6 GiB footprint for
each model.

```bash
LIMITS=$(( $(record qwen3-0-6b) + $(record qwen25-0-5b) ))
echo "card $CARD, limits $LIMITS"
[[ $((LIMITS + 2 * 6 * GIB)) == "$CARD" ]]
[[ $(model qwen3-0.6b | jq .kv_capacity_bytes) == $(record qwen3-0-6b) ]]
[[ $(model qwen2.5-0.5b | jq .kv_capacity_bytes) == $(record qwen25-0-5b) ]]
```

Pass criteria: all three comparisons hold.

### C2: A claim waits for room

The new claim needs 1 GiB more than the card has left. It is placed once
another claim gives its room back.

```bash
claim waits-for-room "  perGPU: {maximumFootprint: $((CARD - 14 * GIB)), kvFloor: 1Gi}"
wait_reason waits-for-room Scheduled NoMatchingPods
kubectl -n "$NS" get modelclaim waits-for-room \
  -o jsonpath='{.status.conditions[?(@.type=="Scheduled")].message}{"\n"}'
[[ $(chat waits-for-room) == 503 ]]
kubectl -n "$NS" delete modelclaim qwen25-0-5b
wait_phase waits-for-room Active 15m
eventually 60 serves waits-for-room
kubectl -n "$NS" delete modelclaim waits-for-room
kubectl -n "$NS" apply -f samples/modelclaim/modelclaims.yaml
wait_phase qwen25-0-5b Active 15m
```

Pass criteria: while the card is full, the claim stays `Pending` and the
gateway answers 503. Its `Scheduled` message says how much it needs and how
much the Pod has free. Once `qwen25-0-5b` is deleted, the claim becomes
`Active` and serves.

### C3: A limit changed behind the controller's back is put back

Raise one engine above its recorded limit through the runtime API, sending the
same operation ID twice:

```bash
for i in 1 2; do
  runtime /v1/runtime/models/kv-limit -H 'Content-Type: application/json' \
    -d "{\"model_name\":\"qwen3-0.6b\",\"limit_bytes\":$(( $(record qwen3-0-6b) + GIB )),\"operation_id\":\"$RUN_ID-c3\"}" \
    | tee "$EVIDENCE/c3-write-$i.json"
done
jq -e '.applied == true' "$EVIDENCE/c3-write-1.json"
jq -e '.applied == false' "$EVIDENCE/c3-write-2.json"
at_record() { [[ $(model qwen3-0.6b | jq .kv_capacity_bytes) == $(record qwen3-0-6b) ]]; }
eventually 60 at_record
eventually 60 serves qwen3-0.6b
events qwen3-0-6b >"$EVIDENCE/c3-events.txt"
```

Pass criteria: the first write applies and the repeat does not, and within a
minute the engine is back at its recorded limit and serves. If the claim's own
check saw the change first, the claim shows `KVLimitNotHeld` and then
`KVLimitSet` Events, and the route was not active in between. If the card's
next division saw it first, the limit is put back without an Event.

## 5. Sleep and Wake

### S1: An idle model goes to sleep

Keep `qwen2.5-0.5b` busy and leave `qwen3-0.6b` idle:

```bash
kubectl -n "$NS" annotate deployment/warm-runtime-pool-b300 --overwrite \
  'claim.model.aibrix.ai/pool-policy={"lifecycle":{"sleepAfterSeconds":60}}'
( while sleep 10; do chat qwen2.5-0.5b >/dev/null; done ) &
KEEPALIVE=$!
wait_phase qwen3-0-6b Sleeping 5m
model qwen3-0.6b | tee "$EVIDENCE/s1-sleeping.json" | jq '{phase, ready, sleeping_footprint_bytes}'
route qwen3-0-6b
events qwen3-0-6b | tail -3
```

Pass criteria:

- `qwen3-0-6b` is `Sleeping`, with Ready reason `EngineSleeping` and a
  `Sleeping` Event that names the idle time;
- its route reads `"state": "sleeping"` with port 0, and the engine is
  `sleeping` in the snapshot;
- `qwen25-0-5b` stays `Active`.

`sleeping_footprint_bytes` is what the sleeping engine still holds on the GPU.
It is null when the runtime cannot attribute GPU memory to the engine's
processes. S3 and S4 need a reading.

### S2: A request wakes it

Send 20 requests at once, then retry until the model answers:

```bash
pids=()
for i in $(seq 20); do
  curl -sS -o /dev/null -D "$EVIDENCE/s2-$i.headers" localhost:8888/v1/chat/completions \
    -H 'Content-Type: application/json' -H 'routing-strategy: random' \
    -d '{"model":"qwen3-0.6b","messages":[{"role":"user","content":"wake"}],"max_tokens":8}' &
  pids+=($!)
done
wait "${pids[@]}"
grep -h '^HTTP' "$EVIDENCE"/s2-*.headers | sort | uniq -c
grep -il '^Retry-After: 10' "$EVIDENCE"/s2-*.headers | wc -l
wake_request qwen3-0-6b
eventually 300 serves qwen3-0.6b
wait_phase qwen3-0-6b Active 2m
events qwen3-0-6b | tail -4
kubectl -n aibrix-system logs -l app=gateway-plugins -c gateway-plugin --since=10m --tail=-1 \
  | grep -c 'asked the controller to wake a ModelClaim'
wake_request qwen3-0-6b
```

Pass criteria:

- every request got 503 with `Retry-After: 10`;
- the Pod carries one wake request, `wake.modelclaim.aibrix.ai/qwen3-0-6b`,
  holding the time of the first request, and the gateway logged it once;
- the claim shows `Waking` and then `Woken` Events, a retry returns 200, and
  the wake request is gone once the claim is `Active`.

### S3: A sleeping model gives its room back, and gets it back

With `noWakeReserveWhileAsleep`, a sleeping engine counts only at what it
holds asleep, so a claim that needs 3 GiB more than the card has left fits.
When the sleeper is asked for again, the controller puts an idle neighbour to
sleep to make room for it.

```bash
kubectl -n "$NS" annotate deployment/warm-runtime-pool-b300 --overwrite \
  'claim.model.aibrix.ai/pool-policy={"lifecycle":{"sleepAfterSeconds":3600,"noWakeReserveWhileAsleep":true,"sleepToMakeRoomAfterSeconds":30}}'
kill "$KEEPALIVE"
for i in 1 2; do
  runtime /v1/runtime/models/sleep -H 'Content-Type: application/json' \
    -d "{\"model_name\":\"qwen3-0.6b\",\"level\":1,\"operation_id\":\"$RUN_ID-s3\"}" \
    | tee "$EVIDENCE/s3-sleep-$i.json"
done
jq -e '.applied == true' "$EVIDENCE/s3-sleep-1.json"
jq -e '.applied == false' "$EVIDENCE/s3-sleep-2.json"
wait_phase qwen3-0-6b Sleeping 2m
model qwen3-0.6b | jq .sleeping_footprint_bytes

claim fits-after-release "  perGPU: {maximumFootprint: $((CARD - 12 * GIB)), kvFloor: 1Gi}"
wait_phase fits-after-release Active 15m

[[ $(chat qwen3-0.6b) == 503 ]]
eventually 300 serves qwen3-0.6b
for c in qwen3-0-6b qwen25-0-5b fits-after-release; do events "$c" | tail -4; done \
  | tee "$EVIDENCE/s3-events.txt"
```

Pass criteria:

- the first sleep call applies and the repeat does not;
- `fits-after-release` becomes `Active` while `qwen3-0-6b` sleeps;
- the wake waits first: `qwen3-0-6b` shows a `WaitingForRoom` Event;
- one idle neighbour gets a `SleptToMakeRoom` Event, and then `qwen3-0-6b`
  wakes and answers 200.

This holds when the sleeping engine holds less than 4 GiB, as its footprint
reading shows. Clean up, and wake both sample models:

```bash
kubectl -n "$NS" delete modelclaim fits-after-release
eventually 300 serves qwen3-0.6b
eventually 300 serves qwen2.5-0.5b
```

### S4: The controller makes room for a new claim

Both models are awake. Wait until they have been idle for 30 seconds. Then a
new claim that needs 3 GiB more than the card has left makes the controller
put one of them to sleep.

```bash
sleep 40
claim needs-room "  perGPU: {maximumFootprint: $((CARD - 12 * GIB)), kvFloor: 1Gi}"
wait_phase needs-room Active 15m
events needs-room | tee "$EVIDENCE/s4-events.txt"
for c in qwen3-0-6b qwen25-0-5b; do events "$c" | tail -3; done \
  | tee -a "$EVIDENCE/s4-events.txt"
kubectl -n "$NS" delete modelclaim needs-room
kubectl -n "$NS" annotate deployment/warm-runtime-pool-b300 'claim.model.aibrix.ai/pool-policy-'
eventually 300 serves qwen3-0.6b
eventually 300 serves qwen2.5-0.5b
```

Pass criteria: `needs-room` shows a `MakingRoom` Event that names the model put
to sleep, that model shows `SleptToMakeRoom`, and `needs-room` becomes
`Active`. The last two commands wake the sleeping model again.

## 6. Runtime Reliability

Both claims are `Active`, and the pool has no policy.

### R1: The agent restarts and adopts its engines

```bash
registry >"$EVIDENCE/r1-before.json"
AGENT_PID=$(kubectl -n "$NS" exec "$POD" -c aibrix-runtime -- ps -eo pid,args \
  | awk '/aibrix\.app/ {print $1; exit}')
kubectl -n "$NS" exec "$POD" -c aibrix-runtime -- kill -KILL "$AGENT_PID"
eventually 60 snapshot >/dev/null
registry >"$EVIDENCE/r1-after.json"
diff <(jq -S '[.engines[] | {model_name, pid, port, ipc_name}]' "$EVIDENCE/r1-before.json") \
     <(jq -S '[.engines[] | {model_name, pid, port, ipc_name}]' "$EVIDENCE/r1-after.json")
```

Pass criteria: a new agent answers within a minute, both engines keep their
PID, port and IPC name, no engine is duplicated, and both claims stay `Active`.

### R2: One engine crashes and restarts alone

```bash
OLD_PID=$(pid qwen3-0.6b)
kubectl -n "$NS" exec "$POD" -c aibrix-runtime -- kill -KILL "$OLD_PID"
not_active() { [[ $(route qwen3-0-6b | jq -r .state) != active ]]; }
eventually 30 not_active
serves qwen2.5-0.5b
wait_phase qwen3-0-6b Active 5m
echo "pid $OLD_PID -> $(pid qwen3-0.6b)"
model qwen3-0.6b | jq '{port, ipc_name, restart_count}'
```

Pass criteria: while `qwen3-0.6b` restarts, its route is not active and
`qwen2.5-0.5b` keeps serving. It comes back with a new PID, the same port and
IPC name, and `restart_count: 1`.

### R3: An engine that keeps crashing fails for good

The runtime restarts an engine after 2, 4, 8, 16 and 32 seconds, five times in
the engine's life. Kill it each time it is back, then once more:

```bash
for count in 2 3 4 5; do
  wait_phase qwen3-0-6b Active 5m
  kubectl -n "$NS" exec "$POD" -c aibrix-runtime -- kill -KILL "$(pid qwen3-0.6b)"
  restarted() { [[ $(model qwen3-0.6b | jq "select(.phase == \"active\") | .restart_count") == "$count" ]]; }
  eventually 300 restarted
done
wait_phase qwen3-0-6b Active 5m
kubectl -n "$NS" exec "$POD" -c aibrix-runtime -- kill -KILL "$(pid qwen3-0.6b)"
wait_phase qwen3-0-6b Failed 2m
model qwen3-0.6b | jq '{phase, alive, restart_count, last_error}'
route qwen3-0-6b
conditions qwen3-0-6b
events qwen3-0-6b | tail -4
serves qwen2.5-0.5b
```

Pass criteria:

- the engine is `failed`, not alive, with `restart_count: 5` and a
  `last_error`;
- the claim is `Failed`, with Ready reason `EngineFailed`, and its route reads
  `"state": "failed"` with port 0;
- with no other warm Pod, the `Scheduled` condition says the model cannot move
  from the failed Pod. With one, the controller moves the claim there instead
  (`Rescheduled`);
- `qwen2.5-0.5b` keeps serving.

## 7. Deletion

Delete the claims before the pool, so the controller can stop their engines:

```bash
kubectl -n "$NS" delete -f samples/modelclaim/modelclaims.yaml --wait=false
kubectl -n "$NS" wait --for=delete modelclaim/qwen3-0-6b modelclaim/qwen25-0-5b --timeout=3m
kubectl -n "$NS" get pod "$POD" -o json \
  | jq '[.metadata.annotations | keys[] | select(startswith("route.claim.model.aibrix.ai/"))]'
no_engines() { [[ $(snapshot | jq '.models | length') == 0 ]]; }
eventually 60 no_engines
kubectl -n "$NS" delete -f samples/modelclaim/warm-runtime-pool.yaml
```

Pass criteria: both claims are gone within three minutes, the Pod has no route
left, and the runtime has no engine. A serving engine is given up to 90
seconds to finish its requests before it is stopped.

## 8. Optional: A TP=2 Pool

This needs a node with two free GPUs. Create a second pool whose Pod requests
two GPUs, and a TP=2 claim for it:

```bash
sed -e "s#aibrix/kvcached-runtime:nightly#$RUNTIME_IMAGE#" -e 's/b300-pool-a/tp2-pool/' \
    -e 's/warm-runtime-pool-b300/warm-runtime-pool-tp2/' -e 's#nvidia.com/gpu: "1"#nvidia.com/gpu: "2"#' \
    samples/modelclaim/warm-runtime-pool.yaml | kubectl -n "$NS" apply -f -
kubectl -n "$NS" rollout status deployment/warm-runtime-pool-tp2 --timeout=15m
kubectl -n "$NS" apply -f - <<'EOF'
apiVersion: model.aibrix.ai/v1alpha1
kind: ModelClaim
metadata:
  name: qwen25-tp2
spec:
  podSelector:
    matchLabels:
      claim.model.aibrix.ai/pool: tp2-pool
  artifactURL: huggingface://Qwen/Qwen2.5-0.5B-Instruct
  perGPU: {maximumFootprint: 6Gi, kvFloor: 1Gi}
  engineConfig:
    args: {"--tensor-parallel-size": "2", "--max-model-len": "2048"}
EOF
wait_phase qwen25-tp2 Active 15m
eventually 60 serves qwen25-tp2
kubectl -n "$NS" delete modelclaim qwen25-tp2
kubectl -n "$NS" delete deployment warm-runtime-pool-tp2
kubectl -n "$NS" delete service warm-runtime-pool-tp2-metrics
```

Pass criteria: the claim becomes `Active` on the two-GPU Pod and serves.

## Result Record

| ID | Check | Result | Evidence or key numbers |
|---|---|---|---|
| A1 | two models share the GPU | pass / fail | PIDs, ports, IPC names |
| A2 | requests reach each engine | pass / fail | |
| V1-V7 | validation | pass / fail | |
| C1 | card division | pass / fail | card, limits |
| C2 | waiting for room | pass / fail | |
| C3 | limit put back | pass / fail | Events |
| S1 | idle sleep | pass / fail | sleeping footprint |
| S2 | wake on request | pass / fail | 503 count, wake request time |
| S3 | room given back and made | pass / fail | Events |
| S4 | room made for a new claim | pass / fail | Events |
| R1 | agent restart | pass / fail | |
| R2 | engine crash | pass / fail | restart count |
| R3 | terminal failure | pass / fail / skipped | |
| D | deletion | pass / fail | |
| TP2 | TP=2 pool | pass / fail / not run | |

## Notes

- Keep `ENABLE_KVCACHED=false` and `KVCACHED_AUTOPATCH=0` on the runtime
  container. The runtime enables kvcached only in each engine process.
- Do not pass `--gpu-memory-utilization`; kvcached owns the KV memory.
- Level-1 sleep keeps a model's weights in host memory, and ModelClaim does not
  account for host memory. Check the node's free memory before you put many
  models to sleep on it.
- Where the runtime cannot attribute GPU memory to an engine's processes, the
  sleeping footprint reads null, and a sleeping engine keeps its whole reserve.
- The controller's Kubernetes client drops a claim's Events after 25 in a burst,
  then lets one through every five minutes. Late in a run, a busy claim can
  miss an Event. Its phase, conditions and route still show what happened.
