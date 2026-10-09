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
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func TestMetricsWorkerScrapesIPv6(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	requests := make(chan string, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Host + r.URL.Path
		// An empty exposition is sufficient to exercise the worker's real HTTP transport.
		w.WriteHeader(http.StatusOK)
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	pod := newReadyMetricsPod("ipv6-metrics", "ipv6-uid")
	pod.Status.PodIP = host
	pod.Labels[MetricPortLabel] = port
	store := &Store{engineMetricsFetcher: metrics.NewEngineMetricsFetcherWithConfig(metrics.EngineMetricsFetcherConfig{Timeout: time.Second})}
	store.metaPods.Store(utils.GeneratePodKey(pod.Namespace, pod.Name), pod)
	jobs := make(chan *Pod, 1)
	jobs <- pod
	close(jobs)
	store.worker(jobs)
	select {
	case request := <-requests:
		require.Equal(t, listener.Addr().String()+"/metrics", request)
	default:
		t.Fatal("worker did not reach the IPv6 metrics endpoint")
	}
}

type addressPrometheusAPI struct {
	prometheusv1.API
	queries []string
}

func (api *addressPrometheusAPI) Query(_ context.Context, query string, _ time.Time, _ ...prometheusv1.Option) (model.Value, prometheusv1.Warnings, error) {
	api.queries = append(api.queries, query)
	return nil, nil, errors.New("stop after capturing query")
}

func TestPromQLUsesIPv6InstanceLabel(t *testing.T) {
	pod := newReadyMetricsPod("ipv6-promql", "ipv6-uid")
	pod.Status.PodIP = "2001:db8::1"
	pod.Labels[MetricPortLabel] = "8000"
	pod.Models.Store("model", "model")
	api := &addressPrometheusAPI{}
	store := &Store{prometheusApi: api}
	require.Error(t, store.updateMetricFromPromQL(context.Background(), pod))
	require.NotEmpty(t, api.queries)
	for _, query := range api.queries {
		require.True(t, strings.Contains(query, "[2001:db8::1]:8000"), "query must target the bracketed instance: %s", query)
	}
}
