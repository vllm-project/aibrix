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

// =============================================================================
// Job-lifecycle requests and responses (Enqueue / GetJob / ListJobs)
// =============================================================================

// EnqueueRequest is the Console -> planner contract for accepting a new job.

type EnqueueRequest struct {
	// JobID is the user-visible Console job identifier. Console generates
	// it (typically as a UUID) and the planner uses it as the primary
	// correlation key — Planner.GetJob and ListJobs surface it back to
	// Console as pb.Job.Id, distinct from the MDS-side batch ID.
	JobID string `json:"job_id"`
	// ModelTemplate is the authoritative Console-selected
	// ModelDeploymentTemplate reference. The planner projects it into
	// extra_body.aibrix.model_template when submitting to MDS.
	ModelTemplate *ModelTemplateRef `json:"model_template,omitempty"`
	// BatchParams is the OpenAI-batch-format submission MDS will execute,
	// in the openai-go SDK's native shape. Console-owned attribution
	// (created_by, display_name, ...) rides on BatchParams.Metadata under
	// the aibrix.console.* namespace and is the single source of truth
	// for read-back via GetJob.
	BatchParams openai.BatchNewParams `json:"batch_params"`
}

// Job is the planner's JobID-keyed result, returned from Enqueue,
// GetJob, Cancel, and each entry of ListJobs.
//
// JobID is the Console-generated correlation key and the only id
// that crosses the planner boundary upward. The MDS-side batch.ID
// is an internal implementation detail of the planner and is not
// exposed here; callers above the planner read job.Batch.ID only
// for rendering MDS-native fields, never for lookups.
//
// Batch is the MDS-side openai.Batch when the planner has submitted
// to MDS. Future queued planners that defer the MDS submit may
// return Batch == nil on Enqueue and rely on the caller polling
// GetJob.
type Job struct {
	JobID string        `json:"job_id"`
	Batch *openai.Batch `json:"batch,omitempty"`
}

// ListJobsRequest queries the planner-merged job list using the same
// cursor semantics as the OpenAI Batches list API: Limit controls page
// size and After carries the trailing batch ID from the previous page.
//
// Keep this request shape aligned with the upstream list contract unless
// the planner grows planner-owned filters that cannot be expressed at the
// MDS / OpenAI layer.
type ListJobsRequest struct {
	Limit int    `json:"limit,omitempty"`
	After string `json:"after,omitempty"`
}

// ListJobsResponse is the planner-facing paginated read result.
//
// Each entry is a Job so the JobID rides alongside the MDS batch
// view; HasMore mirrors the OpenAI SDK's batch list page semantics.
type ListJobsResponse struct {
	Data    []*Job `json:"data"`
	HasMore bool   `json:"has_more"`
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

