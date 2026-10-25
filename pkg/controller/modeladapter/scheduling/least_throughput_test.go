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

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// promqlThroughput is how the cache stores a pod-scoped PromQL metric such as
// avg_prompt_throughput_toks_per_min_pod: a query result under the pod, not under a model.
func promqlThroughput(v float64) metrics.MetricValue {
	var result model.Value = model.Vector{&model.Sample{Value: model.SampleValue(v)}}
	return &metrics.PrometheusMetricValue{Result: &result}
}

func throughputPods(names ...string) ([]*v1.Pod, []v1.Pod) {
	ptrs := make([]*v1.Pod, 0, len(names))
	ready := make([]v1.Pod, 0, len(names))
	for _, name := range names {
		pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		ptrs = append(ptrs, pod)
		ready = append(ready, *pod)
	}
	return ptrs, ready
}

func TestLeastThroughputSchedulerSelectPod(t *testing.T) {
	tests := []struct {
		name       string
		podMetrics map[string]map[string]metrics.MetricValue
		want       string
		wantErr    bool
	}{
		{
			name: "picks the pod with the least weighted PromQL throughput",
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.AvgPromptThroughputToksPerMinPod: promqlThroughput(100), metrics.AvgGenerationThroughputToksPerMinPod: promqlThroughput(50)},
				"p2": {metrics.AvgPromptThroughputToksPerMinPod: promqlThroughput(10), metrics.AvgGenerationThroughputToksPerMinPod: promqlThroughput(5)},
				"p3": {metrics.AvgPromptThroughputToksPerMinPod: promqlThroughput(50), metrics.AvgGenerationThroughputToksPerMinPod: promqlThroughput(50)},
			},
			want: "p2",
		},
		{
			name: "prompt tokens weigh twice generation tokens",
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.AvgPromptThroughputToksPerMinPod: promqlThroughput(10), metrics.AvgGenerationThroughputToksPerMinPod: promqlThroughput(0)},
				"p2": {metrics.AvgPromptThroughputToksPerMinPod: promqlThroughput(0), metrics.AvgGenerationThroughputToksPerMinPod: promqlThroughput(15)},
				"p3": {metrics.AvgPromptThroughputToksPerMinPod: promqlThroughput(8), metrics.AvgGenerationThroughputToksPerMinPod: promqlThroughput(8)},
			},
			want: "p2",
		},
		{
			name: "a pod without a usable throughput is passed over",
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.AvgPromptThroughputToksPerMinPod: promqlThroughput(math.NaN()), metrics.AvgGenerationThroughputToksPerMinPod: promqlThroughput(0)},
				"p2": {metrics.AvgPromptThroughputToksPerMinPod: promqlThroughput(0)},
				"p3": {metrics.AvgPromptThroughputToksPerMinPod: promqlThroughput(40), metrics.AvgGenerationThroughputToksPerMinPod: promqlThroughput(40)},
			},
			want: "p3",
		},
		{
			name:       "no pod with throughput metrics is an error",
			podMetrics: map[string]map[string]metrics.MetricValue{},
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ptrs, ready := throughputPods("p1", "p2", "p3")
			// The cache keeps pod-scoped metrics under the pod only, which is where the PromQL
			// worker stores these two.
			c := cache.InitWithPodsMetrics(cache.NewWithPodsForTest(ptrs, "base-model"), tt.podMetrics)

			pod, err := NewLeastThroughputScheduler(c).SelectPod(context.Background(), "adapter", ready)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, pod.Name)
		})
	}
}
