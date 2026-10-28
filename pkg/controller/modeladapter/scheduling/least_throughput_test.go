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

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

const throughputTestModel = "test-model"

func throughputPod(name string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

// promVector is how the PromQL worker stores a query result for a pod.
func promVector(value float64) metrics.MetricValue {
	var result model.Value = model.Vector{&model.Sample{Value: model.SampleValue(value)}}
	return &metrics.PrometheusMetricValue{Result: &result}
}

func throughputMetrics(prompt, generation float64) map[string]metrics.MetricValue {
	return map[string]metrics.MetricValue{
		metrics.AvgPromptThroughputToksPerMinPod:     promVector(prompt),
		metrics.AvgGenerationThroughputToksPerMinPod: promVector(generation),
	}
}

func TestLeastThroughputReadsPodScopedPromQLResults(t *testing.T) {
	pods := []*v1.Pod{throughputPod("pod-a"), throughputPod("pod-b"), throughputPod("pod-c")}
	c := cache.NewWithPodsMetricsForTest(pods, throughputTestModel, map[string]map[string]metrics.MetricValue{
		"pod-a": throughputMetrics(10, 20), // 2*10 + 20 = 40
		"pod-b": throughputMetrics(1, 2),   // 2*1 + 2 = 4
		"pod-c": throughputMetrics(5, 5),   // 2*5 + 5 = 15
	})

	readyPods := []v1.Pod{*pods[0], *pods[1], *pods[2]}
	selected, err := NewLeastThroughputScheduler(c).SelectPod(context.Background(), throughputTestModel, readyPods)
	require.NoError(t, err)
	assert.Equal(t, "pod-b", selected.Name, "the least 2*prompt+generation pod must win")
}

func TestLeastThroughputReadsScrapedValues(t *testing.T) {
	pods := []*v1.Pod{throughputPod("pod-a"), throughputPod("pod-b")}
	c := cache.NewWithPodsMetricsForTest(pods, throughputTestModel, map[string]map[string]metrics.MetricValue{
		"pod-a": {
			metrics.AvgPromptThroughputToksPerMinPod:     &metrics.SimpleMetricValue{Value: 30},
			metrics.AvgGenerationThroughputToksPerMinPod: &metrics.SimpleMetricValue{Value: 10},
		},
		"pod-b": {
			metrics.AvgPromptThroughputToksPerMinPod:     &metrics.SimpleMetricValue{Value: 1},
			metrics.AvgGenerationThroughputToksPerMinPod: &metrics.SimpleMetricValue{Value: 1},
		},
	})

	readyPods := []v1.Pod{*pods[0], *pods[1]}
	selected, err := NewLeastThroughputScheduler(c).SelectPod(context.Background(), throughputTestModel, readyPods)
	require.NoError(t, err)
	assert.Equal(t, "pod-b", selected.Name)
}

func TestLeastThroughputSkipsPodsWithoutThroughput(t *testing.T) {
	pods := []*v1.Pod{throughputPod("pod-a"), throughputPod("pod-b")}
	c := cache.NewWithPodsMetricsForTest(pods, throughputTestModel, map[string]map[string]metrics.MetricValue{
		"pod-a": throughputMetrics(10, 20),
	})

	readyPods := []v1.Pod{*pods[0], *pods[1]}
	selected, err := NewLeastThroughputScheduler(c).SelectPod(context.Background(), throughputTestModel, readyPods)
	require.NoError(t, err)
	assert.Equal(t, "pod-a", selected.Name, "a pod without throughput data must not be selected")
}

func TestLeastThroughputErrorsWithoutThroughput(t *testing.T) {
	pods := []*v1.Pod{throughputPod("pod-a")}
	c := cache.NewWithPodsMetricsForTest(pods, throughputTestModel, map[string]map[string]metrics.MetricValue{})

	readyPods := []v1.Pod{*pods[0]}
	selected, err := NewLeastThroughputScheduler(c).SelectPod(context.Background(), throughputTestModel, readyPods)
	require.Error(t, err)
	assert.Nil(t, selected)
}
