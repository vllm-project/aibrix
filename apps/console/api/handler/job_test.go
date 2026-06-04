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
	"encoding/json"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
)

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
