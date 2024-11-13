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
	"github.com/aibrix/aibrix/pkg/cache"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type firstFitScheduler struct {
	cache *cache.Cache
}

func NewFirstFitScheduler(c *cache.Cache) Scheduler {
	return firstFitScheduler{
		cache: c,
	}
}

func (r firstFitScheduler) SelectPod(ctx context.Context, pods []v1.Pod) (*v1.Pod, error) {
	selectedPod := v1.Pod{}

	for _, pod := range pods {
		if pod.Status.PodIP == "" {
			continue
		}

		// todo: 判断这个LoRA是否可以部署在这个Pod上
		// - \sum{LoRA_Rank} <= Pod_Rank
		// - Anything else?

		// First fit algorithm, once the constraints are satisfied, select the pod
		selectedPod = pod
		break
	}

	// TODO: selectedPod为NULL怎么处理

	klog.InfoS("pod selected with first fit", "pod", klog.KObj(&selectedPod))
	return &selectedPod, nil
}
