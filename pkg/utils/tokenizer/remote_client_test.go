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

package tokenizer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestHTTPClientRetryableStatusCodes tests the retry logic for different status codes
func TestHTTPClientRetryableStatusCodes(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		expectedRetry bool
		expectedCalls int
		retryAfter    string
		wantErr       bool
	}{
		{
			name:          "200 OK - no retry",
			statusCode:    http.StatusOK,
			expectedRetry: false,
			expectedCalls: 1,
			wantErr:       false,
		},
		{
			name:          "400 Bad Request - no retry",
			statusCode:    http.StatusBadRequest,
			expectedRetry: false,
			expectedCalls: 1,
			wantErr:       true,
		},
		{
			name:          "408 Request Timeout - retry",
			statusCode:    http.StatusRequestTimeout,
			expectedRetry: true,
			expectedCalls: 3, // initial + 2 retries
			wantErr:       true,
		},
		{
			name:          "429 Too Many Requests - retry with Retry-After",
			statusCode:    http.StatusTooManyRequests,
			expectedRetry: true,
			expectedCalls: 3,
			retryAfter:    "1", // 1 second
			wantErr:       true,
		},
		{
			name:          "500 Internal Server Error - retry",
			statusCode:    http.StatusInternalServerError,
			expectedRetry: true,
			expectedCalls: 3,
			wantErr:       true,
		},
		{
			name:          "501 Not Implemented - no retry",
			statusCode:    http.StatusNotImplemented,
			expectedRetry: false,
			expectedCalls: 1,
			wantErr:       true,
		},
		{
			name:          "503 Service Unavailable - retry",
			statusCode:    http.StatusServiceUnavailable,
			expectedRetry: true,
			expectedCalls: 3,
			wantErr:       true,
		},
		{
			name:          "302 Found - redirect error",
			statusCode:    http.StatusFound,
			expectedRetry: false,
			expectedCalls: 1,
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var callCount int32

			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&callCount, 1)

				// Set Retry-After header if specified
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}

				// Return the test status code
				w.WriteHeader(tt.statusCode)

				// Write a response body
				if tt.statusCode == http.StatusOK {
					if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
						t.Logf("Failed to encode response: %v", err)
					}
				} else {
					if err := json.NewEncoder(w).Encode(map[string]string{"error": "test error"}); err != nil {
						t.Logf("Failed to encode response: %v", err)
					}
				}
			}))
			defer server.Close()

			// Create HTTP client with short timeout for testing
			client := newHTTPClient(server.URL, 5*time.Second, 2)

			// Make request
			ctx := context.Background()
			_, err := client.Post(ctx, "/test", map[string]string{"test": "data"})

			// Check error
			if (err != nil) != tt.wantErr {
				t.Errorf("Post() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Check call count
			actualCalls := atomic.LoadInt32(&callCount)
			if actualCalls != int32(tt.expectedCalls) {
				t.Errorf("Expected %d calls, got %d", tt.expectedCalls, actualCalls)
			}
		})
	}
}

// TestHTTPClientRetryAfterHeader tests parsing of Retry-After header
func TestHTTPClientRetryAfterHeader(t *testing.T) {
	tests := []struct {
		name            string
		retryAfter      string
		minExpectedWait time.Duration
		maxExpectedWait time.Duration
	}{
		{
			name:            "Retry-After in seconds",
			retryAfter:      "1",
			minExpectedWait: 900 * time.Millisecond,
			maxExpectedWait: 1200 * time.Millisecond,
		},
		{
			name:            "Invalid Retry-After falls back to exponential backoff",
			retryAfter:      "invalid",
			minExpectedWait: 50 * time.Millisecond,
			maxExpectedWait: 500 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requestTime time.Time

			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requestTime.IsZero() {
					requestTime = time.Now()
					w.Header().Set("Retry-After", tt.retryAfter)
					w.WriteHeader(http.StatusTooManyRequests)
					if err := json.NewEncoder(w).Encode(map[string]string{"error": "rate limited"}); err != nil {
						t.Logf("Failed to encode response: %v", err)
					}
				} else {
					// Second request succeeds
					w.WriteHeader(http.StatusOK)
					if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
						t.Logf("Failed to encode response: %v", err)
					}
				}
			}))
			defer server.Close()

			// Create HTTP client
			client := newHTTPClient(server.URL, 10*time.Second, 1)

			// Make request
			ctx := context.Background()
			startTime := time.Now()
			_, err := client.Post(ctx, "/test", map[string]string{"test": "data"})
			duration := time.Since(startTime)

			// Should succeed after retry
			if err != nil {
				t.Errorf("Expected success after retry, got error: %v", err)
			}

			// Check that we waited approximately the right amount of time
			if duration < tt.minExpectedWait || duration > tt.maxExpectedWait {
				t.Errorf("Expected wait between %v and %v, got %v",
					tt.minExpectedWait, tt.maxExpectedWait, duration)
			}
		})
	}
}

// TestHTTPClientGetMethod tests the Get method status code handling
func TestHTTPClientGetMethod(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
		errMsg     string
	}{
		{
			name:       "200 OK",
			statusCode: http.StatusOK,
			wantErr:    false,
		},
		{
			name:       "404 Not Found",
			statusCode: http.StatusNotFound,
			wantErr:    true,
			errMsg:     "client error",
		},
		{
			name:       "500 Internal Server Error",
			statusCode: http.StatusInternalServerError,
			wantErr:    true,
			errMsg:     "server error",
		},
		{
			name:       "301 Moved Permanently",
			statusCode: http.StatusMovedPermanently,
			wantErr:    true,
			errMsg:     "redirect",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				if err := json.NewEncoder(w).Encode(map[string]string{"status": "test"}); err != nil {
					t.Logf("Failed to encode response: %v", err)
				}
			}))
			defer server.Close()

			// Create HTTP client
			client := newHTTPClient(server.URL, 5*time.Second, 0)

			// Make request
			ctx := context.Background()
			_, err := client.Get(ctx, "/test")

			// Check error
			if (err != nil) != tt.wantErr {
				t.Errorf("Get() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Check error message contains expected text
			if err != nil && tt.errMsg != "" {
				httpErr, ok := err.(ErrHTTPRequest)
				if !ok {
					t.Errorf("Expected ErrHTTPRequest, got %T", err)
				} else if !contains(httpErr.Message, tt.errMsg) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tt.errMsg, httpErr.Message)
				}
			}
		})
	}
}

// TestStatusCodeHelpers tests the status code helper functions
func TestStatusCodeHelpers(t *testing.T) {
	tests := []struct {
		code       int
		isSuccess  bool
		isRedirect bool
		isClient   bool
		isServer   bool
	}{
		{http.StatusOK, true, false, false, false},
		{http.StatusCreated, true, false, false, false},
		{http.StatusMovedPermanently, false, true, false, false},
		{http.StatusFound, false, true, false, false},
		{http.StatusBadRequest, false, false, true, false},
		{http.StatusNotFound, false, false, true, false},
		{http.StatusInternalServerError, false, false, false, true},
		{http.StatusBadGateway, false, false, false, true},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.code), func(t *testing.T) {
			if got := isSuccess(tt.code); got != tt.isSuccess {
				t.Errorf("isSuccess(%d) = %v, want %v", tt.code, got, tt.isSuccess)
			}
			if got := isRedirect(tt.code); got != tt.isRedirect {
				t.Errorf("isRedirect(%d) = %v, want %v", tt.code, got, tt.isRedirect)
			}
			if got := isClientError(tt.code); got != tt.isClient {
				t.Errorf("isClientError(%d) = %v, want %v", tt.code, got, tt.isClient)
			}
			if got := isServerError(tt.code); got != tt.isServer {
				t.Errorf("isServerError(%d) = %v, want %v", tt.code, got, tt.isServer)
			}
		})
	}
}

// TestRemoteTokenizerHealthCheck tests the IsHealthy method with empty string
func TestRemoteTokenizerHealthCheck(t *testing.T) {
	tests := []struct {
		name           string
		serverResponse func(w http.ResponseWriter, r *http.Request)
		expectHealthy  bool
	}{
		{
			name: "healthy service with empty string",
			serverResponse: func(w http.ResponseWriter, r *http.Request) {
				// Verify the request contains empty string
				var req map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Logf("Failed to decode request: %v", err)
				}

				// Check that prompt is empty string
				if prompt, ok := req["prompt"].(string); ok && prompt != "" {
					t.Errorf("Expected empty prompt, got: %s", prompt)
				}

				// Return successful empty tokenization response
				w.WriteHeader(http.StatusOK)
				if err := json.NewEncoder(w).Encode(map[string]interface{}{
					"tokens": []int{},
					"count":  0,
				}); err != nil {
					t.Errorf("Failed to encode response: %v", err)
				}
			},
			expectHealthy: true,
		},
		{
			name: "unhealthy service returns error",
			serverResponse: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				if err := json.NewEncoder(w).Encode(map[string]string{"error": "service unavailable"}); err != nil {
					t.Logf("Failed to encode response: %v", err)
				}
			},
			expectHealthy: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/tokenize" {
					tt.serverResponse(w, r)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			// Create remote tokenizer
			config := RemoteTokenizerConfig{
				Engine:   "vllm",
				Endpoint: server.URL,
				Timeout:  5 * time.Second,
			}
			tokenizer, err := NewRemoteTokenizer(config)
			if err != nil {
				t.Fatalf("Failed to create tokenizer: %v", err)
			}

			// Test health check
			ctx := context.Background()
			// Use type assertion to access remote-specific methods
			remoteTokenizer, ok := tokenizer.(interface {
				IsHealthy(context.Context) bool
			})
			if !ok {
				t.Fatal("Tokenizer does not implement IsHealthy method")
			}
			healthy := remoteTokenizer.IsHealthy(ctx)

			if healthy != tt.expectHealthy {
				t.Errorf("IsHealthy() = %v, want %v", healthy, tt.expectHealthy)
			}
		})
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[:len(substr)] == substr ||
		len(s) > len(substr) && contains(s[1:], substr)
}
