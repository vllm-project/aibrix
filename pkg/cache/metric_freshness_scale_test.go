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
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// Opt-in collection experiment, not a serving or Kubernetes benchmark. Uses
// the actual bounded queue, scheduler and HTTP workers against a fake engine.
// Run alone with AIBRIX_TEST_METRIC_FRESHNESS_SCALE=1 and -parallel 9.
func TestMetricFreshnessCollectionScale(t *testing.T) {
	if os.Getenv("AIBRIX_TEST_METRIC_FRESHNESS_SCALE") != "1" {
		t.Skip("opt-in collection scale experiment")
	}
	require.Equal(t, time.Second, podMetricRefreshInterval, "run with default refresh interval")
	require.Equal(t, 5*time.Second, podMetricsFetchTimeout, "run with default fetch timeout")
	require.Equal(t, 6*time.Second, RoutingMetricStaleThreshold(), "run with default freshness threshold")
	for _, count := range []int{1000, 3000, 5000} {
		for _, latency := range []time.Duration{20 * time.Millisecond, 50 * time.Millisecond, 200 * time.Millisecond} {
			t.Run(fmt.Sprintf("pods%d_latency%dms", count, latency/time.Millisecond), func(t *testing.T) {
				t.Parallel()
				var completed atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					time.Sleep(latency)
					fmt.Fprint(w, "# TYPE vllm:kv_cache_usage_perc gauge\nvllm:kv_cache_usage_perc{model_name=\"model\"} 0.1\n")
					completed.Add(1)
				}))
				defer server.Close()
				_, port, err := net.SplitHostPort(server.Listener.Addr().String())
				require.NoError(t, err)
				c := &Store{engineMetricsFetcher: metrics.NewEngineMetricsFetcher(), podMetricsJobs: make(chan *Pod, 100)}
				pods := make([]*Pod, count)
				for i := range pods {
					name := fmt.Sprintf("scale-%d-%d-%d", count, latency/time.Millisecond, i)
					pod := newReadyMetricsPod(name, name)
					pod.Status.PodIP = "127.0.0.1"
					pod.Labels[MetricPortLabel] = port
					c.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
					pods[i] = pod
				}
				var workers sync.WaitGroup
				for i := 0; i < 10; i++ {
					workers.Add(1)
					go func() { defer workers.Done(); c.worker(c.podMetricsJobs) }()
				}
				stop := make(chan struct{})
				producerDone := make(chan struct{})
				go func() {
					defer close(producerDone)
					ticker := time.NewTicker(podMetricRefreshInterval)
					defer ticker.Stop()
					c.updatePodMetrics()
					for {
						select {
						case <-stop:
							return
						case <-ticker.C:
							c.updatePodMetrics()
						}
					}
				}()
				defer func() { close(stop); <-producerDone; close(c.podMetricsJobs); workers.Wait() }()
				// Two ideal collection cycles for warmup, followed by at least
				// one ideal cycle of observations. Bounded windows do not prove
				// convergence; report halves separately to expose drift.
				idealCycle := time.Duration(count) * latency / 10
				warmup := max(10*time.Second, 2*idealCycle)
				observe := max(10*time.Second, idealCycle)
				time.Sleep(warmup)
				before := completed.Load()
				previous := make([]bool, count)
				var missing, stale, flips [2]int64
				var samples [2]int64
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				start := time.Now()
				ticks := int(observe / time.Second)
				for sample := 0; sample < ticks; sample++ {
					now := <-ticker.C
					half := 0
					if sample >= ticks/2 {
						half = 1
					}
					for i, pod := range pods {
						record, e := c.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.KVCacheUsagePerc)
						fresh := e == nil && now.Sub(record.Timestamp) <= RoutingMetricStaleThreshold()
						if e != nil {
							missing[half]++
						} else if !fresh {
							stale[half]++
						}
						if sample > 0 && fresh != previous[i] {
							flips[half]++
						}
						previous[i] = fresh
						samples[half]++
					}
				}
				elapsed := time.Since(start).Seconds()
				t.Logf("collection pods=%d latency=%s workers=10 queue=100 refresh=%s threshold=%s warmup=%s observe=%.2fs completed_per_s=%.2f", count, latency, podMetricRefreshInterval, RoutingMetricStaleThreshold(), warmup, elapsed, float64(completed.Load()-before)/elapsed)
				for half := range 2 {
					t.Logf("half=%d missing=%.4f stale=%.4f unscorable=%.4f eligibility_flips_per_pod_sample=%.4f samples=%d", half+1, float64(missing[half])/float64(samples[half]), float64(stale[half])/float64(samples[half]), float64(missing[half]+stale[half])/float64(samples[half]), float64(flips[half])/float64(samples[half]), samples[half])
				}
				require.Greater(t, completed.Load(), int64(0), "HTTP collection must actually run")
			})
		}
	}
}
