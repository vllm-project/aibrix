# ModelWarmup Custom Actions Design

## Summary

Extend the existing image-only `ModelWarmup` API with an optional, user-provided
Pod fragment. The extension supports model downloads, node prechecks, cache
preparation, and similar finite actions without adding separate workflow stages
or changing the existing per-node Job and aggregate status model.

The implementation must preserve existing image-only objects and Go callers.
It does not add generic hooks, `ArtifactReady` or `PrecheckReady` state machines,
workload rollout gating, autoscaler integration, or per-container status.

## Goals

- Support image-only, custom-only, and combined warmups.
- Run custom init containers sequentially before regular work.
- Run generated image-preload containers and custom regular containers together
  in the same node-pinned Job.
- Allow custom volumes and image pull secrets while keeping scheduling, retry,
  timeout, lifecycle, and status ownership in the controller.
- Include all custom action inputs in the immutable workload revision.
- Provide runnable public samples for image preloading, model download, generic
  node prechecks, NVIDIA GPU prechecks, and a combined workflow.

## Non-goals

- Separate artifact, precheck, or hook lifecycle APIs and conditions.
- Parsing container output or reporting per-container status.
- Long-running services or inference-engine startup.
- Mutating or gating StormService, RoleSet, ModelClaim, ModelAdapter, Deployment,
  or PodAutoscaler resources.
- Replacing cluster Pod Security, admission policy, or RBAC controls.

## API

`ModelWarmupSpec.ImagePreload` remains a value field for Go source compatibility,
but becomes optional in the CRD and JSON contract. A new optional pointer field
contains native Kubernetes container and volume types:

```go
type ModelWarmupSpec struct {
    Mode         ModelWarmupMode          `json:"mode,omitempty"`
    Targets      []ModelWarmupTarget      `json:"targets"`
    ImagePreload ModelWarmupImagePreload  `json:"imagePreload,omitempty"`
    Custom       *ModelWarmupCustomAction `json:"custom,omitempty"`
    Policies     *ModelWarmupPolicies     `json:"policies,omitempty"`
}

type ModelWarmupCustomAction struct {
    InitContainers   []corev1.Container            `json:"initContainers,omitempty"`
    Containers       []corev1.Container            `json:"containers,omitempty"`
    Volumes          []corev1.Volume               `json:"volumes,omitempty"`
    ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}
```

The API keeps the existing maximum of 32 image entries and pull secrets and
applies a maximum of 32 entries to each custom list. Existing image-only YAML
and generated Jobs remain valid without modification.

## Admission Rules

The validating webhook enforces the following rules in addition to existing
target, image, mode, and policy validation:

1. At least one regular container must result from either
   `imagePreload.images` or `custom.containers`. An init-container-only object is
   rejected because Kubernetes Pods require a regular container.
2. Custom container names are required and must be valid DNS labels.
3. Names are unique across generated image containers (`image-0`, `image-1`,
   and so on), custom init containers, and custom regular containers.
4. Custom volume names are required, valid, and unique.
5. Every custom `volumeMount` and `volumeDevice` refers to a declared custom
   volume.
6. Custom init containers may not use `restartPolicy: Always`; warmup init
   containers must finish before regular work starts.
7. Custom pull-secret names are required. The controller merges image and custom
   pull secrets by name, preserving first occurrence order.
8. The complete spec remains immutable after creation. Custom JSON must also
   remain identical, including Quantity formats, to preserve the workload revision.

The webhook does not reject `hostPath`, privileged containers, GPU resource
requests, or other native security-context settings. Those capabilities are
intentional, and cluster-level Pod Security, admission policy, and RBAC remain
the enforcement boundary.

## Controller Execution

For every authorized target node and revision, the controller continues to
create one Job with required node affinity. It owns and fixes the following Pod
and Job settings:

- target-node affinity;
- `restartPolicy: Never`;
- `automountServiceAccountToken: false`;
- retry limit, active deadline, and post-completion TTL;
- controller labels, annotations, and owner reference.

The controller constructs the Pod workload as follows:

```text
custom.initContainers (sequential)
             |
             v
+---------------- generated Job regular containers ----------------+
| image preload 0 | ... | image preload N | custom containers ...   |
+-------------------------------------------------------------------+
             |
             v
      Job completion or failure
             |
             v
 existing per-node and aggregate ModelWarmup status
```

Generated image-preload containers retain their current default pull policy and
`allowPrivilegeEscalation: false` setting. Custom containers and volumes are
deep-copied without policy mutation. Custom actions must be finite: a regular
container that does not exit is eventually terminated by the Job active
deadline.

## Revision Compatibility

The current image-only revision algorithm remains unchanged when `custom` is
absent. This avoids changing revision values for existing workloads.

When `custom` is present, a deterministic JSON representation of the complete
custom action is added to the revision input. Container and init-container order
is significant because init order is executable behavior. Target membership and
Job policy values remain excluded, matching the existing revision contract.

## Status and Errors

The Job remains the only execution and retry unit. A failed init container or
regular container follows the existing Job retry limit. Terminal Job failure
marks that node failed; aggregate status continues to resolve to `Succeeded`,
`Failed`, or `Degraded` using current rules.

Image-only success retains the existing `ImagePreloadSucceeded` reason. A
warmup containing `custom` uses the generic `WarmupSucceeded` reason. The
controller does not parse stdout, introduce action-specific reasons, or expose
per-container status. Existing bounded Job diagnostics cover failures observed
on created Jobs, including timeouts and runtime failures. Job API creation
rejections are logged and requeued; because no Job exists, they may not appear
in bounded Job diagnostics or ModelWarmup failure status.

## Security Model

Permission to create a `ModelWarmup` with `custom` is equivalent to permission
to run the supplied containers and volumes on eligible warmup nodes. In
particular, `hostPath`, privileged security contexts, host-visible devices, and
secret references can expose node or namespace resources.

The controller limits this surface by accepting only containers, init
containers, volumes, and image pull secrets. Users cannot override affinity,
service accounts, automatic token mounting, restart policy, Job lifecycle, or
controller metadata. Explicit projected `serviceAccountToken` volumes remain
allowed despite `automountServiceAccountToken: false`; their tokens use the
Pod's service account permissions (the namespace default service account unless
cluster admission changes it). Explicit secret volumes can also expose namespace
credentials. Documentation and samples must state these privilege implications,
recommend least-privilege service accounts, and restrict create/update permission
for ModelWarmup resources.

## Samples and Documentation

Keep `samples/modelwarmup/modelwarmup.yaml` as the backward-compatible
image-only example and add:

- `model-download.yaml`: use the public `aibrix/runtime:v0.7.0` image and
  `sshleifer/tiny-gpt2`, writing to a hostPath cache.
- `node-precheck.yaml`: use BusyBox to validate cache-directory writability and
  available disk space without requiring a GPU.
- `gpu-precheck.yaml`: use the public
  `nvidia/cuda:12.4.1-base-ubuntu22.04` image to run `nvidia-smi`; document the
  NVIDIA device plugin and GPU node prerequisites.
- `combined-warmup.yaml`: run a generic precheck first, then execute image
  preloading and the small-model download as concurrent regular containers.

The sample README and feature documentation explain prerequisites, completion
semantics, cache sharing, public image/model references, security boundaries,
and the distinction between an observed successful warmup and durable cache
pinning.

## Test Strategy

- API/webhook unit tests cover all three valid modes, init-only rejection,
  list bounds, reserved and duplicate container names, duplicate volumes,
  missing mount/device references, invalid names, sidecar-style init rejection,
  pull-secret validation, and immutable updates.
- Controller unit tests verify exact Job merging, stable pull-secret deduplication,
  preservation of custom fields, image-only revision compatibility, and revision
  changes for every custom action component.
- Webhook integration tests verify CRD schema optionality and admission behavior.
- Controller integration tests verify that custom-only and combined resources
  produce the expected node-pinned Job templates and status behavior.
- A CI-stable non-GPU end-to-end path uses a synthetic combined warmup and
  observes Job and ModelWarmup completion. Public samples are strict-decoded and
  admission-tested; executing their downloads, host-path checks, and GPU work is
  environment validation and is not required in generic CI.
- Regenerate deepcopy, clients, apply configurations, CRD, and RBAC artifacts;
  run focused tests first, followed by repository generation checks and relevant
  lint/test targets.

## Success Criteria

- Existing image-only manifests and Go field assignments continue to work.
- Image-only, custom-only, and combined ModelWarmups create correct node-pinned
  Jobs and reach status through the existing aggregation flow.
- Invalid or ambiguous fragments covered by ModelWarmup validation are rejected
  before reconciliation; native Job/Pod validation and cluster policy still apply
  when the controller creates the Job and Kubernetes creates its Pods.
- Every custom action change invalidates the custom workload revision, while an
  image-only object's revision remains compatible with the current release.
- Generated artifacts, focused unit and integration tests, formatting, linting,
  and verification checks pass.
