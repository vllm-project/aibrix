# pkg/planner

The AIBrix scheduling core.

The target architecture is asynchronous and queue-based:

1. Console accepts `CreateJob`
2. Console enqueues a `PlannerJob`
3. Planner workers periodically lease queued tasks
4. Workers submit them to MDS
5. Console reads a merged planner + MDS view (`JobView`); the planner
   overlays a live `BatchClient.GetBatch` call at read time

This package defines the boundaries; concrete implementations land in
follow-up PRs.

For the design itself, see [ARCHITECTURE.md](./ARCHITECTURE.md).
For field-level request/response payloads, see [CONTRACTS.md](./CONTRACTS.md).

## Boundaries

The MVP surface is eight interfaces, one per audience:

| Interface                | Audience                       | What it does                                       |
| ------------------------ | ------------------------------ | -------------------------------------------------- |
| `Planner`                | Console BFF                    | `Enqueue` / `GetJob` / `ListJobs` / `CancelJob`    |
| `QueueStatsReader`       | Console / ops dashboards       | Queue depth + worker activity telemetry            |
| `ResourceCapacityReader` | Console / ops dashboards       | Aggregated reserved / in-use / free capacity       |
| `TaskStore`              | Planner internals              | Durable state + lease coordination                 |
| `TaskExecutor`           | Planner worker                 | One MDS `POST /v1/batches`                         |
| `ResourceManager`        | Planner worker                 | Reserve / release capacity for one task            |
| `BatchClient`            | Planner (read overlay + cancel)| MDS read + cancel                                  |
| `Worker`                 | Planner process                | Long-running orchestration loop                    |

`TaskStore` exposes both an FCFS convenience path (`Lease`) and a
policy-aware path (`ListCandidates` + `LeaseByID`). Custom scheduling
policies are pluggable as `PickFunc` values; concrete implementations
land alongside their consumers in follow-ups. A `nil` `PickFunc` falls
through to the store-baked FCFS path.

## State

The package carries two state families:

- `PlannerTaskState` is the planner-internal coordination state stored in
  `TaskStore`: `queued`, `leased`, `submitted`, `retryable_failure`,
  `terminal_failure`, `cancelled`, `superseded`. Each `PlannerTask` is one
  attempt at running a job; reservation-expiry retries create a new task
  (same `JobID`, fresh `TaskID`, `Attempt+1`) and transition the prior
  task to `superseded` rather than mutating it in place.
- `JobLifecycleState` is the typed user-facing state the Console UI renders:
  `queued`, `dispatching`, `submitted`, `created`, `validating`,
  `in_progress`, `finalizing`, `completed`, `failed`, `expired`,
  `cancelling`, `cancelled`.

`JobLifecycleState` is **derived**, not stored. The Planner implementation
computes it at read time from `PlannerTaskState` (when no `BatchID` is set)
or from the MDS `BatchStatus.Status` string (when `BatchID` is set), and
returns the result in `JobView.LifecycleState`. The raw MDS status is
preserved in `JobView.BatchStatus` for forward compatibility.

## Console wiring

```
Console
  └── Planner.Enqueue          (returns immediately; worker submits later)
      Planner.GetJob           (merged JobView)
      Planner.ListJobs         (paginated JobView list)
      Planner.CancelJob        (pre- or post-submit, planner routes)

Worker
  ├── TaskStore.Lease           (acquire N tasks with a lease TTL)
  │   (or PickFunc + TaskStore.LeaseByID for custom policies)
  ├── ResourceManager.Reserve   (idempotent per TaskID)
  ├── TaskStore.RenewLease      (heartbeat goroutine while in flight)
  ├── TaskExecutor.Execute      (translate task → MDS POST /v1/batches)
  ├── TaskStore.Ack/Nack/Fail   (record outcome; Ack does NOT release the reservation)
  └── ResourceManager.Release   (called on Fail/Cancel; not on Ack)

Planner.CancelJob
  ├── (post-submit only) BatchClient.CancelBatch
  └── TaskStore.CancelTask      (transition to cancelled)

Planner.GetJob / ListJobs
  ├── BatchClient.GetBatch      (live MDS overlay assembled into JobView)
  └── TaskStore.ListTasksByJobID (optional: surface the attempt history)

Planner reservation-expiry sweeper (background)
  ├── TaskStore.ListSubmittedWithExpiringReservation
  └── TaskStore.EnqueueContinuation (atomic: prior task -> superseded;
                                     new task inserted with Attempt+1, queued)
```

When a reservation expires, the RM tears down the underlying resources
and MDS marks its batch failed/expired on its own. The planner does
not call `BatchClient.CancelBatch` or `ResourceManager.Release` from
the sweeper - it just inserts the continuation and moves on.

In MVP, `JobView` is assembled live: `Planner.GetJob` reads the
`PlannerTask` from `TaskStore` and (when `BatchID` is set) overlays a
fresh `BatchClient.GetBatch` call. A reconcile-cache layer that mirrors
`BatchStatus` into the store is intentionally not part of the MVP
surface; it can be added later if `ListJobs` becomes a hot path.

## Files

The planner does not own file upload/download. See
[CONSOLE_INTEGRATION.md §9](./CONSOLE_INTEGRATION.md) for details.

## Hard external dependency

The planner depends on MDS persisting and echoing `extra_body.aibrix.job_id`.
See [ARCHITECTURE.md "MDS correlation and dedup"](./ARCHITECTURE.md#mds-correlation-and-dedup)
for the full requirement and what breaks until it ships.

## Not in this PR

This PR defines the contract; concrete `TaskStore` / `TaskExecutor` /
`Worker` / `Planner` implementations and the MDS-side schema work are
separate. See [ARCHITECTURE.md "Expected next PRs"](./ARCHITECTURE.md#expected-next-prs)
for the canonical roadmap.
