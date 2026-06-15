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

	"github.com/stretchr/testify/mock"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stype "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapiv1 "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/typed/apis/v1"
	gatewayapiv1alpha2 "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/typed/apis/v1alpha2"
	gatewayapiv1beta1 "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/typed/apis/v1beta1"
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
	cache.Cache
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

func (m *MockCache) DoneRequestTrace(ctx *types.RoutingContext, requestID string, model string, inputTokens int64, outputTokens int64, traceTerm int64) {
	m.Called(ctx, requestID, model, inputTokens, outputTokens, traceTerm)
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

// MockGatewayClient implements gatewayapi.Clientset interface
type MockGatewayClient struct {
	mock.Mock
}

func (m *MockGatewayClient) GatewayV1() gatewayapiv1.GatewayV1Interface {
	args := m.Called()
	return args.Get(0).(gatewayapiv1.GatewayV1Interface)
}

func (m *MockGatewayClient) GatewayV1alpha2() gatewayapiv1alpha2.GatewayV1alpha2Interface {
	args := m.Called()
	return args.Get(0).(gatewayapiv1alpha2.GatewayV1alpha2Interface)
}

func (m *MockGatewayClient) GatewayV1beta1() gatewayapiv1beta1.GatewayV1beta1Interface {
	args := m.Called()
	return args.Get(0).(gatewayapiv1beta1.GatewayV1beta1Interface)
}

func (m *MockGatewayClient) Discovery() discovery.DiscoveryInterface {
	args := m.Called()
	return args.Get(0).(discovery.DiscoveryInterface)
}

// MockGatewayV1Client implements gatewayapi.Interface
type MockGatewayV1Client struct {
	mock.Mock
}

func (m *MockGatewayV1Client) RESTClient() rest.Interface {
	args := m.Called()
	return args.Get(0).(rest.Interface)
}

func (m *MockGatewayV1Client) Gateways(namespace string) gatewayapiv1.GatewayInterface {
	args := m.Called(namespace)
	return args.Get(0).(gatewayapiv1.GatewayInterface)
}

func (m *MockGatewayV1Client) GatewayClasses() gatewayapiv1.GatewayClassInterface {
	args := m.Called()
	return args.Get(0).(gatewayapiv1.GatewayClassInterface)
}

type MockGatewayClassClient struct {
	mock.Mock
}

func (m *MockGatewayV1Client) HTTPRoutes(namespace string) gatewayapiv1.HTTPRouteInterface {
	args := m.Called(namespace)
	return args.Get(0).(gatewayapiv1.HTTPRouteInterface)
}

// MockHTTPRouteClient implements gatewayapi.HTTPRouteInterface
type MockHTTPRouteClient struct {
	mock.Mock
}

func (m *MockHTTPRouteClient) Get(ctx context.Context, name string, opts metav1.GetOptions) (*gatewayv1.HTTPRoute, error) {
	args := m.Called(ctx, name, opts)
	return args.Get(0).(*gatewayv1.HTTPRoute), args.Error(1)
}

func (m *MockHTTPRouteClient) Create(ctx context.Context, httpRoute *gatewayv1.HTTPRoute, opts metav1.CreateOptions) (*gatewayv1.HTTPRoute, error) {
	args := m.Called(ctx, httpRoute, opts)
	return args.Get(0).(*gatewayv1.HTTPRoute), args.Error(1)
}

func (m *MockHTTPRouteClient) Update(ctx context.Context, httpRoute *gatewayv1.HTTPRoute, opts metav1.UpdateOptions) (*gatewayv1.HTTPRoute, error) {
	args := m.Called(ctx, httpRoute, opts)
	return args.Get(0).(*gatewayv1.HTTPRoute), args.Error(1)
}

func (m *MockHTTPRouteClient) UpdateStatus(ctx context.Context, httpRoute *gatewayv1.HTTPRoute, opts metav1.UpdateOptions) (*gatewayv1.HTTPRoute, error) {
	args := m.Called(ctx, httpRoute, opts)
	return args.Get(0).(*gatewayv1.HTTPRoute), args.Error(1)
}

func (m *MockHTTPRouteClient) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	args := m.Called(ctx, name, opts)
	return args.Error(0)
}

func (m *MockHTTPRouteClient) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error {
	args := m.Called(ctx, opts, listOpts)
	return args.Error(0)
}

func (m *MockHTTPRouteClient) List(ctx context.Context, opts metav1.ListOptions) (*gatewayv1.HTTPRouteList, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(*gatewayv1.HTTPRouteList), args.Error(1)
}

func (m *MockHTTPRouteClient) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(watch.Interface), args.Error(1)
}

func (m *MockHTTPRouteClient) Patch(ctx context.Context, name string, pt k8stype.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *gatewayv1.HTTPRoute, err error) {
	args := m.Called(ctx, name, pt, data, opts, subresources)
	return args.Get(0).(*gatewayv1.HTTPRoute), args.Error(1)
}
