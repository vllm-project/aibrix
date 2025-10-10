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

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/metrics/pkg/client/clientset/versioned"
	"k8s.io/metrics/pkg/client/custom_metrics"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

// MetricFetcher defines a unified interface for fetching metrics.
// All metrics are fetched per-pod to maintain uniform upper layer logic.
// External metrics are adapted to appear as per-pod values.
type MetricFetcher interface {
	// FetchPodMetrics fetches a metric value for a specific pod based on MetricSourceType:
	// - POD: Direct HTTP connection to pod (http://pod_ip:port/path)
	// - RESOURCE: Kubernetes resource metrics API (cpu, memory)
	// - CUSTOM: Kubernetes custom metrics API
	// - EXTERNAL: External services, adapted to per-pod semantics
	FetchPodMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error)
}

// RestMetricsFetcher implements MetricFetcher for POD type metrics (HTTP connections to pods)
type RestMetricsFetcher struct {
	// Centralized engine fetcher with typed metrics
	engineFetcher *metrics.EngineMetricsFetcher
}

var _ MetricFetcher = (*RestMetricsFetcher)(nil)

func NewRestMetricsFetcher() *RestMetricsFetcher {
	return &RestMetricsFetcher{
		engineFetcher: metrics.NewEngineMetricsFetcher(),
	}
}

// NewRestMetricsFetcherWithConfig creates a RestMetricsFetcher with custom configuration
func NewRestMetricsFetcherWithConfig(config metrics.EngineMetricsFetcherConfig) *RestMetricsFetcher {
	return &RestMetricsFetcher{
		engineFetcher: metrics.NewEngineMetricsFetcherWithConfig(config),
	}
}

func (f *RestMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	// RestMetricsFetcher only handles POD type metrics
	if source.MetricSourceType != autoscalingv1alpha1.POD {
		return 0, fmt.Errorf("RestMetricsFetcher only supports POD metric source type, got: %s", source.MetricSourceType)
	}
	return f.fetchFromPod(ctx, pod, source)
}

// fetchFromPod handles pod-level metrics: http://pod_ip:port/path
func (f *RestMetricsFetcher) fetchFromPod(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	// TODO: check pending pod comes here.
	if pod.Status.PodIP == "" {
		return 0, fmt.Errorf("pod %s/%s has no IP address", pod.Namespace, pod.Name)
	}

	klog.V(4).InfoS("Fetching metric from pod",
		"pod", pod.Name,
		"ip", pod.Status.PodIP,
		"port", source.Port,
		"path", source.Path,
		"metric", source.TargetMetric)

	// Use real pod information instead of fake pod creation
	endpoint := fmt.Sprintf("%s:%s", pod.Status.PodIP, source.Port)
	engineType := metrics.GetEngineType(pod)
	identifier := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	// Use the centralized engine fetcher with real pod information
	metricValue, err := f.engineFetcher.FetchTypedMetric(ctx, endpoint, engineType, identifier, source.TargetMetric)
	if err != nil {
		klog.Warningf("Failed to fetch metric %s from pod %s: %v. Returning zero value.",
			source.TargetMetric, identifier, err)
		// Return zero value with warning instead of error - business logic can decide how to handle
		return 0.0, nil
	}

	return metricValue.GetSimpleValue(), nil
}

// ResourceMetricsFetcher handles Kubernetes resource metrics (cpu, memory)
type ResourceMetricsFetcher struct {
	metricsClient *versioned.Clientset
}

var _ MetricFetcher = (*ResourceMetricsFetcher)(nil)

func NewResourceMetricsFetcher(metricsClient *versioned.Clientset) *ResourceMetricsFetcher {
	return &ResourceMetricsFetcher{metricsClient: metricsClient}
}

func (f *ResourceMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	// ResourceMetricsFetcher only handles RESOURCE type metrics
	if source.MetricSourceType != autoscalingv1alpha1.RESOURCE {
		return 0, fmt.Errorf("ResourceMetricsFetcher only supports RESOURCE metric source type, got: %s", source.MetricSourceType)
	}
	return f.fetchResourceMetric(ctx, pod, source)
}

// fetchResourceMetric handles Kubernetes resource metrics (cpu, memory)
func (f *ResourceMetricsFetcher) fetchResourceMetric(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	klog.V(4).InfoS("Fetching resource metric from Kubernetes API",
		"pod", pod.Name,
		"metric", source.TargetMetric)

	if f.metricsClient == nil {
		klog.Warningf("Kubernetes resource metrics client not initialized for metric %s", source.TargetMetric)
		return 0.0, nil
	}

	// Use existing ResourceMetricsFetcher logic
	podMetrics, err := f.metricsClient.MetricsV1beta1().PodMetricses(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to fetch resource metrics for pod %s: %v. Returning zero value.", pod.Name, err)
		return 0.0, nil
	}

	var total float64
	for _, container := range podMetrics.Containers {
		switch source.TargetMetric {
		case "cpu":
			total += float64(container.Usage.Cpu().MilliValue())
		case "memory":
			total += float64(container.Usage.Memory().Value())
		default:
			klog.Warningf("Unsupported resource metric: %s", source.TargetMetric)
			return 0.0, fmt.Errorf("unsupported resource metric: %s", source.TargetMetric)
		}
	}

	klog.V(4).InfoS("Aggregated resource metric for pod",
		"pod", pod.Name,
		"metric", source.TargetMetric,
		"value", total)

	return total, nil
}

// CustomMetricsFetcher handles Kubernetes custom metrics
type CustomMetricsFetcher struct {
	customMetricsClient custom_metrics.CustomMetricsClient
}

var _ MetricFetcher = (*CustomMetricsFetcher)(nil)

func NewCustomMetricsFetcher(client custom_metrics.CustomMetricsClient) *CustomMetricsFetcher {
	return &CustomMetricsFetcher{customMetricsClient: client}
}

func (f *CustomMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	// CustomMetricsFetcher only handles CUSTOM type metrics
	if source.MetricSourceType != autoscalingv1alpha1.CUSTOM {
		return 0, fmt.Errorf("CustomMetricsFetcher only supports CUSTOM metric source type, got: %s", source.MetricSourceType)
	}
	return f.fetchCustomMetric(ctx, pod, source)
}

// fetchCustomMetric handles Kubernetes custom metrics
func (f *CustomMetricsFetcher) fetchCustomMetric(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	klog.V(4).InfoS("Fetching custom metric from Kubernetes API",
		"pod", pod.Name,
		"metric", source.TargetMetric)

	if f.customMetricsClient == nil {
		klog.Warningf("Kubernetes custom metrics client not initialized for metric %s", source.TargetMetric)
		return 0.0, nil
	}

	// Use existing CustomMetricsFetcher logic
	podRef := types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}

	podGK := schema.GroupKind{
		Group: "",
		Kind:  "Pod",
	}

	metricList, err := f.customMetricsClient.NamespacedMetrics(pod.Namespace).GetForObject(podGK, podRef.Name, source.TargetMetric, labels.Everything())
	if err != nil {
		klog.Warningf("Failed to fetch custom metric %s for pod %s: %v. Returning zero value.", source.TargetMetric, pod.Name, err)
		return 0.0, nil
	}

	return float64(metricList.Value.Value()), nil
}

// ExternalMetricsFetcher handles external metrics with per-pod adaptation
type ExternalMetricsFetcher struct {
	engineFetcher *metrics.EngineMetricsFetcher
}

var _ MetricFetcher = (*ExternalMetricsFetcher)(nil)

func NewExternalMetricsFetcher() *ExternalMetricsFetcher {
	return &ExternalMetricsFetcher{
		engineFetcher: metrics.NewEngineMetricsFetcher(),
	}
}

func (f *ExternalMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	// ExternalMetricsFetcher only handles EXTERNAL/DOMAIN type metrics
	if source.MetricSourceType != autoscalingv1alpha1.EXTERNAL && source.MetricSourceType != autoscalingv1alpha1.DOMAIN {
		return 0, fmt.Errorf("ExternalMetricsFetcher only supports EXTERNAL/DOMAIN metric source types, got: %s", source.MetricSourceType)
	}
	return f.fetchFromExternal(ctx, pod, source)
}

// fetchFromExternal handles external service metrics with per-pod adaptation
func (f *ExternalMetricsFetcher) fetchFromExternal(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	// Differentiate between two types of external sources:
	// 1. Endpoint != "" -> AIBrix GPU-Optimizer REST API
	// 2. Endpoint == "" -> Kubernetes external.metrics API

	if source.Endpoint != "" {
		// AIBrix GPU-Optimizer: REST API call
		return f.fetchFromGPUOptimizer(ctx, pod, source)
	} else {
		// Kubernetes external.metrics API
		return f.fetchFromK8sExternalMetrics(ctx, pod, source)
	}
}

// fetchFromGPUOptimizer fetches from AIBrix GPU-Optimizer REST endpoint
func (f *ExternalMetricsFetcher) fetchFromGPUOptimizer(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	klog.V(4).InfoS("Fetching metric from GPU-Optimizer",
		"pod", pod.Name,
		"endpoint", source.Endpoint,
		"path", source.Path,
		"metric", source.TargetMetric)

	// Use the centralized engine fetcher for external HTTP calls
	// This gives us a global value that we need to adapt to per-pod semantics
	metricValue, err := f.engineFetcher.FetchTypedMetric(ctx, source.Endpoint, "external", "gpu-optimizer", source.TargetMetric)
	if err != nil {
		klog.Warningf("Failed to fetch metric %s from GPU-Optimizer %s: %v. Returning zero value.",
			source.TargetMetric, source.Endpoint, err)
		return 0.0, nil
	}

	// Adaptation: Global metric -> per-pod value
	// For now, return the global value as-is. Upper layer will aggregate by averaging.
	// This means each pod gets the same "global recommendation" value.
	// Alternative approaches: divide by pod count, use pod-specific weights, etc.
	return metricValue.GetSimpleValue(), nil
}

// fetchFromK8sExternalMetrics fetches from Kubernetes external.metrics API
func (f *ExternalMetricsFetcher) fetchFromK8sExternalMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	klog.V(4).InfoS("Fetching metric from Kubernetes external.metrics API",
		"pod", pod.Name,
		"metric", source.TargetMetric)

	// TODO: Implement Kubernetes external.metrics API client
	// For now, return a placeholder that won't break the system
	klog.Warningf("Kubernetes external.metrics API not implemented yet for metric %s", source.TargetMetric)
	return 0.0, fmt.Errorf("kubernetes external.metrics API not implemented yet for metric %s", source.TargetMetric)
}

// MetricFetcherFactory provides a clean way to get the right fetcher for each metric source type
type MetricFetcherFactory interface {
	// For returns the appropriate fetcher for the given metric source
	For(source autoscalingv1alpha1.MetricSource) MetricFetcher
}

// DefaultMetricFetcherFactory implements MetricFetcherFactory with all fetcher types
type DefaultMetricFetcherFactory struct {
	rest     *RestMetricsFetcher     // POD (engine /metrics)
	resource *ResourceMetricsFetcher // metrics.k8s.io
	custom   *CustomMetricsFetcher   // custom.metrics
	external *ExternalMetricsFetcher // external.metrics + aibrix-optimizer
}

// NewDefaultMetricFetcherFactory creates a factory with all fetcher types
func NewDefaultMetricFetcherFactory(
	resourceClient *versioned.Clientset,
	customClient custom_metrics.CustomMetricsClient,
) *DefaultMetricFetcherFactory {
	return &DefaultMetricFetcherFactory{
		rest:     NewRestMetricsFetcher(),
		resource: NewResourceMetricsFetcher(resourceClient),
		custom:   NewCustomMetricsFetcher(customClient),
		external: NewExternalMetricsFetcher(),
	}
}

// For returns the appropriate fetcher for the given metric source type
func (f *DefaultMetricFetcherFactory) For(source autoscalingv1alpha1.MetricSource) MetricFetcher {
	switch source.MetricSourceType {
	case autoscalingv1alpha1.POD:
		return f.rest
	case autoscalingv1alpha1.RESOURCE:
		return f.resource
	case autoscalingv1alpha1.CUSTOM:
		return f.custom
	case autoscalingv1alpha1.EXTERNAL, autoscalingv1alpha1.DOMAIN:
		return f.external
	default:
		// Safe fallback - external fetcher can handle most scenarios gracefully
		klog.Warningf("Unknown metric source type %s, falling back to external fetcher", source.MetricSourceType)
		return f.external
	}
}
