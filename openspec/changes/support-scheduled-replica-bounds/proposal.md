## Why

GenAI inference traffic often follows predictable schedules, but `PodAutoscaler` currently exposes only one static `minReplicas` and `maxReplicas` pair. Operators who want higher warm GPU capacity during known peak windows and lower bounds outside those windows must patch resources externally, which makes autoscaling behavior harder to declare, audit, and reason about.

## What Changes

- Add optional scheduled replica bounds to `PodAutoscalerSpec`.
- Resolve an effective `minReplicas` and `maxReplicas` during reconciliation based on the current time and the active cron-triggered schedule window.
- Apply effective bounds consistently to custom PodAutoscaler strategies (`KPA` and `APA`) and generated Kubernetes `HorizontalPodAutoscaler` resources for `HPA`.
- Validate schedule shape, cron expression, duration, timezone, date range, overlaps, and effective bounds through admission webhook and controller-side fallback validation.
- Surface invalid schedule configuration through existing invalid-spec status paths when admission validation is bypassed.
- Add unit and integration coverage for API validation, effective-bound resolution, custom strategy clamping, and HPA reconciliation.

No breaking changes are intended. Existing `PodAutoscaler` resources without `scheduledBounds` continue to use static `minReplicas` and `maxReplicas`.

## Capabilities

### New Capabilities

- `scheduled-replica-bounds`: PodAutoscaler can declare cron-triggered replica bound override windows and the controller applies the active effective bounds during autoscaling reconciliation.

### Modified Capabilities

- None.

## Impact

- API: `api/autoscaling/v1alpha1/podautoscaler_types.go`, generated deepcopy/client/applyconfiguration code, and CRD manifests.
- Validation: `pkg/webhook/podautoscaler_webhook.go`, controller fallback spec validation, cron/timezone/duration parsing, and webhook integration tests.
- Controller behavior: `pkg/controller/podautoscaler/podautoscaler_controller.go`, `pkg/controller/podautoscaler/hpa_resources.go`, and supporting PodAutoscaler context helpers.
- Dependencies: add a maintained cron parser and use Go time zone parsing for schedule evaluation.
- Documentation and samples: autoscaling docs and sample `PodAutoscaler` manifests should show scheduled bounds after implementation.
