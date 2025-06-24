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
	"github.com/stretchr/testify/mock"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// mockRouter implements types.Router interface for testing
type mockRouter struct {
	mock.Mock
}

func (m *mockRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	args := m.Called(ctx, pods)
	return args.String(0), args.Error(1)
}
func (m *mockRouter) Name() string { return "mock-router" }

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
