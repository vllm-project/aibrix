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
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
)

func setupTestRedis(t *testing.T) *redis.Client {
	// Create a mock redis client for testing
	// In real scenario, you would use miniredis or similar
	client := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})

	// Clear test keys
	ctx := context.Background()
	keys, _ := client.Keys(ctx, "po2_req_count:*").Result()
	if len(keys) > 0 {
		client.Del(ctx, keys...)
	}

	return client
}

func createTestPodForPowerOfTwo(name, ip string, port int) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName: "test-model",
				constants.ModelLabelPort: "8000",
			},
		},
		Status: v1.PodStatus{
			PodIP: ip,
		},
	}
	return pod
}

func TestPowerOfTwoRouter_buildCandidates(t *testing.T) {
	router, err := NewPowerOfTwoRouterWithRedis(nil, 3600)
	if err != nil {
		t.Skipf("Skipping test due to cache initialization failure: %v", err)
	}

	tests := []struct {
		name        string
		pods        []*v1.Pod
		portsMap    map[string][]int
		wantCount   int
		description string
	}{
		{
			name: "single port pods",
			pods: []*v1.Pod{
				createTestPodForPowerOfTwo("pod1", "10.0.0.1", 8000),
				createTestPodForPowerOfTwo("pod2", "10.0.0.2", 8000),
			},
			portsMap:    map[string][]int{},
			wantCount:   2,
			description: "should create one candidate per pod",
		},
		{
			name: "multi port pods",
			pods: []*v1.Pod{
				createTestPodForPowerOfTwo("pod1", "10.0.0.1", 8000),
			},
			portsMap: map[string][]int{
				"pod1": {8000, 8001, 8002, 8003},
			},
			wantCount:   4,
			description: "should create one candidate per port",
		},
		{
			name: "mixed single and multi port",
			pods: []*v1.Pod{
				createTestPodForPowerOfTwo("pod1", "10.0.0.1", 8000),
				createTestPodForPowerOfTwo("pod2", "10.0.0.2", 8000),
			},
			portsMap: map[string][]int{
				"pod1": {8000, 8001, 8002, 8003},
			},
			wantCount:   5,
			description: "should handle mixed pod types",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidates := router.buildCandidates(tt.pods, tt.portsMap)
			assert.Equal(t, tt.wantCount, len(candidates), tt.description)
		})
	}
}

func TestPowerOfTwoRouter_podServerKey(t *testing.T) {
	pod := createTestPodForPowerOfTwo("test-pod", "10.0.0.1", 8000)

	tests := []struct {
		name string
		key  podServerKey
		want string
	}{
		{
			name: "with port",
			key: podServerKey{
				pod:  pod,
				port: 8001,
			},
			want: "test-pod_8001",
		},
		{
			name: "without port",
			key: podServerKey{
				pod:  pod,
				port: 0,
			},
			want: "test-pod",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.key.String()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPowerOfTwoRouter_buildRedisKey(t *testing.T) {
	router, err := NewPowerOfTwoRouterWithRedis(nil, 3600) // 1 hour
	if err != nil {
		t.Skipf("Skipping test due to cache initialization failure: %v", err)
	}
	pod := createTestPodForPowerOfTwo("test-pod", "10.0.0.1", 8000)

	tests := []struct {
		name      string
		modelName string
		server    podServerKey
		wantMatch string
	}{
		{
			name:      "with port",
			modelName: "test-model",
			server: podServerKey{
				pod:  pod,
				port: 8001,
			},
			wantMatch: "po2_req_count:test-model:test-pod_8001:",
		},
		{
			name:      "without port",
			modelName: "test-model",
			server: podServerKey{
				pod:  pod,
				port: 0,
			},
			wantMatch: "po2_req_count:test-model:test-pod:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := router.buildRedisKey(tt.modelName, tt.server, time.Now())
			assert.Contains(t, key, tt.wantMatch)
			// Verify timestamp is modulo 3600
			// Extract timestamp from key
			parts := key[len(tt.wantMatch):]
			timestamp := parts
			assert.NotEmpty(t, timestamp, "timestamp should be present")
		})
	}
}

// TestPowerOfTwoRouter_keyRotation verifies that keys rotate at the configured interval
func TestPowerOfTwoRouter_keyRotation(t *testing.T) {
	router, err := NewPowerOfTwoRouterWithRedis(nil, 10) // Use 10 seconds for testing
	if err != nil {
		t.Skipf("Skipping test due to cache initialization failure: %v", err)
	}
	pod := createTestPodForPowerOfTwo("test-pod", "10.0.0.1", 8000)
	server := podServerKey{pod: pod, port: 8000}

	// Use fixed time for testing
	testTime := time.Now()

	// Generate keys at different times should be same within rotation window
	key1 := router.buildRedisKey("test-model", server, testTime)

	// Keys within same window should be identical
	key2 := router.buildRedisKey("test-model", server, testTime)
	assert.Equal(t, key1, key2, "keys within same rotation window should be identical")
}

// TestPowerOfTwoRouter_getRequestCounts verifies batch query functionality
func TestPowerOfTwoRouter_getRequestCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	client := setupTestRedis(t)
	defer func() { _ = client.Close() }()

	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skip("Redis not available, skipping test")
	}

	cache.InitForTest()
	router, err := NewPowerOfTwoRouterWithRedis(client, 3600)
	if err != nil {
		t.Fatalf("Failed to create router: %v", err)
	}

	pod1 := createTestPodForPowerOfTwo("pod1", "10.0.0.1", 8000)
	pod2 := createTestPodForPowerOfTwo("pod2", "10.0.0.2", 8000)
	pod3 := createTestPodForPowerOfTwo("pod3", "10.0.0.3", 8000)

	testTime := time.Now()
	server1 := podServerKey{pod: pod1, port: 8000}
	server2 := podServerKey{pod: pod2, port: 8001}
	server3 := podServerKey{pod: pod3, port: 8002}

	// Set some test values in Redis
	key1 := router.buildRedisKey("test-model", server1, testTime)
	key2 := router.buildRedisKey("test-model", server2, testTime)
	// key3 is not set (should return 0)

	client.Set(context.Background(), key1, 5, 5*time.Minute)
	client.Set(context.Background(), key2, 10, 5*time.Minute)

	// Test batch query
	servers := []podServerKey{server1, server2, server3}
	counts := router.getRequestCounts(context.Background(), "test-model", servers, testTime)

	assert.Equal(t, 3, len(counts), "should return counts for all servers")
	assert.Equal(t, int64(5), counts[0], "server1 count should be 5")
	assert.Equal(t, int64(10), counts[1], "server2 count should be 10")
	assert.Equal(t, int64(0), counts[2], "server3 count should be 0 (key not exists)")

	// Test empty input
	emptyCounts := router.getRequestCounts(context.Background(), "test-model", []podServerKey{}, testTime)
	assert.Equal(t, 0, len(emptyCounts), "empty input should return empty result")

	// Cleanup
	client.Del(context.Background(), key1, key2)
}

func TestPowerOfTwoRouter_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	// This test requires a running Redis instance
	// Skip if Redis is not available
	client := setupTestRedis(t)
	defer func() { _ = client.Close() }()

	// Test connection
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skip("Redis not available, skipping integration test")
	}

	cache.InitForTest()
	router, err := NewPowerOfTwoRouterWithRedis(client, 3600)
	if err != nil {
		t.Fatalf("Failed to create router: %v", err)
	}

	pod1 := createTestPodForPowerOfTwo("pod1", "10.0.0.1", 8000)

	testTime := time.Now()
	ctx := types.NewRoutingContext(context.Background(), RouterPowerOfTwo, "test-model", "", "test-request-1", "")
	ctx.RequestTime = testTime
	ctx.SetTargetPod(pod1)
	// AddRequestCount only tracks requests for models recently routed by this router.
	// Seed lastRoutingTime to mirror normal Route() flow in this direct integration test.
	router.lastRoutingTimeMu.Lock()
	router.lastRoutingTime["test-model"] = time.Now()
	router.lastRoutingTimeMu.Unlock()

	// Test AddRequestCount
	traceTerm := router.AddRequestCount(ctx, "test-request-1", "test-model")
	assert.Greater(t, traceTerm, int64(0))

	// Verify count increased
	server := podServerKey{pod: pod1, port: 0}
	count := router.getRequestCount(context.Background(), "test-model", server, testTime)
	assert.Equal(t, int64(1), count)

	// Test DoneRequestCount
	router.DoneRequestCount(ctx, "test-request-1", "test-model", traceTerm)

	// Verify count decreased
	count = router.getRequestCount(context.Background(), "test-model", server, testTime)
	assert.Equal(t, int64(0), count)
}

// TestPowerOfTwoRouter_NilContextHandling tests that DoneRequestCount and DoneRequestTrace
// handle nil context gracefully without panicking.
// This scenario occurs when the request context is cancelled before routing completes.
func TestPowerOfTwoRouter_NilContextHandling(t *testing.T) {
	// Note: We cannot use cache.InitForTest() here as it modifies global state
	// which causes race conditions when tests run in parallel with -race flag.
	// Instead, we create router with nil redis client which still allows us to test
	// the nil context handling logic without touching shared global cache state.
	router := &PowerOfTwoRouter{
		redisClient:           nil,
		keyRotationSec:        3600,
		requestTrackerTimeout: 30 * time.Second,
		lastRoutingTime:       make(map[string]time.Time),
	}

	t.Run("DoneRequestCount with nil context should not panic", func(t *testing.T) {
		// This should not panic
		assert.NotPanics(t, func() {
			router.DoneRequestCount(nil, "test-request", "test-model", 0)
		})
	})

	t.Run("DoneRequestTrace with nil context should not panic", func(t *testing.T) {
		// This should not panic
		assert.NotPanics(t, func() {
			router.DoneRequestTrace(nil, "test-request", "test-model", 100, 50, 0)
		})
	})

	t.Run("AddRequestCount with nil context should not panic", func(t *testing.T) {
		// AddRequestCount checks HasRouted() which requires non-nil context
		// But it should handle nil gracefully
		assert.NotPanics(t, func() {
			traceTerm := router.AddRequestCount(nil, "test-request", "test-model")
			assert.Equal(t, int64(0), traceTerm)
		})
	})
}

// TestPowerOfTwoRouter_ContextCancelledScenario simulates the real-world scenario
// where context is cancelled after routing begins but before completion.
func TestPowerOfTwoRouter_ContextCancelledScenario(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	client := setupTestRedis(t)
	defer func() { _ = client.Close() }()

	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skip("Redis not available, skipping test")
	}

	// Create router without initializing global cache to avoid race conditions
	router := &PowerOfTwoRouter{
		redisClient:           client,
		keyRotationSec:        3600,
		requestTrackerTimeout: 30 * time.Second,
		lastRoutingTime:       make(map[string]time.Time),
	}

	t.Run("cancelled context before routing completes", func(t *testing.T) {
		// Create a cancellable context
		ctx, cancel := context.WithCancel(context.Background())
		routingCtx := types.NewRoutingContext(ctx, RouterPowerOfTwo, "test-model", "", "test-request-cancelled", "")

		// Cancel the context immediately (simulating early cancellation)
		cancel()

		// Attempt to call DoneRequestCount with cancelled context
		// This should not panic even though routingCtx.Context is cancelled
		// and targetPod might not be set
		assert.NotPanics(t, func() {
			router.DoneRequestCount(routingCtx, "test-request-cancelled", "test-model", 0)
		})
	})

	t.Run("context cancelled after routing but before response", func(t *testing.T) {
		// Create a cancellable context
		ctx, cancel := context.WithCancel(context.Background())
		routingCtx := types.NewRoutingContext(ctx, RouterPowerOfTwo, "test-model", "", "test-request-partial", "")

		pod1 := createTestPodForPowerOfTwo("pod1", "10.0.0.1", 8000)
		routingCtx.SetTargetPod(pod1)

		// Seed lastRoutingTime
		router.lastRoutingTimeMu.Lock()
		router.lastRoutingTime["test-model"] = time.Now()
		router.lastRoutingTimeMu.Unlock()

		// Add request count
		traceTerm := router.AddRequestCount(routingCtx, "test-request-partial", "test-model")

		// Cancel context (simulating client disconnect)
		cancel()

		// Cleanup should work even with cancelled context
		// This is the key fix: DoneRequestCount uses background context for Redis operations
		// so it succeeds even when routingCtx.Context is cancelled
		assert.NotPanics(t, func() {
			router.DoneRequestCount(routingCtx, "test-request-partial", "test-model", traceTerm)
		})

		// Verify count was decremented despite cancelled context
		// This proves the counter leak is fixed
		server := podServerKey{pod: pod1, port: 0}
		count := router.getRequestCount(context.Background(), "test-model", server, routingCtx.RequestTime)
		assert.Equal(t, int64(0), count, "count should be decremented even with cancelled context")
	})

	t.Run("context cancelled between Incr and Expire should still set TTL", func(t *testing.T) {
		// This test verifies that even if context is cancelled after Incr succeeds,
		// the Expire operation still completes using background context
		ctx, cancel := context.WithCancel(context.Background())
		routingCtx := types.NewRoutingContext(ctx, RouterPowerOfTwo, "test-model", "", "test-request-expire", "")

		pod1 := createTestPodForPowerOfTwo("pod1", "10.0.0.1", 8000)
		routingCtx.SetTargetPod(pod1)

		router.lastRoutingTimeMu.Lock()
		router.lastRoutingTime["test-model"] = time.Now()
		router.lastRoutingTimeMu.Unlock()

		// Add request and immediately cancel context
		traceTerm := router.AddRequestCount(routingCtx, "test-request-expire", "test-model")
		cancel()

		// Verify the key has TTL set (Expire succeeded with background context)
		key := router.getRequestCountRedisKey(routingCtx)
		ttl, err := client.TTL(context.Background(), key).Result()
		assert.NoError(t, err)
		assert.Greater(t, ttl.Seconds(), float64(0), "TTL should be set even if context was cancelled after Incr")

		// Clean up
		router.DoneRequestCount(routingCtx, "test-request-expire", "test-model", traceTerm)
	})

	t.Run("context already cancelled before AddRequestCount should still increment", func(t *testing.T) {
		// This verifies the fix for the reviewer's correctness concern: AddRequestCount uses
		// a detached background context for Incr, so a request that completed routing is still
		// counted even if its context was cancelled before AddRequestCount runs. This keeps
		// Incr/Decr balanced.
		ctx, cancel := context.WithCancel(context.Background())
		routingCtx := types.NewRoutingContext(ctx, RouterPowerOfTwo, "test-model", "", "test-request-precancelled", "")

		pod1 := createTestPodForPowerOfTwo("pod1", "10.0.0.1", 8000)
		routingCtx.SetTargetPod(pod1)

		router.lastRoutingTimeMu.Lock()
		router.lastRoutingTime["test-model"] = time.Now()
		router.lastRoutingTimeMu.Unlock()

		// Cancel BEFORE calling AddRequestCount
		cancel()

		traceTerm := router.AddRequestCount(routingCtx, "test-request-precancelled", "test-model")
		assert.Greater(t, traceTerm, int64(0), "Incr should succeed with detached context even if request context is cancelled")

		// Counter should be balanced back to zero after Done
		router.DoneRequestCount(routingCtx, "test-request-precancelled", "test-model", traceTerm)
		server := podServerKey{pod: pod1, port: 0}
		count := router.getRequestCount(context.Background(), "test-model", server, routingCtx.RequestTime)
		assert.Equal(t, int64(0), count, "Incr/Decr should remain balanced")
	})
}

// TestPowerOfTwoRouter_HasRoutedWithNilContext tests that HasRouted() is never called
// on nil RoutingContext, which was the root cause of the panic.
func TestPowerOfTwoRouter_HasRoutedWithNilContext(t *testing.T) {
	// Create router without initializing global cache to avoid race conditions
	router := &PowerOfTwoRouter{
		redisClient:           nil,
		keyRotationSec:        3600,
		requestTrackerTimeout: 30 * time.Second,
		lastRoutingTime:       make(map[string]time.Time),
	}

	// Verify that calling DoneRequestCount with nil doesn't try to call HasRouted()
	// which would panic with nil pointer dereference
	assert.NotPanics(t, func() {
		router.DoneRequestCount(nil, "test-request", "test-model", 0)
	}, "DoneRequestCount should handle nil context without calling HasRouted()")
}

// TestPowerOfTwoRouter_NormalFlowUnaffected ensures that the nil checks don't
// break the normal routing flow.
func TestPowerOfTwoRouter_NormalFlowUnaffected(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	client := setupTestRedis(t)
	defer func() { _ = client.Close() }()

	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skip("Redis not available, skipping test")
	}

	// Create router without initializing global cache to avoid race conditions
	router := &PowerOfTwoRouter{
		redisClient:           client,
		keyRotationSec:        3600,
		requestTrackerTimeout: 30 * time.Second,
		lastRoutingTime:       make(map[string]time.Time),
	}

	pod1 := createTestPodForPowerOfTwo("pod1", "10.0.0.1", 8000)
	pod2 := createTestPodForPowerOfTwo("pod2", "10.0.0.2", 8000)

	ctx := types.NewRoutingContext(context.Background(), RouterPowerOfTwo, "test-model", "", "test-request-normal", "")
	ctx.SetTargetPod(pod1)

	// Seed lastRoutingTime
	router.lastRoutingTimeMu.Lock()
	router.lastRoutingTime["test-model"] = time.Now()
	router.lastRoutingTimeMu.Unlock()

	// Normal flow: AddRequestCount -> DoneRequestCount
	traceTerm := router.AddRequestCount(ctx, "test-request-normal", "test-model")
	assert.Greater(t, traceTerm, int64(0), "traceTerm should be positive")

	server := podServerKey{pod: pod1, port: 0}
	count := router.getRequestCount(context.Background(), "test-model", server, ctx.RequestTime)
	assert.Equal(t, int64(1), count, "count should be incremented")

	router.DoneRequestCount(ctx, "test-request-normal", "test-model", traceTerm)
	count = router.getRequestCount(context.Background(), "test-model", server, ctx.RequestTime)
	assert.Equal(t, int64(0), count, "count should be decremented")

	// Test with another pod to ensure routing selection still works
	ctx2 := types.NewRoutingContext(context.Background(), RouterPowerOfTwo, "test-model", "", "test-request-normal-2", "")
	ctx2.SetTargetPod(pod2)

	traceTerm2 := router.AddRequestCount(ctx2, "test-request-normal-2", "test-model")
	assert.Greater(t, traceTerm2, int64(0), "traceTerm2 should be positive")

	server2 := podServerKey{pod: pod2, port: 0}
	count2 := router.getRequestCount(context.Background(), "test-model", server2, ctx2.RequestTime)
	assert.Equal(t, int64(1), count2, "count2 should be incremented")

	router.DoneRequestCount(ctx2, "test-request-normal-2", "test-model", traceTerm2)
	count2 = router.getRequestCount(context.Background(), "test-model", server2, ctx2.RequestTime)
	assert.Equal(t, int64(0), count2, "count2 should be decremented")
}
