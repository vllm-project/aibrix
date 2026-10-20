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

// Tests for the custom voice API: GET /v1/audio/voices, POST /v1/audio/voices and
// DELETE /v1/audio/voices/{name}. The requests name no model, so the gateway reads it from the
// "model" query parameter, and GET and DELETE have no body, so they are routed at RequestHeaders.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	routingalgorithms "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func Test_IsAudioVoicesPath(t *testing.T) {
	for path, want := range map[string]bool{
		PathAudioVoices:                       true,
		PathAudioVoices + "?model=tts-1":      true,
		PathAudioVoices + "/narrator":         true,
		PathAudioVoices + "/narrator?model=a": true,
		PathAudioVoices + "/":                 true,
		PathAudioSpeech:                       false,
		PathAudioSpeechBatch:                  false,
		"/v1/audio/voicesfoo":                 false, // a different segment, not a sub-resource
		"/v1/audio":                           false,
		PathAudioTranscriptions:               false,
	} {
		assert.Equal(t, want, isAudioVoicesPath(path), path)
	}
}

func Test_ValidateRequestBody_AudioVoices(t *testing.T) {
	testCases := []struct {
		message     string
		requestPath string
		model       string
		statusCode  envoyTypePb.StatusCode
		param       string
	}{
		{
			message:     "list routes on the model query parameter",
			requestPath: PathAudioVoices + "?model=tts-1",
			model:       "tts-1",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "upload routes on the model query parameter",
			requestPath: PathAudioVoices + "?model=tts-1",
			model:       "tts-1",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "delete routes on the model query parameter",
			requestPath: PathAudioVoices + "/narrator?model=tts-1",
			model:       "tts-1",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "other query parameters are ignored",
			requestPath: PathAudioVoices + "?api-version=1&model=tts-1",
			model:       "tts-1",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "a model name is percent-decoded",
			requestPath: PathAudioVoices + "?model=org%2Ftts-1",
			model:       "org/tts-1",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "surrounding whitespace in the model name is trimmed",
			requestPath: PathAudioVoices + "?model=%20tts-1%20",
			model:       "tts-1",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "missing model",
			requestPath: PathAudioVoices,
			statusCode:  envoyTypePb.StatusCode_BadRequest,
			param:       "model",
		},
		{
			message:     "missing model on a voice",
			requestPath: PathAudioVoices + "/narrator",
			statusCode:  envoyTypePb.StatusCode_BadRequest,
			param:       "model",
		},
		{
			message:     "empty model",
			requestPath: PathAudioVoices + "?model=",
			statusCode:  envoyTypePb.StatusCode_BadRequest,
			param:       "model",
		},
		{
			message:     "blank model",
			requestPath: PathAudioVoices + "?model=%20",
			statusCode:  envoyTypePb.StatusCode_BadRequest,
			param:       "model",
		},
		{
			message:     "repeated model is ambiguous",
			requestPath: PathAudioVoices + "?model=tts-1&model=tts-2",
			statusCode:  envoyTypePb.StatusCode_BadRequest,
			param:       "model",
		},
		{
			message:     "malformed query string",
			requestPath: PathAudioVoices + "?model=tts-1&x=%zz",
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
	}

	for _, tt := range testCases {
		t.Run(tt.message, func(t *testing.T) {
			// Whatever the engine is sent as the body (a voice sample, or nothing) is not read.
			model, message, prefixText, stream, errRes := validateRequestBody("test-request-id", tt.requestPath, []byte("not json"), utils.User{})

			assert.False(t, stream)
			assert.Empty(t, message)
			assert.Empty(t, prefixText)

			if tt.statusCode == envoyTypePb.StatusCode_OK {
				assert.Nil(t, errRes)
				assert.Equal(t, tt.model, model)
				return
			}

			require.NotNil(t, errRes)
			assert.Equal(t, tt.statusCode, errRes.GetImmediateResponse().GetStatus().GetCode())
			assert.Empty(t, model)

			var errResponse map[string]any
			require.NoError(t, sonic.Unmarshal([]byte(errRes.GetImmediateResponse().GetBody()), &errResponse))
			errObj, ok := errResponse["error"].(map[string]any)
			require.True(t, ok, "response should have an 'error' object")
			if tt.param != "" {
				assert.Equal(t, tt.param, errObj["param"])
			}
		})
	}
}

func voiceHeadersRequest(method, path string, endOfStream bool, extra ...*configPb.HeaderValue) *extProcPb.ProcessingRequest {
	headers := append([]*configPb.HeaderValue{
		{Key: pathKey, RawValue: []byte(path)},
		{Key: methodKey, RawValue: []byte(method)},
	}, extra...)
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers:     &configPb.HeaderMap{Headers: headers},
				EndOfStream: endOfStream,
			},
		},
	}
}

func newVoicesServer(t *testing.T) (*Server, *MockCache) {
	t.Helper()
	cache.InitForTest()
	routingalgorithms.Init()

	mockCache := &MockCache{Cache: cache.NewForTest()}
	pod := readyPod("pod-a", "ns-a", "1.2.3.4")
	mockCache.On("HasModel", "tts-1").Return(true)
	mockCache.On("ListPodsByModel", "tts-1").Return(&utils.PodArray{Pods: []*v1.Pod{pod}}, nil)
	mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "tts-1").Return(int64(1))
	mockCache.On("GetMetricValueByPod", mock.Anything, mock.Anything, mock.Anything).
		Return(&metrics.SimpleMetricValue{Value: 0}, nil).Maybe()
	return &Server{cache: mockCache}, mockCache
}

// TestHandleRequestHeaders_AudioVoicesWithoutABodyAreRoutedAtHeaders: Envoy sends no RequestBody
// message for a GET or DELETE, so the pod has to be chosen in the RequestHeaders response, with
// the route cache cleared so that the pinned route or the model route is matched.
func TestHandleRequestHeaders_AudioVoicesWithoutABodyAreRoutedAtHeaders(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"list", http.MethodGet, PathAudioVoices + "?model=tts-1"},
		{"delete", http.MethodDelete, PathAudioVoices + "/narrator?model=tts-1"},
		{"the model is read next to other parameters", http.MethodGet, PathAudioVoices + "?x=1&model=tts-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name+" with a routing strategy", func(t *testing.T) {
			server, mockCache := newVoicesServer(t)

			req := voiceHeadersRequest(tt.method, tt.path, true,
				&configPb.HeaderValue{Key: HeaderRoutingStrategy, RawValue: []byte(routingalgorithms.RouterLeastRequest)})
			resp, _, _, routingCtx, term := server.HandleRequestHeaders(
				context.Background(), "req-voices", trace.SpanFromContext(context.Background()), req)

			require.Nil(t, resp.GetImmediateResponse(), "a valid voice request must not be rejected")
			common := resp.GetRequestHeaders().GetResponse()
			require.NotNil(t, common, "the routing decision has to be the RequestHeaders response")
			assert.True(t, common.GetClearRouteCache(), "Envoy has to match the pinned route again")
			assertHeaderRawValue(t, common.GetHeaderMutation().GetSetHeaders(), HeaderTargetPod, "1.2.3.4:8000")
			assertHeaderRawValue(t, common.GetHeaderMutation().GetSetHeaders(), HeaderRoutingStrategy, string(routingalgorithms.RouterLeastRequest))
			for _, h := range common.GetHeaderMutation().GetSetHeaders() {
				assert.NotEqual(t, pathKey, h.GetHeader().GetKey(), "the path, and the model parameter in it, reach the engine as sent")
			}

			assert.Equal(t, "tts-1", routingCtx.Model, "Process takes the model for metrics and request counting from here")
			assert.Equal(t, int64(1), term)
			mockCache.AssertCalled(t, "AddRequestCount", mock.Anything, "req-voices", "tts-1")
		})

		t.Run(tt.name+" without a routing strategy", func(t *testing.T) {
			server, _ := newVoicesServer(t)

			req := voiceHeadersRequest(tt.method, tt.path, true)
			resp, _, _, routingCtx, _ := server.HandleRequestHeaders(
				context.Background(), "req-voices", trace.SpanFromContext(context.Background()), req)

			require.Nil(t, resp.GetImmediateResponse(), "a valid voice request must not be rejected")
			common := resp.GetRequestHeaders().GetResponse()
			require.NotNil(t, common)
			assert.True(t, common.GetClearRouteCache(), "Envoy has to match the model's HTTPRoute")
			assertHeaderRawValue(t, common.GetHeaderMutation().GetSetHeaders(), HeaderModel, "tts-1")
			for _, h := range common.GetHeaderMutation().GetSetHeaders() {
				assert.NotEqual(t, HeaderTargetPod, h.GetHeader().GetKey())
			}
			assert.Equal(t, "tts-1", routingCtx.Model)
		})
	}
}

// TestHandleRequestHeaders_AudioVoicesWithoutABodyAreRejectedAtHeaders: a request the gateway
// cannot route is answered where it is routed, because nothing else will ever look at it.
func TestHandleRequestHeaders_AudioVoicesWithoutABodyAreRejectedAtHeaders(t *testing.T) {
	cache.InitForTest()
	routingalgorithms.Init()

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus envoyTypePb.StatusCode
		wantParam  string
	}{
		{"list without a model", http.MethodGet, PathAudioVoices, envoyTypePb.StatusCode_BadRequest, "model"},
		{"delete without a model", http.MethodDelete, PathAudioVoices + "/narrator", envoyTypePb.StatusCode_BadRequest, "model"},
		{"list of an unknown model", http.MethodGet, PathAudioVoices + "?model=nope", envoyTypePb.StatusCode_BadRequest, "model"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockCache := &MockCache{Cache: cache.NewForTest()}
			mockCache.On("HasModel", "nope").Return(false)
			server := &Server{cache: mockCache}

			resp, _, _, _, _ := server.HandleRequestHeaders(
				context.Background(), "req-voices", trace.SpanFromContext(context.Background()),
				voiceHeadersRequest(tt.method, tt.path, true))

			immediate := resp.GetImmediateResponse()
			require.NotNil(t, immediate, "the request must be answered by the gateway")
			assert.Equal(t, tt.wantStatus, immediate.GetStatus().GetCode())
			assert.Contains(t, immediate.GetBody(), `"param":"`+tt.wantParam+`"`)
			mockCache.AssertNotCalled(t, "AddRequestCount", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// TestHandleRequestHeaders_AudioVoiceUploadWaitsForItsBody: an upload has a body, which is where
// the pod is chosen, so its RequestHeaders response is the ordinary one: no pod yet.
func TestHandleRequestHeaders_AudioVoiceUploadWaitsForItsBody(t *testing.T) {
	server, mockCache := newVoicesServer(t)

	resp, _, _, routingCtx, _ := server.HandleRequestHeaders(
		context.Background(), "req-voices", trace.SpanFromContext(context.Background()),
		voiceHeadersRequest(http.MethodPost, PathAudioVoices+"?model=tts-1", false))

	require.Nil(t, resp.GetImmediateResponse())
	common := resp.GetRequestHeaders().GetResponse()
	require.NotNil(t, common)
	assertHeaderRawValue(t, common.GetHeaderMutation().GetSetHeaders(), HeaderWentIntoReqHeaders, "true")
	for _, h := range common.GetHeaderMutation().GetSetHeaders() {
		assert.NotEqual(t, HeaderTargetPod, h.GetHeader().GetKey(), "the upload is routed with its body")
	}
	assert.Empty(t, routingCtx.Model)
	mockCache.AssertNotCalled(t, "AddRequestCount", mock.Anything, mock.Anything, mock.Anything)
}

// TestHandleRequestBody_AudioVoiceUploadReachesABackendPodUnchanged: the upload is a multipart
// body with a voice sample in it. It is routed on the model query parameter and forwarded byte
// for byte, so the sample, the consent and every engine option reach the engine as sent.
func TestHandleRequestBody_AudioVoiceUploadReachesABackendPodUnchanged(t *testing.T) {
	server, _ := newVoicesServer(t)

	body, contentType := buildMultipartFormWithFile(t,
		map[string]string{"name": "narrator", "consent": "consent-1", "ref_text": "We asked twenty people.", "x_future_option": "1"},
		"audio_sample", "narrator.wav", []byte("RIFF....WAVEfmt "))
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{Body: body},
		},
	}

	routingCtx := types.NewRoutingContext(context.Background(), "", "", "", "test-request-id", "test-user")
	routingCtx.ReqPath = PathAudioVoices + "?model=tts-1"
	routingCtx.ReqHeaders[contentTypeKey] = contentType
	routingCtx.ReqHeaders[methodKey] = http.MethodPost
	routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(routingalgorithms.RouterLeastRequest)

	resp, model, stream, _ := server.HandleRequestBody(context.Background(), routingCtx, "test-request-id", req, utils.User{Name: "test-user"})

	require.NotNil(t, resp)
	require.Nil(t, resp.GetImmediateResponse(), "the request must not be rejected")
	assert.Equal(t, "tts-1", model)
	assert.False(t, stream)

	common := resp.GetRequestBody().GetResponse()
	assert.Equal(t, body, common.GetBodyMutation().GetBody())
	assertHeaderRawValue(t, common.GetHeaderMutation().GetSetHeaders(), HeaderTargetPod, "1.2.3.4:8000")
}

// TestHandleRequestBody_AudioVoiceUploadWithoutAModelIsRejected: a model form field does not stand
// in for the query parameter. The same rule has to hold for the three calls, and a GET and a
// DELETE have no form to read it from.
func TestHandleRequestBody_AudioVoiceUploadWithoutAModelIsRejected(t *testing.T) {
	server, _ := newVoicesServer(t)

	body, contentType := buildMultipartForm(t, map[string]string{"name": "narrator", "model": "tts-1"})
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{Body: body},
		},
	}

	routingCtx := types.NewRoutingContext(context.Background(), "", "", "", "test-request-id", "test-user")
	routingCtx.ReqPath = PathAudioVoices
	routingCtx.ReqHeaders[contentTypeKey] = contentType
	routingCtx.ReqHeaders[methodKey] = http.MethodPost

	resp, _, _, _ := server.HandleRequestBody(context.Background(), routingCtx, "test-request-id", req, utils.User{Name: "test-user"})

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_BadRequest, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Contains(t, resp.GetImmediateResponse().GetBody(), `"param":"model"`)
}

// TestHandleResponseBody_AudioVoicesPassThrough: the voice API answers in JSON, but not with a
// completion. A list, an upload receipt and a delete message have no "model" and no usage, so
// were they read as a language response each would be turned into an error. They are forwarded
// as sent, and the request completes on the last chunk.
func TestHandleResponseBody_AudioVoicesPassThrough(t *testing.T) {
	server := &Server{}

	tests := []struct {
		name   string
		path   string
		chunks []string
	}{
		{
			name:   "list",
			path:   PathAudioVoices + "?model=tts-1",
			chunks: []string{`{"voices":["alloy","echo"],"uploaded_vo`, `ices":[{"name":"narrator","consent":"c-1","created_at":1,"file_size":10,"mime_type":"audio/wav"}]}`},
		},
		{
			name:   "upload",
			path:   PathAudioVoices + "?model=tts-1",
			chunks: []string{`{"success":true,"voice":{"name":"narrator"}}`},
		},
		{
			name:   "delete",
			path:   PathAudioVoices + "/narrator?model=tts-1",
			chunks: []string{`{"success":true,"message":"Voice 'narrator' deleted successfully"}`},
		},
		{
			name:   "delete of a voice that does not exist",
			path:   PathAudioVoices + "/ghost?model=tts-1",
			chunks: []string{`{"success":false,"error":"Voice 'ghost' not found"}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestID := "test-req-voices-" + tt.name + "-" + time.Now().Format("150405.000")
			routerCtx := types.NewRoutingContext(context.Background(), "random", "tts-1", "", requestID, "test-user")
			routerCtx.ReqPath = tt.path
			routerCtx.RequestTime = time.Now()

			for i, chunk := range tt.chunks {
				req := &extProcPb.ProcessingRequest{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{Body: []byte(chunk), EndOfStream: i == len(tt.chunks)-1},
					},
				}

				resp, complete, usage := server.HandleResponseBody(context.Background(), routerCtx, requestID, req, utils.User{Name: "test-user"}, 0, "tts-1", false, false)

				assert.Equal(t, i == len(tt.chunks)-1, complete)
				if assert.NotNil(t, resp) {
					assert.Nil(t, resp.GetImmediateResponse(), "the engine's answer must not become a gateway error")
					assert.Nil(t, resp.GetResponseBody().GetResponse().GetBodyMutation(), "chunks must pass through unchanged")
				}
				assert.Equal(t, TokenUsage{}, usage)
			}

			_, buffered := requestBuffers.Load(requestID)
			assert.False(t, buffered, "the response must not be held in the language-response buffer")
		})
	}
}

// TestGatewayPluginManifest_SharedRouteServesAudioVoices: /v1/audio/voices has to match a reserved
// route before ext_proc can read its model. Its sub-resource, /v1/audio/voices/{name}, rides the
// same PathPrefix, which Envoy Gateway applies on path segment boundaries.
func TestGatewayPluginManifest_SharedRouteServesAudioVoices(t *testing.T) {
	manifest := loadGatewayPluginManifest(t)

	assert.Contains(t, manifest.pathMatches(t, "reserved-router"), manifestPathMatch{Type: "PathPrefix", Value: PathAudioVoices})
}

// TestGeneratedXDS_AudioVoicesStayOnTheSharedFilter: the voice responses are small JSON bodies the
// gateway does not rewrite, so they take the shared, streamed ext_proc filter in both phases, and
// the pinned phase has to land on the original-destination cluster for the chosen pod.
func TestGeneratedXDS_AudioVoicesStayOnTheSharedFilter(t *testing.T) {
	xds := loadGeneratedXDS(t)

	for _, path := range []string{PathAudioVoices, PathAudioVoices + "/narrator"} {
		initial, filter := xds.resolve(t, path, false)
		assert.Contains(t, initial.Name, "httproute/aibrix-system/aibrix-reserved-router/", "path %s should enter through the shared route", path)
		assert.Contains(t, filter.Name, "gateway-plugins-extension-policy", "path %s", path)
		assert.NotContains(t, filter.Name, "videos", "path %s", path)

		pinned, filter := xds.resolve(t, path, true)
		assert.Equal(t, "original_route", pinned.Name, "path %s", path)
		assert.Equal(t, "original_destination_cluster", pinned.Route.Cluster, "path %s", path)
		assert.Contains(t, filter.Name, "gateway-plugins-extension-policy", "path %s", path)
		assert.Equal(t, "STREAMED", filter.TypedConfig.ProcessingMode.ResponseBodyMode, "path %s", path)
	}
}
