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
//   - Calls the metadata service /v1/batches API via the official OpenAI Go
//     SDK (openai-go v3). Talking to the metadata service through the SDK
//     keeps it honest about being OpenAI-compatible — schema drift on the
//     upstream side surfaces immediately as a deserialization or 4xx error.
//   - Persists Console-owned fields (id, display name, created_by, future:
//     organization, tags ...) in the local store.
//   - Aggregates both sources into the wire-level *pb.Job returned to the UI.
//
// The AIBrix-only extension `aibrix.model_template` is passed via the SDK's
// `option.WithJSONSet`, which is the OpenAI-recommended `extra_body` channel.
//
// When the metadata service is unreachable the handler propagates the error
// (codes.Unavailable). The frontend renders its mock fallback in that case.
package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

const (
	metadataDisplayName = "display_name"
	defaultListLimit    = 20
)

// JobHandler implements console.v1.JobService.
type JobHandler struct {
	pb.UnimplementedJobServiceServer

	store                          store.Store
	openai                         openai.Client
	defaultModelDeploymentTemplate string
}

// NewJobHandler creates a JobHandler.
func NewJobHandler(s store.Store, metadataServiceURL, defaultModelDeploymentTemplate string) *JobHandler {

	baseURL := strings.TrimRight(metadataServiceURL, "/")
	client := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("aibrix-console"),
	)
	return &JobHandler{
		store:                          s,
		openai:                         client,
		defaultModelDeploymentTemplate: defaultModelDeploymentTemplate,
	}
}

// ListJobs proxies to GET /v1/batches and merges with store.
func (h *JobHandler) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	params := openai.BatchListParams{}
	if req.After != "" {
		params.After = openai.String(req.After)
	}
	limit := defaultListLimit
	if req.Limit > 0 {
		limit = int(req.Limit)
	}
	params.Limit = openai.Int(int64(limit))

	page, err := h.openai.Batches.List(ctx, params)
	if err != nil {
		return nil, mapSDKError(err, "list batches")
	}

	batches := page.Data
	ids := make([]string, 0, len(batches))
	for i := range batches {
		ids = append(ids, batches[i].ID)
	}
	overlays, err := h.store.ListJobs(ctx, ids)
	if err != nil {
		klog.Warningf("store.ListJobs failed; returning batch state without overlay: %v", err)
		overlays = map[string]*pb.Job{}
	}

	jobs := make([]*pb.Job, 0, len(batches))
	for i := range batches {
		jobs = append(jobs, mergeJob(&batches[i], overlays[batches[i].ID]))
	}
	// SDK CursorPage exposes Data and HasMore. first_id / last_id ride along
	// in the upstream JSON but are not surfaced as named fields; the UI
	// doesn't consume them yet, so leave empty and revisit if pagination
	// becomes user-visible.
	return &pb.ListJobsResponse{
		Jobs:    jobs,
		HasMore: page.HasMore,
	}, nil
}

// GetJob proxies to GET /v1/batches/{id} and merges with store.
func (h *JobHandler) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.Job, error) {
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	batch, err := h.openai.Batches.Get(ctx, req.Id)
	if err != nil {
		return nil, mapSDKError(err, "get batch")
	}
	overlay, _ := h.store.GetJob(ctx, batch.ID)
	return mergeJob(batch, overlay), nil
}

// CreateJob calls POST /v1/batches and persists the Console-owned overlay.
//
// max_tokens / temperature / top_p / n on the request are intentionally NOT
// forwarded yet — per-request JSONL values win. They're reserved on the
// proto so the Console contract is stable; a follow-up will route them into
// aibrix.overrides.engine_args.
func (h *JobHandler) CreateJob(ctx context.Context, req *pb.CreateJobRequest) (*pb.Job, error) {
	if req.InputDataset == "" {
		return nil, status.Error(codes.InvalidArgument, "input_dataset is required")
	}
	if req.Endpoint == "" {
		return nil, status.Error(codes.InvalidArgument, "endpoint is required")
	}

	completionWindow := req.CompletionWindow
	if completionWindow == "" {
		completionWindow = string(openai.BatchNewParamsCompletionWindow24h)
	}

	params := openai.BatchNewParams{
		InputFileID:      req.InputDataset,
		Endpoint:         openai.BatchNewParamsEndpoint(req.Endpoint),
		CompletionWindow: openai.BatchNewParamsCompletionWindow(completionWindow),
	}
	if req.Name != "" {
		params.Metadata = map[string]string{metadataDisplayName: req.Name}
	}

	// AIBrix extension fields ride along via OpenAI's `extra_body` channel.
	var opts []option.RequestOption
	if h.defaultModelDeploymentTemplate != "" {
		opts = append(opts, option.WithJSONSet("aibrix.model_template", h.defaultModelDeploymentTemplate))
	}

	batch, err := h.openai.Batches.New(ctx, params, opts...)
	if err != nil {
		return nil, mapSDKError(err, "create batch")
	}

	overlay := &pb.Job{
		Id:        batch.ID,
		Name:      req.Name,
		CreatedBy: currentUserEmail(ctx),
	}
	if err := h.store.UpsertJob(ctx, overlay); err != nil {
		// Don't fail the request: the metadata service already created the
		// batch. The Console row will be filled by a future reconcile.
		klog.Warningf("store.UpsertJob failed for %s: %v", batch.ID, err)
	}
	return mergeJob(batch, overlay), nil
}

// CancelJob proxies to POST /v1/batches/{id}/cancel and merges with store.
func (h *JobHandler) CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.Job, error) {
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	batch, err := h.openai.Batches.Cancel(ctx, req.Id)
	if err != nil {
		return nil, mapSDKError(err, "cancel batch")
	}
	overlay, _ := h.store.GetJob(ctx, batch.ID)
	return mergeJob(batch, overlay), nil
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

// mapSDKError translates an openai-go API error into a gRPC status, preserving
// the upstream message and using the upstream HTTP status to pick a code.
func mapSDKError(err error, op string) error {
	if err == nil {
		return nil
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		c := codes.Unknown
		switch apiErr.StatusCode {
		case http.StatusBadRequest:
			c = codes.InvalidArgument
		case http.StatusNotFound:
			c = codes.NotFound
		case http.StatusConflict:
			c = codes.FailedPrecondition
		case http.StatusUnauthorized, http.StatusForbidden:
			c = codes.PermissionDenied
		default:
			if apiErr.StatusCode >= 500 {
				c = codes.Unavailable
			}
		}
		return status.Error(c, apiErr.Error())
	}

	return status.Errorf(codes.Unavailable, "%s: %v", op, err)
}

// mergeJob aggregates the OpenAI Batch state with the Console-side overlay.
// Either input may be nil. Console-owned fields override anything that may
// have leaked from the upstream metadata bag.
func mergeJob(b *openai.Batch, overlay *pb.Job) *pb.Job {
	job := &pb.Job{}
	if b != nil {
		job.Id = b.ID
		job.Object = string(b.Object)
		job.Endpoint = b.Endpoint
		job.Model = b.Model
		job.InputDataset = b.InputFileID
		job.CompletionWindow = b.CompletionWindow
		job.Status = string(b.Status)
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
		if len(b.Metadata) > 0 {
			job.Metadata = map[string]string(b.Metadata)
			job.Name = b.Metadata[metadataDisplayName]
		}
		if b.JSON.RequestCounts.Valid() {
			job.RequestCounts = &pb.JobRequestCounts{
				Total:     int32(b.RequestCounts.Total),
				Completed: int32(b.RequestCounts.Completed),
				Failed:    int32(b.RequestCounts.Failed),
			}
		}
		if b.JSON.Usage.Valid() {
			job.Usage = &pb.JobUsage{
				InputTokens:  b.Usage.InputTokens,
				OutputTokens: b.Usage.OutputTokens,
				TotalTokens:  b.Usage.TotalTokens,
			}
		}
	}
	if overlay != nil {
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
