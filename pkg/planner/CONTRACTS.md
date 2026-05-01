# Planner API Contracts

Field-level request/response payloads between AIBrix components in the
queue-based planner architecture.

The MVP surface is small on purpose: every method has a clearly named DTO,
every method that returns no body says so explicitly, and concepts that
will be added with future components (RM client, scheduler, profile
registry, queue telemetry) are intentionally absent.

JSON examples below use RFC3339 timestamps and Go `time.Duration` strings
(such as `"30s"`). In Go, the corresponding fields are `time.Time` for
required timestamps, `*time.Time` for optional/nullable timestamps, and
`time.Duration` for durations.

## Contract Summary

```text
Console -> Planner
  EnqueueRequest         -> EnqueueResult
  GetJob(jobID)          -> JobView
  ListJobsRequest        -> ListJobsResponse
  CancelJobRequest       -> CancelJobResponse

Console / ops -> Planner telemetry
  GetQueueStatsRequest   -> QueueStatsView
  GetCapacityRequest     -> CapacityView

Worker -> TaskStore
  Enqueue(*PlannerTask)        -> no body         (always Attempt = 1)
  LeaseRequest                 -> []*LeasedTask   (FCFS convenience)
  ListCandidatesRequest        -> []*PlannerTask  (read-only, for PickFunc)
  LeaseByIDRequest             -> []*LeasedTask   (atomic by ID)
  RenewLease(...)              -> TaskLease
  AckRequest                   -> no body
  NackRequest                  -> no body
  FailRequest                  -> no body
  GetByJobID(jobID)            -> *PlannerTask    (latest attempt)
  GetByTaskID(taskID)          -> *PlannerTask

Planner -> TaskStore
  CancelTaskRequest                              -> no body
  ListSubmittedWithExpiringReservation(before,limit) -> []*PlannerTask
  EnqueueContinuationRequest                     -> no body
  ListTasksByJobID(jobID)                        -> []*PlannerTask  (full chain)

Worker -> PickFunc (scheduling policy)
  PickRequest            -> []taskID

Worker -> Executor
  Execute(*PlannerTask, *Reservation)  -> batchID

Worker -> ResourceManager
  ReserveRequest         -> *Reservation
  ReleaseRequest         -> no body

Planner -> MDS
  GetBatch(batchID)             -> *BatchStatus
  CancelBatch(batchID)          -> *BatchStatus
  ListBatchesRequest            -> *ListBatchesResponse
```

## 1. Console -> Planner

Interface: `Planner`.

### EnqueueRequest

```json
{
  "job": {
    "job_id": "job_123",
    "source": "console",
    "submitted_by": "alice@example.com",
    "submitted_at": "2026-05-01T10:00:00Z",
    "idempotency_key": "console:create-job:job_123",
    "max_attempts": 3,
    "priority": 0,
    "resource_requirement": {
      "resource_type": "spot",
      "accelerator": { "type": "H100-SXM", "count": 4 }
    },
    "model_template": { "name": "llama3-batch", "version": "v1" },
    "profile":        { "name": "default",      "version": "v1" },
    "batch_payload": {
      "input_file_id": "file_abc",
      "endpoint": "/v1/chat/completions",
      "completion_window": "24h",
      "metadata": { "display_name": "my batch" }
    }
  }
}
```

Required fields: `job_id`, `resource_requirement.resource_type`,
`resource_requirement.accelerator.type`,
`resource_requirement.accelerator.count`, `batch_payload.input_file_id`,
`batch_payload.endpoint`.

Recommended fields: `source`, `submitted_by`, `submitted_at`,
`idempotency_key`, `max_attempts`, `model_template`, `profile`.

Propagation rules:

- `job.model_template` is the single source of truth for
  `extra_body.aibrix.model_template` when the executor builds the MDS
  submission payload.
- `job.profile` is the single source of truth for
  `extra_body.aibrix.profile`.
- `job.max_attempts == 0` means "use the worker's default retry budget".
- `job.idempotency_key` must be carried into the durable `PlannerTask` so
  the store can enforce `ErrDuplicateEnqueue`.
- `job.priority` is copied into `PlannerTask.Priority` for use by score-
  based `PickFunc` implementations. Zero is the default and is ignored
  by FCFS policies.

### EnqueueResult

```json
{
  "task_id": "task_456",
  "job_id": "job_123",
  "state": "queued",
  "enqueued_at": "2026-05-01T10:00:00Z"
}
```

Typical errors: `ErrInvalidJob`, `ErrDuplicateEnqueue`, `ErrStoreFull`,
`ErrStoreUnavailable`.

### GetJob / ListJobs

`GetJob(ctx, jobID)` returns one `JobView`. Returns `ErrJobNotFound` if
the JobID is unknown.

`ListJobsRequest`:

```json
{
  "limit": 20,
  "after": "task_prev",
  "submitted_by": "alice@example.com"
}
```

`ListJobsResponse`:

```json
{
  "data": [ /* JobView[] */ ],
  "has_more": false,
  "next_after": ""
}
```

### CancelJobRequest / Response

```json
{
  "job_id": "job_123",
  "reason": "user requested cancel",
  "requested_by": "alice@example.com"
}
```

```json
{
  "job_id": "job_123",
  "state": "cancelled",
  "batch_id": "batch_xyz",
  "accepted_at": "2026-05-01T10:02:00Z"
}
```

The planner routes the cancel internally:

- pre-submit (`PlannerState ∈ {queued, leased, retryable_failure}`):
  call `TaskStore.CancelTask`; any worker holding the lease discovers
  the cancel on its next `RenewLease`/`Ack`/`Nack`/`Fail` call via
  `ErrLeaseLost`.
- post-submit (`PlannerState == submitted`, `BatchID` set):
  call `BatchClient.CancelBatch(batchID)`, then `TaskStore.CancelTask`.
  Subsequent `GetJob` reads overlay live MDS state (no reconcile loop in
  MVP).

## 2. TaskStore

Interface: `TaskStore`. Called by both the Worker (lease lifecycle:
`Lease` / `LeaseByID` / `RenewLease` / `Ack` / `Nack` / `Fail`) and
the Planner (`Enqueue` / `GetByJobID` / `GetByTaskID` / `CancelTask`
/ `ListSubmittedWithExpiringReservation` / `EnqueueContinuation` /
`ListTasksByJobID`).

Each `PlannerTask` is one attempt at running a job. The first attempt
goes through `Enqueue`; subsequent attempts (after a reservation
expiry) go through `EnqueueContinuation`, which atomically supersedes
the prior attempt and inserts a new task with `Attempt + 1`. State
transitions on a single PlannerTask are forward-only.

### LeaseRequest

```json
{
  "worker_id": "planner-worker-0",
  "limit": 10,
  "lease_ttl": "30s"
}
```

### Lease response (`[]*LeasedTask`)

```json
[
  {
    "task": {
      "task_id": "task_456",
      "job_id": "job_123",
      "idempotency_key": "console:create-job:job_123",
      "state": "leased",
      "resource_requirement": {
        "resource_type": "spot",
        "accelerator": { "type": "H100-SXM", "count": 4 }
      },
      "model_template": { "name": "llama3-batch", "version": "v1" },
      "payload": {
        "input_file_id": "file_abc",
        "endpoint": "/v1/chat/completions",
        "completion_window": "24h"
      },
      "attempts": 0,
      "max_attempts": 3,
      "lease_owner": "planner-worker-0",
      "lease_expires_at": "2026-05-01T10:00:35Z",
      "enqueued_at": "2026-05-01T10:00:00Z",
      "available_at": "2026-05-01T10:00:00Z"
    },
    "lease": {
      "task_id": "task_456",
      "worker_id": "planner-worker-0",
      "lease_expires_at": "2026-05-01T10:00:35Z"
    }
  }
]
```

### Policy-aware lease (used by `PickFunc`)

Custom scheduling policies use a two-stage pickup that splits ranking
from leasing. The store exposes both halves; the policy ranks; the
store leases.

`ListCandidatesRequest`:

```json
{
  "limit": 40,
  "now":   "2026-05-01T10:00:00Z"
}
```

Returns up to `limit` `PlannerTask`s that are currently leaseable
(`state IN ('queued','retryable_failure')`, no live lease,
`available_at <= now`). The store does NOT acquire leases; the
policy is free to inspect, rank, and discard candidates.

`LeaseByIDRequest`:

```json
{
  "worker_id": "planner-worker-0",
  "task_ids":  ["task_456", "task_457", "task_458"],
  "lease_ttl": "30s"
}
```

Atomically acquires leases on exactly the listed task IDs. Tasks that
are no longer leaseable (already leased, became terminal, missing) are
silently skipped; the response carries the subset that was successfully
leased. The worker treats `len(returned) < len(requested)` as a normal
race outcome and operates on what it got.

The shape of the response is the same `[]*LeasedTask` as `Lease`.

### RenewLease

`RenewLease(ctx, lease, extendBy)` returns the renewed `TaskLease`.
Returns `ErrLeaseLost` if the existing lease is no longer valid.

### Ack / Nack / Fail

```json
// AckRequest
{
  "lease": { "task_id": "task_456", "worker_id": "planner-worker-0",
             "lease_expires_at": "2026-05-01T10:00:35Z" },
  "batch_id": "batch_xyz",
  "submitted_at": "2026-05-01T10:00:07Z",
  "reservation_id": "res_789",
  "reservation_expires_at": "2026-05-02T10:00:00Z"
}

// NackRequest
{
  "lease": { /* same shape */ },
  "retry_at": "2026-05-01T10:01:07Z",
  "last_error": "mds timeout"
}

// FailRequest
{
  "lease": { /* same shape */ },
  "last_error": "invalid model_template"
}
```

All three return no body on success. All three return `ErrLeaseLost` if
the store no longer recognizes the lease.

### CancelTaskRequest

```json
{
  "task_id":      "task_abc",
  "reason":       "user requested cancel",
  "requested_by": "alice@example.com",
  "cancelled_at": "2026-05-01T10:02:00Z"
}
```

Returns no body on success. Returns `ErrJobNotFound` if the TaskID does
not exist. Idempotent: cancelling a task already in `cancelled` or any
terminal state is a no-op success. CancelTask carries no `TaskLease`
because `Planner.CancelJob` does not own one; any worker holding the
lease discovers the cancel on its next `RenewLease`/`Ack`/`Nack`/`Fail`
call via `ErrLeaseLost`.

For post-submit cancels (`BatchID` set), `Planner.CancelJob` also calls
`BatchClient.CancelBatch` separately; `CancelTask` only records the
planner-side state transition.

### EnqueueContinuationRequest

```json
{
  "superseded_task_id": "task_456",
  "new_task": {
    "task_id": "task_789",
    "job_id":  "job_123",
    "idempotency_key": "",
    "state": "queued",
    "resource_requirement": { "...": "..." },
    "payload":              { "...": "..." }
  },
  "reason":         "reservation_expired",
  "superseded_at":  "2026-05-01T10:05:00Z"
}
```

Returns no body on success.

Atomic effect:

- the task at `superseded_task_id` transitions
  `submitted -> superseded` (its `BatchID` is preserved for audit);
- `new_task` is inserted with `Attempt = supersededTask.Attempt + 1`,
  state `queued`, and the same `JobID` as the prior attempt. The store
  derives `Attempt` from the prior task; any value the caller sets in
  `new_task.attempt` is overwritten.

The new task starts with empty `BatchID` and empty `ReservationID`;
the next worker to lease it calls `Reserve` to obtain a fresh
reservation. RM idempotency is keyed on `TaskID`, so the new
`task_id` guarantees a fresh reservation rather than reusing the
prior attempt's slot.

`EnqueueContinuation` carries no `TaskLease` (the prior task is no
longer leased after `Ack`). The companion `BatchClient.CancelBatch`
call against MDS is the caller's responsibility; this method only
records the planner-side state transition.

Errors:

- `ErrJobNotFound` — `superseded_task_id` does not exist.
- `ErrTaskAlreadyTerminal` — the prior task is not in `submitted`
  (raced with MDS-side completion or a concurrent `CancelTask`).
  Sweepers SHOULD treat this as a benign no-op.

The store enforces `new_task.job_id == supersededTask.JobID`. See
ARCHITECTURE.md "Reservation expiry handling" for the full sweeper
flow.

### ListSubmittedWithExpiringReservation

`ListSubmittedWithExpiringReservation(ctx, before, limit) → []*PlannerTask`
returns submitted tasks whose `ReservationExpiresAt <= before`, capped
at `limit`. Tasks without a persisted `ReservationExpiresAt` are not
returned. Used by the planner-internal reservation-expiry sweeper.

### Direct read

`GetByTaskID(ctx, taskID)` returns the task with that exact `TaskID`.

`GetByJobID(ctx, jobID)` returns the **latest attempt** for the JobID
— the row with the highest `Attempt`. Most consumers (`Planner.GetJob`,
the worker on lease) want this. To inspect the full attempt chain
including any `superseded` rows, call `ListTasksByJobID(jobID)`, which
returns every `PlannerTask` for the JobID ordered by `Attempt`
ascending. Each row keeps its own `BatchID`, so audit/debug surfaces
can render the per-attempt history.

There is no durable write path for arbitrary task fields in the MVP
surface; state changes go through the narrow methods above
(`Ack`/`Nack`/`Fail`/`CancelTask`/`EnqueueContinuation`).
MDS-derived state is overlaid live into `JobView` via
`BatchClient.GetBatch` rather than mirrored into the store.

## 3. Scheduling Policy (PickFunc)

Scheduling policies plug into the worker as `PickFunc` values:

```go
type PickFunc func(ctx context.Context, store TaskStore, req *PickRequest) ([]string, error)
```

The worker invokes the configured `PickFunc` once per loop tick, then
hands the returned TaskIDs to `TaskStore.LeaseByID`.

`PickRequest`:

```json
{
  "worker_id": "planner-worker-0",
  "limit":     10,
  "now":       "2026-05-01T10:00:00Z"
}
```

Return value: a `[]string` of TaskIDs in preferred lease order; length
must be `<= limit`.

Switching policies is a one-line change at the Worker's construction
site (`Pick: myPolicy`). A `nil` `PickFunc` means "use
`TaskStore.Lease` directly" - the FCFS-only convenience path that
bypasses `PickFunc` entirely.

## 4. Worker -> Executor

Interface: `TaskExecutor`.

`Execute(ctx, *PlannerTask, *Reservation) → (batchID string, err error)`.

The executor builds an `MDSBatchSubmission` from the task and the live
reservation, then translates that into the actual MDS HTTP call. The
`*Reservation` argument is the in-memory `Reservation` the worker just
obtained from `ResourceManager.Reserve` (its `ReservationID` /
`ExpiresAt` / `Allocations` flow into `extra_body.aibrix`). It MAY be
nil when the worker stack runs without an RM (for example unit tests),
in which case the planner_decision and resource_details blocks are
simply omitted.

`MDSBatchSubmission` is the typed cross-team payload between the
planner-side submission builder and any MDS transport adapter:

```go
type MDSBatchSubmission struct {
    InputFileID      string
    Endpoint         string
    CompletionWindow string
    Metadata         map[string]string
    ExtraBody        MDSExtraBody  // {"aibrix": AIBrixExtraBody}
}
```

The on-the-wire shape:

```json
{
  "input_file_id": "file_abc",
  "endpoint": "/v1/chat/completions",
  "completion_window": "24h",
  "metadata": { "display_name": "my batch" },
  "extra_body": {
    "aibrix": {
      "job_id": "job_123",
      "planner_decision": {
        "reservation_id": "res_789",
        "reservation_resource_deadline": 1714550400
      },
      "resource_details": [
        {
          "resource_type": "spot",
          "endpoint_cluster": "cluster-a",
          "gpu_type": "H100-SXM",
          "worker_num": 4
        }
      ],
      "model_template": { "name": "llama3-batch", "version": "v1" },
      "profile":        { "name": "default",      "version": "v1" }
    }
  }
}
```

Notes:

- `planner_decision` is populated by the worker from the
  `Reservation.ReservationID` and `Reservation.ExpiresAt` returned by
  `ResourceManager.Reserve`. `resource_details` is populated from
  `Reservation.Allocations`. Both are present whenever the worker
  acquired a reservation before calling Execute; absent only when the
  worker skipped `Reserve` (e.g., a development build wired without an
  `ResourceManager`).
- `aibrix.job_id` is the primary correlation key between planner tasks
  and MDS batches.
- Pre-submit dedup is part of the executor contract: implementations
  SHOULD short-circuit when an existing MDS batch for the same
  `aibrix.job_id` is found. (The lookup mechanism is being designed
  separately - see the open dedup decision in ARCHITECTURE.md.)
- `Execute` MUST wrap upstream submit failures with `ErrMDSSubmitFailed`.

## 5. Worker -> ResourceManager

Interface: `ResourceManager`.

`Reserve(ctx, *ReserveRequest) → (*Reservation, error)` — request
capacity. Implementations MUST treat `TaskID` as a uniqueness key:
duplicate Reserve calls with the same `TaskID` MUST return the existing
`Reservation` rather than allocating a new one. Without this property,
worker crashes between `Reserve` and `Ack` (or lost RM responses) leak
reservations. Continuation tasks (created by
`TaskStore.EnqueueContinuation`) get a fresh `TaskID` and therefore a
fresh reservation — the dedup keys are intentionally per-attempt.

```json
{
  "job_id": "job_123",
  "task_id": "task_456",
  "resource_requirement": {
    "resource_type": "spot",
    "accelerator": { "type": "H100-SXM", "count": 4 }
  },
  "deadline": "2026-05-01T10:05:00Z",
  "requested_by": "planner-worker-0"
}
```

Reserve returns a `Reservation`:

```json
{
  "reservation_id": "res_789",
  "job_id": "job_123",
  "allocations": [
    {
      "resource_type": "spot",
      "endpoint_cluster": "cluster-a",
      "gpu_type": "H100-SXM",
      "worker_num": 4
    }
  ],
  "expires_at": "2026-05-01T10:05:00Z"
}
```

The `ResourceDetail` wire shape is intentionally flat (no nested
`accelerator` block). MDS consumes `gpu_type` and `worker_num` directly
when rendering the batch worker job.

The worker projects `reservation_id` and `allocations` into the MDS
submission as:

- `extra_body.aibrix.planner_decision.reservation_id`
- `extra_body.aibrix.planner_decision.reservation_resource_deadline` (Unix
  seconds)
- `extra_body.aibrix.resource_details` (= `allocations`)

`Release(ctx, *ReleaseRequest) → error` — return capacity to the pool.

```json
{
  "reservation_id": "res_789",
  "reason": "submitted"
}
```

The worker / planner SHOULD call `Release` once the attempt reaches a
terminal state: `Fail` on terminal failure, post-`MaxAttempts` `Nack`
once retries are exhausted, after `Planner.CancelJob` has transitioned
the task to `cancelled`, or once the planner observes the MDS batch
reach a terminal status (`completed`/`failed`/`expired`/`cancelled`).

`Release` is *not* called on `Ack`: the reservation is held past
submit so the reservation-expiry sweeper can detect an expired
reservation by reading `ReservationExpiresAt` on the submitted task.
The sweeper itself does *not* call `Release`; it relies on RM-side
expiry to reclaim the slot. The RM thus enforces an expiry as a
backstop for crashed workers and as the trigger for the continuation
path. When the reservation expires, the RM tears down the underlying
GPU pods and MDS marks its batch failed/expired on its own, so the
planner does not call `BatchClient.CancelBatch` from the expiry path
either.

Errors:

- `ErrInsufficientResources` — Reserve cannot satisfy the request now;
  the worker should Nack with backoff.
- `ErrResourceManagerUnavailable` — RM backend down; same retry pattern.
- `ErrReservationNotFound` — Release referenced a reservation already
  released or expired; callers may treat as a no-op success.
- `ErrReservationExpired` — the RM reclaimed the reservation before the
  worker used it; the worker must re-Reserve before continuing.

## 6. Planner -> MDS

Interface: `BatchClient`.

`GetBatch(ctx, batchID) → *BatchStatus`
`CancelBatch(ctx, batchID) → *BatchStatus`
`ListBatches(ctx, *ListBatchesRequest) → *ListBatchesResponse`

### ListBatchesRequest / ListBatchesResponse

```json
// request
{ "limit": 20, "after": "batch_xyz" }

// response
{
  "data": [ /* BatchStatus, see below */ ],
  "has_more": true
}
```

`limit` is optional (zero → upstream default, typically 20). `after` is
the batch ID returned at the tail of the previous page; empty means
"first page". Cursor advancement is the caller's responsibility — keep
calling with `after = data[len(data)-1].batch_id` until `has_more` is
`false`.

### BatchStatus

```json
{
  "batch_id": "batch_xyz",
  "job_id": "job_123",
  "status": "validating",
  "model": "llama3-batch",
  "input_file_id": "file_abc",
  "output_file_id": "",
  "error_file_id": "",
  "created_at": "2026-05-01T10:00:00Z",
  "in_progress_at": null,
  "finalizing_at": null,
  "completed_at": null,
  "failed_at": null,
  "cancelled_at": null,
  "expires_at": "2026-05-02T10:00:00Z",
  "request_counts": { "total": 100, "completed": 0, "failed": 0 },
  "errors": [],
  "usage": null,
  "metadata": { "display_name": "my batch" }
}
```

## 7. JobView

`JobView` is the merged planner + MDS read model returned by
`Planner.GetJob` / `Planner.ListJobs`. It is **derived**, not stored.

```json
{
  "task_id": "task_456",
  "job_id": "job_123",
  "planner_state": "submitted",
  "lifecycle_state": "validating",
  "batch_status": "validating",
  "batch_id": "batch_xyz",
  "model": "llama3-batch",
  "input_file_id": "file_abc",
  "output_file_id": "",
  "error_file_id": "",
  "attempts": 1,
  "max_attempts": 3,
  "last_error": "",
  "enqueued_at": "2026-05-01T10:00:00Z",
  "submitted_at": "2026-05-01T10:00:07Z",
  "in_progress_at": null,
  "finalizing_at": null,
  "completed_at": null,
  "failed_at": null,
  "cancelled_at": null,
  "expires_at": "2026-05-02T10:00:00Z",
  "request_counts": { "total": 100, "completed": 0, "failed": 0 },
  "errors": [],
  "usage": null
}
```

`JobView.LifecycleState` derivation, when no `batch_id`:

| `planner_state`     | `lifecycle_state` |
| ------------------- | ----------------- |
| `queued`            | `queued`          |
| `leased`            | `dispatching`     |
| `submitted`         | `submitted`       |
| `retryable_failure` | `queued`          |
| `terminal_failure`  | `failed`          |
| `cancelled`         | `cancelled`       |

When `batch_id` is set, the MDS `BatchStatus.Status` string maps 1:1 to
the matching `JobLifecycleState` (`validating`, `in_progress`,
`finalizing`, `completed`, `failed`, `expired`, `cancelling`,
`cancelled`). The raw MDS string is preserved in
`JobView.batch_status` for forward compatibility.

## 8. CapacityView

Interface: `ResourceCapacityReader`. `GetCapacity(ctx, *GetCapacityRequest)
→ *CapacityView` for Console / ops dashboards that need to display how much
capacity is reserved, in-use, and free across the RM pool.

`GetCapacityRequest`:

```json
{
  "cluster": "cluster-a",
  "resource_type": "spot",
  "accelerator_type": "H100-SXM"
}
```

All fields are optional filters; an empty request returns the full view.

`CapacityView`:

```json
{
  "sampled_at": "2026-05-01T10:00:00Z",
  "total": {
    "total":    256,
    "reserved": 16,
    "in_use":   180,
    "free":     60
  },
  "clusters": [
    {
      "cluster": "cluster-a",
      "region":  "us-west-2",
      "total": {
        "total": 128, "reserved": 8, "in_use": 100, "free": 20
      },
      "accelerators": [
        {
          "type": "H100-SXM",
          "counts": { "total": 64, "reserved": 4, "in_use": 56, "free": 4 }
        },
        {
          "type": "H200",
          "counts": { "total": 64, "reserved": 4, "in_use": 44, "free": 16 }
        }
      ]
    },
    {
      "cluster": "cluster-b",
      "region":  "us-east-1",
      "total": {
        "total": 128, "reserved": 8, "in_use": 80, "free": 40
      },
      "accelerators": [
        {
          "type": "H100-SXM",
          "counts": { "total": 128, "reserved": 8, "in_use": 80, "free": 40 }
        }
      ]
    }
  ]
}
```

Counts at every level satisfy `Free == Total - Reserved - InUse`.

`Reserved` vs `InUse` distinction:

- `Reserved` — capacity held by an active RM reservation but the
  corresponding job has not yet been confirmed running on MDS.
- `InUse` — capacity currently running a job (the planner Ack'd
  submission and MDS reports the batch in `in_progress` or similar).

A first-cut RM that does not yet track the running-vs-reserved split MAY
report `InUse = 0` and put all held capacity under `Reserved`. Consumers
SHOULD render the two counts separately so the UI does not need to
change when the RM grows that capability.

## 9. QueueStatsView

Interface: `QueueStatsReader`. `GetQueueStats(ctx, *GetQueueStatsRequest)
→ *QueueStatsView` for Console / ops dashboards.

`GetQueueStatsRequest`:

```json
{ "queue_name": "planner-default" }
```

`QueueStatsView`:

```json
{
  "queue_name": "planner-default",
  "bounded": true,
  "max_queued_tasks": 10000,
  "current_queued_tasks": 234,
  "current_leased_tasks": 5,
  "current_retryable_tasks": 9,
  "max_lease_ttl": "30s",
  "oldest_queued_at": "2026-05-01T09:58:00Z",
  "sampled_at": "2026-05-01T10:00:00Z"
}
```

Counts mirror the values of `PlannerTaskState` that are observable as
"in flight" from an ops perspective: `queued`, `leased`,
`retryable_failure`. Terminal states (`submitted`, `terminal_failure`,
`cancelled`) live in the per-job read model (`JobView`) and are not
counted here.

## Errors

| Sentinel                        | When                                                            |
| ------------------------------- | --------------------------------------------------------------- |
| `ErrInvalidJob`                 | `Enqueue` rejected a malformed `PlannerJob`.                    |
| `ErrJobNotFound`                | `GetJob` / `CancelJob` looked up an unknown JobID.              |
| `ErrStoreFull`                  | A bounded store reached capacity.                               |
| `ErrStoreUnavailable`           | The store backend is degraded or unreachable.                   |
| `ErrDuplicateEnqueue`           | `JobID` or `IdempotencyKey` already exists in the store.        |
| `ErrLeaseLost`                  | Ack/Nack/Fail referenced a lease the store no longer owns.      |
| `ErrMDSSubmitFailed`            | `TaskExecutor.Execute` failed talking to MDS.                   |
| `ErrInsufficientResources`      | `ResourceManager.Reserve` cannot satisfy the request now.       |
| `ErrResourceManagerUnavailable` | RM backend degraded or unreachable.                             |
| `ErrReservationNotFound`        | `Release` referenced an unknown reservation (treat as no-op).   |
| `ErrReservationExpired`         | RM reclaimed reservation before the worker used or extended it. |
| `ErrTaskAlreadyTerminal`        | Non-lease state transition (`CancelTask`/`EnqueueContinuation`) targeted a task already in a terminal state; safe to treat as no-op. |
