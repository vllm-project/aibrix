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

`PodAutoscalerSpec` gains `ScheduledBounds []ScheduledReplicaBounds`. `cron` defines recurring start instants, `duration` defines how long each occurrence remains active, and optional `startTime` and `endTime` constrain the schedule lifetime. This avoids treating a standard cron expression as a custom window language. The issue example `0 9-18 * * MON-FRI` can represent hourly one-hour windows by setting `duration: 1h`.

The first implementation deliberately supports a simple cron subset only: fixed minute, hour as a single value/list/range, day-of-month as `*`, month as `*`, and day-of-week as `*` or a single value/list/range using names or numbers. Step expressions, restricted day-of-month/month fields, and other complex cron forms are rejected. This keeps overlap validation deterministic and bounded while covering the business-hour use case.

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

Admission validation should reject overlapping schedule windows within their active lifetime. Because the initial cron subset is weekly and bounded, overlap detection can enumerate a representative week of occurrence windows instead of scanning arbitrary cron streams. Runtime resolution should still be deterministic and pick the first matching entry if validation was bypassed, but controller fallback validation should mark the spec invalid before scaling proceeds.

Alternatives considered:

- First match wins as the documented API: rejected for the first version because schedule ordering in a Kubernetes spec is easy to misunderstand and hard to audit.
- Highest explicit priority wins: rejected because it adds another API field before there is a proven need.

### Apply effective bounds consistently

Custom strategies should use effective bounds in all existing places that currently read `pa.Spec.MinReplicas` or `pa.Spec.MaxReplicas` for boundary checks, algorithm clamping, and scaling context. HPA strategy should generate HPA `spec.minReplicas` and `spec.maxReplicas` from the same effective bounds.

The existing periodic enqueue loop already reconciles all `PodAutoscaler` objects every `DefaultResyncInterval` (10 seconds), which is sufficient for schedule transitions in the initial design. Implementation can add a targeted `RequeueAfter` to the next known schedule boundary if that falls out naturally, but correctness must not depend on a separate scheduler.

### Validation responsibilities

The validating webhook and controller fallback validation should check:

- schedule names are non-empty and unique within one `PodAutoscaler`;
- cron values parse successfully and fit the supported simple cron subset;
- duration and timezone values parse successfully;
- `startTime < endTime` when both are set;
- `duration > 0`;
- at least one of scheduled `minReplicas` or `maxReplicas` is set;
- effective `minReplicas <= maxReplicas` for each schedule;
- effective `minReplicas >= 0`;
- effective `maxReplicas > 0`;
- schedule windows do not overlap.

Cron/timezone/duration validation errors should be surfaced with field paths that point to the offending `scheduledBounds[index]` entry.

## Risks / Trade-offs

- [Risk] Cron window overlap detection can be complex for arbitrary cron expressions plus durations. -> Mitigation: initially support only a simple weekly cron subset and reject unsupported complex forms.
- [Risk] Time-sensitive tests can become flaky. -> Mitigation: make the resolver accept `now time.Time` and test it with fixed timestamps and explicit time zones.
- [Risk] Generated client and CRD artifacts are easy to forget. -> Mitigation: include explicit tasks for deepcopy, client/applyconfiguration, CRD generation, and CRD sync verification.
- [Risk] HPA minReplicas cannot be zero in the same way custom PA can express scale-to-zero. -> Mitigation: preserve the existing `makeHPA` behavior that omits HPA `minReplicas` when the effective minimum is zero.

## Migration Plan

Existing `PodAutoscaler` resources do not need migration because `scheduledBounds` is optional. Rollback is removing the optional field from resources before downgrading to a controller/CRD version that does not know the field.

Implementation should regenerate manifests and clients in the same change so API, CRD, and generated code stay in sync.

## Open Questions

- Whether maintainers want broader cron support later. The initial implementation intentionally keeps cron simple for predictable validation.
- Should status expose the current matched schedule and effective bounds? This design keeps it out of the initial scope unless implementation review asks for operator visibility.
