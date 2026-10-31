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

	"github.com/vllm-project/aibrix/pkg/metrics"
)

// vllmHistogramPage renders one vLLM-style cumulative histogram of name with the given
// totals, as an engine serves it on /metrics.
func vllmHistogramPage(name, model string, sum, count float64) string {
	return fmt.Sprintf("# TYPE vllm:%[1]s histogram\n"+
		"vllm:%[1]s_bucket{model_name=%[2]q,le=\"1.0\"} %[4]g\n"+
		"vllm:%[1]s_bucket{model_name=%[2]q,le=\"+Inf\"} %[4]g\n"+
		"vllm:%[1]s_sum{model_name=%[2]q} %[3]g\n"+
		"vllm:%[1]s_count{model_name=%[2]q} %[4]g\n", name, model, sum, count)
}

// The scrape worker is what feeds the routers their recent mean: if it stops calling
// updateRecentMean, or leaves a histogram out of recentMeanHistograms, the routers go
// back to the lifetime mean without any other test noticing.
func TestWorkerStoresRecentMeanOfLatencyHistograms(t *testing.T) {
	const model = "m"
	// The pod served 100,000 requests at 0.05 s, and in the last minute 300 more at 2 s.
	const lifetimeSum, lifetimeCount = 100000*0.05 + 300*2, 100300

	histograms := []string{
		metrics.RequestQueueTimeSeconds,
		metrics.RequestPrefillTimeSeconds,
		metrics.RequestDecodeTimeSeconds,
		metrics.RequestInferenceTimeSeconds,
	}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range histograms {
			_, _ = fmt.Fprint(w, vllmHistogramPage(name, model, lifetimeSum, lifetimeCount))
		}
	}))
	defer engine.Close()
	_, port, err := net.SplitHostPort(engine.Listener.Addr().String())
	require.NoError(t, err)

	pod := newReadyMetricsPod("worker-pod", "uid-worker-pod")
	pod.Status.PodIP = "127.0.0.1"
	pod.Labels[MetricPortLabel] = port
	purgeRateHistory(t, pod)
	for _, name := range histograms {
		// The previous scrape, a minute ago, had seen only the fast requests.
		seedMean1m(pod, model, name, 100000*0.05, 100000, time.Minute)
	}

	c := &Store{engineMetricsFetcher: metrics.NewEngineMetricsFetcher()}
	jobs := make(chan *Pod, 1)
	jobs <- pod
	close(jobs)
	c.worker(jobs)

	for _, name := range histograms {
		stored, ok := pod.ModelMetrics.Load(c.getPodModelMetricName(model, RecentMeanMetricName(name)))
		require.True(t, ok, "%s: the worker must store its recent mean", name)
		require.InDelta(t, 2.0, stored.GetSimpleValue(), 1e-9,
			"%s: recent mean, not the lifetime mean %.4f", name, lifetimeSum/lifetimeCount)
	}
}
