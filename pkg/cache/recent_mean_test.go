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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/metrics"
)

// seedMean1m records a histogram snapshot taken age ago, as an earlier scrape would have.
func seedMean1m(pod *Pod, model, metric string, sum, count float64, age time.Duration) {
	rateCalculator.mu.Lock()
	defer rateCalculator.mu.Unlock()
	key := rateHistoryKey(pod, model, RecentMeanMetricName(metric))
	rateCalculator.history[key] = append(rateCalculator.history[key],
		MetricSnapshot{Value: sum, Count: count, Timestamp: time.Now().Add(-age)})
}

func TestCalculateMean1m(t *testing.T) {
	const model, metric = "m", metrics.RequestQueueTimeSeconds
	hist := func(sum, count float64) *metrics.HistogramMetricValue {
		return &metrics.HistogramMetricValue{Sum: sum, Count: count}
	}

	t.Run("mean of the observations in the window", func(t *testing.T) {
		c, pod := &Store{}, namespacedTestPod("ns", "slow-pod")
		purgeRateHistory(t, pod)
		// 100000 requests queued 0.05 s each, then 300 queued 2 s each in the last minute.
		seedMean1m(pod, model, metric, 100000*0.05, 100000, time.Minute)

		mean, ok := c.calculateMean1m(pod, model, metric, hist(100000*0.05+300*2, 100300))

		require.True(t, ok)
		require.InDelta(t, 2.0, mean, 1e-9)
		// The lifetime mean of the same histogram has hardly moved.
		require.InDelta(t, 0.0558, hist(100000*0.05+300*2, 100300).GetMean(), 1e-4)
	})

	t.Run("no observation in the window", func(t *testing.T) {
		c, pod := &Store{}, namespacedTestPod("ns", "idle-pod")
		purgeRateHistory(t, pod)
		seedMean1m(pod, model, metric, 500, 1000, time.Minute)

		_, ok := c.calculateMean1m(pod, model, metric, hist(500, 1000))

		require.False(t, ok)
	})

	t.Run("too few observations in the window", func(t *testing.T) {
		// One 40 s request on a pod that is otherwise at 2 s must not stand for the
		// whole minute; five observations are enough to trust.
		for _, tc := range []struct {
			observations float64
			wantOK       bool
		}{{1, false}, {recentMeanMinSamples - 1, false}, {recentMeanMinSamples, true}} {
			c, pod := &Store{}, namespacedTestPod("ns", "quiet-pod")
			purgeRateHistory(t, pod)
			seedMean1m(pod, model, metric, 200, 100, time.Minute)

			mean, ok := c.calculateMean1m(pod, model, metric, hist(200+40*tc.observations, 100+tc.observations))

			require.Equal(t, tc.wantOK, ok, "%v observations", tc.observations)
			if ok {
				require.InDelta(t, 40.0, mean, 1e-9)
			}
			purgeRateHistoryNow(pod)
		}
	})

	t.Run("engine restart inside the window", func(t *testing.T) {
		// A young pod had served 100 requests at 0.05 s, restarted, and then served
		// 110 requests at 2 s inside the minute. Base-to-now alone would report
		// (220 - 5) / (110 - 100) = 21.5 s instead of 2 s.
		c, pod := &Store{}, namespacedTestPod("ns", "restarted-in-window")
		purgeRateHistory(t, pod)
		seedMean1m(pod, model, metric, 5, 100, time.Minute)
		seedMean1m(pod, model, metric, 6, 3, 30*time.Second) // counters started again

		_, ok := c.calculateMean1m(pod, model, metric, hist(220, 110))

		require.False(t, ok, "a restart inside the window falls back to the lifetime mean")
	})

	t.Run("growing counters across several snapshots are used", func(t *testing.T) {
		c, pod := &Store{}, namespacedTestPod("ns", "steady-pod")
		purgeRateHistory(t, pod)
		seedMean1m(pod, model, metric, 5, 100, time.Minute)
		seedMean1m(pod, model, metric, 105, 150, 30*time.Second)

		mean, ok := c.calculateMean1m(pod, model, metric, hist(205, 200))

		require.True(t, ok)
		require.InDelta(t, 2.0, mean, 1e-9) // (205 - 5) / (200 - 100)
	})

	t.Run("restart seen only at the current sample", func(t *testing.T) {
		c, pod := &Store{}, namespacedTestPod("ns", "restarted-just-now")
		purgeRateHistory(t, pod)
		seedMean1m(pod, model, metric, 50, 500, time.Minute)
		seedMean1m(pod, model, metric, 60, 520, 30*time.Second)

		_, ok := c.calculateMean1m(pod, model, metric, hist(1, 1))

		require.False(t, ok)
	})

	t.Run("too little history", func(t *testing.T) {
		c, pod := &Store{}, namespacedTestPod("ns", "new-pod")
		purgeRateHistory(t, pod)

		_, ok := c.calculateMean1m(pod, model, metric, hist(10, 20))
		require.False(t, ok, "first scrape")

		purgeRateHistoryNow(pod)
		seedMean1m(pod, model, metric, 10, 20, 6*time.Second)
		_, ok = c.calculateMean1m(pod, model, metric, hist(30, 40))
		require.False(t, ok, "6 s of history is under the 10 s minimum")
	})

	t.Run("histogram reset by an engine restart", func(t *testing.T) {
		c, pod := &Store{}, namespacedTestPod("ns", "restarted-pod")
		purgeRateHistory(t, pod)
		seedMean1m(pod, model, metric, 5000, 100000, time.Minute)

		_, ok := c.calculateMean1m(pod, model, metric, hist(30, 10))

		require.False(t, ok, "a reset falls back to the lifetime mean")
	})
}

func TestUpdateRecentMean(t *testing.T) {
	const model, metric = "m", metrics.RequestQueueTimeSeconds
	recentName := RecentMeanMetricName(metric)

	t.Run("stores the recent mean, then removes it when the window is empty", func(t *testing.T) {
		c, pod := &Store{}, namespacedTestPod("ns", "pod-a")
		purgeRateHistory(t, pod)
		seedMean1m(pod, model, metric, 100, 100, time.Minute)

		c.updateRecentMean(pod, model, metric, &metrics.HistogramMetricValue{Sum: 120, Count: 110})
		stored, ok := pod.ModelMetrics.Load(c.getPodModelMetricName(model, recentName))
		require.True(t, ok)
		require.InDelta(t, 2.0, stored.GetSimpleValue(), 1e-9)

		// A new history in which the count did not move: nothing to average.
		purgeRateHistoryNow(pod)
		seedMean1m(pod, model, metric, 120, 110, time.Minute)
		c.updateRecentMean(pod, model, metric, &metrics.HistogramMetricValue{Sum: 120, Count: 110})
		_, ok = pod.ModelMetrics.Load(c.getPodModelMetricName(model, recentName))
		require.False(t, ok, "a stale recent mean must not outlive its window")
	})

	t.Run("ignores histograms the routers do not score by", func(t *testing.T) {
		c, pod := &Store{}, namespacedTestPod("ns", "pod-b")
		purgeRateHistory(t, pod)
		seedMean1m(pod, model, metrics.TimeToFirstTokenSeconds, 100, 100, time.Minute)

		c.updateRecentMean(pod, model, metrics.TimeToFirstTokenSeconds, &metrics.HistogramMetricValue{Sum: 120, Count: 110})

		_, ok := pod.ModelMetrics.Load(c.getPodModelMetricName(model, RecentMeanMetricName(metrics.TimeToFirstTokenSeconds)))
		require.False(t, ok)
	})
}

func TestRecentHistogramMean(t *testing.T) {
	const model, metric = "m1", metrics.RequestPrefillTimeSeconds
	lifetime := &metrics.HistogramMetricValue{Sum: 10, Count: 100}
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "default"}},
	}
	c := NewWithPodsModelMetricsForTest(pods, model, map[string]map[string]metrics.MetricValue{
		"p1": {metric: lifetime},
		"p2": {metric: lifetime, RecentMeanMetricName(metric): &metrics.SimpleMetricValue{Value: 1.5}},
	})

	require.InDelta(t, 0.1, RecentHistogramMean(c, "p1", "default", model, metric, lifetime), 1e-9,
		"no recent mean: lifetime mean")
	require.InDelta(t, 1.5, RecentHistogramMean(c, "p2", "default", model, metric, lifetime), 1e-9,
		"recent mean available: it wins")
}

// purgeRateHistoryNow drops a pod's rate history immediately.
func purgeRateHistoryNow(pod *Pod) {
	rateCalculator.PurgeEntriesForPod(pod.Namespace, pod.Name)
}
