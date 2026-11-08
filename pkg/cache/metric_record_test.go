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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

func TestMetricRecordValueCompatibilityAndPartialUpdate(t *testing.T) {
	c := NewForTest()
	pod := newReadyMetricsPod("p", "uid-a")
	c.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	before := time.Unix(1000, 0)
	value := &metrics.SimpleMetricValue{Value: 0.1}
	require.NoError(t, c.updatePodRecordAt(pod, "model", metrics.KVCacheUsagePerc, metrics.PodModelMetricScope, value, before))
	require.NoError(t, c.updatePodRecordAt(pod, "", metrics.GPUBusyTimeRatio, metrics.PodMetricScope, value, before))
	for _, model := range []bool{false, true} {
		var record MetricRecord
		var old metrics.MetricValue
		var err error
		if model {
			record, err = c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.KVCacheUsagePerc)
			require.NoError(t, err)
			old, err = c.GetMetricValueByPodModel(pod.Name, pod.Namespace, "model", metrics.KVCacheUsagePerc)
		} else {
			record, err = c.GetMetricRecordByPod(pod.Name, pod.Namespace, metrics.GPUBusyTimeRatio)
			require.NoError(t, err)
			old, err = c.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.GPUBusyTimeRatio)
		}
		require.NoError(t, err)
		require.Same(t, value, old)
		require.Same(t, value, record.Value)
		require.Equal(t, before, record.Timestamp)
	}
	later := before.Add(time.Minute)
	c.updatePodMetricsFromTypedResultAt(pod, &metrics.EngineMetricsResult{
		ModelMetrics: map[string]metrics.MetricValue{"model/" + metrics.NumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
		Errors:       []error{errors.New("KV metric absent")},
	}, later)
	retained, err := c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.KVCacheUsagePerc)
	require.NoError(t, err)
	require.Equal(t, before, retained.Timestamp)
	running, err := c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.NumRequestsRunning)
	require.NoError(t, err)
	require.Equal(t, later, running.Timestamp)
	require.NoError(t, c.updatePodRecordAt(pod, "model", metrics.KVCacheUsagePerc, metrics.PodModelMetricScope, &metrics.SimpleMetricValue{Value: 0.9}, later))
	recovered, err := c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.KVCacheUsagePerc)
	require.NoError(t, err)
	require.Equal(t, later, recovered.Timestamp)
	require.Equal(t, 0.9, recovered.Value.GetSimpleValue())
	_, err = c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.CPUCacheUsagePerc)
	var missing *MetricNotFoundError
	require.ErrorAs(t, err, &missing)
}

func TestMetricRecordWorkerFailureDoesNotRefreshSamples(t *testing.T) {
	var mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case 2:
			fmt.Fprint(w, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running{model_name=\"model\"} 1\n")
		default:
			fmt.Fprint(w, "# TYPE vllm:kv_cache_usage_perc gauge\nvllm:kv_cache_usage_perc{model_name=\"model\"} 0.1\n")
		}
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	pod := newReadyMetricsPod("worker", "uid-worker")
	pod.Status.PodIP = "127.0.0.1"
	pod.Labels[MetricPortLabel] = port
	c := &Store{engineMetricsFetcher: metrics.NewEngineMetricsFetcher()}
	c.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	oldTimeout := podMetricsFetchTimeout
	podMetricsFetchTimeout = 100 * time.Millisecond
	t.Cleanup(func() { podMetricsFetchTimeout = oldTimeout })
	scrape := func() { jobs := make(chan *Pod, 1); jobs <- pod; close(jobs); c.worker(jobs) }
	scrape()
	first, err := c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.KVCacheUsagePerc)
	require.NoError(t, err)
	require.False(t, first.Timestamp.IsZero())
	for _, phase := range []int32{1, 2} {
		mode.Store(phase)
		scrape()
		retained, e := c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.KVCacheUsagePerc)
		require.NoError(t, e)
		require.Equal(t, first, retained, "phase %d", phase)
	}
	running, err := c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.NumRequestsRunning)
	require.NoError(t, err)
	require.GreaterOrEqual(t, running.Timestamp.UnixNano(), first.Timestamp.UnixNano())
}

func TestMetricRecordsDoNotSurvivePodUIDReplacement(t *testing.T) {
	c := NewForTest()
	old := newReadyMetricsPod("p", "old")
	c.addPod(old.Pod)
	key := utils.GeneratePodKey(old.Namespace, old.Name)
	meta, ok := c.metaPods.Load(key)
	require.True(t, ok)
	require.NoError(t, c.updatePodRecord(meta, "model", metrics.KVCacheUsagePerc, metrics.PodModelMetricScope, &metrics.SimpleMetricValue{Value: 0.1}))
	replacement := old.DeepCopy()
	replacement.UID = "new"
	c.updatePod(old.Pod, replacement)
	_, err := c.GetMetricRecordByPodModel(old.Name, old.Namespace, "model", metrics.KVCacheUsagePerc)
	var missing *MetricNotFoundError
	require.ErrorAs(t, err, &missing)
	// A finishing worker for the detached old Pod cannot populate the replacement.
	require.NoError(t, c.updatePodRecord(meta, "model", metrics.KVCacheUsagePerc, metrics.PodModelMetricScope, &metrics.SimpleMetricValue{Value: 0.2}))
	_, err = c.GetMetricRecordByPodModel(old.Name, old.Namespace, "model", metrics.KVCacheUsagePerc)
	require.ErrorAs(t, err, &missing)
}

func TestRoutingMetricStaleThreshold(t *testing.T) {
	oldRefresh, oldTimeout := podMetricRefreshInterval, podMetricsFetchTimeout
	t.Cleanup(func() { podMetricRefreshInterval, podMetricsFetchTimeout = oldRefresh, oldTimeout })
	for _, tc := range []struct {
		name             string
		refresh, timeout time.Duration
		override         string
		want             time.Duration
	}{
		{"defaults", time.Second, 5 * time.Second, "", 6 * time.Second},
		{"refresh dominates", 3 * time.Second, time.Second, "", 9 * time.Second},
		{"timeout dominates", time.Second, 10 * time.Second, "", 11 * time.Second},
		{"floor", 50 * time.Millisecond, time.Second, "", 5 * time.Second},
		{"override", time.Second, 5 * time.Second, "15000", 15 * time.Second},
		{"zero", time.Second, 5 * time.Second, "0", 6 * time.Second},
		{"negative", time.Second, 5 * time.Second, "-1", 6 * time.Second},
		{"malformed", time.Second, 5 * time.Second, "bad", 6 * time.Second},
		{"overflow", time.Second, 5 * time.Second, "9223372036854775807", 6 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			podMetricRefreshInterval, podMetricsFetchTimeout = tc.refresh, tc.timeout
			t.Setenv("AIBRIX_ROUTING_METRIC_STALE_THRESHOLD_MS", tc.override)
			require.Equal(t, tc.want, loadRoutingMetricStaleThreshold())
		})
	}
}

func TestGlobalCacheProvidesMetricRecords(t *testing.T) {
	previous := store
	t.Cleanup(func() { store = previous })
	initialized := InitForTest()
	got, err := Get()
	require.NoError(t, err)
	records, ok := got.(MetricRecordCache)
	require.True(t, ok)
	require.Same(t, initialized, records)
}

// MetricRecordWorkerForTest exposes the real collector only to external-package
// integration tests, so they can run a router without a cache import cycle.
func MetricRecordWorkerForTest(port string) (*Store, *v1.Pod, func()) {
	pod := newReadyMetricsPod("worker-router", "uid-worker-router")
	pod.Status.PodIP = "127.0.0.1"
	pod.Labels[MetricPortLabel] = port
	c := NewWithPodsForTest([]*v1.Pod{pod.Pod}, "model")
	meta, _ := c.metaPods.Load(utils.GeneratePodKey(pod.Namespace, pod.Name))
	return c, pod.Pod, func() {
		jobs := make(chan *Pod, 1)
		jobs <- meta
		close(jobs)
		c.worker(jobs)
	}
}
