# Comet Design Handoff

- Change: support-scheduled-replica-bounds
- Phase: design
- Mode: compact
- Context hash: a66574b3c5d9851c0878791ce049a2e64546ea335127eb77afa5b8bcc9b33ea6

Generated-by: comet-handoff.sh

OpenSpec remains the canonical capability spec. This handoff is a deterministic, source-traceable context pack, not an agent-authored summary.

## openspec/changes/support-scheduled-replica-bounds/proposal.md

- Source: openspec/changes/support-scheduled-replica-bounds/proposal.md
- Lines: 1-32
- SHA256: a0e7f9ea4d5d2328f647586303b8395fb5a18280cb953c2d7d1301f773e892d3

```md
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

```

## openspec/changes/support-scheduled-replica-bounds/design.md

- Source: openspec/changes/support-scheduled-replica-bounds/design.md
- Lines: 1-113
- SHA256: f479d1bd99c3b579c3c3e0bedf31fe9427a4755336662ec724d882dc4d09fd7d

[TRUNCATED]

```md
## Context

`PodAutoscaler` currently stores static replica bounds in `spec.minReplicas` and `spec.maxReplicas`. Custom strategies (`KPA` and `APA`) read those bounds through `createScalingContext` and clamp both boundary checks and metric recommendations in `computeScaleDecision`. The `HPA` strategy creates or updates a Kubernetes `HorizontalPodAutoscaler` in `makeHPA` by copying the static bounds into the generated HPA spec.

Issue #2520 asks for scheduled replica bounds so operators can declare planned peak and cost-saving windows directly in the `PodAutoscaler` resource. This is a public CRD API change and a cross-cutting controller behavior change: admission validation, controller fallback validation, generated CRD/client artifacts, custom strategy reconciliation, and HPA reconciliation must all agree on the same effective bounds.

## Goals / Non-Goals

**Goals:**

- Add `spec.scheduledBounds` as an optional list of named time-based replica bound overrides.
- Compute effective bounds during reconciliation from base bounds plus the active schedule.
- Apply effective bounds to both custom strategies and generated HPA resources.
- Reject invalid schedule configuration through the validating webhook and surface invalid configuration in status if admission is bypassed.
- Keep existing resources without schedules behaviorally unchanged.

**Non-Goals:**

- Do not build a separate scheduler controller or external patching mechanism.
- Do not support multiple simultaneously active schedules in the initial API; overlapping schedules are invalid.
- Do not add status fields for the active schedule in the first implementation unless required during coding for debuggability.
- Do not change autoscaling algorithms beyond replacing static bounds with effective bounds.

## Decisions

### Store schedules declaratively on `PodAutoscalerSpec`

Add a new API type, tentatively:

```go
type ScheduledReplicaBounds struct {
    Name        string       `json:"name"`
    Timezone    string       `json:"timezone,omitempty"`
    StartTime   *metav1.Time `json:"startTime,omitempty"`
    EndTime     *metav1.Time `json:"endTime,omitempty"`
    Cron        string       `json:"cron"`
    Duration    metav1.Duration `json:"duration"`
    MinReplicas *int32       `json:"minReplicas,omitempty"`
    MaxReplicas *int32       `json:"maxReplicas,omitempty"`
}
```

`PodAutoscalerSpec` gains `ScheduledBounds []ScheduledReplicaBounds`. `cron` defines recurring start instants, `duration` defines how long each occurrence remains active, and optional `startTime` and `endTime` constrain the schedule lifetime. This avoids treating a standard cron expression as a custom window language. The issue example `0 9-18 * * MON-FRI` can represent hourly one-hour windows by setting `duration: 1h`. The implementation should use a maintained cron parser rather than hand parsing cron expressions.

Alternatives considered:

- External automation that patches `minReplicas` and `maxReplicas`: rejected because it splits desired autoscaling behavior across resources and automation jobs.
- Annotation-only schedules: rejected because this is core declarative autoscaling behavior and needs typed validation and generated CRD schema.

### Resolve effective bounds in one controller helper

Introduce a helper near the PodAutoscaler controller, for example `resolveEffectiveReplicaBounds(pa, now)`, that returns:

- effective min replicas, using base `minReplicas` defaulting to 1 when unset;
- effective max replicas, using base `maxReplicas` semantics;
- the matched schedule name, if any;
- an error for invalid runtime configuration.

If no schedule window is active, the helper returns the base bounds. If exactly one schedule window is active, each scheduled field overrides only the corresponding base field. A schedule with only `minReplicas` set changes only the minimum; a schedule with only `maxReplicas` set changes only the maximum.

Alternatives considered:

- Resolve bounds inside each algorithm: rejected because HPA generation and boundary checks would need duplicate logic.
- Mutate `pa.Spec` before existing code runs: rejected because it risks status/update side effects and makes it harder to reason about original versus effective configuration.

### Reject overlapping schedules

Admission validation should reject overlapping schedule windows within their active lifetime. This avoids implicit priority surprises when two schedules could both override bounds. Runtime resolution should still be deterministic and pick the first matching entry if validation was bypassed, but controller fallback validation should mark the spec invalid before scaling proceeds.

Alternatives considered:

- First match wins as the documented API: rejected for the first version because schedule ordering in a Kubernetes spec is easy to misunderstand and hard to audit.
- Highest explicit priority wins: rejected because it adds another API field before there is a proven need.

### Apply effective bounds consistently

Custom strategies should use effective bounds in all existing places that currently read `pa.Spec.MinReplicas` or `pa.Spec.MaxReplicas` for boundary checks, algorithm clamping, and scaling context. HPA strategy should generate HPA `spec.minReplicas` and `spec.maxReplicas` from the same effective bounds.

The existing periodic enqueue loop already reconciles all `PodAutoscaler` objects every `DefaultResyncInterval` (10 seconds), which is sufficient for schedule transitions in the initial design. Implementation can add a targeted `RequeueAfter` to the next known schedule boundary if that falls out naturally, but correctness must not depend on a separate scheduler.


```

Full source: openspec/changes/support-scheduled-replica-bounds/design.md

## openspec/changes/support-scheduled-replica-bounds/tasks.md

- Source: openspec/changes/support-scheduled-replica-bounds/tasks.md
- Lines: 1-31
- SHA256: b045238253349fbc224921eada1fbfd81039af13e5d906859c93b64a687d361e

```md
## 1. API and Generated Artifacts

- [ ] 1.1 Add `ScheduledReplicaBounds` and `scheduledBounds` fields to `api/autoscaling/v1alpha1/podautoscaler_types.go` with kubebuilder validation markers.
- [ ] 1.2 Regenerate deepcopy, clientset, informer, lister, applyconfiguration, and CRD manifests for the updated PodAutoscaler API.
- [ ] 1.3 Add or update PodAutoscaler API tests covering deepcopy/default serialization behavior for `scheduledBounds`.

## 2. Effective Bounds Resolver

- [ ] 2.1 Add a focused resolver that computes effective min/max bounds from a `PodAutoscaler`, a fixed `time.Time`, and the schedule list.
- [ ] 2.2 Add unit tests for no schedules, matching windows, non-matching windows, partial overrides, timezone handling, duration handling, start/end lifetime handling, and zero minimum behavior.
- [ ] 2.3 Add deterministic error handling for invalid runtime schedule configuration used by controller fallback validation.

## 3. Validation

- [ ] 3.1 Extend validating admission webhook checks for schedule name uniqueness, cron parsing, timezone parsing, duration parsing, lifetime ordering, required override fields, effective min/max validity, and overlapping schedule windows.
- [ ] 3.2 Extend controller-side `validateSpec` fallback validation to reject the same invalid scheduled-bound configurations when the webhook is bypassed.
- [ ] 3.3 Add webhook unit and integration cases for valid schedules, invalid cron/timezone/duration, invalid effective bounds, missing overrides, duplicate names, and overlaps.

## 4. Controller Integration

- [ ] 4.1 Update custom strategy boundary checks in `computeScaleDecision` to use effective min/max bounds instead of static spec bounds.
- [ ] 4.2 Update `createScalingContext` to set effective bounds so KPA/APA algorithm clamping and stabilization use scheduled bounds.
- [ ] 4.3 Update HPA reconciliation so `makeHPA` receives or resolves effective bounds and writes them to the generated HPA spec.
- [ ] 4.4 Add controller tests proving scheduled bounds clamp custom strategy scaling and update generated HPA min/max during and after a matching window.

## 5. Documentation and Verification

- [ ] 5.1 Add sample YAML showing weekday business-hour scheduled bounds.
- [ ] 5.2 Update autoscaling documentation with API semantics, validation rules, overlap behavior, timezone behavior, and HPA compatibility notes.
- [ ] 5.3 Run targeted Go tests for API, webhook, and PodAutoscaler controller packages.
- [ ] 5.4 Run CRD/generated-code verification commands used by this repository.

```

## openspec/changes/support-scheduled-replica-bounds/specs/scheduled-replica-bounds/spec.md

- Source: openspec/changes/support-scheduled-replica-bounds/specs/scheduled-replica-bounds/spec.md
- Lines: 1-112
- SHA256: 641a7176ae8569342c7fed0b29b6370d9ed9342089d2ca037a306b4eef6f0a7b

[TRUNCATED]

```md
## ADDED Requirements

### Requirement: PodAutoscaler declares scheduled replica bounds
The system SHALL allow a `PodAutoscaler` to declare optional scheduled replica bound overrides in `spec.scheduledBounds` using a cron start expression and a positive duration.

#### Scenario: No scheduled bounds preserves static behavior
- **WHEN** a `PodAutoscaler` has no `spec.scheduledBounds`
- **THEN** the controller uses `spec.minReplicas` and `spec.maxReplicas` as the effective replica bounds

#### Scenario: Matching schedule overrides both bounds
- **WHEN** the current time falls within a scheduled bound window with both `minReplicas` and `maxReplicas`
- **THEN** the controller uses the scheduled `minReplicas` and scheduled `maxReplicas` as the effective replica bounds

#### Scenario: Matching schedule overrides one bound
- **WHEN** the current time falls within a scheduled bound window that sets only one of `minReplicas` or `maxReplicas`
- **THEN** the controller overrides only that bound and keeps the other bound from the base `PodAutoscaler` spec

#### Scenario: No matching schedule uses base bounds
- **WHEN** `spec.scheduledBounds` is configured but no schedule window contains the current time
- **THEN** the controller uses the base `spec.minReplicas` and `spec.maxReplicas` as the effective replica bounds

### Requirement: Schedule matching honors timezone and lifetime
The system SHALL evaluate scheduled bounds using each schedule's timezone, cron start expression, positive duration, and optional lifetime boundaries.

#### Scenario: Timezone controls cron window matching
- **WHEN** a schedule sets `timezone`
- **THEN** the controller evaluates the schedule's cron expression and active duration in that timezone

#### Scenario: Missing timezone uses UTC
- **WHEN** a schedule omits `timezone`
- **THEN** the controller evaluates the schedule using UTC

#### Scenario: Schedule before start time is inactive
- **WHEN** the current time is earlier than a schedule's `startTime`
- **THEN** that schedule does not match

#### Scenario: Schedule after end time is inactive
- **WHEN** the current time is later than or equal to a schedule's `endTime`
- **THEN** that schedule does not match

#### Scenario: Cron occurrence duration defines active window
- **WHEN** the current time is greater than or equal to a cron occurrence instant and earlier than that occurrence plus `duration`
- **THEN** that schedule matches for that active window

### Requirement: Scheduled bounds are validated
The system SHALL reject invalid scheduled bound configuration through admission validation and SHALL mark the spec invalid during controller reconciliation if admission validation was bypassed.

#### Scenario: Invalid cron is rejected
- **WHEN** a scheduled bound has an invalid cron expression
- **THEN** admission validation rejects the `PodAutoscaler` with an error for that schedule's `cron` field

#### Scenario: Invalid duration is rejected
- **WHEN** a scheduled bound has a missing or non-positive duration
- **THEN** admission validation rejects the `PodAutoscaler` with an error for that schedule's `duration` field

#### Scenario: Invalid timezone is rejected
- **WHEN** a scheduled bound has an invalid timezone
- **THEN** admission validation rejects the `PodAutoscaler` with an error for that schedule's `timezone` field

#### Scenario: Invalid lifetime is rejected
- **WHEN** a scheduled bound sets both `startTime` and `endTime` and `startTime` is not earlier than `endTime`
- **THEN** admission validation rejects the `PodAutoscaler`

#### Scenario: Missing scheduled override is rejected
- **WHEN** a scheduled bound sets neither `minReplicas` nor `maxReplicas`
- **THEN** admission validation rejects the `PodAutoscaler`

#### Scenario: Invalid effective bounds are rejected
- **WHEN** a scheduled bound would produce effective `minReplicas` greater than effective `maxReplicas`
- **THEN** admission validation rejects the `PodAutoscaler`

#### Scenario: Non-positive effective max is rejected
- **WHEN** a scheduled bound would produce effective `maxReplicas` less than or equal to zero
- **THEN** admission validation rejects the `PodAutoscaler`

#### Scenario: Negative effective min is rejected
- **WHEN** a scheduled bound would produce effective `minReplicas` less than zero
- **THEN** admission validation rejects the `PodAutoscaler`

#### Scenario: Overlapping schedule windows are rejected

```

Full source: openspec/changes/support-scheduled-replica-bounds/specs/scheduled-replica-bounds/spec.md
