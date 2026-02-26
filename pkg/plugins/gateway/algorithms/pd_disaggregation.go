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
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
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
	CombinedIdentifier            string                 = "model.aibrix.ai/combined"
	RoleReplicaIndex              string                 = "stormservice.orchestration.aibrix.ai/role-replica-index"
	PodGroupIndex                 string                 = "stormservice.orchestration.aibrix.ai/pod-group-index"
	PromptMinLength               string                 = "prompt-min-length"
	PromptMaxLength               string                 = "prompt-max-length"
	defaultPrefillRequestTimeout  int                    = 30

	defaultMaxRequest                   float64 = 32
	defaultMaxTokenThroughputDiff       float64 = 2048
	defaultRequestRateHighLoadThreshold         = 1.0
	defaultRequestRateLowLoadThreshold          = 0.25

	pdRouteValidateLLMEngineFail        = "pd-validate-llm-engine-fail"
	pdRouteFilterPrefillDecodePodsFail  = "pd-filter-prefill-decode-pods-fail"
	pdRoutePrefillRequestError          = "pd-do-prefill-request-error"
	pdRoutePrefillRequestSuccess        = "pd-prefill-request-success"
	pdRoutePrefillEmptyKVTransferParams = "pd-prefill-empty-kv-transfer-params"
)

const (
	// KV connector types for different backends
	KVConnectorTypeSHFS = "shfs" // Default - AIBrix SHFS/KVCacheManager (GPU)
	KVConnectorTypeNIXL = "nixl" // NIXL for Neuron (uses disagg_prefill_resp wrapper)
)

var (
	prefillRequestTimeout         int     = utils.LoadEnvInt("AIBRIX_PREFILL_REQUEST_TIMEOUT", defaultPrefillRequestTimeout)
	aibrixDecodeMaxRequest        float64 = utils.LoadEnvFloat("AIBRIX_DECODE_MAX_REQUEST", defaultMaxRequest)
	aibrixDecodeMaxThroughputDiff float64 = utils.LoadEnvFloat("AIBRIX_DECODE_MAX_THROUGHPUT", defaultMaxTokenThroughputDiff)
	aibrixPromptLengthBucketing   bool    = utils.LoadEnvBool("AIBRIX_PROMPT_LENGTH_BUCKETING", false)
	// KV connector type: "shfs" (default) for GPU/SHFS, "nixl" for Neuron
	aibrixKVConnectorType string = utils.LoadEnv("AIBRIX_KV_CONNECTOR_TYPE", KVConnectorTypeSHFS)
)

func init() {
	Register(RouterPD, NewPDRouter)
}

type pdRouter struct {
	cache                 cache.Cache
	tokenizer             tokenizer.Tokenizer
	prefixCacheIndexer    *prefixcacheindexer.PrefixHashTable
	prefillRequestTracker *PrefillRequestTracker
	httpClient            *http.Client
	prefixUpdateCh        chan prefixUpdateJob
	countersMu            sync.RWMutex
	selectionCounts       map[string]int64
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

	pdRouter := pdRouter{
		cache:                 c,
		tokenizer:             tokenizerObj,
		prefixCacheIndexer:    prefixcacheindexer.GetSharedPrefixHashTable(),
		prefillRequestTracker: NewPrefillRequestTracker(),
		httpClient:            httpClient,
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       make(map[string]int64),
	}

	pdRouter.startPrefixUpdater()
	return &pdRouter, nil
}

// NewPrefillRequestTracker creates a new prefill request tracker
func NewPrefillRequestTracker() *PrefillRequestTracker {
	return &PrefillRequestTracker{
		podRequestCounts: sync.Map{},
		requestToPod:     sync.Map{},
	}
}

func (r *pdRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	// Validate engine consistency across all prefill pods
	llmEngine, err := validateAndGetLLMEngine(readyPodList.All())
	if err != nil {
		metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": pdRouteValidateLLMEngineFail, "status_code": "400"})
		return "", fmt.Errorf("engine validation failed for request %s: %w", ctx.RequestID, err)
	}

	prefillPod, decodePod, err := r.filterPrefillDecodePods(ctx, readyPodList.All())
	if err != nil {
		metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": pdRouteFilterPrefillDecodePodsFail, "status_code": "400"})
		return "", fmt.Errorf("failed to filter prefill/decode pods for request %s: %w", ctx.RequestID, err)
	}

	if prefillPod != nil {
		klog.InfoS("selected prefill/decode pods", "request_id", ctx.RequestID, "prefill_pod", prefillPod.Name, "decode_pod", decodePod.Name)
		err = r.doPrefillRequest(ctx, prefillPod, llmEngine)
		if err != nil {
			metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
				map[string]string{"status": pdRoutePrefillRequestError, "status_code": "500"})
			klog.ErrorS(err, pdRoutePrefillRequestError, "request_id", ctx.RequestID)
			return "", fmt.Errorf("prefill request failed for request %s: %w", ctx.RequestID, err)
		}
		metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestSuccessTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": pdRoutePrefillRequestSuccess, "status_code": "200"})
	}

	ctx.SetTargetPod(decodePod)
	return ctx.TargetAddress(), nil
}

type Scores struct {
	Pod   *v1.Pod
	Score float64
}

// filterPrefillDecodePods filters pods into prefill and decode categories.
// For multi-node tensor parallelism (e.g., TP=16 with node_rank=0 and node_rank=1),
// only pods with PodGroupIndex="0" (node_rank=0) are selected as they run the HTTP server.
// Pods without PodGroupIndex label are also included for backward compatibility.
func (r *pdRouter) filterPrefillDecodePods(routingCtx *types.RoutingContext, readyPods []*v1.Pod) (*v1.Pod, *v1.Pod, error) {
	var promptLength int
	if aibrixPromptLengthBucketing {
		promptLength, _ = routingCtx.PromptLength()
		klog.V(4).InfoS("prompt length based filtering enabled", "request_id", routingCtx.RequestID, "prompt_length", promptLength)
	}

	prefillPods, decodePods, promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods, combinedPods := r.collectAndBucketPods(readyPods, promptLength)
	combinedAvailable := aibrixPromptLengthBucketing && len(combinedPods) > 0
	if len(prefillPods) == 0 && !combinedAvailable {
		return nil, nil, fmt.Errorf("prefill pods are not ready: prefill=%d, decode=%d", len(prefillPods), len(decodePods))
	}
	if len(decodePods) == 0 && !combinedAvailable {
		return nil, nil, fmt.Errorf("decode pods are not ready: prefill=%d, decode=%d", len(prefillPods), len(decodePods))
	}
	if combinedAvailable {
		if len(promptLengthBucketingPrefillPods) == 0 || len(promptLengthBucketingDecodePods) == 0 {
			klog.InfoS("routing to combined pod", "requestId", routingCtx.RequestID, "promptLength", promptLength)
			return nil, combinedPods[rand.Intn(len(combinedPods))], nil
		}

		if r.shouldPickCombined(routingCtx, promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods, combinedPods) {
			combinedPod := r.scoreCombinedPods(routingCtx, combinedPods)
			if combinedPod != nil {
				klog.InfoS("load imbalance detected, selecting combined pod",
					"requestId", routingCtx.RequestID, "selectedCombinedPod", combinedPod.Name)
				return nil, combinedPod, nil
			}
		}
	}

	// check for prefill and decode imbalance
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

	prefillScores, maxPrefillScore, prefixHashes := r.scorePrefillPods(routingCtx, prefillPods)
	decodeScores, maxDecodeScore := r.scoreDecodePods(routingCtx, decodePods, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage)
	return r.finalPDScore(routingCtx, prefixHashes, prefillScores, maxPrefillScore, decodeScores, maxDecodeScore)
}

// loadImbalanceSelectPrefillPod evaluates if the load is imbalanced based on the abs difference between
// pods with min and max outstanding request counts
func (r *pdRouter) loadImbalanceSelectPrefillPod(readyPods []*v1.Pod, podRequestCount map[string]int32) (*v1.Pod, bool) {
	var imbalance bool
	var targetPod *v1.Pod
	targetPods := []string{}
	minValue := int32(math.MaxInt32)
	maxValue := int32(math.MinInt32)
	utils.CryptoShuffle(readyPods)

	if len(podRequestCount) == 0 {
		return targetPod, imbalance
	}

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

// loadImbalanceSelectDecodePod identifies imbalance decode pod using abs diff of max/min request counts and max/min throughputs.
// It returns the selected pod, min/max request counts, min/max throughputs, and min/max free GPU usage
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
	maxFreeGPUUsage := float64(1)
	utils.CryptoShuffle(filteredDecodePods)

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
		podFreeGpuUsage[pod.Name] = math.Round(100 - gpuUsage.GetSimpleValue()*100)
		if podFreeGpuUsage[pod.Name] <= 0 {
			podFreeGpuUsage[pod.Name] = 0.1
		}
		minFreeGPUUsage = math.Min(minFreeGPUUsage, podFreeGpuUsage[pod.Name])
		maxFreeGPUUsage = math.Max(maxFreeGPUUsage, podFreeGpuUsage[pod.Name])
	}

	if minRequestCount == 0 || maxRequestCount-minRequestCount >= aibrixDecodeMaxRequest {
		klog.V(4).InfoS("request imbalance at decode pods", "request_id", ctx.RequestID,
			"min_request_count", minRequestCount, "max_request_count", maxRequestCount,
			"min_throughput", minThroughput, "max_throughput", maxThroughput,
			"free_gpu_percent", podFreeGpuUsage[minRequestPod.Name],
			"decode_pod", minRequestPod.Name)
		return minRequestPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	if maxThroughput-minThroughput > aibrixDecodeMaxThroughputDiff {
		klog.V(4).InfoS("throughput imbalance at decode pods", "request_id", ctx.RequestID,
			"min_request_count", minRequestCount, "max_request_count", maxRequestCount,
			"min_throughput", minThroughput, "max_throughput", maxThroughput,
			"free_gpu_percent", podFreeGpuUsage[minThroughputPod.Name],
			"decode_pod", minThroughputPod.Name)
		return minThroughputPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	return nil, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
}

// scorePrefillPods scores prefill pods using formula (100 - match_percent) * 0.1 + (req_cnt / max_request_count)
// selects pod with lowest score indicating highest percent match and least request count.
func (r *pdRouter) scorePrefillPods(routingCtx *types.RoutingContext, prefillPods []*v1.Pod) (map[string]*Scores, float64, []uint64) {
	prefillScores := map[string]*Scores{}
	tokens, err := r.tokenizer.TokenizeInputText(routingCtx.Message)
	if err != nil {
		return nil, 0, nil
	}
	utils.CryptoShuffle(prefillPods)

	var maxRequestCount float64 = 1
	requestCount := []float64{}
	readyPodsMap := map[string]struct{}{}
	podRequestCount := r.prefillRequestTracker.GetPrefillRequestCountsForPods(prefillPods)

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
			klog.V(4).InfoS("prefill pod request count is higher than mean request count, skipping", "request_id", routingCtx.RequestID, "pod_name", pod.Name,
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

		klog.V(4).InfoS("prefill_score", "request_id", routingCtx.RequestID, "pod_name", pod.Name,
			"prefill_score", prefillScore,
			"score", fmt.Sprintf("(100 - %f) * 0.1 + %f / %f", float64(matchedPods[pod.Name]), reqCnt, maxRequestCount),
			"prefix_match_percent", float64(matchedPods[pod.Name]),
			"running_reqs", reqCnt, "max_running_reqs", maxRequestCount)
	}

	return prefillScores, maxPrefillScore, prefixHashes
}

// scoreDecodePods scores decode pods using formula (running_reqs / max_request_count) + (1 - throughput / max_throughput) / (1 - free_gpu_usage / max_free_gpu_usage)
// selects pod with lowest score indicating least running requests, highest throughput and highest free GPU usage.
func (r *pdRouter) scoreDecodePods(routingCtx *types.RoutingContext, filteredDecodePods []*v1.Pod,
	maxRequestCount float64, maxThroughput float64, maxFreeGPUUsage float64,
	podRequestCounts map[string]float64, podThroughputs map[string]float64, podFreeGpuUsage map[string]float64) (map[string]*Scores, float64) {
	decodeScores := map[string]*Scores{}
	maxDecodeScore := float64(0.01)
	utils.CryptoShuffle(filteredDecodePods)

	for _, pod := range filteredDecodePods {
		rolesetName := pod.Labels[PDRoleSetIdentifier]

		normalizedRunningReqs := podRequestCounts[pod.Name] / maxRequestCount
		normalizedThroughput := 1 - podThroughputs[pod.Name]/maxThroughput
		normalizedFreeGPUPercent := podFreeGpuUsage[pod.Name] / maxFreeGPUUsage

		decodeScore := (normalizedRunningReqs + normalizedThroughput) / normalizedFreeGPUPercent
		if existingScore, exists := decodeScores[rolesetName]; !exists || decodeScore < existingScore.Score {
			decodeScores[rolesetName] = &Scores{
				Pod:   pod,
				Score: decodeScore,
			}
		}
		if decodeScore > maxDecodeScore {
			maxDecodeScore = decodeScore
		}

		klog.V(4).InfoS("decode_score", "request_id", routingCtx.RequestID, "pod_name", pod.Name, "decode_score", decodeScore,
			"score", fmt.Sprintf("(%f + %f) / %f", normalizedRunningReqs, normalizedThroughput, normalizedFreeGPUPercent),
			"running_reqs", podRequestCounts[pod.Name], "max_running_reqs", maxRequestCount,
			"throughput", podThroughputs[pod.Name], "max_throughput", maxThroughput,
			"free_gpu", podFreeGpuUsage[pod.Name], "max_free_gpu_usage", maxFreeGPUUsage)
	}

	return decodeScores, maxDecodeScore
}

func (r *pdRouter) finalPDScore(routingCtx *types.RoutingContext,
	prefixHashes []uint64,
	prefillScores map[string]*Scores, maxPrefillScore float64,
	decodeScores map[string]*Scores, maxDecodeScore float64,
) (*v1.Pod, *v1.Pod, error) {
	var targetPrefillPod, targetDecodePod *v1.Pod
	minScore := math.MaxFloat64

	for roleset, prefillScore := range prefillScores {
		decodeScore, ok := decodeScores[roleset]
		if !ok {
			continue
		}

		normalizedPrefillScore := prefillScore.Score / maxPrefillScore
		normalizedDecodeScore := decodeScore.Score / maxDecodeScore
		final := normalizedPrefillScore + normalizedDecodeScore

		if final < minScore {
			minScore = final
			targetPrefillPod = prefillScore.Pod
			targetDecodePod = decodeScore.Pod
		}

		klog.V(4).InfoS(
			"final_score",
			"request_id", routingCtx.RequestID,
			"roleset", roleset,
			"final_score", final,
			"prefill_score", prefillScore.Score, "normalized_prefill_score", normalizedPrefillScore,
			"decode_score", decodeScore.Score, "normalized_decode_score", normalizedDecodeScore,
		)
	}

	if targetPrefillPod == nil {
		return nil, nil, fmt.Errorf("target prefill pod is nil")
	}
	if targetDecodePod == nil {
		return nil, nil, fmt.Errorf("target decode pod is nil")
	}
	if len(prefixHashes) > 0 && targetPrefillPod != nil {
		r.enqueuePrefixUpdate(prefixHashes, routingCtx.Model, targetPrefillPod.Name)
	}

	r.countersMu.Lock()
	r.selectionCounts[targetPrefillPod.Name]++
	r.selectionCounts[targetDecodePod.Name]++
	r.countersMu.Unlock()

	metrics.EmitMetricToPrometheus(routingCtx, targetPrefillPod, metrics.PDSelectedPrefillPodTotal, &metrics.SimpleMetricValue{Value: 1.0}, nil)
	metrics.EmitMetricToPrometheus(routingCtx, targetDecodePod, metrics.PDSelectedDecodePodTotal, &metrics.SimpleMetricValue{Value: 1.0}, nil)

	return targetPrefillPod, targetDecodePod, nil
}
func (r *pdRouter) doPrefillRequest(routingCtx *types.RoutingContext, prefillPod *v1.Pod, llmEngine string) error {
	// Prepare prefill request payload
	payload, err := r.preparePrefillPayload(routingCtx, prefillPod, llmEngine)
	if err != nil {
		return fmt.Errorf("failed to prepare prefill payload for request %s: %w", routingCtx.RequestID, err)
	}

	// Execute HTTP request
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
		"outstanding_prefill_requests", r.prefillRequestTracker.GetPrefillRequestCountsForPod(prefillPod.Name),
	}
	klog.InfoS("prefill_request_start", fields...)
	if len(fields) >= 2 {
		fields = fields[:len(fields)-2]
	}

	r.prefillRequestTracker.AddPrefillRequest(routingCtx.RequestID, prefillPod.Name)
	routingCtx.PrefillStartTime = time.Now()

	switch llmEngine {
	case SGLangEngine:
		// For SGLang, use async prefill - the bootstrap mechanism (bootstrap_host/port/room)
		// coordinates between prefill and decode pods, so we don't need to wait
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

	case VLLMEngine:
		defer r.prefillRequestTracker.RemovePrefillRequest(routingCtx.RequestID)

		// For vLLM, wait synchronously to get KV transfer params from response
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

		// Update routing context with KV transfer params from prefill response
		if err := r.updateRoutingContextWithKVTransferParams(routingCtx, responseData, prefillPod); err != nil {
			return fmt.Errorf("failed to update routing context with KV transfer params for request %s: %w", routingCtx.RequestID, err)
		}

		routingCtx.PrefillEndTime = time.Now()
		fields = append(fields,
			"routing_time_taken", routingCtx.PrefillStartTime.Sub(routingCtx.RequestTime),
			"prefill_time_taken", routingCtx.PrefillEndTime.Sub(routingCtx.PrefillStartTime),
			"outstanding_prefill_requests", r.prefillRequestTracker.GetPrefillRequestCountsForPod(prefillPod.Name)-1)
		klog.InfoS("prefill_request_end", fields...)

	default:
		defer r.prefillRequestTracker.RemovePrefillRequest(routingCtx.RequestID)

		// For unknown engines, use synchronous approach as a safe default
		if _, err := r.executeHTTPRequest(apiURL, routingCtx, payload); err != nil {
			klog.ErrorS(err, "prefill_request_failed",
				"request_id", routingCtx.RequestID,
				"llm_engine", llmEngine,
				"prefill_pod", prefillPod.Name,
				"prefill_pod_ip", prefillPod.Status.PodIP,
				"elapsed", routingCtx.Elapsed(time.Now()))
			return fmt.Errorf("prefill request failed for request %s, pod %s: %w", routingCtx.RequestID, prefillPod.Name, err)
		}
		routingCtx.PrefillEndTime = time.Now()
		fields = append(fields,
			"routing_time_taken", routingCtx.PrefillStartTime.Sub(routingCtx.RequestTime),
			"prefill_time_taken", routingCtx.PrefillEndTime.Sub(routingCtx.PrefillStartTime),
			"outstanding_prefill_requests", r.prefillRequestTracker.GetPrefillRequestCountsForPod(prefillPod.Name)-1)
		klog.InfoS("prefill_request_end", fields...)
	}

	return nil
}

func (r *pdRouter) preparePrefillPayload(routingCtx *types.RoutingContext, pod *v1.Pod, llmEngine string) ([]byte, error) {
	var completionRequest map[string]any
	if err := sonic.Unmarshal(routingCtx.ReqBody, &completionRequest); err != nil {
		return nil, fmt.Errorf("failed to unmarshal prefill request body: %w", err)
	}

	// Handle SGLang specific configuration
	if llmEngine == SGLangEngine {
		completionRequest["bootstrap_host"] = pod.Status.PodIP
		completionRequest["bootstrap_port"] = getSGLangBootstrapPort(pod)
		completionRequest["bootstrap_room"] = rand.Int63n(1<<63 - 1)

		// Create a copy of the request body
		reqBody, err := sonic.Marshal(completionRequest)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal post prefill request body: %w", err)
		}
		bodyCopy := make([]byte, len(reqBody))
		copy(bodyCopy, reqBody)
		routingCtx.ReqBody = bodyCopy
	}

	// Add kv_transfer_params only for SHFS mode with vLLM
	// For NIXL mode, the backend handles KV transfer via its own mechanism
	if llmEngine == VLLMEngine && aibrixKVConnectorType == KVConnectorTypeSHFS {
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

	return sonic.Marshal(completionRequest)
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
		status, code := metrics.HttpFailureStatusCode(ctx, err, nil)
		metrics.EmitMetricToPrometheus(routingCtx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": status, "status_code": code})
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
		status, code := metrics.HttpFailureStatusCode(ctx, nil, resp)
		metrics.EmitMetricToPrometheus(routingCtx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": status, "status_code": code})
		return nil, fmt.Errorf("http prefill request failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response JSON
	var responseData map[string]any
	if err := sonic.Unmarshal(body, &responseData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal prefill response: %w", err)
	}

	return responseData, nil
}

func (r *pdRouter) updateRoutingContextWithKVTransferParams(routingCtx *types.RoutingContext, responseData map[string]any, prefillPod *v1.Pod) error {
	// Parse the original request body
	var originalRequest map[string]any
	if err := sonic.Unmarshal(routingCtx.ReqBody, &originalRequest); err != nil {
		return fmt.Errorf("failed to unmarshal original request body: %w", err)
	}

	// Handle NIXL mode (Neuron) - wrap entire prefill response in disagg_prefill_resp
	if aibrixKVConnectorType == KVConnectorTypeNIXL {
		// For NIXL, wrap the entire prefill response for decode to process
		// This is the format expected by Neuron's NixlConnector
		originalRequest["disagg_prefill_resp"] = responseData

		// Marshal the updated request body
		updatedReqBody, err := sonic.Marshal(originalRequest)
		if err != nil {
			return fmt.Errorf("failed to marshal updated request body: %w", err)
		}

		// Update routing context with new request body
		routingCtx.ReqBody = updatedReqBody

		klog.InfoS("updated routing context with disagg_prefill_resp (NIXL mode)",
			"request_id", routingCtx.RequestID,
			"prefill_pod", prefillPod.Name,
			"prefill_host", prefillPod.Status.PodIP,
			"kv_connector_type", aibrixKVConnectorType)
	} else {
		// SHFS mode (default) - use kv_transfer_params with remote_host
		// Extract kv_transfer_params from prefill response
		kvTransferParams, exists := responseData["kv_transfer_params"]
		if !exists {
			klog.InfoS("no kv_transfer_params in prefill response", "request_id", routingCtx.RequestID)
			return nil
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
		updatedReqBody, err := sonic.Marshal(originalRequest)
		if err != nil {
			return fmt.Errorf("failed to marshal updated request body: %w", err)
		}

		// Update routing context with new request body
		routingCtx.ReqBody = updatedReqBody

		klog.InfoS("updated routing context with kv_transfer_params (SHFS mode)",
			"request_id", routingCtx.RequestID,
			"prefill_pod", prefillPod.Name,
			"prefill_host", prefillPod.Status.PodIP,
			"kv_connector_type", aibrixKVConnectorType)
	}

	return nil
}

func (r *pdRouter) SubscribedMetrics() []string {
	return []string{}
}

type prefixUpdateJob struct {
	prefixHashes []uint64
	model        string
	pod          string
}

func (r *pdRouter) startPrefixUpdater() {
	// single worker to serialize updates, minimizing lock contention in the indexer
	go func() {
		for job := range r.prefixUpdateCh {
			r.prefixCacheIndexer.AddPrefix(job.prefixHashes, job.model, job.pod)
		}
	}()
}

func (r *pdRouter) enqueuePrefixUpdate(prefixHashes []uint64, model, pod string) {
	// copy slice to avoid data races if caller reuses the backing array
	copyHashes := append([]uint64(nil), prefixHashes...)
	select {
	case r.prefixUpdateCh <- prefixUpdateJob{
		prefixHashes: copyHashes,
		model:        model,
		pod:          pod,
	}:
		// enqueued
	default:
		// channel full; drop to keep routing path non-blocking
		klog.Warningf("Prefix update channel full, dropping update for model %s on pod %s", model, pod)
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

func (t *PrefillRequestTracker) GetPrefillRequestCountsForPod(podname string) int {
	countInterface, exists := t.podRequestCounts.Load(podname)
	if !exists {
		return 0
	}
	return int(countInterface.(*atomic.Int32).Load())
}

func (r *pdRouter) isPodSuitableForPromptLength(pod *v1.Pod, promptLength int) bool {
	minLength, maxLength := r.getPodPromptRange(pod)

	if minLength > maxLength {
		return false
	}
	// If no prompt length range is configured, the pod is assumed to be suitable for handling any length.
	if minLength == 0 && maxLength == math.MaxInt32 {
		return true
	}

	return promptLength >= minLength && promptLength <= maxLength
}

// getPodPromptRange retrieves the minimum and maximum prompt lengths from pod labels.
func (r *pdRouter) getPodPromptRange(pod *v1.Pod) (int, int) {
	minLength := 0
	maxLength := math.MaxInt32

	if val, ok := pod.Labels[PromptMinLength]; ok {
		if parsed, err := strconv.Atoi(val); err == nil {
			minLength = parsed
		}
	}

	if val, ok := pod.Labels[PromptMaxLength]; ok {
		if parsed, err := strconv.Atoi(val); err == nil {
			maxLength = parsed
		}
	}

	return minLength, maxLength
}

func isCombinedPod(pod *v1.Pod) bool {
	return pod != nil && pod.Labels[CombinedIdentifier] == "true"
}

func (r *pdRouter) collectAndBucketPods(readyPods []*v1.Pod, promptLength int) ([]*v1.Pod, []*v1.Pod, []*v1.Pod, []*v1.Pod, []*v1.Pod) {
	prefillPods, decodePods := []*v1.Pod{}, []*v1.Pod{}
	promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods, promptLengthBucketingCombinedPods := []*v1.Pod{}, []*v1.Pod{}, []*v1.Pod{}

	for _, pod := range readyPods {
		if _, ok := pod.Labels[PDRoleSetIdentifier]; !ok {
			continue
		}
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
			if aibrixPromptLengthBucketing && r.isPodSuitableForPromptLength(pod, promptLength) {
				promptLengthBucketingPrefillPods = append(promptLengthBucketingPrefillPods, pod)
			}
		case "decode":
			decodePods = append(decodePods, pod)
			if aibrixPromptLengthBucketing && r.isPodSuitableForPromptLength(pod, promptLength) {
				promptLengthBucketingDecodePods = append(promptLengthBucketingDecodePods, pod)
			}
		default:
			if aibrixPromptLengthBucketing && isCombinedPod(pod) && r.isPodSuitableForPromptLength(pod, promptLength) {
				promptLengthBucketingCombinedPods = append(promptLengthBucketingCombinedPods, pod)
			}
		}
	}

	// Override prefill pods only if bucketing produced results
	if aibrixPromptLengthBucketing && len(promptLengthBucketingPrefillPods) > 0 {
		prefillPods = promptLengthBucketingPrefillPods
	}
	if aibrixPromptLengthBucketing && len(promptLengthBucketingDecodePods) > 0 {
		decodePods = promptLengthBucketingDecodePods
	}

	return prefillPods, decodePods, promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods, promptLengthBucketingCombinedPods
}

func (r *pdRouter) shouldPickCombined(routingCtx *types.RoutingContext, prefillPods, decodePods, combinedPods []*v1.Pod) bool {
	combinedLowLoad := false
	for _, combinePod := range combinedPods {
		if calculatePodScoreBasedOffRequestRate(routingCtx, r.cache, combinePod) < defaultRequestRateLowLoadThreshold {
			combinedLowLoad = true
			break
		}
	}
	if !combinedLowLoad {
		klog.V(4).InfoS("combined_load", "requestId", routingCtx.RequestID, "prefillHighLoad", false, "decodeHighLoad", false, "combinedLowLoad", combinedLowLoad)
		return false
	}

	prefillHighLoad := false
	for _, prefillPod := range prefillPods {
		if calculatePodScoreBasedOffRequestRate(routingCtx, r.cache, prefillPod) > defaultRequestRateHighLoadThreshold {
			prefillHighLoad = true
			break
		}
	}

	decodeHighLoad := false
	if !prefillHighLoad {
		for _, decodePod := range decodePods {
			if calculatePodScoreBasedOffRequestRate(routingCtx, r.cache, decodePod) > defaultRequestRateHighLoadThreshold {
				decodeHighLoad = true
				break
			}
		}
	}

	klog.V(4).InfoS("loads", "requestId", routingCtx.RequestID, "prefillHighLoad", prefillHighLoad, "decodeHighLoad", decodeHighLoad, "combinedLowLoad", combinedLowLoad)
	return (prefillHighLoad || decodeHighLoad) && combinedLowLoad
}

func (r *pdRouter) scoreCombinedPods(routingCtx *types.RoutingContext, combinedPods []*v1.Pod) *v1.Pod {
	utils.CryptoShuffle(combinedPods)
	var bestPod *v1.Pod
	minScore := math.MaxFloat64
	for _, pod := range combinedPods {
		score := calculatePodScoreBasedOffRequestRate(routingCtx, r.cache, pod)
		klog.V(4).InfoS("combined_pod_score", "requestId", routingCtx.RequestID, "pod_name", pod.Name, "score", score)
		if score < minScore {
			minScore = score
			bestPod = pod
		}
	}
	return bestPod
}
