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

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func TestIsNodeListEqual(t *testing.T) {
	node := NodeInfo{Name: "kvcache-0", Addr: "10.0.0.1", Port: 18512, Slots: []SlotRange{{Start: 0, End: 15}}}
	withAddr := node
	withAddr.Addr = "10.0.0.2"
	withPort := node
	withPort.Port = 18513
	withSlots := node
	withSlots.Slots = []SlotRange{{Start: 0, End: 7}}
	withName := node
	withName.Name = "kvcache-1"

	for _, tc := range []struct {
		name string
		b    []NodeInfo
		want bool
	}{
		{name: "same node", b: []NodeInfo{node}, want: true},
		{name: "new address", b: []NodeInfo{withAddr}, want: false},
		{name: "new port", b: []NodeInfo{withPort}, want: false},
		{name: "new slots", b: []NodeInfo{withSlots}, want: false},
		{name: "new name", b: []NodeInfo{withName}, want: false},
		{name: "extra node", b: []NodeInfo{node, withName}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isNodeListEqual([]NodeInfo{node}, tc.b))
		})
	}
}

func TestSyncPodsPublishesTheNewAddressOfARecreatedPod(t *testing.T) {
	setFlag(t, &consistentHashingTotalSlots, 16)
	setFlag(t, &consistentHashingVirtualNodeCount, 4)
	setFlag(t, &kvCacheServerRDMAPort, 18512)

	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	old, err := json.Marshal(ClusterNodes{
		Nodes:   []NodeInfo{{Name: "kvcache-0", Addr: "10.0.0.1", Port: 18512, Slots: []SlotRange{{Start: 0, End: 15}}}},
		Version: 3,
	})
	require.NoError(t, err)
	require.NoError(t, mr.Set(InfiniStoreRedisNodeMemberKey, string(old)))

	informer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Pod{}, 0, cache.Indexers{})
	require.NoError(t, informer.GetStore().Add(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kvcache-0",
			Namespace: "default",
			Labels: map[string]string{
				KVCacheLabelKeyIdentifier: "kvcache",
				KVCacheLabelKeyRole:       KVCacheLabelValueRoleCache,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.2"},
	}))

	require.NoError(t, syncPods(ctx, rdb, informer, "kvcache", InfiniStoreBackend{}))

	raw, err := mr.Get(InfiniStoreRedisNodeMemberKey)
	require.NoError(t, err)
	var got ClusterNodes
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.Len(t, got.Nodes, 1)
	assert.Equal(t, "10.0.0.2", got.Nodes[0].Addr)
	assert.Equal(t, int64(4), got.Version)
}

func setFlag(t *testing.T, flag *int, value int) {
	prev := *flag
	*flag = value
	t.Cleanup(func() { *flag = prev })
}
