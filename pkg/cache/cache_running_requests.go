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

package cache

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// runningRequestsKeyPrefix namespaces the real-time cross-gateway running-request
	// counters in Redis -- distinct from the per-gateway-instance keys in
	// cache_gateway_snapshot.go, which this counter does not use or replace.
	runningRequestsKeyPrefix = "aibrix:rrq"
	// runningRequestsGatewaysKey is a single sorted set recording the last heartbeat
	// timestamp (ms) of every gateway instance that has this counter active. It is
	// what makes crash recovery correct: see the package doc below for why a shared
	// per-pod counter cannot rely on that pod-hash key's own TTL for crash detection.
	runningRequestsGatewaysKey = runningRequestsKeyPrefix + ":gateways"
	// runningRequestsTTL bounds how long a per-pod hash key survives with no writes
	// from ANY gateway -- pure keyspace hygiene (garbage-collecting pods nobody
	// routes to any more), refreshed on every write. It does NOT bound crash
	// recovery -- see runningRequestsLivenessWindow for that.
	runningRequestsTTL = 10 * time.Minute
	// runningRequestsWriteTimeout bounds each detached write (the increment/decrement
	// Lua call, the liveness ZADD, and one coalesced hygiene pass) so a stuck Redis
	// can't leak goroutines. Writes are fire-and-forget with respect to the request
	// path, so this can be generous.
	runningRequestsWriteTimeout = 1 * time.Second
	// runningRequestsLivenessHeartbeatInterval is how often each gateway instance
	// refreshes its own entry in runningRequestsGatewaysKey, independent of request
	// traffic (so a gateway that is alive but has zero in-flight requests anywhere
	// still stays live).
	runningRequestsLivenessHeartbeatInterval = 1 * time.Second
	// runningRequestsLivenessWindow is how far back a gateway's last heartbeat may be
	// and still count as live -- 3x the heartbeat interval (3s) tolerates a missed
	// beat (GC pause, brief network blip) before excluding it. A gateway that misses
	// this window (crashed, or a slow shutdown) has its per-pod hash-field
	// contribution excluded from every read within this bound, regardless of how
	// much traffic OTHER gateways keep sending to the same pod -- unlike relying on
	// the pod-hash key's own TTL, which any other gateway's writes would keep
	// refreshing forever.
	runningRequestsLivenessWindow = 3 * runningRequestsLivenessHeartbeatInterval
	// runningRequestsFieldPruneWindow is how long a gateway must be absent from
	// runningRequestsGatewaysKey before its per-pod hash fields are treated as safe
	// to delete outright (see flushPendingRunningRequestsPrunes) -- distinct from
	// runningRequestsLivenessWindow, which only governs exclusion from a sum.
	// Exclusion is cheap to get wrong: the very next heartbeat undoes it. Deleting a
	// field is not -- it discards that field's accumulated count permanently, with
	// no way for the owning gateway to re-assert it (it only ever sends +1/-1
	// deltas, never "set my count to N") -- so it needs a much larger safety margin
	// against false positives. A GC pause, a dropped heartbeat write, or a brief
	// network blip must never look "gone" long enough to trigger a delete.
	// Deliberately the same duration as runningRequestsTTL: past this point the hash
	// key itself would eventually have idled out anyway had nothing else been
	// writing to it, so a field this stale is at least as old as data this system
	// already treats as garbage-collectable.
	runningRequestsFieldPruneWindow = runningRequestsTTL
	// runningRequestsDriftConfirmPasses is how many consecutive hygiene passes (one per
	// runningRequestsLivenessHeartbeatInterval, so about 5s) must see this gateway's own
	// Redis field differ from its local count in the same direction before
	// reconcileOwnRunningRequests treats the gap as drift and corrects it. The async
	// increment/decrement writes make the two disagree for a few milliseconds around
	// every request; drift is a gap that outlives them.
	runningRequestsDriftConfirmPasses = 5
	// runningRequestsDriftReadBatchSize limits how many HMGET commands share one pipeline
	// Exec in reconcileOwnRunningRequests, for the same reason as
	// gatewaySnapshotHGetAllBatchSize: one pipeline over every pod blocks Redis and spikes
	// client memory in a large cluster.
	runningRequestsDriftReadBatchSize = 100
)

// runningRequestsReadTimeout bounds each read (GetPodRunningRequests /
// GetPodsRunningRequests) and the admission round trip in admitRunningRequest, all of
// which sit on the request/admission/scrape path synchronously. Kept short so a slow or
// unreachable Redis degrades routing quality (falls back to the local atomic counter)
// rather than adding seconds of latency to every request.
//
// A var rather than a const only so tests can widen it: a test that drives many
// concurrent admissions against a single-threaded test Redis can exhaust a budget this
// short, and admitRunningRequest's documented fail-open path then admits past the cap
// for a reason that test is not about. Production never reassigns it.
var runningRequestsReadTimeout = 100 * time.Millisecond

// newRunningRequestsGatewayInstanceID derives the identity this process uses for its
// hash field / liveness ZSET member from podName, salted with this process's start
// time. POD_NAME alone is not enough: an in-place container restart (OOM kill, panic,
// kubelet-initiated restart) keeps the same POD_NAME, so a new process would otherwise
// immediately re-adopt the crashed process's still-"live" liveness entry the moment it
// heartbeats -- and since the new process only ever decrements requests it itself
// incremented, the crashed process's unpaired increments would then never be excluded
// by runningRequestsLivenessWindow, leaking forever (any further traffic through the
// same pod keeps the hash key's hygiene TTL alive too). Salting with a start-time nonce
// means the old process's ID simply ages out like any other crashed gateway's, even
// though POD_NAME repeats.
func newRunningRequestsGatewayInstanceID(podName string) string {
	return podName + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// runningRequestsGatewayInstanceID is this process's identity for the running-requests
// hash field and liveness ZSET member -- see newRunningRequestsGatewayInstanceID. A
// package-level var (computed once, at process start) rather than using gatewayPodName
// directly, so that a same-named restart still gets a fresh identity.
var runningRequestsGatewayInstanceID = newRunningRequestsGatewayInstanceID(gatewayPodName)

// runningRequestsLivenessStarted is true while initRunningRequestsLiveness's ticker
// goroutine is running. Tests must not mutate runningRequestsGatewayInstanceID
// while this is set -- the ticker reads it on every beat.
var runningRequestsLivenessStarted atomic.Bool

// runningRequestsIncrDecrScript atomically applies one gateway's delta to its own
// field in a pod's running-requests hash, clamps that field at zero (defense in
// depth against an unpaired decrement slipping through -- see
// addPodStats/donePodStats's increment/decrement pairing), and refreshes the whole
// hash key's hygiene TTL -- all in one round trip so a dropped connection can never
// apply the HINCRBY without the PEXPIRE (which would otherwise risk an
// immortal key). HINCRBY is commutative, so this needs no read-modify-write guard: a
// delta lands correctly regardless of what other deltas from the same gateway land
// immediately before or after it. reconcileOwnRunningRequests relies on exactly this to
// apply a confirmed drift correction of any magnitude through this same script, not only
// the per-request +1/-1.
//
// A negative delta against a hash key that doesn't exist is a no-op instead of
// creating it: runningRequestsTTL only bounds idle keys, not in-flight requests (the
// TTL is refreshed on writes, not on how long a request has been running), so a
// request that outlives it can find its pod's hash key already expired by the time it
// completes. Letting HINCRBY recreate that key here would resurrect it holding only a
// lying zero for this gateway's field -- indistinguishable, to a reader, from a
// genuine zero -- which can then mask another still-in-flight request to the same pod
// that has no other write to re-expose it. Leaving the key absent instead preserves
// the existing ok=false-on-missing-hash behavior in readPodRunningRequests, which
// correctly falls back to the local atomic counter.
//
// KEYS[1] = the pod's running-requests hash key
// ARGV[1] = field name (this gateway's instance ID)
// ARGV[2] = delta -- "1"/"-1" for a normal request, or a confirmed drift correction of
// any magnitude (see reconcileOwnRunningRequests)
// ARGV[3] = hash key TTL in milliseconds
const runningRequestsIncrDecrScript = `
if tonumber(ARGV[2]) < 0 and redis.call('EXISTS', KEYS[1]) == 0 then
  return -1
end
local newVal = redis.call('HINCRBY', KEYS[1], ARGV[1], ARGV[2])
if newVal < 0 then
  redis.call('HSET', KEYS[1], ARGV[1], 0)
  newVal = 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return newVal
`

// runningRequestsKey returns the Redis key for a pod's running-requests hash: field
// = gateway instance ID, value = that gateway's own running-request count for the
// pod (see runningRequestsIncrDecrScript). A read sums the fields belonging to
// currently-live gateways (see readPodRunningRequests) -- unlike a single shared
// integer INCR/DECR'd by every gateway, a dead gateway's field can be identified and
// excluded independently of whatever other gateways keep doing to the same key.
func runningRequestsKey(namespace, name string) string {
	return runningRequestsKeyPrefix + ":" + utils.GeneratePodKey(namespace, name)
}

// incrPodRunningRequests applies this gateway's +1 to pod (namespace, name)'s
// running-requests hash and reports whether it succeeded. Called from a goroutine
// already spawned by addPodStats (see cache_trace.go) -- not itself
// fire-and-forget, so its caller can gate the matching decrement on success (see
// donePodStats): an increment that never landed must never be paired with a
// decrement, or the count would undercount by one for the rest of that key's life.
func (c *Store) incrPodRunningRequests(namespace, name string) bool {
	if c.redisClient == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), runningRequestsWriteTimeout)
	defer cancel()
	key := runningRequestsKey(namespace, name)
	if _, err := c.redisClient.Eval(ctx, runningRequestsIncrDecrScript, []string{key},
		runningRequestsGatewayInstanceID, "1", strconv.FormatInt(runningRequestsTTL.Milliseconds(), 10)).Result(); err != nil {
		klog.V(4).ErrorS(err, "failed to increment running-requests counter", "key", key)
		return false
	}
	return true
}

// decrPodRunningRequests applies this gateway's -1 to pod (namespace, name)'s
// running-requests hash. Only ever called after the matching incrPodRunningRequests
// call is known to have succeeded (see donePodStats waiting on
// podStatsRecord.runningReqIncrDone) -- see incrPodRunningRequests's doc comment for
// why an unpaired decrement must be avoided.
func (c *Store) decrPodRunningRequests(namespace, name string) {
	if c.redisClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), runningRequestsWriteTimeout)
	defer cancel()
	key := runningRequestsKey(namespace, name)
	if _, err := c.redisClient.Eval(ctx, runningRequestsIncrDecrScript, []string{key},
		runningRequestsGatewayInstanceID, "-1", strconv.FormatInt(runningRequestsTTL.Milliseconds(), 10)).Result(); err != nil {
		klog.V(4).ErrorS(err, "failed to decrement running-requests counter", "key", key)
	}
}

// runningRequestsAdmitScript atomically enforces a hard per-pod concurrency cap: it sums
// the same live-gateway-filtered fields sumLiveFields/readPodRunningRequests would report
// and, only if that sum is still under the limit, applies this gateway's own +1 to the
// same hash incrPodRunningRequests/decrPodRunningRequests use -- all in one round trip.
// This is what makes requestsInflight (see enforceReplicaInflight in
// pkg/plugins/gateway/gateway_inflight.go) an actual hard cap: a plain read (the live sum)
// followed by a *later*, separately-fired increment -- which is what this replaced -- lets
// any number of concurrent requests to the same pod all observe the same pre-increment sum
// and all get admitted, no matter how tight the limit, because none of their increments have
// landed yet by the time the others check.
//
// This gateway's own field in the hash can be stale in either direction, and unlike
// overlaySelfRunningRequests on the read side, the fix is a max() against localRunning, not
// an outright substitution -- because which side leads depends on which path produced the
// pending write:
//
//   - A request that didn't go through this admit path (no cap configured for it, or it hit
//     the AdmitPodRunningRequest error/fail-open case) reaches the hash via
//     incrPodRunningRequests, fired async from addPodStats (cache_trace.go) -- fire-and-
//     forget with respect to the request path. localRunning (already incremented
//     synchronously before that call) leads the hash here, so the raw field would
//     undercount until the HINCRBY lands, and could admit past the cap.
//   - A request THIS script itself just admitted is the mirror image: the HINCRBY below
//     lands synchronously as part of that earlier call, but the matching increment to
//     localRunning only happens later, downstream in addPodStats (which runs after
//     enforceReplicaInflight returns). So immediately after such an admit, the hash field
//     leads localRunning -- substituting localRunning here, as overlaySelfRunningRequests
//     does for reads, would discard an already-confirmed admission and reopen the exact
//     double-admission race this script exists to close.
//
// max() gets both right: it picks up a not-yet-landed plain increment same as a
// substitution would, but never regresses below a hash value this script itself already
// committed. The only cost is the equivalent lag on a decrement (localRunning, decremented
// synchronously in donePodStats, can briefly read lower than the hash before the matching
// decrPodRunningRequests call lands -- see decrCrossGatewayRunningRequests) -- max() then
// keeps the still-live hash value, which can only make this check more conservative
// (a spurious rejection right after a request completes), never less. Other gateways'
// fields are trusted as-is -- there is no local signal for them.
//
// KEYS[1] = the pod's running-requests hash key
// KEYS[2] = runningRequestsGatewaysKey (the liveness ZSET)
// ARGV[1] = field name (this gateway's instance ID)
// ARGV[2] = live-gateway cutoff (unix ms, inclusive lower bound) -- see liveGatewaysCutoffMillis
// ARGV[3] = inflight limit
// ARGV[4] = hash key TTL in milliseconds
// ARGV[5] = this gateway's local atomic running-request count for the pod
// Returns {1, sum} and increments if sum < limit, else {0, sum} with no increment -- sum is
// always the live total *before* this call's own increment (if any).
const runningRequestsAdmitScript = `
local live = redis.call('ZRANGEBYSCORE', KEYS[2], ARGV[2], '+inf')
local liveSet = {}
for _, gw in ipairs(live) do
  liveSet[gw] = true
end
local selfField = ARGV[1]
local selfLocal = tonumber(ARGV[5]) or 0
local selfSeen = false
local fields = redis.call('HGETALL', KEYS[1])
local sum = 0
for i = 1, #fields, 2 do
  if liveSet[fields[i]] then
    local v = tonumber(fields[i + 1]) or 0
    if fields[i] == selfField then
      selfSeen = true
      if selfLocal > v then
        v = selfLocal
      end
    end
    if v > 0 then
      sum = sum + v
    end
  end
end
if not selfSeen and selfLocal > 0 then
  sum = sum + selfLocal
end
if sum >= tonumber(ARGV[3]) then
  return {0, sum}
end
redis.call('HINCRBY', KEYS[1], ARGV[1], 1)
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return {1, sum}
`

// admitRunningRequest is the atomic, hard-cap counterpart of readPodRunningRequests -- see
// runningRequestsAdmitScript. This sits on the request/admission path (like
// readPodRunningRequests, unlike the fire-and-forget incr/decr), so it uses the same short
// runningRequestsReadTimeout: an admission decision must never add meaningful latency to
// routing. For an admitted request, counted is true when the script's +1 is, or may be, in
// the hash: a confirmed increment, or an Eval error after which the script may have run (see
// admitScriptDidNotRun). A nil client, a pre-send failure and, as a fallback policy, an
// unusable reply leave the increment to normal request tracking.
//
// localRunning is the caller's already-known local atomic count for the pod (addPodStats
// will still increment it normally regardless of this call's outcome -- that counter is
// local bookkeeping, not part of this Redis-backed cap). Two roles: (1) the fallback
// decision when Redis is unavailable -- failing open to a local-only decision (rather than
// blocking admission, or admitting unconditionally) matches this package's
// degrade-gracefully-under-a-slow/dead-Redis philosophy everywhere else, at the cost of the
// cap being only best-effort for the duration of an actual outage; (2) passed into
// runningRequestsAdmitScript as a floor on this gateway's own hash field -- a max(), not a
// substitution like overlaySelfRunningRequests uses for reads, because that field's own
// prior writes can come from this very script, which updates it before localRunning
// catches up -- see the script's doc comment for why an unconditional substitution would
// be wrong here specifically.
func (c *Store) admitRunningRequest(namespace, name string, limit, localRunning int64) (admitted, counted bool, running int64) {
	if c.redisClient == nil {
		return localRunning < limit, false, localRunning
	}
	ctx, cancel := context.WithTimeout(context.Background(), runningRequestsReadTimeout)
	defer cancel()
	key := runningRequestsKey(namespace, name)
	client := c.admitRedisClient
	if client == nil {
		// Test stores built without New may supply their own client and hooks here. This path
		// does not disable retries; the pre-send error classification requires that no earlier
		// Eval attempt ran.
		client = c.redisClient
	}
	res, err := client.Eval(ctx, runningRequestsAdmitScript, []string{key, runningRequestsGatewaysKey},
		runningRequestsGatewayInstanceID, c.liveGatewaysCutoffMillis(), strconv.FormatInt(limit, 10),
		strconv.FormatInt(runningRequestsTTL.Milliseconds(), 10), strconv.FormatInt(localRunning, 10)).Result()
	if err != nil {
		klog.V(4).ErrorS(err, "failed to atomically admit against running-requests inflight cap; falling back to local count", "key", key)
		// After a timeout or a dropped connection the script may still have applied its +1, so
		// keep treating it as applied: completion's one decrement then removes it. If the script
		// never ran, that decrement undercounts a positive field; zero-clamping prevents negative
		// values but does not guarantee recovery under sustained traffic. A second increment
		// instead would leave a +1 that nothing removes.
		admitted = localRunning < limit
		return admitted, admitted && !admitScriptDidNotRun(err), localRunning
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 2 {
		klog.V(4).InfoS("unexpected running-requests admit script result shape; falling back to local count", "key", key)
		return localRunning < limit, false, localRunning
	}
	admittedFlag, flagOK := arr[0].(int64)
	running, runningOK := arr[1].(int64)
	if !flagOK || !runningOK || (admittedFlag != 0 && admittedFlag != 1) {
		klog.V(4).InfoS("unexpected running-requests admit script result values; falling back to local count", "key", key)
		return localRunning < limit, false, localRunning
	}
	return admittedFlag == 1, admittedFlag == 1, running
}

// admitScriptDidNotRun reports whether an error from the single-attempt admit Eval (see
// admitRunningRequestsClient) shows the script never ran: the client is closed, or no
// connection could be dialed. Every other error, including a Redis error reply (the script can
// fail after its HINCRBY), may follow a script that already applied its +1.
func admitScriptDidNotRun(err error) bool {
	if errors.Is(err, redis.ErrClosed) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// admitRunningRequestsClient derives the client admitRunningRequest uses from the shared one,
// with go-redis' own retry loop switched off (as asyncJobStoreClient does for its store). The
// admit script is not idempotent: a retry after an attempt that ran but lost its reply applies a
// second +1, and the error that comes back describes only the last attempt. MaxRetries is -1
// because go-redis reads 0 as "use the default".
func admitRunningRequestsClient(client *redis.Client) *redis.Client {
	if client == nil {
		return nil
	}
	options := *client.Options()
	options.MaxRetries = -1
	return redis.NewClient(&options)
}

// heartbeatRunningRequestsLiveness refreshes this gateway instance's entry in
// runningRequestsGatewaysKey. Driven by a periodic ticker (initRunningRequestsLiveness)
// rather than by request traffic, so a gateway that is alive but idle -- zero
// in-flight requests anywhere -- still reports live and isn't mistaken for crashed.
//
// The ZADD (writeRunningRequestsLivenessHeartbeat) is the only work that stays on
// the ticker goroutine: time.Ticker drops ticks while the handler is still running,
// and a slow hygiene pass must not stretch the inter-ZADD gap into
// runningRequestsLivenessWindow. PEXPIRE of in-flight pod hashes and draining
// pending field-prunes run afterwards on one coalesced goroutine
// (scheduleRunningRequestsHygiene) -- at most one of those is in flight, so a slow
// Redis cannot stack work or fan out with QPS.
//
// ZREMRANGEBYSCORE (inside the ZADD pipeline) trims gateway IDs that fell out of
// runningRequestsGatewaysKey entirely -- without it the set only ever grows (ZADD
// adds, but nothing ever removes), so every gateway instance that ever existed on
// this cluster stays a member forever. This deliberately uses
// prunableGatewaysCutoffMillis (the same long window flushPendingRunningRequestsPrunes
// checks), NOT the short liveGatewaysCutoffMillis used for sum-exclusion: a ZSET
// member is this package's only record of a gateway's last heartbeat, and the prune
// flush needs that record to survive as long as it might still need to protect a
// hash field from deletion. Removing it at the short 3s cutoff instead would erase
// the evidence that a gateway merely had a brief hiccup (vs. actually being gone),
// making every hiccup indistinguishable from a real crash to that check.
func (c *Store) heartbeatRunningRequestsLiveness() {
	if c.redisClient == nil {
		return
	}
	c.writeRunningRequestsLivenessHeartbeat()
	c.scheduleRunningRequestsHygiene()
}

// writeRunningRequestsLivenessHeartbeat is the must-succeed half of a heartbeat tick:
// ZADD this instance, refresh the ZSET's hygiene TTL, and trim members older than
// runningRequestsFieldPruneWindow. Kept synchronous on the ticker so a live gateway
// keeps its liveness score fresh even when the async hygiene pass is slow or skipped.
//
// The ZADD score and the prune cutoff both use c.redisNowMillis() (this process's
// clock, corrected by the offset from the last successful sync) rather than raw
// time.Now() -- and that same offset is what every reader uses to compute
// liveGatewaysCutoffMillis/prunableGatewaysCutoffMillis (see redisNowMillis) -- so a
// gateway pod whose local clock has drifted from Redis's still gets judged against
// the same clock everyone else is using. TIME is piggybacked on this pipeline (one
// extra command on an already off-hot-path, once-per-second round trip, not a new
// round trip) and its result re-syncs the offset for use starting with the *next*
// tick -- this tick's own score/cutoff necessarily still use the prior sync, but at
// worst that is runningRequestsLivenessHeartbeatInterval stale, which only
// re-introduces this process's own clock *drift rate* (not the skew being
// corrected for) over that one second -- negligible next to
// runningRequestsLivenessWindow.
func (c *Store) writeRunningRequestsLivenessHeartbeat() {
	ctx, cancel := context.WithTimeout(context.Background(), runningRequestsWriteTimeout)
	defer cancel()
	pipe := c.redisClient.Pipeline()
	timeCmd := pipe.Time(ctx)
	pipe.ZAdd(ctx, runningRequestsGatewaysKey, redis.Z{Score: float64(c.redisNowMillis()), Member: runningRequestsGatewayInstanceID})
	pipe.PExpire(ctx, runningRequestsGatewaysKey, runningRequestsTTL)
	pipe.ZRemRangeByScore(ctx, runningRequestsGatewaysKey, "-inf", "("+c.prunableGatewaysCutoffMillis())
	if _, err := pipe.Exec(ctx); err != nil {
		klog.V(4).ErrorS(err, "failed to heartbeat running-requests liveness", "gateway", runningRequestsGatewayInstanceID)
		return
	}
	if redisTime, err := timeCmd.Result(); err == nil {
		c.runningRequestsClockOffsetMillis.Store(redisTime.UnixMilli() - time.Now().UnixMilli())
	}
}

// scheduleRunningRequestsHygiene runs the best-effort half of a heartbeat tick off
// the ticker: PEXPIRE in-flight pod hashes, flush enqueued field-prunes, then repair
// any of this gateway's own fields that have drifted from its local counts.
// CompareAndSwap skips if a previous pass is still running -- the next tick retries
// -- so hygiene cannot stack goroutines or delay the following ZADD.
func (c *Store) scheduleRunningRequestsHygiene() {
	if !c.runningRequestsHygieneBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.runningRequestsHygieneBusy.Store(false)
		c.refreshInFlightPodHashTTLs()
		c.flushPendingRunningRequestsPrunes()
		c.reconcileOwnRunningRequests()
	}()
}

// refreshInFlightPodHashTTLs PEXPIREs every pod this gateway currently has in-flight
// (local atomic > 0). runningRequestsTTL otherwise only extends on incr/decr writes,
// so a single request running longer than that with no other traffic to its pod
// would otherwise find its own hash key expired by the time it completes -- see
// runningRequestsIncrDecrScript. Failure here is retried on the next hygiene pass;
// incr/decr still refresh TTL on their own writes. Must not run on the ticker
// goroutine (see scheduleRunningRequestsHygiene).
func (c *Store) refreshInFlightPodHashTTLs() {
	ctx, cancel := context.WithTimeout(context.Background(), runningRequestsWriteTimeout)
	defer cancel()
	pipe := c.redisClient.Pipeline()
	n := 0
	c.metaPods.Range(func(_ string, pod *Pod) bool {
		if atomic.LoadInt32(&pod.runningRequests) > 0 {
			pipe.PExpire(ctx, runningRequestsKey(pod.Namespace, pod.Name), runningRequestsTTL)
			n++
		}
		return true
	})
	if n == 0 {
		return
	}
	if _, err := pipe.Exec(ctx); err != nil {
		klog.V(4).ErrorS(err, "failed to refresh in-flight running-requests hash TTLs", "pod_count", n)
	}
}

// runningRequestsDriftState is one pod's progress toward confirming that this gateway's
// own Redis field has drifted from its local count -- see reconcileOwnRunningRequests.
type runningRequestsDriftState struct {
	// direction is +1 while the Redis field is above the local count and -1 while it
	// is below; 0 means no gap is being tracked.
	direction int
	// passes is how many consecutive passes have seen a gap in direction.
	passes int
	// minGap is the smallest gap seen over those passes: the part of the difference
	// that never went away, and so the part safe to correct.
	minGap int64
}

// observe folds one sample of (Redis field, local count) into the state. It returns the
// next state and the correction to add to the Redis field, which is zero until the gap
// has persisted for runningRequestsDriftConfirmPasses passes in one direction. A gap
// that closes, or flips direction, starts over: that is in-flight writes, not drift.
func (s runningRequestsDriftState) observe(redisCount, local int64) (runningRequestsDriftState, int64) {
	gap := redisCount - local
	if gap == 0 {
		return runningRequestsDriftState{}, 0
	}
	direction := 1
	if gap < 0 {
		direction, gap = -1, -gap
	}
	if s.direction != direction {
		s = runningRequestsDriftState{direction: direction, minGap: gap}
	} else if gap < s.minGap {
		s.minGap = gap
	}
	s.passes++
	if s.passes < runningRequestsDriftConfirmPasses {
		return s, 0
	}
	return runningRequestsDriftState{}, -int64(direction) * s.minGap
}

// reconcileOwnRunningRequests repairs this gateway's own field in each pod's
// running-requests hash when it has drifted from the local count.
//
// A field only ever receives +1/-1 deltas, one pair per request, so a delta that is lost
// (a write that times out, which decrPodRunningRequests logs at V(4) and does not retry)
// leaves the field wrong for the rest of this process's life: nothing ever sets it back
// to the true count. This gateway cannot see the error, because overlaySelfRunningRequests
// substitutes the local count for its own field on every read, but every other gateway
// sums the field as-is. On a pod that takes most of the traffic a few such fields are
// enough to make it look busy to the whole fleet and shift its load onto slower replicas.
//
// The local count is the source of truth for this gateway's own share, so a field that
// stays different from it is wrong. A difference seen for a single pass is not evidence:
// the async writes leave the two apart for a few milliseconds around every request. So a
// pod is only corrected once the difference has held in one direction for
// runningRequestsDriftConfirmPasses passes (see runningRequestsDriftState), and only by
// the part that never closed.
//
// The correction is applied with runningRequestsIncrDecrScript, the same unconditional
// HINCRBY every ordinary +1/-1 uses, not a write of the local count and not one guarded
// on the field still holding the value it was sampled at. HINCRBY is commutative, so a
// correction lands correctly whatever real increments or decrements land immediately
// before or after it, with no read-modify-write check required. A guard would only
// reject the correction whenever a real request touched the same field in the gap
// between sampling and writing -- likeliest on exactly the busy pods this exists to fix
// -- and, because a rejected correction resets confirmation to zero, sustained traffic
// to a pod could keep the correction from ever landing.
//
// Runs from the hygiene pass, which runningRequestsHygieneBusy keeps to one at a time,
// so runningRequestsDrift needs no lock. Best-effort: a failed read or write is retried
// on the next pass. The reads go out in batches of runningRequestsDriftReadBatchSize so a
// large pod set does not become one huge pipeline, and a pod whose batch failed keeps the
// drift progress it had rather than starting its confirmation over.
func (c *Store) reconcileOwnRunningRequests() {
	if c.redisClient == nil {
		return
	}
	type sample struct {
		pod   *Pod
		key   string
		local int64
	}
	var samples []sample
	c.metaPods.Range(func(_ string, pod *Pod) bool {
		samples = append(samples, sample{
			pod:   pod,
			key:   runningRequestsKey(pod.Namespace, pod.Name),
			local: int64(atomic.LoadInt32(&pod.runningRequests)),
		})
		return true
	})
	if len(samples) == 0 {
		c.runningRequestsDrift = nil
		return
	}

	type correction struct {
		sample
		observed int64
		delta    int64
	}
	var corrections []correction
	// Rebuilt every pass so a pod that left the cache does not keep a stale entry.
	next := make(map[string]runningRequestsDriftState, len(c.runningRequestsDrift))
	// A pod this pass could not observe keeps the progress it had: a read that fails says
	// nothing about whether the gap closed, so it must not restart the confirmation.
	keepState := func(key string) {
		if state, ok := c.runningRequestsDrift[key]; ok {
			next[key] = state
		}
	}

	// One context for every batch, so a slow Redis bounds the whole read phase rather than
	// each batch in turn; batches past the deadline fail fast and keep their state.
	readCtx, cancelRead := context.WithTimeout(context.Background(), runningRequestsWriteTimeout)
	defer cancelRead()
	for batchStart := 0; batchStart < len(samples); batchStart += runningRequestsDriftReadBatchSize {
		batchEnd := batchStart + runningRequestsDriftReadBatchSize
		if batchEnd > len(samples) {
			batchEnd = len(samples)
		}
		batch := samples[batchStart:batchEnd]
		pipe := c.redisClient.Pipeline()
		reads := make([]*redis.SliceCmd, len(batch))
		for i, s := range batch {
			reads[i] = pipe.HMGet(readCtx, s.key, runningRequestsGatewayInstanceID)
		}
		if _, err := pipe.Exec(readCtx); err != nil {
			klog.V(4).ErrorS(err, "failed to read own running-requests fields for drift check; keeping previous drift state for this batch",
				"batchStart", batchStart, "batchLen", len(batch))
			for _, s := range batch {
				keepState(s.key)
			}
			continue
		}
		for i, s := range batch {
			fields, err := reads[i].Result()
			if err != nil || len(fields) != 1 {
				keepState(s.key)
				continue
			}
			var observed int64 // an absent field or hash counts as zero
			if raw, ok := fields[0].(string); ok {
				n, parseErr := strconv.ParseInt(raw, 10, 64)
				if parseErr != nil {
					keepState(s.key)
					continue
				}
				observed = n
			}
			state, delta := c.runningRequestsDrift[s.key].observe(observed, s.local)
			if delta != 0 {
				corrections = append(corrections, correction{sample: s, observed: observed, delta: delta})
			} else if state.passes > 0 {
				next[s.key] = state
			}
		}
	}
	c.runningRequestsDrift = next
	if len(corrections) == 0 {
		return
	}

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), runningRequestsWriteTimeout)
	defer cancelWrite()
	ttl := strconv.FormatInt(runningRequestsTTL.Milliseconds(), 10)
	for _, cor := range corrections {
		// The same script every ordinary +1/-1 uses (see runningRequestsIncrDecrScript):
		// an unconditional HINCRBY, not one guarded on the field still holding cor.observed.
		res, err := c.redisClient.Eval(writeCtx, runningRequestsIncrDecrScript, []string{cor.key},
			runningRequestsGatewayInstanceID, strconv.FormatInt(cor.delta, 10), ttl).Result()
		if err != nil {
			klog.V(4).ErrorS(err, "failed to correct drifted running-requests field", "key", cor.key)
			continue
		}
		newVal, ok := res.(int64)
		if !ok || newVal < 0 {
			// A negative correction against a hash the script found already gone (see
			// runningRequestsIncrDecrScript's missing-key guard) -- nothing to correct
			// until the pod is routed to again and the key exists once more.
			continue
		}
		// Info, not V(4): this only fires when a count has been wrong for seconds, and
		// it is the evidence an operator needs that counts are drifting.
		klog.InfoS("corrected drifted running-requests count",
			"pod", cor.pod.Name, "namespace", cor.pod.Namespace, "gateway", runningRequestsGatewayInstanceID,
			"redis_count", cor.observed, "local_count", cor.local, "correction", cor.delta, "new_redis_count", newVal)
	}
}

// initRunningRequestsLiveness starts the background heartbeat that keeps this
// gateway instance's entry in runningRequestsGatewaysKey fresh, so readers can tell
// this instance's running-requests hash-field contributions apart from a crashed
// instance's leaked ones. Heartbeats immediately (so a freshly started gateway
// doesn't take runningRequestsLivenessHeartbeatInterval to be considered live), then
// every runningRequestsLivenessHeartbeatInterval until stopCh fires. No-ops when
// Redis isn't configured.
func initRunningRequestsLiveness(store *Store, stopCh <-chan struct{}) {
	if store.redisClient == nil {
		return
	}
	runningRequestsLivenessStarted.Store(true)
	store.heartbeatRunningRequestsLiveness()
	ticker := time.NewTicker(runningRequestsLivenessHeartbeatInterval)
	go func() {
		defer ticker.Stop()
		defer runningRequestsLivenessStarted.Store(false)
		for {
			select {
			case <-ticker.C:
				store.heartbeatRunningRequestsLiveness()
			case <-stopCh:
				return
			}
		}
	}()
}

// redisNowMillis estimates the current time on Redis's clock (unix ms): this
// process's own clock, corrected by the offset observed at the last successful
// liveness heartbeat (see writeRunningRequestsLivenessHeartbeat). Every gateway
// instance heartbeats its score and computes liveness cutoffs through this, rather
// than through raw time.Now(), so all of them reason about liveness on one shared
// clock instead of each other's potentially-skewed local clocks -- important given
// how tight runningRequestsLivenessWindow is (3s).
func (c *Store) redisNowMillis() int64 {
	return time.Now().UnixMilli() + c.runningRequestsClockOffsetMillis.Load()
}

// liveGatewaysCutoffMillis is the earliest heartbeat timestamp (unix ms) still
// considered live -- see runningRequestsLivenessWindow.
func (c *Store) liveGatewaysCutoffMillis() string {
	return strconv.FormatInt(c.redisNowMillis()-runningRequestsLivenessWindow.Milliseconds(), 10)
}

// prunableGatewaysCutoffMillis is the earliest heartbeat timestamp (unix ms) that
// still protects a gateway's hash fields from deletion -- see
// runningRequestsFieldPruneWindow. Deliberately a separate, much longer cutoff than
// liveGatewaysCutoffMillis: summing and deleting need different safety margins.
func (c *Store) prunableGatewaysCutoffMillis() string {
	return strconv.FormatInt(c.redisNowMillis()-runningRequestsFieldPruneWindow.Milliseconds(), 10)
}

// sumLiveFields sums fields whose gateway ID is in live, skipping stale/dead
// gateways' contributions (see readPodRunningRequests) and clamping any individual
// field at zero as defense in depth (the write-side Lua script already clamps at
// zero, so this should never actually trigger). excluded lists the fields that were
// skipped -- these are only PRUNE CANDIDATES, not confirmed dead: live is built from
// the short runningRequestsLivenessWindow so exclusion reacts fast, which also means
// a gateway that merely missed one heartbeat (GC pause, brief network blip) shows up
// here. Callers must re-check candidates against a much longer window before
// deleting anything -- see flushPendingRunningRequestsPrunes.
func sumLiveFields(fields map[string]string, live map[string]struct{}) (total int64, excluded []string) {
	for gatewayID, valStr := range fields {
		if _, ok := live[gatewayID]; !ok {
			excluded = append(excluded, gatewayID)
			continue
		}
		n, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			continue
		}
		if n < 0 {
			n = 0
		}
		total += n
	}
	return total, excluded
}

// overlaySelfRunningRequests replaces this gateway's own field in fields (creating it
// if absent) with its local pod.runningRequests atomic, and marks that field live.
//
// incrPodRunningRequests/decrPodRunningRequests are fired from a detached goroutine,
// fire-and-forget with respect to the request path (see addPodStats/donePodStats), so
// Redis's copy of THIS gateway's own contribution can briefly lag the true value in
// either direction -- most visibly right after this same process just incremented
// locally for a request that another, concurrent admission check (inflight cap,
// routing score) needs to see immediately. The local atomic is not an approximation
// of that field -- it IS the value this process would have written, had the write
// already landed -- so it always wins over whatever Redis currently holds, regardless
// of whether the field was present, stale, or missing. This does not help with lag in
// OTHER gateways' fields; that remains bounded only by eventual consistency.
func (c *Store) overlaySelfRunningRequests(namespace, name string, fields map[string]string, live map[string]struct{}) {
	metaPod, ok := c.metaPods.Load(utils.GeneratePodKey(namespace, name))
	if !ok {
		return
	}
	fields[runningRequestsGatewayInstanceID] = strconv.FormatInt(int64(atomic.LoadInt32(&metaPod.runningRequests)), 10)
	live[runningRequestsGatewayInstanceID] = struct{}{}
}

// enqueueDeadRunningRequestsPrune records hash fields that a read excluded from its
// live sum, to be HDEL'd later by flushPendingRunningRequestsPrunes. Reads must not
// spawn prune goroutines: after a crash those fields stay prune-candidates for the
// full runningRequestsFieldPruneWindow (the short liveness window is not long enough
// to delete), so a per-read goroutine would scale with QPS rather than with stale
// fields. Latest candidates for a key overwrite earlier ones; the next hygiene pass
// drains the map.
func (c *Store) enqueueDeadRunningRequestsPrune(key string, candidates []string) {
	if len(candidates) == 0 {
		return
	}
	c.runningRequestsPendingPrunes.Store(key, append([]string(nil), candidates...))
}

// flushPendingRunningRequestsPrunes best-effort HDELs enqueued fields belonging to
// gateways that have been gone for at least runningRequestsFieldPruneWindow, keeping
// a hash from accumulating one field per gateway instance that has ever routed to
// this pod across the cluster's lifetime (otherwise those fields only disappear when
// the whole key idles out past runningRequestsTTL, which never happens for a pod
// other gateways keep routing to).
//
// Candidates were excluded using the much shorter runningRequestsLivenessWindow (for
// fast crash-detection in a sum) -- that is NOT long enough to safely delete
// anything: a candidate might just be a gateway that missed one heartbeat. So this
// re-checks every candidate against runningRequestsFieldPruneWindow itself -- only a
// candidate that is ALSO absent from that longer window gets deleted. Runs from
// heartbeat hygiene, not the read path. Self-limiting: once a truly-dead field is
// gone, later reads of the same key have nothing left to enqueue.
func (c *Store) flushPendingRunningRequestsPrunes() {
	if c.redisClient == nil {
		return
	}
	type pendingPrune struct {
		key        string
		candidates []string
	}
	var items []pendingPrune
	c.runningRequestsPendingPrunes.Range(func(key string, _ []string) bool {
		got, ok := c.runningRequestsPendingPrunes.LoadAndDelete(key)
		if !ok {
			return true
		}
		items = append(items, pendingPrune{key: key, candidates: got})
		return true
	})
	if len(items) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), runningRequestsWriteTimeout)
	defer cancel()
	client := c.redisClient
	longLived, err := client.ZRangeByScore(ctx, runningRequestsGatewaysKey,
		&redis.ZRangeBy{Min: c.prunableGatewaysCutoffMillis(), Max: "+inf"}).Result()
	if err != nil {
		klog.V(4).ErrorS(err, "failed to check long-window liveness before pruning running-requests fields")
		for _, item := range items {
			c.runningRequestsPendingPrunes.Store(item.key, item.candidates)
		}
		return
	}
	stillProtected := make(map[string]struct{}, len(longLived))
	for _, gw := range longLived {
		stillProtected[gw] = struct{}{}
	}

	pipe := client.Pipeline()
	n := 0
	for _, item := range items {
		safe := make([]string, 0, len(item.candidates))
		for _, id := range item.candidates {
			if _, ok := stillProtected[id]; !ok {
				safe = append(safe, id)
			}
		}
		if len(safe) == 0 {
			continue
		}
		pipe.HDel(ctx, item.key, safe...)
		n++
	}
	if n == 0 {
		return
	}
	if _, err := pipe.Exec(ctx); err != nil {
		klog.V(4).ErrorS(err, "failed to prune dead running-requests fields", "key_count", n)
	}
}

// readPodRunningRequests reads the live cross-gateway running-request count for a
// single pod directly from Redis -- no local cache, no polling delay. It pipelines
// (one round trip) a read of the live-gateways set alongside the pod's
// running-requests hash, then sums only the fields belonging to currently-live
// gateways (see runningRequestsLivenessWindow), so a crashed gateway's leaked
// contribution is excluded within that window regardless of how much traffic other
// gateways keep sending to the same pod. Before summing, this gateway's own field is
// overlaid with its local atomic (see overlaySelfRunningRequests) so this process's
// own just-applied increment/decrement is never masked by its own not-yet-landed
// async Redis write. ok is false when Redis isn't configured, the read fails, or the
// pod's hash doesn't exist yet (never routed to, or fully idle past
// runningRequestsTTL) -- letting the caller fall back to its local atomic counter. A
// hash that DOES exist but sums to zero (e.g. every other contributor is a now-dead
// gateway, and this gateway's own overlaid count is also zero) is a valid, correct
// answer, not a fallback case.
func (c *Store) readPodRunningRequests(namespace, name string) (count int64, ok bool) {
	if c.redisClient == nil {
		return 0, false
	}
	key := runningRequestsKey(namespace, name)
	ctx, cancel := context.WithTimeout(context.Background(), runningRequestsReadTimeout)
	defer cancel()
	client := c.redisClient
	pipe := client.Pipeline()
	liveCmd := pipe.ZRangeByScore(ctx, runningRequestsGatewaysKey, &redis.ZRangeBy{Min: c.liveGatewaysCutoffMillis(), Max: "+inf"})
	hashCmd := pipe.HGetAll(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		klog.V(4).ErrorS(err, "failed to read running-requests counter", "namespace", namespace, "name", name)
		return 0, false
	}
	liveGateways, err := liveCmd.Result()
	if err != nil {
		klog.V(4).ErrorS(err, "failed to read live gateways for running-requests counter")
		return 0, false
	}
	fields, err := hashCmd.Result()
	if err != nil || len(fields) == 0 {
		return 0, false
	}
	live := make(map[string]struct{}, len(liveGateways)+1)
	for _, gw := range liveGateways {
		live[gw] = struct{}{}
	}
	// An empty live set here is not, by itself, a reason to distrust the sum: a hash
	// whose only-ever contributor has gone stale correctly sums to zero -- crash
	// recovery is this liveness check's job, not a signal that something is wrong
	// with the read.
	c.overlaySelfRunningRequests(namespace, name, fields, live)
	total, excluded := sumLiveFields(fields, live)
	if len(excluded) > 0 {
		c.enqueueDeadRunningRequestsPrune(key, excluded)
	}
	return total, true
}

// readPodsRunningRequests batch-reads the live cross-gateway running-request count
// for every pod in pods with a single pipelined round trip (the live-gateways set
// plus one HGetAll per pod) -- so scoring N candidate pods for one routing decision
// costs one Redis round trip, not N. See readPodRunningRequests for the per-pod summing/liveness
// semantics. nil entries in pods are skipped (pod identity is required to build a
// key). The returned map is keyed by utils.GeneratePodKey(pod.Namespace, pod.Name)
// and only contains pods with a live hash; callers fall back to the local atomic
// counter for any pod missing from it.
func (c *Store) readPodsRunningRequests(pods []*v1.Pod) map[string]int64 {
	counts, _ := c.readPodsSharedRoutingState(pods, false)
	return counts
}
