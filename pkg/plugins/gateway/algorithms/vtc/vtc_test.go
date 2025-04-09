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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// SimpleTokenTracker is a simplified implementation of TokenTracker for testing
type SimpleTokenTracker struct {
	tokenCounter map[string]float64
}

func NewSimpleTokenTracker() *SimpleTokenTracker {
	return &SimpleTokenTracker{
		tokenCounter: make(map[string]float64),
	}
}

func (t *SimpleTokenTracker) GetTokenCount(ctx context.Context, user string) (float64, error) {
	return t.tokenCounter[user], nil
}

func (t *SimpleTokenTracker) UpdateTokenCount(ctx context.Context, user string, inputTokens, outputTokens float64) error {
	t.tokenCounter[user] += inputTokens + outputTokens
	return nil
}

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

func (c *SimpleCache) GetMetricValueByPodModel(podName, modelName, metricName string) (metrics.MetricValue, error) {
	if _, ok := c.metrics[podName]; !ok {
		return &metrics.SimpleMetricValue{Value: 0}, nil
	}
	if _, ok := c.metrics[podName][modelName]; !ok {
		return &metrics.SimpleMetricValue{Value: 0}, nil
	}
	if value, ok := c.metrics[podName][modelName][metricName]; ok {
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

func (c *SimpleCache) GetPod(podName string) (*v1.Pod, error) {
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

func (c *SimpleCache) ListModelsByPod(podName string) ([]string, error) {
	return []string{}, nil
}

func (c *SimpleCache) GetMetricValueByPod(podName, metricName string) (metrics.MetricValue, error) {
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

// TestVTCRouterSimple tests the VTC router
func TestVTCRouterSimple(t *testing.T) {
	tokenTracker := NewSimpleTokenTracker()
	tokenEstimator := NewSimpleTokenEstimator()
	cache := NewSimpleCache()

	// Create router with real implementation
	config := &VTCConfig{
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.0,
		Variant:           RouterVTCBasic,
	}
	// Create router directly with the test cache instead of using NewBasicVTCRouter
	// This avoids the need for a real Redis connection in tests
	router := &BasicVTCRouter{
		cache:          cache,
		tokenTracker:   tokenTracker,
		tokenEstimator: tokenEstimator,
		config:         config,
	}

	// Create test pods with proper setup to be considered ready
	pod1 := &v1.Pod{}
	pod1.Status.PodIP = "192.168.1.1"
	pod1.Status.Phase = v1.PodRunning
	pod1.Status.Conditions = []v1.PodCondition{
		{Type: v1.PodReady, Status: v1.ConditionTrue},
	}
	pod1.Name = "pod1"

	pod2 := &v1.Pod{}
	pod2.Status.PodIP = "192.168.1.2"
	pod2.Status.Phase = v1.PodRunning
	pod2.Status.Conditions = []v1.PodCondition{
		{Type: v1.PodReady, Status: v1.ConditionTrue},
	}
	pod2.Name = "pod2"

	pod3 := &v1.Pod{}
	pod3.Status.PodIP = "192.168.1.3"
	pod3.Status.Phase = v1.PodRunning
	pod3.Status.Conditions = []v1.PodCondition{
		{Type: v1.PodReady, Status: v1.ConditionTrue},
	}
	pod3.Name = "pod3"

	// Set up pod metrics for testing
	cache.SetPodMetric("pod1", "model1", metrics.NumRequestsRunning, 0)
	cache.SetPodMetric("pod2", "model1", metrics.NumRequestsRunning, 0)
	cache.SetPodMetric("pod3", "model1", metrics.NumRequestsRunning, 0)

	pods := []*v1.Pod{pod1, pod2, pod3}
	podList := NewSimplePodList(pods)

	// Note: The VTC router uses a hybrid scoring approach that combines fairness (based on token usage)
	// and utilization (based on pod load). The exact pod selected may vary based on the implementation.
	// For testing purposes, we'll just verify that a valid pod is selected.

	// Set up token count for a test user
	ctx := context.Background()
	user := "user1"
	tokenTracker.UpdateTokenCount(ctx, user, 0, 0)

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

	// Test 3: Route with user that has high token count
	// This tests the fairness aspect of the VTC router
	tokenTracker.UpdateTokenCount(ctx, "user2", 5000, 0) // User with high token count
	user2 := "user2"
	routingCtx = types.NewRoutingContext(ctx, "vtc-basic", "model1", "test message", "request3", user2)

	selectedPodAddress, err = router.Route(routingCtx, podList)
	assert.NoError(t, err)
	assert.NotEmpty(t, selectedPodAddress)
	// Check if the selected pod address is one of the expected IP addresses with port (cannot assert specific pod due to hybrid scoring)
	assert.Contains(t, []string{"192.168.1.1:8000", "192.168.1.2:8000", "192.168.1.3:8000"}, selectedPodAddress)
}
