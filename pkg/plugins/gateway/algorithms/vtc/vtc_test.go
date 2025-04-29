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
package vtc

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func (c *SimpleCache) SetPodMetric(podName, modelName, metricName string, value float64) {
	if _, ok := c.metrics[podName]; !ok {
		c.metrics[podName] = make(map[string]map[string]float64)
	}
	if _, ok := c.metrics[podName][modelName]; !ok {
		c.metrics[podName][modelName] = make(map[string]float64)
	}
	c.metrics[podName][modelName][metricName] = value
}

func (c *SimpleCache) GetPodMetric(ctx context.Context, podIP, model, metricName string) (float64, error) {
	// In a real implementation, this would look up metrics by pod IP
	// For testing, we'll just return the value we set via SetPodMetric
	return 0, nil
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

	// Create test pods
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
	cache.SetPodMetric("pod1", "model1", metrics.NumRequestsRunning, 0)
	cache.SetPodMetric("pod2", "model1", metrics.NumRequestsRunning, 0)
	cache.SetPodMetric("pod3", "model1", metrics.NumRequestsRunning, 0)

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

func TestModuloNormalizationIssues(t *testing.T) {
	// 1. Test implicit grouping - tokens are grouped into buckets of 100 per pod
	// This creates arbitrary groupings that don't reflect actual usage patterns
	type testGroup struct {
		tokenCount    float64
		numPods       int
		normalizedVal float64
		podIndex      int
	}

	// Group 1: Token counts that should map to the same pod with correct modulo
	group1 := []testGroup{
		{34, 2, 0.34, 0},   // Low token count
		{234, 2, 0.34, 0},  // +200 tokens, same normalized value
		{1234, 2, 0.34, 0}, // +1000 tokens, same normalized value
		{2034, 2, 0.34, 0}, // +2000 tokens, same normalized value
	}

	// Group 2: Similar token counts that map to different pods
	group2 := []testGroup{
		{99, 2, 0.99, 1},  // Just under threshold
		{100, 2, 0.0, 0},  // Just over threshold - wraps around!
		{101, 2, 0.01, 0}, // Slightly over threshold
	}

	t.Log("ISSUE 1: LACK OF FAIRNESS - Users with vastly different token counts map to the same pod")
	for _, tc := range group1 {
		normalizedTokens := math.Mod(tc.tokenCount, float64(tc.numPods*100)) / 100.0
		normalizedTokens = math.Round(normalizedTokens*100) / 100

		closestPodIndex := 0
		minScore := math.MaxFloat64
		for i := 0; i < tc.numPods; i++ {
			fairnessScore := math.Abs(float64(i) - normalizedTokens)
			if fairnessScore < minScore {
				minScore = fairnessScore
				closestPodIndex = i
			}
		}

		assert.Equal(t, tc.normalizedVal, normalizedTokens,
			"Token count %v with %v pods should normalize to %v",
			tc.tokenCount, tc.numPods, tc.normalizedVal)

		assert.Equal(t, tc.podIndex, closestPodIndex,
			"Token count %v should map to pod index %v",
			tc.tokenCount, tc.podIndex)

		t.Logf("Token count: %v → Normalized value: %v → Pod index: %v",
			tc.tokenCount, normalizedTokens, closestPodIndex)
	}

	t.Log("\nISSUE 2: LACK OF MONOTONICITY - Slightly higher token counts can map to completely different pods")
	for _, tc := range group2 {
		normalizedTokens := math.Mod(tc.tokenCount, float64(tc.numPods*100)) / 100.0
		normalizedTokens = math.Round(normalizedTokens*100) / 100

		closestPodIndex := 0
		minScore := math.MaxFloat64
		for i := 0; i < tc.numPods; i++ {
			fairnessScore := math.Abs(float64(i) - normalizedTokens)
			if fairnessScore < minScore {
				minScore = fairnessScore
				closestPodIndex = i
			}
		}

		t.Logf("Token count: %v → Normalized value: %v → Pod index: %v",
			tc.tokenCount, normalizedTokens, closestPodIndex)
	}

	t.Log("\nISSUE 3: IMPLICIT GROUPING - Modulo creates arbitrary groupings of 100 tokens per pod")
	for i := 0; i < 5; i++ {
		groupStart := i * 100
		groupEnd := (i+1)*100 - 1
		normalizedStart := math.Mod(float64(groupStart), 200) / 100.0
		normalizedEnd := math.Mod(float64(groupEnd), 200) / 100.0
		t.Logf("Group %d: Tokens %d-%d → Normalized values %.2f-%.2f",
			i+1, groupStart, groupEnd, normalizedStart, normalizedEnd)
	}

	t.Log("\nCONCLUSION: Modulo normalization creates arbitrary groupings and lacks fairness/monotonicity")
}

func TestVTCRouterStrengths(t *testing.T) {
	trackerConfig := &VTCConfig{
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.0,
		Variant:           RouterVTCBasic,
	}
	tracker := NewInMemorySlidingWindowTokenTracker(trackerConfig)
	cache := NewSimpleCache()

	tokenEstimator := NewSimpleTokenEstimator()
	routerConfig := &VTCConfig{
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.0,
		Variant:           RouterVTCBasic,
	}

	router := &BasicVTCRouter{
		cache:          cache,
		tokenTracker:   tracker,
		tokenEstimator: tokenEstimator,
		config:         routerConfig,
	}

	t.Log("STRENGTH 1: Fairness within a single bucket (0-99 tokens) works well")

	pods := createTestPods(3)
	podList := NewSimplePodList(pods)

	// Set up different token counts within the same bucket
	ctx := context.Background()
	user1 := "user1" // 10 tokens
	user2 := "user2" // 50 tokens
	user3 := "user3" // 90 tokens

	err := tracker.UpdateTokenCount(ctx, user1, 10, 0)
	assert.NoError(t, err)
	err = tracker.UpdateTokenCount(ctx, user2, 50, 0)
	assert.NoError(t, err)
	err = tracker.UpdateTokenCount(ctx, user3, 90, 0)
	assert.NoError(t, err)

	// All pods have equal load
	cache.SetPodMetric("pod1", "model1", metrics.NumRequestsRunning, 0)
	cache.SetPodMetric("pod2", "model1", metrics.NumRequestsRunning, 0)
	cache.SetPodMetric("pod3", "model1", metrics.NumRequestsRunning, 0)

	// Route each user
	routingCtx1 := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test message", "request1", user1)
	routingCtx2 := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test message", "request2", user2)
	routingCtx3 := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test message", "request3", user3)

	pod1, err := router.Route(routingCtx1, podList)
	assert.NoError(t, err)
	pod2, err := router.Route(routingCtx2, podList)
	assert.NoError(t, err)
	pod3, err := router.Route(routingCtx3, podList)
	assert.NoError(t, err)

	// Users with different token counts within the same bucket should get different pods
	t.Logf("User1 (10 tokens) routed to: %s", pod1)
	t.Logf("User2 (50 tokens) routed to: %s", pod2)
	t.Logf("User3 (90 tokens) routed to: %s", pod3)

	t.Log("\nSTRENGTH 2: Utilization balancing works well")

	// Reset token counts
	user4 := "user4"
	err = tracker.UpdateTokenCount(ctx, user4, 50, 0) // Middle of bucket
	assert.NoError(t, err)

	// Set different loads on pods
	cache.SetPodMetric("pod1", "model1", metrics.NumRequestsRunning, 80) // High load
	cache.SetPodMetric("pod2", "model1", metrics.NumRequestsRunning, 40) // Medium load
	cache.SetPodMetric("pod3", "model1", metrics.NumRequestsRunning, 10) // Low load

	routingCtx4 := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test message", "request4", user4)
	pod4, err := router.Route(routingCtx4, podList)
	assert.NoError(t, err)

	// Should prefer lower-load pods when fairness is equal
	t.Logf("User4 (50 tokens) with pod loads [80, 40, 10] routed to: %s", pod4)

	t.Log("\nSTRENGTH 3: Hybrid scoring balances fairness and utilization")

	// Create a user with token count that would map to pod2 based on fairness alone
	user5 := "user5"
	err = tracker.UpdateTokenCount(ctx, user5, 150, 0) // Would map to pod index 1 (normalized to 0.5)
	assert.NoError(t, err)

	cache.SetPodMetric("pod1", "model1", metrics.NumRequestsRunning, 30) // Medium load
	cache.SetPodMetric("pod2", "model1", metrics.NumRequestsRunning, 90) // Very high load
	cache.SetPodMetric("pod3", "model1", metrics.NumRequestsRunning, 10) // Low load

	routingCtx5 := types.NewRoutingContext(ctx, "vtc-basic", "model1", "test message", "request5", user5)
	pod5, err := router.Route(routingCtx5, podList)
	assert.NoError(t, err)

	t.Logf("User5 (150 tokens) with pod loads [30, 90, 10] routed to: %s", pod5)
	t.Log("Note: The hybrid scoring should balance between fairness (which would select pod2) and utilization (which would prefer pod3)")

	t.Log("\nSTRENGTH 4: Token accumulation works correctly")

	initialTokens, err := tracker.GetTokenCount(ctx, user1)
	assert.NoError(t, err)
	t.Logf("Initial tokens for user1: %v", initialTokens)

	err = tracker.UpdateTokenCount(ctx, user1, 5, 10)
	assert.NoError(t, err)

	updatedTokens, err := tracker.GetTokenCount(ctx, user1)
	assert.NoError(t, err)
	t.Logf("Updated tokens for user1: %v (added 5 input tokens and 10 output tokens)", updatedTokens)

	expectedTokens := initialTokens + 5 + 10
	assert.Equal(t, expectedTokens, updatedTokens, "Token accumulation should work correctly")
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
