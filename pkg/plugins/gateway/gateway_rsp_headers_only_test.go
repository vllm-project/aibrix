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
	"io"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// headersOnlyState is the processState of a request already routed to a pod, and a
// server that accepts the request-count release every exit path performs.
func headersOnlyState(t *testing.T) (*Server, *processState) {
	t.Helper()
	ctx := context.Background()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "1", Namespace: "ns"},
		Status:     v1.PodStatus{PodIP: "10.0.0.1"},
	}
	routerCtx := types.NewRoutingContext(ctx, "random", "test-model", "", "req-headers-only", "test-user")
	routerCtx.SetTargetPod(pod)

	cache := &MockCache{}
	cache.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	server := &Server{cache: cache, requestCountTracker: map[string]int{}}
	st := &processState{ctx: ctx, requestID: "req-headers-only", model: "test-model", routerCtx: routerCtx}
	return server, st
}

func headersOnlyResponseHeaders(status string, endOfStream bool) *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				EndOfStream: endOfStream,
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{
						{Key: ":status", RawValue: []byte(status)},
					},
				},
			},
		},
	}
}

// A success whose response headers end the message (a 204, an empty-body 200) has no body
// callback to mark it complete. It must complete on the headers, or the stream closing
// afterwards is counted as a cancelled request (499) for a request that was answered correctly.
func Test_HeadersOnlySuccess_IsComplete(t *testing.T) {
	for _, statusCode := range []string{"200", "204"} {
		t.Run(statusCode, func(t *testing.T) {
			server, st := headersOnlyState(t)

			_, err := server.handleProcessingRequest(st, headersOnlyResponseHeaders(statusCode, true))

			require.NoError(t, err)
			assert.True(t, st.completed, "a headers-only success is complete: no body callback will follow")
			assert.NoError(t, server.handleRecvError(st, io.EOF), "the stream closing afterwards is a normal end")
		})
	}
}

// With a body still to come, the response headers alone do not complete the request.
func Test_ResponseHeaders_WithBodyToFollowIsNotComplete(t *testing.T) {
	server, st := headersOnlyState(t)

	_, err := server.handleProcessingRequest(st, headersOnlyResponseHeaders("200", false))

	require.NoError(t, err)
	assert.False(t, st.completed)
}
