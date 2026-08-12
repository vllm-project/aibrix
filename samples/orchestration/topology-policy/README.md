# StormService Topology Policy Examples

These examples show how `spec.template.spec.topologyPolicy` places StormService
Pods using Kubernetes topology labels.

Before running the examples, make sure worker nodes have the topology label used
by the selected example:

```bash
kubectl label node <node-a> topology.kubernetes.io/zone=zone-a --overwrite
kubectl label node <node-b> topology.kubernetes.io/zone=zone-b --overwrite
```

For hostname examples, Kubernetes nodes already have `kubernetes.io/hostname`.

Apply one example at a time:

```bash
kubectl apply -f samples/orchestration/topology-policy/role-zone-preferred.yaml
kubectl get pods -l storm-service-name=tp-role-zone-pref -o wide
```

`mode: Preferred` adds a strong pod affinity preference. It allows scheduling to
fall back to another topology domain when the preferred domain cannot fit the
Pods.

`mode: Required` adds hard pod affinity. Pods can remain Pending when no node in
the selected topology domain can satisfy the placement.

## Scope Diagram

`scope` decides which Pods are matched by the injected pod affinity. `key`
decides which node label is used as the topology domain.

```text
scope: StormService

  topology domain
  +--------------------------------------------------+
  | RoleSet A: prefill Pods + decode Pods            |
  | RoleSet B: prefill Pods + decode Pods            |
  +--------------------------------------------------+


scope: RoleSet

  topology domain 1              topology domain 2
  +-------------------------+    +-------------------------+
  | RoleSet A               |    | RoleSet B               |
  | prefill Pods            |    | prefill Pods            |
  | decode Pods             |    | decode Pods             |
  +-------------------------+    +-------------------------+


scope: Role

  topology domain 1              topology domain 2
  +-------------------------+    +-------------------------+
  | prefill Pods            |    | decode Pods             |
  | across all RoleSets     |    | across all RoleSets     |
  +-------------------------+    +-------------------------+
```

For `key: topology.kubernetes.io/zone`, a domain is a zone. For
`key: kubernetes.io/hostname`, a domain is one node.

## Examples

- `role-zone-preferred.yaml`: colocates Pods by role across all RoleSets in the
  same zone when possible. This is useful for separating prefill and decode pools
  by zone.
- `role-zone-required.yaml`: requires Pods with the same role to stay in the same
  zone.
- `roleset-zone-preferred.yaml`: colocates all Pods inside each RoleSet replica
  in the same zone when possible, while different RoleSets may use different
  zones.
- `roleset-hostname-required.yaml`: requires all Pods inside each RoleSet replica
  to run on the same node hostname. The short StormService name keeps generated
  Stateful Pod hostnames under Kubernetes' 63-character DNS label limit.