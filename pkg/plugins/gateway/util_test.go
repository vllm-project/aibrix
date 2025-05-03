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

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
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
