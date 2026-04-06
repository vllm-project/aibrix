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
	"net/http"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPDDisaggregationVLLM verifies that the PD (prefill-decode disaggregation) routing
// strategy performs a prefill request before routing the decode request to a separate pod.
//
// Expected flow:
//  1. Gateway selects a prefill pod and a decode pod.
//  2. Gateway sends max_tokens=1, stream=false to the prefill pod; the mock app returns
//     kv_transfer_params in the response.
//  3. Gateway forwards the original request (with kv_transfer_params updated from the
//     prefill response) to the decode pod.
//  4. Response headers carry both "prefill-target-pod" and "target-pod".
func TestPDDisaggregationVLLM(t *testing.T) {
	k8s := newK8sClientForE2E(t)
	waitForMinRoutablePodsForModel(t, k8s, "default", modelNameVLLM, 2)

	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, "pd", option.WithResponseInto(&dst))

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test for vLLM PD disaggregation"),
		},
		Model: modelNameVLLM,
	})
	require.NoError(t, err, "vLLM PD chat completion request failed")

	// Validate the response payload.
	assert.Equal(t, modelNameVLLM, chatCompletion.Model)
	assert.NotEmpty(t, chatCompletion.Choices, "chat completion returned no choices")
	assert.NotEmpty(t, chatCompletion.Choices[0].Message.Content, "chat completion returned empty message")

	// The decode pod is the final routing target.
	decodePod := dst.Header.Get("target-pod")
	assert.NotEmpty(t, decodePod, "target-pod header must be set (decode pod)")

	// The prefill pod header is set by the gateway before it issues the prefill request.
	prefillPod := dst.Header.Get("prefill-target-pod")
	assert.NotEmpty(t, prefillPod, "prefill-target-pod header must be set")

	// Prefill and decode must be different pods.
	assert.NotEqual(t, prefillPod, decodePod,
		"prefill pod and decode pod should be different (got same pod: %s)", decodePod)

	t.Logf("vLLM — prefill-target-pod: %s, target-pod (decode): %s", prefillPod, decodePod)
}

// TestPDDisaggregationSGLang verifies PD routing for the SGLang engine.
//
// SGLang uses an async prefill: the gateway fires the prefill request in a
// background goroutine (bootstrap_host/port/room coordinates KV transfer) and
// immediately routes the decode request without waiting for the prefill response.
// From the test's perspective the observable contract is identical to vLLM —
// both prefill-target-pod and target-pod headers must be set and must differ.
func TestPDDisaggregationSGLang(t *testing.T) {
	k8s := newK8sClientForE2E(t)
	waitForMinRoutablePodsForModel(t, k8s, "default", modelNameSGLang, 2)

	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, "pd", option.WithResponseInto(&dst))

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test for SGLang PD disaggregation"),
		},
		Model: modelNameSGLang,
	})
	require.NoError(t, err, "SGLang PD chat completion request failed")

	assert.Equal(t, modelNameSGLang, chatCompletion.Model)
	assert.NotEmpty(t, chatCompletion.Choices, "chat completion returned no choices")
	assert.NotEmpty(t, chatCompletion.Choices[0].Message.Content, "chat completion returned empty message")

	decodePod := dst.Header.Get("target-pod")
	assert.NotEmpty(t, decodePod, "target-pod header must be set (decode pod)")

	prefillPod := dst.Header.Get("prefill-target-pod")
	assert.NotEmpty(t, prefillPod, "prefill-target-pod header must be set")

	assert.NotEqual(t, prefillPod, decodePod,
		"prefill pod and decode pod should be different (got same pod: %s)", decodePod)

	t.Logf("SGLang — prefill-target-pod: %s, target-pod (decode): %s", prefillPod, decodePod)
}

// TestPDDisaggregationTRTLLM verifies PD routing for the TensorRT-LLM engine.
//
// TRT-LLM uses a synchronous prefill: the gateway waits for the prefill response
// which carries disaggregated_params (first_gen_tokens, opaque_state) and
// prompt_token_ids, then forwards those to the decode pod for generation.
func TestPDDisaggregationTRTLLM(t *testing.T) {
	k8s := newK8sClientForE2E(t)
	waitForMinRoutablePodsForModel(t, k8s, "default", modelNameTRTLLM, 2)

	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, "pd", option.WithResponseInto(&dst))

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test for TensorRT-LLM PD disaggregation"),
		},
		Model: modelNameTRTLLM,
	})
	require.NoError(t, err, "TRT-LLM PD chat completion request failed")

	assert.Equal(t, modelNameTRTLLM, chatCompletion.Model)
	assert.NotEmpty(t, chatCompletion.Choices, "chat completion returned no choices")
	assert.NotEmpty(t, chatCompletion.Choices[0].Message.Content, "chat completion returned empty message")

	decodePod := dst.Header.Get("target-pod")
	assert.NotEmpty(t, decodePod, "target-pod header must be set (decode pod)")

	prefillPod := dst.Header.Get("prefill-target-pod")
	assert.NotEmpty(t, prefillPod, "prefill-target-pod header must be set")

	assert.NotEqual(t, prefillPod, decodePod,
		"prefill pod and decode pod should be different (got same pod: %s)", decodePod)

	t.Logf("TRT-LLM — prefill-target-pod: %s, target-pod (decode): %s", prefillPod, decodePod)
}

// TestPDDisaggregationVLLMMultipleRequests sends several requests to verify that the
// PD router consistently selects valid prefill/decode pod pairs across requests.
func TestPDDisaggregationVLLMMultipleRequests(t *testing.T) {
	k8s := newK8sClientForE2E(t)
	waitForMinRoutablePodsForModel(t, k8s, "default", modelNameVLLM, 2)

	const iterations = 5

	for i := 0; i < iterations; i++ {
		var dst *http.Response
		client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, "pd", option.WithResponseInto(&dst))

		_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage("vLLM PD disaggregation stress test message"),
			},
			Model: modelNameVLLM,
		})
		require.NoError(t, err, "vLLM PD chat completion request %d failed", i)

		decodePod := dst.Header.Get("target-pod")
		prefillPod := dst.Header.Get("prefill-target-pod")

		assert.NotEmpty(t, decodePod, "request %d: target-pod header must be set", i)
		assert.NotEmpty(t, prefillPod, "request %d: prefill-target-pod header must be set", i)
		assert.NotEqual(t, prefillPod, decodePod,
			"request %d: prefill and decode pods should differ", i)

		t.Logf("request %d — prefill: %s, decode: %s", i, prefillPod, decodePod)
	}
}
