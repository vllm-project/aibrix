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
	"container/heap"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buraksezer/consistent"
	"github.com/cespare/xxhash/v2"
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

const RouterDualMap types.RoutingAlgorithm = "dualmap"

const (
	DualMapLeastLoaded      = "dualmap_least_loaded"
	DualMapCacheAffinity    = "dualmap_cache_affinity"
	DualMapMinTTFT          = "dualmap_min_ttft"
	DualMapDefault          = "dualmap"
	DualMapNoRebalance      = "dualmap_no_rebalance"
	DualMapTTFTSLO          = "ttft_slo"
	DualMapTTFTAvg          = "ttft_avg"
	DualMapNBCost1          = "nb_cost1"
	DualMapRBCost1          = "rb_cost1"
	DualMapRBCost1Aggresive = "rb_cost1_aggresive"
	DualMapRBCost1Avg       = "rb_cost1_avg"
)

const (
	envDualMapBalanceType                = "AIBRIX_DUALMAP_BALANCE_TYPE"
	envDualMapFirstBalanceTTFTThreshold  = "AIBRIX_DUALMAP_FIRST_BALANCE_TTFT_THRESHOLD"
	envDualMapRebalanceThreshold         = "AIBRIX_DUALMAP_REBALANCE_THRESHOLD"
	envDualMapRebalanceWaitingLatencyThd = "AIBRIX_DUALMAP_REBALANCE_WAITING_LATENCY_THRESHOLD"
	envDualMapPrefillTPOT                = "AIBRIX_DUALMAP_PREFILL_TPOT"
	envDualMapRecomputePunishRatio       = "AIBRIX_DUALMAP_RECOMPUTE_PUNISH_RATIO"
	envDualMapHashRingVirtualNodes       = "AIBRIX_DUALMAP_HASH_RING_VIRTUAL_NODES"
)

var (
	dualMapBalanceType                = utils.LoadEnv(envDualMapBalanceType, DualMapDefault)
	dualMapFirstBalanceTTFTThreshold  = utils.LoadEnvFloat(envDualMapFirstBalanceTTFTThreshold, 1000000.0)
	dualMapRebalanceThreshold         = utils.LoadEnvFloat(envDualMapRebalanceThreshold, 1000000.0)
	dualMapRebalanceWaitingLatencyThd = utils.LoadEnvFloat(envDualMapRebalanceWaitingLatencyThd, 60.0)
	dualMapPrefillTPOT                = utils.LoadEnvFloat(envDualMapPrefillTPOT, 0.05)
	dualMapRecomputePunishRatio       = utils.LoadEnvFloat(envDualMapRecomputePunishRatio, 0.1)
	dualMapHashRingVirtualNodes       = utils.LoadEnvInt(envDualMapHashRingVirtualNodes, 100)
)

func init() {
	Register(RouterDualMap, NewDualMapRouter)
}

type xxhashHasher struct{}

func (h xxhashHasher) Sum64(data []byte) uint64 {
	return xxhash.Sum64(data)
}

type fnvHasher struct{}

func (h fnvHasher) Sum64(data []byte) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	hash := uint64(offset64)
	for _, b := range data {
		hash ^= uint64(b)
		hash *= prime64
	}
	return hash
}

type ringMember string

func (m ringMember) String() string {
	return string(m)
}

type HashRing struct {
	hasher   consistent.Hasher
	ring     *consistent.Consistent
	ringName string
}

func NewHashRing(hasher consistent.Hasher, nodes []string, virtualNodes int) *HashRing {
	ring := &HashRing{
		hasher: hasher,
	}
	ring.rebuild(nodes, virtualNodes)
	return ring
}

func (h *HashRing) rebuild(nodes []string, virtualNodes int) {
	cfg := consistent.Config{
		PartitionCount:    consistent.DefaultPartitionCount,
		ReplicationFactor: virtualNodes,
		Hasher:            h.hasher,
	}
	h.ring = consistent.New(nil, cfg)
	for _, node := range nodes {
		h.ring.Add(ringMember(node))
	}
}

func (h *HashRing) GetNode(key string) (string, error) {
	member := h.ring.LocateKey([]byte(key))
	if member == nil {
		return "", fmt.Errorf("no node found for key: %s", key)
	}
	return member.String(), nil
}

func (h *HashRing) UpdateNodes(nodes []string, virtualNodes int) {
	h.rebuild(nodes, virtualNodes)
}

type RequestItem struct {
	NegCacheHitLen int
	ArrivedAt      time.Time
	Req            *types.RoutingContext
	InputLen       int
}

type PriorityQueue []*RequestItem

func (pq PriorityQueue) Len() int { return len(pq) }

func (pq PriorityQueue) Less(i, j int) bool {
	if pq[i].NegCacheHitLen != pq[j].NegCacheHitLen {
		return pq[i].NegCacheHitLen > pq[j].NegCacheHitLen
	}
	return pq[i].ArrivedAt.Before(pq[j].ArrivedAt)
}

func (pq PriorityQueue) Swap(i, j int) { pq[i], pq[j] = pq[j], pq[i] }

func (pq *PriorityQueue) Push(x interface{}) {
	item := x.(*RequestItem)
	*pq = append(*pq, item)
}

func (pq *PriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	*pq = old[0 : n-1]
	return item
}

type GlobalRequestQueue struct {
	podName                   string
	queue                     PriorityQueue
	globalActualWaitingTokens int64
	globalInputWaitingTokens  int64
}

func NewGlobalRequestQueue(podName string) *GlobalRequestQueue {
	g := &GlobalRequestQueue{
		podName:                   podName,
		queue:                     make(PriorityQueue, 0),
		globalActualWaitingTokens: 0,
		globalInputWaitingTokens:  0,
	}
	heap.Init(&g.queue)
	return g
}

func (g *GlobalRequestQueue) Len() int {
	return g.queue.Len()
}

func (g *GlobalRequestQueue) GetGlobalActualWaitingTokens() int64 {
	return g.globalActualWaitingTokens
}

func (g *GlobalRequestQueue) GetGlobalInputWaitingTokens() int64 {
	return g.globalInputWaitingTokens
}

func (g *GlobalRequestQueue) GetMaxWaitingDelay() float64 {
	if g.queue.Len() == 0 {
		return 0
	}
	minArrived := g.queue[0].ArrivedAt
	for i := 1; i < g.queue.Len(); i++ {
		if g.queue[i].ArrivedAt.Before(minArrived) {
			minArrived = g.queue[i].ArrivedAt
		}
	}
	return time.Since(minArrived).Seconds()
}

func (g *GlobalRequestQueue) RecountPendingTokens() int64 {
	var total int64
	for _, item := range g.queue {
		total += int64(item.InputLen + item.NegCacheHitLen)
	}
	g.globalActualWaitingTokens = total
	return total
}

func (g *GlobalRequestQueue) Push(request *types.RoutingContext, prefixCacheHitLen int, inputLen int) {
	item := &RequestItem{
		NegCacheHitLen: -prefixCacheHitLen,
		ArrivedAt:      time.Now(),
		Req:            request,
		InputLen:       inputLen,
	}
	heap.Push(&g.queue, item)
	actualPrefillLen := inputLen - prefixCacheHitLen
	g.globalActualWaitingTokens += int64(actualPrefillLen)
	g.globalInputWaitingTokens += int64(inputLen)
}

func (g *GlobalRequestQueue) Pop() *RequestItem {
	if g.queue.Len() == 0 {
		return nil
	}
	item := heap.Pop(&g.queue).(*RequestItem)
	actualPrefillLen := item.InputLen + item.NegCacheHitLen
	g.globalActualWaitingTokens -= int64(actualPrefillLen)
	g.globalInputWaitingTokens -= int64(item.InputLen)
	return item
}

func (g *GlobalRequestQueue) Peek() *RequestItem {
	if g.queue.Len() == 0 {
		return nil
	}
	return g.queue[0]
}

func (g *GlobalRequestQueue) IsEmpty() bool {
	return g.queue.Len() == 0
}

func (g *GlobalRequestQueue) DelReq(request *types.RoutingContext) bool {
	for idx, item := range g.queue {
		if item.Req == request {
			actualPrefillLen := item.InputLen + item.NegCacheHitLen
			g.queue = append(g.queue[:idx], g.queue[idx+1:]...)
			heap.Init(&g.queue)
			g.globalActualWaitingTokens -= int64(actualPrefillLen)
			g.globalInputWaitingTokens -= int64(item.InputLen)
			return true
		}
	}
	return false
}

func (g *GlobalRequestQueue) DelReqByRequestID(requestID string) bool {
	for idx, item := range g.queue {
		if item.Req != nil && item.Req.RequestID == requestID {
			actualPrefillLen := item.InputLen + item.NegCacheHitLen
			g.queue = append(g.queue[:idx], g.queue[idx+1:]...)
			heap.Init(&g.queue)
			g.globalActualWaitingTokens -= int64(actualPrefillLen)
			g.globalInputWaitingTokens -= int64(item.InputLen)
			return true
		}
	}
	return false
}

func (g *GlobalRequestQueue) DiscardExpired(discardThreshold float64) []*RequestItem {
	now := time.Now()
	var discarded []*RequestItem
	newQueue := make(PriorityQueue, 0, g.queue.Len())
	for _, item := range g.queue {
		if now.Sub(item.ArrivedAt).Seconds() > discardThreshold {
			discarded = append(discarded, item)
			actualPrefillLen := item.InputLen + item.NegCacheHitLen
			g.globalActualWaitingTokens -= int64(actualPrefillLen)
			g.globalInputWaitingTokens -= int64(item.InputLen)
		} else {
			newQueue = append(newQueue, item)
		}
	}
	g.queue = newQueue
	heap.Init(&g.queue)
	return discarded
}

func (g *GlobalRequestQueue) GetNumGlobalActualWaitingTokens(request *types.RoutingContext, actualPrefillLen int, inputLen int) int64 {
	prefixCacheHitLen := inputLen - actualPrefillLen
	if prefixCacheHitLen < 0 {
		prefixCacheHitLen = 0
	}
	newItem := &RequestItem{
		NegCacheHitLen: -prefixCacheHitLen,
		ArrivedAt:      time.Now(),
		Req:            request,
		InputLen:       inputLen,
	}
	simulated := make(PriorityQueue, 0, g.queue.Len()+1)
	for _, item := range g.queue {
		simulated = append(simulated, item)
	}
	simulated = append(simulated, newItem)
	sort.Slice(simulated, func(i, j int) bool {
		if simulated[i].NegCacheHitLen != simulated[j].NegCacheHitLen {
			return simulated[i].NegCacheHitLen > simulated[j].NegCacheHitLen
		}
		return simulated[i].ArrivedAt.Before(simulated[j].ArrivedAt)
	})
	var totalTokens int64
	for _, item := range simulated {
		if item.Req == request {
			break
		}
		totalTokens += int64(item.InputLen + item.NegCacheHitLen)
	}
	return totalTokens
}

type lazyPrefixTable struct {
	levelBuckets map[int]map[uint64]int
	mu           sync.RWMutex
	hashBase     uint64
}

func newLazyPrefixTable() *lazyPrefixTable {
	return &lazyPrefixTable{
		levelBuckets: make(map[int]map[uint64]int),
		hashBase:     1315423911,
	}
}

func (t *lazyPrefixTable) rollingHash(prefixHashes []uint64, depth int) uint64 {
	var h uint64
	for i := 0; i < depth && i < len(prefixHashes); i++ {
		h = (h*t.hashBase + prefixHashes[i]) & 0xFFFFFFFFFFFFFFFF
	}
	return h
}

func (t *lazyPrefixTable) lookup(prefixHashes []uint64) int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	depth := 1
	maxDepth := len(prefixHashes)
	for depth <= maxDepth {
		h := t.rollingHash(prefixHashes, depth)
		bucket, ok := t.levelBuckets[depth]
		if !ok {
			return depth
		}
		decision, ok := bucket[h]
		if !ok {
			return depth
		}
		depth = decision
	}
	return maxDepth
}

func (t *lazyPrefixTable) markExpand(prefixHashes []uint64, newDepth int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	h := t.rollingHash(prefixHashes, newDepth-1)
	if t.levelBuckets[newDepth-1] == nil {
		t.levelBuckets[newDepth-1] = make(map[uint64]int)
	}
	t.levelBuckets[newDepth-1][h] = newDepth
}

func (t *lazyPrefixTable) markShrink(prefixHashes []uint64, oldDepth int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	h := t.rollingHash(prefixHashes, oldDepth)
	if bucket, ok := t.levelBuckets[oldDepth]; ok {
		delete(bucket, h)
	}
}

type hotPrefixDetector struct {
	window     []string
	counts     map[string]int
	mu         sync.Mutex
	windowSize int
	hotRatio   float64
	minSamples int
}

func newHotPrefixDetector(windowSize int, hotRatio float64, minSamples int) *hotPrefixDetector {
	return &hotPrefixDetector{
		window:     make([]string, 0, windowSize),
		counts:     make(map[string]int),
		windowSize: windowSize,
		hotRatio:   hotRatio,
		minSamples: minSamples,
	}
}

func (d *hotPrefixDetector) observe(prefixKey string) (bool, float64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.window = append(d.window, prefixKey)
	d.counts[prefixKey]++

	if len(d.window) > d.windowSize {
		oldest := d.window[0]
		d.window = d.window[1:]
		d.counts[oldest]--
		if d.counts[oldest] <= 0 {
			delete(d.counts, oldest)
		}
	}

	total := len(d.window)
	if total < d.minSamples {
		return false, 0.0
	}

	cnt := d.counts[prefixKey]
	ratio := float64(cnt) / float64(total)
	return ratio >= d.hotRatio, ratio
}

type lazyExpansionController struct {
	table    *lazyPrefixTable
	detector *hotPrefixDetector
	cnt      int
}

func newLazyExpansionController(table *lazyPrefixTable, detector *hotPrefixDetector) *lazyExpansionController {
	return &lazyExpansionController{
		table:    table,
		detector: detector,
	}
}

func (c *lazyExpansionController) process(prefixHashes []uint64) int {
	c.cnt++
	depth := c.table.lookup(prefixHashes)

	var prefixKeyBuilder strings.Builder
	for i := 0; i < depth && i < len(prefixHashes); i++ {
		prefixKeyBuilder.WriteString(fmt.Sprintf("%x", prefixHashes[i]))
	}
	prefixKey := prefixKeyBuilder.String()

	isHot, ratio := c.detector.observe(prefixKey)

	if isHot && depth < len(prefixHashes) {
		c.table.markExpand(prefixHashes, depth+1)
		if depth+1 > 2 {
			klog.V(5).Infof("DualMap LazyPrefixTable: cnt=%d, prefix=%v, ratio=%.2f%%, expand to %d",
				c.cnt, prefixHashes[:depth+1], ratio*100, depth+1)
		}
	}

	if depth > 1 {
		var pPrefixKeyBuilder strings.Builder
		for i := 0; i < depth-1 && i < len(prefixHashes); i++ {
			pPrefixKeyBuilder.WriteString(fmt.Sprintf("%x", prefixHashes[i]))
		}
		pPrefixKey := pPrefixKeyBuilder.String()
		_, pRatio := c.detector.observe(pPrefixKey)

		if pRatio < 0.02 {
			c.table.markShrink(prefixHashes, depth)
			klog.V(5).Infof("DualMap LazyPrefixTable: shrink, p_prefix=%v, p_ratio=%.2f%%, depth=%d",
				prefixHashes[:depth-1], pRatio*100, depth)
		}
	}

	return depth
}

type DualMapRouter struct {
	cache              cache.Cache
	tokenizer          tokenizer.Tokenizer
	prefixCacheIndexer *prefixcacheindexer.PrefixHashTable

	hashRing1 *HashRing
	hashRing2 *HashRing

	globalQueues   map[string]*GlobalRequestQueue
	globalQueuesMu sync.RWMutex

	requestPodMap   map[string]string
	requestPodMapMu sync.Mutex

	lazyPrefixTable   *lazyPrefixTable
	hotPrefixDetector *hotPrefixDetector
	expansionCtrl     *lazyExpansionController

	podLastPrefillCompletedAt map[string]time.Time
	podLastTTFT               map[string]float64
	podRequestTimestamps      map[string][]time.Time
	podTimestampsMu           sync.Mutex
}

func NewDualMapRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		klog.Error("fail to get cache store in dualmap router")
		return nil, err
	}

	tokenizerObj := newTokenizer()

	lazyTable := newLazyPrefixTable()
	hotDetector := newHotPrefixDetector(200, 0.0612, 20)
	expansionCtrl := newLazyExpansionController(lazyTable, hotDetector)

	router := &DualMapRouter{
		cache:                     c,
		tokenizer:                 tokenizerObj,
		prefixCacheIndexer:        prefixcacheindexer.GetSharedPrefixHashTable(),
		globalQueues:              make(map[string]*GlobalRequestQueue),
		requestPodMap:             make(map[string]string),
		lazyPrefixTable:           lazyTable,
		hotPrefixDetector:         hotDetector,
		expansionCtrl:             expansionCtrl,
		podLastPrefillCompletedAt: make(map[string]time.Time),
		podLastTTFT:               make(map[string]float64),
		podRequestTimestamps:      make(map[string][]time.Time),
	}

	c.RegisterRequestTracker(router)

	klog.Infof("DualMapRouter initialized with balanceType=%s, firstBalanceTTFTThreshold=%.4f, rebalanceThreshold=%.4f, prefillTPOT=%.6f, punishRatio=%.4f",
		dualMapBalanceType,
		dualMapFirstBalanceTTFTThreshold,
		dualMapRebalanceThreshold,
		dualMapPrefillTPOT,
		dualMapRecomputePunishRatio)

	return router, nil
}

func (r *DualMapRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

func (r *DualMapRouter) getTokenizer() tokenizer.Tokenizer {
	return r.tokenizer
}

func (r *DualMapRouter) ensureGlobalQueue(podName string) *GlobalRequestQueue {
	r.globalQueuesMu.RLock()
	q, ok := r.globalQueues[podName]
	r.globalQueuesMu.RUnlock()
	if ok {
		return q
	}

	r.globalQueuesMu.Lock()
	defer r.globalQueuesMu.Unlock()
	q, ok = r.globalQueues[podName]
	if ok {
		return q
	}
	q = NewGlobalRequestQueue(podName)
	r.globalQueues[podName] = q
	return q
}

func (r *DualMapRouter) initHashRingsIfNeeded(pods []*v1.Pod) {
	podNames := make([]string, len(pods))
	for i, pod := range pods {
		podNames[i] = pod.Name
	}

	if r.hashRing1 == nil {
		r.hashRing1 = NewHashRing(xxhashHasher{}, podNames, dualMapHashRingVirtualNodes)
	} else {
		r.hashRing1.UpdateNodes(podNames, dualMapHashRingVirtualNodes)
	}

	if r.hashRing2 == nil {
		r.hashRing2 = NewHashRing(fnvHasher{}, podNames, dualMapHashRingVirtualNodes)
	} else {
		r.hashRing2.UpdateNodes(podNames, dualMapHashRingVirtualNodes)
	}
}

func (r *DualMapRouter) hashFunction1(taskID string) (string, error) {
	return r.hashRing1.GetNode(taskID)
}

func (r *DualMapRouter) hashFunction2(taskID string) (string, error) {
	return r.hashRing2.GetNode(taskID)
}

func (r *DualMapRouter) computeHashSessionID(ctx *types.RoutingContext) (string, int, error) {
	tokenizerToUse := r.getTokenizer()
	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		return "", 0, err
	}

	prefixHashes := r.prefixCacheIndexer.GetPrefixHashes(tokens)

	if len(prefixHashes) == 0 {
		shortestPrefix := fmt.Sprintf("%d", len(tokens))
		klog.V(4).Infof("DualMap prefix: request=%s, tokens=%d, prefixHashes=[] (empty, fallback to token count=%s)",
			ctx.RequestID, len(tokens), shortestPrefix)
		return shortestPrefix, len(tokens), nil
	}

	hashPrefixLen := r.expansionCtrl.process(prefixHashes)

	var shortestPrefixBuilder strings.Builder
	for i := 0; i < hashPrefixLen && i < len(prefixHashes); i++ {
		shortestPrefixBuilder.WriteString(fmt.Sprintf("%x", prefixHashes[i]))
	}
	shortestPrefix := shortestPrefixBuilder.String()

	allHashesStr := formatHashes(prefixHashes)
	usedHashesStr := formatHashes(prefixHashes[:hashPrefixLen])
	klog.V(4).Infof("DualMap prefix: request=%s, tokens=%d, allPrefixHashes=[%s], hashPrefixLen=%d, usedPrefixHashes=[%s], shortestPrefix=%s",
		ctx.RequestID, len(tokens), allHashesStr, hashPrefixLen, usedHashesStr, shortestPrefix)

	return shortestPrefix, len(tokens), nil
}

func formatHashes(hashes []uint64) string {
	parts := make([]string, len(hashes))
	for i, h := range hashes {
		parts[i] = fmt.Sprintf("%x", h)
	}
	return strings.Join(parts, ", ")
}

func (r *DualMapRouter) getPodMetrics(ctx *types.RoutingContext, pod *v1.Pod) map[string]float64 {
	modelName := ctx.Model
	if modelName == "" {
		modelName, _ = constants.ModelNameFromMetadata(pod.Labels, pod.Annotations)
	}

	metricsMap := make(map[string]float64)

	runningReqs := GetPodModelMetricsSimpleValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.NumRequestsRunning)
	metricsMap["running_requests"] = runningReqs

	waitingReqs := GetPodModelMetricsSimpleValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.NumRequestsWaiting)
	metricsMap["waiting_requests"] = waitingReqs

	kvCacheUsage := GetPodModelMetricsSimpleValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.KVCacheUsagePerc)
	metricsMap["kv_cache_usage"] = kvCacheUsage

	drainRate := GetPodModelMetricsSimplePrometheusValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.DrainRate1m)
	metricsMap["drain_rate_1m"] = drainRate

	avgTTFT := GetPodModelMetricsSimplePrometheusValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.AvgTTFT5mPod)
	metricsMap["avg_ttft_5m"] = avgTTFT

	avgTPOT := GetPodModelMetricsSimplePrometheusValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.AvgTPOT5mPod)
	metricsMap["avg_tpot_5m"] = avgTPOT

	avgPromptTokens := GetPodModelMetricsSimplePrometheusValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.AvgPromptToksPerReq)
	metricsMap["avg_prompt_toks_per_req"] = avgPromptTokens

	gpuBusyTime := GetPodModelMetricsSimpleValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.GPUBusyTimeRatio)
	metricsMap["gpu_busy_time_ratio"] = gpuBusyTime

	engineUtil := GetPodModelMetricsSimpleValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.EngineUtilization)
	metricsMap["engine_utilization"] = engineUtil

	return metricsMap
}

func (r *DualMapRouter) getPrefixCacheHitLenForPod(ctx *types.RoutingContext, pod *v1.Pod, readyPods types.PodList) int {
	tokenizerToUse := r.getTokenizer()
	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		return 0
	}

	readyPodsMap := make(map[string]struct{})
	if readyPods != nil {
		for _, p := range readyPods.All() {
			readyPodsMap[p.Name] = struct{}{}
		}
	} else {
		readyPodsMap[pod.Name] = struct{}{}
	}

	matchedPods, _ := r.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, readyPodsMap)
	matchPercent, ok := matchedPods[pod.Name]
	if !ok {
		return 0
	}

	return int(float64(len(tokens)) * float64(matchPercent) / 100.0)
}

func (r *DualMapRouter) getReqActualPrefillTokens(ctx *types.RoutingContext, pod *v1.Pod, readyPods types.PodList) int {
	tokenizerToUse := r.getTokenizer()
	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		return 0
	}

	prefixCacheHitLen := r.getPrefixCacheHitLenForPod(ctx, pod, readyPods)
	actualPrefillTokens := len(tokens) - prefixCacheHitLen
	if actualPrefillTokens < 0 {
		actualPrefillTokens = 0
	}
	return actualPrefillTokens
}

func (r *DualMapRouter) getNumActualPendingTokens(pod *v1.Pod) int64 {
	modelName := ""
	pendingReqs := GetPodModelMetricsSimpleValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.NumRequestsWaiting)
	avgPromptTokens := GetPodModelMetricsSimplePrometheusValue(r.cache, pod.Name, pod.Namespace, modelName, metrics.AvgPromptToksPerReq)
	return int64(math.Max(pendingReqs*avgPromptTokens, 0))
}

func (r *DualMapRouter) getPendingInputTokens(pod *v1.Pod) int64 {
	q := r.ensureGlobalQueue(pod.Name)
	globalInputWaiting := q.GetGlobalInputWaitingTokens()

	pendingInputTokens := GetPodModelMetricsSimpleValue(r.cache, pod.Name, pod.Namespace, "", metrics.RequestPromptTokens)
	return globalInputWaiting + int64(pendingInputTokens)
}

func (r *DualMapRouter) getPodRequestRate(pod *v1.Pod) float64 {
	r.podTimestampsMu.Lock()
	defer r.podTimestampsMu.Unlock()

	timestamps, ok := r.podRequestTimestamps[pod.Name]
	if !ok || len(timestamps) == 0 {
		return 0.0
	}

	now := time.Now()
	oldest := timestamps[0]
	windowDuration := now.Sub(oldest).Seconds()
	if windowDuration <= 0 {
		return 0.0
	}

	return float64(len(timestamps)) / windowDuration
}

func (r *DualMapRouter) recordPodRequestTimestamp(pod *v1.Pod) {
	r.podTimestampsMu.Lock()
	defer r.podTimestampsMu.Unlock()

	now := time.Now()
	timestamps, ok := r.podRequestTimestamps[pod.Name]
	if !ok {
		timestamps = make([]time.Time, 0)
	}
	timestamps = append(timestamps, now)

	cutoff := now.Add(-180 * time.Second)
	i := 0
	for i < len(timestamps) && timestamps[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		timestamps = timestamps[i:]
	}
	r.podRequestTimestamps[pod.Name] = timestamps
}

func (r *DualMapRouter) getLoadStates(pod *v1.Pod) (currentBudget float64, lastPrefillCompletedAt time.Time, lastTTFT float64, numPendingRequests int, qps float64) {
	r.podTimestampsMu.Lock()
	lastPrefillCompletedAt = r.podLastPrefillCompletedAt[pod.Name]
	lastTTFT = r.podLastTTFT[pod.Name]
	r.podTimestampsMu.Unlock()

	if lastPrefillCompletedAt.IsZero() {
		lastPrefillCompletedAt = time.Now()
	}

	pendingReqs := GetPodModelMetricsSimpleValue(r.cache, pod.Name, pod.Namespace, "", metrics.NumRequestsWaiting)
	numPendingRequests = int(pendingReqs)

	qps = r.getPodRequestRate(pod)

	return currentBudget, lastPrefillCompletedAt, lastTTFT, numPendingRequests, qps
}

func (r *DualMapRouter) selectReplicasBasedOnMetrics(
	metricDict map[string]float64,
	runningReqBlocksCntList map[string]float64,
	chosenReplicaIDs []string,
	seed int64,
	primaryIsMax bool,
) (string, string) {
	rng := rand.New(rand.NewSource(seed))

	replicaID1 := chosenReplicaIDs[0]
	replicaID2 := chosenReplicaIDs[1]

	if metricDict[replicaID1] != metricDict[replicaID2] {
		if primaryIsMax {
			if metricDict[replicaID1] > metricDict[replicaID2] {
				return replicaID1, replicaID2
			}
			return replicaID2, replicaID1
		} else {
			if metricDict[replicaID1] < metricDict[replicaID2] {
				return replicaID1, replicaID2
			}
			return replicaID2, replicaID1
		}
	}

	if runningReqBlocksCntList[replicaID1] != runningReqBlocksCntList[replicaID2] {
		if runningReqBlocksCntList[replicaID1] < runningReqBlocksCntList[replicaID2] {
			return replicaID1, replicaID2
		}
		return replicaID2, replicaID1
	}

	idx := rng.Intn(2)
	return chosenReplicaIDs[idx], chosenReplicaIDs[1-idx]
}

func (r *DualMapRouter) addRequestToBestGlobalQueue(
	ctx *types.RoutingContext,
	readyPodList types.PodList,
) (string, string, error) {
	pods := readyPodList.All()
	if len(pods) == 0 {
		return "", "", fmt.Errorf("no pods available")
	}

	r.initHashRingsIfNeeded(pods)

	shortestPrefix, inputLen, err := r.computeHashSessionID(ctx)
	if err != nil {
		return "", "", err
	}

	replicaID1, err := r.hashFunction1(shortestPrefix)
	if err != nil {
		return "", "", err
	}
	replicaID2, err := r.hashFunction2(shortestPrefix)
	if err != nil {
		return "", "", err
	}

	podNames := make([]string, len(pods))
	for i, p := range pods {
		podNames[i] = p.Name
	}

	rawReplicaID1 := replicaID1
	rawReplicaID2 := replicaID2
	hashCollision := false
	if replicaID1 == replicaID2 {
		hashCollision = true
		idx := indexOf(podNames, replicaID1)
		if idx >= 0 {
			replicaID2 = podNames[(idx+1)%len(podNames)]
		} else {
			replicaID2 = podNames[0]
		}
		klog.V(4).Infof("DualMap hash collision: request=%s, shortestPrefix=%s, xxhash1=%s == fnv2=%s, fallback to next pod: %s -> %s",
			ctx.RequestID, shortestPrefix, rawReplicaID1, rawReplicaID2, replicaID1, replicaID2)
	}

	klog.V(4).Infof("DualMap hash rings: request=%s, shortestPrefix=%s, xxhash1(raw)=%s, fnv2(raw)=%s, final=(%s, %s), collision=%v, allPods=[%s]",
		ctx.RequestID, shortestPrefix, rawReplicaID1, rawReplicaID2, replicaID1, replicaID2, hashCollision, strings.Join(podNames, ", "))

	chosenReplicaIDs := []string{replicaID1, replicaID2}

	ttftList := make(map[string]float64)
	reqQPSList := make(map[string]float64)
	recomputeLatencyList := make(map[string]float64)
	numGlobalActualWaitingTokensList := make(map[string]int64)
	numReplicaActualPendingTokensList := make(map[string]int64)
	numReqActualPrefillTokensList := make(map[string]int)
	prefixCacheHitLenList := make(map[string]int)
	costList := make(map[string]float64)
	pendingInputTokensList := make(map[string]int64)
	runningReqBlocksCntList := make(map[string]float64)
	numVirtualPendingTokensList := make(map[string]int64)
	maxWaitingDelayList := make(map[string]float64)

	podMap := make(map[string]*v1.Pod)
	for _, pod := range pods {
		podMap[pod.Name] = pod
	}

	for _, replicaID := range chosenReplicaIDs {
		pod, ok := podMap[replicaID]
		if !ok {
			klog.Warningf("DualMap: replica %s not found in pod list, skipping", replicaID)
			continue
		}

		q := r.ensureGlobalQueue(replicaID)
		numGlobalActualWaitingTokensList[replicaID] = q.GetGlobalActualWaitingTokens()
		numReplicaActualPendingTokensList[replicaID] = r.getNumActualPendingTokens(pod)
		numReqActualPrefillTokensList[replicaID] = r.getReqActualPrefillTokens(ctx, pod, readyPodList)

		pendingInputTokensList[replicaID] = r.getPendingInputTokens(pod)

		prefixCacheHitLen := r.getPrefixCacheHitLenForPod(ctx, pod, readyPodList)
		prefixCacheHitLenList[replicaID] = prefixCacheHitLen

		virtualPending := numGlobalActualWaitingTokensList[replicaID] +
			numReplicaActualPendingTokensList[replicaID] +
			int64(numReqActualPrefillTokensList[replicaID])
		numVirtualPendingTokensList[replicaID] = virtualPending
		ttftList[replicaID] = float64(virtualPending) * dualMapPrefillTPOT

		_, _, _, _, qps := r.getLoadStates(pod)
		reqQPSList[replicaID] = qps

		recomputeLatencyList[replicaID] = float64(numReqActualPrefillTokensList[replicaID]) * dualMapPrefillTPOT

		costList[replicaID] = ttftList[replicaID] +
			dualMapRecomputePunishRatio*ttftList[replicaID]*reqQPSList[replicaID]*recomputeLatencyList[replicaID]

		maxWaitingDelayList[replicaID] = q.GetMaxWaitingDelay()

		metricsMap := r.getPodMetrics(ctx, pod)
		runningReqBlocksCntList[replicaID] = metricsMap["kv_cache_usage"]
	}

	var primaryReplicaID, secondReplicaID string

	switch dualMapBalanceType {
	case DualMapLeastLoaded:
		primaryReplicaID, secondReplicaID = r.selectReplicasBasedOnMetrics(
			map[string]float64{
				chosenReplicaIDs[0]: float64(pendingInputTokensList[chosenReplicaIDs[0]]),
				chosenReplicaIDs[1]: float64(pendingInputTokensList[chosenReplicaIDs[1]]),
			},
			runningReqBlocksCntList,
			chosenReplicaIDs,
			42,
			false,
		)

	case DualMapCacheAffinity:
		primaryReplicaID, secondReplicaID = r.selectReplicasBasedOnMetrics(
			map[string]float64{
				chosenReplicaIDs[0]: float64(prefixCacheHitLenList[chosenReplicaIDs[0]]),
				chosenReplicaIDs[1]: float64(prefixCacheHitLenList[chosenReplicaIDs[1]]),
			},
			runningReqBlocksCntList,
			chosenReplicaIDs,
			42,
			true,
		)

	case DualMapMinTTFT:
		primaryReplicaID, secondReplicaID = r.selectReplicasBasedOnMetrics(
			map[string]float64{
				chosenReplicaIDs[0]: ttftList[chosenReplicaIDs[0]],
				chosenReplicaIDs[1]: ttftList[chosenReplicaIDs[1]],
			},
			runningReqBlocksCntList,
			chosenReplicaIDs,
			42,
			false,
		)

	case DualMapDefault, DualMapNoRebalance, DualMapTTFTSLO, DualMapTTFTAvg:
		isReplicaOverloaded := make(map[string]bool)
		for _, replicaID := range chosenReplicaIDs {
			isReplicaOverloaded[replicaID] = false
			if float64(numVirtualPendingTokensList[replicaID]) > dualMapFirstBalanceTTFTThreshold {
				isReplicaOverloaded[replicaID] = true
			}
		}

		cacheHitHighRepID, cacheHitLowRepID := r.selectReplicasBasedOnMetrics(
			map[string]float64{
				chosenReplicaIDs[0]: float64(prefixCacheHitLenList[chosenReplicaIDs[0]]),
				chosenReplicaIDs[1]: float64(prefixCacheHitLenList[chosenReplicaIDs[1]]),
			},
			runningReqBlocksCntList,
			chosenReplicaIDs,
			42,
			true,
		)

		if isReplicaOverloaded[cacheHitHighRepID] {
			primaryReplicaID, secondReplicaID = r.selectReplicasBasedOnMetrics(
				map[string]float64{
					chosenReplicaIDs[0]: ttftList[chosenReplicaIDs[0]],
					chosenReplicaIDs[1]: ttftList[chosenReplicaIDs[1]],
				},
				runningReqBlocksCntList,
				chosenReplicaIDs,
				42,
				false,
			)
		} else {
			primaryReplicaID = cacheHitHighRepID
			secondReplicaID = cacheHitLowRepID
		}

	case DualMapNBCost1, DualMapRBCost1, DualMapRBCost1Aggresive, DualMapRBCost1Avg:
		if len(costList) > 0 {
			primaryReplicaID, secondReplicaID = r.selectReplicasBasedOnMetrics(
				costList,
				runningReqBlocksCntList,
				chosenReplicaIDs,
				42,
				false,
			)
		} else {
			return "", "", fmt.Errorf("cost_list is nil")
		}

	default:
		klog.Warningf("DualMap: unknown balance type '%s', falling back to default", dualMapBalanceType)
		isReplicaOverloaded := make(map[string]bool)
		for _, replicaID := range chosenReplicaIDs {
			isReplicaOverloaded[replicaID] = false
			if float64(numVirtualPendingTokensList[replicaID]) > dualMapFirstBalanceTTFTThreshold {
				isReplicaOverloaded[replicaID] = true
			}
		}

		cacheHitHighRepID, cacheHitLowRepID := r.selectReplicasBasedOnMetrics(
			map[string]float64{
				chosenReplicaIDs[0]: float64(prefixCacheHitLenList[chosenReplicaIDs[0]]),
				chosenReplicaIDs[1]: float64(prefixCacheHitLenList[chosenReplicaIDs[1]]),
			},
			runningReqBlocksCntList,
			chosenReplicaIDs,
			42,
			true,
		)

		if isReplicaOverloaded[cacheHitHighRepID] {
			primaryReplicaID, secondReplicaID = r.selectReplicasBasedOnMetrics(
				map[string]float64{
					chosenReplicaIDs[0]: ttftList[chosenReplicaIDs[0]],
					chosenReplicaIDs[1]: ttftList[chosenReplicaIDs[1]],
				},
				runningReqBlocksCntList,
				chosenReplicaIDs,
				42,
				false,
			)
		} else {
			primaryReplicaID = cacheHitHighRepID
			secondReplicaID = cacheHitLowRepID
		}
	}

	if primaryReplicaID == "" || secondReplicaID == "" {
		return "", "", fmt.Errorf("failed to select primary and secondary replicas")
	}

	q := r.ensureGlobalQueue(primaryReplicaID)
	q.Push(ctx, prefixCacheHitLenList[primaryReplicaID], inputLen)

	r.requestPodMapMu.Lock()
	r.requestPodMap[ctx.RequestID] = primaryReplicaID
	r.requestPodMapMu.Unlock()

	klog.V(4).Infof("DualMap: request=%s, primary=%s, secondary=%s, prefixCacheHitLen=%d, ttftPrimary=%.4f, ttftSecondary=%.4f",
		ctx.RequestID,
		primaryReplicaID,
		secondReplicaID,
		prefixCacheHitLenList[primaryReplicaID],
		ttftList[primaryReplicaID],
		ttftList[secondReplicaID])

	if dualMapBalanceType == DualMapDefault || dualMapBalanceType == DualMapTTFTSLO || dualMapBalanceType == DualMapRBCost1Aggresive {
		isOverloaded := make(map[string]bool)
		numPendingTokensList := make(map[string]int64)

		for _, replicaID := range chosenReplicaIDs {
			isOverloaded[replicaID] = false
			if replicaID == primaryReplicaID {
				numPendingTokensList[replicaID] = numVirtualPendingTokensList[replicaID]
			} else {
				numPendingTokensList[replicaID] = numVirtualPendingTokensList[replicaID] -
					int64(numReqActualPrefillTokensList[replicaID])
			}
			if numPendingTokensList[replicaID] > int64(math.Max(dualMapRebalanceThreshold, 1)) ||
				maxWaitingDelayList[replicaID] >= dualMapRebalanceWaitingLatencyThd {
				isOverloaded[replicaID] = true
			}
		}

		if isOverloaded[primaryReplicaID] && isOverloaded[secondReplicaID] {
			for _, repID := range []string{primaryReplicaID, secondReplicaID} {
				numMigrateTokens := numPendingTokensList[repID] - int64(math.Max(dualMapRebalanceThreshold, 1))
				if numMigrateTokens > 0 {
					r.rebalanceReplicaGlobalWaitingReqs(repID, numMigrateTokens, chosenReplicaIDs, podMap,
						numGlobalActualWaitingTokensList, numReplicaActualPendingTokensList,
						numReqActualPrefillTokensList, maxWaitingDelayList, reqQPSList, ctx, readyPodList)
				}
			}
		}
	}

	return primaryReplicaID, secondReplicaID, nil
}

type rebalanceCostEntry struct {
	cost                   float64
	sourceActualPrefillLen int
	targetReplicaID        string
	targetActualPrefillLen int
}

func (r *DualMapRouter) rebalanceReplicaGlobalWaitingReqs(
	sourceReplicaID string,
	numTargetMigratePrefillTokens int64,
	chosenReplicaIDs []string,
	podMap map[string]*v1.Pod,
	numGlobalActualWaitingTokensList map[string]int64,
	numReplicaActualPendingTokensList map[string]int64,
	numReqActualPrefillTokensList map[string]int,
	maxWaitingDelayList map[string]float64,
	reqQPSList map[string]float64,
	ctx *types.RoutingContext,
	readyPodList types.PodList,
) {
	if dualMapBalanceType != DualMapDefault {
		return
	}

	sourceQ := r.ensureGlobalQueue(sourceReplicaID)
	if sourceQ.Len() <= 1 {
		return
	}

	enableMigrateToNeighborReplica := (dualMapBalanceType == DualMapDefault)

	podNames := make([]string, 0, len(podMap))
	for name := range podMap {
		podNames = append(podNames, name)
	}
	numReplicas := len(podNames)

	podIndexMap := make(map[string]int)
	for i, name := range podNames {
		podIndexMap[name] = i
	}

	curNumMigratedPrefillTokens := int64(0)

	for {
		requestsMigrateCost := make(map[int]rebalanceCostEntry)
		sourceGlobalWaitingRequests := make([]*RequestItem, 0)

		r.globalQueuesMu.RLock()
		sourceQueue := sourceQ.queue
		for _, item := range sourceQueue {
			sourceGlobalWaitingRequests = append(sourceGlobalWaitingRequests, item)
		}
		r.globalQueuesMu.RUnlock()

		for reqIndex := 0; reqIndex < len(sourceGlobalWaitingRequests); reqIndex++ {
			curRequest := sourceGlobalWaitingRequests[reqIndex]
			if curRequest == nil || curRequest.Req == nil {
				continue
			}

			sourcePod, ok := podMap[sourceReplicaID]
			if !ok {
				continue
			}
			sourceActualPrefillLen := r.getReqActualPrefillTokens(curRequest.Req, sourcePod, readyPodList)

			sourceNumGlobalWaitingPrefillTokens := sourceQ.GetNumGlobalActualWaitingTokens(
				curRequest.Req,
				sourceActualPrefillLen,
				curRequest.InputLen,
			)

			sourceTTFT := float64(numReplicaActualPendingTokensList[sourceReplicaID]+
				sourceNumGlobalWaitingPrefillTokens+int64(sourceActualPrefillLen)) * dualMapPrefillTPOT

			sourceRecomputeLatency := float64(sourceActualPrefillLen) * dualMapPrefillTPOT

			sourceNumDelayRequests := math.Max(
				reqQPSList[sourceReplicaID]*sourceTTFT,
				float64(len(sourceGlobalWaitingRequests)-(reqIndex+1)),
			)

			sourceCost := sourceTTFT + dualMapRecomputePunishRatio*sourceNumDelayRequests*sourceRecomputeLatency

			shortestPrefix, _, _ := r.computeHashSessionID(curRequest.Req)
			replicaID1, _ := r.hashFunction1(shortestPrefix)
			replicaID2, _ := r.hashFunction2(shortestPrefix)
			if replicaID1 == replicaID2 {
				idx := indexOf(podNames, replicaID1)
				if idx >= 0 {
					replicaID2 = podNames[(idx+1)%len(podNames)]
				} else {
					replicaID2 = podNames[0]
				}
			}

			targetReplicaID := replicaID2
			if sourceReplicaID == replicaID1 {
				targetReplicaID = replicaID2
			} else if sourceReplicaID == replicaID2 {
				targetReplicaID = replicaID1
			}

			if targetReplicaID == "" {
				continue
			}

			targetPod, ok := podMap[targetReplicaID]
			if !ok {
				continue
			}
			targetActualPrefillLen := r.getReqActualPrefillTokens(curRequest.Req, targetPod, readyPodList)

			targetQ := r.ensureGlobalQueue(targetReplicaID)
			targetNumGlobalActualWaitingTokens := targetQ.GetNumGlobalActualWaitingTokens(
				curRequest.Req,
				targetActualPrefillLen,
				curRequest.InputLen,
			)

			targetVirtualPendingTokens := targetNumGlobalActualWaitingTokens +
				numReplicaActualPendingTokensList[targetReplicaID] +
				int64(targetActualPrefillLen)

			if sourceReplicaID == targetReplicaID ||
				targetVirtualPendingTokens > int64(math.Max(dualMapRebalanceThreshold, 1)) ||
				maxWaitingDelayList[targetReplicaID] >= dualMapRebalanceWaitingLatencyThd {

				if !enableMigrateToNeighborReplica {
					continue
				}

				sourceIdx := podIndexMap[sourceReplicaID]
				targetIdx := podIndexMap[targetReplicaID]

				tmpTargetIdx := (sourceIdx + 1) % numReplicas
				if tmpTargetIdx == sourceIdx || tmpTargetIdx == targetIdx {
					tmpTargetIdx = (targetIdx + 1) % numReplicas
				}
				if tmpTargetIdx == sourceIdx || tmpTargetIdx == targetIdx {
					continue
				}

				tmpTargetReplicaID := podNames[tmpTargetIdx]

				tmpTargetPod, ok := podMap[tmpTargetReplicaID]
				if !ok {
					continue
				}
				tmpTargetActualPrefillLen := r.getReqActualPrefillTokens(curRequest.Req, tmpTargetPod, readyPodList)
				tmpTargetQ := r.ensureGlobalQueue(tmpTargetReplicaID)
				tmpTargetNumGlobalActualWaitingTokens := tmpTargetQ.GetNumGlobalActualWaitingTokens(
					curRequest.Req,
					tmpTargetActualPrefillLen,
					curRequest.InputLen,
				)
				tmpTargetVirtualPendingTokens := tmpTargetNumGlobalActualWaitingTokens +
					numReplicaActualPendingTokensList[tmpTargetReplicaID] +
					int64(tmpTargetActualPrefillLen)

				if tmpTargetVirtualPendingTokens > int64(math.Max(dualMapRebalanceThreshold, 1)) ||
					maxWaitingDelayList[tmpTargetReplicaID] >= dualMapRebalanceWaitingLatencyThd {
					continue
				}

				targetReplicaID = tmpTargetReplicaID
				targetActualPrefillLen = tmpTargetActualPrefillLen
				targetQ = tmpTargetQ
			}

			targetPod, ok = podMap[targetReplicaID]
			if !ok {
				continue
			}
			targetActualPrefillLen = r.getReqActualPrefillTokens(curRequest.Req, targetPod, readyPodList)

			targetNumGlobalActualWaitingTokens = targetQ.GetNumGlobalActualWaitingTokens(
				curRequest.Req,
				targetActualPrefillLen,
				curRequest.InputLen,
			)

			targetVirtualPendingTokens = targetNumGlobalActualWaitingTokens +
				numReplicaActualPendingTokensList[targetReplicaID] +
				int64(targetActualPrefillLen)

			targetTTFT := float64(targetVirtualPendingTokens) * dualMapPrefillTPOT
			targetRecomputeLatency := float64(targetActualPrefillLen) * dualMapPrefillTPOT
			targetNumDelayRequests := reqQPSList[targetReplicaID] * targetTTFT
			targetCost := targetTTFT + dualMapRecomputePunishRatio*targetNumDelayRequests*targetRecomputeLatency

			var cost float64
			switch dualMapBalanceType {
			case DualMapRBCost1, DualMapRBCost1Aggresive, DualMapRBCost1Avg:
				cost = targetCost - sourceCost
			default:
				cost = targetTTFT - sourceTTFT
			}

			requestsMigrateCost[reqIndex] = rebalanceCostEntry{
				cost:                   cost,
				sourceActualPrefillLen: sourceActualPrefillLen,
				targetReplicaID:        targetReplicaID,
				targetActualPrefillLen: targetActualPrefillLen,
			}
		}

		if len(requestsMigrateCost) == 0 {
			break
		}

		type sortEntry struct {
			reqIndex               int
			cost                   float64
			sourceActualPrefillLen int
			targetReplicaID        string
			targetActualPrefillLen int
		}

		var sortedEntries []sortEntry
		for reqIndex, entry := range requestsMigrateCost {
			sortedEntries = append(sortedEntries, sortEntry{
				reqIndex:               reqIndex,
				cost:                   entry.cost,
				sourceActualPrefillLen: entry.sourceActualPrefillLen,
				targetReplicaID:        entry.targetReplicaID,
				targetActualPrefillLen: entry.targetActualPrefillLen,
			})
		}

		sort.Slice(sortedEntries, func(i, j int) bool {
			return sortedEntries[i].cost < sortedEntries[j].cost
		})

		if len(sortedEntries) == 0 {
			break
		}

		topEntry := sortedEntries[0]
		if topEntry.cost >= 0 {
			klog.V(4).Infof("DualMap rebalance: cost=%.4f >= 0, stopping", topEntry.cost)
			break
		}

		if numTargetMigratePrefillTokens > 0 {
			if curNumMigratedPrefillTokens >= numTargetMigratePrefillTokens {
				klog.V(4).Infof("DualMap rebalance: migrated %d >= target %d, stopping",
					curNumMigratedPrefillTokens, numTargetMigratePrefillTokens)
				break
			}
		}

		migrateRequest := sourceGlobalWaitingRequests[topEntry.reqIndex]
		if migrateRequest == nil || migrateRequest.Req == nil {
			break
		}

		targetQ := r.ensureGlobalQueue(topEntry.targetReplicaID)
		if targetQ == nil {
			break
		}

		sourceQ.DelReq(migrateRequest.Req)

		targetPrefixCacheHitLen := migrateRequest.InputLen - topEntry.targetActualPrefillLen
		if targetPrefixCacheHitLen < 0 {
			targetPrefixCacheHitLen = 0
		}
		targetQ.Push(migrateRequest.Req, targetPrefixCacheHitLen, migrateRequest.InputLen)

		r.requestPodMapMu.Lock()
		r.requestPodMap[migrateRequest.Req.RequestID] = topEntry.targetReplicaID
		r.requestPodMapMu.Unlock()

		numGlobalActualWaitingTokensList[topEntry.targetReplicaID] += int64(topEntry.targetActualPrefillLen)

		curNumMigratedPrefillTokens += int64(topEntry.sourceActualPrefillLen)

		klog.V(4).Infof("DualMap rebalance: migrated request=%s from %s to %s, cost=%.4f",
			migrateRequest.Req.RequestID, sourceReplicaID, topEntry.targetReplicaID, topEntry.cost)
	}
}

func (r *DualMapRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	pods := readyPodList.All()
	if len(pods) == 0 {
		return "", fmt.Errorf("no pods available for dualmap routing")
	}

	if len(pods) == 1 {
		ctx.SetTargetPod(pods[0])
		if err := r.PostRouteUpdate(ctx, readyPodList, pods[0]); err != nil {
			klog.Warningf("DualMap: post-route update failed: %v", err)
		}
		return ctx.TargetAddress(), nil
	}

	primaryReplicaID, _, err := r.addRequestToBestGlobalQueue(ctx, readyPodList)
	if err != nil {
		klog.Errorf("DualMap: addRequestToBestGlobalQueue failed: %v", err)
		return "", err
	}

	targetPod := r.popSchedulableFromQueue(primaryReplicaID, ctx)
	if targetPod == nil {
		podMap := make(map[string]*v1.Pod)
		for _, pod := range pods {
			podMap[pod.Name] = pod
		}
		targetPod = podMap[primaryReplicaID]
		if targetPod == nil {
			return "", fmt.Errorf("dualmap: failed to find target pod %s", primaryReplicaID)
		}
	}

	r.recordPodRequestTimestamp(targetPod)

	ctx.SetTargetPod(targetPod)

	if err := r.PostRouteUpdate(ctx, readyPodList, targetPod); err != nil {
		klog.Warningf("DualMap: post-route update failed: %v", err)
	}

	return ctx.TargetAddress(), nil
}

func (r *DualMapRouter) popSchedulableFromQueue(podName string, ctx *types.RoutingContext) *v1.Pod {
	q := r.ensureGlobalQueue(podName)
	if q.Len() == 0 {
		return nil
	}

	item := q.Pop()
	if item == nil || item.Req == nil {
		return nil
	}

	r.requestPodMapMu.Lock()
	delete(r.requestPodMap, item.Req.RequestID)
	r.requestPodMapMu.Unlock()

	pod, err := r.cache.GetPod(podName, "")
	if err != nil {
		return nil
	}

	return pod
}

func (r *DualMapRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	n := len(pods)
	if n == 0 {
		return nil, nil, fmt.Errorf("empty pod list")
	}

	scores := make([]float64, n)
	scored := make([]bool, n)

	r.initHashRingsIfNeeded(pods)

	shortestPrefix, _, err := r.computeHashSessionID(ctx)
	if err != nil {
		return nil, nil, err
	}

	for i, pod := range pods {
		podName := pod.Name

		prefixCacheHitLen := r.getPrefixCacheHitLenForPod(ctx, pod, readyPodList)
		prefixHitRatio := 0.0
		if len(shortestPrefix) > 0 {
			prefixHitRatio = float64(prefixCacheHitLen)
		}

		metricsMap := r.getPodMetrics(ctx, pod)
		loadScore := metricsMap["waiting_requests"] + metricsMap["running_requests"]

		var hashScore float64
		replicaID1, err1 := r.hashFunction1(shortestPrefix)
		replicaID2, err2 := r.hashFunction2(shortestPrefix)
		if err1 == nil && replicaID1 == podName {
			hashScore = 0.0
		} else if err2 == nil && replicaID2 == podName {
			hashScore = 0.5
		} else {
			hashScore = 1.0
		}

		scores[i] = hashScore + loadScore - prefixHitRatio
		scored[i] = true

		klog.V(5).Infof("DualMap score: pod=%s, hashScore=%.4f, loadScore=%.4f, prefixHitRatio=%.4f, finalScore=%.4f",
			podName, hashScore, loadScore, prefixHitRatio, scores[i])
	}

	return scores, scored, nil
}

func (r *DualMapRouter) PostRouteUpdate(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) error {
	if targetPod == nil {
		return nil
	}

	tokenizerToUse := r.getTokenizer()
	tokens, err := tokenizerToUse.TokenizeInputText(ctx.Message)
	if err != nil {
		return err
	}

	prefixHashes := r.prefixCacheIndexer.GetPrefixHashes(tokens)
	if len(prefixHashes) > 0 {
		r.prefixCacheIndexer.AddPrefix(prefixHashes, ctx.Model, targetPod.Name)
	}

	now := time.Now()
	r.podLastPrefillCompletedAt[targetPod.Name] = now

	return nil
}

func (r *DualMapRouter) SubscribedMetrics() []string {
	return []string{
		metrics.NumRequestsRunning,
		metrics.NumRequestsWaiting,
		metrics.KVCacheUsagePerc,
		metrics.DrainRate1m,
		metrics.AvgTTFT5mPod,
		metrics.AvgTPOT5mPod,
		metrics.AvgPromptToksPerReq,
		metrics.GPUBusyTimeRatio,
		metrics.EngineUtilization,
		metrics.RequestPromptTokens,
	}
}

func (r *DualMapRouter) AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) (traceTerm int64) {
	if ctx == nil {
		return 0
	}
	r.requestPodMapMu.Lock()
	r.requestPodMap[requestID] = ctx.TargetPod().Name
	r.requestPodMapMu.Unlock()
	return 0
}

func (r *DualMapRouter) DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64) {
	r.cleanupRequestFromQueue(requestID)
}

func (r *DualMapRouter) DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, inputTokens, outputTokens int64, traceTerm int64) {
	r.cleanupRequestFromQueue(requestID)
}

func (r *DualMapRouter) cleanupRequestFromQueue(requestID string) {
	r.requestPodMapMu.Lock()
	podName, ok := r.requestPodMap[requestID]
	if ok {
		delete(r.requestPodMap, requestID)
	}
	r.requestPodMapMu.Unlock()

	if !ok {
		return
	}

	q := r.ensureGlobalQueue(podName)
	if q.DelReqByRequestID(requestID) {
		klog.V(4).Infof("DualMap: cleaned up request %s from queue %s", requestID, podName)
	}
}

var (
	_ types.Router           = (*DualMapRouter)(nil)
	_ types.PodScorer        = (*DualMapRouter)(nil)
	_ types.PostRouteUpdater = (*DualMapRouter)(nil)
	_ cache.RequestTracker   = (*DualMapRouter)(nil)
)

func indexOf(slice []string, item string) int {
	for i, s := range slice {
		if s == item {
			return i
		}
	}
	return -1
}
