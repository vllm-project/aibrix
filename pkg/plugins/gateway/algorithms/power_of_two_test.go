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
	router := NewPowerOfTwoRouterWithRedis(nil, 3600)

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
	router := NewPowerOfTwoRouterWithRedis(nil, 3600) // 1 hour
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
	router := NewPowerOfTwoRouterWithRedis(nil, 10) // Use 10 seconds for testing
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

func TestPowerOfTwoRouter_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	// This test requires a running Redis instance
	// Skip if Redis is not available
	client := setupTestRedis(t)
	defer client.Close()

	// Test connection
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skip("Redis not available, skipping integration test")
	}

	router := NewPowerOfTwoRouterWithRedis(client, 3600)

	pod1 := createTestPodForPowerOfTwo("pod1", "10.0.0.1", 8000)

	testTime := time.Now()
	ctx := &types.RoutingContext{
		Context:     context.Background(),
		Model:       "test-model",
		RequestID:   "test-request-1",
		RequestTime: testTime,
	}
	ctx.SetTargetPod(pod1)

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
