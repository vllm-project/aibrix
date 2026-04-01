/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this except in compliance with the License.
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
	"net/http"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigProfileRoutingStrategy verifies that passing config-profile as a header
// causes the gateway plugin to select the correct routing-strategy from the model's
// config (model.aibrix.ai/config annotation).
//
// The config is defined in development/app/config/mock/config-profile-llama2-patch.yaml
// (mirrors config-profile.yaml structure):
//   - defaultProfile: "least-request"
//   - profiles: "least-request" (routingStrategy: least-request), "throughput" (routingStrategy: throughput)
//
// The gateway resolves config-profile header -> ResolveProfile -> deriveRoutingStrategyFromContext
// and sets routing-strategy in the response headers.
func TestConfigProfileRoutingStrategy(t *testing.T) {
	msg := "config-profile routing test message"

	t.Run("no_config_profile_uses_default", func(t *testing.T) {
		var dst *http.Response
		client := createOpenAIClientWithConfigProfile(gatewayURL, apiKey, "", option.WithResponseInto(&dst))

		_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(msg)},
			Model:    modelNameQwen3,
		})
		require.NoError(t, err)

		// No config-profile header -> defaultProfile "least-request" is used
		got := dst.Header.Get("routing-strategy")
		assert.Equal(t, "least-request", got,
			"without config-profile header, gateway should use defaultProfile least-request")
	})

	t.Run("config_profile_least_request", func(t *testing.T) {
		var dst *http.Response
		client := createOpenAIClientWithConfigProfile(gatewayURL, apiKey, "least-request", option.WithResponseInto(&dst))

		_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(msg)},
			Model:    modelNameQwen3,
		})
		require.NoError(t, err)

		got := dst.Header.Get("routing-strategy")
		assert.Equal(t, "least-request", got,
			"config-profile: least-request should select routing-strategy least-request")
	})

	t.Run("config_profile_throughput", func(t *testing.T) {
		var dst *http.Response
		client := createOpenAIClientWithConfigProfile(gatewayURL, apiKey, "throughput", option.WithResponseInto(&dst))

		_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(msg)},
			Model:    modelNameQwen3,
		})
		require.NoError(t, err)

		got := dst.Header.Get("routing-strategy")
		assert.Equal(t, "throughput", got,
			"config-profile: throughput should select routing-strategy throughput")
	})
}
