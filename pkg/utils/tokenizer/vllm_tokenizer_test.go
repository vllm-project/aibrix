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

package tokenizer

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestVLLMTokenizerConfig tests the configuration validation
func TestVLLMTokenizerConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  VLLMTokenizerConfig
		wantErr bool
	}{
		{
			name: "valid config",
			config: VLLMTokenizerConfig{
				BaseURL:    "http://localhost:8000",
				Model:      "meta-llama/Llama-2-7b-hf",
				Timeout:    30,
				MaxRetries: 3,
			},
			wantErr: false,
		},
		{
			name: "empty base URL",
			config: VLLMTokenizerConfig{
				BaseURL: "",
			},
			wantErr: true,
		},
		{
			name: "zero timeout sets default",
			config: VLLMTokenizerConfig{
				BaseURL: "http://localhost:8000",
				Timeout: 0,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := tt.config // Make a copy
			err := validateVLLMConfig(&config)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateVLLMConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && tt.config.Timeout == 0 {
				if config.Timeout != 30 {
					t.Errorf("Expected default timeout 30, got %d", config.Timeout)
				}
			}
		})
	}
}

// TestVLLMTokenizerTokenizeInputText tests the backward compatible tokenization
func TestVLLMTokenizerTokenizeInputText(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			t.Errorf("Expected path /tokenize, got %s", r.URL.Path)
		}

		// Decode request
		var req VLLMTokenizeCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("Failed to decode request: %v", err)
		}

		// Verify request
		if req.Prompt != "Hello, world!" {
			t.Errorf("Expected prompt 'Hello, world!', got %s", req.Prompt)
		}

		// Send response
		resp := VLLMTokenizeResponse{
			Count:       3,
			MaxModelLen: 4096,
			Tokens:      []int{15339, 1919, 999},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	// Create tokenizer
	config := VLLMTokenizerConfig{
		BaseURL:          server.URL,
		Timeout:          10,
		MaxRetries:       1,
		AddSpecialTokens: true,
	}
	tokenizer, err := NewVLLMTokenizer(config)
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	// Test tokenization
	result, err := tokenizer.TokenizeInputText("Hello, world!")
	if err != nil {
		t.Fatalf("TokenizeInputText failed: %v", err)
	}

	// Verify result
	expectedTokens := []int{15339, 1919, 999}
	if len(result) != len(expectedTokens)*4 {
		t.Errorf("Expected %d bytes, got %d", len(expectedTokens)*4, len(result))
	}

	// Verify token encoding
	for i, expectedToken := range expectedTokens {
		token := binary.BigEndian.Uint32(result[i*4:])
		if int(token) != expectedToken {
			t.Errorf("Token %d: expected %d, got %d", i, expectedToken, token)
		}
	}
}

// TestVLLMTokenizerTokenizeWithOptions tests advanced tokenization
func TestVLLMTokenizerTokenizeWithOptions(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			t.Errorf("Expected path /tokenize, got %s", r.URL.Path)
		}

		// Check content type to determine request type
		body := make(map[string]interface{})
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("Failed to decode request body: %v", err)
		}

		var resp VLLMTokenizeResponse
		if _, hasMessages := body["messages"]; hasMessages {
			// Chat request
			resp = VLLMTokenizeResponse{
				Count:       5,
				MaxModelLen: 4096,
				Tokens:      []int{1, 2, 3, 4, 5},
				TokenStrs:   []string{"<s>", "Hello", ",", "world", "!"},
			}
		} else {
			// Completion request
			resp = VLLMTokenizeResponse{
				Count:       3,
				MaxModelLen: 4096,
				Tokens:      []int{15339, 1919, 999},
				TokenStrs:   []string{"Hello", ",", "world!"},
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	// Create tokenizer
	config := VLLMTokenizerConfig{
		BaseURL:    server.URL,
		Timeout:    10,
		MaxRetries: 1,
	}
	tokenizer, err := NewVLLMTokenizer(config)
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	ctx := context.Background()

	// Test completion tokenization
	t.Run("completion", func(t *testing.T) {
		input := TokenizeInput{
			Type:               CompletionInput,
			Text:               "Hello, world!",
			AddSpecialTokens:   true,
			ReturnTokenStrings: true,
		}
		result, err := tokenizer.TokenizeWithOptions(ctx, input)
		if err != nil {
			t.Fatalf("TokenizeWithOptions failed: %v", err)
		}

		if result.Count != 3 {
			t.Errorf("Expected count 3, got %d", result.Count)
		}
		if len(result.Tokens) != 3 {
			t.Errorf("Expected 3 tokens, got %d", len(result.Tokens))
		}
		if len(result.TokenStrings) != 3 {
			t.Errorf("Expected 3 token strings, got %d", len(result.TokenStrings))
		}
	})

	// Test chat tokenization
	t.Run("chat", func(t *testing.T) {
		input := TokenizeInput{
			Type: ChatInput,
			Messages: []ChatMessage{
				{Role: "user", Content: "Hello!"},
				{Role: "assistant", Content: "Hi there!"},
			},
			AddSpecialTokens:    true,
			AddGenerationPrompt: true,
			ReturnTokenStrings:  true,
		}
		result, err := tokenizer.TokenizeWithOptions(ctx, input)
		if err != nil {
			t.Fatalf("TokenizeWithOptions failed: %v", err)
		}

		if result.Count != 5 {
			t.Errorf("Expected count 5, got %d", result.Count)
		}
	})
}

// TestVLLMTokenizerDetokenize tests detokenization
func TestVLLMTokenizerDetokenize(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/detokenize" {
			t.Errorf("Expected path /detokenize, got %s", r.URL.Path)
		}

		// Decode request
		var req VLLMDetokenizeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("Failed to decode request: %v", err)
		}

		// Send response
		resp := VLLMDetokenizeResponse{
			Prompt: "Hello, world!",
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	// Create tokenizer
	config := VLLMTokenizerConfig{
		BaseURL:    server.URL,
		Timeout:    10,
		MaxRetries: 1,
	}
	tokenizer, err := NewVLLMTokenizer(config)
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	// Test detokenization
	ctx := context.Background()
	tokens := []int{15339, 1919, 999}
	result, err := tokenizer.Detokenize(ctx, tokens)
	if err != nil {
		t.Fatalf("Detokenize failed: %v", err)
	}

	if result != "Hello, world!" {
		t.Errorf("Expected 'Hello, world!', got %s", result)
	}
}

// TestVLLMTokenizerRetry tests retry logic
func TestVLLMTokenizerRetry(t *testing.T) {
	attempts := 0
	// Create test server that fails initially
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Success on third attempt
		resp := VLLMTokenizeResponse{
			Count:       1,
			MaxModelLen: 4096,
			Tokens:      []int{123},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	// Create tokenizer with retries
	config := VLLMTokenizerConfig{
		BaseURL:    server.URL,
		Timeout:    10,
		MaxRetries: 3,
	}
	tokenizer, err := NewVLLMTokenizer(config)
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	// Test tokenization with retries
	_, err = tokenizer.TokenizeInputText("test")
	if err != nil {
		t.Fatalf("TokenizeInputText failed after retries: %v", err)
	}

	if attempts != 3 {
		t.Errorf("Expected 3 attempts, got %d", attempts)
	}
}

// BenchmarkVLLMTokenizerTokenizeInputText benchmarks tokenization
func BenchmarkVLLMTokenizerTokenizeInputText(b *testing.B) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := VLLMTokenizeResponse{
			Count:       10,
			MaxModelLen: 4096,
			Tokens:      []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			b.Errorf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	// Create tokenizer
	config := VLLMTokenizerConfig{
		BaseURL:    server.URL,
		Timeout:    10,
		MaxRetries: 0,
	}
	tokenizer, err := NewVLLMTokenizer(config)
	if err != nil {
		b.Fatalf("Failed to create tokenizer: %v", err)
	}

	text := "This is a sample text for benchmarking the vLLM tokenizer implementation."

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := tokenizer.TokenizeInputText(text)
		if err != nil {
			b.Fatalf("TokenizeInputText failed: %v", err)
		}
	}
}

// TestVLLMTokenizerTimeout tests request timeout
func TestVLLMTokenizerTimeout(t *testing.T) {
	// Create test server with delay
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // Delay longer than timeout
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create tokenizer with short timeout
	config := VLLMTokenizerConfig{
		BaseURL:    server.URL,
		Timeout:    1, // 1 second timeout
		MaxRetries: 0,
	}
	tokenizer, err := NewVLLMTokenizer(config)
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	// Test should timeout
	_, err = tokenizer.TokenizeInputText("test")
	if err == nil {
		t.Fatal("Expected timeout error, got nil")
	}
}
