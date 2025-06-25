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

	"k8s.io/klog/v2"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func (s *Server) HandleRequestBody(ctx context.Context, requestID string, requestPath string, req *extProcPb.ProcessingRequest,
	user utils.User, routingAlgorithm types.RoutingAlgorithm) (*extProcPb.ProcessingResponse, string, *types.RoutingContext, bool, int64) {
	var routingCtx *types.RoutingContext
	var term int64 // Identify the trace window

	body := req.Request.(*extProcPb.ProcessingRequest_RequestBody)
	model, message, stream, errRes := validateRequestBody(requestID, requestPath, body.RequestBody.GetBody(), user)
	if errRes != nil {
		return errRes, model, routingCtx, stream, term
	}

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

	routingCtx = types.NewRoutingContext(ctx, routingAlgorithm, model, message, requestID, user.Name)
	headers := []*configPb.HeaderValueOption{}
	if routingAlgorithm == routing.RouterNotSet {
		headers = buildEnvoyProxyHeaders(headers, HeaderModel, model)
		klog.InfoS("request start", "requestID", requestID, "requestPath", requestPath, "model", model, "stream", stream)
	} else {
		targetPodIP, err := s.selectTargetPod(routingCtx, podsArr)
		if targetPodIP == "" || err != nil {
			klog.ErrorS(err, "failed to select target pod", "requestID", requestID, "routingAlgorithm", routingAlgorithm, "model", model)
			return generateErrorResponse(
				envoyTypePb.StatusCode_ServiceUnavailable,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorRouting, RawValue: []byte("true")}}},
				"error on selecting target pod"), model, routingCtx, stream, term
		}
		headers = buildEnvoyProxyHeaders(headers,
			HeaderRoutingStrategy, string(routingAlgorithm),
			HeaderTargetPod, targetPodIP)
		klog.InfoS("request start", "requestID", requestID, "requestPath", requestPath, "model", model, "stream", stream, "routingAlgorithm", routingAlgorithm, "targetPodIP", targetPodIP)
	}

	term = s.cache.AddRequestCount(routingCtx, requestID, model)

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
				},
			},
		},
	}, model, routingCtx, stream, term
}
