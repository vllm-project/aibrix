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

package planner

import "time"

// =============================================================================
// Console / ops -> queue telemetry
// =============================================================================

// GetQueueStatsRequest queries queue depth and worker activity intended for
// Console or ops dashboards.
type GetQueueStatsRequest struct {
	QueueName string `json:"queue_name,omitempty"`
}

// QueueStatsView is the normalized read model for queue telemetry.
//
// Counts mirror the values of PlannerTaskState that are observable as
// "in flight" from an ops perspective: queued, leased, retryable_failure.
// Terminal states (submitted, terminal_failure, cancelled) are not
// counted here - they live in the per-job read model (JobView).
type QueueStatsView struct {
	QueueName             string        `json:"queue_name,omitempty"`
	Bounded               bool          `json:"bounded"`
	MaxQueuedTasks        int           `json:"max_queued_tasks,omitempty"`
	CurrentQueuedTasks    int           `json:"current_queued_tasks,omitempty"`
	CurrentLeasedTasks    int           `json:"current_leased_tasks,omitempty"`
	CurrentRetryableTasks int           `json:"current_retryable_tasks,omitempty"`
	MaxLeaseTTL           time.Duration `json:"max_lease_ttl,omitempty"`
	OldestQueuedAt        *time.Time    `json:"oldest_queued_at,omitempty"`
	SampledAt             time.Time     `json:"sampled_at"`
}

// =============================================================================
// Console / ops -> resource capacity telemetry
// =============================================================================

// GetCapacityRequest scopes a capacity query.
//
// All fields are optional filters. An empty request returns the full view.
type GetCapacityRequest struct {
	// Cluster, if non-empty, limits the response to that cluster only.
	Cluster string `json:"cluster,omitempty"`
	// ResourceType, if non-empty, limits the response to that allocation
	// mode (e.g. "spot" / "scheduled").
	ResourceType string `json:"resource_type,omitempty"`
	// AcceleratorType, if non-empty, limits the response to that accelerator
	// SKU (e.g. "H100-SXM").
	AcceleratorType string `json:"accelerator_type,omitempty"`
}

// CapacityCounts is the count breakdown at any aggregation level.
//
// Free is reported as Total - Reserved - InUse but is included explicitly
// so consumers do not have to recompute it. Implementations MUST keep the
// invariant Free == Total - Reserved - InUse.
type CapacityCounts struct {
	Total    int `json:"total"`
	Reserved int `json:"reserved"`
	InUse    int `json:"in_use"`
	Free     int `json:"free"`
}

// AcceleratorCapacity is one accelerator-type breakdown within a cluster.
type AcceleratorCapacity struct {
	Type   string         `json:"type"`
	Counts CapacityCounts `json:"counts"`
}

// ClusterCapacity is one cluster's breakdown of the resource pool.
type ClusterCapacity struct {
	Cluster      string                `json:"cluster"`
	Region       string                `json:"region,omitempty"`
	Total        CapacityCounts        `json:"total"`
	Accelerators []AcceleratorCapacity `json:"accelerators,omitempty"`
}

// CapacityView is the aggregated resource picture across the RM pool.
//
// SampledAt records when the counts were measured. Implementations may
// serve cached values; freshness can be inferred from SampledAt.
type CapacityView struct {
	SampledAt time.Time         `json:"sampled_at"`
	Total     CapacityCounts    `json:"total"`
	Clusters  []ClusterCapacity `json:"clusters,omitempty"`
}

// =============================================================================
// Console -> Planner
// =============================================================================

// EnqueueRequest is the Console -> planner contract for accepting a new job.
type EnqueueRequest struct {
	Job PlannerJob `json:"job"`
}

// EnqueueResult is returned immediately to Console after the task is accepted.
type EnqueueResult struct {
	TaskID     string           `json:"task_id"`
	JobID      string           `json:"job_id"`
	State      PlannerTaskState `json:"state"`
	EnqueuedAt time.Time        `json:"enqueued_at"`
}

// ListJobsRequest queries the merged planner job read model.
//
// Filtering is intentionally minimal for MVP. Add fields when there is a UI
// surface that needs them.
type ListJobsRequest struct {
	Limit       int    `json:"limit,omitempty"`
	After       string `json:"after,omitempty"`
	SubmittedBy string `json:"submitted_by,omitempty"`
}

// ListJobsResponse is the paginated Console-facing planner read result.
type ListJobsResponse struct {
	Data      []*JobView `json:"data"`
	HasMore   bool       `json:"has_more"`
	NextAfter string     `json:"next_after,omitempty"`
}

// CancelJobRequest is the Console -> planner cancel request.
type CancelJobRequest struct {
	JobID       string `json:"job_id"`
	Reason      string `json:"reason,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"`
}

// CancelJobResponse is the normalized result of a cancel routing decision.
type CancelJobResponse struct {
	JobID      string           `json:"job_id"`
	State      PlannerTaskState `json:"state"`
	BatchID    string           `json:"batch_id,omitempty"`
	AcceptedAt time.Time        `json:"accepted_at"`
}

// =============================================================================
// Worker -> TaskStore
// =============================================================================

// LeaseRequest is the worker -> store API for acquiring executable tasks.
//
// Lease is the FCFS convenience path: the store internally uses its
// default ordering (typically `available_at` ascending) to pick up to
// `Limit` candidates and atomically lease them. Custom scheduling
// policies (priority, score-based, fair-share, resource-aware) should
// use a PickFunc against TaskStore.ListCandidates + TaskStore.LeaseByID
// instead.
type LeaseRequest struct {
	WorkerID string        `json:"worker_id"`
	Limit    int           `json:"limit"`
	LeaseTTL time.Duration `json:"lease_ttl"`
}

// ListCandidatesRequest queries the store for tasks that are currently
// leaseable, without acquiring a lease. Used by PickFunc implementations
// to compute their ranking before committing a selection via LeaseByID.
type ListCandidatesRequest struct {
	// Limit caps the number of candidates returned. Policies typically
	// overscan (request more than they intend to lease) so the ranking
	// has room to maneuver.
	Limit int `json:"limit,omitempty"`
	// Now is the reference time for "available_at <= Now" filtering.
	// Pass time.Now() unless you have a deterministic-time test setup.
	Now time.Time `json:"now"`
}

// PickRequest is the standard input to a PickFunc. The worker passes
// itself in via WorkerID, the batch size as Limit, and the current time
// as Now (so policies stay deterministic for tests).
type PickRequest struct {
	WorkerID string    `json:"worker_id"`
	Limit    int       `json:"limit"`
	Now      time.Time `json:"now"`
}

// LeaseByIDRequest atomically acquires leases on the specified task IDs
// in one transaction.
//
// Tasks that are no longer leaseable (already leased, became terminal,
// or do not exist) are silently skipped - the response contains only
// the subset that was successfully leased. The worker treats
// `len(returned) < len(requested)` as a normal race outcome.
type LeaseByIDRequest struct {
	WorkerID string        `json:"worker_id"`
	TaskIDs  []string      `json:"task_ids"`
	LeaseTTL time.Duration `json:"lease_ttl"`
}

// AckRequest records a successful submission to MDS.
//
// ReservationID and ReservationExpiresAt are persisted on the
// PlannerTask so the reservation-expiry sweeper can find submitted
// tasks whose RM reservation has expired. Both are zero-valued when
// the worker stack is running without an RM (e.g. in unit tests).
type AckRequest struct {
	Lease                TaskLease  `json:"lease"`
	BatchID              string     `json:"batch_id"`
	SubmittedAt          time.Time  `json:"submitted_at"`
	ReservationID        string     `json:"reservation_id,omitempty"`
	ReservationExpiresAt *time.Time `json:"reservation_expires_at,omitempty"`
}

// NackRequest records a retryable failure. The store re-enqueues the task
// and makes it available again at RetryAt.
type NackRequest struct {
	Lease     TaskLease `json:"lease"`
	RetryAt   time.Time `json:"retry_at"`
	LastError string    `json:"last_error"`
}

// FailRequest records a non-retryable failure. The task moves to terminal
// failure and will not be retried.
type FailRequest struct {
	Lease     TaskLease `json:"lease"`
	LastError string    `json:"last_error"`
}

// EnqueueContinuationRequest is the planner -> store transition that
// retries a submitted task whose RM reservation expired before MDS
// finished the batch. The store atomically:
//
//  1. transitions the task identified by SupersededTaskID from
//     submitted to superseded (preserving its BatchID for audit), and
//  2. inserts NewTask as a fresh PlannerTask in queued state with
//     Attempt = supersededTask.Attempt + 1 and the same JobID.
//
// The new task starts with empty BatchID and empty ReservationID; the
// next worker that leases it calls Reserve to obtain a fresh
// reservation. RM idempotency is keyed on TaskID, so the new TaskID
// guarantees a fresh reservation.
//
// Unlike Ack/Nack/Fail it carries no TaskLease: the superseded task is
// no longer leased after Ack, and continuation is initiated by a
// planner-internal sweeper, not by a worker.
//
// Returns ErrJobNotFound if SupersededTaskID does not exist.
// Returns ErrTaskAlreadyTerminal if the superseded task is not in
// submitted (e.g. raced with MDS-side completion); callers may treat
// this as a benign no-op. The store enforces JobID consistency between
// NewTask.JobID and the superseded task's JobID.
//
// The companion BatchClient.CancelBatch call against MDS is the
// caller's responsibility; EnqueueContinuationRequest only records the
// planner-side state transition.
type EnqueueContinuationRequest struct {
	SupersededTaskID string       `json:"superseded_task_id"`
	NewTask          *PlannerTask `json:"new_task"`
	Reason           string       `json:"reason,omitempty"`
	SupersededAt     time.Time    `json:"superseded_at"`
}

// CancelTaskRequest is the planner -> store transition that records a
// user-initiated cancel against an existing task.
//
// Unlike Ack/Nack/Fail it carries no TaskLease: Planner.CancelJob does
// not own a lease, and cancellation must work regardless of which worker
// (if any) currently holds one. Any holding worker discovers the
// cancellation on its next RenewLease/Ack/Nack/Fail call via
// ErrLeaseLost and unwinds.
//
// For post-submit cancels (BatchID set on the task), Planner.CancelJob
// is responsible for calling BatchClient.CancelBatch separately;
// CancelTaskRequest only records the planner-side state transition.
type CancelTaskRequest struct {
	TaskID      string    `json:"task_id"`
	Reason      string    `json:"reason,omitempty"`
	RequestedBy string    `json:"requested_by,omitempty"`
	CancelledAt time.Time `json:"cancelled_at"`
}

// =============================================================================
// Worker -> ResourceManager
// =============================================================================

// ReserveRequest is the worker -> RM request for capacity to run one task.
type ReserveRequest struct {
	JobID               string              `json:"job_id"`
	TaskID              string              `json:"task_id"`
	ResourceRequirement ResourceRequirement `json:"resource_requirement"`
	// Deadline, if set, hints to the RM how long the reservation should
	// remain valid if the worker has not yet committed. RMs may also apply
	// their own internal default expiry. Zero means "use RM default".
	Deadline *time.Time `json:"deadline,omitempty"`
	// RequestedBy identifies the worker requesting the reservation, useful
	// for RM-side logging and quota accounting.
	RequestedBy string `json:"requested_by,omitempty"`
}

// Reservation is the RM -> worker confirmation that capacity has been
// allocated to a specific task. ReservationID and Allocations flow into
// extra_body.aibrix.planner_decision and extra_body.aibrix.resource_details
// on the MDS submission.
type Reservation struct {
	ReservationID string           `json:"reservation_id"`
	JobID         string           `json:"job_id"`
	Allocations   []ResourceDetail `json:"allocations,omitempty"`
	// ExpiresAt is when the RM will reclaim this reservation if Release has
	// not been called. Zero means "no automatic expiry".
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// ReleaseRequest is the worker -> RM call that returns a reservation to
// the pool. Workers SHOULD call Release as soon as a task reaches a
// terminal planner state (submitted-and-acked, terminal_failure,
// cancelled) rather than waiting for expiry.
type ReleaseRequest struct {
	ReservationID string `json:"reservation_id"`
	// Reason is free-form telemetry: "submitted" / "failed" / "cancelled" /
	// "retry" so RM operators can see why reservations are being released.
	Reason string `json:"reason,omitempty"`
}

// =============================================================================
// MDS read model
// =============================================================================

// BatchRequestCounts mirrors the MDS/OpenAI batch request_counts block.
type BatchRequestCounts struct {
	Total     int64 `json:"total,omitempty"`
	Completed int64 `json:"completed,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
}

// BatchJobError is the normalized error item surfaced by MDS batch reads.
type BatchJobError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   string `json:"param,omitempty"`
	Line    int64  `json:"line,omitempty"`
}

// BatchUsage is the normalized token usage block surfaced by MDS batch reads.
type BatchUsage struct {
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	TotalTokens  int64 `json:"total_tokens,omitempty"`
}

// ListBatchesRequest is the planner -> MDS paginated read for the batch
// list endpoint. The cursor semantics match MDS (and OpenAI): pass the
// last batch ID from the previous page as After.
type ListBatchesRequest struct {
	// Limit caps the page size. Zero means "use the upstream default"
	// (typically 20).
	Limit int `json:"limit,omitempty"`
	// After is the cursor returned (implicitly, as the trailing batch
	// ID) by a prior page. Empty means "first page".
	After string `json:"after,omitempty"`
}

// ListBatchesResponse is the normalized MDS list payload. The slice is
// in upstream order (newest-first by MDS convention); cursor advancement
// is the caller's responsibility - re-issue ListBatches with
// After = Data[len(Data)-1].BatchID until HasMore is false.
type ListBatchesResponse struct {
	Data    []*BatchStatus `json:"data"`
	HasMore bool           `json:"has_more"`
}

// BatchStatus is the normalized MDS batch read model consumed by planner.
//
// JobID is the planner/console correlation key extracted from
// extra_body.aibrix.job_id. It may be empty until MDS implements the
// round-trip dependency described in ARCHITECTURE.md.
type BatchStatus struct {
	BatchID       string              `json:"batch_id"`
	JobID         string              `json:"job_id,omitempty"`
	Status        string              `json:"status"`
	Model         string              `json:"model,omitempty"`
	InputFileID   string              `json:"input_file_id,omitempty"`
	OutputFileID  string              `json:"output_file_id,omitempty"`
	ErrorFileID   string              `json:"error_file_id,omitempty"`
	CreatedAt     *time.Time          `json:"created_at,omitempty"`
	InProgressAt  *time.Time          `json:"in_progress_at,omitempty"`
	FinalizingAt  *time.Time          `json:"finalizing_at,omitempty"`
	CompletedAt   *time.Time          `json:"completed_at,omitempty"`
	FailedAt      *time.Time          `json:"failed_at,omitempty"`
	CancelledAt   *time.Time          `json:"cancelled_at,omitempty"`
	ExpiresAt     *time.Time          `json:"expires_at,omitempty"`
	RequestCounts *BatchRequestCounts `json:"request_counts,omitempty"`
	Errors        []BatchJobError     `json:"errors,omitempty"`
	Usage         *BatchUsage         `json:"usage,omitempty"`
	Metadata      map[string]string   `json:"metadata,omitempty"`
}

// =============================================================================
// Merged read view
// =============================================================================

// JobView is the merged planner + MDS read model returned to Console.
//
// LifecycleState is the typed user-facing state the UI renders. It is
// derived at read time:
//
//   - if the task has no BatchID, LifecycleState is derived from
//     PlannerState (queued / dispatching / submitted / failed / cancelled);
//   - if the task has a BatchID, LifecycleState is derived from the MDS
//     BatchStatus.Status string (validating / in_progress / finalizing /
//     completed / failed / expired / cancelling / cancelled).
//
// BatchStatus is the raw MDS status string preserved for debugging /
// forward compatibility (so the UI can display details when MDS adds a
// new state ahead of the planner's mapping).
//
// JobView is computed by the Planner implementation; it is not stored.
type JobView struct {
	TaskID         string            `json:"task_id"`
	JobID          string            `json:"job_id"`
	PlannerState   PlannerTaskState  `json:"planner_state"`
	LifecycleState JobLifecycleState `json:"lifecycle_state"`
	BatchStatus    string            `json:"batch_status,omitempty"`

	BatchID      string `json:"batch_id,omitempty"`
	Model        string `json:"model,omitempty"`
	InputFileID  string `json:"input_file_id,omitempty"`
	OutputFileID string `json:"output_file_id,omitempty"`
	ErrorFileID  string `json:"error_file_id,omitempty"`

	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"max_attempts"`
	LastError   string `json:"last_error,omitempty"`

	EnqueuedAt   *time.Time `json:"enqueued_at,omitempty"`
	SubmittedAt  *time.Time `json:"submitted_at,omitempty"`
	InProgressAt *time.Time `json:"in_progress_at,omitempty"`
	FinalizingAt *time.Time `json:"finalizing_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	FailedAt     *time.Time `json:"failed_at,omitempty"`
	CancelledAt  *time.Time `json:"cancelled_at,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`

	RequestCounts *BatchRequestCounts `json:"request_counts,omitempty"`
	Errors        []BatchJobError     `json:"errors,omitempty"`
	Usage         *BatchUsage         `json:"usage,omitempty"`
}
