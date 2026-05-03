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
// The MVP surface is five exported interfaces plus one function type:
//
//   - Planner                Console BFF -> planner front door (Enqueue/Get/List/Cancel)
//   - QueueStatsReader       Console / ops -> queue depth + worker activity
//   - ResourceCapacityReader Console / ops -> aggregated capacity (reserved/used/free)
//   - TaskStore              Planner / Worker -> durable state + lease coordination
//   - BatchClient            Planner / Worker -> MDS create / read / cancel
//   - SchedulerFunc (func)   Worker -> pluggable scheduling policy
//
// The Worker is a concrete struct in this package, not an exported
// interface, since it has only one production implementation. Pre-submit
// dedup and the PlannerTask -> MDS submission translation are private
// methods on the Worker; they hit MDS through the BatchClient interface,
// which is the single mocking seam for MDS in tests.
//
// The planner -> RM boundary is owned by an adjacent RM package and is
// not declared here; the worker calls into it directly and translates
// the result into the in-memory Reservation shape this package defines.
//
// TaskStore exposes both an FCFS convenience path (Lease) and a
// policy-aware path (ListCandidates + LeaseByID). Custom scheduling
// policies plug into the worker as SchedulerFunc values; concrete
// SchedulerFunc implementations land alongside their consumers in
// follow-up PRs. A pluggable Scheduler interface is intentionally not
// part of MVP - SchedulerFunc is the function-shaped equivalent and
// can be promoted to an interface later if a taxonomy of policies
// needs to coexist (the same http.Handler / http.HandlerFunc pattern
// from the standard library).
//
// The MVP runs a single worker process with multiple goroutines
// dispatching from one TaskStore; lease semantics protect against
// process-level crash recovery (long TTL + natural reclaim on restart),
// not against concurrent leasing across multiple workers. Mid-flight
// lease renewal (heartbeats) is intentionally not part of the MVP
// surface; it can be added back if and when the planner scales to
// multiple worker processes.
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
// Lease semantics in MVP: a lease records that one worker owns the task
// between Lease (or LeaseByID) and Ack/Nack/Fail. The MVP runs a single
// worker process, so the lease is primarily a crash-recovery primitive:
// if the process dies, lease TTL eventually expires and the task becomes
// re-leasable on the next Lease call. Lease TTL is sized longer than the
// expected Execute duration so a slow MDS submission does not spuriously
// invalidate the in-flight worker; mid-flight RenewLease is not part of
// the MVP surface. Ack/Nack/Fail are accepted as long as the
// LeaseOwner/LeaseExpiresAt on the request still match what the store
// recorded for the task; if the lease has been replaced (e.g. by a
// concurrent CancelTask or a future multi-worker reclaim) the store
// returns ErrLeaseLost.
//
// Two lease paths are exposed:
//
//   - Lease is the FCFS convenience path: the store applies its default
//     ordering (typically available_at ascending) and atomically leases
//     the top N candidates. Workers that don't need a custom policy use
//     this directly.
//   - ListCandidates + LeaseByID is the policy-aware path: a
//     SchedulerFunc reads candidates without leasing them, applies its
//     ranking policy, and asks the store to lease just the IDs it
//     selected. This supports priority, score-based, fair-share, and
//     resource-aware policies without coupling them to the store.
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

	Ack(ctx context.Context, req *AckRequest) error
	Nack(ctx context.Context, req *NackRequest) error
	Fail(ctx context.Context, req *FailRequest) error

	// CancelTask records a user-initiated cancel. Unlike Ack/Nack/Fail
	// it does not require a lease; it is invoked from Planner.CancelJob
	// and is safe to call regardless of which worker (if any) currently
	// holds the lease. The store transitions the task to
	// PlannerTaskStateCancelled and stamps CancelledAt; a worker
	// currently holding the lease discovers this on its next
	// Ack/Nack/Fail call via ErrLeaseLost.
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

// BatchClient is the planner -> MDS adapter for all batch operations.
// It is the single mocking seam for MDS in tests: a fake BatchClient
// covers both the worker's submit path and the planner's read/cancel
// paths.
//
// CreateBatch is called by the worker to submit a prepared
// MDSBatchSubmission (built from the leased PlannerTask plus the
// in-memory Reservation). The worker is responsible for the
// pre-submit dedup check (calling GetBatch keyed on
// extra_body.aibrix.job_id, or ListBatches once MDS indexes that
// field) and for wrapping upstream submit failures with
// ErrMDSSubmitFailed; BatchClient implementations are thin transport
// adapters and need only return the resulting BatchStatus or a
// transport error.
//
// GetBatch and CancelBatch are used by Planner.GetJob/ListJobs to
// overlay live MDS state onto JobView and by Planner.CancelJob for
// post-submit cancellation.
//
// The primary correlation key between planner tasks and MDS batches is
// extra_body.aibrix.job_id. BatchClient implementations should expose that
// as BatchStatus.JobID so higher layers do not need to understand raw MDS
// payload shapes. Until MDS persists and echoes that field (a documented
// hard dependency), JobID may come back empty.
//
// ListBatches returns one page of batches keyed by MDS-side cursor. It
// powers Planner.ListJobs's MDS overlay and is also the building block for
// the worker pre-submit dedup scan once MDS exposes a job_id index.
type BatchClient interface {
	CreateBatch(ctx context.Context, req *MDSBatchSubmission) (*BatchStatus, error)
	GetBatch(ctx context.Context, batchID string) (*BatchStatus, error)
	CancelBatch(ctx context.Context, batchID string) (*BatchStatus, error)
	ListBatches(ctx context.Context, req *ListBatchesRequest) (*ListBatchesResponse, error)
}
