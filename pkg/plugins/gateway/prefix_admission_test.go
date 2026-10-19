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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

// postRouteFunc is a router whose PostRouteUpdate records the pods it was called for.
type postRouteFunc struct {
	routeFunc
	updated []string
}

func (r *postRouteFunc) PostRouteUpdate(_ *types.RoutingContext, _ types.PodList, pod *v1.Pod) error {
	r.updated = append(r.updated, pod.Name)
	return nil
}

func prefixCacheManager(router types.Router) *routing.RouterManager {
	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterPrefixCache, func(*types.RoutingContext) (types.Router, error) {
		return router, nil
	})
	return manager
}

// The prefix-cache routers index the prompt on their target from inside Route, so they get
// the same early admission as PD: a router that admits its target through AdmitTarget is
// admitted by the gateway's requestsInflight admission, and the post-routing check is
// skipped. Without it AdmitTarget was a no-op outside PD, and the prompt was indexed on a
// pod that enforceReplicaInflight then rejected.
func Test_selectTargetPod_PrefixCacheAdmitsTargetInsideRoute(t *testing.T) {
	podA := podWithReplicaInflight("a", 1)
	podB := podWithReplicaInflight("b", 1)
	pods := []*v1.Pod{podA, podB}
	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequests", mock.Anything).Return(map[string]int64{"ns/a": 0, "ns/b": 0}, nil)
	mockCache.On("AdmitPodRunningRequest", "b", "ns", int64(1)).Return(false, false, nil).Once()

	indexed := false
	router := routeFunc(func(ctx *types.RoutingContext, _ types.PodList) (string, error) {
		if err := ctx.AdmitTarget(podB); err != nil {
			return "", err
		}
		indexed = true
		ctx.SetTargetPod(podB)
		return ctx.TargetAddress(), nil
	})
	s := &Server{cache: mockCache, routerManager: prefixCacheManager(router)}

	routingCtx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", "", "r1", "")
	applyConfigProfile(routingCtx, pods)
	_, err := s.selectTargetPod(context.Background(), routingCtx, &utils.PodArray{Pods: pods}, "")
	require.ErrorIs(t, err, types.ErrTargetReplicaInflightExceeded)
	assert.False(t, indexed, "the router must not index the prompt on a pod that rejected the request")
	assert.True(t, routingCtx.ReplicaInflightChecked)
	mockCache.AssertExpectations(t)
}

// With one ready pod the gateway skips Route and runs the router's PostRouteUpdate itself,
// which is where a prefix-cache router indexes the prompt. The pod is admitted first, so a
// request it has no room for is rejected without being indexed there, and an admitted one
// is not admitted a second time by the post-routing check.
func Test_selectTargetPod_SingleCandidateAdmitsBeforePostRouteUpdate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		admit bool
	}{
		{name: "rejected", admit: false},
		{name: "admitted", admit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := podWithReplicaInflight("a", 1)
			mockCache := &MockCache{}
			mockCache.On("GetPodsRunningRequests", mock.Anything).Return(map[string]int64{"ns/a": 0}, nil)
			mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1)).Return(tc.admit, tc.admit, nil).Once()
			router := &postRouteFunc{routeFunc: func(*types.RoutingContext, types.PodList) (string, error) {
				t.Fatal("the single-candidate fast path must not call Route")
				return "", nil
			}}
			s := &Server{cache: mockCache, routerManager: prefixCacheManager(router)}

			routingCtx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", "", "r1", "")
			applyConfigProfile(routingCtx, []*v1.Pod{pod})
			_, err := s.selectTargetPod(context.Background(), routingCtx, &utils.PodArray{Pods: []*v1.Pod{pod}}, "")
			if !tc.admit {
				require.ErrorIs(t, err, types.ErrTargetReplicaInflightExceeded)
				assert.Empty(t, router.updated, "a rejected request must not reach PostRouteUpdate")
			} else {
				require.NoError(t, err)
				assert.Equal(t, []string{"a"}, router.updated)
				assert.True(t, routingCtx.ReplicaInflightAdmitted)
				assert.Nil(t, s.enforceReplicaInflight(context.Background(), "m", routingCtx))
			}
			mockCache.AssertExpectations(t)
		})
	}
}
