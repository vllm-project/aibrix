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
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
