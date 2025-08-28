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
	"net"
	"strconv"
	"strings"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/metrics"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/metrics/pkg/client/clientset/versioned"
	"k8s.io/metrics/pkg/client/custom_metrics"
)

// MetricType defines the type of metrics to be fetched.
type MetricType string

const (
	ResourceMetrics MetricType = "resource"
	CustomMetrics   MetricType = "custom"
	RawMetrics      MetricType = "raw"
	maxRetries                 = 3
	baseDelay                  = 100 * time.Millisecond
	maxDelay                   = 5 * time.Second
)

// MetricFetcher defines an interface for fetching metrics at the autoscaler level.
// It works with Kubernetes concepts (pods, metric sources) and delegates to underlying fetchers.
type MetricFetcher interface {
	// FetchPodMetrics fetches a metric from a pod using the autoscaler's metric source configuration
	FetchPodMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error)

	// FetchMetric fetches a metric using endpoint-level parameters (for backward compatibility and testing)
	FetchMetric(ctx context.Context, protocol autoscalingv1alpha1.ProtocolType, endpoint, path, metricName string) (float64, error)
}

type abstractMetricsFetcher struct{}

func (f *abstractMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	return 0.0, fmt.Errorf("not implemented")
}

func (f *abstractMetricsFetcher) FetchMetric(ctx context.Context, protocol autoscalingv1alpha1.ProtocolType, endpoint, path, metricName string) (float64, error) {
	return 0.0, fmt.Errorf("not implemented")
}

// RestMetricsFetcher implements MetricFetcher using the centralized EngineFetcher from pkg/metrics
type RestMetricsFetcher struct {
	// For unit test purpose only
	testURLSetter func(string)
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
	// Check for test URL setter (for unit tests)
	if f.testURLSetter != nil {
		pathSeparator := "/"
		if strings.HasPrefix(source.Path, "/") {
			pathSeparator = ""
		}
		url := fmt.Sprintf("%s://%s:%s%s%s", source.ProtocolType, pod.Status.PodIP, source.Port, pathSeparator, source.Path)
		f.testURLSetter(url)
		return 0.0, nil
	}

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

// FetchMetric is deprecated. Use FetchPodMetrics instead which provides proper pod context.
// This method creates a fake pod which loses important metadata and should be avoided.
func (f *RestMetricsFetcher) FetchMetric(ctx context.Context, protocol autoscalingv1alpha1.ProtocolType, endpoint, path, metricName string) (float64, error) {
	// Check for test URL setter (for unit tests)
	if f.testURLSetter != nil {
		// Handle path that may or may not start with a slash
		pathSeparator := "/"
		if strings.HasPrefix(path, "/") {
			pathSeparator = ""
		}
		url := fmt.Sprintf("%s://%s%s%s", protocol, endpoint, pathSeparator, path)
		f.testURLSetter(url)
		return 0.0, nil
	}

	// Parse endpoint to extract podIP and port using Go standard library
	podIP, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		return 0.0, fmt.Errorf("invalid endpoint format %s: %v", endpoint, err)
	}

	metricPort, err := strconv.Atoi(portStr)
	if err != nil {
		return 0.0, fmt.Errorf("invalid port in endpoint %s: %v", endpoint, err)
	}

	// Use the centralized engine fetcher directly with endpoint-based parameters
	// Create a fake pod for engine type extraction (this could be improved in the future)
	fakePod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("endpoint-%s", podIP),
			Labels: map[string]string{
				"engine": "vllm", // Default engine type - business logic should pass this properly
			},
		},
		Status: v1.PodStatus{
			PodIP: podIP,
		},
	}

	engineType := metrics.GetEngineType(fakePod)
	identifier := fmt.Sprintf("endpoint-%s", podIP)
	fullEndpoint := fmt.Sprintf("%s:%d", podIP, metricPort)

	// Use the centralized engine fetcher with endpoint-based parameters
	metricValue, err := f.engineFetcher.FetchTypedMetric(ctx, fullEndpoint, engineType, identifier, metricName)
	if err != nil {
		klog.Warningf("Failed to fetch metric %s from endpoint %s: %v. Returning zero value.",
			metricName, fullEndpoint, err)
		// Return zero value with warning instead of error - business logic can decide how to handle
		return 0.0, nil
	}

	return metricValue.GetSimpleValue(), nil
}

// Helper functions

// ResourceMetricsFetcher fetches resource metrics from Kubernetes metrics API (metrics.k8s.io).
type ResourceMetricsFetcher struct {
	abstractMetricsFetcher
	metricsClient *versioned.Clientset
}

func NewResourceMetricsFetcher(metricsClient *versioned.Clientset) *ResourceMetricsFetcher {
	return &ResourceMetricsFetcher{metricsClient: metricsClient}
}

func (f *ResourceMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	// For resource metrics, we use the TargetMetric from the source
	return f.fetchResourceMetric(ctx, pod, source.TargetMetric)
}

func (f *ResourceMetricsFetcher) fetchResourceMetric(ctx context.Context, pod v1.Pod, metricName string) (float64, error) {
	podMetrics, err := f.metricsClient.MetricsV1beta1().PodMetricses(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to fetch resource metrics for pod %s: %v", pod.Name, err)
	}

	for _, container := range podMetrics.Containers {
		switch metricName {
		case "cpu":
			return float64(container.Usage.Cpu().MilliValue()), nil
		case "memory":
			return float64(container.Usage.Memory().Value()), nil
		}
	}

	return 0, fmt.Errorf("resource metric %s not found for pod %s", metricName, pod.Name)
}

// CustomMetricsFetcher fetches custom metrics from Kubernetes' native Custom Metrics API.
type CustomMetricsFetcher struct {
	abstractMetricsFetcher
	customMetricsClient custom_metrics.CustomMetricsClient
}

// NewCustomMetricsFetcher creates a new fetcher for Custom Metrics API.
func NewCustomMetricsFetcher(client custom_metrics.CustomMetricsClient) *CustomMetricsFetcher {
	return &CustomMetricsFetcher{customMetricsClient: client}
}

// FetchPodMetrics fetches custom metrics for a pod using the Custom Metrics API.
func (f *CustomMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	// For custom metrics, we use the TargetMetric from the source
	return f.fetchCustomMetric(ctx, pod, source.TargetMetric)
}

func (f *CustomMetricsFetcher) fetchCustomMetric(ctx context.Context, pod v1.Pod, metricName string) (float64, error) {
	// Define a reference to the pod (using GroupResource)
	podRef := types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}

	// GroupKind for Pods in Kubernetes
	podGK := schema.GroupKind{
		Group: "",    // Pods are in the core API group, so the group is an empty string
		Kind:  "Pod", // The kind is "Pod"
	}

	// Fetch custom metric for the pod
	metricList, err := f.customMetricsClient.NamespacedMetrics(pod.Namespace).GetForObject(podGK, podRef.Name, metricName, labels.Everything())
	if err != nil {
		return 0, fmt.Errorf("failed to fetch custom metric %s for pod %s: %v", metricName, pod.Name, err)
	}

	// Assume we are dealing with a single metric item (as is typical for a single pod)
	return float64(metricList.Value.Value()), nil
}

type KubernetesMetricsFetcher struct {
	abstractMetricsFetcher
	resourceFetcher *ResourceMetricsFetcher
	customFetcher   *CustomMetricsFetcher
}

// NewKubernetesMetricsFetcher creates a new fetcher for both resource and custom metrics.
func NewKubernetesMetricsFetcher(resourceFetcher *ResourceMetricsFetcher, customFetcher *CustomMetricsFetcher) *KubernetesMetricsFetcher {
	return &KubernetesMetricsFetcher{
		resourceFetcher: resourceFetcher,
		customFetcher:   customFetcher,
	}
}

// FetchPodMetrics implements the MetricFetcher interface by delegating to appropriate sub-fetchers
func (f *KubernetesMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	// Determine metric type based on source configuration
	// For simplicity, assume ResourceMetrics for CPU/memory, CustomMetrics for others
	metricType := CustomMetrics
	if source.TargetMetric == "cpu" || source.TargetMetric == "memory" {
		metricType = ResourceMetrics
	}

	switch metricType {
	case ResourceMetrics:
		return f.resourceFetcher.FetchPodMetrics(ctx, pod, source)
	case CustomMetrics:
		return f.customFetcher.FetchPodMetrics(ctx, pod, source)
	default:
		return 0, fmt.Errorf("unsupported metric type: %s", metricType)
	}
}
