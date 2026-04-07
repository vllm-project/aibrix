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
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// sonicJSONInt64 unmarshals JSON numbers into map[string]any as int64 (not float64), so large
// integer fields (e.g. ctx_request_id, disagg_request_id in disaggregated_params) survive
// marshal/unmarshal without float64 precision loss.
var sonicJSONInt64 = sonic.Config{UseInt64: true}.Froze()

const (
	RouterPD                      types.RoutingAlgorithm = "pd"
	VLLMEngine                    string                 = "vllm"
	SGLangEngine                  string                 = "sglang"
	TensorRTLLM                   string                 = "trtllm"
	SGLangBootstrapPort           int64                  = 8998
	SGLangBootstrapPortIdentifier string                 = "model.aibrix.ai/sglang-bootstrap-port"
	LLMEngineIdentifier           string                 = constants.ModelLabelEngine
	PDRoleSetIdentifier           string                 = "roleset-name"
	PDRoleIdentifier              string                 = "role-name"
	RoleReplicaIndex              string                 = "stormservice.orchestration.aibrix.ai/role-replica-index"
	PodGroupIndex                 string                 = "stormservice.orchestration.aibrix.ai/pod-group-index"
	PromptLenBucketMinLength      string                 = "prompt-len-bucket-min-length"
	PromptLenBucketMaxLength      string                 = "prompt-len-bucket-max-length"
	defaultPrefillRequestTimeout  int                    = 30

	prefillScorePolicyPrefixCache  = "prefix_cache"
	prefillScorePolicyLeastRequest = "least_request"

	defaultPrefillLoadImbalanceMinSpread      int32   = 16
	defaultDecodeLoadImbalanceMinSpread       float64 = 16
	defaultDecodeThroughputImbalanceMinSpread float64 = 2048
	defaultRequestRateHighLoadThreshold               = 1.0
	defaultRequestRateLowLoadThreshold                = 0.25
	defaultDecodeScoreRatioThreshold          float64 = 1.5 // min queue-drain time ratio to trigger drain-rate routing
	defaultDrainRateEpsilon                   float64 = 0.1 // floor for drain rate to avoid division by zero

	pdRouteValidateLLMEngineFail       = "pd-validate-llm-engine-fail"
	pdRouteFilterPrefillDecodePodsFail = "pd-filter-prefill-decode-pods-fail"
	pdRoutePrefillRequestError         = "pd-do-prefill-request-error"
	pdRoutePrefillRequestSuccess       = "pd-prefill-request-success"
)

const (
	// KV connector types for different backends
	KVConnectorTypeSHFS = "shfs" // Default - AIBrix SHFS/KVCacheManager (GPU)
	KVConnectorTypeNIXL = "nixl" // NIXL for Neuron (uses disagg_prefill_resp wrapper)

	HeaderPrefillTargetPodIP = "prefill-target-pod-ip"
	HeaderPrefillTargetPod   = "prefill-target-pod"
)

var (
	// seconds before a prefill HTTP request times out
	prefillRequestTimeout int = utils.LoadEnvInt("AIBRIX_PREFILL_REQUEST_TIMEOUT", defaultPrefillRequestTimeout)
	// min (max-min) request-count spread to trigger prefill load-imbalance routing
	aibrixPrefillLoadImbalanceMinSpread int32 = int32(utils.LoadEnvInt("AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD", int(defaultPrefillLoadImbalanceMinSpread)))
	// min (max-min) request-count spread to trigger decode load-imbalance routing
	aibrixDecodeLoadImbalanceMinSpread float64 = utils.LoadEnvFloat("AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD", defaultDecodeLoadImbalanceMinSpread)
	// min (max-min) token-throughput spread (tok/s) to trigger decode throughput-imbalance routing
	aibrixDecodeThroughputImbalanceMinSpread float64 = utils.LoadEnvFloat("AIBRIX_DECODE_THROUGHPUT_IMBALANCE_MIN_SPREAD", defaultDecodeThroughputImbalanceMinSpread)
	// max/min drain-rate score ratio above which the slowest decode pod is avoided
	aibrixDecodeScoreRatioThreshold float64 = utils.LoadEnvFloat("AIBRIX_DECODE_SCORE_RATIO_THRESHOLD", defaultDecodeScoreRatioThreshold)
	// route to pods whose prompt-length bucket matches the request
	aibrixPromptLengthBucketing bool = utils.LoadEnvBool("AIBRIX_PROMPT_LENGTH_BUCKETING", false)
	// KV transfer backend: "shfs" (GPU/SHFS) or "nixl" (Neuron)
	aibrixKVConnectorType string = utils.LoadEnv("AIBRIX_KV_CONNECTOR_TYPE", KVConnectorTypeSHFS)
	// prefill pod scoring strategy: "prefix_cache" or "least_request"
	aibrixPrefillScorePolicy string = utils.LoadEnv("AIBRIX_PREFILL_SCORE_POLICY", prefillScorePolicyPrefixCache)
)

func init() {
	Register(RouterPD, NewPDRouter)
}

// pdAlgorithmConfig holds PD-specific algorithm configuration parsed from RoutingConfig.
type pdAlgorithmConfig struct {
	PromptLenBucketMinLength int  `json:"promptLenBucketMinLength"`
	PromptLenBucketMaxLength int  `json:"promptLenBucketMaxLength"`
	Combined                 bool `json:"combined"`
}

// parsePDAlgorithmConfig parses PD-specific config from the generic RoutingConfig.
// Returns defaults (min=0, max=MaxInt32, combined=false) if raw is nil or empty.
func parsePDAlgorithmConfig(raw json.RawMessage) *pdAlgorithmConfig {
	cfg := &pdAlgorithmConfig{
		PromptLenBucketMaxLength: math.MaxInt32,
	}
	if len(raw) == 0 {
		return cfg
	}
	if err := sonic.Unmarshal(raw, cfg); err != nil {
		klog.ErrorS(err, "failed to unmarshal PD algorithm config, using default values", "rawConfig", string(raw))
		return &pdAlgorithmConfig{PromptLenBucketMaxLength: math.MaxInt32}
	}
	if cfg.PromptLenBucketMinLength < 0 {
		cfg.PromptLenBucketMinLength = 0
	}
	if cfg.PromptLenBucketMaxLength == 0 {
		cfg.PromptLenBucketMaxLength = math.MaxInt32
	}
	return cfg
}

type pdRouter struct {
	cache                 cache.Cache
	prefillPolicy         prefillScorePolicy
	prefixCacheIndexer    *prefixcacheindexer.PrefixHashTable
	prefillRequestTracker *PrefillRequestTracker
	pendingDecodeTracker  *PendingDecodeTracker
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

// PendingDecodeTracker tracks decode pods that have been selected but whose
// RealtimeNumRequestsRunning has not yet been incremented by AddRequestCount.
// This bridges the gap between decode pod selection and the actual decode
// request starting, preventing concurrent requests from all routing to the
// same decode pod during the prefill phase.
type PendingDecodeTracker struct {
	// Map of pod name -> pending decode request count
	podRequestCounts sync.Map // map[string]*atomic.Int32
	// Map of request ID -> pod name for cleanup
	requestToPod sync.Map // map[string]string
}

func NewPDRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		klog.Error("fail to get cache store in prefix cache router")
		return nil, err
	}

	sharedPrefixTable := prefixcacheindexer.GetSharedPrefixHashTable()

	var policy prefillScorePolicy
	switch aibrixPrefillScorePolicy {
	case prefillScorePolicyLeastRequest:
		policy = &leastRequestPrefillPolicy{}
	default: // prefillScorePolicyPrefixCache
		var tok tokenizer.Tokenizer
		if tokenizerType == tokenizerTypeTiktoken {
			tok = tokenizer.NewTiktokenTokenizer()
		} else {
			tok = tokenizer.NewCharacterTokenizer()
		}
		policy = &prefixCachePrefillPolicy{
			tok:                tok,
			prefixCacheIndexer: sharedPrefixTable,
		}
	}
	klog.InfoS("pd_router prefill score policy", "policy", policy.name())

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
		prefillPolicy:         policy,
		prefixCacheIndexer:    sharedPrefixTable,
		prefillRequestTracker: NewPrefillRequestTracker(),
		pendingDecodeTracker:  NewPendingDecodeTracker(),
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
	readyPods := readyPodList.All()
	// Validate engine consistency across all prefill pods
	llmEngine, err := validateAndGetLLMEngine(readyPods)
	if err != nil {
		metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": pdRouteValidateLLMEngineFail, "status_code": "400"})
		return "", fmt.Errorf("engine validation failed for request %s: %w", ctx.RequestID, err)
	}

	prefillPod, decodePod, err := r.filterPrefillDecodePods(ctx, readyPods)
	if err != nil {
		metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": pdRouteFilterPrefillDecodePodsFail, "status_code": "400"})
		return "", fmt.Errorf("failed to filter prefill/decode pods for request %s: %w", ctx.RequestID, err)
	}

	if prefillPod != nil {
		klog.InfoS("selected prefill/decode pods", "request_id", ctx.RequestID, "prefill_pod", prefillPod.Name, "decode_pod", decodePod.Name)
		r.pendingDecodeTracker.AddPendingDecode(ctx.RequestID, decodePod.Name)
		defer r.pendingDecodeTracker.RemovePendingDecode(ctx.RequestID)
		if ctx.RespHeaders == nil {
			ctx.RespHeaders = make(map[string]string)
		}
		ctx.RespHeaders[HeaderPrefillTargetPod] = prefillPod.Name
		ctx.RespHeaders[HeaderPrefillTargetPodIP] = prefillPod.Status.PodIP
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

	prefillPods, decodePods, promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods, combinedPods := r.collectAndBucketPods(routingCtx, readyPods, promptLength)
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

	if maxValue-minValue > aibrixPrefillLoadImbalanceMinSpread && len(targetPods) > 0 {
		targetPod, _ = utils.FilterPodByName(targetPods[rand.Intn(len(targetPods))], readyPods)
		imbalance = true
	}

	return targetPod, imbalance
}

// loadImbalanceSelectDecodePod selects a decode pod when load imbalance is detected. It walks all
// filtered decode pods once, filling podRequestCounts, podThroughputs, and podFreeGpuUsage, then
// applies three ordered checks (each runs only if the previous did not return):
//
//  1. Request imbalance (fast path): if max minus min running request count (RealtimeNumRequestsRunning
//     plus pending decode count) is at least aibrixDecodeLoadImbalanceMinSpread, return the least-loaded pod.
//
//  2. Throughput spread: if max minus min AvgGenerationThroughputToksPerS (per model) is greater than
//     aibrixDecodeThroughputImbalanceMinSpread (AIBRIX_DECODE_THROUGHPUT_IMBALANCE_MIN_SPREAD), return the pod with minimum
//     throughput. Missing throughput is treated as 0, which can make that pod look like the minimum
//     during scrape gaps or startup.
//
//  3. Drain rate scoring (soft path): if every pod has a positive RealtimeRunningRequestsDrainRate1m,
//     compute time-to-drain score runningRequests/drainRate per pod. If maxScore/minScore exceeds
//     aibrixDecodeScoreRatioThreshold, return the pod with the lowest score. If any drain rate is
//     missing or non-positive, this check is skipped entirely.
//
// Returns nil when none of the above fire; the caller uses scoreDecodePods with the collected maps.
// Non-nil pod returns also carry maxRequestCount, maxThroughput, and maxFreeGPUUsage from the same pass.
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
	maxFreeGPUUsage := float64(1)
	utils.CryptoShuffle(filteredDecodePods)

	for _, pod := range filteredDecodePods {
		runningReqs, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
		if err != nil {
			runningReqs = &metrics.SimpleMetricValue{Value: 0}
		}
		requestCount := runningReqs.GetSimpleValue() + r.pendingDecodeTracker.GetPendingDecodeCount(pod.Name)
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
		maxFreeGPUUsage = math.Max(maxFreeGPUUsage, podFreeGpuUsage[pod.Name])
	}

	if maxRequestCount-minRequestCount >= aibrixDecodeLoadImbalanceMinSpread {
		klog.V(4).InfoS("request imbalance at decode pods", "request_id", ctx.RequestID,
			"min_request_count", minRequestCount, "max_request_count", maxRequestCount,
			"free_gpu_percent", podFreeGpuUsage[minRequestPod.Name],
			"decode_pod", minRequestPod.Name)
		return minRequestPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	if maxThroughput-minThroughput > aibrixDecodeThroughputImbalanceMinSpread {
		klog.V(4).InfoS("throughput imbalance at decode pods", "request_id", ctx.RequestID,
			"min_request_count", minRequestCount, "max_request_count", maxRequestCount,
			"min_throughput", minThroughput, "max_throughput", maxThroughput,
			"free_gpu_percent", podFreeGpuUsage[minThroughputPod.Name],
			"decode_pod", minThroughputPod.Name)
		return minThroughputPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	var minScorePod *v1.Pod
	minScore := math.MaxFloat64
	maxScore := float64(0)
	drainRatesAvailable := true

	for _, pod := range filteredDecodePods {
		drainRate, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeRunningRequestsDrainRate1m)
		if err != nil || drainRate.GetSimpleValue() <= 0 {
			drainRatesAvailable = false
			break
		}
		score := podRequestCounts[pod.Name] / math.Max(drainRate.GetSimpleValue(), defaultDrainRateEpsilon)
		if score < minScore {
			minScore = score
			minScorePod = pod
		}
		maxScore = math.Max(maxScore, score)
	}

	if drainRatesAvailable && minScore > 0 && maxScore/minScore > aibrixDecodeScoreRatioThreshold {
		klog.InfoS("drain rate imbalance at decode pods", "request_id", ctx.RequestID,
			"min_score", minScore, "max_score", maxScore,
			"ratio", maxScore/minScore, "decode_pod", minScorePod.Name)
		return minScorePod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	return nil, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
}

// prefillScorer is a request-scoped scorer created by prefillScorePolicy. prepare for a single request.
// Each call to prepare returns a fresh instance; no state is shared across requests.
type prefillScorer interface {
	// scorePod returns the score for a single pod (lower is better).
	scorePod(pod *v1.Pod, reqCnt, maxRequestCount float64) float64
	// prefixHashes returns token-prefix hashes for cache warming, or nil if not applicable.
	prefixHashes() []uint64
}

// prefillScorePolicy is the stateless scoring strategy for prefill pod selection.
// The policy itself holds only immutable config (e.g. client handles).
// All per-request state is captured inside the prefillScorer returned by prepare.
// Implement this interface to add a new policy; register it in NewPDRouter via AIBRIX_PREFILL_SCORE_POLICY.
type prefillScorePolicy interface {
	// prepare is called once per request. It returns a request-scoped prefillScorer
	// whose state is not shared with any other request.
	// pods and readyPodsMap represent the same set; readyPodsMap is provided for O(1) lookups.
	// Returns an error only if scoring cannot proceed at all (e.g. tokenization failure).
	prepare(routingCtx *types.RoutingContext, pods []*v1.Pod, readyPodsMap map[string]struct{}) (prefillScorer, error)
	// name returns the policy identifier used in log lines.
	name() string
}

// prefixCachePrefillPolicy is a stateless policy that scores pods by prefix-cache match
// percentage plus normalised request count: score = (100 - matchPercent) * 0.1 + reqCnt / maxReqCnt
type prefixCachePrefillPolicy struct {
	tok                tokenizer.Tokenizer
	prefixCacheIndexer *prefixcacheindexer.PrefixHashTable
}

func (p *prefixCachePrefillPolicy) prepare(routingCtx *types.RoutingContext, _ []*v1.Pod, readyPodsMap map[string]struct{}) (prefillScorer, error) {
	tokens, err := p.tok.TokenizeInputText(routingCtx.Message)
	if err != nil {
		return nil, err
	}
	matchedPods, hashes := p.prefixCacheIndexer.MatchPrefix(tokens, routingCtx.Model, readyPodsMap)
	return &prefixCacheScorer{matchedPods: matchedPods, hashes: hashes}, nil
}

func (p *prefixCachePrefillPolicy) name() string { return prefillScorePolicyPrefixCache }

// prefixCacheScorer is the request-scoped scorer produced by prefixCachePrefillPolicy.
type prefixCacheScorer struct {
	matchedPods map[string]int
	hashes      []uint64
}

func (s *prefixCacheScorer) prefixHashes() []uint64 { return s.hashes }

func (s *prefixCacheScorer) scorePod(pod *v1.Pod, reqCnt, maxRequestCount float64) float64 {
	matchPct := float64(s.matchedPods[pod.Name])
	score := (100-matchPct)*.1 + reqCnt/maxRequestCount
	klog.V(4).InfoS("prefill_score", "pod_name", pod.Name,
		"policy", prefillScorePolicyPrefixCache,
		"score", fmt.Sprintf("(100 - %f) * 0.1 + %f / %f", matchPct, reqCnt, maxRequestCount),
		"prefix_match_percent", matchPct,
		"running_reqs", reqCnt, "max_running_reqs", maxRequestCount)
	return score
}

// leastRequestPrefillPolicy is a stateless policy that scores pods by raw running request count.
// No prefix-cache state is consulted; the scorer returned is a zero-size struct.
type leastRequestPrefillPolicy struct{}

func (p *leastRequestPrefillPolicy) prepare(_ *types.RoutingContext, _ []*v1.Pod, _ map[string]struct{}) (prefillScorer, error) {
	return leastRequestScorer{}, nil
}

func (p *leastRequestPrefillPolicy) name() string { return prefillScorePolicyLeastRequest }

// leastRequestScorer is the request-scoped scorer produced by leastRequestPrefillPolicy.
type leastRequestScorer struct{}

func (s leastRequestScorer) prefixHashes() []uint64 { return nil }

func (s leastRequestScorer) scorePod(pod *v1.Pod, reqCnt, _ float64) float64 {
	klog.V(4).InfoS("prefill_score", "pod_name", pod.Name,
		"policy", prefillScorePolicyLeastRequest,
		"running_reqs", reqCnt)
	return reqCnt
}

// scorePrefillPods computes per-roleset prefill scores using the configured prefillScorePolicy.
// It handles the shared bookkeeping (request counts, mean/stddev filter, roleset tracking)
// and delegates per-pod scoring to the policy.
func (r *pdRouter) scorePrefillPods(routingCtx *types.RoutingContext, prefillPods []*v1.Pod) (map[string]*Scores, float64, []uint64) {
	utils.CryptoShuffle(prefillPods)

	podRequestCount := r.prefillRequestTracker.GetPrefillRequestCountsForPods(prefillPods)

	var maxRequestCount float64 = 1
	requestCounts := make([]float64, 0, len(podRequestCount))
	readyPodsMap := make(map[string]struct{}, len(prefillPods))
	for _, pod := range prefillPods {
		readyPodsMap[pod.Name] = struct{}{}
	}
	for _, cnt := range podRequestCount {
		cf := float64(cnt)
		requestCounts = append(requestCounts, cf)
		if cf > maxRequestCount {
			maxRequestCount = cf
		}
	}
	meanRequestCount := mean(requestCounts)
	stdDevRequestCount := standardDeviation(requestCounts)

	scorer, err := r.prefillPolicy.prepare(routingCtx, prefillPods, readyPodsMap)
	if err != nil {
		return nil, 0, nil
	}

	prefillScores := map[string]*Scores{}
	maxPrefillScore := float64(1)
	for _, pod := range prefillPods {
		rolesetName := pod.Labels[PDRoleSetIdentifier]
		reqCnt := float64(podRequestCount[pod.Name])
		if reqCnt > meanRequestCount+float64(standardDeviationFactor)*stdDevRequestCount {
			klog.V(4).InfoS("prefill pod request count is higher than mean request count, skipping",
				"request_id", routingCtx.RequestID, "pod_name", pod.Name,
				"req_cnt", reqCnt, "mean_req_cnt", meanRequestCount, "std_dev_req_cnt", stdDevRequestCount)
			continue
		}

		score := scorer.scorePod(pod, reqCnt, maxRequestCount)
		if existing, exists := prefillScores[rolesetName]; !exists || score < existing.Score {
			prefillScores[rolesetName] = &Scores{Pod: pod, Score: score}
		}
		if score > maxPrefillScore {
			maxPrefillScore = score
		}
	}

	return prefillScores, maxPrefillScore, scorer.prefixHashes()
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
		// For vLLM, wait synchronously to get KV transfer params from response
		return r.handleSyncPrefill(routingCtx, prefillPod, llmEngine, apiURL, payload, fields, r.updateRoutingContextWithKVTransferParams, "KV transfer params")

	case TensorRTLLM:
		// For TensorRT-LLM, wait synchronously to get disaggregated_params from response.
		// The prefill response contains first_gen_tokens and opaque_state needed by the decode worker.
		return r.handleSyncPrefill(routingCtx, prefillPod, llmEngine, apiURL, payload, fields, r.updateRoutingContextWithTRTDisaggParams, "TRT disagg params")

	default:
		// For unknown engines, use synchronous approach as a safe default
		return r.handleSyncPrefill(routingCtx, prefillPod, llmEngine, apiURL, payload, fields, nil, "")
	}

	return nil
}

// handleSyncPrefill executes a synchronous prefill request, optionally calling updateCtxFunc
// to process the response. Pass nil for updateCtxFunc when no response processing is needed.
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

	if llmEngine == TensorRTLLM {
		completionRequest["disaggregated_params"] = map[string]any{
			"request_type":      "context_only",
			"disagg_request_id": getDisaggRequestID(trtMachineID),
		}
	}

	// Set prefill-specific parameters
	completionRequest["max_tokens"] = 1
	if llmEngine == TensorRTLLM {
		delete(completionRequest, "max_completion_tokens")
	} else {
		completionRequest["max_completion_tokens"] = 1
	}
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

	// Parse response JSON. TRT-LLM prefill returns large integer IDs in disaggregated_params; UseInt64 avoids float64 precision loss.
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

func (r *pdRouter) updateRoutingContextWithTRTDisaggParams(routingCtx *types.RoutingContext, responseData map[string]any, prefillPod *v1.Pod) error {
	// Parse the original request body
	var originalRequest map[string]any
	if err := sonicJSONInt64.Unmarshal(routingCtx.ReqBody, &originalRequest); err != nil {
		return fmt.Errorf("failed to unmarshal original request body: %w", err)
	}

	// Extract disaggregated_params from prefill response.
	// TRT-LLM may return it at the top level or inside choices[0].
	var disaggParams any
	var exists bool

	disaggParams, exists = responseData["disaggregated_params"]
	if !exists {
		// Fallback: check choices[0] (TRT-LLM serializes handler output as a choice)
		if choices, ok := responseData["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				disaggParams, exists = choice["disaggregated_params"]
			}
		}
	}

	if !exists {
		klog.InfoS("no disaggregated_params in TRT prefill response", "request_id", routingCtx.RequestID)
		return nil
	}

	disaggParamsMap, ok := disaggParams.(map[string]any)
	if !ok {
		return fmt.Errorf("disaggregated_params has unexpected type %T, expected map[string]any", disaggParams)
	}

	// Override request_type to generation_only for the decode request
	disaggParamsMap["request_type"] = "generation_only"
	originalRequest["disaggregated_params"] = disaggParamsMap

	// Prefill response includes the canonical prompt_token_ids (top-level). Route it based on request path:
	// - /v1/completions: set prompt directly from token ids.
	// - /v1/chat/completions: set prompt_token_ids field.
	if pti, ok := responseData["prompt_token_ids"]; ok && pti != nil {
		if ids, ok := anySliceForJSON(pti); ok {
			if routingCtx.ReqPath == "/v1/completions" {
				originalRequest["prompt"] = ids
			} else if routingCtx.ReqPath == "/v1/chat/completions" {
				originalRequest["prompt_token_ids"] = ids
			}
		}
	}

	updatedReqBody, err := sonic.Marshal(originalRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal updated request body: %w", err)
	}

	routingCtx.ReqBody = updatedReqBody

	klog.InfoS("updated routing context with disaggregated_params (TensorRT-LLM)",
		"request_id", routingCtx.RequestID,
		"prefill_pod", prefillPod.Name,
		"prefill_host", prefillPod.Status.PodIP)

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

// NewPendingDecodeTracker creates a new pending decode tracker.
func NewPendingDecodeTracker() *PendingDecodeTracker {
	return &PendingDecodeTracker{}
}

func (t *PendingDecodeTracker) AddPendingDecode(requestID, podName string) {
	if t == nil {
		return
	}
	countInterface, _ := t.podRequestCounts.LoadOrStore(podName, &atomic.Int32{})
	count := countInterface.(*atomic.Int32)
	count.Add(1)
	t.requestToPod.Store(requestID, podName)
}

func (t *PendingDecodeTracker) RemovePendingDecode(requestID string) {
	if t == nil {
		return
	}
	podNameInterface, exists := t.requestToPod.LoadAndDelete(requestID)
	if !exists {
		return
	}
	podName := podNameInterface.(string)
	countInterface, exists := t.podRequestCounts.Load(podName)
	if !exists {
		return
	}
	count := countInterface.(*atomic.Int32)
	if newCount := count.Add(-1); newCount < 0 {
		// Do not Store(0): between Add(-1) and Store, another goroutine may Add(1), and Store(0)
		// would erase that increment. Clamp to 0 only while the value is still negative (CAS loop).
		for {
			v := count.Load()
			if v >= 0 {
				return
			}
			if count.CompareAndSwap(v, 0) {
				return
			}
		}
	}
}

func (t *PendingDecodeTracker) GetPendingDecodeCount(podName string) float64 {
	if t == nil {
		return 0
	}
	countInterface, exists := t.podRequestCounts.Load(podName)
	if !exists {
		return 0
	}
	return float64(countInterface.(*atomic.Int32).Load())
}

func (r *pdRouter) isPodSuitableForPromptLength(routingCtx *types.RoutingContext, pod *v1.Pod, promptLength int) bool {
	profile := configprofiles.ResolveProfileFromPod(pod, routingCtx.ReqConfigProfile)
	if profile == nil {
		return false
	}
	pdCfg := parsePDAlgorithmConfig(profile.RoutingConfig)
	minLength, maxLength := pdCfg.PromptLenBucketMinLength, pdCfg.PromptLenBucketMaxLength

	if minLength > maxLength {
		return false
	}
	// If no prompt length range is configured, the pod is assumed to be suitable for handling any length.
	if minLength == 0 && maxLength == math.MaxInt32 {
		return true
	}

	return promptLength >= minLength && promptLength <= maxLength
}

func isCombinedPod(routingCtx *types.RoutingContext, pod *v1.Pod) bool {
	profile := configprofiles.ResolveProfileFromPod(pod, routingCtx.ReqConfigProfile)
	if profile == nil {
		return false
	}
	pdCfg := parsePDAlgorithmConfig(profile.RoutingConfig)
	return pdCfg.Combined
}

// collectAndBucketPods partitions readyPods into prefill, decode, and combined
// pod slices for PD-disaggregated routing. It operates in two phases:
//
// Phase 1 groups pods by their roleset (PDRoleSetIdentifier label), separating
// prefill and decode pods, while collecting combined-role pods that are eligible
// for the given promptLength. Pods missing required PD labels or an HTTP server
// are skipped.
//
// Phase 2 builds the output slices from rolesets that have both prefill and
// decode pods (incomplete rolesets are excluded). When prompt-length bucketing
// is enabled, it also produces filtered prefill/decode slices restricted to pods
// whose capacity bucket covers promptLength; these filtered slices replace the
// unfiltered ones in the primary return values when non-empty.
//
// Returns (prefillPods, decodePods, promptLengthBucketingPrefillPods,
// promptLengthBucketingDecodePods, combinedPods).
func (r *pdRouter) collectAndBucketPods(routingCtx *types.RoutingContext, readyPods []*v1.Pod, promptLength int) ([]*v1.Pod, []*v1.Pod, []*v1.Pod, []*v1.Pod, []*v1.Pod) {
	bucketingEnabled := aibrixPromptLengthBucketing

	type rolesetBucket struct {
		prefills []*v1.Pod
		decodes  []*v1.Pod
	}
	byRoleset := make(map[string]*rolesetBucket)
	var combinedPods []*v1.Pod

	// Phase 1: single pass — group pods by roleset, collect combined pods.
	// Applies all eligibility guards (labels, HTTP server) once per pod.
	for _, pod := range readyPods {
		roleSetID, hasRoleset := pod.Labels[PDRoleSetIdentifier]
		if !hasRoleset {
			continue
		}
		roleID, hasRole := pod.Labels[PDRoleIdentifier]
		if !hasRole {
			continue
		}
		// For multi-node scenarios, only select pods from node_rank=0 (PodGroupIndex=0)
		// which have the HTTP server running.
		if !isPodWithHTTPServer(pod) {
			continue
		}

		switch roleID {
		case "prefill":
			b := byRoleset[roleSetID]
			if b == nil {
				b = &rolesetBucket{}
				byRoleset[roleSetID] = b
			}
			b.prefills = append(b.prefills, pod)
		case "decode":
			b := byRoleset[roleSetID]
			if b == nil {
				b = &rolesetBucket{}
				byRoleset[roleSetID] = b
			}
			b.decodes = append(b.decodes, pod)
		default:
			if bucketingEnabled && isCombinedPod(routingCtx, pod) && r.isPodSuitableForPromptLength(routingCtx, pod, promptLength) {
				combinedPods = append(combinedPods, pod)
			}
		}
	}

	// Phase 2: build output slices from rolesets that have both prefill and decode pods.
	var prefillPods, decodePods []*v1.Pod
	var promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods []*v1.Pod

	for _, b := range byRoleset {
		if len(b.prefills) == 0 || len(b.decodes) == 0 {
			continue
		}
		prefillPods = append(prefillPods, b.prefills...)
		decodePods = append(decodePods, b.decodes...)

		if bucketingEnabled {
			var bucketPrefills, bucketDecodes []*v1.Pod
			for _, pod := range b.prefills {
				if r.isPodSuitableForPromptLength(routingCtx, pod, promptLength) {
					bucketPrefills = append(bucketPrefills, pod)
				}
			}
			for _, pod := range b.decodes {
				if r.isPodSuitableForPromptLength(routingCtx, pod, promptLength) {
					bucketDecodes = append(bucketDecodes, pod)
				}
			}
			if len(bucketPrefills) > 0 && len(bucketDecodes) > 0 {
				promptLengthBucketingPrefillPods = append(promptLengthBucketingPrefillPods, bucketPrefills...)
				promptLengthBucketingDecodePods = append(promptLengthBucketingDecodePods, bucketDecodes...)
			}
		}
	}

	// Override prefill/decode with bucket-filtered pods if bucketing produced results.
	if bucketingEnabled {
		if len(promptLengthBucketingPrefillPods) > 0 {
			prefillPods = promptLengthBucketingPrefillPods
		}
		if len(promptLengthBucketingDecodePods) > 0 {
			decodePods = promptLengthBucketingDecodePods
		}
	}

	return prefillPods, decodePods, promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods, combinedPods
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
