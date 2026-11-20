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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodMetricTargetsModelClaims(t *testing.T) {
	pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "warm",
		Annotations: map[string]string{
			constants.ModelClaimPodAnnotationPrefix + "qwen":  `{"model":"qwen","port":20001,"state":"active"}`,
			constants.ModelClaimPodAnnotationPrefix + "llama": `{"model":"llama","port":20000,"state":"active"}`,
			constants.ModelClaimPodAnnotationPrefix + "sleep": `{"model":"sleep","port":0,"state":"sleeping"}`,
		},
	}}}
	require.Equal(t, []podMetricTarget{
		{model: "llama", port: 20000},
		{model: "qwen", port: 20001},
	}, podMetricTargets(pod))
}

func TestPodMetricTargetsSleepingClaimsHaveNoFallback(t *testing.T) {
	pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "warm",
		Annotations: map[string]string{
			constants.ModelClaimPodAnnotationPrefix + "sleep": `{"model":"sleep","port":0,"state":"sleeping"}`,
		},
	}}}
	require.Empty(t, podMetricTargets(pod))
}

func TestPodMetricTargetsMalformedClaimHasNoFallback(t *testing.T) {
	pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "warm",
		Annotations: map[string]string{
			constants.ModelClaimPodAnnotationPrefix + "broken": "{",
		},
	}}}
	require.Empty(t, podMetricTargets(pod))
}

func TestPodMetricTargetsOrdinaryPodRetainsLegacyPort(t *testing.T) {
	pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:   "regular",
		Labels: map[string]string{MetricPortLabel: "9999"},
	}}}
	require.Equal(t, []podMetricTarget{{port: 9999}}, podMetricTargets(pod))
}

func TestBoundModelMetricLabelsAreSetEvenWhenMissing(t *testing.T) {
	result := &metrics.EngineMetricsResult{ModelMetrics: map[string]metrics.MetricValue{
		"internal/" + metrics.KVCacheUsagePerc:       &metrics.SimpleMetricValue{Value: 0.5},
		"internal/" + metrics.TimeToFirstTokenSeconds: &metrics.HistogramMetricValue{},
	}}
	remapBoundModelMetrics(result, "claim-model")
	require.Len(t, result.ModelMetrics, 2)
	simple := result.ModelMetrics["claim-model/"+metrics.KVCacheUsagePerc].(*metrics.SimpleMetricValue)
	hist := result.ModelMetrics["claim-model/"+metrics.TimeToFirstTokenSeconds].(*metrics.HistogramMetricValue)
	require.Equal(t, "claim-model", simple.Labels["model_name"])
	require.Equal(t, "claim-model", hist.Labels["model_name"])
}

func newMockModelClaimMetricsServer(t *testing.T, model string, value float64) int {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, "# TYPE vllm:kv_cache_usage_perc gauge\nvllm:kv_cache_usage_perc{model_name=%q} %g\n", model, value)
	}))
	t.Cleanup(server.Close)
	return server.Listener.Addr().(*net.TCPAddr).Port
}

func newBoundModelClaimPod(ports map[string]int) *Pod {
	pod := newReadyMetricsPod("warm-modelclaim-metrics", "uid-warm-modelclaim")
	pod.Status.PodIP = "127.0.0.1"
	pod.Annotations = make(map[string]string, len(ports))
	for model, port := range ports {
		pod.Annotations[constants.ModelClaimPodAnnotationPrefix+model] = fmt.Sprintf(
			`{"model":%q,"port":%d,"state":"active"}`, model, port)
	}
	return pod
}

func scrapeBoundModelClaims(pod *Pod, c *Store) {
	jobs := make(chan *Pod, 1)
	jobs <- pod
	close(jobs)
	c.worker(jobs)
}

func TestModelClaimWorkerScrapesEachBoundPort(t *testing.T) {
	firstPort := newMockModelClaimMetricsServer(t, "engine-internal-first", 0.25)
	secondPort := newMockModelClaimMetricsServer(t, "engine-internal-second", 0.75)
	pod := newBoundModelClaimPod(map[string]int{"first": firstPort, "second": secondPort})
	c := &Store{engineMetricsFetcher: metrics.NewEngineMetricsFetcherWithConfig(
		metrics.EngineMetricsFetcherConfig{Timeout: time.Second, MaxRetries: 0},
	)}
	c.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	scrapeBoundModelClaims(pod, c)

	for _, test := range []struct {
		model string
		value float64
	}{{"first", 0.25}, {"second", 0.75}} {
		record, err := c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, test.model, metrics.KVCacheUsagePerc)
		require.NoError(t, err, test.model)
		require.InDelta(t, test.value, record.Value.GetSimpleValue(), 1e-9)
		require.Equal(t, test.model, record.Value.GetLabelValues()["model_name"])
	}
	_, err := c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "engine-internal-first", metrics.KVCacheUsagePerc)
	require.Error(t, err, "engine internal model must not appear as a routing model")
}

func TestModelClaimWorkerPartialFailureKeepsHealthyEngine(t *testing.T) {
	failedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(failedServer.Close)
	failedPort := failedServer.Listener.Addr().(*net.TCPAddr).Port
	healthyPort := newMockModelClaimMetricsServer(t, "engine-model", 0.4)
	pod := newBoundModelClaimPod(map[string]int{"bad": failedPort, "healthy": healthyPort})
	c := &Store{engineMetricsFetcher: metrics.NewEngineMetricsFetcherWithConfig(
		metrics.EngineMetricsFetcherConfig{Timeout: time.Second, MaxRetries: 0},
	)}
	c.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	scrapeBoundModelClaims(pod, c)

	record, err := c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "healthy", metrics.KVCacheUsagePerc)
	require.NoError(t, err)
	require.InDelta(t, 0.4, record.Value.GetSimpleValue(), 1e-9)
	_, err = c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "bad", metrics.KVCacheUsagePerc)
	require.Error(t, err)
	require.False(t, c.shouldSkipPodMetricsFetch(pod, time.Now()), "one failing endpoint must not back off healthy engines")
}
