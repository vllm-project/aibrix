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

#### Scenario: Unsupported complex cron is rejected
- **WHEN** a scheduled bound uses cron syntax outside the supported simple subset
- **THEN** admission validation rejects the `PodAutoscaler` with an error for that schedule's `cron` field

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
- **WHEN** two scheduled bounds can have active windows for the same instant
- **THEN** admission validation rejects the `PodAutoscaler` instead of relying on implicit priority

### Requirement: Custom PodAutoscaler strategies use effective bounds
The system SHALL apply effective replica bounds to custom PodAutoscaler strategies.

#### Scenario: Boundary check uses scheduled maximum
- **WHEN** a custom-strategy `PodAutoscaler` has current replicas above the effective maximum from a matching schedule window
- **THEN** reconciliation scales the target down to the effective maximum

#### Scenario: Boundary check uses scheduled minimum
- **WHEN** a custom-strategy `PodAutoscaler` has current replicas below the effective minimum from a matching schedule window
- **THEN** reconciliation scales the target up to the effective minimum

#### Scenario: Algorithm recommendation is clamped by effective bounds
- **WHEN** a custom strategy computes a desired replica count outside the effective bounds
- **THEN** the final desired replica count is clamped to the effective bounds

### Requirement: HPA strategy uses effective bounds
The system SHALL reconcile generated Kubernetes `HorizontalPodAutoscaler` resources with effective replica bounds.

#### Scenario: Generated HPA uses matching scheduled bounds
- **WHEN** an HPA-strategy `PodAutoscaler` has a matching scheduled bound window
- **THEN** the generated HPA `spec.minReplicas` and `spec.maxReplicas` reflect the effective bounds

#### Scenario: Generated HPA returns to base bounds
- **WHEN** a previously matching schedule no longer matches
- **THEN** the generated HPA is updated back to the base `PodAutoscaler` bounds

#### Scenario: Effective zero minimum preserves HPA compatibility
- **WHEN** the effective minimum is zero for an HPA-strategy `PodAutoscaler`
- **THEN** the generated HPA omits `spec.minReplicas` according to the existing HPA compatibility behavior
