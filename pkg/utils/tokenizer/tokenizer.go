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

// Package tokenizer provides a unified interface for text tokenization
// This file contains the main factory function for creating tokenizer instances

// NewTokenizer creates a tokenizer instance based on the provided type
// Supports "tiktoken", "character", and "remote" tokenizer types
func NewTokenizer(tokenizerType string, config interface{}) (Tokenizer, error) {
	switch tokenizerType {
	case "tiktoken":
		// Tiktoken doesn't require configuration
		return NewTiktokenTokenizer(), nil

	case "character":
		// Character tokenizer doesn't require configuration
		return NewCharacterTokenizer(), nil

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
