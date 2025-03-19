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
	"fmt"
	"sync/atomic"

	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
)

// GetPod retrieves a Pod object by name from the cache
// Parameters:
//
//	podName: Name of the pod to retrieve
//
// Returns:
//
//	*v1.Pod: The found Pod object
//	error: Error if pod doesn't exist
func (c *Store) GetPod(podName string) (*v1.Pod, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	pod, ok := c.Pods[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the cache: %s", podName)
	}

	return pod, nil
}

// ListPods returns all cached Pod objects
// Returns:
//
//	map[string]*v1.Pod: Map of pod names to Pod objects
func (c *Store) ListPods() map[string]*v1.Pod {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.Pods
}

// ListPodsByModel gets Pods associated with a specific model
// Parameters:
//
//	modelName: Name of the model to query
//
// Returns:
//
//	map[string]*v1.Pod: Map of pod names to Pod objects
//	error: Error if model doesn't exist
func (c *Store) ListPodsByModel(modelName string) (map[string]*v1.Pod, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	podsMap, ok := c.ModelToPodMapping[modelName]
	if !ok {
		return nil, fmt.Errorf("model does not exist in the cache: %s", modelName)
	}

	return podsMap, nil
}

// ListModels returns all cached model names
// Returns:
//
//	[]string: Slice of model names
func (c *Store) ListModels() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	models := []string{}
	for model := range c.ModelToPodMapping {
		models = append(models, model)
	}

	return models
}

// GetModel checks if a model exists in the cache
// Parameters:
//
//	modelName: Name of the model to check
//
// Returns:
//
//	bool: True if model exists
func (c *Store) GetModel(modelName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, ok := c.ModelToPodMapping[modelName]

	return ok
}

// ListModelsByPod gets models associated with a specific Pod
// Parameters:
//
//	podName: Name of the Pod to query
//
// Returns:
//
//	map[string]struct{}: Set of model names
//	error: Error if Pod doesn't exist
func (c *Store) ListModelsByPod(podName string) (map[string]struct{}, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	models, ok := c.PodToModelMapping[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the cache: %s", podName)
	}

	return models, nil
}

// GetMetricValueByPod retrieves metric value for a Pod
// Parameters:
//
//	podName: Name of the Pod
//	metricName: Name of the metric
//
// Returns:
//
//	metrics.MetricValue: The metric value
//	error: Error if Pod or metric doesn't exist
func (c *Store) GetMetricValueByPod(podName, metricName string) (metrics.MetricValue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	podMetrics, ok := c.PodMetrics[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the podMetrics cache")
	}

	metricVal, ok := podMetrics[metricName]
	if !ok {
		return nil, fmt.Errorf("no metric available for %v", metricName)
	}

	return metricVal, nil
}

// GetMetricValueByPodModel retrieves metric value for Pod-Model combination
// Parameters:
//
//	podName: Name of the Pod
//	modelName: Name of the model
//	metricName: Name of the metric
//
// Returns:
//
//	metrics.MetricValue: The metric value
//	error: Error if Pod, model or metric doesn't exist
func (c *Store) GetMetricValueByPodModel(podName, modelName string, metricName string) (metrics.MetricValue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	podMetrics, ok := c.PodModelMetrics[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the podMetrics cache")
	}

	modelMetrics, ok := podMetrics[modelName]
	if !ok {
		return nil, fmt.Errorf("model does not exist in the podMetrics cache")
	}

	metricVal, ok := modelMetrics[metricName]
	if !ok {
		return nil, fmt.Errorf("no metric available for %v", metricName)
	}

	return metricVal, nil
}

// AddRequestCount tracks new request initiation
// Parameters:
//
//	requestID: Unique request identifier
//	modelName: Model handling the request
//
// Returns:
//
//	int64: Trace term identifier
func (c *Store) AddRequestCount(requestID string, modelName string) (traceTerm int64) {
	success := false
	for {
		trace := c.getRequestTrace(modelName)
		// TODO: use non-empty key if we have output prediction to decide buckets early.
		if traceTerm, success = trace.AddRequest(requestID, ""); success {
			break
		}
		// In case AddRequest return false, it has been recycled and we want to retry.
	}

	newPendingCounter := int32(0)
	pPendingCounter, _ := c.pendingRequests.LoadOrStore(modelName, &newPendingCounter)
	atomic.AddInt32(pPendingCounter.(*int32), 1)
	return
}

// DoneRequestCount completes request tracking
// Parameters:
//
//	requestID: Unique request identifier
//	modelName: Model handling the request
//	traceTerm: Trace term identifier
func (c *Store) DoneRequestCount(requestID string, modelName string, traceTerm int64) {
	pPendingCounter, ok := c.pendingRequests.Load(modelName)
	if ok {
		atomic.AddInt32(pPendingCounter.(*int32), -1)
	}

	// DoneRequest only works for current term, no need to retry.
	c.getRequestTrace(modelName).DoneRequest(requestID, traceTerm)
}

// AddRequestTrace records request tracing information
// Parameters:
//
//	requestID: Unique request identifier
//	modelName: Model handling the request
//	inputTokens: Number of input tokens
//	outputTokens: Number of output tokens
func (c *Store) AddRequestTrace(requestID string, modelName string, inputTokens, outputTokens int64) {
	traceKey := c.getTraceKey(inputTokens, outputTokens)
	for {
		trace := c.getRequestTrace(modelName)
		if trace.AddRequestTrace(requestID, traceKey) {
			break
		}
		// In case DoneRequest return false, it has been recycled and we want to retry.
	}
}

// DoneRequestTrace completes request tracing
// Parameters:
//
//	requestID: Unique request identifier
//	modelName: Model handling the request
//	inputTokens: Input tokens count
//	outputTokens: Output tokens count
//	traceTerm: Trace term identifier
func (c *Store) DoneRequestTrace(requestID string, modelName string, inputTokens, outputTokens, traceTerm int64) {
	pPendingCounter, ok := c.pendingRequests.Load(modelName)
	if ok {
		atomic.AddInt32(pPendingCounter.(*int32), -1)
	}

	traceKey := c.getTraceKey(inputTokens, outputTokens)
	for {
		trace := c.getRequestTrace(modelName)
		if trace.DoneRequestTrace(requestID, traceKey, traceTerm) {
			break
		}
		// In case DoneRequest return false, it has been recycled and we want to retry.
	}
}

// AddSubscriber registers new metric subscriber
// Parameters:
//
//	subscriber: Metric subscriber implementation
func (c *Store) AddSubscriber(subscriber metrics.MetricSubscriber) {
	c.subscribers = append(c.subscribers, subscriber)
	c.aggregateMetrics()
}
