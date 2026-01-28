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
package cache

import (
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCleanupOldSnapshots(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	history := []MetricSnapshot{
		{Value: 1, Timestamp: now.Add(-10 * time.Minute)},
		{Value: 2, Timestamp: now.Add(-4 * time.Minute)},
		{Value: 3, Timestamp: now.Add(-3 * time.Minute)},
		{Value: 4, Timestamp: now.Add(-2 * time.Minute)},
	}

	filtered := cleanupOldSnapshots(history, now, 5*time.Minute, 2)
	require.Len(t, filtered, 2)
	require.Equal(t, float64(3), filtered[0].Value)
	require.Equal(t, float64(4), filtered[1].Value)
}

func TestEmitCounterValue_TotalUsesDelta(t *testing.T) {
	lastCounterValues = sync.Map{}

	var counterAdds []float64
	originalCounterFn := metrics.IncrementCounterMetricFnForTest
	originalGaugeFn := metrics.SetGaugeMetricFnForTest
	defer func() {
		metrics.IncrementCounterMetricFnForTest = originalCounterFn
		metrics.SetGaugeMetricFnForTest = originalGaugeFn
	}()

	metrics.IncrementCounterMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		counterAdds = append(counterAdds, value)
	}
	metrics.SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		t.Fatalf("unexpected gauge metric call: %s=%v", name, value)
	}

	c := &Store{}
	c.emitCounterValue("http_requests_total", 10, []string{"pod"}, "p1")
	require.Empty(t, counterAdds)

	c.emitCounterValue("http_requests_total", 15, []string{"pod"}, "p1")
	require.Equal(t, []float64{5}, counterAdds)

	c.emitCounterValue("http_requests_total", 3, []string{"pod"}, "p1")
	require.Equal(t, []float64{5}, counterAdds)

	c.emitCounterValue("http_requests_total", 20, []string{"pod"}, "p1")
	require.Equal(t, []float64{5, 17}, counterAdds)
}

func TestEmitCounterValue_NonTotalUsesGauge(t *testing.T) {
	lastCounterValues = sync.Map{}

	var gaugeSets []float64
	originalCounterFn := metrics.IncrementCounterMetricFnForTest
	originalGaugeFn := metrics.SetGaugeMetricFnForTest
	defer func() {
		metrics.IncrementCounterMetricFnForTest = originalCounterFn
		metrics.SetGaugeMetricFnForTest = originalGaugeFn
	}()

	metrics.IncrementCounterMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		t.Fatalf("unexpected counter metric call: %s add=%v", name, value)
	}
	metrics.SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		gaugeSets = append(gaugeSets, value)
	}

	c := &Store{}
	c.emitCounterValue("num_requests_running", 7, []string{"pod"}, "p1")
	require.Equal(t, []float64{7}, gaugeSets)
}

func TestUpdatePodRecord(t *testing.T) {
	c := &Store{}
	pod := &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p1",
				Namespace: "default",
				Labels: map[string]string{
					modelLabel: "m2",
				},
			},
		},
	}

	err := c.updatePodRecord(pod, "", metrics.GPUBusyTimeRatio, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: 0.8})
	require.NoError(t, err)
	_, ok := pod.Metrics.Load(metrics.GPUBusyTimeRatio)
	require.True(t, ok)

	err = c.updatePodRecord(pod, "m1", metrics.NumRequestsRunning, metrics.PodModelMetricScope, &metrics.SimpleMetricValue{Value: 3})
	require.NoError(t, err)
	_, ok = pod.ModelMetrics.Load(c.getPodModelMetricName("m1", metrics.NumRequestsRunning))
	require.True(t, ok)

	err = c.updatePodRecord(pod, "", metrics.NumRequestsWaiting, metrics.PodModelMetricScope, &metrics.SimpleMetricValue{Value: 4})
	require.NoError(t, err)
	_, ok = pod.ModelMetrics.Load(c.getPodModelMetricName("m2", metrics.NumRequestsWaiting))
	require.True(t, ok)
}

func TestMetricRoleFilters(t *testing.T) {
	require.True(t, isPrefillOnlyMetric(metrics.TimeToFirstTokenSeconds))
	require.False(t, isPrefillOnlyMetric(metrics.GenerationTokenTotal))
	require.True(t, isDecodeOnlyMetric(metrics.TimePerOutputTokenSeconds))
	require.False(t, isDecodeOnlyMetric(metrics.PromptTokenTotal))
}

func TestShouldSkipMetric(t *testing.T) {
	require.True(t, shouldSkipMetric("llm-prefill-0", metrics.TimePerOutputTokenSeconds))
	require.True(t, shouldSkipMetric("llm-decode-0", metrics.TimeToFirstTokenSeconds))
	require.False(t, shouldSkipMetric("llm-prefill-0", metrics.TimeToFirstTokenSeconds))
	require.False(t, shouldSkipMetric("llm-decode-0", metrics.TimePerOutputTokenSeconds))
}

func TestBuildMetricLabels(t *testing.T) {
	t.Setenv("POD_NAME", "gw-pod-1")
	pod := &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llm-pod-1",
				Namespace: "ns1",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Env: []v1.EnvVar{
							{Name: "ROLESET_NAME", Value: "rs1"},
							{Name: "ROLE_NAME", Value: "role1"},
							{Name: "ROLE_REPLICA_INDEX", Value: "0"},
						},
					},
				},
			},
		},
	}

	labelNames, labelValues := buildMetricLabels(pod, "vllm", "m1")
	require.Equal(t, []string{
		"namespace",
		"pod",
		"model",
		"engine_type",
		"roleset",
		"role",
		"role_replica_index",
		"gateway_pod",
	}, labelNames)
	require.Equal(t, []string{
		"ns1",
		"llm-pod-1",
		"m1",
		"vllm",
		"rs1",
		"role1",
		"0",
		"gw-pod-1",
	}, labelValues)
}

func TestEmitMetricToPrometheus_GaugeAndCounter(t *testing.T) {
	lastCounterValues = sync.Map{}

	var gaugeCalls []struct {
		name  string
		value float64
	}
	var counterAdds []struct {
		name  string
		value float64
	}

	originalCounterFn := metrics.IncrementCounterMetricFnForTest
	originalGaugeFn := metrics.SetGaugeMetricFnForTest
	defer func() {
		metrics.IncrementCounterMetricFnForTest = originalCounterFn
		metrics.SetGaugeMetricFnForTest = originalGaugeFn
	}()

	metrics.SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		gaugeCalls = append(gaugeCalls, struct {
			name  string
			value float64
		}{name: name, value: value})
	}
	metrics.IncrementCounterMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		counterAdds = append(counterAdds, struct {
			name  string
			value float64
		}{name: name, value: value})
	}

	c := &Store{}
	labels := []string{"pod"}
	values := []string{"p1"}

	c.emitMetricToPrometheus(metrics.NumRequestsRunning, &metrics.SimpleMetricValue{Value: 3}, labels, values)
	require.Len(t, gaugeCalls, 1)
	require.Equal(t, metrics.NumRequestsRunning, gaugeCalls[0].name)
	require.Equal(t, 3.0, gaugeCalls[0].value)

	c.emitMetricToPrometheus(metrics.HTTPRequestTotal, &metrics.SimpleMetricValue{Value: 10}, labels, values)
	require.Len(t, counterAdds, 0)
	c.emitMetricToPrometheus(metrics.HTTPRequestTotal, &metrics.SimpleMetricValue{Value: 13}, labels, values)
	require.Len(t, counterAdds, 1)
	require.Equal(t, metrics.HTTPRequestTotal, counterAdds[0].name)
	require.Equal(t, 3.0, counterAdds[0].value)
}

func TestEmitMetricToPrometheus_HistogramAlsoEmitsQuantiles(t *testing.T) {
	registry := prometheus.NewRegistry()
	originalRegisterer := prometheus.DefaultRegisterer
	originalGatherer := prometheus.DefaultGatherer
	prometheus.DefaultRegisterer = registry
	prometheus.DefaultGatherer = registry
	t.Cleanup(func() {
		prometheus.DefaultRegisterer = originalRegisterer
		prometheus.DefaultGatherer = originalGatherer
	})

	var gaugeMetricNames []string
	originalGaugeFn := metrics.SetGaugeMetricFnForTest
	defer func() { metrics.SetGaugeMetricFnForTest = originalGaugeFn }()
	metrics.SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		gaugeMetricNames = append(gaugeMetricNames, name)
	}

	c := &Store{}
	hv := &metrics.HistogramMetricValue{
		Sum:   3,
		Count: 2,
		Buckets: map[string]float64{
			"0.100000": 1,
			"0.500000": 2,
			"+Inf":     2,
		},
	}
	c.emitMetricToPrometheus(metrics.TimeToFirstTokenSeconds, hv, []string{"pod"}, []string{"p1"})

	require.Contains(t, gaugeMetricNames, metrics.TimeToFirstTokenSeconds+"_p50")
	require.Contains(t, gaugeMetricNames, metrics.TimeToFirstTokenSeconds+"_p90")
	require.Contains(t, gaugeMetricNames, metrics.TimeToFirstTokenSeconds+"_p99")

	mfs, err := registry.Gather()
	require.NoError(t, err)
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == metrics.TimeToFirstTokenSeconds {
			found = true
			break
		}
	}
	require.True(t, found)
}
