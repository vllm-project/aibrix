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
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	rand_math "math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
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
	rng                *rand_math.Rand
	rngMu              sync.Mutex
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

	// Create cryptographically secure seed
	seedBytes := make([]byte, 8)
	if _, err := rand.Read(seedBytes); err != nil {
		// Fallback to time-based seed if crypto/rand fails
		seed := time.Now().UnixNano()
		return &pdRouter{
			cache:              c,
			tokenizer:          tokenizerObj,
			prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
			rng:                rand_math.New(rand_math.NewSource(seed)),
		}, nil
	}

	// Convert bytes to int64 seed
	seed := int64(0)
	for i, b := range seedBytes {
		seed |= int64(b) << (8 * uint(i))
	}

	return &pdRouter{
		cache:              c,
		tokenizer:          tokenizerObj,
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
		rng:                rand_math.New(rand_math.NewSource(seed)),
	}, nil
}

func (r *pdRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	prefillPods, decodePods, err := r.filterPrefillDecodePods(readyPodList.All())
	if err != nil {
		return "", err
	}

	prefillPod, err := r.doPrefillRequest(ctx, prefillPods, getLLMEngine(prefillPods[0], LLMEngineIdentifier, VLLMEngine))
	if err != nil {
		klog.ErrorS(err, "prefill request failed", "request_id", ctx.RequestID)
		return "", err
	}

	decodePod := r.selectDecodePod(prefillPod, decodePods, ctx)
	if decodePod == nil {
		return "", fmt.Errorf("decode pod not found")
	}

	klog.InfoS("P/D", "prefill_pod", prefillPod.Name, "decode_pod", decodePod.Name)

	ctx.SetTargetPod(decodePod)
	return ctx.TargetAddress(), nil
}

func (r *pdRouter) filterPrefillDecodePods(readyPods []*v1.Pod) ([]*v1.Pod, []*v1.Pod, error) {
	prefillPods, decodePods := []*v1.Pod{}, []*v1.Pod{}
	for _, pod := range readyPods {
		if _, ok := pod.Labels[PDRoleIdentifier]; !ok {
			continue
		}
		if podGroupIndex, ok := pod.Labels[PodGroupIndex]; ok && podGroupIndex != "0" {
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
		return nil, nil, fmt.Errorf("prefill or decodes pods are not ready")
	}
	return prefillPods, decodePods, nil
}

func (r *pdRouter) evaluatePrefixCache(ctx *types.RoutingContext, prefillPods []*v1.Pod) (*v1.Pod, []uint64, error) {
	tokens, err := r.tokenizer.TokenizeInputText(ctx.Message)
	if err != nil {
		return nil, nil, err
	}

	readyPodsMap := map[string]struct{}{}
	for _, pod := range prefillPods {
		readyPodsMap[pod.Name] = struct{}{}
	}
	matchedPods, prefixHashes := r.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, readyPodsMap)

	var prefillPod *v1.Pod
	if len(matchedPods) > 0 {
		prefillPod = getTargetPodFromMatchedPods(r.cache, prefillPods, matchedPods)
	}
	if prefillPod == nil {
		prefillPod = r.selectPrefillPodWithP2C(prefillPods, ctx)
	}

	return prefillPod, prefixHashes, err
}

func (r *pdRouter) selectDecodePod(prefillPod *v1.Pod, decodePods []*v1.Pod, ctx *types.RoutingContext) *v1.Pod {
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

	return r.selectDecodePodWithP2C(filteredDecodePods, ctx)
}

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
		return nil, fmt.Errorf("failed to prepare prefill payload: %w", err)
	}

	// Execute HTTP request
	apiURL := fmt.Sprintf("http://%s:%d%s",
		prefillPod.Status.PodIP,
		utils.GetModelPortForPod(routingCtx.RequestID, prefillPod),
		routingCtx.ReqPath)

	klog.InfoS("start_prefill_request",
		"request_id", routingCtx.RequestID,
		"llm_engine", llmEngine,
		"prefill_url", apiURL)

	if llmEngine == SGLangEngine {
		go func() {
			if _, err := r.executeHTTPRequest(apiURL, routingCtx, payload); err != nil {
				klog.ErrorS(err, "prefill request for sglang failed", "request_id", routingCtx.RequestID)
				return
			}
			klog.InfoS("prefill_request_complete", "request_id", routingCtx.RequestID)
		}()
	} else if llmEngine == VLLMEngine {
		responseData, err := r.executeHTTPRequest(apiURL, routingCtx, payload)
		if err != nil {
			return nil, fmt.Errorf("failed to execute prefill request: %w", err)
		}

		// Update routing context with KV transfer params from prefill response for vLLM
		if err := r.updateRoutingContextWithKVTransferParams(routingCtx, responseData, prefillPod); err != nil {
			return nil, fmt.Errorf("failed to update routing context with KV transfer params: %w", err)
		}

		klog.InfoS("prefill_request_complete", "request_id", routingCtx.RequestID, "prefill_pod_ip", prefillPod.Status.PodIP)
	} else {
		if _, err := r.executeHTTPRequest(apiURL, routingCtx, payload); err != nil {
			return nil, fmt.Errorf("failed to execute prefill request: %w", err)
		}
		klog.InfoS("prefill_request_complete", "request_id", routingCtx.RequestID, "prefill_pod_ip", prefillPod.Status.PodIP)
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
		r.rngMu.Lock()
		bootstrapRoom := r.rng.Int63n(1<<63 - 1)
		r.rngMu.Unlock()
		completionRequest["bootstrap_room"] = bootstrapRoom

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
	// Create request with context
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create http prefill request: %w", err)
	}

	// Set headers
	for key, value := range routingCtx.ReqHeaders {
		req.Header.Set(key, value)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Request-Id", routingCtx.RequestID)

	client := &http.Client{
		Timeout: time.Duration(prefillRequestTimeout) * time.Second,
	}
	resp, err := client.Do(req)
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

	// Add prefill host information following the Python pattern
	if kvTransferParamsMap, ok := kvTransferParams.(map[string]any); ok {
		kvTransferParamsMap["remote_host"] = prefillPod.Status.PodIP
	}

	// Marshal the updated request body
	updatedReqBody, err := json.Marshal(originalRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal updated request body: %w", err)
	}

	// Update routing context with new request body
	routingCtx.ReqBody = updatedReqBody

	klog.InfoS("updated routing context with kv_transfer_params", "request_id", routingCtx.RequestID, "prefill_host", prefillPod.Status.PodIP)
	return nil
}

func (r *pdRouter) safeIntn(n int) int {
	r.rngMu.Lock()
	defer r.rngMu.Unlock()
	return r.rng.Intn(n)
}

func (r *pdRouter) selectPrefillPodWithP2C(pods []*v1.Pod, ctx *types.RoutingContext) *v1.Pod {
	if len(pods) == 0 {
		return nil
	}
	if len(pods) == 1 {
		return pods[0]
	}
	if len(pods) == 2 {
		return r.chooseBetterPrefillPod(pods[0], pods[1], ctx)
	}

	idx1 := r.safeIntn(len(pods))
	idx2 := r.safeIntn(len(pods))
	for idx2 == idx1 {
		idx2 = r.safeIntn(len(pods))
	}

	return r.chooseBetterPrefillPod(pods[idx1], pods[idx2], ctx)
}

func (r *pdRouter) chooseBetterPrefillPod(pod1, pod2 *v1.Pod, ctx *types.RoutingContext) *v1.Pod {
	score1 := r.calculatePrefillScore(pod1, ctx)
	score2 := r.calculatePrefillScore(pod2, ctx)

	// Fix bias: if scores are equal, randomly choose instead of always picking pod1
	if score1 == score2 {
		if r.safeIntn(2) == 0 {
			return pod1
		}
		return pod2
	}

	if score1 < score2 {
		return pod1
	}
	return pod2
}

func (r *pdRouter) calculatePrefillScore(pod *v1.Pod, ctx *types.RoutingContext) float64 {
	runningReqs, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.NumRequestsRunning)
	if err != nil {
		runningReqs = &metrics.SimpleMetricValue{Value: 0}
	}

	waitingReqs, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.NumRequestsWaiting)
	if err != nil {
		waitingReqs = &metrics.SimpleMetricValue{Value: 0}
	}

	promptThroughput, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgPromptThroughputToksPerS)
	if err != nil {
		promptThroughput = &metrics.SimpleMetricValue{Value: 1}
	}

	// Normalize by throughput capacity to get load score
	throughputValue := math.Max(promptThroughput.GetSimpleValue(), 1)
	loadScore := (runningReqs.GetSimpleValue() + waitingReqs.GetSimpleValue()) / throughputValue

	return loadScore
}

func (r *pdRouter) selectDecodePodWithP2C(pods []*v1.Pod, ctx *types.RoutingContext) *v1.Pod {
	if len(pods) == 0 {
		return nil
	}
	if len(pods) == 1 {
		return pods[0]
	}
	if len(pods) == 2 {
		return r.chooseBetterDecodePod(pods[0], pods[1], ctx)
	}

	idx1 := r.safeIntn(len(pods))
	idx2 := r.safeIntn(len(pods))
	for idx2 == idx1 {
		idx2 = r.safeIntn(len(pods))
	}

	return r.chooseBetterDecodePod(pods[idx1], pods[idx2], ctx)
}

func (r *pdRouter) chooseBetterDecodePod(pod1, pod2 *v1.Pod, ctx *types.RoutingContext) *v1.Pod {
	score1 := r.calculateDecodeScore(pod1, ctx)
	score2 := r.calculateDecodeScore(pod2, ctx)

	// Fix bias: if scores are equal, randomly choose instead of always picking pod1
	if score1 == score2 {
		if r.safeIntn(2) == 0 {
			return pod1
		}
		return pod2
	}

	if score1 < score2 {
		return pod1
	}
	return pod2
}

func (r *pdRouter) calculateDecodeScore(pod *v1.Pod, ctx *types.RoutingContext) float64 {
	m, err := r.cache.GetMetricValueByPodModel(
		pod.Name, pod.Namespace, ctx.Model, metrics.GPUCacheUsagePerc,
	)
	if err != nil || m == nil {
		return 1.0
	}
	score := m.GetSimpleValue()
	if score > 1.0 {
		score = score / 100.0
	}
	if score < 0 {
		score = 0
	} else if score > 1 {
		score = 1
	}
	return score
}

func (r *pdRouter) SubscribedMetrics() []string {
	return []string{
		metrics.NumRequestsRunning,
		metrics.NumRequestsWaiting,
		metrics.AvgGenerationThroughputToksPerS,
		metrics.GPUCacheUsagePerc,
		metrics.AvgPromptThroughputToksPerS,
	}
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
