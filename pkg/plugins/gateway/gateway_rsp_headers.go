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
