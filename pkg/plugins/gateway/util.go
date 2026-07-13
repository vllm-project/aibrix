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
	Messages []contentItem `json:"messages"`
}

// responsesReqMinimal captures the fields needed to route and validate an OpenAI
// Responses API (/v1/responses) request. Input is kept as raw JSON because it may
// be either a plain string or an array of input items; it is parsed lazily in
// parseResponsesInput. Stream uses *bool to distinguish "absent" from "stream: false".
type responsesReqMinimal struct {
	Model  string          `json:"model"`
	Stream *bool           `json:"stream"`
	Input  json.RawMessage `json:"input"`
}

// contentItem holds the raw JSON "content" field of a chat message or a Responses API
// input item. It is shared by chatReqMinimal.Messages, parseChatMessages, and
// parseResponsesInput.
type contentItem struct {
	Content json.RawMessage `json:"content"`
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
func parseChatMessages(requestID string, msgs []contentItem) (string, *extProcPb.ProcessingResponse) {
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

// parseResponsesInput extracts a single concatenated text string from a Responses
// API "input" field for routing purposes. The field is either a plain string or an
// array of input items whose "content" is itself a string or an array of content
// parts; in all cases we reuse the same text-extraction strategy as chat messages.
func parseResponsesInput(requestID string, input json.RawMessage) (string, *extProcPb.ProcessingResponse) {
	if len(input) == 0 || string(input) == "null" {
		klog.ErrorS(nil, "no input in the request body", "requestID", requestID)
		return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' is a required property", "", "input", HeaderErrorRequestBodyProcessing, "true")
	}
	// Plain string input: JSON-unquote it directly.
	if input[0] == '"' {
		var s string
		if err := sonic.Unmarshal(input, &s); err != nil {
			klog.ErrorS(err, "error to unmarshal responses input string", "requestID", requestID)
			return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "input", HeaderErrorRequestBodyProcessing, "true")
		}
		return s, nil
	}
	// Array of input items: each item may carry a "content" field (string or array
	// of content parts). Items without content (e.g. tool/function outputs) simply
	// contribute nothing to the routing key.
	var items []contentItem
	if err := sonic.Unmarshal(input, &items); err != nil {
		klog.ErrorS(err, "error to unmarshal responses input array", "requestID", requestID)
		return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' must be a string or an array of input items", "", "input", HeaderErrorRequestBodyProcessing, "true")
	}
	if len(items) == 0 {
		klog.ErrorS(nil, "empty input array in the request body", "requestID", requestID)
		return "", buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' array cannot be empty", "", "input", HeaderErrorRequestBodyProcessing, "true")
	}
	return parseChatMessages(requestID, items)
}

// validateRequestBody validates input by unmarshaling request body into respective openai-golang struct based on requestpath.
// The per-path parsing is delegated to dedicated validate* helpers to keep this dispatcher simple.
func validateRequestBody(requestID, requestPath string, requestBody []byte, user utils.User) (model, message string, stream bool, errRes *extProcPb.ProcessingResponse) {
	switch requestPath {
	case PathChatCompletions, PathMessages:
		model, message, stream, errRes = validateChatRequest(requestID, requestPath, requestBody, user)
	case PathResponses:
		model, message, stream, errRes = validateResponsesRequest(requestID, requestBody)
	case PathCompletions:
		model, message, stream, errRes = validateCompletionRequest(requestID, requestBody)
	case PathEmbeddings:
		model, errRes = validateEmbeddingRequest(requestID, requestBody)
	case PathImagesGenerations, PathVideoGenerations:
		model, errRes = validateImageGenerationRequest(requestID, requestBody)
	case PathRerank:
		model, message, errRes = validateRerankRequest(requestID, requestBody)
	case PathClassify:
		model, message, errRes = validateClassifyRequest(requestID, requestBody)
	case PathAudioTranscriptions, PathAudioTranslations:
		// Audio endpoints require multipart/form-data content-type, not JSON
		// This case handles the error when JSON is sent to audio endpoints
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "audio requests must use multipart/form-data content-type", "", "", HeaderErrorRequestBodyProcessing, "true")
	default:
		errRes = buildErrorResponse(envoyTypePb.StatusCode_NotImplemented, "unknown request path", "", "", HeaderErrorRequestBodyProcessing, "true")
	}
	if errRes != nil {
		return
	}

	klog.V(4).InfoS("validateRequestBody", "requestID", requestID, "requestPath", requestPath, "model", model, "message", message, "stream", stream)
	return
}

// validateChatRequest parses and validates a chat completions (or Anthropic-style messages) request body.
// nolint:nakedret
func validateChatRequest(requestID, requestPath string, requestBody []byte, user utils.User) (model, message string, stream bool, errRes *extProcPb.ProcessingResponse) {
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
	return
}

// validateResponsesRequest parses and validates an OpenAI Responses API (/v1/responses) request body.
// nolint:nakedret
func validateResponsesRequest(requestID string, requestBody []byte) (model, message string, stream bool, errRes *extProcPb.ProcessingResponse) {
	// OpenAI Responses API. Unlike chat completions, the Responses API always
	// emits usage in the terminal streaming event, so there is no stream_options
	// .include_usage requirement to enforce for TPM-limited users.
	var req responsesReqMinimal
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		klog.ErrorS(err, "error to unmarshal responses object", "requestID", requestID, "requestBody", string(requestBody))
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	// Per the OpenAI Responses API spec, "model" is a required property.
	if req.Model == "" {
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	model = req.Model
	if message, errRes = parseResponsesInput(requestID, req.Input); errRes != nil {
		return
	}
	if req.Stream != nil {
		stream = *req.Stream
	}
	return
}

// validateCompletionRequest parses and validates a legacy completions request body.
// nolint:nakedret
func validateCompletionRequest(requestID string, requestBody []byte) (model, message string, stream bool, errRes *extProcPb.ProcessingResponse) {
	// openai.CompletionsNewParams does not support json unmarshal for CompletionNewParamsPromptUnion in release v0.1.0-beta.10
	// once supported, input request will be directly unmarshal into openai.CompletionsNewParams
	type Completion struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	completionObj := Completion{}
	if err := sonic.Unmarshal(requestBody, &completionObj); err != nil {
		klog.ErrorS(err, "error to unmarshal chat completions object", "requestID", requestID, "requestBody", string(requestBody))
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	model = completionObj.Model
	message = completionObj.Prompt
	stream = completionObj.Stream
	return
}

// embeddingReqRaw is a fallback parse target for embeddings requests whose `input` uses
// multimodal content parts (e.g. image_url for vision-language embedding models), which
// openai.EmbeddingNewParamsInputUnion does not support -- unlike chat completions, the
// OpenAI embeddings spec has no content-parts shape, so backends that accept images this
// way (e.g. qwen3-vl-embedding) are an extension. Input is kept raw so it can be
// re-parsed once the shape is known.
type embeddingReqRaw struct {
	Model  string          `json:"model"`
	Input  json.RawMessage `json:"input"`
	Stream json.RawMessage `json:"stream"`
}

// embeddingContentPart supports two multimodal `input` part shapes:
//   - the OpenAI chat-content-part shape: {"type": ..., "text": ..., "image_url": {...}}
//   - sglang's flat MultimodalEmbeddingInput shape (no "type" discriminator):
//     {"text": "..."} / {"image": "<data-uri>"} / {"video": "<data-uri>"}
//
// sglang's embeddings endpoint only understands the flat shape -- it silently ignores
// unrecognized fields (extra="ignore"), so sending the OpenAI content-part shape leaves
// text/image/video all unset and falls back to a fixed placeholder input.
type embeddingContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
	VideoURL json.RawMessage `json:"video_url"`
	Image    string          `json:"image"`
	Video    string          `json:"video"`
}

// validateEmbeddingContentParts validates a multimodal embeddings `input` array. Only
// "text" parts are tokenized and counted against MaxInputTokensPerModel / MaxTotalTokens;
// image/video parts are excluded because the gateway's text tokenizer cannot estimate
// their cost, and the backend's own multimodal processor is responsible for that
// accounting.
func validateEmbeddingContentParts(parts []embeddingContentPart) error {
	if len(parts) == 0 {
		return errors.New("input array cannot be empty")
	}

	totalEstimatedTokens := 0
	for i, part := range parts {
		typ := part.Type
		if typ == "" {
			// Flat sglang shape: infer the part kind from whichever field is set.
			switch {
			case part.Text != "":
				typ = "text"
			case part.Image != "":
				typ = "image"
			case part.Video != "":
				typ = "video"
			default:
				return fmt.Errorf("input at index %d must set one of text, image, or video", i)
			}
		}

		switch typ {
		case "text":
			if part.Text == "" {
				return fmt.Errorf("input at index %d cannot be an empty string", i)
			}
			tokens, err := utils.TokenizeInputText(part.Text)
			if err != nil {
				return fmt.Errorf("failed to tokenize input for validation: %w", err)
			}
			estimatedTokens := len(tokens)
			if estimatedTokens > MaxInputTokensPerModel {
				return fmt.Errorf("input at index %d exceeds max tokens per model (%d), estimated tokens: %d",
					i, MaxInputTokensPerModel, estimatedTokens)
			}
			totalEstimatedTokens += estimatedTokens
		case "image_url":
			if len(part.ImageURL) == 0 {
				return fmt.Errorf("input at index %d is missing image_url", i)
			}
		case "image":
			if part.Image == "" {
				return fmt.Errorf("input at index %d is missing image", i)
			}
		case "video_url":
			if len(part.VideoURL) == 0 {
				return fmt.Errorf("input at index %d is missing video_url", i)
			}
		case "video":
			if part.Video == "" {
				return fmt.Errorf("input at index %d is missing video", i)
			}
		default:
			return fmt.Errorf("input at index %d has unsupported content type %q", i, typ)
		}
	}

	if totalEstimatedTokens > MaxTotalTokens {
		return fmt.Errorf("total tokens across all inputs exceeds maximum (%d), estimated total: %d",
			MaxTotalTokens, totalEstimatedTokens)
	}

	return nil
}

// validateMultimodalEmbeddingRequest re-parses an embeddings request body whose `input`
// didn't fit openai.EmbeddingNewParamsInputUnion, checking whether it's instead an array
// of multimodal content parts. matched is false if the body doesn't match that shape
// either, signaling the caller to fall back to the original parse error.
func validateMultimodalEmbeddingRequest(requestID string, requestBody []byte) (model string, errRes *extProcPb.ProcessingResponse, matched bool) {
	var req embeddingReqRaw
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		return "", nil, false
	}

	trimmed := bytes.TrimSpace(req.Input)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return "", nil, false
	}

	var parts []embeddingContentPart
	if err := sonic.Unmarshal(trimmed, &parts); err != nil || len(parts) == 0 {
		return "", nil, false
	}
	// Confirm this is actually a content-parts array (typed or sglang's flat shape) and
	// not some other array of objects that merely failed to unmarshal as expected -- an
	// empty first part (no type/text/image_url/video_url/image/video set at all) means
	// the shape doesn't match either content-parts variant, so fall back to the original
	// parse error instead of reporting a misleading per-index validation error.
	first := parts[0]
	if first.Type == "" && first.Text == "" && len(first.ImageURL) == 0 && len(first.VideoURL) == 0 && first.Image == "" && first.Video == "" {
		return "", nil, false
	}

	klog.V(4).InfoS("parsed multimodal embeddings input", "requestID", requestID, "parts", len(parts))

	if err := validateEmbeddingContentParts(parts); err != nil {
		return req.Model, buildErrorResponse(envoyTypePb.StatusCode_BadRequest, err.Error(), "", "input", HeaderErrorRequestBodyProcessing, "true"), true
	}

	if len(req.Stream) > 0 {
		var streamBool bool
		if err := sonic.Unmarshal(req.Stream, &streamBool); err != nil || streamBool {
			return req.Model, buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "stream not supported for embeddings", "", "stream", HeaderErrorRequestBodyProcessing, "true"), true
		}
	}

	return req.Model, nil, true
}

// validateEmbeddingRequest parses and validates an embeddings request body.
// nolint:nakedret
func validateEmbeddingRequest(requestID string, requestBody []byte) (model string, errRes *extProcPb.ProcessingResponse) {
	var embeddingReq embeddingReqMinimal
	if err := sonic.Unmarshal(requestBody, &embeddingReq); err != nil {
		// `input` may be an array of multimodal content parts (e.g. image_url), which
		// the strict OpenAI union type above does not support. Retry with that shape
		// before treating this as a hard parse failure.
		if m, mmErrRes, matched := validateMultimodalEmbeddingRequest(requestID, requestBody); matched {
			return m, mmErrRes
		}
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
	return
}

// validateImageGenerationRequest parses and validates an image/video generation request body.
// nolint:nakedret
func validateImageGenerationRequest(requestID string, requestBody []byte) (model string, errRes *extProcPb.ProcessingResponse) {
	imageGenerationObj := openai.ImageGenerateParams{}
	if err := sonic.Unmarshal(requestBody, &imageGenerationObj); err != nil {
		klog.ErrorS(err, "error to unmarshal image generations object", "requestID", requestID, "requestBody", string(requestBody))
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	model = imageGenerationObj.Model
	return
}

// validateRerankRequest parses and validates a rerank request body.
// nolint:nakedret
func validateRerankRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
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

func GetTraceID(traceparent, requestID string) string {
	traceparent = strings.TrimSpace(traceparent)
	if traceparent != "" {
		// W3C standard: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
		parts := strings.Split(traceparent, "-")
		if len(parts) == 4 && len(parts[1]) == 32 {
			return parts[1]
		}
	}

	return requestID
}
