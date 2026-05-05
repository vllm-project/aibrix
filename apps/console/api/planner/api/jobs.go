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

package plannerapi

import (
	"time"

	"github.com/openai/openai-go/v3"
)

// =============================================================================
// Job-lifecycle requests and responses (Enqueue / GetJob / ListJobs)
// =============================================================================

// AcceleratorRequirement is the Console-provided "what SKU and how
// many" demand for a job. Console populates Type and Count from the
// wizard alongside the chosen ModelTemplate; the planner combines
// this with the ModelDeploymentTemplate registry (which knows the
// per-template replica/group topology) to expand it into the
// rmtypes.ResourceProvisionSpec it persists on PlannerTask before
// any worker leases the task.
//
// This is the only resource-shaped type the planner still owns
// itself: rmtypes.ResourceProvisionSpec (see
// apps/console/api/resource_manager/types) expects already-expanded
// per-replica group shapes and has no first-class slot for
// "user-picked SKU + total count". When RM exposes an equivalent
// input slot the planner can adopt it and this struct goes away.
type AcceleratorRequirement struct {
	// Type is the accelerator SKU the user picked
	// (e.g. "H100-SXM"). The planner promotes this into
	// AcceleratorPreference.PreferredTypes on each derived group of
	// the resolved ResourceProvisionSpec persisted on PlannerTask.
	Type string `json:"type,omitempty"`
	// Count is the total number of accelerators the user requested
	// for the job. The planner divides this across replica groups
	// according to the ModelTemplate topology.
	Count int `json:"count,omitempty"`
}

// EnqueueRequest is the Console -> planner contract for accepting a new job.
//
// All Console-supplied fields the planner needs to derive a PlannerTask
// land directly on this struct: the user-visible JobID, the accelerator
// demand Console picked in the wizard, the chosen ModelDeploymentTemplate
// and BatchProfile, and the OpenAI-batch payload MDS will eventually
// execute. There is no nested "job" wrapper; the planner persists these
// fields on the first PlannerTask (Attempt = 1) for the JobID and reads
// them back from there.
//
// Resource shape: Console supplies only Accelerator. The planner
// expands Accelerator + ModelTemplate into the full
// rmtypes.ResourceProvisionSpec internally (filling Groups,
// Credential, TimeWindow from policy) and persists the resolved spec
// on PlannerTask. ResourceProvisionSpec is intentionally NOT on the
// wire — keeping it client-supplied would invite credential/policy
// injection and couples the Console contract to RM schema changes.
// The worker hands PlannerTask.ProvisionSpec to the RM verbatim at
// lease time.
//

type EnqueueRequest struct {
	// JobID is the user-visible Console job identifier.
	JobID string `json:"job_id"`
	// CreatedBy is the Console end-user identity that created the job.
	// The Console stamps it from the authenticated request context
	// (see handler.currentUserEmail); the planner treats it as opaque
	// audit metadata and does not interpret it.
	CreatedBy string `json:"created_by,omitempty"`
	// Priority is the cross-job ordering hint: higher values are leased
	// before lower ones when a priority-aware SchedulerFunc is wired
	// in. It is a product-level concept (how important is this job
	// relative to others), not a scheduler-implementation detail.
	//
	// MVP status: unused, but kept on the wire as a forward-compatible
	// extension point. The MVP runs a single class of work (batch
	// jobs) under FCFS, so every task is effectively equal-priority
	// and Console does not populate this field. When a future release
	// introduces mixed workloads (interactive vs. scheduled vs. batch)
	// or an Ops override surface, Console can start sending non-zero
	// values and a priority-aware SchedulerFunc will honor them
	// without any wire-format change.
	Priority int `json:"priority,omitempty"`
	// Accelerator is the Console-provided "what SKU and how many"
	// demand. The planner reads it together with ModelTemplate and
	// expands it into the resolved ResourceProvisionSpec persisted on
	// PlannerTask; the worker never sees Accelerator directly.
	Accelerator AcceleratorRequirement `json:"accelerator"`
	// ModelTemplate is the authoritative Console-selected
	// ModelDeploymentTemplate reference. The worker projects it into
	// extra_body.aibrix.model_template when submitting to MDS.
	ModelTemplate *ModelTemplateRef `json:"model_template,omitempty"`
	// Profile is the authoritative Console-selected BatchProfile reference.
	// The worker projects it into extra_body.aibrix.profile when submitting.
	Profile *ProfileRef `json:"profile,omitempty"`
	// BatchPayload is the OpenAI-batch-format submission body MDS will execute.
	BatchPayload BatchPayload `json:"batch_payload"`
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
	Limit int `json:"limit,omitempty"`
	// After is the pagination cursor: pass the JobID of the last item
	// from the previous page. Empty means "first page".
	// Example: "batch_demo_27a6ee2c".
	After string `json:"after,omitempty"`
}

// ListJobsResponse is the paginated Console-facing planner read result.
//
// Data is in newest-first order. Re-issue ListJobs with After = NextAfter
// (equivalently After = Data[len(Data)-1].JobID) until HasMore is false.
// NextAfter is empty when HasMore is false.
//
// Example pagination flow (page size = 2):
//
//	// Request 1: first page
//	req:  {"limit": 2, "after": ""}
//	resp: {
//	  "data":       [ {"job_id": "batch_demo_aaa"}, {"job_id": "batch_demo_bbb"} ],
//	  "has_more":   true,
//	  "next_after": "batch_demo_bbb"
//	}
//
//	// Request 2: feed previous next_after into after
//	req:  {"limit": 2, "after": "batch_demo_bbb"}
//	resp: {
//	  "data":       [ {"job_id": "batch_demo_ccc"}, {"job_id": "batch_demo_ddd"} ],
//	  "has_more":   true,
//	  "next_after": "batch_demo_ddd"
//	}
//
//	// Request 3: last page
//	req:  {"limit": 2, "after": "batch_demo_ddd"}
//	resp: {
//	  "data":       [ {"job_id": "batch_demo_eee"} ],
//	  "has_more":   false,
//	  "next_after": ""
//	}
type ListJobsResponse struct {
	Data      []*JobView `json:"data"`
	HasMore   bool       `json:"has_more"`
	NextAfter string     `json:"next_after,omitempty"`
}

// JobView is the merged planner + MDS read model returned to Console.
//
// Planner-owned fields (TaskID, JobID, PlannerState, LifecycleState,
// Attempts, EnqueuedAt, etc.) describe the planner-internal lifecycle.
// The embedded *BatchView (when set) carries the live MDS overlay:
// every openai.Batch field plus the planner-side JobID. Field promotion
// lets consumers reach MDS-side data directly (for example
// view.Batch.Status, view.Batch.OutputFileID) without unwrapping.
//
// LifecycleState is the typed user-facing state the UI renders. It is
// derived at read time:
//
//   - if the task has no BatchID, LifecycleState is derived from
//     PlannerState (queued / dispatching / submitted / failed);
//   - if the task has a BatchID, LifecycleState is derived from the MDS
//     batch status string (validating / in_progress / finalizing /
//     completed / failed / expired / cancelling / cancelled), available
//     via Batch.Status on the embedded BatchView.
//
// JobView is computed by the Planner implementation; it is not stored.
// The Batch field is nil before submission and populated by the
// read-time MDS overlay once BatchID is set on the underlying
// PlannerTask.
type JobView struct {
	TaskID         string            `json:"task_id"`
	JobID          string            `json:"job_id"`
	CreatedBy      string            `json:"created_by,omitempty"`
	PlannerState   PlannerTaskState  `json:"planner_state"`
	LifecycleState JobLifecycleState `json:"lifecycle_state"`

	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"max_attempts"`
	LastError   string `json:"last_error,omitempty"`

	EnqueuedAt  *time.Time `json:"enqueued_at,omitempty"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`

	// Batch is the live MDS overlay. Nil before the task transitions
	// to submitted; otherwise populated from BatchClient.GetBatch on
	// every read.
	Batch *BatchView `json:"batch,omitempty"`
}

// =============================================================================
// Named references resolved by MDS at render time
// =============================================================================

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

// =============================================================================
// OpenAI-batch payload and read-model overlay
// =============================================================================

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

// BatchView is the planner's read model of an MDS batch. It is the
// OpenAI batch object plus the planner-specific JobID correlation key
// that ties an MDS batch back to its PlannerTask.
//
// The embedded *openai.Batch is the same struct openai-go returns from
// Batches.Get / Batches.New / Batches.List, so BatchView is wire-format
// compatible with OpenAI's batch object — every field on openai.Batch
// is promoted to the top level (ID, Status, OutputFileID, CreatedAt,
// RequestCounts, etc.) and serializes under the same JSON keys. A
// caller that knows the OpenAI batch shape can deserialize a BatchView
// payload by just ignoring the extra "job_id" key.
//
// JobID is the only planner-side addition. It is extracted from
// extra_body.aibrix.job_id (or whatever shape MDS uses to echo the
// correlation key back) by the BatchClient adapter when it constructs
// the BatchView. JobID may be empty until MDS implements the
// round-trip dependency described in README.md "MDS correlation and
// dedup (hard external dependency)".
//
// The *openai.Batch pointer is required (non-nil) on every BatchView
// returned by BatchClient. Consumers can rely on field-promotion
// access (e.g. view.Status, view.OutputFileID, view.RequestCounts)
// without nil-checking the embedded pointer.
type BatchView struct {
	*openai.Batch
	JobID string `json:"job_id,omitempty"`
}
