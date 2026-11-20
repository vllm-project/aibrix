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
package cache_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

func TestMetricRecordHTTPWorkerToRouter(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 1 {
			time.Sleep(4 * time.Second)
		}
		fmt.Fprintf(w, "# TYPE vllm:kv_cache_usage_perc gauge\nvllm:kv_cache_usage_perc{model_name=\"model\"} %.1f\n", 0.2+float64(n%2)*0.2)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	store, pod, scrape := cache.MetricRecordWorkerForTest(port)
	started := time.Now()
	scrape()
	record, err := store.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "model", metrics.KVCacheUsagePerc)
	require.NoError(t, err)
	require.GreaterOrEqual(t, record.Timestamp.Sub(started), 4*time.Second, "stamp is scrape completion, not start")
	router := least.NewLeastKvCacheScorer(store)
	ctx := types.NewRoutingContext(context.Background(), "test", "model", "input", "req", "")
	list := &utils.PodArray{Pods: []*v1.Pod{pod}}
	scores, scored, err := router.ScoreAll(ctx, list)
	require.NoError(t, err)
	require.Equal(t, []bool{true}, scored)
	require.Equal(t, []float64{0.4}, scores)
	// Real HTTP workers write into the same Store while the real scorer reads.
	var writers sync.WaitGroup
	writers.Add(1)
	done := make(chan struct{})
	go func() {
		defer writers.Done()
		defer close(done)
		for i := 0; i < 100; i++ {
			scrape()
		}
	}()
	defer writers.Wait()
	for {
		scores, scored, err = router.ScoreAll(ctx, list)
		require.NoError(t, err)
		require.Equal(t, []bool{true}, scored)
		require.Contains(t, []float64{0.2, 0.4}, scores[0])
		select {
		case <-done:
			require.Equal(t, int64(101), requests.Load())
			return
		default:
		}
	}
}
