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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

// The collector exports a pod's estimate, its age and the fraction once the pod has them, with
// the labels of gateway_pod_outstanding_requests. A pod without an estimate, and a deleted pod,
// have no series.
func TestPodOutputRateCollector(t *testing.T) {
	cache := NewForTest()
	cache.addPod(requestTrackerTestPod("model-0", "inference", "model", "10.0.0.1"))
	cache.addPod(requestTrackerTestPod("model-1", "inference", "model", "10.0.0.2")) // no estimate yet
	cache.addPod(requestTrackerTestPod("model-2", "inference", "model", "10.0.0.3"))
	cache.metaPods.Store("inference/invalid", &Pod{})

	now := time.Now()
	collector := newPodOutputRateCollector(cache, "coord-a")
	collector.now = func() time.Time { return now }
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)

	record := func(podName, metric string, value float64, age time.Duration) {
		pod, ok := cache.metaPods.Load("inference/" + podName)
		require.True(t, ok)
		require.NoError(t, cache.updatePodRecordAt(pod, "", metric, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: value}, now.Add(-age)))
	}
	record("model-0", metrics.RealtimeOutputTokensPerRequestSecondEWMA, 46.5, 90*time.Second)
	record("model-0", metrics.RealtimeRequestsWithoutOutputTokensFraction1m, 0.25, 90*time.Second)
	record("model-2", metrics.RealtimeRequestsWithoutOutputTokensFraction1m, 1, time.Second) // only failures so far

	const labels0 = `{gateway_pod="coord-a",model="model",target_namespace="inference",target_pod="model-0"}`
	const labels2 = `{gateway_pod="coord-a",model="model",target_namespace="inference",target_pod="model-2"}`
	expected := `# HELP gateway_pod_output_rate_age_seconds Seconds since gateway_pod_output_tokens_per_request_second was last updated for the pod.
# TYPE gateway_pod_output_rate_age_seconds gauge
gateway_pod_output_rate_age_seconds` + labels0 + ` 90
# HELP gateway_pod_output_tokens_per_request_second Output tokens per second of one request's time on the pod, over requests this gateway routed (EWMA of ~1-minute windows). Not updated while the pod completes too few requests; see gateway_pod_output_rate_age_seconds.
# TYPE gateway_pod_output_tokens_per_request_second gauge
gateway_pod_output_tokens_per_request_second` + labels0 + ` 46.5
# HELP gateway_pod_requests_without_output_tokens_fraction Share of the requests this gateway routed to the pod that finished without output tokens, in the last ~1-minute window in which any finished. They are not part of gateway_pod_output_tokens_per_request_second.
# TYPE gateway_pod_requests_without_output_tokens_fraction gauge
gateway_pod_requests_without_output_tokens_fraction` + labels0 + ` 0.25
gateway_pod_requests_without_output_tokens_fraction` + labels2 + ` 1
`
	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(expected)))

	// A deleted pod leaves no series.
	cache.metaPods.Delete("inference/model-0")
	cache.metaPods.Delete("inference/model-2")
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Empty(t, families)
}

// The collector registers and, once stopped, unregisters.
func TestInitPodOutputRateCollector(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	stop := make(chan struct{})
	require.NoError(t, initPodOutputRateCollector(NewForTest(), registry, stop))
	require.Error(t, initPodOutputRateCollector(NewForTest(), registry, nil), "a second registration is refused")

	close(stop)
	require.Eventually(t, func() bool {
		return registry.Register(newPodOutputRateCollector(NewForTest(), "probe")) == nil
	}, time.Second, 10*time.Millisecond)
}
