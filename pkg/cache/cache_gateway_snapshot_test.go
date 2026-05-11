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
