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
	"context"
	"fmt"
	"time"
)

const (
	// Default values for RemoteTokenizerConfig
	defaultRemoteTimeout    = 30 * time.Second
	defaultRemoteMaxRetries = 3
)

// remoteTokenizerImpl implements the remoteTokenizer interface
type remoteTokenizerImpl struct {
	config  RemoteTokenizerConfig
	client  *httpClient
	adapter engineAdapter
}

// NewRemoteTokenizer creates a new remote tokenizer instance
func NewRemoteTokenizer(config RemoteTokenizerConfig) (Tokenizer, error) {
	if err := validateRemoteConfig(&config); err != nil {
		return nil, err
	}

	// Create engine adapter
	var adapter engineAdapter
	switch config.Engine {
	case "vllm":
		adapter = newVLLMAdapter(config.Model)
	case "sglang":
		adapter = newSGLangAdapter(config.Model)
	default:
		return nil, fmt.Errorf("unsupported engine: %s", config.Engine)
	}

	client := newHTTPClient(config.Endpoint, config.Timeout, config.MaxRetries)

	return &remoteTokenizerImpl{
		config:  config,
		client:  client,
		adapter: adapter,
	}, nil
}

// TokenizeInputText implements the basic Tokenizer interface for backward compatibility
func (t *remoteTokenizerImpl) TokenizeInputText(text string) ([]byte, error) {
	if !t.adapter.SupportsTokenization() {
		return nil, ErrUnsupportedOperation{
			Engine:    t.config.Engine,
			Operation: "tokenization",
		}
	}

	ctx := context.Background()
	input := TokenizeInput{
		Type:             CompletionInput,
		Text:             text,
		AddSpecialTokens: t.config.AddSpecialTokens,
	}

	result, err := t.TokenizeWithOptions(ctx, input)
	if err != nil {
		return nil, err
	}

	// Convert tokens to byte array (BigEndian int32 format)
	return intToByteArray(result.Tokens), nil
}

// TokenizeWithOptions performs tokenization with advanced options
func (t *remoteTokenizerImpl) TokenizeWithOptions(ctx context.Context, input TokenizeInput) (*TokenizeResult, error) {
	if !t.adapter.SupportsTokenization() {
		return nil, ErrUnsupportedOperation{
			Engine:    t.config.Engine,
			Operation: "tokenization",
		}
	}

	// Apply defaults if not specified
	if input.Type == ChatInput && !t.adapter.SupportsChat() {
		return nil, ErrUnsupportedOperation{
			Engine:    t.config.Engine,
			Operation: "chat tokenization",
		}
	}

	// Prepare request using adapter
	request, err := t.adapter.PrepareTokenizeRequest(input)
	if err != nil {
		return nil, ErrTokenizationFailed{
			Message: "failed to prepare request",
			Cause:   err,
		}
	}

	// Make HTTP request
	path := t.adapter.GetTokenizePath()
	respData, err := t.client.Post(ctx, path, request)
	if err != nil {
		return nil, ErrTokenizationFailed{
			Message: "request failed",
			Cause:   err,
		}
	}

	// Parse response using adapter
	result, err := t.adapter.ParseTokenizeResponse(respData)
	if err != nil {
		return nil, ErrTokenizationFailed{
			Message: "failed to parse response",
			Cause:   err,
		}
	}

	return result, nil
}

// Detokenize converts tokens back to text
func (t *remoteTokenizerImpl) Detokenize(ctx context.Context, tokens []int) (string, error) {
	if !t.adapter.SupportsDetokenization() {
		return "", ErrUnsupportedOperation{
			Engine:    t.config.Engine,
			Operation: "detokenization",
		}
	}

	// Prepare request using adapter
	request, err := t.adapter.PrepareDetokenizeRequest(tokens)
	if err != nil {
		return "", ErrDetokenizationFailed{
			Message: "failed to prepare request",
			Cause:   err,
		}
	}

	// Make HTTP request
	path := t.adapter.GetDetokenizePath()
	respData, err := t.client.Post(ctx, path, request)
	if err != nil {
		return "", ErrDetokenizationFailed{
			Message: "request failed",
			Cause:   err,
		}
	}

	// Parse response using adapter
	result, err := t.adapter.ParseDetokenizeResponse(respData)
	if err != nil {
		return "", ErrDetokenizationFailed{
			Message: "failed to parse response",
			Cause:   err,
		}
	}

	return result, nil
}

// GetEndpoint returns the endpoint URL of the remote tokenizer
func (t *remoteTokenizerImpl) GetEndpoint() string {
	return t.config.Endpoint
}

// IsHealthy checks if the remote tokenizer service is healthy
func (t *remoteTokenizerImpl) IsHealthy(ctx context.Context) bool {
	// Use empty string for minimal health check
	// With add_special_tokens=false, this returns {"tokens": [], "count": 0}
	// This approach minimizes processing overhead while still validating service responsiveness
	testInput := TokenizeInput{
		Type:             CompletionInput,
		Text:             "",
		AddSpecialTokens: false,
	}

	_, err := t.TokenizeWithOptions(ctx, testInput)
	return err == nil
}

// Close closes the HTTP client connections
func (t *remoteTokenizerImpl) Close() error {
	if t.client != nil {
		t.client.Close()
	}
	return nil
}

// validateRemoteConfig validates the remote tokenizer configuration
func validateRemoteConfig(c *RemoteTokenizerConfig) error {
	if c.Engine == "" {
		return ErrInvalidConfig{Message: "Engine cannot be empty"}
	}
	if c.Endpoint == "" {
		return ErrInvalidConfig{Message: "Endpoint cannot be empty"}
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultRemoteTimeout // Default to 30 seconds
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = defaultRemoteMaxRetries // Default to 3 retries
	}
	return nil
}

// Ensure remoteTokenizerImpl implements remoteTokenizer interface
var _ remoteTokenizer = (*remoteTokenizerImpl)(nil)
