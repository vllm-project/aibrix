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

package impl

import (
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
)

// validateEnqueueRequest validates the EnqueueRequest fields including BatchNewParams.
// Returns an error describing the first validation failure, or nil if valid.
func validateEnqueueRequest(r *plannerapi.EnqueueRequest) error {
	if r == nil {
		return fmt.Errorf("request is nil")
	}

	if r.JobID == "" {
		return fmt.Errorf("job_id is required")
	}

	_, _, err := decodeAcceleratorFromTemplate(r.ModelTemplate)
	if err != nil {
		return fmt.Errorf("model_template %s parse error: %w", r.ModelTemplate, err)
	}

	return validateBatchNewParams(&r.BatchParams)
}

// validateBatchNewParams validates OpenAI BatchNewParams according to API requirements.
func validateBatchNewParams(params *openai.BatchNewParams) error {
	// CompletionWindow is required and must be "24h"
	if params.CompletionWindow == "" {
		return fmt.Errorf("completion_window is required")
	}

	// AIBrix does not have 24h limit as OpenAI does, so we do not validate 24h here.
	// if params.CompletionWindow != openai.BatchNewParamsCompletionWindow24h {
	// 	return fmt.Errorf("completion_window must be '24h', got '%s'", params.CompletionWindow)
	// }
	if _, err := time.ParseDuration(string(params.CompletionWindow)); err != nil {
		return fmt.Errorf("completion_window %s parse error: %w", params.CompletionWindow, err)
	}

	// Endpoint is required and must be one of the supported endpoints
	if params.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	if !isValidBatchEndpoint(params.Endpoint) {
		return fmt.Errorf("endpoint must be one of /v1/responses, /v1/chat/completions, /v1/embeddings, /v1/completions, /v1/moderations, /v1/images/generations, /v1/images/edits, /v1/videos, got '%s'", params.Endpoint)
	}

	// InputFileID is required
	if params.InputFileID == "" {
		return fmt.Errorf("input_file_id is required")
	}

	// Metadata is optional but has constraints if provided
	if err := validateMetadata(params.Metadata); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}

	// OutputExpiresAfter is optional but has constraints if provided
	if params.OutputExpiresAfter.Seconds != 0 {
		if err := validateOutputExpiresAfter(&params.OutputExpiresAfter); err != nil {
			return fmt.Errorf("output_expires_after: %w", err)
		}
	}

	return nil
}

// isValidBatchEndpoint checks if the endpoint is one of the supported OpenAI batch endpoints.
func isValidBatchEndpoint(endpoint openai.BatchNewParamsEndpoint) bool {
	validEndpoints := []openai.BatchNewParamsEndpoint{
		openai.BatchNewParamsEndpointV1Responses,
		openai.BatchNewParamsEndpointV1ChatCompletions,
		openai.BatchNewParamsEndpointV1Embeddings,
		openai.BatchNewParamsEndpointV1Completions,
		openai.BatchNewParamsEndpointV1Moderations,
		openai.BatchNewParamsEndpointV1ImagesGenerations,
		openai.BatchNewParamsEndpointV1ImagesEdits,
		openai.BatchNewParamsEndpointV1Videos,
	}
	for _, valid := range validEndpoints {
		if endpoint == valid {
			return true
		}
	}
	return false
}

// validateMetadata validates metadata constraints:
// - Up to 16 key-value pairs
// - Keys max 64 characters
// - Values max 512 characters
func validateMetadata(metadata map[string]string) error {
	if len(metadata) == 0 {
		return nil
	}

	if len(metadata) > 16 {
		return fmt.Errorf("cannot have more than 16 key-value pairs, got %d", len(metadata))
	}

	for key, value := range metadata {
		if len(key) > 64 {
			return fmt.Errorf("key '%s' exceeds maximum length of 64 characters (got %d)", key, len(key))
		}
		if len(value) > 512 {
			return fmt.Errorf("value for key '%s' exceeds maximum length of 512 characters (got %d)", key, len(value))
		}
	}

	return nil
}

// validateOutputExpiresAfter validates the output expiration policy:
// - Seconds is required and must be between 3600 (1 hour) and 2592000 (30 days)
func validateOutputExpiresAfter(exp *openai.BatchNewParamsOutputExpiresAfter) error {
	// Seconds must be between 3600 (1 hour) and 2592000 (30 days)
	if exp.Seconds < 3600 {
		return fmt.Errorf("seconds must be at least 3600 (1 hour), got %d", exp.Seconds)
	}
	if exp.Seconds > 2592000 {
		return fmt.Errorf("seconds must be at most 2592000 (30 days), got %d", exp.Seconds)
	}
	return nil
}
