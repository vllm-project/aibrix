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
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newTestRunningRequestsClient(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// A negative delta against a hash key that does not exist must be a no-op: it must
// not resurrect the key holding a lying zero for this gateway's field (see
// runningRequestsIncrDecrScript's doc comment).
func TestRunningRequestsIncrDecrScript_NegativeDeltaNoOpOnMissingKey(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	key := runningRequestsKey(testNamespace, testPodName)

	res, err := client.Eval(ctx, runningRequestsIncrDecrScript, []string{key},
		"gw-1", "-1", "60000").Result()
	require.NoError(t, err)
	require.EqualValues(t, -1, res)

	exists, err := client.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "negative delta on a missing key must not create it")
}

// A positive delta on a missing key legitimately creates it -- only negative deltas
// are guarded.
func TestRunningRequestsIncrDecrScript_PositiveDeltaCreatesKey(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	key := runningRequestsKey(testNamespace, testPodName)

	res, err := client.Eval(ctx, runningRequestsIncrDecrScript, []string{key},
		"gw-1", "1", "60000").Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, res)

	ttl, err := client.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))
}

// Defense in depth: even if a field goes negative (e.g. an unpaired decrement slips
// through), the script clamps it at zero instead of letting the hash go negative.
func TestRunningRequestsIncrDecrScript_ClampsFieldAtZero(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	key := runningRequestsKey(testNamespace, testPodName)

	// Create the key via a different field so EXISTS is already true when our
	// field takes its first, negative delta.
	require.NoError(t, client.HSet(ctx, key, "gw-other", 5).Err())

	res, err := client.Eval(ctx, runningRequestsIncrDecrScript, []string{key},
		"gw-1", "-1", "60000").Result()
	require.NoError(t, err)
	require.EqualValues(t, 0, res)

	val, err := client.HGet(ctx, key, "gw-1").Result()
	require.NoError(t, err)
	require.Equal(t, "0", val)
}

func TestIncrDecrPodRunningRequests_RoundTrip(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}

	require.True(t, store.incrPodRunningRequests(testNamespace, testPodName))
	key := runningRequestsKey(testNamespace, testPodName)
	val, err := client.HGet(ctx, key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, "1", val)

	store.decrPodRunningRequests(testNamespace, testPodName)
	val, err = client.HGet(ctx, key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, "0", val)
}

// decrPodRunningRequests is fire-and-forget; calling it against a pod with no prior
// increment (missing hash key) must not create the key.
func TestDecrPodRunningRequests_NoOpWhenKeyMissing(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}

	store.decrPodRunningRequests(testNamespace, testPodName)

	exists, err := client.Exists(ctx, runningRequestsKey(testNamespace, testPodName)).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}

// seedLiveGateway marks runningRequestsGatewayInstanceID (this test process's own
// identity) as live in the liveness ZSET, so admitRunningRequest's sum actually counts
// its own field -- without this, the script would treat the field as belonging to a dead
// gateway and always compute a sum of 0, admitting regardless of limit.
func seedLiveGateway(t *testing.T, client *redis.Client) {
	t.Helper()
	require.NoError(t, client.ZAdd(context.Background(), runningRequestsGatewaysKey,
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: runningRequestsGatewayInstanceID}).Err())
}

// admitRunningRequest must admit while the live sum is under limit and increment this
// gateway's field as it goes, then reject once the sum reaches limit -- without ever
// incrementing on the rejected call (that call never actually got in).
func TestAdmitRunningRequest_AdmitsUnderLimitThenRejects(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	seedLiveGateway(t, client)

	admitted, running := store.admitRunningRequest(testNamespace, testPodName, 2, 0)
	require.True(t, admitted)
	require.EqualValues(t, 0, running)

	admitted, running = store.admitRunningRequest(testNamespace, testPodName, 2, 0)
	require.True(t, admitted)
	require.EqualValues(t, 1, running)

	admitted, running = store.admitRunningRequest(testNamespace, testPodName, 2, 0)
	require.False(t, admitted, "a request must be rejected once the live sum reaches the limit")
	require.EqualValues(t, 2, running)

	key := runningRequestsKey(testNamespace, testPodName)
	val, err := client.HGet(ctx, key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, "2", val, "the rejected call must not have incremented the field")
}

// A dead gateway's leaked field must not count against the cap -- same live-only
// summing semantics as sumLiveFields/readPodRunningRequests.
func TestAdmitRunningRequest_OnlyCountsLiveGateways(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	key := runningRequestsKey(testNamespace, testPodName)

	require.NoError(t, client.HSet(context.Background(), key, "gw-dead", 10).Err())
	seedLiveGateway(t, client)

	admitted, running := store.admitRunningRequest(testNamespace, testPodName, 1, 0)
	require.True(t, admitted, "a dead gateway's leaked count must not block admission")
	require.EqualValues(t, 0, running)
}

// When Redis is unavailable, admission must fail open to comparing the caller's local
// atomic count against limit rather than blocking routing.
func TestAdmitRunningRequest_FailsOpenToLocalCountWhenRedisNil(t *testing.T) {
	store := &Store{}

	admitted, running := store.admitRunningRequest(testNamespace, testPodName, 2, 1)
	require.True(t, admitted)
	require.EqualValues(t, 1, running)

	admitted, running = store.admitRunningRequest(testNamespace, testPodName, 2, 2)
	require.False(t, admitted)
	require.EqualValues(t, 2, running)
}

// TestAdmitRunningRequest_ConcurrentCallsNeverExceedLimit is the regression this whole
// mechanism exists for: a plain read (the live sum) followed by a later, separately-fired
// increment -- what enforceReplicaInflight used to do -- lets any number of concurrent
// requests to the same pod all observe the same pre-increment sum and all get admitted,
// however tight the limit, because none of their increments have landed by the time the
// others check. admitRunningRequest folds the check and the increment into one atomic
// Redis script specifically so this cannot happen. Fire far more concurrent callers than
// the limit and assert that exactly limit of them get in, not more.
func TestAdmitRunningRequest_ConcurrentCallsNeverExceedLimit(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	seedLiveGateway(t, client)

	const (
		limit       = 5
		concurrency = 50
	)
	var admittedCount int32
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			if admitted, _ := store.admitRunningRequest(testNamespace, testPodName, limit, 0); admitted {
				atomic.AddInt32(&admittedCount, 1)
			}
		}()
	}
	wg.Wait()

	require.EqualValues(t, limit, admittedCount,
		"exactly limit of the concurrent callers must be admitted -- this is what makes requestsInflight a hard cap")

	key := runningRequestsKey(testNamespace, testPodName)
	val, err := client.HGet(context.Background(), key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(limit), val, "the field must reflect exactly the admitted count, never more")
}

// writeRunningRequestsLivenessHeartbeat must sync this process's clock-offset
// estimate from Redis's own TIME, not trust the local clock -- otherwise a gateway
// pod whose clock has drifted from Redis's would stamp its heartbeat (and later
// compute liveness cutoffs) on the wrong timeline relative to every other gateway
// pod, which under the tight runningRequestsLivenessWindow can make a live instance
// look dead (undercount) or vice versa.
func TestWriteRunningRequestsLivenessHeartbeat_SyncsClockOffsetFromRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := &Store{redisClient: client}

	const skew = 30 * time.Second
	redisTime := time.Now().Add(skew)
	mr.SetTime(redisTime)

	store.writeRunningRequestsLivenessHeartbeat()

	offset := store.runningRequestsClockOffsetMillis.Load()
	require.InDelta(t, skew.Milliseconds(), offset, float64((2 * time.Second).Milliseconds()),
		"offset should track Redis's skew from this process's local clock")
}

// A heartbeat's ZADD score must reflect Redis's clock (via the offset synced by the
// prior tick), not this process's raw, possibly-skewed local clock -- see
// redisNowMillis.
func TestWriteRunningRequestsLivenessHeartbeat_ScoreUsesSyncedRedisClock(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := &Store{redisClient: client}

	const skew = 30 * time.Second
	redisTime := time.Now().Add(skew)
	mr.SetTime(redisTime)

	// First tick only syncs the offset; the second tick's score is the one that
	// benefits from it.
	store.writeRunningRequestsLivenessHeartbeat()
	store.writeRunningRequestsLivenessHeartbeat()

	score, err := client.ZScore(context.Background(), runningRequestsGatewaysKey, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.InDelta(t, float64(redisTime.UnixMilli()), score, float64((2 * time.Second).Milliseconds()),
		"score should track Redis's clock")
	require.Greater(t, score, float64(time.Now().UnixMilli()),
		"score must not have been stamped from raw local time, which lags Redis's clock by the simulated skew")
}

func TestSumLiveFields(t *testing.T) {
	// gw-bad is live but has a malformed value: it must be silently skipped, neither
	// summed nor reported as a dead-gateway prune candidate (it isn't dead -- its
	// value is just bad).
	live := map[string]struct{}{"gw-live": {}, "gw-bad": {}}
	fields := map[string]string{
		"gw-live": "3",
		"gw-dead": "4",
		"gw-bad":  "not-a-number",
	}

	total, excluded := sumLiveFields(fields, live)
	require.EqualValues(t, 3, total, "only the live, well-formed field should be summed")
	require.ElementsMatch(t, []string{"gw-dead"}, excluded, "only the non-live gateway is a prune candidate")
}

func TestSumLiveFields_ClampsNegativeAtZero(t *testing.T) {
	live := map[string]struct{}{"gw-live": {}}
	fields := map[string]string{"gw-live": "-2"}

	total, excluded := sumLiveFields(fields, live)
	require.Zero(t, total)
	require.Empty(t, excluded)
}

// The short liveness window drives fast exclusion from a sum, but must not be
// mistaken for the much longer window that gates actually deleting a field: a
// gateway missing only from the short window is a prune *candidate*, not yet safe
// to delete.
func TestFlushPendingRunningRequestsPrunes_RespectsLongerPruneWindow(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}

	key := runningRequestsKey(testNamespace, testPodName)
	require.NoError(t, client.HSet(ctx, key, map[string]any{
		"gw-recent-blip": "3", // heartbeated recently enough to still be protected
		"gw-long-dead":   "2", // last heartbeat older than the prune window
		"gw-alive":       "1", // currently live, never a candidate
	}).Err())

	now := time.Now()
	require.NoError(t, client.ZAdd(ctx, runningRequestsGatewaysKey,
		redis.Z{Score: float64(now.Add(-1 * time.Second).UnixMilli()), Member: "gw-recent-blip"},
		redis.Z{Score: float64(now.Add(-(runningRequestsFieldPruneWindow + time.Minute)).UnixMilli()), Member: "gw-long-dead"},
		redis.Z{Score: float64(now.UnixMilli()), Member: "gw-alive"},
	).Err())

	// Simulate what a read would have enqueued: both non-live-within-the-short-window
	// gateways were excluded from the sum.
	store.enqueueDeadRunningRequestsPrune(key, []string{"gw-recent-blip", "gw-long-dead"})

	store.flushPendingRunningRequestsPrunes()

	fields, err := client.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"gw-recent-blip": "3",
		"gw-alive":       "1",
	}, fields, "only the field whose gateway is absent from the long prune window should be deleted")

	_, pending := store.runningRequestsPendingPrunes.Load(key)
	require.False(t, pending, "the queue must be drained after a flush")
}

func TestEnqueueDeadRunningRequestsPrune_EmptyCandidatesIsNoOp(t *testing.T) {
	store := &Store{}
	store.enqueueDeadRunningRequestsPrune("some-key", nil)
	_, ok := store.runningRequestsPendingPrunes.Load("some-key")
	require.False(t, ok)
}

// A hash that doesn't exist yet (never routed to, or idled out) must report ok=false
// so callers fall back to the local atomic counter -- distinct from a hash that DOES
// exist but sums to zero, which is a real answer (see next test).
func TestReadPodRunningRequests_MissingHashIsNotOk(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}

	count, ok := store.readPodRunningRequests(testNamespace, testPodName)
	require.False(t, ok)
	require.Zero(t, count)
}

// A hash that exists but whose only contributor(s) are no longer live sums to a
// genuine zero -- ok must be true, not a signal to fall back locally.
func TestReadPodRunningRequests_PresentButAllDeadIsValidZero(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}

	key := runningRequestsKey(testNamespace, testPodName)
	require.NoError(t, client.HSet(ctx, key, "gw-dead", "3").Err())
	// No entries in runningRequestsGatewaysKey at all -- nothing is "live".

	count, ok := store.readPodRunningRequests(testNamespace, testPodName)
	require.True(t, ok)
	require.Zero(t, count)

	_, pending := store.runningRequestsPendingPrunes.Load(key)
	require.True(t, pending, "the dead field should have been enqueued as a prune candidate")
}

func TestReadPodRunningRequests_SumsOnlyLiveGateways(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}

	key := runningRequestsKey(testNamespace, testPodName)
	require.NoError(t, client.HSet(ctx, key, map[string]any{
		"gw-live": "5",
		"gw-dead": "7",
	}).Err())
	require.NoError(t, client.ZAdd(ctx, runningRequestsGatewaysKey,
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: "gw-live"}).Err())

	count, ok := store.readPodRunningRequests(testNamespace, testPodName)
	require.True(t, ok)
	require.EqualValues(t, 5, count)
}

// This gateway's own field must be overlaid with its local atomic even when the hash
// already exists and sums to a nonzero value without it -- addPodStats increments the
// local atomic synchronously but fires the Redis write from a detached goroutine (see
// overlaySelfRunningRequests), so a concurrent admission check on this same gateway
// must never see a stale (here: absent) value for a request this process just started.
func TestReadPodRunningRequests_OverlaysSelfFromLocalAtomicWhenHashExists(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(2)
	store.metaPods.Store(testNamespace+"/"+testPodName, pod)

	// Hash already exists (another gateway's traffic), but has no field for this
	// gateway yet -- its own increment hasn't landed in Redis.
	key := runningRequestsKey(testNamespace, testPodName)
	require.NoError(t, client.HSet(ctx, key, "gw-other", "4").Err())
	require.NoError(t, client.ZAdd(ctx, runningRequestsGatewaysKey,
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: "gw-other"}).Err())

	count, ok := store.readPodRunningRequests(testNamespace, testPodName)
	require.True(t, ok)
	require.EqualValues(t, 6, count, "self's local count (2) must be added on top of the other live gateway's (4)")
}

// Same as above but for the all-dead-except-self case: a hash summing to zero from
// Redis's perspective must still surface this gateway's own live local count.
func TestReadPodRunningRequests_OverlaysSelfEvenWhenOthersAreDead(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(3)
	store.metaPods.Store(testNamespace+"/"+testPodName, pod)

	key := runningRequestsKey(testNamespace, testPodName)
	require.NoError(t, client.HSet(ctx, key, "gw-dead", "9").Err())
	// No entries in runningRequestsGatewaysKey -- gw-dead is not live, and this
	// gateway's own field isn't in the hash at all yet.

	count, ok := store.readPodRunningRequests(testNamespace, testPodName)
	require.True(t, ok)
	require.EqualValues(t, 3, count, "self's local count must surface even though the only hash field is dead")
}

func newTestPod(runningRequests int32) *Pod {
	return &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testPodName,
				Namespace: testNamespace,
			},
		},
		runningRequests: runningRequests,
	}
}

// realtimeRunningRequests must fall back to the local atomic counter when Redis has
// no hash for the pod yet.
func TestRealtimeRunningRequests_FallsBackToLocalWhenHashMissing(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(5)

	require.EqualValues(t, 5, store.realtimeRunningRequests(pod))
}

// When Redis DOES have a hash for the pod, its (possibly zero) answer is
// authoritative -- realtimeRunningRequests must not fall back to the local atomic
// counter just because the live sum came back zero.
func TestRealtimeRunningRequests_TrustsRedisZeroOverLocalCounter(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(9) // local counter disagrees with Redis on purpose

	require.NoError(t, client.HSet(ctx, runningRequestsKey(testNamespace, testPodName), "gw-dead", "3").Err())

	require.EqualValues(t, 0, store.realtimeRunningRequests(pod))
	_ = atomic.LoadInt32(&pod.runningRequests) // sanity: local counter is untouched
}
