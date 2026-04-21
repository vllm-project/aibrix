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
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
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
	aibrixPrefillScorePolicy string = utils.LoadEnv("AIBRIX_PREFILL_SCORE_POLICY", pd.PrefillScorePolicyPrefixCache)
	// decode pod scoring strategy: "load_balancing" or "least_request"
	aibrixDecodeScorePolicy string = utils.LoadEnv("AIBRIX_DECODE_SCORE_POLICY", pd.ScorePolicyLoadBalancing)
)

// loadBalancingDecodePolicy is shared for nil-policy fallback and invalid-score fallback (stateless type).
var loadBalancingDecodePolicy = pd.LoadBalancingDecodePolicy{}

func init() {
	Register(RouterPD, NewPDRouter)
}

// pdAlgorithmConfig holds PD-specific algorithm configuration parsed from RoutingConfig.
type pdAlgorithmConfig struct {
	PromptLenBucketMinLength int    `json:"promptLenBucketMinLength"`
	PromptLenBucketMaxLength int    `json:"promptLenBucketMaxLength"`
	Combined                 bool   `json:"combined"`
	PrefillScorePolicy       string `json:"prefillScorePolicy,omitempty"`
	DecodeScorePolicy        string `json:"decodeScorePolicy,omitempty"`
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

// effectiveScorePolicies returns prefill/decode scoring policies for this request.
// When routingCtx.ConfigProfile.RoutingConfig sets prefillScorePolicy and/or decodeScorePolicy,
// those override the gateway defaults from AIBRIX_PREFILL_SCORE_POLICY / AIBRIX_DECODE_SCORE_POLICY
// (stored on the router at startup). Empty or missing fields keep the env-based policies.
// A non-empty decodeScorePolicy that is not recognized returns an error so routing does not
// silently run with a different policy than the user configured.
func (r *pdRouter) effectiveScorePolicies(routingCtx *types.RoutingContext) (pd.PrefillScorePolicy, pd.DecodeScorePolicy, error) {
	prefill := r.prefillPolicy
	decode := r.decodePolicy
	if routingCtx.ConfigProfile == nil || len(routingCtx.ConfigProfile.RoutingConfig) == 0 {
		return prefill, decode, nil
	}
	cfg := parsePDAlgorithmConfig(routingCtx.ConfigProfile.RoutingConfig)
	if s := strings.TrimSpace(cfg.PrefillScorePolicy); s != "" {
		switch s {
		case pd.PrefillScorePolicyLeastRequest:
			prefill = pd.NewLeastRequestPrefillPolicy()
		case pd.PrefillScorePolicyPrefixCache:
			prefill = newPrefixCachePrefillPolicy(r.prefixCacheIndexer)
		default:
			klog.InfoS("unknown prefillScorePolicy in routingConfig, keeping env-based policy",
				"request_id", routingCtx.RequestID, "value", s,
				"valid", []string{pd.PrefillScorePolicyPrefixCache, pd.PrefillScorePolicyLeastRequest})
			prefill = r.prefillPolicy
		}
	}
	if s := strings.TrimSpace(cfg.DecodeScorePolicy); s != "" {
		d, _, unknown := pd.ResolveDecodePolicy(s)
		if unknown {
			valid := pd.ValidDecodePolicyNames()
			klog.Warningf("unknown decodeScorePolicy in routingConfig (request_id=%s value=%q valid=%v)",
				routingCtx.RequestID, s, valid)
			return nil, nil, fmt.Errorf("unknown decodeScorePolicy %q in routingConfig (valid: %v)", s, valid)
		}
		decode = d
	}
	if strings.TrimSpace(cfg.PrefillScorePolicy) != "" || strings.TrimSpace(cfg.DecodeScorePolicy) != "" {
		klog.V(4).InfoS("pd score policies from model config profile routingConfig",
			"request_id", routingCtx.RequestID,
			"prefill_policy", prefill.Name(), "decode_policy", decode.Name())
	}
	return prefill, decode, nil
}

type pdRouter struct {
	cache                 cache.Cache
	prefillPolicy         pd.PrefillScorePolicy
	decodePolicy          pd.DecodeScorePolicy
	prefixCacheIndexer    *prefixcacheindexer.PrefixHashTable
	prefillRequestTracker *pd.PrefillRequestTracker
	pendingDecodeTracker  *pd.PendingDecodeTracker
	httpClient            *http.Client
	prefixUpdateCh        chan prefixUpdateJob
	countersMu            sync.RWMutex
	selectionCounts       map[string]int64
}

func newPrefixCachePrefillPolicy(sharedPrefixTable *prefixcacheindexer.PrefixHashTable) pd.PrefillScorePolicy {
	var tok tokenizer.Tokenizer
	if tokenizerType == tokenizerTypeTiktoken {
		tok = tokenizer.NewTiktokenTokenizer()
	} else {
		tok = tokenizer.NewCharacterTokenizer()
	}
	return pd.NewPrefixCachePrefillPolicy(tok, sharedPrefixTable)
}

func NewPDRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		klog.Error("fail to get cache store in prefix cache router")
		return nil, err
	}

	sharedPrefixTable := prefixcacheindexer.GetSharedPrefixHashTable()

	var policy pd.PrefillScorePolicy
	switch aibrixPrefillScorePolicy {
	case pd.PrefillScorePolicyLeastRequest:
		policy = pd.NewLeastRequestPrefillPolicy()
	case pd.PrefillScorePolicyPrefixCache:
		policy = newPrefixCachePrefillPolicy(sharedPrefixTable)
	default:
		klog.InfoS("pd_router unknown AIBRIX_PREFILL_SCORE_POLICY, using prefix_cache",
			"value", aibrixPrefillScorePolicy,
			"valid", []string{pd.PrefillScorePolicyPrefixCache, pd.PrefillScorePolicyLeastRequest})
		policy = newPrefixCachePrefillPolicy(sharedPrefixTable)
	}
	klog.InfoS("pd_router prefill score policy", "policy", policy.Name())

	decodePol, _, unknownDecode := pd.ResolveDecodePolicy(aibrixDecodeScorePolicy)
	if unknownDecode {
		klog.InfoS("pd_router unknown AIBRIX_DECODE_SCORE_POLICY, using load_balancing",
			"value", aibrixDecodeScorePolicy,
			"valid", pd.ValidDecodePolicyNames())
	}
	klog.InfoS("pd_router decode score policy", "policy", decodePol.Name(), "describe", decodePol.Describe())

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
		decodePolicy:          decodePol,
		prefixCacheIndexer:    sharedPrefixTable,
		prefillRequestTracker: pd.NewPrefillRequestTracker(),
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		httpClient:            httpClient,
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       make(map[string]int64),
	}

	pdRouter.startPrefixUpdater()
	return &pdRouter, nil
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

	prefillPol, decodePol, err := r.effectiveScorePolicies(routingCtx)
	if err != nil {
		return nil, nil, err
	}
	prefillScores, maxPrefillScore, prefixHashes := r.scorePrefillPods(routingCtx, prefillPods, prefillPol)
	decodeRun := r.scoreDecodePods(routingCtx, decodePods, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage, decodePol)
	return r.finalPDScore(routingCtx, prefixHashes, prefillScores, maxPrefillScore, decodeRun)
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

// scorePrefillPods scores candidate prefill pods using prefillPolicy. Pods whose
// running-request count exceeds mean+stddev*factor are skipped as overloaded.
// Returns a per-roleset map of the best (lowest-score) pod, the global max score
// used by finalPDScore for normalization, and the prefix hashes from the scorer.
func (r *pdRouter) scorePrefillPods(routingCtx *types.RoutingContext, prefillPods []*v1.Pod, prefillPolicy pd.PrefillScorePolicy) (map[string]*Scores, float64, []uint64) {
	if prefillPolicy == nil {
		klog.ErrorS(nil, "scorePrefillPods called with nil prefillPolicy; this is a programming error",
			"request_id", routingCtx.RequestID)
		return nil, 0, nil
	}
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

	scorer, err := prefillPolicy.Prepare(routingCtx, prefillPods, readyPodsMap)
	if err != nil {
		klog.ErrorS(err, "prefill scorer preparation failed",
			"request_id", routingCtx.RequestID, "policy", prefillPolicy.Name(), "model", routingCtx.Model)
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

		score := scorer.ScorePod(pod, reqCnt, maxRequestCount)
		if existing, exists := prefillScores[rolesetName]; !exists || score < existing.Score {
			prefillScores[rolesetName] = &Scores{Pod: pod, Score: score}
		}
		if score > maxPrefillScore {
			maxPrefillScore = score
		}
	}

	return prefillScores, maxPrefillScore, scorer.PrefixHashes()
}

// scoreDecodePods scores candidate decode pods using policy, falling back to
// load_balancing for a nil policy or for individual pods whose primary score is
// invalid (NaN). Returns a DecodeScoreRun with the best (lowest-score) pod per
// roleset and the global max score used by finalPDScore for normalization.
func (r *pdRouter) scoreDecodePods(routingCtx *types.RoutingContext, filteredDecodePods []*v1.Pod,
	maxRequestCount float64, maxThroughput float64, maxFreeGPUUsage float64,
	podRequestCounts map[string]float64, podThroughputs map[string]float64, podFreeGpuUsage map[string]float64,
	policy pd.DecodeScorePolicy) pd.DecodeScoreRun {
	if policy == nil {
		policy = r.decodePolicy
	}
	if policy == nil {
		policy = loadBalancingDecodePolicy
	}
	policyName := policy.Name()

	out := pd.DecodeScoreRun{
		PerRoleset: make(map[string]pd.RolesetDecodePick),
		MaxScore:   0.01,
		Policy:     policyName,
	}
	if len(filteredDecodePods) == 0 {
		return out
	}

	utils.CryptoShuffle(filteredDecodePods)

	for _, pod := range filteredDecodePods {
		rolesetName := pod.Labels[PDRoleSetIdentifier]
		in := pd.DecodePodInput{
			RunningReqs:     podRequestCounts[pod.Name],
			Throughput:      podThroughputs[pod.Name],
			FreeGPUPercent:  podFreeGpuUsage[pod.Name],
			MaxRequestCount: maxRequestCount,
			MaxThroughput:   maxThroughput,
			MaxFreeGPUUsage: maxFreeGPUUsage,
		}

		decodeScore := policy.ScoreDecodePod(routingCtx, pod, in)
		if pd.InvalidDecodeScore(decodeScore) {
			if policyName != pd.DecodePolicyLoadBalancing {
				decodeScore = loadBalancingDecodePolicy.ScoreDecodePod(routingCtx, pod, in)
				out.FallbackUsed = true
			}
			if pd.InvalidDecodeScore(decodeScore) {
				klog.V(2).InfoS("decode score invalid after policy and load_balancing fallback, skipping pod",
					"request_id", routingCtx.RequestID, "pod", pod.Name, "policy", policyName)
				continue
			}
		}

		if existing, exists := out.PerRoleset[rolesetName]; !exists || decodeScore < existing.Score {
			out.PerRoleset[rolesetName] = pd.RolesetDecodePick{Pod: pod, Score: decodeScore}
		}
		if decodeScore > out.MaxScore {
			out.MaxScore = decodeScore
		}
	}

	return out
}

// finalPDScore selects the winning prefill/decode pod pair by normalizing each
// roleset's prefill and decode scores by their respective maxima and picking the
// roleset with the lowest combined score. Enqueues a prefix-cache update and
// emits selection metrics before returning.
func (r *pdRouter) finalPDScore(routingCtx *types.RoutingContext,
	prefixHashes []uint64,
	prefillScores map[string]*Scores, maxPrefillScore float64,
	decodeRun pd.DecodeScoreRun,
) (*v1.Pod, *v1.Pod, error) {
	if decodeRun.Err != nil {
		return nil, nil, fmt.Errorf("decode scoring failed: %w", decodeRun.Err)
	}

	var targetPrefillPod, targetDecodePod *v1.Pod
	minScore := math.MaxFloat64

	for roleset, prefillScore := range prefillScores {
		decodePick, ok := decodeRun.PerRoleset[roleset]
		if !ok {
			continue
		}

		normalizedPrefillScore := prefillScore.Score / maxPrefillScore
		normalizedDecodeScore := decodePick.Score / decodeRun.MaxScore
		final := normalizedPrefillScore + normalizedDecodeScore

		if final < minScore {
			minScore = final
			targetPrefillPod = prefillScore.Pod
			targetDecodePod = decodePick.Pod
		}

		klog.V(4).InfoS(
			"final_score",
			"request_id", routingCtx.RequestID,
			"roleset", roleset,
			"final_score", final,
			"prefill_score", prefillScore.Score, "normalized_prefill_score", normalizedPrefillScore,
			"decode_score", decodePick.Score, "normalized_decode_score", normalizedDecodeScore,
			"decode_policy", decodeRun.Policy,
			"decode_fallback_used", decodeRun.FallbackUsed,
		)
	}

	if targetPrefillPod == nil {
		return nil, nil, fmt.Errorf("target prefill pod is nil")
	}
	if targetDecodePod == nil {
		return nil, nil, fmt.Errorf("target decode pod is nil")
	}
	if len(prefixHashes) > 0 {
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

// shouldPickCombined returns true when at least one combined pod is under low
// load (request rate < defaultRequestRateLowLoadThreshold) and at least one
// prefill or decode pod is over high load (> defaultRequestRateHighLoadThreshold).
// The decode check is skipped when a prefill pod already qualifies as high-load.
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

// scoreCombinedPods returns the combined pod with the lowest request-rate score.
// Pods are shuffled before scoring so ties are broken randomly.
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

func isCombinedPod(routingCtx *types.RoutingContext, pod *v1.Pod) bool {
	profile := configprofiles.ResolveProfileFromPod(pod, routingCtx.ReqConfigProfile)
	if profile == nil {
		return false
	}
	pdCfg := parsePDAlgorithmConfig(profile.RoutingConfig)
	return pdCfg.Combined
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
