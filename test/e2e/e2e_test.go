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
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/stretchr/testify/assert"
)

const (
	baseURL   = "http://localhost:8888"
	apiKey    = "test-key-1234567890"
	modelName = "llama2-7b"
	namespace = "aibrix-system"
)

func TestBaseModelInference(t *testing.T) {
	initializeClient(context.Background(), t)

	client := createOpenAIClient(baseURL, apiKey)
	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: openai.F([]openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test"),
		}),
		Model: openai.F(modelName),
	})
	if err != nil {
		t.Error("chat completions failed", err)
	}
	assert.Equal(t, modelName, chatCompletion.Model)
}

func TestBaseModelInferenceFailures(t *testing.T) {
	// error on invalid api key
	client := createOpenAIClient(baseURL, "fake-api-key")
	_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: openai.F([]openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test"),
		}),
		Model: openai.F(modelName),
	})
	if err == nil {
		t.Error("500 Internal Server Error expected for invalid api-key")
	}

	// error on invalid model name
	client = createOpenAIClient(baseURL, apiKey)
	_, err = client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: openai.F([]openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test"),
		}),
		Model: openai.F("fake-model-name"),
	})
	assert.Contains(t, err.Error(), "400 Bad Request")
	if err == nil {
		t.Error("400 Bad Request expected for invalid api-key")
	}

	// invalid routing strategy
	client = openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
		option.WithHeader("routing-strategy", "invalid-routing-strategy"),
	)
	client.Options = append(client.Options, option.WithHeader("routing-strategy", "invalid-routing-strategy"))
	_, err = client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: openai.F([]openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test"),
		}),
		Model: openai.F(modelName),
	})
	if err == nil {
		t.Error("400 Bad Request expected for invalid routing-strategy")
	}
	assert.Contains(t, err.Error(), "400 Bad Request")
}
