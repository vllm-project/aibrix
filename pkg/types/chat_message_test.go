/*
Copyright 2024 The Aibrix Team.

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

package types

import (
	"encoding/json"
	"testing"
)

func TestChatMessage_SimpleText(t *testing.T) {
	// Test simple text message deserialization
	jsonData := `{
		"role": "user",
		"content": "Hello world"
	}`

	var msg ChatMessage
	err := json.Unmarshal([]byte(jsonData), &msg)
	if err != nil {
		t.Fatalf("Failed to unmarshal simple text message: %v", err)
	}

	if msg.Role != "user" {
		t.Errorf("Expected role 'user', got '%s'", msg.Role)
	}

	// Verify content is stored correctly
	expectedContent := `"Hello world"`
	if string(msg.Content) != expectedContent {
		t.Errorf("Expected content '%s', got '%s'", expectedContent, string(msg.Content))
	}

	// Test re-serialization
	reserialized, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Failed to marshal message: %v", err)
	}

	// Verify it can be unmarshaled back
	var msg2 ChatMessage
	err = json.Unmarshal(reserialized, &msg2)
	if err != nil {
		t.Fatalf("Failed to unmarshal re-serialized message: %v", err)
	}

	if msg2.Role != msg.Role || string(msg2.Content) != string(msg.Content) {
		t.Errorf("Re-serialization changed message content")
	}
}

func TestChatMessage_MultimodalImage(t *testing.T) {
	// Test multimodal message with image
	jsonData := `{
		"role": "user",
		"content": [
			{"type": "text", "text": "What's in this image?"},
			{"type": "image_url", "image_url": {"url": "https://example.com/image.jpg", "detail": "high"}}
		]
	}`

	var msg ChatMessage
	err := json.Unmarshal([]byte(jsonData), &msg)
	if err != nil {
		t.Fatalf("Failed to unmarshal multimodal message: %v", err)
	}

	if msg.Role != "user" {
		t.Errorf("Expected role 'user', got '%s'", msg.Role)
	}

	// Verify content is an array
	var contentArray []interface{}
	err = json.Unmarshal(msg.Content, &contentArray)
	if err != nil {
		t.Fatalf("Content should be unmarshallable as array: %v", err)
	}

	if len(contentArray) != 2 {
		t.Errorf("Expected 2 content parts, got %d", len(contentArray))
	}

	// Test re-serialization preserves structure
	reserialized, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Failed to marshal message: %v", err)
	}

	// Verify content is still an array after re-serialization
	var verification map[string]interface{}
	err = json.Unmarshal(reserialized, &verification)
	if err != nil {
		t.Fatalf("Failed to unmarshal verification: %v", err)
	}

	content, ok := verification["content"].([]interface{})
	if !ok {
		t.Errorf("Content should still be an array after re-serialization, got type %T", verification["content"])
	}

	if len(content) != 2 {
		t.Errorf("Expected 2 content parts after re-serialization, got %d", len(content))
	}
}

func TestChatMessage_MultimodalAudio(t *testing.T) {
	// Test multimodal message with audio (vLLM-omni)
	jsonData := `{
		"role": "user",
		"content": [
			{"type": "audio_url", "audio_url": {"url": "data:audio/wav;base64,..."}}
		]
	}`

	var msg ChatMessage
	err := json.Unmarshal([]byte(jsonData), &msg)
	if err != nil {
		t.Fatalf("Failed to unmarshal audio message: %v", err)
	}

	// Verify content is an array
	var contentArray []interface{}
	err = json.Unmarshal(msg.Content, &contentArray)
	if err != nil {
		t.Fatalf("Content should be unmarshallable as array: %v", err)
	}

	if len(contentArray) != 1 {
		t.Errorf("Expected 1 content part, got %d", len(contentArray))
	}

	// Verify the content part structure
	contentPart := contentArray[0].(map[string]interface{})
	if contentPart["type"] != "audio_url" {
		t.Errorf("Expected type 'audio_url', got '%v'", contentPart["type"])
	}
}

func TestChatMessage_MultimodalMixed(t *testing.T) {
	// Test multimodal message with mixed content types
	jsonData := `{
		"role": "user",
		"content": [
			{"type": "text", "text": "Describe this:"},
			{"type": "image_url", "image_url": {"url": "https://example.com/image1.jpg"}},
			{"type": "image_url", "image_url": {"url": "https://example.com/image2.jpg"}},
			{"type": "audio_url", "audio_url": {"url": "data:audio/wav;base64,..."}}
		]
	}`

	var msg ChatMessage
	err := json.Unmarshal([]byte(jsonData), &msg)
	if err != nil {
		t.Fatalf("Failed to unmarshal mixed multimodal message: %v", err)
	}

	// Verify content is an array with 4 elements
	var contentArray []interface{}
	err = json.Unmarshal(msg.Content, &contentArray)
	if err != nil {
		t.Fatalf("Content should be unmarshallable as array: %v", err)
	}

	if len(contentArray) != 4 {
		t.Errorf("Expected 4 content parts, got %d", len(contentArray))
	}
}

func TestChatMessage_SystemMessage(t *testing.T) {
	// Test system message
	jsonData := `{
		"role": "system",
		"content": "You are a helpful assistant."
	}`

	var msg ChatMessage
	err := json.Unmarshal([]byte(jsonData), &msg)
	if err != nil {
		t.Fatalf("Failed to unmarshal system message: %v", err)
	}

	if msg.Role != "system" {
		t.Errorf("Expected role 'system', got '%s'", msg.Role)
	}
}

func TestChatMessage_AssistantMessage(t *testing.T) {
	// Test assistant message
	jsonData := `{
		"role": "assistant",
		"content": "I can help you with that."
	}`

	var msg ChatMessage
	err := json.Unmarshal([]byte(jsonData), &msg)
	if err != nil {
		t.Fatalf("Failed to unmarshal assistant message: %v", err)
	}

	if msg.Role != "assistant" {
		t.Errorf("Expected role 'assistant', got '%s'", msg.Role)
	}
}

func TestChatMessage_ToolMessage(t *testing.T) {
	// Test tool message with JSON content
	jsonData := `{
		"role": "tool",
		"content": "{\"result\": 42, \"status\": \"success\"}"
	}`

	var msg ChatMessage
	err := json.Unmarshal([]byte(jsonData), &msg)
	if err != nil {
		t.Fatalf("Failed to unmarshal tool message: %v", err)
	}

	if msg.Role != "tool" {
		t.Errorf("Expected role 'tool', got '%s'", msg.Role)
	}

	// Verify content is stored as a JSON string
	var contentStr string
	err = json.Unmarshal(msg.Content, &contentStr)
	if err != nil {
		t.Fatalf("Content should be unmarshallable as string: %v", err)
	}

	// The content itself is a JSON string that can be parsed
	var toolResult map[string]interface{}
	err = json.Unmarshal([]byte(contentStr), &toolResult)
	if err != nil {
		t.Fatalf("Tool content should be valid JSON: %v", err)
	}

	if toolResult["result"] != float64(42) {
		t.Errorf("Expected result 42, got %v", toolResult["result"])
	}
}

func TestChatMessage_EmptyContent(t *testing.T) {
	// Test message with empty string content
	jsonData := `{
		"role": "user",
		"content": ""
	}`

	var msg ChatMessage
	err := json.Unmarshal([]byte(jsonData), &msg)
	if err != nil {
		t.Fatalf("Failed to unmarshal empty content message: %v", err)
	}

	expectedContent := `""`
	if string(msg.Content) != expectedContent {
		t.Errorf("Expected content '%s', got '%s'", expectedContent, string(msg.Content))
	}
}

func TestChatMessage_Conversation(t *testing.T) {
	// Test a full conversation with multiple messages
	jsonData := `[
		{
			"role": "system",
			"content": "You are a helpful assistant."
		},
		{
			"role": "user",
			"content": "Hello!"
		},
		{
			"role": "assistant",
			"content": "Hi! How can I help you?"
		},
		{
			"role": "user",
			"content": [
				{"type": "text", "text": "What's in this image?"},
				{"type": "image_url", "image_url": {"url": "https://example.com/image.jpg"}}
			]
		}
	]`

	var messages []ChatMessage
	err := json.Unmarshal([]byte(jsonData), &messages)
	if err != nil {
		t.Fatalf("Failed to unmarshal conversation: %v", err)
	}

	if len(messages) != 4 {
		t.Errorf("Expected 4 messages, got %d", len(messages))
	}

	// Verify roles
	expectedRoles := []string{"system", "user", "assistant", "user"}
	for i, msg := range messages {
		if msg.Role != expectedRoles[i] {
			t.Errorf("Message %d: expected role '%s', got '%s'", i, expectedRoles[i], msg.Role)
		}
	}

	// Verify last message is multimodal
	var contentArray []interface{}
	err = json.Unmarshal(messages[3].Content, &contentArray)
	if err != nil {
		t.Fatalf("Last message should have array content: %v", err)
	}

	if len(contentArray) != 2 {
		t.Errorf("Last message should have 2 content parts, got %d", len(contentArray))
	}
}
