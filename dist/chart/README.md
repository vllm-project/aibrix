# AIBrix Helm Chart

This Helm chart manages the deployment of AIBrix components using Kubernetes-native workflows. It was initially generated using:

```
kubebuilder edit --plugins=helm/v1-alpha
```
and is now manually maintained under `dist/chart`.


## Limitations

1. Third-Party Dependencies Not Included
Dependencies like `Envoy Gateway` and `KubeRay` have their own Helm charts. This AIBrix chart focuses only on core AIBrix components and does not package or manage external dependencies.

2. Not Compatible with Previous Kustomize-Based Installs
This chart is not intended for upgrades from earlier deployments that used Kustomize. Transitioning requires a clean install.


## Development 

### Envoy Service NodePorts

The default `gateway.envoyProxy.service: {}` preserves Envoy Gateway's Service
defaults. To expose enabled listeners on fixed NodePorts, override:

```yaml
gateway:
  envoyProxy:
    service:
      type: NodePort
      externalTrafficPolicy: Cluster
      nodePorts:
        http: 32080
        https: 32443
```

HTTP uses listener port 80; HTTPS uses `gateway.tls.port`. Only enabled
listeners receive a fixed NodePort patch. Omit either `nodePorts` entry to let
Kubernetes allocate that listener's NodePort. Fixed ports must be available and
within the cluster's configured NodePort range, and must differ when both
listeners are enabled.

`nodePorts` requires `service.type: NodePort` and cannot be combined with
`service.patch`. Raw patches remain unchanged; use them instead of `nodePorts`
for advanced Service customization.

### Diagnostic Envoy access logs

Set `gateway.envoyProxy.accessLog.enabled=true` to add request/response timings
and ext_proc call statistics to the default JSON stdout log for Envoy
OpenTelemetry tracing. `gateway.envoyProxy.accessLog.extProcFilterName` optionally
overrides the filter-state key; otherwise it is derived from the namespace and
the chart's EnvoyExtensionPolicy name.

```json
{
  ":authority": "127.0.0.1:8888",
  "bytes_received": 971858,
  "bytes_sent": 6818,
  "connection_termination_details": null,
  "downstream_local_address": "127.0.0.1:10080",
  "downstream_remote_address": "127.0.0.1:34464",
  "duration": 37095,
  "ext_proc_request_body_call_count": "1",
  "ext_proc_request_body_last_call_status": "0",
  "ext_proc_request_body_max_latency_us": "27798",
  "ext_proc_request_body_total_latency_us": "27798",
  "ext_proc_request_header_call_status": "0",
  "ext_proc_request_header_latency_us": "1362",
  "ext_proc_response_body_call_count": "33",
  "ext_proc_response_body_last_call_status": "0",
  "ext_proc_response_body_max_latency_us": "816",
  "ext_proc_response_body_total_latency_us": "16925",
  "ext_proc_response_header_call_status": "0",
  "ext_proc_response_header_latency_us": "628",
  "method": "POST",
  "protocol": "HTTP/1.1",
  "request_duration_ms": 877,
  "request_tx_duration_ms": 907,
  "requested_server_name": null,
  "response_code": 200,
  "response_code_details": "via_upstream",
  "response_duration_ms": 2444,
  "response_flags": "-",
  "response_tx_duration_ms": 34650,
  "route_name": "original_route",
  "start_time": "2026-09-22T07:45:25.637Z",
  "start_time_precise": "2026-09-22T07:45:25.637745000Z",
  "traceparent": "00-xxxxxxxxxxx-d0bed7ad907df883-01",
  "upstream_cluster": "original_destination_cluster",
  "upstream_connection_pool_ready_duration_ms": 0,
  "upstream_host": "10.xx.xx.xx:8000",
  "upstream_local_address": "10.xx.xx.xx:55322",
  "upstream_transport_failure_reason": null,
  "user-agent": "curl/8.7.1",
  "x-envoy-origin-path": "/v1/chat/completions",
  "x-envoy-upstream-service-time": null,
  "x-forwarded-for": "10.xx.xx.xx",
  "x-request-id": "xxxxxxxxx"
}
```

### Circuit breaker resource gauges

Set `gateway.envoyPatchPolicy.circuitBreakers.trackRemaining=true` to expose
gauges for resources remaining before the original destination cluster's
circuit breakers open. Set it to `false` to disable these gauges. This boolean
option does not change circuit breaker limits; if the key is absent, the
template falls back to `false`.

### Helm Lint

Run the following to validate the chart, If you encounter errors such as:

```
helm lint dist/chart
==> Linting dist/chart
[ERROR] templates/: template: aibrix/templates/gateway-instance/gateway.yaml:76:39: executing "aibrix/templates/gateway-instance/gateway.yaml" at <.Values.gateway.envoyProxy.container.shutdown.image>: nil pointer evaluating interface {}.image

Error: 1 chart(s) linted, 1 chart(s) failed
```

resolve the issue and retry until you get:
```
 helm lint dist/chart
==> Linting dist/chart

1 chart(s) linted, 0 chart(s) failed
```

### helm unittest

Test cases live in `dist/chart/tests`. The common-label suite uses `documentSelector.matchMany` and `skipEmptyTemplates`, which require helm-unittest 1.0 or newer. helm-unittest 0.7.x fails that suite with `label expected to exist`.

```
helm plugin install https://github.com/helm-unittest/helm-unittest.git --version v1.1.2

helm unittest dist/chart
### Chart [ aibrix ] dist/chart

 PASS  Test Redis Configuration Logic   dist/chart/tests/redis_config_test.yaml
 PASS  Test Redis Dependency Passwd Logic       dist/chart/tests/redis_config_test.yaml
 PASS  Test Redis Shared Passwd Conflict        dist/chart/tests/redis_config_test.yaml
 PASS  Test Redis Shared Passwd dist/chart/tests/redis_config_test.yaml

Charts:      1 passed, 1 total
Test Suites: 4 passed, 4 total
Tests:       4 passed, 4 total
Snapshot:    0 passed, 0 total
Time:        14.85875ms
```

### Render yaml files

Render all manifests using:
```
helm template aibrix dist/chart -f dist/chart/values.yaml --namespace aibrix-system --debug > verify.yaml
install.go:222: [debug] Original chart version: ""
install.go:239: [debug] CHART PATH: /Users/username/workspace/aibrix/dist/chart
```

You can also render a specific component for targeted debugging:

```
helm template aibrix dist/chart --show-only templates/gateway-plugin/deployment.yaml
```

### Runtime Debugging

Helm lint only catches syntax and static issues. If components are not appearing as expected (e.g., pods not running), check the live Kubernetes objects:

Sample error:
```
  ----     ------        ----                  ----                   -------
  Warning  FailedCreate  10s (x17 over 5m39s)  replicaset-controller  Error creating: pods "aibrix-gateway-plugins-58cdbc746f-" is forbidden: error looking up service account aibrix-system/aibrix-gateway-plugin: serviceaccount "aibrix-gateway-plugin" not found
```
Ensure all required resources (e.g., ServiceAccounts) are declared in the chart or created manually beforehand.


## Installation

### Dependencies

Apply required dependencies:
```
kubectl apply -k config/dependency --server-side
```

Install KubeRay operator (used by some AIBrix workloads):
```
helm install kuberay-operator kuberay/kuberay-operator \
  --namespace kuberay-system \
  --version 1.2.1 \
  --include-crds \
  --set env[0].name=ENABLE_PROBES_INJECTION \
  --set-string env[0].value=false \
  --set fullnameOverride=kuberay-operator \
  --set featureGates[0].name=RayClusterStatusConditions \
  --set featureGates[0].enabled=true
```

### CRDs

`--install-crds` is not available in local chart installation. We need to manually install it.

```
kubectl apply -f dist/chart/crds/ --server-side
```

### Helm Install

Install AIBrix with the default values:
```
helm install aibrix dist/chart -n aibrix-system --create-namespace
```

Or use a custom values file:

```
helm install aibrix dist/chart -f my-values.yaml -n aibrix-system --create-namespace
```


### Verification

```
helm list -A
kubectl get all -n aibrix-system
```


### Helm upgrade

Upgrade to the latest chart version:
```
helm upgrade aibrix dist/chart -n aibrix-system
```

### Gateway Redis Sentinel

The gateway plugin can connect to an independently deployed Redis Sentinel
cluster. The chart does not deploy Sentinel. Keep `metadata.redis.enabled=false`
and configure the gateway connection:

```yaml
metadata:
  redis:
    enabled: false
gatewayPlugin:
  dependencies:
    redis:
      password: ""
      sentinel:
        enabled: true
        masterName: "mymaster"
        addresses:
          - "redis.redis-sentinel.svc.cluster.local:26379"
        username: ""
        password: ""
```

Use the actual monitored master name. Prefer multiple stable Sentinel Pod DNS
addresses when available. Sentinel must advertise data-node and Sentinel
addresses reachable from gateway Pods. Network policies must allow both the
Sentinel port and the data-node port. In Sentinel mode, the gateway ignores the
direct `host` and `port`; disabling Sentinel restores the existing direct mode.
Other components retain their existing Redis configuration.

When `sentinel.password` is empty, the chart reuses the gateway data-node
password for Sentinel authentication, referencing the same Secret key in both
containers. This includes the existing `metadata.redis.password` fallback, so
one shared password configures both connections. An explicit `sentinel.password`
overrides this fallback and uses a separate Secret key. If no password is
configured, both connections are unauthenticated. Supply real passwords through
a protected values file, not a committed file. `sentinel.username` optionally
enables Sentinel ACL authentication. This password fallback is implemented by
the chart; outside Helm, configure both password environment variables.

Sentinel mode requires a gateway image built with Sentinel support and the
`--check-redis` flag. Build and publish that image, then update
`gatewayPlugin.container.image.tag` before upgrading. The initContainer uses
the same image and Redis settings as the gateway to discover and PING the
master, without Kubernetes discovery or a shell. A failed check exits nonzero;
Kubernetes retries the initContainer. Direct mode keeps the BusyBox check.

The Go client receives master-switch notifications and rediscovers the master
on reconnection. Requests depending on Redis can still fail during the failover
window; Sentinel does not guarantee zero data loss with asynchronous replication.
This change does not migrate user records or configure Redis persistence.
Back up and restore required records into the new cluster before switching.

Environment variables for running the gateway outside this chart:

| Variable | Purpose |
| --- | --- |
| `REDIS_SENTINEL_ENABLED` | Enables Sentinel; defaults to `false` |
| `REDIS_SENTINEL_MASTER_NAME` | Required monitored master name |
| `REDIS_SENTINEL_ADDRESSES` | Required comma-separated `host:port` seed list |
| `REDIS_SENTINEL_USERNAME` | Optional Sentinel ACL user |
| `REDIS_SENTINEL_PASSWORD` | Optional Sentinel password |
| `REDIS_PASSWORD` | Data-node password, shared with direct mode |

Save the configuration above in `redis-sentinel-values.yaml`, supply the
required connection settings, and verify the rendered gateway Deployment
before deploying:

```sh
helm lint dist/chart -f redis-sentinel-values.yaml
helm template aibrix dist/chart -f redis-sentinel-values.yaml \
  --show-only templates/gateway-plugin/deployment.yaml
```

### Helm Uninstall

Remove the release:
```
helm uninstall aibrix -n aibrix-system
```

> **Note:** `helm uninstall` does **not** delete the AIBrix CRDs in `dist/chart/crds/`.
> This is intentional — deleting CRDs cascade-deletes all user CRs (StormService,
> RoleSet, PodAutoscaler, ModelAdapter, etc.). See
> [#2062](https://github.com/vllm-project/aibrix/issues/2062).
>
> Only run the following if you really want to wipe AIBrix CRDs and every CR
> instance depending on them:
> ```
> kubectl delete -f dist/chart/crds/
> ```
