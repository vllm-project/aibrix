/*
Copyright 2026 The Aibrix Team.

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
	"errors"
	"math"
	"math/rand"
	"sync"
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

const RouterLMetricLocal types.RoutingAlgorithm = "l-metric-local"

func init() {
	Register(RouterLMetricLocal, NewLMetricLocalRouter)
}

type localRequestInfo struct {
	podName string
	tokens  int64
}

type podLocalStats struct {
	pendingTokens   int64
	pendingRequests int64
}

type lMetricLocalRouter struct {
	cache              cache.Cache
	tokenizer          tokenizer.Tokenizer
	prefixCacheIndexer *prefixcacheindexer.PrefixHashTable
	tokenizerPool      TokenizerPoolInterface

	mu sync.Mutex

	podStats map[string]*podLocalStats

	requests map[string]*localRequestInfo
}

func NewLMetricLocalRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		klog.Error("fail to get cache store in l-metric-local router")
		return nil, err
	}

	var tokenizerObj tokenizer.Tokenizer
	var tokenizerPool *TokenizerPool

	useRemoteTokenizer := utils.LoadEnvBool(constants.EnvPrefixCacheUseRemoteTokenizer, false)

	if useRemoteTokenizer {
		poolConfig := TokenizerPoolConfig{
			EnableVLLMRemote:     true,
			EndpointTemplate:     utils.LoadEnv("AIBRIX_VLLM_TOKENIZER_ENDPOINT_TEMPLATE", "http://%s:8000"),
			HealthCheckPeriod:    utils.LoadEnvDuration("AIBRIX_TOKENIZER_HEALTH_CHECK_PERIOD", 30) * time.Second,
			TokenizerTTL:         utils.LoadEnvDuration("AIBRIX_TOKENIZER_TTL", 300) * time.Second,
			MaxTokenizersPerPool: utils.LoadEnvInt("AIBRIX_MAX_TOKENIZERS_PER_POOL", 100),
			Timeout:              utils.LoadEnvDuration("AIBRIX_TOKENIZER_REQUEST_TIMEOUT", 5) * time.Second,
			ModelServiceMap:      make(map[string]string),
		}

		tokenizerType := utils.LoadEnv(constants.EnvPrefixCacheTokenizerType, "character")
		var defaultTokenizer tokenizer.Tokenizer
		if tokenizerType == tokenizerTypeTiktoken {
			defaultTokenizer = tokenizer.NewTiktokenTokenizer()
		} else {
			defaultTokenizer = tokenizer.NewCharacterTokenizer()
		}
		poolConfig.DefaultTokenizer = defaultTokenizer

		pool := NewTokenizerPool(poolConfig, c)
		tokenizerPool = pool
		tokenizerObj = &panicTokenizer{}
	} else {
		tokenizerType := utils.LoadEnv(constants.EnvPrefixCacheTokenizerType, "character")
		if tokenizerType == tokenizerTypeTiktoken {
			tokenizerObj = tokenizer.NewTiktokenTokenizer()
		} else {
			tokenizerObj = tokenizer.NewCharacterTokenizer()
		}
	}

	router := &lMetricLocalRouter{
		cache:              c,
		tokenizer:          tokenizerObj,
		prefixCacheIndexer: prefixcacheindexer.GetSharedPrefixHashTable(),
		podStats:           make(map[string]*podLocalStats),
		requests:           make(map[string]*localRequestInfo),
	}

	if tokenizerPool != nil {
		router.tokenizerPool = tokenizerPool
	}

	c.RegisterRequestTracker(router)

	return router, nil
}

func (r *lMetricLocalRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

func (r *lMetricLocalRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	tokenizerToUse := r.getTokenizerForRequest(ctx, readyPodList)
	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		klog.ErrorS(err, "failed to tokenize input text", "request_id", ctx.RequestID)
		return nil, nil, err
	}
	totalTokens := len(tokens)

	readyPodsMap := map[string]struct{}{}
	for _, pod := range pods {
		readyPodsMap[pod.Name] = struct{}{}
	}
	matchedPods, _ := r.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, readyPodsMap)

	r.mu.Lock()
	defer r.mu.Unlock()

	for i, pod := range pods {
		matchPercent := matchedPods[pod.Name]
		hitTokens := int64(totalTokens * matchPercent / 100)
		newRequestTokens := int64(totalTokens) - hitTokens

		stats := r.getOrCreatePodStatsLocked(pod.Name)
		existingPendingTokens := stats.pendingTokens
		existingPendingRequests := stats.pendingRequests

		lMetricValue := existingPendingTokens + newRequestTokens
		bs := float64(existingPendingRequests + 1)

		scores[i] = float64(lMetricValue) * bs
		scored[i] = true

		klog.InfoS("l_metric_local_score",
			"request_id", ctx.RequestID,
			"pod_name", pod.Name,
			"total_tokens", totalTokens,
			"hit_tokens", hitTokens,
			"new_request_tokens", newRequestTokens,
			"existing_pending_tokens", existingPendingTokens,
			"existing_pending_requests", existingPendingRequests,
			"l_metric", lMetricValue,
			"bs", bs,
			"score", scores[i])
	}

	return scores, scored, nil
}

func (r *lMetricLocalRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	readyPods := readyPodList.All()
	if len(readyPods) == 0 {
		return "", errors.New("no ready pods for routing")
	}

	scores, scored, err := r.ScoreAll(ctx, readyPodList)
	if err != nil {
		return "", err
	}

	var targetPod *v1.Pod
	var targetPods []string
	minScore := math.MaxFloat64

	for i, pod := range readyPods {
		if !scored[i] {
			continue
		}

		if scores[i] < minScore {
			minScore = scores[i]
			targetPods = []string{pod.Name}
		} else if scores[i] == minScore {
			targetPods = append(targetPods, pod.Name)
		}
	}

	if len(targetPods) > 0 {
		targetPod, _ = utils.FilterPodByName(targetPods[rand.Intn(len(targetPods))], readyPods)
	}

	if targetPod == nil {
		targetPod, err = SelectRandomPodAsFallback(ctx, readyPods, rand.Intn)
		if err != nil {
			return "", err
		}
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func (r *lMetricLocalRouter) PostRouteUpdate(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) error {
	tokenizerToUse := r.getTokenizerForRequest(ctx, readyPodList)
	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		return err
	}

	totalTokens := len(tokens)
	readyPodsMap := map[string]struct{}{targetPod.Name: {}}
	matchedPods, _ := r.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, readyPodsMap)

	matchPercent := matchedPods[targetPod.Name]
	hitTokens := int64(totalTokens * matchPercent / 100)
	newRequestTokens := int64(totalTokens) - hitTokens

	prefixHashes := r.prefixCacheIndexer.GetPrefixHashes(tokens)
	if len(prefixHashes) > 0 {
		r.prefixCacheIndexer.AddPrefix(prefixHashes, ctx.Model, targetPod.Name)
	}

	r.mu.Lock()
	r.requests[ctx.RequestID] = &localRequestInfo{
		podName: targetPod.Name,
		tokens:  newRequestTokens,
	}
	r.mu.Unlock()

	klog.V(4).InfoS("l_metric_local_post_route",
		"request_id", ctx.RequestID,
		"target_pod", targetPod.Name,
		"total_tokens", totalTokens,
		"hit_tokens", hitTokens,
		"new_request_tokens", newRequestTokens)

	return nil
}

func (r *lMetricLocalRouter) SubscribedMetrics() []string {
	return nil
}

func (r *lMetricLocalRouter) AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) int64 {
	if ctx == nil || !ctx.HasRouted() {
		return 0
	}

	targetPod := ctx.TargetPod()
	if targetPod == nil {
		return 0
	}

	r.mu.Lock()
	info, ok := r.requests[requestID]
	if !ok {
		r.mu.Unlock()
		newRequestTokens := r.calcNewRequestTokens(ctx, targetPod)
		r.mu.Lock()
		info, ok = r.requests[requestID]
		if !ok {
			info = &localRequestInfo{
				podName: targetPod.Name,
				tokens:  newRequestTokens,
			}
			r.requests[requestID] = info
		}
	}

	stats := r.getOrCreatePodStatsLocked(info.podName)
	stats.pendingTokens += info.tokens
	stats.pendingRequests++

	podName := info.podName
	tokens := info.tokens
	pendingTokens := stats.pendingTokens
	pendingRequests := stats.pendingRequests
	r.mu.Unlock()

	klog.V(4).InfoS("l_metric_local_add_request",
		"request_id", requestID,
		"pod_name", podName,
		"tokens", tokens,
		"pending_tokens", pendingTokens,
		"pending_requests", pendingRequests)

	return 0
}

func (r *lMetricLocalRouter) calcNewRequestTokens(ctx *types.RoutingContext, targetPod *v1.Pod) int64 {
	tokenizerToUse := r.tokenizer
	if r.tokenizerPool != nil {
		tokenizerToUse = r.tokenizerPool.GetTokenizer(ctx.Model, []*v1.Pod{targetPod})
	}

	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		klog.V(4).InfoS("l_metric_local_tokenize_failed", "request_id", ctx.RequestID, "err", err)
		return 0
	}

	totalTokens := len(tokens)
	readyPodsMap := map[string]struct{}{targetPod.Name: {}}
	matchedPods, _ := r.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, readyPodsMap)
	matchPercent := matchedPods[targetPod.Name]
	hitTokens := int64(totalTokens * matchPercent / 100)
	newRequestTokens := int64(totalTokens) - hitTokens

	prefixHashes := r.prefixCacheIndexer.GetPrefixHashes(tokens)
	if len(prefixHashes) > 0 {
		r.prefixCacheIndexer.AddPrefix(prefixHashes, ctx.Model, targetPod.Name)
	}

	return newRequestTokens
}

func (r *lMetricLocalRouter) DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64) {
	r.removeRequest(requestID)
}

func (r *lMetricLocalRouter) DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, inputTokens, outputTokens, traceTerm int64) {
	r.removeRequest(requestID)
}

func (r *lMetricLocalRouter) removeRequest(requestID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	info, ok := r.requests[requestID]
	if !ok {
		return
	}

	stats, exists := r.podStats[info.podName]
	if exists {
		stats.pendingTokens -= info.tokens
		if stats.pendingTokens < 0 {
			stats.pendingTokens = 0
		}
		stats.pendingRequests--
		if stats.pendingRequests < 0 {
			stats.pendingRequests = 0
		}

		klog.V(4).InfoS("l_metric_local_done_request",
			"request_id", requestID,
			"pod_name", info.podName,
			"tokens", info.tokens,
			"pending_tokens", stats.pendingTokens,
			"pending_requests", stats.pendingRequests)
	}

	delete(r.requests, requestID)
}

func (r *lMetricLocalRouter) getOrCreatePodStatsLocked(podName string) *podLocalStats {
	stats, ok := r.podStats[podName]
	if !ok {
		stats = &podLocalStats{}
		r.podStats[podName] = stats
	}
	return stats
}

func (r *lMetricLocalRouter) getTokenizerForRequest(ctx *types.RoutingContext, readyPodList types.PodList) tokenizer.Tokenizer {
	if r.tokenizerPool != nil {
		return r.tokenizerPool.GetTokenizer(ctx.Model, readyPodList.All())
	}
	return r.tokenizer
}
