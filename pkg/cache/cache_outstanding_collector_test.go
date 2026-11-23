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
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
)

func assertOutstandingMetrics(t *testing.T, registry *prometheus.Registry, samples string) {
	t.Helper()
	if samples == "" {
		families, err := registry.Gather()
		require.NoError(t, err)
		require.Empty(t, families)
		return
	}
	expected := "# HELP gateway_pod_outstanding_requests Requests routed by this gateway to a pod that have not completed.\n" +
		"# TYPE gateway_pod_outstanding_requests gauge\n" + samples
	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(expected), "gateway_pod_outstanding_requests"))
}

func TestPodOutstandingCollector_SkipsInvalidPods(t *testing.T) {
	for _, tt := range []struct {
		name string
		pod  *Pod
	}{
		{name: "nil pod"},
		{name: "nil underlying pod", pod: &Pod{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewForTest()
			cache.addPod(requestTrackerTestPod("model-0", "inference", "model", "10.0.0.1"))
			cache.metaPods.Store("inference/invalid", tt.pod)
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(newPodOutstandingCollector(cache, "coord-a"))

			assertOutstandingMetrics(t, registry,
				"gateway_pod_outstanding_requests{gateway_pod=\"coord-a\",model=\"model\",namespace=\"inference\",pod=\"model-0\"} 0\n")
		})
	}
}

func TestPodOutstandingCollector_ModelNameMetadata(t *testing.T) {
	for _, tt := range []struct {
		name       string
		label      string
		annotation string
		want       string
	}{
		{name: "label", label: "base-model", want: "base-model"},
		{name: "annotation", annotation: "org/model", want: "org/model"},
		{name: "label takes precedence", label: "base-model", annotation: "org/model", want: "base-model"},
		{name: "missing metadata", want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewForTest()
			// Multiple served models must not duplicate the pod's total count.
			pod := warmModelClaimPod("model-0", "inference", map[string]int{"model-a": 9001, "model-b": 9002})
			pod.Labels[constants.ModelLabelName] = tt.label
			pod.Annotations[constants.ModelLabelName] = tt.annotation
			cache.addPod(pod)
			metaPod, ok := cache.metaPods.Load("inference/model-0")
			require.True(t, ok)
			atomic.StoreInt32(&metaPod.runningRequests, 3)
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(newPodOutstandingCollector(cache, "coord-a"))

			assertOutstandingMetrics(t, registry, fmt.Sprintf(
				"gateway_pod_outstanding_requests{gateway_pod=\"coord-a\",model=%q,namespace=\"inference\",pod=\"model-0\"} 3\n", tt.want))
		})
	}
}

func TestPodOutstandingCollector_ModelNameUpdate(t *testing.T) {
	cache := NewForTest()
	pod := requestTrackerTestPod("model-0", "inference", "old-model", "10.0.0.1")
	cache.addPod(pod)
	metaPod, ok := cache.metaPods.Load("inference/model-0")
	require.True(t, ok)
	atomic.StoreInt32(&metaPod.runningRequests, 3)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPodOutstandingCollector(cache, "coord-a"))
	sample := "gateway_pod_outstanding_requests{gateway_pod=\"coord-a\",model=%q,namespace=\"inference\",pod=\"model-0\"} 3\n"
	assertOutstandingMetrics(t, registry, fmt.Sprintf(sample, "old-model"))

	updated := pod.DeepCopy()
	delete(updated.Labels, constants.ModelLabelName)
	updated.Annotations = map[string]string{constants.ModelLabelName: "org/new-model"}
	cache.updatePod(pod, updated)
	assertOutstandingMetrics(t, registry, fmt.Sprintf(sample, "org/new-model"))
}

func TestPodOutstandingCollector_InvalidModelName(t *testing.T) {
	for _, source := range []string{"label", "annotation"} {
		t.Run(source, func(t *testing.T) {
			cache := NewForTest()
			cache.addPod(requestTrackerTestPod("model-0", "inference", "model", "10.0.0.1"))
			pod := requestTrackerTestPod("invalid-0", "inference", "bad\xffname", "10.0.0.2")
			if source == "annotation" {
				delete(pod.Labels, constants.ModelLabelName)
				pod.Annotations = map[string]string{constants.ModelLabelName: "bad\xffname"}
			}
			cache.addPod(pod)
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(newPodOutstandingCollector(cache, "coord-a"))

			// Gather runs Collect in a worker goroutine. Invalid metadata must
			// report a collection error rather than panic and exit the process.
			families, err := registry.Gather()
			require.ErrorContains(t, err, "not valid UTF-8")
			require.Len(t, families, 1)
			require.Len(t, families[0].Metric, 1, "valid pods must still be collected")
			labels := make(map[string]string)
			for _, label := range families[0].Metric[0].Label {
				labels[label.GetName()] = label.GetValue()
			}
			require.Equal(t, "model-0", labels["pod"])

			cache.deletePod(pod)
			assertOutstandingMetrics(t, registry,
				"gateway_pod_outstanding_requests{gateway_pod=\"coord-a\",model=\"model\",namespace=\"inference\",pod=\"model-0\"} 0\n")
		})
	}
}

func TestPodOutstandingCollector_RequestLifecycle(t *testing.T) {
	for _, completion := range []string{"count", "trace", "nil-context"} {
		t.Run(completion, func(t *testing.T) {
			cache := NewForTest()
			cache.enableTracing = false
			pod := requestTrackerTestPod("model-0", "inference", "model", "10.0.0.1")
			cache.addPod(pod)
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(newPodOutstandingCollector(cache, "coord-a"))
			sample := "gateway_pod_outstanding_requests{gateway_pod=\"coord-a\",model=\"model\",namespace=\"inference\",pod=\"model-0\"} %d\n"
			assertOutstandingMetrics(t, registry, fmt.Sprintf(sample, 0))

			ctx := types.NewRoutingContext(context.Background(), "least-request", "model", "", "req-1", "")
			defer ctx.Delete()
			ctx.SetTargetPod(pod)
			term := cache.AddRequestCount(ctx, "req-1", "model")

			// The log/cache slot may contain a global estimate. The collector must read
			// the local request ledger even without Redis or an engine metrics fetch.
			metaPod, ok := cache.metaPods.Load("inference/model-0")
			require.True(t, ok)
			metaPod.Metrics.Store(metrics.RealtimeNumRequestsRunning, MetricRecord{
				Value:     &metrics.SimpleMetricValue{Value: 99},
				Timestamp: time.Now(),
			})
			cache.gatewaySnapshotCache.Store(map[string][]map[string]string{
				"inference/model-0": {{"gateway_instance_id": "coord-b", "requests_running": "20"}},
			})
			assertOutstandingMetrics(t, registry, fmt.Sprintf(sample, 1))

			switch completion {
			case "count":
				cache.DoneRequestCount(ctx, "req-1", "model", term)
			case "trace":
				cache.DoneRequestTrace(ctx, "req-1", "model", 10, 20, term)
			case "nil-context":
				cache.DoneRequestCount(nil, "req-1", "model", term)
			}
			cache.DoneRequestCount(ctx, "req-1", "model", term)
			assertOutstandingMetrics(t, registry, fmt.Sprintf(sample, 0))
		})
	}
}

func TestPodOutstandingCollector_PodDeletionAndRecreation(t *testing.T) {
	cache := NewForTest()
	cache.enableTracing = false
	pod := requestTrackerTestPod("model-0", "inference", "model", "10.0.0.1")
	cache.addPod(pod)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPodOutstandingCollector(cache, "coord-a"))
	sample := "gateway_pod_outstanding_requests{gateway_pod=\"coord-a\",model=\"model\",namespace=\"inference\",pod=\"model-0\"} %d\n"
	ctx := types.NewRoutingContext(context.Background(), "least-request", "model", "", "req-1", "")
	defer ctx.Delete()
	ctx.SetTargetPod(pod)
	term := cache.AddRequestCount(ctx, "req-1", "model")
	assertOutstandingMetrics(t, registry, fmt.Sprintf(sample, 1))

	cache.deletePod(pod)
	assertOutstandingMetrics(t, registry, "")
	cache.addPod(requestTrackerTestPod("model-0", "inference", "model", "10.0.0.2"))
	cache.DoneRequestCount(ctx, "req-1", "model", term)
	assertOutstandingMetrics(t, registry, fmt.Sprintf(sample, 0))

	cache.addPod(requestTrackerTestPod("model-0", "other", "model", "10.0.0.3"))
	assertOutstandingMetrics(t, registry, fmt.Sprintf(sample, 0)+
		"gateway_pod_outstanding_requests{gateway_pod=\"coord-a\",model=\"model\",namespace=\"other\",pod=\"model-0\"} 0\n")
}

func TestPodOutstandingCollector_GatewayReplicas(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	for _, gateway := range []struct {
		name  string
		count int32
	}{{"coord-a", 3}, {"coord-b", 5}} {
		cache := NewForTest()
		cache.addPod(requestTrackerTestPod("model-0", "inference", "model", "10.0.0.1"))
		pod, ok := cache.metaPods.Load("inference/model-0")
		require.True(t, ok)
		atomic.StoreInt32(&pod.runningRequests, gateway.count)
		registry.MustRegister(newPodOutstandingCollector(cache, gateway.name))
	}
	assertOutstandingMetrics(t, registry,
		"gateway_pod_outstanding_requests{gateway_pod=\"coord-a\",model=\"model\",namespace=\"inference\",pod=\"model-0\"} 3\n"+
			"gateway_pod_outstanding_requests{gateway_pod=\"coord-b\",model=\"model\",namespace=\"inference\",pod=\"model-0\"} 5\n")
}

func TestPodOutstandingCollector_ConcurrentRequestsAndScrapes(t *testing.T) {
	cache := NewForTest()
	cache.enableTracing = false
	pod := requestTrackerTestPod("model-0", "inference", "model", "10.0.0.1")
	cache.addPod(pod)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPodOutstandingCollector(cache, "coord-a"))
	const workers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := 0; i < 100; i++ {
				id := fmt.Sprintf("%d-%d", worker, i)
				ctx := types.NewRoutingContext(context.Background(), "least-request", "model", "", id, "")
				ctx.SetTargetPod(pod)
				term := cache.AddRequestCount(ctx, id, "model")
				cache.DoneRequestCount(ctx, id, "model", term)
				ctx.Delete()
			}
		}(worker)
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				families, err := registry.Gather()
				if !assert.NoError(t, err) {
					return
				}
				for _, family := range families {
					for _, metric := range family.Metric {
						assert.GreaterOrEqual(t, metric.GetGauge().GetValue(), float64(0))
						assert.LessOrEqual(t, metric.GetGauge().GetValue(), float64(workers))
					}
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	assertOutstandingMetrics(t, registry,
		"gateway_pod_outstanding_requests{gateway_pod=\"coord-a\",model=\"model\",namespace=\"inference\",pod=\"model-0\"} 0\n")
}

func TestInitPodOutstandingCollector_WrappedRegisterer(t *testing.T) {
	for _, tt := range []struct {
		name    string
		labels  prometheus.Labels
		wantErr bool
	}{
		{
			name:   "non-conflicting label",
			labels: prometheus.Labels{"cluster": "cluster-a"},
		},
		{
			name:    "gateway pod label",
			labels:  prometheus.Labels{"pod": "coord-a"},
			wantErr: true,
		},
		{
			name:    "gateway namespace label",
			labels:  prometheus.Labels{"namespace": "aibrix-system"},
			wantErr: true,
		},
		{
			name:    "gateway namespace and pod labels",
			labels:  prometheus.Labels{"namespace": "aibrix-system", "pod": "coord-a"},
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewForTest()
			cache.addPod(requestTrackerTestPod("model-0", "inference", "model", "10.0.0.1"))
			registry := prometheus.NewPedanticRegistry()
			registerer := prometheus.WrapRegistererWith(tt.labels, registry)

			err := initPodOutstandingCollector(cache, registerer, nil)
			if tt.wantErr {
				// Registerer const labels cannot duplicate the collector's inference
				// namespace or pod label; scrape-time honor_labels cannot resolve this.
				require.ErrorContains(t, err, "duplicate label names in constant and variable labels")
				assertOutstandingMetrics(t, registry, "")
				return
			}
			require.NoError(t, err)
			assertOutstandingMetrics(t, registry, fmt.Sprintf(
				"gateway_pod_outstanding_requests{cluster=\"cluster-a\",gateway_pod=%q,model=\"model\",namespace=\"inference\",pod=\"model-0\"} 0\n", gatewayPodName))
		})
	}
}

func TestInitPodOutstandingCollector_RegistrationLifecycle(t *testing.T) {
	cache := NewForTest()
	cache.addPod(requestTrackerTestPod("model-0", "inference", "model", "10.0.0.1"))
	registry := prometheus.NewPedanticRegistry()
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopCollector := func() { stopOnce.Do(func() { close(stop) }) }
	t.Cleanup(stopCollector)
	require.NoError(t, initPodOutstandingCollector(cache, registry, stop))
	require.Error(t, initPodOutstandingCollector(cache, registry, nil), "duplicate registration must be reported")
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)

	stopCollector()
	require.Eventually(t, func() bool {
		families, err := registry.Gather()
		return err == nil && len(families) == 0
	}, time.Second, time.Millisecond)
	// A stopped store must not leave a collector pointing at its old ledger.
	require.NoError(t, initPodOutstandingCollector(NewForTest(), registry, nil))
}
