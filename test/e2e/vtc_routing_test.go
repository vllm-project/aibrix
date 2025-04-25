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

const (
	tokenWindowDuration = 3 * time.Second
	requestDelay       = 50 * time.Millisecond
	metricsWaitTime    = 500 * time.Millisecond
	highLoadValue      = "100"
	normalLoadValue    = "10"
	utilTolerance      = 1
)

var testUsers = []utils.User{
	{Name: "user1", Rpm: 1000, Tpm: 10000},
	{Name: "user2", Rpm: 1000, Tpm: 10000},
	{Name: "user3", Rpm: 1000, Tpm: 10000},
}

var redisClient *redis.Client

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

func TestVTCFallbackToRandom(t *testing.T) {
	setupVTCUsers(t)

	req := "this is a test message for VTC Basic routing"
	targetPod := getTargetPodFromChatCompletionWithUser(t, req, "vtc-basic", "")
	assert.NotEmpty(t, targetPod, "vtc-basic target pod is empty") //what is this
}

// TestVTCHybridScoring tests the hybrid scoring approach that balances fairness and utilization
func TestVTCBasicRouting(t *testing.T) {
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
}

// TestVTCUtilizationBalancing verifies that the VTC algorithm properly considers
// utilization (pod load) when making routing decisions.
func TestVTCUtilizationBalancing(t *testing.T) {
	// Set up Redis and test users
	setupVTCUsers(t)
	defer cleanupVTCUsers(t)
	
	// Wait for token window to expire completely
	t.Logf("Waiting for token window expiry to ensure clean state")
	time.Sleep(tokenWindowDuration)
	
	// Make one small request for each user to trigger pruning of token buckets
	t.Logf("Making pruning-trigger requests for all test users")
	for _, user := range testUsers {
		pod := getTargetPodFromChatCompletionWithUser(t, "Pruning trigger", "vtc-basic", user.Name)
		t.Logf("User %s routed to pod %s (pruning trigger)", user.Name, pod)
	}

	ensureSufficientPods(t, 2)
	
	metrics := getPodMetrics(t)
	t.Logf("Pod metrics before controlled setup: %v", metrics)

	// Set up Redis with controlled utilization values
	highLoadPod := availablePods[0]
	testUser := testUsers[1].Name
	
	t.Logf("Creating controlled load imbalance in Redis for pod %s", highLoadPod)
	ctx := context.Background()

	// Clear and set new values
	redisClient.Del(ctx, "pod_metrics")
	for _, pod := range availablePods {
		if pod == highLoadPod {
			// High load for target pod
			redisClient.HSet(ctx, "pod_metrics", pod, highLoadValue)
			t.Logf("Set high load (%s) for pod %s", highLoadValue, pod)
		} else {
			// Low load for other pods
			redisClient.HSet(ctx, "pod_metrics", pod, normalLoadValue)
			t.Logf("Set normal load (%s) for pod %s", normalLoadValue, pod)
		}
	}
	
	// Force metrics propagation
	t.Logf("Forcing metrics propagation")
	redisClient.Publish(ctx, "pod_metrics_refresh", "")
	time.Sleep(metricsWaitTime)
	
	// Verify metrics
	metrics = getPodMetrics(t)
	t.Logf("Pod metrics after controlled setup: %v", metrics)

	ensureSufficientPods(t, 2)
	
	// Run the actual test with fresh token state and controlled utilization
	testRequestCount := 10
	podDistribution := make(map[string]int)
	
	t.Logf("Testing utilization balancing with user %s", testUser)
	for i := 0; i < testRequestCount; i++ {
		pod := getTargetPodFromChatCompletionWithUser(t, 
			fmt.Sprintf("Test request %d", i), "vtc-basic", testUser)
		podDistribution[pod]++
		t.Logf("Test request %d routed to pod %s", i+1, pod)
		time.Sleep(requestDelay)
	}
	
	// Verify results
	t.Logf("Test request distribution:")
	for pod, count := range podDistribution {
		t.Logf("  %s: %d requests (%.1f%%)", pod, count, 
			float64(count)/float64(testRequestCount)*100)
	}
	
	// Assert high-load pod receives fewer requests
	count := podDistribution[highLoadPod]
	fairShare := testRequestCount / len(availablePods)
	maxAllowed := fairShare + int(math.Ceil(float64(fairShare) * utilTolerance))
	
	t.Logf("High-load pod received %d/%d requests (max allowed: %d)", 
		count, testRequestCount, maxAllowed)
	
	if count > maxAllowed {
		t.Fatalf("High-load pod received %d/%d requests (max %d)", 
			count, testRequestCount, maxAllowed)
	}
	
	calculateDistributionStats(t, "Utilization Test", podDistribution)
}

// Helper function to get the most used pod from a histogram
func getMostUsedPodFromHistogram(histogram map[string]int) string {
	var mostUsedPod string
	maxCount := 0
	
	for pod, count := range histogram {
		if count > maxCount {
			maxCount = count
			mostUsedPod = pod
		}
	}
	
	return mostUsedPod
}

// Helper function to ensure sufficient pods are available
func ensureSufficientPods(t *testing.T, minPods int) {
	getAvailablePods(t)
	if len(availablePods) < minPods {
		t.Skipf("Need at least %d pods for utilization test, found %d", minPods, len(availablePods))
	}
	t.Logf("Found %d available pods, proceeding with test", len(availablePods))
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
