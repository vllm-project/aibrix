# Console Gateway API

The Console manages `gateway.networking.k8s.io/v1` Gateways through
`/api/v1/gateway-instances`. This initial API supports listing, reading,
creating and updating resources. It does not add a second Gateway controller
or a Console database representation of a Gateway.

## Configuration

Install the Gateway API CRDs and a compatible Gateway controller first. Use
an existing GatewayClass supported by that controller.

The provider uses the Console's existing `KUBERNETES_KUBECONFIG`,
`KUBERNETES_CONTEXT` and `KUBERNETES_NAMESPACE` configuration, or in-cluster
credentials when no kubeconfig/context is supplied. The namespace defaults
to `default`. Each Console backend operates against one cluster and one
namespace. Requests cannot override that scope.

Give the Console's Kubernetes identity this namespace-scoped permission,
in addition to permissions required by other Console features:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: console-gateways
  namespace: default
rules:
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["gateways"]
    verbs: ["get", "list", "create", "update"]
```

Bind the Role to the Console's service account using a RoleBinding. No
permission to write Gateway status, Deployments or Secrets is needed for
this feature. All authenticated Console users can read Gateways in the
configured namespace; the Console `admin` role is required to write them.
This is a shared cluster administration view, not per-user resource isolation.

## Operations

| Method | Path | Result |
| --- | --- | --- |
| GET | `/api/v1/gateway-instances` | Native GatewayList, sorted by name |
| GET | `/api/v1/gateway-instances/{name}` | Native Gateway |
| POST | `/api/v1/gateway-instances` | Created Gateway, HTTP 201 |
| PUT | `/api/v1/gateway-instances/{name}` | Updated Gateway, HTTP 200 |

Create accepts native Gateway JSON, for example:

```json
{
  "apiVersion": "gateway.networking.k8s.io/v1",
  "kind": "Gateway",
  "metadata": {"name": "inference"},
  "spec": {
    "gatewayClassName": "eg",
    "listeners": [
      {"name": "http", "protocol": "HTTP", "port": 80}
    ]
  }
}
```

For an update, GET the resource, edit its spec, labels or annotations, and
PUT it to the resource URL with the returned `metadata.resourceVersion`.
The name must match the URL. Spec, labels and
annotations are replaced by the submitted values, so retain entries that
should remain present. Server metadata, finalizers, owner references and
status are preserved from the current object. Status in a request is never
written. Creation similarly only writes name, configured namespace, labels,
annotations and spec. The installed CRD and admission rules validate spec
changes. Writes use Kubernetes strict field validation so fields unsupported
by the installed CRD are rejected instead of silently discarded.

Both the provider and Kubernetes enforce optimistic concurrency. A stale
version or a concurrent API-server write returns HTTP 409. GET again and
reconcile the edit before retrying. Missing resources return 404; invalid
requests return 400 or Kubernetes' 422 validation response. Authentication
and authorization failures return 401 and 403.

Successful writes acknowledge persistence, not traffic readiness. Read
`status.conditions` and `status.listeners` from the Gateway controller.
A condition describes the desired configuration only when its
`observedGeneration` matches `metadata.generation`. Listener conditions
must also be checked; an old `Programmed=True` is not sufficient.

## Provider boundary

`api/gateway/contract.Provider` is the four-operation interface consumed by
the HTTP handler. It uses the existing Gateway API Go types. The Kubernetes
implementation calls the typed client directly, and leaves admission,
defaulting, route attachment and reconciliation to Kubernetes.

There is no provider registry, cluster inventory, background Console
reconciler or vendor-specific resource model in this implementation.
Gateway API v1.0 has no portable replica, CPU or memory fields. Capacity
management belongs to a controller-specific extension and is outside this
API's standard contract. Delete, model discovery and route-policy editing
are also outside this initial change.

An alternate implementation must preserve namespace scope, reject
unsupported spec fields and report conflicts using Kubernetes API errors.
Provider-specific extensions belong in that implementation. They must not
be advertised as standard Gateway fields.
