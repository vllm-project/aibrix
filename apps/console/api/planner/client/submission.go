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

// PlannerDecision is the backend-agnostic planner allocation decision
// carried on AIBrixExtraBody.PlannerDecision. Each backend defines its
// own concrete type implementing this interface.
//
// isPlannerDecision is an unexported, no-op marker method (empty body,
// never called at runtime) that exists purely to seal the interface:
// because it is unexported, only types declared in this package can
// satisfy PlannerDecision, preventing arbitrary structs from accidentally
// satisfying it.
type PlannerDecision interface {
	isPlannerDecision()
}

// DefaultDecision is the shape used by backends that only need to carry
// the provision ID (kubernetes / aws / lambdaCloud today).
type DefaultDecision struct {
	ProvisionID string `json:"provision_id,omitempty"`
}

func (*DefaultDecision) isPlannerDecision() {}

// AIBrixExtraBody is the AIBrix-specific extension the BatchClient
// serializes onto POST /v1/batches via the openai-go SDK's extra_body
// channel. Everything else on the submission rides on openai.BatchNewParams
// directly.
type AIBrixExtraBody struct {
	JobID           string                       `json:"job_id,omitempty"`
	PlannerDecision PlannerDecision              `json:"planner_decision,omitempty"`
	ModelTemplate   *plannerapi.ModelTemplateRef `json:"model_template,omitempty"`
}
