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
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	routingalgorithms "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// newBenchmarkServer creates a server and request for benchmark tests.
// mockSetup is called once; mock expectations must use maybe/AnyTimes so they survive b.N iterations.
func newBenchmarkServer(b *testing.B, requestBody string, routingAlgo types.RoutingAlgorithm, mockSetup func(*MockCache, *mockRouter)) (*Server, *extProcPb.ProcessingRequest, *types.RoutingContext, utils.User) {
	b.Helper()

	mockCache := &MockCache{Cache: cache.NewForTest()}
	mr := new(mockRouter)
	if mockSetup != nil {
		mockSetup(mockCache, mr)
	}

	mockGW := &MockGatewayClient{}
	mockGWv1 := &MockGatewayV1Client{}
	mockHTTP := &MockHTTPRouteClient{}
	mockGW.On("GatewayV1").Return(mockGWv1)
	mockGWv1.On("HTTPRoutes", "aibrix-system").Return(mockHTTP)

	route := &gatewayv1.HTTPRoute{
		Status: gatewayv1.HTTPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{
				Parents: []gatewayv1.RouteParentStatus{{
					Conditions: []metav1.Condition{
						{Type: string(gatewayv1.RouteConditionAccepted), Reason: string(gatewayv1.RouteReasonAccepted), Status: metav1.ConditionTrue},
						{Type: string(gatewayv1.RouteConditionResolvedRefs), Reason: string(gatewayv1.RouteReasonResolvedRefs), Status: metav1.ConditionTrue},
					},
				}},
			},
		},
	}
	mockHTTP.On("Get", mock.Anything, "test-model-router", mock.Anything).Return(route, nil)

	server := &Server{cache: mockCache, gatewayClient: mockGW}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{Body: []byte(requestBody)},
		},
	}

	user := utils.User{Name: "bench-user"}
	routingCtx := types.NewRoutingContext(context.Background(), routingAlgo, "test-model", "", "bench-request-id", user.Name)
	routingCtx.ReqPath = PathChatCompletions
	if routingAlgo != "" {
		if routingCtx.ReqHeaders == nil {
			routingCtx.ReqHeaders = make(map[string]string)
		}
		routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(routingAlgo)
	}

	return server, req, routingCtx, user
}

// BenchmarkHandleRequestBody_NoRoutingStrategy benchmarks the happy path where
// no routing algorithm is set and the request goes through HTTPRoute validation.
func BenchmarkHandleRequestBody_NoRoutingStrategy(b *testing.B) {
	routingalgorithms.Init()

	podList := &utils.PodArray{
		Pods: []*v1.Pod{
			{Status: v1.PodStatus{PodIP: "1.2.3.4", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			{Status: v1.PodStatus{PodIP: "4.5.6.7", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
		},
	}

	server, req, baseCtx, user := newBenchmarkServer(b,
		`{"model": "test-model", "messages": [{"role": "user", "content": "hello"}]}`,
		"",
		func(mc *MockCache, _ *mockRouter) {
			mc.On("HasModel", "test-model").Return(true)
			mc.On("ListPodsByModel", "test-model").Return(podList, nil)
			mc.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1))
		},
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Rebuild routing context each iteration (cheap) to avoid shared state.
		routingCtx := types.NewRoutingContext(context.Background(), baseCtx.Algorithm, "test-model", "", "bench-request-id", user.Name)
		routingCtx.ReqPath = PathChatCompletions

		server.HandleRequestBody(routingCtx, "bench-request-id", req, user)
	}
}

// BenchmarkHandleRequestBody_WithRoutingStrategy benchmarks the path where a
// routing algorithm selects a target pod.
func BenchmarkHandleRequestBody_WithRoutingStrategy(b *testing.B) {
	const benchRouter types.RoutingAlgorithm = "bench-router"

	mr := new(mockRouter)
	mr.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil)

	routingalgorithms.Register(benchRouter, func() (types.Router, error) { return mr, nil })
	routingalgorithms.Init()

	podList := &utils.PodArray{
		Pods: []*v1.Pod{
			{Status: v1.PodStatus{PodIP: "1.2.3.4", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			{Status: v1.PodStatus{PodIP: "4.5.6.7", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
		},
	}

	server, req, _, user := newBenchmarkServer(b,
		`{"model": "test-model", "messages": [{"role": "user", "content": "hello"}]}`,
		benchRouter,
		func(mc *MockCache, _ *mockRouter) {
			mc.On("HasModel", "test-model").Return(true)
			mc.On("ListPodsByModel", "test-model").Return(podList, nil)
			mc.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1))
		},
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		routingCtx := types.NewRoutingContext(context.Background(), benchRouter, "test-model", "", "bench-request-id", user.Name)
		routingCtx.ReqPath = PathChatCompletions
		if routingCtx.ReqHeaders == nil {
			routingCtx.ReqHeaders = make(map[string]string)
		}
		routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(benchRouter)

		server.HandleRequestBody(routingCtx, "bench-request-id", req, user)
	}
}

// BenchmarkHandleRequestBody_ModelNotFound benchmarks the early-exit error path
// when the requested model does not exist in the cache.
func BenchmarkHandleRequestBody_ModelNotFound(b *testing.B) {
	routingalgorithms.Init()

	server, req, _, user := newBenchmarkServer(b,
		`{"model": "unknown-model", "messages": [{"role": "user", "content": "hello"}]}`,
		"",
		func(mc *MockCache, _ *mockRouter) {
			mc.On("HasModel", "unknown-model").Return(false)
		},
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		routingCtx := types.NewRoutingContext(context.Background(), "", "unknown-model", "", "bench-request-id", user.Name)
		routingCtx.ReqPath = PathChatCompletions

		server.HandleRequestBody(routingCtx, "bench-request-id", req, user)
	}
}

// BenchmarkHandleRequestBody_NoPodsAvailable benchmarks the error path when all
// pods for the model are not ready.
func BenchmarkHandleRequestBody_NoPodsAvailable(b *testing.B) {
	routingalgorithms.Init()

	podList := &utils.PodArray{
		Pods: []*v1.Pod{
			{Status: v1.PodStatus{PodIP: "1.2.3.4", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}}}},
		},
	}

	server, req, _, user := newBenchmarkServer(b,
		`{"model": "test-model", "messages": [{"role": "user", "content": "hello"}]}`,
		"",
		func(mc *MockCache, _ *mockRouter) {
			mc.On("HasModel", "test-model").Return(true)
			mc.On("ListPodsByModel", "test-model").Return(podList, nil)
		},
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		routingCtx := types.NewRoutingContext(context.Background(), "", "test-model", "", "bench-request-id", user.Name)
		routingCtx.ReqPath = PathChatCompletions

		server.HandleRequestBody(routingCtx, "bench-request-id", req, user)
	}
}

// benchStubCache is a zero-overhead cache stub for benchmarks.
// Using testify/mock causes ~80% of allocs via reflect.packEface in Arguments.Diff,
// masking the real handler cost. This plain struct eliminates that noise entirely.
type benchStubCache struct {
	cache.Cache // embed for interface completeness; only overridden methods are called
	pods        types.PodList
}

func (c *benchStubCache) HasModel(_ string) bool                                     { return true }
func (c *benchStubCache) ListPodsByModel(_ string) (types.PodList, error)            { return c.pods, nil }
func (c *benchStubCache) AddRequestCount(_ *types.RoutingContext, _, _ string) int64 { return 1 }

// benchStubRouter is a zero-overhead router stub for benchmarks.
type benchStubRouter struct{}

func (r *benchStubRouter) Route(_ *types.RoutingContext, _ types.PodList) (string, error) {
	return "1.2.3.4:8000", nil
}
func (r *benchStubRouter) Name() string { return "bench-stub" }

// BenchmarkHandleRequestBody_WithRoutingStrategy_32k benchmarks the routing-
// algorithm path with a large message body: 8 000 randomly generated 4-char
// words joined by spaces (~40 k characters of content).
//
// Uses plain stub implementations instead of testify/mock to eliminate the ~80%
// allocation overhead that mock.Called() introduces via reflect.packEface.
func BenchmarkHandleRequestBody_WithRoutingStrategy_ChatCompletions_32k(b *testing.B) {
	const benchRouter types.RoutingAlgorithm = "bench-router-32k"

	routingalgorithms.Register(benchRouter, func() (types.Router, error) { return &benchStubRouter{}, nil })
	routingalgorithms.Init()

	// Build an 8 000-word message where every word is exactly 4 lowercase letters.
	const (
		wordCount = 8_000
		wordLen   = 4
		charset   = "abcdefghijklmnopqrstuvwxyz"
	)
	rng := rand.New(rand.NewSource(42))
	words := make([]string, wordCount)
	buf := make([]byte, wordLen)
	for i := range words {
		for j := range buf {
			buf[j] = charset[rng.Intn(len(charset))]
		}
		words[i] = string(buf)
	}
	content := strings.Join(words, " ")

	requestBody := fmt.Sprintf(
		`{"model":"test-model","messages":[{"role":"user","content":%s}]}`,
		strconv.Quote(content),
	)

	podList := &utils.PodArray{
		Pods: []*v1.Pod{
			{Status: v1.PodStatus{PodIP: "1.2.3.4", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			{Status: v1.PodStatus{PodIP: "4.5.6.7", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
		},
	}

	stubCache := &benchStubCache{Cache: cache.NewForTest(), pods: podList}
	server := &Server{cache: stubCache}
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{Body: []byte(requestBody)},
		},
	}
	user := utils.User{Name: "bench-user"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		routingCtx := types.NewRoutingContext(context.Background(), benchRouter, "test-model", "", "bench-request-id", user.Name)
		routingCtx.ReqPath = PathChatCompletions
		if routingCtx.ReqHeaders == nil {
			routingCtx.ReqHeaders = make(map[string]string)
		}
		routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(benchRouter)
		server.HandleRequestBody(routingCtx, "bench-request-id", req, user)
	}
}

// BenchmarkHandleRequestBody_WithRoutingStrategy_Completions_32k benchmarks
// the routing-algorithm path for /v1/completions with a large prompt body:
// 8 000 randomly generated 4-char words joined by spaces (~40 k characters
// of content).
func BenchmarkHandleRequestBody_WithRoutingStrategy_Completions_32k(b *testing.B) {
	const benchRouter types.RoutingAlgorithm = "bench-router-completions-32k"

	routingalgorithms.Register(benchRouter, func() (types.Router, error) { return &benchStubRouter{}, nil })
	routingalgorithms.Init()

	// Build an 8 000-word prompt where every word is exactly 4 lowercase letters.
	const (
		wordCount = 8_000
		wordLen   = 4
		charset   = "abcdefghijklmnopqrstuvwxyz"
	)
	rng := rand.New(rand.NewSource(42))
	words := make([]string, wordCount)
	buf := make([]byte, wordLen)
	for i := range words {
		for j := range buf {
			buf[j] = charset[rng.Intn(len(charset))]
		}
		words[i] = string(buf)
	}
	content := strings.Join(words, " ")

	requestBody := fmt.Sprintf(
		`{"model":"test-model","prompt":%s}`,
		strconv.Quote(content),
	)

	podList := &utils.PodArray{
		Pods: []*v1.Pod{
			{Status: v1.PodStatus{PodIP: "1.2.3.4", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			{Status: v1.PodStatus{PodIP: "4.5.6.7", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
		},
	}

	stubCache := &benchStubCache{Cache: cache.NewForTest(), pods: podList}
	server := &Server{cache: stubCache}
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{Body: []byte(requestBody)},
		},
	}
	user := utils.User{Name: "bench-user"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		routingCtx := types.NewRoutingContext(context.Background(), benchRouter, "test-model", "", "bench-request-id", user.Name)
		routingCtx.ReqPath = PathCompletions
		if routingCtx.ReqHeaders == nil {
			routingCtx.ReqHeaders = make(map[string]string)
		}
		routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(benchRouter)
		server.HandleRequestBody(routingCtx, "bench-request-id", req, user)
	}
}

// BenchmarkHandleRequestBody_StreamRequest benchmarks the happy path with a
// streaming request body.
func BenchmarkHandleRequestBody_StreamRequest(b *testing.B) {
	routingalgorithms.Init()

	podList := &utils.PodArray{
		Pods: []*v1.Pod{
			{Status: v1.PodStatus{PodIP: "1.2.3.4", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
		},
	}

	server, req, _, user := newBenchmarkServer(b,
		`{"model": "test-model", "messages": [{"role": "user", "content": "hello"}], "stream": true}`,
		"",
		func(mc *MockCache, _ *mockRouter) {
			mc.On("HasModel", "test-model").Return(true)
			mc.On("ListPodsByModel", "test-model").Return(podList, nil)
			mc.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1))
		},
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		routingCtx := types.NewRoutingContext(context.Background(), "", "test-model", "", "bench-request-id", user.Name)
		routingCtx.ReqPath = PathChatCompletions

		server.HandleRequestBody(routingCtx, "bench-request-id", req, user)
	}
}
