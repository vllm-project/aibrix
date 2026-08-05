# Brainstorm Summary

- Change: support-scheduled-replica-bounds
- Date: 2026-08-05

## Confirmed Technical Direction

Add typed `spec.scheduledBounds` API fields, evaluate cron-triggered active windows with a positive duration in a single effective-bounds resolver, and feed those effective bounds into custom strategy boundary checks, scaling context, and generated HPA specs.

The existing dirty documentation change for `docs/source/features/autoscaling/metric-based-autoscaling.rst` has been explicitly included in this change by user choice. It documents existing metric window fields and should be preserved and verified as part of the documentation scope.

## Trade-offs and Risks

- Use `cron + duration` instead of treating cron as a custom continuous window language.
- Use UTC when `timezone` is omitted.
- Reject overlapping active windows instead of adding priority semantics.
- Risk: proving cron-window overlap can be complex; implementation should either use conservative validation or document unsupported ambiguous cases.
- Risk: adding a cron dependency changes `go.mod` and generated artifacts; build tasks must include dependency and generated-code verification.

## Testing Strategy

Resolver unit tests with fixed timestamps, webhook validation tests, controller fallback validation tests, custom PA clamping tests, HPA generation/update tests, generated-code/CRD verification, and docs build or targeted docs checks.

## Spec Patch

No patch currently required. Candidate design is consistent with the current OpenSpec delta spec.
