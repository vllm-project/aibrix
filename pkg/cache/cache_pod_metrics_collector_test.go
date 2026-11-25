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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func runningRequestsTestPod(namespace, name string) *Pod {
	return &Pod{Pod: requestTrackerTestPod(name, namespace, "", "10.0.0.1")}
}

func runningRequestsMetricsHandler(t *testing.T, store *Store) http.Handler {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	require.NoError(t, initPodGatewayMetricsCollector(store, registry, nil))
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

func TestPodGatewayMetricsCollector_PodEligibility(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*v1.Pod)
		quarantine bool
	}{
		{name: "unready", mutate: func(pod *v1.Pod) { pod.Status.Conditions[0].Status = v1.ConditionFalse }},
		{name: "no-ready-condition", mutate: func(pod *v1.Pod) { pod.Status.Conditions = nil }},
		{name: "no-ip", mutate: func(pod *v1.Pod) { pod.Status.PodIP = "" }},
		{name: "terminating", mutate: func(pod *v1.Pod) {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
		}},
		{name: "draining", mutate: func(pod *v1.Pod) {
			pod.Annotations = map[string]string{constants.PodDrainingAnnotationKey: "true"}
		}},
		{name: "quarantined", quarantine: true},
	}
	for _, source := range []string{"snapshot", "reader"} {
		for _, tt := range tests {
			t.Run(source+"/"+tt.name, func(t *testing.T) {
				now := time.Now()
				store := NewForTest()
				store.snapshotClock = func() time.Time { return now }
				original := requestTrackerTestPod("eligible", "inference", "model", "10.0.0.1")
				store.addPod(original)
				const key = "inference/eligible"
				pod, ok := store.metaPods.Load(key)
				require.True(t, ok)
				atomic.StoreInt32(&pod.runningRequests, 2)
				maxAge := remoteRunningMaxAge
				store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{
					entries: map[string][]remoteRunningEntry{key: {{count: 5, readAt: now}}},
				})
				if source == "reader" {
					reader := newRemoteRunningReader(store, time.Second)
					reader.publish(key, 5, now)
					store.remoteRunningReader.Store(reader)
					maxAge = reader.maxAge
				}
				handler := runningRequestsMetricsHandler(t, store)
				scrape := func() map[string]float64 {
					return runningRequestGaugeSamples(t, handler, original.Namespace, original.Name)
				}
				require.Equal(t, map[string]float64{"model": 7}, scrape())

				updated := original.DeepCopy()
				if tt.mutate != nil {
					tt.mutate(updated)
					store.updatePod(original, updated)
				}
				checker := &pointerQuarantine{quarantined: tt.quarantine}
				store.SetPodQuarantineChecker(checker)
				require.Empty(t, scrape(), "ineligible pods must not look like idle or loaded routing candidates")
				now = now.Add(maxAge + time.Nanosecond)
				require.Empty(t, scrape(), "remote expiry must not bring an ineligible pod back as a local-only sample")

				store.ClearPodQuarantineChecker(checker)
				if tt.mutate != nil {
					store.updatePod(updated, original)
				}
				pod, ok = store.metaPods.Load(key)
				require.True(t, ok)
				atomic.StoreInt32(&pod.runningRequests, 0)
				require.Equal(t, map[string]float64{"model": 0}, scrape(), "a recovered pod exports a valid local zero without another sync")
			})
		}
	}
}

func TestRunningRequestsGauge_RequestInterleaving(t *testing.T) {
	for _, source := range []string{"snapshot", "reader"} {
		t.Run(source, func(t *testing.T) {
			const modelName = "model"
			now := time.Now()
			store := NewForTest()
			store.snapshotClock = func() time.Time { return now }
			original := requestTrackerTestPod("interleaved", "inference", modelName, "10.0.0.1")
			store.addPod(original)
			podKey := "inference/interleaved"
			pod, ok := store.metaPods.Load(podKey)
			require.True(t, ok)
			publish := func(count int64) {
				store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{
					entries: map[string][]remoteRunningEntry{podKey: {{count: count, readAt: now}}},
				})
			}
			if source == "reader" {
				reader := newRemoteRunningReader(store, time.Second)
				store.remoteRunningReader.Store(reader)
				publish = func(count int64) { reader.publish(podKey, count, now) }
			}
			publish(5)
			handler := runningRequestsMetricsHandler(t, store)
			check := func(want float64) {
				t.Helper()
				require.Equal(t, map[string]float64{modelName: want},
					runningRequestGaugeSamples(t, handler, original.Namespace, original.Name))
			}

			store.syncRunningRequestsGlobally(pod)
			check(5)
			ctx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", "request", "")
			ctx.SetTargetPod(original)
			term := store.AddRequestCount(ctx, "request", modelName)
			check(6)
			store.syncRunningRequestsGlobally(pod)
			check(6)
			store.DoneRequestCount(ctx, "request", modelName, term)
			check(5)

			// No engine metrics fetch or estimate sync runs from here on. The
			// record ages out, but a newly read remote count must still be visible.
			now = now.Add(10 * time.Second)
			publish(7)
			check(7)
			ctx = types.NewRoutingContext(context.Background(), "least-request", modelName, "", "later-request", "")
			ctx.SetTargetPod(original)
			term = store.AddRequestCount(ctx, "later-request", modelName)
			check(8)
			store.DoneRequestCount(ctx, "later-request", modelName, term)
			check(7)
		})
	}
}

func TestRunningRequestsGauge_RequestBeforeRemoteInitialization(t *testing.T) {
	for _, source := range []string{"snapshot", "reader"} {
		t.Run(source, func(t *testing.T) {
			const modelName = "model"
			store := NewForTest()
			pod := requestTrackerTestPod("uninitialized", "inference", modelName, "10.0.0.1")
			store.addPod(pod)
			if source == "reader" {
				// An unread active reader must not fall back to this snapshot.
				store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{})
				store.remoteRunningReader.Store(newRemoteRunningReader(store, time.Second))
			}
			handler := runningRequestsMetricsHandler(t, store)
			ctx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", "request", "")
			ctx.SetTargetPod(pod)
			term := store.AddRequestCount(ctx, "request", modelName)
			require.Empty(t, runningRequestGaugeSamples(t, handler, pod.Namespace, pod.Name))
			store.DoneRequestCount(ctx, "request", modelName, term)
			require.Empty(t, runningRequestGaugeSamples(t, handler, pod.Namespace, pod.Name))
		})
	}
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
	require.Equal(t, map[string]float64{"new-model": 7}, scrape(), "relabeling must not wait for another metric sync")
	pod, ok = store.metaPods.Load("inference/running-lifecycle")
	require.True(t, ok)

	store.deletePod(updated)
	store.syncRunningRequestsGlobally(pod)
	require.Empty(t, scrape())
	store.addPod(requestTrackerTestPod(original.Name, original.Namespace, "new-model", "10.0.0.2"))
	store.syncRunningRequestsGlobally(pod)
	require.Equal(t, map[string]float64{"new-model": 0}, scrape(), "syncing the old pod must not affect its replacement")
}

func TestRunningRequestsGauge_Freshness(t *testing.T) {
	for _, source := range []string{"snapshot", "reader"} {
		t.Run(source, func(t *testing.T) {
			now := time.Now()
			store := NewForTest()
			store.snapshotClock = func() time.Time { return now }
			pod := runningRequestsTestPod("inference", "freshness")
			pod.Labels = map[string]string{constants.ModelLabelName: "model"}
			atomic.StoreInt32(&pod.runningRequests, 2)
			const podKey = "inference/freshness"
			store.metaPods.Store(podKey, pod)
			handler := runningRequestsMetricsHandler(t, store)
			scrape := func() map[string]float64 {
				return runningRequestGaugeSamples(t, handler, pod.Namespace, pod.Name)
			}
			require.Empty(t, scrape(), "an uninitialized remote source withholds the estimate")

			maxAge := remoteRunningMaxAge
			publish := func(count int64) {
				store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{
					entries: map[string][]remoteRunningEntry{podKey: {
						{count: count, readAt: now},
						{count: 99, readAt: now.Add(-maxAge - time.Nanosecond)},
					}},
				})
			}
			if source == "reader" {
				reader := newRemoteRunningReader(store, 2*time.Second)
				store.remoteRunningReader.Store(reader)
				maxAge = reader.maxAge
				publish = func(count int64) { reader.publish(podKey, count, now) }
			}
			publish(5)
			require.Equal(t, map[string]float64{"model": 7}, scrape(), "no engine fetch or metric record is needed")

			// Request-path writes, including an old record or a nil value, must
			// neither replace the estimate nor determine whether it is fresh.
			for _, record := range []MetricRecord{
				{Timestamp: now},
				{Value: &metrics.SimpleMetricValue{Value: 99}},
				{Value: &metrics.SimpleMetricValue{Value: 99}, Timestamp: now.Add(-time.Hour)},
				{Value: &metrics.SimpleMetricValue{Value: 99}, Timestamp: now},
			} {
				pod.Metrics.Store(metrics.RealtimeNumRequestsRunning, record)
				require.Equal(t, map[string]float64{"model": 7}, scrape())
			}

			now = now.Add(maxAge)
			require.Equal(t, map[string]float64{"model": 7}, scrape(), "the remote age boundary is inclusive")
			now = now.Add(time.Nanosecond)
			require.Equal(t, map[string]float64{"model": 2}, scrape(), "expired remote contributions fall back to local")
			atomic.StoreInt32(&pod.runningRequests, 0)
			require.Equal(t, map[string]float64{"model": 0}, scrape(), "a local zero remains visible after remote expiry")
			publish(3)
			require.Equal(t, map[string]float64{"model": 3}, scrape(), "a fresh remote read is visible without a metric sync")
		})
	}
}

func TestPodGatewayMetricsCollector_SharedLabels(t *testing.T) {
	store := NewForTest()
	original := requestTrackerTestPod("decode-0", "inference", "model", "10.0.0.1")
	original.Labels = map[string]string{constants.ModelLabelName: "model", constants.ModelLabelEngine: "sglang", "role-name": "decode"}
	original.Spec.Containers = []v1.Container{{Env: []v1.EnvVar{
		{Name: "ROLESET_NAME", Value: "group-a"},
		{Name: "ROLE_NAME", Value: "prefill"},
		{Name: "ROLE_REPLICA_INDEX", Value: "2"},
	}}}
	store.addPod(original)
	pod, ok := store.metaPods.Load("inference/decode-0")
	require.True(t, ok)
	store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{})
	atomic.StoreInt32(&pod.runningRequests, 8)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPodGatewayMetricsCollector(store))
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	require.NotEmpty(t, families[0].GetHelp())
	require.Equal(t, "This gateway's estimate of in-flight requests for the target pod, including recent counts from other gateways", families[0].GetHelp())
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

	updated := original.DeepCopy()
	updated.Labels[constants.ModelLabelName] = "updated-model"
	updated.Labels[constants.ModelLabelEngine] = "vllm"
	updated.Labels["role-name"] = "prefill"
	updated.Spec.Containers[0].Env = []v1.EnvVar{
		{Name: "ROLESET_NAME", Value: "group-b"},
		{Name: "ROLE_REPLICA_INDEX", Value: "3"},
	}
	store.updatePod(original, updated)
	families, err = registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	require.Len(t, families[0].Metric, 1, "old label combinations must disappear")
	sample = families[0].Metric[0]
	labels = make(map[string]string)
	for _, label := range sample.Label {
		labels[label.GetName()] = label.GetValue()
	}
	require.Equal(t, map[string]string{
		"namespace": "inference", "pod": "decode-0", "model": "updated-model", "lora_adapter": "",
		"engine_type": "vllm", "roleset": "group-b", "role": "prefill", "role_replica_index": "3",
		"gateway_pod": metrics.GatewayPodName(),
	}, labels, "pod replacement must rebuild all cached metadata labels without another sync")
	require.Equal(t, float64(8), sample.GetGauge().GetValue())
}

func TestPodGatewayMetricsCollector_InvalidPods(t *testing.T) {
	store := NewForTest()
	store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{})
	store.metaPods.Store("inference/nil", nil)
	store.metaPods.Store("inference/empty", &Pod{})
	pod := runningRequestsTestPod("inference", "invalid-model")
	pod.Annotations = map[string]string{constants.ModelLabelName: "bad\xffname"}
	store.metaPods.Store("inference/invalid-model", pod)
	valid := runningRequestsTestPod("inference", "valid")
	valid.Labels = map[string]string{constants.ModelLabelName: "model"}
	atomic.StoreInt32(&valid.runningRequests, 2)
	store.metaPods.Store("inference/valid", valid)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPodGatewayMetricsCollector(store))
	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	require.Empty(t, runningRequestGaugeSamples(t, handler, pod.Namespace, pod.Name))
	require.Equal(t, map[string]float64{"model": 2},
		runningRequestGaugeSamples(t, handler, valid.Namespace, valid.Name), "one invalid pod must not fail the whole scrape")
}

func TestPodGatewayMetricsCollector_ConcurrentUpdatesAndScrapes(t *testing.T) {
	store := NewForTest()
	original := requestTrackerTestPod("concurrent", "inference", "model", "10.0.0.1")
	original.Spec.Containers = []v1.Container{{Env: []v1.EnvVar{{Name: "ROLESET_NAME", Value: "group-0"}}}}
	store.addPod(original)
	store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{})
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPodGatewayMetricsCollector(store))
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		current := original
		for i := 0; i < 100; i++ {
			requestID := strconv.Itoa(i)
			ctx := types.NewRoutingContext(context.Background(), "least-request", "model", "", requestID, "")
			ctx.SetTargetPod(current)
			term := store.AddRequestCount(ctx, requestID, "model")
			pod, ok := store.metaPods.Load("inference/concurrent")
			if !ok {
				t.Error("pod missing before update")
				return
			}
			store.syncRunningRequestsGlobally(pod)
			updated := current.DeepCopy()
			updated.Spec.Containers[0].Env[0].Value = "group-" + strconv.Itoa(i%2)
			updated.Status.Conditions[0].Status = v1.ConditionFalse
			store.updatePod(current, updated)
			ready := updated.DeepCopy()
			ready.Status.Conditions[0].Status = v1.ConditionTrue
			store.updatePod(updated, ready)
			store.DoneRequestCount(ctx, requestID, "model", term)
			current = ready
		}
	}()
	defer wg.Wait()
	close(start)
	for i := 0; i < 100; i++ {
		families, err := registry.Gather()
		require.NoError(t, err)
		for _, family := range families {
			for _, sample := range family.Metric {
				require.GreaterOrEqual(t, sample.GetGauge().GetValue(), float64(0))
				require.LessOrEqual(t, sample.GetGauge().GetValue(), float64(1))
				roleset, err := metrics.GetLabelValueForKey(sample, "roleset")
				require.NoError(t, err)
				require.Contains(t, []string{"group-0", "group-1"}, roleset)
			}
		}
	}
}

func TestInitWithOptions_PodGatewayMetricsCollector(t *testing.T) {
	// InitWithOptions is a process singleton. Exercise the actual production
	// registration in a child process without resetting globals used by other tests.
	const childEnv = "AIBRIX_TEST_POD_GATEWAY_METRICS_INIT"
	if os.Getenv(childEnv) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInitWithOptions_PodGatewayMetricsCollector$", "-test.count=1")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}

	t.Setenv("PROMETHEUS_ENDPOINT", "")
	t.Setenv("PROMETHEUS_BASIC_AUTH_SECRET_NAME", "")
	enableModelGPUProfileCaching = false
	enableGPUOptimizerTracing = false
	podMetricRefreshInterval = time.Hour
	registry := prometheus.NewPedanticRegistry()
	prometheus.DefaultRegisterer = registry
	prometheus.DefaultGatherer = registry
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopCollector := func() { stopOnce.Do(func() { close(stop) }) }
	t.Cleanup(stopCollector)
	store := InitWithOptions(nil, stop, InitOptions{
		IsGateway: true,
		DiscoveryProvider: &mockProvider{events: []discovery.WatchEvent{{
			Type: discovery.EventAdd, Object: requestTrackerTestPod("registered", "inference", "model", "10.0.0.1"),
		}}},
	})
	store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{})
	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	require.Equal(t, map[string]float64{"model": 0},
		runningRequestGaugeSamples(t, handler, "inference", "registered"))
	stopCollector()
	require.Eventually(t, func() bool {
		families, err := registry.Gather()
		if err != nil {
			return false
		}
		for _, family := range families {
			if family.GetName() == metrics.RealtimeNumRequestsRunning {
				return false
			}
		}
		return true
	}, time.Second, time.Millisecond)
}

func TestInitPodGatewayMetricsCollector_RegistrationLifecycle(t *testing.T) {
	store := NewForTest()
	pod := runningRequestsTestPod("inference", "registration")
	store.metaPods.Store("inference/registration", pod)
	store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{})
	atomic.StoreInt32(&pod.runningRequests, 3)
	registry := prometheus.NewPedanticRegistry()
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopCollector := func() { stopOnce.Do(func() { close(stop) }) }
	t.Cleanup(stopCollector)
	require.NoError(t, initPodGatewayMetricsCollector(store, registry, stop))
	require.Error(t, initPodGatewayMetricsCollector(store, registry, nil))
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)

	stopCollector()
	require.Eventually(t, func() bool {
		families, err := registry.Gather()
		return err == nil && len(families) == 0
	}, time.Second, time.Millisecond)
	require.NoError(t, initPodGatewayMetricsCollector(NewForTest(), registry, nil))
}
