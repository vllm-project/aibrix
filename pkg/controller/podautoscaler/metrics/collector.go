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
	"errors"
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
	// perPodTimeout bounds a single pod's metric scrape. Without it, one unreachable pod (a TCP
	// blackhole, or connection-refused answered through the engine fetcher's retry backoff) can
	// consume the entire reconcile budget before any healthy pod is scraped.
	//
	// Note it sits below the engine fetcher's own 1s/2s/4s retry schedule, so a pod that is slow
	// because it is overloaded (often the hottest pod) can be dropped here; podsCarryingLoad then
	// spreads the mean of the faster pods over it, biasing the per-pod value low exactly when load
	// is high. That is an accepted trade-off for not letting one slow pod stall scaling; the
	// timed-out pods are logged so the bias is visible.
	perPodTimeout time.Duration
	// overallTimeout bounds the whole concurrent collection. It is kept well below the reconcile
	// deadline (DefaultReconcileTimeoutDuration, 10s) so that even a large batch of slow or
	// unreachable pods still returns with time to spare to act on the partial metrics.
	overallTimeout time.Duration
	// maxConcurrency bounds how many pods are scraped in parallel, keeping collection fast without
	// opening an unbounded number of connections against a large fleet. Values below 1 are clamped
	// to 1 (see collectFromPods), so the semaphore channel is always buffered.
	maxConcurrency int
}

// defaultCollectBounds is the production configuration used by CollectMetrics.
var defaultCollectBounds = collectBounds{
	perPodTimeout:  3 * time.Second,
	overallTimeout: 6 * time.Second,
	maxConcurrency: 16,
}

// CollectMetrics gathers metrics for a scaling target using the factory pattern
func CollectMetrics(ctx context.Context, spec types.CollectionSpec, factory MetricFetcherFactory) (*types.MetricSnapshot, error) {
	fetcher := factory.For(spec.MetricSource)

	switch spec.MetricSource.MetricSourceType {
	case autoscalingv1alpha1.POD, autoscalingv1alpha1.RESOURCE, autoscalingv1alpha1.CUSTOM:
		return collectFromPods(ctx, spec, fetcher, defaultCollectBounds)
	case autoscalingv1alpha1.EXTERNAL, autoscalingv1alpha1.DOMAIN:
		return collectFromExternal(ctx, spec, fetcher)
	default:
		return collectFromPods(ctx, spec, fetcher, defaultCollectBounds) // Default to pod collection
	}
}

// scrapeOnePod wraps fetcher.FetchPodMetrics with a per-pod sub-deadline so one slow pod cannot
// consume the collection budget the others need. Pulled out so the deferred slot release lives at
// function scope rather than inside a select case.
func scrapeOnePod(ctx context.Context, sem chan struct{}, fetcher MetricFetcher, pod corev1.Pod, source autoscalingv1alpha1.MetricSource, perPodTimeout time.Duration) (float64, error) {
	defer func() { <-sem }()
	podCtx, cancel := context.WithTimeout(ctx, perPodTimeout)
	defer cancel()
	return fetcher.FetchPodMetrics(podCtx, pod, source)
}

// collectFromPods handles pod-level metric collection. It scrapes the target's pods
// concurrently, each under a per-pod sub-deadline, with the whole collection bounded by an
// overall sub-deadline well below the reconcile budget.
//
// Previously it scraped every pod serially on the shared reconcile context, so a single
// unreachable pod -- one that eats the engine fetcher's full HTTP timeout plus 1s/2s/4s retry
// backoff -- could consume the entire 10s reconcile budget before any healthy pod was reached.
// On total failure with replicas within [min,max] the pass set ScalingActive=False and applied
// no scaling, i.e. scaling stalled during the very scale-ups/incidents where it matters most.
// Scraping concurrently under tight sub-deadlines means slow or unreachable pods no longer
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

	// Bound the whole collection below the reconcile deadline, then scrape pods concurrently,
	// each under its own per-pod sub-deadline, so one slow pod cannot consume the budget the
	// others need.
	collectCtx, cancel := context.WithTimeout(ctx, bounds.overallTimeout)
	defer cancel()

	// Scrape Ready pods first. Every pod is still scraped (the contract is unchanged), but
	// acquiring the concurrency slots in Ready-first order means that when unreachable pods
	// crowd the fleet -- e.g. a large scale-up whose new pods refuse the metrics port but have
	// an IP -- the healthy pods are not left waiting behind them for a slot (#3585's own case).
	order := make([]int, 0, len(spec.Pods))
	for i := range spec.Pods {
		if utils.IsPodReady(&spec.Pods[i]) {
			order = append(order, i)
		}
	}
	for i := range spec.Pods {
		if !utils.IsPodReady(&spec.Pods[i]) {
			order = append(order, i)
		}
	}

	type podResult struct {
		value float64
		err   error
	}
	results := make([]podResult, len(spec.Pods))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, idx := range order {
		// Once the overall budget is spent, record the rest as timed out without launching. This
		// is checked first because a ready select has no priority: with a free slot and a done
		// context, select could still take the sem case and spawn a pointless goroutine.
		if err := collectCtx.Err(); err != nil {
			results[idx] = podResult{err: err}
			continue
		}
		// Acquire a slot in Ready-first order; stop launching once the overall budget is spent
		// (the remaining pods are then recorded as timed out rather than scraped).
		select {
		case sem <- struct{}{}:
		case <-collectCtx.Done():
			results[idx] = podResult{err: collectCtx.Err()}
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value, err := scrapeOnePod(collectCtx, sem, fetcher, spec.Pods[i], spec.MetricSource, bounds.perPodTimeout)
			results[i] = podResult{value: value, err: err}
		}(idx)
	}
	wg.Wait()

	var values []float64
	var collectErrors []error
	var timedOutPods []string
	successCount := 0
	for i, r := range results {
		if r.err != nil {
			collectErrors = append(collectErrors, fmt.Errorf("pod %s/%s: %w", spec.Pods[i].Namespace, spec.Pods[i].Name, r.err))
			if errors.Is(r.err, context.DeadlineExceeded) || errors.Is(r.err, context.Canceled) {
				timedOutPods = append(timedOutPods, fmt.Sprintf("%s/%s", spec.Pods[i].Namespace, spec.Pods[i].Name))
			}
			continue
		}
		values = append(values, r.value)
		successCount++
	}
	if len(timedOutPods) > 0 {
		// Surface the dropped pods: a hot/overloaded pod cut at the per-pod timeout biases the
		// per-pod value low (see its doc comment), so operators can correlate the two.
		klog.V(4).InfoS("Pods timed out during metric collection", "pods", timedOutPods, "perPodTimeout", bounds.perPodTimeout)
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
		Namespace:  spec.Namespace,
		TargetName: spec.TargetName,
		MetricName: spec.MetricName,
		Values:     values,
		Timestamp:  spec.Timestamp,
		Source:     string(spec.MetricSource.MetricSourceType),
		Error:      finalErr,
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
