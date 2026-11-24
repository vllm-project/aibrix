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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func makeRoutableSnapshotPods(n int, extraLabels map[string]string) []*v1.Pod {
	pods := make([]*v1.Pod, n)
	for i := range pods {
		lbls := map[string]string{
			constants.ModelLabelPort: "8000",
		}
		for k, v := range extraLabels {
			lbls[k] = v
		}
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("pod-%03d", i),
				Namespace: "ns",
				Labels:    lbls,
			},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.%d.%d", i/256, i%256),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	return pods
}

// TestSelectTargetPod_PreservesSharedSnapshotOrderConcurrent drives
// selectTargetPod concurrently across all paths where upstream stages may
// return the shared snapshot slice unchanged (K=0, N<=K, exclusive strategies,
// non-candidate-safe session-affinity including fallbackPod's rand.Shuffle,
// empty external-filter) as well as fresh-slice paths (non-empty
// external-filter and K=16 narrowing) on both all-ready and mixed snapshots.
// Run with -race.
func TestSelectTargetPod_PreservesSharedSnapshotOrderConcurrent(t *testing.T) {
	cases := []struct {
		name           string
		n              int
		addUnready     int
		k              int
		algorithm      types.RoutingAlgorithm
		externalFilter string
	}{
		{
			name:      "K=0 least-request all-ready",
			n:         24,
			k:         0,
			algorithm: least.RouterLeastRequest,
		},
		{
			name:       "K=0 least-request mixed ready/unready",
			n:          24,
			addUnready: 6,
			k:          0,
			algorithm:  least.RouterLeastRequest,
		},
		{
			name:      "N<=K least-request",
			n:         8,
			k:         16,
			algorithm: least.RouterLeastRequest,
		},
		{
			name:       "exclusive slo-least-load mixed ready/unready",
			n:          20,
			addUnready: 4,
			k:          16,
			algorithm:  routing.RouterSLOLeastLoad,
		},
		{
			name:      "K=16 non-candidate-safe session-affinity fallbackPod",
			n:         24,
			k:         16,
			algorithm: routing.RouterSessionAffinity,
		},
		{
			name:       "K=0 session-affinity fallbackPod mixed snapshot",
			n:          16,
			addUnready: 4,
			k:          0,
			algorithm:  routing.RouterSessionAffinity,
		},
		{
			// FilterPodsByLabelSelector allocates a fresh slice for non-empty
			// expressions; empty-filter aliasing is exercised by the cases above.
			name:           "fresh-slice external-filter matching all with K=0",
			n:              20,
			k:              0,
			algorithm:      least.RouterLeastRequest,
			externalFilter: "tier=gpu",
		},
		{
			name:           "fresh-slice external-filter matching all with K=16",
			n:              24,
			k:              16,
			algorithm:      least.RouterLeastRequest,
			externalFilter: "tier=gpu",
		},
		{
			name:           "fresh-slice external-filter matching subset with K=0",
			n:              20,
			k:              0,
			algorithm:      least.RouterLeastRequest,
			externalFilter: "subset=even",
		},
		{
			name:      "K=16 least-request narrowed fresh slice",
			n:         40,
			k:         16,
			algorithm: least.RouterLeastRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pods := makeRoutableSnapshotPods(tc.n, map[string]string{"tier": "gpu"})
			for i, pod := range pods {
				if i%2 == 0 {
					pod.Labels["subset"] = "even"
				}
			}
			unreadyAddrs := make(map[string]struct{}, tc.addUnready)
			for i := 0; i < tc.addUnready; i++ {
				ip := fmt.Sprintf("10.99.0.%d", i+1)
				unreadyAddrs[ip+":8000"] = struct{}{}
				pods = append(pods, &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("unready-%02d", i),
						Namespace: "ns",
						Labels:    map[string]string{constants.ModelLabelPort: "8000", "tier": "gpu"},
					},
					Status: v1.PodStatus{
						PodIP:      ip,
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}},
					},
				})
			}

			store := cache.InitWithPods(cache.New(nil, nil, nil), pods, "m")
			rm := routing.NewRouterManagerWithCache(store)
			rm.Init()
			if tc.algorithm == routing.RouterSLOLeastLoad {
				// Register an exclusive router that reorders readyPodList.All() in
				// place, mirroring PD's subset shuffles, on the local RouterManager.
				rm.Register(routing.RouterSLOLeastLoad, func() (types.Router, error) {
					return mutatingExclusiveRouter{}, nil
				})
				rm.Init()
			}
			s := &Server{cache: store, routerManager: rm}

			podsArr, err := store.ListPodsByModel("m")
			require.NoError(t, err)
			arr, ok := podsArr.(*utils.PodArray)
			require.True(t, ok, "expected *utils.PodArray from cache")

			wantAll := slices.Clone(arr.All())
			wantRoutable := slices.Clone(arr.Routable().All())
			require.Len(t, wantRoutable, tc.n)

			var profile *types.ResolvedConfigProfile
			if tc.k > 0 {
				raw := json.RawMessage(fmt.Sprintf(`{"candidate":{"k":%d}}`, tc.k))
				profile = &types.ResolvedConfigProfile{
					RoutingConfig: raw,
					Routing:       configprofiles.ParseRoutingConfig(raw),
				}
			}

			const workers = 8
			const iters = 40
			var wg sync.WaitGroup
			start := make(chan struct{})
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()
					<-start
					for i := 0; i < iters; i++ {
						reqID := fmt.Sprintf("req-%d-%d", workerID, i)
						ctx := types.NewRoutingContext(context.Background(), tc.algorithm, "m", "hello", reqID, "")
						ctx.ConfigProfile = profile
						addr, err := s.selectTargetPod(context.Background(), ctx, arr, tc.externalFilter)
						ctx.Delete()
						if err != nil {
							t.Errorf("selectTargetPod failed: %v", err)
							return
						}
						if addr == "" {
							t.Errorf("selectTargetPod returned empty target address")
							return
						}
						if _, bad := unreadyAddrs[addr]; bad {
							t.Errorf("selectTargetPod routed to unready pod %s", addr)
							return
						}
					}
				}(w)
			}
			close(start)
			wg.Wait()

			assert.True(t, slices.Equal(arr.All(), wantAll), "shared PodArray.All() order was mutated")
			assert.True(t, slices.Equal(arr.Routable().All(), wantRoutable), "shared PodArray.Routable().All() order was mutated")
		})
	}
}

type mutatingExclusiveRouter struct{}

func (mutatingExclusiveRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	pods := readyPodList.All()
	slices.Reverse(pods)
	ctx.SetTargetPod(pods[0])
	return ctx.TargetAddress(), nil
}

type customPodList struct {
	pods []*v1.Pod
}

func (c *customPodList) Len() int                          { return len(c.pods) }
func (c *customPodList) All() []*v1.Pod                    { return c.pods }
func (c *customPodList) ListByIndex(string) []*v1.Pod      { return c.pods }
func (c *customPodList) Indexes() []string                 { return nil }
func (c *customPodList) ListPortsForPod() map[string][]int { return nil }

func TestApplyConfigProfileFromPodList_UsesCachedRoutableCount(t *testing.T) {
	// Place the config annotation only on an unready pod to verify
	// ResolveModelConfigFrom still inspects podsArr.All(), while
	// requestsPerSecondPerReplica counts only routable pods.
	unreadyAnnotated := podWithReplicaRPS("unready-cfg", 5)
	unreadyAnnotated.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}}

	readyA := makeRoutableSnapshotPods(1, nil)[0]
	readyA.Name = "ready-a"
	readyB := makeRoutableSnapshotPods(1, nil)[0]
	readyB.Name = "ready-b"

	arr := &utils.PodArray{Pods: []*v1.Pod{unreadyAnnotated, readyA, readyB}}
	routingCtx := &types.RoutingContext{Model: "m", RequestID: "req-1"}

	applyConfigProfileFromPodList(routingCtx, arr)
	require.NotNil(t, routingCtx.ConfigProfile)
	// 5 rps/replica * 2 routable pods = 10 rps (1s window), not 15.
	assert.Equal(t, int64(10), routingCtx.ConfigProfile.RequestsPerSecond)
	assert.Equal(t, int64(1), routingCtx.ConfigProfile.RateWindowSeconds)
	assert.Equal(t, string(routing.RouterLeastRequest), routingCtx.ConfigProfile.RoutingStrategy)

	// Warmed *PodArray countRoutablePodsFromList allocates nothing.
	assert.Zero(t, testing.AllocsPerRun(100, func() {
		if got := countRoutablePodsFromList(arr); got != 2 {
			t.Fatalf("countRoutablePodsFromList = %d, want 2", got)
		}
	}))

	// Non-*PodArray PodList falls back to CountRoutablePods and matches.
	fallbackCtx := &types.RoutingContext{Model: "m", RequestID: "req-2"}
	applyConfigProfileFromPodList(fallbackCtx, &customPodList{pods: arr.Pods})
	require.NotNil(t, fallbackCtx.ConfigProfile)
	assert.Equal(t, int64(10), fallbackCtx.ConfigProfile.RequestsPerSecond)
	assert.Equal(t, int64(1), fallbackCtx.ConfigProfile.RateWindowSeconds)

	// Nil PodList is a clean no-op.
	nilCtx := &types.RoutingContext{Model: "m", RequestID: "req-3"}
	applyConfigProfileFromPodList(nilCtx, nil)
	assert.Nil(t, nilCtx.ConfigProfile)
}

func TestValidateModelAvailability_UsesPodArrayRoutable(t *testing.T) {
	unready := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "u-0", Namespace: "ns"},
		Status:     v1.PodStatus{PodIP: "10.0.0.1", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}}},
	}
	storeUnready := cache.InitWithPods(cache.New(nil, nil, nil), []*v1.Pod{unready}, "m-unready")
	sUnready := &Server{cache: storeUnready}
	podsArr, errRes := sUnready.validateModelAvailability("req-u", "m-unready")
	assert.Nil(t, podsArr)
	assert.NotNil(t, errRes)

	ready := makeRoutableSnapshotPods(2, nil)
	storeReady := cache.InitWithPods(cache.New(nil, nil, nil), append(ready, unready), "m-ready")
	sReady := &Server{cache: storeReady}
	podsArr, errRes = sReady.validateModelAvailability("req-r", "m-ready")
	require.Nil(t, errRes)
	require.NotNil(t, podsArr)
	arr, ok := podsArr.(*utils.PodArray)
	require.True(t, ok)
	assert.Equal(t, 2, arr.Routable().Len())
}
