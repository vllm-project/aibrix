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

package modeladapter

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
)

func TestIsRetriableError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "Connection refused should be retriable",
			err:      fmt.Errorf("connection refused"),
			expected: true,
		},
		{
			name:     "Timeout should be retriable",
			err:      fmt.Errorf("request timeout occurred"),
			expected: true,
		},
		{
			name:     "Connection reset should be retriable",
			err:      fmt.Errorf("connection reset by peer"),
			expected: true,
		},
		{
			name:     "Network unreachable should be retriable",
			err:      fmt.Errorf("network is unreachable"),
			expected: true,
		},
		{
			name:     "Service unavailable should be retriable",
			err:      fmt.Errorf("service unavailable"),
			expected: true,
		},
		{
			name:     "Bad gateway should be retriable",
			err:      fmt.Errorf("bad gateway"),
			expected: true,
		},
		{
			name:     "Generic error should not be retriable",
			err:      fmt.Errorf("invalid JSON format"),
			expected: false,
		},
		{
			name:     "Authentication error should not be retriable",
			err:      fmt.Errorf("authentication failed"),
			expected: false,
		},
		{
			name:     "Nil error should not be retriable",
			err:      nil,
			expected: false,
		},
	}

	reconciler := &ModelAdapterReconciler{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := reconciler.isRetriableError(tt.err)
			if result != tt.expected {
				t.Errorf("isRetriableError() = %v, want %v for error: %v", result, tt.expected, tt.err)
			}
		})
	}
}

func TestRetryInfoManagement(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = modelv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(10)

	reconciler := &ModelAdapterReconciler{
		Client:        client,
		Scheme:        scheme,
		Recorder:      recorder,
		RuntimeConfig: config.RuntimeConfig{},
	}

	t.Run("Initial state should have zero values", func(t *testing.T) {
		instance := &modelv1alpha1.ModelAdapter{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-adapter",
				Namespace: "default",
			},
		}

		count, lastTime := reconciler.getRetryInfo(instance, "test-pod")
		if count != 0 {
			t.Errorf("Expected retry count 0, got %d", count)
		}
		if !lastTime.IsZero() {
			t.Errorf("Expected zero time, got %v", lastTime)
		}
	})

	t.Run("Should store and retrieve retry count correctly", func(t *testing.T) {
		instance := &modelv1alpha1.ModelAdapter{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-adapter",
				Namespace: "default",
			},
		}

		reconciler.updateRetryInfo(instance, "test-pod", 3)

		count, lastTime := reconciler.getRetryInfo(instance, "test-pod")
		if count != 3 {
			t.Errorf("Expected retry count 3, got %d", count)
		}
		if lastTime.IsZero() {
			t.Errorf("Expected non-zero time, got zero time")
		}

		// Check that annotations are set correctly
		expectedCountKey := fmt.Sprintf("%s/test-pod", RetryCountAnnotationKey)
		expectedTimeKey := fmt.Sprintf("%s/test-pod", LastRetryTimeAnnotationKey)

		if instance.Annotations == nil {
			t.Errorf("Expected annotations to be set, but got nil")
		}

		if _, exists := instance.Annotations[expectedCountKey]; !exists {
			t.Errorf("Expected retry count annotation to exist")
		}

		if _, exists := instance.Annotations[expectedTimeKey]; !exists {
			t.Errorf("Expected retry time annotation to exist")
		}
	})

	t.Run("Should clear retry info correctly", func(t *testing.T) {
		instance := &modelv1alpha1.ModelAdapter{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-adapter",
				Namespace: "default",
				Annotations: map[string]string{
					fmt.Sprintf("%s/test-pod", RetryCountAnnotationKey):    "5",
					fmt.Sprintf("%s/test-pod", LastRetryTimeAnnotationKey): time.Now().Format(time.RFC3339),
					"custom/annotation": "should-remain",
				},
			},
		}

		reconciler.clearRetryInfo(instance, "test-pod")

		count, lastTime := reconciler.getRetryInfo(instance, "test-pod")
		if count != 0 {
			t.Errorf("Expected retry count 0 after clearing, got %d", count)
		}
		if !lastTime.IsZero() {
			t.Errorf("Expected zero time after clearing, got %v", lastTime)
		}

		// Check that custom annotation remains
		if customValue, exists := instance.Annotations["custom/annotation"]; !exists || customValue != "should-remain" {
			t.Errorf("Expected custom annotation to remain unchanged")
		}
	})

	t.Run("Should handle multiple pods independently", func(t *testing.T) {
		instance := &modelv1alpha1.ModelAdapter{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-adapter",
				Namespace: "default",
			},
		}

		reconciler.updateRetryInfo(instance, "pod1", 2)
		reconciler.updateRetryInfo(instance, "pod2", 5)

		count1, _ := reconciler.getRetryInfo(instance, "pod1")
		count2, _ := reconciler.getRetryInfo(instance, "pod2")

		if count1 != 2 {
			t.Errorf("Expected retry count 2 for pod1, got %d", count1)
		}
		if count2 != 5 {
			t.Errorf("Expected retry count 5 for pod2, got %d", count2)
		}
	})
}

func TestIsPodReadyForScheduling(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = modelv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(10)

	reconciler := &ModelAdapterReconciler{
		Client:        client,
		Scheme:        scheme,
		Recorder:      recorder,
		RuntimeConfig: config.RuntimeConfig{},
	}

	ctx := context.Background()
	instance := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-adapter",
			Namespace: "default",
		},
	}

	t.Run("Should return false for non-ready pods", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionFalse,
					},
				},
			},
		}

		result := reconciler.isPodReadyForScheduling(ctx, instance, pod)
		if result {
			t.Errorf("Expected false for non-ready pod, got %v", result)
		}
	})

	t.Run("Should return false for recently ready pods", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:               corev1.PodReady,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.NewTime(time.Now().Add(-1 * time.Second)), // Very recent
					},
				},
			},
		}

		result := reconciler.isPodReadyForScheduling(ctx, instance, pod)
		if result {
			t.Errorf("Expected false for recently ready pod, got %v", result)
		}
	})

	t.Run("Should return true for stable ready pods", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:               corev1.PodReady,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.NewTime(time.Now().Add(-30 * time.Second)), // Well established
					},
				},
			},
		}

		result := reconciler.isPodReadyForScheduling(ctx, instance, pod)
		if !result {
			t.Errorf("Expected true for stable ready pod, got %v", result)
		}
	})
}

func TestIsPodHealthy(t *testing.T) {
	reconciler := &ModelAdapterReconciler{}

	t.Run("Should return false for terminating pods", func(t *testing.T) {
		now := metav1.Now()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-pod",
				Namespace:         "default",
				DeletionTimestamp: &now,
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					},
				},
			},
		}

		result := reconciler.isPodHealthy(pod)
		if result {
			t.Errorf("Expected false for terminating pod, got %v", result)
		}
	})

	t.Run("Should return false for non-ready pods", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionFalse,
					},
				},
			},
		}

		result := reconciler.isPodHealthy(pod)
		if result {
			t.Errorf("Expected false for non-ready pod, got %v", result)
		}
	})

	t.Run("Should return true for healthy ready pods", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					},
				},
			},
		}

		result := reconciler.isPodHealthy(pod)
		if !result {
			t.Errorf("Expected true for healthy ready pod, got %v", result)
		}
	})
}

func TestGetPodNames(t *testing.T) {
	t.Run("Should extract pod names correctly", func(t *testing.T) {
		pods := []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod3"}},
		}

		expected := []string{"pod1", "pod2", "pod3"}
		result := getPodNames(pods)

		if len(result) != len(expected) {
			t.Errorf("Expected length %d, got %d", len(expected), len(result))
		}

		for i, name := range result {
			if name != expected[i] {
				t.Errorf("Expected name %s at index %d, got %s", expected[i], i, name)
			}
		}
	})

	t.Run("Should handle empty pod list", func(t *testing.T) {
		pods := []corev1.Pod{}
		result := getPodNames(pods)

		if len(result) != 0 {
			t.Errorf("Expected empty slice, got length %d", len(result))
		}
	})
}

func TestCalculateExponentialBackoff(t *testing.T) {
	r := &ModelAdapterReconciler{}

	tests := []struct {
		name        string
		retryCount  int32
		expectedMin time.Duration
		expectedMax time.Duration
	}{
		{
			name:        "First attempt (no backoff)",
			retryCount:  0,
			expectedMin: 0,
			expectedMax: 0,
		},
		{
			name:        "First retry",
			retryCount:  1,
			expectedMin: 10 * time.Second, // 5 * 2^1 = 10
			expectedMax: 10 * time.Second,
		},
		{
			name:        "Second retry",
			retryCount:  2,
			expectedMin: 20 * time.Second, // 5 * 2^2 = 20
			expectedMax: 20 * time.Second,
		},
		{
			name:        "Third retry",
			retryCount:  3,
			expectedMin: 40 * time.Second, // 5 * 2^3 = 40
			expectedMax: 40 * time.Second,
		},
		{
			name:        "Fourth retry",
			retryCount:  4,
			expectedMin: 80 * time.Second, // 5 * 2^4 = 80
			expectedMax: 80 * time.Second,
		},
		{
			name:        "High retry count (should cap at max)",
			retryCount:  10,
			expectedMin: 300 * time.Second, // Should cap at 300 seconds
			expectedMax: 300 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.calculateExponentialBackoff(tt.retryCount)

			if tt.expectedMin == tt.expectedMax {
				if result != tt.expectedMin {
					t.Errorf("Expected %v, got %v", tt.expectedMin, result)
				}
			} else {
				if result < tt.expectedMin || result > tt.expectedMax {
					t.Errorf("Expected between %v and %v, got %v", tt.expectedMin, tt.expectedMax, result)
				}
			}
		})
	}
}

func TestScheduledPodsManagement(t *testing.T) {
	r := &ModelAdapterReconciler{}

	instance := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-adapter",
			Namespace: "test-ns",
		},
	}

	// Test setting scheduled pods
	podNames := []string{"pod1", "pod2", "pod3"}
	r.setScheduledPods(instance, podNames)

	if instance.Annotations == nil {
		t.Fatal("Expected annotations to be set")
	}

	expected := "pod1,pod2,pod3"
	if instance.Annotations[ScheduledPodsAnnotationKey] != expected {
		t.Errorf("Expected %s, got %s", expected, instance.Annotations[ScheduledPodsAnnotationKey])
	}

	// Test getting scheduled pods
	retrievedPods := r.getScheduledPods(instance)
	if len(retrievedPods) != len(podNames) {
		t.Errorf("Expected %d pods, got %d", len(podNames), len(retrievedPods))
	}

	for i, pod := range retrievedPods {
		if pod != podNames[i] {
			t.Errorf("Expected pod %s at index %d, got %s", podNames[i], i, pod)
		}
	}

	// Test clearing scheduled pods
	r.clearScheduledPods(instance)
	if _, exists := instance.Annotations[ScheduledPodsAnnotationKey]; exists {
		t.Error("Expected scheduled pods annotation to be removed")
	}

	// Test getting scheduled pods from empty annotations
	emptyPods := r.getScheduledPods(instance)
	if emptyPods != nil {
		t.Errorf("Expected nil, got %v", emptyPods)
	}
}
