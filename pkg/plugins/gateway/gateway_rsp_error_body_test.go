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
	"net/http"
	"strconv"
	"strings"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
)

func TestHandleProcessingRequest_ChunkedUpstreamError(t *testing.T) {
	const nested = `{"error":{"message":"invalid parameter","type":"invalid_request_error","param":"top_p","code":400}}`
	for _, tc := range []struct {
		name    string
		chunks  []string
		message string
	}{
		{"single chunk", []string{nested}, "invalid parameter"},
		{"empty end chunk", []string{nested, ""}, "invalid parameter"},
		{"split JSON", []string{nested[:25], nested[25:70], nested[70:]}, "invalid parameter"},
		{"empty initial chunk", []string{"", nested, ""}, "invalid parameter"},
		{"flat error", []string{`{"message":"invalid `, `parameter","type":"invalid_request_error"}`}, "invalid parameter"},
		{"plain text", []string{"invalid ", "parameter", ""}, "invalid parameter"},
		{"empty body", []string{""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counters := captureCounters(t)
			server := &Server{}
			st := &processState{
				ctx: context.Background(), requestID: "chunked-error", model: "m",
				routerCtx:   types.NewRoutingContext(context.Background(), "random", "m", "", "chunked-error", ""),
				isRespError: true, respErrorCode: http.StatusBadRequest,
			}
			defer st.routerCtx.Delete()
			defer st.releaseModelInFlight()
			var downstream []byte
			for i, chunk := range tc.chunks {
				final := i == len(tc.chunks)-1
				resp, err := server.handleProcessingRequest(st, errorBodyChunk(chunk, final))
				require.NoError(t, err)
				require.Nil(t, resp.GetImmediateResponse(), "a local reply after streamed headers can reset the response")
				body := resp.GetResponseBody().GetResponse()
				require.NotNil(t, body)
				mutation := body.GetBodyMutation()
				require.NotNil(t, mutation, "unmodified chunks would leak partial upstream JSON")
				if !mutation.GetClearBody() {
					downstream = append(downstream, mutation.GetBody()...)
				}
				assert.Equal(t, final, st.completed)
				if final {
					assert.Nil(t, st.respErrorBody, "release the completed error buffer")
					headers := body.GetHeaderMutation().GetSetHeaders()
					require.NotEmpty(t, headers, "final chunk must retain buffered/empty-response header mutations")
					foundJSON := false
					for _, h := range headers {
						if strings.EqualFold(h.GetHeader().GetKey(), "content-type") {
							foundJSON = true
							assert.Empty(t, h.Header.GetValue())
							assert.Equal(t, []byte("application/json"), h.Header.GetRawValue())
							assert.Equal(t, configPb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD, h.GetAppendAction())
						}
					}
					require.True(t, foundJSON, "final chunk must set JSON Content-Type through RawValue")
				}
				if !final {
					assert.Empty(t, downstream)
					assert.Empty(t, requestOutcomeCounters(counters()))
				}
			}
			var parsed struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(downstream, &parsed))
			assert.Equal(t, tc.message, parsed.Error.Message)
			if tc.chunks[0] == nested || tc.name == "split JSON" || tc.name == "empty initial chunk" {
				assert.JSONEq(t, nested, string(downstream))
			}
			outcomes := requestOutcomeCounters(counters())
			require.Len(t, outcomes, 1)
			assert.Equal(t, metrics.GatewayRequestModelFailTotal, outcomes[0].name)
			assert.Equal(t, "400", outcomes[0].labels["status_code"])
			// A queued chunk after completion must not generate another error or metric.
			resp, err := server.handleProcessingRequest(st, errorBodyChunk("", true))
			require.NoError(t, err)
			assert.Nil(t, resp.GetImmediateResponse())
			assert.True(t, resp.GetResponseBody().GetResponse().GetBodyMutation().GetClearBody())
			assert.Len(t, requestOutcomeCounters(counters()), 1)
		})
	}
}

func errorBodyChunk(body string, final bool) *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{Body: []byte(body), EndOfStream: final},
		},
	}
}

func TestHandleResponseHeaders_ErrorBodyFraming(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			server := &Server{}
			req := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseHeaders{
				ResponseHeaders: &extProcPb.HttpHeaders{Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: ":status", RawValue: []byte(strconv.Itoa(code))},
					{Key: "content-length", RawValue: []byte("17")},
					{Key: "content-type", RawValue: []byte("text/plain")},
				}}},
			}}
			resp, isError, gotCode := server.HandleResponseHeaders(context.Background(), nil, "error-framing", "m", req)
			mutation := resp.GetResponseHeaders().GetResponse().GetHeaderMutation()
			require.NotNil(t, mutation)
			if code >= 400 {
				assert.True(t, isError)
				assert.Equal(t, code, gotCode)
				assert.Contains(t, mutation.GetRemoveHeaders(), "content-length")
				contentType := &configPb.HeaderValueOption{
					Header:       &configPb.HeaderValue{Key: "content-type", RawValue: []byte("application/json")},
					AppendAction: configPb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
				}
				assert.Contains(t, mutation.GetSetHeaders(), contentType)
			} else {
				assert.False(t, isError)
				assert.NotContains(t, mutation.GetRemoveHeaders(), "content-length")
				assert.NotContains(t, mutation.GetSetHeaders(), contentTypeHeader())
			}
		})
	}
}

func TestHandleProcessingRequest_ErrorBodyRetentionIsBounded(t *testing.T) {
	counters := captureCounters(t)
	server := &Server{}
	st := &processState{
		ctx: context.Background(), requestID: "large-error", model: "m",
		routerCtx:   types.NewRoutingContext(context.Background(), "random", "m", "", "large-error", ""),
		isRespError: true, respErrorCode: http.StatusBadGateway,
	}
	defer st.routerCtx.Delete()
	defer st.releaseModelInFlight()
	// Reuse a large incoming chunk: retention must depend on the error limit,
	// not the total upstream response size.
	chunk := strings.Repeat("x", 1024*1024)
	for i := 0; i < 6; i++ {
		resp, err := server.handleProcessingRequest(st, errorBodyChunk(chunk, false))
		require.NoError(t, err)
		require.True(t, resp.GetResponseBody().GetResponse().GetBodyMutation().GetClearBody())
		require.LessOrEqual(t, len(st.respErrorBody), 64*1024)
	}
	resp, err := server.handleProcessingRequest(st, errorBodyChunk("", true))
	require.NoError(t, err)
	var result struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(resp.GetResponseBody().GetResponse().GetBodyMutation().GetBody(), &result))
	assert.True(t, strings.HasPrefix(result.Error.Message, strings.Repeat("x", 64*1024)))
	assert.Contains(t, result.Error.Message, "upstream error body truncated")
	assert.Less(t, len(result.Error.Message), 64*1024+128)
	assert.Nil(t, st.respErrorBody)
	require.Len(t, requestOutcomeCounters(counters()), 1)
	assert.Equal(t, "502", requestOutcomeCounters(counters())[0].labels["status_code"])
}

func TestHandleProcessingRequest_ErrorEndsOnHeaders(t *testing.T) {
	counters := captureCounters(t)
	ctx := types.NewRoutingContext(context.Background(), "random", "m", "", "headers-only", "")
	defer ctx.Delete()
	mc := &MockCache{}
	mc.On("DoneRequestCount", ctx, "headers-only", "m", int64(0)).Return().Once()
	server := &Server{cache: mc}
	st := &processState{ctx: context.Background(), routerCtx: ctx, requestID: "headers-only", model: "m"}
	defer st.releaseModelInFlight()
	req := newVideoResponseHeadersRequest("400")
	req.GetResponseHeaders().EndOfStream = true
	resp, err := server.handleProcessingRequest(st, req)
	require.NoError(t, err)
	immediate := resp.GetImmediateResponse()
	require.NotNil(t, immediate, "no body callback is available to finish this error")
	assert.Equal(t, 400, int(immediate.GetStatus().GetCode()))
	assert.JSONEq(t, `{"error":{"message":"","type":"invalid_request_error","code":null,"param":null}}`, immediate.GetBody())
	assert.True(t, st.completed)
	require.Len(t, requestOutcomeCounters(counters()), 1)
	assert.Equal(t, "400", requestOutcomeCounters(counters())[0].labels["status_code"])
	mc.AssertExpectations(t)
}
