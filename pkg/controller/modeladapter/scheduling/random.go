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
	"math/rand"

	"github.com/aibrix/aibrix/pkg/cache"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type randomScheduler struct {
	cache *cache.Cache
}

func NewRandomScheduler(c *cache.Cache) Scheduler {
	return randomScheduler{
		cache: c,
	}
}

func (r randomScheduler) SelectPod(ctx context.Context, pods []v1.Pod) (*v1.Pod, error) {
	if len(pods) == 0 {
		return nil, errors.New("no pods to schedule model adapter")
	}

	idx := rand.Intn(len(pods))
	selectedPod := pods[idx]

	klog.InfoS("pod selected with random scheduler", "pod", klog.KObj(&selectedPod))
	return &selectedPod, nil
}
