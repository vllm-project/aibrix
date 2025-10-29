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

package routingalgorithms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	RouterPD                      types.RoutingAlgorithm = "pd"
	VLLMEngine                    string                 = "vllm"
	SGLangEngine                  string                 = "sglang"
	SGLangBootstrapPort           int64                  = 8998
	SGLangBootstrapPortIdentifier string                 = "model.aibrix.ai/sglang-bootstrap-port"
	LLMEngineIdentifier           string                 = constants.ModelLabelEngine
	PDRoleSetIdentifier           string                 = "roleset-name"
	PDRoleIdentifier              string                 = "role-name"
	RoleReplicaIndex              string                 = "stormservice.orchestration.aibrix.ai/role-replica-index"
	PodGroupIndex                 string                 = "stormservice.orchestration.aibrix.ai/pod-group-index"
	defaultPrefillRequestTimeout  int                    = 30
)

var (
	prefillRequestTimeout int = utils.LoadEnvInt("AIBRIX_PREFILL_REQUEST_TIMEOUT", defaultPrefillRequestTimeout)
)

func init() {
	Register(RouterPD, NewPDRouter)
}

type pdRouter struct {
	cache              cache.Cache
	tokenizer          tokenizer.Tokenizer
	prefixCacheIndexer *prefixcacheindexer.PrefixHashTable
	httpClient         *http.Client
}

func NewPDRouter() (types.Router, error) {
	var tokenizerObj tokenizer.Tokenizer
	if tokenizerType == tokenizerTypeTiktoken {
		tokenizerObj = tokenizer.NewTiktokenTokenizer()
	} else {
		tokenizerObj = tokenizer.NewCharacterTokenizer()
	}

	c, err := cache.Get()
	if err != nil {
		klog.Error("fail to get cache store in prefix cache router")
		return nil, err
	}

	// Create a shared HTTP client with connection pooling
	httpClient := &http.Client{
		Timeout: time.Duration(prefillRequestTimeout) * time.Second,
		Transport: &http.Transport{
			// TODO: tune settings later
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	return pdRouter{
		cache:              c,
		tokenizer:          tokenizerObj,
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
		httpClient:         httpClient,
	}, nil
}

func (r pdRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	prefillPods, decodePods, err := r.filterPrefillDecodePods(readyPodList.All())
	if err != nil {
		return "", fmt.Errorf("failed to filter prefill/decode pods for request %s: %w", ctx.RequestID, err)
	}

	// Validate engine consistency across all prefill pods
	llmEngine, err := validateAndGetLLMEngine(prefillPods)
	if err != nil {
		return "", fmt.Errorf("engine validation failed for request %s: %w", ctx.RequestID, err)
	}

	prefillPod, err := r.doPrefillRequest(ctx, prefillPods, llmEngine)
	if err != nil {
		klog.ErrorS(err, "prefill request failed", "request_id", ctx.RequestID)
		return "", fmt.Errorf("prefill request failed for request %s: %w", ctx.RequestID, err)
	}

	decodePod := r.selectDecodePod(prefillPod, decodePods)
	if decodePod == nil {
		return "", fmt.Errorf("decode pod not found for prefill pod %s, request %s", prefillPod.Name, ctx.RequestID)
	}

	klog.InfoS("P/D routing complete", "request_id", ctx.RequestID, "prefill_pod", prefillPod.Name, "decode_pod", decodePod.Name)

	ctx.SetTargetPod(decodePod)
	return ctx.TargetAddress(), nil
}

// filterPrefillDecodePods filters pods into prefill and decode categories.
// For multi-node tensor parallelism (e.g., TP=16 with node_rank=0 and node_rank=1),
// only pods with PodGroupIndex="0" (node_rank=0) are selected as they run the HTTP server.
// Pods without PodGroupIndex label are also included for backward compatibility.
func (r *pdRouter) filterPrefillDecodePods(readyPods []*v1.Pod) ([]*v1.Pod, []*v1.Pod, error) {
	prefillPods, decodePods := []*v1.Pod{}, []*v1.Pod{}
	for _, pod := range readyPods {
		// Skip pods without role identifier
		if _, ok := pod.Labels[PDRoleIdentifier]; !ok {
			continue
		}

		// For multi-node scenarios, only select pods from node_rank=0 (PodGroupIndex=0)
		// which have the HTTP server running
		if !isPodWithHTTPServer(pod) {
			continue
		}

		switch pod.Labels[PDRoleIdentifier] {
		case "prefill":
			prefillPods = append(prefillPods, pod)
		case "decode":
			decodePods = append(decodePods, pod)
		}
	}

	if len(prefillPods) == 0 || len(decodePods) == 0 {
		return nil, nil, fmt.Errorf("prefill or decode pods are not ready: prefill=%d, decode=%d", len(prefillPods), len(decodePods))
	}
	return prefillPods, decodePods, nil
}

func (r *pdRouter) evaluatePrefixCache(ctx *types.RoutingContext, prefillPods []*v1.Pod) (*v1.Pod, []uint64, error) {
	tokens, err := r.tokenizer.TokenizeInputText(ctx.Message)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to tokenize input for request %s: %w", ctx.RequestID, err)
	}

	readyPodsMap := map[string]struct{}{}
	for _, pod := range prefillPods {
		readyPodsMap[pod.Name] = struct{}{}
	}
	matchedPods, prefixHashes := r.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, readyPodsMap)

	var prefillPod *v1.Pod
	// Check for load imbalance first
	targetPod, isImbalanced := getTargetPodOnLoadImbalance(r.cache, prefillPods)
	if isImbalanced {
		klog.InfoS("load imbalance detected, selecting least-loaded prefill pod",
			"request_id", ctx.RequestID, "selected_pod", targetPod.Name)
		prefillPod = targetPod
	} else if len(matchedPods) > 0 {
		prefillPod = getTargetPodFromMatchedPods(r.cache, prefillPods, matchedPods)
	}

	if prefillPod == nil {
		prefillPod, err = utils.SelectRandomPod(prefillPods, rand.Intn)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to select prefill pod for request %s: %w", ctx.RequestID, err)
		}
		klog.InfoS("fallback to random prefill pod selection",
			"request_id", ctx.RequestID,
			"selected_pod", prefillPod.Name)
	}

	return prefillPod, prefixHashes, nil
}

func (r *pdRouter) selectDecodePod(prefillPod *v1.Pod, decodePods []*v1.Pod) *v1.Pod {
	prefillRoleSet, ok := prefillPod.Labels[PDRoleSetIdentifier]
	if !ok {
		return nil
	}

	filteredDecodePods := []*v1.Pod{}
	for _, pod := range decodePods {
		if podRoleSet, exists := pod.Labels[PDRoleSetIdentifier]; exists && podRoleSet == prefillRoleSet {
			filteredDecodePods = append(filteredDecodePods, pod)
		}
	}
	if len(filteredDecodePods) == 0 {
		return nil
	}

	// prefer decode pod with least running requests
	decodePod := selectPodWithLeastRequestCount(r.cache, filteredDecodePods)
	if decodePod != nil {
		klog.V(5).InfoS("selected decode pod by least request count",
			"prefill_pod", prefillPod.Name,
			"decode_pod", decodePod.Name)
		return decodePod
	}

	// fallback: random selection pods
	decodePod, _ = utils.SelectRandomPod(filteredDecodePods, rand.Intn)
	return decodePod
}

// doPrefillRequest executes the prefill phase of the request.
// For vLLM: waits for prefill completion and extracts KV transfer params for decode phase
// For SGLang: launches async prefill request (bootstrap mechanism handles KV transfer internally)
func (r *pdRouter) doPrefillRequest(routingCtx *types.RoutingContext, prefillPods []*v1.Pod, llmEngine string) (*v1.Pod, error) {
	prefillPod, prefixHashes, err := r.evaluatePrefixCache(routingCtx, prefillPods)
	if err != nil {
		return nil, err
	}
	defer func() {
		if len(prefixHashes) > 0 {
			r.prefixCacheIndexer.AddPrefix(prefixHashes, routingCtx.Model, prefillPod.Name)
		}
	}()

	// Prepare prefill request payload
	payload, err := r.preparePrefillPayload(routingCtx, prefillPod, llmEngine)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare prefill payload for request %s: %w", routingCtx.RequestID, err)
	}

	// Execute HTTP request
	apiURL := fmt.Sprintf("http://%s:%d%s",
		prefillPod.Status.PodIP,
		utils.GetModelPortForPod(routingCtx.RequestID, prefillPod),
		routingCtx.ReqPath)

	klog.InfoS("start_prefill_request",
		"request_id", routingCtx.RequestID,
		"llm_engine", llmEngine,
		"prefill_pod", prefillPod.Name,
		"prefill_url", apiURL)

	if llmEngine == SGLangEngine {
		// For SGLang, use async prefill - the bootstrap mechanism (bootstrap_host/port/room)
		// coordinates between prefill and decode pods, so we don't need to wait
		go func() {
			if _, err := r.executeHTTPRequest(apiURL, routingCtx, payload); err != nil {
				klog.ErrorS(err, "async prefill request failed",
					"request_id", routingCtx.RequestID,
					"llm_engine", llmEngine,
					"prefill_pod", prefillPod.Name,
					"prefill_pod_ip", prefillPod.Status.PodIP)
				return
			}
			klog.InfoS("prefill_request_complete",
				"request_id", routingCtx.RequestID,
				"llm_engine", llmEngine,
				"prefill_pod", prefillPod.Name)
		}()
	} else if llmEngine == VLLMEngine {
		// For vLLM, wait synchronously to get KV transfer params from response
		responseData, err := r.executeHTTPRequest(apiURL, routingCtx, payload)
		if err != nil {
			return nil, fmt.Errorf("prefill request failed for request %s, pod %s: %w", routingCtx.RequestID, prefillPod.Name, err)
		}

		// Update routing context with KV transfer params from prefill response
		if err := r.updateRoutingContextWithKVTransferParams(routingCtx, responseData, prefillPod); err != nil {
			return nil, fmt.Errorf("failed to update routing context with KV transfer params for request %s: %w", routingCtx.RequestID, err)
		}

		klog.InfoS("prefill_request_complete",
			"request_id", routingCtx.RequestID,
			"llm_engine", llmEngine,
			"prefill_pod", prefillPod.Name,
			"prefill_pod_ip", prefillPod.Status.PodIP)
	} else {
		// For unknown engines, use synchronous approach as a safe default
		if _, err := r.executeHTTPRequest(apiURL, routingCtx, payload); err != nil {
			return nil, fmt.Errorf("prefill request failed for request %s, pod %s: %w", routingCtx.RequestID, prefillPod.Name, err)
		}
		klog.InfoS("prefill_request_complete",
			"request_id", routingCtx.RequestID,
			"llm_engine", llmEngine,
			"prefill_pod", prefillPod.Name,
			"prefill_pod_ip", prefillPod.Status.PodIP)
	}

	return prefillPod, nil
}

func (r *pdRouter) preparePrefillPayload(routingCtx *types.RoutingContext, pod *v1.Pod, llmEngine string) ([]byte, error) {
	var completionRequest map[string]any
	if err := json.Unmarshal(routingCtx.ReqBody, &completionRequest); err != nil {
		return nil, fmt.Errorf("failed to unmarshal prefill request body: %w", err)
	}

	// Handle SGLang specific configuration
	if llmEngine == SGLangEngine {
		completionRequest["bootstrap_host"] = pod.Status.PodIP
		completionRequest["bootstrap_port"] = getSGLangBootstrapPort(pod)
		completionRequest["bootstrap_room"] = rand.Int63n(1<<63 - 1)

		// Create a copy of the request body
		reqBody, err := json.Marshal(completionRequest)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal post prefill request body: %w", err)
		}
		bodyCopy := make([]byte, len(reqBody))
		copy(bodyCopy, reqBody)
		routingCtx.ReqBody = bodyCopy
	}

	// Add nixl-specific kv_transfer_params for vLLM prefill requests only
	if llmEngine == VLLMEngine {
		completionRequest["kv_transfer_params"] = map[string]any{
			"do_remote_decode":  true,
			"do_remote_prefill": false,
			"remote_engine_id":  nil,
			"remote_block_ids":  nil,
			"remote_host":       nil,
			"remote_port":       nil,
		}
	}

	// Set prefill-specific parameters
	completionRequest["max_tokens"] = 1
	completionRequest["max_completion_tokens"] = 1
	completionRequest["stream"] = false
	delete(completionRequest, "stream_options")

	return json.Marshal(completionRequest)
}

func (r *pdRouter) executeHTTPRequest(url string, routingCtx *types.RoutingContext, payload []byte) (map[string]any, error) {
	// Create request with context for cancellation support
	ctx, cancel := context.WithTimeout(routingCtx.Context, time.Duration(prefillRequestTimeout)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create http prefill request: %w", err)
	}

	// Set headers
	for key, value := range routingCtx.ReqHeaders {
		req.Header.Set(key, value)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Request-Id", routingCtx.RequestID)

	// Use shared HTTP client with connection pooling
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute http prefill request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read prefill response body: %w", err)
	}

	// Check response status
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http prefill request failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response JSON
	var responseData map[string]any
	if err := json.Unmarshal(body, &responseData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal prefill response: %w", err)
	}

	return responseData, nil
}

func (r *pdRouter) updateRoutingContextWithKVTransferParams(routingCtx *types.RoutingContext, responseData map[string]any, prefillPod *v1.Pod) error {
	// Extract kv_transfer_params from prefill response
	kvTransferParams, exists := responseData["kv_transfer_params"]
	if !exists {
		klog.InfoS("no kv_transfer_params in prefill response", "request_id", routingCtx.RequestID)
		return nil
	}

	// Parse the original request body
	var originalRequest map[string]any
	if err := json.Unmarshal(routingCtx.ReqBody, &originalRequest); err != nil {
		return fmt.Errorf("failed to unmarshal original request body: %w", err)
	}

	// Update request body with KV transfer params from prefill response
	originalRequest["kv_transfer_params"] = kvTransferParams

	// Add prefill host information
	kvTransferParamsMap, ok := kvTransferParams.(map[string]any)
	if !ok {
		return fmt.Errorf("kv_transfer_params has unexpected type %T, expected map[string]any", kvTransferParams)
	}
	kvTransferParamsMap["remote_host"] = prefillPod.Status.PodIP

	// Marshal the updated request body
	updatedReqBody, err := json.Marshal(originalRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal updated request body: %w", err)
	}

	// Update routing context with new request body
	routingCtx.ReqBody = updatedReqBody

	klog.InfoS("updated routing context with kv_transfer_params",
		"request_id", routingCtx.RequestID,
		"prefill_pod", prefillPod.Name,
		"prefill_host", prefillPod.Status.PodIP)
	return nil
}

func (r *pdRouter) SubscribedMetrics() []string {
	return []string{}
}

func getLLMEngine(pod *v1.Pod, labelName string, defaultValue string) string {
	labelTarget, ok := pod.Labels[labelName]
	if !ok {
		return defaultValue
	}
	return labelTarget
}

func getSGLangBootstrapPort(pod *v1.Pod) int64 {
	if portStr, exists := pod.Annotations[SGLangBootstrapPortIdentifier]; exists {
		if port, err := strconv.ParseInt(portStr, 10, 32); err == nil {
			return port
		}
	}
	return SGLangBootstrapPort // Default port
}

// validateAndGetLLMEngine validates that all prefill pods use the same engine and returns it.
func validateAndGetLLMEngine(prefillPods []*v1.Pod) (string, error) {
	if len(prefillPods) == 0 {
		return "", fmt.Errorf("no prefill pods provided")
	}

	firstEngine := getLLMEngine(prefillPods[0], LLMEngineIdentifier, VLLMEngine)

	// Validate all pods use the same engine
	for i := 1; i < len(prefillPods); i++ {
		engine := getLLMEngine(prefillPods[i], LLMEngineIdentifier, VLLMEngine)
		if engine != firstEngine {
			return "", fmt.Errorf("inconsistent LLM engines detected: pod %s has %s, pod %s has %s",
				prefillPods[0].Name, firstEngine, prefillPods[i].Name, engine)
		}
	}

	return firstEngine, nil
}

// isPodWithHTTPServer checks if a pod should be selected for routing.
// In multi-node tensor parallelism setups (e.g., TP=16 with node_rank=0 and node_rank=1),
// only pods with stormservice.orchestration.aibrix.ai/pod-group-index="0" (corresponding to node_rank=0) run the HTTP server.
// Pods without the label are also selected for backward compatibility.
func isPodWithHTTPServer(pod *v1.Pod) bool {
	podGroupIndex, exists := pod.Labels[PodGroupIndex]
	if !exists {
		// No PodGroupIndex label means single-node or old setup - include it
		return true
	}
	// Only include pods from node_rank=0 which have the HTTP server
	return podGroupIndex == "0"
}
