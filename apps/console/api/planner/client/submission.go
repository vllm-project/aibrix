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
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// ResourceAllocation is the backend-agnostic planner allocation metadata
// carried on AIBrixExtraBody.ResourceAllocation. Each backend defines its
// own concrete type implementing this interface.
//
// isResourceAllocation is an unexported, no-op marker method (empty body,
// never called at runtime) that exists purely to seal the interface:
// because it is unexported, only types declared in this package can
// satisfy ResourceAllocation, preventing arbitrary structs from accidentally
// satisfying it.
type ResourceAllocation interface {
	isResourceAllocation()
}

// DefaultResourceAllocation is the shape used by backends that only need to carry
// the provision ID (kubernetes / aws / lambdaCloud today).
type DefaultResourceAllocation struct {
	ProvisionID string `json:"provision_id,omitempty"`
}

func (*DefaultResourceAllocation) isResourceAllocation() {}

// RuntimeRef selects the metadata-service Runtime used to materialize the job.
// Options is intentionally free-form for runtime-specific fields such as
// Kubernetes namespace, region, or provisioner-specific switches.
type RuntimeRef struct {
	Target  string                 `json:"target,omitempty"`
	Options map[string]interface{} `json:"options,omitempty"`
}

// RuntimeForProvisionType maps Resource Manager provider names to MDS Runtime
// targets. Keep this flat for now; if a provider grows many runtimes, introduce
// an explicit console-side runtime selector instead of deriving it here.
func RuntimeForProvisionType(t rmtypes.ResourceProvisionType) *RuntimeRef {
	target := ""
	switch t {
	case rmtypes.ResourceProvisionTypeKubernetes:
		target = "Kubernetes"
	case rmtypes.ResourceProvisionTypeLambdaCloud:
		target = "LambdaCloud"
	case rmtypes.ResourceProvisionTypeRunPod:
		target = "RunPod"
	case rmtypes.ResourceProvisionTypeAWS:
		target = "External"
	default:
		if t != "" {
			target = string(t)
		}
	}
	if target == "" {
		return nil
	}
	return &RuntimeRef{Target: target}
}

// AIBrixExtraBody is the AIBrix-specific extension the BatchClient
// serializes onto POST /v1/batches via the openai-go SDK's extra_body
// channel. Everything else on the submission rides on openai.BatchNewParams
// directly.
type AIBrixExtraBody struct {
	JobID              string                       `json:"job_id,omitempty"`
	Runtime            *RuntimeRef                  `json:"runtime,omitempty"`
	ResourceAllocation ResourceAllocation           `json:"resource_allocation,omitempty"`
	ModelTemplate      *plannerapi.ModelTemplateRef `json:"model_template,omitempty"`
}
