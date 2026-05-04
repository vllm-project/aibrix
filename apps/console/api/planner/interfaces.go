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
// The MVP surface is three exported interfaces plus one function type:
//
//   - Planner                Console BFF -> planner front door (jobs + telemetry)
//   - TaskStore              Planner / Worker -> durable state + claim coordination
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
// TaskStore exposes both an FCFS convenience path (Claim) and a
// policy-aware path (ListCandidates + ClaimByID). Custom scheduling
// policies plug into the worker as SchedulerFunc values; concrete
// SchedulerFunc implementations land alongside their consumers in
// follow-up PRs. A pluggable Scheduler interface is intentionally not
// part of MVP - SchedulerFunc is the function-shaped equivalent and
// can be promoted to an interface later if a taxonomy of policies
// needs to coexist (the same http.Handler / http.HandlerFunc pattern
// from the standard library).
//
// The MVP runs a single Worker process with multiple goroutines
// dispatching from one TaskStore. The state machine itself
// (queued -> claimed -> submitted / etc.) is the coordination
// primitive: Claim atomically transitions tasks queued -> claimed,
// Ack/Nack/Fail check the row is still in claimed before transitioning
// it forward, and TaskStore.RecoverInProgress (called once on Worker
// startup) resets any tasks left in claimed by a crashed prior process
// back to queued. In-process double-claim is prevented by the
// dispatcher's channel-based hand-off.
package planner

import (
	"context"
	"time"
)

// Planner is the Console BFF -> planner boundary. It carries both the
// job-lifecycle methods (Enqueue / GetJob / ListJobs) and the
// telemetry reads Console / ops dashboards need (GetQueueStats /
// GetCapacity). Console talks to one client; the underlying
// implementation can still delegate the read methods to a TaskStore
// query (queue stats) or an RM client (capacity) — that's an
// implementation detail, not part of the public surface.
//
// CreateJob in the Console BFF should stop calling MDS synchronously.
// It builds a PlannerJob, calls Enqueue, and returns a queued view to
// the UI. A planner worker submits the task to MDS later. GetJob /
// ListJobs return the merged planner + MDS view; cancellation flows
// through MDS directly (the OpenAI Batches API exposes /cancel) and
// the planner observes the resulting terminal state on its read-time
// overlay rather than driving cancellation itself.
//
// CapacityView reports counts at three levels (pool total,
// per-cluster, per-accelerator-type within cluster) so the UI can
// render the full breakdown or roll up to the level it cares about.
type Planner interface {
	// Job lifecycle.
	Enqueue(ctx context.Context, req *EnqueueRequest) (*EnqueueResult, error)
	GetJob(ctx context.Context, jobID string) (*JobView, error)
	ListJobs(ctx context.Context, req *ListJobsRequest) (*ListJobsResponse, error)

	// Telemetry.
	GetQueueStats(ctx context.Context, req *GetQueueStatsRequest) (*QueueStatsView, error)
	GetCapacity(ctx context.Context, req *GetCapacityRequest) (*CapacityView, error)
}

// TaskStore is the durable state and worker-coordination boundary.
//
// One interface backs both task coordination and durable task metadata.
// A production implementation against a single relational store
// (Postgres / MySQL with row-level locking) is straightforward; a
// future split into a fast claim layer and a separate durable layer is
// a refactor inside this interface, not a change in the public
// contract.
//
// Claim semantics: Claim and ClaimByID atomically transition queued
// tasks to claimed and return them to the worker. In-process
// double-claim is prevented by the dispatcher's channel-based hand-off,
// and crash recovery is handled by RecoverInProgress on Worker startup
// (which resets every claimed row back to queued). Ack/Nack/Fail
// validate that the task is still in claimed before transitioning it
// forward; if a concurrent CancelTask transitioned the task to
// cancelled (or any terminal state) mid-submit, the store returns
// ErrTaskAlreadyTerminal and the worker drops its in-flight result.
//
// Two claim paths are exposed:
//
//   - Claim is the FCFS convenience path: the store applies its default
//     ordering (typically available_at ascending) and atomically claims
//     the top N candidates. Workers that don't need a custom policy use
//     this directly.
//   - ListCandidates + ClaimByID is the policy-aware path: a
//     SchedulerFunc reads candidates without claiming them, applies its
//     ranking policy, and asks the store to claim just the IDs it
//     selected. This supports priority, score-based, fair-share, and
//     resource-aware policies without coupling them to the store.
type TaskStore interface {
	// Enqueue inserts the first PlannerTask of a JobID's attempt
	// chain. The store sets Attempt = 1 regardless of any value the
	// caller passed in. Subsequent attempts (continuations after a
	// reservation expiry) go through EnqueueContinuation, never
	// through Enqueue.
	Enqueue(ctx context.Context, task *PlannerTask) error

	// FCFS convenience path. Atomically transitions up to req.Limit
	// queued tasks to claimed and returns them.
	Claim(ctx context.Context, req *ClaimRequest) ([]*PlannerTask, error)

	// Policy-aware path. ListCandidates returns claimable tasks
	// without transitioning them; ClaimByID atomically transitions
	// the specified IDs from queued to claimed (silently skipping
	// any that are no longer claimable).
	ListCandidates(ctx context.Context, req *ListCandidatesRequest) ([]*PlannerTask, error)
	ClaimByID(ctx context.Context, req *ClaimByIDRequest) ([]*PlannerTask, error)

	Ack(ctx context.Context, req *AckRequest) error
	Nack(ctx context.Context, req *NackRequest) error
	Fail(ctx context.Context, req *FailRequest) error

	// RecoverInProgress is called once on Worker startup. It resets
	// every PlannerTask currently in PlannerTaskStateClaimed back to
	// PlannerTaskStateQueued so any tasks the prior process was
	// holding when it crashed are re-claimable on the next Claim
	// call. Returns the number of rows transitioned for telemetry.
	//
	// Safe to call when nothing is currently claimed (returns 0).
	// Implementations MUST make this call atomic with respect to
	// concurrent Claim/ClaimByID calls; in MVP the Worker calls it
	// before the dispatcher loop starts so contention is not a
	// concern.
	RecoverInProgress(ctx context.Context) (int, error)

	// ListSubmittedWithExpiringReservation returns submitted tasks whose
	// ReservationExpiresAt is at or before `before`, capped at `limit`.
	// Used by the reservation-expiry sweeper inside Planner to find
	// submitted attempts whose RM reservation is about to expire so it
	// can create a continuation via EnqueueContinuation. Tasks without a
	// persisted ReservationExpiresAt are not returned.
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
// MDSBatchSubmission (built from the claimed PlannerTask plus the
// in-memory Reservation). The worker is responsible for the
// pre-submit dedup check (calling GetBatch keyed on
// extra_body.aibrix.job_id, or ListBatches once MDS indexes that
// field) and for wrapping upstream submit failures with
// ErrMDSSubmitFailed; BatchClient implementations are thin transport
// adapters and need only return the resulting BatchStatus or a
// transport error.
//
// GetBatch is used by Planner.GetJob/ListJobs to overlay live MDS
// state onto JobView and by the worker for pre-submit dedup.
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
	ListBatches(ctx context.Context, req *ListBatchesRequest) (*ListBatchesResponse, error)
}
