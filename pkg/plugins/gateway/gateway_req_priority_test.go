/*
Copyright 2026 The Aibrix Team.

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
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/ratelimiter"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// TestPriorityForTier covers the tier table lookup: only the tiers the gateway
// maps resolve, and the match ignores case. The RequestHeaders phase trims the
// header value, so the lookup itself only normalizes case.
func TestPriorityForTier(t *testing.T) {
	tests := []struct {
		name     string
		tier     string
		want     int
		wantMaps bool
	}{
		{name: "batch", tier: "batch", want: 100, wantMaps: true},
		{name: "background", tier: "background", want: 1000, wantMaps: true},
		{name: "upper case", tier: "BATCH", want: 100, wantMaps: true},
		{name: "mixed case", tier: "Background", want: 1000, wantMaps: true},
		{name: "unknown tier", tier: "realtime", wantMaps: false},
		{name: "empty tier", tier: "", wantMaps: false},
		{name: "whitespace only", tier: "   ", wantMaps: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priority, ok := priorityForTier(tt.tier)
			assert.Equal(t, tt.wantMaps, ok)
			if tt.wantMaps {
				assert.Equal(t, tt.want, priority)
			}
		})
	}
}

// TestApplyPriorityTier covers the injection rules on the routing context
// alone, without going through the ext_proc handler.
func TestApplyPriorityTier(t *testing.T) {
	const body = `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`

	tests := []struct {
		name         string
		enabled      bool
		headers      map[string]string
		body         string
		wantPriority *int
	}{
		{
			name:    "opt-out deployment forwards the body byte for byte",
			enabled: false,
			headers: map[string]string{HeaderPriorityTier: "batch"},
			body:    body,
		},
		{
			name:    "opt-in without tier forwards the body byte for byte",
			enabled: true,
			headers: map[string]string{},
			body:    body,
		},
		{
			name:    "opt-in with an unmapped tier forwards the body byte for byte",
			enabled: true,
			headers: map[string]string{HeaderPriorityTier: "realtime"},
			body:    body,
		},
		{
			name:         "opt-in with batch sets priority 100",
			enabled:      true,
			headers:      map[string]string{HeaderPriorityTier: "batch"},
			body:         body,
			wantPriority: intPtr(100),
		},
		{
			name:         "opt-in with background sets priority 1000",
			enabled:      true,
			headers:      map[string]string{HeaderPriorityTier: "background"},
			body:         body,
			wantPriority: intPtr(1000),
		},
		{
			name:         "a priority the caller set is not overwritten",
			enabled:      true,
			headers:      map[string]string{HeaderPriorityTier: "batch"},
			body:         `{"model":"test-model","priority":7,"messages":[]}`,
			wantPriority: intPtr(7),
		},
		{
			name:         "a caller priority of zero is not overwritten either",
			enabled:      true,
			headers:      map[string]string{HeaderPriorityTier: "background"},
			body:         `{"model":"test-model","priority":0,"messages":[]}`,
			wantPriority: intPtr(0),
		},
		{
			// sjson turns a JSON array into an object instead of failing, so
			// the guard has to reject it before the rewrite is attempted.
			name:    "a JSON array body is forwarded untouched",
			enabled: true,
			headers: map[string]string{HeaderPriorityTier: "batch"},
			body:    `[1,2,3]`,
		},
		{
			name:    "a JSON null body is forwarded untouched",
			enabled: true,
			headers: map[string]string{HeaderPriorityTier: "batch"},
			body:    `null`,
		},
		{
			name:    "a JSON string body is forwarded untouched",
			enabled: true,
			headers: map[string]string{HeaderPriorityTier: "batch"},
			body:    `"hi"`,
		},
		{
			name:    "a truncated body is forwarded untouched",
			enabled: true,
			headers: map[string]string{HeaderPriorityTier: "batch"},
			body:    `{"model":"test-model"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{priorityTier: tt.enabled}
			routingCtx := types.NewRoutingContext(context.Background(), "", "test-model", "", "req-priority", "u")
			routingCtx.ReqBody = []byte(tt.body)
			for key, value := range tt.headers {
				routingCtx.ReqHeaders[key] = value
			}

			s.applyPriorityTier(routingCtx)

			if tt.wantPriority == nil {
				// The untouched cases must forward the very same slice contents,
				// not merely an equivalent JSON document.
				assert.Equal(t, tt.body, string(routingCtx.ReqBody))
				return
			}

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(routingCtx.ReqBody, &decoded))
			assert.EqualValues(t, *tt.wantPriority, decoded["priority"])

			// Everything the caller sent has to survive the rewrite.
			var original map[string]any
			require.NoError(t, json.Unmarshal([]byte(tt.body), &original))
			for key, want := range original {
				assert.Equal(t, want, decoded[key], "field %q must survive the rewrite", key)
			}
		})
	}
}

// TestHandleRequestBody_PriorityTier covers the end-to-end handler behaviour:
// with the deployment opt-in, a tier header reaches the upstream body and the
// rewritten content-length header matches it. Without the opt-in, neither the
// body nor any header changes.
func TestHandleRequestBody_PriorityTier(t *testing.T) {
	const body = `{"model":"test-model","messages":[{"role":"user","content":"test"}]}`

	podList := &utils.PodArray{
		Pods: []*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"},
				Status: v1.PodStatus{
					PodIP:      "1.2.3.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"},
				Status: v1.PodStatus{
					PodIP:      "4.5.6.7",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			},
		},
	}

	tests := []struct {
		name         string
		enabled      bool
		tier         string
		requestBody  string
		unrouted     bool
		wantPriority *int
	}{
		{
			name:         "opt-in maps batch to priority 100",
			enabled:      true,
			tier:         "batch",
			requestBody:  body,
			wantPriority: intPtr(100),
		},
		{
			name:         "opt-in maps a mixed-case tier",
			enabled:      true,
			tier:         "Background",
			requestBody:  body,
			wantPriority: intPtr(1000),
		},
		{
			name:         "opt-in leaves an unmapped tier alone",
			enabled:      true,
			tier:         "realtime",
			requestBody:  body,
			wantPriority: nil,
		},
		{
			name:         "an opt-out deployment ignores the tier",
			enabled:      false,
			tier:         "batch",
			requestBody:  body,
			wantPriority: nil,
		},
		{
			name:         "opt-in preserves a caller priority",
			enabled:      true,
			tier:         "batch",
			requestBody:  `{"model":"test-model","priority":3,"messages":[{"role":"user","content":"test"}]}`,
			wantPriority: intPtr(3),
		},
		{
			name:         "an unrouted request still gets a matching content-length",
			enabled:      true,
			tier:         "batch",
			requestBody:  body,
			unrouted:     true,
			wantPriority: intPtr(100),
		},
		{
			name:         "an unrouted request without a tier keeps the body size",
			enabled:      true,
			requestBody:  body,
			unrouted:     true,
			wantPriority: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockCache := &MockCache{Cache: cache.NewForTest()}
			mockRouter := new(mockRouter)
			registerTestRouter(mockRouter)

			mockCache.On("HasModel", "test-model").Return(true)
			mockCache.On("ListPodsByModel", "test-model").Return(podList, nil)
			mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1))
			mockCache.On("GetMetricValueByPod", mock.Anything, mock.Anything, mock.Anything).
				Return(&metrics.SimpleMetricValue{Value: 0}, nil).Maybe()
			// An unrouted request is delegated to the HTTPRoute, so no pod is selected.
			// A nil gateway client is the standalone mode that validateHTTPRouteStatus
			// treats as nothing to check, which keeps this case free of route mocks.
			if !tt.unrouted {
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).Once()
			}

			server := &Server{
				cache:            mockCache,
				modelRateLimiter: ratelimiter.NewNoopRateLimiter(),
				priorityTier:     tt.enabled,
			}

			req := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestBody{
					RequestBody: &extProcPb.HttpBody{Body: []byte(tt.requestBody)},
				},
			}

			routingCtx := types.NewRoutingContext(context.Background(), "", "", "", "req-priority", "u")
			routingCtx.ReqPath = PathChatCompletions
			if !tt.unrouted {
				routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(TestRouterAlgorithm)
			}
			routingCtx.ReqHeaders[HeaderPriorityTier] = tt.tier

			resp, model, _, term := server.HandleRequestBody(context.Background(), routingCtx, "req-priority", req, utils.User{Name: "u"})
			require.Nil(t, resp.GetImmediateResponse(), "the request must be routed, not rejected")
			assert.Equal(t, "test-model", model)
			assert.Equal(t, int64(1), term)

			common := resp.GetRequestBody().GetResponse()
			forwarded := common.GetBodyMutation().GetBody()

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(forwarded, &decoded))
			if tt.wantPriority == nil {
				assert.NotContains(t, decoded, "priority")
			} else {
				assert.EqualValues(t, *tt.wantPriority, decoded["priority"])
			}
			// The rest of the request has to reach the backend intact.
			assert.Equal(t, "test-model", decoded["model"])

			// Envoy validates the upstream body against the content-length the
			// header phase sets, so the two must agree.
			assertHeaderRawValue(t, common.GetHeaderMutation().GetSetHeaders(), "content-length", strconv.Itoa(len(forwarded)))
			mockCache.AssertExpectations(t)
		})
	}
}

// TestHandleRequestBody_PriorityTierBeforeRouting pins the ordering the PD
// router depends on: the priority has to be in the routing context before the
// router runs, because the PD prefill leg is built out of that same body while
// the router selects the pods (see pd/prefill.PreparePayload). A rewrite that
// waited for pod selection would leave the prefill leg without it, while the
// decode leg, which is forwarded from routingCtx.ReqBody afterwards, had it.
func TestHandleRequestBody_PriorityTierBeforeRouting(t *testing.T) {
	const body = `{"model":"test-model","messages":[{"role":"user","content":"test"}]}`

	// Two ready pods: with a single one selectTargetPod short-circuits into the
	// pinned-pod fast path without ever calling the router.
	podList := &utils.PodArray{
		Pods: []*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"},
				Status: v1.PodStatus{
					PodIP:      "1.2.3.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"},
				Status: v1.PodStatus{
					PodIP:      "4.5.6.7",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			},
		},
	}

	mockCache := &MockCache{Cache: cache.NewForTest()}
	mockRouter := new(mockRouter)
	registerTestRouter(mockRouter)

	mockCache.On("HasModel", "test-model").Return(true)
	mockCache.On("ListPodsByModel", "test-model").Return(podList, nil)
	mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1))
	mockCache.On("GetMetricValueByPod", mock.Anything, mock.Anything, mock.Anything).
		Return(&metrics.SimpleMetricValue{Value: 0}, nil).Maybe()

	var atRoute []byte
	mockRouter.On("Route", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			atRoute = args.Get(0).(*types.RoutingContext).ReqBody
		}).
		Return("1.2.3.4:8000", nil).Once()

	server := &Server{
		cache:            mockCache,
		modelRateLimiter: ratelimiter.NewNoopRateLimiter(),
		priorityTier:     true,
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{Body: []byte(body)},
		},
	}

	routingCtx := types.NewRoutingContext(context.Background(), "", "", "", "req-priority", "u")
	routingCtx.ReqPath = PathChatCompletions
	routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(TestRouterAlgorithm)
	routingCtx.ReqHeaders[HeaderPriorityTier] = "batch"

	resp, _, _, _ := server.HandleRequestBody(context.Background(), routingCtx, "req-priority", req, utils.User{Name: "u"})
	require.Nil(t, resp.GetImmediateResponse(), "the request must be routed, not rejected")

	require.NotNil(t, atRoute, "the router must have been called")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(atRoute, &decoded))
	assert.EqualValues(t, 100, decoded["priority"], "the router must see the priority in the request body")

	// The decode leg is forwarded from the routing context, which is the body
	// the router already saw.
	forwarded := resp.GetRequestBody().GetResponse().GetBodyMutation().GetBody()
	assert.Equal(t, atRoute, forwarded)
	mockCache.AssertExpectations(t)
}

func intPtr(value int) *int {
	return &value
}
