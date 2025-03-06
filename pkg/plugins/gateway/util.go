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
	"encoding/json"
	"slices"
	"strings"

	"github.com/aibrix/aibrix/pkg/utils"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"k8s.io/klog/v2"
)

// validateStreamOptions validates whether stream options to include usage is set for user request
func validateStreamOptions(requestID string, user utils.User, jsonMap map[string]interface{}) *extProcPb.ProcessingResponse {
	if user.Tpm != 0 {
		streamOptions, ok := jsonMap["stream_options"].(map[string]interface{})
		if !ok {
			klog.ErrorS(nil, "no stream option available", "requestID", requestID, "jsonMap", jsonMap)
			return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorNoStreamOptions, RawValue: []byte("stream options not set")}}},
				"no stream option available")
		}
		includeUsage, ok := streamOptions["include_usage"].(bool)
		if !includeUsage || !ok {
			klog.ErrorS(nil, "no stream with usage option available", "requestID", requestID, "jsonMap", jsonMap)
			return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorStreamOptionsIncludeUsage, RawValue: []byte("include usage for stream options not set")}}},
				"no stream with usage option available")
		}
	}
	return nil
}

// getRoutingStrategy retrieves the routing strategy from the headers or environment variable
// It returns the routing strategy value and whether custom routing strategy is enabled.
func getRoutingStrategy(headers []*configPb.HeaderValue) (string, bool) {
	var routingStrategy string
	routingStrategyEnabled := false

	// Check headers for routing strategy
	for _, header := range headers {
		if strings.ToLower(header.Key) == HeaderRoutingStrategy {
			routingStrategy = string(header.RawValue)
			routingStrategyEnabled = true
			break // Prioritize header value over environment variable
		}
	}

	// If header not set, check environment variable
	if !routingStrategyEnabled {
		if value, exists := utils.CheckEnvExists(EnvRoutingAlgorithm); exists {
			routingStrategy = value
			routingStrategyEnabled = true
		}
	}

	return routingStrategy, routingStrategyEnabled
}

// validateRoutingStrategy validates if user provided routing strategy is supported by gateway
func validateRoutingStrategy(routingStrategy string) bool {
	routingStrategy = strings.TrimSpace(routingStrategy)
	return slices.Contains(routingStrategies, routingStrategy)
}

// getRequestMessage returns input request message field which has user prompt
func getRequestMessage(jsonMap map[string]interface{}) (string, *extProcPb.ProcessingResponse) {
	messages, ok := jsonMap["messages"]
	if !ok || messages == "" {
		messages, ok = jsonMap["prompt"]
	}

	if !ok {
		return "", generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{Key: HeaderErrorRequestBodyProcessing, RawValue: []byte("true")}}},
			"no messages/prompt in the request body")
	}
	messagesJSON, err := json.Marshal(messages)
	if err != nil {
		return "", generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{Key: HeaderErrorRequestBodyProcessing, RawValue: []byte("true")}}},
			"unable to marshal messages from request body")
	}
	return string(messagesJSON), nil
}

// generateErrorResponse construct envoy proxy error response
func generateErrorResponse(statusCode envoyTypePb.StatusCode, headers []*configPb.HeaderValueOption, body string) *extProcPb.ProcessingResponse {
	// Set the Content-Type header to application/json
	headers = append(headers, &configPb.HeaderValueOption{
		Header: &configPb.HeaderValue{
			Key:   "Content-Type",
			Value: "application/json",
		},
	})

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extProcPb.ImmediateResponse{
				Status: &envoyTypePb.HttpStatus{
					Code: statusCode,
				},
				Headers: &extProcPb.HeaderMutation{
					SetHeaders: headers,
				},
				Body: generateErrorMessage(body, int(statusCode)),
			},
		},
	}
}

// generateErrorMessage constructs a JSON error message
func generateErrorMessage(message string, code int) string {
	errorStruct := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"code":    code,
		},
	}
	jsonData, _ := json.Marshal(errorStruct)
	return string(jsonData)
}
