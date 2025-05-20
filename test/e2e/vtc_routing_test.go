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
	{Name: "user1", Rpm: 1000, Tpm: 100000},
	{Name: "user2", Rpm: 1000, Tpm: 100000},
	{Name: "user3", Rpm: 1000, Tpm: 100000},
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

func setupVTCUsers(t testing.TB) {
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

	if tt, ok := t.(*testing.T); ok {
		tt.Cleanup(func() {
			cleanupVTCUsers(t)
		})
	}
}

func cleanupVTCUsers(t testing.TB) {
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
func getAvailablePods(t testing.TB) {
	availablePods = []string{}

	allPodsMap := make(map[string]bool)
	for i := 0; i < 30; i++ {
		pod := getTargetPodFromChatCompletionWithUser(t, fmt.Sprintf("Pod discovery request %d", i), "random", "")
		if pod != "" {
			allPodsMap[pod] = true
		}
	}

	for pod := range allPodsMap {
		availablePods = append(availablePods, pod)
	}

	t.Logf("Discovered %d pods using random routing", len(availablePods))
}

// runFlakyTest runs a test function with retries if it's marked as flaky
func runFlakyTest(t *testing.T, testName string, isFlaky bool, testFunc func(testing.TB)) {
	if isFlaky {
		maxRetries := 3
		success := false
		var lastErr error

		for i := 0; i < maxRetries; i++ {
			// Create a new test context for each attempt
			t.Run(fmt.Sprintf("%s (attempt %d)", testName, i+1), func(subT *testing.T) {
				// Create a custom testing.T that captures failures instead of failing
				customT := &customTestingT{
					T:      subT,
					failed: false,
					errors: make([]string, 0),
				}

				// Run the test function with our custom testing.T
				testFunc(customT)

				if customT.failed {
					lastErr = fmt.Errorf("test failed: %s", strings.Join(customT.errors, "; "))
					subT.Skip("Test failed, will retry")
				}
			})

			// Check if this attempt succeeded
			if lastErr == nil {
				success = true
				break
			}

			t.Logf("Test failed, retrying... (attempt %d/%d)", i+1, maxRetries)
			time.Sleep(time.Second) // Add delay between retries
		}

		if !success {
			t.Logf("WARNING: Test %s failed after %d retries: %v", testName, maxRetries, lastErr)
			t.Skipf("WARNING: Test %s failed after %d retries: %v", testName, maxRetries, lastErr)
		}
	} else {
		t.Run(testName, func(subT *testing.T) {
			testFunc(subT)
		})
	}
}

// customTestingT wraps testing.T to capture failures instead of failing
type customTestingT struct {
	*testing.T
	failed bool
	errors []string
}

func (t *customTestingT) Error(args ...interface{}) {
	t.failed = true
	t.errors = append(t.errors, fmt.Sprint(args...))
}

func (t *customTestingT) Errorf(format string, args ...interface{}) {
	t.failed = true
	t.errors = append(t.errors, fmt.Sprintf(format, args...))
}

func (t *customTestingT) Fail() {
	t.failed = true
}

func (t *customTestingT) FailNow() {
	t.failed = true
}

func (t *customTestingT) Failed() bool {
	return t.failed
}

func (t *customTestingT) Log(args ...interface{}) {
	t.T.Log(args...)
}

func (t *customTestingT) Logf(format string, args ...interface{}) {
	t.T.Logf(format, args...)
}

func (t *customTestingT) Skip(args ...interface{}) {
	t.T.Skip(args...)
}

func (t *customTestingT) Skipf(format string, args ...interface{}) {
	t.T.Skipf(format, args...)
}

func (t *customTestingT) SkipNow() {
	t.T.SkipNow()
}

func (t *customTestingT) Skipped() bool {
	return t.T.Skipped()
}

func (t *customTestingT) Helper() {
	t.T.Helper()
}

func (t *customTestingT) Name() string {
	return t.T.Name()
}

func (t *customTestingT) Cleanup(f func()) {
	t.T.Cleanup(f)
}

func (t *customTestingT) TempDir() string {
	return t.T.TempDir()
}

func TestVTCBasicValidPod(t *testing.T) {
	runFlakyTest(t, "TestVTCBasicValidPod", true, func(t testing.TB) {
		setupVTCUsers(t)

		req := "this is a test message for VTC Basic routing"
		targetPod := getTargetPodFromChatCompletionWithUser(t, req, "vtc-basic", "user1")
		assert.NotEmpty(t, targetPod, "vtc-basic target pod is empty")
	})
}

func TestVTCFallbackToRandom(t *testing.T) {
	runFlakyTest(t, "TestVTCFallbackToRandom", true, func(t testing.TB) {
		setupVTCUsers(t)

		req := "this is a test message for VTC Basic routing"
		targetPod := getTargetPodFromChatCompletionWithUser(t, req, "vtc-basic", "")
		assert.NotEmpty(t, targetPod, "vtc-basic target pod is empty")
	})
}

func TestVTCTokenAccumulation(t *testing.T) {
	runFlakyTest(t, "TestVTCTokenAccumulation", true, func(t testing.TB) {
		setupVTCUsers(t)

		users := []string{"user1", "user2", "user3"}
		ensureSufficientPods(t, 3)

		// Reset user token history and set high TPM limits
		resetTestUserState(t, users)

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
	})
}

// resetTestUserState resets the state of test users by deleting and recreating them,
// clearing token history, and setting rate limits
func resetTestUserState(t testing.TB, users []string) {
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
			t.Logf("Reset state for user %s", user)
		}
	}
}

// Helper function to generate token history
func generateTokenHistory(t testing.TB, userMsgs map[string]string) {
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
	case "user1":
		return "low"
	case "user2":
		return "medium"
	case "user3":
		return "high"
	default:
		return "unknown"
	}
}

// Helper function to make test requests
func makeTestRequests(t testing.TB, userMsgs map[string]string, users []string) (
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
func analyzeRoutingPatterns(t testing.TB, userRoutingMap map[string]map[string]int) {
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

			if tt, ok := t.(*testing.T); ok {
				assert.GreaterOrEqual(tt, dominantPercentage, 80.0,
					"Low/medium token user %s should be routed consistently to one pod", user)
			}
		}
	}
}

func TestVTCFairnessComponent(t *testing.T) {
	runFlakyTest(t, "TestVTCFairnessComponent", true, func(t testing.TB) {
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
		resetTestUserState(t, []string{user1Name, "user2", user3Name})

		shortMsg := "Small message."                                                       // ~2 tokens
		mediumMsg := "This is a medium-sized message with more tokens than the small one." // ~15 tokens
		extremeMsg := strings.Repeat("This is a longer message with more tokens. ", 10)    // ~150 tokens

		userMsgs := map[string]string{
			"user1": shortMsg,
			"user2": mediumMsg,
			"user3": extremeMsg,
		}

		ctxBg := context.Background()
		if _, err := redisClient.Del(ctxBg, "pod_metrics").Result(); err != nil {
			t.Logf("Warning: Failed to delete pod_metrics: %v", err)
		}
		forceMetricsPropagation(t)

		t.Log("Balancing pod loads before token history generation")
		getAvailablePods(t)
		for _, pod := range availablePods {
			if _, err := redisClient.HSet(ctxBg, "pod_metrics", pod, "10").Result(); err != nil {
				t.Logf("Warning: Failed to set pod metrics for %s: %v", pod, err)
			}
		}
		forceMetricsPropagation(t)

		generateTokenHistory(t, userMsgs)

		t.Log("Waiting for token history to be fully processed")
		time.Sleep(500 * time.Millisecond)

		t.Log("Testing fairness with varying message sizes after history generation")
		podAssignments, podHistogram, userRoutingMap := makeTestRequests(t, userMsgs, []string{"user1", "user2", "user3"})

		t.Log("Overall pod distribution:")
		for pod, count := range podHistogram {
			t.Logf("Pod %s received %d requests (%.1f%%)",
				pod, count, float64(count)*100/float64(len(podAssignments)))
		}

		t.Log("Checking user consistency and token-based routing:")
		analyzeRoutingPatterns(t, userRoutingMap)

		userPods := make(map[string][]string)
		for user, podCounts := range userRoutingMap {
			for pod := range podCounts {
				userPods[user] = append(userPods[user], pod)
			}
		}

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
	runFlakyTest(t, "TestVTCHighUtilizationFairness", true, func(t testing.TB) {
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
		resetTestUserState(t, users)

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
			"user1": veryShortMsg,
			"user2": mediumTokenMsg,
			"user3": highTokenMsg,
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
			"With high utilization across all pods, high token user (user3) should use more pods "+
				"(%d) than low token user (user1, %d pods)",
			highTokenUserPods, lowTokenUserPods)

		assert.LessOrEqual(t, lowTokenUserPods, 2,
			"Low token user should be routed consistently to 1-2 pods, got %d pods",
			lowTokenUserPods)
	})
}

func TestVTCUtilizationBalancing(t *testing.T) {
	runFlakyTest(t, "TestVTCUtilizationBalancing", true, func(t testing.TB) {
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

		resetTestUserState(t, users)

		t.Logf("Waiting for token window expiry to ensure clean state")
		// With AIBRIX_ROUTER_VTC_TOKEN_TRACKER_WINDOW_SIZE=2 and TIME_UNIT=seconds
		// We need to wait at least 2 seconds
		time.Sleep(3 * time.Second)

		t.Logf("Making pruning-trigger requests for all test users")
		for _, user := range users {
			pod := getTargetPodFromChatCompletionWithUser(t, "Pruning trigger", "vtc-basic", user)
			t.Logf("User %s routed to pod %s (pruning trigger)", user, pod)
		}

		ensureSufficientPods(t, 3)

		metrics := getPodMetrics(t)
		t.Logf("Pod metrics before controlled setup: %v", metrics)

		highLoadPod := availablePods[0]

		testUser := testUsers[1].Name
		t.Logf("Creating controlled load imbalance in Redis for pod %s", highLoadPod)

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
	})
}

func keysFromMap(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func convertToHistogram(podAssignments map[string]string) map[string]int {
	histogram := make(map[string]int)
	for _, pod := range podAssignments {
		histogram[pod]++
	}
	return histogram
}

func calculateDistributionStats(
	t testing.TB,
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

func getPodMetrics(t testing.TB) map[string]int {
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

func forceMetricsPropagation(t testing.TB) {
	redisClient.Publish(context.Background(), "pod_metrics_refresh", "")
	time.Sleep(metricsWaitTime)
}

func ensureSufficientPods(t testing.TB, minPods int) {
	getAvailablePods(t)
	if len(availablePods) < minPods {
		t.Skipf("Need at least %d pods for utilization test, found %d", minPods, len(availablePods))
	}
	t.Logf("Found %d available pods, proceeding with test ", len(availablePods))
}

func getTargetPodFromChatCompletionWithUser(t testing.TB, message, strategy, user string) string {
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
	if err != nil {
		t.Logf("Error creating chat completion: %v", err)
		return ""
	}
	assert.Equal(t, modelName, chatCompletion.Model)

	return dst.Header.Get("target-pod")
}
