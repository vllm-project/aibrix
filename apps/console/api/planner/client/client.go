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
// and the AIBrixExtraBody fields that feed extra_body.aibrix.*.
//
// The package also ships one concrete BatchClient implementation,
// OpenAIBatchClient, talking to the metadata service's
// OpenAI-compatible /v1/batches endpoint via the openai-go SDK.
package client

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"k8s.io/klog/v2"
)

// =============================================================================
// Interface
// =============================================================================

// BatchClient is the planner -> MDS adapter for all batch operations.
// It is the single mocking seam for MDS in tests: a fake BatchClient
// covers both the worker's submit path and the planner's read overlay.
//
// All methods return *openai.Batch directly (no planner-side wrapper):
// the wire shape is OpenAI-compatible and the planner has no overlay
// fields to add today. Re-introduce a thin wrapper here when planner
// state needs to ride alongside the MDS batch on read.
type BatchClient interface {
	CreateBatch(ctx context.Context, req *MDSBatchSubmission) (*openai.Batch, error)
	GetBatch(ctx context.Context, batchID string) (*openai.Batch, error)
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
	Data    []*openai.Batch `json:"data"`
	HasMore bool            `json:"has_more"`
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
// is not surfaced through the Planner interface. Console-facing
// errors live in the plannerapi package.
var ErrMDSSubmitFailed = errors.New("planner/client: submit failed")

// =============================================================================
// OpenAI-compatible BatchClient implementation
// =============================================================================

// OpenAIBatchClient implements BatchClient against an OpenAI-compatible
// /v1/batches endpoint (the metadata service). All AIBrix-namespaced
// fields ride along under the SDK's extra_body channel via
// option.WithJSONSet.
type OpenAIBatchClient struct {
	client openai.Client
}

// NewOpenAIBatchClient constructs a BatchClient pointed at the metadata
// service's base URL (without the trailing /v1).
func NewOpenAIBatchClient(metadataServiceURL string) *OpenAIBatchClient {
	baseURL := strings.TrimRight(metadataServiceURL, "/") + "/v1"
	c := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("aibrix-console"),
	)
	return &OpenAIBatchClient{client: c}
}

var _ BatchClient = (*OpenAIBatchClient)(nil)

func (c *OpenAIBatchClient) CreateBatch(ctx context.Context, req *MDSBatchSubmission) (*openai.Batch, error) {
	if req == nil {
		return nil, fmt.Errorf("openai batch client: nil request")
	}

	completionWindow := req.CompletionWindow
	if completionWindow == "" {
		completionWindow = string(openai.BatchNewParamsCompletionWindow24h)
	}
	params := openai.BatchNewParams{
		InputFileID:      req.InputFileID,
		Endpoint:         openai.BatchNewParamsEndpoint(req.Endpoint),
		CompletionWindow: openai.BatchNewParamsCompletionWindow(completionWindow),
	}
	if len(req.Metadata) > 0 {
		params.Metadata = req.Metadata
	}

	opts := buildExtraBodyOptions(req.ExtraBody.AIBrix)

	batch, err := c.client.Batches.New(ctx, params, opts...)
	if err != nil {
		// errors.Join preserves the inner *openai.Error so callers can
		// errors.As it for HTTP-status-derived gRPC codes, while still
		// matching ErrMDSSubmitFailed via errors.Is.
		return nil, errors.Join(ErrMDSSubmitFailed, err)
	}
	return batch, nil
}

func (c *OpenAIBatchClient) GetBatch(ctx context.Context, batchID string) (*openai.Batch, error) {
	if batchID == "" {
		return nil, fmt.Errorf("openai batch client: empty batch ID")
	}
	return c.client.Batches.Get(ctx, batchID)
}

func (c *OpenAIBatchClient) ListBatches(ctx context.Context, req *ListBatchesRequest) (*ListBatchesResponse, error) {
	params := openai.BatchListParams{}
	if req != nil {
		if req.After != "" {
			params.After = openai.String(req.After)
		}
		if req.Limit > 0 {
			params.Limit = openai.Int(int64(req.Limit))
		}
	}
	page, err := c.client.Batches.List(ctx, params)
	if err != nil {
		return nil, err
	}
	batches := make([]*openai.Batch, 0, len(page.Data))
	for i := range page.Data {
		batches = append(batches, &page.Data[i])
	}
	return &ListBatchesResponse{Data: batches, HasMore: page.HasMore}, nil
}

// buildExtraBodyOptions projects AIBrixExtraBody fields into
// option.WithJSONSet calls so the openai-go SDK serializes them at the
// top level of POST /v1/batches under "aibrix.*". MDS reads them out of
// BatchSpec.aibrix; only the keys it declares are accepted (MDS-side
// AibrixExtension is extra=forbid, see
// python/aibrix/aibrix/metadata/api/v1/batch.py).
//
// Fields the planner computes but MDS does not yet accept (job_id,
// planner_decision) are intentionally NOT emitted here. They are
// logged for verification; flip them onto the wire (one
// option.WithJSONSet per key) once MDS adds them to AibrixExtension.
func buildExtraBodyOptions(eb AIBrixExtraBody) []option.RequestOption {
	logSuppressedAibrixFields(eb)

	var opts []option.RequestOption
	if eb.ModelTemplate != nil && eb.ModelTemplate.Name != "" {
		opts = append(opts, option.WithJSONSet("aibrix.model_template.name", eb.ModelTemplate.Name))
		if eb.ModelTemplate.Version != "" {
			opts = append(opts, option.WithJSONSet("aibrix.model_template.version", eb.ModelTemplate.Version))
		}
	}
	return opts
}

// logSuppressedAibrixFields prints the AIBrix extension fields the
// planner has computed but the BatchClient is suppressing on the wire.
// One INFO line covers the always-on summary; klog.V(2) prints the full
// computed values for deep-dive verification.
func logSuppressedAibrixFields(eb AIBrixExtraBody) {
	hasJobID := eb.JobID != ""
	hasPlannerDecision := eb.PlannerDecision != nil
	if !hasJobID && !hasPlannerDecision {
		return
	}
	klog.Infof("[planner.client] aibrix fields suppressed on wire (MDS extra=forbid): job_id=%t planner_decision=%t",
		hasJobID, hasPlannerDecision)
	if !klog.V(2).Enabled() {
		return
	}
	if hasJobID {
		klog.V(2).Infof("[planner.client] suppressed aibrix.job_id=%q", eb.JobID)
	}
	if hasPlannerDecision {
		klog.V(2).Infof("[planner.client] suppressed aibrix.planner_decision=%+v", *eb.PlannerDecision)
	}
}
