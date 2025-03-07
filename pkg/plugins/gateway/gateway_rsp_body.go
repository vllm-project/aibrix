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

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/ssestream"
	"k8s.io/klog/v2"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func (s *Server) HandleResponseBody(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, user utils.User, rpm int64, model string, targetPodIP string, stream bool, traceTerm int64, hasCompleted bool) (*extProcPb.ProcessingResponse, bool) {
	b := req.Request.(*extProcPb.ProcessingRequest_ResponseBody)
	klog.InfoS("-- In ResponseBody processing ...", "requestID", requestID, "endOfStream", b.ResponseBody.EndOfStream)

	var res openai.ChatCompletion
	var usage openai.CompletionUsage
	var promptTokens, completionTokens int64
	var headers []*configPb.HeaderValueOption
	complete := hasCompleted

	defer func() {
		// Wrapped in a function to delay the evaluation of parameters. Using complete to make sure DoneRequestTrace only call once for a request.
		if !hasCompleted && complete && b.ResponseBody.EndOfStream {
			s.cache.DoneRequestTrace(requestID, model, promptTokens, completionTokens, traceTerm)
		}
	}()

	if stream {
		t := &http.Response{
			Body: io.NopCloser(bytes.NewReader(b.ResponseBody.GetBody())),
		}
		streaming := ssestream.NewStream[openai.ChatCompletionChunk](ssestream.NewDecoder(t), nil)
		for streaming.Next() {
			evt := streaming.Current()
			if len(evt.Choices) == 0 {
				// Do not overwrite model, res can be empty.
				usage = evt.Usage
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
		// Use request ID as a key to store per-request buffer
		// Retrieve or create buffer
		buf, _ := requestBuffers.LoadOrStore(requestID, &bytes.Buffer{})
		buffer := buf.(*bytes.Buffer)
		// Append data to per-request buffer
		buffer.Write(b.ResponseBody.Body)

		if !b.ResponseBody.EndOfStream {
			// Partial data received, wait for more chunks, we just return a common response here.
			return &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseBody{
					ResponseBody: &extProcPb.BodyResponse{
						Response: &extProcPb.CommonResponse{},
					},
				},
			}, complete
		}

		// Last part received, process the full response
		finalBody := buffer.Bytes()
		// Clean up the buffer after final processing
		requestBuffers.Delete(requestID)

		if err := json.Unmarshal(finalBody, &res); err != nil {
			klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "responseBody", string(b.ResponseBody.GetBody()))
			complete = true
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorResponseUnmarshal, RawValue: []byte("true"),
				}}},
				err.Error()), complete
		} else if len(res.Model) == 0 {
			msg := ErrorUnknownResponse.Error()
			responseBodyContent := string(b.ResponseBody.GetBody())
			if len(responseBodyContent) != 0 {
				msg = responseBodyContent
			}
			klog.ErrorS(err, "unexpected response", "requestID", requestID, "responseBody", responseBodyContent)
			complete = true
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorResponseUnknown, RawValue: []byte("true"),
				}}},
				msg), complete
		}
		// Do not overwrite model, res can be empty.
		usage = res.Usage
	}

	var requestEnd string
	if usage.TotalTokens != 0 {
		complete = true
		// Update promptTokens and completeTokens
		promptTokens = usage.PromptTokens
		completionTokens = usage.CompletionTokens
		// Count token per user.
		if user.Name != "" {
			tpm, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_TPM_CURRENT", user), res.Usage.TotalTokens)
			if err != nil {
				return generateErrorResponse(
					envoyTypePb.StatusCode_InternalServerError,
					[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
						Key: HeaderErrorIncrTPM, RawValue: []byte("true"),
					}}},
					err.Error()), complete
			}

			headers = append(headers,
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      HeaderUpdateRPM,
						RawValue: []byte(fmt.Sprintf("%d", rpm)),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      HeaderUpdateTPM,
						RawValue: []byte(fmt.Sprintf("%d", tpm)),
					},
				},
			)
			requestEnd = fmt.Sprintf(requestEnd+"rpm: %s, tpm: %s, ", rpm, tpm)
		}

		if targetPodIP != "" {
			headers = append(headers,
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      HeaderTargetPod,
						RawValue: []byte(targetPodIP),
					},
				},
			)
			requestEnd = fmt.Sprintf(requestEnd+"targetPod: %s", targetPodIP)
		}

		klog.Infof("request end, requestID: %s - %s", requestID, requestEnd)
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
