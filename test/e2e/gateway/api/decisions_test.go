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

const decisionsQuestions = `[{"id": "pick", "type": "choice", "question": "Which replica?", "options": [{"name": "a"}, {"name": "b"}]}]`

// postDecisions sends a raw request to /v1/decisions. Like postPooling, neither the
// openai-go client nor framework.SendPDRequest fits: /v1/decisions is an SGLang
// endpoint with its own body shape, and the error cases below expect a 400.
func postDecisions(t *testing.T, body string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/decisions", bytes.NewBufferString(body))
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

// TestDecisions covers the request half: /v1/decisions has to be a case in
// validateRequestBody and on the Envoy route list, otherwise the gateway answers
// 501 "unknown request path" or never sends the request to the ext-proc.
func TestDecisions(t *testing.T) {
	status, payload := postDecisions(t, `{"model": "`+modelName+`", "input": "Route this request", "questions": `+decisionsQuestions+`}`)

	require.Equal(t, http.StatusOK, status, "decisions should be routed, got: %s", payload)

	var resp struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Answers map[string]struct {
			Type   string  `json:"type"`
			Choice *string `json:"choice"`
		} `json:"answers"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(payload, &resp))

	assert.Equal(t, "decisions", resp.Object)
	assert.Equal(t, modelName, resp.Model, "engine should echo the model")
	require.Contains(t, resp.Answers, "pick")
	assert.Equal(t, "choice", resp.Answers["pick"].Type)
	assert.NotNil(t, resp.Answers["pick"].Choice, "a choice question should carry a choice")
	assert.Positive(t, resp.Usage.PromptTokens, "decisions reports prompt usage")
	assert.Zero(t, resp.Usage.CompletionTokens, "decisions generates no tokens")
	assert.Equal(t, resp.Usage.PromptTokens, resp.Usage.TotalTokens, "nothing is generated, so total == prompt")
}

// TestDecisionsObjectInput checks that "input" may be an object or array, not only
// a string: the gateway serializes it to JSON text for routing and must not reject it.
func TestDecisionsObjectInput(t *testing.T) {
	for name, input := range map[string]string{
		"object": `{"load": [1, 2]}`,
		"array":  `["a", {"b": 2}]`,
	} {
		t.Run(name, func(t *testing.T) {
			status, payload := postDecisions(t, `{"model": "`+modelName+`", "input": `+input+`, "questions": `+decisionsQuestions+`}`)

			assert.Equal(t, http.StatusOK, status, "%s input should be routed, got: %s", name, payload)
		})
	}
}

// TestDecisionsResponseIsForwardedVerbatim covers the response half: the body has
// "model" and "usage", so /v1/decisions stays on the language (usage-metering)
// response path. The test pins that the engine's own fields survive it - a 200
// alone would not catch the body being replaced with an error.
func TestDecisionsResponseIsForwardedVerbatim(t *testing.T) {
	status, payload := postDecisions(t, `{"model": "`+modelName+`", "input": "Route this request", "questions": `+decisionsQuestions+`}`)

	require.Equal(t, http.StatusOK, status, "unexpected status, got: %s", payload)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(payload, &resp))

	assert.Contains(t, resp, "answers")
	assert.Contains(t, resp, "model")
	assert.Contains(t, resp, "usage")
	assert.NotContains(t, resp, "error", "engine body must survive the response path untouched")
}

// TestDecisionsMissingModel checks the one field the gateway requires although
// SGLang treats it as optional: the model is what the router selects a pod with.
func TestDecisionsMissingModel(t *testing.T) {
	status, payload := postDecisions(t, `{"input": "Route this request", "questions": `+decisionsQuestions+`}`)

	assert.Equal(t, http.StatusBadRequest, status, "missing model should be rejected, got: %s", payload)
}

// TestDecisionsMissingQuestions checks the gateway-side guard: a body the engine
// would reject for lacking questions fails at the edge instead of being forwarded.
func TestDecisionsMissingQuestions(t *testing.T) {
	status, payload := postDecisions(t, `{"model": "`+modelName+`", "input": "Route this request"}`)

	assert.Equal(t, http.StatusBadRequest, status, "missing questions should be rejected, got: %s", payload)
}
