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

package types

import (
	"github.com/openai/openai-go"
)

// ChatCompletionRequest extends OpenAI's ChatCompletionNewParams with vLLM-specific parameters
// Reference: https://github.com/vllm-project/vllm/blob/main/vllm/entrypoints/openai/chat_completion/protocol.py
type ChatCompletionRequest struct {
	openai.ChatCompletionNewParams

	// vLLM-specific parameters

	// AddGenerationPrompt controls whether to add the generation prompt to the chat template.
	// Default: true (vLLM default)
	// Reference: https://github.com/vllm-project/vllm/blob/main/vllm/entrypoints/openai/chat_completion/protocol.py
	AddGenerationPrompt *bool `json:"add_generation_prompt,omitempty"`

	// AddSpecialTokens controls whether to add special tokens (e.g. BOS) on top of
	// what is added by the chat template.
	// Default: false (vLLM default) - chat template handles special tokens
	// Reference: https://github.com/vllm-project/vllm/blob/main/vllm/entrypoints/openai/chat_completion/protocol.py
	AddSpecialTokens *bool `json:"add_special_tokens,omitempty"`

	// ReturnTokenStrings controls whether to return token strings in tokenization results.
	// This is used for debugging and verification purposes.
	// Default: false
	// Reference: https://github.com/vllm-project/vllm/blob/main/vllm/entrypoints/openai/chat_completion/protocol.py
	ReturnTokenStrings *bool `json:"return_token_strs,omitempty"`
}
