# Console <-> Planner Integration Guide

Developer-facing instruction for how the Console BFF and the planner package
interact in the queue-based architecture.

It complements:

- [ARCHITECTURE.md](./ARCHITECTURE.md) - the big picture
- [CONTRACTS.md](./CONTRACTS.md)   - field-level DTOs

If you are wiring `JobHandler.CreateJob` to the planner, implementing the
worker loop, or reviewing the merged Console read APIs, start here.

## 1. Overview

Today, `apps/console/api/handler/job.go` calls MDS synchronously through
the openai-go SDK. Every Console HTTP request becomes a blocking MDS HTTP
request.

The new design inserts a queue between Console and MDS:

```
[ Console UI ]
      |  HTTP
      v
[ Console BFF (apps/console) ]
      |  Go interface (Planner)
      v
[ pkg/planner: TaskStore + TaskExecutor + ResourceManager
                + BatchClient + Worker ]
      |
      v
[ MDS  /v1/batches  /v1/files ]      [ Resource Manager (parallel team) ]
```

Console is now a BFF that:

1. Owns Console-only fields (display name, created_by, organization, ...).
2. Enqueues a `PlannerJob` and immediately returns a queued view.
3. Exposes a merged job view (`JobView`) that combines planner state and
   MDS state on read.

The planner package owns: task storage, lease-based crash recovery, retry
and backoff, MDS submission, and the read-time MDS overlay that powers
`JobView`. File operations stay on the Console BFF — see §9.

## 2. Boundaries at a glance

| Caller          | Callee            | Interface                | Carries                                            |
| --------------- | ----------------- | ------------------------ | -------------------------------------------------- |
| Console BFF     | Planner           | `Planner`                | `Enqueue` / `GetJob` / `ListJobs` / `CancelJob`    |
| Console / ops   | Planner           | `QueueStatsReader`       | Queue depth + worker activity telemetry            |
| Console / ops   | Planner           | `ResourceCapacityReader` | Aggregated reserved / in-use / free capacity       |
| Planner         | Store             | `TaskStore`              | `Enqueue` / `GetByJobID` / `ListTasksByJobID` / `CancelTask` / `ListSubmittedWithExpiringReservation` / `EnqueueContinuation` |
| Worker          | Store             | `TaskStore`              | `Lease` (FCFS) / `ListCandidates`+`LeaseByID` (custom) / `RenewLease` / `Ack` / `Nack` / `Fail` |
| Worker          | Resource Manager  | `ResourceManager`        | Reserve / release capacity for one task            |
| Worker          | Executor          | `TaskExecutor`           | Build `MDSBatchSubmission` + POST                  |
| Planner         | MDS               | `BatchClient`            | Live MDS overlay (`GetBatch`) + post-submit cancel (`CancelBatch`) |
| Console BFF     | MDS               | direct proxy             | `/v1/files` (unchanged)                            |

## 3. End-to-end CreateJob flow

The main path. Console returns immediately; the worker submits later.

1. **Console BFF accepts `CreateJob`.**
   - Validates the proto request.
   - Generates the `JobID`.
   - Persists the Console overlay row (`store.UpsertJob` with display name,
     created_by, ...).
2. **Console builds a `PlannerJob`.**
   - `BatchPayload` (input_file_id, endpoint, completion_window, metadata).
   - `ResourceRequirement` (resource_type, accelerator type/count) from the
     wizard.
   - `ModelTemplate` from the wizard selection.
   - `IdempotencyKey` (for example `console:create-job:<job_id>`),
     `Source="console"`, `SubmittedBy=user.email`, `SubmittedAt=now()`.
3. **Console calls `Planner.Enqueue`.**
   - `EnqueueResult{TaskID, JobID, State: queued, EnqueuedAt}` returned
     immediately.
   - Failure mapping at the gRPC boundary:
     - `ErrInvalidJob` -> `codes.InvalidArgument`
     - `ErrDuplicateEnqueue` -> `codes.AlreadyExists` (idempotent retry)
     - `ErrStoreFull` -> `codes.ResourceExhausted`
     - `ErrStoreUnavailable` -> `codes.Unavailable`
4. **Console responds.**
   - Returns a `*pb.Job` populated from the overlay + the queued task.
5. **Worker leases.** (background loop)
   - `TaskStore.Lease` returns one or more `LeasedTask`s with a lease bound
     to the worker.
   - State: `queued -> leased`.
6. **Worker calls `TaskExecutor.Execute`.**
   - The executor builds the MDS submission body from the task. The
     `extra_body.aibrix` block carries `job_id` and `model_template`.
   - Pre-submit dedup: the executor SHOULD `BatchClient.GetBatch` keyed on
     `extra_body.aibrix.job_id` first and short-circuit if a batch already
     exists.
   - Returns `(batchID, err)`.
7. **Worker acks the store.**
   - On success: `TaskStore.Ack` with the batch ID. State: `leased ->
     submitted`.
   - On retryable error: `TaskStore.Nack` with `RetryAt`. State: `leased ->
     retryable_failure -> queued` (after RetryAt).
   - On terminal error: `TaskStore.Fail`. State: `leased ->
     terminal_failure`.
8. **Read-time MDS overlay** (no reconcile goroutine in MVP)
   - `Planner.GetJob` / `ListJobs` reads the `PlannerTask` from `TaskStore`
     and, when `BatchID` is set, calls `BatchClient.GetBatch(BatchID)` and
     overlays the result into `JobView`.
   - The MVP store does not mirror MDS state. A reconcile-cache layer can
     be added later if `ListJobs` becomes a hot path.

## 4. ListJobs / GetJob (the merged read)

The UI must see queued, in-flight, and finished jobs in one list.

`Planner.GetJob(ctx, jobID)` returns a `JobView`. The UI renders
`LifecycleState` (typed enum); `BatchStatus` carries the raw MDS string
for forward compatibility.

When no `BatchID` is set:

| `PlannerState`        | `LifecycleState`  |
| --------------------- | ----------------- |
| `queued`              | `queued`          |
| `leased`              | `dispatching`     |
| `submitted`           | `submitted`       |
| `retryable_failure`   | `queued`          |
| `terminal_failure`    | `failed`          |
| `cancelled`           | `cancelled`       |

When `BatchID` is set, the MDS status string maps 1:1 to
`LifecycleState` (`validating`, `in_progress`, `finalizing`,
`completed`, `failed`, `expired`, `cancelling`, `cancelled`).

The Console BFF then layers on its overlay (display name, created_by) when
populating `*pb.Job` for the UI.

`ListJobs` is paginated by an opaque `after` cursor and supports
`SubmittedBy` filtering. Add new filter fields when there is UI surface
that consumes them.

## 5. CancelJob

`Planner.CancelJob` covers both pre-submit and post-submit cancellation.
The planner routes internally:

| Task state                       | What CancelJob does                                                                          |
| -------------------------------- | -------------------------------------------------------------------------------------------- |
| `queued`, `leased`, `retryable_failure` | `TaskStore.CancelTask`; any holding worker discovers the cancel via `ErrLeaseLost`.   |
| `submitted` (BatchID set)        | `BatchClient.CancelBatch(batchID)`, then `TaskStore.CancelTask`; subsequent `GetJob` reads overlay live MDS state. |
| terminal states                  | `TaskStore.CancelTask` is a no-op success (idempotent).                                      |

Console does not call `BatchClient` directly. The planner owns the routing.

## 6. Worker loop (reference outline)

The planner worker is a long-running goroutine. One conceptual tick:

```text
for ctx.Err() == nil {
    // Two paths, picked by whether a PickFunc is configured:
    //
    //   pick == nil  -> FCFS via store.Lease (the convenience path).
    //   pick != nil  -> custom ranking; the function returns task IDs,
    //                   the store atomically leases them.
    //
    // Switching policies is one line: pick = mySchedulingPolicy, where
    // mySchedulingPolicy is any PickFunc.
    var leased []*LeasedTask
    if w.pick == nil {
        leased, _ = store.Lease(ctx, &LeaseRequest{
            WorkerID: workerID, Limit: batchSize, LeaseTTL: 30 * time.Second,
        })
    } else {
        ids, _ := w.pick(ctx, store, &PickRequest{
            WorkerID: workerID, Limit: batchSize, Now: time.Now(),
        })
        if len(ids) == 0 { time.Sleep(pollInterval); continue }
        leased, _ = store.LeaseByID(ctx, &LeaseByIDRequest{
            WorkerID: workerID, TaskIDs: ids, LeaseTTL: 30 * time.Second,
        })
    }
    if len(leased) == 0 {
        time.Sleep(pollInterval); continue
    }

    for _, item := range leased {
        hbCtx, hbCancel := context.WithCancel(ctx)
        go heartbeat(hbCtx, store, item.Lease, ttl/3)

        // 1. Reserve capacity. Reserve is idempotent per TaskID, so a
        //    re-leasing worker (after a prior worker crashed mid-tick)
        //    gets the same Reservation back rather than a new slot.
        reservation, err := rm.Reserve(ctx, &ReserveRequest{
            JobID: item.Task.JobID, TaskID: item.Task.TaskID,
            ResourceRequirement: item.Task.ResourceRequirement,
            RequestedBy: workerID,
        })
        if errors.Is(err, ErrInsufficientResources) ||
           errors.Is(err, ErrResourceManagerUnavailable) {
            store.Nack(ctx, &NackRequest{
                Lease: item.Lease, RetryAt: backoff.Next(item.Task.Attempts),
                LastError: err.Error(),
            })
            hbCancel()
            continue
        }

        // 2. Project the reservation into extra_body.aibrix.planner_decision
        //    via item.Task before Execute reads it.
        item.Task.ReservationID = reservation.ReservationID
        item.Task.ReservationExpiresAt = reservation.ExpiresAt

        // 3. Submit.
        batchID, err := executor.Execute(ctx, item.Task)
        hbCancel()

        switch {
        case err == nil:
            // The reservation is intentionally NOT released on Ack:
            // it must outlive submit so the reservation-expiry sweeper
            // can detect an expired reservation and create a
            // continuation task. Release happens on Fail/Cancel/
            // observed MDS-terminal state. The sweeper itself does
            // not call Release; RM-side expiry handles slot reclaim.
            store.Ack(ctx, &AckRequest{
                Lease: item.Lease, BatchID: batchID, SubmittedAt: time.Now(),
                ReservationID:        reservation.ReservationID,
                ReservationExpiresAt: reservation.ExpiresAt,
            })
        case errors.Is(err, ErrMDSSubmitFailed) && retryable(err):
            store.Nack(ctx, &NackRequest{
                Lease: item.Lease, RetryAt: backoff.Next(item.Task.Attempts),
                LastError: err.Error(),
            })
            rm.Release(ctx, &ReleaseRequest{
                ReservationID: reservation.ReservationID, Reason: "retry",
            })
        default:
            store.Fail(ctx, &FailRequest{
                Lease: item.Lease, LastError: err.Error(),
            })
            rm.Release(ctx, &ReleaseRequest{
                ReservationID: reservation.ReservationID, Reason: "failed",
            })
        }
    }
}
```

`Release` errors (ErrReservationNotFound / ErrReservationExpired) are
treated as no-ops; the reservation already returned to the pool.

`Worker.Run` wraps this with shutdown handling. `Worker.ProcessAvailable`
exposes one tick for tests with controlled time.

The heartbeat goroutine is mandatory: lease expiry is the only crash
recovery primitive, but during a synchronous `Execute` there is nothing
to renew it from. Renewing every `TTL/3` keeps the lease alive while the
MDS call is in flight; the heartbeat is cancelled when `Execute` returns.

`Execute` must be effectively idempotent against MDS — typically by
keying off `extra_body.aibrix.job_id` so a duplicate submit is detectable.
See §8.

## 7. Read-time MDS overlay

MVP does not run a reconcile goroutine. `JobView` is assembled live:

1. `Planner.GetJob(ctx, jobID)` reads the `PlannerTask` via
   `TaskStore.GetByJobID`.
2. If the task has a `BatchID` and is not in a planner-terminal state,
   the Planner calls `BatchClient.GetBatch(BatchID)` and overlays the
   `BatchStatus` fields (`InProgressAt`, `FinalizingAt`, `RequestCounts`,
   `Errors`, `Usage`, ...) into `JobView`.
3. `JobView.LifecycleState` is derived from `PlannerTaskState` (no
   `BatchID`) or from `BatchStatus.Status` (with `BatchID`).

`ListJobs` follows the same pattern; for the expected MVP scale
(single-digit K active jobs) the per-page `BatchClient.GetBatch` cost is
acceptable. A periodic reconcile that mirrors `BatchStatus` into the
store is a deferred optimization.

`BatchClient.ListBatches` is not in the MVP surface; add it back when a
catastrophic recovery path needs to walk MDS to rebuild the planner store.
When that happens, the lookup key is `extra_body.aibrix.job_id`.

## 8. Crash safety and duplicate submit

Three failure modes can cause one task to be submitted to MDS twice:

1. Worker dies after MDS returns 200 but before `Ack`.
2. `Execute` runs longer than the lease TTL.
3. Network partition swallows the MDS response.

The MVP defenses, in order:

1. **Pre-submit dedup check inside `Execute`.** Call `BatchClient.GetBatch`
   (or, eventually, a `ListBatches` filtered by `aibrix.job_id`) before
   submitting. If a batch already exists for the same `job_id`, return its
   batch ID without resubmitting.
2. **Lease heartbeat.** `Worker` runs a goroutine that calls
   `TaskStore.RenewLease` every `TTL/3` while `Execute` is in flight.
3. **MDS-side dedup (target).** When MDS treats `extra_body.aibrix.job_id`
   as a uniqueness key, retries become safe by construction and (1)
   becomes a fast-path optimization. Until then, (1) is required.

## 9. Files

File operations remain on the Console BFF as a direct passthrough:

- `POST /api/v1/files/upload`            -> `POST {mds}/v1/files`
- `GET  /api/v1/files`                   -> `GET  {mds}/v1/files`
- `GET  /api/v1/files/{file_id}`         -> `GET  {mds}/v1/files/{file_id}`
- `GET  /api/v1/files/{file_id}/content` -> the same on MDS

The planner only consumes `input_file_id` strings and surfaces
`output_file_id`/`error_file_id` strings in `BatchStatus` and `JobView`.
Workers MUST NOT upload file content before submit.

## 10. Identity and correlation keys

| Key                       | Owner   | Used for                                              |
| ------------------------- | ------- | ----------------------------------------------------- |
| `JobID`                   | Console | User-facing identity, primary correlation key         |
| `TaskID`                  | Planner | Lease ownership, retry chain                          |
| `IdempotencyKey`          | Console | Duplicate-enqueue detection in the store              |
| `BatchID`                 | MDS     | MDS batch identity, file IDs                          |
| `extra_body.aibrix.job_id`| Planner | MDS round trip (planner ↔ MDS correlation)            |

`IdempotencyKey` rides into `PlannerTask` so the store can enforce
`ErrDuplicateEnqueue` end to end.

## 11. Error code translation

| Planner sentinel       | gRPC code                | UI behavior                                  |
| ---------------------- | ------------------------ | -------------------------------------------- |
| `ErrInvalidJob`        | `InvalidArgument`        | Show validation message                      |
| `ErrJobNotFound`       | `NotFound`               | UI 404                                       |
| `ErrDuplicateEnqueue`  | `AlreadyExists`          | Treat as success on retry                    |
| `ErrStoreFull`         | `ResourceExhausted`      | "System busy, retry shortly"                 |
| `ErrStoreUnavailable`  | `Unavailable`            | Backend down banner                          |
| `ErrLeaseLost`         | (worker-internal)        | Worker drops in-flight work; not user-facing |
| `ErrMDSSubmitFailed`   | (chain underlying status)| Reuse `mapSDKError` shape                    |

## 12. MDS dependency

The planner depends on MDS persisting and echoing
`extra_body.aibrix.job_id`. See
[ARCHITECTURE.md "MDS correlation and dedup"](./ARCHITECTURE.md#mds-correlation-and-dedup)
for the full requirement and the impact while the change is pending.

## 13. Migration plan

See [ARCHITECTURE.md "Expected next PRs"](./ARCHITECTURE.md#expected-next-prs)
for the canonical roadmap. The cutover step (`JobHandler` switch from
MDS to `Planner`) must preserve the existing `*pb.Job` shape so the UI
sees no breaking change.

## 14. References

- `apps/console/api/handler/job.go` - current synchronous BFF
- `apps/console/api/handler/file.go` - file proxy (unchanged)
- `python/aibrix/aibrix/metadata/api/v1/batch.py` - MDS batch endpoints
- `pkg/planner/interfaces.go` - Go interface signatures
- `pkg/planner/contracts.go` - request/response DTOs
- `pkg/planner/types.go` - `PlannerJob` / `PlannerTask` / state
- `pkg/planner/CONTRACTS.md` - field-level wire docs
- `pkg/planner/ARCHITECTURE.md` - design rationale
