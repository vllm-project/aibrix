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

package e2e

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/stretchr/testify/assert"
)

func TestBaseModelInference(t *testing.T) {
	initializeClient(context.Background(), t)

	client := createOpenAIClient(gatewayURL, apiKey)
	completion, err := client.Completions.New(context.TODO(), openai.CompletionNewParams{
		Prompt: openai.CompletionNewParamsPromptUnion{
			OfString: openai.String("Say this is a test"),
		},
		Model: modelName,
	})
	if err != nil {
		t.Fatalf("completions failed: %v", err)
	}
	assert.Equal(t, modelName, completion.Model)
	assert.NotEmpty(t, completion.Choices, "completion has no choices returned")
	assert.NotEmpty(t, completion.Choices[0].Text, "chat completion has no message returned")
	assert.Greater(t, completion.Usage.CompletionTokens, int64(0), "completion tokens are more than zero")

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test"),
		},
		Model: modelName,
	})
	if err != nil {
		t.Fatalf("chat completions failed: %v", err)
	}
	assert.Equal(t, modelName, chatCompletion.Model)
	assert.NotEmpty(t, chatCompletion.Choices, "chat completion has no choices returned")
	assert.NotNil(t, chatCompletion.Choices[0].Message.Content, "chat completion has no message returned")
}

func TestBaseModelInferenceFailures(t *testing.T) {
	testCases := []struct {
		name            string
		apiKey          string
		modelName       string
		routingStrategy string
		expectErrCode   int
	}{
		{
			name:          "Invalid API Key",
			apiKey:        "fake-api-key",
			modelName:     modelName,
			expectErrCode: 401,
		},
		{
			name:          "Invalid Model Name",
			apiKey:        apiKey,
			modelName:     "fake-model-name",
			expectErrCode: 400,
		},
		{
			name:            "Invalid Routing Strategy",
			apiKey:          apiKey,
			modelName:       modelName,
			routingStrategy: "invalid-routing-strategy",
			expectErrCode:   400,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var client openai.Client
			if tc.routingStrategy != "" {
				var dst *http.Response
				client = createOpenAIClientWithRoutingStrategy(gatewayURL, tc.apiKey,
					tc.routingStrategy, option.WithResponseInto(&dst))
			} else {
				client = createOpenAIClient(gatewayURL, tc.apiKey)
			}

			_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
				Messages: []openai.ChatCompletionMessageParamUnion{
					openai.UserMessage("Say this is a test"),
				},
				Model: tc.modelName,
			})

			assert.Error(t, err)
			var apiErr *openai.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("Error is not an APIError: %+v", err)
			} else {
				t.Logf("API Error code: %d, message: %s", apiErr.StatusCode, apiErr.Message)
			}
			if assert.ErrorAs(t, err, &apiErr) {
				assert.Equal(t, tc.expectErrCode, apiErr.StatusCode, t.Name())
			}
		})
	}
}
