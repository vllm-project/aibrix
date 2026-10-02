# ModelWarmup Custom Actions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add backward-compatible custom init containers, regular containers, volumes, and pull secrets to ModelWarmup, with runnable image, download, precheck, GPU, and combined samples.

**Architecture:** Keep one controller-owned, node-pinned Job per target and revision. Merge a constrained native Kubernetes Pod fragment into that Job, validate cross-field invariants in the webhook, and continue deriving all status from the Job.

**Tech Stack:** Go 1.22, Kubernetes core/batch APIs, controller-runtime, kubebuilder/controller-gen, Ginkgo/Gomega, testify.

---

## File Structure

- Modify `api/model/v1alpha1/modelwarmup_types.go`: public API, bounds, and custom action type.
- Modify `pkg/webhook/modelwarmup_webhook.go`: custom fragment and cross-reference validation.
- Modify `pkg/webhook/modelwarmup_webhook_test.go`: unit admission coverage.
- Modify `pkg/controller/modelwarmup/modelwarmup_controller.go`: revision, Pod merge, secret merge, and success reason.
- Modify `pkg/controller/modelwarmup/modelwarmup_controller_test.go`: controller unit coverage.
- Modify `test/integration/webhook/modelwarmup_test.go`: API-server schema and webhook coverage.
- Modify `test/integration/controller/modelwarmup_test.go`: generated Job and status coverage.
- Modify `test/e2e/controller/modelwarmup/modelwarmup_test.go`: custom-only and combined live Job coverage.
- Modify `test/utils/controller/modelwarmup.go`: reusable custom warmup fixture only if integration/E2E duplication warrants it.
- Regenerate `api/model/v1alpha1/zz_generated.deepcopy.go`, `pkg/client/**`, `config/crd/bases/model.aibrix.ai_modelwarmups.yaml`, `config/crd/model/model.aibrix.ai_modelwarmups.yaml`, and `dist/chart/crds/model.aibrix.ai_modelwarmups.yaml`.
- Keep `samples/modelwarmup/modelwarmup.yaml`; add `model-download.yaml`, `node-precheck.yaml`, `gpu-precheck.yaml`, and `combined-warmup.yaml`.
- Modify `samples/modelwarmup/README.md` and `docs/source/features/model-warmup.rst`.

## Task 1: Add the Backward-Compatible API Contract

**Files:**
- Modify: `api/model/v1alpha1/modelwarmup_types.go:24-105`
- Test: `api/model/v1alpha1/modelwarmup_types_test.go`

- [ ] **Step 1: Write a failing JSON round-trip test without referencing the missing Go field**

Add a test that unmarshals a ModelWarmup containing `spec.custom`, marshals it again, and asserts that `custom`, `initContainers`, `containers`, `volumes`, and `imagePullSecrets` remain present:

```go
func TestModelWarmupCustomActionJSONRoundTrip(t *testing.T) {
    raw := []byte(`{"spec":{"targets":[{"nodes":{"names":["node-a"]}}],"custom":{"initContainers":[{"name":"check","image":"busybox"}],"containers":[{"name":"download","image":"aibrix/runtime:v0.7.0"}],"volumes":[{"name":"cache","emptyDir":{}}],"imagePullSecrets":[{"name":"registry"}]}}}`)
    var warmup ModelWarmup
    require.NoError(t, json.Unmarshal(raw, &warmup))
    encoded, err := json.Marshal(&warmup)
    require.NoError(t, err)
    var roundTripped map[string]interface{}
    require.NoError(t, json.Unmarshal(encoded, &roundTripped))
    spec := roundTripped["spec"].(map[string]interface{})
    custom, exists := spec["custom"].(map[string]interface{})
    require.True(t, exists)
    require.Len(t, custom["initContainers"], 1)
    require.Len(t, custom["containers"], 1)
    require.Len(t, custom["volumes"], 1)
    require.Len(t, custom["imagePullSecrets"], 1)
}
```

- [ ] **Step 2: Run the test and verify RED**

Run: `GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache go test ./api/model/v1alpha1 -run TestModelWarmupCustomActionJSONRoundTrip -count=1`

Expected: FAIL because the unknown `custom` field is discarded during unmarshal.

- [ ] **Step 3: Add the API type and bounds**

Add `+optional` and `omitempty` to `ImagePreload`, add `Custom *ModelWarmupCustomAction`, and define:

```go
const (
    MaxModelWarmupCustomInitContainers = 32
    MaxModelWarmupCustomContainers     = 32
    MaxModelWarmupCustomVolumes        = 32
    MaxModelWarmupCustomPullSecrets    = 32
)

type ModelWarmupCustomAction struct {
    // +optional
    // +kubebuilder:validation:MaxItems=32
    InitContainers []corev1.Container `json:"initContainers,omitempty"`
    // +optional
    // +kubebuilder:validation:MaxItems=32
    Containers []corev1.Container `json:"containers,omitempty"`
    // +optional
    // +kubebuilder:validation:MaxItems=32
    Volumes []corev1.Volume `json:"volumes,omitempty"`
    // +optional
    // +kubebuilder:validation:MaxItems=32
    ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}
```

- [ ] **Step 4: Run the API test and verify GREEN**

Run the command from Step 2.

Expected: PASS.

- [ ] **Step 5: Commit the API source and test**

```bash
git add api/model/v1alpha1/modelwarmup_types.go api/model/v1alpha1/modelwarmup_types_test.go
git commit -m "api: add ModelWarmup custom actions"
```

## Task 2: Validate Custom Pod Fragments

**Files:**
- Modify: `pkg/webhook/modelwarmup_webhook_test.go`
- Modify: `pkg/webhook/modelwarmup_webhook.go:65-202`

- [ ] **Step 1: Add failing tests for valid execution modes**

Add table cases for image-only, custom-only, and combined objects. The custom-only fixture must clear `ImagePreload.Images` and provide:

```go
warmup.Spec.Custom = &modelapi.ModelWarmupCustomAction{
    InitContainers: []corev1.Container{{Name: "check", Image: "busybox:1.36"}},
    Containers: []corev1.Container{{Name: "download", Image: "aibrix/runtime:v0.7.0"}},
    Volumes: []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{
        EmptyDir: &corev1.EmptyDirVolumeSource{},
    }}},
}
```

- [ ] **Step 2: Add failing rejection tables**

Cover these exact mutations and expected field paths:

- no image and no custom regular container: `spec.custom.containers`;
- init-only custom action: `spec.custom.containers`;
- more than 32 init containers, containers, volumes, or pull secrets;
- missing/invalid/duplicate names across init, regular, and reserved `image-0` names;
- empty or duplicate volume names;
- mount/device references to undeclared volumes;
- `restartPolicy: Always` on a custom init container;
- empty or invalid custom pull-secret names.

- [ ] **Step 3: Run webhook unit tests and verify RED**

Run: `GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache go test ./pkg/webhook -run ModelWarmup -count=1`

Expected: FAIL because image preload is still mandatory and custom validation does not exist.

- [ ] **Step 4: Implement custom validation**

Refactor image validation so an empty image list is valid when custom regular containers exist. Add helpers with these responsibilities:

```go
func validateModelWarmupCustom(custom *modelapi.ModelWarmupCustomAction, imageCount int, path *field.Path) field.ErrorList
func validateModelWarmupContainer(container corev1.Container, path *field.Path, names map[string]struct{}, volumes map[string]struct{}, init bool) field.ErrorList
func validateModelWarmupVolumeReferences(container corev1.Container, path *field.Path, volumes map[string]struct{}) field.ErrorList
```

Use `k8s.io/apimachinery/pkg/util/validation.IsDNS1123Label` for container and volume names and `IsDNS1123Subdomain` for secret names. Seed the name set with `image-0` through `image-(n-1)`. Validate list bounds before iterating. Build the declared volume set before checking mounts and devices.

- [ ] **Step 5: Run webhook unit tests and verify GREEN**

Run the command from Step 3, then run:

`GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache go test ./pkg/webhook -count=1`

Expected: PASS.

- [ ] **Step 6: Commit webhook behavior**

```bash
git add pkg/webhook/modelwarmup_webhook.go pkg/webhook/modelwarmup_webhook_test.go
git commit -m "feat: validate ModelWarmup custom actions"
```

## Task 3: Merge Custom Actions into Jobs and Revisions

**Files:**
- Modify: `pkg/controller/modelwarmup/modelwarmup_controller_test.go`
- Modify: `pkg/controller/modelwarmup/modelwarmup_controller.go:375-451,580-590`

- [ ] **Step 1: Add a failing Job merge test**

Construct a combined warmup with one image, one init container, one custom regular container, one hostPath volume, and overlapping image/custom pull secrets. Assert:

```go
require.Equal(t, []string{"check"}, containerNames(job.Spec.Template.Spec.InitContainers))
require.Equal(t, []string{"image-0", "download"}, containerNames(job.Spec.Template.Spec.Containers))
require.Equal(t, []string{"cache"}, volumeNames(job.Spec.Template.Spec.Volumes))
require.Equal(t, []corev1.LocalObjectReference{{Name: "shared"}, {Name: "custom"}}, job.Spec.Template.Spec.ImagePullSecrets)
require.Equal(t, ptr.To(true), job.Spec.Template.Spec.Containers[1].SecurityContext.Privileged)
```

Also assert that controller-owned affinity, restart policy, and token mounting remain unchanged.

- [ ] **Step 2: Add failing revision tests**

Preserve the current expected image-only revision and assert that changing each custom component changes the revision: init container image/order, regular container command/env/resources, volume source, and custom pull secret. Assert target and policy changes still do not affect it.

- [ ] **Step 3: Run controller tests and verify RED**

Run: `GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache go test ./pkg/controller/modelwarmup -run 'Test(JobForMergesCustomAction|Revision)' -count=1`

Expected: FAIL because the Job and revision ignore custom input.

- [ ] **Step 4: Implement deterministic revision extension**

Import `encoding/json`. Keep all existing image-only inputs and sorting unchanged. Only when `Custom != nil`, marshal the complete action and append a `custom=`-prefixed value before the final sort/hash:

```go
if w.Spec.Custom != nil {
    custom, err := json.Marshal(w.Spec.Custom)
    if err != nil {
        panic(fmt.Sprintf("marshal ModelWarmup custom action: %v", err))
    }
    parts = append(parts, "custom="+string(custom))
}
```

- [ ] **Step 5: Implement the Job merge**

Deep-copy custom containers and volumes. Append custom regular containers after generated image containers. Assign custom init containers and volumes directly from the copy. Merge pull secrets with a helper that preserves first occurrence order:

```go
func mergePullSecrets(groups ...[]corev1.LocalObjectReference) []corev1.LocalObjectReference {
    seen := map[string]struct{}{}
    var merged []corev1.LocalObjectReference
    for _, group := range groups {
        for _, secret := range group {
            if _, ok := seen[secret.Name]; ok { continue }
            seen[secret.Name] = struct{}{}
            merged = append(merged, secret)
        }
    }
    return merged
}
```

- [ ] **Step 6: Make success reasons accurate**

Keep `ImagePreloadSucceeded` for `Custom == nil`; return `WarmupSucceeded` when custom is present. Use the helper only at the existing successful terminal-condition update so failure semantics remain untouched.

- [ ] **Step 7: Run controller tests and verify GREEN**

Run the command from Step 3, then:

`GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache go test ./pkg/controller/modelwarmup -count=1`

Expected: PASS.

- [ ] **Step 8: Commit controller behavior**

```bash
git add pkg/controller/modelwarmup/modelwarmup_controller.go pkg/controller/modelwarmup/modelwarmup_controller_test.go
git commit -m "feat: execute ModelWarmup custom actions"
```

## Task 4: Regenerate API Artifacts and Add Integration Coverage

**Files:**
- Modify: `test/integration/webhook/modelwarmup_test.go`
- Modify: `test/integration/controller/modelwarmup_test.go`
- Regenerate: `api/model/v1alpha1/zz_generated.deepcopy.go`
- Regenerate: `pkg/client/**`
- Regenerate: `config/crd/bases/model.aibrix.ai_modelwarmups.yaml`
- Regenerate: `config/crd/model/model.aibrix.ai_modelwarmups.yaml`
- Regenerate: `dist/chart/crds/model.aibrix.ai_modelwarmups.yaml`

- [ ] **Step 1: Add failing webhook integration cases**

Create custom-only and combined resources successfully. Add invalid init-only, `image-0` collision, and missing volume-reference cases to the existing rejection table. Fetch the custom-only object and assert embedded env, resource, volume, and security-context fields survive API-server round trip.

- [ ] **Step 2: Add a failing controller integration case**

Create a combined warmup and wait for one Job. Assert the Job contains the custom init/regular containers, volume, deduplicated secrets, controller affinity, active deadline, and `automountServiceAccountToken: false`. Mark the Job succeeded and assert the ModelWarmup reaches `Succeeded` with `Complete=True` and reason `WarmupSucceeded`.

- [ ] **Step 3: Generate deepcopy, clients, apply configurations, CRDs, and chart CRDs**

Run:

```bash
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache make generate
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache make manifests-all
```

Confirm the CRD no longer lists `imagePreload` as required and includes the structural schemas for custom containers and volumes.

- [ ] **Step 4: Run integration tests**

Run:

```bash
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache make test-integration-webhook
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache make test-integration-controller
```

Expected: all ModelWarmup and existing integration specs pass.

- [ ] **Step 5: Commit generated and integration changes**

```bash
git add api/model/v1alpha1/zz_generated.deepcopy.go pkg/client config/crd/bases/model.aibrix.ai_modelwarmups.yaml config/crd/model/model.aibrix.ai_modelwarmups.yaml dist/chart/crds/model.aibrix.ai_modelwarmups.yaml test/integration/webhook/modelwarmup_test.go test/integration/controller/modelwarmup_test.go
git commit -m "test: cover ModelWarmup custom actions"
```

## Task 5: Add Runnable Public Samples and Documentation

**Files:**
- Keep: `samples/modelwarmup/modelwarmup.yaml`
- Create: `samples/modelwarmup/model-download.yaml`
- Create: `samples/modelwarmup/node-precheck.yaml`
- Create: `samples/modelwarmup/gpu-precheck.yaml`
- Create: `samples/modelwarmup/combined-warmup.yaml`
- Create: `pkg/webhook/modelwarmup_samples_test.go`
- Modify: `samples/modelwarmup/README.md`
- Modify: `docs/source/features/model-warmup.rst`

- [ ] **Step 1: Add a failing sample admission test**

Create `pkg/webhook/modelwarmup_samples_test.go`. Read the five exact YAML paths with `os.ReadFile`, decode each using `sigs.k8s.io/yaml.Unmarshal`, and call `validateModelWarmup`. Assert every file decodes and validates. Also assert `model-download.yaml` has no image entries, `combined-warmup.yaml` has both image and custom regular containers, and `gpu-precheck.yaml` contains a `nvidia.com/gpu` limit.

Run:

`GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache go test ./pkg/webhook -run TestModelWarmupSamples -count=1`

Expected: FAIL because the four new sample files do not exist yet.

- [ ] **Step 2: Add the public samples**

Use these concrete public artifacts and finite commands:

- image-only: existing `busybox:1.36` and `sh -c 'exit 0'`;
- model download: `aibrix/runtime:v0.7.0`, `aibrix_download --model-uri sshleifer/tiny-gpt2 --local-dir /models/sshleifer-tiny-gpt2`, and hostPath `/var/lib/aibrix/models`;
- generic precheck: `busybox:1.36`, checking `/models` is writable and `df -Pk /models` reports available blocks, followed by a regular `busybox` success container;
- GPU precheck: `nvidia/cuda:12.4.1-base-ubuntu22.04`, `nvidia-smi`, and a `nvidia.com/gpu: "1"` limit, followed by a regular `busybox` success container;
- combined: the generic precheck as an init container, existing image preload, and the tiny-model downloader as a custom regular container sharing the hostPath cache.

Every manifest uses the existing latency-pool selector and finite Job policies.

- [ ] **Step 3: Document execution and security**

Update both documentation files to explain image-only, custom-only, and combined execution; sequential init versus concurrent regular containers; hostPath and privileged security implications; RBAC guidance; finite-command requirements; NVIDIA prerequisites; and point-in-time cache status.

- [ ] **Step 4: Validate YAML and documentation references**

Run:

```bash
for file in samples/modelwarmup/*.yaml; do kubectl apply --dry-run=client -f "$file"; done
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache go test ./pkg/webhook -run TestModelWarmupSamples -count=1
rg -n 'model-download.yaml|node-precheck.yaml|gpu-precheck.yaml|combined-warmup.yaml' samples/modelwarmup/README.md docs/source/features/model-warmup.rst
```

Expected: every YAML parses and every sample is referenced in documentation.

- [ ] **Step 5: Commit samples and docs**

```bash
git add samples/modelwarmup docs/source/features/model-warmup.rst pkg/webhook/modelwarmup_samples_test.go
git commit -m "docs: add ModelWarmup custom action samples"
```

## Task 6: Add the Live Combined E2E Path

**Files:**
- Modify: `test/e2e/controller/modelwarmup/modelwarmup_test.go`

- [ ] **Step 1: Add a failing custom-only/combined E2E test**

Use the already available public `testImage` for CI stability. Create a warmup with:

```go
Custom: &modelapi.ModelWarmupCustomAction{
    InitContainers: []corev1.Container{{
        Name: "precheck", Image: testImage,
        Command: []string{"python", "-c", "from pathlib import Path; assert Path('/cache').is_dir()"},
        VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
    }},
    Containers: []corev1.Container{{
        Name: "prepare", Image: testImage,
        Command: successfulWarmupCommand(),
        VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
    }},
    Volumes: []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{
        EmptyDir: &corev1.EmptyDirVolumeSource{},
    }}},
}
```

Wait for the warmup and Job to succeed, then fetch the Job and assert the exact merged Pod template.

- [ ] **Step 2: Run the narrow E2E target when a cluster is configured**

Run the existing ModelWarmup E2E command documented by the package/Makefile. If no cluster is configured, compile the package and report E2E as not run rather than passed:

`GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache go test ./test/e2e/controller/modelwarmup -run '^$' -count=1`

- [ ] **Step 3: Commit E2E coverage**

```bash
git add test/e2e/controller/modelwarmup/modelwarmup_test.go
git commit -m "test: exercise combined ModelWarmup actions"
```

## Task 7: Final Verification and Review

**Files:**
- Review all files changed since `remote/main`.

- [ ] **Step 1: Format and inspect generated scope**

Run:

```bash
gofmt -w api/model/v1alpha1/modelwarmup_types.go api/model/v1alpha1/modelwarmup_types_test.go pkg/webhook/modelwarmup_webhook.go pkg/webhook/modelwarmup_webhook_test.go pkg/controller/modelwarmup/modelwarmup_controller.go pkg/controller/modelwarmup/modelwarmup_controller_test.go test/integration/webhook/modelwarmup_test.go test/integration/controller/modelwarmup_test.go test/e2e/controller/modelwarmup/modelwarmup_test.go test/utils/controller/modelwarmup.go
git diff --check remote/main...HEAD
git status --short
```

Only retain generated changes caused by the ModelWarmup API.

- [ ] **Step 2: Run focused tests and generated verification**

Run:

```bash
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache go test ./api/model/v1alpha1 ./pkg/webhook ./pkg/controller/modelwarmup -count=1
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache make verify
```

- [ ] **Step 3: Run relevant integration, compile, vet, and lint checks**

Run:

```bash
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache make test-integration-webhook
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache make test-integration-controller
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache go test ./test/e2e/controller/modelwarmup -run '^$' -count=1
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache make vet
GOCACHE=/private/tmp/aibrix-modelwarmup-custom-actions-go-cache make lint
```

- [ ] **Step 4: Review requirements and compatibility line by line**

Confirm image-only JSON/Go source compatibility, three execution modes, validation matrix, image-only revision stability, Job ownership invariants, status reasons, all five samples, security documentation, generated artifacts, and no out-of-scope hooks/stages/gating.

- [ ] **Step 5: Request code review and address findings**

Review `git diff --stat remote/main...HEAD` and `git diff remote/main...HEAD`, then request an independent review against the design and this plan. Fix all Critical and Important findings and rerun affected verification.

- [ ] **Step 6: Commit any final verification fixes**

```bash
git add -u
git commit -m "fix: address ModelWarmup custom action review"
```
