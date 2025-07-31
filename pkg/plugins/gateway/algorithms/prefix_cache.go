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
	"math"
	"math/rand"
	"sort"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	defaultTokenizerType                      = "character"
	defaultPodRunningRequestImbalanceAbsCount = 8
	defaultStandardDeviationFactor            = 1
)

var (
	RouterPrefixCache                  types.RoutingAlgorithm = "prefix-cache"
	tokenizerType                                             = utils.LoadEnv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", "character")
	podRunningRequestImbalanceAbsCount int                    = utils.LoadEnvInt("AIBRIX_PREFIX_CACHE_POD_RUNNING_REQUEST_IMBALANCE_ABS_COUNT", defaultPodRunningRequestImbalanceAbsCount)
	standardDeviationFactor            int                    = utils.LoadEnvInt("AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR", defaultStandardDeviationFactor)
)

func init() {
	Register(RouterPrefixCache, NewPrefixCacheRouter)
}

type prefixCacheRouter struct {
	cache              cache.Cache
	tokenizer          tokenizer.Tokenizer
	prefixCacheIndexer *prefixcacheindexer.PrefixHashTable
}

func NewPrefixCacheRouter() (types.Router, error) {
	// Create tokenizer based on type
	// Supported tokenizers: ["character", "tiktoken"]
	// Default: "character" for any unrecognized type
	// TODO: Add support for "remote" and "vllm" tokenizer types in a future PR.
	//       This will require proper configuration handling for remote endpoints.
	var tokenizerObj tokenizer.Tokenizer
	if tokenizerType == "tiktoken" {
		tokenizerObj = tokenizer.NewTiktokenTokenizer()
	} else {
		// Default to character tokenizer for backward compatibility
		if tokenizerType != "character" {
			klog.InfoS("unrecognized tokenizer type, defaulting to character", "type", tokenizerType)
		}
		tokenizerObj = tokenizer.NewCharacterTokenizer()
	}

	c, err := cache.Get()
	if err != nil {
		klog.Error("fail to get cache store in prefix cache router")
		return nil, err
	}

	klog.InfoS("prefix_cache_configurations",
		"tokenizer_type", tokenizerType,
		"pod_running_request_imbalance_abs_count", podRunningRequestImbalanceAbsCount,
		"matched_pods_running_requests_standard_deviation_factor", standardDeviationFactor)

	return prefixCacheRouter{
		cache:              c,
		tokenizer:          tokenizerObj,
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
	}, nil
}

func (p prefixCacheRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	var prefixHashes []uint64
	var matchedPods map[string]int
	var targetPod *v1.Pod

	tokens, err := p.tokenizer.TokenizeInputText(ctx.Message)
	if err != nil {
		return "", err
	}

	readyPods := readyPodList.All()
	readyPodsMap := map[string]struct{}{}
	for _, pod := range readyPods {
		readyPodsMap[pod.Name] = struct{}{}
	}

	var isLoadImbalanced bool
	targetPod, isLoadImbalanced = getTargetPodOnLoadImbalance(p.cache, readyPods)
	if isLoadImbalanced {
		prefixHashes = p.prefixCacheIndexer.GetPrefixHashes(tokens)
		klog.InfoS("prefix_cache_load_imbalanced",
			"request_id", ctx.RequestID,
			"target_pod", targetPod.Name,
			"target_pod_ip", targetPod.Status.PodIP,
			"pod_request_count", getRequestCounts(p.cache, readyPods))
	} else {
		matchedPods, prefixHashes = p.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, readyPodsMap)
		klog.InfoS("prefix_hashes", "request_id", ctx.RequestID, "prefix_hashes", prefixHashes)

		if len(matchedPods) > 0 {
			targetPod = getTargetPodFromMatchedPods(p.cache, readyPods, matchedPods)
			if targetPod != nil {
				klog.InfoS("prefix_cache_matched_pods",
					"request_id", ctx.RequestID,
					"target_pod", targetPod.Name,
					"target_pod_ip", targetPod.Status.PodIP,
					"matched_pods", matchedPods,
					"pod_request_count", getRequestCounts(p.cache, readyPods))
			} else {
				klog.InfoS("prefix_cache_skip_matched_pods",
					"request_id", ctx.RequestID,
					"matched_pods", matchedPods,
					"pod_request_count", getRequestCounts(p.cache, readyPods))
			}
		}
	}

	// no pod with prefix match, as a fallback select pod with least request count
	if len(matchedPods) == 0 || targetPod == nil {
		targetPod = selectTargetPodWithLeastRequestCount(p.cache, readyPods)
		klog.InfoS("prefix_cache_fallback_least_request_count",
			"request_id", ctx.RequestID,
			"target_pod", targetPod.Name,
			"target_pod_ip", targetPod.Status.PodIP,
			"matched_pods", matchedPods,
			"pod_request_count", getRequestCounts(p.cache, readyPods))
	}

	if len(prefixHashes) > 0 {
		p.prefixCacheIndexer.AddPrefix(prefixHashes, ctx.Model, targetPod.Name)
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func getTargetPodFromMatchedPods(cache cache.Cache, readyPods []*v1.Pod, matchedPods map[string]int) *v1.Pod {
	var targetPodName string
	requestCount := []float64{}

	podRequestCount := getRequestCounts(cache, readyPods)
	for _, cnt := range podRequestCount {
		requestCount = append(requestCount, float64(cnt))
	}
	meanRequestCount := mean(requestCount)
	stdDevRequestCount := standardDeviation(requestCount)

	podnames := []string{}
	for podname := range matchedPods {
		podnames = append(podnames, podname)
	}
	rand.Shuffle(len(podnames), func(i, j int) {
		podnames[i], podnames[j] = podnames[j], podnames[i]
	})

	// sort pods with decreasing %perfixmatch AND for same %prefixmatch sort by increasing request count
	sort.SliceStable(podnames, func(i, j int) bool {
		if matchedPods[podnames[i]] == matchedPods[podnames[j]] {
			return podRequestCount[podnames[i]] < podRequestCount[podnames[j]]
		}
		return matchedPods[podnames[i]] > matchedPods[podnames[j]]
	})

	// select targetpod with highest %prefixmatch and request_count within stddev
	for _, podname := range podnames {
		reqCnt := float64(podRequestCount[podname])
		if reqCnt <= meanRequestCount+float64(standardDeviationFactor)*stdDevRequestCount {
			targetPodName = podname
			break
		}
	}
	targetPod, _ := utils.FilterPodByName(targetPodName, readyPods)
	return targetPod
}

// getTargetPodOnLoadImbalance evaluates if the load is imbalanced based on the abs difference between
// pods with min and max outstanding request counts
func getTargetPodOnLoadImbalance(cache cache.Cache, readyPods []*v1.Pod) (*v1.Pod, bool) {
	var imbalance bool
	var targetPod *v1.Pod
	targetPods := []string{}
	minValue := math.MaxInt32
	maxValue := math.MinInt32

	podRequestCount := getRequestCounts(cache, readyPods)
	for _, value := range podRequestCount {
		if value <= minValue {
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

	if maxValue-minValue > podRunningRequestImbalanceAbsCount {
		targetPod, _ = utils.FilterPodByName(targetPods[rand.Intn(len(targetPods))], readyPods)
		imbalance = true
	}

	return targetPod, imbalance
}
