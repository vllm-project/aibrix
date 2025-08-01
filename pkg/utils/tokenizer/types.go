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
	"time"
)

// TokenizeInputType represents the type of tokenization input
type TokenizeInputType string

const (
	// CompletionInput represents simple text completion tokenization
	CompletionInput TokenizeInputType = "completion"
	// ChatInput represents chat message tokenization with templates
	ChatInput TokenizeInputType = "chat"
)

// TokenizeInput represents a unified input structure for tokenization
type TokenizeInput struct {
	Type                TokenizeInputType
	Text                string        // For completion input
	Messages            []ChatMessage // For chat input
	AddSpecialTokens    bool
	ReturnTokenStrings  bool
	AddGenerationPrompt bool // For chat input only
}

// TokenizeResult represents the result of tokenization
type TokenizeResult struct {
	Count        int      `json:"count"`
	MaxModelLen  int      `json:"max_model_len"`
	Tokens       []int    `json:"tokens"`
	TokenStrings []string `json:"token_strs,omitempty"`
}

// ChatMessage represents a single message in a chat conversation
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// RemoteTokenizerConfig represents configuration for a remote tokenizer
type RemoteTokenizerConfig struct {
	Engine             string        // "vllm", "sglang", "triton", etc.
	Endpoint           string        // Base URL of the service
	Model              string        // Model identifier (optional)
	Timeout            time.Duration // Request timeout
	MaxRetries         int           // Max retry attempts
	AddSpecialTokens   bool          // Default: true
	ReturnTokenStrings bool          // Default: false
}

// vllmTokenizeCompletionRequest represents a request to tokenize completion text
type vllmTokenizeCompletionRequest struct {
	Model            string `json:"model,omitempty"`
	Prompt           string `json:"prompt"`
	AddSpecialTokens *bool  `json:"add_special_tokens,omitempty"`
	ReturnTokenStrs  *bool  `json:"return_token_strs,omitempty"`
}

// vllmTokenizeChatRequest represents a request to tokenize chat messages
type vllmTokenizeChatRequest struct {
	Model                string                 `json:"model,omitempty"`
	Messages             []ChatMessage          `json:"messages"`
	AddSpecialTokens     *bool                  `json:"add_special_tokens,omitempty"`
	AddGenerationPrompt  *bool                  `json:"add_generation_prompt,omitempty"`
	ContinueFinalMessage *bool                  `json:"continue_final_message,omitempty"`
	ReturnTokenStrs      *bool                  `json:"return_token_strs,omitempty"`
	ChatTemplate         *string                `json:"chat_template,omitempty"`
	ChatTemplateKwargs   map[string]interface{} `json:"chat_template_kwargs,omitempty"`
	Tools                []interface{}          `json:"tools,omitempty"`
	MMProcessorKwargs    map[string]interface{} `json:"mm_processor_kwargs,omitempty"`
}

// vllmTokenizeResponse represents the response from tokenization endpoints
type vllmTokenizeResponse struct {
	Count       int      `json:"count"`
	MaxModelLen int      `json:"max_model_len"`
	Tokens      []int    `json:"tokens"`
	TokenStrs   []string `json:"token_strs,omitempty"`
}

// vllmDetokenizeRequest represents a request to detokenize tokens
type vllmDetokenizeRequest struct {
	Model  string `json:"model,omitempty"`
	Tokens []int  `json:"tokens"`
}

// vllmDetokenizeResponse represents the response from detokenization endpoint
type vllmDetokenizeResponse struct {
	Prompt string `json:"prompt"`
}
