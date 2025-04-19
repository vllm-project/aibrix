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
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/vtc"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// Test users for VTC routing tests
var testUsers = []utils.User{
	{Name: "user1", Rpm: 1000, Tpm: 1000},
	{Name: "user2", Rpm: 1000, Tpm: 1000},
	{Name: "user3", Rpm: 1000, Tpm: 1000},
}

// Global Redis client for tests
var redisClient *redis.Client

// Global variables for test state
var (
	availablePods  []string
	tokenTracker   vtc.TokenTracker
	tokenEstimator vtc.TokenEstimator
)

func setupVTCUsers(t *testing.T) {
	if redisClient == nil {
		redisClient = utils.GetRedisClient()
		if redisClient == nil {
			t.Fatal("Failed to connect to Redis")
		}
	}

	config := &vtc.VTCConfig{
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.0,
	}

	tokenTracker = vtc.NewInMemorySlidingWindowTokenTracker(
		config,
		vtc.WithWindowSize(100),
		vtc.WithTimeUnit(vtc.Milliseconds),
	)

	tokenEstimator = vtc.NewSimpleTokenEstimator()

	getAvailablePods(t)

	ctx := context.Background()
	for _, user := range testUsers {
		err := utils.SetUser(ctx, user, redisClient)
		if err != nil {
			t.Fatalf("Failed to create test user %s: %v", user.Name, err)
		}
		t.Logf("Created test user: %s", user.Name)
	}

	t.Cleanup(func() {
		cleanupVTCUsers(t)
	})
}

func cleanupVTCUsers(t *testing.T) {
	if redisClient == nil {
		return
	}

	ctx := context.Background()
	for _, user := range testUsers {
		err := utils.DelUser(ctx, user, redisClient)
		if err != nil {
			t.Logf("Warning: Failed to delete test user %s: %v", user.Name, err)
		} else {
			t.Logf("Deleted test user: %s", user.Name)
		}
	}
}

// simple hack to discover pods in e2e test env
func getAvailablePods(t *testing.T) {
	availablePods = []string{}

	allPodsMap := make(map[string]bool)
	for i := 0; i < 30; i++ { // Make multiple requests to discover all pods
		pod := getTargetPodFromChatCompletion(t, fmt.Sprintf("Pod discovery request %d", i), "random")
		if pod != "" {
			allPodsMap[pod] = true
		}
	}

	for pod := range allPodsMap {
		availablePods = append(availablePods, pod)
	}

	for i, pod := range availablePods {
		t.Logf("[DEBUG] Pod %d: %s", i, pod)
	}

	t.Logf("Discovered %d pods using random routing", len(availablePods))
}

func TestVTCBasicRouting(t *testing.T) {
	setupVTCUsers(t)

	req := "this is a test message for VTC Basic routing"
	targetPod := getTargetPodFromChatCompletionWithUser(t, req, "vtc-basic", "user1")
	assert.NotEmpty(t, targetPod, "vtc-basic target pod is empty")
}

func TestVTCFallbackToRandom(t *testing.T) {
	setupVTCUsers(t)

	req := "this is a test message for VTC Basic routing"
	targetPod := getTargetPodFromChatCompletionWithUser(t, req, "vtc-basic", "")
	assert.NotEmpty(t, targetPod, "vtc-basic target pod is empty") //what is this
}

func TestVTCSlidingWindow(t *testing.T) {
	setupVTCUsers(t)

	user := "user1"
	ctx := context.Background()

	// Verify initial token count is zero
	initialTokens, err := tokenTracker.GetTokenCount(ctx, user)
	assert.NoError(t, err)
	assert.Equal(t, float64(0), initialTokens, "Initial token count should be zero")

	// Add tokens
	err = tokenTracker.UpdateTokenCount(ctx, user, 10, 20) // 10 input tokens, 20 output tokens
	assert.NoError(t, err)

	// Verify tokens were added
	tokensAfterUpdate, err := tokenTracker.GetTokenCount(ctx, user)
	assert.NoError(t, err)
	assert.Equal(t, float64(30), tokensAfterUpdate, "Token count should be 30 after update")

	// Wait for the sliding window to expire (100ms + a little buffer)
	t.Log("Waiting for token window to expire (100ms)...")
	time.Sleep(150 * time.Millisecond)

	// Verify tokens were expired
	tokensAfterExpiry, err := tokenTracker.GetTokenCount(ctx, user)
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokensAfterExpiry, "Token count should be 0 after window expiry")

	// Verify that new tokens can be added after expiry
	err = tokenTracker.UpdateTokenCount(ctx, user, 5, 10) // 5 input tokens, 10 output tokens
	assert.NoError(t, err)

	tokensAfterSecondUpdate, err := tokenTracker.GetTokenCount(ctx, user)
	assert.NoError(t, err)
	assert.Equal(t, float64(15), tokensAfterSecondUpdate, "Token count should be 15 after second update")
}

// TestVTCHybridScoring tests the hybrid scoring approach that balances fairness and utilization
func TestVTCHybridScoring(t *testing.T) {
	setupVTCUsers(t)

	users := []string{"user1", "user2", "user3"}
	shortMsg := "Short message."
	mediumMsg := "This is a medium length message with more tokens."
	longMsg := "This is a very long message with many tokens. " +
		"It should be significantly longer than the others to ensure higher token count. " +
		"We want to make sure the VTC algorithm properly accounts for different token usages."

	if len(availablePods) <= 1 {
		t.Logf("[WARNING] Only %d pod(s) detected. VTC routing tests require multiple pods.", len(availablePods))
	}
	t.Logf("[Environment] Using %d pods: %s", len(availablePods), strings.Join(availablePods, ", "))

	// Sub-test 1: Token Accumulation - Verify that users accumulate tokens based on message size
	t.Run("TokenAccumulation", func(t *testing.T) {
		// Track initial token counts
		initialTokens := make(map[string]float64)
		for _, user := range users {
			tokenCount, _ := tokenTracker.GetTokenCount(context.Background(), user)
			initialTokens[user] = tokenCount
			t.Logf("Initial token count for user %s: %.2f", user, tokenCount)
		}

		msgMap := map[string]string{
			users[0]: shortMsg,  // user1 - short messages
			users[1]: mediumMsg, // user2 - medium messages
			users[2]: longMsg,   // user3 - long messages
		}

		podHistogram := make(map[string]int)

		for range 5 {
			for _, user := range users {
				pod := getTargetPodFromChatCompletionWithUser(t, msgMap[user], "vtc-basic", user)
				podHistogram[pod]++
			}
		}

		finalTokens := make(map[string]float64)
		for _, user := range users {
			tokenCount, _ := tokenTracker.GetTokenCount(context.Background(), user)
			finalTokens[user] = tokenCount
			t.Logf("Final token count for user %s: %.2f (delta: +%.2f)",
				user, tokenCount, tokenCount-initialTokens[user])
		}

		assert.Greater(t, finalTokens[users[1]], finalTokens[users[0]],
			"Expected medium messages to accumulate more tokens than short messages")
		assert.Greater(t, finalTokens[users[2]], finalTokens[users[1]],
			"Expected long messages to accumulate more tokens than medium messages")

		calculateDistributionStats(t, "Token Accumulation", podHistogram)
	})

	// Sub-test 2: Fairness Component - Verify that the algorithm considers token counts
	t.Run("FairnessComponent", func(t *testing.T) {
		userTokens := make(map[string]float64)
		for _, user := range users {
			tokenCount, _ := tokenTracker.GetTokenCount(context.Background(), user)
			userTokens[user] = tokenCount
			t.Logf("Token count before fairness test for user %s: %.2f", user, tokenCount)
		}

		testMsg := "Test message for fairness component."

		podAssignments := make(map[string]string)
		for _, user := range users {
			pod := getTargetPodFromChatCompletionWithUser(t, testMsg, "vtc-basic", user)
			podAssignments[user] = pod
			t.Logf("User %s (tokens: %.2f) routed to pod %s", user, userTokens[user], pod)
		}

		if len(availablePods) > 1 {
			distinctPods := make(map[string]bool)
			for _, pod := range podAssignments {
				distinctPods[pod] = true
			}

			calculateDistributionStats(t, "Fairness Test", convertToHistogram(podAssignments))
		}
	})

	// Sub-test 3: Utilization Component - Verify that the algorithm distributes load
	t.Run("UtilizationComponent", func(t *testing.T) {
		if len(availablePods) <= 1 {
			t.Skip("Skipping utilization test due to insufficient pod count")
		}

		podAssignments := make(map[string]map[string]bool)
		for _, user := range users {
			podAssignments[user] = make(map[string]bool)
			for i := 0; i < 5; i++ {
				pod := getTargetPodFromChatCompletionWithUser(t,
					fmt.Sprintf("Utilization test request %d", i), "vtc-basic", user)
				podAssignments[user][pod] = true
			}

			podCount := len(podAssignments[user])
			t.Logf("User %s was routed to %d different pods", user, podCount)

			// This is a reasonable expectation for good utilization
			if len(availablePods) >= 3 {
				assert.GreaterOrEqual(t, podCount, 2,
					"Expected user %s to be routed to at least 2 different pods for good utilization", user)
			}
		}

		calculateDistributionStats(t, "Utilization Test", flattenUtilizationMap(podAssignments))
	})

	// Sub-test 4: Hybrid Scoring - Verify the combined fairness and utilization approach
	t.Run("HybridScoring", func(t *testing.T) {
		if len(availablePods) <= 1 {
			t.Skip("Skipping hybrid scoring test due to insufficient pod count")
		}

		testMsg := "Test message for hybrid scoring."
		for range 3 {
			for _, user := range users {
				getTargetPodFromChatCompletionWithUser(t, testMsg, "vtc-basic", user)
			}
		}

		podAssignments := make(map[string]string)
		for _, user := range users {
			tokenCount, _ := tokenTracker.GetTokenCount(context.Background(), user)
			pod := getTargetPodFromChatCompletionWithUser(t, testMsg, "vtc-basic", user)
			podAssignments[user] = pod
			t.Logf("User %s (tokens: %.2f) routed to pod %s", user, tokenCount, pod)
		}

		distribution := convertToHistogram(podAssignments)
		calculateDistributionStats(t, "Hybrid Test", distribution)

		// Note: We can't always guarantee distribution across pods in a real environment
		// as it depends on the current state of the system
		for _, user := range users {
			assert.NotEmpty(t, podAssignments[user],
				"Expected valid pod assignment for user %s", user)
		}
	})
}

// Helper function to get target pod with user header and track token usage
func getTargetPodFromChatCompletionWithUser(t *testing.T, message, strategy, user string) string {
	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategyAndUser(
		gatewayURL, apiKey, strategy, user, option.WithResponseInto(&dst),
	)

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: openai.F([]openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(message),
		}),
		Model: openai.F(modelName),
	})
	require.NoError(t, err, "chat completions failed %v", err)
	assert.Equal(t, modelName, chatCompletion.Model)

	if tokenTracker != nil && tokenEstimator != nil {
		inputTokens := tokenEstimator.EstimateInputTokens(message)
		outputTokens := tokenEstimator.EstimateOutputTokens(chatCompletion.Choices[0].Message.Content)
		err := tokenTracker.UpdateTokenCount(context.Background(), user, inputTokens, outputTokens)
		require.NoError(t, err, "failed to update token count for user %s", user)
	}

	return dst.Header.Get("target-pod")
}

func convertToHistogram(podAssignments map[string]string) map[string]int {
	histogram := make(map[string]int)
	for _, pod := range podAssignments {
		histogram[pod]++
	}
	return histogram
}

func flattenUtilizationMap(podAssignments map[string]map[string]bool) map[string]int {
	histogram := make(map[string]int)
	for _, podMap := range podAssignments {
		for pod := range podMap {
			histogram[pod]++
		}
	}
	return histogram
}

func calculateDistributionStats(t *testing.T, phaseName string, histogram map[string]int) {
	if len(histogram) == 0 {
		t.Logf("[Distribution] %s: No data available", phaseName)
		return
	}

	total := 0
	for _, count := range histogram {
		total += count
	}

	mean := float64(total) / float64(len(histogram))
	var sumSquared float64
	for _, count := range histogram {
		sumSquared += float64(count) * float64(count)
	}
	variance := sumSquared/float64(len(histogram)) - mean*mean
	stddev := math.Sqrt(variance)
	cv := stddev / mean // Coefficient of variation

	t.Logf("[Distribution] %s: %d pods, %d requests", phaseName, len(histogram), total)
	for pod, count := range histogram {
		percentage := float64(count) / float64(total) * 100
		t.Logf("[Distribution] %s: Pod %s received %d requests (%.1f%%)", phaseName, pod, count, percentage)
	}
	t.Logf("[Distribution] %s: Mean=%.2f, StdDev=%.2f, CV=%.2f", phaseName, mean, stddev, cv)

	if cv < 0.1 {
		t.Logf("[Distribution] %s: EXCELLENT distribution (CV < 0.1)", phaseName)
	} else if cv < 0.3 {
		t.Logf("[Distribution] %s: GOOD distribution (CV < 0.3)", phaseName)
	} else if cv < 0.5 {
		t.Logf("[Distribution] %s: FAIR distribution (CV < 0.5)", phaseName)
	} else {
		t.Logf("[Distribution] %s: POOR distribution (CV >= 0.5)", phaseName)
	}
}

// Create OpenAI client with routing strategy and user header
func createOpenAIClientWithRoutingStrategyAndUser(baseURL, apiKey, routingStrategy, user string,
	respOpt option.RequestOption) *openai.Client {
	return openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
		option.WithMiddleware(func(r *http.Request, mn option.MiddlewareNext) (*http.Response, error) {
			r.URL.Path = "/v1" + r.URL.Path
			return mn(r)
		}),
		option.WithHeader("routing-strategy", routingStrategy),
		option.WithHeader("user", user),
		option.WithMaxRetries(0),
		respOpt,
	)
}
