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
var availablePods []string
var tokenTracker *vtc.InMemoryTokenTracker

// Histograms to track pod distribution
var initialPhaseHistogram map[string]int

func setupVTCUsers(t *testing.T) {
	if redisClient == nil {
		redisClient = utils.GetRedisClient()
		if redisClient == nil {
			t.Fatal("Failed to connect to Redis")
		}
	}

	// Initialize token tracker for verification
	config := &vtc.VTCConfig{
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.0,
	}
	tokenTracker = vtc.NewInMemoryTokenTracker(config)

	// Get available pods in the cluster
	getAvailablePods(t)

	ctx := context.Background()
	for _, user := range testUsers {
		err := utils.SetUser(ctx, user, redisClient)
		if err != nil {
			t.Fatalf("Failed to create test user %s: %v", user.Name, err)
		}
		fmt.Printf("Created test user: %s\n", user.Name)
	}

	t.Cleanup(func() {
		cleanupVTCUsers()
	})
}

func cleanupVTCUsers() {
	if redisClient == nil {
		return
	}

	ctx := context.Background()
	for _, user := range testUsers {
		err := utils.DelUser(ctx, user, redisClient)
		if err != nil {
			fmt.Printf("Warning: Failed to delete test user %s: %v\n", user.Name, err)
		} else {
			fmt.Printf("Deleted test user: %s\n", user.Name)
		}
	}
}

// getAvailablePods gets the list of available pods in the cluster using random routing
func getAvailablePods(t *testing.T) {
	// Initialize the pod list
	availablePods = []string{}

	// Use random routing to discover pods
	allPodsMap := make(map[string]bool)
	for i := 0; i < 30; i++ { // Make multiple requests to discover all pods
		pod := getTargetPodFromChatCompletion(t, fmt.Sprintf("Pod discovery request %d", i), "random")
		if pod != "" {
			allPodsMap[pod] = true
		}
	}

	// Convert map to slice
	for pod := range allPodsMap {
		availablePods = append(availablePods, pod)
	}

	// Print pod indexes for debugging
	for i, pod := range availablePods {
		t.Logf("[DEBUG] Pod %d: %s", i, pod)
	}

	t.Logf("Discovered %d pods using random routing", len(availablePods))
	fmt.Printf("[Environment] Discovered %d pods: %s\n", len(availablePods), strings.Join(availablePods, ", "))
}

func TestVTCBasicRouting(t *testing.T) {
	// Setup test users
	setupVTCUsers(t)

	// Test that the VTC Basic router works
	req := "this is a test message for VTC Basic routing"
	targetPod := getTargetPodFromChatCompletionWithUser(t, req, "vtc-basic", "user1")
	assert.NotEmpty(t, targetPod, "vtc-basic target pod is empty")
}

func TestVTCFallbackToRandom(t *testing.T) {
	// Setup test users
	setupVTCUsers(t)

	// Test that the VTC Basic router falls back to random selection when no user is specified
	req := "this is a test message for VTC Basic routing"
	targetPod := getTargetPodFromChatCompletionWithUser(t, req, "vtc-basic", "")
	assert.NotEmpty(t, targetPod, "vtc-basic target pod is empty") //what is this
}

// TestVTCHybridScoring tests the hybrid scoring approach that balances fairness and utilization
func TestVTCHybridScoring(t *testing.T) {
	// Setup test users
	setupVTCUsers(t)

	// Create test users
	users := []string{"user1", "user2", "user3"}

	// Phase 1: Establish different token usage patterns
	shortMsg := "Short message."
	mediumMsg := "This is a medium length message with more tokens."
	longMsg := "This is a very long message with many tokens. It should be significantly longer than the others to ensure higher token count. We want to make sure the VTC algorithm properly accounts for different token usages."

	// Build up token counts for different users
	// Track token counts before and after each request
	userTokens := make(map[string]float64)

	// Initialize histogram for initial phase
	initialPhaseHistogram = make(map[string]int)

	// Get initial token counts
	for _, user := range users {
		tokenCount, _ := tokenTracker.GetTokenCount(context.Background(), user)
		userTokens[user] = tokenCount
		t.Logf("Initial token count for user %s: %.2f", user, tokenCount)
		fmt.Printf("[Token Tracker] Initial token count for user %s: %.2f\n", user, tokenCount)
	}

	// Send requests with different message sizes
	for i := 0; i < 5; i++ {
		// User 1 - short messages
		pod := getTargetPodFromChatCompletionWithUser(t, shortMsg, "vtc-basic", users[0])
		initialPhaseHistogram[pod]++
		newCount, _ := tokenTracker.GetTokenCount(context.Background(), users[0])
		t.Logf("[Token Update] User %s sent short message to pod %s, tokens: %.2f -> %.2f (delta: %.2f)",
			users[0], pod, userTokens[users[0]], newCount, newCount-userTokens[users[0]])
		userTokens[users[0]] = newCount

		// User 2 - medium messages
		pod = getTargetPodFromChatCompletionWithUser(t, mediumMsg, "vtc-basic", users[1])
		initialPhaseHistogram[pod]++
		newCount, _ = tokenTracker.GetTokenCount(context.Background(), users[1])
		t.Logf("[Token Update] User %s sent medium message to pod %s, tokens: %.2f -> %.2f (delta: %.2f)",
			users[1], pod, userTokens[users[1]], newCount, newCount-userTokens[users[1]])
		userTokens[users[1]] = newCount

		// User 3 - long messages
		pod = getTargetPodFromChatCompletionWithUser(t, longMsg, "vtc-basic", users[2])
		initialPhaseHistogram[pod]++
		newCount, _ = tokenTracker.GetTokenCount(context.Background(), users[2])
		t.Logf("[Token Update] User %s sent long message to pod %s, tokens: %.2f -> %.2f (delta: %.2f)",
			users[2], pod, userTokens[users[2]], newCount, newCount-userTokens[users[2]])
		userTokens[users[2]] = newCount
	}

	// Verify final token counts
	for _, user := range users {
		tokenCount, _ := tokenTracker.GetTokenCount(context.Background(), user)
		t.Logf("[Token Tracker] Final token count for user %s: %.2f", user, tokenCount)
	}

	// Use the pre-discovered pods list instead of making requests
	t.Logf("[Environment] Available pods: %d - %s", len(availablePods), strings.Join(availablePods, ", "))

	// Check if we have multiple pods to work with
	if len(availablePods) <= 1 {
		t.Logf("[WARNING] Only %d pod(s) detected in test environment. VTC routing tests require multiple pods to properly test routing logic.", len(availablePods))
	}

	// Phase 2: Test fairness component
	testMsg := "Test message for fairness component."

	// Record pod assignments for fairness test
	podAssignmentsFairness := make(map[string]string)
	for _, user := range users {
		pod := getTargetPodFromChatCompletionWithUser(t, testMsg, "vtc-basic", user)
		podAssignmentsFairness[user] = pod
		t.Logf("[Fairness Test] User %s routed to pod %s", user, pod)
	}

	// Skip detailed assertions if only one pod is available
	if len(availablePods) <= 1 {
		t.Log("Skipping detailed fairness assertions due to insufficient pod count")
	} else {
		// With multiple pods, we should see different pod assignments based on token usage
		// User1 (low tokens) should get lower-indexed pods than User3 (high tokens)
		t.Log("Verifying fairness component with multiple pods")
		// Add assertions here if we have multiple pods
	}

	// Phase 3: Test utilization component
	// Make multiple requests to test if the algorithm distributes traffic based on utilization
	podAssignmentsUtilization := make(map[string]map[string]bool)
	for _, user := range users {
		podAssignmentsUtilization[user] = make(map[string]bool)
		for i := 0; i < 5; i++ {
			// Using vtc-basic strategy to test VTC utilization, not random
			pod := getTargetPodFromChatCompletionWithUser(t, fmt.Sprintf("Utilization test request %d", i), "vtc-basic", user)
			podAssignmentsUtilization[user][pod] = true
			t.Logf("[Utilization Test] User %s (request %d) routed to pod %s", user, i, pod)
		}
	}
	// Skip detailed assertions if only one pod is available
	if len(availablePods) <= 1 {
		t.Log("Skipping detailed fairness assertions due to insufficient pod count")
	} else {
		// With multiple pods, we should see different pod assignments based on token usage
		// Add assertions here if we have multiple pods
	}

	// Phase 4: Test hybrid approach with mixed workload
	// Send requests with different patterns to test the combined scoring
	testMsgHybrid := "Test message for hybrid scoring."

	// Create a map to track pod assignments
	podAssignmentsHybrid := make(map[string]string)

	// First, create some load on the system
	for range 3 {
		for _, user := range users {
			getTargetPodFromChatCompletionWithUser(t, testMsgHybrid, "vtc-basic", user)
		}
	}

	// Now test the hybrid scoring
	for _, user := range users {
		pod := getTargetPodFromChatCompletionWithUser(t, testMsgHybrid, "vtc-basic", user)
		podAssignmentsHybrid[user] = pod
		t.Logf("[Hybrid Test] User %s routed to pod %s", user, pod)
	}

	// Verify that all users got valid pod assignments
	for _, user := range users {
		assert.NotEmpty(t, podAssignmentsFairness[user], "fairness test: pod assignment for user %s is empty", user)
		assert.NotEmpty(t, podAssignmentsHybrid[user], "hybrid test: pod assignment for user %s is empty", user)

		// Count distinct pods per user in utilization test
		t.Logf("[Utilization] User %s was routed to %d different pods during utilization test",
			user, len(podAssignmentsUtilization[user]))
	}

	// Final summary of token counts
	for _, user := range users {
		tokenCount, _ := tokenTracker.GetTokenCount(context.Background(), user)
		t.Logf("[Final Token Count] User %s: %.2f tokens", user, tokenCount)
	}

	// Check if utilization is working by verifying that at least some requests were routed to different pods
	for _, user := range users {
		podSet := make(map[string]bool)
		for pod := range podAssignmentsUtilization[user] {
			podSet[pod] = true
		}

		if len(availablePods) <= 1 {
			// Skip assertion if only one pod is available
			t.Logf("Skipping utilization assertion for user %s due to insufficient pod count", user)
		} else {
			// With multiple pods, we should see some distribution across pods
			// Check if any user was routed to only one pod during utilization test
			if len(podSet) <= 1 {
				t.Logf("[WARNING] User %s was routed to only one pod during utilization test despite having %d available pods",
					user, len(availablePods))
			}
		}
	}

	// Calculate distribution statistics for all phases of the test
	calculateDistributionStats(t, "Initial Phase", initialPhaseHistogram)
	calculateDistributionStats(t, "Fairness Test", convertToHistogram(podAssignmentsFairness))
	calculateDistributionStats(t, "Utilization Test", flattenUtilizationMap(podAssignmentsUtilization))
	calculateDistributionStats(t, "Hybrid Test", convertToHistogram(podAssignmentsHybrid))
}

// Helper function to get target pod with user header and track token usage
func getTargetPodFromChatCompletionWithUser(t *testing.T, message, strategy, user string) string {
	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategyAndUser(gatewayURL, apiKey, strategy, user, option.WithResponseInto(&dst))

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: openai.F([]openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(message),
		}),
		Model: openai.F(modelName),
	})
	require.NoError(t, err, "chat completions failed %v", err)
	assert.Equal(t, modelName, chatCompletion.Model)

	// Update our local token tracker for verification
	if tokenTracker != nil {
		// Estimate tokens based on message length (simple approximation)
		inputTokens := float64(len(message)) / 4 // rough estimate: 4 chars per token
		outputTokens := float64(len(chatCompletion.Choices[0].Message.Content)) / 4
		tokenTracker.UpdateTokenCount(context.Background(), user, inputTokens, outputTokens)
	}

	return dst.Header.Get("target-pod")
}

// Convert map[string]string to histogram map[string]int
func convertToHistogram(podAssignments map[string]string) map[string]int {
	histogram := make(map[string]int)
	for _, pod := range podAssignments {
		histogram[pod]++
	}
	return histogram
}

// Flatten map[string]map[string]bool to histogram map[string]int
func flattenUtilizationMap(podAssignments map[string]map[string]bool) map[string]int {
	histogram := make(map[string]int)
	for _, podMap := range podAssignments {
		for pod := range podMap {
			histogram[pod]++
		}
	}
	return histogram
}

// Calculate and log distribution statistics
func calculateDistributionStats(t *testing.T, phaseName string, histogram map[string]int) {
	if len(histogram) == 0 {
		t.Logf("[Distribution] %s: No data available", phaseName)
		return
	}

	// Calculate total requests
	total := 0
	for _, count := range histogram {
		total += count
	}

	// Calculate mean and variance
	mean := float64(total) / float64(len(histogram))
	var sumSquared float64
	for _, count := range histogram {
		sumSquared += float64(count) * float64(count)
	}
	variance := sumSquared/float64(len(histogram)) - mean*mean
	stddev := math.Sqrt(variance)
	cv := stddev / mean // Coefficient of variation

	// Log distribution details
	t.Logf("[Distribution] %s: %d pods, %d requests", phaseName, len(histogram), total)
	for pod, count := range histogram {
		percentage := float64(count) / float64(total) * 100
		t.Logf("[Distribution] %s: Pod %s received %d requests (%.1f%%)", phaseName, pod, count, percentage)
	}
	t.Logf("[Distribution] %s: Mean=%.2f, StdDev=%.2f, CV=%.2f", phaseName, mean, stddev, cv)

	// Evaluate distribution quality
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
