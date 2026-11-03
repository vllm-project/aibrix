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

package scheduling

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

// A LoRA adapter goes to the pod with the lowest recent queue + inference time, not the
// lowest lifetime mean (#3297).
func TestLeastLatencySchedulerScoresRecentMean(t *testing.T) {
	newPod := func(name, ip string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"model.aibrix.ai/name": "m1"}},
			Status: v1.PodStatus{
				Phase:      v1.PodRunning,
				PodIP:      ip,
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	podMetrics := func(queueMean, inferenceMean float64) map[string]metrics.MetricValue {
		return map[string]metrics.MetricValue{
			metrics.RequestQueueTimeSeconds:     &metrics.HistogramMetricValue{Sum: queueMean, Count: 1},
			metrics.RequestInferenceTimeSeconds: &metrics.HistogramMetricValue{Sum: inferenceMean, Count: 1},
		}
	}
	selected := func(p1Recent map[string]float64) string {
		pods := []*v1.Pod{newPod("p1", "1.1.1.1"), newPod("p2", "2.2.2.2")}
		p1 := podMetrics(0.1, 1) // p1 is faster than p2 over its lifetime
		for histogram, mean := range p1Recent {
			p1[cache.RecentMeanMetricName(histogram)] = &metrics.SimpleMetricValue{Value: mean}
		}
		c := cache.NewWithPodsModelMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
			"p1": p1,
			"p2": podMetrics(0.2, 2),
		})
		pod, err := NewLeastLatencyScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{*pods[0], *pods[1]})
		require.NoError(t, err)
		return pod.Name
	}

	require.Equal(t, "p1", selected(nil), "lifetime means decide without a recent mean")
	require.Equal(t, "p2", selected(map[string]float64{metrics.RequestQueueTimeSeconds: 50}), "p1's queue time grew")
	require.Equal(t, "p2", selected(map[string]float64{metrics.RequestInferenceTimeSeconds: 50}), "p1's inference time grew")
}
