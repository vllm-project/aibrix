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

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

// MetricCollector orchestrates metric collection from various sources
type MetricCollector interface {
	// CollectMetrics gathers metrics for a scaling target
	CollectMetrics(ctx context.Context, spec types.CollectionSpec) (*types.MetricSnapshot, error)

	// GetCollectorType returns the collector type for logging/debugging
	GetCollectorType() string
}

// PodMetricCollector collects metrics from pods
type PodMetricCollector struct {
	fetcher MetricFetcher
}

func NewPodMetricCollector(fetcher MetricFetcher) *PodMetricCollector {
	return &PodMetricCollector{
		fetcher: fetcher,
	}
}

func (c *PodMetricCollector) CollectMetrics(ctx context.Context, spec types.CollectionSpec) (*types.MetricSnapshot, error) {
	// Reuse existing GetMetricsFromPods logic
	values, err := GetMetricsFromPods(ctx, c.fetcher, spec.Pods, spec.MetricSource)

	return &types.MetricSnapshot{
		Namespace:  spec.Namespace,
		TargetName: spec.TargetName,
		MetricName: spec.MetricName,
		Values:     values,
		Timestamp:  spec.Timestamp,
		Source:     "pod",
		Error:      err,
	}, nil
}

func (c *PodMetricCollector) GetCollectorType() string {
	return "pod"
}

// DomainMetricCollector collects metrics from domain sources
type DomainMetricCollector struct {
	fetcher MetricFetcher
}

func NewDomainMetricCollector(fetcher MetricFetcher) *DomainMetricCollector {
	return &DomainMetricCollector{
		fetcher: fetcher,
	}
}

func (c *DomainMetricCollector) CollectMetrics(ctx context.Context, spec types.CollectionSpec) (*types.MetricSnapshot, error) {
	// Reuse existing GetMetricFromSource logic
	value, err := GetMetricFromSource(ctx, c.fetcher, spec.MetricSource)

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
		Source:     "domain",
		Error:      err,
	}, nil
}

func (c *DomainMetricCollector) GetCollectorType() string {
	return "domain"
}

// NewMetricCollector creates a collector based on metric source type
func NewMetricCollector(sourceType autoscalingv1alpha1.MetricSourceType, fetcher MetricFetcher) MetricCollector {
	switch sourceType {
	case autoscalingv1alpha1.POD:
		return NewPodMetricCollector(fetcher)
	case autoscalingv1alpha1.DOMAIN:
		return NewDomainMetricCollector(fetcher)
	default:
		return NewPodMetricCollector(fetcher) // Default to pod collector
	}
}
