/*
Copyright 2025 The Aibrix Team.

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

package tokenizer

import (
	"encoding/json"
	"testing"
)

func TestChatMessageSerialization(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantRole string
	}{
		{
			name:     "simple text content",
			input:    `{"role":"user","content":"Hello world"}`,
			wantRole: "user",
		},
		{
			name: "multimodal content",
			input: `{"role":"user","content":[
				{"type":"text","text":"What's in this image?"},
				{"type":"image_url","image_url":{"url":"https://example.com/image.jpg"}}
			]}`,
			wantRole: "user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg ChatMessage
			if err := json.Unmarshal([]byte(tt.input), &msg); err != nil {
				t.Fatalf("Failed to unmarshal: %v", err)
			}

			if msg.Role != tt.wantRole {
				t.Errorf("Role = %v, want %v", msg.Role, tt.wantRole)
			}

			// Test that we can marshal it back
			data, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("Failed to marshal: %v", err)
			}

			// Verify it unmarshals again
			var msg2 ChatMessage
			if err := json.Unmarshal(data, &msg2); err != nil {
				t.Fatalf("Failed to unmarshal second time: %v", err)
			}

			if msg2.Role != tt.wantRole {
				t.Errorf("After round-trip, Role = %v, want %v", msg2.Role, tt.wantRole)
			}
		})
	}
}

func TestChatMessageContentPreservation(t *testing.T) {
	// Test that multimodal content structure is preserved
	input := `{"role":"user","content":[{"type":"text","text":"test"},{"type":"image_url","image_url":{"url":"https://example.com/img.jpg"}}]}`

	var msg ChatMessage
	if err := json.Unmarshal([]byte(input), &msg); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	// Marshal back
	output, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	// Unmarshal to generic interface to verify structure
	var inputMap, outputMap map[string]any
	if err := json.Unmarshal([]byte(input), &inputMap); err != nil {
		t.Fatalf("Failed to unmarshal input: %v", err)
	}
	if err := json.Unmarshal(output, &outputMap); err != nil {
		t.Fatalf("Failed to unmarshal output: %v", err)
	}

	// Check that content is an array in both
	inputContent, inputOk := inputMap["content"].([]any)
	outputContent, outputOk := outputMap["content"].([]any)

	if !inputOk || !outputOk {
		t.Fatal("Content should be an array")
	}

	if len(inputContent) != len(outputContent) {
		t.Errorf("Content array length mismatch: input=%d, output=%d", len(inputContent), len(outputContent))
	}
}
