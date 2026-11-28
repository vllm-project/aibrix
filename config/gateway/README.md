# Customizing the gateway manifests

This customization covers gateway references only. It does not establish
support for relocating or renaming the entire AIBrix installation. An overlay
of `config/default` can render consistent gateway references while other
components still require separate configuration or fixes; see
[deployment scope](#deployment-scope) below.

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
- ../config/rbac/gateway
```

Render it with:

```sh
kubectl kustomize custom-gateway
```

This renders Gateway `custom-gateway/custom-eg` and matching Envoy references.
Including the gateway RBAC resources also creates ServiceAccount
`custom-gateway/custom-gateway-plugins` and updates the gateway plugin Deployment
and ClusterRoleBinding to reference it.
An outer overlay can also change these names; no additional transformer entry
is required in the overlay.

## Deployment scope

The example renders gateway and gateway RBAC resources. It does not install
Envoy Gateway, create the namespace, or supply Redis or the metadata service.
Include the supporting resources or configure their references for the intended
deployment. Kustomize can update Service and ServiceAccount references when
those resources are part of the render. If `config/rbac/gateway` is omitted,
the gateway plugin still references ServiceAccount `gateway-plugins`, which
must already exist in the target namespace. The gateway's existing Redis
settings remain explicit deployment inputs.

An outer overlay of `config/default` has additional limitations:

- Kustomize renames the webhook Secret, the Deployment's Secret volume, and the
  webhook configurations and their `clientConfig.service` references together.
  However, the [certificate manager](../../pkg/cert/cert.go) still uses
  fixed names: `aibrix-webhook-service`, `aibrix-webhook-server-cert`,
  `aibrix-validating-webhook-configuration`, and
  `aibrix-mutating-webhook-configuration`. Renaming the manifests does not update
  the service DNS name or resource lookups used at runtime.
- The [controller](../../cmd/controllers/main.go) defaults
  `--leader-election-namespace` to `aibrix-system`. The Kustomize Deployment
  does not override this flag, so changing the Pod namespace alone does not
  move leader election. An explicitly empty flag value would use the in-cluster
  namespace. The controller also passes this namespace to the certificate
  manager. Helm sets the flag explicitly to the release namespace.
- The bundled KubeRay role bindings retain fixed `aibrix-system` subjects and
  ServiceAccount names. An outer prefix or namespace change can therefore leave
  those bindings pointing at a different ServiceAccount from the rendered one.

These are separate installation concerns. Gateway reference customization does
not restrict workload discovery, change RBAC permissions, or isolate
installations.

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
The [Helm chart](../../dist/chart/README.md#gateway-and-model-httproute-namespace)
supplies the controller's Gateway name and both components' route namespace
from the chart's rendered resources as well.

Envoy can briefly retain an older route after the Gateway API reports the model
HTTPRoute as accepted with resolved backends. During this interval, a local
backend-readiness response from a managed forwarding route carries HTTP 503,
`x-error-routing: true`, and the OpenAI `service_unavailable` error code.
An error returned by the model server keeps its original status and body.

## Transformation and validation

`kustomizeconfig.yaml` declares the name references in Gateway API and Envoy
Gateway resources. Embedded Envoy strings use Kustomize `vars`, resolved after
all overlay transformations. Kustomize deprecates `vars` and emits a warning.
They are retained here because base-level `replacements` run before a parent
overlay changes the namespace or prefix, and would require changing existing
overlays to apply the replacements at the outermost level.
The [migration follow-up request](https://github.com/vllm-project/aibrix/pull/3612#discussion_r4698247670)
calls for evaluating alternatives and documenting any changes required in parent
overlays before replacing `vars`.

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
