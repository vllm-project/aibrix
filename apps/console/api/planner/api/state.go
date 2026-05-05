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

// PlannerTaskState is the planner-internal lifecycle of a queued task.
// It is exposed to Console on EnqueueResult.State and JobView.PlannerState
// so callers can distinguish pre-submit states (queued / claimed) from
// the post-submit MDS-driven view.
//
// MVP state machine (purely forward; no backward edges):
//
//	queued -> claimed -> submitted          (happy path)
//	             |
//	             +--> retryable_failure --> queued    (retry within attempt)
//	             |                       \-> terminal_failure (Nack budget)
//	             +--> terminal_failure              (non-retryable)
//	   submitted ----------------------> superseded
//	                                        (a new task with the same JobID
//	                                         was created because this attempt's
//	                                         provision expired before MDS finished)
//
// The planner does not drive cancellation itself; the OpenAI Batches
// API exposes /cancel on MDS, and any user-facing cancel travels via
// MDS directly. The planner observes the resulting MDS terminal state
// at read time and surfaces it through JobView.LifecycleState
// (cancelling / cancelled).
//
// Each PlannerTask is one attempt at running a job. When a provision
// expires and a new attempt is needed, the planner inserts a fresh
// PlannerTask (new TaskID, same JobID, Attempt+1) and transitions the
// old one to superseded. State transitions on a task are forward-only.
//
// PlannerTaskState is the planner-internal coordination state. The
// user-facing JobLifecycleState is derived from this plus the MDS
// batch status when JobView is assembled.
type PlannerTaskState string

const (
	// PlannerTaskStateQueued means the task has been accepted and is waiting
	// for a worker to claim it.
	PlannerTaskStateQueued PlannerTaskState = "queued"
	// PlannerTaskStateClaimed means a worker has atomically taken the task
	// off the queue and is preparing or performing the MDS submission.
	PlannerTaskStateClaimed PlannerTaskState = "claimed"
	// PlannerTaskStateSubmitted means MDS accepted the batch and returned a
	// batch ID. The MDS batch status drives any further user-visible state.
	PlannerTaskStateSubmitted PlannerTaskState = "submitted"
	// PlannerTaskStateRetryableFailure means the most recent attempt failed
	// but the task may be retried later.
	PlannerTaskStateRetryableFailure PlannerTaskState = "retryable_failure"
	// PlannerTaskStateTerminalFailure means the task exhausted retries or
	// hit a non-recoverable validation/configuration error.
	PlannerTaskStateTerminalFailure PlannerTaskState = "terminal_failure"
	// PlannerTaskStateSuperseded means this attempt was abandoned because
	// its RM provision expired before MDS finished, and a continuation
	// task (same JobID, higher Attempt) was inserted to retry the work
	// under a fresh provision. The BatchID on the superseded task is
	// preserved for audit; the live work is on the continuation.
	PlannerTaskStateSuperseded PlannerTaskState = "superseded"
)

// JobLifecycleState is the user-facing unified job status.
//
// This is the typed enum the Console UI renders. It is **derived** at read
// time by merging PlannerTaskState (when no BatchID is set) or the MDS
// batch status string from BatchView.Batch.Status (when a BatchID is
// set) into one of the values below. JobView.LifecycleState carries
// the result.
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
