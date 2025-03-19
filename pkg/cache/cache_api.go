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

package cache

import (
	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
)

// Cache is the root interface aggregating caching functionalities
type Cache interface {
	PodCache
	ModelCache
	MetricCache
	TraceCache
}

// PodCache defines operations for pod information caching
type PodCache interface {
	// GetPod retrieves a Pod object by name
	// Parameters:
	//   podName: Name of the pod
	// Returns:
	//   *v1.Pod: Found pod object
	//   error: Error information if operation fails
	GetPod(podName string) (*v1.Pod, error)

	// ListPods gets all pods mapping
	// Returns:
	//   map[string]*v1.Pod: Pod objects mapped by pod names
	ListPods() map[string]*v1.Pod

	// ListPodsByModel gets pods associated with a model
	// Parameters:
	//   modelName: Name of the model
	// Returns:
	//   map[string]*v1.Pod: Pod objects matching the criteria
	//   error: Error information if operation fails
	ListPodsByModel(modelName string) (map[string]*v1.Pod, error)
}

// ModelCache defines operations for model information caching
type ModelCache interface {
	// GetModel checks existence of a model
	// Parameters:
	//   modelName: Name of the model
	// Returns:
	//   bool: True if model exists, false otherwise
	GetModel(modelName string) bool

	// ListModels gets all model names
	// Returns:
	//   []string: List of model names
	ListModels() []string

	// ListModelsByPod gets models associated with a pod
	// Parameters:
	//   podName: Name of the pod
	// Returns:
	//   map[string]struct{}: Set of model names
	//   error: Error information if operation fails
	ListModelsByPod(podName string) (map[string]struct{}, error)
}

// MetricCache defines operations for metric data caching
type MetricCache interface {
	// GetMetricValueByPod gets metric value for a pod
	// Parameters:
	//   podName: Name of the pod
	//   metricName: Name of the metric
	// Returns:
	//   metrics.MetricValue: Retrieved metric value
	//   error: Error information if operation fails
	GetMetricValueByPod(podName, metricName string) (metrics.MetricValue, error)

	// GetMetricValueByPodModel gets metric value for pod-model pair
	// Parameters:
	//   podName: Name of the pod
	//   modelName: Name of the model
	//   metricName: Name of the metric
	// Returns:
	//   metrics.MetricValue: Retrieved metric value
	//   error: Error information if operation fails
	GetMetricValueByPodModel(podName, modelName string, metricName string) (metrics.MetricValue, error)

	// AddRequestCount starts tracking request count
	// Parameters:
	//   requestID: Unique request identifier
	//   modelName: Name of the model
	// Returns:
	//   int64: Trace term identifier
	AddRequestCount(requestID string, modelName string) (traceTerm int64)

	// DoneRequestCount completes request count tracking
	// Parameters:
	//   requestID: Unique request identifier
	//   modelName: Name of the model
	//   traceTerm: Trace term identifier
	DoneRequestCount(requestID string, modelName string, traceTerm int64)

	// AddSubscriber adds a metric subscriber
	// Parameters:
	//   subscriber: Metric subscriber implementation
	AddSubscriber(subscriber metrics.MetricSubscriber)
}

// TraceCache defines operations for request tracing
type TraceCache interface {
	// AddRequestTrace initiates request tracing
	// Parameters:
	//   requestID: Unique request identifier
	//   modelName: Name of the model
	//   inputTokens: Number of input tokens
	//   outputTokens: Number of output tokens
	AddRequestTrace(requestID string, modelName string, inputTokens, outputTokens int64)

	// DoneRequestTrace completes request tracing
	// Parameters:
	//   requestID: Unique request identifier
	//   modelName: Name of the model
	//   inputTokens: Number of input tokens
	//   outputTokens: Number of output tokens
	//   traceTerm: Trace term identifier
	DoneRequestTrace(requestID string, modelName string, inputTokens, outputTokens, traceTerm int64)
}
