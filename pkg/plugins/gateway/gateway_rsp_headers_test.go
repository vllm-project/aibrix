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
	"net/http"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	// Header parsing is deliberately lifecycle-free. Process owns request
	// completion and RoutingContext cleanup.
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
			name:           "non-200 success (204)",
			routingCtx:     createRoutingCtx(true, nil),
			responseStatus: "204",
			expected: testResponse{
				processingErrorCode: 0,
				isProcessingError:   false,
				headers: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: HeaderWentIntoReqHeaders, RawValue: []byte("true")}},
					{Header: &configPb.HeaderValue{Key: HeaderRequestID, RawValue: []byte("test-req-id")}},
					{Header: &configPb.HeaderValue{Key: "routing-strategy", RawValue: []byte("random")}},
					{Header: &configPb.HeaderValue{Key: HeaderTargetPod, RawValue: []byte("test-pod")}},
					{Header: &configPb.HeaderValue{Key: HeaderTargetPodIP, RawValue: []byte("10.0.0.1:8000")}},
					{Header: &configPb.HeaderValue{Key: ":status", RawValue: []byte("204")}},
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

			ctx := context.Background()

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

			resp, isErr, isErrCode := server.HandleResponseHeaders(ctx, tt.routingCtx, "test-req-id", "test-model", req)

			// Validate status code from :status
			assert.Equal(t, tt.expected.processingErrorCode, isErrCode)

			// Validate processing error flag
			assert.Equal(t, tt.expected.isProcessingError, isErr)

			// Validate headers set in response
			actualHeaders := resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
			if !cmp.Equal(tt.expected.headers, actualHeaders, protocmp.Transform()) {
				t.Fatalf("Headers do not match:\n%s", cmp.Diff(tt.expected.headers, actualHeaders, protocmp.Transform()))
			}
			mockCache.AssertNotCalled(t, "DoneRequestCount")
		})
	}
}

// newVideoResponseHeadersRequest builds the ResponseHeaders message Envoy sends
// with only the pseudo-header the video hooks look at: the status code.
func newVideoResponseHeadersRequest(status string) *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: ":status", RawValue: []byte(status)},
				}},
			},
		},
	}
}

// videoDeleteRoutingContext mirrors what the request phases leave behind for a
// DELETE follow-up: ReqPath still holds the client's original path, so it is
// still the public id -- the backend id only ever appears in the path header
// mutation handed to Envoy.
func videoDeleteRoutingContext(t *testing.T, publicJobID string) *types.RoutingContext {
	t.Helper()
	routerCtx := types.NewRoutingContext(context.Background(), "", "wan2.1-vace-1.3b", "", "req-del", "")
	routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodDelete}
	routerCtx.ReqPath = PathVideos + "/" + publicJobID
	return routerCtx
}

// TestHandleResponseHeaders_VideoDeleteRemovesRecord covers the only point in
// the flow where a record is legitimately dropped on the client's behalf: the
// backend confirmed the job is gone (2xx), or already did not have it (404).
func TestHandleResponseHeaders_VideoDeleteRemovesRecord(t *testing.T) {
	for _, status := range []string{"200", "204", "404"} {
		t.Run("backend "+status, func(t *testing.T) {
			s, _, registry := newTestVideoJobServer(t)
			record := registerTestVideoJob(t, registry, asyncJobOwnerShared, "wan2.1-vace-1.3b", "video_gen_abc",
				readyPod("pod-a", "ns-a", "10.0.0.5"))

			resp, _, _ := s.HandleResponseHeaders(context.Background(), videoDeleteRoutingContext(t, record.PublicJobID),
				"req-del", "wan2.1-vace-1.3b", newVideoResponseHeadersRequest(status))

			assert.Nil(t, resp.GetImmediateResponse())
			assert.Equal(t, 0, videoJobRecordCount(t, registry, asyncJobOwnerShared))
		})
	}
}

// TestHandleResponseHeaders_VideoDeleteKeepsRecordOnBackendFailure keeps a
// retry possible: the job may well still exist on the pod, and forgetting it
// here would strand it with no way for the client to address it again.
func TestHandleResponseHeaders_VideoDeleteKeepsRecordOnBackendFailure(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	record := registerTestVideoJob(t, registry, asyncJobOwnerShared, "wan2.1-vace-1.3b", "video_gen_abc",
		readyPod("pod-a", "ns-a", "10.0.0.5"))

	resp, isErr, code := s.HandleResponseHeaders(context.Background(), videoDeleteRoutingContext(t, record.PublicJobID),
		"req-del", "wan2.1-vace-1.3b", newVideoResponseHeadersRequest("500"))

	assert.True(t, isErr)
	assert.Equal(t, 500, code)
	assert.Nil(t, resp.GetImmediateResponse())
	assert.Equal(t, 1, videoJobRecordCount(t, registry, asyncJobOwnerShared))
}

// TestHandleResponseHeaders_VideoDeleteCleanupFailureReturns503 tells the truth
// to the client: the backend deleted the job, but the registry still lists it,
// so the caller must retry the DELETE to finish the cleanup.
func TestHandleResponseHeaders_VideoDeleteCleanupFailureReturns503(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	record := registerTestVideoJob(t, registry, asyncJobOwnerShared, "wan2.1-vace-1.3b", "video_gen_abc",
		readyPod("pod-a", "ns-a", "10.0.0.5"))
	registry.store = &failingAsyncJobStore{asyncJobStore: registry.store, deleteErr: errAsyncJobStoreUnavailable}

	resp, _, _ := s.HandleResponseHeaders(context.Background(), videoDeleteRoutingContext(t, record.PublicJobID),
		"req-del", "wan2.1-vace-1.3b", newVideoResponseHeadersRequest("204"))

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, resp.GetImmediateResponse().GetStatus().GetCode())
}

// TestHandleResponseHeaders_VideoJSONDropsContentLength: the create/status/delete
// body gets its id rewritten, so the upstream content-length no longer describes it.
// Leaving the header would make Envoy truncate or stall the response.
func TestHandleResponseHeaders_VideoJSONDropsContentLength(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		wantRemoved bool
	}{
		{"create response is rewritten", http.MethodPost, PathVideos, true},
		{"status response is rewritten", http.MethodGet, PathVideos + "/aibrixjob-abc", true},
		{"content response streams untouched", http.MethodGet, PathVideos + "/aibrixjob-abc/content", false},
		{"list response never reaches upstream", http.MethodGet, PathVideos, false},
		{"delete response is rewritten", http.MethodDelete, PathVideos + "/aibrixjob-abc", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := newTestVideoJobServer(t)
			routerCtx := types.NewRoutingContext(context.Background(), "", "wan2.1-vace-1.3b", "", "req-1", "")
			routerCtx.ReqHeaders = map[string]string{methodKey: tt.method}
			routerCtx.ReqPath = tt.path

			resp, _, _ := s.HandleResponseHeaders(context.Background(), routerCtx, "req-1", "wan2.1-vace-1.3b",
				newVideoResponseHeadersRequest("200"))

			removed := resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetRemoveHeaders()
			if tt.wantRemoved {
				assert.Contains(t, removed, "content-length")
			} else {
				assert.NotContains(t, removed, "content-length")
			}
		})
	}
}
