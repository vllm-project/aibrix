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
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestInjectExtraBodyToBatch(t *testing.T) {
	tests := []struct {
		name      string
		batch     *openai.Batch
		extraBody json.RawMessage
		wantNil   bool
	}{
		{
			name:      "nil batch returns nil",
			batch:     nil,
			extraBody: json.RawMessage(`{"key":"value"}`),
			wantNil:   true,
		},
		{
			name:      "empty extraBody returns original batch",
			batch:     &openai.Batch{ID: "batch_123"},
			extraBody: nil,
			wantNil:   false,
		},
		{
			name:      "empty extraBody string returns original batch",
			batch:     &openai.Batch{ID: "batch_123"},
			extraBody: json.RawMessage{},
			wantNil:   false,
		},
		{
			name: "inject into empty object",
			batch: func() *openai.Batch {
				var b openai.Batch
				_ = json.Unmarshal([]byte(`{}`), &b)
				return &b
			}(),
			extraBody: json.RawMessage(`{"runtime":{"target":"default"}}`),
			wantNil:   false,
		},
		{
			name: "inject into non-empty object with RawJSON",
			batch: func() *openai.Batch {
				var b openai.Batch
				_ = json.Unmarshal([]byte(`{"id":"batch_123","status":"validating"}`), &b)
				return &b
			}(),
			extraBody: json.RawMessage(`{"runtime":{"target":"default"}}`),
			wantNil:   false,
		},
		{
			name: "inject into batch without RawJSON (slow path)",
			batch: &openai.Batch{
				ID:     "batch_456",
				Status: openai.BatchStatusValidating,
				Model:  "gpt-4",
			},
			extraBody: json.RawMessage(`{"job_id":"job_789"}`),
			wantNil:   false,
		},
		{
			name: "inject complex extraBody",
			batch: func() *openai.Batch {
				var b openai.Batch
				_ = json.Unmarshal([]byte(`{"id":"batch_complex","metadata":{"key":"value"}}`), &b)
				return &b
			}(),
			extraBody: json.RawMessage(`{"job_id":"job_x","runtime":{"target":"kubernetes","options":{"namespace":"default"}},"model":"gpt-4"}`),
			wantNil:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := InjectExtraBodyToBatch(tt.batch, tt.extraBody)

			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil result, got non-nil")
				}
				return
			}

			if result == nil {
				t.Errorf("expected non-nil result, got nil")
				return
			}

			// Verify extra_body was injected
			extraBody := ParseBatchExtraBody(result)
			if extraBody == nil && len(tt.extraBody) > 0 {
				t.Errorf("expected extra_body to be injected, but ParseBatchExtraBody returned nil")
			}
		})
	}
}

func TestInjectExtraBodyToBatchRoundTrip(t *testing.T) {
	// Test that injected extra_body can be extracted correctly
	// extra_body structure: {"aibrix": {"job_id": ..., "runtime": ...}}
	originalBatch := func() *openai.Batch {
		var b openai.Batch
		_ = json.Unmarshal([]byte(`{"id":"batch_rt","status":"in_progress","model":"gpt-4","metadata":{"created_by":"test"}}`), &b)
		return &b
	}()

	// AIBrixExtraBody JSON (without wrapping)
	aibrixPayload := map[string]any{
		"job_id": "job_rt",
		"runtime": map[string]any{
			"target":  "default",
			"options": map[string]any{"namespace": "aibrix"},
		},
	}
	aibrixJSON, _ := json.Marshal(aibrixPayload)

	// InjectExtraBodyToBatch will wrap it as {"aibrix": <aibrixJSON>}
	result := InjectExtraBodyToBatch(originalBatch, aibrixJSON)
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Extract the extra body
	extracted := ParseBatchExtraBody(result)
	if extracted == nil {
		t.Fatal("expected extra_body to be extracted")
	}

	// Verify the extracted content - "aibrix" should be present
	if string(extracted["aibrix"]) != string(aibrixJSON) {
		t.Errorf("expected aibrix to match, got %s", string(extracted["aibrix"]))
	}
}

func TestParseBatchExtraBody(t *testing.T) {
	tests := []struct {
		name    string
		batch   *openai.Batch
		wantNil bool
	}{
		{
			name:    "nil batch returns nil",
			batch:   nil,
			wantNil: true,
		},
		{
			name: "batch without extra_body returns nil",
			batch: func() *openai.Batch {
				var b openai.Batch
				_ = json.Unmarshal([]byte(`{"id":"batch_123"}`), &b)
				return &b
			}(),
			wantNil: true,
		},
		{
			name: "batch with extra_body returns parsed content",
			batch: func() *openai.Batch {
				var b openai.Batch
				_ = json.Unmarshal([]byte(`{"id":"batch_123","extra_body":{"runtime":{"target":"default"}}}`), &b)
				return &b
			}(),
			wantNil: false,
		},
		{
			name: "batch with non-standard fields extracts them",
			batch: func() *openai.Batch {
				var b openai.Batch
				_ = json.Unmarshal([]byte(`{"id":"batch_123","custom_field":"custom_value"}`), &b)
				return &b
			}(),
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseBatchExtraBody(tt.batch)

			if tt.wantNil && result != nil {
				t.Errorf("expected nil result, got non-nil")
			}

			if !tt.wantNil && result == nil {
				t.Errorf("expected non-nil result, got nil")
			}
		})
	}
}

func TestInjectExtraBodyToBatchIdempotent(t *testing.T) {
	// Test that injecting extra_body twice doesn't corrupt the batch
	batch := func() *openai.Batch {
		var b openai.Batch
		_ = json.Unmarshal([]byte(`{"id":"batch_idem"}`), &b)
		return &b
	}()

	extraBody := json.RawMessage(`{"key":"value"}`)

	result1 := InjectExtraBodyToBatch(batch, extraBody)
	result2 := InjectExtraBodyToBatch(result1, extraBody)

	if result2 == nil {
		t.Fatal("expected non-nil result after second injection")
	}

	// Both should have extra_body
	extra1 := ParseBatchExtraBody(result1)
	extra2 := ParseBatchExtraBody(result2)

	if extra1 == nil || extra2 == nil {
		t.Fatal("expected both results to have extra_body")
	}
}
