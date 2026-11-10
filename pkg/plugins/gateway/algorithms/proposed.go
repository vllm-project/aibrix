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
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const RouterProposed types.RoutingAlgorithm = "proposed"

const (
	proposedEpsilon       = 1e-6
	proposedDefaultLambda = 256.0
)

func init() {
	Register(RouterProposed, NewProposedRouter)
}

type proposedRouter struct {
	cache              cache.Cache
	tokenizer          tokenizer.Tokenizer
	prefixCacheIndexer *prefixcacheindexer.PrefixHashTable
	tokenizerPool      TokenizerPoolInterface
	kvSyncRouter       *proposedKvSyncRouter
}

type proposedKvSyncRouter struct {
	cache         cache.Cache
	tokenizerPool TokenizerPoolInterface
	syncIndexer   *syncindexer.SyncPrefixHashTable
}

func NewProposedRouter() (types.Router, error) {
	var useRemoteTokenizer = utils.LoadEnvBool(constants.EnvPrefixCacheUseRemoteTokenizer, false)
	var kvSyncEnabled = utils.LoadEnvBool(constants.EnvPrefixCacheKVEventSyncEnabled, false)

	if kvSyncEnabled && !useRemoteTokenizer {
		klog.Warning("KV event sync requires remote tokenizer. Remote tokenizer will be automatically enabled.")
		useRemoteTokenizer = true
	}

	c, err := cache.Get()
	if err != nil {
		klog.Error("fail to get cache store in proposed router")
		return nil, err
	}

	var tokenizerObj tokenizer.Tokenizer
	var tokenizerPool *TokenizerPool

	if useRemoteTokenizer {
		poolConfig := TokenizerPoolConfig{
			EnableVLLMRemote:     true,
			EndpointTemplate:     utils.LoadEnv("AIBRIX_VLLM_TOKENIZER_ENDPOINT_TEMPLATE", "http://%s:8000"),
			HealthCheckPeriod:    utils.LoadEnvDuration("AIBRIX_TOKENIZER_HEALTH_CHECK_PERIOD", 30) * time.Second,
			TokenizerTTL:         utils.LoadEnvDuration("AIBRIX_TOKENIZER_TTL", 300) * time.Second,
			MaxTokenizersPerPool: utils.LoadEnvInt("AIBRIX_MAX_TOKENIZERS_PER_POOL", 100),
			DefaultTokenizer:     newTokenizer(),
			Timeout:              utils.LoadEnvDuration("AIBRIX_TOKENIZER_REQUEST_TIMEOUT", 5) * time.Second,
			ModelServiceMap:      make(map[string]string),
		}
		pool := NewTokenizerPool(poolConfig, c)
		tokenizerPool = pool
		tokenizerObj = &panicTokenizer{}
		klog.Info("ProposedRouter: TokenizerPool initialized with remote tokenizer support")
	} else {
		tokenizerObj = newTokenizer()
	}

	router := &proposedRouter{
		cache:              c,
		tokenizer:          tokenizerObj,
		prefixCacheIndexer: prefixcacheindexer.GetSharedPrefixHashTable(),
	}
	if tokenizerPool != nil {
		router.tokenizerPool = tokenizerPool
	}

	if kvSyncEnabled && useRemoteTokenizer && tokenizerPool != nil {
		router.kvSyncRouter = &proposedKvSyncRouter{
			cache:         c,
			tokenizerPool: tokenizerPool,
			syncIndexer:   syncindexer.GetSharedSyncPrefixHashTable(),
		}
	}

	return router, nil
}

func (p *proposedRouter) getTokenizerForRequest(ctx *types.RoutingContext, readyPodList types.PodList) tokenizer.Tokenizer {
	if p.tokenizerPool != nil {
		return p.tokenizerPool.GetTokenizer(ctx.Model, readyPodList.All())
	}
	return p.tokenizer
}

func (k *proposedKvSyncRouter) getTokenizerForRequest(ctx *types.RoutingContext, readyPodList types.PodList) tokenizer.Tokenizer {
	if k.tokenizerPool != nil {
		return k.tokenizerPool.GetTokenizer(ctx.Model, readyPodList.All())
	}
	return nil
}

func (r *proposedRouter) Polarity() types.Polarity {
	return types.PolarityMost
}

func (r *proposedRouter) getPrefixMatchPercentages(ctx *types.RoutingContext, readyPodList types.PodList) (map[*v1.Pod]float64, error) {
	pods := readyPodList.All()
	result := make(map[*v1.Pod]float64, len(pods))
	for _, pod := range pods {
		result[pod] = 0.0
	}

	if r.kvSyncRouter != nil {
		return r.kvSyncRouter.getPrefixMatchPercentages(ctx, readyPodList)
	}

	tokenizerToUse := r.getTokenizerForRequest(ctx, readyPodList)
	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		return nil, err
	}

	readyPodsMap := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		readyPodsMap[pod.Name] = struct{}{}
	}

	matchedPods, _ := r.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, readyPodsMap)
	for _, pod := range pods {
		matchPercent, ok := matchedPods[pod.Name]
		if ok {
			result[pod] = math.Max(0.0, math.Min(100.0, float64(matchPercent)))
		}
	}
	return result, nil
}

func (k *proposedKvSyncRouter) getPrefixMatchPercentages(ctx *types.RoutingContext, readyPodList types.PodList) (map[*v1.Pod]float64, error) {
	pods := readyPodList.All()
	result := make(map[*v1.Pod]float64, len(pods))
	for _, pod := range pods {
		result[pod] = 0.0
	}

	modelName := ctx.Model
	if modelName == "" && len(pods) > 0 {
		modelName, _ = constants.ModelNameFromMetadata(pods[0].Labels, pods[0].Annotations)
	}
	loraID := int64(-1)

	tokenizerToUse := k.getTokenizerForRequest(ctx, readyPodList)
	if tokenizerToUse == nil {
		return nil, fmt.Errorf("TokenizerPool not initialized for KV sync proposed router")
	}
	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		return nil, err
	}

	readyPodsMap := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		readyPodsMap[podKey] = struct{}{}
	}

	if k.syncIndexer == nil {
		return nil, fmt.Errorf("sync indexer not available for KV sync proposed routing")
	}
	matchedPods, _ := k.syncIndexer.MatchPrefix(modelName, loraID, tokens, readyPodsMap)

	podKeyToPod := make(map[string]*v1.Pod, len(pods))
	for _, pod := range pods {
		podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		podKeyToPod[podKey] = pod
	}

	for podKey, matchPercent := range matchedPods {
		pod, ok := podKeyToPod[podKey]
		if ok {
			result[pod] = math.Max(0.0, math.Min(100.0, float64(matchPercent)))
		}
	}
	return result, nil
}

func (r *proposedRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	n := len(pods)
	if n == 0 {
		return nil, nil, fmt.Errorf("empty pod list")
	}

	P, err := ctx.PromptLength()
	if err != nil || P <= 0 {
		P = int(proposedDefaultLambda)
	}

	lambda := proposedDefaultLambda
	for _, pod := range pods {
		avgPrompt, errAvg := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.AvgPromptToksPerReq)
		if errAvg == nil {
			v := avgPrompt.GetSimpleValue()
			if v > 0 {
				lambda = v
				break
			}
		}
	}

	alpha := float64(P) / (float64(P) + lambda)

	matchPercentages, errMatch := r.getPrefixMatchPercentages(ctx, readyPodList)
	if errMatch != nil {
		klog.Warningf("proposed: failed to compute prefix match percentages, fallback to 0: %v", errMatch)
		matchPercentages = make(map[*v1.Pod]float64, n)
		for _, pod := range pods {
			matchPercentages[pod] = 0.0
		}
	}

	loadSeq := make([]float64, n)
	loadKv := make([]float64, n)
	C := make([]float64, n)
	M := make([]float64, n)
	scored := make([]bool, n)

	maxSeqLoad := 0.0
	for i, pod := range pods {
		M[i] = matchPercentages[pod]

		seqVal := 0.0
		normPendings, errNP := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNormalizedPendings)
		if errNP == nil {
			seqVal = math.Max(seqVal, normPendings.GetSimpleValue())
		}
		running, errR := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
		if errR == nil {
			seqVal = math.Max(seqVal, running.GetSimpleValue())
		}
		waiting, errW := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.NumRequestsWaiting)
		if errW == nil {
			seqVal = math.Max(seqVal, waiting.GetSimpleValue())
		}
		loadSeq[i] = seqVal
		if seqVal > maxSeqLoad {
			maxSeqLoad = seqVal
		}

		kvVal, errK := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.KVCacheUsagePerc)
		if errK == nil {
			loadKv[i] = math.Min(1.0, math.Max(0.0, kvVal.GetSimpleValue()))
		} else {
			loadKv[i] = 0.0
		}

		scored[i] = true
	}

	denomSeq := math.Max(1.0, maxSeqLoad)
	for i := 0; i < n; i++ {
		seqNorm := math.Min(1.0, loadSeq[i]/denomSeq)
		C[i] = math.Max(seqNorm, loadKv[i])
	}

	sumC := 0.0
	for i := 0; i < n; i++ {
		sumC += C[i]
	}
	avgC := sumC / float64(n)

	scores := make([]float64, n)
	miPows := make([]float64, n)
	loadTerms := make([]float64, n)
	for i := 0; i < n; i++ {
		mi := math.Max(0.0, math.Min(100.0, M[i]))
		miPow := math.Pow(mi/100.0+1, alpha)
		loadTerm := math.Exp(-C[i] / (avgC + proposedEpsilon))
		miPows[i] = miPow
		loadTerms[i] = loadTerm
		scores[i] = miPow * loadTerm
	}

	// Print basic parameters
	klog.Infof("proposed: P(prompt_tokens)=%d lambda=%.2f alpha=%.4f maxSeqLoad=%.2f avgC=%.4f",
		P, lambda, alpha, denomSeq, avgC)

	// Print each pod's score breakdown
	for i := 0; i < n; i++ {
		klog.Infof("proposed pod=%s M=%.4f seq_raw=%.2f seq_norm=%.4f kv=%.4f C=%.4f miPow=%.6f loadTerm=%.6f score=%.6f",
			pods[i].Name, M[i], loadSeq[i], loadSeq[i]/denomSeq, loadKv[i], C[i], miPows[i], loadTerms[i], scores[i])
	}

	return scores, scored, nil
}

func (r *proposedRouter) PostRouteUpdate(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) error {
	if r.kvSyncRouter != nil {
		return r.kvSyncRouter.PostRouteUpdate(ctx, readyPodList, targetPod)
	}

	tokenizerToUse := r.getTokenizerForRequest(ctx, readyPodList)
	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		return err
	}

	prefixHashes := r.prefixCacheIndexer.GetPrefixHashes(tokens)
	if len(prefixHashes) > 0 {
		r.prefixCacheIndexer.AddPrefix(prefixHashes, ctx.Model, targetPod.Name)
	}

	return nil
}

func (k *proposedKvSyncRouter) PostRouteUpdate(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) error {
	pods := readyPodList.All()
	modelName := ctx.Model
	if modelName == "" && len(pods) > 0 {
		modelName, _ = constants.ModelNameFromMetadata(pods[0].Labels, pods[0].Annotations)
	}

	tokenizerToUse := k.getTokenizerForRequest(ctx, readyPodList)
	if tokenizerToUse == nil {
		return fmt.Errorf("TokenizerPool not initialized for KV sync proposed router")
	}
	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		return err
	}

	readyPodsMap := map[string]struct{}{}
	for _, pod := range pods {
		readyPodsMap[fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)] = struct{}{}
	}
	if k.syncIndexer == nil {
		return fmt.Errorf("sync indexer not available for KV sync proposed routing")
	}
	_, prefixHashes := k.syncIndexer.MatchPrefix(modelName, int64(-1), tokens, readyPodsMap)
	if len(prefixHashes) == 0 {
		return nil
	}
	selectedPodKey := fmt.Sprintf("%s/%s", targetPod.Namespace, targetPod.Name)
	return k.syncIndexer.AddPrefix(modelName, int64(-1), selectedPodKey, prefixHashes)
}

func (r *proposedRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	pods := readyPodList.All()
	if len(pods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}
	if len(pods) == 1 {
		ctx.SetTargetPod(pods[0])
		if err := r.PostRouteUpdate(ctx, readyPodList, pods[0]); err != nil {
			klog.Warningf("proposed: post-route update failed: %v", err)
		}
		return ctx.TargetAddress(), nil
	}

	scores, scored, err := r.ScoreAll(ctx, readyPodList)
	if err != nil {
		return "", err
	}

	var targetPod *v1.Pod
	var targetPods []string
	maxScore := -math.MaxFloat64

	for i, pod := range pods {
		if !scored[i] {
			continue
		}
		if scores[i] > maxScore {
			maxScore = scores[i]
			targetPods = []string{pod.Name}
		} else if math.Abs(scores[i]-maxScore) < 1e-12 {
			targetPods = append(targetPods, pod.Name)
		}
	}

	if len(targetPods) > 0 {
		targetPod, _ = utils.FilterPodByName(targetPods[rand.Intn(len(targetPods))], pods)
	}

	if targetPod == nil {
		targetPod, err = SelectRandomPodAsFallback(ctx, pods, rand.Intn)
		if err != nil {
			return "", err
		}
	}

	if err := r.PostRouteUpdate(ctx, readyPodList, targetPod); err != nil {
		klog.Warningf("proposed: post-route update failed: %v", err)
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func (r *proposedRouter) SubscribedMetrics() []string {
	return []string{
		metrics.RealtimeNumRequestsRunning,
		metrics.RealtimeNormalizedPendings,
		metrics.NumRequestsWaiting,
		metrics.KVCacheUsagePerc,
		metrics.AvgPromptToksPerReq,
	}
}
