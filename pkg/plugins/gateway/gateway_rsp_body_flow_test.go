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
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/errresp"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func Test_responseErrorProcessing_ErrorCodeAndMessage(t *testing.T) {
	baseResp := &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{Header: &configPb.HeaderValue{Key: "x-test", RawValue: []byte("1")}},
						},
					},
				},
			},
		},
	}

	t.Run("401 maps to invalid_api_key and appends httproute error", func(t *testing.T) {
		mockGW := &MockGatewayClient{}
		mockGWV1 := &MockGatewayV1Client{}
		mockHTTP := &MockHTTPRouteClient{}
		mockGW.On("GatewayV1").Return(mockGWV1)
		mockGWV1.On("HTTPRoutes", "aibrix-system").Return(mockHTTP)
		mockHTTP.On("Get", mock.Anything, "m-router", mock.Anything).Return((*gatewayv1.HTTPRoute)(nil), errors.New("httproute boom"))

		s := &Server{gatewayClient: mockGW, gatewayNamespace: "aibrix-system"}
		out := s.responseErrorProcessing(context.Background(), nil, baseResp, 401, "m", "rid", "Incorrect API key provided")
		ir := out.GetImmediateResponse()
		if assert.NotNil(t, ir) {
			assert.Equal(t, envoyTypePb.StatusCode(401), ir.GetStatus().GetCode())
			var parsed map[string]any
			assert.NoError(t, json.Unmarshal([]byte(ir.GetBody()), &parsed))
			errObj := parsed["error"].(map[string]any)
			assert.Equal(t, errresp.ErrorTypeAuthentication, errObj["type"])
			assert.Equal(t, ErrorCodeInvalidAPIKey, errObj["code"])
			assert.Contains(t, errObj["message"].(string), "Incorrect API key provided")
			assert.Contains(t, errObj["message"].(string), "httproute boom")
			assert.Len(t, ir.GetHeaders().GetSetHeaders(), 2)
		}

		mockGW.AssertExpectations(t)
		mockGWV1.AssertExpectations(t)
		mockHTTP.AssertExpectations(t)
	})

	t.Run("explicit routing skips httproute on error path", func(t *testing.T) {
		mockGW := &MockGatewayClient{}
		s := &Server{gatewayClient: mockGW, gatewayNamespace: "aibrix-system"}
		rctx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "", "rid", "")
		out := s.responseErrorProcessingWithHeaders(context.Background(), rctx, nil, 404, "m", "rid", `{"detail":"Not Found"}`)
		ir := out.GetImmediateResponse()
		if assert.NotNil(t, ir) {
			var parsed map[string]any
			assert.NoError(t, json.Unmarshal([]byte(ir.GetBody()), &parsed))
			errObj := parsed["error"].(map[string]any)
			assert.Equal(t, `{"detail":"Not Found"}`, errObj["message"])
		}
		mockGW.AssertNotCalled(t, "GatewayV1")
	})

	t.Run("503 maps to service_unavailable", func(t *testing.T) {
		s := &Server{gatewayClient: nil}
		out := s.responseErrorProcessing(context.Background(), nil, baseResp, 503, "m", "rid", "server shutdown")
		ir := out.GetImmediateResponse()
		if assert.NotNil(t, ir) {
			assert.Equal(t, envoyTypePb.StatusCode(503), ir.GetStatus().GetCode())
			var parsed map[string]any
			assert.NoError(t, json.Unmarshal([]byte(ir.GetBody()), &parsed))
			errObj := parsed["error"].(map[string]any)
			assert.Equal(t, errresp.ErrorTypeOverloaded, errObj["type"])
			assert.Equal(t, ErrorCodeServiceUnavailable, errObj["code"])
		}
	})

	t.Run("500 keeps code null", func(t *testing.T) {
		s := &Server{gatewayClient: nil}
		out := s.responseErrorProcessing(context.Background(), nil, baseResp, 500, "m", "rid", "internal error")
		ir := out.GetImmediateResponse()
		if assert.NotNil(t, ir) {
			assert.Equal(t, envoyTypePb.StatusCode(500), ir.GetStatus().GetCode())
			var parsed map[string]any
			assert.NoError(t, json.Unmarshal([]byte(ir.GetBody()), &parsed))
			errObj := parsed["error"].(map[string]any)
			_, hasCode := errObj["code"]
			assert.True(t, hasCode)
			assert.Nil(t, errObj["code"])
		}
	})

	t.Run("400 with nested upstream error body is not double-wrapped (#2578)", func(t *testing.T) {
		s := &Server{gatewayClient: nil}
		body := `{"error": {"message": "top_p must be in (0, 1], got 2.0.", "type": "BadRequestError", "param": "top_p", "code": 400}}`
		// Provide an explicit-routing ctx so validateHTTPRouteStatus is skipped.
		rctx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "", "rid", "")
		out := s.responseErrorProcessingWithHeaders(context.Background(), rctx, baseResp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders(), 400, "m", "rid", body)
		ir := out.GetImmediateResponse()
		if assert.NotNil(t, ir) {
			assert.Equal(t, envoyTypePb.StatusCode(400), ir.GetStatus().GetCode())
			var parsed map[string]any
			require.NoError(t, json.Unmarshal([]byte(ir.GetBody()), &parsed))
			errObj, ok := parsed["error"].(map[string]any)
			require.True(t, ok, "expected nested error object, got: %s", ir.GetBody())
			assert.Equal(t, "top_p must be in (0, 1], got 2.0.", errObj["message"])
			assert.Equal(t, "BadRequestError", errObj["type"])
			assert.Equal(t, "top_p", errObj["param"])
			assert.EqualValues(t, 400, errObj["code"])
		}
	})

	t.Run("400 with non-error body falls back to string wrap", func(t *testing.T) {
		s := &Server{gatewayClient: nil}
		rctx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "", "rid", "")
		out := s.responseErrorProcessingWithHeaders(context.Background(), rctx, nil, 400, "m", "rid", "plain text failure")
		ir := out.GetImmediateResponse()
		if assert.NotNil(t, ir) {
			var parsed map[string]any
			require.NoError(t, json.Unmarshal([]byte(ir.GetBody()), &parsed))
			errObj := parsed["error"].(map[string]any)
			assert.Equal(t, "plain text failure", errObj["message"])
			assert.Equal(t, errresp.ErrorTypeInvalidRequest, errObj["type"])
		}
	})

	// Header status wins over a semantic (string) body "code". The upstream body carries
	// code:"invalid_api_key", but the gateway observed a 401 header status; the HTTP status
	// must stay 401 and the string code is preserved verbatim in the body. Regression guard
	// for the unified status-precedence rule.
	t.Run("401 header status wins over semantic string body code", func(t *testing.T) {
		s := &Server{gatewayClient: nil}
		rctx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "", "rid", "")
		body := `{"error": {"message": "invalid api key", "type": "authentication_error", "param": null, "code": "invalid_api_key"}}`
		out := s.responseErrorProcessingWithHeaders(context.Background(), rctx, nil, 401, "m", "rid", body)
		ir := out.GetImmediateResponse()
		if assert.NotNil(t, ir) {
			assert.Equal(t, envoyTypePb.StatusCode(401), ir.GetStatus().GetCode())
			var parsed map[string]any
			require.NoError(t, json.Unmarshal([]byte(ir.GetBody()), &parsed))
			errObj, ok := parsed["error"].(map[string]any)
			require.True(t, ok, "expected nested error object, got: %s", ir.GetBody())
			assert.Equal(t, "invalid api key", errObj["message"])
			assert.Equal(t, "authentication_error", errObj["type"])
			assert.Equal(t, "invalid_api_key", errObj["code"])
		}
	})
}

func TestHandleProcessingRequest_ResponseBody_ErrorFromPreviousStage_UsesErrorProcessor(t *testing.T) {
	s := &Server{} // standalone mode; validateHTTPRouteStatus is skipped in error processor

	st := &processState{
		ctx:           context.Background(),
		requestID:     "rid",
		model:         "m",
		isRespError:   true,
		respErrorCode: 401,
		lastRespHeaders: []*configPb.HeaderValueOption{
			{Header: &configPb.HeaderValue{Key: "x-test", RawValue: []byte("1")}},
		},
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"error":"boom"}`),
				EndOfStream: true,
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	require.Nil(t, resp.GetImmediateResponse())
	body := resp.GetResponseBody().GetResponse().GetBodyMutation().GetBody()
	assert.Contains(t, string(body), "boom")
	assert.Equal(t, 401, st.respErrorCode)
	assert.True(t, st.completed)
	// metricLabel should be set for response body processing
	assert.Equal(t, gatewayRespBody, st.metricLabel)
}

func TestHandleProcessingRequest_ResponseBody_ErrorRewritesPinnedVideoBackendID(t *testing.T) {
	s := &Server{} // explicit routing skips HTTPRoute validation in the error processor
	publicID := "aibrixjob-public"
	backendID := "video-abc"
	routerCtx := types.NewRoutingContext(context.Background(), routing.RouterRandom, "video-model", "", "rid-video-error", "")
	routerCtx.ReqPath = PathVideos + "/" + publicID
	routerCtx.AsyncJobBackendID = backendID

	st := &processState{
		ctx:           context.Background(),
		routerCtx:     routerCtx,
		requestID:     "rid-video-error",
		model:         "video-model",
		isRespError:   true,
		respErrorCode: http.StatusNotFound,
	}
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"error":{"message":"Video video-abc not found","param":"video-abc","code":404}}`),
				EndOfStream: true,
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)
	require.NoError(t, err)
	require.Nil(t, resp.GetImmediateResponse())
	body := resp.GetResponseBody().GetResponse().GetBodyMutation().GetBody()
	assert.Equal(t, http.StatusNotFound, st.respErrorCode)
	assert.Contains(t, string(body), publicID)
	assert.NotContains(t, string(body), backendID)
}

// TestHandleProcessingRequest_Non200ResponseHeadersThenErrorBody is the end-to-end
// reproduction for #2578: an upstream engine (vLLM / SGLang) returns a non-2xx status
// with an OpenAI-style error body. The prior fix only normalized error bodies on the
// 200 path (processLanguageResponse), so this non-200 path must normalize too,
// otherwise the raw upstream JSON gets double-wrapped into error.message.
func TestHandleProcessingRequest_Non200ResponseHeadersThenErrorBody(t *testing.T) {
	testPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status: v1.PodStatus{
			PodIP:      "1.2.3.4",
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
	testCache := cache.NewWithPodsForTest([]*v1.Pod{testPod}, "test-model")
	s := &Server{cache: testCache}

	requestID := "req-2578"
	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", requestID, "")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()
	routerCtx.SetTargetPod(testPod)

	st := &processState{
		ctx:       context.Background(),
		routerCtx: routerCtx,
		requestID: requestID,
		model:     "test-model",
		stream:    false,
	}

	// 1) Upstream returns :status 400.
	headersReq := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{
						{Key: ":status", RawValue: []byte("400")},
					},
				},
			},
		},
	}
	resp, err := s.handleProcessingRequest(st, headersReq)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.True(t, st.isRespError, "expected isRespError=true after non-200 response header")
	assert.Equal(t, 400, st.respErrorCode)

	// 2) Upstream error body arrives. This is the exact #2578 payload.
	errorBody := `{"error": {"message": "top_p must be in (0, 1], got 2.0. (parameter=top_p, value=2.0)", "type": "BadRequestError", "param": "top_p", "code": 400}}`
	bodyReq := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(errorBody),
				EndOfStream: true,
			},
		},
	}

	resp, err = s.handleProcessingRequest(st, bodyReq)
	assert.NoError(t, err)
	require.Nil(t, resp.GetImmediateResponse())
	body := resp.GetResponseBody().GetResponse().GetBodyMutation().GetBody()
	assert.Equal(t, 400, st.respErrorCode)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(body, &parsed))
	errObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "expected nested error object, got: %s", body)
	// The upstream message must not be wrapped into a stringified JSON.
	assert.Equal(t, "top_p must be in (0, 1], got 2.0. (parameter=top_p, value=2.0)", errObj["message"])
	assert.Equal(t, "BadRequestError", errObj["type"])
	assert.Equal(t, "top_p", errObj["param"])
	assert.EqualValues(t, 400, errObj["code"])
	assert.Equal(t, gatewayRespBody, st.metricLabel)
}

func TestHandleProcessingRequest_ResponseBody_SuccessMarksCompletionAndEmitsSuccessMetric(t *testing.T) {
	// Use real in-memory cache for underlying HandleResponseBody logic.
	// Register a pod for "test-model" so DoneRequestTrace finds a non-nil OutputPredictor.
	testPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status: v1.PodStatus{
			PodIP:      "1.2.3.4",
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
	testCache := cache.NewWithPodsForTest([]*v1.Pod{testPod}, "test-model")
	s := &Server{
		cache: testCache,
	}

	requestID := "test-req-id"
	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", requestID, "")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()

	st := &processState{
		ctx:       context.Background(),
		routerCtx: routerCtx,
		requestID: requestID,
		model:     "test-model",
		stream:    false,
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model": "test-model", "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}`),
				EndOfStream: true,
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.NotNil(t, resp.GetResponseBody())
	assert.True(t, st.completed, "expected processState.completed to be true")
	assert.True(t, st.isGatewayRspDone, "expected gateway response to be marked done exactly once")
	assert.Equal(t, gatewayRespBody, st.metricLabel)
}

// TestHandleProcessingRequest_ResponseBody_NotYetCompleted covers the ResponseBody switch
// case when the stream is still in progress (EndOfStream=false). The response is returned
// but completed stays false and the success metric is not emitted yet.
func TestHandleProcessingRequest_ResponseBody_NotYetCompleted(t *testing.T) {
	// No cache interaction expected: DoneRequestTrace is only called when complete transitions to true.
	s := &Server{}

	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", "req-rb-partial", "")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()

	st := &processState{
		ctx:       context.Background(),
		routerCtx: routerCtx,
		requestID: "req-rb-partial",
		model:     "test-model",
		stream:    false,
		completed: false,
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"id":"chunk-1"}`),
				EndOfStream: false,
			},
		},
	}

	resp, err := s.handleProcessingRequest(st, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.NotNil(t, resp.GetResponseBody(), "expected a body response for partial data")
	assert.False(t, st.completed, "stream should not be marked complete before EndOfStream")
	assert.False(t, st.isGatewayRspDone)
	assert.Equal(t, gatewayRespBody, st.metricLabel)
}
