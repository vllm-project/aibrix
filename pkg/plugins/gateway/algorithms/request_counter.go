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
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type requestCountAddedKeyType struct{}

var requestCountAddedKey = requestCountAddedKeyType{}

type requestCountDoneKeyType struct{}

var requestCountDoneKey = requestCountDoneKeyType{}

// decrementScript is a cached Lua script for atomically decrementing and deleting hash fields
// Using redis.NewScript allows Redis to cache the script and use EvalSha for better performance
var decrementScript = redis.NewScript(`
	local new_count = redis.call('HINCRBY', KEYS[1], ARGV[1], -1)
	if new_count <= 0 then
		redis.call('HDEL', KEYS[1], ARGV[1])
	end
	return new_count
`)

// MetricsProvider provides pod-level metrics
type MetricsProvider interface {
	GetMetricValueByPod(podName, namespace, metricName string) (metrics.MetricValue, error)
}

// RequestCounter provides pod-level request count
type RequestCounter interface {
	// GetRequestCounts returns a map of pod name to request count
	// Returns:
	//  pod namespace/pod name -> request count
	GetRequestCounts(ctx context.Context, readyPods []*v1.Pod) map[string]int
	// GetRequestCountsWithPort returns running request count for each pod-port combination, used when a pod has multiple ports
	// Returns:
	//  pod namespace/pod name/port -> request count
	GetRequestCountsWithPort(ctx context.Context, readyPods []*v1.Pod,
		portsMap map[string][]int) map[string]int
}

// localRequestCounter is a local implementation of RequestCounter
type localRequestCounter struct {
	metricsProvider MetricsProvider
}

// NewLocalImbalancePodsFilter creates a new local imbalance pods filter
func NewLocalRequestCounter(provider MetricsProvider) *localRequestCounter {
	return &localRequestCounter{
		metricsProvider: provider,
	}
}

func (l *localRequestCounter) GetRequestCounts(ctx context.Context, readyPods []*v1.Pod) map[string]int {
	podRequestCount := make(map[string]int, len(readyPods))
	for _, pod := range readyPods {
		runningReq, err := l.metricsProvider.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
		if err != nil {
			runningReq = &metrics.SimpleMetricValue{Value: 0}
		}

		// Build key based on withNamespace parameter
		k := &podServerKey{pod: pod}
		podRequestCount[k.String()] = int(runningReq.GetSimpleValue())
	}

	return podRequestCount
}

// getRequestCountsWithPort returns running request count for each pod-port combination
func (l *localRequestCounter) GetRequestCountsWithPort(
	ctx context.Context, readyPods []*v1.Pod, portsMap map[string][]int) map[string]int {
	podRequestCount := make(map[string]int)
	for _, pod := range readyPods {
		podPorts, exists := portsMap[pod.Name]

		// Single port or no explicit ports
		if !exists || len(podPorts) == 0 {
			runningReq, err := l.metricsProvider.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
			if err != nil {
				runningReq = &metrics.SimpleMetricValue{Value: 0}
			}
			k := &podServerKey{pod: pod}
			podRequestCount[k.String()] = int(runningReq.GetSimpleValue())
			continue
		}

		// Multi-port pod
		for _, port := range podPorts {
			var metricName string
			var keyName string

			if len(podPorts) == 1 {
				metricName = metrics.RealtimeNumRequestsRunning
				k := &podServerKey{pod: pod}
				keyName = k.String()
			} else {
				metricName = fmt.Sprintf("%s/%d", metrics.RealtimeNumRequestsRunning, port)
				// Multi-port pod
				k := &podServerKey{pod: pod, port: port}
				keyName = k.String()
			}

			var count int
			if val, err := l.metricsProvider.GetMetricValueByPod(pod.Name, pod.Namespace, metricName); err == nil && val != nil {
				count = int(val.GetSimpleValue())
			}
			podRequestCount[keyName] = count
		}
	}

	return podRequestCount
}

// redisRequestCounter is a redis-based implementation of RequestCounter
// redis key design:
//
//		aibrix:prefix-cache-reqcnt:{<modelName>}: hashmap, <pod_name_with_port> -> request_count
//	 we use hashmap here which is different from power-of-two router, because hgetall is needed to scan all the keys
//	 modelName is wrapped in {} to support Redis Cluster hashtag, ensuring all keys for the same model are in the same slot
type redisRequestCounter struct {
	redisCli       *redis.Client
	redisKeyPrefix string

	// lastModelRouteTime tracks when routing was last performed for each model
	// Used to stop tracking request counts for models that are no longer being routed
	lastModelRouteTime    map[string]time.Time
	lastModelRouteTimeMu  sync.RWMutex // Protects lastModelRouteTime map
	requestTrackerTimeout time.Duration
}

// GetRequestCounts implements [RequestCounter].
func (r *redisRequestCounter) GetRequestCounts(ctx context.Context, readyPods []*v1.Pod) map[string]int {
	// Get counts with port first and merge them by pod
	portsMap := make(map[string][]int) // empty ports map

	startTime := time.Now()
	countsWithPort := r.GetRequestCountsWithPort(ctx, readyPods, portsMap)
	klog.V(4).InfoS("redisRequestCounter_GetRequestCounts_withPort_done", "duration", time.Since(startTime))

	// Merge counts for the same pod (sum across all ports)
	result := make(map[string]int)
	for key, count := range countsWithPort {
		// Extract pod key (remove port suffix if present)
		var podKey string
		namespace, podName, _ := parseServerKey(key)
		podKey = fmt.Sprintf("%s/%s", namespace, podName)
		result[podKey] += count
	}

	return result
}

// GetRequestCountsWithPort implements [RequestCounter].
func (r *redisRequestCounter) GetRequestCountsWithPort(ctx context.Context, readyPods []*v1.Pod, portsMap map[string][]int) map[string]int {
	if len(readyPods) == 0 {
		return make(map[string]int)
	}

	// Initialize result with all readyPods set to 0
	// This ensures we always return counts for all pods, even if Redis query fails
	result := make(map[string]int)
	for _, pod := range readyPods {
		podPorts, hasPorts := portsMap[pod.Name]

		if !hasPorts || len(podPorts) == 0 {
			// No ports specified, add single entry for this pod
			k := &podServerKey{pod: pod}
			result[k.String()] = 0
		} else {
			// Has ports, add entry for each port
			for _, port := range podPorts {
				k := &podServerKey{pod: pod, port: port}
				result[k.String()] = 0
			}
		}
	}

	// Get model name from the first pod (all pods should serve the same model)
	modelName := ""
	if readyPods[0].Labels != nil {
		if model, ok := readyPods[0].Labels[constants.ModelLabelName]; ok {
			modelName = model
		}
	}

	if modelName == "" {
		klog.V(4).InfoS("cannot determine model name from pod, returning counts initialized to 0")
		return result
	}

	// Update last route time for this model
	r.lastModelRouteTimeMu.Lock()
	r.lastModelRouteTime[modelName] = time.Now()
	r.lastModelRouteTimeMu.Unlock()

	// Get all request counts from Redis using HGETALL
	key := r.buildRedisKey(modelName)
	counts, err := r.redisCli.HGetAll(ctx, key).Result()
	if err != nil {
		klog.ErrorS(err, "failed to get request counts from redis", "key", key)
		// Return result with all zeros
		return result
	}

	// Parse Redis data and update result map
	for podKey, countStr := range counts {
		// Parse count
		count, err := strconv.Atoi(countStr)
		if err != nil {
			continue
		}

		// Only update if this key was initialized (exists in result)
		if _, keyExists := result[podKey]; keyExists {
			result[podKey] = count
		}
	}

	return result
}

const (
	defaultRedisKeyPrefix          = "aibrix:prefix-cache-reqcnt"
	defaultImbalanceTrackerTimeout = time.Minute * 5 // 5 minutes, stop tracking if filter not called
)

// NewredisRequestCounter creates a new redis-based imbalance pods filter
func NewRedisRequestCounter(redisClient *redis.Client) *redisRequestCounter {
	return &redisRequestCounter{
		redisCli:              redisClient,
		redisKeyPrefix:        defaultRedisKeyPrefix,
		lastModelRouteTime:    make(map[string]time.Time),
		requestTrackerTimeout: defaultImbalanceTrackerTimeout,
	}
}

// buildRedisKey constructs the redis key for a model
// Uses Redis Cluster hashtag to ensure all keys for the same model are in the same slot
func (r *redisRequestCounter) buildRedisKey(modelName string) string {
	return fmt.Sprintf("%s:{%s}", r.redisKeyPrefix, modelName)
}

// AddRequestCount implements [cache.RequestTracker].
func (r *redisRequestCounter) AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) (traceTerm int64) {
	// Check whether target pod is set
	if !ctx.HasRouted() {
		return 0
	}

	// Skip counting if this model hasn't been routed recently
	// This avoids counting requests that are no longer being load-balanced
	r.lastModelRouteTimeMu.RLock()
	lastModelRouteTime, ok := r.lastModelRouteTime[modelName]
	r.lastModelRouteTimeMu.RUnlock()

	if ok {
		// If it's been too long since last route call, skip counting
		if time.Since(lastModelRouteTime) > r.requestTrackerTimeout {
			klog.V(5).InfoS("skip request count tracking, model route timeout",
				"model", modelName,
				"last_route_time", lastModelRouteTime,
				"timeout", r.requestTrackerTimeout)
			return 0
		}
	} else {
		// No route history for this model, skip counting
		klog.V(5).InfoS("skip request count tracking, no route history",
			"model", modelName)
		return 0
	}

	// Check whether it is called the first time
	v := ctx.Context.Value(requestCountAddedKey)
	if v != nil {
		return 0
	}
	ctx.Context = context.WithValue(ctx.Context, requestCountAddedKey, true)

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

	// Set TTL on the hash key to prevent memory leaks for inactive models
	// Use requestTrackerTimeout as the expiry duration
	// This is safe to call on every increment - Redis only updates TTL if key exists
	if err := r.redisCli.Expire(ctx.Context, key, r.requestTrackerTimeout).Err(); err != nil {
		klog.ErrorS(err, "failed to set TTL on request count key",
			"key", key,
			"ttl", r.requestTrackerTimeout)
		// Don't return error - the increment succeeded, TTL failure is not critical
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
func (r *redisRequestCounter) DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64) {
	// Check whether target pod is set
	if ctx.TargetPod() == nil {
		return
	}

	// Check if AddRequestCount was actually called for this request
	// This prevents decrementing the counter if it was never incremented
	if ctx.Context.Value(requestCountAddedKey) == nil {
		klog.V(5).InfoS("skip request count decrement, AddRequestCount was not called",
			"request_id", requestID,
			"model", modelName)
		return
	}

	// Check whether it is called the first time
	v := ctx.Context.Value(requestCountDoneKey)
	if v != nil {
		return
	}
	ctx.Context = context.WithValue(ctx.Context, requestCountDoneKey, true)

	// Build pod server key
	port := ctx.TargetPort()
	serverKey := podServerKey{
		pod:  ctx.TargetPod(),
		port: port,
	}

	// Decrement request count in hashmap
	key := r.buildRedisKey(modelName)
	field := serverKey.String()

	// Use cached Lua script to atomically decrement and delete if zero
	// This prevents race condition where another request increments the counter
	// between our decrement and delete operations
	// The script is cached at package level, allowing Redis to use EvalSha for better performance
	newCount, err := decrementScript.Run(ctx.Context, r.redisCli, []string{key}, field).Int64()
	if err != nil {
		klog.ErrorS(err, "failed to decrement request count in hashmap",
			"request_id", requestID,
			"key", key,
			"field", field)
		return
	}

	klog.V(4).InfoS("imbalance_filter_done_request",
		"request_id", requestID,
		"model", modelName,
		"pod", serverKey.String(),
		"new_count", newCount)
}

// DoneRequestTrace implements [cache.RequestTracker].
// Only one DoneRequestXXX should be called for a request. Idemptency is not required.
func (r *redisRequestCounter) DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, inputTokens int64, outputTokens int64, traceTerm int64) {
	r.DoneRequestCount(ctx, requestID, modelName, traceTerm)
}

var (
	_ RequestCounter = &localRequestCounter{}
	_ RequestCounter = &redisRequestCounter{}
	// redisRequestCounter also implements RequestTracker
	_ cache.RequestTracker = &redisRequestCounter{}
)
