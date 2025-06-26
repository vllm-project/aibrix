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
	histogram := make(map[string]int)
	iterration := 100

	for i := 0; i < iterration; i++ {
		req := "hello test"
		targetPod := getTargetPodFromChatCompletion(t, req, "random")
		assert.NotEmpty(t, targetPod, "target pod should not be empty")
		histogram[targetPod]++
	}

	assert.True(t, len(histogram) > 1, "target pod distribution should be more than 1")

	// Collective the occurrence of each pod
	occurrence := make([]float64, 0, len(histogram))
	for _, count := range histogram {
		occurrence = append(occurrence, float64(count))
	}

	// Perform the Chi-Squared test
	chi2Stat, df, err := chiSquaredGoodnessOfFit(occurrence, float64(iterration/len(occurrence)))
	assert.NoError(t, err, "chi-squared test failed %v", err)
	assert.Equal(t, 2, df, "degrees of freedom should be 2")

	// Using a lower 1% significance level to make sure the null hypothesis is not rejected incorrectly
	significanceLevel := 0.01

	// We need to find the critical value for customed degrees of freedom (df)
	// and significance level (alpha) from a Chi-Squared distribution table
	// or a statistical calculator.
	// For df = 2 ,
	// common critical values are:
	// Alpha = 0.10, Critical Value ≈ 4.605
	// Alpha = 0.05, Critical Value ≈ 5.991
	// Alpha = 0.01, Critical Value ≈ 9.210
	assert.True(t, chi2Stat < 9.210,
		`The observed frequencies (chiSquare: %.3f) are significantly different from the expected 
		frequencies at the %.2f significance level. Suggesting the selection process is likely NOT random according 
		to the expected distribution.`,
		chi2Stat, significanceLevel)
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
