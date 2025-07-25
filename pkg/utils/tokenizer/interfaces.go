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

import "context"

// Tokenizer is the basic tokenizer interface for backward compatibility
type Tokenizer interface {
	TokenizeInputText(string) ([]byte, error)
}

// extendedTokenizer represents an extended tokenizer interface with advanced features
// Advanced features include:
//   - Context-aware tokenization with cancellation support
//   - Tokenization with custom options (special tokens, generation prompts)
//   - Detokenization support for converting tokens back to text
//
// TODO: Consider simplifying the interface hierarchy by removing this intermediate layer
// if only remoteTokenizer needs these advanced features
type extendedTokenizer interface {
	Tokenizer // Embed existing interface for backward compatibility

	// TokenizeWithOptions performs tokenization with advanced options
	TokenizeWithOptions(ctx context.Context, input TokenizeInput) (*TokenizeResult, error)

	// Detokenize converts tokens back to text
	Detokenize(ctx context.Context, tokens []int) (string, error)
}

// remoteTokenizer interface extends extendedTokenizer with remote-specific methods
type remoteTokenizer interface {
	extendedTokenizer
	GetEndpoint() string
	IsHealthy(ctx context.Context) bool
	Close() error
}

// engineAdapter handles engine-specific differences for remote tokenizers
type engineAdapter interface {
	// Request preparation
	PrepareTokenizeRequest(input TokenizeInput) (interface{}, error)
	PrepareDetokenizeRequest(tokens []int) (interface{}, error)

	// Response parsing
	ParseTokenizeResponse(data []byte) (*TokenizeResult, error)
	ParseDetokenizeResponse(data []byte) (string, error)

	// Endpoint information
	GetTokenizePath() string
	GetDetokenizePath() string

	// Capabilities
	SupportsTokenization() bool
	SupportsDetokenization() bool
	SupportsChat() bool
}
