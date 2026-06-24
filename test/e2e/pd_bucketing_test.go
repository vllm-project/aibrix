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
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	bucketShortStorm    = "mock-llama2-7b-pd-bucket-short"
	bucketMediumStorm   = "mock-llama2-7b-pd-bucket-medium"
	bucketCombinedStorm = "mock-llama2-pd-bkt-comb"
)

// promptWithTokenLength returns a string that tokenizes to exactly tokenCount tokens
// using the same cl100k_base tokenizer as the gateway PD router.
func promptWithTokenLength(t *testing.T, tokenCount int) string {
	t.Helper()
	s := ""
	for len(s) < 4096 {
		tokens, err := utils.TokenizeInputText(s)
		require.NoError(t, err)
		if len(tokens) == tokenCount {
			return s
		}
		s += "a"
	}
	t.Fatalf("failed to build prompt with %d tokens", tokenCount)
	return ""
}

func assertPDBucketingRoute(t *testing.T, prompt, stormName string, expectCombined bool) {
	t.Helper()
	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, "pd", option.WithResponseInto(&dst))

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
		Model: modelNameVLLMBucket,
	})
	require.NoError(t, err, "PD bucketing chat completion failed for storm %s", stormName)
	assert.Equal(t, modelNameVLLMBucket, chatCompletion.Model)

	decodePod := dst.Header.Get("target-pod")
	prefillPod := dst.Header.Get("prefill-target-pod")
	require.NotEmpty(t, decodePod, "target-pod header must be set")

	if expectCombined {
		assert.Empty(t, prefillPod, "combined routing should not set prefill-target-pod")
		assert.True(t, strings.Contains(decodePod, bucketCombinedStorm),
			"expected combined pod, got decode=%s", decodePod)
	} else {
		assert.NotEmpty(t, prefillPod, "prefill-target-pod header must be set for PD routing")
		assert.True(t, strings.Contains(prefillPod, stormName),
			"prefill pod %s should belong to storm %s", prefillPod, stormName)
		assert.True(t, strings.Contains(decodePod, stormName),
			"decode pod %s should belong to storm %s", decodePod, stormName)
		assert.NotEqual(t, prefillPod, decodePod)
	}

	t.Logf("storm=%s combined=%v — prefill: %s, decode: %s", stormName, expectCombined, prefillPod, decodePod)
}

// TestPDDisaggregationVLLMBucketDecodeDownFallbackToCombined verifies that when the
// medium-storm decode pod is deleted, a prompt of 20 tokens (which matches the 16–32
// bucket) falls back to the combined pod instead of erroring or cross-routing.
func TestPDDisaggregationVLLMBucketDecodeDownFallbackToCombined(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameVLLMBucket)

	mediumPrompt := promptWithTokenLength(t, 20)
	mediumTokens, err := utils.TokenizeInputText(mediumPrompt)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(mediumTokens), 16)
	require.LessOrEqual(t, len(mediumTokens), 32)

	ctx := context.Background()
	k8sClient, _ := initializeClient(ctx, t)

	initialPods, err := k8sClient.CoreV1().Pods("default").List(ctx, v1.ListOptions{})
	require.NoError(t, err)
	initialCount := len(initialPods.Items)

	decodePods, err := k8sClient.CoreV1().Pods("default").List(ctx, v1.ListOptions{
		LabelSelector: "role-name=decode,storm-service-name=" + bucketMediumStorm,
	})
	require.NoError(t, err)
	require.NotEmpty(t, decodePods.Items, "expected at least one decode pod in medium storm")

	podToDelete := decodePods.Items[0].Name
	t.Logf("deleting medium-storm decode pod: %s", podToDelete)
	require.NoError(t, k8sClient.CoreV1().Pods("default").Delete(ctx, podToDelete, v1.DeleteOptions{}))

	t.Cleanup(func() {
		validateAllPodsAreReady(t, k8sClient, initialCount)
		waitForPDDisaggregationRouting(t, modelNameVLLMBucket)
		t.Logf("medium-storm decode pod %s has been recreated", podToDelete)
	})

	// Poll until the gateway observes the deletion and routes the medium prompt to combined.
	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, "pd", option.WithResponseInto(&dst))

	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
				Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(mediumPrompt)},
				Model:    modelNameVLLMBucket,
			})
			if err != nil {
				t.Logf("waiting for fallback routing: %v", err)
				return false, nil
			}
			prefillPod := dst.Header.Get("prefill-target-pod")
			decodePod := dst.Header.Get("target-pod")
			if prefillPod != "" || !strings.Contains(decodePod, bucketCombinedStorm) {
				t.Logf("not yet routing to combined: prefill=%s decode=%s", prefillPod, decodePod)
				return false, nil
			}
			return true, nil
		})
	require.NoError(t, err, "medium prompt should fall back to combined pod when medium-storm decode is down")

	prefillPod := dst.Header.Get("prefill-target-pod")
	decodePod := dst.Header.Get("target-pod")
	assert.Empty(t, prefillPod, "combined routing should not set prefill-target-pod")
	assert.True(t, strings.Contains(decodePod, bucketCombinedStorm),
		"expected combined pod, got decode=%s", decodePod)
	t.Logf("fallback confirmed — decode: %s", decodePod)
}

// TestPDDisaggregationVLLMPromptLengthBucketing verifies that with
// AIBRIX_PROMPT_LENGTH_BUCKETING enabled, the gateway routes prompts to the
// correct StormService bucket: 0–15 and 16–32 use matching prefill/decode
// rolesets; prompts above 32 fall back to the combined role.
func TestPDDisaggregationVLLMPromptLengthBucketing(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameVLLMBucket)

	shortPrompt := promptWithTokenLength(t, 10)
	mediumPrompt := promptWithTokenLength(t, 20)
	longPrompt := promptWithTokenLength(t, 40)

	shortTokens, err := utils.TokenizeInputText(shortPrompt)
	require.NoError(t, err)
	require.LessOrEqual(t, len(shortTokens), 15)

	mediumTokens, err := utils.TokenizeInputText(mediumPrompt)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(mediumTokens), 16)
	require.LessOrEqual(t, len(mediumTokens), 32)

	longTokens, err := utils.TokenizeInputText(longPrompt)
	require.NoError(t, err)
	require.Greater(t, len(longTokens), 32)

	waitForPDCombinedRouting(t, modelNameVLLMBucket, bucketCombinedStorm, longPrompt)

	assertPDBucketingRoute(t, shortPrompt, bucketShortStorm, false)
	assertPDBucketingRoute(t, mediumPrompt, bucketMediumStorm, false)
	assertPDBucketingRoute(t, longPrompt, bucketCombinedStorm, true)
}
