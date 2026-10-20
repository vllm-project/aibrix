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

package routingalgorithms

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/vtc"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The default auto-blend turns a bare vtc-basic into a multi-strategy blend,
// which scores vtc-basic through ScoreAll and never calls its Route. The
// request's tokens must still be charged to its user.
func TestSelectVTCBasicBlendChargesUserTokens(t *testing.T) {
	withAutoBlendWeights(t, 1, 1)
	cache.InitForTest()

	config := vtc.DefaultVTCConfig()
	tracker := vtc.NewInMemorySlidingWindowTokenTracker(&config)
	vtcRouter, err := vtc.NewBasicVTCRouter(tracker, vtc.NewSimpleTokenEstimator(), &config)
	require.NoError(t, err)

	rm := NewRouterManager()
	rm.RegisterProvider(vtc.RouterVTCBasic, func(_ *types.RoutingContext) (types.Router, error) {
		return vtcRouter, nil
	})
	registerBlendScorers(rm)

	pods := &utils.PodArray{Pods: []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"},
			Status:     v1.PodStatus{PodIP: "10.0.0.1", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"},
			Status:     v1.PodStatus{PodIP: "10.0.0.2", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}},
		},
	}}

	for i := 0; i < 3; i++ {
		ctx := types.NewRoutingContext(context.Background(), vtc.RouterVTCBasic, testModelName,
			"please summarize this document", "req-vtc-blend", "bob")
		router, err := rm.Select(ctx)
		require.NoError(t, err)
		_, isMulti := router.(*multiStrategyRouter)
		require.True(t, isMulti, "a bare vtc-basic is auto-blended")
		_, err = router.Route(ctx, pods)
		require.NoError(t, err)
		ctx.Delete()
	}

	tokens, err := tracker.GetTokenCount(context.Background(), "bob")
	require.NoError(t, err)
	assert.Greater(t, tokens, 0.0, "every blended route charges the user's tokens")

	ctx := types.NewRoutingContext(context.Background(), vtc.RouterVTCBasic, testModelName,
		"please summarize this document", "req-vtc-direct", "alice")
	defer ctx.Delete()
	_, err = vtcRouter.Route(ctx, pods)
	require.NoError(t, err)
	perRequest, err := tracker.GetTokenCount(context.Background(), "alice")
	require.NoError(t, err)
	assert.InDelta(t, 3*perRequest, tokens, 1e-9, "the blend charges once per request, the same as a direct route")
}
