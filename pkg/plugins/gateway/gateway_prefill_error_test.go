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
	"errors"
	"fmt"
	"strings"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// prefillLegError builds the error chain a synchronous PD prefill failure carries
// at the gateway: pd.PrefillHTTPError wrapped by the prefill executor
// (algorithms/pd/prefill/default.go) and then by the PD router
// (algorithms/pd/router/router.go).
func prefillLegError(statusCode int, body string) error {
	prefillErr := &pd.PrefillHTTPError{StatusCode: statusCode, Body: body}
	executorErr := fmt.Errorf("prefill request failed for request r1, pod prefill-0: %w", prefillErr)
	return fmt.Errorf("prefill request failed for request r1: %w", executorErr)
}

func prefillErrorRoutingContext(t *testing.T) *types.RoutingContext {
	t.Helper()
	routingCtx := types.NewRoutingContext(context.Background(), "", "", "", "test-request-id", "test-user")
	routingCtx.ReqPath = PathChatCompletions
	return routingCtx
}

type prefillErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func TestBuildTargetSelectionErrorResponse_PassesPrefillEngineErrorsThrough(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  envoyTypePb.StatusCode
		wantMessage string
		wantType    string
		wantParam   string
		wantCode    any
		rawContains string
		wantMarker  bool
	}{
		{
			name:        "vLLM rejects a sampling parameter",
			err:         prefillLegError(400, `{"error":{"message":"temperature must be in [0, 2], got 2.1. (parameter=temperature, value=2.1)","type":"BadRequestError","param":"temperature","code":400}}`),
			wantStatus:  envoyTypePb.StatusCode_BadRequest,
			wantMessage: "temperature must be in [0, 2], got 2.1. (parameter=temperature, value=2.1)",
			wantType:    "BadRequestError",
			wantParam:   "temperature",
			wantCode:    float64(400),
		},
		{
			name:        "vLLM rejects audio in a text-only deployment",
			err:         prefillLegError(400, `{"error":{"message":"At most 0 audio(s) may be provided in one prompt. (parameter=audio)","type":"BadRequestError","param":"audio","code":400}}`),
			wantStatus:  envoyTypePb.StatusCode_BadRequest,
			wantMessage: "At most 0 audio(s) may be provided in one prompt. (parameter=audio)",
			wantType:    "BadRequestError",
			wantParam:   "audio",
			wantCode:    float64(400),
		},
		{
			name:        "a flat upstream payload keeps its fields",
			err:         prefillLegError(400, `{"message":"bad input","type":"invalid_request_error","param":"messages","code":"bad_request"}`),
			wantStatus:  envoyTypePb.StatusCode_BadRequest,
			wantMessage: "bad input",
			wantType:    "invalid_request_error",
			wantParam:   "messages",
			wantCode:    "bad_request",
		},
		{
			name:        "an unprocessable entity keeps its status",
			err:         prefillLegError(422, `{"error":{"message":"unsupported content type","type":"UnprocessableEntityError","param":"messages","code":422}}`),
			wantStatus:  envoyTypePb.StatusCode_UnprocessableEntity,
			wantMessage: "unsupported content type",
			wantType:    "UnprocessableEntityError",
			wantParam:   "messages",
			wantCode:    float64(422),
		},
		{
			name:        "a non-JSON body still passes the status",
			err:         prefillLegError(400, "upstream refused the request"),
			wantStatus:  envoyTypePb.StatusCode_BadRequest,
			rawContains: "upstream refused the request",
		},
		{
			name:        "an engine 5xx keeps its status and is marked",
			err:         prefillLegError(500, `{"error":{"message":"CUDA out of memory","type":"InternalServerError","param":"","code":500}}`),
			wantStatus:  envoyTypePb.StatusCode_InternalServerError,
			wantMessage: "CUDA out of memory",
			wantType:    "InternalServerError",
			wantCode:    float64(500),
			wantMarker:  true,
		},
		{
			name:        "a non-JSON 5xx keeps its status and marker",
			err:         prefillLegError(502, "bad gateway page"),
			wantStatus:  envoyTypePb.StatusCode_BadGateway,
			rawContains: "bad gateway page",
			wantMarker:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := buildTargetSelectionErrorResponse(prefillErrorRoutingContext(t), "test-request-id", "test-model", tc.err)
			require.NotNil(t, resp, "a prefill engine error must not fall through to the generic 503")
			assert.Equal(t, tc.wantStatus, resp.GetImmediateResponse().GetStatus().GetCode())
			if tc.wantMarker {
				assertHeaderRawValue(t, resp.GetImmediateResponse().GetHeaders().GetSetHeaders(), HeaderErrorPDPrefill, "true")
			} else {
				assertHeaderAbsent(t, resp.GetImmediateResponse().GetHeaders().GetSetHeaders(), HeaderErrorPDPrefill)
			}

			body := resp.GetImmediateResponse().GetBody()
			if tc.rawContains != "" {
				assert.Contains(t, body, tc.rawContains)
				return
			}
			var got prefillErrorBody
			require.NoError(t, json.Unmarshal([]byte(body), &got))
			assert.Equal(t, tc.wantMessage, got.Error.Message)
			assert.Equal(t, tc.wantType, got.Error.Type)
			assert.Equal(t, tc.wantParam, got.Error.Param)
			assert.Equal(t, tc.wantCode, got.Error.Code)
		})
	}
}

func TestBuildTargetSelectionErrorResponse_MatchesNonPDErrorBody(t *testing.T) {
	// Same body and status the non-PD path produces for the same upstream error
	// (see processRequestBodyError in gateway.go). Only a 4xx can match: the non-PD
	// path answers a 5xx with its own generic message instead of the engine's body.
	const engineBody = `{"error":{"message":"temperature must be in [0, 2], got 2.1. (parameter=temperature, value=2.1)","type":"BadRequestError","param":"temperature","code":400}}`

	normalizedBody, statusCode, ok := normalizeUpstreamErrorBody([]byte(engineBody), 400)
	require.True(t, ok)
	nonPDResp := buildErrorResponseWithBody(statusCode, normalizedBody, nil)

	resp := buildTargetSelectionErrorResponse(prefillErrorRoutingContext(t), "test-request-id", "test-model",
		prefillLegError(400, engineBody))
	require.NotNil(t, resp)

	assert.Equal(t, nonPDResp.GetImmediateResponse().GetStatus().GetCode(), resp.GetImmediateResponse().GetStatus().GetCode())
	// Key order is not part of the contract: the normalization re-marshals the upstream
	// fields, so compare the payloads semantically.
	assert.JSONEq(t, nonPDResp.GetImmediateResponse().GetBody(), resp.GetImmediateResponse().GetBody())
	assertHeaderAbsent(t, resp.GetImmediateResponse().GetHeaders().GetSetHeaders(), HeaderErrorPDPrefill)
}

func TestBuildTargetSelectionErrorResponse_MarksPrefillTransportFailures(t *testing.T) {
	// The gateway cannot reach the prefill pod: there is no engine answer to pass
	// through, so the status stays 503 but the marker and the wrapped error tell
	// the client which leg failed.
	transportErr := &pd.PrefillTransportError{Err: errors.New("read tcp 10.0.0.1:34567->10.0.0.2:8000: read: connection reset by peer")}
	wrapped := fmt.Errorf("prefill request failed for request r1, pod prefill-0: %w", transportErr)
	wrapped = fmt.Errorf("prefill request failed for request r1: %w", wrapped)

	resp := buildTargetSelectionErrorResponse(prefillErrorRoutingContext(t), "test-request-id", "test-model", wrapped)
	require.NotNil(t, resp)

	immediate := resp.GetImmediateResponse()
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, immediate.GetStatus().GetCode())
	var got prefillErrorBody
	require.NoError(t, json.Unmarshal([]byte(immediate.GetBody()), &got))
	assert.Contains(t, got.Error.Message, "prefill request failed (transport)")
	assert.Contains(t, got.Error.Message, "connection reset by peer")
	assertHeaderRawValue(t, immediate.GetHeaders().GetSetHeaders(), HeaderErrorPDPrefill, "true")
	assertHeaderAbsent(t, immediate.GetHeaders().GetSetHeaders(), HeaderErrorRouting)
}

func TestBuildTargetSelectionErrorResponse_KeepsGatewayFailuresGeneric(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "an untyped failure",
			err:  errors.New("boom"),
		},
		{
			name: "a status Envoy cannot emit",
			err:  prefillLegError(599, `{"error":{"message":"unknown"}}`),
		},
		{
			name: "a status below the client-error range",
			err:  prefillLegError(204, `{"error":{"message":"no answer"}}`),
		},
		{
			name: "a redirect below the client-error range",
			err:  prefillLegError(302, `{"error":{"message":"moved"}}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := buildTargetSelectionErrorResponse(prefillErrorRoutingContext(t), "test-request-id", "test-model", tc.err)
			assert.Nil(t, resp, "untyped failures and unnamed statuses keep the generic 503")
		})
	}

	// A 4xx that arrives without the router wrapping is still recognized.
	resp := buildTargetSelectionErrorResponse(prefillErrorRoutingContext(t), "test-request-id", "test-model",
		&pd.PrefillHTTPError{StatusCode: 400, Body: `{"error":{"message":"bad","type":"BadRequestError","param":"temperature","code":400}}`})
	require.NotNil(t, resp)
	assert.Equal(t, envoyTypePb.StatusCode_BadRequest, resp.GetImmediateResponse().GetStatus().GetCode())
}

func TestBuildTargetSelectionErrorResponse_EmptyEngineBodyFallsBackToStatusMessage(t *testing.T) {
	resp := buildTargetSelectionErrorResponse(prefillErrorRoutingContext(t), "test-request-id", "test-model",
		prefillLegError(400, ""))
	require.NotNil(t, resp)

	var got prefillErrorBody
	require.NoError(t, json.Unmarshal([]byte(resp.GetImmediateResponse().GetBody()), &got))
	assert.Equal(t, "prefill request failed with status 400", got.Error.Message)
}

func TestBuildTargetSelectionErrorResponse_BoundsNonJSONEngineBody(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)

	resp := buildTargetSelectionErrorResponse(prefillErrorRoutingContext(t), "test-request-id", "test-model",
		prefillLegError(502, huge))
	require.NotNil(t, resp)

	var got prefillErrorBody
	require.NoError(t, json.Unmarshal([]byte(resp.GetImmediateResponse().GetBody()), &got))
	assert.Len(t, got.Error.Message, maxPrefillFailureMessage+3)
}

func TestHandleRequestBody_PrefillErrorsReachTheClient(t *testing.T) {
	tests := []struct {
		name       string
		prefillErr error
		wantStatus envoyTypePb.StatusCode
		wantBody   string
		wantMarker bool
	}{
		{
			name:       "a 4xx is the engine's own answer",
			prefillErr: prefillLegError(400, `{"error":{"message":"temperature must be in [0, 2], got 2.1. (parameter=temperature, value=2.1)","type":"BadRequestError","param":"temperature","code":400}}`),
			wantStatus: envoyTypePb.StatusCode_BadRequest,
			wantBody:   "temperature must be in [0, 2]",
		},
		{
			name:       "a 5xx is marked as a prefill failure",
			prefillErr: prefillLegError(500, `{"error":{"message":"CUDA out of memory","type":"InternalServerError","param":"","code":500}}`),
			wantStatus: envoyTypePb.StatusCode_InternalServerError,
			wantBody:   "CUDA out of memory",
			wantMarker: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := handleRequestBodyWithPrefillError(t, tc.prefillErr)
			assert.Equal(t, tc.wantStatus, resp.GetImmediateResponse().GetStatus().GetCode())
			assert.Contains(t, resp.GetImmediateResponse().GetBody(), tc.wantBody)
			if tc.wantMarker {
				assertHeaderRawValue(t, resp.GetImmediateResponse().GetHeaders().GetSetHeaders(), HeaderErrorPDPrefill, "true")
			} else {
				assertHeaderAbsent(t, resp.GetImmediateResponse().GetHeaders().GetSetHeaders(), HeaderErrorPDPrefill)
			}
			assertHeaderAbsent(t, resp.GetImmediateResponse().GetHeaders().GetSetHeaders(), HeaderErrorRouting)
		})
	}
}

// handleRequestBodyWithPrefillError runs the request-body phase with a PD router
// that fails the prefill leg, and returns the response the client would get.
func handleRequestBodyWithPrefillError(t *testing.T, prefillErr error) *extProcPb.ProcessingResponse {
	t.Helper()

	mockCache := &MockCache{Cache: cache.NewForTest()}
	mockRouter := new(mockRouter)
	registerTestRouter(mockRouter)

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

	mockCache.On("HasModel", "test-model").Return(true).Once()
	mockCache.On("ListPodsByModel", "test-model").Return(podList, nil).Once()
	mockCache.On("GetMetricValueByPod", mock.Anything, mock.Anything, mock.Anything).
		Return(&metrics.SimpleMetricValue{Value: 0}, nil).Maybe()

	// The PD router returns the executor's prefill error unchanged to the gateway.
	mockRouter.On("Route", mock.Anything, mock.Anything).Return("", prefillErr).Once()

	server := &Server{
		cache:               mockCache,
		requestCountTracker: map[string]int{},
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body: []byte(`{"model":"test-model","temperature":2.1,"messages":[{"role":"user","content":"hi"}]}`),
			},
		},
	}

	routingCtx := prefillErrorRoutingContext(t)
	routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(TestRouterAlgorithm)

	resp, _, _, _ := server.HandleRequestBody(context.Background(), routingCtx, "test-request-id", req, utils.User{Name: "test-user"})
	require.NotNil(t, resp)
	return resp
}

// assertHeaderAbsent checks that the response carries none of the gateway's own
// failure markers.
func assertHeaderAbsent(t *testing.T, headers []*configPb.HeaderValueOption, key string) {
	t.Helper()
	for _, h := range headers {
		if h.GetHeader().GetKey() == key {
			t.Errorf("header %q must not be set, got %q", key, string(h.GetHeader().GetRawValue()))
		}
	}
}
