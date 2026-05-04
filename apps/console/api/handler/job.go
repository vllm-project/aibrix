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

// Console-owned fields we stash on the OpenAI batch.metadata map. Namespaced
// to keep them out of user-supplied metadata's key space. The bare
// "display_name" key is kept for backwards compatibility with batches
// created by older console builds.
const (
	metadataDisplayName            = "display_name" // legacy fallback
	metadataConsoleDisplayName     = "aibrix.console.display_name"
	metadataConsoleCreatedBy       = "aibrix.console.created_by"
	metadataConsoleTemplateName    = "aibrix.console.template_name"
	metadataConsoleTemplateVersion = "aibrix.console.template_version"
	defaultListLimit               = 20
)

// JobHandler implements console.v1.JobService.
type JobHandler struct {
	pb.UnimplementedJobServiceServer

	store                          store.Store
	openai                         openai.Client
	defaultModelDeploymentTemplate string
	devMode                        bool
}

// NewJobHandler creates a JobHandler.
func NewJobHandler(s store.Store, metadataServiceURL, defaultModelDeploymentTemplate string, devMode bool) *JobHandler {

	baseURL := strings.TrimRight(metadataServiceURL, "/")
	client := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("aibrix-console"),
	)
	return &JobHandler{
		store:                          s,
		openai:                         client,
		defaultModelDeploymentTemplate: defaultModelDeploymentTemplate,
		devMode:                        devMode,
	}
}

// ListJobs proxies to GET /v1/batches. Console-owned fields ride on
// batch.metadata; the store overlay path is parked (see CreateJob).
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
		// Dev fallback: serve Console's demo batches so the UI is usable
		// end-to-end without a running MDS.
		if h.devMode {
			if dev, ok := h.store.(interface{ ListDemoJobs() []*pb.Job }); ok {
				klog.Warningf("MDS unreachable, falling back to demo jobs: %v", err)
				return &pb.ListJobsResponse{Jobs: dev.ListDemoJobs(), HasMore: false}, nil
			}
		}
		// Non-dev: degrade to empty list. Surfacing the raw SDK error in the UI
		// leaks internals (MDS URL, connection details) and isn't actionable
		// for end users; ops can still diagnose from server logs.
		klog.Warningf("list batches failed; returning empty list: %v", err)
		return &pb.ListJobsResponse{Jobs: nil, HasMore: false}, nil
	}

	batches := page.Data
	jobs := make([]*pb.Job, 0, len(batches))
	for i := range batches {
		jobs = append(jobs, mergeJob(&batches[i], nil))
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

// GetJob proxies to GET /v1/batches/{id}. Console-owned fields ride on
// batch.metadata; the store overlay path is parked (see CreateJob).
func (h *JobHandler) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.Job, error) {
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	batch, err := h.openai.Batches.Get(ctx, req.Id)
	if err != nil {
		// Dev fallback: return the demo job if MDS is unreachable.
		if h.devMode {
			if dev, ok := h.store.(interface {
				GetDemoJob(id string) (*pb.Job, bool)
			}); ok {
				if job, found := dev.GetDemoJob(req.Id); found {
					klog.Warningf("MDS unreachable, falling back to demo job %s: %v", req.Id, err)
					return job, nil
				}
			}
		}
		return nil, mapSDKError(err, "get batch")
	}
	return mergeJob(batch, nil), nil
}

// CreateJob calls POST /v1/batches with console-owned fields (display name,
// created_by, template binding) packed into batch.metadata under the
// aibrix.console.* namespace. The store-overlay path is intentionally parked
// while we treat MDS batch.metadata as the single source of truth for the
// e2e demo loop; the store layer (UpsertJob/GetJob/ListJobs) stays in place
// for the future reconcile / annotations design (see #8 discussion).
//
// Per-batch sampling params (max_tokens / temperature / top_p / n) are baked
// into the JSONL by the console wizard before upload, so they don't appear
// on this request. The OpenAI SDK path can still POST them to MDS directly.
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

	// Pack console-owned fields into batch.Metadata under the aibrix.console.*
	// namespace. This keeps a single source of truth (MDS) for the e2e demo
	// loop. The store-overlay path is parked (not deleted) for the future
	// reconcile / annotation work; see job.go:UpsertJob comment.
	metadata := map[string]string{}
	if req.Name != "" {
		metadata[metadataConsoleDisplayName] = req.Name
		metadata[metadataDisplayName] = req.Name // legacy key, kept for back-compat reads
	}
	if email := currentUserEmail(ctx); email != "" {
		metadata[metadataConsoleCreatedBy] = email
	}
	if req.ModelTemplateName != "" {
		metadata[metadataConsoleTemplateName] = req.ModelTemplateName
	}
	if req.ModelTemplateVersion != "" {
		metadata[metadataConsoleTemplateVersion] = req.ModelTemplateVersion
	}

	params := openai.BatchNewParams{
		InputFileID:      req.InputDataset,
		Endpoint:         openai.BatchNewParamsEndpoint(req.Endpoint),
		CompletionWindow: openai.BatchNewParamsCompletionWindow(completionWindow),
	}
	if len(metadata) > 0 {
		params.Metadata = metadata
	}

	// AIBrix extension fields ride along via OpenAI's `extra_body` channel.
	// The console wizard always picks a template (model_template_name); legacy
	// callers may still hit this path with empty fields, in which case we fall
	// back to the configured default.
	var opts []option.RequestOption
	if req.ModelTemplateName != "" {
		opts = append(opts, option.WithJSONSet("aibrix.model_template.name", req.ModelTemplateName))
		if req.ModelTemplateVersion != "" {
			opts = append(opts, option.WithJSONSet("aibrix.model_template.version", req.ModelTemplateVersion))
		}
	} else if h.defaultModelDeploymentTemplate != "" {
		opts = append(opts, option.WithJSONSet("aibrix.model_template.name", h.defaultModelDeploymentTemplate))
	}

	batch, err := h.openai.Batches.New(ctx, params, opts...)
	if err != nil {
		return nil, mapSDKError(err, "create batch")
	}
	return mergeJob(batch, nil), nil
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
	return mergeJob(batch, nil), nil
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

// mergeJob aggregates the OpenAI Batch state with optional Console overlay.
// Console-owned fields (display name, created_by, template binding) are read
// out of batch.metadata under the aibrix.console.* namespace; the overlay
// argument is plumbing for the future store-backed reconcile path and is
// expected to be nil today.
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
			// Console-owned fields. Prefer namespaced keys; fall back to the
			// legacy bare "display_name" so batches created by older builds
			// still surface their name.
			if v := b.Metadata[metadataConsoleDisplayName]; v != "" {
				job.Name = v
			} else if v := b.Metadata[metadataDisplayName]; v != "" {
				job.Name = v
			}
			if v := b.Metadata[metadataConsoleCreatedBy]; v != "" {
				job.CreatedBy = v
			}
			if v := b.Metadata[metadataConsoleTemplateName]; v != "" {
				job.ModelTemplateName = v
			}
			if v := b.Metadata[metadataConsoleTemplateVersion]; v != "" {
				job.ModelTemplateVersion = v
			}
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
	// Overlay still respected when caller chooses to pass one (future path).
	if overlay != nil {
		if overlay.Name != "" {
			job.Name = overlay.Name
		}
		if overlay.CreatedBy != "" {
			job.CreatedBy = overlay.CreatedBy
		}
		if overlay.ModelTemplateName != "" {
			job.ModelTemplateName = overlay.ModelTemplateName
		}
		if overlay.ModelTemplateVersion != "" {
			job.ModelTemplateVersion = overlay.ModelTemplateVersion
		}
		if job.Id == "" {
			job.Id = overlay.Id
		}
	}
	return job
}
