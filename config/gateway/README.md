# Customizing the gateway manifests

The gateway manifests support Kustomize `namespace`, `namePrefix`, and
`nameSuffix`. Gateway API references, Envoy policy targets, listener and route
configuration names, and the ext_proc filter references follow the rendered
resources. The default installation keeps the `aibrix-system` namespace and
`aibrix-` prefix.

For example, create `custom-gateway/kustomization.yaml` at the repository root:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: custom-gateway
namePrefix: custom-
resources:
- ../config/gateway
```

Render it with:

```sh
kubectl kustomize custom-gateway
```

This renders Gateway `custom-gateway/custom-eg` and matching Envoy references.
An outer overlay can also change these names; no additional transformer entry
is required in the overlay.

## Deployment scope

This is gateway configuration support, not support for relocating or renaming
the entire AIBrix installation. The example renders the gateway resources; it
does not install Envoy Gateway, create the namespace, or supply gateway RBAC,
Redis, or the metadata service. Include the supporting resources or configure
their references for the intended deployment. Kustomize can update references
to Services and ServiceAccounts when those resources are part of the render.
The gateway's existing Redis settings remain explicit deployment inputs.

The controller's certificate manager still uses fixed webhook resource names,
and its leader-election namespace is configured separately. Changing only
`namespace` or `namePrefix` on `config/default` does not establish a working
customized installation of every component. It also does not restrict workload
discovery, change RBAC permissions, or isolate installations.

## ModelRouter configuration

ModelRouter captures `AIBRIX_GATEWAY_NAME` at startup. An absent or empty value
uses `aibrix-eg`. A nonempty value names the Gateway that generated model
HTTPRoutes attach to, including when routes are resynchronized or recreated.
Set it to the Gateway's rendered name when running ModelRouter separately.
Invalid Kubernetes resource names fail controller setup; values are not trimmed
or silently replaced with the default.

`AIBRIX_GATEWAY_NAMESPACE` selects the shared Gateway/HTTPRoute namespace for
ModelRouter and the gateway plugin; its absent or empty default remains
`aibrix-system`. `POD_NAMESPACE` describes Pod placement and does not select
the route namespace. `config/default` supplies both ModelRouter settings from
the rendered Gateway, and `config/gateway` supplies the plugin's route namespace.

## Transformation and validation

`kustomizeconfig.yaml` declares the name references in Gateway API and Envoy
Gateway resources. Embedded Envoy strings use Kustomize `vars`, resolved after
all overlay transformations. Kustomize deprecates `vars` and emits a warning.
They are retained here because base-level `replacements` run before a parent
overlay changes the namespace or prefix, and would require changing existing
overlays to apply the replacements at the outermost level.

Kustomize does not substitute mapping keys. Each pinning route therefore starts
with an empty `typed_per_filter_config`; the following JSON patch adds its
filter configuration through a JSON Pointer, where the namespace and policy
name can be substituted. Keep each route insertion and filter patch together:
the filter patch targets the route just inserted at index zero. The `~1`
sequences escape slashes in the filter name.

Run the render and controller regressions from the repository root:

```sh
go test ./test/e2e/installation -run '^TestKustomizeGatewayReferences$' -count=1
go test ./pkg/controller/modelrouter -run '^TestModelRouter(GatewayNamespaceLifecycle|RejectsInvalidGatewayName)$' -count=1
```

The render test skips explicitly when `kubectl` is unavailable in `PATH`.
When available, it applies the rendered route JSON patches, including Envoy's
protobuf conversion between operations.
It checks filter references against rendered extension policies and compares
the default pinning filters with the existing Envoy Gateway xDS golden. These
checks do not replace a live Kubernetes/Envoy deployment test.
