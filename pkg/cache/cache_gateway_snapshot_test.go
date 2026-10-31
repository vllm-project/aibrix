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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	testGatewaySelf = "gw-self"
	testPodName     = "pod-a"
	testNamespace   = "default"
)

func TestGatewayPodSnapshotKeyAndPattern(t *testing.T) {
	key := gatewayPodSnapshotKey("gw-1", testNamespace, testPodName)
	require.Equal(t, "aibrix:pod:gw-1:default:pod-a", key)

	pattern := gatewayPodSnapshotPattern(testNamespace, testPodName)
	require.Equal(t, "aibrix:pod:*:default:pod-a", pattern)
}

func TestUpsertAndGetGatewayPodSnapshot(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	oldGatewayPodName := gatewayPodName
	gatewayPodName = testGatewaySelf
	t.Cleanup(func() { gatewayPodName = oldGatewayPodName })

	pod := &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testPodName,
				Namespace: testNamespace,
				UID:       types.UID("uid-a"),
			},
			Spec: v1.PodSpec{
				NodeName: "node-a",
			},
		},
	}
	atomic.StoreInt32(&pod.runningRequests, 7)
	atomic.StoreInt64(&pod.completedRequests, 11)

	store := &Store{redisClient: client}
	require.NoError(t, store.UpsertGatewayPodSnapshot(ctx, pod))

	got, err := store.GetGatewayPodSnapshot(ctx, pod)
	require.NoError(t, err)
	require.Equal(t, testGatewaySelf, got["gateway_instance_id"])
	require.Equal(t, "uid-a", got["pod_uid"])
	require.Equal(t, testPodName, got["pod_name"])
	require.Equal(t, testNamespace, got["namespace"])
	require.Equal(t, "node-a", got["node_name"])
	require.Equal(t, "7", got["requests_running"])
	require.Equal(t, "11", got["seq"])
	require.NotEmpty(t, got["update_time"])
}

func TestGetAllGatewayPodSnapshots(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := &Store{redisClient: client}
	pod := &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testPodName,
				Namespace: testNamespace,
			},
		},
	}

	require.NoError(t, client.HSet(ctx, gatewayPodSnapshotKey("gw-1", testNamespace, testPodName),
		map[string]any{"gateway_instance_id": "gw-1", "requests_running": "3"}).Err())
	require.NoError(t, client.HSet(ctx, gatewayPodSnapshotKey("gw-2", testNamespace, testPodName),
		map[string]any{"gateway_instance_id": "gw-2", "requests_running": "5"}).Err())
	require.NoError(t, client.HSet(ctx, gatewayPodSnapshotKey("gw-9", testNamespace, "pod-b"),
		map[string]any{"gateway_instance_id": "gw-9", "requests_running": "9"}).Err())

	snapshots, err := store.GetAllGatewayPodSnapshots(ctx, pod)
	require.NoError(t, err)
	require.Len(t, snapshots, 2)

	found := map[string]bool{}
	for _, snapshot := range snapshots {
		found[snapshot["gateway_instance_id"]] = true
	}
	require.True(t, found["gw-1"])
	require.True(t, found["gw-2"])
}

func TestInitGatewaySnapshotSyncRefreshesCache(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	oldGatewayPodName := gatewayPodName
	gatewayPodName = testGatewaySelf
	t.Cleanup(func() { gatewayPodName = oldGatewayPodName })

	store := &Store{redisClient: client}
	pod := &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testPodName,
				Namespace: testNamespace,
				UID:       types.UID("uid-a"),
			},
			Spec: v1.PodSpec{
				NodeName: "node-a",
			},
		},
	}
	atomic.StoreInt32(&pod.runningRequests, 4)
	atomic.StoreInt64(&pod.completedRequests, 12)
	store.metaPods.Store(testNamespace+"/"+testPodName, pod)

	// Seed one remote gateway snapshot so sync reads cross-gateway keys too.
	require.NoError(t, client.HSet(ctx, gatewayPodSnapshotKey("gw-remote", testNamespace, testPodName),
		map[string]any{
			"gateway_instance_id": "gw-remote",
			"namespace":           testNamespace,
			"pod_name":            testPodName,
			"requests_running":    "2",
		}).Err())

	stopCh := make(chan struct{})
	initGatewaySnapshotSync(store, stopCh)
	t.Cleanup(func() { close(stopCh) })

	var cache map[string][]map[string]string
	require.Eventually(t, func() bool {
		raw := store.gatewaySnapshotCache.Load()
		if raw == nil {
			return false
		}
		cache = raw.(map[string][]map[string]string)
		return len(cache[utils.GeneratePodKey(testNamespace, testPodName)]) >= 2
	}, 2*time.Second, 50*time.Millisecond)

	entries := cache[utils.GeneratePodKey(testNamespace, testPodName)]
	require.Len(t, entries, 2)

	var foundSelf, foundRemote bool
	for _, fields := range entries {
		switch fields["gateway_instance_id"] {
		case testGatewaySelf:
			foundSelf = true
			require.Equal(t, "4", fields["requests_running"])
			require.Equal(t, "12", fields["seq"])
		case "gw-remote":
			foundRemote = true
			require.Equal(t, "2", fields["requests_running"])
		}
	}
	require.True(t, foundSelf)
	require.True(t, foundRemote)

	// The in-memory estimate adds the remote gateway's 2 to this gateway's own 4, without Redis.
	require.Eventually(t, func() bool {
		return store.GetPodsRunningRequestsApprox([]*v1.Pod{pod.Pod})[utils.GeneratePodKey(testNamespace, testPodName)] == 6
	}, 2*time.Second, 50*time.Millisecond)
	// This gateway's own count is read live, not from what it last wrote to Redis.
	atomic.StoreInt32(&pod.runningRequests, 9)
	require.Equal(t, int64(11), store.GetPodsRunningRequestsApprox([]*v1.Pod{pod.Pod})[utils.GeneratePodKey(testNamespace, testPodName)])
}

func TestRemoteRunningByPodSkipsSelfAndSumsOthers(t *testing.T) {
	grouped := map[string][]map[string]string{
		"ns/a": {
			{"gateway_instance_id": "self", "requests_running": "5"},
			{"gateway_instance_id": "gw-1", "requests_running": "2"},
			{"gateway_instance_id": "gw-2", "requests_running": "3"},
		},
		"ns/b": {{"gateway_instance_id": "self", "requests_running": "7"}},
		"ns/c": {{"gateway_instance_id": "gw-1", "requests_running": "not a number"}},
	}
	assert.Equal(t, map[string]int64{"ns/a": 5}, remoteRunningByPod(grouped, "self"))
}

func TestGetPodsRunningRequestsApproxWithoutSnapshot(t *testing.T) {
	store := &Store{}
	known := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}}}
	atomic.StoreInt32(&known.runningRequests, 3)
	store.metaPods.Store("ns/a", known)
	unknown := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}}
	assert.Equal(t, map[string]int64{"ns/a": 3}, store.GetPodsRunningRequestsApprox([]*v1.Pod{known.Pod, unknown, nil}),
		"without a snapshot it is this gateway's own count; pods the cache does not know are omitted")
}

func TestInitGatewaySnapshotSyncHGetAllBatchingLoadsAllKeys(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	oldGatewayPodName := gatewayPodName
	gatewayPodName = testGatewaySelf
	t.Cleanup(func() { gatewayPodName = oldGatewayPodName })

	store := &Store{redisClient: client}
	n := gatewaySnapshotHGetAllBatchSize + 50
	for i := range n {
		podName := fmt.Sprintf("pod-batch-%d", i)
		gw := fmt.Sprintf("gw-batch-%d", i)
		require.NoError(t, client.HSet(ctx, gatewayPodSnapshotKey(gw, testNamespace, podName),
			map[string]any{
				"gateway_instance_id": gw,
				"namespace":           testNamespace,
				"pod_name":            podName,
				"requests_running":    "1",
			}).Err())
	}

	stopCh := make(chan struct{})
	initGatewaySnapshotSync(store, stopCh)
	t.Cleanup(func() { close(stopCh) })

	require.Eventually(t, func() bool {
		raw := store.gatewaySnapshotCache.Load()
		if raw == nil {
			return false
		}
		cache := raw.(map[string][]map[string]string)
		return len(cache) >= n
	}, 3*time.Second, 50*time.Millisecond)

	raw := store.gatewaySnapshotCache.Load()
	cache := raw.(map[string][]map[string]string)
	require.Len(t, cache, n)
	for i := range n {
		pk := utils.GeneratePodKey(testNamespace, fmt.Sprintf("pod-batch-%d", i))
		require.Len(t, cache[pk], 1)
		require.Equal(t, "1", cache[pk][0]["requests_running"])
	}
}

// snapshotFaultHook injects Redis failures into the snapshot sync: SCAN fails while failScan
// is set, and any pipeline carrying an HGETALL fails while failReads is set.
type snapshotFaultHook struct {
	failScan  atomic.Bool
	failReads atomic.Bool
}

func (h *snapshotFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *snapshotFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "scan" && h.failScan.Load() {
			return errors.New("injected scan failure")
		}
		return next(ctx, cmd)
	}
}

func (h *snapshotFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if h.failReads.Load() {
			for _, cmd := range cmds {
				if cmd.Name() == "hgetall" {
					return errors.New("injected read failure")
				}
			}
		}
		return next(ctx, cmds)
	}
}

// newSnapshotSyncUnderTest starts the snapshot sync against a miniredis holding one remote
// gateway's snapshot of testPodName, with failures injectable through the returned hook.
func newSnapshotSyncUnderTest(t *testing.T) (store *Store, client *redis.Client, hook *snapshotFaultHook, remoteKey string) {
	t.Helper()
	mr := miniredis.RunT(t)
	client = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	hook = &snapshotFaultHook{}
	client.AddHook(hook)

	oldGatewayPodName := gatewayPodName
	gatewayPodName = testGatewaySelf
	t.Cleanup(func() { gatewayPodName = oldGatewayPodName })

	remoteKey = gatewayPodSnapshotKey("gw-remote", testNamespace, testPodName)
	setRemoteRunning(t, client, remoteKey, "2")

	store = &Store{redisClient: client}
	stopCh := make(chan struct{})
	initGatewaySnapshotSync(store, stopCh)
	t.Cleanup(func() { close(stopCh) })
	return store, client, hook, remoteKey
}

func setRemoteRunning(t *testing.T, client *redis.Client, key, running string) {
	t.Helper()
	require.NoError(t, client.HSet(context.Background(), key, map[string]any{
		"gateway_instance_id": "gw-remote",
		"namespace":           testNamespace,
		"pod_name":            testPodName,
		"requests_running":    running,
	}).Err())
}

// cachedRemoteRunning is the requests_running the sync cache currently holds for the remote
// gateway's snapshot of testPodName; ok is false when the cache or that entry is absent.
func cachedRemoteRunning(store *Store) (running string, ok bool) {
	raw := store.gatewaySnapshotCache.Load()
	if raw == nil {
		return "", false
	}
	entries := raw.(map[string][]map[string]string)[utils.GeneratePodKey(testNamespace, testPodName)]
	if len(entries) != 1 {
		return "", false
	}
	return entries[0]["requests_running"], true
}

// A scan that fails used to break out of the scan loop and replace the cache with an empty (or
// partial) map, dropping every other gateway's contribution until the next successful tick.
func TestInitGatewaySnapshotSync_KeepsPreviousSnapshotWhenScanFails(t *testing.T) {
	store, client, hook, remoteKey := newSnapshotSyncUnderTest(t)
	require.Eventually(t, func() bool { v, ok := cachedRemoteRunning(store); return ok && v == "2" },
		2*time.Second, 20*time.Millisecond, "sync must populate the remote snapshot before the failure")

	hook.failScan.Store(true)
	// Change the value behind the failure so a cache that merely happens to equal "2" cannot pass.
	setRemoteRunning(t, client, remoteKey, "9")
	require.Never(t, func() bool { v, ok := cachedRemoteRunning(store); return !ok || v != "2" },
		10*gatewaySnapshotSyncInterval, 20*time.Millisecond, "a failed scan must keep the last known snapshot")

	hook.failScan.Store(false)
	require.Eventually(t, func() bool { v, ok := cachedRemoteRunning(store); return ok && v == "9" },
		2*time.Second, 20*time.Millisecond, "the next successful tick must pick up the new value")
}

// A failed HGETALL batch keeps only those keys' last known values; it must not blank them.
func TestInitGatewaySnapshotSync_KeepsPreviousValuesWhenReadFails(t *testing.T) {
	store, client, hook, remoteKey := newSnapshotSyncUnderTest(t)
	require.Eventually(t, func() bool { v, ok := cachedRemoteRunning(store); return ok && v == "2" },
		2*time.Second, 20*time.Millisecond)

	hook.failReads.Store(true)
	setRemoteRunning(t, client, remoteKey, "9")
	require.Never(t, func() bool { v, ok := cachedRemoteRunning(store); return !ok || v != "2" },
		10*gatewaySnapshotSyncInterval, 20*time.Millisecond, "a failed read must keep the key's last known value")

	hook.failReads.Store(false)
	require.Eventually(t, func() bool { v, ok := cachedRemoteRunning(store); return ok && v == "9" },
		2*time.Second, 20*time.Millisecond)
}

// Carrying a value over is only for a failed scan or read. A key the scan no longer returns is
// really gone (the gateway's snapshot expired) and must leave the cache.
func TestInitGatewaySnapshotSync_DropsKeyThatLeftRedis(t *testing.T) {
	store, client, _, remoteKey := newSnapshotSyncUnderTest(t)
	require.Eventually(t, func() bool { _, ok := cachedRemoteRunning(store); return ok },
		2*time.Second, 20*time.Millisecond)

	require.NoError(t, client.Del(context.Background(), remoteKey).Err())

	require.Eventually(t, func() bool {
		raw := store.gatewaySnapshotCache.Load()
		return raw != nil && len(raw.(map[string][]map[string]string)) == 0
	}, 2*time.Second, 20*time.Millisecond, "a key absent from a successful scan must not be carried over")
}

// Before any scan has succeeded there is no snapshot to keep. Publishing an empty one would
// read as "no other gateway has requests running", so the cache stays unset instead (its
// consumer, syncRunningRequestsGlobally, skips the update while it is nil).
func TestInitGatewaySnapshotSync_PublishesNothingBeforeFirstSuccessfulScan(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	hook := &snapshotFaultHook{}
	hook.failScan.Store(true)
	client.AddHook(hook)

	store := &Store{redisClient: client}
	stopCh := make(chan struct{})
	initGatewaySnapshotSync(store, stopCh)
	t.Cleanup(func() { close(stopCh) })

	require.Never(t, func() bool { return store.gatewaySnapshotCache.Load() != nil },
		10*gatewaySnapshotSyncInterval, 20*time.Millisecond)

	hook.failScan.Store(false)
	require.Eventually(t, func() bool { return store.gatewaySnapshotCache.Load() != nil },
		2*time.Second, 20*time.Millisecond)
}

// A hash under the snapshot prefix that lacks its namespace or pod_name (written by another
// tool, or malformed) has no pod to attach to; grouping it would put keys like "/" in the cache.
func TestInitGatewaySnapshotSync_SkipsSnapshotsMissingPodIdentity(t *testing.T) {
	ctx := context.Background()
	store, client, _, remoteKey := newSnapshotSyncUnderTest(t)
	require.Eventually(t, func() bool { v, ok := cachedRemoteRunning(store); return ok && v == "2" },
		2*time.Second, 20*time.Millisecond)

	for gw, fields := range map[string]map[string]any{
		"gw-bad-none":      {"gateway_instance_id": "gw-bad-none", "requests_running": "1"},
		"gw-bad-no-pod":    {"gateway_instance_id": "gw-bad-no-pod", "namespace": testNamespace, "requests_running": "1"},
		"gw-bad-no-ns":     {"gateway_instance_id": "gw-bad-no-ns", "pod_name": testPodName, "requests_running": "1"},
		"gw-bad-empty-val": {"gateway_instance_id": "gw-bad-empty-val", "namespace": "", "pod_name": "", "requests_running": "1"},
	} {
		require.NoError(t, client.HSet(ctx, gatewayPodSnapshotKey(gw, testNamespace, testPodName), fields).Err())
	}
	// The scan that sees this value also sees every malformed hash above, written before it.
	setRemoteRunning(t, client, remoteKey, "3")
	require.Eventually(t, func() bool { v, ok := cachedRemoteRunning(store); return ok && v == "3" },
		2*time.Second, 20*time.Millisecond)

	cache := store.gatewaySnapshotCache.Load().(map[string][]map[string]string)
	require.Len(t, cache, 1, "only the well-formed snapshot is grouped; got keys %v", cache)
	require.Contains(t, cache, utils.GeneratePodKey(testNamespace, testPodName))
	require.Len(t, cache[utils.GeneratePodKey(testNamespace, testPodName)], 1)
}

// The scan-failure twin of PublishesNothingBeforeFirstSuccessfulScan: when every read fails on the
// first tick there is no earlier value to fall back to, so the keys simply vanish from the result.
// Publishing that would be an empty map -- which, unlike nil, syncRunningRequestsGlobally treats as
// "no other gateway has requests running".
func TestInitGatewaySnapshotSync_PublishesNothingBeforeFirstSuccessfulRead(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	hook := &snapshotFaultHook{}
	hook.failReads.Store(true)
	client.AddHook(hook)
	setRemoteRunning(t, client, gatewayPodSnapshotKey("gw-remote", testNamespace, testPodName), "2")

	store := &Store{redisClient: client}
	stopCh := make(chan struct{})
	initGatewaySnapshotSync(store, stopCh)
	t.Cleanup(func() { close(stopCh) })

	require.Never(t, func() bool { return store.gatewaySnapshotCache.Load() != nil },
		10*gatewaySnapshotSyncInterval, 20*time.Millisecond, "an unread first snapshot must not be published")

	hook.failReads.Store(false)
	require.Eventually(t, func() bool { v, ok := cachedRemoteRunning(store); return ok && v == "2" },
		2*time.Second, 20*time.Millisecond, "the first tick that reads every key must publish")
}

// A key Redis rejects outright (here a string under the snapshot prefix, so HGETALL answers
// WRONGTYPE) fails the same way every tick. It must be skipped, not treated as unread: otherwise it
// would hold back the first snapshot forever, and in steady state pin every key sharing its pipeline
// to a stale value.
func TestInitGatewaySnapshotSync_SkipsKeysRedisRejects(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Set(ctx, "aibrix:pod:not-a-snapshot", "x", 0).Err())
	remoteKey := gatewayPodSnapshotKey("gw-remote", testNamespace, testPodName)
	setRemoteRunning(t, client, remoteKey, "2")

	store := &Store{redisClient: client}
	stopCh := make(chan struct{})
	initGatewaySnapshotSync(store, stopCh)
	t.Cleanup(func() { close(stopCh) })

	require.Eventually(t, func() bool { v, ok := cachedRemoteRunning(store); return ok && v == "2" },
		2*time.Second, 20*time.Millisecond, "a rejected key must not block the first snapshot")

	setRemoteRunning(t, client, remoteKey, "9")
	require.Eventually(t, func() bool { v, ok := cachedRemoteRunning(store); return ok && v == "9" },
		2*time.Second, 20*time.Millisecond, "nor may it freeze the keys read in the same pipeline")
	cache := store.gatewaySnapshotCache.Load().(map[string][]map[string]string)
	require.Len(t, cache, 1, "the rejected key is not a snapshot and must not appear in the cache")
}

// capturedErrorLog records what a rateLimitedErrorLog would have written at Error level.
type capturedErrorLog struct {
	mu    sync.Mutex
	lines []capturedErrorLine
}

type capturedErrorLine struct {
	msg        string
	suppressed int
}

func (c *capturedErrorLog) emit(_ error, msg string, keysAndValues ...interface{}) {
	line := capturedErrorLine{msg: msg}
	for i := 0; i+1 < len(keysAndValues); i += 2 {
		if keysAndValues[i] == "suppressedSinceLastLog" {
			line.suppressed, _ = keysAndValues[i+1].(int)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func (c *capturedErrorLog) snapshot() []capturedErrorLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedErrorLine(nil), c.lines...)
}

func newCapturingErrorLog(interval time.Duration) (*rateLimitedErrorLog, *capturedErrorLog) {
	captured := &capturedErrorLog{}
	l := newRateLimitedErrorLog(interval)
	l.emit = captured.emit
	return l, captured
}

// A failure that recurs every tick is logged once per interval, with a count of what it
// stands for; different kinds of failure are limited independently, so one cannot hide another.
func TestRateLimitedErrorLog(t *testing.T) {
	now := time.Unix(1000, 0)
	l, captured := newCapturingErrorLog(30 * time.Second)
	l.now = func() time.Time { return now }
	err := errors.New("boom")

	l.log("scan", err, "scan failed")
	l.log("scan", err, "scan failed")
	l.log("scan", err, "scan failed")
	l.log("read", err, "read failed")
	require.Equal(t, []capturedErrorLine{{"scan failed", 0}, {"read failed", 0}}, captured.snapshot(),
		"the first occurrence of each event is logged; repeats within the interval are not")

	now = now.Add(29 * time.Second)
	l.log("scan", err, "scan failed")
	require.Len(t, captured.snapshot(), 2, "still inside the interval")

	now = now.Add(time.Second)
	l.log("scan", err, "scan failed")
	lines := captured.snapshot()
	require.Len(t, lines, 3)
	require.Equal(t, capturedErrorLine{"scan failed", 3}, lines[2],
		"the next line says how many occurrences were swallowed since the last one")
}

// startSnapshotSyncWithErrorLog starts the sync over a miniredis holding one remote snapshot,
// with its Error log captured. The interval is long enough that a test sees at most one line per
// kind of failure, however many 100ms ticks it runs for.
func startSnapshotSyncWithErrorLog(t *testing.T, hook *snapshotFaultHook) (*Store, *capturedErrorLog) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	client.AddHook(hook)
	setRemoteRunning(t, client, gatewayPodSnapshotKey("gw-remote", testNamespace, testPodName), "2")

	errLog, captured := newCapturingErrorLog(time.Hour)
	store := &Store{redisClient: client, gatewaySnapshotErrorLog: errLog}
	stopCh := make(chan struct{})
	initGatewaySnapshotSync(store, stopCh)
	t.Cleanup(func() { close(stopCh) })
	return store, captured
}

func requireOneErrorLine(t *testing.T, captured *capturedErrorLog, wantInMsg string) {
	t.Helper()
	require.Eventually(t, func() bool { return len(captured.snapshot()) >= 1 },
		2*time.Second, 20*time.Millisecond, "a failing sync must reach the Error log")
	require.Never(t, func() bool { return len(captured.snapshot()) > 1 },
		10*gatewaySnapshotSyncInterval, 20*time.Millisecond, "and only once, however many ticks fail")
	require.Contains(t, captured.snapshot()[0].msg, wantInMsg)
}

// Keep-last makes a failed scan invisible at V(4): the snapshot just stops refreshing. It has to
// surface at Error level, once per interval.
func TestInitGatewaySnapshotSync_LogsFailedScansAtErrorLevel(t *testing.T) {
	hook := &snapshotFaultHook{}
	hook.failScan.Store(true)
	_, captured := startSnapshotSyncWithErrorLog(t, hook)

	requireOneErrorLine(t, captured, "cannot scan Redis")
}

func TestInitGatewaySnapshotSync_LogsFailedFirstReadAtErrorLevel(t *testing.T) {
	hook := &snapshotFaultHook{}
	hook.failReads.Store(true)
	_, captured := startSnapshotSyncWithErrorLog(t, hook)

	requireOneErrorLine(t, captured, "not publishing the first snapshot")
}

// A healthy sync logs nothing at Error level, and a later read failure is reported as the
// snapshot holding its previous values.
func TestInitGatewaySnapshotSync_LogsReadFailureAfterHealthyTicks(t *testing.T) {
	hook := &snapshotFaultHook{}
	store, captured := startSnapshotSyncWithErrorLog(t, hook)
	require.Eventually(t, func() bool { v, ok := cachedRemoteRunning(store); return ok && v == "2" },
		2*time.Second, 20*time.Millisecond)
	require.Never(t, func() bool { return len(captured.snapshot()) > 0 },
		5*gatewaySnapshotSyncInterval, 20*time.Millisecond, "a healthy sync must not log at Error level")

	hook.failReads.Store(true)
	requireOneErrorLine(t, captured, "using their previous values")
}

// syncRunningRequestsGlobally is the snapshot cache's only consumer, and the first-tick fix
// depends on how it treats the two kinds of "no data": an unset cache must leave the metric
// alone, while a published empty snapshot is a real answer (no other gateway has requests) and
// yields this gateway's local count alone.
func TestSyncRunningRequestsGlobally_SnapshotCacheContract(t *testing.T) {
	oldGatewayPodName := gatewayPodName
	gatewayPodName = testGatewaySelf
	t.Cleanup(func() { gatewayPodName = oldGatewayPodName })
	podKey := utils.GeneratePodKey(testNamespace, testPodName)
	const stale = 7.0 // what the metric held before the call

	tests := []struct {
		name  string
		cache map[string][]map[string]string // nil leaves the cache unset
		want  float64
	}{
		{name: "unset cache leaves the metric alone", cache: nil, want: stale},
		{name: "published empty snapshot counts only the local requests", cache: map[string][]map[string]string{}, want: 3},
		{
			name: "sums other gateways and ignores this gateway's own entry",
			cache: map[string][]map[string]string{podKey: {
				{"gateway_instance_id": "gw-other", "requests_running": "5"},
				{"gateway_instance_id": testGatewaySelf, "requests_running": "100"},
			}},
			want: 8,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &Store{}
			if tt.cache != nil {
				store.gatewaySnapshotCache.Store(tt.cache)
			}
			pod := newTestPod(3)
			pod.Metrics.Store(metrics.RealtimeNumRequestsRunning, &metrics.SimpleMetricValue{Value: stale})

			store.syncRunningRequestsGlobally(pod)

			got, ok := pod.Metrics.Load(metrics.RealtimeNumRequestsRunning)
			require.True(t, ok)
			require.Equal(t, tt.want, got.GetSimpleValue())
		})
	}
}
