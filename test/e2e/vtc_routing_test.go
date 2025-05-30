/*
Copyright 2025 The Aibrix Team.

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
	"os"
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
	tokenWindowDuration = 2 * time.Second
	testCooldownPeriod  = 2500 * time.Millisecond
	requestDelay        = 100 * time.Millisecond
	metricsWaitTime     = 100 * time.Millisecond
	highLoadValue       = "100"
	normalLoadValue     = "10"
	utilTolerance       = 1

	smallMessage  = "Hi there!"
	mediumMessage = "This is a medium message that should generate around 20-30 tokens for testing purposes."
)

var (
	largeMessage = strings.Repeat("This is a large message with many tokens. ", 15)
	hugeMessage  = strings.Repeat(
		"This is a very large message with lots of tokens for testing the VTC algorithm behavior with high token counts. ",
		8)
)

var testUsers = []utils.User{
	{Name: "user1", Rpm: 100000, Tpm: 1000000},
	{Name: "user2", Rpm: 100000, Tpm: 1000000},
	{Name: "user3", Rpm: 100000, Tpm: 1000000},
	{Name: "user4", Rpm: 100000, Tpm: 1000000},
	{Name: "user5", Rpm: 100000, Tpm: 1000000},
	{Name: "user6", Rpm: 100000, Tpm: 1000000},
	{Name: "user7", Rpm: 100000, Tpm: 1000000},
	{Name: "user8", Rpm: 100000, Tpm: 1000000},
	{Name: "user9", Rpm: 100000, Tpm: 1000000},
}

func TestMain(m *testing.M) {
	redisClient = utils.GetRedisClient()
	if redisClient == nil {
		fmt.Println("Failed to connect to Redis")
		os.Exit(1)
	}

	code := m.Run()

	if redisClient != nil {
		if err := redisClient.Close(); err != nil {
			fmt.Printf("Error closing Redis client: %v\n", err)
		}
	}

	os.Exit(code)
}

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
		t.Fatal("Redis client is not initialized")
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
		}
	}
}

func getAvailablePods(t *testing.T) {
	availablePods = []string{}

	allPodsMap := make(map[string]bool)
	for i := 0; i < 30; i++ {
		pod := getTargetPodFromChatCompletion(t, fmt.Sprintf("Pod discovery request %d", i), "random")
		if pod != "" {
			allPodsMap[pod] = true
		}
	}

	for pod := range allPodsMap {
		availablePods = append(availablePods, pod)
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
	assert.NotEmpty(t, targetPod, "vtc-basic target pod is empty")
}

func TestVTCBasicRouting(t *testing.T) {
	setupVTCUsers(t)
	ensureSufficientPods(t, 2)

	t.Run("BasicFunctionality", func(t *testing.T) {
		time.Sleep(testCooldownPeriod)

		users := []string{"user1", "user2", "user3"}

		for _, user := range users {
			pod := getTargetPodFromChatCompletionWithUser(t, mediumMessage, "vtc-basic", user)
			assert.NotEmpty(t, pod, "VTC should route to a valid pod for user %s", user)
			assert.Contains(t, availablePods, pod, "Routed pod should be in available pods list")
		}
	})

	t.Run("TokenBasedRouting", func(t *testing.T) {
		time.Sleep(testCooldownPeriod)

		t.Log("Generating token disparity with large messages")

		for i := 0; i < 3; i++ {
			getTargetPodFromChatCompletionWithUser(t, smallMessage, "vtc-basic", "user4")
			getTargetPodFromChatCompletionWithUser(t, hugeMessage, "vtc-basic", "user5")
			time.Sleep(requestDelay)
		}

		t.Log("Testing routing with token disparity")

		user4Pods := make(map[string]int)
		user5Pods := make(map[string]int)

		for i := 0; i < 6; i++ {
			pod4 := getTargetPodFromChatCompletionWithUser(t, mediumMessage, "vtc-basic", "user4")
			pod5 := getTargetPodFromChatCompletionWithUser(t, mediumMessage, "vtc-basic", "user5")

			user4Pods[pod4]++
			user5Pods[pod5]++

			t.Logf("Request %d: user4->%s, user5->%s", i+1, pod4, pod5)
			time.Sleep(requestDelay)
		}

		t.Logf("User4 (low tokens) pod distribution: %v", user4Pods)
		t.Logf("User5 (high tokens) pod distribution: %v", user5Pods)

		assert.Greater(t, len(user4Pods), 0, "User4 should be routed to at least one pod")
		assert.Greater(t, len(user5Pods), 0, "User5 should be routed to at least one pod")
	})
}

func TestVTCHighUtilizationFairness(t *testing.T) {
	setupVTCUsers(t)
	time.Sleep(testCooldownPeriod)

	if len(availablePods) < 2 {
		t.Skip("Need at least 2 pods for high utilization test")
	}

	ctx := context.Background()

	t.Log("Setting all pods to high utilization")
	redisClient.Del(ctx, "pod_metrics")

	for _, pod := range availablePods {
		redisClient.HSet(ctx, "pod_metrics", pod, highLoadValue)
		t.Logf("Set high load (%s) for pod %s", highLoadValue, pod)
	}

	forceMetricsPropagation(t)

	users := []string{"user1", "user2", "user3"}

	t.Log("Creating token disparity with different message sizes")
	for i := 0; i < 2; i++ {
		getTargetPodFromChatCompletionWithUser(t, smallMessage, "vtc-basic", "user1")
		getTargetPodFromChatCompletionWithUser(t, mediumMessage, "vtc-basic", "user2")
		getTargetPodFromChatCompletionWithUser(t, largeMessage, "vtc-basic", "user3")
		time.Sleep(requestDelay)
	}

	userPodCounts := make(map[string]map[string]int)
	for _, user := range users {
		userPodCounts[user] = make(map[string]int)
	}

	testRequests := 12
	t.Log("Testing routing with high utilization on all pods")

	for i := 0; i < testRequests; i++ {
		for _, user := range users {
			pod := getTargetPodFromChatCompletionWithUser(t, mediumMessage, "vtc-basic", user)
			userPodCounts[user][pod]++
			time.Sleep(requestDelay)
		}
	}

	totalUsersWithConsistentRouting := 0
	totalUsersWithSinglePodDominance := 0

	for user, podCounts := range userPodCounts {
		t.Logf("User %s pod distribution: %v", user, podCounts)

		assert.Greater(t, len(podCounts), 0, "User %s should be routed to at least one pod", user)

		total := 0
		maxPodCount := 0
		for _, count := range podCounts {
			total += count
			if count > maxPodCount {
				maxPodCount = count
			}
		}
		assert.Equal(t, testRequests, total, "User %s should have exactly %d requests", user, testRequests)

		dominancePercentage := float64(maxPodCount) * 100 / float64(total)
		t.Logf("User %s: highest pod dominance %.1f%% (%d/%d requests)", user, dominancePercentage, maxPodCount, total)

		if dominancePercentage >= 60 {
			totalUsersWithSinglePodDominance++
			t.Logf("User %s shows strong pod preference (fairness component working)", user)
		}

		if len(podCounts) <= 2 || dominancePercentage >= 50 {
			totalUsersWithConsistentRouting++
		}
	}

	assert.Greater(t, totalUsersWithConsistentRouting, 0,
		"With equal high utilization, at least some users should show consistent routing patterns (fairness dominating)")

	assert.LessOrEqual(t, len(userPodCounts), len(availablePods)+1,
		"Users should not be distributed completely randomly across all pods")

	differentDistributions := 0
	userDistributions := make([]map[string]int, 0, len(users))
	for _, user := range users {
		userDistributions = append(userDistributions, userPodCounts[user])
	}

	for i := 0; i < len(userDistributions); i++ {
		for j := i + 1; j < len(userDistributions); j++ {
			if !mapsEqual(userDistributions[i], userDistributions[j]) {
				differentDistributions++
			}
		}
	}

	assert.Greater(t, differentDistributions, 0,
		"Users with different token histories should show different routing patterns when fairness dominates")

	t.Logf("Fairness test summary: %d/%d users show consistent routing, %d/%d users show single-pod dominance",
		totalUsersWithConsistentRouting, len(users), totalUsersWithSinglePodDominance, len(users))
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestVTCUtilizationBalancing(t *testing.T) {
	setupVTCUsers(t)
	time.Sleep(testCooldownPeriod)

	ensureSufficientPods(t, 3)

	ctx := context.Background()
	highLoadPod := availablePods[0]

	t.Log("Setting up controlled load imbalance")
	redisClient.Del(ctx, "pod_metrics")

	for _, pod := range availablePods {
		if pod == highLoadPod {
			redisClient.HSet(ctx, "pod_metrics", pod, "90")
			t.Logf("Set very high load (90) for pod %s", pod)
		} else {
			redisClient.HSet(ctx, "pod_metrics", pod, "5")
			t.Logf("Set very low load (5) for pod %s", pod)
		}
	}

	forceMetricsPropagation(t)

	metrics := getPodMetrics(t)
	t.Logf("Pod metrics after setup: %v", metrics)

	testUser := "user4"
	podDistribution := make(map[string]int)
	testRequestCount := 15

	t.Log("Testing utilization balancing with completely fresh user")
	for i := 0; i < testRequestCount; i++ {
		pod := getTargetPodFromChatCompletionWithUser(t, mediumMessage, "vtc-basic", testUser)
		podDistribution[pod]++
		t.Logf("Request %d routed to pod %s", i+1, pod)
		time.Sleep(requestDelay)
	}

	highLoadCount := podDistribution[highLoadPod]
	lowLoadTotal := testRequestCount - highLoadCount

	t.Logf("Distribution: %v", podDistribution)
	t.Logf("High-load pod received: %d/%d requests (%.1f%%)",
		highLoadCount, testRequestCount, float64(highLoadCount)*100/float64(testRequestCount))
	t.Logf("Low-load pods received: %d/%d requests (%.1f%%)",
		lowLoadTotal, testRequestCount, float64(lowLoadTotal)*100/float64(testRequestCount))

	assert.Less(t, highLoadCount, testRequestCount,
		"VTC should not send ALL requests to a single pod (got %d/%d to high-load pod)",
		highLoadCount, testRequestCount)

	assert.Greater(t, lowLoadTotal, 0,
		"Low-load pods should receive at least some requests (got %d)", lowLoadTotal)

	randomExpectationPerPod := float64(testRequestCount) / float64(len(availablePods))
	highLoadPercentage := float64(highLoadCount) * 100 / float64(testRequestCount)

	t.Logf("Analysis:")
	t.Logf("- Random distribution would give each pod %.1f requests (%.1f%%)",
		randomExpectationPerPod, 100.0/float64(len(availablePods)))
	t.Logf("- High-load pod got %.1f%% of requests", highLoadPercentage)

	if highLoadPercentage < 40 {
		t.Logf("Strong utilization effect: High-load pod got significantly less than average")
	} else if highLoadPercentage < 60 {
		t.Logf("Moderate utilization effect: Balanced between fairness and utilization")
	} else if highLoadPercentage < 90 {
		t.Logf("Fairness dominates: High token mapping overrides utilization penalty")
	} else {
		t.Logf("Minimal utilization effect: Consider adjusting weights if this is not desired")
	}

	t.Logf("Note: With fairnessWeight=1 and utilizationWeight=1, both fairness and utilization")
	t.Logf("influence routing decisions. High variance between runs is expected behavior.")
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
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(message),
		},
		Model: modelName,
	})
	require.NoError(t, err, "chat completions failed %v", err)
	assert.Equal(t, modelName, chatCompletion.Model)

	return dst.Header.Get("target-pod")
}

func convertToHistogram(podAssignments map[string]string) map[string]int {
	histogram := make(map[string]int)
	for _, pod := range podAssignments {
		histogram[pod]++
	}
	return histogram
}

func calculateDistributionStats(
	t *testing.T,
	phaseName string,
	histogram map[string]int,
) (float64, DistributionQuality) {
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
	cv := stddev / mean

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
	respOpt option.RequestOption) openai.Client {
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
	redisClient.Publish(context.Background(), "pod_metrics_refresh", "")
	time.Sleep(metricsWaitTime)
}

func keysFromMap(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func ensureTokensInWindow(t *testing.T, user string, message string, count int) {
	for i := 0; i < count; i++ {
		pod := getTargetPodFromChatCompletionWithUser(t, message, "vtc-basic", user)
		t.Logf("Token generation %d: User %s routed to pod %s", i+1, user, pod)

		if i < count-1 {
			time.Sleep(requestDelay)
		}
	}
}
