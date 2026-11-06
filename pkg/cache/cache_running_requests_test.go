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
	"math"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
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
// identity) as live in the liveness ZSET for tests verifying gateway liveness tracking.
// Note that runningRequestsAdmitScript always considers selfField live regardless of
// whether it is in the ZSET, matching overlaySelfRunningRequests on the read side.
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
	require.True(t, admit.Options().ContextTimeoutEnabled)
	require.False(t, shared.Options().ContextTimeoutEnabled, "the shared client's deadline behavior is unchanged")
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
	admitted, counted, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 2, nil)
	require.ErrorContains(t, err, "key does not exist in the cache")
	require.False(t, admitted)
	require.False(t, counted)
}

// TestAdmitPodRunningRequest_NilContextSkipsReservation pins the nil-ctx contract: a
// direct-cache caller passes no routing context that could be stamped, and their request
// is never tracked, so no unclaim path exists for them (donePodStats reads the claim off
// the stamped context, and ReleasePodRunningRequest is for a request that reached
// routing). Reserving before the Redis round trip as if the request were tracked then
// strands one claimed slot on the pod's local counter for every nil-ctx admit, donePodStats
// never unwinds it. The nil-ctx path therefore reads the live local count as the script's
// floor, exactly as this API did before the reservation existed.
func TestAdmitPodRunningRequest_NilContextSkipsReservation(t *testing.T) {
	for _, tt := range []struct {
		name     string
		seed     string
		limit    int64
		hook     *admitScriptHook
		admitted bool
		counted  bool
	}{
		{"confirmed admission", "", 4, nil, true, true},
		{"pod at cap rejects", "4", 4, nil, false, false},
		{"script unreachable falls back", "", 4, &admitScriptHook{process: failAdmitScript}, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestRunningRequestsClient(t)
			if tt.hook != nil {
				client.AddHook(*tt.hook)
			}
			store := NewForTest()
			store.redisClient = client
			pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
			store.addPod(pod)
			metaPod, ok := store.metaPods.Load(utils.GeneratePodKey(pod.Namespace, pod.Name))
			require.True(t, ok)
			if tt.seed != "" {
				seedLiveGateway(t, client)
				key := runningRequestsKey(testNamespace, testPodName)
				require.NoError(t, client.HSet(context.Background(), key, runningRequestsGatewayInstanceID, tt.seed).Err())
			}

			admitted, counted, err := store.AdmitPodRunningRequest(testPodName, testNamespace, tt.limit, nil)
			require.NoError(t, err)
			require.Equal(t, tt.admitted, admitted)
			require.Equal(t, tt.counted, counted)
			require.Zero(t, atomic.LoadInt32(&metaPod.runningRequests),
				"a nil-ctx admit must not leave a claim on the pod's local counter: nothing can unclaim it")

			if tt.admitted && counted {
				val, err := client.HGet(context.Background(), runningRequestsKey(testNamespace, testPodName),
					runningRequestsGatewayInstanceID).Result()
				require.NoError(t, err)
				require.Equal(t, "1", val, "the script's own +1 for the admitted request must be visible in Redis")
			}
		})
	}
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

// TestAdmitRunningRequest_SelfAlwaysLiveWithoutHeartbeat verifies that the executing gateway's
// own field is always treated as live by runningRequestsAdmitScript, even if its heartbeat in
// runningRequestsGatewaysKey has not landed yet or was delayed. Without liveSet[selfField] = true,
// the script would treat its own hash field as dead, sum to 0, and admit all callers beyond the limit.
func TestAdmitRunningRequest_SelfAlwaysLiveWithoutHeartbeat(t *testing.T) {
	oldReadTimeout := runningRequestsReadTimeout
	runningRequestsReadTimeout = 30 * time.Second
	t.Cleanup(func() { runningRequestsReadTimeout = oldReadTimeout })

	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	// Intentionally DO NOT call seedLiveGateway: runningRequestsGatewaysKey is completely empty.

	const (
		limit       = 3
		concurrency = 20
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
		"exactly limit callers must be admitted even when no gateway heartbeat is present in Redis")

	key := runningRequestsKey(testNamespace, testPodName)
	val, err := client.HGet(context.Background(), key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(limit), val, "the field must reflect exactly the admitted count")
}

// TestAdmitRunningRequest_SelfLiveWithOtherGateways verifies that when another gateway
// is live in Redis and self is absent from runningRequestsGatewaysKey, both the other
// gateway's running count and self's count are correctly summed together.
func TestAdmitRunningRequest_SelfLiveWithOtherGateways(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	key := runningRequestsKey(testNamespace, testPodName)

	// Seed another gateway as live in the gateways ZSET and with in-flight requests in the hash.
	const otherGatewayID = "gw-other-live"
	require.NoError(t, client.ZAdd(ctx, runningRequestsGatewaysKey,
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: otherGatewayID}).Err())
	require.NoError(t, client.HSet(ctx, key, otherGatewayID, 2).Err())
	// Self is intentionally absent from runningRequestsGatewaysKey.

	// Limit is 3. Other gateway has 2.
	// First call from self should observe sum = 2, so it is admitted (limit 3 > 2), bringing total to 3.
	admitted, counted, running := store.admitRunningRequest(testNamespace, testPodName, 3, 0)
	require.True(t, admitted)
	require.True(t, counted)
	require.EqualValues(t, 2, running)

	// Second call from self should observe sum = 3 (2 from other + 1 from self) and be rejected.
	admitted, counted, running = store.admitRunningRequest(testNamespace, testPodName, 3, 0)
	require.False(t, admitted)
	require.False(t, counted)
	require.EqualValues(t, 3, running)

	val, err := client.HGet(ctx, key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, "1", val, "self field in hash should be incremented exactly once")
}

// TestAdmitPodRunningRequest_ConcurrentFallbackNeverExceedsLimit is the fallback
// counterpart of TestAdmitRunningRequest_ConcurrentCallsNeverExceedLimit: when the admit
// round trip errors, admitRunningRequest decides from localRunning alone. Before
// admission-time reservations that fallback admitted every caller that hit it under the
// cap -- with no slot reservation, concurrent callers before their addPodStats all read
// the same zero count and a live Redis read does not exist to serialize them, so an
// expired round trip under a tight limit admitted two requests at once. The under-count
// behind the flaky e2e in issue #3346
// (TestModelReplicaRequestsInflightLimit/concurrent_requests_beyond_cap_are_rejected:
// expected 1 admitted under cap 1, got 2). AdmitPodRunningRequest now claims the slot on
// the pod's local running-requests counter before the round trip, so concurrent callers
// serialize on that claim however Redis behaves. Fire far more concurrent callers than
// the limit, with every admit failing, and arrive at exactly limit admissions.
func TestAdmitPodRunningRequest_ConcurrentFallbackNeverExceedsLimit(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	// A refused connection, admitScriptDidNotRun's dial error: the script provably never
	// ran, so there is no half-applied hash state to reason about -- this test is purely
	// about the fallback decision. The Redis store stays untouched throughout.
	client.AddHook(admitScriptHook{process: failAdmitScript})
	store := NewForTest()
	store.redisClient = client
	pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
	store.addPod(pod)

	const (
		limit       = 5
		concurrency = 50
	)
	var (
		admittedCount atomic.Int32
		sawCounted    atomic.Bool
		sawUnreserved atomic.Bool
	)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(index int) {
			defer wg.Done()
			// One routing context per caller: the reservation stamping is per request,
			// the same as the gateway would hand in (and shared contexts would race the
			// flags across goroutines).
			routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model",
				"", strconv.Itoa(index), "")
			if ok, counted, _ := store.AdmitPodRunningRequest(testPodName, testNamespace, limit, routingCtx); ok {
				admittedCount.Add(1)
				if counted {
					sawCounted.Store(true)
				}
				if !routingCtx.ReplicaInflightReserved {
					sawUnreserved.Store(true)
				}
			}
		}(i)
	}
	wg.Wait()

	require.False(t, sawCounted.Load(),
		"with the script provably not running, no admission can be counted in the hash")
	require.False(t, sawUnreserved.Load(),
		"every admitted request must hold its reservation for request tracking to skip its increment against")
	require.EqualValues(t, limit, admittedCount.Load(),
		"the locally-serialized fallback must still be a hard cap under cap-hitting concurrency")

	// Every admitted request holds exactly one slot on the local counter until request
	// tracking takes over its bookkeeping; every rejected one must have left no claim.
	metaPod, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
	require.True(t, ok)
	require.EqualValues(t, limit, atomic.LoadInt32(&metaPod.runningRequests),
		" admitted requests hold exactly their slots -- no stray claim, none above the admitted count")

	// The script never ran, so nothing may exist in Redis's copy.
	_, err := client.HGet(context.Background(), runningRequestsKey(testNamespace, testPodName),
		runningRequestsGatewayInstanceID).Result()
	require.ErrorIs(t, err, redis.Nil, "no failed admit may have touched the hash")
}

// TestAdmitPodRunningRequest_FallbackSeesFullyCommittedSlot is the mixed case the
// concurrent fallback test cannot pin down deterministically: one request admitted
// through the script (its +1 confirmed in the hash) and then -- with request tracking
// not having run in between, so the count the fallback will read is exactly the
// admission reservation -- Redis starts erroring and a second request faces cap 1.
// The fallback cannot read the hash (that is what falling back means), but the slot's
// reservation makes the still-running first request visible locally anyway. Without the
// reservation both requests were admitted at once, which is the over-admission shape
// the e2e in issue #3346 asserted against.
func TestAdmitPodRunningRequest_FallbackSeesFullyCommittedSlot(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := NewForTest()
	store.redisClient = client
	pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
	store.addPod(pod)
	seedLiveGateway(t, client)
	key := runningRequestsKey(testNamespace, testPodName)

	routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-1", "")
	admitted, counted, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 1, routingCtx)
	require.NoError(t, err)
	require.True(t, admitted)
	require.True(t, counted)
	require.True(t, routingCtx.ReplicaInflightReserved)
	val, err := client.HGet(context.Background(), key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, "1", val, "the committed request holds the only slot")
	metaPod, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
	require.True(t, ok)
	require.EqualValues(t, 1, atomic.LoadInt32(&metaPod.runningRequests),
		"the reservation must have made the commit locally visible already, before request tracking runs")

	// From here on every admit round trip errors like a blipped connection.
	client.AddHook(admitScriptHook{process: failAdmitScript})

	routingCtx2 := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-2", "")
	admitted2, counted2, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 1, routingCtx2)
	require.NoError(t, err)
	require.False(t, admitted2,
		"the fallback must honor the still-running first request, seen through its admission reservation rather than a (failing) Redis read")
	require.False(t, counted2)
	require.False(t, routingCtx2.ReplicaInflightReserved, "a rejected request must not stamp a reservation")
	val, err = client.HGet(context.Background(), key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, "1", val, "the rejection must not have added anything to the hash")
	require.EqualValues(t, 1, atomic.LoadInt32(&metaPod.runningRequests),
		"the rejected request must not hold a slot that nothing will release")
}

// TestAdmitPodRunningRequest_ScriptRejectUnclaimsSlot covers the reject half of the slot
// claim: another gateway already holds the cap, so the script's sum rejects after this
// gateway already claimed a slot. The claim must be unclaimed, or every rejected local
// admission would consume cap for a request that was never sent -- and nothing would
// ever undo that, since addPodStats never runs for a 429'd request.
func TestAdmitPodRunningRequest_ScriptRejectUnclaimsSlot(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := NewForTest()
	store.redisClient = client
	pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
	store.addPod(pod)
	seedLiveGateway(t, client)
	key := runningRequestsKey(testNamespace, testPodName)
	// This gateway's own field already at the cap, as a committed (counted) admission
	// that request tracking has not processed yet would look once tracking is done
	// elsewhere: the next request can only lose by the script's sum.
	require.NoError(t, client.HSet(context.Background(), key, runningRequestsGatewayInstanceID, 1).Err())

	routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-1", "")
	admitted, counted, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 1, routingCtx)
	require.NoError(t, err)
	require.False(t, admitted)
	require.False(t, counted, "a rejection never applied the script's +1")
	require.False(t, routingCtx.ReplicaInflightReserved, "the unclaimed slot must not stamp the rejected request")
	val, err := client.HGet(context.Background(), key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, "1", val, "the rejection must not have applied a second increment to the hash")
	metaPod, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
	require.True(t, ok)
	require.EqualValues(t, 0, atomic.LoadInt32(&metaPod.runningRequests),
		"the rollback must have freed the claimed slot")

	// And the pod is admittable again the moment its running count clears.
	require.NoError(t, client.HSet(context.Background(), key, runningRequestsGatewayInstanceID, 0).Err())
	routingCtx2 := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-2", "")
	admitted2, counted2, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 1, routingCtx2)
	require.NoError(t, err)
	require.True(t, admitted2)
	require.True(t, counted2)
	require.EqualValues(t, 1, atomic.LoadInt32(&metaPod.runningRequests),
		"the next admitted request holds its slot again")
}

// TestReleasePodRunningRequest_UnclaimsAdmissionSlot pairs the release path with its
// reservation: an admitted-but-untracked request (a replace or prefill failure during
// route-with-early-admission, before AddRequestCount ever runs) holds both its admitted
// hash increment and its slot claim, and ReleasePodRunningRequest must undo both.
func TestReleasePodRunningRequest_UnclaimsAdmissionSlot(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := NewForTest()
	store.redisClient = client
	pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
	store.addPod(pod)
	seedLiveGateway(t, client)
	key := runningRequestsKey(testNamespace, testPodName)

	routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-1", "")
	admitted, counted, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 1, routingCtx)
	require.NoError(t, err)
	require.True(t, admitted)
	require.True(t, counted)
	metaPod, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
	require.True(t, ok)
	require.EqualValues(t, 1, atomic.LoadInt32(&metaPod.runningRequests))

	store.ReleasePodRunningRequest(testPodName, testNamespace, counted, routingCtx.ReplicaInflightReservedGen)

	require.EqualValues(t, 0, atomic.LoadInt32(&metaPod.runningRequests),
		"the released request must not keep its slot claim")
	val, err := client.HGet(context.Background(), key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, "0", val, "the released request must not keep its hash increment either")
}

// TestAdmitPodRunningRequest_AfterRequestTrackingDoesNotLeakLocalSlot verifies that when a
// request has already been tracked (e.g. queueRouter / SLO router calling AddRequestCount
// before admission), AdmitPodRunningRequest skips reserving a local slot and skips setting
// ReplicaInflightReserved. The local counter must return to baseline upon DoneRequestCount.
func TestAdmitPodRunningRequest_AfterRequestTrackingDoesNotLeakLocalSlot(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := NewForTest()
	store.redisClient = client
	pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
	store.addPod(pod)
	seedLiveGateway(t, client)

	metaPod, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
	require.True(t, ok)

	t.Run("admitted_after_track", func(t *testing.T) {
		require.EqualValues(t, 0, atomic.LoadInt32(&metaPod.runningRequests))
		routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-slo-1", "")
		routingCtx.SetTargetPod(pod)

		// 1. Initial track (e.g. queueRouter.Route)
		store.AddRequestCount(routingCtx, routingCtx.RequestID, "test-model")
		require.EqualValues(t, 1, atomic.LoadInt32(&metaPod.runningRequests))
		require.True(t, routingCtx.StatsAdded())

		// 2. Admission runs after track
		admitted, _, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 2, routingCtx)
		require.NoError(t, err)
		require.True(t, admitted)
		require.False(t, routingCtx.ReplicaInflightReserved, "should skip local reservation because stats already added")
		require.EqualValues(t, 1, atomic.LoadInt32(&metaPod.runningRequests), "local counter must not be double-incremented")

		// 3. Downstream AddRequestCount is a deduped no-op
		store.AddRequestCount(routingCtx, routingCtx.RequestID, "test-model")
		require.EqualValues(t, 1, atomic.LoadInt32(&metaPod.runningRequests))

		// 4. DoneRequestCount decrements to baseline
		store.DoneRequestCount(routingCtx, routingCtx.RequestID, "test-model", 0)
		require.EqualValues(t, 0, atomic.LoadInt32(&metaPod.runningRequests), "local counter must return to baseline")
	})

	t.Run("rejected_after_track", func(t *testing.T) {
		require.EqualValues(t, 0, atomic.LoadInt32(&metaPod.runningRequests))
		routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-slo-2", "")
		routingCtx.SetTargetPod(pod)

		// 1. Initial track
		store.AddRequestCount(routingCtx, routingCtx.RequestID, "test-model")
		require.EqualValues(t, 1, atomic.LoadInt32(&metaPod.runningRequests))

		// 2. Admission checks limit 1: pod is already at cap (localRunning is 1 >= limit 1)
		admitted, _, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 1, routingCtx)
		require.NoError(t, err)
		require.False(t, admitted)
		require.False(t, routingCtx.ReplicaInflightReserved)
		require.EqualValues(t, 1, atomic.LoadInt32(&metaPod.runningRequests))

		// 3. Request finishes after 429
		store.DoneRequestCount(routingCtx, routingCtx.RequestID, "test-model", 0)
		require.EqualValues(t, 0, atomic.LoadInt32(&metaPod.runningRequests), "local counter must return to baseline after reject")
	})
}

// TestReleasePodRunningRequest_UncountedAdmissionDoesNotDecrementRedis verifies that when
// admission falls back to the local counter (counted=false, e.g. because the admit script
// had a dial error), ReleasePodRunningRequest with live Redis unclaims the local counter
// without decrementing Redis.
func TestReleasePodRunningRequest_UncountedAdmissionDoesNotDecrementRedis(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := NewForTest()
	store.redisClient = client
	pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
	store.addPod(pod)
	seedLiveGateway(t, client)
	key := runningRequestsKey(testNamespace, testPodName)

	// Seed Redis hash field to 2 and local counter to 2 (simulating 2 requests already running)
	require.NoError(t, client.HSet(context.Background(), key, runningRequestsGatewayInstanceID, "2").Err())
	metaPod, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
	require.True(t, ok)
	atomic.StoreInt32(&metaPod.runningRequests, 2)

	// Set admitRedisClient to an unreachable client so admitRunningRequest fails with dial error
	// while store.redisClient remains alive.
	store.admitRedisClient = redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1", // unreachable port
		MaxRetries:  -1,
		DialTimeout: 10 * time.Millisecond,
	})
	t.Cleanup(func() { _ = store.admitRedisClient.Close() })

	routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-fallback", "")
	admitted, counted, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 4, routingCtx)
	require.NoError(t, err)
	require.True(t, admitted)
	require.False(t, counted, "admit should be uncounted because script dial failed")
	require.True(t, routingCtx.ReplicaInflightReserved, "local slot should be claimed")
	require.EqualValues(t, 3, atomic.LoadInt32(&metaPod.runningRequests), "local counter incremented to 3 by reservation")

	// Redis hash field must still be 2 (the script never ran)
	val, err := client.HGet(context.Background(), key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, "2", val)

	// Release with counted=false: must unclaim local counter without decrementing Redis
	store.ReleasePodRunningRequest(testPodName, testNamespace, counted, routingCtx.ReplicaInflightReservedGen)

	require.EqualValues(t, 2, atomic.LoadInt32(&metaPod.runningRequests), "local counter must return to 2")
	valAfter, err := client.HGet(context.Background(), key, runningRequestsGatewayInstanceID).Result()
	require.NoError(t, err)
	require.Equal(t, "2", valAfter, "Redis hash field must NOT be decremented when counted=false")
}

// TestAdmitPodRunningRequest_FlapBranches verifies that when a pod flaps between admission
// and request tracking:
// 1. Same generation (resumed): reservation is carried over and addPodStats skips its increment.
// 2. Fresh generation: reservation is stranded on the orphan, new object tracks normally.
// 3. Mid-gap snapshot: tracking hits the snapshot before re-add, and re-add resumes with the right count.
// 4. Concurrent flap under -race with simultaneous admits, tracking, and delete+add.
func TestAdmitPodRunningRequest_FlapBranches(t *testing.T) {
	t.Run("same_generation_resumed", func(t *testing.T) {
		client := newTestRunningRequestsClient(t)
		seedLiveGateway(t, client)
		store := NewForTest()
		store.redisClient = client
		pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
		store.addPod(pod)

		routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-flap-1", "")
		admitted, counted, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 2, routingCtx)
		require.NoError(t, err)
		require.True(t, admitted)
		require.True(t, counted)
		require.True(t, routingCtx.ReplicaInflightReserved)
		routingCtx.ReplicaInflightAdmitted = counted

		// Delete pod (moved to snapshot with same generation)
		store.deletePod(pod)

		// Re-add pod within grace with same IP (resumed generation)
		store.addPod(pod)

		resumedPod, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
		require.True(t, ok)
		require.Equal(t, routingCtx.ReplicaInflightReservedGen, resumedPod.statsGeneration)

		// addPodStats skips increment because generation matches
		routingCtx.SetTargetPod(pod)
		store.AddRequestCount(routingCtx, routingCtx.RequestID, "test-model")
		require.EqualValues(t, 1, atomic.LoadInt32(&resumedPod.runningRequests))

		// donePodStats decrements to 0
		store.DoneRequestCount(routingCtx, routingCtx.RequestID, "test-model", 0)
		require.EqualValues(t, 0, atomic.LoadInt32(&resumedPod.runningRequests))
	})

	t.Run("fresh_generation_tracks_normally", func(t *testing.T) {
		client := newTestRunningRequestsClient(t)
		seedLiveGateway(t, client)
		store := NewForTest()
		store.redisClient = client
		pod1 := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
		store.addPod(pod1)

		routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-flap-2", "")
		admitted, counted, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 2, routingCtx)
		require.NoError(t, err)
		require.True(t, admitted)
		require.True(t, counted)
		require.True(t, routingCtx.ReplicaInflightReserved)
		routingCtx.ReplicaInflightAdmitted = counted
		oldGen := routingCtx.ReplicaInflightReservedGen

		// Delete pod1
		store.deletePod(pod1)

		// Re-add with a DIFFERENT IP to force a fresh generation
		pod2 := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.2")
		store.addPod(pod2)

		freshPod, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
		require.True(t, ok)
		require.NotEqual(t, oldGen, freshPod.statsGeneration)

		// addPodStats sees generation mismatch, so it does a normal increment
		routingCtx.SetTargetPod(pod2)
		store.AddRequestCount(routingCtx, routingCtx.RequestID, "test-model")
		require.EqualValues(t, 1, atomic.LoadInt32(&freshPod.runningRequests))

		// donePodStats decrements to 0
		store.DoneRequestCount(routingCtx, routingCtx.RequestID, "test-model", 0)
		require.EqualValues(t, 0, atomic.LoadInt32(&freshPod.runningRequests))
	})

	t.Run("mid_gap_snapshot_tracks_and_resumes", func(t *testing.T) {
		client := newTestRunningRequestsClient(t)
		seedLiveGateway(t, client)
		store := NewForTest()
		store.redisClient = client
		pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
		store.addPod(pod)

		routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-flap-3", "")
		admitted, counted, err := store.AdmitPodRunningRequest(testPodName, testNamespace, 2, routingCtx)
		require.NoError(t, err)
		require.True(t, admitted)
		require.True(t, counted)
		require.True(t, routingCtx.ReplicaInflightReserved)
		routingCtx.ReplicaInflightAdmitted = counted

		// Delete pod (moved to snapshot with same generation)
		store.deletePod(pod)

		// AddRequestCount while pod is in snapshot (deleted, not yet re-added)
		// Hits snap != nil branch in addPodStats: skips increment because generation matches
		routingCtx.SetTargetPod(pod)
		store.AddRequestCount(routingCtx, routingCtx.RequestID, "test-model")

		snap, ok := store.recentlyDeletedPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
		require.True(t, ok)
		require.EqualValues(t, 1, atomic.LoadInt32(&snap.runningRequests))

		// Re-add pod with same IP: resumes from snapshot with runningRequests = 1
		store.addPod(pod)
		resumedPod, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
		require.True(t, ok)
		require.EqualValues(t, 1, atomic.LoadInt32(&resumedPod.runningRequests))

		// donePodStats decrements to 0
		store.DoneRequestCount(routingCtx, routingCtx.RequestID, "test-model", 0)
		require.EqualValues(t, 0, atomic.LoadInt32(&resumedPod.runningRequests))
	})

	t.Run("concurrent_flap_under_race", func(t *testing.T) {
		client := newTestRunningRequestsClient(t)
		seedLiveGateway(t, client)
		store := NewForTest()
		store.redisClient = client
		pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
		store.addPod(pod)

		var wg sync.WaitGroup
		stopCh := make(chan struct{})
		var admitCount atomic.Int32

		// Goroutine 1: Flapper (deletes and re-adds pod)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					store.deletePod(pod)
					time.Sleep(100 * time.Microsecond)
					store.addPod(pod)
					time.Sleep(100 * time.Microsecond)
				}
			}
		}()

		// Goroutines 2-6: Concurrent requests
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				j := 0
				for {
					select {
					case <-stopCh:
						return
					default:
					}
					j++
					reqID := fmt.Sprintf("req-race-%d-%d", workerID, j)
					ctx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", reqID, "")
					admitted, counted, _ := store.AdmitPodRunningRequest(testPodName, testNamespace, 10, ctx)
					if admitted {
						admitCount.Add(1)
						ctx.ReplicaInflightAdmitted = counted
						ctx.SetTargetPod(pod)
						store.AddRequestCount(ctx, reqID, "test-model")
						time.Sleep(50 * time.Microsecond)
						store.DoneRequestCount(ctx, reqID, "test-model", 0)
					}
				}
			}(i)
		}

		time.Sleep(100 * time.Millisecond)
		close(stopCh)
		wg.Wait()

		// Re-add pod if the flapper terminated right after a delete
		store.addPod(pod)

		require.Greater(t, admitCount.Load(), int32(0), "some requests should have been admitted during the run")

		livePod, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
		require.True(t, ok)
		require.EqualValues(t, 0, atomic.LoadInt32(&livePod.runningRequests), "local counter should return to baseline")

		require.Eventually(t, func() bool {
			rKey := runningRequestsKey(testNamespace, testPodName)
			val, err := client.HGet(context.Background(), rKey, runningRequestsGatewayInstanceID).Result()
			if err == redis.Nil {
				return true
			}
			if err != nil {
				return false
			}
			count, _ := strconv.ParseInt(val, 10, 64)
			return count == 0
		}, 2*time.Second, 10*time.Millisecond, "redis field should drain to 0")
	})
}

// TestAdmitPodRunningRequest_MissingOnLockedReloadReturnsError verifies that if a pod is present
// during the initial load but removed before the stripe lock is acquired, AdmitPodRunningRequest
// returns an error (fail-open) rather than a spurious at-capacity rejection (admitted=false, err=nil).
func TestAdmitPodRunningRequest_MissingOnLockedReloadReturnsError(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	seedLiveGateway(t, client)
	store := NewForTest()
	store.redisClient = client
	pod := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
	store.addPod(pod)

	key := utils.GeneratePodKey(testNamespace, testPodName)
	mu := store.podStatsLockFor(key)

	// Hold the stripe lock so AdmitPodRunningRequest blocks after its initial Load.
	mu.Lock()

	done := make(chan struct{})
	var (
		admitted bool
		counted  bool
		err      error
	)
	routingCtx := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "req-miss", "")
	go func() {
		admitted, counted, err = store.AdmitPodRunningRequest(testPodName, testNamespace, 10, routingCtx)
		close(done)
	}()

	// Give the goroutine a brief moment to execute the initial Load and block on mu.Lock().
	time.Sleep(10 * time.Millisecond)

	// Delete the pod while the lock is held, so curMeta, ok := c.metaPods.Load(key) returns false.
	store.metaPods.Delete(key)
	mu.Unlock()

	<-done
	require.Error(t, err, "should return an error when pod vanished on locked reload")
	require.Contains(t, err.Error(), "key does not exist in the cache")
	require.False(t, admitted)
	require.False(t, counted)
	require.False(t, routingCtx.ReplicaInflightReserved)
}

// TestReleasePodRunningRequest_FreshGenerationIgnored verifies that ReleasePodRunningRequest
// will not decrement a fresh generation object if the reservation was made on an earlier generation.
func TestReleasePodRunningRequest_FreshGenerationIgnored(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := NewForTest()
	store.redisClient = client
	pod1 := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.1")
	store.addPod(pod1)
	seedLiveGateway(t, client)

	meta1, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
	require.True(t, ok)
	oldGen := meta1.statsGeneration

	// Replace pod with fresh generation
	store.deletePod(pod1)
	pod2 := requestTrackerTestPod(testPodName, testNamespace, "test-model", "10.0.0.2")
	store.addPod(pod2)

	meta2, ok := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
	require.True(t, ok)
	atomic.StoreInt32(&meta2.runningRequests, 1)

	// Release with oldGen: must not decrement meta2
	store.ReleasePodRunningRequest(testPodName, testNamespace, false, oldGen)
	require.EqualValues(t, 1, atomic.LoadInt32(&meta2.runningRequests), "fresh generation counter must not be decremented by stale release")
}

// TestReserveLocalRunningSlot_ClampsAboveMaxInt32 verifies that limits above math.MaxInt32
// are clamped to math.MaxInt32 and do not overflow.
func TestReserveLocalRunningSlot_ClampsAboveMaxInt32(t *testing.T) {
	pod := &Pod{}
	local, ok := reserveLocalRunningSlot(pod, math.MaxInt64)
	require.True(t, ok)
	require.EqualValues(t, 0, local)
	require.EqualValues(t, 1, atomic.LoadInt32(&pod.runningRequests))
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

// driftFaultHook watches the HMGET pipelines reconcileOwnRunningRequests sends: it records the
// largest one, runs onRead (if set) as the read goes out -- after the pass has sampled the
// local count, before Redis answers, which is where real traffic lands on a slow Redis -- and
// fails the next failNext of them. While failEval is set it also fails the correction script.
type driftFaultHook struct {
	maxPipelineLen atomic.Int64
	failNext       atomic.Int64
	failEval       atomic.Bool
	onRead         func()
}

func (h *driftFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *driftFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "eval" && h.failEval.Load() {
			return errors.New("injected eval failure")
		}
		return next(ctx, cmd)
	}
}

func (h *driftFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if len(cmds) > 0 && cmds[0].Name() == "hmget" {
			if n := int64(len(cmds)); n > h.maxPipelineLen.Load() {
				h.maxPipelineLen.Store(n)
			}
			if h.onRead != nil {
				h.onRead()
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
// count of 0 -- a leaked count on every pod -- behind a driftFaultHook.
func newTestDriftedFleet(t *testing.T, n int) (*Store, *redis.Client, *driftFaultHook) {
	t.Helper()
	client := newTestRunningRequestsClient(t)
	hook := &driftFaultHook{}
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

// A request starting during the Redis read moves the local count and then the field, so the
// pass sees them apart by that request even though nothing has drifted. Sustained across the
// confirmation window (a load ramp) that gap used to survive and be "corrected" into a real
// error: the field ended one below the local count.
func TestReconcileOwnRunningRequests_IgnoresTrafficThatMovesDuringTheRead(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 5)
	own := runningRequestsGatewayInstanceID
	require.NoError(t, client.HSet(ctx, key, own, "5").Err()) // field == local: no drift
	pod, ok := store.metaPods.Load(testNamespace + "/" + testPodName)
	require.True(t, ok)
	hook := &driftFaultHook{onRead: func() {
		atomic.AddInt32(&pod.runningRequests, 1)
		require.NoError(t, client.HIncrBy(ctx, key, own, 1).Err())
	}}
	client.AddHook(hook)

	reconcilePasses(store, 3*runningRequestsDriftConfirmPasses)

	got, err := client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(int(atomic.LoadInt32(&pod.runningRequests))), got,
		"the field tracked the local count throughout; no correction should have been applied")
}

// A pass that could not take a clean sample neither advances nor resets a pod's confirmation:
// real drift keeps counting on the clean passes around it.
func TestReconcileOwnRunningRequests_UnsampledPassKeepsDriftProgress(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 0)
	own := runningRequestsGatewayInstanceID
	require.NoError(t, client.HSet(ctx, key, own, "2").Err()) // a leaked count
	pod, ok := store.metaPods.Load(testNamespace + "/" + testPodName)
	require.True(t, ok)
	hook := &driftFaultHook{}
	client.AddHook(hook)

	reconcilePasses(store, runningRequestsDriftConfirmPasses-1)
	require.Len(t, store.runningRequestsDrift, 1)

	// A request starts during the read, so this pass has no clean sample. It must not be the
	// confirming pass: counted, it would correct now, and against the moved local count.
	hook.onRead = func() { atomic.StoreInt32(&pod.runningRequests, 1) }
	reconcilePasses(store, 1)
	hook.onRead = nil
	got, err := client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "2", got, "an unsampled pass must not count toward confirmation")
	require.Len(t, store.runningRequestsDrift, 1, "nor may it discard the progress made so far")

	// The request finishes. One clean pass is now the confirming one; had the unsampled pass
	// reset the state, it would take five more.
	atomic.StoreInt32(&pod.runningRequests, 0)
	reconcilePasses(store, 1)
	got, err = client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "0", got)
}

// A correction that fails to land is not retried on the spot: emitting it spent the confirmation,
// so the pod has to hold its gap for a full window again before the next attempt. That is a delay,
// not a loss -- the field is still corrected once Redis accepts the write.
func TestReconcileOwnRunningRequests_FailedCorrectionIsReconfirmedThenRetried(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 0)
	own := runningRequestsGatewayInstanceID
	require.NoError(t, client.HSet(ctx, key, own, "2").Err())
	hook := &driftFaultHook{}
	client.AddHook(hook)

	hook.failEval.Store(true)
	reconcilePasses(store, runningRequestsDriftConfirmPasses)
	got, err := client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "2", got, "the confirming pass's correction was rejected")
	require.Empty(t, store.runningRequestsDrift, "emitting the correction restarts the confirmation")

	hook.failEval.Store(false)
	reconcilePasses(store, runningRequestsDriftConfirmPasses-1)
	got, err = client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "2", got, "a full window must be held again before the retry")

	reconcilePasses(store, 1)
	got, err = client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "0", got)
}

// A field that does not parse as a count says nothing about whether the gap closed, so it
// neither advances the confirmation nor discards the progress made before it.
func TestReconcileOwnRunningRequests_UnparseableFieldKeepsDriftProgress(t *testing.T) {
	ctx := context.Background()
	store, client, key := newTestReconcileStore(t, 0)
	own := runningRequestsGatewayInstanceID
	require.NoError(t, client.HSet(ctx, key, own, "2").Err())

	reconcilePasses(store, runningRequestsDriftConfirmPasses-1)
	require.Len(t, store.runningRequestsDrift, 1)

	require.NoError(t, client.HSet(ctx, key, own, "garbage").Err())
	reconcilePasses(store, 1)
	require.Len(t, store.runningRequestsDrift, 1, "an unreadable pass must not discard the progress made so far")
	got, err := client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "garbage", got, "and must not be mistaken for the confirming pass")

	require.NoError(t, client.HSet(ctx, key, own, "2").Err())
	reconcilePasses(store, 1)
	got, err = client.HGet(ctx, key, own).Result()
	require.NoError(t, err)
	require.Equal(t, "0", got, "one readable pass completes the confirmation; a reset would need five more")
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
