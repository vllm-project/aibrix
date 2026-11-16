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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// BenchmarkPodMetricsScrape runs one metrics worker scrape of a vLLM v1 /metrics
// response, which the gateway repeats for every ready pod on each refresh.
func BenchmarkPodMetricsScrape(b *testing.B) {
	payload, err := os.ReadFile("../metrics/testdata/vllm_metrics.txt")
	require.NoError(b, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(b, err)

	pod := newReadyMetricsPod("bench", "uid-bench")
	pod.Status.PodIP = "127.0.0.1"
	pod.Labels[MetricPortLabel] = port
	c := &Store{engineMetricsFetcher: metrics.NewEngineMetricsFetcher()}
	c.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	scrape := func() {
		jobs := make(chan *Pod, 1)
		jobs <- pod
		close(jobs)
		c.worker(jobs)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scrape()
	}
}
