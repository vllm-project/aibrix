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

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

type fakeJobPlanner struct {
	job         *plannerapi.Job
	getErr      error
	cancelErr   error
	cancelledID string
}

func (p *fakeJobPlanner) Start(context.Context) error { return nil }

func (p *fakeJobPlanner) Enqueue(context.Context, *plannerapi.EnqueueRequest) (*plannerapi.Job, error) {
	return nil, nil
}

func (p *fakeJobPlanner) GetJob(context.Context, string) (*plannerapi.Job, error) {
	return p.job, p.getErr
}

func (p *fakeJobPlanner) ListJobs(context.Context, *plannerapi.ListJobsRequest) (*plannerapi.ListJobsResponse, error) {
	return &plannerapi.ListJobsResponse{}, nil
}

func (p *fakeJobPlanner) Cancel(_ context.Context, jobID string) (*plannerapi.Job, error) {
	p.cancelledID = jobID
	return p.job, p.cancelErr
}

func (p *fakeJobPlanner) Recover(context.Context) error { return nil }

func (p *fakeJobPlanner) Close() error { return nil }

func contextWithUserEmail(email string) context.Context {
	return metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs(middleware.MetadataUserEmail, email),
	)
}

func plannerJobWithOwner(jobID, owner string) *plannerapi.Job {
	return &plannerapi.Job{
		JobID: jobID,
		Batch: &openai.Batch{
			ID:               "batch-mds-1",
			Endpoint:         "/v1/chat/completions",
			InputFileID:      "file-input",
			CompletionWindow: "24h",
			Status:           openai.BatchStatusValidating,
			Metadata: shared.Metadata{
				metadataConsoleCreatedBy: owner,
			},
		},
	}
}

func TestCancelJobRejectsNonOwner(t *testing.T) {
	planner := &fakeJobPlanner{job: plannerJobWithOwner("job-console-1", "owner@example.com")}
	handler := NewJobHandler(nil, planner, "", false)

	_, err := handler.CancelJob(contextWithUserEmail("other@example.com"), &pb.CancelJobRequest{
		Id: "job-console-1",
	})

	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("CancelJob code = %v, want PermissionDenied; err=%v", status.Code(err), err)
	}
	if planner.cancelledID != "" {
		t.Fatalf("planner.Cancel called for non-owner with id %q", planner.cancelledID)
	}
}

func TestCancelJobRejectsMissingViewerForOwnedJob(t *testing.T) {
	planner := &fakeJobPlanner{job: plannerJobWithOwner("job-console-1", "owner@example.com")}
	handler := NewJobHandler(nil, planner, "", false)

	_, err := handler.CancelJob(context.Background(), &pb.CancelJobRequest{
		Id: "job-console-1",
	})

	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("CancelJob code = %v, want PermissionDenied; err=%v", status.Code(err), err)
	}
	if planner.cancelledID != "" {
		t.Fatalf("planner.Cancel called without viewer with id %q", planner.cancelledID)
	}
}

func TestCancelJobAllowsOwner(t *testing.T) {
	planner := &fakeJobPlanner{job: plannerJobWithOwner("job-console-1", "owner@example.com")}
	handler := NewJobHandler(nil, planner, "", false)

	job, err := handler.CancelJob(contextWithUserEmail("owner@example.com"), &pb.CancelJobRequest{
		Id: "job-console-1",
	})

	if err != nil {
		t.Fatalf("CancelJob returned error for owner: %v", err)
	}
	if planner.cancelledID != "job-console-1" {
		t.Fatalf("planner.Cancel id = %q, want job-console-1", planner.cancelledID)
	}
	if job == nil || job.Id != "job-console-1" {
		t.Fatalf("job = %#v, want job-console-1", job)
	}
}

func TestGetJobMapsPlannerNotFound(t *testing.T) {
	planner := &fakeJobPlanner{
		getErr: fmt.Errorf("%w: job-console-missing", plannerapi.ErrJobNotFound),
	}
	handler := NewJobHandler(nil, planner, "", false)

	_, err := handler.GetJob(context.Background(), &pb.GetJobRequest{Id: "job-console-missing"})

	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetJob code = %v, want NotFound; err=%v", status.Code(err), err)
	}
}

func TestMergeJobExposesExtraBodyRuntimeAndErrors(t *testing.T) {
	raw := []byte(`{
		"id": "batch-mds-1",
		"object": "batch",
		"endpoint": "/v1/chat/completions",
		"input_file_id": "file-input",
		"completion_window": "24h",
		"status": "failed",
		"created_at": 1700000000,
		"expires_at": 1700086400,
		"failed_at": 1700000120,
		"errors": {
			"object": "list",
			"data": [
				{"code": "invalid_request", "message": "bad jsonl", "param": "input_file_id", "line": 7}
			]
		},
		"metadata": {
			"aibrix.console.display_name": "nightly eval",
			"aibrix.console.created_by": "owner@example.com"
		},
		"aibrix": {
			"job_id": "job-console-1",
			"runtime": {
				"target": "Kubernetes",
				"options": {"namespace": "batch-ns"}
			},
			"resource_allocation": {
				"provision_id": "prov-1",
				"resource_details": [
					{"endpoint_cluster": "cluster-a", "gpu_type": "H100", "replica": 2}
				]
			},
			"model_template": {"name": "mock-vllm", "version": "v1"},
			"profile": {"name": "prod-24h"}
		}
	}`)
	var batch openai.Batch
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatalf("unmarshal batch: %v", err)
	}

	job := mergeJob(&plannerapi.Job{
		JobID: "job-console-1",
		Batch: &batch,
		State: &plannerapi.JobState{
			BatchID:             "batch-mds-1",
			ProvisionID:         "prov-1",
			QueuedAt:            time.Unix(1699999900, 0),
			ResourcePreparingAt: time.Unix(1699999910, 0),
			SubmittingAt:        time.Unix(1699999990, 0),
			ErrorMessage:        "planner saw final failure",
		},
	}, nil)

	if job.BatchId != "batch-mds-1" {
		t.Fatalf("BatchId = %q, want batch-mds-1", job.BatchId)
	}
	if job.ProvisionId != "prov-1" {
		t.Fatalf("ProvisionId = %q, want prov-1", job.ProvisionId)
	}
	if job.ExtraBody["aibrix"] == "" {
		t.Fatalf("extra_body[aibrix] missing: %#v", job.ExtraBody)
	}
	if job.Runtime == nil {
		t.Fatalf("runtime missing")
	}
	if job.Runtime.Target != "Kubernetes" {
		t.Fatalf("runtime target = %q, want Kubernetes", job.Runtime.Target)
	}
	if got := job.Runtime.Options["namespace"]; got != "batch-ns" {
		t.Fatalf("runtime namespace = %q, want batch-ns", got)
	}
	if job.ResourceAllocation == nil ||
		job.ResourceAllocation.ProvisionId != "prov-1" {
		t.Fatalf("resource allocation missing provision: %#v", job.ResourceAllocation)
	}
	if len(job.ResourceAllocation.ResourceDetails) != 1 {
		t.Fatalf("resource details len = %d, want 1", len(job.ResourceAllocation.ResourceDetails))
	}
	if got := job.ResourceAllocation.ResourceDetails[0].GpuType; got != "H100" {
		t.Fatalf("resource detail gpu = %q, want H100", got)
	}
	if job.Profile == nil || job.Profile.Name != "prod-24h" {
		t.Fatalf("profile = %#v, want prod-24h", job.Profile)
	}
	if len(job.Errors) != 1 || job.Errors[0].Code != "invalid_request" {
		t.Fatalf("errors = %#v, want invalid_request", job.Errors)
	}
	if job.ErrorMessage != "planner saw final failure" {
		t.Fatalf("ErrorMessage = %q, want planner error", job.ErrorMessage)
	}
	if len(job.Events) < 4 {
		t.Fatalf("events len = %d, want planner + MDS events", len(job.Events))
	}
}

func TestMergeJobAcceptsNestedExtraBodyExtension(t *testing.T) {
	raw := []byte(`{
		"id": "batch-mds-2",
		"object": "batch",
		"endpoint": "/v1/chat/completions",
		"input_file_id": "file-input",
		"completion_window": "24h",
		"status": "validating",
		"created_at": 1700000000,
		"extra_body": {
			"aibrix": {
				"runtime": {
					"target": "Kubernetes",
					"options": {"namespace": "batch-ns"}
				},
				"resource_allocation": {
					"provision_id": "prov-2"
				}
			}
		}
	}`)
	var batch openai.Batch
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatalf("unmarshal batch: %v", err)
	}

	job := mergeJob(&plannerapi.Job{
		JobID: "job-console-2",
		Batch: &batch,
	}, nil)

	if job.ExtraBody["aibrix"] == "" {
		t.Fatalf("extra_body[aibrix] missing: %#v", job.ExtraBody)
	}
	if job.Runtime == nil || job.Runtime.Target != "Kubernetes" {
		t.Fatalf("runtime = %#v, want Kubernetes", job.Runtime)
	}
	if job.ProvisionId != "prov-2" {
		t.Fatalf("ProvisionId = %q, want prov-2", job.ProvisionId)
	}
}

// Timeline events carry second-granularity timestamps from two clocks (planner
// and MDS), so events frequently collide on the same second. They must then fall
// back to lifecycle order, not the lexical id order that put Failed before
// Finalizing and MDS-batch-created before Submitting.
func TestBuildJobEventsBreaksSameSecondTiesByLifecycle(t *testing.T) {
	const submit = int64(1700000043)
	const finalize = int64(1700001067)
	job := &pb.Job{
		QueuedAt:            1700000033,
		ResourcePreparingAt: 1700000033, // ties with Queued
		CreatedAt:           submit,     // MDS batch created, ties with Submitting
		BatchId:             "batch-1",
		SubmittingAt:        submit,
		InProgressAt:        1700000044,
		FinalizingAt:        finalize,
		FailedAt:            finalize, // ties with Finalizing
		ErrorMessage:        "boom",
	}

	events := buildJobEvents(job)
	got := make([]string, len(events))
	for i, e := range events {
		got[i] = e.Id
	}
	want := []string{
		"queued", "resource_preparing", "submitting", "batch_created",
		"in_progress", "finalizing", "failed",
	}
	if len(got) != len(want) {
		t.Fatalf("event ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event order = %v, want %v", got, want)
		}
	}
}

// Template names are only unique per (model_id, name, version): two models can
// each own a template with the same name. CreateJob must resolve under the
// model the wizard picked, and map it to that model's serving_name — not the
// console-internal model id or the template name.
func TestResolveTemplateAndServingNameScopedByModel(t *testing.T) {
	ctx := context.Background()
	s := store.NewMemoryStore()
	t.Cleanup(func() { _ = s.Close() })

	seed := []struct {
		modelID     string
		servingName string
		sourceURI   string
	}{
		{"model-a", "org/model-a", "org/model-a"},
		{"model-b", "", "/models/model-b"}, // no serving_name → fall back to source uri
	}
	for _, m := range seed {
		if _, err := s.CreateModel(ctx, &pb.Model{Id: m.modelID, Name: m.modelID, ServingName: m.servingName}); err != nil {
			t.Fatalf("CreateModel(%s): %v", m.modelID, err)
		}
		_, err := s.CreateModelDeploymentTemplate(ctx, &pb.CreateModelDeploymentTemplateRequest{
			ModelId: m.modelID,
			Name:    "default",
			Spec: &pb.ModelDeploymentTemplateSpec{
				ModelSource: &pb.ModelSourceSpec{Uri: m.sourceURI},
			},
		})
		if err != nil {
			t.Fatalf("CreateModelDeploymentTemplate(%s): %v", m.modelID, err)
		}
	}

	h := NewJobHandler(s, &fakeJobPlanner{}, "", false)

	tpl := h.resolveTemplate(ctx, "model-b", "default", "")
	if tpl == nil {
		t.Fatal("resolveTemplate(model-b, default) = nil")
	}
	if tpl.ModelId != "model-b" {
		t.Fatalf("resolved template under model %q, want model-b", tpl.ModelId)
	}
	if got := h.resolveServingName(ctx, tpl); got != "/models/model-b" {
		t.Fatalf("serving name = %q, want source uri fallback /models/model-b", got)
	}

	tplA := h.resolveTemplate(ctx, "model-a", "default", "")
	if tplA == nil || tplA.ModelId != "model-a" {
		t.Fatalf("resolveTemplate(model-a, default) = %#v, want template under model-a", tplA)
	}
	if got := h.resolveServingName(ctx, tplA); got != "org/model-a" {
		t.Fatalf("serving name = %q, want org/model-a", got)
	}

	// Legacy path (no model id) still resolves by bare name.
	if tpl := h.resolveTemplate(ctx, "", "default", ""); tpl == nil {
		t.Fatal("legacy resolveTemplate(default) = nil")
	}

	// Templates cannot dangle under an unregistered model.
	_, err := s.CreateModelDeploymentTemplate(ctx, &pb.CreateModelDeploymentTemplateRequest{
		ModelId: "model-ghost",
		Name:    "default",
		Spec:    &pb.ModelDeploymentTemplateSpec{},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("create template under unknown model: err = %v, want FailedPrecondition", err)
	}
}
