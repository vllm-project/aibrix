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

import "context"

// Tokenizer is the basic tokenizer interface for backward compatibility
type Tokenizer interface {
	TokenizeInputText(string) ([]byte, error)
}

// ExtendedTokenizer represents an extended tokenizer interface with advanced features
type ExtendedTokenizer interface {
	Tokenizer // Embed existing interface for backward compatibility

	// TokenizeWithOptions performs tokenization with advanced options
	TokenizeWithOptions(ctx context.Context, input TokenizeInput) (*TokenizeResult, error)

	// Detokenize converts tokens back to text
	Detokenize(ctx context.Context, tokens []int) (string, error)
}

// TokenizerV2 is an alias for ExtendedTokenizer
// Deprecated: Use ExtendedTokenizer instead
type TokenizerV2 = ExtendedTokenizer

// RemoteTokenizer interface extends ExtendedTokenizer with remote-specific methods
type RemoteTokenizer interface {
	ExtendedTokenizer
	GetEndpoint() string
	IsHealthy(ctx context.Context) bool
	Close() error
}

// EngineAdapter handles engine-specific differences for remote tokenizers
type EngineAdapter interface {
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
