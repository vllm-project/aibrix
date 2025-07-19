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
	"fmt"
	"time"
)

const (
	// Default values for VLLMTokenizerConfig
	defaultVLLMTimeout    = 30 // seconds
	defaultVLLMMaxRetries = 3
)

// VLLMTokenizer implements both Tokenizer and ExtendedTokenizer interfaces
// It acts as a proxy to forward tokenization requests to vLLM server
// Deprecated: This is a compatibility wrapper. Use NewRemoteTokenizer instead.
type VLLMTokenizer struct {
	config *VLLMTokenizerConfig
	remote RemoteTokenizer
}

// NewVLLMTokenizer creates a new vLLM tokenizer instance
// Deprecated: Use NewRemoteTokenizer with RemoteTokenizerConfig instead
func NewVLLMTokenizer(config VLLMTokenizerConfig) (*VLLMTokenizer, error) {
	// Validate configuration
	if err := validateVLLMConfig(&config); err != nil {
		return nil, err
	}

	// Convert to RemoteTokenizerConfig
	remoteConfig := RemoteTokenizerConfig{
		Engine:             "vllm",
		Endpoint:           config.BaseURL,
		Model:              config.Model,
		Timeout:            time.Duration(config.Timeout) * time.Second,
		MaxRetries:         config.MaxRetries,
		AddSpecialTokens:   config.AddSpecialTokens,
		ReturnTokenStrings: config.ReturnTokenStrings,
	}

	// Create remote tokenizer
	remote, err := NewRemoteTokenizer(remoteConfig)
	if err != nil {
		return nil, err
	}

	return &VLLMTokenizer{
		config: &config,
		remote: remote,
	}, nil
}

// TokenizeInputText implements the Tokenizer interface for backward compatibility
// It tokenizes the input text and returns the tokens as a byte array (int32 BigEndian)
func (t *VLLMTokenizer) TokenizeInputText(text string) ([]byte, error) {
	return t.remote.TokenizeInputText(text)
}

// TokenizeWithOptions implements the ExtendedTokenizer interface
// It supports both completion and chat tokenization with advanced options
func (t *VLLMTokenizer) TokenizeWithOptions(ctx context.Context, input TokenizeInput) (*TokenizeResult, error) {
	return t.remote.TokenizeWithOptions(ctx, input)
}

// Detokenize implements the ExtendedTokenizer interface
// It converts tokens back to text
func (t *VLLMTokenizer) Detokenize(ctx context.Context, tokens []int) (string, error) {
	return t.remote.Detokenize(ctx, tokens)
}

// String returns a string representation of the tokenizer
func (t *VLLMTokenizer) String() string {
	return fmt.Sprintf("VLLMTokenizer{baseURL=%s, model=%s}", t.config.BaseURL, t.config.Model)
}

// Close closes the underlying remote tokenizer
func (t *VLLMTokenizer) Close() error {
	if t.remote != nil {
		return t.remote.Close()
	}
	return nil
}

// validateVLLMConfig validates the VLLMTokenizerConfig
func validateVLLMConfig(c *VLLMTokenizerConfig) error {
	if c.BaseURL == "" {
		return ErrInvalidConfig{Message: "BaseURL cannot be empty"}
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultVLLMTimeout // Default to 30 seconds
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = defaultVLLMMaxRetries // Default to 3 retries
	}
	return nil
}
