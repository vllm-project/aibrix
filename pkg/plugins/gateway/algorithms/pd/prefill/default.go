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

package prefill

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"time"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// trtllmEngine is kept local to avoid importing routingalgorithms (circular).
const (
	trtllmEngine                = "trtllm"
	prefillRequestSuccessStatus = "pd-prefill-request-success"
)

// DefaultExecutor is the standard PrefillExecutor. It owns the HTTP client and
// the prefill-request tracker so that prefill logic can be tested in isolation
// from the routing/scoring concerns in pdRouter.
type DefaultExecutor struct {
	httpClient     *http.Client
	tracker        *pd.PrefillRequestTracker
	requestTimeout int // seconds
}

// NewDefaultExecutor constructs a DefaultExecutor.
// httpClient and tracker are shared with the router; requestTimeout is in seconds.
func NewDefaultExecutor(httpClient *http.Client, tracker *pd.PrefillRequestTracker, requestTimeout int) PrefillExecutor {
	return &DefaultExecutor{
		httpClient:     httpClient,
		tracker:        tracker,
		requestTimeout: requestTimeout,
	}
}

// Execute implements PrefillExecutor.
func (e *DefaultExecutor) Execute(routingCtx *types.RoutingContext, prefillPod *v1.Pod, llmEngine string, logCtx LogContext) error {
	handler := engine.Resolve(llmEngine)
	payload, err := PreparePayload(routingCtx, prefillPod, llmEngine, handler)
	if err != nil {
		return fmt.Errorf("failed to prepare prefill payload for request %s: %w", routingCtx.RequestID, err)
	}

	apiURL := fmt.Sprintf("http://%s:%d%s",
		prefillPod.Status.PodIP,
		utils.GetModelPortForPod(routingCtx.RequestID, prefillPod),
		routingCtx.ReqPath)

	fields := []interface{}{
		"request_id", routingCtx.RequestID,
		"llm_engine", llmEngine,
		"model_name", routingCtx.Model,
		"prefill_pod", prefillPod.Name,
		"prefill_url", apiURL,
		"prefill_score_policy", logCtx.PrefillScorePolicy,
		"decode_score_policy", logCtx.DecodeScorePolicy,
		"outstanding_prefill_requests", e.tracker.GetPrefillRequestCountsForPod(prefillPod.Name),
	}
	klog.InfoS("prefill_request_start", fields...)
	// Copy and drop the last two fields (outstanding count) before passing to the
	// completion log; the count is re-fetched after the request finishes. A copy
	// avoids aliasing the underlying array when append is called later.
	var completionFields []interface{}
	if len(fields) >= 2 {
		completionFields = append([]interface{}{}, fields[:len(fields)-2]...)
	} else {
		completionFields = append([]interface{}{}, fields...)
	}

	e.tracker.AddPrefillRequest(routingCtx.RequestID, prefillPod.Name)
	routingCtx.PrefillStartTime = time.Now()

	if handler.IsAsync() {
		// SGLang uses a bootstrap handshake to coordinate KV transfer out-of-band;
		// fire asynchronously and return immediately.
		requestID := routingCtx.RequestID
		requestTime := routingCtx.RequestTime
		prefillStartTime := routingCtx.PrefillStartTime
		prefillPodName := prefillPod.Name
		prefillPodIP := prefillPod.Status.PodIP
		asyncCtx := &types.RoutingContext{
			Context:     context.WithoutCancel(routingCtx.Context),
			RequestID:   requestID,
			Model:       routingCtx.Model,
			Engine:      routingCtx.Engine,
			RequestTime: requestTime,
			ReqHeaders:  maps.Clone(routingCtx.ReqHeaders),
		}
		go func() {
			defer e.tracker.RemovePrefillRequest(requestID)

			if _, err := e.executeHTTP(apiURL, asyncCtx, payload); err != nil {
				klog.ErrorS(err, "prefill_request_failed",
					"request_id", requestID,
					"llm_engine", llmEngine,
					"prefill_pod", prefillPodName,
					"prefill_pod_ip", prefillPodIP,
					"elapsed", time.Since(requestTime))
				return
			}

			metrics.EmitMetricToPrometheus(asyncCtx, nil, metrics.GatewayPrefillRequestSuccessTotal, &metrics.SimpleMetricValue{Value: 1.0},
				map[string]string{"status": prefillRequestSuccessStatus, "status_code": "200"})

			prefillEndTime := time.Now()
			completionFields = append(completionFields,
				"routing_time_taken", prefillStartTime.Sub(requestTime),
				"prefill_time_taken", prefillEndTime.Sub(prefillStartTime),
				"outstanding_prefill_requests", e.tracker.GetPrefillRequestCountsForPod(prefillPodName)-1)
			klog.InfoS("prefill_request_end", completionFields...)
		}()
		return nil
	}

	return e.handleSync(routingCtx, prefillPod, llmEngine, apiURL, payload, completionFields, handler.MergePrefillResponse, llmEngine+" response")
}

// handleSync executes a synchronous HTTP prefill and optionally post-processes
// the response via mergeFn. Pass nil mergeFn when no response processing is needed.
func (e *DefaultExecutor) handleSync(
	routingCtx *types.RoutingContext,
	prefillPod *v1.Pod,
	llmEngine, apiURL string,
	payload []byte,
	fields []interface{},
	mergeFn func(*types.RoutingContext, map[string]any, *v1.Pod) error,
	errorContext string,
) error {
	defer e.tracker.RemovePrefillRequest(routingCtx.RequestID)

	responseData, err := e.executeHTTP(apiURL, routingCtx, payload)
	if err != nil {
		klog.ErrorS(err, "prefill_request_failed",
			"request_id", routingCtx.RequestID,
			"llm_engine", llmEngine,
			"prefill_pod", prefillPod.Name,
			"prefill_pod_ip", prefillPod.Status.PodIP,
			"elapsed", routingCtx.Elapsed(time.Now()))
		return fmt.Errorf("prefill request failed for request %s, pod %s: %w", routingCtx.RequestID, prefillPod.Name, err)
	}

	if mergeFn != nil {
		if err := mergeFn(routingCtx, responseData, prefillPod); err != nil {
			return fmt.Errorf("failed to update routing context with %s for request %s: %w", errorContext, routingCtx.RequestID, err)
		}
	}

	routingCtx.PrefillEndTime = time.Now()
	fields = append(fields,
		"routing_time_taken", routingCtx.PrefillStartTime.Sub(routingCtx.RequestTime),
		"prefill_time_taken", routingCtx.PrefillEndTime.Sub(routingCtx.PrefillStartTime),
		"outstanding_prefill_requests", e.tracker.GetPrefillRequestCountsForPod(prefillPod.Name)-1)
	klog.InfoS("prefill_request_end", fields...)
	return nil
}

// executeHTTP posts payload to url and returns the parsed JSON response body.
// Non-200 responses and transport errors are both recorded to Prometheus.
// TRT-LLM responses are parsed with UseInt64=true to prevent float64 precision
// loss on large integer fields such as disagg_request_id.
func (e *DefaultExecutor) executeHTTP(url string, routingCtx *types.RoutingContext, payload []byte) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(routingCtx.Context, time.Duration(e.requestTimeout)*time.Second)
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

	resp, err := e.httpClient.Do(req)
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
	if routingCtx.Engine == trtllmEngine {
		errUnmarshal = pd.SonicJSONInt64.Unmarshal(body, &responseData)
	} else {
		errUnmarshal = sonic.Unmarshal(body, &responseData)
	}
	if errUnmarshal != nil {
		return nil, fmt.Errorf("failed to unmarshal prefill response: %w", errUnmarshal)
	}

	return responseData, nil
}
