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
//   - Routes all job lifecycle calls (Enqueue / Get / List / Cancel)
//     through the Planner. The Planner owns JobID -> MDS batch.ID
//     translation; the handler never holds an MDS batch.ID.
//   - Persists Console-owned fields (id, display name, created_by, future:
//     organization, tags ...) in the local store.
//   - Aggregates Planner Jobs into the wire-level *pb.Job returned to the UI.
//
// The AIBrix-only extension `aibrix.model_template` is forwarded to MDS by the
// planner's BatchClient via the OpenAI SDK's `extra_body` channel.
//
// When the planner / metadata service is unreachable the handler propagates
// the error (codes.Unavailable). The frontend renders its mock fallback in
// that case.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/klog/v2"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
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
	jsonNullLiteral                = "null"
)

// JobHandler implements console.v1.JobService.
type JobHandler struct {
	pb.UnimplementedJobServiceServer

	store                          store.Store
	planner                        plannerapi.Planner
	defaultModelDeploymentTemplate string
	devMode                        bool
}

// NewJobHandler creates a JobHandler.
func NewJobHandler(s store.Store, planner plannerapi.Planner, defaultModelDeploymentTemplate string, devMode bool) *JobHandler {
	return &JobHandler{
		store:                          s,
		planner:                        planner,
		defaultModelDeploymentTemplate: defaultModelDeploymentTemplate,
		devMode:                        devMode,
	}
}

// ListJobs proxies to GET /v1/batches. Console-owned fields ride on
// batch.metadata; the store overlay path is parked (see CreateJob).
func (h *JobHandler) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	limit := defaultListLimit
	if req.Limit > 0 {
		limit = int(req.Limit)
	}
	resp, err := h.planner.ListJobs(ctx, &plannerapi.ListJobsRequest{
		Limit: limit,
		After: req.After,
	})
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

	jobs := make([]*pb.Job, 0, len(resp.Data))
	for _, job := range resp.Data {
		jobs = append(jobs, mergeJob(job, nil))
	}
	h.enrichJobs(ctx, jobs)
	// SDK CursorPage exposes Data and HasMore. first_id / last_id ride along
	// in the upstream JSON but are not surfaced as named fields; the UI
	// doesn't consume them yet, so leave empty and revisit if pagination
	// becomes user-visible.
	return &pb.ListJobsResponse{
		Jobs:    jobs,
		HasMore: resp.HasMore,
	}, nil
}

// GetJob proxies to GET /v1/batches/{id}. Planner owns JobID -> MDS batch.ID
// resolution and reads MDS whenever a batch exists, including terminal
// batches whose usage/output/extra_body fields are not stored locally.
func (h *JobHandler) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.Job, error) {
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	job, err := h.planner.GetJob(ctx, req.Id)
	if err != nil {
		// Dev fallback: return the demo job if MDS is unreachable.
		if h.devMode {
			if dev, ok := h.store.(interface {
				GetDemoJob(id string) (*pb.Job, bool)
			}); ok {
				if demoJob, found := dev.GetDemoJob(req.Id); found {
					klog.Warningf("MDS unreachable, falling back to demo job %s: %v", req.Id, err)
					return demoJob, nil
				}
			}
		}
		if h.store != nil {
			rec, serr := h.store.GetJob(ctx, req.Id)
			if serr != nil {
				klog.Warningf("GetJob store fallback lookup id=%s: %v", req.Id, serr)
			} else if rec != nil && plannerapi.JobStatus(rec.Status).IsTerminal() {
				pbJob, perr := rec.ToPB()
				if perr != nil {
					klog.Warningf("GetJob terminal store fallback id=%s: %v", req.Id, perr)
				} else {
					klog.Warningf("GetJob planner failed; returning terminal store fallback id=%s: %v", req.Id, err)
					return h.enrichJob(ctx, pbJob), nil
				}
			}
		}
		return nil, mapPlannerError(err, "get batch")
	}
	return h.enrichJob(ctx, mergeJob(job, nil)), nil
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

	// Console-generated JobID. The async Scheduler will own a durable
	// JobID -> BatchID map; until then the planner keeps it in-memory.
	jobID := "job_" + uuid.NewString()

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

	// AIBrix extension fields ride along via OpenAI's `extra_body` channel.
	// The console wizard always picks a template (model_template_name); legacy
	// callers may still hit this path with empty fields, in which case we fall
	// back to the configured default.
	templateName := req.ModelTemplateName
	if templateName == "" {
		templateName = h.defaultModelDeploymentTemplate
	}
	var (
		modelTemplate *plannerapi.ModelTemplateRef
		modelID       string
	)
	if templateName != "" {
		modelTemplate = &plannerapi.ModelTemplateRef{
			Name:    templateName,
			Version: req.ModelTemplateVersion,
		}
		if tpl := h.resolveTemplate(ctx, templateName, req.ModelTemplateVersion); tpl != nil {
			modelID = tpl.ModelId
			if tpl.Spec != nil {
				// UseProtoNames keeps snake_case proto field names (engine_args,
				// model_source, ...) that the Python pydantic consumer expects;
				// default protojson uses lowerCamelCase. Enums still serialize as strings.
				if specBytes, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(tpl.Spec); err == nil {
					modelTemplate.Spec = specBytes
				} else {
					klog.Warningf("marshal template spec %q/%q: %v", templateName, req.ModelTemplateVersion, err)
				}
			}
		}
	}

	enqueueReq := &plannerapi.EnqueueRequest{
		JobID:         jobID,
		Model:         modelID,
		ModelTemplate: modelTemplate,
		BatchParams: openai.BatchNewParams{
			InputFileID:      req.InputDataset,
			Endpoint:         openai.BatchNewParamsEndpoint(req.Endpoint),
			CompletionWindow: openai.BatchNewParamsCompletionWindow(completionWindow),
			Metadata:         metadata,
		},
	}

	job, err := h.planner.Enqueue(ctx, enqueueReq)
	if err != nil {
		return nil, mapPlannerError(err, "create batch")
	}
	return h.enrichJob(ctx, mergeJob(job, nil)), nil
}

// CancelJob routes through Planner.Cancel; the planner resolves JobID
// to MDS batch.ID and forwards to /v1/batches/{id}/cancel.
func (h *JobHandler) CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.Job, error) {
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	existing, err := h.planner.GetJob(ctx, req.Id)
	if err != nil {
		return nil, mapPlannerError(err, "cancel batch")
	}
	if err := requireJobOwner(ctx, mergeJob(existing, nil), "cancel"); err != nil {
		return nil, err
	}
	job, err := h.planner.Cancel(ctx, req.Id)
	if err != nil {
		return nil, mapPlannerError(err, "cancel batch")
	}
	return h.enrichJob(ctx, mergeJob(job, nil)), nil
}

func requireJobOwner(ctx context.Context, job *pb.Job, action string) error {
	if job == nil {
		return nil
	}
	viewer := strings.TrimSpace(currentUserEmail(ctx))
	owner := strings.TrimSpace(job.CreatedBy)
	if owner == "" {
		return nil
	}
	if viewer == "" || !strings.EqualFold(viewer, owner) {
		return status.Errorf(codes.PermissionDenied, "only the job owner can %s this batch", action)
	}
	return nil
}

// currentUserEmail returns the authenticated user's email if available.
func currentUserEmail(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(middleware.MetadataUserEmail); len(v) > 0 && v[0] != "" {
			return v[0]
		}
	}
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

// mapPlannerError translates planner sentinel errors into gRPC statuses,
// falling back to mapSDKError for transport-level failures the planner
// surfaces unchanged (errors.Join in the planner preserves the inner
// *openai.Error so HTTP-status-derived codes still come through).
func mapPlannerError(err error, op string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, plannerapi.ErrInvalidJob):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, plannerapi.ErrInsufficientResources):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, plannerapi.ErrJobNotFound):
		return status.Error(codes.NotFound, err.Error())
	}
	return mapSDKError(err, op)
}

// resolveTemplate looks up the ModelDeploymentTemplate by (name, version).
// Returns nil when name is empty, store errors out, or no match.
func (h *JobHandler) resolveTemplate(ctx context.Context, name, version string) *pb.ModelDeploymentTemplate {
	if name == "" {
		return nil
	}
	statusFilter := ""
	if version == "" {
		statusFilter = "active"
	}
	tpls, err := h.store.ListModelDeploymentTemplates(ctx, "", statusFilter, name)
	if err != nil {
		klog.Warningf("resolveTemplate(%q,%q): %v", name, version, err)
		return nil
	}
	for _, t := range tpls {
		if version == "" || t.Version == version {
			return t
		}
	}
	return nil
}

// mergeJob aggregates the planner's Job with optional Console overlay.
// pb.Job.Id is set to the planner's JobID — the MDS batch.ID never reaches
// this layer. Console-owned fields (display name, created_by, template
// binding) are read out of batch.metadata under the aibrix.console.*
// namespace; the overlay argument is plumbing for the future store-backed
// reconcile path and is expected to be nil today.
func mergeJob(v *plannerapi.Job, overlay *pb.Job) *pb.Job {
	job := &pb.Job{Object: "batch"}
	if v != nil {
		job.Id = v.JobID
		if b := v.Batch; b != nil {
			job.BatchId = b.ID
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
			if b.JSON.Errors.Valid() {
				for _, e := range b.Errors.Data {
					job.Errors = append(job.Errors, &pb.JobError{
						Code:    e.Code,
						Message: e.Message,
						Param:   e.Param,
						Line:    e.Line,
					})
				}
			}
			if extraBody := parseBatchExtraBody(b); len(extraBody) > 0 {
				job.ExtraBody = compactRawJSONMap(extraBody)
				applyKnownBatchExtensions(job, extraBody)
			}
		}
		if v.State != nil {
			applyPlannerState(job, v.State)
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
	if job.ProvisionId == "" && job.ResourceAllocation != nil {
		job.ProvisionId = job.ResourceAllocation.ProvisionId
	}
	job.Events = buildJobEvents(job)
	return job
}

type rawBatchExtensionPayload struct {
	JobID              string          `json:"job_id"`
	Runtime            json.RawMessage `json:"runtime"`
	ResourceAllocation json.RawMessage `json:"resource_allocation"`
	ModelTemplate      json.RawMessage `json:"model_template"`
	Profile            json.RawMessage `json:"profile"`
}

type rawRuntimePayload struct {
	Target  string                 `json:"target"`
	Options map[string]interface{} `json:"options"`
}

type rawResourceAllocationPayload struct {
	ProvisionID               string            `json:"provision_id"`
	ProvisionResourceDeadline int64             `json:"provision_resource_deadline"`
	ResourceDetails           []json.RawMessage `json:"resource_details"`
}

type rawResourceDetailPayload struct {
	EndpointCluster string `json:"endpoint_cluster"`
	GPUType         string `json:"gpu_type"`
	Replica         int32  `json:"replica"`
}

type rawNamedRefPayload struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

var standardBatchResponseFields = map[string]struct{}{
	"id":                {},
	"object":            {},
	"endpoint":          {},
	"model":             {},
	"errors":            {},
	"input_file_id":     {},
	"completion_window": {},
	"status":            {},
	"output_file_id":    {},
	"error_file_id":     {},
	"created_at":        {},
	"in_progress_at":    {},
	"expires_at":        {},
	"finalizing_at":     {},
	"completed_at":      {},
	"failed_at":         {},
	"expired_at":        {},
	"cancelling_at":     {},
	"cancelled_at":      {},
	"request_counts":    {},
	"usage":             {},
	"metadata":          {},
}

func parseBatchExtraBody(b *openai.Batch) map[string]json.RawMessage {
	if b == nil || b.RawJSON() == "" {
		return nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(b.RawJSON()), &root); err != nil {
		return nil
	}
	out := make(map[string]json.RawMessage)
	if raw, ok := root["extra_body"]; ok && len(raw) > 0 && string(raw) != jsonNullLiteral {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(raw, &extra); err == nil {
			for k, v := range extra {
				if len(v) > 0 && string(v) != jsonNullLiteral {
					out[k] = v
				}
			}
		}
	}
	for k, v := range root {
		if k == "extra_body" {
			continue
		}
		if _, standard := standardBatchResponseFields[k]; standard {
			continue
		}
		if len(v) > 0 && string(v) != jsonNullLiteral {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func compactRawJSONMap(in map[string]json.RawMessage) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = compactJSON(v)
	}
	return out
}

func applyKnownBatchExtensions(job *pb.Job, extraBody map[string]json.RawMessage) {
	if job == nil || len(extraBody) == 0 {
		return
	}
	if raw, ok := extraBody["aibrix"]; ok {
		applyAibrixBatchExtension(job, raw)
	}
}

func applyAibrixBatchExtension(job *pb.Job, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == jsonNullLiteral {
		return
	}
	var payload rawBatchExtensionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	if len(payload.Runtime) > 0 && string(payload.Runtime) != jsonNullLiteral {
		var runtime rawRuntimePayload
		if err := json.Unmarshal(payload.Runtime, &runtime); err == nil {
			job.Runtime = &pb.JobRuntime{
				Target:  runtime.Target,
				Options: stringifyMap(runtime.Options),
				RawJson: compactJSON(payload.Runtime),
			}
		}
	}
	if len(payload.ResourceAllocation) > 0 && string(payload.ResourceAllocation) != jsonNullLiteral {
		var allocation rawResourceAllocationPayload
		if err := json.Unmarshal(payload.ResourceAllocation, &allocation); err == nil {
			job.ResourceAllocation = &pb.JobResourceAllocation{
				ProvisionId:               allocation.ProvisionID,
				ProvisionResourceDeadline: allocation.ProvisionResourceDeadline,
				RawJson:                   compactJSON(payload.ResourceAllocation),
			}
			for _, rawDetail := range allocation.ResourceDetails {
				detail := parseResourceDetail(rawDetail)
				if detail != nil {
					job.ResourceAllocation.ResourceDetails = append(job.ResourceAllocation.ResourceDetails, detail)
				}
			}
			if job.ProvisionId == "" {
				job.ProvisionId = allocation.ProvisionID
			}
		}
	}
	if len(payload.ModelTemplate) > 0 && string(payload.ModelTemplate) != jsonNullLiteral {
		var modelTemplate rawNamedRefPayload
		if err := json.Unmarshal(payload.ModelTemplate, &modelTemplate); err == nil {
			job.ModelTemplateRef = &pb.JobModelTemplateRef{
				Name:    modelTemplate.Name,
				Version: modelTemplate.Version,
				RawJson: compactJSON(payload.ModelTemplate),
			}
			if job.ModelTemplateName == "" {
				job.ModelTemplateName = modelTemplate.Name
			}
			if job.ModelTemplateVersion == "" {
				job.ModelTemplateVersion = modelTemplate.Version
			}
		}
	}
	if len(payload.Profile) > 0 && string(payload.Profile) != jsonNullLiteral {
		var profile rawNamedRefPayload
		if err := json.Unmarshal(payload.Profile, &profile); err == nil {
			job.Profile = &pb.JobProfileRef{
				Name:    profile.Name,
				RawJson: compactJSON(payload.Profile),
			}
		}
	}
}

func parseResourceDetail(raw json.RawMessage) *pb.JobResourceDetail {
	var detail rawResourceDetailPayload
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil
	}
	var all map[string]interface{}
	_ = json.Unmarshal(raw, &all)
	delete(all, "endpoint_cluster")
	delete(all, "gpu_type")
	delete(all, "replica")
	return &pb.JobResourceDetail{
		EndpointCluster: detail.EndpointCluster,
		GpuType:         detail.GPUType,
		Replica:         detail.Replica,
		Extra:           stringifyMap(all),
	}
}

func stringifyMap(in map[string]interface{}) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = stringifyJSONValue(v)
	}
	return out
}

func stringifyJSONValue(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func applyPlannerState(job *pb.Job, state *plannerapi.JobState) {
	if state.BatchID != "" {
		job.BatchId = state.BatchID
	}
	if state.ProvisionID != "" {
		job.ProvisionId = state.ProvisionID
	}
	if state.ErrorMessage != "" {
		job.ErrorMessage = state.ErrorMessage
	}
	job.QueuedAt = unixOrZero(state.QueuedAt)
	job.ResourcePreparingAt = unixOrZero(state.ResourcePreparingAt)
	job.SubmittingAt = unixOrZero(state.SubmittingAt)
	job.ResourceFailedAt = unixOrZero(state.ResourceFailedAt)
	job.SubmitFailedAt = unixOrZero(state.SubmitFailedAt)
	job.CancelRequestedAt = unixOrZero(state.CancelRequestedAt)
	if job.CancelledAt == 0 {
		job.CancelledAt = unixOrZero(state.CancelledAt)
	}
}

func (h *JobHandler) enrichJob(ctx context.Context, job *pb.Job) *pb.Job {
	if job == nil {
		return nil
	}
	h.attachProvision(ctx, job)
	job.Events = buildJobEvents(job)
	return job
}

func (h *JobHandler) enrichJobs(ctx context.Context, jobs []*pb.Job) {
	provisions := h.listProvisionsForJobs(ctx, jobs)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if prov := provisions[job.ProvisionId]; prov != nil {
			applyProvision(job, prov)
		}
		job.Events = buildJobEvents(job)
	}
}

func (h *JobHandler) listProvisionsForJobs(ctx context.Context, jobs []*pb.Job) map[string]*rmtypes.ProvisionResult {
	if h.store == nil {
		return nil
	}
	seen := make(map[string]struct{})
	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		if job == nil || job.ProvisionId == "" {
			continue
		}
		if _, ok := seen[job.ProvisionId]; ok {
			continue
		}
		seen[job.ProvisionId] = struct{}{}
		ids = append(ids, job.ProvisionId)
	}
	if len(ids) == 0 {
		return nil
	}
	results, err := h.store.ListProvisions(ctx, &rmtypes.ListOptions{
		ProvisionIDs: &ids,
		Limit:        len(ids),
	})
	if err != nil {
		klog.Warningf("list provisions for jobs: %v", err)
		return nil
	}
	out := make(map[string]*rmtypes.ProvisionResult, len(results))
	for _, result := range results {
		if result != nil && result.ProvisionID != "" {
			out[result.ProvisionID] = result
		}
	}
	return out
}

func (h *JobHandler) attachProvision(ctx context.Context, job *pb.Job) {
	if h.store == nil || job.ProvisionId == "" {
		return
	}
	prov, err := h.store.GetProvision(ctx, job.ProvisionId)
	if err != nil || prov == nil {
		return
	}
	applyProvision(job, prov)
}

func applyProvision(job *pb.Job, prov *rmtypes.ProvisionResult) {
	if job == nil || prov == nil {
		return
	}
	raw, _ := json.Marshal(prov)
	job.Provision = &pb.JobProvision{
		ProvisionId:    prov.ProvisionID,
		Provider:       prov.Provider,
		IdempotencyKey: prov.IdempotencyKey,
		Status:         string(prov.Status),
		Region:         prov.Region,
		ErrorMessage:   prov.ErrorMessage,
		CreatedAt:      unixOrZero(prov.CreatedAt),
		UpdatedAt:      unixOrZero(prov.UpdatedAt),
		RawJson:        string(raw),
	}
	if job.ErrorMessage == "" && prov.ErrorMessage != "" {
		job.ErrorMessage = prov.ErrorMessage
	}
}

func buildJobEvents(job *pb.Job) []*pb.JobEvent {
	if job == nil {
		return nil
	}
	events := make([]*pb.JobEvent, 0, 12)
	add := func(id, label, status, source string, at int64, message string) {
		if at == 0 {
			return
		}
		events = append(events, &pb.JobEvent{
			Id:      id,
			Label:   label,
			Status:  status,
			Source:  source,
			At:      at,
			Message: message,
		})
	}
	add("queued", "Queued", "queued", "planner", job.QueuedAt, "Console accepted the job.")
	add("resource_preparing", "Provisioning", "resource_preparing", "planner", job.ResourcePreparingAt, "Resource provisioning started.")
	add("submitting", "Submitting", "submitting", "planner", job.SubmittingAt, "Provisioning reached ready and the batch was submitted to MDS.")
	if job.BatchId != "" {
		add("batch_created", "MDS batch created", "validating", "mds", job.CreatedAt, "Metadata Service created the OpenAI batch.")
	}
	add("in_progress", "In progress", "in_progress", "mds", job.InProgressAt, "MDS started processing requests.")
	add("finalizing", "Finalizing", "finalizing", "mds", job.FinalizingAt, "MDS started finalizing output files.")
	add("cancel_requested", "Cancel requested", "cancelling", "planner", job.CancelRequestedAt, "Console requested cancellation.")
	add("cancelling", "Cancelling", "cancelling", "mds", job.CancellingAt, "MDS started cancelling the batch.")
	add("completed", "Completed", "completed", "mds", job.CompletedAt, "Batch completed.")
	if job.ResourceFailedAt == 0 && job.SubmitFailedAt == 0 {
		add("failed", "Failed", "failed", "mds", job.FailedAt, firstNonEmpty(job.ErrorMessage, "Batch failed."))
	}
	add("expired", "Expired", "expired", "mds", job.ExpiredAt, "Batch expired.")
	add("cancelled", "Cancelled", "cancelled", "mds", job.CancelledAt, "Batch cancelled.")
	add("resource_failed", "Provision failed", "resource_failed", "planner", job.ResourceFailedAt, firstNonEmpty(job.ErrorMessage, "Resource provisioning failed."))
	add("submit_failed", "Submit failed", "submit_failed", "planner", job.SubmitFailedAt, firstNonEmpty(job.ErrorMessage, "MDS batch submission failed."))

	sort.SliceStable(events, func(i, j int) bool {
		if events[i].At == events[j].At {
			return events[i].Id < events[j].Id
		}
		return events[i].At < events[j].At
	})
	return events
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
