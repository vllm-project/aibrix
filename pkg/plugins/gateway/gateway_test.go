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

func Test_selectTargetPod(t *testing.T) {
	// Initialize routing algorithms
	routing.Init()

	tests := []struct {
		name          string
		pods          types.PodList
		mockSetup     func(*MockRouter, types.RoutingAlgorithm)
		expectedError bool
		expectedPodIP string
	}{
		{
			name: "routing.Select returns error",
			pods: &MockPodList{pods: []*v1.Pod{{
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
			mockSetup: func(mockRouter *MockRouter, algo types.RoutingAlgorithm) {
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("", errors.New("test error"))

			},
			expectedError: true,
		},
		{
			name: "no pods available",
			pods: &MockPodList{pods: []*v1.Pod{}},
			mockSetup: func(m *MockRouter, algo types.RoutingAlgorithm) {
				routing.Register(algo, func() (types.Router, error) {
					return m, nil
				})
				// No expectations needed as pods.Len() == 0
			},
			expectedError: true,
		},
		{
			name: "no ready pods available",
			pods: &MockPodList{pods: []*v1.Pod{{
				Status: v1.PodStatus{
					PodIP:      "1.2.3.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}},
				},
			}}},
			mockSetup: func(mockRouter *MockRouter, algo types.RoutingAlgorithm) {
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				// No expectations needed as no ready pods
			},
			expectedError: true,
		},
		{
			name: "single ready pod",
			pods: &MockPodList{pods: []*v1.Pod{{
				Status: v1.PodStatus{
					PodIP:      "1.2.3.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			}}},
			mockSetup: func(mockRouter *MockRouter, algo types.RoutingAlgorithm) {
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
			pods: &MockPodList{pods: []*v1.Pod{
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
			mockSetup: func(mockRouter *MockRouter, algo types.RoutingAlgorithm) {
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
		t.Run(tt.name, func(t *testing.T) {
			mockRouter := new(MockRouter)
			routingAlgo := types.RoutingAlgorithm(fmt.Sprintf("test-router-%s", tt.name))

			tt.mockSetup(mockRouter, routingAlgo)
			routing.Init()

			server := &Server{}
			ctx := types.NewRoutingContext(context.Background(), routingAlgo, "test-model", "test-message", "test-request", "test-user")

			podIP, err := server.selectTargetPod(ctx, tt.pods)

			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedPodIP, podIP)
			}

			mockRouter.AssertExpectations(t)
		})
	}
}
