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

package gateway

import (
	"testing"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func TestExtractChatMessages_SimpleText(t *testing.T) {
	requestBody := `{
		"model": "test-model",
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`

	var chatReq struct {
		Model    string                      `json:"model"`
		Messages []map[string]interface{}    `json:"messages"`
	}
	err := sonic.Unmarshal([]byte(requestBody), &chatReq)
	if err != nil {
		t.Fatalf("Failed to unmarshal request: %v", err)
	}

	// Test validateRequestBody
	model, message, messages, stream, _, _, errRes := validateRequestBody(
		"test-req-1",
		PathChatCompletions,
		[]byte(requestBody),
		utils.User{},
	)

	if errRes != nil {
		t.Fatalf("validateRequestBody failed: %v", errRes)
	}

	if model != "test-model" {
		t.Errorf("Expected model 'test-model', got '%s'", model)
	}

	if len(messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(messages))
	}

	if messages[0].Role != "user" {
		t.Errorf("Expected role 'user', got '%s'", messages[0].Role)
	}

	// Verify content is stored as json.RawMessage
	var contentStr string
	err = sonic.Unmarshal(messages[0].Content, &contentStr)
	if err != nil {
		t.Fatalf("Failed to unmarshal content as string: %v", err)
	}

	if contentStr != "Hello" {
		t.Errorf("Expected content 'Hello', got '%s'", contentStr)
	}

	// Verify backward compatibility - message should be set
	if message != "Hello" {
		t.Errorf("Expected message 'Hello', got '%s'", message)
	}

	if stream {
		t.Errorf("Expected stream false, got true")
	}
}

func TestExtractChatMessages_Multimodal(t *testing.T) {
	requestBody := `{
		"model": "test-model",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "What's in this image?"},
					{"type": "image_url", "image_url": {"url": "https://example.com/image.jpg"}}
				]
			}
		]
	}`

	model, message, messages, stream, _, _, errRes := validateRequestBody(
		"test-req-2",
		PathChatCompletions,
		[]byte(requestBody),
		utils.User{},
	)

	if errRes != nil {
		t.Fatalf("validateRequestBody failed: %v", errRes)
	}

	if model != "test-model" {
		t.Errorf("Expected model 'test-model', got '%s'", model)
	}

	if len(messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(messages))
	}

	// Verify content is an array
	var contentArray []interface{}
	err := sonic.Unmarshal(messages[0].Content, &contentArray)
	if err != nil {
		t.Fatalf("Content should be unmarshallable as array: %v", err)
	}

	if len(contentArray) != 2 {
		t.Errorf("Expected 2 content parts, got %d", len(contentArray))
	}

	// Verify message field has content (for backward compatibility)
	if message == "" {
		t.Errorf("Expected non-empty message field for backward compatibility")
	}

	if stream {
		t.Errorf("Expected stream false, got true")
	}
}

func TestExtractChatMessages_Conversation(t *testing.T) {
	requestBody := `{
		"model": "test-model",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi! How can I help you?"},
			{"role": "user", "content": "Tell me a joke"}
		],
		"stream": true
	}`

	model, message, messages, stream, _, _, errRes := validateRequestBody(
		"test-req-3",
		PathChatCompletions,
		[]byte(requestBody),
		utils.User{},
	)

	if errRes != nil {
		t.Fatalf("validateRequestBody failed: %v", errRes)
	}

	if model != "test-model" {
		t.Errorf("Expected model 'test-model', got '%s'", model)
	}

	if len(messages) != 4 {
		t.Fatalf("Expected 4 messages, got %d", len(messages))
	}

	expectedRoles := []string{"system", "user", "assistant", "user"}
	for i, msg := range messages {
		if msg.Role != expectedRoles[i] {
			t.Errorf("Message %d: expected role '%s', got '%s'", i, expectedRoles[i], msg.Role)
		}
	}

	// Verify backward compatibility - message contains all content
	if message == "" {
		t.Errorf("Expected non-empty message field")
	}

	if !stream {
		t.Errorf("Expected stream true, got false")
	}
}

func TestExtractChatMessages_EmptyMessages(t *testing.T) {
	requestBody := `{
		"model": "test-model",
		"messages": []
	}`

	_, _, _, _, _, _, errRes := validateRequestBody(
		"test-req-4",
		PathChatCompletions,
		[]byte(requestBody),
		utils.User{},
	)

	if errRes == nil {
		t.Errorf("Expected error for empty messages, got nil")
	}
}

func TestExtractChatMessages_MixedMultimodal(t *testing.T) {
	requestBody := `{
		"model": "test-model",
		"messages": [
			{"role": "user", "content": "Hello"},
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "What's this?"},
					{"type": "image_url", "image_url": {"url": "https://example.com/image.jpg"}}
				]
			},
			{"role": "assistant", "content": "That's an image."}
		]
	}`

	model, message, messages, stream, _, _, errRes := validateRequestBody(
		"test-req-5",
		PathChatCompletions,
		[]byte(requestBody),
		utils.User{},
	)

	if errRes != nil {
		t.Fatalf("validateRequestBody failed: %v", errRes)
	}

	if len(messages) != 3 {
		t.Fatalf("Expected 3 messages, got %d", len(messages))
	}

	// First message should have string content
	var content1 string
	err := sonic.Unmarshal(messages[0].Content, &content1)
	if err != nil {
		t.Fatalf("First message should have string content: %v", err)
	}

	// Second message should have array content
	var content2 []interface{}
	err = sonic.Unmarshal(messages[1].Content, &content2)
	if err != nil {
		t.Fatalf("Second message should have array content: %v", err)
	}

	if len(content2) != 2 {
		t.Errorf("Second message should have 2 content parts, got %d", len(content2))
	}

	// Third message should have string content
	var content3 string
	err = sonic.Unmarshal(messages[2].Content, &content3)
	if err != nil {
		t.Fatalf("Third message should have string content: %v", err)
	}

	if model != "test-model" {
		t.Errorf("Expected model 'test-model', got '%s'", model)
	}

	if message == "" {
		t.Errorf("Expected non-empty message field")
	}

	if stream {
		t.Errorf("Expected stream false, got true")
	}
}

func TestExtractChatMessages_AudioMultimodal(t *testing.T) {
	requestBody := `{
		"model": "test-model",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "audio_url", "audio_url": {"url": "data:audio/wav;base64,..."}}
				]
			}
		]
	}`

	_, _, messages, _, _, _, errRes := validateRequestBody(
		"test-req-6",
		PathChatCompletions,
		[]byte(requestBody),
		utils.User{},
	)

	if errRes != nil {
		t.Fatalf("validateRequestBody failed: %v", errRes)
	}

	if len(messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(messages))
	}

	// Debug: print the actual content
	t.Logf("Message Content: %s", string(messages[0].Content))

	if len(messages[0].Content) == 0 {
		t.Fatalf("Content is empty!")
	}

	// Verify content is an array
	var contentArray []interface{}
	err := sonic.Unmarshal(messages[0].Content, &contentArray)
	if err != nil {
		t.Fatalf("Content should be unmarshallable as array: %v (Content was: %s)", err, string(messages[0].Content))
	}

	if len(contentArray) != 1 {
		t.Errorf("Expected 1 content part, got %d", len(contentArray))
	}

	// Verify the audio_url structure
	contentPart := contentArray[0].(map[string]interface{})
	if contentPart["type"] != "audio_url" {
		t.Errorf("Expected type 'audio_url', got '%v'", contentPart["type"])
	}
}

func TestChatMessagesRoundtrip(t *testing.T) {
	// Test that messages can be extracted and re-serialized correctly
	requestBody := `{
		"model": "test-model",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Describe these:"},
					{"type": "image_url", "image_url": {"url": "https://example.com/img1.jpg"}},
					{"type": "image_url", "image_url": {"url": "https://example.com/img2.jpg"}}
				]
			}
		]
	}`

	_, _, messages, _, _, _, errRes := validateRequestBody(
		"test-req-7",
		PathChatCompletions,
		[]byte(requestBody),
		utils.User{},
	)

	if errRes != nil {
		t.Fatalf("validateRequestBody failed: %v", errRes)
	}

	// Re-serialize the messages
	reserializedMessages, err := sonic.Marshal(messages)
	if err != nil {
		t.Fatalf("Failed to re-serialize messages: %v", err)
	}

	// Unmarshal back to verify structure is preserved
	var messages2 []types.ChatMessage
	err = sonic.Unmarshal(reserializedMessages, &messages2)
	if err != nil {
		t.Fatalf("Failed to unmarshal re-serialized messages: %v", err)
	}

	if len(messages2) != 1 {
		t.Fatalf("Expected 1 message after roundtrip, got %d", len(messages2))
	}

	// Verify content is still an array
	var contentArray []interface{}
	err = sonic.Unmarshal(messages2[0].Content, &contentArray)
	if err != nil {
		t.Fatalf("Content should still be an array after roundtrip: %v", err)
	}

	if len(contentArray) != 3 {
		t.Errorf("Expected 3 content parts after roundtrip, got %d", len(contentArray))
	}
}
