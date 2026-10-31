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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

type seededDecisionRouter struct {
	t     *testing.T
	err   error
	seeds int
}

func (r *seededDecisionRouter) CandidateScores(ctx *types.RoutingContext, pods types.PodList) ([]float64, []bool, error) {
	r.seeds++
	assert.Nil(r.t, ctx.SeededPrefixMatch(), "a retry must not reuse the previous attempt's matches")
	ctx.SetSeededPrefixMatch(&types.SeededPrefixMatch{Model: "this decision"})
	return make([]float64, pods.Len()), make([]bool, pods.Len()), nil
}

func (r *seededDecisionRouter) ScoreAll(_ *types.RoutingContext, pods types.PodList) ([]float64, []bool, error) {
	return make([]float64, pods.Len()), make([]bool, pods.Len()), nil
}

func (r *seededDecisionRouter) Polarity() types.Polarity { return types.PolarityMost }

func (r *seededDecisionRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	require.NotNil(r.t, ctx.SeededPrefixMatch(), "seeding results remain available during routing")
	assert.Equal(r.t, "this decision", ctx.SeededPrefixMatch().Model)
	if r.err != nil {
		return "", r.err
	}
	ctx.SetTargetPod(pods.All()[0])
	return ctx.TargetAddress(), nil
}

func TestSelectTargetPodClearsSeededPrefixMatch(t *testing.T) {
	pods := make([]*v1.Pod, 4)
	for i := range pods {
		pods[i] = &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: string(rune('a' + i)), Namespace: "ns"},
			Status: v1.PodStatus{PodIP: "10.0.0.1", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}}
	}
	store := cache.NewWithPodsForTest(pods, "model")
	t.Cleanup(store.Close)
	router := &seededDecisionRouter{t: t}
	rm := routing.NewRouterManager()
	rm.RegisterProvider(routing.RouterLoadBalance, func(*types.RoutingContext) (types.Router, error) { return router, nil })
	s := &Server{cache: store, routerManager: rm}
	ctx := types.NewRoutingContext(context.Background(), routing.RouterLoadBalance, "model", "prompt", "retry", "")
	defer ctx.Delete()
	zero, two := 0, 2
	ctx.ConfigProfile = &types.ResolvedConfigProfile{Routing: &types.RoutingConfig{
		Candidate: &types.CandidateProfileConfig{K: &two},
		AutoBlend: &types.AutoBlendProfileConfig{LoadBalanceWeight: &zero, LeastRequestWeight: &zero},
	}}
	for _, routeErr := range []error{nil, errors.New("routing failed")} {
		router.err = routeErr
		ctx.SetSeededPrefixMatch(&types.SeededPrefixMatch{Model: "previous decision"})
		_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		if routeErr == nil {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, routeErr)
		}
		assert.Nil(t, ctx.SeededPrefixMatch(), "results are released on success and failure")
	}
	assert.Equal(t, 2, router.seeds)
	ctx.SetSeededPrefixMatch(&types.SeededPrefixMatch{Model: "previous decision"})
	_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{}, "")
	require.Error(t, err)
	assert.Nil(t, ctx.SeededPrefixMatch(), "early returns also release results")
}
