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
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
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
