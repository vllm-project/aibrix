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

// This file contains the pdRouter methods responsible for executing prefill HTTP
// requests in disaggregated-inference mode. The three concerns handled here are:
//
//  1. Payload preparation: transforming the incoming chat/completion body into
//     an engine-specific prefill payload (SGLang bootstrap fields, vLLM
//     kv_transfer_params, TensorRT-LLM disaggregated_params).
//
//  2. HTTP execution: posting the payload to the selected prefill pod and
//     reading the response, with shared-client connection pooling and
//     per-request timeout derived from AIBRIX_PREFILL_REQUEST_TIMEOUT.
//
//  3. Response injection: merging the prefill response back into the decode
//     request body so that the decode pod receives all KV-transfer metadata
//     (kv_transfer_params for vLLM/SHFS, disagg_prefill_resp for vLLM/NIXL,
//     disaggregated_params for TensorRT-LLM).

package routingalgorithms

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/transfer"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// doPrefillRequest sends the prefill phase of a disaggregated-inference request
// to prefillPod and blocks until the prefill is complete (vLLM, TRT-LLM, and
// unknown engines) or fires it asynchronously (SGLang).
//
// After the payload is built by preparePrefillPayload, the method resolves the
// effective score-policy names purely for structured logging, then hands off to
// the engine-specific branch:
//
//   - SGLangEngine: fires the request in a goroutine and returns immediately,
//     because SGLang's bootstrap handshake is self-coordinating.
//   - VLLMEngine: calls handleSyncPrefill → updateRoutingContextWithKVTransferParams
//     to inject kv_transfer_params into the decode request body.
//   - TensorRTLLM: calls handleSyncPrefill → updateRoutingContextWithTRTDisaggParams
//     to inject disaggregated_params into the decode request body.
//   - default: synchronous, no response processing.
func (r *pdRouter) doPrefillRequest(routingCtx *types.RoutingContext, prefillPod *v1.Pod, llmEngine string) error {
	payload, err := r.preparePrefillPayload(routingCtx, prefillPod, llmEngine)
	if err != nil {
		return fmt.Errorf("failed to prepare prefill payload for request %s: %w", routingCtx.RequestID, err)
	}

	apiURL := fmt.Sprintf("http://%s:%d%s",
		prefillPod.Status.PodIP,
		utils.GetModelPortForPod(routingCtx.RequestID, prefillPod),
		routingCtx.ReqPath)

	// Resolve effective policy names for logging only; errors are non-fatal here
	// because the policies were already applied during pod selection.
	prefillPol, decodePol, polErr := r.effectiveScorePolicies(routingCtx)
	prefillScorePolicyName := pd.PrefillScorePolicyPrefixCache
	if r.prefillPolicy != nil {
		prefillScorePolicyName = r.prefillPolicy.Name()
	}
	if polErr == nil && prefillPol != nil {
		prefillScorePolicyName = prefillPol.Name()
	}

	decodeScorePolicyName := string(pd.DecodePolicyLoadBalancing)
	if r.decodePolicy != nil {
		decodeScorePolicyName = string(r.decodePolicy.Name())
	}
	if polErr == nil && decodePol != nil {
		decodeScorePolicyName = string(decodePol.Name())
	}

	fields := []interface{}{
		"request_id", routingCtx.RequestID,
		"llm_engine", llmEngine,
		"model_name", routingCtx.Model,
		"prefill_pod", prefillPod.Name,
		"prefill_url", apiURL,
		"prefill_score_policy", prefillScorePolicyName,
		"decode_score_policy", decodeScorePolicyName,
		"outstanding_prefill_requests", r.prefillRequestTracker.GetPrefillRequestCountsForPod(prefillPod.Name),
	}
	klog.InfoS("prefill_request_start", fields...)
	// Drop the last two fields (outstanding count) before passing fields to the
	// completion log; the count is re-fetched after the request finishes.
	if len(fields) >= 2 {
		fields = fields[:len(fields)-2]
	}

	r.prefillRequestTracker.AddPrefillRequest(routingCtx.RequestID, prefillPod.Name)
	routingCtx.PrefillStartTime = time.Now()

	handler := engine.Resolve(llmEngine)

	if handler.IsAsync() {
		// SGLang uses a bootstrap handshake to coordinate KV transfer out-of-band;
		// fire asynchronously and return immediately.
		go func() {
			defer r.prefillRequestTracker.RemovePrefillRequest(routingCtx.RequestID)

			if _, err := r.executeHTTPRequest(apiURL, routingCtx, payload); err != nil {
				klog.ErrorS(err, "prefill_request_failed",
					"request_id", routingCtx.RequestID,
					"llm_engine", llmEngine,
					"prefill_pod", prefillPod.Name,
					"prefill_pod_ip", prefillPod.Status.PodIP,
					"elapsed", routingCtx.Elapsed(time.Now()))
				return
			}

			routingCtx.PrefillEndTime = time.Now()
			fields = append(fields,
				"routing_time_taken", routingCtx.PrefillStartTime.Sub(routingCtx.RequestTime),
				"prefill_time_taken", routingCtx.PrefillEndTime.Sub(routingCtx.PrefillStartTime),
				"outstanding_prefill_requests", r.prefillRequestTracker.GetPrefillRequestCountsForPod(prefillPod.Name)-1)
			klog.InfoS("prefill_request_end", fields...)
		}()
		return nil
	}

	return r.handleSyncPrefill(routingCtx, prefillPod, llmEngine, apiURL, payload, fields, handler.MergePrefillResponse, llmEngine+" response")
}

// handleSyncPrefill executes a synchronous HTTP prefill request and optionally
// post-processes the response via updateCtxFunc. Pass nil for updateCtxFunc
// when no response processing is needed (e.g. unknown engines).
//
// The prefill tracker entry is removed via defer regardless of outcome.
func (r *pdRouter) handleSyncPrefill(
	routingCtx *types.RoutingContext,
	prefillPod *v1.Pod,
	llmEngine, apiURL string,
	payload []byte,
	fields []interface{},
	updateCtxFunc func(*types.RoutingContext, map[string]any, *v1.Pod) error,
	errorContext string) error {
	defer r.prefillRequestTracker.RemovePrefillRequest(routingCtx.RequestID)

	responseData, err := r.executeHTTPRequest(apiURL, routingCtx, payload)
	if err != nil {
		klog.ErrorS(err, "prefill_request_failed",
			"request_id", routingCtx.RequestID,
			"llm_engine", llmEngine,
			"prefill_pod", prefillPod.Name,
			"prefill_pod_ip", prefillPod.Status.PodIP,
			"elapsed", routingCtx.Elapsed(time.Now()))
		return fmt.Errorf("prefill request failed for request %s, pod %s: %w", routingCtx.RequestID, prefillPod.Name, err)
	}

	if updateCtxFunc != nil {
		if err := updateCtxFunc(routingCtx, responseData, prefillPod); err != nil {
			return fmt.Errorf("failed to update routing context with %s for request %s: %w", errorContext, routingCtx.RequestID, err)
		}
	}

	routingCtx.PrefillEndTime = time.Now()
	fields = append(fields,
		"routing_time_taken", routingCtx.PrefillStartTime.Sub(routingCtx.RequestTime),
		"prefill_time_taken", routingCtx.PrefillEndTime.Sub(routingCtx.PrefillStartTime),
		"outstanding_prefill_requests", r.prefillRequestTracker.GetPrefillRequestCountsForPod(prefillPod.Name)-1)
	klog.InfoS("prefill_request_end", fields...)
	return nil
}

// preparePrefillPayload transforms routingCtx.ReqBody into a prefill-specific
// payload for the given llmEngine. The transformations applied are:
//
//   - SGLangEngine: adds bootstrap_host, bootstrap_port, and a random
//     bootstrap_room; also updates routingCtx.ReqBody so the decode request
//     carries the same bootstrap fields.
//   - VLLMEngine (SHFS mode): adds a kv_transfer_params skeleton with
//     do_remote_decode=true; the decode node fills in the remote block IDs
//     after the prefill completes.
//   - TensorRTLLM: adds disaggregated_params with request_type="context_only"
//     and a unique disagg_request_id generated from the machine snowflake ID.
//
// All engines: sets max_tokens=1, max_completion_tokens=1 (except TRT-LLM),
// stream=false, and removes stream_options so the prefill pod returns a single
// JSON response rather than an SSE stream.
func (r *pdRouter) preparePrefillPayload(routingCtx *types.RoutingContext, pod *v1.Pod, llmEngine string) ([]byte, error) {
	var completionRequest map[string]any
	if err := sonic.Unmarshal(routingCtx.ReqBody, &completionRequest); err != nil {
		return nil, fmt.Errorf("failed to unmarshal prefill request body: %w", err)
	}

	// Delegate all engine-specific request augmentation to the engine handler.
	handler := engine.Resolve(llmEngine)
	if err := handler.AugmentPrefillRequest(routingCtx, pod, completionRequest); err != nil {
		return nil, fmt.Errorf("failed to augment prefill request for %s: %w", llmEngine, err)
	}

	// Constrain the prefill to a single token so the pod returns immediately
	// after completing the KV-cache fill without generating any real output.
	completionRequest["max_tokens"] = 1
	if llmEngine == TensorRTLLM {
		// TRT-LLM uses max_tokens only; max_completion_tokens is not supported.
		delete(completionRequest, "max_completion_tokens")
	} else {
		completionRequest["max_completion_tokens"] = 1
	}
	completionRequest["stream"] = false
	delete(completionRequest, "stream_options")
	delete(completionRequest, "min_tokens")

	return sonic.Marshal(completionRequest)
}

// executeHTTPRequest posts payload to url using the router's shared HTTP client
// and returns the parsed JSON response body. The request carries a per-request
// timeout derived from prefillRequestTimeout and all headers from routingCtx.
//
// Non-200 responses and transport errors are both reported to Prometheus via
// GatewayPrefillRequestFailTotal before being returned as errors.
// TRT-LLM responses are parsed with UseInt64=true to avoid float64 precision
// loss on large integer fields such as disagg_request_id.
func (r *pdRouter) executeHTTPRequest(url string, routingCtx *types.RoutingContext, payload []byte) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(routingCtx.Context, time.Duration(prefillRequestTimeout)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create http prefill request: %w", err)
	}

	for key, value := range routingCtx.ReqHeaders {
		req.Header.Set(key, value)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Request-Id", routingCtx.RequestID)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		status, code := metrics.HttpFailureStatusCode(ctx, err, nil)
		metrics.EmitMetricToPrometheus(routingCtx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": status, "status_code": code})
		return nil, fmt.Errorf("failed to execute http prefill request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read prefill response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		status, code := metrics.HttpFailureStatusCode(ctx, nil, resp)
		metrics.EmitMetricToPrometheus(routingCtx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": status, "status_code": code})
		return nil, fmt.Errorf("http prefill request failed with status %d: %s", resp.StatusCode, string(body))
	}

	// TRT-LLM prefill responses contain large integer IDs in disaggregated_params;
	// use UseInt64 to avoid float64 precision loss during unmarshal.
	var responseData map[string]any
	var errUnmarshal error
	if routingCtx.Engine == TensorRTLLM {
		errUnmarshal = sonicJSONInt64.Unmarshal(body, &responseData)
	} else {
		errUnmarshal = sonic.Unmarshal(body, &responseData)
	}
	if errUnmarshal != nil {
		return nil, fmt.Errorf("failed to unmarshal prefill response: %w", errUnmarshal)
	}

	return responseData, nil
}

// updateRoutingContextWithKVTransferParams delegates to the vLLM engine handler's
// MergePrefillResponse so existing tests can call this method directly.
func (r *pdRouter) updateRoutingContextWithKVTransferParams(routingCtx *types.RoutingContext, responseData map[string]any, prefillPod *v1.Pod) error {
	return engine.Resolve(VLLMEngine).MergePrefillResponse(routingCtx, responseData, prefillPod)
}

// updateRoutingContextWithTRTDisaggParams delegates to the TRT-LLM engine handler's
// MergePrefillResponse so existing tests can call this method directly.
func (r *pdRouter) updateRoutingContextWithTRTDisaggParams(routingCtx *types.RoutingContext, responseData map[string]any, prefillPod *v1.Pod) error {
	return engine.Resolve(TensorRTLLM).MergePrefillResponse(routingCtx, responseData, prefillPod)
}

// selectKvConnectorType resolves the KV connector type from a pod label value,
// falling back to aibrixKVConnectorType when the value is absent or unrecognized.
// Delegates to transfer.ResolveConnectorType for consistent resolution logic.
func selectKvConnectorType(value string) string {
	return transfer.ResolveConnectorType(value, aibrixKVConnectorType)
}
