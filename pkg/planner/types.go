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

// PlannerJob is the Console-authored unit handed to the planner via Enqueue.
//
// The shape captures the user-visible job identity, an optional retry budget,
// the resource requirement Console picked in the wizard, the chosen
// ModelDeploymentTemplate and BatchProfile, and the OpenAI-batch payload
// MDS will eventually execute.
type PlannerJob struct {
	// JobID is the user-visible Console job identifier.
	JobID string `json:"job_id"`
	// Source identifies which frontend/backend path created this job
	// (for example "console").
	Source string `json:"source,omitempty"`
	// SubmittedBy is the user/service identity that requested the job.
	SubmittedBy string `json:"submitted_by,omitempty"`
	// SubmittedAt is when the originating component accepted the request.
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	// IdempotencyKey allows callers to safely retry enqueue requests.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// MaxAttempts is an optional per-job retry budget. Zero means
	// "use the queue/worker default policy".
	MaxAttempts int `json:"max_attempts,omitempty"`
	// Priority is the scheduling-policy hint. Higher values are leased
	// sooner by priority/score-based SchedulerFunc implementations. Zero
	// is the default; FCFS policies ignore the field. Reserved on the
	// type ahead of any concrete scoring policy so future scoring
	// rollouts do not require a schema migration.
	Priority int `json:"priority,omitempty"`
	// ResourceRequirement is provided directly by Console so planner can make
	// scheduling decisions without first resolving the template.
	ResourceRequirement ResourceRequirement `json:"resource_requirement"`
	// ModelTemplate is the authoritative Console-selected ModelDeploymentTemplate
	// reference. The worker projects it into extra_body.aibrix.model_template
	// when submitting to MDS.
	ModelTemplate *ModelTemplateRef `json:"model_template,omitempty"`
	// Profile is the authoritative Console-selected BatchProfile reference.
	// The worker projects it into extra_body.aibrix.profile when submitting.
	Profile *ProfileRef `json:"profile,omitempty"`
	// BatchPayload is the OpenAI-batch-format submission body.
	BatchPayload BatchPayload `json:"batch_payload"`
}

// BatchPayload is the OpenAI-batch-format submission MDS will execute.
//
// The InputFileID must already exist in MDS (uploaded out-of-band, typically
// via the console's FileHandler proxy). The planner does not upload files;
// it only creates the batch record referencing an existing file.
type BatchPayload struct {
	InputFileID      string            `json:"input_file_id"`
	Endpoint         string            `json:"endpoint"`
	CompletionWindow string            `json:"completion_window"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// AcceleratorRequirement describes the accelerator shape a job needs.
type AcceleratorRequirement struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// ResourceRequirement is the Console -> planner scheduling hint/requirement.
//
// `resource_type` carries the allocation mode requested by product design
// (for example "spot" or "scheduled"). The field set is intentionally
// minimal for MVP.
type ResourceRequirement struct {
	ResourceType string                 `json:"resource_type"`
	Accelerator  AcceleratorRequirement `json:"accelerator"`
}

// ModelTemplateRef identifies the ModelDeploymentTemplate MDS should use when
// rendering the batch worker job. Empty Version means "latest active version".
type ModelTemplateRef struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ProfileRef identifies the BatchProfile MDS should apply when scheduling
// the batch. Empty Version means "latest active version".
//
// MDS validates the named profile against its ConfigMap-backed registry;
// see python/aibrix/aibrix/metadata/api/v1/batch.py.
type ProfileRef struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// PlannerDecision captures the planner-owned scheduling decision that
// accompanies the final MDS submission.
//
// Populated by the worker after a successful RM-side capacity request,
// from the resulting Reservation: ReservationID is the Reservation's ID,
// ReservationResourceDeadline is the Unix-second form of Reservation.ExpiresAt.
// The worker projects this struct into extra_body.aibrix.planner_decision on
// the MDS submission. Empty when no reservation has been obtained yet.
type PlannerDecision struct {
	ReservationID               string `json:"reservation_id,omitempty"`
	ReservationResourceDeadline int64  `json:"reservation_resource_deadline,omitempty"`
}

// ResourceDetail captures one resource allocation selected by the planner.
//
// Populated by the worker from Reservation.Allocations after Reserve returns;
// AIBrixExtraBody.ResourceDetails carries a list because a single batch may
// legitimately span more than one slice.
//
// The wire shape is intentionally flat (no nested accelerator block) - this
// matches what MDS will consume. ResourceType identifies the allocation
// mode (for example "spot" / "scheduled"), GPUType is the SKU
// (for example "H100-SXM"), and WorkerNum is the worker count for that
// slice.
type ResourceDetail struct {
	ResourceType    string `json:"resource_type"`
	EndpointCluster string `json:"endpoint_cluster,omitempty"`
	GPUType         string `json:"gpu_type"`
	WorkerNum       int    `json:"worker_num"`
}

// AIBrixExtraBody is the logical payload carried under extra_body.aibrix
// when talking to MDS. It is the wire-level contract between the planner
// and the metadata service.
//
// JobID is the primary correlation key between planner tasks and MDS
// batches. MDS must persist and echo it for BatchStatus.JobID round-trips
// to work; see ARCHITECTURE.md "MDS correlation and dedup".
type AIBrixExtraBody struct {
	JobID           string            `json:"job_id"`
	PlannerDecision *PlannerDecision  `json:"planner_decision,omitempty"`
	ResourceDetails []ResourceDetail  `json:"resource_details,omitempty"`
	ModelTemplate   *ModelTemplateRef `json:"model_template,omitempty"`
	Profile         *ProfileRef       `json:"profile,omitempty"`
}

// MDSExtraBody is the top-level extra_body payload the planner sends to MDS.
//
// Today the only namespace is "aibrix"; the wrapper exists because the
// MDSBatchSubmission struct is the shared type that paired components
// (executor, MDS transport adapter) construct and consume.
type MDSExtraBody struct {
	AIBrix AIBrixExtraBody `json:"aibrix"`
}

// MDSBatchSubmission is the fully prepared submit payload for
// POST /v1/batches.
//
// This is the shared contract between the planner-side submission
// builder and any MDS transport adapter (openai-go SDK today; potentially
// others later). By the time MDSBatchSubmission is constructed, all planner
// decisions are materialized; the transport's only job is to translate this
// struct into a concrete HTTP call.
type MDSBatchSubmission struct {
	InputFileID      string            `json:"input_file_id"`
	Endpoint         string            `json:"endpoint"`
	CompletionWindow string            `json:"completion_window"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	ExtraBody        MDSExtraBody      `json:"extra_body"`
}

// PlannerTaskState is the internal lifecycle of a queued planner task.
//
// MVP state machine (purely forward; no backward edges):
//
//	queued -> leased -> submitted          (happy path)
//	            |
//	            +--> retryable_failure --> queued    (retry within attempt)
//	            |                       \-> terminal_failure (Nack budget)
//	            +--> terminal_failure              (non-retryable)
//	            +--> cancelled                     (Cancel before submit)
//	   submitted ----------------------> cancelled (Cancel after submit)
//	   submitted ----------------------> superseded
//	                                        (EnqueueContinuation: a new task
//	                                         with the same JobID was created
//	                                         because this attempt's reservation
//	                                         expired before MDS finished)
//
// Each PlannerTask is one attempt at running a job. When a reservation
// expires and a new attempt is needed, the planner inserts a fresh
// PlannerTask (new TaskID, same JobID, Attempt+1) and transitions the
// old one to superseded. State transitions on a task are forward-only.
//
// PlannerTaskState is the planner-internal coordination state. The
// user-facing JobLifecycleState is derived from this plus BatchStatus
// when JobView is assembled.
type PlannerTaskState string

const (
	// PlannerTaskStateQueued means the task has been accepted and is waiting
	// for a worker to lease it.
	PlannerTaskStateQueued PlannerTaskState = "queued"
	// PlannerTaskStateLeased means a worker currently owns the lease and is
	// preparing or performing the MDS submission.
	PlannerTaskStateLeased PlannerTaskState = "leased"
	// PlannerTaskStateSubmitted means MDS accepted the batch and returned a
	// batch ID. The MDS BatchStatus drives any further user-visible state.
	PlannerTaskStateSubmitted PlannerTaskState = "submitted"
	// PlannerTaskStateRetryableFailure means the most recent attempt failed
	// but the task may be retried later.
	PlannerTaskStateRetryableFailure PlannerTaskState = "retryable_failure"
	// PlannerTaskStateTerminalFailure means the task exhausted retries or
	// hit a non-recoverable validation/configuration error.
	PlannerTaskStateTerminalFailure PlannerTaskState = "terminal_failure"
	// PlannerTaskStateCancelled means the planner queue considers this task
	// closed. Cancellation may have originated before or after submission.
	PlannerTaskStateCancelled PlannerTaskState = "cancelled"
	// PlannerTaskStateSuperseded means this attempt was abandoned because
	// its RM reservation expired before MDS finished, and a continuation
	// PlannerTask (same JobID, higher Attempt) was inserted to retry the
	// work under a fresh reservation. The BatchID on the superseded task
	// is preserved for audit; the live work is on the continuation.
	PlannerTaskStateSuperseded PlannerTaskState = "superseded"
)

// JobLifecycleState is the user-facing unified job status.
//
// This is the typed enum the Console UI renders. It is **derived** at read
// time by merging PlannerTaskState (when no BatchID is set) or the MDS
// BatchStatus.Status string (when a BatchID is set) into one of the values
// below. JobView.LifecycleState carries the result.
//
// The set is closed - if MDS adds a new status string, the planner's
// derivation helper must be updated and the UI agrees on a rendering.
type JobLifecycleState string

const (
	// Planner-derived (no BatchID yet).
	JobLifecycleStateQueued      JobLifecycleState = "queued"
	JobLifecycleStateDispatching JobLifecycleState = "dispatching"
	JobLifecycleStateSubmitted   JobLifecycleState = "submitted"

	// MDS-derived (BatchID present).
	JobLifecycleStateCreated    JobLifecycleState = "created"
	JobLifecycleStateValidating JobLifecycleState = "validating"
	JobLifecycleStateInProgress JobLifecycleState = "in_progress"
	JobLifecycleStateFinalizing JobLifecycleState = "finalizing"
	JobLifecycleStateCompleted  JobLifecycleState = "completed"
	JobLifecycleStateFailed     JobLifecycleState = "failed"
	JobLifecycleStateExpired    JobLifecycleState = "expired"
	JobLifecycleStateCancelling JobLifecycleState = "cancelling"
	JobLifecycleStateCancelled  JobLifecycleState = "cancelled"
)

// PlannerTask is the durable unit stored in the planner store. Each
// PlannerTask is one attempt at running a job: TaskID is the per-attempt
// identity; JobID is the user-facing identity that may have multiple
// attempts (rows) chained via Attempt.
//
// Console enqueues the first PlannerTask (Attempt = 1) derived from
// PlannerJob. A worker later leases it, executes it, and writes back
// the BatchID and retry state on Ack. If that attempt's reservation
// expires before MDS finishes, the planner-internal sweeper inserts a
// continuation PlannerTask via TaskStore.EnqueueContinuation (new
// TaskID, same JobID, Attempt+1) and transitions the prior row to
// superseded. The chain is reconstructable via TaskStore.ListTasksByJobID.
type PlannerTask struct {
	TaskID         string `json:"task_id"`
	JobID          string `json:"job_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// Attempt is the 1-based position of this row in the JobID's
	// attempt chain. The first task Console enqueues has Attempt = 1;
	// each EnqueueContinuation derives Attempt = priorTask.Attempt + 1.
	// State transitions on a single PlannerTask are forward-only;
	// retries that change Attempt always create new rows.
	Attempt int `json:"attempt"`

	State PlannerTaskState `json:"state"`

	ResourceRequirement ResourceRequirement `json:"resource_requirement"`
	ModelTemplate       *ModelTemplateRef   `json:"model_template,omitempty"`
	Profile             *ProfileRef         `json:"profile,omitempty"`
	Payload             BatchPayload        `json:"payload"`

	// Priority is copied from PlannerJob. Persisted on the task so
	// score-based SchedulerFunc implementations can rank candidates
	// without re-resolving the original PlannerJob.
	Priority int `json:"priority,omitempty"`

	// ReservationID, if non-empty, is the active RM-side reservation
	// held for this attempt. The worker carries it in-memory between
	// the RM call and Ack, then Ack persists it on the PlannerTask.
	// The RM-side capacity-request path is idempotent per TaskID, so
	// a re-leasing worker after a crash gets the same Reservation
	// back rather than a new slot.
	ReservationID string `json:"reservation_id,omitempty"`

	// ReservationExpiresAt mirrors Reservation.ExpiresAt at Ack time.
	// Persisted so the reservation-expiry sweeper can find attempts
	// whose RM reservation has expired without round-tripping to the RM.
	ReservationExpiresAt *time.Time `json:"reservation_expires_at,omitempty"`

	// BatchID is populated only after a successful submission to MDS.
	// It is immutable for the lifetime of this task: retries triggered
	// by reservation expiry insert a new task (continuation) instead of
	// clearing this field.
	BatchID string `json:"batch_id,omitempty"`

	// Attempts and MaxAttempts count Nack-driven retries *within* this
	// PlannerTask (i.e. submit failures handled by Nack). They are
	// distinct from Attempt (singular) above, which is the chain
	// position across continuation tasks.
	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"max_attempts"`
	LastError   string `json:"last_error,omitempty"`

	// Lease metadata - at-most-one worker owns the task at a time.
	LeaseOwner     string     `json:"lease_owner,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`

	// Lifecycle timestamps. nil means "not yet".
	EnqueuedAt  *time.Time `json:"enqueued_at,omitempty"`
	AvailableAt *time.Time `json:"available_at,omitempty"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
	LastTriedAt *time.Time `json:"last_tried_at,omitempty"`
}

// TaskLease is the worker's proof of ownership for a leased task. It is
// returned alongside the task by Lease and must accompany every Ack/Nack/Fail
// so the store can reject calls from stale workers.
type TaskLease struct {
	TaskID         string    `json:"task_id"`
	WorkerID       string    `json:"worker_id"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// LeasedTask pairs a leased PlannerTask with the lease token needed to
// ack/nack/fail it.
type LeasedTask struct {
	Task  *PlannerTask `json:"task"`
	Lease TaskLease    `json:"lease"`
}
