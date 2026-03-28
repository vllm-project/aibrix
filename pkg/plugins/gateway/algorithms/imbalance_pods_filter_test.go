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

package routingalgorithms

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-redis/redismock/v9"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// mockMetricsProvider implements MetricsProvider for testing
type mockMetricsProvider struct {
	requestCounts map[string]float64 // pod_name or pod_name_port -> count
}

func newMockMetricsProvider() *mockMetricsProvider {
	return &mockMetricsProvider{
		requestCounts: make(map[string]float64),
	}
}

func (m *mockMetricsProvider) GetMetricValueByPod(podName, namespace, metricName string) (metrics.MetricValue, error) {
	// For multi-port metrics like "vllm:num_requests_running/8000"
	// We need to match against pod_name_port format
	if len(metricName) > len(metrics.RealtimeNumRequestsRunning) && metricName[:len(metrics.RealtimeNumRequestsRunning)] == metrics.RealtimeNumRequestsRunning {
		// Extract port from metric name
		port := metricName[len(metrics.RealtimeNumRequestsRunning)+1:] // skip '/'
		key := podName + "_" + port
		if val, ok := m.requestCounts[key]; ok {
			return &metrics.SimpleMetricValue{Value: val}, nil
		}
	}

	// Try pod name directly
	if val, ok := m.requestCounts[podName]; ok {
		return &metrics.SimpleMetricValue{Value: val}, nil
	}
	return &metrics.SimpleMetricValue{Value: 0}, nil
}

// Helper functions to create test pods for imbalance filter tests
func createTestPodImbalance(name, namespace string, labels map[string]string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
	}
}

func createTestPodImbalanceWithPorts(name, namespace string, labels map[string]string, basePort int, dpSize int) *v1.Pod {
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[constants.ModelLabelPort] = fmt.Sprintf("%d", basePort)

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "main",
					Env: []v1.EnvVar{
						{
							Name:  "data-parallel-size",
							Value: fmt.Sprintf("%d", dpSize),
						},
					},
				},
			},
		},
	}
	return pod
}

func createTestPodListImbalance(pods []*v1.Pod) types.PodList {
	return &utils.PodArray{
		Pods: pods,
	}
}

// Test deduplicatePods function
func TestDeduplicatePods(t *testing.T) {
	pod1 := createTestPodImbalance("pod-1", "default", nil)
	pod2 := createTestPodImbalance("pod-2", "default", nil)

	tests := []struct {
		name     string
		input    []podServerKey
		expected int
	}{
		{
			name: "single pod, single port",
			input: []podServerKey{
				{pod: pod1, port: 8000},
			},
			expected: 1,
		},
		{
			name: "single pod, multiple ports",
			input: []podServerKey{
				{pod: pod1, port: 8000},
				{pod: pod1, port: 8001},
			},
			expected: 1,
		},
		{
			name: "multiple pods, single port each",
			input: []podServerKey{
				{pod: pod1, port: 8000},
				{pod: pod2, port: 8000},
			},
			expected: 2,
		},
		{
			name: "multiple pods, multiple ports",
			input: []podServerKey{
				{pod: pod1, port: 8000},
				{pod: pod1, port: 8001},
				{pod: pod2, port: 8000},
				{pod: pod2, port: 8001},
			},
			expected: 2,
		},
		{
			name:     "empty input",
			input:    []podServerKey{},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deduplicatePods(tt.input)
			assert.Equal(t, tt.expected, len(result))
		})
	}
}

// Test localImbalancePodsFilter
func TestLocalImbalancePodsFilter_FilterPodsWithPort_SinglePort(t *testing.T) {
	mockProvider := newMockMetricsProvider()
	filter := NewLocalImbalancePodsFilter(mockProvider)

	pod1 := createTestPodImbalance("pod-1", "default", nil)
	pod2 := createTestPodImbalance("pod-2", "default", nil)
	pod3 := createTestPodImbalance("pod-3", "default", nil)

	// Set request counts: pod-1=5, pod-2=10, pod-3=2
	mockProvider.requestCounts["pod-1"] = 5
	mockProvider.requestCounts["pod-2"] = 10
	mockProvider.requestCounts["pod-3"] = 2

	podList := createTestPodListImbalance([]*v1.Pod{pod1, pod2, pod3})

	candidates, imbalanced := filter.FilterPodsWithPort(podList)

	// Should be imbalanced (10-2=8 > threshold 2)
	assert.True(t, imbalanced)
	// Should return only pod-3 (min count)
	assert.Equal(t, 1, len(candidates))
	assert.Equal(t, "pod-3", candidates[0].pod.Name)
}

func TestLocalImbalancePodsFilter_FilterPodsWithPort_MultiPort(t *testing.T) {
	mockProvider := newMockMetricsProvider()
	filter := NewLocalImbalancePodsFilter(mockProvider)

	pod1 := createTestPodImbalanceWithPorts("pod-1", "default", nil, 8000, 2)
	pod2 := createTestPodImbalanceWithPorts("pod-2", "default", nil, 8000, 2)

	// Set request counts for multi-port pods
	mockProvider.requestCounts["pod-1_8000"] = 5
	mockProvider.requestCounts["pod-1_8001"] = 3
	mockProvider.requestCounts["pod-2_8000"] = 10
	mockProvider.requestCounts["pod-2_8001"] = 1

	podList := createTestPodListImbalance([]*v1.Pod{pod1, pod2})

	candidates, imbalanced := filter.FilterPodsWithPort(podList)

	// Should be imbalanced (10-1=9 > threshold 2)
	assert.True(t, imbalanced)
	// Should return pod-2:8001 (min count=1)
	assert.Equal(t, 1, len(candidates))
	assert.Equal(t, "pod-2", candidates[0].pod.Name)
	assert.Equal(t, 8001, candidates[0].port)
}

func TestLocalImbalancePodsFilter_FilterPodsWithPort_Balanced(t *testing.T) {
	mockProvider := newMockMetricsProvider()
	filter := NewLocalImbalancePodsFilter(mockProvider)

	pod1 := createTestPodImbalance("pod-1", "default", nil)
	pod2 := createTestPodImbalance("pod-2", "default", nil)

	// Set balanced request counts: pod-1=5, pod-2=6 (diff=1 <= threshold 2)
	mockProvider.requestCounts["pod-1"] = 5
	mockProvider.requestCounts["pod-2"] = 6

	podList := createTestPodListImbalance([]*v1.Pod{pod1, pod2})

	candidates, imbalanced := filter.FilterPodsWithPort(podList)

	// Should not be imbalanced
	assert.False(t, imbalanced)
	// Should return all candidates
	assert.Equal(t, 2, len(candidates))
}

func TestLocalImbalancePodsFilter_FilterPods(t *testing.T) {
	mockProvider := newMockMetricsProvider()
	filter := NewLocalImbalancePodsFilter(mockProvider)

	pod1 := createTestPodImbalanceWithPorts("pod-1", "default", nil, 8000, 2)

	// Multi-port scenario - balanced loads so it tests deduplication without imbalance
	mockProvider.requestCounts["pod-1_8000"] = 5
	mockProvider.requestCounts["pod-1_8001"] = 6

	podList := createTestPodListImbalance([]*v1.Pod{pod1})

	result, imbalanced := filter.FilterPods(podList)

	// Should not be imbalanced (diff=1 <= threshold 2), but should deduplicate - only 1 pod returned
	assert.False(t, imbalanced)
	assert.Equal(t, 1, len(result))
	assert.Equal(t, "pod-1", result[0].Name)
}

// Test redisImbalancePodsFilter
func TestRedisImbalancePodsFilter_FilterPodsWithPort(t *testing.T) {
	db, mock := redismock.NewClientMock()
	filter := NewRedisImbalancePodsFilter(db)

	pod1 := createTestPodImbalance("pod-1", "default", map[string]string{"model": "test-model"})
	pod2 := createTestPodImbalance("pod-2", "default", map[string]string{"model": "test-model"})
	pod3 := createTestPodImbalance("pod-3", "default", map[string]string{"model": "test-model"})

	podList := createTestPodListImbalance([]*v1.Pod{pod1, pod2, pod3})

	// Mock Redis HGETALL response
	mock.ExpectHGetAll("aibrix:prefix-cache-reqcnt:{test-model}").SetVal(map[string]string{
		"pod-1": "5",
		"pod-2": "10",
		"pod-3": "2",
	})

	candidates, imbalanced := filter.FilterPodsWithPort(podList)

	// Should be imbalanced (10-2=8 > threshold 2)
	assert.True(t, imbalanced)
	// Should return only pod-3 (min count)
	assert.Equal(t, 1, len(candidates))
	assert.Equal(t, "pod-3", candidates[0].pod.Name)

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRedisImbalancePodsFilter_FilterPodsWithPort_NoModelLabel(t *testing.T) {
	db, _ := redismock.NewClientMock()
	filter := NewRedisImbalancePodsFilter(db)

	pod1 := createTestPodImbalance("pod-1", "default", nil) // No model label

	podList := createTestPodListImbalance([]*v1.Pod{pod1})

	candidates, imbalanced := filter.FilterPodsWithPort(podList)

	// Should return all candidates without checking Redis
	assert.False(t, imbalanced)
	assert.Equal(t, 1, len(candidates))
}

func TestRedisImbalancePodsFilter_AddRequestCount(t *testing.T) {
	db, mock := redismock.NewClientMock()
	filter := NewRedisImbalancePodsFilter(db)

	pod := createTestPodImbalance("pod-1", "default", nil)
	ctx := types.NewRoutingContext(context.Background(), "", "test-model", "", "req-1", "")
	ctx.SetTargetPod(pod)
	ctx.SetTargetPort(8000)

	// Mock Redis HINCRBY
	mock.ExpectHIncrBy("aibrix:prefix-cache-reqcnt:{test-model}", "pod-1_8000", 1).SetVal(1)

	traceTerm := filter.AddRequestCount(ctx, "req-1", "test-model")

	assert.Equal(t, int64(1), traceTerm)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRedisImbalancePodsFilter_AddRequestCount_NoTargetPod(t *testing.T) {
	db, _ := redismock.NewClientMock()
	filter := NewRedisImbalancePodsFilter(db)

	ctx := types.NewRoutingContext(context.Background(), "", "test-model", "", "req-1", "")
	// No target pod set

	traceTerm := filter.AddRequestCount(ctx, "req-1", "test-model")

	// Should return 0 without calling Redis
	assert.Equal(t, int64(0), traceTerm)
}

func TestRedisImbalancePodsFilter_DoneRequestCount(t *testing.T) {
	db, mock := redismock.NewClientMock()
	filter := NewRedisImbalancePodsFilter(db)

	pod := createTestPodImbalance("pod-1", "default", nil)
	ctx := types.NewRoutingContext(context.Background(), "", "test-model", "", "req-1", "")
	ctx.SetTargetPod(pod)
	ctx.SetTargetPort(8000)

	// Mock Redis HINCRBY and HDEL
	mock.ExpectHIncrBy("aibrix:prefix-cache-reqcnt:{test-model}", "pod-1_8000", -1).SetVal(0)
	mock.ExpectHDel("aibrix:prefix-cache-reqcnt:{test-model}", "pod-1_8000").SetVal(1)

	filter.DoneRequestCount(ctx, "req-1", "test-model", 1)

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRedisImbalancePodsFilter_DoneRequestCount_PositiveCount(t *testing.T) {
	db, mock := redismock.NewClientMock()
	filter := NewRedisImbalancePodsFilter(db)

	pod := createTestPodImbalance("pod-1", "default", nil)
	ctx := types.NewRoutingContext(context.Background(), "", "test-model", "", "req-1", "")
	ctx.SetTargetPod(pod)
	ctx.SetTargetPort(8000)

	// Mock Redis HINCRBY - count remains positive
	mock.ExpectHIncrBy("aibrix:prefix-cache-reqcnt:{test-model}", "pod-1_8000", -1).SetVal(5)
	// Should not call HDEL when count > 0

	filter.DoneRequestCount(ctx, "req-1", "test-model", 1)

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRedisImbalancePodsFilter_FilterPods_Deduplication(t *testing.T) {
	db, mock := redismock.NewClientMock()
	filter := NewRedisImbalancePodsFilter(db)

	pod1 := createTestPodImbalanceWithPorts("pod-1", "default", map[string]string{"model": "test-model"}, 8000, 2)

	podList := createTestPodListImbalance([]*v1.Pod{pod1})

	// Mock Redis HGETALL response - both ports have data
	mock.ExpectHGetAll("aibrix:prefix-cache-reqcnt:{test-model}").SetVal(map[string]string{
		"pod-1_8000": "5",
		"pod-1_8001": "10",
	})

	result, imbalanced := filter.FilterPods(podList)

	// Should return imbalanced (10-5=5 > 2)
	assert.True(t, imbalanced)
	// Should deduplicate - only 1 pod returned
	assert.Equal(t, 1, len(result))
	assert.Equal(t, "pod-1", result[0].Name)

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRedisImbalancePodsFilter_BuildRedisKey(t *testing.T) {
	db, _ := redismock.NewClientMock()
	filter := NewRedisImbalancePodsFilter(db)

	key := filter.buildRedisKey("test-model")
	assert.Equal(t, "aibrix:prefix-cache-reqcnt:{test-model}", key)
}

// Edge cases
func TestLocalImbalancePodsFilter_EmptyPodList(t *testing.T) {
	mockProvider := newMockMetricsProvider()
	filter := NewLocalImbalancePodsFilter(mockProvider)

	podList := createTestPodListImbalance([]*v1.Pod{})

	candidates, imbalanced := filter.FilterPodsWithPort(podList)

	assert.False(t, imbalanced)
	assert.Equal(t, 0, len(candidates))
}

func TestLocalImbalancePodsFilter_SinglePod(t *testing.T) {
	mockProvider := newMockMetricsProvider()
	filter := NewLocalImbalancePodsFilter(mockProvider)

	pod1 := createTestPodImbalance("pod-1", "default", nil)
	mockProvider.requestCounts["pod-1"] = 10

	podList := createTestPodListImbalance([]*v1.Pod{pod1})

	candidates, imbalanced := filter.FilterPodsWithPort(podList)

	// Single pod cannot be imbalanced
	assert.False(t, imbalanced)
	assert.Equal(t, 1, len(candidates))
}
