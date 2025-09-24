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

package routingalgorithms

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mockRouter is a simple router for testing
type mockRouter struct {
	returnErr error
}

func (m *mockRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	if m.returnErr != nil {
		return "", m.returnErr
	}
	if pods.Len() == 0 {
		return "", errors.New("no pods available")
	}
	allPods := pods.All()
	ctx.SetTargetPod(allPods[0])
	return ctx.TargetAddress(), nil
}

// Mock providers for testing
var (
	mockSuccessProvider = func(*types.RoutingContext) (types.Router, error) {
		return &mockRouter{}, nil
	}

	mockErrorProvider = func(*types.RoutingContext) (types.Router, error) {
		return nil, errors.New("provider failed")
	}

	mockNilRouterProvider = func(*types.RoutingContext) (types.Router, error) {
		return nil, nil
	}

	mockRouterWithError = func(*types.RoutingContext) (types.Router, error) {
		return &mockRouter{returnErr: errors.New("router failed")}, nil
	}
)

// Helper function to create routing context
func newRoutingContext() *types.RoutingContext {
	return types.NewRoutingContext(context.Background(), "", "test-model", "test-message", "test-request", "test-user")
}

// Helper function to create test pods for fallback tests
func createFallbackTestPod(name, ip string, ready bool) *v1.Pod {
	condition := v1.ConditionFalse
	if ready {
		condition = v1.ConditionTrue
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"model.aibrix.ai/port": "8000"},
		},
		Status: v1.PodStatus{
			PodIP: ip,
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: condition,
				},
			},
		},
	}
}

func TestFallbackRouter_Route(t *testing.T) {
	tests := []struct {
		name            string
		setupRouter     func() *FallbackRouter
		pods            []*v1.Pod
		wantAlgorithm   types.RoutingAlgorithm
		wantErr         bool
		wantErrContains string
		expectPanic     bool
		verifyState     func(t *testing.T, r *FallbackRouter)
	}{
		{
			name: "nil provider uses default random router",
			setupRouter: func() *FallbackRouter {
				return &FallbackRouter{}
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			wantAlgorithm: RouterRandom,
			wantErr:       false,
			verifyState: func(t *testing.T, r *FallbackRouter) {
				// Verify defaults are set after first Route() call
				assert.Equal(t, RouterRandom, r.fallbackAlgorithm)
				assert.NotNil(t, r.fallbackProvider)
			},
		},
		{
			name: "custom fallback to least-request",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastRequest, mockSuccessProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
				createFallbackTestPod("p2", "2.2.2.2", true),
			},
			wantAlgorithm: RouterLeastRequest,
			wantErr:       false,
		},
		{
			name: "provider returns error",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastRequest, mockErrorProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			wantErr:         true,
			wantErrContains: "provider failed",
		},
		{
			name: "router returns error",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastRequest, mockRouterWithError)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			wantErr:         true,
			wantErrContains: "router failed",
		},
		{
			name: "empty pod list returns error",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastRequest, mockSuccessProvider)
				return r
			},
			pods:            []*v1.Pod{},
			wantErr:         true,
			wantErrContains: "no pods available",
		},
		{
			name: "SetFallback overwrites previous",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastRequest, mockSuccessProvider)
				r.SetFallback(RouterThroughput, mockSuccessProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			wantAlgorithm: RouterThroughput,
			wantErr:       false,
			verifyState: func(t *testing.T, r *FallbackRouter) {
				assert.Equal(t, RouterThroughput, r.fallbackAlgorithm)
			},
		},
		{
			name: "provider returns nil router without error",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastRequest, mockNilRouterProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			expectPanic:     true,
			wantErr:         true,
			wantErrContains: "invalid memory address or nil pointer",
		},
		{
			name: "default assignment persists across calls",
			setupRouter: func() *FallbackRouter {
				return &FallbackRouter{}
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			wantAlgorithm: RouterRandom,
			wantErr:       false,
			verifyState: func(t *testing.T, r *FallbackRouter) {
				// After first call, defaults should be set
				assert.Equal(t, RouterRandom, r.fallbackAlgorithm)
				assert.NotNil(t, r.fallbackProvider)

				// Second call should use already set defaults
				ctx := newRoutingContext()
				podList := &utils.PodArray{Pods: []*v1.Pod{createFallbackTestPod("p2", "2.2.2.2", true)}}
				_, err := r.Route(ctx, podList)
				assert.NoError(t, err)
				assert.Equal(t, RouterRandom, ctx.Algorithm)
			},
		},
		{
			name: "fallback algorithm set but provider is nil",
			setupRouter: func() *FallbackRouter {
				return &FallbackRouter{
					fallbackAlgorithm: RouterLeastRequest,
					fallbackProvider:  nil,
				}
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			wantAlgorithm: RouterRandom, // Should reset to default
			wantErr:       false,
			verifyState: func(t *testing.T, r *FallbackRouter) {
				assert.Equal(t, RouterRandom, r.fallbackAlgorithm)
				assert.NotNil(t, r.fallbackProvider)
			},
		},
		// Additional parameterized test cases for better coverage
		{
			name: "multiple pods with mixed readiness states",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastRequest, mockSuccessProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
				createFallbackTestPod("p2", "2.2.2.2", false),
				createFallbackTestPod("p3", "3.3.3.3", true),
				createFallbackTestPod("p4", "", true), // No IP
			},
			wantAlgorithm: RouterLeastRequest,
			wantErr:       false,
		},
		{
			name: "fallback to throughput algorithm",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterThroughput, mockSuccessProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			wantAlgorithm: RouterThroughput,
			wantErr:       false,
		},
		{
			name: "fallback to least-latency algorithm",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastLatency, mockSuccessProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
				createFallbackTestPod("p2", "2.2.2.2", true),
			},
			wantAlgorithm: RouterLeastLatency,
			wantErr:       false,
		},
		{
			name: "fallback to least-busy-time algorithm",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastBusyTime, mockSuccessProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			wantAlgorithm: RouterLeastBusyTime,
			wantErr:       false,
		},
		{
			name: "fallback to least-gpu-cache algorithm",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastGpuCache, mockSuccessProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			wantAlgorithm: RouterLeastGpuCache,
			wantErr:       false,
		},
		{
			name: "fallback to least-kv-cache algorithm",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastKvCache, mockSuccessProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", true),
			},
			wantAlgorithm: RouterLeastKvCache,
			wantErr:       false,
		},
		{
			name: "single pod with no IP address",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastRequest, mockSuccessProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "", true),
			},
			wantAlgorithm: RouterLeastRequest,
			wantErr:       false,
		},
		{
			name: "all pods not ready",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterLeastRequest, mockSuccessProvider)
				return r
			},
			pods: []*v1.Pod{
				createFallbackTestPod("p1", "1.1.1.1", false),
				createFallbackTestPod("p2", "2.2.2.2", false),
			},
			wantAlgorithm: RouterLeastRequest,
			wantErr:       false,
		},
		{
			name: "large pod list",
			setupRouter: func() *FallbackRouter {
				r := &FallbackRouter{}
				r.SetFallback(RouterRandom, mockSuccessProvider)
				return r
			},
			pods: func() []*v1.Pod {
				pods := make([]*v1.Pod, 10)
				for i := 0; i < 10; i++ {
					pods[i] = createFallbackTestPod(
						fmt.Sprintf("p%d", i),
						fmt.Sprintf("10.0.0.%d", i+1),
						i%2 == 0, // Alternate ready states
					)
				}
				return pods
			}(),
			wantAlgorithm: RouterRandom,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := tt.setupRouter()
			ctx := newRoutingContext()
			podList := &utils.PodArray{Pods: tt.pods}

			// Handle potential panic for nil router case
			if tt.expectPanic {
				defer func() {
					if r := recover(); r != nil {
						err, ok := r.(error)
						assert.True(t, ok, "panic value should be an error")
						if ok && tt.wantErrContains != "" {
							assert.Contains(t, err.Error(), tt.wantErrContains)
						}
					}
				}()
			}

			addr, err := router.Route(ctx, podList)

			if tt.wantErr {
				assert.Error(t, err)
				if tt.wantErrContains != "" && err != nil {
					assert.Contains(t, err.Error(), tt.wantErrContains)
				}
			} else {
				assert.NoError(t, err)
				assert.NotEmpty(t, addr)
				assert.Equal(t, tt.wantAlgorithm, ctx.Algorithm)
			}

			// Verify router state if needed
			if tt.verifyState != nil {
				tt.verifyState(t, router)
			}
		})
	}
}

func TestFallbackRouter_SetFallback(t *testing.T) {
	tests := []struct {
		name             string
		initialAlgorithm types.RoutingAlgorithm
		initialProvider  types.RouterProviderFunc
		newAlgorithm     types.RoutingAlgorithm
		newProvider      types.RouterProviderFunc
		verifyState      func(t *testing.T, r *FallbackRouter)
	}{
		{
			name:             "set fallback from empty state",
			initialAlgorithm: "",
			initialProvider:  nil,
			newAlgorithm:     RouterLeastRequest,
			newProvider:      mockSuccessProvider,
			verifyState: func(t *testing.T, r *FallbackRouter) {
				assert.Equal(t, RouterLeastRequest, r.fallbackAlgorithm)
				assert.NotNil(t, r.fallbackProvider)
			},
		},
		{
			name:             "overwrite existing fallback",
			initialAlgorithm: RouterLeastRequest,
			initialProvider:  mockSuccessProvider,
			newAlgorithm:     RouterThroughput,
			newProvider:      mockErrorProvider,
			verifyState: func(t *testing.T, r *FallbackRouter) {
				assert.Equal(t, RouterThroughput, r.fallbackAlgorithm)
				assert.NotNil(t, r.fallbackProvider)
				// Verify the new provider is actually used
				ctx := newRoutingContext()
				_, err := r.Route(ctx, &utils.PodArray{})
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "provider failed")
			},
		},
		{
			name:             "set to same algorithm with different provider",
			initialAlgorithm: RouterLeastRequest,
			initialProvider:  mockSuccessProvider,
			newAlgorithm:     RouterLeastRequest,
			newProvider:      mockErrorProvider,
			verifyState: func(t *testing.T, r *FallbackRouter) {
				assert.Equal(t, RouterLeastRequest, r.fallbackAlgorithm)
				// Verify new provider is used
				ctx := newRoutingContext()
				_, err := r.Route(ctx, &utils.PodArray{})
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "provider failed")
			},
		},
		// More parameterized edge cases
		{
			name:             "set nil provider",
			initialAlgorithm: RouterLeastRequest,
			initialProvider:  mockSuccessProvider,
			newAlgorithm:     RouterThroughput,
			newProvider:      nil,
			verifyState: func(t *testing.T, r *FallbackRouter) {
				assert.Equal(t, RouterThroughput, r.fallbackAlgorithm)
				assert.Nil(t, r.fallbackProvider)
				// When Route is called, it should use defaults
				ctx := newRoutingContext()
				pods := []*v1.Pod{createFallbackTestPod("p1", "1.1.1.1", true)}
				_, err := r.Route(ctx, &utils.PodArray{Pods: pods})
				assert.NoError(t, err)
				assert.Equal(t, RouterRandom, ctx.Algorithm) // Should reset to default
			},
		},
		{
			name:             "multiple SetFallback calls in sequence",
			initialAlgorithm: "",
			initialProvider:  nil,
			newAlgorithm:     RouterLeastRequest,
			newProvider:      mockSuccessProvider,
			verifyState: func(t *testing.T, r *FallbackRouter) {
				// First set
				assert.Equal(t, RouterLeastRequest, r.fallbackAlgorithm)

				// Second set
				r.SetFallback(RouterThroughput, mockErrorProvider)
				assert.Equal(t, RouterThroughput, r.fallbackAlgorithm)

				// Third set
				r.SetFallback(RouterRandom, mockSuccessProvider)
				assert.Equal(t, RouterRandom, r.fallbackAlgorithm)

				// Verify final provider works
				ctx := newRoutingContext()
				pods := []*v1.Pod{createFallbackTestPod("p1", "1.1.1.1", true)}
				_, err := r.Route(ctx, &utils.PodArray{Pods: pods})
				assert.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &FallbackRouter{
				fallbackAlgorithm: tt.initialAlgorithm,
				fallbackProvider:  tt.initialProvider,
			}

			r.SetFallback(tt.newAlgorithm, tt.newProvider)

			if tt.verifyState != nil {
				tt.verifyState(t, r)
			}
		})
	}
}
