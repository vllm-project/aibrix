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
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/vllm-project/aibrix/apps/console/api/common"
	"github.com/vllm-project/aibrix/apps/console/api/error_injection"
	"github.com/vllm-project/aibrix/apps/console/api/metrics"
	"github.com/vllm-project/aibrix/apps/console/api/utils"
)

const (
	metricConsolePlannerError    = "console.planner.error"
	metricConsolePlannerDuration = "console.planner.duration"
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
	CreateBatch(ctx context.Context, params openai.BatchNewParams, aibrix AIBrixExtraBody) (*openai.Batch, error)
	GetBatch(ctx context.Context, batchID string) (*openai.Batch, error)
	CancelBatch(ctx context.Context, batchID string) (*openai.Batch, error)
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
	client   openai.Client
	injector error_injection.Injector
}

// NewOpenAIBatchClient constructs a BatchClient pointed at the metadata
// service's base URL (without the trailing /v1). The HTTP transport is
// wrapped with a shared logging transport so BFF↔MDS request/response bodies
// surface at klog -v=2 (4xx/5xx always log at info).
func NewOpenAIBatchClient(metadataServiceURL string, injector error_injection.Injector) *OpenAIBatchClient {
	baseURL := strings.TrimRight(metadataServiceURL, "/") + "/v1"
	httpClient := &http.Client{Transport: &utils.LoggingTransport{
		Base:  http.DefaultTransport,
		Label: "PLANNER->MDS",
	}}
	c := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("aibrix-console"),
		option.WithHTTPClient(httpClient),
	)
	return &OpenAIBatchClient{client: c, injector: injector}
}

var _ BatchClient = (*OpenAIBatchClient)(nil)

func (c *OpenAIBatchClient) CreateBatch(ctx context.Context, params openai.BatchNewParams, aibrix AIBrixExtraBody) (*openai.Batch, error) {
	// CheckPoint: batch_client.create_batch
	if c.injector != nil {
		if err := c.injector.CheckPoint(ctx, error_injection.POINT_BATCH_CLIENT_CREATE_BATCH); err != nil {
			return nil, err
		}
	}

	start := time.Now().UTC()
	opts := buildExtraBodyOptions(aibrix)
	batch, err := c.client.Batches.New(ctx, params, opts...)
	if err != nil {
		metrics.Emitter.Counter(metricConsolePlannerError, 1, metrics.T("method", "create_batch"))
		// errors.Join preserves the inner *openai.Error so callers can
		// errors.As it for HTTP-status-derived gRPC codes, while still
		// matching ErrMDSSubmitFailed via errors.Is.
		return nil, errors.Join(ErrMDSSubmitFailed, err)
	}
	metrics.Duration(metrics.Emitter, metricConsolePlannerDuration, start, metrics.T("method", "create_batch"))
	return batch, nil
}

func (c *OpenAIBatchClient) GetBatch(ctx context.Context, batchID string) (*openai.Batch, error) {
	if batchID == "" {
		return nil, fmt.Errorf("openai batch client: empty batch ID")
	}
	// CheckPoint: batch_client.get_batch
	if c.injector != nil {
		if err := c.injector.CheckPoint(ctx, error_injection.POINT_BATCH_CLIENT_GET_BATCH); err != nil {
			return nil, err
		}
	}

	start := time.Now().UTC()
	var opts []option.RequestOption
	if common.IncludeDeploymentFromCtx(ctx) {
		opts = append(opts, option.WithQuery("include_deployment", "true"))
	}
	batch, err := c.client.Batches.Get(ctx, batchID, opts...)
	if err != nil {
		metrics.Emitter.Counter(metricConsolePlannerError, 1, metrics.T("method", "get_batch"))
		return nil, err
	}
	metrics.Duration(metrics.Emitter, metricConsolePlannerDuration, start, metrics.T("method", "get_batch"))
	return batch, nil
}

func (c *OpenAIBatchClient) CancelBatch(ctx context.Context, batchID string) (*openai.Batch, error) {
	if batchID == "" {
		return nil, fmt.Errorf("openai batch client: empty batch ID")
	}
	// CheckPoint: batch_client.cancel_batch
	if c.injector != nil {
		if err := c.injector.CheckPoint(ctx, error_injection.POINT_BATCH_CLIENT_CANCEL_BATCH); err != nil {
			return nil, err
		}
	}
	start := time.Now().UTC()
	batch, err := c.client.Batches.Cancel(ctx, batchID)
	if err != nil {
		metrics.Emitter.Counter(metricConsolePlannerError, 1, metrics.T("method", "cancel_batch"))
		return nil, err
	}
	metrics.Duration(metrics.Emitter, metricConsolePlannerDuration, start, metrics.T("method", "cancel_batch"))
	return batch, nil
}

func (c *OpenAIBatchClient) ListBatches(ctx context.Context, req *ListBatchesRequest) (*ListBatchesResponse, error) {
	// CheckPoint: batch_client.list_batches
	if c.injector != nil {
		if err := c.injector.CheckPoint(ctx, error_injection.POINT_BATCH_CLIENT_LIST_BATCHES); err != nil {
			return nil, err
		}
	}
	params := openai.BatchListParams{}
	if req != nil {
		if req.After != "" {
			params.After = openai.String(req.After)
		}
		if req.Limit > 0 {
			params.Limit = openai.Int(int64(req.Limit))
		}
	}
	start := time.Now().UTC()
	page, err := c.client.Batches.List(ctx, params)
	if err != nil {
		metrics.Emitter.Counter(metricConsolePlannerError, 1, metrics.T("method", "list_batches"))
		return nil, err
	}
	metrics.Duration(metrics.Emitter, metricConsolePlannerDuration, start, metrics.T("method", "list_batches"))
	batches := make([]*openai.Batch, 0, len(page.Data))
	for i := range page.Data {
		batches = append(batches, &page.Data[i])
	}
	return &ListBatchesResponse{Data: batches, HasMore: page.HasMore}, nil
}

// buildExtraBodyOptions projects AIBrixExtraBody fields into
// option.WithJSONSet calls so the openai-go SDK serializes them at the
// top level of POST /v1/batches under "aibrix.*". MDS reads them out of
// BatchSpec.aibrix.
func buildExtraBodyOptions(eb AIBrixExtraBody) []option.RequestOption {
	var opts []option.RequestOption
	if eb.JobID != "" {
		opts = append(opts, option.WithJSONSet("aibrix.job_id", eb.JobID))
	}
	if eb.Runtime != nil && eb.Runtime.Target != "" {
		opts = append(opts, option.WithJSONSet("aibrix.runtime", eb.Runtime))
	}
	if eb.ResourceAllocation != nil {
		opts = append(opts, option.WithJSONSet("aibrix.resource_allocation", eb.ResourceAllocation))
	}
	if eb.ModelTemplate != nil && eb.ModelTemplate.Name != "" {
		opts = append(opts, option.WithJSONSet("aibrix.model_template", eb.ModelTemplate))
	}
	if eb.Model != "" {
		opts = append(opts, option.WithJSONSet("aibrix.model", eb.Model))
	}
	if eb.Client != nil {
		opts = append(opts, option.WithJSONSet("aibrix.client", eb.Client))
	}
	return opts
}
