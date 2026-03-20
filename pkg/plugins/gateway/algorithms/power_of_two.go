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
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
)

type po2RequestCountAddedKeyStruct struct{}
type po2RequestCountDoneKeyStruct struct{}

var po2RequestCountAddedKey = po2RequestCountAddedKeyStruct{}
var po2RequestCountDoneKey = po2RequestCountDoneKeyStruct{}

/*
redis key design:

po2_req_count:<modelName>:<podName>_<port>:timestamp -> realtime running request
*/

const (
	RouterPowerOfTwo      types.RoutingAlgorithm = "power-of-two"
	redisKeyPrefix                               = "po2_req_count"
	defaultRedisKeyExpiry                        = 5 * time.Minute
	defaultKeyRotationSec                        = 3600 // 1 hour, key rotation interval
	// if po2 router is not used for a long time, the request tracker will stop counting after 600 seconds to reduce redis pressure and latency
	defaultTrackerTimeout = time.Minute * 5 // 5 minutes, request tracker timeout
)

func RegisterPowerOfTwoRouter(redisClient *redis.Client) {
	Register(RouterPowerOfTwo, func() (types.Router, error) {
		if redisClient == nil {
			return nil, fmt.Errorf("power-of-two router requires redis configuration")
		}
		return NewPowerOfTwoRouterWithRedis(redisClient)
	})
}

type PowerOfTwoRouter struct {
	redisClient    *redis.Client
	keyRotationSec int64 // Time interval in seconds for key rotation, default 3600 (1 hour)

	// lastRoutingTime is the timestamp of the last routing of a model. Used to avoid unnecessary redis request
	lastRoutingTime       map[string]time.Time
	lastRoutingTimeMu     sync.RWMutex // Protects lastRoutingTime map from concurrent access
	requestTrackerTimeout time.Duration
}

// NewPowerOfTwoRouterWithRedis creates a new Power of Two router with the given Redis client.
// This is useful for testing or when you want to provide a custom Redis configuration.
// Returns error if cache registration fails, as the router cannot function correctly without it.
func NewPowerOfTwoRouterWithRedis(redisClient *redis.Client, keyRotationSec ...int64) (*PowerOfTwoRouter, error) {
	if len(keyRotationSec) == 0 {
		keyRotationSec = []int64{defaultKeyRotationSec}
	}
	if keyRotationSec[0] <= 0 {
		keyRotationSec[0] = defaultKeyRotationSec
	}
	router := &PowerOfTwoRouter{
		redisClient:           redisClient,
		keyRotationSec:        keyRotationSec[0],
		requestTrackerTimeout: defaultTrackerTimeout,
		lastRoutingTime:       make(map[string]time.Time),
	}
	c, err := cache.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get cache for RequestTracker registration: %w", err)
	}
	// Register request tracker to cache
	// This is critical: without registration, DoneRequestCount will never be called,
	// causing Redis counters to only increment and never decrement (resource leak)
	c.RegisterRequestTracker(router)
	return router, nil
}

// podServerKey represents a pod with specific port
type podServerKey struct {
	pod  *v1.Pod
	port int
}

func (k podServerKey) String() string {
	if k.port == 0 {
		return k.pod.Name
	}
	return fmt.Sprintf("%s_%d", k.pod.Name, k.port)
}

// Route implements [types.Router] using power of two choices algorithm.
// It randomly selects two candidates and routes to the one with fewer running requests.
func (p *PowerOfTwoRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	readyPods := readyPodList.All()
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no ready pods available")
	}

	// Build list of all pod-port combinations
	candidates := p.buildCandidates(readyPods, readyPodList.ListPortsForPod())
	if len(candidates) == 0 {
		return "", fmt.Errorf("no valid candidates found")
	}

	// Select target using power of two choices
	var target podServerKey
	if len(candidates) == 1 {
		// Only one candidate, use it directly
		target = candidates[0]
	} else {
		// Randomly pick two candidates
		idx1 := rand.Intn(len(candidates))
		idx2 := rand.Intn(len(candidates))
		// Ensure different candidates
		for idx2 == idx1 && len(candidates) > 1 {
			idx2 = rand.Intn(len(candidates))
		}

		candidate1 := candidates[idx1]
		candidate2 := candidates[idx2]

		// Get request counts for both candidates using batch query (MGET)
		counts := p.getRequestCounts(ctx.Context, ctx.Model, []podServerKey{candidate1, candidate2}, ctx.RequestTime)
		count1 := counts[0]
		count2 := counts[1]

		klog.V(4).InfoS("power_of_two_selection",
			"request_id", ctx.RequestID,
			"request_time", ctx.RequestTime.Unix(),
			"candidate1", candidate1.String(),
			"count1", count1,
			"candidate2", candidate2.String(),
			"count2", count2)

		// Choose the one with fewer requests
		if count1 <= count2 {
			target = candidate1
		} else {
			target = candidate2
		}
	}

	klog.V(4).InfoS("power_of_two_route",
		"request_id", ctx.RequestID,
		"target_pod", target.pod.Name,
		"target_port", target.port)

	// Set target pod and port
	ctx.SetTargetPod(target.pod)
	if target.port != 0 {
		ctx.SetTargetPort(target.port)
	}

	// call immediate after setting target pod to avoid other Route call pick up the same pod
	p.lastRoutingTimeMu.Lock()
	p.lastRoutingTime[ctx.Model] = time.Now()
	p.lastRoutingTimeMu.Unlock()
	p.AddRequestCount(ctx, ctx.RequestID, ctx.Model)
	return ctx.TargetAddress(), nil
}

// buildCandidates creates list of pod-port combinations
func (p *PowerOfTwoRouter) buildCandidates(pods []*v1.Pod, portsMap map[string][]int) []podServerKey {
	var candidates []podServerKey

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

// getRequestCount retrieves the current request count from Redis for a single server
func (p *PowerOfTwoRouter) getRequestCount(ctx context.Context, modelName string, server podServerKey, requestTime time.Time) int64 {
	counts := p.getRequestCounts(ctx, modelName, []podServerKey{server}, requestTime)
	if len(counts) > 0 {
		return counts[0]
	}
	return 0
}

// getRequestCounts retrieves request counts from Redis for multiple servers using MGET.
// Returns a slice of counts with the same length and order as the input servers.
// This is more efficient than multiple GET calls as it only requires one network roundtrip.
func (p *PowerOfTwoRouter) getRequestCounts(ctx context.Context, modelName string, servers []podServerKey, requestTime time.Time) []int64 {
	if len(servers) == 0 {
		return []int64{}
	}

	// Build all keys
	keys := make([]string, len(servers))
	for i, server := range servers {
		keys[i] = p.buildRedisKey(modelName, server, requestTime)
	}

	// Use MGET to fetch all values in one roundtrip
	vals, err := p.redisClient.MGet(ctx, keys...).Result()
	if err != nil {
		klog.V(4).ErrorS(err, "failed to batch get request counts from redis",
			"key_count", len(keys),
			"servers", len(servers))
		// Return zeros on error
		return make([]int64, len(servers))
	}

	// Convert results to int64 slice
	counts := make([]int64, len(servers))
	for i, val := range vals {
		if val == nil {
			// Key doesn't exist, use 0
			counts[i] = 0
			continue
		}

		// Try to convert to int64
		switch v := val.(type) {
		case string:
			// Redis returns strings for integer values
			if intVal, err := strconv.ParseInt(v, 10, 64); err == nil {
				counts[i] = intVal
			} else {
				klog.V(4).ErrorS(err, "failed to parse redis value as int64",
					"key", keys[i],
					"value", v,
					"pod", servers[i].pod.Name,
					"port", servers[i].port)
				counts[i] = 0
			}
		case int64:
			counts[i] = v
		default:
			klog.V(4).InfoS("unexpected redis value type",
				"key", keys[i],
				"type", fmt.Sprintf("%T", v),
				"pod", servers[i].pod.Name,
				"port", servers[i].port)
			counts[i] = 0
		}
	}

	return counts
}

// buildRedisKey constructs the redis key for a pod-port combination.
// The timestamp is modulo keyRotationSec to rotate keys periodically (default: every hour).
// This prevents stale counts from accumulating over time.
func (p *PowerOfTwoRouter) buildRedisKey(modelName string, server podServerKey, requestTime time.Time) string {
	timestamp := requestTime.Unix()
	// Rotate key every keyRotationSec seconds (default 3600s = 1 hour)
	rotatedTimestamp := timestamp - (timestamp % p.keyRotationSec)
	return fmt.Sprintf("%s:%s:%s:%d", redisKeyPrefix, modelName, server.String(), rotatedTimestamp)
}

// getRequestCountRedisKey returns the redis key for current request
func (p *PowerOfTwoRouter) getRequestCountRedisKey(ctx *types.RoutingContext) string {
	port := ctx.TargetPort()
	serverKey := podServerKey{
		pod:  ctx.TargetPod(),
		port: port,
	}
	return p.buildRedisKey(ctx.Model, serverKey, ctx.RequestTime)
}

// AddRequestCount implements [cache.RequestTracker].
func (p *PowerOfTwoRouter) AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) (traceTerm int64) {
	// Check whether routing is done and target pod is set
	if ctx.TargetPod() == nil {
		return 0
	}

	// Skip counting if this model hasn't been routed by this router recently
	// This avoids counting requests that are handled by other routers (e.g., prefix-cache)
	p.lastRoutingTimeMu.RLock()
	lastModelRoutingTime, ok := p.lastRoutingTime[ctx.Model]
	p.lastRoutingTimeMu.RUnlock()

	if ok {
		// If it's been too long since last routing, skip counting
		if time.Since(lastModelRoutingTime) > p.requestTrackerTimeout {
			return 0
		}
	} else {
		// No routing history for this model, skip counting
		return 0
	}

	// Check whether it is called the first time
	added := ctx.Value(po2RequestCountAddedKey)
	if added != nil {
		return 0
	}
	ctx.Context = context.WithValue(ctx.Context, po2RequestCountAddedKey, true)

	key := p.getRequestCountRedisKey(ctx)

	// Increment the request count in Redis
	newCount, err := p.redisClient.Incr(ctx.Context, key).Result()
	if err != nil {
		klog.ErrorS(err, "failed to increment request count",
			"request_id", requestID,
			"key", key)
		return 0
	}

	// Set expiry on the key to prevent memory leak
	p.redisClient.Expire(ctx.Context, key, defaultRedisKeyExpiry)

	klog.V(4).InfoS("power_of_two_add_request",
		"request_id", requestID,
		"key", key,
		"new_count", newCount)

	// Use current count as trace term
	return newCount
}

// DoneRequestCount implements [cache.RequestTracker].
func (p *PowerOfTwoRouter) DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64) {
	// Check whether target pod is set
	if ctx.TargetPod() == nil {
		return
	}

	// avoid decreasing if AddRequestCount is not called
	added := ctx.Value(po2RequestCountAddedKey)
	if added == nil {
		return
	}

	// avoid duplicate decrement
	done := ctx.Value(po2RequestCountDoneKey)
	if done != nil {
		return
	}
	ctx.Context = context.WithValue(ctx.Context, po2RequestCountDoneKey, true)

	key := p.getRequestCountRedisKey(ctx)

	// Decrement the request count in Redis
	newCount, err := p.redisClient.Decr(ctx.Context, key).Result()
	if err != nil {
		klog.ErrorS(err, "failed to decrement request count",
			"request_id", requestID,
			"key", key)
		return
	}

	klog.V(4).InfoS("power_of_two_done_request",
		"request_id", requestID,
		"key", key,
		"new_count", newCount)
}

// DoneRequestTrace implements [cache.RequestTracker].
func (p *PowerOfTwoRouter) DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, inputTokens int64, outputTokens int64, traceTerm int64) {
	// For power of two router, we only track request count, not detailed trace
	// Just call DoneRequestCount
	p.DoneRequestCount(ctx, requestID, modelName, traceTerm)
}

// SubscribedMetrics implements [types.Router].
func (p *PowerOfTwoRouter) SubscribedMetrics() []string {
	// Power of two router doesn't rely on pod metrics, uses Redis instead
	return []string{}
}

var _ types.Router = (*PowerOfTwoRouter)(nil)
var _ cache.RequestTracker = (*PowerOfTwoRouter)(nil)
