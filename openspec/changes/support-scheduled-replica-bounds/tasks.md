## 1. API and Generated Artifacts

- [x] 1.1 Add `ScheduledReplicaBounds` and `scheduledBounds` fields to `api/autoscaling/v1alpha1/podautoscaler_types.go` with kubebuilder validation markers.
- [x] 1.2 Regenerate deepcopy, clientset, informer, lister, applyconfiguration, and CRD manifests for the updated PodAutoscaler API.
- [x] 1.3 Add or update PodAutoscaler API tests covering deepcopy/default serialization behavior for `scheduledBounds`.

## 2. Effective Bounds Resolver

- [x] 2.1 Add a focused resolver that computes effective min/max bounds from a `PodAutoscaler`, a fixed `time.Time`, and the schedule list.
- [x] 2.2 Add unit tests for no schedules, matching windows, non-matching windows, partial overrides, timezone handling, duration handling, start/end lifetime handling, and zero minimum behavior.
- [x] 2.3 Add deterministic error handling for invalid runtime schedule configuration used by controller fallback validation.

## 3. Validation

- [x] 3.1 Extend validating admission webhook checks for schedule name uniqueness, cron parsing, timezone parsing, duration parsing, lifetime ordering, required override fields, effective min/max validity, and overlapping schedule windows.
- [x] 3.2 Extend controller-side `validateSpec` fallback validation to reject the same invalid scheduled-bound configurations when the webhook is bypassed.
- [x] 3.3 Add webhook unit and integration cases for valid schedules, invalid cron/timezone/duration, invalid effective bounds, missing overrides, duplicate names, and overlaps.

## 4. Controller Integration

- [x] 4.1 Update custom strategy boundary checks in `computeScaleDecision` to use effective min/max bounds instead of static spec bounds.
- [x] 4.2 Update `createScalingContext` to set effective bounds so KPA/APA algorithm clamping and stabilization use scheduled bounds.
- [x] 4.3 Update HPA reconciliation so `makeHPA` receives or resolves effective bounds and writes them to the generated HPA spec.
- [x] 4.4 Add controller tests proving scheduled bounds clamp custom strategy scaling and update generated HPA min/max during and after a matching window.

## 5. Documentation and Verification

- [x] 5.1 Add sample YAML showing weekday business-hour scheduled bounds.
- [x] 5.2 Update autoscaling documentation with API semantics, validation rules, overlap behavior, timezone behavior, and HPA compatibility notes.
- [x] 5.3 Run targeted Go tests for API, webhook, and PodAutoscaler controller packages.
- [x] 5.4 Run CRD/generated-code verification commands used by this repository.
