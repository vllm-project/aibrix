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
	tokenWindowDuration = 3 * time.Second
	requestDelay        = 50 * time.Millisecond
	metricsWaitTime     = 100 * time.Millisecond
	highLoadValue       = "100"
	normalLoadValue     = "5"
	utilTolerance       = 3
	veryShortMessage    = "Hi."
	mediumTokenMessage  = "This is a medium-sized message with about 15 tokens or so."
	user1Name           = "user1"
	user3Name           = "user3"
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

	if redisClient != nil {
		if err := redisClient.Close(); err != nil {
			fmt.Printf("Warning: Failed to close Redis client: %v\n", err)
		}
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

	ctx := context.Background()

	// First delete any existing users to ensure clean state
	for _, user := range testUsers {
		if err := utils.DelUser(ctx, user, redisClient); err != nil {
			t.Logf("Warning: Failed to delete existing user %s: %v", user.Name, err)
		}

		// Clean up any token history
		redisClient.Del(ctx, fmt.Sprintf("user_tokens:%s", user.Name))
	}

	// Then create fresh users
	for _, user := range testUsers {
		if err := utils.SetUser(ctx, user, redisClient); err != nil {
			t.Fatalf("Failed to set up user %s: %v", user.Name, err)
		}
		t.Logf("Created test user: %s", user.Name)
	}

	// Wait a bit for user setup to propagate
	time.Sleep(100 * time.Millisecond)

	getAvailablePods(t)

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
	assert.NotEmpty(t, targetPod, "vtc-basic target pod is empty") //what is this
}

func TestVTCTokenAccumulation(t *testing.T) {
	setupVTCUsers(t)

	users := []string{"user1", "user2", "user3"}
	ensureSufficientPods(t, 3)

	// Reset user token history and set high TPM limits
	setupTestUsersWithHighTPM(t, users)

	// Create messages with different token counts
	// With AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MIN_TOKENS=100 and MAX_TOKENS=800,
	// we need to create messages that will show the difference
	veryShortMsg := "Hi." // ~1 token
	mediumTokenMsg := "This is a medium-sized message with about 15 tokens or so."
	highTokenMsg := strings.Repeat("This is a longer message with more tokens. ", 8) // ~120 tokens

	msgMap := map[string]string{
		users[0]: veryShortMsg,
		users[1]: mediumTokenMsg,
		users[2]: highTokenMsg,
	}

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
}

func setupTestUsersWithHighTPM(t *testing.T, users []string) {
	ctx := context.Background()
	for _, user := range users {
		if err := utils.DelUser(ctx, utils.User{Name: user}, redisClient); err != nil {
			t.Logf("Warning: Failed to delete user %s: %v", user, err)
		}
		if err := utils.SetUser(ctx, utils.User{Name: user, Rpm: 1000, Tpm: 100000}, redisClient); err != nil {
			t.Logf("Warning: Failed to set user %s: %v", user, err)
		}

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
}

// Helper function to generate token history
func generateTokenHistory(t *testing.T, userMsgs map[string]string) {
	t.Log("Generating token history with extreme disparity")

	for user, msg := range userMsgs {
		requestCount := 5
		if user == "user3" {
			requestCount = 15 // More requests for high token user
		}

		t.Logf("Generating history for %s (%s token user)", user, getTokenUserType(user))
		for i := 0; i < requestCount; i++ {
			pod := getTargetPodFromChatCompletionWithUser(t, msg, "vtc-basic", user)
			t.Logf("History generation: User %s with message size %d routed to pod %s",
				user, len(msg), pod)
			forceMetricsPropagation(t)
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// Helper function to get token user type
func getTokenUserType(user string) string {
	switch user {
	case user1Name:
		return "low"
	case "user2":
		return "medium"
	case user3Name:
		return "high"
	default:
		return "unknown"
	}
}

// Helper function to make test requests
func makeTestRequests(t *testing.T, userMsgs map[string]string, users []string) (
	map[string]string,
	map[string]int,
	map[string]map[string]int,
) {
	podAssignments := make(map[string]string)
	podHistogram := make(map[string]int)
	userRoutingMap := make(map[string]map[string]int)

	for _, user := range users {
		userRoutingMap[user] = make(map[string]int)
	}

	for i := 0; i < 5; i++ {
		for _, user := range users {
			pod := getTargetPodFromChatCompletionWithUser(t, userMsgs[user], "vtc-basic", user)
			podAssignments[user+strconv.Itoa(i)] = pod
			podHistogram[pod]++
			userRoutingMap[user][pod]++
			t.Logf("Test request %d: User %s (%s tokens) routed to pod %s",
				i+1, user, getTokenUserType(user), pod)
			forceMetricsPropagation(t)
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
	}

	return podAssignments, podHistogram, userRoutingMap
}

// Helper function to analyze routing patterns
func analyzeRoutingPatterns(t *testing.T, userRoutingMap map[string]map[string]int) {
	for user, podCounts := range userRoutingMap {
		totalRequests := 0
		for _, count := range podCounts {
			totalRequests += count
		}

		for pod, count := range podCounts {
			percentage := float64(count) / float64(totalRequests) * 100.0
			t.Logf("User %s routed to pod %s for %.1f%% of requests", user, pod, percentage)
		}

		if user == "user1" || user == "user2" {
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

			assert.GreaterOrEqual(t, dominantPercentage, 80.0,
				"Low/medium token user %s should be routed consistently to one pod", user)
		}
	}
}

func TestVTCFairnessComponent(t *testing.T) {
	setupVTCUsers(t)
	defer cleanupVTCUsers(t)

	ctx := context.Background()
	t.Log("Explicitly resetting user states for clean test environment")
	redisClient.Del(ctx, user1Name+":tokens")
	redisClient.Del(ctx, "user2:tokens")
	redisClient.Del(ctx, user3Name+":tokens")
	redisClient.Del(ctx, "pod_metrics")
	time.Sleep(500 * time.Millisecond)

	ensureSufficientPods(t, 3)

	t.Logf("Clearing existing token history and increasing TPM limits")
	for _, user := range testUsers {
		setupTestUsersWithHighTPM(t, []string{user.Name})
	}

	// Create a controlled pod load environment with significant differences
	t.Logf("Setting up controlled pod load environment")
	getAvailablePods(t)

	if len(availablePods) < 2 {
		t.Skipf("Need at least 2 pods to test fairness component, found only %d", len(availablePods))
		return
	}

	redisClient.Del(ctx, "pod_metrics")
	time.Sleep(200 * time.Millisecond)

	for i, pod := range availablePods {
		var loadValue string
		if i == 0 {
			loadValue = "10"
			t.Logf("Set low load (%s) for pod %s", loadValue, pod)
		} else if i == 1 {
			loadValue = "20"
			t.Logf("Set medium load (%s) for pod %s", loadValue, pod)
		} else {
			loadValue = "30"
			t.Logf("Set high load (%s) for pod %s", loadValue, pod)
		}
		redisClient.HSet(ctx, "pod_metrics", pod, loadValue)
	}

	forceMetricsPropagation(t)

	metrics := getPodMetrics(t)
	t.Logf("Pod metrics after controlled setup: %v", metrics)
	if len(metrics) == 0 {
		t.Log("Warning: No pod metrics found, test may not behave as expected. Run test again.")
	}

	t.Logf("Generating token history with extreme disparity")

	veryShortMsg := veryShortMessage
	mediumTokenMsg := mediumTokenMessage
	highTokenMsg := strings.Repeat("This is a longer message with more tokens. ", 10)
	t.Logf("Generating history for user1 (low token user)")
	for i := 0; i < 8; i++ {
		pod := getTargetPodFromChatCompletionWithUser(t, veryShortMsg, "vtc-basic", "user1")
		t.Logf("History generation: User user1 with message size %d routed to pod %s",
			len(veryShortMsg), pod)
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("Generating history for user2 (medium token user)")
	for i := 0; i < 8; i++ {
		pod := getTargetPodFromChatCompletionWithUser(t, mediumTokenMsg, "vtc-basic", "user2")
		t.Logf("History generation: User user2 with message size %d routed to pod %s",
			len(mediumTokenMsg), pod)
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("Generating history for user3 (high token user)")
	for i := 0; i < 20; i++ {
		pod := getTargetPodFromChatCompletionWithUser(t, highTokenMsg, "vtc-basic", "user3")
		t.Logf("History generation: User user3 with message size %d routed to pod %s",
			len(highTokenMsg), pod)
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("Waiting for token history to be fully processed")
	time.Sleep(2 * time.Second)

	for _, user := range []string{"user1", "user2", "user3"} {
		exists, err := redisClient.Exists(ctx, fmt.Sprintf("%s:tokens", user)).Result()
		if err != nil || exists == 0 {
			t.Logf("Warning: No token history found for %s, test may not behave as expected. Run test again.", user)
		}
	}

	t.Logf("Testing fairness with varying message sizes after history generation")
	userMsgs := map[string]string{
		"user1": veryShortMsg,
		"user2": mediumTokenMsg,
		"user3": highTokenMsg,
	}

	userPods := make(map[string]map[string]int)
	for _, user := range []string{"user1", "user2", "user3"} {
		userPods[user] = make(map[string]int)
	}

	testRequestCount := 10
	for i := 0; i < testRequestCount; i++ {
		for user, msg := range userMsgs {
			pod := getTargetPodFromChatCompletionWithUser(t, msg, "vtc-basic", user)
			userPods[user][pod]++
			t.Logf("Test request %d: User %s (%s tokens) routed to pod %s",
				i+1, user, getTokenUserType(user), pod)
			time.Sleep(50 * time.Millisecond)
		}
	}

	t.Log("Overall pod distribution:")
	overallPodDistribution := make(map[string]int)
	for _, pods := range userPods {
		for pod, count := range pods {
			overallPodDistribution[pod] += count
		}
	}
	for pod, count := range overallPodDistribution {
		t.Logf("Pod %s received %d requests (%.1f%%)", pod, count,
			float64(count)/float64(testRequestCount*3)*100)
	}

	t.Log("Checking user consistency and token-based routing:")
	analyzeRoutingPatterns(t, userPods)

	t.Log("Distinct pods used by each user:")
	distinctPodsPerUser := make(map[string]int)
	allDistinctPods := make(map[string]bool)
	for user, pods := range userPods {
		distinctPodsPerUser[user] = len(pods)
		t.Logf("User %s used %d distinct pods: %v", user, len(pods),
			keysFromMap(pods))
		for pod := range pods {
			allDistinctPods[pod] = true
		}
	}

	cv, quality := calculateDistributionStats(t, "Fairness Test", overallPodDistribution)

	totalDistinctPods := len(allDistinctPods)
	t.Logf("Total distinct pods used across all users: %d", totalDistinctPods)

	if totalDistinctPods < 2 {
		t.Logf("Warning: Only %d distinct pods used across all users. "+
			"This may indicate an issue with pod availability or the routing algorithm.",
			totalDistinctPods)
		t.Logf("Available pods: %v", availablePods)
		t.Logf("Pod metrics: %v", metrics)

		if len(availablePods) >= 2 {
			assert.GreaterOrEqual(t, totalDistinctPods, 2,
				"Expected at least 2 distinct pods across all users to validate token-based routing")
		} else {
			t.Skipf("Only %d pods available, can't fully test fairness component", len(availablePods))
			return
		}
	}

	// Verify that high token users use more pods than low token users
	lowTokenUser := "user1"
	highTokenUser := "user3"

	// Find the dominant pod for the low token user
	lowTokenUserDominantPod := ""
	maxCount := 0
	for pod, count := range userPods[lowTokenUser] {
		if count > maxCount {
			maxCount = count
			lowTokenUserDominantPod = pod
		}
	}

	if lowTokenUserDominantPod != "" {
		t.Logf("Low token user (%s) dominant pod: %s (%.1f%%)", lowTokenUser, lowTokenUserDominantPod,
			float64(maxCount)/float64(testRequestCount)*100)
	} else {
		t.Logf("Low token user (%s) has no dominant pod", lowTokenUser)
	}

	t.Logf("High token user (%s) is using %d distinct pods", highTokenUser, distinctPodsPerUser[highTokenUser])

	t.Logf("VTC algorithm is using %d distinct pods with CV=%.2f (%s distribution)", totalDistinctPods, cv, quality)

	if totalDistinctPods >= 2 {
		assert.GreaterOrEqual(t, distinctPodsPerUser[highTokenUser], distinctPodsPerUser[lowTokenUser],
			"High token user (%s) should use at least as many pods (%d) as low token user (%s, %d pods)",
			highTokenUser, distinctPodsPerUser[highTokenUser],
			lowTokenUser, distinctPodsPerUser[lowTokenUser])
	}
}

func TestVTCHighUtilizationFairness(t *testing.T) {
	setupVTCUsers(t)
	defer cleanupVTCUsers(t)

	ctx := context.Background()
	t.Log("Explicitly resetting user states for clean test environment")
	redisClient.Del(ctx, user1Name+":tokens")
	redisClient.Del(ctx, "user2:tokens")
	redisClient.Del(ctx, user3Name+":tokens")
	redisClient.Del(ctx, "pod_metrics")
	time.Sleep(500 * time.Millisecond)

	users := []string{user1Name, "user2", user3Name}
	setupTestUsersWithHighTPM(t, users)

	t.Logf("Waiting for token window expiry to ensure clean state")
	time.Sleep(3 * time.Second)

	t.Logf("Making pruning-trigger requests for all test users")
	for _, user := range users {
		pod := getTargetPodFromChatCompletionWithUser(t, "Pruning trigger", "vtc-basic", user)
		t.Logf("User %s routed to pod %s (pruning trigger)", user, pod)
		time.Sleep(100 * time.Millisecond) // Small delay between requests for stability
	}

	ensureSufficientPods(t, 3)

	metrics := getPodMetrics(t)
	t.Logf("Pod metrics before controlled setup: %v", metrics)

	t.Logf("Setting ALL pods to high utilization")
	redisClient.Del(ctx, "pod_metrics")
	time.Sleep(200 * time.Millisecond) // Wait after deletion for stability

	highLoadValue := "90" // High load for all pods
	for _, pod := range availablePods {
		redisClient.HSet(ctx, "pod_metrics", pod, highLoadValue)
		t.Logf("Set high load (%s) for pod %s", highLoadValue, pod)
	}

	forceMetricsPropagation(t)
	metrics = getPodMetrics(t)
	t.Logf("Pod metrics after controlled setup: %v", metrics)

	// Verify all pods have the expected load
	for _, pod := range availablePods {
		podLoad, exists := metrics[pod]
		if !exists || podLoad != 90 {
			t.Logf("Warning: Pod %s has unexpected load. Expected 90, got %d",
				pod, podLoad)
		}
	}

	ensureSufficientPods(t, 3)

	veryShortMsg := "Hi."
	mediumTokenMsg := "This is a medium-sized message with about 15 tokens or so."
	highTokenMsg := strings.Repeat("This is a longer message with more tokens. ", 8)

	msgMap := map[string]string{
		"user1": veryShortMsg,
		"user2": mediumTokenMsg,
		"user3": highTokenMsg,
	}

	t.Logf("Generating token history with different message sizes")
	for i := 0; i < 10; i++ {
		for user, msg := range msgMap {
			getTargetPodFromChatCompletionWithUser(t, msg, "vtc-basic", user)
			forceMetricsPropagation(t)
			time.Sleep(50 * time.Millisecond) // Add small delay for stability
		}
	}

	userPods := make(map[string]map[string]int)
	for _, user := range users {
		userPods[user] = make(map[string]int)
	}

	// Increase test request count for better statistical significance
	testRequestCount := 25
	t.Logf("Testing routing with high utilization across all pods (making %d requests per user)", testRequestCount)
	for i := 0; i < testRequestCount; i++ {
		for user, msg := range msgMap {
			pod := getTargetPodFromChatCompletionWithUser(t, msg, "vtc-basic", user)
			userPods[user][pod]++
			t.Logf("Test request %d: User %s with message size %d routed to pod %s",
				i+1, user, len(msg), pod)
			forceMetricsPropagation(t)
			time.Sleep(50 * time.Millisecond) // Add delay to ensure stability
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
	podUsagePercentage := make(map[string]float64)

	for user, pods := range userPods {
		distinctPodsPerUser[user] = len(pods)

		// Calculate percentage of available pods used
		podUsagePercentage[user] = float64(len(pods)) / float64(len(availablePods)) * 100.0

		t.Logf("User %s used %d distinct pods (%.1f%% of available pods): %v",
			user, len(pods), podUsagePercentage[user], keysFromMap(pods))
	}

	highTokenUserPods := distinctPodsPerUser[user3Name]
	lowTokenUserPods := distinctPodsPerUser[user1Name]

	t.Logf("High token user (user3) used %d pods, low token user (user1) used %d pods",
		highTokenUserPods, lowTokenUserPods)

	assert.GreaterOrEqual(t, highTokenUserPods, lowTokenUserPods,
		"With high utilization across all pods, high token user (user3) should use at least as many pods "+
			"(%d) as low token user (user1, %d pods)",
		highTokenUserPods, lowTokenUserPods)

	// In a 3-pod setup, low token users should typically use 1-2 pods
	maxLowTokenPods := 2
	if len(availablePods) <= 2 {
		maxLowTokenPods = 1 // Adjust expectation for smaller pod count
	}

	assert.LessOrEqual(t, lowTokenUserPods, maxLowTokenPods,
		"Low token user should be routed consistently to at most %d pods, got %d pods",
		maxLowTokenPods, lowTokenUserPods)
}

func TestVTCUtilizationBalancing(t *testing.T) {
	ctx := context.Background()
	redisClient.Del(ctx, "pod_metrics")

	setupVTCUsers(t)
	defer cleanupVTCUsers(t)

	users := make([]string, len(testUsers))
	for i, user := range testUsers {
		users[i] = user.Name
		redisClient.Del(ctx, fmt.Sprintf("user_tokens:%s", user.Name))
		redisClient.Del(ctx, fmt.Sprintf("rate_limit:tpm:%s", user.Name))
	}

	setupTestUsersWithHighTPM(t, users)

	t.Logf("Waiting for token window expiry to ensure clean state")
	time.Sleep(4 * time.Second)

	t.Logf("Making pruning-trigger requests for all test users")
	for _, user := range users {
		for i := 0; i < 3; i++ {
			pod := getTargetPodFromChatCompletionWithUser(t,
				fmt.Sprintf("Pruning trigger %d", i), "vtc-basic", user)
			t.Logf("User %s routed to pod %s (pruning trigger %d)", user, pod, i)
			time.Sleep(50 * time.Millisecond)
		}
	}

	ensureSufficientPods(t, 3)

	metrics := getPodMetrics(t)
	t.Logf("Pod metrics before controlled setup: %v", metrics)

	highLoadPod := availablePods[0]
	testUser := testUsers[1].Name
	t.Logf("Creating controlled load imbalance in Redis for pod %s", highLoadPod)

	redisClient.Del(ctx, "pod_metrics")
	time.Sleep(100 * time.Millisecond)

	highLoadValue := "100"
	normalLoadValue := "5"
	for _, pod := range availablePods {
		if pod == highLoadPod {
			_, err := redisClient.HSet(ctx, "pod_metrics", pod, highLoadValue).Result()
			if err != nil {
				t.Logf("Warning: Failed to set high load for pod %s: %v", pod, err)
			}
			t.Logf("Set high load (%s) for pod %s", highLoadValue, pod)
		} else {
			_, err := redisClient.HSet(ctx, "pod_metrics", pod, normalLoadValue).Result()
			if err != nil {
				t.Logf("Warning: Failed to set normal load for pod %s: %v", pod, err)
			}
			t.Logf("Set normal load (%s) for pod %s", normalLoadValue, pod)
		}
	}

	forceMetricsPropagation(t)
	time.Sleep(200 * time.Millisecond)

	metrics = getPodMetrics(t)
	t.Logf("Pod metrics after controlled setup: %v", metrics)

	highLoadMetric, exists := metrics[highLoadPod]
	if !exists || highLoadMetric != 100 {
		t.Logf("Warning: High load pod setup may not be correct. Expected %s to have load 100, got %d",
			highLoadPod, highLoadMetric)
	}

	ensureSufficientPods(t, 3)

	testRequestCount := 50
	podDistribution := make(map[string]int)

	t.Logf("Testing utilization balancing with user %s", testUser)
	for i := 0; i < testRequestCount; i++ {
		pod := getTargetPodFromChatCompletionWithUser(t,
			fmt.Sprintf("Test request %d", i), "vtc-basic", testUser)
		podDistribution[pod]++
		t.Logf("Test request %d routed to pod %s", i+1, pod)
		forceMetricsPropagation(t)
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("Test request distribution:")
	for pod, count := range podDistribution {
		t.Logf("  %s: %d requests (%.1f%%)", pod, count,
			float64(count)/float64(testRequestCount)*100)
	}

	highLoadCount := podDistribution[highLoadPod]
	maxAllowed := int(float64(testRequestCount) * 0.65)
	t.Logf("High-load pod received %d/%d requests (max allowed: %d)",
		highLoadCount, testRequestCount, maxAllowed)

	cv, quality := calculateDistributionStats(t, "Utilization Test", podDistribution)

	assert.LessOrEqual(t, highLoadCount, maxAllowed,
		"High-load pod should receive at most %d/%d requests due to utilization balancing",
		maxAllowed, testRequestCount)

	t.Logf("Distribution quality: %s (CV=%.2f)", quality, cv)
	lowLoadTotal := 0
	lowLoadPodCount := 0
	for pod, count := range podDistribution {
		if pod != highLoadPod {
			lowLoadTotal += count
			lowLoadPodCount++
		}
	}

	// With high load = 100 and low load = 5, the ratio is 20:1
	// In a 3-pod setup with one high-load pod, the theoretical max should be around 60-65%
	maxHighLoadPercentage := 65.0
	actualHighLoadPercentage := float64(highLoadCount) / float64(testRequestCount) * 100.0

	t.Logf("High-load pod percentage: %.1f%% (max expected: %.1f%%)",
		actualHighLoadPercentage, maxHighLoadPercentage)

	assert.LessOrEqual(t, actualHighLoadPercentage, maxHighLoadPercentage,
		"High-load pod (%.1f%%) should not receive more than %.1f%% of requests based on load ratios",
		actualHighLoadPercentage, maxHighLoadPercentage)
}

func keysFromMap(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
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
	// Force metrics propagation by waiting a bit
	time.Sleep(metricsWaitTime)

	// Verify metrics are available
	ctx := context.Background()
	exists, err := redisClient.Exists(ctx, "pod_metrics").Result()
	if err != nil {
		t.Logf("Warning: Error checking pod_metrics existence: %v", err)
		time.Sleep(metricsWaitTime * 2) // Wait longer on error
	}

	if err != nil || exists == 0 {
		// If metrics don't exist, wait a bit longer and try to set them again
		t.Log("Pod metrics not found, waiting longer before attempting restoration")
		time.Sleep(metricsWaitTime * 2)

		// Check if we need to restore the metrics
		metrics := getPodMetrics(t)
		if len(metrics) == 0 {
			t.Log("Warning: Pod metrics missing, attempting to restore with default values")

			// Make sure we have pods to work with
			if len(availablePods) == 0 {
				t.Log("No available pods found, attempting to discover pods")
				getAvailablePods(t)
			}

			if len(availablePods) > 0 {
				// Set first pod to high load and others to normal load as default
				for i, pod := range availablePods {
					if i == 0 {
						redisClient.HSet(ctx, "pod_metrics", pod, highLoadValue)
						t.Logf("Restored high load (%s) for pod %s", highLoadValue, pod)
					} else {
						redisClient.HSet(ctx, "pod_metrics", pod, normalLoadValue)
						t.Logf("Restored normal load (%s) for pod %s", normalLoadValue, pod)
					}
				}

				// Wait longer after restoration to ensure propagation
				time.Sleep(metricsWaitTime * 2)

				// Verify restoration was successful
				verifyMetrics := getPodMetrics(t)
				if len(verifyMetrics) == 0 {
					t.Log("ERROR: Failed to restore pod metrics after multiple attempts")
				} else {
					t.Logf("Successfully restored pod metrics: %v", verifyMetrics)
				}
			} else {
				t.Log("ERROR: Cannot restore pod metrics - no pods available")
			}
		} else {
			t.Logf("Pod metrics found after waiting: %v", metrics)
		}
	}
}
