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
	"errors"
	"fmt"
	"strings"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	// https://github.com/openai/openai-go/blob/main/embedding.go#L126
	MaxInputTokensPerModel = 8192
	MaxTotalTokens         = 300000
	MaxArrayDimensions     = 2048
)

// validateRequestBody validates input by unmarshaling request body into respective openai-golang struct based on requestpath.
// nolint:nakedret
func validateRequestBody(requestID, requestPath string, requestBody []byte, user utils.User) (model, message string, stream bool, errRes *extProcPb.ProcessingResponse) {
	var streamOptions openai.ChatCompletionStreamOptionsParam
	var jsonMap map[string]json.RawMessage
	if err := json.Unmarshal(requestBody, &jsonMap); err != nil {
		klog.ErrorS(err, "error to unmarshal request body", "requestID", requestID, "requestBody", string(requestBody))
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	switch requestPath {
	case "/v1/chat/completions":
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
	case "/v1/completions":
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
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		model = completionObj.Model
		message = completionObj.Prompt
	case "/v1/embeddings":
		embeddingObj := openai.EmbeddingNewParams{}
		if err := json.Unmarshal(requestBody, &embeddingObj); err != nil {
			klog.ErrorS(err, "error to unmarshal embeddings object", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		model = embeddingObj.Model
		if err := validateEmbeddingInput(embeddingObj); err != nil {
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, err.Error(), HeaderErrorRequestBodyProcessing, "true")
			return
		}
		streamVal, ok := jsonMap["stream"]
		if ok {
			var streamBool bool
			if err := json.Unmarshal(streamVal, &streamBool); err != nil || streamBool {
				errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "stream not supported for embeddings", HeaderErrorRequestBodyProcessing, "true")
				return
			}
		}
	case "/v1/image/generations", "/v1/video/generations":
		imageGenerationObj := openai.ImageGenerateParams{}
		if err := json.Unmarshal(requestBody, &imageGenerationObj); err != nil {
			klog.ErrorS(err, "error to unmarshal image generations object", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		model = imageGenerationObj.Model
	default:
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
				Body: generateErrorMessage(errBody, int(statusCode)),
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

// validateEmbeddingInput validates the input according to OpenAI embedding constraints
func validateEmbeddingInput(embeddingObj openai.EmbeddingNewParams) error {
	inputParam := embeddingObj.Input
	switch input := embeddingNewParamsInputUnionAsAny(&inputParam).(type) {
	case *string:
		return validateStringInputs([]string{*input})
	case *[]string:
		return validateStringInputs(*input)
	case *[]int64:
		return validateTokenInputs([][]int64{*input})
	case *[][]int64:
		return validateTokenInputs(*input)
	default:
		if input != nil {
			return fmt.Errorf("input must be a string, []string, []int64, or [][]int64, got %T", input)
		}
		return nil
	}
}

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

// validateStringInputs validates string inputs (both single string and array of strings)
func validateStringInputs(inputs []string) error {
	if len(inputs) == 0 {
		return errors.New("input array cannot be empty")
	}

	totalEstimatedTokens := 0

	for i, input := range inputs {
		if input == "" {
			if len(inputs) == 1 {
				return errors.New("input cannot be an empty string")
			}
			return fmt.Errorf("input at index %d cannot be an empty string", i)
		}

		tokens, err := utils.TokenizeInputText(input)
		if err != nil {
			return fmt.Errorf("failed to tokenize input for validation: %w", err)
		}
		estimatedTokens := len(tokens)
		if estimatedTokens > MaxInputTokensPerModel {
			if len(inputs) == 1 {
				return fmt.Errorf("input exceeds max tokens per model (%d), estimated tokens: %d",
					MaxInputTokensPerModel, estimatedTokens)
			}
			return fmt.Errorf("input at index %d exceeds max tokens per model (%d), estimated tokens: %d",
				i, MaxInputTokensPerModel, estimatedTokens)
		}

		totalEstimatedTokens += estimatedTokens
	}

	if totalEstimatedTokens > MaxTotalTokens {
		return fmt.Errorf("total tokens across all inputs exceeds maximum (%d), estimated total: %d",
			MaxTotalTokens, totalEstimatedTokens)
	}

	return nil
}

// validateTokenInputs validates token inputs (both single token array and multiple token arrays)
func validateTokenInputs(tokenArrays [][]int64) error {
	if len(tokenArrays) == 0 {
		return errors.New("token arrays cannot be empty")
	}

	totalTokens := 0

	for i, tokens := range tokenArrays {
		if len(tokens) == 0 {
			if len(tokenArrays) == 1 {
				return errors.New("token array cannot be empty")
			}
			return fmt.Errorf("token array at index %d cannot be empty", i)
		}

		if len(tokens) > MaxInputTokensPerModel {
			if len(tokenArrays) == 1 {
				return fmt.Errorf("token array exceeds max tokens per model (%d), actual tokens: %d",
					MaxInputTokensPerModel, len(tokens))
			}
			return fmt.Errorf("token array at index %d exceeds max tokens per model (%d), actual tokens: %d",
				i, MaxInputTokensPerModel, len(tokens))
		}

		if len(tokens) > MaxArrayDimensions {
			if len(tokenArrays) == 1 {
				return fmt.Errorf("token array exceeds max dimensions (%d), actual dimensions: %d",
					MaxArrayDimensions, len(tokens))
			}
			return fmt.Errorf("token array at index %d exceeds max dimensions (%d), actual dimensions: %d",
				i, MaxArrayDimensions, len(tokens))
		}

		totalTokens += len(tokens)
	}

	if totalTokens > MaxTotalTokens {
		return fmt.Errorf("total tokens across all inputs exceeds maximum (%d), actual total: %d",
			MaxTotalTokens, totalTokens)
	}

	return nil
}
