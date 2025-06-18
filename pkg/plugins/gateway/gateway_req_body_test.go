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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/vllm-project/aibrix/pkg/metrics"
	routingalgorithms "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

// TestRouterAlgorithm is a dedicated routing algorithm for testing
const TestRouterAlgorithm types.RoutingAlgorithm = "test-router"
const RouterNotSet types.RoutingAlgorithm = "not-set"

// MockCache implements cache.Cache interface for testing
type MockCache struct {
	mock.Mock
}

func (m *MockCache) HasModel(model string) bool {
	args := m.Called(model)
	return args.Bool(0)
}

func (m *MockCache) ListPodsByModel(model string) (types.PodList, error) {
	args := m.Called(model)
	return args.Get(0).(types.PodList), args.Error(1)
}

func (m *MockCache) AddRequestCount(ctx *types.RoutingContext, requestID string, model string) int64 {
	args := m.Called(ctx, requestID, model)
	return args.Get(0).(int64)
}

func (m *MockCache) DoneRequestCount(ctx *types.RoutingContext, requestID string, model string, term int64) {
	m.Called(ctx, requestID, model, term)
}

func (m *MockCache) DoneRequestTrace(ctx *types.RoutingContext, requestID string, model string, term int64, inputTokens int64, outputTokens int64) {
	m.Called(ctx, requestID, model, term, inputTokens, outputTokens)
}

func (m *MockCache) AddSubscriber(subscriber metrics.MetricSubscriber) {
	m.Called(subscriber)
}

func (m *MockCache) GetMetricValueByPod(namespace string, podName string, metricName string) (metrics.MetricValue, error) {
	args := m.Called(namespace, podName, metricName)
	return args.Get(0).(metrics.MetricValue), args.Error(1)
}

func (m *MockCache) GetMetricValueByPodModel(namespace string, podName string, model string, metricName string) (metrics.MetricValue, error) {
	args := m.Called(namespace, podName, model, metricName)
	return args.Get(0).(metrics.MetricValue), args.Error(1)
}

func (m *MockCache) GetPod(namespace string, podName string) (*v1.Pod, error) {
	args := m.Called(namespace, podName)
	return args.Get(0).(*v1.Pod), args.Error(1)
}

func (m *MockCache) ListModels() []string {
	args := m.Called()
	return args.Get(0).([]string)
}

func (m *MockCache) ListModelsByPod(namespace string, podName string) ([]string, error) {
	args := m.Called(namespace, podName)
	return args.Get(0).([]string), args.Error(1)
}

// MockPodList implements types.PodList interface for testing
type MockPodList struct {
	pods []*v1.Pod
}

func (m *MockPodList) All() []*v1.Pod {
	return m.pods
}

func (m *MockPodList) Len() int {
	return len(m.pods)
}

func (m *MockPodList) Indexes() []string {
	return []string{} // For testing, we don't need to implement actual indexing
}

func (m *MockPodList) ListByIndex(index string) []*v1.Pod {
	return []*v1.Pod{} // For testing, we don't need to implement actual indexing
}

// MockRouter implements types.Router interface for testing
type MockRouter struct {
	mock.Mock
}

func (m *MockRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	args := m.Called(ctx, pods)
	return args.String(0), args.Error(1)
}

func (m *MockRouter) Name() string {
	return "mock-router"
}

func TestHandleRequestBody(t *testing.T) {
	// Initialize routing algorithms
	routingalgorithms.Init()


	// testResponse represents the expected response values from HandleRequestBody
	type testResponse struct {
		statusCode envoyTypePb.StatusCode
		headers    []*configPb.HeaderValueOption
		model      string
		routingCtx *types.RoutingContext
		stream     bool
		term       int64
	}

	// testCase represents a test case with its validation function
	type testCase struct {
		name        string
		requestBody string
		user        utils.User
		routingAlgo types.RoutingAlgorithm
		mockSetup   func(*MockCache, *MockRouter)
		expected    testResponse
		validate    func(*testing.T, *testCase, *extProcPb.ProcessingResponse, string, *types.RoutingContext, bool, int64)
		checkStream bool
	}
	tests := []testCase{
		{
			name:        "no routing strategy - should only set model header",
			requestBody: `{"model": "test-model", "messages": [{"role": "user", "content": "test"}]}`,
			user: utils.User{
				Name: "test-user",
			},
			routingAlgo: "", // No routing strategy
			mockSetup: func(mockCache *MockCache, _ *MockRouter) {
				mockCache.On("HasModel", "test-model").Return(true)
				podList := &MockPodList{
					pods: []*v1.Pod{
						{
							Status: v1.PodStatus{
								PodIP: "1.2.3.4",
								Conditions: []v1.PodCondition{
									{
										Type:   v1.PodReady,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
						{
							Status: v1.PodStatus{
								PodIP: "4.5.6.7",
								Conditions: []v1.PodCondition{
									{
										Type:   v1.PodReady,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
					},
				}
				mockCache.On("ListPodsByModel", "test-model").Return(podList, nil)
				mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1))
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_OK,
				headers: []*configPb.HeaderValueOption{
					{
						Header: &configPb.HeaderValue{
							Key:      HeaderModel,
							RawValue: []byte("test-model"),
						},
					},
				},
				model:      "test-model",
				stream:     false,
				term:       1,
				routingCtx: &types.RoutingContext{},
			},
			validate: func(t *testing.T, tt *testCase, resp *extProcPb.ProcessingResponse, model string, routingCtx *types.RoutingContext, stream bool, term int64) {
				assert.Equal(t, tt.expected.statusCode, envoyTypePb.StatusCode_OK)
				assert.Equal(t, tt.expected.headers, resp.GetRequestBody().GetResponse().GetHeaderMutation().GetSetHeaders())
				assert.Equal(t, tt.expected.model, model)
				assert.Equal(t, tt.expected.stream, stream)
				assert.Equal(t, tt.expected.term, term)
				assert.NotNil(t, routingCtx)
				assert.Equal(t, tt.expected.model, routingCtx.Model)
				assert.Equal(t, tt.routingAlgo, routingCtx.Algorithm)
				// Verify no routing headers are set
				for _, header := range resp.GetRequestBody().GetResponse().GetHeaderMutation().GetSetHeaders() {
					assert.NotEqual(t, HeaderRoutingStrategy, header.Header.Key)
					assert.NotEqual(t, HeaderTargetPod, header.Header.Key)
				}
			},
			checkStream: false,
		},
		{
			name:        "model not in cache - should return error",
			requestBody: `{"model": "unknown-model", "messages": [{"role": "user", "content": "test"}]}`,
			user: utils.User{
				Name: "test-user",
			},
			routingAlgo: "", // No routing strategy needed for this test
			mockSetup: func(mockCache *MockCache, _ *MockRouter) {
				mockCache.On("HasModel", "unknown-model").Return(false)
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_BadRequest,
				headers: []*configPb.HeaderValueOption{
					{
						Header: &configPb.HeaderValue{
							Key:      HeaderErrorNoModelBackends,
							RawValue: []byte("unknown-model"),
						},
					},
					{
						Header: &configPb.HeaderValue{
							Key:   "Content-Type",
							Value: "application/json",
						},
					},
				},
				model:      "unknown-model",
				stream:     false,
				term:       0,
				routingCtx: nil,
			},
			validate: func(t *testing.T, tt *testCase, resp *extProcPb.ProcessingResponse, model string, routingCtx *types.RoutingContext, stream bool, term int64) {
				assert.Equal(t, tt.expected.statusCode, resp.GetImmediateResponse().GetStatus().GetCode())
				assert.Equal(t, tt.expected.headers, resp.GetImmediateResponse().GetHeaders().GetSetHeaders())
				assert.Equal(t, tt.expected.model, model)
				assert.Equal(t, tt.expected.stream, stream)
				assert.Equal(t, tt.expected.term, term)
				assert.Nil(t, routingCtx)
			},
			checkStream: false,
		},
		{
			name:        "valid routing strategy - should set both routing and target pod headers",
			requestBody: `{"model": "test-model", "messages": [{"role": "user", "content": "test"}]}`,
			user: utils.User{
				Name: "test-user",
			},
			routingAlgo: TestRouterAlgorithm,
			mockSetup: func(mockCache *MockCache, mockRouter *MockRouter) {
				podList := &MockPodList{
					pods: []*v1.Pod{
						{
							Status: v1.PodStatus{
								PodIP: "1.2.3.4",
								Conditions: []v1.PodCondition{
									{
										Type:   v1.PodReady,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
						{
							Status: v1.PodStatus{
								PodIP: "4.5.6.7",
								Conditions: []v1.PodCondition{
									{
										Type:   v1.PodReady,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
					},
				}
				mockCache.On("HasModel", "test-model").Return(true)
				mockCache.On("ListPodsByModel", "test-model").Return(podList, nil)
				mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1))
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).Once()
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_OK,
				headers: []*configPb.HeaderValueOption{
					{
						Header: &configPb.HeaderValue{
							Key:      HeaderRoutingStrategy,
							RawValue: []byte("test-router"),
						},
					},
					{
						Header: &configPb.HeaderValue{
							Key:      HeaderTargetPod,
							RawValue: []byte("1.2.3.4:8000"),
						},
					},
				},
				model:      "test-model",
				stream:     false,
				term:       1,
				routingCtx: &types.RoutingContext{},
			},
			validate: func(t *testing.T, tt *testCase, resp *extProcPb.ProcessingResponse, model string, routingCtx *types.RoutingContext, stream bool, term int64) {
				assert.Equal(t, tt.expected.statusCode, envoyTypePb.StatusCode_OK)
				assert.Equal(t, tt.expected.headers, resp.GetRequestBody().GetResponse().GetHeaderMutation().GetSetHeaders())
				assert.Equal(t, tt.expected.model, model)
				assert.Equal(t, tt.expected.stream, stream)
				assert.Equal(t, tt.expected.term, term)
				assert.NotNil(t, routingCtx)
				assert.Equal(t, tt.expected.model, routingCtx.Model)
				assert.Equal(t, tt.routingAlgo, routingCtx.Algorithm)
				// Verify both routing headers are set
				foundRoutingStrategy := false
				foundTargetPod := false
				for _, header := range resp.GetRequestBody().GetResponse().GetHeaderMutation().GetSetHeaders() {
					if header.Header.Key == HeaderRoutingStrategy {
						foundRoutingStrategy = true
						assert.Equal(t, "test-router", string(header.Header.RawValue))
					}
					if header.Header.Key == HeaderTargetPod {
						foundTargetPod = true
						assert.Equal(t, "1.2.3.4:8000", string(header.Header.RawValue))
					}
				}
				assert.True(t, foundRoutingStrategy, "HeaderRoutingStrategy not found")
				assert.True(t, foundTargetPod, "HeaderTargetPod not found")
			},
			checkStream: false,
		},
		{
			name:        "invalid routing strategy - should fallback to random router",
			requestBody: `{"model": "test-model", "messages": [{"role": "user", "content": "test"}]}`,
			user: utils.User{
				Name: "test-user",
			},
			routingAlgo: "invalid-router", // Invalid routing strategy
			mockSetup: func(mockCache *MockCache, _ *MockRouter) {
				mockCache.On("HasModel", "test-model").Return(true)
				podList := &MockPodList{
					pods: []*v1.Pod{
						{
							Status: v1.PodStatus{
								PodIP: "1.2.3.4",
								Conditions: []v1.PodCondition{
									{
										Type:   v1.PodReady,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
						{
							Status: v1.PodStatus{
								PodIP: "5.6.7.8",
								Conditions: []v1.PodCondition{
									{
										Type:   v1.PodReady,
										Status: v1.ConditionTrue,
									},
								},
							},
						},
					},
				}
				mockCache.On("ListPodsByModel", "test-model").Return(podList, nil)
				mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1))
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_OK,
				headers: []*configPb.HeaderValueOption{
					{
						Header: &configPb.HeaderValue{
							Key:      HeaderRoutingStrategy,
							RawValue: []byte("invalid-router"),
						},
					},
					{
						Header: &configPb.HeaderValue{
							Key:      HeaderTargetPod,
							RawValue: []byte("1.2.3.4:8000"),
						},
					},
				},
				model:      "test-model",
				stream:     false,
				term:       1,
				routingCtx: &types.RoutingContext{},
			},
			validate: func(t *testing.T, tt *testCase, resp *extProcPb.ProcessingResponse, model string, routingCtx *types.RoutingContext, stream bool, term int64) {
				assert.Equal(t, tt.expected.statusCode, envoyTypePb.StatusCode_OK)
				assert.Equal(t, tt.expected.model, model)
				assert.Equal(t, tt.expected.stream, stream)
				assert.Equal(t, tt.expected.term, term)
				assert.NotNil(t, routingCtx)
				assert.Equal(t, tt.expected.model, routingCtx.Model)
				assert.Equal(t, tt.routingAlgo, routingCtx.Algorithm)
				// Verify both routing headers are set
				foundRoutingStrategy := false
				foundTargetPod := false
				for _, header := range resp.GetRequestBody().GetResponse().GetHeaderMutation().GetSetHeaders() {
					if header.Header.Key == HeaderRoutingStrategy {
						foundRoutingStrategy = true
						assert.Equal(t, "invalid-router", string(header.Header.RawValue))
					}
					if header.Header.Key == HeaderTargetPod {
						foundTargetPod = true
						// Since this is a random router, accept either valid pod IP from the mock setup
						targetPodIP := string(header.Header.RawValue)
						assert.Contains(t, []string{"1.2.3.4:8000", "5.6.7.8:8000"}, targetPodIP, "Target pod IP should be one of the pod IPs from the mock setup")
					}
				}
				assert.True(t, foundRoutingStrategy, "HeaderRoutingStrategy not found")
				assert.True(t, foundTargetPod, "HeaderTargetPod not found")
			},
			checkStream: false,
		},
		{
			name:        "no routable pods available - should return ServiceUnavailable",
			requestBody: `{"model": "test-model", "messages": [{"role": "user", "content": "test"}]}`,
			user: utils.User{
				Name: "test-user",
			},
			routingAlgo: "", // No routing strategy needed for this test
			mockSetup: func(mockCache *MockCache, _ *MockRouter) {
				mockCache.On("HasModel", "test-model").Return(true)
				// Create pods that exist but are not routable (not ready)
				podList := &MockPodList{
					pods: []*v1.Pod{
						{
							Status: v1.PodStatus{
								PodIP: "1.2.3.4",
								Conditions: []v1.PodCondition{
									{
										Type:   v1.PodReady,
										Status: v1.ConditionFalse, // Not ready
									},
								},
							},
						},
						{
							Status: v1.PodStatus{
								PodIP: "5.6.7.8",
								Conditions: []v1.PodCondition{
									{
										Type:   v1.PodReady,
										Status: v1.ConditionFalse, // Not ready
									},
								},
							},
						},
					},
				}
				mockCache.On("ListPodsByModel", "test-model").Return(podList, nil)
				// No AddRequestCount expectation since the function should return early with error
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_ServiceUnavailable,
				headers: []*configPb.HeaderValueOption{
					{
						Header: &configPb.HeaderValue{
							Key:      HeaderErrorNoModelBackends,
							RawValue: []byte("true"),
						},
					},
					{
						Header: &configPb.HeaderValue{
							Key:   "Content-Type",
							Value: "application/json",
						},
					},
				},
				model:      "test-model",
				stream:     false,
				term:       0,
				routingCtx: nil,
			},
			validate: func(t *testing.T, tt *testCase, resp *extProcPb.ProcessingResponse, model string, routingCtx *types.RoutingContext, stream bool, term int64) {
				assert.Equal(t, tt.expected.statusCode, resp.GetImmediateResponse().GetStatus().GetCode())
				assert.Equal(t, tt.expected.headers, resp.GetImmediateResponse().GetHeaders().GetSetHeaders())
				assert.Equal(t, tt.expected.model, model)
				assert.Equal(t, tt.expected.stream, stream)
				assert.Equal(t, tt.expected.term, term)
				assert.Nil(t, routingCtx)
			},
			checkStream: false,
		},
		{
			name:        "routable pod without IP - should return ServiceUnavailable",
			requestBody: `{"model": "test-model", "messages": [{"role": "user", "content": "test"}]}`,
			user: utils.User{
				Name: "test-user",
			},
			routingAlgo: "", // No routing strategy needed for this test
			mockSetup: func(mockCache *MockCache, _ *MockRouter) {
				mockCache.On("HasModel", "test-model").Return(true)
				// Create pods that exist but are not routable (not ready)
				podList := &MockPodList{
					pods: []*v1.Pod{
						{
							Status: v1.PodStatus{
								PodIP: "",
								Conditions: []v1.PodCondition{
									{
										Type:   v1.PodReady,
										Status: v1.ConditionTrue, // Not ready
									},
								},
							},
						},
					},
				}
				mockCache.On("ListPodsByModel", "test-model").Return(podList, nil)
				// No AddRequestCount expectation since the function should return early with error
			},
			expected: testResponse{
				statusCode: envoyTypePb.StatusCode_ServiceUnavailable,
				headers: []*configPb.HeaderValueOption{
					{
						Header: &configPb.HeaderValue{
							Key:      HeaderErrorNoModelBackends,
							RawValue: []byte("true"),
						},
					},
					{
						Header: &configPb.HeaderValue{
							Key:   "Content-Type",
							Value: "application/json",
						},
					},
				},
				model:      "test-model",
				stream:     false,
				term:       0,
				routingCtx: nil,
			},
			validate: func(t *testing.T, tt *testCase, resp *extProcPb.ProcessingResponse, model string, routingCtx *types.RoutingContext, stream bool, term int64) {
				assert.Equal(t, tt.expected.statusCode, resp.GetImmediateResponse().GetStatus().GetCode())
				assert.Equal(t, tt.expected.headers, resp.GetImmediateResponse().GetHeaders().GetSetHeaders())
				assert.Equal(t, tt.expected.model, model)
				assert.Equal(t, tt.expected.stream, stream)
				assert.Equal(t, tt.expected.term, term)
				assert.Nil(t, routingCtx)
			},
			checkStream: false,
		},
	}

	for _, tt := range tests {
		tt := tt // Create new variable for each test case
		t.Run(tt.name, func(t *testing.T) {
			// Add panic recovery for subtests too
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Subtest %v panicked: %v", tt.name, r)
					t.FailNow()
				}
			}()

			// Initialize mock cache and router
			mockCache := new(MockCache)
			mockRouter := new(MockRouter)
			if tt.mockSetup != nil {
				tt.mockSetup(mockCache, mockRouter)
			}

			// Register mock router for this test case if needed
			if tt.routingAlgo != "" && tt.routingAlgo != "not-set" && tt.routingAlgo != "invalid-router" {
				mockRouterProvider := func() (types.Router, error) {
					return mockRouter, nil
				}
				routingalgorithms.Register(tt.routingAlgo, mockRouterProvider)
				routingalgorithms.Init()
			}

			// Create server with mock cache
			server := &Server{
				cache: mockCache,
			}

			// Create request
			req := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestBody{
					RequestBody: &extProcPb.HttpBody{
						Body: []byte(tt.requestBody),
					},
				},
			}

			// Call HandleRequestBody
			resp, model, routingCtx, stream, term := server.HandleRequestBody(
				context.Background(),
				"test-request-id",
				"/v1/chat/completions",
				req,
				tt.user,
				tt.routingAlgo,
			)

			// Validate response using test-specific validation function
			tt.validate(t, &tt, resp, model, routingCtx, stream, term)

			// Verify all mock expectations were met
			mockCache.AssertExpectations(t)
			mockRouter.AssertExpectations(t)
		})
	}
}
