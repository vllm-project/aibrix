package routingalgorithms

import (
	"context"
	"fmt"
	"math/rand"
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
)

func RegisterPowerOfTwoRouter(redisClient *redis.Client) {
	Register(RouterPowerOfTwo, func() (types.Router, error) {
		if redisClient == nil {
			return nil, fmt.Errorf("power-of-two router requires redis configuration")
		}
		return NewPowerOfTwoRouterWithRedis(redisClient), nil
	})
}

type PowerOfTwoRouter struct {
	redisClient    *redis.Client
	keyRotationSec int64 // Time interval in seconds for key rotation, default 3600 (1 hour)
}

// NewPowerOfTwoRouterWithRedis creates a new Power of Two router with the given Redis client.
// This is useful for testing or when you want to provide a custom Redis configuration.
func NewPowerOfTwoRouterWithRedis(redisClient *redis.Client, keyRotationSec ...int64) *PowerOfTwoRouter {
	if len(keyRotationSec) == 0 {
		keyRotationSec = []int64{defaultKeyRotationSec}
	}
	if keyRotationSec[0] <= 0 {
		keyRotationSec[0] = defaultKeyRotationSec
	}
	router := &PowerOfTwoRouter{
		redisClient:    redisClient,
		keyRotationSec: keyRotationSec[0],
	}
	c, err := cache.Get()
	if err != nil {
		klog.Errorf("failed to get cache: %v", err)
	} else {
		// register request tracker to cache
		c.RegisterRequestTracker(router)
	}
	return router
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

		// Get request counts for both candidates
		// TODO: it's better to fetch two request count in the same redis request
		count1 := p.getRequestCount(ctx.Context, ctx.Model, candidate1, ctx.RequestTime)
		count2 := p.getRequestCount(ctx.Context, ctx.Model, candidate2, ctx.RequestTime)

		klog.InfoS("power_of_two_selection",
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

// getRequestCount retrieves the current request count from Redis
func (p *PowerOfTwoRouter) getRequestCount(ctx context.Context, modelName string, server podServerKey, requestTime time.Time) int64 {
	key := p.buildRedisKey(modelName, server, requestTime)

	val, err := p.redisClient.Get(ctx, key).Int64()
	if err == redis.Nil {
		// Key doesn't exist, return 0
		return 0
	}
	if err != nil {
		klog.V(4).ErrorS(err, "failed to get request count from redis",
			"key", key,
			"pod", server.pod.Name,
			"port", server.port)
		return 0
	}

	return val
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
