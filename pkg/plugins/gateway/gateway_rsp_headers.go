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
	"strconv"
	"strings"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/vllm-project/aibrix/pkg/types"
)

func (s *Server) HandleResponseHeaders(ctx context.Context, requestID string, model string, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, bool, int) {
	b := req.Request.(*extProcPb.ProcessingRequest_ResponseHeaders)
	routerCtx, _ := ctx.(*types.RoutingContext)

	var isProcessingError bool
	var processingErrorCode int
	defer func() {
		if isProcessingError {
			s.cache.DoneRequestCount(routerCtx, requestID, model, 0)
			if routerCtx != nil {
				routerCtx.Delete()
			}
		}
	}()

	headers := []*configPb.HeaderValueOption{}
	headers = buildEnvoyProxyHeaders(headers, HeaderWentIntoReqHeaders, "true", HeaderRequestID, requestID)
	if routerCtx != nil && routerCtx.HasRouted() {
		headers = buildEnvoyProxyHeaders(headers,
			HeaderRoutingStrategy, string(routerCtx.Algorithm),
			HeaderTargetPod, routerCtx.TargetPod().Name,
			HeaderTargetPodIP, routerCtx.TargetAddress())
	}

	if routerCtx != nil && routerCtx.RespHeaders != nil {
		for key, value := range routerCtx.RespHeaders {
			// skip HTTP/2 pseudo-header fields (such as :status, :path, etc.) to avoid protocol errors.
			if strings.HasPrefix(key, ":") {
				continue
			}
			headers = append(headers, &configPb.HeaderValueOption{
				Header: &configPb.HeaderValue{
					Key:      key,
					RawValue: []byte(value),
				},
			})
		}
	}

	for _, headerValue := range b.ResponseHeaders.Headers.Headers {
		if headerValue.Key == ":status" {
			code, _ := strconv.Atoi(string(headerValue.RawValue))
			if code != 200 {
				isProcessingError = true
				processingErrorCode = code
			}
			headers = buildEnvoyProxyHeaders(headers, headerValue.Key, string(headerValue.RawValue))
			break
		}
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					ClearRouteCache: true,
				},
			},
		},
	}, isProcessingError, processingErrorCode
}
