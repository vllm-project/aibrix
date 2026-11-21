/*
Copyright 2026 The AIBrix Authors.

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
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
)

func modelClaimMetricsTestPort(t *testing.T, handler http.HandlerFunc) int {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	_, portString, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portString)
	require.NoError(t, err)
	return port
}

func setMetricsTestBinding(pod *v1.Pod, claim, model string, port int, state string) {
	pod.Annotations[constants.ModelClaimPodAnnotationPrefix+claim] =
		fmt.Sprintf(`{"model":%q,"port":%d,"state":%q}`, model, port, state)
}

func newModelClaimMetricsTestPod(name string) *v1.Pod {
	pod := newReadyMetricsPod(name, "uid-"+name).Pod
	pod.Labels = map[string]string{constants.ModelPoolLabelEnabled: constants.ModelPoolLabelEnabledValue}
	pod.Annotations = make(map[string]string)
	pod.Status.PodIP = "127.0.0.1"
	return pod
}

func newModelClaimMetricsTestStore() *Store {
	return &Store{
		engineMetricsFetcher: metrics.NewEngineMetricsFetcherWithConfig(metrics.EngineMetricsFetcherConfig{
			Timeout: 4 * time.Second,
		}),
		podMetricsJobs: make(chan podMetricsJob, 8),
	}
}

func startMetricsTestWorkers(t *testing.T, store *Store) func() {
	t.Helper()
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			store.worker(store.podMetricsJobs)
		}()
	}
	stop := sync.OnceFunc(func() {
		close(store.podMetricsJobs)
		workers.Wait()
	})
	t.Cleanup(stop)
	return stop
}

func writeMetricsTestGauge(w http.ResponseWriter, model string, value float64) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintf(w, "# TYPE vllm:kv_cache_usage_perc gauge\nvllm:kv_cache_usage_perc{model_name=%q} %g\n", model, value)
}

func assertMetricsTestGauge(t *testing.T, pod *v1.Pod, model string, want *float64, engineType ...string) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != metrics.KVCacheUsagePerc {
			continue
		}
		for _, sample := range family.Metric {
			labels := map[string]string{}
			for _, pair := range sample.Label {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels["namespace"] == pod.Namespace && labels["pod"] == pod.Name && labels["model"] == model {
				require.NotNil(t, want, "retired binding still exports a gauge")
				require.InDelta(t, *want, sample.GetGauge().GetValue(), 0.0001)
				if len(engineType) != 0 {
					require.Equal(t, engineType[0], labels["engine_type"])
				}
				return
			}
		}
	}
	require.Nil(t, want, "active binding never exported its gauge")
}

func waitForMetricsTestValue(t *testing.T, store *Store, pod *v1.Pod, model string, want float64) {
	t.Helper()
	require.Eventually(t, func() bool {
		record, err := store.GetMetricRecordByPodModel(pod.Name, pod.Namespace, model, metrics.KVCacheUsagePerc)
		return err == nil && record.Value.GetSimpleValue() == want
	}, 2*time.Second, 5*time.Millisecond, "no KV-cache usage %g for %s", want, model)
}

func TestModelClaimMetricsIndependentScrapes(t *testing.T) {
	stalled := make(chan struct{})
	release := make(chan struct{})
	allowFailure := sync.OnceFunc(func() { close(release) })
	var failedHits atomic.Int32
	a := modelClaimMetricsTestPort(t, func(w http.ResponseWriter, r *http.Request) {
		if failedHits.Add(1) == 1 {
			close(stalled)
		}
		select {
		case <-release:
			http.Error(w, "engine unavailable", http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	})
	t.Cleanup(allowFailure)
	var healthyHits atomic.Int32
	b := modelClaimMetricsTestPort(t, func(w http.ResponseWriter, _ *http.Request) {
		writeMetricsTestGauge(w, "org/model-b", 0.25*float64(healthyHits.Add(1)))
		_, _ = fmt.Fprintln(w, `vllm:kv_cache_usage_perc{model_name="another-model"} 0.9`)
		_, _ = fmt.Fprintln(w, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running 3")
		_, _ = fmt.Fprintln(w, "# TYPE vllm:num_requests_waiting gauge")
		_, _ = fmt.Fprintln(w, `vllm:num_requests_waiting{model_name="another-model"} 7`)
	})

	store := newModelClaimMetricsTestStore()
	pod := newModelClaimMetricsTestPod("claim-metrics-independent")
	setMetricsTestBinding(pod, "a", "model-a", a, constants.ModelClaimRoutingStateActive)
	setMetricsTestBinding(pod, "b", "org/model-b", b, constants.ModelClaimRoutingStateActive)
	store.addPod(pod)
	stop := startMetricsTestWorkers(t, store)
	defer store.deletePod(pod)
	defer allowFailure()

	store.updatePodMetrics()
	select {
	case <-stalled:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not scrape the bound port")
	}
	waitForMetricsTestValue(t, store, pod, "org/model-b", 0.25)
	allowFailure()
	stop()

	store.podMetricsJobs = make(chan podMetricsJob, 8)
	stop = startMetricsTestWorkers(t, store)
	store.updatePodMetrics()
	stop()
	waitForMetricsTestValue(t, store, pod, "org/model-b", 0.5)
	require.EqualValues(t, 1, failedHits.Load(), "only the failing binding should be in backoff")
	_, err := store.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "another-model", metrics.KVCacheUsagePerc)
	require.Error(t, err)
	running, err := store.GetMetricValueByPodModel(pod.Name, pod.Namespace, "org/model-b", metrics.NumRequestsRunning)
	require.NoError(t, err)
	require.Equal(t, float64(3), running.GetSimpleValue())
	require.Equal(t, "org/model-b", running.GetLabelValues()["model_name"])
	_, err = store.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "org/model-b", metrics.NumRequestsWaiting)
	require.Error(t, err, "a named sample for another engine cannot be attributed to the binding")
}

func TestModelClaimMetricsRejectsLateScrapeAfterSamePortWake(t *testing.T) {
	oldInFlight := make(chan struct{})
	release := make(chan struct{})
	releaseOld := sync.OnceFunc(func() { close(release) })
	var hits atomic.Int32
	port := modelClaimMetricsTestPort(t, func(w http.ResponseWriter, r *http.Request) {
		switch hits.Add(1) {
		case 1:
			writeMetricsTestGauge(w, "model", 0.25)
		case 2:
			close(oldInFlight)
			select {
			case <-release:
				writeMetricsTestGauge(w, "model", 0.5)
			case <-r.Context().Done():
			}
		default:
			writeMetricsTestGauge(w, "model", 0.75)
		}
	})
	t.Cleanup(releaseOld)
	store := newModelClaimMetricsTestStore()
	pod := newModelClaimMetricsTestPod("claim-metrics-same-port")
	setMetricsTestBinding(pod, "claim", "model", port, constants.ModelClaimRoutingStateActive)
	store.addPod(pod)
	stop := startMetricsTestWorkers(t, store)
	store.updatePodMetrics()
	stop()
	waitForMetricsTestValue(t, store, pod, "model", 0.25)
	first := 0.25
	assertMetricsTestGauge(t, pod, "model", &first)

	store.podMetricsJobs = make(chan podMetricsJob, 8)
	stop = startMetricsTestWorkers(t, store)
	defer releaseOld()
	store.updatePodMetrics()
	select {
	case <-oldInFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("the old scrape did not start")
	}

	sleeping := pod.DeepCopy()
	setMetricsTestBinding(sleeping, "claim", "model", 0, constants.ModelClaimRoutingStateSleeping)
	store.updatePod(pod, sleeping)
	_, err := store.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.KVCacheUsagePerc)
	require.Error(t, err)
	assertMetricsTestGauge(t, pod, "model", nil)

	awake := pod.DeepCopy()
	store.updatePod(sleeping, awake)
	defer store.deletePod(awake)
	store.updatePodMetrics()
	waitForMetricsTestValue(t, store, pod, "model", 0.75)
	releaseOld()
	stop()
	waitForMetricsTestValue(t, store, pod, "model", 0.75)
	current := 0.75
	assertMetricsTestGauge(t, pod, "model", &current)
}

// Samples for an unchanged engine must survive a sibling's port change.
func TestModelClaimMetricsPortChangePreservesSibling(t *testing.T) {
	first := modelClaimMetricsTestPort(t, func(w http.ResponseWriter, _ *http.Request) {
		writeMetricsTestGauge(w, "a", 0.25)
	})
	second := modelClaimMetricsTestPort(t, func(w http.ResponseWriter, _ *http.Request) {
		writeMetricsTestGauge(w, "b", 0.5)
	})
	replacement := modelClaimMetricsTestPort(t, func(w http.ResponseWriter, _ *http.Request) {
		writeMetricsTestGauge(w, "a", 0.75)
	})
	store := newModelClaimMetricsTestStore()
	pod := newModelClaimMetricsTestPod("claim-metrics-port-change")
	setMetricsTestBinding(pod, "a", "a", first, constants.ModelClaimRoutingStateActive)
	setMetricsTestBinding(pod, "b", "b", second, constants.ModelClaimRoutingStateActive)
	store.addPod(pod)
	defer store.deletePod(pod)

	stop := startMetricsTestWorkers(t, store)
	store.updatePodMetrics()
	stop()
	waitForMetricsTestValue(t, store, pod, "a", 0.25)
	waitForMetricsTestValue(t, store, pod, "b", 0.5)

	rebound := pod.DeepCopy()
	setMetricsTestBinding(rebound, "a", "a", replacement, constants.ModelClaimRoutingStateActive)
	store.updatePod(pod, rebound)
	_, err := store.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "a", metrics.KVCacheUsagePerc)
	require.Error(t, err)
	assertMetricsTestGauge(t, pod, "a", nil)
	waitForMetricsTestValue(t, store, pod, "b", 0.5)

	store.podMetricsJobs = make(chan podMetricsJob, 8)
	stop = startMetricsTestWorkers(t, store)
	store.updatePodMetrics()
	stop()
	waitForMetricsTestValue(t, store, pod, "a", 0.75)

	unbound := rebound.DeepCopy()
	delete(unbound.Annotations, constants.ModelClaimPodAnnotationPrefix+"a")
	store.updatePod(rebound, unbound)
	_, err = store.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "a", metrics.KVCacheUsagePerc)
	require.Error(t, err)
	assertMetricsTestGauge(t, pod, "a", nil)
	waitForMetricsTestValue(t, store, pod, "b", 0.5)
}

func TestModelClaimMetricsDetectsMixedEngines(t *testing.T) {
	vllmPort := modelClaimMetricsTestPort(t, func(w http.ResponseWriter, _ *http.Request) {
		writeMetricsTestGauge(w, "vllm-model", 0.2)
	})
	sglangPort := modelClaimMetricsTestPort(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintln(w, "# TYPE sglang:token_usage gauge")
		_, _ = fmt.Fprintln(w, `sglang:token_usage{model_name="sglang-model"} 0.8`)
	})
	store := newModelClaimMetricsTestStore()
	pod := newModelClaimMetricsTestPod("claim-metrics-mixed-engines")
	pod.Labels[constants.ModelLabelEngine] = metrics.EngineNameVLLM
	setMetricsTestBinding(pod, "vllm", "vllm-model", vllmPort, constants.ModelClaimRoutingStateActive)
	setMetricsTestBinding(pod, "sglang", "sglang-model", sglangPort, constants.ModelClaimRoutingStateActive)
	store.addPod(pod)
	defer store.deletePod(pod)

	stop := startMetricsTestWorkers(t, store)
	store.updatePodMetrics()
	stop()
	waitForMetricsTestValue(t, store, pod, "vllm-model", 0.2)
	waitForMetricsTestValue(t, store, pod, "sglang-model", 0.8)
	usage := 0.8
	assertMetricsTestGauge(t, pod, "sglang-model", &usage, metrics.EngineNameSGLang)
}
