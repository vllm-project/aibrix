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
// "in flight" from an ops perspective: queued, claimed, retryable_failure.
// Terminal states (submitted, terminal_failure, cancelled) are not
// counted here - they live in the per-job read model (JobView).
type QueueStatsView struct {
	QueueName             string     `json:"queue_name,omitempty"`
	Bounded               bool       `json:"bounded"`
	MaxQueuedTasks        int        `json:"max_queued_tasks,omitempty"`
	CurrentQueuedTasks    int        `json:"current_queued_tasks,omitempty"`
	CurrentClaimedTasks   int        `json:"current_claimed_tasks,omitempty"`
	CurrentRetryableTasks int        `json:"current_retryable_tasks,omitempty"`
	OldestQueuedAt        *time.Time `json:"oldest_queued_at,omitempty"`
	SampledAt             time.Time  `json:"sampled_at"`
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
	// AllocationMode, if non-empty, limits the response to that
	// allocation mode (one of ResourceAllocationModeOnDemand / Spot /
	// Scheduled). The field name and JSON tag mirror
	// ResourceRequirement.AllocationMode so a planner request and a
	// capacity filter use the same vocabulary.
	AllocationMode ResourceAllocationMode `json:"allocation_mode,omitempty"`
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

// =============================================================================
// Worker -> TaskStore
// =============================================================================

// ClaimRequest is the worker -> store API for acquiring executable tasks.
//
// Claim is the FCFS convenience path: the store internally uses its
// default ordering (typically `available_at` ascending) to pick up to
// `Limit` candidates and atomically transition them from queued to
// claimed. Custom scheduling policies (priority, score-based,
// fair-share, resource-aware) should use a SchedulerFunc against
// TaskStore.ListCandidates + TaskStore.ClaimByID instead.
//
// Tasks held in claimed across a process crash are reset to queued
// by TaskStore.RecoverInProgress on the next Worker startup.
type ClaimRequest struct {
	WorkerID string `json:"worker_id"`
	Limit    int    `json:"limit"`
}

// ListCandidatesRequest queries the store for tasks that are currently
// claimable, without acquiring them. Used by SchedulerFunc
// implementations to compute their ranking before committing a
// selection via ClaimByID.
type ListCandidatesRequest struct {
	// Limit caps the number of candidates returned. Policies typically
	// overscan (request more than they intend to claim) so the ranking
	// has room to maneuver.
	Limit int `json:"limit,omitempty"`
	// Now is the reference time for "available_at <= Now" filtering.
	// Pass time.Now() unless you have a deterministic-time test setup.
	Now time.Time `json:"now"`
}

// ScheduleRequest is the standard input to a SchedulerFunc. The worker
// passes itself in via WorkerID, the batch size as Limit, and the
// current time as Now (so policies stay deterministic for tests).
type ScheduleRequest struct {
	WorkerID string    `json:"worker_id"`
	Limit    int       `json:"limit"`
	Now      time.Time `json:"now"`
}

// ClaimByIDRequest atomically transitions the specified task IDs from
// queued to claimed in one transaction.
//
// Tasks that are no longer claimable (already claimed, became terminal,
// or do not exist) are silently skipped - the response contains only
// the subset that was successfully claimed. The worker treats
// `len(returned) < len(requested)` as a normal race outcome.
type ClaimByIDRequest struct {
	WorkerID string   `json:"worker_id"`
	TaskIDs  []string `json:"task_ids"`
}

// AckRequest records a successful submission to MDS.
//
// The store transitions the task identified by TaskID from claimed to
// submitted. ReservationID and ReservationExpiresAt are persisted on
// the PlannerTask so the reservation-expiry sweeper can find submitted
// tasks whose RM reservation has expired; both are zero-valued when
// the worker stack is running without an RM (e.g. in unit tests).
//
// Returns ErrTaskAlreadyTerminal if the task is no longer in claimed
// (typically because a concurrent CancelTask transitioned it to
// cancelled mid-submit); the worker drops the in-flight result.
type AckRequest struct {
	TaskID               string     `json:"task_id"`
	BatchID              string     `json:"batch_id"`
	SubmittedAt          time.Time  `json:"submitted_at"`
	ReservationID        string     `json:"reservation_id,omitempty"`
	ReservationExpiresAt *time.Time `json:"reservation_expires_at,omitempty"`
}

// NackRequest records a retryable failure. The store transitions the
// task identified by TaskID from claimed to retryable_failure (or, if
// Attempts has reached MaxAttempts, to terminal_failure) and makes the
// row queued again at RetryAt.
//
// Returns ErrTaskAlreadyTerminal if the task is no longer in claimed.
type NackRequest struct {
	TaskID    string    `json:"task_id"`
	RetryAt   time.Time `json:"retry_at"`
	LastError string    `json:"last_error"`
}

// FailRequest records a non-retryable failure. The store transitions
// the task identified by TaskID from claimed to terminal_failure; the
// task will not be retried.
//
// Returns ErrTaskAlreadyTerminal if the task is no longer in claimed.
type FailRequest struct {
	TaskID    string `json:"task_id"`
	LastError string `json:"last_error"`
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
// next worker that claims it calls Reserve to obtain a fresh
// reservation. RM idempotency is keyed on TaskID, so the new TaskID
// guarantees a fresh reservation.
//
// EnqueueContinuation is initiated by a planner-internal sweeper, not
// by a worker; the superseded task is already in submitted (no longer
// claimed) by the time the sweeper looks at it.
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

// =============================================================================
// Worker in-memory: Reservation
// =============================================================================

// Reservation is the in-memory shape the worker assembles from the
// RM-side response after acquiring capacity from the Resource Manager
// (whose contract is owned by an adjacent RM package). The worker
// projects ReservationID and Allocations into
// extra_body.aibrix.planner_decision and extra_body.aibrix.resource_details
// when building the MDSBatchSubmission for BatchClient.CreateBatch.
type Reservation struct {
	ReservationID string           `json:"reservation_id"`
	JobID         string           `json:"job_id"`
	Allocations   []ResourceDetail `json:"allocations,omitempty"`
	// ExpiresAt is when the RM will reclaim this reservation if Release
	// has not been called. Zero means "no automatic expiry".
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
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
// round-trip dependency described in README.md "MDS correlation and
// dedup (hard external dependency)".
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
