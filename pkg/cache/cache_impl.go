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
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// GetPod retrieves a Pod object by name from the cache
// Parameters:
//
//	podName: Name of the pod to retrieve
//	podNamespace: Namespace of the pod to retrieve
//
// Returns:
//
//	*v1.Pod: The found Pod object
//	error: Error if pod doesn't exist
func (c *Store) GetPod(podName, podNamespace string) (*v1.Pod, error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		return nil, fmt.Errorf("key does not exist in the cache: %s", key)
	}

	return metaPod.Pod, nil
}

// ListPods returns all cached Pod objects
// Do not call this directly, for debug purpose and less efficient.
// Returns:
//
//	[]*v1.Pod: Slice of Pod objects
func (c *Store) ListPods() []*v1.Pod {
	pods := make([]*v1.Pod, 0, c.metaPods.Len())
	c.metaPods.Range(func(_ string, metaPod *Pod) bool {
		pods = append(pods, metaPod.Pod)
		return true
	})
	return pods
}

// ListPodsByModel gets Pods associated with a specific model
// Parameters:
//
//	modelName: Name of the model to query
//
// Returns:
//
//	types.PodList: PodArray wrapper for a slice of Pod objects
//	error: Error if model doesn't exist
func (c *Store) ListPodsByModel(modelName string) (types.PodList, error) {
	meta, ok := c.metaModels.Load(modelName)
	if !ok {
		return nil, fmt.Errorf("model does not exist in the cache: %s", modelName)
	}

	return meta.Pods.Array(), nil
}

// ListModels returns all cached model names
// Returns:
//
//	[]string: Slice of model names
func (c *Store) ListModels() []string {
	return c.metaModels.Keys()
}

// HasModel checks if a model exists in the cache
// Parameters:
//
//	modelName: Name of the model to check
//
// Returns:
//
//	bool: True if model exists
func (c *Store) HasModel(modelName string) bool {
	_, ok := c.metaModels.Load(modelName)

	return ok
}

// ListModelsByPod gets models associated with a specific Pod
// Parameters:
//
//	podName: Name of the Pod to query
//	podNamespace: Namespace of the Pod to query
//
// Returns:
//
//	[]string: Slice of model names
//	error: Error if Pod doesn't exist
func (c *Store) ListModelsByPod(podName, podNamespace string) ([]string, error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		return nil, fmt.Errorf("key does not exist in the cache: %s", key)
	}

	return metaPod.Models.Array(), nil
}

// GetMetricValueByPod retrieves metric value for a Pod
// Parameters:
//
//	podName: Name of the Pod
//	podNamespace: Namespace of the Pod
//	metricName: Name of the metric
//
// Returns:
//
//	metrics.MetricValue: The metric value
//	error: Error if Pod or metric doesn't exist
func (c *Store) GetMetricValueByPod(podName, podNamespace, metricName string) (metrics.MetricValue, error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		return nil, fmt.Errorf("key does not exist in the cache: %s", key)
	}

	return c.getPodMetricImpl(podName, &metaPod.Metrics, metricName)
}

// GetMetricValueByPodModel retrieves metric value for Pod-Model combination
// Parameters:
//
//	podName: Name of the Pod
//	podNamespace: Namespace of the Pod
//	modelName: Name of the model
//	metricName: Name of the metric
//
// Returns:
//
//	metrics.MetricValue: The metric value
//	error: Error if Pod, model or metric doesn't exist
func (c *Store) GetMetricValueByPodModel(podName, podNamespace, modelName string, metricName string) (metrics.MetricValue, error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		return nil, fmt.Errorf("key does not exist in the cache: %s", key)
	}

	return c.getPodMetricImpl(podName, &metaPod.ModelMetrics, c.getPodModelMetricName(modelName, metricName))
}

// AddRequestCount tracks new request initiation.
// If ctx is provided,  AddRequestCount can be called multiple times for the same request.
//
// Parameters:
//
//	ctx: Routing context
//	requestID: Unique request identifier
//	modelName: Model handling the request
//
// Returns:
//
//	int64: Trace term identifier
func (c *Store) AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) (traceTerm int64) {
	// Current implementation assumes AddRequestCount() will not be called concurrently.
	// TODO: Implment "wait for trace term" logic if AddRequestCount() is called concurrently.
	if ctx == nil || ctx.CanAddTrace() {
		if c.enableTracing {
			success := false
			for {
				trace := c.getRequestTrace(modelName)
				// TODO: use non-empty key if we have output prediction to decide buckets early.
				if traceTerm, success = trace.AddRequest(requestID, ""); success {
					if ctx != nil {
						ctx.TraceTerm = traceTerm
					}
					break
				}
				// In case AddRequest return false, it has been recycled and we want to retry.
			}
		}

		meta, ok := c.metaModels.Load(modelName)
		if ok {
			atomic.AddInt32(&meta.pendingRequests, 1)
		}
	}

	// Current implementation assumes AddRequestCount() will not be called concurrently.
	// TODO: Implment "wait for trace term" logic if AddRequestCount() is called concurrently.
	if ctx == nil {
		return traceTerm
	} else if !ctx.HasRouted() || !ctx.CanUpdateStats() {
		return ctx.TraceTerm
	} else {
		traceTerm = ctx.TraceTerm
		c.addPodStats(ctx, requestID)
	}
	return traceTerm
}

// DoneRequestCount completes request tracking
// Parameters:
//
//	 ctx: Routing context
//		requestID: Unique request identifier
//		modelName: Model handling the request
//		traceTerm: Trace term identifier
func (c *Store) DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64) {
	if ctx != nil {
		c.donePodStats(ctx, requestID)
	}

	meta, ok := c.metaModels.Load(modelName)
	if ok {
		atomic.AddInt32(&meta.pendingRequests, -1)
	}

	// DoneRequest only works for current term, no need to retry.
	if c.enableTracing {
		c.getRequestTrace(modelName).DoneRequest(requestID, traceTerm)
	}
}

// DoneRequestTrace completes request tracing
// Parameters:
//
//	ctx: Routing context
//	requestID: Unique request identifier
//	modelName: Model handling the request
//	inputTokens: Input tokens count
//	outputTokens: Output tokens count
//	traceTerm: Trace term identifier
func (c *Store) DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, inputTokens, outputTokens, traceTerm int64) {
	if ctx != nil {
		c.donePodStats(ctx, requestID)
	}

	meta, ok := c.metaModels.Load(modelName)
	if ok {
		atomic.AddInt32(&meta.pendingRequests, -1)
	}

	if c.enableTracing {
		var traceKey string
		for {
			trace := c.getRequestTrace(modelName)
			if traceKey, ok = trace.DoneRequestTrace(requestID, inputTokens, outputTokens, traceKey, traceTerm); ok {
				break
			}
			// In case DoneRequest return false, it has been recycled and we want to retry.
		}
		klog.V(5).Infof("inputTokens: %v, outputTokens: %v, trace key: %s", inputTokens, outputTokens, traceKey)
	}

	//
	meta.OutputPredictor.AddTrace(int(inputTokens), int(outputTokens), 1)
}

// AddSubscriber registers new metric subscriber
// Parameters:
//
//	subscriber: Metric subscriber implementation
func (c *Store) AddSubscriber(subscriber metrics.MetricSubscriber) {
	c.subscribers = append(c.subscribers, subscriber)
	c.aggregateMetrics()
}

func (c *Store) UpdateModelProfile(key string, profile *ModelGPUProfile, force bool) {
	existed, ok := c.deploymentProfiles.Load(key)
	if profile == nil && ok {
		c.deploymentProfiles.Delete(key)
	} else if force || !ok || existed.Created < profile.Created {
		c.deploymentProfiles.Store(key, profile)
	}
}

func (c *Store) GetModelProfileByPod(pod *v1.Pod, modelName string) (*ModelGPUProfile, error) {
	deploymentName := utils.DeploymentNameFromPod(pod)
	if len(deploymentName) == 0 {
		// Handle the case where the deployment name is not found (e.g., log an error)
		return nil, fmt.Errorf("deployment name not found on pod %s", pod.Name)
	}

	key := ModelGPUProfileKey(modelName, deploymentName)
	profile, ok := c.deploymentProfiles.Load(key)
	if !ok {
		return nil, MissingProfileError{
			CacheError: ErrorMissingProfile,
			ProfileKey: key,
		}
	}

	return profile, nil
}

func (c *Store) GetModelProfileByDeploymentName(deploymentName string, modelName string) (*ModelGPUProfile, error) {
	key := ModelGPUProfileKey(modelName, deploymentName)
	profile, ok := c.deploymentProfiles.Load(key)
	if !ok {
		return nil, fmt.Errorf("profile not available for %s", key)
	}

	return profile, nil
}

func (c *Store) GetOutputPredictor(modelName string) (types.OutputPredictor, error) {
	if model, ok := c.metaModels.Load(modelName); ok {
		return model.OutputPredictor, nil
	}
	return nil, fmt.Errorf("model does not exist in the cache: %s", modelName)
}

func (c *Store) GetRouter(ctx *types.RoutingContext) (types.Router, error) {
	if model, ok := c.metaModels.Load(ctx.Model); !ok {
		return nil, fmt.Errorf("model does not exist in the cache: %s", ctx.Model)
	} else if model.QueueRouter == nil {
		return nil, fmt.Errorf("queue router not available for model: %s", ctx.Model)
	} else {
		return model.QueueRouter, nil
	}
}
