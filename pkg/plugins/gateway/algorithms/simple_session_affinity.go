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
	"sync/atomic"
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

// maxSessionKeyPodsEntries bounds how many distinct cache keys sessionKeyPods holds locally.
// The session-key portion of that cache key comes directly from the caller, so without a cap a
// client cycling through continuously-changing x-aibrix-session-key values could grow local
// memory -- and the periodic Redis sync's per-key MGET work, which scans the whole map --
// without limit. Enforced approximately via sessionKeyPodsSize (see storeSessionKeyLocal): a
// handful of concurrent requests can land right at the boundary and push the map slightly past
// this count, but growth stays bounded instead of unbounded. A cache key that doesn't fit simply
// isn't cached locally; resolution still works via Redis (readSessionKeyFromRedis) or, without
// Redis, stateless rendezvous hashing -- it costs a Redis round trip (or cross-replica
// stickiness) instead of a local hit.
var maxSessionKeyPodsEntries = utils.LoadEnvInt("AIBRIX_SESSION_AFFINITY_MAX_LOCAL_KEYS", 100_000)

func sessionAffinityRedisKey(cacheKey string) string {
	return sessionAffinityRedisKeyPrefix + cacheKey
}

func validSessionKey(sessionKey string) bool {
	return sessionKey != "" && len(sessionKey) <= maxSessionKeyLen
}

// sessionCacheKeySeparator joins a request's model onto its caller-provided session key to
// form the local-cache/Redis key. A NUL byte can't appear in a model name or in the raw
// header value, so "modelA"+"x" and "model"+"Ax" can never alias to the same composite key.
const sessionCacheKeySeparator = "\x00"

// sessionCacheKey scopes sessionKey to ctx's model, so the local cache and Redis -- both
// keyed only by this composite -- give two models sharing one Redis instance (or a caller
// re-using the same x-aibrix-session-key value for both) independent pinning state instead
// of racing to overwrite each other's entry. sessionKey is assumed already validated by
// validSessionKey; the model portion is server-derived (the request's own routing target),
// not client-controlled, so it doesn't need the same bound.
func sessionCacheKey(model, sessionKey string) string {
	return model + sessionCacheKeySeparator + sessionKey
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
	// sessionKeyPods is a Redis read-through cache of cache-key -> pod
	// address pins, where cache-key is sessionKey scoped to a model (see
	// sessionCacheKey) so two models can't collide on one pinning. It is
	// only populated when redisClient is non-nil: without Redis the router
	// stays stateless and rendezvousPod remains the sole (and
	// cross-replica-deterministic) mapping. Idle entries are dropped during
	// the sync loop after sessionAffinityTTL.
	sessionKeyPods sync.Map // cacheKey (string) -> sessionKeyCacheItem

	// sessionKeyPodsSize is an approximate count of entries currently in sessionKeyPods,
	// maintained alongside it (incremented on a genuinely new key in storeSessionKeyLocal,
	// decremented on removal in forgetSessionKey) so the cap in storeSessionKeyLocal doesn't
	// need an O(n) Range over sync.Map on every insert. Accessed only via sync/atomic.
	sessionKeyPodsSize int64

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

// handleSessionKeyCacheSyncMiss reacts to cacheKey being absent from Redis
// during a sync pass. A previously confirmed entry is evicted: Redis no
// longer vouches for it (expired, or another replica re-derived it). An
// entry that was never confirmed means this replica's own write is still
// pending or failed — a nil read says nothing about whether that pinning is
// still valid, so it's retried here instead of being dropped as the only
// surviving copy. Idle unconfirmed entries are evicted rather than retried
// forever.
func (r *sessionAffinityRouter) handleSessionKeyCacheSyncMiss(cacheKey string) {
	cached, found := r.sessionKeyPods.Load(cacheKey)
	if !found {
		return
	}
	item, ok := cached.(sessionKeyCacheItem)
	if !ok || item.confirmed || time.Since(item.storedAt) >= sessionAffinityTTL {
		r.forgetSessionKey(cacheKey)
		return
	}
	r.persistSessionKeyToRedis(cacheKey, item.addr, writeClaim)
}

// sessionAffinityWrite describes what the caller knows about any existing Redis value for the
// key it is about to persist. Only writeClaim changes the write today (SET NX); the other
// modes carry the caller's intent for the gating and compare-and-swap that land in the follow-ups
// described below.
type sessionAffinityWrite int

const (
	writeClaim sessionAffinityWrite = iota
	writeRefresh
	writeRepin
)

// persistSessionKeyToRedis write-throughs cacheKey -> addr with a sliding
// TTL: every call, including a refresh of an unchanged pinning, pushes the
// idle-expiry another sessionAffinityTTL out. Intended to be called via `go`
// at the routing call sites so a slow/unavailable Redis never adds latency to
// the request path; a failed write just means this replica's local cache is,
// for now, the only copy of this pinning. Returns whether Redis now has it (for writeClaim:
// whether this replica's claim won).
//
// TODO(session-affinity): claims use SET NX EX and a lost claim now reads the winner back, and
// PostRouteUpdate's blended-final-target write now repins via compare-and-swap (see
// commitFinalTarget and sessionKeyRepinScript), but plain SET is still shared by TTL-only
// refreshes and by resolveSessionPod's own repin of a stale local/Redis entry onto a fresh
// rendezvous pick: a refresh must touch the TTL only while the stored value still matches addr,
// or a delayed refresh can revert a newer repin.
// See https://github.com/vllm-project/aibrix/pull/2742#discussion_r4037407790.
func (r *sessionAffinityRouter) persistSessionKeyToRedis(cacheKey, addr string, mode sessionAffinityWrite) bool {
	if r.redisClient == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionKeyRedisWriteTimeout)
	defer cancel()
	if mode == writeClaim {
		claimed, err := r.redisClient.SetNX(ctx, sessionAffinityRedisKey(cacheKey), addr, sessionAffinityTTL).Result()
		if err != nil {
			klog.V(4).ErrorS(err, "failed to persist session key pinning to redis", "cache_key", cacheKey)
			return false
		}
		if !claimed {
			// Another writer owns this key: read the winner back and converge this
			// replica's local entry on it, so the next request here follows the winner
			// instead of routing from a losing pick until the sync pass.
			r.reconcileLostClaim(cacheKey, addr)
			return false
		}
		r.markSessionKeyConfirmed(cacheKey, addr)
		return true
	}
	if err := r.redisClient.Set(ctx, sessionAffinityRedisKey(cacheKey), addr, sessionAffinityTTL).Err(); err != nil {
		klog.V(4).ErrorS(err, "failed to persist session key pinning to redis", "cache_key", cacheKey)
		return false
	}
	r.markSessionKeyConfirmed(cacheKey, addr)
	return true
}

// reconcileLostClaim converges this replica after its claim for cacheKey lost to another
// writer. The value now in Redis is the winner: when it equals addr, an earlier in-flight
// claim by this replica landed after all, so the local entry is confirmed and the TTL is
// slid forward (the post-route commit paths re-claim their own pin on every request, and
// without the slide an actively used session would expire on the idle clock); otherwise the
// unconfirmed losing entry is replaced with the winner. The replacement only applies while
// the local entry is still that exact losing pick -- a newer local state, such as a fresh
// rendezvous pick after a pod-set change, is left alone. A failed read leaves the entry as
// is; the sync pass or this key's next Redis read converges it then.
func (r *sessionAffinityRouter) reconcileLostClaim(cacheKey, addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionKeyRedisReadTimeout)
	defer cancel()
	winner, err := r.redisClient.Get(ctx, sessionAffinityRedisKey(cacheKey)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			klog.V(4).ErrorS(err, "failed to read back the session key pinning after a lost claim", "cache_key", cacheKey)
		}
		return
	}
	if winner == addr {
		// Confirm the entry and slide the TTL. An EXPIRE cannot overwrite a newer repin
		// the way a SET could; the gated Lua refresh in the TODO is what makes this
		// compare-and-extend atomic later.
		r.markSessionKeyConfirmed(cacheKey, addr)
		if err := r.redisClient.Expire(ctx, sessionAffinityRedisKey(cacheKey), sessionAffinityTTL).Err(); err != nil {
			klog.V(4).ErrorS(err, "failed to extend the session key pinning TTL", "cache_key", cacheKey)
		}
		return
	}
	for {
		cached, ok := r.sessionKeyPods.Load(cacheKey)
		if !ok {
			return
		}
		item, ok := cached.(sessionKeyCacheItem)
		if !ok || item.addr != addr || item.confirmed {
			return
		}
		updated := item
		updated.addr = winner
		updated.confirmed = true
		updated.storedAt = time.Now()
		if r.sessionKeyPods.CompareAndSwap(cacheKey, cached, updated) {
			return
		}
	}
}

// sessionKeyRepinScript atomically moves cacheKey's pinning onto addr (ARGV[2]) when Redis
// still holds the caller's observed oldAddr (ARGV[1]) or the key has since expired, and
// refreshes the TTL when it already holds addr. It leaves any other stored value untouched and
// returns it, so a repin that lost a race against a fresher concurrent write converges the
// local cache onto the winner instead of clobbering it. A Lua script is used because the
// decision has to be atomic with the write: a GET followed by a plain SET could still land
// between another replica's read and write. KEYS[1] is the pinning key, ARGV[3] is the TTL in
// milliseconds.
var sessionKeyRepinScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current or current == ARGV[1] or current == ARGV[2] then
  redis.call('SET', KEYS[1], ARGV[2], 'PX', tonumber(ARGV[3]))
  return ARGV[2]
end
return current
`)

// observeSessionKeyAddr returns this replica's best current knowledge of cacheKey's Redis
// value: the local cache when it has already been confirmed against Redis, or a fresh read
// otherwise. commitFinalTarget uses this as the compare-and-swap basis for a repin, so a call
// that never resolved the key through resolveSessionPod (and so never populated the local
// cache) still gets a same-generation baseline instead of blindly overwriting whatever another
// replica most recently wrote. Returns "" on a miss or a failed read; callers treat that as "no
// existing pinning to preserve."
func (r *sessionAffinityRouter) observeSessionKeyAddr(cacheKey string) string {
	if addr, confirmed, ok := r.loadCachedAddr(cacheKey); ok && confirmed {
		return addr
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionKeyRedisReadTimeout)
	defer cancel()
	val, err := r.redisClient.Get(ctx, sessionAffinityRedisKey(cacheKey)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			klog.V(4).ErrorS(err, "failed to observe session key pinning before repin", "cache_key", cacheKey)
		}
		return ""
	}
	return val
}

// repinSessionKeyInRedis runs sessionKeyRepinScript to move cacheKey from oldAddr to addr and
// reconciles the local cache with whichever address the script reports as the stored value:
// addr on a successful attach/swap, or a different concurrent winner that was left in place.
func (r *sessionAffinityRouter) repinSessionKeyInRedis(cacheKey, oldAddr, addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionKeyRedisWriteTimeout)
	defer cancel()
	ttlMs := strconv.FormatInt(sessionAffinityTTL.Milliseconds(), 10)
	result, err := sessionKeyRepinScript.Run(ctx, r.redisClient,
		[]string{sessionAffinityRedisKey(cacheKey)}, oldAddr, addr, ttlMs).Result()
	if err != nil {
		klog.V(4).ErrorS(err, "failed to repin session key in redis", "cache_key", cacheKey)
		return
	}
	winner, ok := result.(string)
	if !ok {
		return
	}
	r.storeSessionKeyLocal(cacheKey, winner, true)
}

func (r *sessionAffinityRouter) markSessionKeyConfirmed(cacheKey, addr string) {
	for {
		cached, ok := r.sessionKeyPods.Load(cacheKey)
		if !ok {
			return
		}
		item, ok := cached.(sessionKeyCacheItem)
		if !ok || item.addr != addr || item.confirmed {
			return
		}
		updated := item
		updated.confirmed = true
		if r.sessionKeyPods.CompareAndSwap(cacheKey, cached, updated) {
			return
		}
	}
}

// forgetSessionKey removes cacheKey, if present, and keeps sessionKeyPodsSize in step. It uses
// LoadAndDelete rather than a plain Delete so a concurrent double-forget of the same cacheKey
// (e.g. two requests independently finding it stale) only decrements once, for the one call
// that actually removed it.
func (r *sessionAffinityRouter) forgetSessionKey(cacheKey string) {
	if _, loaded := r.sessionKeyPods.LoadAndDelete(cacheKey); loaded {
		atomic.AddInt64(&r.sessionKeyPodsSize, -1)
	}
}

// storeSessionKeyLocal mirrors cacheKey -> addr into the local cache. A
// no-op when Redis isn't configured (the cache is a Redis read-through, not
// a pin of its own), or when cacheKey is new and the cache is already at
// maxSessionKeyPodsEntries (see its doc comment). Same-addr stores refresh
// storedAt and only raise confirmed, so a TTL-refresh does not un-confirm a
// pin whose Redis write already landed. cacheKey is assumed already bounded
// by validSessionKey on its caller-provided portion (see sessionCacheKey)
// -- rechecking the composite length here would wrongly reject a valid
// session key once the model prefix pushes the composite past
// maxSessionKeyLen.
func (r *sessionAffinityRouter) storeSessionKeyLocal(cacheKey, addr string, confirmed bool) {
	if r.redisClient == nil {
		return
	}
	for {
		existing, ok := r.sessionKeyPods.Load(cacheKey)
		if !ok {
			if atomic.LoadInt64(&r.sessionKeyPodsSize) >= int64(maxSessionKeyPodsEntries) {
				klog.V(4).InfoS("session-affinity local cache at capacity, not caching new key locally", "limit", maxSessionKeyPodsEntries)
				return
			}
			newItem := sessionKeyCacheItem{addr: addr, confirmed: confirmed, storedAt: time.Now()}
			if _, loaded := r.sessionKeyPods.LoadOrStore(cacheKey, newItem); !loaded {
				atomic.AddInt64(&r.sessionKeyPodsSize, 1)
				return
			}
			continue
		}
		item, ok := existing.(sessionKeyCacheItem)
		if !ok {
			newItem := sessionKeyCacheItem{addr: addr, confirmed: confirmed, storedAt: time.Now()}
			if r.sessionKeyPods.CompareAndSwap(cacheKey, existing, newItem) {
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
			if r.sessionKeyPods.CompareAndSwap(cacheKey, existing, updated) {
				return
			}
			continue
		}
		newItem := sessionKeyCacheItem{addr: addr, confirmed: confirmed, storedAt: time.Now()}
		if r.sessionKeyPods.CompareAndSwap(cacheKey, existing, newItem) {
			return
		}
	}
}

// rememberSessionKey commits sessionKey -> addr locally (when Redis is
// configured) and write-throughs to Redis in the background, under a cache
// key scoped to ctx's model (see sessionCacheKey).
func (r *sessionAffinityRouter) rememberSessionKey(ctx *types.RoutingContext, sessionKey, addr string, mode sessionAffinityWrite) {
	if r.redisClient == nil || !validSessionKey(sessionKey) {
		return
	}
	cacheKey := sessionCacheKey(ctx.Model, sessionKey)
	r.storeSessionKeyLocal(cacheKey, addr, false)
	go r.persistSessionKeyToRedis(cacheKey, addr, mode)
}

// loadCachedAddr returns the locally cached address for cacheKey. confirmed reports whether
// Redis is known to hold that same pinning; an entry that is not confirmed is a write this
// replica has not seen land (or has lost), so callers must treat it as a claim rather than
// as an authoritative refresh.
func (r *sessionAffinityRouter) loadCachedAddr(cacheKey string) (address string, confirmed bool, ok bool) {
	cached, ok := r.sessionKeyPods.Load(cacheKey)
	if !ok {
		return "", false, false
	}
	item, ok := cached.(sessionKeyCacheItem)
	if !ok {
		r.forgetSessionKey(cacheKey)
		return "", false, false
	}
	return item.addr, item.confirmed, true
}

// readSessionKeyFromRedis looks up cacheKey's pinning in Redis on a local
// cache miss, so a session first pinned by a different gateway replica is
// honored here too. Bounded by sessionKeyRedisReadTimeout so an unresponsive
// Redis degrades to rendezvousPod rather than stalling the request.
func (r *sessionAffinityRouter) readSessionKeyFromRedis(ctx *types.RoutingContext, cacheKey string) (string, bool) {
	if r.redisClient == nil {
		return "", false
	}
	readCtx, cancel := context.WithTimeout(context.Background(), sessionKeyRedisReadTimeout)
	defer cancel()
	val, err := r.redisClient.Get(readCtx, sessionAffinityRedisKey(cacheKey)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			klog.V(4).ErrorS(err, "failed to read session key pinning from redis", "request_id", ctx.RequestID, "cache_key", cacheKey)
		}
		return "", false
	}
	return val, true
}

// resolveSessionPod finds the pod this request's session should be pinned
// to, checking in order: an exact address match on the session-ID header, a
// cached or Redis-backed session-key pinning, and rendezvous hashing on the
// session key. via reports which of those resolved it, for logging only; mode is the write
// intent for persisting the pick (see sessionAffinityWrite). A cached pinning whose Redis
// write is not confirmed still reports writeClaim: it has no verified Redis value to refresh.
//
// The local cache and Redis lookups are scoped to ctx.Model (see
// sessionCacheKey) so two models sharing a session-key value, or a Redis
// instance, don't collide on the same pinning; rendezvous hashing itself
// stays keyed on the raw session key since its candidate pod list (pods) is
// already model-specific.
//
// It only touches the local sessionKeyPods cache (evicting a stale entry, or
// mirroring a Redis hit into it) -- refreshing the Redis TTL and committing
// to this pod as the final decision is the caller's job: Route commits
// immediately, while ScoreAll must not, since a blended competitor could
// still win (see PostRouteUpdate).
func (r *sessionAffinityRouter) resolveSessionPod(ctx *types.RoutingContext, pods []*v1.Pod) (pod *v1.Pod, sessionKey string, via string, mode sessionAffinityWrite) {
	if sessionID := ctx.ReqHeaders[constants.HeaderSessionID]; sessionID != "" {
		decoded, err := base64.StdEncoding.DecodeString(sessionID)
		if err != nil {
			klog.V(4).ErrorS(err, "Invalid session ID format", "request_id", ctx.RequestID)
		} else if p := findReadyPodByAddr(ctx, pods, string(decoded)); p != nil {
			return p, "", "session-id", writeClaim
		}
	}

	sessionKey = ctx.ReqHeaders[constants.HeaderSessionKey]
	if !validSessionKey(sessionKey) {
		return nil, "", "", writeClaim
	}
	cacheKey := sessionCacheKey(ctx.Model, sessionKey)

	var cachedAddr string
	if addr, confirmed, ok := r.loadCachedAddr(cacheKey); ok {
		cachedAddr = addr
		if p := findReadyPodByAddr(ctx, pods, cachedAddr); p != nil {
			if confirmed {
				return p, sessionKey, "session-key-cache", writeRefresh
			}
			// An unconfirmed entry is still this replica's claim: Redis has not vouched
			// for it yet, so persisting it must not use the unconditional write.
			return p, sessionKey, "session-key-cache", writeClaim
		}
		r.forgetSessionKey(cacheKey)
	}

	// Local miss: check Redis before falling back to rendezvousPod, so a
	// pinning made by another gateway replica is still honored. Skipped
	// when Redis agrees with the just-invalidated local cache, since that
	// pod is already known to be unready.
	staleAddr := cachedAddr
	if redisAddr, ok := r.readSessionKeyFromRedis(ctx, cacheKey); ok && redisAddr != cachedAddr {
		if p := findReadyPodByAddr(ctx, pods, redisAddr); p != nil {
			r.storeSessionKeyLocal(cacheKey, redisAddr, true)
			return p, sessionKey, "session-key-redis", writeRefresh
		}
		staleAddr = redisAddr
	}

	// A rendezvous pick either creates a brand-new pinning, or replaces one whose recorded pod
	// is not routable from this replica's ready view -- only the former may use the
	// non-destructive claim write.
	mode = writeClaim
	if staleAddr != "" {
		mode = writeRepin
	}
	return rendezvousPod(ctx, pods, sessionKey), sessionKey, "rendezvous", mode
}

// Route implements session affinity by attempting to route requests to the same pod
// using a session ID stored in the request header. The session ID encodes the target
// pod's address as "IP:Port". If no valid session exists, it falls back to a randomly selected ready pod.
func (r *sessionAffinityRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	pods := readyPodList.All()

	var pod *v1.Pod
	var sessionKey, via string
	var mode sessionAffinityWrite
	if ctx.ReqHeaders == nil {
		klog.V(4).InfoS("No request or headers, skipping session affinity", "request_id", ctx.RequestID)
	} else {
		pod, sessionKey, via, mode = r.resolveSessionPod(ctx, pods)
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
		r.rememberSessionKey(ctx, sessionKey, addr, mode)
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

	if pod, _, _, _ := r.resolveSessionPod(ctx, pods); pod != nil {
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
	if sessionKey := ctx.ReqHeaders[constants.HeaderSessionKey]; validSessionKey(sessionKey) && r.redisClient != nil {
		cacheKey := sessionCacheKey(ctx.Model, sessionKey)
		r.storeSessionKeyLocal(cacheKey, addr, false)
		go r.commitFinalTarget(cacheKey, addr)
	}
	return nil
}

// commitFinalTarget persists PostRouteUpdate's final target pod for cacheKey, off the request
// path. This call site never resolved sessionKey through resolveSessionPod -- the caller may
// have been routed here by an unrelated decision, such as the gateway's single-remaining-
// candidate fast path after a load-imbalance gate excluded the pinned pod -- so, unlike
// rememberSessionKey's claim write, it cannot assume no pinning exists yet. It observes the
// current pinning first and repins atomically (see repinSessionKeyInRedis) when that
// observation differs from addr, so a genuine existing pin moves onto addr instead of losing a
// doomed SET NX and leaving Redis pointed at a pod this request never used. When the
// observation already matches addr, or found nothing to preserve, the plain claim write already
// handles both cases correctly.
func (r *sessionAffinityRouter) commitFinalTarget(cacheKey, addr string) {
	oldAddr := r.observeSessionKeyAddr(cacheKey)
	if oldAddr == "" || oldAddr == addr {
		r.persistSessionKeyToRedis(cacheKey, addr, writeClaim)
		return
	}
	r.repinSessionKeyInRedis(cacheKey, oldAddr, addr)
}
