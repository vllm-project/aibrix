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
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func (s *Server) HandleRequestHeaders(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, utils.User, int64, types.RoutingAlgorithm, string) {
	var username, requestPath string
	var user utils.User
	var rpm int64
	var err error
	var errRes *extProcPb.ProcessingResponse

	h := req.Request.(*extProcPb.ProcessingRequest_RequestHeaders)
	for _, n := range h.RequestHeaders.Headers.Headers {
		// klog.InfoS("", "key", n.Key, "value", n.RawValue)
		if strings.ToLower(n.Key) == "user" {
			username = string(n.RawValue)
		}
		if strings.ToLower(n.Key) == "x-request-id" {
			requestID = string(n.RawValue)
		}
		if strings.ToLower(n.Key) == ":path" {
			requestPath = string(n.RawValue)
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
			}}}, "incorrect routing strategy"), utils.User{}, rpm, routingAlgorithm, requestPath
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
				err.Error()), utils.User{}, rpm, routingAlgorithm, requestPath
		}

		rpm, errRes, err = s.checkLimits(ctx, user)
		if errRes != nil {
			klog.ErrorS(err, "error on checking limits", "requestID", requestID, "username", username)
			return errRes, utils.User{}, rpm, routingAlgorithm, requestPath
		}
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{
								Header: &configPb.HeaderValue{
									Key:      HeaderWentIntoReqHeaders,
									RawValue: []byte("true"),
								},
							},
						},
					},
					ClearRouteCache: true,
				},
			},
		},
	}, user, rpm, routingAlgorithm, requestPath
}
