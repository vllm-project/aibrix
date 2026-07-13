# ModelClaim Phase-2 Foundation

## Goal

Phase 1 proved the basic mechanism: a `ModelClaim` starts a kvcached-enabled
engine in a warm GPU pod, waits for readiness, advertises a route, and stops it
on deletion. Phase 2 adds the smallest cluster-control foundation needed to
make placement and manual lifecycle operations observable and safe.

The scope is deliberately narrower than a full autoscaler or a new scheduling
API. `ModelClaim.spec` remains unchanged. In particular, `engineConfig.args`
continues to be the engine startup pass-through; it is not a resource or
topology contract.

The strategy is informed by the memory-pressure framing in the Prism paper
([arXiv:2505.04021](https://arxiv.org/abs/2505.04021)): placement needs live
memory and locality signals, while fast engine-local actions remain behind a
runtime boundary. AIBrix implements that separation without copying Prism's
engine-integrated scheduler.

## Implemented Foundation

| Area | Phase-2 foundation behavior |
| --- | --- |
| Runtime truth | Each warm pod's `aibrix-runtime` sidecar reports a live snapshot. |
| Controller state | The controller caches snapshots in memory for five seconds. No state CRD is created. |
| Placement | Cached weights, live HBM/KV state, and existing locality/load/name fallbacks rank eligible warm pods. |
| KV control | The runtime exposes an idempotent `kvctl limit` operation per model IPC segment. |
| vLLM lifecycle | The runtime exposes idempotent manual sleep and wake controls for vLLM only. |
| Route safety | A runtime-reported sleeping engine remains assigned but receives the non-routable `port:0` annotation. |

The runtime sidecar is the source of truth. A controller restart loses only its
bounded cache and reconstructs it by polling sidecars. This avoids a
`ModelNodeState` CRD and avoids persisting a second, stale view of GPU state.

## Runtime Contract

### Snapshot

`GET /v1/runtime/snapshot` returns the scheduling inputs for one warm pod:

```json
{
  "observed_at": "2026-07-13T12:00:00Z",
  "accelerators": [
    {"id": "GPU-0", "hbm_total_bytes": 25757220864, "hbm_free_bytes": 18000000000}
  ],
  "models": [
    {
      "model_name": "qwen3-0.6b",
      "artifact_url": "hf://Qwen/Qwen3-0.6B",
      "claim_ref": {"namespace": "default", "name": "qwen3-0-6b", "uid": "..."},
      "port": 20000,
      "ipc_name": "kvc_qwen3-0-6b",
      "phase": "active",
      "ready": true,
      "kv_used_bytes": 0,
      "kv_capacity_bytes": 0
    }
  ],
  "cached_artifacts": ["hf://Qwen/Qwen3-0.6B"]
}
```

GPU observations use NVML when it is available. CPU-only development hosts
return an empty `accelerators` list rather than failing. KV accounting is read
from kvcached's per-engine `/dev/shm/<ipc-name>` metadata. Cache markers are a
locality hint only; a missing or malformed marker never makes placement fail.

### Placement

For each eligible warm pod, the controller derives a `PodPlacementState` and
ranks candidates in this order:

1. Requested artifact is already cached on the pod's shared weight cache.
2. A fresh runtime snapshot is available.
3. HBM observation is available.
4. More free HBM is preferred.
5. Lower aggregate KV use is preferred.
6. Fewer resident engines is preferred.
7. Existing locality cost, current claim load, and pod name break ties.

The cache TTL is five seconds. If refresh fails after expiry, the controller
does not use the stale snapshot; it falls back to the existing deterministic
placement behavior. Because engines are not yet mapped to individual GPUs, a
multi-GPU snapshot uses the smallest reported free-HBM value conservatively.
That is an observation safeguard, not TP/PP support.

### Manual Control Operations

These sidecar endpoints are intended for controller or private administrative
traffic. Every mutation includes a non-empty `operation_id`. Replaying a
completed operation ID returns `applied: false`; a failed underlying action is
not recorded, so it can be retried.

```text
POST /v1/runtime/models/kv-limit
{"model_name":"qwen3-0.6b","limit_bytes":1073741824,"operation_id":"..."}

POST /v1/runtime/models/sleep
{"model_name":"qwen3-0.6b","level":1,"operation_id":"..."}

POST /v1/runtime/models/wake
{"model_name":"qwen3-0.6b","operation_id":"..."}
```

Each returns `status`, `model_name`, `operation_id`, `applied`, and `phase`.
`kv-limit` invokes `kvctl limit <normalized-ipc-name> <bytes>`. Sleep and wake
are vLLM-only. Unsupported engines return HTTP 409; an absent model returns
HTTP 404. The sidecar invokes vLLM's localhost development endpoints rather
than exposing those endpoints directly to clients.

`kvcached` and vLLM sleep remain orthogonal:

- kvcached changes the elastic KV footprint of an active engine.
- vLLM sleep offloads the whole idle engine according to sleep level.

No automatic policy currently calls either operation.

## Routing State

The existing route annotation remains the data-plane contract:

```text
modelclaim.aibrix.ai/<claim-name> = {"model":"<served-model>","port":<port>}
```

`port:0` is deliberately non-routable. It is used while an engine is booting,
is unhealthy, or is sleeping. The relevant status phases are:

| Runtime observation | Instance / claim state | Route |
| --- | --- | --- |
| `active`, `ready=true` | `Active` | real engine port |
| `active`, `ready=false` | `Activating` | `port:0` |
| `sleeping` | `Sleeping` | `port:0` |

A sleeping instance stays in `status.instances`, so the controller does not
activate a duplicate engine merely because a model is asleep. After a wake,
the route is restored only once the runtime reports `active` and ready.

## Explicitly Deferred

The following are intentionally absent from this foundation:

- No `ModelNodeState` CRD, `ModelPoolPolicy` CRD, node daemon, or persisted
  scheduling graph.
- No new `ModelClaim.spec` fields for parallelism, SLO, priority, or KV budget.
- No controller idle detector, automatic sleep/stop loop, or request-triggered
  wake path.
- No gateway hold. Requests to a non-routable model are not held by this work.
- No TP/PP placement, multi-GPU grouping, cross-pod parallelism, or live
  migration.

The absence of an automatic lifecycle policy is a safety decision. An idle
timer based on activation time is not request idleness, and without a durable
wake source it could leave a model permanently unavailable.

If a future policy needs a configuration surface, prefer one versioned JSON
value on the warm-pool Deployment over a new CRD, for example:

```yaml
metadata:
  annotations:
    modelclaim.aibrix.ai/pool-policy: >-
      {"version":"v1alpha1","lifecycle":{"sleepAfterSeconds":60,"stopAfterSeconds":900}}
```

This example is a deferred proposal. Nothing in the current controller reads
or acts on it.

## Support Boundary and Next Work

The supported validation path is one GPU per warm runtime pod and independent
single-GPU engines. vLLM and kvcached may have broader engine-level support,
but ModelClaim does not yet schedule GPU groups, so TP/PP is not a supported
ModelClaim configuration. Passing `--tensor-parallel-size` in `engineConfig`
does not create a resource matching contract.

The next policy work should be sequenced as follows:

1. Define a durable wake source and user-visible behavior before enabling any
   automatic sleep or stop policy.
2. Add topology/resource matching before claiming TP/PP support.
3. Add measured memory-pressure policy only after request activity and wake
   semantics are reliable.
4. Evaluate gateway hold or admission control separately from placement.

Use [the Phase-2 foundation runbook](phase2-foundation-runbook.md) for local,
minikube, and A10 validation.
