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
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

func newPod(name, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Status: v1.PodStatus{
			Phase:      v1.PodRunning,
			PodIP:      ip,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

func podMetrics(queueMean, inferenceMean float64) map[string]metrics.MetricValue {
	return map[string]metrics.MetricValue{
		metrics.RequestQueueTimeSeconds:     &metrics.HistogramMetricValue{Sum: queueMean, Count: 1},
		metrics.RequestInferenceTimeSeconds: &metrics.HistogramMetricValue{Sum: inferenceMean, Count: 1},
	}
}

// A LoRA adapter goes to the pod with the lowest recent queue + inference time, not the
// lowest lifetime mean (#3297).
func TestLeastLatencySchedulerScoresRecentMean(t *testing.T) {
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

func TestLeastLatencySchedulerSkipsUnscoredPods(t *testing.T) {
	queueHistogram := &metrics.HistogramMetricValue{Sum: 0.1, Count: 1}
	inferenceHistogram := &metrics.HistogramMetricValue{Sum: 1, Count: 1}

	tests := []struct {
		name string
		p1   map[string]metrics.MetricValue
	}{
		{
			name: "missing both metrics",
			p1:   nil,
		},
		{
			name: "missing queue time",
			p1: map[string]metrics.MetricValue{
				metrics.RequestInferenceTimeSeconds: inferenceHistogram,
			},
		},
		{
			name: "missing inference time",
			p1: map[string]metrics.MetricValue{
				metrics.RequestQueueTimeSeconds: queueHistogram,
			},
		},
		{
			name: "queue time is not a histogram",
			p1: map[string]metrics.MetricValue{
				metrics.RequestQueueTimeSeconds:     &metrics.SimpleMetricValue{Value: 1},
				metrics.RequestInferenceTimeSeconds: inferenceHistogram,
			},
		},
		{
			name: "inference time is not a histogram",
			p1: map[string]metrics.MetricValue{
				metrics.RequestQueueTimeSeconds:     queueHistogram,
				metrics.RequestInferenceTimeSeconds: &metrics.SimpleMetricValue{Value: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pods := []*v1.Pod{newPod("p1", "1.1.1.1"), newPod("p2", "2.2.2.2")}
			podMetricsByPod := map[string]map[string]metrics.MetricValue{
				"p2": podMetrics(0.2, 2),
			}
			if tt.p1 != nil {
				podMetricsByPod["p1"] = tt.p1
			}
			c := cache.NewWithPodsModelMetricsForTest(pods, "m1", podMetricsByPod)

			pod, err := NewLeastLatencyScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{*pods[0], *pods[1]})
			require.NoError(t, err)
			require.Equal(t, "p2", pod.Name)
		})
	}
}

func TestLeastLatencySchedulerSkipsNonFiniteLatency(t *testing.T) {
	pods := []*v1.Pod{newPod("p1", "1.1.1.1"), newPod("p2", "2.2.2.2")}

	t.Run("NaN queue mean", func(t *testing.T) {
		c := cache.NewWithPodsModelMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
			"p1": podMetrics(math.NaN(), 1),
			"p2": podMetrics(0.2, 2),
		})
		pod, err := NewLeastLatencyScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{*pods[0], *pods[1]})
		require.NoError(t, err)
		require.Equal(t, "p2", pod.Name)
	})

	t.Run("infinite inference mean", func(t *testing.T) {
		c := cache.NewWithPodsModelMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
			"p1": podMetrics(0.1, math.Inf(1)),
			"p2": podMetrics(0.2, 2),
		})
		pod, err := NewLeastLatencyScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{*pods[0], *pods[1]})
		require.NoError(t, err)
		require.Equal(t, "p2", pod.Name)
	})

	t.Run("all non-finite reports no scored pod", func(t *testing.T) {
		c := cache.NewWithPodsModelMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
			"p1": podMetrics(math.NaN(), 1),
			"p2": podMetrics(math.NaN(), 2),
		})
		_, err := NewLeastLatencyScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{*pods[0], *pods[1]})
		require.Error(t, err)
		require.Contains(t, err.Error(), "no ready pod has latency metrics")
		require.NotContains(t, err.Error(), "no ready pods to schedule on")
	})
}

func TestLeastLatencySchedulerNoScoredPods(t *testing.T) {
	pods := []*v1.Pod{newPod("p1", "1.1.1.1"), newPod("p2", "2.2.2.2")}
	c := cache.NewWithPodsModelMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{})

	_, err := NewLeastLatencyScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{*pods[0], *pods[1]})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no ready pod has latency metrics")

	var metricNotFound *cache.MetricNotFoundError
	require.ErrorAs(t, err, &metricNotFound, "the last metric error must stay wrapped")
}

func TestLeastLatencySchedulerNoReadyPods(t *testing.T) {
	c := cache.NewWithPodsModelMetricsForTest(nil, "m1", map[string]map[string]metrics.MetricValue{})
	_, err := NewLeastLatencyScheduler(c).SelectPod(context.Background(), "m1", nil)
	require.Error(t, err)
	require.Equal(t, "no ready pods to schedule on", err.Error())
}
