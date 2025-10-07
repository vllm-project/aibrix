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

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
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

// collectFromPods handles pod-level metric collection
func collectFromPods(ctx context.Context, spec types.CollectionSpec, fetcher MetricFetcher) (*types.MetricSnapshot, error) {
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

	// Determine overall error status
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
			Namespace: "default",
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
