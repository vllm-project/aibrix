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

import "k8s.io/klog/v2"

func (c *Store) updateDebugInfo() {
	c.mu.RLock()
	defer c.mu.RUnlock()

	c.metricsDebugInfo()
}

func (c *Store) metricsDebugInfo() {
	for _, pod := range c.Pods {
		klog.V(4).Infof("pod: %s, podIP: %v", pod.Name, pod.Status.PodIP)
	}
	for podName, models := range c.PodToModelMapping {
		var modelList string
		for modelName := range models {
			modelList += modelName + " "
		}
		klog.V(4).Infof("pod: %s, models: %s", podName, modelList)
	}
	for modelName, pods := range c.ModelToPodMapping {
		var podList string
		for podName := range pods {
			podList += podName + " "
		}
		klog.V(4).Infof("model: %s, pods: %s", modelName, podList)
	}
	for podName, metrics := range c.PodMetrics {
		for metricName, metricVal := range metrics {
			klog.V(5).Infof("%v_%v_%v", podName, metricName, metricVal)
		}
	}
	for podName, models := range c.PodModelMetrics {
		for modelName, metrics := range models {
			for metricName, metricVal := range metrics {
				klog.V(5).Infof("%v_%v_%v_%v", podName, modelName, metricName, metricVal)
			}
		}
	}
}
