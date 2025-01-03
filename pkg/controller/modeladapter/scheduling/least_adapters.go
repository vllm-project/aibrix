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
	"math"

	"github.com/aibrix/aibrix/pkg/cache"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type leastAdapters struct {
	cache *cache.Cache
}

func NewLeastAdapters(c *cache.Cache) Scheduler {
	return leastAdapters{
		cache: c,
	}
}

func (r leastAdapters) SelectPod(ctx context.Context, pods []v1.Pod) (*v1.Pod, error) {
	selectedPod := v1.Pod{}
	modelAdapterCountMin := math.MaxInt

	for _, pod := range pods {
		models, err := r.cache.GetModelsForPod(pod.Name)
		if err != nil {
			return nil, err
		}
		if len(models) < modelAdapterCountMin {
			selectedPod = pod
			modelAdapterCountMin = len(models)
		}
	}

	klog.InfoS("pod selected with least model adapters", "pod", klog.KObj(&selectedPod))
	return &selectedPod, nil
}
