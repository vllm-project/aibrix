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

package metrics

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	patypes "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	corev1 "k8s.io/api/core/v1"
)

type recordingMetricFetcher struct {
	namespace string
}

func (f *recordingMetricFetcher) FetchPodMetrics(ctx context.Context, pod corev1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	f.namespace = pod.Namespace
	return 42, nil
}

type staticMetricFetcherFactory struct {
	fetcher MetricFetcher
}

func (f staticMetricFetcherFactory) For(source autoscalingv1alpha1.MetricSource) MetricFetcher {
	return f.fetcher
}

func TestCollectMetricsExternalUsesCollectionNamespace(t *testing.T) {
	fetcher := &recordingMetricFetcher{}
	spec := patypes.CollectionSpec{
		Namespace:  "tenant-a",
		TargetName: "worker",
		MetricName: "queue_depth",
		MetricSource: autoscalingv1alpha1.MetricSource{
			MetricSourceType: autoscalingv1alpha1.EXTERNAL,
			TargetMetric:     "queue_depth",
		},
		Timestamp: time.Now(),
	}

	snapshot, err := CollectMetrics(context.TODO(), spec, staticMetricFetcherFactory{fetcher: fetcher})

	require.NoError(t, err)
	assert.Equal(t, "tenant-a", fetcher.namespace)
	assert.Equal(t, []float64{42}, snapshot.Values)
}

// podForScrape builds a minimal Running pod with an IP, named so the fetcher can look it up.
func podForScrape(name, ip string) corev1.Pod {
	pod := corev1.Pod{}
	pod.Name = name
	pod.Namespace = "ns"
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = ip
	return pod
}

// reachabilityFetcher is a concurrency-safe fetcher: pods named in unreachable block until the
// context is cancelled (a pod that hangs out the per-pod/collection timeout), every other pod
// returns a value immediately. It records which pods it was asked to scrape.
type reachabilityFetcher struct {
	mu          sync.Mutex
	scraped     map[string]int
	unreachable map[string]bool
	value       float64
}

func (f *reachabilityFetcher) FetchPodMetrics(ctx context.Context, pod corev1.Pod, _ autoscalingv1alpha1.MetricSource) (float64, error) {
	f.mu.Lock()
	if f.scraped == nil {
		f.scraped = map[string]int{}
	}
	f.scraped[pod.Name]++
	block := f.unreachable[pod.Name]
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return f.value, nil
}

func (f *reachabilityFetcher) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scraped[name]
}

func podCollectionSpec(pods []corev1.Pod) patypes.CollectionSpec {
	return patypes.CollectionSpec{
		Namespace:  "ns",
		TargetName: "worker",
		MetricName: "num_requests_running",
		MetricSource: autoscalingv1alpha1.MetricSource{
			MetricSourceType: autoscalingv1alpha1.POD,
			TargetMetric:     "num_requests_running",
		},
		Pods:      pods,
		Timestamp: time.Now(),
	}
}

// TestCollectFromPodsReachableNotStarvedByUnreachable is the regression test for #3585:
// unreachable pods must not starve the reachable ones. Collection must still return the
// reachable pods' values (so a scale decision is possible) within the budget, and must not
// take the serial sum of the unreachable pods' timeouts.
func TestCollectFromPodsReachableNotStarvedByUnreachable(t *testing.T) {
	pods := []corev1.Pod{
		// Unreachable pods first: a serial scrape would spend the whole budget here and never
		// reach the healthy pods before the reconcile deadline.
		podForScrape("bh-1", "10.0.0.1"),
		podForScrape("bh-2", "10.0.0.2"),
		podForScrape("bh-3", "10.0.0.3"),
		// Reachable pods.
		podForScrape("good-1", "10.0.0.4"),
		podForScrape("good-2", "10.0.0.5"),
	}
	fetcher := &reachabilityFetcher{
		unreachable: map[string]bool{"bh-1": true, "bh-2": true, "bh-3": true},
		value:       7,
	}

	// A short overall budget both bounds the unreachable pods and keeps the test fast. The old
	// serial code would spend it all on bh-1 and never scrape good-1/good-2.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	snapshot, err := CollectMetrics(ctx, podCollectionSpec(pods), staticMetricFetcherFactory{fetcher: fetcher})
	elapsed := time.Since(start)

	require.NoError(t, err)
	// The reachable pods produced values despite the unreachable ones blocking.
	assert.Len(t, snapshot.Values, 2, "both reachable pods should be collected")
	for _, v := range snapshot.Values {
		assert.Equal(t, float64(7), v)
	}
	// Partial success => a scale decision is still possible (the aggregator rejects on a set Error).
	assert.NoError(t, snapshot.Error)
	// Reachable pods are scraped exactly once.
	assert.Equal(t, 1, fetcher.count("good-1"))
	assert.Equal(t, 1, fetcher.count("good-2"))
	// Collection returned on the budget, concurrently -- not the serial sum of the three
	// unreachable pods' timeouts.
	assert.Less(t, elapsed, 2*time.Second, "collection must be concurrent, not serial")
}

// TestCollectFromPodsAllUnreachable: when every pod fails to answer within the budget, the
// snapshot carries an Error so the aggregator treats the pass as a failed compute (no scaling),
// preserving the previous total-failure semantics.
func TestCollectFromPodsAllUnreachable(t *testing.T) {
	pods := []corev1.Pod{
		podForScrape("bh-1", "10.0.0.1"),
		podForScrape("bh-2", "10.0.0.2"),
	}
	fetcher := &reachabilityFetcher{unreachable: map[string]bool{"bh-1": true, "bh-2": true}}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	snapshot, err := CollectMetrics(ctx, podCollectionSpec(pods), staticMetricFetcherFactory{fetcher: fetcher})

	require.NoError(t, err)
	assert.Empty(t, snapshot.Values)
	assert.Error(t, snapshot.Error, "total collection failure must surface as a snapshot error")
}
