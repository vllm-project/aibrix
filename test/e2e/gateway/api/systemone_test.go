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

const systemOneQuestions = `{"team": {"type": "choice", "instructions": "Which team?", "criteria": {"billing": null, "support": "Bugs or integration problems"}}, "urgent": {"type": "noul", "instructions": "The customer needs an answer today."}}`

// postSystemOne sends a raw request to /v1/systemone. Like postDecisions, neither the
// openai-go client nor framework.SendPDRequest fits: /v1/systemone is an SGLang endpoint
// with its own body shape, and the error cases below expect a 400.
func postSystemOne(t *testing.T, body string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/systemone", bytes.NewBufferString(body))
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

// TestSystemOne covers the request half: /v1/systemone has to be a case in
// validateRequestBody and on the Envoy route list, otherwise the gateway answers
// 501 "unknown request path" or never sends the request to the ext-proc.
func TestSystemOne(t *testing.T) {
	status, payload := postSystemOne(t, `{"model": "`+modelName+`", "state": "Stripe keeps failing", "questions": `+systemOneQuestions+`}`)

	require.Equal(t, http.StatusOK, status, "systemone should be routed, got: %s", payload)

	var resp struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type   string   `json:"type"`
			Choice *string  `json:"choice"`
			Noul   *float64 `json:"noul"`
		} `json:"answers"`
		Usage struct {
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
			TotalTokens  *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(payload, &resp))

	// The engine reports the model that answered, which need not be the requested name.
	assert.NotEmpty(t, resp.Model)
	require.Contains(t, resp.Answers, "team")
	assert.Equal(t, "choice", resp.Answers["team"].Type)
	assert.NotNil(t, resp.Answers["team"].Choice, "a choice question should carry a choice")
	require.Contains(t, resp.Answers, "urgent")
	assert.Equal(t, "noul", resp.Answers["urgent"].Type)
	assert.NotNil(t, resp.Answers["urgent"].Noul, "a noul question should carry a probability")
	assert.Positive(t, resp.Usage.InputTokens, "systemone reports input usage")
	assert.Zero(t, resp.Usage.OutputTokens, "systemone generates no tokens")
	// The gateway derives a total for its own metering only; it must not add one to the body.
	assert.Nil(t, resp.Usage.TotalTokens, "the usage block should reach the client as the engine sent it")
}

// TestSystemOneStructuredState checks that "state" may be an object or array, not only
// a string: the gateway serializes it to JSON text for routing and must not reject it.
func TestSystemOneStructuredState(t *testing.T) {
	for name, state := range map[string]string{
		"object": `{"load": [1, 2]}`,
		"array":  `["a", {"b": 2}]`,
		// SGLang accepts an empty state on this route, so the gateway must not refuse it.
		"empty string": `""`,
	} {
		t.Run(name, func(t *testing.T) {
			status, payload := postSystemOne(t, `{"model": "`+modelName+`", "state": `+state+`, "questions": `+systemOneQuestions+`}`)

			assert.Equal(t, http.StatusOK, status, "%s state should be routed, got: %s", name, payload)
		})
	}
}

// TestSystemOneResponseIsForwardedVerbatim covers the response half: the body has
// "model" and "usage", so /v1/systemone stays on the language (usage-metering)
// response path. The test pins that the engine's own fields survive it - a 200
// alone would not catch the body being replaced with an error.
func TestSystemOneResponseIsForwardedVerbatim(t *testing.T) {
	status, payload := postSystemOne(t, `{"model": "`+modelName+`", "state": "Stripe keeps failing", "questions": `+systemOneQuestions+`}`)

	require.Equal(t, http.StatusOK, status, "unexpected status, got: %s", payload)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(payload, &resp))

	assert.Contains(t, resp, "answers")
	assert.Contains(t, resp, "model")
	assert.Contains(t, resp, "usage")
	assert.NotContains(t, resp, "error", "engine body must survive the response path untouched")
}

// TestSystemOneMissingModel checks the one field the gateway requires although
// SGLang accepts any model name: the model is what the router selects a pod with.
func TestSystemOneMissingModel(t *testing.T) {
	status, payload := postSystemOne(t, `{"state": "Stripe keeps failing", "questions": `+systemOneQuestions+`}`)

	assert.Equal(t, http.StatusBadRequest, status, "missing model should be rejected, got: %s", payload)
}

// TestSystemOneMissingState checks the gateway-side guard: a body the engine would
// reject for lacking a state fails at the edge instead of being forwarded.
func TestSystemOneMissingState(t *testing.T) {
	status, payload := postSystemOne(t, `{"model": "`+modelName+`", "questions": `+systemOneQuestions+`}`)

	assert.Equal(t, http.StatusBadRequest, status, "missing state should be rejected, got: %s", payload)
}

// TestSystemOneBadQuestions checks that questions must be a non-empty map keyed by id.
// An array is the /v1/decisions shape, which a client may send here by mistake.
func TestSystemOneBadQuestions(t *testing.T) {
	for name, questions := range map[string]string{
		"missing": ``,
		"empty":   `, "questions": {}`,
		"array":   `, "questions": [{"id": "team", "type": "yes_no", "question": "Is it?"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			status, payload := postSystemOne(t, `{"model": "`+modelName+`", "state": "Stripe keeps failing"`+questions+`}`)

			assert.Equal(t, http.StatusBadRequest, status, "%s questions should be rejected, got: %s", name, payload)
		})
	}
}
