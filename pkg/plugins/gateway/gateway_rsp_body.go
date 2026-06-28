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

func (s *Server) HandleResponseBody(ctx context.Context, routerCtx *types.RoutingContext, requestID string, req *extProcPb.ProcessingRequest, user utils.User, rpm int64, model string, stream bool, traceTerm int64, hasCompleted bool) (*extProcPb.ProcessingResponse, bool) {
	b := req.Request.(*extProcPb.ProcessingRequest_ResponseBody)
	arrival := time.Now()

	var processingRes *extProcPb.ProcessingResponse
	var promptTokens, completionTokens, totalTokens int64
	var headers []*configPb.HeaderValueOption
	complete := hasCompleted

	// Omitted tracer.Start(ctx, "HandleResponseBody") here to avoid excessive CPU and gRPC overhead.
	// Creating a span for each individual token in the stream is too resource-intensive.

	defer func() {
		// Wrapped in a function to delay the evaluation of parameters. Using complete to make sure DoneRequestTrace only call once for a request.
		if !hasCompleted && complete {
			s.cache.DoneRequestTrace(routerCtx, requestID, model, promptTokens, completionTokens, traceTerm)
			if routerCtx != nil {
				routerCtx.Delete()
			}
		}
	}()

	if stream {
		bodyBytes := b.ResponseBody.GetBody()

		// The previous implementation unmarshalled every single SSE chunk into a struct (openai.ChatCompletionChunk).
		// This caused significant CPU overhead and high GC pressure under heavy concurrency.
		// The new implementation uses zero-allocation  byte scanning and pre-filtering,
		// selectively extracting only the "usage" metadata via gjson for the final chunks.
		if bytes.Contains(bodyBytes, []byte(`"usage"`)) {
			remaining := bodyBytes

			for len(remaining) > 0 {
				var line []byte
				// Manually find the newline to avoid the allocations of bytes.Split
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
						complete = true
						return generateErrorResponse(
							envoyTypePb.StatusCode_InternalServerError,
							[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
								Key: HeaderErrorStreaming, RawValue: []byte("true"),
							}}},
							"malformed JSON in SSE stream", "", ""), complete
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
							promptTokens = pt.Int()
						} else {
							promptTokens = usageResult.Get("input_tokens").Int()
						}
						if ct := usageResult.Get("completion_tokens"); ct.Exists() {
							completionTokens = ct.Int()
						} else {
							completionTokens = usageResult.Get("output_tokens").Int()
						}
						totalTokens = usageResult.Get("total_tokens").Int()
					}
				}
			}
			// warnings when "usage" is triggered by a false positive in generated content.
			if promptTokens == 0 && totalTokens == 0 {
				klog.V(4).Infof("usage string detected but no valid tokens parsed (likely generated text), requestID: %s", requestID)
			}
		}
	} else {
		if isLanguageRequest(routerCtx.ReqPath) {
			processingRes, complete, promptTokens, completionTokens, totalTokens = processLanguageResponse(requestID, b)
			if processingRes != nil {
				return processingRes, complete
			}
		}
	}

	if totalTokens != 0 {
		complete = true

		// Count token per user.
		if user.Name != "" {
			tpm, err := s.ratelimiter.Incr(routerCtx, fmt.Sprintf("%v_TPM_CURRENT", user.Name), totalTokens)
			if err != nil {
				return generateErrorResponse(
					envoyTypePb.StatusCode_InternalServerError,
					[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
						Key: HeaderErrorIncrTPM, RawValue: []byte("true"),
					}}},
					err.Error(), "", ""), complete
			}

			headers = buildEnvoyProxyHeaders(headers,
				HeaderUpdateRPM, strconv.FormatInt(rpm, 10),
				HeaderUpdateTPM, strconv.FormatInt(tpm, 10))
		}

		headers = buildEnvoyProxyHeaders(headers, HeaderRequestID, routerCtx.RequestID)
		fields := s.requestEndHelper(routerCtx, arrival, promptTokens, completionTokens, totalTokens)
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
	}, complete
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
		klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "responseBody", string(b.ResponseBody.GetBody()))
		complete = true
		processingRes = buildErrorResponse(envoyTypePb.StatusCode_InternalServerError, err.Error(), "", "", HeaderErrorResponseUnmarshal, "true")
		return
	}

	if len(res.Model) == 0 {
		msg := ErrorUnknownResponse.Error()
		responseBodyContent := string(b.ResponseBody.GetBody())
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

	if targetPod != nil {
		outstandingRequestCount := math.Max(0, getRunningRequestsByPod(s, targetPod.Name, targetPod.Namespace)-1)
		fields = append(fields,
			"target_pod", targetPod.Name,
			"outstanding_request_count", outstandingRequestCount)
	}

	ttft := arrival.Sub(routingCtx.RequestTime)
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
			kvTransferTime := ttft - routingCtx.PrefillEndTime.Sub(routingCtx.RequestTime)
			decodeTime := time.Since(routingCtx.PrefillEndTime)
			fields = append(fields,
				"prefill_time_taken", prefillTime,
				"kv_transfer_time_taken", kvTransferTime,
				"decode_time_taken", decodeTime,
			)
			metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayPrefillTimeBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": durationBucketLabel(prefillTime)})
			metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayKVTransferTimeBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": durationBucketLabel(kvTransferTime)})
			metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayDecodeTimeBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": durationBucketLabel(decodeTime)})
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
	}
	fields = append(fields, "total_time_taken", routingCtx.Elapsed(time.Now()))
	metrics.EmitMetricToPrometheus(routingCtx, targetPod, metrics.GatewayTotalTimeBucketTotal, &metrics.SimpleMetricValue{Value: 1.0}, map[string]string{"bucket": durationBucketLabel(routingCtx.Elapsed(time.Now()))})
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
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	bounds := []int64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000}
	low := int64(0)
	for _, b := range bounds {
		if ms < b {
			return fmt.Sprintf("%d-%dms", low, b)
		}
		low = b
	}
	return fmt.Sprintf("%dms+", low)
}
