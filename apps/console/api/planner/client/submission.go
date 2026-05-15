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
	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
)

// PlannerDecision is the planner's allocation decision projected into
// AIBrixExtraBody. ResourceDetails stays as a nested anonymous struct
// because its shape will evolve as the RM populates per-group allocation
// details (currently always nil — RM doesn't return them yet).
type PlannerDecision struct {
	// ProvisionID mirrors rmtypes.ProvisionResult.ProvisionID returned
	// by Provisioner.Provision.
	ProvisionID string `json:"provision_id,omitempty"`
	// ProvisionResourceDeadline is the unix-seconds deadline.
	ProvisionResourceDeadline int64 `json:"provision_resource_deadline,omitempty"`
	ResourceDetails           []struct {
		// ResourceType maps to rmtypes.ResourceProvisionType
		// (kubernetes / aws / lambdaCloud); may grow finer-grained.
		ResourceType string `json:"resource_type"`
		// EndpointCluster identifies the cluster serving this group;
		EndpointCluster string `json:"endpoint_cluster,omitempty"`
		// GPUType identifies the GPU model of the provisioned nodes.
		// The GPU count is not defined here; it comes from the ModelTemplate.
		GPUType string `json:"gpu_type,omitempty"`
		// WorkerNum identifies the number of replicas to provision
		WorkerNum int `json:"worker_num,omitempty"`
	} `json:"resource_details,omitempty"`
}

// AIBrixExtraBody is the AIBrix-specific extension the BatchClient
// serializes onto POST /v1/batches via the openai-go SDK's extra_body
// channel. Everything else on the submission rides on openai.BatchNewParams
// directly — this struct is the only piece of the contract we own.
type AIBrixExtraBody struct {
	JobID           string                       `json:"job_id,omitempty"`
	PlannerDecision *PlannerDecision             `json:"planner_decision,omitempty"`
	ModelTemplate   *plannerapi.ModelTemplateRef `json:"model_template,omitempty"`
}
