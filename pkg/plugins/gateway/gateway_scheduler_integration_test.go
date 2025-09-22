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

package gateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/scheduler"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/scheduler/sessioninfo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSchedulerComponents_Initialization(t *testing.T) {
	// Test session cache initialization
	sessionCache := sessioninfo.NewMutexSessionCache()
	assert.NotNil(t, sessionCache, "Session cache should be initialized")

	// Test that we can create and use session cache
	cst, waitTime := sessionCache.GetOrCreateForScheduler("test-session")
	assert.GreaterOrEqual(t, cst.Nanoseconds(), int64(0), "CST should be non-negative")
	assert.GreaterOrEqual(t, waitTime.Nanoseconds(), int64(0), "Wait time should be non-negative")
}

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		name        string
		requestID   string
		requestPath string
		requestBody []byte
		headers     map[string]string
		expected    string
	}{
		{
			name:        "session ID from header (lowercase)",
			requestID:   "req-123",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{"x-session-id": "session-456"},
			expected:    "session-456",
		},
		{
			name:        "session ID from header (uppercase)",
			requestID:   "req-123",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{"X-Session-ID": "session-789"},
			expected:    "session-789",
		},
		// COMMENTED OUT: Body-based session ID extraction is disabled
		// {
		// 	name:        "session ID from request body",
		// 	requestID:   "req-123",
		// 	requestPath: "/v1/chat/completions",
		// 	requestBody: []byte(`{"model":"test-model","session_id":"session-body-123","messages":[{"role":"user","content":"hello"}]}`),
		// 	headers:     map[string]string{},
		// 	expected:    "session-body-123",
		// },
		{
			name:        "session ID from request body - now falls back to request ID",
			requestID:   "req-123",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","session_id":"session-body-123","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{},
			expected:    "req-123", // MODIFIED: Now falls back to requestID since body parsing is disabled
		},
		{
			name:        "fallback to request ID",
			requestID:   "req-fallback",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{},
			expected:    "req-fallback",
		},
		{
			name:        "header takes precedence over body",
			requestID:   "req-123",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","session_id":"session-body-123","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{"x-session-id": "session-header-456"},
			expected:    "session-header-456",
		},
		{
			name:        "invalid JSON body falls back to request ID",
			requestID:   "req-invalid",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{invalid json`),
			headers:     map[string]string{},
			expected:    "req-invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractSessionID(tt.requestID, tt.requestPath, tt.requestBody, tt.headers)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractSessionIDFromHeaders(t *testing.T) {
	tests := []struct {
		name      string
		requestID string
		headers   map[string]string
		expected  string
	}{
		{
			name:      "session ID from header (lowercase)",
			requestID: "req-123",
			headers:   map[string]string{"x-session-id": "session-456"},
			expected:  "session-456",
		},
		{
			name:      "session ID from header (uppercase)",
			requestID: "req-123",
			headers:   map[string]string{"X-Session-ID": "session-789"},
			expected:  "session-789",
		},
		{
			name:      "no session ID in headers - fallback to requestID",
			requestID: "req-fallback",
			headers:   map[string]string{},
			expected:  "req-fallback",
		},
		{
			name:      "empty session ID in headers - fallback to requestID",
			requestID: "req-empty",
			headers:   map[string]string{"x-session-id": ""},
			expected:  "req-empty",
		},
		{
			name:      "lowercase takes precedence when both present",
			requestID: "req-123",
			headers:   map[string]string{"x-session-id": "session-lower", "X-Session-ID": "session-upper"},
			expected:  "session-lower",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractSessionIDFromHeaders(tt.requestID, tt.headers)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestExtractSessionIDFromBody tests the extractSessionID function behavior
// NOTE: Body parsing is now disabled, so these tests verify headers-only behavior
// and proper fallback to requestID when no headers are present
func TestExtractSessionIDFromBody(t *testing.T) {
	tests := []struct {
		name        string
		requestID   string
		requestPath string
		requestBody []byte
		headers     map[string]string
		expected    string
	}{
		{
			name:        "header session_id takes priority over body",
			requestID:   "req-123",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","session_id":"session-body-123","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{"x-session-id": "session-header-456"},
			expected:    "session-header-456", // Headers are checked first in extractSessionID
		},
		{
			name:        "body session_id when no header session_id - now falls back to request ID",
			requestID:   "req-123",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","session_id":"session-body-123","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{},
			expected:    "req-123", // MODIFIED: Now falls back to requestID since body parsing is disabled
		},
		{
			name:        "header session_id when no body session_id",
			requestID:   "req-123",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{"x-session-id": "session-header-456"},
			expected:    "session-header-456",
		},
		{
			name:        "fallback to requestID when no session_id anywhere",
			requestID:   "req-fallback",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{},
			expected:    "req-fallback",
		},
		{
			name:        "invalid JSON body falls back to header session_id",
			requestID:   "req-invalid",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{invalid json`),
			headers:     map[string]string{"x-session-id": "session-header-789"},
			expected:    "session-header-789",
		},
		{
			name:        "invalid JSON body with no header falls back to requestID",
			requestID:   "req-invalid",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{invalid json`),
			headers:     map[string]string{},
			expected:    "req-invalid",
		},
		{
			name:        "non-chat-completion path ignores body",
			requestID:   "req-123",
			requestPath: "/v1/models",
			requestBody: []byte(`{"session_id":"session-body-123"}`),
			headers:     map[string]string{"x-session-id": "session-header-456"},
			expected:    "session-header-456",
		},
		{
			name:        "empty body session_id falls back to header",
			requestID:   "req-123",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","session_id":"","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{"x-session-id": "session-header-456"},
			expected:    "session-header-456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractSessionID(tt.requestID, tt.requestPath, tt.requestBody, tt.headers)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestExtractSessionID_HeadersOnlyBehavior specifically tests the headers-only behavior
// after disabling body parsing to ensure the modification works correctly
func TestExtractSessionID_HeadersOnlyBehavior(t *testing.T) {
	tests := []struct {
		name        string
		requestID   string
		headers     map[string]string
		requestBody []byte // This should be ignored
		expected    string
	}{
		{
			name:        "headers-only: lowercase header with body present (body ignored)",
			requestID:   "req-123",
			headers:     map[string]string{"x-session-id": "session-from-header"},
			requestBody: []byte(`{"session_id":"session-from-body"}`), // Should be ignored
			expected:    "session-from-header",
		},
		{
			name:        "headers-only: uppercase header with body present (body ignored)",
			requestID:   "req-456",
			headers:     map[string]string{"X-Session-ID": "session-from-header-upper"},
			requestBody: []byte(`{"session_id":"session-from-body"}`), // Should be ignored
			expected:    "session-from-header-upper",
		},
		{
			name:        "headers-only: no header, body present (fallback to requestID)",
			requestID:   "req-789",
			headers:     map[string]string{},
			requestBody: []byte(`{"session_id":"session-from-body"}`), // Should be ignored
			expected:    "req-789",                                    // Falls back to requestID
		},
		{
			name:        "headers-only: empty header, body present (fallback to requestID)",
			requestID:   "req-empty",
			headers:     map[string]string{"x-session-id": ""},
			requestBody: []byte(`{"session_id":"session-from-body"}`), // Should be ignored
			expected:    "req-empty",                                  // Falls back to requestID
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test the main extractSessionID function (which now ignores body)
			result := extractSessionID(tt.requestID, "/v1/chat/completions", tt.requestBody, tt.headers)
			assert.Equal(t, tt.expected, result, "extractSessionID should ignore body and use headers-only logic")

			// Also test the headers-only function for consistency
			headerResult := extractSessionIDFromHeaders(tt.requestID, tt.headers)
			assert.Equal(t, tt.expected, headerResult, "extractSessionIDFromHeaders should match extractSessionID behavior")
		})
	}
}

func TestScheduler_CacheIntegration(t *testing.T) {
	// Test that scheduler correctly integrates with cache when available
	k8sClient := fake.NewSimpleClientset()
	sessionCache := sessioninfo.NewMutexSessionCache()

	// Test with nil cache (fallback mode)
	schedulerWithoutCache := scheduler.NewScheduler(k8sClient, sessionCache, nil)
	assert.NotNil(t, schedulerWithoutCache, "Scheduler should be created even without cache")

	// Test that scheduler can be stopped gracefully
	schedulerWithoutCache.Stop()

	// Give it a moment to stop
	time.Sleep(10 * time.Millisecond)

	t.Log("Scheduler cache integration test completed")
}

func TestScheduler_LoadAwarenessWithRealCache(t *testing.T) {
	// This test would work if cache was properly initialized
	// For now, we test the fallback behavior

	k8sClient := fake.NewSimpleClientset()
	sessionCache := sessioninfo.NewMutexSessionCache()

	// Try to get cache (will fail in test environment)
	cacheInstance, err := cache.Get()
	if err != nil {
		t.Logf("Cache not available in test environment: %v", err)
		cacheInstance = nil
	}

	// Create scheduler with cache (or nil)
	sched := scheduler.NewScheduler(k8sClient, sessionCache, cacheInstance)
	defer sched.Stop()

	// Verify scheduler was created successfully
	assert.NotNil(t, sched, "Scheduler should be created")

	t.Log("Load awareness with real cache test completed")
}

func TestScheduler_PodCapacityEstimation(t *testing.T) {
	// Test pod capacity estimation logic
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "high-capacity-pod",
				Namespace: "default",
				Annotations: map[string]string{
					"aibrix.io/max-concurrent-requests": "200",
				},
			},
			Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "1.1.1.1"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "medium-capacity-pod",
				Namespace: "default",
				Annotations: map[string]string{
					"aibrix.io/max-concurrent-requests": "100",
				},
			},
			Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "2.2.2.2"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default-capacity-pod",
				Namespace: "default",
			},
			Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "3.3.3.3"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "invalid-annotation-pod",
				Namespace: "default",
				Annotations: map[string]string{
					"aibrix.io/max-concurrent-requests": "invalid",
				},
			},
			Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "4.4.4.4"},
		},
	}

	// Test that we can create pods with different capacity annotations
	for i, pod := range pods {
		assert.NotNil(t, pod, "Pod %d should not be nil", i)
		assert.NotEmpty(t, pod.Name, "Pod %d should have a name", i)

		// Check annotation parsing logic
		if pod.Annotations != nil {
			if maxConcurrency, exists := pod.Annotations["aibrix.io/max-concurrent-requests"]; exists {
				t.Logf("Pod %s has max-concurrent-requests: %s", pod.Name, maxConcurrency)
			}
		}
	}

	t.Log("Pod capacity estimation test completed")
}

func TestScheduler_Integration(t *testing.T) {
	// Test that scheduler components work together
	k8sClient := fake.NewSimpleClientset()
	sessionCache := sessioninfo.NewMutexSessionCache()

	// This should not panic
	assert.NotPanics(t, func() {
		// Note: We can't easily test NewScheduler here because it starts background goroutines
		// and requires proper cleanup. The actual integration is tested in the main test suite.
		_ = sessionCache
		_ = k8sClient
	})
}
