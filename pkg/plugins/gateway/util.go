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

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

// validateRequestBody validates input by unmarshaling request body into respective openai-golang struct based on requestpath.
// nolint:nakedret
func validateRequestBody(requestID, requestPath string, requestBody []byte, user utils.User) (model, message string, stream bool, errRes *extProcPb.ProcessingResponse) {
	var streamOptions openai.ChatCompletionStreamOptionsParam
	if requestPath == "/v1/chat/completions" {
		var jsonMap map[string]json.RawMessage
		if err := json.Unmarshal(requestBody, &jsonMap); err != nil {
			klog.ErrorS(err, "error to unmarshal request body", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", HeaderErrorRequestBodyProcessing, "true")
			return
		}

		chatCompletionObj := openai.ChatCompletionNewParams{}
		if err := json.Unmarshal(requestBody, &chatCompletionObj); err != nil {
			klog.ErrorS(err, "error to unmarshal chat completions object", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		model, streamOptions = chatCompletionObj.Model, chatCompletionObj.StreamOptions
		if message, errRes = getChatCompletionsMessage(requestID, chatCompletionObj); errRes != nil {
			return
		}
		if errRes = validateStreamOptions(requestID, user, &stream, streamOptions, jsonMap); errRes != nil {
			return
		}
	} else if requestPath == "/v1/completions" {
		// openai.CompletionsNewParams does not support json unmarshal for CompletionNewParamsPromptUnion in release v0.1.0-beta.10
		// once supported, input request will be directly unmarshal into openai.CompletionsNewParams
		type Completion struct {
			Prompt string `json:"prompt"`
			Model  string `json:"model"`
		}
		completionObj := Completion{}
		err := json.Unmarshal(requestBody, &completionObj)
		if err != nil {
			klog.ErrorS(err, "error to unmarshal chat completions object", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_InternalServerError, "error processing request body", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		model = completionObj.Model
		message = completionObj.Prompt
	} else if requestPath == "/v1/embeddings" {
		message = "" // prefix_cache algorithms are not relevant for embeddings
		var jsonMap map[string]json.RawMessage
		if err := json.Unmarshal(requestBody, &jsonMap); err != nil {
			klog.ErrorS(err, "error to unmarshal request body", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		embeddingObj := openai.EmbeddingNewParams{}
		if err := json.Unmarshal(requestBody, &embeddingObj); err != nil {
			klog.ErrorS(err, "error to unmarshal embeddings object", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		model = embeddingObj.Model
		if errRes = checkEmbeddingInputSequenceLen(requestID, embeddingObj); errRes != nil {
			return
		}
	} else {
		errRes = buildErrorResponse(envoyTypePb.StatusCode_NotImplemented, "unknown request path", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	klog.V(4).InfoS("validateRequestBody", "requestID", requestID, "requestPath", requestPath, "model", model, "message", message, "stream", stream, "streamOptions", streamOptions)
	return
}

// validateStreamOptions validates whether stream options to include usage is set for user request
func validateStreamOptions(requestID string, user utils.User, stream *bool, streamOptions openai.ChatCompletionStreamOptionsParam, jsonMap map[string]json.RawMessage) *extProcPb.ProcessingResponse {
	streamData, ok := jsonMap["stream"]
	if !ok {
		return nil
	}

	if err := json.Unmarshal(streamData, stream); err != nil {
		klog.ErrorS(nil, "no stream option available", "requestID", requestID)
		return buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "stream incorrectly set", HeaderErrorStream, "stream incorrectly set")
	}

	if *stream && user.Tpm > 0 {
		if !streamOptions.IncludeUsage.Value {
			klog.ErrorS(nil, "no stream with usage option available", "requestID", requestID, "streamOption", streamOptions)
			return buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "include usage for stream options not set",
				HeaderErrorStreamOptionsIncludeUsage, "include usage for stream options not set")
		}
	}
	return nil
}

var defaultRoutingStrategy, defaultRoutingStrategyEnabled = utils.LookupEnv(EnvRoutingAlgorithm)

// getRoutingStrategy retrieves the routing strategy from the headers or environment variable
// It returns the routing strategy value and whether custom routing strategy is enabled.
func getRoutingStrategy(headers []*configPb.HeaderValue) (string, bool) {
	// Check headers for routing strategy
	for _, header := range headers {
		if strings.ToLower(header.Key) == HeaderRoutingStrategy {
			return string(header.RawValue), true
		}
	}

	// If header not set, use default routing strategy from environment variable
	return defaultRoutingStrategy, defaultRoutingStrategyEnabled
}

// getChatCompletionsMessage returns message for chat completions object
func getChatCompletionsMessage(requestID string, chatCompletionObj openai.ChatCompletionNewParams) (string, *extProcPb.ProcessingResponse) {
	if len(chatCompletionObj.Messages) == 0 {
		klog.ErrorS(nil, "no messages in the request body", "requestID", requestID)
		return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "no messages in the request body", HeaderErrorRequestBodyProcessing, "true")
	}
	var builder strings.Builder
	for i, m := range chatCompletionObj.Messages {
		if i > 0 {
			builder.WriteString(" ")
		}
		switch content := m.GetContent().AsAny().(type) {
		case *string:
			builder.WriteString(*content)
		default:
			if jsonBytes, err := json.Marshal(content); err == nil {
				builder.Write(jsonBytes)
			} else {
				klog.ErrorS(err, "error marshalling message content", "requestID", requestID, "message", m)
				return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error marshalling message content", HeaderErrorRequestBodyProcessing, "true")
			}
		}
	}
	return builder.String(), nil
}

// getEmbeddingsInputLen returns the len of the embeddings object
func checkEmbeddingInputSequenceLen(requestID string, embeddingObj openai.EmbeddingNewParams) *extProcPb.ProcessingResponse {
	inputParam := embeddingObj.Input
	var size int
	switch input := embeddingNewParamsInputUnionAsAny(&inputParam).(type) {
	case *string:
		size = len(*input)
	case *[]string:
		size = len(*input)
	case *[]int64:
		size = len(*input)
	case *[][]int64:
		size = len(*input)
	default:
	}

	if size == 0 {
		klog.ErrorS(nil, "no input in the request body", "requestID", requestID)
		return buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "no messages in the request body", HeaderErrorRequestBodyProcessing, "true")
	}
	if size > 1024 {
		klog.ErrorS(nil, "embeddings content is too large", "requestID", requestID, "size", size)
		return buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "embeddings content is too large", HeaderErrorRequestBodyProcessing, "true")
	}

	return nil
}

// TODO: make asAny method publicly available on OpenAI go
func embeddingNewParamsInputUnionAsAny(u *openai.EmbeddingNewParamsInputUnion) any {
	if !param.IsOmitted(u.OfString) {
		return &u.OfString.Value
	} else if !param.IsOmitted(u.OfArrayOfStrings) {
		return &u.OfArrayOfStrings
	} else if !param.IsOmitted(u.OfArrayOfTokens) {
		return &u.OfArrayOfTokens
	} else if !param.IsOmitted(u.OfArrayOfTokenArrays) {
		return &u.OfArrayOfTokenArrays
	}
	return nil
}

// generateErrorResponse construct envoy proxy error response
// deprecated: use buildErrorResponse
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
