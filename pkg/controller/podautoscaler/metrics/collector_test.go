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
	"fmt"
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
	// erroring pods return an immediate non-timeout error (e.g. an HTTP 500 or a parse failure),
	// as opposed to unreachable pods, which block until the collection deadline.
	erroring map[string]bool
	value    float64
}

func (f *reachabilityFetcher) FetchPodMetrics(ctx context.Context, pod corev1.Pod, _ autoscalingv1alpha1.MetricSource) (float64, error) {
	f.mu.Lock()
	if f.scraped == nil {
		f.scraped = map[string]int{}
	}
	f.scraped[pod.Name]++
	block := f.unreachable[pod.Name]
	fail := f.erroring[pod.Name]
	f.mu.Unlock()
	if fail {
		return 0, fmt.Errorf("pod %s returned an error", pod.Name)
	}
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

// readyPodForScrape is a Running pod with an IP and a Ready=True condition, so utils.IsPodReady
// returns true and collectFromPods orders it ahead of not-ready pods when acquiring slots.
func readyPodForScrape(name, ip string) corev1.Pod {
	pod := podForScrape(name, ip)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	return pod
}

// delayedFetcher returns value[pod] after delay[pod], respecting the context: if the context is
// done before the delay elapses it returns the context error (the scrape is dropped). It models a
// pod that answers slowly but correctly, to show a merely-slow pod is collected under the single
// overall deadline rather than cut by a per-pod bound.
type delayedFetcher struct {
	value map[string]float64
	delay map[string]time.Duration
}

func (f *delayedFetcher) FetchPodMetrics(ctx context.Context, pod corev1.Pod, _ autoscalingv1alpha1.MetricSource) (float64, error) {
	if d := f.delay[pod.Name]; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return f.value[pod.Name], nil
}

// TestCollectFromPodsCollectsSlowPodWithinDeadline covers the single-deadline design (option 1 on
// #3593, per googs1025): with no per-pod sub-deadline, a pod that answers slowly but within the
// overall budget is still collected, not dropped. This is googs1025's scenario (one pod returns 0
// immediately, the other returns 200 after a delay that is under the budget), scaled down for
// speed. Both samples are collected, so the per-pod value is their mean and the pass can scale up
// -- a 3s per-pod cut would instead drop the slow pod and bias the value low.
func TestCollectFromPodsCollectsSlowPodWithinDeadline(t *testing.T) {
	pods := []corev1.Pod{
		readyPodForScrape("fast-0", "10.0.0.1"),   // returns 0 immediately
		readyPodForScrape("slow-200", "10.0.0.2"), // returns 200 after a delay under the budget
	}
	fetcher := &delayedFetcher{
		value: map[string]float64{"fast-0": 0, "slow-200": 200},
		delay: map[string]time.Duration{"slow-200": 60 * time.Millisecond},
	}
	bounds := collectBounds{overallTimeout: 500 * time.Millisecond, maxConcurrency: 16}

	snapshot, err := collectFromPods(context.Background(), podCollectionSpec(pods), fetcher, bounds)

	require.NoError(t, err)
	assert.ElementsMatch(t, []float64{0, 200}, snapshot.Values, "the slow pod answers within the budget, so both samples are collected")
	assert.Equal(t, 0, snapshot.DroppedReadyPods, "no Ready pod was dropped")
	assert.NoError(t, snapshot.Error)
}

// TestCollectFromPodsOverallDeadlineCapsCollection pins the overall bound: with an unbounded
// parent context, the whole call must return within the overall timeout even though every pod
// blocks. Kills the mutant that replaces the overall WithTimeout with WithCancel (the blockers
// would then run until the parent context, which here never cancels).
func TestCollectFromPodsOverallDeadlineCapsCollection(t *testing.T) {
	pods := []corev1.Pod{
		podForScrape("bh-1", "10.0.0.1"),
		podForScrape("bh-2", "10.0.0.2"),
	}
	fetcher := &reachabilityFetcher{unreachable: map[string]bool{"bh-1": true, "bh-2": true}}
	bounds := collectBounds{overallTimeout: 50 * time.Millisecond, maxConcurrency: 16}

	start := time.Now()
	snapshot, err := collectFromPods(context.Background(), podCollectionSpec(pods), fetcher, bounds)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Empty(t, snapshot.Values)
	assert.Error(t, snapshot.Error, "all pods blocked => total failure")
	// Bounded by the 50ms overall deadline, not the 10s per-pod bound.
	assert.Less(t, elapsed, 2*time.Second, "the overall deadline must bound the whole collection")
}

// TestCollectFromPodsReadyPodsScrapedUnderUnreachableCrowd covers the Ready-first ordering: with
// far more unreachable pods than concurrency slots, listed ahead of the reachable Ready pods, the
// Ready pods must still be scraped every time (they get the slots first), so the pass never
// degrades to a total failure. Run repeatedly because the pre-ordering behavior was scheduler-
// dependent and only failed intermittently.
func TestCollectFromPodsReadyPodsScrapedUnderUnreachableCrowd(t *testing.T) {
	pods := make([]corev1.Pod, 0, 10)
	unreachable := map[string]bool{}
	for i := 0; i < 8; i++ { // unreachable + not-ready, listed first
		name := fmt.Sprintf("bh-%d", i)
		pods = append(pods, podForScrape(name, fmt.Sprintf("10.0.1.%d", i)))
		unreachable[name] = true
	}
	pods = append(pods, readyPodForScrape("good-1", "10.0.2.1"), readyPodForScrape("good-2", "10.0.2.2"))
	fetcher := &reachabilityFetcher{unreachable: unreachable, value: 7}
	bounds := collectBounds{overallTimeout: 100 * time.Millisecond, maxConcurrency: 2}

	for run := 0; run < 20; run++ {
		snapshot, err := collectFromPods(context.Background(), podCollectionSpec(pods), fetcher, bounds)
		require.NoError(t, err)
		require.Len(t, snapshot.Values, 2, "run %d: both Ready pods must be collected despite the unreachable crowd", run)
		require.NoError(t, snapshot.Error, "run %d: Ready pods present => not a total failure", run)
	}
}

// TestCollectFromPodsClampsNonPositiveConcurrency guards the semaphore against a deadlock/panic:
// a non-positive maxConcurrency is clamped to 1, so collection still completes (serially).
func TestCollectFromPodsClampsNonPositiveConcurrency(t *testing.T) {
	pods := []corev1.Pod{podForScrape("good-1", "10.0.0.1"), podForScrape("good-2", "10.0.0.2")}
	fetcher := &reachabilityFetcher{value: 7}
	bounds := collectBounds{overallTimeout: time.Second, maxConcurrency: 0}

	snapshot, err := collectFromPods(context.Background(), podCollectionSpec(pods), fetcher, bounds)
	require.NoError(t, err)
	assert.Len(t, snapshot.Values, 2, "clamping concurrency to 1 must not deadlock; both pods are scraped")
	assert.NoError(t, snapshot.Error)
}

// TestCollectFromPodsCountsDroppedReadyPods: DroppedReadyPods counts only Ready pods whose scrape
// was dropped (timed out/failed) -- a not-Ready pod that fails is not counted, and a Ready pod
// that succeeds is not counted. The autoscaler uses this to optionally hold a scale-down.
func TestCollectFromPodsCountsDroppedReadyPods(t *testing.T) {
	pods := []corev1.Pod{
		readyPodForScrape("ready-dropped", "10.0.0.1"), // Ready + unreachable  -> counted
		podForScrape("notready-dropped", "10.0.0.2"),   // not Ready + unreachable -> not counted
		readyPodForScrape("ready-ok", "10.0.0.3"),      // Ready + reachable    -> not dropped
	}
	fetcher := &reachabilityFetcher{
		unreachable: map[string]bool{"ready-dropped": true, "notready-dropped": true},
		value:       5,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	snapshot, err := CollectMetrics(ctx, podCollectionSpec(pods), staticMetricFetcherFactory{fetcher: fetcher})
	require.NoError(t, err)
	assert.Equal(t, 1, snapshot.DroppedReadyPods, "only the Ready pod whose scrape was dropped is counted")
	assert.Equal(t, []float64{5}, snapshot.Values, "only the reachable Ready pod reported")
	assert.NoError(t, snapshot.Error, "partial success is not a total failure")
}

// TestCollectFromPodsCountsOnlyDeadlineDropsAsDroppedReady pins varungup90's narrowing on #3593:
// DroppedReadyPods counts only Ready pods dropped by the collection deadline, not Ready pods that
// returned their own error (a 500, a parse failure). Because a dropped Ready pod now holds a scale-
// down by default, a pod that persistently errors for another reason must not block scale-downs:
// its sample is skipped and the scale-down proceeds, as before this PR.
func TestCollectFromPodsCountsOnlyDeadlineDropsAsDroppedReady(t *testing.T) {
	pods := []corev1.Pod{
		readyPodForScrape("ready-timeout", "10.0.0.1"), // Ready + blocks past the deadline -> counted
		readyPodForScrape("ready-500", "10.0.0.2"),     // Ready + immediate error           -> not counted
		readyPodForScrape("ready-ok", "10.0.0.3"),      // Ready + reachable                 -> not dropped
	}
	fetcher := &reachabilityFetcher{
		unreachable: map[string]bool{"ready-timeout": true},
		erroring:    map[string]bool{"ready-500": true},
		value:       5,
	}
	bounds := collectBounds{overallTimeout: 100 * time.Millisecond, maxConcurrency: 16}

	snapshot, err := collectFromPods(context.Background(), podCollectionSpec(pods), fetcher, bounds)

	require.NoError(t, err)
	assert.Equal(t, 1, snapshot.DroppedReadyPods, "only the deadline-dropped Ready pod counts, not the one that returned a 500")
	assert.Equal(t, []float64{5}, snapshot.Values, "the reachable Ready pod reported")
	assert.NoError(t, snapshot.Error, "partial success is not a total failure")
}
