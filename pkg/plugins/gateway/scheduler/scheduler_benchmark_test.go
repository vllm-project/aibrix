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
	"k8s.io/client-go/kubernetes/fake"
)

// BenchmarkScheduler_SubmitJob_HighConcurrency tests the performance of the new
// lock-free scheduler under high concurrency scenarios.
func BenchmarkScheduler_SubmitJob_HighConcurrency(b *testing.B) {
	cache := sessioninfo.NewMutexSessionCache()
	k8sClient := fake.NewSimpleClientset()
	scheduler := NewScheduler(k8sClient, cache)
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
	scheduler := NewScheduler(k8sClient, cache)
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

// BenchmarkScheduler_ChannelVsMutex compares the channel-based approach
// with a hypothetical mutex-based approach for job submission.
func BenchmarkScheduler_ChannelVsMutex(b *testing.B) {
	cache := sessioninfo.NewMutexSessionCache()
	k8sClient := fake.NewSimpleClientset()
	scheduler := NewScheduler(k8sClient, cache)
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
	scheduler := NewScheduler(k8sClient, cache)
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
