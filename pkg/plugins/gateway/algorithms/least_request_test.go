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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLeastRequest(t *testing.T) {
	tests := []struct {
		name       string
		readyPods  []*v1.Pod
		podMetrics map[string]map[string]metrics.MetricValue
		expectErr  bool
		expectMsgs []string
	}{
		{
			name: "successful routing with least request",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}, Status: v1.PodStatus{PodIP: "1.1.1.1",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}, Status: v1.PodStatus{PodIP: "2.2.2.2",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p3"}, Status: v1.PodStatus{PodIP: "3.3.3.3",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p4"}, Status: v1.PodStatus{PodIP: "4.4.4.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
				"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
				"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 3}},
				"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 9}},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name:       "no ready pods",
			readyPods:  []*v1.Pod{},
			podMetrics: map[string]map[string]metrics.MetricValue{},
			expectErr:  true,
		},
		{
			name: "multiple pods with same least requests",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}, Status: v1.PodStatus{PodIP: "1.1.1.1",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}, Status: v1.PodStatus{PodIP: "2.2.2.2",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p3"}, Status: v1.PodStatus{PodIP: "3.3.3.3",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
				"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 5}},
				"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000", "3.3.3.3:8000"},
		},
		{
			name: "one pod has no metrics",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}, Status: v1.PodStatus{PodIP: "1.1.1.1",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}, Status: v1.PodStatus{PodIP: "2.2.2.2",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
			},
			expectErr:  false,
			expectMsgs: []string{"2.2.2.2:8000"},
		},
		{
			name: "all pods have no metrics",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}, Status: v1.PodStatus{PodIP: "1.1.1.1",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}, Status: v1.PodStatus{PodIP: "2.2.2.2",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			},
			podMetrics: map[string]map[string]metrics.MetricValue{},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000", "2.2.2.2:8000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cache.NewWithPodsMetricsForTest(
				tt.readyPods,
				"m1",
				tt.podMetrics)
			podList := podsFromCache(c)

			leastRequestRouter := leastRequestRouter{
				cache: c,
			}

			ctx := types.NewRoutingContext(context.Background(), "test", "m1", "message", "request", "user")
			targetPod, err := leastRequestRouter.Route(ctx, podList)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Contains(t, tt.expectMsgs, targetPod)
			}
		})
	}
}

// A pool that mixes a data-parallel (multi-port) pod with a plain single-port
// pod takes the per-port routing path. The single-port pod must be selectable
// when it is the least loaded candidate, in the same "pod/port" key format the
// multi-port pods use. pod-b's count is non-zero so the assertion proves the
// single-port branch reads the live counter rather than the cold-start default.
func TestLeastRequest_MixedPortPool_SelectsLeastLoadedSinglePortPod(t *testing.T) {
	model := testModelName
	podA := newPod("pod-a", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podA.Spec.Containers = []v1.Container{{Env: []v1.EnvVar{{Name: "data-parallel-size", Value: "2"}}}}
	podB := newPod("pod-b", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"})
	c := cache.NewWithPodsMetricsForTest(
		[]*v1.Pod{podA, podB},
		model,
		map[string]map[string]metrics.MetricValue{
			"pod-a": {
				metrics.RealtimeNumRequestsRunning:           &metrics.SimpleMetricValue{Value: 0},
				metrics.RealtimeNumRequestsRunning + "/8000": &metrics.SimpleMetricValue{Value: 5},
				metrics.RealtimeNumRequestsRunning + "/8001": &metrics.SimpleMetricValue{Value: 5},
			},
			"pod-b": {
				metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 3},
			},
		})
	portsMap := map[string][]int{
		"pod-a": {8000, 8001},
		"pod-b": {8000},
	}

	counts := getRequestCountsWithPort(c, []*v1.Pod{podA, podB}, portsMap)
	assert.Equal(t, map[string]int{"pod-a/8000": 5, "pod-a/8001": 5, "pod-b/8000": 3}, counts)

	r := &leastRequestRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLeastRequest, model, "hello", "req-mixed-port", "")
	address, err := r.Route(ctx, portWrapper{pods: []*v1.Pod{podA, podB}, ports: portsMap})

	assert.NoError(t, err)
	assert.Equal(t, "2.2.2.2:8000", address)
	assert.Equal(t, 8000, ctx.TargetPort())
}
