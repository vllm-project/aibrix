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
	"github.com/stretchr/testify/assert"
)

func TestHandleProcessingRequest_RequestHeaders_SetsRoutingContext(t *testing.T) {
	s := &Server{}
	st := &processState{
		ctx:       context.Background(),
		requestID: "test-req-id",
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{},
				},
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "gateway_req_headers", st.metricLabel)
	if assert.NotNil(t, st.routerCtx) {
		assert.NotEqual(t, st.routerCtx, st.ctx)
	}
	assert.Equal(t, "", st.model)
}
