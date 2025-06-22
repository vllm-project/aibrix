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
	"errors"
	"fmt"
	"os"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
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
	cache.InitForTest()
	routing.Init()
	for _, tt := range tests {
		_, currentValidation := routing.Validate(tt.routingStrategy)
		assert.Equal(t, tt.expectedValidation, currentValidation, tt.message)
	}
}

func TestGetRoutingStrategy(t *testing.T) {
	var tests = []struct {
		headers               []*configPb.HeaderValue
		setEnvRoutingStrategy bool
		envRoutingStrategy    string
		expectedStrategy      string
		expectedEnabled       bool
		message               string
	}{
		{
			headers:               []*configPb.HeaderValue{},
			setEnvRoutingStrategy: false,
			expectedStrategy:      "",
			expectedEnabled:       false,
			message:               "no routing strategy in headers or environment variable",
		},
		{
			headers: []*configPb.HeaderValue{
				{Key: "routing-strategy", RawValue: []byte("random")},
			},
			setEnvRoutingStrategy: false,
			expectedStrategy:      "random",
			expectedEnabled:       true,
			message:               "routing strategy from headers",
		},
		{
			headers:               []*configPb.HeaderValue{},
			setEnvRoutingStrategy: true,
			envRoutingStrategy:    "random",
			expectedStrategy:      "random",
			expectedEnabled:       true,
			message:               "routing strategy from environment variable",
		},
		{
			headers: []*configPb.HeaderValue{
				{Key: "routing-strategy", RawValue: []byte("random")},
			},
			setEnvRoutingStrategy: true,
			envRoutingStrategy:    "least-request",
			expectedStrategy:      "random",
			expectedEnabled:       true,
			message:               "header routing strategy takes priority over environment variable",
		},
	}

	for _, tt := range tests {
		if tt.setEnvRoutingStrategy {
			_ = os.Setenv("ROUTING_ALGORITHM", tt.envRoutingStrategy)
		} else {
			_ = os.Unsetenv("ROUTING_ALGORITHM")
		}

		// refresh default values, the process won't modify this environment variable during normal running
		defaultRoutingStrategy, defaultRoutingStrategyEnabled = utils.LookupEnv(EnvRoutingAlgorithm)

		routingStrategy, enabled := getRoutingStrategy(tt.headers)
		assert.Equal(t, tt.expectedStrategy, routingStrategy, tt.message)
		assert.Equal(t, tt.expectedEnabled, enabled, tt.message)

		// Cleanup environment variable for next test
		_ = os.Unsetenv("ROUTING_ALGORITHM")
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
		name          string
		pods          types.PodList
		mockSetup     func(*mockRouter, types.RoutingAlgorithm)
		expectedError bool
		expectedPodIP string
	}{
		{
			name: "routing.Select returns error",
			pods: &mockPodList{pods: []*v1.Pod{{
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
			pods: &mockPodList{pods: []*v1.Pod{}},
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
			pods: &mockPodList{pods: []*v1.Pod{{
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
			pods: &mockPodList{pods: []*v1.Pod{{
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
			name: "multiple ready pods",
			pods: &mockPodList{pods: []*v1.Pod{
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
			podIP, err := server.selectTargetPod(ctx, tt.pods)

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
