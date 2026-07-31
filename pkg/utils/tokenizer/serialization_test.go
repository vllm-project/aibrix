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

	"github.com/bytedance/sonic"
)

func TestSonicSerializationWithRawMessage(t *testing.T) {
	tests := []struct {
		name    string
		message ChatMessage
	}{
		{
			name: "string content",
			message: ChatMessage{
				Role:    "user",
				Content: json.RawMessage(`"Hello world"`),
			},
		},
		{
			name: "multimodal array content",
			message: ChatMessage{
				Role: "user",
				Content: json.RawMessage(`[
					{"type":"text","text":"What's in this image?"},
					{"type":"image_url","image_url":{"url":"https://example.com/img.jpg"}}
				]`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test sonic.Marshal
			data, err := sonic.Marshal(tt.message)
			if err != nil {
				t.Fatalf("sonic.Marshal failed: %v", err)
			}

			// Verify it can be unmarshaled back
			var result ChatMessage
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatalf("json.Unmarshal failed: %v", err)
			}

			if result.Role != tt.message.Role {
				t.Errorf("Role mismatch: got %v, want %v", result.Role, tt.message.Role)
			}

			// Content should be preserved as-is
			if string(result.Content) != string(tt.message.Content) {
				t.Errorf("Content mismatch:\ngot:  %s\nwant: %s", string(result.Content), string(tt.message.Content))
			}
		})
	}
}

func TestVLLMRequestSerialization(t *testing.T) {
	// Simulate what happens in vLLM adapter
	messages := []ChatMessage{
		{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"text","text":"test"},{"type":"image_url","image_url":{"url":"http://example.com/img.jpg"}}]`),
		},
	}

	addSpecialTokens := false
	addGenerationPrompt := true
	returnTokenStrs := false

	request := struct {
		Messages            []ChatMessage `json:"messages"`
		AddSpecialTokens    *bool         `json:"add_special_tokens,omitempty"`
		AddGenerationPrompt *bool         `json:"add_generation_prompt,omitempty"`
		ReturnTokenStrs     *bool         `json:"return_token_strs,omitempty"`
	}{
		Messages:            messages,
		AddSpecialTokens:    &addSpecialTokens,
		AddGenerationPrompt: &addGenerationPrompt,
		ReturnTokenStrs:     &returnTokenStrs,
	}

	// This is what the HTTP client does
	jsonData, err := sonic.Marshal(request)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	t.Logf("Serialized request:\n%s", string(jsonData))

	// Verify the output is valid JSON and content is preserved
	var result map[string]interface{}
	if err := json.Unmarshal(jsonData, &result); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	// Check messages array
	messagesArray, ok := result["messages"].([]interface{})
	if !ok {
		t.Fatal("messages should be an array")
	}

	if len(messagesArray) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(messagesArray))
	}

	// Check first message has content as array
	firstMsg := messagesArray[0].(map[string]interface{})
	content, ok := firstMsg["content"].([]interface{})
	if !ok {
		t.Fatal("content should be an array for multimodal message")
	}

	if len(content) != 2 {
		t.Fatalf("Expected 2 content parts, got %d", len(content))
	}
}
