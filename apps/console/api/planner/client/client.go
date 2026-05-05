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

// Package client defines the planner -> Metadata Service adapter: the
// BatchClient interface, the request/response shapes its methods carry,
// the MDSBatchSubmission a worker builds before calling CreateBatch,
// and the in-memory Provision that feeds extra_body.aibrix.* fields.
//
// The Console-facing read view (BatchView) lives in plannerapi because
// Planner.GetJob / ListJobs surface it through JobView.
package client

import (
	"context"
	"errors"

	"github.com/vllm-project/aibrix/apps/console/api/planner/api"
)

// =============================================================================
// Interface
// =============================================================================

// BatchClient is the planner -> MDS adapter for all batch operations.
// It is the single mocking seam for MDS in tests: a fake BatchClient
// covers both the worker's submit path and the planner's read overlay.
//
// All methods return *plannerapi.BatchView, which embeds openai-go's
// *Batch so the wire shape stays OpenAI-compatible (every openai.Batch
// field is promoted to the top level under its canonical OpenAI JSON
// key) and adds one planner-specific field: JobID, the correlation
// key with PlannerTask.
//
// CreateBatch is called by the worker to submit a prepared
// MDSBatchSubmission (built from the claimed PlannerTask plus the
// in-memory Provision). 
//
// GetBatch is used by Planner.GetJob/ListJobs to overlay live MDS
// state onto JobView and by the worker for pre-submit dedup.
//
// The primary correlation key between planner tasks and MDS batches is
// extra_body.aibrix.job_id. BatchClient implementations should populate
// BatchView.JobID from however MDS echoes that field (typically the
// response Metadata map) so higher layers do not need to understand
// raw MDS payload shapes. Until MDS persists and echoes that field (a
// documented hard dependency), JobID may come back empty.
//
// ListBatches returns one page of batches keyed by MDS-side cursor. It
// powers Planner.ListJobs's MDS overlay and is also the building block
// for the worker pre-submit dedup scan once MDS exposes a job_id index.
type BatchClient interface {
	CreateBatch(ctx context.Context, req *MDSBatchSubmission) (*plannerapi.BatchView, error)
	GetBatch(ctx context.Context, batchID string) (*plannerapi.BatchView, error)
	ListBatches(ctx context.Context, req *ListBatchesRequest) (*ListBatchesResponse, error)
}


// =============================================================================
// ListBatches request / response
// =============================================================================

// ListBatchesRequest is the planner -> MDS paginated read for the batch
// list endpoint. The cursor semantics match MDS (and OpenAI): pass the
// last batch ID from the previous page as After.
type ListBatchesRequest struct {
	// Limit caps the page size. Zero means "use the upstream default"
	// (typically 20).
	Limit int `json:"limit,omitempty"`
	// After is the cursor returned (implicitly, as the trailing batch
	// ID) by a prior page. Empty means "first page".
	After string `json:"after,omitempty"`
}

// ListBatchesResponse is the normalized MDS list payload. The slice is
// in upstream order (newest-first by MDS convention); cursor advancement
// is the caller's responsibility - re-issue ListBatches with
// After = Data[len(Data)-1].ID until HasMore is false.
type ListBatchesResponse struct {
	Data    []*plannerapi.BatchView `json:"data"`
	HasMore bool                    `json:"has_more"`
}

// =============================================================================
// Sentinel errors
// =============================================================================

// ErrMDSSubmitFailed indicates submitting the OpenAI batch to MDS
// failed. The Worker wraps upstream BatchClient.CreateBatch errors
// with this sentinel when the failure occurred after planning but
// before a batch ID was durably recorded, so callers can route on
// errors.Is without parsing transport-specific error strings.
//
// This is a planner-internal sentinel (worker -> store boundary); it
// is not surfaced through plannerapi.Planner. Console-facing errors
// live in plannerapi/errors.go.
var ErrMDSSubmitFailed = errors.New("planner/client: submit failed")
