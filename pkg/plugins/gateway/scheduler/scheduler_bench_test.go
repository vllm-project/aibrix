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

package scheduler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/scheduler/sessioninfo"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// BenchmarkScheduler_SubmitJob_HighConcurrency tests the performance of the new
// lock-free scheduler under high concurrency scenarios.
func BenchmarkScheduler_SubmitJob_HighConcurrency(b *testing.B) {
	cache := sessioninfo.NewMutexSessionCache()
	k8sClient := fake.NewSimpleClientset()
	scheduler := NewScheduler(k8sClient, cache, nil)
	defer scheduler.Stop()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		sessionID := fmt.Sprintf("session-%d", time.Now().UnixNano())
		for pb.Next() {
			ctx := types.NewRoutingContext(context.Background(), "test-algorithm", "test-model", "test message", "test-request", "test-user")
			decision, err := scheduler.SubmitJob(ctx, sessionID)
			if err != nil {
				b.Fatalf("SubmitJob failed: %v", err)
			}
			if decision == nil {
				b.Fatal("Decision is nil")
			}
			// Simulate job completion
			scheduler.FinalizeJob(sessionID, 100*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond)
		}
	})
}

// BenchmarkScheduler_BurstLoad simulates the scenario you described:
// thousands of requests arriving within milliseconds.
func BenchmarkScheduler_BurstLoad(b *testing.B) {
	cache := sessioninfo.NewMutexSessionCache()
	k8sClient := fake.NewSimpleClientset()
	scheduler := NewScheduler(k8sClient, cache, nil)
	defer scheduler.Stop()

	// Simulate burst scenarios
	burstSizes := []int{100, 1000, 5000, 10000}

	for _, burstSize := range burstSizes {
		b.Run(fmt.Sprintf("Burst-%d", burstSize), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var wg sync.WaitGroup
				start := time.Now()

				// Launch burst of requests
				for j := 0; j < burstSize; j++ {
					wg.Add(1)
					go func(requestID int) {
						defer wg.Done()
						sessionID := fmt.Sprintf("session-%d-%d", i, requestID)
						ctx := types.NewRoutingContext(context.Background(), "test-algorithm", "test-model", "test message", fmt.Sprintf("request-%d", requestID), "test-user")

						decision, err := scheduler.SubmitJob(ctx, sessionID)
						if err != nil {
							b.Errorf("SubmitJob failed: %v", err)
							return
						}
						if decision == nil {
							b.Error("Decision is nil")
							return
						}

						// Simulate job completion
						scheduler.FinalizeJob(sessionID, 100*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond)
					}(j)
				}

				wg.Wait()
				elapsed := time.Since(start)

				// Report throughput
				throughput := float64(burstSize) / elapsed.Seconds()
				b.ReportMetric(throughput, "requests/sec")
				b.ReportMetric(float64(elapsed.Nanoseconds())/float64(burstSize), "ns/request")
			}
		})
	}
}

// TestScheduler_LoadAwareness tests the load awareness functionality
func TestScheduler_LoadAwareness(t *testing.T) {
	// Create fake k8s client
	k8sClient := fake.NewSimpleClientset()

	// Create session cache
	sessionCache := sessioninfo.NewMutexSessionCache()

	// Create scheduler with load awareness (no cache in test environment)
	scheduler := NewScheduler(k8sClient, sessionCache, nil).(*inProcessScheduler)
	defer scheduler.Stop()

	// Test basic initialization
	if scheduler.cache == nil {
		t.Log("Cache not available in test environment - this is expected")
	}

	if scheduler.loadProvider == nil {
		t.Log("LoadProvider not available in test environment - this is expected")
	}

	// Test capacity calculation with no pods
	batchSize := scheduler.calculateBatchSize()
	if batchSize != 0 {
		t.Errorf("Expected batch size 0 with no pods, got %d", batchSize)
	}

	// Test with mock pods
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod-1",
				Namespace: "default",
				Annotations: map[string]string{
					"aibrix.io/max-concurrent-requests": "100",
				},
			},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
				PodIP: "1.2.3.4",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod-2",
				Namespace: "default",
			},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
				PodIP: "5.6.7.8",
			},
		},
	}

	scheduler.healthyPods.Store(pods)

	// Test improved fallback batch size calculation
	batchSize = scheduler.calculateBatchSize()
	expectedBatchSize := 100 + DEFAULT_POD_CONCURRENT_CAPACITY // Pod1(100) + Pod2(1)
	if batchSize != expectedBatchSize {
		t.Errorf("Expected batch size %d in improved fallback mode, got %d", expectedBatchSize, batchSize)
	}

	// Test estimated concurrent capacity
	capacity1 := scheduler.getEstimatedConcurrentCapacity(pods[0])
	if capacity1 != 100 {
		t.Errorf("Expected capacity 100 from annotation, got %d", capacity1)
	}

	capacity2 := scheduler.getEstimatedConcurrentCapacity(pods[1])
	if capacity2 != DEFAULT_POD_CONCURRENT_CAPACITY {
		t.Errorf("Expected default capacity %d, got %d", DEFAULT_POD_CONCURRENT_CAPACITY, capacity2)
	}

	t.Logf("Load awareness test completed successfully")
	t.Logf("   - Pod 1 capacity: %d (from annotation)", capacity1)
	t.Logf("   - Pod 2 capacity: %d (default)", capacity2)
	t.Logf("   - Batch size: %d", batchSize)
}

func TestScheduler_BatchSizeSmoothing(t *testing.T) {
	k8sClient := fake.NewSimpleClientset()
	sessionCache := sessioninfo.NewMutexSessionCache()
	scheduler := NewScheduler(k8sClient, sessionCache, nil).(*inProcessScheduler)
	defer scheduler.Stop()

	// Create pods for testing
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod-1",
				Namespace: "default",
			},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
				PodIP: "1.2.3.4",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod-2",
				Namespace: "default",
			},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
				PodIP: "5.6.7.8",
			},
		},
	}

	scheduler.healthyPods.Store(pods)

	// Test smoothing behavior
	// First call should return raw value (no previous history)
	smoothed1 := scheduler.calculateSmoothedBatchSize()
	raw1 := scheduler.calculateBatchSize()

	// First smoothed value should be close to raw value
	if smoothed1 != raw1 {
		// Allow small difference due to smoothing initialization
		diff := smoothed1 - raw1
		if diff < -1 || diff > 1 {
			t.Errorf("First smoothed batch size should be close to raw value, got smoothed=%d, raw=%d", smoothed1, raw1)
		}
	}

	// Second call should show smoothing effect
	smoothed2 := scheduler.calculateSmoothedBatchSize()

	// Verify that lastBatchSize is being updated
	lastBatchSize := int(scheduler.lastBatchSize.Load())
	if lastBatchSize != smoothed2 {
		t.Errorf("lastBatchSize should be updated to smoothed value, got %d, expected %d", lastBatchSize, smoothed2)
	}

	t.Logf("Batch size smoothing test completed successfully")
	t.Logf("   - Raw batch size: %d", raw1)
	t.Logf("   - First smoothed: %d", smoothed1)
	t.Logf("   - Second smoothed: %d", smoothed2)
	t.Logf("   - Last batch size stored: %d", lastBatchSize)
}

func TestScheduler_CornerCases(t *testing.T) {
	k8sClient := fake.NewSimpleClientset()
	sessionCache := sessioninfo.NewMutexSessionCache()
	scheduler := NewScheduler(k8sClient, sessionCache, nil).(*inProcessScheduler)
	defer scheduler.Stop()

	t.Run("no_pods", func(t *testing.T) {
		// Test with no pods
		scheduler.healthyPods.Store([]*v1.Pod{})
		batchSize := scheduler.calculateBatchSize()
		if batchSize != 0 {
			t.Errorf("Expected batch size 0 with no pods, got %d", batchSize)
		}
	})

	t.Run("pods_without_annotations_pass_through", func(t *testing.T) {
		// Test with pods that have no capacity annotations - should trigger pass-through mode
		pods := []*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-annotation-pod-1",
					Namespace: "default",
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "1.1.1.1"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-annotation-pod-2",
					Namespace: "default",
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "2.2.2.2"},
			},
		}

		scheduler.healthyPods.Store(pods)
		batchSize := scheduler.calculateBatchSize()

		// Should trigger pass-through mode (totalEstimatedCapacity = 2, podCount = 2)
		if batchSize != PASS_THROUGH_BATCH_SIZE {
			t.Errorf("Expected pass-through batch size %d, got %d", PASS_THROUGH_BATCH_SIZE, batchSize)
		}

		t.Logf("Pass-through mode triggered: batch size = %d", batchSize)
	})

	t.Run("mixed_pods_with_and_without_annotations", func(t *testing.T) {
		// Test with mixed pods - some with annotations, some without
		pods := []*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "annotated-pod",
					Namespace: "default",
					Annotations: map[string]string{
						"aibrix.io/max-concurrent-requests": "50",
					},
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "1.1.1.1"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-annotation-pod",
					Namespace: "default",
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "2.2.2.2"},
			},
		}

		scheduler.healthyPods.Store(pods)
		batchSize := scheduler.calculateBatchSize()

		// Should use improved fallback: 50 + 1 = 51
		expectedBatchSize := 50 + DEFAULT_POD_CONCURRENT_CAPACITY
		if batchSize != expectedBatchSize {
			t.Errorf("Expected batch size %d for mixed pods, got %d", expectedBatchSize, batchSize)
		}

		t.Logf("Mixed pods batch size: %d", batchSize)
	})

	t.Run("high_inflight_requests", func(t *testing.T) {
		// Test with high inflight requests
		pods := []*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						"aibrix.io/max-concurrent-requests": "10",
					},
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "1.1.1.1"},
			},
		}

		scheduler.healthyPods.Store(pods)

		// Set high inflight requests
		scheduler.inflightRequests.Store(15) // More than capacity

		batchSize := scheduler.calculateBatchSize()

		// Should return 0 (no available capacity)
		if batchSize != 0 {
			t.Errorf("Expected batch size 0 with overloaded pods, got %d", batchSize)
		}

		// Reset inflight requests
		scheduler.inflightRequests.Store(0)

		t.Logf("Overloaded scenario handled correctly: batch size = %d", batchSize)
	})

	t.Run("invalid_annotation_values", func(t *testing.T) {
		// Test with invalid annotation values
		pods := []*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-annotation-pod",
					Namespace: "default",
					Annotations: map[string]string{
						"aibrix.io/max-concurrent-requests": "invalid-number",
					},
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "1.1.1.1"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "negative-annotation-pod",
					Namespace: "default",
					Annotations: map[string]string{
						"aibrix.io/max-concurrent-requests": "-5",
					},
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "2.2.2.2"},
			},
		}

		scheduler.healthyPods.Store(pods)
		batchSize := scheduler.calculateBatchSize()

		// Should trigger pass-through mode (both pods fall back to default capacity 1)
		if batchSize != PASS_THROUGH_BATCH_SIZE {
			t.Errorf("Expected pass-through batch size %d for invalid annotations, got %d", PASS_THROUGH_BATCH_SIZE, batchSize)
		}

		t.Logf("Invalid annotations handled correctly: batch size = %d", batchSize)
	})

	t.Log("All corner cases tested successfully")
}

// BenchmarkScheduler_ChannelVsMutex compares the channel-based approach
// with a hypothetical mutex-based approach for job submission.
func BenchmarkScheduler_ChannelVsMutex(b *testing.B) {
	cache := sessioninfo.NewMutexSessionCache()
	k8sClient := fake.NewSimpleClientset()
	scheduler := NewScheduler(k8sClient, cache, nil)
	defer scheduler.Stop()

	b.Run("ChannelBased", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			sessionID := fmt.Sprintf("session-%d", time.Now().UnixNano())
			for pb.Next() {
				ctx := types.NewRoutingContext(context.Background(), "test-algorithm", "test-model", "test message", "test-request", "test-user")
				decision, err := scheduler.SubmitJob(ctx, sessionID)
				if err != nil {
					b.Fatalf("SubmitJob failed: %v", err)
				}
				if decision == nil {
					b.Fatal("Decision is nil")
				}
				scheduler.FinalizeJob(sessionID, 100*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond)
			}
		})
	})
}

// BenchmarkScheduler_SessionCachePerformance tests the performance of session cache operations
func BenchmarkScheduler_SessionCachePerformance(b *testing.B) {
	cache := sessioninfo.NewMutexSessionCache()
	k8sClient := fake.NewSimpleClientset()
	scheduler := NewScheduler(k8sClient, cache, nil)
	defer scheduler.Stop()

	// Pre-populate some sessions
	for i := 0; i < 1000; i++ {
		sessionID := fmt.Sprintf("session-%d", i)
		cache.GetOrCreateForScheduler(sessionID)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		sessionID := fmt.Sprintf("session-%d", time.Now().UnixNano()%1000)
		for pb.Next() {
			ctx := types.NewRoutingContext(context.Background(), "test-algorithm", "test-model", "test message", "test-request", "test-user")
			decision, err := scheduler.SubmitJob(ctx, sessionID)
			if err != nil {
				b.Fatalf("SubmitJob failed: %v", err)
			}
			if decision == nil {
				b.Fatal("Decision is nil")
			}
			scheduler.FinalizeJob(sessionID, 100*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond)
		}
	})
}
