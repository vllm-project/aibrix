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
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/prefill"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/selector"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	RouterPD                     types.RoutingAlgorithm = "pd"
	VLLMEngine                   string                 = "vllm"
	SGLangEngine                 string                 = "sglang"
	TensorRTLLM                  string                 = "trtllm"
	LLMEngineIdentifier          string                 = constants.ModelLabelEngine
	PDRoleSetIdentifier          string                 = "roleset-name"
	PDRoleIdentifier             string                 = "role-name"
	PDRolePrefill                string                 = "prefill"
	PDRoleDecode                 string                 = "decode"
	RoleReplicaIndex             string                 = "stormservice.orchestration.aibrix.ai/role-replica-index"
	PodGroupIndex                string                 = "stormservice.orchestration.aibrix.ai/pod-group-index"
	PromptLenBucketMinLength     string                 = "prompt-len-bucket-min-length"
	PromptLenBucketMaxLength     string                 = "prompt-len-bucket-max-length"
	defaultPrefillRequestTimeout int                    = 30

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
	// KVConnectorTypeIdentifier specifies the KV transfer backend for a PD pod. Supported values include "shfs", "nixl", and "mooncake".
	KVConnectorTypeIdentifier = "model.aibrix.ai/kv-connector-type"
	// KV connector types for different backends
	KVConnectorTypeSHFS     = "shfs"     // Default - AIBrix SHFS/KVCacheManager (GPU)
	KVConnectorTypeNIXL     = "nixl"     // NIXL for Neuron (uses disagg_prefill_resp wrapper)
	KVConnectorTypeMooncake = "mooncake" // Mooncake KV transfer backend

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

var (
	pdGatewayPodName     = os.Getenv("POD_NAME")
	pdPrefillOutstanding int64
)

func init() {
	Register(RouterPD, NewPDRouter)
	// Point the vLLM engine handler at the live connector-type var so that tests
	// (and runtime config changes via env) are reflected without a second copy.
	engine.SetConnectorTypeFunc(func() string { return aibrixKVConnectorType })
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
		case pd.PrefillScorePolicyConductor:
			prefill = newConductorPrefillPolicy(r.prefixCacheIndexer, r.cache)
		default:
			klog.InfoS("unknown prefillScorePolicy in routingConfig, keeping env-based policy",
				"request_id", routingCtx.RequestID, "value", s,
				"valid", []string{pd.PrefillScorePolicyPrefixCache, pd.PrefillScorePolicyLeastRequest, pd.PrefillScorePolicyConductor})
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
			"request_id", routingCtx.RequestID, "prefill_policy", prefill.Name(),
			"decode_policy", decode.Name())
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
	podSelector           selector.PodSelector
	prefillExecutor       prefill.PrefillExecutor
}

func newPrefixCachePrefillPolicy(sharedPrefixTable *prefixcacheindexer.PrefixHashTable) pd.PrefillScorePolicy {
	return pd.NewPrefixCachePrefillPolicy(newTokenizer(), sharedPrefixTable)
}

func newConductorPrefillPolicy(sharedPrefixTable *prefixcacheindexer.PrefixHashTable, metricCache cache.MetricCache) pd.PrefillScorePolicy {
	return pd.NewConductorPrefillPolicy(newTokenizer(), sharedPrefixTable, metricCache, pd.NewConductorPrefillPolicyConfig())
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
	case pd.PrefillScorePolicyConductor:
		policy = newConductorPrefillPolicy(sharedPrefixTable, c)
	default:
		klog.InfoS("pd_router unknown AIBRIX_PREFILL_SCORE_POLICY, using prefix_cache",
			"value", aibrixPrefillScorePolicy, "valid", []string{pd.PrefillScorePolicyPrefixCache, pd.PrefillScorePolicyLeastRequest})
		policy = newPrefixCachePrefillPolicy(sharedPrefixTable)
	}
	klog.InfoS("pd_router prefill score policy", "policy", policy.Name())

	decodePol, _, unknownDecode := pd.ResolveDecodePolicy(aibrixDecodeScorePolicy)
	if unknownDecode {
		klog.InfoS("pd_router unknown AIBRIX_DECODE_SCORE_POLICY, using load_balancing",
			"value", aibrixDecodeScorePolicy, "valid", pd.ValidDecodePolicyNames())
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

	r := &pdRouter{
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
	r.podSelector = selector.NewDefaultSelector(r.filterPrefillDecodePods)
	r.prefillExecutor = prefill.NewDefaultExecutor(httpClient, r.prefillRequestTracker, prefillRequestTimeout)

	r.startPrefixUpdater()
	return r, nil
}

func (r *pdRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	readyPods := readyPodList.All()

	prefillPod, decodePod, err := r.podSelector.Select(ctx, readyPods)
	if err != nil {
		metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": pdRouteFilterPrefillDecodePodsFail, "status_code": "400"})
		return "", fmt.Errorf("failed to filter prefill/decode pods for request %s: %w", ctx.RequestID, err)
	}

	r.pendingDecodeTracker.AddPendingDecode(ctx.RequestID, decodePod.Name)
	defer r.pendingDecodeTracker.RemovePendingDecode(ctx.RequestID)

	if prefillPod != nil {

		klog.InfoS("selected prefill/decode pods", "request_id", ctx.RequestID, "prefill_pod", prefillPod.Name, "decode_pod", decodePod.Name)
		if ctx.RespHeaders == nil {
			ctx.RespHeaders = make(map[string]string)
		}
		ctx.RespHeaders[HeaderPrefillTargetPod] = prefillPod.Name
		ctx.RespHeaders[HeaderPrefillTargetPodIP] = prefillPod.Status.PodIP

		n := atomic.AddInt64(&pdPrefillOutstanding, 1)
		metrics.SetGaugeMetric(metrics.GatewayPrefillOutstandingRequests, metrics.GetMetricHelp(metrics.GatewayPrefillOutstandingRequests), float64(n), []string{"gateway_pod"}, pdGatewayPodName)
		err = r.doPrefillRequest(ctx, prefillPod, ctx.Engine)
		remaining := atomic.AddInt64(&pdPrefillOutstanding, -1)
		metrics.SetGaugeMetric(metrics.GatewayPrefillOutstandingRequests, metrics.GetMetricHelp(metrics.GatewayPrefillOutstandingRequests), float64(remaining), []string{"gateway_pod"}, pdGatewayPodName)

		if err != nil {
			// Remove is a no-op if the executor already cleaned up (e.g. sync HTTP failure).
			r.prefillRequestTracker.RemovePrefillRequest(ctx.RequestID)
			metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
				map[string]string{"status": pdRoutePrefillRequestError, "status_code": "500"})
			klog.ErrorS(err, pdRoutePrefillRequestError, "request_id", ctx.RequestID)
			return "", fmt.Errorf("prefill request failed for request %s: %w", ctx.RequestID, err)
		}
		if !engine.Resolve(ctx.Engine).IsAsync() {
			metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestSuccessTotal, &metrics.SimpleMetricValue{Value: 1.0},
				map[string]string{"status": pdRoutePrefillRequestSuccess, "status_code": "200"})
		}
	}

	ctx.SetTargetPod(decodePod)
	return ctx.TargetAddress(), nil
}

type Scores struct {
	Pod   *v1.Pod
	Score float64
}

// filterPrefillDecodePods selects one prefill pod and one decode pod for the request.
// For multi-node tensor parallelism, only pods with PodGroupIndex="0" run the HTTP
// server and are routable; pods without that label are included for backward compatibility.
//
// Selection sequence:
//
//  1. collectAndBucketPods — partitions readyPods from eligible rolesets into prefill,
//     decode, and (when bucketing is on) prompt-length-filtered and combined-role slices.
//
//  2. Pod availability check — errors immediately when no prefill or decode pods exist
//     and no combined pod is available to cover the gap.
//
//  3. Bucketing path (AIBRIX_PROMPT_LENGTH_BUCKETING only) —
//     a. No bucket match: prompt length falls outside every declared range.
//     - Combined pods available → pick a random combined pod (no prefill HTTP call).
//     - No combined pods → error; do not route to the wrong storm.
//     b. Bucket match + load imbalance: prefill or decode pods are hot while a
//     combined pod is idle → score and pick the best combined pod.
//
//  4. Prefill load-imbalance fast path — if outstanding prefill counts are skewed
//     beyond aibrixPrefillLoadImbalanceMinSpread, narrow to the single least-loaded
//     prefill pod and align decodePods to its roleset. Continues to step 5.
//
//  5. Decode load-imbalance fast path — three ordered checks (request-count spread,
//     throughput spread, drain-rate ratio); the first that fires narrows decodePods
//     to a single pod and aligns prefillPods to its roleset. Steps 4 and 5 are
//     independent: both can fire on the same request if both prefill and decode are
//     imbalanced, with each narrowing its own side of the pair.
//
//  6. Score remaining candidates — scorePrefillPods and scoreDecodePods evaluate
//     every roleset; finalPDScore picks the roleset with the lowest combined score.
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

	if aibrixPromptLengthBucketing {
		if len(promptLengthBucketingPrefillPods) == 0 || len(promptLengthBucketingDecodePods) == 0 {
			// No bucket matches the request's prompt length.
			if combinedAvailable {
				// Combined pods cover any prompt length; pick one at random.
				klog.InfoS("no bucket matches prompt length, routing to combined pod",
					"requestId", routingCtx.RequestID, "promptLength", promptLength, "combinedPods", len(combinedPods),
					"bucketPrefillPods", len(promptLengthBucketingPrefillPods), "bucketDecodePods", len(promptLengthBucketingDecodePods))
				return nil, combinedPods[rand.Intn(len(combinedPods))], nil
			}
			// Do not fall back to unfiltered PD pods: each pod group (storm) is sized for a
			// specific prompt-length range and cross-bucket routing produces incorrect results.
			return nil, nil, fmt.Errorf("no prompt-length bucket matches prompt length %d and no combined pods available", promptLength)
		}

		// Bucket match exists; check if load imbalance favours a combined pod instead.
		if r.shouldPickCombined(routingCtx, promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods, combinedPods) {
			combinedPod := r.scoreCombinedPods(routingCtx, combinedPods)
			if combinedPod != nil {
				klog.InfoS("load imbalance detected, selecting combined pod", "requestId", routingCtx.RequestID, "selectedCombinedPod", combinedPod.Name)
				return nil, combinedPod, nil
			}
		}
	}

	// check for prefill and decode imbalance
	targetPod, isImbalanced := r.loadImbalanceSelectPrefillPod(prefillPods, r.prefillRequestTracker.GetPrefillRequestCountsForPods(prefillPods))
	if isImbalanced && targetPod != nil {
		prefillPods = []*v1.Pod{targetPod}
		decodePods = utils.FilterPodsByLabel(decodePods, PDRoleSetIdentifier, targetPod.Labels[PDRoleSetIdentifier])
		klog.V(4).InfoS("load imbalance detected, selecting least-loaded prefill pod",
			"request_id", routingCtx.RequestID, "selected_prefill_pod", targetPod.Name,
			"roleset", targetPod.Labels[PDRoleSetIdentifier], "decode_pods_after_align", pdPodNames(decodePods),
		)
	}

	targetPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage := r.loadImbalanceSelectDecodePod(routingCtx, decodePods)
	if targetPod != nil {
		decodePods = []*v1.Pod{targetPod}
		if aligned := utils.FilterPodsByLabel(prefillPods, PDRoleSetIdentifier, targetPod.Labels[PDRoleSetIdentifier]); len(aligned) > 0 {
			prefillPods = aligned
		}
		klog.V(4).InfoS("load imbalance detected in decode pods",
			"request_id", routingCtx.RequestID, "selected_decode_pod", targetPod.Name,
			"roleset", targetPod.Labels[PDRoleSetIdentifier], "prefill_pods_after_align", pdPodNames(prefillPods),
		)
	}

	prefillPol, decodePol, err := r.effectiveScorePolicies(routingCtx)
	if err != nil {
		return nil, nil, err
	}

	prefillScores, maxPrefillScore, prefixHashes := r.scorePrefillPods(routingCtx, prefillPods, prefillPol)
	decodeRun := r.scoreDecodePods(routingCtx, decodePods, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage, decodePol)
	selectedPrefill, selectedDecode, err := r.finalPDScore(routingCtx, prefixHashes, prefillScores, maxPrefillScore, decodeRun)
	if err != nil {
		return nil, nil, err
	}

	return selectedPrefill, selectedDecode, nil
}

// loadImbalanceSelectPrefillPod is a fast path that runs before scorePrefillPods when
// outstanding prefill load is visibly skewed across readyPods.
//
// podRequestCount holds per-pod active prefill counts from PrefillRequestTracker for
// in-flight prefill HTTP calls from other concurrent requests. The current request is
// not counted yet — it is registered by the caller after selection completes.
// readyPods is shuffled before evaluation so ties among equally-loaded pods are broken
// randomly rather than by map iteration order.
//
// Returns one pod tied for the minimum count and imbalance=true when
// max(count) − min(count) > aibrixPrefillLoadImbalanceMinSpread (strictly greater than).
// Otherwise returns nil, false. An empty podRequestCount map always returns nil, false.
//
// The caller (filterPrefillDecodePods) narrows decodePods to the selected pod's roleset
// so that prefill and decode remain aligned to the same roleset pair after this step.
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
		if targetPod != nil {
			klog.V(4).InfoS("prefill request imbalance detected, selecting least-loaded pod",
				"min_request_count", minValue, "max_request_count", maxValue,
				"selected_prefill_pod", targetPod.Name, "roleset", targetPod.Labels[PDRoleSetIdentifier],
				"candidate_pods", strings.Join(targetPods, ","),
			)
		}
	}

	return targetPod, imbalance
}

// loadImbalanceSelectDecodePod is a fast path that runs before scoreDecodePods when decode
// load is visibly skewed. It walks filteredDecodePods once, filling podRequestCounts,
// podThroughputs, and podFreeGpuUsage for the scoring pass, then applies three ordered checks
// (each runs only if the previous did not return):
//
//  1. Request imbalance: among pods that report RealtimeNumRequestsRunning, compute
//     effective count = running + PendingDecodeTracker pending count for that pod.
//     Pending counts come from concurrent Route calls that have registered
//     AddPendingDecode but not yet returned. If max − min effective count is at least
//     aibrixDecodeLoadImbalanceMinSpread, return the least-loaded metric-bearing pod.
//     Pods without a running-request metric are excluded from the spread (their pending
//     count is still stored in podRequestCounts for scoring) so freshly restarted pods
//     are not treated as idle and given a thundering herd.
//
//  2. Throughput spread: among pods that report AvgGenerationThroughputToksPerS (per model),
//     if max − min throughput exceeds aibrixDecodeThroughputImbalanceMinSpread, return the
//     lowest-throughput pod. Pods missing the metric are excluded from this check.
//
//  3. Drain-rate scoring: if every pod has a positive RealtimeRunningRequestsDrainRate1m,
//     score each pod as effectiveRequestCount / drainRate. If maxScore/minScore exceeds
//     aibrixDecodeScoreRatioThreshold, return the pod with the lowest score. Skipped when
//     any drain rate is missing or non-positive.
//
// Returns nil when none of the checks fire; the caller falls through to scoreDecodePods with
// the collected maps. A non-nil pod return also carries maxRequestCount, maxThroughput, and
// maxFreeGPUUsage from the same pass (maxima include all pods, including those without metrics).
// maxRequestCount and maxThroughput are floored at 1 and maxFreeGPUUsage is floored at 1 so
// that scoreDecodePods normalization denominators are always positive. The caller narrows
// prefillPods to the selected pod's roleset. KV cache headroom uses KVCacheUsagePerc
// (missing metric is treated as 0% usage = 100% free).
func (r *pdRouter) loadImbalanceSelectDecodePod(ctx *types.RoutingContext, filteredDecodePods []*v1.Pod) (*v1.Pod, float64, float64, float64, map[string]float64, map[string]float64, map[string]float64) {
	podRequestCounts := make(map[string]float64)
	podThroughputs := make(map[string]float64)
	podFreeGpuUsage := make(map[string]float64)

	var minRequestPod *v1.Pod
	maxRequestCount := float64(1)
	var minThroughputPod *v1.Pod
	maxThroughput := float64(1)
	maxFreeGPUUsage := float64(1)
	maxObservedRequestCount := float64(0)
	minObservedRequestCount := math.MaxFloat64
	maxObservedThroughput := float64(0)
	minObservedThroughput := math.MaxFloat64
	utils.CryptoShuffle(filteredDecodePods)

	for _, pod := range filteredDecodePods {
		runningReqs, runningErr := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
		requestCount := r.pendingDecodeTracker.GetPendingDecodeCount(pod.Name)
		if runningErr != nil {
			podRequestCounts[pod.Name] = requestCount
		} else {
			requestCount += runningReqs.GetSimpleValue()
			podRequestCounts[pod.Name] = requestCount
			if requestCount < minObservedRequestCount {
				minObservedRequestCount = requestCount
				minRequestPod = pod
			}
			maxObservedRequestCount = math.Max(maxObservedRequestCount, requestCount)
		}
		maxRequestCount = math.Max(maxRequestCount, podRequestCounts[pod.Name])

		tokenThroughput, throughputErr := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgGenerationThroughputToksPerS)
		if throughputErr != nil {
			podThroughputs[pod.Name] = 0
		} else {
			throughput := tokenThroughput.GetSimpleValue()
			podThroughputs[pod.Name] = throughput
			if throughput < minObservedThroughput {
				minObservedThroughput = throughput
				minThroughputPod = pod
			}
			maxObservedThroughput = math.Max(maxObservedThroughput, throughput)
			maxThroughput = math.Max(maxThroughput, throughput)
		}

		kvUsage, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.KVCacheUsagePerc)
		kvUsageVal := float64(0)
		if err == nil {
			kvUsageVal = kvUsage.GetSimpleValue()
		}
		podFreeGpuUsage[pod.Name] = math.Round(100 - kvUsageVal*100)
		if podFreeGpuUsage[pod.Name] <= 0 {
			podFreeGpuUsage[pod.Name] = 0.1
		}
		maxFreeGPUUsage = math.Max(maxFreeGPUUsage, podFreeGpuUsage[pod.Name])
	}

	if minRequestPod != nil && maxObservedRequestCount-minObservedRequestCount >= aibrixDecodeLoadImbalanceMinSpread {
		klog.V(4).InfoS("request imbalance at decode pods", "request_id", ctx.RequestID,
			"min_request_count", minObservedRequestCount, "max_request_count", maxObservedRequestCount,
			"free_gpu_percent", podFreeGpuUsage[minRequestPod.Name], "decode_pod", minRequestPod.Name)
		return minRequestPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	if minThroughputPod != nil && maxObservedThroughput-minObservedThroughput > aibrixDecodeThroughputImbalanceMinSpread {
		klog.V(4).InfoS("throughput imbalance at decode pods", "request_id", ctx.RequestID,
			"min_request_count", minObservedRequestCount, "max_request_count", maxObservedRequestCount,
			"min_throughput", minObservedThroughput, "max_throughput", maxObservedThroughput,
			"free_gpu_percent", podFreeGpuUsage[minThroughputPod.Name], "decode_pod", minThroughputPod.Name)
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
		klog.V(4).InfoS("drain rate imbalance at decode pods", "request_id", ctx.RequestID,
			"min_score", minScore, "max_score", maxScore,
			"ratio", maxScore/minScore, "decode_pod", minScorePod.Name)
		return minScorePod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	return nil, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
}

// scorePrefillPods scores candidate prefill pods with prefillPolicy after any imbalance
// fast path has narrowed the list. prefillPods are shuffled before scoring so ties
// within a roleset are broken randomly.
//
// Active prefill counts come from PrefillRequestTracker (in-flight prefill HTTP from
// other concurrent requests; the current request is not counted yet). Pods whose count
// exceeds mean + standardDeviationFactor×stddev
// (AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR) are skipped as overloaded. When
// every pod in a roleset is filtered this way, that roleset has no entry in the
// returned map and finalPDScore will not be able to select it.
//
// Returns a per-roleset score map, maxPrefillScore (the maximum score among all pods
// scored in this pass, used by finalPDScore for normalization), and prefix hashes
// from the policy's Prepare step for asynchronous prefix-cache updates.
// Returns nil, 0, nil when Prepare fails (distinct from an empty map, which means
// all pods were filtered by the stddev check).
//
// Policy resolution: the policy argument, then r.prefillPolicy, then prefix_cache.
func (r *pdRouter) scorePrefillPods(routingCtx *types.RoutingContext, prefillPods []*v1.Pod, prefillPolicy pd.PrefillScorePolicy) (map[string]*Scores, float64, []uint64) {
	policy := prefillPolicy
	if policy == nil {
		policy = r.prefillPolicy
	}
	if policy == nil {
		tbl := r.prefixCacheIndexer
		if tbl == nil {
			tbl = prefixcacheindexer.GetSharedPrefixHashTable()
		}
		policy = newPrefixCachePrefillPolicy(tbl)
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

	scorer, err := policy.Prepare(routingCtx, prefillPods, readyPodsMap)
	if err != nil {
		klog.ErrorS(err, "prefill scorer preparation failed",
			"request_id", routingCtx.RequestID, "policy", policy.Name(), "model", routingCtx.Model)
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

// scoreDecodePods scores candidate decode pods with policy after any imbalance fast path
// has narrowed the list. filteredDecodePods are shuffled before scoring.
//
// Metric inputs (podRequestCounts, podThroughputs, podFreeGpuUsage and their batch
// maxima) are normally populated by loadImbalanceSelectDecodePod; when that returns
// nil, the caller still passes the maps built during that pass.
//
// Policy resolution: the policy argument, then r.decodePolicy, then load_balancing.
// When at least one pod reports RealtimeNumRequestsRunning, pods missing that metric
// receive a cold-start score (1.0 + PendingDecodeTracker pending count) instead of
// full policy scoring. podRequestCounts include pending decode from concurrent Route
// calls that have registered AddPendingDecode. If no pod has the running-request metric,
// all pods are scored normally.
//
// Lower score is better. The lowest-scoring pod per roleset is kept in DecodeScoreRun.
// PerRoleset. Invalid (NaN) scores from the primary policy are retried once with
// load_balancing; FallbackUsed is set when that happens. Pods still invalid after
// fallback are skipped. MaxScore is the maximum score among pods scored in this pass
// (minimum 0.01), used by finalPDScore for normalization.
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

	anyMetricsReady := false
	metricsReadyByPod := make(map[string]bool, len(filteredDecodePods))
	for _, pod := range filteredDecodePods {
		ready := r.decodePodMetricsReady(routingCtx, pod)
		metricsReadyByPod[pod.Name] = ready
		if ready {
			anyMetricsReady = true
		}
	}

	scoredPods := make([]string, 0, len(filteredDecodePods))
	skippedPods := make([]string, 0, len(filteredDecodePods))

	for _, pod := range filteredDecodePods {
		rolesetName := pod.Labels[PDRoleSetIdentifier]
		if anyMetricsReady && !metricsReadyByPod[pod.Name] {
			// Cold-start: assign a neutral score (1.0 = idle warm pod) plus gateway-tracked
			// pending requests so the roleset competes fairly instead of being excluded entirely.
			// Once the pod's first request completes and metrics arrive, it transitions to full scoring.
			pending := float64(r.pendingDecodeTracker.GetPendingDecodeCount(pod.Name))
			coldScore := 1.0 + pending
			scoredPods = append(scoredPods, fmt.Sprintf("%s:score=%.4f,roleset=%s(cold)", pod.Name, coldScore, rolesetName))
			klog.V(4).InfoS("decode pod metrics not ready, using cold-start score",
				"request_id", routingCtx.RequestID, "pod", pod.Name, "roleset", rolesetName,
				"cold_score", coldScore, "pending", pending)
			if existing, exists := out.PerRoleset[rolesetName]; !exists || coldScore < existing.Score {
				out.PerRoleset[rolesetName] = pd.RolesetDecodePick{Pod: pod, Score: coldScore}
			}
			if coldScore > out.MaxScore {
				out.MaxScore = coldScore
			}
			continue
		}
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
				skippedPods = append(skippedPods, fmt.Sprintf("%s:invalid_score", pod.Name))
				klog.V(4).InfoS("decode score invalid after policy and load_balancing fallback, skipping pod",
					"request_id", routingCtx.RequestID, "pod", pod.Name, "policy", policyName,
					"roleset", pod.Labels[PDRoleSetIdentifier])
				continue
			}
		}

		scoredPods = append(scoredPods, fmt.Sprintf("%s:score=%.4f,roleset=%s", pod.Name, decodeScore, rolesetName))

		if existing, exists := out.PerRoleset[rolesetName]; !exists || decodeScore < existing.Score {
			out.PerRoleset[rolesetName] = pd.RolesetDecodePick{Pod: pod, Score: decodeScore}
		}
		if decodeScore > out.MaxScore {
			out.MaxScore = decodeScore
		}
	}

	if klog.V(4).Enabled() {
		perRolesetBest := make([]string, 0, len(out.PerRoleset))
		for roleset, pick := range out.PerRoleset {
			perRolesetBest = append(perRolesetBest, fmt.Sprintf("%s:%s=%.4f", roleset, pick.Pod.Name, pick.Score))
		}
		sort.Strings(perRolesetBest)
		klog.V(4).InfoS("decode_score_summary",
			"request_id", routingCtx.RequestID, "policy", policyName,
			"fallback_used", out.FallbackUsed, "any_metrics_ready", anyMetricsReady,
			"scored", strings.Join(scoredPods, " "), "skipped", strings.Join(skippedPods, " "),
			"per_roleset_best", strings.Join(perRolesetBest, " "),
		)
	}

	return out
}

// finalPDScore picks the winning prefill/decode pod pair after scorePrefillPods and
// scoreDecodePods. Only rolesets with an entry in both prefillScores and
// decodeRun.PerRoleset are considered, so prefill and decode always come from the
// same roleset.
//
// For each eligible roleset, combined score is:
//
//	normalizedPrefill + normalizedDecode
//	  = prefillScore.Score / maxPrefillScore + decodePick.Score / decodeRun.MaxScore
//
// Lower combined score wins. Ties retain whichever roleset was last seen with that
// score (map iteration order). Emits per-roleset final_score V(4) logs.
//
// Error cases:
//   - decodeRun.Err is set: decode scoring failed upstream.
//   - prefillScores is nil: Prepare failed in scorePrefillPods.
//   - prefillScores is non-nil but empty: all prefill pods were filtered by the stddev
//     check, leaving no eligible prefill candidate; targetPrefillPod stays nil.
//   - prefillScores is non-empty but no roleset has a matching decode entry: the
//     roleset sets are disjoint after load-imbalance alignment; targetDecodePod stays nil.
//
// On success, enqueues a prefix-cache update when prefixHashes is non-empty, increments
// internal selection counters, and emits PDSelectedPrefillPodTotal / PDSelectedDecodePodTotal.
// Does not update request trackers; Route registers them after pod selection returns.
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
			klog.V(4).InfoS("final_score_skip_roleset",
				"request_id", routingCtx.RequestID, "roleset", roleset,
				"prefill_pod", prefillScore.Pod.Name, "reason", "no_decode_score_for_roleset")
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

		klog.V(4).InfoS("final_score",
			"request_id", routingCtx.RequestID, "roleset", roleset,
			"final_score", final, "prefill_pod", prefillScore.Pod.Name,
			"decode_pod", decodePick.Pod.Name,
			"prefill_score", prefillScore.Score, "normalized_prefill_score", normalizedPrefillScore,
			"decode_score", decodePick.Score, "normalized_decode_score", normalizedDecodeScore,
			"decode_policy", decodeRun.Policy, "decode_fallback_used", decodeRun.FallbackUsed,
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
		// Pods without model.aibrix.ai/config are not bucket-scoped; treat as any length.
		return true
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
// Phase 2 builds the output slices from replica-complete rolesets (all expected
// prefill and decode replicas ready). When prompt-length bucketing is enabled, it
// also produces filtered prefill/decode slices restricted to pods whose capacity
// bucket covers promptLength; bucket-filtered prefill and decode slices are applied
// together only when both sides have suitable pods.
//
// Returns (prefillPods, decodePods, promptLengthBucketingPrefillPods,
// promptLengthBucketingDecodePods, combinedPods).
func (r *pdRouter) collectAndBucketPods(routingCtx *types.RoutingContext, readyPods []*v1.Pod, promptLength int) ([]*v1.Pod, []*v1.Pod, []*v1.Pod, []*v1.Pod, []*v1.Pod) {
	bucketingEnabled := aibrixPromptLengthBucketing

	var combinedPods []*v1.Pod
	if bucketingEnabled {
		for _, pod := range readyPods {
			_, hasRoleset := pod.Labels[PDRoleSetIdentifier]
			if !hasRoleset {
				continue
			}
			roleID, hasRole := pod.Labels[PDRoleIdentifier]
			if !hasRole || roleID == PDRolePrefill || roleID == PDRoleDecode {
				continue
			}
			if !isPodWithHTTPServer(pod) {
				continue
			}
			if isCombinedPod(routingCtx, pod) && r.isPodSuitableForPromptLength(routingCtx, pod, promptLength) {
				combinedPods = append(combinedPods, pod)
			}
		}
	}

	eligible := eligiblePDRolesets(readyPods)
	var prefillPods, decodePods []*v1.Pod
	var promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods []*v1.Pod

	for _, b := range eligible {
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

	// Apply bucket-filtered pods atomically so prefill/decode stay roleset-aligned.
	if bucketingEnabled && len(promptLengthBucketingPrefillPods) > 0 && len(promptLengthBucketingDecodePods) > 0 {
		prefillPods = promptLengthBucketingPrefillPods
		decodePods = promptLengthBucketingDecodePods
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

// decodePodMetricsReady reports whether RealtimeNumRequestsRunning is available for pod.
// Used by scoreDecodePods to gate cold-start scoring vs full policy scoring.
func (r *pdRouter) decodePodMetricsReady(ctx *types.RoutingContext, pod *v1.Pod) bool {
	if r == nil || r.cache == nil || ctx == nil || pod == nil {
		return false
	}
	_, runningErr := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
	return runningErr == nil
}

func getLLMEngine(pod *v1.Pod, labelName string, defaultValue string) string {
	labelTarget, ok := pod.Labels[labelName]
	if !ok {
		return defaultValue
	}
	return labelTarget
}

// ValidateAndGetLLMEngine validates that all pods use the same LLM engine label and returns it.
func ValidateAndGetLLMEngine(pods []*v1.Pod) (string, error) {
	if len(pods) == 0 {
		return "", fmt.Errorf("no pods provided")
	}

	firstEngine := getLLMEngine(pods[0], LLMEngineIdentifier, VLLMEngine)

	for i := 1; i < len(pods); i++ {
		engine := getLLMEngine(pods[i], LLMEngineIdentifier, VLLMEngine)
		if engine != firstEngine {
			return "", fmt.Errorf("inconsistent LLM engines detected: pod %s has %s, pod %s has %s",
				pods[0].Name, firstEngine, pods[i].Name, engine)
		}
	}

	return firstEngine, nil
}
