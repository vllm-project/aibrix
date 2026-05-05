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

// Package task defines the planner-internal durable unit (PlannerTask).
//
// PlannerTask is the only persistent type in the planner: each row is one
// attempt at running a job, identified per-attempt by TaskID and per-job by
// JobID. Console-supplied "spec" fields (Accelerator, ProvisionSpec,
// ModelTemplate, Profile, Payload, Priority, CreatedBy) are persisted
// directly on PlannerTask — there is no separate "PlannerJob" record. The
// first row for a JobID is constructed from plannerapi.EnqueueRequest at
// Enqueue time; continuations are inserted via
// store.TaskLifecycle.Supersede.
//
// Worker-side retry bookkeeping (Attempts, MaxAttempts) lives on the
// PlannerTask too but is owned by the planner/worker, not by Console: the
// planner stamps MaxAttempts from its default policy on insert, and the
// worker increments Attempts on every Nack.
package task

import (
	"time"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// PlannerTask is the durable unit stored in the planner store. Each
// PlannerTask is one attempt at running a job: TaskID is the per-attempt
// identity; JobID is the user-facing identity that may have multiple
// attempts (rows) chained via Attempt.
//
// Console enqueues the first PlannerTask (Attempt = 1) derived from
// EnqueueRequest. A worker later claims it, executes it, and writes back
// the BatchID and retry state on Ack. If that attempt's provision
// expires before MDS finishes, the planner-internal sweeper inserts a
// continuation PlannerTask via store.TaskLifecycle.Supersede (new TaskID,
// same JobID, Attempt+1) and transitions the prior row to superseded.
// The chain is reconstructable via store.TaskRepository.ListTasksByJobID.
type PlannerTask struct {
	TaskID    string `json:"task_id"`
	JobID     string `json:"job_id"`
	CreatedBy string `json:"created_by,omitempty"`

	// Attempt is the 1-based position of this row in the JobID's
	// attempt chain. The first task Console enqueues has Attempt = 1;
	// each Supersede insert derives Attempt = priorTask.Attempt + 1.
	// State transitions on a single PlannerTask are forward-only;
	// retries that change Attempt always create new rows.
	Attempt int `json:"attempt"`

	State plannerapi.PlannerTaskState `json:"state"`

	// Spec fields, copied from EnqueueRequest at insert time. Console
	// populates Accelerator; the planner expands it into
	// ProvisionSpec.Groups during Enqueue and may overwrite the
	// resolved ProvisionSpec (Credential, TimeWindow, placement) at
	// any point before a worker leases the task.
	Accelerator   plannerapi.AcceleratorRequirement `json:"accelerator"`
	ProvisionSpec rmtypes.ResourceProvisionSpec     `json:"provision_spec"`
	ModelTemplate *plannerapi.ModelTemplateRef      `json:"model_template,omitempty"`
	Profile       *plannerapi.ProfileRef            `json:"profile,omitempty"`
	Payload       plannerapi.BatchPayload           `json:"payload"`
	Priority      int                               `json:"priority,omitempty"`

	// ProvisionID, if non-empty, is the active RM-side provision
	// held for this attempt. The worker carries it in-memory between
	// the RM call and Ack, then Ack persists it on the PlannerTask.
	// The RM-side capacity-request path is idempotent per TaskID, so
	// a re-claiming worker after a crash gets the same Provision
	// back rather than a new slot.
	ProvisionID string `json:"provision_id,omitempty"`

	// ProvisionExpiresAt mirrors Provision.ExpiresAt at Ack time, so
	// the provision-expiry sweeper can find attempts whose RM provision
	// has expired without round-tripping to RM.

	ProvisionExpiresAt *time.Time `json:"provision_expires_at,omitempty"`

	// BatchID is populated only after a successful submission to MDS.
	// It is immutable for the lifetime of this task: retries triggered
	// by provision expiry insert a new task (continuation) instead of
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
