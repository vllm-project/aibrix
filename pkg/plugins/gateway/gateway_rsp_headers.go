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

	"k8s.io/klog/v2"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

func (s *Server) HandleResponseHeaders(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, targetPodIP string) (*extProcPb.ProcessingResponse, bool, int) {
	klog.InfoS("-- In ResponseHeaders processing ...", "requestID", requestID)
	b := req.Request.(*extProcPb.ProcessingRequest_ResponseHeaders)

	headers := []*configPb.HeaderValueOption{{
		Header: &configPb.HeaderValue{
			Key:      HeaderWentIntoReqHeaders,
			RawValue: []byte("true"),
		},
	}}
	if targetPodIP != "" {
		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      HeaderTargetPod,
				RawValue: []byte(targetPodIP),
			},
		})
	}

	var isProcessingError bool
	var processingErrorCode int
	for _, headerValue := range b.ResponseHeaders.Headers.Headers {
		if headerValue.Key == ":status" {
			code, _ := strconv.Atoi(string(headerValue.RawValue))
			if code != 200 {
				isProcessingError = true
				processingErrorCode = code
			}
		}
		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      headerValue.Key,
				RawValue: headerValue.RawValue,
			},
		})
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
