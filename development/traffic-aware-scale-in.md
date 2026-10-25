# Coordinated traffic-aware RoleSet scale-in

This opt-in mode closes admission before it considers a replica idle. A Gateway
with an old Pod snapshot must still reserve the request in the shared coordinator
before returning a target to Envoy. The controller atomically seals the same state
before deletion, so a zero observation cannot be followed by a new admission.

## Supported deployment

- Stateless RoleSets with one Pod per group, using Recreate updates.
- Synchronous `/v1/chat/completions` and `/v1/completions` through updated Gateways.
  Protected traffic uses least-request / least-request-top-k admission-aware
  selection. An unset strategy becomes least-request; other strategies, separate
  encoders, asynchronous endpoints and PD are rejected before dispatch.
- All ingress must traverse the protected Gateway path. Old Gateway binaries,
  direct Pod clients, and in-place installation of protection on a serving Pod are
  unsupported. Upgrade the controller and every Gateway before enabling the option.
  Existing unprotected Pods retain their original behavior until recreated.
- The bundled fail-closed ext_proc behavior is required (`failure_mode_allow: false`).
  A custom fail-open configuration can bypass admission and is unsupported.
- The controller and Gateways must use the same standalone Redis primary (the bundle uses Redis 7.4), via
  `REDIS_HOST`, `REDIS_PORT`, `REDIS_PASSWORD` and DB 0, as in the existing Redis
  configuration. The connection must allow `INFO server` and the hash/Lua commands.
  Automatic failover, replica reads and online rollback of live coordinator data
  are not supported.

The mode does not prevent node failure, explicit administrative Pod deletion, or
arbitrary backend faults. It protects controller-driven retirement of initialized
Pods against concurrent admitted traffic under the requirements above.

## Enable it

Add these settings to an existing role, keeping its container specification:

```yaml
name: worker
replicas: 3
stateful: false
podGroupSize: 1
updateStrategy:
  type: Recreate
drain:
  timeoutSeconds: 20
template:
  metadata:
    annotations:
      aibrix.ai/traffic-scale-in-max-wait: "90s"
```

The explicit maximum must exceed a positive `drain.timeoutSeconds`. The same duration
is the cooldown after an unsuccessful attempt. No annotation preserves the ordinary
path for newly created unprotected Pods. Removing it does not strip protection from
existing initialized Pods; those continue using their birth configuration until
recreated. Converting a role with protected Pods to Stateful/PodSet is rejected.
Changing template annotations can trigger a rollout.

`aibrix.ai/traffic-drain-protocol`, `aibrix.ai/traffic-drain-epoch`, and
`aibrix.ai/traffic-scale-in-retry-at` are controller-owned. Do not set them in the
Pod template. The controller marks the protocol at creation, initializes the shared
record, then publishes the epoch with a resource-version precondition. Admission
is refused before publication. A published epoch with missing data is never silently
initialized again.

## Request and retirement lifecycle

1. A Gateway reserves a unique request token before committing its routing target.
   Admission and closing admission use atomic operations on the same Pod-UID hash.
   A closed stale candidate is retried against another candidate; implicit router
   blends cannot bypass the admission-aware path.
2. The controller prefers lower shared active counts within existing template,
   readiness and availability priorities. It closes admission before marking drain.
3. Previously reserved requests continue. Normal completion releases the token at
   response transport EOS (headers/body) or trailers. An SSE `[DONE]` alone keeps
   the processing stream and reservation open until transport completion.
4. A committed non-OK `Process` return releases an aborted request under the required
   fail-closed Envoy contract. Merely observing EOF in a helper does not release it.
   Shutdown waits for processing cleanup before closing the coordinator pool.
   Process crashes and unconfirmed release RPCs can leave pinned reservations.
5. After the minimum wait, the controller checks a fresh engine response for zero
   running/waiting requests as an additional condition. It then atomically checks
   the shared count, epoch and attempt generation and changes the state to sealed.
   A sealed identity can never reopen, even if desired capacity increases or a
   controller restarts. It is deleted with UID/resource-version preconditions and,
   if needed, replaced by a new Pod identity.
6. A non-sealed attempt that reaches its maximum is cancelled in shared state before
   clearing drain metadata and publishing its cooldown. Busy or unknown state never
   authorizes forced deletion. Managed rollouts use the same coordination.

Metadata and all request tokens occupy one Redis hash. Missing/corrupt state is
unknown, not zero. Tokens have no TTL and heartbeats are not completion evidence.
Every connection checks the Redis process run ID, which is also part of the Pod's
epoch. A restarted coordinator cannot make a previous incarnation's restored state
look current.

## Failure and recovery

Redis errors reject protected admissions and retain capacity. A lost coordinator
record or an orphan from a process crash can block automatic retirement; this is
intentional rather than guessing that an unconfirmed request has ended. The system
does not automatically clear live records or replace a published epoch to unstick it.
After coordinator loss/restart, recovery requires controlled quiescence of the full
ingress/proxy/Gateway request path and recreation of protected Pod identities; the
coordinator clients also need a fresh incarnation. Do not restore or erase live
coordinator state while requests may still use it.

The controller emits errors/cancellation events rather than forcing deletion.
Engine metrics must expose one model and one unambiguous total per running/waiting
gauge (vLLM or SGLang; SGLang priority totals are supported). Missing, ambiguous,
negative and non-finite samples retain the Pod. Large-scale overhead and latency
must be validated for a deployment; no throughput improvement is claimed.

Local tests use real production methods, Redis Lua through miniredis, an INFO-server
test fixture, local HTTP metrics and a fake Kubernetes client. They are not a live
Envoy/Kubernetes or Redis failover certification.
