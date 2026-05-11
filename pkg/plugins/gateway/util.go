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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"strings"

	"github.com/bytedance/sonic"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var (
	POD_NAME = os.Getenv("POD_NAME")
)

// chatReqMinimal is a lightweight alternative to openai.ChatCompletionNewParams used
// in validateRequestBody. It avoids the reflection-heavy apijson decoder and gjson
// parsing in the openai SDK by capturing only the fields we actually need.
//
// Stream uses *bool so we can distinguish "field absent" (nil) from "stream: false",
// matching the semantics previously provided by map[string]json.RawMessage.
// Messages are kept as raw JSON to skip the expensive ChatCompletionMessageParamUnion
// unmarshaling; content is extracted in parseChatMessages.
type chatReqMinimal struct {
	Model         string `json:"model"`
	Stream        *bool  `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// embeddingReqMinimal captures the embedding fields needed for validation in a
// single unmarshal pass, including raw stream for strict stream=false checks.
type embeddingReqMinimal struct {
	Model  string                              `json:"model"`
	Input  openai.EmbeddingNewParamsInputUnion `json:"input"`
	Stream json.RawMessage                     `json:"stream"`
}

// parseChatMessages extracts a single concatenated text string from the minimal
// chat request messages. For simple string content it unquotes the JSON string
// directly; for array/object content it writes the raw JSON bytes.
func parseChatMessages(requestID string, msgs []struct {
	Content json.RawMessage `json:"content"`
}) (string, *extProcPb.ProcessingResponse) {
	if len(msgs) == 0 {
		klog.ErrorS(nil, "no messages in the request body", "requestID", requestID)
		return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "no messages in the request body", "", "messages", HeaderErrorRequestBodyProcessing, "true")
	}
	// Pre-grow the builder to avoid repeated internal buffer doublings for large messages.
	// Each Content entry is a raw JSON value; the unescaped string is at most len(Content) bytes.
	var builder strings.Builder
	growHint := len(msgs) - 1 // space separators
	for _, m := range msgs {
		growHint += len(m.Content)
	}
	builder.Grow(growHint)
	for i, m := range msgs {
		if i > 0 {
			builder.WriteByte(' ')
		}
		if len(m.Content) > 0 && m.Content[0] == '"' {
			// Simple string content: JSON-unquote it without allocating an interface.
			var s string
			if err := sonic.Unmarshal(m.Content, &s); err == nil {
				builder.WriteString(s)
				continue
			}
		}
		// Array or object content parts: write raw JSON.
		builder.Write(m.Content)
	}
	return builder.String(), nil
}

// validateRequestBody validates input by unmarshaling request body into respective openai-golang struct based on requestpath.
// nolint:nakedret
func validateRequestBody(requestID, requestPath string, requestBody []byte, user utils.User) (model, message string, stream bool, errRes *extProcPb.ProcessingResponse) {
	switch requestPath {
	case PathChatCompletions, PathMessages:
		// Single-pass minimal unmarshal: avoids the openai SDK's reflection-heavy
		// apijson decoder and gjson parsing, and eliminates the previous redundant
		// map[string]json.RawMessage unmarshal used only for stream-field detection.
		var req chatReqMinimal
		if err := sonic.Unmarshal(requestBody, &req); err != nil {
			klog.ErrorS(err, "error to unmarshal chat completions object", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		model = req.Model
		if message, errRes = parseChatMessages(requestID, req.Messages); errRes != nil {
			return
		}
		if req.Stream != nil {
			stream = *req.Stream
			// stream_options.include_usage is an OpenAI-specific field; Anthropic-style
			// clients hitting /v1/messages will not include it, so skip this check for that path.
			if stream && user.Tpm > 0 && requestPath == PathChatCompletions && !req.StreamOptions.IncludeUsage {
				klog.ErrorS(nil, "no stream with usage option available", "requestID", requestID)
				errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "include usage for stream options not set",
					"", "stream_options", HeaderErrorStreamOptionsIncludeUsage, "include usage for stream options not set")
				return
			}
		}
	case PathCompletions:
		// openai.CompletionsNewParams does not support json unmarshal for CompletionNewParamsPromptUnion in release v0.1.0-beta.10
		// once supported, input request will be directly unmarshal into openai.CompletionsNewParams
		type Completion struct {
			Prompt string `json:"prompt"`
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		completionObj := Completion{}
		err := sonic.Unmarshal(requestBody, &completionObj)
		if err != nil {
			klog.ErrorS(err, "error to unmarshal chat completions object", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		model = completionObj.Model
		message = completionObj.Prompt
		stream = completionObj.Stream
	case PathEmbeddings:
		var embeddingReq embeddingReqMinimal
		if err := sonic.Unmarshal(requestBody, &embeddingReq); err != nil {
			klog.ErrorS(err, "error to unmarshal embeddings object", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		model = embeddingReq.Model
		if err := validateEmbeddingInput(openai.EmbeddingNewParams{
			Model: embeddingReq.Model,
			Input: embeddingReq.Input,
		}); err != nil {
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, err.Error(), "", "input", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		// Preserve behavior: if stream is provided, it must be a valid bool and false.
		if len(embeddingReq.Stream) > 0 {
			var streamBool bool
			if err := sonic.Unmarshal(embeddingReq.Stream, &streamBool); err != nil || streamBool {
				errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "stream not supported for embeddings", "", "stream", HeaderErrorRequestBodyProcessing, "true")
				return
			}
		}
	case PathImagesGenerations, PathVideoGenerations:
		imageGenerationObj := openai.ImageGenerateParams{}
		if err := sonic.Unmarshal(requestBody, &imageGenerationObj); err != nil {
			klog.ErrorS(err, "error to unmarshal image generations object", "requestID", requestID, "requestBody", string(requestBody))
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		model = imageGenerationObj.Model
	case PathRerank:
		type RerankRequest struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
		}
		var req RerankRequest
		if err := sonic.Unmarshal(requestBody, &req); err != nil {
			klog.ErrorS(err, "error to unmarshal rerank object", "requestID", requestID)
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
			return
		}

		if req.Model == "" {
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		if req.Query == "" {
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'query' is a required property", "", "query", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		if len(req.Documents) == 0 {
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'documents' is a required property and cannot be empty", "", "documents", HeaderErrorRequestBodyProcessing, "true")
			return
		}

		model = req.Model
		message = strings.Join(append([]string{req.Query}, req.Documents...), " ")
	case PathClassify:
		model, message, errRes = validateClassifyRequest(requestID, requestBody)
		if errRes != nil {
			return
		}
	case PathAudioTranscriptions, PathAudioTranslations:
		// Audio endpoints require multipart/form-data content-type, not JSON
		// This case handles the error when JSON is sent to audio endpoints
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "audio requests must use multipart/form-data content-type", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	default:
		errRes = buildErrorResponse(envoyTypePb.StatusCode_NotImplemented, "unknown request path", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	klog.V(4).InfoS("validateRequestBody", "requestID", requestID, "requestPath", requestPath, "model", model, "message", message, "stream", stream)
	return
}

// isAudioRequest returns true if the request path is an audio endpoint
func isAudioRequest(requestPath string) bool {
	return requestPath == PathAudioTranscriptions || requestPath == PathAudioTranslations
}

// validateClassifyRequest validates a classify request and returns the model and message.
// nolint:nakedret
func validateClassifyRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	type ClassifyRequest struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	var req ClassifyRequest
	if err := json.Unmarshal(requestBody, &req); err != nil {
		klog.ErrorS(err, "error to unmarshal classify object", "requestID", requestID)
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if req.Model == "" {
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if len(req.Input) == 0 || string(req.Input) == "null" {
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' is a required property", "", "input", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	// Parse input - can be string or array of strings
	var inputStr string
	if err := json.Unmarshal(req.Input, &inputStr); err == nil {
		if inputStr == "" {
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' cannot be an empty string", "", "input", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		message = inputStr
	} else {
		var inputArr []string
		if err := json.Unmarshal(req.Input, &inputArr); err != nil {
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' must be a string or array of strings", "", "input", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		if len(inputArr) == 0 {
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' array cannot be empty", "", "input", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		message = strings.Join(inputArr, " ")
	}

	model = req.Model
	return
}

// isMultipartRequest returns true if the content type indicates multipart form data
func isMultipartRequest(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	return strings.HasPrefix(mediaType, "multipart/")
}

// parseMultipartFormData parses multipart/form-data request body and extracts the model field.
// It returns the model name, stream flag, and any processing error response.
// nolint:nakedret
func parseMultipartFormData(requestID string, contentType string, requestBody []byte) (model string, stream bool, errRes *extProcPb.ProcessingResponse) {
	const trueStr = "true"

	// Extract boundary from Content-Type
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		klog.ErrorS(err, "failed to parse content-type", "requestID", requestID, "contentType", contentType)
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "invalid content-type header", "", "", HeaderErrorMultipartParsing, trueStr)
		return
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "expected multipart/form-data content-type", "", "", HeaderErrorMultipartParsing, trueStr)
		return
	}

	boundary := params["boundary"]
	if boundary == "" {
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "missing boundary in content-type", "", "", HeaderErrorMultipartParsing, trueStr)
		return
	}

	// Parse multipart form
	reader := multipart.NewReader(bytes.NewReader(requestBody), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			klog.ErrorS(err, "failed to read multipart part", "requestID", requestID)
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "failed to parse multipart form", "", "", HeaderErrorMultipartParsing, trueStr)
			return
		}

		fieldName := part.FormName()

		switch fieldName {
		case "model":
			modelBytes, err := io.ReadAll(part)
			if err != nil {
				klog.ErrorS(err, "failed to read model field", "requestID", requestID)
				errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "failed to read model field", "", "model", HeaderErrorMultipartParsing, trueStr)
				return
			}
			model = strings.TrimSpace(string(modelBytes))

		case "stream":
			streamBytes, err := io.ReadAll(part)
			if err == nil {
				streamVal := strings.TrimSpace(strings.ToLower(string(streamBytes)))
				stream = streamVal == trueStr || streamVal == "1"
			}
		}

		_ = part.Close()
	}

	// Validate required model field
	if model == "" {
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorMultipartParsing, trueStr)
		return
	}

	klog.V(4).InfoS("parseMultipartFormData", "requestID", requestID, "model", model, "stream", stream)
	return
}

// validateStreamOptions validates whether stream options to include usage is set for user request
func validateStreamOptions(requestID string, user utils.User, stream *bool, streamOptions openai.ChatCompletionStreamOptionsParam, jsonMap map[string]json.RawMessage) *extProcPb.ProcessingResponse {
	streamData, ok := jsonMap["stream"]
	if !ok {
		return nil
	}

	if err := sonic.Unmarshal(streamData, stream); err != nil {
		klog.ErrorS(nil, "no stream option available", "requestID", requestID)
		return buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "stream incorrectly set", "", "stream", HeaderErrorStream, "stream incorrectly set")
	}

	if *stream && user.Tpm > 0 {
		if !streamOptions.IncludeUsage.Value {
			klog.ErrorS(nil, "no stream with usage option available", "requestID", requestID, "streamOption", streamOptions)
			return buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "include usage for stream options not set",
				"", "stream_options", HeaderErrorStreamOptionsIncludeUsage, "include usage for stream options not set")
		}
	}
	return nil
}

// applyConfigProfile resolves the model config from pod annotation (model.aibrix.ai/config)
// and applies the selected profile: sets ConfigProfile on routingCtx.
// - If the client provides config-profile, use that profile name.
// - If not provided or not found, fall back to defaultProfile (or "default") in the JSON.
func applyConfigProfile(routingCtx *types.RoutingContext, pods []*v1.Pod) {
	headerProfile := routingCtx.ReqConfigProfile
	profile := configprofiles.ResolveProfile(pods, headerProfile)
	if profile == nil {
		return
	}
	routingCtx.ConfigProfile = &types.ResolvedConfigProfile{
		RoutingStrategy:   profile.RoutingStrategy,
		RoutingConfig:     profile.RoutingConfig,
		RequestsPerSecond: profile.RequestsPerSecond,
	}
}

var defaultRoutingStrategy, defaultRoutingStrategyEnabled = utils.LookupEnv(EnvRoutingAlgorithm)

// deriveRoutingStrategyFromContext retrieves routing strategy from headers or resolved profile, falling back to env defaults.
func deriveRoutingStrategyFromContext(routingCtx *types.RoutingContext) (string, bool) {
	// Check request headers (case-insensitive key match)
	if routingCtx != nil && routingCtx.ReqHeaders != nil {
		for k, v := range routingCtx.ReqHeaders {
			if strings.EqualFold(k, HeaderRoutingStrategy) {
				if strings.TrimSpace(v) != "" {
					return v, true
				}
				break
			}
		}
	}
	// Fallback to resolved profile on routing context
	if routingCtx != nil && routingCtx.ConfigProfile != nil {
		s := strings.TrimSpace(routingCtx.ConfigProfile.RoutingStrategy)
		if s != "" {
			return s, true
		}
	}
	// Fallback to environment default
	return defaultRoutingStrategy, defaultRoutingStrategyEnabled
}

// getChatCompletionsMessage returns message for chat completions object
func getChatCompletionsMessage(requestID string, chatCompletionObj openai.ChatCompletionNewParams) (string, *extProcPb.ProcessingResponse) {
	if len(chatCompletionObj.Messages) == 0 {
		klog.ErrorS(nil, "no messages in the request body", "requestID", requestID)
		return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "no messages in the request body", "", "messages", HeaderErrorRequestBodyProcessing, "true")
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
			if jsonBytes, err := sonic.Marshal(content); err == nil {
				builder.Write(jsonBytes)
			} else {
				klog.ErrorS(err, "error marshalling message content", "requestID", requestID, "message", m)
				return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error marshalling message content", "", "messages", HeaderErrorRequestBodyProcessing, "true")
			}
		}
	}
	return builder.String(), nil
}

// generateErrorResponse construct envoy proxy error response
// errorCode and param are optional (pass "" for null)
func generateErrorResponse(statusCode envoyTypePb.StatusCode, headers []*configPb.HeaderValueOption, message, errorCode, param string) *extProcPb.ProcessingResponse {
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
				Body: generateErrorMessageWithHTTPCode(message, int(statusCode), errorCode, param),
			},
		},
	}
}

// generateErrorMessage constructs a JSON error message in OpenAI format
func generateErrorMessage(message, errorType, errorCode, param string) string {
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

// generateErrorMessageWithHTTPCode constructs a JSON error message with appropriate type based on HTTP status code
func generateErrorMessageWithHTTPCode(message string, httpStatusCode int, errorCode, param string) string {
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

	return generateErrorMessage(message, errorType, errorCode, param)
}

// buildErrorResponse constructs an error response with OpenAI-compatible error format
// errorCode and param are optional (pass "" for null)
func buildErrorResponse(statusCode envoyTypePb.StatusCode, errBody, errorCode, param string, headers ...string) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extProcPb.ImmediateResponse{
				Status: &envoyTypePb.HttpStatus{
					Code: statusCode,
				},
				Headers: &extProcPb.HeaderMutation{
					SetHeaders: buildEnvoyProxyHeaders([]*configPb.HeaderValueOption{}, headers...),
				},
				Body: generateErrorMessageWithHTTPCode(errBody, int(statusCode), errorCode, param),
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

func buildGatewayPodMetricLabels(model, status, statusCode string) map[string]string {
	return map[string]string{
		"model":       GetModelTag(model),
		"status":      status,
		"status_code": statusCode,
		"pod_name":    POD_NAME,
	}
}

func GetModelTag(model string) string {
	if model == "" {
		return "unknown"
	}
	return model
}
