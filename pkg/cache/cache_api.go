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
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// Cache is the root interface aggregating caching functionalities
type Cache interface {
	PodCache
	ModelCache
	MetricCache
	RequestTracker
	ProfileCache
	types.OutputPredictorProvider
	types.RouterProvider
}

// PodCache defines operations for pod information caching
type PodCache interface {
	// GetPod retrieves a Pod object by name
	// Parameters:
	//   podName: Name of the pod
	//   podNamespace: Namespace of the pod
	// Returns:
	//   *v1.Pod: Found pod object
	//   error: Error information if operation fails
	GetPod(podName, podNamespace string) (*v1.Pod, error)

	// ListPodsByModel gets pods associated with a model
	// Parameters:
	//   modelName: Name of the model
	// Returns:
	//   map[string]*v1.Pod: Pod objects matching the criteria
	//   error: Error information if operation fails
	ListPodsByModel(modelName string) (types.PodList, error)
}

// ModelCache defines operations for model information caching
type ModelCache interface {
	// HasModel checks existence of a model
	// Parameters:
	//   modelName: Name of the model
	// Returns:
	//   bool: True if model exists, false otherwise
	HasModel(modelName string) bool

	// ListModels gets all model names
	// Returns:
	//   []string: List of model names
	ListModels() []string

	// ListModelsByPod gets models associated with a pod
	// Parameters:
	//   podName: Name of the pod
	//   podNamespace: Namespace of the pod
	// Returns:
	//   map[string]struct{}: Set of model names
	//   error: Error information if operation fails
	ListModelsByPod(podName, podNamespace string) ([]string, error)
}

// MetricCache defines operations for metric data caching
type MetricCache interface {
	// GetMetricValueByPod gets metric value for a pod
	// Parameters:
	//   podName: Name of the pod
	//   podNamespace: Namespace of the pod
	//   metricName: Name of the metric
	// Returns:
	//   metrics.MetricValue: Retrieved metric value
	//   error: Error information if operation fails
	GetMetricValueByPod(podName, podNamespace, metricName string) (metrics.MetricValue, error)

	// GetMetricValueByPodModel gets metric value for pod-model pair
	// Parameters:
	//   ctx: Routing context
	//   podName: Name of the pod
	//   podNamespace: Namespace of the pod
	//   modelName: Name of the model
	//   metricName: Name of the metric
	// Returns:
	//   metrics.MetricValue: Retrieved metric value
	//   error: Error information if operation fails
	GetMetricValueByPodModel(podName, podNamespace, modelName string, metricName string) (metrics.MetricValue, error)

	// AddSubscriber adds a metric subscriber
	// Parameters:
	//   subscriber: Metric subscriber implementation
	AddSubscriber(subscriber metrics.MetricSubscriber)
}

// RequestTracker defines operations for track workload statistics
type RequestTracker interface {
	// AddRequestCount tracks the start of a request after routing.
	// To support realtime statistics update and access, AddRequestCount can be called multiple times for a request.
	// As the result, implementation should ensure thread-safe access to the counterm and idempotency.
	//
	// Parameters:
	//   ctx: Routing context
	//   requestID: Unique request identifier
	//   modelName: Name of the model
	// Returns:
	//   int64: Trace term identifier
	AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) (traceTerm int64)

	// DoneRequestCount tracks the completion of a request without usage information like inputTokens and outputTokens.
	// Only one DoneRequestXXX should be called for a request. Idemptency is not required.
	//
	// Parameters:
	//   requestID: Unique request identifier
	//   modelName: Name of the model
	//   traceTerm: Trace term identifier
	DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64)

	// DoneRequestTrace tracks the completion of a request with usage information like inputTokens and outputTokens.
	// Only one DoneRequestXXX should be called for a request. Idemptency is not required.
	//
	// Parameters:
	//   ctx: Routing context
	//   requestID: Unique request identifier
	//   modelName: Name of the model
	//   inputTokens: Number of input tokens
	//   outputTokens: Number of output tokens
	//   traceTerm: Trace term identifier
	DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, inputTokens, outputTokens, traceTerm int64)
}

// ProfileCache defines operations for model profiles
type ProfileCache interface {
	// GetModelProfileByPod gets model profile for a pod
	// Parameters:
	//   pod: Pod object
	//   modelName: Name of the model
	GetModelProfileByPod(pod *v1.Pod, modelName string) (*ModelGPUProfile, error)

	// GetModelProfileByDeploymentName gets model profile for a deployment
	// Parameters:
	//   deploymentName: Name of the deployment
	//   modelName: Name of the model
	GetModelProfileByDeploymentName(deploymentName string, modelName string) (*ModelGPUProfile, error)
}
