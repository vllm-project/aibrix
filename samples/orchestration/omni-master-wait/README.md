# Waiting for the vLLM-Omni master address

This sample is a Kubernetes wiring example for a multi-stage vLLM-Omni
deployment started as a StormService: the stages after the head cannot start
before the address of the head's master endpoint resolves, so the head has to be
reachable while it is still `Ready=False`.

The scope is the Kubernetes objects and the startup ordering. The AIBrix
controller, real `OmniMasterServer` registration, model startup and inter-stage
payload transfer are out of scope; see [Out of scope](#out-of-scope).

## The problem

A regular `ClusterIP` Service only lists **Ready** Pods in its Endpoints. A head
that stays `Ready=False` — because its readiness depends on the stages behind
it, or because it starts serving the master port late — therefore never appears
in its own Service, and any stage that waits for that address before starting
deadlocks.

`publishNotReadyAddresses: true` publishes the Pod IP of a not-ready Pod as
well, which is what breaks the deadlock.

The StormService controller already creates a headless Service with
`publishNotReadyAddresses: true`, but that Service selects every role in the
pipeline, so its name resolves to all role Pods and cannot serve as the head's
master address. What the controller does not create is a role-scoped Service
with the same flag, which is what this sample isolates.

## What this sample applies

| File | Contents |
| --- | --- |
| `mock-head.yaml` | A mock head Pod that is deliberately `Ready=False`, the `omni-master-wait` Service with `publishNotReadyAddresses: true`, and `omni-master-wait-normal`, the same Service without the flag (negative control) |
| `mock-worker.yaml` | A worker whose init container waits for `omni-master-wait:26000` before its main container starts, plus a negative-control worker that waits for `omni-master-wait-normal` |
| `head-service.yaml` | The head Service a real deployment needs: it selects `storm-service-name` plus `role-name`, and carries `publishNotReadyAddresses: true`. Companion only, see [The head Service in a real deployment](#the-head-service-in-a-real-deployment) |

The mock head is a plain TCP listener (`busybox` + `nc`), not `OmniMasterServer`:
it makes the address reachable before the Pod is Ready and it does nothing else.
The worker init container only checks TCP reachability, not the registration
protocol.

The head and the workers are plain Pods rather than Deployments to keep the
sample minimal; `publishNotReadyAddresses` behaves the same for any Pod the
Service selects. The head carries a readiness probe that fails on purpose, which
is what keeps it out of the Ready set.

## Run it

```bash
kubectl apply -f samples/orchestration/omni-master-wait/mock-head.yaml
kubectl apply -f samples/orchestration/omni-master-wait/mock-worker.yaml

kubectl get pods
kubectl get endpoints omni-master-wait omni-master-wait-normal
kubectl logs omni-master-wait-worker -c wait-for-master
kubectl logs omni-master-wait-worker-normal -c wait-for-master
```

Expected after a few seconds:

```text
NAME                            READY   STATUS     RESTARTS
omni-master-wait-head           0/1     Running    0
omni-master-wait-worker         1/1     Running    0
omni-master-wait-worker-normal  0/1     Init:0/1   0
```

- `omni-master-wait` has one endpoint (`<head pod IP>:26000`) even though the
  head is not Ready; `kubectl get endpointslice` shows the same address.
- `omni-master-wait-normal` has no endpoints.
- The `omni-master-wait-worker` init log ends with `connected`, the main
  container starts, and the Pod keeps 0 restarts.
- `omni-master-wait-worker-normal` keeps logging `waiting` and stays in
  `Init:0/1`.

The first `nc` attempts can print `nc: bad address` before the Service DNS name
resolves in that Pod; the retry loop covers it.

The run above only applies `mock-head.yaml` and `mock-worker.yaml`.
`head-service.yaml` is not part of it: it selects a StormService that does not
exist in the mock cluster, so it matches no Pods there and its Endpoints stay
empty.

Clean up with the same files in reverse order:

```bash
kubectl delete -f samples/orchestration/omni-master-wait/mock-worker.yaml
kubectl delete -f samples/orchestration/omni-master-wait/mock-head.yaml
```

## The head Service in a real deployment

The StormService controller already creates one headless Service per
StormService with `publishNotReadyAddresses: true` (`syncHeadlessService` in
`pkg/controller/stormservice/sync.go`). Its selector is only
`storm-service-name`, so it spans every role: the name resolves to all role Pods
and cannot serve as the head's master address.

Every role Pod also carries `role-name`, and its hostname is the Pod name, which
embeds the role template hash or a generated suffix. A worker cannot hardcode a
Pod FQDN across a rollout, which is the second reason for a role-scoped Service.

`head-service.yaml` is that Service in miniature: name `qwen2-5-omni-head`,
selector `storm-service-name: qwen2-5-omni` plus `role-name: thinker`, port 26000
and the same `publishNotReadyAddresses` flag. In a real deployment the wait in
`mock-worker.yaml` stays as it is; only its target changes to
`qwen2-5-omni-head:26000`. 26000 is the value the upstream `docs/cli/serve.md`
examples use; `--omni-master-port` has no default, so the Service port has to
match whatever the stage processes are started with.

The roles are independent objects: the role list carries no startup dependency,
and `upgradeOrder` orders upgrades, not stages. The stage placement comes from
the vLLM-Omni deploy config. The pipeline manifest is not duplicated here: the
#3305 thread carries it (v0.30.0, `stage-id` annotations, `--headless` and
`--omni-master-*` on the stage processes), and a GPU manifest that cannot run
today would be an apply hazard in this directory.

One caveat: this Service assumes one head replica. With several, the Service
name load-balances across all of them, and a worker has to reach the specific
master replica it belongs to; that mapping is out of scope here.

## Out of scope

- The AIBrix controller: this sample does not install it, so RoleSet and Pod
  creation from a StormService are not exercised.
- Real `OmniMasterServer` registration and the protocol the stages use to talk
  to the head. The mock head only proves the address is reachable.
- Model startup, tokenizer and request handling.
- Inter-stage payload transfer. On the upstream side this path is still being
  stabilised in vllm-omni ([#9357], [#9358], [#9359]), so this sample does not
  claim a working end-to-end multi-stage deployment.
- Shared memory between Pods. The upstream deploy config sets no connector, so
  vLLM-Omni uses its default shared-memory connector, which relies on a common
  `/dev/shm`; separate Pods do not have one. `topologyPolicy` only places Pods
  together and does not share IPC, so a multi-Pod split has to set
  `hostIPC: true`. The CRD passes `hostIPC` through the Pod template, but there
  is no sample or validation for it yet, and this sample does not exercise it.

[#9357]: https://github.com/vllm-project/vllm-omni/issues/9357
[#9358]: https://github.com/vllm-project/vllm-omni/issues/9358
[#9359]: https://github.com/vllm-project/vllm-omni/issues/9359
