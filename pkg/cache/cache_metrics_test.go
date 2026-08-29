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
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
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

func TestUpdatePodRecord(t *testing.T) {
	c := &Store{}
	pod := &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p1",
				Namespace: "default",
				Labels: map[string]string{
					constants.ModelLabelName: "m2",
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

func TestUpdatePodMetricsFromTypedResultModelFallback(t *testing.T) {
	tests := []struct {
		name      string
		rawModel  string
		wantModel string
	}{
		{
			name:      "keeps valid model name",
			rawModel:  "raw-model",
			wantModel: "raw-model",
		},
		{
			name:      "keeps path-style model name",
			rawModel:  "/models/mock",
			wantModel: "/models/mock",
		},
		{
			name:      "falls back for empty model name",
			rawModel:  "",
			wantModel: "pod-model",
		},
		{
			name:      "falls back for undefined model name",
			rawModel:  undefinedMetricLabelValue,
			wantModel: "pod-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &Store{}
			pod := &Pod{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "p1",
						Namespace: "default",
						Labels: map[string]string{
							constants.ModelLabelName: "pod-model",
						},
					},
				},
			}
			result := &metrics.EngineMetricsResult{
				ModelMetrics: map[string]metrics.MetricValue{
					store.getPodModelMetricName(tt.rawModel, metrics.NumRequestsWaiting): &metrics.SimpleMetricValue{Value: 1},
				},
			}

			store.updatePodMetricsFromTypedResult(pod, result)

			_, ok := pod.ModelMetrics.Load(store.getPodModelMetricName(tt.wantModel, metrics.NumRequestsWaiting))
			require.True(t, ok)
		})
	}
}

func TestUpdatePodMetricsFromTypedResultAnnotationFallback(t *testing.T) {
	store := &Store{}
	pod := &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p1",
				Namespace: "default",
				Annotations: map[string]string{
					constants.ModelLabelName: "/models/mock",
				},
			},
		},
	}
	result := &metrics.EngineMetricsResult{
		ModelMetrics: map[string]metrics.MetricValue{
			store.getPodModelMetricName("", metrics.NumRequestsWaiting): &metrics.SimpleMetricValue{Value: 1},
		},
	}

	store.updatePodMetricsFromTypedResult(pod, result)

	_, ok := pod.ModelMetrics.Load(store.getPodModelMetricName("/models/mock", metrics.NumRequestsWaiting))
	require.True(t, ok)
}

func TestSanitizeMetricLabelsFallback(t *testing.T) {
	pod := &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p1",
				Namespace: "default",
				Labels: map[string]string{
					constants.ModelLabelName: "pod-model",
					engineLabel:              "trtllm",
				},
			},
		},
	}

	sanitized := sanitizeMetricLabels(pod, map[string]string{
		"model_name":  undefinedMetricLabelValue,
		"engine_type": "",
		"instance":    "pod:8000",
	})

	require.Equal(t, "pod-model", sanitized["model_name"])
	require.Equal(t, "trtllm", sanitized["engine_type"])
	require.Equal(t, "pod:8000", sanitized["instance"])
}

func TestSanitizeMetricLabels_ReturnsSameMapWhenClean(t *testing.T) {
	pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}}

	in := map[string]string{
		"model_name":  "raw-model",
		"engine_type": "vllm",
		"instance":    "pod:8000",
	}
	out := sanitizeMetricLabels(pod, in)

	// Mutating in must show up in out — proves early-return returned the same underlying map
	// (i.e. no alloc + copy happened).
	in["__canary"] = "x"
	require.Equal(t, "x", out["__canary"], "expected early-return to return same map when labels are clean")
}

func TestSanitizeMetricLabels_NoFalsePositiveWhenKeyMissing(t *testing.T) {
	pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}}

	in := map[string]string{"instance": "pod:8000"}
	out := sanitizeMetricLabels(pod, in)

	in["__canary"] = "x"
	require.Equal(t, "x", out["__canary"], "missing model_name / engine_type keys must not trigger alloc path")
}

func TestSanitizeMetricLabels_EmptyMap(t *testing.T) {
	pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}}

	in := map[string]string{}
	out := sanitizeMetricLabels(pod, in)

	in["__canary"] = "x"
	require.Equal(t, "x", out["__canary"], "empty map must short-circuit to same map")
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
	var gaugeCalls []struct {
		name  string
		value float64
	}

	originalGaugeFn := metrics.SetGaugeMetricFnForTest
	defer func() {
		metrics.SetGaugeMetricFnForTest = originalGaugeFn
	}()

	metrics.SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		gaugeCalls = append(gaugeCalls, struct {
			name  string
			value float64
		}{name: name, value: value})
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "ns1",
		},
	}
	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: ""}, pod, metrics.NumRequestsRunning, &metrics.SimpleMetricValue{Value: 3}, nil)
	require.Len(t, gaugeCalls, 1)
	require.Equal(t, metrics.NumRequestsRunning, gaugeCalls[0].name)
	require.Equal(t, 3.0, gaugeCalls[0].value)
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

	hv := &metrics.HistogramMetricValue{
		Sum:   3,
		Count: 2,
		Buckets: map[string]float64{
			"0.100000": 1,
			"0.500000": 2,
			"+Inf":     2,
		},
	}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "ns1",
		},
	}
	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: ""}, pod, metrics.TimeToFirstTokenSeconds, hv, nil)

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

func TestLoadPrometheusBasicAuth_FromEnv(t *testing.T) {
	prometheusBasicAuthOnce = sync.Once{}
	prometheusBasicAuthUser = ""
	prometheusBasicAuthPass = ""

	t.Setenv("PROMETHEUS_BASIC_AUTH_SECRET_NAME", "")
	t.Setenv("PROMETHEUS_BASIC_AUTH_USERNAME", "u1")
	t.Setenv("PROMETHEUS_BASIC_AUTH_PASSWORD", "p1")

	loadPrometheusBasicAuth(nil)
	require.Equal(t, "u1", prometheusBasicAuthUser)
	require.Equal(t, "p1", prometheusBasicAuthPass)
}

func TestLoadPrometheusBasicAuth_FromSecretNilKubeConfig(t *testing.T) {
	prometheusBasicAuthOnce = sync.Once{}
	prometheusBasicAuthUser = ""
	prometheusBasicAuthPass = ""

	t.Setenv("PROMETHEUS_BASIC_AUTH_SECRET_NAME", "prom-basic-auth")
	t.Setenv("PROMETHEUS_BASIC_AUTH_SECRET_NAMESPACE", "ns1")

	loadPrometheusBasicAuth(nil)
	require.Equal(t, "", prometheusBasicAuthUser)
	require.Equal(t, "", prometheusBasicAuthPass)
}

func TestLoadPrometheusBasicAuth_FromSecret(t *testing.T) {
	prometheusBasicAuthOnce = sync.Once{}
	prometheusBasicAuthUser = ""
	prometheusBasicAuthPass = ""

	ns := "ns1"
	name := "prom-basic-auth"
	usernameKey := "username"
	passwordKey := "password"
	username := "u2"
	password := "p2"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expectedPath := fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", ns, name)
		if r.Method != http.MethodGet || r.URL.Path != expectedPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":%q,"namespace":%q},"data":{%q:%q,%q:%q}}`,
			name, ns,
			usernameKey, base64.StdEncoding.EncodeToString([]byte(username)),
			passwordKey, base64.StdEncoding.EncodeToString([]byte(password)),
		)
	}))
	t.Cleanup(server.Close)

	t.Setenv("PROMETHEUS_BASIC_AUTH_SECRET_NAME", name)
	t.Setenv("PROMETHEUS_BASIC_AUTH_SECRET_NAMESPACE", ns)
	t.Setenv("PROMETHEUS_BASIC_AUTH_USERNAME_KEY", usernameKey)
	t.Setenv("PROMETHEUS_BASIC_AUTH_PASSWORD_KEY", passwordKey)

	loadPrometheusBasicAuth(&rest.Config{Host: server.URL})
	require.Equal(t, username, prometheusBasicAuthUser)
	require.Equal(t, password, prometheusBasicAuthPass)
}

func TestInitPrometheusAPI_EndpointEmpty(t *testing.T) {
	prometheusBasicAuthOnce = sync.Once{}
	prometheusBasicAuthUser = ""
	prometheusBasicAuthPass = ""

	t.Setenv("PROMETHEUS_ENDPOINT", "")
	api := initPrometheusAPI(nil)
	require.Nil(t, api)
}

func TestInitPrometheusAPI_EndpointSet(t *testing.T) {
	prometheusBasicAuthOnce = sync.Once{}
	prometheusBasicAuthUser = ""
	prometheusBasicAuthPass = ""

	t.Setenv("PROMETHEUS_ENDPOINT", "http://example.com")
	t.Setenv("PROMETHEUS_BASIC_AUTH_SECRET_NAME", "")
	t.Setenv("PROMETHEUS_BASIC_AUTH_USERNAME", "u3")
	t.Setenv("PROMETHEUS_BASIC_AUTH_PASSWORD", "p3")

	api := initPrometheusAPI(nil)
	require.NotNil(t, api)
}

func TestUpdateModelReplicaMetrics(t *testing.T) {
	t.Setenv("POD_NAME", "gateway-0")

	var emitted []map[string]string
	originalFn := metrics.SetGaugeMetricFnForTest
	defer func() { metrics.SetGaugeMetricFnForTest = originalFn }()
	metrics.SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		if name != metrics.ModelReplicas {
			return
		}
		labels := make(map[string]string, len(labelNames))
		for i, ln := range labelNames {
			labels[ln] = labelValues[i]
		}
		emitted = append(emitted, labels)
	}

	readyPod := func(name, model, role string, groupIndex string) *Pod {
		labels := map[string]string{
			constants.ModelLabelName: model,
			pdRoleIdentifier:         role,
		}
		if groupIndex != "" {
			labels[podGroupIndex] = groupIndex
		}
		return &Pod{
			Pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "default",
					Labels:    labels,
				},
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					PodIP: "10.0.0.1",
					Conditions: []v1.PodCondition{
						{Type: v1.PodReady, Status: v1.ConditionTrue},
					},
				},
			},
		}
	}

	store := &Store{}
	store.metaPods.Store("default/prefill-0", readyPod("prefill-0", "qwen3-8B", "prefill", "0"))
	store.metaPods.Store("default/decode-0", readyPod("decode-0", "qwen3-8B", "decode", "0"))
	store.metaPods.Store("default/prefill-worker", readyPod("prefill-worker", "qwen3-8B", "prefill", "1"))
	store.metaPods.Store("default/unready", &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "unready",
				Namespace: "default",
				Labels: map[string]string{
					constants.ModelLabelName: "qwen3-8B",
					pdRoleIdentifier:         "prefill",
				},
			},
			Status: v1.PodStatus{Phase: v1.PodPending},
		},
	})

	store.updateModelReplicaMetrics()
	require.Len(t, emitted, 2)
	byPod := make(map[string]map[string]string, len(emitted))
	for _, labels := range emitted {
		byPod[labels["pod"]] = labels
	}
	require.Equal(t, "prefill", byPod["prefill-0"]["role"])
	require.Equal(t, "qwen3-8B", byPod["prefill-0"]["model_name"])
	require.Equal(t, "decode", byPod["decode-0"]["role"])
	require.Equal(t, "qwen3-8B", byPod["decode-0"]["model_name"])
	require.Equal(t, 2, store.modelReplicaEmitted.Len())

	store.metaPods.Delete("default/prefill-0")
	emitted = nil
	store.updateModelReplicaMetrics()
	require.Len(t, emitted, 1)
	require.Equal(t, "decode-0", emitted[0]["pod"])
	require.Equal(t, 1, store.modelReplicaEmitted.Len())
}

func TestUpdatePodMetricsNonBlocking(t *testing.T) {
	var counters []counterCall
	restore := captureCounterCalls(&counters)
	defer restore()

	store := &Store{
		podMetricsJobs: make(chan *Pod, 2),
	}

	for i := range 5 {
		name := fmt.Sprintf("pod-%d", i)
		pod := newReadyMetricsPod(name, "uid-"+name)
		pod.Status.PodIP = fmt.Sprintf("10.0.0.%d", i+1)
		store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	}

	store.updatePodMetrics()

	require.Equal(t, 2, len(store.podMetricsJobs))
	require.Equal(t, 3, countCounterCalls(counters, metrics.PodMetricsEnqueueDroppedTotal, "reason", metrics.PodMetricsDropReasonQueueFull))
}

func TestPodMetricsConfigDefaultsAndOverrides(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", "")
		t.Setenv("AIBRIX_POD_METRICS_WORKER_COUNT", "")
		t.Setenv("AIBRIX_POD_METRICS_JOB_QUEUE_SIZE", "")

		require.Equal(t, time.Second, loadPodMetricRefreshInterval())
		workerCount := loadPodMetricsWorkerCount()
		require.Equal(t, 10, workerCount)
		require.Equal(t, 100, loadPodMetricsJobQueueSize(workerCount))
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", "2500")
		t.Setenv("AIBRIX_POD_METRICS_WORKER_COUNT", "20")
		t.Setenv("AIBRIX_POD_METRICS_JOB_QUEUE_SIZE", "55")

		require.Equal(t, 2500*time.Millisecond, loadPodMetricRefreshInterval())
		workerCount := loadPodMetricsWorkerCount()
		require.Equal(t, 20, workerCount)
		require.Equal(t, 55, loadPodMetricsJobQueueSize(workerCount))
	})

	t.Run("invalid values", func(t *testing.T) {
		t.Setenv("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", "0")
		t.Setenv("AIBRIX_POD_METRICS_WORKER_COUNT", "-1")
		t.Setenv("AIBRIX_POD_METRICS_JOB_QUEUE_SIZE", "bad")

		require.Equal(t, time.Second, loadPodMetricRefreshInterval())
		workerCount := loadPodMetricsWorkerCount()
		require.Equal(t, 10, workerCount)
		require.Equal(t, 100, loadPodMetricsJobQueueSize(workerCount))
	})
}

func TestPodMetricsBackoffTransitions(t *testing.T) {
	store := &Store{}
	pod := newReadyMetricsPod("pod-0", "uid-0")
	now := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)

	require.False(t, store.shouldSkipPodMetricsFetch(pod, now))

	store.recordPodMetricsFetchFailure(pod, now)
	state, ok := store.podMetricsBackoff.Load(podMetricsBackoffKey(pod))
	require.True(t, ok)
	require.Equal(t, 1, state.failures)
	require.Equal(t, now.Add(time.Second), state.nextFetchAt)
	require.True(t, store.shouldSkipPodMetricsFetch(pod, now.Add(500*time.Millisecond)))
	require.False(t, store.shouldSkipPodMetricsFetch(pod, now.Add(time.Second)))

	store.recordPodMetricsFetchFailure(pod, now.Add(time.Second))
	state, ok = store.podMetricsBackoff.Load(podMetricsBackoffKey(pod))
	require.True(t, ok)
	require.Equal(t, 2, state.failures)
	require.Equal(t, now.Add(3*time.Second), state.nextFetchAt)

	store.recordPodMetricsFetchSuccess(pod)
	require.Equal(t, 0, store.podMetricsBackoff.Len())
}

func TestPodMetricsBackoffCapsAtMaxDelay(t *testing.T) {
	store := &Store{}
	pod := newReadyMetricsPod("pod-0", "uid-0")
	now := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)

	for range 10 {
		store.recordPodMetricsFetchFailure(pod, now)
	}

	state, ok := store.podMetricsBackoff.Load(podMetricsBackoffKey(pod))
	require.True(t, ok)
	require.Equal(t, now.Add(30*time.Second), state.nextFetchAt)
}

func TestPodMetricsBackoffConcurrentFailuresDoNotLoseUpdates(t *testing.T) {
	store := &Store{}
	pod := newReadyMetricsPod("pod-0", "uid-0")
	now := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	const failureCount = 100

	var wg sync.WaitGroup
	wg.Add(failureCount)
	for range failureCount {
		go func() {
			defer wg.Done()
			store.recordPodMetricsFetchFailure(pod, now)
		}()
	}
	wg.Wait()

	state, ok := store.podMetricsBackoff.Load(podMetricsBackoffKey(pod))
	require.True(t, ok)
	require.Equal(t, failureCount, state.failures)
	require.Equal(t, now.Add(30*time.Second), state.nextFetchAt)
}

func TestPodMetricsBackoffUIDChangeClearsState(t *testing.T) {
	store := &Store{}
	pod := newReadyMetricsPod("pod-0", "uid-0")
	now := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)

	store.recordPodMetricsFetchFailure(pod, now)
	recreated := newReadyMetricsPod("pod-0", "uid-1")

	require.False(t, store.shouldSkipPodMetricsFetch(recreated, now.Add(500*time.Millisecond)))
	require.Equal(t, 0, store.podMetricsBackoff.Len())
}

func TestUpdatePodMetricsSkipsBackoffPods(t *testing.T) {
	var counters []counterCall
	restore := captureCounterCalls(&counters)
	defer restore()

	store := &Store{
		podMetricsJobs: make(chan *Pod, 2),
	}
	backoffPod := newReadyMetricsPod("pod-backoff", "uid-backoff")
	readyPod := newReadyMetricsPod("pod-ready", "uid-ready")
	store.metaPods.Store(utils.GeneratePodKey(backoffPod.Namespace, backoffPod.Name), backoffPod)
	store.metaPods.Store(utils.GeneratePodKey(readyPod.Namespace, readyPod.Name), readyPod)
	store.recordPodMetricsFetchFailure(backoffPod, time.Now())

	store.updatePodMetrics()

	require.Equal(t, 1, len(store.podMetricsJobs))
	require.Equal(t, 1, countCounterCalls(counters, metrics.PodMetricsEnqueueDroppedTotal, "reason", metrics.PodMetricsDropReasonBackoff))
}

func TestDeletePodClearsPodMetricsBackoff(t *testing.T) {
	store := &Store{}
	pod := newReadyMetricsPod("pod-0", "uid-0")
	store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	store.recordPodMetricsFetchFailure(pod, time.Now())
	require.Equal(t, 1, store.podMetricsBackoff.Len())

	store.deletePod(pod.Pod)

	require.Equal(t, 0, store.podMetricsBackoff.Len())
}

func BenchmarkUpdatePodMetricsEnqueue(b *testing.B) {
	scenarios := []struct {
		name         string
		pods         int
		queueSize    int
		backoffEvery int
	}{
		{name: "pods_100_no_backoff", pods: 100, queueSize: 100},
		{name: "pods_1000_no_backoff", pods: 1000, queueSize: 1000},
		{name: "pods_1000_queue_full", pods: 1000, queueSize: 10},
		{name: "pods_1000_50pct_backoff", pods: 1000, queueSize: 1000, backoffEvery: 2},
		{name: "pods_1000_90pct_backoff", pods: 1000, queueSize: 1000, backoffEvery: 10},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			var counters []counterCall
			restore := captureCounterCalls(&counters)
			defer restore()

			store := &Store{
				podMetricsJobs: make(chan *Pod, scenario.queueSize),
			}
			now := time.Now()
			for i := range scenario.pods {
				pod := newReadyMetricsPod(fmt.Sprintf("pod-%d", i), fmt.Sprintf("uid-%d", i))
				store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
				if scenario.backoffEvery > 0 && i%scenario.backoffEvery != 0 {
					store.recordPodMetricsFetchFailure(pod, now)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			var enqueuedTotal int
			for range b.N {
				store.podMetricsJobs = make(chan *Pod, scenario.queueSize)
				store.updatePodMetrics()
				enqueuedTotal += len(store.podMetricsJobs)
			}
			b.StopTimer()

			b.ReportMetric(float64(enqueuedTotal)/float64(b.N), "jobs_enqueued/op")
			b.ReportMetric(float64(countCounterCalls(counters, metrics.PodMetricsEnqueueDroppedTotal, "reason", metrics.PodMetricsDropReasonQueueFull))/float64(b.N), "queue_full_drops/op")
			b.ReportMetric(float64(countCounterCalls(counters, metrics.PodMetricsEnqueueDroppedTotal, "reason", metrics.PodMetricsDropReasonBackoff))/float64(b.N), "backoff_drops/op")
		})
	}
}

type counterCall struct {
	name        string
	labelNames  []string
	labelValues []string
}

func captureCounterCalls(calls *[]counterCall) func() {
	originalFn := metrics.IncrementCounterMetricFnForTest
	metrics.IncrementCounterMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		*calls = append(*calls, counterCall{
			name:        name,
			labelNames:  append([]string(nil), labelNames...),
			labelValues: append([]string(nil), labelValues...),
		})
	}
	return func() {
		metrics.IncrementCounterMetricFnForTest = originalFn
	}
}

func countCounterCalls(calls []counterCall, name, labelName, labelValue string) int {
	count := 0
	for _, call := range calls {
		if call.name != name {
			continue
		}
		for i, ln := range call.labelNames {
			if ln == labelName && i < len(call.labelValues) && call.labelValues[i] == labelValue {
				count++
			}
		}
	}
	return count
}

func newReadyMetricsPod(name, uid string) *Pod {
	return &Pod{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				UID:       k8stypes.UID(uid),
				Labels: map[string]string{
					constants.ModelLabelName: "model",
				},
			},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
				PodIP: "10.0.0.1",
				Conditions: []v1.PodCondition{
					{Type: v1.PodReady, Status: v1.ConditionTrue},
				},
			},
		},
		Models: utils.NewRegistry[string](),
	}
}
