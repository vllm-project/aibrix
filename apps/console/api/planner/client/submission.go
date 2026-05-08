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

// =============================================================================
// MDS submission payload
//
// AIBrixExtraBody mirrors the planner -> MDS contract under
// extra_body.aibrix. The struct carries every field the planner has
// computed; the BatchClient adapter is responsible for dropping fields
// MDS does not yet accept (see buildExtraBodyOptions in client.go).
//
// MDS-side BatchSpec.aibrix is Pydantic with extra="forbid" and only
// declares model_template / profile today; sending unknown keys yields
// 400 validation errors. Other fields here (JobID, PlannerDecision)
// are populated for log-trace verification and will start riding the
// wire once MDS accepts them.
// See python/aibrix/aibrix/metadata/api/v1/batch.py.
// =============================================================================

// AIBrixExtraBody is the in-memory contract between the planner and the
// MDS adapter. Only ModelTemplate currently rides the wire; the other
// fields are computed and logged for verification.
type AIBrixExtraBody struct {
	JobID           string `json:"job_id,omitempty"`
	PlannerDecision *struct {
		ProvisionID               string `json:"provision_id,omitempty"`
		ProvisionResourceDeadline int64  `json:"provision_resource_deadline,omitempty"`
	} `json:"planner_decision,omitempty"`
	ModelTemplate *plannerapi.ModelTemplateRef `json:"model_template,omitempty"`
}

// MDSExtraBody is the wrapper that lands as extra_body on the openai-go
// client call. The "aibrix" key is the only namespace MDS understands.
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
