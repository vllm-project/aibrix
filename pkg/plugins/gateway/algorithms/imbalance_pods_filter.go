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
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type imbalancePodsFilterKeyType struct{}

var imbalancePodsFilterReqAddedKey = imbalancePodsFilterKeyType{}

type imbalancePodsFilterReqDelKeyType struct{}

var imbalancePodsFilterReqDelKey = imbalancePodsFilterReqDelKeyType{}

// MetricsProvider provides pod-level metrics
type MetricsProvider interface {
	GetMetricValueByPod(podName, namespace, metricName string) (metrics.MetricValue, error)
}

// filter route candicates if running requests are imbalance
type imbalancePodsFilter interface {
	// Check whether the realtime running requests are imbalanced among the pods
	// ctx: context for cancellation and timeout propagation
	// Returns:
	//     filteredPods: if imbalanced, return the pod with least requests; otherwise, return all the pods
	//     imbalanced: whether the pods are imbalanced
	FilterPodsWithPort(ctx context.Context, readyPodList types.PodList) ([]podServerKey, bool)
	// FilterPods filters the pods that are imbalanced in the given list, this is the version without port.
	// Currently, prefix-cache router does not support multi-port, so all the requests are route to the first dp rank in the pod
	// In the future, when prefix-cache router supports multi-port, this function will be deprecated and use FilterPodsWithPort instead
	// ctx: context for cancellation and timeout propagation
	// Returns:
	//     filteredPods: if imbalanced, return the pod with least requests; otherwise, return all the pods
	//     imbalanced: whether the pods are imbalanced
	FilterPods(ctx context.Context, readyPodList types.PodList) ([]*v1.Pod, bool)
}

// localImbalancePodsFilter is a local implementation of imbalancePodsFilter
type localImbalancePodsFilter struct {
	metricsProvider    MetricsProvider
	imbalanceThreshold int64
}

// NewLocalImbalancePodsFilter creates a new local imbalance pods filter
func NewLocalImbalancePodsFilter(provider MetricsProvider) *localImbalancePodsFilter {
	return &localImbalancePodsFilter{
		metricsProvider:    provider,
		imbalanceThreshold: defaultImbalanceThreshold,
	}
}

// getRequestCountsWithPort returns running request count for each pod-port combination
func (l *localImbalancePodsFilter) getRequestCountsWithPort(readyPods []*v1.Pod, portsMap map[string][]int) map[string]int64 {
	podRequestCount := make(map[string]int64)
	for _, pod := range readyPods {
		podPorts, exists := portsMap[pod.Name]

		// Single port or no explicit ports
		if !exists || len(podPorts) == 0 {
			runningReq, err := l.metricsProvider.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
			if err != nil {
				runningReq = &metrics.SimpleMetricValue{Value: 0}
			}
			podRequestCount[pod.Name] = int64(runningReq.GetSimpleValue())
			continue
		}

		// Multi-port pod
		for _, port := range podPorts {
			var metricName string
			var keyName string

			if len(podPorts) == 1 {
				metricName = metrics.RealtimeNumRequestsRunning
				keyName = pod.Name
			} else {
				metricName = fmt.Sprintf("%s/%d", metrics.RealtimeNumRequestsRunning, port)
				keyName = fmt.Sprintf("%s_%d", pod.Name, port)
			}

			var count int64
			if val, err := l.metricsProvider.GetMetricValueByPod(pod.Name, pod.Namespace, metricName); err == nil && val != nil {
				count = int64(val.GetSimpleValue())
			}
			podRequestCount[keyName] = count
		}
	}

	return podRequestCount
}

// FilterPodsWithPort implements [imbalancePodsFilter].
func (l *localImbalancePodsFilter) FilterPodsWithPort(ctx context.Context, readyPodList types.PodList) ([]podServerKey, bool) {
	if len(readyPodList.All()) == 0 {
		return []podServerKey{}, false
	}

	// Build all candidates
	candidates := l.buildCandidates(readyPodList)
	if len(candidates) == 0 {
		return []podServerKey{}, false
	}

	// If only one candidate, no imbalance possible
	if len(candidates) == 1 {
		return candidates, false
	}

	// Get request counts for all pod-port combinations
	readyPods := readyPodList.All()
	portsMap := readyPodList.ListPortsForPod()
	podRequestCount := l.getRequestCountsWithPort(readyPods, portsMap)

	// Find min and max values
	var minValue int64 = math.MaxInt64
	var maxValue int64 = math.MinInt64

	for _, candidate := range candidates {
		key := candidate.String()
		count := podRequestCount[key]

		if count < minValue {
			minValue = count
		}
		if count > maxValue {
			maxValue = count
		}
	}

	// Check if imbalanced
	imbalanced := (maxValue - minValue) > l.imbalanceThreshold

	if !imbalanced {
		return candidates, false
	}

	// Return candidates with minimum count
	var minCandidates []podServerKey
	for _, candidate := range candidates {
		if podRequestCount[candidate.String()] == minValue {
			minCandidates = append(minCandidates, candidate)
		}
	}

	klog.V(4).InfoS("detected load imbalance (local)",
		"min_count", minValue,
		"max_count", maxValue,
		"threshold", l.imbalanceThreshold,
		"filtered_candidates", len(minCandidates),
		"total_candidates", len(candidates))

	return minCandidates, true
}

// deduplicatePods extracts and deduplicates pods from podServerKey list
func deduplicatePods(serverKeys []podServerKey) []*v1.Pod {
	podMap := make(map[string]*v1.Pod)
	for _, serverKey := range serverKeys {
		if serverKey.pod != nil {
			podMap[serverKey.pod.Name] = serverKey.pod
		}
	}

	result := make([]*v1.Pod, 0, len(podMap))
	for _, pod := range podMap {
		result = append(result, pod)
	}
	return result
}

// FilterPods implements [imbalancePodsFilter].
// This is the version without port, used by prefix-cache router.
func (l *localImbalancePodsFilter) FilterPods(ctx context.Context, readyPodList types.PodList) ([]*v1.Pod, bool) {
	filteredWithPort, imbalanced := l.FilterPodsWithPort(ctx, readyPodList)
	return deduplicatePods(filteredWithPort), imbalanced
}

// buildCandidates creates list of pod-port combinations for local filter
func (l *localImbalancePodsFilter) buildCandidates(readyPodList types.PodList) []podServerKey {
	var candidates []podServerKey
	pods := readyPodList.All()
	portsMap := readyPodList.ListPortsForPod()

	for _, pod := range pods {
		ports, hasMultiplePorts := portsMap[pod.Name]
		if hasMultiplePorts && len(ports) > 0 {
			// Multi-port pod: create candidate for each port
			for _, port := range ports {
				candidates = append(candidates, podServerKey{
					pod:  pod,
					port: port,
				})
			}
		} else {
			// Single-port pod: use default port
			candidates = append(candidates, podServerKey{
				pod:  pod,
				port: 0, // Will use default port via GetModelPortForPod
			})
		}
	}

	return candidates
}

// redisImbalancePodsFilter is a redis implementation of imbalancePodsFilter
// redis key design:
//
//		aibrix:prefix-cache-reqcnt:{<modelName>}: hashmap, <pod_name_with_port> -> request_count
//	 we use hashmap here which is different from power-of-two router, because hgetall is needed to scan all the keys
//	 modelName is wrapped in {} to support Redis Cluster hashtag, ensuring all keys for the same model are in the same slot
type redisImbalancePodsFilter struct {
	redisCli           *redis.Client
	imbalanceThreshold int64 // Threshold for considering pods imbalanced (default: 2)
	redisKeyPrefix     string

	// lastModelFilterTime tracks when FilterPods was last called for each model
	// Used to stop tracking request counts for models that are no longer being filtered
	lastModelFilterTime   map[string]time.Time
	lastModelFilterTimeMu sync.RWMutex // Protects lastModelFilterTime map
	requestTrackerTimeout time.Duration
}

const (
	defaultImbalanceThreshold     = int64(2)
	defaultRedisKeyPrefix         = "aibrix:prefix-cache-reqcnt"
	defaultImbalanceTrackerTimeout = time.Minute * 5 // 5 minutes, stop tracking if filter not called
)

// NewRedisImbalancePodsFilter creates a new redis-based imbalance pods filter
func NewRedisImbalancePodsFilter(redisClient *redis.Client) *redisImbalancePodsFilter {
	return &redisImbalancePodsFilter{
		redisCli:              redisClient,
		imbalanceThreshold:    defaultImbalanceThreshold,
		redisKeyPrefix:        defaultRedisKeyPrefix,
		lastModelFilterTime:   make(map[string]time.Time),
		requestTrackerTimeout: defaultImbalanceTrackerTimeout,
	}
}

// buildRedisKey constructs the redis key for a model
// Uses Redis Cluster hashtag to ensure all keys for the same model are in the same slot
func (r *redisImbalancePodsFilter) buildRedisKey(modelName string) string {
	return fmt.Sprintf("%s:{%s}", r.redisKeyPrefix, modelName)
}

// buildCandidates creates list of pod-port combinations
func (r *redisImbalancePodsFilter) buildCandidates(readyPodList types.PodList) []podServerKey {
	var candidates []podServerKey
	pods := readyPodList.All()
	portsMap := readyPodList.ListPortsForPod()

	for _, pod := range pods {
		ports, hasMultiplePorts := portsMap[pod.Name]
		if hasMultiplePorts && len(ports) > 0 {
			// Multi-port pod: create candidate for each port
			for _, port := range ports {
				candidates = append(candidates, podServerKey{
					pod:  pod,
					port: port,
				})
			}
		} else {
			// Single-port pod: use default port
			candidates = append(candidates, podServerKey{
				pod:  pod,
				port: 0, // Will use default port via GetModelPortForPod
			})
		}
	}

	return candidates
}

// FilterPodsWithPort implements [imbalancePodsFilter].
func (r *redisImbalancePodsFilter) FilterPodsWithPort(ctx context.Context, readyPodList types.PodList) ([]podServerKey, bool) {
	if len(readyPodList.All()) == 0 {
		return []podServerKey{}, false
	}

	// Build all candidates
	candidates := r.buildCandidates(readyPodList)
	if len(candidates) == 0 {
		return []podServerKey{}, false
	}

	// If only one candidate, no imbalance possible
	if len(candidates) == 1 {
		return candidates, false
	}

	// Get model name from the first pod (all pods should serve the same model)
	modelName := "" // TODO: get model name from context or pod labels
	// For now, we'll need model name passed in - this is a design issue
	// Let's check pod labels for model name
	firstPod := candidates[0].pod
	if firstPod.Labels != nil {
		if model, ok := firstPod.Labels["model"]; ok {
			modelName = model
		}
	}

	if modelName == "" {
		klog.V(4).InfoS("cannot determine model name from pod, skipping imbalance check")
		return candidates, false
	}

	// Update last filter time for this model
	r.lastModelFilterTimeMu.Lock()
	r.lastModelFilterTime[modelName] = time.Now()
	r.lastModelFilterTimeMu.Unlock()

	// Get all request counts from Redis using HGETALL
	key := r.buildRedisKey(modelName)
	counts, err := r.redisCli.HGetAll(ctx, key).Result()
	if err != nil {
		klog.V(4).ErrorS(err, "failed to get request counts from redis", "key", key)
		return candidates, false
	}

	// Build count map for candidates
	candidateCounts := make(map[string]int64)
	var minCount int64 = math.MaxInt64
	var maxCount int64 = math.MinInt64
	for _, candidate := range candidates {
		candidateKey := candidate.String()
		count := int64(0)
		if countStr, exists := counts[candidateKey]; exists {
			if parsedCount, err := strconv.ParseInt(countStr, 10, 64); err == nil {
				count = parsedCount
			}
		}
		candidateCounts[candidateKey] = count

		if count < minCount {
			minCount = count
		}
		if count > maxCount {
			maxCount = count
		}
	}

	// Check if imbalanced
	imbalanced := (maxCount - minCount) > r.imbalanceThreshold

	if !imbalanced {
		return candidates, false
	}

	// Return candidates with minimum count
	var minCandidates []podServerKey
	for _, candidate := range candidates {
		if candidateCounts[candidate.String()] == minCount {
			minCandidates = append(minCandidates, candidate)
		}
	}

	klog.V(4).InfoS("detected load imbalance",
		"model", modelName,
		"min_count", minCount,
		"max_count", maxCount,
		"threshold", r.imbalanceThreshold,
		"filtered_candidates", len(minCandidates),
		"total_candidates", len(candidates))

	return minCandidates, true
}

// FilterPods implements [imbalancePodsFilter].
// This is the version without port, used by prefix-cache router.
func (r *redisImbalancePodsFilter) FilterPods(ctx context.Context, readyPodList types.PodList) ([]*v1.Pod, bool) {
	filteredWithPort, imbalanced := r.FilterPodsWithPort(ctx, readyPodList)
	return deduplicatePods(filteredWithPort), imbalanced
}

// AddRequestCount implements [cache.RequestTracker].
func (r *redisImbalancePodsFilter) AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) (traceTerm int64) {
	// Check whether target pod is set
	if !ctx.HasRouted() {
		return 0
	}

	// Skip counting if this model hasn't been filtered recently
	// This avoids counting requests that are no longer being load-balanced
	r.lastModelFilterTimeMu.RLock()
	lastModelFilterTime, ok := r.lastModelFilterTime[modelName]
	r.lastModelFilterTimeMu.RUnlock()

	if ok {
		// If it's been too long since last filter call, skip counting
		if time.Since(lastModelFilterTime) > r.requestTrackerTimeout {
			klog.V(5).InfoS("skip request count tracking, model filter timeout",
				"model", modelName,
				"last_filter_time", lastModelFilterTime,
				"timeout", r.requestTrackerTimeout)
			return 0
		}
	} else {
		// No filter history for this model, skip counting
		klog.V(5).InfoS("skip request count tracking, no filter history",
			"model", modelName)
		return 0
	}

	// Check whether it is called the first time
	v := ctx.Context.Value(imbalancePodsFilterReqAddedKey)
	if v != nil {
		return 0
	}
	ctx.Context = context.WithValue(ctx.Context, imbalancePodsFilterReqAddedKey, true)

	// Build pod server key
	port := ctx.TargetPort()
	serverKey := podServerKey{
		pod:  ctx.TargetPod(),
		port: port,
	}

	// Increment request count in hashmap
	key := r.buildRedisKey(modelName)
	field := serverKey.String()

	newCount, err := r.redisCli.HIncrBy(ctx.Context, key, field, 1).Result()
	if err != nil {
		klog.ErrorS(err, "failed to increment request count in hashmap",
			"request_id", requestID,
			"key", key,
			"field", field)
		return 0
	}

	klog.V(4).InfoS("imbalance_filter_add_request",
		"request_id", requestID,
		"model", modelName,
		"pod", serverKey.String(),
		"new_count", newCount)

	return newCount
}

// DoneRequestCount implements [cache.RequestTracker].
// Only one DoneRequestXXX should be called for a request. Idemptency is not required.
func (r *redisImbalancePodsFilter) DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64) {
	// Check whether target pod is set
	if ctx.TargetPod() == nil {
		return
	}

	// Check whether it is called the first time
	v := ctx.Context.Value(imbalancePodsFilterReqDelKey)
	if v != nil {
		return
	}
	ctx.Context = context.WithValue(ctx.Context, imbalancePodsFilterReqDelKey, true)

	// Build pod server key
	port := ctx.TargetPort()
	serverKey := podServerKey{
		pod:  ctx.TargetPod(),
		port: port,
	}

	// Decrement request count in hashmap
	key := r.buildRedisKey(modelName)
	field := serverKey.String()

	newCount, err := r.redisCli.HIncrBy(ctx.Context, key, field, -1).Result()
	if err != nil {
		klog.ErrorS(err, "failed to decrement request count in hashmap",
			"request_id", requestID,
			"key", key,
			"field", field)
		return
	}

	// If count is zero or negative, delete the field
	if newCount <= 0 {
		if err := r.redisCli.HDel(ctx.Context, key, field).Err(); err != nil {
			klog.V(4).ErrorS(err, "failed to delete field from hashmap",
				"key", key,
				"field", field)
		}
	}

	klog.V(4).InfoS("imbalance_filter_done_request",
		"request_id", requestID,
		"model", modelName,
		"pod", serverKey.String(),
		"new_count", newCount)
}

// DoneRequestTrace implements [cache.RequestTracker].
// Only one DoneRequestXXX should be called for a request. Idemptency is not required.
func (r *redisImbalancePodsFilter) DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, inputTokens int64, outputTokens int64, traceTerm int64) {
	r.DoneRequestCount(ctx, requestID, modelName, traceTerm)
}

var (
	_ imbalancePodsFilter = &localImbalancePodsFilter{}
	_ imbalancePodsFilter = &redisImbalancePodsFilter{}
	// redisImbalancePodsFilter also implements RequestTracker
	_ cache.RequestTracker = &redisImbalancePodsFilter{}
)
