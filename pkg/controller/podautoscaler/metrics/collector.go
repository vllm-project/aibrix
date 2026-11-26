/*
Copyright 2024 The Aibrix Team.

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
	"strings"
	"sync"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// collectBounds controls how collectFromPods scrapes pods. It is a value passed in (not package
// globals) so tests can shrink the deadlines without mutating shared state, keeping them
// race-free and parallel-safe.
type collectBounds struct {
	// overallTimeout bounds the whole concurrent collection, and with no per-pod sub-deadline it is
	// also the only bound on any single scrape. It is kept well below the reconcile deadline
	// (DefaultReconcileTimeoutDuration, 10s) so that even a large batch of slow or unreachable pods
	// still returns with time to spare to act on the partial metrics. A pod that answers within the
	// budget is collected -- so a merely-slow pod (often the hottest one, under load) is not dropped
	// and does not bias the per-pod value low -- and only a scrape still outstanding when the budget
	// is spent is cut and recorded as timed out.
	//
	// Note this budget is per metric source. A PodAutoscaler with N POD sources collects them
	// serially (ComputeDesiredReplicas iterates MetricsSources), so in the worst case the budgets
	// sum to N*overallTimeout and can exceed the reconcile deadline. A shared budget across sources
	// is tracked as a follow-up (#3605); today each source gets the full overallTimeout.
	overallTimeout time.Duration
	// maxConcurrency bounds how many pods are scraped in parallel, keeping collection fast without
	// opening an unbounded number of connections against a large fleet. Values below 1 are clamped
	// to 1 (see collectFromPods), so the semaphore channel is always buffered.
	maxConcurrency int
}

// defaultCollectBounds is the production configuration used by CollectMetrics.
var defaultCollectBounds = collectBounds{
	overallTimeout: 6 * time.Second,
	maxConcurrency: 16,
}

// CollectMetrics gathers metrics for a scaling target using the factory pattern
func CollectMetrics(ctx context.Context, spec types.CollectionSpec, factory MetricFetcherFactory) (*types.MetricSnapshot, error) {
	fetcher := factory.For(spec.MetricSource)

	switch spec.MetricSource.MetricSourceType {
	case autoscalingv1alpha1.POD, autoscalingv1alpha1.RESOURCE:
		// POD scrapes over HTTP and RESOURCE via the metrics API both honor the context, so the
		// concurrent per-pod/overall sub-deadlines actually bound a slow or unreachable pod.
		return collectFromPods(ctx, spec, fetcher, defaultCollectBounds)
	case autoscalingv1alpha1.CUSTOM:
		// The Kubernetes custom-metrics client (GetForObject) takes no context, so a sub-deadline
		// would not cut a hung request and would only block the collection's WaitGroup. Keep CUSTOM
		// on the pre-existing serial path -- #3585 is about direct POD scrapes -- so this change
		// makes no claim it does not keep. Bounding the custom-metrics client is tracked separately.
		return collectFromPodsSerial(ctx, spec, fetcher)
	case autoscalingv1alpha1.EXTERNAL, autoscalingv1alpha1.DOMAIN:
		return collectFromExternal(ctx, spec, fetcher)
	default:
		return collectFromPods(ctx, spec, fetcher, defaultCollectBounds) // Default to pod collection
	}
}

// collectFromPodsSerial scrapes pods one at a time with no added sub-deadline, matching the
// pre-concurrency behavior. It is used for CUSTOM metrics, whose client ignores the context (so
// the concurrent path's deadlines are a no-op for it); the only bound is the caller's reconcile
// context. Error semantics match collectFromPods: only a total failure sets snapshot.Error.
func collectFromPodsSerial(ctx context.Context, spec types.CollectionSpec, fetcher MetricFetcher) (*types.MetricSnapshot, error) {
	var values []float64
	var collectErrors []error
	successCount := 0
	for _, pod := range spec.Pods {
		value, err := fetcher.FetchPodMetrics(ctx, pod, spec.MetricSource)
		if err != nil {
			collectErrors = append(collectErrors, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err))
			continue
		}
		values = append(values, value)
		successCount++
	}

	var finalErr error
	if len(collectErrors) > 0 && successCount == 0 {
		finalErr = fmt.Errorf("failed to collect metrics from all %d pods: %v", len(spec.Pods), combineErrors(collectErrors))
	}

	return &types.MetricSnapshot{
		Namespace:  spec.Namespace,
		TargetName: spec.TargetName,
		MetricName: spec.MetricName,
		Values:     values,
		Timestamp:  spec.Timestamp,
		Source:     string(spec.MetricSource.MetricSourceType),
		Error:      finalErr,
	}, nil
}

// scrapeOnePod scrapes one pod on the shared collection context, which the overall deadline
// bounds. There is no per-pod sub-deadline: a pod that answers within the budget is collected (so
// a merely-slow pod is not dropped), and a scrape still outstanding when the budget is spent is cut
// by the context. Pulled out so the deferred slot release lives at function scope rather than
// inside a select case.
func scrapeOnePod(ctx context.Context, sem chan struct{}, fetcher MetricFetcher, pod corev1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	defer func() { <-sem }()
	return fetcher.FetchPodMetrics(ctx, pod, source)
}

// collectFromPods handles pod-level metric collection. It scrapes the target's pods concurrently,
// with the whole collection bounded by a single overall deadline well below the reconcile budget.
// There is no per-pod sub-deadline, so a pod that answers within the budget is collected rather
// than cut; only scrapes still outstanding when the budget is spent are dropped.
//
// Previously it scraped every pod serially on the shared reconcile context, so a single
// unreachable pod -- one that eats the engine fetcher's full HTTP timeout plus 1s/2s/4s retry
// backoff -- could consume the entire 10s reconcile budget before any healthy pod was reached.
// On total failure with replicas within [min,max] the pass set ScalingActive=False and applied
// no scaling, i.e. scaling stalled during the very scale-ups/incidents where it matters most.
// Scraping concurrently under one overall deadline means slow or unreachable pods no longer
// starve the healthy ones, and partial results still drive a scale decision. Readiness is left
// to the caller's ReplicaState accounting, so the "scrape the pods you are given" contract and
// the downstream load math are unchanged -- readiness only decides scrape ORDER (see below).
func collectFromPods(ctx context.Context, spec types.CollectionSpec, fetcher MetricFetcher, bounds collectBounds) (*types.MetricSnapshot, error) {
	// A non-positive concurrency would make an unbuffered (or panicking) semaphore and deadlock,
	// since the receiver is only spawned after the send succeeds. Clamp to at least 1.
	concurrency := bounds.maxConcurrency
	if concurrency < 1 {
		concurrency = 1
	}

	// Bound the whole collection below the reconcile deadline, then scrape pods concurrently on
	// that shared context. A slow pod that still answers within the budget is collected; only a
	// scrape still outstanding when the budget is spent is cut.
	collectCtx, cancel := context.WithTimeout(ctx, bounds.overallTimeout)
	defer cancel()

	// Scrape Ready pods first. Every pod is still scraped (the contract is unchanged), but
	// acquiring the concurrency slots in Ready-first order means that when unreachable pods
	// crowd the fleet -- e.g. a large scale-up whose new pods refuse the metrics port but have
	// an IP -- the healthy pods are not left waiting behind them for a slot (#3585's own case).
	// Single pass: Ready indices go up front; the rest are collected separately and appended
	// after, so IsPodReady is evaluated once per pod. notReady stays nil when every pod is Ready.
	order := make([]int, 0, len(spec.Pods))
	var notReady []int
	for i := range spec.Pods {
		if utils.IsPodReady(&spec.Pods[i]) {
			order = append(order, i)
		} else {
			notReady = append(notReady, i)
		}
	}
	order = append(order, notReady...)

	type podResult struct {
		value float64
		err   error
		// timedOut marks a drop caused by the collection deadline (a scrape the deadline cut, or a
		// pod never launched because the budget was already spent), as opposed to a scrape that
		// returned its own error (a 500, a parse failure) while the budget was still live.
		timedOut bool
	}
	results := make([]podResult, len(spec.Pods))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, idx := range order {
		// Once the overall budget is spent, record the rest as timed out without launching. This
		// is checked first because a ready select has no priority: with a free slot and a done
		// context, select could still take the sem case and spawn a pointless goroutine.
		if err := collectCtx.Err(); err != nil {
			results[idx] = podResult{err: err, timedOut: true}
			continue
		}
		// Acquire a slot in Ready-first order; stop launching once the overall budget is spent
		// (the remaining pods are then recorded as timed out rather than scraped).
		select {
		case sem <- struct{}{}:
		case <-collectCtx.Done():
			results[idx] = podResult{err: collectCtx.Err(), timedOut: true}
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value, err := scrapeOnePod(collectCtx, sem, fetcher, spec.Pods[i], spec.MetricSource)
			// Classify the drop by the collection context, not the returned error: a deadline that
			// fires mid-scrape comes back wrapped with %v (the engine fetcher), so an
			// errors.Is(context.DeadlineExceeded) check would miss it. A non-timeout error (a 500, a
			// parse failure) returns while collectCtx is still live, so timedOut stays false.
			results[i] = podResult{value: value, err: err, timedOut: err != nil && collectCtx.Err() != nil}
		}(idx)
	}
	wg.Wait()

	// Pre-size values for the common case, where most pods succeed. collectErrors and
	// timedOutPods stay lazily allocated: they are the uncommon failure path, so pre-sizing them
	// to len(spec.Pods) would allocate a full-length backing array on every successful reconcile.
	values := make([]float64, 0, len(spec.Pods))
	var collectErrors []error
	var timedOutPods []string
	successCount := 0
	droppedReadyPods := 0
	for i, r := range results {
		if r.err != nil {
			collectErrors = append(collectErrors, fmt.Errorf("pod %s/%s: %w", spec.Pods[i].Namespace, spec.Pods[i].Name, r.err))
			if r.timedOut {
				timedOutPods = append(timedOutPods, fmt.Sprintf("%s/%s", spec.Pods[i].Namespace, spec.Pods[i].Name))
				// A Ready pod dropped by the collection deadline has its sample missing, which biases
				// the per-pod value low (its load is assumed to be the mean of the pods that did
				// report) and can cause a spurious scale-down. Count only these deadline drops -- the
				// samples this bounded collection newly drops -- so the autoscaler holds scale-down by
				// default on them. A Ready pod that returned its own error (a 500, a parse failure)
				// keeps the pre-existing behavior: its sample is skipped and the scale-down proceeds.
				if utils.IsPodReady(&spec.Pods[i]) {
					droppedReadyPods++
				}
			}
			continue
		}
		values = append(values, r.value)
		successCount++
	}
	if len(timedOutPods) > 0 {
		// Surface the dropped pods: a hot/overloaded pod cut at the per-pod timeout biases the
		// per-pod value low (see its doc comment), so operators can correlate the two.
		klog.V(4).InfoS("Pods timed out during metric collection", "pods", timedOutPods, "overallTimeout", bounds.overallTimeout)
	}

	// Determine overall error status (unchanged semantics): only a total failure sets Error,
	// which makes the aggregator treat the pass as a failed compute (no scaling). Partial
	// success still scales.
	var finalErr error
	if len(collectErrors) > 0 {
		if successCount == 0 {
			finalErr = fmt.Errorf("failed to collect metrics from all %d pods: %v", len(spec.Pods), combineErrors(collectErrors))
		} else {
			klog.V(4).InfoS("Partial metric collection failure",
				"successful", successCount,
				"failed", len(collectErrors),
				"errors", collectErrors)
		}
	}

	return &types.MetricSnapshot{
		Namespace:        spec.Namespace,
		TargetName:       spec.TargetName,
		MetricName:       spec.MetricName,
		Values:           values,
		Timestamp:        spec.Timestamp,
		Source:           string(spec.MetricSource.MetricSourceType),
		Error:            finalErr,
		DroppedReadyPods: droppedReadyPods,
	}, nil
}

// collectFromExternal handles external metric collection
func collectFromExternal(ctx context.Context, spec types.CollectionSpec, fetcher MetricFetcher) (*types.MetricSnapshot, error) {
	// For external metrics, create a dummy pod since the interface requires one
	dummyPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-source",
			Namespace: spec.Namespace,
		},
	}
	value, err := fetcher.FetchPodMetrics(ctx, dummyPod, spec.MetricSource)

	var values []float64
	if err == nil {
		values = []float64{value}
	}

	return &types.MetricSnapshot{
		Namespace:  spec.Namespace,
		TargetName: spec.TargetName,
		MetricName: spec.MetricName,
		Values:     values,
		Timestamp:  spec.Timestamp,
		Source:     "external",
		Error:      err,
	}, nil
}

// combineErrors combines multiple errors into a single error
func combineErrors(errors []error) error {
	if len(errors) == 0 {
		return nil
	}
	if len(errors) == 1 {
		return errors[0]
	}

	var errStrings []string
	for _, err := range errors {
		if err != nil {
			errStrings = append(errStrings, err.Error())
		}
	}
	return fmt.Errorf("[%s]", strings.Join(errStrings, "; "))
}
