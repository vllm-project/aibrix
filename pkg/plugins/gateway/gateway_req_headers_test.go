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
	"context"
	"fmt"
	"os"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"

	routingalgorithms "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

var redisClient *redis.Client

func TestMain(m *testing.M) {
	redisClient = utils.GetRedisClient()
	if redisClient == nil {
		fmt.Println("Warning: Failed to connect to Redis. VTC routing tests will be skipped.")
	}

	code := m.Run()

	if redisClient != nil {
		if err := redisClient.Close(); err != nil {
			fmt.Printf("Error closing Redis client: %v\n", err)
		}
	}

	os.Exit(code)
}

// Test_handleRequestBody tests the HandleRequestBody function for various scenarios
func Test_handleRequestHeaders(t *testing.T) {
	// Initialize routing algorithms
	routingalgorithms.Init()

	// testResponse represents the expected response values from HandleRequestBody
	type testResponse struct {
		statusCode envoyTypePb.StatusCode
		headers    []*configPb.HeaderValueOption
		routingCtx *types.RoutingContext
		rpm        int64
	}

	// testCase represents a test case with its validation function
	type testCase struct {
		name           string
		requestHeaders []*configPb.HeaderValue
		user           utils.User
		mockSetup      func(*MockCache, *mockRouter)
		expected       testResponse
		validate       func(*testing.T, *testCase, *extProcPb.ProcessingResponse, utils.User, *types.RoutingContext, int64)
	}

	// Define test cases for different routing and error scenarios
	tests := []testCase{
		{
			name: "no routing strategy - should only set model header",
			requestHeaders: []*configPb.HeaderValue{
				{
					Key:      userKey,
					RawValue: []byte("test-user"),
				},
				{
					Key:      pathKey,
					RawValue: []byte("/v1/chat"),
				},
				{
					Key:      authorizationKey,
					RawValue: []byte("token:aibrix"),
				},
				{
					Key:      HeaderRoutingStrategy,
					RawValue: []byte("not-found-strategy"),
				},
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_OK,
				headers:    []*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{Key: HeaderModel, RawValue: []byte("test-model")}}},
				routingCtx: &types.RoutingContext{},
			},
			validate: func(t *testing.T, tt *testCase, resp *extProcPb.ProcessingResponse, user utils.User, routingCtx *types.RoutingContext, rpm int64) {
				// Validate that only the model header is set and no routing headers are present
				assert.Equal(t, tt.expected.statusCode, envoyTypePb.StatusCode_OK)
				assert.Equal(t, tt.expected.headers, resp.GetRequestBody().GetResponse().GetHeaderMutation().GetSetHeaders())
				assert.NotNil(t, routingCtx)
				// Verify no routing headers are set
				for _, header := range resp.GetRequestBody().GetResponse().GetHeaderMutation().GetSetHeaders() {
					assert.NotEqual(t, HeaderRoutingStrategy, header.Header.Key)
					assert.NotEqual(t, HeaderTargetPod, header.Header.Key)
				}
			},
		},
	}

	for _, tt := range tests {
		// Run each test case as a subtest
		t.Run(tt.name, func(subtest *testing.T) {
			subtest.Parallel()
			// Add panic recovery for subtests too
			subtest.Cleanup(func() {
				if r := recover(); r != nil {
					subtest.Errorf("Subtest %v panicked: %v", tt.name, r)
					subtest.FailNow()
				}
			})
			mockRouter := new(mockRouter)

			// Create server with mock cache
			server := &Server{
				redisClient: redisClient,
			}

			// Create request for the test case
			req := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extProcPb.HttpHeaders{
						Headers: &configPb.HeaderMap{Headers: tt.requestHeaders},
					},
				},
			}

			resp, user, rpm, routingCtx := server.HandleRequestHeaders(
				context.Background(),
				"test-request-id",
				req,
			)

			// Validate response using test-specific validation function
			tt.validate(subtest, &tt, resp, user, routingCtx, rpm)

			// Verify all mock expectations were met
			mockRouter.AssertExpectations(subtest)
		})
	}
}
