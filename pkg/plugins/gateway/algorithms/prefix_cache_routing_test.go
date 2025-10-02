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

package routingalgorithms

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestGetTargetPodOnLoadImbalance tests load imbalance detection and pod selection
func TestGetTargetPodOnLoadImbalance(t *testing.T) {
	tests := []struct {
		name            string
		podCount        int
		requestCounts   map[string]int // pod name -> request count
		imbalanceCount  int            // threshold value
		expectImbalance bool
		expectPodPrefix string // Expected pod name prefix (e.g., "pod-0" for min count pod)
		description     string // Test description
	}{
		{
			name:            "no_imbalance_identical_counts",
			podCount:        4,
			requestCounts:   map[string]int{"pod-0": 10, "pod-1": 10, "pod-2": 10, "pod-3": 10},
			imbalanceCount:  8,
			expectImbalance: false,
			expectPodPrefix: "",
			description:     "Identical request counts should show no imbalance",
		},
		{
			name:            "imbalance_detected_single_min",
			podCount:        3,
			requestCounts:   map[string]int{"pod-0": 0, "pod-1": 10, "pod-2": 20},
			imbalanceCount:  8,
			expectImbalance: true,
			expectPodPrefix: "pod-0",
			description:     "Clear imbalance should select single minimum pod",
		},
		{
			name:            "imbalance_multiple_min_pods",
			podCount:        4,
			requestCounts:   map[string]int{"pod-0": 0, "pod-1": 0, "pod-2": 10, "pod-3": 20},
			imbalanceCount:  8,
			expectImbalance: true,
			expectPodPrefix: "pod-", // Any of pod-0 or pod-1
			description:     "Multiple minimum pods should select one of them",
		},
		{
			name:            "exactly_at_threshold",
			podCount:        2,
			requestCounts:   map[string]int{"pod-0": 0, "pod-1": 8},
			imbalanceCount:  8,
			expectImbalance: false,
			expectPodPrefix: "",
			description:     "Difference exactly at threshold should show no imbalance",
		},
		{
			name:            "just_above_threshold",
			podCount:        2,
			requestCounts:   map[string]int{"pod-0": 0, "pod-1": 9},
			imbalanceCount:  8,
			expectImbalance: true,
			expectPodPrefix: "pod-0",
			description:     "Difference just above threshold should trigger imbalance",
		},
		{
			name:            "empty_pods_list",
			podCount:        0,
			requestCounts:   map[string]int{},
			imbalanceCount:  8,
			expectImbalance: false,
			expectPodPrefix: "",
			description:     "Empty pods list should show no imbalance",
		},
		{
			name:            "single_pod_no_imbalance",
			podCount:        1,
			requestCounts:   map[string]int{"pod-0": 10},
			imbalanceCount:  8,
			expectImbalance: false,
			expectPodPrefix: "",
			description:     "Single pod should never show imbalance",
		},
		{
			name:            "all_zero_counts",
			podCount:        3,
			requestCounts:   map[string]int{"pod-0": 0, "pod-1": 0, "pod-2": 0},
			imbalanceCount:  8,
			expectImbalance: false,
			expectPodPrefix: "",
			description:     "All zero counts should show no imbalance",
		},
		{
			name:            "large_imbalance",
			podCount:        4,
			requestCounts:   map[string]int{"pod-0": 0, "pod-1": 5, "pod-2": 50, "pod-3": 100},
			imbalanceCount:  8,
			expectImbalance: true,
			expectPodPrefix: "pod-0",
			description:     "Large spread in request counts should trigger imbalance and select minimum",
		},
		{
			name:            "default_threshold_value_no_imbalance",
			podCount:        4,
			requestCounts:   map[string]int{"pod-0": 1, "pod-1": 2, "pod-2": 3, "pod-3": 9},
			imbalanceCount:  8, // Default threshold
			expectImbalance: false,
			expectPodPrefix: "",
			description:     "Pod running request count ≤ default abs value of 8 should show no imbalance",
		},
		{
			name:            "default_threshold_value_with_imbalance",
			podCount:        4,
			requestCounts:   map[string]int{"pod-0": 2, "pod-1": 2, "pod-2": 8, "pod-3": 16},
			imbalanceCount:  8, // Default threshold
			expectImbalance: true,
			expectPodPrefix: "pod-", // Any of pod-0 or pod-1
			description:     "Pod running request count > default abs value of 8 should trigger imbalance",
		},
	}

	// Save original value and restore after tests
	originalImbalanceCount := podRunningRequestImbalanceAbsCount
	defer func() {
		podRunningRequestImbalanceAbsCount = originalImbalanceCount
	}()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set the imbalance threshold for this test
			podRunningRequestImbalanceAbsCount = tt.imbalanceCount

			// Create test pods
			pods := make([]*v1.Pod, tt.podCount)
			for i := 0; i < tt.podCount; i++ {
				pods[i] = &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("pod-%d", i),
						Namespace: "default",
					},
					Status: v1.PodStatus{
						PodIP: fmt.Sprintf("10.0.0.%d", i+1),
						Conditions: []v1.PodCondition{
							{Type: v1.PodReady, Status: v1.ConditionTrue},
						},
					},
				}
			}

			// Setup metrics in cache
			metricsMap := make(map[string]map[string]metrics.MetricValue)
			for podName, reqCount := range tt.requestCounts {
				metricsMap[podName] = map[string]metrics.MetricValue{
					metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: float64(reqCount)},
				}
			}

			testCache := cache.NewWithPodsMetricsForTest(pods, "test-model", metricsMap)

			// Execute function
			targetPod, imbalance := getTargetPodOnLoadImbalance(testCache, pods)

			// Verify imbalance detection
			assert.Equal(t, tt.expectImbalance, imbalance, "Imbalance detection mismatch")

			// Verify target pod selection
			if tt.expectImbalance {
				assert.NotNil(t, targetPod, "Expected a target pod when imbalance detected")
				if tt.expectPodPrefix != "" && targetPod != nil {
					assert.Contains(t, targetPod.Name, tt.expectPodPrefix, "Unexpected target pod selected")
					// Verify it has minimum count
					minCount := tt.requestCounts[targetPod.Name]
					for _, count := range tt.requestCounts {
						assert.GreaterOrEqual(t, count, minCount, "Selected pod should have minimum request count")
					}
				}
			} else {
				assert.Nil(t, targetPod, "No target pod expected when no imbalance")
			}
		})
	}
}

// TestGetTargetPodFromMatchedPods tests pod selection from matched pods
func TestGetTargetPodFromMatchedPods(t *testing.T) {
	// Helper to create test setup
	createTestSetup := func(podMetrics map[string]int) (cache.Cache, []*v1.Pod) {
		var pods []*v1.Pod
		metricsMap := make(map[string]map[string]metrics.MetricValue)

		for podName, reqCount := range podMetrics {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: "default",
				},
				Status: v1.PodStatus{
					PodIP: fmt.Sprintf("10.0.0.%s", podName[len(podName)-1:]),
					Conditions: []v1.PodCondition{
						{Type: v1.PodReady, Status: v1.ConditionTrue},
					},
				},
			}
			pods = append(pods, pod)

			metricsMap[podName] = map[string]metrics.MetricValue{
				metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: float64(reqCount)},
			}
		}

		testCache := cache.NewWithPodsMetricsForTest(pods, "test-model", metricsMap)
		return testCache, pods
	}

	// Save original value and restore
	originalStdDevFactor := standardDeviationFactor
	defer func() {
		standardDeviationFactor = originalStdDevFactor
	}()

	tests := []struct {
		name         string
		podMetrics   map[string]int // pod name -> request count
		matchedPods  map[string]int // pod name -> match percentage
		stdDevFactor int
		expectedPod  string
		expectedNil  bool
		description  string
	}{
		{
			name:         "highest_match_within_stddev",
			podMetrics:   map[string]int{"pod-1": 5, "pod-2": 10, "pod-3": 15},
			matchedPods:  map[string]int{"pod-1": 100, "pod-2": 50, "pod-3": 25},
			stdDevFactor: 1,
			expectedPod:  "pod-1",
			description:  "Should select pod with highest match % when within std dev",
		},
		{
			name:         "skip_overloaded_pod_high_match",
			podMetrics:   map[string]int{"pod-1": 100, "pod-2": 10, "pod-3": 12},
			matchedPods:  map[string]int{"pod-1": 100, "pod-2": 80, "pod-3": 60},
			stdDevFactor: 1,
			expectedPod:  "pod-2",
			description:  "Should skip pod with highest match if overloaded",
		},
		{
			name:         "same_match_select_least_loaded",
			podMetrics:   map[string]int{"pod-1": 20, "pod-2": 10, "pod-3": 30},
			matchedPods:  map[string]int{"pod-1": 50, "pod-2": 50, "pod-3": 50},
			stdDevFactor: 1,
			expectedPod:  "pod-2",
			description:  "With same match %, should select least loaded",
		},
		{
			name:         "all_pods_exceed_stddev_select_best_match",
			podMetrics:   map[string]int{"pod-1": 11, "pod-2": 12, "pod-3": 13},
			matchedPods:  map[string]int{"pod-1": 90, "pod-2": 80, "pod-3": 70},
			stdDevFactor: -10, // Impossible threshold: mean=12, threshold becomes negative, all pods > threshold
			expectedNil:  true,
			description:  "When all exceed std dev, should return nil",
		},
		{
			name:         "empty_matched_pods",
			podMetrics:   map[string]int{"pod-1": 10, "pod-2": 20},
			matchedPods:  map[string]int{},
			stdDevFactor: 1,
			expectedNil:  true,
			description:  "Empty matched pods should return nil",
		},
		{
			name:         "single_matched_pod_within_threshold",
			podMetrics:   map[string]int{"pod-1": 10, "pod-2": 20},
			matchedPods:  map[string]int{"pod-1": 75},
			stdDevFactor: 2,
			expectedPod:  "pod-1",
			description:  "Single matched pod within threshold",
		},
		{
			name:         "prefer_higher_match_with_acceptable_load",
			podMetrics:   map[string]int{"pod-1": 11, "pod-2": 10, "pod-3": 12},
			matchedPods:  map[string]int{"pod-1": 100, "pod-2": 90, "pod-3": 80},
			stdDevFactor: 5,
			expectedPod:  "pod-1",
			description:  "Should prefer higher match when load difference is minimal",
		},
		{
			name:         "match_pod_with_highest_prefix_match_percent",
			podMetrics:   map[string]int{"pod-1": 1, "pod-2": 2, "pod-3": 3, "pod-4": 4},
			matchedPods:  map[string]int{"pod-1": 50, "pod-2": 60},
			stdDevFactor: 1,
			expectedPod:  "pod-2", // Should select highest match % (60%)
			description:  "Should select pod with highest prefix match percent",
		},
		{
			name:         "lowest_running_request_count_for_same_prefix_match",
			podMetrics:   map[string]int{"pod-1": 1, "pod-2": 2, "pod-3": 3, "pod-4": 4},
			matchedPods:  map[string]int{"pod-1": 50, "pod-2": 50, "pod-3": 50},
			stdDevFactor: 1,
			expectedPod:  "pod-1", // All have 50% match, pod-1 has lowest load (1)
			description:  "Should select pod with lowest running request count for same prefix match percent",
		},
		{
			name:         "any_pod_with_same_request_count_and_match_percent",
			podMetrics:   map[string]int{"pod-1": 1, "pod-2": 1, "pod-3": 1, "pod-4": 4},
			matchedPods:  map[string]int{"pod-1": 50, "pod-2": 50, "pod-3": 50},
			stdDevFactor: 1,
			expectedPod:  "", // Any of pod-1, pod-2, pod-3 (will be handled in test)
			description:  "Should select any pod with same running request count and same prefix match percent",
		},
		{
			name:         "lower_prefix_match_with_requests_below_threshold",
			podMetrics:   map[string]int{"pod-1": 1, "pod-2": 10, "pod-3": 1, "pod-4": 4},
			matchedPods:  map[string]int{"pod-1": 50, "pod-2": 100},
			stdDevFactor: 1,
			expectedPod:  "pod-1", // pod-2 is overloaded (10), so select pod-1 with lower match but acceptable load
			description:  "Should select pod with lower prefix match percent when other has requests below imbalance threshold",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			standardDeviationFactor = tt.stdDevFactor

			testCache, pods := createTestSetup(tt.podMetrics)
			result := getTargetPodFromMatchedPods(testCache, pods, tt.matchedPods)

			if tt.expectedNil {
				assert.Nil(t, result, tt.description)
			} else {
				assert.NotNil(t, result, "Expected non-nil result")
				if result != nil {
					if tt.expectedPod == "" {
						// Special case: any pod with same characteristics is acceptable
						// For "any_pod_with_same_request_count_and_match_percent" test
						acceptablePods := []string{"pod-1", "pod-2", "pod-3"}
						assert.Contains(t, acceptablePods, result.Name, tt.description)
					} else {
						assert.Equal(t, tt.expectedPod, result.Name, tt.description)
					}
				}
			}
		})
	}
}

// TestGetRequestCounts tests request count retrieval with various scenarios
func TestGetRequestCounts(t *testing.T) {
	tests := []struct {
		name           string
		podMetrics     map[string]map[string]metrics.MetricValue
		expectedCounts map[string]int
		description    string
	}{
		{
			name: "normal_metrics_retrieval",
			podMetrics: map[string]map[string]metrics.MetricValue{
				"pod-1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 10}},
				"pod-2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 20}},
				"pod-3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 30}},
			},
			expectedCounts: map[string]int{"pod-1": 10, "pod-2": 20, "pod-3": 30},
			description:    "Should retrieve all metrics correctly",
		},
		{
			name: "missing_metrics_defaults_to_zero",
			podMetrics: map[string]map[string]metrics.MetricValue{
				"pod-1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 15}},
				"pod-2": {}, // No metrics
				"pod-3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 25}},
			},
			expectedCounts: map[string]int{"pod-1": 15, "pod-2": 0, "pod-3": 25},
			description:    "Missing metrics should default to 0",
		},
		{
			name:           "empty_pods_list",
			podMetrics:     map[string]map[string]metrics.MetricValue{},
			expectedCounts: map[string]int{},
			description:    "Empty pods should return empty counts",
		},
		{
			name: "all_zero_metrics",
			podMetrics: map[string]map[string]metrics.MetricValue{
				"pod-1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
				"pod-2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
			},
			expectedCounts: map[string]int{"pod-1": 0, "pod-2": 0},
			description:    "Zero metrics should be preserved",
		},
		{
			name: "decimal_values_truncated",
			podMetrics: map[string]map[string]metrics.MetricValue{
				"pod-1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 10.7}},
				"pod-2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 20.3}},
			},
			expectedCounts: map[string]int{"pod-1": 10, "pod-2": 20},
			description:    "Decimal values should be truncated to int",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create pods based on metrics keys
			var pods []*v1.Pod
			for podName := range tt.podMetrics {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: "default",
					},
				}
				pods = append(pods, pod)
			}

			// Create cache with metrics
			testCache := cache.NewWithPodsMetricsForTest(pods, "test-model", tt.podMetrics)

			// Get request counts
			result := getRequestCounts(testCache, pods)

			// Verify results
			assert.Equal(t, tt.expectedCounts, result, tt.description)
		})
	}
}

// TestGetRequestCountsWithKeysHelper tests the key-based version of request counts
func TestGetRequestCountsWithKeysHelper(t *testing.T) {
	tests := []struct {
		name           string
		pods           []string
		namespaces     []string // Different namespace per pod
		podMetrics     map[string]map[string]metrics.MetricValue
		expectedCounts map[string]int // namespace/name -> count
		description    string
	}{
		{
			name:       "normal_with_namespace",
			pods:       []string{"pod-1", "pod-2"},
			namespaces: []string{"test-ns", "test-ns"},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"pod-1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 10}},
				"pod-2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 20}},
			},
			expectedCounts: map[string]int{
				"test-ns/pod-1": 10,
				"test-ns/pod-2": 20,
			},
			description: "Should format keys as namespace/name",
		},
		{
			name:       "mixed_namespaces",
			pods:       []string{"pod1", "pod2"},
			namespaces: []string{"default", "kube-system"},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"pod1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 5}},
				"pod2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 10}},
			},
			expectedCounts: map[string]int{
				"default/pod1":     5,
				"kube-system/pod2": 10,
			},
			description: "Should handle pods in different namespaces",
		},
		{
			name:       "missing_metrics_defaults_to_zero",
			pods:       []string{"pod-1", "pod-2"},
			namespaces: []string{"ns1", "ns1"},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"pod-1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 15}},
				// pod-2 missing metrics
			},
			expectedCounts: map[string]int{
				"ns1/pod-1": 15,
				"ns1/pod-2": 0, // Should default to 0
			},
			description: "Missing pod metrics should default to 0",
		},
		{
			name:           "empty_pods_list",
			pods:           []string{},
			namespaces:     []string{},
			podMetrics:     map[string]map[string]metrics.MetricValue{},
			expectedCounts: map[string]int{},
			description:    "Empty pods list should return empty counts",
		},
		{
			name:       "pod_name_collision_different_namespaces",
			pods:       []string{"worker", "worker"},
			namespaces: []string{"prod", "staging"},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"worker": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 42}}, // Both pods will get same metric
			},
			expectedCounts: map[string]int{
				"prod/worker":    42,
				"staging/worker": 42,
			},
			description: "Same pod names in different namespaces should be handled correctly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create pods with specified namespaces
			var pods []*v1.Pod
			for i, podName := range tt.pods {
				namespace := "default"
				if i < len(tt.namespaces) {
					namespace = tt.namespaces[i]
				}
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: namespace,
					},
				}
				pods = append(pods, pod)
			}

			// Create cache
			testCache := cache.NewWithPodsMetricsForTest(pods, "test-model", tt.podMetrics)

			// Get request counts with keys
			result := getRequestCountsWithKeys(testCache, pods)

			// Verify
			assert.Equal(t, tt.expectedCounts, result, tt.description)
		})
	}
}

// TestSelectTargetPodWithLeastRequestCount tests the fallback selection
func TestSelectTargetPodWithLeastRequestCount(t *testing.T) {
	tests := []struct {
		name         string
		podMetrics   map[string]int
		expectedPods []string // List of acceptable pod names (for tie cases)
		description  string
	}{
		{
			name:         "single_minimum",
			podMetrics:   map[string]int{"pod-1": 10, "pod-2": 5, "pod-3": 15},
			expectedPods: []string{"pod-2"},
			description:  "Should select pod with lowest count",
		},
		{
			name:         "multiple_minimums",
			podMetrics:   map[string]int{"pod-1": 5, "pod-2": 5, "pod-3": 10},
			expectedPods: []string{"pod-1", "pod-2"},
			description:  "Should randomly select from pods with same min count",
		},
		{
			name:         "all_same_count",
			podMetrics:   map[string]int{"pod-1": 10, "pod-2": 10, "pod-3": 10},
			expectedPods: []string{"pod-1", "pod-2", "pod-3"},
			description:  "Should randomly select when all have same count",
		},
		{
			name:         "single_pod",
			podMetrics:   map[string]int{"pod-1": 100},
			expectedPods: []string{"pod-1"},
			description:  "Should select the only pod",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create pods and cache
			var pods []*v1.Pod
			metricsMap := make(map[string]map[string]metrics.MetricValue)

			for podName, reqCount := range tt.podMetrics {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: "default",
					},
				}
				pods = append(pods, pod)
				metricsMap[podName] = map[string]metrics.MetricValue{
					metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: float64(reqCount)},
				}
			}

			testCache := cache.NewWithPodsMetricsForTest(pods, "test-model", metricsMap)

			// Test function
			result := selectTargetPodWithLeastRequestCount(testCache, pods)

			// Verify
			assert.NotNil(t, result, "Should always return a pod when pods exist")
			if result != nil {
				assert.Contains(t, tt.expectedPods, result.Name, tt.description)
			}
		})
	}
}

// TestLoadImbalanceEdgeCases tests extreme edge cases for load imbalance detection
func TestLoadImbalanceEdgeCases(t *testing.T) {
	tests := []struct {
		name            string
		podCount        int
		requestCounts   map[string]int
		imbalanceCount  int
		expectImbalance bool
		expectPodPrefix string
		description     string
	}{
		{
			name:            "max_int_values_no_overflow",
			podCount:        2,
			requestCounts:   map[string]int{"pod-0": math.MaxInt32, "pod-1": math.MaxInt32 - 1},
			imbalanceCount:  8,
			expectImbalance: false, // Difference = 1, below threshold
			expectPodPrefix: "",
			description:     "Should handle MaxInt32 values without overflow",
		},
		{
			name:            "max_int_imbalance_detected",
			podCount:        2,
			requestCounts:   map[string]int{"pod-0": math.MaxInt32 - 20, "pod-1": math.MaxInt32},
			imbalanceCount:  8,
			expectImbalance: true, // Difference = 20, above threshold
			expectPodPrefix: "pod-0",
			description:     "Should detect imbalance with large MaxInt32 values",
		},
		{
			name:            "min_int_values_protection",
			podCount:        2,
			requestCounts:   map[string]int{"pod-0": math.MinInt32, "pod-1": 0},
			imbalanceCount:  8,
			expectImbalance: true, // Massive difference
			expectPodPrefix: "pod-0",
			description:     "Should handle MinInt32 values correctly",
		},
		{
			name:            "zero_threshold_no_imbalance",
			podCount:        3,
			requestCounts:   map[string]int{"pod-0": 5, "pod-1": 10, "pod-2": 15},
			imbalanceCount:  0,    // Zero threshold - no imbalance allowed
			expectImbalance: true, // Any difference triggers imbalance
			expectPodPrefix: "pod-0",
			description:     "Zero threshold should trigger imbalance for any difference",
		},
		{
			name:            "negative_threshold_no_imbalance",
			podCount:        2,
			requestCounts:   map[string]int{"pod-0": 0, "pod-1": 100},
			imbalanceCount:  -1,   // Negative threshold (invalid)
			expectImbalance: true, // Large difference should still trigger
			expectPodPrefix: "pod-0",
			description:     "Negative threshold should not prevent imbalance detection",
		},
		{
			name:            "all_pods_same_max_value",
			podCount:        3,
			requestCounts:   map[string]int{"pod-0": math.MaxInt32, "pod-1": math.MaxInt32, "pod-2": math.MaxInt32},
			imbalanceCount:  8,
			expectImbalance: false, // No difference
			expectPodPrefix: "",
			description:     "Identical MaxInt32 values should show no imbalance",
		},
		{
			name:            "mixed_extreme_values",
			podCount:        3,
			requestCounts:   map[string]int{"pod-0": math.MinInt32, "pod-1": 0, "pod-2": math.MaxInt32},
			imbalanceCount:  8,
			expectImbalance: true, // Extreme spread
			expectPodPrefix: "pod-0",
			description:     "Mixed extreme values should select minimum correctly",
		},
	}

	// Save original value and restore after tests
	originalImbalanceCount := podRunningRequestImbalanceAbsCount
	defer func() {
		podRunningRequestImbalanceAbsCount = originalImbalanceCount
	}()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set the imbalance threshold for this test
			podRunningRequestImbalanceAbsCount = tt.imbalanceCount

			// Create test pods
			pods := make([]*v1.Pod, tt.podCount)
			for i := 0; i < tt.podCount; i++ {
				pods[i] = &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("pod-%d", i),
						Namespace: "default",
					},
					Status: v1.PodStatus{
						PodIP: fmt.Sprintf("10.0.0.%d", i+1),
						Conditions: []v1.PodCondition{
							{Type: v1.PodReady, Status: v1.ConditionTrue},
						},
					},
				}
			}

			// Setup metrics in cache
			metricsMap := make(map[string]map[string]metrics.MetricValue)
			for podName, reqCount := range tt.requestCounts {
				metricsMap[podName] = map[string]metrics.MetricValue{
					metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: float64(reqCount)},
				}
			}

			testCache := cache.NewWithPodsMetricsForTest(pods, "test-model", metricsMap)

			// Execute function
			targetPod, imbalance := getTargetPodOnLoadImbalance(testCache, pods)

			// Verify imbalance detection
			assert.Equal(t, tt.expectImbalance, imbalance, "Imbalance detection mismatch: %s", tt.description)

			// Verify target pod selection
			if tt.expectImbalance {
				assert.NotNil(t, targetPod, "Expected a target pod when imbalance detected: %s", tt.description)
				if tt.expectPodPrefix != "" && targetPod != nil {
					assert.Contains(t, targetPod.Name, tt.expectPodPrefix, "Unexpected target pod selected: %s", tt.description)
					// Verify it has minimum count
					minCount := tt.requestCounts[targetPod.Name]
					for _, count := range tt.requestCounts {
						assert.GreaterOrEqual(t, count, minCount, "Selected pod should have minimum request count: %s", tt.description)
					}
				}
			} else {
				assert.Nil(t, targetPod, "No target pod expected when no imbalance: %s", tt.description)
			}
		})
	}

}

// TestPrefixMatchingStandardDeviationEdgeCases tests edge cases for standard deviation handling
func TestPrefixMatchingStandardDeviationEdgeCases(t *testing.T) {
	// Helper to create test setup
	createTestSetup := func(podMetrics map[string]int) (cache.Cache, []*v1.Pod) {
		var pods []*v1.Pod
		metricsMap := make(map[string]map[string]metrics.MetricValue)

		for podName, reqCount := range podMetrics {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: "default",
				},
				Status: v1.PodStatus{
					PodIP: fmt.Sprintf("10.0.0.%s", podName[len(podName)-1:]),
					Conditions: []v1.PodCondition{
						{Type: v1.PodReady, Status: v1.ConditionTrue},
					},
				},
			}
			pods = append(pods, pod)

			metricsMap[podName] = map[string]metrics.MetricValue{
				metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: float64(reqCount)},
			}
		}

		testCache := cache.NewWithPodsMetricsForTest(pods, "test-model", metricsMap)
		return testCache, pods
	}

	// Save original value and restore
	originalStdDevFactor := standardDeviationFactor
	defer func() {
		standardDeviationFactor = originalStdDevFactor
	}()

	tests := []struct {
		name         string
		podMetrics   map[string]int // pod name -> request count
		matchedPods  map[string]int // pod name -> match percentage
		stdDevFactor int
		expectedPod  string
		expectedNil  bool
		description  string
	}{
		{
			name:         "zero_standard_deviation_factor",
			podMetrics:   map[string]int{"pod-1": 10, "pod-2": 15, "pod-3": 20},
			matchedPods:  map[string]int{"pod-1": 100, "pod-2": 90, "pod-3": 80},
			stdDevFactor: 0,       // No tolerance - only mean
			expectedPod:  "pod-1", // Should still select best match within mean
			description:  "Zero stddev factor should use strict mean threshold",
		},
		{
			name:         "negative_standard_deviation_factor",
			podMetrics:   map[string]int{"pod-1": 10, "pod-2": 10, "pod-3": 10},
			matchedPods:  map[string]int{"pod-1": 100, "pod-2": 90, "pod-3": 80},
			stdDevFactor: -1,      // Invalid negative
			expectedPod:  "pod-1", // Should still select best match
			description:  "Negative stddev factor should be handled gracefully",
		},
		{
			name:         "very_large_standard_deviation_factor",
			podMetrics:   map[string]int{"pod-1": 1000, "pod-2": 10, "pod-3": 15},
			matchedPods:  map[string]int{"pod-1": 100, "pod-2": 90, "pod-3": 80},
			stdDevFactor: 1000,    // Very large tolerance
			expectedPod:  "pod-1", // Should select highest match even if overloaded
			description:  "Large stddev factor should allow overloaded pods",
		},
		{
			name:         "all_pods_within_threshold_with_factor_1",
			podMetrics:   map[string]int{"pod-1": 100, "pod-2": 101, "pod-3": 102},
			matchedPods:  map[string]int{"pod-1": 90, "pod-2": 80, "pod-3": 70},
			stdDevFactor: 1,       // Mean=101, StdDev=1, Threshold=102 - all pods ≤ 102
			expectedPod:  "pod-1", // Should select highest match % (90%)
			description:  "When all pods are within threshold, should select highest match",
		},
		{
			name:         "all_pods_exceed_threshold_tight_bound",
			podMetrics:   map[string]int{"pod-1": 200, "pod-2": 201, "pod-3": 202},
			matchedPods:  map[string]int{"pod-1": 90, "pod-2": 80, "pod-3": 70},
			stdDevFactor: 0,     // Mean=201, StdDev=1, Threshold=201 - only pod-1 within
			expectedNil:  false, // pod-1 (200) should be within threshold (≤201)
			expectedPod:  "pod-1",
			description:  "With zero stddev factor, only pods ≤ mean should be selected",
		},
		{
			name:         "identical_match_percentages_different_loads",
			podMetrics:   map[string]int{"pod-1": 50, "pod-2": 10, "pod-3": 30},
			matchedPods:  map[string]int{"pod-1": 75, "pod-2": 75, "pod-3": 75},
			stdDevFactor: 2,       // Generous tolerance
			expectedPod:  "pod-2", // Should select least loaded with same match
			description:  "Identical match % should select least loaded",
		},
		{
			name:         "single_pod_match_within_threshold",
			podMetrics:   map[string]int{"pod-1": 5, "pod-2": 50, "pod-3": 60},
			matchedPods:  map[string]int{"pod-2": 80}, // Only one pod matches
			stdDevFactor: 2,
			expectedPod:  "pod-2", // Should select the only matching pod if within threshold
			description:  "Single matching pod within threshold should be selected",
		},
		{
			name:         "single_pod_match_exceeds_threshold",
			podMetrics:   map[string]int{"pod-1": 5, "pod-2": 200, "pod-3": 10}, // pod-2 heavily loaded
			matchedPods:  map[string]int{"pod-2": 100},                          // Only one pod matches but overloaded
			stdDevFactor: 1,
			expectedNil:  true, // Should reject overloaded pod
			description:  "Single matching pod exceeding threshold should be rejected",
		},
		{
			name:         "extreme_load_imbalance_high_match",
			podMetrics:   map[string]int{"pod-1": math.MaxInt32, "pod-2": 0, "pod-3": 5},
			matchedPods:  map[string]int{"pod-1": 100, "pod-2": 50, "pod-3": 25},
			stdDevFactor: 1,
			expectedPod:  "pod-2", // pod-2 and pod-3 both within threshold, pod-2 has higher match (50% > 25%)
			description:  "With extreme imbalance, should select highest match among low-load pods",
		},
		{
			name:         "zero_variance_request_counts",
			podMetrics:   map[string]int{"pod-1": 10, "pod-2": 10, "pod-3": 10}, // Identical loads
			matchedPods:  map[string]int{"pod-1": 90, "pod-2": 80, "pod-3": 70},
			stdDevFactor: 1,
			expectedPod:  "pod-1", // With zero variance, all pods are within threshold
			description:  "Zero variance should allow all pods, select highest match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			standardDeviationFactor = tt.stdDevFactor

			testCache, pods := createTestSetup(tt.podMetrics)
			result := getTargetPodFromMatchedPods(testCache, pods, tt.matchedPods)

			if tt.expectedNil {
				assert.Nil(t, result, tt.description)
			} else {
				assert.NotNil(t, result, "Expected non-nil result: %s", tt.description)
				if result != nil {
					assert.Equal(t, tt.expectedPod, result.Name, tt.description)
				}
			}
		})
	}
}

// TestKVSyncPodKeyHandlingEdgeCases tests edge cases specific to KV sync pod key handling
func TestKVSyncPodKeyHandlingEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		pods        []string
		namespaces  []string
		podMetrics  map[string]int // pod name -> request count
		matchedPods map[string]int // pod key (namespace/name) -> match percentage
		expectedPod string
		expectedNil bool
		description string
	}{
		{
			name:       "mixed_namespaces_key_formatting",
			pods:       []string{"pod-1", "pod-2", "pod-3"},
			namespaces: []string{"ns-a", "ns-b", "ns-a"},
			podMetrics: map[string]int{"pod-1": 10, "pod-2": 15, "pod-3": 20},
			matchedPods: map[string]int{
				"ns-a/pod-1": 100,
				"ns-b/pod-2": 80,
				"ns-a/pod-3": 60,
			},
			expectedPod: "pod-1", // Should select highest match with proper key mapping
			description: "Mixed namespaces should map pod keys correctly",
		},
		{
			name:       "namespace_collision_different_pods",
			pods:       []string{"pod-1", "pod-1"}, // Same name, different namespaces
			namespaces: []string{"ns-a", "ns-b"},
			podMetrics: map[string]int{"pod-1": 10}, // Only one in metrics (simulates metric key collision)
			matchedPods: map[string]int{
				"ns-a/pod-1": 90,
				"ns-b/pod-1": 80,
			},
			expectedPod: "pod-1", // Should handle namespace collision gracefully
			description: "Same pod names in different namespaces should be handled",
		},
		{
			name:        "missing_pod_key_in_matched_pods",
			pods:        []string{"pod-1", "pod-2"},
			namespaces:  []string{"default", "default"},
			podMetrics:  map[string]int{"pod-1": 5, "pod-2": 10},
			matchedPods: map[string]int{"default/pod-3": 100}, // Non-existent pod
			expectedNil: true,                                 // Should return nil for missing pods
			description: "Missing pod keys should result in nil selection",
		},
		{
			name:        "empty_matched_pods_kv_sync",
			pods:        []string{"pod-1", "pod-2"},
			namespaces:  []string{"default", "default"},
			podMetrics:  map[string]int{"pod-1": 5, "pod-2": 10},
			matchedPods: map[string]int{}, // Empty matches
			expectedNil: true,
			description: "Empty matched pods should return nil in KV sync path",
		},
		{
			name:       "long_namespace_names",
			pods:       []string{"pod-1", "pod-2"},
			namespaces: []string{"very-long-namespace-name-that-exceeds-normal-length", "default"},
			podMetrics: map[string]int{"pod-1": 5, "pod-2": 10},
			matchedPods: map[string]int{
				"very-long-namespace-name-that-exceeds-normal-length/pod-1": 100,
				"default/pod-2": 80,
			},
			expectedPod: "pod-1", // Should select highest match % with proper key mapping
			description: "Long namespace names should be handled correctly with multiple pods",
		},
	}

	// Save original value and restore
	originalStdDevFactor := standardDeviationFactor
	defer func() {
		standardDeviationFactor = originalStdDevFactor
	}()
	standardDeviationFactor = 2 // Use generous factor for these tests

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create pods with specified namespaces
			var pods []*v1.Pod
			metricsMap := make(map[string]map[string]metrics.MetricValue)

			for i, podName := range tt.pods {
				namespace := "default"
				if i < len(tt.namespaces) {
					namespace = tt.namespaces[i]
				}

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: namespace,
					},
					Status: v1.PodStatus{
						PodIP: fmt.Sprintf("10.0.0.%d", i+1),
						Conditions: []v1.PodCondition{
							{Type: v1.PodReady, Status: v1.ConditionTrue},
						},
					},
				}
				pods = append(pods, pod)

				// Add metrics if specified
				if reqCount, exists := tt.podMetrics[podName]; exists {
					metricsMap[podName] = map[string]metrics.MetricValue{
						metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: float64(reqCount)},
					}
				}
			}

			testCache := cache.NewWithPodsMetricsForTest(pods, "test-model", metricsMap)

			// Test KV sync function
			result := getTargetPodFromMatchedPodsWithKeys(testCache, pods, tt.matchedPods)

			if tt.expectedNil {
				assert.Nil(t, result, tt.description)
			} else {
				assert.NotNil(t, result, "Expected non-nil result: %s", tt.description)
				if result != nil {
					assert.Equal(t, tt.expectedPod, result.Name, tt.description)
				}
			}
		})
	}
}

// TestSelectPodWithLeastRequestCountEdgeCases tests edge cases for KV sync fallback selection
func TestSelectPodWithLeastRequestCountEdgeCases(t *testing.T) {
	tests := []struct {
		name         string
		podMetrics   map[string]int
		expectedPods []string // List of acceptable pod names
		expectNil    bool
		description  string
	}{
		{
			name:         "max_int_request_counts",
			podMetrics:   map[string]int{"pod-1": math.MaxInt32, "pod-2": math.MaxInt32 - 1, "pod-3": math.MaxInt32},
			expectedPods: []string{"pod-2"}, // Should select the one with MaxInt32-1
			description:  "Should handle MaxInt32 values and select minimum",
		},
		{
			name:         "all_max_int_identical",
			podMetrics:   map[string]int{"pod-1": math.MaxInt32, "pod-2": math.MaxInt32, "pod-3": math.MaxInt32},
			expectedPods: []string{"pod-1", "pod-2", "pod-3"}, // Any is acceptable
			description:  "All MaxInt32 values should allow random selection",
		},
		{
			name:         "min_int_values",
			podMetrics:   map[string]int{"pod-1": math.MinInt32, "pod-2": 0, "pod-3": 10},
			expectedPods: []string{"pod-1"}, // MinInt32 is the minimum
			description:  "Should handle MinInt32 values correctly",
		},
		{
			name:        "empty_pod_list_kv_sync",
			podMetrics:  map[string]int{},
			expectNil:   true,
			description: "Empty pod list should return nil",
		},
		{
			name:         "single_pod_max_load",
			podMetrics:   map[string]int{"pod-1": math.MaxInt32},
			expectedPods: []string{"pod-1"},
			description:  "Single pod with max load should still be selected",
		},
		{
			name:         "mixed_extreme_values_selection",
			podMetrics:   map[string]int{"pod-1": math.MinInt32, "pod-2": math.MaxInt32, "pod-3": 0},
			expectedPods: []string{"pod-1"}, // MinInt32 is minimum
			description:  "Mixed extreme values should select absolute minimum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create pods and cache
			var pods []*v1.Pod
			metricsMap := make(map[string]map[string]metrics.MetricValue)

			for podName, reqCount := range tt.podMetrics {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: "default",
					},
				}
				pods = append(pods, pod)
				metricsMap[podName] = map[string]metrics.MetricValue{
					metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: float64(reqCount)},
				}
			}

			testCache := cache.NewWithPodsMetricsForTest(pods, "test-model", metricsMap)

			// Test function
			result := selectPodWithLeastRequestCount(testCache, pods)

			if tt.expectNil {
				assert.Nil(t, result, tt.description)
			} else {
				assert.NotNil(t, result, "Expected non-nil result: %s", tt.description)
				if result != nil {
					assert.Contains(t, tt.expectedPods, result.Name, tt.description)
				}
			}
		})
	}
}
