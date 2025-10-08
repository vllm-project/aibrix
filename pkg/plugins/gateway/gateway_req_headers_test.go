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

package gateway

import (
	"context"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
)

// Test_HandleRequestHeaders_StateMachine tests HandleRequestHeaders in state machine mode
// In state machine mode, HandleRequestHeaders should extract headers info without performing routing
func Test_HandleRequestHeaders_StateMachine(t *testing.T) {
	tests := []struct {
		name            string
		headers         map[string]string
		expectResponse  bool // Should response be non-nil (for legacy mode routing)?
		expectRPM       int64
		expectAlgorithm types.RoutingAlgorithm
	}{
		{
			name: "state machine mode - extract routing info",
			headers: map[string]string{
				":path":            "/v1/chat/completions",
				":method":          "POST",
				"content-type":     "application/json",
				"routing-strategy": "random",
			},
			expectResponse:  false, // In state machine mode, no immediate routing response
			expectRPM:       0,     // Rate limiting not tested here
			expectAlgorithm: "random",
		},
		{
			name: "state machine mode - no routing algorithm specified",
			headers: map[string]string{
				":path":        "/v1/chat/completions",
				":method":      "POST",
				"content-type": "application/json",
			},
			expectResponse:  false,
			expectRPM:       0,
			expectAlgorithm: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(subtest *testing.T) {
			// Initialize mock cache
			mockCache := &MockCache{Cache: cache.NewForTest()}

			// Create server in STATE MACHINE mode (useLegacyMode = false)
			server := &Server{
				cache:         mockCache,
				useLegacyMode: false, // State machine mode
			}

			// Build request headers
			var envoyHeaders []*configPb.HeaderValue
			for key, value := range tt.headers {
				envoyHeaders = append(envoyHeaders, &configPb.HeaderValue{
					Key:      key,
					RawValue: []byte(value),
				})
			}

			// Create request
			req := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extProcPb.HttpHeaders{
						Headers: &configPb.HeaderMap{
							Headers: envoyHeaders,
						},
					},
				},
			}

			// Call HandleRequestHeaders
			resp, _, rpm, routingCtx := server.HandleRequestHeaders(
				context.Background(),
				"test-request-id",
				req,
			)

			// Validate response
			// In state machine mode, HandleRequestHeaders should:
			// 1. Extract routing context
			// 2. NOT perform immediate routing (that's done by scheduler)
			// 3. Return nil response (state machine will build the actual response)

			assert.Nil(subtest, resp, "response should be nil in state machine mode")
			assert.Equal(subtest, tt.expectRPM, rpm)
			assert.NotNil(subtest, routingCtx, "routing context should be created")

			// Verify routing context contains extracted info
			assert.Equal(subtest, "/v1/chat/completions", routingCtx.ReqPath)
			assert.Equal(subtest, tt.expectAlgorithm, routingCtx.Algorithm)
			assert.NotNil(subtest, routingCtx.ReqHeaders)
		})
	}
}

// Note: Legacy mode tests are covered by the existing Test_handleRequestBody tests
// which set useLegacyMode = true. We focus on state machine mode tests here.

// Test_HandleRequestHeaders_RoutingAlgorithmExtraction tests routing algorithm extraction
func Test_HandleRequestHeaders_RoutingAlgorithmExtraction(t *testing.T) {
	tests := []struct {
		name              string
		headers           map[string]string
		expectedAlgorithm types.RoutingAlgorithm
	}{
		{
			name: "extract routing algorithm from header",
			headers: map[string]string{
				":path":            "/v1/chat/completions",
				"routing-strategy": "random",
			},
			expectedAlgorithm: "random",
		},
		{
			name: "no routing algorithm specified",
			headers: map[string]string{
				":path": "/v1/chat/completions",
			},
			expectedAlgorithm: "", // Default/empty
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(subtest *testing.T) {
			mockCache := &MockCache{Cache: cache.NewForTest()}
			server := &Server{
				cache:         mockCache,
				useLegacyMode: false,
			}

			var envoyHeaders []*configPb.HeaderValue
			for key, value := range tt.headers {
				envoyHeaders = append(envoyHeaders, &configPb.HeaderValue{
					Key:      key,
					RawValue: []byte(value),
				})
			}

			req := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extProcPb.HttpHeaders{
						Headers: &configPb.HeaderMap{
							Headers: envoyHeaders,
						},
					},
				},
			}

			_, _, _, routingCtx := server.HandleRequestHeaders(
				context.Background(),
				"test-request-id",
				req,
			)

			assert.NotNil(subtest, routingCtx)
			assert.Equal(subtest, tt.expectedAlgorithm, routingCtx.Algorithm)
		})
	}
}
