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
	"io"
	"net/http"
	"strings"

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
)

// MetricFetcher defines an interface for fetching metrics. it could be Kubernetes metrics or Pod prometheus metrics.
type MetricFetcher interface {
	// Obseleted: Call FetchMetric instead.
	FetchPodMetrics(ctx context.Context, pod v1.Pod, metricsPort int, metricName string) (float64, error)

	FetchMetric(ctx context.Context, endpoint string, path string, metricName string) (float64, error)
}

// RestMetricsFetcher implements MetricFetcher to fetch metrics from Pod's /metrics endpoint.
type RestMetricsFetcher struct{}

func (f *RestMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, metricsPort int, metricName string) (float64, error) {
	// Use http to fetch pod's /metrics endpoint and parse the value
	return f.FetchMetric(ctx, fmt.Sprintf("http://%s:%d", pod.Status.PodIP, metricsPort), "/metrics", metricName)
}

func (f *RestMetricsFetcher) FetchMetric(ctx context.Context, endpoint string, path string, metricName string) (float64, error) {

	// We should use the primary container port. In future, we can decide whether to use sidecar container's port
	url := fmt.Sprintf("http://%s/%s", strings.TrimRight(endpoint, "/"), strings.TrimLeft(path, "/"))

	// Create request with context, so that the request will be canceled if the context is canceled
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0.0, fmt.Errorf("failed to create request to source %s: %v", url, err)
	}

	// Send the request using the default client
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0.0, fmt.Errorf("failed to fetch metrics from source %s: %v", url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			// Handle the error here. For example, log it or take appropriate corrective action.
			klog.InfoS("Error closing response body:", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0.0, fmt.Errorf("failed to read response from source %s: %v", url, err)
	}

	metricValue, err := ParseMetricFromBody(body, metricName)
	if err != nil {
		return 0.0, fmt.Errorf("failed to parse metrics from source %s: %v", url, err)
	}

	klog.InfoS("Successfully parsed metrics", "metric", metricName, "source", url, "metricValue", metricValue)

	return metricValue, nil
}

// ResourceMetricsFetcher fetches resource metrics from Kubernetes metrics API (metrics.k8s.io).
type ResourceMetricsFetcher struct {
	metricsClient *versioned.Clientset
}

func NewResourceMetricsFetcher(metricsClient *versioned.Clientset) *ResourceMetricsFetcher {
	return &ResourceMetricsFetcher{metricsClient: metricsClient}
}

func (f *ResourceMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, metricName string) (float64, error) {
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
	customMetricsClient custom_metrics.CustomMetricsClient
}

// NewCustomMetricsFetcher creates a new fetcher for Custom Metrics API.
func NewCustomMetricsFetcher(client custom_metrics.CustomMetricsClient) *CustomMetricsFetcher {
	return &CustomMetricsFetcher{customMetricsClient: client}
}

// FetchPodMetrics fetches custom metrics for a pod using the Custom Metrics API.
func (f *CustomMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, metricName string) (float64, error) {
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

func (f *KubernetesMetricsFetcher) FetchPodMetrics(ctx context.Context, pod v1.Pod, containerPort int, metricName string, metricType MetricType) (float64, error) {
	switch metricType {
	case ResourceMetrics:
		return f.resourceFetcher.FetchPodMetrics(ctx, pod, metricName)
	case CustomMetrics:
		return f.customFetcher.FetchPodMetrics(ctx, pod, metricName)
	default:
		return 0, fmt.Errorf("unsupported metric type: %s", metricType)
	}
}
