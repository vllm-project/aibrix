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
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/errresp"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
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
	Messages []contentItem   `json:"messages"`
	System   json.RawMessage `json:"system"`
	// Tools is kept raw and only canonicalized into the prefix-match text.
	Tools json.RawMessage `json:"tools"`
}

// responsesReqMinimal captures the fields needed to route and validate an OpenAI
// Responses API (/v1/responses) request. Input is kept as raw JSON because it may
// be either a plain string or an array of input items; it is parsed lazily in
// parseResponsesInput. Stream uses *bool to distinguish "absent" from "stream: false".
type responsesReqMinimal struct {
	Model        string          `json:"model"`
	Stream       *bool           `json:"stream"`
	Input        json.RawMessage `json:"input"`
	Instructions json.RawMessage `json:"instructions"`
	Tools        json.RawMessage `json:"tools"`
}

// contentItem holds the raw JSON "content" field of a chat message or a Responses API
// input item, plus the "tool_calls" of an assistant chat message and its deprecated
// single-call form "function_call". It is shared by chatReqMinimal.Messages,
// parseChatMessages, and parseResponsesInput.
type contentItem struct {
	Content      json.RawMessage `json:"content"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
}

// engineNativeReqMinimal captures the fields needed to route a vLLM engine-native
// request (/tokenize, /pooling, /classify): the completion form carries "prompt"
// (tokenize) or "input" (pooling, classify), the chat form "messages". Prompt,
// input, and messages all stay raw JSON so a wrongly-typed value reaches the
// engine's validator instead of failing this unmarshal -- in particular a non-array
// "messages" on a completion-form body (an ignored extra) must not reject a valid
// "input".
type engineNativeReqMinimal struct {
	Model          string          `json:"model"`
	Prompt         json.RawMessage `json:"prompt"`
	Input          json.RawMessage `json:"input"`
	Messages       json.RawMessage `json:"messages"`
	Stream         json.RawMessage `json:"stream"`
	EncodingFormat json.RawMessage `json:"encoding_format"`
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
// directly; for array/object content it writes the raw JSON bytes. A null content
// contributes nothing. An assistant message's tool_calls, or its deprecated
// function_call, are written as raw JSON after its content: the engine renders them
// into the prompt, so they belong in the text used for prefix matching and
// prompt-length estimates.
func parseChatMessages(requestID string, msgs []contentItem) (string, *extProcPb.ProcessingResponse) {
	if len(msgs) == 0 {
		klog.ErrorS(nil, "no messages in the request body", "requestID", requestID)
		return "", errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "no messages in the request body", "", "messages", HeaderErrorRequestBodyProcessing, "true")
	}
	// Pre-grow the builder to avoid repeated internal buffer doublings for large messages.
	// Each Content entry is a raw JSON value; the unescaped string is at most len(Content) bytes.
	var builder strings.Builder
	growHint := len(msgs) - 1 // space separators
	for _, m := range msgs {
		growHint += len(m.Content) + len(m.ToolCalls) + len(m.FunctionCall) + 2
	}
	builder.Grow(growHint)
	for i, m := range msgs {
		if i > 0 {
			builder.WriteByte(' ')
		}
		hasContent := len(m.Content) > 0 && string(m.Content) != jsonNull
		if hasContent {
			writeChatContent(&builder, m.Content)
		}
		calls := m.ToolCalls
		if !hasToolCalls(calls) {
			calls = m.FunctionCall
		}
		if hasToolCalls(calls) {
			if hasContent {
				builder.WriteByte(' ')
			}
			builder.Write(calls)
		}
	}
	return builder.String(), nil
}

// hasToolCalls reports whether a message's raw "tool_calls" or "function_call"
// holds a call. An empty array counts as none, with or without inner whitespace.
func hasToolCalls(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == jsonNull {
		return false
	}
	if raw[0] == '[' && raw[len(raw)-1] == ']' {
		return len(bytes.TrimSpace(raw[1:len(raw)-1])) > 0
	}
	return true
}

// writeChatContent appends one message's content to the routing text.
func writeChatContent(builder *strings.Builder, content json.RawMessage) {
	if content[0] == '"' {
		// Simple string content: JSON-unquote it without allocating an interface.
		var s string
		if err := sonic.Unmarshal(content, &s); err == nil {
			builder.WriteString(s)
			return
		}
	}
	// Array or object content parts: write raw JSON.
	builder.Write(content)
}

// parseResponsesInput extracts a single concatenated text string from a Responses
// API "input" field for routing purposes. The field is either a plain string or an
// array of input items whose "content" is itself a string or an array of content
// parts; in all cases we reuse the same text-extraction strategy as chat messages.
func parseResponsesInput(requestID string, input json.RawMessage) (string, *extProcPb.ProcessingResponse) {
	if len(input) == 0 || string(input) == jsonNull {
		klog.ErrorS(nil, "no input in the request body", "requestID", requestID)
		return "", errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' is a required property", "", "input", HeaderErrorRequestBodyProcessing, "true")
	}
	// Plain string input: JSON-unquote it directly.
	if input[0] == '"' {
		var s string
		if err := sonic.Unmarshal(input, &s); err != nil {
			klog.ErrorS(nil, "error to unmarshal responses input string", "requestID", requestID, "reason", jsonParseReason(err))
			return "", errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "input", HeaderErrorRequestBodyProcessing, "true")
		}
		return s, nil
	}
	// Array of input items: each item may carry a "content" field (string or array
	// of content parts). Items without content (e.g. tool/function outputs) simply
	// contribute nothing to the routing key.
	var items []contentItem
	if err := sonic.Unmarshal(input, &items); err != nil {
		klog.ErrorS(nil, "error to unmarshal responses input array", "requestID", requestID, "reason", jsonParseReason(err))
		return "", errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' must be a string or an array of input items", "", "input", HeaderErrorRequestBodyProcessing, "true")
	}
	if len(items) == 0 {
		klog.ErrorS(nil, "empty input array in the request body", "requestID", requestID)
		return "", errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' array cannot be empty", "", "input", HeaderErrorRequestBodyProcessing, "true")
	}
	return parseChatMessages(requestID, items)
}

// validateRequestBody validates input by unmarshaling request body into respective openai-golang struct based on requestpath.
// The per-path parsing is delegated to dedicated validate* helpers to keep this dispatcher simple.
//
// prefixText is the text prefix-matching policies should hash when it differs from
// message (see RoutingContext.PrefixMatchText); it is empty otherwise.
// nolint:nakedret
func validateRequestBody(requestID, requestPath string, requestBody []byte, user utils.User) (model, message, prefixText string, stream bool, errRes *extProcPb.ProcessingResponse) {
	path := pathWithoutQuery(requestPath)
	switch path {
	// Text generation
	case PathChatCompletions, PathMessages, PathMessagesCountTokens:
		model, message, prefixText, stream, errRes = validateChatRequest(requestID, path, requestBody, user)
	case PathChatCompletionsBatch:
		model, message, errRes = validateChatBatchRequest(requestID, requestBody)
	case PathResponses:
		model, message, prefixText, stream, errRes = validateResponsesRequest(requestID, requestBody)
	case PathCompletions:
		model, message, stream, errRes = validateCompletionRequest(requestID, requestBody, user)

	// Embeddings, ranking, classification and scoring
	case PathEmbeddings:
		model, errRes = validateEmbeddingRequest(requestID, requestBody)
	case PathRerank:
		model, message, errRes = validateRerankRequest(requestID, requestBody)
	case PathClassify:
		model, message, errRes = validateClassifyRequest(requestID, requestBody)
	case PathNativeClassify:
		model, message, errRes = validateNativeClassifyRequest(requestID, requestBody)
	case PathDecisions:
		model, message, errRes = validateDecisionsRequest(requestID, requestBody)
	case PathSystemOne:
		model, message, errRes = validateSystemOneRequest(requestID, requestBody)
	case PathScore:
		model, message, errRes = validateScoreRequest(requestID, requestBody)

	// Engine-native utilities
	case PathTokenize:
		model, message, errRes = validateTokenizeRequest(requestID, requestBody)
	case PathPooling:
		model, message, errRes = validatePoolingRequest(requestID, requestBody)

	// Image and video generation share one JSON validator
	case PathImagesGenerations, PathVideoGenerations:
		model, errRes = validateImageGenerationRequest(requestID, path, requestBody)

	case PathImagesEdits:
		// Image edits upload the source images as files, so the body is multipart/form-data.
		// A multipart request never gets here (see isMultipartFormPath); this is a JSON or
		// content-type-less one.
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "image edit requests must use multipart/form-data content-type", "", "", HeaderErrorRequestBodyProcessing, "true")

	// Audio
	case PathAudioTranscriptions, PathAudioTranslations:
		// Audio transcription and translation require multipart/form-data content-type, not JSON
		// This case handles the error when JSON is sent to these audio endpoints
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "audio requests must use multipart/form-data content-type", "", "", HeaderErrorRequestBodyProcessing, "true")
	case PathAudioSpeech, PathAudioGenerate:
		model, message, errRes = validateAudioInputRequest(requestID, requestBody)
	case PathAudioSpeechBatch:
		model, message, errRes = validateAudioSpeechBatchRequest(requestID, requestBody)
	default:
		if isAudioVoicesPath(path) {
			// The body (an uploaded voice sample, or none at all) is the engine's business.
			model, errRes = audioVoicesModel(requestPath)
			break
		}
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_NotImplemented, "unknown request path", "", "", HeaderErrorRequestBodyProcessing, "true")
	}
	if errRes != nil {
		return
	}

	// The model name is kept for as long as the request is tracked (routing
	// context, in-flight gauges, log fields), but the validate* helpers decode
	// with sonic's default config, which does not copy strings: model would be a
	// slice of sonic's private copy of the entire request body and keep it alive.
	model = strings.Clone(model)

	klog.V(4).InfoS("validateRequestBody", "requestID", requestID, "requestPath", requestPath, "model", model, "messageBytes", len(message), "stream", stream)
	if v := klog.V(6); v.Enabled() {
		text, truncated := truncateForLog([]byte(message))
		v.InfoS("validateRequestBody message", "requestID", requestID, "message", text, "messageTruncated", truncated)
	}
	return
}

// validateChatRequest parses and validates a chat completions (or Anthropic-style messages) request body.
// nolint:nakedret
func validateChatRequest(requestID, requestPath string, requestBody []byte, user utils.User) (model, message, prefixText string, stream bool, errRes *extProcPb.ProcessingResponse) {
	// Single-pass minimal unmarshal: avoids the openai SDK's reflection-heavy
	// apijson decoder and gjson parsing, and eliminates the previous redundant
	// map[string]json.RawMessage unmarshal used only for stream-field detection.
	var req chatReqMinimal
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		logRequestBodyParseFailure(requestID, requestPath, "error to unmarshal chat completions object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	model = req.Model
	if message, errRes = parseChatMessages(requestID, req.Messages); errRes != nil {
		return
	}
	systemText := ""
	if requestPath == PathMessages || requestPath == PathMessagesCountTokens {
		systemText = requestPromptText(requestID, req.System)
	}
	prefixText = combinePrefixText(message, systemText, canonicalToolsText(requestID, req.Tools))
	if req.Stream != nil {
		stream = *req.Stream
		// stream_options.include_usage is an OpenAI-specific field; Anthropic-style
		// clients hitting /v1/messages will not include it, so skip this check for that path.
		if stream && user.Tpm > 0 && requestPath == PathChatCompletions && !req.StreamOptions.IncludeUsage {
			klog.ErrorS(nil, "no stream with usage option available", "requestID", requestID)
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "include usage for stream options not set",
				"", "stream_options", HeaderErrorStreamOptionsIncludeUsage, "include usage for stream options not set")
			return
		}
	}
	return
}

// validateChatBatchRequest parses and validates a /v1/chat/completions/batch request body.
// Only the envelope is checked: a JSON object with a non-empty "model" to route on and a
// non-empty "messages" array whose entries are non-empty conversations, each an array of
// messages. The sampling options and message contents belong to the engine. The returned message
// joins the conversation texts for routing. A "stream" field is deliberately not surfaced as a
// stream: the gateway always treats a batch as non-SSE, and the engine does not stream one (it
// expects stream to be false or omitted and may reject true), so its response is one JSON
// document that SSE parsing would fail on.
// nolint:nakedret
func validateChatBatchRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	if trimmed := bytes.TrimSpace(requestBody); len(trimmed) == 0 || trimmed[0] != '{' {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "request body must be a JSON object", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	// Conversations stay raw so each is parsed once, after its shape is known to be an array.
	var req struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		logRequestBodyParseFailure(requestID, PathChatCompletionsBatch, "error to unmarshal chat completions batch object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	// The gateway selects the backend pod by model, so it cannot fall back to an engine default.
	if strings.TrimSpace(req.Model) == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if len(req.Messages) == 0 {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'messages' is a required property and cannot be empty", "", "messages", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	conversationTexts := make([]string, 0, len(req.Messages))
	for i, rawConversation := range req.Messages {
		param := fmt.Sprintf("messages.%d", i)
		// A flat list of messages is the single-conversation shape; name the entry that gives it away.
		if trimmed := bytes.TrimSpace(rawConversation); len(trimmed) == 0 || trimmed[0] != '[' {
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, fmt.Sprintf("'%s' must be an array of messages", param), "", param, HeaderErrorRequestBodyProcessing, "true")
			return
		}
		var conversation []contentItem
		if err := sonic.Unmarshal(rawConversation, &conversation); err != nil {
			logRequestBodyParseFailure(requestID, PathChatCompletionsBatch, "error to unmarshal chat completions batch conversation", err, rawConversation)
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", param, HeaderErrorRequestBodyProcessing, "true")
			return
		}
		if len(conversation) == 0 {
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, fmt.Sprintf("'%s' cannot be empty", param), "", param, HeaderErrorRequestBodyProcessing, "true")
			return
		}
		var conversationText string
		if conversationText, errRes = parseChatMessages(requestID, conversation); errRes != nil {
			return
		}
		conversationTexts = append(conversationTexts, conversationText)
	}

	model, message = req.Model, strings.Join(conversationTexts, "\n")
	return
}

// validateResponsesRequest parses and validates an OpenAI Responses API (/v1/responses) request body.
// nolint:nakedret
func validateResponsesRequest(requestID string, requestBody []byte) (model, message, prefixText string, stream bool, errRes *extProcPb.ProcessingResponse) {
	// OpenAI Responses API. Unlike chat completions, the Responses API always
	// emits usage in the terminal streaming event, so there is no stream_options
	// .include_usage requirement to enforce for TPM-limited users.
	var req responsesReqMinimal
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		logRequestBodyParseFailure(requestID, PathResponses, "error to unmarshal responses object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	// Per the OpenAI Responses API spec, "model" is a required property.
	if req.Model == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	model = req.Model
	if message, errRes = parseResponsesInput(requestID, req.Input); errRes != nil {
		return
	}
	prefixText = combinePrefixText(
		message,
		requestPromptText(requestID, req.Instructions),
		canonicalToolsText(requestID, req.Tools),
	)
	if req.Stream != nil {
		stream = *req.Stream
	}
	return
}

// validateCompletionRequest parses and validates a legacy completions request body.
// nolint:nakedret
func validateCompletionRequest(requestID string, requestBody []byte, user utils.User) (model, message string, stream bool, errRes *extProcPb.ProcessingResponse) {
	var req struct {
		Prompt        json.RawMessage `json:"prompt"`
		Model         string          `json:"model"`
		Stream        bool            `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		logRequestBodyParseFailure(requestID, PathCompletions, "error to unmarshal completions object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	model = req.Model
	message = completionPromptText(requestID, req.Prompt)
	stream = req.Stream
	// Without stream_options.include_usage the engine sends no usage chunk, so a
	// TPM-limited user's streamed tokens would never be metered. Mirror the check
	// validateChatRequest applies to /v1/chat/completions.
	if stream && user.Tpm > 0 && !req.StreamOptions.IncludeUsage {
		klog.ErrorS(nil, "no stream with usage option available", "requestID", requestID)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "include usage for stream options not set",
			"", "stream_options", HeaderErrorStreamOptionsIncludeUsage, "include usage for stream options not set")
		return
	}
	return
}

// completionPromptText renders all four prompt shapes accepted by the OpenAI
// completions API. String batches are joined for useful routing text; token
// arrays retain their canonical JSON form because the gateway cannot decode
// model-specific token IDs.
func completionPromptText(requestID string, prompt json.RawMessage) string {
	raw := bytes.TrimSpace(prompt)
	if len(raw) > 1 && raw[0] == '[' {
		firstElement := bytes.TrimSpace(raw[1:])
		if len(firstElement) > 0 && firstElement[0] == '"' {
			var prompts []string
			if err := sonic.Unmarshal(raw, &prompts); err == nil {
				return strings.Join(prompts, " ")
			}
		}
	}
	return requestPromptText(requestID, raw)
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

// validateEmbeddingContentParts validates the shape of a multimodal embeddings
// `input` array. Text length is not checked, for the reasons given on
// validateStringInputs. Image and video parts were never counted anyway, since
// the gateway's text tokenizer cannot estimate their cost.
func validateEmbeddingContentParts(parts []embeddingContentPart) error {
	if len(parts) == 0 {
		return errors.New("input array cannot be empty")
	}

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
		return req.Model, errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, err.Error(), "", "input", HeaderErrorRequestBodyProcessing, "true"), true
	}

	if len(req.Stream) > 0 {
		var streamBool bool
		if err := sonic.Unmarshal(req.Stream, &streamBool); err != nil || streamBool {
			return req.Model, errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "stream not supported for embeddings", "", "stream", HeaderErrorRequestBodyProcessing, "true"), true
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
		logRequestBodyParseFailure(requestID, PathEmbeddings, "error to unmarshal embeddings object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	model = embeddingReq.Model
	if err := validateEmbeddingInput(openai.EmbeddingNewParams{
		Model: embeddingReq.Model,
		Input: embeddingReq.Input,
	}); err != nil {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, err.Error(), "", "input", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	// Preserve behavior: if stream is provided, it must be a valid bool and false.
	if len(embeddingReq.Stream) > 0 {
		var streamBool bool
		if err := sonic.Unmarshal(embeddingReq.Stream, &streamBool); err != nil || streamBool {
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "stream not supported for embeddings", "", "stream", HeaderErrorRequestBodyProcessing, "true")
			return
		}
	}
	return
}

// validateImageGenerationRequest parses and validates an image/video generation request body.
// nolint:nakedret
func validateImageGenerationRequest(requestID, requestPath string, requestBody []byte) (model string, errRes *extProcPb.ProcessingResponse) {
	imageGenerationObj := openai.ImageGenerateParams{}
	if err := sonic.Unmarshal(requestBody, &imageGenerationObj); err != nil {
		logRequestBodyParseFailure(requestID, requestPath, "error to unmarshal image generations object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
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
		logRequestBodyParseFailure(requestID, PathRerank, "error to unmarshal rerank object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if req.Model == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	if req.Query == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'query' is a required property", "", "query", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	if len(req.Documents) == 0 {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'documents' is a required property and cannot be empty", "", "documents", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	model = req.Model
	message = strings.Join(append([]string{req.Query}, req.Documents...), " ")
	return
}

// validateTokenizeRequest parses and validates a vLLM /tokenize request body. Only "model"
// is required - the gateway needs it to route, though vLLM itself treats it as optional -
// and the rest of the schema is left to the engine. Nothing is metered: no tokens are generated.
// nolint:nakedret
func validateTokenizeRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	return validateEngineNativeRequest(requestID, PathTokenize, requestBody)
}

// validatePoolingRequest parses and validates a vLLM /pooling request body. Only "model"
// is required - the one field the gateway routes on - and the rest of the schema is left
// to the engine. Stream is rejected when present and true, as for embeddings: pooling
// never streams, and a stream=true body would otherwise be forwarded just to fail in the
// engine with a less specific error. The octet-stream encoding formats (bytes, bytes_only)
// are rejected at the edge too, because the language response path cannot meter them.
// nolint:nakedret
func validatePoolingRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	return validateEngineNativeRequest(requestID, PathPooling, requestBody)
}

// validateNativeClassifyRequest parses and validates a vLLM /classify request body, which
// takes the same "input" or "messages" forms as /pooling. Only "model" is required, and the
// rest of the schema is left to the engine.
// nolint:nakedret
func validateNativeClassifyRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	return validateEngineNativeRequest(requestID, PathNativeClassify, requestBody)
}

// validateEngineNativeRequest is the shared validator for vLLM engine-native paths
// (/tokenize, /pooling, /classify), whose request bodies are a union of a completion
// form ("prompt" for tokenize, "input" otherwise) and a chat form ("messages").
// Only "model" is required - the one field the gateway routes on; a body without
// any input field still reaches the engine, which owns that error. The routing
// message is best-effort: parseChatMessages unquotes a JSON string and writes any
// other JSON value (array, token ids) as raw bytes, so the whole input value becomes
// one content item rather than being expanded into several.
// nolint:nakedret
func validateEngineNativeRequest(requestID, endpointPath string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	endpoint := strings.TrimPrefix(endpointPath, "/")
	pooling := endpointPath == PathPooling
	var req engineNativeReqMinimal
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		logRequestBodyParseFailure(requestID, endpointPath, "error to unmarshal "+endpoint+" object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if req.Model == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	model = req.Model

	// Select the completion field by endpoint: tokenize reads "prompt", pooling
	// and classify read "input". The other completion field is an ignored extra on
	// that path (vLLM's OpenAIBaseModel is extra="allow"), so it must not win the
	// routing key. The chat form ("messages") is the fallback for all of them.
	completionField := req.Input
	if endpointPath == PathTokenize {
		completionField = req.Prompt
	}

	// Best-effort routing key: the completion form's raw field first, then the
	// chat form's messages. Messages is held as raw JSON and parsed only when it
	// is the chosen field, so a non-array value on a completion-form body cannot
	// fail this unmarshal.
	switch {
	case len(completionField) > 0 && string(completionField) != jsonNull:
		message, errRes = parseChatMessages(requestID, []contentItem{{Content: completionField}})
	case len(req.Messages) > 0 && string(req.Messages) != jsonNull:
		var msgs []contentItem
		if err := sonic.Unmarshal(req.Messages, &msgs); err != nil {
			// Non-array messages on a messages-only body: no routing key, but the
			// engine owns the schema error, so forward without one.
			klog.ErrorS(nil, "error to unmarshal "+endpoint+" messages", "requestID", requestID, "reason", jsonParseReason(err))
			break
		}
		message, errRes = parseChatMessages(requestID, msgs)
	}
	if errRes != nil {
		return
	}

	// Non-streaming engine paths reject stream at the edge, like embeddings;
	// tokenize and classify have no stream field, and a stray one is left to the engine.
	if pooling && len(req.Stream) > 0 {
		var streamBool bool
		if err := sonic.Unmarshal(req.Stream, &streamBool); err != nil || streamBool {
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "stream not supported for pooling", "", "stream", HeaderErrorRequestBodyProcessing, "true")
			return
		}
	}

	// vLLM's other two pooling encoding formats (bytes, bytes_only) return
	// application/octet-stream, which the language response path cannot meter;
	// reject them at the edge and keep JSON pooling (float, base64) on that path.
	if pooling && len(req.EncodingFormat) > 0 {
		var enc string
		if err := sonic.Unmarshal(req.EncodingFormat, &enc); err == nil && (enc == "bytes" || enc == "bytes_only") {
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "encoding_format 'bytes' and 'bytes_only' are not supported for pooling", "", "encoding_format", HeaderErrorRequestBodyProcessing, "true")
			return
		}
	}
	return
}

// pathWithoutQuery strips the query string from an Envoy :path value. HTTP/2
// :path includes both path and query (RFC 7540), so exact/prefix matchers must
// cut on '?' before comparing.
func pathWithoutQuery(requestPath string) string {
	return utils.PathWithoutQuery(requestPath)
}

// validateRoutingStrategyForPath checks whether strategy is supported for requestPath.
// The prefill-decode (PD) disaggregated router mutates generation-specific fields
// and builds an engine-specific prefill request. It supports the generation endpoints
// /v1/chat/completions, /v1/completions, and /v1/responses. Responses is limited to
// engines whose Responses API exposes the PD handshake: vLLM (>= 0.19.0) and SGLang
// (>= 0.5.22). An empty engine label is the PD router's default vLLM engine (see
// routingalgorithms.ValidateAndGetLLMEngine). Rejecting unsupported endpoint/engine
// combinations protects other APIs from invalid request mutations and unexpected
// routing behavior.
func validateRoutingStrategyForPath(requestPath string, strategy types.RoutingAlgorithm, llmEngine string) error {
	resolvedStrategy := string(strategy)
	if resolvedExclusive, isExclusive := routing.ResolveExclusiveStrategy(string(strategy)); isExclusive {
		resolvedStrategy = resolvedExclusive
	}

	if resolvedStrategy == string(routing.RouterPD) {
		cleanPath := pathWithoutQuery(requestPath)
		switch cleanPath {
		case PathChatCompletions, PathCompletions:
			return nil
		case PathResponses:
			switch llmEngine {
			case "", EngineVLLM, EngineSGLang:
				return nil
			default:
				return fmt.Errorf("routing strategy '%s' is not supported for endpoint '%s' with engine '%s'", routing.RouterPD, cleanPath, llmEngine)
			}
		default:
			return fmt.Errorf("routing strategy '%s' is not supported for endpoint '%s'", routing.RouterPD, cleanPath)
		}
	}

	return nil
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
		logRequestBodyParseFailure(requestID, PathClassify, "error to unmarshal classify object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if req.Model == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if len(req.Input) == 0 || string(req.Input) == jsonNull {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' is a required property", "", "input", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	// Parse input - can be string or array of strings
	var inputStr string
	if err := json.Unmarshal(req.Input, &inputStr); err == nil {
		if inputStr == "" {
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' cannot be an empty string", "", "input", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		message = inputStr
	} else {
		var inputArr []string
		if err := json.Unmarshal(req.Input, &inputArr); err != nil {
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' must be a string or array of strings", "", "input", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		if len(inputArr) == 0 {
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' array cannot be empty", "", "input", HeaderErrorRequestBodyProcessing, "true")
			return
		}
		message = strings.Join(inputArr, " ")
	}

	model = req.Model
	return
}

// validateDecisionsRequest parses and validates a /v1/decisions request body in either
// engine's format (SGLang's, or OpenAI's as served by vLLM). "model" is required although
// SGLang treats it as optional, because the gateway routes on it. "input" and a non-empty
// "questions" array are required too, so a body the engine would reject fails at the edge;
// the question schema is left to the engine.
//
// The routing message is "input" alone: the engine renders it ahead of the questions, so it
// is the shared prefix that prefix-cache-aware routing should see. For vLLM's user-message
// array it is the JSON of the array rather than the rendered prompt, so only approximate.
// nolint:nakedret
func validateDecisionsRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	var req struct {
		Model     string          `json:"model"`
		Input     json.RawMessage `json:"input"`
		Questions json.RawMessage `json:"questions"`
	}
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		logRequestBodyParseFailure(requestID, PathDecisions, "error to unmarshal decisions object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	input := bytes.TrimSpace(req.Input)
	if len(input) == 0 || string(input) == jsonNull {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' is a required property", "", "input", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	text, errMsg := decisionsInputText(input)
	if errMsg != "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, errMsg, "", "input", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	var questions []json.RawMessage
	if err := sonic.Unmarshal(req.Questions, &questions); err != nil || len(questions) == 0 {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'questions' is a required property and must be a non-empty array", "", "questions", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	model, message = req.Model, text
	return
}

// validateSystemOneRequest parses and validates a /v1/systemone request body (vLLM or SGLang).
// Unlike a decisions "input", "state" may be blank: SGLang accepts an empty state, so the
// gateway does not refuse what the engine would answer. The question schema is left to the
// engine.
//
// The routing message is the state alone: both engines put it at the head of every
// question's prompt, so it is the prefix that prefix-cache-aware routing should see.
// nolint:nakedret
func validateSystemOneRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	var req struct {
		Model     string          `json:"model"`
		State     json.RawMessage `json:"state"`
		Questions json.RawMessage `json:"questions"`
	}
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		logRequestBodyParseFailure(requestID, PathSystemOne, "error to unmarshal systemone object", err, requestBody)
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	state := bytes.TrimSpace(req.State)
	if len(state) == 0 || string(state) == jsonNull {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'state' is a required property", "", "state", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	text, _, ok := decisionText(state)
	if !ok {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'state' must be a string, object, or array", "", "state", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	var questions map[string]json.RawMessage
	if err := sonic.Unmarshal(req.Questions, &questions); err != nil || len(questions) == 0 {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'questions' is a required property and must be a non-empty object", "", "questions", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	model, message = req.Model, text
	return
}

// validateScoreRequest parses and validates an SGLang /v1/score request body. Only what the
// gateway itself needs is checked: a JSON object with a non-empty "model" to route on, a
// non-empty "items" to score, and no streaming, since the response is a single JSON body.
// The scoring options (label_token_ids, apply_softmax, item_first, score_extraction_token,
// ...) are SGLang's to validate and are forwarded as sent.
// nolint:nakedret
func validateScoreRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	if trimmed := bytes.TrimSpace(requestBody); len(trimmed) == 0 || trimmed[0] != '{' {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "request body must be a JSON object", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	var req struct {
		Model  string          `json:"model"`
		Query  json.RawMessage `json:"query"`
		Items  json.RawMessage `json:"items"`
		Stream json.RawMessage `json:"stream"`
	}
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		// The body can hold customer text, so log its size, not its content.
		klog.ErrorS(nil, "error to unmarshal score object", "requestID", requestID, "reason", jsonParseReason(err), "requestBodyBytes", len(requestBody))
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	// SGLang falls back to a default model, but the gateway selects pods by it.
	if strings.TrimSpace(req.Model) == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	items := bytes.TrimSpace(req.Items)
	switch string(items) {
	case "", jsonNull, `""`, "[]":
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'items' is a required property and cannot be empty", "", "items", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if len(req.Stream) > 0 {
		var streamBool bool
		if err := sonic.Unmarshal(req.Stream, &streamBool); err != nil || streamBool {
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "stream not supported for score", "", "stream", HeaderErrorRequestBodyProcessing, "true")
			return
		}
	}

	model, message = req.Model, scoreRoutingText(req.Query, items)
	return
}

// scoreRoutingText picks the text prefix-cache-aware routing should see for a score request:
// the query when it is non-blank text, otherwise the first item when items are text. Token-ID
// queries and items give no routing text, because stringified ids would not share a prefix
// with the same text sent as a string.
func scoreRoutingText(query, items json.RawMessage) string {
	if text, ok := jsonString(query); ok && strings.TrimSpace(text) != "" {
		return text
	}
	if text, ok := jsonString(items); ok {
		return text
	}
	var list []json.RawMessage
	if items[0] == '[' && sonic.Unmarshal(items, &list) == nil && len(list) > 0 {
		if text, ok := jsonString(list[0]); ok {
			return text
		}
	}
	return ""
}

// jsonString returns value as a Go string when it is a JSON string.
func jsonString(value json.RawMessage) (string, bool) {
	value = bytes.TrimSpace(value)
	if len(value) == 0 || value[0] != '"' {
		return "", false
	}
	var text string
	if err := sonic.Unmarshal(value, &text); err != nil {
		return "", false
	}
	return text, true
}

// validateAudioInputRequest parses and validates the body of a vLLM-Omni request that takes a
// model and a text input and answers with binary audio: /v1/audio/speech, where "input" is the
// text to synthesize, and /v1/audio/generate, where it is the prompt that describes the audio.
// Only what the gateway itself needs is checked: a JSON object with a non-empty "model" to route
// on and a non-empty textual "input". The remaining options (voice, response_format, speed,
// instructions, language, stream_format, ...) belong to the engine and are forwarded as sent.
// A request-level "stream" field is deliberately not surfaced as a stream here: the response is
// binary audio, so the gateway must never treat it as an SSE stream.
// nolint:nakedret
func validateAudioInputRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	if trimmed := bytes.TrimSpace(requestBody); len(trimmed) == 0 || trimmed[0] != '{' {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "request body must be a JSON object", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	var req struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		// The body can hold customer text, so log its size, not its content.
		klog.ErrorS(nil, "error to unmarshal audio speech or generate object", "requestID", requestID, "reason", jsonParseReason(err), "requestBodyBytes", len(requestBody))
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	// The gateway selects the backend pod by model, so it cannot fall back to an engine default.
	if strings.TrimSpace(req.Model) == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if strings.TrimSpace(req.Input) == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'input' is a required property and cannot be empty", "", "input", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	model, message = req.Model, req.Input
	return
}

// isAudioVoicesPath reports whether requestPath is part of the custom voice API: the collection
// (PathAudioVoices, for GET and POST) or one voice in it (PathAudioVoices/{name}, for DELETE).
// The method and the shape of the remainder are left to the engine, which answers 404 or 405
// for a combination it does not serve.
func isAudioVoicesPath(requestPath string) bool {
	path := pathWithoutQuery(requestPath)
	return path == PathAudioVoices || strings.HasPrefix(path, PathAudioVoices+"/")
}

// audioVoicesModel reads the model a voice request is routed to from its "model" query
// parameter. The voice API names no model of its own: an engine serves one model, so upstream
// needs none, but the gateway chooses the pod by model, and a GET or DELETE has no body to put
// it in. The parameter is forwarded to the engine with the rest of the query, which ignores it.
// nolint:nakedret
func audioVoicesModel(requestPath string) (model string, errRes *extProcPb.ProcessingResponse) {
	_, rawQuery, _ := strings.Cut(requestPath, "?")
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "invalid query string", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	models := values["model"]
	if len(models) > 1 {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' query parameter must be supplied once", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	if len(models) == 0 || strings.TrimSpace(models[0]) == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' query parameter is required", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	model = strings.TrimSpace(models[0])
	return
}

// validateAudioSpeechBatchRequest parses and validates a /v1/audio/speech/batch request body.
// Only the envelope is checked: a JSON object with a non-empty "model" to route on and a
// non-empty "items" array of objects. What is inside an item (input, voice, ref_audio, a
// per-item stream, ...) belongs to the engine, which reports a bad item as a failed entry in an
// otherwise successful response, so one bad item must not fail the whole batch here. The returned
// message joins the textual item inputs for routing. As with a single speech request, a "stream"
// field is deliberately not surfaced as a stream: the response is one JSON document, not SSE.
// nolint:nakedret
func validateAudioSpeechBatchRequest(requestID string, requestBody []byte) (model, message string, errRes *extProcPb.ProcessingResponse) {
	if trimmed := bytes.TrimSpace(requestBody); len(trimmed) == 0 || trimmed[0] != '{' {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "request body must be a JSON object", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}
	// Items stay raw so a reference clip inlined as a base64 data URL is never decoded here.
	var req struct {
		Model string            `json:"model"`
		Items []json.RawMessage `json:"items"`
	}
	if err := sonic.Unmarshal(requestBody, &req); err != nil {
		// The body can hold customer text, so log its size, not its content.
		klog.ErrorS(nil, "error to unmarshal audio speech batch object", "requestID", requestID, "reason", jsonParseReason(err), "requestBodyBytes", len(requestBody))
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", "", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	// The gateway selects the backend pod by model, so it cannot fall back to an engine default.
	if strings.TrimSpace(req.Model) == "" {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	if len(req.Items) == 0 {
		errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'items' is a required property and cannot be empty", "", "items", HeaderErrorRequestBodyProcessing, "true")
		return
	}

	inputs := make([]string, 0, len(req.Items))
	for i, rawItem := range req.Items {
		if trimmed := bytes.TrimSpace(rawItem); len(trimmed) == 0 || trimmed[0] != '{' {
			param := fmt.Sprintf("items.%d", i)
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, fmt.Sprintf("'%s' must be a JSON object", param), "", param, HeaderErrorRequestBodyProcessing, "true")
			return
		}
		var item struct {
			Input json.RawMessage `json:"input"`
		}
		if err := sonic.Unmarshal(rawItem, &item); err != nil {
			klog.ErrorS(nil, "error to unmarshal audio speech batch item", "requestID", requestID, "item", i, "reason", jsonParseReason(err), "itemBytes", len(rawItem))
			errRes = errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "error processing request body", "", fmt.Sprintf("items.%d", i), HeaderErrorRequestBodyProcessing, "true")
			return
		}
		// A missing, blank or non-string input is the engine's per-item error to report.
		if input, ok := jsonString(item.Input); ok && strings.TrimSpace(input) != "" {
			inputs = append(inputs, input)
		}
	}

	model, message = req.Model, strings.Join(inputs, "\n")
	return
}

// jsonParseReason returns why sonic rejected a body without the excerpt of the body that its
// error text embeds (a syntax error quotes the bytes around the failure, a type mismatch
// quotes the offending value), so the result is safe to log.
func jsonParseReason(err error) string {
	var withMessage interface{ Message() string }
	if errors.As(err, &withMessage) {
		return withMessage.Message()
	}
	return "invalid JSON body"
}

// maxLoggedRequestBytes caps the request text the V(6) diagnostics write.
const maxLoggedRequestBytes = 4096

// logRequestBodyParseFailure logs a request body that did not parse. The body can hold
// prompts, tool arguments or base64 images, so normal levels get its size and the
// sanitized reason; the body itself is only written, capped, at V(6).
func logRequestBodyParseFailure(requestID, endpoint, msg string, err error, requestBody []byte) {
	klog.ErrorS(nil, msg, "requestID", requestID, "endpoint", endpoint, "reason", jsonParseReason(err), "requestBodyBytes", len(requestBody))
	// A multipart body sent to a JSON endpoint carries file contents and form values,
	// which are not logged at any level.
	if v := klog.V(6); v.Enabled() && !isMultipartBody(requestBody) {
		text, truncated := truncateForLog(requestBody)
		v.InfoS("request body parse failure details", "requestID", requestID, "requestBody", text, "requestBodyTruncated", truncated)
	}
}

// truncateForLog returns at most maxLoggedRequestBytes of b, cut on a rune boundary, and
// whether anything was cut.
func truncateForLog(b []byte) (string, bool) {
	if len(b) <= maxLoggedRequestBytes {
		return string(b), false
	}
	cut := maxLoggedRequestBytes
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return string(b[:cut]), true
}

// decisionsInputText renders a /v1/decisions "input" value (a string, object, or array)
// to text and returns an error message when it is not one of those or is blank.
func decisionsInputText(input []byte) (text, errMsg string) {
	text, blank, ok := decisionText(input)
	switch {
	case !ok:
		return "", "'input' must be a string, object, or array"
	case blank:
		return "", "'input' cannot be blank"
	}
	return text, ""
}

// decisionText renders a JSON string, object, or array, the type SGLang calls a decision
// text, to the text SGLang puts in the prompt. An object or array is re-serialized as compact
// JSON the way SGLang does (see renderJSONLikePython). blank reports what SGLang treats as
// blank: an empty or whitespace-only string, or an empty object or array. ok is false for
// any other JSON type or a value that does not parse.
func decisionText(value []byte) (text string, blank, ok bool) {
	if len(value) == 0 {
		return "", false, false
	}
	switch value[0] {
	case '"':
		if err := sonic.Unmarshal(value, &text); err != nil {
			return "", false, false
		}
		return text, strings.TrimSpace(text) == "", true
	case '{', '[':
		rendered, err := renderJSONLikePython(value)
		if err != nil {
			return "", false, false
		}
		return rendered, rendered == "{}" || rendered == "[]", true
	default:
		return "", false, false
	}
}

// validateStreamOptions validates whether stream options to include usage is set for user request
func validateStreamOptions(requestID string, user utils.User, stream *bool, streamOptions openai.ChatCompletionStreamOptionsParam, jsonMap map[string]json.RawMessage) *extProcPb.ProcessingResponse {
	streamData, ok := jsonMap["stream"]
	if !ok {
		return nil
	}

	if err := sonic.Unmarshal(streamData, stream); err != nil {
		klog.ErrorS(nil, "no stream option available", "requestID", requestID)
		return errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "stream incorrectly set", "", "stream", HeaderErrorStream, "stream incorrectly set")
	}

	if *stream && user.Tpm > 0 {
		if !streamOptions.IncludeUsage.Value {
			klog.ErrorS(nil, "no stream with usage option available", "requestID", requestID, "streamOption", streamOptions)
			return errresp.BuildErrorResponse(envoyTypePb.StatusCode_BadRequest, "include usage for stream options not set",
				"", "stream_options", HeaderErrorStreamOptionsIncludeUsage, "include usage for stream options not set")
		}
	}
	return nil
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

// validateStringInputs validates the shape of string inputs (both single string
// and array of strings).
//
// Input length is deliberately not checked here. The gateway estimates tokens
// with tiktoken (cl100k_base), but the limit that matters is denominated in the
// target model's own tokens -- a different unit. For sentencepiece models such
// as bge-m3 or multilingual-e5, cl100k over-counts non-English text by roughly
// 2x, so valid requests are rejected with a misleading message. The gateway
// also cannot know which model's limit to apply: validateRequestBody runs
// before the pod list and engine are resolved, so the addressed model's
// tokenizer and context length are both out of reach.
//
// The backend already enforces the real limit with the model's own tokenizer
// and reports it accurately. It also honours truncate_prompt_tokens, which a
// gateway-side rejection pre-empts entirely -- clients that ask the backend to
// truncate oversized input currently get a 400 from the gateway instead.
//
// The batch size is still bounded: MaxArrayDimensions is OpenAI's documented
// maxItems for the input array, a property of the request rather than of the
// model, so the gateway can and does check it.
func validateStringInputs(inputs []string) error {
	if len(inputs) == 0 {
		return errors.New("input array cannot be empty")
	}

	if len(inputs) > MaxArrayDimensions {
		return fmt.Errorf("input array exceeds max dimensions (%d), actual dimensions: %d",
			MaxArrayDimensions, len(inputs))
	}

	for i, input := range inputs {
		if input == "" {
			if len(inputs) == 1 {
				return errors.New("input cannot be an empty string")
			}
			return fmt.Errorf("input at index %d cannot be an empty string", i)
		}
	}

	return nil
}

// validateTokenInputs validates pre-tokenized inputs (both single token array
// and multiple token arrays).
//
// As in validateStringInputs, the token count is not checked against a context
// length: a single global constant cannot match every served model, and only
// the backend knows the real limit.
//
// MaxArrayDimensions bounds the batch size, matching OpenAI's maxItems for the
// input array. It is deliberately not compared against len(tokens): the number
// of tokens in one input is a context-length question, and capping it at 2048
// would reject a pre-tokenized input that the same model accepts happily when
// sent as a string.
func validateTokenInputs(tokenArrays [][]int64) error {
	if len(tokenArrays) == 0 {
		return errors.New("token arrays cannot be empty")
	}

	if len(tokenArrays) > MaxArrayDimensions {
		return fmt.Errorf("input array exceeds max dimensions (%d), actual dimensions: %d",
			MaxArrayDimensions, len(tokenArrays))
	}

	for i, tokens := range tokenArrays {
		if len(tokens) == 0 {
			if len(tokenArrays) == 1 {
				return errors.New("token array cannot be empty")
			}
			return fmt.Errorf("token array at index %d cannot be empty", i)
		}
	}

	return nil
}
