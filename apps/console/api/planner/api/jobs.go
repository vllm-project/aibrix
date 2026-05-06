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
	// BatchPayload is the OpenAI-batch-format submission body MDS will execute.
	// Console-owned attribution (created_by, display_name, ...) rides on
	// BatchPayload.Metadata under the aibrix.console.* namespace and is the
	// single source of truth for read-back via GetJob.
	BatchPayload BatchPayload `json:"batch_payload"`
}

// EnqueueResult is the planner's response to a successful Enqueue.
//
// JobID echoes the user-facing identity from the request so callers
// don't have to carry it across the boundary themselves.
//
// Batch is the MDS-side openai.Batch when the planner submits to MDS
// inline (passthrough mode). Future queued planners that defer the
// MDS submit may return Batch == nil and rely on the caller polling
// GetJob — that's why this is a wrapper rather than just *openai.Batch
// directly.
type EnqueueResult struct {
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
// Data and HasMore intentionally mirror the OpenAI SDK's batch list page
// fields so the planner can stay OpenAI-compatible while still owning a
// stable boundary type. ListJobs forwards MDS batches verbatim; planner-
// side JobID overlay is not attached here because the passthrough planner
// does not maintain a durable JobID -> BatchID mapping yet.

type ListJobsResponse struct {
	Data    []*openai.Batch `json:"data"`
	HasMore bool            `json:"has_more"`
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

// =============================================================================
// OpenAI-batch payload
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
