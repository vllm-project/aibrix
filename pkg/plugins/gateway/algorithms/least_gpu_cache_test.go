/*
Copyright 2025 The Aibrix Team.

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

func TestLeastGpuCache(t *testing.T) {
	tests := []struct {
		name            string
		readyPods       []*v1.Pod
		podModelMetrics map[string]map[string]metrics.MetricValue
		expectErr       bool
		expectMsgs      []string
	}{
		{
			name: "successful routing with least gpu cache",
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
			podModelMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.GPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.1}},
				"p2": {metrics.GPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.2}},
				"p3": {metrics.GPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.3}},
				"p4": {metrics.GPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.4}},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name:            "no ready pods",
			readyPods:       []*v1.Pod{},
			podModelMetrics: map[string]map[string]metrics.MetricValue{},
			expectErr:       true,
		},
		{
			name: "multiple pods with same gpu cache",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}, Status: v1.PodStatus{PodIP: "1.1.1.1",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}, Status: v1.PodStatus{PodIP: "2.2.2.2",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p3"}, Status: v1.PodStatus{PodIP: "3.3.3.3",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			},
			podModelMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.GPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.1}},
				"p2": {metrics.GPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.5}},
				"p3": {metrics.GPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.1}},
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
			podModelMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.GPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.1}},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name: "all pods have no metrics",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}, Status: v1.PodStatus{PodIP: "1.1.1.1",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}, Status: v1.PodStatus{PodIP: "2.2.2.2",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			},
			podModelMetrics: map[string]map[string]metrics.MetricValue{},
			expectErr:       false,
			expectMsgs:      []string{"1.1.1.1:8000", "2.2.2.2:8000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cache.NewWithPodModelMetricsForTest(
				tt.readyPods,
				"m1",
				tt.podModelMetrics)
			podList := podsFromCache(c)

			leastGpuCacheRouter := leastGpuCacheRouter{
				cache: c,
			}

			ctx := types.NewRoutingContext(context.Background(), "test", "m1", "message", "request", "user")
			targetPod, err := leastGpuCacheRouter.Route(ctx, podList)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Contains(t, tt.expectMsgs, targetPod)
			}
		})
	}
}
