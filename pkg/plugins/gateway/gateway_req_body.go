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
	"fmt"
	"strconv"
	"strings"

	"k8s.io/klog/v2"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func (s *Server) HandleRequestBody(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, user utils.User) (*extProcPb.ProcessingResponse, string, *types.RoutingContext, bool, int64) {
	var term int64 // Identify the trace window
	routingCtx, _ := ctx.(*types.RoutingContext)

	// Check if this is a continued streaming request - if so, skip path check
	if forwardReq, shouldForward := s.activeForwards.Load(requestID); shouldForward {
		if forwardReq.writer != nil {
			klog.InfoS("continuing streaming forward request", "requestID", requestID, "phase", forwardReq.phase)
			return s.continueForward(ctx, requestID, req, forwardReq), "", routingCtx, false, term
		}

		klog.InfoS("forward request to extend API server", "requestID", requestID, "requestPath", forwardReq.targetURL, "httpMethod", forwardReq.httpMethod)

		// Check if this should use streaming for large file uploads
		body := req.Request.(*extProcPb.ProcessingRequest_RequestBody)
		requestBody := body.RequestBody.GetBody()

		// Use streaming for multipart/form-data (file uploads) or large payloads
		// Check content type rather than body content for multipart detection
		isMultipart := strings.Contains(forwardReq.contentType, "multipart/form-data")
		shouldUseStreaming := len(requestBody) > 1024*1024 || isMultipart // > 1MB or multipart
		return s.forwardRequest(ctx, requestID, req, forwardReq, shouldUseStreaming), "", routingCtx, false, term
	}
	if routingCtx == nil {
		return generateErrorResponse(envoyTypePb.StatusCode_BadRequest,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorUnexpected, RawValue: []byte("true")}}},
			"RoutingContext not available"), "", routingCtx, false, term
	}
	requestPath := routingCtx.ReqPath
	routingAlgorithm := routingCtx.Algorithm

	body := req.Request.(*extProcPb.ProcessingRequest_RequestBody)
	model, message, stream, errRes := validateRequestBody(requestID, requestPath, body.RequestBody.GetBody(), user)
	if errRes != nil {
		return errRes, model, routingCtx, stream, term
	}
	routingCtx.Model = model
	routingCtx.Message = message
	routingCtx.ReqBody = body.RequestBody.GetBody()

	// early reject the request if model doesn't exist.
	if !s.cache.HasModel(model) {
		klog.ErrorS(nil, "model doesn't exist in cache, probably wrong model name", "requestID", requestID, "model", model)
		return generateErrorResponse(envoyTypePb.StatusCode_BadRequest,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorNoModelBackends, RawValue: []byte(model)}}},
			fmt.Sprintf("model %s does not exist", model)), model, routingCtx, stream, term
	}

	// early reject if no pods are ready to accept request for a model
	podsArr, err := s.cache.ListPodsByModel(model)
	if err != nil || podsArr == nil || utils.CountRoutablePods(podsArr.All()) == 0 {
		klog.ErrorS(err, "no ready pod available", "requestID", requestID, "model", model)
		return generateErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorNoModelBackends, RawValue: []byte("true")}}},
			fmt.Sprintf("error on getting pods for model %s", model)), model, routingCtx, stream, term
	}

	headers := []*configPb.HeaderValueOption{}
	if routingAlgorithm == routing.RouterNotSet {
		if err := s.validateHTTPRouteStatus(ctx, model); err != nil {
			return buildErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable, err.Error(), HeaderErrorRouting, "true"), model, routingCtx, stream, term
		}
		headers = buildEnvoyProxyHeaders(headers, HeaderModel, model)
		klog.InfoS("request start", "requestID", requestID, "requestPath", requestPath, "model", model, "stream", stream)
	} else {
		targetPodIP, err := s.selectTargetPod(routingCtx, podsArr)
		if targetPodIP == "" || err != nil {
			klog.ErrorS(err, "failed to select target pod", "requestID", requestID, "routingStrategy", routingAlgorithm, "model", model, "routingDuration", routingCtx.GetRoutingDelay())
			return generateErrorResponse(
				envoyTypePb.StatusCode_ServiceUnavailable,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorRouting, RawValue: []byte("true")}}},
				"error on selecting target pod"), model, routingCtx, stream, term
		}
		headers = buildEnvoyProxyHeaders(headers,
			HeaderRoutingStrategy, string(routingAlgorithm),
			HeaderTargetPod, targetPodIP,
			"content-length", strconv.Itoa(len(routingCtx.ReqBody)),
			"X-Request-Id", routingCtx.RequestID)
		klog.InfoS("request start", "requestID", requestID, "requestPath", requestPath, "model", model, "stream", stream, "routingAlgorithm", routingAlgorithm, "targetPodIP", targetPodIP, "routingDuration", routingCtx.GetRoutingDelay())
	}

	term = s.cache.AddRequestCount(routingCtx, requestID, model)

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					BodyMutation: &extProcPb.BodyMutation{
						Mutation: &extProcPb.BodyMutation_Body{
							Body: routingCtx.ReqBody,
						},
					},
				},
			},
		},
	}, model, routingCtx, stream, term
}
