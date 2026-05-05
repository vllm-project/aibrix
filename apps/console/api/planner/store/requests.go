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

package store

import (
	"time"

	"github.com/vllm-project/aibrix/apps/console/api/planner/task"
)

// DequeueRequest is the worker -> queue API for atomically transitioning
// tasks from queued to claimed. It unifies the prior Claim / ClaimByID
// split:
//
//   - TaskIDs == nil : FCFS convenience path. The queue applies its
//     default ordering (typically `available_at` ascending) and claims
//     up to `Limit` candidates in a single round-trip.
//   - TaskIDs != nil : policy-aware path. The queue atomically claims
//     only the specified IDs (typically the output of a SchedulerFunc
//     acting on PeekRequest candidates). IDs that are no longer
//     claimable are silently skipped; the response contains only the
//     subset that was successfully claimed. The worker treats
//     `len(returned) < len(requested)` as a normal race outcome.
//
// Tasks held in claimed across a process crash are reset to queued
// by TaskQueue.Recover on the next Worker startup.
type DequeueRequest struct {
	WorkerID string `json:"worker_id"`
	// Limit caps how many tasks the queue may claim when TaskIDs is
	// nil. Ignored when TaskIDs is non-nil.
	Limit int `json:"limit,omitempty"`
	// TaskIDs, when non-nil, restricts the claim to exactly these IDs.
	// When nil, the queue picks by its default ordering.
	TaskIDs []string `json:"task_ids,omitempty"`
}

// PeekRequest queries the queue for tasks that are currently claimable
// without acquiring them. Used by SchedulerFunc implementations to
// compute their ranking before committing a selection via
// TaskQueue.Dequeue(TaskIDs=...).
type PeekRequest struct {
	// Limit caps the number of candidates returned. Policies typically
	// overscan (request more than they intend to claim) so the ranking
	// has room to maneuver.
	Limit int `json:"limit,omitempty"`
	// Now is the reference time for "available_at <= Now" filtering.
	// Pass time.Now() unless you have a deterministic-time test setup.
	Now time.Time `json:"now"`
}

// AckRequest records a successful submission to MDS.
//
// The store transitions the task identified by TaskID from claimed to
// submitted. ProvisionID and ProvisionExpiresAt are persisted on the
// PlannerTask so the provision-expiry sweeper can find submitted
// tasks whose RM provision has expired; both are zero-valued when the
// worker stack is running without an RM (e.g. in unit tests).
//
// ProvisionExpiresAt is derived from
// rmtypes.ResourceProvisionSpec.TimeWindow.EndTime (the spec the
// planner submitted to RM.Provision) — rmtypes.ProvisionResult has
// no explicit expiry field. See mds.Provision.ExpiresAt for the
// canonical mapping note.
//
// Returns ErrTaskAlreadyTerminal if the task is no longer in claimed
// (typically because a concurrent CancelTask transitioned it to
// cancelled mid-submit); the worker drops the in-flight result.
type AckRequest struct {
	TaskID             string     `json:"task_id"`
	BatchID            string     `json:"batch_id"`
	SubmittedAt        time.Time  `json:"submitted_at"`
	ProvisionID        string     `json:"provision_id,omitempty"`
	ProvisionExpiresAt *time.Time `json:"provision_expires_at,omitempty"`
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
// retries a submitted task whose RM provision expired before MDS
// finished the batch. The store atomically:
//
//  1. transitions the task identified by SupersededTaskID from
//     submitted to superseded (preserving its BatchID for audit), and
//  2. inserts NewTask as a fresh PlannerTask in queued state with
//     Attempt = supersededTask.Attempt + 1 and the same JobID.
//
// The new task starts with empty BatchID and empty ProvisionID; the
// next worker that claims it calls RM.Provision to obtain a fresh
// provision. RM idempotency is keyed on TaskID, so the new TaskID
// guarantees a fresh provision.
//
// Supersede is initiated by a planner-internal sweeper, not by a
// worker; the superseded task is already in submitted (no longer
// claimed) by the time the sweeper looks at it.
//
// Returns plannerapi.ErrJobNotFound if SupersededTaskID does not
// exist. Returns ErrTaskAlreadyTerminal if the superseded task is not
// in submitted (e.g. raced with MDS-side completion); callers may
// treat this as a benign no-op. The store enforces JobID consistency
// between NewTask.JobID and the superseded task's JobID.
//
// The companion BatchClient.CancelBatch call against MDS is the
// caller's responsibility; EnqueueContinuationRequest only records the
// planner-side state transition.
type EnqueueContinuationRequest struct {
	SupersededTaskID string            `json:"superseded_task_id"`
	NewTask          *task.PlannerTask `json:"new_task"`
	Reason           string            `json:"reason,omitempty"`
	SupersededAt     time.Time         `json:"superseded_at"`
}
