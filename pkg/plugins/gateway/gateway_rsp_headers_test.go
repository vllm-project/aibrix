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

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/testing/protocmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	routingalgorithms "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
)

func Test_HandleResponseHeaders(t *testing.T) {
	// Initialize routing algorithms if needed
	routingalgorithms.Init()

	// Mock cache to verify DoneRequestCount is called on error
	mockCache := &MockCache{Cache: cache.NewForTest()}
	server := &Server{
		cache: mockCache,
	}

	// Helper to create a minimal valid RoutingContext for tests
	createRoutingCtx := func(hasRouted bool, respHeaders map[string]string) *types.RoutingContext {
		ctx := types.NewRoutingContext(context.Background(), "random", "test-model", "", "test-req-id", "test-user")
		if hasRouted {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Status: v1.PodStatus{
					PodIP: "10.0.0.1",
				},
			}
			ctx.SetTargetPod(pod)
		}
		ctx.RespHeaders = respHeaders
		return ctx
	}

	type testResponse struct {
		processingErrorCode int
		isProcessingError   bool
		headers             []*configPb.HeaderValueOption
	}

	type testCase struct {
		name           string
		routingCtx     *types.RoutingContext
		responseStatus string // value of :status header
		respHeaders    map[string]string
		expected       testResponse
	}

	tests := []testCase{
		{
			name:           "successful response (200)",
			routingCtx:     createRoutingCtx(true, map[string]string{"X-Custom": "value"}),
			responseStatus: "200",
			expected: testResponse{
				processingErrorCode: 0,
				isProcessingError:   false,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderWentIntoReqHeaders, RawValue: []byte("true")}},
					{Header: &configPb.HeaderValue{Key: HeaderRequestID, RawValue: []byte("test-req-id")}},
					{Header: &configPb.HeaderValue{Key: "routing-strategy", RawValue: []byte("random")}},
					{Header: &configPb.HeaderValue{Key: HeaderTargetPod, RawValue: []byte("test-pod")}},
					{Header: &configPb.HeaderValue{Key: HeaderTargetPodIP, RawValue: []byte("10.0.0.1:8000")}},
					{Header: &configPb.HeaderValue{Key: "X-Custom", RawValue: []byte("value")}},
					{Header: &configPb.HeaderValue{Key: ":status", RawValue: []byte("200")}},
				},
			},
		},
		{
			name:           "error response (500)",
			routingCtx:     createRoutingCtx(true, nil),
			responseStatus: "500",
			expected: testResponse{
				processingErrorCode: 500,
				isProcessingError:   true,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderWentIntoReqHeaders, RawValue: []byte("true")}},
					{Header: &configPb.HeaderValue{Key: HeaderRequestID, RawValue: []byte("test-req-id")}},
					{Header: &configPb.HeaderValue{Key: "routing-strategy", RawValue: []byte("random")}},
					{Header: &configPb.HeaderValue{Key: HeaderTargetPod, RawValue: []byte("test-pod")}},
					{Header: &configPb.HeaderValue{Key: HeaderTargetPodIP, RawValue: []byte("10.0.0.1:8000")}},
					{Header: &configPb.HeaderValue{Key: ":status", RawValue: []byte("500")}},
				},
			},
		},
		{
			name:           "nil routing context",
			routingCtx:     nil,
			responseStatus: "200",
			expected: testResponse{
				processingErrorCode: 0,
				isProcessingError:   false,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderWentIntoReqHeaders, RawValue: []byte("true")}},
					{Header: &configPb.HeaderValue{Key: HeaderRequestID, RawValue: []byte("test-req-id")}},
					{Header: &configPb.HeaderValue{Key: ":status", RawValue: []byte("200")}},
				},
			},
		},
		{
			name:           "response headers include pseudo-header (should be skipped)",
			routingCtx:     createRoutingCtx(false, map[string]string{":path": "/ignored", "X-Real": "ok"}),
			responseStatus: "200",
			expected: testResponse{
				processingErrorCode: 0,
				isProcessingError:   false,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderWentIntoReqHeaders, RawValue: []byte("true")}},
					{Header: &configPb.HeaderValue{Key: HeaderRequestID, RawValue: []byte("test-req-id")}},
					{Header: &configPb.HeaderValue{Key: "X-Real", RawValue: []byte("ok")}},
					{Header: &configPb.HeaderValue{Key: ":status", RawValue: []byte("200")}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			var ctx context.Context
			if tt.routingCtx != nil {
				ctx = tt.routingCtx
			} else {
				ctx = context.Background()
			}

			if tt.expected.isProcessingError {
				mockCache.On("DoneRequestCount",
					tt.routingCtx,
					"test-req-id",
					"test-model",
					int64(0),
				).Return()
			}

			// Build response headers input
			headers := []*configPb.HeaderValue{
				{Key: ":status", RawValue: []byte(tt.responseStatus)},
			}
			req := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_ResponseHeaders{
					ResponseHeaders: &extProcPb.HttpHeaders{
						Headers: &configPb.HeaderMap{Headers: headers},
					},
				},
			}

			resp, isErr, isErrCode := server.HandleResponseHeaders(ctx, "test-req-id", "test-model", req)

			// Validate status code from :status
			assert.Equal(t, tt.expected.processingErrorCode, isErrCode)

			// Validate processing error flag
			assert.Equal(t, tt.expected.isProcessingError, isErr)

			// Validate headers set in response
			actualHeaders := resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
			if !cmp.Equal(tt.expected.headers, actualHeaders, protocmp.Transform()) {
				t.Fatalf("Headers do not match:\n%s", cmp.Diff(tt.expected.headers, actualHeaders, protocmp.Transform()))
			}
		})
	}
}
