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
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	metrics "github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/tokenizer"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_PrefixCacheE2E(t *testing.T) {
	readyPods := getReadyPods()
	c := cache.NewTestCacheWithPodsMetrics(
		readyPods,
		"m1",
		map[string]map[string]metrics.MetricValue{
			"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
			"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
			"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
			"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
		})
	podList := podsFromCache(c)

	prefixCacheRouter := prefixCacheRouter{
		cache:              c,
		tokenizer:          tokenizer.NewCharacterTokenizer(),
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
	}

	os.Setenv("AIBRIX_PREFIX_CACHE_BLOCK_SIZE", "4")
	// no prefix match -> select least request pod
	// input: abcdegfh
	// pre_request_count: [p1: 0, p2: 0, p3: 0, p4: 0]
	// post_request_count: [p1: 0, p2: 0, p3: 0, p4: 1(abcdefgh)]
	ctx1 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", "abcdefgh", "r1")
	p4, err := prefixCacheRouter.Route(ctx1, podList)
	assert.NoError(t, err)

	c.AddRequestCount(ctx1, ctx1.RequestID, ctx1.Model)
	fmt.Println(p4)

	// no prefix match -> select least request pod
	// input: wxyz
	// pre_request_count: [p1: 0, p2: 0, p3: 0, p4: 1(abcdefgh)]
	// post_request_count: [p1: 0, p2: 0, p3: 1 (wxyz), p4: 1(abcdefgh)]
	ctx2 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", "wxyz", "r2")
	p3, err := prefixCacheRouter.Route(ctx2, podList)
	assert.NoError(t, err)
	assert.NotEqual(t, p4, p3)

	c.AddRequestCount(ctx2, ctx2.RequestID, ctx2.Model)
	fmt.Println(p3)

	// prefix match, load balanced -> select cached pod
	// input: abcdefgh
	// pre_request_count: [p1: 0, p2: 0, p3: 1 (wxyz), p4: 1(abcdefgh)]
	// post_request_count: [p1: 0, p2: 0, p3: 1 (wxyz), p4: 2(abcdefgh)]
	ctx3 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", "abcdefgh", "r3")
	targetPod, err := prefixCacheRouter.Route(ctx3, podList)
	assert.NoError(t, err)
	assert.Equal(t, p4, targetPod)

	c.AddRequestCount(ctx3, ctx3.RequestID, ctx3.Model)
	fmt.Println(targetPod)

	// prefix match, load imbalanced -> select least request pod
	// input: abcd
	// pre_request_count: [p1: 0, p2: 0, p3: 1 (wxyz), p4: 2(abcdefgh)]
	// post_request_count: [p1: 0, p2: 1 (abcd), p3: 1 (wxyz), p4: 2(abcdefgh)]
	ctx4 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", "abcd", "r4")
	p2, err := prefixCacheRouter.Route(ctx4, podList)
	assert.NoError(t, err)
	assert.NotEqual(t, p4, p2)

	c.AddRequestCount(ctx4, ctx4.RequestID, ctx4.Model)
	fmt.Println(p2)

	// prefix match, load imbalanced -> selects p2 with lower prefix match
	// input: abcdefghijkl
	// pre_request_count: [p1: 0, p2: 1 (abcd), p3: 1 (wxyz), p4: 2 (abcdefgh)]
	// post_request_count: [p1: 0, p2: 2 (abcdefghijkl), p3: 1 (wxyz), p4: 2(abcdefgh)]
	ctx5 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", "abcdefghijkl", "r5")
	targetPod, err = prefixCacheRouter.Route(ctx5, podList)
	assert.NoError(t, err)
	assert.Equal(t, p2, targetPod)

	c.AddRequestCount(ctx5, ctx5.RequestID, ctx5.Model)
	fmt.Println(targetPod)

	// prefix match, load balanced -> selects p2 or p3
	// input: abcdefgh
	// pre_request_count: [p1: 0, p2: 2 (abcdefghijkl), p3: 1 (wxyz), p4: 2(abcdefgh)]
	// post_request_count: [p1: 0, p2: 3 (abcdefghijkl), p3: 1 (wxyz), p4: 2(abcdefgh)]
	ctx6 := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", "abcdefgh", "r6")
	targetPod, err = prefixCacheRouter.Route(ctx6, podList)
	assert.NoError(t, err)
	assert.True(t, slices.Contains([]string{p2, p4}, targetPod))
	c.AddRequestCount(ctx6, ctx6.RequestID, ctx6.Model)
	fmt.Println(targetPod)
}

func getReadyPods() []*v1.Pod {
	return []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p1"},
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
			ObjectMeta: metav1.ObjectMeta{Name: "p2"},
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
			ObjectMeta: metav1.ObjectMeta{Name: "p3"},
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
			ObjectMeta: metav1.ObjectMeta{Name: "p4"},
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
