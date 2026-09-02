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
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/tidwall/gjson"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	defaultTTFTThreshold = 1
	// maxStreamBufferSize bounds the trailing partial SSE line carried over between
	// HandleResponseBody calls, so a malformed or malicious upstream that never emits
	// a newline cannot grow streamBuffers without bound.
	maxStreamBufferSize = 1024 * 1024 // 1MB
)

var (
	ttftThreshold = time.Duration(utils.LoadEnvInt("AIBRIX_TTFT_THRESHOLD_S", defaultTTFTThreshold)) * time.Second
)

type OpenAIResponse struct {
	Model string `json:"model"`
	// Usage carries token accounting. The Chat Completions/Completions APIs report
	// prompt_tokens/completion_tokens, while the Responses API (/v1/responses) reports
	// the same two semantic values under input_tokens/output_tokens. Both naming pairs
	// are therefore aliases for the same concepts:
	//   prompt_tokens     == input_tokens   (tokens in the request)
	//   completion_tokens == output_tokens  (tokens generated)
	// Only one pair is populated per response depending on the upstream API. Fields are
	// pointers so an absent field (nil) is distinguishable from a genuine zero count,
	// which lets the prompt/input (and completion/output) fallback select the right alias.
	Usage *struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
		TotalTokens      *int64 `json:"total_tokens"`
		InputTokens      *int64 `json:"input_tokens"`
		OutputTokens     *int64 `json:"output_tokens"`
	} `json:"usage"`
	Code int `json:"code"`
}

// TokenUsage contains the token counts reported by an inference response.
type TokenUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

func processStreamingResponse(requestID string, bodyBytes []byte, endOfStream bool) (TokenUsage, *extProcPb.ProcessingResponse) {
	var usage TokenUsage

	// HandleResponseBody runs once per ext_proc callback, and Envoy delivers response
	// body chunks as TCP data arrives rather than aligned to SSE line boundaries. A
	// "data: {...}" line (and the "usage" field inside it) can therefore be split
	// across two consecutive calls, and the split can land anywhere in the line --
	// not only after "usage" has already appeared. Reassemble any trailing partial
	// line carried over from the previous chunk before scanning.
	if v, ok := streamBuffers.LoadAndDelete(requestID); ok {
		if buf, ok := v.([]byte); ok {
			bodyBytes = append(buf, bodyBytes...)
		}
	}

	// Unless this is the final chunk, carve off any trailing line that isn't yet
	// newline-terminated and carry it over to the next call, regardless of whether
	// it currently contains "usage" -- the "usage" key itself may not have arrived
	// yet. This keeps the scanning below operating only on complete lines, so a
	// chunk boundary landing mid-JSON never gets misreported as malformed JSON.
	// The carried-over tail is capped so a malformed upstream that never emits a
	// newline cannot grow this buffer without bound.
	if !endOfStream {
		if idx := bytes.LastIndexByte(bodyBytes, '\n'); idx >= 0 {
			if tail := bodyBytes[idx+1:]; len(tail) > 0 {
				if len(tail) > maxStreamBufferSize {
					return usage, generateErrorResponse(
						envoyTypePb.StatusCode_InternalServerError,
						[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
							Key: HeaderErrorStreaming, RawValue: []byte("true"),
						}}},
						"buffered SSE line exceeded size limit", "", "")
				}
				streamBuffers.Store(requestID, bytes.Clone(tail))
				bodyBytes = bodyBytes[:idx+1]
			}
		} else if len(bodyBytes) > 0 {
			if len(bodyBytes) > maxStreamBufferSize {
				return usage, generateErrorResponse(
					envoyTypePb.StatusCode_InternalServerError,
					[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
						Key: HeaderErrorStreaming, RawValue: []byte("true"),
					}}},
					"buffered SSE line exceeded size limit", "", "")
			}
			streamBuffers.Store(requestID, bytes.Clone(bodyBytes))
			bodyBytes = nil
		}
	}

	// The previous implementation unmarshalled every single SSE chunk into a struct (openai.ChatCompletionChunk).
	// This caused significant CPU overhead and high GC pressure under heavy concurrency.
	// The new implementation uses zero-allocation  byte scanning and pre-filtering,
	// selectively extracting only the "usage" metadata via gjson for the final chunks.
	if bytes.Contains(bodyBytes, []byte(`"usage"`)) {
		remaining := bodyBytes

		for len(remaining) > 0 {
			var line []byte
			// Manually find the newline to avoid the allocations of bytes.Split.
			// Every line here is guaranteed complete: bodyBytes was trimmed to a
			// newline boundary above unless this is the final chunk, in which case
			// a trailing line with no newline is legitimately the last line.
			if idx := bytes.IndexByte(remaining, '\n'); idx >= 0 {
				line = remaining[:idx]
				remaining = remaining[idx+1:]
			} else {
				line = remaining
				remaining = nil
			}

			// Handle SSE \r\n line endings. bytes.TrimSpace safely strips trailing \r
			// as well as any leading/trailing whitespace.
			line = bytes.TrimSpace(line)

			// Look for the SSE data prefix
			if bytes.HasPrefix(line, []byte("data:")) {
				// Slice the "data:" prefix (zero allocation)
				jsonBytes := bytes.TrimSpace(line[5:])

				// Check for the end of the stream
				if bytes.Equal(jsonBytes, []byte("[DONE]")) {
					continue
				}

				// While gjson.ValidBytes is O(N), it does not degrade gateway throughput.
				// Guarded by the bytes.Contains pre-filter, it bypasses the hot path of streaming standard text
				// and only executes on final chunks, ensuring strict correctness.
				if !gjson.ValidBytes(jsonBytes) {
					return usage, generateErrorResponse(
						envoyTypePb.StatusCode_InternalServerError,
						[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
							Key: HeaderErrorStreaming, RawValue: []byte("true"),
						}}},
						"malformed JSON in SSE stream", "", "")
				}

				// gjson avoids full deserialization by only extracting the usage field.
				usageResult := gjson.GetBytes(jsonBytes, "usage")
				if !usageResult.Exists() {
					// The Responses API (/v1/responses) emits usage nested inside the
					// terminal "response.completed" SSE event, where the full response
					// object (including its "usage" field) lives under "response".
					// Hence the usage path there is "response.usage".
					usageResult = gjson.GetBytes(jsonBytes, "response.usage")
				}
				if usageResult.Exists() && usageResult.IsObject() {
					// Assumption: The upstream sends the usage object only in the final chunk
					// (standard vLLM/OpenAI behavior). We overwrite/set the values here.
					// The Responses API uses input_tokens/output_tokens instead of
					// prompt_tokens/completion_tokens, so fall back to those names only when
					// the primary field is genuinely absent (Exists() == false), since a
					// zero count is a semantically valid value.
					if pt := usageResult.Get("prompt_tokens"); pt.Exists() {
						usage.PromptTokens = pt.Int()
					} else {
						usage.PromptTokens = usageResult.Get("input_tokens").Int()
					}
					if ct := usageResult.Get("completion_tokens"); ct.Exists() {
						usage.CompletionTokens = ct.Int()
					} else {
						usage.CompletionTokens = usageResult.Get("output_tokens").Int()
					}
					usage.TotalTokens = usageResult.Get("total_tokens").Int()
				}
			}
		}
		// warnings when "usage" is triggered by a false positive in generated content.
		if usage.PromptTokens == 0 && usage.TotalTokens == 0 {
			klog.V(4).Infof("usage string detected but no valid tokens parsed (likely generated text), requestID: %s", requestID)
		}
	}

	return usage, nil
}

// HandleResponseBody parses and accounts for a response body but deliberately
// does not finalize cache bookkeeping or release routerCtx. Process owns that
// lifecycle and keeps the context valid until its final deferred cleanup.
func (s *Server) HandleResponseBody(ctx context.Context, routerCtx *types.RoutingContext, requestID string, req *extProcPb.ProcessingRequest, user utils.User, rpm int64, model string, stream bool, hasCompleted bool) (*extProcPb.ProcessingResponse, bool, TokenUsage) {
	if routerCtx == nil {
		return generateErrorResponse(
			envoyTypePb.StatusCode_InternalServerError,
			nil,
			"routing context is nil", "", ""), true, TokenUsage{}
	}
	b := req.Request.(*extProcPb.ProcessingRequest_ResponseBody)
	arrival := time.Now()

	// Record the arrival time of the first response body chunk. For streaming
	// responses HandleResponseBody runs once per SSE chunk, but request_end
	// metrics are only emitted on the final chunk. Without capturing the first
	// arrival here, TTFT and KV-transfer time would be measured from the last
	// chunk (≈ total request time) instead of the first token.
	if stream && routerCtx.FirstTokenTime.IsZero() {
		routerCtx.FirstTokenTime = arrival
	}

	var processingRes *extProcPb.ProcessingResponse
	var usage TokenUsage
	var headers []*configPb.HeaderValueOption
	complete := hasCompleted

	// Omitted tracer.Start(ctx, "HandleResponseBody") here to avoid excessive CPU and gRPC overhead.
	// Creating a span for each individual token in the stream is too resource-intensive.

	if stream {
		var streamRes *extProcPb.ProcessingResponse
		usage, streamRes = processStreamingResponse(requestID, b.ResponseBody.GetBody(), b.ResponseBody.EndOfStream)
		if streamRes != nil {
			complete = true
			return streamRes, complete, usage
		}
	} else {
		if isLanguageRequest(routerCtx.ReqPath) {
			processingRes, complete, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens = processLanguageResponse(requestID, b)
			if processingRes != nil {
				return processingRes, complete, usage
			}
		}
	}

	if usage.TotalTokens != 0 {
		complete = true

		// Count token per user.
		if user.Name != "" {
			tpm, err := s.ratelimiter.Incr(routerCtx, fmt.Sprintf("%v_TPM_CURRENT", user.Name), usage.TotalTokens)
			if err != nil {
				return generateErrorResponse(
					envoyTypePb.StatusCode_InternalServerError,
					[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
						Key: HeaderErrorIncrTPM, RawValue: []byte("true"),
					}}},
					err.Error(), "", ""), complete, usage
			}

			headers = buildEnvoyProxyHeaders(headers,
				HeaderUpdateRPM, strconv.FormatInt(rpm, 10),
				HeaderUpdateTPM, strconv.FormatInt(tpm, 10))
		}

		headers = buildEnvoyProxyHeaders(headers, HeaderRequestID, routerCtx.RequestID)
		// arrival is this (final) chunk's arrival time, i.e. the true request-end time.
		// requestEndHelper derives TTFT from routerCtx.FirstTokenTime for streaming
		// requests, so passing the final arrival here (rather than the first chunk's)
		// keeps decode-time/KV-transfer math, which spans first-token-to-end, correct.
		fields := s.requestEndHelper(routerCtx, arrival, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
		if routerCtx.Span != nil {
			routerCtx.Span.SetAttributes(fieldsToAttributes(fields)...)
		}
		klog.InfoS("request_end", fields...)
	} else if b.ResponseBody.EndOfStream {
		complete = true
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseBody{
			ResponseBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
				},
			},
		},
	}, complete, usage
}

func isLanguageRequest(requestPath string) bool {
	nonLanguagePrefixes := []string{
		PathImagesGenerations,
		PathVideoGenerations,
		PathAudioTranscriptions,
		PathAudioTranslations,
	}
	for _, prefix := range nonLanguagePrefixes {
		if strings.HasPrefix(requestPath, prefix) {
			return false
		}
	}
	return true
}

// processLanguageResponse processes output response for /chatcompletions, /completions, /responses and /embedding endpoints.
// nolint:nakedret
func processLanguageResponse(requestID string, b *extProcPb.ProcessingRequest_ResponseBody) (processingRes *extProcPb.ProcessingResponse, complete bool, promptTokens, completionTokens, totalTokens int64) {
	var res *OpenAIResponse
	// Use request ID as a key to store per-request buffer
	// Retrieve or create buffer
	buf, _ := requestBuffers.LoadOrStore(requestID, &bytes.Buffer{})
	buffer := buf.(*bytes.Buffer)
	// Append data to per-request buffer
	buffer.Write(b.ResponseBody.Body)

	if !b.ResponseBody.EndOfStream {
		// Partial data received, wait for more chunks, we just return a common response here.
		processingRes = &extProcPb.ProcessingResponse{
			Response: &extProcPb.ProcessingResponse_ResponseBody{
				ResponseBody: &extProcPb.BodyResponse{
					Response: &extProcPb.CommonResponse{},
				},
			},
		}
		return
	}

	// Last part received, process the full response
	finalBody := buffer.Bytes()
	// Clean up the buffer after final processing
	requestBuffers.Delete(requestID)

	if err := sonic.Unmarshal(finalBody, &res); err != nil {
		klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "responseBody", string(finalBody))
		complete = true
		processingRes = buildErrorResponse(envoyTypePb.StatusCode_InternalServerError, err.Error(), "", "", HeaderErrorResponseUnmarshal, "true")
		return
	}

	if len(res.Model) == 0 {
		// The body is not a recognized OpenAI response. Normalize any upstream error payload
		// (nested {"error":{...}} or the flat shape) to a single envelope and pass it through;
		// headerStatus == 0 because this 200 path carries a fake-success status, so the real
		// status is derived from the body's numeric code (see upstreamErrorHTTPStatus).
		if body, code, ok := normalizeUpstreamErrorBody(finalBody, 0); ok {
			klog.ErrorS(ErrorUnknownResponse, "unexpected response", "requestID", requestID, "responseBody", string(finalBody))
			complete = true
			errHeaders := buildEnvoyProxyHeaders(nil, HeaderErrorResponseUnknown, "true")
			processingRes = buildErrorResponseWithBody(code, body, errHeaders)
			return
		}

		// Fallback: the body is not a recognizable error payload (e.g. non-JSON or an
		// arbitrary object). Wrap the reassembled finalBody as the error message rather
		// than the current chunk, so multi-chunk error bodies are preserved in full.
		msg := ErrorUnknownResponse.Error()
		responseBodyContent := string(finalBody)
		if len(responseBodyContent) != 0 {
			msg = responseBodyContent
		}
		klog.ErrorS(ErrorUnknownResponse, "unexpected response", "requestID", requestID, "responseBody", responseBodyContent)

		code := envoyTypePb.StatusCode_InternalServerError
		if res.Code >= 100 && res.Code < 600 {
			code = envoyTypePb.StatusCode(res.Code)
		}

		complete = true
		processingRes = buildErrorResponse(code, msg, "", "", HeaderErrorResponseUnknown, "true")
		return
	}

	if res.Usage != nil {
		// Prefer the prompt/completion names; fall back to the Responses API's
		// input/output aliases only when the primary field is genuinely absent
		// (nil), not merely zero.
		if res.Usage.PromptTokens != nil {
			promptTokens = *res.Usage.PromptTokens
		} else if res.Usage.InputTokens != nil {
			promptTokens = *res.Usage.InputTokens
		}
		if res.Usage.CompletionTokens != nil {
			completionTokens = *res.Usage.CompletionTokens
		} else if res.Usage.OutputTokens != nil {
			completionTokens = *res.Usage.OutputTokens
		}
		if res.Usage.TotalTokens != nil {
			totalTokens = *res.Usage.TotalTokens
		}
	}
	return
}

// normalizeUpstreamErrorBody normalizes an upstream engine's error payload to the nested
// {"error": {...}} shape. It recognizes both the nested form ({"error": {...}}) and the
// flat form where the error fields sit at the top level. It returns the normalized body,
// the HTTP status to use for the response, and whether the body was a recognizable error
// payload (false for invalid JSON, a non-object "error" field, or for flat payloads:
// missing message). For nested payloads, a missing message falls back to a generic error
// string rather than rejecting the payload.
//
// headerStatus is the upstream HTTP status the gateway observed (> 0 for a real non-200
// :status, 0 when the 200 header is a fake success over an error body). The HTTPS-status
// precedence rule (header wins over body code, semantic string code only preserved) lives in
// upstreamErrorHTTPStatus.
func normalizeUpstreamErrorBody(body []byte, headerStatus int) (string, envoyTypePb.StatusCode, bool) {
	if !gjson.ValidBytes(body) {
		return "", 0, false
	}

	nested := gjson.GetBytes(body, "error")
	if nested.Exists() && nested.IsObject() {
		return renderErrorBody(nested), upstreamErrorHTTPStatus(nested, headerStatus), true
	}

	// Flat shape: the error fields sit at the top level. Require a JSON object with a
	// message field so ordinary (non-error) responses such as arrays or primitives are
	// not misclassified.
	flat := gjson.ParseBytes(body)
	if !flat.IsObject() || !flat.Get("message").Exists() {
		return "", 0, false
	}
	return renderErrorBody(flat), upstreamErrorHTTPStatus(flat, headerStatus), true
}

// renderErrorBody renders the normalized {"error": {...}} JSON body from a gjson Result
// representing either the nested error object or the flat error object. Fields absent from
// the upstream payload are emitted as JSON null, except message which falls back to a
// generic error string. The "code" field preserves whatever type the upstream supplied
// (integer HTTP status, string semantic code, or null) verbatim.
func renderErrorBody(e gjson.Result) string {
	obj := map[string]interface{}{}

	if m := e.Get("message"); m.Exists() && m.Type != gjson.Null {
		obj["message"] = m.String()
	} else {
		obj["message"] = ErrorUnknownResponse.Error()
	}

	if t := e.Get("type"); t.Exists() && t.Type != gjson.Null {
		obj["type"] = t.String()
	} else {
		obj["type"] = nil
	}

	if p := e.Get("param"); p.Exists() && p.Type != gjson.Null {
		obj["param"] = p.Value()
	} else {
		obj["param"] = nil
	}

	if c := e.Get("code"); c.Exists() && c.Type != gjson.Null {
		obj["code"] = c.Value()
	} else {
		obj["code"] = nil
	}

	body, err := sonic.Marshal(map[string]interface{}{"error": obj})
	if err != nil {
		klog.ErrorS(err, "failed to marshal upstream error body")
		return `{"error":{"message":"internal server error while formatting error response","type":"api_error","code":null,"param":null}}`
	}
	return string(body)
}

// upstreamErrorHTTPStatus derives the envoy HTTP status for an upstream error payload.
// Precedence (unified across both response paths via headerStatus):
//
//  1. headerStatus > 0 (a real non-200 :status): use it — authoritative, and the body's own
//     "code" must not override a genuine transport status.
//  2. headerStatus == 0 (a 200 body that smuggled an error): the 200 header is a fake success,
//     so derive the status from the body's numeric "code" (stringified integers like "400" too)
//     — this stops clients getting HTTP 200 wrapping an error body.
//  3. A non-numeric semantic "code" (e.g. "invalid_api_key") never derives the status; it is
//     only preserved verbatim in the body. Residual case with no usable status: return 500.
//
// Callers MUST pass the real observed header status (respErrorCode on the non-200 path, 0 on
// the 200 path) rather than re-deriving it, so the rule stays in one place.
func upstreamErrorHTTPStatus(e gjson.Result, headerStatus int) envoyTypePb.StatusCode {
	if headerStatus > 0 {
		return envoyTypePb.StatusCode(headerStatus)
	}
	c := e.Get("code")
	if c.Exists() && c.Type == gjson.Number {
		if v := c.Int(); v >= 100 && v < 600 {
			return envoyTypePb.StatusCode(v)
		}
	}
	// Some upstream engines or intermediary proxies stringify the status code.
	if c.Exists() && c.Type == gjson.String {
		if v, err := strconv.Atoi(c.String()); err == nil {
			if v >= 100 && v < 600 {
				return envoyTypePb.StatusCode(v)
			}
		}
	}
	return envoyTypePb.StatusCode_InternalServerError
}

func (s *Server) requestEndHelper(routingCtx *types.RoutingContext, arrival time.Time,
	promptTokens, completionTokens, totalTokens int64) []interface{} {
	requestID := routingCtx.RequestID
	model := routingCtx.Model
	var targetPod *v1.Pod
	if routingCtx.HasRouted() {
		targetPod = routingCtx.TargetPod()
	}

	fields := []interface{}{
		"request_id", requestID,
		"model_name", model,
		"prompt_tokens", promptTokens,
		"completion_tokens", completionTokens,
		"total_tokens", totalTokens,
	}
	pBucket := tokenBucketLabel(promptTokens)
	cBucket := tokenBucketLabel(completionTokens)
	metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayPromptTokenBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": pBucket})
	metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayCompletionTokenBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": cBucket})
	metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayInputTokensTotal, &metrics.SimpleMetricValue{Value: float64(promptTokens)}, nil)
	metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayOutputTokensTotal, &metrics.SimpleMetricValue{Value: float64(completionTokens)}, nil)
	metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayRequestsWithUsageTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"has_usage": "true"})

	if targetPod != nil {
		outstandingRequestCount := math.Max(0, getRunningRequestsByPod(s, targetPod.Name, targetPod.Namespace)-1)
		fields = append(fields,
			"target_pod", targetPod.Name,
			"outstanding_request_count", outstandingRequestCount)
	}

	ttft := arrival.Sub(routingCtx.RequestTime)
	if routingCtx.Stream && !routingCtx.FirstTokenTime.IsZero() {
		ttft = routingCtx.FirstTokenTime.Sub(routingCtx.RequestTime)
	}
	if routingCtx.Stream {
		ttftBucket := durationBucketLabel(ttft)
		metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayTTFTBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": ttftBucket})
	}

	if routingCtx.Algorithm == "pd" {
		routingTime := routingCtx.PrefillStartTime.Sub(routingCtx.RequestTime)
		fields = append(fields,
			"routing_time_taken", routingTime,
			"ttft", ttft,
		)
		metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayRoutingTimeBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": durationBucketLabel(routingTime)})
		if !routingCtx.PrefillEndTime.IsZero() {
			prefillTime := routingCtx.PrefillEndTime.Sub(routingCtx.PrefillStartTime)
			// KV transfer: time from prefill HTTP completion to first decode token.
			kvTransferTime := ttft - routingCtx.PrefillEndTime.Sub(routingCtx.RequestTime)
			// Decode generation: time from first token to request end. Do not use
			// PrefillEndTime here — that interval includes KV transfer and would
			// double-count kv_transfer_time_taken when summing phase latencies.
			decodeTime := arrival.Sub(routingCtx.PrefillEndTime)
			if routingCtx.Stream && !routingCtx.FirstTokenTime.IsZero() {
				decodeTime = arrival.Sub(routingCtx.FirstTokenTime)
			}
			fields = append(fields,
				"prefill_time_taken", prefillTime,
				"kv_transfer_time_taken", kvTransferTime,
				"decode_time_taken", decodeTime,
			)
			metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayPrefillTimeBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": durationBucketLabel(prefillTime)})
			metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayKVTransferTimeBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": durationBucketLabel(kvTransferTime)})
			metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayDecodeTimeBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": durationBucketLabel(decodeTime)})
			if routingCtx.Stream && completionTokens > 0 && decodeTime > 0 {
				tpot := time.Duration(decodeTime.Nanoseconds() / completionTokens)
				metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayTPOTBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": durationBucketLabel(tpot)})
			}
			if ttft > ttftThreshold {
				metrics.EmitMetricToPrometheus(routingCtx, nil, metrics.GatewayFirstTokenDelayOver1sTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{
					"request_id": requestID,
					"p_bucket":   pBucket, "c_bucket": cBucket,
					"routing_time_taken":     fmt.Sprintf("%v", routingTime),
					"prefill_time_taken":     fmt.Sprintf("%v", prefillTime),
					"kv_transfer_time_taken": fmt.Sprintf("%v", kvTransferTime),
					"ttft":                   fmt.Sprintf("%v", ttft),
					"decode_time_taken":      fmt.Sprintf("%v", decodeTime),
				})
			}
		}
	} else if routingCtx.Algorithm != "" {
		fields = append(fields, "routing_time_taken", routingCtx.GetRoutingDelay())
		if routingCtx.Stream && completionTokens > 0 {
			decodeTimeApprox := routingCtx.Elapsed(time.Now()) - ttft
			if decodeTimeApprox > 0 {
				tpot := time.Duration(decodeTimeApprox.Nanoseconds() / completionTokens)
				metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayTPOTBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": durationBucketLabel(tpot)})
			}
		}
	}
	fields = append(fields, "total_time_taken", routingCtx.Elapsed(time.Now()))
	metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayTotalTimeBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": totalTimeBucketLabel(routingCtx.Elapsed(time.Now()))})
	return fields
}

// tokenBucketLabel returns a human-readable bucket label for token counts.
// Buckets: [0-256), [256-512), [512-1024), [1024-2048), [2048-4096), [4096-8192), [8192-16384), [16384-32768), [32768+]
func tokenBucketLabel(n int64) string {
	bounds := []int64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768}
	low := int64(0)
	for _, b := range bounds {
		if n < b {
			return fmt.Sprintf("%d-%d", low, b)
		}
		low = b
	}
	return fmt.Sprintf("%d+", low)
}

// Add duration bucketizer: ms buckets [0-1), [1-2), [2-5), [5-10), [20-50), [50-100), [100-200), [200-500), [500-1000), [1000-2000), [2000-5000), [5000+}
func durationBucketLabel(d time.Duration) string {
	return msBucketLabel(d.Milliseconds(), []int64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000})
}

// totalTimeBucketLabel buckets end-to-end request latency with coarser windows suited to full request duration.
// Buckets: [0-100), [100-250), [250-500), [500-1000), [1000-5000), [5000-20000), [20000-60000), [60000+)
func totalTimeBucketLabel(d time.Duration) string {
	return msBucketLabel(d.Milliseconds(), []int64{100, 250, 500, 1000, 5000, 20000, 60000})
}

func msBucketLabel(ms int64, bounds []int64) string {
	if ms < 0 {
		ms = 0
	}
	low := int64(0)
	for _, b := range bounds {
		if ms < b {
			return fmt.Sprintf("%d-%dms", low, b)
		}
		low = b
	}
	return fmt.Sprintf("%dms+", low)
}
