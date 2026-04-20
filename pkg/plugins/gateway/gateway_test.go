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
	"io"
	"testing"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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

func TestHandleProcessingRequest_RequestHeaders_SetsRoutingContext(t *testing.T) {
	s := &Server{}
	st := &processState{
		ctx:       context.Background(),
		requestID: "test-req-id",
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{},
				},
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "gateway_req_headers", st.metricLabel)
	if assert.NotNil(t, st.routerCtx) {
		assert.Equal(t, st.routerCtx, st.ctx)
	}
	assert.Equal(t, "", st.model)
}

func TestHandleProcessingRequest_NoResponseGenerated_ReturnsInternalErrorAndCleansUp(t *testing.T) {
	mc := &MockCache{}
	// routerCtx is nil in this scenario; traceTerm defaults to 0.
	mc.On("DoneRequestCount", (*types.RoutingContext)(nil), "rid", "m", int64(0)).Return()

	s := &Server{
		cache: mc,
	}
	st := &processState{
		ctx:       context.Background(),
		requestID: "rid",
		model:     "m",
	}

	// ProcessingRequest with no concrete oneof set => default branch, no resp produced.
	req := &extProcPb.ProcessingRequest{}

	resp, err := s.handleProcessingRequest(st, req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	stErr, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Internal, stErr.Code())
	assert.Contains(t, stErr.Message(), "no response generated")

	mc.AssertExpectations(t)
}

func TestHandleProcessingRequest_ResponseBody_ErrorFromPreviousStage_UsesErrorProcessor(t *testing.T) {
	s := &Server{} // standalone mode; validateHTTPRouteStatus is skipped in error processor

	st := &processState{
		ctx:           context.Background(),
		requestID:     "rid",
		model:         "m",
		isRespError:   true,
		respErrorCode: 401,
		lastRespHeaders: []*configPb.HeaderValueOption{
			{Header: &configPb.HeaderValue{Key: "x-test", RawValue: []byte("1")}},
		},
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"error":"boom"}`),
				EndOfStream: true,
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	if assert.NotNil(t, resp) {
		imm := resp.GetImmediateResponse()
		assert.NotNil(t, imm, "expected ImmediateResponse for error path")
		assert.Equal(t, envoyTypePb.StatusCode(401), imm.GetStatus().GetCode())
	}
	// metricLabel should be set for response body processing
	assert.Equal(t, gatewayRespBody, st.metricLabel)
}

func TestHandleProcessingRequest_ResponseBody_SuccessMarksCompletionAndEmitsSuccessMetric(t *testing.T) {
	// Use real in-memory cache for underlying HandleResponseBody logic.
	// Register a pod for "test-model" so DoneRequestTrace finds a non-nil OutputPredictor.
	testPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status: v1.PodStatus{
			PodIP:      "1.2.3.4",
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
	testCache := cache.NewWithPodsForTest([]*v1.Pod{testPod}, "test-model")
	s := &Server{
		cache: testCache,
	}

	requestID := "test-req-id"
	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", requestID, "")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()

	st := &processState{
		ctx:       routerCtx,
		requestID: requestID,
		model:     "test-model",
		stream:    false,
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model": "test-model", "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}`),
				EndOfStream: true,
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.NotNil(t, resp.GetResponseBody())
	assert.True(t, st.completed, "expected processState.completed to be true")
	assert.True(t, st.isGatewayRspDone, "expected gateway response to be marked done exactly once")
	assert.Equal(t, gatewayRespBody, st.metricLabel)
}

// TestHandleProcessingRequest_RequestBody_ModelNotFound covers the RequestBody switch case
// when the model extracted from the body does not exist in the cache. The handler returns an
// immediate 400 response and sets metricLabel to gatewayReqBody.
func TestHandleProcessingRequest_RequestBody_ModelNotFound(t *testing.T) {
	mc := &MockCache{}
	mc.On("HasModel", "no-such-model").Return(false)

	s := &Server{cache: mc}

	routerCtx := types.NewRoutingContext(context.Background(), "random", "", "", "req-rb-1", "")
	routerCtx.ReqPath = PathChatCompletions

	st := &processState{
		ctx:       routerCtx,
		requestID: "req-rb-1",
		model:     "",
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body: []byte(`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`),
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	if assert.NotNil(t, resp) {
		assert.NotNil(t, resp.GetImmediateResponse(), "expected 400 ImmediateResponse for missing model")
		assert.Equal(t, envoyTypePb.StatusCode(400), resp.GetImmediateResponse().GetStatus().GetCode())
	}
	assert.Equal(t, gatewayReqBody, st.metricLabel)
	assert.Equal(t, "no-such-model", st.model)

	mc.AssertExpectations(t)
}

// TestHandleProcessingRequest_ResponseHeaders_200 covers the ResponseHeaders switch case
// when the upstream returns 200 OK. No error is raised and isRespError stays false.
func TestHandleProcessingRequest_ResponseHeaders_200(t *testing.T) {
	s := &Server{} // no cache needed — DoneRequestCount is only called on non-200

	st := &processState{
		ctx:       context.Background(),
		requestID: "req-rh-200",
		model:     "test-model",
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{
						{Key: ":status", RawValue: []byte("200")},
					},
				},
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Nil(t, resp.GetImmediateResponse(), "expected no ImmediateResponse for 200 OK")
	assert.False(t, st.isRespError)
	assert.Equal(t, gatewayRespHeaders, st.metricLabel)
}

// TestHandleProcessingRequest_ResponseHeaders_NonOK covers the ResponseHeaders switch case
// when the upstream returns a non-200 status. HandleResponseHeaders calls DoneRequestCount
// and handleProcessingRequest transforms the response into an ImmediateResponse.
func TestHandleProcessingRequest_ResponseHeaders_NonOK(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", (*types.RoutingContext)(nil), "req-rh-401", "m", int64(0)).Return()

	s := &Server{
		cache:         mc,
		gatewayClient: nil, // standalone — validateHTTPRouteStatus skipped
	}

	st := &processState{
		ctx:       context.Background(),
		requestID: "req-rh-401",
		model:     "m",
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{
						{Key: ":status", RawValue: []byte("401")},
					},
				},
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	if assert.NotNil(t, resp) {
		imm := resp.GetImmediateResponse()
		assert.NotNil(t, imm, "expected ImmediateResponse for non-200 response header")
		assert.Equal(t, envoyTypePb.StatusCode(401), imm.GetStatus().GetCode())
	}
	assert.True(t, st.isRespError)
	assert.Equal(t, 401, st.respErrorCode)
	assert.Equal(t, gatewayRespHeaders, st.metricLabel)

	mc.AssertExpectations(t)
}

// TestHandleProcessingRequest_ResponseBody_NotYetCompleted covers the ResponseBody switch
// case when the stream is still in progress (EndOfStream=false). The response is returned
// but completed stays false and the success metric is not emitted yet.
func TestHandleProcessingRequest_ResponseBody_NotYetCompleted(t *testing.T) {
	// No cache interaction expected: DoneRequestTrace is only called when complete transitions to true.
	s := &Server{}

	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", "req-rb-partial", "")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()

	st := &processState{
		ctx:       routerCtx,
		requestID: "req-rb-partial",
		model:     "test-model",
		stream:    false,
		completed: false,
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"id":"chunk-1"}`),
				EndOfStream: false,
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.NotNil(t, resp.GetResponseBody(), "expected a body response for partial data")
	assert.False(t, st.completed, "stream should not be marked complete before EndOfStream")
	assert.False(t, st.isGatewayRspDone)
	assert.Equal(t, gatewayRespBody, st.metricLabel)
}

// mockProcessServer implements extProcPb.ExternalProcessor_ProcessServer for testing.
type mockProcessServer struct {
	mock.Mock
	ctx context.Context
}

func (m *mockProcessServer) Send(resp *extProcPb.ProcessingResponse) error {
	args := m.Called(resp)
	return args.Error(0)
}

func (m *mockProcessServer) Recv() (*extProcPb.ProcessingRequest, error) {
	args := m.Called()
	req, _ := args.Get(0).(*extProcPb.ProcessingRequest)
	return req, args.Error(1)
}

func (m *mockProcessServer) Context() context.Context     { return m.ctx }
func (m *mockProcessServer) SetHeader(metadata.MD) error  { return nil }
func (m *mockProcessServer) SendHeader(metadata.MD) error { return nil }
func (m *mockProcessServer) SetTrailer(metadata.MD)       {}
func (m *mockProcessServer) SendMsg(interface{}) error    { return nil }
func (m *mockProcessServer) RecvMsg(interface{}) error    { return nil }

// newProcessTestServer creates a minimal Server for Process tests.
func newProcessTestServer(shutdownCh <-chan struct{}, c *MockCache) *Server {
	return &Server{
		shutdownCh:          shutdownCh,
		cache:               c,
		requestCountTracker: map[string]int{},
	}
}

func closedShutdownCh() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func openShutdownCh() <-chan struct{} {
	return make(chan struct{})
}

// TestProcess_ServerShutdown verifies that Process exits immediately when the
// shutdown channel is closed before any message is received.
func TestProcess_ServerShutdown(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()

	srv := &mockProcessServer{ctx: context.Background()}
	s := newProcessTestServer(closedShutdownCh(), mc)

	err := s.Process(srv)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.Contains(t, st.Message(), "server shutdown in progress")

	mc.AssertExpectations(t)
	srv.AssertExpectations(t)
}

// TestProcess_ContextCancelled verifies that Process exits when the client
// context is cancelled before any message is received.
func TestProcess_ContextCancelled(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	srv := &mockProcessServer{ctx: ctx}
	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.ErrorIs(t, err, context.Canceled)

	mc.AssertExpectations(t)
	srv.AssertExpectations(t)
}

// TestProcess_RecvEOF_NotCompleted verifies that an EOF received before the
// request completes is surfaced as io.EOF (client closed stream prematurely).
func TestProcess_RecvEOF_NotCompleted(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()

	srv := &mockProcessServer{ctx: context.Background()}
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), io.EOF).Once()

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.Equal(t, io.EOF, err)

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_RecvGRPCCanceled verifies that a gRPC Canceled error from Recv
// is treated as a normal stream closure and returned as codes.Canceled.
func TestProcess_RecvGRPCCanceled(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()

	srv := &mockProcessServer{ctx: context.Background()}
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), status.Error(codes.Canceled, "client disconnected")).Once()

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Canceled, st.Code())

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_RecvGRPCError verifies that an unexpected gRPC error from Recv
// is propagated back to the caller.
func TestProcess_RecvGRPCError(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()

	srv := &mockProcessServer{ctx: context.Background()}
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), status.Error(codes.Internal, "something broke")).Once()

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Internal, st.Code())

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_RecvNonGRPCError verifies that a non-gRPC error from Recv is
// wrapped and returned as a codes.Unknown gRPC error.
func TestProcess_RecvNonGRPCError(t *testing.T) {
	mc := &MockCache{}
	// DoneRequestCount is NOT called for non-gRPC recv errors.

	srv := &mockProcessServer{ctx: context.Background()}
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), errors.New("transport error")).Once()

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Unknown, st.Code())
	assert.Contains(t, st.Message(), "recv stream error")

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_RecvEOF_DuringShutdown verifies that an EOF received while a
// server shutdown is in progress results in a codes.Unavailable error.
func TestProcess_RecvEOF_DuringShutdown(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()

	shutdownCh := make(chan struct{})

	srv := &mockProcessServer{ctx: context.Background()}
	// Close the shutdown channel when Recv is called so that handleRecvError
	// picks it up (preRecvCheck already passed via the default branch).
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), io.EOF).Once().
		Run(func(args mock.Arguments) { close(shutdownCh) })

	s := newProcessTestServer(shutdownCh, mc)

	err := s.Process(srv)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.Contains(t, st.Message(), "server shutdown in progress")

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}
