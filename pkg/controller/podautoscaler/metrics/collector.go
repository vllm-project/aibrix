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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const (
	// perPodMetricTimeout bounds a single pod's metric scrape. Without it, one unreachable
	// pod (a TCP blackhole, or connection-refused answered through the engine fetcher's
	// retry backoff) can consume the entire reconcile budget before any healthy pod is scraped.
	perPodMetricTimeout = 3 * time.Second
	// metricCollectionTimeout bounds the whole concurrent collection. It is kept well below
	// the reconcile deadline (DefaultReconcileTimeoutDuration, 10s) so that even a large batch
	// of slow or unreachable pods still returns with time to spare to act on the partial metrics.
	metricCollectionTimeout = 6 * time.Second
	// maxConcurrentPodScrapes bounds how many pods are scraped in parallel, keeping collection
	// fast without opening an unbounded number of connections against a large fleet.
	maxConcurrentPodScrapes = 16
)

// CollectMetrics gathers metrics for a scaling target using the factory pattern
func CollectMetrics(ctx context.Context, spec types.CollectionSpec, factory MetricFetcherFactory) (*types.MetricSnapshot, error) {
	fetcher := factory.For(spec.MetricSource)

	switch spec.MetricSource.MetricSourceType {
	case autoscalingv1alpha1.POD, autoscalingv1alpha1.RESOURCE, autoscalingv1alpha1.CUSTOM:
		return collectFromPods(ctx, spec, fetcher)
	case autoscalingv1alpha1.EXTERNAL, autoscalingv1alpha1.DOMAIN:
		return collectFromExternal(ctx, spec, fetcher)
	default:
		return collectFromPods(ctx, spec, fetcher) // Default to pod collection
	}
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
// the downstream load math are unchanged.
func collectFromPods(ctx context.Context, spec types.CollectionSpec, fetcher MetricFetcher) (*types.MetricSnapshot, error) {
	// Bound the whole collection below the reconcile deadline, then scrape pods concurrently,
	// each under its own per-pod sub-deadline, so one slow pod cannot consume the budget the
	// others need.
	collectCtx, cancel := context.WithTimeout(ctx, metricCollectionTimeout)
	defer cancel()

	type podResult struct {
		value float64
		err   error
	}
	results := make([]podResult, len(spec.Pods))
	sem := make(chan struct{}, maxConcurrentPodScrapes)
	var wg sync.WaitGroup
	for i := range spec.Pods {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Acquire a concurrency slot, but give up if the overall budget is already spent.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-collectCtx.Done():
				results[i] = podResult{err: collectCtx.Err()}
				return
			}
			podCtx, podCancel := context.WithTimeout(collectCtx, perPodMetricTimeout)
			defer podCancel()
			value, err := fetcher.FetchPodMetrics(podCtx, spec.Pods[i], spec.MetricSource)
			results[i] = podResult{value: value, err: err}
		}(i)
	}
	wg.Wait()

	var values []float64
	var collectErrors []error
	successCount := 0
	for i, r := range results {
		if r.err != nil {
			collectErrors = append(collectErrors, fmt.Errorf("pod %s/%s: %w", spec.Pods[i].Namespace, spec.Pods[i].Name, r.err))
			continue
		}
		values = append(values, r.value)
		successCount++
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
