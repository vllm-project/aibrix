---
change: support-scheduled-replica-bounds
design-doc: docs/superpowers/specs/2026-08-05-scheduled-replica-bounds-design.md
base-ref: e7574c4610d611c0024c50b81adb6affc171cc68
---

# Scheduled Replica Bounds Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add scheduled replica bound windows to `PodAutoscaler` and apply effective bounds consistently to custom PA strategies and generated HPA resources.

**Architecture:** Add typed API fields and a focused resolver that computes effective min/max bounds from base spec fields plus the active `cron + duration` window. Admission webhook and controller fallback validation share the same semantic checks; reconciliation passes effective bounds to boundary checks, scaling context, and HPA generation.

**Tech Stack:** Go, controller-runtime, Kubernetes CRDs/code generation, `metav1.Time`, `metav1.Duration`, Go `time`, a small maintained cron parser, existing AIBrix PodAutoscaler tests.

---

## File Structure

- Modify `api/autoscaling/v1alpha1/podautoscaler_types.go` for `ScheduledReplicaBounds` and `PodAutoscalerSpec.ScheduledBounds`.
- Create `pkg/controller/podautoscaler/scheduled_bounds.go` for resolver and validation helpers.
- Create `pkg/controller/podautoscaler/scheduled_bounds_test.go` for fixed-time resolver tests.
- Modify `pkg/webhook/podautoscaler_webhook.go` and `pkg/webhook/podautoscaler_webhook_test.go` for admission validation.
- Modify `pkg/controller/podautoscaler/podautoscaler_controller.go` and tests for custom strategy effective bounds.
- Modify `pkg/controller/podautoscaler/hpa_resources.go` and tests for HPA effective bounds.
- Regenerate API artifacts under `api/`, `pkg/client/`, and `config/crd/`.
- Preserve and verify `docs/source/features/autoscaling/metric-based-autoscaling.rst`; add scheduled bounds docs and sample YAML.

### Task 1: API Shape and Dependency

**Files:**
- Modify: `api/autoscaling/v1alpha1/podautoscaler_types.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Test: `api/autoscaling/v1alpha1/podautoscaler_types_test.go`

- [x] **Step 1: Add failing API serialization test**

Add a test that creates a `PodAutoscaler` with `ScheduledBounds`, marshals it to JSON, unmarshals it back, and verifies all scheduled fields and pointer values survive. Do not require generated deepcopy output in this task; generated deepcopy verification happens after `make generate`.

Run: `go test ./api/autoscaling/v1alpha1 -run TestPodAutoscalerScheduledBoundsJSONRoundTrip -count=1`
Expected: FAIL because `ScheduledBounds` is not defined.

- [x] **Step 2: Add API fields**

Add `ScheduledReplicaBounds` near `PodAutoscalerSpec` and add `ScheduledBounds []ScheduledReplicaBounds` to `PodAutoscalerSpec`.

Use these field names: `name`, `timezone`, `startTime`, `endTime`, `cron`, `duration`, `minReplicas`, `maxReplicas`.

- [x] **Step 3: Add cron dependency**

Choose a small maintained cron parser, then run:

```bash
go get github.com/robfig/cron/v3@latest
```

Expected: `go.mod` and `go.sum` update.

- [x] **Step 4: Run API tests**

Run: `go test ./api/autoscaling/v1alpha1 -count=1`
Expected: PASS.

- [x] **Step 5: Commit API shape**

```bash
git add api/autoscaling/v1alpha1/podautoscaler_types.go api/autoscaling/v1alpha1/podautoscaler_types_test.go go.mod go.sum
git commit -m "feat: add PodAutoscaler scheduled bounds API"
```

### Task 2: Effective Bounds Resolver

**Files:**
- Create: `pkg/controller/podautoscaler/scheduled_bounds.go`
- Create: `pkg/controller/podautoscaler/scheduled_bounds_test.go`
- Modify: `openspec/changes/support-scheduled-replica-bounds/tasks.md`

- [ ] **Step 1: Write resolver tests first**

Cover:
- no schedules uses base bounds
- matching window overrides both bounds
- partial override keeps the other base bound
- non-matching window uses base bounds
- timezone affects matching
- omitted timezone uses UTC
- start/end lifetime gates matching
- zero minimum is preserved
- invalid cron/duration/timezone returns an error

Run: `go test ./pkg/controller/podautoscaler -run 'TestResolveEffectiveReplicaBounds|TestValidateScheduledBounds' -count=1`
Expected: FAIL because resolver does not exist.

- [ ] **Step 2: Implement resolver**

Create a small API:

```go
type effectiveReplicaBounds struct {
	MinReplicas int32
	MaxReplicas int32
	ScheduleName string
}

func resolveEffectiveReplicaBounds(pa *autoscalingv1alpha1.PodAutoscaler, now time.Time) (effectiveReplicaBounds, error)
```

Use UTC when `Timezone` is empty. Treat an active window as `[occurrence, occurrence+duration)`.

- [ ] **Step 3: Implement semantic validation helper**

Add helper logic that validation callers can reuse conceptually:

```go
func validateScheduledBounds(pa *autoscalingv1alpha1.PodAutoscaler) field.ErrorList
```

If import layering makes `field.ErrorList` unsuitable outside webhook code, return plain errors from core helpers and map them to fields in webhook/controller callers.

- [ ] **Step 4: Run resolver tests**

Run: `go test ./pkg/controller/podautoscaler -run 'TestResolveEffectiveReplicaBounds|TestValidateScheduledBounds' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit resolver**

```bash
git add pkg/controller/podautoscaler/scheduled_bounds.go pkg/controller/podautoscaler/scheduled_bounds_test.go openspec/changes/support-scheduled-replica-bounds/tasks.md
git commit -m "feat: resolve PodAutoscaler scheduled bounds"
```

### Task 3: Validation

**Files:**
- Modify: `pkg/webhook/podautoscaler_webhook.go`
- Modify: `pkg/webhook/podautoscaler_webhook_test.go`
- Modify: `test/integration/webhook/podautoscaler_webhook_test.go`
- Modify: `pkg/controller/podautoscaler/podautoscaler_controller.go`
- Modify: `pkg/controller/podautoscaler/podautoscaler_controller_test.go`

- [ ] **Step 1: Add failing webhook tests**

Add cases for valid scheduled bounds, invalid cron, invalid duration, invalid timezone, duplicate name, missing min/max override, invalid effective min/max, and overlap.

Run: `go test ./pkg/webhook -run TestPodAutoscaler -count=1`
Expected: FAIL on missing validation.

- [ ] **Step 2: Add failing controller fallback tests**

Add `validateSpec` tests for invalid scheduled bound configurations when admission is bypassed.

Run: `go test ./pkg/controller/podautoscaler -run TestValidateSpec -count=1`
Expected: FAIL on missing fallback validation.

- [ ] **Step 3: Implement webhook validation**

Map semantic validation errors to `field.NewPath("spec").Child("scheduledBounds").Index(i)` and the concrete child field where possible.

- [ ] **Step 4: Implement controller fallback validation**

Call the same validation semantics from `validateSpec` after static replica bound validation and before metrics validation.

- [ ] **Step 5: Run validation tests**

Run:

```bash
go test ./pkg/webhook -count=1
go test ./pkg/controller/podautoscaler -run TestValidateSpec -count=1
go test ./test/integration/webhook -run PodAutoscaler -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit validation**

```bash
git add pkg/webhook/podautoscaler_webhook.go pkg/webhook/podautoscaler_webhook_test.go test/integration/webhook/podautoscaler_webhook_test.go pkg/controller/podautoscaler/podautoscaler_controller.go pkg/controller/podautoscaler/podautoscaler_controller_test.go
git commit -m "feat: validate PodAutoscaler scheduled bounds"
```

### Task 4: Controller and HPA Integration

**Files:**
- Modify: `pkg/controller/podautoscaler/podautoscaler_controller.go`
- Modify: `pkg/controller/podautoscaler/podautoscaler_controller_test.go`
- Modify: `pkg/controller/podautoscaler/hpa_resources.go`
- Modify: `pkg/controller/podautoscaler/hpa_resources_test.go`
- Modify: `pkg/controller/podautoscaler/context/context_test.go`

- [ ] **Step 1: Add failing custom strategy tests**

Add tests proving current replicas above scheduled max scale down and below scheduled min scale up.

Run: `go test ./pkg/controller/podautoscaler -run 'TestComputeScaleDecision.*Scheduled' -count=1`
Expected: FAIL because static bounds are still used.

- [ ] **Step 2: Add failing HPA tests**

Add tests proving `makeHPA` uses scheduled effective min/max during a matching window and returns to base bounds outside the window.

Run: `go test ./pkg/controller/podautoscaler -run 'TestMakeHPA.*Scheduled' -count=1`
Expected: FAIL.

- [ ] **Step 3: Wire effective bounds into custom PA**

Use `resolveEffectiveReplicaBounds(&pa, time.Now())` once per reconciliation path and pass the result into boundary checks and scaling context setup. Keep original `pa.Spec` intact.

- [ ] **Step 4: Wire effective bounds into HPA**

Update `makeHPA` or its caller so generated HPA min/max fields come from effective bounds. Preserve existing behavior that omits HPA `MinReplicas` when effective minimum is zero.

- [ ] **Step 5: Run controller tests**

Run:

```bash
go test ./pkg/controller/podautoscaler -count=1
go test ./pkg/controller/podautoscaler/context -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit controller integration**

```bash
git add pkg/controller/podautoscaler/podautoscaler_controller.go pkg/controller/podautoscaler/podautoscaler_controller_test.go pkg/controller/podautoscaler/hpa_resources.go pkg/controller/podautoscaler/hpa_resources_test.go pkg/controller/podautoscaler/context/context_test.go
git commit -m "feat: apply scheduled bounds during autoscaling"
```

### Task 5: Generated Artifacts, Docs, and Final Verification

**Files:**
- Modify: `api/autoscaling/v1alpha1/zz_generated.deepcopy.go`
- Modify: `pkg/client/**`
- Modify: `config/crd/autoscaling/autoscaling.aibrix.ai_podautoscalers.yaml`
- Modify: `samples/autoscaling/*.yaml` or `config/samples/*.yaml`
- Modify: `docs/source/features/autoscaling/metric-based-autoscaling.rst`
- Modify: `openspec/changes/support-scheduled-replica-bounds/tasks.md`

- [ ] **Step 1: Regenerate code and manifests**

Run the repository generation command used for API changes, usually:

```bash
make generate
make manifests
```

Expected: generated clients, deepcopy, and CRD schema include `scheduledBounds`.

- [ ] **Step 2: Add scheduled bounds docs and sample**

Extend autoscaling docs with scheduled bounds semantics and preserve the already-included metric-window section. Add a sample showing `cron + duration`.

- [ ] **Step 3: Run verification**

Run:

```bash
go test ./api/autoscaling/v1alpha1 ./pkg/webhook ./pkg/controller/podautoscaler ./pkg/controller/podautoscaler/context -count=1
./hack/verify-crd-sync.sh
git diff --check
```

Expected: PASS.

- [ ] **Step 4: Mark OpenSpec tasks complete**

Update `openspec/changes/support-scheduled-replica-bounds/tasks.md` checkboxes only after implementation and verification pass.

- [ ] **Step 5: Commit final artifacts**

```bash
git add api pkg config samples docs openspec/changes/support-scheduled-replica-bounds/tasks.md
git commit -m "docs: add scheduled bounds examples and generated artifacts"
```

## Self-Review

- Spec coverage: API declaration, schedule matching, validation, custom PA, HPA, docs, and generated-code verification all map to tasks.
- Placeholder scan: no task uses TBD/TODO/fill-in placeholders.
- Type consistency: plan consistently uses `ScheduledReplicaBounds`, `scheduledBounds`, `cron`, `duration`, `timezone`, `startTime`, `endTime`, `minReplicas`, and `maxReplicas`.
