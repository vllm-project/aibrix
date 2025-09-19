# AIBrix Helm Chart

This Helm chart manages the deployment of AIBrix components using Kubernetes-native workflows. It was initially generated using:

```
kubebuilder edit --plugins=helm/v1-alpha
```
and is now manually maintained under `dist/chart`.


## Limitations

1. Missing Standard Labels
Common Kubernetes labels such as `app.kubernetes.io/managed-by` and `app.kubernetes.io/name` are currently not included. These should be added to improve consistency and observability.

2. Third-Party Dependencies Not Included
Dependencies like `Envoy Gateway` and `KubeRay` have their own Helm charts. This AIBrix chart focuses only on core AIBrix components and does not package or manage external dependencies.

3. Not Compatible with Previous Kustomize-Based Installs
This chart is not intended for upgrades from earlier deployments that used Kustomize. Transitioning requires a clean install.


## Development 

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
