---
comet_change: support-scheduled-replica-bounds
role: technical-design
canonical_spec: openspec
---

# Scheduled Replica Bounds Technical Design

## Context

`PodAutoscaler` currently exposes static `spec.minReplicas` and `spec.maxReplicas` bounds. Custom strategies use those bounds in `computeScaleDecision` and `createScalingContext`, while the HPA strategy copies them into the generated `HorizontalPodAutoscaler` in `makeHPA`.

Issue #2520 adds scheduled replica bounds so operators can declare planned peak and cost-saving windows in the `PodAutoscaler` resource instead of patching it externally.

This change also keeps the existing documentation update in `docs/source/features/autoscaling/metric-based-autoscaling.rst`, which documents already-present metric window fields. The user explicitly chose to include that dirty worktree change in this Comet change.

## Confirmed Approach

Add typed `spec.scheduledBounds` entries. Each entry defines a named active window using:

- `cron`: recurring start instants
- `duration`: positive active-window length
- `timezone`: optional IANA timezone, defaulting to UTC
- `startTime` and `endTime`: optional schedule lifetime bounds
- `minReplicas` and `maxReplicas`: optional field-level overrides

The first implementation supports a deliberately simple cron subset: fixed minute, hour as a single value/list/range, day-of-month as `*`, month as `*`, and day-of-week as `*` or a single value/list/range using names or numbers. Step expressions, restricted day-of-month/month fields, and other complex forms are rejected.

Use a single resolver, for example `resolveEffectiveReplicaBounds(pa, now)`, as the source of truth for effective bounds. The resolver returns effective min, effective max, the matched schedule name if any, and an error for invalid runtime configuration.

## Key Decisions

Use `cron + duration` rather than treating cron as a custom continuous-window language. Standard cron expressions describe trigger instants, not intervals. With this model, `0 9-18 * * MON-FRI` plus `duration: 1h` expresses hourly business-hour windows without inventing ambiguous cron semantics.

Reject overlapping active windows. This keeps the first API version predictable and avoids adding priority semantics before there is evidence that operators need them.

Keep cron support simple in the first version. This covers business-hour schedules such as `0 9-18 * * MON-FRI` and makes overlap checks bounded by a representative week rather than requiring a full cron satisfiability solver.

Default omitted `timezone` to UTC. This avoids controller-local timezone drift and gives deterministic behavior across clusters.

Preserve partial overrides. A schedule that sets only `minReplicas` changes only the minimum; a schedule that sets only `maxReplicas` changes only the maximum.

## Implementation Shape

API changes belong in `api/autoscaling/v1alpha1/podautoscaler_types.go`, followed by generated deepcopy/client/applyconfiguration/informer/lister and CRD updates.

Validation should be shared conceptually between admission validation and controller fallback validation:

- schedule name is non-empty and unique
- cron parses and fits the supported simple subset
- duration is present and positive
- timezone parses or is omitted
- `startTime < endTime` when both are set
- at least one scheduled bound field is set
- effective min is non-negative
- effective max is positive
- effective min is not greater than effective max
- active windows do not overlap

Controller integration should replace direct static-bound reads at these points:

- boundary checks in `computeScaleDecision`
- min/max values set in `createScalingContext`
- HPA min/max fields in `makeHPA` or its caller

The existing periodic PodAutoscaler enqueue loop is sufficient for correctness. A future optimization can compute `RequeueAfter` for the next schedule boundary, but the first implementation should not depend on a separate scheduler.

## Risks

Cron-window overlap detection becomes complex with arbitrary expressions and durations. The first implementation avoids this by supporting a simple weekly subset and rejecting unsupported complex forms.

Time-sensitive tests can be flaky if they depend on wall clock time. The resolver must accept `now time.Time` and tests should use fixed timestamps and explicit time zones.

Adding a cron parser changes dependencies. Keep the dependency small and maintained, and include `go.mod` / `go.sum` in verification.

HPA does not support `minReplicas: 0` in the same way custom PA can. Preserve the existing behavior that omits HPA `spec.minReplicas` when effective minimum is zero.

## Testing Strategy

Add resolver unit tests for no schedules, matching windows, non-matching windows, partial overrides, timezone handling, duration handling, start/end lifetime handling, overlap handling, and zero minimum behavior.

Add webhook and controller fallback validation tests for invalid cron, invalid timezone, invalid duration, duplicate names, missing overrides, invalid effective bounds, and overlaps.

Add controller tests proving scheduled bounds clamp custom strategy decisions and generated HPA min/max values change during and after matching windows.

Run generated-code and CRD sync checks after API changes. Verify autoscaling docs, including the included metric-window documentation change.

## Spec Patch

No OpenSpec delta spec patch is required. The confirmed design matches the current `scheduled-replica-bounds` delta spec.
