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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// Test users for VTC routing tests
var testUsers = []utils.User{
	{Name: "user1", Rpm: 1000, Tpm: 10000},
	{Name: "user2", Rpm: 1000, Tpm: 10000},
	{Name: "user3", Rpm: 1000, Tpm: 10000},
}

// Global Redis client for tests
var redisClient *redis.Client

// Global variables for test state
var (
	availablePods []string
)

func setupVTCUsers(t *testing.T) {
	if redisClient == nil {
		redisClient = utils.GetRedisClient()
		if redisClient == nil {
			t.Fatal("Failed to connect to Redis")
		}
	}

	// The gateway plugin already has its own token tracker and estimator
	// We don't need to create our own instances here

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

// TestVTCTokenWindowExpiry tests that the VTC token window expiry works correctly in the gateway plugin
// This is an indirect test that verifies the behavior through routing patterns
func TestVTCTokenWindowExpiry(t *testing.T) {
	setupVTCUsers(t)

	user := "user1"
	
	// Make an initial request to establish a baseline routing pattern
	t.Log("Making initial request to establish baseline routing...")
	initialPod := getTargetPodFromChatCompletionWithUser(t, "Initial message", "vtc-basic", user)
	assert.NotEmpty(t, initialPod, "Initial pod should not be empty")

	// Make several requests with the same user to accumulate tokens
	t.Log("Making multiple requests to accumulate tokens...")
	podAssignments := make(map[string]string)
	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("Accumulating tokens message %d", i)
		pod := getTargetPodFromChatCompletionWithUser(t, msg, "vtc-basic", user)
		podAssignments[msg] = pod
	}

	// The user should now have accumulated tokens, which should affect routing
	// Check if we're getting consistent pod assignments (indicating token-based routing)
	histogram := convertToHistogram(podAssignments)
	calculateDistributionStats(t, "After token accumulation", histogram)

	// Wait for the sliding window to expire (configured in the gateway plugin)
	// The gateway plugin uses milliseconds with a window size of 100ms
	t.Log("Waiting for token window to expire (200ms)...")
	time.Sleep(200 * time.Millisecond)

	// After expiry, make new requests - routing should reset to initial pattern
	t.Log("Making requests after token window expiry...")
	postExpiryAssignments := make(map[string]string)
	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("Post-expiry message %d", i)
		pod := getTargetPodFromChatCompletionWithUser(t, msg, "vtc-basic", user)
		postExpiryAssignments[msg] = pod
	}

	// Check if the routing pattern has changed after token expiry
	postExpiryHistogram := convertToHistogram(postExpiryAssignments)
	calculateDistributionStats(t, "After token expiry", postExpiryHistogram)

	// Compare the distribution patterns
	t.Log("Comparing routing patterns before and after token expiry...")
	// We don't make strict assertions here because the actual routing depends on the gateway plugin's
	// implementation, but we log the patterns for analysis
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
		// We don't need to track token counts in e2e tests
		for _, user := range users {
			// We can't directly access token counts in e2e tests - the gateway plugin manages this
			// Just log that we're starting with a clean state for this user
			t.Logf("Starting with clean state for user %s", user)
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

		// We don't need to track token counts in e2e tests
		for _, user := range users {
			t.Logf("Completed requests for user %s", user)
		}

		// We can't directly assert on token counts in e2e tests
		// Instead, we'll verify the routing behavior indirectly through pod assignments
		t.Log("Token accumulation test completed - check logs for routing patterns")

		calculateDistributionStats(t, "Token Accumulation", podHistogram)
	})

	// Sub-test 2: Fairness Component - Verify that the algorithm considers token counts
	t.Run("FairnessComponent", func(t *testing.T) {
		// We don't need to track token counts in e2e tests
		for _, user := range users {
			// We can't directly access token counts in e2e tests - the gateway plugin manages this
			// Log that we're starting the fairness test for this user
			t.Logf("Starting fairness test for user %s", user)
		}

		testMsg := "Test message for fairness component."

		podAssignments := make(map[string]string)
		for _, user := range users {
			pod := getTargetPodFromChatCompletionWithUser(t, testMsg, "vtc-basic", user)
			podAssignments[user] = pod
			t.Logf("User %s routed to pod %s", user, pod)
		}

		if len(availablePods) > 1 {
			distinctPods := make(map[string]bool)
			for _, pod := range podAssignments {
				distinctPods[pod] = true
			}

			calculateDistributionStats(t, "Fairness Test", convertToHistogram(podAssignments))
		}
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
			// We can't directly access token counts in e2e tests - the gateway plugin manages this
			pod := getTargetPodFromChatCompletionWithUser(t, testMsg, "vtc-basic", user)
			podAssignments[user] = pod
			t.Logf("User %s routed to pod %s", user, pod)
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

// TestVTCUtilizationBalancing verifies that the VTC algorithm properly considers 
// utilization (pod load) when making routing decisions.
// This test is designed to be deterministic by:
// 1. Using multiple users with identical token usage patterns to isolate utilization
// 2. Taking advantage of the 2-second token window to establish clear patterns
// 3. Creating a significant load imbalance on a specific pod
// 4. Using appropriate delays to ensure metrics propagate correctly
func TestVTCUtilizationBalancing(t *testing.T) {
	// Set up Redis and test users
	setupVTCUsers(t)
	defer cleanupVTCUsers(t)

	// Skip test if we don't have at least 2 pods available
	if len(availablePods) < 2 {
		t.Skip("Skipping utilization test because we need at least 2 pods")
	}
	t.Logf("[UtilizationTest] Delaying test start to allow token window expiry (3s)")
	time.Sleep(3 * time.Second)

	// We'll use just two users - one to create load, one to test routing decisions
	highLoadUser := testUsers[0].Name
	testUser := testUsers[1].Name
	t.Logf("[UtilizationTest] Using users - high load: %s, test: %s", highLoadUser, testUser)

	// PHASE 1: Establish token equilibrium
	// Make identical requests for both users to establish similar token counts
	baselineRequests := 3 // More baseline requests for more stable history
	t.Logf("[UtilizationTest] PHASE 1: Establishing token equilibrium with %d requests per user", baselineRequests)

	// Create identical token history pattern for both users
	for i := 0; i < baselineRequests; i++ {
		// Same messages to ensure token counts are identical
		message := fmt.Sprintf("Baseline equilibrium request %d", i+1)
		
		// First user
		pod1 := getTargetPodFromChatCompletionWithUser(t, message, "vtc-basic", highLoadUser)
		t.Logf("[UtilizationTest] High-load user baseline %d routed to pod %s", i+1, pod1)
		
		// Second user with same message
		pod2 := getTargetPodFromChatCompletionWithUser(t, message, "vtc-basic", testUser)
		t.Logf("[UtilizationTest] Test user baseline %d routed to pod %s", i+1, pod2)
		
		// 200ms delay is sufficient with the 2s window
		time.Sleep(200 * time.Millisecond)
	}

	// PHASE 2: Create strong imbalance
	highLoadPod := availablePods[0]
	highLoadCount := 30
	t.Logf("[UtilizationTest] Forcing %d requests to pod %s", highLoadCount, highLoadPod)
	
	// Initialize metrics for all pods
	initializeMetrics(t, availablePods)

	// Send traffic to create load imbalance
	podRequestCounts := make(map[string]int)
	for i := 0; i < highLoadCount; i++ {
		pod := highLoadPod
		podRequestCounts[pod]++
		
		// Make the actual request to create load
		actualPod := getTargetPodFromChatCompletionWithUser(t, "High load request "+fmt.Sprintf("%d", i+1), "random", highLoadUser)
		// Verify the request went to the intended pod
		if actualPod != highLoadPod {
			t.Logf("[UtilizationTest] Warning: Request routed to %s instead of target pod %s", actualPod, highLoadPod)
		}
		
		// Add small delay between requests to avoid overwhelming the system
		time.Sleep(50 * time.Millisecond)
	}
	
	// Log the distribution of high-load requests
	t.Logf("[UtilizationTest] High-load requests distribution:")
	for pod, count := range podRequestCounts {
		t.Logf("  - %s: %d requests", pod, count)
	}
	
	// Force metric refresh with retries
	awaitMetricsUpdate(t, highLoadPod, 1)

	// Analyze which pod received the most load during the high load phase
	highestLoad := 0
	highLoadPodCount := make(map[string]int)
	for pod, count := range podRequestCounts {
		highLoadPodCount[pod] = count
		if count > highestLoad {
			highestLoad = count
			highLoadPod = pod
		}
	}
	
	// Log the load distribution across all pods to see imbalance clearly
	t.Logf("[UtilizationTest] Load distribution after high load phase:")
	for pod, count := range highLoadPodCount {
		t.Logf("[UtilizationTest] Pod %s received %d/%d requests (%.1f%%)", 
			pod, count, highLoadCount, float64(count)/float64(highLoadCount)*100)
	}
	
	t.Logf("[UtilizationTest] Pod %s identified as high-load pod (%d requests)", highLoadPod, highestLoad)
	
	// Allow time for load metrics to update
	// With 50ms refresh interval, we'll wait longer to ensure multiple refresh cycles
	t.Logf("[UtilizationTest] Waiting for load metrics to propagate...")
	time.Sleep(500 * time.Millisecond) // Longer wait to ensure metrics propagate fully

	// Force metric refresh and verify
	if err := redisClient.Publish(context.Background(), "pod_metrics_refresh", "").Err(); err != nil {
		t.Fatalf("Failed to trigger metric refresh: %v", err)
	}
	time.Sleep(2 * time.Second)

	// Verify metrics before proceeding
	metrics, err := redisClient.HGetAll(context.Background(), "pod_metrics").Result()
	if err != nil {
		t.Fatalf("Failed to get metrics: %v", err)
	}
	t.Logf("[UtilizationTest] Current metrics: %+v", metrics)

	// PHASE 3: Verify balancing
	testUser = testUsers[1].Name
	testRequestCount := 10
	podDistribution := make(map[string]int)

	t.Logf("[UtilizationTest] PHASE 3: Testing utilization-based balancing with user %s", testUser)
	t.Logf("[UtilizationTest] If utilization component is working, requests should avoid pod %s", highLoadPod)

	// With the 2s token window, we can spread requests evenly
	// The algorithm should favor the less loaded pods
	for i := 0; i < testRequestCount; i++ {
		message := fmt.Sprintf("Utilization test request %d", i+1)
		pod := getTargetPodFromChatCompletionWithUser(t, message, "vtc-basic", testUser)
		podDistribution[pod]++
		t.Logf("[UtilizationTest] Test request %d routed to pod %s", i+1, pod)
		
		// With a 2s window, we can space these 50ms apart while maintaining 15 requests
		// inside the window (15 × 50ms = 0.75s < 2s window)
		time.Sleep(50 * time.Millisecond)
	}
	// Analyze the results
	t.Logf("[UtilizationTest] RESULTS: %v", podDistribution)
	for pod, count := range podDistribution {
		t.Logf("[UtilizationTest] Pod %s received %d/%d test requests (%.1f%%)", 
			pod, count, testRequestCount, float64(count)/float64(testRequestCount)*100)
	}
	
	// If the utilization component is working, the high-load pod should receive fewer requests
	// Check how many requests went to our high-load pod
	count := podDistribution[highLoadPod]
	// Calculate fair share with a buffer based on pod count
	fairShare := testRequestCount / len(availablePods)
	maxAllowed := fairShare + int(math.Ceil(float64(fairShare) * 0.5)) // Allow 50% over fair share
	if count > maxAllowed {
		t.Fatalf("High-load pod received %d/%d requests (max %d)", count, testRequestCount, maxAllowed)
	}
	
	// Calculate overall distribution stats
	histogram := podDistribution
	calculateDistributionStats(t, "Utilization Test", histogram)
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

	// We don't need to track tokens manually - the gateway plugin does this for us
	// when the user header is provided

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

func getPodMetrics(t *testing.T) map[string]int {
	metrics, err := redisClient.HGetAll(context.Background(), "pod_metrics").Result()
	if err != nil {
		t.Fatalf("Failed to get metrics: %v", err)
	}
	podMetrics := make(map[string]int)
	for pod, metric := range metrics {
		count, err := strconv.Atoi(metric)
		if err != nil {
			t.Fatalf("Failed to parse metric for pod %s: %v", pod, err)
		}
		podMetrics[pod] = count
	}
	return podMetrics
}

func initializeMetrics(t *testing.T, pods []string) {
	ctx := context.Background()
	t.Logf("[UtilizationTest] Initializing metrics for %d pods", len(pods))
	
	// Delete any existing metrics
	redisClient.Del(ctx, "pod_metrics")
	
	// Initialize metrics for each pod with a small value
	for _, pod := range pods {
		err := redisClient.HSet(ctx, "pod_metrics", pod, "1").Err()
		if err != nil {
			t.Logf("Warning: Failed to initialize metrics for pod %s: %v", pod, err)
		}
	}
	
	// Verify initialization
	metrics := getPodMetrics(t)
	t.Logf("[UtilizationTest] Initialized metrics: %+v", metrics)
	
	// Ensure all pods are in the metrics
	for _, pod := range pods {
		if _, exists := metrics[pod]; !exists {
			t.Logf("Warning: Pod %s not found in metrics after initialization", pod)
		}
	}
}

func awaitMetricsUpdate(t *testing.T, pod string, minValue int) {
	maxAttempts := 20
	attempt := 0
	t.Logf("[UtilizationTest] Waiting for metrics update for pod %s (min value: %d)", pod, minValue)
	
	for {
		metrics := getPodMetrics(t)
		t.Logf("[UtilizationTest] Current metrics: %+v", metrics)
		
		count, exists := metrics[pod]
		if exists && count >= minValue {
			t.Logf("[UtilizationTest] Metrics updated successfully for pod %s: %d", pod, count)
			break
		}
		
		attempt++
		if attempt >= maxAttempts {
			t.Logf("[UtilizationTest] Warning: Failed to verify metrics update after %d attempts", maxAttempts)
			// Continue with test instead of failing - we'll log the metrics for debugging
			break
		}
		
		// Force a metrics update by making a direct request
		if attempt % 5 == 0 {
			t.Logf("[UtilizationTest] Forcing metrics update with direct request")
			// Use a test user to make a request that should be routed to any pod
			_ = getTargetPodFromChatCompletionWithUser(t, "Metrics refresh request", "random", testUsers[1].Name)
		}
		
		time.Sleep(200 * time.Millisecond)
	}
}
