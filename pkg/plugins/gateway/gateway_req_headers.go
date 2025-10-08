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
	"strings"

	"k8s.io/klog/v2"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcFilterPb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func (s *Server) HandleRequestHeaders(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, utils.User, int64, *types.RoutingContext) {
	var username, requestPath string
	var user utils.User
	var rpm int64
	var err error
	var errRes *extProcPb.ProcessingResponse
	var routingCtx *types.RoutingContext

	h := req.Request.(*extProcPb.ProcessingRequest_RequestHeaders)
	reqHeaders := map[string]string{}
	for _, n := range h.RequestHeaders.Headers.Headers {
		if strings.ToLower(n.Key) == "user" {
			username = string(n.RawValue)
		}
		if strings.ToLower(n.Key) == ":path" {
			requestPath = string(n.RawValue)
		}
		if strings.ToLower(n.Key) == "authorization" {
			reqHeaders[n.Key] = string(n.RawValue)
		}
	}

	routingStrategy, routingStrategyEnabled := getRoutingStrategy(h.RequestHeaders.Headers.Headers)
	routingAlgorithm, ok := routing.Validate(routingStrategy)
	if routingStrategyEnabled && !ok {
		klog.ErrorS(nil, "incorrect routing strategy", "requestID", requestID, "routing-strategy", routingStrategy)
		return generateErrorResponse(
			envoyTypePb.StatusCode_BadRequest,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorInvalidRouting, RawValue: []byte(routingStrategy),
			}}}, "incorrect routing strategy"), utils.User{}, rpm, routingCtx
	}

	if username != "" {
		user, err = utils.GetUser(ctx, utils.User{Name: username}, s.redisClient)
		if err != nil {
			klog.ErrorS(err, "unable to process user info", "requestID", requestID, "username", username)
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorUser, RawValue: []byte("true"),
				}}},
				err.Error()), utils.User{}, rpm, routingCtx
		}

		rpm, errRes, err = s.checkLimits(ctx, user)
		if errRes != nil {
			klog.ErrorS(err, "error on checking limits", "requestID", requestID, "username", username)
			return errRes, utils.User{}, rpm, routingCtx
		}
	}

	routingCtx = types.NewRoutingContext(ctx, routingAlgorithm, "", "", requestID, user.Name)
	routingCtx.ReqPath = requestPath
	routingCtx.ReqHeaders = reqHeaders

	// For legacy Process function (non-state machine), we need to handle routing here
	// For state machine version, this function is only used to extract headers info
	if s.useLegacyMode {
		// Legacy mode: complete routing in RequestHeaders phase
		klog.InfoS("legacy mode: completing processing in RequestHeaders phase", "requestID", requestID)

		// early reject the request if model doesn't exist.
		model := "default" // TODO: Extract model from headers if available
		if !s.cache.HasModel(model) {
			klog.ErrorS(nil, "model doesn't exist in cache", "requestID", requestID, "model", model)
			return generateErrorResponse(envoyTypePb.StatusCode_BadRequest,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorNoModelBackends, RawValue: []byte(model)}}},
				"model does not exist"), user, rpm, routingCtx
		}

		// early reject if no pods are ready to accept request for a model
		podsArr, err := s.cache.ListPodsByModel(model)
		if err != nil || podsArr == nil || utils.CountRoutablePods(podsArr.All()) == 0 {
			klog.ErrorS(err, "no ready pod available", "requestID", requestID, "model", model)
			return generateErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorNoModelBackends, RawValue: []byte("true")}}},
				"no ready pods available"), user, rpm, routingCtx
		}

		headers := []*configPb.HeaderValueOption{}
		if routingAlgorithm == routing.RouterNotSet {
			if err := s.validateHTTPRouteStatus(ctx, model); err != nil {
				return buildErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable, err.Error(), HeaderErrorRouting, "true"), user, rpm, routingCtx
			}
			headers = buildEnvoyProxyHeaders(headers, HeaderModel, model)
			klog.InfoS("request start", "requestID", requestID, "requestPath", routingCtx.ReqPath, "model", model)
		} else {
			targetPodIP, err := s.selectTargetPod(routingCtx, podsArr)
			if targetPodIP == "" || err != nil {
				klog.ErrorS(err, "failed to select target pod", "requestID", requestID, "routingStrategy", routingAlgorithm, "model", model, "routingDuration", routingCtx.GetRoutingDelay())
				return generateErrorResponse(
					envoyTypePb.StatusCode_ServiceUnavailable,
					[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
						Key: HeaderErrorRouting, RawValue: []byte("true")}}},
					"error on selecting target pod"), user, rpm, routingCtx
			}
			headers = buildEnvoyProxyHeaders(headers,
				HeaderRoutingStrategy, string(routingAlgorithm),
				HeaderTargetPod, targetPodIP,
				"X-Request-Id", routingCtx.RequestID)
			klog.InfoS("request start", "requestID", requestID, "requestPath", routingCtx.ReqPath, "routingAlgorithm", routingAlgorithm, "targetPodIP", targetPodIP, "routingDuration", routingCtx.GetRoutingDelay())
		}

		return &extProcPb.ProcessingResponse{
			Response: &extProcPb.ProcessingResponse_RequestHeaders{
				RequestHeaders: &extProcPb.HeadersResponse{
					Response: &extProcPb.CommonResponse{
						HeaderMutation: &extProcPb.HeaderMutation{
							SetHeaders: headers,
						},
						ClearRouteCache: true,
					},
				},
			},
			// Don't request RequestBody in legacy mode
			ModeOverride: &extProcFilterPb.ProcessingMode{
				RequestBodyMode: extProcFilterPb.ProcessingMode_NONE,
			},
		}, user, rpm, routingCtx
	}

	// State machine mode: just return basic info, don't do routing here
	klog.InfoS("state machine mode: headers processed, waiting for body", "requestID", requestID)
	return nil, user, rpm, routingCtx
}
