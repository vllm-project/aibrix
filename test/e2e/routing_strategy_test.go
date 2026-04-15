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
	"fmt"
	"math/rand"
	"net/http"
	"testing"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStrategyRequiresCache(t *testing.T) {
	req := "this is test message"
	targetPod := getTargetPodFromChatCompletion(t, req, "least-request")
	assert.NotEmpty(t, targetPod, "least request target pod is empty")
}

func TestRandomRouting(t *testing.T) {
	// Retry up to 3 times to tolerate statistical flakiness in the chi-squared test.
	// Even with a correct random router, the test has a ~1% false-negative rate per run.
	maxAttempts := 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if lastErr = runRandomRoutingCheck(); lastErr == nil {
			return
		}
		t.Logf("Attempt %d/%d failed: %v", attempt, maxAttempts, lastErr)
	}
	t.Fatalf("TestRandomRouting failed after %d attempts: %v", maxAttempts, lastErr)
}

// chiSquaredCriticalValues contains critical values at 0.01 significance level
// for varying degrees of freedom, used to dynamically validate chi-squared results
// regardless of the number of pods in the environment.
var chiSquaredCriticalValues = map[int]float64{
	1: 6.635, 2: 9.210, 3: 11.345, 4: 13.277, 5: 15.086,
}

func runRandomRoutingCheck() error {
	histogram := make(map[string]int)
	iteration := 100

	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, "random", option.WithResponseInto(&dst))

	for i := 0; i < iteration; i++ {
		_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage("hello test"),
			},
			Model: modelName,
		})
		if err != nil {
			return fmt.Errorf("chat completion request %d failed: %w", i, err)
		}
		targetPod := dst.Header.Get("target-pod")
		if targetPod == "" {
			return fmt.Errorf("request %d: target pod should not be empty", i)
		}
		histogram[targetPod]++
	}

	if len(histogram) <= 1 {
		return fmt.Errorf("target pod distribution should be more than 1, got %d", len(histogram))
	}

	// Collect the occurrence of each pod
	occurrence := make([]float64, 0, len(histogram))
	for _, count := range histogram {
		occurrence = append(occurrence, float64(count))
	}

	// Perform the Chi-Squared test using floating-point division for accurate expected frequency
	chi2Stat, df, err := chiSquaredGoodnessOfFit(occurrence, float64(iteration)/float64(len(occurrence)))
	if err != nil {
		return fmt.Errorf("chi-squared test failed: %w", err)
	}

	// Validate degrees of freedom matches observed pod count
	expectedDf := len(occurrence) - 1
	if df != expectedDf {
		return fmt.Errorf("degrees of freedom should be %d, got %d", expectedDf, df)
	}

	// Using a lower 1% significance level to make sure the null hypothesis is not rejected incorrectly
	criticalValue, ok := chiSquaredCriticalValues[df]
	if !ok {
		return fmt.Errorf("no chi-squared critical value configured for df=%d, "+
			"pod count %d is unexpected", df, len(histogram))
	}

	if chi2Stat >= criticalValue {
		return fmt.Errorf(
			"the observed frequencies (chiSquare: %.3f, df: %d) are significantly different from the expected "+
				"frequencies at the 0.01 significance level (critical value: %.3f), suggesting the selection "+
				"process is likely NOT random", chi2Stat, df, criticalValue)
	}

	return nil
}

// nolint:lll
func TestPrefixCacheRouting(t *testing.T) {
	// #1 request - cache first time request
	req := "prefix-cache routing algorithm test message, ensure test message is longer than 128 bytes!! this is first message! 这是测试消息！"
	targetPod := getTargetPodFromChatCompletion(t, req, "prefix-cache")
	t.Logf("req: %s, target pod: %v\n", req, targetPod)

	time.Sleep(2 * time.Second)

	// #2 request - reuse target pod from first time
	targetPod2 := getTargetPodFromChatCompletion(t, req, "prefix-cache")
	t.Logf("req: %s, target pod: %v\n", req, targetPod2)
	assert.Equal(t, targetPod, targetPod2)

	// #3 request - new request with a completely different prefix, should route to a different pod
	var count int
	for count < 5 {
		generateMessage := fmt.Sprintf("%d: completely different request prefix to avoid cache hits between "+
			"iterations, and padding to exceed 128 bytes for prefix cache routing test!!", rand.Intn(1000))
		targetPod3 := getTargetPodFromChatCompletion(t, generateMessage, "prefix-cache")
		t.Logf("req: %s, target pod from #3 request: %v\n", generateMessage, targetPod3)
		if targetPod != targetPod3 {
			break
		}
		count++
	}

	assert.NotEqual(t, 5, count)
}

// nolint:lll
func TestMultiTurnConversation(t *testing.T) {
	var dst *http.Response
	var targetPod string
	messages := []openai.ChatCompletionMessageParamUnion{}
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, "prefix-cache", option.WithResponseInto(&dst))

	for i := 1; i <= 5; i++ {
		input := fmt.Sprintf("Ensure test message is longer than 128 bytes!! This is test %d for multiturn conversation!! 这是多轮对话测试!! Have a good day!!", i)
		messages = append(messages, openai.UserMessage(input))

		chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
			Messages: messages,
			Model:    modelName,
		})
		require.NoError(t, err, "chat completitions failed %v", err)
		assert.Greater(t, chatCompletion.Usage.CompletionTokens, int64(0), "chat completions usage tokens greater than 0")
		assert.NotEmpty(t, chatCompletion.Choices[0].Message.Content)

		messages = append(messages, openai.AssistantMessage(chatCompletion.Choices[0].Message.Content))
		if i == 1 {
			targetPod = dst.Header.Get("target-pod")
			time.Sleep(2 * time.Second)
		}

		assert.Equal(t, targetPod, dst.Header.Get("target-pod"), "each multiturn conversation must route to same target pod")
	}
}

// nolint:lll
// TestPrefixCacheRoutingConsistency sends a warm-up request, waits 2 seconds,
// then sends 10 requests with the same prompt and asserts they all route to the
// same backend pod (prefix cache hit).
func TestPrefixCacheRoutingConsistency(t *testing.T) {
	// Message must exceed the prefix cache block threshold (>128 bytes) so that
	// at least one full block is hashed and stored in the prefix cache.
	const msg = "prefix-cache consistency test: this message is intentionally long to exceed the " +
		"128-byte block threshold required for prefix cache routing to engage. 这是前缀缓存路由一致性测试消息！"

	// Warm up: populate the prefix cache for this prompt on the target pod.
	warmPod := getTargetPodFromChatCompletion(t, msg, "prefix-cache")
	require.NotEmpty(t, warmPod, "warm-up request returned no target-pod header")
	t.Logf("warm-up routed to: %s", warmPod)

	time.Sleep(2 * time.Second)

	// All 10 subsequent requests with the same prefix must route to the same pod.
	for i := 0; i < 10; i++ {
		pod := getTargetPodFromChatCompletion(t, msg, "prefix-cache")
		assert.Equal(t, warmPod, pod, "request %d: expected pod %s, got %s", i+1, warmPod, pod)
		t.Logf("request %d routed to: %s", i+1, pod)
	}
}

func getTargetPodFromChatCompletion(t *testing.T, message string, strategy string) string {
	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, strategy, option.WithResponseInto(&dst))

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(message),
		},
		Model: modelName,
	})
	require.NoError(t, err, "chat completitions failed %v", err)
	assert.Equal(t, modelName, chatCompletion.Model)

	return dst.Header.Get("target-pod")
}

// ChiSquaredGoodnessOfFit calculates the chi-squared test statistic and degrees of freedom
// for a goodness-of-fit test.
// observed: A slice of observed frequencies for each category.
// expected: A slice of expected frequencies for each category.
// Returns the calculated chi-squared statistic and degrees of freedom.
// Returns an error if the input slices are invalid (e.g., different lengths, negative values).
func chiSquaredGoodnessOfFit(observed []float64, expected float64) (chi2Stat float64, degreesOfFreedom int, err error) {
	// Validate inputs
	if len(observed) == 0 {
		return 0, 0, fmt.Errorf("input slices cannot be empty")
	}

	// Calculate the chi-squared statistic
	chi2Stat = 0.0
	for i := 0; i < len(observed); i++ {
		if expected < 0 || observed[i] < 0 {
			return 0, 0, fmt.Errorf("frequencies cannot be negative")
		}
		if expected == 0 {
			// If expected frequency is 0, the term is typically skipped,
			// but this can indicate issues with the model or data.
			// For a strict goodness-of-fit, expected frequencies should ideally be > 0.
			// We'll return an error here as it often suggests a problem.
			return 0, 0, fmt.Errorf("expected frequency for category %d is zero, which is not allowed for this test", i)
		}
		diff := observed[i] - expected
		chi2Stat += (diff * diff) / expected
	}

	// Calculate degrees of freedom
	// For a goodness-of-fit test comparing observed frequencies to expected
	// frequencies from a theoretical distribution, the degrees of freedom
	// are typically the number of categories minus 1.
	degreesOfFreedom = len(observed) - 1

	return chi2Stat, degreesOfFreedom, nil
}
