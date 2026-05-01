/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package planner defines the queue-based async batch scheduling boundaries
// between the AIBrix Console BFF, the planner workers, and the metadata
// service (MDS).
//
// The MVP surface is eight interfaces, one per audience:
//
//   - Planner                Console BFF -> planner front door (Enqueue/Get/List/Cancel)
//   - QueueStatsReader       Console / ops -> queue depth + worker activity
//   - ResourceCapacityReader Console / ops -> aggregated capacity (reserved/used/free)
//   - TaskStore              Planner -> durable state + worker coordination
//   - TaskExecutor           Worker -> MDS submission
//   - ResourceManager        Worker -> RM (capacity reserve/release)
//   - BatchClient            Planner -> MDS read/cancel
//   - Worker                 Long-running orchestration loop
//
// TaskStore exposes both an FCFS convenience path (Lease) and a
// policy-aware path (ListCandidates + LeaseByID). Custom scheduling
// policies plug into the worker as PickFunc values; concrete PickFunc
// implementations land alongside their consumers in follow-up PRs. A
// pluggable TaskScheduler interface is intentionally not part of MVP -
// PickFunc is the function-shaped equivalent and can be promoted to an
// interface later if a taxonomy of policies needs to coexist.
//
// Cross-team types defined here ahead of their consumers - because the teams
// building those consumers need a stable shape to design against - include
// ProfileRef, PlannerDecision, ResourceDetail, AIBrixExtraBody, MDSExtraBody,
// MDSBatchSubmission, JobLifecycleState, and QueueStatsView.
package planner

import (
	"context"
	"time"
)

// Planner is the Console BFF -> planner boundary.
//
// CreateJob in the Console BFF should stop calling MDS synchronously. It
// builds a PlannerJob, calls Enqueue, and returns a queued view to the UI.
// A planner worker submits the task to MDS later. GetJob/ListJobs return the
// merged planner + MDS view; CancelJob covers both pre- and post-submit
// cancellation.
type Planner interface {
	Enqueue(ctx context.Context, req *EnqueueRequest) (*EnqueueResult, error)
	GetJob(ctx context.Context, jobID string) (*JobView, error)
	ListJobs(ctx context.Context, req *ListJobsRequest) (*ListJobsResponse, error)
	CancelJob(ctx context.Context, req *CancelJobRequest) (*CancelJobResponse, error)
}

// QueueStatsReader is the Console/ops read boundary for planner queue
// depth and worker activity.
//
// This is split from Planner so ops dashboards can depend on it without
// pulling in the full Console-facing surface. A concrete implementation
// can be backed by direct queries against TaskStore.
type QueueStatsReader interface {
	GetQueueStats(ctx context.Context, req *GetQueueStatsRequest) (*QueueStatsView, error)
}

// ResourceCapacityReader is the Console/ops read boundary for aggregated
// resource availability across the planner's RM pool.
//
// Like QueueStatsReader, this is split from Planner so dashboard code can
// depend on a narrow surface. Concrete implementations are typically backed
// by the same RM client that the worker uses for Reserve/Release, but are
// served read-only here.
//
// CapacityView reports counts at three levels (pool total, per-cluster,
// per-accelerator-type within cluster) so the UI can render the full
// breakdown or roll up to the level it cares about.
type ResourceCapacityReader interface {
	GetCapacity(ctx context.Context, req *GetCapacityRequest) (*CapacityView, error)
}

// TaskStore is the durable state and worker-coordination boundary.
//
// One interface backs both lease coordination and durable task metadata. A
// production implementation against a single relational store (Postgres /
// MySQL with row-level locking) is straightforward; a future split into a
// fast lease layer and a separate durable layer is a refactor inside this
// interface, not a change in the public contract.
//
// Lease semantics: at-most-one worker owns a task between Lease (or
// LeaseByID) and Ack/Nack/Fail. If the worker dies or its lease expires,
// the task becomes re-leasable on the next Lease call - that is the
// crash-recovery primitive.
//
// Two lease paths are exposed:
//
//   - Lease is the FCFS convenience path: the store applies its default
//     ordering (typically available_at ascending) and atomically leases
//     the top N candidates. Workers that don't need a custom policy use
//     this directly.
//   - ListCandidates + LeaseByID is the policy-aware path: a PickFunc
//     reads candidates without leasing them, applies its ranking policy,
//     and asks the store to lease just the IDs it selected. This
//     supports priority, score-based, fair-share, and resource-aware
//     policies without coupling them to the store.
type TaskStore interface {
	// Enqueue inserts the first PlannerTask of a JobID's attempt
	// chain. The store sets Attempt = 1 regardless of any value the
	// caller passed in. Subsequent attempts (continuations after a
	// reservation expiry) go through EnqueueContinuation, never
	// through Enqueue.
	Enqueue(ctx context.Context, task *PlannerTask) error

	// FCFS convenience path.
	Lease(ctx context.Context, req *LeaseRequest) ([]*LeasedTask, error)

	// Policy-aware path. ListCandidates returns leaseable tasks
	// without acquiring a lease; LeaseByID atomically acquires
	// leases on the specified IDs (silently skipping any that are
	// no longer leaseable).
	ListCandidates(ctx context.Context, req *ListCandidatesRequest) ([]*PlannerTask, error)
	LeaseByID(ctx context.Context, req *LeaseByIDRequest) ([]*LeasedTask, error)

	RenewLease(ctx context.Context, lease TaskLease, extendBy time.Duration) (TaskLease, error)
	Ack(ctx context.Context, req *AckRequest) error
	Nack(ctx context.Context, req *NackRequest) error
	Fail(ctx context.Context, req *FailRequest) error

	// CancelTask records a user-initiated cancel. Unlike Ack/Nack/Fail
	// it does not require a lease; it is invoked from Planner.CancelJob
	// and is safe to call regardless of which worker (if any) currently
	// holds the lease. The store transitions the task to
	// PlannerTaskStateCancelled and stamps CancelledAt; a worker
	// currently holding the lease discovers this on its next
	// RenewLease/Ack/Nack/Fail call via ErrLeaseLost.
	//
	// Idempotent: cancelling a task already in cancelled or any
	// terminal state (terminal_failure, superseded, or any post-submit
	// MDS-driven terminal) is a no-op success. Returns ErrJobNotFound
	// if the TaskID does not exist.
	CancelTask(ctx context.Context, req *CancelTaskRequest) error

	// ListSubmittedWithExpiringReservation returns submitted tasks whose
	// ReservationExpiresAt is at or before `before`, capped at `limit`.
	// Used by the reservation-expiry sweeper inside Planner to find
	// submitted attempts whose RM reservation is about to expire so it
	// can cancel the MDS batch and create a continuation via
	// EnqueueContinuation. Tasks without a persisted
	// ReservationExpiresAt are not returned.
	ListSubmittedWithExpiringReservation(ctx context.Context, before time.Time, limit int) ([]*PlannerTask, error)

	// EnqueueContinuation atomically supersedes a submitted task and
	// inserts a continuation: the prior task transitions to
	// PlannerTaskStateSuperseded (BatchID preserved for audit) and a
	// new PlannerTask is inserted in PlannerTaskStateQueued with the
	// same JobID and Attempt = priorTask.Attempt + 1. Used by the
	// reservation-expiry sweeper.
	//
	// Does not require a lease (the prior task is no longer leased
	// after Ack). Returns ErrJobNotFound if SupersededTaskID does not
	// exist; ErrTaskAlreadyTerminal if the prior task is not in
	// submitted (raced with MDS-side completion or a concurrent
	// CancelTask).
	EnqueueContinuation(ctx context.Context, req *EnqueueContinuationRequest) error

	// GetByJobID returns the *latest* PlannerTask for a JobID - the
	// row with the highest Attempt. Most consumers (Planner.GetJob,
	// the worker on lease) want this. Use ListTasksByJobID for the
	// full chain of attempts.
	GetByJobID(ctx context.Context, jobID string) (*PlannerTask, error)

	// ListTasksByJobID returns every PlannerTask for a JobID, ordered
	// by Attempt ascending. Used by audit/debug surfaces and by
	// Planner.GetJob when it needs to surface the prior-attempts
	// history alongside the latest attempt's BatchStatus.
	ListTasksByJobID(ctx context.Context, jobID string) ([]*PlannerTask, error)

	GetByTaskID(ctx context.Context, taskID string) (*PlannerTask, error)
}

// TaskExecutor is the worker's MDS-submission boundary.
//
// Implementations translate the leased PlannerTask into one POST /v1/batches
// against MDS and return the resulting batch ID. They should:
//
//   - perform a pre-submit ListBatches/GetBatch dedup check by
//     extra_body.aibrix.job_id to avoid creating duplicate MDS batches when
//     a previous worker died after MDS accepted but before Ack ran;
//   - wrap upstream submit failures with ErrMDSSubmitFailed so callers can
//     route on errors.Is without parsing transport-specific error strings.
//
// res carries the live ResourceManager.Reserve result the worker is
// holding in-memory for this attempt; the executor projects
// res.ReservationID / res.ExpiresAt into extra_body.aibrix.planner_decision
// and res.Allocations into extra_body.aibrix.resource_details. res may be
// nil when the worker stack runs without an RM (for example unit tests),
// in which case those extra_body fields are simply omitted.
type TaskExecutor interface {
	Execute(ctx context.Context, task *PlannerTask, res *Reservation) (batchID string, err error)
}

// ResourceManager is the planner -> RM (resource manager) boundary.
//
// Workers call Reserve before submitting to MDS to obtain capacity for the
// task's ResourceRequirement. The returned Reservation carries an ID and
// allocation details that the worker projects into
// extra_body.aibrix.planner_decision and extra_body.aibrix.resource_details
// on the MDS submission.
//
// Reserve is idempotent per TaskID. Implementations MUST treat TaskID
// as a uniqueness key: a duplicate Reserve call with the same TaskID
// MUST return the existing Reservation rather than allocating a new
// one. This is the only correctness fix for "worker called Reserve,
// RM committed, response was lost." Without it, worker crashes between
// Reserve and Ack would leak reservations. Continuation tasks (created
// by TaskStore.EnqueueContinuation) get a new TaskID and therefore a
// fresh reservation - the dedup keys are intentionally per-attempt.
//
// Release is called from worker-driven terminal states - Fail on
// terminal failure, post-MaxAttempts Nack, or after Planner.CancelJob
// has transitioned the task to cancelled. It is *not* called on Ack:
// the reservation is intentionally held past submit so the
// reservation-expiry sweeper can detect "reservation expired before
// MDS finished" by reading ReservationExpiresAt on the submitted task.
// The sweeper itself does not call Release; it relies on RM-side
// expiry to reclaim the slot. Implementations MUST enforce a
// reservation expiry as the backstop for crashed workers and as the
// trigger for the continuation path. RM implementations SHOULD size
// the reservation TTL to cover the batch's expected duration
// (typically the job's completion_window). When the reservation
// expires, the RM is responsible for tearing down the underlying
// resources; MDS observes the resource loss and marks its batch
// failed/expired on its own, so the planner does not call CancelBatch
// from the expiry path.
//
// The concrete RM is being developed in parallel by the RM team. This
// package defines the contract so both sides can develop independently
// against a stable shape.
type ResourceManager interface {
	Reserve(ctx context.Context, req *ReserveRequest) (*Reservation, error)
	Release(ctx context.Context, req *ReleaseRequest) error
}

// BatchClient is the planner -> MDS read/control boundary used by
// Planner.GetJob/ListJobs to overlay live MDS state onto JobView and by
// Planner.CancelJob for post-submit cancellation.
//
// The primary correlation key between planner tasks and MDS batches is
// extra_body.aibrix.job_id. BatchClient implementations should expose that
// as BatchStatus.JobID so higher layers do not need to understand raw MDS
// payload shapes. Until MDS persists and echoes that field (a documented
// hard dependency), JobID may come back empty.
//
// ListBatches returns one page of batches keyed by MDS-side cursor. It
// powers Planner.ListJobs's MDS overlay and is also the building block for
// the TaskExecutor pre-submit dedup scan once MDS exposes a job_id index.
type BatchClient interface {
	GetBatch(ctx context.Context, batchID string) (*BatchStatus, error)
	CancelBatch(ctx context.Context, batchID string) (*BatchStatus, error)
	ListBatches(ctx context.Context, req *ListBatchesRequest) (*ListBatchesResponse, error)
}

// Worker is the long-running planner loop.
//
// Run is the production entry point; ProcessAvailable is one tick exposed for
// tests. Worker is process-local; wire DTOs use RFC3339 strings on the wire,
// but this interface uses native time.Time values.
//
// In MVP the Worker only handles submission (Lease -> TaskExecutor.Execute
// -> Ack/Nack/Fail). MDS state is overlaid live into JobView at read time
// via BatchClient.GetBatch; there is no reconcile-cache write path in the
// MVP TaskStore surface.
type Worker interface {
	Run(ctx context.Context) error
	ProcessAvailable(ctx context.Context, now time.Time) error
}
