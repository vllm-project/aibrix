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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/ssestream"
	"k8s.io/klog/v2"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

type OpenAIResponse struct {
	Model string `json:"model"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
	Code int `json:"code"`
}

func (s *Server) HandleResponseBody(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, user utils.User, rpm int64, model string, stream bool, traceTerm int64, hasCompleted bool) (*extProcPb.ProcessingResponse, bool) {
	b := req.Request.(*extProcPb.ProcessingRequest_ResponseBody)

	var processingRes *extProcPb.ProcessingResponse
	var promptTokens, completionTokens, totalTokens int64
	var headers []*configPb.HeaderValueOption
	headers = buildEnvoyProxyHeaders(headers, HeaderRequestID, requestID)

	complete := hasCompleted
	routerCtx, _ := ctx.(*types.RoutingContext)

	defer func() {
		// Wrapped in a function to delay the evaluation of parameters. Using complete to make sure DoneRequestTrace only call once for a request.
		if !hasCompleted && complete {
			s.cache.DoneRequestTrace(routerCtx, requestID, model, promptTokens, completionTokens, traceTerm)
			if routerCtx != nil {
				routerCtx.Delete()
			}
		}
	}()

	if stream {
		t := &http.Response{
			Body: io.NopCloser(bytes.NewReader(b.ResponseBody.GetBody())),
		}
		streaming := ssestream.NewStream[openai.ChatCompletionChunk](ssestream.NewDecoder(t), nil)
		defer func() {
			_ = streaming.Close()
		}()
		for streaming.Next() {
			evt := streaming.Current()
			if len(evt.Choices) == 0 {
				// Do not overwrite model, res can be empty.
				promptTokens = evt.Usage.PromptTokens
				totalTokens = evt.Usage.TotalTokens
				completionTokens = evt.Usage.CompletionTokens
			}
		}
		if err := streaming.Err(); err != nil {
			klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "responseBody", string(b.ResponseBody.GetBody()))
			complete = true
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorStreaming, RawValue: []byte("true"),
				}}},
				err.Error()), complete
		}
	} else {
		if isLanguageRequest(routerCtx.ReqPath) {
			processingRes, complete, promptTokens, completionTokens, totalTokens = processLanguageResponse(requestID, b)
			if processingRes != nil {
				return processingRes, complete
			}
		} else if !isLanguageRequest(routerCtx.ReqPath) {
			processingRes = s.processNonLanguangeResponse(ctx, b)
			if processingRes != nil {
				return processingRes, true
			}
			totalTokens = 1
		}
	}

	var requestEnd string
	if totalTokens != 0 {
		complete = true

		// Count token per user.
		if user.Name != "" {
			tpm, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_TPM_CURRENT", user.Name), totalTokens)
			if err != nil {
				return generateErrorResponse(
					envoyTypePb.StatusCode_InternalServerError,
					[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
						Key: HeaderErrorIncrTPM, RawValue: []byte("true"),
					}}},
					err.Error()), complete
			}
			headers = buildEnvoyProxyHeaders(headers, HeaderUpdateRPM, fmt.Sprintf("%d", rpm),
				HeaderUpdateTPM, fmt.Sprintf("%d", tpm))
			requestEnd = fmt.Sprintf(requestEnd+"rpm: %d, tpm: %d, ", rpm, tpm)
		}

		if routerCtx != nil && routerCtx.HasRouted() {
			headers = buildEnvoyProxyHeaders(headers, HeaderTargetPod, routerCtx.TargetAddress())
			requestEnd = fmt.Sprintf(requestEnd+"targetPod: %s", routerCtx.TargetAddress())
		}

		klog.Infof("request end, requestID: %s - %s", requestID, requestEnd)
	} else if b.ResponseBody.EndOfStream {
		complete = true
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseBody{
			ResponseBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
				},
			},
		},
	}, complete
}

func isLanguageRequest(requestPath string) bool {
	nonLanguagePrefixes := []string{
		"/v1/image/generations",
		"/v1/video/generations",
	}
	for _, prefix := range nonLanguagePrefixes {
		if strings.HasPrefix(requestPath, prefix) {
			return false
		}
	}
	return true
}

// processLanguageResponse processes output response for /chatcompletions, /completions and /embedding endpoints.
// nolint:nakedret
func processLanguageResponse(requestID string, b *extProcPb.ProcessingRequest_ResponseBody) (processingRes *extProcPb.ProcessingResponse, complete bool, promptTokens, completionTokens, totalTokens int64) {
	var res *OpenAIResponse
	// Use request ID as a key to store per-request buffer
	// Retrieve or create buffer
	buf, _ := requestBuffers.LoadOrStore(requestID, &bytes.Buffer{})
	buffer := buf.(*bytes.Buffer)
	// Append data to per-request buffer
	buffer.Write(b.ResponseBody.Body)

	if !b.ResponseBody.EndOfStream {
		// Partial data received, wait for more chunks, we just return a common response here.
		processingRes = &extProcPb.ProcessingResponse{
			Response: &extProcPb.ProcessingResponse_ResponseBody{
				ResponseBody: &extProcPb.BodyResponse{
					Response: &extProcPb.CommonResponse{},
				},
			},
		}
		return
	}

	// Last part received, process the full response
	finalBody := buffer.Bytes()
	// Clean up the buffer after final processing
	requestBuffers.Delete(requestID)

	if err := json.Unmarshal(finalBody, &res); err != nil {
		klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "responseBody", string(b.ResponseBody.GetBody()))
		complete = true
		processingRes = buildErrorResponse(envoyTypePb.StatusCode_InternalServerError, err.Error(), HeaderErrorResponseUnmarshal, "true")
		return
	}

	if len(res.Model) == 0 {
		msg := ErrorUnknownResponse.Error()
		responseBodyContent := string(b.ResponseBody.GetBody())
		if len(responseBodyContent) != 0 {
			msg = responseBodyContent
		}
		klog.ErrorS(ErrorUnknownResponse, "unexpected response", "requestID", requestID, "responseBody", responseBodyContent)

		code := envoyTypePb.StatusCode_InternalServerError
		if res.Code >= 100 && res.Code < 600 {
			code = envoyTypePb.StatusCode(res.Code)
		}

		complete = true
		processingRes = buildErrorResponse(code, msg, HeaderErrorResponseUnknown, "true")
		return
	}

	if res.Usage != nil {
		promptTokens = res.Usage.PromptTokens
		completionTokens = res.Usage.CompletionTokens
		totalTokens = res.Usage.TotalTokens
	}
	return
}

func (s *Server) processNonLanguangeResponse(ctx context.Context, b *extProcPb.ProcessingRequest_ResponseBody) (processingRes *extProcPb.ProcessingResponse) {
	routerCtx, _ := ctx.(*types.RoutingContext)
	if !routerCtx.SaveToRemoteStorage {
		return
	}

	var jsonMap map[string]interface{}
	if err := json.Unmarshal(b.ResponseBody.GetBody(), &jsonMap); err != nil {
		return buildErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			err.Error(), HeaderErrorResponseUnmarshal, "true")
	}

	storagePath, ok := jsonMap["output"].(string)
	if !ok {
		klog.ErrorS(ErrorUnknownResponse, "path not found in response", "requestID", routerCtx.RequestID, "responseBody", string(b.ResponseBody.GetBody()))
		return buildErrorResponse(envoyTypePb.StatusCode_InternalServerError, "path not found in response")
	}

	if err := s.writeStorageRequest(ctx, routerCtx.RequestID, types.RequestStore{
		RequestID:  routerCtx.RequestID,
		Status:     true,
		IP:         strings.Split(routerCtx.TargetAddress(), ":")[0],
		Port:       "8080",
		Path:       storagePath,
		UpdateTime: time.Now(),
	}); err != nil {
		klog.ErrorS(err, "error to store request response in redis", "requestID", routerCtx.RequestID)
		return buildErrorResponse(envoyTypePb.StatusCode_InternalServerError, err.Error())
	}

	return
}
