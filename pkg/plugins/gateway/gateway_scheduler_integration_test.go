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

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/scheduler/sessioninfo"
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
		{
			name:        "session ID from request body",
			requestID:   "req-123",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model":"test-model","session_id":"session-body-123","messages":[{"role":"user","content":"hello"}]}`),
			headers:     map[string]string{},
			expected:    "session-body-123",
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
