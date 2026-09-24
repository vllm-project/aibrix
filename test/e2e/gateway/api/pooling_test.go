/*
Copyright 2026 The Aibrix Team.

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

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postPooling sends a raw request to /pooling. Like postTokenize, neither the
// openai-go client nor framework.SendPDRequest fits: /pooling is a vLLM endpoint
// served at the root rather than an OpenAI-compatible one under /v1, and
// SendPDRequest hardcodes /v1/chat/completions and treats a non-2xx as an error,
// while the missing-model case below expects a 400.
func postPooling(t *testing.T, body string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, gatewayURL+"/pooling", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, payload
}

// TestPooling covers the request half of the fix: /pooling has to be a case in
// validateRequestBody, otherwise the gateway answers 501 "unknown request path"
// before the request reaches an engine.
func TestPooling(t *testing.T) {
	status, payload := postPooling(t, `{"model": "`+modelName+`", "input": "Say this is a test"}`)

	require.Equal(t, http.StatusOK, status, "pooling should be routed, got: %s", payload)

	var resp struct {
		Data []struct {
			Index  int64     `json:"index"`
			Object string    `json:"object"`
			Data   []float64 `json:"data"`
		} `json:"data"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(payload, &resp))

	assert.NotEmpty(t, resp.Data, "data should not be empty")
	assert.Equal(t, modelName, resp.Model, "engine should echo the model")
	assert.Positive(t, resp.Usage.PromptTokens, "pooling reports prompt usage")
	assert.Equal(t, resp.Usage.PromptTokens, resp.Usage.TotalTokens, "pooling generates no tokens, so total == prompt")
}

// TestPoolingResponseIsForwardedVerbatim covers the response half. Unlike
// /tokenize, the pooling response carries "model" and "usage", so /pooling stays
// on the language (usage-metering) response path: processLanguageResponse parses
// the usage and re-serializes the body. The test pins that the engine's own
// fields survive that path - a 200 alone would not catch the body being replaced
// with an ImmediateResponse error.
func TestPoolingResponseIsForwardedVerbatim(t *testing.T) {
	status, payload := postPooling(t, `{"model": "`+modelName+`", "input": "Say this is a test"}`)

	require.Equal(t, http.StatusOK, status, "unexpected status, got: %s", payload)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(payload, &resp))

	assert.Contains(t, resp, "data")
	assert.Contains(t, resp, "model")
	assert.Contains(t, resp, "usage")
	assert.NotContains(t, resp, "error", "engine body must survive the response path untouched")
}

// TestPoolingChatForm covers vLLM's chat pooling form: PoolingChatRequest carries
// "messages" instead of "input". The gateway routes on "model" either way, and the
// routing message comes from the messages, like tokenize's chat form.
func TestPoolingChatForm(t *testing.T) {
	status, payload := postPooling(t, `{"model": "`+modelName+`", "messages": [{"role": "user", "content": "Say this is a test"}]}`)

	require.Equal(t, http.StatusOK, status, "chat-form pooling should be routed, got: %s", payload)

	var resp struct {
		Data  []struct{} `json:"data"`
		Model string     `json:"model"`
	}
	require.NoError(t, json.Unmarshal(payload, &resp))
	assert.NotEmpty(t, resp.Data, "data should not be empty")
	assert.Equal(t, modelName, resp.Model, "engine should echo the model")
}

// TestPoolingMissingModel checks the one field the gateway itself requires. It
// has to reject the body rather than forward it, because the model is what the
// router selects a pod with.
func TestPoolingMissingModel(t *testing.T) {
	status, payload := postPooling(t, `{"input": "Say this is a test"}`)

	assert.Equal(t, http.StatusBadRequest, status, "missing model should be rejected, got: %s", payload)
}

// TestPoolingStreamRejected checks the gateway-side stream guard: pooling never
// streams, and the gateway rejects stream=true up front with a field-specific
// error instead of forwarding it to fail in the engine.
func TestPoolingStreamRejected(t *testing.T) {
	status, payload := postPooling(t, `{"model": "`+modelName+`", "input": "Say this is a test", "stream": true}`)

	assert.Equal(t, http.StatusBadRequest, status, "stream=true should be rejected, got: %s", payload)
}
