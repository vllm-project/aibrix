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
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func Test_ValidateRoutingStrategy(t *testing.T) {
	var tests = []struct {
		routingStrategy    string
		message            string
		expectedValidation bool
	}{
		{
			routingStrategy:    "",
			message:            "empty routing strategy",
			expectedValidation: false,
		},
		{
			routingStrategy:    "  ",
			message:            "spaced routing strategy",
			expectedValidation: false,
		},
		{
			routingStrategy:    "random",
			message:            "random routing strategy",
			expectedValidation: true,
		},
		{
			routingStrategy:    "least-request",
			message:            "least-request routing strategy",
			expectedValidation: true,
		},
		{
			routingStrategy:    "rrandom",
			message:            "misspell routing strategy",
			expectedValidation: false,
		},
	}
	cache.NewForTest()
	routing.Init()
	for _, tt := range tests {
		_, currentValidation := routing.Validate(tt.routingStrategy)
		assert.Equal(t, tt.expectedValidation, currentValidation, tt.message)
	}
}

func Test_buildEnvoyProxyHeaders(t *testing.T) {
	headers := []*configPb.HeaderValueOption{}

	headers = buildEnvoyProxyHeaders(headers, "key1", "value1", "key2")
	assert.Equal(t, 0, len(headers))

	headers = buildEnvoyProxyHeaders(headers, "key1", "value1", "key2", "value2")
	assert.Equal(t, 2, len(headers))

	headers = buildEnvoyProxyHeaders(headers, "key3", "value3")
	assert.Equal(t, 3, len(headers))
}

// Test_selectTargetPod tests the selectTargetPod method for various pod selection scenarios
func Test_selectTargetPod(t *testing.T) {
	// Initialize routing algorithms for the test
	routing.Init()

	// Define test cases for different pod selection and error scenarios
	tests := []struct {
		name           string
		pods           types.PodList
		mockSetup      func(*mockRouter, types.RoutingAlgorithm)
		expectedError  bool
		expectedPodIP  string
		externalFilter string
	}{
		{
			name: "routing.Route returns error",
			pods: &utils.PodArray{Pods: []*v1.Pod{{
				Status: v1.PodStatus{
					PodIP:      "1.2.3.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			},
				{
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				}}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that returns an error
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("", errors.New("test error"))

			},
			expectedError: true,
		},
		{
			name: "no pods available",
			pods: &utils.PodArray{Pods: []*v1.Pod{}},
			mockSetup: func(m *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router, but no pods are available
				routing.Register(algo, func() (types.Router, error) {
					return m, nil
				})
				// No expectations needed as pods.Len() == 0
			},
			expectedError: true,
		},
		{
			name: "no ready pods available",
			pods: &utils.PodArray{Pods: []*v1.Pod{{
				Status: v1.PodStatus{
					PodIP:      "1.2.3.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}},
				},
			}}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router, but no pods are ready
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				// No expectations needed as no ready pods
			},
			expectedError: true,
		},
		{
			name: "single ready pod",
			pods: &utils.PodArray{Pods: []*v1.Pod{{
				Status: v1.PodStatus{
					PodIP:      "1.2.3.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			}}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router, but only one pod is ready so Route should not be called
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				// Explicitly set expectation that Route should not be called
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError: false,
			expectedPodIP: "1.2.3.4:8000",
		},
		{
			name: "single ready pod out of two",
			pods: &utils.PodArray{Pods: []*v1.Pod{{
				Status: v1.PodStatus{
					PodIP:      "8.9.10.11",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			},
				{
					Status: v1.PodStatus{
						PodIP:      "4.5.6.7",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}},
					},
				}}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router, but only one pod is ready so Route should not be called
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				// Explicitly set expectation that Route should not be called
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError: false,
			expectedPodIP: "8.9.10.11:8000",
		},
		{
			name: "multiple ready pods",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).Once()
			},
			expectedError: false,
			expectedPodIP: "1.2.3.4:8000",
		},
		{
			name: "single external filter",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "sad",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError:  false,
			expectedPodIP:  "1.2.3.4:8000",
			externalFilter: "foo=bar",
		},
		{
			name: "slice external filter",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"env": "prod",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
							"env": "prod",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "2.3.4.5",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError:  false,
			expectedPodIP:  "2.3.4.5:8000",
			externalFilter: "foo=bar,env=prod",
		},
		{
			name: "external filter and route multiple pods",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).Once()
			},
			expectedError:  false,
			expectedPodIP:  "1.2.3.4:8000",
			externalFilter: "foo=bar",
		},
		{
			name: "external filter use 'in' and route multiple pods",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bug",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).Once()
			},
			expectedError:  false,
			expectedPodIP:  "1.2.3.4:8000",
			externalFilter: "foo in (bar, bug)",
		},
		{
			name: "external filter use 'not in' and route multiple pods",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bug",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "par",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "2.3.4.5",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError:  false,
			expectedPodIP:  "2.3.4.5:8000",
			externalFilter: "foo notin (bar, bug)",
		},
		{
			name: "external filter use !=",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bug",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError:  false,
			expectedPodIP:  "5.6.7.8:8000",
			externalFilter: "foo!=bar",
		},
		{
			name: "external filter with key exists",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "sad",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).Once()
			},
			expectedError:  false,
			expectedPodIP:  "1.2.3.4:8000",
			externalFilter: "foo",
		},
		{
			name: "external filter with key not exists",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"sad": "sad",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError:  false,
			expectedPodIP:  "5.6.7.8:8000",
			externalFilter: "!foo",
		},
	}

	for _, tt := range tests {
		// Run each test case as a subtest
		t.Run(tt.name, func(subtest *testing.T) {
			subtest.Parallel() // Run subtests in parallel
			mockRouter := new(mockRouter)
			routingAlgo := types.RoutingAlgorithm(fmt.Sprintf("test-router-%s", tt.name))

			// Set up the mock router and register the routing algorithm for this test
			tt.mockSetup(mockRouter, routingAlgo)
			routing.Init()

			server := &Server{}
			ctx := types.NewRoutingContext(context.Background(), routingAlgo, "test-model", "test-message", "test-request", "test-user")

			// Call selectTargetPod and check the result
			podIP, err := server.selectTargetPod(ctx, tt.pods, tt.externalFilter)

			if tt.expectedError {
				assert.Error(subtest, err)
			} else {
				assert.NoError(subtest, err)
				assert.Equal(subtest, tt.expectedPodIP, podIP)
			}

			// Ensure all mock expectations are met
			mockRouter.AssertExpectations(subtest)
		})
	}
}

func TestValidateHTTPRouteStatus(t *testing.T) {
	tests := []struct {
		name        string
		model       string
		setupMock   func(*MockGatewayClient, *MockGatewayV1Client, *MockHTTPRouteClient)
		wantErr     bool
		errContains string
	}{
		{
			name:  "successful validation",
			model: "test-model",
			setupMock: func(gw *MockGatewayClient, gwv1 *MockGatewayV1Client, http *MockHTTPRouteClient) {
				gw.On("GatewayV1").Return(gwv1)
				gwv1.On("HTTPRoutes", "aibrix-system").Return(http)

				route := &gatewayv1.HTTPRoute{
					Status: gatewayv1.HTTPRouteStatus{
						RouteStatus: gatewayv1.RouteStatus{
							Parents: []gatewayv1.RouteParentStatus{{
								Conditions: []metav1.Condition{{
									Type:   string(gatewayv1.RouteConditionAccepted),
									Reason: string(gatewayv1.RouteReasonAccepted),
									Status: metav1.ConditionTrue,
								}, {
									Type:   string(gatewayv1.RouteConditionResolvedRefs),
									Reason: string(gatewayv1.RouteReasonResolvedRefs),
									Status: metav1.ConditionTrue,
								}},
							}},
						},
					},
				}
				http.On("Get", mock.Anything, "test-model-router", mock.Anything).Return(route, nil)
			},
			wantErr: false,
		},
		{
			name:  "httproute get returns error",
			model: "get-failed",
			setupMock: func(gw *MockGatewayClient, gwv1 *MockGatewayV1Client, http *MockHTTPRouteClient) {
				gw.On("GatewayV1").Return(gwv1)
				gwv1.On("HTTPRoutes", "aibrix-system").Return(http)
				http.On("Get", mock.Anything, "get-failed-router", mock.Anything).Return((*gatewayv1.HTTPRoute)(nil), errors.New("boom"))
			},
			wantErr:     true,
			errContains: "boom",
		},
		{
			name:  "no valid status conditions",
			model: "no-conditions",
			setupMock: func(gw *MockGatewayClient, gwv1 *MockGatewayV1Client, http *MockHTTPRouteClient) {
				gw.On("GatewayV1").Return(gwv1)
				gwv1.On("HTTPRoutes", "aibrix-system").Return(http)
				route := &gatewayv1.HTTPRoute{
					Status: gatewayv1.HTTPRouteStatus{
						RouteStatus: gatewayv1.RouteStatus{
							Parents: []gatewayv1.RouteParentStatus{{
								Conditions: []metav1.Condition{},
							}},
						},
					},
				}
				http.On("Get", mock.Anything, "no-conditions-router", mock.Anything).Return(route, nil)
			},
			wantErr:     true,
			errContains: "does not have valid status",
		},
		{
			name:  "resolved refs not resolved",
			model: "refs-not-resolved",
			setupMock: func(gw *MockGatewayClient, gwv1 *MockGatewayV1Client, http *MockHTTPRouteClient) {
				gw.On("GatewayV1").Return(gwv1)
				gwv1.On("HTTPRoutes", "aibrix-system").Return(http)
				route := &gatewayv1.HTTPRoute{
					Status: gatewayv1.HTTPRouteStatus{
						RouteStatus: gatewayv1.RouteStatus{
							Parents: []gatewayv1.RouteParentStatus{{
								Conditions: []metav1.Condition{{
									Type:   string(gatewayv1.RouteConditionAccepted),
									Reason: string(gatewayv1.RouteReasonAccepted),
									Status: metav1.ConditionTrue,
								}, {
									Type:   string(gatewayv1.RouteConditionResolvedRefs),
									Reason: "InvalidRef",
									Status: metav1.ConditionFalse,
								}},
							}},
						},
					},
				}
				http.On("Get", mock.Anything, "refs-not-resolved-router", mock.Anything).Return(route, nil)
			},
			wantErr:     true,
			errContains: "object references are not resolved",
		},
		{
			name:  "invalid route status",
			model: "invalid-model",
			setupMock: func(gw *MockGatewayClient, gwv1 *MockGatewayV1Client, http *MockHTTPRouteClient) {
				gw.On("GatewayV1").Return(gwv1)
				gwv1.On("HTTPRoutes", "aibrix-system").Return(http)

				route := &gatewayv1.HTTPRoute{
					Status: gatewayv1.HTTPRouteStatus{
						RouteStatus: gatewayv1.RouteStatus{
							Parents: []gatewayv1.RouteParentStatus{{
								Conditions: []metav1.Condition{{
									Type:   string(gatewayv1.RouteConditionAccepted),
									Reason: "InvalidReason",
								}},
							}},
						},
					},
				}
				http.On("Get", mock.Anything, "invalid-model-router", mock.Anything).Return(route, nil)
			},
			wantErr:     true,
			errContains: "route is not accepted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup mocks
			mockGW := &MockGatewayClient{}
			mockGWV1 := &MockGatewayV1Client{}
			mockHTTP := &MockHTTPRouteClient{}
			tt.setupMock(mockGW, mockGWV1, mockHTTP)

			// Create test server with mock client
			s := &Server{
				gatewayClient: mockGW,
			}

			// Run test
			err := s.validateHTTPRouteStatus(context.Background(), tt.model)

			// Verify results
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
			} else {
				assert.NoError(t, err)
			}

			// Verify mock expectations
			mockGW.AssertExpectations(t)
			mockGWV1.AssertExpectations(t)
			mockHTTP.AssertExpectations(t)
		})
	}
}

func TestValidateHTTPRouteStatus_StandaloneModeSkipsValidation(t *testing.T) {
	s := &Server{gatewayClient: nil}
	assert.NoError(t, s.validateHTTPRouteStatus(context.Background(), "any-model"))
}

func Test_responseErrorProcessing_ErrorCodeAndMessage(t *testing.T) {
	baseResp := &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{Header: &configPb.HeaderValue{Key: "x-test", RawValue: []byte("1")}},
						},
					},
				},
			},
		},
	}

	t.Run("401 maps to invalid_api_key and appends httproute error", func(t *testing.T) {
		mockGW := &MockGatewayClient{}
		mockGWV1 := &MockGatewayV1Client{}
		mockHTTP := &MockHTTPRouteClient{}
		mockGW.On("GatewayV1").Return(mockGWV1)
		mockGWV1.On("HTTPRoutes", "aibrix-system").Return(mockHTTP)
		mockHTTP.On("Get", mock.Anything, "m-router", mock.Anything).Return((*gatewayv1.HTTPRoute)(nil), errors.New("httproute boom"))

		s := &Server{gatewayClient: mockGW}
		out := s.responseErrorProcessing(context.Background(), baseResp, 401, "m", "rid", "Incorrect API key provided")
		ir := out.GetImmediateResponse()
		if assert.NotNil(t, ir) {
			assert.Equal(t, envoyTypePb.StatusCode(401), ir.GetStatus().GetCode())
			var parsed map[string]any
			assert.NoError(t, json.Unmarshal([]byte(ir.GetBody()), &parsed))
			errObj := parsed["error"].(map[string]any)
			assert.Equal(t, ErrorTypeAuthentication, errObj["type"])
			assert.Equal(t, ErrorCodeInvalidAPIKey, errObj["code"])
			assert.Contains(t, errObj["message"].(string), "Incorrect API key provided")
			assert.Contains(t, errObj["message"].(string), "httproute boom")
			assert.Len(t, ir.GetHeaders().GetSetHeaders(), 2)
		}

		mockGW.AssertExpectations(t)
		mockGWV1.AssertExpectations(t)
		mockHTTP.AssertExpectations(t)
	})

	t.Run("503 maps to service_unavailable", func(t *testing.T) {
		s := &Server{gatewayClient: nil}
		out := s.responseErrorProcessing(context.Background(), baseResp, 503, "m", "rid", "server shutdown")
		ir := out.GetImmediateResponse()
		if assert.NotNil(t, ir) {
			assert.Equal(t, envoyTypePb.StatusCode(503), ir.GetStatus().GetCode())
			var parsed map[string]any
			assert.NoError(t, json.Unmarshal([]byte(ir.GetBody()), &parsed))
			errObj := parsed["error"].(map[string]any)
			assert.Equal(t, ErrorTypeOverloaded, errObj["type"])
			assert.Equal(t, ErrorCodeServiceUnavailable, errObj["code"])
		}
	})

	t.Run("500 keeps code null", func(t *testing.T) {
		s := &Server{gatewayClient: nil}
		out := s.responseErrorProcessing(context.Background(), baseResp, 500, "m", "rid", "internal error")
		ir := out.GetImmediateResponse()
		if assert.NotNil(t, ir) {
			assert.Equal(t, envoyTypePb.StatusCode(500), ir.GetStatus().GetCode())
			var parsed map[string]any
			assert.NoError(t, json.Unmarshal([]byte(ir.GetBody()), &parsed))
			errObj := parsed["error"].(map[string]any)
			_, hasCode := errObj["code"]
			assert.True(t, hasCode)
			assert.Nil(t, errObj["code"])
		}
	})
}

func Test_getMetricErr(t *testing.T) {
	t.Run("uses Header.Value when present", func(t *testing.T) {
		ir := &extProcPb.ImmediateResponse{
			Headers: &extProcPb.HeaderMutation{
				SetHeaders: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: metricHeaderErr, Value: "bad"}},
				},
			},
		}
		assert.Equal(t, "gateway_req_headers_bad", getMetricErr(ir, "gateway_req_headers"))
	})

	t.Run("uses Header.RawValue when Value empty", func(t *testing.T) {
		ir := &extProcPb.ImmediateResponse{
			Headers: &extProcPb.HeaderMutation{
				SetHeaders: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: metricHeaderErr, RawValue: []byte("oops")}},
				},
			},
		}
		assert.Equal(t, "gateway_rsp_headers_oops", getMetricErr(ir, "gateway_rsp_headers"))
	})

	t.Run("returns label underscore when header missing", func(t *testing.T) {
		ir := &extProcPb.ImmediateResponse{
			Headers: &extProcPb.HeaderMutation{
				SetHeaders: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: "x-other", RawValue: []byte("1")}},
				},
			},
		}
		assert.Equal(t, "gateway_rsp_body", getMetricErr(ir, "gateway_rsp_body"))
	})
}
