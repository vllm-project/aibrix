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
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/types"
)

// TestHandleProcessingRequest_ResponseHeaders_200 covers the ResponseHeaders switch case
// when the upstream returns 200 OK. No error is raised and isRespError stays false.
func TestHandleProcessingRequest_ResponseHeaders_200(t *testing.T) {
	s := &Server{} // no cache needed — DoneRequestCount is only called on non-200

	st := &processState{
		ctx:       context.Background(),
		requestID: "req-rh-200",
		model:     "test-model",
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{
						{Key: ":status", RawValue: []byte("200")},
					},
				},
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Nil(t, resp.GetImmediateResponse(), "expected no ImmediateResponse for 200 OK")
	assert.False(t, st.isRespError)
	assert.Equal(t, gatewayRespHeaders, st.metricLabel)
}

// TestHandleProcessingRequest_ResponseHeaders_NonOK covers the ResponseHeaders switch case
// when the upstream returns a non-200 status. HandleResponseHeaders calls DoneRequestCount
// and handleProcessingRequest transforms the response into an ImmediateResponse.
func TestHandleProcessingRequest_ResponseHeaders_NonOK(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", (*types.RoutingContext)(nil), "req-rh-401", "m", int64(0)).Return()

	s := &Server{
		cache:         mc,
		gatewayClient: nil, // standalone — validateHTTPRouteStatus skipped
	}

	st := &processState{
		ctx:       context.Background(),
		requestID: "req-rh-401",
		model:     "m",
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{
						{Key: ":status", RawValue: []byte("401")},
					},
				},
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	if assert.NotNil(t, resp) {
		imm := resp.GetImmediateResponse()
		assert.NotNil(t, imm, "expected ImmediateResponse for non-200 response header")
		assert.Equal(t, envoyTypePb.StatusCode(401), imm.GetStatus().GetCode())
	}
	assert.True(t, st.isRespError)
	assert.True(t, st.completed, "header-stage local replies must finish processing")
	assert.Equal(t, 401, st.respErrorCode)
	assert.Equal(t, gatewayRespHeaders, st.metricLabel)

	mc.AssertExpectations(t)
}
