/*
Copyright 2025 The Aibrix Team.

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
	"os"
	"strconv"
	"strings"

	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

func getPodMetricPort(pod *Pod) int {
	if pod == nil || pod.Labels == nil {
		return defaultMetricPort
	}
	if v, ok := pod.Labels[MetricPortLabel]; ok && v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			return p
		} else {
			klog.Warningf("Invalid value for label %s on pod %s/%s: %q. Using default port %d.", MetricPortLabel, pod.Namespace, pod.Name, v, defaultMetricPort)
		}
	}
	return defaultMetricPort
}

func getPodLabel(pod *Pod, labelName string) (string, error) {
	labelTarget, ok := pod.Labels[labelName]
	if !ok {
		klog.V(4).Infof("No label %v name for pod %v, default to %v", labelName, pod.Name, defaultEngineLabelValue)
		err := fmt.Errorf("error executing query: no label %v found for pod %v", labelName, pod.Name)
		return "", err
	}
	return labelTarget, nil
}

func buildMetricLabels(pod *Pod, engineType string, model string) ([]string, []string) {
	labelNames := []string{
		"namespace",
		"pod",
		"model",
		"engine_type",
		"roleset",
		"role",
		"role_replica_index",
		"gateway_pod",
	}
	labelValues := []string{
		pod.Namespace,
		pod.Name,
		model,
		engineType,
		utils.GetPodEnv(pod.Pod, "ROLESET_NAME", ""),
		utils.GetPodEnv(pod.Pod, "ROLE_NAME", ""),
		utils.GetPodEnv(pod.Pod, "ROLE_REPLICA_INDEX", ""),
		os.Getenv("POD_NAME"),
	}
	return labelNames, labelValues
}

func shouldSkipMetric(podName string, metricName string) bool {
	if strings.Contains(podName, "prefill") && isDecodeOnlyMetric(metricName) {
		return true
	}
	if strings.Contains(podName, "decode") && isPrefillOnlyMetric(metricName) {
		return true
	}
	return false
}
