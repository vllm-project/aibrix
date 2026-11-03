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

package metrics

import (
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testMetricName  = "test_metric"
	testMetricHelp  = "Test metric for unit tests"
	testLabelName   = "test_label"
	testLabelValue  = "test_value"
	testLabelName2  = "test_label2"
	testLabelValue2 = "test_value2"
)

func TestSetGaugeMetric(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	registry := prometheus.NewRegistry()
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: testMetricName, Help: testMetricHelp},
		[]string{testLabelName},
	)
	registry.MustRegister(gauge)

	originalFn := SetGaugeMetricFnForTest
	defer func() { SetGaugeMetricFnForTest = originalFn }()

	SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		if name == testMetricName {
			gauge.WithLabelValues(labelValues...).Set(value)
		}
	}

	testValue := 42.0
	SetGaugeMetric(testMetricName, testMetricHelp, testValue, []string{testLabelName}, testLabelValue)

	value := testutil.ToFloat64(gauge.WithLabelValues(testLabelValue))
	assert.Equal(t, testValue, value, "Metric value should match what was set")
}

func TestSetupMetricsForTest(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	testGauge, cleanup := SetupMetricsForTest(testMetricName, []string{testLabelName})
	defer cleanup()

	testValue := 42.0
	SetGaugeMetric(testMetricName, testMetricHelp, testValue, []string{testLabelName}, testLabelValue)

	value := testutil.ToFloat64(testGauge.WithLabelValues(testLabelValue))
	assert.Equal(t, testValue, value, "Metric value should match what was set")

	cleanup()
	SetGaugeMetric(testMetricName, testMetricHelp, 99.0, []string{testLabelName}, testLabelValue)

	value = testutil.ToFloat64(testGauge.WithLabelValues(testLabelValue))
	assert.Equal(t, testValue, value, "Metric value should not change after cleanup")
}

func TestDeleteGaugeMetric(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	const name = "test_delete_gauge_metric"

	// Deleting before the gauge exists is a no-op.
	DeleteGaugeMetric(name, []string{testLabelName, testLabelName2}, testLabelValue, testLabelValue2)

	defaultSetGaugeMetric(name, testMetricHelp, 1, []string{testLabelName, testLabelName2}, "a", "b")
	defaultSetGaugeMetric(name, testMetricHelp, 2, []string{testLabelName, testLabelName2}, "c", "d")
	customGaugesMu.RLock()
	gauge := customGauges[name]
	customGaugesMu.RUnlock()
	assert.Equal(t, 2, testutil.CollectAndCount(gauge))

	// Label order need not match the registration order.
	DeleteGaugeMetric(name, []string{testLabelName2, testLabelName}, "b", "a")
	assert.Equal(t, 1, testutil.CollectAndCount(gauge))
	assert.Equal(t, 2.0, testutil.ToFloat64(gauge.WithLabelValues("c", "d")))

	// Mismatched or unknown labels do nothing.
	DeleteGaugeMetric(name, []string{testLabelName}, "c", "d")
	DeleteGaugeMetric(name, []string{testLabelName, testLabelName2}, "x", "y")
	assert.Equal(t, 1, testutil.CollectAndCount(gauge))

	DeleteGaugeMetric(name, []string{testLabelName, testLabelName2}, "c", "d")
	assert.Equal(t, 0, testutil.CollectAndCount(gauge))
}

func TestMetricRegistrationOnlyOnce(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	customGauges = make(map[string]*prometheus.GaugeVec)

	for i := 0; i < 5; i++ {
		SetGaugeMetric(testMetricName, testMetricHelp, float64(i), []string{testLabelName}, testLabelValue)
	}

	customGaugesMu.RLock()
	count := len(customGauges)
	customGaugesMu.RUnlock()

	assert.Equal(t, 1, count, "Should only register the metric once")
}

func TestConcurrentMetricUpdates(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	customGauges = make(map[string]*prometheus.GaugeVec)

	numGoroutines := 10
	numUpdatesPerGoroutine := 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numUpdatesPerGoroutine; j++ {
				labelValue := testLabelValue + "_" + string(rune('A'+id))
				SetGaugeMetric(testMetricName, testMetricHelp, float64(j), []string{testLabelName}, labelValue)
			}
		}(i)
	}

	wg.Wait()

	customGaugesMu.RLock()
	count := len(customGauges)
	gauge := customGauges[testMetricName]
	customGaugesMu.RUnlock()

	assert.Equal(t, 1, count, "Should only register the metric once even with concurrent updates")

	ch := make(chan *prometheus.Desc, 1)
	gauge.Describe(ch)
	desc := <-ch
	assert.Contains(t, desc.String(), testMetricName, "Metric description should contain the metric name")
}

func TestMultipleLabels(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	customGauges = make(map[string]*prometheus.GaugeVec)

	registry := prometheus.NewRegistry()
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: testMetricName, Help: testMetricHelp},
		[]string{testLabelName, testLabelName2},
	)
	registry.MustRegister(gauge)

	originalFn := SetGaugeMetricFnForTest
	defer func() { SetGaugeMetricFnForTest = originalFn }()

	SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		if name == testMetricName {
			gauge.WithLabelValues(labelValues...).Set(value)
		}
	}

	testValue := 42.0
	SetGaugeMetric(testMetricName, testMetricHelp, testValue,
		[]string{testLabelName, testLabelName2},
		testLabelValue, testLabelValue2)

	value := testutil.ToFloat64(gauge.WithLabelValues(testLabelValue, testLabelValue2))
	assert.Equal(t, testValue, value, "Metric value with multiple labels should match what was set")
}

func TestSetupCounterMetricsForTest(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	customCounters = make(map[string]*prometheus.CounterVec)

	testCounter, cleanup := SetupCounterMetricsForTest("test_counter", []string{"pod", "model"})
	defer cleanup()

	IncrementCounterMetric("test_counter", "Test counter metric", 5.0, []string{"pod", "model"}, "pod-1", "model-1")

	metricValue := testutil.ToFloat64(testCounter.WithLabelValues("pod-1", "model-1"))
	assert.Equal(t, 5.0, metricValue, "Counter metric value should match the incremented value")

	IncrementCounterMetric("test_counter", "Test counter metric", 3.0, []string{"pod", "model"}, "pod-1", "model-1")

	metricValue = testutil.ToFloat64(testCounter.WithLabelValues("pod-1", "model-1"))
	assert.Equal(t, 8.0, metricValue, "Counter metric value should accumulate")

	IncrementCounterMetric("test_counter", "Test counter metric", 10.0, []string{"pod", "model"}, "pod-2", "model-1")

	metricValue = testutil.ToFloat64(testCounter.WithLabelValues("pod-1", "model-1"))
	assert.Equal(t, 8.0, metricValue, "Original counter metric should be unchanged")

	metricValue = testutil.ToFloat64(testCounter.WithLabelValues("pod-2", "model-1"))
	assert.Equal(t, 10.0, metricValue, "Counter metric with different labels should have correct value")
}

func TestSetHistogramMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = registry
	prometheus.DefaultGatherer = registry
	customHistograms = make(map[string]*histogramCollector)

	hv := &HistogramMetricValue{
		Sum:   3,
		Count: 2,
		Buckets: map[string]float64{
			"0.100000": 1,
			"0.500000": 2,
		},
	}

	SetHistogramMetric("test_histogram", "test histogram", hv, []string{"pod"}, "pod-1")

	mfs, err := registry.Gather()
	assert.NoError(t, err)

	var found bool
	for _, mf := range mfs {
		if mf.GetName() != "test_histogram" {
			continue
		}
		found = true
		assert.NotNil(t, mf.GetMetric())
		assert.Len(t, mf.GetMetric(), 1)
		h := mf.GetMetric()[0].GetHistogram()
		assert.NotNil(t, h)
		assert.Equal(t, uint64(2), h.GetSampleCount())
		assert.InDelta(t, 3.0, h.GetSampleSum(), 1e-9)
	}
	assert.True(t, found)
}

func TestBuildMetricLabels_LoraVsBase(t *testing.T) {
	t.Setenv("POD_NAME", "gw-0")
	gatewayPodName = "gw-0"

	loraCtx := &types.RoutingContext{Model: "hip-adapter", BaseModel: "qwen3-8b"}
	names, values := buildMetricLabels(nil, loraCtx, nil)
	got := zipLabels(names, values)
	assert.Equal(t, "qwen3-8b", got["model"])
	assert.Equal(t, "hip-adapter", got["lora_adapter"])

	baseCtx := &types.RoutingContext{Model: "qwen3-8b"}
	names, values = buildMetricLabels(nil, baseCtx, nil)
	got = zipLabels(names, values)
	assert.Equal(t, "qwen3-8b", got["model"])
	assert.Equal(t, "", got["lora_adapter"])
}

func zipLabels(names, values []string) map[string]string {
	out := make(map[string]string, len(names))
	for i, n := range names {
		out[n] = values[i]
	}
	return out
}

// podQuantileSeries counts the series of the custom gauge name that belong to pod.
func podQuantileSeries(t *testing.T, name, pod string) int {
	customGaugesMu.RLock()
	gauge := customGauges[name]
	customGaugesMu.RUnlock()
	if gauge == nil {
		return 0
	}
	ch := make(chan prometheus.Metric)
	go func() {
		gauge.Collect(ch)
		close(ch)
	}()
	n := 0
	for m := range ch {
		var out dto.Metric
		assert.NoError(t, m.Write(&out))
		for _, l := range out.Label {
			if l.GetName() == "pod" && l.GetValue() == pod {
				n++
			}
		}
	}
	return n
}

func TestEmitHistogramWithoutObservationsExportsNoQuantiles(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	ctx := &types.RoutingContext{}
	quantiles := []string{"_p50", "_p90", "_p99"}
	empty := func() *HistogramMetricValue {
		return &HistogramMetricValue{Buckets: map[string]float64{"0.300000": 0, "0.800000": 0, "+Inf": 0}}
	}

	// A pod that has not served a request yet.
	fresh := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "quantile-fresh-pod", Namespace: "ns1"}}
	EmitMetricToPrometheus(ctx, fresh, TimeToFirstTokenSeconds, empty(), nil)
	for _, q := range quantiles {
		assert.Equal(t, 0, podQuantileSeries(t, TimeToFirstTokenSeconds+q, fresh.Name), q)
	}

	// A pod with observations, then the same pod after its engine restarted.
	served := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "quantile-served-pod", Namespace: "ns1"}}
	withData := &HistogramMetricValue{Sum: 1.2, Count: 2, Buckets: map[string]float64{"0.300000": 0, "0.800000": 2, "+Inf": 2}}
	EmitMetricToPrometheus(ctx, served, TimeToFirstTokenSeconds, withData, nil)
	for _, q := range quantiles {
		assert.Equal(t, 1, podQuantileSeries(t, TimeToFirstTokenSeconds+q, served.Name), q)
	}
	EmitMetricToPrometheus(ctx, served, TimeToFirstTokenSeconds, empty(), nil)
	for _, q := range quantiles {
		assert.Equal(t, 0, podQuantileSeries(t, TimeToFirstTokenSeconds+q, served.Name), q)
	}
}
