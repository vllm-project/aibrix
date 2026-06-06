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
	"github.com/openai/openai-go/v3"
)

// JobStatus is the authoritative user-facing lifecycle status for a Job.
type JobStatus string

const (
	JobStatusQueued            JobStatus = "queued"
	JobStatusPlanned           JobStatus = "planned"
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

func (s JobStatus) ToBatchStatus() openai.BatchStatus {
	switch s {
	case JobStatusResourceFailed, JobStatusSubmitFailed:
		return openai.BatchStatusFailed
	case JobStatusCancelled:
		return openai.BatchStatusCancelled
	case JobStatusExpired:
		return openai.BatchStatusExpired
	}
	return openai.BatchStatus(string(s))
}
