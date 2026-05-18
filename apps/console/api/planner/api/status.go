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

// JobStatus is the authoritative user-facing lifecycle status for a Job.
// 13 values across three segments:
//   - Pre-submit (Planner-driven): Queued, ResourcePreparing, Submitting.
//   - Post-submit (mirrors openai.Batch.status 1:1): Validating, InProgress,
//     Finalizing, Cancelling.
//   - Terminal: Completed, Failed, Expired, Cancelled, ResourceFailed,
//     SubmitFailed.
//
// See docs/source/designs/batch-job-state-machine.md §5.
type JobStatus string

const (
	JobStatusQueued            JobStatus = "queued"
	JobStatusResourcePreparing JobStatus = "resource_preparing"
	JobStatusSubmitting        JobStatus = "submitting"

	JobStatusValidating JobStatus = "validating"
	JobStatusInProgress JobStatus = "in_progress"
	JobStatusFinalizing JobStatus = "finalizing"
	JobStatusCancelling JobStatus = "cancelling"

	JobStatusCompleted      JobStatus = "completed"
	JobStatusFailed         JobStatus = "failed"
	JobStatusExpired        JobStatus = "expired"
	JobStatusCancelled      JobStatus = "cancelled"
	JobStatusResourceFailed JobStatus = "resource_failed"
	JobStatusSubmitFailed   JobStatus = "submit_failed"
)

// IsTerminal reports whether the job will not transition further.
func (s JobStatus) IsTerminal() bool {
	switch s {
	case JobStatusCompleted,
		JobStatusFailed,
		JobStatusExpired,
		JobStatusCancelled,
		JobStatusResourceFailed,
		JobStatusSubmitFailed:
		return true
	}
	return false
}
