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

package least

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

type kvRecordCache struct {
	cache.Cache
	records      map[string]cache.MetricRecord
	lookupErrors map[string]error
}

func (c *kvRecordCache) GetMetricRecordByPod(_, _, metric string) (cache.MetricRecord, error) {
	return cache.MetricRecord{}, &cache.MetricNotFoundError{MetricName: metric}
}
func (c *kvRecordCache) GetMetricRecordByPodModel(pod, _, _, metric string) (cache.MetricRecord, error) {
	if err := c.lookupErrors[pod+"/"+metric]; err != nil {
		return cache.MetricRecord{}, err
	}
	record, ok := c.records[pod+"/"+metric]
	if !ok {
		return cache.MetricRecord{}, &cache.MetricNotFoundError{MetricName: metric}
	}
	return record, nil
}
func TestKVFreshnessScoringAndFallback(t *testing.T) {
	now := time.Unix(1000, 0)
	threshold := 5 * time.Second
	a := newPod("a", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	b := newPod("b", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"})
	base := cache.NewWithPodsModelMetricsForTest([]*v1.Pod{a, b}, "m1", map[string]map[string]metrics.MetricValue{})
	record := func(v float64, age time.Duration) cache.MetricRecord {
		return cache.MetricRecord{Value: &metrics.SimpleMetricValue{Value: v}, Timestamp: now.Add(-age)}
	}
	for _, tc := range []struct {
		name string
		gpu  *cache.MetricRecord
		cpu  *cache.MetricRecord
		want bool
	}{
		{"never reported CPU", ptrRecord(record(0.1, 0)), nil, true},
		{"exact threshold", ptrRecord(record(0.1, threshold)), nil, true},
		{"CPU exact threshold", ptrRecord(record(0.1, 0)), ptrRecord(record(0.1, threshold)), true},
		// A collector may finish after ScoreAll captures now; that fresh record is valid.
		{"concurrent fresh update", ptrRecord(record(0.1, -time.Nanosecond)), nil, true},
		{"empty GPU record", &cache.MetricRecord{Timestamp: now}, nil, false},
		{"empty CPU record", ptrRecord(record(0.1, 0)), &cache.MetricRecord{Timestamp: now}, false},
		{"CPU unknown timestamp", ptrRecord(record(0.1, 0)), &cache.MetricRecord{Value: &metrics.SimpleMetricValue{Value: 0.1}}, false},
		{"GPU stale", ptrRecord(record(0.1, threshold+time.Nanosecond)), nil, false},
		{"CPU stale", ptrRecord(record(0.1, 0)), ptrRecord(record(0.1, threshold+time.Nanosecond)), false},
		{"fresh CPU", ptrRecord(record(0.1, 0)), ptrRecord(record(0.2, 0)), true},
		{"GPU missing", nil, nil, false},
		{"unknown timestamp", &cache.MetricRecord{Value: &metrics.SimpleMetricValue{Value: 0.1}}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &kvRecordCache{Cache: base, records: map[string]cache.MetricRecord{"b/" + metrics.KVCacheUsagePerc: record(0.4, 0)}}
			if tc.gpu != nil {
				c.records["a/"+metrics.KVCacheUsagePerc] = *tc.gpu
			}
			if tc.cpu != nil {
				c.records["a/"+metrics.CPUCacheUsagePerc] = *tc.cpu
			}
			r := leastKvCacheRouter{cache: c, now: func() time.Time { return now }, staleThreshold: threshold}
			ctx := types.NewRoutingContext(context.Background(), "test", "m1", "input", "req", "")
			list := &utils.PodArray{Pods: []*v1.Pod{a, b}}
			scores, scored, err := r.ScoreAll(ctx, list)
			require.NoError(t, err)
			pods := list.All()
			for i, p := range pods {
				if p.Name == "a" {
					require.Equal(t, tc.want, scored[i])
					if tc.want {
						require.Less(t, scores[i], 0.4)
					}
				}
			}
			route := func() (string, error) {
				freshCtx := types.NewRoutingContext(context.Background(), "test", "m1", "input", "req", "")
				return r.Route(freshCtx, list)
			}
			target, err := route()
			require.NoError(t, err)
			want := "2.2.2.2:8000"
			if tc.want {
				want = "1.1.1.1:8000"
			}
			require.Equal(t, want, target)
			// When every record ages out, Route keeps the existing random fallback.
			r.now = func() time.Time { return now.Add(time.Minute) }
			target, err = route()
			require.NoError(t, err)
			require.Contains(t, []string{"1.1.1.1:8000", "2.2.2.2:8000"}, target)
			// A successful sample recovers A without replacing the router.
			c.records["a/"+metrics.KVCacheUsagePerc] = record(0.1, 0)
			if tc.cpu != nil {
				c.records["a/"+metrics.CPUCacheUsagePerc] = record(0.1, 0)
			}
			r.now = func() time.Time { return now }
			target, err = route()
			require.NoError(t, err)
			require.Equal(t, "1.1.1.1:8000", target)
		})
	}
}
func ptrRecord(r cache.MetricRecord) *cache.MetricRecord { return &r }

// A custom value-only Cache remains usable without implementing the extension.
func TestKVFreshnessValueOnlyCacheCompatibility(t *testing.T) {
	pod := newPod("p", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	store := cache.NewWithPodsModelMetricsForTest([]*v1.Pod{pod}, "m1", map[string]map[string]metrics.MetricValue{"p": {metrics.KVCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.2}}})
	valueOnly := struct{ cache.Cache }{Cache: store}
	_, hasRecords := any(valueOnly).(cache.MetricRecordCache)
	require.False(t, hasRecords)
	router := newLeastKvCacheRouter(valueOnly)
	ctx := types.NewRoutingContext(context.Background(), "test", "m1", "input", "req", "")
	scores, scored, err := router.ScoreAll(ctx, podsFromCache(store))
	require.NoError(t, err)
	require.Equal(t, []bool{true}, scored)
	require.Equal(t, []float64{0.2}, scores)
}

// Only an absent CPU metric is optional, not a failed cache lookup.
func TestKVFreshnessCPULookupErrors(t *testing.T) {
	now := time.Unix(1000, 0)
	pod := newPod("p", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"wrapped missing metric", fmt.Errorf("lookup: %w", &cache.MetricNotFoundError{MetricName: metrics.CPUCacheUsagePerc}), true},
		{"cache unavailable", errors.New("cache unavailable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &kvRecordCache{records: map[string]cache.MetricRecord{
				"p/" + metrics.KVCacheUsagePerc: {Value: &metrics.SimpleMetricValue{Value: 0.2}, Timestamp: now},
			}, lookupErrors: map[string]error{"p/" + metrics.CPUCacheUsagePerc: tc.err}}
			r := leastKvCacheRouter{cache: c, now: func() time.Time { return now }, staleThreshold: time.Second}
			ctx := types.NewRoutingContext(context.Background(), "test", "m1", "input", "req", "")
			scores, scored, err := r.ScoreAll(ctx, &utils.PodArray{Pods: []*v1.Pod{pod}})
			require.NoError(t, err)
			require.Equal(t, []bool{tc.want}, scored)
			if tc.want {
				require.Equal(t, []float64{0.2}, scores)
			}
		})
	}
}

// Exercise real Store timestamps and writes, without overriding record getters.
func TestKVFreshnessStoreIntegrationAndConcurrentUpdates(t *testing.T) {
	pod := newPod("p", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	values := func(v float64) map[string]map[string]metrics.MetricValue {
		return map[string]map[string]metrics.MetricValue{"p": {metrics.KVCacheUsagePerc: &metrics.SimpleMetricValue{Value: v}}}
	}
	store := cache.NewWithPodsModelMetricsForTest([]*v1.Pod{pod}, "m1", values(0.2))
	record, err := store.GetMetricRecordByPodModel(pod.Name, pod.Namespace, "m1", metrics.KVCacheUsagePerc)
	require.NoError(t, err)
	threshold := time.Second
	now := record.Timestamp.Add(threshold)
	router := leastKvCacheRouter{cache: store, now: func() time.Time { return now }, staleThreshold: threshold}
	ctx := types.NewRoutingContext(context.Background(), "test", "m1", "input", "req", "")
	list := &utils.PodArray{Pods: []*v1.Pod{pod}}
	_, scored, err := router.ScoreAll(ctx, list)
	require.NoError(t, err)
	require.Equal(t, []bool{true}, scored)
	now = now.Add(time.Nanosecond)
	_, scored, err = router.ScoreAll(ctx, list)
	require.NoError(t, err)
	require.Equal(t, []bool{false}, scored)
	cache.InitWithPodsModelMetrics(store, values(0.4))
	now = time.Now()
	scores, scored, err := router.ScoreAll(ctx, list)
	require.NoError(t, err)
	require.Equal(t, []bool{true}, scored)
	require.Equal(t, []float64{0.4}, scores)
	// now stays fixed while successful writes advance timestamps. Race runs
	// exercise the atomic value/timestamp pair, including negative age reads.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			cache.InitWithPodsModelMetrics(store, values(float64(i%2)*0.2+0.2))
		}
	}()
	defer wg.Wait()
	for i := 0; i < 1000; i++ {
		scores, scored, err := router.ScoreAll(ctx, list)
		require.NoError(t, err)
		require.Equal(t, []bool{true}, scored)
		require.Contains(t, []float64{0.2, 0.4}, scores[0])
	}
}

type countingKVRecords struct {
	*kvRecordCache
	reads []string
}

func (c *countingKVRecords) GetMetricRecordByPodModel(pod, ns, model, metric string) (cache.MetricRecord, error) {
	c.reads = append(c.reads, metric)
	return c.kvRecordCache.GetMetricRecordByPodModel(pod, ns, model, metric)
}

func TestStaleGPUSkipsCPURead(t *testing.T) {
	now := time.Unix(1000, 0)
	pod := newPod("a", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	base := cache.NewWithPodsModelMetricsForTest([]*v1.Pod{pod}, "m1", map[string]map[string]metrics.MetricValue{})
	c := &countingKVRecords{kvRecordCache: &kvRecordCache{Cache: base, records: map[string]cache.MetricRecord{
		"a/" + metrics.KVCacheUsagePerc: {Value: &metrics.SimpleMetricValue{Value: 0.9}, Timestamp: now.Add(-2 * time.Second)},
	}}}
	router := leastKvCacheRouter{cache: c, now: func() time.Time { return now }, staleThreshold: time.Second}
	ctx := types.NewRoutingContext(context.Background(), "test", "m1", "input", "req", "")
	_, scored, err := router.ScoreAll(ctx, &utils.PodArray{Pods: []*v1.Pod{pod}})
	require.NoError(t, err)
	require.Equal(t, []bool{false}, scored)
	require.Equal(t, []string{metrics.KVCacheUsagePerc}, c.reads)
}
