//go:build flaky_vtc_e2e
// +build flaky_vtc_e2e

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

// standalone test file runs pass, a bit tricky to run with other e2e tests. We have good unit tests though.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
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
	requestDelay        = 50 * time.Millisecond
	metricsWaitTime     = 50 * time.Millisecond
	highLoadValue       = "100"
	normalLoadValue     = "10"
	utilTolerance       = 1
)

var testUsers = []utils.User{
	{Name: "user1", Rpm: 1000, Tpm: 10000},
	{Name: "user2", Rpm: 1000, Tpm: 10000},
	{Name: "user3", Rpm: 1000, Tpm: 10000},
}

// DistributionQuality represents the quality of request distribution across pods
type DistributionQuality int

const (
	PoorDistribution DistributionQuality = iota
	FairDistribution
	GoodDistribution
	ExcellentDistribution
)

var distributionQualityStrings = map[DistributionQuality]string{
	ExcellentDistribution: "EXCELLENT",
	GoodDistribution:      "GOOD",
	FairDistribution:      "FAIR",
	PoorDistribution:      "POOR",
}

var redisClient *redis.Client

var (
	availablePods []string
)

func (d DistributionQuality) String() string {
	if str, ok := distributionQualityStrings[d]; ok {
		return str
	}
	return "UNKNOWN"
}

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
		redisClient.Close()
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

func TestVTCBasicValidPod(t *testing.T) {
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

func TestVTCBasicRouting(t *testing.T) {
	setupVTCUsers(t)

	users := []string{"user1", "user2", "user3"}
	shortMsg := "Short message."
	mediumMsg := "This is a medium length message with more tokens."
	longMsg := "This is a very long message with many tokens. " +
		"It should be significantly longer than the others to ensure higher token count. " +
		"We want to make sure the VTC algorithm properly accounts for different token usages."

	ensureSufficientPods(t, 3)

	// Sub-test 1: Verify that users accumulate tokens based on message size
	t.Run("TokenAccumulation", func(t *testing.T) {
		for _, user := range users {
			t.Logf("Starting with clean state for user %s", user)
		}

		msgMap := map[string]string{
			users[0]: shortMsg,
			users[1]: mediumMsg,
			users[2]: longMsg,
		}

		podHistogram := make(map[string]int)

		for i := 0; i < 5; i++ {
			for _, user := range users {
				pod := getTargetPodFromChatCompletionWithUser(t, msgMap[user], "vtc-basic", user)
				podHistogram[pod]++
				forceMetricsPropagation(t)
			}
		}

		for _, user := range users {
			t.Logf("Completed requests for user %s", user)
		}

		t.Log("Token accumulation test completed - check logs for routing patterns")

		cv, quality := calculateDistributionStats(t, "Token Accumulation", podHistogram)

		assert.Less(t, quality, ExcellentDistribution,
			"Token accumulation should show some imbalance (not EXCELLENT), got %s distribution with CV=%.2f",
			quality, cv)
	})

	// Sub-test 2: Verify that the algorithm considers token counts
	t.Run("FairnessComponent", func(t *testing.T) {
		for _, user := range users {
			t.Logf("Starting fairness test for user %s", user)
		}

		testMsg := "Test message for fairness component."

		podAssignments := make(map[string]string)
		for _, user := range users {
			pod := getTargetPodFromChatCompletionWithUser(t, testMsg, "vtc-basic", user)
			podAssignments[user] = pod
			t.Logf("User %s routed to pod %s", user, pod)
			forceMetricsPropagation(t)
		}

		distinctPods := make(map[string]bool)
		for _, pod := range podAssignments {
			distinctPods[pod] = true
		}

		cv, quality := calculateDistributionStats(t, "Fairness Test", convertToHistogram(podAssignments))

		assert.Greater(t, len(distinctPods), 1,
			"Expected at least 2 pods for request distribution, but none were used")

		assert.GreaterOrEqual(t, quality, FairDistribution,
			"Distribution should be at least FAIR with CV=%.2f", cv)
	})
}

func TestVTCUtilizationBalancing(t *testing.T) {
	setupVTCUsers(t)
	defer cleanupVTCUsers(t)

	t.Logf("Waiting for token window expiry to ensure clean state")
	time.Sleep(tokenWindowDuration)

	// Make one small request for each user to trigger pruning of token buckets (bcos of InMemoryTokenTracker)
	t.Logf("Making pruning-trigger requests for all test users")
	for _, user := range testUsers {
		pod := getTargetPodFromChatCompletionWithUser(t, "Pruning trigger", "vtc-basic", user.Name)
		t.Logf("User %s routed to pod %s (pruning trigger)", user.Name, pod)
	}

	ensureSufficientPods(t, 3)

	metrics := getPodMetrics(t)
	t.Logf("Pod metrics before controlled setup: %v", metrics)
	highLoadPod := availablePods[0]
	testUser := testUsers[1].Name
	t.Logf("Creating controlled load imbalance in Redis for pod %s", highLoadPod)
	ctx := context.Background()

	redisClient.Del(ctx, "pod_metrics")
	for _, pod := range availablePods {
		if pod == highLoadPod {
			redisClient.HSet(ctx, "pod_metrics", pod, highLoadValue)
			t.Logf("Set high load (%s) for pod %s", highLoadValue, pod)
		} else {
			redisClient.HSet(ctx, "pod_metrics", pod, normalLoadValue)
			t.Logf("Set normal load (%s) for pod %s", normalLoadValue, pod)
		}
	}

	forceMetricsPropagation(t)
	metrics = getPodMetrics(t)
	t.Logf("Pod metrics after controlled setup: %v", metrics)
	ensureSufficientPods(t, 3)

	testRequestCount := 10
	podDistribution := make(map[string]int)

	t.Logf("Testing utilization balancing with user %s", testUser)
	for i := 0; i < testRequestCount; i++ {
		pod := getTargetPodFromChatCompletionWithUser(t,
			fmt.Sprintf("Test request %d", i), "vtc-basic", testUser)
		podDistribution[pod]++
		t.Logf("Test request %d routed to pod %s", i+1, pod)
		forceMetricsPropagation(t)
	}

	t.Logf("Test request distribution:")
	for pod, count := range podDistribution {
		t.Logf("  %s: %d requests (%.1f%%)", pod, count,
			float64(count)/float64(testRequestCount)*100)
	}

	count := podDistribution[highLoadPod]
	maxAllowed := testRequestCount / 2
	t.Logf("High-load pod received %d/%d requests (max allowed: %d)",
		count, testRequestCount, maxAllowed)
	calculateDistributionStats(t, "Utilization Test", podDistribution)

	if count > maxAllowed {
		t.Fatalf("High-load pod received %d/%d requests (max %d)",
			count, testRequestCount, maxAllowed)
	}
}

func ensureSufficientPods(t *testing.T, minPods int) {
	getAvailablePods(t)
	if len(availablePods) < minPods {
		t.Skipf("Need at least %d pods for utilization test, found %d", minPods, len(availablePods))
	}
	t.Logf("Found %d available pods, proceeding with test", len(availablePods))
}

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

func calculateDistributionStats(t *testing.T, phaseName string, histogram map[string]int) (float64, DistributionQuality) {
	if len(histogram) == 0 {
		t.Logf("[Distribution] %s: No data available", phaseName)
		return 0, PoorDistribution
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

	var quality DistributionQuality
	if cv < 0.1 {
		quality = ExcellentDistribution
		t.Logf("[Distribution] %s: EXCELLENT distribution (CV < 0.1)", phaseName)
	} else if cv < 0.3 {
		quality = GoodDistribution
		t.Logf("[Distribution] %s: GOOD distribution (CV < 0.3)", phaseName)
	} else if cv < 0.5 {
		quality = FairDistribution
		t.Logf("[Distribution] %s: FAIR distribution (CV < 0.5)", phaseName)
	} else {
		quality = PoorDistribution
		t.Logf("[Distribution] %s: POOR distribution (CV >= 0.5)", phaseName)
	}

	return cv, quality
}

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

func forceMetricsPropagation(t *testing.T) {
	t.Logf("Forcing metrics propagation")
	redisClient.Publish(context.Background(), "pod_metrics_refresh", "")
	time.Sleep(metricsWaitTime)
}
