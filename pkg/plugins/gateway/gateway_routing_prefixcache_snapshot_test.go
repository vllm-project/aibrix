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

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func TestIsSnapshot(t *testing.T) {
	pods := makeRoutableSnapshotPods(4, nil)
	snap := &utils.PodArray{Pods: pods}

	assert.False(t, isSnapshot(pods, nil))
	assert.True(t, isSnapshot(pods, snap))
	assert.False(t, isSnapshot(pods[:3], snap), "narrowed prefix slice is not the full snapshot")
	assert.False(t, isSnapshot(pods[1:], snap), "offset suffix slice is not the full snapshot")
	assert.False(t, isSnapshot(slices.Clone(pods), snap), "freshly allocated copy is not the snapshot")
	assert.True(t, isSnapshot(nil, &utils.PodArray{}))
}

// TestSelectTargetPod_PrefixCacheSnapshotReuseGuard verifies that:
//  1. With empty external-filter and no encoders, selectTargetPod passes the
//     routable *utils.PodArray snapshot into SelectCandidatesFromPodList so
//     prefix-cache FindCandidates reuses KeySet/ByKey across requests.
//  2. With a non-empty external-filter, it falls back cleanly so excluded
//     prefix holders are never selected or leaked into SeededPrefixMatch.
//  3. With encoder pods present, it falls back cleanly so encoder pods are
//     excluded from candidate lookup and never selected.
//  4. Concurrent -race execution preserves the shared snapshot order and
//     KeySet/ByKey integrity.
func TestSelectTargetPod_PrefixCacheSnapshotReuseGuard(t *testing.T) {
	raw := json.RawMessage(`{"candidate":{"k":8}}`)
	profile := &types.ResolvedConfigProfile{
		RoutingConfig: raw,
		Routing:       configprofiles.ParseRoutingConfig(raw),
	}
	prompt := strings.Repeat("shared prefix routing prompt ", 32)

	t.Run("reuses routable snapshot when unfiltered and zero encoders", func(t *testing.T) {
		// Need > 2*4*K = 64 pods so candidateScout invokes FindCandidates.
		pods := makeRoutableSnapshotPods(80, map[string]string{"tier": "gpu"})
		store := cache.InitWithPods(cache.New(nil, nil, nil), pods, "m")
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		podsArr, err := store.ListPodsByModel("m")
		require.NoError(t, err)
		arr := podsArr.(*utils.PodArray)
		wantOrder := slices.Clone(arr.All())

		// Seed prefix onto one pod via a normal routing decision.
		seedCtx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", prompt, "seed", "")
		seedCtx.ConfigProfile = profile
		seedAddr, err := s.selectTargetPod(context.Background(), seedCtx, arr, "")
		seedCtx.Delete()
		require.NoError(t, err)
		require.NotEmpty(t, seedAddr)

		// Drive concurrent requests under -race; all should succeed and leave snapshot order untouched.
		var wg sync.WaitGroup
		start := make(chan struct{})
		for w := 0; w < 6; w++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				<-start
				for i := 0; i < 25; i++ {
					ctx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", prompt, fmt.Sprintf("r-%d-%d", id, i), "")
					ctx.ConfigProfile = profile
					addr, err := s.selectTargetPod(context.Background(), ctx, arr, "")
					ctx.Delete()
					if err != nil || addr == "" {
						t.Errorf("selectTargetPod failed: addr=%q err=%v", addr, err)
						return
					}
				}
			}(w)
		}
		close(start)
		wg.Wait()

		assert.True(t, slices.Equal(arr.All(), wantOrder), "shared snapshot order was mutated")
		assert.Equal(t, 80, len(arr.Routable().KeySet()), "routable snapshot KeySet should cover all ready pods")
	})

	t.Run("external-filter falls back cleanly and never routes to filtered-out prefix holder", func(t *testing.T) {
		pods := makeRoutableSnapshotPods(80, map[string]string{"pool": "b"})
		// Mark pods[0..9] as pool=a and seed the prefix on pods[0] (pool=a).
		for i := 0; i < 10; i++ {
			pods[i].Labels["pool"] = "a"
		}
		store := cache.InitWithPods(cache.New(nil, nil, nil), pods, "m")
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		podsArr, err := store.ListPodsByModel("m")
		require.NoError(t, err)
		arr := podsArr.(*utils.PodArray)

		// Route with pool=a to seed prefix onto a pool=a pod.
		seedCtx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", prompt, "seed-a", "")
		seedCtx.ConfigProfile = profile
		poolAAddr, err := s.selectTargetPod(context.Background(), seedCtx, arr, "pool=a")
		seedCtx.Delete()
		require.NoError(t, err)

		poolAAddrs := make(map[string]struct{}, 10)
		for i := 0; i < 10; i++ {
			poolAAddrs[pods[i].Status.PodIP+":8000"] = struct{}{}
		}
		require.Contains(t, poolAAddrs, poolAAddr)

		// Now route with pool=b (> 64 pods, so candidateScout runs FindCandidates).
		// Must never reuse the full snapshot or select the pool=a holder.
		for i := 0; i < 20; i++ {
			ctx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", prompt, fmt.Sprintf("req-b-%d", i), "")
			ctx.ConfigProfile = profile
			addr, err := s.selectTargetPod(context.Background(), ctx, arr, "pool=b")
			ctx.Delete()
			require.NoError(t, err)
			assert.NotContains(t, poolAAddrs, addr, "filtered-out pool=a prefix holder must not be routed to")
		}
	})

	t.Run("mixed encoder fleet falls back cleanly and never routes to encoder pods", func(t *testing.T) {
		pods := makeRoutableSnapshotPods(80, nil)
		encoderAddrs := make(map[string]struct{}, 5)
		for i := 0; i < 5; i++ {
			pods[i].Labels[pd.RoleLabel] = encode.Role
			encoderAddrs[pods[i].Status.PodIP+":8000"] = struct{}{}
		}
		store := cache.InitWithPods(cache.New(nil, nil, nil), pods, "m")
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		podsArr, err := store.ListPodsByModel("m")
		require.NoError(t, err)
		arr := podsArr.(*utils.PodArray)

		// Seed the prefix directly onto an encoder pod (pods[0]) so that a
		// regression reusing routableSnapshot despite SplitEncoders would have
		// FindCandidates return pods[0] and route to an encoder.
		seedCtx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", prompt, "seed-enc", "")
		router, err := rm.Select(seedCtx)
		require.NoError(t, err)
		updater, ok := router.(types.PostRouteUpdater)
		require.True(t, ok, "prefix-cache router should implement PostRouteUpdater")
		require.NoError(t, updater.PostRouteUpdate(seedCtx, arr, pods[0]))
		seedCtx.Delete()

		for i := 0; i < 20; i++ {
			ctx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", prompt, fmt.Sprintf("enc-%d", i), "")
			ctx.ConfigProfile = profile
			addr, err := s.selectTargetPod(context.Background(), ctx, arr, "")
			ctx.Delete()
			require.NoError(t, err)
			assert.NotContains(t, encoderAddrs, addr, "encoder pod must never be routed to")
		}
	})
}
