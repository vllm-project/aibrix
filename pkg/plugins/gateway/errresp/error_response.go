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

package errresp

import (
	"github.com/bytedance/sonic"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"k8s.io/klog/v2"
)

const (
	// OpenAI Error Types
	ErrorTypeInvalidRequest = "invalid_request_error"
	ErrorTypeAuthentication = "authentication_error"
	ErrorTypeRateLimit      = "rate_limit_error"
	ErrorTypeApi            = "api_error"
	ErrorTypeOverloaded     = "overloaded_error"
)

// GenerateErrorResponse construct envoy proxy error response
// errorCode and param are optional (pass "" for null)
func GenerateErrorResponse(statusCode envoyTypePb.StatusCode, headers []*configPb.HeaderValueOption, message, errorCode, param string) *extProcPb.ProcessingResponse {
	// Set the Content-Type header to application/json
	headers = append(headers, ContentTypeHeader())
	return ImmediateErrorResponse(statusCode, GenerateErrorMessageWithHTTPCode(message, int(statusCode), errorCode, param), headers)
}

// GenerateErrorMessage constructs a JSON error message in OpenAI format
func GenerateErrorMessage(message, errorType, errorCode, param string) string {
	errorStruct := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errorType,
			"code":    nil,
			"param":   nil,
		},
	}

	// Set code if provided (null if empty string)
	if errorCode != "" {
		errorStruct["error"].(map[string]interface{})["code"] = errorCode
	}

	// Set param if provided (null if empty string)
	if param != "" {
		errorStruct["error"].(map[string]interface{})["param"] = param
	}

	jsonData, err := sonic.Marshal(errorStruct)
	if err != nil {
		klog.ErrorS(err, "failed to marshal OpenAI error response")
		return `{"error":{"message":"internal server error while formatting error response","type":"api_error","code":null,"param":null}}`
	}
	return string(jsonData)
}

// GenerateErrorMessageWithHTTPCode constructs a JSON error message with appropriate type based on HTTP status code
func GenerateErrorMessageWithHTTPCode(message string, httpStatusCode int, errorCode, param string) string {
	var errorType string
	switch httpStatusCode {
	case 400, 404:
		errorType = ErrorTypeInvalidRequest
	case 401:
		errorType = ErrorTypeAuthentication
	case 429:
		errorType = ErrorTypeRateLimit
	case 503:
		errorType = ErrorTypeOverloaded
	default:
		errorType = ErrorTypeApi
	}

	return GenerateErrorMessage(message, errorType, errorCode, param)
}

// BuildErrorResponse constructs an error response with OpenAI-compatible error format
// errorCode and param are optional (pass "" for null). Unlike BuildErrorResponseWithBody,
// it builds the body from the supplied fields and does not add a Content-Type header.
func BuildErrorResponse(statusCode envoyTypePb.StatusCode, errBody, errorCode, param string, headers ...string) *extProcPb.ProcessingResponse {
	return BuildErrorResponseWithType(statusCode, errBody, "", errorCode, param, headers...)
}

// BuildErrorResponseWithType is BuildErrorResponse with an explicit OpenAI error.type.
// Empty errorType falls back to the HTTP-status mapping used by GenerateErrorMessageWithHTTPCode.
func BuildErrorResponseWithType(statusCode envoyTypePb.StatusCode, errBody, errorType, errorCode, param string, headers ...string) *extProcPb.ProcessingResponse {
	body := GenerateErrorMessageWithHTTPCode(errBody, int(statusCode), errorCode, param)
	if errorType != "" {
		body = GenerateErrorMessage(errBody, errorType, errorCode, param)
	}
	return ImmediateErrorResponse(
		statusCode,
		body,
		BuildEnvoyProxyHeaders([]*configPb.HeaderValueOption{}, headers...),
	)
}

// BuildErrorResponseWithBody constructs an immediate error response whose body is passed
// through verbatim (already an OpenAI-shaped error payload) with no re-wrapping. The caller
// supplies the full header set it wants to be forwarded (typically the upstream headers plus the
// error header); a Content-Type: application/json header is always prepended.
func BuildErrorResponseWithBody(statusCode envoyTypePb.StatusCode, body string, headers []*configPb.HeaderValueOption) *extProcPb.ProcessingResponse {
	setHeaders := make([]*configPb.HeaderValueOption, 0, 1+len(headers))
	setHeaders = append(setHeaders, ContentTypeHeader())
	setHeaders = append(setHeaders, headers...)
	return ImmediateErrorResponse(statusCode, body, setHeaders)
}

// ContentTypeHeader returns a fresh Content-Type: application/json header.
func ContentTypeHeader() *configPb.HeaderValueOption {
	return &configPb.HeaderValueOption{
		Header: &configPb.HeaderValue{
			Key:   "Content-Type",
			Value: "application/json",
		},
	}
}

// ImmediateErrorResponse builds an envoy ImmediateResponse error with the given status, body,
// and header set. Shared by all gateway error builders so the ImmediateResponse construction
// lives in exactly one place.
func ImmediateErrorResponse(statusCode envoyTypePb.StatusCode, body string, headers []*configPb.HeaderValueOption) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extProcPb.ImmediateResponse{
				Status: &envoyTypePb.HttpStatus{
					Code: statusCode,
				},
				Headers: &extProcPb.HeaderMutation{
					SetHeaders: headers,
				},
				Body: body,
			},
		},
	}
}

func BuildEnvoyProxyHeaders(headers []*configPb.HeaderValueOption, keyValues ...string) []*configPb.HeaderValueOption {
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
