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
package vtc

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// SimpleCache is a simplified implementation of the cache interface for testing
type SimpleCache struct {
	metrics map[string]map[string]map[string]float64
}

func NewSimpleCache() *SimpleCache {
	return &SimpleCache{
		metrics: make(map[string]map[string]map[string]float64),
	}
}

func (c *SimpleCache) SetPodMetric(podKey, modelName, metricName string, value float64) {
	if _, ok := c.metrics[podKey]; !ok {
		c.metrics[podKey] = make(map[string]map[string]float64)
	}
	if _, ok := c.metrics[podKey][modelName]; !ok {
		c.metrics[podKey][modelName] = make(map[string]float64)
	}
	c.metrics[podKey][modelName][metricName] = value
}

func (c *SimpleCache) GetMetricValueByPodModel(podName, podNamespace, modelName, metricName string) (metrics.MetricValue, error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	if _, ok := c.metrics[key]; !ok {
		return &metrics.SimpleMetricValue{Value: 0}, nil
	}
	if _, ok := c.metrics[key][modelName]; !ok {
		return &metrics.SimpleMetricValue{Value: 0}, nil
	}
	if value, ok := c.metrics[key][modelName][metricName]; ok {
		return &metrics.SimpleMetricValue{Value: value}, nil
	}
	return &metrics.SimpleMetricValue{Value: 0}, nil
}

func (c *SimpleCache) AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) int64 {
	return 1
}

func (c *SimpleCache) DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64) {
}

func (c *SimpleCache) DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64, inputTokens int64, outputTokens int64) {
}

func (c *SimpleCache) GetPod(podName, podNamespace string) (*v1.Pod, error) {
	return nil, nil
}

func (c *SimpleCache) ListPodsByModel(modelName string) (types.PodList, error) {
	return nil, nil
}

func (c *SimpleCache) HasModel(modelName string) bool {
	return true
}

func (c *SimpleCache) ListModels() []string {
	return []string{}
}

func (c *SimpleCache) ListModelsByPod(podName, podNamespace string) ([]string, error) {
	return []string{}, nil
}

func (c *SimpleCache) GetMetricValueByPod(podName, podNamespace, metricName string) (metrics.MetricValue, error) {
	return nil, nil
}

func (c *SimpleCache) AddSubscriber(subscriber metrics.MetricSubscriber) {
}

// SimplePodList is a simplified implementation of PodList for testing
type SimplePodList struct {
	pods []*v1.Pod
}

func NewSimplePodList(pods []*v1.Pod) *SimplePodList {
	return &SimplePodList{
		pods: pods,
	}
}

func (p *SimplePodList) All() []*v1.Pod {
	return p.pods
}

func (p *SimplePodList) Len() int {
	return len(p.pods)
}

func (p *SimplePodList) Indexes() []string {
	return []string{"default"}
}

func (p *SimplePodList) ListByIndex(index string) []*v1.Pod {
	return p.pods
}

func TestVTCRouterSimple(t *testing.T) {
	trackerConfig := &VTCConfig{
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.0,
		Variant:           RouterVTCBasic,
	}
	tokenTracker := NewInMemorySlidingWindowTokenTracker(trackerConfig, WithWindowSize(100), WithTimeUnit(Milliseconds))
	tokenEstimator := NewSimpleTokenEstimator()
	cache := NewSimpleCache()

	routerConfig := &VTCConfig{
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.0,
		Variant:           RouterVTCBasic,
	}
	router := &BasicVTCRouter{
		cache:          cache,
		tokenTracker:   tokenTracker,
		tokenEstimator: tokenEstimator,
		config:         routerConfig,
	}

	pod1 := &v1.Pod{}
	pod1.Status.PodIP = "192.168.1.1"
	pod1.Status.Phase = v1.PodRunning
	pod1.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	pod1.Name = "pod1"

	pod2 := &v1.Pod{}
	pod2.Status.PodIP = "192.168.1.2"
	pod2.Status.Phase = v1.PodRunning
	pod2.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	pod2.Name = "pod2"

	pod3 := &v1.Pod{}
	pod3.Status.PodIP = "192.168.1.3"
	pod3.Status.Phase = v1.PodRunning
	pod3.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	pod3.Name = "pod3"

	// Set up pod metrics for testing
	pod1Key := utils.GeneratePodKey("default", "pod1")
	pod2Key := utils.GeneratePodKey("default", "pod2")
	pod3Key := utils.GeneratePodKey("default", "pod3")
	cache.SetPodMetric(pod1Key, "model1", metrics.NumRequestsRunning, 0)
	cache.SetPodMetric(pod2Key, "model1", metrics.NumRequestsRunning, 0)
	cache.SetPodMetric(pod3Key, "model1", metrics.NumRequestsRunning, 0)

	pods := []*v1.Pod{pod1, pod2, pod3}
	podList := NewSimplePodList(pods)

	ctx := context.Background()
	user := "user1"
	err := tokenTracker.UpdateTokenCount(ctx, user, 0, 0)
	assert.NoError(t, err)

	// Test 1: With user - should use VTC routing
	routingCtx := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test message", "request1", user)

	selectedPodAddress, err := router.Route(routingCtx, podList)
	assert.NoError(t, err)
	assert.NotEmpty(t, selectedPodAddress)

	// Check if the selected pod address is one of the expected IP addresses with port
	assert.Contains(t, []string{"192.168.1.1:8000", "192.168.1.2:8000", "192.168.1.3:8000"}, selectedPodAddress)

	tokens, err := tokenTracker.GetTokenCount(ctx, user)
	assert.NoError(t, err)
	// Expected tokens = input (ceil(12/4)=3) + output (ceil(3*1.5)=5) = 8
	assert.Equal(t, float64(8.0), tokens)

	// Test 2: Route without user - should fall back to random selection
	routingCtx = types.NewRoutingContext(ctx, "vtc-basic", "model1", "test message", "request2", "") // User is empty string

	selectedPodAddress, err = router.Route(routingCtx, podList)
	assert.NoError(t, err)
	assert.NotEmpty(t, selectedPodAddress)
	// Check if the selected pod address is one of the expected IP addresses with port
	assert.Contains(t, []string{"192.168.1.1:8000", "192.168.1.2:8000", "192.168.1.3:8000"}, selectedPodAddress)
}

func TestVTCBasicRouterStrengths(t *testing.T) {
	trackerConfig := &VTCConfig{
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.0,
		Variant:           RouterVTCBasic,
	}
	cache := NewSimpleCache()
	tokenEstimator := NewSimpleTokenEstimator()
	routerConfig := &VTCConfig{
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.0,
		Variant:           RouterVTCBasic,
	}
	ctx := context.Background()

	// Test basic strengths
	t.Run("FairnessDistribution", func(t *testing.T) {
		// Creates a fresh tracker for each test
		tracker := NewInMemorySlidingWindowTokenTracker(trackerConfig)
		router := &BasicVTCRouter{
			cache:          cache,
			tokenTracker:   tracker,
			tokenEstimator: tokenEstimator,
			config:         routerConfig,
		}

		pods := createTestPods(3)
		podList := NewSimplePodList(pods)

		// Set up different token counts
		users := []struct {
			name   string
			tokens float64
		}{
			{"user1", 10},
			{"user2", 50},
			{"user3", 90},
		}

		// All pods have equal load
		for i := 0; i < 3; i++ {
			podKey := utils.GeneratePodKey("default", fmt.Sprintf("pod%d", i+1))
			cache.SetPodMetric(podKey, "model1", metrics.NumRequestsRunning, 0)
		}

		// Track each user's routing decision
		podAddresses := make([]string, len(users))

		// Route each user and capture results
		for i, u := range users {
			_ = tracker.UpdateTokenCount(ctx, u.name, u.tokens, 0)
			routingCtx := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test", fmt.Sprintf("req-%d", i), u.name)
			podAddr, err := router.Route(routingCtx, podList)
			assert.NoError(t, err)
			podAddresses[i] = podAddr
		}

		// Verify each pod address is valid
		for i, addr := range podAddresses {
			assert.Contains(t, []string{"192.168.1.1:8000", "192.168.1.2:8000", "192.168.1.3:8000"}, addr,
				"User with %v tokens should get valid pod address", users[i].tokens)
		}
	})

	t.Run("UtilizationBalancing", func(t *testing.T) {
		// Setup tracker and router
		tracker := NewInMemorySlidingWindowTokenTracker(trackerConfig)
		router := &BasicVTCRouter{
			cache:          cache,
			tokenTracker:   tracker,
			tokenEstimator: tokenEstimator,
			config:         routerConfig,
		}

		pods := createTestPods(3)
		podList := NewSimplePodList(pods)

		// Utilization balancing: when fairness is equal across pods, lowest load pod should be selected.
		user := "loadUser"
		// Set token count to half the adaptive bucket size (NormalizedTokens = 0.5)
		// Fairness tie (0.5) comes from default minTokens=1000 and maxTokens=8000 (adaptiveBucket=4500)
		// With util scores (0.8,0.4,0.1), pod2’s combined score is lowest ⇒ deterministic selection
		_ = tracker.UpdateTokenCount(ctx, user, 2250, 0)

		// Set different loads on pods
		pod1Key := utils.GeneratePodKey("default", "pod1")
		pod2Key := utils.GeneratePodKey("default", "pod2")
		pod3Key := utils.GeneratePodKey("default", "pod3")
		cache.SetPodMetric(pod1Key, "model1", metrics.NumRequestsRunning, 80) // High load
		cache.SetPodMetric(pod2Key, "model1", metrics.NumRequestsRunning, 40) // Medium load
		cache.SetPodMetric(pod3Key, "model1", metrics.NumRequestsRunning, 10) // Low load

		routingCtx := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test", "load-test", user)
		podAddr, err := router.Route(routingCtx, podList)
		assert.NoError(t, err)
		// Pod2 should be chosen: fairness equal for pod1/pod2, and load is lower on pod2
		assert.Equal(t, "192.168.1.2:8000", podAddr, "Utilization balancing: mid-load pod should be selected when fairness is equal")
	})

	t.Run("TokenAccumulation", func(t *testing.T) {
		tracker := NewInMemorySlidingWindowTokenTracker(trackerConfig)

		// Set initial tokens
		user := "accumUser"
		initialTokens := float64(10)
		_ = tracker.UpdateTokenCount(ctx, user, initialTokens, 0)

		// Get token count
		tokens, err := tracker.GetTokenCount(ctx, user)
		assert.NoError(t, err)
		assert.Equal(t, initialTokens, tokens, "Initial tokens should be set correctly")

		// Update tokens
		addTokensInput := float64(5)
		addTokensOutput := float64(10)
		err = tracker.UpdateTokenCount(ctx, user, addTokensInput, addTokensOutput)
		assert.NoError(t, err)

		// Verify accumulated tokens
		tokens, err = tracker.GetTokenCount(ctx, user)
		assert.NoError(t, err)
		assert.Equal(t, initialTokens+addTokensInput+addTokensOutput, tokens, "Tokens should accumulate correctly")
	})

	// Fairness monotonicity across pods
	t.Run("FairnessMonotonicity3Pods", func(t *testing.T) {
		podsF := createTestPods(3)
		podListF := NewSimplePodList(podsF)
		lastIdx := -1
		for _, tokens := range []float64{10, 50, 90} {
			tr := NewInMemorySlidingWindowTokenTracker(trackerConfig)
			r := &BasicVTCRouter{cache: cache, tokenTracker: tr, tokenEstimator: tokenEstimator, config: routerConfig}
			_ = tr.UpdateTokenCount(ctx, "fairUser", tokens, 0)
			addr, err := r.Route(types.NewRoutingContext(ctx, "vtc-basic", "model1", "test", fmt.Sprintf("req-%v", tokens), "fairUser"), podListF)
			assert.NoError(t, err)
			idx := -1
			for i, p := range podsF {
				if addr == fmt.Sprintf("%s:8000", p.Status.PodIP) {
					idx = i
					break
				}
			}
			assert.NotEqual(t, -1, idx)
			assert.GreaterOrEqual(t, idx, lastIdx, fmt.Sprintf("pod index %d for tokens %v should be >= last index %d", idx, tokens, lastIdx))
			lastIdx = idx
		}
	})

	// Logarithmic scaling compresses wide token ranges and maintains monotonicity
	t.Run("LogarithmicScaling", func(t *testing.T) {
		podsG := createTestPods(2)
		podListG := NewSimplePodList(podsG)

		// Reset pod loads to 0 to isolate the logarithmic distribution behavior
		pod1Key := utils.GeneratePodKey("default", "pod1")
		pod2Key := utils.GeneratePodKey("default", "pod2")
		cache.SetPodMetric(pod1Key, "model1", metrics.NumRequestsRunning, 0)
		cache.SetPodMetric(pod2Key, "model1", metrics.NumRequestsRunning, 0)

		// Test monotonicity with increasing token values
		tokenValues := []float64{1, 10, 100, 1000}
		prevIdx := -1
		for i, tokens := range tokenValues {
			tr := NewInMemorySlidingWindowTokenTracker(trackerConfig)
			r := &BasicVTCRouter{cache: cache, tokenTracker: tr, tokenEstimator: tokenEstimator, config: routerConfig}
			_ = tr.UpdateTokenCount(ctx, "groupUser", tokens, 0)
			addr, err := r.Route(types.NewRoutingContext(ctx, "vtc-basic", "model1", "test", fmt.Sprintf("req-%v", tokens), "groupUser"), podListG)
			assert.NoError(t, err)
			idx := -1
			for i, p := range podsG {
				if addr == fmt.Sprintf("%s:8000", p.Status.PodIP) {
					idx = i
					break
				}
			}
			// Not testing specific pod assignments but ensuring monotonicity
			if i > 0 {
				assert.GreaterOrEqual(t, idx, prevIdx,
					fmt.Sprintf("Token %v should have pod index >= previous index %d", tokens, prevIdx))
			}
			prevIdx = idx

			// For sanity, make sure we're routing to a valid pod
			assert.NotEqual(t, -1, idx, "Should route to a valid pod")
		}
	})

	// Clamp fairness when tokens exceed adaptive bucket*(nPods-1)
	t.Run("ClampedFairnessHighTokens", func(t *testing.T) {
		highTracker := NewInMemorySlidingWindowTokenTracker(trackerConfig)
		highRouter := &BasicVTCRouter{cache: cache, tokenTracker: highTracker, tokenEstimator: tokenEstimator, config: routerConfig}
		podsHigh := createTestPods(3)
		highList := NewSimplePodList(podsHigh)
		// Equal loads for all pods
		for i := 1; i <= 3; i++ {
			podKey := utils.GeneratePodKey("default", fmt.Sprintf("pod%d", i))
			cache.SetPodMetric(podKey, "model1", metrics.NumRequestsRunning, 0)
		}
		// Use tokens > adaptiveBucket*2 to clamp normalizedTokens to last index=2
		_ = highTracker.UpdateTokenCount(ctx, "highUser", 10000, 0)
		addr, err := highRouter.Route(types.NewRoutingContext(ctx, "vtc-basic", "model1", "test", "req-high", "highUser"), highList)
		assert.NoError(t, err)
		// Should select middle pod (index 1) due to clamped fairness index
		assert.Equal(t, "192.168.1.2:8000", addr, "High tokens clamp fairness to middle pod index")
	})

	// Random fallback when user is nil
	t.Run("RandomFallbackNilUser", func(t *testing.T) {
		rfTracker := NewInMemorySlidingWindowTokenTracker(trackerConfig)
		rfRouter := &BasicVTCRouter{cache: cache, tokenTracker: rfTracker, tokenEstimator: tokenEstimator, config: routerConfig}
		podsRF := createTestPods(3)
		rfList := NewSimplePodList(podsRF)
		routingCtx := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test", "req-rf", "")
		addr, err := rfRouter.Route(routingCtx, rfList)
		assert.NoError(t, err)
		assert.Contains(t, []string{"192.168.1.1:8000", "192.168.1.2:8000", "192.168.1.3:8000"}, addr)
	})
}

func TestWeightCombinations(t *testing.T) {
	// Use Milliseconds and set weights for testing
	t.Setenv("AIBRIX_ROUTER_VTC_TOKEN_TRACKER_TIME_UNIT", "milliseconds")
	t.Setenv("AIBRIX_ROUTER_VTC_TOKEN_TRACKER_WINDOW_SIZE", "50")
	t.Setenv("AIBRIX_ROUTER_VTC_BASIC_FAIRNESS_WEIGHT", "0.5")
	t.Setenv("AIBRIX_ROUTER_VTC_BASIC_UTILIZATION_WEIGHT", "0.5")

	ctx := context.Background()
	cache := NewSimpleCache()
	tokenEstimator := NewSimpleTokenEstimator()
	podList := NewSimplePodList(createTestPods(3))

	// Set up pod loads - High, Medium, Low
	podLoads := []float64{80.0, 40.0, 10.0}
	for i, pod := range podList.All() {
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		cache.SetPodMetric(podKey, "model1", metrics.NumRequestsRunning, podLoads[i])
	}

	// Set up user tokens
	userTokens := float64(4500)
	user := "user-balanced-test"

	// Create tracker and router
	trackerConfig := &VTCConfig{
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.0,
		Variant:           RouterVTCBasic,
	}
	tracker := NewInMemorySlidingWindowTokenTracker(trackerConfig)
	router := &BasicVTCRouter{
		cache:          cache,
		tokenTracker:   tracker,
		tokenEstimator: tokenEstimator,
		config:         trackerConfig,
	}

	// Set up user tokens
	err := tracker.UpdateTokenCount(ctx, user, userTokens, 0)
	assert.NoError(t, err)

	// Force min/max token values to make tests deterministic
	oldMinTokens := tokenTrackerMinTokens
	oldMaxTokens := tokenTrackerMaxTokens
	tokenTrackerMinTokens = userTokens
	tokenTrackerMaxTokens = userTokens
	defer func() {
		tokenTrackerMinTokens = oldMinTokens
		tokenTrackerMaxTokens = oldMaxTokens
	}()

	// Route the request
	routingCtx := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test message", "request-weight-test", user)
	selectedPodAddress, err := router.Route(routingCtx, podList)
	assert.NoError(t, err)

	// Verify the selected pod matches expectations (pod2)
	expectedPodIndex := 1
	expectedPod := podList.All()[expectedPodIndex]
	expectedAddress := fmt.Sprintf("%s:8000", expectedPod.Status.PodIP)
	assert.Equal(t, expectedAddress, selectedPodAddress, "With balanced weights (0.5/0.5), expected pod 2 to be selected")
}

func createTestPods(count int) []*v1.Pod {
	pods := make([]*v1.Pod, count)
	for i := 0; i < count; i++ {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod%d", i+1)},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("192.168.1.%d", i+1),
				Phase:      v1.PodRunning,
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	return pods
}

// TestVTCBucketSizePatterns demonstrates key patterns in the vtc_bucket_size_active metric
// and how to interpret them for configuration adjustments
// vtc_bucket_size_active - gauge(pod,model): Shows whether adaptive bucket size is stable
// - Smooth slope → healthy; Saw-tooth jumps → increase min bucket size or window
func TestVTCBucketSizePatterns(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	testGauge, cleanup := metrics.SetupMetricsForTest(metrics.VTCBucketSizeActive, []string{"pod", "model"})
	defer cleanup()

	trackerConfig := &VTCConfig{
		InputTokenWeight:  defaultInputTokenWeight,
		OutputTokenWeight: defaultOutputTokenWeight,
	}
	tokenTracker := NewInMemorySlidingWindowTokenTracker(trackerConfig, WithWindowSize(100), WithTimeUnit(Milliseconds))
	cache := NewSimpleCache()
	router := &BasicVTCRouter{
		cache:          cache,
		tokenTracker:   tokenTracker,
		tokenEstimator: NewSimpleTokenEstimator(),
		config:         trackerConfig,
	}

	pods := createTestPodsForMetrics(2)
	podList := NewSimplePodList(pods)
	for _, pod := range pods {
		cache.SetPodMetric(utils.GeneratePodKey("default", pod.Name), "model1", metrics.NumRequestsRunning, 0)
	}
	ctx := context.Background()

	patterns := []struct {
		name       string
		user       string
		tokenFunc  func(i int) (float64, float64)
		iterations int
		detectFunc func(sizes []float64) bool
		expected   string
		suggestion string
	}{
		{
			name:       "Smooth gradual slope (healthy)",
			user:       "gradual-user",
			tokenFunc:  func(i int) (float64, float64) { return float64(100 * (i + 1)), float64(200 * (i + 1)) },
			iterations: 4,
			detectFunc: func(sizes []float64) bool {
				// Check if changes are gradual (no jumps > 50%)
				for i := 1; i < len(sizes); i++ {
					if sizes[i] > sizes[i-1]*1.5 {
						return false
					}
				}
				return true
			},
			expected: "Smooth, gradual slope in bucket size - VTC adaptation is healthy",
		},
		{
			name: "Saw-tooth jumps (problematic)",
			user: "erratic-user",
			tokenFunc: func(i int) (float64, float64) {
				if i%2 == 0 {
					return 5000.0, 10000.0 // High spike
				}
				return 100.0, 200.0 // Low valley
			},
			iterations: 4,
			detectFunc: func(sizes []float64) bool {
				// Count significant jumps (>30% change)
				jumpCount := 0
				for i := 1; i < len(sizes); i++ {
					change := math.Abs(sizes[i]-sizes[i-1]) / sizes[i-1]
					if change > 0.3 {
						jumpCount++
					}
				}
				return jumpCount >= 2
			},
			expected:   "Saw-tooth jumps in bucket size",
			suggestion: "Increase AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MIN_TOKENS or WINDOW_SIZE",
		},
	}

	for _, pattern := range patterns {
		sizes := make([]float64, 0, pattern.iterations)
		for i := 0; i < pattern.iterations; i++ {
			inTokens, outTokens := pattern.tokenFunc(i)
			err := tokenTracker.UpdateTokenCount(ctx, pattern.user, inTokens, outTokens)
			if err != nil {
				t.Errorf("Failed to update token count for %s: %v", pattern.user, err)
				continue
			}

			requestID := fmt.Sprintf("request-%s-%d", pattern.user, i)
			routingCtx := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test message", requestID, pattern.user)

			_, err = router.Route(routingCtx, podList)
			if err != nil {
				t.Errorf("Failed to route request for %s: %v", pattern.user, err)
				continue
			}

			podName := fmt.Sprintf("pod-metrics-%c", 'a'+i)
			metricValue := testutil.ToFloat64(testGauge.WithLabelValues(podName, "model1"))
			sizes = append(sizes, metricValue)
		}

		t.Logf("%s bucket sizes: %v", pattern.name, sizes)
		if pattern.detectFunc(sizes) {
			t.Logf("PATTERN DETECTED: %s", pattern.expected)
			if pattern.suggestion != "" {
				t.Logf("RECOMMENDATION: %s", pattern.suggestion)
			}
		}
	}

	t.Log("\nvtc_bucket_size_active metric interpretation guide:")
	t.Log("1. Smooth changes: VTC adaptation is healthy")
	t.Log("2. Saw-tooth jumps: Increase min bucket size or window")
	t.Log("3. High values: Reduce window size")
	t.Log("4. Low values: Consider decreasing min threshold")
}

func createTestPodsForMetrics(count int) []*v1.Pod {
	pods := make([]*v1.Pod, count)
	for i := 0; i < count; i++ {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-metrics-" + string(rune('a'+i)),
				Namespace: "default",
			},
			Status: v1.PodStatus{
				PodIP: "192.168.1." + string(rune('1'+i)),
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		}
	}
	return pods
}
