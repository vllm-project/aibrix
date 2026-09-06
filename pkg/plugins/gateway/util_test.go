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
	"testing"
	"time"

	"github.com/bytedance/sonic"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"go.opentelemetry.io/otel/attribute"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func int64Ptr(v int64) *int64 {
	return &v
}

const autoConfigProfile = "auto"

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
			// Unknown paths return 501 Not Implemented. Previously the outer JSON unmarshal
			// ran before the switch and accidentally returned 400 for empty bodies; now the
			// switch default correctly returns 501.
			message:     "unknown path",
			requestPath: "/v1/unknown",
			statusCode:  envoyTypePb.StatusCode_NotImplemented,
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
			// parseChatMessages writes raw JSON bytes directly, preserving the original field order from the request.
			messages:   "this is system [{\"type\": \"text\", \"text\": \"say this is test\"}, {\"type\": \"text\", \"text\": \"say this is test\"}]",
			statusCode: envoyTypePb.StatusCode_OK,
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
		{
			message:     "/v1/messages valid request body (same as chat completions)",
			user:        utils.User{Tpm: 1},
			requestPath: "/v1/messages",
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
		// Long inputs are not rejected here: the gateway cannot measure them in
		// the target model's tokens, so the backend decides. This holds whether
		// or not the client asked the backend to truncate, and the three cases
		// below must not collapse into an identical gateway-side 400.
		{
			message:     "/v1/embeddings long string is forwarded, no truncate_prompt_tokens",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "input": "` + strings.Repeat("word ", 9000) + `"}`),
			model:       "text-embedding-ada-002",
			messages:    "",
			stream:      false,
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings long string is forwarded with truncate_prompt_tokens -1",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "truncate_prompt_tokens": -1, "input": "` + strings.Repeat("word ", 9000) + `"}`),
			model:       "text-embedding-ada-002",
			messages:    "",
			stream:      false,
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings long string is forwarded with explicit truncate_prompt_tokens",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "text-embedding-ada-002", "truncate_prompt_tokens": 400, "input": "` + strings.Repeat("word ", 9000) + `"}`),
			model:       "text-embedding-ada-002",
			messages:    "",
			stream:      false,
			statusCode:  envoyTypePb.StatusCode_OK,
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

		// Multimodal content-parts input (e.g. image_url for vision-language embedding models)
		{
			message:     "/v1/embeddings multimodal image_url input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}}]}`),
			model:       "qwen3-vl-embedding-8b",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings multimodal text and image_url input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{"type": "text", "text": "a photo of pets"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}}]}`),
			model:       "qwen3-vl-embedding-8b",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings multimodal input with stream true - should fail",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}}], "stream": true}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/embeddings multimodal input missing image_url",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{"type": "image_url"}]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/embeddings multimodal video_url input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{"type": "video_url", "video_url": {"url": "data:video/mp4;base64,aGVsbG8="}}]}`),
			model:       "qwen3-vl-embedding-8b",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings multimodal input missing video_url",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{"type": "video_url"}]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/embeddings multimodal input unsupported content type",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{"type": "audio_url"}]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},

		// sglang's flat MultimodalEmbeddingInput shape (no "type" discriminator):
		// {"text": "..."} / {"image": "<data-uri>"} / {"video": "<data-uri>"}
		{
			message:     "/v1/embeddings flat image input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{"image": "data:image/png;base64,aGVsbG8="}]}`),
			model:       "qwen3-vl-embedding-8b",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings flat video input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{"video": "data:video/mp4;base64,aGVsbG8="}]}`),
			model:       "qwen3-vl-embedding-8b",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings flat text and image input",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{"text": "a photo of pets"}, {"image": "data:image/png;base64,aGVsbG8="}]}`),
			model:       "qwen3-vl-embedding-8b",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/embeddings flat input part with nothing set",
			requestPath: "/v1/embeddings",
			requestBody: []byte(`{"model": "qwen3-vl-embedding-8b", "input": [{}]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
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

func Test_ValidateRequestBody_Rerank(t *testing.T) {
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
			message:     "/v1/rerank valid request",
			requestPath: "/v1/rerank",
			requestBody: []byte(`{"model": "bge-reranker-base", "query": "what is panda?", "documents": ["hi", "panda is a bear"]}`),
			model:       "bge-reranker-base",
			messages:    "what is panda? hi panda is a bear",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/rerank missing model",
			requestPath: "/v1/rerank",
			requestBody: []byte(`{"query": "what is panda?", "documents": ["hi", "panda is a bear"]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/rerank missing query",
			requestPath: "/v1/rerank",
			requestBody: []byte(`{"model": "bge-reranker-base", "documents": ["hi", "panda is a bear"]}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/rerank missing documents",
			requestPath: "/v1/rerank",
			requestBody: []byte(`{"model": "bge-reranker-base", "query": "what is panda?"}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/rerank empty documents",
			requestPath: "/v1/rerank",
			requestBody: []byte(`{"model": "bge-reranker-base", "query": "what is panda?", "documents": []}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/rerank invalid json",
			requestPath: "/v1/rerank",
			requestBody: []byte(`{"model": "bge-reranker-base", "query": "what is panda?", "documents": ["hi", "panda is a bear"]`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
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
			// The gateway does not enforce a context length; the backend does,
			// with the addressed model's own tokenizer.
			name: "long string input is accepted",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfString: openai.Opt(strings.Repeat("word ", 9000)),
				},
			},
			expectError: false,
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
			name: "array of strings with one long entry is accepted",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfStrings: []string{"Hello", strings.Repeat("word ", 9000)},
				},
			},
			expectError: false,
		},
		{
			name: "array of strings with a large total length is accepted",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfStrings: func() []string {
						largeString := strings.Repeat("word ", 9000)
						return []string{largeString, largeString, largeString}
					}(),
				},
			},
			expectError: false,
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
			// A long pre-tokenized input is accepted, exactly as the same
			// content sent as a string would be. MaxArrayDimensions bounds the
			// batch, not the token count of one input.
			name: "long single token array is accepted",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokens: make([]int64, MaxArrayDimensions+1),
				},
			},
			expectError: false,
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
			name: "batch with one long token array is accepted",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokenArrays: [][]int64{
						{1, 2, 3},
						make([]int64, MaxArrayDimensions+1),
					},
				},
			},
			expectError: false,
		},
		{
			name: "multiple token arrays with a large total length is accepted",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokenArrays: func() [][]int64 {
						// Each array is within MaxArrayDimensions; only the sum
						// is large, which is no longer capped.
						largeArray := make([]int64, MaxArrayDimensions)
						return [][]int64{largeArray, largeArray, largeArray}
					}(),
				},
			},
			expectError: false,
		},

		// Batch size is still bounded: MaxArrayDimensions is OpenAI's maxItems
		// for the input array, which the gateway can measure exactly.
		{
			name: "array of strings exceeding max batch size",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfStrings: func() []string {
						inputs := make([]string, MaxArrayDimensions+1)
						for i := range inputs {
							inputs[i] = "hello"
						}
						return inputs
					}(),
				},
			},
			expectError: true,
			errorMsg:    "input array exceeds max dimensions (2048)",
		},
		{
			name: "multiple token arrays exceeding max batch size",
			input: openai.EmbeddingNewParams{
				Input: openai.EmbeddingNewParamsInputUnion{
					OfArrayOfTokenArrays: func() [][]int64 {
						arrays := make([][]int64, MaxArrayDimensions+1)
						for i := range arrays {
							arrays[i] = []int64{1, 2, 3}
						}
						return arrays
					}(),
				},
			},
			expectError: true,
			errorMsg:    "input array exceeds max dimensions (2048)",
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
			// Length is the backend's call, not the gateway's: the gateway has
			// no access to the target model's tokenizer or context length.
			name:        "very long single string",
			inputs:      []string{strings.Repeat("word ", 100000)},
			expectError: false,
		},
		{
			name:        "non-ASCII input that cl100k over-counts",
			inputs:      []string{strings.Repeat("Příliš žluťoučký kůň úpěl ďábelské ódy. ", 2000)},
			expectError: false,
		},
		{
			name: "array exceeding max batch size",
			inputs: func() []string {
				inputs := make([]string, MaxArrayDimensions+1)
				for i := range inputs {
					inputs[i] = "hello"
				}
				return inputs
			}(),
			expectError: true,
			errorMsg:    "input array exceeds max dimensions (2048)",
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
			// Token count per input is not capped: that is a context-length
			// question, and the same content sent as a string is accepted.
			name:        "long token arrays are accepted",
			tokenArrays: [][]int64{make([]int64, MaxArrayDimensions+1), make([]int64, MaxArrayDimensions+1)},
			expectError: false,
		},
		{
			// Batch size is capped, per OpenAI's maxItems for the input array.
			name: "batch exceeding max dimensions",
			tokenArrays: func() [][]int64 {
				arrays := make([][]int64, MaxArrayDimensions+1)
				for i := range arrays {
					arrays[i] = []int64{1, 2, 3}
				}
				return arrays
			}(),
			expectError: true,
			errorMsg:    "input array exceeds max dimensions (2048)",
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
			resp := buildErrorResponse(tt.statusCode, tt.errBody, tt.errorCode, tt.param, tt.headers...)

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

func Test_ValidateRequestBody_Classify(t *testing.T) {
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
			message:     "/v1/classify valid string input",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"model": "classifier-model", "input": "text to classify"}`),
			model:       "classifier-model",
			messages:    "text to classify",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/classify valid array input",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"model": "classifier-model", "input": ["text1", "text2"]}`),
			model:       "classifier-model",
			messages:    "text1 text2",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/classify missing model",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"input": "text to classify"}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/classify empty model",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"model": "", "input": "text to classify"}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/classify missing input",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"model": "classifier-model"}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/classify null input",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"model": "classifier-model", "input": null}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/classify empty string input",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"model": "classifier-model", "input": ""}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/classify empty array input",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"model": "classifier-model", "input": []}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/classify invalid input type (number)",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"model": "classifier-model", "input": 123}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/classify invalid input type (object)",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"model": "classifier-model", "input": {"key": "value"}}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/classify invalid json",
			requestPath: "/v1/classify",
			requestBody: []byte(`{"model": "classifier-model", "input": "text"`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
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

func Test_ValidateRequestBody_Responses(t *testing.T) {
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
			message:     "/v1/responses valid string input",
			requestPath: "/v1/responses",
			requestBody: []byte(`{"model": "llama2-7b", "input": "say this is test"}`),
			model:       "llama2-7b",
			messages:    "say this is test",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/responses valid array input with string content",
			requestPath: "/v1/responses",
			requestBody: []byte(`{"model": "llama2-7b", "input": [{"role": "system", "content": "this is system"},{"role": "user", "content": "say this is test"}]}`),
			model:       "llama2-7b",
			messages:    "this is system say this is test",
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/responses valid array input with content parts",
			requestPath: "/v1/responses",
			requestBody: []byte(`{"model": "llama2-7b", "input": [{"role": "user", "content": [{"type": "input_text", "text": "say this is test"}]}]}`),
			model:       "llama2-7b",
			messages:    `[{"type": "input_text", "text": "say this is test"}]`,
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/responses valid streaming request",
			requestPath: "/v1/responses",
			user:        utils.User{Tpm: 1},
			requestBody: []byte(`{"model": "llama2-7b", "stream": true, "input": "say this is test"}`),
			model:       "llama2-7b",
			messages:    "say this is test",
			stream:      true,
			statusCode:  envoyTypePb.StatusCode_OK,
		},
		{
			message:     "/v1/responses json unmarshal error",
			requestPath: "/v1/responses",
			requestBody: []byte("bad_request"),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/responses missing input",
			requestPath: "/v1/responses",
			requestBody: []byte(`{"model": "llama2-7b"}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/responses null input",
			requestPath: "/v1/responses",
			requestBody: []byte(`{"model": "llama2-7b", "input": null}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/responses empty array input",
			requestPath: "/v1/responses",
			requestBody: []byte(`{"model": "llama2-7b", "input": []}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
		},
		{
			message:     "/v1/responses invalid input type (number)",
			requestPath: "/v1/responses",
			requestBody: []byte(`{"model": "llama2-7b", "input": 123}`),
			statusCode:  envoyTypePb.StatusCode_BadRequest,
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

func TestGetTraceID(t *testing.T) {
	tests := []struct {
		name        string
		traceparent string
		requestID   string
		want        string
	}{
		{
			name:        "Valid W3C traceparent",
			traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			requestID:   "req-id-12345",
			want:        "4bf92f3577b34da6a3ce929d0e0e4736",
		},
		{
			name:        "Valid traceparent with leading/trailing spaces",
			traceparent: "   00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01   ",
			requestID:   "req-id-12345",
			want:        "4bf92f3577b34da6a3ce929d0e0e4736",
		},
		{
			name:        "Empty traceparent",
			traceparent: "",
			requestID:   "fallback-req-id",
			want:        "fallback-req-id",
		},
		{
			name:        "Whitespace only traceparent",
			traceparent: "     ",
			requestID:   "fallback-req-id",
			want:        "fallback-req-id",
		},
		{
			name:        "Invalid format: missing parts",
			traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736", // 只有两部分，少于4部分
			requestID:   "fallback-req-id",
			want:        "fallback-req-id",
		},
		{
			name:        "Invalid format: trace ID length not 32",
			traceparent: "00-shortid-00f067aa0ba902b7-01", // ID长度不对
			requestID:   "fallback-req-id",
			want:        "fallback-req-id",
		},
		{
			name:        "Invalid format: completely random string",
			traceparent: "just-a-random-string-without-proper-format",
			requestID:   "fallback-req-id",
			want:        "fallback-req-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetTraceID(tt.traceparent, tt.requestID)
			if got != tt.want {
				t.Errorf("GetTraceID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFieldsToAttributes(t *testing.T) {
	tests := []struct {
		name   string
		fields []interface{}
		want   []attribute.KeyValue
	}{
		{
			name: "supported and fallback value types",
			fields: []interface{}{
				"string", "value",
				"int", 7,
				"int64", int64(8),
				"float64", 1.5,
				"duration", 1500 * time.Millisecond,
				"fallback", true,
				99, "numeric key",
			},
			want: []attribute.KeyValue{
				attribute.String("string", "value"),
				attribute.Int("int", 7),
				attribute.Int64("int64", 8),
				attribute.Float64("float64", 1.5),
				attribute.String("duration", "1.5s"),
				attribute.String("fallback", "true"),
				attribute.String("99", "numeric key"),
			},
		},
		{
			name:   "empty fields",
			fields: nil,
			want:   []attribute.KeyValue{},
		},
		{
			name:   "incomplete key value pair is ignored",
			fields: []interface{}{"complete", "value", "orphan"},
			want: []attribute.KeyValue{
				attribute.String("complete", "value"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, fieldsToAttributes(tt.fields))
		})
	}
}

func TestApplyConfigProfile_AutoProfileRoutingConfigHints(t *testing.T) {
	profileJSON := `{
		"defaultProfile":"default",
		"profiles":{
			"default":{"routingStrategy":"least-request"},
			"large-input":{"routingStrategy":"pd","routingConfig":{"promptTokensGte":4},"requestsPerSecond":5},
			"offline-generation":{"routingStrategy":"throughput","routingConfig":{"maxTokensGte":2048}}
		}
	}`
	pods := []*v1.Pod{{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod-a",
			Annotations: map[string]string{constants.ModelAnnoConfig: profileJSON},
		},
	}}

	ctx := types.NewRoutingContext(context.Background(), "", "", "", "request-1", "")
	ctx.ReqConfigProfile = autoConfigProfile
	ctx.Message = "one two three four"
	ctx.ReqBody = []byte(`{"max_tokens":128}`)
	ctx.ReqHeaders = map[string]string{}

	applyConfigProfile(ctx, pods)

	assert.NotNil(t, ctx.ConfigProfile)
	assert.Equal(t, "pd", ctx.ConfigProfile.RoutingStrategy)
	assert.Equal(t, int64(5), ctx.ConfigProfile.RequestsPerSecond)
	assert.Equal(t, "large-input", ctx.ReqConfigProfile)
	assert.Equal(t, "large-input", ctx.RespHeaders[HeaderAIBrixConfigProfile])
}

func TestApplyConfigProfile_AutoProfileRoutingConfigHintFallbacks(t *testing.T) {
	tests := []struct {
		name             string
		profileJSON      string
		reqHeaders       map[string]string
		reqBody          []byte
		wantStrategy     string
		wantReqProfile   string
		wantProfileHdr   string
		wantHeaderAbsent bool
	}{
		{
			name: "no matching hint uses default profile",
			profileJSON: `{
				"defaultProfile":"default",
				"profiles":{"default":{"routingStrategy":"least-request"},"throughput":{"routingStrategy":"throughput","routingConfig":{"maxTokensGte":2048}}}
			}`,
			reqHeaders:     map[string]string{},
			reqBody:        []byte(`{"max_tokens":128}`),
			wantStrategy:   "least-request",
			wantReqProfile: "default",
			wantProfileHdr: "default",
		},
		{
			name: "invalid routingConfig hint uses default profile",
			profileJSON: `{
				"defaultProfile":"default",
				"profiles":{"default":{"routingStrategy":"least-request"},"large-input":{"routingStrategy":"pd","routingConfig":"invalid"}}
			}`,
			reqHeaders:     map[string]string{},
			reqBody:        []byte(`{"max_tokens":128}`),
			wantStrategy:   "least-request",
			wantReqProfile: "default",
			wantProfileHdr: "default",
		},
		{
			name: "routing strategy header does not skip auto selection",
			profileJSON: `{
				"defaultProfile":"default",
				"profiles":{"default":{"routingStrategy":"least-request"},"large-input":{"routingStrategy":"pd","routingConfig":{"promptTokensGte":1},"requestsPerSecond":5}}
			}`,
			reqHeaders:     map[string]string{HeaderRoutingStrategy: "throughput"},
			reqBody:        []byte(`{"max_tokens":128}`),
			wantStrategy:   "pd",
			wantReqProfile: "large-input",
			wantProfileHdr: "large-input",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pods := []*v1.Pod{{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "pod-a",
					Annotations: map[string]string{constants.ModelAnnoConfig: tt.profileJSON},
				},
			}}
			ctx := types.NewRoutingContext(context.Background(), "", "", "", "request-1", "")
			ctx.ReqConfigProfile = autoConfigProfile
			ctx.Message = "prompt"
			ctx.ReqBody = tt.reqBody
			ctx.ReqHeaders = tt.reqHeaders

			applyConfigProfile(ctx, pods)

			assert.NotNil(t, ctx.ConfigProfile)
			assert.Equal(t, tt.wantStrategy, ctx.ConfigProfile.RoutingStrategy)
			assert.Equal(t, tt.wantReqProfile, ctx.ReqConfigProfile)
			if tt.wantHeaderAbsent {
				assert.NotContains(t, ctx.RespHeaders, HeaderAIBrixConfigProfile)
			} else {
				assert.Equal(t, tt.wantProfileHdr, ctx.RespHeaders[HeaderAIBrixConfigProfile])
			}
		})
	}
}

func TestApplyConfigProfile_BuildsFeaturesOnlyForAutoSelection(t *testing.T) {
	profileJSON := `{
		"defaultProfile":"default",
		"profiles":{
			"default":{"routingStrategy":"least-request"},
			"batch":{"routingStrategy":"throughput","routingConfig":{"promptTokensGte":1}}
		}
	}`
	pods := []*v1.Pod{{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod-a",
			Annotations: map[string]string{constants.ModelAnnoConfig: profileJSON},
		},
	}}

	tests := []struct {
		name             string
		reqConfigProfile string
		reqHeaders       map[string]string
		wantStrategy     string
		wantReqProfile   string
	}{
		{
			name:             "concrete profile skips request feature parsing",
			reqConfigProfile: "batch",
			reqHeaders:       map[string]string{},
			wantStrategy:     "throughput",
			wantReqProfile:   "batch",
		},
		{
			name:             "routing strategy header still resolves auto profile",
			reqConfigProfile: autoConfigProfile,
			reqHeaders:       map[string]string{HeaderRoutingStrategy: "throughput"},
			wantStrategy:     "throughput",
			wantReqProfile:   "batch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), "", "", "", "request-1", "")
			ctx.ReqConfigProfile = tt.reqConfigProfile
			ctx.ReqHeaders = tt.reqHeaders
			ctx.ReqBody = []byte(`{`)
			ctx.Message = "prompt"

			applyConfigProfile(ctx, pods)

			assert.NotNil(t, ctx.ConfigProfile)
			assert.Equal(t, tt.wantStrategy, ctx.ConfigProfile.RoutingStrategy)
			assert.Equal(t, tt.wantReqProfile, ctx.ReqConfigProfile)
			if tt.reqConfigProfile == autoConfigProfile {
				assert.Equal(t, tt.wantReqProfile, ctx.RespHeaders[HeaderAIBrixConfigProfile])
			} else {
				assert.NotContains(t, ctx.RespHeaders, HeaderAIBrixConfigProfile)
			}
		})
	}
}

func TestMaxTokensFromRequestBody(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want *int64
	}{
		{name: "max tokens", body: []byte(`{"max_tokens":128}`), want: int64Ptr(128)},
		{name: "max completion tokens", body: []byte(`{"max_completion_tokens":256}`), want: int64Ptr(256)},
		{name: "max tokens wins", body: []byte(`{"max_tokens":128,"max_completion_tokens":256}`), want: int64Ptr(128)},
		{name: "absent", body: []byte(`{"model":"m"}`), want: nil},
		{name: "invalid", body: []byte(`{`), want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maxTokensFromRequestBody(tt.body)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			if assert.NotNil(t, got) {
				assert.Equal(t, *tt.want, *got)
			}
		})
	}
}

func TestDeriveRoutingStrategyFromContext(t *testing.T) {
	// lockedRoutingStrategy (model-wide) always wins over header and profile.
	lockedCtx := &types.RoutingContext{
		ConfigProfile: &types.ResolvedConfigProfile{
			LockedRoutingStrategy: "pd",
			RoutingStrategy:       "random",
		},
		ReqHeaders: map[string]string{HeaderRoutingStrategy: "throughput"},
	}

	// Header wins over profile strategy.
	headerCtx := &types.RoutingContext{
		ConfigProfile: &types.ResolvedConfigProfile{RoutingStrategy: "random"},
		ReqHeaders:    map[string]string{HeaderRoutingStrategy: "throughput"},
	}

	// Profile strategy is used when no header is present.
	profileCtx := &types.RoutingContext{
		ConfigProfile: &types.ResolvedConfigProfile{RoutingStrategy: "least-request"},
		ReqHeaders:    map[string]string{},
	}

	// Case-insensitive header key match.
	headerCaseCtx := &types.RoutingContext{
		ConfigProfile: &types.ResolvedConfigProfile{RoutingStrategy: "random"},
		ReqHeaders:    map[string]string{"Routing-Strategy": "pd"},
	}

	// Nothing set: falls back to environment default.
	emptyCtx := &types.RoutingContext{ReqHeaders: map[string]string{}}

	tests := []struct {
		name   string
		ctx    *types.RoutingContext
		want   string
		wantOK bool
	}{
		{"locked strategy wins over header and profile", lockedCtx, "pd", true},
		{"header wins over profile strategy", headerCtx, "throughput", true},
		{"profile strategy used when header absent", profileCtx, "least-request", true},
		{"case-insensitive header key", headerCaseCtx, "pd", true},
		{"falls back to env when nothing set", emptyCtx, defaultRoutingStrategy, defaultRoutingStrategyEnabled},
		{"nil context falls back to env", nil, defaultRoutingStrategy, defaultRoutingStrategyEnabled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := deriveRoutingStrategyFromContext(tt.ctx)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}
