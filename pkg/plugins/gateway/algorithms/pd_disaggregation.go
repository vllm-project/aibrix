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
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
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

	defaultMaxRequest             float64 = 32
	defaultMaxTokenThroughputDiff float64 = 2048
)

var (
	prefillRequestTimeout         int     = utils.LoadEnvInt("AIBRIX_PREFILL_REQUEST_TIMEOUT", defaultPrefillRequestTimeout)
	aibrixDecodeMaxRequest        float64 = utils.LoadEnvFloat("AIBRIX_DECODE_MAX_REQUEST", defaultMaxRequest)
	aibrixDecodeMaxThroughputDiff float64 = utils.LoadEnvFloat("AIBRIX_DECODE_MAX_THROUGHPUT", defaultMaxTokenThroughputDiff)
)

func init() {
	Register(RouterPD, NewPDRouter)
}

type pdRouter struct {
	cache                 cache.Cache
	tokenizer             tokenizer.Tokenizer
	prefixCacheIndexer    *prefixcacheindexer.PrefixHashTable
	prefillRequestTracker *PrefillRequestTracker
}

// PrefillRequestTracker manages prefill-specific request counts
type PrefillRequestTracker struct {
	// Map of pod name -> active prefill request count
	podRequestCounts sync.Map // map[string]*int32
	// Map of request ID -> pod name for cleanup
	requestToPod sync.Map // map[string]string
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

	return pdRouter{
		cache:                 c,
		tokenizer:             tokenizerObj,
		prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
		prefillRequestTracker: NewPrefillRequestTracker(),
	}, nil
}

// NewPrefillRequestTracker creates a new prefill request tracker
func NewPrefillRequestTracker() *PrefillRequestTracker {
	return &PrefillRequestTracker{
		podRequestCounts: sync.Map{},
		requestToPod:     sync.Map{},
	}
}

func (r pdRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	prefillPod, decodePod, err := r.filterPrefillDecodePods(ctx, readyPodList.All())
	if err != nil {
		return "", err
	}

	klog.InfoS("P/D", "request_id", ctx.RequestID, "prefill_pod", prefillPod.Name, "decode_pod", decodePod.Name)

	err = r.doPrefillRequest(ctx, prefillPod, getLLMEngine(prefillPod, LLMEngineIdentifier, VLLMEngine))
	if err != nil {
		klog.ErrorS(err, "prefill request failed", "request_id", ctx.RequestID)
		return "", err
	}

	ctx.SetTargetPod(decodePod)
	return ctx.TargetAddress(), nil
}

type Scores struct {
	Pod   *v1.Pod
	Score float64
}

func (r *pdRouter) filterPrefillDecodePods(routingCtx *types.RoutingContext, readyPods []*v1.Pod) (*v1.Pod, *v1.Pod, error) {
	prefillPods, decodePods := []*v1.Pod{}, []*v1.Pod{}
	for _, pod := range readyPods {
		if _, ok := pod.Labels[PDRoleSetIdentifier]; !ok {
			continue
		}
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

	// Check for prefill and decode imbalance
	// TODO: consider prefill/decode imbalance pod by roleset rather than individual pods because in corner case,
	// if roleset1 has prefill imbalance and roleset2 has decode imbalance then always prefill/decode will be selected for roleset2
	// and make roleset2 decode imbalance worse.
	targetPod, isImbalanced := r.loadImbalanceSelectPrefillPod(prefillPods, r.prefillRequestTracker.GetPrefillRequestCountsForPods(prefillPods))
	if isImbalanced {
		klog.InfoS("load imbalance detected, selecting least-loaded prefill pod", "request_id", routingCtx.RequestID, "selected_prefill_pod", targetPod.Name)
		prefillPods = []*v1.Pod{targetPod}
		decodePods = utils.FilterPodsByLabel(decodePods, PDRoleSetIdentifier, targetPod.Labels[PDRoleSetIdentifier])
	}
	targetPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage := r.loadImbalanceSelectDecodePod(routingCtx, decodePods)
	if targetPod != nil {
		klog.InfoS("load imbalance detected in decode pods", "request_id", routingCtx.RequestID, "selected_decode_pod", targetPod.Name)
		decodePods = []*v1.Pod{targetPod}
		if len(prefillPods) > 1 {
			prefillPods = utils.FilterPodsByLabel(prefillPods, PDRoleSetIdentifier, targetPod.Labels[PDRoleSetIdentifier])
		}
	}

	klog.InfoS("selected pods", "prefill_pods", len(prefillPods), "decode_pods", len(decodePods))

	prefillScores, maxPrefillScore, prefixHashes := r.scorePrefillPods(routingCtx, prefillPods)
	decodeScores, maxDecodeScore := r.scoreDecodePods(routingCtx, decodePods, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage)

	var targetPrefillPod, targetDecodePod *v1.Pod
	minScore := math.MaxFloat64
	for roleset, prefillScore := range prefillScores {
		decodeScore, ok := decodeScores[roleset]
		if !ok {
			continue
		}

		normalizedPrefillScore := prefillScore.Score / maxPrefillScore
		normalizedDecodeScore := decodeScore.Score / maxDecodeScore

		if normalizedPrefillScore+normalizedDecodeScore < minScore {
			minScore = normalizedPrefillScore + normalizedDecodeScore
			targetPrefillPod = prefillScore.Pod
			targetDecodePod = decodeScore.Pod
		}
		klog.InfoS("final_score", "request_id", routingCtx.RequestID, "roleset", roleset,
			"final_score", minScore,
			"prefill_score", prefillScore.Score, "normalized_prefill_score", normalizedPrefillScore,
			"decode_score", decodeScore.Score, "normalized_decode_score", normalizedDecodeScore)
	}

	defer func() {
		if len(prefixHashes) > 0 {
			r.prefixCacheIndexer.AddPrefix(prefixHashes, routingCtx.Model, targetPrefillPod.Name)
		}
	}()

	if targetPrefillPod == nil {
		return nil, nil, fmt.Errorf("target prefill  pod is nil")
	}
	if targetDecodePod == nil {
		return nil, nil, fmt.Errorf("target decode pod is nil")
	}

	return targetPrefillPod, targetDecodePod, nil
}

func (r *pdRouter) scorePrefillPods(routingCtx *types.RoutingContext, prefillPods []*v1.Pod) (map[string]*Scores, float64, []uint64) {
	prefillScores := map[string]*Scores{}
	tokens, err := r.tokenizer.TokenizeInputText(routingCtx.Message)
	if err != nil {
		return nil, 0, nil
	}

	var maxRequestCount float64 = 1
	requestCount := []float64{}
	readyPodsMap := map[string]struct{}{}
	podRequestCount := r.prefillRequestTracker.GetPrefillRequestCountsForPods(prefillPods)

	// klog.InfoS("prefill_pod_request_count", "request_id", routingCtx.RequestID, "pod_request_count", podRequestCount)
	for _, cnt := range podRequestCount {
		countFloat := float64(cnt)
		requestCount = append(requestCount, countFloat)
		if countFloat > maxRequestCount {
			maxRequestCount = countFloat
		}
	}
	meanRequestCount := mean(requestCount)
	stdDevRequestCount := standardDeviation(requestCount)
	for _, pod := range prefillPods {
		readyPodsMap[pod.Name] = struct{}{}
	}

	matchedPods, prefixHashes := r.prefixCacheIndexer.MatchPrefix(tokens, routingCtx.Model, readyPodsMap)

	maxPrefillScore := float64(1)
	for _, pod := range prefillPods {
		rolesetName := pod.Labels[PDRoleSetIdentifier]
		reqCnt := float64(podRequestCount[pod.Name])
		if reqCnt > meanRequestCount+float64(standardDeviationFactor)*stdDevRequestCount {
			klog.InfoS("prefill pod request count is higher than mean request count, skipping", "request_id", routingCtx.RequestID, "pod_name", pod.Name,
				"req_cnt", reqCnt, "mean_req_cnt", meanRequestCount, "std_dev_req_cnt", stdDevRequestCount)
			continue
		}

		prefillScore := (100-float64(matchedPods[pod.Name]))*.1 + (reqCnt / maxRequestCount)
		if existingScore, exists := prefillScores[rolesetName]; !exists || prefillScore < existingScore.Score {
			prefillScores[rolesetName] = &Scores{
				Pod:   pod,
				Score: prefillScore,
			}
		}
		if prefillScore > maxPrefillScore {
			maxPrefillScore = prefillScore
		}

		klog.InfoS("prefill score", "request_id", routingCtx.RequestID, "pod_name", pod.Name,
			"prefill_score", prefillScore,
			"prefix_match_percent", float64(matchedPods[pod.Name]),
			"running_reqs", reqCnt, "max_running_reqs", maxRequestCount)
	}

	return prefillScores, maxPrefillScore, prefixHashes
}

func (r *pdRouter) scoreDecodePods(routingCtx *types.RoutingContext, filteredDecodePods []*v1.Pod,
	maxRequestCount float64, maxThroughput float64, maxFreeGPUUsage float64,
	podRequestCounts map[string]float64, podThroughputs map[string]float64, podFreeGpuUsage map[string]float64) (map[string]*Scores, float64) {
	decodeScores := map[string]*Scores{}
	maxDecodeScore := float64(0.01)

	for _, pod := range filteredDecodePods {
		rolesetName := pod.Labels[PDRoleSetIdentifier]

		normalizedRunningReqs := podRequestCounts[pod.Name] / maxRequestCount
		normalizedThroughput := 1 - podThroughputs[pod.Name]/maxThroughput
		normalizedFreeGPUPercent := podFreeGpuUsage[pod.Name] / maxFreeGPUUsage

		decodeScore := ((normalizedRunningReqs) + normalizedThroughput) / normalizedFreeGPUPercent
		if existingScore, exists := decodeScores[rolesetName]; !exists || decodeScore < existingScore.Score {
			decodeScores[rolesetName] = &Scores{
				Pod:   pod,
				Score: decodeScore,
			}
		}
		if decodeScore > maxDecodeScore {
			maxDecodeScore = decodeScore
		}

		klog.InfoS("decode score", "request_id", routingCtx.RequestID, "pod_name", pod.Name, "decode_score", decodeScore,
			"running_reqs", podRequestCounts[pod.Name], "normalized_running_reqs", normalizedRunningReqs,
			"throughput", podThroughputs[pod.Name], "normalized_throughput", normalizedThroughput,
			"free_gpu", podFreeGpuUsage[pod.Name], "normalized_free_gpu", normalizedFreeGPUPercent)
	}

	return decodeScores, maxDecodeScore
}

func (r *pdRouter) doPrefillRequest(routingCtx *types.RoutingContext, prefillPod *v1.Pod, llmEngine string) error {
	// Prepare prefill request payload
	payload, err := r.preparePrefillPayload(routingCtx, prefillPod, llmEngine)
	if err != nil {
		return fmt.Errorf("failed to prepare prefill payload: %w", err)
	}

	// Execute HTTP request
	apiURL := fmt.Sprintf("http://%s:%d%s",
		prefillPod.Status.PodIP,
		utils.GetModelPortForPod(routingCtx.RequestID, prefillPod),
		routingCtx.ReqPath)

	klog.InfoS("start_prefill_request",
		"request_id", routingCtx.RequestID,
		"llm_engine", llmEngine,
		"prefill_pod_name", prefillPod.Name,
		"prefill_url", apiURL)

	r.prefillRequestTracker.AddPrefillRequest(routingCtx.RequestID, prefillPod.Name)
	if llmEngine == SGLangEngine {
		go func() {
			defer r.prefillRequestTracker.RemovePrefillRequest(routingCtx.RequestID)
			if _, err := r.executeHTTPRequest(apiURL, routingCtx, payload); err != nil {
				klog.ErrorS(err, "prefill request for sglang failed", "request_id", routingCtx.RequestID)
				return
			}
			klog.InfoS("prefill_request_complete",
				"request_id", routingCtx.RequestID,
				"prefill_pod_name", prefillPod.Name,
				"elapsed", routingCtx.Elapsed(time.Now()))
		}()
	} else if llmEngine == VLLMEngine {
		defer r.prefillRequestTracker.RemovePrefillRequest(routingCtx.RequestID)
		responseData, err := r.executeHTTPRequest(apiURL, routingCtx, payload)
		if err != nil {
			return fmt.Errorf("failed to execute prefill request: %w", err)
		}

		// Update routing context with KV transfer params from prefill response for vLLM
		if err := r.updateRoutingContextWithKVTransferParams(routingCtx, responseData, prefillPod); err != nil {
			return fmt.Errorf("failed to update routing context with KV transfer params: %w", err)
		}

		klog.InfoS("prefill_request_complete",
			"request_id", routingCtx.RequestID,
			"prefill_pod_name", prefillPod.Name,
			"elapsed", routingCtx.Elapsed(time.Now()))
	} else {
		defer r.prefillRequestTracker.RemovePrefillRequest(routingCtx.RequestID)
		if _, err := r.executeHTTPRequest(apiURL, routingCtx, payload); err != nil {
			return fmt.Errorf("failed to execute prefill request: %w", err)
		}
		klog.InfoS("prefill_request_complete",
			"request_id", routingCtx.RequestID,
			"prefill_pod_name", prefillPod.Name,
			"elapsed", routingCtx.Elapsed(time.Now()))
	}

	return nil
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

// getTargetPodOnLoadImbalance evaluates if the load is imbalanced based on the abs difference between
// pods with min and max outstanding request counts
func (r *pdRouter) loadImbalanceSelectPrefillPod(readyPods []*v1.Pod, podRequestCount map[string]int32) (*v1.Pod, bool) {
	var imbalance bool
	var targetPod *v1.Pod
	targetPods := []string{}
	minValue := int32(math.MaxInt32)
	maxValue := int32(math.MinInt32)

	// Handle empty podRequestCount case
	if len(podRequestCount) == 0 {
		return targetPod, imbalance
	}

	// Find min/max values
	for _, value := range podRequestCount {
		if value < minValue {
			minValue = value
		}
		if value > maxValue {
			maxValue = value
		}
	}
	for podname, value := range podRequestCount {
		if minValue == value {
			targetPods = append(targetPods, podname)
		}
	}

	if maxValue-minValue > 32 && len(targetPods) > 0 {
		targetPod, _ = utils.FilterPodByName(targetPods[rand.Intn(len(targetPods))], readyPods)
		imbalance = true
	}

	return targetPod, imbalance
}

func (r *pdRouter) loadImbalanceSelectDecodePod(ctx *types.RoutingContext, filteredDecodePods []*v1.Pod) (*v1.Pod, float64, float64, float64, map[string]float64, map[string]float64, map[string]float64) {
	podRequestCounts := make(map[string]float64)
	podThroughputs := make(map[string]float64)
	podFreeGpuUsage := make(map[string]float64)

	minRequestPod := filteredDecodePods[0]
	minRequestCount := math.MaxFloat64
	maxRequestCount := float64(1)

	minThroughputPod := filteredDecodePods[0]
	minThroughput := float64(math.MaxFloat64)
	maxThroughput := float64(1)

	minFreeGPUUsage := float64(math.MaxFloat64)
	maxFreeGPUUsage := float64(100)

	for _, pod := range filteredDecodePods {
		runningReqs, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
		if err != nil {
			runningReqs = &metrics.SimpleMetricValue{Value: 0}
		}
		requestCount := runningReqs.GetSimpleValue()
		podRequestCounts[pod.Name] = requestCount
		if requestCount < minRequestCount {
			minRequestCount = requestCount
			minRequestPod = pod
		}
		maxRequestCount = math.Max(maxRequestCount, requestCount)

		tokenThroughput, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgGenerationThroughputToksPerS)
		if err != nil {
			tokenThroughput = &metrics.SimpleMetricValue{Value: 0}
		}
		throughput := tokenThroughput.GetSimpleValue()
		podThroughputs[pod.Name] = throughput
		if throughput < minThroughput {
			minThroughput = throughput
			minThroughputPod = pod
		}
		maxThroughput = math.Max(maxThroughput, throughput)

		gpuUsage, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.GPUCacheUsagePerc)
		if err != nil {
			gpuUsage = &metrics.SimpleMetricValue{Value: 0}
		}
		podFreeGpuUsage[pod.Name] = 100 - gpuUsage.GetSimpleValue()*100
		if podFreeGpuUsage[pod.Name] <= 0 {
			podFreeGpuUsage[pod.Name] = 0.1
		}
		minFreeGPUUsage = math.Min(minFreeGPUUsage, podFreeGpuUsage[pod.Name])
		maxFreeGPUUsage = math.Max(maxFreeGPUUsage, podFreeGpuUsage[pod.Name])
	}

	if minRequestCount == 0 || maxRequestCount-minRequestCount >= aibrixDecodeMaxRequest {
		klog.V(4).InfoS("REQUEST_SELECTED_DECODE_POD", "request_id", ctx.RequestID,
			"min_request_count", minRequestCount, "max_request_count", maxRequestCount,
			"min_throughput", minThroughput, "max_throughput", maxThroughput,
			"free_gpu_percent", podFreeGpuUsage[minRequestPod.Name],
			"decode_pod", minRequestPod.Name)
		return minRequestPod, maxRequestCount, maxFreeGPUUsage, maxThroughput, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	if maxThroughput-minThroughput > aibrixDecodeMaxThroughputDiff {
		klog.V(4).InfoS("THROUGHPUT_SELECTED_DECODE_POD", "request_id", ctx.RequestID,
			"min_request_count", minRequestCount, "max_request_count", maxRequestCount,
			"min_throughput", minThroughput, "max_throughput", maxThroughput,
			"free_gpu_percent", podFreeGpuUsage[minThroughputPod.Name],
			"decode_pod", minThroughputPod.Name)
		return minThroughputPod, maxRequestCount, maxFreeGPUUsage, maxThroughput, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	return nil, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
}

func (t *PrefillRequestTracker) AddPrefillRequest(requestID, podName string) {
	countInterface, _ := t.podRequestCounts.LoadOrStore(podName, &atomic.Int32{})
	count := countInterface.(*atomic.Int32)

	// Increment counter
	newCount := count.Add(1)

	// Track request to pod mapping for cleanup
	t.requestToPod.Store(requestID, podName)

	klog.V(4).InfoS("prefill_request_added",
		"request_id", requestID,
		"pod_name", podName,
		"new_count", newCount)
}

func (t *PrefillRequestTracker) RemovePrefillRequest(requestID string) {
	podNameInterface, exists := t.requestToPod.LoadAndDelete(requestID)
	if !exists {
		klog.V(4).InfoS("prefill_request_not_found_for_removal", "request_id", requestID)
		return
	}

	podName := podNameInterface.(string)
	countInterface, exists := t.podRequestCounts.Load(podName)
	if !exists {
		klog.V(4).InfoS("pod_counter_not_found", "pod_name", podName, "request_id", requestID)
		return
	}

	count := countInterface.(*atomic.Int32)
	newCount := count.Add(-1)

	// Ensure count doesn't go below zero
	if newCount < 0 {
		count.Store(0)
		newCount = 0
	}

	klog.V(4).InfoS("prefill_request_removed",
		"request_id", requestID,
		"pod_name", podName,
		"new_count", newCount)
}

func (t *PrefillRequestTracker) GetPrefillRequestCountsForPods(pods []*v1.Pod) map[string]int32 {
	counts := make(map[string]int32)
	for _, pod := range pods {
		countInterface, exists := t.podRequestCounts.Load(pod.Name)
		if !exists {
			counts[pod.Name] = 0
		} else {
			counts[pod.Name] = countInterface.(*atomic.Int32).Load()
		}
	}
	return counts
}
