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

package vtc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The gateway shuffles candidates before routing, so the fairness term must
// map a user's tokens onto a pod by a stable order, not by list position.
func TestVTCBasic_PodChoiceIgnoresCandidateOrder(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config)
	router := &BasicVTCRouter{
		cache:          NewSimpleCache(),
		tokenTracker:   tracker,
		tokenEstimator: NewSimpleTokenEstimator(),
		config:         &config,
	}
	// Enough tokens to map the user onto the second pod by name.
	require.NoError(t, tracker.UpdateTokenCount(context.Background(), "heavy", 5000, 0))

	pods := createTestPodsForMetrics(2)
	orders := [][]*v1.Pod{{pods[0], pods[1]}, {pods[1], pods[0]}}
	for i := 0; i < 20; i++ {
		order := orders[i%2]

		scoreCtx := types.NewRoutingContext(context.Background(), RouterVTCBasic, "model1", "", "req-score", "heavy")
		scores, scored, err := router.ScoreAll(scoreCtx, NewSimplePodList(order))
		scoreCtx.Delete()
		require.NoError(t, err)
		require.Equal(t, []bool{true, true}, scored)
		best := order[0]
		if scores[1] < scores[0] {
			best = order[1]
		}
		assert.Equal(t, pods[1].Name, best.Name, "ScoreAll prefers the same pod in either order")

		routeCtx := types.NewRoutingContext(context.Background(), RouterVTCBasic, "model1", "", "req-route", "heavy")
		_, err = router.Route(routeCtx, NewSimplePodList(order))
		require.NoError(t, err)
		assert.Equal(t, pods[1].Name, routeCtx.TargetPod().Name, "Route picks the same pod in either order")
		routeCtx.Delete()
	}
}

func TestStablePodRanksOrdersByNamespaceThenName(t *testing.T) {
	pod := func(ns, name string) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	b, a, other := pod("default", "b"), pod("default", "a"), pod("alpha", "z")
	ranks := stablePodRanks([]*v1.Pod{b, a, other})
	assert.Equal(t, map[*v1.Pod]int{other: 0, a: 1, b: 2}, ranks)
}
