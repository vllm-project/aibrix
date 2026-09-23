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
	"strings"
	"time"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const prefillRequestSuccessStatus = "pd-prefill-request-success"

func incPrefillOutstanding() {
	metrics.IncGaugeMetric(
		metrics.GatewayPrefillOutstandingRequests,
		metrics.GetMetricHelp(metrics.GatewayPrefillOutstandingRequests),
		[]string{"gateway_pod"},
		metrics.GatewayPodName(),
	)
}

func decPrefillOutstanding() {
	metrics.DecGaugeMetric(
		metrics.GatewayPrefillOutstandingRequests,
		metrics.GetMetricHelp(metrics.GatewayPrefillOutstandingRequests),
		[]string{"gateway_pod"},
		metrics.GatewayPodName(),
	)
}

// DefaultExecutor is the standard PrefillExecutor. It owns the HTTP client and
// the prefill-request tracker so that prefill logic can be tested in isolation
// from the routing/scoring concerns in pdRouter.
type DefaultExecutor struct {
	httpClient *http.Client
	tracker    *pd.PrefillRequestTracker
	tokenLoad  *pd.TokenLoadTracker // optional; nil when no policy charges it
}

// ExecutorOption customizes a DefaultExecutor.
type ExecutorOption func(*DefaultExecutor)

// WithTokenLoadTracker makes the executor release a request's active
// token-load charge (TokenLoadTracker.ReleaseTokens) at the same point where
// it removes the request from the PrefillRequestTracker: when the prefill
// HTTP call returns, on both the sync and the async path.
func WithTokenLoadTracker(tokenLoad *pd.TokenLoadTracker) ExecutorOption {
	return func(e *DefaultExecutor) { e.tokenLoad = tokenLoad }
}

// effectiveRequestTimeout returns the deadline of this request's prefill call:
// its resolved PD overrides, which carry the AIBRIX_PREFILL_REQUEST_TIMEOUT
// default when its profile sets none.
func (e *DefaultExecutor) effectiveRequestTimeout(routingCtx *types.RoutingContext) time.Duration {
	return routingCtx.PDOverrides().PrefillRequestTimeout
}

// NewDefaultExecutor constructs a DefaultExecutor. httpClient and tracker are
// shared with the router.
func NewDefaultExecutor(httpClient *http.Client, tracker *pd.PrefillRequestTracker, opts ...ExecutorOption) PrefillExecutor {
	e := &DefaultExecutor{
		httpClient: httpClient,
		tracker:    tracker,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// prefillDone is the single point where a finished prefill call is taken off
// the router's ledgers: the request-count tracker always, the token-load
// tracker when one is attached.
func (e *DefaultExecutor) prefillDone(requestID string) {
	e.tracker.RemovePrefillRequest(requestID)
	if e.tokenLoad != nil {
		e.tokenLoad.ReleaseTokens(requestID)
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
		"outstanding_prefill_requests", e.tracker.GetPrefillRequestCountsForPod(pd.PodKey(prefillPod)),
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

	routingCtx.PrefillStartTime = time.Now()

	// Resolved before the async split: the goroutine must use the same deadline
	// as the sync path, and it must not read the pooled routing context.
	prefillTimeout := e.effectiveRequestTimeout(routingCtx)

	if handler.IsAsync() {
		// SGLang uses a bootstrap handshake to coordinate KV transfer out-of-band;
		// fire asynchronously and return immediately.
		requestID := routingCtx.RequestID
		requestTime := routingCtx.RequestTime
		prefillStartTime := routingCtx.PrefillStartTime
		prefillPodName := prefillPod.Name
		prefillPodKey := pd.PodKey(prefillPod)
		prefillPodIP := prefillPod.Status.PodIP
		model := routingCtx.Model
		asyncCtx := &types.RoutingContext{
			Context:     context.WithoutCancel(routingCtx.Context),
			RequestID:   requestID,
			Model:       model,
			Engine:      routingCtx.Engine,
			RequestTime: requestTime,
			ReqHeaders:  maps.Clone(routingCtx.ReqHeaders),
		}
		// Captured before the goroutine starts, and used instead of
		// routingCtx from inside it: this goroutine regularly outlives the
		// client stream, and RoutingContext is pooled, so reporting through it
		// could land on whichever request has since taken the object. The leg
		// is per-incarnation and inert once its request is done.
		leg := routingCtx.PDLeg()
		go func() {
			incPrefillOutstanding()
			defer decPrefillOutstanding()
			defer e.prefillDone(requestID)

			if _, err := e.executeHTTP(apiURL, asyncCtx, payload, prefillTimeout); err != nil {
				// The prefill leg is fire-and-forget, so nobody is waiting on
				// this error: record it on the leg (and abort the decode leg
				// that will never receive its KV) before it is only logged.
				failure := pd.OnPrefillLegFailed(e.httpClient, leg, requestID, model, err)
				klog.ErrorS(err, "prefill_request_failed",
					"request_id", requestID,
					"llm_engine", llmEngine,
					"prefill_pod", prefillPodName,
					"prefill_pod_ip", prefillPodIP,
					"prefill_failure_class", failure.ClassOrEmpty(),
					"elapsed", time.Since(requestTime))
				return
			}

			metrics.EmitMetricToPrometheus(asyncCtx, nil, metrics.GatewayPrefillRequestSuccessTotal, &metrics.SimpleMetricValue{Value: 1.0},
				map[string]string{"status": prefillRequestSuccessStatus, "status_code": "200"})

			prefillEndTime := time.Now()
			completionFields = append(completionFields,
				"routing_time_taken", prefillStartTime.Sub(requestTime),
				"prefill_time_taken", prefillEndTime.Sub(prefillStartTime),
				"outstanding_prefill_requests", e.tracker.GetPrefillRequestCountsForPod(prefillPodKey)-1)
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
	mergeFn func(*types.RoutingContext, []byte, *v1.Pod) error,
	errorContext string,
) error {
	incPrefillOutstanding()
	defer decPrefillOutstanding()
	defer e.prefillDone(routingCtx.RequestID)

	prefillResponse, err := e.executeHTTP(apiURL, routingCtx, payload, e.effectiveRequestTimeout(routingCtx))
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
		if err := mergeFn(routingCtx, prefillResponse, prefillPod); err != nil {
			return fmt.Errorf("failed to update routing context with %s for request %s: %w", errorContext, routingCtx.RequestID, err)
		}
	}

	routingCtx.PrefillEndTime = time.Now()
	fields = append(fields,
		"routing_time_taken", routingCtx.PrefillStartTime.Sub(routingCtx.RequestTime),
		"prefill_time_taken", routingCtx.PrefillEndTime.Sub(routingCtx.PrefillStartTime),
		"outstanding_prefill_requests", e.tracker.GetPrefillRequestCountsForPod(pd.PodKey(prefillPod))-1)
	klog.InfoS("prefill_request_end", fields...)
	return nil
}

// executeHTTP posts payload to url and returns the raw JSON response body,
// which is guaranteed to be a JSON object. Non-200 responses and transport
// errors are both recorded to Prometheus. The body is deliberately not
// decoded: merge functions read the fields they need with gjson so that
// large integers (e.g. TRT-LLM disagg_request_id) and nested key order are
// preserved exactly.
//
// Failures are returned as the typed errors of package pd (PrefillSetupError,
// PrefillHTTPError, PrefillBodyError) or as a wrapped transport error, so that
// pd.OnPrefillLegFailed can classify them without parsing error strings.
func (e *DefaultExecutor) executeHTTP(url string, routingCtx *types.RoutingContext, payload []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(routingCtx.Context, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return nil, &pd.PrefillSetupError{Err: fmt.Errorf("failed to create http prefill request: %w", err)}
	}

	// ReqHeaders is populated from Envoy and includes HTTP/2 pseudo-headers
	// such as ":method". net/http rejects those names on the outbound HTTP/1
	// prefill call (invalid header field name), which fails every PD route
	// before the prefill pod is contacted. Combined routing never makes this
	// call, so only the prefill/decode path would 503.
	for key, value := range routingCtx.ReqHeaders {
		if !forwardablePrefillHeader(key) {
			continue
		}
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
		return nil, &pd.PrefillHTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	if err := pd.ValidateJSONObject(body, "prefill response"); err != nil {
		// A 200 with an unparseable body still completed the KV transfer, so
		// this is kept distinct from a transport failure: the decode leg must
		// not be aborted for it.
		return nil, &pd.PrefillBodyError{Err: err}
	}

	return body, nil
}

// forwardablePrefillHeader reports whether key can be copied onto the
// outbound prefill HTTP request. Envoy :pseudo-headers are not valid HTTP/1
// field names and must be dropped.
func forwardablePrefillHeader(key string) bool {
	return key != "" && !strings.HasPrefix(key, ":")
}
