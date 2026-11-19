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

package cache

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
)

func runningRequestsMetricsHandler(t *testing.T, store *Store) http.Handler {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	require.NoError(t, initPodRunningRequestsCollector(store, registry, nil))
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

func runningRequestGaugeSamples(t *testing.T, handler http.Handler, namespace, podName string) map[string]float64 {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(response.Body)
	require.NoError(t, err)
	values := make(map[string]float64)
	for _, sample := range families[metrics.RealtimeNumRequestsRunning].GetMetric() {
		name, _ := metrics.GetLabelValueForKey(sample, "pod")
		ns, _ := metrics.GetLabelValueForKey(sample, "namespace")
		if name == podName && ns == namespace {
			model, err := metrics.GetLabelValueForKey(sample, "model")
			require.NoError(t, err)
			require.NotNil(t, sample.Gauge)
			_, duplicate := values[model]
			require.False(t, duplicate, "duplicate series for model %q", model)
			values[model] = sample.Gauge.GetValue()
		}
	}
	return values
}

func TestRunningRequestsGauge_PodLifecycle(t *testing.T) {
	store := NewForTest()
	original := requestTrackerTestPod("running-lifecycle", "inference", "old-model", "10.0.0.1")
	updated := original.DeepCopy()
	updated.Labels[constants.ModelLabelName] = "new-model"
	store.addPod(original)
	store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{})
	pod, ok := store.metaPods.Load("inference/running-lifecycle")
	require.True(t, ok)
	atomic.StoreInt32(&pod.runningRequests, 7)
	store.syncRunningRequestsGlobally(pod)
	handler := runningRequestsMetricsHandler(t, store)
	scrape := func() map[string]float64 {
		return runningRequestGaugeSamples(t, handler, original.Namespace, original.Name)
	}
	require.Equal(t, map[string]float64{"old-model": 7}, scrape())

	store.updatePod(original, updated)
	require.Empty(t, scrape(), "the replacement pod has not computed a new estimate yet")
	pod, ok = store.metaPods.Load("inference/running-lifecycle")
	require.True(t, ok)
	atomic.StoreInt32(&pod.runningRequests, 0)
	store.syncRunningRequestsGlobally(pod)
	require.Equal(t, map[string]float64{"new-model": 0}, scrape())

	store.deletePod(updated)
	store.syncRunningRequestsGlobally(pod)
	require.Empty(t, scrape())
	store.addPod(requestTrackerTestPod(original.Name, original.Namespace, "new-model", "10.0.0.2"))
	store.syncRunningRequestsGlobally(pod)
	require.Empty(t, scrape(), "a recreated pod must not inherit the old sample")
	pod, ok = store.metaPods.Load("inference/running-lifecycle")
	require.True(t, ok)
	store.syncRunningRequestsGlobally(pod)
	require.Equal(t, map[string]float64{"new-model": 0}, scrape())
}

func TestPodRunningRequestsCollector_Freshness(t *testing.T) {
	store := NewForTest()
	pod := namespacedTestPod("inference", "freshness")
	pod.Labels = map[string]string{constants.ModelLabelName: "model"}
	store.metaPods.Store("inference/freshness", pod)
	collector := newPodRunningRequestsCollector(store)
	now := time.Now()
	collector.now = func() time.Time { return now }
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	scrape := func() map[string]float64 {
		return runningRequestGaugeSamples(t, handler, pod.Namespace, pod.Name)
	}
	require.Empty(t, scrape(), "never-synced pods have no estimate")
	pod.Metrics.Store(metrics.RealtimeNumRequestsRunning, MetricRecord{Timestamp: now})
	require.Empty(t, scrape(), "nil values must be skipped")
	pod.Metrics.Store(metrics.RealtimeNumRequestsRunning, MetricRecord{Value: &metrics.SimpleMetricValue{Value: 3}})
	require.Empty(t, scrape(), "untimestamped values must be skipped")
	pod.Metrics.Store(metrics.RealtimeNumRequestsRunning, MetricRecord{
		Value: &metrics.SimpleMetricValue{Value: 3}, Timestamp: now,
	})
	require.Equal(t, map[string]float64{"model": 3}, scrape())
	now = now.Add(collector.maxAge)
	require.Equal(t, map[string]float64{"model": 3}, scrape(), "the age boundary is inclusive")
	now = now.Add(time.Nanosecond)
	require.Empty(t, scrape(), "failed or skipped syncs must not keep an estimate alive")
	pod.Metrics.Store(metrics.RealtimeNumRequestsRunning, MetricRecord{
		Value: &metrics.SimpleMetricValue{Value: 0}, Timestamp: now,
	})
	require.Equal(t, map[string]float64{"model": 0}, scrape(), "a fresh zero is a real observation")
}

func TestPodRunningRequestsCollector_SharedLabels(t *testing.T) {
	store := NewForTest()
	pod := namespacedTestPod("inference", "decode-0")
	pod.Labels = map[string]string{constants.ModelLabelName: "model", constants.ModelLabelEngine: "sglang", "role-name": "decode"}
	pod.Spec.Containers = []v1.Container{{Env: []v1.EnvVar{
		{Name: "ROLESET_NAME", Value: "group-a"},
		{Name: "ROLE_NAME", Value: "prefill"},
		{Name: "ROLE_REPLICA_INDEX", Value: "2"},
	}}}
	store.metaPods.Store("inference/decode-0", pod)
	require.NoError(t, store.updatePodRecord(pod, "", metrics.RealtimeNumRequestsRunning,
		metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: 8}))
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPodRunningRequestsCollector(store))
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	require.Equal(t, metrics.GetMetricHelp(metrics.RealtimeNumRequestsRunning), families[0].GetHelp())
	require.Len(t, families[0].Metric, 1)
	sample := families[0].Metric[0]
	labels := make(map[string]string)
	for _, label := range sample.Label {
		labels[label.GetName()] = label.GetValue()
	}
	require.Equal(t, map[string]string{
		"namespace": "inference", "pod": "decode-0", "model": "model", "lora_adapter": "",
		"engine_type": "sglang", "roleset": "group-a", "role": "decode", "role_replica_index": "2",
		"gateway_pod": metrics.GatewayPodName(),
	}, labels)
	require.Equal(t, float64(8), sample.GetGauge().GetValue())
}

func TestPodRunningRequestsCollector_InvalidPods(t *testing.T) {
	store := NewForTest()
	store.metaPods.Store("inference/nil", nil)
	store.metaPods.Store("inference/empty", &Pod{})
	pod := namespacedTestPod("inference", "invalid-model")
	pod.Annotations = map[string]string{constants.ModelLabelName: "bad\xffname"}
	store.metaPods.Store("inference/invalid-model", pod)
	require.NoError(t, store.updatePodRecord(pod, "", metrics.RealtimeNumRequestsRunning,
		metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: 1}))
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPodRunningRequestsCollector(store))
	_, err := registry.Gather()
	require.ErrorContains(t, err, "not valid UTF-8", "bad metadata must report an error instead of panicking")
	store.metaPods.Delete("inference/invalid-model")
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Empty(t, families)
}

func TestPodRunningRequestsCollector_ConcurrentUpdatesAndScrapes(t *testing.T) {
	store := NewForTest()
	pod := namespacedTestPod("inference", "concurrent")
	store.metaPods.Store("inference/concurrent", pod)
	store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{})
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPodRunningRequestsCollector(store))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			atomic.StoreInt32(&pod.runningRequests, int32(i))
			store.syncRunningRequestsGlobally(pod)
			store.metaPods.Delete("inference/concurrent")
			store.metaPods.Store("inference/concurrent", pod)
		}
	}()
	defer wg.Wait()
	for i := 0; i < 100; i++ {
		_, err := registry.Gather()
		require.NoError(t, err)
	}
}

func TestInitPodRunningRequestsCollector_RegistrationLifecycle(t *testing.T) {
	store := NewForTest()
	pod := namespacedTestPod("inference", "registration")
	store.metaPods.Store("inference/registration", pod)
	require.NoError(t, store.updatePodRecord(pod, "", metrics.RealtimeNumRequestsRunning,
		metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: 3}))
	registry := prometheus.NewPedanticRegistry()
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopCollector := func() { stopOnce.Do(func() { close(stop) }) }
	t.Cleanup(stopCollector)
	require.NoError(t, initPodRunningRequestsCollector(store, registry, stop))
	require.Error(t, initPodRunningRequestsCollector(store, registry, nil))
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)

	stopCollector()
	require.Eventually(t, func() bool {
		families, err := registry.Gather()
		return err == nil && len(families) == 0
	}, time.Second, time.Millisecond)
	require.NoError(t, initPodRunningRequestsCollector(NewForTest(), registry, nil))
}
