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

// postTokenize sends a raw request to /tokenize. Neither the openai-go client nor
// framework.SendPDRequest fits: /tokenize is a vLLM endpoint served at the root
// rather than an OpenAI-compatible one under /v1, so the SDK has no method for it,
// and SendPDRequest hardcodes /v1/chat/completions and treats a non-2xx as an
// error, while the missing-model case below expects a 400.
func postTokenize(t *testing.T, body string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, gatewayURL+"/tokenize", bytes.NewBufferString(body))
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

// TestTokenize covers the request half of the fix: /tokenize has to be a case in
// validateRequestBody, otherwise the gateway answers 501 "unknown request path"
// before the request reaches an engine.
func TestTokenize(t *testing.T) {
	status, payload := postTokenize(t, `{"model": "`+modelName+`", "prompt": "Say this is a test"}`)

	require.Equal(t, http.StatusOK, status, "tokenize should be routed, got: %s", payload)

	var resp struct {
		Tokens      []int64 `json:"tokens"`
		Count       int64   `json:"count"`
		MaxModelLen int64   `json:"max_model_len"`
	}
	require.NoError(t, json.Unmarshal(payload, &resp))

	assert.NotEmpty(t, resp.Tokens, "tokens should not be empty")
	assert.Equal(t, int64(len(resp.Tokens)), resp.Count, "count should match the number of tokens")
	assert.Positive(t, resp.MaxModelLen, "max_model_len should be reported")
}

// TestTokenizeResponseIsForwardedVerbatim covers the response half, which is the
// half that is easy to regress: the tokenize response carries neither "model" nor
// "usage", so if /tokenize is ever dropped from nonLanguagePrefixes then
// processLanguageResponse rejects the engine's body as an unknown response and
// replaces it with an ImmediateResponse 500. A 200 alone does not catch that -
// the body has to be the engine's own.
func TestTokenizeResponseIsForwardedVerbatim(t *testing.T) {
	status, payload := postTokenize(t, `{"model": "`+modelName+`", "prompt": "Say this is a test"}`)

	require.Equal(t, http.StatusOK, status, "unexpected status, got: %s", payload)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(payload, &resp))

	assert.Contains(t, resp, "tokens")
	assert.Contains(t, resp, "count")
	assert.NotContains(t, resp, "model", "gateway must not add fields the engine did not send")
	assert.NotContains(t, resp, "usage", "tokenize does no generation, so there is no usage to report")
	assert.NotContains(t, resp, "error", "engine body must survive the response path untouched")
}

// TestTokenizeMissingModel checks the one field the gateway itself requires. It
// has to reject the body rather than forward it, because the model is what the
// router selects a pod with.
func TestTokenizeMissingModel(t *testing.T) {
	status, payload := postTokenize(t, `{"prompt": "Say this is a test"}`)

	assert.Equal(t, http.StatusBadRequest, status, "missing model should be rejected, got: %s", payload)
}
