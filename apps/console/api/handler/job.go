/*
Copyright 2025 The Aibrix Team.

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

// JobHandler implements the Console BFF JobService:
//
//   - Proxies to the metadata service /v1/batches API for OpenAI Batch state
//     (status, usage, request_counts, timestamps, output/error file ids ...)
//   - Persists Console-owned fields (id, display name, created_by, future:
//     organization, tags ...) in the local store
//   - Aggregates both sources into the wire-level *pb.Job returned to the UI
//
// When the metadata service is unreachable the handler propagates the error
// (codes.Unavailable). The frontend renders its mock fallback in that case.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

const (
	jobHTTPClientTimeout = 60 * time.Second
	metadataDisplayName  = "display_name"
)

// JobHandler implements console.v1.JobService.
type JobHandler struct {
	pb.UnimplementedJobServiceServer

	store                          store.Store
	metadataServiceURL             string
	defaultModelDeploymentTemplate string
	httpClient                     *http.Client
}

// NewJobHandler creates a JobHandler.
//
// defaultModelDeploymentTemplate is injected as aibrix.model_template on
// CreateJob requests when the caller does not provide one.
//
// TODO: replace this env-var default with a per-Model batch_template lookup
// once the Model entity carries that field.
func NewJobHandler(s store.Store, metadataServiceURL, defaultModelDeploymentTemplate string) *JobHandler {
	return &JobHandler{
		store:                          s,
		metadataServiceURL:             strings.TrimRight(metadataServiceURL, "/"),
		defaultModelDeploymentTemplate: defaultModelDeploymentTemplate,
		httpClient:                     &http.Client{Timeout: jobHTTPClientTimeout},
	}
}

// ListJobs proxies to GET /v1/batches and merges with store.
func (h *JobHandler) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	q := url.Values{}
	if req.After != "" {
		q.Set("after", req.After)
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(int(req.Limit)))
	}
	target := h.metadataServiceURL + "/v1/batches"
	if encoded := q.Encode(); encoded != "" {
		target += "?" + encoded
	}

	body, code, err := h.do(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "metadata service unreachable: %v", err)
	}
	if code >= 400 {
		return nil, statusFromUpstream(code, body)
	}

	var listResp openaiBatchList
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, status.Errorf(codes.Internal, "decode batch list: %v", err)
	}

	ids := make([]string, 0, len(listResp.Data))
	for i := range listResp.Data {
		ids = append(ids, listResp.Data[i].ID)
	}
	overlays, err := h.store.ListJobs(ctx, ids)
	if err != nil {
		klog.Warningf("store.ListJobs failed; returning batch state without overlay: %v", err)
		overlays = map[string]*pb.Job{}
	}

	jobs := make([]*pb.Job, 0, len(listResp.Data))
	for i := range listResp.Data {
		jobs = append(jobs, mergeJob(&listResp.Data[i], overlays[listResp.Data[i].ID]))
	}
	return &pb.ListJobsResponse{
		Jobs:    jobs,
		FirstId: listResp.FirstID,
		LastId:  listResp.LastID,
		HasMore: listResp.HasMore,
	}, nil
}

// GetJob proxies to GET /v1/batches/{id} and merges with store.
func (h *JobHandler) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.Job, error) {
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	target := fmt.Sprintf("%s/v1/batches/%s", h.metadataServiceURL, url.PathEscape(req.Id))
	body, code, err := h.do(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "metadata service unreachable: %v", err)
	}
	if code >= 400 {
		return nil, statusFromUpstream(code, body)
	}
	var batch openaiBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, status.Errorf(codes.Internal, "decode batch: %v", err)
	}
	overlay, _ := h.store.GetJob(ctx, batch.ID)
	return mergeJob(&batch, overlay), nil
}

// CreateJob translates the request to OpenAI shape, POSTs to /v1/batches,
// then writes the Console-owned fields to the store.
func (h *JobHandler) CreateJob(ctx context.Context, req *pb.CreateJobRequest) (*pb.Job, error) {
	if req.InputDataset == "" {
		return nil, status.Error(codes.InvalidArgument, "input_dataset is required")
	}
	if req.Endpoint == "" {
		return nil, status.Error(codes.InvalidArgument, "endpoint is required")
	}

	upstream := openaiCreateBatch{
		InputFileID:      req.InputDataset,
		Endpoint:         req.Endpoint,
		CompletionWindow: req.CompletionWindow,
		Metadata:         map[string]string{},
	}
	if upstream.CompletionWindow == "" {
		upstream.CompletionWindow = "24h"
	}
	if req.Name != "" {
		upstream.Metadata[metadataDisplayName] = req.Name
	}
	if h.defaultModelDeploymentTemplate != "" {
		upstream.Aibrix = &openaiAibrixExt{ModelTemplate: h.defaultModelDeploymentTemplate}
	}

	payload, err := json.Marshal(upstream)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode create batch: %v", err)
	}
	body, code, err := h.do(ctx, http.MethodPost, h.metadataServiceURL+"/v1/batches", payload)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "metadata service unreachable: %v", err)
	}
	if code >= 400 {
		return nil, statusFromUpstream(code, body)
	}
	var batch openaiBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, status.Errorf(codes.Internal, "decode batch: %v", err)
	}

	overlay := &pb.Job{
		Id:        batch.ID,
		Name:      req.Name,
		CreatedBy: currentUserEmail(ctx),
	}
	if err := h.store.UpsertJob(ctx, overlay); err != nil {
		// Don't fail the request: the metadata service already created the
		// batch, so the user's intent succeeded. Console-side fields will
		// be blank for this row until a follow-up reconciliation.
		klog.Warningf("store.UpsertJob failed for %s: %v", batch.ID, err)
	}
	return mergeJob(&batch, overlay), nil
}

// CancelJob proxies to POST /v1/batches/{id}/cancel and merges with store.
func (h *JobHandler) CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.Job, error) {
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	target := fmt.Sprintf("%s/v1/batches/%s/cancel", h.metadataServiceURL, url.PathEscape(req.Id))
	body, code, err := h.do(ctx, http.MethodPost, target, nil)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "metadata service unreachable: %v", err)
	}
	if code >= 400 {
		return nil, statusFromUpstream(code, body)
	}
	var batch openaiBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, status.Errorf(codes.Internal, "decode batch: %v", err)
	}
	overlay, _ := h.store.GetJob(ctx, batch.ID)
	return mergeJob(&batch, overlay), nil
}

func (h *JobHandler) do(ctx context.Context, method, target string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBody, resp.StatusCode, nil
}

// statusFromUpstream maps an HTTP status from the metadata service to a gRPC
// status, preserving the upstream error body where possible.
func statusFromUpstream(code int, body []byte) error {
	c := codes.Unknown
	switch {
	case code == http.StatusBadRequest:
		c = codes.InvalidArgument
	case code == http.StatusNotFound:
		c = codes.NotFound
	case code == http.StatusConflict:
		c = codes.FailedPrecondition
	case code >= 500:
		c = codes.Unavailable
	}
	msg := string(body)
	if msg == "" {
		msg = http.StatusText(code)
	}
	return status.Error(c, msg)
}

// currentUserEmail returns the authenticated user's email if available, else
// empty. The auth middleware sets this on the HTTP request context; once the
// gateway propagates it to gRPC metadata it will surface here.
func currentUserEmail(ctx context.Context) string {
	if u := middleware.GetUser(ctx); u != nil {
		return u.Email
	}
	return ""
}

// ---- OpenAI Batch wire types (from the metadata service) ----

type openaiBatch struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Endpoint         string            `json:"endpoint"`
	Model            string            `json:"model,omitempty"`
	InputFileID      string            `json:"input_file_id"`
	CompletionWindow string            `json:"completion_window"`
	Status           string            `json:"status"`
	OutputFileID     string            `json:"output_file_id,omitempty"`
	ErrorFileID      string            `json:"error_file_id,omitempty"`
	CreatedAt        int64             `json:"created_at"`
	InProgressAt     int64             `json:"in_progress_at,omitempty"`
	ExpiresAt        int64             `json:"expires_at,omitempty"`
	FinalizingAt     int64             `json:"finalizing_at,omitempty"`
	CompletedAt      int64             `json:"completed_at,omitempty"`
	FailedAt         int64             `json:"failed_at,omitempty"`
	ExpiredAt        int64             `json:"expired_at,omitempty"`
	CancellingAt     int64             `json:"cancelling_at,omitempty"`
	CancelledAt      int64             `json:"cancelled_at,omitempty"`
	RequestCounts    *openaiCounts     `json:"request_counts,omitempty"`
	Usage            *openaiBatchUsage `json:"usage,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

type openaiCounts struct {
	Total     int32 `json:"total"`
	Completed int32 `json:"completed"`
	Failed    int32 `json:"failed"`
}

type openaiBatchUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type openaiBatchList struct {
	Object  string        `json:"object"`
	Data    []openaiBatch `json:"data"`
	FirstID string        `json:"first_id,omitempty"`
	LastID  string        `json:"last_id,omitempty"`
	HasMore bool          `json:"has_more"`
}

type openaiCreateBatch struct {
	InputFileID      string            `json:"input_file_id"`
	Endpoint         string            `json:"endpoint"`
	CompletionWindow string            `json:"completion_window,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Aibrix           *openaiAibrixExt  `json:"aibrix,omitempty"`
}

type openaiAibrixExt struct {
	ModelTemplate string `json:"model_template,omitempty"`
}

// mergeJob aggregates the OpenAI Batch state with the Console-side overlay.
// Either input may be nil. Console-owned fields override anything that may
// have leaked from the upstream metadata bag.
func mergeJob(b *openaiBatch, overlay *pb.Job) *pb.Job {
	job := &pb.Job{}
	if b != nil {
		job.Id = b.ID
		job.Object = b.Object
		job.Endpoint = b.Endpoint
		job.Model = b.Model
		job.InputDataset = b.InputFileID
		job.CompletionWindow = b.CompletionWindow
		job.Status = b.Status
		job.OutputDataset = b.OutputFileID
		job.ErrorDataset = b.ErrorFileID
		job.CreatedAt = b.CreatedAt
		job.InProgressAt = b.InProgressAt
		job.ExpiresAt = b.ExpiresAt
		job.FinalizingAt = b.FinalizingAt
		job.CompletedAt = b.CompletedAt
		job.FailedAt = b.FailedAt
		job.ExpiredAt = b.ExpiredAt
		job.CancellingAt = b.CancellingAt
		job.CancelledAt = b.CancelledAt
		job.Metadata = b.Metadata
		if b.RequestCounts != nil {
			job.RequestCounts = &pb.JobRequestCounts{
				Total:     b.RequestCounts.Total,
				Completed: b.RequestCounts.Completed,
				Failed:    b.RequestCounts.Failed,
			}
		}
		if b.Usage != nil {
			job.Usage = &pb.JobUsage{
				InputTokens:  b.Usage.InputTokens,
				OutputTokens: b.Usage.OutputTokens,
				TotalTokens:  b.Usage.TotalTokens,
			}
		}
		// Default name from upstream metadata in case overlay is empty.
		if b.Metadata != nil {
			job.Name = b.Metadata[metadataDisplayName]
		}
	}
	if overlay != nil {
		// Console-owned fields take precedence.
		if overlay.Name != "" {
			job.Name = overlay.Name
		}
		if overlay.CreatedBy != "" {
			job.CreatedBy = overlay.CreatedBy
		}
		if job.Id == "" {
			job.Id = overlay.Id
		}
	}
	return job
}
