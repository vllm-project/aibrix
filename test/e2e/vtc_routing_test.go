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

// TestMain sets up and tears down the test environment
func TestMain(m *testing.M) {
	redisClient = utils.GetRedisClient()
	if redisClient == nil {
		fmt.Println("Failed to connect to Redis")
		os.Exit(1)
	}

	code := m.Run()

	// Clean up resources after all tests are done
	if redisClient != nil {
		redisClient.Close()
	}

	os.Exit(code)
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
		// Don't close Redis client here as it's shared across tests
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

	ensureSufficientPods(t, 3)

	// Sub-test 1: Verify that users accumulate tokens based on message size
	t.Run("TokenAccumulation", func(t *testing.T) {
		// Reset user token history and set high TPM limits
		ctx := context.Background()
		for _, user := range users {
			// Reset user with high TPM limit
			utils.DelUser(ctx, utils.User{Name: user}, redisClient)
			utils.SetUser(ctx, utils.User{Name: user, Rpm: 1000, Tpm: 100000}, redisClient)

			var delErr error
			_, delErr = redisClient.Del(ctx, fmt.Sprintf("user_tokens:%s", user)).Result()
			if delErr != nil {
				t.Logf("Warning: Failed to delete token history for %s: %v", user, delErr)
			}

			var setErr error
			key := fmt.Sprintf("rate_limit:tpm:%s", user)
			_, setErr = redisClient.Set(ctx, key, "100000", 10*time.Minute).Result()
			if setErr != nil {
				t.Logf("Warning: Failed to set TPM limit for %s: %v", user, setErr)
			} else {
				t.Logf("Increased TPM limit for user %s to 100,000", user)
			}
			t.Logf("Starting with clean state for user %s", user)
		}

		// Create messages with different token counts
		// With AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MIN_TOKENS=100 and MAX_TOKENS=800,
		// we need to create messages that will show the difference
		veryShortMsg := "Hi." // ~1 token
		mediumTokenMsg := "This is a medium-sized message with about 15 tokens or so."
		highTokenMsg := strings.Repeat("This is a longer message with more tokens. ", 8) // ~120 tokens

		msgMap := map[string]string{
			users[0]: veryShortMsg,   // Low token user
			users[1]: mediumTokenMsg, // Medium token user
			users[2]: highTokenMsg,   // High token user
		}

		// Track which pods each user gets routed to
		userPods := make(map[string]map[string]int)
		for _, user := range users {
			userPods[user] = make(map[string]int)
		}

		podHistogram := make(map[string]int)

		// Make more requests to create significant token disparity
		// With WINDOW_SIZE=2 seconds, we need to make requests quickly
		for i := 0; i < 10; i++ {
			for _, user := range users {
				pod := getTargetPodFromChatCompletionWithUser(t, msgMap[user], "vtc-basic", user)
				podHistogram[pod]++
				userPods[user][pod]++
				t.Logf("Request %d: User %s with message size %d routed to pod %s",
					i+1, user, len(msgMap[user]), pod)
				forceMetricsPropagation(t)
			}
		}

		t.Log("Token accumulation results:")
		for user, pods := range userPods {
			t.Logf("User %s pod distribution:", user)
			for pod, count := range pods {
				t.Logf("  Pod %s: %d requests (%.1f%%)", pod, count, float64(count)*10.0)
			}
		}

		cv, quality := calculateDistributionStats(t, "Token Accumulation", podHistogram)
		t.Logf("Distribution quality: %s (CV=%.2f)", quality, cv)

		// Check if high token user uses more pods than low token user
		highTokenUserPods := len(userPods[users[2]])
		lowTokenUserPods := len(userPods[users[0]])

		assert.Less(t, quality, ExcellentDistribution,
			"Token accumulation should show some imbalance (not EXCELLENT), got %s distribution with CV=%.2f",
			quality, cv)

		assert.GreaterOrEqual(t, highTokenUserPods, lowTokenUserPods,
			"High token user (%s) should use at least as many pods (%d) as low token user (%s, %d pods)",
			users[2], highTokenUserPods, users[0], lowTokenUserPods)
	})

	// Sub-test 2: Verify that the algorithm considers token counts
	t.Run("FairnessComponent", func(t *testing.T) {
		t.Log("Clearing existing token history and increasing TPM limits")
		ctx := context.Background()
		for _, user := range users {
			utils.DelUser(ctx, utils.User{Name: user}, redisClient)
			utils.SetUser(ctx, utils.User{Name: user, Rpm: 1000, Tpm: 100000}, redisClient)

			var delErr error
			_, delErr = redisClient.Del(ctx, fmt.Sprintf("user_tokens:%s", user)).Result()
			if delErr != nil {
				t.Logf("Warning: Failed to delete token history for %s: %v", user, delErr)
			}

			var setErr error
			key := fmt.Sprintf("rate_limit:tpm:%s", user)
			_, setErr = redisClient.Set(ctx, key, "100000", 10*time.Minute).Result()
			if setErr != nil {
				t.Logf("Warning: Failed to set TPM limit for %s: %v", user, setErr)
			} else {
				t.Logf("Increased TPM limit for user %s to 100,000", user)
			}
		}

		t.Log("Generating token history with disparity")

		shortMsg := "Small message."                                                       // ~2 tokens
		mediumMsg := "This is a medium-sized message with more tokens than the small one." // ~15 tokens
		extremeMsg := strings.Repeat("This is a longer message with more tokens. ", 10)    // ~150 tokens

		userMsgs := map[string]string{
			"user1": shortMsg,
			"user2": mediumMsg,
			"user3": extremeMsg,
		}

		var err error
		ctxBg := context.Background()
		_, err = redisClient.Del(ctxBg, "pod_metrics").Result()
		if err != nil {
			t.Logf("Warning: Failed to delete pod_metrics: %v", err)
		}
		forceMetricsPropagation(t)

		t.Log("Balancing pod loads before token history generation")
		getAvailablePods(t) // This populates the global availablePods slice
		for _, pod := range availablePods {
			_, err = redisClient.HSet(ctxBg, "pod_metrics", pod, "10").Result() // Set all pods to same load
			if err != nil {
				t.Logf("Warning: Failed to set pod metrics for %s: %v", pod, err)
			}
		}
		forceMetricsPropagation(t)

		t.Log("Generating token history with extreme disparity")

		t.Log("Generating history for user1 (low token user)")
		for i := 0; i < 5; i++ {
			pod := getTargetPodFromChatCompletionWithUser(t, userMsgs["user1"], "vtc-basic", "user1")
			t.Logf("History generation: User user1 with message size %d routed to pod %s",
				len(userMsgs["user1"]), pod)
			forceMetricsPropagation(t)
			time.Sleep(100 * time.Millisecond)
		}

		t.Log("Generating history for user2 (medium token user)")
		for i := 0; i < 5; i++ {
			pod := getTargetPodFromChatCompletionWithUser(t, userMsgs["user2"], "vtc-basic", "user2")
			t.Logf("History generation: User user2 with message size %d routed to pod %s",
				len(userMsgs["user2"]), pod)
			forceMetricsPropagation(t)
			time.Sleep(100 * time.Millisecond)
		}

		t.Log("Generating history for user3 (very high token user)")
		for i := 0; i < 15; i++ { // More requests for high token user
			pod := getTargetPodFromChatCompletionWithUser(t, userMsgs["user3"], "vtc-basic", "user3")
			t.Logf("History generation: User user3 with message size %d routed to pod %s",
				len(userMsgs["user3"]), pod)
			forceMetricsPropagation(t)
			time.Sleep(100 * time.Millisecond)
		}

		t.Log("Waiting for token history to be fully processed")
		time.Sleep(500 * time.Millisecond)

		t.Log("Testing fairness with varying message sizes after history generation")

		// Make multiple requests per user to better demonstrate routing patterns
		podAssignments := make(map[string]string)
		podHistogram := make(map[string]int)
		userRoutingMap := make(map[string]map[string]int) // Track pod assignments per user

		for _, user := range users {
			userRoutingMap[user] = make(map[string]int)
		}

		// Make 5 requests per user (15 total) to better observe routing patterns
		for i := 0; i < 5; i++ {
			pod1 := getTargetPodFromChatCompletionWithUser(t, userMsgs["user1"], "vtc-basic", "user1")
			podAssignments["user1"+strconv.Itoa(i)] = pod1
			podHistogram[pod1]++
			userRoutingMap["user1"][pod1]++
			t.Logf("Test request %d: User user1 (low tokens) routed to pod %s", i+1, pod1)
			forceMetricsPropagation(t)
			time.Sleep(50 * time.Millisecond)

			pod2 := getTargetPodFromChatCompletionWithUser(t, userMsgs["user2"], "vtc-basic", "user2")
			podAssignments["user2"+strconv.Itoa(i)] = pod2
			podHistogram[pod2]++
			userRoutingMap["user2"][pod2]++
			t.Logf("Test request %d: User user2 (medium tokens) routed to pod %s", i+1, pod2)
			forceMetricsPropagation(t)
			time.Sleep(50 * time.Millisecond)

			pod3 := getTargetPodFromChatCompletionWithUser(t, userMsgs["user3"], "vtc-basic", "user3")
			podAssignments["user3"+strconv.Itoa(i)] = pod3
			podHistogram[pod3]++
			userRoutingMap["user3"][pod3]++
			t.Logf("Test request %d: User user3 (high tokens) routed to pod %s", i+1, pod3)
			forceMetricsPropagation(t)
			time.Sleep(50 * time.Millisecond)

			time.Sleep(100 * time.Millisecond)
		}

		t.Log("Overall pod distribution:")
		for pod, count := range podHistogram {
			t.Logf("Pod %s received %d requests (%.1f%%)",
				pod, count, float64(count)*100/float64(len(podAssignments)))
		}

		t.Log("Checking user consistency and token-based routing:")

		for user, podCounts := range userRoutingMap {
			totalRequests := 0
			for _, count := range podCounts {
				totalRequests += count
			}

			for pod, count := range podCounts {
				percentage := float64(count) / float64(totalRequests) * 100.0
				t.Logf("User %s routed to pod %s for %.1f%% of requests", user, pod, percentage)
			}

			// Different expectations based on token usage:
			// - Low/medium token users (user1, user2) should be routed consistently to one pod
			// - High token user (user3) might be routed to multiple pods due to fairness component
			if user == "user1" || user == "user2" {
				// For low/medium token users, expect consistent routing (one dominant pod)
				dominantPod := ""
				highestCount := 0
				for pod, count := range podCounts {
					if count > highestCount {
						highestCount = count
						dominantPod = pod
					}
				}

				dominantPercentage := float64(highestCount) / float64(totalRequests) * 100.0
				t.Logf("User %s has dominant pod %s (%.1f%%)", user, dominantPod, dominantPercentage)

				// Low token users should be routed consistently
				assert.GreaterOrEqual(t, dominantPercentage, 80.0,
					"Low/medium token user %s should be routed consistently to one pod", user)
			}
		}

		// Count distinct pods used for each user type
		userPods := make(map[string][]string)
		for user, podCounts := range userRoutingMap {
			for pod := range podCounts {
				userPods[user] = append(userPods[user], pod)
			}
		}

		// Check if we have at least 2 distinct pods used across all users
		allPods := make(map[string]bool)
		for _, pods := range userPods {
			for _, pod := range pods {
				allPods[pod] = true
			}
		}

		t.Log("Distinct pods used by each user:")
		for user, pods := range userPods {
			t.Logf("User %s used %d distinct pods: %v", user, len(pods), pods)
		}

		podHistogramStr := make(map[string]string)
		for pod, count := range podHistogram {
			podHistogramStr[pod] = strconv.Itoa(count)
		}

		cv, quality := calculateDistributionStats(t, "Fairness Test", convertToHistogram(podHistogramStr))

		distinctPodCount := len(allPods)
		t.Logf("Total distinct pods used across all users: %d", distinctPodCount)

		assert.GreaterOrEqual(t, distinctPodCount, 2,
			"Expected at least 2 distinct pods across all users to validate token-based routing")

		user1DominantPod := ""
		user1HighestCount := 0
		for pod, count := range userRoutingMap["user1"] {
			if count > user1HighestCount {
				user1HighestCount = count
				user1DominantPod = pod
			}
		}

		user3PodCount := len(userRoutingMap["user3"])
		t.Logf("Low token user (user1) dominant pod: %s (%.1f%%)",
			user1DominantPod, float64(user1HighestCount)*100/5.0)
		t.Logf("High token user (user3) is using %d distinct pods", user3PodCount)

		t.Logf("VTC algorithm is using %d distinct pods with CV=%.2f (%s distribution)",
			len(podHistogram), cv, quality)

		if distinctPodCount >= 2 {
			assert.GreaterOrEqual(t, user3PodCount, 2,
				"High token user (user3) should be routed to multiple pods due to fairness component")

			assert.Equal(t, len(userRoutingMap["user1"]), 1,
				"Low token user (user1) should be routed consistently to a single pod")
		} else {
			t.Log("Only one pod is available, can't fully test fairness component")
		}
		assert.GreaterOrEqual(t, quality, FairDistribution,
			"Distribution should be at least FAIR with CV=%.2f", cv)
	})
}

func TestVTCHighUtilizationFairness(t *testing.T) {
	// This test verifies that when all pods have high utilization,
	// the fairness component still works correctly
	setupVTCUsers(t)
	defer cleanupVTCUsers(t)

	ctx := context.Background()
	users := []string{"user1", "user2", "user3"}
	for _, user := range users {
		utils.DelUser(ctx, utils.User{Name: user}, redisClient)
		utils.SetUser(ctx, utils.User{Name: user, Rpm: 1000, Tpm: 100000}, redisClient)

		var delErr error
		_, delErr = redisClient.Del(ctx, fmt.Sprintf("user_tokens:%s", user)).Result()
		if delErr != nil {
			t.Logf("Warning: Failed to delete token history for %s: %v", user, delErr)
		}

		var setErr error
		key := fmt.Sprintf("rate_limit:tpm:%s", user)
		_, setErr = redisClient.Set(ctx, key, "100000", 10*time.Minute).Result()
		if setErr != nil {
			t.Logf("Warning: Failed to set TPM limit for %s: %v", user, setErr)
		} else {
			t.Logf("Increased TPM limit for user %s to 100,000", user)
		}
	}

	t.Logf("Waiting for token window expiry to ensure clean state")
	time.Sleep(3 * time.Second)

	t.Logf("Making pruning-trigger requests for all test users")
	for _, user := range users {
		pod := getTargetPodFromChatCompletionWithUser(t, "Pruning trigger", "vtc-basic", user)
		t.Logf("User %s routed to pod %s (pruning trigger)", user, pod)
	}

	ensureSufficientPods(t, 3)

	metrics := getPodMetrics(t)
	t.Logf("Pod metrics before controlled setup: %v", metrics)

	t.Logf("Setting ALL pods to high utilization")
	redisClient.Del(ctx, "pod_metrics")

	highLoadValue := "90" // High load for all pods
	for _, pod := range availablePods {
		redisClient.HSet(ctx, "pod_metrics", pod, highLoadValue)
		t.Logf("Set high load (%s) for pod %s", highLoadValue, pod)
	}

	forceMetricsPropagation(t)
	metrics = getPodMetrics(t)
	t.Logf("Pod metrics after controlled setup: %v", metrics)
	ensureSufficientPods(t, 3)

	veryShortMsg := "Hi."
	mediumTokenMsg := "This is a medium-sized message with about 15 tokens or so."
	highTokenMsg := strings.Repeat("This is a longer message with more tokens. ", 8)

	msgMap := map[string]string{
		"user1": veryShortMsg,   // Low token user
		"user2": mediumTokenMsg, // Medium token user
		"user3": highTokenMsg,   // High token user
	}

	t.Logf("Generating token history with different message sizes")
	for i := 0; i < 10; i++ {
		for user, msg := range msgMap {
			getTargetPodFromChatCompletionWithUser(t, msg, "vtc-basic", user)
			forceMetricsPropagation(t)
		}
	}

	userPods := make(map[string]map[string]int)
	for _, user := range users {
		userPods[user] = make(map[string]int)
	}

	testRequestCount := 15
	t.Logf("Testing routing with high utilization across all pods")
	for i := 0; i < testRequestCount; i++ {
		for user, msg := range msgMap {
			pod := getTargetPodFromChatCompletionWithUser(t, msg, "vtc-basic", user)
			userPods[user][pod]++
			t.Logf("Test request %d: User %s with message size %d routed to pod %s",
				i+1, user, len(msg), pod)
			forceMetricsPropagation(t)
		}
	}

	t.Log("User pod distribution with high utilization across all pods:")
	for user, pods := range userPods {
		t.Logf("User %s pod distribution:", user)
		for pod, count := range pods {
			t.Logf("  Pod %s: %d requests (%.1f%%)", pod, count,
				float64(count)/float64(testRequestCount)*100)
		}
	}

	distinctPodsPerUser := make(map[string]int)
	for user, pods := range userPods {
		distinctPodsPerUser[user] = len(pods)
		t.Logf("User %s used %d distinct pods: %v", user, len(pods),
			keysFromMap(pods))
	}

	highTokenUserPods := distinctPodsPerUser["user3"]
	lowTokenUserPods := distinctPodsPerUser["user1"]

	assert.Greater(t, highTokenUserPods, lowTokenUserPods,
		"With high utilization across all pods, high token user (user3) should use more pods (%d) than low token user (user1, %d pods)",
		highTokenUserPods, lowTokenUserPods)

	assert.LessOrEqual(t, lowTokenUserPods, 2,
		"Low token user should be routed consistently to 1-2 pods, got %d pods",
		lowTokenUserPods)
}

func keysFromMap(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestVTCUtilizationBalancing(t *testing.T) {
	setupVTCUsers(t)
	defer cleanupVTCUsers(t)

	ctx := context.Background()
	for _, user := range testUsers {
		utils.DelUser(ctx, utils.User{Name: user.Name}, redisClient)
		utils.SetUser(ctx, utils.User{Name: user.Name, Rpm: 1000, Tpm: 100000}, redisClient)

		var delErr error
		_, delErr = redisClient.Del(ctx, fmt.Sprintf("user_tokens:%s", user.Name)).Result()
		if delErr != nil {
			t.Logf("Warning: Failed to delete token history for %s: %v", user.Name, delErr)
		}

		var setErr error
		key := fmt.Sprintf("rate_limit:tpm:%s", user.Name)
		_, setErr = redisClient.Set(ctx, key, "100000", 10*time.Minute).Result()
		if setErr != nil {
			t.Logf("Warning: Failed to set TPM limit for %s: %v", user.Name, setErr)
		} else {
			t.Logf("Increased TPM limit for user %s to 100,000", user.Name)
		}
	}

	t.Logf("Waiting for token window expiry to ensure clean state")
	// With AIBRIX_ROUTER_VTC_TOKEN_TRACKER_WINDOW_SIZE=2 and TIME_UNIT=seconds
	// We need to wait at least 2 seconds
	time.Sleep(3 * time.Second)

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

	redisClient.Del(ctx, "pod_metrics")

	highLoadValue := "100"  // Max load
	normalLoadValue := "10" // Low load

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

	testRequestCount := 20
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

	highLoadCount := podDistribution[highLoadPod]
	maxAllowed := int(float64(testRequestCount) * 0.4) // 40% threshold
	t.Logf("High-load pod received %d/%d requests (max allowed: %d)",
		highLoadCount, testRequestCount, maxAllowed)

	cv, quality := calculateDistributionStats(t, "Utilization Test", podDistribution)

	assert.LessOrEqual(t, highLoadCount, maxAllowed,
		"High-load pod should receive at most %d/%d requests due to utilization balancing",
		maxAllowed, testRequestCount)

	// With AIBRIX_ROUTER_VTC_BASIC_FAIRNESS_WEIGHT=1 (default), we expect more uneven distribution
	// as the algorithm balances between fairness and utilization
	t.Logf("Distribution quality: %s (CV=%.2f)", quality, cv)

	// Assert that low load pods get more requests than high load pod
	lowLoadTotal := 0
	for pod, count := range podDistribution {
		if pod != highLoadPod {
			lowLoadTotal += count
		}
	}

	assert.Greater(t, lowLoadTotal, highLoadCount,
		"Low-load pods combined (%d) should receive more requests than high-load pod (%d)",
		lowLoadTotal, highLoadCount)
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
