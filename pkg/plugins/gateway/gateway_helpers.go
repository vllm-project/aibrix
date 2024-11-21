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
	"slices"
	"strings"

	"github.com/aibrix/aibrix/pkg/utils"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

// getRoutingStrategy retrieves the routing strategy from the headers or environment variable
// It returns the routing strategy value and whether custom routing strategy is enabled.
func getRoutingStrategy(headers []*configPb.HeaderValue) (string, bool) {
	var routingStrategy string
	routingStrategyEnabled := false

	// Check headers for routing strategy
	for _, header := range headers {
		if strings.ToLower(header.Key) == "routing-strategy" {
			routingStrategy = string(header.RawValue)
			routingStrategyEnabled = true
			break // Prioritize header value over environment variable
		}
	}

	// If header not set, check environment variable
	if !routingStrategyEnabled {
		if value, exists := utils.CheckEnvExists("ROUTING_ALGORITHM"); exists {
			routingStrategy = value
			routingStrategyEnabled = true
		}
	}

	return routingStrategy, routingStrategyEnabled
}

// validateRoutingStrategy checks whether routing strategy provided in the request is
// one of the implemented routing algorithms
func validateRoutingStrategy(routingStrategy string) bool {
	routingStrategy = strings.TrimSpace(routingStrategy)
	return slices.Contains(routingStrategies, routingStrategy)
}

func (s *Server) validateUserConfig(ctx context.Context, headers []*configPb.HeaderValue) (utils.User, *extProcPb.ProcessingResponse, error) {
	var username string
	var user utils.User
	var err error
	for _, n := range headers {
		if strings.ToLower(n.Key) == "user" {
			username = string(n.RawValue)
		}
	}

	if username != "" {
		user, err = utils.GetUser(utils.User{Name: username}, s.redisClient)
		if err != nil {
			return user, buildErrorResponse(InternalServerError, err.Error(), "x-error-get-user", "true", "username", username), err
		}

		errRes, err := s.CheckLimits(ctx, user)
		if errRes != nil {
			return user, errRes, err
		}
	}

	return user, nil, nil
}

func buildErrorResponse(statusCode envoyTypePb.StatusCode, errBody string, headers ...string) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extProcPb.ImmediateResponse{
				Status: &envoyTypePb.HttpStatus{
					Code: statusCode,
				},
				Headers: &extProcPb.HeaderMutation{
					SetHeaders: buildEnvoyProxyHeaders([]*configPb.HeaderValueOption{}, headers...),
				},
				Body: errBody,
			},
		},
	}
}

func buildEnvoyProxyHeaders(headers []*configPb.HeaderValueOption, keyValues ...string) []*configPb.HeaderValueOption {
	if len(keyValues)%2 != 0 {
		return headers
	}

	for i := 0; i < len(keyValues); {
		headers = append(headers,
			&configPb.HeaderValueOption{
				Header: &configPb.HeaderValue{
					Key:      keyValues[i],
					RawValue: []byte(keyValues[i+1]),
				},
			},
		)
		i += 2
	}

	return headers
}
