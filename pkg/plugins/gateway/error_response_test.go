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
	"testing"

	"github.com/bytedance/sonic"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/errresp"
)

func TestGenerateErrorMessage(t *testing.T) {
	testCases := []struct {
		name      string
		message   string
		errorType string
		errorCode string
		param     string
		wantJSON  string
	}{
		{
			name:      "error with all fields",
			message:   "Invalid API key",
			errorType: errresp.ErrorTypeAuthentication,
			errorCode: ErrorCodeInvalidAPIKey,
			param:     "api_key",
			wantJSON:  `{"error":{"code":"invalid_api_key","message":"Invalid API key","param":"api_key","type":"authentication_error"}}`,
		},
		{
			name:      "error without code and param (null values)",
			message:   "Server error occurred",
			errorType: errresp.ErrorTypeApi,
			errorCode: "",
			param:     "",
			wantJSON:  `{"error":{"code":null,"message":"Server error occurred","param":null,"type":"api_error"}}`,
		},
		{
			name:      "error with code but no param",
			message:   "Model not found",
			errorType: errresp.ErrorTypeInvalidRequest,
			errorCode: ErrorCodeModelNotFound,
			param:     "",
			wantJSON:  `{"error":{"code":"model_not_found","message":"Model not found","param":null,"type":"invalid_request_error"}}`,
		},
		{
			name:      "error with param but no code",
			message:   "Invalid parameter value",
			errorType: errresp.ErrorTypeInvalidRequest,
			errorCode: "",
			param:     "temperature",
			wantJSON:  `{"error":{"code":null,"message":"Invalid parameter value","param":"temperature","type":"invalid_request_error"}}`,
		},
		{
			name:      "rate limit error",
			message:   "Rate limit exceeded",
			errorType: errresp.ErrorTypeRateLimit,
			errorCode: ErrorCodeRateLimitExceeded,
			param:     "",
			wantJSON:  `{"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded","param":null,"type":"rate_limit_error"}}`,
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			result := errresp.GenerateErrorMessage(tt.message, tt.errorType, tt.errorCode, tt.param)
			assert.JSONEq(t, tt.wantJSON, result, "Error message JSON should match expected format")
		})
	}
}

func TestGenerateErrorMessageWithHTTPCode(t *testing.T) {
	testCases := []struct {
		name           string
		message        string
		httpStatusCode int
		errorCode      string
		param          string
		wantType       string
	}{
		{
			name:           "400 Bad Request maps to invalid_request_error",
			message:        "Missing required parameter",
			httpStatusCode: 400,
			errorCode:      "",
			param:          "model",
			wantType:       errresp.ErrorTypeInvalidRequest,
		},
		{
			name:           "401 Unauthorized maps to authentication_error",
			message:        "Invalid API key",
			httpStatusCode: 401,
			errorCode:      ErrorCodeInvalidAPIKey,
			param:          "",
			wantType:       errresp.ErrorTypeAuthentication,
		},
		{
			name:           "429 Too Many Requests maps to rate_limit_error",
			message:        "Rate limit exceeded",
			httpStatusCode: 429,
			errorCode:      ErrorCodeRateLimitExceeded,
			param:          "",
			wantType:       errresp.ErrorTypeRateLimit,
		},
		{
			name:           "500 Internal Server Error maps to api_error",
			message:        "Internal server error",
			httpStatusCode: 500,
			errorCode:      "",
			param:          "",
			wantType:       errresp.ErrorTypeApi,
		},
		{
			name:           "503 Service Unavailable maps to overloaded_error",
			message:        "Service unavailable",
			httpStatusCode: 503,
			errorCode:      ErrorCodeServiceUnavailable,
			param:          "",
			wantType:       errresp.ErrorTypeOverloaded,
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			result := errresp.GenerateErrorMessageWithHTTPCode(tt.message, tt.httpStatusCode, tt.errorCode, tt.param)

			// Parse JSON to verify structure
			var errResponse map[string]interface{}
			err := sonic.Unmarshal([]byte(result), &errResponse)
			assert.NoError(t, err, "Result should be valid JSON")

			errObj, ok := errResponse["error"].(map[string]interface{})
			assert.True(t, ok, "Response should have 'error' object")

			assert.Equal(t, tt.message, errObj["message"], "Message should match")
			assert.Equal(t, tt.wantType, errObj["type"], "Error type should be correctly mapped from HTTP status code")

			// Verify code field
			if tt.errorCode != "" {
				assert.Equal(t, tt.errorCode, errObj["code"], "Error code should match when provided")
			} else {
				assert.Nil(t, errObj["code"], "Error code should be null when not provided")
			}

			// Verify param field
			if tt.param != "" {
				assert.Equal(t, tt.param, errObj["param"], "Param should match when provided")
			} else {
				assert.Nil(t, errObj["param"], "Param should be null when not provided")
			}
		})
	}
}

func TestBuildErrorResponse(t *testing.T) {
	testCases := []struct {
		name       string
		statusCode envoyTypePb.StatusCode
		errBody    string
		errorCode  string
		param      string
		headers    []string
	}{
		{
			name:       "400 error with model_not_found code",
			statusCode: envoyTypePb.StatusCode_BadRequest,
			errBody:    "Model 'gpt-5' does not exist",
			errorCode:  ErrorCodeModelNotFound,
			param:      "model",
			headers:    []string{"X-Error-Type", "model_not_found"},
		},
		{
			name:       "401 error with invalid_api_key code",
			statusCode: envoyTypePb.StatusCode_Unauthorized,
			errBody:    "Incorrect API key provided",
			errorCode:  ErrorCodeInvalidAPIKey,
			param:      "",
			headers:    []string{},
		},
		{
			name:       "429 rate limit error",
			statusCode: envoyTypePb.StatusCode_TooManyRequests,
			errBody:    "Rate limit exceeded for requests",
			errorCode:  ErrorCodeRateLimitExceeded,
			param:      "",
			headers:    []string{"X-RateLimit-Limit", "100"},
		},
		{
			name:       "503 service unavailable",
			statusCode: envoyTypePb.StatusCode_ServiceUnavailable,
			errBody:    "No available pods for model",
			errorCode:  ErrorCodeServiceUnavailable,
			param:      "",
			headers:    []string{},
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			resp := errresp.BuildErrorResponse(tt.statusCode, tt.errBody, tt.errorCode, tt.param, tt.headers...)

			assert.NotNil(t, resp, "Response should not be nil")
			assert.NotNil(t, resp.GetImmediateResponse(), "Should have immediate response")
			assert.Equal(t, tt.statusCode, resp.GetImmediateResponse().GetStatus().GetCode(), "Status code should match")

			// Verify error body is valid JSON with correct structure
			body := resp.GetImmediateResponse().GetBody()
			var errResponse map[string]interface{}
			err := sonic.Unmarshal([]byte(body), &errResponse)
			assert.NoError(t, err, "Response body should be valid JSON")

			errObj, ok := errResponse["error"].(map[string]interface{})
			assert.True(t, ok, "Response should have 'error' object")
			assert.Equal(t, tt.errBody, errObj["message"], "Error message should match")

			// Verify error type is correctly inferred from status code
			var expectedType string
			switch tt.statusCode {
			case envoyTypePb.StatusCode_BadRequest:
				expectedType = errresp.ErrorTypeInvalidRequest
			case envoyTypePb.StatusCode_Unauthorized:
				expectedType = errresp.ErrorTypeAuthentication
			case envoyTypePb.StatusCode_TooManyRequests:
				expectedType = errresp.ErrorTypeRateLimit
			case envoyTypePb.StatusCode_ServiceUnavailable:
				expectedType = errresp.ErrorTypeOverloaded
			case envoyTypePb.StatusCode_InternalServerError:
				expectedType = errresp.ErrorTypeApi
			default:
				expectedType = errresp.ErrorTypeApi
			}
			assert.Equal(t, expectedType, errObj["type"], "Error type should match status code")

			// Verify code and param
			if tt.errorCode != "" {
				assert.Equal(t, tt.errorCode, errObj["code"], "Error code should match")
			} else {
				assert.Nil(t, errObj["code"], "Error code should be null")
			}

			if tt.param != "" {
				assert.Equal(t, tt.param, errObj["param"], "Param should match")
			} else {
				assert.Nil(t, errObj["param"], "Param should be null")
			}
		})
	}
}
