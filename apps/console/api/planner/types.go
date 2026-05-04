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
	// ResourceRequirement carries the Console-provided demand
	// (Accelerator.Type and Accelerator.Count) plus a set of
	// planner-owned fields the planner fills in during Enqueue.
	// Console populates only ResourceRequirement.Accelerator; the
	// planner reads it together with ModelTemplate and derives the
	// resolved Groups, TimeWindow, AllocationMode, Provider, and
	// per-provider placement blocks from scheduling policy, then
	// persists the fully resolved struct on the PlannerTask before
	// any worker leases it.
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

// ResourceAllocationMode is the typed allocation mode of a reservation.
// The values mirror the categories haiyang's catalog read API surfaces
// on apps/console/api/resource_manager/catalog.ResourceStat (one of
// OnDemand / Spot / Scheduled). Casing matches the catalog JSON shape
// so a marshalled planner request and a marshalled catalog response
// use the same string for the same concept.
//
// The mode does NOT currently flow into the cross-team
// ResourceProvisionSpec - that contract has no first-class slot for it
// on writes today. The mode is preserved on the planner side and
// projected into extra_body.aibrix.resource_details[*].resource_type
// for MDS audit / billing correlation. When the RM team adds a
// first-class slot to ResourceProvisionSpec, the rmadapter forwards it
// without any planner-side change.
type ResourceAllocationMode string

const (
	ResourceAllocationModeOnDemand  ResourceAllocationMode = "onDemand"
	ResourceAllocationModeSpot      ResourceAllocationMode = "spot"
	ResourceAllocationModeScheduled ResourceAllocationMode = "scheduled"
)

// ResourceProviderType names which provisioner backend a reservation
// targets. Values mirror the
// apps/console/api/resource_manager/types.ResourceProvisionType enum so
// the rmadapter can cross-check against its underlying
// Provisioner.Type() and reject mismatches with ErrInvalidJob.
type ResourceProviderType string

const (
	ResourceProviderKubernetes  ResourceProviderType = "kubernetes"
	ResourceProviderAWS         ResourceProviderType = "aws"
	ResourceProviderLambdaCloud ResourceProviderType = "lambdaCloud"
)

// AcceleratorRequirement is the Console-provided "what type and how
// many" demand for a job. Console populates Type and Count from the
// wizard alongside the chosen ModelTemplate; the planner combines
// this with the ModelDeploymentTemplate registry (which knows the
// per-template replica/group topology) to build the resolved
// ResourceRequirement.Groups[].
type AcceleratorRequirement struct {
	// Type is the accelerator SKU the user picked
	// (e.g. "H100-SXM"). The planner promotes this into the resolved
	// Groups[].PreferredAcceleratorTypes for each derived group.
	Type string `json:"type,omitempty"`
	// Count is the total number of accelerators the user requested
	// for the job. The planner divides this across replica groups
	// according to the ModelTemplate topology.
	Count int `json:"count,omitempty"`
}

// ResourceRequirement is the Console -> planner scheduling
// hint/requirement, distinct from the cross-team
// rmtypes.ResourceProvisionSpec the RM team owns - the rmadapter is
// the single point that translates between the two so the planner
// stays decoupled from the RM team's full schema.
//
// Most fields on this struct are planner-owned: Console populates
// only Accelerator (Type + Count) alongside PlannerJob.ModelTemplate;
// the planner derives Groups, TimeWindow, AllocationMode, Provider,
// and the per-provider placement blocks from those two inputs plus
// scheduling policy, and persists the resolved struct on the
// PlannerTask before any worker leases it.
type ResourceRequirement struct {
	// Accelerator is the Console-provided "what type and how many"
	// demand. The planner reads it together with ModelTemplate and
	// expands it into the resolved Groups[] before persisting.
	Accelerator AcceleratorRequirement `json:"accelerator,omitempty"`

	// AllocationMode selects spot vs on-demand vs scheduled. Empty
	// means "no preference / use backend default".
	AllocationMode ResourceAllocationMode `json:"allocation_mode,omitempty"`

	// Provider names which provisioner backend this request targets.
	// The rmadapter validates against its Provisioner.Type() when
	// non-empty and rejects mismatches with ErrInvalidJob; empty
	// passes through (test convenience, single-backend deployments).
	Provider ResourceProviderType `json:"provider,omitempty"`

	// Groups is one or more replica groups. A single-group request
	// covers most batch jobs; multi-group supports prefill/decode
	// disaggregation, parameter-server topologies, and other
	// heterogeneous placements.
	Groups []ResourceGroup `json:"groups,omitempty"`

	// TimeWindow optionally pins reservation start/end. Nil = use the
	// adapter's DefaultTTL for EndTime starting at now.
	TimeWindow *ReservationTimeWindow `json:"time_window,omitempty"`

	// Per-provider placement hints. Only the field matching Provider
	// is honored by the rmadapter; the others are ignored.
	Kubernetes  *KubernetesPlacement  `json:"kubernetes,omitempty"`
	AWS         *AWSPlacement         `json:"aws,omitempty"`
	LambdaCloud *LambdaCloudPlacement `json:"lambda_cloud,omitempty"`
}

// ResourceGroup is one replica group within a ResourceRequirement.
type ResourceGroup struct {
	// GpusPerReplica is the accelerator count each replica needs.
	GpusPerReplica int `json:"gpus_per_replica"`

	// Replicas defaults to 1 when zero.
	Replicas int `json:"replicas,omitempty"`

	// PreferredAcceleratorTypes is an ordered preference list, e.g.
	// ["H100-SXM", "H100-PCIe"]. The first available SKU wins.
	PreferredAcceleratorTypes []string `json:"preferred_accelerator_types,omitempty"`

	// GroupRole is optional ("prefill" / "decode" / etc); set only
	// for multi-group P/D disaggregation.
	GroupRole string `json:"group_role,omitempty"`
}

// ReservationTimeWindow optionally pins the start/end of a reservation.
// StartTime nil = "now"; EndTime nil = "now + adapter.DefaultTTL".
type ReservationTimeWindow struct {
	StartTime *time.Time `json:"start_time,omitempty"`
	EndTime   *time.Time `json:"end_time,omitempty"`
}

// KubernetesPlacement carries Kubernetes-specific placement hints.
// Only honored when ResourceRequirement.Provider ==
// ResourceProviderKubernetes.
type KubernetesPlacement struct {
	// Clusters / Contexts / Namespaces are required-set lists (any-of
	// match per dimension). Empty = no constraint on that dimension.
	Clusters   []string `json:"clusters,omitempty"`
	Contexts   []string `json:"contexts,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`

	// ReplicaAffinity is an ordered fallback list across the haiyang
	// affinity policies (numa > host > tor > minipod > bigpod).
	ReplicaAffinity []string `json:"replica_affinity,omitempty"`

	// NUMARequired forces NUMA topology awareness.
	NUMARequired bool `json:"numa_required,omitempty"`
}

// AWSPlacement carries AWS-specific placement hints. Only honored when
// ResourceRequirement.Provider == ResourceProviderAWS.
type AWSPlacement struct {
	Regions []string `json:"regions,omitempty"`
	Zones   []string `json:"zones,omitempty"`
}

// LambdaCloudPlacement carries Lambda Cloud-specific placement hints.
// Only honored when ResourceRequirement.Provider ==
// ResourceProviderLambdaCloud.
type LambdaCloudPlacement struct {
	Regions []string `json:"regions,omitempty"`
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
// Populated by the worker from Reservation.Allocations after the
// RM-side capacity request returns. AIBrixExtraBody.ResourceDetails
// carries a list because a single batch may legitimately span more
// than one slice — most directly when ResourceRequirement.Groups has
// more than one entry (multi-group / P/D-disaggregation), but also
// when a single group's reservation lands on multiple SKUs or
// clusters. The mapping from the input groups to output ResourceDetail
// entries is the rmadapter's responsibility.
//
// The wire shape is intentionally flat (no nested accelerator block) -
// this matches what MDS will consume. ResourceType carries the
// effective allocation mode for this slice (one of the
// ResourceAllocationMode values: "onDemand" / "spot" / "scheduled"),
// GPUType is the SKU (for example "H100-SXM"), and WorkerNum is the
// worker count for that slice.
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
// batches. MDS must persist and echo it for BatchStatus.JobID
// round-trips to work; see README.md "MDS correlation and dedup
// (hard external dependency)".
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
//	queued -> claimed -> submitted          (happy path)
//	             |
//	             +--> retryable_failure --> queued    (retry within attempt)
//	             |                       \-> terminal_failure (Nack budget)
//	             +--> terminal_failure              (non-retryable)
//	   submitted ----------------------> superseded
//	                                        (EnqueueContinuation: a new task
//	                                         with the same JobID was created
//	                                         because this attempt's reservation
//	                                         expired before MDS finished)
//
// The planner does not drive cancellation itself; the OpenAI Batches
// API exposes /cancel on MDS, and any user-facing cancel travels via
// MDS directly. The planner observes the resulting MDS terminal state
// at read time and surfaces it through JobView.LifecycleState
// (cancelling / cancelled).
//
// Each PlannerTask is one attempt at running a job. When a reservation
// expires and a new attempt is needed, the planner inserts a fresh
// PlannerTask (new TaskID, same JobID, Attempt+1) and transitions the
// old one to superseded. State transitions on a task are forward-only.
//
// Claim atomically transitions queued -> claimed and returns the
// matching PlannerTasks; Ack/Nack/Fail validate that the task is still
// in claimed before transitioning it forward, and return
// ErrTaskAlreadyTerminal otherwise. On Worker startup,
// TaskStore.RecoverInProgress resets every claimed row back to queued
// so any tasks the previous process was holding when it died can be
// picked up again.
//
// PlannerTaskState is the planner-internal coordination state. The
// user-facing JobLifecycleState is derived from this plus BatchStatus
// when JobView is assembled.
type PlannerTaskState string

const (
	// PlannerTaskStateQueued means the task has been accepted and is waiting
	// for a worker to claim it.
	PlannerTaskStateQueued PlannerTaskState = "queued"
	// PlannerTaskStateClaimed means a worker has atomically taken the task
	// off the queue and is preparing or performing the MDS submission.
	PlannerTaskStateClaimed PlannerTaskState = "claimed"
	// PlannerTaskStateSubmitted means MDS accepted the batch and returned a
	// batch ID. The MDS BatchStatus drives any further user-visible state.
	PlannerTaskStateSubmitted PlannerTaskState = "submitted"
	// PlannerTaskStateRetryableFailure means the most recent attempt failed
	// but the task may be retried later.
	PlannerTaskStateRetryableFailure PlannerTaskState = "retryable_failure"
	// PlannerTaskStateTerminalFailure means the task exhausted retries or
	// hit a non-recoverable validation/configuration error.
	PlannerTaskStateTerminalFailure PlannerTaskState = "terminal_failure"
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

	// Lifecycle timestamps. nil means "not yet".
	EnqueuedAt  *time.Time `json:"enqueued_at,omitempty"`
	AvailableAt *time.Time `json:"available_at,omitempty"`
	ClaimedAt   *time.Time `json:"claimed_at,omitempty"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	LastTriedAt *time.Time `json:"last_tried_at,omitempty"`
}
