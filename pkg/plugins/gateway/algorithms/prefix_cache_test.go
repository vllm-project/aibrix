/*
Copyright 2024 The Aibrix Team.

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
	"log"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_PrefixCacheE2E(t *testing.T) {
	readyPods := getReadyPods()
	c := cache.NewWithPodsMetricsForTest(
		readyPods,
		"m1",
		map[string]map[string]metrics.MetricValue{
			"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
			"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
			"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
			"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
		})
	podList := podsFromCache(c)

	// Create tokenizer using factory method
	tokenizerObj, err := tokenizer.NewTokenizer("character", nil)
	assert.NoError(t, err)

	prefixCacheRouter := prefixCacheRouter{
		cache:              c,
		tokenizer:          tokenizerObj,
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
		// No KV sync router, uses original implementation
	}

	// no prefix match -> select least request pod
	input := "abcdegfh"
	// pre_request_count: [p1: 0, p2: 0, p3: 0, p4: 0]
	// post_request_count: [p1: 0, p2: 0, p3: 0, p4: 1(abcdefgh)]
	t.Log(input)
	ctx1 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", input, "r1", "")
	p4, err := prefixCacheRouter.Route(ctx1, podList)
	assert.NoError(t, err)

	c.AddRequestCount(ctx1, ctx1.RequestID, ctx1.Model)
	t.Log(p4)

	// no prefix match -> select least request pod
	input = "wxyz"
	// pre_request_count: [p1: 0, p2: 0, p3: 0, p4: 1(abcdefgh)]
	// post_request_count: [p1: 0, p2: 0, p3: 1 (wxyz), p4: 1(abcdefgh)]
	t.Log(input)
	ctx2 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", input, "r2", "")
	p3, err := prefixCacheRouter.Route(ctx2, podList)
	assert.NoError(t, err)
	assert.NotEqual(t, p4, p3)

	c.AddRequestCount(ctx2, ctx2.RequestID, ctx2.Model)
	t.Log(p3)

	// prefix match, load balanced -> select cached pod
	input = "abcdegfh"
	// pre_request_count: [p1: 0, p2: 0, p3: 1 (wxyz), p4: 1(abcdefgh)]
	// post_request_count: [p1: 0, p2: 0, p3: 1 (wxyz), p4: 2(abcdefgh)]
	t.Log(input)
	ctx3 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", input, "r3", "")
	targetPod, err := prefixCacheRouter.Route(ctx3, podList)
	assert.NoError(t, err)
	assert.Equal(t, p4, targetPod)

	c.AddRequestCount(ctx3, ctx3.RequestID, ctx3.Model)
	t.Log(targetPod)

	// prefix match, load imbalanced -> select least request pod
	input = "abcd"
	// pre_request_count: [p1: 0, p2: 0, p3: 1 (wxyz), p4: 2(abcdefgh)]
	// post_request_count: [p1: 0, p2: 1 (abcd), p3: 1 (wxyz), p4: 2(abcdefgh)]
	t.Log(input)
	ctx4 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", input, "r4", "")
	p2, err := prefixCacheRouter.Route(ctx4, podList)
	assert.NoError(t, err)
	assert.NotEqual(t, p4, p2)

	c.AddRequestCount(ctx4, ctx4.RequestID, ctx4.Model)
	t.Log(p2)

	// prefix match, load imbalanced -> selects p2 with lower prefix match
	input = "abcdefghijkl"
	// pre_request_count: [p1: 0, p2: 1 (abcd), p3: 1 (wxyz), p4: 2 (abcdefgh)]
	// post_request_count: [p1: 0, p2: 2 (abcdefghijkl), p3: 1 (wxyz), p4: 2(abcdefgh)]
	t.Log(input)
	ctx5 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", input, "r5", "")
	targetPod, err = prefixCacheRouter.Route(ctx5, podList)
	assert.NoError(t, err)
	assert.Equal(t, p2, targetPod)

	c.AddRequestCount(ctx5, ctx5.RequestID, ctx5.Model)
	t.Log(targetPod)

	// prefix match, load balanced -> selects p2 or p3
	input = "abcdefgh"
	// pre_request_count: [p1: 0, p2: 2 (abcdefghijkl), p3: 1 (wxyz), p4: 2(abcdefgh)]
	// post_request_count: [p1: 0, p2: 3 (abcdefghijkl), p3: 1 (wxyz), p4: 2(abcdefgh)]
	t.Log(input)
	ctx6 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", input, "r6", "")
	targetPod, err = prefixCacheRouter.Route(ctx6, podList)
	assert.NoError(t, err)
	assert.True(t, slices.Contains([]string{p2, p4}, targetPod))
	c.AddRequestCount(ctx6, ctx6.RequestID, ctx6.Model)
	t.Log(targetPod)

	// pre prefix match, load imbalance -> select least request pod
	input = "abcdefgh"
	// pre_request_count: [p1: 0, p2: 9 (abcdefghijkl), p3: 1 (wxyz), p4: 2(abcdefgh)]
	// post_request_count: [p1: 1 (abcdefgh), p2: 9 (abcdefghijkl), p3: 1 (wxyz), p4: 2(abcdefgh)]
	t.Log(input)
	for i := 0; i < 6; i++ {
		ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", input, "r7-12", "")
		ctx.SetTargetPod(ctx6.TargetPod())
		c.AddRequestCount(ctx, ctx.RequestID, ctx.Model)
	}
	ctx7 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", input, "r7", "")
	p1, err := prefixCacheRouter.Route(ctx7, podList)
	log.Println(p2, p3, p4)
	log.Println(p1)
	assert.NoError(t, err)
	assert.False(t, slices.Contains([]string{p2, p3, p4}, p1))
}

func getReadyPods() []*v1.Pod {
	return []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p1", Labels: map[string]string{
				utils.DeploymentIdentifier: "deployment",
			}},
			Status: v1.PodStatus{
				PodIP: "1.1.1.1",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p2", Labels: map[string]string{
				utils.DeploymentIdentifier: "deployment",
			}},
			Status: v1.PodStatus{
				PodIP: "2.2.2.2",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p3", Labels: map[string]string{
				utils.DeploymentIdentifier: "deployment",
			}},
			Status: v1.PodStatus{
				PodIP: "3.3.3.3",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p4", Labels: map[string]string{
				utils.DeploymentIdentifier: "deployment",
			}},
			Status: v1.PodStatus{
				PodIP: "4.4.4.4",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
	}
}

func Test_ValidatePrePrefixMatchLoadBalance(t *testing.T) {
	// no imbalance
	readyPods := getReadyPods()
	c := cache.NewWithPodsMetricsForTest(
		readyPods,
		"m1",
		map[string]map[string]metrics.MetricValue{
			"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
			"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
			"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 3}},
			"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 9}},
		})
	targetPod, imbalance := getTargetPodOnLoadImbalance(c, readyPods)
	assert.False(t, imbalance, "pod running request count is less than equal to default abs value of 8")
	assert.Nil(t, targetPod)

	// imbalance with multiple pods matching criteria
	c = cache.NewWithPodsMetricsForTest(
		readyPods,
		"m1",
		map[string]map[string]metrics.MetricValue{
			"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
			"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
			"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 8}},
			"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 16}},
		})
	targetPod, imbalance = getTargetPodOnLoadImbalance(c, readyPods)
	assert.True(t, imbalance, "pod running request count is more than default abs value of 8")
	assert.True(t, slices.Contains([]string{"p1", "p2"}, targetPod.Name))
}

func Test_ValidatePostPrefixMatchLoadBalance(t *testing.T) {
	readyPods := getReadyPods()
	testcases := []struct {
		name        string
		c           cache.Cache
		readyPods   []*v1.Pod
		matchedPods map[string]int
		targetPods  []string
	}{
		{
			name: "match pod with highest prefix match percent",
			c: cache.NewWithPodsMetricsForTest(
				readyPods,
				"m1",
				map[string]map[string]metrics.MetricValue{
					"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
					"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
					"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 3}},
					"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 4}},
				}),
			matchedPods: map[string]int{
				"p1": 50,
				"p2": 60,
			},
			targetPods: []string{"p2"},
		},
		{
			name: "match pod with lowest running request count for same prefix match percent",
			c: cache.NewWithPodsMetricsForTest(
				readyPods,
				"m1",
				map[string]map[string]metrics.MetricValue{
					"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
					"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
					"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 3}},
					"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 4}},
				}),
			matchedPods: map[string]int{
				"p1": 50,
				"p2": 50,
				"p3": 50,
			},
			targetPods: []string{"p1"},
		},
		{
			name: "match any pod with same running request count and same prefix match percent",
			c: cache.NewWithPodsMetricsForTest(
				readyPods,
				"m1",
				map[string]map[string]metrics.MetricValue{
					"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
					"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
					"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
					"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 4}},
				}),
			matchedPods: map[string]int{
				"p1": 50,
				"p2": 50,
				"p3": 50,
			},
			targetPods: []string{"p1", "p2", "p3"},
		},
		{
			name: "match pod with lower prefix match percent with running requests below imbalance threshold",
			c: cache.NewWithPodsMetricsForTest(
				readyPods,
				"m1",
				map[string]map[string]metrics.MetricValue{
					"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
					"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 10}},
					"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
					"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 4}},
				}),
			matchedPods: map[string]int{
				"p1": 50,
				"p2": 100,
			},
			targetPods: []string{"p1"},
		},
		{
			name: "ignore matched pod if their running requests are more than threshold",
			c: cache.NewWithPodsMetricsForTest(
				readyPods,
				"m1",
				map[string]map[string]metrics.MetricValue{
					"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 4}},
					"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
					"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
				}),
			matchedPods: map[string]int{
				"p1": 100,
			},
			targetPods: []string{},
		},
	}
	for _, test := range testcases {
		targetPod := getTargetPodFromMatchedPods(test.c, readyPods, test.matchedPods)
		if len(test.targetPods) == 0 {
			assert.Nil(t, targetPod, test.name)
		} else {
			assert.True(t, slices.Contains(test.targetPods, targetPod.Name), test.name)
		}
	}
}
