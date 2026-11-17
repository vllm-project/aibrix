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

### Gateway plugin graceful shutdown

The Helm hook is disabled by default and requires `gateway.envoyAsSideCar=false`.
Rebuild the gateway image with `build/container/Dockerfile.gateway` to include
the standalone `/prestop` binary, then configure:

```yaml
gatewayPlugin:
  container:
    image:
      repository: your-registry/gateway-plugins
      tag: your-rebuilt-tag
  gracefulShutdown:
    enabled: true
    terminationGracePeriodSeconds: 340
    propagationDelaySeconds: 10
    timeoutSeconds: 300
    grpcDrainTimeoutSeconds: 30
    pollIntervalSeconds: 2
    metricsURL: http://127.0.0.1:8080/metrics
```

The standalone Go preStop command waits for endpoint propagation, then polls this Pod's local
`gateway_in_flight_requests` metric until three consecutive zero samples or the
hook timeout. The plugin initializes the metric to zero before accepting RPCs,
so an idle Pod can drain without waiting for the deadline. Missing or invalid
metrics and fetch failures reset the zero count.
SIGTERM then starts a bounded gRPC drain; remaining RPCs are stopped at its
deadline. View hook progress with `kubectl logs -c gateway-plugin`.

In rebuilt gateway images, SIGTERM always marks gRPC health NOT_SERVING and
drains gRPC before stopping gateway services, state synchronization, and cache
workers. Without an explicit flag, `--graceful-shutdown-timeout` defaults to `20s`
and must be positive. This leaves 10 seconds of the default Kubernetes 30-second
termination grace period for cleanup. Enabling the Helm hook supplies the
independent `grpcDrainTimeoutSeconds` value (30 seconds by default).

When customizing the gateway-plugin container's `args`, keep a single list. With
`gatewayPlugin.gracefulShutdown.enabled=true`, append additional flags to the
generated list and preserve `--graceful-shutdown-timeout`, whose value comes from
`grpcDrainTimeoutSeconds`. Replacing the list without that flag falls back to the
binary's 20-second default. When editing the template's commented QPS/burst
example, add the flags to the existing list instead of adding another `args:` key.

The metrics URL must point to this Pod directly. The hook matches the metric
name without filtering labels and requires exactly one valid sample per scrape.
Fetches bypass proxy environment variables and do not follow redirects. Each
fetch is limited to 5 seconds and the remaining hook budget, with an 8 MiB
response limit. Fetch errors and oversized responses reset the zero count.

All Helm durations are integer seconds. The hook timeout includes propagation and
fetches. Required constraints:

- `timeoutSeconds > propagationDelaySeconds`
- `terminationGracePeriodSeconds >= timeoutSeconds + grpcDrainTimeoutSeconds + 10`

Update existing grace-period overrides to satisfy these constraints. Tune budgets
for observed request durations, cleanup time, and endpoint update latency; the
propagation delay does not confirm Envoy updates. Keep another ready plugin
replica with spare capacity.
The 10-second cleanup reserve is only a minimum validation allowance. With
`AIBRIX_STATESYNC_ENABLED=true`, `RedisSync.Stop()` alone can wait up to 90 seconds;
HTTP shutdown and the OpenTelemetry flush can each take another 5 seconds.
Increase the Pod grace period to cover these waits when those services are enabled.
Enabling the hook sets `maxUnavailable: 0`, `maxSurge: 1`, and `minReadySeconds: 10`;
reserve capacity for a surge Pod. With the hook disabled, the chart retains
`maxUnavailable: 1`, `maxSurge: 1`, and no readiness stabilization delay.
This hook does not coordinate Envoy or model backend termination.

The Helm value names and budget rules are unchanged. Upgrade the chart and gateway
image together. Custom hooks that invoke `/prestop.sh` must switch to `/prestop`;
the gateway image no longer includes that script or `/busybox`. The existing
`/gateway-plugins prestop` subcommand remains available for custom hooks, but the
chart uses the smaller binary to avoid gateway package initialization and its logs.

Run the core drain and command tests without Docker:

```sh
go test ./pkg/plugins/gateway/drain ./cmd/prestop ./cmd/plugins
go test ./pkg/plugins/gateway -run TestProcessInFlightMetricLabelContract
```

Run image smoke tests for the command entrypoint, proxy bypass, and hook logging:

```sh
docker build -f build/container/Dockerfile.gateway -t aibrix/gateway-plugins:prestop-test .
python3 build/container/entrypoints/test_prestop.py
```

These image smoke tests are run manually. Override `AIBRIX_PRESTOP_TEST_IMAGE`
to test another rebuilt gateway image.

### Envoy proxy graceful shutdown

Disabled by default; applies only to Envoy Gateway-managed deployments with
`gateway.enable=true` and `gateway.envoyAsSideCar=false`. Requires Envoy Gateway
**v1.2.0 or later** with the matching `EnvoyProxy` CRDs. Upgrade the controller and
CRDs together: older CRDs may prune `spec.shutdown` while leaving the longer Pod
grace period in place, without enabling draining. Enable separately:

```yaml
gateway:
  envoyProxy:
    gracefulShutdown:
      enabled: true
      terminationGracePeriodSeconds: 330
      drainTimeoutSeconds: 300
      minDrainDurationSeconds: 10
```

Uses native `EnvoyProxy.spec.shutdown` and Envoy Gateway's lifecycle hooks; no
custom image is needed. The shutdown-manager fails readiness and drains existing
connections, including SSE streams. It waits at least `minDrainDurationSeconds`,
then exits when connections drain or the timeout expires. Idle keepalive
connections can prolong draining; requests still running at timeout may be cut off.

All durations are integer seconds. Required constraints:

- `terminationGracePeriodSeconds > drainTimeoutSeconds + 10` (includes the
  shutdown-manager readiness wait in the supported v1.2 baseline; inspect the
  generated hooks and arguments when upgrading Envoy Gateway)
- `drainTimeoutSeconds > minDrainDurationSeconds`

Enabling sets `maxUnavailable: 0` and `maxSurge: 1`; reserve capacity for an extra
Pod. New Pods must stay Ready for 10 seconds before becoming available. Keep
plugins and model backends available during draining; route and stream timeouts
still apply.

Before relying on either feature, test Envoy and plugin rolling updates separately
with long SSE requests. Verify that existing streams finish, new requests succeed,
and over-budget streams terminate within the configured budgets. Inspect Envoy's
generated Deployment and `kubectl logs -c shutdown-manager` for drain behavior.

#### Rollout verification procedure

Use a test cluster with two plugin replicas, two Envoy replicas, and capacity for
the surge Pods. Record Kubernetes and Envoy Gateway versions, image digests,
rendered shutdown settings, and the generated Envoy lifecycle hooks and
shutdown-manager arguments. Keep the backend and the other Deployment running
while testing each Deployment separately.

Use an SSE backend with a controlled response duration. Wait for the first event
and identify the serving plugin Pod from its request logs before starting
`kubectl rollout restart deployment/<plugin-deployment> -n <namespace>`.
Repeat with `<envoy-deployment>`, and send new requests throughout each rollout.
Ensure request and route timeouts exceed the tested shutdown budgets.

| Case | Stream remaining when termination starts | Required observation |
| --- | --- | --- |
| Idle Pod | No requests have reached the plugin | After propagation, three zero samples end the hook; it does not use the full 300-second budget. |
| Within budget | 120 seconds with the default 300-second hook / Envoy drain budget | The client receives the final SSE completion marker; new requests succeed through replacement replicas. |
| Over budget | At least 400 seconds with the default budgets | The stream is interrupted when draining is exhausted; record the client exit status, last event and elapsed time, and confirm Pod termination remains bounded. |

For each case, save the client output, rollout status, Pod termination timestamps,
plugin hook logs and Envoy shutdown-manager logs. Report observed durations and
outcomes separately for plugin and Envoy rollouts. Unit tests and image smoke
tests do not establish these cluster-level results.

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
