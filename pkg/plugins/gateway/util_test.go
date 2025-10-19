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
	"strings"
	"testing"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/openai/openai-go"
	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func Test_ValidateRequestBody(t *testing.T) {
	testCases := []struct {
		message     string
		requestPath string
		requestBody []byte
		model       string
		messages    string
		stream      bool
		user        utils.User
		statusCode  envoyTypePb.StatusCode
	}{
		{
			message:     "unknown path",
			requestPath: "/v1/unknown",
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/chat/completions json unmarhsal error",
			requestPath: "/v1/chat/completions",
			requestBody: []byte("bad_request"),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/chat/completions json unmarhsal ChatCompletionsNewParams",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": 1}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/chat/completions json unmarhsal no messages",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": "llama2-7b"}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/chat/completions json unmarhsal valid messages",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": "llama2-7b", "messages": [{"role": "system", "content": "this is system"},{"role": "user", "content": "say this is test"}]}`),
			model:       "llama2-7b",
			messages:    "this is system say this is test",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/chat/completions json unmarhsal invalid messages with complex content",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": "llama2-7b", "messages": [{"role": "system", "content": "this is system"},{"role": "user", "content": {"type": "text", "text": "say this is test", "complex": make(chan int)}}]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/chat/completions json unmarhsal valid messages with complex content",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": "llama2-7b", "messages": [{"role": "system", "content": "this is system"},{"role": "user", "content": [{"type": "text", "text": "say this is test"}, {"type": "text", "text": "say this is test"}]}]}`),
			model:       "llama2-7b",
			messages:    "this is system [{\"text\":\"say this is test\",\"type\":\"text\"},{\"text\":\"say this is test\",\"type\":\"text\"}]",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/chat/completions json unmarhsal valid messages with stop string param",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": "llama2-7b", "messages": [{"role": "system", "content": "this is system"},{"role": "user", "content": "say this is test"}], "stop": "stop"}`),
			model:       "llama2-7b",
			messages:    "this is system say this is test",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/chat/completions json unmarhsal valid messages with stop array param",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": "llama2-7b", "messages": [{"role": "system", "content": "this is system"},{"role": "user", "content": "say this is test"}], "stop": ["stop"]}`),
			model:       "llama2-7b",
			messages:    "this is system say this is test",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/chat/completions json unmarshal invalid stream bool",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": "llama2-7b", "stream": "true", "messages": [{"role": "system", "content": "this is system"}]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/chat/completions json unmarshal stream options is null",
			requestPath: "/v1/chat/completions",
			user:        utils.User{Tpm: 1},
			requestBody: []byte(`{"model": "llama2-7b", "stream": true, "messages": [{"role": "system", "content": "this is system"}]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/chat/completions stream_options.include_usage == false with user.TPM >= 1 is NOT OK",
			user:        utils.User{Tpm: 1},
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": "llama2-7b", "stream": true, "stream_options": {"include_usage": false},  "messages": [{"role": "system", "content": "this is system"}]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/chat/completions stream_options.include_usage == false with user.TPM == 0 is OK",
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": "llama2-7b", "stream": true, "stream_options": {"include_usage": false},  "messages": [{"role": "system", "content": "this is system"}]}`),
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/chat/completions valid request body",
			user:        utils.User{Tpm: 1},
			requestPath: "/v1/chat/completions",
			requestBody: []byte(`{"model": "llama2-7b", "stream": true, "stream_options": {"include_usage": true}, "messages": [{"role": "system", "content": "this is system"},{"role": "user", "content": "say this is test"}]}`),
			stream:      true,
			model:       "llama2-7b",
			messages:    "this is system say this is test",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
	}

	for _, tt := range testCases {
		model, messages, stream, errRes := validateRequestBody("1", tt.requestPath, tt.requestBody, tt.user)

		if tt.statusCode == 200 {
			assert.Equal(t, (*extProcPb.ProcessingResponse)(nil), errRes, tt.message)
		}
		if tt.statusCode != 200 {
			assert.Equal(t, tt.statusCode, errRes.GetImmediateResponse().Status.Code, tt.message)
		}

		if tt.model != "" {
			assert.Equal(t, tt.model, model, tt.message, tt.message)
		}
		if tt.messages != "" {
			assert.Equal(t, tt.messages, messages, tt.message, tt.message)
		}
		if tt.stream {
			assert.Equal(t, tt.stream, stream, tt.message, tt.message)
		}
	}
}

func Test_ValidateRequestBody_Embeddings(t *testing.T) {
	testCases := []struct {
		message     string
		requestPath string
		requestBody []byte
		model       string
		messages    string
		stream      bool
		user        utils.User
		statusCode  envoyTypePb.StatusCode
	}{
		// Valid embeddings requests
		{
			message:     "/v1/embeddings valid string input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": "Hello world"}`),
			model:       "text-embedding-ada-002",
			messages:    "",
			stream:      false,
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings valid array of strings input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": ["Hello", "world", "test"]}`),
			model:       "text-embedding-ada-002",
			messages:    "",
			stream:      false,
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings valid token array input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": [1, 2, 3, 4, 5]}`),
			model:       "text-embedding-ada-002",
			messages:    "",
			stream:      false,
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings valid multiple token arrays input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": [[1, 2, 3], [4, 5, 6], [7, 8, 9]]}`),
			model:       "text-embedding-ada-002",
			messages:    "",
			stream:      false,
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings with stream false",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": "Hello world", "stream": false}`),
			model:       "text-embedding-ada-002",
			messages:    "",
			stream:      false,
			statusCode:  envoyTypePb.StatusCode_OK,
		},

		// JSON unmarshaling errors
		{
			message:     "/v1/embeddings json unmarshal error - malformed JSON",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": "Hello world"`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		// [bug in openai-go library for unmarshal
		// {
		// 	message:     "/v1/embeddings json unmarshal error - invalid field types",
		// 	requestPath: "/v1/embeddings",
		// 	requestBody: []byte(`{"model": 123, "input": "Hello world"}`),
		// 	statusCode:  envoyTypePb.StatusCode_BadRequest,
		// },
		{
			message:     "/v1/embeddings json unmarshal error - unquoted keys",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{model: "text-embedding-ada-002", input: "Hello world"}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/embeddings json unmarshal error - trailing comma",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": "Hello world",}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},

		// Input validation errors
		{
			message:     "/v1/embeddings empty string input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": ""}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/embeddings empty array input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": []}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/embeddings array with empty string",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": ["Hello", "", "world"]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/embeddings empty token array",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": [[]]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/embeddings string exceeding max tokens",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": "` + strings.Repeat("word ", MaxInputTokensPerModel+1) + `"}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},

		// Stream validation errors
		{
			message:     "/v1/embeddings with stream true - should fail",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": "Hello world", "stream": true}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/embeddings with invalid stream value",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": "Hello world", "stream": "invalid"}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},

		// Edge cases
		{
			message:     "/v1/embeddings minimal valid request",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": "a"}`),
			model:       "text-embedding-ada-002",
			messages:    "",
			stream:      false,
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings with additional valid fields",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": "Hello world", "encoding_format": "float", "dimensions": 1536}`),
			model:       "text-embedding-ada-002",
			messages:    "",
			stream:      false,
			statusCode:  envoyTypePb.StatusCode_OK,
		},
	}

	for _, tt := range testCases {
		model, messages, stream, errRes := validateRequestBody("test-request-id", tt.requestPath, tt.requestBody, tt.user)
		t.Log(tt.message)
		if tt.statusCode == 200 {
			assert.Equal(t, (*extProcPb.ProcessingResponse)(nil), errRes, tt.message)
		}
		if tt.statusCode != 200 {
			assert.Equal(t, tt.statusCode, errRes.GetImmediateResponse().Status.Code, tt.message)
		}

		if tt.model != "" {
			assert.Equal(t, tt.model, model, tt.message)
		}
		if tt.messages != "" {
			assert.Equal(t, tt.messages, messages, tt.message)
		}
		if tt.stream {
			assert.Equal(t, tt.stream, stream, tt.message)
		}
	}
}

func TestValidateEmbeddingInput(t *testing.T) {
	testCases := []struct {
		name        string
		input       openai.EmbeddingNewParams
		expectError bool
		errorMsg    string
	}{
		// String input tests
		{
			name: "valid single string input",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfString: openai.Opt("Hello world"),
				},
			},
			expectError: false,
		},
		{
			name: "empty string input",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfString: openai.Opt(""),
				},
			},
			expectError: true,
			errorMsg:    "input cannot be an empty string",
		},
		{
			name: "string input exceeding max tokens per model",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfString: openai.Opt(strings.Repeat("word ", MaxInputTokensPerModel+1)),
				},
			},
			expectError: true,
			errorMsg:    "input exceeds max tokens per model",
		},

		// Array of strings tests
		{
			name: "valid array of strings",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfStrings: []string{"Hello", "world", "test"},
				},
			},
			expectError: false,
		},
		{
			name: "empty array of strings",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfStrings: []string{},
				},
			},
			expectError: true,
			errorMsg:    "input array cannot be empty",
		},
		{
			name: "array of strings with empty string",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfStrings: []string{"Hello", "", "world"},
				},
			},
			expectError: true,
			errorMsg:    "input at index 1 cannot be an empty string",
		},
		{
			name: "array of strings with one exceeding max tokens",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfStrings: []string{"Hello", strings.Repeat("word ", MaxInputTokensPerModel+1)},
				},
			},
			expectError: true,
			errorMsg:    "input at index 1 exceeds max tokens per model",
		},
		{
			name: "array of strings exceeding total tokens",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfStrings: func() []string {
						// Create an array that would exceed MaxTotalTokens
						largeString := strings.Repeat("word ", MaxTotalTokens/2)
						return []string{largeString, largeString, largeString}
					}(),
				},
			},
			expectError: true,
			errorMsg:    "input at index 0 exceeds max tokens per model (8192)",
		},

		// Single token array tests
		{
			name: "valid single token array",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokens: []int64{1, 2, 3, 4, 5},
				},
			},
			expectError: false,
		},
		{
			name: "empty single token array",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokens: []int64{},
				},
			},
			expectError: true,
			errorMsg:    "token array cannot be empty",
		},
		{
			name: "single token array exceeding max tokens per model",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokens: make([]int64, MaxInputTokensPerModel+1),
				},
			},
			expectError: true,
			errorMsg:    "token array exceeds max tokens per model",
		},
		{
			name: "single token array exceeding max dimensions",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokens: make([]int64, MaxArrayDimensions+1),
				},
			},
			expectError: true,
			errorMsg:    "token array exceeds max dimensions",
		},

		// Multiple token arrays tests
		{
			name: "valid multiple token arrays",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokenArrays: [][]int64{
						{1, 2, 3},
						{4, 5, 6},
						{7, 8, 9},
					},
				},
			},
			expectError: false,
		},
		{
			name: "empty multiple token arrays",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokenArrays: [][]int64{},
				},
			},
			expectError: true,
			errorMsg:    "token arrays cannot be empty",
		},
		{
			name: "multiple token arrays with empty array",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokenArrays: [][]int64{},
				},
			},
			expectError: true,
			errorMsg:    "token arrays cannot be empty",
		},
		{
			name: "multiple token arrays with empty array",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokenArrays: [][]int64{
						{1, 2, 3},
						{},
						{7, 8, 9},
					},
				},
			},
			expectError: true,
			errorMsg:    "token array at index 1 cannot be empty",
		},
		{
			name: "multiple token arrays with one exceeding max tokens per model",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokenArrays: [][]int64{
						{1, 2, 3},
						make([]int64, MaxInputTokensPerModel+1),
					},
				},
			},
			expectError: true,
			errorMsg:    "token array at index 1 exceeds max tokens per model",
		},
		{
			name: "multiple token arrays with one exceeding max dimensions",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokenArrays: [][]int64{
						{1, 2, 3},
						make([]int64, MaxArrayDimensions+1),
					},
				},
			},
			expectError: true,
			errorMsg:    "token array at index 1 exceeds max dimensions",
		},
		{
			name: "multiple token arrays exceeding total tokens",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokenArrays: func() [][]int64 {
						// Create arrays that would exceed MaxTotalTokens
						largeArray := make([]int64, MaxTotalTokens/2)
						return [][]int64{largeArray, largeArray, largeArray}
					}(),
				},
			},
			expectError: true,
			errorMsg:    "token array at index 0 exceeds max tokens per model (8192)",
		},

		// Nil input test
		{
			name: "nil input",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{},
			},
			expectError: false,
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEmbeddingInput(tt.input)

			if tt.expectError {
				assert.Error(t, err, "Expected error for test case: %s", tt.name)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg, "Error message should contain expected text for test case: %s", tt.name)
				}
			} else {
				assert.NoError(t, err, "Expected no error for test case: %s", tt.name)
			}
		})
	}
}

func TestValidateStringInputs(t *testing.T) {
	testCases := []struct {
		name        string
		inputs      []string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "valid single string",
			inputs:      []string{"Hello world"},
			expectError: false,
		},
		{
			name:        "valid multiple strings",
			inputs:      []string{"Hello", "world", "test"},
			expectError: false,
		},
		{
			name:        "empty array",
			inputs:      []string{},
			expectError: true,
			errorMsg:    "input array cannot be empty",
		},
		{
			name:        "single empty string",
			inputs:      []string{""},
			expectError: true,
			errorMsg:    "input cannot be an empty string",
		},
		{
			name:        "multiple strings with empty string",
			inputs:      []string{"Hello", "", "world"},
			expectError: true,
			errorMsg:    "input at index 1 cannot be an empty string",
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStringInputs(tt.inputs)

			if tt.expectError {
				assert.Error(t, err, "Expected error for test case: %s", tt.name)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg, "Error message should contain expected text for test case: %s", tt.name)
				}
			} else {
				assert.NoError(t, err, "Expected no error for test case: %s", tt.name)
			}
		})
	}
}

func TestValidateTokenInputs(t *testing.T) {
	testCases := []struct {
		name        string
		tokenArrays [][]int64
		expectError bool
		errorMsg    string
	}{
		{
			name:        "valid single token array",
			tokenArrays: [][]int64{{1, 2, 3, 4, 5}},
			expectError: false,
		},
		{
			name:        "valid multiple token arrays",
			tokenArrays: [][]int64{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}},
			expectError: false,
		},
		{
			name:        "empty token arrays",
			tokenArrays: [][]int64{},
			expectError: true,
			errorMsg:    "token arrays cannot be empty",
		},
		{
			name:        "single empty token array",
			tokenArrays: [][]int64{{}},
			expectError: true,
			errorMsg:    "token array cannot be empty",
		},
		{
			name:        "multiple token arrays with empty array",
			tokenArrays: [][]int64{{1, 2, 3}, {}, {7, 8, 9}},
			expectError: true,
			errorMsg:    "token array at index 1 cannot be empty",
		},
		{
			name:        "single token array exceeding max tokens per model",
			tokenArrays: [][]int64{make([]int64, MaxInputTokensPerModel+1)},
			expectError: true,
			errorMsg:    "token array exceeds max tokens per model (8192)",
		},
		{
			name:        "multiple token arrays with one exceeding max tokens per model",
			tokenArrays: [][]int64{{1, 2, 3}, make([]int64, MaxInputTokensPerModel+1)},
			expectError: true,
			errorMsg:    "token array at index 1 exceeds max tokens per model",
		},
		{
			name:        "single token array exceeding max dimensions",
			tokenArrays: [][]int64{make([]int64, MaxArrayDimensions+1)},
			expectError: true,
			errorMsg:    "token array exceeds max dimensions (2048)",
		},
		{
			name:        "multiple token arrays with one exceeding max dimensions",
			tokenArrays: [][]int64{{1, 2, 3}, make([]int64, MaxArrayDimensions+1)},
			expectError: true,
			errorMsg:    "token array at index 1 exceeds max dimensions",
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTokenInputs(tt.tokenArrays)

			if tt.expectError {
				assert.Error(t, err, "Expected error for test case: %s", tt.name)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg, "Error message should contain expected text for test case: %s", tt.name)
				}
			} else {
				assert.NoError(t, err, "Expected no error for test case: %s", tt.name)
			}
		})
	}
}

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
			errorType: ErrorTypeAuthentication,
			errorCode: ErrorCodeInvalidAPIKey,
			param:     "api_key",
			wantJSON:  `{"error":{"code":"invalid_api_key","message":"Invalid API key","param":"api_key","type":"authentication_error"}}`,
		},
		{
			name:      "error without code and param (null values)",
			message:   "Server error occurred",
			errorType: ErrorTypeApi,
			errorCode: "",
			param:     "",
			wantJSON:  `{"error":{"code":null,"message":"Server error occurred","param":null,"type":"api_error"}}`,
		},
		{
			name:      "error with code but no param",
			message:   "Model not found",
			errorType: ErrorTypeInvalidRequest,
			errorCode: ErrorCodeModelNotFound,
			param:     "",
			wantJSON:  `{"error":{"code":"model_not_found","message":"Model not found","param":null,"type":"invalid_request_error"}}`,
		},
		{
			name:      "error with param but no code",
			message:   "Invalid parameter value",
			errorType: ErrorTypeInvalidRequest,
			errorCode: "",
			param:     "temperature",
			wantJSON:  `{"error":{"code":null,"message":"Invalid parameter value","param":"temperature","type":"invalid_request_error"}}`,
		},
		{
			name:      "rate limit error",
			message:   "Rate limit exceeded",
			errorType: ErrorTypeRateLimit,
			errorCode: ErrorCodeRateLimitExceeded,
			param:     "",
			wantJSON:  `{"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded","param":null,"type":"rate_limit_error"}}`,
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			result := generateErrorMessage(tt.message, tt.errorType, tt.errorCode, tt.param)
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
			wantType:       ErrorTypeInvalidRequest,
		},
		{
			name:           "401 Unauthorized maps to authentication_error",
			message:        "Invalid API key",
			httpStatusCode: 401,
			errorCode:      ErrorCodeInvalidAPIKey,
			param:          "",
			wantType:       ErrorTypeAuthentication,
		},
		{
			name:           "429 Too Many Requests maps to rate_limit_error",
			message:        "Rate limit exceeded",
			httpStatusCode: 429,
			errorCode:      ErrorCodeRateLimitExceeded,
			param:          "",
			wantType:       ErrorTypeRateLimit,
		},
		{
			name:           "500 Internal Server Error maps to api_error",
			message:        "Internal server error",
			httpStatusCode: 500,
			errorCode:      "",
			param:          "",
			wantType:       ErrorTypeApi,
		},
		{
			name:           "503 Service Unavailable maps to overloaded_error",
			message:        "Service unavailable",
			httpStatusCode: 503,
			errorCode:      ErrorCodeServiceUnavailable,
			param:          "",
			wantType:       ErrorTypeOverloaded,
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			result := generateErrorMessageWithHTTPCode(tt.message, tt.httpStatusCode, tt.errorCode, tt.param)

			// Parse JSON to verify structure
			var errResponse map[string]interface{}
			err := json.Unmarshal([]byte(result), &errResponse)
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
			resp := buildErrorResponse(tt.statusCode, tt.errBody, tt.errorCode, tt.param, tt.headers...)

			assert.NotNil(t, resp, "Response should not be nil")
			assert.NotNil(t, resp.GetImmediateResponse(), "Should have immediate response")
			assert.Equal(t, tt.statusCode, resp.GetImmediateResponse().GetStatus().GetCode(), "Status code should match")

			// Verify error body is valid JSON with correct structure
			body := resp.GetImmediateResponse().GetBody()
			var errResponse map[string]interface{}
			err := json.Unmarshal([]byte(body), &errResponse)
			assert.NoError(t, err, "Response body should be valid JSON")

			errObj, ok := errResponse["error"].(map[string]interface{})
			assert.True(t, ok, "Response should have 'error' object")
			assert.Equal(t, tt.errBody, errObj["message"], "Error message should match")

			// Verify error type is correctly inferred from status code
			var expectedType string
			switch tt.statusCode {
			case envoyTypePb.StatusCode_BadRequest:
				expectedType = ErrorTypeInvalidRequest
			case envoyTypePb.StatusCode_Unauthorized:
				expectedType = ErrorTypeAuthentication
			case envoyTypePb.StatusCode_TooManyRequests:
				expectedType = ErrorTypeRateLimit
			case envoyTypePb.StatusCode_ServiceUnavailable:
				expectedType = ErrorTypeOverloaded
			case envoyTypePb.StatusCode_InternalServerError:
				expectedType = ErrorTypeApi
			default:
				expectedType = ErrorTypeApi
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
