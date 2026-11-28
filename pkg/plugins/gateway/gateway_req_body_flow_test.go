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

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/types"
)

// TestHandleProcessingRequest_RequestBody_ModelNotFound covers the RequestBody switch case
// when the model extracted from the body does not exist in the cache. The handler returns an
// immediate 400 response and sets metricLabel to gatewayReqBody.
func TestHandleProcessingRequest_RequestBody_ModelNotFound(t *testing.T) {
	mc := &MockCache{}
	mc.On("HasModel", "no-such-model").Return(false)

	s := &Server{cache: mc}

	routerCtx := types.NewRoutingContext(context.Background(), "random", "", "", "req-rb-1", "")
	routerCtx.ReqPath = PathChatCompletions

	st := &processState{
		ctx:       context.Background(),
		routerCtx: routerCtx,
		requestID: "req-rb-1",
		model:     "",
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body: []byte(`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`),
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	if assert.NotNil(t, resp) {
		assert.NotNil(t, resp.GetImmediateResponse(), "expected 400 ImmediateResponse for missing model")
		assert.Equal(t, envoyTypePb.StatusCode(400), resp.GetImmediateResponse().GetStatus().GetCode())
	}
	assert.Equal(t, gatewayReqBody, st.metricLabel)
	assert.Equal(t, "no-such-model", st.model)

	mc.AssertExpectations(t)
}
