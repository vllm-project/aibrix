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

package scheduling

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/aibrix/aibrix/pkg/cache"
	"github.com/aibrix/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type leastUtilizationScheduler struct {
	cache *cache.Cache
}

func NewLeastUtilizationScheduler(c *cache.Cache) Scheduler {
	return leastUtilizationScheduler{
		cache: c,
	}
}

func (r leastUtilizationScheduler) SelectPod(ctx context.Context, pods []v1.Pod) (*v1.Pod, error) {
	selectedPod := v1.Pod{}
	utilizationTag := "RPM"
	minUtilizationVal := math.MaxFloat64

	for _, pod := range pods {
		if pod.Status.PodIP == "" {
			continue
		}

		// todo: 判断这个LoRA是否可以部署在这个Pod上
		// - \sum{LoRA_Rank} <= Pod_Rank
		// - Anything else?

		// Get utilization
		var err error
		var utilization metrics.MetricValue
		switch utilizationTag {
		case "RPM":
			// todo: 从cache获取相关指标, 接口待定
			utilization, err = r.cache.GetPodMetric(pod.Name, "requests_per_minute")
		case "TPM":
			utilization, err = r.cache.GetPodMetric(pod.Name, "tokens_per_minute")
		case "kv_cache":
			utilization, err = r.cache.GetPodMetric(pod.Name, "kv_cache")
		case "busy_time":
			utilization, err = r.cache.GetPodMetric(pod.Name, "gpu_busy_time_ratio")
		default:
			err = errors.New(fmt.Sprintf("Invalid case: %s.", utilizationTag))
		}
		if err != nil {
			klog.Error(err)
			continue
		}

		utilizationVal := utilization.GetSimpleValue()
		// Select the pod with minimum utilization
		if utilizationVal <= minUtilizationVal {
			selectedPod = pod
			minUtilizationVal = utilizationVal
		}
	}

	// TODO: selectedPod为NULL怎么处理

	logMsg := fmt.Sprintf("pod selected with least utilization with %s", utilizationTag)
	klog.InfoS(logMsg, "pod", klog.KObj(&selectedPod))
	return &selectedPod, nil
}
