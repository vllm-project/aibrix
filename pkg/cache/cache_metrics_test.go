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
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
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

type staticMetricSubscriber []string

func (s staticMetricSubscriber) SubscribedMetrics() []string {
	return []string(s)
}

func TestAddSubscriberInitializesMetricSet(t *testing.T) {
	store := &Store{}
	require.NotPanics(t, func() {
		store.AddSubscriber(staticMetricSubscriber{"metric-a", "metric-b"})
	})
	require.Equal(t, "yes", store.metrics["metric-a"])
	require.Equal(t, "yes", store.metrics["metric-b"])
}

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
	named := func(name string) *v1.Pod { return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}} }
	labeled := func(role string) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "model-127-0-0-1-2", Labels: map[string]string{pdRoleIdentifier: role}}}
	}

	require.True(t, shouldSkipMetric(named("llm-prefill-0"), metrics.TimePerOutputTokenSeconds))
	require.True(t, shouldSkipMetric(named("llm-decode-0"), metrics.TimeToFirstTokenSeconds))
	require.False(t, shouldSkipMetric(named("llm-prefill-0"), metrics.TimeToFirstTokenSeconds))
	require.False(t, shouldSkipMetric(named("llm-decode-0"), metrics.TimePerOutputTokenSeconds))

	// Static-discovery pods carry the role only in the label.
	require.True(t, shouldSkipMetric(labeled("prefill"), metrics.TimePerOutputTokenSeconds))
	require.True(t, shouldSkipMetric(labeled("decode"), metrics.TimeToFirstTokenSeconds))
	require.False(t, shouldSkipMetric(labeled("prefill"), metrics.TimeToFirstTokenSeconds))
	require.False(t, shouldSkipMetric(labeled("decode"), metrics.TimePerOutputTokenSeconds))

	require.False(t, shouldSkipMetric(named("model-127-0-0-1-2"), metrics.TimeToFirstTokenSeconds))
	require.False(t, shouldSkipMetric(nil, metrics.TimeToFirstTokenSeconds))
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

func TestSyncRunningRequestsGloballyExportsGauge(t *testing.T) {
	tests := []struct {
		name       string
		label      string
		annotation string
		wantModel  string
	}{
		{name: "label", label: "label-model", annotation: "org/annotation-model", wantModel: "label-model"},
		{name: "annotation", annotation: "org/annotation-model", wantModel: "org/annotation-model"},
		{name: "no-metadata"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &Store{}
			pod := runningRequestsTestPod("inference", "running-gauge-test-"+tt.name)
			pod.Labels = map[string]string{constants.ModelLabelName: tt.label}
			pod.Annotations = map[string]string{constants.ModelLabelName: tt.annotation}
			podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
			store.metaPods.Store(podKey, pod)
			handler := runningRequestsMetricsHandler(t, store)

			checkGauge := func(want float64) {
				t.Helper()
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				require.Equal(t, http.StatusOK, w.Code)
				var parser expfmt.TextParser
				families, err := parser.TextToMetricFamilies(w.Body)
				require.NoError(t, err)
				family := families[metrics.RealtimeNumRequestsRunning]
				require.NotNil(t, family, "missing %s metric family in /metrics response", metrics.RealtimeNumRequestsRunning)
				for _, m := range family.GetMetric() {
					name, _ := metrics.GetLabelValueForKey(m, "pod")
					namespace, _ := metrics.GetLabelValueForKey(m, "namespace")
					if name == pod.Name && namespace == pod.Namespace {
						model, err := metrics.GetLabelValueForKey(m, "model")
						require.NoError(t, err)
						require.Equal(t, tt.wantModel, model)
						require.NotNil(t, m.Gauge)
						require.Equal(t, want, m.Gauge.GetValue())
						return
					}
				}
				t.Fatalf("missing %s for pod %s", metrics.RealtimeNumRequestsRunning, podKey)
			}

			pod.runningRequests = 1
			store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{
				entries: map[string][]remoteRunningEntry{podKey: {{count: 2, readAt: time.Now()}}},
			})
			store.syncRunningRequestsGlobally(pod)
			checkGauge(3)

			pod.runningRequests = 0
			store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{})
			store.syncRunningRequestsGlobally(pod)
			checkGauge(0)
		})
	}
}

func TestSyncRunningRequestsGloballyExportsGaugeRemoteReader(t *testing.T) {
	tests := []struct {
		name   string
		seen   bool
		remote int64
		stale  bool
		local  int32
		want   []float64
	}{
		{name: "unread", local: 2},
		{name: "empty-first-read", seen: true, local: 2, want: []float64{2}},
		{name: "fresh", seen: true, remote: 4, local: 2, want: []float64{6}},
		{name: "stale", seen: true, remote: 4, stale: true, local: 2, want: []float64{2}},
		{name: "zero", seen: true, want: []float64{0}},
	}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &Store{snapshotClock: func() time.Time { return now }}
			pod := runningRequestsTestPod("inference", "running-reader-gauge-test-"+tt.name)
			const modelName = "reader-model"
			pod.Labels = map[string]string{constants.ModelLabelName: modelName}
			pod.runningRequests = tt.local
			podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
			store.metaPods.Store(podKey, pod)
			handler := runningRequestsMetricsHandler(t, store)

			// The reader must take precedence over a conflicting snapshot, even
			// while the pod is unread and the gauge must not be emitted.
			store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{
				entries: map[string][]remoteRunningEntry{podKey: {{count: 99, readAt: now}}},
			})
			reader := newRemoteRunningReader(store, time.Second)
			store.remoteRunningReader.Store(reader)
			if tt.seen {
				readAt := now
				if tt.stale {
					readAt = now.Add(-reader.maxAge - time.Nanosecond)
				}
				reader.publish(podKey, tt.remote, readAt)
			}
			store.syncRunningRequestsGlobally(pod)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			require.Equal(t, http.StatusOK, w.Code)
			var parser expfmt.TextParser
			families, err := parser.TextToMetricFamilies(w.Body)
			require.NoError(t, err)
			var values []float64
			if family := families[metrics.RealtimeNumRequestsRunning]; family != nil {
				for _, m := range family.GetMetric() {
					name, _ := metrics.GetLabelValueForKey(m, "pod")
					namespace, _ := metrics.GetLabelValueForKey(m, "namespace")
					if name != pod.Name || namespace != pod.Namespace {
						continue
					}
					model, err := metrics.GetLabelValueForKey(m, "model")
					require.NoError(t, err)
					require.Equal(t, modelName, model)
					require.NotNil(t, m.Gauge)
					values = append(values, m.Gauge.GetValue())
				}
			}
			require.Equal(t, tt.want, values, "unexpected %s samples for pod %s", metrics.RealtimeNumRequestsRunning, podKey)
		})
	}
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
		podMetricsJobs: make(chan podMetricsJob, 2),
	}

	for i := range 5 {
		name := fmt.Sprintf("pod-%d", i)
		pod := newReadyMetricsPod(name, "uid-"+name)
		pod.Status.PodIP = fmt.Sprintf("10.0.0.%d", i+1)
		store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	}

	done := make(chan struct{})
	go func() {
		store.updatePodMetrics()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("updatePodMetrics blocked while pod metrics queue was full")
	}

	require.Equal(t, 2, len(store.podMetricsJobs))
	require.Equal(t, 3, countCounterCalls(counters, metrics.PodMetricsEnqueueDroppedTotal, "reason", metrics.PodMetricsDropReasonQueueFull))
}

func TestPodMetricsConfigDefaultsAndOverrides(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", "")
		t.Setenv("AIBRIX_POD_METRICS_WORKER_COUNT", "")
		t.Setenv("AIBRIX_POD_METRICS_JOB_QUEUE_SIZE", "")
		t.Setenv("AIBRIX_POD_METRICS_FETCH_TIMEOUT_MS", "")

		require.Equal(t, time.Second, loadPodMetricRefreshInterval())
		workerCount := loadPodMetricsWorkerCount()
		require.Equal(t, 10, workerCount)
		require.Equal(t, 100, loadPodMetricsJobQueueSize(workerCount))
		require.Equal(t, 5*time.Second, loadPodMetricsFetchTimeout())
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", "2500")
		t.Setenv("AIBRIX_POD_METRICS_WORKER_COUNT", "20")
		t.Setenv("AIBRIX_POD_METRICS_JOB_QUEUE_SIZE", "55")
		t.Setenv("AIBRIX_POD_METRICS_FETCH_TIMEOUT_MS", "1500")

		require.Equal(t, 2500*time.Millisecond, loadPodMetricRefreshInterval())
		workerCount := loadPodMetricsWorkerCount()
		require.Equal(t, 20, workerCount)
		require.Equal(t, 55, loadPodMetricsJobQueueSize(workerCount))
		require.Equal(t, 1500*time.Millisecond, loadPodMetricsFetchTimeout())
	})

	t.Run("invalid values", func(t *testing.T) {
		t.Setenv("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", "0")
		t.Setenv("AIBRIX_POD_METRICS_WORKER_COUNT", "-1")
		t.Setenv("AIBRIX_POD_METRICS_JOB_QUEUE_SIZE", "bad")
		t.Setenv("AIBRIX_POD_METRICS_FETCH_TIMEOUT_MS", "0")

		require.Equal(t, time.Second, loadPodMetricRefreshInterval())
		workerCount := loadPodMetricsWorkerCount()
		require.Equal(t, 10, workerCount)
		require.Equal(t, 100, loadPodMetricsJobQueueSize(workerCount))
		require.Equal(t, 5*time.Second, loadPodMetricsFetchTimeout())
	})
}

func TestUpdatePodMetricsDedupesQueuedPods(t *testing.T) {
	var counters []counterCall
	restore := captureCounterCalls(&counters)
	defer restore()

	store := &Store{
		podMetricsJobs: make(chan podMetricsJob, 10),
	}
	pod := newReadyMetricsPod("pod-0", "uid-0")
	store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)

	store.updatePodMetrics()
	store.updatePodMetrics()

	require.Equal(t, 1, len(store.podMetricsJobs))
	require.Equal(t, 1, store.podMetricsScheduling.Len())
	require.Equal(t, 1, countCounterCalls(counters, metrics.PodMetricsEnqueueDroppedTotal, "reason", metrics.PodMetricsDropReasonAlreadyQueued))
}

func TestPodMetricsSchedulingUIDChangeClearsState(t *testing.T) {
	store := &Store{
		podMetricsJobs: make(chan podMetricsJob, 10),
	}
	pod := newReadyMetricsPod("pod-0", "uid-0")
	recreated := newReadyMetricsPod("pod-0", "uid-1")
	store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)

	require.True(t, store.tryMarkPodMetricsQueued(pod))
	require.True(t, store.tryMarkPodMetricsQueued(recreated))
	require.Equal(t, 1, store.podMetricsScheduling.Len())
}

func TestPodMetricsSchedulingCompletesAndAllowsRequeue(t *testing.T) {
	store := &Store{
		podMetricsJobs: make(chan podMetricsJob, 10),
	}
	pod := newReadyMetricsPod("pod-0", "uid-0")

	require.True(t, store.tryMarkPodMetricsQueued(pod))
	require.False(t, store.tryMarkPodMetricsQueued(pod))

	store.markPodMetricsInFlight(pod)
	require.False(t, store.tryMarkPodMetricsQueued(pod))

	store.finishPodMetricsScheduling(pod)
	require.True(t, store.tryMarkPodMetricsQueued(pod))
}

func TestPodMetricsBackoffTransitions(t *testing.T) {
	store := &Store{}
	pod := newReadyMetricsPod("pod-0", "uid-0")
	store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
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
	store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
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
	store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
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
	store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
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
		podMetricsJobs: make(chan podMetricsJob, 2),
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

func TestPodMetricsFetchSuccessDoesNotClearRecreatedPodBackoff(t *testing.T) {
	store := &Store{}
	stalePod := newReadyMetricsPod("pod-0", "uid-0")
	recreatedPod := newReadyMetricsPod("pod-0", "uid-1")
	now := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	store.metaPods.Store(utils.GeneratePodKey(recreatedPod.Namespace, recreatedPod.Name), recreatedPod)
	store.podMetricsBackoff.Store(podMetricsBackoffKey(recreatedPod), &podMetricsBackoffState{
		uid:         string(recreatedPod.UID),
		failures:    1,
		nextFetchAt: now.Add(time.Second),
	})

	store.recordPodMetricsFetchSuccess(stalePod)

	state, ok := store.podMetricsBackoff.Load(podMetricsBackoffKey(recreatedPod))
	require.True(t, ok)
	require.Equal(t, string(recreatedPod.UID), state.uid)
}

func TestPodMetricsFailureDoesNotRecordBackoffForDeletedPod(t *testing.T) {
	store := &Store{}
	pod := newReadyMetricsPod("pod-0", "uid-0")

	store.recordPodMetricsFetchFailure(pod, time.Now())

	require.Equal(t, 0, store.podMetricsBackoff.Len())
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

func TestDeletePodClearsPodMetricsScheduling(t *testing.T) {
	store := &Store{}
	pod := newReadyMetricsPod("pod-0", "uid-0")
	store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	require.True(t, store.tryMarkPodMetricsQueued(pod))
	require.Equal(t, 1, store.podMetricsScheduling.Len())

	store.deletePod(pod.Pod)

	require.Equal(t, 0, store.podMetricsScheduling.Len())
}

func TestUpdatePodClearsPodMetricsStateWhenModelInfoIsRemoved(t *testing.T) {
	store := &Store{}
	oldPod := newReadyMetricsPod("pod-0", "uid-0")
	newPod := oldPod.DeepCopy()
	newPod.Labels = map[string]string{}
	metaPod := &Pod{Pod: oldPod.Pod, Models: utils.NewRegistry[string]()}
	metaPod.Models.Store("model", "model")
	store.metaPods.Store(utils.GeneratePodKey(oldPod.Namespace, oldPod.Name), metaPod)
	store.recordPodMetricsFetchFailure(metaPod, time.Now())
	require.True(t, store.tryMarkPodMetricsQueued(metaPod))

	store.updatePod(oldPod.Pod, newPod)

	require.Equal(t, 0, store.podMetricsBackoff.Len())
	require.Equal(t, 0, store.podMetricsScheduling.Len())
}

func TestUpdatePodPreservesPodMetricsStateWhenPodRemainsTracked(t *testing.T) {
	store := &Store{}
	oldPod := newReadyMetricsPod("pod-0", "uid-0")
	newPod := oldPod.DeepCopy()
	newPod.Status.PodIP = "10.0.0.2"
	metaPod := &Pod{Pod: oldPod.Pod, Models: utils.NewRegistry[string]()}
	metaPod.Models.Store("model", "model")
	store.metaPods.Store(utils.GeneratePodKey(oldPod.Namespace, oldPod.Name), metaPod)
	store.recordPodMetricsFetchFailure(metaPod, time.Now())
	require.True(t, store.tryMarkPodMetricsQueued(metaPod))

	store.updatePod(oldPod.Pod, newPod)

	require.Equal(t, 1, store.podMetricsBackoff.Len())
	require.Equal(t, 1, store.podMetricsScheduling.Len())
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
				podMetricsJobs: make(chan podMetricsJob, scenario.queueSize),
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
				store.podMetricsJobs = make(chan podMetricsJob, scenario.queueSize)
				store.updatePodMetrics()
				enqueuedTotal += len(store.podMetricsJobs)
				for len(store.podMetricsJobs) > 0 {
					store.finishPodMetricsScheduling((<-store.podMetricsJobs).Pod)
				}
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

// seedCompletedOutputTokensHistory plants a rate-window baseline so calculateRate1m has a
// snapshot far enough in the past (it discards windows shorter than 10s) without sleeping.
func seedCompletedOutputTokensHistory(t *testing.T, pod *Pod, baseline float64, age time.Duration) {
	t.Helper()
	key := rateHistoryKey(pod, "", "completed_output_tokens")
	rateCalculator.mu.Lock()
	rateCalculator.history[key] = []MetricSnapshot{{Value: baseline, Timestamp: time.Now().Add(-age)}}
	rateCalculator.mu.Unlock()
	t.Cleanup(func() {
		rateCalculator.mu.Lock()
		delete(rateCalculator.history, key)
		rateCalculator.mu.Unlock()
	})
}

func TestUpdateRealtimeOutputTokenRateEWMA(t *testing.T) {
	c := &Store{}
	pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ewma-pod", Namespace: "default"}}}
	seedCompletedOutputTokensHistory(t, pod, 0, 30*time.Second)

	// 3000 tokens over the 30s window = 100 tokens/s. The first sample seeds the EWMA as is.
	atomic.StoreInt64(&pod.completedOutputTokens, 3000)
	c.updateRealtimeOutputTokenRateEWMA(pod)
	v, ok := pod.Metrics.Load(metrics.RealtimeOutputTokenRateEWMA)
	require.True(t, ok)
	require.InDelta(t, 100, v.Value.GetSimpleValue(), 1)

	// A higher window rate (~200 tokens/s) moves the estimate only part of the way toward it.
	atomic.StoreInt64(&pod.completedOutputTokens, 6000)
	c.updateRealtimeOutputTokenRateEWMA(pod)
	v, ok = pod.Metrics.Load(metrics.RealtimeOutputTokenRateEWMA)
	require.True(t, ok)
	alpha := 1 - math.Exp(-podMetricRefreshInterval.Seconds()/outputTokenRateEWMATau.Seconds())
	want := alpha*200 + (1-alpha)*100
	require.InDelta(t, want, v.Value.GetSimpleValue(), 2)
	require.Greater(t, v.Value.GetSimpleValue(), 100.0)
	require.Less(t, v.Value.GetSimpleValue(), 200.0)
}

// A window with no completed output tokens says nothing about capacity, so the previous
// estimate must survive instead of decaying to zero.
func TestUpdateRealtimeOutputTokenRateEWMA_IdleKeepsPreviousEstimate(t *testing.T) {
	c := &Store{}
	pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "idle-ewma-pod", Namespace: "default"}}}
	seedCompletedOutputTokensHistory(t, pod, 500, 30*time.Second)
	atomic.StoreInt64(&pod.completedOutputTokens, 500) // no new tokens in the window
	require.NoError(t, c.updatePodRecord(pod, "", metrics.RealtimeOutputTokenRateEWMA, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: 123}))

	c.updateRealtimeOutputTokenRateEWMA(pod)

	v, ok := pod.Metrics.Load(metrics.RealtimeOutputTokenRateEWMA)
	require.True(t, ok)
	require.Equal(t, 123.0, v.Value.GetSimpleValue())
}

func TestUpdateRealtimeOutputTokenRateEWMA_NoHistoryStoresNothing(t *testing.T) {
	c := &Store{}
	pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cold-ewma-pod", Namespace: "default"}}}
	t.Cleanup(func() {
		rateCalculator.mu.Lock()
		delete(rateCalculator.history, rateHistoryKey(pod, "", "completed_output_tokens"))
		rateCalculator.mu.Unlock()
	})
	atomic.StoreInt64(&pod.completedOutputTokens, 1000)

	c.updateRealtimeOutputTokenRateEWMA(pod)

	_, ok := pod.Metrics.Load(metrics.RealtimeOutputTokenRateEWMA)
	require.False(t, ok, "a single sample has no window, so no capacity estimate yet")
}

// seedOutputRateHistory gives the pod's completion-counter history one baseline snapshot age ago,
// so the next update has a window to take deltas over.
func seedOutputRateHistory(t *testing.T, pod *Pod, tokens, busySeconds, requests, finished float64, age time.Duration) {
	t.Helper()
	setOutputRateHistory(t, pod, outputRateSnapshot{
		counters: outputRateCounters{tokens: tokens, busySeconds: busySeconds, requests: requests, finished: finished},
		at:       time.Now().Add(-age),
	})
}

func setOutputRateHistory(t *testing.T, pod *Pod, history ...outputRateSnapshot) {
	t.Helper()
	key := utils.GeneratePodKey(pod.Namespace, pod.Name)
	rateCalculator.mu.Lock()
	if rateCalculator.outputRate == nil {
		rateCalculator.outputRate = make(map[string][]outputRateSnapshot)
	}
	rateCalculator.outputRate[key] = history
	rateCalculator.mu.Unlock()
	t.Cleanup(func() { rateCalculator.PurgeEntriesForPod(pod.Namespace, pod.Name) })
}

func outputRateHistoryLen(pod *Pod) int {
	rateCalculator.mu.Lock()
	defer rateCalculator.mu.Unlock()
	return len(rateCalculator.outputRate[utils.GeneratePodKey(pod.Namespace, pod.Name)])
}

// setOutputCounters sets the pod's completion counters: output tokens, the time those requests
// spent on the pod, how many requests had output tokens, and how many requests finished at all.
func setOutputCounters(pod *Pod, tokens int64, busy time.Duration, requests, finished int64) {
	atomic.StoreInt64(&pod.completedOutputTokens, tokens)
	atomic.StoreInt64(&pod.completedOutputBusyNanos, int64(busy))
	atomic.StoreInt64(&pod.completedOutputRequests, requests)
	atomic.StoreInt64(&pod.completedRequests, finished)
}

// storeWithMinSamples is a store whose per-request rate needs n requests in a window.
func storeWithMinSamples(n int) *Store {
	return &Store{outputRateMinSamples: &n}
}

func podMetric(t *testing.T, pod *Pod, name string) (float64, bool) {
	t.Helper()
	v, ok := pod.Metrics.Load(name)
	if !ok {
		return 0, false
	}
	return v.Value.GetSimpleValue(), true
}

func perRequestRate(t *testing.T, pod *Pod) float64 {
	t.Helper()
	v, ok := podMetric(t, pod, metrics.RealtimeOutputTokensPerRequestSecondEWMA)
	require.True(t, ok)
	return v
}

// TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_IndependentOfTrafficShare is the #3450
// property: two identical pods, one of which got 3x the traffic, measure the same per-request rate,
// while RealtimeOutputTokenRateEWMA reads 3x higher on the busier one.
func TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_IndependentOfTrafficShare(t *testing.T) {
	c := &Store{}
	busy := namespacedTestPod("default", "share-busy")
	idle := namespacedTestPod("default", "share-idle")
	for _, pod := range []*Pod{busy, idle} {
		seedOutputRateHistory(t, pod, 0, 0, 0, 0, 30*time.Second)
		seedCompletedOutputTokensHistory(t, pod, 0, 30*time.Second)
	}
	// Over 30 s each request decodes at 46 tokens/s. busy carried ~5 requests at a time, idle ~1.7.
	setOutputCounters(busy, 46*150, 150*time.Second, 216, 216)
	setOutputCounters(idle, 46*50, 50*time.Second, 72, 72)

	for _, pod := range []*Pod{busy, idle} {
		c.updateRealtimeOutputTokensPerRequestSecondEWMA(pod)
		c.updateRealtimeOutputTokenRateEWMA(pod)
	}

	require.InDelta(t, 46, perRequestRate(t, busy), 0.5)
	require.InDelta(t, 46, perRequestRate(t, idle), 0.5)
	total := func(pod *Pod) float64 {
		v, ok := podMetric(t, pod, metrics.RealtimeOutputTokenRateEWMA)
		require.True(t, ok)
		return v
	}
	require.InDelta(t, 3, total(busy)/total(idle), 0.05, "the total-rate signal follows the traffic share")
}

// A saturated pod's requests queue, so each spends longer on the pod for the same tokens and the
// estimate falls. A faster pod's requests finish sooner and the estimate rises.
func TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_FallsWithQueueingRisesWithSpeed(t *testing.T) {
	c := &Store{}
	slow := namespacedTestPod("default", "rate-saturated")
	fast := namespacedTestPod("default", "rate-fast")
	for _, pod := range []*Pod{slow, fast} {
		seedOutputRateHistory(t, pod, 0, 0, 0, 0, 30*time.Second)
	}
	setOutputCounters(slow, 3000, 150*time.Second, 94, 94) // 20 tokens per request-second
	setOutputCounters(fast, 3000, 30*time.Second, 94, 94)  // 100

	c.updateRealtimeOutputTokensPerRequestSecondEWMA(slow)
	c.updateRealtimeOutputTokensPerRequestSecondEWMA(fast)

	require.InDelta(t, 20, perRequestRate(t, slow), 0.5)
	require.InDelta(t, 100, perRequestRate(t, fast), 1)
}

// TestOutputRateDeltas1m: every delta is taken against the same snapshot, the one closest to a
// minute ago and not the oldest; a call within the snapshot interval adds no snapshot; a counter
// that went backwards resets them all.
func TestOutputRateDeltas1m(t *testing.T) {
	c := &Store{}
	pod := namespacedTestPod("default", "deltas")
	now := time.Now()
	setOutputRateHistory(t, pod,
		outputRateSnapshot{counters: outputRateCounters{tokens: 100, busySeconds: 4, requests: 3, finished: 5}, at: now.Add(-150 * time.Second)},
		outputRateSnapshot{counters: outputRateCounters{tokens: 300, busySeconds: 9, requests: 8, finished: 12}, at: now.Add(-58 * time.Second)},
	)

	cur := outputRateCounters{tokens: 760, busySeconds: 19, requests: 18, finished: 27}
	for i := 0; i < 2; i++ { // the second call, in the same tick, reads the same baseline
		d, ok := c.outputRateDeltas1m(pod, cur)
		require.True(t, ok)
		require.Equal(t, outputRateCounters{tokens: 460, busySeconds: 10, requests: 10, finished: 15}, d)
		require.Equal(t, 3, outputRateHistoryLen(pod))
	}

	reset := outputRateCounters{tokens: 50, busySeconds: 2, requests: 20, finished: 30} // tokens went backwards
	d, ok := c.outputRateDeltas1m(pod, reset)
	require.True(t, ok)
	require.Equal(t, reset, d)
}

// TestOutputRateBaseMatchesBaseSnapshot1m: the completion-counter history picks its baseline and
// prunes with its own helpers; they must agree with the single-series ones on any history.
func TestOutputRateBaseMatchesBaseSnapshot1m(t *testing.T) {
	rng := rand.New(rand.NewSource(3450))
	now := time.Now()
	for trial := 0; trial < 500; trial++ {
		n := 2 + rng.Intn(60)
		single := make([]MetricSnapshot, n)
		group := make([]outputRateSnapshot, n)
		age := time.Duration(rng.Int63n(int64(400 * time.Second)))
		for i := n - 1; i >= 0; i-- { // newest last
			at := now.Add(-age)
			single[i] = MetricSnapshot{Value: float64(i), Timestamp: at}
			group[i] = outputRateSnapshot{counters: outputRateCounters{tokens: float64(i)}, at: at}
			age += time.Duration(rng.Int63n(int64(9*time.Second))) + time.Millisecond
		}

		single = cleanupOldSnapshots(single, now, window1mHistoryMaxAge, window1mHistoryMaxCount)
		group = pruneOutputRateHistory(group, now)
		require.Len(t, group, len(single), "trial %d: pruned length", trial)
		for i := range single {
			require.Equal(t, single[i].Value, group[i].counters.tokens, "trial %d: kept snapshot %d", trial, i)
		}
		if len(single) < 2 {
			continue
		}
		require.Equal(t, baseSnapshot1m(single, now).Value, group[outputRateBaseIndex1m(group, now)].counters.tokens, "trial %d: baseline", trial)
	}
}

// No result before there are two snapshots, or while the baseline is younger than the minimum.
func TestOutputRateDeltas1mNeedsHistory(t *testing.T) {
	c := &Store{}
	pod := namespacedTestPod("default", "deltas-young")
	t.Cleanup(func() { rateCalculator.PurgeEntriesForPod(pod.Namespace, pod.Name) })

	_, ok := c.outputRateDeltas1m(pod, outputRateCounters{tokens: 32, busySeconds: 1, requests: 1, finished: 1})
	require.False(t, ok, "first snapshot")
	require.Equal(t, 1, outputRateHistoryLen(pod))

	seedOutputRateHistory(t, pod, 0, 0, 0, 0, 6*time.Second)
	_, ok = c.outputRateDeltas1m(pod, outputRateCounters{tokens: 32, busySeconds: 1, requests: 1, finished: 1})
	require.False(t, ok, "baseline 6 s old")
}

// Deleting a pod drops its completion-counter history with its other rate histories.
func TestPurgeEntriesForPodDropsOutputRateHistory(t *testing.T) {
	pod := namespacedTestPod("default", "purge")
	other := namespacedTestPod("other", "purge")
	seedOutputRateHistory(t, pod, 1, 1, 1, 1, 30*time.Second)
	seedOutputRateHistory(t, other, 1, 1, 1, 1, 30*time.Second)

	rateCalculator.PurgeEntriesForPod(pod.Namespace, pod.Name)

	require.Equal(t, 0, outputRateHistoryLen(pod))
	require.Equal(t, 1, outputRateHistoryLen(other), "a same-named pod in another namespace keeps its history")
}

// TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_WindowNeedsMinSamples: a window updates the
// estimate only if enough requests with output tokens completed in it.
func TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_WindowNeedsMinSamples(t *testing.T) {
	c := storeWithMinSamples(8)
	pod := namespacedTestPod("default", "rate-min-samples")
	seedOutputRateHistory(t, pod, 0, 0, 0, 0, 30*time.Second)

	// 7 requests in the window: 7 x 32 tokens over 7 x 0.8 s on the pod.
	setOutputCounters(pod, 7*32, 7*800*time.Millisecond, 7, 7)
	c.updateRealtimeOutputTokensPerRequestSecondEWMA(pod)
	_, ok := podMetric(t, pod, metrics.RealtimeOutputTokensPerRequestSecondEWMA)
	require.False(t, ok, "7 of 8 samples in the window: no estimate")

	setOutputCounters(pod, 8*32, 8*800*time.Millisecond, 8, 8)
	c.updateRealtimeOutputTokensPerRequestSecondEWMA(pod)
	require.InDelta(t, 40, perRequestRate(t, pod), 0.5)
}

// TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_LifetimeSamplesDoNotCount: the threshold
// counts the window, not the pod's lifetime. A pod that completed many requests long ago and
// one request in this window keeps its previous estimate; that one request, which here is far
// off the pod's rate, does not move it.
func TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_LifetimeSamplesDoNotCount(t *testing.T) {
	c := storeWithMinSamples(8)
	pod := namespacedTestPod("default", "rate-after-idle")
	seedOutputRateHistory(t, pod, 16000, 400, 500, 500, 30*time.Second) // 500 requests before the window
	require.NoError(t, c.updatePodRecord(pod, "", metrics.RealtimeOutputTokensPerRequestSecondEWMA, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: 40}))

	setOutputCounters(pod, 16000+32, 400*time.Second+8*time.Second, 501, 501) // one request at 4 tokens/s
	c.updateRealtimeOutputTokensPerRequestSecondEWMA(pod)

	require.Equal(t, 40.0, perRequestRate(t, pod))
}

// With the threshold at 0 a window with one completed request updates the estimate.
func TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_MinSamplesZero(t *testing.T) {
	c := storeWithMinSamples(0)
	pod := namespacedTestPod("default", "rate-min-samples-off")
	seedOutputRateHistory(t, pod, 0, 0, 0, 0, 30*time.Second)
	setOutputCounters(pod, 32, 800*time.Millisecond, 1, 1)

	c.updateRealtimeOutputTokensPerRequestSecondEWMA(pod)

	require.InDelta(t, 40, perRequestRate(t, pod), 0.5)
}

// The default is pinned so a change to it is deliberate, and a store without an override uses
// the process default.
func TestOutputRateMinSamplesDefault(t *testing.T) {
	require.Equal(t, 8, outputRateMinSamplesDefault)
	require.Equal(t, outputRateMinSamplesFromEnv, (&Store{}).minOutputRateSamples())
	require.Equal(t, 3, storeWithMinSamples(3).minOutputRateSamples())
}

// TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_IdlePodKeepsValuesAndTheirAge pins what an
// idle pod reports: nothing finished in the window, so both values stay as they were, with the
// time of their last update. The fraction here is from an incident ten minutes ago. A reader
// that does not check the record's age takes it for the last minute.
func TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_IdlePodKeepsValuesAndTheirAge(t *testing.T) {
	c := &Store{}
	pod := namespacedTestPod("default", "rate-idle")
	seedOutputRateHistory(t, pod, 500, 10, 15, 25, 30*time.Second)
	setOutputCounters(pod, 500, 10*time.Second, 15, 25) // nothing new
	then := time.Now().Add(-10 * time.Minute)
	require.NoError(t, c.updatePodRecordAt(pod, "", metrics.RealtimeOutputTokensPerRequestSecondEWMA, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: 42}, then))
	require.NoError(t, c.updatePodRecordAt(pod, "", metrics.RealtimeRequestsWithoutOutputTokensFraction1m, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: 0.4}, then))

	c.updateRealtimeOutputTokensPerRequestSecondEWMA(pod)

	for name, want := range map[string]float64{
		metrics.RealtimeOutputTokensPerRequestSecondEWMA:      42,
		metrics.RealtimeRequestsWithoutOutputTokensFraction1m: 0.4,
	} {
		record, ok := pod.Metrics.Load(name)
		require.True(t, ok, name)
		require.Equal(t, want, record.Value.GetSimpleValue(), name)
		require.True(t, record.Timestamp.Equal(then), "%s keeps the time of its last update", name)
	}
}

// TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_WeighsByElapsedTime: the gate makes updates
// irregular, so the EWMA weighs a new window by the time since the estimate was last updated,
// not by one refresh interval. After 30 s without an update (tau 10 s) the old estimate keeps
// 5% of the weight; one refresh interval after an update it keeps 90%.
func TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_WeighsByElapsedTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		want    float64
	}{
		{"30 s since the last update", 30 * time.Second, 40 + 60*(1-math.Exp(-3))},
		{"one refresh interval", podMetricRefreshInterval, 40 + 60*(1-math.Exp(-podMetricRefreshInterval.Seconds()/10))},
		{"updated just now: never less than one interval", 0, 40 + 60*(1-math.Exp(-podMetricRefreshInterval.Seconds()/10))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := storeWithMinSamples(8)
			pod := namespacedTestPod("default", "rate-ewma")
			seedOutputRateHistory(t, pod, 0, 0, 0, 0, 30*time.Second)
			require.NoError(t, c.updatePodRecordAt(pod, "", metrics.RealtimeOutputTokensPerRequestSecondEWMA, metrics.PodMetricScope,
				&metrics.SimpleMetricValue{Value: 40}, time.Now().Add(-tc.elapsed)))
			setOutputCounters(pod, 3200, 32*time.Second, 100, 100) // this window: 100 tokens per request-second

			c.updateRealtimeOutputTokensPerRequestSecondEWMA(pod)

			require.InDelta(t, tc.want, perRequestRate(t, pod), 0.05)
		})
	}
}

// TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_AbortedRequestsAreNotInTheRate documents a
// known bias and the metric that makes it visible. 100 requests finish in the window. 60 complete
// with 32 tokens after 0.8 s. 40 are abandoned after 5 s without output tokens: they are the
// slowest requests on the pod, and they are in neither the tokens nor the time. The rate is
// that of the 60 survivors, so the pod reads as fast as an unloaded one.
func TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_AbortedRequestsAreNotInTheRate(t *testing.T) {
	c := &Store{}
	pod := namespacedTestPod("default", "rate-aborts")
	seedOutputRateHistory(t, pod, 0, 0, 0, 0, 30*time.Second)
	setOutputCounters(pod, 60*32, 60*800*time.Millisecond, 60, 100)

	c.updateRealtimeOutputTokensPerRequestSecondEWMA(pod)

	require.InDelta(t, 40, perRequestRate(t, pod), 0.5, "the 40 abandoned requests do not lower the rate")
	without, ok := podMetric(t, pod, metrics.RealtimeRequestsWithoutOutputTokensFraction1m)
	require.True(t, ok)
	require.InDelta(t, 0.4, without, 1e-9, "but their share is recorded")
}

// TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_DependsOnRequestMix pins the other known
// bias: the time is the whole request, so two pods with the same decode speed read differently
// when they serve different mixes. Both decode at 100 tokens/s after 0.3 s before the first
// token. One serves 32-token answers, the other 512-token ones, and the second reads 1.8x faster.
func TestUpdateRealtimeOutputTokensPerRequestSecondEWMA_DependsOnRequestMix(t *testing.T) {
	c := &Store{}
	short := namespacedTestPod("default", "mix-short-answers")
	long := namespacedTestPod("default", "mix-long-answers")
	for _, pod := range []*Pod{short, long} {
		seedOutputRateHistory(t, pod, 0, 0, 0, 0, 30*time.Second)
	}
	setOutputCounters(short, 40*32, 40*620*time.Millisecond, 40, 40)  // 0.3 s + 32/100 s each
	setOutputCounters(long, 10*512, 10*5420*time.Millisecond, 10, 10) // 0.3 s + 512/100 s each

	c.updateRealtimeOutputTokensPerRequestSecondEWMA(short)
	c.updateRealtimeOutputTokensPerRequestSecondEWMA(long)

	require.InDelta(t, 51.6, perRequestRate(t, short), 0.1)
	require.InDelta(t, 94.5, perRequestRate(t, long), 0.1)
}

// TestReadOutputRateCountersIsConsistent: donePodStats adds a request's tokens, time and sample
// under the pod's stats lock, and the metrics tick reads them under the same lock, so it never
// sees one without the others. Every request here adds exactly 20 tokens and one sample.
func TestReadOutputRateCountersIsConsistent(t *testing.T) {
	const modelName, namespace, podName = "test-model", "default", "decode-0"
	cache := NewForTest()
	pod := requestTrackerTestPod(podName, namespace, modelName, "10.0.0.1")
	cache.addPod(pod)
	metaPod, ok := cache.metaPods.Load(namespace + "/" + podName)
	require.True(t, ok)

	const writers, perWriter = 4, 300
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				requestID := fmt.Sprintf("w%d-r%d", w, i)
				ctx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
				ctx.SetTargetPod(pod)
				term := cache.AddRequestCount(ctx, requestID, modelName)
				cache.DoneRequestTrace(ctx, requestID, modelName, 10, 20, term)
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	reads := 0
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		counters := cache.readOutputRateCounters(metaPod)
		require.Equal(t, 20*counters.requests, counters.tokens, "a read saw tokens without their sample, or the reverse")
		require.Equal(t, counters.requests, counters.finished)
		reads++
	}
	counters := cache.readOutputRateCounters(metaPod)
	require.Equal(t, float64(writers*perWriter), counters.requests)
	require.Equal(t, float64(20*writers*perWriter), counters.tokens)
	require.Positive(t, counters.busySeconds)
	require.Positive(t, reads)
}

const benchPods = 5000

// seedBenchHistories gives each of benchPods pods a full completion-counter history and a full
// history for each of names (the single-series rate histories a pod already has), and returns
// the pods.
func seedBenchHistories(b *testing.B, names ...string) []*Pod {
	b.Helper()
	fleet := make([]*Pod, benchPods)
	start := time.Now().Add(-window1mHistoryMaxAge + 10*time.Second)
	rateCalculator.mu.Lock()
	if rateCalculator.outputRate == nil {
		rateCalculator.outputRate = make(map[string][]outputRateSnapshot)
	}
	for i := range fleet {
		fleet[i] = namespacedTestPod("bench", fmt.Sprintf("pod-%d", i))
		group := make([]outputRateSnapshot, 0, window1mHistoryMaxCount)
		for k := 0; k < window1mHistoryMaxCount-2; k++ {
			at := start.Add(time.Duration(k) * window1mSnapshotInterval)
			group = append(group, outputRateSnapshot{counters: outputRateCounters{tokens: float64(k * 100), busySeconds: float64(k), requests: float64(k * 3), finished: float64(k * 3)}, at: at})
		}
		rateCalculator.outputRate[utils.GeneratePodKey(fleet[i].Namespace, fleet[i].Name)] = group
		for _, name := range names {
			history := make([]MetricSnapshot, 0, window1mHistoryMaxCount)
			for k := 0; k < window1mHistoryMaxCount-2; k++ {
				history = append(history, MetricSnapshot{Value: float64(k * 100), Timestamp: start.Add(time.Duration(k) * window1mSnapshotInterval)})
			}
			rateCalculator.history[rateHistoryKey(fleet[i], "", name)] = history
		}
	}
	rateCalculator.mu.Unlock()
	b.Cleanup(func() {
		for _, pod := range fleet {
			rateCalculator.PurgeEntriesForPod(pod.Namespace, pod.Name)
		}
	})
	return fleet
}

// BenchmarkOutputRateDeltas1m is outputRateDeltas1m for one pod of a 5,000-pod fleet with full
// histories, under the process-wide rateCalculator lock. One metrics tick calls it once per pod.
// BenchmarkCalculateRate1m is the existing single-series helper on the same fleet.
func BenchmarkOutputRateDeltas1m(b *testing.B) {
	c := &Store{}
	fleet := seedBenchHistories(b)
	cur := outputRateCounters{tokens: 5000, busySeconds: 120, requests: 150, finished: 160}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.outputRateDeltas1m(fleet[i%benchPods], cur)
	}
}

func BenchmarkCalculateRate1m(b *testing.B) {
	c := &Store{}
	fleet := seedBenchHistories(b, "bench_rate")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.calculateRate1m(fleet[i%benchPods], "bench_rate", 5000)
	}
}

// BenchmarkPurgeEntriesForPod is one pod deletion on a 5,000-pod fleet: a scan of every
// single-series history key (two per pod today: completed requests and completed output tokens)
// under the process-wide lock, plus one delete of the pod's completion-counter history, which
// adds no keys to that scan. A rolling restart of N pods does this N times.
func BenchmarkPurgeEntriesForPod(b *testing.B) {
	fleet := seedBenchHistories(b, "completed_requests", "completed_output_tokens")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pod := fleet[i%benchPods]
		rateCalculator.PurgeEntriesForPod(pod.Namespace, pod.Name)

		// Put the pod's entries back, off the clock, so every iteration scans a full map.
		b.StopTimer()
		rateCalculator.mu.Lock()
		rateCalculator.history[rateHistoryKey(pod, "", "completed_requests")] = []MetricSnapshot{{}}
		rateCalculator.history[rateHistoryKey(pod, "", "completed_output_tokens")] = []MetricSnapshot{{}}
		rateCalculator.outputRate[utils.GeneratePodKey(pod.Namespace, pod.Name)] = []outputRateSnapshot{{}}
		rateCalculator.mu.Unlock()
		b.StartTimer()
	}
}

func namespacedTestPod(namespace, name string) *Pod {
	return &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}}
}

// purgeRateHistory drops the rate history of the given pods when the test ends.
func purgeRateHistory(t *testing.T, pods ...*Pod) {
	t.Helper()
	t.Cleanup(func() {
		for _, pod := range pods {
			rateCalculator.PurgeEntriesForPod(pod.Namespace, pod.Name)
		}
	})
}

// Same-named pods in different namespaces must keep separate rate histories. Sharing
// one would compute a pod's rate against the other pod's counter.
func TestCalculatePerSecondRate_SameNameInTwoNamespaces(t *testing.T) {
	c := &Store{}
	podA := namespacedTestPod("team-a", "decode-0")
	podB := namespacedTestPod("team-b", "decode-0")
	purgeRateHistory(t, podA, podB)

	require.Equal(t, -1.0, c.calculatePerSecondRate(podA, "m", "gen_tokens", 100000))
	// team-b has no history of its own yet, so it has no rate.
	require.Equal(t, -1.0, c.calculatePerSecondRate(podB, "m", "gen_tokens", 500))

	rateCalculator.mu.RLock()
	defer rateCalculator.mu.RUnlock()
	require.Equal(t, []float64{100000}, snapshotValues(rateCalculator.history[rateHistoryKey(podA, "m", "gen_tokens")]))
	require.Equal(t, []float64{500}, snapshotValues(rateCalculator.history[rateHistoryKey(podB, "m", "gen_tokens")]))
}

func TestCalculateRate1m_SameNameInTwoNamespaces(t *testing.T) {
	c := &Store{}
	podA := namespacedTestPod("team-a", "decode-0")
	podB := namespacedTestPod("team-b", "decode-0")
	purgeRateHistory(t, podA, podB)

	// Seed a 30s-old baseline for team-a only.
	seededAt := time.Now().Add(-30 * time.Second)
	rateCalculator.mu.Lock()
	rateCalculator.history[rateHistoryKey(podA, "", "completed_requests")] = []MetricSnapshot{{Value: 1000, Timestamp: seededAt}}
	rateCalculator.mu.Unlock()

	// team-b's much smaller counter must not be read as a counter reset of team-a's.
	require.Equal(t, -1.0, c.calculateRate1m(podB, "completed_requests", 5))
	// team-a's rate is still computed against its own baseline: 3000 over the time since
	// the seed. Bracket the elapsed time so a slow run cannot fail the check.
	before := time.Since(seededAt).Seconds()
	rate := c.calculateRate1m(podA, "completed_requests", 4000)
	after := time.Since(seededAt).Seconds()
	require.GreaterOrEqual(t, rate, 3000/after)
	require.LessOrEqual(t, rate, 3000/before)
}

func TestPurgeEntriesForPod_NamespaceScoped(t *testing.T) {
	c := &Store{}
	podA := namespacedTestPod("team-a", "decode-0")
	podB := namespacedTestPod("team-b", "decode-0")
	podA2 := namespacedTestPod("team-a", "decode-00") // name shares a prefix with podA
	purgeRateHistory(t, podA, podB, podA2)

	for _, pod := range []*Pod{podA, podB, podA2} {
		c.calculatePerSecondRate(pod, "m", "gen_tokens", 1)
		c.calculateRate1m(pod, "completed_requests", 1)
	}

	rateCalculator.PurgeEntriesForPod("team-a", "decode-0")

	rateCalculator.mu.RLock()
	defer rateCalculator.mu.RUnlock()
	require.NotContains(t, rateCalculator.history, rateHistoryKey(podA, "m", "gen_tokens"))
	require.NotContains(t, rateCalculator.history, rateHistoryKey(podA, "", "completed_requests"))
	for _, pod := range []*Pod{podB, podA2} {
		require.Contains(t, rateCalculator.history, rateHistoryKey(pod, "m", "gen_tokens"))
		require.Contains(t, rateCalculator.history, rateHistoryKey(pod, "", "completed_requests"))
	}
}

func snapshotValues(history []MetricSnapshot) []float64 {
	values := make([]float64, len(history))
	for i, s := range history {
		values[i] = s.Value
	}
	return values
}

func TestPDRole(t *testing.T) {
	pod := func(name string, labels map[string]string) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	cases := []struct {
		name string
		pod  *v1.Pod
		want string
	}{
		{"label decode, name without role (static discovery)", pod("qwen-127-0-0-1-2", map[string]string{pdRoleIdentifier: "decode"}), "decode"},
		{"label prefill, name without role", pod("qwen-127-0-0-1-1", map[string]string{pdRoleIdentifier: "prefill"}), "prefill"},
		{"label wins over the name", pod("prefill-cache-decode-0", map[string]string{pdRoleIdentifier: "decode"}), "decode"},
		{"no label: name fallback, decode", pod("rs-decode-abcde", nil), "decode"},
		{"no label: name fallback, prefill", pod("rs-prefill-abcde", nil), "prefill"},
		{"other label value: name fallback", pod("rs-decode-abcde", map[string]string{pdRoleIdentifier: "worker"}), "decode"},
		{"neither", pod("vllm-0", map[string]string{pdRoleIdentifier: "worker"}), ""},
		{"nil pod", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, pdRole(tc.pod))
		})
	}
}

// A decode pod from static discovery is named after its address, not its role. Its
// generation throughput must still be derived from the role-name label.
func TestUpdateThroughputToksPerS_UsesRoleLabel(t *testing.T) {
	c := &Store{}
	decode := newReadyMetricsPod("model-127-0-0-1-2", "uid-decode")
	decode.Labels[pdRoleIdentifier] = "decode"
	prefill := newReadyMetricsPod("model-127-0-0-1-1", "uid-prefill")
	prefill.Labels[pdRoleIdentifier] = "prefill"
	purgeRateHistory(t, decode, prefill)

	// Seed a baseline one second back instead of sleeping between two scrapes.
	seededAt := time.Now().Add(-time.Second)
	rateCalculator.mu.Lock()
	rateCalculator.history[rateHistoryKey(decode, "model", metrics.GenerationTokenTotal)] = []MetricSnapshot{{Value: 1000, Timestamp: seededAt}}
	rateCalculator.history[rateHistoryKey(prefill, "model", metrics.PromptTokenTotal)] = []MetricSnapshot{{Value: 1000, Timestamp: seededAt}}
	rateCalculator.mu.Unlock()

	c.updateThroughputToksPerS(decode, "model", metrics.GenerationTokenTotal, &metrics.SimpleMetricValue{Value: 1100})
	c.updateThroughputToksPerS(prefill, "model", metrics.PromptTokenTotal, &metrics.SimpleMetricValue{Value: 1100})

	gen, ok := decode.ModelMetrics.Load(c.getPodModelMetricName("model", metrics.AvgGenerationThroughputToksPerS))
	require.True(t, ok, "the decode pod must get a generation throughput")
	require.InDelta(t, 100.0, gen.Value.GetSimpleValue(), 5.0)
	prompt, ok := prefill.ModelMetrics.Load(c.getPodModelMetricName("model", metrics.AvgPromptThroughputToksPerS))
	require.True(t, ok, "the prefill pod must get a prompt throughput")
	require.InDelta(t, 100.0, prompt.Value.GetSimpleValue(), 5.0)

	// A role only gets the rate of its own counter.
	_, ok = decode.ModelMetrics.Load(c.getPodModelMetricName("model", metrics.AvgPromptThroughputToksPerS))
	require.False(t, ok)
}

func seedPodGaugeLifecycleMetrics(t *testing.T, store *Store, pod *Pod, model, sourceMetric, derivedMetric string) {
	t.Helper()
	extra := map[string]string{"model_name": model}
	rawValue := &metrics.SimpleMetricValue{Value: 3, Labels: extra}
	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: model}, pod.Pod, metrics.NumRequestsRunning, rawValue, extra)
	require.NoError(t, store.updatePodRecord(pod, model, metrics.NumRequestsRunning, metrics.PodModelMetricScope, rawValue))

	rateCalculator.mu.Lock()
	rateCalculator.history[rateHistoryKey(pod, model, sourceMetric)] = []MetricSnapshot{{Value: 1000, Timestamp: time.Now().Add(-time.Second)}}
	rateCalculator.mu.Unlock()
	sourceValue := &metrics.SimpleMetricValue{Value: 1100, Labels: extra}
	store.updateThroughputToksPerS(pod, model, sourceMetric, sourceValue)
	require.NoError(t, store.updatePodRecord(pod, model, sourceMetric, metrics.PodModelMetricScope, sourceValue))
	derived, ok := pod.ModelMetrics.Load(store.getPodModelMetricName(model, derivedMetric))
	require.True(t, ok)
	require.Equal(t, extra, derived.Value.GetLabelValues())

	histogramValue := &metrics.HistogramMetricValue{
		Sum: 1.2, Count: 2, Buckets: map[string]float64{"0.300000": 0, "0.800000": 2, "+Inf": 2}, Labels: extra,
	}
	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: model}, pod.Pod, metrics.TimeToFirstTokenSeconds, histogramValue, extra)
	require.NoError(t, store.updatePodRecord(pod, model, metrics.TimeToFirstTokenSeconds, metrics.PodModelMetricScope, histogramValue))

	for _, metricName := range []string{metrics.NumRequestsRunning, derivedMetric, metrics.TimeToFirstTokenSeconds + "_p50"} {
		family := metricFamilyFromHTTP(t, scrapeGatewayMetrics(t).Body, metricName)
		_, ok := gaugeValueForLabels(family, map[string]string{
			"namespace": pod.Namespace, "pod": pod.Name, "model": model,
		})
		require.True(t, ok, "%s must be exported before the pod lifecycle event", metricName)
	}
}

func TestDeletePodRemovesRawAndDerivedGauges(t *testing.T) {
	store := NewForTest()
	original := requestTrackerTestPod("prefill-gauge-delete", "inference", "old-model", "10.0.0.1")
	original.Status.Phase = v1.PodRunning
	store.addPod(original)
	pod, ok := store.metaPods.Load(utils.GeneratePodKey(original.Namespace, original.Name))
	require.True(t, ok)
	purgeRateHistory(t, pod)
	seedPodGaugeLifecycleMetrics(t, store, pod, "old-model", metrics.PromptTokenTotal, metrics.AvgPromptThroughputToksPerS)

	store.deletePod(original)

	for _, metricName := range []string{metrics.NumRequestsRunning, metrics.AvgPromptThroughputToksPerS, metrics.TimeToFirstTokenSeconds + "_p50"} {
		family := metricFamilyFromHTTP(t, scrapeGatewayMetrics(t).Body, metricName)
		_, ok := gaugeValueForLabels(family, map[string]string{
			"namespace": original.Namespace, "pod": original.Name, "model": "old-model",
		})
		require.False(t, ok, "%s must not survive pod deletion", metricName)
	}
}

func TestUpdatePodRemovesRelabeledRawAndDerivedGauges(t *testing.T) {
	store := NewForTest()
	original := requestTrackerTestPod("decode-gauge-relabel", "inference", "old-model", "10.0.0.1")
	original.Status.Phase = v1.PodRunning
	store.addPod(original)
	pod, ok := store.metaPods.Load(utils.GeneratePodKey(original.Namespace, original.Name))
	require.True(t, ok)
	purgeRateHistory(t, pod)
	seedPodGaugeLifecycleMetrics(t, store, pod, "old-model", metrics.GenerationTokenTotal, metrics.AvgGenerationThroughputToksPerS)

	updated := original.DeepCopy()
	updated.Labels[constants.ModelLabelName] = "new-model"
	store.updatePod(original, updated)

	for _, metricName := range []string{metrics.NumRequestsRunning, metrics.AvgGenerationThroughputToksPerS, metrics.TimeToFirstTokenSeconds + "_p50"} {
		family := metricFamilyFromHTTP(t, scrapeGatewayMetrics(t).Body, metricName)
		_, ok := gaugeValueForLabels(family, map[string]string{
			"namespace": original.Namespace, "pod": original.Name, "model": "old-model",
		})
		require.False(t, ok, "%s must not survive a model relabel", metricName)
	}

	pod, ok = store.metaPods.Load(utils.GeneratePodKey(updated.Namespace, updated.Name))
	require.True(t, ok)
	seedPodGaugeLifecycleMetrics(t, store, pod, "new-model", metrics.GenerationTokenTotal, metrics.AvgGenerationThroughputToksPerS)
}

func TestUpdatePodKeepsGaugesWhenModelUnchanged(t *testing.T) {
	store := NewForTest()
	original := requestTrackerTestPod("prefill-gauge-update", "inference", "model", "10.0.0.1")
	original.Status.Phase = v1.PodRunning
	store.addPod(original)
	pod, ok := store.metaPods.Load(utils.GeneratePodKey(original.Namespace, original.Name))
	require.True(t, ok)
	purgeRateHistory(t, pod)
	seedPodGaugeLifecycleMetrics(t, store, pod, "model", metrics.PromptTokenTotal, metrics.AvgPromptThroughputToksPerS)

	updated := original.DeepCopy()
	updated.Status.PodIP = "10.0.0.2"
	store.updatePod(original, updated)

	metricNames := []string{metrics.NumRequestsRunning, metrics.AvgPromptThroughputToksPerS, metrics.TimeToFirstTokenSeconds + "_p50"}
	for _, metricName := range metricNames {
		family := metricFamilyFromHTTP(t, scrapeGatewayMetrics(t).Body, metricName)
		_, ok := gaugeValueForLabels(family, map[string]string{
			"namespace": original.Namespace, "pod": original.Name, "model": "model",
		})
		require.True(t, ok, "%s must survive an unrelated pod update", metricName)
	}

	store.deletePod(updated)
	for _, metricName := range metricNames {
		family := metricFamilyFromHTTP(t, scrapeGatewayMetrics(t).Body, metricName)
		_, ok := gaugeValueForLabels(family, map[string]string{
			"namespace": original.Namespace, "pod": original.Name, "model": "model",
		})
		require.False(t, ok, "%s must be removed when the updated pod is deleted before another scrape", metricName)
	}
}

// fakeQuarantine quarantines the pods named in the set.
type fakeQuarantine map[string]bool

func (q fakeQuarantine) IsQuarantined(pod *v1.Pod) bool { return q[pod.Name] }

// pointerQuarantine is a comparable checker, as a real one (a pointer) is.
type pointerQuarantine struct{ quarantined bool }

func (q *pointerQuarantine) IsQuarantined(*v1.Pod) bool { return q.quarantined }

func TestClearPodQuarantineChecker(t *testing.T) {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}}
	first, second := &pointerQuarantine{true}, &pointerQuarantine{true}
	store := NewForTest()

	store.SetPodQuarantineChecker(first)
	require.True(t, store.isPodQuarantined(pod))
	store.ClearPodQuarantineChecker(first)
	require.False(t, store.isPodQuarantined(pod), "the checker that was set can clear itself")

	// One slot, last set wins: an earlier checker clearing itself afterwards must not
	// take the later one's registration with it.
	store.SetPodQuarantineChecker(first)
	store.SetPodQuarantineChecker(second)
	store.ClearPodQuarantineChecker(first)
	require.True(t, store.isPodQuarantined(pod), "clearing a checker that is no longer in the slot changes nothing")
	store.ClearPodQuarantineChecker(second)
	require.False(t, store.isPodQuarantined(pod))

	// Clearing an empty slot, or with a checker of an uncomparable type, is harmless.
	require.NotPanics(t, func() {
		store.ClearPodQuarantineChecker(second)
		store.SetPodQuarantineChecker(fakeQuarantine{"pod-1": true})
		store.ClearPodQuarantineChecker(fakeQuarantine{"pod-1": true})
	})
}

func TestUpdatePodMetricsSkipsQuarantinedPods(t *testing.T) {
	var counters []counterCall
	restore := captureCounterCalls(&counters)
	defer restore()

	store := &Store{podMetricsJobs: make(chan podMetricsJob, 10)}
	for _, name := range []string{"pod-0", "pod-1"} {
		pod := newReadyMetricsPod(name, "uid-"+name)
		store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	}

	store.updatePodMetrics()
	require.Equal(t, 2, len(store.podMetricsJobs), "without a checker every ready pod is scraped")
	for len(store.podMetricsJobs) > 0 {
		store.finishPodMetricsScheduling((<-store.podMetricsJobs).Pod)
	}

	var checker PodQuarantineChecker = fakeQuarantine{"pod-1": true}
	store.SetPodQuarantineChecker(checker)
	store.updatePodMetrics()

	require.Equal(t, 1, len(store.podMetricsJobs))
	require.Equal(t, "pod-0", (<-store.podMetricsJobs).Name, "only the pod that is not quarantined is scraped")
	require.Zero(t, countCounterCalls(counters, metrics.PodMetricsEnqueueDroppedTotal, "reason", metrics.PodMetricsDropReasonQueueFull))
}

func TestUpdateModelReplicaMetricsSkipsQuarantinedPods(t *testing.T) {
	t.Setenv("POD_NAME", "gateway-0")

	var emitted []string
	originalFn := metrics.SetGaugeMetricFnForTest
	defer func() { metrics.SetGaugeMetricFnForTest = originalFn }()
	metrics.SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		if name != metrics.ModelReplicas {
			return
		}
		for i, ln := range labelNames {
			if ln == "pod" {
				emitted = append(emitted, labelValues[i])
			}
		}
	}

	store := &Store{}
	for _, name := range []string{"pod-0", "pod-1"} {
		store.metaPods.Store("default/"+name, newReadyMetricsPod(name, "uid-"+name))
	}

	store.updateModelReplicaMetrics()
	require.ElementsMatch(t, []string{"pod-0", "pod-1"}, emitted)
	require.Equal(t, 2, store.modelReplicaEmitted.Len())

	// pod-1 is quarantined: it stops counting as a replica and its series goes.
	store.SetPodQuarantineChecker(fakeQuarantine{"pod-1": true})
	emitted = nil
	store.updateModelReplicaMetrics()
	require.Equal(t, []string{"pod-0"}, emitted)
	require.Equal(t, 1, store.modelReplicaEmitted.Len())

	// The quarantine ends: it counts again.
	store.SetPodQuarantineChecker(fakeQuarantine{})
	emitted = nil
	store.updateModelReplicaMetrics()
	require.ElementsMatch(t, []string{"pod-0", "pod-1"}, emitted)
	require.Equal(t, 2, store.modelReplicaEmitted.Len())
}

// seedCompletedRequestsHistory plants a rate-window baseline for the pod's
// completed_requests counter so calculateRate1m has a snapshot far enough in
// the past (it discards windows shorter than 10s) without sleeping.
func seedCompletedRequestsHistory(t *testing.T, pod *Pod, baseline float64, age time.Duration) {
	t.Helper()
	key := rateHistoryKey(pod, "", "completed_requests")
	rateCalculator.mu.Lock()
	rateCalculator.history[key] = []MetricSnapshot{{Value: baseline, Timestamp: time.Now().Add(-age)}}
	rateCalculator.mu.Unlock()
	t.Cleanup(func() {
		rateCalculator.mu.Lock()
		delete(rateCalculator.history, key)
		rateCalculator.mu.Unlock()
	})
}

// scrapeGatewayMetrics renders the gateway plugin's /metrics response in
// process: a promhttp handler over the default registry, the same gatherer the
// gateway serves (gateway.go mounts promhttp on the default registry), invoked
// through a recorder so the test needs no listening port.
func scrapeGatewayMetrics(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	handler := promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	return recorder
}

// scrapeHandler renders a /metrics response from an isolated test registry.
func scrapeHandler(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	return recorder
}

// metricFamilyFromHTTP parses a /metrics response and returns the named family,
// nil when the endpoint does not export it at all.
func metricFamilyFromHTTP(t *testing.T, body io.Reader, metricName string) *dto.MetricFamily {
	t.Helper()
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(body)
	require.NoError(t, err)
	return families[metricName]
}

// gaugeValueForLabels returns the value of the gauge series whose labels
// include wantLabels.
func gaugeValueForLabels(family *dto.MetricFamily, wantLabels map[string]string) (float64, bool) {
	if family == nil {
		return 0, false
	}
	for _, m := range family.Metric {
		labels := make(map[string]string, len(m.Label))
		for _, lp := range m.Label {
			labels[lp.GetName()] = lp.GetValue()
		}
		matched := true
		for name, want := range wantLabels {
			if labels[name] != want {
				matched = false
				break
			}
		}
		if matched {
			if g := m.GetGauge(); g != nil {
				return g.GetValue(), true
			}
			return 0, false
		}
	}
	return 0, false
}

// Regression for #3501: updateRealtimeRunningRequestsDrainRate1m always tried
// to export realtime_running_requests_drain_rate_1m, but the metric had no
// definition entry, so EmitMetricToPrometheus dropped it and the gateway's
// /metrics response never carried the series. Thirty requests completed over a
// seeded ~30s window must export a gauge of ~1 request per second, with the
// model label following the pod's own metadata.
func TestUpdateRealtimeRunningRequestsDrainRate1m_ExportsDrainRateGauge(t *testing.T) {
	tests := []struct {
		name        string
		podName     string
		labels      map[string]string
		annotations map[string]string
		wantModel   string
	}{
		{
			name:      "model label comes from the pod label",
			podName:   "drain-gauge-label",
			labels:    map[string]string{constants.ModelLabelName: "test-model"},
			wantModel: "test-model",
		},
		{
			name:        "model label falls back to the pod annotation",
			podName:     "drain-gauge-annotation",
			annotations: map[string]string{constants.ModelLabelName: "org/model-with-slashes"},
			wantModel:   "org/model-with-slashes",
		},
		{
			name:    "model label is empty when the pod has neither",
			podName: "drain-gauge-empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &Store{}
			pod := namespacedTestPod(testNamespace, tt.podName)
			pod.Labels = tt.labels
			pod.Annotations = tt.annotations
			pod.Status.PodIP = "10.0.0.1"
			pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
			store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)

			seedCompletedRequestsHistory(t, pod, 0, 30*time.Second)
			atomic.StoreInt64(&pod.completedRequests, 30) // 30 completions over the ~30s window

			store.updateRealtimeRunningRequestsDrainRate1m(pod)
			handler := runningRequestsMetricsHandler(t, store)

			family := metricFamilyFromHTTP(t, scrapeHandler(t, handler).Body, metrics.RealtimeRunningRequestsDrainRate1m)
			require.NotNil(t, family, "the gateway /metrics response must export %s", metrics.RealtimeRunningRequestsDrainRate1m)
			require.Equal(t, dto.MetricType_GAUGE, family.GetType())

			wantLabels := map[string]string{
				"namespace":   testNamespace,
				"pod":         pod.Name,
				"model":       tt.wantModel,
				"gateway_pod": metrics.GatewayPodName(),
			}
			value, ok := gaugeValueForLabels(family, wantLabels)
			require.True(t, ok, "series with labels %v must be exported", wantLabels)
			require.InDelta(t, 1.0, value, 0.05, "30 completions over the seeded ~30s window")
		})
	}
}

// A later update that computes a zero rate must report zero on the same series,
// not keep the last non-zero value (#3501).
func TestUpdateRealtimeRunningRequestsDrainRate1m_ZeroRateReportsZeroOnSameSeries(t *testing.T) {
	store := &Store{}
	pod := namespacedTestPod(testNamespace, "drain-gauge-zero")
	pod.Labels = map[string]string{constants.ModelLabelName: "test-model-zero"}
	pod.Status.PodIP = "10.0.0.1"
	pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)

	wantLabels := map[string]string{
		"namespace":   testNamespace,
		"pod":         pod.Name,
		"model":       "test-model-zero",
		"gateway_pod": metrics.GatewayPodName(),
	}

	// 30 completions over the ~30s window: the gauge reports ~1 request per second.
	seedCompletedRequestsHistory(t, pod, 0, 30*time.Second)
	atomic.StoreInt64(&pod.completedRequests, 30)
	store.updateRealtimeRunningRequestsDrainRate1m(pod)
	handler := runningRequestsMetricsHandler(t, store)

	family := metricFamilyFromHTTP(t, scrapeHandler(t, handler).Body, metrics.RealtimeRunningRequestsDrainRate1m)
	value, ok := gaugeValueForLabels(family, wantLabels)
	require.True(t, ok, "the series must be exported before the rate reaches zero")
	require.InDelta(t, 1.0, value, 0.05)

	// A later window with no further completions computes a zero rate.
	seedCompletedRequestsHistory(t, pod, 30, 30*time.Second)
	store.updateRealtimeRunningRequestsDrainRate1m(pod)

	family = metricFamilyFromHTTP(t, scrapeHandler(t, handler).Body, metrics.RealtimeRunningRequestsDrainRate1m)
	value, ok = gaugeValueForLabels(family, wantLabels)
	require.True(t, ok, "the series must stay exported when the rate reaches zero")
	require.Equal(t, 0.0, value)
}
