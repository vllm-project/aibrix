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

package least

import (
	"context"

	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

func TestLeastLatency(t *testing.T) {
	tests := []struct {
		name            string
		readyPods       []*v1.Pod
		podModelMetrics map[string]map[string]metrics.MetricValue
		expectErr       bool
		expectMsgs      []string
	}{
		{
			name: "successful routing with least latency",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p3", "3.3.3.3", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p4", "4.4.4.4", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podModelMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 10, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 10, Count: 1},
				},
				"p2": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 20},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 200},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 21, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 21, Count: 1},
				},
				"p3": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 30},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 300},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 31, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 31, Count: 1},
				},
				"p4": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 40},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 400},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 41, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 41, Count: 1},
				},
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
			name: "multiple pods with same latency",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p3", "3.3.3.3", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podModelMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 10, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 10, Count: 1},
				},
				"p2": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 20},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 200},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 21, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 21, Count: 1},
				},
				"p3": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 30},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 300},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 30, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 30, Count: 1},
				},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000", "3.3.3.3:8000"},
		},
		{
			name: "use guessed prompt tokens",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podModelMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 0},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 10, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 10, Count: 1},
				},
				"p2": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 0},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 20, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 10, Count: 1},
				},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name: "use guesses generation tokens",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podModelMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 0},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 10, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 10, Count: 1},
				},
				"p2": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 0},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 10, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 20, Count: 1},
				},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name: "queue time outweighs faster prefill and decode",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podModelMetrics: map[string]map[string]metrics.MetricValue{
				// p1 computes faster but its requests wait 30s on average in the queue.
				"p1": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 300, Count: 10},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 1, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 1, Count: 1},
				},
				"p2": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 0.1, Count: 10},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 2, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 2, Count: 1},
				},
			},
			expectErr:  false,
			expectMsgs: []string{"2.2.2.2:8000"},
		},
		{
			name: "one pod has no metrics",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podModelMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {
					metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
					metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
					metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
					metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 10, Count: 1},
					metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 10, Count: 1},
				},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name: "all pods have no metrics",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podModelMetrics: map[string]map[string]metrics.MetricValue{},
			expectErr:       false,
			expectMsgs:      []string{"1.1.1.1:8000", "2.2.2.2:8000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cache.NewWithPodsModelMetricsForTest(
				tt.readyPods,
				"m1",
				tt.podModelMetrics)
			podList := podsFromCache(c)

			leastExpectedLatencyRouter := leastExpectedLatencyRouter{
				cache: c,
			}

			ctx := types.NewRoutingContext(context.Background(), "test", "m1", "message", "request", "user")
			targetPod, err := leastExpectedLatencyRouter.Route(ctx, podList)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Contains(t, tt.expectMsgs, targetPod)
			}
		})
	}
}

func TestLeastLatencyScoreAllHandlesNonHistogramMetrics(t *testing.T) {
	readyPods := []*v1.Pod{
		newPod("p1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
		newPod("p2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
		newPod("p3", "3.3.3.3", true, map[string]string{"model.aibrix.ai/port": "8000"}),
		newPod("p4", "4.4.4.4", true, map[string]string{"model.aibrix.ai/port": "8000"}),
	}
	c := cache.NewWithPodsModelMetricsForTest(readyPods, "m1", map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
			metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
			metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
			metrics.RequestPrefillTimeSeconds: &metrics.SimpleMetricValue{Value: 10},
			metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 10, Count: 1},
		},
		"p2": {
			metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
			metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
			metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
			metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 10, Count: 1},
			metrics.RequestDecodeTimeSeconds:  &metrics.SimpleMetricValue{Value: 10},
		},
		"p3": {
			metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 5, Count: 1},
			metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
			metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
			metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 10, Count: 1},
			metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 10, Count: 1},
		},
		"p4": {
			metrics.RequestQueueTimeSeconds:   &metrics.SimpleMetricValue{Value: 5},
			metrics.AvgPromptToksPerReq:       &metrics.SimpleMetricValue{Value: 10},
			metrics.AvgGenerationToksPerReq:   &metrics.SimpleMetricValue{Value: 100},
			metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 10, Count: 1},
			metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 10, Count: 1},
		},
	})

	router := leastExpectedLatencyRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), "test", "m1", "message", "request", "user")
	scores, scored, err := router.ScoreAll(ctx, newMockPodList(readyPods, nil))

	assert.NoError(t, err)
	assert.Len(t, scores, 4)
	assert.Len(t, scored, 4)
	assert.False(t, scored[0])
	assert.False(t, scored[1])
	assert.True(t, scored[2])
	assert.Greater(t, scores[2], 0.0)
	assert.False(t, scored[3], "a queue time that is not a histogram leaves the pod unscored")
}

func TestLeastLatencyReadsPromQLTokenAverages(t *testing.T) {
	promqlAverage := func(v float64) *metrics.PrometheusMetricValue {
		var result model.Value = model.Vector{&model.Sample{Value: model.SampleValue(v)}}
		return &metrics.PrometheusMetricValue{Result: &result}
	}
	pods := []*v1.Pod{
		newPod("p1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
		newPod("p2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
		newPod("p3", "3.3.3.3", true, map[string]string{"model.aibrix.ai/port": "8000"}),
	}
	// p1 takes longer per request but serves requests ten times as long, so per token it is the
	// faster pod: prefill 1s/1000 tokens against 0.5s/100, decode 2s/1000 tokens against 0.5s/100.
	// p3 has served nothing, so its averages are 0 and stay out of the pool averages, which are
	// 550 prompt and 550 generation tokens. Scaled to them, p1's prefill and decode estimates are
	// 0.55s and 1.1s, and p2's are 2.75s each. Without the token averages every pod falls back to
	// the same guess and the raw means decide, which favours p2.
	c := cache.NewWithPodsModelMetricsForTest(pods, testModelName, map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 1, Count: 1},
			metrics.AvgPromptToksPerReq:       promqlAverage(1000),
			metrics.AvgGenerationToksPerReq:   promqlAverage(1000),
			metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 1, Count: 1},
			metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 2, Count: 1},
		},
		"p2": {
			metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 1, Count: 1},
			metrics.AvgPromptToksPerReq:       promqlAverage(100),
			metrics.AvgGenerationToksPerReq:   promqlAverage(100),
			metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 0.5, Count: 1},
			metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 0.5, Count: 1},
		},
		"p3": {
			metrics.RequestQueueTimeSeconds:   &metrics.HistogramMetricValue{Sum: 1, Count: 1},
			metrics.AvgPromptToksPerReq:       promqlAverage(0),
			metrics.AvgGenerationToksPerReq:   promqlAverage(0),
			metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 3, Count: 1},
			metrics.RequestDecodeTimeSeconds:  &metrics.HistogramMetricValue{Sum: 3, Count: 1},
		},
	})
	podList := podsFromCache(c)
	r := leastExpectedLatencyRouter{cache: c}

	ctx := types.NewRoutingContext(context.Background(), "least-latency", testModelName, "message", "request", "user")
	scores, scored, err := r.ScoreAll(ctx, podList)
	assert.NoError(t, err)
	got := map[string]float64{}
	for i, pod := range podList.All() {
		if scored[i] {
			got[pod.Name] = scores[i]
		}
	}
	assert.InDelta(t, 1+0.55+1.1, got["p1"], 1e-9)
	assert.InDelta(t, 1+2.75+2.75, got["p2"], 1e-9)
	assert.InDelta(t, 1+3+3, got["p3"], 1e-9, "a pod without averages uses the pool's")

	target, err := r.Route(ctx, podList)
	assert.NoError(t, err)
	assert.Equal(t, "1.1.1.1:8000", target)
}
