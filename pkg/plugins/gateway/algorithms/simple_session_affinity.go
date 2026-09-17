/*
Copyright 2025 The Aibrix Team.

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
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const RouterSessionAffinity types.RoutingAlgorithm = "session-affinity"

const maxSessionKeyLen = 256

const (
	// sessionAffinityRedisKeyPrefix namespaces session-key pinning in the
	// shared Redis keyspace.
	sessionAffinityRedisKeyPrefix = "aibrix:gateway:session_affinity:"

	// sessionAffinityTTL bounds how long an idle x-aibrix-session-key pinning
	// survives in Redis. It is refreshed (re-Set) on every use, so it acts as
	// a sliding idle-expiry: a session in continuous use never expires, one
	// that goes unused for longer than this is forgotten. The local cache
	// uses the same bound: an entry this replica hasn't touched in this long
	// is dropped even if Redis still has it.
	sessionAffinityTTL = 1 * time.Hour

	// sessionKeyCacheSyncInterval bounds how stale this replica's local
	// sessionKeyPods cache can be vs Redis: a re-pin or idle-expiry on
	// another replica would otherwise stay invisible here until this
	// replica's own next Redis round trip for that key.
	sessionKeyCacheSyncInterval = 60 * time.Second

	// sessionKeyCacheSyncBatchSize bounds how many keys share one MGET round-trip.
	sessionKeyCacheSyncBatchSize = 200

	// sessionKeyRedisReadTimeout bounds the synchronous, must-read Redis
	// lookup on a local cache miss, so an unresponsive Redis degrades to
	// rendezvousPod instead of stalling the request. Matches the read
	// timeout cache.GetPodsRunningRequests already uses on this same hot path.
	sessionKeyRedisReadTimeout = 100 * time.Millisecond

	// sessionKeyRedisWriteTimeout bounds the background persist call. It runs
	// in its own goroutine off the request path, so this only guards against
	// a leaked goroutine piling up on a stuck connection, not request latency.
	sessionKeyRedisWriteTimeout = 2 * time.Second
)

func sessionAffinityRedisKey(sessionKey string) string {
	return sessionAffinityRedisKeyPrefix + sessionKey
}

func validSessionKey(sessionKey string) bool {
	return sessionKey != "" && len(sessionKey) <= maxSessionKeyLen
}

// sessionKeyCacheItem is what's stored in sessionKeyPods. confirmed is
// local-only bookkeeping (never written to Redis): true once this replica
// has successfully written or read the pinning in Redis. A Redis miss for an
// unconfirmed entry is not proof another replica deleted it — the write may
// still be in flight or have failed — so sync retries persist instead of
// dropping the only surviving copy (see handleSessionKeyCacheSyncMiss).
type sessionKeyCacheItem struct {
	addr      string
	confirmed bool
	storedAt  time.Time
}

// RedisBackedRouter is implemented by routers that persist routing state to
// Redis and need a background sync loop started once both Redis and a
// process shutdown signal are available -- typically later than the router
// itself is constructed (see RouterManager.Init). The gateway server type-
// asserts to this interface after building its own shutdown channel.
type RedisBackedRouter interface {
	Start(stopCh <-chan struct{}, redisClient *redis.Client)
}

func init() {
	Register(RouterSessionAffinity, NewSessionAffinityRouter)
}

type sessionAffinityRouter struct {
	// sessionKeyPods is a Redis read-through cache of session-key -> pod
	// address pins. It is only populated when redisClient is non-nil:
	// without Redis the router stays stateless and rendezvousPod remains
	// the sole (and cross-replica-deterministic) mapping. Idle entries
	// are dropped during the sync loop after sessionAffinityTTL.
	sessionKeyPods sync.Map // sessionKey (string) -> sessionKeyCacheItem

	// redisClient persists sessionKeyPods entries across gateway replicas.
	// Nil until Start is called (e.g. in tests, or when Redis isn't
	// configured), in which case session-key routing stays stateless
	// rendezvous hashing, exactly as before Redis support was added.
	redisClient *redis.Client
	startOnce   sync.Once
}

func NewSessionAffinityRouter() (types.Router, error) {
	return &sessionAffinityRouter{}, nil
}

// Start wires redisClient into the router and launches a background loop
// that reconciles sessionKeyPods against Redis every sessionKeyCacheSyncInterval,
// so a re-pin or idle-expiry on another gateway replica becomes visible here
// too. Idempotent (only the first call takes effect) and a no-op when
// redisClient is nil. Stops when stopCh is closed.
func (r *sessionAffinityRouter) Start(stopCh <-chan struct{}, redisClient *redis.Client) {
	if redisClient == nil {
		return
	}
	r.startOnce.Do(func() {
		r.redisClient = redisClient
		ticker := time.NewTicker(sessionKeyCacheSyncInterval)
		go func() {
			for {
				select {
				case <-ticker.C:
					r.syncSessionKeyPodsFromRedis()
				case <-stopCh:
					ticker.Stop()
					return
				}
			}
		}()
	})
}

// syncSessionKeyPodsFromRedis reconciles this replica's local sessionKeyPods
// against Redis: missing/expired keys are evicted, changed values are
// refreshed, and idle local copies are dropped even if Redis still has them.
// It only walks session keys already cached locally -- discovering a session
// pinned by another replica but never seen here is readSessionKeyFromRedis's
// job, on that key's next local miss. A whole-batch Redis error leaves the
// local cache untouched (a failed read is not a miss).
func (r *sessionAffinityRouter) syncSessionKeyPodsFromRedis() {
	if r.redisClient == nil {
		return
	}
	var sessionKeys []string
	r.sessionKeyPods.Range(func(key, value any) bool {
		k, ok := key.(string)
		if !ok {
			r.sessionKeyPods.Delete(key)
			return true
		}
		item, ok := value.(sessionKeyCacheItem)
		if !ok || time.Since(item.storedAt) >= sessionAffinityTTL {
			r.forgetSessionKey(k)
			return true
		}
		sessionKeys = append(sessionKeys, k)
		return true
	})
	if len(sessionKeys) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), sessionKeyCacheSyncInterval)
	defer cancel()

	for batchStart := 0; batchStart < len(sessionKeys); batchStart += sessionKeyCacheSyncBatchSize {
		chunk := sessionKeys[batchStart:min(batchStart+sessionKeyCacheSyncBatchSize, len(sessionKeys))]

		redisKeys := make([]string, len(chunk))
		for i, k := range chunk {
			redisKeys[i] = sessionAffinityRedisKey(k)
		}
		vals, err := r.redisClient.MGet(ctx, redisKeys...).Result()
		if err != nil {
			klog.V(4).ErrorS(err, "failed to refresh session-key cache from redis", "batchStart", batchStart, "batchLen", len(chunk))
			continue
		}

		for i, k := range chunk {
			raw, ok := vals[i].(string)
			if !ok {
				r.handleSessionKeyCacheSyncMiss(k)
				continue
			}
			r.storeSessionKeyLocal(k, raw, true)
		}
	}
}

// handleSessionKeyCacheSyncMiss reacts to sessionKey being absent from Redis
// during a sync pass. A previously confirmed entry is evicted: Redis no
// longer vouches for it (expired, or another replica re-derived it). An
// entry that was never confirmed means this replica's own write is still
// pending or failed — a nil read says nothing about whether that pinning is
// still valid, so it's retried here instead of being dropped as the only
// surviving copy. Idle unconfirmed entries are evicted rather than retried
// forever.
func (r *sessionAffinityRouter) handleSessionKeyCacheSyncMiss(sessionKey string) {
	cached, found := r.sessionKeyPods.Load(sessionKey)
	if !found {
		return
	}
	item, ok := cached.(sessionKeyCacheItem)
	if !ok || item.confirmed || time.Since(item.storedAt) >= sessionAffinityTTL {
		r.forgetSessionKey(sessionKey)
		return
	}
	r.persistSessionKeyToRedis(sessionKey, item.addr)
}

// persistSessionKeyToRedis write-throughs sessionKey -> addr with a sliding
// TTL: every call, including a refresh of an unchanged pinning, pushes the
// idle-expiry another sessionAffinityTTL out. Intended to be called via `go`
// at the routing call sites so a slow/unavailable Redis never adds latency to
// the request path; a failed write just means this replica's local cache is,
// for now, the only copy of this pinning. Returns whether Redis now has it.
func (r *sessionAffinityRouter) persistSessionKeyToRedis(sessionKey, addr string) bool {
	if r.redisClient == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionKeyRedisWriteTimeout)
	defer cancel()
	if err := r.redisClient.Set(ctx, sessionAffinityRedisKey(sessionKey), addr, sessionAffinityTTL).Err(); err != nil {
		klog.V(4).ErrorS(err, "failed to persist session key pinning to redis", "session_key", sessionKey)
		return false
	}
	r.markSessionKeyConfirmed(sessionKey, addr)
	return true
}

func (r *sessionAffinityRouter) markSessionKeyConfirmed(sessionKey, addr string) {
	for {
		cached, ok := r.sessionKeyPods.Load(sessionKey)
		if !ok {
			return
		}
		item, ok := cached.(sessionKeyCacheItem)
		if !ok || item.addr != addr || item.confirmed {
			return
		}
		updated := item
		updated.confirmed = true
		if r.sessionKeyPods.CompareAndSwap(sessionKey, cached, updated) {
			return
		}
	}
}

func (r *sessionAffinityRouter) forgetSessionKey(sessionKey string) {
	r.sessionKeyPods.Delete(sessionKey)
}

// storeSessionKeyLocal mirrors sessionKey -> addr into the local cache. A
// no-op when Redis isn't configured (the cache is a Redis read-through, not
// a pin of its own). Same-addr stores refresh storedAt and only raise
// confirmed, so a TTL-refresh does not un-confirm a pin whose Redis write
// already landed.
func (r *sessionAffinityRouter) storeSessionKeyLocal(sessionKey, addr string, confirmed bool) {
	if r.redisClient == nil || !validSessionKey(sessionKey) {
		return
	}
	for {
		existing, ok := r.sessionKeyPods.Load(sessionKey)
		if !ok {
			newItem := sessionKeyCacheItem{addr: addr, confirmed: confirmed, storedAt: time.Now()}
			if _, loaded := r.sessionKeyPods.LoadOrStore(sessionKey, newItem); !loaded {
				return
			}
			continue
		}
		item, ok := existing.(sessionKeyCacheItem)
		if !ok {
			newItem := sessionKeyCacheItem{addr: addr, confirmed: confirmed, storedAt: time.Now()}
			if r.sessionKeyPods.CompareAndSwap(sessionKey, existing, newItem) {
				return
			}
			continue
		}
		if item.addr == addr {
			updated := item
			updated.storedAt = time.Now()
			if confirmed {
				updated.confirmed = true
			}
			if r.sessionKeyPods.CompareAndSwap(sessionKey, existing, updated) {
				return
			}
			continue
		}
		newItem := sessionKeyCacheItem{addr: addr, confirmed: confirmed, storedAt: time.Now()}
		if r.sessionKeyPods.CompareAndSwap(sessionKey, existing, newItem) {
			return
		}
	}
}

// rememberSessionKey commits sessionKey -> addr locally (when Redis is
// configured) and write-throughs to Redis in the background.
func (r *sessionAffinityRouter) rememberSessionKey(sessionKey, addr string) {
	if r.redisClient == nil || !validSessionKey(sessionKey) {
		return
	}
	r.storeSessionKeyLocal(sessionKey, addr, false)
	go r.persistSessionKeyToRedis(sessionKey, addr)
}

func (r *sessionAffinityRouter) loadCachedAddr(sessionKey string) (string, bool) {
	cached, ok := r.sessionKeyPods.Load(sessionKey)
	if !ok {
		return "", false
	}
	item, ok := cached.(sessionKeyCacheItem)
	if !ok {
		r.forgetSessionKey(sessionKey)
		return "", false
	}
	return item.addr, true
}

// readSessionKeyFromRedis looks up sessionKey's pinning in Redis on a local
// cache miss, so a session first pinned by a different gateway replica is
// honored here too. Bounded by sessionKeyRedisReadTimeout so an unresponsive
// Redis degrades to rendezvousPod rather than stalling the request.
func (r *sessionAffinityRouter) readSessionKeyFromRedis(ctx *types.RoutingContext, sessionKey string) (string, bool) {
	if r.redisClient == nil {
		return "", false
	}
	readCtx, cancel := context.WithTimeout(context.Background(), sessionKeyRedisReadTimeout)
	defer cancel()
	val, err := r.redisClient.Get(readCtx, sessionAffinityRedisKey(sessionKey)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			klog.V(4).ErrorS(err, "failed to read session key pinning from redis", "request_id", ctx.RequestID, "session_key", sessionKey)
		}
		return "", false
	}
	return val, true
}

// resolveSessionPod finds the pod this request's session should be pinned
// to, checking in order: an exact address match on the session-ID header, a
// cached or Redis-backed session-key pinning, and rendezvous hashing on the
// session key. via reports which of those resolved it, for logging only.
//
// It only touches the local sessionKeyPods cache (evicting a stale entry, or
// mirroring a Redis hit into it) -- refreshing the Redis TTL and committing
// to this pod as the final decision is the caller's job: Route commits
// immediately, while ScoreAll must not, since a blended competitor could
// still win (see PostRouteUpdate).
func (r *sessionAffinityRouter) resolveSessionPod(ctx *types.RoutingContext, pods []*v1.Pod) (pod *v1.Pod, sessionKey string, via string) {
	if sessionID := ctx.ReqHeaders[constants.HeaderSessionID]; sessionID != "" {
		decoded, err := base64.StdEncoding.DecodeString(sessionID)
		if err != nil {
			klog.V(4).ErrorS(err, "Invalid session ID format", "request_id", ctx.RequestID)
		} else if p := findReadyPodByAddr(ctx, pods, string(decoded)); p != nil {
			return p, "", "session-id"
		}
	}

	sessionKey = ctx.ReqHeaders[constants.HeaderSessionKey]
	if !validSessionKey(sessionKey) {
		return nil, "", ""
	}

	var cachedAddr string
	if addr, ok := r.loadCachedAddr(sessionKey); ok {
		cachedAddr = addr
		if p := findReadyPodByAddr(ctx, pods, cachedAddr); p != nil {
			return p, sessionKey, "session-key-cache"
		}
		r.forgetSessionKey(sessionKey)
	}

	// Local miss: check Redis before falling back to rendezvousPod, so a
	// pinning made by another gateway replica is still honored. Skipped
	// when Redis agrees with the just-invalidated local cache, since that
	// pod is already known to be unready.
	if redisAddr, ok := r.readSessionKeyFromRedis(ctx, sessionKey); ok && redisAddr != cachedAddr {
		if p := findReadyPodByAddr(ctx, pods, redisAddr); p != nil {
			r.storeSessionKeyLocal(sessionKey, redisAddr, true)
			return p, sessionKey, "session-key-redis"
		}
	}

	return rendezvousPod(ctx, pods, sessionKey), sessionKey, "rendezvous"
}

// Route implements session affinity by attempting to route requests to the same pod
// using a session ID stored in the request header. The session ID encodes the target
// pod's address as "IP:Port". If no valid session exists, it falls back to a randomly selected ready pod.
func (r *sessionAffinityRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	pods := readyPodList.All()

	var pod *v1.Pod
	var sessionKey, via string
	if ctx.ReqHeaders == nil {
		klog.V(4).InfoS("No request or headers, skipping session affinity", "request_id", ctx.RequestID)
	} else {
		pod, sessionKey, via = r.resolveSessionPod(ctx, pods)
	}

	if pod == nil {
		if pod = fallbackPod(ctx, pods); pod == nil {
			return "", fmt.Errorf("no fallback pod found with a valid network address")
		}
		sessionKey, via = "", "fallback"
	}

	port := utils.GetModelPortForPod(ctx.RequestID, pod)
	addr := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port)))
	ctx.SetTargetPod(pod)
	r.setSessionHeader(ctx, addr)
	if sessionKey != "" {
		r.rememberSessionKey(sessionKey, addr)
	}
	klog.V(4).InfoS("Session affinity resolved", "request_id", ctx.RequestID, "addr", addr, "via", via)
	return ctx.TargetAddress(), nil
}

// findReadyPodByAddr returns the ready pod whose "IP:Port" equals targetAddr,
// or nil if none matches (e.g. it was scaled down or restarted with a new address).
func findReadyPodByAddr(ctx *types.RoutingContext, pods []*v1.Pod, targetAddr string) *v1.Pod {
	for _, pod := range pods {
		port := utils.GetModelPortForPod(ctx.RequestID, pod)
		if port == 0 {
			continue
		}
		if net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port))) == targetAddr {
			return pod
		}
	}
	return nil
}

// rendezvousPod maps an opaque caller-owned session key to a ready pod without
// keeping gateway-side session state. Ties are broken by address so selection
// does not depend on the PodList order.
func rendezvousPod(ctx *types.RoutingContext, pods []*v1.Pod, sessionKey string) *v1.Pod {
	if !validSessionKey(sessionKey) {
		return nil
	}

	var selected *v1.Pod
	var bestScore uint64
	var bestAddr string
	for _, pod := range pods {
		port := utils.GetModelPortForPod(ctx.RequestID, pod)
		if port == 0 || pod.Status.PodIP == "" {
			continue
		}
		addr := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port)))
		const (
			fnvOffset64 = 14695981039346656037
			fnvPrime64  = 1099511628211
		)
		score := uint64(fnvOffset64)
		for i := 0; i < len(sessionKey); i++ {
			score ^= uint64(sessionKey[i])
			score *= fnvPrime64
		}
		// Separate the two variable-length inputs so their concatenations
		// cannot alias (for example, "ab"+"c" and "a"+"bc").
		score ^= 0
		score *= fnvPrime64
		for i := 0; i < len(addr); i++ {
			score ^= uint64(addr[i])
			score *= fnvPrime64
		}
		if selected == nil || score > bestScore || (score == bestScore && addr < bestAddr) {
			selected = pod
			bestScore = score
			bestAddr = addr
		}
	}
	return selected
}

func (r *sessionAffinityRouter) setSessionHeader(ctx *types.RoutingContext, addr string) {
	if ctx.RespHeaders == nil {
		ctx.RespHeaders = make(map[string]string)
	}
	ctx.RespHeaders[constants.HeaderSessionID] = base64.StdEncoding.EncodeToString([]byte(addr))
}

// fallbackPod selects a random ready pod with a valid network address, or
// nil if none exists. The caller (Route) commits to it the same way it
// commits to any other resolveSessionPod result.
func fallbackPod(ctx *types.RoutingContext, pods []*v1.Pod) *v1.Pod {
	rand.Shuffle(len(pods), func(i, j int) { pods[i], pods[j] = pods[j], pods[i] })
	for _, pod := range pods {
		port := utils.GetModelPortForPod(ctx.RequestID, pod)
		// A routable pod must have a valid IP and port.
		if port == 0 || pod.Status.PodIP == "" {
			klog.V(4).Infof("Fallback skipping pod %s with invalid "+
				"network address (IP: %s, Port: %d)", pod.Name, pod.Status.PodIP, port)
			continue
		}
		return pod
	}
	return nil
}

// ScoreAll computes the scores for all ready pods in a single batch operation.
// The chosen pod receives score 1, all others score 0, using the same
// resolution order as Route (see resolveSessionPod). If none of these
// resolve to a pod, every pod scores 0. Unlike Route, it never writes to the
// Redis-backed pinning (see resolveSessionPod's doc comment).
func (r *sessionAffinityRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) (scores []float64, scored []bool, err error) {
	pods := readyPodList.All()
	scores = make([]float64, len(pods))
	scored = make([]bool, len(pods))
	for i := range pods {
		scored[i] = true
	}
	if ctx.ReqHeaders == nil {
		return
	}

	if pod, _, _ := r.resolveSessionPod(ctx, pods); pod != nil {
		for i, p := range pods {
			if p == pod {
				scores[i] = 1
				break
			}
		}
	}
	return
}

// Polarity returns whether higher or lower score is better.
func (r *sessionAffinityRouter) Polarity() types.Polarity {
	return types.PolarityMost
}

// PostRouteUpdate ensures the session header is set on the response *after* the final target pod is chosen.
// This is necessary for multi-strategy routing where ScoreAll is read-only.
func (r *sessionAffinityRouter) PostRouteUpdate(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) error {
	port := utils.GetModelPortForPod(ctx.RequestID, targetPod)
	if port == 0 {
		return nil
	}
	addr := net.JoinHostPort(targetPod.Status.PodIP, strconv.Itoa(int(port)))
	r.setSessionHeader(ctx, addr)
	if ctx.ReqHeaders == nil {
		return nil
	}
	if sessionKey := ctx.ReqHeaders[constants.HeaderSessionKey]; validSessionKey(sessionKey) {
		r.rememberSessionKey(sessionKey, addr)
	}
	return nil
}
