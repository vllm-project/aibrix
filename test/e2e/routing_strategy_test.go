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
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		passed := runRandomRoutingCheck(t, attempt)
		if passed {
			return
		}
		if attempt < maxAttempts {
			t.Logf("Attempt %d/%d failed, retrying...", attempt, maxAttempts)
		}
	}
	t.Fatalf("TestRandomRouting failed after %d attempts", maxAttempts)
}

func runRandomRoutingCheck(t *testing.T, attempt int) bool {
	histogram := make(map[string]int)
	iterration := 100

	for i := 0; i < iterration; i++ {
		req := "hello test"
		targetPod := getTargetPodFromChatCompletion(t, req, "random")
		if targetPod == "" {
			t.Logf("attempt %d: target pod should not be empty at request %d", attempt, i)
			return false
		}
		histogram[targetPod]++
	}

	if len(histogram) <= 1 {
		t.Logf("attempt %d: target pod distribution should be more than 1", attempt)
		return false
	}

	// Collective the occurrence of each pod
	occurrence := make([]float64, 0, len(histogram))
	for _, count := range histogram {
		occurrence = append(occurrence, float64(count))
	}

	// Perform the Chi-Squared test
	chi2Stat, df, err := chiSquaredGoodnessOfFit(occurrence, float64(iterration/len(occurrence)))
	if err != nil {
		t.Logf("attempt %d: chi-squared test failed: %v", attempt, err)
		return false
	}
	if df != 2 {
		t.Logf("attempt %d: degrees of freedom should be 2, got %d", attempt, df)
		return false
	}

	// Using a lower 1% significance level to make sure the null hypothesis is not rejected incorrectly
	// For df = 2, critical value at alpha = 0.01 is 9.210
	if chi2Stat >= 9.210 {
		t.Logf("attempt %d: chiSquare %.3f >= 9.210, distribution not random enough at 0.01 significance level", attempt, chi2Stat)
		return false
	}

	return true
}

// nolint:lll
func TestPrefixCacheRouting(t *testing.T) {
	// #1 request - cache first time request
	req := "prefix-cache routing algorithm test message, ensure test message is longer than 128 bytes!! this is first message! 这是测试消息！"
	targetPod := getTargetPodFromChatCompletion(t, req, "prefix-cache")
	t.Logf("req: %s, target pod: %v\n", req, targetPod)

	// #2 request - reuse target pod from first time
	targetPod2 := getTargetPodFromChatCompletion(t, req, "prefix-cache")
	t.Logf("req: %s, target pod: %v\n", req, targetPod2)
	assert.Equal(t, targetPod, targetPod2)

	// #3 request - new request, match to random pod
	var count int
	for count < 5 {
		generateMessage := fmt.Sprintf("prefix-cache routing algorithm test message, ensure test message is longer than 128 bytes!! this is %v message! 这是测试消息！", rand.Intn(1000))
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
		}

		assert.Equal(t, targetPod, dst.Header.Get("target-pod"), "each multiturn conversation must route to same target pod")
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
