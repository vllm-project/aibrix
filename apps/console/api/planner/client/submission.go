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

package client

import (
	"time"

	"github.com/vllm-project/aibrix/apps/console/api/planner/api"
)

// =============================================================================
// MDS submission payload
//
// The worker assembles MDSBatchSubmission bottom-up from a claimed
// PlannerTask plus a Provision: ResourceDetail entries are derived from
// Provision.Allocations, PlannerDecision is derived from Provision
// identity + ExpiresAt, and the AIBrix-namespaced wrapper carries them
// (plus the JobID correlation key and the Console-supplied
// ModelTemplate/Profile refs) into extra_body.aibrix on the
// final POST /v1/batches call.
// =============================================================================

type ResourceDetail struct {
	ResourceType    string `json:"resource_type"`
	EndpointCluster string `json:"endpoint_cluster,omitempty"`
	GPUType         string `json:"gpu_type"`
	WorkerNum       int    `json:"worker_num"`
}

// AIBrixExtraBody is the logical payload carried under extra_body.aibrix
// when talking to MDS. It is the wire-level contract between the planner
// and the metadata service.
//
// JobID is the primary correlation key between planner tasks and MDS
// batches. MDS must persist and echo it for BatchView.JobID round-trips
// to work; see README.md "MDS correlation and dedup (hard external
// dependency)".

type AIBrixExtraBody struct {
	JobID           string `json:"job_id"`
	PlannerDecision *struct {
		ProvisionID               string `json:"provision_id,omitempty"`
		ProvisionResourceDeadline int64  `json:"provision_resource_deadline,omitempty"`
	} `json:"planner_decision,omitempty"`
	ResourceDetails []ResourceDetail             `json:"resource_details,omitempty"`
	ModelTemplate   *plannerapi.ModelTemplateRef `json:"model_template,omitempty"`
	Profile         *plannerapi.ProfileRef       `json:"profile,omitempty"`
}

// MDSExtraBody is the top-level extra_body payload the planner sends to MDS.
//
// Today the only namespace is "aibrix"; the wrapper exists because the
// MDSBatchSubmission struct is the shared type that paired components
// (executor, MDS transport adapter) construct and consume.
type MDSExtraBody struct {
	AIBrix AIBrixExtraBody `json:"aibrix"`
}

// MDSBatchSubmission is the fully prepared submit payload for
// POST /v1/batches.

type MDSBatchSubmission struct {
	InputFileID      string            `json:"input_file_id"`
	Endpoint         string            `json:"endpoint"`
	CompletionWindow string            `json:"completion_window"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	ExtraBody        MDSExtraBody      `json:"extra_body"`
}

type Provision struct {
	ProvisionID string           `json:"provision_id"`
	JobID       string           `json:"job_id"`
	Allocations []ResourceDetail `json:"allocations,omitempty"`
	// ExpiresAt is when the RM will reclaim this provision if Release
	// has not been called.
	//
	// This is the SAME concept as RM-side provision expiry
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}
