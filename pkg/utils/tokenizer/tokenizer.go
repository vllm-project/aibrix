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
	"fmt"
)

// Factory functions moved to factory.go
// This file now only contains the backward-compatible NewTokenizer function

// NewTokenizer creates a tokenizer instance based on the provided type
// Deprecated: Use specific factory functions like NewRemoteTokenizer, NewTiktokenTokenizer, or NewCharacterTokenizer instead
func NewTokenizer(tokenizerType string, config interface{}) (Tokenizer, error) {
	switch tokenizerType {
	case "tiktoken":
		// Tiktoken doesn't require configuration
		return NewTiktokenTokenizer(), nil

	case "character":
		// Character tokenizer doesn't require configuration
		return NewCharacterTokenizer(), nil

	case "vllm":
		// For backward compatibility, use VLLMTokenizer
		vllmConfig, ok := config.(VLLMTokenizerConfig)
		if !ok {
			return nil, fmt.Errorf("invalid config type for vLLM tokenizer")
		}
		return NewVLLMTokenizer(vllmConfig)

	case "remote":
		// Generic remote tokenizer
		remoteConfig, ok := config.(RemoteTokenizerConfig)
		if !ok {
			return nil, fmt.Errorf("invalid config type for remote tokenizer")
		}
		return NewRemoteTokenizer(remoteConfig)

	default:
		return nil, fmt.Errorf("unsupported tokenizer type: %s", tokenizerType)
	}
}
