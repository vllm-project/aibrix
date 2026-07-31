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

package common

import (
	"context"
	"encoding/json"

	"github.com/openai/openai-go/v3"
	"k8s.io/klog/v2"
)

type ctxKey string

const includeDeploymentKey ctxKey = "include_deployment"

// WithIncludeDeployment attaches the include_deployment flag to the context.
func WithIncludeDeployment(ctx context.Context, val bool) context.Context {
	return context.WithValue(ctx, includeDeploymentKey, val)
}

// IncludeDeploymentFromCtx returns whether deployment detail should be fetched.
func IncludeDeploymentFromCtx(ctx context.Context) bool {
	v, _ := ctx.Value(includeDeploymentKey).(bool)
	return v
}

// Console-owned fields we stash on the OpenAI batch.metadata map. Namespaced
// to keep them out of user-supplied metadata's key space. The bare
// "display_name" key is kept for backwards compatibility with batches
// created by older console builds.
const (
	MetadataDisplayName            = "display_name" // legacy fallback
	MetadataConsoleDisplayName     = "aibrix.console.display_name"
	MetadataConsoleCreatedBy       = "aibrix.console.created_by"
	MetadataConsoleTemplateName    = "aibrix.console.template_name"
	MetadataConsoleTemplateVersion = "aibrix.console.template_version"
	BatchExtraBodyField            = "extra_body"
	AIBrixExtraBodyField           = "aibrix"
	JsonNullLiteral                = "null"
)

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

func ParseBatchExtraBody(b *openai.Batch) map[string]json.RawMessage {
	if b == nil || b.RawJSON() == "" {
		return nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(b.RawJSON()), &root); err != nil {
		klog.Errorf("ParseBatchExtraBody: failed to parse batch metadata: %v", err)
		return nil
	}
	out := make(map[string]json.RawMessage)
	if raw, ok := root[BatchExtraBodyField]; ok && len(raw) > 0 && string(raw) != JsonNullLiteral {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(raw, &extra); err == nil {
			for k, v := range extra {
				if len(v) > 0 && string(v) != JsonNullLiteral {
					out[k] = v
				}
			}
		} else {
			klog.Errorf("ParseBatchExtraBody: failed to parse extraBody: %v", err)
		}
	}
	for k, v := range root {
		if k == BatchExtraBodyField {
			continue
		}
		if _, standard := standardBatchResponseFields[k]; standard {
			continue
		}
		if len(v) > 0 && string(v) != JsonNullLiteral {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// InjectExtraBodyToBatch injects AIBrixExtraBody into the batch's raw JSON.
// The extraBody parameter is the raw AIBrixExtraBody JSON, which will be wrapped
// under extra_body.aibrix in the batch.
// Uses fast path (direct byte manipulation) when possible, falls back to map-based approach.
func InjectExtraBodyToBatch(batch *openai.Batch, extraBody json.RawMessage) *openai.Batch {
	if batch == nil || len(extraBody) == 0 {
		return batch
	}

	// Prefer existing RawJSON to avoid redundant marshal
	var batchJSON []byte
	if raw := batch.RawJSON(); raw != "" {
		batchJSON = []byte(raw)
	} else {
		var err error
		batchJSON, err = json.Marshal(batch)
		if err != nil {
			klog.Errorf("InjectExtraBodyToBatch: failed to marshal batch: %v", err)
			return batch
		}
	}

	// Build the wrapped extra_body structure: {"aibrix": <extraBody>}
	wrappedExtraBody := make([]byte, 0, len(AIBrixExtraBodyField)+len(extraBody)+10)
	wrappedExtraBody = append(wrappedExtraBody, '{', '"')
	wrappedExtraBody = append(wrappedExtraBody, AIBrixExtraBodyField...)
	wrappedExtraBody = append(wrappedExtraBody, '"', ':')
	wrappedExtraBody = append(wrappedExtraBody, extraBody...)
	wrappedExtraBody = append(wrappedExtraBody, '}')

	// Fast path: direct byte manipulation for valid JSON objects
	if len(batchJSON) >= 2 && batchJSON[0] == '{' && batchJSON[len(batchJSON)-1] == '}' {
		var finalJSON []byte
		if len(batchJSON) == 2 {
			// Empty object {} - build directly
			finalJSON = make([]byte, 0, 3+len(BatchExtraBodyField)+len(wrappedExtraBody))
			finalJSON = append(finalJSON, '{', '"')
			finalJSON = append(finalJSON, BatchExtraBodyField...)
			finalJSON = append(finalJSON, '"', ':')
			finalJSON = append(finalJSON, wrappedExtraBody...)
			finalJSON = append(finalJSON, '}')
		} else {
			// Non-empty object - insert before closing brace
			finalJSON = make([]byte, 0, len(batchJSON)+len(BatchExtraBodyField)+len(wrappedExtraBody)+5)
			finalJSON = append(finalJSON, batchJSON[:len(batchJSON)-1]...)
			finalJSON = append(finalJSON, ",\""...)
			finalJSON = append(finalJSON, BatchExtraBodyField...)
			finalJSON = append(finalJSON, "\":"...)
			finalJSON = append(finalJSON, wrappedExtraBody...)
			finalJSON = append(finalJSON, '}')
		}

		var newBatch openai.Batch
		if err := json.Unmarshal(finalJSON, &newBatch); err == nil {
			return &newBatch
		}
		// Fast path failed, fall through to slow path
		klog.V(2).Infof("InjectExtraBodyToBatch: fast path failed, using slow path")
	}

	// Slow path: unmarshal to map, add field, marshal back
	var batchMap map[string]json.RawMessage
	if err := json.Unmarshal(batchJSON, &batchMap); err != nil {
		klog.Errorf("InjectExtraBodyToBatch: failed to unmarshal batch: %v", err)
		return batch
	}
	batchMap[BatchExtraBodyField] = json.RawMessage(wrappedExtraBody)
	finalJSON, err := json.Marshal(batchMap)
	if err != nil {
		klog.Errorf("InjectExtraBodyToBatch: failed to marshal batchMap: %v", err)
		return batch
	}

	var newBatch openai.Batch
	if err := json.Unmarshal(finalJSON, &newBatch); err != nil {
		klog.Errorf("InjectExtraBodyToBatch: failed to unmarshal batch: %v", err)
		return batch
	}
	return &newBatch
}
