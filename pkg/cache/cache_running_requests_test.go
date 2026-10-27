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
	"fmt"
	"io"
	"net"
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

type admitScriptHook struct {
	run     bool // let the script run before process picks the reply
	process func(*redis.Cmd) error
}

func (h admitScriptHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h admitScriptHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "eval" && cmd.Args()[1] == runningRequestsAdmitScript {
			if h.run {
				if err := next(ctx, cmd); err != nil {
					return err
				}
			}
			return h.process(cmd.(*redis.Cmd))
		}
		return next(ctx, cmd)
	}
}

func (h admitScriptHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// failAdmitScript fails the admit before it is sent, as a refused connection does.
func failAdmitScript(*redis.Cmd) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

// loseAdmitReply reports a timeout for an admit script that already ran.
func loseAdmitReply(*redis.Cmd) error { return context.DeadlineExceeded }

type testRedisError string

func (e testRedisError) Error() string { return string(e) }
func (testRedisError) RedisError()     {}

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

	admitted, counted, running := store.admitRunningRequest(testNamespace, testPodName, 2, 0)
	require.True(t, admitted)
	require.True(t, counted)
	require.EqualValues(t, 0, running)

	admitted, counted, running = store.admitRunningRequest(testNamespace, testPodName, 2, 0)
	require.True(t, admitted)
	require.True(t, counted)
	require.EqualValues(t, 1, running)

	admitted, counted, running = store.admitRunningRequest(testNamespace, testPodName, 2, 0)
	require.False(t, admitted, "a request must be rejected once the live sum reaches the limit")
	require.False(t, counted)
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

	admitted, counted, running := store.admitRunningRequest(testNamespace, testPodName, 1, 0)
	require.True(t, admitted, "a dead gateway's leaked count must not block admission")
	require.True(t, counted)
	require.EqualValues(t, 0, running)
}

// When Redis is unavailable, admission must fail open to comparing the caller's local
// atomic count against limit rather than blocking routing.
func TestAdmitRunningRequest_FailsOpenToLocalCountWhenRedisNil(t *testing.T) {
	store := &Store{}

	admitted, counted, running := store.admitRunningRequest(testNamespace, testPodName, 2, 1)
	require.True(t, admitted)
	require.False(t, counted)
	require.EqualValues(t, 1, running)

	admitted, counted, running = store.admitRunningRequest(testNamespace, testPodName, 2, 2)
	require.False(t, admitted)
	require.False(t, counted)
	require.EqualValues(t, 2, running)
}

func TestAdmitRunningRequest_FailsOpenToLocalCountWhenEvalFails(t *testing.T) {
	for _, tt := range []struct {
		name      string
		err       error
		didNotRun bool
	}{
		{"dial error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"Redis error reply", testRedisError("ERR Error running script"), false},
		{"closed client", redis.ErrClosed, true},
		{"deadline", context.DeadlineExceeded, false},
		{"read timeout", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("i/o timeout")}, false},
		{"dropped connection", io.ErrUnexpectedEOF, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestRunningRequestsClient(t)
			client.AddHook(admitScriptHook{process: func(*redis.Cmd) error { return tt.err }})
			store := &Store{redisClient: client}
			for _, local := range []int64{1, 2} {
				admitted, counted, running := store.admitRunningRequest(testNamespace, testPodName, 2, local)
				require.Equal(t, local < 2, admitted)
				require.Equal(t, admitted && !tt.didNotRun, counted,
					"only an error that shows the script never ran leaves the +1 to normal tracking")
				require.Equal(t, local, running)
			}
		})
	}
}

func TestAdmitRunningRequestsClient_DisablesRetries(t *testing.T) {
	require.Nil(t, admitRunningRequestsClient(nil))
	shared := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0", MaxRetries: 5})
	t.Cleanup(func() { _ = shared.Close() })
	admit := admitRunningRequestsClient(shared)
	t.Cleanup(func() { _ = admit.Close() })
	require.Zero(t, admit.Options().MaxRetries, "go-redis normalises -1 to 0, meaning no retries")
	require.Equal(t, 5, shared.Options().MaxRetries, "the shared client keeps its retries")
	require.Equal(t, shared.Options().Addr, admit.Options().Addr)
}

func TestAdmitRunningRequest_MalformedRepliesFallBackToLocalCount(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply interface{}
	}{
		{"not an array", int64(1)},
		{"empty array", []interface{}{}},
		{"short array", []interface{}{int64(1)}},
		{"long array", []interface{}{int64(1), int64(9), int64(0)}},
		{"wrong flag type", []interface{}{"1", int64(9)}},
		{"wrong sum type", []interface{}{int64(1), "9"}},
		{"both wrong types", []interface{}{1, 9}},
		{"negative flag", []interface{}{int64(-1), int64(9)}},
		{"flag above one", []interface{}{int64(2), int64(9)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestRunningRequestsClient(t)
			client.AddHook(admitScriptHook{process: func(cmd *redis.Cmd) error {
				cmd.SetVal(tt.reply)
				return nil
			}})
			store := &Store{redisClient: client}
			for _, local := range []int64{1, 2} {
				admitted, counted, running := store.admitRunningRequest(testNamespace, testPodName, 2, local)
				require.Equal(t, local < 2, admitted)
				require.False(t, counted)
				require.Equal(t, local, running)
			}
		})
	}
}

func TestAdmitPodRunningRequest_MissingPod(t *testing.T) {
	store := NewForTest()
	admitted, counted, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 2)
	require.ErrorContains(t, err, "key does not exist in the cache")
	require.False(t, admitted)
	require.False(t, counted)
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
	// admitRunningRequest falls back to a local-only decision when its Redis round trip
	// errors (see its doc comment). That fail-open path is not what this test is about,
	// and with every caller reporting localRunning=0 it admits every caller that hits
	// it, so a single expired round trip turns this assertion into a flake. The
	// production budget is short enough that 50 concurrent callers against one Redis
	// can exhaust it whenever the rest of the suite competes for CPU -- widen it here
	// to keep the test about the script's atomicity, which is what it documents.
	oldReadTimeout := runningRequestsReadTimeout
	runningRequestsReadTimeout = 30 * time.Second
	t.Cleanup(func() { runningRequestsReadTimeout = oldReadTimeout })

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
			if admitted, _, _ := store.admitRunningRequest(testNamespace, testPodName, limit, 0); admitted {
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

// observe must stay silent until a gap has held in one direction for
// runningRequestsDriftConfirmPasses passes, and then correct only the part of the gap that
// never closed.
func TestRunningRequestsDriftState_Observe(t *testing.T) {
	type sample struct{ redis, local int64 }
	repeat := func(n int, s sample) []sample {
		out := make([]sample, n)
		for i := range out {
			out[i] = s
		}
		return out
	}
	join := func(parts ...[]sample) []sample {
		var out []sample
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	n := runningRequestsDriftConfirmPasses

	tests := []struct {
		name    string
		samples []sample
		// wantDeltas maps a 0-based sample index to the correction expected there; every
		// other sample must return zero.
		wantDeltas map[int]int64
	}{
		{name: "matching counts never correct", samples: repeat(3*n, sample{2, 2})},
		{name: "idle pod with no field never corrects", samples: repeat(3*n, sample{0, 0})},
		{
			name:       "excess that persists is removed once confirmed",
			samples:    repeat(n, sample{2, 0}),
			wantDeltas: map[int]int64{n - 1: -2},
		},
		{
			name:       "shortfall that persists is added back once confirmed",
			samples:    repeat(n, sample{0, 2}),
			wantDeltas: map[int]int64{n - 1: 2},
		},
		{
			name:    "gap that closes in time starts over",
			samples: join(repeat(n-1, sample{1, 0}), repeat(1, sample{0, 0}), repeat(n-1, sample{1, 0})),
		},
		{
			name:    "gap that flips direction starts over",
			samples: join(repeat(n-1, sample{1, 0}), repeat(n-1, sample{0, 1})),
		},
		{
			name:       "only the part of the gap that never closed is corrected",
			samples:    join(repeat(2, sample{3, 0}), repeat(1, sample{2, 0}), repeat(n-3, sample{3, 0})),
			wantDeltas: map[int]int64{n - 1: -2},
		},
		{
			name:       "confirmation restarts after a correction",
			samples:    repeat(2*n, sample{1, 0}),
			wantDeltas: map[int]int64{n - 1: -1, 2*n - 1: -1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var state runningRequestsDriftState
			for i, s := range tt.samples {
				var delta int64
				state, delta = state.observe(s.redis, s.local)
				require.Equal(t, tt.wantDeltas[i], delta, "sample %d (redis=%d local=%d)", i, s.redis, s.local)
			}
		})
	}
}

// A confirmed correction is applied as a plain delta (runningRequestsIncrDecrScript), the
// same script and semantics as an ordinary +1/-1: no guard on the field still holding a
// previously observed value and an arbitrary magnitude allowed.
func TestRunningRequestsIncrDecrScript_AppliesAMultiUnitCorrectionDelta(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	key := runningRequestsKey(testNamespace, testPodName)
	ttl := strconv.FormatInt(runningRequestsTTL.Milliseconds(), 10)
	require.NoError(t, client.HSet(ctx, key, "gw-1", "3").Err())

	res, err := client.Eval(ctx, runningRequestsIncrDecrScript, []string{key}, "gw-1", "-2", ttl).Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, res)

	field, err := client.HGet(ctx, key, "gw-1").Result()
	require.NoError(t, err)
	require.Equal(t, "1", field)
}

func newTestReconcileStore(t *testing.T, local int32) (*Store, *redis.Client, string) {
	t.Helper()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	store.metaPods.Store(testNamespace+"/"+testPodName, newTestPod(local))
	return store, client, runningRequestsKey(testNamespace, testPodName)
}

func reconcilePasses(store *Store, n int) {
	for i := 0; i < n; i++ {
		store.reconcileOwnRunningRequests()
	}
}

// The bug this exists for: a decrement that never lands leaves this gateway's field above
// its local count, and other gateways sum that field as-is.
func TestReconcileOwnRunningRequests_RemovesLeakedCountOnceConfirmed(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 0)
	own := runningRequestsGatewayInstanceID
	require.NoError(t, client.HSet(ctx, key, map[string]interface{}{own: "2", "gw-other": "1"}).Err())

	reconcilePasses(store, runningRequestsDriftConfirmPasses-1)
	got, err := client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "2", got, "a gap that has not persisted long enough must be left alone")

	reconcilePasses(store, 1)
	fields, err := client.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, map[string]string{own: "0", "gw-other": "1"}, fields,
		"only this gateway's own field is corrected; other gateways' fields are theirs to keep")
	ttlLeft, err := client.PTTL(ctx, key).Result()
	require.NoError(t, err)
	require.Positive(t, ttlLeft)
}

// A real request landing between the second-to-last and the confirming pass must not
// throw away the confirmation or leave the correction wrong: the confirming pass reads
// the field fresh, so it folds the new write into what it corrects rather than acting on
// a cached value from an earlier pass.
//
// This does not exercise a write landing between reconcileOwnRunningRequests' own HMGET
// and its own Eval, an interior gap of one Redis round trip within a single pass. That
// gap is not deterministically reproducible against an in-process fake Redis with no
// simulated latency. What guarantees against it is structural: the Eval call passes no
// previously-observed value for the script to compare against (see
// TestRunningRequestsIncrDecrScript_AppliesAMultiUnitCorrectionDelta), so there is
// nothing for a write landing in that gap to invalidate.
func TestReconcileOwnRunningRequests_CorrectionLandsDespiteConcurrentRealWrite(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 0)
	own := runningRequestsGatewayInstanceID
	require.NoError(t, client.HSet(ctx, key, own, "2").Err())

	reconcilePasses(store, runningRequestsDriftConfirmPasses-1)

	// A real request starts on this pod right as the confirming pass is about to run.
	require.NoError(t, client.HIncrBy(ctx, key, own, 1).Err())

	reconcilePasses(store, 1)

	got, err := client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "1", got,
		"the confirmed -2 correction must land on top of the real +1, leaving local(0) + the new real request(1)")
}

func TestReconcileOwnRunningRequests_LeavesFieldThatMatchesLocalCount(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 3)
	own := runningRequestsGatewayInstanceID
	require.NoError(t, client.HSet(ctx, key, own, "3").Err())

	reconcilePasses(store, 3*runningRequestsDriftConfirmPasses)

	got, err := client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "3", got)
}

// Requests coming and going keep the field briefly apart from the local count; that must
// never be mistaken for drift.
func TestReconcileOwnRunningRequests_IgnoresGapThatCloses(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 0)
	own := runningRequestsGatewayInstanceID
	require.NoError(t, client.HSet(ctx, key, own, "1").Err())

	reconcilePasses(store, runningRequestsDriftConfirmPasses-1)
	// The request this gateway's field was counting is now reflected locally too.
	pod, ok := store.metaPods.Load(testNamespace + "/" + testPodName)
	require.True(t, ok)
	atomic.StoreInt32(&pod.runningRequests, 1)
	reconcilePasses(store, 2*runningRequestsDriftConfirmPasses)

	got, err := client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "1", got)
}

// The other direction: a field below the local count hides real load from every other
// gateway.
func TestReconcileOwnRunningRequests_RaisesUndercountedField(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 2)
	own := runningRequestsGatewayInstanceID
	require.NoError(t, client.HSet(ctx, key, "gw-other", "1").Err())

	reconcilePasses(store, runningRequestsDriftConfirmPasses)

	got, err := client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "2", got)
}

// A pod this gateway has no requests for and no field for must not get a hash created.
func TestReconcileOwnRunningRequests_DoesNotCreateHashForIdlePod(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 0)

	reconcilePasses(store, 3*runningRequestsDriftConfirmPasses)

	exists, err := client.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}

func TestReconcileOwnRunningRequests_ForgetsPodsThatLeftTheCache(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 0)
	require.NoError(t, client.HSet(ctx, key, runningRequestsGatewayInstanceID, "1").Err())

	reconcilePasses(store, runningRequestsDriftConfirmPasses-1)
	require.Len(t, store.runningRequestsDrift, 1)

	store.metaPods.Delete(testNamespace + "/" + testPodName)
	reconcilePasses(store, 1)
	require.Empty(t, store.runningRequestsDrift)
}

// driftReadHook watches the HMGET pipelines reconcileOwnRunningRequests sends: it records the
// largest one and fails the next failNext of them.
type driftReadHook struct {
	maxPipelineLen atomic.Int64
	failNext       atomic.Int64
}

func (h *driftReadHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *driftReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (h *driftReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if len(cmds) > 0 && cmds[0].Name() == "hmget" {
			if n := int64(len(cmds)); n > h.maxPipelineLen.Load() {
				h.maxPipelineLen.Store(n)
			}
			if h.failNext.Load() > 0 {
				h.failNext.Add(-1)
				return errors.New("injected read failure")
			}
		}
		return next(ctx, cmds)
	}
}

// newTestDriftedFleet is a store with n pods, each holding a Redis field of 2 against a local
// count of 0 -- a leaked count on every pod -- behind a driftReadHook.
func newTestDriftedFleet(t *testing.T, n int) (*Store, *redis.Client, *driftReadHook) {
	t.Helper()
	client := newTestRunningRequestsClient(t)
	hook := &driftReadHook{}
	client.AddHook(hook)
	store := &Store{redisClient: client}
	for i := 0; i < n; i++ {
		pod := newTestPod(0)
		pod.Name = fmt.Sprintf("pod-drift-%d", i)
		store.metaPods.Store(testNamespace+"/"+pod.Name, pod)
		require.NoError(t, client.HSet(context.Background(),
			runningRequestsKey(testNamespace, pod.Name), runningRequestsGatewayInstanceID, "2").Err())
	}
	return store, client, hook
}

// countDriftedFields is how many of the n pods' own fields still hold the leaked count.
func countDriftedFields(t *testing.T, client *redis.Client, n int) int {
	t.Helper()
	drifted := 0
	for i := 0; i < n; i++ {
		got, err := client.HGet(context.Background(),
			runningRequestsKey(testNamespace, fmt.Sprintf("pod-drift-%d", i)), runningRequestsGatewayInstanceID).Result()
		require.NoError(t, err)
		if got == "2" {
			drifted++
		}
	}
	return drifted
}

// A large pod set must be read in bounded pipelines, and every batch's pods must still be
// corrected, not only the first.
func TestReconcileOwnRunningRequests_ReadsPodsInBoundedBatches(t *testing.T) {
	n := runningRequestsDriftReadBatchSize + 50
	store, client, hook := newTestDriftedFleet(t, n)

	reconcilePasses(store, runningRequestsDriftConfirmPasses)

	require.EqualValues(t, runningRequestsDriftReadBatchSize, hook.maxPipelineLen.Load())
	require.Zero(t, countDriftedFields(t, client, n))
}

// A batch whose read fails says nothing about whether its pods' gaps closed, so those pods
// keep the confirmation progress they had instead of starting over -- while the other
// batch carries on.
func TestReconcileOwnRunningRequests_FailedBatchKeepsDriftState(t *testing.T) {
	n := runningRequestsDriftReadBatchSize + 50
	store, client, hook := newTestDriftedFleet(t, n)

	reconcilePasses(store, runningRequestsDriftConfirmPasses-1)
	require.Len(t, store.runningRequestsDrift, n)

	// The confirming pass loses one batch (which one depends on map iteration order).
	hook.failNext.Store(1)
	reconcilePasses(store, 1)
	left := countDriftedFields(t, client, n)
	require.Contains(t, []int{50, runningRequestsDriftReadBatchSize}, left,
		"exactly the failed batch's pods stay uncorrected")
	require.Len(t, store.runningRequestsDrift, left,
		"the failed batch's pods keep their progress; the corrected ones are done")

	// One clean pass finishes them; restarting the confirmation would take five.
	reconcilePasses(store, 1)
	require.Zero(t, countDriftedFields(t, client, n))
	require.Empty(t, store.runningRequestsDrift)
}

func TestReconcileOwnRunningRequests_NoRedisIsNoOp(t *testing.T) {
	store := &Store{}
	store.metaPods.Store(testNamespace+"/"+testPodName, newTestPod(1))
	require.NotPanics(t, func() { store.reconcileOwnRunningRequests() })
	require.Empty(t, store.runningRequestsDrift)
}

// The reconcile must actually run from the heartbeat's hygiene pass, not only when called
// directly.
func TestScheduleRunningRequestsHygiene_RepairsDriftedField(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 0)
	own := runningRequestsGatewayInstanceID
	require.NoError(t, client.HSet(ctx, key, own, "2").Err())

	for i := 0; i < runningRequestsDriftConfirmPasses; i++ {
		store.scheduleRunningRequestsHygiene()
		require.Eventually(t, func() bool { return !store.runningRequestsHygieneBusy.Load() },
			5*time.Second, time.Millisecond)
	}

	got, err := client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "0", got)
}
